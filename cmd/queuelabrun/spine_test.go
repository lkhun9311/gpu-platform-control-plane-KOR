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
	"strings"
	"testing"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

func TestParseArmAcceptsOnlyTheThreeArms(t *testing.T) {
	for _, want := range []queuelab.Arm{queuelab.ArmAHonor, queuelab.ArmAIgnore, queuelab.ArmNRef} {
		got, err := parseArm(string(want))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if got != want {
			t.Fatalf("parseArm(%q) = %q", want, got)
		}
	}
	// The old CLI accepted any study/variant pair, which is how an arm the experiment never defined could
	// still be run; anything outside the closed set must be refused rather than defaulted.
	for _, bad := range []string{"", "Any", "reclaim", "fifo", "a-honor", "A-Honor"} {
		if _, err := parseArm(bad); err == nil {
			t.Fatalf("parseArm(%q) must be refused", bad)
		}
	}
}

func TestNamespaceForIsDerivedAndValidated(t *testing.T) {
	ns, err := namespaceFor("p1a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ns, "p1a") {
		t.Fatalf("namespace %q must carry the run id", ns)
	}
	// Two runs must never share a namespace, which is what let a previous run's objects satisfy this run's
	// barriers, so the namespace is derived rather than accepted from a flag.
	other, err := namespaceFor("p1b")
	if err != nil {
		t.Fatal(err)
	}
	if ns == other {
		t.Fatal("different run ids must yield different namespaces")
	}
	for _, bad := range []string{"", "P1A", "p1_a", "a/b", strings.Repeat("x", 200)} {
		if _, err := namespaceFor(bad); err == nil {
			t.Fatalf("namespaceFor(%q) must be refused", bad)
		}
	}
}

func TestProtocolConstantsMatchTheDesignOfRecord(t *testing.T) {
	if victimServiceSec != 60 {
		t.Fatalf("victim service = %d, want 60", victimServiceSec)
	}
	// 40 s, not the 49 s the old offset subtraction produced.
	if doseSec != 40 {
		t.Fatalf("dose = %d, want 40", doseSec)
	}
}

// The four tests that stood here — TestGateRefusalBlocksCountableResults and the three that pinned
// unimplementedGates()' entries — went with the functions they tested, and what they were protecting did not
// go with them.
//
// They existed to stop a list of missing work from decaying: from going empty, from naming something already
// built, from being quietly widened back after somebody narrowed it. That list is now recordUnchecked(workloadProvenance{}), it
// is persisted into every record rather than printed once, and the successor to all four is
// TestTheRecordsUncheckedListNamesTheResidualAndNothingWider in record_test.go, which applies the same three
// pressures to it. Deleting these without a successor would have left the surviving list guarded only by
// tests that check it names the canary, which an entry narrowed to a single vague word would satisfy.
//
// Nothing replaces the refusal itself, because there is nothing left to refuse: what a run may claim is
// decided after the fact by deriveValidity, from fields the run wrote.

func TestRequireRunIDRejectsOnlyEmpty(t *testing.T) {
	// The flag used to default to "r1", which made colliding with a previous run's cluster-scoped fixtures
	// the default behaviour; there is no safe default, so only a genuinely supplied id may pass.
	if err := requireRunID(""); err == nil {
		t.Fatal("an empty run id must be refused")
	}
	if err := requireRunID("r1"); err != nil {
		t.Fatalf("a non-empty run id must be accepted: %v", err)
	}
}

func TestCheckFlavorVariantCatchesAReusedRunID(t *testing.T) {
	// A reused run id leaves the old arm's ResourceFlavor in place; its variant label must match the new
	// arm's PolicyVariant() or the run would silently execute under the old mechanism.
	if err := checkFlavorVariant(map[string]string{variantLabelKey: "Never"}, "Any"); err == nil {
		t.Fatal("a mismatched variant must be refused")
	}
	if err := checkFlavorVariant(map[string]string{variantLabelKey: "Any"}, "Any"); err != nil {
		t.Fatalf("a matching variant must be allowed: %v", err)
	}
	if err := checkFlavorVariant(map[string]string{}, "Any"); err == nil {
		t.Fatal("a flavor missing the variant label entirely must be refused, not treated as a match")
	}
}

func TestHorizonSecCoversTheProtocolsFixedWindow(t *testing.T) {
	// 40 s dose + 60 s victim service + 30 s termination grace + 20 s startup margin = 150 s, the same
	// duration a1 now runs for; a shorter horizon would end the observation before the owner is ever Ready.
	if horizonSec != 150 {
		t.Fatalf("horizonSec = %d, want 150", horizonSec)
	}
}

// decideOperatorMode is the pure layer dispatchOperatorMode hoists all validation into, precisely so a
// malformed invocation is refused before it needs a kubeconfig. This table is the regression test for the
// swapped-flag failure mode a review of the previous positional-parameter version found: every refusal this
// layer is responsible for — combining a mode with -arm, more than one mode at once, and both halves of
// each two-flag requirement (-release-stale needs -txid AND -confirm-owner-dead; -force-release needs
// -node-uid AND -accept-divergence; -clear-quarantine needs -quarantine-id AND -confirm-owner-dead) — is
// exercised here without touching a cluster.
func TestDecideOperatorMode(t *testing.T) {
	cases := []struct {
		name       string
		args       operatorModeArgs
		wantMode   operatorMode
		wantErr    bool
		wantErrHas string
	}{
		{
			name:     "nothing requested falls through to the ordinary run",
			args:     operatorModeArgs{},
			wantMode: modeNone,
			wantErr:  false,
		},
		{
			name:       "two modes at once refuses",
			args:       operatorModeArgs{Inspect: true, ReleaseStale: true, TxID: "tx-1"},
			wantErr:    true,
			wantErrHas: "only one of",
		},
		{
			name:       "a mode combined with -arm refuses",
			args:       operatorModeArgs{Arm: "A-honor", Inspect: true},
			wantErr:    true,
			wantErrHas: "-arm",
		},
		{
			name:     "inspect-worker alone dispatches",
			args:     operatorModeArgs{Inspect: true},
			wantMode: modeInspect,
		},
		{
			// -arm was refused and every other run-only flag was silently ignored, so an invocation that
			// named a run id, an output path or a horizon looked configured to its author while doing none of
			// it. The refusal has to name the flags, or the operator cannot tell which part was the mistake.
			name:       "a mode combined with run-only flags refuses",
			args:       operatorModeArgs{Inspect: true, RunOnlyFlags: []string{"-out", "-runid"}},
			wantErr:    true,
			wantErrHas: "-out, -runid",
		},
		{
			// -horizon is the one that cannot be caught by looking at values: it has a default, so a supplied
			// -horizon equal to that default is indistinguishable from an absent one unless what the operator
			// TYPED is what gets carried here.
			name:       "a mode combined with -horizon refuses even at its default value",
			args:       operatorModeArgs{ForceRelease: true, NodeUID: "uid-1", AcceptDivergence: true, RunOnlyFlags: []string{"-horizon"}},
			wantErr:    true,
			wantErrHas: "-horizon",
		},
		{
			name:       "release-stale without -txid refuses",
			args:       operatorModeArgs{ReleaseStale: true, ConfirmOwnerDead: true},
			wantErr:    true,
			wantErrHas: "-txid",
		},
		{
			// A -txid match identifies the transaction, not the liveness of the process holding it, so the
			// mode that most often costs a live run its worker is attested exactly like the other two
			// destructive ones.
			name:       "release-stale with -txid but without -confirm-owner-dead refuses",
			args:       operatorModeArgs{ReleaseStale: true, TxID: "tx-1"},
			wantErr:    true,
			wantErrHas: "-confirm-owner-dead",
		},
		{
			name:     "release-stale with both -txid and -confirm-owner-dead dispatches",
			args:     operatorModeArgs{ReleaseStale: true, TxID: "tx-1", ConfirmOwnerDead: true},
			wantMode: modeReleaseStale,
		},
		{
			name:       "force-release without -node-uid refuses",
			args:       operatorModeArgs{ForceRelease: true, AcceptDivergence: true},
			wantErr:    true,
			wantErrHas: "-node-uid",
		},
		{
			name:       "force-release with -node-uid but without -accept-divergence refuses",
			args:       operatorModeArgs{ForceRelease: true, NodeUID: "uid-1"},
			wantErr:    true,
			wantErrHas: "-accept-divergence",
		},
		{
			name:     "force-release with both -node-uid and -accept-divergence dispatches",
			args:     operatorModeArgs{ForceRelease: true, NodeUID: "uid-1", AcceptDivergence: true},
			wantMode: modeForceRelease,
		},
		{
			name:       "clear-quarantine without -quarantine-id refuses",
			args:       operatorModeArgs{ClearQuarantine: true, ConfirmOwnerDead: true},
			wantErr:    true,
			wantErrHas: "-quarantine-id",
		},
		{
			name:       "clear-quarantine with -quarantine-id but without -confirm-owner-dead refuses",
			args:       operatorModeArgs{ClearQuarantine: true, QuarantineID: "q1"},
			wantErr:    true,
			wantErrHas: "-confirm-owner-dead",
		},
		{
			name:     "clear-quarantine with both -quarantine-id and -confirm-owner-dead dispatches",
			args:     operatorModeArgs{ClearQuarantine: true, QuarantineID: "q1", ConfirmOwnerDead: true},
			wantMode: modeClearQuarantine,
		},
		{
			// The canary needs nothing but the node. It is deliberately NOT behind an attestation like the three
			// destructive modes: those require one because nothing this tool can observe tells the operator
			// whether the previous process is dead, and this mode asks nobody to judge anything — it takes the
			// worker through the ordinary transaction, which refuses a node somebody else holds.
			//
			// Mutation that turns this row red: leave TerminationCanary out of decideOperatorMode's switch. It
			// falls through to the ClearQuarantine default and refuses for a missing -quarantine-id, which is a
			// flag this mode has nothing to do with.
			name:     "termination-canary alone dispatches",
			args:     operatorModeArgs{TerminationCanary: true},
			wantMode: modeTerminationCanary,
		},
		{
			// Mutation that turns this row red: leave TerminationCanary out of the count above the switch. Two
			// modes are then requested and only one is seen, so the canary silently runs the inspection.
			name:       "termination-canary alongside another mode refuses",
			args:       operatorModeArgs{TerminationCanary: true, Inspect: true},
			wantErr:    true,
			wantErrHas: "only one of",
		},
		{
			// A canary is not a run, so an invocation that names a run id was written by somebody expecting
			// something this mode does not do.
			name:       "termination-canary combined with run-only flags refuses",
			args:       operatorModeArgs{TerminationCanary: true, RunOnlyFlags: []string{"-runid"}},
			wantErr:    true,
			wantErrHas: "-runid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, err := decideOperatorMode(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want a refusal, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErrHas) {
					t.Fatalf("refusal %q does not mention %q", err.Error(), tc.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if mode != tc.wantMode {
				t.Fatalf("mode = %v, want %v", mode, tc.wantMode)
			}
		})
	}
}

// The refusal above is only as good as what feeds it: it must report the flags the operator TYPED, which is
// not the same as the flags holding a non-default value. -horizon is the case that proves the difference —
// supplied with exactly its default, it is still a flag the operator believed did something.
func TestSuppliedRunOnlyFlagsReportsWhatWasTyped(t *testing.T) {
	newSet := func() *flag.FlagSet {
		fs := flag.NewFlagSet("queuelabrun", flag.ContinueOnError)
		fs.String("runid", "", "")
		fs.String("out", "", "")
		fs.Bool("preview", false, "")
		fs.Duration("horizon", time.Duration(horizonSec)*time.Second, "")
		fs.String("worker", "platform-worker", "")
		fs.Bool("inspect-worker", false, "")
		return fs
	}

	fs := newSet()
	if err := fs.Parse([]string{"-inspect-worker", "-worker", "platform-worker2"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := suppliedRunOnlyFlags(fs); len(got) != 0 {
		t.Fatalf("a recovery invocation with no run-only flags must report none, got %v", got)
	}

	fs = newSet()
	// -horizon is given its own default value, which is exactly the case a value comparison cannot see.
	if err := fs.Parse([]string{"-inspect-worker", "-runid", "r1", "-horizon",
		(time.Duration(horizonSec) * time.Second).String()}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := suppliedRunOnlyFlags(fs)
	if len(got) != 2 || got[0] != "-horizon" || got[1] != "-runid" {
		t.Fatalf("want [-horizon -runid] in a stable order, got %v", got)
	}
}

// The old `-inspect-worker platform-worker2` form still parses now that the flag is a bool, and would
// report on -worker's default while looking like it named a node, so a leftover positional argument is
// refused rather than ignored.
func TestRefuseExtraArgs(t *testing.T) {
	if err := refuseExtraArgs(nil); err != nil {
		t.Fatalf("no positional arguments must be allowed: %v", err)
	}
	err := refuseExtraArgs([]string{"platform-worker2"})
	if err == nil {
		t.Fatal("a leftover positional argument must be refused, not silently ignored")
	}
	if !strings.Contains(err.Error(), "platform-worker2") || !strings.Contains(err.Error(), "-worker") {
		t.Fatalf("the refusal must name the stray argument and the flag to use instead, got: %v", err)
	}
}

func TestHorizonForRefusesBelowTheFixedWindow(t *testing.T) {
	min := time.Duration(horizonSec) * time.Second
	if _, err := horizonFor(min-time.Second, selfCompletingProtocol()); err == nil {
		t.Fatal("a horizon below the protocol's fixed window must be refused")
	}
	if got, err := horizonFor(min, selfCompletingProtocol()); err != nil || got != min {
		t.Fatalf("horizonFor(min, selfCompletingProtocol()) = %v, %v; want %v, nil", got, err, min)
	}
	if got, err := horizonFor(min+time.Minute, selfCompletingProtocol()); err != nil || got != min+time.Minute {
		t.Fatalf("a wider horizon must be allowed unchanged: got %v, %v", got, err)
	}
}

// The horizon is derived from the dose, so resolving them separately is how a second regime would truncate
// its own runs: a window summed from the default dose is 20 s too long for one regime and would have been
// 20 s too short had the doses gone the other way.
//
// Mutation that turns this red: have HorizonSec use the package-level doseSec instead of p.DoseSec.
func TestDoseProtocolResolvesItsOwnHorizon(t *testing.T) {
	self, err := doseProtocolFor(string(queuelab.DoseSelfCompleting))
	if err != nil {
		t.Fatalf("the default regime must resolve: %v", err)
	}
	bounded, err := doseProtocolFor(string(queuelab.DoseGraceBounded))
	if err != nil {
		t.Fatalf("the grace-bounded regime must resolve: %v", err)
	}
	// The default must keep the window every earlier run was measured under, or this change silently
	// reinterprets the baseline it is meant to be compared against.
	if got := self.HorizonSec(); got != horizonSec {
		t.Fatalf("the default regime's window is %d s, want the protocol's %d s", got, horizonSec)
	}
	if bounded.HorizonSec() == self.HorizonSec() {
		t.Fatalf("both regimes report a %d s window, so the horizon is not following the dose", bounded.HorizonSec())
	}
	// The point of the second regime: an ignoring victim must still be running when the grace period expires.
	if rem := bounded.VictimServiceSec - bounded.DoseSec; rem < terminationGraceSec {
		t.Fatalf("grace-bounded leaves %d s of remaining service against a %d s grace, so the victim would "+
			"finish on its own and the regime measures the same thing as the default", rem, terminationGraceSec)
	}
	if _, err := doseProtocolFor("neither"); err == nil {
		t.Fatal("an unknown regime must be refused rather than falling back to the default")
	}
	// Zero means the selected regime's own window; anything shorter truncates it.
	h, err := horizonFor(0, bounded)
	if err != nil || h != time.Duration(bounded.HorizonSec())*time.Second {
		t.Fatalf("zero horizon must resolve to the regime's own window, got %v (%v)", h, err)
	}
	if _, err := horizonFor(time.Duration(bounded.HorizonSec())*time.Second-time.Second, bounded); err == nil {
		t.Fatal("a horizon below the regime's window must be refused")
	}
}
