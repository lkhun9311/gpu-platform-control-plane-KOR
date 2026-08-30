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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

const (
	// kueueCohort is the single cohort every tenant ClusterQueue joins.
	//
	// One shared cohort is what makes cross-tenant borrowing and reclaim coherent: an idle tenant's nominal quota can be lent within the cohort and reclaimed by preemption when its owner submits.
	kueueCohort = "gpu-platform"

	// kueueFlavor is the shared ResourceFlavor every tenant ClusterQueue references.
	//
	// The flavor is a static fixture shared across tenants, so this controller references it by name and never creates or owns it.
	kueueFlavor = "gpu"

	// reasonKueueConflict marks a deterministic failure to adopt a Kueue queue this policy does not own.
	reasonKueueConflict = "KueueQueueConflict"
)

// kueueGPUResource is the extended resource the training ClusterQueue caps.
//
// Training GPU quota lives on this key in Kueue, while serving quota stays on the namespace ResourceQuota, so the same GPUs are not counted twice.
var kueueGPUResource = corev1.ResourceName("nvidia.com/gpu")

// kueueQueueName is the deterministic name of the per-tenant ClusterQueue and LocalQueue.
func kueueQueueName(tenant string) string {
	return "gpu-" + tenant
}

// applyClusterQueueSpec sets only the fields this controller owns on the ClusterQueue.
//
// It leaves server-defaulted fields (queueingStrategy, stopPolicy, flavorFungibility) untouched, so drift detection compares like with like and does not fight the apiserver in an update loop.
func applyClusterQueueSpec(cq *kueuev1beta1.ClusterQueue, policy *platformv1.GPUQuotaPolicy) {
	quota := resource.NewQuantity(int64(policy.Spec.Limits.GPUCount), resource.DecimalSI)

	cq.Spec.Cohort = kueuev1beta1.CohortReference(kueueCohort)
	cq.Spec.NamespaceSelector = &metav1.LabelSelector{}
	cq.Spec.ResourceGroups = []kueuev1beta1.ResourceGroup{{
		CoveredResources: []corev1.ResourceName{kueueGPUResource},
		Flavors: []kueuev1beta1.FlavorQuotas{{
			Name: kueuev1beta1.ResourceFlavorReference(kueueFlavor),
			Resources: []kueuev1beta1.ResourceQuota{{
				Name:         kueueGPUResource,
				NominalQuota: *quota,
			}},
		}},
	}}

	if cq.Spec.Preemption == nil {
		cq.Spec.Preemption = &kueuev1beta1.ClusterQueuePreemption{}
	}
	cq.Spec.Preemption.ReclaimWithinCohort = kueuev1beta1.PreemptionPolicyAny
	cq.Spec.Preemption.WithinClusterQueue = kueuev1beta1.PreemptionPolicyLowerPriority
}

// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=localqueues,verbs=get;list;watch;create;update;patch;delete

// syncKueueTrainingQuota ensures the per-tenant Kueue ClusterQueue and LocalQueue track the policy.
//
// When trainingQuota is false the queues are removed, so training GPU quota is not double counted against the namespace ResourceQuota.
//
// It returns handled=true when the caller should return the given result immediately, which happens on an ownership conflict or a create race.
func (r *GPUQuotaPolicyReconciler) syncKueueTrainingQuota(ctx context.Context, policy *platformv1.GPUQuotaPolicy) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)

	// When the tenant does not opt into training quota, ensure any previously synced queues are removed.
	if !policy.Spec.TrainingQuota {
		if err := r.deleteKueueQuota(ctx, policy); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{}, false, nil
	}

	name := kueueQueueName(policy.Spec.Tenant)

	// Sync the cluster-scoped ClusterQueue.
	//
	// A cluster-scoped GPUQuotaPolicy may own a cluster-scoped dependent, so the owner reference drives garbage collection.
	cqKey := types.NamespacedName{Name: name}
	var cq kueuev1beta1.ClusterQueue
	switch err := r.Get(ctx, cqKey, &cq); {
	case apierrors.IsNotFound(err):
		cq = kueuev1beta1.ClusterQueue{ObjectMeta: metav1.ObjectMeta{Name: name}}
		applyClusterQueueSpec(&cq, policy)
		if err := controllerutil.SetControllerReference(policy, &cq, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}
		if err := r.Create(ctx, &cq); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost a race: the object now exists, so requeue and reconcile it on the next pass instead of failing.
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			return ctrl.Result{}, false, err
		}
		log.Info("Created ClusterQueue", "clusterQueue", name)
	case err != nil:
		return ctrl.Result{}, false, err
	default:
		// Refuse to hijack a ClusterQueue this policy does not own.
		if !metav1.IsControlledBy(&cq, policy) {
			res, err := r.markDegraded(ctx, policy, reasonKueueConflict,
				fmt.Sprintf("ClusterQueue %s already exists and is not owned by this policy", name))
			return res, true, err
		}
		// Correct drift on the fields this controller owns, leaving server-defaulted fields intact.
		before := cq.Spec.DeepCopy()
		applyClusterQueueSpec(&cq, policy)
		if !equality.Semantic.DeepEqual(*before, cq.Spec) {
			if err := r.Update(ctx, &cq); err != nil {
				return ctrl.Result{}, false, err
			}
			log.Info("Corrected ClusterQueue drift", "clusterQueue", name)
		}
	}

	// Sync the namespaced LocalQueue in the tenant target namespace.
	lqKey := types.NamespacedName{Name: name, Namespace: policy.Spec.TargetNamespace}
	want := kueuev1beta1.ClusterQueueReference(name)
	var lq kueuev1beta1.LocalQueue
	switch err := r.Get(ctx, lqKey, &lq); {
	case apierrors.IsNotFound(err):
		lq = kueuev1beta1.LocalQueue{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policy.Spec.TargetNamespace},
			Spec:       kueuev1beta1.LocalQueueSpec{ClusterQueue: want},
		}
		if err := controllerutil.SetControllerReference(policy, &lq, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}
		if err := r.Create(ctx, &lq); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			return ctrl.Result{}, false, err
		}
		log.Info("Created LocalQueue", "localQueue", lqKey.String())
	case err != nil:
		return ctrl.Result{}, false, err
	default:
		if !metav1.IsControlledBy(&lq, policy) {
			res, err := r.markDegraded(ctx, policy, reasonKueueConflict,
				fmt.Sprintf("LocalQueue %s already exists and is not owned by this policy", lqKey.String()))
			return res, true, err
		}
		if lq.Spec.ClusterQueue != want {
			lq.Spec.ClusterQueue = want
			if err := r.Update(ctx, &lq); err != nil {
				return ctrl.Result{}, false, err
			}
			log.Info("Corrected LocalQueue drift", "localQueue", lqKey.String())
		}
	}

	return ctrl.Result{}, false, nil
}

// deleteKueueQuota removes the per-tenant Kueue ClusterQueue and LocalQueue this policy owns.
//
// It is called when trainingQuota flips false or on policy deletion, and it ignores not-found so it is safe to call repeatedly.
//
// The queue name is derived from the tenant, which two policies in different namespaces can share, so deletion only ever removes a queue this policy controls.
//
// Deleting purely by name would let a non-owning policy tear down another policy's active queue, which the create path already refuses to do.
//
// envtest has no garbage collector, so this explicit cleanup is what removes the owned queues in tests; in a real cluster the owner references remove them as well.
func (r *GPUQuotaPolicyReconciler) deleteKueueQuota(ctx context.Context, policy *platformv1.GPUQuotaPolicy) error {
	name := kueueQueueName(policy.Spec.Tenant)

	lqKey := types.NamespacedName{Name: name, Namespace: policy.Spec.TargetNamespace}
	var lq kueuev1beta1.LocalQueue
	switch err := r.Get(ctx, lqKey, &lq); {
	case err == nil:
		if metav1.IsControlledBy(&lq, policy) {
			if err := r.Delete(ctx, &lq); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	case apierrors.IsNotFound(err):
		// already gone
	default:
		return err
	}

	cqKey := types.NamespacedName{Name: name}
	var cq kueuev1beta1.ClusterQueue
	switch err := r.Get(ctx, cqKey, &cq); {
	case err == nil:
		if metav1.IsControlledBy(&cq, policy) {
			if err := r.Delete(ctx, &cq); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	case apierrors.IsNotFound(err):
		// already gone
	default:
		return err
	}

	return nil
}
