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
// 같은 디렉터리(internal/controller)의 모든 .go 파일은 반드시 같은 package 이름(controller)을 가져야 한다.
// 그래서 reconcile_helpers.go에 있는 nodeHealthFinalizer, ensureUnhealthyTaint, setPhase 등을 import 없이 바로 쓸 수 있다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	"context"
	// fmt: 문자열 포매팅 표준 패키지이며, 여기서는 fmt.Errorf로 에러에 문맥을 덧붙인다.
	"fmt"

	// corev1: 쿠버네티스 코어 API 타입(Node, Taint, NodeCondition 등)이다.
	// NodeHealth는 우리 CRD지만 실제 격리는 코어 Node 오브젝트에 가하므로 이 패키지가 필요하다.
	corev1 "k8s.io/api/core/v1"
	// equality: 쿠버네티스 오브젝트를 의미론적으로 비교하는 도우미다.
	// 아래 멱등성 검사에서 equality.Semantic.DeepEqual로 쓴다.
	"k8s.io/apimachinery/pkg/api/equality"
	// apierrors: 쿠버네티스 API 에러를 종류별로 판별하는 도우미이며, apierrors.IsNotFound를 쓴다.
	// 별칭을 붙인 이유는 표준 라이브러리 errors와 이름이 겹치지 않게 하기 위해서다.
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	// runtime: Scheme 타입(Go 타입 ↔ GroupVersionKind 대응표)이 들어 있다.
	"k8s.io/apimachinery/pkg/runtime"
	// types: 오브젝트를 지목하는 NamespacedName 타입이 들어 있다.
	"k8s.io/apimachinery/pkg/types"
	// ctrl: sigs.k8s.io/controller-runtime의 별칭이며, ctrl.Request/ctrl.Result/ctrl.Manager가 여기 있다.
	ctrl "sigs.k8s.io/controller-runtime"
	// client: 쿠버네티스 API를 읽고 쓰는 클라이언트와 patch 헬퍼(client.MergeFrom 등)를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// controllerutil: finalizer를 붙이고 떼는 표준 도우미(AddFinalizer 등)를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	// handler: 감시 대상 이벤트를 재조정 요청으로 바꾸는 규칙(EnqueueRequestsFromMapFunc 등)이 들어 있다.
	"sigs.k8s.io/controller-runtime/pkg/handler"
	// logf: controller-runtime의 구조화 로깅 도우미이며, 표준 log와 겹치지 않도록 별칭을 쓴다.
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	// reconcile: mapNodeToNodeHealth가 돌려줄 reconcile.Request 타입이 들어 있다.
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(NodeHealth, FaultSignal 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// NodeHealthReconciler: NodeHealth object를 재조정하는 reconciler다.
//
// 이 컨트롤러는 앞의 MLTrainingJob과 성격이 다르다.
// 관측만 하는 것이 아니라 실제 Node 오브젝트에 taint를 밀어넣는 enforcement 컨트롤러이며, 클러스터 스케줄링에 직접 영향을 준다.
// 그래서 "무엇을 소유하는지"와 "어떻게 되돌리는지"를 코드가 엄격히 지켜야 한다.
//
// Go 문법 설명:
//   - client.Client 처럼 필드 이름 없이 타입만 적은 것을 "임베딩(embedding)"이라고 한다.
//     임베딩 덕분에 아래에서 r.Get, r.Patch, r.Status()를 r.Client를 거치지 않고 바로 부를 수 있다.
//   - Scheme *runtime.Scheme 의 별표(*)는 포인터를 뜻하며, 매니저가 만든 Scheme 하나를 공유하려고 포인터로 들고 있는다.
type NodeHealthReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// 아래 네 줄은 주석처럼 보이지만 실제로는 코드 생성 지시자(kubebuilder 마커)다.
// make manifests가 이 마커를 읽어 ClusterRole YAML을 만들므로 번역하거나 수정하면 컨트롤러가 권한을 잃는다.
// 앞의 세 줄은 NodeHealth 본체/status/finalizers에 대한 권한이다.
// 마지막 줄의 groups=""는 코어 API 그룹을 뜻하며, Node에 update/patch 권한을 요구한다.
// taint를 붙이고 떼려면 반드시 필요한 권한이지만, 동시에 이 컨트롤러가 클러스터 스케줄링에 개입할 수 있다는 뜻이기도 하다.
// 그래서 delete는 요청하지 않는다.
// 이 컨트롤러는 Node를 지울 이유가 전혀 없으므로 최소 권한 원칙에 따라 뺀다.
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile: 대상 Node를 관찰해 readiness를 NodeHealth 상태에 반영하고 격리를 적용하는데,
// not-ready node에는 taint를 걸어 scheduler가 GPU workload를 배치하지 못하게 하고,
// node가 복구되거나 NodeHealth가 삭제되면 taint를 제거한다.
//
// 재조정 흐름:
//  1. 삭제 중이면 부여한 unhealthy taint를 걷어낸 뒤 finalizer를 떼어 실제 삭제가 진행되게 한다,
//  2. finalizer가 없으면 붙이고 이번 pass를 끝낸다 (소유 taint를 만들기 전에 정리 약속을 먼저 건다),
//  3. 대상 Node를 읽어 세 상태 중 하나로 판정한다: node 없음→Pending, Ready→Ready(격리 해제), not-ready→Quarantine(격리 taint 부여),
//  4. taint 변경을 Node에 먼저 patch해 반영하지 못한 격리를 status가 주장하지 않게 한다,
//  5. status가 실제로 바뀐 경우에만 멱등하게 기록한다.
//
// Go 문법 설명:
//   - ctrl.Request에는 오브젝트가 아니라 이름만 들어 있으므로, 항상 이름으로 최신 상태를 다시 읽어야 한다.
//     요청이 큐에 머무는 동안 오브젝트가 이미 바뀌었을 수 있기 때문이다.
//   - 반환하는 ctrl.Result가 비어 있으면 "재큐 없음"이고, error가 nil이 아니면 컨트롤러 런타임이 지수 백오프로 자동 재시도한다.
//     이 컨트롤러가 직접 재시도 루프를 돌리지 않고 에러를 그대로 올리는 이유가 이것이다.
func (r *NodeHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// ctx에 실려 온 로거를 꺼낸다.
	// 재조정 대상 이름 같은 문맥이 이미 붙어 있어 로그를 요청 단위로 추적하기 좋다.
	log := logf.FromContext(ctx)

	// 재조정 대상 NodeHealth를 이름으로 읽어 온다.
	// var nh ... 로 빈 값을 만들고 &nh로 주소를 넘겨 Get이 원본을 채우게 한다.
	var nh platformv1.NodeHealth
	if err := r.Get(ctx, req.NamespacedName, &nh); err != nil {
		// client.IgnoreNotFound(err)는 NotFound면 nil을, 그 외 에러면 원래 에러를 돌려주는 도우미다.
		// 오브젝트가 이미 삭제된 뒤 큐에 남아 있던 요청이 도착하는 것은 정상 상황이므로 에러로 취급해 재시도하면 안 된다.
		// 반면 진짜 API 오류는 그대로 올려 재시도되게 해야 한다.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 삭제 처리: node에서 부여한 taint를 지운 뒤 finalizer 제거
	//
	// DeletionTimestamp는 오브젝트에 finalizer가 남아 있는 동안 API 서버가 찍어 두는 삭제 예약 시각이다.
	// finalizer가 있으면 kubectl delete를 해도 오브젝트는 즉시 사라지지 않고 이 타임스탬프만 채워진 채 남는다.
	// IsZero()가 false라는 것은 "삭제가 요청되었고 우리가 정리할 차례"라는 뜻이다.
	//
	// 설계 근거: 이 정리 경로가 없으면 NodeHealth를 지운 뒤에도 Node에 taint가 영원히 남아 노드가 스케줄 불가 상태로 방치된다.
	// finalizer는 바로 그 유출을 막기 위한 장치다.
	if !nh.DeletionTimestamp.IsZero() {
		// finalizer가 이미 제거된 상태로 다시 불릴 수도 있으므로, 있을 때만 정리한다.
		if controllerutil.ContainsFinalizer(&nh, nodeHealthFinalizer) {
			var node corev1.Node
			// switch err := ...; { } 는 초기화문이 붙은 switch다.
			// 케이스에 조건식을 직접 적는 형태이며, err를 한 번만 만들어 여러 갈래로 분기할 때 쓰는 Go 관용구다.
			switch err := r.Get(ctx, types.NamespacedName{Name: nh.Spec.NodeName}, &node); {
			case err == nil:
				// node.DeepCopy()로 변경 전 스냅샷을 떠 둔다.
				// 아래 MergeFrom(base)가 "이 스냅샷 대비 무엇이 바뀌었는지"만 계산해 patch를 만들기 때문에 반드시 수정 전에 떠야 한다.
				base := node.DeepCopy()
				// removeUnhealthyTaint는 우리 소유 taint만 골라 지우고, 실제로 바뀌었으면 true를 준다.
				// 바뀐 게 없으면 patch를 아예 보내지 않아 불필요한 쓰기를 피한다.
				if removeUnhealthyTaint(&node) {
					// client.MergeFrom(base)는 base와 현재 node의 차이만 담은 merge patch를 만든다.
					// 노드 전체를 Update로 덮어쓰지 않는 이유가 중요하다.
					// Node는 kubelet이 계속 갱신하는 hot object라, 우리가 읽은 시점의 전체 오브젝트를 그대로 되쓰면 그 사이 kubelet이 쓴 status나 다른 컨트롤러의 변경을 되돌려 버린다.
					// taint delta만 보내면 우리가 실제로 의도한 필드만 건드리게 된다.
					if err := r.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
						// %w 동사는 원래 에러를 감싸(wrap) 보존하므로, 상위에서 errors.Is로 원인을 판별할 수 있다.
						// 여기서 에러를 올리면 finalizer가 남아 있는 채로 재시도되므로, 정리가 끝나기 전에 오브젝트가 사라질 일은 없다.
						return ctrl.Result{}, fmt.Errorf("remove unhealthy taint from node %s on deletion: %w", node.Name, err)
					}
					log.Info("Removed unhealthy taint on deletion", "node", node.Name)
				}
			case apierrors.IsNotFound(err):
				// node가 이미 사라짐, 정리할 것 없음
				//
				// 노드 자체가 없어졌으면 지울 taint도 없으므로 이건 성공 경로다.
				// 여기서 에러를 내면 노드가 제거된 클러스터에서 NodeHealth가 영영 삭제되지 못하고 멈춰 버린다.
			default:
				// NotFound가 아닌 진짜 읽기 실패다.
				// 노드 상태를 모르는 채로 finalizer를 떼면 taint가 남을 수 있으므로, 정리를 포기하지 않고 에러를 올려 재시도한다.
				return ctrl.Result{}, fmt.Errorf("get node %s on deletion: %w", nh.Spec.NodeName, err)
			}
			// taint 정리가 끝났으니 이제 finalizer를 떼어 API 서버가 실제 삭제를 진행하게 한다.
			// 순서가 핵심이다.
			// 반드시 정리를 먼저 하고 finalizer를 나중에 떼야 한다.
			// 반대로 하면 오브젝트가 즉시 사라져 taint를 지울 기회를 영원히 잃는다.
			controllerutil.RemoveFinalizer(&nh, nodeHealthFinalizer)
			// finalizer는 status가 아니라 metadata에 있으므로 Status().Update가 아니라 일반 Update로 쓴다.
			if err := r.Update(ctx, &nh); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer from nodehealth %s: %w", nh.Name, err)
			}
		}
		// 삭제 중인 오브젝트에는 더 이상 격리 로직을 돌리지 않고 여기서 끝낸다.
		return ctrl.Result{}, nil
	}

	// 작업 시작 전 finalizer 존재 보장
	//
	// 설계 근거: taint를 붙이기 "전에" 정리 약속(finalizer)을 먼저 걸어야 한다.
	// 만약 taint를 먼저 붙이고 finalizer를 나중에 붙이는 사이에 프로세스가 죽고 그 틈에 NodeHealth가 삭제되면,
	// 아무도 책임지지 않는 taint가 노드에 남아 노드를 영구히 스케줄 불가로 만든다.
	// 순서를 뒤집어 두면 최악의 경우가 "쓸모없는 finalizer가 잠깐 남는 것"이라 훨씬 안전하다.
	if !controllerutil.ContainsFinalizer(&nh, nodeHealthFinalizer) {
		controllerutil.AddFinalizer(&nh, nodeHealthFinalizer)
		if err := r.Update(ctx, &nh); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to nodehealth %s: %w", nh.Name, err)
		}
		// finalizer를 쓰면 오브젝트의 resourceVersion이 올라가 손에 든 nh가 낡은 값이 된다.
		// 그래서 이번 pass를 여기서 끝낸다.
		// Update가 이미 새 이벤트를 만들어 곧 다시 재조정되므로 일을 잃지는 않는다.
		return ctrl.Result{}, nil
	}

	// 대상 Node를 관찰해 원하는 상태와 taint 적용 여부 계산
	//
	// nh.Status.DeepCopy()로 현재 status의 사본을 떠서 그 위에 원하는 값을 그려 나간다.
	// 원본을 직접 고치지 않는 이유는 아래에서 "원본 vs 원하는 값"을 비교해 실제 변경이 있을 때만 쓰기 위해서다.
	desired := nh.Status.DeepCopy()
	// ObservedGeneration은 "우리가 어느 세대의 spec을 보고 이 status를 냈는지"를 기록한다.
	// Generation은 spec이 바뀔 때만 올라가므로, 이 둘을 비교하면 status가 최신 spec을 반영했는지 알 수 있다.
	desired.ObservedGeneration = nh.Generation

	var node corev1.Node
	// nodeBase: 변경 전 노드 스냅샷을 담을 포인터이며, 노드를 못 읽은 경우엔 nil로 남는다.
	var nodeBase *corev1.Node
	// nodeChanged: taint를 실제로 바꿨는지 표시하는 플래그이며, 바뀐 경우에만 patch를 보낸다.
	nodeChanged := false
	err := r.Get(ctx, types.NamespacedName{Name: nh.Spec.NodeName}, &node)
	// switch { } 는 조건식 없는 switch이며, 각 case에 불리언 식을 적어 if/else if 사슬처럼 쓴다.
	// 케이스는 위에서부터 순서대로 평가되고 처음 참인 하나만 실행되며, Go는 자동으로 fallthrough하지 않는다.
	switch {
	case apierrors.IsNotFound(err):
		// 관리할 node 없음, Pending 보고 후 결함 신호 clear
		//
		// 노드가 아직 클러스터에 조인하지 않았거나 이미 빠진 경우다.
		// 이건 결함이 아니라 "아직 관측할 대상이 없음"이므로 Pending으로 두고 FaultSignal을 지운다.
		// 없는 노드에 대해 결함을 계속 주장하면 운영자에게 거짓 알람이 된다.
		setPhase(desired, phasePending)
		desired.FaultSignal = nil
		setReadyCondition(desired, false, reasonNodeNotFound, "Target node not found", nh.Generation)
	case err != nil:
		// NotFound가 아닌 진짜 읽기 실패다.
		// 노드 상태를 모르면서 격리를 걸거나 풀면 위험하므로, 아무것도 하지 않고 에러를 올려 재시도한다.
		return ctrl.Result{}, fmt.Errorf("get node %s: %w", nh.Spec.NodeName, err)
	case isNodeReady(&node):
		// 노드가 Ready로 복귀한 경로다.
		// 수정 전에 스냅샷을 떠 둬야 아래 MergeFrom이 올바른 delta를 계산한다.
		nodeBase = node.DeepCopy()
		setPhase(desired, phaseReady)
		desired.FaultSignal = nil
		nodeChanged = removeUnhealthyTaint(&node) // Ready 복귀 시 격리 taint 해제
		setReadyCondition(desired, true, reasonNodeReady, "Target node is Ready", nh.Generation)
	default:
		// 노드가 존재하지만 Ready가 아닌 경로이며, 여기서 실제 격리가 일어난다.
		nodeBase = node.DeepCopy()
		setPhase(desired, phaseQuarantine)
		// FaultSignal에 출처를 남겨 "왜 격리했는지"를 운영자와 후속 단계가 알 수 있게 한다.
		// &platformv1.FaultSignal{...}로 포인터를 넣는 이유는 이 필드가 optional이라 "없음"을 nil로 표현하기 때문이다.
		desired.FaultSignal = &platformv1.FaultSignal{Source: faultSourceNodeNotReady}
		nodeChanged = ensureUnhealthyTaint(&node) // not-ready node에 격리 taint 부여
		setReadyCondition(desired, false, reasonNodeNotReady, "Target node is not Ready", nh.Generation)
	}

	// taint 변경을 node에 먼저 반영해 적용하지 못한 격리를 상태가 주장하지 않도록 하고,
	// 변경 전 base 대비 taint delta만 patch하여 hot한 Node object에 대한 kubelet 동시 갱신을 덮어쓰지 않게 한다.
	//
	// 설계 근거: 쓰기 순서가 안전성의 핵심이다.
	// status를 먼저 쓰고 노드 patch가 실패하면, NodeHealth는 "Quarantine"이라고 주장하는데 실제 노드엔 taint가 없는 거짓 상태가 된다.
	// 운영자는 격리됐다고 믿지만 스케줄러는 계속 GPU 워크로드를 그 노드에 얹는, 가장 위험한 조합이다.
	// 반대로 노드를 먼저 patch하면 최악의 경우가 "taint는 걸렸는데 status가 아직 안 따라옴"이고, 이는 다음 재조정에서 저절로 수렴한다.
	// 즉 실패 모드를 안전한 쪽(과잉 격리)으로 기울인다.
	if nodeChanged {
		if err := r.Patch(ctx, &node, client.MergeFrom(nodeBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update node %s taints: %w", node.Name, err)
		}
		log.Info("Updated node taints", "node", node.Name, "phase", desired.Phase)
	}

	// 멱등: 실제로 바뀐 경우에만 상태 기록
	//
	// equality.Semantic.DeepEqual은 쿠버네티스가 정의한 의미론적 비교다.
	// 예를 들어 nil 슬라이스와 빈 슬라이스를 같은 것으로 취급해, 표현상의 차이를 실제 변경으로 오인하지 않는다.
	// *desired 의 별표는 포인터가 가리키는 실제 값을 꺼내는 역참조 연산자다.
	//
	// 이 가드가 없으면 재조정마다 status를 되써서 resourceVersion이 계속 올라가고,
	// 그 쓰기가 다시 watch 이벤트를 만들어 스스로를 깨우는 무한 재조정 루프(hot loop)에 빠진다.
	if !equality.Semantic.DeepEqual(nh.Status, *desired) {
		nh.Status = *desired
		// status 서브리소스에만 쓴다.
		// 일반 Update를 쓰면 사용자가 그사이 편집한 spec을 우리가 읽은 낡은 값으로 덮어쓸 수 있다.
		if err := r.Status().Update(ctx, &nh); err != nil {
			return ctrl.Result{}, fmt.Errorf("update nodehealth status %s: %w", nh.Name, err)
		}
		log.Info("Updated NodeHealth status", "name", nh.Name, "phase", desired.Phase)
	}

	// 여기까지 왔으면 이번 pass는 성공이고 재큐도 필요 없다.
	// 노드에 변화가 생기면 아래 Watches가 다시 깨워 주므로 주기적 폴링(RequeueAfter)이 필요 없다.
	return ctrl.Result{}, nil
}

// mapNodeToNodeHealth: Node event를 spec.nodeName이 일치하는 모든 NodeHealth의 재조정 요청으로 변환하고,
// node 측 drift를 상태로 다시 전파하는 역할을 한다.
//
// 설계 근거: 이 매핑이 없으면 컨트롤러는 NodeHealth가 바뀔 때만 깨어난다.
// 그러면 노드가 not-ready로 떨어져도 아무도 재조정을 촉발하지 않아 격리가 늦어지고,
// 누군가 우리 taint를 손으로 지워도 알아채지 못한다.
// 노드를 감시해야 "노드의 실제 변화"가 곧바로 재조정으로 이어진다.
//
// Go 문법 설명:
//   - client.Object는 모든 쿠버네티스 오브젝트가 만족하는 인터페이스이며, GetName() 같은 공통 메서드를 제공한다.
//     구체 타입(*corev1.Node)이 아니라 인터페이스로 받는 이유는 handler가 이 시그니처를 요구하기 때문이다.
//   - 반환 타입 []reconcile.Request는 슬라이스(가변 길이 배열)이며, 하나의 노드 이벤트가 여러 NodeHealth를 깨울 수 있음을 뜻한다.
func (r *NodeHealthReconciler) mapNodeToNodeHealth(ctx context.Context, obj client.Object) []reconcile.Request {
	var list platformv1.NodeHealthList
	if err := r.List(ctx, &list); err != nil {
		// 매핑 함수는 error를 돌려줄 수 없는 시그니처이므로 nil(요청 없음)을 준다.
		// 이번 이벤트는 놓치지만, 캐시가 회복되면 이후 이벤트가 다시 들어와 결국 수렴한다.
		return nil
	}
	// reqs: 결과를 모을 슬라이스이며, 선언만 하면 nil 슬라이스다.
	// Go에서는 nil 슬라이스에 append해도 안전하며 자동으로 새 배열이 잡힌다.
	var reqs []reconcile.Request
	// for i := range list.Items 로 인덱스를 돌린다.
	// 값 복사 대신 인덱스로 접근하면 큰 구조체를 매 반복마다 복사하지 않아도 된다.
	for i := range list.Items {
		// 이 NodeHealth가 방금 이벤트가 난 그 노드를 가리키는지 확인한다.
		// 이름이 다르면 무관한 NodeHealth이므로 깨우지 않는다.
		if list.Items[i].Spec.NodeName == obj.GetName() {
			// NodeHealth는 클러스터 스코프라 NamespacedName에 Name만 채우고 Namespace는 비운다.
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
			})
		}
	}
	return reqs
}

// SetupWithManager: Manager에 controller 등록
// cmd/main.go가 프로세스 시작 시 한 번 호출하며, 이 호출이 있어야 Reconcile이 이벤트를 받기 시작한다.
//
// Go 문법 설명:
//   - 아래는 "빌더 체인(builder chain)"이며, 각 메서드가 빌더 자신을 돌려주므로 점(.)으로 계속 이어 붙일 수 있다.
//   - 줄 끝의 점은 "다음 줄에 이어진다"는 표시이며, Go의 자동 세미콜론 삽입을 피하려면 점을 줄 끝에 두어야 한다.
func (r *NodeHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// For(...): 이 컨트롤러의 주 대상(primary resource)이며, 재조정 요청의 이름은 이 타입의 오브젝트를 가리킨다.
		For(&platformv1.NodeHealth{}).
		// Watches(...): 주 대상이 아닌 부가 리소스(secondary resource)를 감시한다.
		// handler.EnqueueRequestsFromMapFunc(r.mapNodeToNodeHealth)는 "Node 이벤트가 오면 이 함수를 돌려 나온 요청들을 큐에 넣으라"는 뜻이다.
		// Node는 우리가 소유한 오브젝트가 아니므로 Owns()가 아니라 Watches()를 쓴다.
		// Owns()는 ownerReference를 따라가는데, 코어 Node에 우리 CRD를 소유자로 걸 수는 없기 때문이다.
		// r.mapNodeToNodeHealth처럼 메서드를 괄호 없이 넘기면 리시버 r이 묶인 함수 값(method value)이 전달된다.
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToNodeHealth)).
		// Named(...): 로그와 메트릭 레이블에 쓰이는 컨트롤러 이름이며, 매니저 안에서 유일해야 한다.
		Named("nodehealth").
		// Complete(r): 설정한 내용으로 컨트롤러를 만들고 리시버 r을 재조정기로 연결하며, error를 그대로 올려보낸다.
		Complete(r)
}
