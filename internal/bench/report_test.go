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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// completedRow builds a raw row for a request that finished with the given TTFT in ms.
func completedRow(index int, ttftMs float64, estInput int) RawRow {
	base := int64(1_000_000_000)
	return RawRow{
		Index:               index,
		SendUnixNanos:       base,
		FirstTokenUnixNanos: base + int64(ttftMs*1e6),
		EndUnixNanos:        base + int64((ttftMs+1)*1e6),
		EstInputTokens:      estInput,
		OutputTokens:        8,
		HTTPStatus:          200,
	}
}

var _ = Describe("percentile", func() {
	It("uses nearest-rank on an observed value", func() {
		s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		Expect(percentile(s, 0.50)).To(Equal(float64(5)))
		Expect(percentile(s, 0.99)).To(Equal(float64(10)))
		Expect(percentile(s, 0.90)).To(Equal(float64(9)))
	})
})

var _ = Describe("Summarize", func() {
	It("counts outcomes and computes the tail over completed requests", func() {
		var rows []RawRow
		// 100 completed requests with TTFT 1..100 ms.
		for i := 1; i <= 100; i++ {
			rows = append(rows, completedRow(i, float64(i), 100))
		}
		// One rejection and one timeout.
		//
		// Only the rejection is offered work. A timeout produced no response, so nothing says whether the
		// guard admitted it or refused it, and the offered total is 8000 rather than 16000 for that reason.
		rows = append(rows, RawRow{Index: 101, SendUnixNanos: 1, HTTPStatus: 429, EstInputTokens: 8000})
		rows = append(rows, RawRow{Index: 102, SendUnixNanos: 1, ErrorKind: "timeout", EstInputTokens: 8000})

		s := Summarize("off", rows)
		Expect(s.Total).To(Equal(102))
		Expect(s.Completed).To(Equal(100))
		Expect(s.Rejected).To(Equal(1))
		Expect(s.TimedOut).To(Equal(1))
		Expect(s.TTFTMsP50).To(Equal(float64(50)))
		Expect(s.TTFTMsP99).To(Equal(float64(99)))
		Expect(s.TailSampleSize).To(Equal(100))
		// Admitted-work over the eligible (>=4096-token) population.
		//
		// The rejection is offered and not admitted. The timeout is neither: no response arrived, so nothing
		// says what the guard decided about it, and it is counted as a lost verdict instead. It used to be
		// scored as admitted for the sole reason that it was not a 429, which is how an arm that transmitted
		// nothing could report a perfect admission match.
		Expect(s.OfferedInputTokens).To(Equal(int64(8000)))
		Expect(s.AdmittedInputTokens).To(BeZero())
		Expect(s.AdmissionLost).To(Equal(1))
	})

	It("flags the tail as censored when more than 1% of requests time out", func() {
		var rows []RawRow
		for i := 1; i <= 98; i++ {
			rows = append(rows, completedRow(i, float64(i), 100))
		}
		rows = append(rows, RawRow{Index: 99, SendUnixNanos: 1, ErrorKind: "timeout"})
		rows = append(rows, RawRow{Index: 100, SendUnixNanos: 1, ErrorKind: "timeout"})
		s := Summarize("off", rows)
		Expect(s.TimedOut).To(Equal(2))
		Expect(s.Censored).To(BeTrue()) // 2/100 = 2% > 1%
	})
})

var _ = Describe("BootstrapCI", func() {
	It("degenerates to the point estimate for a single repetition", func() {
		ci := BootstrapCI([]float64{42}, 1000, 7, 0.05)
		Expect(ci.Lo).To(Equal(float64(42)))
		Expect(ci.Hi).To(Equal(float64(42)))
	})

	It("brackets the mean and is reproducible for a fixed seed", func() {
		vals := []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19} // mean 14.5
		a := BootstrapCI(vals, 2000, 99, 0.05)
		b := BootstrapCI(vals, 2000, 99, 0.05)
		Expect(a).To(Equal(b)) // deterministic
		Expect(a.Lo).To(BeNumerically("<", 14.5))
		Expect(a.Hi).To(BeNumerically(">", 14.5))
	})
})

var _ = Describe("EvaluateChecks", func() {
	It("passes when C beats R1 absolutely and B incrementally with a matched admission fraction", func() {
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		// static-cap and kv-aware admit the same work (matched), but kv-aware protects the tail better.
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 500}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 110, OfferedInputTokens: 1000, AdmittedInputTokens: 510, TailSampleSize: 500}
		ci := CI{Lo: 0.45, Hi: 0.65, Valid: true} // C/B ratio CI, upper bound < 1.0
		c := EvaluateChecks(r1, staticCap, kvAware, ci, 0.05)
		Expect(c.Invalid).To(BeFalse())
		Expect(c.AbsoluteProtectionPass).To(BeTrue()) // 110/100 = 1.10 <= 1.25
		Expect(c.IncrementalValuePass).To(BeTrue())   // 110/200 = 0.55 <= 0.90, CI hi 0.65 < 1.0
		Expect(c.AdmissionMatchPass).To(BeTrue())     // |0.5-0.51|/0.51 ~ 0.02 <= 0.05
		Expect(c.OverallPass).To(BeTrue())
	})

	It("disqualifies a run whose tail is thin enough that its p99 is just the slowest request", func() {
		// 99 is the largest count at which nearest-rank p99 still lands on the maximum: index
		// ceil(0.99*99)-1 = 98 of 0..98. Every check below would otherwise pass on it -- the numbers are the
		// ones from the passing case above -- so the disqualification has to come from the sample size
		// alone, which is the whole point.
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 500}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 110, OfferedInputTokens: 1000, AdmittedInputTokens: 510, TailSampleSize: 99}
		c := EvaluateChecks(r1, staticCap, kvAware, CI{Lo: 0.45, Hi: 0.65, Valid: true}, 0.05)
		Expect(c.Invalid).To(BeTrue())
		Expect(c.InvalidReason).To(ContainSubstring("99 premium completions"))
		Expect(c.OverallPass).To(BeFalse())
	})

	It("accepts the smallest tail at which the p99 is no longer the maximum", func() {
		// One more completion than the case above, and the same numbers, so this pins the boundary rather
		// than merely re-testing the passing case: 100 is where ceil(0.99*n)-1 first falls below n-1.
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 100}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 100}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 110, OfferedInputTokens: 1000, AdmittedInputTokens: 510, TailSampleSize: 100}
		c := EvaluateChecks(r1, staticCap, kvAware, CI{Lo: 0.45, Hi: 0.65, Valid: true}, 0.05)
		Expect(c.Invalid).To(BeFalse())
		Expect(c.OverallPass).To(BeTrue())
	})

	It("fails incremental value when C does not beat the admission-matched B (just load shedding)", func() {
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 115, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 500}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 112, OfferedInputTokens: 1000, AdmittedInputTokens: 505, TailSampleSize: 500}
		ci := CI{Lo: 0.90, Hi: 1.05, Valid: true} // ratio ~0.97, CI crosses 1.0
		c := EvaluateChecks(r1, staticCap, kvAware, ci, 0.05)
		Expect(c.AbsoluteProtectionPass).To(BeTrue()) // 112/100 = 1.12 <= 1.25
		Expect(c.IncrementalValuePass).To(BeFalse())  // ratio 0.97 > 0.90 and CI hi >= 1.0
		Expect(c.OverallPass).To(BeFalse())
	})

	It("fails admission match when B and C admit different work fractions", func() {
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 300, TailSampleSize: 500}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 110, OfferedInputTokens: 1000, AdmittedInputTokens: 600, TailSampleSize: 500}
		ci := CI{Lo: 0.45, Hi: 0.65, Valid: true}
		c := EvaluateChecks(r1, staticCap, kvAware, ci, 0.05)
		Expect(c.AdmissionMatchPass).To(BeFalse()) // 0.3 vs 0.6 is a 50% gap
		Expect(c.OverallPass).To(BeFalse())
	})

	It("invalidates the run when a compared arm completed no premium requests", func() {
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 500}
		// kv-aware completed nothing (e.g. every admitted request timed out): its p99 is 0 and would otherwise pass every ratio check.
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 0, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 0}
		ci := CI{} // no interval: the bootstrap had nothing to resample
		c := EvaluateChecks(r1, staticCap, kvAware, ci, 0.05)
		Expect(c.Invalid).To(BeTrue())
		Expect(c.OverallPass).To(BeFalse()) // never certifies a zero-completion run as protection
	})

	It("invalidates the run when a compared arm's tail is censored", func() {
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 500}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 110, OfferedInputTokens: 1000, AdmittedInputTokens: 510, TailSampleSize: 500, Censored: true}
		ci := CI{Lo: 0.45, Hi: 0.65, Valid: true}
		c := EvaluateChecks(r1, staticCap, kvAware, ci, 0.05)
		Expect(c.Invalid).To(BeTrue())
		Expect(c.OverallPass).To(BeFalse())
	})
})

var _ = Describe("the two ways a truncated arm used to certify itself", func() {
	// Both were demonstrated by an adversarial review that fed the real harness an arm cut short at 120
	// premium completions with 380 transport failures and one repetition against static-cap's two. It printed
	// "VERDICT: all checks passed; the guard protects".
	//
	// That is the failure mode of a node going away mid-run, and it is not specific to Spot: an evicted engine
	// Pod, an OOM kill or a network blip produce the same rows.

	It("censors a tail lost to transport errors, not only to timeouts", func() {
		// 120 completions and 380 transport failures: 76% of the premium requests never produced a latency,
		// and the old rule -- which counted only errKindTimeout -- left this tail uncensored.
		rows := make([]RawRow, 0, 500)
		for i := range 120 {
			rows = append(rows, RawRow{
				Tenant: "premium", HTTPStatus: 200,
				SendUnixNanos:       1,
				FirstTokenUnixNanos: 1 + int64(i+1)*1e6,
				EndUnixNanos:        1 + int64(i+2)*1e6,
			})
		}
		for range 380 {
			rows = append(rows, RawRow{Tenant: "premium", ErrorKind: "transport"})
		}
		s := Summarize("kv-aware", rows)
		Expect(s.TailSampleSize).To(Equal(120))
		Expect(s.Failed).To(Equal(380))
		Expect(s.Censored).To(BeTrue(), "a tail missing 76% of its premium requests is a lower bound whatever the error kind was")
	})

	It("does not censor a tail that only shed load through admission", func() {
		// The mirror image, and the reason the rule cannot simply count every non-completion: a 429 is the
		// guard doing its job. If rejections censored the tail, the arm that protects best would disqualify
		// itself for protecting.
		rows := make([]RawRow, 0, 500)
		for i := range 120 {
			rows = append(rows, RawRow{
				Tenant: "premium", HTTPStatus: 200,
				SendUnixNanos:       1,
				FirstTokenUnixNanos: 1 + int64(i+1)*1e6,
				EndUnixNanos:        1 + int64(i+2)*1e6,
			})
		}
		for range 380 {
			rows = append(rows, RawRow{Tenant: "premium", HTTPStatus: 429})
		}
		s := Summarize("kv-aware", rows)
		Expect(s.Rejected).To(Equal(380))
		Expect(s.Censored).To(BeFalse())
	})

	It("refuses the incremental check when no confidence interval was computed", func() {
		// Unequal repetition counts make the caller skip the bootstrap, leaving the zero CI. Its Hi is 0.0,
		// so the gate `Hi < 1.0` was satisfied by the ABSENCE of an interval -- truncation disarmed the
		// strictest check in the design instead of tripping it.
		r1 := ArmSummary{Arm: "R1", TTFTMsP99: 100, TailSampleSize: 500}
		staticCap := ArmSummary{Arm: "static-cap", TTFTMsP99: 200, OfferedInputTokens: 1000, AdmittedInputTokens: 500, TailSampleSize: 500}
		kvAware := ArmSummary{Arm: "kv-aware", TTFTMsP99: 110, OfferedInputTokens: 1000, AdmittedInputTokens: 510, TailSampleSize: 500}
		c := EvaluateChecks(r1, staticCap, kvAware, CI{}, 0.05)
		Expect(c.IncrementalValuePass).To(BeFalse())
		Expect(c.Invalid).To(BeTrue())
		Expect(c.InvalidReason).To(ContainSubstring("no usable confidence interval"))
		Expect(c.OverallPass).To(BeFalse())
	})

	It("marks a single repetition's degenerate interval invalid", func() {
		// BootstrapCI already said a single repetition "cannot bound its own variance" and then returned a
		// value the gate could not tell from a real interval.
		ci := BootstrapCI([]float64{0.55}, 2000, 1, 0.05)
		Expect(ci.Hi).To(Equal(0.55))
		Expect(ci.Valid).To(BeFalse())
	})

	It("marks a real bootstrap interval valid", func() {
		ci := BootstrapCI([]float64{0.50, 0.55, 0.60, 0.52}, 2000, 1, 0.05)
		Expect(ci.Valid).To(BeTrue())
		Expect(ci.Hi).To(BeNumerically(">", 0))
	})
})

var _ = Describe("the truncated repetition that pooling hides", func() {
	// The residual hole after censoring and CI.Valid were closed: an arm whose repetitions completed
	// 500/500/500/30 pools to 1,530 premium completions, clears MinTailSamples by a wide margin, and carries
	// one repetition whose p99 is a maximum over thirty requests -- which the incremental bootstrap then
	// resamples with equal weight.

	base := func(arm string, p99 float64, admitted int64) ArmSummary {
		return ArmSummary{Arm: arm, TTFTMsP99: p99, OfferedInputTokens: 1000, AdmittedInputTokens: admitted, TailSampleSize: 1530}
	}

	It("invalidates an arm whose weakest repetition is below the floor, however healthy the pool looks", func() {
		r1 := base("R1", 100, 500)
		staticCap := base("static-cap", 200, 500)
		kvAware := base("kv-aware", 110, 510)
		kvAware.RepetitionCount = 4
		kvAware.MinRepetitionTail = 30

		c := EvaluateChecks(r1, staticCap, kvAware, CI{Lo: 0.45, Hi: 0.65, Valid: true}, 0.05)
		Expect(c.Invalid).To(BeTrue())
		Expect(c.InvalidReason).To(ContainSubstring("30 premium completions"))
		Expect(c.InvalidReason).To(ContainSubstring("1530"), "the message names the pooled figure that hid it")
		Expect(c.OverallPass).To(BeFalse())
	})

	It("accepts an arm whose weakest repetition is exactly at the floor", func() {
		// Pins the boundary rather than re-testing the passing case: 100 is where ceil(0.99*n)-1 first falls
		// below n-1, and it is the same derivation as MinTailSamples.
		r1 := base("R1", 100, 500)
		staticCap := base("static-cap", 200, 500)
		kvAware := base("kv-aware", 110, 510)
		kvAware.RepetitionCount = 4
		kvAware.MinRepetitionTail = MinTailSamples

		c := EvaluateChecks(r1, staticCap, kvAware, CI{Lo: 0.45, Hi: 0.65, Valid: true}, 0.05)
		Expect(c.Invalid).To(BeFalse())
		Expect(c.OverallPass).To(BeTrue())
	})

	It("skips the check when the caller did not supply a repetition shape", func() {
		// Summarize sees pooled rows and cannot know how they were split, so absence must not fail closed --
		// otherwise every hand-built summary in this file would be invalid for a reason about plumbing.
		r1 := base("R1", 100, 500)
		staticCap := base("static-cap", 200, 500)
		kvAware := base("kv-aware", 110, 510)

		c := EvaluateChecks(r1, staticCap, kvAware, CI{Lo: 0.45, Hi: 0.65, Valid: true}, 0.05)
		Expect(c.Invalid).To(BeFalse())
	})
})

var _ = Describe("a request shed with 413", func() {
	// The gateway refuses a request larger than the bucket can ever hold with 413 rather than 429, because the
	// caller's remedy is a smaller prompt rather than a later one.
	// The report knew only about 429, so the arm that shed the hardest was the one that reported shedding
	// nothing: a paid static-cap replay refused 447 noisy requests and printed rejected=0.
	// These rows are that run's shape -- status 413, error kind "http", no first token -- so the fix is pinned
	// to the evidence that exposed it rather than to an invented one.
	shed := func(index int, tenant string, noisy bool) RawRow {
		return RawRow{
			Index:          index,
			SendUnixNanos:  1,
			Tenant:         tenant,
			IsNoisy:        noisy,
			EstInputTokens: 8000,
			HTTPStatus:     413,
			ErrorKind:      "http",
			LongThreshold:  4000,
		}
	}

	It("is a rejection, not a failure", func() {
		rows := []RawRow{completedRow(1, 10, 100), shed(2, "noisy", true), shed(3, "noisy", true)}
		s := Summarize("static-cap", rows)
		Expect(s.Rejected).To(Equal(2))
		Expect(s.Failed).To(BeZero())
	})

	It("is not counted as admitted work", func() {
		// Every one of these was refused, so an admission-match check reading this arm must see none of their
		// tokens admitted. Counting them made a cap that admitted nothing look like a cap that admitted all.
		rows := []RawRow{shed(1, "noisy", true), shed(2, "noisy", true)}
		s := Summarize("static-cap", rows)
		Expect(s.OfferedInputTokens).To(Equal(int64(16000)))
		Expect(s.AdmittedInputTokens).To(BeZero())
	})

	It("does not censor the premium tail, because shedding is the treatment", func() {
		// This is the distinction the 429 path already drew and this one did not: a rejection is the guard
		// working, while a transport error is a measurement that got away. Ten refusals against 100 premium
		// completions is far past the 1% censoring trigger, so a wrong answer here is visible.
		var rows []RawRow
		for i := 1; i <= 100; i++ {
			r := completedRow(i, float64(i), 100)
			r.Tenant = "premium"
			r.LongThreshold = 4000
			rows = append(rows, r)
		}
		for i := 101; i <= 110; i++ {
			rows = append(rows, shed(i, "premium", false))
		}
		s := Summarize("static-cap", rows)
		Expect(s.Censored).To(BeFalse())
		Expect(s.Rejected).To(Equal(10))
	})

	It("counts against a threshold probe's rejections", func() {
		rows := []RawRow{shed(1, thresholdProbePrefix+"long", false)}
		s := Summarize("static-cap", rows)
		Expect(s.ThresholdProbe[thresholdProbePrefix+"long"].Rejected).To(Equal(1))
	})
})

var _ = Describe("a threshold probe the gateway never evaluated", func() {
	// The probe tenants exist to prove the eligibility threshold discriminates between 4095 and 4096 estimated
	// tokens. A 403 means the gateway refused the tenant before admission ever ran, so the threshold did not
	// get to speak and the section has no evidence in it -- but it printed "rejected=0", which reads as the
	// threshold having considered the probe and let it through. A paid run shipped 92 such rows per arm and the
	// report presented them as a result.
	probe := func(status int) ArmSummary {
		return Summarize("off", []RawRow{{
			Index:          1,
			SendUnixNanos:  1,
			Tenant:         thresholdProbePrefix + "over",
			EstInputTokens: 4096,
			HTTPStatus:     status,
			ErrorKind:      "http",
			LongThreshold:  4000,
		}})
	}

	It("is tallied apart from a rejection", func() {
		o := probe(403).ThresholdProbe[thresholdProbePrefix+"over"]
		Expect(o.Unevaluated).To(Equal(1))
		Expect(o.Rejected).To(BeZero())
	})

	It("voids the section rather than reporting a threshold result", func() {
		out := FormatReport([]ArmSummary{probe(403)}, Checks{}, 0.05)
		Expect(out).To(ContainSubstring("VOID"))
	})

	It("leaves the section standing when the probes were actually evaluated", func() {
		out := FormatReport([]ArmSummary{probe(200)}, Checks{}, 0.05)
		Expect(out).NotTo(ContainSubstring("VOID"))
	})
})

var _ = Describe("admitted work when a request never reached admission", func() {
	// The 413 fix put neverEvaluated into the probe tally and not into the admitted-work block directly above
	// it, which is the same defect the fix was for, committed inside the fix. A 403 is not 429 and not 413, so
	// shedByAdmission says false and the row was counted as work the guard admitted. In the paid run the
	// probes estimate at 4,096 tokens against a 4,096 threshold, so all 180 of them per arm landed in the
	// eligible population -- and static-cap, which admitted nothing at all, scored 737,280 admitted tokens.
	//
	// A request the gateway turned away on credentials is not admitted work and not offered work either: it
	// is outside the population the guard was measured on, because the guard never saw it.
	It("is neither offered nor admitted", func() {
		rows := []RawRow{
			completedRow(1, 10, 8000),
			{Index: 2, SendUnixNanos: 1, Tenant: thresholdProbePrefix + "over", EstInputTokens: 4096, HTTPStatus: 403, ErrorKind: "http", LongThreshold: 4000},
		}
		rows[0].LongThreshold = 4000
		s := Summarize("static-cap", rows)
		Expect(s.OfferedInputTokens).To(Equal(int64(8000)))
		Expect(s.AdmittedInputTokens).To(Equal(int64(8000)))
	})
})

var _ = Describe("the eligible population when the evidence records a tier", func() {
	// The gateway's rule is tier == standard AND input over the threshold. Summarize read only the threshold,
	// because a raw row had no tier to read, and the two agreed only because the paid run's sole premium
	// tenant sent 50-token prompts against a 4,096 threshold. A premium tenant with a long prompt would have
	// put premium tokens into both terms of the admission-match fraction, which is a comparison of how much
	// STANDARD work each arm let through.
	//
	// Evidence written before the gateway reported its tier carries none, and must keep scoring the way it
	// did -- otherwise fixing this would silently rewrite the numbers of every run already on disk.
	row := func(tier string, est int) RawRow {
		r := completedRow(1, 10, est)
		r.Tier = tier
		r.LongThreshold = 4000
		return r
	}

	It("excludes a long premium request", func() {
		s := Summarize("off", []RawRow{row("premium", 8000), row("standard", 8000)})
		Expect(s.OfferedInputTokens).To(Equal(int64(8000)))
	})

	It("still counts a long request when the evidence predates the tier header", func() {
		s := Summarize("off", []RawRow{row("", 8000)})
		Expect(s.OfferedInputTokens).To(Equal(int64(8000)))
	})
})

var _ = Describe("an eligible request that never got an admission verdict", func() {
	// A request the gateway never answered says nothing about what the guard admitted, and Summarize counted
	// it as admitted work anyway: anything that was not 429, 413, 401 or 403 landed in both terms of the
	// fraction. Driven through the real report binary, an arm whose noisy traffic died in transport -- a third
	// of the arm gone -- printed "admission match |B-C|/C = 0.000 PASS".
	//
	// That is not a hypothetical failure mode. A port-forward dying mid-replay is the most frequent failure
	// this project has observed, twice in one run, and the script checks the forward only BEFORE each replay.
	//
	// The rule is the one the tail already uses: a lost observation is neither a success nor a refusal. It
	// leaves both terms and is counted, and past a threshold it disqualifies the comparison instead of
	// quietly shrinking it.
	dead := func(i int) RawRow {
		return RawRow{Index: i, SendUnixNanos: 1, Tenant: "standard-noisy", IsNoisy: true,
			EstInputTokens: 10000, HTTPStatus: 0, ErrorKind: "transport", LongThreshold: 4096}
	}
	admitted := func(i int) RawRow {
		r := completedRow(i, 10, 10000)
		r.Tenant = "standard-noisy"
		r.IsNoisy = true
		r.LongThreshold = 4096
		return r
	}

	It("is in neither term of the admitted-work fraction", func() {
		s := Summarize("static-cap", []RawRow{admitted(1), dead(2)})
		Expect(s.OfferedInputTokens).To(Equal(int64(10000)))
		Expect(s.AdmittedInputTokens).To(Equal(int64(10000)))
		Expect(s.AdmissionLost).To(Equal(1))
	})

	It("disqualifies the comparison once more than 1% of the eligible population is lost", func() {
		var rows []RawRow
		for i := 1; i <= 98; i++ {
			rows = append(rows, admitted(i))
		}
		rows = append(rows, dead(100), dead(101))
		b := Summarize("static-cap", rows)
		c := Summarize("kv-aware", rows[:98])
		r1 := Summarize("R1", []RawRow{completedRow(1, 10, 50)})
		checks := EvaluateChecks(r1, b, c, CI{}, 0.05)
		Expect(checks.Invalid).To(BeTrue())
		Expect(checks.InvalidReason).To(ContainSubstring("admission"))
	})

	It("keeps every reason when a run is broken in more than one way", func() {
		// The reason was overwritten rather than accumulated, so a run failing three ways reported the last
		// one and an operator fixed one problem at a time, paying for a run each round.
		empty := Summarize("kv-aware", nil)
		r1 := Summarize("R1", []RawRow{completedRow(1, 10, 50)})
		checks := EvaluateChecks(r1, empty, empty, CI{}, 0.05)
		Expect(checks.Invalid).To(BeTrue())
		Expect(strings.Count(checks.InvalidReason, "arm ")).To(BeNumerically(">=", 2))
	})
})

var _ = Describe("the incremental check when the repetition ratios scatter", func() {
	// The percentile bootstrap over four values is anti-conservative once the per-repetition ratios spread
	// out: simulated against this package's own BootstrapCI, a true ratio of 1.00 -- no effect whatsoever --
	// clears the gate 10.2% of the time at a coefficient of variation of 0.20, against a nominal 5%.
	//
	// So the interval is only worth reading while the ratios are tight. The 2026-09-03 pilot measured 0.001
	// for the contended arms and 0.056 for the isolation-like ones, well inside that, but a run is not
	// entitled to assume it stayed there. This refuses rather than reports, because the direction matters:
	// the failure mode is a gate that passes when it should not.
	It("is refused when the ratios are too scattered for the interval to mean anything", func() {
		Expect(RatioScatterTooHigh([]float64{0.9, 0.9, 0.9, 0.9})).To(BeFalse())
		Expect(RatioScatterTooHigh([]float64{0.6, 0.9, 1.2, 1.5})).To(BeTrue())
	})

	It("says nothing about scatter it cannot measure", func() {
		// One repetition has no spread to speak of, and the CI is already invalid for that reason.
		Expect(RatioScatterTooHigh([]float64{0.9})).To(BeFalse())
		Expect(RatioScatterTooHigh(nil)).To(BeFalse())
	})
})

var _ = Describe("the admitted-work fraction over exact tokens", func() {
	// The design defines the admission-match criterion over EXACT target-tokenizer input tokens. Every run so
	// far computed it over ceil(chars/4), which this project's own calibration records as 36% low on a
	// 200-character prompt and 23% high on a 40,000-character one. The pre-registered criterion has therefore
	// never been evaluated -- a proxy for it has.
	//
	// Falling back to the estimate is exactly how that happened, so a report with no exact counts refuses the
	// check instead of quietly answering a different question.
	row := func(i, est, exact, engine, status int) RawRow {
		r := completedRow(i, 10, est)
		r.Tenant = "standard-noisy"
		r.IsNoisy = true
		r.LongThreshold = 4096
		r.ExactInputTokens = exact
		r.EngineInputTokens = engine
		r.HTTPStatus = status
		if status != 200 {
			r.ErrorKind = "rejected"
			r.FirstTokenUnixNanos, r.EndUnixNanos = 0, 0
		}
		return r
	}

	It("scores offered and admitted work in exact tokens, including the refused", func() {
		// 10,000 estimated is 7,695 exact. One admitted, one refused.
		s := Summarize("static-cap", []RawRow{row(1, 10000, 7695, 7695, 200), row(2, 10000, 7695, 0, 429)})
		Expect(s.OfferedExactTokens).To(Equal(int64(15390)))
		Expect(s.AdmittedExactTokens).To(Equal(int64(7695)))
		Expect(s.ExactTokensMissing).To(BeZero())
	})

	It("counts an eligible row that carries no exact measurement", func() {
		s := Summarize("static-cap", []RawRow{row(1, 10000, 0, 0, 200)})
		Expect(s.ExactTokensMissing).To(Equal(1))
	})

	It("refuses the admission-match check rather than scoring it on estimates", func() {
		unmeasured := Summarize("static-cap", []RawRow{row(1, 10000, 0, 0, 200)})
		r1 := Summarize("R1", []RawRow{completedRow(1, 10, 50)})
		checks := EvaluateChecks(r1, unmeasured, unmeasured, CI{}, 0.05)
		Expect(checks.Invalid).To(BeTrue())
		Expect(checks.InvalidReason).To(ContainSubstring("no measured input-token count"))
	})

	It("refuses when the engine's own count contradicts the trace's", func() {
		// The trace is stamped once per prompt length; the engine reports on every admitted request. A
		// disagreement means the trace was stamped against a different tokenizer, or a different prompt ran.
		s := Summarize("static-cap", []RawRow{row(1, 10000, 7695, 6000, 200)})
		Expect(s.ExactTokensContradicted).To(Equal(1))
	})
})
