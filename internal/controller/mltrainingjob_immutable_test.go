package controller

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// These specs pin what the API accepts but cannot act on.
//
// The reconciler writes the Job's pod template ONCE, on create, because a batch/v1 Job's template is
// immutable afterwards. That is the right thing for the reconciler to do, and it leaves a gap above it: an
// edit to a template-derived field is accepted by the apiserver, survives a reconcile that reports no error,
// and changes nothing. The user is told the write succeeded, which it did — the API stored it — while the
// thing they were trying to change is untouched.
//
// A silent no-op is worse than a rejection here. These specs exist to hold that gap still and describe it
// precisely, so a validating webhook can be built against measured behaviour rather than an assumption
// about it.
var _ = Describe("MLTrainingJob fields the controller cannot re-apply", func() {
	const ns = "default"

	var key types.NamespacedName

	// createAndSettle creates an MLTrainingJob and drives reconciles until the owned Job exists.
	createAndSettle := func(image string) *batchv1.Job {
		GinkgoHelper()
		key = types.NamespacedName{Name: "immutable-" + rand.String(5), Namespace: ns}
		Expect(k8sClient.Create(ctx, &platformv1.MLTrainingJob{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: platformv1.MLTrainingJobSpec{
				Queue: "team-a", Image: image, GPUCount: 1, Parallelism: 1, Completions: 1,
			},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &platformv1.MLTrainingJob{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			})
		})

		r := &MLTrainingJobReconciler{Client: cachedClient, Scheme: cachedClient.Scheme()}
		for range 3 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		return job
	}

	It("accepts an image edit after the Job exists and then ignores it", func() {
		job := createAndSettle("trainer:v1")
		Expect(job.Spec.Template.Spec.Containers).NotTo(BeEmpty())
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("trainer:v1"))

		By("editing the image on the MLTrainingJob")
		var mltj platformv1.MLTrainingJob
		Expect(k8sClient.Get(ctx, key, &mltj)).To(Succeed())
		mltj.Spec.Image = "trainer:v2"
		// The apiserver stores it. Nothing at this layer objects, which is the gap.
		Expect(k8sClient.Update(ctx, &mltj)).To(Succeed())

		By("reconciling, which reports success")
		r := &MLTrainingJobReconciler{Client: cachedClient, Scheme: cachedClient.Scheme()}
		for range 3 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		By("finding the Job still running the original image")
		updated := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Spec.Template.Spec.Containers[0].Image).To(Equal("trainer:v1"),
			"the reconcile succeeded and the stored spec says v2, so nothing anywhere reports the divergence")

		By("confirming the stored spec and the running Job now disagree")
		var stored platformv1.MLTrainingJob
		Expect(k8sClient.Get(ctx, key, &stored)).To(Succeed())
		Expect(stored.Spec.Image).To(Equal("trainer:v2"))
	})

	It("accepts a blank image, which +required does not prevent", func() {
		// +required means the field is PRESENT, not that it is non-empty, so the CRD schema admits "".
		// The Job is then built around a container with no image and can never run.
		name := "blank-" + rand.String(5)
		err := k8sClient.Create(ctx, &platformv1.MLTrainingJob{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: platformv1.MLTrainingJobSpec{
				Queue: "team-a", Image: "", GPUCount: 1, Parallelism: 1, Completions: 1,
			},
		})
		Expect(err).NotTo(HaveOccurred(), "if this ever fails, the schema gained a MinLength and the webhook's create rule is redundant")
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &platformv1.MLTrainingJob{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			})
		})
	})

	It("accepts a blank queue, which routes the workload to a LocalQueue that cannot exist", func() {
		name := "noqueue-" + rand.String(5)
		err := k8sClient.Create(ctx, &platformv1.MLTrainingJob{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: platformv1.MLTrainingJobSpec{
				Queue: "", Image: "trainer:v1", GPUCount: 1, Parallelism: 1, Completions: 1,
			},
		})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("schema rejected a blank queue for %s; the webhook's create rule would be redundant", name))
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &platformv1.MLTrainingJob{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			})
		})
	})
})
