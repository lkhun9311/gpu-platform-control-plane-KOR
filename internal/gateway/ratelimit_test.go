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
// 그래서 소문자로 감춰진 policyForTenant, newBucketRegistry 같은 비공개 멤버를 직접 부를 수 있다.
package gateway

// import 블록: 이 테스트가 사용하는 패키지들이다.
import (
	// context: policyForTenant의 첫 인자로 넘길 ctx를 만들기 위해 필요하다.
	"context"
	// time: 정책의 생성 시각을 조작해 "더 오래된 정책"을 꾸며내는 데 쓴다.
	"time"

	// ginkgo/gomega: BDD 스타일 테스트 프레임워크와 단언 라이브러리다.
	// 앞의 점(.)은 dot import이며, Describe/It/Expect를 패키지 접두사 없이 쓰게 해준다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// metav1: 모든 쿠버네티스 객체가 공통으로 갖는 메타데이터 타입들(ObjectMeta, Time 등)이다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// runtime: Go 타입과 API 그룹/버전을 연결하는 등록부인 Scheme 타입이 들어 있다.
	"k8s.io/apimachinery/pkg/runtime"
	// clientgoscheme: 쿠버네티스 기본 제공 타입들(Pod, Secret 등)을 Scheme에 등록해주는 헬퍼다.
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	// fake: 실제 클러스터 없이 메모리에서 동작하는 가짜 client 구현이다.
	// 덕분에 이 테스트들은 apiserver 없이 밀리초 단위로 끝난다.
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(GPUQuotaPolicy 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// newSchemeForTest: client-go와 platform type을 모두 등록한 scheme을 만들어 돌려주는 테스트 헬퍼다.
//
// Go 문법 설명:
//   - Scheme은 "이 Go 타입이 어떤 API 그룹/버전에 해당하는가"를 담은 등록부다.
//     fake client는 이 등록부를 보고 객체를 다루므로, 등록되지 않은 타입을 넣으면 런타임 에러가 난다.
//   - runtime.NewScheme()은 아무것도 등록되지 않은 빈 등록부를 만든다.
//     그래서 아래에서 두 종류를 직접 채워 넣어야 한다.
//   - Expect(...).To(Succeed())는 gomega의 단언이며, 인자가 nil 에러일 때만 통과한다.
//     AddToScheme은 error를 돌려주므로, 등록이 조용히 실패하고 나중에 엉뚱한 곳에서 터지는 것을 여기서 막는다.
//   - 이 헬퍼가 It 블록 안이 아니라 함수로 빠져 있어도 괜찮은 이유는, 호출 자체가 It 안에서 일어나기 때문이다.
//     gomega 단언은 It 실행 중에만 유효하게 동작한다.
//
// 두 종류를 모두 등록하는 이유:
// clientgoscheme는 Secret 같은 기본 타입을, platformv1은 GPUQuotaPolicy 같은 우리 CRD를 등록한다.
// 게이트웨이는 양쪽을 다 읽으므로 테스트 scheme도 양쪽을 다 알아야 한다.
func newSchemeForTest() *runtime.Scheme {
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(platformv1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

// policyForTenant에 대한 명세 묶음이다.
//
// Go 문법 설명: var _ = Describe(...) 형태는 반환값을 버리고 등록이라는 부수 효과만 취하는 Ginkgo의 관용구다.
// Describe와 It의 문자열은 주석이 아니라 실행되는 코드 리터럴이며, 테스트 출력에 그대로 찍힌다.
var _ = Describe("policyForTenant", func() {
	// 이 테스트가 막는 회귀(설계서 Identity model 절):
	// 정책이 없는 tenant에 대해 nil을 조용히 돌려주면, 호출한 쪽이 nil을 역참조해 패닉이 나거나
	// "제한 없음"으로 오해해 모든 요청을 통과시킬 수 있다.
	// 반드시 구분 가능한 ErrNoPolicy가 나와야 상위 핸들러가 이를 403으로 변환할 수 있다.
	It("returns ErrNoPolicy when no policy matches the tenant", func() {
		// 객체를 하나도 넣지 않고 fake client를 만든다.
		//
		// Go 문법 설명: fake.NewClientBuilder()... 처럼 메서드를 점으로 계속 이어 붙이는 방식을 빌더 패턴이라 한다.
		// 각 메서드가 빌더 자신을 돌려주므로 체인이 가능하고, 마지막 Build()가 완성된 client를 만든다.
		// 여기서는 WithObjects를 부르지 않았으므로 클러스터가 텅 빈 상태를 흉내 낸다.
		c := fake.NewClientBuilder().WithScheme(newSchemeForTest()).Build()
		// fake client를 Server의 Client 필드에 꽂는다.
		// Client가 인터페이스 타입이라 이런 교체가 가능하며, 이것이 인터페이스를 쓰는 실질적 이득이다.
		s := &Server{Client: c}
		// 반환값 두 개 중 첫 번째(정책)는 밑줄(_)로 버린다.
		// 여기서 관심사는 에러뿐이며, Go는 쓰지 않는 변수를 컴파일 에러로 막으므로 _로 명시해 버려야 한다.
		// context.Background()는 취소도 마감도 없는 최상위 빈 ctx이며, 테스트에서 흔히 쓰는 기본값이다.
		_, err := s.policyForTenant(context.Background(), "team-vision")
		// MatchError(ErrNoPolicy)는 내부적으로 errors.Is로 비교하므로, 감싸인(wrap된) 에러도 잡아낸다.
		// 에러 메시지 문자열을 비교하지 않는 이유는, 문구가 바뀌어도 테스트가 깨지지 않게 하기 위해서다.
		Expect(err).To(MatchError(ErrNoPolicy))
	})

	// 이 테스트가 막는 회귀(설계서 Identity model 절):
	// GPUQuotaPolicy는 클러스터 스코프라 같은 tenant를 가리키는 정책이 2개 이상 존재할 수 있다.
	// 이때 선택이 비결정적이면 요청마다 다른 정책이 적용되어 rate limit이 오락가락한다.
	// "가장 오래된 것이 이긴다"는 규칙이 실제로 지켜지는지 검증한다.
	It("returns the oldest policy when more than one matches the tenant", func() {
		// Go 문법 설명:
		//   - &platformv1.GPUQuotaPolicy{...}는 구조체 값을 만들고 그 주소(포인터)를 얻는 표현이다.
		//   - 중괄호 안의 필드명: 값 형태를 "구조체 리터럴"이라 하며, 원하는 필드만 골라 채울 수 있다.
		//     적지 않은 필드는 자동으로 제로값이 된다.
		//   - ObjectMeta는 이름/namespace/생성 시각 같은 공통 메타데이터를 담고, Spec은 이 타입 고유의 설정을 담는다.
		older := &platformv1.GPUQuotaPolicy{
			// CreationTimestamp 줄에 대한 설명:
			//   - metav1.NewTime(...)은 Go의 time.Time을 쿠버네티스의 metav1.Time으로 감싼다.
			//   - time.Now().Add(-time.Hour)는 현재 시각에 한 시간을 "빼서"(음수를 더해서) 과거 시각을 만든다.
			//   - 진짜로 한 시간을 기다릴 수는 없으므로 생성 시각을 직접 심어 과거를 흉내 내는 것이다.
			ObjectMeta: metav1.ObjectMeta{
				Name:              "team-vision-old",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)), // 한 시간 이른 생성 시각
			},
			// Spec.Tenant를 같은 값으로 두는 것이 이 테스트의 핵심이다.
			// 두 정책이 같은 tenant를 두고 경합하는 상황을 만들어야 tie-break 규칙을 시험할 수 있다.
			Spec: platformv1.GPUQuotaPolicySpec{Tenant: "team-vision"},
		}
		newer := &platformv1.GPUQuotaPolicy{
			// 이쪽 CreationTimestamp는 지금 시각이므로 older보다 한 시간 늦다.
			ObjectMeta: metav1.ObjectMeta{
				Name:              "team-vision-new",
				CreationTimestamp: metav1.NewTime(time.Now()),
			},
			Spec: platformv1.GPUQuotaPolicySpec{Tenant: "team-vision"},
		}
		// WithObjects(newer, older)로 두 정책을 가짜 클러스터에 미리 심는다.
		//
		// 인자 순서가 newer, older인 점이 의도적이다.
		// 구현이 시각을 비교하지 않고 그냥 "처음 찾은 것"을 고르는 버그가 있다면 newer가 뽑혀 이 테스트가 실패한다.
		// 즉 순서를 일부러 뒤집어 두어 버그가 우연히 숨지 못하게 한다.
		c := fake.NewClientBuilder().WithScheme(newSchemeForTest()).WithObjects(newer, older).Build()
		s := &Server{Client: c}
		got, err := s.policyForTenant(context.Background(), "team-vision")
		// HaveOccurred()는 "에러가 발생했다"를 뜻하므로, NotTo(...)를 붙이면 "에러가 없어야 한다"가 된다.
		// 정책이 존재하는 경로이므로 err는 nil이어야 한다.
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Name).To(Equal("team-vision-old")) // 더 오래된 정책이 뽑혀야 함
	})
})

// bucketRegistry에 대한 명세 묶음이다.
// 여기서는 쿠버네티스 client가 전혀 필요 없다(레지스트리는 순수한 인메모리 자료구조다).
var _ = Describe("bucketRegistry", func() {
	// 이 테스트가 막는 회귀(설계서 Components 절):
	// 토큰 버킷이 실제로 요청을 막지 못하면 rate limit 기능 전체가 무의미해진다.
	// burst=1이라는 최소 설정으로 "첫 요청은 통과, 두 번째는 차단"이라는 가장 기본적인 계약을 확인한다.
	It("limits per the rate and refreshes on change", func() {
		// newBucketRegistry()는 빈 레지스트리를 만드는 생성자다.
		// 소문자 이름이지만 같은 패키지라서 테스트에서 직접 부를 수 있다.
		b := newBucketRegistry()
		// RequestsPerMinute: 60은 초당 1개(60/60)로 환산되므로 토큰 보충이 1초에 하나뿐이다.
		// Burst: 1은 버킷 용량이 1이라는 뜻이며, 생성 직후 버킷은 가득 차 있으므로 토큰이 딱 하나 있다.
		// 이 조합이면 두 번째 요청은 1초를 기다려야 하므로 즉시 연속 호출 시 반드시 차단된다.
		// 시간에 의존하지 않고 결정적으로 판정되도록 일부러 이렇게 극단적인 값을 골랐다.
		rl := &platformv1.GPUQuotaRateLimit{RequestsPerMinute: 60, Burst: 1} // burst 1이라 첫 요청만 통과
		// 첫 호출은 남아 있던 토큰 하나를 소비하며 true를 준다.
		// BeTrue()/BeFalse()는 불리언 값을 검사하는 gomega 단언이다.
		Expect(b.Allow("t", rl)).To(BeTrue())
		// 바로 이어진 두 번째 호출은 토큰이 없으므로 false다.
		// 상위 핸들러는 이 false를 429 rate_limited로 변환한다.
		Expect(b.Allow("t", rl)).To(BeFalse()) // 두 번째는 한도 초과로 차단
	})

	// 이 테스트가 막는 회귀(설계서 Components 절):
	// rateLimit이 설정되지 않은(nil) tenant는 "무제한"이라는 뜻이다.
	// 여기서 nil 검사를 빠뜨리면 nil 포인터 역참조로 게이트웨이가 패닉을 일으키거나,
	// rpm=0으로 해석되어 무제한이어야 할 tenant의 모든 요청이 차단되는 정반대 동작이 된다.
	It("returns true for an unlimited (nil) rate limit", func() {
		b := newBucketRegistry()
		// 두 번째 인자로 nil을 넘겨 "rateLimit 설정 없음"을 표현한다.
		Expect(b.Allow("t", nil)).To(BeTrue()) // nil이면 무제한이라 항상 통과
	})
})
