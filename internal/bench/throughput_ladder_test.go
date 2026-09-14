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

package bench

import (
	"strings"
	"testing"
)

// ladderCell builds a cell that clears every precondition, so each test changes exactly one thing.
//
// The contender count is the registered 139 and the tail is well above the sample floor, because a fixture
// that left either at a refusing value would make every case below fail through L0 and the test would pass
// for a reason that has nothing to do with what it is about. This repository has paid for that kind of
// green more than once.
func ladderCell(rung int, topology string, ttftP99 float64) ArmSummary {
	return ArmSummary{
		Arm:            ThroughputLadderArm(rung, topology),
		Total:          4655,
		Completed:      4655,
		TTFTMsP99:      ttftP99,
		TailSampleSize: 4655,
		DispositionByTenant: map[string]Disposition{
			NoisyTenant: {Offered: ladderContenderOffers, Completed: ladderContenderOffers},
		},
	}
}

// ladderR1 is the isolated baseline cell, which carries no contender by design.
func ladderR1(rung int, ttftP99 float64) ArmSummary {
	s := ladderCell(rung, ArmR1, ttftP99)
	s.DispositionByTenant = map[string]Disposition{}
	return s
}

func ladderAnswer(t *testing.T, summaries []ArmSummary) LadderResult {
	t.Helper()
	return EvaluateThroughputLadder(summaries)
}

func TestLadderSplitSustainsAHigherRung(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 90),
		ladderCell(3, ArmShared, 900), ladderCell(3, ArmTimeSlicing, 700),
	})
	if res.Answer != "L1" {
		t.Fatalf("answer = %q, want L1; readings: %s", res.Answer, FormatThroughputLadder(res))
	}
	detail := readingDetail(res, "L1")
	if !strings.Contains(detail, "buys headroom") {
		t.Fatalf("L1 detail does not say which direction separated: %q", detail)
	}
	if !strings.Contains(detail, "rung 2") || !strings.Contains(detail, "rung 1") {
		t.Fatalf("L1 detail does not name both brackets: %q", detail)
	}
}

// The reversed direction is a separate test because it is the outcome that REVERSES the platform
// recommendation, and a reading that fired the same way for both directions would report the split buying
// headroom when it was spending it.
func TestLadderSplitCostsCapacityReversesTheRecommendation(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 90), ladderCell(2, ArmTimeSlicing, 400),
	})
	if res.Answer != "L1" {
		t.Fatalf("answer = %q, want L1", res.Answer)
	}
	detail := readingDetail(res, "L1")
	if !strings.Contains(detail, "COSTS capacity") || !strings.Contains(detail, "reverses") {
		t.Fatalf("L1 did not report the reversing direction: %q", detail)
	}
}

// A topology that met at NO rung has no rung number, and the reading must not invent one. The second paid
// ladder produced exactly this: the whole-card control breached at all three rungs and the reading said it
// "sustained the target to rung 0" -- a sentence about a cell that does not exist.
func TestLadderTopologyThatNeverMetHasNoRungNumber(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 1282), ladderCell(1, ArmTimeSlicing, 124),
		ladderCell(2, ArmShared, 1694), ladderCell(2, ArmTimeSlicing, 130),
		ladderCell(3, ArmShared, 2303), ladderCell(3, ArmTimeSlicing, 143),
	})
	if res.Answer != "L1" {
		t.Fatalf("answer = %q, want L1", res.Answer)
	}
	d := readingDetail(res, "L1")
	if strings.Contains(d, "rung 0") {
		t.Fatalf("the reading names a rung the ladder never offered: %q", d)
	}
	if !strings.Contains(d, "at no rung this ladder offered") {
		t.Fatalf("the reading does not say the control qualified nowhere: %q", d)
	}
	if !strings.Contains(d, "to rung 2") {
		t.Fatalf("the reading does not name the split's own bracket: %q", d)
	}
	// And both borderline rungs must be flagged for repetition, since the boundary rests on them.
	if len(res.RepeatRequired) != 2 || res.RepeatRequired[0] != 2 || res.RepeatRequired[1] != 3 {
		t.Fatalf("RepeatRequired = %v, want [2 3]", res.RepeatRequired)
	}
}

// The report must name the experiment it actually read, not the first study that used this formatter.
func TestLadderReportNamesTheStudyItRead(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55)})
	res.Study = StudyThroughputLadderDown
	out := FormatThroughputLadder(res)
	if !strings.Contains(out, StudyThroughputLadderDown) {
		t.Fatalf("the report does not name the study it read:\n%s", out)
	}
	if strings.Contains(out, "CAPACITY LADDER ("+StudyThroughputLadder+")") {
		t.Fatalf("the report names the wrong study, which files a result under the wrong question:\n%s", out)
	}
}

func TestLadderBothBreachAtTheSameRung(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 380),
	})
	if res.Answer != "L2" {
		t.Fatalf("answer = %q, want L2", res.Answer)
	}
	// The reading must refuse the inference a reader most wants to make from it.
	if d := readingDetail(res, "L2"); !strings.Contains(d, "NOT a measurement that the capacities are equal") {
		t.Fatalf("L2 does not refuse the equal-capacity reading: %q", d)
	}
}

func TestLadderNothingQualifiesEvenAtTheBottomRung(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 900), ladderCell(1, ArmTimeSlicing, 700),
	})
	if res.Answer != "L5" {
		t.Fatalf("answer = %q, want L5", res.Answer)
	}
	// A ladder that searched upward and found nothing has NOT measured zero capacity, and saying so is the
	// whole reason this reading is separate from L2's "both breached at the same rung".
	if d := readingDetail(res, "L5"); !strings.Contains(d, "NOT that capacity is zero") {
		t.Fatalf("L5 does not refuse the zero-capacity reading: %q", d)
	}
}

func TestLadderExhaustedGivesALowerBoundOnly(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 70), ladderCell(2, ArmTimeSlicing, 65),
	})
	if res.Answer != "L6" {
		t.Fatalf("answer = %q, want L6", res.Answer)
	}
	if d := readingDetail(res, "L6"); !strings.Contains(d, "lower bound") || !strings.Contains(d, "NOT extrapolated") {
		t.Fatalf("L6 does not report itself as a bound: %q", d)
	}
}

// A breached baseline qualifies every rung under it, so it must win over the separation reading -- otherwise
// a ladder that found this model's limit on this card would be reported as one topology out-sustaining the
// other.
func TestLadderBaselineBreachPrecedesTheSeparation(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 90),
		ladderR1(2, 300),
	})
	if res.Answer != "L4" {
		t.Fatalf("answer = %q, want L4 -- the baseline breached and that qualifies the rungs under it", res.Answer)
	}
	if d := readingDetail(res, "L4"); !strings.Contains(d, "not either topology's") {
		t.Fatalf("L4 does not say whose limit was found: %q", d)
	}
}

// A baseline that MET the target must not fire L4, or every ladder carrying an R1 cell would report the
// engine's limit regardless of what the cell said.
func TestLadderBaselineThatMetTheTargetDoesNotFire(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 90),
		ladderR1(2, 70),
	})
	if res.Answer != "L1" {
		t.Fatalf("answer = %q, want L1 -- the baseline met the target so L4 must stay silent", res.Answer)
	}
}

func TestLadderRungWithOneContendedArmIsInvalid(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400),
	})
	if res.Answer != "L0" {
		t.Fatalf("answer = %q, want L0 -- rung 2 has no split cell to compare against", res.Answer)
	}
	if d := readingDetail(res, "L0"); !strings.Contains(d, "rung 2 has no timeSlicing cell") {
		t.Fatalf("L0 does not name the missing cell: %q", d)
	}
}

// The contender is what the ladder holds fixed. A rung that moved it changed two things at once, and its
// comparison is not the one the pre-registration describes.
func TestLadderContenderOutsideToleranceIsInvalid(t *testing.T) {
	drifted := ladderCell(2, ArmTimeSlicing, 90)
	drifted.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: ladderContenderOffers + ladderContenderTolerance + 1},
	}
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), drifted,
	})
	if res.Answer != "L0" {
		t.Fatalf("answer = %q, want L0", res.Answer)
	}
	if d := readingDetail(res, "L0"); !strings.Contains(d, "varied two things at once") {
		t.Fatalf("L0 does not say why a drifted contender disqualifies the rung: %q", d)
	}
}

// Inside the tolerance it must NOT refuse, or the registered 139 +/- 2 would be a 139 exactly and two of the
// four rungs the pre-registration lists would be unscorable before they ran.
func TestLadderContenderInsideToleranceIsScored(t *testing.T) {
	within := ladderCell(1, ArmTimeSlicing, 55)
	within.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: ladderContenderOffers + ladderContenderTolerance},
	}
	res := ladderAnswer(t, []ArmSummary{ladderCell(1, ArmShared, 60), within})
	if res.Answer == "L0" {
		t.Fatalf("a contender inside the registered tolerance was refused: %s", readingDetail(res, "L0"))
	}
}

func TestLadderThinTailIsInvalidRatherThanABreach(t *testing.T) {
	thin := ladderCell(1, ArmTimeSlicing, 55)
	thin.TailSampleSize = ladderMinTailSamples - 1
	res := ladderAnswer(t, []ArmSummary{ladderCell(1, ArmShared, 60), thin})
	if res.Answer != "L0" {
		t.Fatalf("answer = %q, want L0", res.Answer)
	}
	// The distinction this whole file is built on: unscorable is not breached.
	cell := cellFor(res, ThroughputLadderArm(1, ArmTimeSlicing))
	if cell == nil || !cell.Invalid || cell.Met {
		t.Fatalf("a thin tail was scored as a verdict rather than refused: %+v", cell)
	}
}

// Censoring fails the criterion rather than invalidating the cell, because a premium tail lost to timeouts
// at a high offered rate is exactly what breaching looks like.
func TestLadderCensoredCellBreachesRatherThanRefuses(t *testing.T) {
	censored := ladderCell(2, ArmTimeSlicing, 60)
	censored.Censored = true
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), censored,
	})
	cell := cellFor(res, ThroughputLadderArm(2, ArmTimeSlicing))
	if cell == nil || cell.Invalid || cell.Met {
		t.Fatalf("a censored cell was not treated as a breach: %+v", cell)
	}
	if res.Answer != "L2" {
		t.Fatalf("answer = %q, want L2 -- both topologies last met at rung 1", res.Answer)
	}
}

// A repetition per repetition-noise is the one place the pre-registration allows a second buy, so the rule
// has to be visible in the result rather than left to a reader's eye on the table.
func TestLadderNearTheTargetDemandsARepetition(t *testing.T) {
	borderline := ladderCell(2, ArmTimeSlicing, ladderTTFTTargetMs*0.95)
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), borderline,
	})
	if len(res.RepeatRequired) != 1 || res.RepeatRequired[0] != 2 {
		t.Fatalf("RepeatRequired = %v, want [2]", res.RepeatRequired)
	}
	if !strings.Contains(FormatThroughputLadder(res), "must be repeated") {
		t.Fatalf("the report does not tell the reader which rung to repeat:\n%s", FormatThroughputLadder(res))
	}
	// And a cell far from the target must not demand one, or every ladder would ask to be re-bought.
	if c := cellFor(res, ThroughputLadderArm(1, ArmTimeSlicing)); c == nil || c.NearTarget {
		t.Fatalf("a cell far below the target was marked near it: %+v", c)
	}
}

// Two rungs must never pool into one arm. This is the property the arm naming exists to guarantee, and it
// is worth a test because the guarantee is in a format string.
func TestLadderRungsAreDistinctArms(t *testing.T) {
	seen := map[string]bool{}
	for _, arm := range throughputLadderArms() {
		if seen[arm] {
			t.Fatalf("two ladder cells share the arm name %q, so their evidence would pool", arm)
		}
		seen[arm] = true
	}
	if len(seen) != throughputLadderRungs*3 {
		t.Fatalf("the registry admits %d arms, want %d", len(seen), throughputLadderRungs*3)
	}
	study, ok := LookupStudy(StudyThroughputLadder)
	if !ok {
		t.Fatalf("the ladder study is not registered, so its runner could write arms nobody validates")
	}
	for _, arm := range throughputLadderArms() {
		if !study.Admits(arm) {
			t.Fatalf("the registry does not admit %q", arm)
		}
	}
}

// Arms belonging to another study must be ignored rather than parsed, or a report over a directory holding
// both studies' evidence would refuse or, worse, score the wrong rows.
func TestLadderIgnoresForeignArms(t *testing.T) {
	foreign := ArmSummary{Arm: ArmShared, TTFTMsP99: 1892, TailSampleSize: 9310}
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55), foreign,
	})
	for _, c := range res.Cells {
		if c.Arm == ArmShared {
			t.Fatalf("a sharing-matrix arm was scored as a ladder cell: %+v", c)
		}
	}
}

// The ORDER is registered, so it is pinned here rather than left to whichever reading happens to refuse
// first.
//
// This test exists because of a mutation that did not go red: moving L5 below L2 changed no answer, since
// L2 independently refuses when neither topology met the target. Two guards agreeing is not a reason to
// leave one of them untested -- the day the second one is relaxed, the order is all that is left, and the
// pre-registration's claim that readings are evaluated in a fixed sequence would be true of the document
// and not of the code.
func TestLadderReadingsAreEvaluatedInTheRegisteredOrder(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 380),
	})
	want := []string{"L0", "L4", "L5", "L6", "L1", "L2"}
	if len(res.Readings) != len(want) {
		t.Fatalf("evaluated %d readings, want %d: %s", len(res.Readings), len(want), FormatThroughputLadder(res))
	}
	for i, id := range want {
		if res.Readings[i].ID != id {
			t.Fatalf("reading %d is %s, want %s -- the registered order changed", i, res.Readings[i].ID, id)
		}
	}
}

// And the first that fires STOPS the evaluation, which is what makes the order load-bearing rather than
// decorative.
func TestLadderStopsAtTheFirstReadingThatFires(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{ladderCell(1, ArmShared, 60)})
	if res.Answer != "L0" {
		t.Fatalf("answer = %q, want L0", res.Answer)
	}
	if len(res.Readings) != 1 {
		t.Fatalf("evaluated %d readings after one fired, want 1", len(res.Readings))
	}
}

func TestLadderWithNoEvidenceRefuses(t *testing.T) {
	res := ladderAnswer(t, nil)
	if res.Answer != "L0" {
		t.Fatalf("answer = %q, want L0 for empty evidence", res.Answer)
	}
}

// Rungs must be their OWN comparison group, because they replay different traces on purpose. The report's
// trace-identity refusal was written against a single group and refused the ladder's own evidence the first
// time the ladder ran -- after all five cells had been measured.
func TestEachRungIsItsOwnComparisonGroup(t *testing.T) {
	if g := ArmComparisonGroup(ThroughputLadderArm(1, ArmShared)); g != "rung01" {
		t.Fatalf("group of a rung-1 cell is %q, want rung01", g)
	}
	if ArmComparisonGroup(ThroughputLadderArm(1, ArmShared)) == ArmComparisonGroup(ThroughputLadderArm(2, ArmShared)) {
		t.Fatalf("two rungs share a comparison group, so their traces would be required to agree -- which would make the ladder four measurements of one load")
	}
	if ArmComparisonGroup(ThroughputLadderArm(1, ArmShared)) != ArmComparisonGroup(ThroughputLadderArm(1, ArmTimeSlicing)) {
		t.Fatalf("the two topologies of one rung are in different groups, so nothing would check that they replayed the same trace")
	}
	// Every other study stays one group, or their identity refusal stops refusing.
	for _, arm := range []string{ArmR1, ArmShared, ArmTimeSlicing, ArmMPS, ArmDefaultFCFS, "static-cap", "kv-aware", "off"} {
		if g := ArmComparisonGroup(arm); g != "" {
			t.Fatalf("arm %q was put in group %q; every study but the ladder compares all its arms against each other", arm, g)
		}
	}
}

// Two call sites decide something load-bearing from "is this the baseline": gen-trace filters the
// contender out of its trace, and the trace-identity check exempts it because its row count differs. A
// ladder baseline they did not recognise would have carried the contender and then been refused for
// disagreeing with the arms it was supposed to be the denominator of -- silently, in both directions.
func TestIsolatedBaselineIsRecognisedInEveryStudy(t *testing.T) {
	for _, arm := range []string{ArmR1, ThroughputLadderArm(1, ArmR1), ThroughputLadderArm(throughputLadderRungs, ArmR1)} {
		if !IsIsolatedBaseline(arm) {
			t.Fatalf("%q is a baseline and was not recognised as one", arm)
		}
	}
	for _, arm := range []string{ArmShared, ArmTimeSlicing, ArmMPS, ArmDefaultFCFS,
		ThroughputLadderArm(1, ArmShared), ThroughputLadderArm(2, ArmTimeSlicing), "", "R1-ish", "rung01-R1x"} {
		if IsIsolatedBaseline(arm) {
			t.Fatalf("%q is not a baseline and was treated as one, which would filter the contender out of its trace", arm)
		}
	}
}

// The registered combination rule says the two runs are never pooled. The instrument could not honour it --
// an ArmSummary pools every file carrying its arm name -- so it refuses instead.
//
// An adversarial review built the counterexample: two replays of 300 premium completions and 70 contender
// offers each pool into 600 and 140, clearing both floors, and the ladder returns a verdict on a cell where
// neither replay would have been scorable.
func TestLadderRefusesPooledReplays(t *testing.T) {
	pooled := ladderCell(1, ArmTimeSlicing, 55)
	pooled.RepetitionCount = 2
	res := ladderAnswer(t, []ArmSummary{ladderCell(1, ArmShared, 60), pooled})
	c := cellFor(res, ThroughputLadderArm(1, ArmTimeSlicing))
	if c == nil || !c.Invalid {
		t.Fatalf("a cell pooling two replays was scored rather than refused: %+v", c)
	}
	if !strings.Contains(c.InvalidReason, "pools 2 replays") {
		t.Fatalf("the refusal does not say what is wrong: %q", c.InvalidReason)
	}
	if res.Answer != "L0" {
		t.Fatalf("answer = %q, want L0", res.Answer)
	}
}

// Censoring at or above one percent FAILS the registered criterion, so it is a breach. It used to be
// reached only after the tail-sample floor, so an arm that lost a fifth of its premium requests to timeouts
// -- which is what breaching looks like at a high offered rate -- came back INVALID for having too few
// completions left to estimate a p99 from. The instrument refused to score its own clearest failure.
func TestLadderHeavyCensoringIsABreachNotARefusal(t *testing.T) {
	drowned := ladderCell(2, ArmTimeSlicing, 60)
	drowned.Censored = true
	drowned.TailSampleSize = ladderMinTailSamples - 100 // what is left after the timeouts
	res := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), drowned,
	})
	c := cellFor(res, ThroughputLadderArm(2, ArmTimeSlicing))
	if c == nil || c.Invalid {
		t.Fatalf("a censored cell was refused instead of scored as a breach: %+v", c)
	}
	if c.Met {
		t.Fatalf("a censored cell met the criterion, which requires censoring under 1%%: %+v", c)
	}
	if c.NearTarget {
		t.Fatalf("a censored cell was judged close to the target on a p99 over the requests that survived")
	}
}

// A final report needs the isolated baseline; a between-rung verdict does not, because the baseline is
// bought last. Removing the baseline used to leave the report saying "no isolated-baseline cell breached
// the target" when none had been measured at all.
func TestLadderFinalReportRequiresABaseline(t *testing.T) {
	without := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 380),
	})
	if !without.BaselineMissing {
		t.Fatalf("a report with no baseline at the top rung did not say so")
	}
	if !strings.Contains(FormatThroughputLadder(without), "INVALID") {
		t.Fatalf("the missing baseline is not visible in the report:\n%s", FormatThroughputLadder(without))
	}
	with := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 60), ladderCell(1, ArmTimeSlicing, 55),
		ladderCell(2, ArmShared, 400), ladderCell(2, ArmTimeSlicing, 380), ladderR1(2, 70),
	})
	if with.BaselineMissing {
		t.Fatalf("a report WITH a baseline at the top rung was told it had none")
	}
}

// "This rung is not here" and "this rung stopped the ladder" are different facts, and a boolean cannot carry
// the difference. Asking about an absent rung used to come back as a registered STOP.
func TestLadderIncompleteRungIsNotAStop(t *testing.T) {
	res := ladderAnswer(t, []ArmSummary{ladderCell(2, ArmShared, 60), ladderCell(2, ArmTimeSlicing, 55)})
	if LadderRungComplete(res, 4) {
		t.Fatalf("a rung with no cells was reported complete")
	}
	if !LadderRungComplete(res, 2) {
		t.Fatalf("a rung with both topologies present and scorable was reported incomplete")
	}
	// And a rung whose pair exists but is unscorable is not complete either.
	broken := ladderCell(3, ArmTimeSlicing, 55)
	broken.TailSampleSize = 1
	res2 := ladderAnswer(t, []ArmSummary{ladderCell(3, ArmShared, 60), broken})
	if LadderRungComplete(res2, 3) {
		t.Fatalf("a rung with an unscorable cell was reported complete")
	}
}

// The stopping rule is what the runner obeys between rungs, so each of its three answers is pinned.
func TestLadderStoppingRule(t *testing.T) {
	climbing := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 400), ladderCell(1, ArmTimeSlicing, 90),
	})
	if cont, detail := LadderShouldContinue(climbing, 1); !cont {
		t.Fatalf("the ladder stopped while the split still met the target: %s", detail)
	}

	both := ladderAnswer(t, []ArmSummary{
		ladderCell(1, ArmShared, 400), ladderCell(1, ArmTimeSlicing, 380),
	})
	cont, detail := LadderShouldContinue(both, 1)
	if cont {
		t.Fatalf("the ladder kept climbing after both topologies breached: %s", detail)
	}
	if !strings.Contains(detail, "registered stopping point") {
		t.Fatalf("the stop does not say it is the registered rule: %q", detail)
	}

	// An incomplete rung is not a decision. Continuing on one would climb past a topology that was never
	// measured at this load.
	half := ladderAnswer(t, []ArmSummary{ladderCell(1, ArmShared, 60)})
	if cont, detail := LadderShouldContinue(half, 1); cont {
		t.Fatalf("the ladder climbed on a rung with one cell: %s", detail)
	}
}

func readingDetail(res LadderResult, id string) string {
	for _, r := range res.Readings {
		if r.ID == id {
			return r.Detail
		}
	}
	return ""
}

func cellFor(res LadderResult, arm string) *LadderCell {
	for i := range res.Cells {
		if res.Cells[i].Arm == arm {
			return &res.Cells[i]
		}
	}
	return nil
}
