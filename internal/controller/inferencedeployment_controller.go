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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// 서빙 pod가 GPU 하나당 요청하는 node 확장 resource로,
// pod 수준 resource이며 GPUQuotaPolicy의 ResourceQuota 키와는 별개다.
const nvidiaGPUResource = corev1.ResourceName("nvidia.com/gpu")

const (
	// 하나의 InferenceDeployment가 소유한 pod를 선택하는 label로,
	// Deployment의 selector는 생성 후 불변이라 한 번 설정하고 이후 변경하지 않는다.
	instanceLabel = "app.kubernetes.io/instance"
)

// InferenceDeployment 객체를 조정하는 reconciler
type InferenceDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch

// InferenceDeployment로부터 Deployment와 Service를 동기화하고 준비 상태를 status에 반영한다.
//
// 재조정 흐름:
//  1. 같은 이름의 Deployment가 남의 소유면 덮어쓰지 않고 Degraded로 보고한다,
//  2. CreateOrUpdate로 소유 Deployment를 원하는 spec(replica·image·GPU·probe)에 맞추고 owner 참조를 걸어 GC와 연동한다,
//  3. Service도 같은 방식으로 소유권을 확인한 뒤 ClusterIP Service를 동기화한다,
//  4. Deployment status로부터 phase(Pending/Progressing/Ready/Degraded)와 Available condition을 도출한다,
//  5. status가 실제로 바뀐 경우에만 갱신한다.
func (r *InferenceDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var infd platformv1.InferenceDeployment
	if err := r.Get(ctx, req.NamespacedName, &infd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err) // 이미 삭제된 경우는 오류로 다루지 않음
	}

	// 같은 이름의 Deployment가 남의 소유면 덮어쓰지 않음
	if conflict, err := r.ownedConflict(ctx, &infd, &appsv1.Deployment{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("check deployment ownership %s/%s: %w", infd.Namespace, infd.Name, err)
	} else if conflict {
		log.Info("Deployment exists and is not owned by this InferenceDeployment; refusing to adopt", "name", infd.Name)
		return r.markDegraded(ctx, &infd, infdReasonConflict, "a Deployment of the same name is not owned by this InferenceDeployment")
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: infd.Name, Namespace: infd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		r.mutateDeployment(&infd, dep)
		return controllerutil.SetControllerReference(&infd, dep, r.Scheme) // owner 참조 설정으로 소유권 표시 및 GC 연동
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync deployment %s/%s: %w", infd.Namespace, infd.Name, err)
	}

	// Service도 마찬가지로 소유권 충돌 확인
	if conflict, err := r.ownedConflict(ctx, &infd, &corev1.Service{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("check service ownership %s/%s: %w", infd.Namespace, infd.Name, err)
	} else if conflict {
		log.Info("Service exists and is not owned by this InferenceDeployment; refusing to adopt", "name", infd.Name)
		return r.markDegraded(ctx, &infd, infdReasonServiceConflict, "a Service of the same name is not owned by this InferenceDeployment")
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: infd.Name, Namespace: infd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		r.mutateService(&infd, svc)
		return controllerutil.SetControllerReference(&infd, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync service %s/%s: %w", infd.Namespace, infd.Name, err)
	}

	log.Info("Synced serving objects", "inferenceDeployment", req.String())

	phase, cond := computeInfDPhase(&infd, dep)
	desired := infd.Status.DeepCopy()
	desired.Phase = phase
	desired.ReadyReplicas = dep.Status.ReadyReplicas
	desired.ObservedGeneration = infd.Generation
	meta.SetStatusCondition(&desired.Conditions, cond)

	if !equality.Semantic.DeepEqual(infd.Status, *desired) { // 실제 바뀐 게 있을 때만 status 갱신
		infd.Status = *desired
		if err := r.Status().Update(ctx, &infd); err != nil {
			return ctrl.Result{}, fmt.Errorf("update inferencedeployment status %s/%s: %w", infd.Namespace, infd.Name, err)
		}
		log.Info("Updated InferenceDeployment status", "name", infd.Name, "phase", phase)
	}
	return ctrl.Result{}, nil
}

// 설정된 서빙 port 반환, 미설정 시 8080 기본값
func servingPort(infd *platformv1.InferenceDeployment) int32 {
	if infd.Spec.Port == 0 {
		return 8080
	}
	return infd.Spec.Port
}

// 소유한 Deployment와 Service에 붙이는 권장 label set
func infdLabels(infd *platformv1.InferenceDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":              "inferencedeployment",
		instanceLabel:                         infd.Name,
		"app.kubernetes.io/managed-by":        "gpu-platform-control-plane",
		"platform.lkhun9311.github.io/tenant": infd.Namespace,
	}
}

// 이 controller가 관리하는 field만 Deployment에 설정하고,
// selector는 생성 후 불변이라 한 번만 설정하고 이후 건드리지 않는다.
func (r *InferenceDeploymentReconciler) mutateDeployment(infd *platformv1.InferenceDeployment, dep *appsv1.Deployment) {
	labels := infdLabels(infd)
	port := servingPort(infd)

	dep.Labels = labels
	dep.Spec.Replicas = ptr.To(infd.Spec.Replicas)
	dep.Spec.ProgressDeadlineSeconds = ptr.To(int32(600)) // rollout 실패 판정 시한 600초
	if dep.Spec.Selector == nil {                         // 최초 생성 때만 selector 지정 (불변 field)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: infd.Name}}
	}
	dep.Spec.Template.Labels = labels

	container := corev1.Container{
		Name:  "server",
		Image: infd.Spec.Image,
		Args:  []string{"--model", infd.Spec.Model.Name, "--model-path", infd.Spec.Model.StorageURI},
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		}},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		}},
	}
	if infd.Spec.GPUCount > 0 { // GPU를 요청한 경우에만 requests/limits 지정
		q := *resource.NewQuantity(int64(infd.Spec.GPUCount), resource.DecimalSI)
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{nvidiaGPUResource: q},
			Limits:   corev1.ResourceList{nvidiaGPUResource: q}, // GPU는 requests와 limits를 같게 둠
		}
	}
	dep.Spec.Template.Spec.Containers = []corev1.Container{container}
}

// 이 controller가 관리하는 field만 Service에 설정
func (r *InferenceDeploymentReconciler) mutateService(infd *platformv1.InferenceDeployment, svc *corev1.Service) {
	port := servingPort(infd)
	svc.Labels = infdLabels(infd)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.Selector = map[string]string{instanceLabel: infd.Name}
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       "http",
		Port:       port,
		TargetPort: intstr.FromString("http"),
	}}
}

const (
	infdPhasePending     = "Pending"
	infdPhaseProgressing = "Progressing"
	infdPhaseReady       = "Ready"
	infdPhaseDegraded    = "Degraded"

	infdCondAvailable         = "Available"
	infdReasonScaledZero      = "ScaledToZero"
	infdReasonRollout         = "RolloutInProgress"
	infdReasonAvailable       = "MinimumReplicasAvailable"
	infdReasonConflict        = "DeploymentConflict"
	infdReasonServiceConflict = "ServiceConflict"
)

// Deployment status로부터 phase와 Available condition 도출
// 우선순위 (위에서 아래로)
//  1. stale gate, spec.Replicas > 0 이고 ObservedGeneration < Generation 이면 Progressing으로 두어,
//     Deployment가 아직 관측하지 못한 0으로의 scale-down이 성급하게 Ready로 보고되지 않도록 한다.
//  2. ScaledToZero, Replicas == 0 이면 Ready (0 replica는 spec만으로 항상 확정)
//  3. Degraded, Deployment의 Progressing condition이 ProgressDeadlineExceeded로 False
//  4. Pending, ReadyReplicas == 0
//  5. Progressing, UpdatedReplicas나 ReadyReplicas가 spec.Replicas 미만이거나 옛 replica가 아직 안 빠짐
//  6. Ready, 완전히 수렴
func computeInfDPhase(infd *platformv1.InferenceDeployment, dep *appsv1.Deployment) (string, metav1.Condition) {
	avail := func(status metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: infdCondAvailable, Status: status, Reason: reason, Message: msg, ObservedGeneration: infd.Generation}
	}
	// 1. stale gate, Deployment controller가 현재 spec을 관측할 때까지 phase 판단을 보류하고,
	// spec이 0 replica를 원하고 status도 이미 0개를 보이는 경우에만 gate를 생략하는데,
	// 빠질 replica가 없고 spec 의도가 확정적이기 때문이다.
	staleDep := dep.Status.ObservedGeneration < dep.Generation
	zeroAndDrained := infd.Spec.Replicas == 0 && dep.Status.Replicas == 0
	if staleDep && !zeroAndDrained {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "deployment not yet observed")
	}
	// 2. ScaledToZero, 의도적인 0 replica는 status가 실행 중 replica 0을 보이면 Ready
	if infd.Spec.Replicas == 0 {
		return infdPhaseReady, avail(metav1.ConditionTrue, infdReasonScaledZero, "scaled to zero replicas")
	}
	// 3. Degraded, ProgressDeadlineExceeded condition은 rollout이 스스로 완료되지 못함을 의미
	for i := range dep.Status.Conditions {
		c := dep.Status.Conditions[i]
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return infdPhaseDegraded, avail(metav1.ConditionFalse, "ProgressDeadlineExceeded", "deployment rollout failed")
		}
	}
	// 4. Pending, 아직 준비된 replica 없음
	if dep.Status.ReadyReplicas == 0 {
		return infdPhasePending, avail(metav1.ConditionFalse, infdReasonRollout, "no replicas ready yet")
	}
	// 5. Progressing, replica가 아직 완전히 갱신/준비되지 않았거나 옛 replica가 안 빠짐
	if dep.Status.UpdatedReplicas < infd.Spec.Replicas || dep.Status.ReadyReplicas < infd.Spec.Replicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "rollout in progress")
	}
	if dep.Status.Replicas != dep.Status.UpdatedReplicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "waiting for old replicas to drain")
	}
	// 6. Ready, scale-down 도중 남은 잉여 replica가 아직 제거되지 않은 상황을 막기 위해,
	// 세 replica count가 모두 원하는 값과 같아야 한다.
	if dep.Status.Replicas != infd.Spec.Replicas || dep.Status.UpdatedReplicas != infd.Spec.Replicas || dep.Status.ReadyReplicas != infd.Spec.Replicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "waiting for replica count to converge")
	}
	// 7. Ready, 완전히 수렴
	return infdPhaseReady, avail(metav1.ConditionTrue, infdReasonAvailable, "all replicas ready")
}

// 결정적 실패를 Available=False의 Degraded로 status에 반영하고,
// DeploymentConflict가 낡은 ready count를 남기지 않도록 ReadyReplicas를 0으로 초기화한다.
func (r *InferenceDeploymentReconciler) markDegraded(ctx context.Context, infd *platformv1.InferenceDeployment, reason, msg string) (ctrl.Result, error) {
	desired := infd.Status.DeepCopy()
	desired.Phase = infdPhaseDegraded
	desired.ReadyReplicas = 0
	desired.ObservedGeneration = infd.Generation
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: infdCondAvailable, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: infd.Generation,
	})
	if !equality.Semantic.DeepEqual(infd.Status, *desired) {
		infd.Status = *desired
		if err := r.Status().Update(ctx, infd); err != nil {
			return ctrl.Result{}, fmt.Errorf("update inferencedeployment status %s/%s to Degraded: %w", infd.Namespace, infd.Name, err)
		}
	}
	return ctrl.Result{}, nil
}

// 주어진 이름의 객체가 존재하지만 infd가 소유하지 않는지 여부 반환
func (r *InferenceDeploymentReconciler) ownedConflict(ctx context.Context, infd *platformv1.InferenceDeployment, obj client.Object) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Name: infd.Name, Namespace: infd.Namespace}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil // 없으면 충돌 아님
	case err != nil:
		return false, err
	default:
		return !metav1.IsControlledBy(obj, infd), nil // 존재하나 controller owner가 아니면 충돌
	}
}

// controller를 Manager에 등록
func (r *InferenceDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.InferenceDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("inferencedeployment").
		Complete(r)
}
