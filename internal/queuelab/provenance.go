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

package queuelab

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// ObservedState is the authoritative reading of one observed object version: the lifecycle event its
// conditions now justify (empty when none), and whether that state forces the whole run invalid.
//
// The collector (a later slice) tracks the last emitted event per object and only journals a transition,
// so a repeated observation of the same admitted Workload does not produce duplicate Admitted events. This
// classifier is stateless: it answers "what does THIS version mean", not "did it just change".
type ObservedState struct {
	// Event is the lifecycle transition this object version justifies, or "" for none.
	Event EventType
	// Reason carries the condition reason behind Event (e.g. the preemption mechanism), for provenance.
	Reason string
	// Invalid is set when this state proves the run cannot be a clean comparison (an unexpected eviction,
	// a failed Job), so the runner must discard the run rather than report a contaminated number.
	Invalid bool
	// InvalidReason explains the invalidation.
	InvalidReason string
	// ComponentStampUnixNanos is the CLUSTER COMPONENT's own wall clock for the state this observation
	// describes: the kubelet's finishedAt for a stopped container, the kubelet's Ready condition transition
	// for a running one, Kueue's Admitted condition transition for an admission.
	//
	// It used to be finished-only, and that narrowness was a measurement defect rather than a naming one.
	// The spread between this stamp and the collector's arrival time is what bounds how finely an interval
	// can be read — and every interval this lab publishes has endpoints that are NOT container stops. The
	// headline is admission to running, so neither of its endpoints was sampled, and the bound derived from
	// stop events was being applied to intervals it did not describe. Two reviewers found it independently.
	//
	// Every source is quantised to the second, because metav1.Time serialises RFC3339 without fractions. The
	// arithmetic that turns these into a bound lives in resolution.go and accounts for that.
	//
	// A pointer, because an observation whose component published no timestamp must not read as one stamped
	// at the epoch.
	ComponentStampUnixNanos *int64
	// ExitCode is the terminated container's status, and it is what tells one kind of stop from another.
	//
	// Reason carries the Pod PHASE, which is "Failed" both for a workload that honoured SIGTERM and exited
	// promptly and for one that ignored it and was SIGKILLed at the end of the grace period. Those are the two
	// arms of this experiment, so a ledger that records only the phase cannot say which of them it observed —
	// the termination canary distinguishes them by exit status (143 against 137) and the run itself could not.
	//
	// It is a pointer because "no terminated container status to read" is a different fact from "exited 0",
	// and nil is the only honest spelling of the first.
	ExitCode *int32
	// Iterations is how much work the container had completed when it stopped, read out of the same terminated
	// status the exit code comes from.
	//
	// It is what turns a discarded GPU-second into a discarded UNIT OF WORK. Without it the waste figure says
	// how long a device was held and nothing about what was lost, and a workload that computed nothing looks
	// exactly like one that saturated the card.
	//
	// nil for the same reason ExitCode is: a status that could not be read, or a message that does not carry a
	// count, is a different fact from zero iterations.
	Iterations *int
	// WorkloadKind and DeviceStatus are the workload's own account of WHICH loop produced Iterations, read
	// out of the same terminated status message.
	//
	// They exist because the count alone cannot tell a device run from a CPU run: both paths increment it,
	// both finish healthy, and a card the scheduler reserved and nothing touched produces the same plausible
	// figures as one that computed. Without this the harness could only assert device work from an EXTERNAL
	// observer, which is a claim about the exporter as much as about the card -- and a fake exporter was made
	// to satisfy it.
	//
	// Both are closed-set tokens or empty. A message this build cannot parse into a known pair yields all
	// three fields empty rather than a guess, because the message is a channel the workload controls.
	WorkloadKind string
	DeviceStatus string
}

// ClassifyWorkload reads a Workload's conditions into the authoritative admission/preemption state.
//
// The ONLY preemption the reclaim study expects is in-cohort reclamation: Preempted=True with reason
// InCohortReclamation AND Evicted=True with reason Preempted. Any other eviction (PodsReadyTimeout,
// NodeFailures, AdmissionCheck, a stopped queue, a non-reclaim preemption) is a different mechanism than
// the one under test and would silently change the arm, so it invalidates the run instead of being folded
// as if it were the study's preemption.
func ClassifyWorkload(wl *kueuev1beta2.Workload) ObservedState {
	evicted, evictReason := conditionTrue(wl.Status.Conditions, kueuev1beta2.WorkloadEvicted)
	if evicted {
		preempted, preemptReason := conditionTrue(wl.Status.Conditions, kueuev1beta2.WorkloadPreempted)
		if evictReason == kueuev1beta2.WorkloadEvictedByPreemption &&
			preempted && preemptReason == kueuev1beta2.InCohortReclamationReason {
			return ObservedState{Event: EventPreempted, Reason: kueuev1beta2.InCohortReclamationReason}
		}
		return ObservedState{
			Invalid:       true,
			InvalidReason: fmt.Sprintf("unexpected Workload eviction (reason %q), not the study's in-cohort reclamation", evictReason),
		}
	}
	if admitted, _ := conditionTrue(wl.Status.Conditions, kueuev1beta2.WorkloadAdmitted); admitted && wl.Status.Admission != nil {
		return ObservedState{Event: EventAdmitted, Reason: kueuev1beta2.WorkloadAdmitted,
			ComponentStampUnixNanos: conditionStamp(wl.Status.Conditions, kueuev1beta2.WorkloadAdmitted)}
	}
	return ObservedState{}
}

// ClassifyJob reads a batch/v1 Job's conditions. Only the terminal Complete condition is the authoritative
// Completed event; a failure invalidates the run, because a lab Job runs with backoffLimit 0 (set by the
// runner) and a real training row is not supposed to fail, so a failure means the arm did not run as
// designed.
//
// SuccessCriteriaMet is deliberately NOT treated as completion: Kubernetes sets it before the Job's Pods
// have all terminated, so completing on it would let an orchestrator close the horizon while a Pod is still
// holding a GPU, dropping that interval. Complete is the condition that waits for terminal Pods.
func ClassifyJob(job *batchv1.Job) ObservedState {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return ObservedState{Event: EventCompleted, Reason: string(c.Type)}
		case batchv1.JobFailed:
			return ObservedState{Invalid: true, InvalidReason: fmt.Sprintf("Job failed (%s)", c.Reason)}
		}
	}
	return ObservedState{}
}

// ClassifyPod reads a Pod's status into the execution-start and execution-stop events.
//
// AttemptStopped is a genuine TERMINAL phase (Succeeded or Failed), not the first deletionTimestamp: the
// deletion timestamp only starts the grace period, during which the Pod keeps holding the GPU, so waste
// measured to it would undercount exactly the grace window this event exists to capture.
func ClassifyPod(pod *corev1.Pod) ObservedState {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		code, iters, finished, kind, device := soleTerminated(pod)
		return ObservedState{Event: EventAttemptStopped, Reason: string(pod.Status.Phase),
			ExitCode: code, Iterations: iters, ComponentStampUnixNanos: finished,
			WorkloadKind: kind, DeviceStatus: device}
	}
	if pod.Status.Phase == corev1.PodRunning && podConditionTrue(pod, corev1.PodReady) {
		return ObservedState{Event: EventPodReady, Reason: string(corev1.PodReady),
			ComponentStampUnixNanos: podConditionStamp(pod, corev1.PodReady)}
	}
	return ObservedState{}
}

// conditionTrue reports whether the named condition is present with status True, returning its reason.
func conditionTrue(conditions []metav1.Condition, condType string) (bool, string) {
	for i := range conditions {
		if conditions[i].Type == condType {
			return conditions[i].Status == metav1.ConditionTrue, conditions[i].Reason
		}
	}
	return false, ""
}

// podConditionTrue reports whether the Pod carries the named condition with status True.
func podConditionTrue(pod *corev1.Pod, condType corev1.PodConditionType) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condType {
			return pod.Status.Conditions[i].Status == corev1.ConditionTrue
		}
	}
	return false
}

// soleTerminated returns the exit status and iteration count when exactly one container terminated, and nil otherwise.
//
// The ambiguity is refused rather than resolved by picking a container, because this package has no way to
// know which one carried the workload: the trace's Pods have a single container today, and a template that
// grew a sidecar would make any choice here a guess presented as a measurement. nil then reports that the
// stop was observed but its kind could not be established, which is a weaker claim than a number and the
// only true one.
func soleTerminated(pod *corev1.Pod) (code *int32, iters *int, finished *int64, kind, device string) {
	for i := range pod.Status.ContainerStatuses {
		t := pod.Status.ContainerStatuses[i].State.Terminated
		if t == nil {
			continue
		}
		if code != nil {
			return nil, nil, nil, "", ""
		}
		c := t.ExitCode
		code = &c
		iters, kind, device = ReportFromMessage(t.Message)
		if !t.FinishedAt.IsZero() {
			f := t.FinishedAt.UnixNano()
			finished = &f
		}
	}
	return code, iters, finished, kind, device
}

// Workload kind tokens, as the workload spells them in its own report.
//
// They are exported because the record's provenance block is derived from them and its writer must not keep
// a second copy: a workload change that edited one spelling and not the other would leave the contradiction
// check comparing against a kind nothing emits, which is guarding nothing while looking like a guard.
const (
	// KindCPUFloat is the fallback loop: pure Python arithmetic that makes no driver call.
	KindCPUFloat = "cpu-float"
	// KindCUDAFMA is the device loop: a PTX kernel launched through the CUDA driver API.
	KindCUDAFMA = "cuda-fma"
)

// DeviceOK is the only device status that means a kernel actually ran.
const DeviceOK = "ok"

// DeviceLaunchFailedMidrun is a card that ran a kernel and then stopped, which is the one failure that says
// something about the HARDWARE rather than about the image or the passthrough.
//
// It is a constant because the token crosses a language boundary: the embedded Python writes it into the
// termination message and this package parses it back out, and a rename on one side would silently reclassify
// a dying card as a run that never reached the device. TestTheWorkloadEmitsTheDeviceTokenThisPackageParses
// holds the two together.
const DeviceLaunchFailedMidrun = "launch-failed-midrun"

// deviceStatuses is every device-path outcome the workload can report, one per call it makes.
//
// The set is closed and each member sends a reader somewhere different: no-libcuda is a base image or a
// container runtime that never injected the driver, no-device is a card that was not passed through,
// ptx-load-failed is a kernel this driver would not JIT, and launch-failed-midrun is a card that worked and
// then stopped. Collapsing them into a bool would turn every one of those into the same shrug.
var deviceStatuses = map[string]bool{
	DeviceOK: true, "not-attempted": true, "no-libcuda": true, "cuinit-failed": true, "no-device": true,
	"ctx-failed": true, "ptx-load-failed": true, "no-kernel": true, "alloc-failed": true,
	"memset-failed": true, "launch-failed": true, DeviceLaunchFailedMidrun: true,
}

// ReportFromMessage reads the workload's own account out of the terminated status message.
//
// It is exported because the device preflight reads the same sentence from a Pod it applied directly, without
// a ledger anywhere: one parser for one wire format, so a preflight that passes cannot be followed by a run
// whose collector reads the same container differently.
//
// The kubelet copies /dev/termination-log there, and the workload rewrites that file from inside its loop, so
// this arrives even for a container SIGKILLed at the grace boundary -- the arm whose discarded work the
// experiment is about, and the one that can print no final line because SIGKILL cannot be handled.
//
// Anything that is not exactly the expected shape yields nothing rather than a guess, and the count is
// refused ALONGSIDE the tokens rather than kept. They are three readings of one sentence: a message this
// build cannot parse is a message whose iteration count it also has no reason to trust, and keeping the
// number from a sentence whose other half was unintelligible is how a measurement gets invented from
// whatever happened to be in a file.
//
// The two tokens are checked against each other, not just against their own sets. dev=ok means a kernel
// launched, so it cannot appear beside the CPU fallback; and the device kind can only carry ok or the
// mid-run failure, because every earlier failure returns before the kind is set. A pair outside that
// relation was not written by this workload.
func ReportFromMessage(msg string) (iters *int, kind, device string) {
	fields := strings.Fields(strings.TrimSpace(msg))
	if len(fields) != 3 {
		return nil, "", ""
	}
	n, err := strconv.Atoi(strings.TrimPrefix(fields[0], "iters="))
	if !strings.HasPrefix(fields[0], "iters=") || err != nil || n < 0 {
		return nil, "", ""
	}
	if !strings.HasPrefix(fields[1], "kind=") || !strings.HasPrefix(fields[2], "dev=") {
		return nil, "", ""
	}
	k := strings.TrimPrefix(fields[1], "kind=")
	d := strings.TrimPrefix(fields[2], "dev=")
	if !deviceStatuses[d] {
		return nil, "", ""
	}
	switch {
	case k == KindCPUFloat && d != DeviceOK:
	case k == KindCUDAFMA && (d == DeviceOK || d == DeviceLaunchFailedMidrun):
	default:
		return nil, "", ""
	}
	return &n, k, d
}

// conditionStamp is the component's own transition time for a metav1 condition, or nil when it published none.
//
// Kueue writes lastTransitionTime on the Admitted condition, so this is Kueue's clock for the admission
// rather than the collector's clock for hearing about it. That distinction is the entire reason the field
// exists: an interval between two arrival times carries the difference of their delivery lags, and nothing
// bounds that difference unless both endpoints can be compared against the component that produced them.
func conditionStamp(conditions []metav1.Condition, condType string) *int64 {
	for i := range conditions {
		if conditions[i].Type != condType {
			continue
		}
		if conditions[i].LastTransitionTime.IsZero() {
			return nil
		}
		ns := conditions[i].LastTransitionTime.UnixNano()
		return &ns
	}
	return nil
}

// podConditionStamp is conditionStamp for a Pod's own condition type, which uses a different struct.
//
// The kubelet writes lastTransitionTime on the Ready condition, so this is the kubelet's clock for the
// container becoming ready — the far endpoint of the owner's wait, and the one that went unsampled while the
// harness bounded that wait with the spread of container STOPS.
func podConditionStamp(pod *corev1.Pod, condType corev1.PodConditionType) *int64 {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type != condType {
			continue
		}
		if pod.Status.Conditions[i].LastTransitionTime.IsZero() {
			return nil
		}
		ns := pod.Status.Conditions[i].LastTransitionTime.UnixNano()
		return &ns
	}
	return nil
}
