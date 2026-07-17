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
//
// 같은 디렉터리(internal/gateway)의 모든 .go 파일은 반드시 같은 package 이름(gateway)을 가져야 한다.
//
// 그래야 server.go의 Server 타입이나 tenant.go의 함수 등을 이 파일에서 import 없이 바로 쓸 수 있다.
package gateway

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
//
// 괄호로 묶으면 여러 개를 한 번에 나열할 수 있다.
//
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	//
	// 쿠버네티스 클라이언트 호출은 요청 중단을 위해 전부 ctx를 첫 인자로 받는다.
	"context"
	// errors: 에러 값을 만들고 비교하는 표준 패키지이며, 아래 errors.New에서 쓴다.
	"errors"
	// sync: 동시성 도구 모음이고, 여기서는 Mutex(상호 배제 잠금)를 쓴다.
	//
	// 게이트웨이는 여러 HTTP 요청을 동시에 처리하므로 공유 map 보호가 필요하다.
	"sync"

	// golang.org/x/time/rate: 구글이 관리하는 표준 토큰 버킷 리미터다.
	//
	// 직접 토큰 버킷을 구현하면 버그가 나기 쉬우므로 검증된 이 라이브러리를 쓴다(설계서 Components 절).
	"golang.org/x/time/rate"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(GPUQuotaPolicy 등)이다.
	//
	// import 경로 앞의 platformv1은 별칭(alias)이다.
	//
	// 원래 패키지 이름은 v1이지만 다른 v1들과 헷갈리지 않도록 platformv1이라는 이름으로 부른다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// ErrNoPolicy: "이 테넌트에 해당하는 GPUQuotaPolicy가 하나도 없다"를 나타내는 센티넬 에러다.
//
// Go 문법 설명.
//
//   - var 는 변수(여기서는 패키지 전역 변수) 선언 키워드다.
//   - errors.New("문구")는 그 문구를 담은 에러 값 하나를 만들어 돌려준다.
//   - 이 값을 전역에 딱 하나 만들어 두면 호출한 쪽에서 "이 특정 에러인지"를 정확히 비교할 수 있다.
//     (예: errors.Is(err, ErrNoPolicy), 테스트의 MatchError(ErrNoPolicy))
//
// 설계 근거(설계서 Identity model 절): 테넌트에 정책이 0개면 게이트웨이는 403을 반환해야 한다.
//
// 이 에러는 "읽기 실패"가 아니라 "정책 미프로비저닝"이라는 정상적 결과 신호다.
//
// 따라서 상위 핸들러가 이걸 받아 403으로 변환한다.
var ErrNoPolicy = errors.New("no GPUQuotaPolicy for tenant")

// policyForTenant: 주어진 tenant를 담당하는 GPUQuotaPolicy 하나를 찾아 돌려준다.
//
// 없으면 위의 ErrNoPolicy를 반환한다.
//
// Go 문법 설명.
//
//   - func (s *Server) 부분은 리시버(receiver)다.
//     이 함수가 Server 타입에 붙는 "메서드"라는 뜻이고, 호출은 server.policyForTenant(...) 형태가 된다.
//   - *Server 처럼 별표(*)를 붙이면 "Server의 포인터"를 받는다는 의미다.
//     값 복사가 아니라 원본을 가리키므로 s.Client 같은 필드를 그대로 쓸 수 있고 복사 비용도 없다.
//   - 반환 타입 (*platformv1.GPUQuotaPolicy, error)처럼 Go는 값을 여러 개 돌려줄 수 있다.
//     관례상 마지막 반환값을 error로 두고, 에러가 없으면 nil을 넣는다.
//
// 설계 근거(설계서 Identity model 절): GPUQuotaPolicy는 클러스터 스코프다.
//
// 그래서 같은 spec.tenant를 가진 정책이 2개 이상 존재하는 것을 막지 못한다.
//
// 그런 경우에도 리미터 상태가 비결정적이 되면 안 되므로 결정론적 규칙으로 딱 하나를 고른다.
//
// 규칙은 "가장 오래된(creationTimestamp가 이른) 정책이 이기고, 시간이 같으면 이름 오름차순"이다.
func (s *Server) policyForTenant(ctx context.Context, tenant string) (*platformv1.GPUQuotaPolicy, error) {
	// var list ... : GPUQuotaPolicy 목록을 담을 빈 변수를 선언한다.
	//
	// GPUQuotaPolicyList는 .Items 슬라이스(배열 비슷한 것) 안에 정책들을 담는 컨테이너 타입이다.
	var list platformv1.GPUQuotaPolicyList
	// s.Client.List(...)는 캐시에서 모든 GPUQuotaPolicy를 읽어 list에 채운다.
	//
	// &list 의 & 는 주소(포인터)를 넘긴다는 뜻이며, List가 list 원본을 채워야 하므로 포인터가 필요하다.
	//
	// if err := ...; err != nil 은 호출과 동시에 err 변수를 만들고 즉시 검사하는 Go의 관용구다.
	//
	// 이렇게 만든 err는 이 if 블록 안에서만 유효하다.
	if err := s.Client.List(ctx, &list); err != nil {
		// 읽기 자체가 실패(예: API 서버 오류)하면 그 에러를 그대로 위로 올린다.
		//
		// 첫 반환값은 "정책 없음"이 아니라 "값 없음"이므로 nil을 넣는다.
		return nil, err
	}

	// oldest: 지금까지 본 것 중 가장 오래된 정책을 가리킬 포인터이며, 처음엔 아무것도 없으니 nil이다.
	var oldest *platformv1.GPUQuotaPolicy
	// for i := range list.Items : Items 슬라이스를 처음부터 끝까지 훑는 반복문이다.
	//
	// range는 인덱스 i를 0,1,2...로 준다.
	//
	// 값 대신 인덱스를 쓰는 이유는 아래 &list.Items[i]에서 "원본 요소의 주소"가 필요하기 때문이다.
	//
	// (값으로 받으면 복사본의 주소가 되어버린다.)
	for i := range list.Items {
		// p: i번째 정책 요소의 포인터이고, &는 주소를 뜻한다.
		p := &list.Items[i]
		// 이 정책의 tenant가 우리가 찾는 tenant와 다르면 건너뛴다.
		//
		// != 는 "같지 않다" 비교 연산자이고, continue는 이번 반복을 끝내고 다음으로 넘어간다.
		if p.Spec.Tenant != tenant {
			continue
		}
		// 아직 후보가 없거나(oldest == nil) 현재 p가 기존 후보보다 더 오래됐다면 후보를 교체한다.
		//
		// || 는 "또는(OR)"이며, 앞 조건이 참이면 뒤는 검사하지 않는다(short-circuit).
		if oldest == nil || olderPolicy(p, oldest) {
			oldest = p
		}
	}

	// 반복이 끝나도 후보가 없으면(nil이면) 이 테넌트엔 정책이 하나도 없다는 뜻이다.
	if oldest == nil {
		return nil, ErrNoPolicy
	}
	// 정상 경로: 결정된 정책과 "에러 없음"을 뜻하는 nil을 함께 돌려준다.
	return oldest, nil
}

// olderPolicy: a가 b보다 우선(더 오래됨)인지 true/false로 답하는 헬퍼 함수다.
//
// 규칙은 생성 시각이 더 이르면 우선이고, 시각이 완전히 같으면 이름 사전순으로 앞선 쪽이 우선이다.
//
// Go 문법 설명.
//
//   - 리시버가 없는 일반 함수다(특정 타입에 붙지 않음).
//   - (a, b *platformv1.GPUQuotaPolicy) 처럼 같은 타입 인자는 타입을 한 번만 적어도 된다.
//   - 반환 타입 bool 은 참/거짓 값이다.
func olderPolicy(a, b *platformv1.GPUQuotaPolicy) bool {
	// CreationTimestamp는 metav1.Time 타입이고, .Equal은 "같은 시각인지"를 판단하는 메서드다.
	//
	// &b.CreationTimestamp 로 주소를 넘기는 이유는 Equal의 시그니처가 포인터를 받기 때문이다.
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		// 시각이 같으니 이름으로 tie-break 한다.
		//
		// 문자열에 < 를 쓰면 사전(유니코드)순 비교가 된다.
		return a.Name < b.Name
	}
	// 시각이 다르면 a가 b보다 "이전(Before)"인지로 결정한다.
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// bucketRegistry: 테넌트별로 토큰 버킷 리미터를 하나씩 보관하는 저장소다.
//
// Go 문법 설명.
//
//   - type 이름 struct { ... } 는 여러 필드를 묶는 사용자 정의 타입(구조체) 선언이다.
//   - 대문자로 시작하는 이름(예: 다른 파일의 Server)은 패키지 밖에서도 보이는 "공개(export)"다.
//     소문자로 시작하면(bucketRegistry, mu, buckets) 이 패키지 안에서만 보이는 "비공개"다.
//     이 타입은 게이트웨이 내부 구현 세부이므로 소문자로 감춘다.
//
// 설계 근거(설계서 Components 절): map[tenant]*rate.Limiter 구조다.
//
// 단일 replica라 인메모리 map으로 충분하다(분산 버킷은 out of scope).
//
// 여러 요청이 동시에 이 map을 읽고 쓰므로 Mutex로 보호한다.
type bucketRegistry struct {
	// mu: 상호 배제 잠금이며, 아래 buckets map을 동시에 건드리다 깨지는 것을 막는다.
	//
	// Go의 내장 map은 동시 쓰기에 안전하지 않아서(패닉 발생) 잠금이 반드시 필요하다.
	mu sync.Mutex
	// buckets: 키=테넌트 이름(string), 값=그 테넌트의 버킷(*trackedBucket 포인터)인 map이다.
	//
	// map[K]V 는 "K 타입 키로 V 타입 값을 찾는 해시맵"이라는 뜻이다.
	buckets map[string]*trackedBucket
}

// trackedBucket: 리미터 하나와 그 리미터를 만들 때 쓴 정책 설정(rpm, burst)을 함께 보관한다.
//
// 설정값을 같이 들고 있어야 나중에 정책이 바뀌었는지(rpm/burst가 달라졌는지) 비교해서 리미터를 갱신할지 판단할 수 있다.
type trackedBucket struct {
	// limiter: 실제 토큰 버킷이며, *rate.Limiter 는 rate 패키지의 Limiter 포인터 타입이다.
	limiter *rate.Limiter
	// rpm: 이 리미터가 현재 반영하고 있는 requestsPerMinute 값이다(정책 변경 감지용).
	rpm int32
	// burst: 이 리미터가 현재 반영하고 있는 burst 값이다(정책 변경 감지용).
	burst int32
}

// newBucketRegistry: 비어 있는 bucketRegistry를 만들어 그 포인터를 돌려주는 생성자 함수다.
//
// Go 문법 설명.
//
//   - Go에는 다른 언어의 "생성자"가 문법으로 없어서 관례상 newXxx 함수를 직접 만든다.
//   - make(map[string]*trackedBucket)는 map을 실제로 초기화한다.
//     이걸 안 하고 nil map에 값을 넣으려 하면 런타임 패닉이 나므로 여기서 미리 make로 만들어 둔다.
//   - &bucketRegistry{...} 는 구조체 값을 만든 뒤 그 주소(포인터)를 얻는 표현이다.
//     포인터를 돌려줘야 호출한 쪽과 같은 인스턴스를(잠금과 map을) 공유할 수 있다.
func newBucketRegistry() *bucketRegistry {
	return &bucketRegistry{buckets: make(map[string]*trackedBucket)}
}

// Allow: tenant가 지금 요청을 보내도 되는지 판단하고, 허용이면 토큰 하나를 소비한다.
//
// true면 통과이고, false면 한도 초과다(상위 핸들러가 429 rate_limited로 변환).
//
// Go 문법 설명.
//
//   - 리시버 (b *bucketRegistry): 이 메서드는 레지스트리 인스턴스 b에 붙는다.
//   - 대문자 Allow: 패키지 밖(핸들러)에서 호출해야 하므로 공개 메서드로 둔다.
//
// 설계 근거(설계서 Components 절, Request flow 4단계).
//
//   - rateLimit이 nil이면 "무제한" 테넌트이므로 항상 허용한다.
//   - 초당 속도 = requestsPerMinute / 60 이다.
//     (분당 값을 그대로 초당에 넣으면 60배 빨라지는 버그가 되므로 /60이 필수다.)
//   - 정책이 바뀌면 리미터를 새로 만들지 않고 SetLimit/SetBurst로 갱신해 그동안 쌓인 토큰 상태를 보존한다.
func (b *bucketRegistry) Allow(tenant string, rl *platformv1.GPUQuotaRateLimit) bool {
	// rl(rateLimit 설정)이 nil이면 이 테넌트엔 게이트웨이 속도 제한이 없다는 뜻이므로 무조건 통과다.
	//
	// nil은 "가리키는 대상이 없는 포인터"를 뜻하는 Go의 값이다.
	if rl == nil {
		return true
	}

	// 정책 값(int32)을 rate 라이브러리가 원하는 타입으로 변환해 둔다.
	//
	// rate.Limit(...)는 "초당 토큰 보충 속도" 타입이며, float64(...)로 실수 변환 후 60으로 나눈다.
	//
	// 정수끼리 나누면 소수점이 잘리므로 먼저 float64로 바꿔 실수 나눗셈을 하는 게 중요하다.
	limit := rate.Limit(float64(rl.RequestsPerMinute) / 60.0)
	// burst(버킷 최대 용량)는 int 타입이 필요하므로 int(...)로 int32 → int 변환을 한다.
	burst := int(rl.Burst)

	// 여기서부터 공유 map을 건드리므로 잠금을 건다.
	//
	// Lock() 이후에는 반드시 Unlock() 되어야 한다.
	b.mu.Lock()
	// map에서 tenant 키로 값을 찾는다.
	//
	// Go의 map 조회는 값과 "존재 여부(ok)"를 함께 주며, tb에는 값이, ok에는 키가 있었으면 true가 들어온다.
	tb, ok := b.buckets[tenant]
	if !ok {
		// 이 테넌트를 처음 본 경우이므로 리미터를 새로 만들어 map에 등록한다.
		//
		// rate.NewLimiter(초당속도, 버스트)로 토큰 버킷을 생성하며, 생성 직후 버킷은 가득 차 있다.
		tb = &trackedBucket{
			limiter: rate.NewLimiter(limit, burst),
			rpm:     rl.RequestsPerMinute,
			burst:   rl.Burst,
		}
		b.buckets[tenant] = tb
	} else if tb.rpm != rl.RequestsPerMinute || tb.burst != rl.Burst {
		// 이미 있던 테넌트인데 정책 값이 달라진 경우이므로 리미터를 버리지 않고 설정만 갱신한다.
		//
		// SetLimit/SetBurst는 기존 토큰 상태를 유지하며 속도만 바꾸는, rate.Limiter가 제공하는 메서드다.
		//
		// 통째로 새로 만들면 버킷이 다시 가득 차서 설정을 한 번 바꾼 테넌트에게 공짜 버스트를 주게 된다.
		tb.limiter.SetLimit(limit)
		tb.limiter.SetBurst(burst)
		// 갱신했으니 "현재 반영 중인 값"도 새 값으로 기록해 다음 비교의 기준으로 삼는다.
		tb.rpm = rl.RequestsPerMinute
		tb.burst = rl.Burst
	}
	// 잠금이 걸린 동안 리미터 포인터만 지역 변수로 빼둔다.
	limiter := tb.limiter
	// map 조작이 끝났으니 잠금을 푼다.
	//
	// 실제 토큰 소비(Allow)는 잠금 밖에서 한다.
	b.mu.Unlock()

	// rate.Limiter 자체가 내부적으로 동시성 안전하므로 레지스트리 잠금을 푼 뒤 호출해도 된다.
	//
	// 잠금 구간을 짧게 유지해 다른 테넌트의 요청과 경합을 줄이는 최적화다.
	//
	// limiter.Allow()는 토큰이 있으면 하나 꺼내며 true를, 없으면 false를 준다.
	return limiter.Allow()
}
