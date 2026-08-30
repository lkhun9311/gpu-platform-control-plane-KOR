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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

var _ = Describe("InferenceDeployment Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "llama3-8b"
		const ns = "default"

		ctx := context.Background()
		key := types.NamespacedName{Name: resourceName, Namespace: ns}

		reconciler := func() *InferenceDeploymentReconciler {
			return &InferenceDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		newInfD := func(replicas, gpu int32) *platformv1.InferenceDeployment {
			return &platformv1.InferenceDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ns},
				Spec: platformv1.InferenceDeploymentSpec{
					Model:    platformv1.InferenceModel{Name: "llama3-8b", StorageURI: "s3://models/llama3-8b/v1"},
					Image:    "vllm/vllm-openai:test",
					GPUClass: "a10g",
					GPUCount: gpu,
					Replicas: replicas,
					Port:     8080,
				},
			}
		}

		AfterEach(func() {
			infd := &platformv1.InferenceDeployment{}
			if err := k8sClient.Get(ctx, key, infd); err == nil {
				Expect(k8sClient.Delete(ctx, infd)).To(Succeed())
			}
			for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
				_ = k8sClient.DeleteAllOf(ctx, obj, client.InNamespace(ns))
			}
		})

		It("creates an owned Deployment and Service with the model spec", func() {
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
			Expect(dep.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", resourceName))
			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.Image).To(Equal("vllm/vllm-openai:test"))
			Expect(c.Ports[0].Name).To(Equal("http"))
			Expect(c.Ports[0].ContainerPort).To(Equal(int32(8080)))
			gpuReq := c.Resources.Requests[nvidiaGPUResource]
			gpuLim := c.Resources.Limits[nvidiaGPUResource]
			Expect(gpuReq.Value()).To(Equal(int64(1)))
			Expect(gpuLim.Value()).To(Equal(int64(1)))
			Expect(metav1.IsControlledBy(dep, mustGet(ctx, key))).To(BeTrue())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/instance", resourceName))
			Expect(svc.Spec.Ports[0].Name).To(Equal("http"))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(metav1.IsControlledBy(svc, mustGet(ctx, key))).To(BeTrue())
		})

		It("omits the GPU resource when GPUCount is zero", func() {
			Expect(k8sClient.Create(ctx, newInfD(1, 0))).To(Succeed())
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			_, hasReq := dep.Spec.Template.Spec.Containers[0].Resources.Requests[nvidiaGPUResource]
			Expect(hasReq).To(BeFalse())
		})

		// markDeploymentObserved patches the Deployment status as the (absent) Deployment controller would.
		markDeploymentObserved := func(ready int32) {
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = ready
			dep.Status.UpdatedReplicas = ready
			dep.Status.ReadyReplicas = ready
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		}

		It("reports Progressing then Ready as the Deployment becomes ready", func() {
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Deployment status is stale (observedGeneration 0) -> Progressing.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Progressing"))

			markDeploymentObserved(2)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := mustGet(ctx, key)
			Expect(got.Status.Phase).To(Equal("Ready"))
			Expect(got.Status.ReadyReplicas).To(Equal(int32(2)))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		})

		It("reports Ready with ScaledToZero when replicas is zero", func() {
			Expect(k8sClient.Create(ctx, newInfD(0, 0))).To(Succeed())
			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := mustGet(ctx, key)
			Expect(got.Status.Phase).To(Equal("Ready"))
			cond := findCondition(got.Status.Conditions, "Available")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("ScaledToZero"))
		})

		It("reports Degraded when the Deployment exceeds its progress deadline", func() {
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Conditions = []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
			}}
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Degraded"))
		})

		It("refuses to adopt an unowned Deployment of the same name", func() {
			foreign := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ns, Labels: map[string]string{"owner": "someone-else"}},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"owner": "someone-else"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"owner": "someone-else"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())

			before := testutil.ToFloat64(inferenceDeploymentDegradedTotal.WithLabelValues(infdReasonConflict))

			res, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// The result is asserted, not discarded, because discarding it is what let a Degraded resource stay Degraded forever.
			//
			// The conflicting Deployment is not owned by this InferenceDeployment, so deleting it fires no Owns watch and schedules no reconcile; without a RequeueAfter nothing ever looks again and the resource never recovers on its own.
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Degraded"))
			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.OwnerReferences).To(BeEmpty())
			Expect(got.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox"))

			after := testutil.ToFloat64(inferenceDeploymentDegradedTotal.WithLabelValues(infdReasonConflict))
			Expect(after - before).To(Equal(1.0))
		})

		It("reports Progressing (not Ready) when Replicas=3 but desired is 2 (surplus not yet removed)", func() {
			// Create an InferenceDeployment with 2 desired replicas.
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Simulate a scale-down in flight: Deployment controller reports 3 total replicas (all updated, all ready) but the surplus old replica has not yet been removed.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 3
			dep.Status.UpdatedReplicas = 3
			dep.Status.ReadyReplicas = 3
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			// Must NOT report Ready: total replica count has not converged to desired=2.
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Progressing"))
		})

		It("does not prematurely report Ready when scale-down to zero is not yet observed by the Deployment", func() {
			// Start with 2 ready replicas.
			Expect(k8sClient.Create(ctx, newInfD(2, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			markDeploymentObserved(2)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Ready"))

			// Scale to zero in the InferenceDeployment spec.
			infd := mustGet(ctx, key)
			infd.Spec.Replicas = 0
			Expect(k8sClient.Update(ctx, infd)).To(Succeed())

			// Reconcile: the Deployment now has Replicas=0 in spec but the Deployment status still shows the old generation (ObservedGeneration < Generation).
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Stale gate fires before ScaledToZero: must not report Ready prematurely.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation - 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Progressing"))
		})

		It("is idempotent once steady", func() {
			// Create with 0 replicas so the reconciler reaches Ready on first pass without needing a manual Deployment status patch.
			Expect(k8sClient.Create(ctx, newInfD(0, 0))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			depBefore := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, depBefore)).To(Succeed())
			infdBefore := mustGet(ctx, key)

			// A second reconcile must not write any Deployment or InferenceDeployment update.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			depAfter := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, depAfter)).To(Succeed())
			Expect(depAfter.ResourceVersion).To(Equal(depBefore.ResourceVersion))
			Expect(mustGet(ctx, key).ResourceVersion).To(Equal(infdBefore.ResourceVersion))
		})

		It("restores a drifted Deployment image", func() {
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Tamper with the Deployment image directly to simulate drift.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Spec.Template.Spec.Containers[0].Image = "tampered:bad"
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			// The next reconcile must overwrite the drifted image.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			restored := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, restored)).To(Succeed())
			Expect(restored.Spec.Template.Spec.Containers[0].Image).To(Equal("vllm/vllm-openai:test"))
		})

		It("refuses to adopt an unowned Service of the same name", func() {
			foreignSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: ns, Labels: map[string]string{"owner": "someone-else"}},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"owner": "someone-else"},
					Ports:    []corev1.ServicePort{{Name: "foreign", Port: 9999}},
				},
			}
			Expect(k8sClient.Create(ctx, foreignSvc)).To(Succeed())
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// InferenceDeployment must be Degraded; we did not own the Service.
			Expect(mustGet(ctx, key).Status.Phase).To(Equal("Degraded"))

			// Service must not have been adopted or overwritten.
			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.OwnerReferences).To(BeEmpty())
			Expect(got.Spec.Selector).To(HaveKeyWithValue("owner", "someone-else"))
		})

		It("removes the GPU resource when GPUCount changes from 1 to 0", func() {
			// Create an InferenceDeployment with 1 GPU and verify the resource is set.
			Expect(k8sClient.Create(ctx, newInfD(1, 1))).To(Succeed())
			r := reconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].Resources.Requests).To(HaveKey(nvidiaGPUResource))
			Expect(dep.Spec.Template.Spec.Containers[0].Resources.Limits).To(HaveKey(nvidiaGPUResource))

			// Update GPUCount to 0 and reconcile; the GPU resource must be removed.
			infd := mustGet(ctx, key)
			infd.Spec.GPUCount = 0
			Expect(k8sClient.Update(ctx, infd)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			_, hasReq := updated.Spec.Template.Spec.Containers[0].Resources.Requests[nvidiaGPUResource]
			_, hasLim := updated.Spec.Template.Spec.Containers[0].Resources.Limits[nvidiaGPUResource]
			Expect(hasReq).To(BeFalse())
			Expect(hasLim).To(BeFalse())
		})
	})
})

func mustGet(ctx context.Context, key types.NamespacedName) *platformv1.InferenceDeployment {
	infd := &platformv1.InferenceDeployment{}
	Expect(k8sClient.Get(ctx, key, infd)).To(Succeed())
	return infd
}
