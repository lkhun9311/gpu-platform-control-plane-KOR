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
	"testing"
)

func TestReclaimVariantsDifferOnlyInReclaimKnob(t *testing.T) {
	never, err := BuildFixtures(StudyReclaim, "Never", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	any, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertOneKnobDiff(StudyReclaim, never, any, "r1", "r1"); err != nil {
		t.Fatalf("reclaim variants should differ only in ReclaimWithinCohort: %v", err)
	}
}

func TestFIFOVariantsDifferOnlyInQueueingStrategy(t *testing.T) {
	strict, err := BuildFixtures(StudyFIFO, "StrictFIFO", FixtureIdentity{TxID: "tx-1", RunID: "f1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	best, err := BuildFixtures(StudyFIFO, "BestEffortFIFO", FixtureIdentity{TxID: "tx-1", RunID: "f1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertOneKnobDiff(StudyFIFO, strict, best, "f1", "f1"); err != nil {
		t.Fatalf("fifo variants should differ only in QueueingStrategy: %v", err)
	}
}

func TestOneKnobDiffWorksAcrossDifferentRunIDs(t *testing.T) {
	// Live arms use different run ids, so their cohort/flavor/queue names all differ. Canonicalizing the run
	// id must let the mechanism comparison still see a single-knob difference.
	never, err := BuildFixtures(StudyReclaim, "Never", FixtureIdentity{TxID: "tx-1", RunID: "run-a", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	any, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "run-b", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertOneKnobDiff(StudyReclaim, never, any, "run-a", "run-b"); err != nil {
		t.Fatalf("cross-run reclaim comparison should still see one knob: %v", err)
	}
}

func TestOneKnobDiffCatchesLeakedDifference(t *testing.T) {
	// If a variant silently changed a second mechanism field (here the nominal quota), the assertion must
	// catch it rather than pass because the intended knob also changed.
	never, err := BuildFixtures(StudyReclaim, "Never", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	any, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	q := any.ClusterQueue[0].Spec.ResourceGroups[0].Flavors[0].Resources[0].NominalQuota
	q.Set(5)
	any.ClusterQueue[0].Spec.ResourceGroups[0].Flavors[0].Resources[0].NominalQuota = q
	if err := AssertOneKnobDiff(StudyReclaim, never, any, "r1", "r1"); err == nil {
		t.Fatalf("a leaked quota difference must be rejected")
	}
}

func TestOneKnobDiffCatchesNoDifference(t *testing.T) {
	// Two identical variants (both Any) do not exercise the knob at all; the comparison must not silently
	// pass a study that fails to vary its mechanism.
	a, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertOneKnobDiff(StudyReclaim, a, b, "r1", "r1"); err == nil {
		t.Fatalf("identical variants should fail: the knob never varies")
	}
}
