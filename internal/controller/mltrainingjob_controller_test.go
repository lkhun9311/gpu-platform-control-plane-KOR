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

// 이 파일은 _test.go로 끝나므로 go test 실행 시에만 컴파일되며, 최종 바이너리에는 포함되지 않는다.
// package 이름이 controller_test가 아니라 controller이므로 패키지 내부의 비공개 식별자도 그대로 쓸 수 있다.
package controller

import (
	// context: 아래 클라이언트 호출에 넘길 취소 신호 타입이다.
	"context"

	// 앞의 점(.)은 "dot import"라는 문법이며, 해당 패키지의 공개 식별자를 접두사 없이 쓰게 해준다.
	// 그래서 ginkgo.Describe가 아니라 Describe로, gomega.Expect가 아니라 Expect로 바로 쓸 수 있다.
	// 보통 dot import는 이름 충돌 위험 때문에 피하지만, Ginkgo/Gomega는 테스트를 문장처럼 읽히게 하려고 관례적으로 이 방식을 쓴다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// errors: 쿠버네티스 API 에러를 종류별로 판별하는 도우미이며, 여기서는 errors.IsNotFound를 쓴다.
	"k8s.io/apimachinery/pkg/api/errors"
	// types: 오브젝트를 지목하는 NamespacedName(이름 + 네임스페이스) 타입이 들어 있다.
	"k8s.io/apimachinery/pkg/types"
	// reconcile: Reconcile에 직접 넘길 reconcile.Request 타입이 들어 있다.
	// 테스트는 컨트롤러 런타임을 거치지 않고 Reconcile을 손으로 호출하므로 요청을 직접 만들어야 한다.
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// metav1: 모든 쿠버네티스 오브젝트가 공통으로 갖는 메타데이터(ObjectMeta 등) 타입이다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(MLTrainingJob 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// 이 블록은 MLTrainingJob 컨트롤러의 envtest 기반 테스트 묶음이다.
// envtest는 진짜 kube-apiserver와 etcd 바이너리를 띄우므로, CRD 검증과 status 서브리소스 같은 API 서버 동작까지 실제로 확인할 수 있다.
// 여기서 쓰는 k8sClient와 CRD 설치는 suite_test.go가 미리 준비해 둔다.
//
// Go 문법 설명:
//   - var _ = ... 에서 밑줄(_)은 "값을 버리는 빈 식별자"다.
//     Describe(...)는 반환값이 있는 함수 호출이라 그냥 문장으로 둘 수 없기에, 전역 변수 초기화 식으로 감싸 패키지 로드 시점에 실행시킨다.
//     이렇게 하면 go test가 시작되기 전에 Ginkgo가 이 명세들을 자기 트리에 등록한다.
//   - func() { ... } 는 이름 없는 함수(익명 함수)이며, Ginkgo는 이 함수 안의 구조를 읽어 테스트 트리를 만든다.
//
// 주의: Describe/Context/It/By에 넘기는 설명 문자열은 주석이 아니라 실행되는 코드 리터럴이며, 테스트 리포트에 그대로 출력되므로 번역하지 않는다.
var _ = Describe("MLTrainingJob Controller", func() {
	Context("When reconciling a resource", func() {
		// const: 컴파일 시점에 값이 고정되는 상수 선언이다.
		// 테스트 전반에서 같은 이름을 재사용하므로 오타로 인한 미스매치를 막아 준다.
		const resourceName = "test-training"
		const resourceNamespace = "default"

		// context.Background()는 아무 취소/기한도 없는 최상위 빈 컨텍스트이며, 테스트처럼 수명이 짧은 코드에서 흔히 쓰는 출발점이다.
		ctx := context.Background()

		// typeNamespacedName: 테스트 대상 오브젝트를 지목하는 키다.
		// MLTrainingJob은 네임스페이스 스코프 리소스라 이름과 네임스페이스가 모두 필요하다.
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		// mltrainingjob: BeforeEach에서 "이미 존재하는지" 확인할 때 읽어 담을 빈 그릇이다.
		// &platformv1.MLTrainingJob{}는 모든 필드가 제로값인 구조체를 만들고 그 주소를 얻는다.
		mltrainingjob := &platformv1.MLTrainingJob{}

		// BeforeEach: 아래 It 하나하나가 실행되기 직전마다 매번 호출되는 준비 훅이다.
		// 각 테스트가 동일한 출발 상태에서 시작하도록 보장해 테스트 간 순서 의존을 없앤다.
		BeforeEach(func() {
			// By(...)는 테스트가 실패했을 때 어느 단계에서 깨졌는지 보여주는 진행 표시이며, 실행되는 코드다.
			By("creating the custom resource for the Kind MLTrainingJob")
			// 먼저 읽어 보고, 없을 때만 만든다.
			// envtest의 API 서버는 스펙 하나가 끝나도 초기화되지 않으므로, 무조건 Create하면 AlreadyExists로 실패할 수 있다.
			err := k8sClient.Get(ctx, typeNamespacedName, mltrainingjob)
			// errors.IsNotFound(err)는 "그냥 아직 없음"과 "진짜 API 오류"를 구분한다.
			// 이 구분을 안 하면 API 서버 장애를 "없으니 만들자"로 오인하게 된다.
			if err != nil && errors.IsNotFound(err) { // 아직 없을 때만 새로 생성
				// 테스트 픽스처를 만든다.
				// 아래 spec 값들은 이 CRD가 실제 GPU 학습 잡을 표현할 수 있는지(그리고 CRD 검증을 통과하는지) 보여주는 대표 예시다.
				resource := &platformv1.MLTrainingJob{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: platformv1.MLTrainingJobSpec{
						Queue:       "team-vision-queue",
						Image:       "pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime",
						Command:     []string{"python", "train.py"},
						GPUClass:    "l40s",
						GPUCount:    2,
						Parallelism: 2,
						Completions: 2,
					},
				}
				// Expect(...).To(Succeed())는 "이 호출이 nil error를 돌려줘야 한다"는 Gomega 단언이다.
				// Succeed()는 error를 반환하는 호출 전용 매처다.
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		// AfterEach: 각 It이 끝난 뒤 매번 호출되는 정리 훅이며, 성공/실패와 무관하게 실행된다.
		// 여기서 지워 주지 않으면 남은 오브젝트가 다음 스펙의 BeforeEach에 영향을 준다.
		AfterEach(func() {
			resource := &platformv1.MLTrainingJob{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			// 정리 대상이 반드시 있어야 한다고 단언한다.
			// 없다면 테스트가 픽스처를 예상치 못하게 지웠다는 뜻이므로, 조용히 넘어가지 않고 여기서 드러낸다.
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance MLTrainingJob")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		// 이 테스트가 막는 회귀:
		// (1) CRD 스키마가 spec 필드를 잃어버리거나 잘못된 타입으로 저장해 round-trip이 깨지는 경우,
		// (2) M1의 빈 Reconcile이 에러를 뱉거나 패닉하는 경우다.
		// 즉 "CRD가 쓴 대로 되읽히는가"와 "컨트롤러 배선이 살아 있는가"를 한 번에 확인하는 기본 검증이다.
		It("should round-trip the spec and reconcile without error", func() {
			By("reading the created resource back")
			// API 서버에 저장된 것을 새 그릇에 다시 읽어 온다.
			// 로컬 변수를 그대로 검사하면 "저장/역직렬화가 제대로 됐는지"를 전혀 확인하지 못하므로 반드시 되읽어야 한다.
			fetched := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fetched)).To(Succeed())
			// 필드별로 우리가 넣은 값이 그대로 살아 돌아왔는지 확인한다.
			Expect(fetched.Spec.Queue).To(Equal("team-vision-queue"))
			Expect(fetched.Spec.Image).To(Equal("pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime"))
			// 슬라이스는 Equal로 요소별 비교가 되며, 순서까지 같아야 통과한다.
			Expect(fetched.Spec.Command).To(Equal([]string{"python", "train.py"}))
			// int32(2)처럼 타입을 명시하는 이유는 Gomega의 Equal이 타입까지 엄격하게 비교하기 때문이다.
			// 그냥 2를 쓰면 int로 추론되어 int32와 다른 타입이라 실패한다.
			Expect(fetched.Spec.GPUCount).To(Equal(int32(2)))
			Expect(fetched.Spec.Parallelism).To(Equal(int32(2)))
			Expect(fetched.Spec.Completions).To(Equal(int32(2)))

			By("Reconciling the created resource")
			// 컨트롤러 런타임을 띄우지 않고 재조정기를 직접 만들어 손으로 호출한다.
			// 이러면 이벤트 전달 타이밍에 의존하지 않아 테스트가 결정론적으로 돈다.
			controllerReconciler := &MLTrainingJobReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			// _, err := ... 에서 첫 반환값(ctrl.Result)은 밑줄로 버린다.
			// M1의 Reconcile은 재큐를 요청하지 않으므로 Result에 확인할 내용이 없다.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// 이 테스트가 막는 회귀: CRD에서 status 서브리소스 설정이 빠지는 경우다.
		// 서브리소스가 없으면 Status().Update가 조용히 실패하거나 spec까지 덮어쓰게 되고,
		// 그러면 컨트롤러가 상태를 보고할 방법을 잃는다.
		It("should persist a status phase via the status subresource", func() {
			const phasePending = "Pending"

			fetched := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fetched)).To(Succeed())

			fetched.Status.Phase = phasePending
			// k8sClient.Status()는 본체가 아니라 status 서브리소스만 대상으로 하는 writer를 준다.
			// 일반 Update와 분리되어 있어야 사용자의 spec 편집과 컨트롤러의 status 기록이 서로를 덮어쓰지 않는다.
			Expect(k8sClient.Status().Update(ctx, fetched)).To(Succeed()) // status subresource로만 갱신

			// 다시 읽어 실제로 서버에 저장됐는지 확인한다.
			// 로컬 구조체는 이미 값을 갖고 있으니, 되읽지 않으면 아무것도 증명하지 못한다.
			updated := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(phasePending))
		})
	})
})
