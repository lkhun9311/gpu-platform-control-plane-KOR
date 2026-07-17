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

// 이 파일은 _test.go로 끝나므로 go test 실행 시에만 컴파일된다.
// package 이름이 controller이므로 nodeHealthFinalizer나 unhealthyTaintKey 같은 비공개 식별자도 그대로 쓸 수 있다.
package controller

import (
	// context: 아래 클라이언트 호출과 Reconcile에 넘길 취소 신호 타입이다.
	"context"

	// 앞의 점(.)은 dot import이며, Describe/Expect 등을 패키지 접두사 없이 쓰게 해준다.
	// Ginkgo/Gomega는 테스트가 문장처럼 읽히도록 관례적으로 이 방식을 쓴다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	// corev1: Node, Taint, NodeCondition 등 코어 API 타입이다.
	corev1 "k8s.io/api/core/v1"
	// errors: 쿠버네티스 API 에러를 종류별로 판별하며, 여기서는 errors.IsNotFound를 쓴다.
	"k8s.io/apimachinery/pkg/api/errors"
	// metav1: ObjectMeta, Condition 등 공통 메타데이터 타입이다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// types: 오브젝트를 지목하는 NamespacedName 타입이다.
	"k8s.io/apimachinery/pkg/types"
	// reconcile: Reconcile에 직접 넘길 reconcile.Request 타입이다.
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(NodeHealth 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// 이 블록은 NodeHealth 컨트롤러의 envtest 기반 테스트 묶음이다.
// 이 컨트롤러는 실제 Node에 taint를 밀어넣는 enforcement 컨트롤러라 잘못 동작하면 노드가 영구히 스케줄 불가가 될 수 있다.
// 그래서 아래 스펙들은 "격리가 되는가"만이 아니라 "격리가 정확히 되돌려지는가", "남의 것을 건드리지 않는가"까지 촘촘히 검증한다.
// envtest가 진짜 API 서버를 띄우므로 CRD 검증 웹훅과 status 서브리소스 동작도 실제로 확인된다.
//
// Go 문법 설명: var _ = ... 의 밑줄(_)은 값을 버리는 빈 식별자다.
// Describe(...)는 반환값이 있는 호출이라 전역 변수 초기화 식으로 감싸야 패키지 로드 시점에 실행되어 Ginkgo 트리에 등록된다.
//
// 주의: Describe/Context/It/By의 설명 문자열은 주석이 아니라 리포트에 출력되는 코드 리터럴이므로 번역하지 않는다.
var _ = Describe("NodeHealth Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-nodehealth"
		const nodeName = "test-node"

		ctx := context.Background()
		// NodeHealth와 Node는 둘 다 클러스터 스코프라 NamespacedName에 Name만 채운다.
		nhKey := types.NamespacedName{Name: resourceName}
		nodeKey := types.NamespacedName{Name: nodeName}

		// reconciler: 매번 새 재조정기를 만들어 주는 헬퍼다.
		// 재조정기를 재사용하지 않고 새로 만드는 이유는, 컨트롤러가 호출 사이에 숨은 상태를 들고 있지 않음을 함께 보장하기 위해서다.
		//
		// Go 문법 설명: 함수도 값이라 변수에 담을 수 있으며, func() *NodeHealthReconciler {...} 는 인자 없이 포인터를 돌려주는 익명 함수다.
		reconciler := func() *NodeHealthReconciler {
			return &NodeHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		// finalizer가 추가되고 status가 안정된 값에 도달하도록 Reconcile을 몇 번 구동
		//
		// 여러 번 도는 이유가 중요하다.
		// 컨트롤러는 finalizer를 붙인 뒤 곧바로 return하며 그 pass를 끝내므로(낡은 resourceVersion을 들고 있지 않으려고),
		// 실제 격리 판정은 그다음 pass에서야 일어난다.
		// 실제 운영에서는 Update가 만든 이벤트가 컨트롤러 런타임을 통해 자동으로 다음 pass를 부르지만,
		// 테스트는 런타임 없이 손으로 호출하므로 그 재큐를 이 루프가 대신한다.
		//
		// Go 문법 설명: for range 3 은 Go 1.22에서 들어온 문법으로, 인덱스 변수 없이 정확히 3번 반복한다.
		reconcileUntilSteady := func() {
			for range 3 {
				_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		// makeNode: 주어진 Ready 상태를 갖는 Node 픽스처를 만드는 헬퍼다.
		// corev1.ConditionStatus를 인자로 받아 Ready 노드와 not-ready 노드를 같은 코드로 찍어낸다.
		makeNode := func(ready corev1.ConditionStatus) *corev1.Node {
			return &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: ready},
					},
				},
			}
		}

		// BeforeEach: 각 It 직전에 매번 실행되어 동일한 출발 상태를 만든다.
		// 여기서는 NodeHealth만 만들고 Node는 각 스펙이 필요에 따라 직접 만든다.
		// "노드가 없는 경우"를 검증하는 스펙이 있기 때문에 노드 생성을 공통 훅에 두지 않는 것이다.
		BeforeEach(func() {
			By("creating a NodeHealth pointing at the target node")
			nh := &platformv1.NodeHealth{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec:       platformv1.NodeHealthSpec{NodeName: nodeName},
			}
			Expect(k8sClient.Create(ctx, nh)).To(Succeed())
		})

		// AfterEach: 각 It 후에 성공/실패와 무관하게 실행되는 정리 훅이다.
		//
		// finalizer를 강제로 비우고 지우는 이유가 핵심이다.
		// 컨트롤러가 붙인 finalizer가 남아 있으면 Delete를 해도 오브젝트가 사라지지 않고 삭제 대기 상태로 남아,
		// 다음 스펙의 BeforeEach가 Create에서 AlreadyExists로 실패한다.
		// 테스트 정리에서는 컨트롤러를 또 돌려 finalizer를 떼기보다 직접 걷어내는 편이 단순하고 확실하다.
		AfterEach(func() {
			nh := &platformv1.NodeHealth{}
			// if err := ...; err == nil 로 "있으면 지운다"를 표현한다.
			// 이미 지워진 경우(삭제 검증 스펙들)에도 정리가 실패하지 않게 하려는 것이다.
			if err := k8sClient.Get(ctx, nhKey, nh); err == nil {
				nh.Finalizers = nil
				Expect(k8sClient.Update(ctx, nh)).To(Succeed())
				Expect(k8sClient.Delete(ctx, nh)).To(Succeed())
			}
			node := &corev1.Node{}
			if err := k8sClient.Get(ctx, nodeKey, node); err == nil {
				Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			}
		})

		// 이 테스트가 막는 회귀: 컨트롤러가 finalizer를 붙이지 않거나, Ready 노드를 잘못 판정하는 경우다.
		// finalizer가 없으면 삭제 시 taint 정리 기회를 잃으므로, 정상 경로에서 finalizer가 실제로 붙었는지부터 확인한다.
		It("adds a finalizer and reports Ready for a ready node", func() {
			node := makeNode(corev1.ConditionTrue)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			// Create는 status를 저장하지 않으므로 Status().Update를 따로 불러야 한다.
			// status는 서브리소스라 본체 생성과 분리되어 있고, 이걸 빠뜨리면 노드의 Ready condition이 비어 not-ready로 오판된다.
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Finalizers).To(ContainElement(nodeHealthFinalizer))
			Expect(got.Status.Phase).To(Equal(phaseReady))
			// ObservedGeneration이 Generation과 같아야 "이 status가 최신 spec을 반영했다"고 말할 수 있다.
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			cond := findCondition(got.Status.Conditions, conditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		// 이 테스트가 막는 회귀: 상태가 안정된 뒤에도 재조정마다 status를 되쓰는 hot loop다.
		// 매번 쓰면 resourceVersion이 오르고, 그 쓰기가 다시 watch 이벤트를 만들어 컨트롤러가 스스로를 무한히 깨운다.
		// ResourceVersion이 그대로인지 보는 것이 "아무것도 쓰지 않았다"를 확인하는 가장 정확한 방법이다.
		It("is idempotent once steady", func() {
			node := makeNode(corev1.ConditionTrue)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			before := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, before)).To(Succeed())

			// 안정된 상태에서 한 번 더 돌린다.
			// 이 pass는 아무 쓰기도 하지 않아야 한다.
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			after := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, after)).To(Succeed())
			// ResourceVersion은 오브젝트가 쓰일 때만 바뀌므로, 같으면 쓰기가 없었다는 증거다.
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		})

		// 이 테스트가 막는 회귀: 컨트롤러가 status를 한 번 쓰고 나서 이후 drift를 바로잡지 않는 경우다.
		// 컨트롤러는 "한 번 설정하고 끝"이 아니라 실제 상태로 계속 수렴시켜야 한다.
		// 누군가(사람이든 다른 컨트롤러든) status를 틀어 놓아도 다음 재조정이 진실로 되돌려야 한다.
		It("recovers from manual status drift", func() {
			node := makeNode(corev1.ConditionTrue)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			drifted := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, drifted)).To(Succeed())
			drifted.Status.Phase = phaseQuarantine // 상태를 수동으로 틀어 drift 유발
			Expect(k8sClient.Status().Update(ctx, drifted)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			// 노드는 여전히 Ready이므로 위조된 Quarantine은 Ready로 정정되어야 한다.
			Expect(got.Status.Phase).To(Equal(phaseReady))
		})

		// 이 테스트가 막는 회귀: 격리 자체가 동작하지 않는 경우다.
		// not-ready 노드에 taint가 걸리지 않으면 스케줄러가 고장난 노드에 계속 GPU 워크로드를 얹게 되며, 이 컨트롤러의 존재 이유가 사라진다.
		// taint 개수를 정확히 1로 보는 것은 중복 부착까지 함께 막기 위해서다.
		It("quarantines a not-ready node: taints it, sets phase and fault signal", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phaseQuarantine))
			Expect(got.Status.FaultSignal).NotTo(BeNil())
			// 결함 출처를 확인해 "왜 격리됐는지"가 상태에 정확히 남는지 검증한다.
			Expect(got.Status.FaultSignal.Source).To(Equal(faultSourceNodeNotReady))

			// status만 믿지 않고 실제 Node를 되읽어 taint가 진짜 있는지 본다.
			// enforcement 컨트롤러에서는 "주장"이 아니라 "실제 반영"이 검증 대상이다.
			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(1)) // taint 정확히 하나 부여 확인
		})

		// 이 테스트가 막는 회귀: taint를 걸기만 하고 되돌리지 못하는 경우다.
		// 격리를 풀지 못하면 일시적으로 not-ready였던 노드가 복구된 뒤에도 영원히 스케줄 불가로 남아 클러스터 용량이 조용히 잠식된다.
		// 부착만큼 해제도 반드시 검증해야 하는 이유다.
		It("removes the taint and clears the fault signal when the node recovers", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			// 노드를 Ready로 되돌려 복구를 흉내낸다.
			recovered := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, recovered)).To(Succeed())
			recovered.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, recovered)).To(Succeed())
			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phaseReady))
			// FaultSignal이 nil로 지워져야 한다.
			// 남아 있으면 이미 복구된 노드에 대해 거짓 알람이 계속 뜬다.
			Expect(got.Status.FaultSignal).To(BeNil())

			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(0)) // 복구 후 taint 제거 확인
		})

		// 이 테스트가 막는 회귀: 이미 격리된 노드를 재조정할 때마다 taint를 또 붙이거나 오브젝트를 되쓰는 경우다.
		// taint가 중복되면 노드 spec이 계속 부풀고, 매번 되쓰면 Node와 NodeHealth 양쪽에서 hot loop가 생긴다.
		// 특히 Node는 kubelet이 공유하는 hot object라 불필요한 쓰기가 실제 충돌 비용으로 이어진다.
		// 그래서 NodeHealth뿐 아니라 Node의 ResourceVersion까지 함께 검사한다.
		It("does not duplicate the taint or rewrite when already quarantined", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			nhBefore := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, nhBefore)).To(Succeed())
			nodeBefore := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, nodeBefore)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			nhAfter := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, nhAfter)).To(Succeed())
			nodeAfter := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, nodeAfter)).To(Succeed())

			Expect(unhealthyTaintCount(nodeAfter)).To(Equal(1))
			// 이미 격리 상태면 재작성 없이 ResourceVersion 그대로여야 멱등
			Expect(nhAfter.ResourceVersion).To(Equal(nhBefore.ResourceVersion))
			Expect(nodeAfter.ResourceVersion).To(Equal(nodeBefore.ResourceVersion))
		})

		// 이 테스트가 막는 회귀 — 이 파일에서 가장 중요한 안전성 검증 중 하나다.
		// 컨트롤러가 taint를 지울 때 "우리 것"만 정확히 골라내지 못하고 노드의 taint 목록을 통째로 비워 버리는 경우를 막는다.
		// 실제 클러스터의 노드에는 다른 주체가 건 taint가 얼마든지 있다 — 전용 노드 풀 표시, 클러스터 오토스케일러, 사람이 건 cordon 등이다.
		// 그걸 우리가 날려 버리면 격리와 무관한 워크로드가 엉뚱한 노드로 쏟아지는 광범위한 사고가 된다.
		// removeUnhealthyTaint가 key와 effect로 소유권을 정확히 매칭해야 하는 이유가 바로 이것이며,
		// 이 스펙은 격리 시점과 복구 시점 양쪽 모두에서 무관한 taint가 살아남는지 확인한다.
		It("preserves unrelated taints through quarantine and recovery", func() {
			node := makeNode(corev1.ConditionFalse)
			// 우리 소유가 아닌 taint를 미리 심어 둔다.
			// 이것이 이 테스트의 핵심 픽스처이며, 끝까지 살아남아야 한다.
			node.Spec.Taints = []corev1.Taint{{Key: "example.com/other", Value: "x", Effect: corev1.TaintEffectNoSchedule}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			// hasOther: 무관한 taint가 아직 노드에 남아 있는지 매번 새로 읽어 확인하는 헬퍼다.
			// 여러 시점에서 같은 검사를 반복해야 하므로 클로저로 묶었다.
			hasOther := func() bool {
				n := &corev1.Node{}
				Expect(k8sClient.Get(ctx, nodeKey, n)).To(Succeed())
				// for _, t := range ... 는 인덱스를 버리고 값만 받는 반복문이다.
				for _, t := range n.Spec.Taints {
					if t.Key == "example.com/other" {
						return true
					}
				}
				return false
			}
			// 격리 시점 확인: 우리 taint를 붙이면서 남의 taint를 밀어내지 않았는지 본다.
			Expect(hasOther()).To(BeTrue())

			recovered := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, recovered)).To(Succeed())
			recovered.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, recovered)).To(Succeed())
			reconcileUntilSteady()

			Expect(hasOther()).To(BeTrue()) // 무관한 taint는 격리, 복구 전 구간 내내 유지
			// 동시에 우리 taint는 제대로 지워졌는지도 확인한다.
			// 이 단언이 함께 있어야 "아무것도 안 지워서 통과"하는 가짜 성공을 배제할 수 있다.
			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(0))
		})

		// 이 테스트가 막는 회귀: finalizer 정리 경로가 taint를 남기는 경우다.
		// NodeHealth를 지웠는데 taint가 남으면 아무도 그 taint를 책임지지 않으며,
		// 이제 그것을 지워 줄 오브젝트조차 없으니 노드는 사람이 손으로 고치기 전까지 영구히 스케줄 불가로 남는다.
		// 이것이 애초에 finalizer를 쓰는 이유이며, 이 스펙은 그 약속이 실제로 지켜지는지 확인한다.
		It("removes the taint on deletion (finalizer cleanup)", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			// 먼저 taint가 실제로 걸린 상태를 확인해 둔다.
			// 이 사전 확인이 없으면 "애초에 taint가 없어서 통과"하는 무의미한 테스트가 된다.
			tainted := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, tainted)).To(Succeed())
			Expect(unhealthyTaintCount(tainted)).To(Equal(1))

			toDelete := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, toDelete)).To(Succeed())
			// finalizer가 있으므로 이 Delete는 오브젝트를 즉시 없애지 않고 DeletionTimestamp만 찍는다.
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			// 이제 한 번 재조정하면 컨트롤러가 삭제 경로를 타 taint를 걷고 finalizer를 뗀다.
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			// finalizer가 떨어졌으므로 API 서버가 오브젝트를 실제로 없앴어야 한다.
			Expect(errors.IsNotFound(k8sClient.Get(ctx, nhKey, &platformv1.NodeHealth{}))).To(BeTrue())
			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(0)) // finalizer 정리로 taint까지 제거 확인
		})

		// 이 테스트가 막는 회귀: 대상 노드가 없을 때 컨트롤러가 에러를 뿜거나 노드를 결함으로 오판하는 경우다.
		// 노드가 아직 조인하지 않은 것은 정상 상황이므로 Pending으로 조용히 기다려야 한다.
		// 이 스펙은 BeforeEach가 만든 NodeHealth만 있고 Node는 만들지 않은 상태로 돈다.
		It("reports Pending when the target node is absent", func() {
			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phasePending))
		})

		// 이 테스트가 막는 회귀: 노드가 없을 때 삭제 경로가 막히는 경우다.
		// 위의 taint 정리 스펙과 달리 여기엔 Node가 아예 없으며, 이때 컨트롤러가 NotFound를 에러로 취급하면
		// finalizer를 영영 떼지 못해 NodeHealth가 삭제 대기 상태로 갇힌다.
		// 즉 nodehealth_controller.go의 삭제 경로에 있는 IsNotFound 케이스를 직접 겨냥한 테스트다.
		It("removes the finalizer on deletion", func() {
			reconcileUntilSteady()

			toDelete := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, nhKey, &platformv1.NodeHealth{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		// 이 테스트가 막는 회귀: nodeName 불변 규칙이 CRD에서 빠지는 경우다.
		// nodeName이 바뀔 수 있다면 NodeHealth가 A 노드에 taint를 건 뒤 B 노드를 가리키게 되어,
		// A의 taint는 소유자를 잃고 영원히 남는다.
		// 불변으로 막는 편이 컨트롤러가 이전 노드를 추적해 정리하는 것보다 훨씬 단순하고 안전하다.
		// 이 규칙은 컨트롤러가 아니라 API 서버가 강제하므로, 여기서는 Update가 거부되는지를 확인한다.
		It("rejects a change to the immutable nodeName", func() {
			nh := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, nh)).To(Succeed())
			nh.Spec.NodeName = "some-other-node" // 불변 필드 변경 시도
			err := k8sClient.Update(ctx, nh)
			Expect(err).To(HaveOccurred())
			// 메시지 내용까지 확인해 "어쩌다 다른 이유로 실패한 것"이 아니라 실제 불변 규칙에 걸렸음을 확정한다.
			Expect(err.Error()).To(ContainSubstring("nodeName is immutable"))
		})
	})

	// 이 Context는 Node 이벤트를 재조정 요청으로 바꾸는 매핑 함수만 따로 검증한다.
	// 컨트롤러 전체를 돌리지 않고 함수를 직접 부르므로 매핑 규칙만 좁게 확인할 수 있다.
	Context("mapNodeToNodeHealth", func() {
		ctx := context.Background()

		// 이 테스트가 막는 회귀: 매핑이 nodeName을 무시하고 모든 NodeHealth를 깨우는 경우다.
		// 그렇게 되면 노드 하나가 바뀔 때마다 클러스터의 모든 NodeHealth가 재조정 큐에 들어가,
		// 노드 수와 NodeHealth 수의 곱만큼 부하가 폭발한다.
		// 반대로 아무것도 매핑하지 않으면 노드 변화가 격리로 이어지지 않는다.
		// 그래서 "정확히 일치하는 하나만"을 검증한다.
		It("returns requests only for NodeHealths matching the node name", func() {
			r := &NodeHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// 일치하는 것과 일치하지 않는 것을 둘 다 만든다.
			// 둘 다 있어야 "골라내는지"를 검증할 수 있으며, 하나만 있으면 무조건 통과하는 테스트가 된다.
			match := &platformv1.NodeHealth{
				ObjectMeta: metav1.ObjectMeta{Name: "map-match"},
				Spec:       platformv1.NodeHealthSpec{NodeName: "map-node"},
			}
			other := &platformv1.NodeHealth{
				ObjectMeta: metav1.ObjectMeta{Name: "map-other"},
				Spec:       platformv1.NodeHealthSpec{NodeName: "different-node"},
			}
			Expect(k8sClient.Create(ctx, match)).To(Succeed())
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			// defer는 이 함수가 끝날 때(성공이든 실패든) 실행을 미뤄 두는 Go 문법이다.
			// 단언이 중간에 실패해도 정리가 반드시 돌아 다른 스펙에 픽스처가 새지 않게 한다.
			defer func() {
				Expect(k8sClient.Delete(ctx, match)).To(Succeed())
				Expect(k8sClient.Delete(ctx, other)).To(Succeed())
			}()

			// 매핑 함수는 이름만 쓰므로 노드를 실제로 만들 필요 없이 메모리상의 값이면 충분하다.
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "map-node"}}
			reqs := r.mapNodeToNodeHealth(ctx, node)

			Expect(reqs).To(HaveLen(1)) // 이름 일치하는 하나만 매핑
			Expect(reqs[0].Name).To(Equal("map-match"))
		})
	})
})

// unhealthyTaintCount: platform unhealthy key를 가진 taint 개수 count
//
// 개수를 세는 이유는 단순히 있는지 없는지만 보면 중복 부착을 놓치기 때문이다.
// 컨트롤러가 재조정마다 taint를 또 붙이는 버그가 있으면 존재 여부 검사는 통과하지만 개수 검사는 실패한다.
// 여기서는 effect를 보지 않고 key만 세는데, 이는 컨트롤러가 소유권을 잘못 판단해 effect가 다른 중복 taint를 만들어도 잡아내기 위해서다.
//
// Go 문법 설명: 리시버가 없는 일반 함수이며, 테스트 파일에 있으므로 테스트 빌드에만 포함된다.
func unhealthyTaintCount(node *corev1.Node) int {
	n := 0
	for _, t := range node.Spec.Taints {
		if t.Key == unhealthyTaintKey {
			n++
		}
	}
	return n
}

// findCondition: 주어진 type의 condition pointer 반환, 없으면 nil
//
// Go 문법 설명:
//   - for i := range conds 로 인덱스를 도는 이유는 &conds[i]에서 "원본 요소의 주소"가 필요하기 때문이다.
//     for _, c := range 처럼 값으로 받으면 복사본이 만들어져 그 주소를 돌려줘도 원본과 무관한 값이 된다.
//   - 포인터를 돌려주므로 "없음"을 nil로 표현할 수 있고, 호출한 쪽은 NotTo(BeNil())로 존재 여부를 먼저 단언할 수 있다.
func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
