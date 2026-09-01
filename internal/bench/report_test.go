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
		// Admitted-work over the eligible (>=4096-token) population: one offered (rejected) + one offered (timed out, not a 429 so counts as admitted work).
		Expect(s.OfferedInputTokens).To(Equal(int64(16000)))
		Expect(s.AdmittedInputTokens).To(Equal(int64(8000))) // the timeout row was admitted (not 429); the 429 was not
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
		Expect(c.InvalidReason).To(ContainSubstring("no incremental confidence interval"))
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
