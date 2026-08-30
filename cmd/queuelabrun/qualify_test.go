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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// gpuPod is a Pod that asks for devices, in the ordinary shape: the request named on the container.
func gpuPod(namespace, name, nodeName string, gpus int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "train",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{gpuResourceName: *resource.NewQuantity(gpus, resource.DecimalSI)},
					Limits:   corev1.ResourceList{gpuResourceName: *resource.NewQuantity(gpus, resource.DecimalSI)},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// simulatorPod is the gpu-simulator DaemonSet's Pod as config/device-plugin/daemonset.yaml actually declares
// it: 10m of CPU, 32Mi of memory, a toleration for everything, and no device request at all.
//
// It is written out here rather than described in a comment because it is the single most dangerous input
// this check has. The simulator is a device PLUGIN — it is where nvidia.com/gpu on these nodes comes from —
// and it runs on every node, so a predicate that rejected it would refuse every run on every cluster this lab
// has ever used, including the one the reviewer is about to point at it.
func simulatorPod(nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "system",
			Name:      "gpu-simulator-abcde",
			UID:       types.UID("sim-uid"),
			Labels: map[string]string{
				"app.kubernetes.io/name":      "gpu-platform-control-plane",
				"app.kubernetes.io/component": "gpu-simulator",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:          nodeName,
			PriorityClassName: "system-node-critical",
			Tolerations:       []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:  "gpu-simulator",
				Image: "gpu-simulator:latest",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// testReq is a requirement whose provenance is not what the test is about. The tests that DO care about the
// provenance build a gpuRequirement by hand instead, so that this helper can never be what makes one of them
// pass.
func testReq(n int64) gpuRequirement {
	return gpuRequirement{Total: n, BoundBy: boundByQuotaSum, From: "test"}
}

// testFixtures renders the real fixture set for an arm, so the requirement under test is the one the run
// would actually apply rather than a hand-built imitation of it.
func testFixtures(t *testing.T, study queuelab.Study, variant string) *queuelab.FixtureSet {
	t.Helper()
	fs, err := queuelab.BuildFixtures(study, variant, queuelab.FixtureIdentity{TxID: "tx-1111", RunID: "r7", Namespace: "queuelab-r7"})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	return fs
}

// The requirement has to be COMPUTED from this run's own protocol, and the two real studies cannot prove
// that on their own: reclaim (two ClusterQueues at nominal 1, largest row 1) and FIFO (one queue at nominal
// 2, largest row 2) both come to 2, so `return 2` passes for both and the check silently stops being about
// this run's arm. The synthetic set is what makes the summation observable — 1 + 3 is a number no constant
// anyone would plausibly type produces.
//
// The quota on a foreign flavor is there for the same reason: only this run's flavor is pinned to this run's
// worker, so a summation that ignored the flavor would size the node against capacity that lives elsewhere.
//
// The provenance is asserted on the CONTRIBUTING queue count, not on len(fs.ClusterQueue). The synthetic set
// has three queues and two of them were summed; a provenance saying "3 ClusterQueue(s)" would describe a sum
// that was not taken over them, in the one field whose entire job is to be trustworthy.
//
// Mutation that turns this red: replace the summation with a constant 2 while keeping an honest provenance
// string (the synthetic case catches it); or build the provenance with len(fs.ClusterQueue) instead of the
// contributing count (the "2 ClusterQueue(s)" assertion catches that one).
func TestRequiredGPUIsDerivedFromTheFixturesNotHardCoded(t *testing.T) {
	reclaimTrace, err := queuelab.TerminationContractTrace(victimServiceSec, doseSec, queuelab.DoseSelfCompleting)
	if err != nil {
		t.Fatalf("build the reclaim trace: %v", err)
	}
	reclaim := testFixtures(t, queuelab.StudyReclaim, "Any")
	req, err := requiredGPU(reclaim, reclaimTrace)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if req.Total != 2 {
		t.Fatalf("reclaim needs %d GPUs, want 2: two ClusterQueues at nominal 1 in one cohort is exactly the "+
			"borrow-then-reclaim contrast the arm is named after, and a node that cannot back both never "+
			"produces it", req.Total)
	}
	if req.BoundBy != boundByQuotaSum {
		t.Fatalf("reclaim is bound by %q, want %q: its rows are 1 GPU each, so the sum is the only bound that "+
			"can be doing any work", req.BoundBy, boundByQuotaSum)
	}
	if !strings.Contains(req.From, "ClusterQueue") || !strings.Contains(req.From, reclaim.Flavor.Name) {
		t.Fatalf("the provenance %q names neither the queues it summed nor the flavor it summed them on, so a "+
			"reader cannot tell a derived number from a typed one", req.From)
	}

	fifo, err := requiredGPU(testFixtures(t, queuelab.StudyFIFO, "StrictFIFO"),
		queuelab.FIFOHeadOfLineScenario(120, 30))
	if err != nil || fifo.Total != 2 {
		t.Fatalf("fifo needs %d GPUs (err %v), want 2: one ClusterQueue at nominal 2, and a 2-GPU head row "+
			"that ties with it", fifo.Total, err)
	}

	// Two quotas on this run's flavor and one on somebody else's, so both halves of the rule are exercised at
	// once: the total is summed, and the foreign flavor contributes nothing to either the sum or the count.
	synthetic := &queuelab.FixtureSet{
		Flavor: &kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{Name: "queuelab-gpu-r7"}},
		ClusterQueue: []*kueuev1beta2.ClusterQueue{
			syntheticCQ("cq-a", "queuelab-gpu-r7", 1),
			syntheticCQ("cq-b", "queuelab-gpu-r7", 3),
			syntheticCQ("cq-elsewhere", "some-other-flavor", 8),
		},
	}
	req, err = requiredGPU(synthetic, reclaimTrace)
	if err != nil {
		t.Fatalf("synthetic: %v", err)
	}
	if req.Total != 4 {
		t.Fatalf("requiredGPU = %d, want 4 (1+3 on this run's flavor, and none of the 8 on another): the "+
			"number is not being summed out of the fixtures", req.Total)
	}
	if !strings.Contains(req.From, "over 2 ClusterQueue(s)") {
		t.Fatalf("the provenance %q counts queues that contributed nothing to the sum it describes", req.From)
	}
}

// The bound my brief denied existed. It said every trace row requests one GPU; FIFOHeadOfLineScenario emits a
// 2-GPU head row and ValidateTrace REQUIRES one, because a head job that cannot immediately fit is the entire
// mechanism that study compares.
//
// The two bounds are independent and neither implies the other. A Pod is scheduled whole onto one node, so a
// row larger than the node advertises can never be scheduled at all however much aggregate quota the queues
// hold — the run would proceed, the head job would sit unschedulable forever, and the arm would report the
// head-of-line comparison it structurally never made. Today the FIFO sum (2) and its largest row (2) happen
// to coincide, which is exactly the coincidence that lets a sum-only derivation look correct indefinitely.
//
// Mutation that turns this red: derive the requirement from the nominal quota sum alone (drop the
// `largest > sum` branch). Both real studies stay green — that is the point of this test existing.
func TestRequiredGPUTakesTheLargestSingleRowWhenItExceedsTheQuotaSum(t *testing.T) {
	fs := &queuelab.FixtureSet{
		Flavor:       &kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{Name: "queuelab-gpu-r7"}},
		ClusterQueue: []*kueuev1beta2.ClusterQueue{syntheticCQ("cq-a", "queuelab-gpu-r7", 2)},
	}
	trace := []queuelab.TrainingTraceRow{
		{Index: 0, Name: "small", Tenant: "tenant-a", GPUCount: 1, DurationSec: 30},
		{Index: 1, Name: "head3", Tenant: "tenant-a", GPUCount: 3, DurationSec: 30},
	}

	req, err := requiredGPU(fs, trace)
	if err != nil {
		t.Fatalf("requiredGPU: %v", err)
	}
	if req.Total != 3 {
		t.Fatalf("requiredGPU = %d, want 3: a 3-GPU Pod is scheduled whole onto one node, so a 2-GPU node "+
			"can never run it however much aggregate quota the queues hold", req.Total)
	}
	if req.BoundBy != boundByLargestRow {
		t.Fatalf("BoundBy = %q, want %q: which bound decided is the difference between \"the node cannot hold "+
			"the whole arm\" and \"one Pod can never be scheduled at all\"", req.BoundBy, boundByLargestRow)
	}
	if !strings.Contains(req.From, `"head3"`) {
		t.Fatalf("the provenance %q does not name the row that bound the requirement", req.From)
	}
	// Both numbers are carried whichever one won, so the margin between them is legible in the record rather
	// than having to be reconstructed from the fixtures afterwards.
	if !strings.Contains(req.From, "= 2;") {
		t.Fatalf("the provenance %q drops the bound that lost, leaving no way to see how close the two were",
			req.From)
	}

	// A node that satisfies the sum and not the row must still refuse, which is the whole reason the bound is
	// computed at all: the aggregate looks fine and exactly one Pod is unschedulable forever.
	twoGPU := node(nil, nil)
	if _, err := qualify(twoGPU, nil, req, mustHarnessContract(t)); err == nil {
		t.Fatal("a 2-GPU node was accepted for a trace containing a 3-GPU row: the run proceeds, the head job " +
			"never schedules, and the study reports a comparison it never made")
	}
}

func syntheticCQ(name, flavor string, nominal int64) *kueuev1beta2.ClusterQueue {
	return &kueuev1beta2.ClusterQueue{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kueuev1beta2.ClusterQueueSpec{
			ResourceGroups: []kueuev1beta2.ResourceGroup{{
				CoveredResources: []corev1.ResourceName{gpuResourceName},
				Flavors: []kueuev1beta2.FlavorQuotas{{
					Name: kueuev1beta2.ResourceFlavorReference(flavor),
					Resources: []kueuev1beta2.ResourceQuota{{
						Name:         gpuResourceName,
						NominalQuota: *resource.NewQuantity(nominal, resource.DecimalSI),
					}},
				}},
			}},
		},
	}
}

// A protocol asking for no device at all would make the capacity check pass on any machine, including one
// with no GPU — a bar that reads as a check and is not one. It is a defect in this program rather than in the
// node, and the message has to say so or an operator goes looking at the cluster. Both bounds are zero here,
// because either one alone being positive is a legitimate requirement.
//
// Mutation that turns this red: drop the `req.Total < 1` branch from requiredGPU.
func TestRequiredGPURefusesAProtocolThatAsksForNoDevice(t *testing.T) {
	empty := &queuelab.FixtureSet{
		Flavor:       &kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{Name: "queuelab-gpu-r7"}},
		ClusterQueue: []*kueuev1beta2.ClusterQueue{syntheticCQ("cq-a", "queuelab-gpu-r7", 0)},
	}
	_, err := requiredGPU(empty, []queuelab.TrainingTraceRow{{Index: 0, Name: "cpu-only", GPUCount: 0}})
	if err == nil {
		t.Fatal("a requirement of zero was accepted: every node passes a bar of zero, so the capacity check " +
			"would still be present and would no longer be checking anything")
	}
	if !strings.Contains(err.Error(), "defect in the protocol") {
		t.Fatalf("the refusal %q blames the machine for a defect that is in this program", err)
	}
}

// The whole point of the check: the ownership taint reserves the node against FUTURE placement and evicts
// nothing, so a Pod that was already holding a device keeps holding it for the entire run.
//
// Mutation that turns this red: drop the `len(consumers) > 0` branch from qualify. The node is Ready,
// schedulable and advertises two devices, so nothing else in qualify has anything to object to and the run
// proceeds onto a machine with one of its two GPUs already spoken for.
func TestQualifyRefusesAWorkerThatAlreadyHoldsAForeignGPUPod(t *testing.T) {
	q, err := qualify(node(nil, nil), []corev1.Pod{*gpuPod("tenant-a", "train-7", "platform-worker", 1)},
		testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker running somebody else's GPU Pod qualified: the run would admit against capacity it " +
			"does not have and report a number measured on a different machine")
	}
	if !strings.Contains(err.Error(), "train-7") {
		t.Fatalf("the refusal %q does not name what is on the node, so the operator is told to look and not "+
			"told where", err)
	}
	if len(q.GPUConsumers) != 1 || q.GPUConsumers[0].Name != "train-7" || q.GPUConsumers[0].GPUs != 1 {
		t.Fatalf("the observation must carry what was found, got %+v", q.GPUConsumers)
	}
}

// The trap, pinned as a test rather than as a comment.
//
// The simulator is where nvidia.com/gpu comes from on these nodes, it runs on every node, and it requests no
// device — so the predicate has to be "what does this Pod REQUEST", never "is this Pod something to do with
// GPUs". Rejecting the provider of the resource would make every run on every one of this lab's clusters
// refuse, and it would refuse for a reason that reads entirely plausible in the log.
//
// Mutation that turns this red: change gpuConsumersOn's test from `podGPURequest(&p.Spec) == 0` to something
// keyed on the node advertising the resource, on the priority class, or on the Pod's own name — every
// variant that "looks GPU-ish" rather than asking what was requested.
func TestQualifyDoesNotRejectTheDevicePluginThatProvidesTheResource(t *testing.T) {
	q, err := qualify(node(nil, nil), []corev1.Pod{*simulatorPod("platform-worker")}, testReq(2), mustHarnessContract(t))
	if err != nil {
		t.Fatalf("the gpu-simulator DaemonSet was treated as a GPU consumer, so every run against every "+
			"cluster this lab has would refuse: %v", err)
	}
	if len(q.GPUConsumers) != 0 {
		t.Fatalf("the simulator was counted as a consumer: %+v", q.GPUConsumers)
	}
	if q.PodsOnNode != 1 {
		t.Fatalf("PodsOnNode = %d, want 1: a clean verdict with no denominator cannot be told apart from a "+
			"List that looked in the wrong place", q.PodsOnNode)
	}
}

// A Pod with a deletionTimestamp is still Running and still holds its device until the kubelet is finished
// with it — and it is invisible to a taint, which is why it is the state a previous run's stuck teardown
// leaves behind and the state this check most needs to see.
//
// Mutation that turns this red: skip Pods with a deletionTimestamp in gpuConsumersOn (the natural "it is
// going away anyway" reading), or test holdsDevices on the deletion timestamp instead of on the phase.
func TestQualifyCountsATerminatingGPUPodBecauseItStillHoldsTheDevice(t *testing.T) {
	dying := gpuPod("tenant-a", "train-8", "platform-worker", 2)
	ts := metav1.NewTime(time.Now())
	dying.DeletionTimestamp = &ts
	dying.Finalizers = []string{"kubernetes"}

	q, err := qualify(node(nil, nil), []corev1.Pod{*dying}, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a terminating GPU Pod was treated as gone: it holds both devices until the kubelet finishes, " +
			"and the run would have measured against nothing at all")
	}
	if len(q.GPUConsumers) != 1 || !q.GPUConsumers[0].Terminating {
		t.Fatalf("the observation must record that it was terminating, since that is what tells the operator " +
			"to wait rather than to go looking for whose job it is")
	}
	if !strings.Contains(err.Error(), "terminating") {
		t.Fatalf("the refusal %q does not distinguish a Pod that will clear itself from one that will not", err)
	}
}

// Pending counts, and the reason is not symmetry with Running: a Pod already assigned to this node has had
// the device reserved for it by the scheduler and will take it the moment its image finishes pulling. This is
// the ordinary state of a GPU Pod in the first seconds of its life, so a run started just after somebody
// else's job was admitted is exactly when it would be missed — and it would be missed silently, because by
// the time the run submitted anything the Pod would be Running and holding the device.
//
// This branch was the one predicate my first round left untested: mutating holdsDevices to exclude Pending
// kept the whole targeted suite green.
//
// Mutation that turns this red: `return p.Status.Phase != corev1.PodPending && p.Status.Phase !=
// corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed` in holdsDevices.
func TestQualifyCountsAPendingGPUPodBecauseTheDeviceIsAlreadyReservedForIt(t *testing.T) {
	starting := gpuPod("tenant-a", "train-12", "platform-worker", 2)
	starting.Status.Phase = corev1.PodPending

	q, err := qualify(node(nil, nil), []corev1.Pod{*starting}, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a Pending GPU Pod already assigned to this node was treated as consuming nothing: the " +
			"scheduler has reserved both devices for it, and it will hold them before this run submits a thing")
	}
	if len(q.GPUConsumers) != 1 || q.GPUConsumers[0].Phase != string(corev1.PodPending) {
		t.Fatalf("the observation must record the phase it found, so a reader can tell a Pod that is starting "+
			"from one that has been running for an hour, got %+v", q.GPUConsumers)
	}
}

// Two exclusions, both of which would otherwise refuse runs on perfectly good clusters: a GPU Pod on the
// OTHER worker is not on this run's machine at all, and a Succeeded one has already had its device released
// by the kubelet and is only waiting to be garbage collected. This lab's clusters accumulate both.
//
// Mutation that turns this red: drop the `p.Spec.NodeName != node` guard, or drop the holdsDevices guard.
// Either one alone makes this test fail, and either one alone would make a run refuse over a Pod on another
// machine or a Pod that finished days ago.
func TestQualifyIgnoresPodsOnOtherNodesAndPodsThatHaveFinished(t *testing.T) {
	elsewhere := gpuPod("tenant-a", "train-9", "platform-worker2", 2)
	finished := gpuPod("tenant-b", "train-old", "platform-worker", 2)
	finished.Status.Phase = corev1.PodSucceeded
	failed := gpuPod("tenant-b", "train-crashed", "platform-worker", 2)
	failed.Status.Phase = corev1.PodFailed

	q, err := qualify(node(nil, nil), []corev1.Pod{*elsewhere, *finished, *failed}, testReq(2), mustHarnessContract(t))
	if err != nil {
		t.Fatalf("a worker whose only GPU Pods are on another node or already finished was refused: %v", err)
	}
	if len(q.GPUConsumers) != 0 {
		t.Fatalf("nothing on this node holds a device, got %+v", q.GPUConsumers)
	}
	if q.PodsOnNode != 2 {
		t.Fatalf("PodsOnNode = %d, want 2: the Pod on the other worker must not be counted in this node's "+
			"denominator either", q.PodsOnNode)
	}
}

// The run installs its own NoSchedule taint moments before this check, so a qualification that objected to
// taints would refuse every run on the marker the run itself put there — and it would do it AFTER the worker
// was acquired, so every invocation would also have to be recovered by hand.
//
// Mutation that turns this red: add a "the node carries no NoSchedule taint" condition to qualify.
func TestQualifyAcceptsTheRunsOwnOwnershipTaint(t *testing.T) {
	ours := node(map[string]string{workerLabelKey: "r7"}, nil, ourTaint())
	if _, err := qualify(ours, nil, testReq(2), mustHarnessContract(t)); err != nil {
		t.Fatalf("the worker was refused for the marker this run installed on it a moment earlier: %v", err)
	}
}

// Capacity is derived, and a node that cannot back the whole arm is a node the arm completes on while
// measuring something else: with one device the two reclaim queues never both hold a workload, so the
// borrow-then-reclaim contrast never happens and the run reports its absence as a result.
//
// Mutation that turns this red: drop the `q.AllocatableGPU < required` branch from qualify.
func TestQualifyRefusesANodeTooSmallForTheArm(t *testing.T) {
	small := node(nil, nil)
	small.Status.Allocatable[gpuResourceName] = *resource.NewQuantity(1, resource.DecimalSI)

	q, err := qualify(small, nil, gpuRequirement{Total: 2, BoundBy: boundByQuotaSum,
		From: "nominal quota over 2 ClusterQueue(s)"}, mustHarnessContract(t))
	if err == nil {
		t.Fatal("a one-device node was accepted for a two-device arm: the run completes and reports the " +
			"contrast it structurally could not produce")
	}
	if !strings.Contains(err.Error(), "ClusterQueue") {
		t.Fatalf("the refusal %q does not say where the requirement came from, which is what tells the "+
			"operator whether to grow the node or fix the fixtures", err)
	}
	if q.AllocatableGPU != 1 || q.RequiredGPU != 2 {
		t.Fatalf("the observation must carry both numbers, got %+v", q)
	}
}

// Ready and schedulable are two separate facts and both are reported, because an operator who cordoned a node
// and also let it go NotReady should not have to run the tool twice to find that out. The missing-condition
// case is in here deliberately: a Node object with no Ready condition at all must fail toward the refusal,
// the same way an unclassifiable teardown target holds the worker.
//
// Mutation that turns this red: make nodeReady return true when no Ready condition is present, or drop the
// Schedulable branch from qualify.
func TestQualifyRefusesANodeThatIsNotReadyOrIsCordoned(t *testing.T) {
	notReady := node(nil, nil)
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	if _, err := qualify(notReady, nil, testReq(2), mustHarnessContract(t)); err == nil {
		t.Fatal("a NotReady worker was accepted")
	}

	silent := node(nil, nil)
	silent.Status.Conditions = nil
	if _, err := qualify(silent, nil, testReq(2), mustHarnessContract(t)); err == nil {
		t.Fatal("a Node reporting no Ready condition at all was accepted: an unclassifiable machine must fail " +
			"toward the refusal, not toward the measurement")
	}

	cordoned := node(nil, nil)
	cordoned.Spec.Unschedulable = true
	q, err := qualify(cordoned, nil, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a cordoned worker was accepted: nothing this run submits can land on it, so the whole run " +
			"would time out at its first barrier with no explanation")
	}
	if q.Schedulable {
		t.Fatal("the observation must record the cordon it refused on")
	}
	if !strings.Contains(err.Error(), "cordoned") {
		t.Fatalf("the refusal %q does not name the cordon", err)
	}
}

// One node, three things wrong with it. Reporting only the first would make an operator pay a round trip
// against a real cluster per defect, which on a lab whose runs are minutes long is the difference between one
// correction and three.
//
// Mutation that turns this red: return on the first failed condition in qualify instead of accumulating them.
func TestQualifyReportsEveryConditionItFailedNotJustTheFirst(t *testing.T) {
	bad := node(nil, nil)
	bad.Status.Allocatable[gpuResourceName] = *resource.NewQuantity(1, resource.DecimalSI)
	bad.Spec.Unschedulable = true

	_, err := qualify(bad, []corev1.Pod{*gpuPod("tenant-a", "train-7", "platform-worker", 1)}, testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a node that failed three conditions qualified")
	}
	for _, want := range []string{"cordoned", "allocatable", "train-7"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not mention %q, so the operator fixes one thing and meets the next on "+
				"the following run:\n%s", want, err)
		}
	}
}

// A Pod may name only the limit and have the request defaulted to match it, which is how a great many GPU
// Pods are actually written. Reading Requests alone scores exactly those Pods as consuming nothing, and they
// are the ones most likely to be sitting on a shared lab worker.
//
// Mutation that turns this red: delete the Limits fallback from gpuOf.
func TestPodGPURequestReadsALimitOnlyPod(t *testing.T) {
	limitOnly := gpuPod("tenant-a", "train-10", "platform-worker", 1)
	limitOnly.Spec.Containers[0].Resources.Requests = nil
	if got := podGPURequest(&limitOnly.Spec); got != 1 {
		t.Fatalf("podGPURequest = %d, want 1: a Pod that names only its limit has its request defaulted to "+
			"match, and reading Requests alone makes it invisible", got)
	}

	// An init container's device is held while it runs, and it is the only thing running at that moment, so
	// the effective request is the larger of the two rather than the regular containers' sum alone.
	initOnly := gpuPod("tenant-a", "train-11", "platform-worker", 0)
	initOnly.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	initOnly.Spec.InitContainers = []corev1.Container{{
		Name: "fetch",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{gpuResourceName: *resource.NewQuantity(2, resource.DecimalSI)},
		},
	}}
	if got := podGPURequest(&initOnly.Spec); got != 2 {
		t.Fatalf("podGPURequest = %d, want 2: an init container holds its device while it runs", got)
	}
}

// A run that could not read the Pods on its worker has not established the premise it is about to measure
// under. Treating an unreadable cluster as a clean one is the substitution — absence of evidence for evidence
// of absence — that this whole gate exists to stop, and it is the shape an RBAC gap takes: the runner is
// newly required to list Pods cluster-wide, and a cluster where it may not is the cluster where this would
// silently pass.
//
// Mutation that turns this red: swallow the List error in qualifyWorker and carry on with no Pods.
func TestQualifyWorkerRefusesWhenItCannotSeeThePods(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "",
						errors.New("no list permission on pods at cluster scope"))
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	q, err := qualifyWorker(context.Background(), fc, "platform-worker", testReq(2), mustHarnessContract(t))
	if err == nil {
		t.Fatal("a worker whose Pods could not be listed was qualified, which is exactly how an RBAC gap on " +
			"Pods would present itself: as a clean cluster")
	}
	if q != nil {
		t.Fatal("no qualification may be returned from a read that failed: a document full of zero values " +
			"claims a node advertising nothing and carrying no Pods was inspected and found fine")
	}
}

// The observation has to be returned alongside the refusal, not instead of it. A refused run is precisely the
// run whose evidence would otherwise be a sentence on somebody's terminal, and the later
// validity-bearing-artifact gate can only be serialization of what each gate wrote if each gate wrote.
//
// Mutation that turns this red: return `nil, err` from qualifyWorker when qualify refuses.
func TestQualifyWorkerReturnsWhatItSawEvenWhenItRefuses(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node(nil, nil), gpuPod("tenant-a", "train-7", "platform-worker", 2),
			simulatorPod("platform-worker")).
		Build()

	q, err := qualifyWorker(context.Background(), fc, "platform-worker",
		gpuRequirement{Total: 2, BoundBy: boundByQuotaSum, From: "nominal quota"}, mustHarnessContract(t))
	if err == nil {
		t.Fatal("the contaminated worker qualified")
	}
	if q == nil {
		t.Fatal("a refusal with no observation leaves the record saying only that something was wrong")
	}
	if q.Node != "platform-worker" || q.NodeUID != "uid-node" {
		t.Fatalf("the observation must identify the machine it is about, got %+v", q)
	}
	if q.AllocatableGPU != 2 || q.RequiredGPU != 2 || q.RequiredFrom != "nominal quota" ||
		q.RequiredBoundBy != boundByQuotaSum {
		t.Fatalf("the observation must carry the capacity claim and where the requirement came from, got %+v", q)
	}
	if !q.Ready || !q.Schedulable {
		t.Fatalf("readiness must be recorded even when it was not the reason for the refusal, got %+v", q)
	}
	if q.PodsOnNode != 2 || len(q.GPUConsumers) != 1 {
		t.Fatalf("PodsOnNode = %d and %d consumer(s): the verdict and its denominator are one claim",
			q.PodsOnNode, len(q.GPUConsumers))
	}
}

// mustHarnessContract is harnessTerminationContract for tests, which have no use for an error that the
// package's own constants cannot produce.
//
// It exists so the specs read as they did before the renderer grew an error return, while the production
// callers still handle it — the point of that change was the operator command, not these.
func mustHarnessContract(t *testing.T) canaryContract {
	t.Helper()
	c, err := harnessTerminationContract()
	if err != nil {
		t.Fatalf("harness termination contract: %v", err)
	}
	return c
}

// A node with MORE devices than the protocol needs is refused, and that direction is the dangerous one.
//
// Too few devices is obviously fatal and was already refused. Too many destroys the experiment silently:
// the contrast this lab publishes is physical card scarcity, and every recorded run shows it directly --
// Kueue admits the owner within 0.1 s of the preemption decision, and the owner's Pod becomes Ready one to
// two seconds after the VICTIM'S terminal phase, in both arms. It is waiting for a card. The 29-second arm
// difference is that wait.
//
// Give the run two spare devices and the owner's Pod binds at admission in both arms. The wait collapses to
// a container start on each side, the difference falls below the floor, and every other figure in the
// record looks exactly as it does today. Nothing downstream could tell that from a real finding -- and the
// preregistration's third refutation condition would read it as the session's most useful discovery.
//
// The instance this study rents carries four cards because no rentable one carries two, so this is not
// hypothetical: it is the default in infra/aws/cluster/variables.tf.
//
// Mutation that turns this red: restore `AllocatableGPU < req.Total` as the only device check.
func TestANodeWithSpareDevicesIsRefused(t *testing.T) {
	node := func(allocatable int64) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "platform-worker", UID: "n1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					gpuResourceName: *resource.NewQuantity(allocatable, resource.DecimalSI),
				},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		}
	}
	// A Pod holding devices to keep the node SCARCE, which is what makes a four-card instance measurable.
	occupier := func(gpus int64) []corev1.Pod {
		return []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "gpu-platform-control-plane-system", Name: "surplus-occupier",
				// A valid label value: alphanumeric with dashes, dots and underscores. The first version of
				// this fixture used a sentence, which a fake client accepts and a real API server rejects --
				// so the unit test passed while the manifest the session applies was invalid.
				Labels: map[string]string{surplusOccupierLabel: "session"},
			},
			Spec: corev1.PodSpec{
				NodeName: "platform-worker",
				Containers: []corev1.Container{{
					Name: "hold",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						gpuResourceName: *resource.NewQuantity(gpus, resource.DecimalSI),
					}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}}
	}
	// The same Pod without the label is a foreign tenant, and must stay refused.
	foreign := func(gpus int64) []corev1.Pod {
		p := occupier(gpus)
		p[0].Labels = nil
		p[0].Name = "somebody-elses-training-job"
		return p
	}
	for _, tc := range []struct {
		name        string
		allocatable int64
		pods        []corev1.Pod
		wantFail    string
	}{
		{"exactly what the protocol needs", 2, nil, ""},
		{"a smaller machine", 1, nil, "the contrast it never produced"},
		// The g4dn.12xlarge case: four T4s, two of them spare.
		{"the instance this study actually rents", 4, nil, "collapse below the floor"},
		{"one spare device", 3, nil, "collapse below the floor"},
		// The same instance with the surplus held: measurable, because what the run can schedule is two.
		{"four cards with the surplus held", 4, occupier(2), ""},
		{"four cards with too little held", 4, occupier(1), "collapse below the floor"},
		{"four cards with too much held", 4, occupier(3), "the contrast it never produced"},
		// An unlabelled holder is somebody else's work and still refuses the run, occupier or not.
		{"a foreign tenant holding the surplus", 4, foreign(2), "already hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := qualify(node(tc.allocatable), tc.pods, testReq(2), mustHarnessContract(t))
			if tc.wantFail == "" {
				if err != nil && strings.Contains(err.Error(), "allocatable") {
					t.Fatalf("a node advertising exactly the requirement was refused on device count: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a node advertising %d devices against a requirement of 2 qualified",
					tc.allocatable)
			}
			if !strings.Contains(err.Error(), tc.wantFail) {
				t.Fatalf("a node advertising %d devices was not refused for the right reason (want %q): %v",
					tc.allocatable, tc.wantFail, err)
			}
		})
	}
}
