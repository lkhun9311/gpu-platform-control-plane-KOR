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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

const (
	// gpuQuotaFinalizer guards GPUQuotaPolicy cleanup.
	//
	// On deletion the reconciler deletes the synced ResourceQuota before dropping this finalizer.
	//
	// envtest has no GC, so cleanup is explicit.
	gpuQuotaFinalizer = "gpuquotapolicy.platform.lkhun9311.github.io/finalizer"

	// conditionSynced reports whether the namespace ResourceQuota matches the policy.
	conditionSynced = "Synced"
	// conditionAdmitting reports whether the Kueue ClusterQueue this policy owns can actually admit work.
	//
	// It exists because Synced answers a narrower question than a reader assumes: it says the objects were
	// WRITTEN. A ClusterQueue can be written exactly as specified and still admit nothing — most obviously
	// when the shared ResourceFlavor it references is absent, which Kueue reports as Active=False
	// FlavorNotFound. That happened on a live cluster: the policy read Synced=True, phase=Synced, and every
	// training Job submitted to it sat suspended forever with the tenant's quota apparently in place.
	//
	// Two facts, two conditions. Collapsing them means the only signal a reader has cannot distinguish a
	// quota that exists from a quota that works.
	conditionAdmitting  = "Admitting"
	reasonQueueActive   = "ClusterQueueActive"
	reasonQueueInactive = "ClusterQueueInactive"
	reasonQueueUnread   = "ClusterQueueUnreadable"
	reasonQuotaSynced   = "QuotaSynced"
	reasonQuotaConflict = "QuotaConflict"

	// phaseSynced is set once the ResourceQuota matches the policy ceiling.
	//
	// phaseDegraded is set on a deterministic enforcement failure (e.g. a name collision with a ResourceQuota this policy does not own).
	//
	// Transient API errors are not reflected in status: they are requeued instead, so the phase does not flap on retry.
	//
	// These phases are owned by this controller, not shared, so the NodeHealth controller can rename its own phases independently.
	phaseSynced   = "Synced"
	phaseDegraded = "Degraded"

	// gpuRequestsResource is the ResourceQuota key that caps GPU consumption.
	//
	// Extended resources are tracked under requests.<resource>.
	//
	// Locally this caps simulated nvidia.com/gpu capacity.
	gpuRequestsResource = corev1.ResourceName("requests.nvidia.com/gpu")
)

// GPUQuotaPolicyReconciler reconciles a GPUQuotaPolicy object
type GPUQuotaPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;patch

// Reconcile syncs a namespace ResourceQuota from the GPUQuotaPolicy: the GPU ceiling is enforced as a hard requests.nvidia.com/gpu limit, kept in sync against drift, and removed on deletion.
func (r *GPUQuotaPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy platformv1.GPUQuotaPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rqKey := types.NamespacedName{Name: quotaName(policy.Name), Namespace: policy.Spec.TargetNamespace}

	// Handle deletion: delete the synced ResourceQuota and any Kueue queues, then drop the finalizer.
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&policy, gpuQuotaFinalizer) {
			var rq corev1.ResourceQuota
			switch err := r.Get(ctx, rqKey, &rq); {
			case err == nil:
				// Only delete a ResourceQuota this policy owns, so a same-named ResourceQuota planted by someone else is never removed on this policy's deletion.
				if metav1.IsControlledBy(&rq, &policy) {
					if err := r.Delete(ctx, &rq); err != nil && !apierrors.IsNotFound(err) {
						return ctrl.Result{}, err
					}
					log.Info("Deleted synced ResourceQuota on deletion", "resourceQuota", rqKey.String())
				}
			case apierrors.IsNotFound(err):
				// already gone
			default:
				return ctrl.Result{}, err
			}

			// Delete the per-tenant Kueue queues explicitly for ordered cleanup, mirroring the ResourceQuota path above.
			//
			// The owner references also garbage collect them in a real cluster, but envtest has no garbage collector.
			if err := r.deleteKueueQuota(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(&policy, gpuQuotaFinalizer)
			if err := r.Update(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before creating owned objects.
	if !controllerutil.ContainsFinalizer(&policy, gpuQuotaFinalizer) {
		controllerutil.AddFinalizer(&policy, gpuQuotaFinalizer)
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Turn the admission guards on for this namespace BEFORE syncing any ceiling into it.
	//
	// The order is the argument, and it is the opposite of the convenient one. Syncing a quota into a
	// namespace where the guards are off publishes a ceiling that a bare Pod walks straight past -- measured,
	// not supposed: a Pod asking for two devices ran in a namespace with a synced policy and no Kueue
	// Workload behind it. Doing the enforcement first means the worst case of a half-finished reconcile is a
	// guarded namespace with no ceiling yet, rather than a ceiling nobody has to respect.
	//
	// A failure here fails the whole reconcile rather than degrading, and that is deliberate. The obvious
	// alternative -- carry on and record a condition -- reproduces exactly the state this exists to end: a
	// policy that reads Synced while the bypass it was written to close is open. A missing RBAC grant is
	// loud, diagnosable and fixed once; a silently unguarded namespace is none of those.
	if err := r.ensureNamespaceEnforced(ctx, policy.Spec.TargetNamespace); err != nil {
		return r.markDegraded(ctx, &policy, "EnforcementNotApplied", fmt.Sprintf(
			"could not mark namespace %q as GPU-quota-enforced, so this policy's ceiling would not be "+
				"enforced against direct device requests: %v", policy.Spec.TargetNamespace, err))
	}

	// Sync the namespace ResourceQuota for any non-training quota, then the Kueue ClusterQueue for training quota.
	//
	// A conflict or a create race in the ResourceQuota sync short-circuits this reconcile.
	if res, handled, err := r.syncNamespaceResourceQuota(ctx, &policy, rqKey); handled || err != nil {
		return res, err
	}

	// Sync the per-tenant Kueue ClusterQueue and LocalQueue when the tenant opts into training quota.
	//
	// A conflict or a create race is handled inside the sync and short-circuits this reconcile.
	if res, handled, err := r.syncKueueTrainingQuota(ctx, &policy); handled || err != nil {
		return res, err
	}

	// Reflect the synced state into status, idempotently.
	//
	// The message names where the GPU ceiling is enforced, since training quota lives in the Kueue ClusterQueue and no namespace ResourceQuota exists for it.
	syncedMessage := "ResourceQuota synced from policy"
	if policy.Spec.TrainingQuota {
		syncedMessage = "GPU quota synced to Kueue ClusterQueue from policy"
	}
	desired := policy.Status.DeepCopy()
	desired.ObservedGeneration = policy.Generation
	setQuotaPhase(desired, phaseSynced)
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               conditionSynced,
		Status:             metav1.ConditionTrue,
		Reason:             reasonQuotaSynced,
		Message:            syncedMessage,
		ObservedGeneration: policy.Generation,
	})
	// Reported only for a policy that has a queue at all. Without training quota the GPU ceiling is the
	// namespace ResourceQuota, which admits nothing and is not supposed to.
	if policy.Spec.TrainingQuota {
		cond := r.admittingCondition(ctx, &policy)
		cond.ObservedGeneration = policy.Generation
		meta.SetStatusCondition(&desired.Conditions, cond)
	} else {
		meta.RemoveStatusCondition(&desired.Conditions, conditionAdmitting)
	}

	if !equality.Semantic.DeepEqual(policy.Status, *desired) {
		policy.Status = *desired
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Updated GPUQuotaPolicy status", "name", policy.Name, "phase", desired.Phase)
	}

	return ctrl.Result{}, nil
}

// syncNamespaceResourceQuota keeps the namespace ResourceQuota in step with the policy's non-training quota.
//
// spec.gpuClass is not yet enforced per class, so this caps a single aggregate key (requests.nvidia.com/gpu) regardless of class; two policies with different gpuClass values targeting one namespace cap the same key, and k8s AND-s the quotas so the strictest wins.
//
// When trainingQuota is set, the GPU ceiling is enforced by the Kueue ClusterQueue instead, so the namespace ResourceQuota must not also cap GPUs, or admitted training pods are rejected by a second quota and the same GPUs are counted twice.
//
// The namespace ResourceQuota holds only the GPU key today, so enabling training quota leaves nothing for it to enforce and it is removed; a future serving quota key would keep it.
//
// It returns handled=true when the caller should return the given result immediately, which happens on an ownership conflict or a create race.
func (r *GPUQuotaPolicyReconciler) syncNamespaceResourceQuota(ctx context.Context, policy *platformv1.GPUQuotaPolicy, rqKey types.NamespacedName) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)

	desiredHard := corev1.ResourceList{}
	if !policy.Spec.TrainingQuota {
		desiredHard[gpuRequestsResource] = *resource.NewQuantity(int64(policy.Spec.Limits.GPUCount), resource.DecimalSI)
	}

	// Nothing is left for the namespace ResourceQuota to enforce, so remove any ResourceQuota this policy previously synced.
	if len(desiredHard) == 0 {
		var rq corev1.ResourceQuota
		switch err := r.Get(ctx, rqKey, &rq); {
		case err == nil:
			if metav1.IsControlledBy(&rq, policy) {
				if err := r.Delete(ctx, &rq); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, false, err
				}
				log.Info("Deleted namespace ResourceQuota because training quota is enforced by Kueue", "resourceQuota", rqKey.String())
			}
		case apierrors.IsNotFound(err):
			// already absent
		default:
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{}, false, nil
	}

	var rq corev1.ResourceQuota
	switch err := r.Get(ctx, rqKey, &rq); {
	case apierrors.IsNotFound(err):
		rq = corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: rqKey.Name, Namespace: rqKey.Namespace},
			Spec:       corev1.ResourceQuotaSpec{Hard: desiredHard},
		}
		if err := controllerutil.SetControllerReference(policy, &rq, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}
		if err := r.Create(ctx, &rq); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost a race (concurrent reconcile or informer lag): the object now exists, so requeue and reconcile it on the next pass instead of failing.
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			return ctrl.Result{}, false, err
		}
		log.Info("Created ResourceQuota", "resourceQuota", rqKey.String())
	case err != nil:
		return ctrl.Result{}, false, err
	default:
		// Refuse to hijack a ResourceQuota this policy does not own (name collision with an unrelated object).
		//
		// Overwriting it would clobber someone else's quota, so report Degraded and recheck later instead of taking it over.
		if !metav1.IsControlledBy(&rq, policy) {
			log.Info("ResourceQuota exists but is not owned by this policy; refusing to overwrite",
				"resourceQuota", rqKey.String())
			res, err := r.markDegraded(ctx, policy, reasonQuotaConflict,
				fmt.Sprintf("ResourceQuota %s already exists and is not owned by this policy", rqKey.String()))
			return res, true, err
		}
		if !equality.Semantic.DeepEqual(rq.Spec.Hard, desiredHard) {
			rq.Spec.Hard = desiredHard
			if err := r.Update(ctx, &rq); err != nil {
				return ctrl.Result{}, false, err
			}
			log.Info("Corrected ResourceQuota drift", "resourceQuota", rqKey.String())

			// Count the correction only after the update succeeds, so a failed write is never counted as a fix.
			gpuQuotaPolicyDriftCorrectedTotal.Inc()
		}
	}

	return ctrl.Result{}, false, nil
}

// quotaName is the deterministic name of the ResourceQuota synced for a policy.
func quotaName(policyName string) string {
	return "gpuquota-" + policyName
}

// markDegraded reflects a deterministic enforcement failure into status as Degraded with a Synced=False condition.
//
// It returns a RequeueAfter so the policy recovers automatically once the blocking condition clears (the Owns watch does not fire for a ResourceQuota we do not own).
func (r *GPUQuotaPolicyReconciler) markDegraded(ctx context.Context, policy *platformv1.GPUQuotaPolicy, reason, msg string) (ctrl.Result, error) {
	desired := policy.Status.DeepCopy()
	desired.ObservedGeneration = policy.Generation
	setQuotaPhase(desired, phaseDegraded)
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               conditionSynced,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: policy.Generation,
	})
	if !equality.Semantic.DeepEqual(policy.Status, *desired) {
		policy.Status = *desired
		if err := r.Status().Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// admittingCondition asks the ClusterQueue whether it can admit, and reports its answer verbatim.
//
// Kueue's own Active condition is the authority and its reason is carried through unchanged rather than
// reworded: an operator searching for "FlavorNotFound" should find it here, and a reason this controller
// invented would be one more string to correlate.
//
// An unreadable queue is reported as not admitting rather than as an error on the reconcile. The policy's
// other guarantees still hold and failing the whole reconcile would hide them behind a read that may recover
// on its own; what must not happen is the condition silently staying True from a previous pass.
func (r *GPUQuotaPolicyReconciler) admittingCondition(ctx context.Context, policy *platformv1.GPUQuotaPolicy) metav1.Condition {
	name := kueueQueueName(policy.Spec.Tenant)
	var cq kueuev1beta1.ClusterQueue
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &cq); err != nil {
		return metav1.Condition{
			Type: conditionAdmitting, Status: metav1.ConditionFalse, Reason: reasonQueueUnread,
			Message: fmt.Sprintf("could not read ClusterQueue %s: %v", name, err),
		}
	}
	active := meta.FindStatusCondition(cq.Status.Conditions, "Active")
	if active == nil {
		return metav1.Condition{
			Type: conditionAdmitting, Status: metav1.ConditionFalse, Reason: reasonQueueInactive,
			Message: fmt.Sprintf("ClusterQueue %s has not reported whether it is active", name),
		}
	}
	if active.Status != metav1.ConditionTrue {
		return metav1.Condition{
			Type: conditionAdmitting, Status: metav1.ConditionFalse, Reason: reasonQueueInactive,
			Message: fmt.Sprintf("ClusterQueue %s admits nothing: %s: %s", name, active.Reason, active.Message),
		}
	}
	return metav1.Condition{
		Type: conditionAdmitting, Status: metav1.ConditionTrue, Reason: reasonQueueActive,
		Message: fmt.Sprintf("ClusterQueue %s is admitting", name),
	}
}

// setQuotaPhase updates the phase and bumps lastTransitionTime only when the phase changes.
func setQuotaPhase(status *platformv1.GPUQuotaPolicyStatus, phase string) {
	if status.Phase == phase {
		return
	}
	status.Phase = phase
	now := metav1.Now()
	status.LastTransitionTime = &now
}

// SetupWithManager sets up the controller with the Manager.
func (r *GPUQuotaPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.GPUQuotaPolicy{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&kueuev1beta1.ClusterQueue{}).
		Owns(&kueuev1beta1.LocalQueue{}).
		Named("gpuquotapolicy").
		Complete(r)
}

// ensureNamespaceEnforced stamps the guard's selector label on the policy's target namespace.
//
// It patches rather than updates so it cannot clobber labels it did not write, and it reads first so the
// ordinary steady-state reconcile issues no write at all -- this runs on every reconcile of every policy,
// and an unconditional patch would be a write per reconcile forever.
//
// It never REMOVES the label, including when a policy is deleted. That asymmetry is deliberate: turning
// enforcement off is the operation that re-opens a bypass, and it would fire on a policy deleted by mistake,
// on a finalizer race, or on any reconcile that saw a stale cache. Leaving a namespace guarded after its
// policy is gone costs a refusal message that names a ClusterQueue nobody is using; removing it costs the
// guarantee. An operator who genuinely wants the namespace unguarded can remove one label by hand, which is
// the correct amount of friction for that direction.
func (r *GPUQuotaPolicyReconciler) ensureNamespaceEnforced(ctx context.Context, name string) error {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		return fmt.Errorf("read namespace %q: %w", name, err)
	}
	if ns.Labels[platformv1.QuotaEnforcedLabel] == platformv1.QuotaEnforcedValue {
		return nil
	}
	patch := client.MergeFrom(ns.DeepCopy())
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels[platformv1.QuotaEnforcedLabel] = platformv1.QuotaEnforcedValue
	if err := r.Patch(ctx, &ns, patch); err != nil {
		return fmt.Errorf("label namespace %q: %w", name, err)
	}
	return nil
}
