package bench

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The fixtures are built from the pre-registration's bars rather than from the pilot's numbers, so a reading
// that drifts from the document fails here rather than agreeing with whatever the last run happened to do.
//
// R1: TTFT p99 100 ms, premium TPOT p99 20 ms. Control: TTFT p99 1000 ms (10x, so reading 4 never fires by
// accident), 1000 output tokens of which the noisy tenant has 400, throughput 100 tok/s.
func popR1() ArmSummary {
	return ArmSummary{
		Arm: ArmR1, TTFTMsP99: 100, TailSampleSize: 500,
		TPOTMsP99ByTenant: map[string]float64{PremiumTenant: 20},
	}
}

func popControl() ArmSummary {
	return ArmSummary{
		Arm: ArmDefaultFCFS, TTFTMsP99: 1000, TailSampleSize: 500,
		TPOTMsP99ByTenant:     map[string]float64{PremiumTenant: 200},
		OutputTokens:          1000,
		OutputTokensByTenant:  map[string]int64{PremiumTenant: 600, NoisyTenant: 400},
		OutputTokensPerSecond: 100,
		RepetitionTTFTMsP99:   []float64{980, 1000, 1020},
		RepetitionCount:       3,
		// Both populations clear MinTailSamples, so reading 4b passes and the readings under it are reached.
		// A control that does not clear it makes every ratio below a ratio over a remnant.
		DispositionByTenant: map[string]Disposition{
			PremiumTenant: {Offered: 600, Completed: 500, TimedOut: 100},
			NoisyTenant:   {Offered: 600, Completed: 400, TimedOut: 200},
		},
	}
}

// popCell builds a cell at the given multiples of the bars. share is the noisy tenant's fraction of the
// control's share, and thru is the fraction of the control's throughput.
func popCell(name string, ttftX, tpotX, share, thru float64) ArmSummary {
	noisy := int64(400 * share)
	return ArmSummary{
		Arm: name, TTFTMsP99: 100 * ttftX, TailSampleSize: 500,
		TPOTMsP99ByTenant:     map[string]float64{PremiumTenant: 20 * tpotX},
		OutputTokens:          1000,
		OutputTokensByTenant:  map[string]int64{PremiumTenant: 1000 - noisy, NoisyTenant: noisy},
		OutputTokensPerSecond: 100 * thru,
		DispositionByTenant:   map[string]Disposition{NoisyTenant: {Offered: 100, Completed: 100}},
	}
}

var _ = Describe("the price-of-protection readings", func() {
	It("fires reading 4 and stops when the load made no contention", func() {
		control := popControl()
		control.TTFTMsP99 = 400 // 4x R1, under the 5x bar
		res := EvaluatePriceOfProtection(popR1(), control,
			[]ArmSummary{popCell("mbt-0512-priority", 1.0, 1.0, 1.0, 1.0)}, PremiumTenant, NoisyTenant)

		Expect(res.Readings[0].ID).To(Equal("4"))
		Expect(res.Readings[0].Fired).To(BeTrue())
		Expect(res.Answer).To(Equal("4"))
		// And nothing below it is scored: an uncontended load makes those comparisons meaningless, not
		// negative, so reporting them as "did not fire" would be four wrong answers.
		Expect(res.Readings).To(HaveLen(1))
	})

	It("fires reading 4b and stops when the load was too high to measure", func() {
		// The 2026-09-07 pilot's control: contended past any doubt, and 17 premium completions of 555.
		// Reading 4 passes it, every reading below it computes a tail and two shares over a remnant, and
		// nothing in the original pre-registration would have said so.
		control := popControl()
		control.TailSampleSize = 17
		control.DispositionByTenant = map[string]Disposition{
			PremiumTenant: {Offered: 555, Completed: 17, TimedOut: 538},
			NoisyTenant:   {Offered: 544, Completed: 26, TimedOut: 518},
		}
		res := EvaluatePriceOfProtection(popR1(), control,
			[]ArmSummary{popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98)}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "4").Fired).To(BeFalse()) // the load WAS contended
		Expect(readingByID(res, "4b").Fired).To(BeTrue())
		Expect(res.Answer).To(Equal("4b"))
		Expect(res.Readings).To(HaveLen(2)) // and nothing below is scored on a remnant
	})

	It("fires reading 4b when the premium tail collapses even though the contending tenant's survives", func() {
		// The mirror of the case below, and it exists because the two clauses have to be pinned separately.
		// A fixture that starves both populations at once passes whichever clause is left when the other is
		// broken, so it cannot tell which one is doing the work -- and a mutation removing the premium clause
		// went undetected until this case was added.
		control := popControl()
		control.TailSampleSize = 17
		control.DispositionByTenant = map[string]Disposition{
			PremiumTenant: {Offered: 555, Completed: 17, TimedOut: 538},
			NoisyTenant:   {Offered: 600, Completed: 400},
		}
		res := EvaluatePriceOfProtection(popR1(), control,
			[]ArmSummary{popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98)}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "4b").Fired).To(BeTrue())
		Expect(readingByID(res, "4b").Detail).To(ContainSubstring("premium"))
	})

	It("fires reading 4b when the premium tail survives but the contending tenant's does not", func() {
		control := popControl()
		control.DispositionByTenant = map[string]Disposition{
			PremiumTenant: {Offered: 600, Completed: 500},
			NoisyTenant:   {Offered: 600, Completed: 12, TimedOut: 588},
		}
		res := EvaluatePriceOfProtection(popR1(), control,
			[]ArmSummary{popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98)}, PremiumTenant, NoisyTenant)

		// The share clauses divide by this tenant's share under the control. Twelve completions is not a share.
		Expect(readingByID(res, "4b").Fired).To(BeTrue())
		Expect(readingByID(res, "4b").Detail).To(ContainSubstring(NoisyTenant))
	})

	It("fires reading 1 for a cell that holds all four bars", func() {
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0256-fcfs", 5.0, 1.0, 1.0, 1.0),      // tail too slow
			popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98), // all four
		}, PremiumTenant, NoisyTenant)

		Expect(res.Answer).To(Equal("1 (mbt-0512-priority)"))
	})

	It("does not fire reading 1 for a cell that protects the tail by deleting the tenant's work", func() {
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0512-priority", 1.5, 1.1, 0.10, 1.0), // share far below 0.75
		}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "1").Fired).To(BeFalse())
	})

	It("fires reading 1b when the only thing a cell misses is throughput", func() {
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.80), // 80% of the control's throughput
		}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "1").Fired).To(BeFalse())
		Expect(readingByID(res, "1b").Fired).To(BeTrue())
		Expect(readingByID(res, "1b").Detail).To(ContainSubstring("20%"))
	})

	It("fires reading 2 only when the ledger shows the work was refused, not merely late", func() {
		deleted := popCell("mbt-0512-fcfs", 1.5, 1.1, 0.10, 1.0)
		deleted.DispositionByTenant = map[string]Disposition{
			NoisyTenant: {Offered: 100, Completed: 10, Rejected: 90},
		}
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{deleted}, PremiumTenant, NoisyTenant)
		Expect(readingByID(res, "2").Fired).To(BeTrue())

		// The same small share, reached by timing out instead. That is delay, and the pre-registration is
		// explicit that a share loss alone cannot tell the two apart.
		late := popCell("mbt-0512-fcfs", 1.5, 1.1, 0.10, 1.0)
		late.DispositionByTenant = map[string]Disposition{
			NoisyTenant: {Offered: 100, Completed: 10, TimedOut: 90},
		}
		res = EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{late}, PremiumTenant, NoisyTenant)
		Expect(readingByID(res, "2").Fired).To(BeFalse())
		Expect(readingByID(res, "2").Detail).To(ContainSubstring("delay rather than deletion"))
	})

	It("declines to decide reading 2 when the evidence carries no per-tenant ledger", func() {
		blind := popCell("mbt-0512-fcfs", 1.5, 1.1, 0.10, 1.0)
		blind.DispositionByTenant = nil
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{blind}, PremiumTenant, NoisyTenant)

		r := readingByID(res, "2")
		Expect(r.NotEvaluable).To(BeTrue())
		Expect(r.Fired).To(BeFalse())
	})

	It("declines to decide reading 3 without at least two repetitions of the control", func() {
		control := popControl()
		control.RepetitionTTFTMsP99 = []float64{1000}
		control.RepetitionCount = 1
		res := EvaluatePriceOfProtection(popR1(), control, []ArmSummary{
			popCell("mbt-0512-fcfs", 5.0, 5.0, 1.0, 1.0),
		}, PremiumTenant, NoisyTenant)

		r := readingByID(res, "3")
		Expect(r.NotEvaluable).To(BeTrue())
		Expect(r.Fired).To(BeFalse())
	})

	It("fires reading 3 when no cell beats the control by more than its own spread", func() {
		// Control spread is 1020-980 = 40 ms. This cell improves it by 10 ms, which is inside the noise.
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			{Arm: "mbt-0512-fcfs", TTFTMsP99: 990, TailSampleSize: 500,
				TPOTMsP99ByTenant: map[string]float64{PremiumTenant: 200}, OutputTokens: 1000,
				OutputTokensByTenant: map[string]int64{NoisyTenant: 400}, OutputTokensPerSecond: 100},
		}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "3").Fired).To(BeTrue())
	})

	It("picks the highest noisy share when several cells qualify", func() {
		// The confirmatory run offers eight cells at once and nothing until now has scored more than two.
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0256-fcfs", 5.0, 1.0, 1.00, 1.0),     // tail fails
			popCell("mbt-0256-priority", 1.2, 1.1, 0.80, 1.0), // qualifies, share 0.80
			popCell("mbt-0512-fcfs", 1.9, 5.0, 1.00, 1.0),     // TPOT fails
			popCell("mbt-0512-priority", 1.3, 1.1, 0.95, 1.0), // qualifies, share 0.95 <- winner
			popCell("mbt-1024-fcfs", 1.5, 1.1, 0.50, 1.0),     // share fails
			popCell("mbt-1024-priority", 1.4, 1.1, 0.85, 1.0), // qualifies, share 0.85
			popCell("mbt-2048-fcfs", 1.6, 1.1, 0.90, 0.50),    // throughput fails
			popCell("mbt-2048-priority", 1.7, 1.1, 0.78, 1.0), // qualifies, share 0.78
		}, PremiumTenant, NoisyTenant)

		Expect(res.Answer).To(Equal("1 (mbt-0512-priority)"))
	})

	It("breaks an exact tie toward the larger budget", func() {
		// The pre-registration says so and says why: the larger budget is the smaller change from the
		// control, and its first draft broke ties the other way on a claim about operating cost that this
		// run has no evidence for. Cells arrive in the study's order, which is ascending by budget, so the
		// later of two equal shares is the larger budget.
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0256-priority", 1.2, 1.1, 0.90, 1.0),
			popCell("mbt-2048-priority", 1.2, 1.1, 0.90, 1.0),
		}, PremiumTenant, NoisyTenant)

		Expect(res.Answer).To(Equal("1 (mbt-2048-priority)"))
	})

	// The four defects an adversarial review found on 2026-09-09, each pinned by the case that produced it.
	It("refuses to score a cell that completed no premium requests", func() {
		// This fired reading 1 POSITIVE and named the cell the deliverable. percentile returns 0 for an
		// empty slice, so a cell whose premium traffic ALL timed out had a tail of 0, and 0 clears a 2x bar.
		// The configuration that starved the protected tenant completely was reported as the one protecting
		// it, which is the worst number this file could print.
		starved := popCell("mbt-0256-fcfs", 1.0, 1.0, 1.0, 1.0)
		starved.TTFTMsP99 = 0
		starved.TailSampleSize = 0
		starved.TPOTMsP99ByTenant = map[string]float64{}
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{starved}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "1").Fired).To(BeFalse())
		Expect(res.Answer).NotTo(ContainSubstring("mbt-0256-fcfs"))
		Expect(readingByID(res, "1").Detail).To(ContainSubstring("no tail"))
	})

	It("refuses to score a cell whose tail is thinner than a p99 needs, or censored", func() {
		thin := popCell("mbt-0512-priority", 1.0, 1.0, 1.0, 1.0)
		thin.TailSampleSize = 40
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{thin}, PremiumTenant, NoisyTenant)
		Expect(readingByID(res, "1").Fired).To(BeFalse())
		Expect(readingByID(res, "1").Detail).To(ContainSubstring("40 premium completions"))

		censored := popCell("mbt-0512-priority", 1.0, 1.0, 1.0, 1.0)
		censored.Censored = true
		res = EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{censored}, PremiumTenant, NoisyTenant)
		Expect(readingByID(res, "1").Fired).To(BeFalse())
		Expect(readingByID(res, "1").Detail).To(ContainSubstring("censored"))
	})

	It("does not call an unmeasured control an uncontended load", func() {
		// 0/67 is under the 5x floor, so reading 4 fired INVALID saying the load made no contention -- the
		// inverse of what happened -- and short-circuited ahead of 4b, which exists for exactly this case.
		control := popControl()
		control.TTFTMsP99 = 0
		res := EvaluatePriceOfProtection(popR1(), control,
			[]ArmSummary{popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98)}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "4").Fired).To(BeFalse())
		Expect(readingByID(res, "4").NotEvaluable).To(BeTrue())
		Expect(readingByID(res, "4").Detail).To(ContainSubstring("too high to measure"))
	})

	It("declines reading 3 rather than answering over cells nobody could score", func() {
		// With no premium TPOT on R1 every cell is unscorable, and reading 3 -- the last one, the one that
		// fires when the others did not -- turned that into an INCONCLUSIVE verdict about unscored evidence.
		r1 := popR1()
		r1.TPOTMsP99ByTenant = map[string]float64{}
		res := EvaluatePriceOfProtection(r1, popControl(),
			[]ArmSummary{popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98)}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "3").Fired).To(BeFalse())
		Expect(readingByID(res, "3").NotEvaluable).To(BeTrue())
		Expect(res.Answer).To(BeEmpty())
	})

	It("says a control ledger that does not carry the contender is unevaluable, not a starved tenant", func() {
		control := popControl()
		control.DispositionByTenant = map[string]Disposition{PremiumTenant: {Offered: 600, Completed: 500}}
		res := EvaluatePriceOfProtection(popR1(), control,
			[]ArmSummary{popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98)}, PremiumTenant, NoisyTenant)

		Expect(readingByID(res, "4b").NotEvaluable).To(BeTrue())
		Expect(readingByID(res, "4b").Fired).To(BeFalse())
	})

	It("reports no answer rather than a false one when nothing fired", func() {
		// A cell that beats the control well past its spread, but misses every positive bar. None of the five
		// applies, and the honest output is silence rather than the nearest negative.
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0512-priority", 3.0, 5.0, 1.0, 1.0),
		}, PremiumTenant, NoisyTenant)

		Expect(res.Answer).To(BeEmpty())
		Expect(FormatPriceOfProtection(res)).To(ContainSubstring("none of the readings fired"))
	})

	It("prints every reading with the numbers it was decided on", func() {
		res := EvaluatePriceOfProtection(popR1(), popControl(), []ArmSummary{
			popCell("mbt-0512-priority", 1.5, 1.1, 0.9, 0.98),
		}, PremiumTenant, NoisyTenant)
		out := FormatPriceOfProtection(res)

		for _, id := range []string{"1", "1b", "2", "3", "4"} {
			Expect(out).To(MatchRegexp(`(?m)^  ` + id + `\s`))
		}
		Expect(strings.Count(out, "\n")).To(BeNumerically(">", 10))
	})
})

func readingByID(res PoPResult, id string) PoPReading {
	for _, r := range res.Readings {
		if r.ID == id {
			return r
		}
	}
	return PoPReading{ID: "missing:" + id}
}
