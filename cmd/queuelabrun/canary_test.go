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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// The kubelet and runtime the test worker reports. They are plausible kind values rather than placeholders
// because they are half the qualification key, and a key whose node-side fields were "" would compare equal
// to any other empty key — which is exactly the shape of a check that has stopped checking.
const (
	testKubeletVersion   = "v1.31.0"
	testContainerRuntime = "containerd://1.7.18"
)

// passingCanary is a qualification of the test worker that demonstrated the contrast.
//
// The key is built from harnessTerminationContract rather than from literals, and that is a deliberate and
// bounded piece of self-reference: it is the only way a fixture can track the pinned image digest without
// becoming a second copy of it that drifts. What stops it from turning into a test that compares a function
// against itself is that nothing here is only checked against itself —
// TestCanaryContractMirrorsWhatTheHarnessActuallyRenders pins the contract's fields against the measurement
// package's own output and against the literal exit code, decodeCanary refuses a document whose key is
// missing any of them, and every mismatch test below supplies a literal that differs.
func passingCanary() canaryQualification {
	// No *testing.T here, and the error cannot fire: harnessTerminationContract passes package constants and
	// the renderer's only failure is an unknown contract. Every test that CAN report an error uses
	// mustHarnessContract instead.
	c, _ := harnessTerminationContract()
	return canaryQualification{
		Schema:          canarySchema,
		CanaryID:        "canary-0001",
		Node:            "platform-worker",
		QualifiedAt:     "2026-08-15T00:00:00Z",
		HarnessRevision: "abc123",
		Key: canaryKey{
			Image:            c.Image,
			HonorCommand:     c.HonorCommand,
			IgnoreCommand:    c.IgnoreCommand,
			GraceSec:         terminationGraceSec,
			HonorExitCode:    honorExitCode,
			NodeUID:          "uid-node",
			KubeletVersion:   testKubeletVersion,
			ContainerRuntime: testContainerRuntime,
			PodTemplateHash:  c.PodTemplateHash,
		},
		Honor:  passingHonorProbe(),
		Ignore: passingIgnoreProbe(),
	}
}

// The two probe readings a healthy cluster produces: the honouring workload stops in under a second with the
// trap's own code, and the ignoring one spends the whole grace period and is killed at the end of it.
func passingHonorProbe() canaryProbe {
	c, _ := harnessTerminationContract()
	return canaryProbe{
		Pod: "tc-abcd1234-honor", Contract: "HonorsSIGTERM", Command: c.HonorCommand,
		GraceSec: terminationGraceSec, Terminated: true, ExitCode: honorExitCode, Reason: "Error",
		StoppedAfterMs: 800,
	}
}

func passingIgnoreProbe() canaryProbe {
	c, _ := harnessTerminationContract()
	return canaryProbe{
		Pod: "tc-abcd1234-ignore", Contract: "IgnoresSIGTERM", Command: c.IgnoreCommand,
		GraceSec: terminationGraceSec, Terminated: true, ExitCode: killExitCode, Reason: "Error",
		StoppedAfterMs: 30400,
	}
}

// qualifiedCanaryAnnotation is what node() hangs on the test worker.
//
// The encode error is ignored deliberately: every field of the document is a string, a slice of strings, an
// int or a bool, so json.Marshal has no failure mode reachable from here, and threading a *testing.T through
// node() would touch fifty call sites for an error that cannot occur. A malformed document would still fail
// loudly rather than silently — decodeCanary refuses it, and every test whose worker has to qualify would
// refuse at the gate.
func qualifiedCanaryAnnotation() string {
	raw, _ := encodeCanary(passingCanary())
	return raw
}

// canaryAnnotation renders a qualification with one thing about it changed.
func canaryAnnotation(t *testing.T, mutate func(*canaryQualification)) string {
	t.Helper()
	q := passingCanary()
	mutate(&q)
	raw, err := encodeCanary(q)
	if err != nil {
		t.Fatalf("encode canary: %v", err)
	}
	return raw
}

// testCanaryReference is what a run that stood on a qualification carries in its record.
func testCanaryReference() *canaryReference {
	q := passingCanary()
	return &canaryReference{
		CanaryID:             q.CanaryID,
		QualifiedAt:          q.QualifiedAt,
		Key:                  q.Key,
		HonorStoppedAfterMs:  q.Honor.StoppedAfterMs,
		IgnoreStoppedAfterMs: q.Ignore.StoppedAfterMs,
	}
}

// canaryReferenceJSON is the reference as a hand-written record's qualification block has to carry it.
//
// Marshalled from the real type rather than typed out, because decodeRunRecord reads records with
// DisallowUnknownFields: a hand-written reference would have to be kept in step with the struct by hand, and
// the failure when it was not would look like the guard under test rejecting a good document.
func canaryReferenceJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(testCanaryReference())
	if err != nil {
		t.Fatalf("marshal the canary reference: %v", err)
	}
	return string(b)
}

// The canary probes what the arm submits, or it probes nothing worth knowing. Every field of the contract is
// read out of internal/queuelab's own renderer — the same function submit() calls — so this test is what
// stands between that claim and a hand-written imitation of the workload drifting away from it.
//
// The exit code is the one field that IS a local copy (termExitCode is private), so it is checked against the
// rendered command's own text rather than against itself: if the measurement package ever changes what its
// trap exits with, the string stops containing this constant.
//
// Mutation that turns this red: replace harnessTerminationContract's renderer calls with literals for the
// image or either command, or change honorExitCode to anything the trap does not use.
func TestCanaryContractMirrorsWhatTheHarnessActuallyRenders(t *testing.T) {
	c := mustHarnessContract(t)

	// Compared against the measurement package's own constant rather than a literal copied to here. The copy
	// this replaces broke the moment the workload legitimately changed image, and it would not have caught the
	// case it existed for: a canary quietly pinned to a DIFFERENT digest still matched the prefix.
	if c.Image != queuelab.WorkloadImage {
		t.Fatalf("the canary probes image %q, which is not the %q the arm runs: a canary that qualifies "+
			"different bytes than the run submits qualifies nothing", c.Image, queuelab.WorkloadImage)
	}
	if !strings.Contains(c.Image, "@sha256:") {
		t.Fatalf("the workload image %q is not pinned by digest, so the same study re-run later could execute "+
			"different bytes", c.Image)
	}
	honor := strings.Join(c.HonorCommand, " ")
	ignore := strings.Join(c.IgnoreCommand, " ")
	for _, want := range []string{"signal.SIGTERM", "honor"} {
		if !strings.Contains(honor, want) {
			t.Fatalf("the honouring command %q does not contain %q, so it is not the workload that stops when "+
				"asked", honor, want)
		}
	}
	// honorExitCode is a local copy of a private constant, so it is checked against the rendered command's own
	// text rather than against itself: if the measurement package changes what its handler exits with, this
	// stops matching.
	if !strings.Contains(honor, fmt.Sprintf("exit(%d)", honorExitCode)) {
		t.Fatalf("the honouring command is %q; honorExitCode is %d and the workload's own handler must exit "+
			"with it, or the canary is looking for a code nothing produces", honor, honorExitCode)
	}
	// The two probes must be different probes. They share one script and differ by the contract argument, so
	// the check is on that argument: identical commands would make the contrast one between a workload and
	// itself, which is the single thing this canary exists to rule out.
	if honor == ignore {
		t.Fatalf("both probes run the same command %q, so the canary would compare a workload with itself", honor)
	}
	if !strings.HasSuffix(ignore, " ignore") || !strings.HasSuffix(honor, " honor") {
		t.Fatalf("the probes are not the two contract arms: honour=%q ignore=%q", honor, ignore)
	}
	if c.GraceSec != terminationGraceSec {
		t.Fatalf("the contract requires a %ds grace period, but the protocol's horizon is derived from %ds",
			c.GraceSec, terminationGraceSec)
	}
}

// The single most important property in this file: the canary qualifies the CONTRAST, not one half of it.
//
// A runtime that SIGKILLs every container the instant it is asked passes the honouring half for entirely the
// wrong reason — the Pod stops promptly, and 143 is what an untrapped SIGTERM leaves behind too — and it is
// precisely the runtime on which A-honor and A-ignore become the same experiment. Only the ignoring probe
// outlasting the grace period rules it out, so a canary that judged the honouring probe alone would qualify
// the one cluster this whole file exists to refuse.
//
// Mutation that turns this red: drop the ignoring probe's survival clause from judgeCanary (the honouring
// half is untouched and still passes, which is the point).
func TestCanaryRefusesAClusterThatStopsEverythingImmediately(t *testing.T) {
	honor := passingHonorProbe()
	// Both stopped at once, both with plausible codes. This is the whole failure: nothing about the honouring
	// probe alone looks wrong.
	honor.StoppedAfterMs = 120
	ignore := passingIgnoreProbe()
	ignore.StoppedAfterMs = 130

	failures := judgeCanary(honor, ignore)
	if len(failures) == 0 {
		t.Fatal("a cluster that killed both probes immediately qualified: A-honor and A-ignore are the same " +
			"experiment on it, and every run would report the contrast it structurally could not produce")
	}
	joined := strings.Join(failures, "\n")
	if !strings.Contains(joined, "ignoring probe") {
		t.Fatalf("the refusal must name the half that failed, got:\n%s", joined)
	}
	if strings.Contains(joined, "honouring probe") {
		t.Fatalf("the honouring probe did exactly what it was supposed to; naming it sends the operator after "+
			"the wrong workload:\n%s", joined)
	}
}

// The other direction, and the one the arm is actually built on: SIGTERM never reaching the workload. Both
// probes then behave like the ignoring one — killed at the grace deadline with 137 — and the honouring half
// has to say so.
//
// Mutation that turns this red: accept any exit code from the honouring probe, or drop its promptness clause.
func TestCanaryRefusesAClusterThatNeverDeliversTheSignal(t *testing.T) {
	honor := passingHonorProbe()
	honor.ExitCode = killExitCode
	honor.Reason = "Error"
	honor.StoppedAfterMs = 30500

	failures := judgeCanary(honor, passingIgnoreProbe())
	joined := strings.Join(failures, "\n")
	if len(failures) < 2 {
		t.Fatalf("a workload that was killed rather than signalled fails on its code AND on its promptness, "+
			"and both are worth telling the operator; got:\n%s", joined)
	}
	if !strings.Contains(joined, "SIGKILL") {
		t.Fatalf("the refusal must connect a 137 to the post-grace kill, or an operator reads it as an "+
			"arbitrary exit code:\n%s", joined)
	}
}

// The ignoring probe's exit code is a CLUSTER-SIDE cross-check on a duration this program measured itself,
// and until this test existed nothing pinned it: deleting the clause left the whole package green, because no
// fixture anywhere supplied a probe that outlasted the threshold on a code the kubelet does not produce.
//
// What it closes is narrow and real. StoppedAfterMs is wall-clock from the moment Delete returned to the
// moment a terminal status was first READ, so a stall between those two — a slow status update, an apiserver
// pause, a poll that lost its turn — inflates it. A probe killed promptly and observed late therefore reads
// as a survivor, and "the ignoring workload outlasted the grace period" is exactly the half that makes the
// contrast a contrast. 137 is the one part of the reading this program did not compute: the kubelet had to
// have taken the container out for it to say that.
//
// (There is a second guard, and it is why this is a narrow gap rather than an open door: both probes are read
// by one loop on one clock, so a GLOBAL stall inflates the honouring probe too and trips its upper bound.
// This clause covers the stall that reached only one of the two reads. See killExitCode.)
//
// Mutation that turns this red: delete the `ignore.ExitCode != killExitCode` clause from judgeCanary. Nothing
// else in the package notices.
func TestTheIgnoringProbesSurvivalHasToBeSomethingTheKubeletEnded(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int32
	}{
		// A clean exit: the sleeper finished on its own, or something stopped it in a way that let it exit
		// normally. Either way nothing had to kill it, so nothing was outlasted.
		{"exited zero", 0},
		// The honouring arm's own code on the IGNORING probe: the shell stopped on SIGTERM after all. A
		// duration alone cannot tell that from surviving to the deadline, and it is the reading a workload
		// whose two arms had drifted into one would produce.
		{"exited on SIGTERM like the honouring arm", honorExitCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ignore := passingIgnoreProbe()
			// Every other thing about this reading is textbook: it terminated, and its duration is past the
			// survival threshold. Only the code says the kubelet never had to end it.
			ignore.ExitCode = tc.code
			ignore.Reason = "Completed"

			failures := judgeCanary(passingHonorProbe(), ignore)
			if len(failures) == 0 {
				t.Fatalf("an ignoring probe that ended with %d — nothing killed it — qualified as having "+
					"outlasted the grace period; the only evidence for that was a duration this program "+
					"measured itself, and a read stall produces the same number", tc.code)
			}
			joined := strings.Join(failures, "\n")
			if !strings.Contains(joined, "not the 137") {
				t.Fatalf("the refusal must say which code was missing and why it matters, got:\n%s", joined)
			}
			if strings.Contains(joined, "honouring probe") {
				t.Fatalf("the honouring probe was impeccable; naming it sends the operator to the wrong "+
					"workload:\n%s", joined)
			}
		})
	}
}

// A probe whose ending was never seen is not evidence of anything, in either direction. The honouring one
// missing means nothing showed the cluster can stop a Pod; the ignoring one missing means there is nothing to
// compare against. Both must refuse, and neither may fall through to a number-based clause reading a zero.
//
// Mutation that turns this red: judge the exit code and duration without testing Terminated first. Be exact
// about what that produces, because the first version of this comment was not: the honouring half does not
// silently pass — the unobserved fixture's ExitCode of 0 still fails the 143 clause — so what is lost is the
// SENTENCE. The run is refused for "exited 0, not 143" about a probe that never exited at all, the recorded
// Unobserved reason is never quoted, and the operator is sent looking for a workload that mis-exited instead
// of for the reason nothing was seen. This test fails on the assertion that the reason survives, which is the
// thing actually at stake.
func TestCanaryRefusesAProbeItNeverSawStop(t *testing.T) {
	honor := passingHonorProbe()
	honor.Terminated = false
	honor.ExitCode = 0
	honor.StoppedAfterMs = 0
	honor.Unobserved = "it was still running when the budget expired"

	failures := judgeCanary(honor, passingIgnoreProbe())
	if len(failures) == 0 {
		t.Fatal("a canary whose honouring probe was never observed to stop qualified the worker: a zero " +
			"duration reads as the promptest possible stop, which is how absence of evidence becomes evidence")
	}
	if !strings.Contains(strings.Join(failures, "\n"), "still running when the budget expired") {
		t.Fatalf("the refusal must carry why nothing was observed, got %v", failures)
	}

	ignore := passingIgnoreProbe()
	ignore.Terminated = false
	ignore.Unobserved = "the Pod object was removed"
	if len(judgeCanary(passingHonorProbe(), ignore)) == 0 {
		t.Fatal("the honouring probe alone qualified the worker: with no ignoring reading there is no contrast, " +
			"which is the same state a SIGKILL-everything runtime produces")
	}
}

// The grace period nothing in this harness sets. The controller leaves terminationGracePeriodSeconds unset,
// so a run's Pods take the apiserver's default, and every number in spine.go's horizon is arithmetic over
// that default being 30. A cluster defaulting something else is not running this protocol.
//
// Mutation that turns this red: drop the grace clause from judgeCanary. Both probes still show a textbook
// contrast, so nothing else in the function objects.
func TestCanaryRefusesAClusterWhoseDefaultGracePeriodIsNotTheProtocolsOwn(t *testing.T) {
	honor, ignore := passingHonorProbe(), passingIgnoreProbe()
	honor.GraceSec, ignore.GraceSec = 60, 60
	// Scaled to the grace period it was actually given, so the contrast itself is impeccable: only the window
	// is wrong.
	ignore.StoppedAfterMs = 60400

	failures := judgeCanary(honor, ignore)
	if len(failures) == 0 {
		t.Fatalf("a cluster defaulting a %ds grace period qualified against a protocol whose horizon assumes "+
			"%ds", honor.GraceSec, terminationGraceSec)
	}
	if !strings.Contains(strings.Join(failures, "\n"), "nothing in this harness sets one") {
		t.Fatalf("the refusal has to say the default is the cluster's rather than this tool's, or an operator "+
			"goes looking for the flag that set it: %v", failures)
	}
}

// A healthy cluster must actually pass, or the gate is a wall. This is the reading a working kind cluster
// produces and it has to qualify, including the fact that the ignoring probe's exit code is the SIGKILL its
// survival is proven by.
//
// It is the positive control for every refusal above: a threshold tightened until nothing real can meet it
// would leave all of them green and this one alone red.
//
// Mutation that turns this red: set canaryPromptWithin to zero, or require the ignoring probe to outlast more
// than the grace period it is given.
func TestCanaryQualifiesTheReadingAHealthyClusterProduces(t *testing.T) {
	if failures := judgeCanary(passingHonorProbe(), passingIgnoreProbe()); len(failures) > 0 {
		t.Fatalf("the textbook contrast was refused, so no cluster could ever qualify: %v", failures)
	}
}

// A run may not stand on a worker nobody has qualified, and the refusal has to be the one an operator can act
// on: it names the missing thing and prints the command that produces it.
//
// Mutation that turns this red: return (nil, nil) from checkTerminationCanary when the annotation is absent,
// which is the natural "there is nothing to check here" reading and is how a gate becomes decorative.
func TestQualifyRefusesAWorkerNoCanaryHasEverQualified(t *testing.T) {
	// Explicitly blanked rather than merely omitted: node() carries a passing qualification the way it carries
	// a Ready condition, so a test about the absence has to create the absence.
	bare := node(nil, map[string]string{canaryAnnotationKey: ""})

	q, err := qualify(bare, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker whose ability to stop a Pod nothing had ever checked was accepted: the arms differ " +
			"only by whether the workload stops when asked, so this is the one property whose absence makes the " +
			"whole comparison meaningless while every number still looks fine")
	}
	if !strings.Contains(err.Error(), "-termination-canary -worker platform-worker") {
		t.Fatalf("the refusal must hand over the runnable command that fixes it, got %q", err)
	}
	if q.TerminationCanary != nil {
		t.Fatal("no reference may be recorded for a qualification that was never consulted")
	}
}

// A recorded canary that FAILED is worse than none, and must refuse louder rather than quieter: this cluster
// has been shown not to produce the contrast. The refusal has to quote what was found, because "re-run the
// canary" is the wrong advice when the canary already ran and said no.
//
// Mutation that turns this red: treat a recorded document as a qualification whenever it decodes, ignoring
// Failures.
func TestQualifyRefusesAWorkerWhoseRecordedCanaryFailed(t *testing.T) {
	failed := node(nil, map[string]string{canaryAnnotationKey: canaryAnnotation(t, func(q *canaryQualification) {
		q.Ignore.StoppedAfterMs = 90
		q.Failures = judgeCanary(q.Honor, q.Ignore)
	})})

	_, err := qualify(failed, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker carrying a canary that failed was accepted: the document proving the arms collapse " +
			"on this cluster was read as the document proving they do not")
	}
	if !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), "ignoring probe") {
		t.Fatalf("the refusal must quote what the canary found, got %q", err)
	}
}

// A canary that failed on a combination this run does not use is a statement about a different machine, and
// the refusal has to say so. Tested the other way round — failures before the key — a stale failed document
// produced the most alarming sentence this file can print, about a cluster it was not taken on, in the one
// branch that offered the operator no next step.
//
// Mutation that turns this red: test Failures before the key in checkTerminationCanary.
func TestAFailedCanaryFromAnotherCombinationIsReportedAsTheWrongCombination(t *testing.T) {
	stale := node(nil, map[string]string{canaryAnnotationKey: canaryAnnotation(t, func(q *canaryQualification) {
		// It failed, and it was taken on an image this run does not submit. Both are true of the document; only
		// one of them is true of this run.
		q.Key.Image = "busybox:1.34@sha256:1111111111111111111111111111111111111111111111111111111111111111"
		q.Ignore.StoppedAfterMs = 90
		q.Failures = judgeCanary(q.Honor, q.Ignore)
	})})

	_, err := qualify(stale, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker whose only qualification was taken on another image was accepted")
	}
	if !strings.Contains(err.Error(), "image: this run needs") {
		t.Fatalf("the refusal must say the reading is about a different combination, got %q", err)
	}
	if strings.Contains(err.Error(), "shown not to produce the contrast") {
		t.Fatalf("the refusal tells the operator this cluster cannot stop a Pod, on the evidence of a reading "+
			"taken against an image this run does not submit; that is a claim the document cannot support: %q", err)
	}
	if !strings.Contains(err.Error(), "-termination-canary -worker platform-worker") {
		t.Fatalf("the refusal must hand over the command that resolves it, got %q", err)
	}

	// And a canary that failed on THIS run's own combination still says the alarming thing, because there it
	// is true — with a next step of its own, which that branch used to lack.
	own := node(nil, map[string]string{canaryAnnotationKey: canaryAnnotation(t, func(q *canaryQualification) {
		q.Ignore.StoppedAfterMs = 90
		q.Failures = judgeCanary(q.Honor, q.Ignore)
	})})
	_, err = qualify(own, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker whose canary failed on this run's own combination was accepted")
	}
	if !strings.Contains(err.Error(), "shown not to produce the contrast") {
		t.Fatalf("a canary that failed on this run's own combination must say exactly that, got %q", err)
	}
	if !strings.Contains(err.Error(), "fix the cluster") {
		t.Fatalf("even the branch whose fix is not a re-run needs a next step, got %q", err)
	}
}

// The qualification is a property of a COMBINATION, and every field of that combination has to be able to
// refuse on its own. A bumped sleeper digest, a re-imaged node, a cluster upgrade and a recreated Node are
// four completely different next steps, so the refusal names the field rather than saying the key differs.
//
// Each expected string is the field's own name FOLLOWED BY the phrase only its comparison emits, and the
// bare field name is not good enough — which a review caught here rather than in theory. The refusal used to
// end with boilerplate reading "a property of one (image, cluster, runtime, harness) combination", so
// Contains(err, "image") passed on a refusal that named no field at all, and dropping the image comparison —
// the one the brief singled out as most likely to be updated in one place and not the other — was green
// everywhere. The boilerplate no longer says "image", and the assertions no longer depend on it not saying it.
//
// Mutation that turns this red: drop any single comparison from keyDifferences (each row catches its own), or
// compare the key with a single equality that reports no field at all.
func TestQualifyRefusesACanaryTakenOnADifferentCombination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*canaryQualification)
		want   string
	}{
		{"a bumped sleeper image", func(q *canaryQualification) {
			q.Key.Image = "busybox:1.37@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}, "image: this run needs"},
		{"a changed honouring command", func(q *canaryQualification) {
			q.Key.HonorCommand = []string{"sh", "-c", "sleep 600"}
		}, "honoring command: this run needs"},
		{"a changed ignoring command", func(q *canaryQualification) {
			q.Key.IgnoreCommand = []string{"sh", "-c", "trap 'exit 143' TERM; sleep 600 & wait"}
		}, "ignoring command: this run needs"},
		{"a node recreated under the same name", func(q *canaryQualification) {
			q.Key.NodeUID = "uid-some-other-machine"
		}, "node UID: this run needs"},
		{"a kubelet upgrade", func(q *canaryQualification) {
			q.Key.KubeletVersion = "v1.33.2"
		}, "kubelet version: this run needs"},
		{"a runtime change", func(q *canaryQualification) {
			q.Key.ContainerRuntime = "cri-o://1.30.0"
		}, "container runtime: this run needs"},
		{"a canary taken under another grace period", func(q *canaryQualification) {
			q.Key.GraceSec = 60
		}, "termination grace period: this run's timing"},
		// The one that used to change nothing. A preStop hook or an explicit grace period added to the
		// controller's Job template changes what a run's Pods do when they are asked to stop, and every other
		// field of this key stays identical while it does.
		{"a changed operator pod template", func(q *canaryQualification) {
			q.Key.PodTemplateHash = "0000000000000000000000000000000000000000000000000000000000000000"
		}, "operator pod template: this build renders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := node(nil, map[string]string{canaryAnnotationKey: canaryAnnotation(t, tc.mutate)})
			_, err := qualify(n, nil, testReq(2), mustHarnessContract(t))
			if err == nil {
				t.Fatalf("%s did not invalidate the qualification: it is a statement about one combination and "+
					"says nothing about another", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name %q, so the operator cannot tell which of four different "+
					"things changed: %v", tc.want, err)
			}
		})
	}
}

// Every field of the key has to be able to refuse on its own, INCLUDING one added after this was written.
//
// The field list in keyDifferences is prose and prose falls behind: a field added to canaryKey without a line
// there would be compared by nothing, and a qualification taken on a different value of it would match. So the
// equality is the authority and the names are the explanation, and this test walks the struct by reflection
// rather than listing what it knows about — a new field is covered the day it is added, whether or not
// anybody remembers this test exists.
//
// The assertion is that no field reaches canaryKeyMatches' FALLBACK, and it replaced one that could never
// fire: the fallback synthesises a diff whenever keyDifferences returns none, so `len(diffs) == 0` after a
// non-match was structurally unreachable and the mutation the comment named turned it red for no field at
// all. The net is still there and still makes the refusal correct for a field nobody named — that is its job
// — but a net that silently absorbs every missing comparison is also where the diagnosis goes to die, so the
// sentinel is what separates "refused and said why" from "refused and could not".
//
// Mutation that turns this red: drop any single comparison from keyDifferences (that field then reaches the
// fallback), or let the named list be the verdict as well as the explanation — `return len(diffs) == 0,
// diffs` — with any one comparison missing, which makes the mismatch match.
func TestEveryFieldOfTheKeyCanRefuseOnItsOwn(t *testing.T) {
	base := passingCanary().Key
	rt := reflect.TypeFor[canaryKey]()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		got := base
		v := reflect.ValueOf(&got).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString(v.String() + "-changed")
		case reflect.Int64, reflect.Int32:
			v.SetInt(v.Int() + 1)
		case reflect.Slice:
			v.Set(reflect.ValueOf([]string{"something", "else"}))
		default:
			t.Fatalf("canaryKey.%s is a %s, which this test does not know how to vary; a field the drift check "+
				"skips is a field that can change without invalidating a qualification", f.Name, v.Kind())
		}
		matched, diffs := canaryKeyMatches(base, got)
		if matched {
			t.Fatalf("a qualification differing only in %s matched: the key is what says this reading was taken "+
				"on the combination this run needs", f.Name)
		}
		for _, d := range diffs {
			if strings.Contains(d, unnamedKeyDifference) {
				t.Fatalf("a mismatch on %s fell through to the unnamed-difference fallback: the refusal is still "+
					"correct, but an operator is told the key differs and not that it is %s — which is the "+
					"difference between re-taking a reading and hunting for what changed", f.Name, f.Name)
			}
		}
	}
	if matched, _ := canaryKeyMatches(base, base); !matched {
		t.Fatal("a key did not match itself, so no qualification could ever be consulted")
	}
}

// The trap the required-field sweep exists for. A qualification document that decoded into zero values would
// carry a zero key, and a zero key compares equal to another zero key — so a bug that built the expected side
// out of nothing would match anything, and the gate would still be present and no longer checking. The floor
// is in decodeCanary rather than in the comparison, so nothing downstream can be handed one.
//
// This test is written as one blanked field at a time against an OTHERWISE COMPLETE document, and that shape
// is the correction of a defect in its own first version: it originally listed four hand-written documents,
// every one of which was refused by a different clause — the command check or the schema check — so removing
// the required-field sweep left it green. A trap that passes under the mutation its own comment names is a
// trap this lineage has already shipped once, so each swept field now has to be the only thing wrong.
//
// Mutation that turns this red: drop the required-field sweep from decodeCanary (each blanked-field case
// below), or drop either structural clause under it (the last two cases).
func TestCanaryDocumentWithNothingInItIsNotAQualification(t *testing.T) {
	for _, blank := range []struct {
		field  string
		mutate func(*canaryQualification)
	}{
		{"canaryID", func(q *canaryQualification) { q.CanaryID = "" }},
		{"node", func(q *canaryQualification) { q.Node = "" }},
		{"qualifiedAt", func(q *canaryQualification) { q.QualifiedAt = "" }},
		{"key.image", func(q *canaryQualification) { q.Key.Image = "" }},
		{"key.nodeUID", func(q *canaryQualification) { q.Key.NodeUID = "" }},
		{"key.kubeletVersion", func(q *canaryQualification) { q.Key.KubeletVersion = "" }},
		{"key.containerRuntime", func(q *canaryQualification) { q.Key.ContainerRuntime = "" }},
		{"key.podTemplateHash", func(q *canaryQualification) { q.Key.PodTemplateHash = "" }},
	} {
		raw := canaryAnnotation(t, blank.mutate)
		if _, err := decodeCanary(raw); err == nil {
			t.Fatalf("a qualification with %s blank and everything else intact decoded: a document missing any "+
				"one of these identifies nothing a reader could go and check, and its key would compare equal to "+
				"another key missing the same thing", blank.field)
		}
	}

	// The structural clauses under the sweep, each of which is the only thing wrong with its own document.
	for _, tc := range []struct {
		name   string
		mutate func(*canaryQualification)
	}{
		{"no honouring command", func(q *canaryQualification) { q.Key.HonorCommand = nil }},
		{"no ignoring command", func(q *canaryQualification) { q.Key.IgnoreCommand = nil }},
		{"no grace period at all", func(q *canaryQualification) { q.Key.GraceSec = 0 }},
	} {
		if _, err := decodeCanary(canaryAnnotation(t, tc.mutate)); err == nil {
			t.Fatalf("a qualification with %s decoded; it establishes nothing about a workload and must not be "+
				"readable as though it did", tc.name)
		}
	}

	// And the empty document, which is what a zero-valued key would arrive as, beside a document from the
	// version before the operator's template joined the key: the first is refused by the sweep and the second
	// by the version, and both would otherwise be read as readings they are not.
	for _, raw := range []string{`{"schema":2}`, `{"schema":1,"canaryID":"c","node":"n","qualifiedAt":"t"}`} {
		if _, err := decodeCanary(raw); err == nil {
			t.Fatalf("%s decoded as a qualification", raw)
		}
	}

	// And the whole document round-trips, so the strictness above is not simply refusing everything.
	raw, err := encodeCanary(passingCanary())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeCanary(raw)
	if err != nil {
		t.Fatalf("a document this build wrote must decode: %v", err)
	}
	if got.CanaryID != "canary-0001" || got.Key.Image != mustHarnessContract(t).Image {
		t.Fatalf("round trip lost the qualification's identity: %+v", got)
	}
}

// An unreadable qualification is not a missing one and must not be treated as either a pass or a plain
// absence: it is a document this tool cannot act on, sitting where the answer should be.
//
// Mutation that turns this red: ignore the decode error and fall through to "no canary recorded", which reads
// as a node nobody has qualified rather than one carrying something nobody can read.
func TestQualifyRefusesACanaryItCannotRead(t *testing.T) {
	broken := node(nil, map[string]string{canaryAnnotationKey: `{"schema":1,"canaryID":"c",` +
		`"unexpectedField":"from a newer build"}`})

	_, err := qualify(broken, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker carrying an unreadable qualification was accepted")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("the refusal must distinguish an unreadable document from an absent one, got %q", err)
	}
	if !strings.Contains(err.Error(), "unexpectedField") {
		t.Fatalf("the refusal must quote what it found, or the operator has nothing to inspect: %q", err)
	}
}

// The passing path, and the reference it leaves behind. A run that stood on a qualification has to say which
// one in its own record, or a reader holding the file has to go and ask a cluster that may no longer exist.
//
// Mutation that turns this red: return a bare nil reference from checkTerminationCanary on the passing path,
// or drop the assignment in qualify. The run still proceeds, and its record silently stops saying what it
// stood on — which is also what makes environmentEstablished refuse it.
func TestAQualifiedWorkerRecordsWhichCanaryTheRunStoodOn(t *testing.T) {
	q, err := qualify(node(nil, nil), nil, testReq(2), mustHarnessContract(t))
	if err != nil {
		t.Fatalf("a worker with a passing canary for exactly this combination was refused: %v", err)
	}
	if q.TerminationCanary == nil {
		t.Fatal("the qualification carries no reference to the canary it consulted, so the record cannot say " +
			"what the run stood on")
	}
	ref := q.TerminationCanary
	if ref.CanaryID != "canary-0001" || ref.QualifiedAt != "2026-08-15T00:00:00Z" {
		t.Fatalf("the reference must identify the qualification, got %+v", ref)
	}
	if ref.Key.Image != mustHarnessContract(t).Image || ref.Key.NodeUID != "uid-node" {
		t.Fatalf("the reference must carry the combination that was qualified, got %+v", ref.Key)
	}
	// The measured contrast travels with it: an id alone points into a cluster the reader may not have, and
	// these two numbers are what the qualification actually established.
	if ref.HonorStoppedAfterMs != 800 || ref.IgnoreStoppedAfterMs != 30400 {
		t.Fatalf("the reference must carry the separation that was measured, got %+v", ref)
	}
}

// The record's own verdict has to fall when the canary reference does. A run refused at this gate carries a
// qualification block full of perfectly good numbers — the node was Ready, schedulable, uncontended and large
// enough — so a verdict derived from those alone would call the environment established for a run that was
// refused precisely because it was not.
//
// Mutation that turns this red: drop the TerminationCanary clause from environmentEstablished. Every other
// field of the fixture below is the passing one, so nothing else in deriveValidity objects and the record
// calls itself admissible.
func TestARecordWhoseRunStoodOnNoCanaryDoesNotCallItselfAdmissible(t *testing.T) {
	q := testQualification()
	q.TerminationCanary = nil

	v := deriveValidity(outcome{Disposition: dispChecksPassed}, nil, q, testWindow(), testObservation(), nil, false)
	if v.Verdict != verdictRefused {
		t.Fatalf("a run that stood on no termination qualification called itself %q; every number in its "+
			"environment block is fine, which is exactly why the verdict must not be derived from them alone",
			v.Verdict)
	}
	found := false
	for _, f := range v.Failures {
		if f == failureEnvironment {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failure must be named against the claim it belongs to, got %v", v.Failures)
	}

	// And the passing fixture still passes, or the clause above would be refusing everything.
	if got := deriveValidity(outcome{Disposition: dispChecksPassed}, nil, testQualification(), testWindow(),
		testObservation(), nil, false); got.Verdict != verdictAdmissible {
		t.Fatalf("a run that did stand on one was refused: %v", got.Failures)
	}
}

// A document claiming this schema while carrying a reference that names nothing is the route the version bump
// alone does not close: a zero canaryReference satisfies environmentEstablished's non-nil test — the strongest
// thing the block can say about the mechanism the arms differ by — while identifying no qualification and
// naming no image, so a reader could not go and check it against anything.
//
// Mutation that turns this red: drop the reference guard from decodeRunRecord.
func TestDecodeRefusesACanaryReferenceThatNamesNothing(t *testing.T) {
	q := testQualification()
	q.TerminationCanary = &canaryReference{}
	rec := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, q, testWindow(), testObservation(), nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("a record whose canary reference identifies no qualification was read as evidence that one was " +
			"consulted")
	}
}

// The same guard one field at a time, which is the shape the sibling above cannot have: it blanks the whole
// reference, so CanaryID alone refuses it and the clauses beside that one are never the thing under test. A
// document whose reference names its canary, its image and its node and is silent only about the operator's
// template is the one a projection that dropped a field, or a hand-edited file, actually produces.
//
// Mutation that turns this red: drop the PodTemplateHash clause from decodeRunRecord's reference guard. The
// version check does not cover it — this document claims version 6 — and the record then reads as a run gated
// on a template it names nothing about.
func TestDecodeRefusesACanaryReferenceThatNamesNoTemplate(t *testing.T) {
	q := testQualification()
	ref := *testCanaryReference()
	ref.Key.PodTemplateHash = ""
	q.TerminationCanary = &ref
	rec := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, q, testWindow(), testObservation(), nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("a record whose canary reference fingerprints no pod template was read as evidence that the " +
			"reading it stood on covered the one the operator renders")
	}
}

// The schema bump, in the direction it exists for. A version-4 record decodes into today's struct without
// complaint and leaves TerminationCanary nil — which is not a spare slot but exactly what THIS build writes
// for a run it refused at this gate. Without the bump every record every earlier build wrote, including the
// good ones, reads as a run that faced the gate and did not get past it.
//
// Mutation that turns this red: leave recordSchemaVersion at 4.
//
// The constant has moved on since — the pod template key took it to 6 — so what is pinned here is the floor
// and not the exact value: any version below 5 is a build that cannot tell a run refused at this gate from one
// that could not ask. TestARecordFromBeforeTheTemplateKeyIsRefusedRatherThanReinterpreted pins the next bump.
func TestARecordFromBeforeTheCanaryGateIsRefusedRatherThanReinterpreted(t *testing.T) {
	if recordSchemaVersion < 5 {
		t.Fatalf("recordSchemaVersion is %d; the canary reference was added to the qualification block, and a "+
			"reader that cannot tell a run refused for it from a build that could not ask has been told nothing",
			recordSchemaVersion)
	}
	older := []byte(`{"schemaVersion":4,"runID":"r7","arm":"A-honor","disposition":"checks-passed",` +
		`"validity":{"verdict":"refused","failures":["environment-not-established"]}}`)
	if _, err := decodeRunRecord(older); err == nil {
		t.Fatal("a record written before this gate existed decoded under today's rules, so its silence about " +
			"the canary reads as a run that was refused for one")
	}
}

// The revision is recorded and deliberately not keyed, and the reason is measurable rather than stylistic: a
// `go test` binary carries no vcs settings at all, so a key that included one could not be matched from half
// the ways this code is exercised. What the field must never do is decode as an empty string a reader cannot
// tell from a writer that forgot it.
//
// Mutation that turns this red: return "" instead of the sentinel when the build carries no stamp.
func TestHarnessRevisionIsHonestAboutBeingAbsent(t *testing.T) {
	got := harnessRevision()
	if got == "" {
		t.Fatal("the harness revision must never be empty in a document somebody reads later: an empty string " +
			"there cannot be told apart from a writer that skipped the field")
	}
	// Under `go test` this is the expected value, and asserting it is what pins the claim the key's own comment
	// makes: the stamp genuinely is unavailable here, which is why it is not what the match turns on.
	if got != unstampedRevision && len(got) < 7 {
		t.Fatalf("a stamped revision should be a commit hash and an unstamped one the sentinel, got %q", got)
	}
}

// The template the key covers has to be the OPERATOR'S rendering rather than a description of it, because the
// one failure this field exists to catch is the two drifting apart. So every assertion below is against
// something only internal/controller's BuildJob puts there: an MLTrainingJob carries no container name, no
// restart policy and no resource limits at all, and all three are in what gets hashed.
//
// The device count is checked against the probe's own 7 and not against 1, which is what the probe's three
// distinct numbers buy: parallelism is 3 and completions 5, so a rendering that reached for the wrong one of
// them shows up as a wrong number here instead of as an indistinguishable 1.
//
// Mutation that turns this red: have renderedPodTemplate build a template locally instead of calling
// controller.BuildJob — which is the re-derivation this field exists to make pointless — or point it at the
// MLTrainingJob's own spec, which carries none of the three.
func TestTheTemplateTheKeyCoversIsTheOneTheOperatorRenders(t *testing.T) {
	probe := templateProbeJob()
	tpl := renderedPodTemplate(probe)

	if len(tpl.Spec.Containers) != 1 {
		t.Fatalf("the hashed template carries %d containers; a run's Pod is one trainer, and this is not what "+
			"the operator renders", len(tpl.Spec.Containers))
	}
	ctr := tpl.Spec.Containers[0]
	if ctr.Name != "trainer" {
		t.Fatalf("the hashed template names its container %q; an MLTrainingJob carries no container name at "+
			"all, so a template that does not say \"trainer\" did not come from the operator's renderer", ctr.Name)
	}
	if tpl.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("the hashed template has restart policy %q; the operator pins Never and the MLTrainingJob says "+
			"nothing about restarting, so this template is not the operator's", tpl.Spec.RestartPolicy)
	}
	gpu := ctr.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if gpu.Value() != int64(probe.Spec.GPUCount) {
		t.Fatalf("the hashed template limits nvidia.com/gpu to %d and the probe asks for %d; the device limit is "+
			"the operator's own translation of gpuCount, and a template carrying another number was rendered "+
			"from something else", gpu.Value(), probe.Spec.GPUCount)
	}
	if ctr.Image != probe.Spec.Image {
		t.Fatalf("the hashed template runs %q, not the probe's %q, so it was rendered from another job",
			ctr.Image, probe.Spec.Image)
	}
	if strings.Join(ctr.Command, " ") != strings.Join(probe.Spec.Command, " ") {
		t.Fatalf("the hashed template runs %v, not the probe's %v", ctr.Command, probe.Spec.Command)
	}
}

// The property the field is bought for: anything the operator adds to that template about STOPPING changes the
// key, so a reading taken before it stops matching and the run refuses instead of measuring something else.
//
// The first three rows are the three the brief named as the realistic additions, and they are the whole reason
// this is not an empty gate: each of them changes what a run's Pod does when it is deleted, and before this
// field every one of them left every field of the key identical. The rest are there because the argument is
// not really about those three — it is that the template is hashed WHOLE, so an added env var or a second
// container is caught by the same mechanism and nobody has to have anticipated it.
//
// Mutation that turns this red: return a constant from podTemplateHashOf, or hash any PART of the template
// rather than the whole of it — hashing just the container list turns rows 1, 3, 4 and 7 red, since the grace
// period, the shared PID namespace, the restart policy and a template annotation all sit outside the
// containers. That is the argument for hashing everything rather than the fields that looked relevant.
func TestEveryAdditionToTheOperatorsTemplateChangesTheKey(t *testing.T) {
	base := renderedPodTemplate(templateProbeJob())
	baseHash := podTemplateHashOf(base)
	if len(baseHash) != 64 {
		t.Fatalf("the template hash is %q; a key field that is not a hash is one nothing can be compared on",
			baseHash)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodTemplateSpec)
	}{
		{"an explicit terminationGracePeriodSeconds", func(p *corev1.PodTemplateSpec) {
			p.Spec.TerminationGracePeriodSeconds = new(int64(60))
		}},
		{"a preStop hook", func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
				PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sleep", "5"}}},
			}
		}},
		{"shareProcessNamespace", func(p *corev1.PodTemplateSpec) {
			p.Spec.ShareProcessNamespace = new(true)
		}},
		{"a changed restart policy", func(p *corev1.PodTemplateSpec) {
			p.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
		}},
		{"an added environment variable", func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "GRACE", Value: "60"}}
		}},
		{"a second container", func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "x"})
		}},
		{"a template annotation", func(p *corev1.PodTemplateSpec) {
			p.Annotations = map[string]string{"queuelab.gpu-platform/anything": "1"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := base.DeepCopy()
			tc.mutate(got)
			if podTemplateHashOf(*got) == baseHash {
				t.Fatalf("%s left the key identical: a run's Pods would carry it, the recorded reading would go "+
					"on matching, and the run would report a contrast it was no longer producing", tc.name)
			}
		})
	}
}

// A hash that moved with the row a run submits would be useless: every run would refuse the reading before it,
// and the field would be a per-run ritual rather than a key. Two things keep it still, and both are testable.
//
// Only the Job's TEMPLATE is hashed, so the name and namespace — which are per-row, since a trace row's name is
// the object's name — cannot reach it. And the template is rendered at sentinel values rather than at the
// harness's own workload, so the pinned image digest and the arm's rendered command are keyed by their own
// fields and are not also inside this one, where a digest bump would be reported a second time as a template
// change.
//
// Mutation that turns this red: let the Job's own identity into what is hashed — renderedPodTemplate stamping
// j.Name and j.Namespace onto the template it returns, which is what hashing the whole rendered Job rather
// than its Spec.Template would amount to (the first half); or render the probe from
// harnessTerminationContract's own MLTrainingJob instead of the sentinel (the second half, where the row's
// duration would put a per-row number in the command as well).
func TestTheTemplateKeyDoesNotMoveWithWhatARunSubmits(t *testing.T) {
	probe := templateProbeJob()
	other := templateProbeJob()
	other.Name = "row-000017-tenant-b"
	other.Namespace = "queuelab-run-r7"
	other.Spec.Queue = "lq-tenant-b"
	if podTemplateHashOf(renderedPodTemplate(probe)) != podTemplateHashOf(renderedPodTemplate(other)) {
		t.Fatal("the template key changed with the name, namespace and queue of the job it was rendered from; " +
			"those are per-row, so every run would refuse the reading taken before it")
	}

	c := mustHarnessContract(t)
	tpl := renderedPodTemplate(probe)
	ctr := tpl.Spec.Containers[0]
	if ctr.Image == c.Image {
		t.Fatalf("the template is keyed at the harness's own image %q, which is already a key field of its own; "+
			"a digest bump would then be reported twice, once as the image and once as a template change",
			ctr.Image)
	}
	for _, cmd := range [][]string{c.HonorCommand, c.IgnoreCommand} {
		if strings.Join(ctr.Command, " ") == strings.Join(cmd, " ") {
			t.Fatalf("the template is keyed at the arm's own command %v, which is already a key field of its "+
				"own and carries the row's duration besides", ctr.Command)
		}
	}
}

// The probe is only as good as the fields it exercises, and the field it does not exercise is the one that
// goes wrong silently. A spec field left at its zero value renders whatever the operator does for an unset
// field, so a later line that adds something only when that field IS set — gpuClass becoming a nodeSelector is
// the obvious one — would be taken by every run and skipped by the key.
//
// This walks the spec by reflection rather than listing what it knows about, for the reason
// TestEveryFieldOfTheKeyCanRefuseOnItsOwn does: a field added to the CRD after this was written is covered on
// the day it is added, whether or not anybody remembers this test exists.
//
// Mutation that turns this red: drop any field from templateProbeJob's literal, or add a field to
// MLTrainingJobSpec without giving the probe a value for it.
func TestTheTemplateProbeLeavesNoFieldOfTheSpecUnexercised(t *testing.T) {
	spec := reflect.ValueOf(templateProbeJob().Spec)
	for i := 0; i < spec.NumField(); i++ {
		if spec.Field(i).IsZero() {
			t.Fatalf("templateProbeJob leaves MLTrainingJobSpec.%s at its zero value; the operator's template is "+
				"keyed at this one input, so anything it renders only when that field is set is outside the key",
				spec.Type().Field(i).Name)
		}
	}
}

// The schema bump on the canary document itself, and what it does and does not buy — which is worth being
// exact about, because the honest answer is narrower than "it is what refuses the old document".
//
// A version-1 reading carries no podTemplateHash, and it would be refused with or without the bump:
// decodeCanary's required-field sweep stops an empty hash on its own. What the version buys is the DIAGNOSIS.
// "This document is from before the template was keyed" and "a field of this document is empty" are the same
// outcome to a parser and completely different sentences to an operator deciding whether the node is carrying
// an old reading or a corrupted one.
//
// Mutation that turns this red: leave canarySchema at 1 — the first assertion. It is asserted on the constant
// rather than left implicit in the refusal precisely because the sweep would otherwise cover for it, which is
// the shape of guard-overlap this lineage has already been caught by once. Dropping the sweep entry AND the
// bump together turns the second half red instead: the document then decodes, and the refusal an operator
// meets names a template change that never happened.
func TestACanaryFromBeforeTheTemplateKeyIsRefusedRatherThanReinterpreted(t *testing.T) {
	if canarySchema != 2 {
		t.Fatalf("canarySchema is %d; the operator's pod template joined the key, and a document that predates "+
			"it cannot be told apart from one whose template differs", canarySchema)
	}
	older := passingCanary()
	older.Schema = 1
	older.Key.PodTemplateHash = ""
	raw, err := json.Marshal(older)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	n := node(nil, map[string]string{canaryAnnotationKey: string(raw)})
	_, err = qualify(n, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a reading taken before the operator's template was part of the key was stood on")
	}
	// The refusal has to be the unreadable-document one, not the wrong-combination one: the reading is not about
	// a different template, it is about a build in which the template was not a fact the reading carried.
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("the refusal must say the document cannot be read rather than blame the template, got: %v", err)
	}
}

// And the same bump on the run record, which carries the key inside its canary reference.
//
// The document below carries NO qualification block at all, which is the case the reference guard cannot
// reach: with no reference there is nothing for it to sweep, so the version is the only thing that refuses it.
// A version-5 record that does carry a reference is stopped by the guard as well, and there the version buys
// the diagnosis rather than the refusal — recordSchemaVersion's own comment argues that, and it is thinner
// than the four bumps before it.
//
// The floor is asserted rather than an exact version, so a LATER deliberate bump does not have to edit the
// test that documents an EARLIER one. Each bump brings its own document that must stop decoding; this one
// owns the version-5 document below, and the newest bump owns its own test beside this file.
//
// Mutation that turns this red: leave recordSchemaVersion at 5 — both halves, the constant and the document.
func TestARecordFromBeforeTheTemplateKeyIsRefusedRatherThanReinterpreted(t *testing.T) {
	if recordSchemaVersion < 6 {
		t.Fatalf("recordSchemaVersion is %d; the pod template joined the key a record's reference carries, and "+
			"a reader cannot tell a run gated on the template from one that could not be", recordSchemaVersion)
	}
	older := []byte(`{"schemaVersion":5,"runID":"r7","arm":"A-honor","disposition":"checks-passed",` +
		`"validity":{"verdict":"refused","failures":["environment-not-established"]}}`)
	if _, err := decodeRunRecord(older); err == nil {
		t.Fatal("a record written before the operator's template was keyed decoded under today's rules, so its " +
			"silence about the template reads as a run that was gated on one")
	}
}
