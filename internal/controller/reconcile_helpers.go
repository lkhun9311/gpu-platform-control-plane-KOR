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

// nodeHealthFinalizer guards NodeHealth cleanup.
//
// On deletion the reconciler removes the unhealthy taint it owns before dropping this finalizer.
const nodeHealthFinalizer = "nodehealth.platform.lkhun9311.github.io/finalizer"

// mlTrainingJobFinalizer guards MLTrainingJob cleanup.
//
// The owned Job is removed by garbage collection via the owner reference, so on deletion the reconciler only needs to drop this finalizer.
const mlTrainingJobFinalizer = "mltrainingjob.platform.lkhun9311.github.io/finalizer"

// kueueQueueLabel is the label Kueue reads to admit a Job through a LocalQueue of the same name.
const kueueQueueLabel = "kueue.x-k8s.io/queue-name"

// kueueJobUIDLabel is the label Kueue stamps on a Workload with the UID of the Job it wraps.
//
// This is a label, not an annotation, so it can be used as a List label selector.
const kueueJobUIDLabel = "kueue.x-k8s.io/job-uid"

// MLTrainingJob phase constants for the whole lifecycle ladder.
//
// Pending, Admitted, Running and Succeeded are derived from the owned Job and its Kueue Workload by computeMLTJPhase.
//
// Failed is reached either the same way, from a Job Failed condition, or directly by markFailed for a deterministic controller-side error such as a Job name conflict.
const (
	mltjPhasePending   = "Pending"
	mltjPhaseAdmitted  = "Admitted"
	mltjPhaseRunning   = "Running"
	mltjPhaseSucceeded = "Succeeded"
	mltjPhaseFailed    = "Failed"
)

// MLTrainingJob condition types and reasons.
//
// mltjCondSynced reflects whether the owned Job was synced from spec, and is only ever set to False by markFailed.
//
// mltjCondAdmitted reflects Kueue admission and Job progress, and is set by computeMLTJPhase on every normal reconcile.
const (
	mltjCondSynced     = "JobSynced"
	mltjCondAdmitted   = "Admitted"
	mltjReasonConflict = "JobConflict"
)

// unhealthyTaintKey/Value/Effect is the taint the reconciler applies to quarantine a not-ready node so the scheduler stops placing GPU workloads on it.
//
// The reconciler manages only this taint.
const (
	unhealthyTaintKey   = "platform.lkhun9311.github.io/unhealthy"
	unhealthyTaintValue = "true"
)

// faultSourceNodeNotReady is the faultSignal source recorded while a node is quarantined for being not ready.
//
// Honesty: this is a readiness-derived signal, not a real hardware fault signal.
const faultSourceNodeNotReady = "node-not-ready"

// conditionReady is the NodeHealth condition type that mirrors target node readiness.
const conditionReady = "Ready"

// Condition reasons for conditionReady.
const (
	reasonNodeReady    = "NodeReady"
	reasonNodeNotReady = "NodeNotReady"
	reasonNodeNotFound = "NodeNotFound"
)

// NodeHealth phases emitted in M3.
//
// M3 drives readiness into Pending (node absent), Ready (node ready), and Quarantine (node not ready -> tainted).
//
// The Intake and Degraded phases in the CRD enum are reserved for later lifecycle stages (see docs/03) and are not emitted here.
const (
	phasePending    = "Pending"
	phaseReady      = "Ready"
	phaseQuarantine = "Quarantine"
)

// setPhase updates the phase and bumps lastTransitionTime only when the phase changes.
func setPhase(status *platformv1.NodeHealthStatus, phase string) {
	if status.Phase == phase {
		return
	}
	status.Phase = phase
	now := metav1.Now()
	status.LastTransitionTime = &now
}

// setReadyCondition sets the Ready condition, stamping observedGeneration.
//
// It is a thin wrapper over meta.SetStatusCondition (which preserves lastTransitionTime when unchanged).
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

// isNodeReady reports whether the node's Ready condition is True.
func isNodeReady(node *corev1.Node) bool {
	for i := range node.Status.Conditions {
		c := node.Status.Conditions[i]
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// isManagedTaint reports whether a taint is the exact one this controller manages.
//
// It is identified by key AND effect, so a same-key taint with a different effect owned by another actor is left alone.
func isManagedTaint(t corev1.Taint) bool {
	return t.Key == unhealthyTaintKey && t.Effect == corev1.TaintEffectNoSchedule
}

// ensureUnhealthyTaint adds the platform unhealthy taint if it is absent.
//
// It returns whether the node's taints changed.
//
// Other taints are left untouched.
func ensureUnhealthyTaint(node *corev1.Node) bool {
	if slices.ContainsFunc(node.Spec.Taints, isManagedTaint) {
		return false
	}
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    unhealthyTaintKey,
		Value:  unhealthyTaintValue,
		Effect: corev1.TaintEffectNoSchedule,
	})
	return true
}

// removeUnhealthyTaint removes only the taint this controller manages, if present.
//
// It returns whether the node's taints changed.
//
// Other taints, including a same-key taint with a different effect, are preserved.
func removeUnhealthyTaint(node *corev1.Node) bool {
	var kept []corev1.Taint
	changed := false
	for i := range node.Spec.Taints {
		if isManagedTaint(node.Spec.Taints[i]) {
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
