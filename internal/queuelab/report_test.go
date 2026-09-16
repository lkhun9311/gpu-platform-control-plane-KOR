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

package queuelab

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRenderResultAlwaysExposesExecutionStart is the regression guard for the published overclaim: a report
// that shows only admission lets "the owner was admitted in 120 ms" stand for "the owner was running".
func TestRenderResultAlwaysExposesExecutionStart(t *testing.T) {
	res, err := Reconstruct("Any", reclaimAnyTrace(), ineffectivePreemptionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderResult(res)
	for _, want := range []string{"admitLatency", "readyLatency", "admitToReady"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered report is missing %q:\n%s", want, out)
		}
	}
	assertTimingLinesAdjacent(t, out, len(res.Outcomes))
}

// assertTimingLinesAdjacent checks per-line adjacency rather than aggregate label counts, because two counts
// that match across the whole output can still hide a refactor that moved admissions and execution starts
// into separate sections: the figures must sit on the SAME line to keep the framing this guard exists for.
func assertTimingLinesAdjacent(t *testing.T, out string, wantLines int) {
	t.Helper()
	timingLines := 0
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(line, "admitLatency=") {
			continue
		}
		timingLines++
		if !strings.Contains(line, "readyLatency=") || !strings.Contains(line, "admitToReady=") {
			t.Fatalf("line reports admitLatency without readyLatency and admitToReady beside it: %q", line)
		}
	}
	if timingLines != wantLines {
		t.Fatalf("got %d timing lines, want %d (one per outcome, none silently dropped):\n%s", timingLines, wantLines, out)
	}
}

// TestRenderResultExposesExecutionStartForUnexecutedRows guards the row a refactor is most likely to drop
// silently: one that was offered but never admitted and never executed, so it has no execution timing to
// report. Its missing execution is exactly the fact the guard exists to keep visible, not to hide.
func TestRenderResultExposesExecutionStartForUnexecutedRows(t *testing.T) {
	events := ineffectivePreemptionEvents()
	filtered := events[:0]
	for _, e := range events {
		if e.Job == "b1" && (e.Type == EventAdmitted || e.Type == EventPodReady || e.Type == EventCompleted) {
			continue
		}
		filtered = append(filtered, e)
	}
	res, err := Reconstruct("Any", reclaimAnyTrace(), filtered, 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var b1 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "b1" {
			b1 = o
		}
	}
	if b1.Admitted || b1.Executed {
		t.Fatalf("b1 must be offered but never admitted and never executed for this test to be meaningful: %+v", b1)
	}
	out := RenderResult(res)
	assertTimingLinesAdjacent(t, out, len(res.Outcomes))
	// Ensure censoredWait is rendered for the never-admitted row with its caveat text.
	if !strings.Contains(out, "censoredWait=") || !strings.Contains(out, "(never admitted by horizon)") {
		t.Fatalf("censoredWait with caveat must be rendered for never-admitted rows:\n%s", out)
	}
}

func TestRenderResultSurfacesIneffectivePreemption(t *testing.T) {
	res, err := Reconstruct("Any", reclaimAnyTrace(), ineffectivePreemptionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderResult(res)
	if !strings.Contains(out, "PREEMPTION INEFFECTIVE") {
		t.Fatalf("an ineffective preemption must be stated loudly:\n%s", out)
	}
	if !strings.Contains(out, "unattributedOccupancy") {
		t.Fatalf("unattributed occupancy must be reported:\n%s", out)
	}
	// The per-row flag must be pinned on the row that carries it. The earlier form searched the timing line
	// for the job name, which that line does not contain, so its failure branch was unreachable and deleting
	// the field from the renderer would not have failed any test.
	foundIneffectiveRow := false
	for _, o := range res.Outcomes {
		if !o.PreemptionIneffective {
			continue
		}
		foundIneffectiveRow = true
		if !hasLineWith(out, o.Job, "preemptionIneffective=true") {
			t.Fatalf("job %s had ineffective preemption but no row line states preemptionIneffective=true:\n%s", o.Job, out)
		}
	}
	if !foundIneffectiveRow {
		t.Fatalf("no row with ineffective preemption found in outcomes, test setup may be broken: %+v", res.Outcomes)
	}
	// A row without the flag must say so on its own line, so the field is pinned as a per-row value rather
	// than as a string that merely appears somewhere in the output.
	for _, o := range res.Outcomes {
		if !o.PreemptionIneffective && !hasLineWith(out, o.Job, "preemptionIneffective=false") {
			t.Fatalf("row %s should render preemptionIneffective=false:\n%s", o.Job, out)
		}
	}
}

// hasLineWith reports whether any single line of out contains all the given substrings, which is how a
// per-row field is pinned to its own row rather than to the output as a whole.
func hasLineWith(out string, substrings ...string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		all := true
		for _, s := range substrings {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestRenderResultSummarizesOccupancyAndReExecution(t *testing.T) {
	// The run-level summary is what a reader takes away, and hiding re-execution there is exactly what the
	// published run did: the header showed admissions and waste with no sign that a completed attempt had
	// been re-run from zero.
	res, err := Reconstruct("Any", reclaimAnyTrace(), reExecutionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReExecutedRows != 1 {
		t.Fatalf("ReExecutedRows = %d, want 1", res.ReExecutedRows)
	}
	var want float64
	for _, o := range res.Outcomes {
		want += o.TotalOccupancyGPUSeconds
	}
	if res.TotalOccupancyGPUSeconds != want {
		t.Fatalf("TotalOccupancyGPUSeconds = %.1f, want %.1f (the sum over rows)", res.TotalOccupancyGPUSeconds, want)
	}
	out := RenderResult(res)
	header := strings.Split(out, "  a1")[0]
	for _, s := range []string{"totalOccupancyGPUSeconds=", "reExecutedRows=1"} {
		if !strings.Contains(header, s) {
			t.Fatalf("run-level header is missing %q:\n%s", s, out)
		}
	}
}

func TestRenderResultReportsOccupancyAgainstServiceTime(t *testing.T) {
	// A bare totalOccupancy=81.0 means nothing; "81 s for a 40 s job" is the entire finding, so the row's
	// declared service time has to sit beside its occupancy.
	res, err := Reconstruct("Any", reclaimAnyTrace(), reExecutionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range res.Outcomes {
		if o.ServiceDurationSec == 0 {
			t.Fatalf("row %s carries no service time from its trace row", o.Job)
		}
	}
	out := RenderResult(res)
	if !hasLineWith(out, "totalOccupancy=", "serviceTime=") {
		t.Fatalf("occupancy must be rendered beside the row's service time:\n%s", out)
	}
}

// occupancyLinePattern pulls the two figures back out of a rendered row so the RELATION between them can be
// asserted, not merely the presence of the two labels.
var occupancyLinePattern = regexp.MustCompile(`totalOccupancy=([0-9.]+)\(serviceTime=([0-9]+)s\)`)

func TestRenderResultShowsOccupancyExceedingDeclaredServiceTime(t *testing.T) {
	// The whole point of printing service time is that a reader can see the pool paid 81 s for 40 s of asked-
	// for service. A fixture whose declared duration does not match its own events renders the label while
	// demonstrating the inverse, so the ordering of the two figures is what has to be pinned.
	res, err := Reconstruct("Any", matchedServiceTimeTrace(), reExecutionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var a2 WorkloadOutcome
	for _, o := range res.Outcomes {
		if o.Job == "a2" {
			a2 = o
		}
	}
	if !a2.ReExecuted {
		t.Fatalf("a2 must be the re-executed row for this demonstration to mean anything: %+v", a2)
	}
	out := RenderResult(res)
	var line string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, "totalOccupancy=") && strings.Contains(l, "serviceTime=40s") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no row line carries a2's occupancy against its declared 40 s of service:\n%s", out)
	}
	m := occupancyLinePattern.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("occupancy and service time are not rendered in a readable pair: %q", line)
	}
	occupancy, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	service, err := strconv.Atoi(m[2])
	if err != nil {
		t.Fatal(err)
	}
	if occupancy <= float64(service) {
		t.Fatalf("rendered occupancy %.1f must EXCEED the declared service time %ds; a fixture that renders the "+
			"reverse demonstrates nothing: %q", occupancy, service, line)
	}
}

func TestRenderResultShowsCensoredWaitOnlyOnNeverAdmittedRows(t *testing.T) {
	// The suffix is a lower bound on wait for a row never admitted, and a bare number on an ADMITTED row
	// would misread as that row's exact wait. Pinning both sides in one rendering is what makes the guard a
	// constraint: an assertion that only checks the never-admitted row still passes if the guard is deleted.
	events := ineffectivePreemptionEvents()
	kept := events[:0]
	for _, e := range events {
		if e.Job == "b1" && (e.Type == EventAdmitted || e.Type == EventPodReady || e.Type == EventCompleted) {
			continue
		}
		kept = append(kept, e)
	}
	res, err := Reconstruct("Any", reclaimAnyTrace(), kept, 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var admitted, never []string
	for _, o := range res.Outcomes {
		if o.Admitted {
			admitted = append(admitted, o.Job)
		} else {
			never = append(never, o.Job)
		}
	}
	if len(admitted) == 0 || len(never) == 0 {
		t.Fatalf("this test needs BOTH an admitted and a never-admitted row, got admitted=%v never=%v", admitted, never)
	}
	// Timing lines are emitted one per outcome in Outcomes order, which is what lets each rendered line be
	// paired with the row it belongs to and checked BOTH ways.
	out := RenderResult(res)
	var timing []string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, "admitLatency=") {
			timing = append(timing, l)
		}
	}
	if len(timing) != len(res.Outcomes) {
		t.Fatalf("got %d timing lines for %d outcomes:\n%s", len(timing), len(res.Outcomes), out)
	}
	for i, o := range res.Outcomes {
		shown := strings.Contains(timing[i], "censoredWait=")
		if o.Admitted && shown {
			t.Fatalf("admitted row %s rendered a censored wait, which misreads as its exact wait: %q", o.Job, timing[i])
		}
		if !o.Admitted && !shown {
			t.Fatalf("never-admitted row %s must render its censored wait: %q", o.Job, timing[i])
		}
	}
}

func TestRenderResultSurfacesUnknownAttribution(t *testing.T) {
	// Occupancy that no evidence can attribute either way must not be a silent flag: a reader who sees
	// waste=0.0 in the header would otherwise conclude nothing was lost.
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), preemptedNoTerminalPhaseEvents(), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AnyAttributionUnknown {
		t.Fatalf("fixture must produce unknown attribution: %+v", res)
	}
	out := RenderResult(res)
	header := strings.Split(out, "\n  ")[0]
	for _, s := range []string{"UNATTRIBUTED", "could not be attributed"} {
		if !strings.Contains(header, s) {
			t.Fatalf("run-level header must state %q so the reader knows unattributable occupancy exists:\n%s", s, out)
		}
	}
	if !hasLineWith(out, "a2", "attributionUnknown=true") {
		t.Fatalf("the per-row flag must be rendered on its own row:\n%s", out)
	}
}

func TestRenderResultStatesKnownAttributionAsFalse(t *testing.T) {
	// The flag is pinned as a per-row VALUE, so a row whose cause is established must say so rather than
	// simply omit the field, which a substring search would not distinguish from a deleted renderer line.
	res, err := Reconstruct("Any", reclaimAnyTrace(), ineffectivePreemptionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	if res.AnyAttributionUnknown {
		t.Fatalf("every attempt in this fixture reached an observed terminal phase: %+v", res)
	}
	out := RenderResult(res)
	for _, o := range res.Outcomes {
		if !hasLineWith(out, o.Job, "attributionUnknown=false") {
			t.Fatalf("row %s must render attributionUnknown=false:\n%s", o.Job, out)
		}
	}
	if strings.Contains(out, "UNATTRIBUTED") {
		t.Fatalf("the run-level banner must stay off when every cause is established:\n%s", out)
	}
}

// f1UnattributedBannerTrace pairs a row whose preemption target succeeded anyway (cause established) with a
// row that reaches no terminal phase at all (cause unknown), so the banner's single figure can be checked
// against the RIGHT subtotal instead of their sum.
func f1UnattributedBannerTrace() []TrainingTraceRow {
	return []TrainingTraceRow{
		{Index: 0, Name: "r1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 100},
		{Index: 1, Name: "r2", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 100},
	}
}

// f1UnattributedBannerEvents is the two-row scenario the finding names verbatim: r1 is preempted but
// succeeds anyway (33 GPU-seconds, cause established), r2 is preempted and reaches no terminal phase by the
// horizon at all (47 GPU-seconds, cause unknown).
func f1UnattributedBannerEvents() []LifecycleEvent {
	return []LifecycleEvent{
		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "r1"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "r1"},
		{ElapsedNs: 2 * sec, Kind: "Pod", Type: EventPodReady, Job: "r1", ObjectUID: "pod-r1"},
		{ElapsedNs: 10 * sec, Kind: "Workload", Type: EventPreempted, Job: "r1", Reason: "InCohortReclamation"},
		// r1 ignores the signal and finishes on its own: 35 - 2 = 33 GPU-seconds, cause established.
		{ElapsedNs: 35 * sec, Kind: "Pod", Type: EventAttemptStopped, Job: "r1", ObjectUID: "pod-r1", Reason: StopReasonSucceeded},

		{ElapsedNs: 0, Kind: "MLTrainingJob", Type: EventSubmitted, Job: "r2"},
		{ElapsedNs: 1 * sec, Kind: "Workload", Type: EventAdmitted, Job: "r2"},
		{ElapsedNs: 3 * sec, Kind: "Pod", Type: EventPodReady, Job: "r2", ObjectUID: "pod-r2"},
		{ElapsedNs: 20 * sec, Kind: "Workload", Type: EventPreempted, Job: "r2", Reason: "InCohortReclamation"},
		// r2 reaches no terminal phase at all: charged 50 - 3 = 47 to the horizon, cause unknown.
	}
}

var unattributedBannerPattern = regexp.MustCompile(`UNATTRIBUTED OCCUPANCY: ([0-9.]+) GPU-seconds`)

// TestRenderResultUnattributedBannerReportsOnlyNoTerminalPhaseOccupancy is F1: the banner is gated on
// AnyAttributionUnknown but historically printed the COMBINED unattributed figure, which also folds in
// occupancy from a Succeeded stop where the cause IS established. Only r2's 47 lacks a terminal phase; r1's
// 33 must not be added into the number this banner's own sentence claims to describe.
func TestRenderResultUnattributedBannerReportsOnlyNoTerminalPhaseOccupancy(t *testing.T) {
	res, err := Reconstruct("Any", f1UnattributedBannerTrace(), f1UnattributedBannerEvents(), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderResult(res)
	m := unattributedBannerPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("banner did not render in the expected shape:\n%s", out)
	}
	got, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	if got != 47.0 {
		t.Fatalf("banner figure = %.1f, want 47.0 (only r2's no-terminal-phase occupancy, not r1's 33 + r2's"+
			" 47 = 80.0)", got)
	}
}

// TestRenderResultDoesNotContradictCreditedIneffectivePreemption is F2: on the scenario this branch exists to
// correct (a sole attempt preempted, no terminal phase observed, but the row's ledger contains a completion,
// so the sole-attempt credit fires), the header must not follow "its target completed successfully" with a
// sentence claiming the evidence supports neither conclusion -- for this row the second claim is false.
func TestRenderResultDoesNotContradictCreditedIneffectivePreemption(t *testing.T) {
	res, err := Reconstruct("Any", noTerminalPhaseTrace(), completedNoTerminalPhaseEvents(), 50*sec)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderResult(res)
	if !strings.Contains(out, "PREEMPTION INEFFECTIVE") || !strings.Contains(out, "its target completed successfully") {
		t.Fatalf("the credited row's ineffectiveness must still be stated loudly:\n%s", out)
	}
	if strings.Contains(out, "supports neither") {
		t.Fatalf("a row credited via the sole-attempt-plus-completion rule must not also be described as"+
			" supporting neither conclusion, which contradicts the ineffectiveness line above it:\n%s", out)
	}
}

func TestRenderResultStatesUncreditedLossOnReExecutedIneffectiveRow(t *testing.T) {
	// waste=0.0 is true about mechanism and false about loss: the attempt succeeded, was not credited, and
	// re-ran from zero. Replacing an overclaim with an underclaim is not a fix, so the banner must state the
	// loss rather than leave a reader to infer it from a zero.
	res, err := Reconstruct("Any", reclaimAnyTrace(), reExecutionEvents(), 200*sec)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range res.Outcomes {
		if o.PreemptionIneffective && o.ReExecuted {
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture must contain a row that is both ineffective and re-executed: %+v", res.Outcomes)
	}
	out := RenderResult(res)
	banner := ""
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "PREEMPTION INEFFECTIVE") {
			banner = line
		}
	}
	if banner == "" {
		t.Fatalf("an ineffective preemption must be stated loudly:\n%s", out)
	}
	for _, s := range []string{"NOT credited", "re-executed"} {
		if !strings.Contains(banner, s) {
			t.Fatalf("banner must state that the completed attempt's work was %q: %q", s, banner)
		}
	}
}

// The floor line is not optional decoration: it is the only thing on the screen that tells a reader the
// magnitudes above it have a size below which they say nothing. A render that drops it hands back exactly
// the output that produced the retracted 0.94 second claim.
func TestRenderResultAlwaysStatesTheFloor(t *testing.T) {
	bounded := SpreadOf([]LifecycleEvent{skewed(430 * int64(time.Millisecond)), skewed(2389 * int64(time.Millisecond))})
	out := RenderResult(LabResult{Arm: "A-honor", TotalWastedGPUSeconds: 41.0, Spread: bounded})
	if !strings.Contains(out, "observationFloor=2.959s") {
		t.Fatalf("floor absent or wrong:\n%s", out)
	}
	if !strings.Contains(out, "NOT resolved") {
		t.Fatalf("floor stated without saying what it forbids:\n%s", out)
	}
}

// A run that bounded nothing must say so more loudly than one that bounded something coarsely, because the
// silence is the more dangerous of the two states.
func TestRenderResultShoutsWhenNothingBoundedTheIntervals(t *testing.T) {
	out := RenderResult(LabResult{Arm: "A-honor", TotalWastedGPUSeconds: 41.0})
	if !strings.Contains(out, "observationFloor=UNBOUNDED") {
		t.Fatalf("an unbounded run rendered without saying so:\n%s", out)
	}
	if !strings.Contains(out, "unverified") {
		t.Fatalf("unbounded run did not tell the reader what to do with the magnitudes:\n%s", out)
	}
}
