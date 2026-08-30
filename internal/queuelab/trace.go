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

// Package queuelab replays deterministic MLTrainingJob traces against a real Kueue on kind and compares
// one queue-policy knob at a time (reclaim, FIFO), using a list/watch lifecycle ledger as the authority.
//
// The traces here are HANDCRAFTED and deterministic, not random: a mechanism experiment must reliably
// exercise late reclaim or FIFO head-of-line blocking, which a random arrival process does poorly.
package queuelab

import (
	"fmt"
	"io"
	"math"
	"regexp"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/exputil"
)

// rfc1123Name is the lowercase DNS label form a Kubernetes object name must take, so a trace row cannot
// name a job the API server would then reject at create time (which would look like a censored job).
var rfc1123Name = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// labMaxGPU is the lab worker's fake GPU count; a row asking for more can never be admitted, so it would
// masquerade as a censored job rather than a bad trace.
const labMaxGPU = 2

// TrainingTraceRow is one scheduled training submission in an immutable trace.
//
// It carries only what is fixed before a run starts: when the job is submitted, who submits it, how much
// GPU quota it requests, and how long its service would take uninterrupted.
type TrainingTraceRow struct {
	// Index is the row's stable position, used to join raw ledger events back to the trace.
	Index int `json:"index"`
	// Name is a stable per-run job name, so the ledger can attribute events without guessing.
	Name string `json:"name"`
	// OffsetMs is the submission time as a millisecond offset from the run start; offsets are non-decreasing.
	OffsetMs int64 `json:"offsetMs"`
	// Tenant is the submitting tenant, which resolves to its own LocalQueue.
	Tenant string `json:"tenant"`
	// GPUCount is the requested nvidia.com/gpu units (fake capacity on kind).
	GPUCount int `json:"gpuCount"`
	// DurationSec is the uninterrupted service time; the sleeper runs this long unless preempted.
	DurationSec int `json:"durationSec"`
}

// ReclaimScenario builds the trace for the reclaim study (Never vs Any).
//
// Capacity is one nominal unit per tenant in a shared cohort (total 2). Tenant A fills its own unit, then
// borrows tenant B's idle unit, then tenant B submits and forces the borrowed unit's fate.
//
// lateReturn places tenant B's arrival near the end of the borrowed job's service, where preempting it
// discards almost a whole job of work; an early return makes preemption nearly free. That contrast is the
// study's whole point, so both are generated from the same builder.
func ReclaimScenario(lateReturn bool, durationSec int) []TrainingTraceRow {
	ownerOffsetMs := int64(5_000)
	if lateReturn {
		// Return with only a small remainder of the borrowed job's service left.
		ownerOffsetMs = int64(durationSec)*1000 - 10_000
	}
	return []TrainingTraceRow{
		{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: durationSec},
		{Index: 1, Name: "a2-borrow", OffsetMs: 1_000, Tenant: "tenant-a", GPUCount: 1, DurationSec: durationSec},
		{Index: 2, Name: "b1-owner", OffsetMs: ownerOffsetMs, Tenant: "tenant-b", GPUCount: 1, DurationSec: durationSec},
	}
}

// FIFOHeadOfLineScenario builds the trace for the FIFO study (StrictFIFO vs BestEffortFIFO).
//
// Capacity is 2 units for one tenant's queue. A long 1-GPU job holds one unit; a 2-GPU job then queues at
// the head and cannot currently fit; several 1-GPU jobs arrive behind it. StrictFIFO leaves the free unit
// idle to protect the head job's position, while BestEffortFIFO lets the younger fitting jobs bypass it.
//
// The critical arrivals are spaced far enough apart that the asynchronous MLTrainingJob -> Job -> Workload
// creation cannot randomly reverse their intended queue order.
func FIFOHeadOfLineScenario(longDurationSec, smallDurationSec int) []TrainingTraceRow {
	rows := make([]TrainingTraceRow, 0, 5)
	rows = append(rows,
		TrainingTraceRow{Index: 0, Name: "long1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: longDurationSec},
		TrainingTraceRow{Index: 1, Name: "head2", OffsetMs: 3_000, Tenant: "tenant-a", GPUCount: 2, DurationSec: smallDurationSec},
	)
	for i := range 3 {
		rows = append(rows, TrainingTraceRow{
			Index:       2 + i,
			Name:        fmt.Sprintf("small%d", i+1),
			OffsetMs:    int64(6_000 + i*3_000),
			Tenant:      "tenant-a",
			GPUCount:    1,
			DurationSec: smallDurationSec,
		})
	}
	return rows
}

// ownRowGraceMarginSec is how far a1's service must exceed the victim's service plus the termination grace
// period.
//
// It exists because a1 releasing a GPU first destroys the experiment: with two units, whichever of a1 and
// the victim releases first is what lets the owner run, so if a1 can finish during the restoration window
// the owner's execution start cannot be attributed to the reclamation under test.
const ownRowGraceMarginSec = 60

// terminationGraceSec is the Pod termination grace period the fixture runs with, mirrored here so the trace
// can reason about the worst-case restoration window.
const terminationGraceSec = 30

// DoseRegime names which side of the termination grace period the victim's remaining service falls on.
//
// It exists because the two sides measure different things, and the earlier guard permitted only one of them
// while its own comment described the other. What an IGNORING victim costs the owner is:
//
//	DoseSelfCompleting  the victim's remaining service — the grace period never expires, so the platform's
//	                    configured patience is irrelevant and could be any value without changing the result
//	DoseGraceBounded    the grace period itself — a constant the platform sets, independent of how long the
//	                    victim's job would otherwise have run
//
// Only the second is what a termination CONTRACT binds, and it is the operationally ordinary one: real
// training jobs are long, so remaining service exceeds any sane grace period almost always. The first is
// still worth running, because it is the regime that reproduces the confound this lineage was built on — a
// preempted victim completing successfully, its work uncredited, the row re-executing from zero — which
// cannot happen at all when the grace period cuts the victim short first.
//
// The regime is declared by the caller rather than inferred from the arithmetic, so a dose that does not
// produce the regime the run says it is measuring is refused instead of quietly measuring the other one.
type DoseRegime string

const (
	// DoseSelfCompleting returns while the victim has less service left than the grace period.
	DoseSelfCompleting DoseRegime = "self-completing"
	// DoseGraceBounded returns while the victim has at least a grace period of service left.
	DoseGraceBounded DoseRegime = "grace-bounded"
)

// TerminationContractTrace builds the trace for the termination-contract experiment.
//
// victimServiceSec is the borrowed job's service time and doseSec is how long after the victim's Pod Ready
// the owner returns; the dose is carried on the owner row's offset ONLY as provenance, because the schedule
// gates the owner on the victim's observed Ready rather than on wall-clock offsets.
//
// A dose at or past the victim's full service is refused under either regime: it leaves nothing running to
// reclaim, so there is no termination to contract about.
func TerminationContractTrace(victimServiceSec, doseSec int, regime DoseRegime) ([]TrainingTraceRow, error) {
	remaining := victimServiceSec - doseSec
	if remaining <= 0 {
		return nil, fmt.Errorf("dose %d s on a %d s victim leaves %d s of remaining service, so there is "+
			"nothing running to reclaim", doseSec, victimServiceSec, remaining)
	}
	switch regime {
	case DoseSelfCompleting:
		if remaining >= terminationGraceSec {
			return nil, fmt.Errorf("dose %d s on a %d s victim leaves %d s of remaining service, which the %d s "+
				"grace period would cut short; that is %s, not %s",
				doseSec, victimServiceSec, remaining, terminationGraceSec, DoseGraceBounded, DoseSelfCompleting)
		}
	case DoseGraceBounded:
		if remaining < terminationGraceSec {
			return nil, fmt.Errorf("dose %d s on a %d s victim leaves %d s of remaining service, so an ignoring "+
				"victim would finish before the %d s grace period expired; that is %s, not %s",
				doseSec, victimServiceSec, remaining, terminationGraceSec, DoseSelfCompleting, DoseGraceBounded)
		}
	default:
		return nil, fmt.Errorf("unknown dose regime %q; want %q or %q", regime, DoseSelfCompleting, DoseGraceBounded)
	}
	return []TrainingTraceRow{
		{
			Index: 0, Name: OwnRow, OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1,
			DurationSec: victimServiceSec + terminationGraceSec + ownRowGraceMarginSec,
		},
		{Index: 1, Name: VictimRow, OffsetMs: 1_000, Tenant: "tenant-a", GPUCount: 1, DurationSec: victimServiceSec},
		{
			Index: 2, Name: OwnerRow, OffsetMs: int64(1_000 + doseSec*1_000), Tenant: "tenant-b",
			GPUCount: 1, DurationSec: victimServiceSec,
		},
	}, nil
}

// WriteTrace serializes a trace as JSON Lines.
func WriteTrace(w io.Writer, rows []TrainingTraceRow) error {
	return exputil.WriteJSONL(w, rows)
}

// ReadTrace parses a JSON Lines trace and runs the study-agnostic structural checks, so a hand-edited
// trace with duplicate names, non-contiguous indices, negative offsets, or unsafe values is rejected
// rather than replayed into a reconstruction that would silently misattribute or collide its jobs.
//
// Study-specific rules (allowed tenants, per-study GPU counts) are enforced separately by ValidateTrace,
// which the runner calls once the study is known.
func ReadTrace(r io.Reader) ([]TrainingTraceRow, error) {
	rows, err := exputil.ReadJSONL[TrainingTraceRow](r)
	if err != nil {
		return nil, err
	}
	if err := validateStructural(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// validateStructural enforces the invariants Reconstruct relies on regardless of study: unique contiguous
// indices, unique valid names, non-decreasing non-negative offsets, and positive int32-safe GPU/duration.
func validateStructural(rows []TrainingTraceRow) error {
	if len(rows) == 0 {
		return fmt.Errorf("trace is empty")
	}
	names := map[string]bool{}
	var prevOffset int64
	for i, row := range rows {
		// The name becomes a Job/Pod name prefix, so it must be a valid label-length DNS name, or the API
		// server rejects the create and the job masquerades as censored rather than as a bad trace.
		if !rfc1123Name.MatchString(row.Name) || len(row.Name) > 63 {
			return fmt.Errorf("row %d name %q is not a valid Kubernetes name (<=63 chars)", i, row.Name)
		}
		if names[row.Name] {
			return fmt.Errorf("duplicate job name %q", row.Name)
		}
		names[row.Name] = true
		// The index IS the row position so an event join by index cannot land on a gap or a reordering.
		if row.Index != i {
			return fmt.Errorf("row %q index %d does not match its position %d", row.Name, row.Index, i)
		}
		if row.OffsetMs < 0 || row.OffsetMs > math.MaxInt32 {
			return fmt.Errorf("row %q offset %d is outside [0,%d] ms", row.Name, row.OffsetMs, math.MaxInt32)
		}
		if row.OffsetMs < prevOffset {
			return fmt.Errorf("trace offsets are not non-decreasing at row %q", row.Name)
		}
		prevOffset = row.OffsetMs
		if row.GPUCount < 1 || row.GPUCount > labMaxGPU {
			return fmt.Errorf("row %q GPUCount %d is outside [1,%d]", row.Name, row.GPUCount, labMaxGPU)
		}
		if row.DurationSec < 1 || row.DurationSec > math.MaxInt32 {
			return fmt.Errorf("row %q DurationSec %d is not a positive int32", row.Name, row.DurationSec)
		}
	}
	return nil
}

// ValidateTrace runs the structural checks and the study-specific semantic checks a runner must pass a
// trace before replaying it: the reclaim study is a two-tenant borrowing story, while the FIFO study is a
// single-tenant head-of-line story, and a row that violates the study would exercise a different mechanism
// than the one under test.
func ValidateTrace(study Study, rows []TrainingTraceRow) error {
	if err := validateStructural(rows); err != nil {
		return err
	}
	switch study {
	case StudyReclaim:
		seen := map[string]bool{}
		for _, row := range rows {
			if row.Tenant != "tenant-a" && row.Tenant != "tenant-b" {
				return fmt.Errorf("reclaim row %q has tenant %q, want tenant-a or tenant-b", row.Name, row.Tenant)
			}
			if row.GPUCount != 1 {
				return fmt.Errorf("reclaim row %q GPUCount %d, want 1 (per-tenant nominal quota)", row.Name, row.GPUCount)
			}
			seen[row.Tenant] = true
		}
		// Reclaim is a cross-tenant borrowing-and-return story: with only one tenant there is nothing to
		// reclaim, so a single-tenant trace would silently not exercise the mechanism under test.
		if !seen["tenant-a"] || !seen["tenant-b"] {
			return fmt.Errorf("reclaim trace needs both tenant-a and tenant-b to exercise reclamation")
		}
	case StudyFIFO:
		hasHead := false
		for _, row := range rows {
			if row.Tenant != "tenant-a" {
				return fmt.Errorf("fifo row %q has tenant %q, want the single tenant tenant-a", row.Name, row.Tenant)
			}
			if row.GPUCount == labMaxGPU {
				hasHead = true
			}
		}
		// The FIFO study turns on a head job that cannot immediately fit (the 2-GPU job) blocking smaller
		// ones behind it; without such a head there is no head-of-line decision to compare.
		if !hasHead {
			return fmt.Errorf("fifo trace needs a %d-GPU head job to create head-of-line blocking", labMaxGPU)
		}
	default:
		return fmt.Errorf("unknown study %q", study)
	}
	return nil
}
