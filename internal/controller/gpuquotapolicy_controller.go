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

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

const (
	// 삭제 전에 정리할 게 있다는 표식,
	// 이 표식이 붙어 있으면 Kubernetes는 object를 실제로 지우지 않고 기다리고
	// reconciler가 동기화해 둔 ResourceQuota를 먼저 지운 뒤 표식을 떼어야 실제 삭제가 진행된다.
	// envtest에는 garbage collection이 없어 정리를 코드로 직접 해야 한다.
	gpuQuotaFinalizer = "gpuquotapolicy.platform.lkhun9311.github.io/finalizer"

	// ResourceQuota가 정책과 맞는지 여부를 보고하는 condition
	conditionSynced     = "Synced"
	reasonQuotaSynced   = "QuotaSynced"
	reasonQuotaConflict = "QuotaConflict"

	// 상위 수준 진행 상태를 한 단어로 요약한 phase,
	// ResourceQuota가 정책 상한과 일치하면 Synced이고 결정적 실패(남의 ResourceQuota와 이름 충돌 등)면 Degraded이며,
	// 일시적 API 오류는 phase에 반영하지 않고 requeue만 하므로 재시도해도 phase가 흔들리지 않고,
	// 이 phase는 이 controller만 소유하므로 NodeHealth controller는 자기 phase를 독립적으로 정할 수 있다.
	phaseSynced   = "Synced"
	phaseDegraded = "Degraded"

	// GPU 소비를 제한하는 ResourceQuota key,
	// 확장 resource(nvidia.com/gpu)는 quota에서 requests.<resource> 형태의 key로 추적하며,
	// local에서는 시뮬레이션한 nvidia.com/gpu 용량을 이 key로 제한한다.
	gpuRequestsResource = corev1.ResourceName("requests.nvidia.com/gpu")
)

// GPUQuotaPolicy를 관찰해 실제 ResourceQuota를 원하는 상한으로 맞춰 가는 controller
type GPUQuotaPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// 아래 marker는 코드 생성기(controller-gen)가 읽어 RBAC 권한을 자동 생성하므로 문구를 바꾸지 말 것
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete

// 정책이 바뀔 때마다 불려 대상 namespace의 ResourceQuota를 원하는 상한으로 맞추고,
// 누가 손대서 어긋나면 다시 맞추며 정책이 지워지면 ResourceQuota도 지우고,
// 몇 번을 다시 불려도 같은 결과가 나오도록 멱등하게 설계한다.
//
// 재조정 흐름:
//  1. 삭제 중이면 동기화해 둔 ResourceQuota를 지운 뒤 finalizer를 떼어 실제 삭제가 진행되게 한다,
//  2. finalizer가 없으면 붙이고 이번 pass를 끝낸다,
//  3. 정책의 GPUCount로 원하는 상한(requests.nvidia.com/gpu)을 계산한다,
//  4. ResourceQuota가 없으면 만들고, 남의 소유면 Degraded로 보고하며, drift가 있으면 원하는 값으로 되돌린다,
//  5. Synced phase와 condition을 status에 멱등하게 기록한다.
func (r *GPUQuotaPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 이번에 바뀐 정책을 읽고,
	// 이미 삭제됐으면 IgnoreNotFound가 조용히 끝내준다.
	var policy platformv1.GPUQuotaPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 이 정책에 대응하는 ResourceQuota의 위치를 미리 계산
	rqKey := types.NamespacedName{Name: quotaName(policy.Name), Namespace: policy.Spec.TargetNamespace}

	// 삭제 중이면 우리가 만든 ResourceQuota부터 지운 뒤 표식을 떼어 실제 삭제가 진행되게 함
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&policy, gpuQuotaFinalizer) {
			var rq corev1.ResourceQuota
			switch err := r.Get(ctx, rqKey, &rq); {
			case err == nil:
				// 이미 없으면 성공으로 보고 그 밖의 삭제 오류만 실패로 취급
				if err := r.Delete(ctx, &rq); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
				log.Info("Deleted synced ResourceQuota on deletion", "resourceQuota", rqKey.String())
			case apierrors.IsNotFound(err):
				// 이미 사라져 지울 게 없음
			default:
				return ctrl.Result{}, err
			}
			// 정리가 끝났으니 표식을 떼어 저장
			controllerutil.RemoveFinalizer(&policy, gpuQuotaFinalizer)
			if err := r.Update(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 표식이 없으면 먼저 붙이고 이번 pass는 종료하며,
	// 소유 resource를 만들기 전에 정리 약속을 먼저 걸어야 중간에 삭제돼도 정리가 보장된다.
	if !controllerutil.ContainsFinalizer(&policy, gpuQuotaFinalizer) {
		controllerutil.AddFinalizer(&policy, gpuQuotaFinalizer)
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 원하는 상한 계산은 정책의 GPUCount만큼을 requests.nvidia.com/gpu로 제한하며,
	// NOTE spec.gpuClass는 아직 class별로 시행하지 않고,
	// 이 milestone은 class와 무관하게 단일 key(requests.nvidia.com/gpu)만 제한하므로,
	// 같은 namespace를 겨냥한 gpuClass가 다른 두 정책이 같은 key를 제한하면,
	// Kubernetes가 quota를 AND로 합쳐 가장 엄격한 쪽이 이기고,
	// class별 key 분리는 시뮬레이션 용량 modeling 방식에 달려 보류하며 gpuClass는 기록만 하고 quota 범위는 안 정한다.
	desiredHard := corev1.ResourceList{
		gpuRequestsResource: *resource.NewQuantity(int64(policy.Spec.Limits.GPUCount), resource.DecimalSI),
	}

	var rq corev1.ResourceQuota
	switch err := r.Get(ctx, rqKey, &rq); {
	case apierrors.IsNotFound(err):
		// 없으면 새로 만듦
		rq = corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: rqKey.Name, Namespace: rqKey.Namespace},
			Spec:       corev1.ResourceQuotaSpec{Hard: desiredHard},
		}
		// 소유 참조를 걸어 정책이 지워질 때 이 ResourceQuota도 함께 정리되게 함
		if err := controllerutil.SetControllerReference(&policy, &rq, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &rq); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// 경쟁에서 밀림(동시 reconcile이나 informer 지연),
				// 이제는 객체가 있으므로 실패시키지 않고 1초 뒤 다시 reconcile해 정상 경로로 흡수한다.
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		log.Info("Created ResourceQuota", "resourceQuota", rqKey.String())
	case err != nil:
		return ctrl.Result{}, err
	default:
		// 이미 있으면 이 정책이 소유한 게 맞는지부터 확인하고,
		// 우리가 안 만든 남의 ResourceQuota를 덮어쓰면 남의 quota를 훼손하므로 가로채지 않고 Degraded로 보고한다.
		if !metav1.IsControlledBy(&rq, &policy) {
			log.Info("ResourceQuota exists but is not owned by this policy; refusing to overwrite",
				"resourceQuota", rqKey.String())
			return r.markDegraded(ctx, &policy, reasonQuotaConflict,
				fmt.Sprintf("ResourceQuota %s already exists and is not owned by this policy", rqKey.String()))
		}
		// 소유가 맞고 상한이 어긋났으면(drift) 원하는 값으로 되돌림
		if !equality.Semantic.DeepEqual(rq.Spec.Hard, desiredHard) {
			rq.Spec.Hard = desiredHard
			if err := r.Update(ctx, &rq); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Corrected ResourceQuota drift", "resourceQuota", rqKey.String())
		}
	}

	// 동기화 성공을 status에 멱등하게 기록하되,
	// 복사본에 원하는 값을 채운 뒤 실제로 달라졌을 때만 저장한다.
	desired := policy.Status.DeepCopy()
	desired.ObservedGeneration = policy.Generation
	setQuotaPhase(desired, phaseSynced)
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               conditionSynced,
		Status:             metav1.ConditionTrue,
		Reason:             reasonQuotaSynced,
		Message:            "ResourceQuota synced from policy",
		ObservedGeneration: policy.Generation,
	})

	// 바뀐 게 있을 때만 저장하며 매번 쓰면 불필요한 event와 충돌이 늘어남
	if !equality.Semantic.DeepEqual(policy.Status, *desired) {
		policy.Status = *desired
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Updated GPUQuotaPolicy status", "name", policy.Name, "phase", desired.Phase)
	}

	return ctrl.Result{}, nil
}

// 정책 이름으로 ResourceQuota 이름을 항상 같은 규칙으로 만들며,
// 같은 정책은 늘 같은 이름으로 mapping돼 중복 생성이나 추적 혼선을 막는다.
func quotaName(policyName string) string {
	return "gpuquota-" + policyName
}

// 결정적 실패를 Synced=False condition과 Degraded phase로 status에 기록하고,
// 막힌 조건이 풀리면 스스로 복구되도록 RequeueAfter로 재확인을 예약하며,
// Owns watch는 우리가 소유하지 않은 ResourceQuota 변화에는 안 울리므로 시간 기반 재확인이 필요하다.
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
	// 1분 뒤 다시 reconcile 예약
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// phase를 바꾸되 값이 실제로 달라질 때만 전환 시각을 갱신하며,
// 같은 phase를 다시 써도 전환 시각이 튀지 않게 하려는 것이다.
func setQuotaPhase(status *platformv1.GPUQuotaPolicyStatus, phase string) {
	if status.Phase == phase {
		return
	}
	status.Phase = phase
	now := metav1.Now()
	status.LastTransitionTime = &now
}

// 이 controller를 Manager에 등록해 무엇을 지켜보고 무엇을 소유하는지 알려주며,
// For는 GPUQuotaPolicy가 바뀌면 reconcile하고 Owns는 우리가 만든 ResourceQuota가 바뀌어도 reconcile한다.
func (r *GPUQuotaPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.GPUQuotaPolicy{}).
		Owns(&corev1.ResourceQuota{}).
		Named("gpuquotapolicy").
		Complete(r)
}
