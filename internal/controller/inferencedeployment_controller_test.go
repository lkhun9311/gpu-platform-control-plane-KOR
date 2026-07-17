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

// package 선언: 테스트 파일이지만 controller 패키지 안에 있다.
// _test 접미사 없이 같은 패키지에 두면 nvidiaGPUResource나 InferenceDeploymentReconciler 같은
// 비공개(소문자) 식별자에도 접근할 수 있어서 내부 동작까지 검증할 수 있다.
//
// 이 테스트들은 envtest 위에서 돈다.
// envtest는 진짜 API 서버와 etcd를 로컬에서 띄우는 방식이라, fake client와 달리 실제 API 검증/기본값/불변 필드 규칙이 그대로 적용된다.
// 다만 kube-controller-manager는 띄우지 않으므로 Deployment controller가 없다.
// 즉 pod가 실제로 뜨거나 dep.Status가 저절로 채워지지 않으며, 그래서 아래 테스트들은 Deployment status를 손으로 patch한다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
import (
	// context: 쿠버네티스 클라이언트 호출에 넘길 취소/타임아웃 신호 타입이다.
	"context"

	// ginkgo: Describe/Context/It 형태로 테스트를 구성하는 BDD 스타일 테스트 프레임워크다.
	// 앞의 점(.)은 "점 import"이며, 패키지 이름 없이 Describe(...)처럼 바로 쓰게 해 준다.
	// 일반 코드에서는 이름 충돌 위험 때문에 피하지만, Ginkgo/Gomega 테스트에서는 가독성을 위해 관례로 허용된다.
	. "github.com/onsi/ginkgo/v2"
	// gomega: Expect(...).To(...) 형태의 단언(assertion) 라이브러리이며 Ginkgo와 짝을 이룬다.
	. "github.com/onsi/gomega"
	// appsv1, corev1, metav1, types: 프로덕션 코드와 같은 쿠버네티스 API 타입들이다.
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	// client: DeleteAllOf의 옵션(client.InNamespace) 등을 쓰기 위해 필요하다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// reconcile: Reconcile에 넘길 Request 타입이 들어 있다.
	// ctrl.Request는 사실 이 reconcile.Request의 별칭이라 둘은 같은 타입이다.
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// Ginkgo 테스트 트리의 뿌리다.
//
// Go 문법 설명:
//   - var _ = ... 의 밑줄(_)은 "이 값을 변수에 담지 않고 버리겠다"는 표시다.
//     그런데도 이렇게 쓰는 이유는 대입문 우변인 Describe(...)를 실행시키기 위해서다.
//     패키지 수준 변수의 초기화식은 테스트가 시작되기 전에 자동으로 실행되며,
//     그 과정에서 Describe가 이 테스트 트리를 Ginkgo에 등록한다.
//   - func() { ... } 는 이름 없는 함수(클로저)이며, Describe에 넘겨져 그 안의 It들을 등록한다.
//
// 중요: Describe/Context/It/By에 넘기는 설명 문자열은 주석이 아니라 실행되는 코드 리터럴이다.
// 테스트 리포트에 그대로 출력되고 --focus 같은 필터의 대상이 되므로 번역하지 않고 원문을 유지한다.
var _ = Describe("InferenceDeployment Controller", func() {
	Context("When reconciling a resource", func() {
		// 이 테스트들이 공통으로 쓰는 고정 이름이다.
		// 모든 케이스가 같은 이름을 쓰므로 AfterEach의 정리가 반드시 동작해야 서로 간섭하지 않는다.
		const resourceName = "llama3-8b"
		const ns = "default"

		// context.Background()는 취소도 만료도 없는 최상위 빈 context다.
		// 테스트에서는 요청을 중간에 끊을 일이 없어서 이걸 그대로 쓴다.
		ctx := context.Background()
		// key: 이 테스트가 다루는 객체의 좌표다.
		// InferenceDeployment와 그것이 만드는 Deployment/Service가 모두 같은 이름을 쓰므로 key 하나로 셋 다 조회할 수 있다.
		key := types.NamespacedName{Name: resourceName, Namespace: ns}

		// reconciler: 테스트용 reconciler를 매번 새로 만들어 주는 헬퍼다.
		//
		// Go 문법 설명: 함수를 변수에 담았으므로 reconciler()처럼 호출한다.
		// k8sClient는 suite_test.go가 envtest API 서버에 연결해 만들어 둔 공유 클라이언트다.
		// 같은 패키지라서 import 없이 바로 쓸 수 있다.
		//
		// 변수 하나를 공유하지 않고 매번 새로 만드는 이유는 케이스 간에 상태가 새지 않게 하기 위해서다.
		reconciler := func() *InferenceDeploymentReconciler {
			return &InferenceDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		// newInfD: 테스트용 InferenceDeployment를 만들어 주는 팩토리다.
		// replicas와 gpu만 인자로 받고 나머지는 고정값이라, 각 테스트가 자기가 관심 있는 변수만 드러낼 수 있다.
		// 이렇게 하면 "이 테스트가 무엇을 바꿔서 무엇을 보는지"가 한눈에 들어온다.
		newInfD := func(replicas, gpu int32) *platformv1.InferenceDeployment {
			return &platformv1.InferenceDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ns},
				Spec: platformv1.InferenceDeploymentSpec{
					Model:    platformv1.InferenceModel{Name: "llama3-8b", StorageURI: "s3://models/llama3-8b/v1"},
					Image:    "vllm/vllm-openai:test",
					GPUClass: "a10g",
					GPUCount: gpu,
					Replicas: replicas,
					Port:     8080,
				},
			}
		}

		// AfterEach: 각 It이 끝날 때마다(성공하든 실패하든) 자동으로 실행되는 정리 블록이다.
		//
		// 모든 테스트가 같은 이름/네임스페이스를 재사용하므로 정리가 없으면 이전 테스트의 객체가 남아
		// 다음 테스트를 오염시킨다.
		// 특히 "refuses to adopt" 계열 테스트는 남의 소유 객체를 일부러 만들기 때문에,
		// 이게 안 지워지면 뒤따르는 테스트가 전부 Degraded로 실패하게 된다.
		AfterEach(func() {
			infd := &platformv1.InferenceDeployment{}
			// 이미 없을 수도 있으므로 Get이 성공한 경우에만 지운다.
			// err == nil 비교로 "존재함"을 확인하는 것이며, 없으면 조용히 넘어간다.
			if err := k8sClient.Get(ctx, key, infd); err == nil {
				Expect(k8sClient.Delete(ctx, infd)).To(Succeed())
			}
			// envtest에는 GC controller가 없어서 owner를 지워도 Deployment/Service가 자동으로 사라지지 않는다.
			// 그래서 소유 객체를 직접 지워야 한다.
			//
			// Go 문법 설명:
			//   - []client.Object{...} 는 인터페이스 슬라이스이며, 서로 다른 타입을 한 슬라이스에 담을 수 있다.
			//   - for _, obj := range ... 에서 밑줄(_)은 인덱스를 안 쓰겠다는 뜻이고, obj에 각 원소가 들어온다.
			//   - _ = ... 로 반환 에러를 버리는 이유는 지울 게 없어도 정리는 실패가 아니기 때문이다.
			//
			// DeleteAllOf는 조건에 맞는 객체를 한 번에 지우며, client.InNamespace(ns)로 범위를 좁힌다.
			for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
				_ = k8sClient.DeleteAllOf(ctx, obj, client.InNamespace(ns))
			}
		})

		// 기본 동작(happy path) 검증이다.
		// InferenceDeployment 하나를 만들고 reconcile하면 Deployment와 Service가 spec대로 생기고,
		// 둘 다 우리 소유로 표시되어야 한다.
		//
		// 이 테스트가 막는 회귀는 mutateDeployment/mutateService가 spec의 어떤 필드를 옮기다 빠뜨리는 것이다.
		// 특히 owner 참조 확인이 중요한데, 이게 없으면 InferenceDeployment를 지워도 Deployment가 고아로 남아 GPU를 계속 점유한다.
		It("creates an owned Deployment and Service with the model spec", func() {
			// replica 2개, GPU 1개짜리로 만든다.
			// Expect(...).To(Succeed())는 반환된 에러가 nil인지 확인하는 Gomega 관용구다.
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())

			// reconcile을 직접 호출한다.
			// Manager를 띄워 이벤트를 기다리지 않고 함수를 바로 부르므로, 테스트가 결정적이고 빠르다.
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Deployment가 원하는 모습대로 생겼는지 확인한다.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			// *dep.Spec.Replicas 의 별표는 포인터 역참조이며, Replicas가 *int32라서 값을 꺼내야 비교할 수 있다.
			// int32(2)로 명시 변환하는 이유는 Gomega의 Equal이 타입까지 엄격히 비교하기 때문이다.
			// 그냥 2를 쓰면 int 타입이라 int32와 다르다고 판정되어 실패한다.
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
			// selector가 instanceLabel로 우리 pod를 고르는지 확인한다.
			// selector는 불변 필드라 처음에 틀리면 나중에 고칠 수 없어서 특히 중요하다.
			Expect(dep.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", resourceName))
			// 컨테이너 spec이 InferenceDeployment의 값에서 제대로 왔는지 본다.
			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.Image).To(Equal("vllm/vllm-openai:test"))
			// 포트 이름이 "http"여야 하는데, probe와 Service의 TargetPort가 이 이름으로 참조하기 때문이다.
			// 이름이 틀리면 probe와 Service가 조용히 깨진다.
			Expect(c.Ports[0].Name).To(Equal("http"))
			Expect(c.Ports[0].ContainerPort).To(Equal(int32(8080)))
			// GPU가 requests와 limits 양쪽에 같은 값으로 들어갔는지 확인한다.
			// 확장 resource는 둘이 다르면 API 서버가 pod 생성을 거부하므로 반드시 같아야 한다.
			gpuReq := c.Resources.Requests[nvidiaGPUResource]
			gpuLim := c.Resources.Limits[nvidiaGPUResource]
			// .Value()는 Quantity에서 int64 숫자를 꺼내는 메서드다.
			// Quantity는 내부 표현이 여러 가지라("1", "1000m") 구조체를 직접 비교하면 안 되고 이렇게 값으로 비교해야 한다.
			Expect(gpuReq.Value()).To(Equal(int64(1)))
			Expect(gpuLim.Value()).To(Equal(int64(1)))
			// SetControllerReference가 제대로 걸렸는지 확인한다.
			// 이 표시가 owner GC와 ownedConflict 판정의 근거이므로 빠지면 두 기능이 동시에 무너진다.
			Expect(metav1.IsControlledBy(dep, mustGet(ctx, key))).To(BeTrue())

			// Service도 같은 방식으로 확인한다.
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
			// Service의 selector는 Deployment의 pod label과 맞아야 트래픽이 흐른다.
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/instance", resourceName))
			Expect(svc.Spec.Ports[0].Name).To(Equal("http"))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(metav1.IsControlledBy(svc, mustGet(ctx, key))).To(BeTrue())
		})

		// GPU를 요청하지 않은 경우 GPU resource 자체가 없어야 한다.
		//
		// 이 테스트가 막는 회귀는 mutateDeployment가 GPUCount == 0 일 때 "nvidia.com/gpu: 0"을 넣어 버리는 것이다.
		// 확장 resource는 0을 명시하는 것과 아예 없는 것이 다르게 취급될 수 있으므로 키가 없어야 옳다.
		It("omits the GPU resource when GPUCount is zero", func() {
			Expect(k8sClient.Create(ctx, newInfD(1, 0))).To(Succeed())
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			// map 조회는 값과 "존재 여부(ok)"를 함께 준다.
			// 여기서는 값은 필요 없고 존재 여부만 보므로 첫 반환값을 밑줄(_)로 버린다.
			_, hasReq := dep.Spec.Template.Spec.Containers[0].Resources.Requests[nvidiaGPUResource]
			Expect(hasReq).To(BeFalse())
		})

		// markDeploymentObserved: 실제 Deployment controller가 할 일을 흉내 내는 헬퍼다.
		//
		// envtest에는 kube-controller-manager가 없어서 dep.Status가 영원히 비어 있다.
		// 그래서 "pod가 다 떴다"는 상황을 만들려면 status를 직접 써 줘야 한다.
		// ObservedGeneration을 Generation과 맞추는 게 핵심인데, 이래야 computeInfDPhase의 stale gate를 통과한다.
		//
		// 세 count를 모두 같은 값으로 넣는 이유는 "완전히 수렴한 정상 상태"를 표현하기 위해서다.
		// k8sClient.Status().Update는 status 서브리소스만 갱신하며, 일반 Update로는 status를 못 바꾼다.
		//
		// 부재중인 Deployment controller를 대신해 Deployment status를 직접 patch
		markDeploymentObserved := func(ready int32) {
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = ready
			dep.Status.UpdatedReplicas = ready
			dep.Status.ReadyReplicas = ready
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		}

		// phase가 Progressing에서 Ready로 올바르게 전이하는지 본다.
		//
		// 이 테스트가 막는 회귀는 stale gate가 사라지는 것이다.
		// gate가 없으면 Deployment status가 아직 비어 있는(ObservedGeneration=0) 시점에도
		// 판정 로직이 그 빈 status를 사실로 믿고 엉뚱한 답을 내게 된다.
		It("reports Progressing then Ready as the Deployment becomes ready", func() {
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			// 여러 번 reconcile해야 하므로 reconciler를 변수에 담아 재사용한다.
			r := reconciler()
			// 첫 reconcile이 Deployment와 Service를 만든다.
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Deployment status가 아직 반영 전(observedGeneration 0)이라 Progressing
			//
			// 방금 만든 Deployment의 Generation은 1인데 Status.ObservedGeneration은 0이다.
			// 즉 stale이므로 stale gate에 걸려 Progressing이 나와야 한다.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Progressing"))

			// 이제 "replica 2개가 모두 준비됨"을 흉내 낸다.
			markDeploymentObserved(2)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := mustGet(ctx, key)
			// stale gate를 통과하고 세 count가 desired와 정확히 같으므로 Ready여야 한다.
			Expect(got.Status.Phase).To(Equal("Ready"))
			// status가 Deployment의 실제 ready 수를 그대로 전달하는지 확인한다.
			Expect(got.Status.ReadyReplicas).To(Equal(int32(2)))
			// ObservedGeneration이 현재 Generation과 같아야 사용자가 이 status를 최신으로 믿을 수 있다.
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		})

		// replica 0은 실패가 아니라 의도된 정상 상태이므로 Ready여야 한다.
		//
		// 이 테스트가 막는 회귀는 0 replica를 Pending이나 Degraded로 보고하는 것이다.
		// ReadyReplicas가 0이라는 이유만으로 Pending 규칙(4번)이 먼저 걸리면 그렇게 되는데,
		// ScaledToZero 규칙(2번)이 Pending보다 위에 있어야 이를 막는다.
		// Reason까지 확인하는 이유는 phase만 Ready면 "왜 Ready인지"를 구분할 수 없기 때문이다.
		It("reports Ready with ScaledToZero when replicas is zero", func() {
			Expect(k8sClient.Create(ctx, newInfD(0, 0))).To(Succeed())
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := mustGet(ctx, key)
			Expect(got.Status.Phase).To(Equal("Ready"))
			// findCondition은 이 패키지의 다른 테스트 파일에 있는 헬퍼이며, 조건 목록에서 Type으로 하나를 찾아 준다.
			cond := findCondition(got.Status.Conditions, "Available")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("ScaledToZero"))
		})

		// rollout이 시한을 넘겨 실패하면 Degraded로 보고해야 한다.
		//
		// 이 테스트가 막는 회귀는 실패한 rollout을 Pending으로 보고하는 것이다.
		// 실패한 rollout도 ReadyReplicas가 0이라, Degraded 검사(3번)가 Pending 검사(4번)보다 위에 있지 않으면
		// 영영 "곧 준비됩니다(Pending)"라고 거짓 보고하게 되어 운영자가 장애를 놓친다.
		It("reports Degraded when the Deployment exceeds its progress deadline", func() {
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// 실제 Deployment controller가 시한 초과 시 붙이는 condition을 손으로 만들어 준다.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			// stale gate를 먼저 통과시켜야 3번 검사까지 도달한다.
			// 이걸 빼먹으면 Degraded가 아니라 stale gate의 Progressing이 나와 테스트가 엉뚱하게 실패한다.
			dep.Status.ObservedGeneration = dep.Generation
			// Progressing=False + Reason=ProgressDeadlineExceeded 조합이 rollout 실패의 표준 신호다.
			dep.Status.Conditions = []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
			}}
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Degraded"))
		})

		// 소유하지 않은 동명 Deployment를 절대 입양하지 않는지 검증한다.
		//
		// 이 테스트가 막는 회귀는 워크로드 탈취이며, 이 파일에서 가장 보안상 중요한 케이스다.
		// ownedConflict 확인이 사라지면 CreateOrUpdate가 이름만 보고 남의 Deployment를 우리 spec으로 덮어쓴다.
		// 그러면 아무나 InferenceDeployment를 하나 만드는 것만으로 남의 운영 중인 워크로드를 자기 image로 갈아치울 수 있다.
		//
		// 그래서 세 가지를 모두 확인한다.
		// 첫째 우리 status가 Degraded인지, 둘째 남의 객체에 owner 참조가 안 붙었는지, 셋째 남의 image가 그대로인지다.
		// 특히 셋째가 핵심인데, 실제 탈취가 일어났는지를 직접 보는 단언이기 때문이다.
		It("refuses to adopt an unowned Deployment of the same name", func() {
			// 남이 소유한 Deployment를 같은 이름으로 미리 만들어 둔다.
			// Selector/Template/Containers는 API 서버가 요구하는 필수 필드라 채워야 Create가 통과한다.
			foreign := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ns, Labels: map[string]string{"owner": "someone-else"}},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"owner": "someone-else"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"owner": "someone-else"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())

			// 충돌은 에러가 아니라 status로 보고해야 한다.
			// 재시도해도 풀리지 않는 문제라 에러를 내면 백오프 재시도만 무한 반복하기 때문이다.
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// 우리 쪽은 Degraded로 문제를 드러내야 한다.
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Degraded"))
			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			// owner 참조가 안 붙었는지 확인한다.
			// 붙었다면 나중에 InferenceDeployment 삭제 시 GC가 남의 Deployment까지 지워 버린다.
			Expect(got.OwnerReferences).To(BeEmpty())
			// image가 그대로 busybox인지 확인한다.
			// 만약 vllm/vllm-openai:test로 바뀌었다면 탈취가 실제로 일어난 것이다.
			Expect(got.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox"))
		})

		// scale-down 도중 replica가 남아도는 상황에서 Ready로 올리면 안 된다.
		//
		// 이 테스트가 막는 회귀는 computeInfDPhase의 6번 검사(정확히 같은지 비교)가 사라지는 것이다.
		// 앞선 5번 검사는 전부 "미만(<)" 비교라서 개수가 넘치는 경우를 못 잡는다.
		// 세 count가 모두 3이고 desired가 2라면 3 >= 2 이므로 5번을 그냥 통과해 Ready가 나와 버린다.
		// 하지만 pod가 3개 떠 있는데 "원하는 2개 상태에 도달함"이라고 하는 건 거짓 보고다.
		It("reports Progressing (not Ready) when Replicas=3 but desired is 2 (surplus not yet removed)", func() {
			// desired replica 2개로 InferenceDeployment 생성
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// scale down 진행 중 상황 재현으로, Deployment controller가 총 replica 3개(모두 updated, 모두 ready)를 보고하지만,
			// 잉여 old replica는 아직 제거되지 않은 상태.
			//
			// 여기서 stale gate는 통과시키고(ObservedGeneration = Generation) 개수만 넘치게 만든 게 핵심이다.
			// 그래야 6번 검사만 단독으로 시험할 수 있다.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 3
			dep.Status.UpdatedReplicas = 3
			dep.Status.ReadyReplicas = 3
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			// Ready로 올리면 안 됨, 총 replica 수가 아직 desired=2로 수렴하지 않음
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Progressing"))
		})

		// 0으로의 scale-down을 Deployment가 아직 관측하지 못했을 때 성급히 Ready라고 하면 안 된다.
		//
		// 이 테스트가 막는 회귀는 stale gate(1번)와 ScaledToZero(2번)의 순서가 뒤집히는 것이다.
		// 순서가 뒤집히면 spec.Replicas == 0 이라는 이유만으로 즉시 Ready를 보고한다.
		// 하지만 그 시점에 pod 2개는 여전히 떠 있고 GPU도 잡고 있으므로 명백한 거짓말이다.
		// 이 거짓 Ready를 믿고 다음 단계를 진행하는 자동화가 있다면 실제 사고로 이어진다.
		It("does not prematurely report Ready when scale-down to zero is not yet observed by the Deployment", func() {
			// ready replica 2개로 시작
			//
			// 먼저 정상 Ready 상태를 만들어 둔다.
			// 그래야 이후 Progressing이 나올 때 "원래 Ready였는데 제대로 내려갔다"는 전이를 확인할 수 있다.
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			markDeploymentObserved(2)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Ready"))

			// InferenceDeployment spec을 0으로 scale down
			//
			// spec을 바꾸면 API 서버가 Generation을 1 올린다.
			// 그래서 Deployment의 status가 곧바로 stale이 된다.
			infd := mustGet(ctx, key)
			infd.Spec.Replicas = 0
			Expect(k8sClient.Update(ctx, infd)).To(Succeed())

			// Reconcile 시점에 이제 Deployment spec은 Replicas=0이지만,
			// Deployment status는 아직 이전 generation을 가리킴(ObservedGeneration < Generation).
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// stale gate가 ScaledToZero보다 먼저 걸리므로 성급하게 Ready로 올리면 안 됨
			//
			// ObservedGeneration을 Generation - 1로 되돌려 "아직 관측하지 못한 상태"를 명시적으로 만든다.
			// 이때 Status.Replicas는 여전히 2이므로 zeroAndDrained 예외에도 해당하지 않는다.
			// 즉 stale gate가 반드시 걸려야 하는 조건이 갖춰진다.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation - 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			// Ready가 아니라 Progressing이어야 한다.
			// pod가 실제로 다 빠진 뒤에야 Ready가 될 자격이 생긴다.
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Progressing"))
		})

		// 멱등성 검증이며, 안정 상태에서 다시 reconcile해도 아무것도 쓰지 않아야 한다.
		//
		// 이 테스트가 막는 회귀는 무한 reconcile 루프다.
		// 바뀐 게 없는데도 Update를 보내면 resourceVersion이 올라가고, 그 watch 이벤트가 다시 reconcile을 부른다.
		// 그러면 controller가 자기 자신을 영원히 깨우며 API 서버를 두들기게 된다.
		//
		// resourceVersion을 비교하는 게 핵심인데, 이 값은 객체에 쓰기가 일어날 때만 바뀌기 때문이다.
		// 즉 "정말로 API 호출이 없었는지"를 확인하는 가장 직접적인 증거다.
		It("is idempotent once steady", func() {
			// replica 0으로 생성해 첫 pass에서 바로 Ready에 도달하도록 하여,
			// 수동 Deployment status patch 없이도 진행되게 함.
			//
			// replica가 0이면 zeroAndDrained 예외 덕분에 stale gate 없이 한 번에 Ready가 된다.
			// 즉 status를 손으로 건드리지 않아도 안정 상태를 만들 수 있어서 테스트가 단순해진다.
			Expect(k8sClient.Create(ctx, newInfD(0, 0))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// 두 번째 reconcile 직전의 resourceVersion을 기록해 둔다.
			depBefore := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, depBefore)).To(Succeed())
			infdBefore := mustGet(ctx, key)

			// 두 번째 reconcile은 Deployment나 InferenceDeployment 어느 것도 갱신하면 안 됨
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// resourceVersion이 그대로면 쓰기가 한 번도 없었다는 뜻이다.
			// Deployment 쪽은 CreateOrUpdate의 변경 감지가, InferenceDeployment 쪽은 DeepEqual 가드가 각각 지켜 준다.
			depAfter := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, depAfter)).To(Succeed())
			Expect(depAfter.ResourceVersion).To(Equal(depBefore.ResourceVersion))
			Expect(mustGet(ctx, key).ResourceVersion).To(Equal(infdBefore.ResourceVersion))
		})

		// drift 복구 검증이며, 누가 손으로 바꿔 놓은 값을 다음 reconcile이 되돌려야 한다.
		//
		// 이 테스트가 막는 회귀는 mutateDeployment가 생성 시에만 동작하고 갱신 시에는 필드를 안 건드리는 것이다.
		// 그러면 desired state를 계속 강제한다는 controller의 존재 이유 자체가 무너진다.
		// image를 고른 이유는 drift 중에서도 가장 위험하기 때문인데, 검증되지 않은 image가 계속 서빙될 수 있다.
		It("restores a drifted Deployment image", func() {
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// drift 재현을 위해 Deployment image를 직접 변조
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Spec.Template.Spec.Containers[0].Image = "tampered:bad"
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			// 다음 reconcile에서 변조된 image를 덮어써야 함
			//
			// mutateDeployment가 Containers 슬라이스를 통째로 교체하므로 변조된 값이 사라진다.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// InferenceDeployment의 spec.Image 값으로 되돌아왔는지 확인한다.
			restored := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, restored)).To(Succeed())
			Expect(restored.Spec.Template.Spec.Containers[0].Image).To(Equal("vllm/vllm-openai:test"))
		})

		// Deployment와 같은 방어를 Service에도 적용하는지 검증한다.
		//
		// 이 테스트가 막는 회귀는 Service 쪽 ownedConflict 확인만 빠뜨리는 것이다.
		// Deployment만 막고 Service를 안 막는 실수는 실제로 흔한데, 두 검사가 별개의 코드 경로라서다.
		//
		// Service 탈취는 Deployment 탈취보다 더 은밀하고 위험하다.
		// selector를 우리 것으로 바꾸는 순간 남에게 가던 트래픽이 조용히 우리 pod로 흘러들기 때문이며,
		// 남의 Deployment는 멀쩡히 살아 있어서 눈에 띄지도 않는다.
		It("refuses to adopt an unowned Service of the same name", func() {
			// 남이 소유한 Service를 같은 이름으로 미리 만들어 둔다.
			foreignSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ns, Labels: map[string]string{"owner": "someone-else"}},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"owner": "someone-else"},
					Ports:    []corev1.ServicePort{{Name: "foreign", Port: 9999}},
				},
			}
			Expect(k8sClient.Create(ctx, foreignSvc)).To(Succeed())
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())

			// 여기서도 충돌은 에러가 아니라 status로 보고한다.
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Service를 소유하지 않았으므로 InferenceDeployment는 Degraded여야 함
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Degraded"))

			// Service가 adopt되거나 덮어써지면 안 됨
			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.OwnerReferences).To(BeEmpty())
			// selector가 남의 것 그대로여야 한다.
			// 우리 instanceLabel로 바뀌었다면 트래픽 탈취가 실제로 일어난 것이다.
			Expect(got.Spec.Selector).To(HaveKeyWithValue("owner", "someone-else"))
		})

		// GPU resource 제거 검증이며, GPUCount를 1에서 0으로 바꾸면 GPU 필드가 사라져야 한다.
		//
		// 이 테스트가 막는 회귀는 "추가만 하고 제거는 못 하는" mutate 로직이다.
		// mutateDeployment가 매번 container를 새로 만드는 대신 기존 container를 부분 수정하도록 바뀌면,
		// GPUCount가 0이 되어도 예전 GPU requests/limits가 그대로 남는다.
		// 그러면 GPU가 필요 없어진 pod가 계속 GPU를 점유해 클러스터의 비싼 자원이 낭비되고,
		// 다른 워크로드는 스케줄되지 못한다.
		It("removes the GPU resource when GPUCount changes from 1 to 0", func() {
			// GPU 1개로 InferenceDeployment 생성 후 resource가 설정됐는지 확인
			//
			// 먼저 GPU가 실제로 붙었는지 확인해야, 나중에 사라진 것이 "제거됨"의 증거가 된다.
			// 이 사전 확인이 없으면 애초에 안 붙은 것과 구분할 수 없다.
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].Resources.Requests).To(HaveKey(nvidiaGPUResource))
			Expect(dep.Spec.Template.Spec.Containers[0].Resources.Limits).To(HaveKey(nvidiaGPUResource))

			// GPUCount를 0으로 바꾸고 reconcile하면 GPU resource가 제거돼야 함
			infd := mustGet(ctx, key)
			infd.Spec.GPUCount = 0
			Expect(k8sClient.Update(ctx, infd)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// requests와 limits 양쪽 모두에서 사라졌는지 확인한다.
			// 한쪽만 남아도 requests와 limits가 달라져 API 서버가 pod 생성을 거부한다.
			updated := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			_, hasReq := updated.Spec.Template.Spec.Containers[0].Resources.Requests[nvidiaGPUResource]
			_, hasLim := updated.Spec.Template.Spec.Containers[0].Resources.Limits[nvidiaGPUResource]
			Expect(hasReq).To(BeFalse())
			Expect(hasLim).To(BeFalse())
		})
	})
})

// mustGet: InferenceDeployment를 읽어 돌려주되, 읽기에 실패하면 그 자리에서 테스트를 실패시킨다.
//
// Go 문법 설명:
//   - Go 관례에서 must 접두사는 "실패하면 그냥 중단한다"는 뜻이며, 에러를 반환하지 않는다.
//   - 그래서 반환값이 (*InferenceDeployment, error)가 아니라 포인터 하나뿐이다.
//   - Expect(...).To(Succeed())가 실패하면 Ginkgo가 이 지점에서 테스트를 중단하므로,
//     호출한 쪽은 반환값이 항상 유효하다고 믿고 mustGet(ctx, key).Status.Phase 처럼 바로 이어 쓸 수 있다.
//
// 이 헬퍼가 없으면 모든 단언마다 Get + 에러 확인 두 줄이 붙어 테스트의 의도가 파묻힌다.
func mustGet(ctx context.Context, key types.NamespacedName) *platformv1.InferenceDeployment {
	infd := &platformv1.InferenceDeployment{}
	Expect(k8sClient.Get(ctx, key, infd)).To(Succeed())
	return infd
}
