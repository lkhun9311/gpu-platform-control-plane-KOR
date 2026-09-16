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

// The three rows of the reclaim trace, named so the protocol can address them without positional guessing.
//
// OwnRow holds tenant-a's own nominal unit for the whole run, VictimRow borrows tenant-b's idle unit and is
// the row the preemption targets, and OwnerRow is tenant-b returning to reclaim it.
const (
	OwnRow    = "a1"
	VictimRow = "a2-borrow"
	OwnerRow  = "b1-owner"
)

// Arm is the closed set of experimental conditions.
//
// It is an enum rather than a free-form study/variant pair because the previous design let any combination
// of knobs be requested, which is how an arm that the experiment never defined could still be run.
type Arm string

const (
	// ArmAHonor is reclamation enabled against a workload that stops when asked.
	ArmAHonor Arm = "A-honor"
	// ArmAIgnore is reclamation enabled against a workload that ignores the request; the contrast arm.
	ArmAIgnore Arm = "A-ignore"
	// ArmNRef is the no-reclamation reference, with workloads identical to A-honor.
	ArmNRef Arm = "N-ref"
	// ArmDFull and ArmDQuarter are the idling study, whose only axis is how much of its service the victim
	// spends computing.
	//
	// They exist because the first hardware session could not decide the reading it was built to decide.
	// Reserved GPU-seconds and observed device-seconds agreed to within a tenth of the run's floor, which
	// sounds like reservation being a good proxy for use and is not evidence of it: the trace workload
	// computes continuously by construction, so agreement was the only answer available. These two arms plant
	// a difference and ask whether the instrument recovers it.
	//
	// The termination contract is FIXED across them, at the ignoring arm's, and that is the whole point of
	// keeping this separate from the reclaim study. Two axes at once would give four cells and a session
	// twice as long, and any difference would carry both. Here the contract is a constant and the duty is the
	// only thing that moves. The ignoring contract is chosen rather than the honouring one because its hold
	// is tens of seconds rather than milliseconds, so the observer has samples inside it.
	ArmDFull    Arm = "D-full"
	ArmDQuarter Arm = "D-quarter"
)

// PolicyVariant returns the ClusterQueue reclaimWithinCohort setting this arm applies.
func (a Arm) PolicyVariant() (string, error) {
	switch a {
	case ArmAHonor, ArmAIgnore, ArmDFull, ArmDQuarter:
		return "Any", nil
	case ArmNRef:
		return "Never", nil
	default:
		return "", fmt.Errorf("unknown arm %q", a)
	}
}

// ContractFor returns the termination contract this arm renders for one trace row.
//
// The contract is per row rather than per arm because the treatment under test is the VICTIM's behaviour.
// An arm-wide switch would change all three manifests at once, so a difference in the owner's or a1's
// manifest could not be distinguished from the difference the experiment intends to measure.
func (a Arm) ContractFor(rowName string) (TerminationContract, error) {
	switch rowName {
	case OwnRow, VictimRow, OwnerRow:
	default:
		return "", fmt.Errorf("unknown trace row %q", rowName)
	}
	if _, err := a.PolicyVariant(); err != nil {
		return "", err
	}
	if rowName == VictimRow {
		switch a {
		// The idling arms hold the contract constant at the ignoring one, so the only thing that differs
		// between them is the duty. A honouring victim would stop in milliseconds and leave the observer
		// nothing to see inside the hold, which is the defect that invalidated a measured run once already.
		case ArmAIgnore, ArmDFull, ArmDQuarter:
			return IgnoresSIGTERM, nil
		}
	}
	return HonorsSIGTERM, nil
}

// DutyFor returns the fraction of its service this arm's row spends computing.
//
// Per row for the reason ContractFor is per row: the treatment is the VICTIM's behaviour, and an arm-wide
// duty would idle the owner and the co-tenant too. Their occupancy is not what any reading here is about,
// and changing three manifests when one is meant to differ is how a difference stops being attributable.
func (a Arm) DutyFor(rowName string) (DutyCycle, error) {
	switch rowName {
	case OwnRow, VictimRow, OwnerRow:
	default:
		return 0, fmt.Errorf("unknown trace row %q", rowName)
	}
	if _, err := a.PolicyVariant(); err != nil {
		return 0, err
	}
	if rowName != VictimRow {
		return FullDuty, nil
	}
	switch a {
	case ArmDQuarter:
		return QuarterDuty, nil
	default:
		return FullDuty, nil
	}
}

// AssertCardinality checks that a reconstructed run has the shape the protocol declares.
//
// It matters more than it looks: once causality is no longer inferred from cross-watch timestamps, the
// victim attempt is identified by BEING the only one open to the decision, so an unexpected count is not a
// cosmetic surprise — it means the pairing this arm relies on was not actually unambiguous.
func (a Arm) AssertCardinality(res LabResult) error {
	wantPreemptions := 1
	if a == ArmNRef {
		// Never must not reclaim; a preemption here means the applied policy was not the intended one.
		wantPreemptions = 0
	} else if _, err := a.PolicyVariant(); err != nil {
		return err
	}

	seen := map[string]WorkloadOutcome{}
	for _, o := range res.Outcomes {
		seen[o.Job] = o
	}
	for _, row := range []string{OwnRow, VictimRow, OwnerRow} {
		if _, ok := seen[row]; !ok {
			return fmt.Errorf("row %q is missing from the reconstruction", row)
		}
	}
	for _, row := range []string{OwnRow, OwnerRow} {
		if n := seen[row].Preemptions; n != 0 {
			return fmt.Errorf("row %q was preempted %d times; only the victim may be preempted", row, n)
		}
	}
	if n := seen[VictimRow].Preemptions; n != wantPreemptions {
		return fmt.Errorf("victim was preempted %d times, want %d for arm %s", n, wantPreemptions, a)
	}
	return nil
}
