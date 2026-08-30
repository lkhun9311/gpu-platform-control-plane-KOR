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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// The preflight is the termination canary's mirror image, and the pair is worth reading together.
//
// The canary STRIPS the device request, because what it qualifies is signal delivery and a probe that needed a
// free card would fail on a node whose devices are legitimately held. The preflight KEEPS it, because the only
// question it asks is whether a real device works here. Everything else -- the Pod template, the placement,
// the toleration, the finalizer that keeps a terminated status readable -- is deliberately identical, so a
// preflight that passes is a statement about the shape a run actually gets rather than about a Pod invented
// for the occasion.
//
// It exists because nothing else in this harness can ask that question. The canary cannot: its probes have no
// device. A run cannot either, not usefully -- it answers only at the end, after the protocol has spent its
// minutes and the meter has run, and it answers with a refused record rather than with which of the eight
// driver calls said no. On rented hardware that difference is the whole cost of the session.
const (
	// preflightDurationSec is how long the workload runs when nobody stops it.
	//
	// Short, because the preflight measures nothing: it asks whether a kernel launches. Long enough that the
	// loop marks its termination log at least once from inside itself, which is what the ignoring arm depends
	// on in a real run and therefore what is worth exercising here too.
	preflightDurationSec = 8

	// preflightStartBudget bounds waiting for the Pod to reach a terminal phase.
	//
	// It is the canary's start budget plus the run itself, and it is generous for one reason: the first Pod on
	// a fresh GPU node pulls an image and initialises a driver context, and a preflight that timed out on a
	// cold cache would send an operator looking for a device fault that is not there.
	preflightBudget = 6 * time.Minute

	// preflightPollInterval is how often the Pod's status is re-read.
	preflightPollInterval = 2 * time.Second
)

// preflightModeTimeout bounds the whole mode, including its cleanup.
const preflightModeTimeout = preflightBudget + 3*time.Minute

// devicePreflight applies one GPU-holding Pod to a worker and reports what the workload said about the device.
//
// It acquires the worker through the ordinary transaction rather than applying a Pod beside whatever is
// running. Two reasons, and the second is the one that matters: a preflight racing a run would take the device
// the run is measuring, and a preflight that found no free device would report a device fault when what it
// actually found was a busy node.
//
// It is not a run and writes no record. The verdict here is a yes/no about hardware, and putting it in a
// document whose schema describes measured intervals would let a reader take it for one.
func devicePreflight(ctx context.Context, c client.Client, nodeName, metricsURL, observerIdentity string,
	now func() time.Time, sleep func(time.Duration), out io.Writer) (err error) {
	contract, cerr := harnessTerminationContract()
	if cerr != nil {
		return fmt.Errorf("build the preflight's workload contract: %w", cerr)
	}
	command, cmderr := preflightCommand()
	if cmderr != nil {
		return fmt.Errorf("render the preflight's workload command: %w", cmderr)
	}
	id := string(uuid.NewUUID())
	runID := canaryRunID(id)

	j, aerr := acquireWorker(ctx, c, nodeName, ownerIdentity{
		Kind:      ownerCanary,
		TxID:      newTxID(),
		RunID:     runID,
		Arm:       canaryArm,
		Namespace: canaryNamespace,
		CanaryID:  id,
	})
	if aerr != nil {
		return fmt.Errorf("acquire worker %s to preflight it: %w", nodeName, aerr)
	}
	_, _ = fmt.Fprintf(out, "worker %s acquired for device preflight %s: tx=%s\n"+
		"  (if this process dies, run: queuelabrun -inspect-worker -worker %s)\n",
		nodeName, id, j.TxID, nodeName)
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
		_, _ = fmt.Fprintf(out, "WORKER NOT RESTORED: %v\n  run: queuelabrun -inspect-worker -worker %s\n",
			rerr, nodeName)
		// Only when nothing else already failed: a preflight that found no usable device AND could not give
		// the worker back is still primarily a preflight that found no usable device, and overwriting its
		// reason would hide the thing an operator needs.
		if err == nil {
			err = fmt.Errorf("release worker %s after the preflight: %w", nodeName, rerr)
		}
	}()

	// Read the node's image list BEFORE applying anything, because this is the last moment the answer is
	// still about the node rather than about this preflight.
	reportNodeWarmth(ctx, c, nodeName, out)

	if nerr := ensureCanaryNamespace(ctx, c); nerr != nil {
		return fmt.Errorf("ensure the preflight namespace: %w", nerr)
	}
	pod, perr := preflightPod(id, nodeName, runID, contract, command)
	if perr != nil {
		return perr
	}
	if cerr := c.Create(ctx, pod); cerr != nil {
		return fmt.Errorf("apply the preflight pod: %w", cerr)
	}
	// releaseProbes removes the finalizer this Pod carries and deletes it, and it is deferred before the wait
	// so a cancelled preflight does not strand a Pod holding a device on the node it was checking.
	defer func() {
		relCtx, relCancel := cleanupContext()
		defer relCancel()
		releaseProbes(relCtx, c, []*corev1.Pod{pod}, out)
	}()

	// The observer runs WHILE the Pod holds the device, because that is the only moment there is anything to
	// see. It is started before the wait and stopped after it, so the window it covers is the window the
	// workload occupied.
	t0 := now()
	elapsed := func() int64 { return now().Sub(t0).Nanoseconds() }
	watcher := startPreflightObserver(ctx, pod, metricsURL, observerIdentity, elapsed)

	term, fromNs, toNs, werr := awaitPreflightStopped(ctx, c, pod, now, elapsed, sleep)
	obs, missed, oerr := watcher.stop()
	if werr != nil {
		return werr
	}
	// Both verdicts are computed before either is reported, because their COMBINATION is a third answer that
	// neither carries alone. A workload on the CPU beside an observer reporting its card busy is not two
	// independent failures; it is the fake-exporter shape, and the record refuses exactly that pairing. A
	// preflight that returned on the first failure would never look at the second and would send an operator
	// to replace a driver when the problem was a misattributing exporter.
	wl := preflightWorkload(term)
	if metricsURL == "" {
		if wl.err != nil {
			return wl.err
		}
		_, _ = fmt.Fprint(out, wl.pass)
		_, _ = fmt.Fprint(out, "OBSERVER NOT CHECKED: no -device-metrics was given, so this preflight says "+
			"nothing about whether an observer would attribute that work. A run on this node would still "+
			"record device-not-observed.\n")
		return nil
	}
	// The scrape's own error is reported BEFORE the gate, because the gate can only ever describe an
	// interval and this describes why there was nothing to judge over it.
	if oerr != nil {
		return fmt.Errorf("OBSERVER FAILED on %s: %w. Nothing below is a statement about the card: the "+
			"attribution gate would report a coverage gap for this, which is a message about an interval "+
			"produced by an error somewhere else", nodeName, oerr)
	}
	if missed > 0 {
		// Not fatal: it means the exporter saw Pods this preflight did not, which on a shared node is
		// ordinary. It is printed because on a node that should be exclusive it is not.
		_, _ = fmt.Fprintf(out, "  device observer: %d sample(s) named Pods this preflight never applied\n",
			missed)
	}
	// The preflight's probe has no ledger and no owner waiting on it, so its attempt and its "hold" are the
	// same interval: the window it ran for. Passing it twice is not a shortcut here -- there is genuinely
	// only one interval, and the gate's two clauses both apply to it.
	attributed, why := queuelab.EstablishesDeviceWork(obs, queuelab.DeviceClaim{
		PodUID:     string(pod.UID),
		WorkFromNs: fromNs, WorkToNs: toNs,
		HoldFromNs: fromNs, HoldToNs: toNs,
	})
	switch {
	case attributed && !wl.usedDevice:
		return fmt.Errorf("OBSERVER CONTRADICTS THE WORKLOAD on %s: the exporter at %s says this Pod's card "+
			"was busy, and the Pod itself reports it never reached a driver call (kind=%q dev=%q). A card busy "+
			"while the only tenant on it computed nothing is another process's activity arriving under this "+
			"Pod's label, and a run here would refuse the observation for the same reason -- so this is an "+
			"exporter or a node to fix, not a device to trust", nodeName, obs.Endpoint, wl.kind, wl.device)
	case wl.err != nil:
		// The device did not work AND nothing claimed otherwise, which is the ordinary honest failure.
		return wl.err
	case !attributed:
		return fmt.Errorf("OBSERVER CANNOT ATTRIBUTE on %s: %s. The card worked and nothing outside the "+
			"workload can say so, which is the same record a run with no exporter at all produces -- so a "+
			"session started now would spend its minutes and come back device-not-observed", nodeName, why)
	}
	_, _ = fmt.Fprint(out, wl.pass)
	_, _ = fmt.Fprintf(out, "OBSERVER ATTRIBUTES: over the %.1f s the preflight Pod held the device, the "+
		"exporter at %s named its UID on exactly one card and showed it busy, with no second tenant. A run on "+
		"this node can reach device-work-observed.\n", float64(toNs-fromNs)/1e9, obs.Endpoint)
	return nil
}

// preflightObserver is the running device scrape, or the absence of one.
type preflightObserver struct {
	done   chan struct{}
	cancel context.CancelFunc
	obs    *queuelab.DeviceObservation
	missed int
	// err is the scrape's own failure, and discarding it was a diagnostic hole rather than tidiness. A
	// refused HTTP request, a malformed payload or a parser incompatibility all end the observation early,
	// and the attribution gate then reports a coverage gap -- a message about an interval, produced by an
	// error somewhere else entirely.
	err error
}

// stop ends the scrape and returns what it saw.
//
// A nil observer returns a nil observation, which is exactly what EstablishesDeviceWork refuses on, so the
// no-endpoint case needs no special handling downstream.
func (p *preflightObserver) stop() (*queuelab.DeviceObservation, int, error) {
	if p == nil {
		return nil, 0, nil
	}
	p.cancel()
	<-p.done
	return p.obs, p.missed, p.err
}

// startPreflightObserver begins scraping the exporter for the duration of the preflight.
//
// The resolver knows exactly one Pod, which is the whole difference from a run's: a run resolves through the
// collector's registry of everything it watched, and this has one object whose UID it holds already. Samples
// naming any other Pod resolve to "" and are KEPT rather than dropped -- unattributable, but present, because
// the exclusivity clause needs to see a second tenant on the card to convict one.
func startPreflightObserver(ctx context.Context, pod *corev1.Pod, metricsURL, identity string,
	elapsed func() int64) *preflightObserver {
	if metricsURL == "" {
		return nil
	}
	uid := string(pod.UID)
	scraper := &queuelab.DeviceScraper{
		// Declared by configuration, not verified here, exactly as a run declares it. The preflight cannot
		// check an exporter's identity any more than a run can; what it checks is that the endpoint answers,
		// parses, and names this Pod on a card.
		Observer: queuelab.ObserverDCGM,
		Identity: identity,
		Endpoint: metricsURL,
		Interval: deviceScrapeInterval,
		Elapsed:  elapsed,
		Resolve: func(namespace, name string) string {
			if namespace == pod.Namespace && name == pod.Name {
				return uid
			}
			return ""
		},
		Fetch: func(fctx context.Context) ([]byte, error) { return fetchMetrics(fctx, metricsURL) },
	}
	dctx, dcancel := context.WithCancel(ctx)
	p := &preflightObserver{done: make(chan struct{}), cancel: dcancel}
	go func() {
		defer close(p.done)
		p.obs, p.missed, p.err = scraper.Observe(dctx)
	}()
	return p
}

// reportNodeWarmth says whether the workload's image was already on the node when the preflight arrived.
//
// It matters because this Pod is about to put it there. Everything after the preflight runs on a warm node:
// the layer cache is populated, so the protocol's own Pods start without a pull.
//
// That is desirable and it is also a property of the measurement rather than of the platform, which is why
// it is reported rather than left implicit. A cold first run pulls inside the observation window, pushes
// every container stop later, and can push one past the horizon -- which flips measurement.censored and
// turns wastedGPUSeconds from a value into a floor, with nothing in the terminal saying why. So the
// preflight is not merely a check that happens to warm the node; running it is what makes every subsequent
// run comparable to every other.
//
// A failure to read the node is not fatal. This is a diagnostic, and refusing a preflight because the
// warmth of a node could not be established would refuse the check for the sake of its footnote.
func reportNodeWarmth(ctx context.Context, c client.Client, nodeName string, out io.Writer) {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
		_, _ = fmt.Fprintf(out, "  node warmth: unknown (%v)\n", err)
		return
	}
	// Matched on the DIGEST rather than the whole reference. The kubelet reports what it pulled, which for a
	// digest-pinned image is "docker.io/library/python@sha256:..." -- repository plus digest, without the tag
	// this build's constant carries. Comparing the strings whole reported every warm node as cold, which the
	// first run of this check showed immediately.
	//
	// The digest is the right key anyway: it is what identifies the bytes, and it is why the image is pinned
	// by one.
	digest := queuelab.WorkloadImage
	if at := strings.Index(digest, "@"); at >= 0 {
		digest = digest[at+1:]
	}
	for _, img := range n.Status.Images {
		for _, name := range img.Names {
			if strings.Contains(name, digest) {
				_, _ = fmt.Fprintf(out, "  node warmth: WARM -- the workload image is already on %s, so the "+
					"runs after this one start without a pull\n", nodeName)
				return
			}
		}
	}
	_, _ = fmt.Fprintf(out, "  node warmth: COLD -- the workload image is not on %s yet. This preflight is "+
		"about to pull it, so the protocol's runs will start warm and this preflight will not; do not "+
		"compare its timings with theirs\n", nodeName)
}

// preflightCommand renders the workload the preflight runs.
//
// It is the HONOURING arm, and the choice is not arbitrary: this Pod is deleted at the end whether it finished
// or not, and an ignoring workload would sit through the full grace period before SIGKILL, adding thirty
// seconds to every preflight for no reading. The device path is identical in both arms.
func preflightCommand() ([]string, error) {
	job, err := queuelab.RenderMLTrainingJobWithContract(queuelab.TrainingTraceRow{
		Index: 0, Name: "device-preflight", Tenant: "canary",
		GPUCount: 1, DurationSec: preflightDurationSec,
	}, canaryNamespace, queuelab.HonorsSIGTERM)
	if err != nil {
		return nil, err
	}
	return job.Spec.Command, nil
}

// preflightPod builds the preflight Pod from the same rendered template the canary probes.
//
// It goes through probePodFrom and then puts the device request BACK, rather than building a Pod here. The
// duplication that would otherwise arise is not cosmetic: probePodFrom carries the placement, the toleration
// that matches the ResourceFlavor a run's Pods are admitted under, the trainer-by-name lookup and the
// finalizer that keeps a terminated status readable after the container stops. A second copy of those would
// drift, and the copy that drifted would be the one nobody runs until a GPU node is already rented.
//
// Exactly one device, not the sentinel's seven and not the row's count: the preflight asks whether A card
// works, and asking for more would turn a working node with a busy neighbour into a preflight failure.
func preflightPod(id, node, runID string, contract canaryContract, command []string) (*corev1.Pod, error) {
	pod, err := probePodFrom(renderedPodTemplate(templateProbeJob()), id, node, runID, contract,
		probeSpec{name: "dp-" + shortID(id), contract: "HonorsSIGTERM", command: command})
	if err != nil {
		return nil, err
	}
	trainer := -1
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == probeTrainerContainer {
			trainer = i
			break
		}
	}
	if trainer < 0 {
		// probePodFrom refuses this case already, so reaching it means the two disagree about which container
		// is the workload -- which would put the device on one container and the command on another.
		return nil, fmt.Errorf("internal error: the preflight pod has no %q container after probePodFrom "+
			"accepted the template", probeTrainerContainer)
	}
	if pod.Spec.Containers[trainer].Resources.Limits == nil {
		pod.Spec.Containers[trainer].Resources.Limits = corev1.ResourceList{}
	}
	pod.Spec.Containers[trainer].Resources.Limits[gpuResourceName] = *resource.NewQuantity(1, resource.DecimalSI)
	return pod, nil
}

// shortID trims an id to the prefix the probe names use.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// awaitPreflightStopped waits for the preflight's trainer container to terminate and returns its final state.
//
// It waits for a TERMINAL container state rather than for the Pod to be Running, because a workload that
// starts and then cannot reach the driver is exactly the case this exists to catch: it runs, it reports, and
// it exits zero. Readiness would have called that a pass.
func awaitPreflightStopped(ctx context.Context, c client.Client, pod *corev1.Pod,
	now func() time.Time, elapsed func() int64, sleep func(time.Duration),
) (term *corev1.ContainerStateTerminated, fromNs, toNs int64, err error) {
	deadline := now().Add(preflightBudget)
	var last string
	// The window opens when the container is first seen RUNNING rather than when the Pod was applied. An
	// image pull can take minutes, and charging the observer with covering it would refuse every cold node
	// for a gap that is not about the card.
	fromNs = -1
	for {
		var live corev1.Pod
		err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &live)
		switch {
		case apierrors.IsNotFound(err):
			// Deleted by something other than this process, so the reading is gone rather than negative.
			return nil, 0, 0, fmt.Errorf("the preflight pod %s/%s disappeared before it terminated; something "+
				"outside this invocation deleted it, and no verdict about the device can come from that",
				pod.Namespace, pod.Name)
		case err != nil:
			return nil, 0, 0, fmt.Errorf("read the preflight pod: %w", err)
		}
		if fromNs < 0 && probeRunning(&live) {
			fromNs = elapsed()
		}
		if t := terminatedState(&live); t != nil {
			if fromNs < 0 {
				// It terminated without this loop ever catching it Running, which a short workload on a fast
				// node can do between polls. The Pod's own start time is not on this clock, so the window is
				// taken from the beginning rather than invented: an interval that is too WIDE can only make
				// the coverage check stricter, never falsely pass it.
				fromNs = 0
			}
			// The far end comes from the KUBELET's own finishedAt where there is one, not from the poll that
			// happened to notice. This loop polls every two seconds and the gate allows a two-second gap, so
			// a poll-derived end is up to a full budget late: the Pod's label leaves the exporter's view
			// within a scrape of the real stop, and the last attributed sample then sits further from the
			// interval's end than the gate permits. A healthy card refused on a race with the poll phase.
			//
			// finishedAt is on the kubelet's wall clock and the samples are on this process's elapsed one,
			// so it is converted through the poll that observed it rather than used raw; when the kubelet
			// published none, the poll time is all there is and the previous poll bounds it.
			to := elapsed()
			if !t.FinishedAt.IsZero() {
				if lag := now().Sub(t.FinishedAt.Time); lag > 0 && lag < preflightPollInterval {
					to -= lag.Nanoseconds()
				}
			}
			if to < fromNs {
				to = fromNs
			}
			return t, fromNs, to, nil
		}
		if trouble := probeTrouble(&live); trouble != "" {
			last = trouble
		}
		if !now().Before(deadline) {
			if last == "" {
				last = fmt.Sprintf("phase %s", live.Status.Phase)
			}
			return nil, 0, 0, fmt.Errorf("the preflight pod did not terminate within %s: %s. A device request that "+
				"no node can satisfy looks exactly like this, so check that %s advertises %s before reading it "+
				"as a driver fault", preflightBudget, last, pod.Spec.NodeName, gpuResourceName)
		}
		sleep(preflightPollInterval)
	}
}

// reportPreflight turns the workload's own report into the verdict an operator acts on.
//
// The failing case names the DEVICE STATUS token rather than saying the device did not work, and that is the
// entire value of this mode. "no-libcuda" is a base image or a container runtime that never injected the
// driver; "no-device" is a card that was not passed through; "ptx-load-failed" is a kernel this driver would
// not compile. Those send an operator to three different places, and a session that discovered the difference
// from a refused record would have paid for the protocol to find out.
type workloadVerdict struct {
	// usedDevice is whether the workload reached a driver call at all, which is the fact the contradiction
	// check needs and which `err` alone cannot carry: an unreadable report is also an error, and it is NOT
	// evidence that the device went unused.
	usedDevice bool
	kind       string
	device     string
	// pass is what to print when nothing else refuses. It is held rather than written because a verdict that
	// printed as it decided could put "DEVICE USABLE" above a refusal.
	pass string
	err  error
}

// preflightWorkload turns the container's own report into a verdict, without printing.
//
// The failing cases name the DEVICE STATUS TOKEN, and that is the whole reason this mode exists. A preflight
// that said "the device did not work" would leave an operator with a rented GPU node and eight candidate
// causes. no-libcuda is a base image or a runtime that never injected the driver; no-device is a card that
// was not passed through; ptx-load-failed is a kernel this driver would not compile. Three different
// afternoons.
func preflightWorkload(t *corev1.ContainerStateTerminated) workloadVerdict {
	iters, kind, device := queuelab.ReportFromMessage(t.Message)
	if iters == nil {
		return workloadVerdict{err: fmt.Errorf(
			"the preflight workload left no report this build can read (exit %d, message %q). That is not a "+
				"statement about the device: it is the workload and this harness disagreeing about the wire "+
				"format, and a run on this node would record its provenance as unreported",
			t.ExitCode, t.Message)}
	}
	v := workloadVerdict{kind: kind, device: device, usedDevice: kind == queuelab.KindCUDAFMA}
	switch {
	case !v.usedDevice:
		v.err = fmt.Errorf("DEVICE NOT USABLE: the workload fell back to the CPU loop, reporting dev=%q "+
			"after %d iterations. A run here would complete and refuse to attribute device work, so its "+
			"GPU-seconds would be seconds of RESERVATION exactly as they are on a cluster with no cards",
			device, *iters)
	case device != queuelab.DeviceOK:
		v.err = fmt.Errorf("DEVICE UNRELIABLE: kernels launched and then stopped (dev=%q after %d "+
			"launches). A run here would abort mid-protocol rather than produce a refused record",
			device, *iters)
	default:
		v.pass = fmt.Sprintf("DEVICE USABLE: the workload loaded its PTX through the CUDA driver and "+
			"completed %d kernel launches in %d s.\n", *iters, preflightDurationSec)
	}
	return v
}
