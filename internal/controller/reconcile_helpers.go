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
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// NodeHealth 정리를 지키는 finalizer,
// 삭제 시 reconciler가 자신이 소유한 unhealthy taint를 먼저 걷어낸 뒤 이 finalizer를 제거한다.
const nodeHealthFinalizer = "nodehealth.platform.lkhun9311.github.io/finalizer"

// not-ready node를 격리하려고 reconciler가 붙이는 taint의 key/Value/Effect,
// scheduler가 해당 node에 GPU workload를 더 얹지 않도록 하며,
// reconciler는 오직 이 taint만 관리한다.
const (
	unhealthyTaintKey   = "platform.lkhun9311.github.io/unhealthy"
	unhealthyTaintValue = "true"
)

// node가 not-ready라 격리되는 동안 기록되는 faultSignal의 source,
// 이는 readiness에서 파생된 신호이지 실제 hardware 결함 신호는 아니다.
const faultSourceNodeNotReady = "node-not-ready"

// 대상 node readiness를 그대로 반영하는 NodeHealth condition type
const conditionReady = "Ready"

// conditionReady용 condition reason들
const (
	reasonNodeReady    = "NodeReady"
	reasonNodeNotReady = "NodeNotReady"
	reasonNodeNotFound = "NodeNotFound"
)

// M3에서 방출되는 NodeHealth phase들,
// M3는 readiness를 Pending(node 없음), Ready(node 준비됨), Quarantine(node not-ready라 taint 부착)으로 몰아가며,
// CRD enum의 Intake, Degraded phase는 이후 lifecycle 단계용 예약(docs/03 참고)이라 여기서는 방출 안 한다.
const (
	phasePending    = "Pending"
	phaseReady      = "Ready"
	phaseQuarantine = "Quarantine"
)

// phase가 실제로 바뀔 때만 phase를 갱신하고 lastTransitionTime을 올림
func setPhase(status *platformv1.NodeHealthStatus, phase string) {
	if status.Phase == phase { // 동일 phase면 시각 갱신 없이 조기 반환
		return
	}
	status.Phase = phase
	now := metav1.Now()
	status.LastTransitionTime = &now
}

// Ready condition을 설정하며 observedGeneration을 찍고,
// meta.SetStatusCondition을 감싼 얇은 wrapper라 값이 안 바뀌면 lastTransitionTime을 보존한다.
func setReadyCondition(status *platformv1.NodeHealthStatus, ready bool, reason, msg string, generation int64) {
	condStatus := metav1.ConditionFalse
	if ready {
		condStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
}

// node의 Ready condition이 True인지 여부 반환
func isNodeReady(node *corev1.Node) bool {
	for i := range node.Status.Conditions {
		c := node.Status.Conditions[i]
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// 이 controller가 관리하는 바로 그 taint인지 여부를 반환하며,
// key와 effect 둘 다로 식별하므로 key는 같아도 effect가 다른(다른 주체 소유) taint는 건드리지 않는다.
func isManagedTaint(t corev1.Taint) bool {
	return t.Key == unhealthyTaintKey && t.Effect == corev1.TaintEffectNoSchedule
}

// unhealthy taint가 없으면 붙이고,
// node의 taint가 바뀌었는지 반환하며,
// 다른 taint는 그대로 둔다.
func ensureUnhealthyTaint(node *corev1.Node) bool {
	if slices.ContainsFunc(node.Spec.Taints, isManagedTaint) { // 이미 있으면 그대로 둠
		return false
	}
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    unhealthyTaintKey,
		Value:  unhealthyTaintValue,
		Effect: corev1.TaintEffectNoSchedule,
	})
	return true
}

// 이 controller가 관리하는 taint만 있을 때 제거하고,
// node의 taint가 바뀌었는지 반환하며,
// key는 같아도 effect가 다른 taint를 포함해 나머지 taint는 보존한다.
func removeUnhealthyTaint(node *corev1.Node) bool {
	var kept []corev1.Taint
	changed := false
	for i := range node.Spec.Taints {
		if isManagedTaint(node.Spec.Taints[i]) { // 관리 대상만 걸러 버림
			changed = true
			continue
		}
		kept = append(kept, node.Spec.Taints[i])
	}
	if changed {
		node.Spec.Taints = kept
	}
	return changed
}
