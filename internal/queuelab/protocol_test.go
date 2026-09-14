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

func TestArmPolicyVariant(t *testing.T) {
	for _, tc := range []struct {
		arm  Arm
		want string
	}{
		{ArmAHonor, "Any"},
		{ArmAIgnore, "Any"},
		{ArmNRef, "Never"},
	} {
		got, err := tc.arm.PolicyVariant()
		if err != nil {
			t.Fatalf("%s: %v", tc.arm, err)
		}
		if got != tc.want {
			t.Fatalf("%s policy = %q, want %q", tc.arm, got, tc.want)
		}
	}
	if _, err := Arm("nonsense").PolicyVariant(); err == nil {
		t.Fatal("an unknown arm must be rejected, not silently defaulted")
	}
}

// TestArmContractIsPerRow is the point of the whole type: the treatment is the victim's behaviour, so the
// two Any arms must differ on exactly one row and agree on the other two.
func TestArmContractIsPerRow(t *testing.T) {
	rows := []string{OwnRow, VictimRow, OwnerRow}
	diff := 0
	for _, row := range rows {
		h, err := ArmAHonor.ContractFor(row)
		if err != nil {
			t.Fatalf("A-honor %s: %v", row, err)
		}
		i, err := ArmAIgnore.ContractFor(row)
		if err != nil {
			t.Fatalf("A-ignore %s: %v", row, err)
		}
		if h != i {
			diff++
			if row != VictimRow {
				t.Fatalf("the arms differ on row %q; only the victim may differ", row)
			}
		}
	}
	if diff != 1 {
		t.Fatalf("A-honor and A-ignore differ on %d rows, want exactly 1 (the victim)", diff)
	}

	if c, _ := ArmAIgnore.ContractFor(VictimRow); c != IgnoresSIGTERM {
		t.Fatalf("A-ignore victim contract = %v, want IgnoresSIGTERM", c)
	}
	// N-ref exists to show what happens without reclamation, so its workloads must match A-honor exactly.
	for _, row := range rows {
		n, _ := ArmNRef.ContractFor(row)
		h, _ := ArmAHonor.ContractFor(row)
		if n != h {
			t.Fatalf("N-ref row %q contract = %v, want the same as A-honor (%v)", row, n, h)
		}
	}
	if _, err := ArmAHonor.ContractFor("not-a-row"); err == nil {
		t.Fatal("an unknown row must be rejected")
	}

	// An unknown arm must be rejected even with a valid row, or a typo silently runs a different experiment.
	if _, err := Arm("nonsense").ContractFor(OwnRow); err == nil {
		t.Fatal("an unknown arm must be rejected, not return a default contract")
	}
}

func TestArmAssertCardinality(t *testing.T) {
	ok := LabResult{Outcomes: []WorkloadOutcome{
		{Job: OwnRow, Preemptions: 0, Attempts: 1},
		{Job: VictimRow, Preemptions: 1, Attempts: 1},
		{Job: OwnerRow, Preemptions: 0, Attempts: 1},
	}}
	if err := ArmAHonor.AssertCardinality(ok); err != nil {
		t.Fatalf("the expected shape must pass: %v", err)
	}

	// N-ref must never preempt; a preemption there means the policy did not take effect.
	if err := ArmNRef.AssertCardinality(ok); err == nil {
		t.Fatal("N-ref must reject any preemption")
	}

	ownPreempted := LabResult{Outcomes: []WorkloadOutcome{
		{Job: OwnRow, Preemptions: 1, Attempts: 1},
		{Job: VictimRow, Preemptions: 0, Attempts: 1},
		{Job: OwnerRow, Preemptions: 0, Attempts: 1},
	}}
	if err := ArmAHonor.AssertCardinality(ownPreempted); err == nil {
		t.Fatal("a preemption on a row other than the victim must be rejected")
	}

	twice := LabResult{Outcomes: []WorkloadOutcome{
		{Job: OwnRow, Preemptions: 0, Attempts: 1},
		{Job: VictimRow, Preemptions: 2, Attempts: 2},
		{Job: OwnerRow, Preemptions: 0, Attempts: 1},
	}}
	if err := ArmAHonor.AssertCardinality(twice); err == nil {
		t.Fatal("more than one preemption on the victim must be rejected")
	}

	missing := LabResult{Outcomes: []WorkloadOutcome{
		{Job: OwnRow, Preemptions: 0, Attempts: 1},
		{Job: VictimRow, Preemptions: 0, Attempts: 1},
	}}
	if err := ArmAHonor.AssertCardinality(missing); err == nil {
		t.Fatal("a missing row must be rejected")
	}
}

// TestTheIdlingArmsDifferOnlyInDuty is the claim that makes the study a study.
//
// Two axes at once would give a difference that carries both. These two arms must be identical in policy and
// in termination contract, and differ in exactly one thing: how much of its service the victim computes for.
func TestTheIdlingArmsDifferOnlyInDuty(t *testing.T) {
	rows := []string{OwnRow, VictimRow, OwnerRow}

	fullPolicy, err := ArmDFull.PolicyVariant()
	if err != nil {
		t.Fatalf("D-full has no policy: %v", err)
	}
	quarterPolicy, err := ArmDQuarter.PolicyVariant()
	if err != nil {
		t.Fatalf("D-quarter has no policy: %v", err)
	}
	if fullPolicy != quarterPolicy {
		t.Errorf("the idling arms apply different reclaim policies (%q vs %q), so a difference between them "+
			"carries the policy too", fullPolicy, quarterPolicy)
	}

	for _, row := range rows {
		fc, err := ArmDFull.ContractFor(row)
		if err != nil {
			t.Fatalf("D-full contract for %s: %v", row, err)
		}
		qc, err := ArmDQuarter.ContractFor(row)
		if err != nil {
			t.Fatalf("D-quarter contract for %s: %v", row, err)
		}
		if fc != qc {
			t.Errorf("row %s renders %q under D-full and %q under D-quarter; the contract is meant to be the "+
				"constant in this study", row, fc, qc)
		}
	}

	// The one difference, and it is on the victim alone.
	for _, row := range rows {
		fd, err := ArmDFull.DutyFor(row)
		if err != nil {
			t.Fatalf("D-full duty for %s: %v", row, err)
		}
		qd, err := ArmDQuarter.DutyFor(row)
		if err != nil {
			t.Fatalf("D-quarter duty for %s: %v", row, err)
		}
		switch row {
		case VictimRow:
			if fd != FullDuty || qd != QuarterDuty {
				t.Errorf("the victim runs at %v under D-full and %v under D-quarter, want %v and %v",
					fd, qd, FullDuty, QuarterDuty)
			}
		default:
			if fd != FullDuty || qd != FullDuty {
				t.Errorf("row %s idles under one of the arms (%v, %v); only the victim's occupancy is under "+
					"test, and idling the others makes the difference unattributable", row, fd, qd)
			}
		}
	}
}

// TestTheIdlingArmsUseTheIgnoringContract records why, because it is not arbitrary.
//
// A honouring victim stops in milliseconds, so its hold is shorter than a scrape interval and the observer
// has nothing inside it. That is the defect that invalidated a measured run once already. The idling study
// needs samples inside the hold, so it fixes the contract at the ignoring one.
func TestTheIdlingArmsUseTheIgnoringContract(t *testing.T) {
	for _, a := range []Arm{ArmDFull, ArmDQuarter} {
		got, err := a.ContractFor(VictimRow)
		if err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		if got != IgnoresSIGTERM {
			t.Errorf("%s renders the victim as %q; a honouring victim's hold is shorter than a scrape "+
				"interval and the observer sees nothing inside it", a, got)
		}
	}
}

// TestTheReclaimArmsAreUnchanged keeps the new arms from moving the old study.
func TestTheReclaimArmsAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		arm      Arm
		row      string
		contract TerminationContract
		duty     DutyCycle
	}{
		{ArmAHonor, VictimRow, HonorsSIGTERM, FullDuty},
		{ArmAIgnore, VictimRow, IgnoresSIGTERM, FullDuty},
		{ArmNRef, VictimRow, HonorsSIGTERM, FullDuty},
		{ArmAIgnore, OwnRow, HonorsSIGTERM, FullDuty},
		{ArmAIgnore, OwnerRow, HonorsSIGTERM, FullDuty},
	} {
		c, err := tc.arm.ContractFor(tc.row)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.arm, tc.row, err)
		}
		if c != tc.contract {
			t.Errorf("%s/%s renders %q, want %q", tc.arm, tc.row, c, tc.contract)
		}
		d, err := tc.arm.DutyFor(tc.row)
		if err != nil {
			t.Fatalf("%s/%s duty: %v", tc.arm, tc.row, err)
		}
		if d != tc.duty {
			t.Errorf("%s/%s runs at duty %v, want %v", tc.arm, tc.row, d, tc.duty)
		}
	}
}
