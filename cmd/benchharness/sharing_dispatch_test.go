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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// The sharing readings need R1 and `shared`, and a missing one must produce NO readings rather than readings
// computed against a zero ArmSummary.
//
// A zero value has TTFTMsP99 = 0, so every ratio built from it is either 0 or a division this code guards
// into 0 -- and every one of those still PRINTS, as a number a reader would take for a measurement. This is
// the same failure the price-of-protection evaluator was fixed for, arriving one level up in the dispatch.
func TestTheSharingReadingsAreNotEvaluatedWithoutTheirDenominators(t *testing.T) {
	full := map[string]bench.ArmSummary{
		bench.ArmR1:          {Arm: bench.ArmR1, TTFTMsP99: 67.3, TailSampleSize: 200},
		bench.ArmShared:      {Arm: bench.ArmShared, TTFTMsP99: 1400, TailSampleSize: 200},
		bench.ArmTimeSlicing: {Arm: bench.ArmTimeSlicing, TTFTMsP99: 900, TailSampleSize: 200},
	}
	order := []bench.ArmSummary{full[bench.ArmR1], full[bench.ArmShared], full[bench.ArmTimeSlicing]}

	if got := evaluateSharingMatrix(full, order, nil); got == nil {
		t.Fatal("complete evidence produced no readings at all")
	}

	for _, missing := range []string{bench.ArmR1, bench.ArmShared} {
		partial := map[string]bench.ArmSummary{}
		var kept []bench.ArmSummary
		for name, s := range full {
			if name == missing {
				continue
			}
			partial[name] = s
			kept = append(kept, s)
		}
		got := evaluateSharingMatrix(partial, kept, nil)
		// AMENDED 2026-09-12. This used to require nil, and nil was worse than it looked: the caller only
		// runs the verdict when this is non-nil, so `report` printed a stderr warning and exited ZERO --
		// and the paid runner calls it as `... || fail`. The property worth holding is not "no result" but
		// "no ratio computed from a zero ArmSummary", and one uncomputable gate says that out loud.
		if got == nil {
			t.Fatalf("evidence missing %s produced no result at all, so the verdict block never runs and the "+
				"command exits zero on a run with no %s", missing, missing)
		}
		if len(got.Readings) != 1 {
			t.Errorf("evidence missing %s produced %d readings; every ratio in them is built from a zero "+
				"ArmSummary and would print as a measurement", missing, len(got.Readings))
		}
		if len(got.Readings) > 0 {
			r := got.Readings[0]
			if !r.NotEvaluable || r.Fired {
				t.Errorf("the single reading for evidence missing %s is not a refusal (fired=%v, n/e=%v): %s",
					missing, r.Fired, r.NotEvaluable, r.Detail)
			}
			if !strings.Contains(r.Detail, missing) {
				t.Errorf("the refusal does not name the missing arm %s: %s", missing, r.Detail)
			}
		}
		if got.Answer != "" {
			t.Errorf("evidence missing %s produced the answer %q", missing, got.Answer)
		}
		// And it must reach the process's exit status, which is the thing the paid runner reads.
		if err := sharingRunInvalid(*got); err == nil {
			t.Errorf("a run with no %s arm exits zero; `benchharness report ... || fail` would accept it", missing)
		}
	}
}

// Everything that is not R1 or `shared` is a sharing arm, taken as present rather than looked up by name.
//
// An operator running ARMS="shared timeSlicing" should get a matrix with one sharing arm and a reading that
// says the other is absent -- not a name lookup that misses and reports a mode as having failed to engage.
func TestASubsetRunIsAMatrixWithFewerArmsRatherThanAFailedOne(t *testing.T) {
	summ := map[string]bench.ArmSummary{
		bench.ArmR1:          {Arm: bench.ArmR1, TTFTMsP99: 67.3, TailSampleSize: 200},
		bench.ArmShared:      {Arm: bench.ArmShared, TTFTMsP99: 1400, TailSampleSize: 200},
		bench.ArmTimeSlicing: {Arm: bench.ArmTimeSlicing, TTFTMsP99: 900, TailSampleSize: 200},
	}
	order := []bench.ArmSummary{summ[bench.ArmR1], summ[bench.ArmShared], summ[bench.ArmTimeSlicing]}

	res := evaluateSharingMatrix(summ, order, nil)
	if res == nil {
		t.Fatal("a two-arm matrix produced no readings")
	}
	var fourC bench.PoPReading
	for _, r := range res.Readings {
		if r.ID == "4c" {
			fourC = r
		}
	}
	if fourC.Fired {
		t.Errorf("reading 4c fired for an arm the operator simply did not run: %s", fourC.Detail)
	}
}

// The refusal the runner writes must be the refusal the report finds, from a directory it discovers itself.
//
// Reading 4c can only fire on a recorded refusal, and nothing outside the unit tests populated that map
// until refusalsBeside existed. The firing itself is covered in internal/bench; what is covered here is the
// half that cannot be: that a file written beside the raw evidence is picked up without an operator
// remembering a flag, and that an absent file yields no refusals rather than an empty-string entry.
func TestTheReportFindsTheRefusalTheRunnerWroteBesideTheEvidence(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"raw-R1-1.jsonl", "raw-shared-1.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if got := refusalsBeside([]string{filepath.Join(dir, "raw-R1-1.jsonl")}); len(got) != 0 {
		t.Errorf("a directory with no refusal file yielded %v; an arm that was never refused must not appear "+
			"as one with an empty reason", got)
	}

	want := "the engines are not MPS clients"
	if err := os.WriteFile(filepath.Join(dir, "refused-mps.txt"), []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("write the refusal: %v", err)
	}
	got := refusalsBeside([]string{filepath.Join(dir, "raw-R1-1.jsonl"), filepath.Join(dir, "raw-shared-1.jsonl")})
	if got["mps"] != want {
		t.Errorf("the report read %q for the mps arm and the runner wrote %q. The reason lives beside the raw "+
			"files precisely so that `benchharness report` needs no extra argument to see it", got["mps"], want)
	}

	// An empty file is not a refusal. The runner writes a reason or it writes nothing.
	if err := os.WriteFile(filepath.Join(dir, "refused-timeSlicing.txt"), []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write the empty refusal: %v", err)
	}
	if _, ok := refusalsBeside([]string{filepath.Join(dir, "raw-R1-1.jsonl")})["timeSlicing"]; ok {
		t.Error("an empty refusal file was read as a refusal, which would fire reading 4c with no reason to give")
	}
}
