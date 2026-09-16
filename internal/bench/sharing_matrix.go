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
	"fmt"
	"sort"
	"strings"
)

// The M5-c sharing matrix's readings, from
// docs/superpowers/specs/2026-09-10-does-splitting-the-card-buy-protection.md.
//
// WHY THIS FILE EXISTS AT ALL
//
// The pre-registration lists seven readings and says "the first that fires is the answer". Until this file
// there was no code for any of them, so the answer would have been computed by hand from raw JSONL -- which
// is what the price-of-protection plan called out about itself and fixed: "a confirmatory run whose readings
// are evaluated by hand is not the pre-registered instrument". The pre-registration now gates renting a card
// on this file existing.
//
// It shares PoPReading with price_of_protection.go on purpose. The two studies print through one formatter
// and mean the same thing by Fired, NotEvaluable and Detail; a second copy of that struct would drift, and
// the drift would be between two things a reader compares side by side.
//
// WHAT IT REFUSES TO DO
//
// Every reading below can come back NotEvaluable, and that is a third outcome rather than a polite false.
// A reading that reports "did not fire" when it could not be computed is indistinguishable from one that
// looked and found nothing, and this repository has already paid for that confusion once: an R1 carrying no
// premium TPOT made every cell unscorable while three readings printed "no cell met..." as though they had
// looked.

// The bars, named rather than inlined so the pre-registration and the code can be diffed by eye.
const (
	m5cTTFTBar = 2.00 // premium TTFT p99, as a multiple of R1's
	m5cTPOTBar = 1.25 // premium TPOT p99, as a multiple of R1's
	// m5cContentionBar is reading 4: below this multiple of R1, `shared` produced no interference to study.
	m5cContentionBar = 5.00
	// m5cContenderShareBar is reading 2: the contender's completed output as a fraction of its output under
	// `shared`. Falling below it is necessary for starvation and NOT sufficient -- see readingTwoSharing.
	m5cContenderShareBar = 0.75
)

// SharingArms is everything the matrix produced, sorted into the roles the readings speak about.
type SharingArms struct {
	// R1 is the isolated premium baseline, and it is the DENOMINATOR of both bars rather than an arm.
	//
	// It carries no contender by design, which is why reading 4b's contender floor does not apply to it.
	R1 ArmSummary
	// Shared is the control: both tenants on one engine with the whole card.
	Shared ArmSummary
	// Sharing holds the split arms in the study's order.
	Sharing []ArmSummary
	// Refused records, per arm name, why the runner declined to replay it -- the MPS control daemon being
	// unreachable being the case reading 4c is about.
	//
	// It is a map rather than an inference from a missing arm, because "this arm is absent" and "this arm
	// refused for a stated reason" are different facts and only the second is reading 4c. An arm that is
	// simply not there could equally be an interruption, and calling that a sharing-mode failure would be
	// describing a quantity by a cause the ledger does not establish.
	Refused map[string]string
}

// SharingResult is the ordered readings plus the one that answered.
type SharingResult struct {
	Readings []PoPReading
	// Answer is the first reading that fired, or "" when none did.
	Answer string
}

// sharingScored is one split arm measured against every bar, so no two readings recompute a ratio and
// disagree about the same arm.
type sharingScored struct {
	ArmSummary
	ttft, tpot   float64
	contenderRel float64
	ttftOK       bool
	tpotOK       bool
	// starved is reading 2's condition: the contender lost work AND the ledger shows it was refused or
	// discarded rather than merely late.
	starved bool
	// starvationUnknown marks an arm whose ledger cannot tell deletion from delay.
	starvationUnknown bool
	// contenderLost marks an arm where the contender's work went MISSING without being refused.
	//
	// This is a third thing, and it was the gap between the other two. `starved` means the ledger shows
	// refusals accounting for the shortfall, which is reading 2 and a real finding. `starvationUnknown`
	// means there is no ledger. An arm with a full ledger showing zero rejections and a third of the
	// contender's requests simply never completing is neither: nothing refused the work and nothing
	// explains where it went. Timeouts and broken connections do that.
	//
	// It has to exclude the arm rather than merely be noted, because the direction is the dangerous one:
	// the contender's load is what the premium tail is supposed to be protected FROM, so an arm that lost
	// half of it shows a calm premium tail for the one reason that is not protection. Reading 1 would have
	// fired POSITIVE and printed the contender's collapsed share in its own detail line as the price.
	contenderLost bool
	computable    bool
	why           string
}

// EvaluateSharingMatrix scores the matrix against the pre-registered readings, in the registered order.
//
// premiumTenant and contenderTenant are passed rather than assumed, for the reason the price-of-protection
// evaluator gives: a reading that hard-codes a tenant name goes quietly wrong the first time a trace
// renames one.
func EvaluateSharingMatrix(a SharingArms, premiumTenant, contenderTenant string) SharingResult {
	var res SharingResult

	// A MISSING baseline or control is an invalid run, and it used to be a quiet one.
	//
	// Every reading below divides by R1 or compares against `shared`. With either arm absent the gates come
	// back NotEvaluable one at a time, nothing fires, the answer is blank -- and `sharingRunInvalid` saw no
	// fired gate and returned nil, so `benchharness report ... || fail` accepted it. The two arms are named
	// here rather than inferred from a zero tail, because "this arm produced no evidence" and "this arm's
	// evidence is unusable" are different sentences and only the first one is this.
	for _, missing := range []struct {
		arm  ArmSummary
		name string
		why  string
	}{
		{a.R1, ArmR1, "the isolated baseline both bars divide by"},
		{a.Shared, ArmShared, "the control every improvement is measured from"},
	} {
		if missing.arm.Arm == "" {
			res.Readings = append(res.Readings, PoPReading{
				ID: "4", Name: "the load did not create contention -- INVALID", NotEvaluable: true,
				Detail: fmt.Sprintf("there is no %s arm in this evidence, and it is %s; nothing below can be scored without it",
					missing.name, missing.why),
			})
			return res
		}
	}

	// A REFUSED arm leaves the scored set here, and at one repetition that used to happen by itself.
	//
	// The runner declines an arm before replaying it, so a refused arm carried no evidence and the comment
	// on reading 4c said, correctly, that excluding it needed no further step. That stops being true the
	// moment REPS exceeds one: mps can complete repetition 1 and be refused at repetition 2, and then it is
	// an arm with real rows and a recorded refusal. Reading 4b's equal-repetitions check would see 1 against
	// the others' 2, fire, and declare the WHOLE RUN invalid -- before 4c, whose entire job is to say that a
	// refusal belongs to one arm. The next run is the first with two repetitions, so this is due now.
	//
	// Its partial rows are not scored either. Half an arm held against a bar is a comparison between
	// different amounts of evidence, and 4c already reports what happened to it.
	if len(a.Refused) > 0 {
		kept := make([]ArmSummary, 0, len(a.Sharing))
		for _, s := range a.Sharing {
			if _, refused := a.Refused[s.Arm]; !refused {
				kept = append(kept, s)
			}
		}
		a.Sharing = kept
	}

	// Readings 4 and 4b short-circuit, because each one says the TRACE rather than the topology is what has
	// to change. Scoring arms underneath either would be scoring comparisons that do not mean anything.
	// A gate that FIRED stops the evaluation. A gate that could not be COMPUTED does not stop the next gate.
	//
	// Those two were on the same footing -- the loop returned at the first reading that either fired or came
	// back NotEvaluable -- and that let reading 4 silence 4b. It matters in exactly the case reading 4 now
	// refuses: a control whose tail is censored makes 4 uncomputable, and 4b, the reading that would have
	// said the load was too HIGH, never ran. The answer came back blank where a diagnosis was available.
	//
	// A fired gate still stops everything under it, including the other gate. Each one says the TRACE rather
	// than the topology has to change, so anything scored below is a comparison that does not mean anything.
	notEvaluable := false
	for _, r := range []PoPReading{
		sharingReadingFour(a.R1, a.Shared),
		sharingReadingFourB(a, premiumTenant, contenderTenant),
	} {
		res.Readings = append(res.Readings, r)
		if r.Fired {
			res.Answer = answerOf(r)
			return res
		}
		notEvaluable = notEvaluable || r.NotEvaluable
	}
	if notEvaluable {
		// Answer stays "", which is what NotEvaluable means: nothing below this can be scored either.
		return res
	}

	// 4c does NOT short-circuit, and the difference is in its own name: "INVALID for that arm". A refused
	// MPS arm says nothing about the time-slicing arm beside it, and stopping here would throw away a
	// measurement that was made and paid for. The refused arm is absent from a.Sharing already -- the runner
	// declines it before replay -- so excluding it from the scoring needs no further step.
	fourC := sharingReadingFourC(a)
	res.Readings = append(res.Readings, fourC)

	all := scoreSharingArms(a, premiumTenant, contenderTenant)
	scored := []PoPReading{
		sharingReadingOne(a.R1, premiumTenant, contenderTenant, all),
		sharingReadingTwo(all, contenderTenant),
		sharingReadingThree(a.Shared, all),
		sharingReadingFive(a.Shared, all, contenderTenant),
	}
	res.Readings = append(res.Readings, scored...)

	// The ANSWER is chosen from the scored readings FIRST, and 4c is the fallback rather than the winner.
	//
	// 4c is appended above them because that is the order a reader should meet the readings in, and the
	// answer used to be the first fired reading in that same order -- so a refused MPS arm took the answer
	// away from a time-slicing result that fired underneath it. That is the opposite of what 4c is for. Its
	// name says "INVALID for that arm", the comment above says a refused arm says nothing about the arm
	// beside it, and this repository has already MEASURED MPS failing to engage on this AMI: the case is
	// not hypothetical, it is the expected one.
	//
	// 4c still becomes the answer when nothing else fired, because then the refused arm is all there is.
	for _, r := range scored {
		if r.Fired {
			res.Answer = answerOf(r)
			break
		}
	}
	if res.Answer == "" && fourC.Fired {
		res.Answer = answerOf(fourC)
	}
	return res
}

// censored reports whether an arm's tail is censored, in the pool OR in any single repetition.
//
// ArmSummary.Censored is computed from POOLED rows, and censoring is a fraction: a repetition that lost
// 1.50% of its premium requests sits beside two clean ones as 0.75% of the pool and passes a 1% bar. The
// readings that refuse a censored control therefore could not see it, and a review reproduced reading 5
// firing on exactly that. Every reading asks this function now, so the two cannot drift apart again.
func censored(s ArmSummary) bool { return s.Censored || s.AnyRepetitionCensored }

// armNotScorable says, in words, why an arm cannot be held against the bars -- or "" when it can.
func armNotScorable(c ArmSummary, premiumTenant string) string {
	switch {
	case c.TTFTMsP99 <= 0:
		return fmt.Sprintf("%s completed no premium requests, so it has no tail to hold against a bar", c.Arm)
	case c.TPOTMsP99ByTenant[premiumTenant] <= 0:
		return fmt.Sprintf("%s has no premium TPOT, so the stream bar cannot be applied to it", c.Arm)
	case c.TailSampleSize < MinTailSamples:
		return fmt.Sprintf("%s has %d premium completions, below the %d a nearest-rank p99 needs to be anything but the maximum",
			c.Arm, c.TailSampleSize, MinTailSamples)
	case censored(c):
		return fmt.Sprintf("%s has a censored tail, which is a lower bound rather than a p99", c.Arm)
	}
	return ""
}

func scoreSharingArms(a SharingArms, premiumTenant, contenderTenant string) []sharingScored {
	// The contender's ABSOLUTE output under the control, not its share of it.
	//
	// shareOf divides by the arm's own total, and the pre-registration asks a different question: reading 2
	// is "the contender's completed output falls below 75% of ITS OUTPUT under `shared`". Those come apart
	// exactly when the split costs both tenants alike. A control producing 60k premium and 40k contender
	// against a split arm producing 30k and 20k has given the contender HALF as much work, and its share is
	// 40% in both -- so a share ratio reports 1.00, reading 2 never fires, and reading 1 calls an arm that
	// discarded half the contender's work the deliverable.
	sharedContender := completedOutput(a.Shared, contenderTenant)
	out := make([]sharingScored, 0, len(a.Sharing))
	for _, c := range a.Sharing {
		s := sharingScored{ArmSummary: c}
		s.ttft = ratioOr(c.TTFTMsP99, a.R1.TTFTMsP99)
		s.tpot = ratioOr(c.TPOTMsP99ByTenant[premiumTenant], a.R1.TPOTMsP99ByTenant[premiumTenant])
		s.contenderRel = ratioOr(completedOutput(c, contenderTenant), sharedContender)

		// The arm's OWN numerators are checked, not only the denominators.
		//
		// price_of_protection.go records what happens otherwise, and it is the worst wrong number either
		// file could print: percentile() returns 0 for an empty slice, so an arm whose premium requests all
		// timed out arrives with TTFTMsP99 = 0, and 0/100 clears a 2x bar while 0 clears a 1.25x one. The
		// arm that starved the protected tenant completely would be reported as the one that protected it.
		// The DENOMINATORS are held to the same standard as the arm, which they were not.
		//
		// R1 is divided by twice and `shared` is the control every improvement is measured from. A censored
		// R1 is a lower bound, so a ratio against it is a lower bound wearing a measurement's name, and an
		// R1 below the sample floor is the slowest premium request calling itself a p99. Checking only that
		// they are positive let a survivor-biased baseline produce a confident POSITIVE.
		// censored() rather than .Censored, because pooling averages a censored repetition away.
		s.computable = a.R1.TTFTMsP99 > 0 && a.R1.TPOTMsP99ByTenant[premiumTenant] > 0 &&
			a.R1.TailSampleSize >= MinTailSamples && !censored(a.R1) &&
			// `shared` is the control every improvement is measured FROM, so a censored control makes
			// readings 3 and 5 comparisons against a lower bound. Only R1 was being held to this.
			a.Shared.TTFTMsP99 > 0 && a.Shared.TailSampleSize >= MinTailSamples && !censored(a.Shared) &&
			sharedContender > 0 &&
			c.TTFTMsP99 > 0 && c.TPOTMsP99ByTenant[premiumTenant] > 0 &&
			c.TailSampleSize >= MinTailSamples && !censored(c)
		s.why = armNotScorable(c, premiumTenant)
		s.ttftOK = s.ttft <= m5cTTFTBar
		s.tpotOK = s.tpot <= m5cTPOTBar

		// Starvation is TWO conditions, and the pre-registration is explicit that the second is required:
		// "Starvation must be shown, not inferred from a smaller number." A share that fell because work was
		// delayed is a different finding from one that fell because work was refused.
		if s.contenderRel < m5cContenderShareBar {
			d, ok := c.DispositionByTenant[contenderTenant]
			switch {
			case !ok:
				s.starvationUnknown = true
			default:
				// Refusals must ACCOUNT FOR the deficit, not merely be present.
				//
				// This was `d.Rejected+d.Failed > 0`, so one broken stream among thousands of timeouts
				// printed "refused work, not merely late work" over a ledger that principally said late.
				// Failed counts transport and mid-stream breaks, which a flaky tunnel produces on its own.
				//
				// The rule: the work that was REFUSED has to be at least as large as the work that went
				// missing. Anything less and the shortfall is mostly delay, which the pre-registration says
				// in as many words is a different finding and not this reading.
				// REJECTIONS, not failures, and measured against what went missing.
				//
				// Two corrections live here. The first: Failed counts transport and mid-stream breaks, and a
				// contender backend whose connections fail produces hundreds of them while the premium
				// tenant stays healthy -- which is broken delivery, not a system withholding service. The
				// pre-registration's word is "refused", and admission is what refuses. Failures are reported
				// in the detail so a reader can see them, and they no longer establish the finding.
				//
				// The second: the two sides of this test were in DIFFERENT UNITS, and the factor of two
				// that used to sit here was a fudge for that. Rejections are requests; the deficit was
				// measured as missing output share, and a refused request can carry any amount of output.
				// Doubling one side to make the comparison work is not an argument, and an independent
				// review reproduced what it let through: 400 offered, 200 completed, 100 rejected and 100
				// TIMED OUT fired this reading and printed "refused work, not merely late work" over a
				// ledger where refusals explain half the loss and delay explains the other half.
				//
				// Both sides are REQUESTS now, from the one ledger, and the rule is the plain one: the
				// refusals have to account for the work that went missing. The 0.9 is slack for rounding
				// and for a stray broken stream, not for a second cause.
				lost := d.Offered - d.Completed
				s.starved = d.Rejected > 0 && lost > 0 &&
					float64(d.Rejected) >= 0.9*float64(lost)
			}
		}

		// Work that vanished without being refused, which is neither starvation nor a price.
		//
		// Measured against what the contender was OFFERED rather than against the control's output, because
		// the question here is different: not "did this arm serve the contender less" but "did this arm's
		// contender load actually land". An arm that was offered 238 requests and completed 120 with no
		// rejections ran half the experiment, whatever its output ratio says.
		//
		// The same 0.75 reading 2 uses, and now EXHAUSTIVE with it rather than overlapping.
		//
		// `starved` and this flag used to have independent conditions, which left a gap between them: a
		// loss that refusals explained partly was neither, so the arm stayed scorable and could carry a
		// finding on a contender load that half evaporated. They now partition the same fact. The contender
		// lost real work, and either the refusals account for it -- reading 2, a finding -- or they do not,
		// and this arm cannot be scored. There is no third case and no arm falls between them.
		if d, ok := c.DispositionByTenant[contenderTenant]; ok && d.Offered > 0 && !s.starved {
			completedRel := float64(d.Completed) / float64(d.Offered)
			if completedRel < m5cContenderShareBar {
				s.contenderLost = true
				// NOT SCORABLE, rather than excluded from one reading.
				//
				// The first version of this flag was only consulted by reading 1, and an independent review
				// reproduced what that left open: an arm that lost half its contender work and missed both
				// bars fired reading 5 as protection, and the same arm meeting both bars VETOED reading 5
				// for a healthy arm beside it. Reading 2 printed that every qualifying arm had "kept the
				// contender's work". One flag consulted in one place is how a partial fix looks.
				//
				// `computable` is the single gate every reading already asks about, and the honest answer
				// for this arm is no: its premium tail cannot be held against a bar when the load it was
				// supposed to be protected from did not arrive. `starved` arms stay computable on purpose --
				// that is reading 2's finding and it is set only when this flag is not.
				s.computable = false
				// Only when armNotScorable found nothing to say. An arm can be both uncomputable and
				// missing contender work, and the first reason is the one that came first.
				if s.why == "" {
					s.why = fmt.Sprintf("%s: the contender completed %d of %d requests with %d rejected, so the work went missing rather than being refused",
						s.Arm, d.Completed, d.Offered, d.Rejected)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// sharingReadingFour: the load made no contention, so nothing below it measures protection.
func sharingReadingFour(r1, shared ArmSummary) PoPReading {
	r := PoPReading{ID: "4", Name: "the load did not create contention -- INVALID"}
	if r1.TTFTMsP99 <= 0 {
		r.NotEvaluable = true
		r.Detail = "R1 has no premium tail, so there is no baseline to hold the control against"
		return r
	}
	if shared.TTFTMsP99 <= 0 {
		r.NotEvaluable = true
		r.Detail = "the `shared` control completed no premium requests, so whether it produced contention cannot be read from its tail"
		return r
	}

	// The operands have to be trustworthy before this reading is allowed to diagnose, and a positive number
	// is not the same as a trustworthy one.
	//
	// This reading says the load was too LOW. The evidence that most resembles a low load is an overloaded
	// control whose few survivors were fast: censor enough of the slow requests and the p99 of what is left
	// is small, the ratio falls under 5x, and this fires. It would then short-circuit 4b -- the reading that
	// exists to say the load was too HIGH -- and send the next paid run to raise the load that was already
	// drowning it. The two gates sit next to each other and this is the one place they can be confused, so
	// it refuses instead and lets 4b speak.
	switch {
	case censored(shared):
		r.NotEvaluable = true
		r.Detail = "the control's premium tail is censored, so a low ratio here cannot be told apart from an overload that dropped its slow requests"
		return r
	// R1 was never checked here, and it is the DENOMINATOR of this ratio and of both bars. A censored
	// baseline makes every multiple of it a lower bound, including the one this gate compares against 5x.
	case censored(r1):
		r.NotEvaluable = true
		r.Detail = "R1's premium tail is censored, so every ratio measured against it -- this one included -- is a lower bound rather than a multiple"
		return r
	case shared.TailSampleSize < MinTailSamples:
		r.NotEvaluable = true
		r.Detail = fmt.Sprintf("the control completed %d premium requests, below the %d this p99 needs, so its tail is the slowest survivor rather than a percentile",
			shared.TailSampleSize, MinTailSamples)
		return r
	case r1.TailSampleSize < MinTailSamples:
		r.NotEvaluable = true
		r.Detail = fmt.Sprintf("R1 completed %d premium requests, below the %d this p99 needs; the baseline both bars divide by is the slowest survivor",
			r1.TailSampleSize, MinTailSamples)
		return r
	}

	ratio := shared.TTFTMsP99 / r1.TTFTMsP99
	r.Fired = ratio < m5cContentionBar
	r.Detail = fmt.Sprintf("the control's premium TTFT p99 is %.1fx R1's (%.1f ms against %.1f ms), against an INVALID threshold of %.1fx",
		ratio, shared.TTFTMsP99, r1.TTFTMsP99, m5cContentionBar)
	return r
}

// sharingReadingFourB: the load was too high for any arm to be measured.
//
// Applied to every ARM of the matrix and not only the control, because a split card gives each engine less
// to work with and an arm collapsing is a live outcome for the arm itself.
//
// R1 is exempt from the CONTENDER clause and only that clause. It is the isolated premium baseline: it has
// no contender by construction, so requiring a hundred contender completions of it would fire this reading
// on every run that was working perfectly. Its premium floor still applies, because R1 is the denominator of
// both bars and a baseline built on fewer than a hundred completions is the slowest request wearing a
// percentile's name.
func sharingReadingFourB(a SharingArms, premiumTenant, contenderTenant string) PoPReading {
	r := PoPReading{ID: "4b", Name: "the load was too high to measure -- INVALID"}
	var thin []string

	check := func(s ArmSummary, wantContender bool) {
		if s.Arm == "" {
			return
		}
		if s.TailSampleSize < MinTailSamples {
			thin = append(thin, fmt.Sprintf("%s completed %d %s requests", s.Arm, s.TailSampleSize, premiumTenant))
		}
		// EVERY REPETITION, not the pool. Pooling adds the blocks together, so an arm of 3000, 3000 and 50
		// completions clears the floor with 6050 while its third block's "p99" is that block's maximum --
		// and the table prints reps=3 and a healthy pooled count with nothing saying one paid block was
		// unusable. MinRepetitionTail is carried on the summary for exactly this and no reading used it.
		// The `> 0` exemption is gone, and the check reads the MAP instead of MinRepetitionTail.
		//
		// Zero is below a hundred, and a repetition that completed nothing is the worst case this floor
		// exists for -- it was the one case the floor waved through. But MinRepetitionTail cannot tell a
		// real zero from a field nobody attached, which is what the exemption was really guarding. The map
		// can: a tenant PRESENT with zero was measured at zero, and a tenant absent was not measured.
		if s.RepetitionCount > 1 {
			if n, ok := s.MinRepetitionCompletedByTenant[premiumTenant]; ok && n < MinTailSamples {
				thin = append(thin, fmt.Sprintf("%s has a repetition with only %d %s completions (pooled: %d over %d repetitions)",
					s.Arm, n, premiumTenant, s.TailSampleSize, s.RepetitionCount))
			}
		}
		if !wantContender {
			return
		}
		d, ok := s.DispositionByTenant[contenderTenant]
		if !ok {
			thin = append(thin, fmt.Sprintf("%s carries no disposition for %s at all", s.Arm, contenderTenant))
			return
		}
		if d.Completed < MinTailSamples {
			thin = append(thin, fmt.Sprintf("%s completed %d %s requests", s.Arm, d.Completed, contenderTenant))
		}
		// EVERY REPETITION for the contender too, and not only for the premium tenant.
		//
		// The line above applies the floor to the POOL, which is the same hiding place the premium floor
		// was given a per-repetition check to close. An independent review reproduced the contender's
		// version: repetitions of 140, 140 and 50 completions, each offered 140, clear a hundred at 330
		// pooled and fire reading 1 POSITIVE. The pooled completion fraction is 78.6%, so `contenderLost`
		// does not catch it either -- both guards look at the total and the bad block is inside it.
		if s.RepetitionCount > 1 {
			if n, ok := s.MinRepetitionCompletedByTenant[contenderTenant]; ok && n < MinTailSamples {
				thin = append(thin, fmt.Sprintf("%s has a repetition with only %d %s completions (pooled: %d over %d repetitions)",
					s.Arm, n, contenderTenant, d.Completed, s.RepetitionCount))
			}
		}
		// A COUNT is not a FRACTION, and the floor above only asks the first.
		//
		// Repetitions of 100/139 and 139/139 both clear a hundred completions while the first lost 28% of
		// its load, and an arm whose contention arrived that unevenly is not comparable with one where it
		// did not. The bar is the same 0.75 reading 2 uses for the contender's share, applied to what was
		// DELIVERED in a repetition rather than to what the pool totals. It runs for one repetition too:
		// a single block that served two thirds of its contender load is the same defect without the
		// pooling to hide it.
		if f, ok := s.WorstRepetitionServedFractionByTenant[contenderTenant]; ok && f < m5cContenderShareBar {
			thin = append(thin, fmt.Sprintf("%s has a repetition that served only %.0f%% of the %s load it was offered",
				s.Arm, f*100, contenderTenant))
		}
	}

	check(a.R1, false)
	check(a.Shared, true)
	for _, s := range a.Sharing {
		check(s, true)
	}

	// The arms must have been repeated the same number of times.
	//
	// The confirmatory run is three separate single-repetition sessions pooled at report time, so an arm
	// that lost a session is pooled from two while the others come from three. Reading 3's threshold is the
	// control's repetition-to-repetition spread, and comparing an arm measured on two instances against a
	// spread measured over three weights instance variation differently by arm -- while the report prints a
	// perfectly ordinary POSITIVE or NEGATIVE.
	counts := map[string]int{}
	for _, s := range append([]ArmSummary{a.R1, a.Shared}, a.Sharing...) {
		if s.Arm != "" {
			counts[s.Arm] = s.RepetitionCount
		}
	}
	want, from := 0, ""
	for _, arm := range []string{ArmR1, ArmShared} {
		if n, ok := counts[arm]; ok && n > 0 {
			want, from = n, arm
			break
		}
	}
	if want > 0 {
		var uneven []string
		for arm, n := range counts {
			if n != want {
				uneven = append(uneven, fmt.Sprintf("%s has %d", arm, n))
			}
		}
		if len(uneven) > 0 {
			sort.Strings(uneven)
			thin = append(thin, fmt.Sprintf("the arms were not repeated equally: %s has %d repetition(s) and %s",
				from, want, strings.Join(uneven, ", ")))
		}
	}

	if len(thin) == 0 {
		r.Detail = fmt.Sprintf("every arm completed at least %d requests for both tenants", MinTailSamples)
		return r
	}
	r.Fired = true
	r.Detail = fmt.Sprintf("against a floor of %d: %s", MinTailSamples, strings.Join(thin, "; "))
	return r
}

// sharingReadingFourC: a sharing mode did not engage, which makes that arm the other one under its name.
//
// hack/m5c-matrix.sh refuses an MPS arm whose control daemon never became ready rather than replaying it,
// so the evidence for this reading is a recorded refusal and not a number. The reading exists so that
// refusal is a registered outcome rather than a detail buried in a run log.
//
// An arm that is absent with NO recorded refusal is NotEvaluable, not fired. Absence is equally consistent
// with an interruption, a deadline that ran out on a cell boundary, or an operator running a subset with
// ARMS=; calling any of those a sharing-mode failure would be attributing a result to a cause the evidence
// does not establish.
func sharingReadingFourC(a SharingArms) PoPReading {
	r := PoPReading{ID: "4c", Name: "the sharing mode did not engage -- INVALID for that arm"}

	present := map[string]bool{}
	for _, s := range a.Sharing {
		present[s.Arm] = true
	}
	var refused, missing []string
	for _, arm := range []string{ArmTimeSlicing, ArmMPS} {
		if why, ok := a.Refused[arm]; ok && why != "" {
			refused = append(refused, fmt.Sprintf("%s: %s", arm, why))
			continue
		}
		if !present[arm] {
			missing = append(missing, arm)
		}
	}
	if len(refused) > 0 {
		r.Fired, r.Cell = true, strings.SplitN(refused[0], ":", 2)[0]
		r.Detail = strings.Join(refused, "; ")
		return r
	}
	if len(missing) > 0 {
		r.NotEvaluable = true
		r.Detail = fmt.Sprintf("no evidence for %s and no recorded refusal either; absence alone does not say the mode failed to engage, and the run log is what distinguishes a refusal from an interruption",
			strings.Join(missing, " and "))
		return r
	}
	r.Detail = "both sharing arms produced evidence and neither was refused"
	return r
}

// sharingReadingOne: separation protects, and this is the deliverable.
//
// THREE clauses, not two. The tail bar, the stream bar, and NOT reading 2's starvation -- because reading 2's
// condition is otherwise a strict subset of this one, and "the first that fires is the answer" would report
// an arm that bought its tail by taking the other tenant's work as POSITIVE with reading 2 never reached.
// The pre-registration was corrected for this on 2026-09-10, before any card, and says so.
// completedOutput is a tenant's output MINUS whatever arrived on a stream that then broke.
//
// The sender keeps HTTPStatus at 200 for a broken stream, because its headers did arrive, so the partial
// tokens land in OutputTokensByTenant. Reading 2 asks about completed output and those tokens are not that:
// report.go's own tallyDelivered says "a stream that died partway delivered nothing the client could use",
// and until this existed the code did not honour its own comment.
func completedOutput(s ArmSummary, tenant string) float64 {
	return float64(s.OutputTokensByTenant[tenant] - s.OutputTokensFromFailedStreamsByTenant[tenant])
}

// contenderRate is the contender's completed output per second of the arm's sending time.
func contenderRate(s ArmSummary, tenant string) float64 {
	if s.ActiveSeconds <= 0 {
		return 0
	}
	return completedOutput(s, tenant) / s.ActiveSeconds
}

// premiumRate is one tenant's output tokens per second of the arm's active time.
//
// Derived rather than read off, because ArmSummary carries per-tenant TOKENS and a whole-arm RATE, and the
// two are not interchangeable the moment an arm serves more than one tenant.
func premiumRate(s ArmSummary, tenant string) float64 {
	if s.ActiveSeconds <= 0 {
		return 0
	}
	return float64(s.OutputTokensByTenant[tenant]) / s.ActiveSeconds
}

func sharingReadingOne(r1 ArmSummary, premiumTenant, contenderTenant string, all []sharingScored) PoPReading {
	r := PoPReading{ID: "1", Name: "separation protects -- POSITIVE"}
	if countSharingScorable(all) == 0 {
		r.NotEvaluable = true
		r.Detail = "no sharing arm could be scored against the bars" + sharingUnscorableSuffix(all)
		return r
	}
	var won *sharingScored
	for i := range all {
		s := &all[i]
		if !s.computable || !s.ttftOK || !s.tpotOK || s.starved || s.starvationUnknown || s.contenderLost {
			continue
		}
		// Higher contender throughput wins. An EXACT tie goes to timeSlicing, because MPS needs a control
		// daemon that can be absent and the simpler mechanism is the smaller claim.
		switch {
		case won == nil:
			won = s
		// Throughput, not volume. contenderRel divides by the same control for both candidates, so comparing
		// it compares token TOTALS -- and an arm that took longer to drain can produce more tokens at a lower
		// rate. The page says "the one with the higher contender throughput".
		case contenderRate(s.ArmSummary, contenderTenant) > contenderRate(won.ArmSummary, contenderTenant):
			won = s
		case contenderRate(s.ArmSummary, contenderTenant) == contenderRate(won.ArmSummary, contenderTenant) && s.Arm == ArmTimeSlicing:
			won = s
		}
	}
	if won == nil {
		r.Detail = "no sharing arm held both bars with the contender's work intact" + sharingUnscorableSuffix(all)
		return r
	}
	r.Fired, r.Cell = true, won.Arm
	// THE PRICE IS IN THE HEADLINE, because the pre-registration requires it: "Protection that costs half
	// the machine is a real answer and a different product from one that costs a tenth, and a pass/fail line
	// would report them identically."
	// The PREMIUM tenant's throughput, not the arm's.
	//
	// R1 serves premium alone, so its OutputTokensPerSecond is premium-only. A split arm's is both tenants
	// added together, and dividing one by the other compares unlike things: an arm serving premium at 10
	// tok/s and the contender at 10 against an R1 serving premium at 20 reports 1.00 when the premium tenant
	// actually gets half. The pre-registration puts this number in the write-up's first sentence.
	price := ratioOr(premiumRate(won.ArmSummary, premiumTenant), premiumRate(r1, premiumTenant))
	r.Detail = fmt.Sprintf("%s holds the premium tail at %.2fx R1 and the stream at %.2fx, with the contender at %.2f of its output under `shared`; it runs at %.2f of R1's throughput",
		won.Arm, won.ttft, won.tpot, won.contenderRel, price)
	return r
}

// sharingReadingTwo: the tail was bought by starving the contender.
func sharingReadingTwo(all []sharingScored, contenderTenant string) PoPReading {
	r := PoPReading{ID: "2", Name: "separation protects only by starving the contender -- NEGATIVE"}
	var metBars []*sharingScored
	for i := range all {
		if all[i].computable && all[i].ttftOK && all[i].tpotOK {
			metBars = append(metBars, &all[i])
		}
	}
	if len(metBars) == 0 {
		r.Detail = "no sharing arm met both bars, so this reading does not apply; that is reading 3 or 5" + sharingUnscorableSuffix(all)
		return r
	}
	for _, s := range metBars {
		if s.starvationUnknown {
			r.NotEvaluable = true
			r.Detail = fmt.Sprintf("%s carries no per-tenant disposition for %s, and a smaller share is equally consistent with deletion, delay and starvation",
				s.Arm, contenderTenant)
			return r
		}
		if s.starved {
			r.Fired, r.Cell = true, s.Arm
			d := s.DispositionByTenant[contenderTenant]
			r.Detail = fmt.Sprintf("%s met both bars with %s at %.2f of its output under `shared`, and the ledger shows %d of %d requests REJECTED at admission (%d failed in transport, %d timed out) -- refused work, not merely late work",
				s.Arm, contenderTenant, s.contenderRel, d.Rejected, d.Offered, d.Failed, d.TimedOut)
			return r
		}
	}
	r.Detail = fmt.Sprintf("every arm that met both bars kept %s's work", contenderTenant)
	return r
}

// sharingReadingThree: splitting the card changes nothing that matters.
//
// The threshold is `shared`'s own repetition-to-repetition spread. With fewer than two repetitions there IS
// no spread, so this declines rather than reporting every arm as failing to beat noise nobody measured.
func sharingReadingThree(shared ArmSummary, all []sharingScored) PoPReading {
	r := PoPReading{ID: "3", Name: "splitting the card changes nothing that matters -- INCONCLUSIVE"}
	if countSharingScorable(all) == 0 {
		r.NotEvaluable = true
		r.Detail = "no sharing arm could be scored against the bars" + sharingUnscorableSuffix(all)
		return r
	}
	best, spread, ok := bestImprovementOverShared(shared, all)
	if !ok {
		r.NotEvaluable = true
		r.Detail = fmt.Sprintf("`shared` carries %d per-repetition tail(s); its repetition-to-repetition spread is what this reading compares against and needs at least 2",
			len(shared.RepetitionTTFTMsP99))
		return r
	}
	r.Fired = best <= spread
	r.Detail = fmt.Sprintf("the best sharing arm improves the control's premium TTFT p99 by %.1f ms against a repetition-to-repetition spread of %.1f ms",
		best, spread)
	return r
}

// sharingReadingFive: it protects, but not to the bar.
//
// This is the reading the previous study had no name for. Its readings required either that some cell met
// the bar or that no cell beat the control; the evidence landed between them and nothing fired. Here that
// gap is closed in advance rather than after seeing which way the numbers went.
func sharingReadingFive(shared ArmSummary, all []sharingScored, contenderTenant string) PoPReading {
	r := PoPReading{ID: "5", Name: "it protects but not to the bar -- measured partial result"}
	if countSharingScorable(all) == 0 {
		r.NotEvaluable = true
		r.Detail = "no sharing arm could be scored against the bars" + sharingUnscorableSuffix(all)
		return r
	}
	best, spread, ok := bestImprovementOverShared(shared, all)
	if !ok {
		r.NotEvaluable = true
		r.Detail = fmt.Sprintf("`shared` carries %d per-repetition tail(s), so there is no spread to call an improvement real against",
			len(shared.RepetitionTTFTMsP99))
		return r
	}
	// BOTH bars, not the tail alone.
	//
	// This checked ttftOK only, which left a hole exactly where this reading was supposed to close one. An
	// arm holding the tail at 1.5x R1 while its stream runs at 10x misses reading 1 (which gates on both),
	// misses reading 2 (which needs both bars met), misses reading 3 (it improved on the control), and was
	// then turned away here for having "met the 2x tail bar" -- so nothing fired at all. That is the same
	// gap the previous study's readings left and that this page was written to close, reappearing one bar
	// over.
	//
	// With both bars the four readings partition the space: met both and kept the contender's work is 1,
	// met both by starving it is 2, improved on the control without meeting both is this reading, and did
	// not improve beyond the control's own noise is 3.
	for i := range all {
		if all[i].computable && all[i].ttftOK && all[i].tpotOK {
			r.Detail = fmt.Sprintf("%s met both bars, so this reading does not apply", all[i].Arm)
			return r
		}
	}
	if best <= spread {
		r.Detail = fmt.Sprintf("no arm improved on the control by more than its %.1f ms spread, which is reading 3 rather than this one", spread)
		return r
	}
	r.Fired = true
	var closest *sharingScored
	for i := range all {
		if !all[i].computable {
			continue
		}
		if closest == nil || all[i].ttft < closest.ttft {
			closest = &all[i]
		}
	}
	r.Cell = closest.Arm
	missed := "the tail bar"
	switch {
	case closest.ttftOK && !closest.tpotOK:
		missed = "the stream bar"
	case !closest.ttftOK && !closest.tpotOK:
		missed = "both bars"
	}
	// AND IT SAYS WHETHER THE CONTENDER WAS REFUSED, because that changes what the improvement is.
	//
	// A starved arm stays computable on purpose -- that is reading 2's finding and reading 2 is the one
	// that names it. But reading 2 requires BOTH bars, so an arm that refuses most of the contender's work,
	// improves the tail, and misses a bar falls out of reading 2 and lands here, where the sentence read
	// "a real improvement that does not reach the bar" and never mentioned the refusals. A review
	// reproduced exactly that. The improvement is real; what it was bought with belongs in the same line.
	// The LEDGER, not the starvation flag. `starved` is a threshold, and a threshold is the wrong thing to
	// hang a factual sentence on: refusing 20 of 139 requests per repetition leaves the flag false and this
	// line said "with the contender's work intact" over a ledger that records twenty refusals. A review
	// reproduced it. What a reader needs is the counts; whether they amount to starvation is reading 2's
	// judgement and it is made elsewhere.
	bought := "the contender's disposition was not recorded"
	if d, ok := closest.DispositionByTenant[contenderTenant]; ok {
		switch {
		case d.Rejected == 0 && d.TimedOut == 0 && d.Failed == 0:
			bought = fmt.Sprintf("with all %d of the contender's requests served", d.Offered)
		default:
			bought = fmt.Sprintf("with the contender at %d of %d served (%d refused, %d timed out, %d failed)",
				d.Completed, d.Offered, d.Rejected, d.TimedOut, d.Failed)
		}
	}
	r.Detail = fmt.Sprintf("%s improves the control's premium tail by %.1f ms against a %.1f ms spread, and misses %s: tail %.1fx R1 against %.1fx, stream %.2fx against %.2fx -- a real improvement that does not reach the bar, %s",
		closest.Arm, best, spread, missed, closest.ttft, m5cTTFTBar, closest.tpot, m5cTPOTBar, bought)
	return r
}

// bestImprovementOverShared returns the largest premium-tail improvement any scorable arm makes on the
// control, and the control's own repetition spread to judge it against.
//
// Only SCORABLE arms count. An arm whose premium requests all timed out has a TTFTMsP99 of 0, which would
// otherwise register as the largest improvement of all.
func bestImprovementOverShared(shared ArmSummary, all []sharingScored) (best, spread float64, ok bool) {
	spread, ok = repetitionSpread(shared.RepetitionTTFTMsP99)
	if !ok {
		return 0, 0, false
	}
	for i := range all {
		if !all[i].computable {
			continue
		}
		if imp := shared.TTFTMsP99 - all[i].TTFTMsP99; imp > best {
			best = imp
		}
	}
	return best, spread, true
}

func countSharingScorable(all []sharingScored) int {
	n := 0
	for i := range all {
		if all[i].computable {
			n++
		}
	}
	return n
}

// sharingUnscorableSuffix names what could not be scored, so a reading that did not fire says whether it
// looked at everything or at nothing.
func sharingUnscorableSuffix(all []sharingScored) string {
	var why []string
	for i := range all {
		if !all[i].computable && all[i].why != "" {
			why = append(why, all[i].why)
		}
	}
	if len(why) == 0 {
		return ""
	}
	sort.Strings(why)
	return " (" + strings.Join(why, "; ") + ")"
}

// FormatSharingMatrix renders the readings in the order they were evaluated.
func FormatSharingMatrix(res SharingResult) string {
	var b strings.Builder
	b.WriteString("PRE-REGISTERED READINGS (docs/superpowers/specs/2026-09-10-does-splitting-the-card-buy-protection.md)\n")
	for _, r := range res.Readings {
		mark := "     "
		switch {
		case r.Fired:
			mark = readingFired
		case r.NotEvaluable:
			mark = " N/E "
		}
		fmt.Fprintf(&b, "  [%s] %-3s %s\n", mark, r.ID, r.Name)
		if r.Detail != "" {
			b.WriteString("          " + r.Detail + "\n")
		}
	}
	if res.Answer == "" {
		b.WriteString("\nANSWER: none of the readings fired. That is not a result; it is a gap in the outcome space,\n")
		b.WriteString("and the pre-registration says what to do about one rather than leaving it to a reader.\n")
	} else {
		b.WriteString("\nANSWER: " + res.Answer + "\n")
	}
	return b.String()
}
