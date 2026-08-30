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

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

var _ = Describe("NodeHealth Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-nodehealth"
		const nodeName = "test-node"

		ctx := context.Background()
		nhKey := types.NamespacedName{Name: resourceName}
		nodeKey := types.NamespacedName{Name: nodeName}

		reconciler := func() *NodeHealthReconciler {
			return &NodeHealthReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		// reconcileUntilSteady drives Reconcile a few times so the finalizer is added and
		// the status reaches its steady value.
		reconcileUntilSteady := func() {
			for range 3 {
				_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		makeNode := func(ready corev1.ConditionStatus) *corev1.Node {
			return &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: ready},
					},
				},
			}
		}

		BeforeEach(func() {
			By("creating a NodeHealth pointing at the target node")
			nh := &platformv1.NodeHealth{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec:       platformv1.NodeHealthSpec{NodeName: nodeName},
			}
			Expect(k8sClient.Create(ctx, nh)).To(Succeed())
		})

		AfterEach(func() {
			nh := &platformv1.NodeHealth{}
			if err := k8sClient.Get(ctx, nhKey, nh); err == nil {
				nh.Finalizers = nil
				Expect(k8sClient.Update(ctx, nh)).To(Succeed())
				Expect(k8sClient.Delete(ctx, nh)).To(Succeed())
			}
			node := &corev1.Node{}
			if err := k8sClient.Get(ctx, nodeKey, node); err == nil {
				Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			}
		})

		It("adds a finalizer and reports Ready for a ready node", func() {
			node := makeNode(corev1.ConditionTrue)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Finalizers).To(ContainElement(nodeHealthFinalizer))
			Expect(got.Status.Phase).To(Equal(phaseReady))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			cond := findCondition(got.Status.Conditions, conditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("is idempotent once steady", func() {
			node := makeNode(corev1.ConditionTrue)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			before := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, before)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			after := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		})

		It("recovers from manual status drift", func() {
			node := makeNode(corev1.ConditionTrue)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			drifted := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, drifted)).To(Succeed())
			drifted.Status.Phase = phaseQuarantine
			Expect(k8sClient.Status().Update(ctx, drifted)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phaseReady))
		})

		It("quarantines a not-ready node: taints it, sets phase and fault signal", func() {
			before := testutil.ToFloat64(nodeHealthTaintTotal.WithLabelValues("applied"))

			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phaseQuarantine))
			Expect(got.Status.FaultSignal).NotTo(BeNil())
			Expect(got.Status.FaultSignal.Source).To(Equal(faultSourceNodeNotReady))

			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(1))

			after := testutil.ToFloat64(nodeHealthTaintTotal.WithLabelValues("applied"))
			Expect(after - before).To(Equal(1.0))
		})

		It("removes the taint and clears the fault signal when the node recovers", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			before := testutil.ToFloat64(nodeHealthTaintTotal.WithLabelValues("removed"))

			recovered := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, recovered)).To(Succeed())
			recovered.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, recovered)).To(Succeed())
			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phaseReady))
			Expect(got.Status.FaultSignal).To(BeNil())

			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(0))

			after := testutil.ToFloat64(nodeHealthTaintTotal.WithLabelValues("removed"))
			Expect(after - before).To(Equal(1.0))
		})

		It("does not duplicate the taint or rewrite when already quarantined", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			nhBefore := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, nhBefore)).To(Succeed())
			nodeBefore := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, nodeBefore)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			nhAfter := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, nhAfter)).To(Succeed())
			nodeAfter := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, nodeAfter)).To(Succeed())

			Expect(unhealthyTaintCount(nodeAfter)).To(Equal(1))
			Expect(nhAfter.ResourceVersion).To(Equal(nhBefore.ResourceVersion))
			Expect(nodeAfter.ResourceVersion).To(Equal(nodeBefore.ResourceVersion))
		})

		It("preserves unrelated taints through quarantine and recovery", func() {
			node := makeNode(corev1.ConditionFalse)
			node.Spec.Taints = []corev1.Taint{{Key: "example.com/other", Value: "x", Effect: corev1.TaintEffectNoSchedule}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			hasOther := func() bool {
				n := &corev1.Node{}
				Expect(k8sClient.Get(ctx, nodeKey, n)).To(Succeed())
				for _, t := range n.Spec.Taints {
					if t.Key == "example.com/other" {
						return true
					}
				}
				return false
			}
			Expect(hasOther()).To(BeTrue())

			recovered := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, recovered)).To(Succeed())
			recovered.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, recovered)).To(Succeed())
			reconcileUntilSteady()

			Expect(hasOther()).To(BeTrue())
			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(0))
		})

		It("removes the taint on deletion (finalizer cleanup)", func() {
			node := makeNode(corev1.ConditionFalse)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			reconcileUntilSteady()

			tainted := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, tainted)).To(Succeed())
			Expect(unhealthyTaintCount(tainted)).To(Equal(1))

			toDelete := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(errors.IsNotFound(k8sClient.Get(ctx, nhKey, &platformv1.NodeHealth{}))).To(BeTrue())
			gotNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, gotNode)).To(Succeed())
			Expect(unhealthyTaintCount(gotNode)).To(Equal(0))
		})

		It("reports Pending when the target node is absent", func() {
			reconcileUntilSteady()

			got := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(phasePending))
		})

		It("removes the finalizer on deletion", func() {
			reconcileUntilSteady()

			toDelete := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nhKey})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, nhKey, &platformv1.NodeHealth{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("rejects a change to the immutable nodeName", func() {
			nh := &platformv1.NodeHealth{}
			Expect(k8sClient.Get(ctx, nhKey, nh)).To(Succeed())
			nh.Spec.NodeName = "some-other-node"
			err := k8sClient.Update(ctx, nh)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nodeName is immutable"))
		})
	})

	Context("mapNodeToNodeHealth", func() {
		ctx := context.Background()

		// newMapperClient builds a fake client with NodeNameIndex registered.
		//
		// The suite's envtest client cannot serve this lookup: it is uncached, so client.MatchingFields becomes a real field selector on the wire, and the apiserver supports no field selector over a CRD's spec fields.
		//
		// SetupWithManager installs the index on the manager's cache in production, so the fake client is given the same key and the same extractor to make MatchingFields resolve as it does there.
		newMapperClient := func(objs ...client.Object) client.Client {
			return fake.NewClientBuilder().
				WithScheme(k8sClient.Scheme()).
				WithIndex(&platformv1.NodeHealth{}, NodeNameIndex, indexNodeHealthByNodeName).
				WithObjects(objs...).
				Build()
		}

		It("returns requests only for NodeHealths matching the node name", func() {
			match := &platformv1.NodeHealth{
				ObjectMeta: metav1.ObjectMeta{Name: "map-match"},
				Spec:       platformv1.NodeHealthSpec{NodeName: "map-node"},
			}
			other := &platformv1.NodeHealth{
				ObjectMeta: metav1.ObjectMeta{Name: "map-other"},
				Spec:       platformv1.NodeHealthSpec{NodeName: "different-node"},
			}
			c := newMapperClient(match, other)
			r := &NodeHealthReconciler{Client: c, Scheme: c.Scheme()}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "map-node"}}
			reqs := r.mapNodeToNodeHealth(ctx, node)

			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Name).To(Equal("map-match"))
		})

		// Pins that a failed lookup is reported rather than dropped.
		//
		// The regression this prevents: a map function has no error return, so the failure has nowhere to go but the log.
		//
		// Returning a bare nil leaves node-side drift silently unpropagated, and an operator looking at a NodeHealth that stopped updating has nothing at all to go on.
		It("logs the node it could not map when the lookup fails", func() {
			// A recording logger installed on the context the mapper is given, so the assertion reads the same call the production logger would receive.
			var logged []string
			recorder := funcr.New(func(prefix, args string) {
				logged = append(logged, prefix+args)
			}, funcr.Options{})

			c := fake.NewClientBuilder().
				WithScheme(k8sClient.Scheme()).
				WithIndex(&platformv1.NodeHealth{}, NodeNameIndex, indexNodeHealthByNodeName).
				WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return fmt.Errorf("cache read failed")
					},
				}).
				Build()
			r := &NodeHealthReconciler{Client: c, Scheme: c.Scheme()}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unmappable-node"}}
			reqs := r.mapNodeToNodeHealth(logf.IntoContext(ctx, recorder), node)

			// Nothing to enqueue is still the right return; the point is that it is no longer the only thing that happens.
			Expect(reqs).To(BeEmpty())
			Expect(logged).To(HaveLen(1))
			// The node name is the one piece of context that makes the line actionable, so it is asserted rather than just the message.
			Expect(logged[0]).To(ContainSubstring("unmappable-node"))
			Expect(logged[0]).To(ContainSubstring("cache read failed"))
		})
	})
})

// unhealthyTaintCount counts taints carrying the platform unhealthy key.
func unhealthyTaintCount(node *corev1.Node) int {
	n := 0
	for _, t := range node.Spec.Taints {
		if t.Key == unhealthyTaintKey {
			n++
		}
	}
	return n
}

// findCondition returns a pointer to the condition of the given type, or nil.
func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
