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

import "testing"

const (
	jobBorrow = "a2-borrow"
	jobHead   = "head2"
)

func hasBarrier(after []Barrier, kind BarrierKind) *Barrier {
	for i := range after {
		if after[i].Kind == kind {
			return &after[i]
		}
	}
	return nil
}

func TestReclaimScheduleStagesOnObservedState(t *testing.T) {
	steps, err := StudySchedule(StudyReclaim, ReclaimScenario(true, 600))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("reclaim schedule should have 3 steps, got %d", len(steps))
	}
	// Step 1 submits the first holder with no precondition.
	if steps[0].Row.Name != "a1" || len(steps[0].After) != 0 {
		t.Fatalf("step 0 should submit a1 with no barrier, got %s / %v", steps[0].Row.Name, steps[0].After)
	}
	// The borrower waits until a1 is running and its unit is used.
	if steps[1].Row.Name != jobBorrow {
		t.Fatalf("step 1 should submit the borrower, got %s", steps[1].Row.Name)
	}
	if hasBarrier(steps[1].After, BarrierAdmittedReady) == nil || hasBarrier(steps[1].After, BarrierFlavorUsage) == nil {
		t.Fatalf("borrower must wait for a1 ready and usage=1, got %v", steps[1].After)
	}
	// The owner waits until the borrower has actually borrowed (usage 2) and a late delay from ITS Ready.
	if steps[2].Row.Name != "b1-owner" {
		t.Fatalf("step 2 should submit the owner, got %s", steps[2].Row.Name)
	}
	usage := hasBarrier(steps[2].After, BarrierFlavorUsage)
	if usage == nil || usage.Count != 2 {
		t.Fatalf("owner must wait for borrowing (usage=2), got %v", steps[2].After)
	}
	delay := hasBarrier(steps[2].After, BarrierDelayFromReady)
	if delay == nil || delay.Job != jobBorrow {
		t.Fatalf("owner's late delay must be measured from the borrower's Ready, got %v", delay)
	}
	if delay.DelaySec <= 0 {
		t.Fatalf("late-return delay should be positive, got %d", delay.DelaySec)
	}
}

func TestFIFOScheduleGatesSmallsOnHeadPending(t *testing.T) {
	steps, err := StudySchedule(StudyFIFO, FIFOHeadOfLineScenario(600, 120))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 5 {
		t.Fatalf("fifo schedule should have 5 steps, got %d", len(steps))
	}
	if steps[0].Row.Name != "long1" || len(steps[0].After) != 0 {
		t.Fatalf("step 0 should submit long1 with no barrier")
	}
	if steps[1].Row.Name != jobHead || hasBarrier(steps[1].After, BarrierAdmittedReady) == nil {
		t.Fatalf("head2 must wait for long1 running")
	}
	// Every small job must wait until the head is genuinely Pending, or there is no head-of-line decision.
	for _, s := range steps[2:] {
		if hasBarrier(s.After, BarrierPending) == nil {
			t.Fatalf("small job %s must wait for the head to be Pending, got %v", s.Row.Name, s.After)
		}
		if hasBarrier(s.After, BarrierPending).Job != jobHead {
			t.Fatalf("small job %s should gate on head2 pending", s.Row.Name)
		}
	}
}

func TestStudyScheduleRejectsWrongShape(t *testing.T) {
	if _, err := StudySchedule(StudyReclaim, FIFOHeadOfLineScenario(600, 120)); err == nil {
		t.Fatalf("a 5-row trace is not a valid reclaim schedule")
	}
	if _, err := StudySchedule("nope", ReclaimScenario(true, 600)); err == nil {
		t.Fatalf("unknown study should error")
	}
}

func TestTerminationContractScheduleUsesTheStatedDose(t *testing.T) {
	trace, err := TerminationContractTrace(60, 40, DoseSelfCompleting)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := TerminationContractSchedule(trace, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(steps))
	}
	owner := steps[2]
	if owner.Row.Name != OwnerRow {
		t.Fatalf("last step submits %q, want %q", owner.Row.Name, OwnerRow)
	}
	var delay *Barrier
	for i := range owner.After {
		if owner.After[i].Kind == BarrierDelayFromReady {
			delay = &owner.After[i]
		}
	}
	if delay == nil {
		t.Fatal("the owner step must be gated on a delay from the victim's Ready")
	}
	if delay.Job != VictimRow {
		t.Fatalf("delay measured from %q, want the victim %q", delay.Job, VictimRow)
	}
	// The whole point: 40 is what the protocol says, not 49 derived from two trace offsets.
	if delay.DelaySec != 40 {
		t.Fatalf("dose = %d s, want the stated 40 s", delay.DelaySec)
	}

	// Deriving the dose from the old trace builder is what produced 49; prove the two disagree so nobody
	// reintroduces the derivation.
	old, err := StudySchedule(StudyReclaim, ReclaimScenario(true, 60))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range old[2].After {
		if b.Kind == BarrierDelayFromReady && b.DelaySec == 40 {
			t.Fatal("the offset-derived schedule now yields 40 s; this test's premise needs revisiting")
		}
	}
}

func TestTerminationContractScheduleRejectsADoseThatContradictsTheTrace(t *testing.T) {
	// The trace states 40 s as provenance on the owner row's offset; a schedule asked for 5 s would silently
	// run a different experiment than the one the trace records, which is the exact failure this branch
	// exists to prevent.
	trace, err := TerminationContractTrace(60, 40, DoseSelfCompleting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TerminationContractSchedule(trace, 5); err == nil {
		t.Fatal("a dose that disagrees with the trace's stated provenance must be rejected")
	}
}

func TestTerminationContractScheduleRejectsRowsThatDoNotOutliveTheWindow(t *testing.T) {
	// ReclaimScenario names its rows a1 / a2-borrow / b1-owner, byte-identical to OwnRow / VictimRow /
	// OwnerRow, so a name-only guard would pass this trace even though all three durations are equal and a1
	// can release before the victim -- exactly the confound the branch removes. The check must be structural
	// (durations), not the names that happen to match here.
	if _, err := TerminationContractSchedule(ReclaimScenario(true, 600), 40); err == nil {
		t.Fatal("a1 duration equal to the victim's must be rejected: a1 could release first")
	}
}
