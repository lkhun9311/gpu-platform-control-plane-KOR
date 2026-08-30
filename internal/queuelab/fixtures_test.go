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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// Lowercase because these become object NAMES. The earlier values were "runA"/"runB", which no apiserver
// would have accepted — DNS-1123 labels are lowercase — so the fixture was exercising a run id a real run
// could never have. BuildFixtures' identity validation is what surfaced that.
const runA = "run-a"

func TestReclaimFixturesVaryOnlyReclaimPolicy(t *testing.T) {
	never, err := BuildFixtures(StudyReclaim, "Never", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	any, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}

	if len(never.ClusterQueue) != 2 || len(never.LocalQueue) != 2 {
		t.Fatalf("reclaim study needs two per-tenant queues")
	}
	// Both tenants share one cohort so borrowing/reclaim is possible.
	for _, cq := range never.ClusterQueue {
		if string(cq.Spec.CohortName) != cohortName("r1") {
			t.Fatalf("ClusterQueue %s not in the shared cohort", cq.Name)
		}
		if cq.Spec.Preemption.ReclaimWithinCohort != kueuev1beta2.PreemptionPolicyNever {
			t.Fatalf("Never variant should set ReclaimWithinCohort=Never")
		}
	}
	// The one varied knob: Never vs Any.
	for i := range any.ClusterQueue {
		if any.ClusterQueue[i].Spec.Preemption.ReclaimWithinCohort != kueuev1beta2.PreemptionPolicyAny {
			t.Fatalf("Any variant should set ReclaimWithinCohort=Any")
		}
		// Everything else (name, cohort, nominal quota) is identical between the two variants at the same runID.
		if any.ClusterQueue[i].Name != never.ClusterQueue[i].Name {
			t.Fatalf("variants must share queue names at the same runID")
		}
		nq := any.ClusterQueue[i].Spec.ResourceGroups[0].Flavors[0].Resources[0].NominalQuota
		if nq.Value() != 1 {
			t.Fatalf("reclaim tenant nominal quota should be 1, got %d", nq.Value())
		}
	}
}

func TestFIFOFixturesVaryQueueingStrategy(t *testing.T) {
	strict, err := BuildFixtures(StudyFIFO, "StrictFIFO", FixtureIdentity{TxID: "tx-1", RunID: "f1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	best, err := BuildFixtures(StudyFIFO, "BestEffortFIFO", FixtureIdentity{TxID: "tx-1", RunID: "f1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if len(strict.ClusterQueue) != 1 {
		t.Fatalf("fifo study uses one ClusterQueue")
	}
	if strict.ClusterQueue[0].Spec.QueueingStrategy != kueuev1beta2.StrictFIFO {
		t.Fatalf("StrictFIFO variant not set")
	}
	if best.ClusterQueue[0].Spec.QueueingStrategy != kueuev1beta2.BestEffortFIFO {
		t.Fatalf("BestEffortFIFO variant not set")
	}
	nq := strict.ClusterQueue[0].Spec.ResourceGroups[0].Flavors[0].Resources[0].NominalQuota
	if nq.Value() != 2 {
		t.Fatalf("fifo capacity should be 2, got %d", nq.Value())
	}
}

func TestFixtureNamesAreUniquePerRun(t *testing.T) {
	a, _ := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: runA, Namespace: "ns"})
	b, _ := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "run-b", Namespace: "ns"})
	if a.ClusterQueue[0].Name == b.ClusterQueue[0].Name {
		t.Fatalf("different runs must not share queue names: %s", a.ClusterQueue[0].Name)
	}
	if !strings.Contains(a.ClusterQueue[0].Name, runA) {
		t.Fatalf("queue name should carry the run id, got %s", a.ClusterQueue[0].Name)
	}
	// The review's isolation fix: different runs must be in DIFFERENT cohorts and use DIFFERENT flavors,
	// so a delayed old ClusterQueue cannot contribute quota into a new run's cohort.
	if a.ClusterQueue[0].Spec.CohortName == b.ClusterQueue[0].Spec.CohortName {
		t.Fatalf("different runs must not share a cohort: %s", a.ClusterQueue[0].Spec.CohortName)
	}
	if a.Flavor.Name == b.Flavor.Name {
		t.Fatalf("different runs must not share a ResourceFlavor: %s", a.Flavor.Name)
	}
	// The flavor must pin one dedicated worker so a 2-GPU pod maps to a single node.
	if a.Flavor.Spec.NodeLabels[labWorkerLabel] != runA {
		t.Fatalf("flavor should select the run's dedicated worker, got %v", a.Flavor.Spec.NodeLabels)
	}
	// The flavor must taint the worker AND carry the matching toleration Kueue injects, or admitted pods
	// could not schedule onto the isolated node.
	if len(a.Flavor.Spec.NodeTaints) != 1 || a.Flavor.Spec.NodeTaints[0].Value != runA {
		t.Fatalf("flavor should taint the dedicated worker for the run, got %v", a.Flavor.Spec.NodeTaints)
	}
	// The worker toleration must be PRESENT, not the only one. The count used to be pinned at 1, which broke
	// the moment the flavour also had to tolerate the GPU node group's own nvidia.com/gpu taint -- a
	// toleration whose absence left every trace Pod Pending on real hardware. What matters here is that the
	// taint this flavour applies is one this flavour tolerates; how many others it carries is not this
	// test's business.
	var worker *corev1.Toleration
	for i := range a.Flavor.Spec.Tolerations {
		if a.Flavor.Spec.Tolerations[i].Key == a.Flavor.Spec.NodeTaints[0].Key {
			worker = &a.Flavor.Spec.Tolerations[i]
		}
	}
	if worker == nil || worker.Value != runA {
		t.Fatalf("flavor should tolerate its own worker taint, got %v", a.Flavor.Spec.Tolerations)
	}
	// Lab objects must be labelled for the reset audit.
	if a.ClusterQueue[0].Labels[runLabel] != runA {
		t.Fatalf("ClusterQueue should carry the run label")
	}
}

// The identity fields become object NAMES and ownership LABELS, so an empty one does not render less — it
// renders a different, wrong object. teardown.go refuses these same empties, which is what makes them
// invariants; refusing them only there means the run creates the wrong objects first and finds out when it
// tries to remove them.
//
// Mutation that turns this red: drop the id.validate() call from BuildFixtures.
func TestBuildFixturesRejectsAnUnusableIdentity(t *testing.T) {
	good := FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"}

	rows := []struct {
		name  string
		id    FixtureIdentity
		field string
	}{
		{"empty TxID", FixtureIdentity{RunID: "r1", Namespace: "ns"}, "TxID"},
		{"empty RunID", FixtureIdentity{TxID: "tx-1", Namespace: "ns"}, "RunID"},
		{"empty Namespace", FixtureIdentity{TxID: "tx-1", RunID: "r1"}, "Namespace"},
		// Non-empty but unrenderable. Without this row a check that only tested for "" would pass, and the
		// apiserver would reject the object at Create with an error naming a generated name rather than the
		// field that produced it.
		{"RunID with an underscore", FixtureIdentity{TxID: "tx-1", RunID: "r_1", Namespace: "ns"}, "RunID"},
		{"Namespace in capitals", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "NS"}, "Namespace"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := BuildFixtures(StudyReclaim, "Any", row.id)
			if err == nil {
				t.Fatalf("accepted %s", row.name)
			}
			if !strings.Contains(err.Error(), row.field) {
				t.Fatalf("error does not name the offending field %s: %v", row.field, err)
			}
		})
	}

	// The control: a usable identity must still build. Without it, a validate() that rejected everything
	// would satisfy every row above.
	if _, err := BuildFixtures(StudyReclaim, "Any", good); err != nil {
		t.Fatalf("refused a usable identity: %v", err)
	}
}

func TestBuildFixturesRejectsBadInput(t *testing.T) {
	if _, err := BuildFixtures(StudyReclaim, "sometimes", FixtureIdentity{TxID: "tx-1", RunID: "r", Namespace: "ns"}); err == nil {
		t.Fatalf("bad reclaim variant should error")
	}
	if _, err := BuildFixtures("nope", "Any", FixtureIdentity{TxID: "tx-1", RunID: "r", Namespace: "ns"}); err == nil {
		t.Fatalf("unknown study should error")
	}
}

// The stamp is what lets teardown tell this run's objects from a previous run's under the same name, and it
// has to be on every object the builder produces — an unstamped one is unrecoverable and undeletable.
func TestEveryFixtureCarriesTheTransactionStamp(t *testing.T) {
	fs, err := BuildFixtures(StudyReclaim, "Any", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "queuelab-r1"})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	objs := []metav1.Object{fs.Flavor}
	for _, cq := range fs.ClusterQueue {
		objs = append(objs, cq)
	}
	for _, lq := range fs.LocalQueue {
		objs = append(objs, lq)
	}
	if len(objs) < 3 {
		t.Fatalf("expected at least a flavor, a cluster queue and a local queue, got %d objects", len(objs))
	}
	for _, o := range objs {
		if got := o.GetLabels()[TxLabel]; got != "tx-1" {
			t.Errorf("%s carries tx stamp %q, want tx-1", o.GetName(), got)
		}
	}
}
