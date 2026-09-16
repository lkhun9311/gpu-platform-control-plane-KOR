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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

var _ = Describe("MLTrainingJob Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceNamespace = "default"

		ctx := context.Background()
		var key types.NamespacedName

		reconciler := func() *MLTrainingJobReconciler {
			return &MLTrainingJobReconciler{Client: cachedClient, Scheme: cachedClient.Scheme()}
		}

		// reconcileUntilSteady drives Reconcile a few times so the finalizer is added and the owned Job is created.
		reconcileUntilSteady := func() {
			for range 3 {
				_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		BeforeEach(func() {
			// Each spec gets its own resource name.
			//
			// envtest's apiserver adds the batch.kubernetes.io/job-tracking finalizer to every Job it admits, and only a running Job controller (absent here) ever retires it, so a Job this suite creates stays "Terminating" forever once deleted.
			//
			// A unique name per spec means that lingering Job never collides with the next spec's Job of the "same" logical name.
			key = types.NamespacedName{Name: fmt.Sprintf("test-training-%s", rand.String(8)), Namespace: resourceNamespace}

			By("creating the custom resource for the Kind MLTrainingJob")
			mltj := &platformv1.MLTrainingJob{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: platformv1.MLTrainingJobSpec{
					Queue:       "team-a",
					Image:       "busybox",
					Command:     []string{"python", "train.py"},
					GPUCount:    2,
					Parallelism: 1,
					Completions: 1,
				},
			}
			Expect(k8sClient.Create(ctx, mltj)).To(Succeed())
		})

		AfterEach(func() {
			By("dropping the MLTrainingJob's finalizer since there is no running manager to do it")
			mltj := &platformv1.MLTrainingJob{}
			if err := k8sClient.Get(ctx, key, mltj); err == nil {
				mltj.Finalizers = nil
				Expect(k8sClient.Update(ctx, mltj)).To(Succeed())
				Expect(k8sClient.Delete(ctx, mltj)).To(Succeed())
			}

			// Best-effort: ask the Job to delete too, but do not wait for it to actually disappear.
			//
			// Its job-tracking finalizer only clears once a real Job controller marks it complete, which never happens in envtest, so it would stay "Terminating" forever; the unique name per spec is what actually prevents cross-spec collisions.
			job := &batchv1.Job{}
			if err := k8sClient.Get(ctx, key, job); err == nil {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, job))).To(Succeed())
			}

			// Best-effort: any Workload a spec created has no controller running to finalize it, so just ask for deletion.
			wl := &kueuev1beta1.Workload{}
			if err := k8sClient.Get(ctx, key, wl); err == nil {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, wl))).To(Succeed())
			}
		})

		It("creates an owned, suspended, Kueue-labeled Job", func() {
			reconcileUntilSteady()

			got := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Finalizers).To(ContainElement(mlTrainingJobFinalizer))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
			Expect(metav1.IsControlledBy(job, got)).To(BeTrue())
			Expect(job.Labels).To(HaveKeyWithValue("kueue.x-k8s.io/queue-name", "team-a"))

			Expect(job.Spec.Suspend).NotTo(BeNil())
			Expect(*job.Spec.Suspend).To(BeTrue())

			Expect(*job.Spec.Parallelism).To(Equal(int32(1)))
			Expect(*job.Spec.Completions).To(Equal(int32(1)))

			Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("trainer"))
			Expect(container.Image).To(Equal("busybox"))
			Expect(container.Command).To(Equal([]string{"python", "train.py"}))
			gpu := container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
			Expect(gpu.Value()).To(Equal(int64(2)))
		})

		It("is idempotent once steady", func() {
			reconcileUntilSteady()

			before := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, before)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			after := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		})

		It("does not reconcile Suspend back to true once Kueue has admitted the Job", func() {
			reconcileUntilSteady()

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
			job.Spec.Suspend = new(bool)
			*job.Spec.Suspend = false
			Expect(k8sClient.Update(ctx, job)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			after := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			Expect(after.Spec.Suspend).NotTo(BeNil())
			Expect(*after.Spec.Suspend).To(BeFalse())
		})

		It("refuses to adopt a Job of the same name it does not own", func() {
			foreign := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "x", Image: "busybox"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			before := testutil.ToFloat64(mlTrainingJobFailedTotal.WithLabelValues(mltjReasonConflict))

			reconcileUntilSteady()

			got := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(mltjPhaseFailed))

			gotJob := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, gotJob)).To(Succeed())
			Expect(gotJob.OwnerReferences).To(BeEmpty())

			after := testutil.ToFloat64(mlTrainingJobFailedTotal.WithLabelValues(mltjReasonConflict))
			Expect(after - before).To(Equal(1.0))
		})

		It("persists a status phase via the status subresource", func() {
			const phasePending = "Pending"

			fetched := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())

			fetched.Status.Phase = phasePending
			Expect(k8sClient.Status().Update(ctx, fetched)).To(Succeed())

			updated := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(phasePending))
		})

		It("moves to Admitted once the Kueue Workload reports Admitted=True, then to Running once the Job has active pods", func() {
			reconcileUntilSteady()

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())

			By("creating the Kueue Workload Kueue would create for this Job, labeled with the Job's UID")
			wl := &kueuev1beta1.Workload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      key.Name,
					Namespace: key.Namespace,
					Labels:    map[string]string{"kueue.x-k8s.io/job-uid": string(job.UID)},
				},
				Spec: kueuev1beta1.WorkloadSpec{
					PodSets: []kueuev1beta1.PodSet{{
						Name:  "main",
						Count: 1,
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								RestartPolicy: corev1.RestartPolicyNever,
								Containers:    []corev1.Container{{Name: "trainer", Image: "busybox"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, wl)).To(Succeed())

			wl.Status.Conditions = []metav1.Condition{{
				Type:               "Admitted",
				Status:             metav1.ConditionTrue,
				Reason:             "QuotaReserved",
				Message:            "",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, wl)).To(Succeed())

			awaitCachedWorkload(wl.Name, wl.Namespace, hasAdmittedCondition)

			By("reconciling so the MLTrainingJob picks up the Workload's Admitted condition")
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			admitted := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, admitted)).To(Succeed())
			Expect(admitted.Status.Phase).To(Equal("Admitted"))
			Expect(admitted.Status.LastTransitionTime).NotTo(BeNil())

			By("giving the Job an active pod, as Kueue's unsuspend would eventually lead to")
			job.Status.Active = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			_, err = reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			running := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, running)).To(Succeed())
			Expect(running.Status.Phase).To(Equal("Running"))
		})

		It("increments mlTrainingJobPhaseTotal counter when transitioning to Admitted phase", func() {
			reconcileUntilSteady()

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())

			By("reading the counter value before the Admitted phase transition")
			before := testutil.ToFloat64(mlTrainingJobPhaseTotal.WithLabelValues(mltjPhaseAdmitted))

			By("creating the Kueue Workload and admitting it")
			wl := &kueuev1beta1.Workload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      key.Name,
					Namespace: key.Namespace,
					Labels:    map[string]string{"kueue.x-k8s.io/job-uid": string(job.UID)},
				},
				Spec: kueuev1beta1.WorkloadSpec{
					PodSets: []kueuev1beta1.PodSet{{
						Name:  "main",
						Count: 1,
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								RestartPolicy: corev1.RestartPolicyNever,
								Containers:    []corev1.Container{{Name: "trainer", Image: "busybox"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, wl)).To(Succeed())

			wl.Status.Conditions = []metav1.Condition{{
				Type:               "Admitted",
				Status:             metav1.ConditionTrue,
				Reason:             "QuotaReserved",
				Message:            "",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, wl)).To(Succeed())

			awaitCachedWorkload(wl.Name, wl.Namespace, hasAdmittedCondition)

			By("reconciling to trigger the phase transition to Admitted")
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the MLTrainingJob transitioned to Admitted")
			admitted := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, key, admitted)).To(Succeed())
			Expect(admitted.Status.Phase).To(Equal(mltjPhaseAdmitted))

			By("reading the counter value after the phase transition")
			after := testutil.ToFloat64(mlTrainingJobPhaseTotal.WithLabelValues(mltjPhaseAdmitted))

			By("asserting the counter incremented by exactly 1")
			Expect(after - before).To(Equal(1.0))
		})
	})
})

// Duplicate Workloads must resolve to the SAME one every time.
//
// Kueue keeps one Workload per Job UID, so more than one is an upstream invariant violation — but the
// reconciler still has to make progress, and "the first the cache returned" is not a rule. List order can
// differ between reconciles, so the phase written to status could oscillate between two Workloads with
// nothing having changed.
//
// Mutation that turns this red: take the first match instead of the oldest.
func TestOlderWorkloadIsADeterministicOrder(t *testing.T) {
	older := &kueuev1beta1.Workload{ObjectMeta: metav1.ObjectMeta{
		Name: "wl-b", CreationTimestamp: metav1.Unix(1000, 0),
	}}
	newer := &kueuev1beta1.Workload{ObjectMeta: metav1.ObjectMeta{
		Name: "wl-a", CreationTimestamp: metav1.Unix(2000, 0),
	}}

	if !olderWorkload(older, newer) {
		t.Fatal("the earlier creation timestamp must win regardless of name")
	}
	if olderWorkload(newer, older) {
		t.Fatal("the ordering is not antisymmetric")
	}

	// Same second, which metav1.Time cannot separate: the name is the tie-break, and without one the answer
	// would still depend on list order for the case duplicates are most likely to hit.
	sameA := &kueuev1beta1.Workload{ObjectMeta: metav1.ObjectMeta{
		Name: "wl-a", CreationTimestamp: metav1.Unix(1000, 0),
	}}
	sameB := &kueuev1beta1.Workload{ObjectMeta: metav1.ObjectMeta{
		Name: "wl-b", CreationTimestamp: metav1.Unix(1000, 0),
	}}
	if !olderWorkload(sameA, sameB) || olderWorkload(sameB, sameA) {
		t.Fatal("two Workloads created in the same second do not order deterministically")
	}
}

// A Pod that asks for a device must say which driver libraries it needs, because it cannot say so later.
//
// nvidia-container-toolkit bind-mounts the host's driver libraries and decides WHICH at container creation,
// from NVIDIA_DRIVER_CAPABILITIES. Nothing inside the container can influence that: by the time a process
// runs, the mounts are made. Leaving it unset takes the toolkit's default, which was historically "utility"
// -- nvidia-smi and libnvidia-ml, and not libcuda.so.1, which is the one a CUDA program loads.
//
// The failure is silent: a device is allocated, the Pod starts, loading libcuda raises, the workload falls
// back to its CPU loop, and the run completes with plausible numbers and no device evidence -- exactly what
// a cluster with no cards produces. That is the outcome the alpine-to-glibc image move was made to prevent,
// and the move fixed the linkage half while leaving this half to a default that varies by AMI and toolkit
// version.
//
// Mutations that turn this red: drop the Env, set only "utility", or set it for a Pod requesting no device.
func TestAGPURequestingPodDeclaresTheDriverLibrariesItNeeds(t *testing.T) {
	envOf := func(gpu int32) []corev1.EnvVar {
		job := BuildJob(&platformv1.MLTrainingJob{
			ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "n"},
			Spec:       platformv1.MLTrainingJobSpec{Image: "i", Queue: "q", GPUCount: gpu},
		})
		return job.Spec.Template.Spec.Containers[0].Env
	}
	var caps string
	for _, e := range envOf(1) {
		if e.Name == "NVIDIA_DRIVER_CAPABILITIES" {
			caps = e.Value
		}
	}
	if caps == "" {
		t.Fatal("a Pod requesting a GPU declares no driver capabilities, so whether libcuda.so.1 is mounted " +
			"into it is decided by whatever the toolkit on that node happens to default to")
	}
	if !strings.Contains(caps, "compute") {
		t.Fatalf("the declared capabilities are %q and do not include compute, which is the one that mounts "+
			"libcuda.so.1; utility alone mounts nvidia-smi and leaves a CUDA program unable to start", caps)
	}

	// And a Pod that asked for no device must not claim capabilities on hardware it was never given.
	for _, e := range envOf(0) {
		if e.Name == "NVIDIA_DRIVER_CAPABILITIES" {
			t.Fatalf("a Pod requesting no GPU declares driver capabilities: %+v", e)
		}
	}
}
