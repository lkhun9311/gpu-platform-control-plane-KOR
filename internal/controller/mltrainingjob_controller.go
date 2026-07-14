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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// MLTrainingJob object를 reconcile
type MLTrainingJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs/finalizers,verbs=update

// cluster의 현재 상태를 원하는 상태에 가깝게 옮기는 main Kubernetes reconciliation loop의 일부
// TODO(user): MLTrainingJob object가 지정한 상태와 실제 cluster 상태를 비교하고,
// 사용자가 지정한 상태를 cluster가 반영하도록 동작을 수행하게끔 Reconcile 함수를 수정할 것
//
// Reconcile과 그 Result에 대한 자세한 내용은 아래 참고
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *MLTrainingJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	// M1: 빈 reconciler, 요청만 log로 기록
	// batch/v1 Job 생성과 Kueue admission은 M5에서 다룸
	log.Info("Reconciling MLTrainingJob", "name", req.Name, "namespace", req.Namespace)

	return ctrl.Result{}, nil
}

// controller를 Manager에 등록
func (r *MLTrainingJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.MLTrainingJob{}).
		Named("mltrainingjob").
		Complete(r)
}
