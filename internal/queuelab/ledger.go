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
	"slices"
	"sort"
)

// LifecycleEvent is one observed transition in the run's authoritative ledger.
//
// The list/watch collector (a later slice) records these for MLTrainingJob, Job, Workload, and Pod.
// Workload and Pod events are the primary evidence; the MLTrainingJob phase is a derived cross-check.
//
// Every latency, occupancy, and waste number the report makes is reconstructed from these events, never
// from an aggregated Prometheus histogram.
type LifecycleEvent struct {
	// ElapsedNs is the transition time as a monotonic offset from the run's t0.
	//
	// The review caught that a wall-clock UnixNano is not monotonic (it drops Go's monotonic component), so
	// the collector stamps this elapsed offset as the timing authority and keeps the wall clock separately.
	ElapsedNs int64 `json:"elapsedNs"`
	// WallUnixNanos is the wall-clock observation time, kept for provenance only, never for arithmetic.
	WallUnixNanos int64 `json:"wallUnixNanos,omitempty"`
	// Kind is the object kind: "Workload" | "Pod" | "Job" | "MLTrainingJob".
	Kind string `json:"kind"`
	// Type is the transition within the closed vocabulary below.
	Type EventType `json:"type"`
	// ExitCode is the terminated container's status on an AttemptStopped event, absent everywhere else and
	// absent when no single container's status could be read.
	//
	// Without it the ledger cannot tell the two arms apart at the moment that matters: Reason is the Pod
	// phase, and "Failed" covers both a workload that honoured SIGTERM and exited promptly and one that
	// ignored it until the grace period ran out and was killed. The canary qualifies that contrast by exit
	// status; until this field the run could not observe it.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Iterations is how much work the attempt had completed when it stopped, absent when no single container's
	// terminated status carried a count. It is what makes a discarded GPU-second a discarded unit of work.
	Iterations *int `json:"iterations,omitempty"`
	// WorkloadKind and DeviceStatus are the workload's own account of which loop produced Iterations, and they
	// are in the LEDGER rather than derived at write time on purpose.
	//
	// The record's workload provenance is the claim "this run's device was used", and a claim whose support
	// lives only in the writer is a claim the artifact cannot be audited for. Carrying the tokens here means a
	// reader holding nothing but the JSON can re-derive the provenance from the same evidence the writer had.
	//
	// Absent together with Iterations, for the same reason: they are three readings of one message.
	WorkloadKind string `json:"workloadKind,omitempty"`
	DeviceStatus string `json:"deviceStatus,omitempty"`
	// Job is the trace job name this event belongs to, resolved by the collector through the UID chain.
	Job string `json:"job"`
	// ComponentStampUnixNanos is the cluster component's own wall clock for the state this event describes:
	// the kubelet's finishedAt for a stop, its Ready condition transition for a running Pod, Kueue's Admitted
	// transition for an admission. Absent when the component published none.
	//
	// It is beside ElapsedNs, which is when this collector HEARD about the state, and the gap between them is
	// what ObservedSkewNs carries. Both are kept because a reader holding only one of them cannot tell a slow
	// cluster from a slow watch.
	ComponentStampUnixNanos *int64 `json:"componentStampUnixNanos,omitempty"`
	// ObservedSkewNs is the gap between the kubelet's stamp and this collector's arrival time, in the
	// collector's own frame.
	//
	// It is deliberately NOT called a delivery lag, and the difference matters. The two times come from
	// different machines with unsynchronised clocks, and the kubelet's is truncated to the second, so this
	// number is (true propagation) + (clock offset between the two hosts) + (up to a second of truncation)
	// and nothing here can separate the three. Calling it a lag would claim a measurement of propagation
	// that this harness cannot make without clock synchronisation or distributed tracing.
	//
	// What it IS good for is a bound: an interval whose endpoints carry skew of this size is not resolved
	// below it. That is the use it is put to, and the only one it supports.
	//
	// It can come out negative, which is reported rather than clamped — a negative value is direct evidence
	// of clock offset, and hiding it would turn a known unknown into an unknown one.
	ObservedSkewNs *int64 `json:"observedSkewNs,omitempty"`
	// Tenant and GPUCount used to sit here, described as the submitting tenant and the quota occupancy is
	// weighted by. Nothing ever wrote them: LedgerBuilder.Observe is the only constructor of a LifecycleEvent
	// and it never set either, so every event in every record this build has ever produced carried "" and 0
	// under two sentences saying otherwise.
	//
	// They are removed rather than populated because the builder is a pure observer of Kubernetes objects and
	// neither value is one: both are properties of the TRACE, which the record already carries as arm and
	// dose, and Reconstruct weights from the trace-seeded timeline for exactly that reason. Populating them
	// would mean handing the builder the trace so it could copy back what the reader already has.
	// ObjectUID is the observed object's UID; for Pod events it is the attempt identity that preemption
	// waste is paired on, so a duplicate delta or an unrelated Pod stop cannot truncate another attempt.
	ObjectUID string `json:"objectUID,omitempty"`
	// ResourceVersion and Reason carry the condition provenance so a duplicate delta or the preemption
	// mechanism (e.g. InCohortReclamation) can be checked.
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// EventType is the closed vocabulary of lifecycle transitions the ledger records.
type EventType string

const (
	// EventSubmitted is the MLTrainingJob API acceptance (the offered-work clock start).
	//
	// It is written by the submitting path right after a successful Create, not by a watch, so unlike every
	// other event here its instant is a local fact rather than a delivery time. That is what lets Reconstruct
	// treat a Pod Ready before it as impossible evidence: the comparison holds because only one side of it
	// came off a watch. The admission comparison is NOT of that kind — Admitted is watch-observed, so an
	// admission delivered before this event is legal cross-watch reordering and is folded, not refused.
	EventSubmitted EventType = "Submitted"
	// EventAdmitted is the Workload reaching Admitted=True.
	EventAdmitted EventType = "Admitted"
	// EventPodReady is the training Pod becoming Ready (execution start); it carries the Pod UID as the
	// attempt identity.
	EventPodReady EventType = "PodReady"
	// EventPreempted is when Kueue DECIDES to preempt the Workload (Preempted=True).
	//
	// It is the decision, not the moment execution stops; the two are separated because the discarded work
	// continues between the decision and the Pod actually terminating.
	EventPreempted EventType = "Preempted"
	// EventAttemptStopped is when the preempted Pod actually terminates (terminal/deleted observation, not
	// the first deletionTimestamp which merely starts the grace period); it carries the Pod UID.
	//
	// Waste is measured up to here, not up to the preemption decision, so the work done during the grace
	// window is not silently dropped from the discarded total.
	EventAttemptStopped EventType = "AttemptStopped"
	// EventCompleted is the Job reaching successful completion.
	EventCompleted EventType = "Completed"
)

// StopReasonSucceeded and StopReasonFailed are the terminal Pod phases the collector stamps onto an
// AttemptStopped event.
//
// They are the ONLY evidence in the ledger that distinguishes "the preemption stopped this attempt" from
// "this attempt finished its own service while marked preempted", and the second case is not discarded work.
const (
	StopReasonSucceeded = "Succeeded"
	StopReasonFailed    = "Failed"
)

// attempt is one Pod's execution, keyed by Pod UID, so preemption waste is paired to the exact Pod that
// ran rather than to whichever stop happened to be recorded next.
type attempt struct {
	// readyStampNs is the KUBELET's own clock for the readiness this attempt reached, beside readyNs which
	// is when the collector heard about it. Zero when the component published none.
	readyStampNs int64
	uid          string
	readyNs      int64
	stopped      bool
	stopNs       int64
	stopReason   string // the terminal Pod phase, which is what makes a stop attributable or not
}

// jobTimeline reconstructs one job's story from its events. It is SEEDED from the frozen trace row, not
// discovered from whatever events arrived, so a job whose events were all missed still appears (censored)
// in the denominators rather than silently vanishing and flattering the worse arm.
type jobTimeline struct {
	tenant      string
	gpuCount    int
	durationSec int

	submitted   bool
	submittedNs int64
	admitted    bool
	admitNs     int64
	// admitStampNs is KUEUE's own clock for the admission, beside admitNs which is when the collector heard
	// about it. Zero when the component published none.
	admitStampNs int64
	completed    bool
	completedNs  int64

	attempts    map[string]*attempt
	attemptSeq  []string // insertion order of attempt UIDs, for deterministic pairing
	preemptedNs []int64
}

// WorkloadOutcome is one job's reconstructed, censoring-aware result.
type WorkloadOutcome struct {
	Job    string
	Tenant string
	// AdmitLatencyNs is submitted -> first admitted; valid only when Admitted is true.
	AdmitLatencyNs int64
	Admitted       bool
	// Executed is true when the row reached Pod Ready, which is the harness's execution-start proxy.
	//
	// It is NOT service restoration: a busybox sleeper has no application readiness contract.
	Executed bool
	// ReadyLatencyNs is submitted -> first observed Pod Ready, a client-observed propagation gap; valid only
	// when Executed is true.
	ReadyLatencyNs int64
	// AdmitToReadyStampNs is the same interval measured on the CLUSTER COMPONENTS' own clocks -- Kueue's
	// Admitted transition to the kubelet's Ready transition -- rather than on this collector's.
	//
	// nil when either component published no timestamp. It exists because the arrival-based figure below
	// scattered across a second between otherwise identical runs while this one did not move at all, which
	// is how the scatter was identified as watch jitter rather than as the cluster behaving differently.
	AdmitToReadyStampNs *int64
	// AdmitToReadyNs is admission -> first observed Pod Ready, a client-observed propagation gap; valid only
	// when the row was both Admitted and Executed.
	//
	// It exists because admission is a QUOTA RESERVATION, not the start of execution: a preempted victim can
	// still hold the physical device while the owner's Workload is already admitted, so reporting admission
	// alone overstates how quickly the owner was actually restored.
	AdmitToReadyNs int64
	Completed      bool
	// Preemptions counts how many times the job was preempted.
	Preemptions int
	// WastedGPUSeconds is the EXACT discarded work ATTRIBUTABLE to preemption: sum over preempted attempts
	// whose Pod reached a FAILED terminal phase at or before the horizon, of gpuCount * (stop - ready).
	//
	// A Failed terminal phase is evidence that the attempt was cut short, not evidence that the PREEMPTION is
	// what cut it short: an unrelated crash after the preemption decision reaches the same Failed phase and is
	// charged identically, so this field rests on the assumption that a Failed phase following a preemption
	// decision was caused by it, an assumption the ledger has no independent way to confirm or refute. Nothing
	// else in this file may charge waste. A workload that ignores SIGTERM keeps running after the preemption
	// decision and reaches a terminal Succeeded phase on its own; charging that run as discarded work reports
	// a completed job as destroyed progress, which is exactly the error this field's earlier definition
	// produced.
	WastedGPUSeconds float64
	// UnattributedOccupancyGPUSeconds is the occupancy of preempted attempts whose loss could NOT be
	// established from Pod state: those observed to terminate as Succeeded (so their work was not cut short),
	// and those with no terminal phase observed at all (so nothing says either way). It is reported, never
	// folded into waste.
	//
	// It MIXES an exact value with a horizon-truncated lower bound and does not distinguish them: an attempt
	// whose Succeeded stop was observed in-horizon contributes its exact interval, while a post-horizon stop
	// or an unobserved one contributes only ready->horizon. AttributionUnknown implies at least one truncated
	// contribution; the converse does not hold, so read the whole quantity as ">=" whenever the run has any
	// ineffective preemption. A separate censoring flag was rejected in favour of this note because the field
	// set is already the hard part of reading this report.
	UnattributedOccupancyGPUSeconds float64
	// PreemptionIneffective is true when at least one preemption decision is KNOWN not to have stopped its
	// target: the attempt reached a Succeeded terminal phase, or the row completed on what was its only
	// attempt. Either way the platform decided to reclaim and the workload did not comply.
	PreemptionIneffective bool
	// WasteLowerBoundGPUSeconds is >= WastedGPUSeconds: it also counts attempts whose FAILED terminal phase
	// was truncated by the horizon, charged only up to the horizon. It is a lower bound, never exact.
	WasteLowerBoundGPUSeconds float64
	// WasteCensored is true when at least one preempted attempt's Failed terminal phase fell outside the
	// horizon, so an ATTRIBUTABLE loss is being reported over a truncated interval and the exact total
	// understates it.
	//
	// It is deliberately NOT set when no terminal phase was observed at all: that is not a truncated loss but
	// an unestablished cause, which is what AttributionUnknown is for.
	WasteCensored bool
	// AttributionUnknown is true when a preempted attempt reached no observed terminal phase, so Pod state
	// cannot say whether the preemption stopped it or it ran on and succeeded.
	//
	// Its interval is charged to UnattributedOccupancyGPUSeconds rather than to waste, because the design's
	// rule is to report an unestablishable cause as unattributed; the old fallback presumed loss instead, on
	// exactly the arm where the workload is more likely than not to succeed.
	AttributionUnknown bool
	// UncreditedAttributionUnknownOccupancyGPUSeconds is the subset of UnattributedOccupancyGPUSeconds that
	// STILL has no cause pointing either way: a no-terminal-phase attempt the sole-attempt-plus-completion
	// rule (see PreemptionIneffective) did NOT credit as ineffective.
	//
	// UnattributedOccupancyGPUSeconds also folds in occupancy whose cause the ledger DOES establish -- a
	// Succeeded stop, or a no-terminal-phase attempt credited by that same rule -- so a report claiming the
	// evidence "supports neither conclusion" must read this field, or it contradicts the PreemptionIneffective
	// line describing the very same seconds.
	UncreditedAttributionUnknownOccupancyGPUSeconds float64
	// UncreditedAttributionUnknown is true when this row carries occupancy of that kind, mirroring
	// AttributionUnknown but excluding the credited case the same way the float above does.
	UncreditedAttributionUnknown bool
	// CensoredWaitNs is a lower-bound wait for a job never admitted by the horizon (submitted -> horizon).
	CensoredWaitNs int64
	// Attempts is how many Pod attempts this row ran.
	//
	// More than one means the platform re-ran work it had already executed, which a report limited to the
	// first attempt would hide entirely.
	Attempts int
	// ReExecuted is true when the row ran more than one attempt.
	ReExecuted bool
	// TotalOccupancyGPUSeconds is gpuCount-weighted execution time summed over ALL attempts, censored
	// attempts charged only to the horizon. It is the row's real cost to the pool regardless of attribution.
	TotalOccupancyGPUSeconds float64
	// ServiceDurationSec is the row's declared uninterrupted service time, carried from the frozen trace.
	//
	// Occupancy alone is uninterpretable: 81 GPU-seconds is neither good nor bad until it is read against the
	// 40 seconds of service that were asked for, and that ratio is what makes re-execution legible.
	ServiceDurationSec int
}

// LabResult is the censoring-aware reconstruction of one arm's run.
type LabResult struct {
	Arm string
	// Offered is the number of trace rows the arm was asked to run; it is the denominator the other counts
	// are read against, so a missed job cannot shrink the denominator.
	Offered   int
	Outcomes  []WorkloadOutcome
	Admitted  int
	Completed int
	// UnfinishedAtHorizon is the count still pending or running at the horizon; excluding these from
	// latency percentiles would flatter the worse policy, so they are reported explicitly.
	UnfinishedAtHorizon int
	// AdmittedWaitP95Ns is the p95 admission latency over ADMITTED jobs only, in ns. It is a SECONDARY
	// statistic: on a censored arm it is admitted-survivor-biased (0 admissions reads as 0), so it must be
	// read together with WaitP95FullyObserved and the censored waits, never in place of them.
	AdmittedWaitP95Ns int64
	// WaitP95FullyObserved is true only when EVERY offered job was admitted, which is the only case where
	// the admitted-only p95 equals the offered-work p95. Any unadmitted job is right-censored with an
	// unknown true wait that could occupy the p95 rank, so an admission fraction below 100% cannot make the
	// admitted-only p95 a defensible estimate of the offered-work p95; when false, treat AdmittedWaitP95Ns
	// as a within-survivors descriptor only.
	WaitP95FullyObserved bool
	// Spread is how finely this run's own observations can distinguish an interval, or nil when the events
	// bounded nothing.
	//
	// It sits on the result beside the magnitudes it qualifies because the failure it exists to prevent is a
	// reader holding a magnitude WITHOUT it: a 0.9 second residual was published from this lab as a
	// measurement while the run's own spread ran from 0.4 to 2.4 seconds. Every consumer of the totals below
	// is expected to ask Spread.Resolves before calling a difference an effect.
	Spread *ObservationSpread
	// TotalWastedGPUSeconds is the run's total EXACT discarded (restart-from-zero) work.
	TotalWastedGPUSeconds float64
	// TotalWasteLowerBoundGPUSeconds is >= the exact total, counting censored attempts up to the horizon.
	TotalWasteLowerBoundGPUSeconds float64
	// AnyWasteCensored is true when any outcome's attributable waste is censored, so the exact total is a
	// lower bound.
	AnyWasteCensored bool
	// TotalUnattributedOccupancyGPUSeconds is the run's total occupancy that a preemption was decided over but
	// that could not be charged as loss, either because the attempt succeeded anyway or because no terminal
	// phase was observed.
	TotalUnattributedOccupancyGPUSeconds float64
	// AnyPreemptionIneffective is true when any preemption in the run is known not to have stopped its target.
	AnyPreemptionIneffective bool
	// AnyAttributionUnknown is true when any preempted attempt in the run reached no observed terminal phase.
	//
	// It has to be run-level and rendered, because a reader who sees waste=0.0 in the header must not conclude
	// that nothing was lost: occupancy exists here that the evidence cannot attribute either way.
	AnyAttributionUnknown bool
	// TotalUncreditedAttributionUnknownOccupancyGPUSeconds is the run-level sum of
	// UncreditedAttributionUnknownOccupancyGPUSeconds: occupancy whose cause is genuinely unestablished, as
	// opposed to TotalUnattributedOccupancyGPUSeconds, which also includes occupancy a Succeeded stop or a
	// credited completion DOES explain. A banner stating that the evidence "supports neither conclusion" must
	// report this figure, not the combined one, or it describes seconds the ledger has already accounted for.
	TotalUncreditedAttributionUnknownOccupancyGPUSeconds float64
	// AnyUncreditedAttributionUnknown is true when at least one row carries occupancy of that kind, gating the
	// "neither" banner so it is never printed about a row the PreemptionIneffective line already explains.
	AnyUncreditedAttributionUnknown bool
	// TotalOccupancyGPUSeconds is the run's occupancy summed over every row.
	//
	// It is a run-level field because a reader takes the header away and the per-row detail on trust: the
	// published run's header showed admissions and waste only, so the pool paying twice for the same service
	// was visible nowhere a reader would look.
	TotalOccupancyGPUSeconds float64
	// ReExecutedRows counts rows that ran more than one attempt, which is re-execution stated as a number in
	// the summary rather than left to be counted out of the per-row lines.
	ReExecutedRows int
}

// Reconstruct turns a raw event ledger into a censoring-aware result for one arm, up to horizonElapsedNs
// (the monotonic run offset the runner marks as the observation horizon).
//
// It is SEEDED from the frozen trace: every offered row gets exactly one timeline whether or not its events
// arrived, so a job pending or missed at the horizon becomes an unfinished, right-censored outcome rather
// than a silent omission. It returns an error rather than a number on evidence that could not have come from
// a real run, because for an experiment tool a plausible wrong number is worse than a refusal. What it
// refuses is exactly this:
//
//   - shape: a duplicate job name in the trace, an event for a job not in the trace, an unknown event type,
//     a negative elapsed time, a Pod event with no Pod UID, and an AttemptStopped whose reason is neither
//     Succeeded nor Failed, since that reason is the only thing separating discarded work from finished work;
//   - identity: a row with no Submitted event at all, a second Submitted or Completed at a different instant,
//     and an AttemptStopped for a Pod whose Ready was never seen;
//   - impossible order WITHIN one watch or against the locally stamped Submitted: a Pod Ready before its own
//     row was created, an attempt stopped before it was Ready, two attempts of one row overlapping, and a
//     completion with no admission evidence at all;
//   - arithmetic that contradicts the protocol: more preemption decisions than the row has attempts.
//
// It deliberately does NOT refuse orderings that are legal cross-watch reordering, and this is the part that
// changed: Workload, Job and Pod arrive on three independent watches, so an Admitted delivered before its
// Submitted, a Completed delivered before its Admitted, and a preemption delivered after the Pod it stopped
// are all realistic deliveries rather than impossible evidence. Earlier revisions rejected the first two and
// paired preemptions by comparing instants across watches; both discarded valid runs. Preemptions are paired
// to attempts ordinally now (see pairPreemptionsToAttempts), never by instant comparison.
//
// It assumes its input came from a fail-closed collector that observes every terminal Pod transition and
// invalidates a run on any unexplained one (the live runner's contract). That precondition is what makes
// "no in-horizon stop" mean the attempt still held the GPU at the horizon, so the censored waste lower
// bound is a true lower bound rather than a guess; on a raw hand-authored ledger that guarantee is absent.
func Reconstruct(arm string, trace []TrainingTraceRow, events []LifecycleEvent, horizonElapsedNs int64) (LabResult, error) {
	byJob := map[string]*jobTimeline{}
	order := make([]string, 0, len(trace))
	for _, row := range trace {
		if _, dup := byJob[row.Name]; dup {
			return LabResult{}, fmt.Errorf("trace has duplicate job name %q", row.Name)
		}
		byJob[row.Name] = &jobTimeline{
			tenant:      row.Tenant,
			gpuCount:    row.GPUCount,
			durationSec: row.DurationSec,
			attempts:    map[string]*attempt{},
		}
		order = append(order, row.Name)
	}

	for i := range events {
		if err := foldEvent(byJob, &events[i], horizonElapsedNs); err != nil {
			return LabResult{}, err
		}
	}

	// The spread is derived from the same events the intervals below are, so a reader can never hold one
	// without the other. Carrying it on the result rather than computing it in the runner is what stops a
	// second, looser derivation appearing beside a number that was already published once without one.
	res := LabResult{Arm: arm, Offered: len(trace), Spread: SpreadOf(events)}
	var admitWaits []float64
	for _, job := range order {
		t := byJob[job]
		if !t.submitted {
			// Every offered row must carry exactly one Submitted (written after a successful Create). Its
			// absence means the job was never created, which invalidates the run rather than censoring it.
			return LabResult{}, fmt.Errorf("job %q has no Submitted event; the run is invalid", job)
		}
		out := WorkloadOutcome{Job: job, Tenant: t.tenant, ServiceDurationSec: t.durationSec}
		if t.admitted {
			out.Admitted = true
			out.AdmitLatencyNs = t.admitNs - t.submittedNs
			admitWaits = append(admitWaits, float64(out.AdmitLatencyNs))
			res.Admitted++
		} else {
			out.CensoredWaitNs = horizonElapsedNs - t.submittedNs
		}
		if t.completed {
			out.Completed = true
			res.Completed++
		} else {
			res.UnfinishedAtHorizon++
		}
		if err := chargeWaste(t, horizonElapsedNs, &out); err != nil {
			return LabResult{}, fmt.Errorf("job %q: %w", job, err)
		}
		res.TotalWastedGPUSeconds += out.WastedGPUSeconds
		res.TotalWasteLowerBoundGPUSeconds += out.WasteLowerBoundGPUSeconds
		if out.WasteCensored {
			res.AnyWasteCensored = true
		}
		res.TotalUnattributedOccupancyGPUSeconds += out.UnattributedOccupancyGPUSeconds
		res.TotalUncreditedAttributionUnknownOccupancyGPUSeconds += out.UncreditedAttributionUnknownOccupancyGPUSeconds
		if out.PreemptionIneffective {
			res.AnyPreemptionIneffective = true
		}
		if out.AttributionUnknown {
			res.AnyAttributionUnknown = true
		}
		if out.UncreditedAttributionUnknown {
			res.AnyUncreditedAttributionUnknown = true
		}
		res.TotalOccupancyGPUSeconds += out.TotalOccupancyGPUSeconds
		if out.ReExecuted {
			res.ReExecutedRows++
		}
		res.Outcomes = append(res.Outcomes, out)
	}

	sort.Float64s(admitWaits)
	res.AdmittedWaitP95Ns = int64(percentileNs(admitWaits, 0.95))
	// The admitted-only p95 equals the offered-work p95 only when nothing is censored. With any unadmitted
	// job the offered p95 is not identifiable from admitted data, so the fully-observed flag is exactly
	// "every offered job was admitted", not an admission-fraction threshold.
	res.WaitP95FullyObserved = res.Offered > 0 && res.Admitted == res.Offered
	return res, nil
}

// foldEvent validates one event against the seeded timelines and folds it in, returning an error on any
// evidence that could not have happened in a real run.
//
// Shape validation (known job, known type, Pod events carry a UID, non-negative timestamp) runs BEFORE the
// horizon rule, so a corrupt event is rejected whether it lands before or after the horizon. Only a valid
// post-horizon AttemptStopped is then folded, as the causal tail that closes a pre-horizon preemption; any
// other post-horizon delta is validated but not folded into in-horizon state.
func foldEvent(byJob map[string]*jobTimeline, e *LifecycleEvent, horizonElapsedNs int64) error {
	if e.ElapsedNs < 0 {
		return fmt.Errorf("job %q event has negative elapsed time %d", e.Job, e.ElapsedNs)
	}
	t, ok := byJob[e.Job]
	if !ok {
		return fmt.Errorf("event for job %q is not in the trace", e.Job)
	}
	switch e.Type {
	case EventSubmitted, EventAdmitted, EventPreempted, EventCompleted:
	case EventPodReady:
		if e.ObjectUID == "" {
			return fmt.Errorf("job %q %s has no Pod UID", e.Job, e.Type)
		}
	case EventAttemptStopped:
		if e.ObjectUID == "" {
			return fmt.Errorf("job %q %s has no Pod UID", e.Job, e.Type)
		}
		// The stop reason is the ONLY evidence that separates discarded work from a run that finished its own
		// service, so attribution must not be a blacklist that exempts one literal and charges everything else
		// as preemption waste. ClassifyPod can emit only the two terminal phases, so any other value means the
		// evidence is not interpretable and a plausible wrong number is worse than a refusal.
		if e.Reason != StopReasonSucceeded && e.Reason != StopReasonFailed {
			return fmt.Errorf("job %q AttemptStopped has uninterpretable reason %q; want %q or %q",
				e.Job, e.Reason, StopReasonSucceeded, StopReasonFailed)
		}
	default:
		return fmt.Errorf("job %q has unknown event type %q", e.Job, e.Type)
	}

	if e.ElapsedNs > horizonElapsedNs && e.Type != EventAttemptStopped {
		return nil
	}

	switch e.Type {
	case EventSubmitted:
		if t.submitted && t.submittedNs != e.ElapsedNs {
			return fmt.Errorf("job %q submitted twice at %d and %d", e.Job, t.submittedNs, e.ElapsedNs)
		}
		t.submitted = true
		t.submittedNs = e.ElapsedNs
	case EventAdmitted:
		if !t.admitted {
			t.admitted = true
			t.admitNs = e.ElapsedNs
			t.admitStampNs = stampOf(e)
		} else if e.ElapsedNs < t.admitNs {
			t.admitNs = e.ElapsedNs
			t.admitStampNs = stampOf(e)
		}
	case EventPodReady:
		a, ok := t.attempts[e.ObjectUID]
		if !ok {
			a = &attempt{uid: e.ObjectUID, readyNs: e.ElapsedNs, readyStampNs: stampOf(e)}
			t.attempts[e.ObjectUID] = a
			t.attemptSeq = append(t.attemptSeq, e.ObjectUID)
		} else if e.ElapsedNs < a.readyNs {
			a.readyNs = e.ElapsedNs
			a.readyStampNs = stampOf(e)
		}
	case EventPreempted:
		t.preemptedNs = append(t.preemptedNs, e.ElapsedNs)
	case EventAttemptStopped:
		a, ok := t.attempts[e.ObjectUID]
		if !ok {
			// A stop past the horizon for a Pod whose PodReady was ALSO past the horizon is an ordinary
			// consequence of where the window closes, not a malformed sequence.
			//
			// The gate above folds a post-horizon AttemptStopped — deliberately, because an attempt that was
			// running when the window closed needs its end to charge occupancy correctly — while dropping every
			// other post-horizon event, PodReady included. So a retry Pod that became Ready after the horizon
			// and stopped shortly after arrived here with no attempt to attach to, and the run was refused
			// under dispReconstructRefused with a reason that reads as a broken ledger. Both events are
			// deliverable in the interval between waitForHorizon returning and the watches being cancelled, so
			// the window is narrow and real.
			//
			// Inside the horizon this stays an error, and that is the half worth keeping: a stop for a Pod
			// never seen Ready within the measured window IS a malformed sequence, and folding it silently
			// would charge occupancy from an instant nothing established.
			if e.ElapsedNs > horizonElapsedNs {
				return nil
			}
			return fmt.Errorf("job %q AttemptStopped for unknown Pod %q (no PodReady seen)", e.Job, e.ObjectUID)
		}
		if !a.stopped || e.ElapsedNs < a.stopNs {
			a.stopped = true
			a.stopNs = e.ElapsedNs
			a.stopReason = e.Reason
		}
	case EventCompleted:
		if t.completed && t.completedNs != e.ElapsedNs {
			return fmt.Errorf("job %q completed twice at %d and %d", e.Job, t.completedNs, e.ElapsedNs)
		}
		t.completed = true
		t.completedNs = e.ElapsedNs
	}
	return nil
}

// occupancyEnd returns the point up to which one attempt's occupancy may be charged.
//
// An observed stop is authoritative regardless of the horizon: an in-horizon stop ends the interval exactly
// there, and a post-horizon one still bounds it at the horizon because the attempt is known to have held the
// GPU through the boundary. Only when NO stop was observed does a completion help: a Job's Complete
// condition is observed only after its Pod has terminated, so an unstopped attempt on an otherwise-completed
// row could not still have been running past that completion, making it a sound and still-conservative upper
// bound tighter than the horizon. With neither a stop nor a completion, the horizon is what remains.
func occupancyEnd(t *jobTimeline, a *attempt, horizonElapsedNs int64) int64 {
	end := horizonElapsedNs
	switch {
	case a.stopped:
		if a.stopNs <= horizonElapsedNs {
			end = a.stopNs
		}
	case t.completed:
		end = t.completedNs
	}
	// Clamped up to readyNs, never below it, because the endpoint can legally be observed first.
	//
	// completedNs comes from the JOB watch and readyNs from the POD watch, and the ledger already states that
	// ordering across independent watches proves nothing about what happened. So a completion seen before its
	// own attempt's Ready is not impossible evidence — it is the reordering this design accepts — and the
	// subtraction below it would produce NEGATIVE occupancy, which does not merely misreport this attempt: it
	// silently cancels another attempt's real cost out of the row's total, with nothing flagged.
	//
	// Refusing the run instead would discard legitimate measurements over an ordering the harness has already
	// decided it cannot read. Clamping keeps the accounting non-negative and treats the interval as the
	// zero-width evidence it actually is.
	//
	// The same-watch case is deliberately NOT handled here: a Pod stopping before it was Ready comes from one
	// watch, is therefore impossible rather than reordered, and chargeWaste refuses the run over it.
	if end < a.readyNs {
		return a.readyNs
	}
	return end
}

// chargeWaste runs the per-job ordering checks and the attempt-paired preemption accounting, writing the
// exact and lower-bound waste onto out.
func chargeWaste(t *jobTimeline, horizonElapsedNs int64, out *WorkloadOutcome) error {
	// Only checks that hold REGARDLESS of delivery order belong here. Workload, Job and Pod arrive on three
	// independent watches, so comparing their observed instants proves nothing about what happened first —
	// a promptly stopped Pod really can be delivered before the preemption that stopped it. What remains
	// impossible under any ordering is a completion with no admission evidence at all.
	if t.completed && !t.admitted {
		return fmt.Errorf("completed with no admission evidence")
	}

	out.Preemptions = len(t.preemptedNs)
	out.Attempts = len(t.attemptSeq)
	out.ReExecuted = out.Attempts > 1
	if len(t.attemptSeq) > 0 {
		// attemptSeq is observation-insertion order, not chronological order, so the earliest-observed Ready
		// is the minimum over all attempts, the same pattern already used for EventAdmitted and per-UID
		// readyNs; picking attemptSeq[0] would silently misreport a re-executed row whose retry's PodReady
		// happened to be folded first.
		firstReady := t.attempts[t.attemptSeq[0]].readyNs
		firstReadyStamp := t.attempts[t.attemptSeq[0]].readyStampNs
		for _, uid := range t.attemptSeq[1:] {
			if r := t.attempts[uid].readyNs; r < firstReady {
				firstReady = r
				firstReadyStamp = t.attempts[uid].readyStampNs
			}
		}
		out.Executed = true
		if firstReady < t.submittedNs {
			// A Pod cannot become Ready before its own row was even created (Submitted is stamped locally,
			// not watch-observed), so a negative gap here is impossible evidence, not legal reordering.
			return fmt.Errorf("first Pod Ready at %d before submitted at %d", firstReady, t.submittedNs)
		}
		out.ReadyLatencyNs = firstReady - t.submittedNs
		if t.admitted {
			// Both endpoints here are watch-observed (Admitted and PodReady come from independent watches), so
			// this gap can legally be slightly negative when deliveries are reordered across watches; erroring
			// on that would discard a valid run, so the raw value is left as-is, unclamped.
			out.AdmitToReadyNs = firstReady - t.admitNs
			// The same interval read off the two COMPONENTS' clocks instead of this collector's, and it is
			// carried beside rather than instead of the arrival-based figure because the two bound different
			// error sources and neither dominates.
			//
			// The arrival figure is fine-grained and carries the delivery lag of two independent watches,
			// which the recorded runs put at whole seconds. This one has no watch lag at all, and instead
			// carries up to a second of truncation at each end plus the offset between Kueue's clock and
			// the kubelet's. The offset is a CONSTANT for a given pair of machines, so it cancels in a
			// difference between two arms measured on the same node -- and does not cancel between nodes,
			// which is why a node comparison must not use this figure.
			//
			// Where they agree, that is corroboration from two directions. Where they disagree by more than
			// both bounds allow, something is wrong and the run should not be published either way.
			if t.admitStampNs != 0 && firstReadyStamp != 0 {
				v := firstReadyStamp - t.admitStampNs
				out.AdmitToReadyStampNs = &v
			}
		}
	}
	for _, uid := range t.attemptSeq {
		a := t.attempts[uid]
		if a.stopped && a.stopNs < a.readyNs {
			// A Pod's Ready and its terminal phase come from the SAME watch, so unlike the cross-watch gaps
			// above this ordering is not legal reordering but impossible evidence. Charging it would produce
			// negative occupancy that silently cancels another attempt's real cost with no flag raised.
			return fmt.Errorf("attempt %s stopped at %d before it was Ready at %d", a.uid, a.stopNs, a.readyNs)
		}
		end := occupancyEnd(t, a, horizonElapsedNs)
		out.TotalOccupancyGPUSeconds += float64(t.gpuCount) * float64(end-a.readyNs) / 1e9
	}
	if err := attemptsDoNotOverlap(t); err != nil {
		return err
	}
	paired, err := pairPreemptionsToAttempts(t, len(t.preemptedNs))
	if err != nil {
		return err
	}
	// No per-attempt "consumed" flag: pairPreemptionsToAttempts returns each ordinal attempt once, so a single
	// pass cannot charge one twice. A mutable guard here read as double-charge protection while providing none,
	// and it would only start mattering if pairing ever became incremental or multi-pass.
	for _, a := range paired {
		gpu := float64(t.gpuCount)
		// The attempt's in-horizon occupancy, which every arm below charges somewhere. It is the SAME endpoint
		// rule the occupancy loop above uses, so no arm can charge more than the row's own occupancy and a
		// double-charge cannot hide behind two different interval definitions.
		end := occupancyEnd(t, a, horizonElapsedNs)
		charged := gpu * float64(end-a.readyNs) / 1e9
		switch {
		case a.stopped && a.stopReason == StopReasonSucceeded:
			// The attempt reached a successful terminal phase, so it finished its own service rather than
			// being stopped by the preemption. Its occupancy is real but it is not discarded work, and folding
			// it into waste would report a completed run as destroyed progress: the published error verbatim.
			// This holds whether the stop landed in-horizon or beyond it, because the reason refutes loss
			// wherever it is read.
			out.UnattributedOccupancyGPUSeconds += charged
			out.PreemptionIneffective = true
		case a.stopped && a.stopNs <= horizonElapsedNs:
			// A Failed terminal phase observed in-horizon is the only evidence that supports an exact charge:
			// the attempt was cut short, and the interval from Ready to the stop is fully observed.
			out.WastedGPUSeconds += charged
			out.WasteLowerBoundGPUSeconds += charged
		case a.stopped:
			// A Failed terminal phase beyond the horizon: the loss IS attributable, only its interval is
			// truncated, which is exactly what WasteCensored means. The attempt is known to have held the GPU
			// through the boundary, so Ready->horizon is a true lower bound on the discarded work.
			out.WasteLowerBoundGPUSeconds += charged
			out.WasteCensored = true
		default:
			// No terminal phase was observed at all, so Pod state cannot say whether the preemption stopped
			// this attempt or it ran on and succeeded. Presuming loss here is what published completed work as
			// discarded, and it is backwards on the very arm this study characterises, where an ignoring
			// workload is more likely than not to finish. So the occupancy is charged as unattributed and the
			// cause is reported as unknown rather than guessed.
			out.UnattributedOccupancyGPUSeconds += charged
			out.AttributionUnknown = true
			if t.completed && len(t.attemptSeq) == 1 {
				// With exactly ONE attempt the row's completion can only have happened on this attempt, so it
				// was not stopped. Gated strictly on the count: with several attempts a completion says
				// nothing about which one it belongs to, and crediting it would be inference from adjacency
				// again.
				out.PreemptionIneffective = true
			} else {
				// This charge was NOT credited, so unlike the branch above its cause is still genuinely
				// unknown; a report is free to say so about these seconds without contradicting a
				// PreemptionIneffective line, because no such line was written for them.
				out.UncreditedAttributionUnknownOccupancyGPUSeconds += charged
				out.UncreditedAttributionUnknown = true
			}
		}
	}
	return nil
}

// attemptsInStartOrder returns the row's attempts ordered by when their Pod became Ready.
//
// Ordering attempts among THEMSELVES is legal: every Ready observation comes from the same Pod watch, whose
// deliveries are ordered. What is illegal is comparing one of those instants against a preemption instant
// from the Workload watch, which is why the ordering is computed here once rather than inside a pairing
// predicate.
func attemptsInStartOrder(t *jobTimeline) []*attempt {
	out := make([]*attempt, 0, len(t.attemptSeq))
	for _, uid := range t.attemptSeq {
		out = append(out, t.attempts[uid])
	}
	slices.SortStableFunc(out, func(a, b *attempt) int {
		switch {
		case a.readyNs < b.readyNs:
			return -1
		case a.readyNs > b.readyNs:
			return 1
		default:
			return 0
		}
	})
	return out
}

// attemptsDoNotOverlap checks that a row's attempts, ordered by Ready, never run concurrently.
//
// It compares one attempt's stop instant against the NEXT attempt's Ready instant, and both come from the
// same Pod watch, so unlike a decision-to-attempt comparison this ordering is trustworthy evidence, not a
// delivery-latency artifact. A row runs with Parallelism: 1, one attempt at a time, so the earlier attempt
// must have stopped before the later one became Ready.
// An attempt with no observed stop cannot be shown to have ended, so it is treated as still open: a later
// attempt becoming Ready while an earlier one has no observed stop is also an overlap, not a pass by
// default.
func attemptsDoNotOverlap(t *jobTimeline) error {
	ordered := attemptsInStartOrder(t)
	for i := 1; i < len(ordered); i++ {
		prev, cur := ordered[i-1], ordered[i]
		if !prev.stopped || prev.stopNs >= cur.readyNs {
			return fmt.Errorf("attempt %s and attempt %s overlap: a row runs one attempt at a time",
				prev.uid, cur.uid)
		}
	}
	return nil
}

// pairPreemptionsToAttempts matches each preemption decision to the attempt it stopped, ordinally.
//
// It never compares a decision instant against an attempt instant, because those come from different
// watches: a victim that honors SIGTERM and stops within a second can have its Pod terminal delivered
// BEFORE the Workload preemption, and an instant comparison would reject the real victim as already
// stopped. Both orderings it does use are within a single watch and therefore trustworthy — decisions
// among decisions, attempts among attempts.
//
// Ordinal pairing is what the mechanism actually does: a row runs one attempt at a time, so the k-th
// preemption is the one that stopped the k-th attempt. A row that was preempted once and then re-executed
// has two attempts and one decision, and only the first attempt is charged — the retry was not preempted.
func pairPreemptionsToAttempts(t *jobTimeline, decisions int) ([]*attempt, error) {
	ordered := attemptsInStartOrder(t)
	if decisions > len(ordered) {
		return nil, fmt.Errorf("%d preemptions but only %d attempts; the ledger disagrees with the protocol",
			decisions, len(ordered))
	}
	return ordered[:decisions], nil
}

// percentileNs is a nearest-rank percentile over a sorted slice, returning 0 for empty input.
func percentileNs(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*q+0.9999) - 1
	rank = max(rank, 0)
	rank = min(rank, len(sorted)-1)
	return sorted[rank]
}

// stampOf is the component's own clock for an event, or zero when it published none.
func stampOf(e *LifecycleEvent) int64 {
	if e.ComponentStampUnixNanos == nil {
		return 0
	}
	return *e.ComponentStampUnixNanos
}
