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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

var _ = Describe("GPUQuotaPolicy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-quota"
		const targetNS = "tenant-quota-ns"
		const gpuResource = corev1.ResourceName("requests.nvidia.com/gpu")

		ctx := context.Background()
		key := types.NamespacedName{Name: resourceName}
		rqKey := types.NamespacedName{Name: "gpuquota-" + resourceName, Namespace: targetNS}

		reconciler := func() *GPUQuotaPolicyReconciler {
			return &GPUQuotaPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		reconcileUntilSteady := func() {
			for range 3 {
				_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		BeforeEach(func() {
			By("ensuring the target namespace exists")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNS}}
			if err := k8sClient.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating a GPUQuotaPolicy")
			policy := &platformv1.GPUQuotaPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: platformv1.GPUQuotaPolicySpec{
					Tenant:          "team-vision",
					TargetNamespace: targetNS,
					GPUClass:        "l40s",
					Limits:          platformv1.GPUQuotaLimits{GPUCount: 8},
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		})

		AfterEach(func() {
			policy := &platformv1.GPUQuotaPolicy{}
			if err := k8sClient.Get(ctx, key, policy); err == nil {
				policy.Finalizers = nil
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
			}
			rq := &corev1.ResourceQuota{}
			if err := k8sClient.Get(ctx, rqKey, rq); err == nil {
				Expect(k8sClient.Delete(ctx, rq)).To(Succeed())
			}
		})

		It("syncs a ResourceQuota with the GPU ceiling and reports Synced", func() {
			reconcileUntilSteady()

			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())
			q := rq.Spec.Hard[gpuResource]
			Expect(q.Value()).To(Equal(int64(8)))

			got := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Finalizers).To(ContainElement(gpuQuotaFinalizer))
			Expect(got.Status.Phase).To(Equal(phaseSynced))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			cond := findCondition(got.Status.Conditions, conditionSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("labels the target namespace so the admission guards actually apply there", func() {
			reconcileUntilSteady()

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetNS}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue(platformv1.QuotaEnforcedLabel, platformv1.QuotaEnforcedValue))
		})

		It("does not disturb labels it did not write", func() {
			By("putting an unrelated label on the namespace first")
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetNS}, ns)).To(Succeed())
			patch := client.MergeFrom(ns.DeepCopy())
			if ns.Labels == nil {
				ns.Labels = map[string]string{}
			}
			ns.Labels["istio-injection"] = "enabled"
			Expect(k8sClient.Patch(ctx, ns, patch)).To(Succeed())

			reconcileUntilSteady()

			got := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetNS}, got)).To(Succeed())
			Expect(got.Labels).To(HaveKeyWithValue(platformv1.QuotaEnforcedLabel, platformv1.QuotaEnforcedValue))
			Expect(got.Labels).To(HaveKeyWithValue("istio-injection", "enabled"))
		})

		// Turning enforcement OFF is the operation that re-opens a bypass, so deletion must not do it. A
		// policy deleted by mistake, a finalizer race or a stale cache would each otherwise unguard a
		// namespace that still holds running tenants.
		It("leaves the namespace guarded after the policy is deleted", func() {
			reconcileUntilSteady()

			policy := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
			Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
			reconcileUntilSteady()

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetNS}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue(platformv1.QuotaEnforcedLabel, platformv1.QuotaEnforcedValue),
				"deleting a policy unguarded a namespace, which is the one direction that re-opens the bypass")
		})

		// A ceiling synced into an unguarded namespace is a ceiling a bare Pod walks past, so the reconcile
		// refuses to publish Synced when it could not turn the guards on.
		It("refuses to sync a ceiling into a namespace it cannot mark enforced", func() {
			missing := &platformv1.GPUQuotaPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "quota-for-absent-ns"},
				Spec: platformv1.GPUQuotaPolicySpec{
					Tenant:          "team-ghost",
					TargetNamespace: "no-such-namespace",
					GPUClass:        "l40s",
					Limits:          platformv1.GPUQuotaLimits{GPUCount: 4},
				},
			}
			Expect(k8sClient.Create(ctx, missing)).To(Succeed())
			defer func() {
				got := &platformv1.GPUQuotaPolicy{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: missing.Name}, got); err == nil {
					got.Finalizers = nil
					Expect(k8sClient.Update(ctx, got)).To(Succeed())
					Expect(k8sClient.Delete(ctx, got)).To(Succeed())
				}
			}()

			mkey := types.NamespacedName{Name: missing.Name}
			for range 3 {
				_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: mkey})
				Expect(err).NotTo(HaveOccurred())
			}

			got := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, mkey, got)).To(Succeed())
			Expect(got.Status.Phase).NotTo(Equal(phaseSynced),
				"a policy reported Synced while the namespace it governs was never guarded")
		})

		It("is idempotent once steady", func() {
			reconcileUntilSteady()

			before := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
			rqBefore := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rqBefore)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			after := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			rqAfter := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rqAfter)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(rqAfter.ResourceVersion).To(Equal(rqBefore.ResourceVersion))
		})

		It("recreates the ResourceQuota after it is deleted (drift recovery)", func() {
			reconcileUntilSteady()

			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())
			Expect(k8sClient.Delete(ctx, rq)).To(Succeed())

			reconcileUntilSteady()

			restored := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, restored)).To(Succeed())
			q := restored.Spec.Hard[gpuResource]
			Expect(q.Value()).To(Equal(int64(8)))
		})

		It("corrects a mutated ResourceQuota hard limit", func() {
			reconcileUntilSteady()

			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())
			rq.Spec.Hard[gpuResource] = *resource.NewQuantity(99, resource.DecimalSI)
			Expect(k8sClient.Update(ctx, rq)).To(Succeed())

			// Capture the before-value rather than assuming zero, because the counter is process-global and other specs may have already incremented it.
			before := testutil.ToFloat64(gpuQuotaPolicyDriftCorrectedTotal)

			reconcileUntilSteady()

			corrected := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, corrected)).To(Succeed())
			q := corrected.Spec.Hard[gpuResource]
			Expect(q.Value()).To(Equal(int64(8)))

			after := testutil.ToFloat64(gpuQuotaPolicyDriftCorrectedTotal)
			Expect(after - before).To(Equal(1.0))
		})

		It("deletes the ResourceQuota and clears the finalizer on deletion", func() {
			reconcileUntilSteady()

			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, rq)).To(Succeed())

			toDelete := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			reconcileUntilSteady()

			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &platformv1.GPUQuotaPolicy{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, rqKey, &corev1.ResourceQuota{}))).To(BeTrue())
		})

		It("refuses to overwrite a ResourceQuota it does not own and reports Degraded", func() {
			By("pre-creating an unowned ResourceQuota occupying the policy's target name")
			foreign := &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{Name: rqKey.Name, Namespace: rqKey.Namespace},
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					gpuResource: *resource.NewQuantity(3, resource.DecimalSI),
				}},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			reconcileUntilSteady()

			By("leaving the foreign ResourceQuota untouched (not hijacked)")
			got := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, rqKey, got)).To(Succeed())
			q := got.Spec.Hard[gpuResource]
			Expect(q.Value()).To(Equal(int64(3)))
			Expect(got.OwnerReferences).To(BeEmpty())

			By("reporting Degraded with Synced=False on the policy")
			policy := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
			Expect(policy.Status.Phase).To(Equal(phaseDegraded))
			cond := findCondition(policy.Status.Conditions, conditionSynced)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reasonQuotaConflict))
		})

		It("rejects a change to the immutable targetNamespace", func() {
			policy := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
			policy.Spec.TargetNamespace = "some-other-ns"
			err := k8sClient.Update(ctx, policy)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("targetNamespace is immutable"))
		})

		It("round-trips the optional rateLimit", func() {
			p := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
			p.Spec.RateLimit = &platformv1.GPUQuotaRateLimit{RequestsPerMinute: 600, Burst: 100}
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			got := &platformv1.GPUQuotaPolicy{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.RateLimit.RequestsPerMinute).To(Equal(int32(600)))
			Expect(got.Spec.RateLimit.Burst).To(Equal(int32(100)))
		})
	})
})
