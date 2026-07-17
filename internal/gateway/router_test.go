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

package gateway

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// newInfD: 테스트용 InferenceDeployment를 한 줄로 만드는 헬퍼다.
//
// Go 문법 설명.
//
//   - 인자가 많아 호출부가 길어지지만, 테스트마다 구조체 리터럴을 반복해 쓰는 것보다 의도가 잘 드러난다.
//   - metav1.NewTime(...)은 time.Time을 쿠버네티스가 쓰는 metav1.Time으로 감싼다.
func newInfD(name, ns, model string, port int32, created time.Time) *platformv1.InferenceDeployment {
	return &platformv1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: platformv1.InferenceDeploymentSpec{
			Model: platformv1.InferenceModel{Name: model},
			Port:  port,
		},
	}
}

// newRouterClient: ModelNameIndex를 등록한 fake client를 만든다.
//
// 왜 인덱스를 따로 등록하는가.
//
// 운영에서는 server.go의 NewCache가 이 인덱스를 캐시에 걸어 준다.
//
// fake client는 그 캐시를 쓰지 않으므로, 테스트에서 같은 키와 같은 추출 함수로 직접 등록해야 client.MatchingFields{ModelNameIndex: ...} 조회가 운영과 똑같이 동작한다.
//
// 등록하지 않으면 조회 자체가 "unknown index" 에러로 실패한다.
func newRouterClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(newSchemeForTest()).
		WithIndex(&platformv1.InferenceDeployment{}, ModelNameIndex, func(o client.Object) []string {
			return []string{o.(*platformv1.InferenceDeployment).Spec.Model.Name}
		}).
		WithObjects(objs...).
		Build()
}

// policyFor: TargetNamespace만 지정한 최소 정책을 만든다.
//
// backendFor는 정책에서 TargetNamespace만 읽으므로 나머지 필드는 채울 필요가 없다.
//
// 왜 ns 인자를 유지하는가(unparam 경고를 끄는 이유).
//
// 지금은 모든 호출이 "vision"을 넘기고 있어 unparam 린터가 "인자가 항상 같은 값이니 없애라"고 지적한다.
//
// 하지만 이 인자는 각 호출부에서 "정책이 어느 namespace로 범위를 좁히는가"를 눈에 보이게 하는 역할을 한다.
//
// 특히 tenant 격리 테스트는 InferenceDeployment를 "nlp"에 두고 정책은 "vision"으로 물어 ErrNoRoute를 기대한다.
//
// 그 대비는 호출부에 두 namespace가 나란히 적혀 있어야 드러난다.
//
// 인자를 없애면 policyFor()가 되어 그 대비가 헬퍼 안으로 숨고, 격리 테스트가 무엇을 시험하는지 읽히지 않는다.
//
// nolint 주석은 실행되는 코드가 아니라 golangci-lint에게 이 한 곳만 예외로 두라고 주는 지시문이다.
//
// nolint:unparam
func policyFor(ns string) *platformv1.GPUQuotaPolicy {
	return &platformv1.GPUQuotaPolicy{
		Spec: platformv1.GPUQuotaPolicySpec{TargetNamespace: ns},
	}
}

var _ = Describe("backendFor", func() {
	var (
		ctx = context.Background()
		now = time.Now()
	)

	It("returns ErrNoRoute when no InferenceDeployment serves the model", func() {
		// 이 테스트가 막는 회귀: 없는 model을 요청했을 때 조용히 nil URL을 돌려주면 상위 handler가 nil로 프록시를 시도해 panic이 난다.
		//
		// 설계서 Error codes 절에 따라 이 경우는 404가 되어야 하므로, 구분 가능한 센티넬 에러가 필요하다.
		s := &Server{Client: newRouterClient()}
		_, err := s.backendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).To(MatchError(ErrNoRoute))
	})

	It("resolves the model to its Service URL", func() {
		// 정상 경로: model 이름이 InferenceDeployment의 Service URL로 해석되어야 한다.
		//
		// Service 이름은 InferenceDeployment 이름과 같고, 포트는 spec.port를 따른다.
		infd := newInfD("llama", "vision", "llama-3-8b", 9000, now)
		s := &Server{Client: newRouterClient(infd)}
		got, err := s.backendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.String()).To(Equal("http://llama.vision.svc:9000"))
	})

	It("defaults the port to 8080 when spec.port is unset", func() {
		// spec.port에는 +kubebuilder:default=8080이 걸려 있어 API server를 거친 객체는 항상 값이 채워진다.
		//
		// 하지만 fake client는 defaulting을 적용하지 않아 0이 그대로 남는다.
		//
		// 포트 0으로 URL을 만들면 프록시가 붙지 못하므로 코드 쪽에서도 같은 기본값을 보장해야 한다.
		infd := newInfD("llama", "vision", "llama-3-8b", 0, now)
		s := &Server{Client: newRouterClient(infd)}
		got, err := s.backendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.String()).To(Equal("http://llama.vision.svc:8080"))
	})

	It("uses the older InferenceDeployment when more than one serves the model", func() {
		// 같은 model을 서빙하는 InferenceDeployment가 둘 이상인 것을 막을 수단이 없다.
		//
		// 그런 경우 요청마다 다른 backend로 가면 안 되므로, 설계서는 결정론적으로 가장 오래된 것을 쓰라고 정한다.
		older := newInfD("llama-old", "vision", "llama-3-8b", 8080, now.Add(-time.Hour))
		newer := newInfD("llama-new", "vision", "llama-3-8b", 8080, now)
		// 일부러 newer를 먼저 넣어, 저장 순서가 아니라 생성 시각으로 고르는지 확인한다.
		s := &Server{Client: newRouterClient(newer, older)}
		got, err := s.backendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.String()).To(Equal("http://llama-old.vision.svc:8080"))
	})

	It("picks the lexicographically smaller name when creation timestamps tie", func() {
		// 같은 초에 만들어진 둘 중 이름이 앞선 쪽이 뽑히는지 확인한다.
		//
		// 주의: 이 테스트만으로는 비결정성 회귀를 잡지 못한다.
		//
		// fake client가 목록을 이름순으로 정렬해 돌려주기 때문에, tie-break 승자(가장 작은 이름)가 이미 항상 첫 번째로 온다.
		//
		// 그래서 시각만 비교하는 잘못된 구현도 여기서는 통과한다.
		//
		// 결정론 규칙 자체는 아래 olderInfD 단위 테스트가 고정한다.
		same := now
		b := newInfD("llama-b", "vision", "llama-3-8b", 8080, same)
		a := newInfD("llama-a", "vision", "llama-3-8b", 8080, same)
		s := &Server{Client: newRouterClient(b, a)}
		got, err := s.backendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.String()).To(Equal("http://llama-a.vision.svc:8080"))
	})

	It("ignores an InferenceDeployment outside the policy's target namespace", func() {
		// 이 테스트가 막는 회귀: namespace 필터를 빠뜨리면 다른 tenant의 backend로 요청이 새어 나간다.
		//
		// 같은 model 이름을 서로 다른 tenant가 쓰는 것은 충분히 흔한 일이라 실제로 일어날 수 있다.
		other := newInfD("llama", "nlp", "llama-3-8b", 8080, now)
		s := &Server{Client: newRouterClient(other)}
		_, err := s.backendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).To(MatchError(ErrNoRoute))
	})
})

var _ = Describe("olderInfD", func() {
	now := time.Now()

	It("prefers the earlier creation timestamp", func() {
		older := newInfD("z-name", "vision", "m", 8080, now.Add(-time.Hour))
		newer := newInfD("a-name", "vision", "m", 8080, now)
		// 이름이 뒤여도 시각이 이르면 이긴다.
		//
		// 즉 1차 기준은 어디까지나 시각이다.
		Expect(olderInfD(older, newer)).To(BeTrue())
		Expect(olderInfD(newer, older)).To(BeFalse())
	})

	It("breaks an exact timestamp tie by ascending name", func() {
		// 이 테스트가 막는 회귀: CreationTimestamp는 초 단위 정밀도라, 같은 초에 만들어진 두 객체는 시각이 완전히 같다.
		//
		// 시각만 비교하면 양방향 비교가 모두 false가 되어 둘 사이에 순서가 정의되지 않는다.
		//
		// 그 상태로 정렬하면 어느 쪽이 뽑힐지가 입력 순서에 좌우되고, 캐시가 돌려주는 순서는 보장되지 않는다.
		//
		// 그러면 같은 요청이 매번 다른 backend로 갈 수 있는데, 설계서 Identity model 절이 금지하는 상태다.
		//
		// 이름은 namespace 안에서 유일하므로, 이름 tie-break가 있어야 순서가 항상 한쪽으로 결정된다.
		same := now
		a := newInfD("llama-a", "vision", "m", 8080, same)
		b := newInfD("llama-b", "vision", "m", 8080, same)
		Expect(olderInfD(a, b)).To(BeTrue())
		// 반대 방향이 false여야 순서가 정의된 것이다.
		//
		// 둘 다 false면 비결정적이라는 뜻이다.
		Expect(olderInfD(b, a)).To(BeFalse())
	})
})
