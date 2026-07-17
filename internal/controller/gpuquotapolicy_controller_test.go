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

// package 선언: 파일 이름이 _test.go로 끝나면 go test로 실행될 때만 컴파일된다.
// package controller(테스트 대상과 같은 패키지)로 두었기 때문에 gpuQuotaFinalizer나 phaseSynced 같은 비공개 상수를 그대로 쓸 수 있다.
// 이걸 package controller_test로 두면 비공개 식별자에 접근하지 못한다.
//
// 이 파일의 테스트는 envtest(진짜 kube-apiserver와 etcd 바이너리) 위에서 돈다.
// suite_test.go가 그 API 서버를 띄우고 아래에서 쓰는 k8sClient를 채워 준다.
// mock이 아니라 진짜 API 서버를 쓰는 이유는 finalizer, status subresource, CRD validation, owner reference 같은
// 이 controller가 의존하는 동작이 전부 API 서버 쪽 기능이라 fake client로는 검증이 안 되기 때문이다.
package controller

import (
	// context: 쿠버네티스 클라이언트 호출에 넘길 취소/타임아웃 신호 타입이다.
	"context"

	// ginkgo: Describe/Context/It로 테스트를 서술형으로 구조화하는 BDD 테스트 프레임워크다.
	// gomega: Expect(...).To(...) 형태로 단언을 쓰는 matcher 라이브러리다.
	//
	// Go 문법 설명:
	//   - import 앞의 점(.)은 "dot import"이며, 패키지 이름 없이 식별자를 바로 쓰게 해 준다.
	//   - 즉 ginkgo.Describe가 아니라 그냥 Describe로, gomega.Expect가 아니라 그냥 Expect로 쓸 수 있다.
	//   - dot import는 이름 충돌 위험 때문에 보통 피하지만, Ginkgo/Gomega는 문장처럼 읽히게 하려고 공식적으로 이 방식을 권장한다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// corev1: Namespace, ResourceQuota 같은 core API 그룹 v1 타입들이다.
	corev1 "k8s.io/api/core/v1"
	// errors: 여기서는 표준 errors가 아니라 쿠버네티스의 api/errors다.
	// IsNotFound, IsAlreadyExists로 API 에러의 종류를 판별한다.
	// (controller 본문에서는 apierrors라는 별칭을 썼지만 여기선 표준 errors를 안 쓰므로 별칭이 필요 없다.)
	"k8s.io/apimachinery/pkg/api/errors"
	// resource: GPU 개수 같은 수량을 담는 Quantity 타입을 만든다.
	"k8s.io/apimachinery/pkg/api/resource"
	// metav1: ObjectMeta, Condition 등 모든 객체가 공유하는 메타데이터 타입이다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// types: NamespacedName(이름+namespace)으로 객체를 가리키는 키 타입이다.
	"k8s.io/apimachinery/pkg/types"
	// reconcile: reconcile.Request 타입을 제공하며 ctrl.Request와 사실상 같은 타입이다.
	// (ctrl.Request는 이 타입의 별칭이므로 어느 쪽을 써도 동작이 같다.)
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// Describe: 이 블록 전체가 무엇에 대한 테스트인지 묶어 주는 Ginkgo의 최상위 컨테이너다.
//
// Go 문법 설명:
//   - var _ = ... 에서 밑줄(_)은 "빈 식별자"이며 "값은 받되 이름은 안 붙인다"는 뜻이다.
//     Go는 쓰지 않는 변수를 컴파일 에러로 막는데, 이 트릭으로 그 규칙을 피한다.
//   - Describe는 함수라 패키지 최상위에서 그냥 호출할 수 없어서, 이렇게 변수 초기화식으로 감싸 실행시킨다.
//     그래서 Go 런타임이 패키지를 로드할 때 이 Describe가 실행되어 테스트 트리가 등록된다.
//   - func() { ... } 는 이름 없는 함수(익명 함수)이며, Describe에 통째로 넘겨 나중에 실행되게 한다.
//
// 참고: Describe/Context/It/By에 넘기는 영문 문자열은 주석이 아니라 실행되는 코드 리터럴이다.
// 테스트 리포트에 그대로 출력되는 이름이라 번역하지 않는다.
var _ = Describe("GPUQuotaPolicy Controller", func() {
	// Context: 같은 상황(전제 조건)을 공유하는 테스트들을 묶는 Ginkgo 컨테이너이며, 기능은 Describe와 같고 이름만 다르다.
	Context("When reconciling a resource", func() {
		// 테스트 전반에서 쓰는 고정값들이다.
		// resourceName: 테스트가 만들 GPUQuotaPolicy의 이름이다.
		const resourceName = "test-quota"
		// targetNS: 정책이 quota를 걸 대상 namespace다.
		const targetNS = "tenant-quota-ns"
		// gpuResource: 검증할 quota key이며, controller의 gpuRequestsResource와 같은 값이다.
		// 상수를 재사용하지 않고 문자열을 다시 적은 이유는, 이 key가 바뀌면 테스트가 실패해 알려 주도록 하기 위해서다.
		// (상수를 그대로 쓰면 controller와 테스트가 함께 바뀌어 실수를 못 잡는다.)
		const gpuResource = corev1.ResourceName("requests.nvidia.com/gpu")

		// context.Background()는 취소도 시한도 없는 최상위 빈 ctx이며, 테스트에서는 이걸로 충분하다.
		ctx := context.Background()
		// key: 정책을 가리키는 키다.
		// Namespace를 안 채운 이유는 GPUQuotaPolicy가 cluster-scoped라 namespace가 없기 때문이다.
		key := types.NamespacedName{Name: resourceName}
		// rqKey: controller가 만들 ResourceQuota의 예상 위치다.
		// 이름 규칙("gpuquota-" + 정책 이름)을 quotaName() 함수를 부르지 않고 직접 적어, 이름 규칙이 바뀌면 테스트가 잡아내게 한다.
		rqKey := types.NamespacedName{Name: "gpuquota-" + resourceName, Namespace: targetNS}

		// reconciler: 매번 새 reconciler 인스턴스를 만들어 주는 헬퍼다.
		//
		// Go 문법 설명:
		//   - 변수에 함수를 대입하는 형태이며, Go에서 함수는 값처럼 주고받을 수 있는 일급 값이다.
		//   - k8sClient는 suite_test.go가 envtest API 서버에 연결해 만들어 둔 패키지 수준 변수다.
		//   - k8sClient.Scheme()으로 스킴을 넘겨야 controller가 SetControllerReference에서 owner reference를 만들 수 있다.
		//
		// 매 호출마다 새로 만드는 이유는 테스트 사이에 reconciler 내부 상태가 새지 않게 하기 위해서다.
		reconciler := func() *GPUQuotaPolicyReconciler {
			return &GPUQuotaPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		// reconcileUntilSteady: Reconcile을 세 번 연달아 불러 안정 상태(steady state)까지 밀어붙이는 헬퍼다.
		//
		// 왜 여러 번 부르는가:
		//   - 이 controller는 한 번의 Reconcile에서 모든 걸 끝내지 않는다.
		//   - 1회차는 finalizer만 붙이고 early return하고, 2회차에 ResourceQuota를 만들고, 3회차에 status가 정리된다.
		//   - 실제 클러스터에서는 Update가 만든 watch event가 다음 호출을 자동으로 유발하지만,
		//     테스트는 Reconcile을 직접 부르므로 그 연쇄를 손으로 흉내 내야 한다.
		//   - 3번은 여유분까지 포함한 횟수이며, controller가 멱등하므로 필요보다 더 불러도 결과가 달라지지 않는다.
		//     즉 이 헬퍼 자체가 "여러 번 불러도 안전하다"는 멱등성을 은연중에 검증한다.
		//
		// Go 문법 설명:
		//   - for range 3 은 Go 1.22에서 들어온 문법으로 "0,1,2 세 번 반복"이라는 뜻이다(인덱스 변수를 안 쓸 때 쓴다).
		//   - _, err := ... 의 밑줄(_)은 첫 반환값인 ctrl.Result를 버린다는 뜻이며, 여기선 requeue 여부가 관심사가 아니다.
		//   - Expect(err).NotTo(HaveOccurred())는 "에러가 나지 않아야 한다"는 Gomega 단언이다.
		reconcileUntilSteady := func() {
			for range 3 {
				_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		// BeforeEach: 아래 It 하나하나가 실행되기 직전에 매번 새로 실행되는 준비 블록이다.
		// 각 테스트가 항상 같은 출발점에서 시작하도록 보장해, 테스트 사이의 순서 의존성을 없앤다.
		BeforeEach(func() {
			// By는 테스트 리포트에 진행 단계를 남기는 Ginkgo 함수이며, 실패했을 때 어디서 깨졌는지 읽기 쉽게 해 준다.
			By("ensuring the target namespace exists")
			// GPUQuotaPolicy는 cluster-scoped지만 ResourceQuota는 namespace 안에 사는 객체다.
			// namespace가 없으면 ResourceQuota 생성이 실패하므로 먼저 만들어 둔다.
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNS}}
			// envtest의 namespace는 한 번 만들면 테스트 스위트가 끝날 때까지 남으므로, 두 번째 테스트부터는 AlreadyExists가 난다.
			// 그래서 AlreadyExists만 정상으로 흡수하고 나머지 에러는 실패로 처리한다.
			if err := k8sClient.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating a GPUQuotaPolicy")
			// 모든 테스트의 출발점이 되는 기본 정책이다.
			// GPUCount 8이 "원하는 상한"이고, 아래 테스트들은 이 8이 ResourceQuota에 제대로 반영되는지를 본다.
			policy := &platformv1.GPUQuotaPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: platformv1.GPUQuotaPolicySpec{
					Tenant:          "team-vision",
					TargetNamespace: targetNS,
					GPUClass:        "l40s",
					Limits:          platformv1.GPUQuotaLimits{GPUCount: 8},
				},
			}
			// Expect(...).To(Succeed())는 error를 돌려주는 호출이 성공했는지 확인하는 Gomega 관용구다.
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		})

		// AfterEach: 각 It이 끝난 뒤 매번 실행되는 정리 블록이며, 다음 테스트에 잔재가 남지 않게 한다.
		//
		// 여기서 finalizer를 손으로 떼는 게 핵심이다.
		// envtest에는 garbage collection controller가 없어 finalizer가 붙은 객체는 Delete해도 영영 사라지지 않는다.
		// 그러면 다음 테스트의 BeforeEach가 같은 이름으로 Create할 때 AlreadyExists로 실패한다.
		// 그래서 Finalizers를 nil로 만들어 Update한 뒤 Delete해야 실제로 지워진다.
		AfterEach(func() {
			policy := &platformv1.GPUQuotaPolicy{}
			// err == nil이면 객체가 아직 있다는 뜻이므로 그때만 정리한다.
			// 이미 지워진 경우(삭제 테스트 이후)에는 조용히 건너뛴다.
			if err := k8sClient.Get(ctx, key, policy); err == nil {
				policy.Finalizers = nil
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
			}
			// ResourceQuota도 마찬가지로, owner reference가 있어도 envtest는 자동 정리를 안 해 주므로 직접 지운다.
			rq := &corev1.ResourceQuota{}
			if err := k8sClient.Get(ctx, rqKey, rq); err == nil {
				Expect(k8sClient.Delete(ctx, rq)).To(Succeed())
			}
		})

		// 정상 경로(happy path) 검증이다.
		// 정책 하나를 만들고 reconcile하면 (1)상한이 맞는 ResourceQuota가 생기고 (2)finalizer가 붙고 (3)status가 Synced로 보고되는지를 한 번에 본다.
		// 이 테스트가 막는 회귀: 상한 계산이 어긋나거나(예: 8이 아닌 값), finalizer를 안 붙여 정리가 안 되거나, status를 안 써서 관측 불가능해지는 것.
		It("syncs a ResourceQuota with the GPU ceiling and reports Synced", func() {
			reconcileUntilSteady()

			// 실제로 API 서버에 ResourceQuota가 생겼는지 읽어서 확인한다.
			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())
			// map에서 값을 꺼내 지역 변수 q에 담는다.
			// 바로 rq.Spec.Hard[gpuResource].Value()라고 쓰지 못하는 이유는, map 조회 결과는 주소를 얻을 수 없는 값이라
			// 포인터 리시버 메서드인 Value()를 곧바로 부를 수 없기 때문이다(그래서 변수로 한 번 받는다).
			q := rq.Spec.Hard[gpuResource]
			// Value()는 Quantity를 int64로 바꿔 준다.
			// Equal(int64(8))처럼 타입까지 명시하는 이유는 Gomega의 Equal이 타입까지 엄격히 비교하기 때문이다(8은 int, Value()는 int64).
			Expect(q.Value()).To(Equal(int64(8)))

			// 정책 쪽 상태도 확인한다.
			got := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			// finalizer가 붙어야 나중에 삭제 시 ResourceQuota 정리가 보장된다.
			Expect(got.Finalizers).To(ContainElement(gpuQuotaFinalizer))
			Expect(got.Status.Phase).To(Equal(phaseSynced))
			// ObservedGeneration == Generation은 "이 status가 최신 spec을 보고 쓴 것"이라는 뜻이다.
			// 이게 어긋나면 사용자는 낡은 status를 최신인 줄 알고 읽게 된다.
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			// condition은 사람과 도구가 함께 읽는 표준 보고 창구이므로 phase와 별개로 검증한다.
			cond := findCondition(got.Status.Conditions, conditionSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		// 멱등성(idempotency) 검증이다.
		// 안정 상태에 도달한 뒤 한 번 더 reconcile해도 정책과 ResourceQuota가 전혀 안 바뀌어야 한다.
		//
		// ResourceVersion으로 확인하는 이유는, 이 값이 API 서버가 객체를 쓸 때마다 올려 주는 카운터라서
		// "값이 그대로 = 쓰기가 아예 일어나지 않았다"를 정확히 증명해 주기 때문이다.
		//
		// 이 테스트가 막는 회귀: 매 reconcile마다 무조건 Update/Status().Update를 날리는 코드다.
		// 그런 코드는 watch event를 스스로 만들어 Reconcile을 다시 부르는 무한 루프(hot loop)가 되고, API 서버에 부하를 준다.
		// 특히 setQuotaPhase의 early return이나 status의 DeepEqual 비교를 지우면 이 테스트가 바로 깨진다.
		It("is idempotent once steady", func() {
			reconcileUntilSteady()

			// 추가 reconcile 전의 ResourceVersion을 기록해 둔다.
			before := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
			rqBefore := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rqBefore)).To(Succeed())

			// 이미 원하는 상태에 도달한 상태에서 한 번 더 부른다.
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// 두 객체 모두 ResourceVersion이 그대로여야 한다(= 아무것도 쓰지 않았다).
			after := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			rqAfter := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rqAfter)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(rqAfter.ResourceVersion).To(Equal(rqBefore.ResourceVersion))
		})

		// drift recovery 검증 그 첫 번째로, "객체가 통째로 사라진" 경우다.
		// 누가 ResourceQuota를 지워 버려도 controller가 다시 만들어 상한이 복구돼야 한다.
		//
		// 이 테스트가 막는 회귀: 생성을 "정책이 처음 만들어질 때 한 번"으로만 처리하는 코드다.
		// 그런 코드는 quota가 지워진 뒤 영영 복구되지 않아, 테넌트가 상한 없이 GPU를 쓰게 되는 보안/과금 사고로 이어진다.
		// controller가 매 reconcile마다 실제 상태를 Get해 desired와 맞추기 때문에 복구가 가능하다.
		It("recreates the ResourceQuota after it is deleted (drift recovery)", func() {
			reconcileUntilSteady()

			// 사람이나 다른 도구가 quota를 지운 상황을 흉내 낸다.
			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())
			Expect(k8sClient.Delete(ctx, rq)).To(Succeed())

			reconcileUntilSteady()

			// 같은 이름, 같은 상한으로 되살아났는지 확인한다.
			// 이름이 같아야 한다는 점이 quotaName()이 순수 함수여야 하는 이유이기도 하다.
			restored := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, restored)).To(Succeed())
			q := restored.Spec.Hard[gpuResource]
			Expect(q.Value()).To(Equal(int64(8)))
		})

		// drift recovery 검증 그 두 번째로, "객체는 있는데 값이 변조된" 경우다.
		// 누가 상한을 99로 올려 놔도 controller가 정책 값인 8로 되돌려야 한다.
		//
		// 이 테스트가 막는 회귀: "객체가 있으면 그냥 두는(create-if-missing)" 코드다.
		// 그런 코드는 상한을 몰래 올리는 변조를 그대로 방치해, 정책이 사실상 무력화된다.
		// 이 시나리오가 앞의 삭제 케이스와 별개로 필요한 이유는 코드 경로가 다르기 때문이다(Create가 아니라 DeepEqual 비교 후 Update).
		It("corrects a mutated ResourceQuota hard limit", func() {
			reconcileUntilSteady()

			// 상한을 8에서 99로 몰래 올린 상황을 흉내 낸다.
			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())
			rq.Spec.Hard[gpuResource] = *resource.NewQuantity(99, resource.DecimalSI)
			Expect(k8sClient.Update(ctx, rq)).To(Succeed())

			reconcileUntilSteady()

			// 정책이 정한 8로 되돌아왔는지 확인한다.
			corrected := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, corrected)).To(Succeed())
			q := corrected.Spec.Hard[gpuResource]
			Expect(q.Value()).To(Equal(int64(8)))
		})

		// finalizer 기반 정리 검증이다.
		// 정책을 지우면 (1)동기화해 둔 ResourceQuota도 지워지고 (2)finalizer가 떨어져 정책 자체도 실제로 사라져야 한다.
		//
		// 이 테스트가 막는 회귀 두 가지:
		//   - 정리를 안 해서 quota만 남는 고아 객체 문제(정책은 없는데 상한만 남아 워크로드가 막힌다).
		//   - finalizer를 안 떼서 정책이 Terminating 상태에 영원히 갇히는 문제.
		// 두 번째가 특히 위험한데, 사람이 손으로 finalizer를 떼기 전까지 그 이름을 다시 쓸 수 없게 된다.
		It("deletes the ResourceQuota and clears the finalizer on deletion", func() {
			reconcileUntilSteady()

			// 지우기 전에 quota가 실제로 있었음을 확인해 둔다(없는 걸 지웠다고 착각하는 헛된 통과를 막는다).
			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())

			// 정책 삭제를 요청한다.
			// finalizer가 붙어 있으므로 이 시점에는 객체가 사라지지 않고 DeletionTimestamp만 찍힌다.
			toDelete := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			// 삭제 경로의 reconcile이 quota를 지우고 finalizer를 떼어 준다.
			reconcileUntilSteady()

			// 둘 다 완전히 사라졌는지 확인한다.
			// Get을 인자로 바로 넘겨 그 반환 error를 IsNotFound로 판정하는 압축된 표현이다.
			// &platformv1.GPUQuotaPolicy{}처럼 빈 객체를 즉석에서 만들어 넘기는 이유는, 결과를 담을 그릇만 필요하고 내용은 안 볼 것이기 때문이다.
			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &platformv1.GPUQuotaPolicy{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, rqKey, &corev1.ResourceQuota{}))).To(BeTrue())
		})

		// 소유권 충돌 검증이다.
		// 목표 이름을 남이 이미 쓰고 있으면 controller는 가로채지 않고 Degraded로 물러나야 한다.
		//
		// 이 테스트가 막는 회귀: 무조건 덮어쓰는(create-or-update) 코드다.
		// 그런 코드는 다른 팀이나 다른 도구가 관리하던 ResourceQuota를 조용히 훼손하는데,
		// quota는 워크로드 스케줄링을 직접 막는 객체라 남의 상한을 바꾸면 남의 서비스가 죽는다.
		// 그래서 "모르는 객체는 건드리지 않고 사람에게 보고한다"가 안전한 기본값이다.
		It("refuses to overwrite a ResourceQuota it does not own and reports Degraded", func() {
			By("pre-creating an unowned ResourceQuota occupying the policy's target name")
			// controller가 쓰려던 바로 그 이름/namespace를 미리 점유한다.
			// owner reference를 일부러 안 달았으므로 IsControlledBy 판정에서 "남의 것"으로 걸린다.
			// 상한을 3으로 둔 이유는 정책 값 8과 달라서, 덮어쓰기가 일어나면 값이 8로 바뀌어 바로 들통나게 하기 위해서다.
			foreign := &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{Name: rqKey.Name, Namespace: rqKey.Namespace},
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					gpuResource: *resource.NewQuantity(3, resource.DecimalSI),
				}},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			reconcileUntilSteady()

			By("leaving the foreign ResourceQuota untouched (not hijacked)")
			got := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, got)).To(Succeed())
			q := got.Spec.Hard[gpuResource]
			// 3 그대로여야 한다(8로 바뀌었다면 남의 quota를 덮어쓴 것이다).
			Expect(q.Value()).To(Equal(int64(3)))
			// owner reference도 안 붙어야 한다(붙였다면 소유권을 몰래 가로챈 것이며, 나중에 정책이 지워질 때 남의 quota까지 삭제된다).
			Expect(got.OwnerReferences).To(BeEmpty())

			By("reporting Degraded with Synced=False on the policy")
			// 조용히 실패하면 안 되고, 사람이 알아챌 수 있게 status로 보고해야 한다.
			policy := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
			Expect(policy.Status.Phase).To(Equal(phaseDegraded))
			cond := findCondition(policy.Status.Conditions, conditionSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			// Reason까지 확인하는 이유는, 무슨 이유로든 실패하면 통과하는 느슨한 테스트가 되지 않게 하기 위해서다.
			// QuotaConflict라는 구체적 사유가 나와야 운영자가 원인을 즉시 안다.
			Expect(cond.Reason).To(Equal(reasonQuotaConflict))
		})

		// CRD validation 검증이며, 이 테스트는 controller 코드가 아니라 API 타입의 규칙을 본다.
		// targetNamespace를 나중에 바꾸는 것은 CEL validation rule로 API 서버 단에서 막혀 있어야 한다.
		//
		// 왜 immutable이어야 하는가: 대상 namespace를 바꾸면 controller는 새 namespace에 quota를 만드는데,
		// 옛 namespace의 quota는 이름이 정책 이름 기반이라 그대로 남아 아무도 정리하지 않는 고아가 된다.
		// 그래서 애초에 변경 자체를 API 서버가 거부하게 하는 편이 안전하다.
		//
		// 이 테스트가 막는 회귀: CRD 정의에서 immutability rule이 실수로 빠지거나 manifest 재생성 때 사라지는 것.
		// 에러 문구까지 확인하므로 "다른 이유로 실패해서 통과"하는 헛된 통과를 방지한다.
		It("rejects a change to the immutable targetNamespace", func() {
			policy := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
			policy.Spec.TargetNamespace = "some-other-ns"
			err := k8sClient.Update(ctx, policy)
			// 성공하면 안 되고 반드시 에러가 나야 한다.
			Expect(err).To(HaveOccurred())
			// ContainSubstring은 문구 전체가 아니라 일부만 맞으면 되는 matcher이며,
			// API 서버가 앞뒤로 덧붙이는 부가 정보에 영향받지 않게 해 준다.
			Expect(err.Error()).To(ContainSubstring("targetNamespace is immutable"))
		})

		// 선택 필드(optional field)의 왕복(round-trip) 검증이다.
		// rateLimit은 이 controller가 쓰지 않고 게이트웨이가 읽는 필드이지만, CRD 스키마에 제대로 정의돼 있어야 저장되고 다시 읽힌다.
		//
		// 이 테스트가 막는 회귀: 구조체에 필드는 있는데 CRD 스키마 생성이 누락돼 값이 조용히 사라지는 것이다.
		// 쿠버네티스 API 서버는 스키마에 없는 필드를 에러 없이 버리므로(pruning), 이런 버그는 런타임에야 드러난다.
		// 게이트웨이 입장에서는 rate limit이 통째로 무시되는 셈이라, 쓰기와 읽기를 함께 확인하는 이 테스트가 필요하다.
		It("round-trips the optional rateLimit", func() {
			p := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
			// RateLimit은 포인터 필드라 &로 주소를 넘긴다.
			// 포인터인 이유는 "설정 안 함(nil)"과 "0으로 설정함"을 구분하기 위해서이며, 게이트웨이는 nil을 무제한으로 해석한다.
			p.Spec.RateLimit = &platformv1.GPUQuotaRateLimit{RequestsPerMinute: 600, Burst: 100}
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			// 캐시가 아니라 API 서버에서 다시 읽어 값이 실제로 저장됐는지 확인한다.
			got := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			// int32(600)처럼 타입을 명시하는 이유는 Gomega의 Equal이 타입까지 비교하기 때문이다.
			Expect(got.Spec.RateLimit.RequestsPerMinute).To(Equal(int32(600)))
			Expect(got.Spec.RateLimit.Burst).To(Equal(int32(100)))
		})
	})
})
