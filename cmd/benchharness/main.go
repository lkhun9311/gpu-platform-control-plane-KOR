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
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	case "power":
		err = power(os.Args[2:])
	case "stamp-exact-tokens":
		err = stampExactTokens(os.Args[2:])
	case "check-replay":
		err = checkReplay(os.Args[2:])
	case "ladder-verdict":
		err = ladderVerdict(os.Args[2:])
	case "ladder-plan-check":
		err = ladderPlanCheck(os.Args[2:])
	case "sim-cap":
		err = simCap(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "usage: benchharness <gen-trace|replay|report|ladder-verdict|ladder-plan-check|print-prompt|check-replay|stamp-exact-tokens|sim-cap|power|stub-serve> [flags]")
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
	// Meant to be small, and 0.1 is NOT small the way this comment used to claim.
	//
	// These are a probe population rather than a load driver, and each carries about 3,171 real tokens. The
	// comment here said a large share would move the pressure the arms are supposed to differ under, and
	// then left a default that does exactly that: measured on the 2026-09-07 price-of-protection pilot, the
	// two probe tenants at 0.1 each carried 78% of the engine's prefill capacity between them, against the
	// protected tenant's 5%. A weight is a share of the total arrival rate, so what it costs the engine
	// depends on the prompt behind it, and 0.1 of a 3,171-token prompt is not 0.1 of the load.
	//
	// Left at 0.1 because lowering it silently would change every existing caller's trace. Set it from the
	// engine's measured prefill capacity, as
	// docs/superpowers/specs/2026-09-08-the-load-needs-an-upper-gate.md derives.
	probeWeight := fs.Float64("probe-weight", 0.1, "arrival share of EACH threshold-probe tenant; 0 disables them (see the comment: 0.1 is not a small load)")
	probeUnderChars := fs.Int("probe-under-chars", bench.ProbeUnderChars, "probe prompt scoring just BELOW the guard threshold")
	probeOverChars := fs.Int("probe-over-chars", bench.ProbeOverChars, "probe prompt scoring exactly AT the guard threshold")
	// Defaulted rather than required, because every existing caller is the M5-b gateway experiment and
	// making them all pass a flag to keep working would be a migration with no reader.
	study := fs.String("study", bench.StudyM5BGateway,
		"pre-registered experiment this manifest belongs to; its registry decides which arms are admissible")
	arm := fs.String("arm", "off", "arm this manifest measures, one of the named study's arms")
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
		{Tenant: bench.NoisyTenant, Weight: *noisyWeight, PromptLenChars: *noisyChars, MaxOutputTokens: 16, IsNoisy: true},
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
	if bench.IsIsolatedBaseline(*arm) {
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
		Study:           *study,
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
	// The axis the paid evidence showed actually works, exposed so an arm can carry it.
	//
	// vLLM's priority scheduler reads a per-request field, and the microtest measured it moving the premium
	// tail where every backend-telemetry signal the gateway could see did not. Without this flag the runner
	// can only test admission, which is the axis that failed.
	priorities := fs.String("priorities", "",
		"comma-separated tenant=priority pairs sent with each request (lower is more urgent); "+
			"requires the engine to run with --scheduling-policy=priority")
	connMode := fs.String("conn-mode", bench.SenderModePooled,
		"client connection handling: \"pooled\" (pool sized from the run, plus the drain that lets it be "+
			"used), \"drain-only\", or \"legacy\" (the pre-fix client: http.DefaultTransport, no drain)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Parsed before anything is loaded or dialled, so a typo in the treatment costs nothing.
	prio, err := parsePriorities(*priorities)
	if err != nil {
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
	// Printed for the same reason the connection mode is: which arm this replay actually was is part of what
	// its rows mean, and a priority map that silently arrived empty must be visible in the run log.
	if len(prio) > 0 {
		sender.SetPriorities(prio)
		fmt.Printf("request priorities: %v\n", prio)
	} else {
		fmt.Printf("request priorities: none (requests carry no priority field)\n")
	}

	// The frozen manifest's provenance is stamped into every raw row.
	//
	// That lets the report enforce trace identity and read the pre-registered knobs from the evidence.
	tol, err := strconv.ParseFloat(m.MatchTolerance, 64)
	if err != nil {
		return fmt.Errorf("manifest matchTolerance %q is not a number: %w", m.MatchTolerance, err)
	}
	raw := bench.Replay(context.Background(), sender, rows, bench.ReplayOptions{
		Study:          m.Study,
		Arm:            m.Arm,
		Priorities:     prio,
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
// ladderPlanCheck asks, of a trace that has been generated but not replayed, whether the cell it describes
// could ever be scored.
//
// It takes a generated trace file rather than counts on the command line, so the thing being checked is the
// artefact the run will actually replay and not a number somebody typed twice.
func ladderPlanCheck(args []string) error {
	fs := flag.NewFlagSet("ladder-plan-check", flag.ExitOnError)
	trace := fs.String("trace", "", "the generated trace file to check")
	study := fs.String("study", "", "the study the cell belongs to")
	arm := fs.String("arm", "", "the arm name the cell will record")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *trace == "" || *study == "" || *arm == "" {
		return fmt.Errorf("--trace, --study and --arm are all required")
	}
	tf, err := os.Open(*trace)
	if err != nil {
		return fmt.Errorf("open trace %s: %w", *trace, err)
	}
	defer func() { _ = tf.Close() }()
	rows, err := bench.ReadTrace(tf)
	if err != nil {
		return fmt.Errorf("read trace %s: %w", *trace, err)
	}
	premium, contender := 0, 0
	for _, r := range rows {
		switch r.Tenant {
		case bench.PremiumTenant:
			premium++
		case bench.NoisyTenant:
			contender++
		}
	}
	if perr := bench.LadderPlanRefusal(*study, *arm, premium, contender); perr != nil {
		return fmt.Errorf("%s: %w", *arm, perr)
	}
	fmt.Printf("%s: %d premium, %d contender -- scorable\n", *arm, premium, contender)
	return nil
}

// ladderVerdictStop is the exit code that tells the runner to stop climbing.

// ladderVerdictStop is the exit code that tells the runner to stop climbing.
//
// A distinct code rather than a non-zero, because the runner must tell "both topologies breached, which is
// what we were climbing to find" apart from "something went wrong". The first is the successful end of a
// ladder and the second must not be mistaken for it.
const ladderVerdictStop = 10

// ladderVerdict answers the one question the runner asks between rungs: climb again, or stop here?
//
// It exists so that the stopping rule lives in the same package as the criterion it applies. The
// alternative was a shell script reading a p99 out of a table and comparing it to a threshold written down
// twice -- and a threshold written down twice is a threshold that will eventually differ, in the direction
// whoever edits it wants.
//
// It prints one line as well as setting an exit code. The line is what the runner asserts on, so a change
// to either alone is caught rather than silently obeyed.
func ladderVerdict(args []string) error {
	fs := flag.NewFlagSet("ladder-verdict", flag.ExitOnError)
	var rawFiles multiFlag
	fs.Var(&rawFiles, "raw", "a raw evidence file (repeatable); pass every ladder cell measured so far")
	rung := fs.Int("rung", 0, "the rung just measured")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(rawFiles) == 0 {
		return fmt.Errorf("at least one --raw file is required")
	}
	if *rung <= 0 {
		return fmt.Errorf("--rung is required and must name the rung just measured")
	}
	e, err := loadArmEvidence(rawFiles)
	if err != nil {
		return err
	}
	// The SAME evidence refusal the report applies, before a purchase decision is made on the evidence.
	//
	// It was absent here, so a rung whose two cells replayed different traces -- which `report` refuses with
	// a non-zero exit -- produced LADDER: CONTINUE and bought the next rung. The cheap decision accepted
	// evidence the expensive one would throw away.
	if terr := e.refuseIfTracesDisagree(); terr != nil {
		fmt.Println("LADDER: INVALID")
		return fmt.Errorf("rung %d cannot be scored: %w", *rung, terr)
	}
	summaries, _ := e.summarize()
	res := bench.EvaluateThroughputLadder(summaries)

	// An unscorable cell stops the ladder for a different reason than a breach does, and says so.
	for _, r := range res.Readings {
		if r.Fired && r.ID == "L0" {
			fmt.Println("LADDER: INVALID")
			return fmt.Errorf("rung %d cannot be scored: %s", *rung, r.Detail)
		}
	}
	// An INCOMPLETE requested rung is not a stopping point, and the caller cannot tell the two apart from a
	// boolean. Asking about a rung whose pair is not in the evidence used to print STOP and exit 10, which
	// the runner acts on by ending the climb -- a missing cell reported as a registered result.
	if !bench.LadderRungComplete(res, *rung) {
		fmt.Println("LADDER: INVALID")
		return fmt.Errorf("rung %d is not complete in this evidence, so there is no verdict to give: both contended topologies must be present and scorable", *rung)
	}
	cont, detail := bench.LadderShouldContinue(res, *rung)
	if cont {
		fmt.Println("LADDER: CONTINUE")
		fmt.Fprintln(os.Stderr, detail)
		return nil
	}
	fmt.Println("LADDER: STOP")
	fmt.Fprintln(os.Stderr, detail)
	os.Exit(ladderVerdictStop)
	return nil
}

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var rawFiles multiFlag
	fs.Var(&rawFiles, "raw", "a raw evidence file (repeatable, one arm per file)")
	matchTolFlag := fs.Float64("match-tolerance", 0.05,
		"fallback admission-work match tolerance when the evidence carries none")
	out := fs.String("out", "", "report file to write (default stdout)")
	// A machine-readable copy, because the paid raw evidence is gitignored and 7 MB.
	//
	// A number quoted in a write-up whose source is not in the repository is a number nobody can re-derive,
	// and every overclaim this project has had to withdraw took that shape. This file is what a spec's table
	// is checked against, so the derivation runs from the report code rather than from a hand copy.
	jsonOut := fs.String("json-out", "", "machine-readable per-arm summary to write alongside the report")
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
	// The three checks below are M5-b's pre-registered comparison, and they name M5-b's arms.
	//
	// Running them for another experiment asked for static-cap and kv-aware, found neither, and reported
	// the run disqualified -- a verdict about arms the experiment never had. Another study's readings are
	// its own, so until they are implemented the report prints that study's tables and says plainly that
	// it evaluated no criteria, rather than failing it against somebody else's.
	checks, pop, sharing, ladder := evaluateRegisteredReadings(e, summ, summaries, rawFiles, incCI, matchTolerance)
	text := bench.FormatReport(summaries, checks, matchTolerance)
	if pop != nil {
		text += bench.FormatPriceOfProtection(*pop)
	}
	if sharing != nil {
		text += bench.FormatSharingMatrix(*sharing)
	}
	if ladder != nil {
		text += bench.FormatThroughputLadder(*ladder)
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(text), 0o600); err != nil {
			return fmt.Errorf("write report %s: %w", *out, err)
		}
		fmt.Printf("wrote report to %s\n", *out)
	} else {
		fmt.Print(text)
	}

	if *jsonOut != "" {
		enc, merr := json.MarshalIndent(summaries, "", "  ")
		if merr != nil {
			return fmt.Errorf("encode summaries: %w", merr)
		}
		if werr := os.WriteFile(*jsonOut, append(enc, '\n'), 0o600); werr != nil {
			return fmt.Errorf("write %s: %w", *jsonOut, werr)
		}
		fmt.Printf("wrote per-arm summaries to %s\n", *jsonOut)
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
	//
	// A study with no implemented readings reaches here with nil checks. It cannot be INVALID, because
	// nothing evaluated it -- and it must not be reported as valid either. The exit code says only what this
	// binary actually decided, and the report itself says the criteria were not evaluated.
	if checks != nil && checks.Invalid {
		return fmt.Errorf("run invalid: %s", checks.InvalidReason)
	}
	// The price-of-protection study reaches here with nil checks, so its INVALID readings used to exit 0 --
	// and the paid runner calls this as `benchharness report ... || fail`, which is the whole reason the
	// comment above says an invalid run exits non-zero. A run whose load made no contention, or whose load
	// was too high to measure, printed its refusal and told the wrapper it had succeeded.
	// The sharing study's INVALID readings exit non-zero too, for the reason the price-of-protection block
	// below gives: automation that writes `benchharness report ... || fail` would otherwise accept a run the
	// readings had just declared unusable.
	if sharing != nil {
		if err := sharingRunInvalid(*sharing); err != nil {
			return err
		}
	}
	if pop != nil {
		for _, r := range pop.Readings {
			if r.Fired && (r.ID == "4" || r.ID == "4b") {
				return fmt.Errorf("run invalid: reading %s fired -- %s", r.ID, r.Detail)
			}
		}
	}
	// The ladder's L0 is its only INVALID reading, and the distinction it draws is the one the runner acts
	// on: a cell that could not be scored must stop the ladder, and a rung where both topologies BREACHED
	// must not -- a breach is what the ladder is climbing to find.
	//
	// L5 is the same kind of result. "No qualified operating point at or above the bottom rung" is a
	// finding about where the answer lies, not evidence that failed, and exiting non-zero for it would tell
	// a wrapper that writes `benchharness report ... || fail` to discard a rung it paid for.
	if ladder != nil {
		for _, r := range ladder.Readings {
			if r.Fired && r.ID == "L0" {
				return fmt.Errorf("run invalid: reading L0 fired -- %s", r.Detail)
			}
		}
		// A FINAL report needs the baseline; a between-rung verdict does not, which is why this is here and
		// not in L0. ladderVerdict deliberately does not consult it.
		if ladder.BaselineMissing {
			return fmt.Errorf("run invalid: %s", ladder.BaselineNote)
		}
	}
	return nil
}

// armEvidence is every arm's rows plus the per-repetition shape the pooled rows cannot carry.
//
// It exists because report's refusals ask questions of the SPLIT -- did each repetition record the same
// number of rows, is any repetition's tail thin -- and pooling answers none of them.
type armEvidence struct {
	byArm   map[string][]bench.RawRow
	repP99  map[string][]float64
	repTail map[string][]int
	// repDone is each repetition's completed count per tenant: arm -> one map per repetition.
	//
	// repTail carries only the premium tenant's, so reading 4b's contender floor could be applied to the
	// POOL and nothing else. An independent review reproduced what that allows: repetitions of 140, 140 and
	// 50 contender completions, each offered 140, clear a hundred-completion floor at 330 pooled and fire
	// reading 1 POSITIVE -- on a run containing one block the registration calls invalid.
	repDone map[string][]map[string]int
	// replayFrom maps a replay's IDENTITY to the file that carried it, so the same replay cannot be
	// counted twice as two repetitions.
	//
	// A repetition is supposed to be an independent measurement. Nothing stopped the same file being
	// passed twice, or copied under a second name: both produced RepetitionCount=2, equal counts across
	// arms, and a control whose repetition-to-repetition spread is EXACTLY ZERO -- which is the threshold
	// readings 3 and 5 compare an improvement against. A review reproduced it by passing each of the eighth
	// pilot's files twice, and reading 5 fired on single-repetition evidence.
	//
	// The identity is the arm plus every row's send timestamp. Two real replays of the same trace start at
	// different nanoseconds; a copy is bit-identical. The trace checksum cannot do this job -- repetitions
	// of one arm are SUPPOSED to share it, and the check beside this one refuses them when they do not.
	replayFrom map[string]string
	// repServed is each repetition's completed/offered per tenant, which the counts alone cannot express.
	repServed map[string][]map[string]float64
	// repCensored records whether ANY of an arm's repetitions was censored, which pooling hides.
	repCensored map[string]bool
	repRows     map[string][]int
	// repSeconds is each repetition's own wall clock, kept because the arm's throughput must be its tokens
	// over the time it was actually sending -- not over a pooled span that includes the washout pauses
	// between repetitions.
	repSeconds map[string][]float64
	checksum   map[string]string
	tolerance  map[string]float64
	// treatment is the canonical rendering of the priorities each arm's rows carried, so two replays that
	// differ only in treatment cannot be pooled as repetitions of one condition.
	treatment map[string]string
	// study is the one pre-registered experiment every row in this report came from, and studyFrom is
	// the file that established it, so a refusal can name both sides of the disagreement.
	//
	// studySeen is separate because the empty string is a legitimate value here -- it is what every raw
	// file written before the study field existed carries. Using "" as the not-yet-set sentinel made
	// unlabelled evidence silently adopt whatever study the next file named, which is the one mixing
	// this check exists to prevent.
	study string
	// studyRecorded is what the file literally carried, kept alongside the normalized value so a refusal
	// can say "unlabelled" instead of silently presenting old evidence as if it had named a study.
	studyRecorded string
	studySeen     bool
	studyFrom     string
}

// singleStudy returns the one study every row in a file belongs to, or refuses.
//
// Normalization is applied before comparing, so an unlabelled legacy file and one that names
// m5b-gateway-v1 explicitly are the same study rather than two.
// singleArm is singleStudy's twin: a raw file is ONE arm, and every row has to say so.
//
// It also checks the trace checksum, because the two travel together -- rows from another cell carry both a
// different arm and a different trace, and a file doctored to fix one would still fail the other.
func singleArm(path string, rows []bench.RawRow) (string, error) {
	arm, sum := rows[0].Arm, rows[0].TraceChecksum
	for i, r := range rows {
		if r.Arm != arm {
			return "", fmt.Errorf("%s mixes arms within one file: row 0 carries %q but row %d carries %q;"+
				" a raw file is one arm of one experiment, and pooling two of them reports a cell nobody ran",
				path, arm, i, r.Arm)
		}
		if r.TraceChecksum != sum {
			return "", fmt.Errorf("%s mixes traces within one file: row 0 carries checksum %s but row %d carries %s;"+
				" one replay of one immutable trace is what a raw file records",
				path, sum, i, r.TraceChecksum)
		}
	}
	return arm, nil
}

func singleStudy(path string, rows []bench.RawRow) (canonical, recorded string, err error) {
	recorded = rows[0].Study
	canonical = bench.CanonicalStudyID(recorded)
	for i, r := range rows {
		if got := bench.CanonicalStudyID(r.Study); got != canonical {
			return "", "", fmt.Errorf("%s mixes studies within one file: row 0 carries %s but row %d carries %s;"+
				" a raw file is one arm of one experiment",
				path, studyLabel(canonical, recorded), i, studyLabel(got, r.Study))
		}
	}
	return canonical, recorded, nil
}

// treatmentOf renders the priorities a file's rows carried, as a stable string.
//
// Derived from the rows rather than from a flag the runner claims to have passed, because the point is to
// describe what the requests actually carried. Sorted so the same treatment always renders identically.
func treatmentOf(rows []bench.RawRow) string {
	seen := map[string]int{}
	for _, r := range rows {
		if r.Priority != nil {
			seen[r.Tenant] = *r.Priority
		}
	}
	if len(seen) == 0 {
		return "none"
	}
	tenants := make([]string, 0, len(seen))
	for t := range seen {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	parts := make([]string, 0, len(tenants))
	for _, t := range tenants {
		parts = append(parts, fmt.Sprintf("%s=%d", t, seen[t]))
	}
	return strings.Join(parts, ",")
}

// studyLabel renders a study for a refusal message, disclosing when the file did not name one.
//
// The comparison normalizes an empty study to the M5-b experiment, because unlabelled evidence IS that
// experiment and refusing to pool it with evidence that says so would reject a legitimate comparison. The
// message must not normalize as well: an operator reading "these are different experiments" needs to know
// that one side was old evidence carrying no label, not a file naming a study they do not recognise.
func studyLabel(canonical, recorded string) string {
	if recorded == "" {
		return canonical + " (unlabelled, predates the study field)"
	}
	return canonical
}

// loadArmEvidence reads one raw file per repetition and groups it by arm.
func loadArmEvidence(rawFiles []string) (*armEvidence, error) {
	// Group rows and per-file p99 repetitions by arm, and capture each arm's frozen provenance.
	e := &armEvidence{
		byArm: map[string][]bench.RawRow{}, repP99: map[string][]float64{},
		repTail: map[string][]int{}, repRows: map[string][]int{}, repSeconds: map[string][]float64{},
		repDone:     map[string][]map[string]int{},
		replayFrom:  map[string]string{},
		repCensored: map[string]bool{},
		repServed:   map[string][]map[string]float64{},
		checksum:    map[string]string{}, tolerance: map[string]float64{},
		treatment: map[string]string{},
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
		// Two studies in one report is not a comparison, it is a category error.
		//
		// Nothing else catches it. Arm names are not unique across studies, and the trace checksum does
		// not stand in for study identity either: refuseIfTracesDisagree only inspects three hard-coded
		// M5-b arm names and skips anything it does not recognise, so rows from a different experiment
		// pass through it untouched. This is the check, and it runs before a single row is pooled.
		// EVERY row, not the first one.
		//
		// Reading rows[0] made the check defeatable by concatenation: a file whose first row is legacy and
		// whose remaining rows belong to another experiment passed, and ReadRawRows sorts by index so which
		// row lands first is not even under the operator's control among ties.
		study, recorded, err := singleStudy(path, rows)
		if err != nil {
			return nil, err
		}
		if !e.studySeen {
			e.study = study
			e.studyRecorded = recorded
			e.studySeen = true
			e.studyFrom = path
		} else if study != e.study {
			return nil, fmt.Errorf("%s carries study %s but %s carries %s; these are different"+
				" pre-registered experiments and their arms do not mean the same thing, so pooling them"+
				" would report a comparison that was never run",
				path, studyLabel(study, recorded), e.studyFrom, studyLabel(e.study, e.studyRecorded))
		}

		// EVERY row's arm and trace, for the reason every row's study is checked: a file is one arm of one
		// experiment, and reading row zero made both defeatable by concatenation. An adversarial review
		// appended one arm's rows to another's and watched the pooled p99 move from 1,694.7 ms to 1,179.3
		// while the report exited 0.
		arm, err := singleArm(path, rows)
		if err != nil {
			return nil, err
		}
		// An UNREGISTERED study is refused rather than warned about.
		//
		// A typo in the study id used to fall through: LookupStudy misses, the arm order falls back to
		// M5-b's, every ladder arm drops out of the table, and the report exits 0 saying only that it
		// evaluated no criteria. Evidence whose experiment this binary does not know is evidence it cannot
		// place, and "no criteria evaluated" is what it says about a study whose readings are not written --
		// a different thing, and one a reader has no way to tell apart.
		//
		// Rows written before studies existed carry an empty id, which CanonicalStudyID maps to M5-b, so
		// this refusal only bites an id that was actually wrong.
		st, known := bench.LookupStudy(study)
		if !known {
			return nil, fmt.Errorf("%s records study %s, which is not registered; known studies are %s."+
				" A report cannot place evidence from an experiment it does not know, and reporting it under"+
				" another study's arm order would be a comparison nobody ran",
				path, studyLabel(study, recorded), strings.Join(bench.KnownStudyIDs(), ", "))
		}
		// And the arm must be one the study admits. An unknown arm is not a row this report can place: it
		// falls out of the study's order, out of contendedArms, and out of every reading that names arms --
		// silently, because nothing downstream asks whether it belonged.
		if !st.Admits(arm) {
			return nil, fmt.Errorf("%s records arm %q, which study %s does not admit (%s);"+
				" a report cannot place a row whose arm its own registry does not name",
				path, arm, st.ID, strings.Join(st.Arms, ", "))
		}

		e.byArm[arm] = append(e.byArm[arm], rows...)
		// Keep the whole per-repetition summary, not just its p99.
		//
		// This line used to discard everything except TTFTMsP99, which is how a truncated repetition became
		// invisible: the pooled arm summary sums every row, so three healthy repetitions carry a fourth whose
		// p99 is a maximum over thirty requests -- and the bootstrap then resamples that fourth value with
		// equal weight.
		// The same replay may not be counted twice.
		h := fnv.New64a()
		var buf [8]byte
		for _, r := range rows {
			binary.LittleEndian.PutUint64(buf[:], uint64(r.SendUnixNanos))
			_, _ = h.Write(buf[:])
		}
		id := fmt.Sprintf("%s/%x", arm, h.Sum64())
		if prev, dup := e.replayFrom[id]; dup {
			return nil, fmt.Errorf("%s and %s carry the SAME replay of arm %s -- every row was sent at the"+
				" same nanosecond, so one is a copy of the other. Counting it twice would report two"+
				" repetitions whose spread is exactly zero, and that spread is what readings 3 and 5"+
				" measure an improvement against", path, prev, arm)
		}
		e.replayFrom[id] = path

		rs := bench.Summarize(arm, rows)
		e.repP99[arm] = append(e.repP99[arm], rs.TTFTMsP99)
		e.repTail[arm] = append(e.repTail[arm], rs.TailSampleSize)
		e.repRows[arm] = append(e.repRows[arm], len(rows))
		e.repSeconds[arm] = append(e.repSeconds[arm], rs.ActiveSeconds)
		done := map[string]int{}
		served := map[string]float64{}
		for tenant, d := range rs.DispositionByTenant {
			done[tenant] = d.Completed
			if d.Offered > 0 {
				served[tenant] = float64(d.Completed) / float64(d.Offered)
			}
		}
		e.repDone[arm] = append(e.repDone[arm], done)
		e.repServed[arm] = append(e.repServed[arm], served)
		if rs.Censored {
			e.repCensored[arm] = true
		}
		// Every repetition's checksum, not the last one's.
		//
		// This assigned, so loadArmEvidence kept only whichever file it read last and a repetition replayed
		// from a different trace was invisible to refuseIfTracesDisagree -- the one check whose entire job is
		// to prove the arms saw identical traffic. The trigger is a workflow this runner endorses: re-running
		// one botched arm into the same output directory.
		if prev, ok := e.checksum[arm]; ok && prev != rows[0].TraceChecksum {
			return nil, fmt.Errorf("arm %s has repetitions replayed from different traces (%s and %s); its rows are pooled into one summary, so mixing them compares an arm against itself across two workloads", arm, prev, rows[0].TraceChecksum)
		}
		e.checksum[arm] = rows[0].TraceChecksum
		if prev, ok := e.tolerance[arm]; ok && prev != rows[0].MatchTolerance {
			return nil, fmt.Errorf("arm %s has repetitions carrying different admission-match tolerances (%v and %v); the pre-registered tolerance cannot be two values", arm, prev, rows[0].MatchTolerance)
		}
		e.tolerance[arm] = rows[0].MatchTolerance
		// The treatment is part of an arm's identity, exactly like its trace and its tolerance.
		//
		// Without this, replaying one manifest twice -- once with --priorities and once without -- produced
		// two files a report happily pooled as repetitions of the same condition. The bootstrap would then
		// resample across a treated and an untreated run and report the interval as if one thing had been
		// measured four times.
		treat := treatmentOf(rows)
		if prev, ok := e.treatment[arm]; ok && prev != treat {
			return nil, fmt.Errorf("arm %s has repetitions replayed under different priority treatments (%s and %s);"+
				" they are two conditions, and pooling them reports an interval over a comparison rather than a repetition",
				arm, prev, treat)
		}
		e.treatment[arm] = treat
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
	// The identity holds WITHIN A COMPARISON GROUP, which is the whole study for every experiment that
	// offers one load and one rung for the capacity ladder, which offers four.
	//
	// It was written as one group across all arms, and that is correct for every study that existed when it
	// was written. The ladder broke it in the direction that matters: its rungs replay DIFFERENT traces on
	// purpose -- that is what a rung is -- so the report refused the ladder's own evidence after the cells
	// were bought. The first ladder rehearsal caught it on a free cluster; on a card it would have refused
	// at the end of the session, with every cell paid for.
	wantSum := map[string]string{}
	wantSumFrom := map[string]string{}
	for _, arm := range e.contendedArms() {
		sum, ok := e.checksum[arm]
		if !ok {
			continue
		}
		if sum == "" {
			return fmt.Errorf("arm %s carries no trace checksum; regenerate its manifest with a current gen-trace", arm)
		}
		g := comparisonGroup(arm)
		if _, seen := wantSum[g]; !seen {
			wantSum[g], wantSumFrom[g] = sum, arm
			continue
		}
		if sum != wantSum[g] {
			return fmt.Errorf("arm %s replayed a different trace (%s) than %s (%s);"+
				" arms compared against each other need one immutable trace", arm, sum, wantSumFrom[g], wantSum[g])
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
	contended := e.contendedArms()
	wantRows := map[string]int{}
	wantFrom := map[string]string{}
	for _, arm := range contended {
		g := comparisonGroup(arm)
		for i, n := range e.repRows[arm] {
			if wantRows[g] == 0 {
				wantRows[g], wantFrom[g] = n, fmt.Sprintf("%s[%d]", arm, i)
				continue
			}
			if n != wantRows[g] {
				return fmt.Errorf("arm %s repetition %d recorded %d rows but %s recorded %d;"+
					" these arms share one immutable trace, so a shorter recording means a run that did not finish,"+
					" and the requests it is missing are the late ones contention makes slow",
					arm, i, n, wantFrom[g], wantRows[g])
			}
		}
	}

	// A baseline replays the same trace with the contender filtered out, so its count legitimately differs
	// from the contended arms -- but not from itself.
	//
	// Every baseline, not the literal "R1": the ladder's is called rung02-R1, and a loop over one hardcoded
	// name would have checked nothing at all for it while looking exactly as though it had.
	for arm, rows := range e.repRows {
		if !bench.IsIsolatedBaseline(arm) {
			continue
		}
		for i, n := range rows {
			if n != rows[0] {
				return fmt.Errorf("arm %s repetition %d recorded %d rows but repetition 0 recorded %d;"+
					" its repetitions replay one trace and must record the same number of rows",
					arm, i, n, rows[0])
			}
		}
	}
	return nil
}

// comparisonGroup names the set of arms an arm is compared against, which is what "one immutable trace"
// has to hold within.
//
// Every study but one offers a single load, so every arm is in one group and the group name is empty. The
// capacity ladder offers a different load per rung by design, so its group is the rung: rung01-shared and
// rung01-timeSlicing must replay the same trace as each other and MUST NOT replay the same trace as
// rung02's cells -- a ladder whose rungs agreed would be four measurements of one load.
func comparisonGroup(arm string) string { return bench.ArmComparisonGroup(arm) }

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

// contendedArms are the study's arms that replay the full trace, so their traffic must be identical.
//
// R1 is excluded because it replays the same trace with the contending tenant filtered out, which is what
// makes it a ceiling rather than a condition -- its record count legitimately differs.
//
// Derived from the study rather than listed, because the list used to be three M5-b names and every arm of
// any other experiment therefore skipped the identity check entirely: refuseIfTracesDisagree looked at
// nothing at all for the price-of-protection sweep.
func (e *armEvidence) contendedArms() []string {
	study, ok := bench.LookupStudy(e.study)
	if !ok {
		study, _ = bench.LookupStudy(bench.StudyM5BGateway)
	}
	arms := make([]string, 0, len(study.Arms))
	for _, a := range study.Arms {
		if !bench.IsIsolatedBaseline(a) {
			arms = append(arms, a)
		}
	}
	return arms
}

// summarize builds the per-arm summaries in report order and attaches the repetition shape.
func (e *armEvidence) summarize() ([]bench.ArmSummary, map[string]bench.ArmSummary) {
	// The order is the study's, not four literals.
	//
	// Hard-coded, this dropped nine of the price-of-protection sweep's ten arms on the floor: summarize
	// kept only R1, because that is the one name the two studies share, and the report then complained
	// about arms belonging to an experiment it was not reporting on.
	study, ok := bench.LookupStudy(e.study)
	if !ok {
		study, _ = bench.LookupStudy(bench.StudyM5BGateway)
	}
	order := study.Arms
	var summaries []bench.ArmSummary
	summ := map[string]bench.ArmSummary{}
	for _, arm := range order {
		if rows, ok := e.byArm[arm]; ok {
			s := bench.Summarize(arm, rows)
			// Summarize sees pooled rows and cannot know how they were split, so the repetition shape is
			// attached here where the split is known.
			s.AnyRepetitionCensored = e.repCensored[arm]
			// The worst fraction any repetition served, per tenant. A tenant absent from a repetition was
			// offered nothing there, so that repetition says nothing about its fraction and is skipped --
			// unlike the count, where absence means zero served.
			if reps := e.repServed[arm]; len(reps) > 0 {
				s.WorstRepetitionServedFractionByTenant = map[string]float64{}
				for _, served := range reps {
					for tenant, f := range served {
						if prev, seen := s.WorstRepetitionServedFractionByTenant[tenant]; !seen || f < prev {
							s.WorstRepetitionServedFractionByTenant[tenant] = f
						}
					}
				}
			}
			if tails := e.repTail[arm]; len(tails) > 0 {
				s.RepetitionCount = len(tails)
				s.MinRepetitionTail = slices.Min(tails)
			}
			// The thinnest repetition per tenant, so a floor can be applied where the pool hides it.
			//
			// A tenant MISSING from a repetition counts as zero for that repetition, which is the whole
			// point: a block that served the contender nothing carries no disposition entry for it, and
			// taking the minimum over only the repetitions that mention the tenant would skip exactly the
			// block the floor is looking for.
			if reps := e.repDone[arm]; len(reps) > 0 {
				tenants := map[string]bool{}
				for _, done := range reps {
					for tenant := range done {
						tenants[tenant] = true
					}
				}
				s.MinRepetitionCompletedByTenant = map[string]int{}
				for tenant := range tenants {
					minSeen := -1
					for _, done := range reps {
						n := done[tenant] // zero when this repetition served the tenant nothing
						if minSeen < 0 || n < minSeen {
							minSeen = n
						}
					}
					s.MinRepetitionCompletedByTenant[tenant] = minSeen
				}
			}
			// The per-repetition tails travel with the summary too, because the price-of-protection run's
			// reading 3 needs the CONTROL'S spread as its threshold and a pooled p99 cannot supply it.
			s.RepetitionTTFTMsP99 = append([]float64(nil), e.repP99[arm]...)
			// The pooled span Summarize just computed spans the washouts between repetitions, so replace it
			// with the sum of the repetitions' own spans, which is the time the arm was actually sending.
			if spans := e.repSeconds[arm]; len(spans) > 0 {
				total := 0.0
				for _, v := range spans {
					total += v
				}
				s.SetActiveSeconds(total)
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
		// A bootstrap interval over four values only bounds what it claims to while those values are tight.
		// Marking it invalid rather than reporting it keeps a scattered run from clearing the gate on an
		// interval that is narrower than the evidence supports; see bench.MaxRatioScatter.
		if bench.RatioScatterTooHigh(ratios) {
			incCI.Valid = false
			incCI.InvalidReason = fmt.Sprintf("the per-repetition C/B ratios scatter beyond a coefficient of variation of %.2f, past which a percentile bootstrap over %d values fires on no effect at all more often than its nominal 5 percent", bench.MaxRatioScatter, len(ratios))
		}
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

// evaluatePoP scores the price-of-protection readings, or returns nil when the arms they need are absent.
//
// R1 and the control are not optional: every reading is a ratio against one or the other, and a report that
// quietly scored the cells against a missing baseline would produce ratios against zero. Saying so on stderr
// and returning nil puts the run in the "criteria not evaluated" state, which the report already renders
// honestly, rather than inventing a verdict.
func evaluatePoP(summ map[string]bench.ArmSummary, summaries []bench.ArmSummary) *bench.PoPResult {
	r1, haveR1 := summ[bench.ArmR1]
	control, haveControl := summ[bench.ArmDefaultFCFS]
	if !haveR1 || !haveControl {
		fmt.Fprintf(os.Stderr,
			"warning: the price-of-protection readings need both %s and %s and this evidence has %s; no criteria were evaluated\n",
			bench.ArmR1, bench.ArmDefaultFCFS, strings.Join(armNames(summaries), ", "))
		return nil
	}
	var cells []bench.ArmSummary
	for _, s := range summaries {
		if s.Arm != bench.ArmR1 && s.Arm != bench.ArmDefaultFCFS {
			cells = append(cells, s)
		}
	}
	res := bench.EvaluatePriceOfProtection(r1, control, cells, bench.PremiumTenant, bench.NoisyTenant)
	return &res
}

// refusalsBeside reads the arm refusals the runner wrote next to its raw evidence.
//
// Reading 4c is "the sharing mode did not engage", and it distinguishes a RECORDED refusal from an arm that
// is merely absent -- because absence is equally consistent with an interruption or an operator running a
// subset. Until this existed nothing populated that map outside the unit tests, so the one registered
// outcome meant to identify an MPS engagement failure could never fire on evidence the runner produced: the
// reason lived in evidence.log and `report` reads only raw files.
//
// Discovered rather than passed, because a flag an operator must remember is a flag that is forgotten on the
// run that needed it. hack/m5c-matrix.sh writes refused-<arm>.txt into the same directory as the raw files.
func refusalsBeside(rawFiles []string) map[string]string {
	if len(rawFiles) == 0 {
		return nil
	}
	out := map[string]string{}
	seen := map[string]bool{}
	for _, f := range rawFiles {
		dir := filepath.Dir(f)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		matches, err := filepath.Glob(filepath.Join(dir, "refused-*.txt"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			arm := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "refused-"), ".txt")
			b, rerr := os.ReadFile(m)
			if rerr != nil {
				continue
			}
			if why := strings.TrimSpace(string(b)); why != "" {
				out[arm] = why
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// evaluateRegisteredReadings runs whichever study's readings the evidence belongs to, and none of anybody
// else's.
//
// It is one function rather than four branches inside report for a reason worth keeping: a fifth study adds
// a case here and nothing else, and report's job stays "load, evaluate, format, decide the exit code". The
// three checks the M5-b branch names are M5-b's pre-registered comparison. Running them for another
// experiment asked for static-cap and kv-aware, found neither, and reported the run disqualified -- a
// verdict about arms the experiment never had. A study whose readings are not implemented gets its tables
// and a line on stderr saying plainly that no criteria were evaluated, rather than a failure against
// somebody else's.
func evaluateRegisteredReadings(e *armEvidence, summ map[string]bench.ArmSummary, summaries []bench.ArmSummary,
	rawFiles []string, incCI bench.CI, matchTolerance float64,
) (*bench.Checks, *bench.PoPResult, *bench.SharingResult, *bench.LadderResult) {
	switch bench.CanonicalStudyID(e.study) {
	case bench.StudyM5BGateway:
		// Name the arms that are absent, because the refusal downstream cannot: summ is a map, so a missing
		// arm yields the zero ArmSummary and EvaluateChecks disqualifies on its zero TailSampleSize. The
		// refusal WORKS; what it cannot do is say which arm, and that is a bad minute to spend at the end of
		// a paid session.
		var missing []string
		for _, arm := range []string{bench.ArmR1, "static-cap", "kv-aware"} {
			if _, ok := summ[arm]; !ok {
				missing = append(missing, arm)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "warning: no records for arm(s) %s; the comparison will be disqualified\n",
				strings.Join(missing, ", "))
		}
		evaluated := bench.EvaluateChecks(summ[bench.ArmR1], summ["static-cap"], summ["kv-aware"], incCI, matchTolerance)
		return &evaluated, nil, nil, nil
	case bench.StudyPriceOfProtection:
		return nil, evaluatePoP(summ, summaries), nil, nil
	case bench.StudySharingMatrix:
		return nil, nil, evaluateSharingMatrix(summ, summaries, refusalsBeside(rawFiles)), nil
	case bench.StudyThroughputLadder, bench.StudyThroughputLadderDown:
		// The ladder takes the summaries rather than the arm map, because its cells are identified by rung
		// and topology parsed out of the arm name and it has to see every one of them -- including arms this
		// study does not name, which it ignores.
		evaluated := bench.EvaluateThroughputLadder(summaries)
		evaluated.Study = bench.CanonicalStudyID(e.study)
		return nil, nil, nil, &evaluated
	default:
		fmt.Fprintf(os.Stderr,
			"warning: study %s has no implemented readings, so this report shows its measurements and evaluates no criteria\n",
			e.study)
		return nil, nil, nil, nil
	}
}

// evaluateSharingMatrix sorts the M5-c evidence into the roles its readings speak about.
//
// R1 and `shared` are required, and their absence is a warning with no readings rather than readings
// computed against a zero ArmSummary. R1 is the denominator of both bars and `shared` is the control every
// improvement is measured from; a zero value for either would make every ratio meaningless in a way that
// still prints a number, which is the failure this file's other evaluator was fixed for.
//
// The sharing arms are taken as whatever else is present, rather than looked up by name. An operator running
// ARMS="shared timeSlicing" gets a matrix with one sharing arm and readings that say so, instead of a
// lookup miss reported as a mode that did not engage.
func evaluateSharingMatrix(summ map[string]bench.ArmSummary, summaries []bench.ArmSummary, refused map[string]string) *bench.SharingResult {
	// A missing baseline or control is REPORTED as an uncomputable gate, not returned as nil.
	//
	// Returning nil printed a warning to stderr and left `sharing` unset, so the verdict block never ran and
	// `report` exited zero -- and the paid runner calls it as `benchharness report ... || fail`. The
	// evaluator carries the same refusal for its own callers, and that one is unreachable from here because
	// this function stops first: the fix belonged in both places and was put in only one.
	r1, haveR1 := summ[bench.ArmR1]
	shared, haveShared := summ[bench.ArmShared]
	if !haveR1 || !haveShared {
		want := bench.ArmR1
		why := "the isolated baseline both bars divide by"
		if haveR1 {
			want, why = bench.ArmShared, "the control every improvement is measured from"
		}
		return &bench.SharingResult{Readings: []bench.PoPReading{{
			ID: "4", Name: "the load did not create contention -- INVALID", NotEvaluable: true,
			Detail: fmt.Sprintf("this evidence carries %s and no %s arm, which is %s; nothing below can be scored without it",
				strings.Join(armNames(summaries), ", "), want, why),
		}}}
	}
	arms := bench.SharingArms{R1: r1, Shared: shared, Refused: refused}
	for _, s := range summaries {
		if s.Arm != bench.ArmR1 && s.Arm != bench.ArmShared {
			arms.Sharing = append(arms.Sharing, s)
		}
	}
	res := bench.EvaluateSharingMatrix(arms, bench.PremiumTenant, bench.NoisyTenant)
	return &res
}

func armNames(summaries []bench.ArmSummary) []string {
	names := make([]string, 0, len(summaries))
	for _, s := range summaries {
		names = append(names, s.Arm)
	}
	return names
}

// sharingRunInvalid reports the error that must end the process, or nil when the run stands.
//
// Reading 4c is deliberately NOT in this list, and matching on the word INVALID used to put it there.
//
// 4 and 4b invalidate the RUN: they say the trace, not the topology, is what has to change, and nothing
// measured under them means anything. 4c invalidates ONE ARM -- its name ends "INVALID for that arm" --
// and the arm beside it was still measured and still paid for. Name matching could not tell those apart,
// so a refused MPS arm made `benchharness report ... || fail` reject a run whose time-slicing arm had
// produced a result. That is the expected case for this study rather than a corner: MPS has already been
// measured failing to engage on this AMI. The IDs are listed explicitly so an exit status turns on which
// reading fired rather than on how it was worded.
//
// A named function rather than a condition inside report(), because a rule that was wrong once should be
// reachable from a test, and inline in a command that wants files on disk it is not.
func sharingRunInvalid(res bench.SharingResult) error {
	for _, r := range res.Readings {
		if r.Fired && (r.ID == "4" || r.ID == "4b") {
			return fmt.Errorf("run invalid: reading %s fired -- %s", r.ID, r.Detail)
		}
		// A GATE that could not be computed is also not a run that stands.
		//
		// 4 and 4b are prerequisites: they ask whether the evidence can be assessed at all. When one of them
		// comes back NotEvaluable the answer is "we could not tell", and this returned nil -- so a control
		// whose premium tail is censored printed a report with no verdict, no answer and exit status zero,
		// and `benchharness report ... || fail` accepted it as a successful run. Reproduced by an
		// independent review with 2% premium timeouts in the control.
		//
		// Only the GATES. A reading below them coming back NotEvaluable is an ordinary "no finding here".
		if r.NotEvaluable && (r.ID == "4" || r.ID == "4b") {
			return fmt.Errorf("run invalid: reading %s could not be evaluated -- %s", r.ID, r.Detail)
		}
	}
	return nil
}
