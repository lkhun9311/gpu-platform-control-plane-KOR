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
	"slices"
	"sort"
	"strings"
)

// The capacity ladder's readings, from
// docs/superpowers/specs/2026-09-13-what-the-split-costs-in-throughput.md.
//
// WHY THIS FILE EXISTS
//
// The sharing matrix answered whether splitting a card protects the premium tail. It could not answer what
// the split costs in capacity, and the reason was structural rather than a shortage of evidence: every arm
// was offered the same fixed trace and every arm finished all of it, so the delivered work was identical by
// construction and only the elapsed-time denominator moved. No re-reading of that evidence produces a
// capacity number. A ladder of offered loads does, and this file is its instrument.
//
// It shares PoPReading with the other two studies for the same reason they share it with each other: three
// studies printing through one formatter is one place for "fired", "not evaluable" and "detail" to mean the
// same thing.
//
// WHAT MAKES THIS LADDER CHEAP, AND WHY THAT IS SAFE
//
// The criterion below is an ABSOLUTE millisecond figure rather than a multiple of a baseline measured in
// the same session. That is what lets the ladder buy one R1 cell instead of one per rung. It is safe only
// because the number was fixed before any rung ran -- see ladderTTFTTargetMs -- and it stops being safe the
// moment someone recomputes it from this ladder's own evidence.

const (
	// ladderTTFTTargetMs is the registered premium latency target, in milliseconds.
	//
	// It is 2.00x the sharing matrix's pooled R1 premium TTFT p99 of 69.524 ms, which two repetitions of the
	// same trace agreed on to within 0.179 ms on an arm whose premium concurrency peaked at 26 against 64
	// sequence slots -- a baseline nowhere near its own limits.
	//
	// Written as an absolute number ON PURPOSE. A threshold recomputed per rung from a baseline measured
	// beside it would move with whatever that session's card was doing, and a ladder whose target moves is a
	// ladder that cannot bracket anything. It is also written here, in code, before any rung was bought, so
	// that it cannot be chosen to sit above or below what the rungs produced.
	ladderTTFTTargetMs = 139.0
	// ladderRepeatMargin is how close to the target a rung may land before it has to be repeated.
	//
	// Within a tenth of the target, the difference between meeting and breaching is inside the range an
	// instrument with this much repeat noise could produce twice. The sharing matrix measured that noise:
	// 1.375 ms on the control and 0.179 ms on R1, across repetitions of the same trace. The rule is
	// registered rather than applied where it happens to help, which is why it lives in the result as a
	// demand for a repetition rather than as a verdict.
	ladderRepeatMargin = 0.10
	// ladderContenderOffers is the contender load every rung holds fixed, in requests per 505-second window.
	//
	// Holding the contender in ABSOLUTE terms is the whole design. The trace generator takes a total rate and
	// per-tenant weights, so raising the premium rate while keeping a weight raises both tenants together --
	// which is the mistake that made the seventh pilot unscorable, and it changed two things at once while
	// reporting one.
	ladderContenderOffers = 139
	// ladderContenderTolerance is how far a rung's realised contender count may fall from that.
	//
	// The generator is stochastic at a fixed seed, and a search over its parameter space found an exact 139
	// at two rungs and 139 and 141 at the others. The tolerance is recorded here because it was measured
	// offline before the run rather than discovered in the evidence afterwards.
	ladderContenderTolerance = 2
	// ladderMinTailSamples is the thinnest premium tail a rung's p99 may rest on.
	//
	// A nearest-rank p99 over n observations is the ceil(0.99n)-th. Below this, one slow request moves it.
	ladderMinTailSamples = 500
)

// LadderCell is one topology at one rung: the unit the criterion is applied to.
type LadderCell struct {
	// Rung is 1-based and indexes the pre-registration's rung table.
	Rung int
	// Topology is ArmShared, ArmTimeSlicing or ArmR1.
	Topology string
	// Arm is the name the evidence carries, which is what a reader sees in the report's arm column.
	Arm string
	// TTFTMsP99 is the premium tail this cell produced.
	TTFTMsP99 float64
	// Censored says more than one percent of premium requests did not complete for a reason other than
	// admission, which is the second half of the criterion.
	Censored bool
	// ContenderOffers is what the contending tenant was actually offered here.
	//
	// Recorded per cell rather than assumed from the rung table, because the rung table describes what the
	// runner was ASKED to generate and this is what arrived.
	ContenderOffers int
	// TailSamples is how many completed premium requests the p99 rests on.
	TailSamples int
	// Met says this cell satisfies the registered criterion.
	Met bool
	// NearTarget says the cell landed within ladderRepeatMargin of the target either way, so repeat noise
	// could decide it and the pre-registration demands that rung be repeated.
	NearTarget bool
	// Invalid marks a cell whose evidence cannot be scored at all, with the reason.
	Invalid       bool
	InvalidReason string
}

// LadderResult is every cell, the readings in registered order, and the one that answered.
type LadderResult struct {
	// Study is the identifier the evidence carries, so the report names the experiment it actually read.
	//
	// It was a literal in the formatter, and the second ladder -- same arms, same criterion, rungs in a
	// different place -- printed the FIRST ladder's id over its own numbers. A report that names the wrong
	// experiment is the cheapest possible way to file a result under the wrong question.
	Study string
	// Cells are in rung then topology order, which is the order the ladder ran them.
	Cells []LadderCell
	// Readings are evaluated in the registered order and the first that fires is the answer.
	Readings []PoPReading
	// Answer is the ID of the reading that fired, or empty when none did.
	Answer string
	// BaselineMissing says no isolated-baseline cell was measured at the highest rung this evidence carries.
	//
	// It is NOT a reading, and that is the point. A verdict asked between rungs is provisional and the
	// baseline is bought last, so demanding one there would stop every ladder at its first rung. A FINAL
	// report is different: without the baseline, "the split ran out of capacity" and "one engine of this
	// model on this card ran out of capacity" are the same observation, and the report used to say "no
	// isolated-baseline cell breached the target" when none had been measured at all.
	BaselineMissing bool
	BaselineNote    string
	// RepeatRequired lists the rungs that landed close enough to the target to need a repetition.
	//
	// It is not a reading. A rung needing a repetition does not change what the other rungs say, and
	// folding it into the answer would let one borderline cell suppress a result the rest of the ladder
	// established.
	RepeatRequired []int
}

// EvaluateThroughputLadder applies the registered criterion to every cell and then reads the ladder.
//
// summaries is every arm the report loaded, ladder arms and anything else; arms this study does not name are
// ignored rather than refused, because a reader pointing the report at a directory holding two studies'
// evidence should get this study's answer and not an error about the other one's files.
func EvaluateThroughputLadder(summaries []ArmSummary) LadderResult {
	var res LadderResult
	for _, s := range summaries {
		rung, topology, ok := parseLadderArmName(s.Arm)
		if !ok {
			continue
		}
		res.Cells = append(res.Cells, scoreLadderCell(rung, topology, s))
	}
	sort.Slice(res.Cells, func(i, j int) bool {
		if res.Cells[i].Rung != res.Cells[j].Rung {
			return res.Cells[i].Rung < res.Cells[j].Rung
		}
		return res.Cells[i].Arm < res.Cells[j].Arm
	})
	for _, c := range res.Cells {
		if c.NearTarget && !slices.Contains(res.RepeatRequired, c.Rung) {
			res.RepeatRequired = append(res.RepeatRequired, c.Rung)
		}
	}
	// The highest rung that carries a CONTENDED cell is the rung the ladder ended on, and that is where the
	// baseline belongs. Derived from the evidence rather than from the runner's plan, because this is the
	// check that catches a plan and an output disagreeing.
	top := 0
	for _, c := range res.Cells {
		if c.Topology != ArmR1 && c.Rung > top {
			top = c.Rung
		}
	}
	if top > 0 && findCell(res.Cells, top, ArmR1) == nil {
		res.BaselineMissing = true
		res.BaselineNote = fmt.Sprintf("no isolated-baseline cell was measured at rung %d, the highest rung in this evidence. Without it a breach at that rung cannot be attributed to the topology rather than to this model on this card",
			top)
	}

	// The order is the pre-registration's and the first that fires is the answer.
	//
	// L0 comes first for the reason reading 4 does in the sharing matrix: a cell the evidence cannot score is
	// not a cell that breached, and letting an unscorable rung count as a breach would locate a limit the run
	// never found. L4 comes next because it qualifies everything under it -- a baseline that also breached
	// says the ladder found this model's limit on this card rather than either topology's.
	for _, r := range []PoPReading{
		ladderReadingInvalid(res.Cells),
		ladderReadingBaselineBreached(res.Cells),
		ladderReadingNoQualifiedPoint(res.Cells),
		ladderReadingExhausted(res.Cells),
		ladderReadingSeparation(res.Cells),
		ladderReadingSameRung(res.Cells),
	} {
		res.Readings = append(res.Readings, r)
		if r.Fired {
			res.Answer = r.ID
			break
		}
	}
	return res
}

// scoreLadderCell applies the criterion to one arm summary.
//
// Every refusal here returns Invalid rather than Met=false. The difference is the one this package keeps
// insisting on: a cell that could not be scored and a cell that breached are different facts, and only the
// second locates a capacity boundary.
func scoreLadderCell(rung int, topology string, s ArmSummary) LadderCell {
	c := LadderCell{
		Rung:        rung,
		Topology:    topology,
		Arm:         s.Arm,
		TTFTMsP99:   s.TTFTMsP99,
		Censored:    s.Censored || s.AnyRepetitionCensored,
		TailSamples: s.TailSampleSize,
	}
	if d, ok := s.DispositionByTenant[NoisyTenant]; ok {
		c.ContenderOffers = d.Offered
	}

	// The order below is the order the questions have to be asked in, and it was wrong twice.
	//
	// WORKLOAD INTEGRITY FIRST. A cell that offered a different amount of contention, or that pooled two
	// replays, is not a measurement of this rung at all -- whatever its p99 says.
	//
	// THEN CENSORING, because censoring at or above one percent FAILS THE REGISTERED CRITERION and is
	// therefore a BREACH, not a refusal. It used to be reached after the tail-sample floor, so an arm that
	// lost a fifth of its premium requests to timeouts -- which is what breaching looks like at a high
	// offered rate -- came back INVALID for having too few completions left to estimate a p99 from. The
	// instrument refused to score the clearest failure it can produce.
	//
	// THEN the tail floor, which is about whether an UNCENSORED p99 rests on enough observations.
	switch {
	case s.RepetitionCount > 1:
		// The registered combination rule says the runs are NOT pooled: a pooled p99 can read "met" while one
		// replay breached, and a pooled contender count reads 278 where each replay offered 139. This package
		// cannot honour that rule by averaging, so it refuses instead, and the rule is executed by reading
		// each run's own report.
		c.Invalid = true
		c.InvalidReason = fmt.Sprintf("this cell pools %d replays into one summary. The ladder's combination rule compares runs side by side and never pools them, because a pooled p99 can read met while one replay breached -- report each run separately",
			s.RepetitionCount)
	case topology != ArmR1 && offBy(c.ContenderOffers, ladderContenderOffers) > ladderContenderTolerance:
		// R1 is exempt because it carries no contender by design, which is the same exemption the sharing
		// matrix's contender floor makes for it.
		c.Invalid = true
		c.InvalidReason = fmt.Sprintf("the contender was offered %d requests where every rung holds it at %d +/- %d, so this rung varied two things at once",
			c.ContenderOffers, ladderContenderOffers, ladderContenderTolerance)
	case c.Censored:
		// A BREACH with a reason, not a refusal. Met stays false and NearTarget stays false: a p99 over the
		// requests that survived is not a number this cell gets to be judged close to the target on.
		c.Met = false
	case s.TTFTMsP99 <= 0:
		c.Invalid = true
		c.InvalidReason = "no premium tail was recorded, so this rung has no p99 to compare against the target"
	case s.TailSampleSize < ladderMinTailSamples:
		c.Invalid = true
		c.InvalidReason = fmt.Sprintf("the premium tail rests on %d completed requests, below the registered floor of %d",
			s.TailSampleSize, ladderMinTailSamples)
	default:
		c.Met = s.TTFTMsP99 <= ladderTTFTTargetMs
		c.NearTarget = s.TTFTMsP99 >= ladderTTFTTargetMs*(1-ladderRepeatMargin) &&
			s.TTFTMsP99 <= ladderTTFTTargetMs*(1+ladderRepeatMargin)
	}
	return c
}

// ladderReadingInvalid is the refusal: a scored ladder needs both contended arms at every rung it reports.
func ladderReadingInvalid(cells []LadderCell) PoPReading {
	r := PoPReading{ID: "L0", Name: "INVALID -- the ladder cannot be read as it stands"}
	if len(cells) == 0 {
		r.Fired = true
		r.Detail = "no ladder arms are present in this evidence"
		return r
	}
	var bad []string
	for _, c := range cells {
		if c.Invalid {
			bad = append(bad, fmt.Sprintf("%s: %s", c.Arm, c.InvalidReason))
		}
	}
	// A rung with one contended arm cannot be compared, and a ladder that silently skipped the pair would
	// report a separation between an arm that ran and an arm that did not.
	for _, rung := range rungsPresent(cells) {
		for _, topology := range []string{ArmShared, ArmTimeSlicing} {
			if findCell(cells, rung, topology) == nil {
				bad = append(bad, fmt.Sprintf("rung %d has no %s cell, so its two topologies cannot be compared", rung, topology))
			}
		}
	}
	if len(bad) > 0 {
		r.Fired = true
		r.Detail = strings.Join(bad, "; ")
	}
	return r
}

// ladderReadingBaselineBreached is the qualifier: an R1 cell that also breached says whose limit was found.
func ladderReadingBaselineBreached(cells []LadderCell) PoPReading {
	r := PoPReading{ID: "L4", Name: "the ladder found a single engine's limit, not a topology's"}
	var worst *LadderCell
	for i := range cells {
		if cells[i].Topology == ArmR1 && !cells[i].Met && (worst == nil || cells[i].Rung > worst.Rung) {
			worst = &cells[i]
		}
	}
	if worst == nil {
		r.Detail = "no isolated-baseline cell breached the target"
		return r
	}
	r.Fired = true
	r.Cell = worst.Arm
	r.Detail = fmt.Sprintf("the isolated baseline itself missed the %.1f ms target at rung %d (%.1f ms%s), so what this rung located is this model's limit on this card and not either topology's; the bracket the lower rungs established still stands and lower rungs are what is missing",
		ladderTTFTTargetMs, worst.Rung, worst.TTFTMsP99, censoredNote(worst.Censored))
	return r
}

// ladderReadingNoQualifiedPoint fires when nothing met the criterion anywhere, including the bottom rung.
func ladderReadingNoQualifiedPoint(cells []LadderCell) PoPReading {
	r := PoPReading{ID: "L5", Name: "no qualified operating point at or above the bottom rung"}
	lowest := lowestRung(cells)
	if lowest == 0 {
		r.NotEvaluable = true
		r.Detail = "no rung is present"
		return r
	}
	for _, c := range cells {
		if c.Topology != ArmR1 && c.Met {
			r.Detail = fmt.Sprintf("%s met the target, so the ladder found at least one qualified point", c.Arm)
			return r
		}
	}
	r.Fired = true
	r.Detail = fmt.Sprintf("neither topology met the %.1f ms target at rung %d, the lowest rung this ladder offered. The ladder searched UPWARD, so this says the qualified rate is below where it started and NOT that capacity is zero; searching downward is a separate purchase",
		ladderTTFTTargetMs, lowest)
	return r
}

// ladderReadingExhausted fires when both topologies were still meeting the target at the top rung.
func ladderReadingExhausted(cells []LadderCell) PoPReading {
	r := PoPReading{ID: "L6", Name: "a lower bound only -- neither topology was brought to its limit"}
	top := highestRung(cells)
	if top == 0 {
		r.NotEvaluable = true
		r.Detail = "no rung is present"
		return r
	}
	shared, split := findCell(cells, top, ArmShared), findCell(cells, top, ArmTimeSlicing)
	if shared == nil || split == nil || !shared.Met || !split.Met {
		r.Detail = fmt.Sprintf("at rung %d at least one topology breached the target, so a boundary was located", top)
		return r
	}
	r.Fired = true
	r.Detail = fmt.Sprintf("both topologies still met the %.1f ms target at rung %d, the highest this ladder offered (%s %.1f ms, %s %.1f ms). The result is a lower bound on both and is NOT extrapolated to a limit neither run located",
		ladderTTFTTargetMs, top, shared.Arm, shared.TTFTMsP99, split.Arm, split.TTFTMsP99)
	return r
}

// ladderReadingSeparation fires when the two topologies sustain different rungs, in either direction.
func ladderReadingSeparation(cells []LadderCell) PoPReading {
	r := PoPReading{ID: "L1", Name: "the topologies sustain different loads"}
	sharedTop, sharedOK := highestMetRung(cells, ArmShared)
	splitTop, splitOK := highestMetRung(cells, ArmTimeSlicing)
	if !sharedOK && !splitOK {
		r.NotEvaluable = true
		r.Detail = "neither topology met the target at any rung, which reading L5 is about"
		return r
	}
	if sharedTop == splitTop {
		r.Detail = fmt.Sprintf("both topologies last met the target at rung %d", sharedTop)
		return r
	}
	r.Fired = true
	if splitTop > sharedTop {
		r.Cell = ThroughputLadderArm(splitTop, ArmTimeSlicing)
		r.Detail = fmt.Sprintf("the split sustained the %.1f ms target %s and the whole-card control %s, so the split buys headroom as well as a better tail",
			ladderTTFTTargetMs, rungPhrase(splitTop, splitOK), rungPhrase(sharedTop, sharedOK))
		return r
	}
	r.Cell = ThroughputLadderArm(sharedTop, ArmShared)
	r.Detail = fmt.Sprintf("the whole-card control sustained the %.1f ms target %s and the split %s, so the split COSTS capacity and the tail improvement the sharing matrix measured was bought out of headroom -- the platform recommendation reverses",
		ladderTTFTTargetMs, rungPhrase(sharedTop, sharedOK), rungPhrase(splitTop, splitOK))
	return r
}

// ladderReadingSameRung is what is left: both topologies breached at the same rung.
func ladderReadingSameRung(cells []LadderCell) PoPReading {
	r := PoPReading{ID: "L2", Name: "a latency-distribution trade at a capacity the ladder could not separate"}
	sharedTop, sharedOK := highestMetRung(cells, ArmShared)
	splitTop, splitOK := highestMetRung(cells, ArmTimeSlicing)
	if !sharedOK || !splitOK || sharedTop != splitTop {
		r.NotEvaluable = true
		r.Detail = "the topologies did not last meet the target at the same rung, which the readings above are about"
		return r
	}
	r.Fired = true
	r.Detail = fmt.Sprintf("both topologies last met the %.1f ms target at rung %d and both breached at the next. The ladder's rungs could not separate their capacities, which is NOT a measurement that the capacities are equal",
		ladderTTFTTargetMs, sharedTop)
	return r
}

// parseLadderArmName reads "rung02-shared" back into its rung and topology.
//
// It lives here with the readings and is used from study.go's IsIsolatedBaseline, which is the one thing
// outside this file that has to know a ladder arm when it sees one.
// rungPhrase says where a topology's bracket closed, in words rather than in a rung number.
//
// A topology that never met the target has no rung, and printing "rung 0" for it reads as a rung the ladder
// measured. The second ladder produced exactly that: the whole-card control met at no rung and the reading
// said it "sustained the target to rung 0", which is a sentence about a cell that does not exist.
func rungPhrase(rung int, met bool) string {
	if !met {
		return "at no rung this ladder offered"
	}
	return fmt.Sprintf("to rung %d", rung)
}

func parseLadderArmName(arm string) (int, string, bool) {
	for rung := 1; rung <= throughputLadderRungs; rung++ {
		for _, topology := range []string{ArmShared, ArmTimeSlicing, ArmR1} {
			if arm == ThroughputLadderArm(rung, topology) {
				return rung, topology, true
			}
		}
	}
	return 0, "", false
}

func findCell(cells []LadderCell, rung int, topology string) *LadderCell {
	for i := range cells {
		if cells[i].Rung == rung && cells[i].Topology == topology {
			return &cells[i]
		}
	}
	return nil
}

// highestMetRung is the top rung at which a topology met the criterion.
//
// It is the HIGHEST met rather than the rung below the lowest breach, and the difference matters when a
// ladder has a hole in it: reporting the highest met describes what was observed, where inferring from the
// lowest breach would assume every rung between them behaves monotonically -- which this ladder measures
// rather than assumes.
func highestMetRung(cells []LadderCell, topology string) (int, bool) {
	top, ok := 0, false
	for _, c := range cells {
		if c.Topology == topology && c.Met && c.Rung > top {
			top, ok = c.Rung, true
		}
	}
	return top, ok
}

func rungsPresent(cells []LadderCell) []int {
	var out []int
	for _, c := range cells {
		if !slices.Contains(out, c.Rung) {
			out = append(out, c.Rung)
		}
	}
	sort.Ints(out)
	return out
}

func lowestRung(cells []LadderCell) int {
	r := rungsPresent(cells)
	if len(r) == 0 {
		return 0
	}
	return r[0]
}

func highestRung(cells []LadderCell) int {
	r := rungsPresent(cells)
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1]
}

func offBy(got, want int) int {
	if got > want {
		return got - want
	}
	return want - got
}

func censoredNote(censored bool) string {
	if censored {
		return ", and its premium tail is censored"
	}
	return ""
}

// LadderPlanRefusal says why a planned cell could never be scored, before it is bought.
//
// # WHY THIS IS HERE AND NOT IN THE RUNNER
//
// The floors it applies -- the contender's fixed count and its tolerance, and the minimum premium tail --
// are this file's, and a shell script holding its own copy of them is a second place for a registered
// number to be edited. The runner generates each planned trace locally, counts what came out, and asks
// this function; nothing about the thresholds crosses into the shell.
//
// # WHY IT EXISTS AT ALL
//
// Every one of these refusals already existed and every one of them fired ON THE RENTED CARD: `replay`
// validates the arm against the study after the engines are up, and the cell floors are applied by the
// readings after the replay. A mistyped study, a rung the registry does not admit, or a trace whose counts
// the readings would refuse therefore cost a bring-up each time. The same questions asked before launch
// cost nothing.
func LadderPlanRefusal(study, arm string, premiumOffers, contenderOffers int) error {
	s, ok := LookupStudy(study)
	if !ok {
		return fmt.Errorf("study %q is not registered; known studies are %s", study, strings.Join(KnownStudyIDs(), ", "))
	}
	if !s.Admits(arm) {
		return fmt.Errorf("arm %q is not one of study %s's arms (%s)", arm, s.ID, strings.Join(s.Arms, ", "))
	}
	if premiumOffers < ladderMinTailSamples {
		return fmt.Errorf("the trace offers %d premium requests and the readings refuse a premium tail below %d completed, so this cell could not be scored even if every request succeeded",
			premiumOffers, ladderMinTailSamples)
	}
	if IsIsolatedBaseline(arm) {
		if contenderOffers != 0 {
			return fmt.Errorf("the isolated baseline's trace carries %d contender requests and must carry none", contenderOffers)
		}
		return nil
	}
	if offBy(contenderOffers, ladderContenderOffers) > ladderContenderTolerance {
		return fmt.Errorf("the trace offers %d contender requests where every rung holds it at %d +/- %d, so this cell would be refused as having varied two things at once",
			contenderOffers, ladderContenderOffers, ladderContenderTolerance)
	}
	return nil
}

// LadderRungComplete says whether a rung has both contended topologies present and scorable.
//
// It exists so that a caller can tell "this rung is not here" apart from "this rung stopped the ladder".
// LadderShouldContinue answers false for both, and a boolean cannot carry the difference -- which is how a
// request for a rung that was never measured came back as a registered STOP.
func LadderRungComplete(res LadderResult, rung int) bool {
	shared, split := findCell(res.Cells, rung, ArmShared), findCell(res.Cells, rung, ArmTimeSlicing)
	return shared != nil && split != nil && !shared.Invalid && !split.Invalid
}

// LadderShouldContinue is the registered stopping rule, applied to the rung just measured.
//
// "Stop at the first rung where BOTH topologies have breached" -- one that still meets the target has not
// been bracketed, and stopping there would report a bound for an arm the ladder never pushed. It is here
// rather than in the runner because the threshold it compares against is here: a shell script holding its
// own copy of 139.0 ms is a second place for the criterion to be edited, and the pre-registration's whole
// claim is that the criterion was fixed before the rungs ran.
//
// The rung must be complete. A caller asking about a rung whose cells are not both present gets a refusal
// to continue rather than a decision, because "this rung has no split cell" and "the split met the target"
// are different facts and only one of them is a reason to climb.
func LadderShouldContinue(res LadderResult, rung int) (bool, string) {
	shared, split := findCell(res.Cells, rung, ArmShared), findCell(res.Cells, rung, ArmTimeSlicing)
	if shared == nil || split == nil {
		return false, fmt.Sprintf("rung %d is incomplete, so the stopping rule has nothing to apply", rung)
	}
	if shared.Invalid || split.Invalid {
		return false, fmt.Sprintf("rung %d has a cell that cannot be scored, so climbing further would build on it", rung)
	}
	if shared.Met || split.Met {
		var still []string
		if shared.Met {
			still = append(still, shared.Arm)
		}
		if split.Met {
			still = append(still, split.Arm)
		}
		return true, fmt.Sprintf("rung %d: %s still met the %.1f ms target, so neither bracket is closed yet",
			rung, strings.Join(still, " and "), ladderTTFTTargetMs)
	}
	return false, fmt.Sprintf("rung %d: both topologies breached the %.1f ms target (%s %.1f ms, %s %.1f ms), which is the registered stopping point",
		rung, ladderTTFTTargetMs, shared.Arm, shared.TTFTMsP99, split.Arm, split.TTFTMsP99)
}

// FormatThroughputLadder renders the cells and the readings beneath the report's arm table.
func FormatThroughputLadder(res LadderResult) string {
	var b strings.Builder
	study := res.Study
	if study == "" {
		study = StudyThroughputLadder
	}
	fmt.Fprintf(&b, "\nCAPACITY LADDER (%s)\n", study)
	fmt.Fprintf(&b, "  criterion: premium TTFT p99 <= %.1f ms and premium censoring under 1%%\n", ladderTTFTTargetMs)
	fmt.Fprintf(&b, "  %-22s %12s %8s %10s %9s  %s\n", "cell", "TTFT p99", "tail n", "contender", "verdict", "note")
	for _, c := range res.Cells {
		verdict := "BREACH"
		switch {
		case c.Invalid:
			verdict = "INVALID"
		case c.Met:
			verdict = "met"
		}
		note := ""
		switch {
		case c.Invalid:
			note = c.InvalidReason
		case c.NearTarget:
			note = "within a tenth of the target -- this rung must be repeated"
		case c.Censored:
			note = "premium tail censored"
		}
		fmt.Fprintf(&b, "  %-22s %9.1f ms %8d %10d %9s  %s\n",
			c.Arm, c.TTFTMsP99, c.TailSamples, c.ContenderOffers, verdict, note)
	}
	if len(res.RepeatRequired) > 0 {
		fmt.Fprintf(&b, "  rungs the pre-registration requires a repetition of: %v\n", res.RepeatRequired)
	}
	if res.BaselineMissing {
		fmt.Fprintf(&b, "  INVALID: %s\n", res.BaselineNote)
	}
	b.WriteString("\n  readings, in the registered order:\n")
	for _, r := range res.Readings {
		state := "did not fire"
		switch {
		case r.Fired:
			state = readingFired
		case r.NotEvaluable:
			state = "not evaluable"
		}
		fmt.Fprintf(&b, "  %-3s %-62s %s\n", r.ID, r.Name, state)
		if r.Detail != "" {
			fmt.Fprintf(&b, "      %s\n", r.Detail)
		}
	}
	if res.Answer == "" {
		b.WriteString("\n  ANSWER: none -- no registered reading fired, which is itself a result this page did not anticipate\n")
	} else {
		fmt.Fprintf(&b, "\n  ANSWER: %s\n", res.Answer)
	}
	return b.String()
}
