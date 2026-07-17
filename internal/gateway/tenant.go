/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// package 선언: 이 파일이 속한 패키지 이름이다.
// server.go와 같은 gateway 패키지이므로, 여기서 Server 타입을 import 없이 바로 쓸 수 있다.
package gateway

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티/쿠버네티스 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 전달하는 표준 타입이며, 아래 Client.Get에 넘긴다.
	"context"
	// net/http: 여기서는 들어온 요청을 나타내는 http.Request 타입을 쓰기 위해 필요하다.
	"net/http"
	// strings: 문자열을 자르고 다듬고 비교하는 표준 도구 모음이다.
	// Authorization 헤더를 파싱하는 데 Cut, EqualFold, TrimSpace를 쓴다.
	"strings"

	// corev1: 쿠버네티스 core/v1 API 그룹의 타입들이며, 여기서는 Secret을 쓴다.
	// 별칭 corev1을 붙이는 이유는 원래 패키지 이름 v1이 다른 v1들과 헷갈리기 때문이다.
	corev1 "k8s.io/api/core/v1"
	// types: 쿠버네티스의 공용 식별자 타입들이며, 여기서는 NamespacedName을 쓴다.
	"k8s.io/apimachinery/pkg/types"
)

// resolveTenant: 요청의 Bearer API key를 api-keys Secret을 통해 tenant로 해석한다.
// key가 없거나 알 수 없는 key면 ok=false를 반환한다.
//
// Go 문법 설명:
//   - func (s *Server) 부분은 리시버(receiver)다.
//     이 함수가 Server 타입에 붙는 "메서드"라는 뜻이고, 호출은 server.resolveTenant(...) 형태가 된다.
//   - *Server 처럼 별표(*)를 붙이면 "Server의 포인터"를 받는다는 의미이며, 원본을 가리키므로 s.Client를 그대로 쓸 수 있다.
//   - 반환 타입 (string, bool)은 "해석된 tenant 이름"과 "성공했는지 여부"의 짝이다.
//     여기서 error 대신 bool을 쓰는 이유는, 인증 실패가 시스템 장애가 아니라 예상된 정상 결과이기 때문이다.
//     또 실패 사유를 굳이 구분하지 않는 편이 안전하다(아래 설계 근거 참고).
//   - 소문자 이름이라 이 패키지 안에서만 호출할 수 있다.
//
// 설계 근거(설계서 Identity model 절): tenant는 요청자가 스스로 주장하는 값이 아니라 API key로부터 유도된다.
// 헤더에 tenant 이름을 직접 담게 하면 누구나 남의 tenant를 사칭할 수 있기 때문이다.
// 또 실패 사유(헤더 없음/형식 오류/모르는 key)를 구분해서 알려주지 않는 이유는,
// 그 차이가 공격자에게 "이 key는 존재한다" 같은 정보를 흘리기 때문이다.
// 그래서 모든 실패를 똑같이 ok=false로 뭉뚱그린다.
func (s *Server) resolveTenant(ctx context.Context, r *http.Request) (string, bool) {
	// Authorization 헤더 값을 통째로 읽는다.
	// Header.Get은 헤더 이름의 대소문자를 알아서 정규화하며, 없으면 빈 문자열("")을 준다.
	// 즉 헤더가 없어도 패닉이 나지 않고 아래 파싱이 자연스럽게 실패로 이어진다.
	h := r.Header.Get("Authorization")
	// 첫 공백 기준으로 scheme과 자격증명을 분리한다.
	//
	// Go 문법 설명: strings.Cut(문자열, 구분자)는 값을 세 개 돌려준다.
	// 구분자 앞부분(scheme), 뒷부분(credential), 그리고 구분자를 찾았는지 여부(found)다.
	// "Bearer k1"이면 scheme="Bearer", credential="k1", found=true가 된다.
	// Split과 달리 첫 번째 구분자에서만 자르므로, 자격증명 안에 공백이 있어도 잘리지 않는다.
	scheme, credential, found := strings.Cut(h, " ")
	// HTTP 인증 scheme은 RFC 7235상 대소문자를 구분하지 않으므로 EqualFold로 비교한다.
	//
	// Go 문법 설명:
	//   - strings.EqualFold(a, b)는 대소문자를 무시하고 두 문자열이 같은지 본다.
	//     == 로 비교하면 "bearer k1"처럼 소문자로 보낸 정상 클라이언트를 규격 위반으로 거절하게 된다.
	//   - ! 는 부정(NOT)이고 || 는 "또는(OR)"이다.
	//     앞 조건이 참이면 뒤는 검사하지 않는다(short-circuit).
	//   - 따라서 이 조건은 "공백이 아예 없었거나, scheme이 Bearer가 아니면"이라는 뜻이다.
	if !found || !strings.EqualFold(scheme, "Bearer") {
		// 실패이므로 tenant 자리엔 빈 문자열을, ok 자리엔 false를 넣는다.
		return "", false
	}
	// 자격증명 앞뒤의 공백/개행을 제거한다.
	// "Bearer  k1"처럼 공백이 둘이거나 값 끝에 공백이 붙어 오는 경우를 흡수해, 멀쩡한 key가 헛되이 거절되지 않게 한다.
	key := strings.TrimSpace(credential)
	// 다듬고 나니 아무것도 남지 않았다면("Bearer " 같은 요청) 조회할 key가 없으므로 실패다.
	// 이 검사를 빼면 빈 key로 Secret을 조회하게 되어 의미 없는 작업을 하게 된다.
	if key == "" {
		return "", false
	}
	// var sec ... : 읽어온 Secret을 담을 빈 변수를 선언한다.
	// Go는 var로 선언한 구조체를 자동으로 "제로값"(모든 필드가 비어 있는 상태)으로 초기화하므로 그대로 써도 된다.
	var sec corev1.Secret
	// s.Client.Get(...)으로 api-keys Secret 하나를 읽는다.
	//
	// Go 문법 설명:
	//   - types.NamespacedName{Name: ..., Namespace: ...}는 "어느 namespace의 어떤 이름"인지를 담는 좌표다.
	//     쿠버네티스에서 namespace 스코프 객체 하나를 특정하려면 이름만으로는 부족하고 namespace가 함께 있어야 한다.
	//   - &sec 의 & 는 주소(포인터)를 넘긴다는 뜻이며, Get이 sec 원본을 채워야 하므로 포인터가 필요하다.
	//   - if err := ...; err != nil 은 호출과 동시에 err 변수를 만들고 즉시 검사하는 Go의 관용구다.
	//     이렇게 만든 err는 이 if 블록 안에서만 유효해서, 바깥 이름 공간을 어지럽히지 않는다.
	//
	// 이 Get은 cache에서 읽으므로 요청마다 apiserver를 때리지 않는다(설계서 Components 절).
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.APIKeySecret, Namespace: s.Namespace}, &sec); err != nil {
		// Secret을 못 읽으면(없거나 권한이 없거나) 어떤 key도 검증할 수 없으므로 실패로 처리한다.
		// 에러 내용을 호출한 쪽에 넘기지 않는 이유는, 인증 경로에서 내부 사정을 노출하지 않기 위해서다.
		return "", false
	}
	// Secret의 Data에서 key로 tenant 이름을 찾는다.
	//
	// Go 문법 설명:
	//   - sec.Data는 map[string][]byte 타입이며, 값이 문자열이 아니라 바이트 슬라이스([]byte)다.
	//     쿠버네티스 Secret은 임의의 바이너리도 담을 수 있어서 값 타입이 []byte로 정의되어 있다.
	//   - Go의 map 조회는 값과 "존재 여부(ok)"를 함께 준다.
	//     키가 없으면 tenant엔 제로값(nil 슬라이스)이, ok엔 false가 들어온다.
	//     이 두 값짜리 형태를 쓰지 않으면 "키가 없음"과 "값이 비어 있음"을 구분할 수 없다.
	tenant, ok := sec.Data[key]
	// string(tenant)로 []byte를 문자열로 변환해 돌려준다.
	//
	// Go 문법 설명:
	//   - && 는 "그리고(AND)"이며, 양쪽이 모두 참일 때만 참이다.
	//   - len(tenant)는 바이트 슬라이스의 길이다.
	// 값이 비어있지 않아야 유효한 tenant다.
	// key는 등록되어 있는데 값이 빈 문자열인 Secret 항목은 잘못 설정된 상태이며,
	// 이때 ok=true를 주면 tenant 이름이 ""인 채로 인증이 통과해 버린다.
	// 그래서 존재 여부(ok)와 길이(len)를 함께 확인해 그런 항목을 인증 실패로 막는다.
	return string(tenant), ok && len(tenant) > 0 // 값이 비어있지 않아야 유효한 tenant
}
