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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// runToken replaces the per-run identifier so two arms with DIFFERENT run ids can be compared on their
// mechanism alone; a raw DeepEqual would always differ on run-scoped names, cohorts, and flavors.
const runToken = "<RUN>"

// CQPolicy is the mechanism-defining projection of a ClusterQueue: every field that decides how the queue
// admits, borrows, reclaims, and orders, with run-scoped identifiers canonicalized.
//
// The review's contradiction was that a literal "exactly one JSON path differs" check must fail across
// arms whose names and references all differ by run id. Projecting to this canonical policy, then diffing
// the projections, compares the mechanism and nothing else, so the one intended knob is the only diff.
//
// The projection is deliberately broad (borrowing/lending limits, borrow-within-cohort, fungibility, stop
// policy, and EVERY covered resource and flavor quota, not just the first), because a silent difference in
// any of them, e.g. one arm capping borrowingLimit to zero, would make the arms non-comparable while a
// narrow projection still reported a single-knob difference.
type CQPolicy struct {
	// Cohort is the canonicalized borrowing scope.
	Cohort string
	// The three preemption knobs and the queueing strategy a study may vary.
	ReclaimWithinCohort string
	WithinClusterQueue  string
	BorrowWithinCohort  string
	QueueingStrategy    string
	// FlavorFungibility and StopPolicy change admission behaviour and must not silently differ.
	FlavorFungibility string
	StopPolicy        string
	// NamespaceSelectorEmpty records whether the queue admits from all namespaces, so a defaulted selector
	// that silently narrowed admission is caught.
	NamespaceSelectorEmpty bool
	// Resources is a canonical dump of every resource group: covered resources and, per flavor, the nominal
	// quota and borrowing/lending limits, with run-scoped flavor names canonicalized. A change to any quota,
	// limit, covered resource, or flavor surfaces here rather than being dropped by projecting only [0][0][0].
	Resources string
}

// ProjectCQPolicy canonicalizes one ClusterQueue into its mechanism projection under the given run id.
func ProjectCQPolicy(cq *kueuev1beta2.ClusterQueue, runID string) CQPolicy {
	p := CQPolicy{
		Cohort:           canonicalize(string(cq.Spec.CohortName), runID),
		QueueingStrategy: string(cq.Spec.QueueingStrategy),
		NamespaceSelectorEmpty: cq.Spec.NamespaceSelector != nil &&
			len(cq.Spec.NamespaceSelector.MatchLabels) == 0 &&
			len(cq.Spec.NamespaceSelector.MatchExpressions) == 0,
		Resources: canonResources(cq, runID),
	}
	if cq.Spec.Preemption != nil {
		p.ReclaimWithinCohort = string(cq.Spec.Preemption.ReclaimWithinCohort)
		p.WithinClusterQueue = string(cq.Spec.Preemption.WithinClusterQueue)
		if cq.Spec.Preemption.BorrowWithinCohort != nil {
			p.BorrowWithinCohort = string(cq.Spec.Preemption.BorrowWithinCohort.Policy)
		}
	}
	if cq.Spec.FlavorFungibility != nil {
		p.FlavorFungibility = fmt.Sprintf("%s/%s",
			cq.Spec.FlavorFungibility.WhenCanBorrow, cq.Spec.FlavorFungibility.WhenCanPreempt)
	}
	if cq.Spec.StopPolicy != nil {
		p.StopPolicy = string(*cq.Spec.StopPolicy)
	}
	return p
}

// canonResources renders every resource group's covered resources and per-flavor quotas/limits into one
// deterministic string, so any change to quota, borrowing/lending limit, covered resource, or flavor is
// compared, not just the first entry.
func canonResources(cq *kueuev1beta2.ClusterQueue, runID string) string {
	groups := make([]string, 0, len(cq.Spec.ResourceGroups))
	for _, g := range cq.Spec.ResourceGroups {
		covered := make([]string, 0, len(g.CoveredResources))
		for _, r := range g.CoveredResources {
			covered = append(covered, string(r))
		}
		sort.Strings(covered)
		flavors := make([]string, 0, len(g.Flavors))
		for _, f := range g.Flavors {
			quotas := make([]string, 0, len(f.Resources))
			for _, q := range f.Resources {
				quotas = append(quotas, fmt.Sprintf("%s=nom:%s,borrow:%s,lend:%s",
					q.Name, q.NominalQuota.String(), quantityOrNil(q.BorrowingLimit), quantityOrNil(q.LendingLimit)))
			}
			sort.Strings(quotas)
			flavors = append(flavors, fmt.Sprintf("flavor:%s{%s}", canonicalize(string(f.Name), runID), strings.Join(quotas, ",")))
		}
		sort.Strings(flavors)
		groups = append(groups, fmt.Sprintf("covered:[%s];%s", strings.Join(covered, ","), strings.Join(flavors, ";")))
	}
	sort.Strings(groups)
	return strings.Join(groups, "|")
}

// quantityOrNil renders a resource quantity pointer, distinguishing an unset limit ("nil") from a zero one.
func quantityOrNil(q *resource.Quantity) string {
	if q == nil {
		return "nil"
	}
	return q.String()
}

// canonicalize replaces a trailing "-"+runID suffix on an identifier with a run token, leaving the rest
// intact. It strips only the exact generated suffix (not every occurrence), so a short run id like "a"
// cannot rewrite fixed text such as "tenant-a", and two different environment names sharing a suffix are
// not collapsed.
func canonicalize(name, runID string) string {
	if runID == "" {
		return name
	}
	if base, ok := strings.CutSuffix(name, "-"+runID); ok {
		return base + "-" + runToken
	}
	return name
}

// DiffCQPolicy returns the sorted names of the projection fields that differ between two policies, so a
// caller can assert the difference is EXACTLY the study's intended knob and nothing leaked.
func DiffCQPolicy(a, b CQPolicy) []string {
	var diffs []string
	add := func(name string, differ bool) {
		if differ {
			diffs = append(diffs, name)
		}
	}
	add("Cohort", a.Cohort != b.Cohort)
	add("ReclaimWithinCohort", a.ReclaimWithinCohort != b.ReclaimWithinCohort)
	add("WithinClusterQueue", a.WithinClusterQueue != b.WithinClusterQueue)
	add("BorrowWithinCohort", a.BorrowWithinCohort != b.BorrowWithinCohort)
	add("QueueingStrategy", a.QueueingStrategy != b.QueueingStrategy)
	add("FlavorFungibility", a.FlavorFungibility != b.FlavorFungibility)
	add("StopPolicy", a.StopPolicy != b.StopPolicy)
	add("NamespaceSelectorEmpty", a.NamespaceSelectorEmpty != b.NamespaceSelectorEmpty)
	add("Resources", a.Resources != b.Resources)
	sort.Strings(diffs)
	return diffs
}

// studyKnobField is the single projection field each study is allowed to vary between its variants.
func studyKnobField(study Study) (string, error) {
	switch study {
	case StudyReclaim:
		return "ReclaimWithinCohort", nil
	case StudyFIFO:
		return "QueueingStrategy", nil
	default:
		return "", fmt.Errorf("unknown study %q", study)
	}
}

// AssertOneKnobDiff verifies that two variants of a study differ, across their ClusterQueues, in EXACTLY
// the study's one intended policy field and nowhere else.
//
// It requires EVERY paired ClusterQueue to differ in exactly the knob. Requiring only that some queue shows
// the knob (and none leaks anything else) would pass a broken reclaim arm that flipped just one tenant's
// queue, leaving the other at the old policy so no reclamation actually occurs. It pairs queues by their
// canonicalized name, so it works whether the arms share a run id or not.
func AssertOneKnobDiff(study Study, a, b *FixtureSet, runIDa, runIDb string) error {
	knob, err := studyKnobField(study)
	if err != nil {
		return err
	}
	aByName := map[string]CQPolicy{}
	for _, cq := range a.ClusterQueue {
		aByName[canonicalize(cq.Name, runIDa)] = ProjectCQPolicy(cq, runIDa)
	}
	bByName := map[string]CQPolicy{}
	for _, cq := range b.ClusterQueue {
		bByName[canonicalize(cq.Name, runIDb)] = ProjectCQPolicy(cq, runIDb)
	}
	if len(aByName) != len(bByName) {
		return fmt.Errorf("arms have different ClusterQueue counts: %d vs %d", len(aByName), len(bByName))
	}
	for name, ap := range aByName {
		bp, ok := bByName[name]
		if !ok {
			return fmt.Errorf("ClusterQueue %q present in one arm but not the other", name)
		}
		diffs := DiffCQPolicy(ap, bp)
		if len(diffs) != 1 || diffs[0] != knob {
			return fmt.Errorf("ClusterQueue %q must differ in exactly %q, got diffs %v", name, knob, diffs)
		}
	}
	return nil
}
