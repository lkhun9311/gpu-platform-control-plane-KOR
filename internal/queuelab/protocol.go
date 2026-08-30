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
)

// PolicyVariant returns the ClusterQueue reclaimWithinCohort setting this arm applies.
func (a Arm) PolicyVariant() (string, error) {
	switch a {
	case ArmAHonor, ArmAIgnore:
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
	if a == ArmAIgnore && rowName == VictimRow {
		return IgnoresSIGTERM, nil
	}
	return HonorsSIGTERM, nil
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
