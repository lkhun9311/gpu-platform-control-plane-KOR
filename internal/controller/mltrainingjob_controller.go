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
	"maps"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// MLTrainingJobReconciler reconciles a MLTrainingJob object
type MLTrainingJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch

// Reconcile syncs an owned, suspended batch/v1 Job from the MLTrainingJob spec.
//
// Suspend is set true only when the Job is first created.
//
// Kueue flips it to false to admit the workload, and this reconciler never touches it again on update, or the operator and Kueue would fight over the field forever.
func (r *MLTrainingJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var mltj platformv1.MLTrainingJob
	if err := r.Get(ctx, req.NamespacedName, &mltj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: the owned Job carries a controller owner reference, so garbage collection removes it.
	//
	// The reconciler only needs to drop the finalizer once that has had a chance to happen.
	if !mltj.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&mltj, mlTrainingJobFinalizer) {
			controllerutil.RemoveFinalizer(&mltj, mlTrainingJobFinalizer)
			if err := r.Update(ctx, &mltj); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer from mltrainingjob %s: %w", mltj.Name, err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before doing any work.
	if !controllerutil.ContainsFinalizer(&mltj, mlTrainingJobFinalizer) {
		controllerutil.AddFinalizer(&mltj, mlTrainingJobFinalizer)
		if err := r.Update(ctx, &mltj); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to mltrainingjob %s: %w", mltj.Name, err)
		}
		return ctrl.Result{}, nil
	}

	if conflict, err := r.ownedConflict(ctx, &mltj, &batchv1.Job{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("check job ownership %s/%s: %w", mltj.Namespace, mltj.Name, err)
	} else if conflict {
		log.Info("Job exists and is not owned by this MLTrainingJob; refusing to adopt", "name", mltj.Name)
		return r.markFailed(ctx, &mltj, mltjReasonConflict, "a Job of the same name is not owned by this MLTrainingJob")
	}

	desired := BuildJob(&mltj)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: mltj.Name, Namespace: mltj.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Suspend and the pod template are set once, on create, and never touched again.
		//
		// Suspend must stay untouched after that so Kueue can unsuspend to admit the workload.
		//
		// The template is immutable once the Job exists (its labels must keep matching the server-assigned selector), so re-sending our locally built copy on every reconcile would fail validation.
		if job.CreationTimestamp.IsZero() {
			job.Spec.Suspend = new(true)
			job.Spec.Template = desired.Spec.Template
		}
		// Merge the desired labels in rather than replacing the map, so any label Kueue or the apiserver adds to the Job is preserved across reconciles.
		if job.Labels == nil {
			job.Labels = map[string]string{}
		}
		maps.Copy(job.Labels, desired.Labels)
		job.Spec.Parallelism = desired.Spec.Parallelism
		job.Spec.Completions = desired.Spec.Completions
		return controllerutil.SetControllerReference(&mltj, job, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync training job %s/%s: %w", mltj.Namespace, mltj.Name, err)
	}

	log.Info("Synced training job", "mlTrainingJob", req.String())

	wl, err := r.getWorkloadForJob(ctx, job)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get kueue workload for job %s/%s: %w", mltj.Namespace, mltj.Name, err)
	}

	phase, cond := computeMLTJPhase(job, wl)
	if err := r.setMLTJPhase(ctx, &mltj, phase, cond); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// BuildJob renders the desired batch/v1 Job for a training job.
//
// Suspend is deliberately absent here: the caller decides whether to set it, since it only applies on the create path and must never be reconciled on update, as Kueue owns it after admission.
//
// It is exported, and takes no receiver, because the Pod template below is the one thing about this controller another binary has to be able to ask about without running it: cmd/queuelabrun keys its termination qualification on this template, so that adding a preStop hook or a grace period here invalidates a reading taken before the change instead of quietly changing what a run measures.
//
// That makes one property of this function load-bearing outside this package, and it was already true when the receiver was still here and unused: it must stay a pure function of its argument.
//
// A later version that read anything off the client, the manager or the cluster would still render a Job for the reconciler and would panic or lie for a caller that has none of those.
func BuildJob(mltj *platformv1.MLTrainingJob) *batchv1.Job {
	labels := map[string]string{kueueQueueLabel: mltj.Spec.Queue}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: mltj.Name, Namespace: mltj.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			Parallelism: new(defaultOne(mltj.Spec.Parallelism)),
			Completions: new(defaultOne(mltj.Spec.Completions)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "trainer",
						Image:   mltj.Spec.Image,
						Command: mltj.Spec.Command,
						Env:     driverCapabilities(mltj.Spec.GPUCount),
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{nvidiaGPUResource: *resource.NewQuantity(int64(mltj.Spec.GPUCount), resource.DecimalSI)},
						},
					}},
				},
			},
		},
	}
}

// driverCapabilities states which NVIDIA driver libraries this Pod needs mounted into it, for a Pod that asks for a device.
//
// nvidia-container-toolkit does not ship a CUDA runtime into a container: it bind-mounts the HOST's driver libraries, and which of them it mounts is decided by NVIDIA_DRIVER_CAPABILITIES at container CREATION. The workload cannot set it, because by the time any process in the container runs, the mounts have already been decided.
//
// Leaving it unset means taking whatever the toolkit on that node defaults to, and the default is the problem. Historically it was "utility", which mounts nvidia-smi and libnvidia-ml and NOT libcuda.so.1 -- "compute" is what mounts the one a CUDA program loads. Newer toolkit builds default to "utility,compute" and CDI-based setups are more permissive still, so whether a container gets libcuda depends on the AMI, the toolkit version and the runtime mode rather than on anything this repository states.
//
// The failure that produces is silent and expensive. A GPU is allocated, the Pod starts, the workload's ctypes.CDLL("libcuda.so.1") raises, it falls back to the CPU loop, and the run completes with plausible iteration counts and device-not-observed -- indistinguishable from a cluster with no cards at all. That is exactly the outcome the alpine-to-glibc image move was made to prevent; the move fixed the linkage half and left this half to a default.
//
// It is set only when a device is requested. A Pod asking for no GPU that declared driver capabilities anyway would be making a claim about hardware it has not been given.
//
// This changes the rendered Pod template, so it changes podTemplateHashOf and invalidates every termination canary taken before it -- by design, and the reason to do it before a GPU node exists rather than after: the re-take is free on a cluster whose workers are already there.
func driverCapabilities(gpuCount int32) []corev1.EnvVar {
	if gpuCount <= 0 {
		return nil
	}
	return []corev1.EnvVar{{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "compute,utility"}}
}

// defaultOne returns v, or 1 when v is not positive.
//
// The CRD's kubebuilder default of 1 only applies when the field is omitted from a request payload.
//
// It does not backfill a zero-value struct built in Go, so BuildJob applies the same default explicitly.
func defaultOne(v int32) int32 {
	if v <= 0 {
		return 1
	}
	return v
}

// markFailed reflects a deterministic failure into status as Failed with the JobSynced condition set to False.
//
// It returns a RequeueAfter so the job recovers automatically once the blocking condition clears, since a conflicting foreign Job is not owned and so does not trigger the owned-Job watch.
func (r *MLTrainingJobReconciler) markFailed(ctx context.Context, mltj *platformv1.MLTrainingJob, reason, msg string) (ctrl.Result, error) {
	desired := mltj.Status.DeepCopy()
	phaseChanged := desired.Phase != mltjPhaseFailed
	desired.Phase = mltjPhaseFailed
	desired.ObservedGeneration = mltj.Generation
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: mltjCondSynced, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: mltj.Generation,
	})
	if !equality.Semantic.DeepEqual(mltj.Status, *desired) {
		if phaseChanged {
			now := metav1.Now()
			desired.LastTransitionTime = &now
		}
		mltj.Status = *desired
		if err := r.Status().Update(ctx, mltj); err != nil {
			return ctrl.Result{}, fmt.Errorf("update mltrainingjob status %s/%s to Failed: %w", mltj.Namespace, mltj.Name, err)
		}

		// Count the transition only after the status write succeeds, so a reconcile that finds the object already Failed does not inflate the metric.
		mlTrainingJobFailedTotal.WithLabelValues(reason).Inc()

		// The phase counter tracks every entry into a phase, so a failure reached here counts the same as one reached through the normal phase translation.
		if phaseChanged {
			mlTrainingJobPhaseTotal.WithLabelValues(mltjPhaseFailed).Inc()
		}
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// computeMLTJPhase derives the training phase from the Job and its Kueue Workload.
//
// Kueue admission is read from the Workload's Admitted condition, since the Job's suspend flag alone is ambiguous mid-transition.
func computeMLTJPhase(job *batchv1.Job, wl *kueuev1beta1.Workload) (string, metav1.Condition) {
	if isJobConditionTrue(job, batchv1.JobFailed) {
		return mltjPhaseFailed, admittedCondition(metav1.ConditionFalse, "JobFailed")
	}
	if isJobConditionTrue(job, batchv1.JobComplete) {
		return mltjPhaseSucceeded, admittedCondition(metav1.ConditionTrue, "JobComplete")
	}
	if job.Status.Active > 0 {
		return mltjPhaseRunning, admittedCondition(metav1.ConditionTrue, "PodsRunning")
	}
	if wl != nil && meta.IsStatusConditionTrue(wl.Status.Conditions, kueuev1beta1.WorkloadAdmitted) {
		return mltjPhaseAdmitted, admittedCondition(metav1.ConditionTrue, "QuotaReserved")
	}
	return mltjPhasePending, admittedCondition(metav1.ConditionFalse, "QueuedForAdmission")
}

// isJobConditionTrue reports whether the Job carries the given condition type with status True.
func isJobConditionTrue(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for i := range job.Status.Conditions {
		if job.Status.Conditions[i].Type == condType {
			return job.Status.Conditions[i].Status == corev1.ConditionTrue
		}
	}
	return false
}

// admittedCondition builds the MLTrainingJob Admitted condition for a phase transition.
//
// The message is left empty; the reason and the phase it is paired with already say what happened.
func admittedCondition(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: mltjCondAdmitted, Status: status, Reason: reason}
}

// WorkloadJobRefIndex indexes a Kueue Workload by every Job UID that could claim it.
//
// A cached List narrows by FIELD indexes only. A label selector is applied by walking the whole store, so
// the label lookup this replaced cost O(N) in the number of Workloads even when it matched immediately —
// measured in BenchmarkGetWorkloadForJobHit, which never reaches the fallback and still grew linearly.
const WorkloadJobRefIndex = ".metadata.jobRef"

// indexWorkloadByJobRef emits both claims a Workload can carry: the job-uid label and its owner references.
//
// Both go into ONE index so the lookup is a single List. Indexing only the label would leave the owner-
// reference fallback listing the entire namespace, which measured about 20x the label walk because it
// deep-copies every Workload into the result rather than the one it matched.
func indexWorkloadByJobRef(o client.Object) []string {
	wl, ok := o.(*kueuev1beta1.Workload)
	if !ok {
		return nil
	}
	// A Workload can be claimed by at most one Job, but the two claims are indexed separately because a
	// Workload missing the label is exactly the case the owner-reference path exists to catch.
	refs := make([]string, 0, 1+len(wl.OwnerReferences))
	if uid := wl.Labels[kueueJobUIDLabel]; uid != "" {
		refs = append(refs, uid)
	}
	for _, ref := range wl.OwnerReferences {
		if string(ref.UID) != "" {
			refs = append(refs, string(ref.UID))
		}
	}
	return refs
}

// getWorkloadForJob returns the Kueue Workload created for a Job, or nil if Kueue has not created one yet.
//
// Kueue stamps every Workload it creates with the kueue.x-k8s.io/job-uid label, so a labelled match is
// preferred. A Workload whose owner reference points at the Job's UID is accepted as a fallback, in case a
// Workload ever exists without the label.
//
// Both claims resolve through one indexed List. This lookup runs on the way in for EVERY MLTrainingJob —
// before Kueue has created the Workload there is nothing to match, which is the path that used to pay for
// both a full label walk and a full namespace list. With MaxConcurrentReconciles at its default of 1, a
// per-item cost proportional to the namespace's size drains the queue in O(N^2).
//
// The index lives on the manager's cache, so a client that reads through to the apiserver cannot serve this
// List: the apiserver rejects field selectors it does not define for the resource.
func (r *MLTrainingJobReconciler) getWorkloadForJob(ctx context.Context, job *batchv1.Job) (*kueuev1beta1.Workload, error) {
	var claimed kueuev1beta1.WorkloadList
	if err := r.List(ctx, &claimed, client.InNamespace(job.Namespace),
		client.MatchingFields{WorkloadJobRefIndex: string(job.UID)}); err != nil {
		return nil, fmt.Errorf("list workloads claiming job %s/%s: %w", job.Namespace, job.Name, err)
	}
	if len(claimed.Items) == 0 {
		return nil, nil
	}

	// The label is preferred over the owner reference, so the two are separated here rather than taking
	// whichever the index happened to return first.
	//
	// Within each group the OLDEST wins, by creation time and then by name. List order is not a selection
	// rule: the cache can return duplicates in either order, so "the first one" made the chosen Workload — and
	// therefore the phase written to status — depend on which way a map happened to iterate. Status could
	// oscillate between two Workloads with nothing having changed. Oldest-first is the same tie-break the
	// gateway's backend ordering uses, and for the same reason: a deterministic answer that does not move.
	var labelled, owned *kueuev1beta1.Workload
	for i := range claimed.Items {
		wl := &claimed.Items[i]
		if wl.Labels[kueueJobUIDLabel] == string(job.UID) {
			if labelled == nil || olderWorkload(wl, labelled) {
				labelled = wl
			}
			continue
		}
		if owned == nil || olderWorkload(wl, owned) {
			owned = wl
		}
	}

	if labelled != nil {
		// Kueue keeps one Workload per Job UID, so more than one match means that invariant was violated upstream.
		//
		// Surface it rather than silently picking one, then take the oldest so the reconcile still makes
		// progress, and so a repeated reconcile keeps choosing the same object.
		if n := countLabelled(claimed.Items, string(job.UID)); n > 1 {
			logf.FromContext(ctx).Info("multiple Kueue Workloads share one job-uid label; using the oldest",
				"job", job.Namespace+"/"+job.Name, "count", n)
		}
		return labelled, nil
	}
	return owned, nil
}

// olderWorkload orders two Workloads deterministically: by creation time, then by name.
//
// The name tie-break matters because two objects created in the same second are indistinguishable by
// timestamp alone, and metav1.Time has one-second resolution.
func olderWorkload(a, b *kueuev1beta1.Workload) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// countLabelled reports how many Workloads carry the given Job UID in the job-uid label.
func countLabelled(items []kueuev1beta1.Workload, uid string) int {
	n := 0
	for i := range items {
		if items[i].Labels[kueueJobUIDLabel] == uid {
			n++
		}
	}
	return n
}

// setMLTJPhase writes the phase and the Admitted condition to status, but only when either actually changes.
//
// lastTransitionTime is stamped only on a phase transition, not on every reconcile that finds the same phase.
func (r *MLTrainingJobReconciler) setMLTJPhase(ctx context.Context, mltj *platformv1.MLTrainingJob, phase string, cond metav1.Condition) error {
	desired := mltj.Status.DeepCopy()
	phaseChanged := desired.Phase != phase
	desired.Phase = phase
	desired.ObservedGeneration = mltj.Generation

	cond.ObservedGeneration = mltj.Generation
	meta.SetStatusCondition(&desired.Conditions, cond)

	if equality.Semantic.DeepEqual(mltj.Status, *desired) {
		return nil
	}

	if phaseChanged {
		now := metav1.Now()
		desired.LastTransitionTime = &now
	}

	mltj.Status = *desired
	if err := r.Status().Update(ctx, mltj); err != nil {
		return fmt.Errorf("update mltrainingjob status %s/%s to phase %s: %w", mltj.Namespace, mltj.Name, phase, err)
	}

	// Increment the phase transition counter only when the phase actually changed.
	if phaseChanged {
		mlTrainingJobPhaseTotal.WithLabelValues(phase).Inc()
	}

	return nil
}

// mapWorkloadToMLTrainingJob maps a Workload event to a reconcile request for the MLTrainingJob that owns its Job.
//
// A Kueue Workload's owner reference points at the batch/v1 Job it is admitting, and that Job always shares its name with the MLTrainingJob that created it, so no further lookup is needed.
func mapWorkloadToMLTrainingJob(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		// Match the batch/v1 Job specifically, so a same-named owner from another API group cannot enqueue a spurious request.
		if ref.Kind == "Job" && ref.APIVersion == "batch/v1" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ref.Name, Namespace: obj.GetNamespace()}}}
		}
	}
	return nil
}

// ownedConflict reports whether an object of the given name exists but is not controlled by mltj.
func (r *MLTrainingJobReconciler) ownedConflict(ctx context.Context, mltj *platformv1.MLTrainingJob, obj client.Object) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Name: mltj.Name, Namespace: mltj.Namespace}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	default:
		return !metav1.IsControlledBy(obj, mltj), nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MLTrainingJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// context.Background() rather than a scoped context because IndexField only installs the extractor on the cache; it starts nothing that would need cancelling.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &kueuev1beta1.Workload{}, WorkloadJobRefIndex, indexWorkloadByJobRef,
	); err != nil {
		return fmt.Errorf("index workloads by job ref: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.MLTrainingJob{}).
		Owns(&batchv1.Job{}).
		Watches(&kueuev1beta1.Workload{}, handler.EnqueueRequestsFromMapFunc(mapWorkloadToMLTrainingJob)).
		Named("mltrainingjob").
		Complete(r)
}
