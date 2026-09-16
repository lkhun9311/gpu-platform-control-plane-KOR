package queuelab

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// Every taint the GPU node group declares must be tolerated by the flavour a run is admitted under.
//
// This is the defect that would have killed the session most quietly. eks.tf taints the node
// nvidia.com/gpu=present:NoSchedule; nothing else in this repository tolerated it. Kueue checks a podset's
// tolerations against the FLAVOUR's declared taints rather than the node's real ones, so the Workload is
// admitted, the Job unsuspends, the ledger records Admitted -- and the scheduler then refuses the Pod. Every
// arm dies at a barrier with a desync.
//
// Neither existing gate could see it: the canary and the preflight both build their Pods through
// probePodFrom, which sets spec.nodeName and bypasses the scheduler, and NoSchedule is a scheduler
// predicate. Both would call the node healthy.
//
// The taints are read from the terraform rather than restated here, so the two cannot drift apart.
func TestTheFlavourToleratesEveryTaintTheGPUNodeGroupDeclares(t *testing.T) {
	tf := repoFile(t, "infra/aws/cluster/eks.tf")
	keys := regexp.MustCompile(`(?m)^\s*key\s*=\s*"([^"]+)"`).FindAllStringSubmatch(tf, -1)
	if len(keys) == 0 {
		t.Fatal("no taint keys found in eks.tf; this test is reading a shape that has moved and must be " +
			"rewritten rather than deleted")
	}
	flavor := labResourceFlavor(FixtureIdentity{RunID: "r1"}, StudyReclaim, "v")
	// A toleration must actually TOLERATE the taint, not merely mention its key. The first version of this
	// accepted any entry with a matching key, so operator, value and effect could all be wrong -- a
	// `nvidia.com/gpu=wrong:NoExecute` toleration would have passed while leaving every Pod Pending against
	// the node's `nvidia.com/gpu=present:NoSchedule`. A test that checks the name of the thing rather than
	// the thing is the failure this file exists to catch, committed inside the file.
	//
	// The rule is Kubernetes': Exists matches any value, Equal matches only its own, and an empty effect
	// matches every effect.
	tolerates := func(t corev1.Taint) bool {
		for _, tol := range flavor.Spec.Tolerations {
			if tol.Key != t.Key {
				continue
			}
			if tol.Effect != "" && tol.Effect != t.Effect {
				continue
			}
			switch tol.Operator {
			case corev1.TolerationOpExists:
				return true
			case corev1.TolerationOpEqual, "":
				if tol.Value == t.Value {
					return true
				}
			}
		}
		return false
	}
	// The effect and value are read from the terraform beside the key, so the toleration is checked against
	// the taint as declared rather than against its name alone.
	effects := regexp.MustCompile(`(?m)^\s*effect\s*=\s*"([^"]+)"`).FindAllStringSubmatch(tf, -1)
	values := regexp.MustCompile(`(?m)^\s*value\s*=\s*"([^"]+)"`).FindAllStringSubmatch(tf, -1)
	for i, m := range keys {
		key := m[1]
		// The lab's own ownership taint is keyed per run and is applied by the harness, not by terraform.
		if strings.HasPrefix(key, "queuelab.") {
			continue
		}
		taint := corev1.Taint{Key: key}
		if i < len(values) {
			taint.Value = values[i][1]
		}
		if i < len(effects) {
			// Terraform spells the effect NO_SCHEDULE; the API spells it NoSchedule.
			taint.Effect = corev1.TaintEffect(camelFromScreamingSnake(effects[i][1]))
		}
		if !tolerates(taint) {
			t.Fatalf("the GPU node group taints %s=%s:%s and the run's ResourceFlavor does not tolerate it, "+
				"so every trace Pod stays Pending while Kueue reports the Workload admitted",
				taint.Key, taint.Value, taint.Effect)
		}
	}
}

// The exporter must be configured to emit the labels the parser reads, and to collect often enough for the
// gate that judges it.
//
// Both defaults are against us and both are silent. dcgm-exporter's kubernetes mapping is OFF by default:
// mounting the kubelet's pod-resources socket is necessary and not sufficient, and without the mapping every
// sample arrives with no pod label -- which ParseDCGMUtilisation skips, so a healthy exporter reporting full
// utilisation yields zero attributable samples and the harness reports that the observer could not
// attribute. And the collection interval defaults to thirty seconds, while the exporter serves a CACHED
// snapshot in between, so the pod-to-device mapping in a served scrape can be far staler than the
// stale-label margin the exclusivity clause allows.
func TestTheExporterManifestEmitsWhatTheParserReads(t *testing.T) {
	ds := repoFile(t, "config/dcgm-exporter/daemonset.yaml")
	// The VALUE, not the name. Checking only that the variable appears accepts `value: "false"`, which
	// disables the very mapping this test exists to require -- the same name-not-thing mistake the
	// toleration check above was making.
	if !regexp.MustCompile(`name:\s*DCGM_EXPORTER_KUBERNETES\s*\n\s*value:\s*"true"`).MatchString(ds) {
		t.Fatal("the exporter does not enable the kubernetes mapping with value \"true\", so no sample will " +
			"carry a pod label and every device claim will fail attribution for a reason that is not about " +
			"the card")
	}
	// The exporter's device-to-Pod mapping is per node and it has to know which node it is on. NVIDIA's own
	// reference DaemonSet sets NODE_NAME from the downward API; this manifest omitted it, and a review
	// predicted empty Pod attribution on a real cluster -- indistinguishable downstream from a card nobody
	// used, which is the failure the mapping flag above exists to prevent.
	if !regexp.MustCompile(`name:\s*NODE_NAME\s*\n\s*valueFrom:\s*\n\s*fieldRef:\s*\n\s*fieldPath:\s*spec\.nodeName`).MatchString(ds) {
		t.Fatal("the exporter does not learn its own node name from the downward API, so its per-node " +
			"device-to-Pod mapping has nothing to key on and its samples may carry no pod label at all")
	}
	m := regexp.MustCompile(`-\s*"-c"\s*\n\s*-\s*"(\d+)"`).FindStringSubmatch(ds)
	if m == nil {
		t.Fatal("the exporter names no collection interval, so it takes the 30 s default while serving a " +
			"cached snapshot in between")
	}
	ms, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("collection interval %q is not a number", m[1])
	}
	if got := time.Duration(ms) * time.Millisecond; got >= maxObserverGap {
		t.Fatalf("the exporter collects every %s, which is not shorter than the %s gap a run may blink "+
			"through: a collection slower than the gap cannot cover the interval it is asked about",
			got, maxObserverGap)
	}
}

// The session script must address the exporter's container by a name the manifest actually uses.
//
// It hardcoded the DAEMONSET's name where the CONTAINER's belonged, matched nothing, and exited on the first
// command of the session. What makes that worth a test rather than a fix is how it was "verified": a
// stand-in DaemonSet was written with the container name the script expected, so the check confirmed the
// script agreed with a fixture built from the script. The manifest is the only thing worth checking against.
func TestTheSessionScriptAddressesTheExportersRealContainer(t *testing.T) {
	sh := repoFile(t, "hack/gpu-session.sh")
	ds := repoFile(t, "config/dcgm-exporter/daemonset.yaml")
	names := regexp.MustCompile(`(?m)^\s{8}-\s+name:\s+(\S+)`).FindAllStringSubmatch(ds, -1)
	if len(names) == 0 {
		t.Fatal("no container name found in the exporter manifest")
	}
	container := names[0][1]
	for _, hardcoded := range regexp.MustCompile(`@\.name==\\?"([^"\\]+)\\?"`).FindAllStringSubmatch(sh, -1) {
		if strings.HasPrefix(hardcoded[1], "$") {
			continue // derived from the manifest at run time, which is the point
		}
		if hardcoded[1] != container {
			t.Fatalf("the script selects container %q and the manifest declares %q, so it resolves nothing "+
				"and exits before the preflight", hardcoded[1], container)
		}
	}
	// exec replaces the shell and destroys the EXIT trap that tears the port-forward down, so the two must
	// not appear together.
	if regexp.MustCompile(`(?m)^exec\s`).MatchString(sh) && strings.Contains(sh, "trap ") {
		t.Fatal("the script installs an EXIT trap and then execs, which destroys it: the port-forward the " +
			"trap promises to clean up is either orphaned or dies with the runner")
	}
}

// The infrastructure must be able to run the axes the preregistration promises.
//
// The GPU node group was capped at one node while the preregistration promised a node comparison. The cap
// saved nothing -- desired_size is 0 either way, and nothing bills at zero -- so a number nobody had argued
// for made a preregistered axis unrunnable. The failure would not have been silent (`-compare -mode node`
// refuses a set in which nothing varies) but it would have been discovered on the rented node.
//
// This ties the promise to the capability. If the node axis is ever withdrawn from the preregistration, this
// test should be deleted with it rather than left asserting a requirement nothing has.
//
// Mutation that turns this red: put the cap back to 1.
func TestTheGPUNodeGroupCanRunTheAxesThePreregistrationPromises(t *testing.T) {
	prereg := repoFile(t, "hack/gpu-session-preregistration.md")
	if !strings.Contains(prereg, "-mode node") {
		t.Skip("the preregistration no longer promises a node axis, so nothing here is required")
	}
	tf := repoFile(t, "infra/aws/cluster/eks.tf")
	// The GPU group is the block carrying the NVIDIA AMI; read its cap rather than the first one in the file,
	// and read ONLY its block.
	//
	// This used to slice from "gpu = {" to end-of-file, which worked while the cap was a literal. When it
	// became max_size = var.gpu_max_nodes the numeric regex stopped matching inside the gpu block and
	// silently matched gpu_single's max_size = 1 further down -- so the test asserted about the wrong node
	// group while naming the right one. Bounded now.
	start := strings.Index(tf, "gpu = {")
	if start < 0 {
		t.Fatal("eks.tf declares no gpu node group")
	}
	gpu := tf[start:]
	if end := strings.Index(gpu, "gpu_single = {"); end > 0 {
		gpu = gpu[:end]
	}
	if found := strings.Contains(gpu, "AL2023_x86_64_NVIDIA"); !found {
		t.Fatal("the GPU node group is not the block this test thinks it is; it must be rewritten rather " +
			"than left matching the wrong sizes")
	}

	m := regexp.MustCompile(`max_size\s*=\s*(\S+)`).FindStringSubmatch(gpu)
	if m == nil {
		t.Fatal("the GPU node group declares no max_size")
	}
	raw := m[1]
	if raw == "var.gpu_max_nodes" {
		vars := repoFile(t, "infra/aws/cluster/variables.tf")
		d := regexp.MustCompile(`(?s)variable "gpu_max_nodes".*?default\s*=\s*(\d+)`).FindStringSubmatch(vars)
		if d == nil {
			t.Fatal("max_size is var.gpu_max_nodes but variables.tf declares no default for it")
		}
		raw = d[1]
	}
	max, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("max_size %q is neither a number nor var.gpu_max_nodes", m[1])
	}

	// A cap of one is allowed, but only as a RECORDED constraint rather than a silent regression.
	//
	// The preregistration promises a node axis and says plainly what a one-node session delivers instead --
	// the arm contrast, the dose axis and the device evidence -- with -compare -mode node refusing rather
	// than fabricating. What must never happen is the cap dropping to one because somebody edited a number.
	//
	// So at one, two things must hold: the cap is carried by the variable that exists to record the decision,
	// and the quota that forces it is actually read. Two g4dn.12xlarge is 96 vCPU against the 52 granted on
	// 2026-08-26, so today the cap is arithmetic rather than preference.
	if max < 2 {
		if m[1] != "var.gpu_max_nodes" {
			t.Fatalf("the GPU node group caps at %d node(s) as a literal. One is acceptable only as a "+
				"quota-gated decision carried by var.gpu_max_nodes, because -compare -mode node refuses a "+
				"set in which the node does not vary and a session would discover that on rented hardware", max)
		}
		placement := repoFile(t, "infra/aws/cluster/placement.tf")
		if !strings.Contains(placement, "L-DB2E81BA") {
			t.Fatal("the GPU node group caps at one node and nothing reads the On-Demand G/VT quota, so the " +
				"cap is a preference rather than a constraint. Either restore the axis, or keep the quota " +
				"precondition that explains why it cannot run")
		}
		t.Logf("node axis not delivered at cap %d: gated on the G/VT quota, see placement.tf", max)
	}

	// And the cost control must still be there: a group that defaults to running is the discipline this
	// repository is built around, lost.
	if !regexp.MustCompile(`desired_size\s*=\s*0`).MatchString(gpu) {
		t.Fatal("the GPU node group does not default to zero nodes, so it bills whether or not anybody is " +
			"running an experiment")
	}
}

// The session script's device count must be the one the harness derives.
//
// hack/gpu-session.sh sizes the surplus occupier from a REQUIRED constant. The harness derives the same
// number from the run's own fixtures and trace, and qualify.go's own comment names a hard-coded copy of it
// as "the constant this derivation exists to avoid". Two truths, and nothing kept them in step: a drifted
// script would hold the wrong number of cards and the run would refuse -- after the canary and the preflight
// had spent their minutes on a rented node.
//
// Mutation that turns this red: change REQUIRED in the script without changing the fixtures.
func TestTheSessionScriptHoldsTheDeviceCountTheHarnessDerives(t *testing.T) {
	sh := repoFile(t, "hack/gpu-session.sh")
	m := regexp.MustCompile(`REQUIRED="\$\{REQUIRED:-(\d+)\}"`).FindStringSubmatch(sh)
	if m == nil {
		t.Fatal("the session script names no REQUIRED default; this test is checking a shape that has moved")
	}
	scripted, cerr := strconv.Atoi(m[1])
	if cerr != nil {
		t.Fatalf("REQUIRED %q is not a number: %v", m[1], cerr)
	}
	// The same two bounds qualify() uses: the nominal quota summed over the study's ClusterQueues, and the
	// largest single row. Derived here from the fixtures rather than restated.
	fs, err := BuildFixtures(StudyReclaim, "Never", FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("build the study's fixtures: %v", err)
	}
	var sum int64
	for _, cq := range fs.ClusterQueue {
		if cq == nil {
			continue
		}
		for _, rg := range cq.Spec.ResourceGroups {
			for _, fq := range rg.Flavors {
				for _, r := range fq.Resources {
					if r.Name == "nvidia.com/gpu" {
						sum += r.NominalQuota.Value()
					}
				}
			}
		}
	}
	if sum == 0 {
		t.Fatal("no nvidia.com/gpu quota found in the study's fixtures; this test would pass vacuously")
	}
	if int64(scripted) != sum {
		t.Fatalf("the session script holds the surplus down to %d devices and the study's fixtures need %d; "+
			"the occupier would hold the wrong number of cards and the run would refuse on rented hardware",
			scripted, sum)
	}
}

// Growing the study's repetitions must not break the orderings its comparisons depend on.
//
// n=2 per cell was the kind allocation and the weakest thing about the design: a cell's spread is one
// number and cannot be told from its own noise. Raising it is the cheapest axis this study can buy on
// rented hardware -- about three minutes a run -- so the session script repeats a block rather than
// carrying a hand-written list per size.
//
// That only works if the block composes. Every comparison the script prints requires ITS OWN factor to
// alternate in time; a blocked sequence makes the harness print CONFOUNDED and the study cannot use its own
// records. The subsets matter: the dose comparison reads the grace runs on the FIRST worker only, so a
// check that pooled both workers' grace runs would report a break that no published comparison has. That
// mistake was made while writing this, which is why the subsets here are the globs the script prints
// rather than the factors in the abstract.
func TestGrowingTheRepetitionsKeepsEveryComparisonInterleaved(t *testing.T) {
	sh := repoFile(t, "hack/gpu-session.sh")
	if !strings.Contains(sh, `REPS="${REPS:-`) {
		t.Fatal("the session script no longer parameterises repetitions; this test is checking a shape " +
			"that has moved and must be rewritten rather than deleted")
	}
	type run struct{ dose, arm, id, node string }
	// PARSED from the script, not transcribed into Go. A hand-copied block is a fixture written to fit the
	// code: edit the shell and this test keeps checking the copy. That is the failure this repository has
	// made repeatedly, and it was made here first -- the block below was a Go transcription until a mutation
	// of the copy proved the copy was all that was being checked.
	blockFor := func(twoWorkers bool) []run {
		body := sh[strings.Index(sh, "SEQUENCE=()"):]
		branch := body[strings.Index(body, "if [[ ${#WORKERS[@]} -ge 2 ]]; then"):]
		if twoWorkers {
			branch = branch[:strings.Index(branch, "\n  else")]
		} else {
			branch = branch[strings.Index(branch, "\n  else"):]
			branch = branch[:strings.Index(branch, "\n  fi")]
		}
		var out []run
		for _, m := range regexp.MustCompile(`"(\S+)\s+(A-\S+)\s+(\S+)\s+\$(W\d)"`).
			FindAllStringSubmatch(branch, -1) {
			dose := "self"
			if strings.HasPrefix(m[1], "grace") {
				dose = "grace"
			}
			arm := "h"
			if strings.HasSuffix(m[2], "ignore") {
				arm = "i"
			}
			out = append(out, run{dose, arm, m[3], m[4]})
		}
		return out
	}
	for _, tw := range []bool{false, true} {
		want := 4
		if tw {
			want = 6
		}
		if got := len(blockFor(tw)); got != want {
			t.Fatalf("parsed %d runs from the %d-worker block, want %d; the script's shape has moved and "+
				"this test is reading the wrong region", got, map[bool]int{false: 1, true: 2}[tw], want)
		}
	}
	// Every (dose, arm) cell must appear EXACTLY ONCE per block. That is what makes repetition safe: with
	// one of each, every comparison's subset alternates however the block is ordered, and with two of any
	// the alternation breaks at the duplicate no matter how the rest is arranged.
	for _, tw := range []bool{false, true} {
		seen := map[string]int{}
		for _, r := range blockFor(tw) {
			seen[r.dose+"/"+r.arm+"/"+r.node]++
		}
		for cell, n := range seen {
			if n != 1 {
				t.Fatalf("the %d-worker block runs cell %s %d times; repeating a block with a duplicated "+
					"cell breaks the alternation its comparison depends on",
					map[bool]int{false: 1, true: 2}[tw], cell, n)
			}
		}
	}
	alternates := func(v []string) bool {
		for i := 0; i+1 < len(v); i++ {
			if v[i] == v[i+1] {
				return false
			}
		}
		return true
	}
	for _, twoWorkers := range []bool{false, true} {
		for _, reps := range []int{2, 3, 4, 6} {
			var seq []run
			for r := 1; r <= reps; r++ {
				for _, b := range blockFor(twoWorkers) {
					b.id = fmt.Sprintf("%s%d", strings.TrimSuffix(b.id, "$r"), r)
					seq = append(seq, b)
				}
			}
			// Keyed by the glob each printed command selects with.
			subsets := map[string][]string{}
			for _, r := range seq {
				onW1Grace := r.dose == "grace" && r.id[0] == 'g'
				if r.dose == "self" {
					subsets["arm | self-completing-*"] = append(subsets["arm | self-completing-*"], r.arm)
				}
				if onW1Grace {
					subsets["arm | grace-bounded-*-g??"] = append(subsets["arm | grace-bounded-*-g??"], r.arm)
				}
				if r.arm == "h" && (r.dose == "self" || onW1Grace) {
					subsets["dose | A-honor"] = append(subsets["dose | A-honor"], r.dose)
				}
				if r.arm == "i" && (r.dose == "self" || onW1Grace) {
					subsets["dose | A-ignore"] = append(subsets["dose | A-ignore"], r.dose)
				}
				if twoWorkers && r.dose == "grace" {
					k := "node | grace-A-" + map[string]string{"h": "honor", "i": "ignore"}[r.arm] + "-*"
					subsets[k] = append(subsets[k], r.node)
				}
			}
			for name, v := range subsets {
				if !alternates(v) {
					t.Errorf("workers=%d reps=%d: %s does not alternate: %v",
						map[bool]int{false: 1, true: 2}[twoWorkers], reps, name, v)
				}
			}
		}
	}
}

// Every GPU node group must carry EXACTLY ONE of the two mode labels.
//
// eks.tf's own comment names half of this and it is worth a test rather than a paragraph: the DaemonSets
// select on this repository's labels, not NVIDIA's GPU Feature Discovery ones. A tainted group carrying
// NEITHER schedules the tenant's Pods and not the observer, so the card is used and nothing watches it --
// and the run reports "no device observer ran" without anything saying why the observer was absent.
//
// Carrying BOTH is the newer hazard and the worse one. platform.lkhun9311.github.io/gpu selects the
// exclusive device plugin and the observer; platform.lkhun9311.github.io/gpu-sharing selects the
// time-slicing plugin. A node with both runs two device plugins registering nvidia.com/gpu against one
// kubelet socket, on a rented card, in the middle of a paid session.
//
// The split is the mechanism that keeps M5-c's sharing matrix away from queuelab's device evidence: under
// time-slicing a busy SM belongs to no single Pod, so the sharing node has no observer BY DESIGN, and that
// absence must be distinguishable from the absence that is a bug. One label each is how.
func TestEveryGPUTaintedNodeGroupCarriesExactlyOneModeLabel(t *testing.T) {
	tf := repoFile(t, "infra/aws/cluster/eks.tf")

	blockStart := regexp.MustCompile(`(?m)^    ([a-z_]+) = \{$`)
	idx := blockStart.FindAllStringSubmatchIndex(tf, -1)
	if len(idx) == 0 {
		t.Fatal("no node-group blocks found in eks.tf; this test is reading a shape that has moved and must " +
			"be rewritten rather than deleted")
	}

	// Matched with a whitespace-tolerant pattern rather than a literal, because HCL aligns the `=` in a
	// block and terraform fmt does it for you. A literal with single spaces stops matching the moment a
	// longer key is added beside it -- and it fails OPEN: the label reads as absent, so a node group that
	// carries both labels passes as though it carried one. That is exactly the mutation that survived the
	// first version of this test.
	//
	// The closing quote after `gpu` is what keeps the exclusive pattern from matching `gpu-sharing`.
	exclusiveLabel := regexp.MustCompile(`"platform\.lkhun9311\.github\.io/gpu"\s*=\s*"true"`)
	sharingLabel := regexp.MustCompile(`"platform\.lkhun9311\.github\.io/gpu-sharing"\s*=\s*"true"`)
	gpuTaint := regexp.MustCompile(`key\s*=\s*"nvidia\.com/gpu"`)

	tainted, exclusive, sharing := 0, 0, 0
	for i, m := range idx {
		name := tf[m[2]:m[3]]
		end := len(tf)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := tf[m[0]:end]
		if !gpuTaint.MatchString(block) {
			continue
		}
		tainted++
		hasExclusive := exclusiveLabel.MatchString(block)
		hasSharing := sharingLabel.MatchString(block)
		switch {
		case hasExclusive && hasSharing:
			t.Errorf("node group %q carries both mode labels, so the exclusive and time-slicing device "+
				"plugins would both schedule there and register nvidia.com/gpu against one kubelet socket", name)
		case hasExclusive:
			exclusive++
		case hasSharing:
			sharing++
		default:
			t.Errorf("node group %q is tainted for GPUs and carries neither %s nor %s, so no device plugin "+
				"is scheduled there and the node advertises nothing -- which reads downstream as 'no GPU "+
				"nodes' rather than as a missing plugin", name, exclusiveLabel, sharingLabel)
		}
	}
	if tainted == 0 {
		t.Fatal("no GPU-tainted node group found in eks.tf; this test would pass vacuously")
	}
	// queuelab needs a multi-GPU node under the exclusive plugin, M5-b needs a single card under it, and
	// M5-c needs a card under the sharing plugin. Losing either side of the split loses an experiment.
	if exclusive < 2 {
		t.Errorf("found %d exclusive GPU node group(s); queuelab needs a multi-GPU node and M5-b a single-card one", exclusive)
	}
	if sharing < 1 {
		t.Error("no sharing node group; M5-c's matrix has nowhere to run that is not also observed")
	}
}

// The README's run count must match the record set it describes.
//
// This is the drift that actually happened rather than one imagined for a test: the paragraph said "four
// runs, two per arm" long after the set had grown to twelve, and nothing noticed because a sentence and a
// directory have no relationship a compiler can see. A reader who takes the README at its word is being
// told the experiment is a third of its size.
//
// The magnitudes were removed from the README at the same time for the same reason -- a number restated in
// two places drifts in one of them -- so the count is what is left to hold.
func TestTheReadmeRunCountMatchesTheRecordsOnDisk(t *testing.T) {
	readme := repoFile(t, "README.md")

	m := regexp.MustCompile(`accept: ([a-z]+)\s*\n?runs,`).FindStringSubmatch(readme)
	if m == nil {
		t.Fatal("the README no longer states a run count in the shape this reads; if the sentence moved, " +
			"point this test at the new one rather than deleting it -- the drift it guards is silent")
	}
	words := map[string]int{
		"four": 4, "six": 6, "eight": 8, "ten": 10, "twelve": 12, "sixteen": 16, "twenty": 20, "twenty-four": 24,
	}
	claimed, ok := words[m[1]]
	if !ok {
		t.Fatalf("the README claims %q runs, which this test cannot compare against a count", m[1])
	}

	files, err := filepath.Glob("../../ex/e17-*.json")
	if err != nil {
		t.Fatalf("glob the record set: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no e17 records on disk; this test would pass vacuously against any claim")
	}
	if claimed != len(files) {
		t.Errorf("the README says %d runs and %d records are on disk; a reader is being told the experiment "+
			"is a different size than it is", claimed, len(files))
	}
}

// No terraform variable may be declared twice, and no GPU group may point at a card the sizing was not done for.
//
// Both halves of this were broken at once, and the first `terraform plan` would have been where anyone
// found out. Editing variables.tf to move M5-b and M5-c onto one card class left the previous declarations
// in place, so `gpu_shared_node_instance_type` and `gpu_single_node_instance_type` each appeared twice --
// which terraform rejects outright as a duplicate declaration, meaning the committed configuration did not
// parse. Worse if it had: one of the stale copies still defaulted to a T4, and every figure in
// hack/m5b-vllm-sizing.md is derived for an A10G's 23,028 MiB. A run on the wrong card would have produced
// numbers that looked ordinary.
func TestTerraformDeclaresEachVariableOnceAndSizesTheGPUGroupsForTheirCard(t *testing.T) {
	tf := repoFile(t, "infra/aws/cluster/variables.tf")

	decl := regexp.MustCompile(`(?m)^variable "([^"]+)"`)
	seen := map[string]int{}
	for _, m := range decl.FindAllStringSubmatch(tf, -1) {
		seen[m[1]]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("variable %q is declared %d times; terraform refuses a duplicate declaration, so this "+
				"configuration does not parse", name, n)
		}
	}

	// The card each GPU group provisions, against what its milestone was sized for.
	//
	// queuelab needs two devices on ONE node and no G-family size has exactly two, so its smallest machine
	// is a 12xlarge. M5-b and M5-c share one card class deliberately: the matrix's exclusive arm IS the
	// flagship's configuration, and two Qwen2.5-3B engines do not fit on a T4.
	want := map[string]string{
		"gpu_node_instance_type":        "g4dn.12xlarge",
		"gpu_single_node_instance_type": "g5.",
		"gpu_shared_node_instance_type": "g5.",
	}
	for name, prefix := range want {
		block := regexp.MustCompile(`(?s)variable "` + name + `" \{.*?\n\}`).FindString(tf)
		if block == "" {
			t.Errorf("variable %q is missing; a node group would fall back to a default nobody sized for", name)
			continue
		}
		def := regexp.MustCompile(`default\s*=\s*"([^"]+)"`).FindStringSubmatch(block)
		if def == nil {
			t.Errorf("variable %q declares no default", name)
			continue
		}
		if !strings.HasPrefix(def[1], prefix) {
			t.Errorf("variable %q defaults to %q, and its milestone is sized for a %s* card; a run on the "+
				"wrong card produces numbers that look ordinary", name, def[1], prefix)
		}
	}

	// And the two that must agree, must. M5-b and M5-c on different silicon would confound the one arm they
	// share, which is the whole reason they were consolidated.
	one := regexp.MustCompile(`(?s)variable "gpu_single_node_instance_type" \{.*?default\s*=\s*"([^"]+)"`).FindStringSubmatch(tf)
	two := regexp.MustCompile(`(?s)variable "gpu_shared_node_instance_type" \{.*?default\s*=\s*"([^"]+)"`).FindStringSubmatch(tf)
	if one != nil && two != nil && one[1] != two[1] {
		t.Errorf("M5-b provisions %q and M5-c provisions %q; the matrix's exclusive arm is the flagship's own "+
			"measurement and cannot be taken on different silicon", one[1], two[1])
	}
}

// Every terraform region must be the one the GPU quota was granted in, and they must all agree.
//
// The failure this guards does not happen at apply time, which is why it would have survived one. Every GPU
// node group runs at desired_size 0, so `terraform apply` in the wrong region succeeds: it builds a VPC, a
// cluster and four node groups, none of which launches a G instance. The quota error arrives later, when a
// session scales one up -- on a cluster that has looked healthy for hours, at the start of the window the
// card is being paid for.
//
// The account's "Running On-Demand G and VT instances" quota was raised to 52 vCPU in ap-northeast-2 on
// 2026-08-26. Everything terraform builds has to live there: a region set in the cluster but not the state
// backend, or in the backend but not the registry, is the same failure with an extra step.
func TestEveryTerraformRegionIsWhereTheGPUQuotaLives(t *testing.T) {
	const quotaRegion = "ap-northeast-2"

	// The workflows and the runbook script are in this list because THEY are what regressed.
	//
	// This test was written after the terraform default was found pointing at us-east-1, and it covered the
	// five terraform files -- while .github/workflows/{infra,destroy,ci}.yml and hack/m5a-ephemeral-runbook.sh
	// still carried the same wrong region. A later review found those, and this test had been green
	// throughout: it excluded exactly the callers that had the bug.
	//
	// The consequence for destroy.yml was the worst of them. Its first call is
	// `aws eks describe-cluster --region "$AWS_REGION"`, which returns ResourceNotFoundException in Virginia,
	// so the nightly teardown logged "cluster not found; skipping" and exited clean while Seoul kept billing.
	// ci.yml would have pushed the operator image to a registry that does not exist there.
	files := map[string]string{
		"infra/aws/cluster/variables.tf":        `(?s)variable "region" \{.*?default\s*=\s*"([^"]+)"`,
		"infra/aws/bootstrap/variables.tf":      `(?s)variable "region" \{.*?default\s*=\s*"([^"]+)"`,
		"infra/aws/argo-bootstrap/variables.tf": `(?s)variable "region" \{.*?default\s*=\s*"([^"]+)"`,
		"infra/aws/cluster/backend.tf":          `region\s*=\s*"([^"]+)"`,
		"infra/aws/argo-bootstrap/backend.tf":   `region\s*=\s*"([^"]+)"`,
		".github/workflows/infra.yml":           `(?m)^\s*AWS_REGION:\s*(\S+)\s*$`,
		".github/workflows/destroy.yml":         `(?m)^\s*AWS_REGION:\s*(\S+)\s*$`,
		".github/workflows/ci.yml":              `(?m)^\s*AWS_REGION:\s*(\S+)\s*$`,
		"hack/m5a-ephemeral-runbook.sh":         `(?m)^REGION="\$\{AWS_REGION:-([^}]+)\}"\s*$`,
	}
	for path, pat := range files {
		body := repoFile(t, path)
		m := regexp.MustCompile(pat).FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s declares no region in the shape this reads; if the file moved, point this test at "+
				"the new one rather than deleting it -- the failure it guards arrives at session time", path)
			continue
		}
		if m[1] != quotaRegion {
			t.Errorf("%s uses region %q and the GPU quota was granted in %q; the apply would succeed and the "+
				"scale-up would fail with the card already being paid for", path, m[1], quotaRegion)
		}
	}
}

// No GPU node group may pin itself to a subnet index, because an index does not know what runs there.
//
// Every group used module.vpc.public_subnets[0], which is whichever zone happened to come back first. In
// ap-northeast-2 that is 2a and g5 is offered there, so it worked -- and would have kept working until the
// day 2a dropped out of the available list, when the slice becomes [2b, 2c], subnet zero becomes 2b, and
// g5 is not offered in 2b at all.
//
// Like the region, this fails at SESSION time rather than at apply: the groups run at desired_size 0, so
// terraform builds them and launches nothing. placement.tf derives each group's subnets from where its own
// instance type is actually offered and stops the plan when that set is empty. This keeps the derivation.
func TestNoGPUNodeGroupPinsItselfToASubnetIndex(t *testing.T) {
	tf := repoFile(t, "infra/aws/cluster/eks.tf")

	blockStart := regexp.MustCompile(`(?m)^    ([a-z_]+) = \{$`)
	idx := blockStart.FindAllStringSubmatchIndex(tf, -1)
	if len(idx) == 0 {
		t.Fatal("no node-group blocks found in eks.tf; this test reads a shape that has moved")
	}
	indexed := regexp.MustCompile(`public_subnets\[\d+\]`)
	gpuTaint := regexp.MustCompile(`key\s*=\s*"nvidia\.com/gpu"`)

	checked := 0
	for i, m := range idx {
		name := tf[m[2]:m[3]]
		end := len(tf)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := tf[m[0]:end]
		if !gpuTaint.MatchString(block) {
			continue
		}
		checked++
		if loc := indexed.FindString(block); loc != "" {
			t.Errorf("GPU node group %q pins %s; a subnet index does not know whether its zone offers the "+
				"group's instance type, and the mismatch surfaces when a session scales up rather than at "+
				"apply", name, loc)
		}
	}
	if checked == 0 {
		t.Fatal("no GPU-tainted node group found; this test would pass vacuously")
	}

	// And the derivation has to exist, or the groups are pinned to nothing at all.
	place := repoFile(t, "infra/aws/cluster/placement.tf")
	for _, want := range []string{"aws_ec2_instance_type_offerings", "precondition"} {
		if !strings.Contains(place, want) {
			t.Errorf("placement.tf no longer contains %q; the subnets would be derived from something other "+
				"than where the instance type is offered, or an empty set would reach apply silently", want)
		}
	}
}

// camelFromScreamingSnake turns NO_EXECUTE into NoExecute.
//
// This was strings.Title, which Go deprecated because its word boundaries are wrong for Unicode punctuation.
// The inputs here are terraform enum spellings and will never be anything but ASCII, so the fix is to say
// that in code rather than to pull in golang.org/x/text for a three-value vocabulary.
func camelFromScreamingSnake(s string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(strings.ToLower(s), "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}
