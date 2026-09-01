package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// findingFor picks one quantity out of a comparison, since every arm pair now produces several.
// checkSensitivity2 keeps the dose-factor call site short in tests.
func checkSensitivity2(recs []runRecord) (doseSensitivity, error) {
	return checkSensitivity(recs, factorDose)
}

func findingFor(t *testing.T, c comparison, quantity string) finding {
	t.Helper()
	for _, f := range c.Findings {
		if f.Quantity == quantity {
			return f
		}
	}
	t.Fatalf("the comparison reports no %s finding", quantity)
	return finding{}
}

// cmpRec builds a minimal admissible record for the comparison tests.
func cmpRec(runID, arm, dose, startedAt string, waste float64, floorNs int64) runRecord {
	m := &measurement{WastedGPUSeconds: waste}
	if floorNs > 0 {
		m.Resolution = &observationResolution{Samples: 2, ResolvedToNs: floorNs, QuantisationNs: int64(time.Second)}
	}
	return runRecord{
		SchemaVersion: recordSchemaVersion,
		RunID:         runID,
		Arm:           arm,
		Dose:          dose,
		StartedAt:     startedAt,
		Measurement:   m,
		Validity:      validity{Verdict: verdictAdmissible, DeviceEvidence: deviceNotObserved},
	}
}

// The difference the lab actually supports: an order of magnitude against a two-second floor.
func TestCompareResolvesTheCategoricalDifference(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("r001", "A-honor", "self-completing", "2026-08-19T05:45:57Z", 41.0, 1959*int64(time.Millisecond)),
		cmpRec("r002", "A-ignore", "self-completing", "2026-08-19T05:48:56Z", 0.0, 1959*int64(time.Millisecond)),
		cmpRec("r003", "A-honor", "self-completing", "2026-08-19T05:52:50Z", 40.9, 1959*int64(time.Millisecond)),
		cmpRec("r004", "A-ignore", "self-completing", "2026-08-19T05:55:30Z", 0.0, 1959*int64(time.Millisecond)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !c.Bounded || !c.Interleaved {
		t.Fatalf("bounded=%v interleaved=%v, want both true", c.Bounded, c.Interleaved)
	}
	waste := findingFor(t, c, "wastedGPUSeconds")
	if !waste.Resolved {
		t.Fatalf("a 41s difference against a 3.918s difference floor must resolve: %+v", waste)
	}
	if strings.Contains(waste.Statement, "CONFOUNDED") {
		t.Fatalf("alternating runs reported as confounded: %s", waste.Statement)
	}
}

// The retracted claim, as a test: a sub-second difference against this lab's own floor must come back NOT
// RESOLVED, and the statement must say why repeating the runs would not help.
func TestCompareRefusesADifferenceInsideTheFloor(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, 1959*int64(time.Millisecond)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 40.06, 1959*int64(time.Millisecond)),
		cmpRec("a2", "A-honor", "self-completing", "2026-08-19T05:10:00Z", 41.0, 1959*int64(time.Millisecond)),
		cmpRec("b2", "A-ignore", "self-completing", "2026-08-19T05:15:00Z", 40.06, 1959*int64(time.Millisecond)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	waste := findingFor(t, c, "wastedGPUSeconds")
	if waste.Resolved {
		t.Fatalf("0.94s resolved against a 3.918s difference floor: %+v", waste)
	}
	if !strings.Contains(waste.Statement, "NOT RESOLVED") ||
		!strings.Contains(waste.Statement, "not a noise level") {
		t.Fatalf("statement does not tell the reader repetition will not help: %s", waste.Statement)
	}
}

// The floor is the WORST among contributors, because that run's coarseness is inside every difference it
// contributes to. Taking the best would let one well-resolved run launder a poorly-resolved one.
func TestComparePoolsTheCoarsestFloor(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, 100*int64(time.Millisecond)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 36.0, 6*int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.WorstRunFloorSeconds != 6 {
		t.Fatalf("worst-run floor = %v, want the coarsest contributor's 6s", c.WorstRunFloorSeconds)
	}
	// The finding is tested against the SUM of the two arms' floors, not the larger, because the difference
	// carries both errors: 0.1 + 6 = 6.1 s here.
	if findingFor(t, c, "wastedGPUSeconds").Resolved {
		t.Fatal("a 5s difference resolved against a 6.1s difference floor")
	}
}

// One unbounded run makes the whole comparison unbounded: there is no bound to pool.
func TestCompareIsUnboundedWhenAnyRunBoundedNothing(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 0.0, 0),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.Bounded || findingFor(t, c, "wastedGPUSeconds").Resolved {
		t.Fatalf("an unbounded contributor produced a resolved finding: %+v", c)
	}
	if !strings.Contains(renderComparison(c), "UNBOUNDED") {
		t.Fatal("the render did not say the comparison was unbounded")
	}
}

// Blocked runs -- all of one arm, then all of the other -- are confounded with whatever changed between the
// two blocks, and the comparison has to say so on the finding itself, not only in a header a reader skips.
func TestCompareFlagsArmsThatDidNotAlternate(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, int64(time.Second)),
		cmpRec("a2", "A-honor", "self-completing", "2026-08-19T05:05:00Z", 41.0, int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T09:00:00Z", 0.0, int64(time.Second)),
		cmpRec("b2", "A-ignore", "self-completing", "2026-08-19T09:05:00Z", 0.0, int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.Interleaved {
		t.Fatal("A,A,B,B was reported as interleaved")
	}
	if !strings.Contains(findingFor(t, c, "wastedGPUSeconds").Statement, "CONFOUNDED") {
		t.Fatalf("the confound is missing from the finding a reader quotes: %s",
			findingFor(t, c, "wastedGPUSeconds").Statement)
	}
}

// A record missing its start time cannot rule the confound out, and unknown must read as not interleaved.
func TestCompareTreatsAnUnknownRunOrderAsConfounded(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "", 41.0, int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 0.0, int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.Interleaved {
		t.Fatal("a record with no start time let the comparison claim interleaving")
	}
}

// Refusal, not exclusion: a comparison whose file list does not describe its evidence is the failure this
// tool exists to prevent.
func TestCompareRefusesRatherThanSilentlyDropping(t *testing.T) {
	refused := cmpRec("bad", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 0.0, int64(time.Second))
	refused.Validity = validity{Verdict: verdictRefused, Failures: []string{failureObservation}}
	good := cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, int64(time.Second))
	if _, err := compareRecords([]runRecord{good, refused}); err == nil {
		t.Fatal("a refused record was folded into a comparison")
	}

	// Two regimes measure different quantities, so their difference answers no single question.
	other := cmpRec("gb1", "A-ignore", "grace-bounded", "2026-08-19T05:05:00Z", 51.5, int64(time.Second))
	if _, err := compareRecords([]runRecord{good, other}); err == nil {
		t.Fatal("records from two dose regimes were compared")
	}

	// One arm is not a comparison.
	same := cmpRec("a2", "A-honor", "self-completing", "2026-08-19T05:05:00Z", 41.0, int64(time.Second))
	if _, err := compareRecords([]runRecord{good, same}); err == nil {
		t.Fatal("a single arm produced a comparison")
	}
}

// The render leads with the limits, because a reader who stops after one screen must not leave holding only
// the headline.
func TestRenderComparisonLeadsWithWhatItDoesNotEstablish(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 0.0, int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	out := renderComparison(c)
	noEffect := strings.Index(out, "no effect is claimed")
	headline := strings.Index(out, "wastedGPUSeconds")
	if noEffect < 0 || headline < 0 || noEffect > headline {
		t.Fatalf("the disclaimer must precede the finding:\n%s", out)
	}
}

// A comparison over runs that observed no device must say the GPU-seconds are reservation, on the same
// screen as the numbers. Without it the headline reads as a statement about hardware nothing touched.
func TestCompareStatesThatNoDeviceWasObserved(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 0.0, int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.DeviceEvidence != deviceNotObserved {
		t.Fatalf("device axis = %q over runs on a fake device plugin", c.DeviceEvidence)
	}
	if !strings.Contains(renderComparison(c), "second of RESERVATION") {
		t.Fatalf("the render did not say what the GPU-seconds are:\n%s", renderComparison(c))
	}
}

// One run that observed nothing makes the whole comparison observe nothing: the strongest contributor must
// not launder the weakest one's silence.
func TestCompareTakesTheWeakestDeviceAxis(t *testing.T) {
	strong := cmpRec("a1", "A-honor", "self-completing", "2026-08-19T05:00:00Z", 41.0, int64(time.Second))
	strong.Validity.DeviceEvidence = deviceWorkObserved
	weak := cmpRec("b1", "A-ignore", "self-completing", "2026-08-19T05:05:00Z", 0.0, int64(time.Second))
	c, err := compareRecords([]runRecord{strong, weak})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.DeviceEvidence != deviceNotObserved {
		t.Fatalf("one silent run was laundered into %q", c.DeviceEvidence)
	}
}

// withOwnerWait attaches an owner restoration time to a comparison fixture.
func withOwnerWait(r runRecord, seconds float64) runRecord {
	ns := int64(seconds * float64(time.Second))
	r.Measurement.OwnerAdmitToReadyNs = &ns
	return r
}

// The number a reclaim promise is judged on. The lab measured it from the first run and never carried it out
// of the reconstruction, so answering "how long did the owner wait" meant parsing the ledger by hand -- the
// state this tool exists to end.
func TestCompareReportsWhatTheQuotaOwnerWaited(t *testing.T) {
	c, err := compareRecords([]runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T00:57:03Z", 21.336, 1878*int64(time.Millisecond)), 2.740),
		withOwnerWait(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-20T00:59:29Z", 51.315, 1878*int64(time.Millisecond)), 30.806),
		withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T01:02:21Z", 21.288, 1878*int64(time.Millisecond)), 2.792),
		withOwnerWait(cmpRec("gi2", "A-ignore", "grace-bounded", "2026-08-20T01:04:48Z", 51.278, 1878*int64(time.Millisecond)), 30.807),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	var owner *finding
	for i := range c.Findings {
		if c.Findings[i].Quantity == "ownerAdmitToReadySeconds" {
			owner = &c.Findings[i]
		}
	}
	if owner == nil {
		t.Fatal("the comparison reports the borrower's loss and not the owner's wait")
	}
	if !owner.Resolved {
		t.Fatalf("a 28 s difference against a 3.756 s difference floor did not resolve: %+v", owner)
	}
	if !strings.Contains(owner.Statement, "reclaim promise is judged on") {
		t.Fatalf("the statement does not say which of the two numbers is the promise: %s", owner.Statement)
	}
	for _, a := range c.Arms {
		if a.OwnerWaitRuns != 2 {
			t.Fatalf("arm %s counted %d restored runs, want 2", a.Arm, a.OwnerWaitRuns)
		}
	}
}

// An arm whose owner never came back has no wait to average, and averaging the runs that did would report
// the best case of an arm defined by its worst.
func TestCompareRefusesToAverageAwayAnOwnerThatNeverReturned(t *testing.T) {
	c, err := compareRecords([]runRecord{
		withOwnerWait(cmpRec("h1", "A-honor", "self-completing", "2026-08-20T00:45:14Z", 41.0, int64(time.Second)), 2.5),
		cmpRec("i1", "A-ignore", "self-completing", "2026-08-20T00:47:57Z", 0.0, int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	var owner *finding
	for i := range c.Findings {
		if c.Findings[i].Quantity == "ownerAdmitToReadySeconds" {
			owner = &c.Findings[i]
		}
	}
	if owner == nil || owner.Resolved {
		t.Fatalf("a comparison resolved an owner wait one arm never produced: %+v", owner)
	}
	if !strings.Contains(owner.Statement, "NOT COMPUTED") {
		t.Fatalf("the statement does not say it could not be computed: %s", owner.Statement)
	}
	if !strings.Contains(renderComparison(c), "NONE of 1 runs restored the quota owner") {
		t.Fatalf("the render hides an arm that never restored its owner:\n%s", renderComparison(c))
	}
}

// The check that decides whether the real-GPU session has a baseline at all, with its own positive control
// beside it. A test that only ever reports "no response" would prove nothing, so both arms are asserted: the
// honouring one must come back inside the floor and the ignoring one must come back outside it.
func TestDoseSensitivitySeparatesABaselineFromAQuantityTheDoseDetermines(t *testing.T) {
	honor := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T02:42:43Z", 21.349, 1876*int64(time.Millisecond)), 2.729),
		withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T02:47:58Z", 21.283, 1000*int64(time.Millisecond)), 2.789),
		withOwnerWait(cmpRec("sh1", "A-honor", "self-completing", "2026-08-20T02:44:00Z", 41.503, 1672*int64(time.Millisecond)), 2.589),
		withOwnerWait(cmpRec("sh2", "A-honor", "self-completing", "2026-08-20T02:49:00Z", 41.414, 1762*int64(time.Millisecond)), 2.685),
	}
	d, err := checkSensitivity2(honor)
	if err != nil {
		t.Fatalf("dose sensitivity: %v", err)
	}
	if d.MovesWithDose {
		t.Fatalf("the honouring arm's restoration moved with the dose: %+v", d)
	}
	if !strings.Contains(d.Statement, "not proof that it does not") {
		t.Fatalf("a negative result was stated as proof of no response: %s", d.Statement)
	}

	// Positive control. In the ignoring arm the owner's wait IS the dose-dependent quantity -- what the victim
	// had left, bounded by grace -- so a check that could not see this one respond could not see anything.
	ignore := []runRecord{
		withOwnerWait(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-20T02:45:05Z", 51.262, 1938*int64(time.Millisecond)), 30.846),
		withOwnerWait(cmpRec("gi2", "A-ignore", "grace-bounded", "2026-08-20T02:50:20Z", 51.233, 1000*int64(time.Millisecond)), 30.891),
		withOwnerWait(cmpRec("si1", "A-ignore", "self-completing", "2026-08-20T02:46:00Z", 0.0, 1280*int64(time.Millisecond)), 19.665),
		withOwnerWait(cmpRec("si2", "A-ignore", "self-completing", "2026-08-20T02:51:00Z", 0.0, 2364*int64(time.Millisecond)), 19.733),
	}
	di, err := checkSensitivity2(ignore)
	if err != nil {
		t.Fatalf("dose sensitivity: %v", err)
	}
	if !di.MovesWithDose {
		t.Fatalf("an 11 s response against a 2.364 s floor was not seen; the check cannot detect anything: %+v", di)
	}
	if !strings.Contains(di.Statement, "cannot be quoted as a property of the platform") {
		t.Fatalf("the statement does not say what the response costs: %s", di.Statement)
	}
}

// Holding the arm fixed is the whole method. Pooling arms would measure the arm difference and report it as a
// dose response, which is exactly the confusion the GPU session's baseline must not inherit.
func TestDoseSensitivityRefusesToPoolArms(t *testing.T) {
	mixed := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T02:42:43Z", 21.3, int64(time.Second)), 2.729),
		withOwnerWait(cmpRec("si1", "A-ignore", "self-completing", "2026-08-20T02:46:00Z", 0.0, int64(time.Second)), 19.665),
	}
	if _, err := checkSensitivity2(mixed); err == nil {
		t.Fatal("two arms were pooled into a dose response")
	}

	// One regime is not a variation.
	same := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T02:42:43Z", 21.3, int64(time.Second)), 2.729),
		withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T02:47:58Z", 21.2, int64(time.Second)), 2.789),
	}
	if _, err := checkSensitivity2(same); err == nil {
		t.Fatal("a single dose regime produced a dose-sensitivity result")
	}

	// A regime where the owner never came back has no wait to test, and averaging the other regime's runs
	// would answer a question nobody asked.
	absent := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T02:42:43Z", 21.3, int64(time.Second)), 2.729),
		cmpRec("sh1", "A-honor", "self-completing", "2026-08-20T02:44:00Z", 41.5, int64(time.Second)),
	}
	if _, err := checkSensitivity2(absent); err == nil {
		t.Fatal("a regime whose owner never returned was folded into a dose response")
	}
}

// A difference between two arms carries BOTH of their errors, and they can lean opposite ways, so the
// resolution of the difference is the sum of the two floors and never the larger.
//
// Mutation that turns this red: return max(a.FloorSeconds, b.FloorSeconds) from differenceFloor.
func TestTheDifferenceFloorIsTheSumOfBothArms(t *testing.T) {
	c, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-20T05:00:00Z", 10.0, 2*int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-20T05:05:00Z", 7.0, 2*int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	// 3 s of difference against two 2 s arms. Under the larger-of-the-two rule this resolved; it must not.
	if findingFor(t, c, "wastedGPUSeconds").Resolved {
		t.Fatal("a 3 s difference resolved against two arms each bounded at 2 s; the error budget is 4 s")
	}
	// And 5 s does clear it, so the rule is a bound rather than a refusal to ever conclude.
	c2, err := compareRecords([]runRecord{
		cmpRec("a1", "A-honor", "self-completing", "2026-08-20T05:00:00Z", 10.0, 2*int64(time.Second)),
		cmpRec("b1", "A-ignore", "self-completing", "2026-08-20T05:05:00Z", 5.0, 2*int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !findingFor(t, c2, "wastedGPUSeconds").Resolved {
		t.Fatal("a 5 s difference did not clear a 4 s budget; the floor has stopped being a bound")
	}
}

// A mean over the runs whose owner came back omits the slow tail, and the tail is exactly what a reader
// worried about an arm wants to see. The figures stay; the verdict does not.
func TestASurvivorMeanIsNeverResolved(t *testing.T) {
	c, err := compareRecords([]runRecord{
		withOwnerWait(cmpRec("a1", "A-honor", "self-completing", "2026-08-20T05:00:00Z", 41.0, int64(time.Second)), 2.5),
		withOwnerWait(cmpRec("a2", "A-honor", "self-completing", "2026-08-20T05:10:00Z", 41.0, int64(time.Second)), 2.6),
		withOwnerWait(cmpRec("b1", "A-ignore", "self-completing", "2026-08-20T05:05:00Z", 0.0, int64(time.Second)), 30.0),
		cmpRec("b2", "A-ignore", "self-completing", "2026-08-20T05:15:00Z", 0.0, int64(time.Second)),
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	owner := findingFor(t, c, "ownerAdmitToReadySeconds")
	if owner.Resolved {
		t.Fatal("a 27 s difference resolved while one arm's mean was over its survivors only")
	}
	if !strings.Contains(owner.Statement, "SURVIVOR MEAN") {
		t.Fatalf("the statement does not name the bias: %s", owner.Statement)
	}
}

// The dose check refuses a regime that did not restore its owner every time, rather than reporting a mean
// with a known direction of bias. It licenses a baseline for a session nobody can re-run cheaply.
func TestDoseSensitivityRefusesASurvivorMean(t *testing.T) {
	recs := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.7),
		withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T05:10:00Z", 21.3, int64(time.Second)), 2.8),
		withOwnerWait(cmpRec("sh1", "A-honor", "self-completing", "2026-08-20T05:05:00Z", 41.5, int64(time.Second)), 2.6),
		cmpRec("sh2", "A-honor", "self-completing", "2026-08-20T05:15:00Z", 41.4, int64(time.Second)),
	}
	if _, err := checkSensitivity2(recs); err == nil {
		t.Fatal("a regime that restored its owner once out of twice produced a dose verdict")
	}
}

// Blocked regimes are confounded with whatever changed between the blocks, and this document had no such
// check while the arm comparison did — in the direction where its absence hurts most, since "no response" is
// the verdict that licenses a baseline.
func TestDoseSensitivityReportsWhetherTheRegimesAlternated(t *testing.T) {
	blocked := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.7),
		withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T05:05:00Z", 21.3, int64(time.Second)), 2.8),
		withOwnerWait(cmpRec("sh1", "A-honor", "self-completing", "2026-08-20T09:00:00Z", 41.5, int64(time.Second)), 2.6),
		withOwnerWait(cmpRec("sh2", "A-honor", "self-completing", "2026-08-20T09:05:00Z", 41.4, int64(time.Second)), 2.65),
	}
	d, err := checkSensitivity2(blocked)
	if err != nil {
		t.Fatalf("dose sensitivity: %v", err)
	}
	if d.Interleaved {
		t.Fatal("two blocks of runs were reported as alternating")
	}
	if !strings.Contains(renderDoseSensitivity(d), "NOT INTERLEAVED") {
		t.Fatalf("the render hides the confound:\n%s", renderDoseSensitivity(d))
	}

	// And the alternating case is reported as such, or the check is a constant.
	alternating := []runRecord{
		withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.7),
		withOwnerWait(cmpRec("sh1", "A-honor", "self-completing", "2026-08-20T05:05:00Z", 41.5, int64(time.Second)), 2.6),
		withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T05:10:00Z", 21.3, int64(time.Second)), 2.8),
		withOwnerWait(cmpRec("sh2", "A-honor", "self-completing", "2026-08-20T05:15:00Z", 41.4, int64(time.Second)), 2.65),
	}
	da, err := checkSensitivity2(alternating)
	if err != nil {
		t.Fatalf("dose sensitivity: %v", err)
	}
	if !da.Interleaved {
		t.Fatal("alternating regimes were reported as blocked")
	}
}

// nodeRec puts a record on a named worker, which the baseline and the node-sensitivity check group by.
func nodeRec(r runRecord, node string) runRecord {
	r.Qualification = &qualification{Node: node}
	return r
}

// The last hand-typed number in this lab, as an artifact. It refuses more than the comparisons do, because a
// baseline is quoted long after its runs are gone by someone who will not re-derive it — which is exactly
// what happened to the figure this replaces.
func TestBaselineNamesEverythingBehindIt(t *testing.T) {
	recs := []runRecord{
		nodeRec(withOwnerWait(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, 1876*int64(time.Millisecond)), 2.729), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("sh1", "A-honor", "self-completing", "2026-08-20T05:05:00Z", 41.5, 1672*int64(time.Millisecond)), 2.589), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("gh2", "A-honor", "grace-bounded", "2026-08-20T05:10:00Z", 21.3, 1000*int64(time.Millisecond)), 2.789), "platform-worker2"),
		nodeRec(withOwnerWait(cmpRec("sh2", "A-honor", "self-completing", "2026-08-20T05:15:00Z", 41.4, 1762*int64(time.Millisecond)), 2.685), "platform-worker2"),
	}
	b, err := computeBaseline(recs)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if b.N != 4 || len(b.RunIDs) != 4 {
		t.Fatalf("n=%d runs=%v, want all four named", b.N, b.RunIDs)
	}
	if len(b.Doses) != 2 || len(b.Nodes) != 2 {
		t.Fatalf("doses=%v nodes=%v; a baseline must name what it spans", b.Doses, b.Nodes)
	}
	if b.OwnerWaitSecondsMin != 2.589 || b.OwnerWaitSecondsMax != 2.789 {
		t.Fatalf("min=%v max=%v, want the observed extremes", b.OwnerWaitSecondsMin, b.OwnerWaitSecondsMax)
	}
	if b.DeviceEvidence != deviceNotObserved {
		t.Fatalf("device axis = %q on a fake device plugin", b.DeviceEvidence)
	}
	out := renderBaseline(b)
	if !strings.Contains(out, "control-plane figure and not a statement about hardware") {
		t.Fatalf("the render lets a fake-plugin baseline read as hardware:\n%s", out)
	}
	if !strings.Contains(b.Statement, "add its own floor") {
		t.Fatalf("the statement does not tell a later session how to difference against it: %s", b.Statement)
	}
}

// A run whose owner never came back cannot contribute to a floor: averaging the ones that did produces the
// platform's best case wearing the name of its baseline.
func TestBaselineRefusesARunThatNeverRestoredItsOwner(t *testing.T) {
	recs := []runRecord{
		nodeRec(withOwnerWait(cmpRec("h1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.7), "platform-worker"),
		nodeRec(cmpRec("h2", "A-honor", "grace-bounded", "2026-08-20T05:10:00Z", 21.3, int64(time.Second)), "platform-worker"),
	}
	if _, err := computeBaseline(recs); err == nil {
		t.Fatal("a baseline was pooled over a run whose owner never returned")
	}

	// And two arms are never one baseline.
	mixed := []runRecord{
		nodeRec(withOwnerWait(cmpRec("h1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.7), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("i1", "A-ignore", "grace-bounded", "2026-08-20T05:10:00Z", 51.3, int64(time.Second)), 30.8), "platform-worker"),
	}
	if _, err := computeBaseline(mixed); err == nil {
		t.Fatal("two arms were averaged into one baseline, erasing the difference they exist to show")
	}
}

// The node is the factor "one cluster, one pinned worker" left unqualified on every figure this lab has
// produced. Varying it requires holding the dose fixed, or a node difference gets published as a dose one.
func TestNodeSensitivityHoldsTheDoseFixed(t *testing.T) {
	varied := []runRecord{
		nodeRec(withOwnerWait(cmpRec("a", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.7), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("b", "A-honor", "self-completing", "2026-08-20T05:05:00Z", 41.5, int64(time.Second)), 2.6), "platform-worker2"),
	}
	if _, err := checkSensitivity(varied, factorNode); err == nil {
		t.Fatal("the node and the dose varied together and a node response was reported")
	}

	held := []runRecord{
		nodeRec(withOwnerWait(cmpRec("a1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 2.70), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("b1", "A-honor", "grace-bounded", "2026-08-20T05:05:00Z", 21.3, int64(time.Second)), 2.75), "platform-worker2"),
		nodeRec(withOwnerWait(cmpRec("a2", "A-honor", "grace-bounded", "2026-08-20T05:10:00Z", 21.3, int64(time.Second)), 2.72), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("b2", "A-honor", "grace-bounded", "2026-08-20T05:15:00Z", 21.3, int64(time.Second)), 2.78), "platform-worker2"),
	}
	d, err := checkSensitivity(held, factorNode)
	if err != nil {
		t.Fatalf("node sensitivity: %v", err)
	}
	if d.Factor != factorNode || len(d.Doses) != 2 {
		t.Fatalf("factor=%q levels=%d, want node over two workers", d.Factor, len(d.Doses))
	}
	if d.MovesWithDose {
		t.Fatalf("a 60 ms difference between nodes resolved against a %.3f s floor", d.FloorSeconds)
	}
	if !strings.Contains(renderDoseSensitivity(d), "SENSITIVITY TO NODE") {
		t.Fatalf("the render does not name the factor:\n%s", renderDoseSensitivity(d))
	}
}

// holdRec gives a record a ledger the hold and dose can be read from.
//
// The events are the shape the recorded runs take: the victim becomes Ready, the owner is submitted a dose
// later, Kueue admits it, and the victim's Pod reaches a terminal phase after holding the device.
func holdRec(r runRecord, doseSeconds, holdSeconds float64) runRecord {
	const victimReady = 2 * float64(time.Second)
	submitted := victimReady + doseSeconds*float64(time.Second)
	admitted := submitted + 1.1*float64(time.Second)
	r.Events = []queuelab.LifecycleEvent{
		{Job: queuelab.VictimRow, Type: queuelab.EventPodReady, ElapsedNs: int64(victimReady)},
		{Job: queuelab.OwnerRow, Type: queuelab.EventSubmitted, ElapsedNs: int64(submitted)},
		{Job: queuelab.OwnerRow, Type: queuelab.EventAdmitted, ElapsedNs: int64(admitted)},
		{Job: queuelab.VictimRow, Type: queuelab.EventAttemptStopped,
			ElapsedNs: int64(admitted + holdSeconds*float64(time.Second))},
	}
	// Both of the hold's endpoints must carry a skew, or the floor cannot be derived and the check refuses.
	// They must also carry the components' own stamps: the hold is read on two clocks and a run offering only
	// one is refused, so a fixture without them would be testing the refusal rather than the model.
	//
	// The stamps are the arrival figure TRUNCATED to the second, which is what a real run produces -- both
	// components serialise through metav1.Time -- so the two readings agree to within truncation exactly as
	// the recorded runs do.
	stampBase := int64(1_700_000_000_000_000_000)
	stampHoldSeconds := int64(holdSecondsOf(r))
	for i := range r.Events {
		e := &r.Events[i]
		if e.Type == queuelab.EventAdmitted || e.Type == queuelab.EventAttemptStopped {
			v := int64(200 * time.Millisecond)
			stamp := stampBase
			if e.Type == queuelab.EventAttemptStopped {
				v = int64(700 * time.Millisecond)
				stamp = stampBase + stampHoldSeconds*int64(time.Second)
			}
			e.ObservedSkewNs = &v
			st := stamp
			e.ComponentStampUnixNanos = &st
		}
	}
	r.Qualification = &qualification{Node: "platform-worker"}
	return r
}

// holdSecondsOf is the whole-second part of the hold this fixture was built with, which is what a
// second-truncated component stamp would report.
func holdSecondsOf(r runRecord) int {
	h := queuelab.DeviceHoldNs(r.Events)
	if h == nil {
		return 0
	}
	return int(*h / int64(time.Second))
}

// The lab's central claim, tested on the quantity the claim is about.
//
// The honouring arm is a CONTROL rather than a subtrahend: its hold is near zero, which is what shows the
// interval contains no scheduling or container start. Nothing is subtracted from anything.
//
// Mutation that turns this red: subtract the control from the prediction, or take the declared dose instead
// of the achieved one.
func TestTheModelPredictsTheDeviceHoldFromOneRule(t *testing.T) {
	// Achieved doses overrun their declared 20 and 40 by 1.2 s, as every recorded run does. Remaining service
	// is then 38.8 (grace binds, hold 30) and 18.8 (service binds, hold 18.8).
	recs := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 30.050),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	m, err := checkModel(recs)
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if !m.Holds {
		t.Fatalf("the model failed on data generated from it: %+v", m.Cases)
	}
	byDose := map[string]modelCase{}
	for _, c := range m.Cases {
		byDose[c.Dose] = c
	}
	if byDose["grace-bounded"].BindingTerm != "termination grace" {
		t.Fatalf("38.8 s of remaining service against a 30 s grace must bind on grace: %+v", byDose["grace-bounded"])
	}
	if byDose["self-completing"].BindingTerm != "remaining service" {
		t.Fatalf("18.8 s of remaining service must bind on service: %+v", byDose["self-completing"])
	}
	// The achieved dose is used, not the declared one — 20 would put remaining at 40 and predict the same
	// hold, but 40 declared against 41.1 achieved moves the prediction by 1.1 s and the test would fail.
	if got := byDose["self-completing"].AchievedDoseSeconds; got < 41 || got > 41.2 {
		t.Fatalf("achieved dose = %v, want the measured 41.1 rather than the declared 40", got)
	}
	// The observed hold is the raw measurement. Nothing is subtracted from it -- the control exists to show
	// the interval contains no platform work, not to correct it, and an earlier version of this check
	// subtracted a term it had borrowed from the other arm.
	if got := byDose["grace-bounded"].ObservedSeconds; got < 30.04 || got > 30.06 {
		t.Fatalf("observed hold = %v, want the raw 30.050; something was subtracted from it", got)
	}
	if m.ControlRuns != 2 || m.ControlHoldSeconds > 0.1 {
		t.Fatalf("control = %.3f s over %d runs; the honouring arm must hold the device for approximately "+
			"nothing, which is what shows the interval contains no platform work",
			m.ControlHoldSeconds, m.ControlRuns)
	}
}

// The contrast is the only part of the check that tests the model's kink. Anything common to both regimes
// cancels in it, so a rule that happened to fit both levels by coincidence cannot fit their difference.
func TestTheModelTestsTheContrastBetweenRegimes(t *testing.T) {
	recs := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 30.050),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	m, err := checkModel(recs)
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if m.Contrast == nil {
		t.Fatal("both regimes were present and no contrast was computed; the levels alone can be fitted by a " +
			"rule that just happens to land there")
	}
	// grace 30 against remaining 18.9: the kink says the gap is 11.1 s.
	if d := m.Contrast.PredictedSeconds; d < 11 || d > 11.2 {
		t.Fatalf("predicted contrast = %v, want about 11.1 (30 - 18.9)", d)
	}
	if !m.Contrast.InsideFloor {
		t.Fatalf("the contrast fell outside the floor on data generated from the model: %+v", m.Contrast)
	}
	if !strings.Contains(renderModel(m), "the kink") {
		t.Fatalf("the render does not say what the contrast is for:\n%s", renderModel(m))
	}
}

// And it can fail. A refutation nobody can trigger is not one.
func TestTheModelCanBeRefuted(t *testing.T) {
	recs := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		// The borrower held the device for twice the grace period.
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 60.0),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	m, err := checkModel(recs)
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if m.Holds {
		t.Fatal("a borrower that held the device for twice the grace period did not refute the model")
	}
	if !strings.Contains(m.Statement, "REFUTED") {
		t.Fatalf("the statement does not say it was refuted: %s", m.Statement)
	}
}

// The statement must not claim more than two runs per cell evaluated in-sample can support.
func TestTheModelClaimsConsistencyRatherThanValidation(t *testing.T) {
	recs := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 30.050),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	m, err := checkModel(recs)
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if !strings.Contains(m.Statement, "consistency") || strings.Contains(m.Statement, "VALIDATED") {
		t.Fatalf("the statement overclaims what two runs per cell, evaluated on themselves, support: %s",
			m.Statement)
	}
	// The two retractions a review forced, each defended here so the statement cannot slide back.
	//
	// The control is a few tens of milliseconds against a floor of seconds, so it is UNRESOLVED. The
	// statement used to say it "shows the interval contains no platform work", which is exactly the
	// inversion resolution.go exists to prevent: "the harness cannot see this" printed as "the harness
	// measured this". The whole model chain rests on that one figure.
	if !strings.Contains(m.Statement, "unresolved") {
		t.Fatalf("the control is printed without saying it is below the floor, so a reader takes a number "+
			"this harness cannot resolve as a measured near-zero: %s", m.Statement)
	}
	// And the self-completing cell's prediction is built from its own achieved dose, so its residual reduces
	// to an instrumentation offset -- every term of the rule cancels out of it. A statement that presents it
	// as a test of min(remaining service, grace) is claiming a cell that cannot refute anything.
	if !strings.Contains(m.Statement, "instrumentation offset") {
		t.Fatalf("the statement presents the self-completing cell as a test of the rule, when its prediction "+
			"is built from the same run's own timings: %s", m.Statement)
	}
}

// The hold's endpoints are arrival times from two watches, so pooling nodes pools two collectors' delivery
// behaviour into an interval whose entire value is that it is quiet.
func TestTheModelRefusesToPoolNodes(t *testing.T) {
	// A complete set, so the ONLY thing wrong is the node. An earlier version of this test used a single
	// regime and passed on the "needs both dose regimes" refusal instead -- it asserted that an error came
	// back without checking which one, and a mutation removing the node guard survived it.
	one := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 30.050),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	if _, err := checkModel(one); err != nil {
		t.Fatalf("the single-node fixture must pass, or this test proves nothing: %v", err)
	}
	mixed := append([]runRecord{}, one...)
	mixed[3].Qualification = &qualification{Node: "platform-worker2"}
	_, err := checkModel(mixed)
	if err == nil {
		t.Fatal("two nodes were pooled into one hold measurement")
	}
	if !strings.Contains(err.Error(), "node") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// The levels can both land inside the floor while their DIFFERENCE does not, and that case is the whole
// reason the contrast is computed: a rule fitted to two levels by coincidence cannot also fit their gap.
func TestTheContrastFailsWhereTheLevelsPass(t *testing.T) {
	// Each level is 1.4 s off its prediction -- inside the ~3 s floor -- but in OPPOSITE directions, so the
	// contrast is 2.8 s out. Widen it until it clears the floor while each level stays within.
	recs := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 32.9),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 16.0),
	}
	m, err := checkModel(recs)
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	for _, c := range m.Cases {
		if !c.InsideFloor {
			t.Fatalf("the fixture is meant to keep every LEVEL inside the floor: %s residual %+.3f against %.3f",
				c.Dose, c.ResidualSeconds, m.FloorSeconds)
		}
	}
	if m.Contrast == nil || m.Contrast.InsideFloor {
		t.Fatalf("levels that each land inside the floor with opposite errors must fail the contrast: %+v",
			m.Contrast)
	}
	if m.Holds {
		t.Fatal("the check reported the model holding while its kink was refuted")
	}
}

// The statement's spread is in milliseconds and the field is in seconds, which is exactly the kind of unit
// slip that ships. It shipped once: a 930 ms spread printed as "a spread of 1 ms" in the sentence a reader
// quotes, beside the correct figure in the line below it.
func TestBaselineStatementReportsTheSpreadInMilliseconds(t *testing.T) {
	recs := []runRecord{
		nodeRec(withOwnerWait(cmpRec("h1", "A-honor", "grace-bounded", "2026-08-20T05:00:00Z", 21.3, int64(time.Second)), 1.688), "platform-worker"),
		nodeRec(withOwnerWait(cmpRec("h2", "A-honor", "grace-bounded", "2026-08-20T05:10:00Z", 21.3, int64(time.Second)), 2.619), "platform-worker"),
	}
	b, err := computeBaseline(recs)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if !strings.Contains(b.Statement, "931 ms") {
		t.Fatalf("the statement's spread is not the 931 ms the runs show: %s", b.Statement)
	}
}

// stampedHoldRec gives a record a ledger whose two readings of the hold can be made to disagree.
func stampedHoldRec(r runRecord, doseSeconds, arrivalHold, stampHold float64) runRecord {
	r = holdRec(r, doseSeconds, arrivalHold)
	base := int64(1_700_000_000_000_000_000)
	for i := range r.Events {
		e := &r.Events[i]
		switch e.Type {
		case queuelab.EventAdmitted:
			v := base
			e.ComponentStampUnixNanos = &v
		case queuelab.EventAttemptStopped:
			v := base + int64(stampHold*float64(time.Second))
			e.ComponentStampUnixNanos = &v
		}
	}
	return r
}

// Two readings of one interval that describe different intervals mean neither can be published. The check was
// promised in a comment beside the second reading and performed nowhere, so a clock step would have corrupted
// it in silence.
//
// Mutation that turns this red: drop the ClocksDisagree call, or widen the tolerance without bound.
func TestARunWhoseTwoClocksDisagreeIsRefused(t *testing.T) {
	agree := []runRecord{
		stampedHoldRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045, 0),
		stampedHoldRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 30.050, 30),
		stampedHoldRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045, 0),
		stampedHoldRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850, 18),
	}
	if _, err := checkModel(agree); err != nil {
		t.Fatalf("readings agreeing within truncation must pass, or this tests nothing: %v", err)
	}

	// The stamps now say the hold was twelve seconds while the arrivals say thirty. Truncation cannot span
	// that, so the two instruments are describing different intervals.
	disagree := append([]runRecord{}, agree...)
	disagree[1] = stampedHoldRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0),
		21.2, 30.050, 12)
	_, err := checkModel(disagree)
	if err == nil {
		t.Fatal("a run whose clocks disagreed by eighteen seconds was folded into the model")
	}
	if !strings.Contains(err.Error(), "different intervals") {
		t.Fatalf("the refusal does not say what the disagreement means: %v", err)
	}
}

// "The hold is read on two clocks" has to mean two readings EXIST, not merely that two agree when both
// happen to be present. A run carrying one reading has an arrival figure checked against nothing, and
// ClocksDisagree answers "no disagreement" for it — the honest answer to the question it was asked, and the
// wrong basis for admitting the run.
//
// Mutation that turns this red: drop the DeviceHoldStampNs nil check and rely on ClocksDisagree alone.
func TestARunReadOnOneClockIsRefused(t *testing.T) {
	full := []runRecord{
		stampedHoldRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-22T05:00:00Z", 21.3, 0), 21.2, 0.045, 0),
		stampedHoldRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-22T05:05:00Z", 51.3, 0), 21.2, 30.050, 30),
		stampedHoldRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-22T05:10:00Z", 41.4, 0), 41.1, 0.045, 0),
		stampedHoldRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-22T05:15:00Z", 0.0, 0), 41.1, 18.850, 18),
	}
	if _, err := checkModel(full); err != nil {
		t.Fatalf("a complete set must pass, or this tests nothing: %v", err)
	}

	// Strip the victim's stop stamp from one run: the arrival figure survives, the second reading does not.
	oneClock := append([]runRecord{}, full...)
	stripped := stampedHoldRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-22T05:05:00Z", 51.3, 0),
		21.2, 30.050, 30)
	for i := range stripped.Events {
		if stripped.Events[i].Type == queuelab.EventAttemptStopped {
			stripped.Events[i].ComponentStampUnixNanos = nil
		}
	}
	oneClock[1] = stripped
	_, err := checkModel(oneClock)
	if err == nil {
		t.Fatal("a run whose hold was read on one clock entered the model; its arrival figure was corroborated " +
			"by nothing")
	}
	if !strings.Contains(err.Error(), "nothing corroborates") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}

// The resolution rule applies to the baseline too, and it did not until a hostile review found the asymmetry.
//
// The model check refuses to read the honouring arm's 0.041 s hold as a measured near-zero because it sits
// under the floor. The baseline printed a mean under ITS floor as though it were measured -- and a baseline
// is exactly the figure a later session differences against, so an unresolved one propagates into a
// comparison nobody can support.
func TestTheBaselineAppliesTheResolutionRuleToItself(t *testing.T) {
	b := baseline{
		Arm: "A-honor", N: 4,
		OwnerWaitSecondsMean: 2.195, OwnerWaitSecondsMin: 1.208, OwnerWaitSecondsMax: 3.210,
		OwnerWaitSpreadSeconds: 2.003,
		FloorSeconds:           3.106,
	}
	b.Resolved = queuelab.ResolvesAgainst(queuelab.SecondsToNs(b.OwnerWaitSecondsMean), queuelab.SecondsToNs(b.FloorSeconds))
	if b.Resolved {
		t.Fatal("a mean of 2.195 s was called resolved against a 3.106 s floor")
	}

	// And one that clears its floor must NOT be qualified, or the warning becomes noise a reader learns to
	// skip past on the figures it does apply to.
	ok := baseline{Arm: "A-ignore", OwnerWaitSecondsMean: 31.213, FloorSeconds: 3.338}
	ok.Resolved = queuelab.ResolvesAgainst(queuelab.SecondsToNs(ok.OwnerWaitSecondsMean), queuelab.SecondsToNs(ok.FloorSeconds))
	if !ok.Resolved {
		t.Fatal("a mean of 31.213 s was called unresolved against a 3.338 s floor")
	}
}

// Dispersion is judged against what the harness can see, not against the figure's own size.
func TestDispersionIsJudgedAgainstTheFloorRatherThanTheMean(t *testing.T) {
	// The committed honouring arm: 1.208 s to 3.210 s looks alarming and is entirely inside the floor, so
	// the runs do not disagree about anything this harness could have seen.
	tight := baseline{Arm: "A-honor", OwnerWaitSecondsMean: 2.195, OwnerWaitSpreadSeconds: 2.003, FloorSeconds: 3.106}
	if got := renderBaseline(tight); strings.Contains(got, "DISPERSED") {
		t.Errorf("a spread inside the floor was called dispersed; comparing it to the mean instead of the "+
			"instrument is a ratio with no instrument in it:\n%s", got)
	}

	// Runs that disagree by more than the floor disagree about something nothing controlled.
	loose := baseline{Arm: "A-honor", OwnerWaitSecondsMean: 6.0, OwnerWaitSpreadSeconds: 9.0, FloorSeconds: 3.106}
	if got := renderBaseline(loose); !strings.Contains(got, "DISPERSED") {
		t.Errorf("a spread of 9 s against a 3.106 s floor was not flagged:\n%s", got)
	}
}

// The model check pools two dose regimes, so it has to say when they were not interleaved.
//
// Every other comparison in this file checked it and this one did not, which an independent review found.
// It is the comparison carrying the headline: if the regimes were run in blocks rather than alternating,
// anything that drifted over the session lands entirely on one of them and arrives as the kink. The
// committed set IS interleaved, so the warning correctly stays silent there -- which is exactly why the
// omission survived.
func TestTheModelCheckSaysWhenTheRegimesWereNotInterleaved(t *testing.T) {
	blocked := modelCheck{
		Interleaved:     false,
		FloorSeconds:    2.940,
		ControlResolved: false,
	}
	if got := renderModel(blocked); !strings.Contains(got, "NOT INTERLEAVED") {
		t.Errorf("a blocked model check printed no warning, so a reader takes a kink that could be session "+
			"drift for a property of the protocol:\n%s", got)
	}

	interleaved := modelCheck{Interleaved: true, FloorSeconds: 2.940}
	if got := renderModel(interleaved); strings.Contains(got, "NOT INTERLEAVED") {
		t.Errorf("an interleaved model check was warned about; the warning becomes noise a reader skips on "+
			"the runs where it matters:\n%s", got)
	}

	// And the flag has to be COMPUTED from the runs, not merely rendered. The first version of this test
	// built modelCheck structs by hand, so hardcoding Interleaved = true in checkModel passed it -- the
	// renderer was proven correct about a field nothing derived. That is the same shape as a helper that is
	// correct while its caller never calls it, which this repository has been bitten by more than once.
	//
	// Same four runs both times; only the ORDER they were started in differs.
	alternating := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:05:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:10:00Z", 51.3, 0), 21.2, 30.050),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	if m, err := checkModel(alternating); err != nil {
		t.Fatalf("model over alternating runs: %v", err)
	} else if !m.Interleaved {
		t.Error("runs that alternate by dose were reported as blocked")
	}

	blockedRuns := []runRecord{
		holdRec(cmpRec("gh1", "A-honor", "grace-bounded", "2026-08-21T05:00:00Z", 21.3, 0), 21.2, 0.045),
		holdRec(cmpRec("gi1", "A-ignore", "grace-bounded", "2026-08-21T05:05:00Z", 51.3, 0), 21.2, 30.050),
		holdRec(cmpRec("sh1", "A-honor", "self-completing", "2026-08-21T05:10:00Z", 41.4, 0), 41.1, 0.045),
		holdRec(cmpRec("si1", "A-ignore", "self-completing", "2026-08-21T05:15:00Z", 0.0, 0), 41.1, 18.850),
	}
	if m, err := checkModel(blockedRuns); err != nil {
		t.Fatalf("model over blocked runs: %v", err)
	} else if m.Interleaved {
		t.Error("both grace-bounded runs were taken before both self-completing ones and the check called " +
			"that interleaved; session drift would land entirely on one regime and arrive as the kink")
	}
}
