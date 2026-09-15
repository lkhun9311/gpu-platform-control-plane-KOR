package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// runGenTrace runs gen-trace into a temporary directory and returns the rows it wrote.
func runGenTrace(t *testing.T, args ...string) ([]bench.TraceRow, error) {
	t.Helper()
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")
	args = append(args, "--trace-out", tracePath, "--manifest-out", filepath.Join(dir, "manifest.yaml"))
	if err := genTrace(args); err != nil {
		return nil, err
	}
	f, err := os.Open(tracePath)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := bench.ReadTrace(f)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	return rows, nil
}

// scheduleOf returns one tenant's rows with Index cleared, since Index moves when another tenant gains arrivals.
func scheduleOf(rows []bench.TraceRow, tenant string) []bench.TraceRow {
	var out []bench.TraceRow
	for _, r := range rows {
		if r.Tenant == tenant {
			r.Index = 0
			out = append(out, r)
		}
	}
	return out
}

// The property the flags exist for, checked through the command a ladder actually runs rather than the library.
func TestGenTraceHoldsTheContenderFixedWhileThePremiumRateClimbs(t *testing.T) {
	rung := func(premiumRate string) []bench.TraceRow {
		rows, err := runGenTrace(t, "--seed", "11", "--duration-ms", "60000",
			"--premium-rate", premiumRate, "--noisy-rate", "0.28", "--probe-rate", "0")
		if err != nil {
			t.Fatalf("gen-trace at premium rate %s: %v", premiumRate, err)
		}
		return rows
	}
	low, high := rung("1.16"), rung("4.61")

	lowNoisy, highNoisy := scheduleOf(low, bench.NoisyTenant), scheduleOf(high, bench.NoisyTenant)
	if len(lowNoisy) == 0 {
		t.Fatal("no contender rows, so the comparison below is vacuous")
	}
	if len(lowNoisy) != len(highNoisy) {
		t.Fatalf("contender offers moved from %d to %d when only the premium rate changed", len(lowNoisy), len(highNoisy))
	}
	for i := range lowNoisy {
		if lowNoisy[i] != highNoisy[i] {
			t.Fatalf("contender row %d moved when only the premium rate changed: %+v then %+v", i, lowNoisy[i], highNoisy[i])
		}
	}
	if got, was := len(scheduleOf(high, "premium-1")), len(scheduleOf(low, "premium-1")); got <= 2*was {
		t.Fatalf("premium offers went from %d to only %d, so the premium side did not actually climb", was, got)
	}
}

// Every trace a paid manifest pins was generated through the weighted flags, so their output must not move.
//
// The expected tenants are spelled out here rather than taken from traceTenants, so a swapped weight or a
// reordered tenant list in the refactor fails instead of agreeing with itself.
func TestGenTraceWeightedFlagsStillProduceTheSameTrace(t *testing.T) {
	got, err := runGenTrace(t, "--seed", "11", "--duration-ms", "60000", "--rate", "3",
		"--premium-weight", "1", "--noisy-weight", "0.4", "--probe-weight", "0.05")
	if err != nil {
		t.Fatalf("gen-trace: %v", err)
	}
	want, err := bench.GenerateTrace(bench.TraceParams{
		Seed: 11, DurationMs: 60_000, RatePerSec: 3,
		Tenants: []bench.TenantSpec{
			{Tenant: "premium-1", Weight: 1, PromptLenChars: 200, MaxOutputTokens: 64},
			{Tenant: bench.NoisyTenant, Weight: 0.4, PromptLenChars: 40_000, MaxOutputTokens: 16, IsNoisy: true},
			{Tenant: bench.ProbeUnderTenant, Weight: 0.05, PromptLenChars: bench.ProbeUnderChars, MaxOutputTokens: 8, IsNoisy: true},
			{Tenant: bench.ProbeOverTenant, Weight: 0.05, PromptLenChars: bench.ProbeOverChars, MaxOutputTokens: 8, IsNoisy: true},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("gen-trace wrote %d rows, the weighted generator gives %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d differs: gen-trace %+v, generator %+v", i, got[i], want[i])
		}
	}
}

func TestGenTraceRefusesAnUnstatedArrivalProcess(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a weight alongside the rates",
			[]string{"--premium-rate", "1", "--noisy-rate", "0.3", "--probe-rate", "0", "--noisy-weight", "0.4"},
			"--noisy-weight was passed alongside per-tenant rates"},
		{"a total rate alongside the rates",
			[]string{"--premium-rate", "1", "--noisy-rate", "0.3", "--probe-rate", "0", "--rate", "2"},
			"--rate was passed alongside per-tenant rates"},
		// The probe rate is the one a caller forgets, and forgetting it must not silently drop the probes.
		{"the probe rate left out",
			[]string{"--premium-rate", "1", "--noisy-rate", "0.3"},
			"--probe-rate is required"},
		{"a negative probe rate",
			[]string{"--premium-rate", "1", "--noisy-rate", "0.3", "--probe-rate", "-1"},
			"--probe-rate must be zero or positive"},
		{"a contender with no rate",
			[]string{"--premium-rate", "1", "--noisy-rate", "0", "--probe-rate", "0"},
			"every tenant needs its own rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runGenTrace(t, append([]string{"--duration-ms", "60000"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// A trace file does not say which model produced it, so the study has to be the thing that refuses.
func TestGenTraceRefusesTheOtherArrivalModelForARegisteredStudy(t *testing.T) {
	weighted := []string{"--rate", "1.431979", "--premium-weight", "1", "--noisy-weight", "0.23386882", "--probe-weight", "0"}
	rated := []string{"--premium-rate", "1.16", "--noisy-rate", "0.289", "--probe-rate", "0"}
	common := []string{"--seed", "11", "--duration-ms", "505000", "--arm", "rung01-shared"}

	withStudy := func(study string, load []string) []string {
		return append(append(append([]string{}, common...), "--study", study), load...)
	}

	if _, err := runGenTrace(t, withStudy(bench.StudyThroughputLadderIndependent, weighted)...); err == nil ||
		!strings.Contains(err.Error(), "registered independent arrivals and these flags describe weighted") {
		t.Fatalf("weighted flags for the independent ladder: got %v", err)
	}
	if _, err := runGenTrace(t, withStudy(bench.StudyThroughputLadderDown, rated)...); err == nil ||
		!strings.Contains(err.Error(), "registered weighted arrivals and these flags describe independent") {
		t.Fatalf("per-tenant rates for the weighted down ladder: got %v", err)
	}

	rows, err := runGenTrace(t, withStudy(bench.StudyThroughputLadderIndependent, rated)...)
	if err != nil {
		t.Fatalf("per-tenant rates for the independent ladder were refused: %v", err)
	}
	// The rung the pre-registration draft cites, so a change to the generator that moves it fails here first.
	if got := len(scheduleOf(rows, bench.NoisyTenant)); got != 139 {
		t.Fatalf("contender offers at 0.289/s over 505 s with seed 11: got %d, want 139", got)
	}
}

func TestArrivalsOfReadsTheRegistry(t *testing.T) {
	for study, want := range map[string]bench.ArrivalModel{
		bench.StudyThroughputLadder:            bench.ArrivalsWeighted,
		bench.StudyThroughputLadderDown:        bench.ArrivalsWeighted,
		bench.StudyThroughputLadderIndependent: bench.ArrivalsIndependent,
	} {
		if got, err := arrivalsOf(study); err != nil || got != want {
			t.Errorf("arrivalsOf(%s) = %q, %v; want %q", study, got, err, want)
		}
	}
	// A study that registered no model must not be answered with a guess, or a script would pick flags for it.
	if _, err := arrivalsOf(bench.StudySharingMatrix); err == nil || !strings.Contains(err.Error(), "registered no arrival model") {
		t.Errorf("arrivalsOf(sharing matrix) = %v, want a refusal", err)
	}
	if _, err := arrivalsOf("no-such-study"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("arrivalsOf(unknown) = %v, want a refusal", err)
	}
}
