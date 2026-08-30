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
	"bytes"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/exputil"
)

const sec = int64(1_000_000_000)

// reclaimAnyTrace is the frozen offered work Reconstruct is seeded from: a1 and a2 both borrow tenant-a's
// side, b1 is the returning owner. Reconstruct never invents jobs beyond these rows.
func reclaimAnyTrace() []TrainingTraceRow {
	return []TrainingTraceRow{
		{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 600},
		{Index: 1, Name: "a2", OffsetMs: 1_000, Tenant: "tenant-a", GPUCount: 1, DurationSec: 600},
		{Index: 2, Name: "b1", OffsetMs: 590_000, Tenant: "tenant-b", GPUCount: 1, DurationSec: 60},
	}
}

// matchedServiceTimeTrace declares each row's service time as the synthetic ledgers below actually run it, so
// the rendered occupancy can be READ against it.
//
// reclaimAnyTrace declares 600 s for rows whose events run 40 s, which renders as
// "totalOccupancy=81.0(serviceTime=600s)" and reads as a job using 81 of 600 requested seconds -- the exact
// inverse of the finding ("81 s for a 40 s job") that showing service time exists to make legible.
func matchedServiceTimeTrace() []TrainingTraceRow {
	return []TrainingTraceRow{
		{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 600},
		// a2 runs 3 s -> 43 s on its first attempt and 46 s -> 87 s on its retry: 40 s of declared service,
		// executed twice.
		{Index: 1, Name: "a2", OffsetMs: 1_000, Tenant: "tenant-a", GPUCount: 1, DurationSec: 40},
		{Index: 2, Name: "b1", OffsetMs: 34_000, Tenant: "tenant-b", GPUCount: 1, DurationSec: 46},
	}
}

// reclaimAnyEvents is a synthetic ledger for the reclaim "Any", late-owner-return case:
// a1 runs to completion; a2 borrows then is preempted late (its work discarded, its Pod stopping a couple
// seconds after the decision); b1 the owner is admitted quickly after the preemption and completes.
func reclaimAnyEvents() []LifecycleEvent {
	return []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
		{ElapsedNs: 1*sec + sec/2, Kind: "Pod", Type: EventPodReady, Job: "a1", ObjectUID: "pod-a1"},
		{ElapsedNs: 601 * sec, Kind: "Job", Type: EventCompleted, Job: "a1"},

		{ElapsedNs: 1 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 2*sec + sec/2, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 591 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
		// The Pod does not stop the instant Kueue decides to preempt; it keeps running through the
		// termination grace window and only stops here, so the discarded work is measured up to this point.
		// The Failed phase is what makes this stop attributable to the preemption rather than to the
		// workload finishing its own service, and the collector always stamps one of the two terminal phases.
		{ElapsedNs: 593 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonFailed},

		{ElapsedNs: 590 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "b1"},
		{ElapsedNs: 592 * sec, Kind: "Workload", Type: EventAdmitted, Job: "b1"},
		{ElapsedNs: 592*sec + sec/2, Kind: "Pod", Type: EventPodReady, Job: "b1", ObjectUID: "pod-b1"},
		{ElapsedNs: 650 * sec, Kind: "Job", Type: EventCompleted, Job: "b1"},
	}
}

func TestReconstructReclaimAny(t *testing.T) {
	horizon := 700 * sec
	res, err := Reconstruct("Any", reclaimAnyTrace(), reclaimAnyEvents(), horizon)
	if err != nil {
		t.Fatal(err)
	}

	if res.Offered != 3 {
		t.Fatalf("offered = %d, want 3 (the trace rows)", res.Offered)
	}
	if res.Admitted != 3 {
		t.Fatalf("all three jobs were admitted, got %d", res.Admitted)
	}
	if res.Completed != 2 {
		t.Fatalf("a1 and b1 complete, a2 preempted-not-completed, got completed=%d", res.Completed)
	}
	if res.UnfinishedAtHorizon != 1 {
		t.Fatalf("a2 is unfinished at the horizon, got %d", res.UnfinishedAtHorizon)
	}
	if !res.WaitP95FullyObserved {
		t.Fatalf("all offered jobs admitted, p95 should be fully observed")
	}

	byJob := map[string]WorkloadOutcome{}
	for _, o := range res.Outcomes {
		byJob[o.Job] = o
	}
	// b1 owner admission latency: 592 - 590 = 2s.
	if byJob["b1"].AdmitLatencyNs != 2*sec {
		t.Fatalf("b1 admit latency = %d ns, want %d", byJob["b1"].AdmitLatencyNs, 2*sec)
	}
	// a2 wasted GPU-seconds: measured from PodReady to the Pod actually stopping, not the preemption
	// decision, so 1 * (593 - 2.5) = 590.5, not 588.5. The stop was observed in-horizon, so it is EXACT.
	if got := byJob["a2"].WastedGPUSeconds; got < 590.4 || got > 590.6 {
		t.Fatalf("a2 wasted GPU-seconds = %.2f, want ~590.5", got)
	}
	if byJob["a2"].WasteCensored {
		t.Fatalf("a2 stop was observed in-horizon, waste must not be censored")
	}
	if byJob["a2"].Preemptions != 1 {
		t.Fatalf("a2 should have one preemption, got %d", byJob["a2"].Preemptions)
	}
	if res.AnyWasteCensored {
		t.Fatalf("no attempt was censored in this ledger")
	}
	if res.TotalWastedGPUSeconds < 590.4 {
		t.Fatalf("total wasted should include a2's discarded work, got %.2f", res.TotalWastedGPUSeconds)
	}
}

func TestWasteNoStopChargesToHorizonAsUnattributedOccupancy(t *testing.T) {
	// No AttemptStopped was observed by the horizon. Under the fail-closed collector contract (every terminal
	// transition is observed or the run is invalidated), an absent stop means the attempt still held the GPU
	// at the horizon, so Ready->horizon is a floor on its OCCUPANCY. It is not a floor on its waste: without a
	// Failed terminal phase nothing establishes that the preemption stopped it, so the interval is charged as
	// unattributed and flagged unknown rather than presumed lost.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 2, DurationSec: 300}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 100 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
	}
	res, err := Reconstruct("Any", trace, events, 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalWastedGPUSeconds != 0 {
		t.Fatalf("exact total must be 0 when no stop was observed, got %.2f", res.TotalWastedGPUSeconds)
	}
	if res.TotalWasteLowerBoundGPUSeconds != 0 {
		t.Fatalf("lower-bound waste = %.2f, want 0: no Failed terminal phase was observed",
			res.TotalWasteLowerBoundGPUSeconds)
	}
	if res.AnyWasteCensored {
		t.Fatalf("nothing attributable is being truncated here, so the censored flag must stay off")
	}
	// 2 GPUs * (200 - 2) = 396, the horizon floor, charged as occupancy of unestablished cause.
	if got := res.TotalUnattributedOccupancyGPUSeconds; got < 395.9 || got > 396.1 {
		t.Fatalf("unattributed occupancy = %.2f, want ~396 (horizon floor)", got)
	}
	if !res.AnyAttributionUnknown {
		t.Fatalf("a run with an unobserved terminal phase must be flagged as unattributable")
	}
}

func TestReconstructRejectsOverlappingAttempts(t *testing.T) {
	// Two attempts are both Ready with neither observed to stop.
	// Both instants compared here -- one attempt's Ready and the other's Ready/stop -- come from the SAME
	// Pod watch, so unlike a decision-to-attempt comparison this ordering is trustworthy evidence, not a
	// delivery-latency artifact.
	// A row runs with Parallelism: 1, one attempt at a time, so it cannot legitimately have two attempts
	// Ready at once: the ledger disagrees with the mechanism, and Reconstruct must refuse it rather than
	// silently pairing the preemption to one of them.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 300}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 10 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-A"},
		{ElapsedNs: 20 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-B"},
		{ElapsedNs: 30 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("two attempts Ready with neither stopped must error as overlapping")
	}
}

func TestReconstructAllowsSequentialReExecutionWithOrdinalPreemptionPairing(t *testing.T) {
	// The legitimate re-execution shape: one attempt Ready at 10s and stopped at 20s, then a second Ready at
	// 30s -- sequential, not overlapping, so attemptsDoNotOverlap must let it through.
	// A Workload preemption delta names no Pod UID, so pairing uses ordinal order: with one decision and two
	// attempts it pairs to the first (only) attempt that was running when the row had a single attempt.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 300}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 10 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-A"},
		{ElapsedNs: 20 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-A", Reason: StopReasonFailed},
		{ElapsedNs: 30 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-B"},
		{ElapsedNs: 40 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
	}
	res, err := Reconstruct("Any", trace, events, 100*sec)
	if err != nil {
		t.Fatalf("sequential re-execution must not be rejected as overlapping: %v", err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	if a2.Preemptions != 1 {
		t.Fatalf("a2 preemptions = %d, want 1", a2.Preemptions)
	}
	// The preemption pairs ordinally to the first attempt (pod-A, ready 10s -> stopped Failed at 20s), so
	// its 10 GPU-seconds are charged as exact waste; the second attempt is untouched by the preemption.
	if a2.WastedGPUSeconds != 10 {
		t.Fatalf("a2 waste = %.1f, want 10 (charged to the first attempt only)", a2.WastedGPUSeconds)
	}
}

func TestReconstructRejectsPostHorizonUnknownJob(t *testing.T) {
	// Corruption is rejected whether it lands before OR after the horizon: a post-horizon event for a job
	// not in the trace must still error, not be silently skipped by the horizon rule.
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 10}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 500 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "ghost", ObjectUID: "pod-x"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("a post-horizon event for an unknown job must error")
	}
}

func TestWasteStopBeyondHorizonChargesToHorizon(t *testing.T) {
	// The stop is observed but beyond the horizon, so the attempt is KNOWN to have run through the horizon:
	// the lower bound includes the whole in-horizon interval (grace period included), still flagged.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 800}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 699 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
		{ElapsedNs: 704 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonFailed},
	}
	res, err := Reconstruct("Any", trace, events, 700*sec)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalWastedGPUSeconds != 0 {
		t.Fatalf("exact total must be 0 when the stop is beyond the horizon, got %.2f", res.TotalWastedGPUSeconds)
	}
	// 1 GPU * (700 - 2) = 698, the horizon floor including [699,700].
	if got := res.TotalWasteLowerBoundGPUSeconds; got < 697.9 || got > 698.1 {
		t.Fatalf("lower-bound waste = %.2f, want ~698 (horizon floor)", got)
	}
	if !res.AnyWasteCensored {
		t.Fatalf("a stop beyond the horizon must be flagged censored")
	}
}

func TestReconstructNeverHasNoWaste(t *testing.T) {
	// The "Never" arm: the owner waits instead of preempting, so nothing is discarded.
	trace := []TrainingTraceRow{
		{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 600},
		{Index: 1, Name: "b1", OffsetMs: 590_000, Tenant: "tenant-b", GPUCount: 1, DurationSec: 60},
	}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 600 * sec, Kind: "Job", Type: EventCompleted, Job: "a2"},
		{ElapsedNs: 590 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "b1"},
	}
	res, err := Reconstruct("Never", trace, events, 700*sec)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalWastedGPUSeconds != 0 {
		t.Fatalf("Never arm should waste nothing, got %.2f", res.TotalWastedGPUSeconds)
	}
	// b1 is submitted but never admitted, so the arm is censored: p95 must NOT be marked fully observed, or
	// the fastest admitted survivor would be reported as the arm's p95.
	if res.WaitP95FullyObserved {
		t.Fatalf("only 1 of 2 offered jobs admitted, p95 must not be fully observed")
	}
	// b1 never admitted by the horizon -> unfinished + right-censored wait, not dropped.
	var b1 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "b1" {
			b1 = o
		}
	}
	if b1.Admitted {
		t.Fatalf("b1 should not be admitted in this censored case")
	}
	if b1.CensoredWaitNs != 700*sec-590*sec {
		t.Fatalf("b1 censored wait = %d, want %d", b1.CensoredWaitNs, 700*sec-590*sec)
	}
	if res.UnfinishedAtHorizon != 1 {
		t.Fatalf("b1 is unfinished, got %d", res.UnfinishedAtHorizon)
	}
}

func TestReconstructRejectsUnknownJob(t *testing.T) {
	// An event for a job not in the frozen trace means the collector joined to something the run did not
	// offer; that is corruption, so Reconstruct refuses rather than inventing a denominator entry.
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 10}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "ghost"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("an event for a job absent from the trace must error")
	}
}

func TestReconstructRejectsMissingSubmission(t *testing.T) {
	// Every offered row must carry a Submitted (written after a successful Create). A row with no submission
	// evidence means the job was never created, which invalidates the run instead of censoring it.
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 10}}
	if _, err := Reconstruct("Any", trace, nil, 100*sec); err == nil {
		t.Fatalf("a trace row with no Submitted event must error")
	}
}

func TestReconstructToleratesAdmittedBeforeSubmitted(t *testing.T) {
	// The Submitted stamp is written by the client after its Create call returns, while admission
	// is observed on a separate Workload watch; each has independent delivery latency.
	// A reversal in their arrival order is a latency artifact, not impossible evidence.
	// Rejecting such traces would discard runs that actually executed correctly.
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 10}}
	events := []LifecycleEvent{
		{ElapsedNs: 5 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err != nil {
		t.Fatalf("admitted-before-submitted is a legal cross-watch reordering: %v", err)
	}
}

func TestReconstructRejectsUnpairedPreemption(t *testing.T) {
	// A preemption decision with no attempt running at that time cannot be attributed to discarded work;
	// silently charging zero would hide a collector gap, so it errors.
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 10}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
		{ElapsedNs: 5 * sec, Kind: "Workload", Type: EventPreempted, Job: "a1"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("a preemption with no running attempt must error")
	}
}

func TestLedgerJSONLRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := exputil.WriteJSONL(&buf, reclaimAnyEvents()); err != nil {
		t.Fatal(err)
	}
	got, err := exputil.ReadJSONL[LifecycleEvent](bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(reclaimAnyEvents()) {
		t.Fatalf("round-trip lost events: %d vs %d", len(got), len(reclaimAnyEvents()))
	}
}

// ineffectivePreemptionEvents is the case the live run actually produced: a preemption is decided while the
// borrower runs, but the borrower ignores the signal and reaches a terminal Succeeded phase on its own.
func ineffectivePreemptionEvents() []LifecycleEvent {
	return []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a1", ObjectUID: "pod-a1"},
		{ElapsedNs: 601 * sec, Kind: "Job", Type: EventCompleted, Job: "a1"},

		{ElapsedNs: 1 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 3 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 34 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
		// The workload ignored the signal and finished its own service, so this stop was NOT caused by the
		// preemption and its occupancy must not be reported as discarded work.
		{ElapsedNs: 43 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonSucceeded},

		{ElapsedNs: 34 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "b1"},
		{ElapsedNs: 34 * sec, Kind: "Workload", Type: EventAdmitted, Job: "b1"},
		{ElapsedNs: 44 * sec, Kind: "Pod", Type: EventPodReady, Job: "b1", ObjectUID: "pod-b1"},
		{ElapsedNs: 90 * sec, Kind: "Job", Type: EventCompleted, Job: "b1"},
	}
}

func TestReconstructDoesNotChargeIneffectivePreemptionAsWaste(t *testing.T) {
	res, err := Reconstruct("Any", reclaimAnyTrace(), ineffectivePreemptionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	if a2.Preemptions != 1 {
		t.Fatalf("a2 preemptions = %d, want 1", a2.Preemptions)
	}
	if a2.WastedGPUSeconds != 0 {
		t.Fatalf("a2 waste = %.1f, want 0: a Succeeded stop is not preemption-caused loss", a2.WastedGPUSeconds)
	}
	if a2.WasteLowerBoundGPUSeconds != 0 {
		t.Fatalf("a2 waste lower bound = %.1f, want 0", a2.WasteLowerBoundGPUSeconds)
	}
	// The occupancy is still real and still reported, just not as discarded work.
	if got := a2.UnattributedOccupancyGPUSeconds; got != 40 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 40", got)
	}
	if !a2.PreemptionIneffective {
		t.Fatal("a2 must be flagged as an ineffective preemption")
	}
	if res.TotalWastedGPUSeconds != 0 {
		t.Fatalf("total waste = %.1f, want 0", res.TotalWastedGPUSeconds)
	}
	if res.TotalUnattributedOccupancyGPUSeconds != 40 {
		t.Fatalf("total unattributed occupancy = %.1f, want 40", res.TotalUnattributedOccupancyGPUSeconds)
	}
	if !res.AnyPreemptionIneffective {
		t.Fatal("result must flag that a preemption was ineffective")
	}
}

func TestReconstructChargesSignalledStopAsWaste(t *testing.T) {
	events := ineffectivePreemptionEvents()
	for i := range events {
		if events[i].Job == "a2" && events[i].Type == EventAttemptStopped {
			// A workload that honors the signal terminates non-zero, which IS attributable to the preemption.
			events[i].Reason = StopReasonFailed
		}
	}
	res, err := Reconstruct("Any", reclaimAnyTrace(), events, 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	if a2.WastedGPUSeconds != 40 {
		t.Fatalf("a2 waste = %.1f, want 40 for a signalled stop", a2.WastedGPUSeconds)
	}
	if a2.UnattributedOccupancyGPUSeconds != 0 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 0", a2.UnattributedOccupancyGPUSeconds)
	}
	if a2.PreemptionIneffective {
		t.Fatal("a signalled stop is an effective preemption")
	}
}

// reExecutionEvents is the published run in full: the ineffective preemption, and then the victim being
// re-admitted and re-executing its whole service from zero because its completed attempt was never credited.
func reExecutionEvents() []LifecycleEvent {
	// After the ineffective preemption the victim was re-admitted and re-executed its whole service, which
	// is the occupancy a report showing only the first attempt would hide.
	return append(ineffectivePreemptionEvents(),
		LifecycleEvent{ElapsedNs: 45 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		LifecycleEvent{ElapsedNs: 46 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2-retry"},
		LifecycleEvent{ElapsedNs: 87 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2-retry", Reason: StopReasonSucceeded},
		LifecycleEvent{ElapsedNs: 88 * sec, Kind: "Job", Type: EventCompleted, Job: "a2"},
	)
}

func TestReconstructAccountsForReExecution(t *testing.T) {
	res, err := Reconstruct("Any", reclaimAnyTrace(), reExecutionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	if a2.Attempts != 2 {
		t.Fatalf("a2 attempts = %d, want 2", a2.Attempts)
	}
	if !a2.ReExecuted {
		t.Fatal("a2 must be flagged as re-executed")
	}
	// 40 s for the first attempt (3 s -> 43 s) plus 41 s for the second (46 s -> 87 s).
	if got := a2.TotalOccupancyGPUSeconds; got != 81 {
		t.Fatalf("a2 total occupancy = %.1f, want 81", got)
	}
	// a1 ran once and must not be flagged.
	for _, o := range res.Outcomes {
		if o.Job == "a1" && o.ReExecuted {
			t.Fatal("a1 ran a single attempt and must not be flagged as re-executed")
		}
	}
}

func TestReconstructChargesUnstoppedAttemptOccupancyToHorizon(t *testing.T) {
	// No AttemptStopped observed by the horizon.
	// Occupancy is charged only from ready (3s) to the horizon (50s), without overflow.
	// AttributionUnknown flags that the interval belongs to no established cause.
	events := ineffectivePreemptionEvents()
	var filtered []LifecycleEvent
	for _, e := range events {
		// Exclude a2's EventAttemptStopped to simulate an attempt never observed stopping.
		if e.Job == "a2" && e.Type == EventAttemptStopped {
			continue
		}
		filtered = append(filtered, e)
	}
	res, err := Reconstruct("Any", reclaimAnyTrace(), filtered, 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	// Ready at 3s, horizon at 50s: 1 GPU * (50 - 3) = 47.
	if a2.TotalOccupancyGPUSeconds != 47 {
		t.Fatalf("a2 occupancy = %.1f, want 47 (ready at 3s, charged to 50s horizon)", a2.TotalOccupancyGPUSeconds)
	}
	// An attempt with no observed terminal phase has an unestablished cause, not a truncated loss.
	if !a2.AttributionUnknown {
		t.Fatalf("a2 must be flagged attribution-unknown when no terminal phase was observed by the horizon")
	}
	if a2.WasteCensored || a2.WasteLowerBoundGPUSeconds != 0 {
		t.Fatalf("a2 must carry no waste without a Failed phase, got lb=%.1f censored=%v",
			a2.WasteLowerBoundGPUSeconds, a2.WasteCensored)
	}
}

// postHorizonStopEvents moves a2's stop to 60 s with the given terminal phase, which against a 50 s horizon
// is the exact shape of the published run: victim Ready 3 s, preempted 34 s, stop past the horizon.
func postHorizonStopEvents(reason string) []LifecycleEvent {
	events := ineffectivePreemptionEvents()
	for i := range events {
		if events[i].Job == "a2" && events[i].Type == EventAttemptStopped {
			events[i].ElapsedNs = 60 * sec
			events[i].Reason = reason
		}
	}
	return events
}

// postHorizonStopOutcome reconstructs that ledger against the 50 s horizon and returns a2's row.
func postHorizonStopOutcome(t *testing.T, reason string) WorkloadOutcome {
	t.Helper()
	res, err := Reconstruct("Any", reclaimAnyTrace(), postHorizonStopEvents(reason), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			return o
		}
	}
	t.Fatal("a2 is in the trace and must appear in the outcomes")
	return WorkloadOutcome{}
}

func TestReconstructChargesPostHorizonStopOccupancyToHorizon(t *testing.T) {
	// An in-horizon ready (3s) with a post-horizon stop (60s) observed.
	// Occupancy is charged only from ready to the horizon, same as no stop at all.
	// A stop beyond the horizon must be charged as if it still ran through the boundary.
	a2 := postHorizonStopOutcome(t, StopReasonSucceeded)
	// Ready at 3s, horizon at 50s: 1 GPU * (50 - 3) = 47.
	// Post-horizon stop does not extend the charged interval.
	if a2.TotalOccupancyGPUSeconds != 47 {
		t.Fatalf("a2 occupancy = %.1f, want 47 (ready at 3s, charged to 50s horizon, post-horizon stop)", a2.TotalOccupancyGPUSeconds)
	}
}

func TestReconstructDoesNotChargePostHorizonSucceededStopAsWaste(t *testing.T) {
	// This is the published error verbatim: the victim ignored SIGTERM, ran to natural completion and exited
	// zero, and the stop landed past the horizon. Reading only "no in-horizon stop" charged 47 GPU-seconds of
	// completed work as censored discarded work; the Succeeded reason is folded even past the horizon and
	// must refute that reading wherever it is read.
	a2 := postHorizonStopOutcome(t, StopReasonSucceeded)
	if a2.WastedGPUSeconds != 0 {
		t.Fatalf("a2 waste = %.1f, want 0: a Succeeded stop is not preemption-caused loss", a2.WastedGPUSeconds)
	}
	if a2.WasteLowerBoundGPUSeconds != 0 {
		t.Fatalf("a2 waste lower bound = %.1f, want 0 for a known-succeeded attempt", a2.WasteLowerBoundGPUSeconds)
	}
	if a2.WasteCensored {
		t.Fatal("there is no attributable waste to understate when the attempt is known to have succeeded")
	}
	// Ready at 3s charged to the 50s horizon: a lower bound on that attempt's occupancy, not on its waste.
	if a2.UnattributedOccupancyGPUSeconds != 47 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 47 (ready 3s charged to the 50s horizon)", a2.UnattributedOccupancyGPUSeconds)
	}
	if !a2.PreemptionIneffective {
		t.Fatal("a preemption whose target succeeded anyway must be flagged ineffective, in-horizon or not")
	}
}

func TestReconstructKeepsPostHorizonFailedStopCensored(t *testing.T) {
	// The contrast case: the same post-horizon stop with a Failed phase IS attributable to the preemption,
	// so it keeps the censored lower-bound treatment rather than being exempted along with the succeeded one.
	a2 := postHorizonStopOutcome(t, StopReasonFailed)
	if a2.WastedGPUSeconds != 0 {
		t.Fatalf("a2 exact waste = %.1f, want 0 when the stop is beyond the horizon", a2.WastedGPUSeconds)
	}
	if a2.WasteLowerBoundGPUSeconds != 47 {
		t.Fatalf("a2 waste lower bound = %.1f, want 47 (ready 3s charged to the 50s horizon)", a2.WasteLowerBoundGPUSeconds)
	}
	if !a2.WasteCensored {
		t.Fatal("a signalled stop beyond the horizon must stay flagged censored")
	}
	if a2.PreemptionIneffective {
		t.Fatal("a signalled stop is an effective preemption")
	}
	// The Failed phase WAS observed, so the cause is established and only the interval is truncated; this is
	// the boundary between the censored arm and the unattributed one.
	if a2.AttributionUnknown {
		t.Fatal("an observed Failed phase establishes the cause, so attribution is not unknown")
	}
	if a2.UnattributedOccupancyGPUSeconds != 0 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 0 for an attributable loss", a2.UnattributedOccupancyGPUSeconds)
	}
}

func TestReconstructRecordsAdmissionToExecutionGap(t *testing.T) {
	res, err := Reconstruct("Any", reclaimAnyTrace(), ineffectivePreemptionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var b1 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "b1" {
			b1 = o
		}
	}
	if !b1.Executed {
		t.Fatal("b1 reached Pod Ready and must be marked executed")
	}
	// Submitted at 34 s, admitted at 34 s, Pod Ready at 44 s: admission is a quota reservation and says
	// nothing about when execution began.
	if b1.AdmitLatencyNs != 0 {
		t.Fatalf("b1 admit latency = %d, want 0", b1.AdmitLatencyNs)
	}
	if b1.ReadyLatencyNs != 10*sec {
		t.Fatalf("b1 ready latency = %d, want %d", b1.ReadyLatencyNs, 10*sec)
	}
	if b1.AdmitToReadyNs != 10*sec {
		t.Fatalf("b1 admit-to-ready gap = %d, want %d", b1.AdmitToReadyNs, 10*sec)
	}
}

func TestReconstructLeavesGapZeroWhenNeverExecuted(t *testing.T) {
	events := ineffectivePreemptionEvents()
	kept := events[:0]
	for _, e := range events {
		if e.Job == "b1" && (e.Type == EventPodReady || e.Type == EventCompleted) {
			continue
		}
		kept = append(kept, e)
	}
	res, err := Reconstruct("Any", reclaimAnyTrace(), kept, 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range res.Outcomes {
		if o.Job != "b1" {
			continue
		}
		if o.Executed {
			t.Fatal("b1 never reached Pod Ready and must not be marked executed")
		}
		if o.ReadyLatencyNs != 0 || o.AdmitToReadyNs != 0 {
			t.Fatalf("b1 gaps must stay zero when unexecuted, got ready=%d gap=%d", o.ReadyLatencyNs, o.AdmitToReadyNs)
		}
	}
}

func TestReconstructUsesEarliestReadyAcrossOutOfOrderAttempts(t *testing.T) {
	// The retry attempt's PodReady is placed BEFORE the original attempt's PodReady in the event slice, even
	// though the retry's own timestamp (46s) is later than the original's (3s): this simulates two
	// independent watches delivering their PodReady deltas out of order, which attemptSeq's insertion order
	// must not be trusted to reflect.
	events := ineffectivePreemptionEvents()
	retryReady := LifecycleEvent{
		ElapsedNs: 46 * sec, Kind: "Pod", Type: EventPodReady,
		Job: "a2", ObjectUID: "pod-a2-retry",
	}
	reordered := append([]LifecycleEvent{retryReady}, events...)

	res, err := Reconstruct("Any", reclaimAnyTrace(), reordered, 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	// a2 is submitted at 1s, admitted at 2s; the earliest observed Ready is the original attempt's at 3s, not
	// the retry's at 46s, so both gaps must be computed from 3s regardless of fold order.
	if a2.ReadyLatencyNs != 2*sec {
		t.Fatalf("a2 ready latency = %d, want %d (earliest Ready, not attemptSeq[0])", a2.ReadyLatencyNs, 2*sec)
	}
	if a2.AdmitToReadyNs != 1*sec {
		t.Fatalf("a2 admit-to-ready = %d, want %d (earliest Ready, not attemptSeq[0])", a2.AdmitToReadyNs, 1*sec)
	}
}

// twoPreemptedAttemptsTrace is a single row for F5: the invariant test's fixtures otherwise always have one
// of WasteLowerBoundGPUSeconds or UnattributedOccupancyGPUSeconds at exactly zero, which reduces the combined
// invariant to two separate single-term bounds and never actually exercises the sum.
func twoPreemptedAttemptsTrace() []TrainingTraceRow {
	return []TrainingTraceRow{{Index: 0, Name: "m1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60}}
}

// twoPreemptedAttemptsEvents preempts two different attempts on the same row, SEQUENTIALLY: a row runs
// Parallelism: 1, one attempt at a time, so the second attempt's Ready only follows the first attempt's
// observed stop (attemptsDoNotOverlap enforces exactly this). The first stops Failed in-horizon (a non-zero
// exact waste, which also lower-bounds it), and the second reaches no terminal phase at all by the horizon
// (a non-zero unattributed occupancy), so both terms of the invariant are non-zero on the very same row.
func twoPreemptedAttemptsEvents() []LifecycleEvent {
	return []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "m1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "m1"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "m1", ObjectUID: "pod-m1-a"},
		{ElapsedNs: 10 * sec, Kind: "Workload", Type: EventPreempted, Job: "m1", Reason: "InCohortReclamation"},
		// The first attempt's Failed phase lands in-horizon: an exact, attributable loss.
		{ElapsedNs: 20 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "m1", ObjectUID: "pod-m1-a", Reason: StopReasonFailed},

		// Ready only after pod-m1-a's stop at 20s, so the row never runs two attempts at once.
		{ElapsedNs: 25 * sec, Kind: "Pod", Type: EventPodReady, Job: "m1", ObjectUID: "pod-m1-b"},
		{ElapsedNs: 30 * sec, Kind: "Workload", Type: EventPreempted, Job: "m1", Reason: "InCohortReclamation"},
		// The second attempt reaches no terminal phase by the horizon at all: cause unknown.
	}
}

func TestReconstructHoldsWasteAndOccupancyInvariants(t *testing.T) {
	// The two relations the field doc comments promise, asserted over the fixtures rather than left as prose:
	// a lower bound that dips below the exact total, or attributed plus unattributed work exceeding the row's
	// whole occupancy, would each mean the accounting had double-charged or lost an interval.
	cases := []struct {
		name    string
		trace   []TrainingTraceRow
		events  []LifecycleEvent
		horizon int64
	}{
		{"reclaimAny", reclaimAnyTrace(), reclaimAnyEvents(), 700 * sec},
		{"ineffectivePreemption", reclaimAnyTrace(), ineffectivePreemptionEvents(), 200 * sec},
		{"reExecution", reclaimAnyTrace(), reExecutionEvents(), 200 * sec},
		{"postHorizonSucceededStop", reclaimAnyTrace(), postHorizonStopEvents(StopReasonSucceeded), 50 * sec},
		{"postHorizonFailedStop", reclaimAnyTrace(), postHorizonStopEvents(StopReasonFailed), 50 * sec},
		{"noTerminalPhase", noTerminalPhaseTrace(), preemptedNoTerminalPhaseEvents(), 50 * sec},
		{"noTerminalPhaseCompleted", noTerminalPhaseTrace(), completedNoTerminalPhaseEvents(), 50 * sec},
		{"twoPreemptedAttemptsOneRow", twoPreemptedAttemptsTrace(), twoPreemptedAttemptsEvents(), 50 * sec},
	}
	for _, tc := range cases {
		res, err := Reconstruct("Any", tc.trace, tc.events, tc.horizon)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.name == "twoPreemptedAttemptsOneRow" {
			// F5: this is the fixture that must make BOTH terms non-zero at once, or the invariant checks
			// below reduce to two separate single-term bounds without ever exercising their sum.
			o := res.Outcomes[0]
			if o.WasteLowerBoundGPUSeconds <= 0 || o.UnattributedOccupancyGPUSeconds <= 0 {
				t.Fatalf("%s: want both WasteLowerBoundGPUSeconds and UnattributedOccupancyGPUSeconds non-zero"+
					" on the same row, got lb=%.1f unattributed=%.1f", tc.name,
					o.WasteLowerBoundGPUSeconds, o.UnattributedOccupancyGPUSeconds)
			}
		}
		for _, o := range res.Outcomes {
			// A tolerance is needed because these are float sums of ns-scaled intervals, not exact decimals.
			const eps = 1e-6
			if o.WasteLowerBoundGPUSeconds < o.WastedGPUSeconds-eps {
				t.Fatalf("%s/%s: lower bound %.6f is below the exact waste %.6f",
					tc.name, o.Job, o.WasteLowerBoundGPUSeconds, o.WastedGPUSeconds)
			}
			if o.WastedGPUSeconds+o.UnattributedOccupancyGPUSeconds > o.TotalOccupancyGPUSeconds+eps {
				t.Fatalf("%s/%s: waste %.6f + unattributed %.6f exceeds the row's occupancy %.6f",
					tc.name, o.Job, o.WastedGPUSeconds, o.UnattributedOccupancyGPUSeconds, o.TotalOccupancyGPUSeconds)
			}
			// The exact-waste relation above cannot catch a double-charge on the CENSORED path, where the
			// exact total is 0 by construction; only the lower bound is charged there, so the lower bound is
			// what has to be bounded by the row's occupancy.
			if o.WasteLowerBoundGPUSeconds+o.UnattributedOccupancyGPUSeconds > o.TotalOccupancyGPUSeconds+eps {
				t.Fatalf("%s/%s: waste lower bound %.6f + unattributed %.6f exceeds the row's occupancy %.6f",
					tc.name, o.Job, o.WasteLowerBoundGPUSeconds, o.UnattributedOccupancyGPUSeconds, o.TotalOccupancyGPUSeconds)
			}
		}
	}
}

func TestReconstructRejectsUninterpretableStopReason(t *testing.T) {
	// Attribution must fail closed rather than blacklist the one reason it knows to exempt. ClassifyPod can
	// only ever emit Succeeded or Failed, so any other value means the evidence is not interpretable, and
	// charging it as preemption waste by default is how a completed run got published as discarded work.
	for _, reason := range []string{"", "Evicted", "succeeded", "Unknown"} {
		trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60}}
		events := []LifecycleEvent{
			{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
			{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
			{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
			{ElapsedNs: 30 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2"},
			{ElapsedNs: 40 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: reason},
		}
		if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
			t.Fatalf("an AttemptStopped with reason %q must error, not default to attributable waste", reason)
		}
	}
}

func TestReconstructRejectsUninterpretablePostHorizonStopReason(t *testing.T) {
	// The same validation must apply beyond the horizon, because foldEvent deliberately folds post-horizon
	// stops and their reasons; validating only in-horizon would leave the blacklist alive on that path.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 30 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2"},
		{ElapsedNs: 400 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: "Evicted"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("a post-horizon AttemptStopped with an uninterpretable reason must error")
	}
}

func TestReconstructKeepsUnattributedTreatmentWhenNoStopObserved(t *testing.T) {
	// An attempt with no stop at all has no reason to validate, so fail-closed reason checking must not turn
	// the documented unattributed case into an error.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 30 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2"},
	}
	res, err := Reconstruct("Any", trace, events, 100*sec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AnyAttributionUnknown {
		t.Fatal("an attempt with no observed stop keeps its unattributed-occupancy treatment")
	}
	if res.AnyWasteCensored || res.TotalWasteLowerBoundGPUSeconds != 0 {
		t.Fatalf("no Failed phase means no waste of any kind, got lb=%.2f censored=%v",
			res.TotalWasteLowerBoundGPUSeconds, res.AnyWasteCensored)
	}
}

func TestReconstructRejectsStopBeforeReady(t *testing.T) {
	// A Pod's Ready and its terminal phase come from the SAME watch, so they cannot legitimately be
	// reordered; a stop before ready is impossible evidence, and charging it would subtract negative
	// occupancy that silently cancels another attempt's real cost.
	trace := []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 40 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 10 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonFailed},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("a stop observed before its own Pod's Ready must error, not yield negative occupancy")
	}
}

// noTerminalPhaseTrace is a single-row trace for the attribution cases below, where the whole question is
// what one preempted attempt's missing terminal phase may be charged as.
func noTerminalPhaseTrace() []TrainingTraceRow {
	return []TrainingTraceRow{{Index: 0, Name: "a2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 40}}
}

// preemptedNoTerminalPhaseEvents is the ledger a live run of the ignoring workload actually produces: the
// victim is Ready, a preemption is decided, and no terminal Pod phase is observed by the horizon.
func preemptedNoTerminalPhaseEvents() []LifecycleEvent {
	return []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 3 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 34 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
	}
}

// completedNoTerminalPhaseEvents adds the row's completion to that ledger, which is the refuting evidence the
// old fallback never read: the ledger says the row finished while the row was being charged for lost work.
func completedNoTerminalPhaseEvents() []LifecycleEvent {
	return append(preemptedNoTerminalPhaseEvents(),
		LifecycleEvent{ElapsedNs: 49 * sec, Kind: "Job", Type: EventCompleted, Job: "a2"},
	)
}

func TestReconstructDoesNotChargeWasteOnARowTheLedgerSaysCompleted(t *testing.T) {
	// A row cannot both have completed and have had its work discarded, yet the fallback arm rendered exactly
	// that pair on one line. Waste requires a Failed terminal phase; with no terminal phase at all the cause
	// is not establishable from Pod state, so it is unattributed, not loss.
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), completedNoTerminalPhaseEvents(), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	a2 := res.Outcomes[0]
	if !a2.Completed {
		t.Fatalf("the ledger carries a Completed event; this test is meaningless without it: %+v", a2)
	}
	if a2.WastedGPUSeconds != 0 || a2.WasteLowerBoundGPUSeconds != 0 {
		t.Fatalf("a2 waste = %.1f (lb %.1f), want 0/0: no Failed terminal phase was observed",
			a2.WastedGPUSeconds, a2.WasteLowerBoundGPUSeconds)
	}
	if a2.WasteCensored {
		t.Fatal("WasteCensored means an attributable loss was truncated; there is no attributable loss here")
	}
	// Ready at 3 s charged to the observed completion at 49 s (the completion is a tighter, sound bound than
	// the 50 s horizon): real occupancy that could not be attributed either way.
	if a2.UnattributedOccupancyGPUSeconds != 46 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 46", a2.UnattributedOccupancyGPUSeconds)
	}
	if res.TotalWasteLowerBoundGPUSeconds != 0 || res.AnyWasteCensored {
		t.Fatalf("run-level waste must be 0 and uncensored, got lb=%.1f censored=%v",
			res.TotalWasteLowerBoundGPUSeconds, res.AnyWasteCensored)
	}
	if res.TotalUnattributedOccupancyGPUSeconds != 46 {
		t.Fatalf("run-level unattributed occupancy = %.1f, want 46", res.TotalUnattributedOccupancyGPUSeconds)
	}
	if !a2.AttributionUnknown || !res.AnyAttributionUnknown {
		t.Fatalf("the missing terminal phase must be flagged unknown, got row=%v run=%v",
			a2.AttributionUnknown, res.AnyAttributionUnknown)
	}
}

func TestReconstructDoesNotChargeWasteForAnAttemptStillRunningAtTheHorizon(t *testing.T) {
	// This is the arm the study exists to characterise: a workload that ignores the signal is still running
	// at the horizon, and it is MORE LIKELY THAN NOT to go on to succeed. Presuming discarded work here is
	// backwards, so the occupancy is reported as unattributed and the cause is left unstated.
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), preemptedNoTerminalPhaseEvents(), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	a2 := res.Outcomes[0]
	if a2.Completed {
		t.Fatalf("this case has no completion evidence at all: %+v", a2)
	}
	if a2.WastedGPUSeconds != 0 || a2.WasteLowerBoundGPUSeconds != 0 || a2.WasteCensored {
		t.Fatalf("a2 waste = %.1f (lb %.1f, censored %v), want 0/0/false with no Failed terminal phase",
			a2.WastedGPUSeconds, a2.WasteLowerBoundGPUSeconds, a2.WasteCensored)
	}
	if a2.UnattributedOccupancyGPUSeconds != 47 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 47", a2.UnattributedOccupancyGPUSeconds)
	}
	// Nothing in the ledger says the attempt finished, so the preemption cannot be called ineffective either.
	if a2.PreemptionIneffective {
		t.Fatal("with no completion and no terminal phase, neither outcome may be asserted")
	}
	if !a2.AttributionUnknown {
		t.Fatal("unattributable occupancy must be flagged, not left as a silent zero in the waste column")
	}
	if !res.AnyAttributionUnknown {
		t.Fatal("the run must surface that some occupancy could not be attributed either way")
	}
}

func TestReconstructCreditsASoleAttemptWithTheRowsCompletion(t *testing.T) {
	// With exactly one attempt, the row's completion can only have happened on that attempt, so the attempt
	// was not stopped and the preemption did not take effect. That is the refuting evidence N1 named, read.
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), completedNoTerminalPhaseEvents(), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	a2 := res.Outcomes[0]
	if a2.Attempts != 1 {
		t.Fatalf("a2 attempts = %d, want 1; the credit rule is gated on a sole attempt", a2.Attempts)
	}
	if !a2.PreemptionIneffective {
		t.Fatal("the row completed on its only attempt, so the preemption stopped nothing")
	}
	if !res.AnyPreemptionIneffective {
		t.Fatal("the run must surface that a preemption was ineffective")
	}
}

// TestReconstructChargesAPreemptedAttemptsOwnFailedStopEvenWhenTheRowLaterCompletes checks a DIFFERENT rule
// than its name once suggested: this attempt has its own observed Failed stop, so it is resolved in the
// "a.stopped" branch of chargeWaste and never reaches the "no stop observed" fallback where the
// len(attemptSeq)==1 credit gate lives. What this pins is that the row's later completion (which belongs to
// the retry) is not read back onto an already-resolved earlier attempt. The credit-gate rule itself -- that a
// completion may only be credited when there is exactly one attempt -- is pinned by
// TestReconstructDoesNotCreditACompletionToAnUnstoppedAttemptOfSeveral below.
func TestReconstructChargesAPreemptedAttemptsOwnFailedStopEvenWhenTheRowLaterCompletes(t *testing.T) {
	events := append(preemptedNoTerminalPhaseEvents(),
		// The first attempt is observed to stop Failed before the retry becomes Ready.
		LifecycleEvent{ElapsedNs: 36 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonFailed},
		// The retry becomes Ready after both the preemption decision (34 s) and the first attempt's stop
		// (36 s), so the pairing stays unambiguous and the two attempts are sequential, not concurrent.
		LifecycleEvent{ElapsedNs: 40 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2-retry"},
		LifecycleEvent{ElapsedNs: 49 * sec, Kind: "Job", Type: EventCompleted, Job: "a2"},
	)
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), events, 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	a2 := res.Outcomes[0]
	if a2.Attempts != 2 || !a2.Completed {
		t.Fatalf("this test needs a completed row with two attempts, got attempts=%d completed=%v", a2.Attempts, a2.Completed)
	}
	if a2.PreemptionIneffective {
		t.Fatal("the row's completion belongs to the retry, not the already-Failed preempted attempt, and must not be credited to it")
	}
	// The preempted attempt has its own Failed evidence (Ready 3 s, stop 36 s): 1 GPU * (36 - 3) = 33 exact,
	// attributable waste, fully explained without reference to the row's later completion.
	if a2.WastedGPUSeconds != 33 || a2.WasteLowerBoundGPUSeconds != 33 || a2.WasteCensored {
		t.Fatalf("a2 waste = %.1f (lb %.1f, censored %v), want 33/33/false: an observed in-horizon Failed stop",
			a2.WastedGPUSeconds, a2.WasteLowerBoundGPUSeconds, a2.WasteCensored)
	}
	// The retry was never preempted, so it contributes nothing here, and the preempted attempt's own Failed
	// stop already explains its loss, so nothing is left unattributed.
	if a2.UnattributedOccupancyGPUSeconds != 0 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 0 (the preempted attempt's Failed stop fully"+
			" explains its own loss)", a2.UnattributedOccupancyGPUSeconds)
	}
}

// TestReconstructDoesNotCreditACompletionToAnUnstoppedAttemptOfSeveral is the shape that actually reaches the
// len(t.attemptSeq)==1 gate in chargeWaste's default case: the FIRST attempt is resolved by its own Failed
// stop (so it never touches the gate), but the SECOND -- and last -- attempt has no observed stop at all, so
// only the gate stands between the row's later completion and a wrongful credit to that unresolved attempt.
// With two attempts the completion cannot say which one it belongs to, so it must not be credited here either.
func TestReconstructDoesNotCreditACompletionToAnUnstoppedAttemptOfSeveral(t *testing.T) {
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 3 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		{ElapsedNs: 18 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
		// The first attempt has its own Failed evidence, so it is resolved before the gate is ever reached.
		{ElapsedNs: 20 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonFailed},
		// The retry becomes Ready after the first attempt's own stop, so the two attempts stay sequential.
		{ElapsedNs: 25 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2-retry"},
		// A second preemption decision, so the retry is also a preemption target with no terminal Pod phase.
		{ElapsedNs: 40 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},
		{ElapsedNs: 45 * sec, Kind: "Job", Type: EventCompleted, Job: "a2"},
	}
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), events, 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	a2 := res.Outcomes[0]
	if a2.Attempts != 2 || !a2.Completed {
		t.Fatalf("this test needs a completed row with two attempts, got attempts=%d completed=%v", a2.Attempts, a2.Completed)
	}
	// The retry (the last attempt) has no terminal Pod phase, so this is the one line where the gate's answer
	// is visible: crediting it would report the retry as stopped by the second preemption when Pod state
	// cannot say that.
	if a2.PreemptionIneffective {
		t.Fatal("two attempts means the completion cannot be attributed to either one; it must not be credited")
	}
	if !a2.UncreditedAttributionUnknown {
		t.Fatal("the retry's occupancy must be reported as uncredited, unattributed loss, not silently dropped")
	}
	// The retry's occupancy (Ready 25 s -> completion 45 s) is charged as unattributed, and specifically as
	// UNCREDITED unattributed, which is the field the wrongful-credit bug would leave at zero.
	if a2.UncreditedAttributionUnknownOccupancyGPUSeconds != 20 {
		t.Fatalf("uncredited unattributed occupancy = %.1f, want 20 (the retry's Ready-to-completion span)",
			a2.UncreditedAttributionUnknownOccupancyGPUSeconds)
	}
	// The first attempt's own Failed evidence (Ready 3 s, stop 20 s) is unaffected by the gate either way.
	if a2.WastedGPUSeconds != 17 || a2.WasteLowerBoundGPUSeconds != 17 {
		t.Fatalf("a2 waste = %.1f (lb %.1f), want 17/17: the first attempt's own observed Failed stop",
			a2.WastedGPUSeconds, a2.WasteLowerBoundGPUSeconds)
	}
}

// TestReconstructBoundsUnstoppedAttemptOccupancyAtObservedCompletion is F3: a Job's Complete condition is
// only observed after its Pod has terminated, so once the ledger holds a completion the horizon is no longer
// the tightest available bound on an unstopped attempt's occupancy -- charging past the completion reports
// GPU-seconds the row could not still have been holding.
func TestReconstructBoundsUnstoppedAttemptOccupancyAtObservedCompletion(t *testing.T) {
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), completedNoTerminalPhaseEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	a2 := res.Outcomes[0]
	// Ready at 3s, completion observed at 49s: 1 GPU * (49 - 3) = 46, not 1 * (200 - 3) = 197 from the horizon.
	if got := a2.TotalOccupancyGPUSeconds; got != 46 {
		t.Fatalf("a2 total occupancy = %.1f, want 46 (bounded by the observed completion at 49s, not 197"+
			" from the 200s horizon)", got)
	}
	if got := a2.UnattributedOccupancyGPUSeconds; got != 46 {
		t.Fatalf("a2 unattributed occupancy = %.1f, want 46 (bounded by the observed completion, not 197)", got)
	}
}

func TestReconstructRejectsReadyBeforeSubmitted(t *testing.T) {
	// A Pod cannot become Ready before its own row's Submitted, which is stamped locally right after Create
	// and is never watch-observed, so this ordering is impossible evidence rather than legal reordering.
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 10}}
	events := []LifecycleEvent{
		{ElapsedNs: 5 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Pod", Type: EventPodReady, Job: "a1", ObjectUID: "pod-a1"},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatalf("Pod Ready before Submitted must error")
	}
}

// TestReconstructToleratesCrossWatchReordering pins the rule the design of record states: Workload, Job and
// Pod are three independent watches, so an observed order that violates causal expectation is delivery
// latency, not impossible evidence, and must not discard a valid run.
func TestReconstructToleratesCrossWatchReordering(t *testing.T) {
	// The Job watch delivered Complete before the Workload watch delivered Admitted.
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 30 * sec, Kind: "Job", Type: EventCompleted, Job: "a1"},
		{ElapsedNs: 31 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},

		{ElapsedNs: 1 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 3 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},

		{ElapsedNs: 5 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "b1"},
		{ElapsedNs: 6 * sec, Kind: "Workload", Type: EventAdmitted, Job: "b1"},
	}
	if _, err := Reconstruct("A-honor", reclaimAnyTrace(), events, 200*sec); err != nil {
		t.Fatalf("legal cross-watch reordering must not invalidate a run: %v", err)
	}
}

// TestReconstructStillRejectsACompletionWithNoAdmission keeps the check that holds no matter how deliveries
// were ordered: a job that never has admission evidence at all cannot have completed.
func TestReconstructStillRejectsACompletionWithNoAdmission(t *testing.T) {
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 30 * sec, Kind: "Job", Type: EventCompleted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 5 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "b1"},
	}
	if _, err := Reconstruct("A-honor", reclaimAnyTrace(), events, 200*sec); err == nil {
		t.Fatal("a completion with no admission evidence at all must still be an error")
	}
}

// TestReconstructPairsAPromptlyStoppedVictim is the failure the honoring arm would hit: the victim stops so
// fast that the Pod watch delivers its terminal state before the Workload watch delivers the preemption.
func TestReconstructPairsAPromptlyStoppedVictim(t *testing.T) {
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},

		{ElapsedNs: 1 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 2 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 3 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
		// Pod terminal observed at 43 s, preemption observed at 44 s — reversed by delivery latency.
		{ElapsedNs: 43 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a2", ObjectUID: "pod-a2", Reason: StopReasonFailed},
		{ElapsedNs: 44 * sec, Kind: "Workload", Type: EventPreempted, Job: "a2", Reason: "InCohortReclamation"},

		{ElapsedNs: 40 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "b1"},
		{ElapsedNs: 44 * sec, Kind: "Workload", Type: EventAdmitted, Job: "b1"},
	}
	res, err := Reconstruct("A-honor", reclaimAnyTrace(), events, 200*sec)
	if err != nil {
		t.Fatalf("a promptly stopped victim must pair to its preemption: %v", err)
	}
	for _, o := range res.Outcomes {
		if o.Job != "a2" {
			continue
		}
		if o.Preemptions != 1 {
			t.Fatalf("a2 preemptions = %d, want 1", o.Preemptions)
		}
		// Ready 3 s -> stop 43 s, attributable because the stop was Failed.
		if o.WastedGPUSeconds != 40 {
			t.Fatalf("a2 waste = %.1f, want 40", o.WastedGPUSeconds)
		}
	}
}

// reorderedCompletionEvents has the Job's completion observed BEFORE its own Pod's Ready.
//
// That is not corrupt data. Job and Pod arrive on independent watches and the ledger says outright that
// comparing their observed instants proves nothing about what happened first, so this ordering is legal and
// a run carrying it must still be readable.
// reorderedTrace declares exactly the two rows reorderedCompletionEvents drives, so the reconstruction is
// not refused for a third row the fixture never submits.
func reorderedTrace() []TrainingTraceRow {
	return []TrainingTraceRow{
		{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 600},
		{Index: 1, Name: "a2", OffsetMs: 1_000, Tenant: "tenant-a", GPUCount: 1, DurationSec: 600},
	}
}

func reorderedCompletionEvents() []LifecycleEvent {
	return []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 0, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "a1", ObjectUID: "pod-a1"},
		{ElapsedNs: 40 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a1", ObjectUID: "pod-a1", Reason: StopReasonSucceeded},
		{ElapsedNs: 41 * sec, Kind: "Job", Type: EventCompleted, Job: "a1"},

		// a2's completion lands at 20s while its Pod's Ready is only observed at 30s.
		{ElapsedNs: 1 * sec, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a2"},
		{ElapsedNs: 5 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a2"},
		{ElapsedNs: 20 * sec, Kind: "Job", Type: EventCompleted, Job: "a2"},
		{ElapsedNs: 30 * sec, Kind: "Pod", Type: EventPodReady, Job: "a2", ObjectUID: "pod-a2"},
	}
}

// A completion observed before its own attempt's Ready must charge zero, never a negative interval.
//
// Negative occupancy does not merely misreport the attempt it belongs to: the row's total is a sum, so a
// negative term silently cancels another attempt's real cost and the report comes out lower with nothing
// flagged. That is the failure mode this lab exists to refuse.
//
// Mutation that turns this red: return end without clamping it up to readyNs in occupancyEnd.
func TestOccupancyIsNeverNegativeWhenWatchesReorder(t *testing.T) {
	res, err := Reconstruct("Any", reorderedTrace(), reorderedCompletionEvents(), 200*sec)
	if err != nil {
		t.Fatalf("a legal cross-watch reordering must still reconstruct: %v", err)
	}

	var total float64
	for _, o := range res.Outcomes {
		if o.TotalOccupancyGPUSeconds < 0 {
			t.Fatalf("%s charged %.1f GPU-seconds; occupancy can never be negative",
				o.Job, o.TotalOccupancyGPUSeconds)
		}
		total += o.TotalOccupancyGPUSeconds
	}

	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	// Ready at 30s against a completion seen at 20s is zero-width evidence, not minus ten seconds.
	if a2.TotalOccupancyGPUSeconds != 0 {
		t.Fatalf("a2 charged %.1f, want 0 for an interval whose end was observed before its start",
			a2.TotalOccupancyGPUSeconds)
	}
	// The control: a1's ordinary 2s -> 40s attempt must still be charged in full, or the clamp has become
	// "charge nothing" and the measurement is gone rather than corrected.
	if total != 38 {
		t.Fatalf("total occupancy = %.1f, want 38 from a1 alone", total)
	}
}

// A retry Pod that becomes Ready AFTER the horizon and stops shortly after is an ordinary consequence of
// where the window closes, not a broken ledger.
//
// The horizon gate folds a post-horizon AttemptStopped deliberately — an attempt still running when the
// window closed needs its end to charge occupancy correctly — while dropping every other post-horizon event,
// PodReady included. So the stop arrived with no attempt to attach to and the whole arm was refused under a
// reason that reads as a malformed sequence. Both events are deliverable in the interval between the horizon
// being reached and the watches being cancelled.
//
// Mutation that turns this red: error on an unknown Pod regardless of when the stop happened.
func TestReconstructIgnoresAPostHorizonStopForAPostHorizonAttempt(t *testing.T) {
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 300}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
		{ElapsedNs: 10 * sec, Kind: "Pod", Type: EventPodReady, Job: "a1", ObjectUID: "pod-1"},
		{ElapsedNs: 40 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a1", ObjectUID: "pod-1", Reason: StopReasonSucceeded},
		// Both past the 100s horizon: the retry's readiness is dropped by the gate, and its stop must be too.
		{ElapsedNs: 110 * sec, Kind: "Pod", Type: EventPodReady, Job: "a1", ObjectUID: "pod-2"},
		{ElapsedNs: 115 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a1", ObjectUID: "pod-2", Reason: StopReasonSucceeded},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err != nil {
		t.Fatalf("an ordinary post-horizon retry refused the whole arm: %v", err)
	}
}

// The half that must keep erroring. A stop INSIDE the window for a Pod never seen Ready inside it is a
// malformed sequence, and folding it silently would charge occupancy from an instant nothing established.
//
// Mutation that turns this red: return nil for an unknown Pod at any elapsed time.
func TestReconstructStillRefusesAnInHorizonStopWithNoReadiness(t *testing.T) {
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 300}}
	events := []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "a1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "a1"},
		{ElapsedNs: 40 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "a1", ObjectUID: "pod-ghost", Reason: StopReasonSucceeded},
	}
	if _, err := Reconstruct("Any", trace, events, 100*sec); err == nil {
		t.Fatal("a stop inside the window for a Pod never seen Ready was folded as if it were ordinary")
	}
}

// stamped builds an event carrying both clocks: when the collector heard about it, and the component's own.
func stamped(kind, job, reason string, ev EventType, elapsedNs, stampNs int64) LifecycleEvent {
	e := LifecycleEvent{Kind: kind, Job: job, Reason: reason, Type: ev, ElapsedNs: elapsedNs, ObjectUID: job + "-uid"}
	if stampNs != 0 {
		s := stampNs
		e.ComponentStampUnixNanos = &s
	}
	return e
}

// The reconstruction must carry the interval on the components' own clocks beside the arrival-based one,
// because they bound different error sources and the pair is what separates watch jitter from the cluster
// behaving differently.
//
// The numbers are the shape the recorded runs actually took: an admission and a readiness whose ARRIVALS are
// 30.687 s apart while the components' own transitions are exactly 31 s apart, the arrival figure carrying
// the two watches' delivery lag and the stamp figure carrying a second of truncation instead.
//
// Mutation that turns this red: stop emitting AdmitToReadyStampNs in Reconstruct, or take the stamp from the
// wrong endpoint.
func TestReconstructCarriesTheIntervalOnBothClocks(t *testing.T) {
	trace := []TrainingTraceRow{{Index: 0, Name: "b1", OffsetMs: 0, Tenant: "tenant-b", GPUCount: 1, DurationSec: 60}}
	const admitArrival, readyArrival = int64(24_217_000_000), int64(54_904_000_000)
	const admitStamp, readyStamp = int64(1_700_000_024_000_000_000), int64(1_700_000_055_000_000_000)

	res, err := Reconstruct("A-ignore", trace, []LifecycleEvent{
		stamped("MLTrainingJob", "b1", "", EventSubmitted, 0, 0),
		stamped("Workload", "b1", "Admitted", EventAdmitted, admitArrival, admitStamp),
		stamped("Pod", "b1", "Ready", EventPodReady, readyArrival, readyStamp),
	}, 150_000_000_000)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	o := res.Outcomes[0]
	if o.AdmitToReadyNs != readyArrival-admitArrival {
		t.Fatalf("arrival interval = %d, want %d", o.AdmitToReadyNs, readyArrival-admitArrival)
	}
	if o.AdmitToReadyStampNs == nil {
		t.Fatal("both components stamped their transitions and the reconstruction carried no stamp interval; " +
			"the run's scatter could then not be told from the cluster behaving differently")
	}
	if *o.AdmitToReadyStampNs != readyStamp-admitStamp {
		t.Fatalf("stamp interval = %d, want %d", *o.AdmitToReadyStampNs, readyStamp-admitStamp)
	}
	// The two must be different here, or the test would pass against a build that returned the arrival figure
	// from both.
	if *o.AdmitToReadyStampNs == o.AdmitToReadyNs {
		t.Fatal("the fixture no longer distinguishes the two clocks")
	}
}

// A run whose components published no transition times reports the arrival figure alone rather than a zero
// interval, which would claim the owner was running the instant it was admitted.
func TestReconstructReportsNoStampIntervalWhenTheComponentsPublishedNone(t *testing.T) {
	trace := []TrainingTraceRow{{Index: 0, Name: "b1", OffsetMs: 0, Tenant: "tenant-b", GPUCount: 1, DurationSec: 60}}
	res, err := Reconstruct("A-ignore", trace, []LifecycleEvent{
		stamped("MLTrainingJob", "b1", "", EventSubmitted, 0, 0),
		stamped("Workload", "b1", "Admitted", EventAdmitted, 24_217_000_000, 0),
		stamped("Pod", "b1", "Ready", EventPodReady, 54_904_000_000, 0),
	}, 150_000_000_000)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if s := res.Outcomes[0].AdmitToReadyStampNs; s != nil {
		t.Fatalf("an unstamped run reported a stamp interval of %d", *s)
	}
}

// A re-executed row has several attempts, the earliest-observed Ready is the minimum over them, and the
// STAMP must come from that same attempt.
//
// If it does not, the interval is built from two different Pods: one attempt's readiness on the components'
// clocks against another's on the collector's. The two figures would then disagree for a reason that has
// nothing to do with watch lag, which is the one thing the pair exists to tell apart.
//
// attemptSeq is observation-insertion order rather than chronological order, so this is reachable: the
// fixture below folds the LATER attempt first, exactly as a reordered watch would deliver it.
func TestTheStampFollowsTheAttemptItsArrivalCameFrom(t *testing.T) {
	trace := []TrainingTraceRow{{Index: 0, Name: "b1", OffsetMs: 0, Tenant: "tenant-b", GPUCount: 1, DurationSec: 60}}
	const admitArrival, admitStamp = int64(10_000_000_000), int64(1_700_000_010_000_000_000)
	// A row runs one attempt at a time, so the first must stop before the second is Ready — the
	// reconstruction refuses overlapping attempts and is right to.
	early := stamped("Pod", "b1", "Ready", EventPodReady, 20_000_000_000, 1_700_000_020_000_000_000)
	early.ObjectUID = "attempt-early"
	earlyStop := stamped("Pod", "b1", "Failed", EventAttemptStopped, 30_000_000_000, 1_700_000_030_000_000_000)
	earlyStop.ObjectUID = "attempt-early"
	late := stamped("Pod", "b1", "Ready", EventPodReady, 40_000_000_000, 1_700_000_040_000_000_000)
	late.ObjectUID = "attempt-late"

	// The LATER attempt is folded first, exactly as a reordered watch would deliver it: attemptSeq is
	// observation-insertion order, not chronological order, which is what makes the mismatch reachable.
	res, err := Reconstruct("A-ignore", trace, []LifecycleEvent{
		stamped("MLTrainingJob", "b1", "", EventSubmitted, 0, 0),
		stamped("Workload", "b1", "Admitted", EventAdmitted, admitArrival, admitStamp),
		late, early, earlyStop,
	}, 150_000_000_000)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	o := res.Outcomes[0]
	if o.AdmitToReadyNs != 10_000_000_000 {
		t.Fatalf("arrival interval = %d, want the EARLIEST attempt's 10s", o.AdmitToReadyNs)
	}
	if o.AdmitToReadyStampNs == nil {
		t.Fatal("a re-executed row carried no stamp interval")
	}
	if *o.AdmitToReadyStampNs != 10_000_000_000 {
		t.Fatalf("stamp interval = %d ns, want 10s: it was taken from a different attempt than the arrival "+
			"figure, so the two clocks are measuring two different Pods", *o.AdmitToReadyStampNs)
	}
}
