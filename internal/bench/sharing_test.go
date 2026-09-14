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
// the engines' KV budget and their count out of config/vllm-shared, and the replicas out of the
// time-slicing plugin's ConfigMap, and builds the plan those files actually describe.
func TestTheCommittedSharedEnginesFormAPlanThatValidates(t *testing.T) {
	// Globbed rather than named: the engines live one per file so each can be applied into its own
	// namespace, and a test that named one file would stop counting the moment a third engine was added --
	// silently, by validating a two-engine plan for a three-engine deployment.
	files, err := filepath.Glob("../../config/vllm-shared/engine-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no shared engine manifests found: %v", err)
	}
	// Read the ARGS, with comments stripped, and never the file as a whole.
	//
	// The files are also read ONE AT A TIME now. Concatenating them first is what made the old count
	// meaningless: a flag missing from one manifest was invisible as long as the other carried it.
	//
	// This test used to match --gpu-memory-utilization anywhere in the bytes. The 2026-09-12 fix replaced
	// that flag with an absolute --kv-cache-memory and left a paragraph explaining why, so the only
	// remaining match was in engine-a's PROSE: one hit across two engine files. The test went on passing
	// while validating a ONE-engine plan at a fraction no engine is given, and the replicas check below
	// compared the plugin's devices against 1 instead of 2 -- so a ConfigMap dropped to one device would
	// have left an engine Pending with this suite green. A manifest test has to read what the engine will
	// actually be handed, which is the args list and nothing else.
	budgets := make([]int64, 0, len(files))
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		args := stripYAMLComments(string(b))
		m := regexp.MustCompile(`--kv-cache-memory=(\d+)`).FindAllStringSubmatch(args, -1)
		if len(m) != 1 {
			t.Fatalf("%s passes --kv-cache-memory %d times in its args; each engine manifest declares one "+
				"engine and must state its cache exactly once, or this plan describes a deployment that "+
				"does not exist", f, len(m))
		}
		// One engine per manifest is what makes len(files) the engine count. A replicas bump would put two
		// processes on the card for one entry here and halve every per-engine figure below, silently.
		if r := regexp.MustCompile(`(?m)^\s*replicas:\s*(\d+)`).FindStringSubmatch(args); r == nil || r[1] != "1" {
			t.Fatalf("%s does not declare replicas: 1, so the card hosts more engines than this plan counts", f)
		}
		v, perr := strconv.ParseInt(m[0][1], 10, 64)
		if perr != nil {
			t.Fatalf("--kv-cache-memory %q in %s is not a number: %v", m[0][1], f, perr)
		}
		budgets = append(budgets, v)
	}

	// Every engine must claim the same cache: the matrix compares one mode against another, and engines
	// that differed in memory would differ in cache size too, which is the thing the mode is supposed to
	// change. TestTheSplitEnginesClaimTheSameKVBudget argues this at length from the pilot that proved it.
	for i, v := range budgets[1:] {
		if v != budgets[0] {
			t.Fatalf("engines claim different caches (%d and %d bytes); the arms would differ in cache size "+
				"as well as in sharing mode", budgets[0], budgets[i+1])
		}
	}

	// The plan is expressed as a fraction of the card, so an absolute cache is turned back into one: what
	// an engine addresses is its cache plus its weights and overhead. The overhead here is the sizing
	// model's 1400 MiB estimate and the card's measured per-engine cost is nearer 1600, which makes this
	// the more permissive of the two arithmetics -- the measured fit is checked in
	// TestTheSplitEnginesClaimTheSameKVBudget, and this one exists to catch the plan that cannot be a plan.
	kvMiB := float64(budgets[0]) / (1 << 20)
	util := (kvMiB + float64(ModelQwen3B.WeightsMiB) + float64(sizingOverheadMiB)) / float64(CardA10G.TotalMiB)

	plan := SharingPlan{
		Card: CardA10G, Model: ModelQwen3B,
		Engines:              len(files),
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
	if replicas < len(files) {
		t.Errorf("the plugin advertises %d device(s) and the manifests ask for %d engines; the surplus engines "+
			"stay Pending", replicas, len(files))
	}
}

// The second split engine must clear vLLM's startup gate while the first already holds the card.
//
// This is the check that decides whether a split arm runs at all, and it is not the same question as
// whether the caches fit. vLLM v0.27.1's request_memory() raises ValueError when free memory is below
// gpu_memory_utilization * total, it reads that fraction whether or not --kv-cache-memory is configured,
// and its default is 0.92 -- 20.30 GiB of this card. The two engines start together, so whichever profiles
// second sees the card minus the first engine's whole footprint: 6.57 GiB of weights and non-torch memory,
// 0.59 for peak activation, 0.22 for CUDA graphs, and its cache. The 2026-09-12 pilot measured that second
// engine seeing 13.81 GiB free.
//
// Dropping --gpu-memory-utilization therefore does not relax this gate, it raises it to the default and
// both split arms fail to start. That was nearly bought: the flag was removed when the absolute cache was
// added, on the strength of vLLM's own advice to "Replace gpu_memory_utilization config with
// --kv-cache-memory=...", which is advice about sizing and not about admission.
func TestTheSecondSplitEngineClearsTheStartupGate(t *testing.T) {
	files, err := filepath.Glob("../../config/vllm-shared/engine-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sharing engine manifests found: %v", err)
	}

	// Measured on the card, from the engines' own startup reports, not from the sizing model.
	const (
		totalGiB      = 22.06
		weightsGiB    = 6.57 // consumed memory: weights + non-torch
		activationGiB = 0.59
		graphsGiB     = 0.22
	)

	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		args := stripYAMLComments(string(b))

		m := regexp.MustCompile(`--gpu-memory-utilization=([0-9.]+)`).FindStringSubmatch(args)
		if m == nil {
			t.Errorf("%s passes no --gpu-memory-utilization, so vLLM uses its 0.92 default and the engine "+
				"demands %.2f GiB free. The second engine to profile has about %.2f. It would not start",
				f, 0.92*totalGiB, totalGiB-(weightsGiB+activationGiB+graphsGiB+3.2))
			continue
		}
		util, perr := strconv.ParseFloat(m[1], 64)
		if perr != nil {
			t.Fatalf("--gpu-memory-utilization %q in %s is not a number: %v", m[1], f, perr)
		}

		kv := regexp.MustCompile(`--kv-cache-memory=(\d+)`).FindStringSubmatch(args)
		if kv == nil {
			continue // TestTheSplitEnginesClaimTheSameKVBudget says what that costs.
		}
		kvBytes, kerr := strconv.ParseFloat(kv[1], 64)
		if kerr != nil {
			t.Fatalf("unparseable --kv-cache-memory %q in %s: %v", kv[1], f, kerr)
		}
		kvGiB := kvBytes / (1 << 30)

		// What the gate demands, against what the card has left once one engine is up.
		demands := util * totalGiB
		freeForSecond := totalGiB - (weightsGiB + activationGiB + graphsGiB + kvGiB)
		if demands > freeForSecond {
			t.Errorf("%s asks to be admitted for %.2f GiB (utilization %.3f of %.2f), and the second engine "+
				"to profile sees %.2f GiB once the first holds its weights and %.2f GiB of cache. "+
				"request_memory() raises ValueError and the arm never starts",
				f, demands, util, totalGiB, freeForSecond, kvGiB)
		}
	}
}

// stripYAMLComments removes whole-line and trailing comments so a manifest test reads args, not prose.
//
// It cuts at an unquoted '#', which is enough for these manifests: every arg here is a bare scalar, and a
// '#' inside a quoted value would have to be added before this becomes wrong.
func stripYAMLComments(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
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

// devicePluginOverlays are the four configurations of one plugin the matrix switches between.
//
// Three of them belong to an arm: whole-card is `shared`, and the other two are the sharing modes. The
// fourth is the production overlay that runs on the ordinary GPU node group and is here because the checks
// below are about all of them agreeing on the things that are not the experiment.
var devicePluginOverlays = []string{
	"nvidia-device-plugin",
	"nvidia-device-plugin-whole-card",
	"nvidia-device-plugin-timeslicing",
	"nvidia-device-plugin-mps",
}

// Every plugin overlay must render into the namespace the operator actually creates.
//
// `system` in these manifests is kubebuilder's placeholder: config/default rewrites it, and no namespace by
// that name is ever created. An overlay that does not carry the rewrite renders `namespace: system` and is
// refused at apply time with `namespaces "system" not found`.
//
// Both sharing overlays were in exactly that state, so neither sharing arm of the matrix could ever have
// deployed. It survived because TestTheSharingOverlaysAreExclusiveAndDifferOnlyInTheMechanism compares the
// two of them against EACH OTHER: they were identically wrong, and agreement is not correctness. This test
// compares them against the cluster instead.
func TestEveryDevicePluginOverlayRendersIntoTheOperatorsNamespace(t *testing.T) {
	def, err := os.ReadFile("../../config/default/kustomization.yaml")
	if err != nil {
		t.Fatalf("read config/default/kustomization.yaml: %v", err)
	}
	nsLine := regexp.MustCompile(`(?m)^namespace:\s*(\S+)\s*$`)
	m := nsLine.FindStringSubmatch(string(def))
	if m == nil {
		t.Fatal("config/default declares no namespace, so there is nothing to hold the overlays to")
	}
	want := m[1]
	if want == "system" {
		t.Fatal("config/default declares the literal namespace `system`; this test's whole premise is that it does not")
	}

	for _, o := range devicePluginOverlays {
		path := "../../config/" + o + "/kustomization.yaml"
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			continue
		}
		got := nsLine.FindStringSubmatch(string(b))
		if got == nil {
			t.Errorf("config/%s/kustomization.yaml sets no namespace, so it renders the placeholder "+
				"`system` and every apply of it is refused with `namespaces \"system\" not found`; the arm "+
				"using it could never deploy", o)
			continue
		}
		if got[1] != want {
			t.Errorf("config/%s/kustomization.yaml renders into %q but the operator creates %q", o, got[1], want)
		}
	}
}

// The `shared` arm needs a plugin advertising ONE device on the sharing node, and for a long time none
// existed.
//
// config/nvidia-device-plugin selects platform.lkhun9311.github.io/gpu, a label the sharing node group
// deliberately does not carry (infra/aws/cluster/eks.tf says why: two plugins on one kubelet socket). The
// two sharing overlays select gpu-sharing and advertise two. So the control arm's engine -- the one the
// pre-registration says every other number is measured against -- had a plugin for neither its node nor its
// topology. On a fresh node it would have sat Pending to the 900-second rollout timeout; run after a sharing
// arm it would have scheduled onto a leftover replica and reported a control that was quietly running on
// half a card.
//
// These are the properties that make the whole-card overlay the `shared` arm's plugin rather than a copy of
// one of the others.
func TestTheWholeCardOverlayIsTheExclusivePluginOnTheSharingNode(t *testing.T) {
	whole, err := os.ReadFile("../../config/nvidia-device-plugin-whole-card/daemonset.yaml")
	if err != nil {
		t.Fatalf("read the whole-card overlay: %v", err)
	}

	if !regexp.MustCompile(`platform\.lkhun9311\.github\.io/gpu-sharing:\s*"true"`).Match(whole) {
		t.Error("the whole-card overlay does not select the sharing node, so the `shared` arm's engine would " +
			"find nothing advertising a device and sit Pending to its rollout timeout")
	}
	if regexp.MustCompile(`(?m)^\s*platform\.lkhun9311\.github\.io/gpu:\s*"true"`).Match(whole) {
		t.Error("the whole-card overlay selects the ordinary GPU label as well; it would land on the node " +
			"the production plugin already serves and two plugins would register against one kubelet socket")
	}

	// No CONFIG_FILE, and that omission is the overlay.
	//
	// The plugin's default is one device per physical card, which is exactly what this arm needs. The check
	// runs in the opposite direction from the one on the sharing overlays: there a missing CONFIG_FILE makes
	// a sharing arm exclusive, here a present one would split the control's card.
	if regexp.MustCompile(`(?m)^\s*-\s*name:\s*CONFIG_FILE\s*$`).Match(whole) {
		t.Error("the whole-card overlay sets CONFIG_FILE; it would advertise a split card and the `shared` " +
			"arm would be a sharing arm wearing the control's name")
	}

	// One digest across all four, so an arm cannot differ in the plugin build as well as in the mechanism.
	dig := regexp.MustCompile(`k8s-device-plugin:[^@\s]+@(sha256:[a-f0-9]{64})`)
	digests := map[string]string{}
	for _, o := range devicePluginOverlays {
		b, rerr := os.ReadFile("../../config/" + o + "/daemonset.yaml")
		if rerr != nil {
			t.Fatalf("read config/%s/daemonset.yaml: %v", o, rerr)
		}
		d := dig.FindStringSubmatch(string(b))
		if d == nil {
			t.Fatalf("config/%s does not pin the plugin by digest", o)
		}
		digests[o] = d[1]
	}
	for o, d := range digests {
		if d != digests["nvidia-device-plugin"] {
			t.Errorf("config/%s runs plugin %s but the production overlay runs %s; an arm would carry that "+
				"difference as well as its topology", o, d, digests["nvidia-device-plugin"])
		}
	}

	// Distinct DaemonSet names across all four, or applying one adopts another's Pods rather than replacing
	// it -- and the production overlay is in that set on purpose, because a name collision there would
	// retarget the plugin serving the ordinary GPU node group.
	nameOf := regexp.MustCompile(`(?m)^  name:\s*(\S+)`)
	owner := map[string]string{}
	for _, o := range devicePluginOverlays {
		b, _ := os.ReadFile("../../config/" + o + "/daemonset.yaml")
		for _, n := range nameOf.FindAllStringSubmatch(string(b), -1) {
			if prev, dup := owner[n[1]]; dup && prev != o {
				t.Errorf("config/%s and config/%s both declare %q; applying one would adopt the other's "+
					"DaemonSet instead of replacing it", prev, o, n[1])
			}
			owner[n[1]] = o
		}
	}
}

// The matrix must apply a device plugin for every arm it deploys, including the control.
//
// This is the check that would have caught the missing whole-card plugin. The script applied one for the two
// sharing arms and nothing for `shared`, which reads as an omission only if you already know the sharing
// node has no plugin of its own -- so the guard is mechanical: every arm in the case statement reaches
// apply_device_plugin.
func TestTheMatrixAppliesADevicePluginForEveryArm(t *testing.T) {
	body, err := os.ReadFile("../../hack/m5c-matrix.sh")
	if err != nil {
		t.Fatalf("read the matrix: %v", err)
	}
	src := string(body)

	for _, arm := range []string{"shared", "timeSlicing", "mps"} {
		if !regexp.MustCompile(`(?m)^\s+`+arm+`\)\s`).MatchString(src) &&
			!strings.Contains(src, arm+"|") && !strings.Contains(src, "|"+arm) {
			t.Errorf("hack/m5c-matrix.sh has no case branch for the %s arm", arm)
		}
	}
	if !strings.Contains(src, "apply_device_plugin shared") {
		t.Error("hack/m5c-matrix.sh does not apply a device plugin for the `shared` arm; nothing advertises " +
			"nvidia.com/gpu on the sharing node, so the control's engine would sit Pending or inherit the " +
			"previous arm's split card")
	}
	if !strings.Contains(src, `apply_device_plugin "$arm"`) {
		t.Error("hack/m5c-matrix.sh does not apply a device plugin for the sharing arms")
	}
	// Exactly, not at least: `-ge` reads the previous arm's larger advertisement as success, which is how a
	// one-device arm starts on a card the last arm already split.
	if !strings.Contains(src, `-eq "$want"`) {
		t.Error("hack/m5c-matrix.sh does not require the node to advertise EXACTLY the arm's device count; " +
			"a `-ge` comparison passes on the outgoing arm's stale advertisement")
	}
}

// The matrix must pass the WHOLE load to gen-trace, and the served model with it.
//
// hack/m5b-price-of-protection.sh carries the lesson in one line beside its own gen-trace call: "gen-trace's
// defaults are stub-calibrated and the first pilot ran them at a GPU at ten times its prefill capacity."
// hack/m5c-matrix.sh asked its operator for RATE and left everything else defaulted, which is the larger
// half of the same mistake and could not be fixed by any choice of RATE:
//
//   - The default mix is premium 1, noisy 1 and two probe tenants at 0.1, so the 40,000-character contender
//     takes about 45% of arrivals. At roughly 1.03 s of engine per contender prompt -- the figure the paid
//     evidence measured -- that is four to five times an A10G's prefill capacity at the rate this study
//     would use, and every arm is censored.
//   - Lowering RATE until the contender fits leaves the premium tenant below the MinTailSamples floor that
//     reading 4b exists to enforce, so the run is INVALID from the other direction.
//
// And --model, which is a different failure with the same silence: gen-trace defaults to "llama-3-8b",
// internal/gateway resolves a backend by matching the requested model against the InferenceDeployment index
// in the tenant's namespace, and the routing records this script writes serve Qwen2.5-3B. Every request of
// every arm would have come back ErrNoRoute, after both engines had finished loading.
func TestTheMatrixPassesTheWholeLoadAndTheModelToGenTrace(t *testing.T) {
	src, err := os.ReadFile("../../hack/m5c-matrix.sh")
	if err != nil {
		t.Fatalf("read the matrix: %v", err)
	}
	body := string(src)

	// COMMENTS STRIPPED FIRST, and this is not tidiness.
	//
	// The first version of this test matched from the word "gen-trace" onwards over the whole file, and the
	// rationale comment directly above the call names every flag it is arguing for -- including --model. So
	// deleting `--model "$MODEL"` from the actual command left this test green, satisfied entirely by the
	// prose explaining why the flag matters. Found by deleting the flag and watching nothing go red, which
	// is the same way this repository found `use_name_prefix` and CONFIG_FILE being satisfied by comments.
	var code strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	stripped := code.String()

	gen := regexp.MustCompile(`(?s)benchharness" gen-trace.*?manifest-out[^\n]*\n`).FindString(stripped)
	if gen == "" {
		t.Fatal("hack/m5c-matrix.sh has no gen-trace invocation to check")
	}
	for _, flag := range []string{
		"--rate", "--duration-ms", "--model",
		"--premium-weight", "--noisy-weight", "--probe-weight",
	} {
		if !strings.Contains(gen, flag) {
			t.Errorf("the matrix's gen-trace call does not pass %s, so it takes the harness's "+
				"stub-calibrated default for it", flag)
		}
	}

	// Refused, not defaulted. A default the operator never sees is how the stub calibration got onto a card
	// the first time.
	for _, v := range []string{"RATE", "PREMIUM_WEIGHT", "NOISY_WEIGHT", "PROBE_WEIGHT", "DURATION_MS"} {
		if !strings.Contains(stripped, v) {
			t.Errorf("the matrix never mentions %s", v)
			continue
		}
		if regexp.MustCompile(`(?m)^` + v + `="\$\{` + v + `:-`).MatchString(stripped) {
			t.Errorf("the matrix gives %s a default. Every part of the load has to be derived on the card "+
				"this run is using; a default here is a number nobody measured arriving in the evidence", v)
		}
	}

	// The derived trace length is gone, and it must stay gone: 500/(RATE/2) assumes the two tenants split
	// arrivals evenly, which is true of the defaults this study is refusing and false of any mix it derives.
	if regexp.MustCompile(`DURATION_MS=\$\(python3`).MatchString(stripped) {
		t.Error("the matrix still derives DURATION_MS from RATE, overwriting whatever the caller measured; " +
			"the arithmetic assumes an even tenant split, which is the premise a calibrated mix abandons")
	}
}

// sessionRunners are the scripts that build an EC2 user-data payload and launch an instance with it.
var sessionRunners = []string{
	"hack/m5b-scheduler-microtest.sh",
	"hack/m5b-price-of-protection.sh",
	"hack/queuelab-gpu-session.sh",
	"hack/m5c-gpu-session.sh",
}

// Every runner that builds user-data must re-emit the shebang, and must refuse a payload that lacks one.
//
// These scripts keep the instance's script in a heredoc and strip it on the way out -- EC2 caps user-data at
// 25600 encoded bytes and the explanations do not fit. The stripping starts with `tail -n +2`, which drops
// line 1, which is `#!/bin/bash`. cloud-init executes user-data as a script ONLY when it begins with `#!`.
//
// hack/m5c-gpu-session.sh was written from the shape of the others and dropped that line. On 2026-09-11 its
// first paid run launched a g5.2xlarge, cloud-init declined to execute the payload, and the instance sat
// idle for 145 minutes -- about $1.64 -- and produced nothing. There was not even a log, because the trap
// that uploads one lives inside the script that never ran.
//
// NOTHING CAUGHT IT, and that is why this test exists rather than a comment. `bash -n` passes on a script
// with no shebang, since it parses fine. The characterization goldens pass because the stubs record the
// launch without executing the payload. Every check was green and none of them could see it.
func TestEverySessionRunnerEmitsAShebangAndRefusesAPayloadWithout(t *testing.T) {
	for _, r := range sessionRunners {
		body := readRepoFile(t, r)

		// It must put the shebang back after stripping it.
		if !strings.Contains(body, `echo "#!/bin/bash"`) {
			t.Errorf("%s never re-emits a shebang. Its user-data stripping drops the heredoc's own with "+
				"`tail -n +2`, so cloud-init would not execute the payload and the instance would boot, do "+
				"nothing, and bill until its backstop", r)
		}

		// And it must refuse rather than launch if one is missing anyway -- a guard that only works when
		// the code above it is right is not a guard.
		if !regexp.MustCompile(`head -1 "\$UD" \| grep -q '\^#!'`).MatchString(body) {
			t.Errorf("%s does not check that its generated user-data begins with a shebang before launching. "+
				"`bash -n` cannot see this: a script without one parses perfectly and does not run", r)
		}
	}
}

// The session script and the matrix must agree on which arms a default run measures.
//
// hack/m5c-gpu-session.sh EXPORTS ARMS into hack/m5c-matrix.sh, so when the two defaults differ the
// session's wins silently and the matrix's is dead text. They did differ: the matrix gained R1 after the
// first paid run showed its readings could not be evaluated without the isolated baseline, and the session
// still carried the three-arm list, which would have bought a second run with no denominator.
//
// This is the same defect shape the repetition-count test above was written for, where two scripts that do
// not read each other disagreed about REPS and a re-run silently bought half the repetitions the design was
// built on. The fix is not the value. It is that a disagreement now fails.
func TestTheSessionAndTheMatrixAgreeOnTheArms(t *testing.T) {
	re := regexp.MustCompile(`(?m)^ARMS="\$\{ARMS:-([^}]*)\}"\s*$`)

	want, wantFrom := "", ""
	for _, script := range []string{"hack/m5c-matrix.sh", "hack/m5c-gpu-session.sh"} {
		m := re.FindStringSubmatch(readRepoFile(t, script))
		if m == nil {
			t.Fatalf("%s has no ARMS default in the expected form", script)
		}
		if want == "" {
			want, wantFrom = m[1], script
			continue
		}
		if m[1] != want {
			t.Errorf("%s defaults ARMS to %q but %s defaults to %q. The session exports this into the matrix, "+
				"so the difference is not a preference: it decides which arms a paid run measures",
				script, m[1], wantFrom, want)
		}
	}

	// And R1 has to be among them, because it is the denominator of both of this study's bars.
	if !strings.Contains(want, ArmR1) {
		t.Errorf("the default arm set is %q and does not include %s, the isolated baseline both bars are "+
			"ratios against. A run without it can produce numerators and nothing to divide them by", want, ArmR1)
	}
}

// Every arm the matrix runs by default must have a branch that can deploy it.
//
// ARMS and deploy_arm are the same list written twice, and they disagreed: the default named R1 and
// deploy_arm had cases only for shared and for the sharing pair. A run would have reached the R1 cell,
// fallen through the case with PREMIUM_NS unset, and deployed nothing before replaying at a gateway that
// was not there.
//
// The version of this defect that was actually bought is the mirror image -- deploy_arm had no R1 case and
// ARMS did not name it either, so the matrix simply could not produce the baseline both bars divide by, and
// the readings declined to evaluate anything. Either way the check is the same: the two lists are one list.
func TestEveryDefaultArmHasADeployBranch(t *testing.T) {
	body := readRepoFile(t, "hack/m5c-matrix.sh")

	armsRe := regexp.MustCompile(`(?m)^ARMS="\$\{ARMS:-([^}]*)\}"\s*$`)
	m := armsRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("hack/m5c-matrix.sh has no ARMS default in the expected form")
	}

	// The case labels deploy_arm offers, e.g. "R1|shared)" and "timeSlicing|mps)".
	deploy := body[strings.Index(body, "deploy_arm() {"):]
	if i := strings.Index(deploy, "\nrouting_record"); i > 0 {
		deploy = deploy[:i]
	}
	branches := map[string]bool{}
	for _, c := range regexp.MustCompile(`(?m)^\s{4}([A-Za-z0-9|]+)\)`).FindAllStringSubmatch(deploy, -1) {
		for label := range strings.SplitSeq(c[1], "|") {
			branches[label] = true
		}
	}
	if len(branches) == 0 {
		t.Fatal("no case branches found in deploy_arm; this check would pass vacuously")
	}

	for arm := range strings.FieldsSeq(m[1]) {
		if !branches[arm] {
			t.Errorf("the default arm set runs %q and deploy_arm has no branch for it, so that cell would "+
				"deploy nothing and then replay against whatever the previous arm left behind", arm)
		}
	}
}

// Every tenant the trace can generate must have an API key, or not be generated at all.
//
// The gateway resolves a tenant from an API key and refuses a request that carries none. The matrix's replay
// carries keys for the premium and contending tenants; gen-trace also emits two probe tenants whenever
// their weight is above zero, and those have no key. The first working pilot sent 41 such requests and the
// gateway turned every one away, which the report recorded as two VOID rows.
//
// hack/lib/spot-run.sh's header already names this failure twice -- "one paid run answered a quarter of
// every replay with 401, the next answered its probe tenants with 403" -- so the run that found it was the
// third. This is the check that makes a fourth fail here instead.
func TestNoTenantIsSentWithoutAKey(t *testing.T) {
	matrix := readRepoFile(t, "hack/m5c-matrix.sh")
	session := readRepoFile(t, "hack/m5c-gpu-session.sh")

	keys := regexp.MustCompile(`--api-keys "([^"]*)"`).FindStringSubmatch(matrix)
	if keys == nil {
		t.Fatal("the matrix's replay passes no --api-keys at all")
	}
	keyed := map[string]bool{}
	for pair := range strings.SplitSeq(keys[1], ",") {
		if name, _, ok := strings.Cut(pair, "="); ok {
			keyed[strings.TrimSpace(name)] = true
		}
	}
	for _, tenant := range []string{PremiumTenant, NoisyTenant} {
		if !keyed[tenant] {
			t.Errorf("the trace sends %s and the replay carries no key for it; the gateway would refuse "+
				"every one of its requests", tenant)
		}
	}

	// The probe tenants are the ones with no key. They may only be generated if they are given one.
	probeWeight := regexp.MustCompile(`(?m)^PROBE_WEIGHT="\$\{PROBE_WEIGHT:-([^}]*)\}"`).FindStringSubmatch(session)
	if probeWeight == nil {
		t.Fatal("hack/m5c-gpu-session.sh has no PROBE_WEIGHT default in the expected form")
	}
	probesKeyed := keyed[ProbeUnderTenant] && keyed[ProbeOverTenant]
	if probeWeight[1] != "0" && !probesKeyed {
		t.Errorf("PROBE_WEIGHT defaults to %q, so gen-trace emits %s and %s, and the replay carries keys for "+
			"neither. Either set it to 0 or give them keys -- requests the gateway refuses are not a "+
			"measurement, and this run's report marks them VOID",
			probeWeight[1], ProbeUnderTenant, ProbeOverTenant)
	}
}

// No runner may apply a redirection to its own shell with a bare `exec`.
//
// `exec` with redirections and no command changes the CURRENT shell, permanently. hack/m5c-matrix.sh
// briefly ended its port-forward probe with `exec 3<&- 2>/dev/null`, which sent the matrix's own stderr to
// /dev/null for the rest of the run: `set -x` traces, the cell-budget's STOPPING message and every `fail`
// after the first cell all vanished. The run that exposed it stopped after one cell having said nothing at
// all, not even from its EXIT trap, and read exactly like a shell that had been killed.
//
// The line `exec > >(tee ...)` at the top of a user-data heredoc is the legitimate use -- a script
// deliberately capturing its own output from the first line -- so it is allowed. What is not is a
// redirection applied mid-script, which is always a mistake in these files: the intent is to silence one
// command and the effect is to silence the rest of the run.
func TestNoRunnerSilencesItsOwnShellMidScript(t *testing.T) {
	runners := append([]string{"hack/m5c-matrix.sh"}, sessionRunners...)
	// Scanned rather than matched at the start of a line, because the occurrence that caused this was in the
	// MIDDLE of one: `then pf_up=1; exec 3<&- 2>/dev/null; break`. A line-anchored pattern walked straight
	// past it, which is how the first version of this test passed while the defect was put back.
	//
	// What is allowed is `(exec ...` -- a subshell opening a descriptor, which is the probe's own form and
	// affects nothing outside it -- and `exec > >(tee ...)`, a script deliberately capturing its own output
	// from its first line.
	capture := regexp.MustCompile(`exec\s*>\s*>\(tee`)
	redir := regexp.MustCompile(`^\s*[0-9]*[<>]`)

	for _, r := range runners {
		for line := range strings.SplitSeq(readRepoFile(t, r), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || capture.MatchString(trimmed) {
				continue
			}
			for i := 0; i+4 <= len(line); i++ {
				if line[i:i+4] != "exec" {
					continue
				}
				// A whole word, and not `(exec` which is the subshell form.
				if i > 0 && line[i-1] == '(' {
					continue
				}
				if i > 0 && (isWordByte(line[i-1])) {
					continue
				}
				rest := line[i+4:]
				if rest == "" || !redir.MatchString(rest) {
					continue
				}
				t.Errorf("%s applies a redirection to its own shell: %q. `exec` with no command changes "+
					"this shell for the rest of the run, so a fragment meant to silence one command "+
					"silences every message the script has left to give", r, trimmed)
				break
			}
		}
	}
}

// isWordByte reports whether b could be part of a shell word, so that "exec" inside a longer name is not
// mistaken for the builtin.
func isWordByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' || b == '/' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// The split engines must claim the SAME KV budget as each other.
//
// The whole of the split arm is two engines treated alike, so an asymmetry between them is a second
// variable inside the arm that is supposed to hold one. The 2026-09-12 pilot measured exactly that: at
// --gpu-memory-utilization=0.475 the engine that started second saw the first's allocation as memory
// already consumed and was left with 1.52 GiB of KV against 3.33 -- 44,144 tokens against 97,056, which is
// 5.7 of this study's contender prompts against 12.5. The smaller engine timed out 188 of the contender's
// 238 requests and reading 4b declared the run INVALID.
//
// An absolute --kv-cache-memory removes the start-order dependence, and this is what keeps the two values
// equal. TestTheCommittedSharedEnginesFormAPlanThatValidates checks the FRACTIONS agree and went quiet the
// moment the fractions were replaced, so the property moved and its guard did not.
func TestTheSplitEnginesClaimTheSameKVBudget(t *testing.T) {
	files, err := filepath.Glob("../../config/vllm-shared/engine-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sharing engine manifests found: %v", err)
	}

	re := regexp.MustCompile(`--kv-cache-memory=(\d+)`)
	want, wantFrom := "", ""
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		m := re.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s sets no --kv-cache-memory, so its cache is whatever the engine derives from a card "+
				"another engine is already using -- which is the asymmetry that invalidated the 2026-09-12 pilot", f)
			continue
		}
		if want == "" {
			want, wantFrom = m[1], f
			continue
		}
		if m[1] != want {
			t.Errorf("%s claims %s bytes of KV and %s claims %s. The split arm is two engines treated alike; "+
				"a difference here is a second variable inside the arm that is meant to hold one",
				f, m[1], wantFrom, want)
		}
	}

	// And the pair must fit the card with room for what is not cache.
	//
	// Derived from the engines' own startup report rather than from the sizing model: 22.06 GiB visible on
	// an A10G, and 7.32 GiB per engine of weights, non-torch memory, peak activation and CUDA graphs.
	if want == "" {
		return
	}
	bytes, perr := strconv.ParseFloat(want, 64)
	if perr != nil {
		t.Fatalf("unparseable --kv-cache-memory %q", want)
	}
	const visibleGiB, fixedPerEngineGiB = 22.06, 7.32
	kvGiB := bytes / (1 << 30)
	if used := 2 * (kvGiB + fixedPerEngineGiB); used > visibleGiB {
		t.Errorf("two engines at %.2f GiB of KV each need %.2f GiB with their weights and activations, and "+
			"the card shows %.2f. The second engine would fail to start or start with less than it asked for",
			kvGiB, used, visibleGiB)
	}
}

// No unquoted heredoc may contain an unescaped backtick, because the shell runs it.
//
// These runners build Kubernetes manifests with `k apply -f - <<EOF`, unquoted on purpose so that $arm,
// $GW_IMAGE and $NS_A expand. An unquoted heredoc expands backticks too, so prose inside one is executed:
// hack/m5c-matrix.sh carried a YAML comment reading "without one `rollout status` means only that the
// container started", and every gateway deploy ran `rollout status` and printed "rollout: command not
// found" to stderr. Four times a run, in the log an operator reads to find real faults.
//
// It was harmless only by accident -- the empty substitution landed inside a comment. The same line one
// indent further left would have edited a manifest, and any backticked text naming a real command would
// have run it against the rented cluster.
//
// `bash -n` cannot see this: the script is syntactically perfect. Only executing it shows the error, and
// only a reader who noticed one unfamiliar line in several hundred would catch it there.
func TestNoUnquotedHeredocExecutesItsOwnProse(t *testing.T) {
	runners := append([]string{"hack/m5c-matrix.sh"}, sessionRunners...)
	// <<EOF and <<-EOF expand; <<'EOF' and <<"EOF" do not. The delimiter is captured so the end of the
	// heredoc is found rather than guessed.
	//
	// (?:^|[^<]) excludes the here-string `<<<`, and comment lines are skipped before this runs. Both were
	// found by this test's own first run: it reported nine hits in the two session runners, every one of
	// them the section marker `# <<< REHEARSABLE`. A guard that cries wolf on its first outing gets read as
	// noise, which is the failure mode that matters more here than a missed case.
	open := regexp.MustCompile(`(?:^|[^<])<<-?\s*([A-Za-z_][A-Za-z0-9_]*)\s*(\||$)`)

	for _, runner := range runners {
		path := filepath.Join("../..", runner)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", runner, err)
		}
		var delim string
		var startLine int
		for i, line := range strings.Split(string(b), "\n") {
			if delim == "" {
				// A heredoc cannot be opened from inside a comment.
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if m := open.FindStringSubmatch(line); m != nil {
					delim, startLine = m[1], i+1
				}
				continue
			}
			if strings.TrimSpace(line) == delim {
				delim = ""
				continue
			}
			// Inside an expanding heredoc. A backslash-escaped backtick is the deliberate form and is fine.
			for j := 0; j < len(line); j++ {
				if line[j] != '`' {
					continue
				}
				if j > 0 && line[j-1] == '\\' {
					continue
				}
				t.Errorf("%s:%d is inside the unquoted heredoc opened at line %d and contains an unescaped "+
					"backtick, so the shell runs what is between them:\n\t%s\nQuote it with \"\" or escape "+
					"it as \\`; the heredoc cannot be quoted because it needs its variables expanded",
					runner, i+1, startLine, strings.TrimSpace(line))
				break
			}
		}
	}
}

// A process the runner intends to KILL may not be launched through a shell-function wrapper.
//
// These scripts define `k() { kubectl --context "$KCTX" "$@"; }` and use it everywhere, which is good for
// every call except one shape: `k something &` followed later by `kill $!`. Backgrounding a function runs
// it in a SUBSHELL, and bash replaces that subshell with the command only when it has no traps left to
// run -- these runners install three. So `$!` names the subshell, `kill` ends the subshell, `wait` reaps
// it and returns promptly, and the real process keeps running as an orphan.
//
// Measured on a rented card on 2026-09-12: the R1 cell replayed 3,882 rows through a port-forward, the
// next cell asked for the same local port and got "bind: address already in use", and the session ended
// with one arm of four. The kill-and-wait that was added to close exactly that race had been waiting on
// the wrong process since it was written, so the race it governed was never governed at all.
//
// It is invisible to `bash -n`, to a reading of the teardown, and to any rehearsal where the orphan
// happens to die on its own when its target pod is replaced -- which is most of them, most of the time.
func TestNothingTheRunnerWillKillIsLaunchedThroughAFunctionWrapper(t *testing.T) {
	runners := append([]string{"hack/m5c-matrix.sh"}, sessionRunners...)
	// Parsed by first word and last character rather than by a pattern over the middle.
	//
	// The first version of this test was a regex whose body excluded "&" so it would not catch "&&". The
	// real line ends "2>&1 &", the redirection's ampersand stopped the match dead, and the test passed on a
	// file that still had the defect in it. It was written to catch one specific line and could not see
	// that line. Checking the command word and the trailing byte has no middle to get wrong.
	isBackgroundedWrapper := func(line string) bool {
		t := strings.TrimSpace(line)
		if !strings.HasSuffix(t, "&") || strings.HasSuffix(t, "&&") {
			return false
		}
		first, _, _ := strings.Cut(t, " ")
		return first == "k"
	}

	for _, runner := range runners {
		path := filepath.Join("../..", runner)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", runner, err)
		}
		lines := strings.Split(string(b), "\n")

		// Only a runner that actually kills something can be bitten by this.
		kills := false
		for _, line := range lines {
			if strings.Contains(line, "kill ") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				kills = true
				break
			}
		}
		if !kills {
			continue
		}
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if isBackgroundedWrapper(line) {
				t.Errorf("%s:%d backgrounds the `k` function in a script that kills a pid, so $! is a "+
					"subshell and the kill never reaches the real process:\n\t%s\nCall kubectl directly here",
					runner, i+1, strings.TrimSpace(line))
			}
		}
	}
}
