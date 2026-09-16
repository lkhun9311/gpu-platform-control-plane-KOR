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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// This file records how long a tenant waited between having quota and using it.
//
// The platform already BOUNDS that wait and has never REPORTED it. GPUQuotaPolicy configures the cohort
// with reclaimWithinCohort, so an idle tenant's quota is lent and taken back by preemption; the admission
// webhook caps terminationGracePeriodSeconds at 120 because reclaim waits for termination and the field is
// set by the tenant being preempted. Both of those are about the worst case. Neither tells the tenant who
// owns the quota whether their reclaim actually took two seconds or a hundred and nineteen.
//
// queuelab measured that number under preemption on a kind cluster: 2.180 s when the preempted borrower
// honoured SIGTERM and 31.213 s when it ignored it, over four interleaved runs against a 5.906 s floor. The
// lab measures it. The platform did not.

// mltjCondAdmitToRunning reports whether the admission-to-running window was observed.
//
// It is a condition and not merely an absent field because absent has two meanings -- not yet running, and
// running but unwatched -- and a tenant reading an empty admitToRunningSeconds cannot tell them apart.
const mltjCondAdmitToRunning = "AdmitToRunningObserved"

// Reasons the window was not observed. Each is a different operational story and they are not merged.
const (
	// reasonAdmissionNotObserved is a controller that was not watching when the job was admitted.
	//
	// Kueue's admission stamp survives on the Workload, so this controller could read it after the fact and
	// subtract. It must not: the result would be a duration nobody watched, computed from an endpoint
	// recovered from another component's history. That is the defect queuelab's ledger refuses -- "the
	// controller restarted and the target recovered while it was down" and "the target recovered" are
	// different facts.
	reasonAdmissionNotObserved = "AdmissionNotObserved"
	// reasonKueueStampMissing is a Workload admitted with no usable stamp on its condition.
	reasonKueueStampMissing = "KueueStampMissing"
	// reasonNegativeInterval is a running observation that precedes the admission it is measured from.
	//
	// It happens when the two clocks disagree, and the interval is refused rather than clamped to zero: a
	// zero here would enter the histogram as the fastest reclaim the platform ever achieved.
	reasonNegativeInterval = "NegativeInterval"
)

// admitToRunningOutcome is what recordAdmitToRunning decided, so the caller can emit metrics without
// re-deriving it from the status it just wrote.
type admitToRunningOutcome struct {
	// Seconds is the observed interval, set only when Observed is true.
	Seconds float64
	// Observed is true when this call completed a window this controller watched from both ends.
	Observed bool
	// UnobservedReason is set when a running transition could not be measured, and empty otherwise.
	//
	// Empty is not "fine": most calls neither observe nor fail, because most calls are not the transition
	// this is about.
	UnobservedReason string
}

// recordAdmitToRunning stamps the admission and running ends of the window onto a status.
//
// Pure, and takes the times rather than reading a clock, because the rule it enforces is about WHICH
// reconcile may record WHICH endpoint, and a rule like that is only worth what its test is.
//
// prev is the phase this job was in before this reconcile, next is the phase it is moving to, and
// kueueAdmittedAt is the stamp Kueue wrote on its own Admitted condition, or nil when there is none.
func recordAdmitToRunning(
	status *platformv1.MLTrainingJobStatus,
	prev, next string,
	kueueAdmittedAt *metav1.Time,
	now metav1.Time,
) admitToRunningOutcome {
	switch {
	case next == mltjPhaseAdmitted && prev != mltjPhaseAdmitted:
		// The admission end, recorded only on the reconcile that saw the job admitted and NOT yet running.
		//
		// Kueue's own stamp is used rather than now, because Kueue is the component that acted and its stamp
		// is the accurate one. Reading it is legitimate here precisely because this reconcile witnessed the
		// transition; the same read on a later reconcile would not be.
		if status.AdmittedAt != nil {
			return admitToRunningOutcome{}
		}
		if kueueAdmittedAt == nil || kueueAdmittedAt.IsZero() {
			setUnobserved(status, reasonKueueStampMissing, now)
			return admitToRunningOutcome{UnobservedReason: reasonKueueStampMissing}
		}
		stamp := *kueueAdmittedAt
		status.AdmittedAt = &stamp
		return admitToRunningOutcome{}

	case next == mltjPhaseRunning && prev != mltjPhaseRunning:
		// The running end. This one is this controller's observation and carries the watch lag, which is why
		// the field it lands in is called runningObservedAt and not runningAt.
		if status.RunningObservedAt != nil {
			return admitToRunningOutcome{}
		}
		if status.AdmittedAt == nil {
			// A start that is missing is not the same as a start that was never witnessed, and only one of
			// those is this branch's business.
			//
			// AdmittedAt is nil in two ways. Either this controller never saw the Admitted transition, which
			// is what reasonAdmissionNotObserved says and means. Or it DID see it and Kueue carried no usable
			// stamp, which the Admitted branch above already recorded as reasonKueueStampMissing. Both arrive
			// here identically, and this used to overwrite the second with a message reading "this job was
			// already past admission when this controller first saw it" -- which is false, because the
			// controller saw it and said so one transition earlier. The accurate refusal was replaced by an
			// inaccurate one, and the unobserved counter was incremented twice for one job.
			if r := admitToRunningRefusal(status); r != "" {
				return admitToRunningOutcome{}
			}
			setUnobserved(status, reasonAdmissionNotObserved, now)
			return admitToRunningOutcome{UnobservedReason: reasonAdmissionNotObserved}
		}
		secs := now.Time.Sub(status.AdmittedAt.Time).Seconds()
		if secs < 0 {
			setUnobserved(status, reasonNegativeInterval, now)
			return admitToRunningOutcome{UnobservedReason: reasonNegativeInterval}
		}
		observed := now
		status.RunningObservedAt = &observed
		status.AdmitToRunningSeconds = fmt.Sprintf("%.3f", secs)
		setObserved(status, secs, now)
		return admitToRunningOutcome{Seconds: secs, Observed: true}
	}

	return admitToRunningOutcome{}
}

// setObserved records that the window was watched end to end.
func setObserved(status *platformv1.MLTrainingJobStatus, secs float64, now metav1.Time) {
	setAdmitToRunningCondition(status, now, metav1.ConditionTrue, "Observed",
		fmt.Sprintf("the tenant waited %.3fs between Kueue admitting this job and this controller seeing it run;"+
			" the admission end is Kueue's own stamp and the running end is this controller's observation,"+
			" so the interval carries one watch lag", secs))
}

// setUnobserved records that a running job's wait cannot be reported, and why.
func setUnobserved(status *platformv1.MLTrainingJobStatus, reason string, now metav1.Time) {
	var msg string
	switch reason {
	case reasonAdmissionNotObserved:
		msg = "this job was already past admission when this controller first saw it, so the wait it reports" +
			" would be a duration nobody watched; Kueue's stamp is still on the Workload and is deliberately" +
			" not used here"
	case reasonKueueStampMissing:
		msg = "the Workload is admitted but carries no usable admission stamp, so the window has no start"
	case reasonNegativeInterval:
		msg = "the running observation precedes the admission it would be measured from, which means the two" +
			" clocks disagree; the interval is refused rather than clamped, because a clamped zero would" +
			" enter the histogram as the fastest reclaim this platform ever achieved"
	default:
		msg = "the admission-to-running window was not observed"
	}
	setAdmitToRunningCondition(status, now, metav1.ConditionFalse, reason, msg)
}

// setAdmitToRunningCondition writes the condition without disturbing the others.
//
// It takes the time rather than reading a clock so the whole path stays pure and testable, and it sets
// LastTransitionTime on every write that changes the status. That field is REQUIRED by the apiserver, which
// is not obvious from the type: metav1.Condition has no omitempty on it and a zero value is rejected with
// "status.conditions[N].lastTransitionTime: Required value". The first version of this function left it
// unset, unit tests passed, and the envtest suite caught it against a real apiserver.
//
// meta.SetStatusCondition would have set it, and is deliberately not used for a different reason: it keeps
// the existing transition time when the status is unchanged, which is right for the phase conditions and
// wrong here, where two consecutive refusals for different reasons must not read as one unchanging fact.
func setAdmitToRunningCondition(
	status *platformv1.MLTrainingJobStatus,
	now metav1.Time,
	s metav1.ConditionStatus,
	reason, msg string,
) {
	cond := metav1.Condition{
		Type:               mltjCondAdmitToRunning,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type != mltjCondAdmitToRunning {
			continue
		}
		if status.Conditions[i].Status == s && status.Conditions[i].Reason == reason {
			// Same fact as last time: only the wording may have moved, so the transition stands.
			status.Conditions[i].Message = msg
			if status.Conditions[i].LastTransitionTime.IsZero() {
				status.Conditions[i].LastTransitionTime = now
			}
			return
		}
		status.Conditions[i] = cond
		return
	}
	status.Conditions = append(status.Conditions, cond)
}

// kueueAdmittedStamp returns the time Kueue recorded on its Admitted condition, or nil.
func kueueAdmittedStamp(conds []metav1.Condition) *metav1.Time {
	for i := range conds {
		if conds[i].Type != string(kueuev1beta1.WorkloadAdmitted) || conds[i].Status != metav1.ConditionTrue {
			continue
		}
		if conds[i].LastTransitionTime.IsZero() {
			return nil
		}
		t := conds[i].LastTransitionTime
		return &t
	}
	return nil
}

// admitToRunningRefusal is the reason already on the condition when it is a refusal, or "" when there is
// none. It exists so a later transition can leave an earlier, more specific refusal standing.
func admitToRunningRefusal(status *platformv1.MLTrainingJobStatus) string {
	for i := range status.Conditions {
		c := status.Conditions[i]
		if c.Type == mltjCondAdmitToRunning && c.Status == metav1.ConditionFalse {
			return c.Reason
		}
	}
	return ""
}
