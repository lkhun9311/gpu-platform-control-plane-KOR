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
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

// httpStatusTooManyRequests is the status the gateway returns when an admission control rejects a request.
const httpStatusTooManyRequests = 429

// httpStatusInputExceedsBurst is the status the gateway returns when a prompt is larger than the bucket can ever hold.
//
// The gateway splits its refusals deliberately: 429 tells the caller to come back later, 413 tells it to send
// something smaller, because a request that cannot fit any bucket would otherwise be retried forever.
const httpStatusInputExceedsBurst = 413

// httpStatusUnauthorized and httpStatusForbidden are the statuses the gateway returns before admission runs.
const (
	httpStatusUnauthorized = 401
	httpStatusForbidden    = 403
)

// errKindTimeout is the RawRow.ErrorKind the replay client records when a request exceeds its deadline.
const errKindTimeout = "timeout"

// ArmSummary is the analysis of one arm's raw evidence for one repetition.
//
// Latency percentiles are in milliseconds, computed from the client-side raw timestamps.
// thresholdProbePrefix names the probe tenants the trace generator creates to straddle the guard's
// eligibility threshold. Matching on a prefix rather than an exact pair keeps the report from having to be
// edited every time a probe is added, and keeps the two files from disagreeing about a literal.
const thresholdProbePrefix = "standard-probe-"

// The threshold probes, defined here because the report keys on them and the trace generator builds them.
//
// They lived in cmd/benchharness first, and the test for them transcribed the numbers -- so setting both
// probes to the same length passed everything while silently removing the only traffic that touches the
// threshold. One definition, read by both sides, is what makes that mutation fail.
//
// The gateway scores (chars+3)/4, so these land one point either side of a 4096 threshold: 16,380 scores
// 4,095 and passes an engaged guard, 16,384 scores 4,096 and does not. Four characters apart.
const (
	ProbeUnderTenant = thresholdProbePrefix + "under"
	ProbeOverTenant  = thresholdProbePrefix + "over"
	ProbeUnderChars  = 16_380
	ProbeOverChars   = 16_384
)

// isThresholdProbe reports whether tenant is one of the threshold probes.
func isThresholdProbe(tenant string) bool { return strings.HasPrefix(tenant, thresholdProbePrefix) }

// shedByAdmission reports whether the gateway refused this request as an admission decision.
//
// It reads the status rather than RawRow.ErrorKind because the replay client labelled only 429 "rejected" and
// left 413 under the generic "http", so evidence already on disk carries the wrong label and the status is the
// only field that was right at the time. Scoring off the status lets a finished paid run be re-scored.
//
// Counting only 429 made the arm that shed hardest the one that reported shedding nothing: a static-cap replay
// refused 447 noisy requests with 413 and printed rejected=0, having filed all 447 under Failed instead.
// neverEvaluated reports whether the gateway turned this request away before admission control ran.
//
// Authentication and authorisation are decided ahead of admission, so a request refused there carries no
// information about the guard: the threshold, the bucket, and the cap all never saw it.
// eligibleTier reports whether the tier the gateway recorded admits this row to the gated population.
func eligibleTier(r RawRow) bool {
	return r.Tier == "" || r.Tier == tierStandard
}

// tierStandard is the gateway's name for the tier its admission controls gate.
const tierStandard = "standard"

// admissionUnknown reports whether this request left no record of what the guard decided about it.
//
// A transport error or a timeout means no response arrived, so there are no headers to read and no status to
// classify. It is the admission-side twin of a censored latency observation, and it is treated the same way:
// removed from the measurement rather than guessed at, counted, and disqualifying past a threshold.
func admissionUnknown(r RawRow) bool {
	return r.HTTPStatus == 0 || r.ErrorKind == errKindTimeout
}

func neverEvaluated(r RawRow) bool {
	return r.HTTPStatus == httpStatusUnauthorized || r.HTTPStatus == httpStatusForbidden
}

func shedByAdmission(r RawRow) bool {
	return r.HTTPStatus == httpStatusTooManyRequests || r.HTTPStatus == httpStatusInputExceedsBurst
}

// ProbeOutcome is one probe tenant's admission tally, and the real token cost the estimate stood in for.
type ProbeOutcome struct {
	// Total is how many of this tenant's requests the arm sent.
	Total int
	// Rejected is how many the gateway refused on admission, whether it said 429 or 413.
	Rejected int
	// EstInputTokens is the score the gateway assigned, which is what the threshold is compared against.
	EstInputTokens int
	// Unevaluated is how many the gateway turned away before admission ran, so the threshold never judged them.
	//
	// A probe refused for its credentials is not a probe that passed the threshold, and the difference is the
	// difference between evidence and none. Without this the section printed rejected=0 for probes the guard
	// had never seen, which reads as the threshold having considered them and let them through.
	Unevaluated int
}

type ArmSummary struct {
	// Arm names the condition.
	Arm string
	// Total is every request the trace offered for this arm.
	Total int
	// Completed counts requests that produced a full response (a first token and an end).
	Completed int
	// OfferedExactTokens and AdmittedExactTokens are the admitted-work fraction in the units the design
	// actually specifies: the served tokenizer's own count, not ceil(chars/4).
	//
	// The estimate is not a neutral stand-in. This project's calibration measures it 36 percent low on a
	// 200-character prompt and 23 percent high on a 40,000-character one, so a fraction built from it weighs
	// the population differently than the criterion says to.
	OfferedExactTokens  int64
	AdmittedExactTokens int64
	// ExactTokensMissing counts eligible requests with no measured count, and ExactTokensContradicted counts
	// those whose engine-reported count disagrees with the trace's.
	//
	// Either one disqualifies the admission-match check. The alternative is to fall back to the estimate,
	// which is how the criterion came to be unevaluated for three paid runs without anyone noticing.
	ExactTokensMissing      int
	ExactTokensContradicted int
	// AdmissionLost is how many ELIGIBLE requests got no admission verdict at all, so the guard's behaviour
	// toward them is unknown.
	//
	// A request that never received an HTTP response was not admitted and was not refused. Counting it as
	// either invents an observation; counting it as admitted -- which is what happened -- let an arm whose
	// traffic died in transport report a perfect admission match on work it never offered.
	AdmissionLost int
	// eligibleScored is how many eligible requests did get a verdict; AdmissionLost is judged against it.
	eligibleScored int
	// Rejected counts admission refusals (HTTP 429 or 413), the load the guard or static cap shed.
	Rejected int
	// TimedOut counts requests recorded with a timeout error kind.
	TimedOut int
	// Failed counts other non-completing requests (transport or stream errors).
	Failed int
	// TTFTMsP50/P95/P99 are the time-to-first-token percentiles over COMPLETED requests, in ms.
	TTFTMsP50 float64
	TTFTMsP95 float64
	TTFTMsP99 float64
	// E2EMsP99 is the end-to-end p99 over completed requests, in ms.
	E2EMsP99 float64
	// OfferedInputTokens is the estimated input tokens of every eligible request the trace offered.
	//
	// AdmittedInputTokens is the estimated input tokens of the eligible requests that were NOT rejected.
	//
	// Their ratio is the admitted-work fraction the design uses to check that arm B is admission-matched to arm C.
	OfferedInputTokens  int64
	AdmittedInputTokens int64
	// ThresholdProbe records how the guard's eligibility threshold behaved, keyed by tenant.
	//
	// The rest of this summary cannot show it. Contender and premium traffic sit thousands of tokens either
	// side of the threshold, so their admission outcome is the same for any threshold in a wide range, and a
	// report built only from them describes a guard whose configured number never mattered. The probe
	// tenants straddle it by four characters; this is where their opposite outcomes become visible.
	ThresholdProbe map[string]ProbeOutcome

	// RepetitionCount and MinRepetitionTail describe the arm's repetitions rather than its pooled rows, and
	// they exist because pooling hides a truncated repetition.
	//
	// TailSampleSize is computed over every row in the arm. An arm whose repetitions completed 500, 500, 500
	// and 30 premium requests therefore reports 1,530 -- far above MinTailSamples -- while one quarter of the
	// evidence is a p99 taken over thirty requests, which is that repetition's MAXIMUM. That fourth value is
	// then resampled with equal weight by the incremental bootstrap.
	//
	// The floor is the same number and the same derivation as MinTailSamples, applied per repetition because
	// that is the unit the bootstrap resamples: below 100, nearest-rank p99 lands on the maximum.
	//
	// Zero means the caller did not supply them (Summarize cannot know how its rows were split), and the
	// check is then skipped rather than failing closed on absence -- the harness fills them and the unit
	// tests that build summaries by hand do not.
	RepetitionCount int

	// MinRepetitionTail is the smallest premium-completion count among the arm's repetitions.
	MinRepetitionTail int

	// Censored is true when more than 1% of the premium requests failed to complete for a non-admission reason -- a timeout OR a transport/stream error -- so the tail is a lower bound rather than an exact p99 (design spec: report it as at least the timeout, never drop it).
	Censored bool
	// TailSampleSize is the number of completed premium requests the tail percentiles were computed over.
	//
	// The pre-registered checks refuse a run whose compared arms have a zero-size tail, so a run that completed nothing cannot be certified as protection.
	TailSampleSize int
}

// eligibleLongThreshold mirrors the guard's standard-long threshold, so admitted-work is measured over the same population the guard and static cap actually gate.
const eligibleLongThreshold = 4096

// Summarize computes one arm's summary from its raw rows.
//
// The tail percentiles are the DESIGN'S PRIMARY ENDPOINT: premium (victim) TTFT p99, so they are computed only over the premium tenant's completed requests, never over the noisy contender whose long prefills would otherwise dominate the tail and let a guard flatter its number by shedding contender load.
//
// The overall completed/rejected/timed-out/failed counts still account for every offered request, so shedding load always shows up as a rejection count.
//
// The admitted-work fractions are measured over the eligible (standard-long) population, which is the contender the controls actually gate.
func Summarize(arm string, rows []RawRow) ArmSummary {
	s := ArmSummary{Arm: arm, Total: len(rows)}

	// The eligible-population threshold comes from the manifest provenance stamped into the rows, so admitted-work is scored over the same population the guard gated even if the paid run tuned it.
	threshold := eligibleLongThreshold
	if len(rows) > 0 && rows[0].LongThreshold > 0 {
		threshold = rows[0].LongThreshold
	}

	var ttft, e2e []float64
	var premiumTotal, premiumTimedOut, premiumLost int
	for _, r := range rows {
		// Admitted-work accounting covers the eligible population, for the admission-match check.
		//
		// The gateway gates on tier == standard AND EstInputTokens >= threshold, and this applies the same
		// rule -- against the tier the gateway itself reported, not one inferred from a tenant name.
		//
		// A row with no tier predates the gateway reporting it and is scored on the threshold alone, as it
		// always was. Treating "not recorded" as "not standard" would empty the eligible population of every
		// run already on disk and silently rewrite its numbers.
		//
		// A request the gateway turned away before admission ran is outside that population entirely, in
		// neither term of the fraction, because the guard never saw it. Counting it as offered-and-admitted
		// scored 737,280 admitted tokens for an arm that admitted nothing: the paid run's probes estimate at
		// exactly the 4,096 threshold, so all 180 of them per arm were eligible, and all 180 were 403.
		if r.EstInputTokens >= threshold && eligibleTier(r) && !neverEvaluated(r) {
			if admissionUnknown(r) {
				// Out of both terms. The guard may have admitted this request and the connection died after,
				// or it may never have arrived; the evidence cannot say which, and a fraction built on a
				// guess is worse than one that reports how much it could not see.
				s.AdmissionLost++
			} else {
				s.eligibleScored++
				s.OfferedInputTokens += int64(r.EstInputTokens)
				if !shedByAdmission(r) {
					s.AdmittedInputTokens += int64(r.EstInputTokens)
				}
				switch {
				case r.ExactInputTokens <= 0:
					s.ExactTokensMissing++
				case r.EngineInputTokens > 0 && r.EngineInputTokens != r.ExactInputTokens:
					s.ExactTokensContradicted++
				default:
					s.OfferedExactTokens += int64(r.ExactInputTokens)
					if !shedByAdmission(r) {
						s.AdmittedExactTokens += int64(r.ExactInputTokens)
					}
				}
			}
		}

		// The probe tally is keyed by tenant rather than by a flag on the row, because what makes a request a
		// probe is the prompt length the trace gave it, and the tenant name is where that choice is recorded.
		// Any tenant whose name marks it a probe is counted; the report does not need to know how many there
		// are, only that their outcomes are reported separately from the populations that cannot see the
		// threshold.
		if isThresholdProbe(r.Tenant) {
			if s.ThresholdProbe == nil {
				s.ThresholdProbe = map[string]ProbeOutcome{}
			}
			o := s.ThresholdProbe[r.Tenant]
			o.Total++
			o.EstInputTokens = r.EstInputTokens
			switch {
			case neverEvaluated(r):
				o.Unevaluated++
			case shedByAdmission(r):
				o.Rejected++
			}
			s.ThresholdProbe[r.Tenant] = o
		}

		// Overall outcome counts cover every offered request, so load shedding is always visible as a rejection.
		switch {
		case r.ErrorKind == errKindTimeout:
			s.TimedOut++
		case shedByAdmission(r):
			s.Rejected++
		case r.ErrorKind != "":
			s.Failed++
		default:
			if _, ok := r.TTFTNanos(); ok {
				s.Completed++
			} else {
				s.Failed++
			}
		}

		// The tail is premium-only; the contender's requests never enter the protected metric.
		if r.IsNoisy {
			continue
		}
		premiumTotal++
		if r.ErrorKind == errKindTimeout {
			premiumTimedOut++
			continue
		}
		if shedByAdmission(r) {
			continue
		}
		if r.ErrorKind != "" {
			// A transport or stream error is a LOST OBSERVATION, exactly like a timeout, and it was not
			// counted as one.
			//
			// The censoring rule read only premiumTimedOut, so a run whose premium requests died on
			// connection resets kept an uncensored tail. That is the failure mode of a node going away --
			// a reclaimed Spot instance, an evicted engine Pod, an OOM kill, a network blip -- and none of
			// them produce timeouts. An adversarial review demonstrated it by feeding this function an arm
			// truncated at 120 premium completions with 380 transport failures, and the report printed
			// "all checks passed".
			//
			// An admission refusal stays excluded because a rejection is the treatment, not a lost
			// measurement: shedding is what the guard is for and it is accounted separately in Rejected.
			// That holds for 413 exactly as it does for 429, and reading only 429 here would have censored
			// the tail of any arm whose premium traffic ran into the burst ceiling.
			premiumLost++
			continue
		}
		t, okT := r.TTFTNanos()
		e, okE := r.E2ENanos()
		if !okT || !okE {
			continue
		}
		ttft = append(ttft, nanosToMs(t))
		e2e = append(e2e, nanosToMs(e))
	}

	sort.Float64s(ttft)
	sort.Float64s(e2e)
	s.TailSampleSize = len(ttft)
	s.TTFTMsP50 = percentile(ttft, 0.50)
	s.TTFTMsP95 = percentile(ttft, 0.95)
	s.TTFTMsP99 = percentile(ttft, 0.99)
	s.E2EMsP99 = percentile(e2e, 0.99)

	// The tail is censored when more than 1% of the PREMIUM requests did not complete for a reason other than
	// admission, since those missing requests are exactly the ones a p99 should capture.
	//
	// Timeouts and transport/stream errors are both counted. They are the same thing for this purpose -- a
	// premium request whose latency is unknown and was probably long -- and separating them let a whole class
	// of degraded run through uncensored.
	// >= rather than >, because at exactly one percent the p99 is already gone.
	//
	// A nearest-rank p99 over n observations is the ceil(0.99n)-th, so it rests on the slowest n/100 of them.
	// Losing exactly that many -- 18 of 1,840, which the old boundary waved through -- can remove the entire
	// quantile mass the statistic is made of, and the ones that vanish are the ones that were slow enough to
	// die. The reported p99 then is a p98 wearing the other name.
	if premiumTotal > 0 && float64(premiumTimedOut+premiumLost)/float64(premiumTotal) >= 0.01 {
		s.Censored = true
	}
	return s
}

// nanosToMs converts a nanosecond duration to fractional milliseconds.
func nanosToMs(n int64) float64 { return float64(n) / 1e6 }

// percentile returns the q-quantile of a sorted slice using nearest-rank, or 0 for an empty slice.
//
// Nearest-rank (rather than interpolation) is used because a tail claim should land on an actually observed latency, not a value that never occurred.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	// Nearest-rank: the smallest index whose cumulative share reaches q.
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	rank = max(rank, 0)
	rank = min(rank, len(sorted)-1)
	return sorted[rank]
}

// CI is a two-sided confidence interval.
type CI struct {
	Lo float64
	Hi float64

	// Valid distinguishes "the interval is [0,0]" from "there is no interval".
	//
	// The zero CI used to be indistinguishable from a computed one, and the incremental gate reads
	// `incrementalCI.Hi < 1.0`. When repetition counts differ between arms the caller skips the bootstrap and
	// leaves the zero value, so Hi is 0.0 and the STRICTEST check in the design passes vacuously -- a
	// truncated run disarms the gate instead of tripping it. Making the zero value invalid by construction is
	// what stops that, rather than relying on every caller to remember.
	Valid bool

	// InvalidReason says WHY there is no usable interval, because the two causes call for different actions.
	//
	// The gate reported "unequal or insufficient repetitions" for every invalid interval, so a run refused
	// for scatter would have sent an operator to check repetition counts that were fine.
	InvalidReason string
}

// BootstrapCI returns a percentile-bootstrap confidence interval for the mean of values.
//
// The design requires a repetition/block-aware bootstrap rather than a naive bootstrap over pooled requests, so values here are the PER-REPETITION statistics (e.g. each repetition's TTFT p99), and this resamples whole repetitions with replacement.
//
// alpha is the two-sided error, so 0.05 yields a 95% interval; iterations sets the resample count; seed makes it reproducible.
func BootstrapCI(values []float64, iterations int, seed int64, alpha float64) CI {
	if len(values) == 0 {
		return CI{}
	}
	if len(values) == 1 {
		// A single repetition cannot bound its own variance, so the interval degenerates to the point estimate
		// -- and a point estimate is not a confidence interval. It is returned for display and marked INVALID,
		// because the incremental gate asks whether the interval's upper bound clears 1.0 and a degenerate
		// "interval" answers that question vacuously. This function said as much in its own comment while
		// returning a value the gate could not tell apart from a real one.
		return CI{Lo: values[0], Hi: values[0]}
	}

	src := rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15)
	rng := rand.New(src)

	means := make([]float64, iterations)
	n := len(values)
	for i := range iterations {
		var sum float64
		for range n {
			sum += values[rng.IntN(n)]
		}
		means[i] = sum / float64(n)
	}
	sort.Float64s(means)
	return CI{
		Lo:    percentile(means, alpha/2),
		Hi:    percentile(means, 1-alpha/2),
		Valid: true,
	}
}

// admittedWorkFraction is the eligible input tokens admitted over those offered, the design's admission-match quantity.
func admittedWorkFraction(s ArmSummary) float64 {
	if s.OfferedInputTokens == 0 {
		return 0
	}
	return float64(s.AdmittedInputTokens) / float64(s.OfferedInputTokens)
}

// Checks holds the pre-registered success criteria and whether each passed.
type Checks struct {
	// AbsoluteProtection is C premium TTFT p99 <= 1.25 x R1 premium TTFT p99.
	AbsoluteProtectionRatio float64
	AbsoluteProtectionPass  bool
	// IncrementalValue is C/B premium TTFT p99, which must be <= 0.90 with the CI upper bound below 1.0.
	IncrementalRatio     float64
	IncrementalRatioCI   CI
	IncrementalValuePass bool
	// AdmissionMatch is |wB - wC| / wC over admitted-work fractions, which must be within MatchTolerance.
	AdmissionMatchDelta float64
	AdmissionMatchPass  bool
	// Invalid marks a run whose evidence disqualifies the comparison before any check is read, so a degenerate run can never be certified as protection.
	//
	// InvalidReason names why, for the report.
	Invalid       bool
	InvalidReason string
	// OverallPass is true only when the run is valid and every check passed, which is the condition for using the word "protects".
	OverallPass bool
}

// invalidate records a reason a run cannot be certified, keeping every reason rather than the last.
//
// It used to assign, so a run broken three ways reported one problem and an operator fixed them one at a
// time -- paying for a run each round to discover the next.
func (c *Checks) invalidate(reason string) {
	c.Invalid = true
	if c.InvalidReason != "" {
		c.InvalidReason += "; "
	}
	c.InvalidReason += reason
}

// MaxRatioScatter is the per-repetition coefficient of variation past which the incremental interval stops
// meaning what it says.
//
// A percentile bootstrap over a handful of values is anti-conservative once those values spread out. Against
// this package's own BootstrapCI at four repetitions, a true ratio of 1.00 -- no effect at all -- clears the
// pre-registered gate 10.2 percent of the time at a coefficient of variation of 0.20, and 1.8 percent at
// 0.10, against a nominal 5. The bound sits between them.
//
// The 2026-09-03 pilot measured 0.001 for the contended arms and 0.056 for the isolation-like ones, so this
// is not expected to bind. It exists because the failure mode is a gate that PASSES when it should not, and
// a run is not entitled to assume its variability stayed where the pilot's was. Reproduce the numbers with
// "benchharness power".
const MaxRatioScatter = 0.15

// RatioScatterTooHigh reports whether per-repetition ratios are too scattered for their bootstrap interval
// to be read as a 95 percent bound.
//
// Fewer than two values have no scatter to measure, and their interval is already invalid for that reason.
func RatioScatterTooHigh(ratios []float64) bool {
	if len(ratios) < 2 {
		return false
	}
	mean := 0.0
	for _, r := range ratios {
		mean += r
	}
	mean /= float64(len(ratios))
	if mean <= 0 {
		return false
	}
	ss := 0.0
	for _, r := range ratios {
		ss += (r - mean) * (r - mean)
	}
	return math.Sqrt(ss/float64(len(ratios)-1))/mean > MaxRatioScatter
}

// MaxLostAdmissionFraction is the share of the eligible population whose admission verdict may go missing
// before the admitted-work fraction stops describing the population it claims to.
//
// The same one percent the tail uses, for the same reason: past it the statistic is reporting on requests it
// never saw.
const MaxLostAdmissionFraction = 0.01

// AdmissionScored is how many eligible requests did get a verdict, the denominator AdmissionLost is judged
// against.
func (s ArmSummary) AdmissionScored() int {
	return s.eligibleScored
}

// MinTailSamples is the smallest premium-completion count at which the reported p99 is not simply the
// largest observation.
//
// It is derived, not chosen. The percentile is nearest-rank: index ceil(0.99*n)-1, clamped to n-1. Solve
// ceil(0.99*n)-1 < n-1 and the smallest integer that satisfies it is 100 -- at n=99 the index lands on 98
// of 0..98, the maximum, and every value below 99 does the same. So a run with fewer than 100 premium
// completions reports its slowest request and calls it a tail.
//
// This mattered because nothing read TailSampleSize. It was computed, stored, asserted on in unit tests,
// and never consulted by any production path: not printed, not gating validity. A sixty-second run at a
// rate the engine cannot serve yields a few dozen completions, and the report would have presented that
// maximum with exactly the authority of a p99 over five thousand.
const MinTailSamples = 100

// EvaluateChecks computes the design's pre-registered checks from the four arms' aggregated p99 values and admission-work fractions.
//
// r1P99, offP99 are provenance context; the checks compare C against R1 (absolute) and against B (incremental). incrementalCI is the bootstrap CI of the C/B ratio, produced by the caller from per-repetition ratios.
func EvaluateChecks(r1, staticCap, kvAware ArmSummary, incrementalCI CI, matchTolerance float64) Checks {
	var c Checks

	// A comparison is disqualified before any check is read if a compared arm completed no premium requests or has a censored tail, since its p99 is then not a real tail.
	for _, s := range []ArmSummary{r1, staticCap, kvAware} {
		if s.TailSampleSize == 0 {
			c.invalidate(fmt.Sprintf("arm %s completed no premium requests, so its tail is undefined", s.Arm))
		}
		if s.TailSampleSize > 0 && s.TailSampleSize < MinTailSamples {
			c.invalidate(fmt.Sprintf("arm %s has %d premium completions, below the %d a nearest-rank p99 needs to be anything other than the maximum",
				s.Arm, s.TailSampleSize, MinTailSamples))
		}
		if s.RepetitionCount > 0 && s.MinRepetitionTail < MinTailSamples {
			c.invalidate(fmt.Sprintf("arm %s has a repetition with %d premium completions, below the %d a nearest-rank p99 needs; pooling its %d rows hides that one repetition's p99 is a maximum",
				s.Arm, s.MinRepetitionTail, MinTailSamples, s.TailSampleSize))
		}
		if s.Censored {
			c.invalidate(fmt.Sprintf("arm %s tail is censored (>1%% of premium requests did not complete), so its p99 is only a lower bound", s.Arm))
		}
		// The criterion is defined over exact tokens, so a population that cannot supply them cannot be
		// scored against it. Refusing is the point: falling back to the estimate is what made three paid
		// runs report a number nobody had asked for.
		if s.ExactTokensMissing > 0 {
			c.invalidate(fmt.Sprintf("arm %s has %d eligible requests with no measured input-token count, and the admission-match criterion is defined over the served tokenizer's own count rather than the ceil(chars/4) estimate", s.Arm, s.ExactTokensMissing))
		}
		if s.ExactTokensContradicted > 0 {
			c.invalidate(fmt.Sprintf("arm %s has %d eligible requests whose engine-reported input-token count disagrees with the trace's measurement, so the trace was stamped against a different tokenizer or a different prompt ran", s.Arm, s.ExactTokensContradicted))
		}
		// An eligible request with no admission verdict is unknown work, not admitted work, and the
		// admitted-work fraction is what the whole matched comparison rests on. The threshold is the tail's:
		// past one percent the fraction is describing a population it could not see.
		if eligible := s.AdmissionLost + s.AdmissionScored(); eligible > 0 &&
			float64(s.AdmissionLost)/float64(eligible) >= MaxLostAdmissionFraction {
			c.invalidate(fmt.Sprintf("arm %s lost the admission verdict for %d of %d eligible requests (>%.0f%%), so its admitted-work fraction is measured over a population it could not see",
				s.Arm, s.AdmissionLost, eligible, MaxLostAdmissionFraction*100))
		}
	}

	if r1.TTFTMsP99 > 0 {
		c.AbsoluteProtectionRatio = kvAware.TTFTMsP99 / r1.TTFTMsP99
		c.AbsoluteProtectionPass = c.AbsoluteProtectionRatio <= 1.25
	}

	if staticCap.TTFTMsP99 > 0 {
		c.IncrementalRatio = kvAware.TTFTMsP99 / staticCap.TTFTMsP99
	}
	c.IncrementalRatioCI = incrementalCI
	// An absent interval fails the gate rather than satisfying it. See the comment on CI.Valid.
	c.IncrementalValuePass = c.IncrementalRatio <= 0.90 && incrementalCI.Valid && incrementalCI.Hi < 1.0
	if !incrementalCI.Valid {
		why := incrementalCI.InvalidReason
		if why == "" {
			why = "unequal or insufficient repetitions"
		}
		c.invalidate("the incremental-value check has no usable confidence interval (" + why + "), so it cannot be evaluated")
	}

	wB := admittedWorkFraction(staticCap)
	wC := admittedWorkFraction(kvAware)
	if wC > 0 {
		c.AdmissionMatchDelta = math.Abs(wB-wC) / wC
		c.AdmissionMatchPass = c.AdmissionMatchDelta <= matchTolerance
	}

	c.OverallPass = !c.Invalid && c.AbsoluteProtectionPass && c.IncrementalValuePass && c.AdmissionMatchPass
	return c
}

// FormatReport renders the summaries and checks as a plain-text report.
//
// It states explicitly when the comparison is invalid (admission match missed) or the tail is censored, so a reader is never handed a clean-looking number that the methodology already disqualified.
func FormatReport(summaries []ArmSummary, checks Checks, matchTolerance float64) string {
	var b strings.Builder
	b.WriteString("M5-b benchmark report\n\n")
	// reps is printed for the same reason tailN is. The incremental interval below is a bootstrap over
	// REPETITIONS -- BootstrapCI resamples whole repetitions, so the repetition count IS the n of the only
	// interval this study reports. Without it on the page, a CI built from two blocks is typographically
	// indistinguishable from one built from four, and the two mean very different things: with two blocks
	// the nearest-rank 2.5/97.5 percentiles land on the smaller and larger of exactly two numbers, so the
	// printed interval is their range and not a 95% interval at all.
	//
	// A reader who cannot see the block count has no way to tell those apart, which is the same defect the
	// tailN column exists to prevent one level down.
	fmt.Fprintf(&b, "%-12s %8s %8s %8s %8s %8s %8s %8s %8s %8s\n", "arm", "total", "done", "shed", "timeout", "ttftP50", "ttftP95", "ttftP99", "tailN", "reps")
	for _, s := range summaries {
		censored := ""
		if s.Censored {
			censored = " (p99 censored: >1% timeouts, lower bound)"
		}
		// tailN is printed because the p99 beside it is meaningless without it, and because a reader who
		// cannot see the sample count has no way to tell a tail from a maximum.
		thin := ""
		if s.TailSampleSize > 0 && s.TailSampleSize < MinTailSamples {
			thin = fmt.Sprintf(" (p99 is the maximum: %d < %d premium completions)", s.TailSampleSize, MinTailSamples)
		}
		fmt.Fprintf(&b, "%-12s %8d %8d %8d %8d %8.1f %8.1f %8.1f %8d %8d%s%s\n",
			s.Arm, s.Total, s.Completed, s.Rejected, s.TimedOut, s.TTFTMsP50, s.TTFTMsP95, s.TTFTMsP99, s.TailSampleSize, s.RepetitionCount, censored, thin)
	}
	// The threshold's own evidence, printed before the checks because it qualifies them: the checks compare
	// arms, and this says whether the number those arms were configured with did any work at all.
	probed := false
	for _, sm := range summaries {
		if len(sm.ThresholdProbe) > 0 {
			probed = true
			break
		}
	}
	unevaluated := false
	if probed {
		b.WriteString("\nEligibility threshold (probe tenants, four characters apart)\n")
		for _, sm := range summaries {
			names := make([]string, 0, len(sm.ThresholdProbe))
			for n := range sm.ThresholdProbe {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				o := sm.ThresholdProbe[n]
				void := ""
				if o.Unevaluated > 0 {
					unevaluated = true
					void = fmt.Sprintf("  VOID: %d never reached admission (401/403)", o.Unevaluated)
				}
				fmt.Fprintf(&b, "  %-12s %-22s est=%d  sent=%d  rejected=%d%s\n", sm.Arm, n, o.EstInputTokens, o.Total, o.Rejected, void)
			}
		}
		if unevaluated {
			b.WriteString("  VOID: the gateway turned these probes away on credentials, so the threshold never judged them\n")
			b.WriteString("  and rejected=0 above is the absence of a measurement rather than the threshold letting them\n")
			b.WriteString("  through. This run does not evidence the configured threshold.\n")
		} else {
			b.WriteString("  the estimate is what the threshold compares; the measured real cost of these prompts is\n")
			b.WriteString("  about 3171 tokens, so a rejection here fires on an over-estimate of roughly 29 percent\n")
		}
	} else {
		b.WriteString("\nEligibility threshold: NOT TESTED -- no probe tenant straddled it, so any threshold in a wide\n")
		b.WriteString("  range would have produced these same arms. The configured value is not evidenced by this run.\n")
	}

	b.WriteString("\nPre-registered checks (primary endpoint: TTFT p99)\n")
	fmt.Fprintf(&b, "  absolute protection  C/R1 = %.3f  (<= 1.25)  %s\n", checks.AbsoluteProtectionRatio, pass(checks.AbsoluteProtectionPass))
	fmt.Fprintf(&b, "  incremental value    C/B  = %.3f  CI[%.3f, %.3f]  (<= 0.90, CI hi < 1.0)  %s\n",
		checks.IncrementalRatio, checks.IncrementalRatioCI.Lo, checks.IncrementalRatioCI.Hi, pass(checks.IncrementalValuePass))
	fmt.Fprintf(&b, "  admission match      |B-C|/C = %.3f  (<= %.3f)  %s\n", checks.AdmissionMatchDelta, matchTolerance, pass(checks.AdmissionMatchPass))
	if checks.Invalid {
		fmt.Fprintf(&b, "  RUN INVALID: %s\n", checks.InvalidReason)
	}
	b.WriteString("\n")
	switch {
	case checks.Invalid:
		b.WriteString("VERDICT: run invalid; no protection claim is made from this evidence.\n")
	case checks.OverallPass:
		b.WriteString("VERDICT: all checks passed; the guard protects the premium tenant's tail beyond load shedding.\n")
	default:
		b.WriteString("VERDICT: not all checks passed; the word \"protects\" is not used for this run.\n")
	}
	return b.String()
}

// pass renders a boolean as a short marker for the report.
func pass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
