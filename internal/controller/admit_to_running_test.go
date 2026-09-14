package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

func at(sec int) metav1.Time {
	return metav1.NewTime(time.Date(2026, 9, 5, 12, 0, sec, 0, time.UTC))
}

func conditionOf(status *platformv1.MLTrainingJobStatus) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == mltjCondAdmitToRunning {
			return &status.Conditions[i]
		}
	}
	return nil
}

// TestTheWaitIsRecordedOnlyWhenBothEndsWereWatched is the whole rule.
//
// A tenant is admitted, then runs, and this controller saw both transitions. Kueue's stamp is the start
// because Kueue is the component that admitted; this controller's observation is the end because nothing
// else here can see a Pod become active.
func TestTheWaitIsRecordedOnlyWhenBothEndsWereWatched(t *testing.T) {
	var s platformv1.MLTrainingJobStatus

	admitted := at(0)
	if out := recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseAdmitted, &admitted, at(1)); out.Observed {
		t.Error("admission alone is not an observed window")
	}
	if s.AdmittedAt == nil || !s.AdmittedAt.Equal(&admitted) {
		t.Fatalf("admittedAt = %v, want Kueue's stamp %v", s.AdmittedAt, admitted)
	}

	out := recordAdmitToRunning(&s, mltjPhaseAdmitted, mltjPhaseRunning, &admitted, at(3))
	if !out.Observed {
		t.Fatalf("the window was watched from both ends and was not recorded: %+v", out)
	}
	if s.AdmitToRunningSeconds != "3.000" {
		t.Errorf("admitToRunningSeconds = %q, want \"3.000\"", s.AdmitToRunningSeconds)
	}
	if c := conditionOf(&s); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Observed" {
		t.Errorf("condition = %+v, want True/Observed", c)
	}
}

// TestAControllerThatMissedAdmissionRefusesToInventTheWait is the case the whole design exists for.
//
// Kueue's admission stamp survives on the Workload, so subtracting it from now would always produce a
// number. That number would describe a window nobody watched, and it would enter the histogram looking
// exactly like a measured one.
func TestAControllerThatMissedAdmissionRefusesToInventTheWait(t *testing.T) {
	var s platformv1.MLTrainingJobStatus

	// The controller's first sight of this job is already Running. Kueue's stamp is available and old.
	admitted := at(0)
	out := recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseRunning, &admitted, at(90))

	if out.Observed {
		t.Fatal("a wait was reported for a window this controller did not watch")
	}
	if out.UnobservedReason != reasonAdmissionNotObserved {
		t.Errorf("reason = %q, want %q", out.UnobservedReason, reasonAdmissionNotObserved)
	}
	if s.AdmitToRunningSeconds != "" {
		t.Errorf("admitToRunningSeconds = %q, want empty", s.AdmitToRunningSeconds)
	}
	if s.RunningObservedAt != nil {
		t.Error("runningObservedAt was stamped for an unmeasurable window")
	}
	// Absent and unmeasurable must not look alike to a reader.
	c := conditionOf(&s)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonAdmissionNotObserved {
		t.Fatalf("condition = %+v, want False/%s", c, reasonAdmissionNotObserved)
	}
	if c.Message == "" {
		t.Error("the refusal carries no message, so a tenant cannot tell it from 'not running yet'")
	}
}

// TestClockSkewIsRefusedRatherThanClamped keeps a disagreement out of the tail.
//
// A negative interval means the two clocks disagree. Clamping it to zero would enter the histogram as the
// fastest reclaim this platform ever achieved, which is the most flattering possible reading of a fault.
func TestClockSkewIsRefusedRatherThanClamped(t *testing.T) {
	var s platformv1.MLTrainingJobStatus

	admitted := at(30)
	recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseAdmitted, &admitted, at(30))
	out := recordAdmitToRunning(&s, mltjPhaseAdmitted, mltjPhaseRunning, &admitted, at(10))

	if out.Observed {
		t.Fatal("a running observation before its own admission was accepted")
	}
	if out.UnobservedReason != reasonNegativeInterval {
		t.Errorf("reason = %q, want %q", out.UnobservedReason, reasonNegativeInterval)
	}
	if s.AdmitToRunningSeconds != "" {
		t.Errorf("admitToRunningSeconds = %q, want empty", s.AdmitToRunningSeconds)
	}
}

// TestAnAdmittedWorkloadWithNoStampHasNoWindowStart covers the third refusal.
func TestAnAdmittedWorkloadWithNoStampHasNoWindowStart(t *testing.T) {
	var s platformv1.MLTrainingJobStatus

	out := recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseAdmitted, nil, at(1))
	if out.UnobservedReason != reasonKueueStampMissing {
		t.Errorf("reason = %q, want %q", out.UnobservedReason, reasonKueueStampMissing)
	}
	if s.AdmittedAt != nil {
		t.Error("an admission with no stamp was given one")
	}
}

// A refusal that named the real cause must survive the transition that follows it.
//
// AdmittedAt is nil in two ways, and the Running branch cannot tell them apart from the field alone: either
// this controller never saw the admission, or it saw it and Kueue carried no usable stamp. The second is
// already recorded, accurately, one transition earlier. Running used to overwrite it with a message reading
// "this job was already past admission when this controller first saw it" -- which is false, because the
// controller saw it and said so. The unobserved counter was also incremented twice for one job.
//
// Mutation that turns this red: drop the existing-refusal check in the Running branch.
func TestAStampMissingRefusalIsNotRewrittenAsNeverObserved(t *testing.T) {
	var s platformv1.MLTrainingJobStatus

	first := recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseAdmitted, nil, at(1))
	if first.UnobservedReason != reasonKueueStampMissing {
		t.Fatalf("first reason = %q, want %q", first.UnobservedReason, reasonKueueStampMissing)
	}

	second := recordAdmitToRunning(&s, mltjPhaseAdmitted, mltjPhaseRunning, nil, at(4))
	if second.UnobservedReason != "" {
		t.Errorf("the same job was counted unobserved twice, the second time as %q", second.UnobservedReason)
	}
	if got := admitToRunningRefusal(&s); got != reasonKueueStampMissing {
		t.Errorf("the condition now reads %q, so the accurate cause was replaced by one that did not happen", got)
	}
}

// TestTheWindowIsRecordedOnce keeps a requeue from restamping a measurement.
//
// Reconcile runs on every watch event, and a job sitting in Running produces many of them. Only the
// transition is the observation.
func TestTheWindowIsRecordedOnce(t *testing.T) {
	var s platformv1.MLTrainingJobStatus
	admitted := at(0)
	recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseAdmitted, &admitted, at(0))
	recordAdmitToRunning(&s, mltjPhaseAdmitted, mltjPhaseRunning, &admitted, at(4))
	first := s.AdmitToRunningSeconds

	// A later reconcile with the job still Running.
	out := recordAdmitToRunning(&s, mltjPhaseRunning, mltjPhaseRunning, &admitted, at(400))
	if out.Observed || out.UnobservedReason != "" {
		t.Errorf("a steady-state reconcile produced an outcome: %+v", out)
	}
	if s.AdmitToRunningSeconds != first {
		t.Errorf("the recorded wait moved from %q to %q on a reconcile that observed nothing",
			first, s.AdmitToRunningSeconds)
	}
}

// TestTheQueuelabNumbersLandInDistinctBuckets checks the histogram can tell the two arms apart.
//
// The buckets exist to separate three regimes that already have measurements: a borrower that honoured
// SIGTERM (2.180 s), one that ignored it (31.213 s), and the admission webhook's cap on how long a
// borrower may take to stop (120 s). A bucket layout that puts the first two in the same bin would report
// the difference this platform was built to bound as no difference at all.
func TestTheQueuelabNumbersLandInDistinctBuckets(t *testing.T) {
	buckets := []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 240}
	bucketOf := func(v float64) int {
		for i, b := range buckets {
			if v <= b {
				return i
			}
		}
		return len(buckets)
	}
	honour, ignore, cap := bucketOf(2.180), bucketOf(31.213), bucketOf(120)
	if honour == ignore {
		t.Errorf("2.180s and 31.213s share bucket %d; the arms queuelab separated would look identical", honour)
	}
	if ignore == cap {
		t.Errorf("31.213s and the 120s cap share bucket %d", ignore)
	}
}

// TestARestartedPodDoesNotRestampTheWait covers the transition the phase ladder can repeat.
//
// A Job whose Pods all die drops to Active=0 and the phase falls back to Admitted, then returns to Running
// when a replacement starts. That is a second Admitted-to-Running transition, and without the guard it
// would overwrite the measured wait with the time since the ORIGINAL admission -- reporting a reclaim of
// several minutes for a job that started in four seconds.
func TestARestartedPodDoesNotRestampTheWait(t *testing.T) {
	var s platformv1.MLTrainingJobStatus
	admitted := at(0)
	recordAdmitToRunning(&s, mltjPhasePending, mltjPhaseAdmitted, &admitted, at(0))
	recordAdmitToRunning(&s, mltjPhaseAdmitted, mltjPhaseRunning, &admitted, at(4))
	first := s.AdmitToRunningSeconds
	firstObserved := s.RunningObservedAt.DeepCopy()

	// Every Pod dies, the phase falls back, and a replacement starts five minutes later.
	out := recordAdmitToRunning(&s, mltjPhaseAdmitted, mltjPhaseRunning, &admitted, at(304))

	if out.Observed {
		t.Error("a restart was recorded as a fresh admission-to-running window")
	}
	if s.AdmitToRunningSeconds != first {
		t.Errorf("the wait moved from %q to %q on a Pod restart", first, s.AdmitToRunningSeconds)
	}
	if !s.RunningObservedAt.Equal(firstObserved) {
		t.Errorf("runningObservedAt moved from %v to %v", firstObserved, s.RunningObservedAt)
	}
}

// TestEveryConditionCarriesATransitionTime pins what the apiserver requires.
//
// metav1.Condition.LastTransitionTime has no omitempty and the apiserver rejects a zero value with
// "Required value". Unit tests that only read Status and Reason passed while every write this file made was
// invalid, and the envtest suite is what found it. This asserts it here so the cheap test fails first.
func TestEveryConditionCarriesATransitionTime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prev, next string
		stamp      *metav1.Time
	}{
		{"admitted with a stamp", mltjPhasePending, mltjPhaseAdmitted, new(at(0))},
		{"admitted without one", mltjPhasePending, mltjPhaseAdmitted, nil},
		{"running unwatched", mltjPhasePending, mltjPhaseRunning, new(at(0))},
	} {
		var s platformv1.MLTrainingJobStatus
		recordAdmitToRunning(&s, tc.prev, tc.next, tc.stamp, at(9))
		for i := range s.Conditions {
			if s.Conditions[i].LastTransitionTime.IsZero() {
				t.Errorf("%s: condition %q has a zero LastTransitionTime, which the apiserver rejects",
					tc.name, s.Conditions[i].Type)
			}
		}
	}
}
