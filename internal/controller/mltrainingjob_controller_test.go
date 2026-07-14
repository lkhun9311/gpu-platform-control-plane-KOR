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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

var _ = Describe("MLTrainingJob Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-training"
		const resourceNamespace = "default"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		mltrainingjob := &platformv1.MLTrainingJob{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind MLTrainingJob")
			err := k8sClient.Get(ctx, typeNamespacedName, mltrainingjob)
			if err != nil && errors.IsNotFound(err) { // 아직 없을 때만 새로 생성
				resource := &platformv1.MLTrainingJob{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: platformv1.MLTrainingJobSpec{
						Queue:       "team-vision-queue",
						Image:       "pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime",
						Command:     []string{"python", "train.py"},
						GPUClass:    "l40s",
						GPUCount:    2,
						Parallelism: 2,
						Completions: 2,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &platformv1.MLTrainingJob{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance MLTrainingJob")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should round-trip the spec and reconcile without error", func() {
			By("reading the created resource back")
			fetched := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fetched)).To(Succeed())
			Expect(fetched.Spec.Queue).To(Equal("team-vision-queue"))
			Expect(fetched.Spec.Image).To(Equal("pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime"))
			Expect(fetched.Spec.Command).To(Equal([]string{"python", "train.py"}))
			Expect(fetched.Spec.GPUCount).To(Equal(int32(2)))
			Expect(fetched.Spec.Parallelism).To(Equal(int32(2)))
			Expect(fetched.Spec.Completions).To(Equal(int32(2)))

			By("Reconciling the created resource")
			controllerReconciler := &MLTrainingJobReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should persist a status phase via the status subresource", func() {
			const phasePending = "Pending"

			fetched := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fetched)).To(Succeed())

			fetched.Status.Phase = phasePending
			Expect(k8sClient.Status().Update(ctx, fetched)).To(Succeed()) // status subresource로만 갱신

			updated := &platformv1.MLTrainingJob{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(phasePending))
		})
	})
})
