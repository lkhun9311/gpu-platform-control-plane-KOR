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
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fakeKubelet is the double these tests drive the real canary against, and it is worth being exact about what
// it does and does not stand in for.
//
// It models three behaviours of a cluster that the fake client has none of, each of which the canary depends
// on: the apiserver DEFAULTING a Pod's termination grace period (the fake stores what it is given and defaults
// nothing), a container reporting itself Running, and a container reporting a terminal status with an exit
// code some interval after the Pod was asked to stop. That interval is the entire experiment, so it is a
// per-probe parameter here rather than a fixed behaviour: "SIGTERM honoured" and "SIGKILL after the grace
// period" and "everything killed at once" are the same double with different numbers.
//
// What it emphatically does NOT model is whether a real kubelet delivers SIGTERM at all — which is the very
// question the canary exists to answer and cannot be answered by a double. These tests establish that the
// canary reads a cluster correctly and judges what it read; only a run against a real cluster establishes what
// the reading is.
type fakeKubelet struct {
	mu sync.Mutex
	// stopAfter is how long each probe takes to report a terminal status once deleted, keyed by Pod name
	// suffix ("honor" / "ignore"), with the code it ends on.
	stopAfter map[string]time.Duration
	exitCode  map[string]int32
	deletedAt map[string]time.Time
	// defaultGrace is what this cluster's apiserver defaults onto a Pod that names none.
	defaultGrace int64
	now          func() time.Time
}

func newFakeKubelet(now func() time.Time, honorStop, ignoreStop time.Duration,
	honorExit, ignoreExit int32) *fakeKubelet {
	return &fakeKubelet{
		stopAfter:    map[string]time.Duration{"honor": honorStop, "ignore": ignoreStop},
		exitCode:     map[string]int32{"honor": honorExit, "ignore": ignoreExit},
		deletedAt:    map[string]time.Time{},
		defaultGrace: terminationGraceSec,
		now:          now,
	}
}

func (k *fakeKubelet) which(name string) string {
	if strings.HasSuffix(name, "-honor") {
		return "honor"
	}
	return "ignore"
}

// interceptors wires the double into a fake client on the three verbs it has to be present for.
func (k *fakeKubelet) interceptors() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
			opts ...client.CreateOption) error {
			if p, ok := obj.(*corev1.Pod); ok && p.Spec.TerminationGracePeriodSeconds == nil {
				// The defaulting the canary reads back out of the Create response, which is the one place the
				// cluster's own grace period becomes visible to it.
				g := k.defaultGrace
				p.Spec.TerminationGracePeriodSeconds = &g
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
			opts ...client.DeleteOption) error {
			if p, ok := obj.(*corev1.Pod); ok {
				k.mu.Lock()
				if _, seen := k.deletedAt[p.Name]; !seen {
					k.deletedAt[p.Name] = k.now()
				}
				k.mu.Unlock()
			}
			return c.Delete(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			p, ok := obj.(*corev1.Pod)
			if !ok {
				return nil
			}
			k.mu.Lock()
			defer k.mu.Unlock()
			t0, deleted := k.deletedAt[p.Name]
			w := k.which(p.Name)
			switch {
			case deleted && !k.now().Before(t0.Add(k.stopAfter[w])):
				p.Status.Phase = corev1.PodFailed
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: probeContainerName(p),
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: k.exitCode[w], Reason: "Error",
					}},
				}}
			default:
				p.Status.Phase = corev1.PodRunning
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  probeContainerName(p),
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			}
			return nil
		},
	}
}

// trainerContainerOf finds the container the arm's command is in, by name.
//
// Tests that assert on "the probe's container" have to look it up rather than index it, for the reason the
// production readers do: a template can put a sidecar in front of the trainer, and an assertion that indexes 0
// would then be checking the wrong container while still passing.
func trainerContainerOf(t *testing.T, p *corev1.Pod) corev1.Container {
	t.Helper()
	for _, ctr := range p.Spec.Containers {
		if ctr.Name == probeTrainerContainer {
			return ctr
		}
	}
	t.Fatalf("the probe has no %q container: %+v", probeTrainerContainer, p.Spec.Containers)
	return corev1.Container{}
}

// probeContainerName is the container a kubelet would publish a status for: the one the Pod declares.
//
// It reads the Pod instead of naming a container, and that is the correction of a fixture that had gone stale
// the moment the probe stopped being hand-built. The double went on stamping "sleeper" while the probe's
// container came from the operator's template and was called "trainer", and nothing failed — production
// matches on container STATE, not on the name — so a double describing a Pod that no longer existed was
// invisible. A double that invents its own names is one that can disagree with the cluster it stands for.
func probeContainerName(p *corev1.Pod) string {
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Name
	}
	return probeTrainerContainer
}

// canaryCluster builds a fake worker plus the double, and returns the client and the clock the canary runs on.
func canaryCluster(t *testing.T, k *fakeKubelet, objs ...client.Object) client.WithWatch {
	t.Helper()
	// The worker starts with NO recorded qualification: this is the cluster before anybody has canaried it,
	// which is the state the mode exists to change.
	all := []client.Object{node(nil, map[string]string{canaryAnnotationKey: ""})}
	all = append(all, objs...)
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(all...).
		WithInterceptorFuncs(k.interceptors()).Build()
}

// readCanary pulls the qualification back off the node, the way a later run's qualify would.
func readCanary(t *testing.T, c client.Client) canaryQualification {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &n); err != nil {
		t.Fatalf("read the worker back: %v", err)
	}
	raw := n.Annotations[canaryAnnotationKey]
	if raw == "" {
		t.Fatal("the canary recorded nothing on the worker, so nothing it observed survives the process that " +
			"observed it")
	}
	q, err := decodeCanary(raw)
	if err != nil {
		t.Fatalf("the canary wrote a document it cannot read back: %v (%s)", err, raw)
	}
	return q
}

// The whole mode, end to end, against a cluster that behaves the way the arms need it to: the honouring probe
// stops in under a second, the ignoring one spends the grace period and is killed at the end of it.
//
// What this establishes is that the reading reaches the cluster and comes back: a qualification the next run
// can consult is on the Node, the probes are gone, and the worker is free again. Everything the canary
// concluded is derived from what the double reported, so the double is the only thing pretending here.
//
// Mutation that turns this red: skip stampCanary (the qualification never reaches the node and no run could
// consult it); or release the worker before the stamp (the stamp then fails ownership verification).
func TestTheCanaryRecordsAPassingQualificationOnTheWorkerItsProbes(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, 700*time.Millisecond, 30*time.Second+400*time.Millisecond,
		honorExitCode, killExitCode)
	c := canaryCluster(t, k)
	var out bytes.Buffer

	if err := terminationCanary(context.Background(), c, "platform-worker", now, sleep, &out); err != nil {
		t.Fatalf("a cluster that stops a workload when asked did not qualify: %v\n%s", err, out.String())
	}

	q := readCanary(t, c)
	if len(q.Failures) != 0 {
		t.Fatalf("the recorded qualification names failures: %v", q.Failures)
	}
	if !q.Honor.Terminated || q.Honor.ExitCode != honorExitCode {
		t.Fatalf("the honouring probe's own reading must be recorded, got %+v", q.Honor)
	}
	if !q.Ignore.Terminated || q.Ignore.ExitCode != killExitCode {
		t.Fatalf("the ignoring probe's own reading must be recorded, got %+v", q.Ignore)
	}
	// The separation is the product. Anything that collapsed it would still have produced two readings.
	if q.Ignore.StoppedAfterMs-q.Honor.StoppedAfterMs < canarySurvivesAtLeast.Milliseconds() {
		t.Fatalf("the recorded contrast is %dms against %dms, which is not a separation the arms could be "+
			"built on", q.Honor.StoppedAfterMs, q.Ignore.StoppedAfterMs)
	}
	if q.Key.GraceSec != terminationGraceSec || q.Key.NodeUID != "uid-node" ||
		q.Key.KubeletVersion != testKubeletVersion || q.Key.ContainerRuntime != testContainerRuntime {
		t.Fatalf("the key must record the combination the reading was taken on, got %+v", q.Key)
	}
	// The operator's template travels with the rest, and it is the one field of the key this mode copies from
	// the contract rather than reading off the cluster: a canary that recorded it empty would write a document
	// decodeCanary refuses, and one that recorded somebody else's would be matched by no run. The consult below
	// is what proves it is the right one; this is what proves it is there at all.
	if len(q.Key.PodTemplateHash) != 64 {
		t.Fatalf("the recorded key fingerprints the operator's pod template as %q, which is not a hash",
			q.Key.PodTemplateHash)
	}

	// A later run must be able to stand on what was just written, through the real consult rather than by
	// inspection: this is the join between the two halves of the feature, and it is where a key the canary
	// writes but the run cannot match would show up.
	var worker corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &worker); err != nil {
		t.Fatalf("read the worker: %v", err)
	}
	ref, err := checkTerminationCanary(&worker, mustHarnessContract(t), "")
	if err != nil {
		t.Fatalf("the qualification this canary just recorded does not satisfy the gate that consults it: %v", err)
	}
	if ref.CanaryID != q.CanaryID {
		t.Fatalf("the reference names canary %q, the node carries %q", ref.CanaryID, q.CanaryID)
	}

	// Nothing of the canary's is left running, and the worker is back.
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("the canary left %d probe Pod(s) behind: its finalizer is what keeps them readable, and "+
			"nothing else removes it", len(pods.Items))
	}
	obs := observe(&worker)
	if obs.HasLabel || len(obs.Taints) > 0 || obs.JournalRaw != "" {
		t.Fatalf("the canary did not give the worker back: %+v", obs)
	}
}

// The failure this whole file exists to catch, driven through the real mode: a cluster that stops every
// container the instant it is asked. Both probes end promptly with plausible codes, and the honouring half is
// impeccable — which is exactly why the verdict must not be taken from it alone.
//
// The recorded document is the point. A canary that printed its refusal and wrote nothing would leave a
// worker carrying whatever qualification it had before, so the next run would consult a document this canary
// had just falsified.
//
// Mutation that turns this red: return before stampCanary when the judgement failed, which is the natural
// "only record successes" reading.
func TestAFailingCanaryOverwritesTheQualificationItJustFalsified(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	// The killing runtime: both stop at once, and the honouring one even carries 143 — which an untrapped
	// SIGTERM also produces.
	k := newFakeKubelet(now, 300*time.Millisecond, 400*time.Millisecond, honorExitCode, honorExitCode)
	c := canaryCluster(t, k)
	// A qualification from before the cluster broke, which this canary must replace rather than leave standing.
	var before corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &before); err != nil {
		t.Fatalf("read the worker: %v", err)
	}
	before.Annotations[canaryAnnotationKey] = qualifiedCanaryAnnotation()
	if err := c.Update(context.Background(), &before); err != nil {
		t.Fatalf("seed the previous qualification: %v", err)
	}

	var out bytes.Buffer
	err := terminationCanary(context.Background(), c, "platform-worker", now, sleep, &out)
	if err == nil {
		t.Fatalf("a cluster that kills everything immediately qualified:\n%s", out.String())
	}

	q := readCanary(t, c)
	if len(q.Failures) == 0 {
		t.Fatal("the previous, passing qualification is still on the node: runs would keep consulting a " +
			"document this canary has just shown to be false")
	}
	if !strings.Contains(strings.Join(q.Failures, "\n"), "ignoring probe") {
		t.Fatalf("the recorded failure must name the half that broke, got %v", q.Failures)
	}

	// And the gate refuses on it, which is the behaviour the record exists for.
	var after corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &after); err != nil {
		t.Fatalf("read the worker: %v", err)
	}
	if _, cerr := checkTerminationCanary(&after, mustHarnessContract(t), ""); cerr == nil {
		t.Fatal("a worker carrying a failed canary passed the consult")
	}
	if !strings.Contains(out.String(), "NOT QUALIFIED") {
		t.Fatalf("the operator must be told on their terminal too, got:\n%s", out.String())
	}
}

// The exit code is read off a Pod that has already been deleted, and it is only readable because the canary
// hangs its own finalizer on the probe: without one the object is removed as soon as its containers stop, and
// the code the whole reading turns on goes with it.
//
// This test drives that directly — the double removes the terminal status the moment the object would have
// been collected — so it fails for the reason the finalizer exists rather than by inspecting a field.
//
// Mutation that turns this red: drop the Finalizers line from canaryPod. The fake client then removes each
// probe on Delete exactly as a real apiserver does once the kubelet is finished, every subsequent read is a
// NotFound, and the canary records two probes it never saw stop.
func TestTheCanaryCanStillReadAProbeThatHasBeenDeleted(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, 500*time.Millisecond, 30*time.Second, honorExitCode, killExitCode)
	c := canaryCluster(t, k)

	if err := terminationCanary(context.Background(), c, "platform-worker", now, sleep, &bytes.Buffer{}); err != nil {
		t.Fatalf("canary: %v", err)
	}
	q := readCanary(t, c)
	for _, p := range []canaryProbe{q.Honor, q.Ignore} {
		if !p.Terminated {
			t.Fatalf("probe %s was deleted and its ending was never read (%s); the exit code exists only "+
				"between the container stopping and the object being collected", p.Pod, p.Unobserved)
		}
	}
}

// The Pod the canary probes has to be the one a run's Pod would be, on the machine being qualified.
//
// spec.nodeName rather than a selector, because what is under test is THIS node's kubelet and runtime and a
// probe that landed elsewhere would qualify a machine nobody asked about.
//
// The grace period is now asserted against the TEMPLATE's rather than against nil, and the change is the point
// of this commit rather than a loosening. What must never happen is this function pinning a period of its own,
// which would measure a configuration no run uses and hide the cluster default four pieces of the protocol's
// arithmetic rest on. What must now happen is the probe carrying whatever the operator's template says — nil
// today — so that a template which pins one is measured under it instead of around it.
//
// Mutation that turns this red: pin TerminationGracePeriodSeconds in probePodFrom (it then differs from the
// template's nil), or swap nodeName for a NodeSelector.
func TestTheProbePodIsTheWorkloadAsTheRunWouldSubmitIt(t *testing.T) {
	c := mustHarnessContract(t)
	honor, _ := canaryProbeSpecs("abcd1234-5678", c)
	p, err := canaryPod("abcd1234-5678", "platform-worker", "canary-abcd1234", c, honor)
	if err != nil {
		t.Fatalf("build the probe: %v", err)
	}

	if p.Spec.NodeName != "platform-worker" {
		t.Fatalf("the probe does not name the node it is qualifying (nodeName %q): a probe that ran elsewhere "+
			"would report another machine's runtime", p.Spec.NodeName)
	}
	if want := renderedPodTemplate(templateProbeJob()).Spec.TerminationGracePeriodSeconds; !reflect.DeepEqual(
		p.Spec.TerminationGracePeriodSeconds, want) {
		t.Fatalf("the probe's grace period is %v and the operator's template says %v; a probe that pins its own "+
			"measures a configuration no run uses, and one that drops the template's measures around the change "+
			"it exists to catch", p.Spec.TerminationGracePeriodSeconds, want)
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart policy %q: a probe that restarts after being killed has no terminal status to read",
			p.Spec.RestartPolicy)
	}
	// Exactly the canary's, which is a claim about the OVERLAY and not only about presence: the finalizer is the
	// one thing here that replaces rather than merges, because releaseProbes can only remove its own and a
	// foreign one would strand the probe object. The row in
	// TestTheProbeCarriesWhatTheOperatorAddsToTheTemplate drives the case where the template has one.
	if len(p.Finalizers) != 1 || p.Finalizers[0] != canaryFinalizer {
		t.Fatalf("the probe carries finalizers %v; without the canary's own the exit code is collected along "+
			"with the object, and with anybody else's the object cannot be cleared", p.Finalizers)
	}
	if p.Spec.Containers[0].Image != c.Image {
		t.Fatalf("the probe runs image %q, not the pinned sleeper %q", p.Spec.Containers[0].Image, c.Image)
	}
	if strings.Join(p.Spec.Containers[0].Command, " ") != strings.Join(c.HonorCommand, " ") {
		t.Fatalf("the probe runs %v, not the honouring command the arm submits (%v)",
			p.Spec.Containers[0].Command, c.HonorCommand)
	}
	// The node is tainted by the canary's own acquisition moments before the probe is created, and the
	// toleration is the same shape internal/queuelab's ResourceFlavor gives the arm's Pods.
	if len(p.Spec.Tolerations) != 1 || p.Spec.Tolerations[0].Key != workerTaintKey ||
		p.Spec.Tolerations[0].Value != "canary-abcd1234" {
		t.Fatalf("the probe does not tolerate the ownership taint this canary installed: %+v", p.Spec.Tolerations)
	}
	// And the container it put the arm's command in is the one the OPERATOR names, which is what says this Pod
	// came out of the template rather than out of this file.
	if p.Spec.Containers[0].Name != probeTrainerContainer {
		t.Fatalf("the probe's container is %q; the operator renders a run's workload into %q, so a probe that "+
			"names it anything else was hand-built", p.Spec.Containers[0].Name, probeTrainerContainer)
	}
}

// The property this commit exists for: whatever the operator adds to its Pod template is IN the Pod the canary
// probes, so the re-take a key mismatch forces actually exercises the change.
//
// Before this, the loop detected and then rubber-stamped. A preStop hook added to the template changed the key,
// the run refused, the operator re-took the reading — and the re-take probed a Pod with no preStop hook in it
// and passed. A hollow remediation is worse than an absent one, because it looks like a check that ran.
//
// The template is passed in rather than rendered, so these are additions this build's operator does not make
// yet — which is the only way to test the behaviour before the change that needs it exists.
//
// Mutation that turns this red: hand-build the probe's PodSpec in probePodFrom instead of adopting tpl (every
// row below vanishes from the Pod), or copy only the containers across (the three Pod-level rows survive).
func TestTheProbeCarriesWhatTheOperatorAddsToTheTemplate(t *testing.T) {
	c := mustHarnessContract(t)
	honor, _ := canaryProbeSpecs("abcd1234-5678", c)

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodTemplateSpec)
		check  func(*testing.T, *corev1.Pod)
	}{
		// Command and Args are one unit, and the probe replaced only Command. BuildJob renders no Args today,
		// so this is the drift the template hash exists to catch — and catching it was not enough: the
		// re-taken probe would have run `sh -c '<arm command>' <template args>`, where those args become $0
		// and $1 to the shell, while canaryProbe records only the command. The qualification would have named
		// a command line nobody ran, which is detection with hollow remediation.
		//
		// Mutation that turns this red: drop the Args = nil assignment in probePodFrom.
		{"args the template carried alongside its own command", func(tpl *corev1.PodTemplateSpec) {
			for i := range tpl.Spec.Containers {
				if tpl.Spec.Containers[i].Name == probeTrainerContainer {
					tpl.Spec.Containers[i].Args = []string{"--dataset", "/mnt/imagenet"}
				}
			}
		}, func(t *testing.T, p *corev1.Pod) {
			trainer := trainerContainerOf(t, p)
			if len(trainer.Args) != 0 {
				t.Fatalf("the probe kept the template's args %v beside the arm's command; under sh -c those "+
					"become $0 and $1, so the probe runs a different process shape than the arm and the "+
					"recorded command names something nobody ran", trainer.Args)
			}
		}},
		{"an explicit grace period", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.TerminationGracePeriodSeconds = new(int64(60))
		}, func(t *testing.T, p *corev1.Pod) {
			if p.Spec.TerminationGracePeriodSeconds == nil || *p.Spec.TerminationGracePeriodSeconds != 60 {
				t.Fatalf("the probe does not carry the template's grace period: %v",
					p.Spec.TerminationGracePeriodSeconds)
			}
		}},
		{"a preStop hook", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
				PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sleep", "45"}}},
			}
		}, func(t *testing.T, p *corev1.Pod) {
			l := p.Spec.Containers[0].Lifecycle
			if l == nil || l.PreStop == nil || l.PreStop.Exec == nil {
				t.Fatalf("the probe does not carry the template's preStop hook: %+v", l)
			}
		}},
		{"shareProcessNamespace", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.ShareProcessNamespace = new(true)
		}, func(t *testing.T, p *corev1.Pod) {
			if p.Spec.ShareProcessNamespace == nil || !*p.Spec.ShareProcessNamespace {
				t.Fatalf("the probe does not share the process namespace the template asked for: %v",
					p.Spec.ShareProcessNamespace)
			}
		}},
		// PREPENDED, not appended, and that is the whole value of the row. The trainer sitting at index 0 is what
		// let an earlier version of this suite pass while probePodFrom ignored the name and took Containers[0] —
		// which is the exact template probeTrainerContainer's comment describes, one that "grew a sidecar in front
		// of the trainer".
		{"a sidecar in front of the trainer", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.Containers = append([]corev1.Container{{Name: "sidecar", Image: "busybox:1.36"}},
				tpl.Spec.Containers...)
		}, func(t *testing.T, p *corev1.Pod) {
			if len(p.Spec.Containers) != 2 || p.Spec.Containers[0].Name != "sidecar" {
				t.Fatalf("the probe dropped the template's other container: %+v", p.Spec.Containers)
			}
			// And the sidecar is still the sidecar: writing the arm's command into it would be a reading of a
			// workload nobody chose.
			if len(p.Spec.Containers[0].Command) != 0 {
				t.Fatalf("the arm's command went into the sidecar: %+v", p.Spec.Containers[0])
			}
		}},
		// The device strip has to be per-POD, because the admission it avoids is: the kubelet sums the whole Pod
		// against the node's allocatable, so a sidecar's request makes the probe unschedulable exactly as the
		// trainer's would.
		{"a sidecar that asks for a device", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.Containers = append(tpl.Spec.Containers, corev1.Container{
				Name: "sidecar", Image: "busybox:1.36",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{gpuResourceName: *resource.NewQuantity(1, resource.DecimalSI)},
				},
			})
		}, func(t *testing.T, p *corev1.Pod) {
			for _, ctr := range p.Spec.Containers {
				if _, ok := ctr.Resources.Limits[gpuResourceName]; ok {
					t.Fatalf("container %q still asks for %s; the kubelet admits the POD against this node's "+
						"allocatable, so one container's request is enough to make the probe unschedulable",
						ctr.Name, gpuResourceName)
				}
			}
		}},
		// Requests as well as Limits. BuildJob writes only Limits today, so nothing observes the Requests half of
		// the strip unless a template spells it — and an unobserved clause is one that can be deleted without a
		// test noticing, which is how the first version of this suite left it.
		{"a device spelled as a request", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
				gpuResourceName: *resource.NewQuantity(7, resource.DecimalSI),
			}
		}, func(t *testing.T, p *corev1.Pod) {
			if _, ok := p.Spec.Containers[0].Resources.Requests[gpuResourceName]; ok {
				t.Fatalf("the probe still requests %s: %+v", gpuResourceName, p.Spec.Containers[0].Resources)
			}
			// Nil rather than an empty map, or the Pod is the template's shape plus an artefact of the removal.
			if p.Spec.Containers[0].Resources.Requests != nil {
				t.Fatalf("the emptied requests map was sent as %v rather than dropped",
					p.Spec.Containers[0].Resources.Requests)
			}
		}},
		// The one overlay that REPLACES rather than merges, and the only ObjectMeta field where that is the
		// intended semantics: releaseProbes can remove the finalizer it installed and no other, so adopting the
		// template's would strand a probe object nobody can clear. It is still keyed — the hash covers the
		// template's metadata — so this is noticed and deliberately not adopted.
		{"a finalizer of the template's own", func(tpl *corev1.PodTemplateSpec) {
			tpl.Finalizers = []string{"platform.example/keep"}
		}, func(t *testing.T, p *corev1.Pod) {
			if len(p.Finalizers) != 1 || p.Finalizers[0] != canaryFinalizer {
				t.Fatalf("the probe carries finalizers %v; a foreign one cannot be removed by releaseProbes, so "+
					"the probe object would outlive the canary with no command that clears it", p.Finalizers)
			}
		}},
		{"a cpu limit", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(500,
				resource.DecimalSI)
		}, func(t *testing.T, p *corev1.Pod) {
			// The device request is removed and nothing else is: a limit that shapes what the kubelet does to the
			// container has to survive, or the strip has quietly become "drop the resources".
			if q, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; !ok || q.MilliValue() != 500 {
				t.Fatalf("the probe dropped the template's cpu limit along with the device request: %+v",
					p.Spec.Containers[0].Resources)
			}
		}},
		{"a template label", func(tpl *corev1.PodTemplateSpec) {
			tpl.Labels = map[string]string{"platform.example/tier": "batch"}
		}, func(t *testing.T, p *corev1.Pod) {
			if p.Labels["platform.example/tier"] != "batch" {
				t.Fatalf("the probe dropped the template's own labels: %v", p.Labels)
			}
			// And its own are still there, or the merge has become a replacement in the other direction.
			if p.Labels["queuelab.gpu-platform/contract"] != honor.contract {
				t.Fatalf("the probe lost the label that says which arm it is: %v", p.Labels)
			}
		}},
		// This row exists because a mutation escaped without it. Turning the toleration overlay from an append
		// into a replacement was green everywhere: the structural equality below runs against today's template,
		// which carries no toleration of its own, so appending to nothing and replacing nothing produce the same
		// one-element list. Only a template that already tolerates something can tell those apart, and a probe
		// that dropped it would fail to land on a node tainted for any other reason.
		{"a toleration of the template's own", func(tpl *corev1.PodTemplateSpec) {
			tpl.Spec.Tolerations = []corev1.Toleration{{
				Key: "platform.example/reserved", Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoSchedule,
			}}
		}, func(t *testing.T, p *corev1.Pod) {
			var kept, own bool
			for _, tol := range p.Spec.Tolerations {
				if tol.Key == "platform.example/reserved" {
					kept = true
				}
				if tol.Key == workerTaintKey {
					own = true
				}
			}
			if !kept {
				t.Fatalf("the probe replaced the template's tolerations with the canary's own (%+v); a run's Pod "+
					"would tolerate what the template says, and a probe that does not cannot land where it does",
					p.Spec.Tolerations)
			}
			if !own {
				t.Fatalf("the probe no longer tolerates the ownership taint this canary installed: %+v",
					p.Spec.Tolerations)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tpl := renderedPodTemplate(templateProbeJob())
			tc.mutate(&tpl)
			p, err := probePodFrom(tpl, "abcd1234-5678", "platform-worker", "canary-abcd1234", c, honor)
			if err != nil {
				t.Fatalf("build the probe: %v", err)
			}
			tc.check(t, p)
			// Whatever the template said, the probe is still the probe: the arm's command is the mechanism under
			// test, and a Pod that adopted the template's workload as well would measure the sentinel. Looked up
			// BY NAME, because the row above puts the trainer at index 1.
			trainer := trainerContainerOf(t, p)
			if strings.Join(trainer.Command, " ") != strings.Join(c.HonorCommand, " ") {
				t.Fatalf("the probe runs %v, not the honouring command the arm submits", trainer.Command)
			}
			if trainer.Image != c.Image {
				t.Fatalf("the probe runs image %q, not the pinned sleeper", trainer.Image)
			}
		})
	}
}

// The one field where what is hashed and what is run differ, asserted in both directions so neither half can
// rot: the template DOES ask for a device and the probe does NOT.
//
// The precondition matters as much as the conclusion. Without it, a day when the operator stops requesting a
// device at all leaves this test passing for a reason that has nothing to do with the strip, which is how a
// check stops checking without failing.
//
// Why the device goes: spec.nodeName removes the scheduler but not the kubelet's admission, so a probe asking
// for seven devices is rejected outright by any node advertising fewer — and seven is a hash-distinctness
// sentinel, not a number any run needs. Requiring seven free devices before a worker could be qualified would
// make the canary unrunnable on the clusters it exists for.
//
// Mutation that turns this red: stop deleting the device from the probe's limits (the second assertion), or
// delete the whole Resources block instead of the one key (TestTheProbeCarriesWhatTheOperatorAddsToTheTemplate's
// cpu row).
func TestTheProbeAsksForNoDeviceThoughTheTemplateDoes(t *testing.T) {
	tpl := renderedPodTemplate(templateProbeJob())
	want := tpl.Spec.Containers[0].Resources.Limits[gpuResourceName]
	if want.Value() != int64(templateProbeJob().Spec.GPUCount) {
		t.Fatalf("the operator's template requests %v of %s and the probe job asks for %d; this test's point is "+
			"that the probe drops a request the template makes, and it makes none", want.Value(), gpuResourceName,
			templateProbeJob().Spec.GPUCount)
	}

	c := mustHarnessContract(t)
	honor, _ := canaryProbeSpecs("abcd1234-5678", c)
	p, err := canaryPod("abcd1234-5678", "platform-worker", "canary-abcd1234", c, honor)
	if err != nil {
		t.Fatalf("build the probe: %v", err)
	}
	if q, ok := p.Spec.Containers[0].Resources.Limits[gpuResourceName]; ok {
		t.Fatalf("the probe asks for %v of %s; the canary pins spec.nodeName, so the kubelet admits it against "+
			"this node's allocatable and rejects it outright where the devices are not free — a refusal about "+
			"allocation on a reading about signal delivery", q.Value(), gpuResourceName)
	}
}

// A template whose workload container this canary cannot identify is a refusal, not a guess.
//
// Writing the arm's command into whatever container happens to be first would produce a reading of something
// nobody chose — and it would do it silently, since the Pod would start and stop perfectly well. This is also
// the boundary at which the honest answer stops being "adopt the template": a template that can no longer say
// which container is the workload is one the full CRD-path canary should be reading back from the cluster.
//
// Mutation that turns this red: fall back to Containers[0] when no trainer is found.
func TestAProbeCannotBeBuiltFromATemplateWithNoTrainerContainer(t *testing.T) {
	tpl := renderedPodTemplate(templateProbeJob())
	tpl.Spec.Containers[0].Name = "worker"
	c := mustHarnessContract(t)
	honor, _ := canaryProbeSpecs("abcd1234-5678", c)

	_, err := probePodFrom(tpl, "abcd1234-5678", "platform-worker", "canary-abcd1234", c, honor)
	if err == nil {
		t.Fatal("a template with no trainer container built a probe anyway; the arm's command would have gone " +
			"into a container nobody chose and the reading would have looked entirely normal")
	}
	if !strings.Contains(err.Error(), probeTrainerContainer) || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("the refusal must name the container it looked for and what it found instead, got: %v", err)
	}
}

// The probe is the rendered template plus exactly the changes probePodFrom is documented to make, and nothing
// else. Written as one structural equality rather than a list of assertions, because a list only covers what
// somebody thought to list: this fails on any field the transformation touches that its comment does not claim.
//
// Both sides derive from the same renderer, which is deliberate and is not the self-comparison this lineage has
// been caught by: the LEFT side is the transformation under test and the RIGHT side is the template with the
// three documented overlays applied by hand. A transformation that copied nothing, stripped extra, or forgot an
// overlay fails; a change to the renderer itself moves both sides, which is correct, and is pinned separately
// by TestTheTemplateTheKeyCoversIsTheOneTheOperatorRenders.
//
// Mutation that turns this red: replace the DeepCopy of the template's spec with a fresh PodSpec, strip
// anything beyond the device request, or remove either overlay this compares (the nodeName or the toleration).
//
// What it does NOT catch is listed here because the list itself has been wrong twice, and both times in the
// same way: an overlay whose behaviour today's template cannot exercise. Everything below is covered by a row
// in TestTheProbeCarriesWhatTheOperatorAddsToTheTemplate instead, which drives templates this build does not
// render.
//
//   - Turning the toleration overlay from an append into a REPLACEMENT. Today's template tolerates nothing, so
//     both produce the same one-element list.
//   - Anything about ObjectMeta at all — this compares Spec — so neither the label merge nor the finalizer
//     replacement is in scope.
//   - The Requests half of the device strip, and the emptied-map-to-nil that follows it. BuildJob writes only
//     Limits, so `want` has no Requests to model and deleting both lines leaves this green.
//   - The strip being per-container rather than trainer-only, since there is only ever one container here.
func TestTheProbeIsTheTemplateAndTheThreeDocumentedChanges(t *testing.T) {
	c := mustHarnessContract(t)
	honor, _ := canaryProbeSpecs("abcd1234-5678", c)
	got, err := canaryPod("abcd1234-5678", "platform-worker", "canary-abcd1234", c, honor)
	if err != nil {
		t.Fatalf("build the probe: %v", err)
	}

	tpl := renderedPodTemplate(templateProbeJob())
	want := *tpl.Spec.DeepCopy()
	want.Containers[0].Image = c.Image
	want.Containers[0].Command = c.HonorCommand
	for i := range want.Containers {
		delete(want.Containers[i].Resources.Limits, gpuResourceName)
		if len(want.Containers[i].Resources.Limits) == 0 {
			want.Containers[i].Resources.Limits = nil
		}
	}
	want.NodeName = "platform-worker"
	want.Tolerations = append(want.Tolerations, corev1.Toleration{
		Key: workerTaintKey, Operator: corev1.TolerationOpEqual, Value: "canary-abcd1234",
		Effect: corev1.TaintEffectNoSchedule,
	})
	if !reflect.DeepEqual(got.Spec, want) {
		t.Fatalf("the probe is not the operator's template with the documented changes applied:\n got %+v\nwant %+v",
			got.Spec, want)
	}
}

// A canary cannot run on a worker somebody else is holding, and the reason is not tidiness: two probe Pods
// appearing on a node in the middle of a measured run are exactly the foreign contamination the capacity gate
// refuses runs over, and the grace period being exercised by a stranger is the one thing that run is
// measuring.
//
// Mutation that turns this red: create the probes without acquiring the worker first.
func TestTheCanaryRefusesAWorkerSomebodyElseIsHolding(t *testing.T) {
	held := testJournal()
	raw, err := encodeJournal(held)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, time.Second, 30*time.Second, honorExitCode, killExitCode)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node(map[string]string{workerLabelKey: held.Installed.LabelValue},
			map[string]string{canaryAnnotationKey: "", journalKey: raw}, ourTaint())).
		WithInterceptorFuncs(k.interceptors()).Build()

	var out bytes.Buffer
	err = terminationCanary(context.Background(), c, "platform-worker", now, sleep, &out)
	if err == nil {
		t.Fatal("the canary ran on a node another transaction holds: its probes would be foreign Pods on " +
			"somebody else's measured run")
	}
	var r *refusal
	if !asRefusal(err, &r) || r.Reason != reasonForeignOwner {
		t.Fatalf("the refusal must be the ownership transaction's own, classifiable by reason, got %v", err)
	}
	var pods corev1.PodList
	if lerr := c.List(context.Background(), &pods); lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("the canary created %d Pod(s) on a worker it does not hold", len(pods.Items))
	}
}

// Every exit path has to release the probes, and the refusal paths most of all: the finalizer is the one thing
// this mode leaves behind that does not clean itself up, and a probe stuck under it holds a name the next
// canary attempt would collide with.
//
// This drives the path where the probes never start at all — an image that will not pull, in the double's
// terms a Pod that never reports Running — so the failure happens with two live Pods outstanding.
//
// Mutation that turns this red: move releaseProbes out of a defer and onto the success path only.
func TestTheCanaryClearsItsProbesEvenWhenItRefuses(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, time.Second, 30*time.Second, honorExitCode, killExitCode)
	// Nothing ever runs: the Get double is replaced by one that leaves every probe Pending with a waiting
	// container, which is what an ImagePullBackOff looks like from here.
	stuck := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node(nil, map[string]string{canaryAnnotationKey: ""})).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: k.interceptors().Create,
			Delete: k.interceptors().Delete,
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if p, ok := obj.(*corev1.Pod); ok {
					p.Status.Phase = corev1.PodPending
					p.Status.ContainerStatuses = []corev1.ContainerStatus{{
						Name: probeContainerName(p),
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff", Message: "manifest unknown",
						}},
					}}
				}
				return nil
			},
		}).Build()

	var out bytes.Buffer
	err := terminationCanary(context.Background(), stuck, "platform-worker", now, sleep, &out)
	if err == nil {
		t.Fatal("probes that never started produced a qualification")
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("the refusal must carry what the cluster said was wrong, or the operator is told only that "+
			"three minutes passed: %v", err)
	}

	var pods corev1.PodList
	if lerr := stuck.List(context.Background(), &pods); lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("%d probe Pod(s) survived a refused canary, held by a finalizer only this tool removes: the "+
			"next attempt collides with them and an operator has to patch them by hand", len(pods.Items))
	}
	// And the worker goes back, or a failed canary costs the lab a node.
	var worker corev1.Node
	if gerr := stuck.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &worker); gerr != nil {
		t.Fatalf("read the worker: %v", gerr)
	}
	if obs := observe(&worker); obs.HasLabel || len(obs.Taints) > 0 || obs.JournalRaw != "" {
		t.Fatalf("a refused canary kept the worker: %+v", obs)
	}
}

// A cluster that defaults something other than 30 seconds is not running this protocol, and the canary is
// where that becomes visible: nothing in this harness sets a grace period, so the default is what every Pod a
// run creates takes, and spine.go's horizon is arithmetic over it being 30.
//
// Mutation that turns this red: read the grace period out of the terminationGraceSec constant instead of out
// of the Create response. The probe then records 30 whatever the cluster did, and the reading that would have
// caught it agrees with itself.
func TestTheCanaryReadsTheGracePeriodTheClusterActuallyDefaulted(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, 700*time.Millisecond, 60*time.Second, honorExitCode, killExitCode)
	k.defaultGrace = 60
	c := canaryCluster(t, k)

	var out bytes.Buffer
	err := terminationCanary(context.Background(), c, "platform-worker", now, sleep, &out)
	if err == nil {
		t.Fatalf("a cluster defaulting a 60 second grace period qualified against a protocol timed on 30:\n%s",
			out.String())
	}
	q := readCanary(t, c)
	if q.Honor.GraceSec != 60 || q.Key.GraceSec != 60 {
		t.Fatalf("the canary recorded a %ds grace period on the probe and %ds in its key; both must be what "+
			"the cluster stored, not what this build wanted", q.Honor.GraceSec, q.Key.GraceSec)
	}
	if !strings.Contains(strings.Join(q.Failures, "\n"), "grace period") {
		t.Fatalf("the failure must name the grace period, got %v", q.Failures)
	}
}

// A probe still running when the budget expires is recorded as unobserved rather than as anything else, and
// the canary refuses on it. The alternative — treating a probe nobody watched to the end as fine — is the
// substitution this entire package exists to refuse.
//
// Mutation that turns this red: leave the budget-expiry branch of awaitProbesStopped without setting
// Unobserved. Terminated stays false and StoppedAfterMs stays 0, and 0ms passes the promptness threshold.
func TestAProbeThatOutlastsTheBudgetIsRecordedAsUnobserved(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	// Neither probe ever reports a terminal status inside the budget the canary allows.
	k := newFakeKubelet(now, time.Hour, time.Hour, honorExitCode, killExitCode)
	c := canaryCluster(t, k)

	err := terminationCanary(context.Background(), c, "platform-worker", now, sleep, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a canary that watched neither probe to its end qualified the worker")
	}
	q := readCanary(t, c)
	for _, p := range []canaryProbe{q.Honor, q.Ignore} {
		if p.Terminated {
			t.Fatalf("probe %s was recorded as terminated by a double that never terminated it", p.Pod)
		}
		if p.Unobserved == "" {
			t.Fatalf("probe %s carries no reason for having been unobserved, which is indistinguishable from "+
				"a probe that was never run at all", p.Pod)
		}
	}
}

// "Could not be read" and "would not stop" are different facts about a cluster and send an operator to
// different places, so a probe whose reads never succeed must not be recorded as one that outlasted its
// budget. The refusal is the same either way; the diagnosis is not.
//
// The blip half is the other side of it: a single failed read against a real apiserver must not throw away a
// probe that stopped perfectly well a moment later, which is what returning on the first error would do.
//
// Mutation that turns this red: set Unobserved on the first read error instead of keeping it (the blip half
// fails), or report the still-running message regardless of what the last read did (the unreadable half).
func TestAProbeThatCouldNotBeReadIsNotAProbeThatWouldNotStop(t *testing.T) {
	// Every read of a Pod fails from the moment the probes are deleted onward.
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, 500*time.Millisecond, 30*time.Second, honorExitCode, killExitCode)
	base := k.interceptors()
	var deleted bool
	unreadable := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node(nil, map[string]string{canaryAnnotationKey: ""})).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: base.Create,
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					deleted = true
				}
				return base.Delete(ctx, c, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok && deleted {
					return errors.New("etcdserver: request timed out")
				}
				return base.Get(ctx, c, key, obj, opts...)
			},
		}).Build()

	if err := terminationCanary(context.Background(), unreadable, "platform-worker", now, sleep,
		&bytes.Buffer{}); err == nil {
		t.Fatal("a canary that could not read either probe qualified the worker")
	}
	q := readCanary(t, unreadable)
	for _, p := range []canaryProbe{q.Honor, q.Ignore} {
		if !strings.Contains(p.Unobserved, "request timed out") {
			t.Fatalf("probe %s records %q; a probe nobody could read must not be reported as one that would "+
				"not stop", p.Pod, p.Unobserved)
		}
	}

	// And one blip in the middle of an otherwise healthy reading changes nothing.
	now2, sleep2 := fakeClock(time.Unix(0, 0))
	k2 := newFakeKubelet(now2, 500*time.Millisecond, 30*time.Second, honorExitCode, killExitCode)
	base2 := k2.interceptors()
	var reads int
	blippy := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node(nil, map[string]string{canaryAnnotationKey: ""})).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: base2.Create,
			Delete: base2.Delete,
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					reads++
					if reads == 3 {
						return errors.New("connection reset by peer")
					}
				}
				return base2.Get(ctx, c, key, obj, opts...)
			},
		}).Build()

	if err := terminationCanary(context.Background(), blippy, "platform-worker", now2, sleep2,
		&bytes.Buffer{}); err != nil {
		t.Fatalf("one failed read cost a healthy cluster its qualification: %v", err)
	}
}

// The delete must carry NO grace-period option, and nothing pinned that: passing
// client.GracePeriodSeconds(1) was green everywhere. It is the whole reason the probe leaves
// terminationGracePeriodSeconds unset in the first place — the Pod is stopped under the window the apiserver
// defaulted onto it, which is the window a run's Pods are stopped under. An override here would measure a
// number this harness never gives a workload, and would do it invisibly, since every threshold is a fraction
// of the grace period the probe was recorded with rather than of the one it was actually given.
//
// Mutation that turns this red: pass client.GracePeriodSeconds(anything) to the probe delete.
func TestTheProbesAreStoppedUnderTheGracePeriodTheClusterGaveThem(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	k := newFakeKubelet(now, 500*time.Millisecond, 30*time.Second, honorExitCode, killExitCode)
	base := k.interceptors()
	var overrides []int64
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node(nil, map[string]string{canaryAnnotationKey: ""})).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: base.Create,
			Get:    base.Get,
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					var o client.DeleteOptions
					o.ApplyOptions(opts)
					if o.GracePeriodSeconds != nil {
						overrides = append(overrides, *o.GracePeriodSeconds)
					}
				}
				return base.Delete(ctx, cl, obj, opts...)
			},
		}).Build()

	if err := terminationCanary(context.Background(), c, "platform-worker", now, sleep,
		&bytes.Buffer{}); err != nil {
		t.Fatalf("canary: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("the probe delete overrode the grace period with %v: the reading then describes a window no "+
			"run's Pod is ever given, and every threshold is a fraction of the recorded grace period rather "+
			"than of the one that was actually applied", overrides)
	}
}

// A Pod is phase Running before its container is, and the difference is the whole premise of the honouring
// probe: what has to be up before anything is signalled is the SHELL, with its trap installed. Signalling a
// container that has not started yet measures the start-up race instead of the termination contract — and it
// does so in the direction that flatters the honouring arm, since a container that never ran stops instantly.
//
// It is tested here rather than through the mode because the double sets phase and container state together,
// so no cluster this suite can build separates them; a real one does, in the ordinary course of starting a
// Pod.
//
// Mutation that turns this red: reduce probeRunning to `p.Status.Phase == corev1.PodRunning`.
func TestAProbeIsNotRunningUntilItsContainerIs(t *testing.T) {
	starting := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  probeTrainerContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
	}}
	if probeRunning(starting) {
		t.Fatal("a Pod whose container is still being created was treated as ready to be signalled: the trap " +
			"is not installed until the shell is up, so the honouring probe would be measured on a container " +
			"that never ran")
	}

	// No container statuses at all is the same state a moment earlier, and reads the same way.
	if probeRunning(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}) {
		t.Fatal("a Pod reporting no container status at all was treated as running")
	}

	up := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  probeTrainerContainer,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}}
	if !probeRunning(up) {
		t.Fatal("a Pod whose container is running was not treated as running, so no probe could ever start")
	}

	// And the phase still has to be Running: a container reported running inside a Pod the apiserver has not
	// moved out of Pending is a state to keep waiting through, not to signal.
	pending := up.DeepCopy()
	pending.Status.Phase = corev1.PodPending
	if probeRunning(pending) {
		t.Fatal("a Pending Pod was treated as running")
	}
}

// A probe that ended before anybody asked it to stop measured nothing, and carrying on would produce a
// "stopped promptly" reading for a container that was never signalled — the single most misleading output
// this file could generate, since it is indistinguishable from the healthy honouring result.
//
// The sleeper is rendered at ten minutes precisely so this cannot happen, which is why it is a refusal rather
// than a retry: reaching it means the probe died of something else.
//
// Mutation that turns this red: drop the Succeeded/Failed arm from awaitProbesRunning. The probe is then
// never "running", the start budget is spent in full, and the refusal blames a timeout for a container that
// had already exited and said why.
func TestAProbeThatEndedBeforeItWasAskedToStopRefusesImmediately(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: canaryNamespace, Name: "tc-early-honor"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if p, ok := obj.(*corev1.Pod); ok {
					p.Status.Phase = corev1.PodFailed
					p.Status.Reason = "Evicted"
				}
				return nil
			},
		}).Build()

	err := awaitProbesRunning(context.Background(), c, []*corev1.Pod{pod}, now, sleep)
	if err == nil {
		t.Fatal("a probe that had already ended was waited on as though it were starting")
	}
	if !strings.Contains(err.Error(), "before it was asked to stop") {
		t.Fatalf("the refusal must say the probe ended on its own rather than blaming a timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "Evicted") {
		t.Fatalf("the refusal must carry what the cluster said about it, got %v", err)
	}
	if got := now().Sub(time.Unix(0, 0)); got >= canaryStartBudget {
		t.Fatalf("the refusal spent %v, the whole start budget: a probe that has already ended is not "+
			"something to keep waiting for", got)
	}
}

// The two probes must be distinguishable by name, because everything downstream — the double above, an
// operator reading `kubectl get pods`, and the leaked-probe recovery hint — keys off the suffix.
//
// Mutation that turns this red: give both probes the same name. One Create then overwrites the other and the
// contrast is measured between a Pod and itself.
func TestTheTwoProbesAreNamedApart(t *testing.T) {
	honor, ignore := canaryProbeSpecs("0123456789abcdef", mustHarnessContract(t))
	if honor.name == ignore.name {
		t.Fatal("both probes carry the same name, so one Create would overwrite the other and the contrast " +
			"would be a Pod against itself")
	}
	for _, n := range []string{honor.name, ignore.name} {
		if len(n) > 63 || strings.ToLower(n) != n {
			t.Fatalf("probe name %q is not a DNS label, so the Create is refused before anything is measured", n)
		}
	}
	if !strings.HasSuffix(honor.name, "-honor") || !strings.HasSuffix(ignore.name, "-ignore") {
		t.Fatalf("the probes must say which contract they carry in their names: %q and %q", honor.name, ignore.name)
	}
}

// A reading that could not be recorded is a refusal, and the refusal has to say what is STILL ON THE NODE.
//
// This failure leaves the previous document untouched, and on a failing reading that is the dangerous case:
// the canary has just contradicted a qualification that is still standing, and runs will go on consulting it
// until somebody records or clears this. "The reading was taken but not recorded" leaves that to be worked
// out by whoever reads the terminal.
//
// Mutation that turns this red: drop the standing-document clause from the stamp failure, or report the stamp
// failure as a warning and return nil (which would report a cluster proven broken as qualified).
func TestAReadingThatCouldNotBeRecordedSaysWhatIsStillOnTheNode(t *testing.T) {
	now, sleep := fakeClock(time.Unix(0, 0))
	// A killing runtime: the reading fails, so the previously recorded document is the one that matters.
	k := newFakeKubelet(now, 300*time.Millisecond, 400*time.Millisecond, honorExitCode, honorExitCode)
	base := k.interceptors()
	var nodePatches int
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: base.Create,
			Get:    base.Get,
			Delete: base.Delete,
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					// 1 is the acquisition, 2 is the stamp, 3 is the release.
					nodePatches++
					if nodePatches == 2 {
						return errors.New("apiserver unreachable")
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	var out bytes.Buffer
	err := terminationCanary(context.Background(), c, "platform-worker", now, sleep, &out)
	if err == nil {
		t.Fatal("a reading that reached no cluster was reported as a qualification")
	}
	if !strings.Contains(err.Error(), "may now be false") {
		t.Fatalf("the refusal does not say that the qualification still standing on the node has just been "+
			"contradicted, which is the whole consequence of this failure: %v", err)
	}
	if nodePatches < 3 {
		t.Fatalf("the worker was not released after a failed stamp (%d node patches): a canary that could not "+
			"record its reading must still give the node back", nodePatches)
	}
	// The previous document is genuinely still there, which is what the sentence is about.
	var worker corev1.Node
	if gerr := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &worker); gerr != nil {
		t.Fatalf("read the worker: %v", gerr)
	}
	q, derr := decodeCanary(worker.Annotations[canaryAnnotationKey])
	if derr != nil {
		t.Fatalf("the previous qualification should be untouched, got %v", derr)
	}
	if len(q.Failures) != 0 {
		t.Fatal("this test proves nothing unless what survived is the PASSING document the failed reading " +
			"contradicted")
	}
}

// releaseProbes must survive a probe that is already gone. It runs from a defer on every path, including the
// ones where the probes were never created, and a cleanup that errored on NotFound would print an alarming
// recovery instruction for an object nobody has to recover.
//
// Mutation that turns this red: return the Get error from releaseProbe without testing for NotFound.
func TestReleasingAProbeThatIsAlreadyGoneIsNotAFailure(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	var out bytes.Buffer
	releaseProbes(context.Background(), c, []*corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Namespace: canaryNamespace, Name: "tc-none-honor"},
	}}, &out)
	if out.Len() != 0 {
		t.Fatalf("cleanup of an absent probe reported a problem: %s", out.String())
	}
}

// The reading has to be the TRAINER's, and this is the check that was missing when multi-container templates
// stopped being impossible.
//
// Both readers were written when the probe was one hand-built container, so "the first status carrying a
// terminated state" and "the trainer's ending" were the same sentence. Building the probe from the operator's
// template made them different, and nothing noticed: a sidecar that exits promptly supplies a terminal status
// this canary would record as the reading, and the exit code and stop latency in the document would describe a
// container the arms are not built out of.
//
// The direction is survivable rather than catastrophic — a prompt sidecar makes the reading look FASTER, both
// arms carry it, and judgeCanary's survival bound and exit-137 requirement turn that into a refusal rather than
// a false qualification — but "it fails safe" is not the same as "it measures the right container", and only
// one of those is worth recording on a node.
//
// Mutation that turns this red: return the first status carrying a terminal state from terminatedState (the
// first case), or accept any running container in probeRunning (the second).
func TestTheReadingIsTheTrainersAndNotWhicheverContainerAnswersFirst(t *testing.T) {
	// A sidecar that finished immediately, in front of a trainer that is still going.
	withSidecarDone := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed",
			}}},
			{Name: probeTrainerContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	}}
	if term := terminatedState(withSidecarDone); term != nil {
		t.Fatalf("a sidecar's ending was read as the probe's (exit %d, %q) while the trainer was still running; "+
			"that exit code and the latency measured to it would go into the document as the reading",
			term.ExitCode, term.Reason)
	}

	// And the trainer's own ending is still read, or the fix above would simply have stopped reading anything.
	withTrainerDone := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed",
			}}},
			{Name: probeTrainerContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: honorExitCode, Reason: "Error",
			}}},
		},
	}}
	term := terminatedState(withTrainerDone)
	if term == nil || term.ExitCode != honorExitCode {
		t.Fatalf("the trainer's own ending was not read: %+v", term)
	}

	// The start-up race, which is the other half and the more dangerous one: deleting a Pod whose trainer has not
	// started measures the race rather than the contract, and it does it in the direction that flatters the
	// honouring arm.
	sidecarUpTrainerStarting := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: probeTrainerContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ContainerCreating",
			}}},
		},
	}}
	if probeRunning(sidecarUpTrainerStarting) {
		t.Fatal("a Pod whose sidecar was up and whose trainer was still being created was treated as ready to " +
			"be signalled: the trap is not installed until the trainer's shell is, so the honouring probe would " +
			"be measured on a container that had not started")
	}

	bothUp := sidecarUpTrainerStarting.DeepCopy()
	bothUp.Status.ContainerStatuses[1].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if !probeRunning(bothUp) {
		t.Fatal("a Pod whose trainer is running was not treated as running, so no probe could ever start")
	}

	// A Pod with no trainer status at all is neither running nor terminated, rather than either by default.
	none := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	}}
	if probeRunning(none) || terminatedState(none) != nil {
		t.Fatal("a Pod publishing no status for the trainer was read as one that had started or stopped")
	}
}
