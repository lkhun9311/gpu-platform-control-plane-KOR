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
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file takes the reading canary.go judges. It creates two Pods on the worker that differ only in the one
// shell command the arms differ in, asks both to stop at once, and watches what each of them does about it.

const (
	// canaryFinalizer keeps the probe Pod's OBJECT alive after its containers are gone, and it is the reason
	// this measurement is deterministic rather than a race.
	//
	// A Pod deleted without one disappears shortly after the kubelet writes its terminal status: the exit code
	// this canary exists to read is published and then garbage-collected, and a poller can lose the race. The
	// finalizer blocks only removal from etcd — the kubelet still receives the deletion, still sends SIGTERM at
	// the deletionTimestamp and still SIGKILLs at the deadline — so it changes nothing about what is being
	// measured and everything about whether it can be read afterwards. teardown_kind_test.go holds a Pod the
	// same way and for the same kind of reason.
	//
	// What it costs is one cleanup obligation, discharged by releaseProbes on every exit path. A canary killed
	// between the delete and that cleanup leaves two terminated Pod objects in the canary namespace holding no
	// device and no CPU; the refusal below says how to clear them.
	canaryFinalizer = "queuelab.gpu-platform/canary-hold"

	// canaryPollInterval is how often the probes' state is read.
	//
	// It is well under a tenth of the promptness threshold, because this poll is what MEASURES that threshold:
	// an interval anywhere near it would put the measurement's own granularity inside the number being
	// compared.
	canaryPollInterval = 250 * time.Millisecond

	// canaryStartBudget bounds waiting for both probes to be running.
	//
	// The pinned sleeper is a multi-arch busybox that may not be on the node yet, so this has to cover an image
	// pull on a cold cluster and not merely a container start. Exceeding it is a refusal that names the Pod's
	// own phase and waiting reason, which is where an operator meets an ImagePullBackOff or a full node.
	canaryStartBudget = 3 * time.Minute

	// canaryStopSlack is what the stop budget adds on top of the grace period the cluster actually defaulted.
	//
	// The ignoring probe is expected to spend the WHOLE grace period and then be killed, so a budget of exactly
	// the grace period would be a race against the thing being measured; the slack covers the kubelet noticing
	// the deadline, the runtime killing, and the status making it back to the apiserver.
	canaryStopSlack = 30 * time.Second

	// canaryStopBudgetMax is the ceiling on that budget, and it exists for the case where the cluster's default
	// grace period is not the 30 seconds this protocol is timed against at all. Without it a cluster defaulting
	// ten minutes would hold this process for ten minutes before reporting a fact that was already visible on
	// the created Pod: judgeCanary refuses the wrong grace period whatever the probes went on to do.
	canaryStopBudgetMax = 3 * time.Minute

	// canaryModeTimeout bounds the whole mode, and is deliberately larger than the sum of the budgets above so
	// that it is never the binding constraint — the same relationship teardownContextTimeout has to
	// teardownBudget, and for the same reason. What decides has to be the loop that can report why it gave up,
	// not a context expiring underneath it and turning every subsequent read into a cancellation.
	canaryModeTimeout = canaryStartBudget + canaryStopBudgetMax + 4*time.Minute
)

// canaryRunID is the run id a canary's ownership transaction is taken under.
//
// It becomes the Node's dedication label value and its taint value, so it has to be a valid label value; it is
// derived from the canary id so that an operator meeting a held node in -inspect-worker can match the journal
// against the qualification the canary went on to write.
func canaryRunID(canaryID string) string {
	short := canaryID
	if len(short) > 8 {
		short = short[:8]
	}
	return "canary-" + short
}

// probeSpec is one half of the contrast: a name, a contract and the command that expresses it.
type probeSpec struct {
	name     string
	contract string
	command  []string
}

// canaryProbeSpecs names the two probes for one canary attempt.
//
// The two names differ only in their suffix so that a leaked pair is obviously one pair, and both carry the
// canary id so a leftover Pod can be traced to the invocation that made it.
func canaryProbeSpecs(canaryID string, c canaryContract) (honor, ignore probeSpec) {
	short := canaryID
	if len(short) > 8 {
		short = short[:8]
	}
	return probeSpec{name: "tc-" + short + "-honor", contract: "HonorsSIGTERM", command: c.HonorCommand},
		probeSpec{name: "tc-" + short + "-ignore", contract: "IgnoresSIGTERM", command: c.IgnoreCommand}
}

// probeTrainerContainer is the container the operator renders a training job's workload into.
//
// The probe finds the container BY NAME rather than by index, because the index is what would go wrong
// quietly: a template that grew a sidecar in front of the trainer would have the arm's command written into
// the sidecar, and the reading would be of a workload nobody meant to probe.
const probeTrainerContainer = "trainer"

// canaryPod builds one probe Pod out of the Pod template the operator would actually render.
//
// The template is fetched from the same place the key's hash is taken (renderedPodTemplate at
// templateProbeJob), so the thing hashed and the thing run are one rendering. That is what closes the gap the
// hash alone left: a hash detects a change and then refuses, and before this the re-take that follows the
// refusal probed a Pod without the change in it. Detection with hollow remediation is worse than no detection,
// because it looks like a check. Now a preStop hook added to the template is in the Pod this creates, and the
// re-take measures it.
func canaryPod(canaryID, node, runID string, contract canaryContract, p probeSpec) (*corev1.Pod, error) {
	return probePodFrom(renderedPodTemplate(templateProbeJob()), canaryID, node, runID, contract, p)
}

// probePodFrom turns one rendered Pod template into one probe Pod.
//
// It takes the template as a parameter rather than fetching it, so a test can drive a template this build does
// not render — a preStop hook, an explicit grace period — through the real transformation, instead of asserting
// against what the transformation itself produced.
//
// The template is adopted WHOLE and three things are changed, each of which is a decision rather than a tidy-up:
//
// The WORKLOAD is replaced. Image and command come from the contract and the probe spec, because the mechanism
// under test is the honouring/ignoring pair and the template's own image is a sentinel that does not exist. The
// probe adopts the template's SHAPE, not its workload; both replaced values are key fields in their own right,
// so nothing about them is being taken on trust here.
//
// The DEVICE REQUEST is dropped from EVERY container, and this is the one place where what is hashed and what
// is run differ, so it has to be argued rather than assumed. spec.nodeName removes the scheduler but NOT the
// kubelet's admission: a Pod asking for more of an extended resource than the node advertises is rejected
// outright, and the sentinel's gpuCount is 7 because 7 is distinct from the other two numbers in the probe job,
// not because any run needs seven devices. Keeping it would make a worker unqualifiable unless it had seven free
// devices, and it would make a node whose devices are legitimately held fail the canary for a reason that has
// nothing to do with signal delivery. Allocation is not what this measures. Only the device is removed — a cpu
// or memory limit the template later grows is left in place and probed, because those DO shape what the kubelet
// does to a container. If the operator ever renames the device resource, this stops matching it and the probe
// asks for something the node does not have: that is a loud refusal at the start budget, not a silent one, and
// the hash will have changed anyway.
//
// IDENTITY AND PLACEMENT are added. spec.nodeName is set rather than a nodeSelector because what is being
// qualified is THIS node's kubelet and THIS node's runtime, so the probe must not be allowed to land elsewhere.
// The toleration is appended to whatever the template carries rather than replacing it, and it is the same
// shape internal/queuelab's ResourceFlavor gives the arm's own Pods, so the probe is admitted under the rule
// the workload it stands for is. The finalizer is what keeps the exit code readable after the container stops,
// and it is the one overlay that replaces rather than merges; the reason is at the assignment.
//
// A MULTI-CONTAINER template is adopted rather than refused, and the alternative is worth stating because it is
// arguably the more conservative reading of this function's own refuse-rather-than-guess rule. A sidecar in the
// template is something a RUN's Pod would carry, and it is not inert to what this canary measures: deleting a
// Pod signals every container in it, and under RestartPolicyNever the Pod is not finished until all of them
// stop, so a sidecar that ignores SIGTERM changes the shutdown a run actually gets. Refusing it would make a
// template the operator supports unqualifiable and would throw that fidelity away. What must not be guessed is
// WHICH container's ending is the reading, and the trainer's name is what settles that — here, and in
// terminatedState and probeRunning, which read the same name for the same reason.
//
// terminationGracePeriodSeconds is still not pinned HERE, and the reason is unchanged even though the source of
// the value has moved: a run's Pods take whatever the apiserver defaults, and four separate pieces of the
// protocol's arithmetic assume that default is 30. What HAS changed is that if the template ever pins one, the
// probe now carries it and the reading is taken under it — which is the whole point — and judgeCanary refuses
// anything but terminationGraceSec, so the change surfaces as a refusal rather than as a quietly different
// experiment.
func probePodFrom(tpl corev1.PodTemplateSpec, canaryID, node, runID string, contract canaryContract,
	p probeSpec) (*corev1.Pod, error) {
	spec := *tpl.Spec.DeepCopy()
	trainer := -1
	for i := range spec.Containers {
		if spec.Containers[i].Name == probeTrainerContainer {
			trainer = i
			break
		}
	}
	if trainer < 0 {
		// Refused rather than guessed. Writing the arm's command into whatever container happened to be there
		// would produce a reading of something nobody chose, and a canary that cannot say which container is the
		// workload cannot say what it qualified.
		names := make([]string, 0, len(spec.Containers))
		for i := range spec.Containers {
			names = append(names, spec.Containers[i].Name)
		}
		return nil, fmt.Errorf(
			"the operator's pod template has no %q container (it has %v), so this canary cannot tell which "+
				"container a run's workload would be, and cannot put the arm's command anywhere with confidence",
			probeTrainerContainer, names)
	}
	spec.Containers[trainer].Image = contract.Image
	spec.Containers[trainer].Command = p.command
	// Args is cleared because Command and Args are one unit: Command is the entrypoint and Args is what
	// follows it, so replacing one and adopting the other leaves half of a pair and is not a replacement at
	// all. BuildJob renders no Args today, which is exactly why this is worth writing down rather than
	// leaving to be discovered — if it ever renders mltj.Spec.Args, the template hash changes and forces a
	// re-take, and the re-taken probe would run `sh -c '<arm command>' <template args>`, where those args
	// become $0 and $1 to the shell. That is a different process shape from the arm's, and canaryProbe
	// records only p.command, so the qualification would name a command line nobody ran. The mechanism would
	// have detected the drift and then qualified the wrong thing, which is the hollow remediation this file
	// argues against elsewhere.
	spec.Containers[trainer].Args = nil
	// EVERY container, not just the trainer, because the admission this is avoiding is per-POD: the kubelet adds
	// up what the whole Pod asks for and rejects it against the node's allocatable, so a sidecar holding a device
	// request makes the probe unschedulable exactly as the trainer's would. Stripping only the container whose
	// command was replaced would have left the strip working for the shape of template that exists today and
	// silently not working for the one this commit made possible.
	//
	// Requests as well as Limits: BuildJob writes only Limits today, but a template that spelled the device as a
	// request would be admitted against the same allocatable, and a strip that covered one of the two fields
	// would be a guard that looks complete.
	for i := range spec.Containers {
		delete(spec.Containers[i].Resources.Limits, gpuResourceName)
		delete(spec.Containers[i].Resources.Requests, gpuResourceName)
		// Emptied maps are dropped rather than sent as `{}`, so the Pod this creates is the template's shape and
		// not the template's shape plus an artefact of the removal.
		if len(spec.Containers[i].Resources.Limits) == 0 {
			spec.Containers[i].Resources.Limits = nil
		}
		if len(spec.Containers[i].Resources.Requests) == 0 {
			spec.Containers[i].Resources.Requests = nil
		}
	}
	spec.NodeName = node
	spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
		Key:      workerTaintKey,
		Operator: corev1.TolerationOpEqual,
		Value:    runID,
		Effect:   corev1.TaintEffectNoSchedule,
	})

	meta := *tpl.ObjectMeta.DeepCopy()
	meta.Namespace = canaryNamespace
	meta.Name = p.name
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	// Merged into the template's own labels rather than replacing them, for the reason the toleration is
	// appended: a label the operator puts on the template is part of what a run's Pod carries.
	meta.Labels["queuelab.gpu-platform/termination-canary"] = canaryID
	meta.Labels["queuelab.gpu-platform/contract"] = p.contract
	// The canary's finalizer REPLACES whatever the template carries, and this is the one overlay that does not
	// merge. The reason is not symmetry with the others, it is what a finalizer does: it decides when the object
	// disappears, not what the kubelet does to the containers, so adopting one adds nothing to a reading about
	// signal delivery. What it would add is a probe nobody can clear — releaseProbes removes the finalizer it
	// installed and no other, so a foreign one would strand a Pod object in the canary namespace and the refusal
	// printed for it names a command that would not be enough.
	//
	// The template's own finalizer is still KEYED: podTemplateHashOf hashes the whole PodTemplateSpec, metadata
	// included, so a template that grows one invalidates every reading taken before it. It is noticed and
	// deliberately not adopted, which is a different thing from not noticed.
	meta.Finalizers = []string{canaryFinalizer}
	return &corev1.Pod{ObjectMeta: meta, Spec: spec}, nil
}

// ensureCanaryNamespace creates the canary's namespace if it is not already there.
//
// It accepts one it did not create, unlike ensureNamespace, and the difference is not laxity: a run's
// namespace IS its unit of teardown, so a run inside somebody else's namespace cannot clean up after itself,
// while this namespace is only a container for two Pods that are named per attempt, created by name and
// deleted by name. Nothing about the canary's verdict depends on who made the namespace.
//
// It is also never deleted. An empty namespace costs nothing, and deleting one is the asynchronous,
// finalizer-bound operation this repository has a whole file of teardown machinery for — none of which a
// recovery tool should be dragging in to tidy up after itself.
func ensureCanaryNamespace(ctx context.Context, c client.Client) error {
	err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   canaryNamespace,
		Labels: map[string]string{"queuelab.gpu-platform/purpose": "termination-canary"},
	}})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create the canary namespace %s: %w", canaryNamespace, err)
}

// trainerStatus is the status of the container the arm's command is in, or nil while the cluster has published
// none for it.
//
// Both readers below go through this, and the reason is a change of premise rather than a tidy-up. They were
// written when the probe was one hand-built container, so "the first status carrying X" and "the trainer's
// status" were the same sentence. Building the probe from the operator's template made a MULTI-CONTAINER probe
// possible, and the two stopped being the same sentence the moment that landed: a sidecar that exits promptly
// would supply the terminal status this canary records as the reading, and a sidecar that is up while the
// trainer is still starting would satisfy "something is running".
//
// That is the same argument probeTrainerContainer already makes one level up — a reading of a workload nobody
// meant to probe — and it was left unapplied here. Matching by name is what keeps the two levels consistent.
//
// It returns nil rather than an error for a missing status because an empty list and a list whose trainer entry
// is still Waiting are ordinary intermediate states, not failures: the budget in awaitProbesStopped is what
// decides when waiting has gone on too long, and it records that as Unobserved with the reason.
func trainerStatus(p *corev1.Pod) *corev1.ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == probeTrainerContainer {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// terminatedState returns the trainer's terminal status, or nil while it has none.
//
// The exit code and the stop latency this canary records are the whole reading, so they have to be the
// TRAINER's: an exit code from a container running something else is a number about a workload the arms are not
// built out of, and it would look entirely reasonable in the record.
func terminatedState(p *corev1.Pod) *corev1.ContainerStateTerminated {
	if s := trainerStatus(p); s != nil {
		return s.State.Terminated
	}
	return nil
}

// probeRunning reports whether the probe's trainer container is actually executing.
//
// The phase alone is not enough. What has to be true before the delete is that the SHELL IS RUNNING with its
// trap installed: signalling a Pod whose container has not started yet would measure the start-up race rather
// than the termination contract, and would do it in the direction that makes the honouring probe look good.
//
// So it is the trainer's own Running state that is required, not any container's. A sidecar that came up first
// says nothing about whether the trap exists yet, and accepting it would re-open exactly the race this function
// was written to close.
func probeRunning(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	s := trainerStatus(p)
	return s != nil && s.State.Running != nil
}

// probeTrouble describes why a probe is not running yet, in the words the cluster used.
//
// It exists so the start-budget refusal names an ImagePullBackOff or an admission failure instead of saying
// only that something did not happen in three minutes, which is the difference between a fixable report and a
// rerun.
func probeTrouble(p *corev1.Pod) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("phase %s", p.Status.Phase))
	if p.Status.Reason != "" {
		parts = append(parts, fmt.Sprintf("reason %q", p.Status.Reason))
	}
	for i := range p.Status.ContainerStatuses {
		if w := p.Status.ContainerStatuses[i].State.Waiting; w != nil {
			parts = append(parts, fmt.Sprintf("container waiting: %q %q", w.Reason, w.Message))
		}
	}
	return strings.Join(parts, ", ")
}

// awaitProbesRunning blocks until both probes are executing, or refuses with what the cluster said.
func awaitProbesRunning(ctx context.Context, c client.Client, pods []*corev1.Pod,
	now func() time.Time, sleep func(time.Duration)) error {
	deadline := now().Add(canaryStartBudget)
	for {
		allRunning := true
		var trouble []string
		for _, p := range pods {
			var got corev1.Pod
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
				// Retried rather than returned, for the reason the stop loop below keeps its last error: a single
				// blip against the apiserver would otherwise throw away a probe that is starting perfectly well,
				// and the budget is what decides. A read that keeps failing spends the budget and is reported as
				// the trouble it is.
				allRunning = false
				trouble = append(trouble, fmt.Sprintf("%s: could not be read: %v", p.Name, err))
				continue
			}
			switch {
			case got.Status.Phase == corev1.PodSucceeded || got.Status.Phase == corev1.PodFailed:
				// A probe that ended before anybody asked it to stop measured nothing at all, and carrying on
				// would produce a "stopped promptly" reading for a container that was never signalled.
				return fmt.Errorf("probe %s reached phase %s before it was asked to stop (%s); its sleeper is "+
					"meant to outlast the whole probe", p.Name, got.Status.Phase, probeTrouble(&got))
			case !probeRunning(&got):
				allRunning = false
				trouble = append(trouble, fmt.Sprintf("%s: %s", p.Name, probeTrouble(&got)))
			}
		}
		if allRunning {
			return nil
		}
		if !now().Before(deadline) {
			return fmt.Errorf("the probes were not both running within %s: %s",
				canaryStartBudget, strings.Join(trouble, "; "))
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for the probes to start: %w", err)
		}
		sleep(canaryPollInterval)
	}
}

// deleteProbe asks one probe to stop and returns the instant from which its stopping is measured.
//
// No GracePeriodSeconds option is passed, deliberately: the delete has to use the Pod's own defaulted grace
// period, because that is what a run's Pods are deleted under. Overriding it here would measure a window this
// harness never actually gives a workload.
func deleteProbe(ctx context.Context, c client.Client, p *corev1.Pod, now func() time.Time) (time.Time, error) {
	if err := c.Delete(ctx, p); err != nil {
		return time.Time{}, fmt.Errorf("ask probe %s to stop: %w", p.Name, err)
	}
	// Stamped after the call rather than before it, so the probe is not charged for the round trip: by the time
	// Delete returns the apiserver has set the deletionTimestamp, which is the earliest instant from which the
	// kubelet could have been told anything. The same rule is used for both probes, so the contrast between
	// them — which is what the verdict actually turns on — is unaffected either way.
	return now(), nil
}

// awaitProbesStopped watches both probes until each has a terminal container status or the budget runs out.
//
// The budget is derived from the grace period the cluster ACTUALLY defaulted rather than from the constant,
// because a probe that was given five minutes to shut down cannot be expected to finish inside thirty
// seconds; the ceiling is what stops that reasoning turning into an unbounded wait. A probe still running when
// the budget expires is recorded as unobserved rather than as anything else, and judgeCanary refuses on it.
func awaitProbesStopped(ctx context.Context, c client.Client, probes []*canaryProbe, pods []*corev1.Pod,
	t0 []time.Time, graceSec int64, now func() time.Time, sleep func(time.Duration)) {
	budget := min(time.Duration(graceSec)*time.Second+canaryStopSlack, canaryStopBudgetMax)
	deadline := now().Add(budget)
	// The last read failure per probe, kept rather than acted on immediately: one blip against the apiserver
	// must not end a reading that is going fine, but a read that never succeeds has to be what the record says
	// happened rather than being reported as a probe that would not stop. The same reasoning
	// resolveAmbiguousAcquire's lastReadErr follows, including clearing it on a read that works.
	lastReadErr := make([]error, len(pods))
	for {
		done := true
		for i, p := range pods {
			if probes[i].Terminated || probes[i].Unobserved != "" {
				continue
			}
			var got corev1.Pod
			err := c.Get(ctx, client.ObjectKeyFromObject(p), &got)
			switch {
			case apierrors.IsNotFound(err):
				// The finalizer this canary installed is what keeps the object readable after its containers
				// stop, so a Pod that vanished anyway had that finalizer removed by something else — and with it
				// went the exit code, which cannot be recovered from anywhere. Not retried, unlike the error
				// below: nothing is coming back.
				probes[i].Unobserved = "the Pod object was removed before any terminal container status could be " +
					"read; its canary finalizer must have been stripped by something other than this canary"
			case err != nil:
				lastReadErr[i] = err
				done = false
			default:
				lastReadErr[i] = nil
				if term := terminatedState(&got); term != nil {
					probes[i].Terminated = true
					probes[i].ExitCode = term.ExitCode
					probes[i].Reason = term.Reason
					probes[i].StoppedAfterMs = now().Sub(t0[i]).Milliseconds()
					continue
				}
				done = false
			}
		}
		if done {
			return
		}
		if !now().Before(deadline) {
			for i := range probes {
				if probes[i].Terminated || probes[i].Unobserved != "" {
					continue
				}
				if lastReadErr[i] != nil {
					// "Could not be read" and "would not stop" send an operator to completely different places,
					// and only one of them is about this cluster's ability to stop a Pod.
					probes[i].Unobserved = fmt.Sprintf(
						"it could not be read for the whole %s this canary waits; the last attempt failed with: %v",
						budget, lastReadErr[i])
					continue
				}
				probes[i].Unobserved = fmt.Sprintf(
					"it was still running %s after it was asked to stop, which is the whole budget this canary "+
						"waits (a %ds grace period plus %s)", budget, graceSec, canaryStopSlack)
			}
			return
		}
		if err := ctx.Err(); err != nil {
			for i := range probes {
				if !probes[i].Terminated && probes[i].Unobserved == "" {
					probes[i].Unobserved = fmt.Sprintf("watching it stop was cancelled: %v", err)
				}
			}
			return
		}
		sleep(canaryPollInterval)
	}
}

// releaseProbes removes the canary's finalizer from every probe and deletes what is left.
//
// It runs on every exit path, including the refusals, because the finalizer is the one thing this mode leaves
// behind that does not clean itself up. Each failure is reported and none of them stops the others: a probe
// that could not be released must not take the second one's cleanup down with it.
//
// The finalizer is removed before the delete is issued rather than after, so the two orders a crash can
// interrupt are the same one: a Pod with the finalizer already gone is deleted by the outstanding deletion the
// probe was already under, and one that was never deleted is deleted here.
func releaseProbes(ctx context.Context, c client.Client, pods []*corev1.Pod, out io.Writer) {
	for _, p := range pods {
		if err := releaseProbe(ctx, c, p); err != nil {
			_, _ = fmt.Fprintf(out, "CANARY PROBE NOT CLEARED: %s/%s: %v\n"+
				"  it holds no device and no CPU, but its object will not go away while this canary's finalizer "+
				"is on it:\n"+
				"    kubectl -n %s patch pod %s --type=json -p '[{\"op\":\"remove\",\"path\":\"/metadata/finalizers\"}]'\n",
				canaryNamespace, p.Name, err, canaryNamespace, p.Name)
		}
	}
}

func releaseProbe(ctx context.Context, c client.Client, p *corev1.Pod) error {
	for range acquireAttempts {
		var got corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		base := got.DeepCopy()
		kept := make([]string, 0, len(got.Finalizers))
		for _, f := range got.Finalizers {
			if f != canaryFinalizer {
				kept = append(kept, f)
			}
		}
		if len(kept) != len(got.Finalizers) {
			got.Finalizers = kept
			err := c.Patch(ctx, &got, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
			switch {
			case apierrors.IsNotFound(err):
				return nil
			case apierrors.IsConflict(err):
				// Somebody wrote the Pod between the read and the patch — the kubelet publishing a status is the
				// ordinary case — so re-read and decide again rather than forcing a stale copy over it.
				continue
			case err != nil:
				return fmt.Errorf("remove the canary finalizer: %w", err)
			}
		}
		if err := c.Delete(ctx, &got); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the probe: %w", err)
		}
		return nil
	}
	return fmt.Errorf("remove the canary finalizer: %d conflicts, giving up", acquireAttempts)
}

// stampCanary records the qualification on the worker itself.
//
// It re-reads and re-verifies ownership before writing, for the reason stampResidue does: a status code proves
// the apiserver accepted a request, not that the node is still the one this transaction acquired, and writing
// a qualification onto somebody else's worker would be a document claiming a reading that was taken elsewhere.
// verifyObserved rather than verifyInstalled, because the marker values are the run id and a different
// transaction under the same run id would carry byte-identical ones.
func stampCanary(ctx context.Context, c client.Client, j journal, q canaryQualification) error {
	raw, err := encodeCanary(q)
	if err != nil {
		return err
	}
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: j.Node}, &n); err != nil {
		return fmt.Errorf("get node %s: %w", j.Node, err)
	}
	obs := observe(&n)
	if err := verifyObserved(obs, j); err != nil {
		return fmt.Errorf("record the canary on %s: %w", j.Node, err)
	}
	base := n.DeepCopy()
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[canaryAnnotationKey] = raw
	if err := c.Patch(ctx, &n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("record the canary on %s (state changed under you, re-inspect): %w", j.Node, err)
	}
	return nil
}

// terminationCanary takes the reading and records it on the worker.
//
// It ACQUIRES the worker through the ordinary ownership transaction rather than just creating Pods on it, and
// that is not ceremony. Two probe Pods appearing on a node in the middle of somebody's measured run would be
// exactly the foreign contamination the capacity gate refuses runs over, and a run starting underneath a
// canary would find the node's grace period being exercised by somebody else. The transaction makes both
// impossible, refuses with a name when the node is already held, and gives an operator the same
// -inspect-worker recovery for a crashed canary that they already have for a crashed run.
//
// The verdict is written to the Node whether it passed or failed. A cluster proven unable to stop a Pod is the
// most valuable thing this can record, and leaving a previous PASSING qualification standing after proving the
// opposite would be the worst outcome available: runs would keep consulting a document this canary had just
// falsified.
func terminationCanary(ctx context.Context, c client.Client, nodeName string,
	now func() time.Time, sleep func(time.Duration), out io.Writer) (err error) {
	contract, cerr := harnessTerminationContract()
	if cerr != nil {
		return fmt.Errorf("build the canary's termination contract: %w", cerr)
	}
	canaryID := string(uuid.NewUUID())
	runID := canaryRunID(canaryID)

	// The canary's journal says what the canary actually leaves behind: two Pods whose names derive from this
	// id, in a namespace it SHARES and must never delete. It carries no study or variant, and decodeJournal
	// refuses a canary journal that has them — an earlier version invented both to satisfy a non-empty check,
	// which made the document look recoverable while enumerate failed on it every time.
	j, aerr := acquireWorker(ctx, c, nodeName, ownerIdentity{
		Kind:      ownerCanary,
		TxID:      newTxID(),
		RunID:     runID,
		Arm:       canaryArm,
		Namespace: canaryNamespace,
		CanaryID:  canaryID,
	})
	if aerr != nil {
		return fmt.Errorf("acquire worker %s to canary it: %w", nodeName, aerr)
	}
	_, _ = fmt.Fprintf(out, "worker %s acquired for canary %s: tx=%s\n"+
		"  (if this process dies, run: queuelabrun -inspect-worker -worker %s)\n",
		nodeName, canaryID, j.TxID, nodeName)
	defer func() {
		// On cleanupContext for the reason every release path in this package is: a signal arriving here must
		// not be what decides whether the worker goes back.
		relCtx, relCancel := cleanupContext()
		defer relCancel()
		rerr := releaseOwned(relCtx, c, j)
		if rerr == nil {
			_, _ = fmt.Fprintf(out, "worker %s released\n", nodeName)
			return
		}
		_, _ = fmt.Fprintf(out, "WORKER NOT RESTORED: %v\n  run: queuelabrun -inspect-worker -worker %s\n", rerr, nodeName)
		if err == nil {
			// Only when nothing else already failed: a canary that refused AND could not give the worker back is
			// still primarily a refused canary, and overwriting its reason would hide what it found.
			err = fmt.Errorf("release worker %s after the canary: %w", nodeName, rerr)
		}
	}()

	// Read AFTER acquiring, so the kubelet and runtime the key records are the ones on the node this
	// transaction actually holds.
	var n corev1.Node
	if gerr := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); gerr != nil {
		return fmt.Errorf("read node %s to key the canary: %w", nodeName, gerr)
	}
	if err := ensureCanaryNamespace(ctx, c); err != nil {
		return err
	}

	honorSpec, ignoreSpec := canaryProbeSpecs(canaryID, contract)
	// Both Pods are built before either is created, so a template this canary cannot probe refuses with nothing
	// of its own on the cluster: the worker's release is already deferred above, and no probe exists to leak.
	var pods []*corev1.Pod
	for _, ps := range []probeSpec{honorSpec, ignoreSpec} {
		pod, perr := canaryPod(canaryID, nodeName, runID, contract, ps)
		if perr != nil {
			return fmt.Errorf("canary on %s: %w", nodeName, perr)
		}
		pods = append(pods, pod)
	}
	probes := []*canaryProbe{
		{Pod: honorSpec.name, Contract: honorSpec.contract, Command: honorSpec.command},
		{Pod: ignoreSpec.name, Contract: ignoreSpec.contract, Command: ignoreSpec.command},
	}
	defer func() {
		relCtx, relCancel := cleanupContext()
		defer relCancel()
		releaseProbes(relCtx, c, pods, out)
	}()

	for i, p := range pods {
		if cerr := c.Create(ctx, p); cerr != nil {
			return fmt.Errorf("create probe %s: %w", p.Name, cerr)
		}
		// The Create RESPONSE is the persisted object — the typed client decodes it back into the pointer it was
		// given — so this is the grace period the apiserver defaulted, not the one that was asked for. Reading
		// it here rather than with a second Get is the same argument ensureNamespace makes about its stamp.
		if p.Spec.TerminationGracePeriodSeconds != nil {
			probes[i].GraceSec = *p.Spec.TerminationGracePeriodSeconds
		}
		_, _ = fmt.Fprintf(out, "  probe %s created (%s, grace %ds): %s\n",
			p.Name, probes[i].Contract, probes[i].GraceSec, strings.Join(probes[i].Command, " "))
	}

	if werr := awaitProbesRunning(ctx, c, pods, now, sleep); werr != nil {
		return fmt.Errorf("canary on %s: %w", nodeName, werr)
	}
	_, _ = fmt.Fprintf(out, "  both probes running; asking both to stop\n")

	t0 := make([]time.Time, len(pods))
	for i, p := range pods {
		stamp, derr := deleteProbe(ctx, c, p, now)
		if derr != nil {
			return fmt.Errorf("canary on %s: %w", nodeName, derr)
		}
		t0[i] = stamp
	}
	// The observed grace period rather than the constant, so a cluster that defaults something else is waited
	// out and reported rather than timed out and reported as a stall. Both probes carry the same default; the
	// first is taken because judgeCanary is what refuses them if they are not what this protocol needs.
	awaitProbesStopped(ctx, c, probes, pods, t0, probes[0].GraceSec, now, sleep)

	q := canaryQualification{
		Schema:          canarySchema,
		CanaryID:        canaryID,
		Node:            nodeName,
		QualifiedAt:     nowStamp(),
		HarnessRevision: harnessRevision(),
		Key: canaryKey{
			Image:         contract.Image,
			HonorCommand:  contract.HonorCommand,
			IgnoreCommand: contract.IgnoreCommand,
			// The OBSERVED default, not the contract's requirement: this document says what was measured, and
			// canaryKeyFor is what turns the requirement into a comparison at consult time.
			GraceSec:         probes[0].GraceSec,
			HonorExitCode:    contract.HonorExitCode,
			NodeUID:          string(n.UID),
			KubeletVersion:   n.Status.NodeInfo.KubeletVersion,
			ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
			// The contract's, like the image and the commands above and unlike the grace period: this is what the
			// build that took the reading renders, not something observed on the cluster, so it is recorded and
			// compared out of the same place on both sides.
			PodTemplateHash: contract.PodTemplateHash,
			// Observed, like the grace period and unlike the template hash: it is which manager image is
			// running on this cluster right now. A reading taken under one digest must not qualify a run
			// under another, and recording it here is the half that makes canaryKeyFor's comparison possible.
			OperatorImageID: operatorImageIDFrom(ctx, c),
		},
		Honor:    *probes[0],
		Ignore:   *probes[1],
		Failures: judgeCanary(*probes[0], *probes[1]),
	}
	reportCanary(out, q)
	// On cleanupContext, not ctx, and it is the only write in this mode that needs saying twice. Every other
	// thing this function does is reversible or self-clearing — probes are deleted, the worker is released —
	// but the stamp is the entire product, and a signal or an expiring mode budget landing here would throw
	// away a reading whose probes have already been spent. It is the same argument the release path makes,
	// applied to the one write whose loss cannot be recovered by re-reading anything.
	stampCtx, stampCancel := cleanupContext()
	serr := stampCanary(stampCtx, c, j, q)
	stampCancel()
	if serr != nil {
		// Refused rather than noted, on a passing canary as much as on a failing one: a qualification nobody
		// recorded qualifies nothing, and reporting success for a reading that reached no cluster would leave
		// the operator believing their worker was qualified when the next run will refuse it.
		//
		// What the operator most needs to know here is what is STILL ON THE NODE, because this failure leaves
		// whatever was there before untouched — and on a failing reading that is the dangerous case: a
		// previously passing qualification survives the canary that has just contradicted it, and runs go on
		// consulting it. Saying only "not recorded" leaves that to be worked out.
		standing := "whatever qualification was already on the node is unchanged, so runs will go on consulting it"
		if len(q.Failures) > 0 {
			standing = "this reading FAILED and did not replace what was already on the node: any qualification " +
				"standing there has just been contradicted and may now be false, and runs will consult it until " +
				"this is recorded or cleared"
		}
		return fmt.Errorf("canary on %s: the reading was taken but not recorded: %w\n  %s", nodeName, serr, standing)
	}
	if len(q.Failures) > 0 {
		return fmt.Errorf("worker %s did not qualify: this cluster does not produce the contrast the arms are "+
			"built on (%d failed claim(s), recorded on the node as canary %s)", nodeName, len(q.Failures), canaryID)
	}
	return nil
}

// reportCanary prints what the two probes did, passing or failing.
//
// The numbers are printed on the passing path too, unlike most of this program's operator output: this is the
// one command whose entire product is a measurement, and an operator who cannot see the separation between the
// two probes has no way to tell a healthy margin from one that scraped past a threshold.
func reportCanary(out io.Writer, q canaryQualification) {
	_, _ = fmt.Fprintf(out, "\ntermination canary %s on node %s (harness %s)\n", q.CanaryID, q.Node, q.HarnessRevision)
	for _, p := range []canaryProbe{q.Honor, q.Ignore} {
		if !p.Terminated {
			_, _ = fmt.Fprintf(out, "  %-10s %s: NOT OBSERVED TO STOP: %s\n", p.Contract, p.Pod, p.Unobserved)
			continue
		}
		_, _ = fmt.Fprintf(out, "  %-10s %s: stopped after %dms, exit %d (%s)\n",
			p.Contract, p.Pod, p.StoppedAfterMs, p.ExitCode, p.Reason)
	}
	if len(q.Failures) == 0 {
		_, _ = fmt.Fprintf(out, "QUALIFIED: the honouring workload stopped inside %s and the ignoring one outlasted %s, "+
			"so the two arms differ on this cluster by what they are supposed to differ by.\n",
			canaryPromptWithin, canarySurvivesAtLeast)
		return
	}
	_, _ = fmt.Fprintf(out, "NOT QUALIFIED:\n")
	for _, f := range q.Failures {
		_, _ = fmt.Fprintf(out, "  - %s\n", f)
	}
}

// operatorImageIDFrom reads the running controller-manager's image digest, or "" when it cannot tell.
//
// It exists beside operatorImageID rather than replacing it because the two callers hold different things:
// qualify is already given every Pod in the cluster and must not list them twice, while the canary take path
// has only a client. A read failure returns "" for the same reason ambiguity does -- the honest answer to
// "which image rendered this" is sometimes "nobody here can say", and a canary keyed on that matches only
// another run equally unable to say.
func operatorImageIDFrom(ctx context.Context, c client.Client) string {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.MatchingLabels{"control-plane": "controller-manager"}); err != nil {
		return ""
	}
	return operatorImageID(pods.Items)
}
