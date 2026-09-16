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
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// checkReplay decides whether one replay's evidence is usable, and says why when it is not.
//
// It exists so the runner stops at the replay that broke rather than at the report three hours later. The
// runner's own check asked a single question -- did anything at all complete -- and a replay that lost a
// third of its eligible traffic answered yes. The report can refuse such a run now, but only once every
// remaining replay has been paid for.
//
// The rules live here rather than in the shell because they are the report's rules. A copy in bash would be
// a second definition of "usable", and the two would drift the first time either moved: that shape has cost
// this project two paid runs already. bench.Summarize is what decides, and the thresholds are its constants.
//
// This is NOT identical to what the report refuses, and the difference is deliberate. The two answer
// different questions: this one asks whether the run should continue, the report asks what can be concluded
// from evidence that already exists. A replay carrying 401s or 403s stops the run here, because a
// misconfiguration should be fixed before another three hours are bought -- while the report scores the arm
// comparisons on the rows that did reach admission and marks only the threshold section void, which is the
// right granularity once the money is spent. An earlier version of this comment claimed the two refuse
// exactly the same things. They do not.
func checkReplay(args []string) error {
	fs := flag.NewFlagSet("check-replay", flag.ExitOnError)
	rawPath := fs.String("raw", "", "one replay's raw evidence file (required)")
	label := fs.String("label", "", "how to name this replay in a refusal, e.g. \"static-cap replay 2\"")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rawPath == "" {
		return fmt.Errorf("-raw is required")
	}
	name := *label
	if name == "" {
		name = *rawPath
	}

	f, err := os.Open(*rawPath)
	if err != nil {
		return fmt.Errorf("open replay evidence: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	rows, err := bench.ReadRawRows(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("%s recorded no rows at all", name)
	}

	s := bench.Summarize("check", rows)

	if s.Completed == 0 {
		return fmt.Errorf("%s completed nothing: %s. Every later replay would do the same, so this stops here rather than after the rest of them. 404 means the trace asks for a model no InferenceDeployment serves; 401 means a tenant is missing from the gateway-api-keys secret", name, statusHistogram(rows))
	}

	// Decided before admission control runs, so such a request measures nothing about the guard -- while the
	// arm still completes, still reports, and still satisfies a check that only asks whether anything did.
	if n := countUnauthorized(rows); n != 0 {
		return fmt.Errorf("%s has %d requests that came back 401 or 403. That is a configuration fault, not a result: they never reached admission control, so whatever they were meant to measure went unmeasured. Check that every tenant in the trace has both a key in gateway-api-keys and a GPUQuotaPolicy", name, n)
	}

	// An eligible request with no response was neither admitted nor refused, and the admitted-work fraction
	// is what the matched comparison rests on. Past the same threshold the report uses, the fraction would be
	// describing a population this replay could not see.
	if eligible := s.AdmissionLost + s.AdmissionScored(); eligible > 0 {
		if lost := float64(s.AdmissionLost) / float64(eligible); lost >= bench.MaxLostAdmissionFraction {
			return fmt.Errorf("%s lost the admission verdict for %d of %d eligible requests (%.1f%%, limit %.0f%%). A port-forward that dies mid-replay looks exactly like this, and the report would refuse the whole run for it after every other replay had been paid for", name, s.AdmissionLost, eligible, lost*100, bench.MaxLostAdmissionFraction*100)
		}
	}

	// The premium tail is the primary endpoint, so losing it ends the replay for the same reason.
	if s.Censored {
		return fmt.Errorf("%s lost more than 1%% of its premium requests, so its TTFT p99 is only a lower bound and the arm cannot carry the primary endpoint", name)
	}

	fmt.Printf("%s usable: %d completed, %d shed, %d premium in the tail\n", name, s.Completed, s.Rejected, s.TailSampleSize)
	return nil
}

// statusHistogram renders the response codes a dead replay produced, so the refusal names the cause.
func statusHistogram(rows []bench.RawRow) string {
	counts := map[int]int{}
	for _, r := range rows {
		counts[r.HTTPStatus]++
	}
	codes := make([]int, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool { return counts[codes[i]] > counts[codes[j]] })
	var out strings.Builder
	for i, c := range codes {
		if i == 4 {
			break
		}
		if i > 0 {
			out.WriteString(", ")
		}
		fmt.Fprintf(&out, "%dx%d", c, counts[c])
	}
	return out.String()
}

// countUnauthorized counts responses decided before admission control ran.
func countUnauthorized(rows []bench.RawRow) int {
	n := 0
	for _, r := range rows {
		if r.HTTPStatus == 401 || r.HTTPStatus == 403 {
			n++
		}
	}
	return n
}
