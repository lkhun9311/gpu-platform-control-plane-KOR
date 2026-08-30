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
	"errors"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testJournal() journal {
	return journal{
		Schema:    journalSchema,
		TxID:      "tx-1111",
		RunID:     "r7",
		Arm:       "A-honor",
		Kind:      ownerRun,
		Study:     "reclaim",
		Variant:   "reclaim-on",
		Namespace: "queuelab-r7",
		Node:      "platform-worker",
		NodeUID:   "uid-node",
		TakenAt:   "2026-08-06T10:00:00Z",
		Installed: installedTuple{
			LabelValue:  "r7",
			TaintValue:  "r7",
			TaintEffect: corev1.TaintEffectNoSchedule,
		},
	}
}

func TestJournalRoundTrips(t *testing.T) {
	s, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeJournal(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != testJournal() {
		t.Fatalf("round trip changed the journal:\n got %+v\nwant %+v", got, testJournal())
	}
}

// A state machine that trusts this annotation must not accept a document it does not fully understand,
// so an unknown field and an unknown schema are both rejected rather than ignored.
func TestDecodeJournalRejectsUnknownFieldAndSchema(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field": `{"schema":1,"txID":"tx-1111","runID":"r7","arm":"A-honor","node":"platform-worker",` +
			`"nodeUID":"uid-node","takenAt":"t","installed":{"labelValue":"r7","taintValue":"r7",` +
			`"taintEffect":"NoSchedule"},"extra":true}`,
		"unknown schema": `{"schema":99,"txID":"tx-1111","runID":"r7","arm":"A-honor","node":"platform-worker",` +
			`"nodeUID":"uid-node","takenAt":"t","installed":{"labelValue":"r7","taintValue":"r7",` +
			`"taintEffect":"NoSchedule"}}`,
		"trailing data": `{"schema":1,"txID":"tx-1111","runID":"r7","arm":"A-honor","node":"platform-worker",` +
			`"nodeUID":"uid-node","takenAt":"t","installed":{"labelValue":"r7","taintValue":"r7",` +
			`"taintEffect":"NoSchedule"}} {"schema":1}`,
		"missing txid": `{"schema":1,"txID":"","runID":"r7","arm":"A-honor","node":"platform-worker",` +
			`"nodeUID":"uid-node","takenAt":"t","installed":{"labelValue":"r7","taintValue":"r7",` +
			`"taintEffect":"NoSchedule"}}`,
	} {
		if _, err := decodeJournal(raw); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

// node is the worker every test in this package acquires, and it is Ready and advertising two devices
// because that is what the cluster these runs are aimed at looks like: two kind workers, a gpu-simulator
// DaemonSet registered on each with FAKE_GPU_COUNT=2 (hack/m6-kind-e2e.sh), and Kueue on top.
//
// Before the environment qualification existed this status was absent and nothing noticed, which is exactly
// the point: a Node object with no allocatable devices and no Ready condition was an adequate stand-in for a
// GPU worker only as long as nothing in the run ever asked what the machine was. Leaving it that way now
// would make every run() test refuse at qualification and prove nothing about the paths they were written
// for, so the double is corrected to match the machine rather than the check weakened to accept the double.
//
// The termination canary is the same argument once more, and it is the reason this node now carries a kubelet
// version, a container runtime and a passing qualification annotation. A worker a canary has never qualified
// is a worker no run may measure on, so a double without one would send every run() test to the same refusal
// and none of them would reach the path it was written for. A test that wants an UNQUALIFIED worker asks for
// one explicitly — `node(nil, map[string]string{canaryAnnotationKey: ""})` — rather than getting it by
// omission, so the tests about this gate say so in their own bodies.
func node(labels map[string]string, ann map[string]string, taints ...corev1.Taint) *corev1.Node {
	annotations := map[string]string{canaryAnnotationKey: qualifiedCanaryAnnotation()}
	maps.Copy(annotations, ann)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "platform-worker",
			UID:         types.UID("uid-node"),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{gpuResourceName: *resource.NewQuantity(2, resource.DecimalSI)},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:          testKubeletVersion,
				ContainerRuntimeVersion: testContainerRuntime,
			},
		},
	}
}

func ourTaint() corev1.Taint {
	return corev1.Taint{Key: workerTaintKey, Value: "r7", Effect: corev1.TaintEffectNoSchedule}
}

// The free state is the ONLY state acquisition may proceed from, so each row here is a named refusal and
// the matrix is the specification: anything not on this list must still refuse by falling through.
func TestDecideAcquireRefusesEveryNonFreeState(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	foreign := testJournal()
	foreign.TxID = "tx-other"
	foreign.RunID = "r9"
	foreignRaw, err := encodeJournal(foreign)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	cases := map[string]struct {
		n    *corev1.Node
		want string
	}{
		"foreign owner": {
			node(map[string]string{workerLabelKey: "r9"}, map[string]string{journalKey: foreignRaw},
				corev1.Taint{Key: workerTaintKey, Value: "r9", Effect: corev1.TaintEffectNoSchedule}),
			reasonForeignOwner,
		},
		"our own txid again": {
			node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint()),
			reasonOwnTxID,
		},
		"marker without journal": {
			node(map[string]string{workerLabelKey: "r7"}, nil, ourTaint()),
			reasonMarkerWithoutJournal,
		},
		"journal without marker": {
			node(nil, map[string]string{journalKey: good}),
			reasonJournalWithoutMarker,
		},
		"label without taint": {
			node(map[string]string{workerLabelKey: "r7"}, nil),
			reasonMarkerWithoutJournal,
		},
		"taint without label": {
			node(nil, nil, ourTaint()),
			reasonMarkerWithoutJournal,
		},
		"duplicate taint key with different effects": {
			node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint(),
				corev1.Taint{Key: workerTaintKey, Value: "r7", Effect: corev1.TaintEffectNoExecute}),
			reasonDuplicateTaintKey,
		},
		"unparseable journal": {
			node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: "{"}, ourTaint()),
			reasonBadJournal,
		},
		"quarantined": {
			node(nil, map[string]string{quarantineKey: `{"schema":1,"quarantineID":"q1","forcedAt":"t",` +
				`"node":"platform-worker","nodeUID":"uid-node","priorJournal":"","observedLabel":""}`}),
			reasonQuarantined,
		},
	}

	for name, tc := range cases {
		obs := observe(tc.n)
		if _, err := decideAcquire(obs, acquisitionSeed("tx-1111", "r7", "A-honor"), "t"); err == nil {
			t.Fatalf("%s: acquisition must refuse", name)
		} else {
			var r *refusal
			if !asRefusal(err, &r) {
				t.Fatalf("%s: want a named refusal, got %T: %v", name, err, err)
			}
			if r.Reason != tc.want {
				t.Fatalf("%s: refusal %q, want %q", name, r.Reason, tc.want)
			}
		}
	}
}

func TestDecideAcquireProceedsOnlyFromTheFreeState(t *testing.T) {
	// An unrelated taint is not a reason to refuse: this repository's own nodehealth controller taints the
	// worker with its distinct unhealthy key and that must not block the lab.
	n := node(map[string]string{"kubernetes.io/hostname": "platform-worker"}, map[string]string{"other": "x"},
		corev1.Taint{Key: "gpu-platform/unhealthy", Value: "true", Effect: corev1.TaintEffectNoSchedule})
	j, err := decideAcquire(observe(n), acquisitionSeed("tx-1111", "r7", "A-honor"), "2026-08-06T10:00:00Z")
	if err != nil {
		t.Fatalf("free node must be acquirable: %v", err)
	}
	if j.TxID != "tx-1111" || j.RunID != "r7" || j.Arm != "A-honor" || j.NodeUID != "uid-node" {
		t.Fatalf("journal does not identify the transaction: %+v", j)
	}
	if j.Installed.LabelValue != "r7" || j.Installed.TaintValue != "r7" ||
		j.Installed.TaintEffect != corev1.TaintEffectNoSchedule {
		t.Fatalf("journal must record exactly what will be installed: %+v", j.Installed)
	}
}

func TestDecideRelease(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	foreign := testJournal()
	foreign.TxID = "tx-other"
	foreignRaw, err := encodeJournal(foreign)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	t.Run("restores when the installed tuple is intact", func(t *testing.T) {
		n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint())
		act, err := decideRelease(observe(n), "tx-1111")
		if err != nil || act != releaseRestore {
			t.Fatalf("got %v, %v; want releaseRestore, nil", act, err)
		}
	})

	// An unrelated taint added while the run was in flight is benign drift, not divergence: only the values
	// this transaction installed are compared.
	t.Run("tolerates an unrelated taint added mid-run", func(t *testing.T) {
		n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint(),
			corev1.Taint{Key: "gpu-platform/unhealthy", Value: "true", Effect: corev1.TaintEffectNoSchedule})
		act, err := decideRelease(observe(n), "tx-1111")
		if err != nil || act != releaseRestore {
			t.Fatalf("got %v, %v; want releaseRestore, nil", act, err)
		}
	})

	t.Run("already released is success, not failure", func(t *testing.T) {
		n := node(nil, nil)
		act, err := decideRelease(observe(n), "tx-1111")
		if err != nil || act != releaseAlreadyDone {
			t.Fatalf("got %v, %v; want releaseAlreadyDone, nil", act, err)
		}
	})

	// A forced break removed our state and left a quarantine record; that is a restoration FAILURE, not the
	// clean already-released case, or a run whose worker was taken from it could still publish a number.
	t.Run("quarantine is a restoration failure", func(t *testing.T) {
		n := node(nil, map[string]string{quarantineKey: `{"schema":1,"quarantineID":"q1","forcedAt":"t",` +
			`"node":"platform-worker","nodeUID":"uid-node","priorJournal":"","observedLabel":""}`})
		if _, err := decideRelease(observe(n), "tx-1111"); err == nil {
			t.Fatal("a quarantined node must fail release")
		}
	})

	t.Run("a diverged installed value refuses to restore", func(t *testing.T) {
		n := node(map[string]string{workerLabelKey: "someone-else"}, map[string]string{journalKey: good},
			ourTaint())
		_, err := decideRelease(observe(n), "tx-1111")
		var r *refusal
		if err == nil || !asRefusal(err, &r) || r.Reason != reasonInstalledDiverged {
			t.Fatalf("want %s, got %v", reasonInstalledDiverged, err)
		}
	})

	t.Run("a foreign journal is never restored from", func(t *testing.T) {
		n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: foreignRaw}, ourTaint())
		_, err := decideRelease(observe(n), "tx-1111")
		var r *refusal
		if err == nil || !asRefusal(err, &r) || r.Reason != reasonNotOurs {
			t.Fatalf("want %s, got %v", reasonNotOurs, err)
		}
	})
}

// RFC 7386 merge patch replaces spec.taints wholesale, so the new list must be built from the list we just
// observed or an unrelated taint would be deleted as a side effect of dedicating the worker.
func TestTaintListPreservesUnrelatedTaints(t *testing.T) {
	unrelated := corev1.Taint{Key: "gpu-platform/unhealthy", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	got := withOwnershipTaint([]corev1.Taint{unrelated}, "r7")
	if len(got) != 2 {
		t.Fatalf("want the unrelated taint kept plus ours, got %+v", got)
	}
	if got[0] != unrelated {
		t.Fatalf("unrelated taint was not preserved: %+v", got)
	}
	if got[1].Key != workerTaintKey || got[1].Value != "r7" || got[1].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("ownership taint not appended correctly: %+v", got[1])
	}

	back := withoutOwnershipTaint(got)
	if len(back) != 1 || back[0] != unrelated {
		t.Fatalf("release must remove only our taint, got %+v", back)
	}
}

// decodeQuarantine's strictness is what the whole two-step design leans on to trust the record it reads
// back, and the happy-path round trip embedded in the other quarantine tests would still pass if any one of
// these four checks (unknown fields, unknown schema, empty quarantineID, trailing data) were silently
// dropped.
//
// The trailing-data row is the one decodeJournal always had and this decoder did not: two documents in one
// annotation means the second one was written by something other than encodeQuarantine, and decideClear
// matches the operator's -quarantine-id against whichever of them happened to decode first.
func TestDecodeQuarantineRejectsUnknownFieldSchemaEmptyIDAndTrailingData(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field": `{"schema":1,"quarantineID":"q1","forcedAt":"t","node":"platform-worker",` +
			`"nodeUID":"uid-node","priorJournal":"","observedLabel":"","extra":true}`,
		"unknown schema": `{"schema":99,"quarantineID":"q1","forcedAt":"t","node":"platform-worker",` +
			`"nodeUID":"uid-node","priorJournal":"","observedLabel":""}`,
		"empty quarantineID": `{"schema":1,"quarantineID":"","forcedAt":"t","node":"platform-worker",` +
			`"nodeUID":"uid-node","priorJournal":"","observedLabel":""}`,
		"trailing data": `{"schema":1,"quarantineID":"q1","forcedAt":"t","node":"platform-worker",` +
			`"nodeUID":"uid-node","priorJournal":"","observedLabel":""} {"schema":1,"quarantineID":"q2"}`,
		"trailing garbage": `{"schema":1,"quarantineID":"q1","forcedAt":"t","node":"platform-worker",` +
			`"nodeUID":"uid-node","priorJournal":"","observedLabel":""} not json at all`,
	} {
		if _, err := decodeQuarantine(raw); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

// Forcing twice would overwrite the original record with one describing an already-emptied node, which
// would destroy the only surviving evidence of who held the worker.
func TestDecideForceRefusesWhenAlreadyQuarantined(t *testing.T) {
	q := quarantine{Schema: quarantineSchema, QuarantineID: "q1", ForcedAt: "t", Node: "platform-worker",
		NodeUID: "uid-node"}
	raw, err := encodeQuarantine(q)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(nil, map[string]string{quarantineKey: raw})
	if _, err := decideForce(observe(n), "uid-node", "q2", "t2"); err == nil {
		t.Fatal("forcing an already quarantined node must refuse")
	}
}

// The forced break must preserve everything it removed — which node, when, and exactly what was on it — or
// the operator loses the evidence of what they broke. Node, NodeUID and ForcedAt are asserted here too
// (not just PriorJournal/ObservedLabel/ObservedTaints): those three are exactly what tells an operator
// which machine was broken and at what time if the record is ever read on its own, and dropping any one of
// the six fields checked below must fail this test, not just the round-trip test elsewhere in this file.
func TestDecideForceRecordsWhatItRemoves(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// An unrelated taint alongside the ownership one is what makes ObservedTaints meaningful to assert: with
	// only the ownership taint present, a record built from obs.AllTaints (the whole taint list) would look
	// identical to one correctly built from obs.Taints (the ownership-key subset), and this test would not
	// catch the difference.
	unrelated := corev1.Taint{Key: "gpu-platform/unhealthy", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint(), unrelated)
	q, err := decideForce(observe(n), "uid-node", "q1", "2026-08-06T11:00:00Z")
	if err != nil {
		t.Fatalf("force: %v", err)
	}
	if q.QuarantineID == "" {
		t.Fatal("a quarantine must be identified so clearing it can be targeted")
	}
	if q.ForcedAt != "2026-08-06T11:00:00Z" {
		t.Fatalf("forcedAt = %q, want the caller's timestamp", q.ForcedAt)
	}
	if q.Node != "platform-worker" {
		t.Fatalf("node = %q, want platform-worker", q.Node)
	}
	if q.NodeUID != "uid-node" {
		t.Fatalf("nodeUID = %q, want uid-node", q.NodeUID)
	}
	if q.PriorJournal != good {
		t.Fatalf("the prior journal must be preserved verbatim, got %q", q.PriorJournal)
	}
	if q.ObservedLabel != "r7" {
		t.Fatalf("observedLabel = %q, want r7", q.ObservedLabel)
	}
	if len(q.ObservedTaints) != 1 || q.ObservedTaints[0] != ourTaint() {
		t.Fatalf("observedTaints must be exactly the ownership taint, not the unrelated one, got %+v",
			q.ObservedTaints)
	}
}

func TestDecideForceRefusesAWrongNodeUID(t *testing.T) {
	n := node(map[string]string{workerLabelKey: "r7"}, nil, ourTaint())
	if _, err := decideForce(observe(n), "uid-typed-wrong", "q1", "t"); err == nil {
		t.Fatal("a mistyped node UID must refuse: it is the operator's confirmation of the target")
	}
}

func TestDecideClearRequiresTheExactQuarantineID(t *testing.T) {
	q := quarantine{Schema: quarantineSchema, QuarantineID: "q1", ForcedAt: "t", Node: "platform-worker",
		NodeUID: "uid-node"}
	raw, err := encodeQuarantine(q)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(nil, map[string]string{quarantineKey: raw})
	if err := decideClear(observe(n), "q-wrong"); err == nil {
		t.Fatal("clearing must be targeted at the exact quarantine record")
	}
	if err := decideClear(observe(n), "q1"); err != nil {
		t.Fatalf("the matching id must clear: %v", err)
	}
	if err := decideClear(observe(node(nil, nil)), "q1"); err == nil {
		t.Fatal("clearing a node with no quarantine record must refuse rather than silently succeed")
	}
}

// A record this tool cannot read is its own state, not a variant of an unreadable journal: the two send an
// operator to different places, so decideClear names it as such. The decode error stays reachable through
// the chain, because "the reason is classifiable" and "the cause is gone" must not be the same trade.
func TestDecideClearNamesAnUnreadableQuarantineAndKeepsTheCause(t *testing.T) {
	n := node(nil, map[string]string{quarantineKey: "{not valid json"})

	err := decideClear(observe(n), "q1")
	var r *refusal
	if err == nil || !asRefusal(err, &r) || r.Reason != reasonBadQuarantine {
		t.Fatalf("want the %s refusal, got %v", reasonBadQuarantine, err)
	}
	cause := errors.Unwrap(err)
	if cause == nil {
		t.Fatal("the decode failure must stay reachable through the error chain, not only in the sentence")
	}
	if !strings.Contains(cause.Error(), "decode quarantine") {
		t.Fatalf("the unwrapped cause must be the decode failure itself, got %v", cause)
	}
}

// decideForce records the node name AND the node UID; decideClear validated neither, so the id alone decided
// whether a node could be unblocked. Clearing is the step that makes a node acquirable again, and these two
// rows are the ways it could have been done to the wrong object: a record copied onto another Node, and a
// Node deleted and recreated under the same name — a different machine wearing the same label, which is
// exactly the case verifyInstalled already refuses to release across.
func TestDecideClearRequiresTheRecordToNameTheObservedNode(t *testing.T) {
	rec := func(nodeName, nodeUID string) map[string]string {
		t.Helper()
		raw, err := encodeQuarantine(quarantine{Schema: quarantineSchema, QuarantineID: "q1", ForcedAt: "t",
			Node: nodeName, NodeUID: nodeUID})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return map[string]string{quarantineKey: raw}
	}

	cases := map[string]struct {
		ann     map[string]string
		wantErr bool
	}{
		"the record names this node":         {rec("platform-worker", "uid-node"), false},
		"the record was copied from another": {rec("platform-worker9", "uid-other"), true},
		"same name, recreated node":          {rec("platform-worker", "uid-recreated"), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := decideClear(observe(node(nil, tc.ann)), "q1")
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("want the record cleared, got %v", err)
				}
				return
			}
			var r *refusal
			if err == nil || !asRefusal(err, &r) || r.Reason != reasonWrongNode {
				t.Fatalf("want the %s refusal, got %v", reasonWrongNode, err)
			}
		})
	}
}

// withOwnershipTaint replaces the deleted upsertTaint's de-duplication role: a retried acquire attempt must
// not leave two entries under workerTaintKey, or decideAcquire's own duplicate-taint-key check would refuse
// a Node this transaction just wrote.
func TestWithOwnershipTaintReplacesStaleValue(t *testing.T) {
	stale := corev1.Taint{Key: workerTaintKey, Value: "old-run", Effect: corev1.TaintEffectNoSchedule}
	got := withOwnershipTaint([]corev1.Taint{stale}, "new-run")
	if len(got) != 1 {
		t.Fatalf("want exactly one taint under %s, got %+v", workerTaintKey, got)
	}
	if got[0].Value != "new-run" {
		t.Fatalf("stale taint value was not replaced: %+v", got[0])
	}
}

// A residue record is quoted at an operator, so it is decoded as strictly as the journal: a document with
// fields that were silently ignored is not one to print as fact. What differs is the consequence, and that
// is decideAcquire's business, not this decoder's.
func TestDecodeResidueRefusesWhatItDoesNotFullyUnderstand(t *testing.T) {
	good := `{"schema":1,"txID":"tx-1","runID":"r7","leftAt":"2026-08-13T01:07:29Z",` +
		`"left":[{"kind":"Namespace","name":"queuelab-r7","absence":"present"}]}`
	if _, err := decodeResidue(good); err != nil {
		t.Fatalf("a well-formed record was refused: %v", err)
	}
	for name, raw := range map[string]string{
		"unknown field": `{"schema":1,"txID":"tx-1","runID":"r7","leftAt":"t","left":[{"kind":"Namespace","name":"n","absence":"present"}],"extra":1}`,
		"wrong schema":  `{"schema":2,"txID":"tx-1","runID":"r7","leftAt":"t","left":[{"kind":"Namespace","name":"n","absence":"present"}]}`,
		"trailing data": `{"schema":1,"txID":"tx-1","runID":"r7","leftAt":"t","left":[{"kind":"Namespace","name":"n","absence":"present"}]}{}`,
		"nothing left":  `{"schema":1,"txID":"tx-1","runID":"r7","leftAt":"t","left":[]}`,
		"not json":      `{nope`,
	} {
		if _, err := decodeResidue(raw); err == nil {
			t.Errorf("%s was accepted; an operator would be shown a record this decoder could not vouch for", name)
		}
	}
}

// observe is the one place Node state becomes a decision input, so a field it does not read is a field no
// decision can ever see.
func TestObserveSurfacesTheResidueRecord(t *testing.T) {
	raw := `{"schema":1,"txID":"tx-1","runID":"r7","leftAt":"2026-08-13T01:07:29Z",` +
		`"left":[{"kind":"Namespace","name":"queuelab-r7","absence":"present"}]}`
	obs := observe(node(nil, map[string]string{residueKey: raw}))
	if obs.ResidueRaw != raw {
		t.Fatalf("ResidueRaw is %q, want the annotation verbatim", obs.ResidueRaw)
	}
	if obs.ResidueErr != nil {
		t.Fatalf("a well-formed record produced ResidueErr %v", obs.ResidueErr)
	}
	if len(obs.Residue.Left) != 1 || obs.Residue.Left[0].Name != "queuelab-r7" {
		t.Fatalf("decoded record is %+v, want one entry naming queuelab-r7", obs.Residue)
	}

	// A record that cannot be read must still be OBSERVED as present. Reporting neither the raw text nor an
	// error would make an unreadable record indistinguishable from no record at all, and the refusal could
	// then say nothing about it.
	bad := observe(node(nil, map[string]string{residueKey: "{not json"}))
	if bad.ResidueRaw == "" || bad.ResidueErr == nil {
		t.Fatalf("an unreadable record observed as raw=%q err=%v; it must be visible as present-but-unreadable",
			bad.ResidueRaw, bad.ResidueErr)
	}
}

// heldByAnother builds the state every test below starts from: a node another transaction holds, with the
// residue annotation set to whatever the case under test needs.
func heldByAnother(t *testing.T, residueRaw string) *corev1.Node {
	t.Helper()
	j := journal{
		Schema: journalSchema, TxID: "tx-old", RunID: "r7", Arm: "reclaim-on",
		Kind: ownerRun, Study: "reclaim", Variant: "reclaim-on", Namespace: "queuelab-r7",
		Node: "platform-worker", NodeUID: "uid-node", TakenAt: "t0",
		Installed: installedTuple{LabelValue: "r7", TaintValue: "r7", TaintEffect: corev1.TaintEffectNoSchedule},
	}
	raw, err := encodeJournal(j)
	if err != nil {
		t.Fatalf("encode journal: %v", err)
	}
	ann := map[string]string{journalKey: raw}
	if residueRaw != "" {
		ann[residueKey] = residueRaw
	}
	return node(map[string]string{workerLabelKey: "r7"}, ann, ourTaint())
}

func residueRaw(t *testing.T, left ...residueLeft) string {
	t.Helper()
	raw, err := encodeResidue(residueRecord{
		Schema: residueSchema, TxID: "tx-old", RunID: "r7", LeftAt: "2026-08-13T01:07:29Z",
		RecordPath: "queuelabrun-record-20260813T010422Z-31288.json", Left: left,
	})
	if err != nil {
		t.Fatalf("encode residue: %v", err)
	}
	return raw
}

// The foreign-owner refusal is what an operator sees when a previous run left GPU Pods behind, and today it
// is indistinguishable from a run that is legitimately still in flight. Those two need opposite responses.
func TestAcquireRefusalNamesWhatThePreviousTeardownLeft(t *testing.T) {
	n := heldByAnother(t, residueRaw(t,
		residueLeft{Kind: "Namespace", Name: "queuelab-r7", Absence: "present"},
		residueLeft{Kind: "ClusterQueue", Name: "ql-reclaim-tenant-a-r7", Absence: "present"}))

	_, err := decideAcquire(observe(n), acquisitionSeed("tx-new", "r8", "reclaim-on"), "t1")
	if err == nil {
		t.Fatal("acquisition was allowed on a node another transaction holds")
	}
	var r *refusal
	if !asRefusal(err, &r) || r.Reason != reasonForeignOwner {
		t.Fatalf("refusal is %v, want reason %q: residue explains WHY the owner is still there, it is not a "+
			"different state of the node", err, reasonForeignOwner)
	}
	for _, want := range []string{
		"queuelab-r7", "ql-reclaim-tenant-a-r7",
		"queuelabrun-record-20260813T010422Z-31288.json",
		"do NOT strip",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got:\n%s", want, err.Error())
		}
	}
}

// Without a record the refusal must be exactly what it was before this feature existed, or every operator
// who has learned to read it has to learn again.
func TestAcquireRefusalIsUnchangedWithoutAResidueRecord(t *testing.T) {
	n := heldByAnother(t, "")

	_, err := decideAcquire(observe(n), acquisitionSeed("tx-new", "r8", "reclaim-on"), "t1")
	// refusal.Error() always prepends "reason: ", so the pre-existing message carries that prefix too — this
	// is what every caller has always seen, not a new addition.
	want := `foreign-owner: node platform-worker is held by run "r7" under tx "tx-old" since "t0"`
	if err == nil || err.Error() != want {
		t.Fatalf("refusal is %v, want exactly %q", err, want)
	}
}

// An informational field must not invent a failure mode. This is deliberately unlike the journal, where an
// unreadable document IS a refusal, because the journal decides ownership and this record decides nothing.
func TestAnUnreadableResidueRecordDoesNotBecomeItsOwnRefusal(t *testing.T) {
	n := heldByAnother(t, "{not json")

	_, err := decideAcquire(observe(n), acquisitionSeed("tx-new", "r8", "reclaim-on"), "t1")
	var r *refusal
	if !asRefusal(err, &r) || r.Reason != reasonForeignOwner {
		t.Fatalf("refusal is %v, want reason %q; an unreadable explanation must not change what the node IS",
			err, reasonForeignOwner)
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("refusal hides that an unreadable residue record is present; got:\n%s", err.Error())
	}
}

// A quarantined node is refused for being quarantined, whatever else it carries — the quarantine branch runs
// first and is the one state this tool has no remaining move for.
//
// The fixture also carries a journal, on top of the quarantine record, so reasonForeignOwner is a genuinely
// live alternative here and not merely absent. A fixture with no journal at all would still pass this
// assertion if the quarantine check were moved anywhere before the success return, since nothing else would
// ever fire for it either way — that would prove only "quarantine is checked eventually," not "quarantine is
// checked first."
func TestQuarantineStillWinsOverAResidueNote(t *testing.T) {
	q, err := encodeQuarantine(quarantine{Schema: quarantineSchema, QuarantineID: "q1", ForcedAt: "t",
		Node: "platform-worker", NodeUID: "uid-node"})
	if err != nil {
		t.Fatalf("encode quarantine: %v", err)
	}
	n := heldByAnother(t, residueRaw(t, residueLeft{Kind: "Namespace", Name: "queuelab-r7", Absence: "present"}))
	n.Annotations[quarantineKey] = q

	_, aerr := decideAcquire(observe(n), acquisitionSeed("tx-new", "r8", "reclaim-on"), "t1")
	var r *refusal
	if !asRefusal(aerr, &r) || r.Reason != reasonQuarantined {
		t.Fatalf("refusal is %v, want reason %q", aerr, reasonQuarantined)
	}
}

// Every field the note prints came out of a Node annotation, and the note ends by handing the operator a
// command to run. That is the same argument quotedTaints and inspectWorker's held branch already make about
// their own decoded fields, and it applies here with more force: this refusal is printed by the RUN path too,
// so it reaches a terminal without anybody having asked to inspect anything. Unescaped, a crafted kind or
// record path can erase the line it is on and print what looks like a legitimate queuelabrun command a few
// characters above a real one.
//
// The payload goes in through encodeResidue rather than being hand-written, so it travels the whole way a
// real one would: JSON-escaped into the annotation, decoded back into raw bytes by decodeResidue, and only
// then rendered. A hand-written annotation would prove nothing about the render.
//
// Mutation: change any one of the five %q verbs residueNote uses for its decoded fields — kind, name,
// absence, leftAt, recordPath — back to %s, and the control bytes reach the terminal.
func TestResidueNoteEscapesEveryFieldItDecodedOutOfTheAnnotation(t *testing.T) {
	// Erase the line, recolour, and offer a break-glass command this tool never printed.
	payload := "\x1b[2K\x1b[32m queuelabrun -force-release -worker platform-worker -accept-divergence\a"
	raw, err := encodeResidue(residueRecord{
		Schema: residueSchema, TxID: "tx-old", RunID: "r7",
		LeftAt:     "2026-08-13T01:07:29Z" + payload,
		RecordPath: "queuelabrun-record.json" + payload,
		Left: []residueLeft{{
			Kind: "Namespace" + payload, Name: "queuelab-r7" + payload, Absence: "present" + payload,
		}},
	})
	if err != nil {
		t.Fatalf("encode residue: %v", err)
	}
	n := heldByAnother(t, raw)

	_, aerr := decideAcquire(observe(n), acquisitionSeed("tx-new", "r8", "reclaim-on"), "t1")
	var r *refusal
	if !asRefusal(aerr, &r) || r.Reason != reasonForeignOwner {
		t.Fatalf("refusal is %v, want reason %q", aerr, reasonForeignOwner)
	}
	msg := aerr.Error()
	// The note has to have actually rendered, or this proves only that a refusal without one carries no
	// control bytes.
	if !strings.Contains(msg, "teardown left 1 object(s)") {
		t.Fatalf("the residue note did not render, so none of its fields were printed:\n%s", msg)
	}
	if !strings.Contains(msg, "full record:") {
		t.Fatalf("the record path line did not render:\n%s", msg)
	}
	for _, control := range []string{"\x1b", "\a"} {
		if strings.Contains(msg, control) {
			t.Fatalf("control byte %q reached the terminal:\n%q", control, msg)
		}
	}
	// Escaped, not stripped: an operator has to be able to see what is actually on the node.
	if !strings.Contains(msg, `\x1b[2K`) {
		t.Fatalf("the escape sequence must remain visible, just inert:\n%s", msg)
	}
}
