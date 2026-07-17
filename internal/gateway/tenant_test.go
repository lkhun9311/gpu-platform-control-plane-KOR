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

// package 선언: 테스트 대상과 같은 gateway 패키지에 속하는 "내부 테스트"다.
// 그래서 소문자로 감춰진 resolveTenant를 직접 호출할 수 있다.
package gateway

// import 블록: 이 테스트가 사용하는 패키지들이다.
import (
	// context: resolveTenant의 첫 인자로 넘길 ctx를 만들기 위해 필요하다.
	"context"
	// net/http: 요청 메서드 상수(MethodPost)와 헤더 조작에 필요하다.
	"net/http"
	// net/http/httptest: 진짜 서버 없이 가짜 HTTP 요청을 만들어 주는 표준 도구다.
	"net/http/httptest"

	// ginkgo/gomega: BDD 스타일 테스트 프레임워크와 단언 라이브러리다.
	// 앞의 점(.)은 dot import이며, Describe/It/Expect를 패키지 접두사 없이 쓰게 해준다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// corev1: 쿠버네티스 core/v1 타입들이며, 여기서는 가짜 Secret을 만드는 데 쓴다.
	corev1 "k8s.io/api/core/v1"
	// metav1: 이름/namespace 같은 공통 메타데이터를 담는 ObjectMeta 타입을 제공한다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// fake: 실제 클러스터 없이 메모리에서 동작하는 가짜 client 구현이다.
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// resolveTenant에 대한 명세 묶음이다.
//
// Go 문법 설명: var _ = Describe(...) 형태는 반환값을 버리고 등록이라는 부수 효과만 취하는 Ginkgo의 관용구다.
// Describe와 It의 문자열은 주석이 아니라 실행되는 코드 리터럴이며, 테스트 출력에 그대로 찍힌다.
//
// 아래 It들은 하나의 표(table)를 이루듯 같은 입력 축(Authorization 헤더)을 여러 값으로 바꿔가며 검증한다.
// 헤더 없음 / 모르는 key / 정상 key / 대소문자가 다른 scheme, 이렇게 네 가지가 인증 경로의 주요 분기다.
var _ = Describe("resolveTenant", func() {
	// newServer: k1 key를 team-vision tenant로 매핑한 Secret을 담은 fake client를 준비하는 헬퍼다.
	//
	// Go 문법 설명:
	//   - 이것은 변수에 대입된 함수 리터럴(익명 함수)이며, newServer()처럼 호출할 수 있다.
	//   - Describe 블록 안에 선언했으므로 이 블록의 It들만 이 헬퍼를 볼 수 있다(클로저 스코프).
	//   - 각 It이 이 함수를 새로 호출해 매번 새 Server와 새 Secret을 얻는다.
	//     상태를 공유하지 않아야 한 테스트의 변경이 다른 테스트로 새지 않고, 실행 순서에도 영향받지 않는다.
	//   - 여기서는 WithScheme을 부르지 않는데, Secret은 fake client의 기본 scheme에 이미 들어 있는 기본 타입이기 때문이다.
	//     (반면 ratelimit_test.go는 CRD를 쓰므로 scheme을 직접 등록해야 했다.)
	//
	// Secret의 Data는 map[string][]byte 타입이라 값이 문자열이 아니라 바이트 슬라이스다.
	// 그래서 []byte("team-vision")처럼 변환해서 넣는다.
	// 이 구조가 곧 "key → tenant" 매핑이며, 운영에서도 같은 모양의 Secret을 쓴다.
	newServer := func() *Server {
		c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gateway-api-keys", Namespace: "gw"},
			Data:       map[string][]byte{"k1": []byte("team-vision")},
		}).Build()
		// Namespace와 APIKeySecret은 위에서 만든 Secret의 좌표와 정확히 일치해야 한다.
		// 하나라도 어긋나면 Get이 실패해 모든 테스트가 ok=false로 떨어진다.
		return &Server{Client: c, Namespace: "gw", APIKeySecret: "gateway-api-keys"}
	}

	// 이 테스트가 막는 회귀(설계서 Identity model 절):
	// Authorization 헤더가 아예 없는 익명 요청이 통과하면 인증이 무의미해진다.
	// 헤더가 없을 때 Header.Get이 빈 문자열을 주는데, 이를 제대로 걸러내지 못하면
	// 빈 key로 조회가 이어지거나 tenant가 ""인 채로 통과할 위험이 있다.
	It("returns ok=false when the Authorization header is missing", func() {
		s := newServer()
		// 헤더를 일부러 설정하지 않은 요청을 만든다.
		// 마지막 인자 nil은 "요청 본문 없음"을 뜻한다.
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse()) // Authorization header 없으면 실패
		// tenant도 함께 확인하는 이유는, ok=false인데 tenant에 값이 남아 있으면
		// 호출한 쪽이 실수로 그 값을 써버릴 여지가 생기기 때문이다.
		// BeEmpty()는 빈 문자열/빈 슬라이스/빈 map을 검사하는 gomega 단언이다.
		Expect(tenant).To(BeEmpty())
	})

	// 이 테스트가 막는 회귀(설계서 Identity model 절):
	// Secret에 등록되지 않은 임의의 key로 인증이 통과하면 누구나 게이트웨이를 쓸 수 있다.
	// map 조회의 두 값짜리 형태(ok)를 쓰지 않고 값만 받으면, 없는 key에 대해 제로값(빈 슬라이스)이 나오는데
	// 이때 ok 검사가 없으면 tenant=""로 통과해 버린다.
	It("returns ok=false for an unknown bearer key", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		// Header.Set(이름, 값)으로 요청 헤더를 채운다.
		// 형식은 올바른 Bearer지만 key "unknown"은 Secret에 없다.
		// 즉 이 테스트는 "형식 검증"이 아니라 "실제 조회"가 동작하는지를 본다.
		r.Header.Set("Authorization", "Bearer unknown")
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse()) // Secret에 없는 key라 실패
		Expect(tenant).To(BeEmpty())
	})

	// 이 테스트가 막는 회귀(설계서 Identity model 절):
	// 위의 두 테스트는 전부 "거절"만 확인하므로, resolveTenant가 무조건 false를 돌려주는
	// 망가진 구현이어도 통과해 버린다.
	// 정상 경로를 함께 검증해야 그런 구멍이 막힌다.
	// 또한 반환된 tenant가 정확히 Secret의 값이어야 한다(엉뚱한 tenant로 해석되면 격리가 깨진다).
	It("resolves a known bearer key to its tenant", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer k1")
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeTrue())
		// []byte였던 Secret 값이 string으로 올바르게 변환되어 나오는지도 함께 확인된다.
		Expect(tenant).To(Equal("team-vision"))
	})

	// 이 테스트가 막는 회귀(설계서 Identity model 절):
	// HTTP 인증 scheme은 RFC 7235상 대소문자를 구분하지 않는다.
	// 구현이 EqualFold 대신 == 로 "Bearer"를 비교하면, 소문자 bearer를 보내는 정상 클라이언트가 부당하게 401을 받는다.
	// 이 테스트는 바로 그 == 로의 퇴행을 잡아낸다.
	It("resolves a known bearer key case-insensitively", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "bearer k1") // 소문자 bearer도 통과하는지 확인
		tenant, ok := s.resolveTenant(context.Background(), r)
		// scheme 표기만 다를 뿐이므로 위의 정상 경로와 완전히 같은 결과가 나와야 한다.
		Expect(ok).To(BeTrue())
		Expect(tenant).To(Equal("team-vision"))
	})
})
