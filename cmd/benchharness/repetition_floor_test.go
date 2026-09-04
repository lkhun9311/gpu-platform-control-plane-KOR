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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// writeRepetition writes one repetition file: n completed premium rows with increasing TTFT, then rejected
// premium rows (HTTP 429) so the TOTAL row count can be held constant while the completion count varies, plus
// one contender row carrying the arm's admitted work.
//
// Holding rows constant matters: the arms share one immutable trace, so `report` refuses outright when their
// recorded row counts differ. A fixture that shortened a file would exercise that refusal instead of the
// per-repetition floor it is meant to test.
func writeRepetition(t *testing.T, dir, name, arm string, n int, ttftBaseMs int64, admitted int, rejected int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	for i := range n {
		ttft := (ttftBaseMs + int64(i)) * 1e6
		row := bench.RawRow{
			Index: i, Arm: arm, Tenant: "premium", HTTPStatus: 200,
			SendUnixNanos: 1, FirstTokenUnixNanos: 1 + ttft, EndUnixNanos: 1 + ttft + 1e6,
			EstInputTokens: 10, MatchTolerance: 0.05, TraceChecksum: "0000000000000000000000000000000000000000000000000000000000000000",
		}
		if err := enc.Encode(row); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	for i := range rejected {
		r := bench.RawRow{
			Index: n + i, Arm: arm, Tenant: "premium", HTTPStatus: 429,
			SendUnixNanos: 1, EstInputTokens: 10, MatchTolerance: 0.05,
			TraceChecksum: "0000000000000000000000000000000000000000000000000000000000000000",
		}
		if err := enc.Encode(r); err != nil {
			t.Fatalf("encode rejected: %v", err)
		}
	}
	// One contender row carrying the offered/admitted work, so admittedWorkFraction is well defined and
	// identical across arms — the admission-match check must not be what refuses these runs.
	c := bench.RawRow{
		Index: n + rejected, Arm: arm, Tenant: "contender", IsNoisy: true, HTTPStatus: 200,
		SendUnixNanos: 1, FirstTokenUnixNanos: 2, EndUnixNanos: 3,
		EstInputTokens: admitted, MatchTolerance: 0.05, TraceChecksum: "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := enc.Encode(c); err != nil {
		t.Fatalf("encode contender: %v", err)
	}
	return path
}

// TestReportRefusesATruncatedRepetitionThroughTheRealCommand covers the wiring, not the rule.
//
// internal/bench has specs for the rule itself, and they pass on a hand-built ArmSummary with the repetition
// fields already set. What they cannot see is whether the command FILLS those fields: Summarize is handed
// pooled rows and cannot know how they were split, so `report` attaches the repetition shape itself. Deleting
// that attachment leaves every unit spec green -- which is the same defect class as a Terraform variable no
// caller ever passed, and it is why this test drives the actual command over actual files.
func TestReportRefusesATruncatedRepetitionThroughTheRealCommand(t *testing.T) {
	dir := t.TempDir()

	// R1 and static-cap are healthy. kv-aware is split into two repetitions, the second cut short at 30
	// premium completions -- the shape a node going away mid-run produces. Pooled, kv-aware still reports
	// 530 completions, comfortably above MinTailSamples, which is exactly how the truncation used to hide.
	// Every arm gets TWO repetitions so the counts match and the incremental CI is computed. Otherwise the
	// unequal-repetition refusal fires first and this test would pass without the floor existing at all.
	raw := []string{
		writeRepetition(t, dir, "r1a.jsonl", "R1", 500, 100, 250, 0),
		writeRepetition(t, dir, "r1b.jsonl", "R1", 500, 100, 250, 0),
		writeRepetition(t, dir, "ba.jsonl", "static-cap", 500, 200, 250, 0),
		writeRepetition(t, dir, "bb.jsonl", "static-cap", 500, 200, 250, 0),
		writeRepetition(t, dir, "c1.jsonl", "kv-aware", 500, 110, 255, 0),
		// Same row total, but this repetition completed 30 premium requests and rejected 470.
		writeRepetition(t, dir, "c2.jsonl", "kv-aware", 30, 110, 255, 470),
	}

	outPath := filepath.Join(dir, "report.txt")
	args := []string{"-out", outPath}
	for _, p := range raw {
		args = append(args, "-raw", p)
	}
	// report exits non-zero on an invalid run, and still writes the report first so the refusal survives as
	// evidence.
	err := report(args)
	if err == nil {
		t.Fatal("a run with a 30-completion repetition exited zero")
	}
	if !strings.Contains(err.Error(), "run invalid") {
		t.Errorf("the error did not identify the run as invalid: %v", err)
	}

	b, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("read report: %v", rerr)
	}
	got := string(b)

	if !strings.Contains(got, "run invalid") {
		t.Errorf("a run with a 30-completion repetition was not refused; report said:\n%s", got)
	}
	if !strings.Contains(got, "30 premium completions") {
		t.Errorf("the refusal did not name the truncated repetition's size; report said:\n%s", got)
	}
	if strings.Contains(got, "all checks passed") {
		t.Errorf("the report certified a truncated run:\n%s", got)
	}
}

// TestReportAcceptsEqualHealthyRepetitions is the control.
//
// Without it the test above proves only that something refuses, not that the FLOOR is what refuses -- a
// report that refused every input would pass it.
func TestReportAcceptsEqualHealthyRepetitions(t *testing.T) {
	dir := t.TempDir()
	raw := []string{
		writeRepetition(t, dir, "r1a.jsonl", "R1", 500, 100, 250, 0),
		writeRepetition(t, dir, "r1b.jsonl", "R1", 500, 100, 250, 0),
		writeRepetition(t, dir, "ba.jsonl", "static-cap", 500, 200, 250, 0),
		writeRepetition(t, dir, "bb.jsonl", "static-cap", 500, 200, 250, 0),
		writeRepetition(t, dir, "c1.jsonl", "kv-aware", 500, 110, 255, 0),
		writeRepetition(t, dir, "c2.jsonl", "kv-aware", 500, 110, 255, 0),
	}
	outPath := filepath.Join(dir, "report.txt")
	args := []string{"-out", outPath}
	for _, p := range raw {
		args = append(args, "-raw", p)
	}
	if err := report(args); err != nil {
		t.Fatalf("report: %v", err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(b), "premium completions, below") {
		t.Errorf("healthy equal repetitions were refused by the floor:\n%s", string(b))
	}
}

// TestReportRefusesAnArmRecordedShorterThanTheTraceItShares is the fifth certification hole.
//
// The four earlier fixes all guard the SHAPE of a tail that is present — is it censored, is it big enough,
// does an interval exist. None guards whether the recording FINISHED. An arm cut off partway leaves every row
// it did write complete: nothing censors, no repetition is thin, the tail clears its floor, and the arm is
// simply shorter than the trace it claims to have replayed.
//
// The direction is what makes it the worst of the set. Contention builds over a trace, so the requests that
// arrive late are the slow ones, and dropping the tail of the RECORDING drops exactly the evidence that would
// fail the arm. An adversarial review demonstrated a genuine FAIL (C/R1 = 3.434) becoming
// "all checks passed" (C/R1 = 1.066) by truncating the arm under test to its first 60% of rows.
func TestReportRefusesAnArmRecordedShorterThanTheTraceItShares(t *testing.T) {
	dir := t.TempDir()

	// off, static-cap and kv-aware share one immutable trace, verified by checksum. kv-aware here recorded
	// 300 rows against their 500 — a run that stopped early, with every recorded row complete.
	raw := []string{
		writeRepetition(t, dir, "r1.jsonl", "R1", 500, 100, 500, 0),
		writeRepetition(t, dir, "off.jsonl", "off", 500, 300, 500, 0),
		writeRepetition(t, dir, "b.jsonl", "static-cap", 500, 200, 500, 0),
		writeRepetition(t, dir, "c.jsonl", "kv-aware", 300, 110, 500, 0),
	}
	outPath := filepath.Join(dir, "report.txt")
	args := []string{"-out", outPath}
	for _, p := range raw {
		args = append(args, "-raw", p)
	}

	err := report(args)
	if err == nil {
		b, _ := os.ReadFile(outPath)
		t.Fatalf("an arm recorded 300 rows against 500 for the arms it shares a trace with was not refused; report said:\n%s", string(b))
	}
	if !strings.Contains(err.Error(), "recorded 301 rows") {
		t.Errorf("the refusal did not name the short recording: %v", err)
	}
	if !strings.Contains(err.Error(), "immutable trace") {
		t.Errorf("the refusal did not say why equal counts are required: %v", err)
	}
}

// TestReportAcceptsR1BeingShorterThanTheContendedArms is the control that keeps the rule honest.
//
// R1 replays the same trace with the contender filtered out, so it legitimately records fewer rows. A rule
// that simply demanded every arm match would refuse every valid run — and would still have passed the test
// above, which is why this exists.
func TestReportAcceptsR1BeingShorterThanTheContendedArms(t *testing.T) {
	dir := t.TempDir()
	// Two repetitions per arm so the incremental interval is real: with one each, the run is refused for a
	// reason about the CI and this control would pass without the row rule being exercised at all.
	raw := []string{
		writeRepetition(t, dir, "r1a.jsonl", "R1", 250, 100, 250, 0),
		writeRepetition(t, dir, "r1b.jsonl", "R1", 250, 100, 250, 0),
		writeRepetition(t, dir, "offa.jsonl", "off", 500, 300, 250, 0),
		writeRepetition(t, dir, "offb.jsonl", "off", 500, 300, 250, 0),
		writeRepetition(t, dir, "ba.jsonl", "static-cap", 500, 200, 250, 0),
		writeRepetition(t, dir, "bb.jsonl", "static-cap", 500, 200, 250, 0),
		writeRepetition(t, dir, "ca.jsonl", "kv-aware", 500, 110, 255, 0),
		writeRepetition(t, dir, "cb.jsonl", "kv-aware", 500, 110, 255, 0),
	}
	outPath := filepath.Join(dir, "report.txt")
	args := []string{"-out", outPath}
	for _, p := range raw {
		args = append(args, "-raw", p)
	}
	if err := report(args); err != nil {
		t.Fatalf("R1 being shorter than the contended arms was refused, which would refuse every valid run: %v", err)
	}
}

// TestARepetitionCarryingADifferentTraceIsRefused closes a hole in the identity check.
//
// refuseIfTracesDisagree exists to prove the contended arms replayed identical traffic. It compared one
// checksum per arm, and loadArmEvidence wrote that field once per FILE -- so only the last repetition's
// checksum survived, and a repetition replayed from a different trace was invisible.
//
// The trigger is a workflow the runner endorses: re-running one botched arm into the same output directory.
// Driven through the real binary, an arm holding checksums "X" and "T" among all-"T" arms reported cleanly
// and exited 0.
func TestARepetitionCarryingADifferentTraceIsRefused(t *testing.T) {
	dir := t.TempDir()
	write := func(arm string, rep int, sum string) string {
		path := filepath.Join(dir, fmt.Sprintf("raw-%s-%d.jsonl", arm, rep))
		var b strings.Builder
		for i := 1; i <= 200; i++ {
			base := int64(1_000_000_000 + i*1_000_000)
			row := bench.RawRow{
				Index: i, Arm: arm, Tenant: "premium-1", SendUnixNanos: base,
				FirstTokenUnixNanos: base + 50_000_000, EndUnixNanos: base + 60_000_000,
				EstInputTokens: 50, OutputTokens: 8, HTTPStatus: 200,
				TraceChecksum: sum, LongThreshold: 4096, MatchTolerance: 0.05,
			}
			enc, err := json.Marshal(row)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			b.Write(enc)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return path
	}

	var paths []string
	for _, arm := range []string{"R1", "off", "static-cap", "kv-aware"} {
		for rep := 1; rep <= 2; rep++ {
			sum := "T"
			// The mixed repetition is not the last one, which is exactly the case last-writer-wins hid.
			if arm == "static-cap" && rep == 1 {
				sum = "X"
			}
			paths = append(paths, write(arm, rep, sum))
		}
	}

	// Either refusal point is acceptable, and the earlier one is better: loading is where the mixture becomes
	// visible, and refusing there means no downstream stage ever sees a pooled arm built from two workloads.
	e, err := loadArmEvidence(paths)
	if err == nil {
		err = e.refuseIfTracesDisagree()
	}
	if err == nil {
		t.Fatal("a repetition replayed from a different trace was accepted; the arms are not known to have seen the same traffic and the comparison means nothing")
	}
	if !strings.Contains(err.Error(), "static-cap") {
		t.Errorf("the refusal does not name the arm that mixed traces: %v", err)
	}
}
