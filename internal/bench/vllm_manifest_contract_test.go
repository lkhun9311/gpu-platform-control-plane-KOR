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

// TestTheDeadlineIsSizedFromTheRepetitionCountRatherThanALiteral pins the two call sites together.
//
// hack/m5b-gpu-session.sh re-arms the deadline for the run it just measured, and to do that it needs the
// number of replays. It carried the literal 16 -- four arms times the default four repetitions -- which is
// correct only while that default is four and nobody sets REPS. At REPS=2 it asked for twice the deadline
// the run needed; raised, it would have armed a deadline too short for the run it was sizing, and a session
// cut by its own backstop is the failure the backstop exists to prevent.
//
// This is the same defect class as the Region repeated across five files, the quota that had to agree with
// the offerings, and the Go version in the Makefile: a value carried by hand across two call sites with
// nothing that fails when they disagree. It has now appeared often enough here to be worth a test each time
// a new instance is closed.
func TestTheDeadlineIsSizedFromTheRepetitionCountRatherThanALiteral(t *testing.T) {
	session := readRepoFile(t, "hack/m5b-gpu-session.sh")

	// The arms script computes `4 * REPS`. The session script must reach the same product, and the only way
	// it can be right for every REPS is by reading REPS rather than a number.
	if !strings.Contains(session, "arms_reps=") {
		t.Fatal("hack/m5b-gpu-session.sh does not derive a repetition count; the deadline it arms cannot track REPS")
	}
	if !strings.Contains(session, "replays = 4 * $arms_reps") {
		t.Error("hack/m5b-gpu-session.sh does not size the deadline as four arms times the repetition count")
	}
	// Anchored to the arithmetic line so a comment mentioning 16 does not satisfy or trip this.
	if regexp.MustCompile(`replays?\s*=\s*16\b|\b16 \* \(replay`).MatchString(session) {
		t.Error("hack/m5b-gpu-session.sh still hardcodes 16 replays; set REPS and the deadline stops matching the run")
	}

	// The completions target is the other half of the replay length, and it was a literal 500 in both
	// scripts. Lowering it in the harness to buy a shorter run -- the cheaper of the two ways to shorten a
	// session, and the one a review recommended over cutting repetitions -- would otherwise leave the
	// session sizing its deadline for the run it used to be.
	if !strings.Contains(session, "arms_target=") {
		t.Fatal("hack/m5b-gpu-session.sh does not derive the per-replay completion target; the deadline it arms cannot track a change to it")
	}
	if regexp.MustCompile(`replay\s*=\s*500\s*/`).MatchString(session) {
		t.Error("hack/m5b-gpu-session.sh still hardcodes 500 completions; change it in the harness and the deadline stops matching the run")
	}
}

// TestTheDeadlineCeilingAdmitsTheShippedDesign keeps a cost guard from refusing the thing it guards.
//
// MAX_TTL_MINUTES caps how long a session may buy. At four repetitions and the rate an A10G is expected to
// give, the design asks for 358 minutes -- and the ceiling was 360, so a card measuring a few percent slow
// would have been refused at the re-arming step with the engine already warm and paid for. That is the same
// defect as hack/m5c-matrix.sh budgeting 456 minutes against its own 240-minute deadline: a guard that
// cannot pass the configuration the repository ships.
func TestTheDeadlineCeilingAdmitsTheShippedDesign(t *testing.T) {
	session := readRepoFile(t, "hack/m5b-gpu-session.sh")
	m := regexp.MustCompile(`max_min="\$\{MAX_TTL_MINUTES:-(\d+)\}"`).FindStringSubmatch(session)
	if m == nil {
		t.Fatal("hack/m5b-gpu-session.sh has no MAX_TTL_MINUTES default in the expected form")
	}
	ceiling, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("MAX_TTL_MINUTES default %q is not a number", m[1])
	}
	// Checked at a rate BELOW the expectation, not at it.
	//
	// The first version of this test used 1.142/s, the estimate itself, and passed against the old ceiling
	// of 360 -- the design asks for 358 there, so two minutes of slack was enough to satisfy it. That made
	// the test agree with the defect it was written for: the estimate is an estimate, the rate is not known
	// until the probe runs on the actual card, and a ceiling with two minutes of room is a ceiling that
	// refuses as soon as the card is a few percent slower than guessed.
	//
	// So the requirement is that the ceiling still admits the design at 1.0/s, about twelve percent below
	// the estimate. Below that the card is slow enough that stopping to reconsider is the right answer,
	// which is what the ceiling is for.
	const slowRate = 1.0
	replay := 500.0 / (slowRate / 2)
	armsMin := int((16*(replay+180))/60) + 1
	needed := armsMin*12/10 + 20
	if ceiling < needed {
		t.Errorf("MAX_TTL_MINUTES defaults to %d but the shipped design needs %d at %.2f/s, %d%% below the expected rate; a card that measures a little slow would be refused after it is warm and paid for",
			ceiling, needed, slowRate, 12)
	}
}

// TestTheDigestIsInTheTransformNotTheDeployment keeps two consumers of one manifest from fighting.
//
// config/manager/manager.yaml and config/gateway/deployment.yaml are read by two paths that need different
// images. `make deploy` runs `kustomize edit set image controller=${IMG}`, so the kind path builds locally,
// side-loads, and overrides the pin; Argo CD builds the same directories untouched and must get the
// published digest.
//
// Writing the ECR reference into the Deployment removed the name the transform matches on. The override
// silently stopped applying, and every e2e run failed on a Pod that could not pull from a registry kind has
// no credentials for -- a change that looked like pinning an image and was actually unpinning an override.
func TestTheDigestIsInTheTransformNotTheDeployment(t *testing.T) {
	for _, tc := range []struct{ manifest, kustomization, placeholder, name string }{
		{"config/manager/manager.yaml", "config/manager/kustomization.yaml", "image: controller:latest", "controller"},
		{"config/gateway/deployment.yaml", "config/gateway/kustomization.yaml", "image: gateway:latest", "gateway"},
	} {
		manifest := readRepoFile(t, tc.manifest)
		if !strings.Contains(manifest, tc.placeholder) {
			t.Errorf("%s does not carry %q; `kustomize edit set image %s=...` has nothing to match and the local override stops applying",
				tc.manifest, tc.placeholder, tc.name)
		}
		if strings.Contains(manifest, "dkr.ecr.") {
			t.Errorf("%s names a registry directly; the digest belongs in %s where the image transform can be overridden",
				tc.manifest, tc.kustomization)
		}

		kust := readRepoFile(t, tc.kustomization)
		if !strings.Contains(kust, "images:") {
			t.Errorf("%s has no image transform, so Argo CD would deploy the %s placeholder, which no registry serves",
				tc.kustomization, tc.placeholder)
		}
		if !strings.Contains(kust, "digest: sha256:") {
			t.Errorf("%s pins by tag rather than digest; a tag names whatever was pushed under it most recently",
				tc.kustomization)
		}
		if !strings.Contains(kust, "name: "+tc.name) {
			t.Errorf("%s does not transform the image named %q, so the pin would not apply to the Deployment",
				tc.kustomization, tc.name)
		}
	}
}

// TestCRDApplicationsDoNotPrune keeps a rename from deleting a cluster's data.
//
// Deleting a CustomResourceDefinition is not a schema change: the API server removes every instance of it
// in the same operation. An Argo Application that owns CRDs with prune enabled will therefore delete every
// Workload, LocalQueue, ClusterQueue and ResourceFlavor -- or every InferenceDeployment, GPUQuotaPolicy and
// NodeHealth -- when a file is renamed, a path stops rendering one of them, or the child is removed from
// the root Application. A cascade finalizer on the Application makes that last one a two-stage version of
// the same thing.
//
// selfHeal is left on deliberately. Re-creating a definition someone deleted by hand is safe; removing one
// because a file moved is not, and that is not a decision to make unattended.
func TestCRDApplicationsDoNotPrune(t *testing.T) {
	for _, app := range []string{"config/argocd/crds.yaml", "config/argocd/kueue-crds.yaml"} {
		body := readRepoFile(t, app)
		if strings.Contains(body, "prune: true") {
			t.Errorf("%s prunes; a renamed file would delete the CRDs and every custom resource stored under them", app)
		}
		if !strings.Contains(body, "prune: false") {
			t.Errorf("%s does not state prune: false, so the default could change under it", app)
		}
		if strings.Contains(body, "resources-finalizer.argocd.argoproj.io") {
			t.Errorf("%s carries a cascade finalizer; deleting the Application would take the CRDs and their instances with it", app)
		}
	}

	// And the CRDs the operator cannot start without must be owned by config/, not by a test fixture.
	// They lived under test/crd/kueue, which made tidying a fixture a production change.
	kueue := readRepoFile(t, "config/argocd/kueue-crds.yaml")
	if !strings.Contains(kueue, "path: config/kueue-crds") {
		t.Error("the Kueue CRD Application does not source config/kueue-crds; a runtime dependency should not be served out of test/")
	}
}

// TestTheKueueCRDDirectoryHoldsOnlyCRDs keeps a helpful file from breaking three consumers.
//
// config/kueue-crds is read as a directory of Kubernetes objects by three things at once: Argo CD renders
// the path, the e2e suite runs `kubectl apply --server-side -f` on it, and envtest loads it through
// CRDDirectoryPaths. All three take every .yaml in the directory and send it somewhere that expects an
// object with a kind.
//
// A kustomization.yaml was added here to describe the set, which is exactly the sort of thing that looks
// like documentation and is not. The four CRDs applied and then the kustomization failed with
// "error: resource mapping", and the e2e suite went red on a change that only moved files. Prose belongs in
// a .md, which kubectl's directory expansion ignores -- verified, along with the fact that a stray .yaml
// does not get ignored.
func TestTheKueueCRDDirectoryHoldsOnlyCRDs(t *testing.T) {
	const dir = "../../config/kueue-crds"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", dir, err)
	}

	seen := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".json") {
			continue // kubectl, Argo and envtest all skip these; prose is safe here
		}
		seen++
		body := readRepoFile(t, "config/kueue-crds/"+name)
		if !strings.Contains(body, "kind: CustomResourceDefinition") {
			t.Errorf("config/kueue-crds/%s is not a CustomResourceDefinition; every consumer of this directory sends each .yaml to an API that expects one", name)
		}
	}
	if seen != 4 {
		t.Errorf("config/kueue-crds holds %d manifests, want the 4 Kueue definitions the operator's field index requires", seen)
	}
}

// TestTheDevicePluginDoesNotNameDeviceIndices keeps a manifest from depending on which card it lands on.
//
// The daemonset set NVIDIA_VISIBLE_DEVICES to "0,1". The queuelab node is a g4dn.12xlarge with four cards,
// so both indices exist there; g5.xlarge has one A10G, so index 1 does not, and the NVIDIA container
// runtime refuses to create the container at all:
//
//	failed to generate CDI spec for mode "auto": failed to construct device spec generators:
//	failed to get device handle from index: Invalid Argument
//
// The plugin CrashLoopBackOff'd, nvidia.com/gpu was never advertised, and the paid session refused after the
// card had been billing for ten minutes -- with a message that said the device plugin was not working and
// could not say why.
//
// A device plugin advertises whatever the node has, so an index list is the wrong shape on every node, not
// just the ones where it happens to be short. This is the same defect as a Region or a node group name
// written into one file: a value that depends on the machine, carried by hand.
func TestTheDevicePluginDoesNotNameDeviceIndices(t *testing.T) {
	body := readRepoFile(t, "config/nvidia-device-plugin/daemonset.yaml")

	m := regexp.MustCompile(`(?m)^\s*-\s*name:\s*NVIDIA_VISIBLE_DEVICES\s*\n\s*value:\s*"([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("config/nvidia-device-plugin/daemonset.yaml does not set NVIDIA_VISIBLE_DEVICES in the expected form")
	}
	switch m[1] {
	case "all", "void":
		// all: advertise whatever the node has. void: no injection at all.
	default:
		t.Errorf("NVIDIA_VISIBLE_DEVICES is %q; naming devices ties this manifest to one instance type, and the runtime refuses to start the container on any node that has fewer", m[1])
	}
}

// TestGPUNodeGroupsHoldTheEngine keeps the node disk large enough for what has to land on it.
//
// EKS defaults to 20 GiB when disk_size is unset, and nothing set it. The first paid session reached the
// engine rollout and the node reported DiskPressure=True: kubelet evicted the vLLM Pod twice and left the
// third Pending, and the session refused with the card already billing.
//
// What must fit, measured rather than estimated -- the image pulled locally, the weights read from the
// Hugging Face API:
//
//	vllm/vllm-openai      21.7 GiB by docker history, 30.8 GiB by docker system df
//	Qwen2.5-3B weights     6.18 GiB
//	AMI, kubelet, containerd, DCGM, device plugin   roughly 5 GiB
//
// The image alone exceeds the default. This pins a floor rather than the exact number, so raising it stays
// easy and dropping it back to something that cannot hold the engine does not.
func TestGPUNodeGroupsHoldTheEngine(t *testing.T) {
	body := readRepoFile(t, "infra/aws/cluster/eks.tf")

	// volume_size on a block_device_mappings block, NOT disk_size.
	//
	// The first version of this test read disk_size, and passed while the cluster kept the AMI default of
	// 20 GiB: this module overrides disk_size to null whenever a custom launch template is used, which is
	// the default, so terraform reported no change and the setting never reached a node. A test that reads
	// the file rather than what the file does is exactly the shape it was written to catch.
	//
	// Counting the group blocks was also tried and dropped: `gpu = {` appears inside each group's labels,
	// so the pattern matched six times and failed against a correct file.
	const floorGiB = 60
	if regexp.MustCompile(`(?m)^\s*disk_size\s*=`).MatchString(body) {
		t.Error("eks.tf sets disk_size; with a custom launch template this module discards it, so the node keeps the AMI default. Use block_device_mappings.")
	}
	sizes := regexp.MustCompile(`(?m)^\s*volume_size\s*=\s*(\d+)`).FindAllStringSubmatch(body, -1)
	if len(sizes) < 3 {
		t.Fatalf("eks.tf sets volume_size %d times; each GPU group needs one or the node keeps the 20 GiB AMI default, which the vLLM image alone exceeds", len(sizes))
	}
	for _, m := range sizes {
		got, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("disk_size %q is not a number", m[1])
		}
		if got < floorGiB {
			t.Errorf("a node group sets disk_size = %d; the engine image measures 21.7-30.8 GiB and the weights another 6.18, so anything under %d evicts the Pod after the card is warm", got, floorGiB)
		}
	}
}
