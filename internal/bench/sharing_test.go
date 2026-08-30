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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const sizingOverheadMiB = 1400

// The finding that chose the card, kept as a test so it cannot quietly stop being true.
//
// Two Qwen2.5-3B engines do not fit on a T4, and the failure is the dangerous kind: the second engine
// starts, profiles, and finds a cache too small to hold one prompt. Nothing errors. The run produces
// latencies that describe eviction and look like a sharing-mode result.
func TestTwoEnginesOfTheFlagshipModelDoNotFitOnTheCardM5bUses(t *testing.T) {
	p := SharingPlan{Card: CardT4, Model: ModelQwen3B, Engines: 2, UtilizationPerEngine: 0.475, NonKVOverheadMiB: sizingOverheadMiB}

	err := p.Validate()
	if err == nil {
		t.Fatalf("a T4 hosting two %s engines validated; it leaves %d KV tokens each, which is less than one contender prompt",
			ModelQwen3B.Name, p.KVTokensPerEngine())
	}
	if !strings.Contains(err.Error(), "KV") {
		t.Errorf("the refusal does not name the KV cache as the thing that ran out: %v", err)
	}
	if got := p.KVTokensPerEngine(); got >= minUsefulKVTokens {
		t.Errorf("the T4 plan yields %d KV tokens per engine, which would make this test vacuous", got)
	}
}

// The card M5-c does use has to actually work, or the conclusion is only half checked.
func TestTwoEnginesOfTheFlagshipModelFitOnTheCardM5cUses(t *testing.T) {
	p := SharingPlan{Card: CardA10G, Model: ModelQwen3B, Engines: 2, UtilizationPerEngine: 0.475, NonKVOverheadMiB: sizingOverheadMiB}

	if err := p.Validate(); err != nil {
		t.Fatalf("an A10G hosting two %s engines was refused: %v", ModelQwen3B.Name, err)
	}
	// Enough to batch several contender prompts at once, which is what makes a sharing comparison mean
	// anything: two engines that can each only hold one sequence are not sharing, they are taking turns.
	if got := p.KVTokensPerEngine(); got < 8*7695 {
		t.Errorf("A10G plan leaves %d KV tokens per engine, too few to batch", got)
	}
}

// The exclusive arm is the same card with one engine, and it must remain the roomiest.
func TestTheExclusiveArmHasStrictlyMoreCacheThanTheSharedOne(t *testing.T) {
	one := SharingPlan{Card: CardA10G, Model: ModelQwen3B, Engines: 1, UtilizationPerEngine: 0.95, NonKVOverheadMiB: sizingOverheadMiB}
	two := SharingPlan{Card: CardA10G, Model: ModelQwen3B, Engines: 2, UtilizationPerEngine: 0.475, NonKVOverheadMiB: sizingOverheadMiB}

	if err := one.Validate(); err != nil {
		t.Fatalf("the exclusive arm was refused: %v", err)
	}
	if one.KVTokensPerEngine() <= two.KVTokensPerEngine() {
		t.Errorf("exclusive gives %d KV tokens and shared gives %d; if sharing did not cost cache there would be nothing to measure",
			one.KVTokensPerEngine(), two.KVTokensPerEngine())
	}
}

// Utilization that adds past the card is the mistake time-slicing invites, because the plugin advertises
// more devices and says nothing about memory.
func TestAPlanThatClaimsMoreThanOneCardIsRefused(t *testing.T) {
	p := SharingPlan{Card: CardA10G, Model: ModelQwen3B, Engines: 2, UtilizationPerEngine: 0.90, NonKVOverheadMiB: sizingOverheadMiB}

	err := p.Validate()
	if err == nil {
		t.Fatal("two engines at 0.90 utilization validated; that claims 1.8 cards from a machine that has one")
	}
	if !strings.Contains(err.Error(), "they do not create a second") {
		t.Errorf("the refusal does not explain that a shared pool is not a second pool: %v", err)
	}
}

// Not starting and thrashing are different outcomes, and the refusal has to say which one it is.
//
// This branch was reachable and untested: every other case here runs out of USEFUL cache before it runs
// out of cache, so disabling the negative-cache gate entirely left the suite green and only changed which
// sentence the operator read. The two sentences describe different mornings -- one where the Pod never
// became ready, and one where it did and the numbers are about eviction.
func TestAPlanWithNoCacheAtAllSaysTheEngineWillNotStart(t *testing.T) {
	p := SharingPlan{Card: CardT4, Model: ModelQwen3B, Engines: 2, UtilizationPerEngine: 0.40, NonKVOverheadMiB: sizingOverheadMiB}
	if p.KVMiBPerEngine() > 0 {
		t.Fatalf("this plan leaves %.0f MiB, so it no longer covers the negative-cache case", p.KVMiBPerEngine())
	}

	err := p.Validate()
	if err == nil {
		t.Fatal("a plan with negative KV cache validated")
	}
	if !strings.Contains(err.Error(), "will not start") {
		t.Errorf("the refusal reports thrashing rather than a failure to start, which sends the operator "+
			"looking at latencies for a Pod that never became ready: %v", err)
	}
}

// A plan whose cache is positive but tiny is still refused, because "it started" is not the bar.
func TestAPlanIsRefusedWhileItsCacheIsTooSmallToBatch(t *testing.T) {
	// Chosen to land between "engine starts" and "engine can batch": positive KV, far below four prompts.
	p := SharingPlan{Card: CardT4, Model: ModelQwen3B, Engines: 2, UtilizationPerEngine: 0.49, NonKVOverheadMiB: sizingOverheadMiB}
	if p.KVMiBPerEngine() <= 0 {
		t.Skip("this plan fails the earlier gate; the case it means to cover has moved")
	}
	err := p.Validate()
	if err == nil {
		t.Fatalf("a plan with %d KV tokens per engine validated", p.KVTokensPerEngine())
	}
	if !strings.Contains(err.Error(), "eviction") {
		t.Errorf("the refusal does not say what such an arm would actually measure: %v", err)
	}
}

// The committed manifests must form a plan Validate accepts, or the arithmetic is decoration.
//
// SharingPlan can refuse a bad plan and cannot refuse a bad manifest. What ties them is this test: it reads
// the engines' --gpu-memory-utilization and their count out of config/vllm-shared, and the replicas out of
// the time-slicing plugin's ConfigMap, and builds the plan those files actually describe.
func TestTheCommittedSharedEnginesFormAPlanThatValidates(t *testing.T) {
	// Globbed rather than named: the engines live one per file so each can be applied into its own
	// namespace, and a test that named one file would stop counting the moment a third engine was added --
	// silently, by validating a two-engine plan for a three-engine deployment.
	files, err := filepath.Glob("../../config/vllm-shared/engine-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no shared engine manifests found: %v", err)
	}
	var engines []byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		engines = append(engines, b...)
	}
	utils := regexp.MustCompile(`--gpu-memory-utilization=([0-9.]+)`).FindAllStringSubmatch(string(engines), -1)
	if len(utils) == 0 {
		t.Fatal("the shared engines set no --gpu-memory-utilization; under time-slicing that defaults each " +
			"engine to most of the card and the second one has nowhere to live")
	}

	// Every engine must claim the same slice: the matrix compares one mode against another, and engines
	// that differed in memory would differ in cache size too, which is the thing the mode is supposed to
	// change.
	first := utils[0][1]
	for _, u := range utils[1:] {
		if u[1] != first {
			t.Fatalf("engines claim different slices (%s and %s); the arms would differ in cache size as "+
				"well as in sharing mode", first, u[1])
		}
	}
	util, err := strconv.ParseFloat(first, 64)
	if err != nil {
		t.Fatalf("--gpu-memory-utilization %q is not a number: %v", first, err)
	}

	plan := SharingPlan{
		Card: CardA10G, Model: ModelQwen3B,
		Engines:              len(utils),
		UtilizationPerEngine: util,
		NonKVOverheadMiB:     sizingOverheadMiB,
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("the committed manifests describe a plan that cannot produce a result: %v", err)
	}

	// The plugin must advertise at least as many devices as there are engines, or the extra engines stay
	// Pending -- which looks like a scheduling problem and is a configuration one.
	cm, err := os.ReadFile("../../config/nvidia-device-plugin-timeslicing/configmap.yaml")
	if err != nil {
		t.Fatalf("read the sharing config: %v", err)
	}
	m := regexp.MustCompile(`replicas:\s*(\d+)`).FindStringSubmatch(string(cm))
	if m == nil {
		t.Fatal("the time-slicing ConfigMap declares no replicas; the plugin would advertise one device per card")
	}
	replicas, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("replicas %q is not a number: %v", m[1], err)
	}
	if replicas < len(utils) {
		t.Errorf("the plugin advertises %d device(s) and the manifests ask for %d engines; the surplus engines "+
			"stay Pending", replicas, len(utils))
	}
}

// The two sharing overlays must be mutually exclusive on one node, and equal in everything but the mechanism.
//
// They select the same node label on purpose -- MPS and time-slicing are two arms of one matrix on one
// machine, run at different times -- which makes applying both a live hazard rather than a hypothetical:
// two plugins registering nvidia.com/gpu against one kubelet socket, on a rented card. The script deletes
// one before applying the other; this checks the properties that make that both necessary and sufficient.
func TestTheSharingOverlaysAreExclusiveAndDifferOnlyInTheMechanism(t *testing.T) {
	ts, err := os.ReadFile("../../config/nvidia-device-plugin-timeslicing/daemonset.yaml")
	if err != nil {
		t.Fatalf("read the time-slicing overlay: %v", err)
	}
	mps, err := os.ReadFile("../../config/nvidia-device-plugin-mps/daemonset.yaml")
	if err != nil {
		t.Fatalf("read the MPS overlay: %v", err)
	}

	sel := regexp.MustCompile(`platform\.lkhun9311\.github\.io/gpu-sharing:\s*"true"`)
	if !sel.MatchString(string(ts)) || !sel.MatchString(string(mps)) {
		t.Fatal("the two overlays no longer select the same node, so the script's delete-then-apply is " +
			"guarding an exclusion that no longer exists and both could run at once elsewhere")
	}

	// Distinct DaemonSet names, or applying one adopts the other's pods and the delete never happens.
	nameOf := regexp.MustCompile(`(?m)^  name:\s*(\S+)`)
	names := map[string]string{}
	for label, b := range map[string][]byte{"timeslicing": ts, "mps": mps} {
		for _, m := range nameOf.FindAllStringSubmatch(string(b), -1) {
			if prev, dup := names[m[1]]; dup && prev != label {
				t.Errorf("both overlays declare %q; applying one would adopt the other's DaemonSet instead "+
					"of replacing it", m[1])
			}
			names[m[1]] = label
		}
	}

	// Same plugin digest. Sharing is a configuration of one binary, and arms that differed in the plugin
	// would differ in more than the mechanism under test.
	// Exactly 64 hex, because prose in these files abbreviates digests and a truncated one is not a pin.
	// The first version of this matched `sha256:ed39e22c...` inside a comment and reported the two arms as
	// running different builds -- a false alarm that would have been read as a real one.
	dig := regexp.MustCompile(`k8s-device-plugin:[^@\s]+@(sha256:[a-f0-9]{64})`)
	tsDig := dig.FindStringSubmatch(string(ts))
	mpsDig := dig.FindStringSubmatch(string(mps))
	if tsDig == nil || mpsDig == nil {
		t.Fatal("one of the overlays does not pin the plugin by digest")
	}
	if tsDig[1] != mpsDig[1] {
		t.Errorf("the arms run different plugin builds (%s and %s); the comparison would carry that "+
			"difference as well as the sharing mechanism", tsDig[1], mpsDig[1])
	}

	// Both must set CONFIG_FILE. Without it the plugin ignores the mounted config and advertises one device
	// per card -- the EXCLUSIVE behaviour under a manifest that says otherwise, which is an arm in name only.
	// Anchored on the whole env-var name. A substring check passed a manifest whose variable had been
	// renamed to CONFIG_FILE_DISABLED, which the plugin ignores exactly as it ignores an absent one.
	cfgVar := regexp.MustCompile(`(?m)^\s*-\s*name:\s*CONFIG_FILE\s*$`)
	for label, b := range map[string][]byte{"timeslicing": ts, "mps": mps} {
		if !cfgVar.MatchString(string(b)) {
			t.Errorf("the %s overlay does not set CONFIG_FILE; the plugin would ignore its sharing config "+
				"and the arm would be the exclusive one wearing another name", label)
		}
	}

	// Equal replica counts. The matrix varies the mechanism and holds the tenant count fixed; arms that
	// differed here would be measuring the count.
	rep := regexp.MustCompile(`replicas:\s*(\d+)`)
	tsCM, err := os.ReadFile("../../config/nvidia-device-plugin-timeslicing/configmap.yaml")
	if err != nil {
		t.Fatalf("read the time-slicing config: %v", err)
	}
	mpsCM, err := os.ReadFile("../../config/nvidia-device-plugin-mps/configmap.yaml")
	if err != nil {
		t.Fatalf("read the MPS config: %v", err)
	}
	a, b := rep.FindStringSubmatch(string(tsCM)), rep.FindStringSubmatch(string(mpsCM))
	if a == nil || b == nil {
		t.Fatal("a sharing config declares no replicas; the plugin would advertise one device per card")
	}
	if a[1] != b[1] {
		t.Errorf("time-slicing advertises %s replicas and MPS %s; the arms would differ in how many tenants "+
			"share the card as well as in how", a[1], b[1])
	}
}

// The write-up must not claim to be finished while it still has placeholders in it.
//
// M5-d is written before the run so its reasoning cannot be fitted to whatever the card produces, which
// means it ships full of markers. That is fine while it says so; it stops being fine the moment the page
// presents itself as a result. This is the check that keeps those two states apart, and it is the same
// discipline the report applies to an arm whose tail is too thin to be a tail.
func TestTheWriteUpDoesNotClaimNumbersItStillMarksAsMissing(t *testing.T) {
	b, err := os.ReadFile("../../hack/m5d-writeup.md")
	if err != nil {
		t.Fatalf("read the write-up: %v", err)
	}
	text := string(b)

	markers := regexp.MustCompile(`\[\[[A-Z0-9_]+\]\]`).FindAllString(text, -1)
	// The page's own title is the declaration that the numbers are absent. If it is edited to drop that,
	// every marker below becomes a claim.
	declares := strings.Contains(text, "with the numbers left out")

	switch {
	case len(markers) > 0 && !declares:
		t.Errorf("the write-up carries %d unfilled marker(s) and no longer says the numbers are missing; a "+
			"reader would take %q for a result", len(markers), markers[0])
	case len(markers) == 0 && declares:
		t.Error("every marker is filled but the write-up still announces that its numbers are missing, which " +
			"understates a finished result as badly as the other direction overstates an unfinished one")
	}
}
