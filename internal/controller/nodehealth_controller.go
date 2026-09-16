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

// NodeHealthReconciler reconciles a NodeHealth object
type NodeHealthReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile observes the target Node, reflects its readiness into NodeHealth status, and enforces quarantine:
// a not-ready node is tainted so the scheduler stops placing GPU workloads on it,
// and the taint is removed when the node recovers or the NodeHealth is deleted.
func (r *NodeHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var nh platformv1.NodeHealth
	if err := r.Get(ctx, req.NamespacedName, &nh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: remove our taint from the node, then drop the finalizer.
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
				// Node already gone; nothing to clean up.
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

	// Ensure the finalizer is present before doing work.
	if !controllerutil.ContainsFinalizer(&nh, nodeHealthFinalizer) {
		controllerutil.AddFinalizer(&nh, nodeHealthFinalizer)
		if err := r.Update(ctx, &nh); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to nodehealth %s: %w", nh.Name, err)
		}
		return ctrl.Result{}, nil
	}

	// Observe the target Node and compute the desired status + taint enforcement.
	desired := nh.Status.DeepCopy()
	desired.ObservedGeneration = nh.Generation

	var node corev1.Node
	var nodeBase *corev1.Node
	nodeChanged := false
	err := r.Get(ctx, types.NamespacedName{Name: nh.Spec.NodeName}, &node)
	switch {
	case apierrors.IsNotFound(err):
		// No node to manage: report Pending and clear any fault signal.
		setPhase(desired, phasePending)
		desired.FaultSignal = nil
		setReadyCondition(desired, false, reasonNodeNotFound, "Target node not found", nh.Generation)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get node %s: %w", nh.Spec.NodeName, err)
	case isNodeReady(&node):
		nodeBase = node.DeepCopy()
		setPhase(desired, phaseReady)
		desired.FaultSignal = nil
		nodeChanged = removeUnhealthyTaint(&node)
		setReadyCondition(desired, true, reasonNodeReady, "Target node is Ready", nh.Generation)
	default:
		nodeBase = node.DeepCopy()
		setPhase(desired, phaseQuarantine)
		desired.FaultSignal = &platformv1.FaultSignal{Source: faultSourceNodeNotReady}
		nodeChanged = ensureUnhealthyTaint(&node)
		setReadyCondition(desired, false, reasonNodeNotReady, "Target node is not Ready", nh.Generation)
	}

	// Enforce taint changes on the node first, so status never claims a quarantine we failed to apply.
	// Patch only the taint delta from the pre-mutation base, so concurrent kubelet updates to the hot Node object are not clobbered.
	if nodeChanged {
		if err := r.Patch(ctx, &node, client.MergeFrom(nodeBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update node %s taints: %w", node.Name, err)
		}
		log.Info("Updated node taints", "node", node.Name, "phase", desired.Phase)

		// Count the taint transition only after the patch succeeds, so the metric never claims an enforcement that did not happen.
		//
		// Pending has no taint change, so only these two phases increment the counter.
		switch desired.Phase {
		case phaseQuarantine:
			nodeHealthTaintTotal.WithLabelValues("applied").Inc()
		case phaseReady:
			nodeHealthTaintTotal.WithLabelValues("removed").Inc()
		}
	}

	// Idempotent: write status only when it actually changed.
	if !equality.Semantic.DeepEqual(nh.Status, *desired) {
		nh.Status = *desired
		if err := r.Status().Update(ctx, &nh); err != nil {
			return ctrl.Result{}, fmt.Errorf("update nodehealth status %s: %w", nh.Name, err)
		}
		log.Info("Updated NodeHealth status", "name", nh.Name, "phase", desired.Phase)
	}

	return ctrl.Result{}, nil
}

// NodeNameIndex is the cache field-index key over NodeHealth.spec.nodeName.
//
// It mirrors the gateway's ModelNameIndex: the lookup that needs it runs on the hot path, so it resolves against the informer cache with no field selector and no apiserver call.
const NodeNameIndex = ".spec.nodeName"

// indexNodeHealthByNodeName is the extractor behind NodeNameIndex.
//
// It is a named function rather than a literal because the tests must register the identical extractor to make MatchingFields behave as it does against the manager's cache.
func indexNodeHealthByNodeName(o client.Object) []string {
	return []string{o.(*platformv1.NodeHealth).Spec.NodeName}
}

// mapNodeToNodeHealth maps a Node event to reconcile requests for every NodeHealth whose spec.nodeName matches the node.
// This propagates node-side drift back into status.
//
// The lookup goes through NodeNameIndex rather than listing and filtering in Go, because kubelet posts a status heartbeat for every node on a fixed cadence: an unindexed List would walk every NodeHealth in the cluster once per node per heartbeat, so its cost grows with the product of the two rather than with the handful of objects that actually match.
func (r *NodeHealthReconciler) mapNodeToNodeHealth(ctx context.Context, obj client.Object) []reconcile.Request {
	var list platformv1.NodeHealthList
	if err := r.List(ctx, &list, client.MatchingFields{NodeNameIndex: obj.GetName()}); err != nil {
		// A map function has no error return, so the only alternative to logging is dropping the failure on the floor.
		//
		// That is the worst outcome available here: drift stops reaching status and the resource simply stops updating, with nothing anywhere saying which node stopped being watched or why.
		logf.FromContext(ctx).Error(err, "Could not list NodeHealth resources for Node event", "node", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
		})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// The index must exist before the Node watch can fire, so it is registered here rather than lazily.
	//
	// context.Background() rather than a scoped context because IndexField only installs the extractor on the cache; it starts nothing that would need cancelling.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &platformv1.NodeHealth{}, NodeNameIndex, indexNodeHealthByNodeName); err != nil {
		return fmt.Errorf("index nodehealth by %s: %w", NodeNameIndex, err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.NodeHealth{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToNodeHealth)).
		Named("nodehealth").
		Complete(r)
}
