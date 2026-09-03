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

// Command benchharness drives the M5-b GPU-free benchmark harness: generate an immutable trace and its
// frozen manifest, replay a trace against the gateway recording raw per-request evidence, and turn raw
// evidence into a report with the design's pre-registered checks.
//
// A stub-serve subcommand runs a trivial streaming backend so the whole gen -> replay -> report path can
// be exercised end to end with no GPU and no cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// traceRefFor returns the trace path as a manifest should record it: relative to the manifest's own
// directory when both live under it, and unchanged otherwise.
//
// LoadManifest resolves relative paths against the manifest's directory, so a working-directory-relative
// path recorded verbatim is joined twice and cannot be found.
func traceRefFor(manifestOut, traceOut string) string {
	if manifestOut == "" || filepath.IsAbs(traceOut) {
		return traceOut
	}
	rel, err := filepath.Rel(filepath.Dir(manifestOut), traceOut)
	if err != nil || strings.HasPrefix(rel, "..") {
		// Outside the manifest's directory: an absolute path is the only unambiguous answer.
		abs, absErr := filepath.Abs(traceOut)
		if absErr != nil {
			return traceOut
		}
		return abs
	}
	return rel
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen-trace":
		err = genTrace(os.Args[2:])
	case "replay":
		err = replay(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	case "print-prompt":
		printPrompt(os.Args[2:])
	case "stub-serve":
		err = stubServe(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: benchharness <gen-trace|replay|report|print-prompt|stub-serve> [flags]")
}

// genTrace generates an immutable trace file and a frozen manifest that pins its checksum.
func genTrace(args []string) error {
	fs := flag.NewFlagSet("gen-trace", flag.ExitOnError)
	seed := fs.Int64("seed", 1, "trace generator seed")
	durationMs := fs.Int64("duration-ms", 60_000, "trace duration in ms")
	// 20/s is calibrated for hack/m5b-harness-dryrun.sh, whose stub backend costs nothing to serve. It is
	// NOT a value a GPU can take: see hack/m5b-vllm-sizing.md, where sustaining half of it in contender
	// traffic works out to 7.3x a T4's theoretical peak. Set it from the ceiling derived there.
	rate := fs.Float64("rate", 20, "mean total arrival rate per second (stub-calibrated; see hack/m5b-vllm-sizing.md before a GPU run)")
	premiumChars := fs.Int("premium-prompt-chars", 200, "premium tenant prompt length in chars")
	// The old help here said ">= 4x the guard threshold" and that was wrong: ceil(40000/4) is 10,000 against
	// a 4,096 threshold, which is 2.44x. Corrected rather than restated, since the margin is the reason the
	// contender population is unambiguously eligible and a wrong multiple invites someone to shrink it.
	noisyChars := fs.Int("noisy-prompt-chars", 40_000, "noisy tenant prompt length in chars (estimates at 10,000 tokens, 2.44x the 4,096 guard threshold)")
	premiumWeight := fs.Float64("premium-weight", 1, "premium tenant arrival share")
	noisyWeight := fs.Float64("noisy-weight", 1, "noisy tenant arrival share")
	// Small on purpose. These are a probe population, not a load driver: each carries about 3,171 real
	// tokens, so a large share would move the pressure the arms are supposed to differ under.
	probeWeight := fs.Float64("probe-weight", 0.1, "arrival share of EACH threshold-probe tenant; 0 disables them")
	probeUnderChars := fs.Int("probe-under-chars", bench.ProbeUnderChars, "probe prompt scoring just BELOW the guard threshold")
	probeOverChars := fs.Int("probe-over-chars", bench.ProbeOverChars, "probe prompt scoring exactly AT the guard threshold")
	arm := fs.String("arm", "off", "arm this manifest measures (R1|off|static-cap|kv-aware)")
	gatewayURL := fs.String("gateway-url", "http://localhost:8080", "gateway URL the replay targets")
	model := fs.String("model", "llama-3-8b", "model name")
	timeoutMs := fs.Int("timeout-ms", 30_000, "per-request timeout in ms")
	matchTol := fs.String("match-tolerance", "0.05", "admission-work match tolerance")
	longThreshold := fs.Int("admission-long-threshold", 4096, "eligible-population token threshold the guard gates on")
	// Provenance the record has to carry, because nothing else can reconstruct it later.
	//
	// RunManifest has declared gatewaySHA and imageDigests since it was written and nothing ever filled them:
	// no flag accepted them, no script passed them, and every manifest this repository has produced left them
	// empty. A paid run's numbers belong to a build, and after the cluster is gone the record is the only
	// place that association can live.
	//
	// The image flags take `name@sha256:...` references. replay -require-provenance refuses a tag, because a
	// tag names whatever was pushed under it most recently.
	gatewaySHA := fs.String("gateway-sha", "", "commit SHA of the gateway build under test")
	gatewayImage := fs.String("gateway-image", "", "digest-pinned gateway image reference (name@sha256:...)")
	engineImage := fs.String("engine-image", "", "digest-pinned inference engine image reference (name@sha256:...)")
	traceOut := fs.String("trace-out", "trace.jsonl", "trace file to write")
	manifestOut := fs.String("manifest-out", "manifest.yaml", "manifest file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Two probe tenants that straddle the guard's eligibility threshold, four characters apart.
	//
	// Without them the trace does not test the threshold at all: the contender estimates at 10,000 tokens and
	// premium at 50, so ANY threshold between 51 and 10,000 produces identical behaviour in every arm, and the
	// number the guard is configured with is decorative. These two make it load-bearing. The gateway scores
	// (chars+3)/4, so 16,380 characters score 4,095 and pass while 16,384 score 4,096 and are rejected -- one
	// real token apart, opposite decisions.
	//
	// They also walk into the false-positive band the calibration measured: both prompts carry about 3,171
	// real tokens against a threshold of 4,096, so the rejected one is rejected on an over-estimate of 29
	// percent. That band was measurable before and unreachable; now the run reports how often it fires.
	//
	// IsNoisy is true for both, and it is not a claim that they are contenders. The field's operational
	// meaning is "not the victim whose tail is the primary endpoint" -- it gates exactly two things, the
	// tail's population and R1's filter, and a probe tenant belongs in neither. Marking them false would put
	// borderline traffic inside the p99 the whole experiment is judged on.
	tenants := []bench.TenantSpec{
		{Tenant: "premium-1", Weight: *premiumWeight, PromptLenChars: *premiumChars, MaxOutputTokens: 64, IsNoisy: false},
		{Tenant: "standard-noisy", Weight: *noisyWeight, PromptLenChars: *noisyChars, MaxOutputTokens: 16, IsNoisy: true},
	}
	if *probeWeight > 0 {
		tenants = append(tenants,
			bench.TenantSpec{Tenant: bench.ProbeUnderTenant, Weight: *probeWeight, PromptLenChars: *probeUnderChars, MaxOutputTokens: 8, IsNoisy: true},
			bench.TenantSpec{Tenant: bench.ProbeOverTenant, Weight: *probeWeight, PromptLenChars: *probeOverChars, MaxOutputTokens: 8, IsNoisy: true},
		)
	}

	rows, err := bench.GenerateTrace(bench.TraceParams{
		Seed: *seed, DurationMs: *durationMs, RatePerSec: *rate, Tenants: tenants,
	})
	if err != nil {
		return fmt.Errorf("generate trace: %w", err)
	}

	// R1 is the uncontended premium baseline.
	//
	// It is the SAME two-tenant trace with the contender filtered out, not a premium-only trace at the full rate.
	//
	// That way premium arrives on the identical schedule it has in the contended arms.
	//
	// So the 1.25x baseline is not inflated by running premium at double its share.
	if *arm == "R1" {
		premiumOnly := rows[:0]
		for _, r := range rows {
			if !r.IsNoisy {
				premiumOnly = append(premiumOnly, r)
			}
		}
		rows = premiumOnly
	}

	var traceBuf strings.Builder
	if err := bench.WriteTrace(&traceBuf, rows); err != nil {
		return fmt.Errorf("serialize trace: %w", err)
	}
	traceBytes := []byte(traceBuf.String())
	if err := os.WriteFile(*traceOut, traceBytes, 0o600); err != nil {
		return fmt.Errorf("write trace %s: %w", *traceOut, err)
	}

	m := bench.RunManifest{
		SchemaVersion:   "v2",
		PromptCorpusSHA: bench.PromptCorpusSHA256,
		Arm:             *arm,
		GatewayURL:      *gatewayURL,
		// Relative to the MANIFEST, not to the working directory.
		//
		// LoadManifest resolves a relative TracePath against the manifest's own directory, which is what
		// makes a run directory portable. gen-trace wrote the flag through verbatim, so
		// `--trace-out hack/run-x/trace.jsonl --manifest-out hack/run-x/manifest.yaml` recorded a
		// working-directory path that the loader then joined onto hack/run-x again:
		//
		//   read trace file hack/run-x/hack/run-x/trace-R1-1.jsonl: no such file or directory
		//
		// The paid run failed on its first replay because of it. Storing the path the loader expects keeps
		// the manifest movable, which is the point of recording a checksum beside it.
		TracePath:       traceRefFor(*manifestOut, *traceOut),
		TraceChecksum:   bench.Checksum(traceBytes),
		Model:           *model,
		TimeoutMs:       *timeoutMs,
		Seed:            *seed,
		PrimaryEndpoint: "ttft_p99",
		MatchTolerance:  *matchTol,
		LongThreshold:   *longThreshold,
		GatewaySHA:      *gatewaySHA,
	}
	// Only set the map when something was supplied, so a free run's manifest carries no empty scaffolding
	// that could later be mistaken for a recorded value.
	for role, ref := range map[string]string{"gateway": *gatewayImage, "engine": *engineImage} {
		if ref == "" {
			continue
		}
		if m.ImageDigests == nil {
			m.ImageDigests = map[string]string{}
		}
		m.ImageDigests[role] = ref
	}
	if err := writeManifest(*manifestOut, m); err != nil {
		return err
	}
	fmt.Printf("wrote %d trace rows to %s and manifest %s (arm=%s)\n", len(rows), *traceOut, *manifestOut, *arm)
	return nil
}

// replay loads a manifest, verifies its trace checksum, and replays the trace against the gateway.
func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "manifest to replay")
	rawOut := fs.String("raw-out", "raw.jsonl", "raw evidence file to write")
	target := fs.String("target", "", "override the manifest gateway URL (e.g. a stub)")
	apiKeys := fs.String("api-keys", "", "comma-separated tenant=key pairs")
	// The pool mode is a flag rather than a constant so the cost of pooling at all stays measurable.
	//
	// "legacy" reproduces the client this sender replaced, which used http.DefaultTransport and its two idle
	// connections per host. It exists for a before/after comparison and for nothing else; a run that reports
	// latency should always use the derived pool. The name written here used to be "go-default", which the
	// flag has never accepted, so an operator copying it out of this rationale got "unknown sender mode".
	// The paid path's counterpart to -require-device.
	//
	// Opt-in rather than always-on: a kind run against a stub has no build worth pinning, and demanding one
	// from every free run would push an operator toward inventing a value. Where the record has to outlive
	// the cluster, it is required.
	requireProvenance := fs.Bool("require-provenance", false,
		"refuse a manifest that does not name the gateway build and digest-pin every image the number depends on")
	connMode := fs.String("conn-mode", bench.SenderModePooled,
		"client connection handling: \"pooled\" (pool sized from the run, plus the drain that lets it be "+
			"used), \"drain-only\", or \"legacy\" (the pre-fix client: http.DefaultTransport, no drain)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	m, err := bench.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	if *requireProvenance {
		if perr := m.RequireProvenance(); perr != nil {
			return fmt.Errorf("manifest %s: %w", *manifestPath, perr)
		}
	}
	rows, err := readTraceFile(m.TracePath)
	if err != nil {
		return err
	}
	url := m.GatewayURL
	if *target != "" {
		url = *target
	}
	timeout := time.Duration(m.TimeoutMs) * time.Millisecond
	conn, err := bench.SenderConnForMode(*connMode, rows, timeout)
	if err != nil {
		return err
	}
	// Printed, not just applied: which client a run used is part of what its raw evidence means, and this
	// line is what puts it in the evidence log beside the numbers it produced.
	fmt.Printf("client connection mode: %s (MaxIdleConnsPerHost=%d, drain=%t) for %d rows at timeout %s\n",
		*connMode, conn.MaxIdleConnsPerHost, conn.DrainForReuse, len(rows), timeout)
	sender := bench.NewHTTPSender(url, m.Model, parseAPIKeys(*apiKeys), timeout, conn)

	// The frozen manifest's provenance is stamped into every raw row.
	//
	// That lets the report enforce trace identity and read the pre-registered knobs from the evidence.
	tol, err := strconv.ParseFloat(m.MatchTolerance, 64)
	if err != nil {
		return fmt.Errorf("manifest matchTolerance %q is not a number: %w", m.MatchTolerance, err)
	}
	raw := bench.Replay(context.Background(), sender, rows, bench.ReplayOptions{
		Arm:            m.Arm,
		TraceChecksum:  m.TraceChecksum,
		LongThreshold:  m.LongThreshold,
		MatchTolerance: tol,
	})

	f, err := os.Create(*rawOut)
	if err != nil {
		return fmt.Errorf("create raw file %s: %w", *rawOut, err)
	}
	defer func() { _ = f.Close() }()
	if err := bench.WriteRawRows(f, raw); err != nil {
		return err
	}
	fmt.Printf("replayed %d rows against %s (arm=%s) to %s\n", len(raw), url, m.Arm, *rawOut)
	return nil
}

// report turns one raw file per arm into the design's pre-registered report.
//
// Each --raw file is one arm's evidence; multiple files for the same arm are treated as repetitions for the bootstrap.
func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var rawFiles multiFlag
	fs.Var(&rawFiles, "raw", "a raw evidence file (repeatable, one arm per file)")
	matchTolFlag := fs.Float64("match-tolerance", 0.05,
		"fallback admission-work match tolerance when the evidence carries none")
	out := fs.String("out", "", "report file to write (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(rawFiles) == 0 {
		return fmt.Errorf("at least one --raw file is required")
	}

	e, err := loadArmEvidence(rawFiles)
	if err != nil {
		return err
	}
	if err := e.refuseIfTracesDisagree(); err != nil {
		return err
	}
	matchTolerance := e.frozenMatchTolerance(*matchTolFlag)
	summaries, summ := e.summarize()
	incCI := e.incrementalCI()

	// Name the arms that are absent, because the refusal downstream cannot.
	//
	// summ is a map, so a missing arm yields the zero ArmSummary, whose TailSampleSize is 0, and
	// EvaluateChecks disqualifies the comparison on exactly that. The refusal therefore WORKS -- a session cut
	// short by a reclaimed node or an aborted run cannot be reported as a result. Two independent reviews
	// asserted the opposite, and reading this path is what settled it.
	//
	// What the refusal cannot do is say WHICH arm. Its message interpolates s.Arm, which on a zero value is the
	// empty string, so the operator reads "arm  completed no premium requests" and has to work out from the
	// record directory what is missing. That is a bad minute to spend at the end of a paid session, and it is
	// the difference between a gate that stops you and a gate that tells you what to re-run.
	var missing []string
	for _, arm := range []string{"R1", "static-cap", "kv-aware"} {
		if _, ok := summ[arm]; !ok {
			missing = append(missing, arm)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "warning: no records for arm(s) %s; the comparison will be disqualified\n",
			strings.Join(missing, ", "))
	}

	checks := bench.EvaluateChecks(summ["R1"], summ["static-cap"], summ["kv-aware"], incCI, matchTolerance)
	text := bench.FormatReport(summaries, checks, matchTolerance)

	if *out != "" {
		if err := os.WriteFile(*out, []byte(text), 0o600); err != nil {
			return fmt.Errorf("write report %s: %w", *out, err)
		}
		fmt.Printf("wrote report to %s\n", *out)
	} else {
		fmt.Print(text)
	}

	// An INVALID run exits non-zero. A run that merely fails its checks does not.
	//
	// The distinction is the whole point. "Not all checks passed" is a scientific result -- the guard did not
	// protect, and that is a finding worth recording. "Run invalid" means the evidence cannot answer the
	// question at all, and the paid wrapper was treating the two identically: hack/m5b-arms.sh runs
	//
	//     benchharness report ... || fail "report"
	//
	// and this command returned 0 for both, so a session whose evidence had been disqualified printed a
	// success line and moved on to the next arm. Every hole closed today -- the missing arm, the transport
	// censoring, the absent interval, the thin repetition, the short recording -- reaches the operator through
	// this exit code, and until now none of them did.
	//
	// The report file is still written first, so the refusal is preserved as evidence rather than discarded.
	if checks.Invalid {
		return fmt.Errorf("run invalid: %s", checks.InvalidReason)
	}
	return nil
}

// armEvidence is every arm's rows plus the per-repetition shape the pooled rows cannot carry.
//
// It exists because report's refusals ask questions of the SPLIT -- did each repetition record the same
// number of rows, is any repetition's tail thin -- and pooling answers none of them.
type armEvidence struct {
	byArm     map[string][]bench.RawRow
	repP99    map[string][]float64
	repTail   map[string][]int
	repRows   map[string][]int
	checksum  map[string]string
	tolerance map[string]float64
}

// loadArmEvidence reads one raw file per repetition and groups it by arm.
func loadArmEvidence(rawFiles []string) (*armEvidence, error) {
	// Group rows and per-file p99 repetitions by arm, and capture each arm's frozen provenance.
	e := &armEvidence{
		byArm: map[string][]bench.RawRow{}, repP99: map[string][]float64{},
		repTail: map[string][]int{}, repRows: map[string][]int{},
		checksum: map[string]string{}, tolerance: map[string]float64{},
	}
	for _, path := range rawFiles {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		rows, err := bench.ReadRawRows(f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue
		}
		arm := rows[0].Arm
		e.byArm[arm] = append(e.byArm[arm], rows...)
		// Keep the whole per-repetition summary, not just its p99.
		//
		// This line used to discard everything except TTFTMsP99, which is how a truncated repetition became
		// invisible: the pooled arm summary sums every row, so three healthy repetitions carry a fourth whose
		// p99 is a maximum over thirty requests -- and the bootstrap then resamples that fourth value with
		// equal weight.
		rs := bench.Summarize(arm, rows)
		e.repP99[arm] = append(e.repP99[arm], rs.TTFTMsP99)
		e.repTail[arm] = append(e.repTail[arm], rs.TailSampleSize)
		e.repRows[arm] = append(e.repRows[arm], len(rows))
		e.checksum[arm] = rows[0].TraceChecksum
		e.tolerance[arm] = rows[0].MatchTolerance
	}
	return e, nil
}

// refuseIfTracesDisagree stops a comparison whose arms did not replay the same trace, or did not finish
// replaying it.
func (e *armEvidence) refuseIfTracesDisagree() error {
	// The contended arms must have replayed identical traffic, so their trace checksums must match.
	//
	// R1 legitimately differs (it is the same trace with the contender filtered out).
	//
	// So it is excluded from the identity check.
	var wantSum string
	for _, arm := range []string{"off", "static-cap", "kv-aware"} {
		sum, ok := e.checksum[arm]
		if !ok {
			continue
		}
		if sum == "" {
			return fmt.Errorf("arm %s carries no trace checksum; regenerate its manifest with a current gen-trace", arm)
		}
		if wantSum == "" {
			wantSum = sum
		} else if sum != wantSum {
			return fmt.Errorf("arm %s replayed a different trace (%s) than the other contended arms (%s);"+
				" a valid comparison needs one immutable trace", arm, sum, wantSum)
		}
	}

	// The same trace must also have produced the same NUMBER of records.
	//
	// The checksum above proves the arms replayed identical traffic. It says nothing about whether the
	// recording of that traffic finished, and the two are different questions with no shared symptom: a run
	// cut off partway -- a reclaimed node, a killed port-forward, a resumed session, a short duration -- leaves
	// every row it did write COMPLETE. Nothing is censored, no repetition is thin, the tail clears its floor,
	// and the arm is simply shorter than the trace it claims to have replayed.
	//
	// The direction is what makes this the worst of the set. Contention builds over a trace, so the requests
	// that arrive late are the slow ones; dropping the tail of the recording drops exactly the evidence that
	// would fail the arm. An adversarial review demonstrated it on this binary: complete evidence printed
	//
	//     absolute protection  C/R1 = 3.434  FAIL   ...  VERDICT: not all checks passed
	//
	// and the identical evidence truncated to its first 60% of rows printed
	//
	//     absolute protection  C/R1 = 1.066  PASS   ...  VERDICT: all checks passed; the guard protects
	//
	// with kv-aware at 960 recorded rows against 1,600 for the two arms it shares a trace with, unremarked.
	// A genuine FAIL certified as protection is the one outcome this study must never produce.
	//
	// This is a refusal rather than a check result, and it sits beside the checksum test for that reason: two
	// arms of different lengths cannot be compared at all, which is a statement about the evidence rather than
	// about the guard.
	contended := []string{"off", "static-cap", "kv-aware"}
	wantRows, wantFrom := 0, ""
	for _, arm := range contended {
		for i, n := range e.repRows[arm] {
			if wantRows == 0 {
				wantRows, wantFrom = n, fmt.Sprintf("%s[%d]", arm, i)
				continue
			}
			if n != wantRows {
				return fmt.Errorf("arm %s repetition %d recorded %d rows but %s recorded %d;"+
					" these arms share one immutable trace, so a shorter recording means a run that did not finish,"+
					" and the requests it is missing are the late ones contention makes slow",
					arm, i, n, wantFrom, wantRows)
			}
		}
	}

	// R1 replays the same trace with the contender filtered out, so its count legitimately differs from the
	// contended arms -- but not from itself.
	for i, n := range e.repRows["R1"] {
		if n != e.repRows["R1"][0] {
			return fmt.Errorf("arm R1 repetition %d recorded %d rows but repetition 0 recorded %d;"+
				" its repetitions replay one trace and must record the same number of rows",
				i, n, e.repRows["R1"][0])
		}
	}
	return nil
}

// frozenMatchTolerance prefers the tolerance recorded in the evidence over the CLI default.
func (e *armEvidence) frozenMatchTolerance(fallback float64) float64 {
	// The admission-match tolerance is the frozen one from the evidence, not a CLI default.
	//
	// So it cannot be loosened after the fact.
	matchTolerance := fallback
	if tol, ok := e.tolerance["kv-aware"]; ok && tol > 0 {
		matchTolerance = tol
	} else if tol, ok := e.tolerance["static-cap"]; ok && tol > 0 {
		matchTolerance = tol
	}
	return matchTolerance
}

// summarize builds the per-arm summaries in report order and attaches the repetition shape.
func (e *armEvidence) summarize() ([]bench.ArmSummary, map[string]bench.ArmSummary) {
	order := []string{"R1", "off", "static-cap", "kv-aware"}
	var summaries []bench.ArmSummary
	summ := map[string]bench.ArmSummary{}
	for _, arm := range order {
		if rows, ok := e.byArm[arm]; ok {
			s := bench.Summarize(arm, rows)
			// Summarize sees pooled rows and cannot know how they were split, so the repetition shape is
			// attached here where the split is known.
			if tails := e.repTail[arm]; len(tails) > 0 {
				s.RepetitionCount = len(tails)
				s.MinRepetitionTail = slices.Min(tails)
			}
			summaries = append(summaries, s)
			summ[arm] = s
		}
	}
	return summaries, summ
}

// incrementalCI resamples per-repetition C/B ratios, and warns when the repetitions do not line up.
func (e *armEvidence) incrementalCI() bench.CI {
	// The incremental C/B CI resamples per-repetition ratios when both arms have matching repetitions.
	//
	// With a single repetition per arm it degenerates to the point estimate.
	incCI := bench.CI{}
	b, c := e.repP99["static-cap"], e.repP99["kv-aware"]
	if len(b) == len(c) && len(b) > 0 {
		ratios := make([]float64, len(b))
		for i := range b {
			if b[i] > 0 {
				ratios[i] = c[i] / b[i]
			}
		}
		incCI = bench.BootstrapCI(ratios, 2000, 1, 0.05)
	} else if len(b) > 0 && len(c) > 0 {
		// Unequal repetition counts leave the incremental CI at the degenerate point estimate.
		//
		// That would make the CI-upper-bound gate trivially true.
		// Left as a warning here on purpose: the refusal now lives in the checks rather than in this branch.
		//
		// It used to be the ONLY response to unequal repetitions, and it refused nothing -- the caller then
		// passed the zero CI into EvaluateChecks, whose incremental gate reads `Hi < 1.0`, and 0.0 satisfies
		// it. Truncation disarmed the strictest check in the design instead of tripping it. CI.Valid closes
		// that; this line stays so the operator learns WHY the run was refused without reading the code.
		fmt.Fprintf(os.Stderr, "warning: static-cap has %d repetitions but kv-aware has %d;"+
			" no incremental CI will be computed and the comparison will be refused\n", len(b), len(c))
	}
	return incCI
}

// printPrompt writes the exact prompt text a trace row of the given length produces, and nothing else.
//
// It exists so a session can MEASURE with the bytes it will later SEND. The paid run derives its arrival
// rate from one contender prefill against an idle engine, and a prefill's cost is in tokens rather than
// characters -- so measuring with a stand-in payload measures the wrong thing. A run of one character is
// the worst possible stand-in: a byte-pair tokenizer collapses 40,000 of them to about 5,000 tokens where
// the corpus gives 7,695, so the card would look half again faster than it is and the derived rate would
// oversubscribe it.
func printPrompt(args []string) {
	fs := flag.NewFlagSet("print-prompt", flag.ExitOnError)
	chars := fs.Int("chars", 0, "prompt length in characters")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *chars <= 0 {
		fmt.Fprintln(os.Stderr, "print-prompt: --chars must be positive")
		os.Exit(2)
	}
	fmt.Print(bench.PromptText(*chars))
}
