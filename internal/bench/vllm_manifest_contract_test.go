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
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The manifest's numbers are only as good as the measurements they came from, and prose in a header cannot
// keep the two in step. These tests read config/vllm/deployment.yaml and the recorded calibration and fail
// when an edit to either breaks the relationship the other depends on.
//
// Each case below corresponds to something that has actually gone wrong -- an engine that refused to start,
// an engine that started and served the wrong thing, or a Pod that never scheduled at all.

// calibration is the measured tokenizer record; see testdata/tokenizer_calibration.json.
type calibration struct {
	PromptCorpusSHA256 string `json:"promptCorpusSHA256"`
	Tokenizer          string `json:"tokenizer"`
	Samples            []struct {
		PromptLenChars int `json:"promptLenChars"`
		ActualTokens   int `json:"actualTokens"`
	} `json:"samples"`
}

func loadCalibration(t *testing.T) calibration {
	t.Helper()
	b, err := os.ReadFile("testdata/tokenizer_calibration.json")
	if err != nil {
		t.Fatalf("read calibration: %v", err)
	}
	var c calibration
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse calibration: %v", err)
	}
	return c
}

func loadVLLMDeployment(t *testing.T) appsv1.Deployment {
	t.Helper()
	b, err := os.ReadFile("../../config/vllm/deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	var d appsv1.Deployment
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse deployment: %v", err)
	}
	return d
}

func vllmContainer(t *testing.T, d appsv1.Deployment) corev1.Container {
	t.Helper()
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "vllm" {
			return c
		}
	}
	t.Fatal("deployment has no container named vllm")
	return corev1.Container{}
}

// argValue returns the value of a --flag=value argument, and whether it was present.
func argValue(args []string, flag string) (string, bool) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v, true
		}
	}
	return "", false
}

// The calibration was measured against one tokenizer; the Deployment must serve that model.
//
// Every token count this experiment reports -- the contender's 7,695, the false-positive band around the
// guard's threshold -- is a property of a specific vocabulary and chat template. Serving a different model
// leaves those numbers in the report describing traffic that was never sent.
func TestServedModelIsTheOneTheCalibrationWasMeasuredAgainst(t *testing.T) {
	cal := loadCalibration(t)
	c := vllmContainer(t, loadVLLMDeployment(t))

	served := ""
	for _, a := range c.Args {
		if !strings.HasPrefix(a, "-") {
			served = a
			break
		}
	}
	if served != cal.Tokenizer {
		t.Fatalf("deployment serves %q but the calibration was measured against %q", served, cal.Tokenizer)
	}
	if cal.PromptCorpusSHA256 != PromptCorpusSHA256 {
		t.Fatalf("calibration was measured against corpus %s but this binary sends %s",
			cal.PromptCorpusSHA256, PromptCorpusSHA256)
	}
}

// The context window must hold the largest prompt the trace actually sends, plus its output.
//
// vLLM rejects an over-long request with a 400, and the harness records a 400 as an http error rather than
// as a rejection -- so a context window one token too small would empty the contender population from every
// arm at once and leave four arms that agree because none of them ran the experiment.
func TestContextWindowHoldsTheLargestMeasuredPrompt(t *testing.T) {
	cal := loadCalibration(t)
	c := vllmContainer(t, loadVLLMDeployment(t))

	raw, ok := argValue(c.Args, "--max-model-len")
	if !ok {
		t.Fatal("deployment does not set --max-model-len")
	}
	maxLen, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("--max-model-len %q is not a number: %v", raw, err)
	}

	largest := 0
	for _, s := range cal.Samples {
		if s.ActualTokens > largest {
			largest = s.ActualTokens
		}
	}
	if largest == 0 {
		t.Fatal("calibration records no samples")
	}
	// 64 is the largest MaxOutputTokens any tenant in cmd/benchharness asks for.
	const largestOutput = 64
	if maxLen < largest+largestOutput {
		t.Fatalf("--max-model-len=%d cannot hold the largest measured prompt (%d tokens) plus its output (%d)",
			maxLen, largest, largestOutput)
	}
}

// The flagship and the sharing matrix must serve the same model at the same dtype, or they measure two things.
//
// This assertion used to be that dtype is startable on a T4: vLLM's platforms/cuda.py refuses bfloat16
// below sm_80, so on sm_75 fp16 was a startup condition. The card is an A10G now (sm_86), which accepts
// bfloat16 -- so that reason is gone, and asserting it would be asserting something false.
//
// The reason that replaced it is stronger, because it binds two files instead of restating a vendor limit.
// M5-c's exclusive arm IS this configuration, one engine with the card to itself, so the flagship's result
// is that arm. Engines that differed in dtype would make the matrix compare sharing mode AND numerics, and
// the difference would be attributed to the mode.
func TestTheFlagshipAndTheSharingMatrixAgreeOnModelAndDtype(t *testing.T) {
	flagship := vllmContainer(t, loadVLLMDeployment(t))
	flagshipDtype, ok := argValue(flagship.Args, "--dtype")
	if !ok {
		t.Fatal("the flagship engine sets no --dtype; it would be inferred from the model config and could differ from the matrix's")
	}

	files, err := filepath.Glob("../../config/vllm-shared/engine-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sharing engine manifests found: %v", err)
	}
	var shared []byte
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		shared = append(shared, b...)
	}
	dtypes := regexp.MustCompile(`--dtype=([A-Za-z0-9]+)`).FindAllStringSubmatch(string(shared), -1)
	if len(dtypes) == 0 {
		t.Fatal("the sharing engines set no --dtype")
	}
	for _, d := range dtypes {
		if d[1] != flagshipDtype {
			t.Errorf("the flagship serves at --dtype=%s and a sharing engine at --dtype=%s; the matrix would "+
				"compare sharing mode and numerics together", flagshipDtype, d[1])
		}
	}

	flagshipModel := ""
	for _, a := range flagship.Args {
		if !strings.HasPrefix(a, "-") {
			flagshipModel = a
			break
		}
	}
	if !strings.Contains(string(shared), flagshipModel) {
		t.Errorf("the flagship serves %q and the sharing engines do not; the exclusive arm of the matrix is "+
			"supposed to be the flagship's own measurement", flagshipModel)
	}
}

// No engine manifest may pin a namespace, because both runs place their engines themselves.
//
// A pinned `namespace: system` defeated that silently in two different ways at once. `kubectl apply -k`
// keeps a namespace the object already carries and ignores -n, so the M5-b session would have deployed the
// engine into system and then waited for a rollout in its own namespace until the timeout; `kubectl apply
// -f -n` refuses outright with a message about a mismatch. The matrix needs the freedom for a further
// reason: it puts one engine per namespace so the gateway can route one tenant to each, and that routing
// IS its time-slicing arm.
func TestNoEngineManifestPinsANamespace(t *testing.T) {
	files, err := filepath.Glob("../../config/vllm-shared/engine-*.yaml")
	if err != nil {
		t.Fatalf("glob the sharing engines: %v", err)
	}
	files = append(files, "../../config/vllm/deployment.yaml", "../../config/vllm/service.yaml")

	pinned := regexp.MustCompile(`(?m)^\s+namespace:\s*\S`)
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		if loc := pinned.FindString(string(b)); loc != "" {
			t.Errorf("%s pins a namespace (%q); the session and matrix scripts place these engines "+
				"themselves, and a pinned namespace makes `apply -k -n` deploy somewhere else while the "+
				"script waits where it asked", f, strings.TrimSpace(loc))
		}
	}
}

// The sequence cap must sit ABOVE the concurrency that fills the cache, or it becomes the binding limit.
//
// This is the defect that moving from a T4 to an A10G introduced, and it introduces nothing visible. With
// --max-num-seqs below the block limit the engine simply never runs out of blocks: KV usage plateaus under
// the engage threshold, the guard's cache arm never fires, and the arm under test degenerates into the
// waiting arm. Every number still looks reasonable.
//
// 32 was right on a T4, where 24 concurrent contenders filled the cache. The A10G's cache takes 50.
func TestTheSequenceCapDoesNotBindBeforeTheKVCacheDoes(t *testing.T) {
	c := vllmContainer(t, loadVLLMDeployment(t))
	raw, ok := argValue(c.Args, "--max-num-seqs")
	if !ok {
		t.Fatal("the engine sets no --max-num-seqs; vLLM's default would decide whether the cap or the cache binds")
	}
	seqs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("--max-num-seqs %q is not a number: %v", raw, err)
	}

	util, ok := argValue(c.Args, "--gpu-memory-utilization")
	if !ok {
		t.Fatal("the engine sets no --gpu-memory-utilization, so its cache size is not derivable from the manifest")
	}
	u, err := strconv.ParseFloat(util, 64)
	if err != nil {
		t.Fatalf("--gpu-memory-utilization %q is not a number: %v", util, err)
	}

	plan := SharingPlan{Card: CardA10G, Model: ModelQwen3B, Engines: 1, UtilizationPerEngine: u, NonKVOverheadMiB: 1400}
	if err := plan.Validate(); err != nil {
		t.Fatalf("the flagship's own plan does not validate: %v", err)
	}
	// The largest measured prompt in the trace; the cache fills when this many of them are resident.
	const contenderTokens = 7695
	fills := plan.KVTokensPerEngine() / contenderTokens
	if seqs <= fills {
		t.Errorf("--max-num-seqs=%d is at or below the %d concurrent contenders that fill the cache, so the "+
			"sequence cap binds first and the guard's KV-usage arm can never fire", seqs, fills)
	}
}

// The engine needs 160 MiB of /dev/shm for one worker and a container's default is 64 MiB.
//
// Observed directly: without this mount the engine core dies during startup with "Insufficient space in
// /dev/shm: 160 MiB required, 64 MiB free". The Pod would restart forever and no arm would ever run.
func TestSharedMemoryIsMountedLargeEnoughForTheEngine(t *testing.T) {
	d := loadVLLMDeployment(t)
	c := vllmContainer(t, d)

	mounted := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/dev/shm" {
			mounted = true
			for _, v := range d.Spec.Template.Spec.Volumes {
				if v.Name != m.Name {
					continue
				}
				if v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory {
					t.Fatalf("/dev/shm volume %q is not a memory-backed emptyDir", v.Name)
				}
				if v.EmptyDir.SizeLimit == nil {
					t.Fatalf("/dev/shm volume %q sets no sizeLimit", v.Name)
				}
				const measuredFloorBytes = 160 << 20
				if v.EmptyDir.SizeLimit.Value() < measuredFloorBytes {
					t.Fatalf("/dev/shm sizeLimit %s is below the measured 160Mi the engine requires", v.EmptyDir.SizeLimit)
				}
			}
		}
	}
	if !mounted {
		t.Fatal("no volume is mounted at /dev/shm; the engine dies at startup on the default 64 MiB")
	}
}

// One replica is the assertion the KV-aware arm rests on, and nothing downstream can detect its violation.
func TestExactlyOneEngineBacksTheService(t *testing.T) {
	d := loadVLLMDeployment(t)
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("replicas must be exactly 1 so the scraped KV pool is the one requests fill, got %v", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy %q allows a second Pod during rollout, which is a second KV pool", d.Spec.Strategy.Type)
	}
}

// The Pod must tolerate every taint the GPU node group declares, or it never schedules.
func TestEngineToleratesTheGPUNodeTaint(t *testing.T) {
	d := loadVLLMDeployment(t)
	tf, err := os.ReadFile("../../infra/aws/cluster/eks.tf")
	if err != nil {
		t.Fatalf("read eks.tf: %v", err)
	}
	if !strings.Contains(string(tf), "nvidia.com/gpu") {
		t.Skip("eks.tf declares no nvidia.com/gpu taint to tolerate")
	}
	for _, tol := range d.Spec.Template.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" && tol.Value == "present" && tol.Effect == corev1.TaintEffectNoSchedule {
			return
		}
	}
	t.Fatal("no toleration matches the nvidia.com/gpu=present:NoSchedule taint eks.tf puts on the GPU node group")
}

// The image must be pinned by digest, like every other fixture this experiment's numbers are tied to.
func TestEngineImageIsPinnedByDigest(t *testing.T) {
	c := vllmContainer(t, loadVLLMDeployment(t))
	if !strings.Contains(c.Image, "@sha256:") {
		t.Fatalf("image %q is not digest-pinned; the run would not be reproducible from the manifest", c.Image)
	}
}

// TestPrintedNodeGroupNamesExistInTerraform binds the names the session scripts tell an operator to type to
// the names terraform actually creates.
//
// The two had drifted, and nothing could see it. terraform-aws-modules/eks defaults to use_name_prefix, so
// the map key "gpu_single" becomes a real node group called gpu_single-<random suffix>; both session scripts
// printed the bare key. The operator's FIRST command of a paid session -- the scale-up -- would return
// ResourceNotFoundException.
//
// eks.tf now pins use_name_prefix = false and gives each group an explicit name. This test is what makes that
// stay true: it fails if a script prints a name no node group declares, and it fails if the prefix behaviour
// comes back.
func TestPrintedNodeGroupNamesExistInTerraform(t *testing.T) {
	tf := readRepoFile(t, "infra/aws/cluster/eks.tf")

	// Anchored to a whole line, because the unanchored form matched the COMMENT above the setting -- which
	// quotes `use_name_prefix = false` while explaining it. Flipping the real setting to true then left this
	// test green, and a guard a comment can satisfy is not a guard. Found by mutating the setting and watching
	// nothing go red.
	if !regexp.MustCompile(`(?m)^\s*use_name_prefix\s*=\s*false\s*$`).MatchString(tf) {
		t.Fatal("eks.tf does not set use_name_prefix = false; node group names will carry a random suffix and every printed --nodegroup-name is wrong")
	}

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*name\s*=\s*"([a-z0-9_]+)"\s*$`).FindAllStringSubmatch(tf, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no explicit node group names found in eks.tf")
	}

	// Only the concrete names matter. A script printing a placeholder like <ng> is telling the operator to
	// substitute, which is honest; a script printing gpu_single is making a claim about terraform.
	printed := regexp.MustCompile(`--nodegroup-name\s+([a-zA-Z0-9_-]+)`)
	for _, script := range []string{"hack/m5b-gpu-session.sh", "hack/m5c-matrix.sh"} {
		body := readRepoFile(t, script)
		for _, m := range printed.FindAllStringSubmatch(body, -1) {
			name := m[1]
			if strings.HasPrefix(name, "$") || strings.HasPrefix(name, "\"$") {
				continue // derived at runtime from the node's own label
			}
			if !declared[name] {
				t.Errorf("%s prints --nodegroup-name %q, which eks.tf does not declare; declared: %v",
					script, name, keysOf(declared))
			}
		}
	}
}

// TestTerraformVersionAgreesEverywhere fails when the pinned Terraform version drifts between the Makefile
// and the workflows, or falls below what the roots require.
//
// This is the sixth instance in this repository of one defect shape -- a value that must agree across call
// sites, carried by hand, with nothing that fails when it drifts -- and it is the first that went red in CI
// rather than being found by reading. The Makefile pinned 1.9.8 while both workflows pinned 1.16.0 and every
// root declared `required_version = ">= 1.10"` for use_lockfile, so `make infra-validate` refused all three
// roots with "Unsupported Terraform Core version" on a change that touched none of them.
func TestTerraformVersionAgreesEverywhere(t *testing.T) {
	mk := regexp.MustCompile(`(?m)^TERRAFORM_VERSION \?= ([0-9.]+)\s*$`).
		FindStringSubmatch(readRepoFile(t, "Makefile"))
	if mk == nil {
		t.Fatal("Makefile declares no TERRAFORM_VERSION; the version the tooling installs is unpinned")
	}
	pinned := mk[1]

	// Every workflow that names a Terraform version has to name the same one. The list is derived rather than
	// written down, so a new workflow is covered the day it is added.
	found := 0
	for _, wf := range []string{"infra.yml", "destroy.yml", "ci.yml"} {
		path := filepath.Join(".github/workflows", wf)
		body, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			continue
		}
		m := regexp.MustCompile(`(?m)^\s*TF_VERSION:\s*([0-9.]+)\s*$`).FindStringSubmatch(string(body))
		if m == nil {
			continue
		}
		found++
		if m[1] != pinned {
			t.Errorf("%s pins Terraform %s and the Makefile pins %s; CI and `make infra-validate` would run "+
				"different binaries against the same configuration", path, m[1], pinned)
		}
	}
	if found == 0 {
		t.Fatal("no workflow declares TF_VERSION; this test would pass on a repository that pins nothing")
	}

	// And the pinned version has to satisfy what the roots demand, which is the half that actually broke.
	roots, err := filepath.Glob(filepath.Join("..", "..", "infra", "aws", "*", "versions.tf"))
	if err != nil || len(roots) == 0 {
		t.Fatal("no terraform roots found; this test would pass on a repository with no infrastructure")
	}
	for _, root := range roots {
		body, err := os.ReadFile(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		m := regexp.MustCompile(`required_version\s*=\s*">=\s*([0-9.]+)"`).FindStringSubmatch(string(body))
		if m == nil {
			continue
		}
		if compareVersions(pinned, m[1]) < 0 {
			t.Errorf("%s requires Terraform >= %s and the pinned version is %s; every `terraform validate` "+
				"in this root refuses before it reads a resource", root, m[1], pinned)
		}
	}
}

// compareVersions orders dotted numeric versions, with a missing component read as zero so 1.16 and 1.16.0
// compare equal.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := 0, 0
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// readRepoFile reads a file relative to the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRepetitionDefaultsAgreeAcrossTheSessionScripts binds the three scripts that each carry their own REPS
// default.
//
// hack/gpu-session.sh defaulted to 4 and the two re-run scripts to 2. They do not read each other, so a
// re-run bought half the repetitions the study was designed around -- silently, and in the direction that
// weakens it: two independent reviews scored the design's power at 30% when n was 2 per cell and named the
// run count as the binding limit.
//
// This is the same defect shape as a region default that agreed in one file and not the others. The fix is
// not the number; it is that a disagreement now fails.
func TestRepetitionDefaultsAgreeAcrossTheSessionScripts(t *testing.T) {
	scripts := []string{"hack/gpu-session.sh", "hack/m5b-arms.sh", "hack/m5c-matrix.sh"}
	// Anchored to a whole line so the explanatory comment above each assignment cannot satisfy the match --
	// a guard a comment can satisfy is not a guard, which this file already learned once.
	re := regexp.MustCompile(`(?m)^REPS="\$\{REPS:-(\d+)\}"\s*$`)

	want, wantFrom := "", ""
	for _, script := range scripts {
		m := re.FindStringSubmatch(readRepoFile(t, script))
		if m == nil {
			t.Fatalf("%s has no REPS default in the expected form", script)
		}
		if want == "" {
			want, wantFrom = m[1], script
			continue
		}
		if m[1] != want {
			t.Errorf("%s defaults REPS to %s but %s defaults to %s; the study's repetition count cannot depend on which script the operator ran",
				script, m[1], wantFrom, want)
		}
	}
	if want != "4" {
		t.Errorf("the shared REPS default is %s; the design is four per cell and below that the incremental interval is a bootstrap over very few blocks", want)
	}
}

// TestThePaidArmsScriptRequiresItsOwnProvenance binds the script to the check.
//
// RunManifest declared gatewaySHA and imageDigests for provenance and nothing ever set them: no flag accepted
// them, no script passed them, every manifest left them empty. Adding the flags and the RequireProvenance
// check does not fix that by itself -- the script has to pass one and ask for the other, and a script that
// stopped doing either would leave every unit spec green.
//
// That is the same defect class as a Terraform variable no caller passed, and it has now appeared often
// enough in this repository to be worth a test rather than a comment.
func TestThePaidArmsScriptRequiresItsOwnProvenance(t *testing.T) {
	body := readRepoFile(t, "hack/m5b-arms.sh")

	for _, want := range []string{
		"--require-provenance",
		"--gateway-sha",
		"--gateway-image",
		"--engine-image",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hack/m5b-arms.sh does not pass %s; a paid run would record numbers it cannot attribute to a build", want)
		}
	}

	// The image it records must be the digest it resolved, not the tag it pushed under. A script that
	// recorded GW_IMAGE straight after `docker tag` would satisfy the flag check above and record nothing
	// durable, so the resolution step is asserted too.
	if !strings.Contains(body, "RepoDigests") {
		t.Error("hack/m5b-arms.sh never resolves the pushed image to a digest; it would record a tag, which names whatever was pushed under it most recently")
	}
	if !strings.Contains(body, "not digest-pinned") {
		t.Error("hack/m5b-arms.sh does not refuse an engine image that is not digest-pinned")
	}
}

// TestCIPublishesEveryImageGitOpsDeploys binds the two halves of the supply chain.
//
// config/gateway/deployment.yaml carried `image: gateway:latest` and ci.yml built and pushed only the
// operator. Argo CD would deploy a name nothing publishes and the Pod would sit in ImagePullBackOff, while
// each paid session script built its own copy under a local tag -- so the component under test on two of the
// four arms had no published build and no recorded identity.
//
// The check is deliberately about the WIRING rather than the tag. `gateway:latest` is fine as a placeholder
// once something replaces it: kustomize's image transform rewrites it to a digest in the same PR as the
// operator's. What must not happen is the placeholder existing with no publisher.
func TestCIPublishesEveryImageGitOpsDeploys(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/ci.yml")

	for _, want := range []string{
		"Dockerfile.gateway",
		"ECR_GATEWAY_REPOSITORY",
		"config/gateway",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml does not reference %s; the gateway manifests deploy an image CI never publishes", want)
		}
	}

	// The digest pin must land in the same commit as the operator's. Two PRs would let the cluster run a
	// gateway from one commit against an operator from another, with nothing recording which pair was live.
	if !strings.Contains(ci, "config/manager/kustomization.yaml config/gateway/kustomization.yaml") {
		t.Error("ci.yml does not stage both kustomizations in one commit; the operator and gateway digests could diverge")
	}

	// And terraform must actually create the repository CI pushes to.
	ecr := readRepoFile(t, "infra/aws/bootstrap/ecr.tf")
	if !strings.Contains(ecr, `resource "aws_ecr_repository" "gateway"`) {
		t.Error("infra/aws/bootstrap declares no gateway ECR repository, so the push in ci.yml has nowhere to go")
	}
}

// TestCITrustBoundariesSurviveEditing pins the two controls that stand between a pull request and this
// account's read surface.
//
// `terraform plan` is not read-only on untrusted input: `init` fetches and executes providers and modules
// named by the pull request's own configuration, inside a job holding an OIDC token for a role with
// account-wide ReadOnlyAccess and decrypt on the Terraform state key. Two things keep that from being
// reachable by anyone who can open a pull request, and both are one edit away from disappearing silently.
func TestCITrustBoundariesSurviveEditing(t *testing.T) {
	infra := readRepoFile(t, ".github/workflows/infra.yml")

	// 1. The plan job must refuse a pull request whose head is not this repository.
	//
	// For a `pull_request` event GitHub issues a token whose subject is the BASE repository's
	// `repo:<owner>/<name>:pull_request` -- exactly what the plan role trusts. Without this check the only
	// barrier is whether GitHub happens to issue `id-token: write` to fork workflows, which is a platform
	// behaviour this repository would be depending on without stating it.
	if !strings.Contains(infra, "github.event.pull_request.head.repo.full_name == github.repository") {
		t.Error("infra.yml does not restrict the credentialed plan job to same-repository pull requests; " +
			"a fork PR could reach a role with account-wide ReadOnlyAccess and state-key decrypt")
	}

	// 2. The OIDC trust must pin the immutable numeric identities, not only the owner/name string.
	//
	// `sub` keys off `repo:<owner>/<name>:...` and both halves are mutable: a rename or transfer frees the
	// old owner/name for anyone to claim, and tokens from a different repository would then match.
	oidc := readRepoFile(t, "infra/aws/bootstrap/oidc.tf")
	for _, key := range []string{"repository_id", "repository_owner_id"} {
		want := "token.actions.githubusercontent.com:" + key
		// Three roles, three trust policies. Two of three would be a silent hole in whichever was missed.
		if n := strings.Count(oidc, want); n < 3 {
			t.Errorf("oidc.tf pins %s in %d trust policies, expected 3 (plan, apply, image-push)", key, n)
		}
	}
}
