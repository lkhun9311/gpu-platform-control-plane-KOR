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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// condTrue builds a status-True condition; every provenance case here turns on a True condition, and a
// missing/False condition is modelled by simply omitting it.
func condTrue(t, reason string) metav1.Condition {
	return metav1.Condition{Type: t, Status: metav1.ConditionTrue, Reason: reason}
}

func TestClassifyWorkloadAdmitted(t *testing.T) {
	wl := &kueuev1beta2.Workload{Status: kueuev1beta2.WorkloadStatus{
		Conditions: []metav1.Condition{condTrue(kueuev1beta2.WorkloadAdmitted, "Admitted")},
		Admission:  &kueuev1beta2.Admission{},
	}}
	if got := ClassifyWorkload(wl); got.Event != EventAdmitted {
		t.Fatalf("admitted workload = %+v, want EventAdmitted", got)
	}
}

func TestClassifyWorkloadAdmittedRequiresAdmissionField(t *testing.T) {
	// Admitted=True without status.admission is a half-written status; it must not count as an admission.
	wl := &kueuev1beta2.Workload{Status: kueuev1beta2.WorkloadStatus{
		Conditions: []metav1.Condition{condTrue(kueuev1beta2.WorkloadAdmitted, "Admitted")},
	}}
	if got := ClassifyWorkload(wl); got.Event != "" {
		t.Fatalf("admitted without admission field = %+v, want no event", got)
	}
}

func TestClassifyWorkloadExpectedReclaimPreemption(t *testing.T) {
	wl := &kueuev1beta2.Workload{Status: kueuev1beta2.WorkloadStatus{Conditions: []metav1.Condition{
		condTrue(kueuev1beta2.WorkloadEvicted, kueuev1beta2.WorkloadEvictedByPreemption),
		condTrue(kueuev1beta2.WorkloadPreempted, kueuev1beta2.InCohortReclamationReason),
	}}}
	got := ClassifyWorkload(wl)
	if got.Event != EventPreempted || got.Invalid {
		t.Fatalf("in-cohort reclamation = %+v, want EventPreempted and not invalid", got)
	}
}

func TestClassifyWorkloadUnexpectedEvictionInvalidates(t *testing.T) {
	// A PodsReadyTimeout eviction is a different mechanism than the study's reclaim; folding it as the
	// study's preemption would contaminate the arm, so it invalidates the run.
	wl := &kueuev1beta2.Workload{Status: kueuev1beta2.WorkloadStatus{Conditions: []metav1.Condition{
		condTrue(kueuev1beta2.WorkloadEvicted, kueuev1beta2.WorkloadEvictedByPodsReadyTimeout),
	}}}
	if got := ClassifyWorkload(wl); !got.Invalid {
		t.Fatalf("PodsReadyTimeout eviction = %+v, want invalid", got)
	}
}

func TestClassifyWorkloadPreemptionWrongReasonInvalidates(t *testing.T) {
	// Evicted by preemption but NOT for in-cohort reclamation (e.g. a priority preemption within the queue)
	// is not the mechanism under test.
	wl := &kueuev1beta2.Workload{Status: kueuev1beta2.WorkloadStatus{Conditions: []metav1.Condition{
		condTrue(kueuev1beta2.WorkloadEvicted, kueuev1beta2.WorkloadEvictedByPreemption),
		condTrue(kueuev1beta2.WorkloadPreempted, "PriorityPreemption"),
	}}}
	if got := ClassifyWorkload(wl); !got.Invalid {
		t.Fatalf("non-reclaim preemption = %+v, want invalid", got)
	}
}

func TestClassifyJobCompleteAndFailed(t *testing.T) {
	complete := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}}}
	if got := ClassifyJob(complete); got.Event != EventCompleted {
		t.Fatalf("complete job = %+v, want EventCompleted", got)
	}
	failed := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
	}}}
	if got := ClassifyJob(failed); !got.Invalid {
		t.Fatalf("failed job = %+v, want invalid", got)
	}
}

func TestClassifyPodReadyAndStopped(t *testing.T) {
	ready := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	if got := ClassifyPod(ready); got.Event != EventPodReady {
		t.Fatalf("ready pod = %+v, want EventPodReady", got)
	}
	stopped := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	if got := ClassifyPod(stopped); got.Event != EventAttemptStopped {
		t.Fatalf("terminal pod = %+v, want EventAttemptStopped", got)
	}
	// A pod merely marked for deletion (grace period started) but not yet terminal must NOT be stopped, or
	// the grace-window work would be dropped from waste.
	deleting := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if got := ClassifyPod(deleting); got.Event == EventAttemptStopped {
		t.Fatalf("a deleting-but-running pod must not be AttemptStopped yet")
	}
}

// The Pod phase cannot tell the two arms apart at the moment the experiment is about.
//
// "Failed" is the phase both for a workload that honoured SIGTERM and exited promptly and for one that
// ignored it until the grace period ran out and was killed — 143 against 137, the exact contrast the
// termination canary qualifies and the run itself could not observe. Recording the phase alone left the
// ledger unable to say which kind of stop it had watched, so a claim that a run exercised the SIGKILL path
// rested on the clock rather than on evidence.
//
// Mutation that turns this red: drop ExitCode from the ObservedState that ClassifyPod returns.
func TestClassifyPodRecordsWhichKindOfStopItObserved(t *testing.T) {
	stopped := func(phase corev1.PodPhase, codes ...int32) *corev1.Pod {
		p := &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		for _, c := range codes {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: c}},
			})
		}
		return p
	}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want *int32
	}{
		{"a victim that honoured the signal", stopped(corev1.PodFailed, 143), new(int32(143))},
		{"a victim that was killed at the grace period", stopped(corev1.PodFailed, 137), new(int32(137))},
		{"a victim that ran out its own service", stopped(corev1.PodSucceeded, 0), new(int32(0))},
		// Absent rather than guessed: nothing here knows which container carried the workload.
		{"two terminated containers", stopped(corev1.PodFailed, 143, 0), nil},
		{"a terminal phase with no container status", stopped(corev1.PodFailed), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyPod(tc.pod)
			if got.Event != EventAttemptStopped {
				t.Fatalf("a terminal phase must be an AttemptStopped, got %q", got.Event)
			}
			switch {
			case tc.want == nil && got.ExitCode != nil:
				t.Fatalf("the exit code is ambiguous here, yet %d was reported as if measured", *got.ExitCode)
			case tc.want != nil && got.ExitCode == nil:
				t.Fatalf("exit %d was readable and was not recorded; the ledger then cannot tell this stop "+
					"from the other arm's", *tc.want)
			case tc.want != nil && *got.ExitCode != *tc.want:
				t.Fatalf("recorded exit %d, want %d", *got.ExitCode, *tc.want)
			}
		})
	}
}

//go:fix inline

// The count arrives in the Pod object, which is what lets it survive the arm that cannot report anything.
//
// The ignoring workload is SIGKILLed at the grace boundary and SIGKILL cannot be handled, so it prints no
// final line — its stdout ends wherever the last periodic flush landed, and teardown deletes that anyway. The
// kubelet copies /dev/termination-log into the terminated status instead, and the workload rewrites that file
// from inside its loop, so the number is in the object the collector already watches.
//
// A message that is not exactly the expected shape yields nil rather than a guess: it is a channel the
// workload controls, and a number parsed loosely out of it would be a measurement invented from whatever
// happened to be in a file.
//
// Mutation that turns this red: drop Iterations from the ObservedState ClassifyPod returns.
func TestClassifyPodReadsTheWorkCompletedFromTheTerminatedStatus(t *testing.T) {
	stopped := func(phase corev1.PodPhase, code int32, msg string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}},
		}}}}
	}
	for _, tc := range []struct {
		name string
		msg  string
		want *int
	}{
		{"a killed workload's last written report", "iters=137 kind=cpu-float dev=no-libcuda",
			func() *int { v := 137; return &v }()},
		{"trailing newline from the file", "iters=40 kind=cpu-float dev=no-libcuda\n",
			func() *int { v := 40; return &v }()},
		{"zero is a count, not an absence", "iters=0 kind=cpu-float dev=no-libcuda",
			func() *int { v := 0; return &v }()},
		{"the device path, which is the pair the session exists to produce", "iters=9001 kind=cuda-fma dev=ok",
			func() *int { v := 9001; return &v }()},
		{"a card that worked and then stopped", "iters=12 kind=cuda-fma dev=launch-failed-midrun",
			func() *int { v := 12; return &v }()},
		{"a message that is not a report", "OOMKilled", nil},
		{"an empty termination log", "", nil},
		{"something shaped like a count but not one", "iters=many kind=cpu-float dev=no-libcuda", nil},
		{"a negative count", "iters=-3 kind=cpu-float dev=no-libcuda", nil},
		// The count is refused ALONGSIDE the tokens rather than kept. A message this build cannot parse is a
		// message whose number it also has no reason to trust, and the old shape -- a bare count with nothing
		// saying which loop produced it -- is exactly what a version-16 workload wrote.
		{"the count alone, which is what the previous workload wrote", "iters=137", nil},
		{"a kind no workload here emits", "iters=5 kind=torch dev=ok", nil},
		{"a device status outside the closed set", "iters=5 kind=cuda-fma dev=probably-fine", nil},
		// The same unknown status beside the FALLBACK, which is the row that makes the closed-set check
		// load-bearing. The cross-token clause below catches the device-kind row above on its own -- an
		// unknown status is not ok and not the mid-run failure -- so without this row the whole set could be
		// deleted and the suite would stay green, and any string at all would be recorded as a device status
		// for a reader to interpret.
		{"an unknown device status beside the fallback", "iters=5 kind=cpu-float dev=probably-fine", nil},
		// The two tokens are checked against EACH OTHER, and these two rows are why that clause exists. Both
		// halves are individually valid and the pair is impossible: dev=ok means a kernel launched, so it
		// cannot sit beside the fallback; and every device failure before the launch returns before the kind
		// is set, so the device kind cannot carry one.
		{"the fallback claiming a kernel ran", "iters=5 kind=cpu-float dev=ok", nil},
		{"the device kind wearing a pre-launch failure", "iters=5 kind=cuda-fma dev=no-libcuda", nil},
		{"a report missing its device status", "iters=5 kind=cuda-fma", nil},
		// The row that makes the prefix check load-bearing. Every other rejection above is caught by Atoi
		// failing, so without this one the check could be deleted and the suite would stay green — a bare
		// number would then be read as an iteration count. The termination log is a file anything in the
		// container can write, and "42" is exactly what an unrelated tool leaves behind.
		{"a bare number nobody labelled", "42", nil},
		{"another tool's key", "tokens=5", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyPod(stopped(corev1.PodFailed, 137, tc.msg)).Iterations
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("message %q yielded %d; a number invented from an unrecognised message is not a "+
					"measurement", tc.msg, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("message %q carried %d and it was dropped, so the arm that can report nothing else "+
					"leaves no evidence of the work it lost", tc.msg, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("read %d, want %d", *got, *tc.want)
			}
		})
	}
}

// Every endpoint kind must carry the component's own stamp, not only container stops.
//
// This is the regression guard for a measurement defect two reviewers found independently: the harness
// bounded the resolution of its intervals with the spread of container STOP skews, while the interval it
// publishes as its headline runs from a Kueue admission to a kubelet readiness. Neither endpoint was
// sampled, so the bound described events the interval did not contain.
//
// Mutation that turns this red: drop ComponentStampUnixNanos from either branch of ClassifyPod, or from the
// admitted branch of ClassifyWorkload.
func TestEveryEndpointCarriesItsComponentsOwnStamp(t *testing.T) {
	at := metav1.Date(2026, 8, 20, 5, 0, 30, 0, time.UTC)

	ready := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: at,
		}},
	}}
	got := ClassifyPod(ready)
	if got.Event != EventPodReady {
		t.Fatalf("event = %v, want PodReady", got.Event)
	}
	if got.ComponentStampUnixNanos == nil {
		t.Fatal("a running Pod's readiness carried no kubelet stamp; the far endpoint of the owner's wait " +
			"would go unsampled and the interval's bound would come from events it does not contain")
	}
	if *got.ComponentStampUnixNanos != at.UnixNano() {
		t.Fatalf("stamp = %d, want the Ready condition's own transition time %d",
			*got.ComponentStampUnixNanos, at.UnixNano())
	}

	// The near endpoint: Kueue's own clock for the admission. Without it the owner's wait is bounded at
	// neither end, which is the state the whole rename corrects.
	admitted := &kueuev1beta2.Workload{Status: kueuev1beta2.WorkloadStatus{
		Conditions: []metav1.Condition{{
			Type: kueuev1beta2.WorkloadAdmitted, Status: metav1.ConditionTrue,
			Reason: "Admitted", LastTransitionTime: at,
		}},
		Admission: &kueuev1beta2.Admission{},
	}}
	gotWL := ClassifyWorkload(admitted)
	if gotWL.Event != EventAdmitted {
		t.Fatalf("event = %v, want Admitted", gotWL.Event)
	}
	if gotWL.ComponentStampUnixNanos == nil {
		t.Fatal("an admission carried no Kueue stamp; the near endpoint of the owner's wait would go " +
			"unsampled and the interval's bound would again come from events it does not contain")
	}
	if *gotWL.ComponentStampUnixNanos != at.UnixNano() {
		t.Fatalf("stamp = %d, want the Admitted condition's own transition time %d",
			*gotWL.ComponentStampUnixNanos, at.UnixNano())
	}

	// A Pod whose component published no transition time must read as unsampled rather than as stamped at
	// the epoch, which would present a fifty-six year skew as a measurement.
	blank := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	if s := ClassifyPod(blank).ComponentStampUnixNanos; s != nil {
		t.Fatalf("a Ready condition with no transition time produced a stamp of %d", *s)
	}
}
