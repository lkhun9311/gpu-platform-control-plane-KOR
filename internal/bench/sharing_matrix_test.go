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

// healthyArm builds an arm that clears every floor, so each test below changes exactly one thing.
//
// Built rather than written out because the readings share a lot of preconditions: an arm has to have a
// hundred premium completions, an uncensored tail, a premium TPOT and a contender disposition before any
// bar can be applied to it, and a test that spelled all of that out per case would hide which field it was
// actually about.
func healthyArm(name string, ttftP99, tpotP99 float64, contenderTokens int64) ArmSummary {
	// OutputTokens is set, and it is not decoration: shareOf divides by it, so a fixture that left it at
	// zero would make every tenant's share zero and every arm unscorable -- the tests would then pass or
	// fail for a reason that has nothing to do with what they are about.
	return ArmSummary{
		Arm:          name,
		Total:        4000,
		Completed:    3900,
		OutputTokens: 50_000 + contenderTokens,
		// ActiveSeconds is set because reading 1's price is the PREMIUM tenant's tokens per second, which is
		// derived from per-tenant tokens and the arm's own sending time rather than read off the arm's
		// aggregate rate. An arm without it has no premium throughput to report.
		ActiveSeconds:         1000,
		TTFTMsP99:             ttftP99,
		TailSampleSize:        3000,
		RepetitionCount:       3,
		RepetitionTTFTMsP99:   []float64{ttftP99 - 1, ttftP99, ttftP99 + 1},
		OutputTokensPerSecond: 40,
		TPOTMsP99ByTenant:     map[string]float64{PremiumTenant: tpotP99},
		OutputTokensByTenant:  map[string]int64{PremiumTenant: 50_000, NoisyTenant: contenderTokens},
		DispositionByTenant: map[string]Disposition{
			NoisyTenant: {Offered: 300, Completed: 250, TimedOut: 50},
		},
	}
}

// healthyMatrix is a run where the load worked and the split arms improved on the control without reaching
// the bar -- the reading-5 shape, chosen as the baseline because it is the one every INVALID reading has to
// stay silent about.
func healthyMatrix() SharingArms {
	r1 := healthyArm(ArmR1, 67.3, 17.8, 0)
	r1.OutputTokensPerSecond = 46
	r1.DispositionByTenant = map[string]Disposition{}
	return SharingArms{
		R1:     r1,
		Shared: healthyArm(ArmShared, 1400, 80, 40_000),
		Sharing: []ArmSummary{
			healthyArm(ArmTimeSlicing, 900, 60, 38_000),
			healthyArm(ArmMPS, 1000, 62, 37_000),
		},
	}
}

func sharingReadingByID(t *testing.T, res SharingResult, id string) PoPReading {
	t.Helper()
	for _, r := range res.Readings {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("reading %s was not evaluated at all; the readings present were %v", id, idsOf(res))
	return PoPReading{}
}

func idsOf(res SharingResult) []string {
	out := make([]string, 0, len(res.Readings))
	for _, r := range res.Readings {
		out = append(out, r.ID)
	}
	return out
}

// THE ORDERING TRAP, and it is the reason reading 1 has a third clause.
//
// Reading 2's condition -- meets both bars AND starves the contender -- was a strict subset of reading 1's
// two-clause form. With "the first that fires is the answer", an arm that bought its tail by refusing the
// other tenant's work would have been reported as the deliverable and reading 2 would never have been
// reached. The pre-registration was corrected before any card was rented; this is what holds the correction.
func TestAnArmThatBuysItsTailByStarvingTheContenderIsNegativeNotPositive(t *testing.T) {
	m := healthyMatrix()
	// Meets both bars, and takes the contender's work to do it: a fifth of the control's output, and the
	// ledger shows the loss as refusals rather than as delay.
	starving := healthyArm(ArmTimeSlicing, 100, 20, 5_000)
	starving.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 300, Completed: 120, Rejected: 170, TimedOut: 10},
	}
	m.Sharing = []ArmSummary{starving}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	one := sharingReadingByID(t, res, "1")
	if one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm that met the bars by refusing 170 of the contender's "+
			"requests: %s", one.Detail)
	}
	two := sharingReadingByID(t, res, "2")
	if !two.Fired {
		t.Fatalf("reading 2 did not fire for a starving arm, so the outcome has no reading at all: %s", two.Detail)
	}
	if !strings.HasPrefix(res.Answer, "2") {
		t.Errorf("the answer is %q; an arm that starves the contender to meet the bars is reading 2, the NEGATIVE one", res.Answer)
	}
	// The detail has to show the ledger, because the pre-registration forbids calling a smaller share
	// starvation without it.
	if !strings.Contains(two.Detail, "170") {
		t.Errorf("reading 2's detail does not carry the rejection count that makes it starvation rather than delay: %s", two.Detail)
	}
}

// The same arm, losing the same share to timeouts rather than refusal, is not reading 2 -- and not reading
// 1 either.
//
// AMENDED 2026-09-12. This test used to require reading 1 to fire POSITIVE here, on the argument that work
// which was not refused was "merely delayed". A cold review took the second half of that apart: the ledger
// says 180 of the contender's 300 requests never came back, and a request that timed out was not served
// late, it was not served. Calling that delay and then crediting the arm with a POSITIVE is the one way a
// calm premium tail can be manufactured -- the tail is quiet because the load the arm was supposed to be
// protected FROM evaporated. Reading 1's own detail line would have printed the collapsed contender share
// as though it were the price of the protection.
//
// The first half of the original argument stands and is what this test still holds: a share that fell
// without rejections is NOT starvation, because the pre-registration says starvation has to be shown in
// the ledger rather than inferred from a smaller number. The correction is that the remaining outcome is
// "this arm cannot be scored", not "this arm wins".
func TestAContenderThatTimedOutRatherThanBeingRefusedIsNeitherStarvedNorProtected(t *testing.T) {
	m := healthyMatrix()
	lost := healthyArm(ArmTimeSlicing, 100, 20, 5_000)
	lost.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 300, Completed: 120, TimedOut: 180},
	}
	m.Sharing = []ArmSummary{lost}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if two := sharingReadingByID(t, res, "2"); two.Fired {
		t.Errorf("reading 2 fired on a share that fell to timeouts with no rejections; the pre-registration "+
			"says delay and deletion are different findings and only the second is this reading: %s", two.Detail)
	}
	one := sharingReadingByID(t, res, "1")
	if one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm that completed 120 of the contender's 300 requests "+
			"with none rejected. Whether the premium tail is calm because the split protected it or because "+
			"60%% of the contending load went missing is exactly what this evidence cannot say: %s", one.Detail)
	}
	if !strings.Contains(one.Detail, "went missing") {
		t.Errorf("reading 1 declined without naming the lost contender work, leaving a reader to guess which "+
			"of the arm's numbers disqualified it: %s", one.Detail)
	}
}

// An arm whose premium requests ALL timed out must not clear the bars with a zero tail.
//
// percentile() returns 0 for an empty slice, so 0/67.3 clears a 2x bar and 0 clears a 1.25x one. The
// price-of-protection evaluator records this as the worst wrong number it could print: the arm that starved
// the protected tenant completely reported as the one that protected it.
func TestAnArmWithNoPremiumTailIsNotScoredAgainstTheBars(t *testing.T) {
	m := healthyMatrix()
	collapsed := healthyArm(ArmTimeSlicing, 0, 0, 38_000)
	collapsed.TPOTMsP99ByTenant = map[string]float64{}
	collapsed.TailSampleSize = 0
	m.Sharing = []ArmSummary{collapsed}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	// It is caught before the bars are ever applied: an arm with no completions is reading 4b's business.
	fourB := sharingReadingByID(t, res, "4b")
	if !fourB.Fired {
		t.Fatalf("reading 4b did not fire for an arm with zero premium completions: %s", fourB.Detail)
	}
	if res.Answer != "4b" {
		t.Errorf("the answer is %q; an arm that completed nothing makes the run INVALID through reading 4b", res.Answer)
	}
	for _, r := range res.Readings {
		if r.ID == "1" && r.Fired {
			t.Error("reading 1 fired POSITIVE beneath an INVALID reading, which is the wrong number this " +
				"whole file exists to refuse")
		}
	}
}

// R1 has no contender by construction, so reading 4b's contender floor must not be applied to it.
//
// Applying it would fire INVALID on every run that was working perfectly, which is the failure mode of a
// guard that cannot tell "absent by design" from "absent because something broke".
func TestReadingFourBDoesNotDemandAContenderOfTheIsolatedBaseline(t *testing.T) {
	m := healthyMatrix()
	if len(m.R1.DispositionByTenant) != 0 {
		t.Fatal("this test's premise is that R1 carries no contender disposition")
	}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if fourB := sharingReadingByID(t, res, "4b"); fourB.Fired {
		t.Errorf("reading 4b fired because the isolated baseline has no contender, which is what an "+
			"isolated baseline IS: %s", fourB.Detail)
	}
}

// Reading 4 guards the low side: a control that produced no contention is not a study of protection.
func TestReadingFourFiresWhenTheControlBarelyContends(t *testing.T) {
	m := healthyMatrix()
	m.Shared = healthyArm(ArmShared, 200, 20, 40_000) // 3.0x R1, under the 5x threshold

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	four := sharingReadingByID(t, res, "4")
	if !four.Fired {
		t.Fatalf("reading 4 did not fire at 3x R1 against a 5x threshold: %s", four.Detail)
	}
	if len(res.Readings) != 1 {
		t.Errorf("evaluation continued past an INVALID reading and produced %v; everything below it would be "+
			"scoring comparisons that do not mean anything", idsOf(res))
	}
}

// Reading 4c fires on a RECORDED refusal, and declines on mere absence.
func TestReadingFourCSeparatesARefusalFromAnAbsence(t *testing.T) {
	t.Run("a recorded refusal fires it", func(t *testing.T) {
		m := healthyMatrix()
		m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 900, 60, 38_000)}
		m.Refused = map[string]string{ArmMPS: "the MPS control daemon never became ready"}

		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		fourC := sharingReadingByID(t, res, "4c")
		if !fourC.Fired {
			t.Fatalf("reading 4c did not fire on a recorded refusal: %s", fourC.Detail)
		}
		if fourC.Cell != ArmMPS {
			t.Errorf("reading 4c names %q rather than the arm that was refused", fourC.Cell)
		}
	})

	t.Run("bare absence is not evaluable", func(t *testing.T) {
		m := healthyMatrix()
		m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 900, 60, 38_000)}

		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		fourC := sharingReadingByID(t, res, "4c")
		if fourC.Fired {
			t.Errorf("reading 4c fired on an arm that is merely absent; absence is equally consistent with "+
				"an interruption or an operator running a subset, and calling it a sharing-mode failure "+
				"attributes a result to a cause the evidence does not establish: %s", fourC.Detail)
		}
		if !fourC.NotEvaluable {
			t.Errorf("reading 4c reported a plain negative for an arm it has no evidence about: %s", fourC.Detail)
		}
	})
}

// Reading 5 is the outcome the previous study had no name for, and it must actually fire.
func TestReadingFiveFiresOnARealImprovementThatMissesTheBar(t *testing.T) {
	res := EvaluateSharingMatrix(healthyMatrix(), PremiumTenant, NoisyTenant)

	if one := sharingReadingByID(t, res, "1"); one.Fired {
		t.Fatalf("reading 1 fired at 13x R1 against a 2x bar: %s", one.Detail)
	}
	if three := sharingReadingByID(t, res, "3"); three.Fired {
		t.Fatalf("reading 3 fired despite a 500 ms improvement against a 2 ms spread: %s", three.Detail)
	}
	five := sharingReadingByID(t, res, "5")
	if !five.Fired {
		t.Fatalf("reading 5 did not fire; the evidence would then land in the same gap the previous study's "+
			"readings left, which is exactly what this reading was added to close: %s", five.Detail)
	}
	// Both distances, because a partial result is only useful if a reader can see how far it still is, and
	// an arm can be close on one bar and nowhere on the other.
	for _, want := range []string{"against 2.0x", "against 1.25"} {
		if !strings.Contains(five.Detail, want) {
			t.Errorf("reading 5 does not report the remaining distance to %q, which is the half of it that "+
				"makes a partial result useful: %s", want, five.Detail)
		}
	}
}

// Reading 3 fires when no arm beats the control by more than the control's own noise.
func TestReadingThreeFiresWhenNoArmBeatsTheControlsOwnSpread(t *testing.T) {
	m := healthyMatrix()
	m.Shared = healthyArm(ArmShared, 1400, 80, 40_000)
	m.Shared.RepetitionTTFTMsP99 = []float64{1200, 1400, 1600} // spread 400 ms
	m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 1350, 78, 38_000)}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	three := sharingReadingByID(t, res, "3")
	if !three.Fired {
		t.Fatalf("reading 3 did not fire on a 50 ms improvement against a 400 ms spread: %s", three.Detail)
	}
	if five := sharingReadingByID(t, res, "5"); five.Fired {
		t.Error("reading 5 also fired; an improvement inside the control's own noise is reading 3, and two " +
			"readings claiming one outcome is the overlap this study was corrected for")
	}
}

// One repetition means there is no spread, and the readings that compare against one must decline.
func TestTheSpreadReadingsDeclineAtOneRepetition(t *testing.T) {
	m := healthyMatrix()
	// EVERY arm at one repetition, not only the control. Reading 4b now refuses a matrix whose arms were
	// repeated unequally, because pooling two instances against a spread measured over three weights
	// instance variation differently by arm -- so a fixture that changed the control alone was describing a
	// run that would be refused before these readings were reached.
	one := func(s *ArmSummary) { s.RepetitionCount = 1; s.RepetitionTTFTMsP99 = s.RepetitionTTFTMsP99[:1] }
	one(&m.R1)
	one(&m.Shared)
	for i := range m.Sharing {
		one(&m.Sharing[i])
	}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	for _, id := range []string{"3", "5"} {
		r := sharingReadingByID(t, res, id)
		if r.Fired {
			t.Errorf("reading %s fired against a spread that does not exist at one repetition: %s", id, r.Detail)
		}
		if !r.NotEvaluable {
			t.Errorf("reading %s reported a plain negative rather than declining; with one repetition it is "+
				"comparing against noise nobody measured: %s", id, r.Detail)
		}
	}
}

// Reading 1's tie-break goes to timeSlicing, because MPS needs a daemon that can be absent.
func TestAnExactTieGoesToTimeSlicing(t *testing.T) {
	m := healthyMatrix()
	ts := healthyArm(ArmTimeSlicing, 100, 20, 38_000)
	mps := healthyArm(ArmMPS, 100, 20, 38_000)
	// MPS listed first, so a naive "last one wins" or "first one wins" would pick it.
	m.Sharing = []ArmSummary{mps, ts}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	one := sharingReadingByID(t, res, "1")
	if !one.Fired {
		t.Fatalf("reading 1 did not fire for two arms that both met the bars: %s", one.Detail)
	}
	if one.Cell != ArmTimeSlicing {
		t.Errorf("an exact tie went to %q; the pre-registration breaks ties toward timeSlicing because MPS "+
			"needs a control daemon that can be absent and the simpler mechanism is the smaller claim", one.Cell)
	}
}

// The deliverable has to carry its price, because a pass/fail line reports a tenth and a half identically.
func TestReadingOneReportsWhatTheProtectionCost(t *testing.T) {
	m := healthyMatrix()
	won := healthyArm(ArmTimeSlicing, 100, 20, 38_000)
	// The same premium tokens over twice the time: half R1's premium throughput. The arm's AGGREGATE rate is
	// deliberately left high, because reporting that instead of the premium tenant's is the defect this
	// test pins -- an arm serving premium at half speed while a contender fills the gap reads as 1.00 if the
	// aggregate is used.
	won.ActiveSeconds = 2000
	won.OutputTokensPerSecond = 999
	m.Sharing = []ArmSummary{won}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	one := sharingReadingByID(t, res, "1")
	if !one.Fired {
		t.Fatalf("reading 1 did not fire: %s", one.Detail)
	}
	if !strings.Contains(one.Detail, "0.50 of R1's throughput") {
		t.Errorf("reading 1's detail does not carry the premium tenant's throughput as a fraction of R1's, "+
			"which the pre-registration requires in the write-up's first sentence: %s", one.Detail)
	}
}

// A run where nothing fired must say so as a gap rather than printing a verdict.
func TestNoReadingFiringIsReportedAsAGapRatherThanAnAnswer(t *testing.T) {
	m := healthyMatrix()
	// Every arm unscorable for a reason 4b does not catch: a censored tail.
	censored := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
	censored.Censored = true
	m.Sharing = []ArmSummary{censored}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
	out := FormatSharingMatrix(res)

	// ASSERTED, not logged. This block used to t.Logf an unexpected answer and then look for the substring
	// "INVALID", which appears in reading NAMES whether or not one fired -- so it would have accepted an
	// invented answer while claiming to test that none was invented.
	for _, r := range res.Readings {
		if r.ID == "1" && r.Fired {
			t.Errorf("reading 1 fired on an arm whose tail is a lower bound rather than a p99: %s", r.Detail)
		}
	}
	if res.Answer != "" {
		t.Errorf("an answer of %q was produced from evidence in which the only sharing arm has a censored "+
			"tail. No reading can conclude from a lower bound", res.Answer)
	}
	if !strings.Contains(out, "censored") {
		t.Errorf("the report does not name the censored tail, so a reader cannot tell whether the readings "+
			"looked at everything or at nothing:\n%s", out)
	}
}

// An arm with a healthy premium tail but NO premium TPOT must not clear the stream bar with a zero.
//
// This is the same defect as a missing TTFT and it hides better, because reading 4b does not catch it:
// TailSampleSize counts completions and can be well over the floor while every stream broke after its first
// token, leaving TPOTMsP99ByTenant without an entry. The ratio is then 0/17.8, which clears a 1.25x bar the
// way an empty tail clears a 2x one -- so the arm whose streams all died would be scored as the one that
// kept them alive.
//
// It exists because removing the arm's own numerators from the computable check left every other test in
// this file green. A guard nothing exercises is a guard that will be deleted by someone tidying up.
func TestAnArmWithNoPremiumTPOTIsNotScoredAgainstTheStreamBar(t *testing.T) {
	m := healthyMatrix()
	broken := healthyArm(ArmTimeSlicing, 100, 0, 38_000)
	broken.TPOTMsP99ByTenant = map[string]float64{} // first tokens arrived; no stream produced a second
	m.Sharing = []ArmSummary{broken}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if fourB := sharingReadingByID(t, res, "4b"); fourB.Fired {
		t.Fatalf("this test's premise is that 4b does NOT catch this arm, and it did: %s", fourB.Detail)
	}
	one := sharingReadingByID(t, res, "1")
	if one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm with no premium TPOT at all; 0/17.8 clears the 1.25x "+
			"stream bar, so the arm whose streams all broke would be reported as the one that protected "+
			"them: %s", one.Detail)
	}
	if !strings.Contains(one.Detail, "no premium TPOT") {
		t.Errorf("reading 1 did not say WHY it could not score the arm, so a reader cannot tell it from a "+
			"reading that looked and found nothing: %s", one.Detail)
	}
}

// Reading 2 asks about the contender's OUTPUT, not its share of an arm's total.
//
// The two come apart exactly when the split costs both tenants alike, and a share ratio then reports that
// nothing was lost. This is the case an independent review raised, with its numbers: a control producing
// 60k premium and 40k contender against an arm producing 30k and 20k has given the contender half as much
// work, while its share is 40 percent in both. Scored as a share the arm reports 1.00, reading 2 never
// fires, and reading 1 calls an arm that discarded half the contender's work the deliverable.
func TestTheContenderIsMeasuredByItsOutputAndNotItsShare(t *testing.T) {
	m := healthyMatrix()
	m.Shared = healthyArm(ArmShared, 1400, 80, 40_000)
	m.Shared.OutputTokensByTenant = map[string]int64{PremiumTenant: 60_000, NoisyTenant: 40_000}
	m.Shared.OutputTokens = 100_000

	halved := healthyArm(ArmTimeSlicing, 100, 20, 20_000)
	halved.OutputTokensByTenant = map[string]int64{PremiumTenant: 30_000, NoisyTenant: 20_000}
	halved.OutputTokens = 50_000
	// The ledger shows the missing work was refused rather than delayed.
	halved.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 300, Completed: 150, Rejected: 150},
	}
	m.Sharing = []ArmSummary{halved}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if one := sharingReadingByID(t, res, "1"); one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm that produced half the contender's output. Its SHARE "+
			"is unchanged at 40 percent, which is why a share ratio misses this: %s", one.Detail)
	}
	two := sharingReadingByID(t, res, "2")
	if !two.Fired {
		t.Fatalf("reading 2 did not fire for an arm that halved the contender's output with the ledger "+
			"showing refusals: %s", two.Detail)
	}
	if !strings.Contains(two.Detail, "0.50") {
		t.Errorf("reading 2 reports the contender at something other than 0.50 of its output under the "+
			"control, so it is still measuring a share: %s", two.Detail)
	}
}

// A shortfall that is mostly timeouts is delay, and the pre-registration says delay is not this reading.
//
// The check was `Rejected+Failed > 0`, so a single broken stream among thousands of timeouts printed
// "refused work, not merely late work" over a ledger that principally said late -- and Failed counts
// transport breaks, which a flaky tunnel produces on its own.
func TestOneBrokenStreamAmongManyTimeoutsIsNotStarvation(t *testing.T) {
	m := healthyMatrix()
	arm := healthyArm(ArmTimeSlicing, 100, 20, 5_000)
	arm.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 3000, Completed: 120, TimedOut: 2879, Failed: 1},
	}
	m.Sharing = []ArmSummary{arm}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if two := sharingReadingByID(t, res, "2"); two.Fired {
		t.Errorf("reading 2 fired on 1 failure against 2879 timeouts. The refused work has to account for "+
			"the deficit, not merely be non-zero: %s", two.Detail)
	}
}

// A censored or thin R1 may not be divided by, because every bar is a ratio against it.
func TestACensoredBaselineIsNotDividedBy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*ArmSummary)
	}{
		{"censored", func(s *ArmSummary) { s.Censored = true }},
		{"below the sample floor", func(s *ArmSummary) { s.TailSampleSize = MinTailSamples - 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := healthyMatrix()
			tc.damage(&m.R1)
			// Make the arms look excellent, so anything that scores them would fire POSITIVE.
			m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 100, 20, 38_000)}

			res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

			// Reading 1 must not fire POSITIVE. WHICH guard stops it is deliberately not asserted: a thin R1
			// is caught by reading 4b, which declares the whole run invalid and short-circuits, while a
			// censored R1 is caught by the scoring precondition further down. Both are correct and pinning
			// one of them would fail the day the other became responsible.
			for _, r := range res.Readings {
				if r.ID == "1" && r.Fired {
					t.Errorf("reading 1 fired POSITIVE against an R1 that is %s. Every bar is a ratio "+
						"against that baseline, so the result is a lower bound wearing a measurement's "+
						"name: %s", tc.name, r.Detail)
				}
			}
			if res.Answer == "1" || strings.HasPrefix(res.Answer, "1 ") {
				t.Errorf("the answer is %q for a run whose baseline is %s", res.Answer, tc.name)
			}
		})
	}
}

// Reading 4c fires on a refusal the runner recorded, which is how an MPS arm that never engaged is reported.
func TestARecordedRefusalIsWhatMakesFourCReachable(t *testing.T) {
	m := healthyMatrix()
	m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 900, 60, 38_000)}
	m.Refused = map[string]string{ArmMPS: "the engines are not MPS clients"}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
	fourC := sharingReadingByID(t, res, "4c")
	if !fourC.Fired || fourC.Cell != ArmMPS {
		t.Fatalf("reading 4c did not fire for a recorded refusal of the mps arm: fired=%v cell=%q detail=%s",
			fourC.Fired, fourC.Cell, fourC.Detail)
	}
}

// The STREAM bar is a bar, and nothing was holding it.
//
// Reading 1 gates on two: the tail at 2x R1 and the stream at 1.25x. A mutation battery relaxing
// m5cTPOTBar from 1.25 to 100 broke no test in this file, which means an arm whose inter-token time was
// fifty times the baseline's could have been reported as the deliverable. TPOT is the half of the answer a
// first-token metric cannot see -- a topology that protects the first token and wrecks the stream after it
// passes every TTFT check there is -- and this study's own report header says so.
func TestAnArmThatWrecksTheStreamDoesNotFireTheDeliverable(t *testing.T) {
	m := healthyMatrix()
	// A perfect tail, and a stream ten times R1's 17.8 ms.
	wrecked := healthyArm(ArmTimeSlicing, 100, 178, 38_000)
	m.Sharing = []ArmSummary{wrecked}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	one := sharingReadingByID(t, res, "1")
	if one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm holding the tail at 1.5x while its stream runs at 10x "+
			"R1's. The pre-registration gates on both bars: %s", one.Detail)
	}
	// And it must be reading 5's business, not silence: the arm did improve on the control.
	if five := sharingReadingByID(t, res, "5"); !five.Fired {
		t.Errorf("no reading claimed an arm that beat the control on the tail and missed the stream bar; "+
			"that is the gap in the outcome space this study was written to close: %s", five.Detail)
	}
}

// And the tail bar, pinned the same way, because a battery that relaxes it must not pass either.
func TestAnArmThatMissesTheTailBarDoesNotFireTheDeliverable(t *testing.T) {
	m := healthyMatrix()
	// Stream is fine; the tail is three times R1's 67.3 ms, against a 2x bar.
	m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 202, 20, 38_000)}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if one := sharingReadingByID(t, res, "1"); one.Fired {
		t.Errorf("reading 1 fired POSITIVE at 3x R1 against a 2x tail bar: %s", one.Detail)
	}
}

// Reading 2 requires BOTH bars, and relaxing it to the tail alone broke no test until this one.
//
// The page's words are "if a sharing arm meets both bars while the contender's completed output falls
// below 75%". An arm that starves the contender while missing the stream bar has not bought protection at
// all, so calling it "protects only by starving" would credit it with a tail it did not deliver. That arm
// is reading 5's: a real improvement that does not reach the bar.
//
// The gate is duplicated -- reading 2 and reading 5 both ask whether an arm met both bars -- and a mutation
// battery that replaced only the first occurrence reported reading 5 as unprotected when it was reading 2
// that had no test. Both sites are pinned now.
func TestReadingTwoNeedsBothBarsAndNotTheTailAlone(t *testing.T) {
	m := healthyMatrix()
	// Tail well inside 2x, stream ten times R1's, and the contender's work refused.
	arm := healthyArm(ArmTimeSlicing, 100, 178, 5_000)
	arm.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 300, Completed: 120, Rejected: 170, TimedOut: 10},
	}
	m.Sharing = []ArmSummary{arm}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if two := sharingReadingByID(t, res, "2"); two.Fired {
		t.Errorf("reading 2 fired for an arm whose stream runs at 10x R1. Protection bought by starving the "+
			"contender is only that if the protection was delivered, and this arm missed a bar: %s", two.Detail)
	}
	if five := sharingReadingByID(t, res, "5"); !five.Fired {
		t.Errorf("no reading claimed an arm that improved on the control, missed the stream bar, and starved "+
			"the contender. That is an outcome with no name again: %s", five.Detail)
	}
}

// Tokens that arrived on a stream which then broke are not completed output.
//
// The sender keeps HTTPStatus at 200 for a broken stream because its headers arrived, so the partial tokens
// land in OutputTokensByTenant. An arm that completes half the contender's responses and breaks the other
// half after fifteen of sixteen tokens then looks like 97 percent of the control's output instead of 50,
// reading 2 never fires, and reading 1 calls it the deliverable. The scenario and its numbers are an
// independent review's.
func TestBrokenStreamsDoNotCountAsCompletedOutput(t *testing.T) {
	m := healthyMatrix()
	m.Shared = healthyArm(ArmShared, 1400, 80, 6_400)
	m.Shared.OutputTokensByTenant = map[string]int64{PremiumTenant: 60_000, NoisyTenant: 6_400}

	half := healthyArm(ArmTimeSlicing, 100, 20, 6_200)
	// 200 completed at 16 tokens, and 200 that broke after 15.
	half.OutputTokensByTenant = map[string]int64{PremiumTenant: 60_000, NoisyTenant: 3_200 + 3_000}
	half.OutputTokensFromFailedStreamsByTenant = map[string]int64{NoisyTenant: 3_000}
	half.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 400, Completed: 200, Rejected: 200},
	}
	m.Sharing = []ArmSummary{half}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if one := sharingReadingByID(t, res, "1"); one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm that delivered half the contender's completed output. "+
			"Counting the tokens of streams that broke makes it look like 97 percent: %s", one.Detail)
	}
	if two := sharingReadingByID(t, res, "2"); !two.Fired {
		t.Errorf("reading 2 did not fire for an arm at 0.50 of the control's completed output with 200 of "+
			"400 requests rejected: %s", two.Detail)
	}
}

// A contender whose requests failed in transport was not refused service, and must not read as starvation.
//
// Failed counts transport and mid-stream breaks. A backend whose connections fail produces hundreds while
// the premium tenant stays healthy, which is broken delivery. The pre-registration's word is "refused", and
// admission is what refuses.
func TestTransportFailureIsNotRefusal(t *testing.T) {
	m := healthyMatrix()
	arm := healthyArm(ArmTimeSlicing, 100, 20, 5_000)
	arm.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 400, Completed: 200, Failed: 200},
	}
	m.Sharing = []ArmSummary{arm}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if two := sharingReadingByID(t, res, "2"); two.Fired {
		t.Errorf("reading 2 fired on 200 transport failures and zero admission rejections. The evidence "+
			"establishes broken delivery, not a system withholding service: %s", two.Detail)
	}
}

// No sharing arm means no conclusion about sharing arms.
//
// The guards read `countSharingScorable(all) == 0 && len(all) > 0`, so an EMPTY slice slipped past them and
// reading 3 fired "splitting the card changes nothing that matters" over evidence in which nothing was
// split. bestImprovementOverShared returns a zero improvement when there is nothing to improve with.
func TestNoSharingArmsMeansNoConclusionAboutThem(t *testing.T) {
	m := healthyMatrix()
	m.Sharing = nil

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	for _, id := range []string{"1", "3", "5"} {
		r := sharingReadingByID(t, res, id)
		if r.Fired {
			t.Errorf("reading %s fired over evidence with no sharing arm in it at all: %s", id, r.Detail)
		}
	}
}

// The winner is the arm with the higher contender THROUGHPUT, which is not the larger token total.
func TestTheWinnerIsChosenByThroughputAndNotVolume(t *testing.T) {
	m := healthyMatrix()
	// timeSlicing: more tokens, over a longer window. mps: fewer tokens, faster.
	ts := healthyArm(ArmTimeSlicing, 100, 20, 3_000)
	ts.ActiveSeconds = 450
	mps := healthyArm(ArmMPS, 100, 20, 2_990)
	mps.ActiveSeconds = 420
	m.Sharing = []ArmSummary{ts, mps}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	one := sharingReadingByID(t, res, "1")
	if !one.Fired {
		t.Fatalf("reading 1 did not fire for two arms that both met the bars: %s", one.Detail)
	}
	if one.Cell != ArmMPS {
		t.Errorf("the winner is %q. timeSlicing produced 3000 tokens in 450 s and mps 2990 in 420 s, so mps "+
			"has the higher contender throughput and the page chooses on throughput, not on volume", one.Cell)
	}
}

// Pooling adds repetitions together, so one unusable block disappears into two healthy ones.
//
// An arm of 3000, 3000 and 50 premium completions clears the hundred-sample floor with 6050 pooled, while
// the third block's "p99" is that block's maximum. The table prints reps=3 and a healthy pooled count and
// nothing says a paid block was unusable. MinRepetitionTail is carried on the summary for exactly this
// question and no reading was asking it.
func TestOneThinRepetitionIsNotHiddenByPooling(t *testing.T) {
	m := healthyMatrix()
	arm := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
	arm.TailSampleSize = 6050
	arm.RepetitionCount = 3
	// The per-repetition counts travel in the MAP now. MinRepetitionTail cannot tell a measured zero from a
	// field nobody attached, and the floor needs that distinction: a repetition that completed nothing is
	// the case it most exists for.
	arm.MinRepetitionTail = 50
	arm.MinRepetitionCompletedByTenant = map[string]int{PremiumTenant: 50, NoisyTenant: 250}
	m.Sharing = []ArmSummary{arm}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	fourB := sharingReadingByID(t, res, "4b")
	if !fourB.Fired {
		t.Fatalf("reading 4b did not fire for an arm one of whose three repetitions completed 50 premium "+
			"requests. Pooled it reads 6050, which is why the pooled count alone cannot see this: %s", fourB.Detail)
	}
	if !strings.Contains(fourB.Detail, "50") {
		t.Errorf("reading 4b fired without naming the thin repetition, so a reader cannot tell which paid "+
			"block was unusable: %s", fourB.Detail)
	}
}

// Arms repeated a different number of times are not comparable, and the report would not say so.
//
// The confirmatory run is three separate single-repetition sessions pooled at report time. An arm that lost
// a session is pooled from two while the others come from three, and reading 3's threshold is the control's
// spread across those repetitions -- so instance variation ends up weighted differently by arm while the
// output looks like an ordinary verdict.
func TestArmsRepeatedUnequallyAreRefused(t *testing.T) {
	m := healthyMatrix()
	lost := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
	lost.RepetitionCount = 2
	lost.RepetitionTTFTMsP99 = []float64{899, 901}
	m.Sharing = []ArmSummary{lost, healthyArm(ArmMPS, 1000, 62, 37_000)}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	fourB := sharingReadingByID(t, res, "4b")
	if !fourB.Fired {
		t.Fatalf("reading 4b did not fire for a matrix whose timeSlicing arm has 2 repetitions and whose "+
			"others have 3: %s", fourB.Detail)
	}
	if !strings.Contains(fourB.Detail, "not repeated equally") {
		t.Errorf("reading 4b fired for some other reason and not the unequal repetitions: %s", fourB.Detail)
	}
}

// A refused MPS arm must not take the answer away from a time-slicing arm that fired.
//
// Reading 4c's own name ends "INVALID for that arm", and the evaluator's comment says a refused arm says
// nothing about the arm beside it. The answer was still chosen as "the first reading that fired" over a
// list where 4c sits ahead of readings 1, 2, 3 and 5, so 4c won every time it fired. This is not a corner
// case for this study: MPS has already been measured failing to engage on this AMI, so the expected shape
// of a run is a refused MPS arm and a scored time-slicing one.
func TestARefusedArmDoesNotTakeTheAnswerFromAnArmThatWasMeasured(t *testing.T) {
	m := healthyMatrix()
	// Time-slicing clears both bars; MPS never engaged and the runner recorded why.
	m.Sharing = []ArmSummary{healthyArm(ArmTimeSlicing, 100, 20, 38_000)}
	m.Refused = map[string]string{ArmMPS: "the engines are not MPS clients"}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	fourC := sharingReadingByID(t, res, "4c")
	if !fourC.Fired {
		t.Fatalf("reading 4c did not fire on a recorded refusal, so this test is not about what it says: %s", fourC.Detail)
	}
	one := sharingReadingByID(t, res, "1")
	if !one.Fired {
		t.Fatalf("reading 1 did not fire for a time-slicing arm inside both bars, so this test cannot check "+
			"which of the two becomes the answer: %s", one.Detail)
	}
	if res.Answer != answerOf(one) {
		t.Errorf("the answer is %q; a refused MPS arm overrode a time-slicing result that was measured and "+
			"paid for. 4c invalidates ONE ARM, and the readings below it were still reached", res.Answer)
	}
}

// An arm whose contender work went missing without being refused must not fire reading 1.
//
// The premium tail is supposed to be protected FROM the contender's load. An arm that lost half of that
// load to timeouts shows a calm premium tail for the one reason that is not protection, and neither of the
// two existing exclusions caught it: `starved` needs rejections in the ledger and there are none, and
// `starvationUnknown` needs the ledger to be missing and it is present. Reading 1 fired POSITIVE and
// printed the collapsed contender share in its own detail line, as though it were the price.
func TestAnArmThatLostTheContendersWorkWithoutRefusingItCannotBePositive(t *testing.T) {
	m := healthyMatrix()
	broken := healthyArm(ArmTimeSlicing, 100, 20, 38_000)
	// Every request accounted for, none refused, and half of them simply never came back.
	broken.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 300, Completed: 140, TimedOut: 120, Failed: 40},
	}
	m.Sharing = []ArmSummary{broken}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	one := sharingReadingByID(t, res, "1")
	if one.Fired {
		t.Errorf("reading 1 fired POSITIVE for an arm that completed 140 of the contender's 300 requests "+
			"with none rejected. The tail is calm because the load went missing: %s", one.Detail)
	}
	if !strings.Contains(one.Detail, "went missing") {
		t.Errorf("reading 1 declined without saying the contender's work went missing, so a reader is left "+
			"to guess which of the arm's numbers disqualified it: %s", one.Detail)
	}
}

// A control whose tail is censored must not let reading 4 diagnose a load that was too LOW.
//
// Censor enough slow requests and the p99 of the survivors is small, the ratio falls under 5x, and the
// reading that means "raise the load" fires on a control that was drowning. It also short-circuited 4b,
// the reading that would have said the opposite -- so the run's one instruction to its successor was to
// add load. Reading 4 now refuses, and 4b is reached.
func TestACensoredControlCannotBeReadAsTooLittleLoad(t *testing.T) {
	m := healthyMatrix()
	m.Shared.Censored = true
	// The survivors are fast, which is what censoring the slow ones leaves behind.
	m.Shared.TTFTMsP99 = 120

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	four := sharingReadingByID(t, res, "4")
	if four.Fired {
		t.Errorf("reading 4 called a censored control's fast survivors 'the load did not create contention', "+
			"which points the next paid run at raising a load that was already too high: %s", four.Detail)
	}
	if !four.NotEvaluable {
		t.Errorf("reading 4 reported a plain negative on evidence it cannot read; that is indistinguishable "+
			"from having looked: %s", four.Detail)
	}
	// And 4b must still have been reached, or the refusal above simply moved the silence.
	sharingReadingByID(t, res, "4b")
}

// An arm that lost the contender's work may not carry ANY reading, not merely reading 1.
//
// The first version of contenderLost was consulted in exactly one place, and an independent review
// reproduced both halves of what that left open. Reading 5's eligibility and its veto, and reading 2's
// "met both bars" set, all still asked only `computable && ttftOK && tpotOK`.
func TestAnArmThatLostTheContendersWorkCarriesNoReadingAtAll(t *testing.T) {
	broken := func(ttft, tpot float64) ArmSummary {
		a := healthyArm(ArmTimeSlicing, ttft, tpot, 38_000)
		// Every request accounted for, none refused, and half of them never came back.
		a.DispositionByTenant = map[string]Disposition{
			NoisyTenant: {Offered: 300, Completed: 140, TimedOut: 120, Failed: 40},
		}
		return a
	}

	t.Run("missing both bars, it must not fire reading 5 as protection", func(t *testing.T) {
		m := healthyMatrix()
		m.Sharing = []ArmSummary{broken(1200, 70)} // better than the control, short of the bars
		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		if five := sharingReadingByID(t, res, "5"); five.Fired {
			t.Errorf("reading 5 called an arm that lost 160 of the contender's 300 requests a protection "+
				"that fell short of the bar: %s", five.Detail)
		}
	})

	t.Run("meeting both bars, it must not veto a healthy arm's reading 5", func(t *testing.T) {
		m := healthyMatrix()
		healthy := healthyArm(ArmMPS, 1200, 70, 38_000) // a real partial improvement on the control
		m.Sharing = []ArmSummary{broken(100, 20), healthy}
		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		five := sharingReadingByID(t, res, "5")
		if !five.Fired {
			t.Errorf("an arm whose contender work went missing vetoed reading 5 for %s, which improved on "+
				"the control on evidence of its own: %s", ArmMPS, five.Detail)
		}
	})

	t.Run("reading 2 must not count it as having kept the contender's work", func(t *testing.T) {
		m := healthyMatrix()
		m.Sharing = []ArmSummary{broken(100, 20)} // meets both bars, on vanished load
		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		two := sharingReadingByID(t, res, "2")
		if two.Fired {
			t.Errorf("reading 2 fired on an arm whose contender work went missing rather than being refused: %s", two.Detail)
		}
		// The exact sentence reading 2 ends on when nothing starved. The first version of this assertion
		// looked for "kept the contender", which appears only in a source comment and never in output -- a
		// check that could not fail, in a test about a fix that was itself incomplete.
		if strings.Contains(two.Detail, "kept "+NoisyTenant+"'s work") {
			t.Errorf("reading 2 reports that every arm meeting both bars kept the contender's work; this one "+
				"completed 140 of 300 with none rejected: %s", two.Detail)
		}
	})
}

// Mixed refusal and delay is not starvation, and it is not scorable either.
//
// Reproduced by an independent review against the committed binary: 400 offered, 200 completed, 100
// REJECTED and 100 TIMED OUT fired reading 2 and printed "refused work, not merely late work" over a
// ledger in which refusals explain half the loss. The test that allowed it compared rejected REQUESTS
// against missing OUTPUT share and doubled one side to make the units meet.
//
// The two flags now partition one fact: the contender lost work, and either the refusals account for it or
// they do not. Nothing falls between them.
func TestRefusalsMustAccountForTheWholeLossBeforeItIsCalledStarvation(t *testing.T) {
	// The contender keeps HALF the control's output (20,000 of 40,000), and that number is chosen so the
	// OLD rule fires and the new one does not. At 100 of 400 rejected the old test read
	// refusedFraction*2 = 0.50 against a missing output share of 0.50 and called it starvation. A first
	// version of this fixture used a fifth of the control's output, where the old rule declined anyway --
	// so the test passed against the defect it was written for, which is the third blind check written in
	// this session and the reason each one is now mutated before it is believed.
	arm := func(rejected, timedOut int) ArmSummary {
		a := healthyArm(ArmTimeSlicing, 100, 20, 20_000) // meets both bars, contender share halved
		a.DispositionByTenant = map[string]Disposition{
			NoisyTenant: {Offered: 400, Completed: 200, Rejected: rejected, TimedOut: timedOut},
		}
		return a
	}

	t.Run("half refused and half timed out is neither reading 2 nor a finding", func(t *testing.T) {
		m := healthyMatrix()
		m.Sharing = []ArmSummary{arm(100, 100)}
		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		if two := sharingReadingByID(t, res, "2"); two.Fired {
			t.Errorf("reading 2 called it starvation where refusals explain 100 of 200 lost requests: %s", two.Detail)
		}
		for _, id := range []string{"1", "5"} {
			if r := sharingReadingByID(t, res, id); r.Fired {
				t.Errorf("reading %s fired on an arm that lost 200 of the contender's 400 requests with only "+
					"100 of them refused: %s", id, r.Detail)
			}
		}
	})

	t.Run("refusals accounting for the loss is still reading 2", func(t *testing.T) {
		m := healthyMatrix()
		m.Sharing = []ArmSummary{arm(195, 5)} // 195 of 200 lost were refused
		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)
		two := sharingReadingByID(t, res, "2")
		if !two.Fired {
			t.Errorf("reading 2 did not fire where refusals account for 195 of 200 lost requests, so an arm "+
				"that bought its tail by refusing work has no reading at all: %s", two.Detail)
		}
	})
}

// The contender's floor applies to EVERY repetition, not to the pool.
//
// Reproduced by an independent review against the committed binary: repetitions of 140, 140 and 50
// contender completions, each offered 140, clear a hundred-completion floor at 330 pooled and fire reading
// 1 POSITIVE. The pooled completion fraction is 78.6%, above the 0.75 bar, so `contenderLost` does not
// catch it either -- both guards were reading the total while the unusable block sat inside it.
//
// The premium tenant has had a per-repetition check since defect 36. The contender never did, and the next
// run is the first to use more than one repetition, which is what makes this live rather than theoretical.
func TestTheContendersFloorAppliesToEveryRepetition(t *testing.T) {
	m := healthyMatrix()
	arm := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
	arm.RepetitionCount = 3
	arm.MinRepetitionTail = 3000
	arm.DispositionByTenant = map[string]Disposition{
		NoisyTenant: {Offered: 420, Completed: 330, TimedOut: 90}, // pooled, and comfortably over 100
	}
	arm.MinRepetitionCompletedByTenant = map[string]int{PremiumTenant: 3000, NoisyTenant: 50}
	m.Sharing = []ArmSummary{arm}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	fourB := sharingReadingByID(t, res, "4b")
	if !fourB.Fired {
		t.Fatalf("reading 4b did not fire for an arm whose thinnest repetition completed 50 of the "+
			"contender's 140 requests. Pooled it reads 330, which is why the pooled count cannot see it: %s",
			fourB.Detail)
	}
	if !strings.Contains(fourB.Detail, NoisyTenant) || !strings.Contains(fourB.Detail, "50") {
		t.Errorf("reading 4b fired without naming the contender or the thin repetition, so a reader cannot "+
			"tell which paid block was unusable: %s", fourB.Detail)
	}
}

// A repetition that completed NOTHING is below the floor, and the exemption that let it through is gone.
//
// The premium check read `MinRepetitionTail > 0 && MinRepetitionTail < 100`, so zero -- the worst case a
// floor exists for -- was the one value it waved through. The exemption was really guarding against a field
// nobody had attached; the per-tenant map carries that distinction explicitly, so the exemption is not
// needed and the zero is caught.
func TestARepetitionThatCompletedNothingIsBelowTheFloor(t *testing.T) {
	m := healthyMatrix()
	arm := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
	arm.RepetitionCount = 3
	arm.MinRepetitionCompletedByTenant = map[string]int{PremiumTenant: 0, NoisyTenant: 250}
	m.Sharing = []ArmSummary{arm}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	if fourB := sharingReadingByID(t, res, "4b"); !fourB.Fired {
		t.Errorf("reading 4b did not fire for an arm with a repetition that completed no %s requests at all: %s",
			PremiumTenant, fourB.Detail)
	}

	// And an arm whose repetitions were never counted must NOT fire: absent is not zero.
	m2 := healthyMatrix()
	quiet := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
	quiet.RepetitionCount = 3 // no MinRepetitionCompletedByTenant attached at all
	m2.Sharing = []ArmSummary{quiet}
	if fourB := sharingReadingByID(t, EvaluateSharingMatrix(m2, PremiumTenant, NoisyTenant), "4b"); fourB.Fired {
		t.Errorf("reading 4b fired on an arm whose per-repetition counts were never attached; that is "+
			"'not measured', not 'measured at zero': %s", fourB.Detail)
	}
}

// A censored REPETITION must disqualify the arm even when the pool looks clean.
//
// Censoring is a fraction and pooling averages it: a repetition that lost 1.50% of its premium requests
// sits beside two clean ones as 0.75% of the pool, under the 1% bar. A review reproduced it on the eighth
// pilot's own rows -- mark the second control repetition's 70 slowest premium responses timed out, and
// reading 5 fires on a censored control while both completion floors pass.
func TestACensoredRepetitionDisqualifiesTheArmThePoolCallsClean(t *testing.T) {
	t.Run("the control", func(t *testing.T) {
		m := healthyMatrix()
		m.Shared.Censored = false // the pool is clean, which is the whole point
		m.Shared.AnyRepetitionCensored = true

		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

		four := sharingReadingByID(t, res, "4")
		if !four.NotEvaluable {
			t.Errorf("reading 4 read a control with a censored repetition as though its tail were exact: %s", four.Detail)
		}
		// And nothing below the gate is scored at all. A gate that cannot be computed stops the evaluation,
		// so the right assertion is that readings 1 and 5 are ABSENT rather than present and silent -- the
		// first version of this test asked for the latter and failed on its own fixture.
		for _, id := range idsOf(res) {
			if id == "1" || id == "5" {
				t.Errorf("reading %s was scored under a gate that could not be computed", id)
			}
		}
		if res.Answer != "" {
			t.Errorf("the answer is %q for a run whose control has a censored repetition", res.Answer)
		}
	})

	t.Run("R1, the denominator of both bars", func(t *testing.T) {
		m := healthyMatrix()
		m.R1.AnyRepetitionCensored = true

		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

		four := sharingReadingByID(t, res, "4")
		if !four.NotEvaluable {
			t.Errorf("reading 4 divided by a censored baseline; every multiple of it is a lower bound: %s", four.Detail)
		}
	})

	t.Run("a sharing arm", func(t *testing.T) {
		m := healthyMatrix()
		arm := healthyArm(ArmTimeSlicing, 100, 20, 38_000)
		arm.AnyRepetitionCensored = true
		m.Sharing = []ArmSummary{arm}

		res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

		if one := sharingReadingByID(t, res, "1"); one.Fired {
			t.Errorf("reading 1 fired POSITIVE for an arm one of whose repetitions is censored: %s", one.Detail)
		}
	})
}

// An arm refused PART WAY through must not invalidate the run before 4c can exempt it.
//
// At one repetition a refused arm carried no evidence, so excluding it from scoring happened by itself and
// the evaluator said so. At two it can complete repetition 1 and be refused at repetition 2 — real rows and
// a recorded refusal. Reading 4b's equal-repetitions check then sees 1 against the others' 2, fires, and
// calls the WHOLE RUN invalid, which is the opposite of what 4c exists to say. MPS has failed to engage on
// this AMI three times, so this is the expected shape of the next run and not a corner.
func TestAnArmRefusedPartWayThroughDoesNotInvalidateTheOthers(t *testing.T) {
	m := healthyMatrix()
	twoReps := func(s ArmSummary) ArmSummary {
		s.RepetitionCount = 2
		s.MinRepetitionCompletedByTenant = map[string]int{PremiumTenant: 3000, NoisyTenant: 250}
		return s
	}
	m.R1 = twoReps(m.R1)
	m.Shared = twoReps(m.Shared)

	// timeSlicing ran twice; mps managed one repetition and was then refused.
	ts := twoReps(healthyArm(ArmTimeSlicing, 900, 60, 38_000))
	half := healthyArm(ArmMPS, 1000, 62, 37_000)
	half.RepetitionCount = 1
	m.Sharing = []ArmSummary{ts, half}
	m.Refused = map[string]string{ArmMPS: "the engines are not MPS clients"}

	res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

	fourB := sharingReadingByID(t, res, "4b")
	if fourB.Fired {
		t.Fatalf("reading 4b invalidated the whole run because the REFUSED arm has one repetition against "+
			"the others' two. A refusal belongs to one arm, which is what 4c says: %s", fourB.Detail)
	}
	fourC := sharingReadingByID(t, res, "4c")
	if !fourC.Fired || fourC.Cell != ArmMPS {
		t.Errorf("reading 4c did not report the refusal it owns (fired=%v cell=%q): %s", fourC.Fired, fourC.Cell, fourC.Detail)
	}
	// And the refused arm's partial rows are not scored beside the complete ones.
	if strings.Contains(sharingReadingByID(t, res, "1").Detail, ArmMPS) {
		t.Errorf("reading 1 considered the refused arm's partial evidence: %s", sharingReadingByID(t, res, "1").Detail)
	}
}

// A missing baseline or control is an INVALID run, and it used to exit zero.
//
// Every reading divides by R1 or compares against `shared`. With either absent the gates came back
// NotEvaluable one at a time, nothing fired, the answer was blank — and the exit status, which turns on a
// FIRED gate, stayed zero. The paid runner calls `benchharness report ... || fail`, so it would have
// accepted a session whose baseline never ran.
func TestAMissingBaselineOrControlIsAnInvalidRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(*SharingArms)
		says string
	}{
		{"no R1", func(m *SharingArms) { m.R1 = ArmSummary{} }, ArmR1},
		{"no shared", func(m *SharingArms) { m.Shared = ArmSummary{} }, ArmShared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := healthyMatrix()
			tc.drop(&m)

			res := EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant)

			four := sharingReadingByID(t, res, "4")
			if !four.NotEvaluable {
				t.Errorf("a run with no %s arm was not reported as uncomputable: %s", tc.says, four.Detail)
			}
			if !strings.Contains(four.Detail, tc.says) {
				t.Errorf("the refusal does not name the arm that is missing: %s", four.Detail)
			}
			if res.Answer != "" {
				t.Errorf("the answer is %q for a run missing %s", res.Answer, tc.says)
			}
			for _, id := range idsOf(res) {
				if id != "4" {
					t.Errorf("reading %s was evaluated without a %s arm", id, tc.says)
				}
			}
		})
	}
}

// Reading 5 must report the contender's LEDGER, not a verdict derived from a threshold.
//
// A starved arm stays computable on purpose: that is reading 2's finding. But reading 2 requires BOTH bars,
// so an arm that refuses contender work, improves the tail and misses one bar lands here — where the
// sentence said only "a real improvement that does not reach the bar" and never mentioned the refusals.
//
// The first fix hung the sentence on the `starved` FLAG, and a review took that apart too: refusing 20 of
// 139 requests leaves the flag false, so the line then claimed "with the contender's work intact" over a
// ledger recording twenty refusals. A threshold is the wrong thing to hang a factual sentence on. The
// counts are reported; whether they amount to starvation is reading 2's judgement, made elsewhere.
func TestReadingFiveReportsTheContendersLedgerRatherThanAVerdict(t *testing.T) {
	fire := func(t *testing.T, d Disposition) PoPReading {
		t.Helper()
		m := healthyMatrix()
		arm := healthyArm(ArmTimeSlicing, 900, 200, 5_000) // improves, misses the stream bar
		arm.DispositionByTenant = map[string]Disposition{NoisyTenant: d}
		m.Sharing = []ArmSummary{arm}
		five := sharingReadingByID(t, EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant), "5")
		if !five.Fired {
			t.Fatalf("reading 5 did not fire on the partial-improvement shape, so this test checks nothing: %s", five.Detail)
		}
		return five
	}

	t.Run("refusals well past the starvation threshold", func(t *testing.T) {
		five := fire(t, Disposition{Offered: 300, Completed: 120, Rejected: 175, TimedOut: 5})
		for _, want := range []string{"175 refused", "120 of 300"} {
			if !strings.Contains(five.Detail, want) {
				t.Errorf("reading 5 does not report %q: %s", want, five.Detail)
			}
		}
	})

	t.Run("refusals BELOW it, which the starvation flag calls intact", func(t *testing.T) {
		// 20 of 139 refused: `starved` is false, and the previous version printed "work intact" here.
		five := fire(t, Disposition{Offered: 139, Completed: 119, Rejected: 20})
		if strings.Contains(five.Detail, "intact") || strings.Contains(five.Detail, "all 139") {
			t.Errorf("reading 5 calls the contender's work intact over a ledger with 20 refusals: %s", five.Detail)
		}
		if !strings.Contains(five.Detail, "20 refused") {
			t.Errorf("reading 5 does not report the 20 refusals: %s", five.Detail)
		}
	})

	t.Run("nothing refused, nothing lost", func(t *testing.T) {
		five := fire(t, Disposition{Offered: 139, Completed: 139})
		if !strings.Contains(five.Detail, "all 139") {
			t.Errorf("reading 5 does not distinguish a fully served contender from a partly refused one: %s", five.Detail)
		}
	})
}

// A repetition that served only part of its contender load is unscorable, even when the counts clear.
//
// A count and a fraction are different questions and the floor only asked the first. Repetitions of 100/139
// and 139/139 both clear a hundred completions while the first lost 28% of what it was offered, and an arm
// whose contention arrived that unevenly is not comparable with one where it did not. The control is held
// to it too: every ratio in this study is measured against `shared`, so a control that lost a third of its
// contender load is a weakened denominator wearing a full one's name.
func TestARepetitionThatServedPartOfItsContenderLoadIsUnscorable(t *testing.T) {
	t.Run("a sharing arm", func(t *testing.T) {
		m := healthyMatrix()
		arm := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
		arm.RepetitionCount = 2
		arm.DispositionByTenant = map[string]Disposition{
			NoisyTenant: {Offered: 278, Completed: 239, TimedOut: 39}, // pooled: 86% served, over the bar
		}
		arm.MinRepetitionCompletedByTenant = map[string]int{PremiumTenant: 3000, NoisyTenant: 100}
		arm.WorstRepetitionServedFractionByTenant = map[string]float64{NoisyTenant: 100.0 / 139.0} // 0.719
		m.Sharing = []ArmSummary{arm}

		fourB := sharingReadingByID(t, EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant), "4b")
		if !fourB.Fired {
			t.Fatalf("reading 4b accepted an arm whose thinnest repetition served 100 of 139 contender "+
				"requests. Its count clears the floor and its pooled fraction is 86%%, which is why neither "+
				"of the existing guards sees it: %s", fourB.Detail)
		}
		if !strings.Contains(fourB.Detail, "72%") {
			t.Errorf("reading 4b fired without naming the fraction that was served: %s", fourB.Detail)
		}
	})

	t.Run("the control, whose ratios everything is measured against", func(t *testing.T) {
		m := healthyMatrix()
		m.Shared.WorstRepetitionServedFractionByTenant = map[string]float64{NoisyTenant: 0.60}

		fourB := sharingReadingByID(t, EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant), "4b")
		if !fourB.Fired {
			t.Errorf("reading 4b accepted a CONTROL that served 60%% of its contender load in a repetition; "+
				"every improvement in this study is measured against that arm: %s", fourB.Detail)
		}
	})

	t.Run("a healthy run is untouched", func(t *testing.T) {
		m := healthyMatrix()
		m.Shared.WorstRepetitionServedFractionByTenant = map[string]float64{NoisyTenant: 1.0}
		arm := healthyArm(ArmTimeSlicing, 900, 60, 38_000)
		arm.WorstRepetitionServedFractionByTenant = map[string]float64{NoisyTenant: 1.0}
		m.Sharing = []ArmSummary{arm}

		if fourB := sharingReadingByID(t, EvaluateSharingMatrix(m, PremiumTenant, NoisyTenant), "4b"); fourB.Fired {
			t.Errorf("reading 4b fired on a run that served every contender request in every repetition: %s", fourB.Detail)
		}
	})
}
