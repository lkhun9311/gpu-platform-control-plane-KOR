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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// NodeHealth object를 재조정하는 reconciler
type NodeHealthReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// 대상 Node를 관찰해 readiness를 NodeHealth 상태에 반영하고 격리를 적용하는데,
// not-ready node에는 taint를 걸어 scheduler가 GPU workload를 배치하지 못하게 하고,
// node가 복구되거나 NodeHealth가 삭제되면 taint를 제거한다.
//
// 재조정 흐름:
//  1. 삭제 중이면 부여한 unhealthy taint를 걷어낸 뒤 finalizer를 떼어 실제 삭제가 진행되게 한다,
//  2. finalizer가 없으면 붙이고 이번 pass를 끝낸다 (소유 taint를 만들기 전에 정리 약속을 먼저 건다),
//  3. 대상 Node를 읽어 세 상태 중 하나로 판정한다 — node 없음→Pending, Ready→Ready(격리 해제), not-ready→Quarantine(격리 taint 부여),
//  4. taint 변경을 Node에 먼저 patch해 반영하지 못한 격리를 status가 주장하지 않게 한다,
//  5. status가 실제로 바뀐 경우에만 멱등하게 기록한다.
func (r *NodeHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var nh platformv1.NodeHealth
	if err := r.Get(ctx, req.NamespacedName, &nh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 삭제 처리: node에서 부여한 taint를 지운 뒤 finalizer 제거
	if !nh.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&nh, nodeHealthFinalizer) {
			var node corev1.Node
			switch err := r.Get(ctx, types.NamespacedName{Name: nh.Spec.NodeName}, &node); {
			case err == nil:
				base := node.DeepCopy()
				if removeUnhealthyTaint(&node) {
					if err := r.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
						return ctrl.Result{}, fmt.Errorf("remove unhealthy taint from node %s on deletion: %w", node.Name, err)
					}
					log.Info("Removed unhealthy taint on deletion", "node", node.Name)
				}
			case apierrors.IsNotFound(err):
				// node가 이미 사라짐, 정리할 것 없음
			default:
				return ctrl.Result{}, fmt.Errorf("get node %s on deletion: %w", nh.Spec.NodeName, err)
			}
			controllerutil.RemoveFinalizer(&nh, nodeHealthFinalizer)
			if err := r.Update(ctx, &nh); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer from nodehealth %s: %w", nh.Name, err)
			}
		}
		return ctrl.Result{}, nil
	}

	// 작업 시작 전 finalizer 존재 보장
	if !controllerutil.ContainsFinalizer(&nh, nodeHealthFinalizer) {
		controllerutil.AddFinalizer(&nh, nodeHealthFinalizer)
		if err := r.Update(ctx, &nh); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to nodehealth %s: %w", nh.Name, err)
		}
		return ctrl.Result{}, nil
	}

	// 대상 Node를 관찰해 원하는 상태와 taint 적용 여부 계산
	desired := nh.Status.DeepCopy()
	desired.ObservedGeneration = nh.Generation

	var node corev1.Node
	var nodeBase *corev1.Node
	nodeChanged := false
	err := r.Get(ctx, types.NamespacedName{Name: nh.Spec.NodeName}, &node)
	switch {
	case apierrors.IsNotFound(err):
		// 관리할 node 없음, Pending 보고 후 결함 신호 clear
		setPhase(desired, phasePending)
		desired.FaultSignal = nil
		setReadyCondition(desired, false, reasonNodeNotFound, "Target node not found", nh.Generation)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get node %s: %w", nh.Spec.NodeName, err)
	case isNodeReady(&node):
		nodeBase = node.DeepCopy()
		setPhase(desired, phaseReady)
		desired.FaultSignal = nil
		nodeChanged = removeUnhealthyTaint(&node) // Ready 복귀 시 격리 taint 해제
		setReadyCondition(desired, true, reasonNodeReady, "Target node is Ready", nh.Generation)
	default:
		nodeBase = node.DeepCopy()
		setPhase(desired, phaseQuarantine)
		desired.FaultSignal = &platformv1.FaultSignal{Source: faultSourceNodeNotReady}
		nodeChanged = ensureUnhealthyTaint(&node) // not-ready node에 격리 taint 부여
		setReadyCondition(desired, false, reasonNodeNotReady, "Target node is not Ready", nh.Generation)
	}

	// taint 변경을 node에 먼저 반영해 적용하지 못한 격리를 상태가 주장하지 않도록 하고,
	// 변경 전 base 대비 taint delta만 patch하여 hot한 Node object에 대한 kubelet 동시 갱신을 덮어쓰지 않게 한다.
	if nodeChanged {
		if err := r.Patch(ctx, &node, client.MergeFrom(nodeBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update node %s taints: %w", node.Name, err)
		}
		log.Info("Updated node taints", "node", node.Name, "phase", desired.Phase)
	}

	// 멱등: 실제로 바뀐 경우에만 상태 기록
	if !equality.Semantic.DeepEqual(nh.Status, *desired) {
		nh.Status = *desired
		if err := r.Status().Update(ctx, &nh); err != nil {
			return ctrl.Result{}, fmt.Errorf("update nodehealth status %s: %w", nh.Name, err)
		}
		log.Info("Updated NodeHealth status", "name", nh.Name, "phase", desired.Phase)
	}

	return ctrl.Result{}, nil
}

// Node event를 spec.nodeName이 일치하는 모든 NodeHealth의 재조정 요청으로 변환하고,
// node 측 drift를 상태로 다시 전파하는 역할을 한다.
func (r *NodeHealthReconciler) mapNodeToNodeHealth(ctx context.Context, obj client.Object) []reconcile.Request {
	var list platformv1.NodeHealthList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.NodeName == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
			})
		}
	}
	return reqs
}

// Manager에 controller 등록
func (r *NodeHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.NodeHealth{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToNodeHealth)).
		Named("nodehealth").
		Complete(r)
}
