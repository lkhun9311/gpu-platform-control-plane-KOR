package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// The preflight Pod must ASK FOR A DEVICE, and that is the single thing separating it from a canary probe.
//
// probePodFrom strips the device request from every container, deliberately and for a good reason of its own.
// The preflight goes through that same function and puts one back, so a version of this that forgot the
// second half would produce a Pod identical to a canary probe: it would schedule anywhere, run the workload
// on the CPU, report dev=no-libcuda, and be indistinguishable from a genuine device fault. The operator would
// then cancel a GPU session over a bug in the checker.
//
// Mutations that turn this red, both run: delete the limit assignment, and put the limit on every container
// rather than the trainer.
func TestThePreflightPodAsksForExactlyOneDeviceOnTheTrainer(t *testing.T) {
	contract, err := harnessTerminationContract()
	if err != nil {
		t.Fatalf("contract: %v", err)
	}
	command, err := preflightCommand()
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	pod, err := preflightPod("abcdefgh-1111", "platform-worker", "run-1", contract, command)
	if err != nil {
		t.Fatalf("preflightPod: %v", err)
	}
	var withDevice []string
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		q, ok := c.Resources.Limits[gpuResourceName]
		if !ok {
			continue
		}
		withDevice = append(withDevice, c.Name)
		if q.Value() != 1 {
			t.Fatalf("container %q asks for %d devices; asking for more turns a working node with a busy "+
				"neighbour into a preflight failure", c.Name, q.Value())
		}
	}
	if len(withDevice) != 1 || withDevice[0] != probeTrainerContainer {
		t.Fatalf("the device request landed on %v, want exactly [%s]", withDevice, probeTrainerContainer)
	}

	// The placement a canary probe gets is inherited rather than re-derived, so the preflight qualifies the
	// node it names. A preflight that landed elsewhere would report on somebody else's hardware.
	if pod.Spec.NodeName != "platform-worker" {
		t.Fatalf("the preflight pod is not pinned to its worker: %q", pod.Spec.NodeName)
	}
	if len(pod.Finalizers) == 0 {
		t.Fatal("the preflight pod carries no finalizer, so its terminated status can vanish before it is read")
	}
	// The workload has to be the one this build ships, or the preflight qualifies a Pod nobody runs.
	trainer := pod.Spec.Containers[0]
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == probeTrainerContainer {
			trainer = pod.Spec.Containers[i]
		}
	}
	if trainer.Image != queuelab.WorkloadImage {
		t.Fatalf("the preflight runs %q, not the workload image %q", trainer.Image, queuelab.WorkloadImage)
	}
}

// The failing verdict must name the DEVICE STATUS TOKEN, and that is the whole reason this mode exists.
//
// A preflight that said "the device did not work" would leave an operator with a rented GPU node and eight
// candidate causes. no-libcuda is a base image or a runtime that never injected the driver; no-device is a
// card that was not passed through; ptx-load-failed is a kernel this driver would not compile. Those are
// three different afternoons.
//
// Mutation that turns this red: drop the token from the message, or report the fallback as a pass.
func TestThePreflightVerdictNamesWhichDriverCallRefused(t *testing.T) {
	term := func(msg string) *corev1.ContainerStateTerminated {
		return &corev1.ContainerStateTerminated{ExitCode: 0, Message: msg}
	}
	for _, tc := range []struct {
		name, msg, wants string
		ok, usedDevice   bool
	}{
		{name: "a driver that was never injected", msg: "iters=9 kind=cpu-float dev=no-libcuda",
			wants: "no-libcuda"},
		{name: "a card that was not passed through", msg: "iters=9 kind=cpu-float dev=no-device",
			wants: "no-device"},
		{name: "a kernel this driver would not compile", msg: "iters=9 kind=cpu-float dev=ptx-load-failed",
			wants: "ptx-load-failed"},
		// Kernels ran and then stopped, which is a different afternoon again: the node is not unusable, it is
		// unreliable, and a run would abort mid-protocol rather than come back refused. usedDevice stays TRUE
		// here, because the card demonstrably worked -- an observer reporting it busy is corroboration rather
		// than the contradiction the CPU rows would make it.
		{name: "a card that worked and then stopped", msg: "iters=400 kind=cuda-fma dev=launch-failed-midrun",
			wants: "launch-failed-midrun", usedDevice: true},
		// A message this build cannot parse is not evidence about the device at all, and saying so is the
		// difference between cancelling a session and fixing a wire format.
		{name: "a report in a shape this build cannot read", msg: "iters=9", wants: "wire format"},
		{name: "the pass", msg: "iters=1200 kind=cuda-fma dev=ok", ok: true, usedDevice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := preflightWorkload(term(tc.msg))
			if v.usedDevice != tc.usedDevice {
				t.Fatalf("usedDevice = %v, want %v; this field decides whether a busy card is corroboration "+
					"or a contradiction", v.usedDevice, tc.usedDevice)
			}
			if tc.ok {
				if v.err != nil {
					t.Fatalf("a launched kernel was reported as a failure: %v", v.err)
				}
				if !strings.Contains(v.pass, "DEVICE USABLE") || !strings.Contains(v.pass, "1200") {
					t.Fatalf("the pass does not say so, or does not report how much ran -- a workload that "+
						"launched once would read the same as one that saturated the card: %q", v.pass)
				}
				return
			}
			if v.err == nil {
				t.Fatalf("message %q was reported as a usable device", tc.msg)
			}
			if !strings.Contains(v.err.Error(), tc.wants) {
				t.Fatalf("the verdict does not name what refused (%q): %v", tc.wants, v.err)
			}
			if strings.Contains(v.pass, "DEVICE USABLE") {
				t.Fatalf("a failing verdict also carries a pass line: %q", v.pass)
			}
		})
	}
}

// The mode is selected, and it is refused in combination like every other operator mode.
//
// Mutation that turns this red: leave DevicePreflight out of decideOperatorMode's requested count, which is
// the edit that would let `-device-preflight -inspect-worker` run one of them silently.
func TestTheDevicePreflightIsAnOperatorModeLikeTheOthers(t *testing.T) {
	if m, err := decideOperatorMode(operatorModeArgs{DevicePreflight: true, Worker: "w"}); err != nil ||
		m != modeDevicePreflight {
		t.Fatalf("mode = %v, err = %v", m, err)
	}
	if _, err := decideOperatorMode(operatorModeArgs{
		DevicePreflight: true, Inspect: true, Worker: "w",
	}); err == nil {
		t.Fatal("two operator modes at once were accepted, so one of them ran and the operator cannot say which")
	}
	// It is not a run, so the flags that configure one must not look configured beside it.
	if _, err := decideOperatorMode(operatorModeArgs{
		DevicePreflight: true, Worker: "w", Arm: "A-honor",
	}); err == nil {
		t.Fatal("-arm was accepted beside the preflight, which reads as an invocation that runs an arm")
	}
	if _, err := decideOperatorMode(operatorModeArgs{
		DevicePreflight: true, Worker: "w", RunOnlyFlags: []string{"-out"},
	}); err == nil {
		t.Fatal("-out was accepted beside the preflight, so the invocation would look like it wrote a record")
	}
	// And its own timeout, or the mode inherits a budget shorter than an image pull on a cold GPU node.
	ctx, cancel := operatorModeContext(modeDevicePreflight)
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the preflight runs on a context with no deadline")
	}
	if d.Before(time.Now().Add(preflightBudget)) {
		t.Fatalf("the mode's budget is shorter than the wait it contains, so it can only ever time out")
	}
}

// Warmth is read from the DIGEST, not from the whole image reference.
//
// The kubelet reports what it pulled. For a digest-pinned image that is "docker.io/library/python@sha256:..."
// -- repository plus digest, without the tag this build's constant carries -- so comparing the references
// whole reports every warm node as cold. The first execution of this check did exactly that on a node the
// image had been sitting on for an hour.
//
// A cold node is not an error, but it is a fact about the first run's timings: the pull happens inside the
// observation window, pushes every container stop later, and can push one past the horizon, which flips
// measurement.censored and turns wastedGPUSeconds from a value into a floor.
//
// Mutation that turns this red: compare the whole reference again.
func TestNodeWarmthIsReadFromTheImageDigest(t *testing.T) {
	digest := queuelab.WorkloadImage
	at := strings.Index(digest, "@")
	if at < 0 {
		t.Fatal("the workload image is not pinned by digest, so nothing here can identify the bytes a node " +
			"holds; this test is checking a shape that has moved and must be rewritten rather than deleted")
	}
	digest = digest[at+1:]

	// What a kubelet actually reports for this image, and what it would report for a different one.
	kubeletNames := []string{"docker.io/library/python@" + digest}
	other := []string{"docker.io/library/python@sha256:" + strings.Repeat("a", 64),
		"docker.io/library/python:3.12-alpine"}

	warm := func(names []string) bool {
		for _, n := range names {
			if strings.Contains(n, digest) {
				return true
			}
		}
		return false
	}
	if !warm(kubeletNames) {
		t.Fatalf("a node holding this exact image reads as cold: kubelet reports %v, the build wants %q",
			kubeletNames, queuelab.WorkloadImage)
	}
	if warm(other) {
		t.Fatalf("a node holding a DIFFERENT image reads as warm: %v", other)
	}
	// And the trap that produced the defect: the whole reference never matches what a kubelet reports.
	for _, n := range kubeletNames {
		if n == queuelab.WorkloadImage {
			t.Fatal("the kubelet's name and the build's constant are equal in this fixture, so it no longer " +
				"reproduces the mismatch it exists to guard")
		}
	}
}
