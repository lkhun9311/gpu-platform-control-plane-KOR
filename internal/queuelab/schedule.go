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

import "fmt"

// BarrierKind is a condition the orchestrator waits for on the live cluster before submitting the next job.
//
// The review's point (finding #3) was that a fixed sleep does not reliably exercise the mechanism: reclaim
// only happens if the borrower is actually running when the owner arrives, and FIFO head-of-line blocking
// only happens if the head is actually Pending when the small jobs arrive. So each submission is gated on
// observed STATE, not elapsed time.
type BarrierKind string

const (
	// BarrierAdmittedReady waits until the referenced job's Workload is admitted and its Pod is Ready, so a
	// later submission cannot race ahead of a job that has not yet started running.
	BarrierAdmittedReady BarrierKind = "AdmittedReady"
	// BarrierFlavorUsage waits until the GPU usage Kueue reports against the run's ResourceFlavor REACHES
	// Count, confirming borrowing actually took effect (usage 2 on a 1+1 cohort means tenant-a borrowed
	// tenant-b's unit).
	//
	// "reaches" rather than "equals" because usage is sampled: an exact match can be stepped over between two
	// polls, and a barrier waiting for equality would then time out on a run that had already got there.
	//
	// The observation is Kueue's own ClusterQueue status, not this operator's phase field. An earlier
	// implementation summed the declared GPUCount of MLTrainingJobs marked Running, which could hold while
	// Kueue had admitted nothing — and deciding exactly that is what this barrier is for.
	BarrierFlavorUsage BarrierKind = "FlavorUsage"
	// BarrierPending waits until the referenced job's Workload is Pending (not admitted), confirming the
	// head job genuinely cannot fit yet, so the FIFO decision under test is real.
	BarrierPending BarrierKind = "Pending"
	// BarrierDelayFromReady waits DelaySec after the referenced job became Ready, so the owner's late return
	// is measured from the borrower's actual execution start, not from run t0 (which would be wrong by all
	// startup delay).
	BarrierDelayFromReady BarrierKind = "DelayFromReady"
)

// Barrier is one condition to satisfy before a step's submission.
type Barrier struct {
	Kind BarrierKind
	// Job is the referenced trace job for AdmittedReady, Pending, and DelayFromReady.
	Job string
	// Count is the target GPU usage for FlavorUsage.
	Count int
	// DelaySec is the delay for DelayFromReady.
	DelaySec int
}

// Step is one staged action: satisfy After, then submit Row.
type Step struct {
	// After are the barriers that must hold before this step submits.
	After []Barrier
	// Row is the trace job this step submits.
	Row TrainingTraceRow
}

// StudySchedule turns a study's trace into a barrier-staged submission schedule, so the runner submits each
// job only once the cluster has reached the state that makes the study's mechanism deterministic.
//
// It replaces the trace's wall-clock offsets (kept only for provenance) with observed-state barriers.
func StudySchedule(study Study, trace []TrainingTraceRow) ([]Step, error) {
	switch study {
	case StudyReclaim:
		return reclaimSchedule(trace)
	case StudyFIFO:
		return fifoSchedule(trace)
	default:
		return nil, fmt.Errorf("unknown study %q", study)
	}
}

// reclaimSchedule stages a1 -> a2-borrow -> owner, gating a2 on a1 running (usage 1) and the owner on a2
// borrowing (usage 2) plus a late delay measured from a2's actual Ready.
func reclaimSchedule(trace []TrainingTraceRow) ([]Step, error) {
	if len(trace) != 3 {
		return nil, fmt.Errorf("reclaim schedule needs 3 rows, got %d", len(trace))
	}
	a1, a2, owner := trace[0], trace[1], trace[2]
	// The owner's return delay is the trace's intended gap between the borrower and the owner, measured from
	// the borrower's Ready rather than from t0.
	delaySec := max(int((owner.OffsetMs-a2.OffsetMs)/1000), 0)
	return []Step{
		{Row: a1},
		{
			After: []Barrier{
				{Kind: BarrierAdmittedReady, Job: a1.Name},
				{Kind: BarrierFlavorUsage, Count: 1},
			},
			Row: a2,
		},
		{
			After: []Barrier{
				{Kind: BarrierAdmittedReady, Job: a2.Name},
				{Kind: BarrierFlavorUsage, Count: 2},
				{Kind: BarrierDelayFromReady, Job: a2.Name, DelaySec: delaySec},
			},
			Row: owner,
		},
	}, nil
}

// fifoSchedule stages long1 -> head2 -> smalls, gating head2 on long1 running (usage 1) and each small on
// the head being Pending, so the head-of-line blocking under test is real before the smalls arrive.
func fifoSchedule(trace []TrainingTraceRow) ([]Step, error) {
	if len(trace) < 3 {
		return nil, fmt.Errorf("fifo schedule needs at least 3 rows, got %d", len(trace))
	}
	long1, head2 := trace[0], trace[1]
	steps := []Step{
		{Row: long1},
		{
			After: []Barrier{
				{Kind: BarrierAdmittedReady, Job: long1.Name},
				{Kind: BarrierFlavorUsage, Count: 1},
			},
			Row: head2,
		},
	}
	for _, small := range trace[2:] {
		steps = append(steps, Step{
			After: []Barrier{{Kind: BarrierPending, Job: head2.Name}},
			Row:   small,
		})
	}
	return steps, nil
}

// TerminationContractSchedule stages the termination-contract experiment: a1 runs, the victim borrows, and
// the owner returns exactly doseSec after the victim's observed Pod Ready.
//
// The dose is a parameter rather than a subtraction of two trace offsets. The offset-derived form silently
// produced 49 s where the design of record specified 40 s, and nothing would have caught it until the runs
// were finished.
func TerminationContractSchedule(trace []TrainingTraceRow, doseSec int) ([]Step, error) {
	if len(trace) != 3 {
		return nil, fmt.Errorf("termination-contract schedule needs 3 rows, got %d", len(trace))
	}
	if doseSec < 0 {
		return nil, fmt.Errorf("dose %d s must not be negative", doseSec)
	}
	own, victim, owner := trace[0], trace[1], trace[2]
	if own.Name != OwnRow || victim.Name != VictimRow || owner.Name != OwnerRow {
		return nil, fmt.Errorf("rows must be %q, %q, %q in order, got %q, %q, %q",
			OwnRow, VictimRow, OwnerRow, own.Name, victim.Name, owner.Name)
	}
	// Row names can collide with these constants without the rows meaning what they say (ReclaimScenario
	// names its rows identically), so the guard against a1 releasing first checks the duration that actually
	// governs release, not the name.
	if minOwnDuration := victim.DurationSec + terminationGraceSec; own.DurationSec <= minOwnDuration {
		return nil, fmt.Errorf("a1 duration %d s does not exceed victim service + grace (%d s); a1 could "+
			"release before the victim and the owner's execution could no longer be attributed to reclamation",
			own.DurationSec, minOwnDuration)
	}
	// The dose is stated on the owner row's offset as provenance by TerminationContractTrace; if the caller's
	// doseSec disagrees, the schedule and the trace would silently run two different experiments.
	if wantOffsetMs := victim.OffsetMs + int64(doseSec)*1_000; owner.OffsetMs != wantOffsetMs {
		return nil, fmt.Errorf("dose %d s disagrees with the trace's provenance: owner offset %d ms implies a %d s dose",
			doseSec, owner.OffsetMs, (owner.OffsetMs-victim.OffsetMs)/1_000)
	}
	return []Step{
		{Row: own},
		{
			After: []Barrier{
				{Kind: BarrierAdmittedReady, Job: own.Name},
				{Kind: BarrierFlavorUsage, Count: 1},
			},
			Row: victim,
		},
		{
			After: []Barrier{
				{Kind: BarrierAdmittedReady, Job: victim.Name},
				{Kind: BarrierFlavorUsage, Count: 2},
				{Kind: BarrierDelayFromReady, Job: victim.Name, DelaySec: doseSec},
			},
			Row: owner,
		},
	}, nil
}
