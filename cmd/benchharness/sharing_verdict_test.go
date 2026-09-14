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

// The exit status must separate a refused ARM from a refused RUN.
//
// hack/m5c-matrix.sh calls `benchharness report ... || fail`, so this predicate decides whether a paid
// session is reported as having produced nothing. It used to match the substring "INVALID" against the
// reading's NAME, and reading 4c is named "the sharing mode did not engage -- INVALID for that arm" -- so
// a run that measured time-slicing and merely failed to bring MPS up was thrown away whole. MPS has
// already been measured failing to engage on this AMI, which makes that the expected run, not an edge.
func TestOnlyTheWholeRunGatesTheExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading bench.PoPReading
		invalid bool
	}{
		{
			name:    "4 fired: the load made no contention, so nothing under it means anything",
			reading: bench.PoPReading{ID: "4", Name: "the load did not create contention -- INVALID", Fired: true},
			invalid: true,
		},
		{
			name:    "4b fired: the load was too high for any arm to be measured",
			reading: bench.PoPReading{ID: "4b", Name: "the load was too high to measure -- INVALID", Fired: true},
			invalid: true,
		},
		{
			name:    "4c fired: one arm did not engage, and the arm beside it was still measured",
			reading: bench.PoPReading{ID: "4c", Name: "the sharing mode did not engage -- INVALID for that arm", Fired: true, Cell: bench.ArmMPS},
			invalid: false,
		},
		{
			name:    "4 did not fire",
			reading: bench.PoPReading{ID: "4", Name: "the load did not create contention -- INVALID"},
			invalid: false,
		},
		{
			// A gate that could not be computed is not a run that stands: it says the evidence could not be
			// assessed. This exited zero, so the paid runner accepted a censored control as a good session.
			name:    "4 could not be evaluated: the control's tail is censored",
			reading: bench.PoPReading{ID: "4", Name: "the load did not create contention -- INVALID", NotEvaluable: true},
			invalid: true,
		},
		{
			name:    "4b could not be evaluated",
			reading: bench.PoPReading{ID: "4b", Name: "the load was too high to measure -- INVALID", NotEvaluable: true},
			invalid: true,
		},
		{
			// But a reading BELOW the gates coming back NotEvaluable is an ordinary "no finding".
			name:    "1 could not be evaluated",
			reading: bench.PoPReading{ID: "1", Name: "separation protects -- POSITIVE", NotEvaluable: true},
			invalid: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sharingRunInvalid(bench.SharingResult{Readings: []bench.PoPReading{tc.reading}})
			switch {
			case tc.invalid && err == nil:
				t.Errorf("reading %s fired and the run exited zero; the paid runner would accept evidence "+
					"the readings had just disqualified", tc.reading.ID)
			case !tc.invalid && err != nil:
				t.Errorf("reading %s made the whole run invalid: %v", tc.reading.ID, err)
			case tc.invalid && !strings.Contains(err.Error(), "run invalid"):
				t.Errorf("the error does not identify the run as invalid: %v", err)
			}
		})
	}
}

// The same replay passed twice may not become two repetitions.
//
// A repetition is an independent measurement, and readings 3 and 5 compare an improvement against the
// CONTROL'S repetition-to-repetition spread. Two copies of one replay have a spread of exactly zero, so
// every improvement clears it. A review reproduced this by passing each of the eighth pilot's raw files
// twice: RepetitionCount became 2, the equal-repetitions check passed, and reading 5 fired on evidence
// that contained one repetition.
func TestTheSameReplayCannotBeCountedTwice(t *testing.T) {
	dir := t.TempDir()
	a := writeRepetition(t, dir, "a.jsonl", "R1", 200, 100, 150, 0)

	// A byte-for-byte copy under a different name is the case that matters: a second -raw of the SAME path
	// could be caught by comparing paths, and that is not the defect.
	b := filepath.Join(dir, "a-copy.jsonl")
	blob, err := os.ReadFile(a)
	if err != nil {
		t.Fatalf("read %s: %v", a, err)
	}
	if err := os.WriteFile(b, blob, 0o600); err != nil {
		t.Fatalf("write %s: %v", b, err)
	}

	err = report([]string{"-out", filepath.Join(dir, "report.txt"), "-raw", a, "-raw", b})
	if err == nil {
		t.Fatal("a replay and a copy of it were accepted as two repetitions; their spread is zero and that " +
			"spread is the threshold readings 3 and 5 measure against")
	}
	for _, want := range []string{"same nanosecond", "copy", "spread"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, so a reader cannot tell a duplicate from a real "+
				"disagreement between repetitions: %v", want, err)
		}
	}
}
