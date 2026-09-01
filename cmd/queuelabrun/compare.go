package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// comparisonSchemaVersion is versioned for the same reason the run record's is: a consumer classifies on the
// fields below, and a reworded or re-derived field would silently become a different claim.
const comparisonSchemaVersion = 1

// comparison is what a SET of records supports about the difference between their arms.
//
// It exists because the runs were reproducible and the conclusion was not. Every record in ex/ carries its
// own verdict, its own ledger and enough to re-derive its own numbers; the sentence those records were
// gathered to support — that honouring SIGTERM discards work and ignoring it defeats the reclaim — lived in
// a markdown file and in ad-hoc arithmetic typed at a shell. Nobody could reproduce the CONCLUSION from the
// artifacts, only the runs, and a lab whose conclusion is not an artifact is a lab that publishes prose.
//
// What it deliberately does NOT do is inference. There is no p-value here, no interval, and no threshold on
// how many runs make a difference real, because this build has no basis for one: the runs are not sampled
// from anything and their variance has never been characterised. It answers exactly three questions, each of
// which the records can actually settle — is the difference bigger than what the instrument could see, is
// the arm confounded with the order the runs were taken in, and how many runs are behind each arm — and it
// says "not resolved" whenever the first answer is no. Returning "not resolved" is its main job, not its
// failure mode.
type comparison struct {
	SchemaVersion int `json:"schemaVersion"`
	// Dose is the regime every contributing record shared. Records from different regimes measure different
	// quantities, so a comparison across them is refused rather than reported.
	Dose string `json:"dose"`
	// Arms summarises each arm's contributing runs.
	Arms []armSummary `json:"arms"`
	// WorstRunFloorSeconds is the coarsest single-run bound among the contributors, reported for orientation.
	//
	// It is NOT what any finding is tested against, and the distinction cost a review to notice. A finding
	// compares two arms, so it carries both arms' errors and is tested against the SUM of their floors --
	// see armSummary.FloorSeconds and differenceFloor. Publishing this number as though it governed the
	// findings understates their error budget by up to a factor of two.
	WorstRunFloorSeconds float64 `json:"worstRunFloorSeconds"`
	// Bounded is false when any contributing run bounded nothing, in which case nothing here is resolved.
	// The zero value is the safe one: an unbounded comparison resolves nothing.
	Bounded bool `json:"bounded"`
	// Interleaved is whether the arms alternate in the order the runs were actually taken.
	//
	// When false, every difference below is confounded with time: node warming, image cache state and any
	// drift in the cluster all move together with the arm, and nothing in the numbers can separate them.
	Interleaved bool `json:"interleaved"`
	// DeviceEvidence is the weakest axis among the contributing runs, because a comparison can establish
	// device work only if every run behind it did. One run that observed nothing makes the whole document a
	// statement about seconds of RESERVATION, and stating the strongest contributor's value instead would
	// launder that run's silence through its neighbours.
	DeviceEvidence string `json:"deviceEvidence"`
	// Findings is one entry per quantity and arm pair.
	Findings []finding `json:"findings"`
	// Note states, in the document, what this comparison is not.
	Note string `json:"note"`
}

// armSummary is one arm's contribution, including when its runs were taken.
//
// The time span is here rather than only in the interleaving verdict because the alternation test is coarse:
// four runs alternating A,B,A,B pass it, and so do six where the last two A runs were taken six hours after
// everything else. The span lets a reader see that without the tool having to invent a rule about how far
// apart is too far.
type armSummary struct {
	Arm    string   `json:"arm"`
	N      int      `json:"n"`
	RunIDs []string `json:"runIDs"`
	// WastedGPUSecondsMean, Min and Max describe this arm's runs. The spread is reported rather than a
	// standard deviation because two or three runs do not describe a distribution and a summary statistic
	// would imply they do.
	WastedGPUSecondsMean float64 `json:"wastedGPUSecondsMean"`
	WastedGPUSecondsMin  float64 `json:"wastedGPUSecondsMin"`
	WastedGPUSecondsMax  float64 `json:"wastedGPUSecondsMax"`
	// OwnerWaitSecondsMean is the quota owner's admission-to-running wait, averaged over this arm's runs, and
	// OwnerWaitRuns is how many of them restored their owner at all. The count is carried rather than implied
	// because a mean over a subset of the arm is a different statistic from a mean over the arm, and the run
	// where the owner never came back is the one a reader most needs to know about.
	OwnerWaitSecondsMean float64 `json:"ownerWaitSecondsMean,omitempty"`
	OwnerWaitSecondsMin  float64 `json:"ownerWaitSecondsMin,omitempty"`
	OwnerWaitSecondsMax  float64 `json:"ownerWaitSecondsMax,omitempty"`
	OwnerWaitRuns        int     `json:"ownerWaitRuns"`
	// FloorSeconds is the coarsest resolution among THIS arm's runs. It is per-arm because a difference
	// between two arms carries both of their errors, and they can be opposite-signed: the resolution of the
	// difference is the SUM of the two, never the larger. Taking the larger bounds one side and forgets the
	// other, which understates the error budget by up to a factor of two.
	FloorSeconds float64 `json:"floorSeconds"`
	// Bounded is false when any of this arm's runs bounded nothing.
	Bounded        bool   `json:"bounded"`
	FirstStartedAt string `json:"firstStartedAt"`
	LastStartedAt  string `json:"lastStartedAt"`
}

// finding is one quantity compared across one pair of arms.
type finding struct {
	Quantity string  `json:"quantity"`
	ArmA     string  `json:"armA"`
	ArmB     string  `json:"armB"`
	ValueA   float64 `json:"valueA"`
	ValueB   float64 `json:"valueB"`
	// DifferenceSeconds is |ValueA - ValueB|.
	DifferenceSeconds float64 `json:"differenceSeconds"`
	// Resolved is the only claim this document makes about the difference, and it is a claim about the
	// INSTRUMENT rather than about the world: true means the difference is larger than the coarsest
	// resolution among the contributing runs, so the harness could see it at all.
	Resolved bool `json:"resolved"`
	// Statement is the sentence a reader may quote. It is generated from the fields above so that it cannot
	// drift from them, which is exactly how the retracted claim survived review the first time.
	Statement string `json:"statement"`
}

// compareRecords builds the comparison, or refuses.
//
// Refusal rather than exclusion is the rule, and it is worth stating why, because dropping the offending
// records and comparing the rest is friendlier and wrong. The record set is named explicitly by whoever runs
// this; silently narrowing it produces a document whose file list does not describe the evidence behind it,
// which is the reproducibility this tool exists to provide. An operator who wants a subset names the subset.
func compareRecords(recs []runRecord) (comparison, error) {
	if len(recs) < 2 {
		return comparison{}, fmt.Errorf("a comparison needs at least 2 records, got %d", len(recs))
	}
	dose := recs[0].Dose
	for _, r := range recs {
		if r.Validity.Verdict != verdictAdmissible {
			return comparison{}, fmt.Errorf("run %q has verdict %q: a comparison is built only from records "+
				"whose own gates passed, and excluding it silently would leave this document's file list "+
				"describing evidence it does not contain", r.RunID, r.Validity.Verdict)
		}
		if r.Measurement == nil {
			return comparison{}, fmt.Errorf("run %q carries no measurement", r.RunID)
		}
		if r.Dose != dose {
			return comparison{}, fmt.Errorf("run %q is dose %q and run %q is dose %q: the two regimes measure "+
				"different quantities, so a difference between them answers no single question",
				recs[0].RunID, dose, r.RunID, r.Dose)
		}
	}

	byArm := map[string][]runRecord{}
	for _, r := range recs {
		byArm[r.Arm] = append(byArm[r.Arm], r)
	}
	if len(byArm) < 2 {
		return comparison{}, fmt.Errorf("every record is arm %q: there is nothing to compare", recs[0].Arm)
	}

	floorNs, bounded := pooledFloorNs(recs)
	c := comparison{
		SchemaVersion:        comparisonSchemaVersion,
		Dose:                 dose,
		WorstRunFloorSeconds: float64(floorNs) / float64(time.Second),
		Bounded:              bounded,
		Interleaved:          armsInterleave(recs),
		DeviceEvidence:       pooledDeviceEvidence(recs),
		Note: "this document reports whether a difference exceeded the coarsest resolution among the runs " +
			"behind it, whether the arms were interleaved in time, and how many runs each arm has. It makes " +
			"no claim of statistical significance and establishes no effect: these runs are not a sample of " +
			"anything and their variance has never been characterised",
	}

	arms := make([]string, 0, len(byArm))
	for a := range byArm {
		arms = append(arms, a)
	}
	sort.Strings(arms)
	for _, a := range arms {
		c.Arms = append(c.Arms, summariseArm(a, byArm[a]))
	}
	for i := 0; i < len(c.Arms); i++ {
		for j := i + 1; j < len(c.Arms); j++ {
			floor, ok := differenceFloor(c.Arms[i], c.Arms[j])
			c.Findings = append(c.Findings, compareWaste(c.Arms[i], c.Arms[j], floor, ok, c.Interleaved))
			c.Findings = append(c.Findings, compareOwnerWait(c.Arms[i], c.Arms[j], floor, ok, c.Interleaved))
		}
	}
	return c, nil
}

// pooledDeviceEvidence is the weakest axis among the contributors, for the same reason the floor is the
// coarsest: a set of runs establishes only what its weakest member did.
func pooledDeviceEvidence(recs []runRecord) string {
	for _, r := range recs {
		if r.Validity.DeviceEvidence != deviceWorkObserved {
			return deviceNotObserved
		}
	}
	return deviceWorkObserved
}

// pooledFloorNs is the coarsest floor among the contributing runs, and false when any of them had none.
//
// The maximum rather than a mean or the best one: a set of runs cannot distinguish anything more finely than
// its worst-resolved member, because that member's number is in every difference it contributes to. And a
// single unbounded run makes the whole comparison unbounded for the same reason — there is no bound to pool.
func pooledFloorNs(recs []runRecord) (int64, bool) {
	var worst int64
	for _, r := range recs {
		if r.Measurement.Resolution == nil {
			return 0, false
		}
		if f := r.Measurement.Resolution.ResolvedToNs; f > worst {
			worst = f
		}
	}
	return worst, worst > 0
}

// armsInterleave reports whether the arms alternate in the order the runs were started.
//
// The test is that the sorted sequence of arms contains more consecutive blocks than there are arms, which
// is exactly the condition that some arm was returned to after another had run. Four runs taken A,B,A,B give
// four blocks over two arms and interleave; the same four taken A,A,B,B give two blocks over two arms and do
// not, and in that second case every difference between them is confounded with whatever changed over the
// morning.
//
// A record without a start time makes the answer unknown, and unknown is reported as not interleaved: the
// confounding either happened or cannot be ruled out, and those two are the same for a reader deciding how
// much to trust the difference.
func armsInterleave(recs []runRecord) bool {
	return armsInterleaveBy(recs, func(r runRecord) string { return r.Arm })
}

// armsInterleaveBy is the same test over any grouping of the runs.
//
// It is generalised because the dose-sensitivity check needs exactly this question about REGIMES and went
// without it: the arm comparison warned about blocked runs while the document that licenses a GPU baseline
// did not, and the bias runs the wrong way there. Its favourable verdict is "no response", and undetected
// drift between two blocks manufactures a response rather than hiding one -- so the missing check made the
// document harder to trust in the direction it was least able to notice.
func armsInterleaveBy(recs []runRecord, key func(runRecord) string) bool {
	type started struct {
		at    time.Time
		group string
	}
	seq := make([]started, 0, len(recs))
	distinct := map[string]struct{}{}
	for _, r := range recs {
		t, err := time.Parse(time.RFC3339, r.StartedAt)
		if err != nil {
			return false
		}
		g := key(r)
		seq = append(seq, started{at: t, group: g})
		distinct[g] = struct{}{}
	}
	sort.Slice(seq, func(i, j int) bool { return seq[i].at.Before(seq[j].at) })
	blocks := 0
	for i, s := range seq {
		if i == 0 || seq[i-1].group != s.group {
			blocks++
		}
	}
	return blocks > len(distinct)
}

// summariseArm reduces one arm's runs to what a comparison may say about them.
func summariseArm(arm string, recs []runRecord) armSummary {
	sort.Slice(recs, func(i, j int) bool { return recs[i].StartedAt < recs[j].StartedAt })
	s := armSummary{
		Arm:                 arm,
		N:                   len(recs),
		WastedGPUSecondsMin: recs[0].Measurement.WastedGPUSeconds,
		WastedGPUSecondsMax: recs[0].Measurement.WastedGPUSeconds,
		FirstStartedAt:      recs[0].StartedAt,
		LastStartedAt:       recs[len(recs)-1].StartedAt,
	}
	var sum float64
	for _, r := range recs {
		s.RunIDs = append(s.RunIDs, r.RunID)
		w := r.Measurement.WastedGPUSeconds
		sum += w
		if w < s.WastedGPUSecondsMin {
			s.WastedGPUSecondsMin = w
		}
		if w > s.WastedGPUSecondsMax {
			s.WastedGPUSecondsMax = w
		}
	}
	s.WastedGPUSecondsMean = sum / float64(len(recs))

	var waitSum float64
	for _, r := range recs {
		if r.Measurement.OwnerAdmitToReadyNs == nil {
			continue
		}
		w := float64(*r.Measurement.OwnerAdmitToReadyNs) / float64(time.Second)
		if s.OwnerWaitRuns == 0 {
			s.OwnerWaitSecondsMin, s.OwnerWaitSecondsMax = w, w
		}
		if w < s.OwnerWaitSecondsMin {
			s.OwnerWaitSecondsMin = w
		}
		if w > s.OwnerWaitSecondsMax {
			s.OwnerWaitSecondsMax = w
		}
		waitSum += w
		s.OwnerWaitRuns++
	}
	if s.OwnerWaitRuns > 0 {
		s.OwnerWaitSecondsMean = waitSum / float64(s.OwnerWaitRuns)
	}
	floorNs, bounded := pooledFloorNs(recs)
	s.FloorSeconds, s.Bounded = float64(floorNs)/float64(time.Second), bounded
	return s
}

// differenceFloor is the resolution of a difference between two arms: the SUM of their floors.
//
// Each arm's figure carries its own error, the two are measured independently, and nothing forces them to
// lean the same way. An arm biased high against one biased low produces a difference wrong by both amounts,
// so the larger of the two bounds one side and silently drops the other. The current published differences
// survive the correction with an order of magnitude to spare; a GPU session's expected two-to-five second
// effect would not, which is why this is worth getting right before the money rather than after.
func differenceFloor(a, b armSummary) (float64, bool) {
	if !a.Bounded || !b.Bounded {
		return 0, false
	}
	return a.FloorSeconds + b.FloorSeconds, true
}

// compareWaste states what the two arms' discarded work supports, and nothing beyond it.
func compareWaste(a, b armSummary, floor float64, bounded, interleaved bool) finding {
	diff := a.WastedGPUSecondsMean - b.WastedGPUSecondsMean
	if diff < 0 {
		diff = -diff
	}
	f := finding{
		Quantity:          "wastedGPUSeconds",
		ArmA:              a.Arm,
		ArmB:              b.Arm,
		ValueA:            a.WastedGPUSecondsMean,
		ValueB:            b.WastedGPUSecondsMean,
		DifferenceSeconds: diff,
		Resolved:          bounded && queuelab.ResolvesAgainst(queuelab.SecondsToNs(diff), queuelab.SecondsToNs(floor)),
	}
	f.Statement = wasteStatement(f, floor, bounded, interleaved, a, b)
	return f
}

// wasteStatement writes the one sentence a reader may quote, from the fields beside it.
//
// It is generated rather than written by hand because the failure this whole file exists to prevent is a
// sentence that outlives the numbers it was true of. A hand-written line saying "honouring SIGTERM costs 41
// GPU-seconds" stayed correct-looking after the harness learned it could not resolve the residual inside it.
func wasteStatement(f finding, floor float64, bounded, interleaved bool, a, b armSummary) string {
	var sb strings.Builder
	if !bounded {
		fmt.Fprintf(&sb, "NOT RESOLVED: a contributing run bounded nothing, so the %.1f s gap between %s and %s "+
			"is not known to be larger than what the harness could see", f.DifferenceSeconds, f.ArmA, f.ArmB)
		return sb.String()
	}
	if !f.Resolved {
		fmt.Fprintf(&sb, "NOT RESOLVED: %s and %s differ by %.3f s, which is inside this comparison's %.3f s "+
			"floor. No number of repetitions changes that -- a resolution limit is not a noise level",
			f.ArmA, f.ArmB, f.DifferenceSeconds, floor)
		return sb.String()
	}
	fmt.Fprintf(&sb, "%s discarded %.1f GPU-s and %s discarded %.1f, a difference of %.1f s against a %.3f s "+
		"floor, over n=%d and n=%d runs", f.ArmA, f.ValueA, f.ArmB, f.ValueB, f.DifferenceSeconds, floor, a.N, b.N)
	if !interleaved {
		sb.WriteString(". CONFOUNDED: the arms did not alternate in time, so this difference moves together " +
			"with everything else that changed between the two blocks of runs")
	}
	return sb.String()
}

// compareOwnerWait states what the two arms cost the quota OWNER, which is the only quantity here that a
// platform promises anybody.
//
// The discarded seconds beside it are the borrower's loss: real, and not a service-level objective. "Your
// reclaimed capacity comes back within X" is, and this is most of X.
//
// Most, not all, and the gap is named rather than left implied. The interval starts when Kueue ADMITS the
// owner, so the tenant's queueing time before that is outside it -- the reconstruction computes that half as
// AdmitLatencyNs and the record does not carry it. It ends at Pod Ready, which for a container with no
// readiness probe is container start, so anything the process does to make itself useful is outside it too.
// What sits between those two points is the part reclamation governs, which is why it is the arm contrast;
// what sits outside them is why the field is called ownerAdmitToReady and not "the owner's wait".
func compareOwnerWait(a, b armSummary, floor float64, bounded, interleaved bool) finding {
	f := finding{Quantity: "ownerAdmitToReadySeconds", ArmA: a.Arm, ArmB: b.Arm}
	if a.OwnerWaitRuns == 0 || b.OwnerWaitRuns == 0 {
		f.Statement = fmt.Sprintf("NOT COMPUTED: %s restored its owner in %d of %d runs and %s in %d of %d; "+
			"an arm whose owner never came back has no wait to average, and averaging the others would "+
			"report the best case of an arm defined by its worst",
			a.Arm, a.OwnerWaitRuns, a.N, b.Arm, b.OwnerWaitRuns, b.N)
		return f
	}
	f.ValueA, f.ValueB = a.OwnerWaitSecondsMean, b.OwnerWaitSecondsMean
	diff := f.ValueA - f.ValueB
	if diff < 0 {
		diff = -diff
	}
	f.DifferenceSeconds = diff
	f.Resolved = bounded && queuelab.ResolvesAgainst(queuelab.SecondsToNs(diff), queuelab.SecondsToNs(floor))
	// A mean taken over the runs whose owner came back is a SURVIVOR mean, and it is biased in a known
	// direction: the runs excluded are the slow tail, up to and including never. Comparing one against an
	// arm that restored every time understates exactly the arm that restored less often, which is the arm a
	// reader is most likely to be worried about. The figures are still reported -- they are what happened --
	// but nothing derived from them may be called resolved.
	if a.OwnerWaitRuns < a.N || b.OwnerWaitRuns < b.N {
		f.Resolved = false
		f.Statement = fmt.Sprintf("SURVIVOR MEAN, NOT RESOLVED: %s restored its owner in %d of %d runs and %s "+
			"in %d of %d. The means below are over the runs that came back, so they omit the slow tail of "+
			"whichever arm omitted more of it: %.3f s under %s against %.3f s under %s",
			a.Arm, a.OwnerWaitRuns, a.N, b.Arm, b.OwnerWaitRuns, b.N, f.ValueA, a.Arm, f.ValueB, b.Arm)
		return f
	}
	switch {
	case !bounded:
		f.Statement = fmt.Sprintf("NOT RESOLVED: a contributing run bounded nothing, so the %.1f s gap "+
			"between %s and %s is not known to be larger than what the harness could see", diff, a.Arm, b.Arm)
	case !f.Resolved:
		f.Statement = fmt.Sprintf("NOT RESOLVED: the owner waited %.3f s under %s and %.3f s under %s, a "+
			"difference inside this comparison's %.3f s floor", f.ValueA, a.Arm, f.ValueB, b.Arm, floor)
	default:
		f.Statement = fmt.Sprintf("the quota owner was running %.3f s AFTER KUEUE ADMITTED IT under %s and "+
			"%.3f s under %s -- a difference of %.1f s against a %.3f s floor, over n=%d and n=%d restored "+
			"runs. It is the reclaim-side half of what a tenant experiences and the half a reclaim promise is "+
			"judged on; the time from SUBMISSION to admission is not in it, and neither is anything after the "+
			"container starts. The discarded seconds beside it are the borrower's loss, not a promise",
			f.ValueA, a.Arm, f.ValueB, b.Arm, diff, floor, a.OwnerWaitRuns, b.OwnerWaitRuns)
	}
	if f.Resolved && !interleaved {
		f.Statement += ". CONFOUNDED: the arms did not alternate in time"
	}
	return f
}

// renderComparison prints the comparison, leading with what it does not establish.
//
// The order is the argument. A reader who stops after the first screen should have read the limits, not the
// headline, because the headline is the part that travels into a slide unaccompanied.
func renderComparison(c comparison) string {
	var b strings.Builder
	fmt.Fprintf(&b, "===== COMPARISON (dose %s) =====\n", c.Dose)
	if !c.Bounded {
		b.WriteString("UNBOUNDED: a contributing run carried no resolution, so nothing below is resolved\n")
	} else {
		fmt.Fprintf(&b, "worstRunFloor=%.3fs -- orientation only; each finding below is tested against the SUM "+
			"of its two arms' floors, because a difference carries both of their errors\n", c.WorstRunFloorSeconds)
	}
	if !c.Interleaved {
		b.WriteString("NOT INTERLEAVED: the arms did not alternate in time, so every difference below is " +
			"confounded with the order the runs were taken in\n")
	} else {
		b.WriteString("interleaved: the arms alternated in time\n")
	}
	b.WriteString("no effect is claimed: this tool reports resolution and confounding, not inference\n")
	if c.DeviceEvidence != deviceWorkObserved {
		b.WriteString("device: NOT OBSERVED -- every GPU-second below is a second of RESERVATION. No run " +
			"behind this comparison established that a device did work, so nothing here is a statement " +
			"about GPU computation\n")
	}
	for _, a := range c.Arms {
		fmt.Fprintf(&b, "  %-9s n=%d waste mean=%.3f min=%.3f max=%.3f runs=%s\n    %s .. %s\n",
			a.Arm, a.N, a.WastedGPUSecondsMean, a.WastedGPUSecondsMin, a.WastedGPUSecondsMax,
			strings.Join(a.RunIDs, ","), a.FirstStartedAt, a.LastStartedAt)
		if a.OwnerWaitRuns == 0 {
			fmt.Fprintf(&b, "    ownerWait: NONE of %d runs restored the quota owner\n", a.N)
			continue
		}
		fmt.Fprintf(&b, "    ownerWait mean=%.3f min=%.3f max=%.3f over %d of %d runs\n",
			a.OwnerWaitSecondsMean, a.OwnerWaitSecondsMin, a.OwnerWaitSecondsMax, a.OwnerWaitRuns, a.N)
	}
	for _, f := range c.Findings {
		fmt.Fprintf(&b, "  [%s] %s\n", f.Quantity, f.Statement)
	}
	return b.String()
}

// runCompare is the -compare mode: read the named records, compare them, print and optionally persist.
//
// It writes the document only when asked, and the flag's help says what declining costs: a comparison that
// exists on a terminal and nowhere else is exactly the state this file was written to end.
// The -mode values that produce a document other than an arm comparison.
const (
	// modeBaseline emits the pooled restoration figure a later session differences against.
	modeBaseline = "baseline"
	// modeModel tests held = min(remaining service, grace) as arithmetic rather than as a judgement.
	modeModel = "model"
)

func runCompare(spec, outPath, mode string) error {
	paths, err := expandRecordSpec(spec)
	if err != nil {
		return err
	}
	recs := make([]runRecord, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		r, err := decodeRunRecord(b)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		recs = append(recs, r)
	}
	var doc any
	switch {
	case mode == modeBaseline:
		b, err := computeBaseline(recs)
		if err != nil {
			return err
		}
		fmt.Print(renderBaseline(b))
		doc = b
	case mode == modeModel:
		mc, err := checkModel(recs)
		if err != nil {
			return err
		}
		fmt.Print(renderModel(mc))
		doc = mc
	case mode == factorDose || mode == factorNode:
		d, err := checkSensitivity(recs, mode)
		if err != nil {
			return err
		}
		fmt.Print(renderDoseSensitivity(d))
		doc = d
	case mode != "":
		return fmt.Errorf("unknown -mode %q; want %q, %q, %q or %q",
			mode, modeBaseline, modeModel, factorDose, factorNode)
	default:
		c, err := compareRecords(recs)
		if err != nil {
			return err
		}
		fmt.Print(renderComparison(c))
		doc = c
	}
	if outPath == "" {
		return nil
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode comparison: %w", err)
	}
	if err := os.WriteFile(outPath, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Fprintln(os.Stderr, "  comparison:", outPath)
	return nil
}

// expandRecordSpec turns a comma-separated list of paths or globs into the files to read.
//
// A glob matching nothing is an error rather than an empty contribution, because the shell-quoted pattern
// that silently matches no files is how a comparison ends up built from half the runs its author believed
// they had passed it.
func expandRecordSpec(spec string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		matches, err := filepath.Glob(part)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", part, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%q matched no files", part)
		}
		out = append(out, matches...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-compare named no files")
	}
	sort.Strings(out)
	return out, nil
}

// doseSensitivity is whether a quantity moves with the protocol's dose.
//
// It is a separate document from comparison because it answers a separate question, and because compareRecords
// REFUSES to pool records from two dose regimes -- correctly, since the regimes measure different quantities
// and a difference between them answers nothing. That refusal is right about the borrower's discarded seconds,
// which the trace determines outright, and it is too coarse for the owner's wait: whether THAT moves with the
// dose is not an assumption to be protected, it is the question.
//
// It exists because the answer decides whether the real-GPU session has a baseline at all. A GPU workload will
// not have this one's service time, so any quantity the dose determines cannot be differenced against these
// runs. A quantity that does not move with it can.
type doseSensitivity struct {
	SchemaVersion int    `json:"schemaVersion"`
	Arm           string `json:"arm"`
	// Factor names what was varied while the arm was held fixed: the dose regime, or the worker node.
	//
	// The node is here because "one cluster, one pinned worker" was the largest unqualified limit on every
	// figure this lab has produced. A result that holds on one node and not another is a property of that
	// node, and until the node is varied nothing in the record distinguishes the two readings.
	Factor string `json:"factor"`
	// Doses is one entry per level of the factor, each summarising that level's runs of this arm.
	Doses []doseGroup `json:"doses"`
	// FloorSeconds is the SUM of the two regimes' floors, because the spread below is a difference between
	// two independently measured means and carries both of their errors.
	FloorSeconds float64 `json:"floorSeconds"`
	Bounded      bool    `json:"bounded"`
	// Resolved is whether the mean above clears its own floor.
	//
	// It exists because it did not, and the omission was an asymmetry rather than an oversight: the model
	// check refuses to read a sub-floor magnitude as measured, and this printed one as though it were.
	Resolved bool `json:"resolved"`
	// Interleaved is whether the regimes alternate in the order the runs were taken. When false, any response
	// this reports moves together with everything else that changed between the two blocks -- node warming,
	// image cache, cluster drift -- and the check cannot tell them apart. It was missing from this document
	// while the arm comparison had it, which is exactly the direction the bias runs: the verdict that licenses
	// a GPU baseline is "no response", and undetected drift makes a response appear rather than disappear.
	Interleaved bool `json:"interleaved"`
	// SpreadSeconds is the largest gap between any two regimes' mean owner wait.
	SpreadSeconds float64 `json:"spreadSeconds"`
	// MovesWithDose is true when that spread EXCEEDS the floor, meaning the harness can see the quantity
	// responding to the dose. False means it could not see a response, which is not the same as proving there
	// is none -- Statement says which of the two this is.
	MovesWithDose bool   `json:"movesWithDose"`
	Statement     string `json:"statement"`
}

// doseGroup is one regime's runs of the arm under test.
type doseGroup struct {
	// Dose is the level of the factor this group holds -- a regime name, or a node name.
	Dose                 string   `json:"dose"`
	N                    int      `json:"n"`
	RunIDs               []string `json:"runIDs"`
	OwnerWaitSecondsMean float64  `json:"ownerWaitSecondsMean"`
	OwnerWaitRuns        int      `json:"ownerWaitRuns"`
}

// The factors a sensitivity check can vary. They are constants because the document persists one and a
// reader classifies on it.
const (
	factorDose = "dose"
	factorNode = "node"
)

// levelOf extracts a record's level of the named factor, or "" when the record cannot say.
//
// The node comes from the qualification rather than from the ownership window, because the qualification is
// what the run inspected BEFORE creating anything -- the window's node is the same value observed later, and
// preferring the earlier one keeps the grouping independent of how the run ended.
func levelOf(r runRecord, factor string) string {
	switch factor {
	case factorDose:
		return r.Dose
	case factorNode:
		if r.Qualification == nil {
			return ""
		}
		return r.Qualification.Node
	}
	return ""
}

// checkSensitivity holds the arm fixed, varies one named factor, and reports whether the owner's wait
// responds to it.
func checkSensitivity(recs []runRecord, factor string) (doseSensitivity, error) {
	if len(recs) < 2 {
		return doseSensitivity{}, fmt.Errorf("a dose-sensitivity check needs at least 2 records, got %d", len(recs))
	}
	arm := recs[0].Arm
	byDose := map[string][]runRecord{}
	for _, r := range recs {
		if r.Validity.Verdict != verdictAdmissible {
			return doseSensitivity{}, fmt.Errorf("run %q has verdict %q", r.RunID, r.Validity.Verdict)
		}
		if r.Measurement == nil {
			return doseSensitivity{}, fmt.Errorf("run %q carries no measurement", r.RunID)
		}
		// One arm only. Pooling arms here would measure the arm difference and call it a dose response, which
		// is the confusion this whole document exists to keep out of the GPU session's baseline.
		if r.Arm != arm {
			return doseSensitivity{}, fmt.Errorf("run %q is arm %q and run %q is arm %q: a dose-sensitivity "+
				"check varies the dose and holds the arm fixed, or it measures the arms and calls it a dose",
				recs[0].RunID, arm, r.RunID, r.Arm)
		}
		level := levelOf(r, factor)
		if level == "" {
			return doseSensitivity{}, fmt.Errorf("run %q does not state its %s, so it cannot be grouped by one",
				r.RunID, factor)
		}
		byDose[level] = append(byDose[level], r)
	}
	if len(byDose) < 2 {
		return doseSensitivity{}, fmt.Errorf("every record is %s %q: nothing varies",
			factor, levelOf(recs[0], factor))
	}
	// When the node is the factor, the DOSE must be held fixed, and vice versa. Varying two things and
	// reporting one of them is how a node difference gets published as a dose response.
	held := factorDose
	if factor == factorDose {
		held = factorNode
	}
	fixed := levelOf(recs[0], held)
	for _, r := range recs {
		if levelOf(r, held) != fixed {
			return doseSensitivity{}, fmt.Errorf("this varies %s, so %s must be held fixed: run %q is %s %q "+
				"and run %q is %q", factor, held, recs[0].RunID, held, fixed, r.RunID, levelOf(r, held))
		}
	}

	d := doseSensitivity{SchemaVersion: comparisonSchemaVersion, Arm: arm, Factor: factor,
		Interleaved: armsInterleaveBy(recs, func(r runRecord) string { return levelOf(r, factor) })}

	doses := make([]string, 0, len(byDose))
	for k := range byDose {
		doses = append(doses, k)
	}
	sort.Strings(doses)
	for _, dose := range doses {
		g := doseGroup{Dose: dose}
		var sum float64
		for _, r := range byDose[dose] {
			g.N++
			g.RunIDs = append(g.RunIDs, r.RunID)
			if r.Measurement.OwnerAdmitToReadyNs == nil {
				continue
			}
			sum += float64(*r.Measurement.OwnerAdmitToReadyNs) / float64(time.Second)
			g.OwnerWaitRuns++
		}
		// EVERY run must have restored its owner, not merely one. A mean over the runs that came back is a
		// survivor mean, and it omits the slow tail -- which is the part a dose response would show up in
		// first. This document exists to license a baseline for a session nobody can re-run cheaply, so it
		// refuses rather than reports a figure with a known direction of bias.
		if g.OwnerWaitRuns != g.N {
			return doseSensitivity{}, fmt.Errorf("%s %q restored its owner in %d of %d runs; a mean over the "+
				"survivors omits the slow tail, which is where a response would appear first",
				factor, dose, g.OwnerWaitRuns, g.N)
		}
		g.OwnerWaitSecondsMean = sum / float64(g.OwnerWaitRuns)
		d.Doses = append(d.Doses, g)
	}

	lo, hi := d.Doses[0].OwnerWaitSecondsMean, d.Doses[0].OwnerWaitSecondsMean
	loDose, hiDose := d.Doses[0].Dose, d.Doses[0].Dose
	for _, g := range d.Doses {
		if g.OwnerWaitSecondsMean < lo {
			lo, loDose = g.OwnerWaitSecondsMean, g.Dose
		}
		if g.OwnerWaitSecondsMean > hi {
			hi, hiDose = g.OwnerWaitSecondsMean, g.Dose
		}
	}
	d.SpreadSeconds = hi - lo
	// The floor is the sum of the two extreme regimes' own floors, for the reason differenceFloor gives: the
	// spread is a difference between independently measured means and carries both of their errors.
	var floor float64
	d.Bounded = true
	for _, dose := range []string{loDose, hiDose} {
		f, ok := pooledFloorNs(byDose[dose])
		if !ok {
			d.Bounded = false
			break
		}
		floor += float64(f) / float64(time.Second)
	}
	d.FloorSeconds = floor
	d.MovesWithDose = d.Bounded && queuelab.ResolvesAgainst(
		queuelab.SecondsToNs(d.SpreadSeconds), queuelab.SecondsToNs(d.FloorSeconds))
	switch {
	case !d.Bounded:
		d.Statement = "UNBOUNDED: a contributing run bounded nothing, so no conclusion about the dose follows"
	case d.MovesWithDose:
		d.Statement = fmt.Sprintf("the owner's wait under %s moves %.3f s across %d levels of %s, which "+
			"EXCEEDS the %.3f s floor: this quantity responds to %s, and a figure that responds to %s cannot "+
			"be quoted as a property of the platform",
			arm, d.SpreadSeconds, len(d.Doses), factor, d.FloorSeconds, factor, factor)
	default:
		d.Statement = fmt.Sprintf("the owner's wait under %s moves %.3f s across %d levels of %s, INSIDE the "+
			"%.3f s floor. The harness cannot see it responding to %s -- which is not proof that it does not, "+
			"and is the condition a baseline needs: a quantity that varies with %s could not be quoted as a "+
			"property of the platform",
			arm, d.SpreadSeconds, len(d.Doses), factor, d.FloorSeconds, factor, factor)
	}
	return d, nil
}

// renderDoseSensitivity prints the check, leading with the statement rather than the numbers.
func renderDoseSensitivity(d doseSensitivity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "===== SENSITIVITY TO %s (arm %s) =====\n%s\n",
		strings.ToUpper(d.Factor), d.Arm, d.Statement)
	if !d.Interleaved {
		fmt.Fprintf(&b, "NOT INTERLEAVED: the %s levels did not alternate in time, so anything reported above "+
			"moves together with whatever else changed between the blocks of runs\n", d.Factor)
	}
	for _, g := range d.Doses {
		fmt.Fprintf(&b, "  %-16s n=%d ownerWait mean=%.3f over %d restored runs=%s\n",
			g.Dose, g.N, g.OwnerWaitSecondsMean, g.OwnerWaitRuns, strings.Join(g.RunIDs, ","))
	}
	return b.String()
}

// baseline is the pooled restoration figure a later session differences against, emitted as an artifact.
//
// It exists because it was the last hand-typed number in this lab, and it was wrong. A pre-registration
// fixed the GPU session's baseline at "2.690 s mean, 2.534-2.792 s, spread 258 ms, 8 observations" — a table
// assembled at a shell from four records that were subsequently deleted, beside machine-generated figures in
// the same document that reproduce to the digit. The value 2.534 appears in no record that exists. Every
// other conclusion in this lab had already been converted from arithmetic into a document; this one had not,
// and it is the one the entire session is defined against.
//
// So it refuses more than the comparisons do. One arm, every run admissible, every owner restored, at least
// two runs — because a baseline is quoted long after the runs are gone, by someone who will not re-derive it.
type baseline struct {
	SchemaVersion int    `json:"schemaVersion"`
	Arm           string `json:"arm"`
	N             int    `json:"n"`
	// RunIDs, Doses and Nodes name exactly what stands behind the figure, so a reader can go and check rather
	// than take it. The pre-registration's table could not be checked, which is how it stayed wrong.
	RunIDs []string `json:"runIDs"`
	Doses  []string `json:"doses"`
	Nodes  []string `json:"nodes"`

	OwnerWaitSecondsMean   float64 `json:"ownerWaitSecondsMean"`
	OwnerWaitSecondsMin    float64 `json:"ownerWaitSecondsMin"`
	OwnerWaitSecondsMax    float64 `json:"ownerWaitSecondsMax"`
	OwnerWaitSpreadSeconds float64 `json:"ownerWaitSpreadSeconds"`

	// FloorSeconds is the coarsest single-run bound among the contributors. A later session differencing
	// against this figure must ADD its own floor to this one, for the reason differenceFloor gives.
	FloorSeconds float64 `json:"floorSeconds"`
	Bounded      bool    `json:"bounded"`
	// Resolved is whether the mean above clears its own floor.
	//
	// It exists because it did not, and the omission was an asymmetry rather than an oversight: the model
	// check refuses to read a sub-floor magnitude as a measured near-zero, and this printed one as though it
	// were measured. An unresolved baseline is exactly the case where a later session would difference
	// against a number nobody measured.
	Resolved bool `json:"resolved"`
	// DeviceEvidence is the weakest axis among the contributors, so a baseline taken on a fake device plugin
	// cannot be quoted as though a device were involved.
	DeviceEvidence string `json:"deviceEvidence"`
	Interleaved    bool   `json:"interleaved"`
	Statement      string `json:"statement"`
}

// computeBaseline pools one arm's runs into the figure, or refuses.
func computeBaseline(recs []runRecord) (baseline, error) {
	if len(recs) < 2 {
		return baseline{}, fmt.Errorf("a baseline needs at least 2 runs, got %d", len(recs))
	}
	arm := recs[0].Arm
	b := baseline{SchemaVersion: comparisonSchemaVersion, Arm: arm, N: len(recs)}
	seenDose, seenNode := map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		if r.Arm != arm {
			return baseline{}, fmt.Errorf("run %q is arm %q and run %q is arm %q: a baseline is one arm's "+
				"figure, and pooling two would average the very difference the arms exist to show",
				recs[0].RunID, arm, r.RunID, r.Arm)
		}
		if r.Validity.Verdict != verdictAdmissible {
			return baseline{}, fmt.Errorf("run %q has verdict %q", r.RunID, r.Validity.Verdict)
		}
		if r.Measurement == nil || r.Measurement.OwnerAdmitToReadyNs == nil {
			return baseline{}, fmt.Errorf("run %q never restored its quota owner; a baseline averaged over "+
				"the runs that came back omits the slow tail and would be quoted as the platform's floor",
				r.RunID)
		}
		b.RunIDs = append(b.RunIDs, r.RunID)
		if !seenDose[r.Dose] {
			seenDose[r.Dose] = true
			b.Doses = append(b.Doses, r.Dose)
		}
		if n := levelOf(r, factorNode); n != "" && !seenNode[n] {
			seenNode[n] = true
			b.Nodes = append(b.Nodes, n)
		}
	}
	sort.Strings(b.Doses)
	sort.Strings(b.Nodes)

	var sum float64
	for i, r := range recs {
		w := float64(*r.Measurement.OwnerAdmitToReadyNs) / float64(time.Second)
		if i == 0 {
			b.OwnerWaitSecondsMin, b.OwnerWaitSecondsMax = w, w
		}
		if w < b.OwnerWaitSecondsMin {
			b.OwnerWaitSecondsMin = w
		}
		if w > b.OwnerWaitSecondsMax {
			b.OwnerWaitSecondsMax = w
		}
		sum += w
	}
	b.OwnerWaitSecondsMean = sum / float64(len(recs))
	b.OwnerWaitSpreadSeconds = b.OwnerWaitSecondsMax - b.OwnerWaitSecondsMin

	floorNs, bounded := pooledFloorNs(recs)
	b.FloorSeconds, b.Bounded = float64(floorNs)/float64(time.Second), bounded
	b.DeviceEvidence = pooledDeviceEvidence(recs)
	b.Interleaved = armsInterleaveBy(recs, func(r runRecord) string { return r.Dose })
	// The harness's own resolution rule, applied to the harness's own baseline.
	//
	// It was not, and a hostile review found the asymmetry: the model check refuses to read the honouring
	// arm's 0.041 s hold as a measured near-zero because it sits under the floor, while this printed a mean
	// under ITS floor as though it were measured. The rule does not become optional when the figure is one
	// this lab wants to quote -- an unresolved baseline is exactly the case where a later session would
	// difference against a number nobody measured.
	b.Resolved = queuelab.ResolvesAgainst(queuelab.SecondsToNs(b.OwnerWaitSecondsMean), queuelab.SecondsToNs(b.FloorSeconds))
	qualifier := ""
	if !b.Resolved {
		qualifier = fmt.Sprintf(" THIS MEAN IS BELOW ITS OWN FLOOR and is therefore UNRESOLVED: it lies "+
			"somewhere in [0, %.3f s] and must not be quoted as a measured duration, only differenced against "+
			"with both floors added.", b.FloorSeconds)
	}
	b.Statement = fmt.Sprintf("under %s the quota owner was running %.3f s after admission, over %d runs "+
		"spanning %d dose regime(s) and %d node(s), with a spread of %.0f ms against a worst-run floor of "+
		"%.3f s. A session differencing against this must add its own floor to that one; the difference of "+
		"two independently measured means carries both of their errors.%s",
		arm, b.OwnerWaitSecondsMean, b.N, len(b.Doses), len(b.Nodes), b.OwnerWaitSpreadSeconds*1000,
		b.FloorSeconds, qualifier)
	return b, nil
}

// renderBaseline prints the figure with what qualifies it, never the figure alone.
func renderBaseline(b baseline) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "===== BASELINE (arm %s) =====\n%s\n", b.Arm, b.Statement)
	fmt.Fprintf(&sb, "  ownerWait mean=%.3f min=%.3f max=%.3f spread=%.0fms n=%d\n",
		b.OwnerWaitSecondsMean, b.OwnerWaitSecondsMin, b.OwnerWaitSecondsMax,
		b.OwnerWaitSpreadSeconds*1000, b.N)
	fmt.Fprintf(&sb, "  runs=%s\n  doses=%s\n  nodes=%s\n",
		strings.Join(b.RunIDs, ","), strings.Join(b.Doses, ","), strings.Join(b.Nodes, ","))
	if !b.Bounded {
		sb.WriteString("  UNBOUNDED: a contributing run bounded nothing\n")
	}
	if !b.Interleaved {
		sb.WriteString("  NOT INTERLEAVED: the dose regimes did not alternate in time\n")
	}
	// Dispersion is judged against the FLOOR, not against the mean, and the difference matters.
	//
	// The first version compared the spread to the mean, which is a ratio with no instrument in it: it would
	// call a tight set "dispersed" merely for having a small mean. The floor is what this harness can see, so
	// runs that disagree by more than it disagree about something the harness did NOT control -- and runs
	// that disagree by less than it do not disagree at all, however large the gap looks. Nothing else gates
	// on within-cell dispersion, so this is the only place it is a judgement rather than two numbers to
	// subtract.
	//
	// On the committed set it does not fire: the honouring arm spans 1.208 s to 3.210 s, which looks alarming
	// and is entirely inside a 3.106 s floor.
	if b.OwnerWaitSpreadSeconds > b.FloorSeconds {
		fmt.Fprintf(&sb, "  DISPERSED: the runs spread %.0f ms, which exceeds the %.3f s this harness can "+
			"resolve, so they disagree about something it did not control\n",
			b.OwnerWaitSpreadSeconds*1000, b.FloorSeconds)
	}
	if b.DeviceEvidence != deviceWorkObserved {
		sb.WriteString("  device: NOT OBSERVED -- this baseline was taken where no run established that a " +
			"device did work, so it is a control-plane figure and not a statement about hardware\n")
	}
	return sb.String()
}

// modelCheck tests the lab's central claim against the quantity the claim is about.
//
// held = min(remaining service, termination grace period) is a statement about how long a preempted borrower
// keeps its DEVICE after the platform has committed that capacity to its owner. An earlier version of this
// check tested it against the owner's admission-to-running wait instead, which contains the hold and then the
// scheduling, the image and the container start on top — so it could only reach the model by subtracting an
// estimate of those, borrowed from the honouring arm.
//
// Two reviews took that apart independently and both were right. The borrowed term is not arm-independent:
// the owner's Pod spends the grace window pending against the device, so restoration after release costs
// about 1.6 s LESS in the arm being predicted than in the arm the estimate came from, which is the entire
// magnitude and sign of the residuals that version published. And the term could not be validated either way
// — at the floor it ran against, deleting the subtraction altogether left both residuals inside.
//
// So the subtraction is gone rather than corrected. The hold needs no platform-cost term because it contains
// no platform work, the honouring arm becomes a genuine zero-hold control instead of a subtrahend, and the
// interval is three orders of magnitude quieter than the one it replaces: tens of milliseconds of within-cell
// scatter against an owner-wait floor of seconds.
//
// The prediction is built from the dose the run ACHIEVED rather than the one it declared. Every recorded run
// overran its declared dose by 1.0 to 1.4 seconds, systematically, because the schedule gates the owner on a
// two-second poll — and the declared value is what the old prediction used.
type modelCheck struct {
	SchemaVersion int `json:"schemaVersion"`
	// Protocol states the constants the prediction was computed from, so a reader can check the arithmetic
	// rather than trust it. They live in this binary rather than in the record, so a record from a build with
	// different constants would be mispredicted silently — printing them is what makes that visible.
	Protocol modelProtocol `json:"protocol"`
	// ControlHoldSeconds is the honouring arm's own hold, and it is a CONTROL rather than a correction.
	//
	// Nothing is subtracted from anything. It is a few tens of milliseconds, and a review established that
	// this figure is BELOW THIS HARNESS'S OWN RESOLUTION by more than an order of magnitude: the per-run
	// floor for the hold's endpoints is around a second, and the control is around forty milliseconds.
	//
	// So it does not establish that the interval contains no platform work, and the earlier version of this
	// comment and of the printed statement said it did. What it establishes is weaker and still worth
	// printing: the honouring arm's hold is UNRESOLVED, somewhere in [0, floor], while the ignoring arm's is
	// thirty seconds -- a difference far larger than the floor. The thirty seconds are the borrower's rather
	// than the scheduler's because the two arms differ by nothing except whether the victim honours the
	// signal, not because the control was measured to be small.
	//
	// The inversion this avoids is named in resolution.go: turning "the harness cannot see this" into "the
	// harness measured this". It happened here, in the one figure the model check's whole chain rests on.
	ControlHoldSeconds float64 `json:"controlHoldSeconds"`
	ControlRuns        int     `json:"controlRuns"`
	// ControlResolved says whether the control is a quantity this harness can SEE, and it is a field rather
	// than a sentence because that is the difference between a rule and a hope.
	//
	// The statement carried the qualification in prose for exactly one commit before this. Prose is what a
	// consumer skips: a document whose control is unresolved and whose text says so is still a document a
	// script reads as a measured near-zero. Derived through queuelab.ResolvesAgainst, which is now the one
	// place the comparison's direction lives.
	ControlResolved bool `json:"controlResolved"`
	// Interleaved is whether the two dose regimes alternated in the order the runs were actually taken.
	//
	// Every other comparison here checks it and this one did not, which an independent review found. The
	// model check is the one carrying the headline, and it POOLS the two regimes: if they were run in blocks
	// rather than alternating, anything that drifted over the session -- cluster warmth, an image cache
	// filling, another tenant arriving -- lands entirely on one regime and arrives as the kink. That is
	// precisely the confound interleaving exists to rule out.
	Interleaved bool        `json:"interleaved"`
	Cases       []modelCase `json:"cases"`
	// Contrast is the difference BETWEEN the regimes, and it is the only part of this check that tests the
	// model's kink rather than its levels.
	//
	// min() says the hold follows remaining service on one side of the grace period and stops following it on
	// the other, so the gap between the two regimes must equal grace minus the shorter remaining service.
	//
	// It is NOT independent evidence, and the earlier version of this comment implied it was. The contrast
	// residual is identically the difference of the two case residuals -- observed and predicted are both
	// differences of the same two cells -- so it carries no degree of freedom the levels do not, and it is
	// already constrained to twice the floor before any datum is read.
	//
	// What cancels is a confound that is ADDITIVE and the same in both regimes. A dose-dependent one does
	// not: the two predictions are not symmetric, one being a compiled constant and the other a measured
	// achieved dose, so the self-completing cell's instrumentation offset enters the contrast with a single
	// sign. The contrast's real content is insensitivity to a common additive bias, which is worth having
	// and is less than "the test the model cannot pass by accident".
	Contrast     *modelContrast `json:"contrast,omitempty"`
	FloorSeconds float64        `json:"floorSeconds"`
	Bounded      bool           `json:"bounded"`
	Holds        bool           `json:"holds"`
	Statement    string         `json:"statement"`
}

// modelProtocol is the arithmetic's inputs.
type modelProtocol struct {
	VictimServiceSec    int `json:"victimServiceSec"`
	TerminationGraceSec int `json:"terminationGraceSec"`
}

// modelContrast is the difference between two regimes' holds, predicted and observed.
type modelContrast struct {
	From             string  `json:"from"`
	To               string  `json:"to"`
	PredictedSeconds float64 `json:"predictedSeconds"`
	ObservedSeconds  float64 `json:"observedSeconds"`
	ResidualSeconds  float64 `json:"residualSeconds"`
	InsideFloor      bool    `json:"insideFloor"`
}

// modelCase is one regime's prediction against its observation.
type modelCase struct {
	Dose string `json:"dose"`
	// DeclaredDoseSec and AchievedDoseSeconds are both carried because their difference is the correction.
	DeclaredDoseSec     int     `json:"declaredDoseSec"`
	AchievedDoseSeconds float64 `json:"achievedDoseSeconds"`
	RemainingSeconds    float64 `json:"remainingSeconds"`
	BindingTerm         string  `json:"bindingTerm"`
	PredictedSeconds    float64 `json:"predictedSeconds"`
	ObservedSeconds     float64 `json:"observedSeconds"`
	ResidualSeconds     float64 `json:"residualSeconds"`
	InsideFloor         bool    `json:"insideFloor"`
	N                   int     `json:"n"`
}

// holdSample pairs one run's device hold with the dose that run was actually given.
type holdSample struct{ hold, dose float64 }

// collectHoldSamples validates every record and splits it into the control's holds and the ignoring arm's
// dose/hold pairs.
//
// It is separate from checkModel because the two do different jobs: this refuses records, and checkModel
// fits a model to whatever survives. Reading them together hid that the refusals outnumber the arithmetic.
func collectHoldSamples(recs []runRecord) (map[string][]holdSample, float64, int, error) {
	ignore := map[string][]holdSample{}
	var controlSum float64
	var controlRuns int
	// One node only. The hold's endpoints are arrival times from two watches, and pooling nodes would add a
	// second collector's delivery behaviour to an interval whose whole value is that it is quiet.
	node := ""
	for _, r := range recs {
		if r.Validity.Verdict != verdictAdmissible {
			return nil, 0, 0, fmt.Errorf("run %q has verdict %q", r.RunID, r.Validity.Verdict)
		}
		if n := levelOf(r, factorNode); n != "" {
			if node == "" {
				node = n
			} else if n != node {
				return nil, 0, 0, fmt.Errorf("run %q is on node %q and run %q is on %q: the hold is read "+
					"from two watches' arrival times, so pooling nodes pools two collectors' delivery behaviour "+
					"into an interval whose value is that it is quiet", recs[0].RunID, node, r.RunID, n)
			}
		}
		hold := queuelab.DeviceHoldNs(r.Events)
		if hold == nil {
			return nil, 0, 0, fmt.Errorf("run %q's ledger does not carry the device hold", r.RunID)
		}
		// The two readings of that hold have to agree, and this is the check a comment beside the second one
		// promised and nothing performed. A stamp figure recorded and never compared is decoration: a clock
		// step between Kueue's machine and a kubelet would corrupt it silently, and watch pathology would
		// corrupt the arrival figure with the stamp fine.
		//
		// It sharpens nothing -- truncation swamps the differential lag in every recorded run -- and that is
		// not what it is for. It catches the arrival figure going wrong, which is the figure every published
		// number is built from.
		// BOTH readings are required, not merely consistent when both happen to be there. ClocksDisagree
		// answers "no disagreement" for a run carrying only one of them -- which is the honest answer to the
		// question it was asked and the wrong basis for admitting the run, because a rule stated as "the hold
		// is read on two clocks" is satisfied by neither reading being checked against anything. A review
		// found the gap: the check refused two readings that disagreed and never required two to exist.
		if queuelab.DeviceHoldStampNs(r.Events) == nil {
			return nil, 0, 0, fmt.Errorf("run %q carries no component-stamp reading of the device hold, so "+
				"its arrival figure was never checked against anything: one of the two endpoints' components "+
				"published no timestamp, and a hold read on one clock is a hold nothing corroborates", r.RunID)
		}
		if runFloor, ok := holdFloorNs([]runRecord{r}); ok {
			if bad, gap, tol := queuelab.ClocksDisagree(r.Events, runFloor); bad {
				return nil, 0, 0, fmt.Errorf("run %q's two readings of the device hold differ by %s against "+
					"a %s tolerance: the collector's arrival times and the components' own stamps describe "+
					"different intervals, so neither can be published",
					r.RunID, time.Duration(gap), time.Duration(tol))
			}
		}
		h := float64(*hold) / float64(time.Second)
		switch r.Arm {
		case string(queuelab.ArmAHonor):
			controlSum += h
			controlRuns++
		case string(queuelab.ArmAIgnore):
			dose := queuelab.AchievedDoseNs(r.Events)
			if dose == nil {
				return nil, 0, 0, fmt.Errorf("run %q's ledger does not carry the achieved dose", r.RunID)
			}
			ignore[r.Dose] = append(ignore[r.Dose], holdSample{hold: h, dose: float64(*dose) / float64(time.Second)})
		default:
			return nil, 0, 0, fmt.Errorf("run %q is arm %q; the model is stated over the two termination "+
				"contract arms", r.RunID, r.Arm)
		}
	}
	return ignore, controlSum, controlRuns, nil
}

// checkModel builds the check from one node's runs of both arms and both regimes.
func checkModel(recs []runRecord) (modelCheck, error) {
	m := modelCheck{SchemaVersion: comparisonSchemaVersion, Protocol: modelProtocol{
		VictimServiceSec:    victimServiceSec,
		TerminationGraceSec: terminationGraceSec,
	}}

	ignore, controlSum, controlRuns, err := collectHoldSamples(recs)
	if err != nil {
		return modelCheck{}, err
	}
	m.ControlRuns = controlRuns
	if m.ControlRuns == 0 {
		return modelCheck{}, fmt.Errorf("the check needs the honouring arm as its zero-hold control: without " +
			"it nothing shows the interval contains no platform work")
	}
	if len(ignore) < 2 {
		return modelCheck{}, fmt.Errorf("the check needs both dose regimes: one prediction can be fitted by " +
			"any model, and the two regimes put the victim on opposite sides of the grace period")
	}
	m.ControlHoldSeconds = controlSum / float64(m.ControlRuns)

	// The floor is restricted to the endpoints the hold is actually built from — the owner's admission and the
	// victim's stop — rather than pooled over every event kind. Pooling charges this interval the worst
	// behaviour of the owner's own completion, which it does not contain.
	floorNs, bounded := holdFloorNs(recs)
	m.FloorSeconds, m.Bounded = 2*float64(floorNs)/float64(time.Second), bounded
	// The control is judged by the same rule as everything else, and it does not pass it. Deriving this
	// rather than asserting it is what stops the statement drifting back to "shows the interval contains no
	// platform work" the next time someone edits the sentence.
	m.Interleaved = armsInterleaveBy(recs, func(r runRecord) string { return r.Dose })
	m.ControlResolved = m.Bounded && queuelab.ResolvesAgainst(
		queuelab.SecondsToNs(m.ControlHoldSeconds), queuelab.SecondsToNs(m.FloorSeconds))
	m.Holds = bounded

	doses := make([]string, 0, len(ignore))
	for d := range ignore {
		doses = append(doses, d)
	}
	sort.Strings(doses)
	for _, d := range doses {
		declared, err := declaredDoseFor(d)
		if err != nil {
			return modelCheck{}, err
		}
		var holdSum, doseSum float64
		for _, s := range ignore[d] {
			holdSum += s.hold
			doseSum += s.dose
		}
		n := float64(len(ignore[d]))
		c := modelCase{Dose: d, DeclaredDoseSec: declared, AchievedDoseSeconds: doseSum / n, N: len(ignore[d])}
		c.RemainingSeconds = float64(victimServiceSec) - c.AchievedDoseSeconds
		c.PredictedSeconds, c.BindingTerm = c.RemainingSeconds, "remaining service"
		if float64(terminationGraceSec) < c.RemainingSeconds {
			c.PredictedSeconds, c.BindingTerm = float64(terminationGraceSec), "termination grace"
		}
		c.ObservedSeconds = holdSum / n
		c.ResidualSeconds = c.ObservedSeconds - c.PredictedSeconds
		r := c.ResidualSeconds
		if r < 0 {
			r = -r
		}
		c.InsideFloor = bounded && r <= m.FloorSeconds
		if !c.InsideFloor {
			m.Holds = false
		}
		m.Cases = append(m.Cases, c)
	}

	// The contrast, when both regimes are present. Anything common to them cancels in it, so it is the test
	// the model cannot pass by accident.
	if len(m.Cases) == 2 {
		lo, hi := m.Cases[0], m.Cases[1]
		if lo.PredictedSeconds > hi.PredictedSeconds {
			lo, hi = hi, lo
		}
		c := &modelContrast{
			From:             lo.Dose,
			To:               hi.Dose,
			PredictedSeconds: hi.PredictedSeconds - lo.PredictedSeconds,
			ObservedSeconds:  hi.ObservedSeconds - lo.ObservedSeconds,
		}
		c.ResidualSeconds = c.ObservedSeconds - c.PredictedSeconds
		r := c.ResidualSeconds
		if r < 0 {
			r = -r
		}
		c.InsideFloor = bounded && r <= m.FloorSeconds
		if !c.InsideFloor {
			m.Holds = false
		}
		m.Contrast = c
	}

	switch {
	case !bounded:
		m.Statement = "UNBOUNDED: a contributing run bounded nothing, so no residual can be judged"
	case m.Holds:
		// "consistent with" rather than "validates", and the distinction is not modesty. Two runs per cell is
		// not an inferential study, the residuals are tested against a floor rather than an interval, and a
		// check evaluated on the same runs that produced it is in-sample. What the check does establish is
		// that no regime's hold falls outside the instrument's own bound of the prediction -- and that the
		// CONTRAST between the regimes, which nothing common to them can move, lands where the kink says.
		m.Statement = fmt.Sprintf("the device hold in every regime is CONSISTENT WITH held = min(remaining "+
			"service, %d s grace), to within the %.3f s floor and with nothing subtracted from anything. The "+
			"honouring arm's hold over %d runs measures %.3f s, which is %s -- it lies somewhere in "+
			"[0, %.3f s], and reading it as a measured near-zero would be "+
			"the inversion this harness's resolution rule exists to prevent. What the arms support is that "+
			"their difference is far larger than the floor. Two runs per cell, evaluated on the runs that "+
			"produced them; the self-completing cell's prediction is built from its own achieved dose, so its "+
			"residual is an instrumentation offset rather than a test of the rule -- this is consistency, and "+
			"weaker than validation",
			terminationGraceSec, m.FloorSeconds, m.ControlRuns, m.ControlHoldSeconds,
			resolvedWord(m.ControlResolved), m.FloorSeconds)
	default:
		m.Statement = fmt.Sprintf("REFUTED: at least one regime's device hold falls outside the %.3f s floor "+
			"of what held = min(remaining service, %d s grace) predicts", m.FloorSeconds, terminationGraceSec)
	}
	return m, nil
}

// holdFloorNs is the coarsest bound among the contributing runs, restricted to the hold's own endpoints.
func holdFloorNs(recs []runRecord) (int64, bool) {
	var worst int64
	for _, r := range recs {
		s := queuelab.SpreadOfMatching(r.Events, func(e *queuelab.LifecycleEvent) bool {
			return (e.Job == queuelab.OwnerRow && e.Type == queuelab.EventAdmitted) ||
				(e.Job == queuelab.VictimRow && e.Type == queuelab.EventAttemptStopped)
		})
		if s == nil {
			return 0, false
		}
		if s.FloorNs > worst {
			worst = s.FloorNs
		}
	}
	return worst, worst > 0
}

// declaredDoseFor is the dose the protocol declared for a regime, kept so the achieved value can be shown
// against it rather than silently replacing it.
func declaredDoseFor(dose string) (int, error) {
	switch queuelab.DoseRegime(dose) {
	case queuelab.DoseSelfCompleting:
		return doseSec, nil
	case queuelab.DoseGraceBounded:
		return graceBoundedDoseSec, nil
	}
	return 0, fmt.Errorf("unknown dose regime %q", dose)
}

// renderModel prints the prediction beside the observation, never one without the other.
// resolvedWord renders the control's verdict so the sentence follows the field rather than the other way
// round. If a future control ever does clear the floor, the statement says so instead of being wrong.
func resolvedWord(resolved bool) string {
	if resolved {
		return "ABOVE this floor and therefore resolved"
	}
	return "far BELOW this floor and therefore unresolved"
}

func renderModel(m modelCheck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "===== MODEL: held = min(remaining service, grace), tested on the DEVICE HOLD =====\n%s\n",
		m.Statement)
	fmt.Fprintf(&b, "  protocol: victimService=%ds grace=%ds\n",
		m.Protocol.VictimServiceSec, m.Protocol.TerminationGraceSec)
	fmt.Fprintf(&b, "  control: the honouring arm held the device %.3f s over %d runs (nothing is subtracted)\n",
		m.ControlHoldSeconds, m.ControlRuns)
	for _, c := range m.Cases {
		fmt.Fprintf(&b, "  %-16s dose declared=%ds achieved=%6.3f -> remaining=%6.3f binds on %-18s "+
			"predicted=%6.3f observed=%6.3f residual=%+.3f %s (n=%d)\n",
			c.Dose, c.DeclaredDoseSec, c.AchievedDoseSeconds, c.RemainingSeconds, c.BindingTerm,
			c.PredictedSeconds, c.ObservedSeconds, c.ResidualSeconds, insideMark(c.InsideFloor), c.N)
	}
	if c := m.Contrast; c != nil {
		fmt.Fprintf(&b, "  CONTRAST %s -> %s: predicted=%6.3f observed=%6.3f residual=%+.3f %s "+
			"(the kink; anything common to both regimes cancels here)\n",
			c.From, c.To, c.PredictedSeconds, c.ObservedSeconds, c.ResidualSeconds, insideMark(c.InsideFloor))
	}
	if !m.Interleaved {
		b.WriteString("  NOT INTERLEAVED: the dose regimes did not alternate in time, so anything that drifted " +
			"over the session lands on one of them and arrives as the kink this check reads\n")
	}
	fmt.Fprintf(&b, "  residuals judged against a %.3f s floor, restricted to the hold's own endpoints "+
		"(the owner's admission and the victim's stop) rather than pooled over every event kind\n",
		m.FloorSeconds)
	return b.String()
}

// insideMark names the verdict on one residual.
func insideMark(inside bool) string {
	if inside {
		return "INSIDE"
	}
	return "OUTSIDE"
}
