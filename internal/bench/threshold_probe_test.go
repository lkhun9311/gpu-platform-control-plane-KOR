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
	"os"
	"regexp"
	"strings"
	"testing"
)

// The probe prompts must land on opposite sides of the guard's threshold, and the margin is four characters.
//
// The numbers live in cmd/benchharness, and the arithmetic that decides them lives here. If either moves
// without the other, the probes stop probing: both land on the same side, both get the same decision, and
// the report shows a threshold that was never tested while looking exactly like one that was.
func TestProbePromptsStraddleTheGuardThreshold(t *testing.T) {
	const threshold = 4096
	// Read from the package the generator builds its tenants from, NOT transcribed. Transcribing them is how
	// the first version of this test passed while both probes sat on the same side of the threshold.
	under, over := ProbeUnderChars, ProbeOverChars

	if got := defaultEstInputTokens(under); got != threshold-1 {
		t.Errorf("under-probe of %d chars scores %d, want %d (one below the threshold)", under, got, threshold-1)
	}
	if got := defaultEstInputTokens(over); got != threshold {
		t.Errorf("over-probe of %d chars scores %d, want %d (exactly at the threshold)", over, got, threshold)
	}
	if defaultEstInputTokens(under) >= threshold {
		t.Error("the under-probe is eligible; it must pass an engaged guard or it proves nothing")
	}
	if defaultEstInputTokens(over) < threshold {
		t.Error("the over-probe is not eligible; it must be rejected by an engaged guard or it proves nothing")
	}
	if over-under != 4 {
		t.Errorf("the probes are %d characters apart; the point of this pair is that the margin is tiny", over-under)
	}
}

// The harness and the gateway each implement the estimate, and nothing else keeps them in step.
//
// Every admission-match number depends on the two agreeing: the harness scores a prompt to decide what work
// was OFFERED, and the gateway scores the same prompt to decide what to admit. Drift would not fail
// anything -- it would silently move the offered-work denominator away from the admitted-work numerator, and
// the arms would be declared matched or mismatched on an accounting error.
//
// Reading the other package's source is crude, and it is what is available: the gateway's estimator is
// unexported and in a package this one must not import. The repository does the same thing elsewhere, for
// the same reason -- a test that transcribes the other side's constant checks only the transcription.
func TestTheHarnessAndTheGatewayScoreAPromptTheSameWay(t *testing.T) {
	src, err := os.ReadFile("../gateway/proxy.go")
	if err != nil {
		t.Fatalf("read the gateway's estimator: %v", err)
	}
	m := regexp.MustCompile(`meta\.EstInputTokens = \(totalChars \+ (\d+)\) / (\d+)`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("the gateway's estimator is not the shape this test reads; it has been rewritten and this " +
			"test must be rewritten with it rather than deleted, because the drift it guards is silent")
	}
	addend, divisor := m[1], m[2]
	if addend != "3" || divisor != "4" {
		t.Fatalf("the gateway now scores (chars+%s)/%s; the harness still scores (chars+3)/4, so offered work "+
			"and admitted work are no longer measured on the same scale", addend, divisor)
	}
	// Behavioural check on top of the textual one, so a matching expression that computes something else
	// still fails: these are the values the probes depend on.
	for chars, want := range map[int]int{0: 0, 1: 1, 3: 1, 4: 1, 5: 2, 16_380: 4095, 16_384: 4096} {
		if got := defaultEstInputTokens(chars); got != want {
			t.Errorf("harness scores %d chars as %d, want %d", chars, got, want)
		}
	}
}

// Summarize must FILL the probe tally from the rows, which the renderer test cannot show.
//
// That test builds an ArmSummary by hand, so deleting the aggregation entirely left it passing: the
// renderer was proven correct about a field nothing populated. This is the same shape as a prompt helper
// that is correct while the sender never calls it, which this harness has already been bitten by once.
func TestSummarizeFillsTheProbeTallyFromTheRows(t *testing.T) {
	rows := []RawRow{
		{Arm: "kv-aware", Tenant: ProbeUnderTenant, IsNoisy: true, EstInputTokens: 4095, HTTPStatus: 200, FirstTokenUnixNanos: 2, SendUnixNanos: 1, EndUnixNanos: 3},
		{Arm: "kv-aware", Tenant: ProbeUnderTenant, IsNoisy: true, EstInputTokens: 4095, HTTPStatus: 200, FirstTokenUnixNanos: 2, SendUnixNanos: 1, EndUnixNanos: 3},
		{Arm: "kv-aware", Tenant: ProbeOverTenant, IsNoisy: true, EstInputTokens: 4096, HTTPStatus: 429},
		{Arm: "kv-aware", Tenant: ProbeOverTenant, IsNoisy: true, EstInputTokens: 4096, HTTPStatus: 429},
		{Arm: "kv-aware", Tenant: "premium-1", IsNoisy: false, EstInputTokens: 50, HTTPStatus: 200, FirstTokenUnixNanos: 2, SendUnixNanos: 1, EndUnixNanos: 3},
	}
	s := Summarize("kv-aware", rows)

	u, ok := s.ThresholdProbe[ProbeUnderTenant]
	if !ok {
		t.Fatal("Summarize recorded no tally for the under-probe; the boundary result would be absent from every report")
	}
	if u.Total != 2 || u.Rejected != 0 || u.EstInputTokens != 4095 {
		t.Errorf("under-probe tallied %+v, want total 2, rejected 0, est 4095", u)
	}
	o := s.ThresholdProbe[ProbeOverTenant]
	if o.Total != 2 || o.Rejected != 2 || o.EstInputTokens != 4096 {
		t.Errorf("over-probe tallied %+v, want total 2, rejected 2, est 4096", o)
	}
	if _, leaked := s.ThresholdProbe["premium-1"]; leaked {
		t.Error("premium was counted as a probe; the tally must be the probes only")
	}
}

// A report built from traffic that never approaches the threshold must SAY so rather than look complete.
func TestAReportWithoutProbesSaysTheThresholdWasNotTested(t *testing.T) {
	out := FormatReport([]ArmSummary{{Arm: "kv-aware", TailSampleSize: 500}}, Checks{}, 0.05)
	if !strings.Contains(out, "NOT TESTED") {
		t.Error("a report with no probe tenants does not say the threshold was untested; a reader would take " +
			"the configured value as evidenced when any value in a wide range would have produced this run")
	}
}

// With probes, the opposite outcomes are what the report has to show.
func TestAReportWithProbesShowsBothSidesOfTheBoundary(t *testing.T) {
	out := FormatReport([]ArmSummary{{
		Arm:            "kv-aware",
		TailSampleSize: 500,
		ThresholdProbe: map[string]ProbeOutcome{
			ProbeUnderTenant: {Total: 50, Rejected: 0, EstInputTokens: 4095},
			ProbeOverTenant:  {Total: 50, Rejected: 47, EstInputTokens: 4096},
		},
	}}, Checks{}, 0.05)

	if strings.Contains(out, "NOT TESTED") {
		t.Error("the report claims the threshold was untested while carrying probe outcomes")
	}
	for _, want := range []string{ProbeUnderTenant, "est=4095", "rejected=0", ProbeOverTenant, "est=4096", "rejected=47"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q, so the boundary result is invisible to a reader", want)
		}
	}
	if !strings.Contains(out, "3171") {
		t.Error("the report does not state the measured real cost these estimates stood in for, so a reader " +
			"cannot tell that the rejection fired on an over-estimate")
	}
}
