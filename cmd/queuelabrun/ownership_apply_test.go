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
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return scheme
}

func conflictErr(name string) error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, name, fmt.Errorf("conflict"))
}

// lostResponseErr is a non-conflict failure of the kind that proves nothing about whether the write
// committed: this is exactly the class of error resolveAmbiguousAcquire exists to resolve, since retrying
// it blindly could double-apply and refusing it blindly could abandon a node still carrying our markers.
func lostResponseErr() error {
	return apierrors.NewInternalError(fmt.Errorf("connection reset by peer"))
}

// The optimistic lock only does its job if a 409 makes the caller re-read and re-decide rather than resend
// the same patch: this is the mechanism the whole design exists for, and until now nothing exercised it.
func TestAcquireWorkerRetriesConflictWithFreshReadAndDecide(t *testing.T) {
	n := node(nil, nil)
	var getCalls, patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			getCalls++
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			if patchCalls == 1 {
				return conflictErr(obj.GetName())
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-a", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire must succeed once the second attempt lands: %v", err)
	}
	if j.TxID != "tx-a" {
		t.Fatalf("journal txid = %q, want tx-a", j.TxID)
	}
	if patchCalls != 2 {
		t.Fatalf("want exactly 2 patch attempts (1 conflict + 1 success), got %d", patchCalls)
	}
	// One Get per acquire-loop attempt (2) plus verifyAcquired's own Get (1): the second patch attempt can
	// only have succeeded against the tracker's current resourceVersion if it was built from a fresh Get
	// rather than resending the stale patch built from the first Get.
	if getCalls != 3 {
		t.Fatalf("want 3 gets (2 acquire attempts + 1 verify), got %d — the retry did not re-read", getCalls)
	}
}

// A conflict that never clears must not spin forever: it has to refuse once the bound is exhausted.
func TestAcquireWorkerRefusesAfterConflictBoundNotSpin(t *testing.T) {
	n := node(nil, nil)
	var getCalls, patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			getCalls++
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return conflictErr(obj.GetName())
		},
	}).Build()

	_, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-b", "r1", "A-honor"))
	if err == nil {
		t.Fatal("acquire must refuse once the conflict bound is exhausted")
	}
	if patchCalls != acquireAttempts {
		t.Fatalf("want exactly %d patch attempts (the bound), got %d — it did not stop spinning",
			acquireAttempts, patchCalls)
	}
	if getCalls != acquireAttempts {
		t.Fatalf("want exactly %d gets (one fresh read per attempt), got %d", acquireAttempts, getCalls)
	}
}

// A refusal from decideAcquire is a decision about observed state, not a transient failure, so it must
// return on the first read without touching the API server again.
func TestAcquireWorkerReturnsRefusalWithoutRetry(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint())
	var getCalls, patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			getCalls++
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	_, err = acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-c", "r9", "A-honor"))
	var r *refusal
	if err == nil || !asRefusal(err, &r) || r.Reason != reasonForeignOwner {
		t.Fatalf("want a foreign-owner refusal, got %v", err)
	}
	if getCalls != 1 {
		t.Fatalf("want exactly 1 get, got %d — a refusal must not be retried", getCalls)
	}
	if patchCalls != 0 {
		t.Fatalf("want 0 patches, got %d — a refusal must not attempt to write", patchCalls)
	}
}

// This is the regression test for the critical finding in Task 3's review: acquireWorker's self-release
// must run on a fresh background context, never on ctx itself. A signal landing the instant after the
// acquire Patch commits is exactly what makes verifyAcquired fail via its ctx.Done() branch, and if the
// self-release that follows reused the same cancelled ctx, its own first Get would fail immediately, the
// release Patch would never even be attempted, and the label, taint and journal would be stranded on the
// node with nothing left to undo them — the exact failure this task exists to prevent.
//
// The interceptor simulates what a real REST client does on a cancelled context (the fake client does not
// check ctx on its own): a Get issued after ctx is cancelled fails immediately, exactly like the trace the
// review walked.
func TestAcquireWorkerSelfReleaseSurvivesContextCancelledDuringVerify(t *testing.T) {
	n := node(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var patched bool
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			patched = true
			// The signal lands the instant after the acquire Patch commits — exactly the window
			// verifyAcquired's ctx.Done() branch, and this test, exist for.
			cancel()
			return nil
		},
	}).Build()

	if _, err := acquireWorker(ctx, fc, "platform-worker", acquisitionSeed("tx-cancel", "r1", "A-honor")); err == nil {
		t.Fatal("a cancelled verify must refuse acquisition")
	}
	if !patched {
		t.Fatal("test did not exercise the patch-then-cancel window it depends on")
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := got.Labels[workerLabelKey]; ok {
		t.Fatalf("self-release did not run: node still carries the ownership label: %+v", got.Labels)
	}
	if _, ok := got.Annotations[journalKey]; ok {
		t.Fatalf("self-release did not run: node still carries the journal: %+v", got.Annotations)
	}
	for _, tt := range got.Spec.Taints {
		if tt.Key == workerTaintKey {
			t.Fatalf("self-release did not run: node still carries the ownership taint: %+v", tt)
		}
	}
}

// A script wrapping -inspect-worker treats a nil error as "healthy"; an unreadable quarantine record needs
// a human, and printing the warning while still returning nil would make that state indistinguishable from
// FREE or HELD to anything checking the exit code rather than reading the output.
// It is also a NAMED refusal rather than a bare error: every other refusal this state machine can reach is
// classifiable by reason, and the one state the tool has no remaining move for was the exception.
func TestInspectWorkerReturnsErrorOnUnreadableQuarantine(t *testing.T) {
	n := node(nil, map[string]string{quarantineKey: "{not valid json"})
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	err := inspectWorker(context.Background(), fc, "platform-worker")
	if err == nil {
		t.Fatal("an unreadable quarantine record must return an error, not exit 0")
	}
	var r *refusal
	if !asRefusal(err, &r) || r.Reason != reasonBadQuarantine {
		t.Fatalf("want the %s refusal, got %v", reasonBadQuarantine, err)
	}
	// Naming the state must not cost the cause: a caller that wants to tell a decode failure from a transport
	// one needs the error itself, not a sentence containing it.
	if cause := errors.Unwrap(err); cause == nil {
		t.Fatal("the decode failure must stay reachable through the error chain")
	}
}

// captureStdout runs fn with os.Stdout redirected and returns everything it printed.
//
// The escaping this file's inspection tests assert on only exists on the way to a terminal, so asserting it
// means reading what was actually written rather than what was passed in.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return out
}

// quotedOrNone escapes the raw annotation documents, but the fields DECODED out of them were still printed
// raw — including the transaction id, which goes straight into a -release-stale command the operator is
// being invited to copy, and the taints, which went out through %+v. Decoding a hostile string does not make
// it safe: anyone who can write the Node chooses those bytes.
func TestInspectWorkerEscapesFieldsDecodedOutOfNodeAnnotations(t *testing.T) {
	// Erase the line, recolour, and print the reassuring words this tool would otherwise print for a healthy
	// node — the same payload the raw-document test uses, now placed in the decoded fields.
	payload := "\x1b[2K\x1b[32mFREE.\a"
	j := testJournal()
	j.TxID = "tx-" + payload
	j.RunID = "r7" + payload
	j.Arm = "A-honor" + payload
	j.TakenAt = "2026-08-06T10:00:00Z" + payload
	// decodeJournal checks only that installed.taintEffect is non-empty, so the effect is node-controlled
	// bytes too. This one reaches the terminal through verifyInstalled's divergence refusal, which
	// inspectWorker prints immediately above the -force-release command it invites the operator to copy.
	j.Installed.TaintEffect = corev1.TaintEffect("NoSchedule" + payload)
	raw, err := encodeJournal(j)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tainted := corev1.Taint{Key: workerTaintKey, Value: "r7" + payload, Effect: corev1.TaintEffectNoSchedule}
	n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: raw}, tainted)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	var ierr error
	out := captureStdout(t, func() { ierr = inspectWorker(context.Background(), fc, "platform-worker") })
	if ierr != nil {
		t.Fatalf("inspecting a held node must succeed: %v", ierr)
	}

	// The decoding round trip has to have actually happened, or this test proves only that the raw document
	// was escaped.
	if !strings.Contains(out, "HELD by run") {
		t.Fatalf("the held branch did not run, so nothing decoded was printed:\n%s", out)
	}
	// The divergence warning is the second decoded surface, and the one that renders the journal's own
	// installed values through a refusal rather than directly.
	if !strings.Contains(out, "the installed values have diverged") {
		t.Fatalf("the divergence branch did not run, so the refusal's fields were never printed:\n%s", out)
	}
	for _, control := range []string{"\x1b", "\a"} {
		if strings.Contains(out, control) {
			t.Fatalf("control byte %q reached the terminal:\n%q", control, out)
		}
	}
	// Escaped, not stripped: an operator has to be able to see what is on the node.
	if !strings.Contains(out, `\x1b[2K`) {
		t.Fatalf("the escape sequence must remain visible, just inert:\n%s", out)
	}
	// The taints line specifically, which was the %+v.
	if !strings.Contains(out, "taints on "+workerTaintKey+": [") {
		t.Fatalf("the taints line is missing:\n%s", out)
	}
}

// The recovery tool must not contradict the runner about the state of a node, and it must never say the
// safe-sounding thing when it does.
//
// A node carrying an undecodable journal and no marker used to fall through inspectWorker's switch to the
// default branch, print FREE and exit 0, while decideAcquire refuses that same node as unreadable-journal.
// An operator, or a script reading the exit code, would then keep pointing runs at a node no run can ever
// take. This asserts both halves at once, so the two can never silently disagree again.
func TestInspectWorkerRefusesAnUnreadableJournalRatherThanReportingFree(t *testing.T) {
	n := node(nil, map[string]string{journalKey: "{not valid json"})
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	err := inspectWorker(context.Background(), fc, "platform-worker")
	if err == nil {
		t.Fatal("an undecodable journal must return an error; reporting FREE and exiting 0 is the bug")
	}
	if !strings.Contains(err.Error(), "unreadable ownership journal") {
		t.Fatalf("the refusal must name what it found, got: %v", err)
	}

	// The other half of the contradiction: acquisition refuses this exact node, so inspection saying FREE
	// would have been wrong rather than merely terse.
	_, aerr := decideAcquire(observe(n), acquisitionSeed("tx-new", "r1", "A-honor"), "t")
	var r *refusal
	if !asRefusal(aerr, &r) || r.Reason != reasonBadJournal {
		t.Fatalf("acquisition must refuse the same node as %s, got %v", reasonBadJournal, aerr)
	}
}

// This is the round-4 finding the two-step quarantine design exists to satisfy: a second force must never
// even attempt a write, because a retry that reached the API server would overwrite the original record
// with one describing an already-emptied node, destroying the only surviving evidence of who held it.
func TestForceQuarantineRefusesWhenAlreadyQuarantinedWithoutPatching(t *testing.T) {
	q := quarantine{Schema: quarantineSchema, QuarantineID: "q1", ForcedAt: "t", Node: "platform-worker",
		NodeUID: "uid-node"}
	raw, err := encodeQuarantine(q)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(nil, map[string]string{quarantineKey: raw})
	var patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	if err := forceQuarantine(context.Background(), fc, "platform-worker", "uid-node"); err == nil {
		t.Fatal("forcing an already-quarantined node must refuse")
	}
	if patchCalls != 0 {
		t.Fatalf("want 0 patch attempts, got %d — a refusal must never touch the API server", patchCalls)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[quarantineKey] != raw {
		t.Fatalf("the original quarantine record must survive untouched, got %q", got.Annotations[quarantineKey])
	}
}

// The happy path: forcing a held node preserves what it removes in the quarantine record and leaves the
// node carrying no label, no journal and no ownership taint — never a free node in one step.
func TestForceQuarantineBreaksAHeldNodeIntoAQuarantineRecord(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint())

	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()
	if err := forceQuarantine(context.Background(), fc, "platform-worker", "uid-node"); err != nil {
		t.Fatalf("force: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := got.Labels[workerLabelKey]; ok {
		t.Fatalf("force must remove the label, got %+v", got.Labels)
	}
	if _, ok := got.Annotations[journalKey]; ok {
		t.Fatalf("force must remove the journal, got %+v", got.Annotations)
	}
	for _, tt := range got.Spec.Taints {
		if tt.Key == workerTaintKey {
			t.Fatalf("force must remove the ownership taint, got %+v", tt)
		}
	}
	raw, ok := got.Annotations[quarantineKey]
	if !ok {
		t.Fatal("force must leave a quarantine record")
	}
	q, err := decodeQuarantine(raw)
	if err != nil {
		t.Fatalf("decode quarantine: %v", err)
	}
	if q.PriorJournal != good || q.ObservedLabel != "r7" {
		t.Fatalf("quarantine record does not preserve what was removed: %+v", q)
	}
}

// clearQuarantine must remove only the exact record the operator names, leaving the node otherwise free.
func TestClearQuarantineRemovesTheNamedRecord(t *testing.T) {
	q := quarantine{Schema: quarantineSchema, QuarantineID: "q1", ForcedAt: "t", Node: "platform-worker",
		NodeUID: "uid-node"}
	raw, err := encodeQuarantine(q)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(nil, map[string]string{quarantineKey: raw})
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	if err := clearQuarantine(context.Background(), fc, "platform-worker", "q1"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := got.Annotations[quarantineKey]; ok {
		t.Fatalf("clear must remove the quarantine record, got %+v", got.Annotations)
	}
}

// releaseStale is the ordinary recovery path: it must restore the node once the operator names the correct
// transaction id, and must refuse without writing anything for the wrong one.
func TestReleaseStaleRestoresTheNamedTransaction(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint())
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	if err := releaseStale(context.Background(), fc, "platform-worker", "tx-1111"); err != nil {
		t.Fatalf("release: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := got.Labels[workerLabelKey]; ok {
		t.Fatalf("release must remove the label, got %+v", got.Labels)
	}
	if _, ok := got.Annotations[journalKey]; ok {
		t.Fatalf("release must remove the journal, got %+v", got.Annotations)
	}
}

func TestReleaseStaleRefusesTheWrongTxIDWithoutWriting(t *testing.T) {
	good, err := encodeJournal(testJournal())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	n := node(map[string]string{workerLabelKey: "r7"}, map[string]string{journalKey: good}, ourTaint())
	var patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	if err := releaseStale(context.Background(), fc, "platform-worker", "tx-wrong"); err == nil {
		t.Fatal("releasing the wrong tx id must refuse")
	}
	if patchCalls != 0 {
		t.Fatalf("want 0 patch attempts, got %d — a refusal must not write", patchCalls)
	}
}

// Dedicating the worker must not delete anything else the cluster put on it, and the pure-helper test in
// ownership_test.go is not enough to pin that: the merge patch replaces spec.taints wholesale, so building
// the new list from the ownership-key subset instead of the whole observed list would silently strip this
// repository's own nodehealth unhealthy taint from the worker as a side effect. Every fake-client acquire
// test elsewhere in this file uses a Node with no taints, so that substitution would pass all of them.
//
// It runs the real acquire and the real release end to end, because the two patches are where the whole
// list is actually written, and asserts an unrelated taint, label and annotation survive both.
func TestAcquireAndReleasePreserveUnrelatedNodeState(t *testing.T) {
	unrelated := corev1.Taint{Key: "gpu-platform/unhealthy", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	n := node(map[string]string{"unrelated-label": "keep"}, map[string]string{"unrelated-ann": "keep"}, unrelated)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-preserve", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(got.Spec.Taints) != 2 {
		t.Fatalf("acquire must add its taint alongside the unrelated one, got %+v", got.Spec.Taints)
	}
	if !hasTaint(got.Spec.Taints, unrelated) {
		t.Fatalf("acquire deleted the unrelated taint: %+v", got.Spec.Taints)
	}
	if got.Labels["unrelated-label"] != "keep" {
		t.Fatalf("acquire deleted the unrelated label: %+v", got.Labels)
	}
	if got.Annotations["unrelated-ann"] != "keep" {
		t.Fatalf("acquire deleted the unrelated annotation: %+v", got.Annotations)
	}

	if err := releaseOwned(context.Background(), fc, j); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(got.Spec.Taints) != 1 || got.Spec.Taints[0] != unrelated {
		t.Fatalf("release must leave exactly the unrelated taint, got %+v", got.Spec.Taints)
	}
	if got.Labels["unrelated-label"] != "keep" || got.Annotations["unrelated-ann"] != "keep" {
		t.Fatalf("release deleted unrelated state: labels %+v annotations %+v", got.Labels, got.Annotations)
	}
	if _, ok := got.Labels[workerLabelKey]; ok {
		t.Fatalf("release must remove the ownership label, got %+v", got.Labels)
	}
	if _, ok := got.Annotations[journalKey]; ok {
		t.Fatalf("release must remove the journal, got %+v", got.Annotations)
	}
}

// The companion of the test above, for the case where our taint is the only one: the resulting empty list
// is what a real API server has to accept for a worker to end the run schedulable again.
func TestReleaseEmptiesTheTaintListWhenOnlyOursWasPresent(t *testing.T) {
	n := node(nil, nil)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-only", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := releaseOwned(context.Background(), fc, j); err != nil {
		t.Fatalf("release: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(got.Spec.Taints) != 0 {
		t.Fatalf("release must leave no taints, got %+v", got.Spec.Taints)
	}
}

func hasTaint(taints []corev1.Taint, want corev1.Taint) bool {
	return slices.Contains(taints, want)
}

// This is the critical finding of the whole-branch review, and the exact shape of the failure this branch
// exists to stop: a run whose markers are removed under it must not be able to report a clean release and
// publish a number.
//
// The removal is reachable three realistic ways — an operator running -release-stale against a transaction
// they believe is dead but is not, the -force-release then -clear-quarantine pair run while a run is live,
// or the Node being deleted and recreated — and all three land on the same observable state: a free node.
// Note the asymmetry that makes this the dangerous window rather than an obvious one: if a second run has
// already acquired by the time this run releases, decideRelease refuses with not-our-transaction and the
// run correctly invalidates. It is precisely the case where the node is left FREE that used to publish.
func TestReleaseOwnedInvalidatesWhenMarkersVanishMidRun(t *testing.T) {
	n := node(nil, nil)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-vanish", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// An operator frees the node while this run is still holding it, using the tool's own stale release.
	if err := releaseStale(context.Background(), fc, "platform-worker", j.TxID); err != nil {
		t.Fatalf("stale release: %v", err)
	}

	err = releaseOwned(context.Background(), fc, j)
	var r *refusal
	if err == nil || !asRefusal(err, &r) || r.Reason != reasonOwnershipLost {
		t.Fatalf("the run's own release must invalidate once its markers are gone, got %v", err)
	}
	if !strings.Contains(err.Error(), j.TxID) {
		t.Fatalf("the invalidation must name the transaction that lost the worker: %v", err)
	}

	// The operator paths keep the opposite contract on the very same state, which is why this is fixed at
	// the call site and not in decideRelease: a node with nothing of ours on it is still a clean success for
	// a stale release racing a genuine one, and for acquire's own self-release.
	act, err := releaseAcquired(context.Background(), fc, j)
	if err != nil || act != releaseAlreadyDone {
		t.Fatalf("releaseAcquired on a clean node = %v, %v; want releaseAlreadyDone, nil", act, err)
	}
}

// resolveAmbiguousAcquire is the only path that can return "acquired" without going through verifyAcquired,
// and it answers one of the five adversarial findings this branch exists to close: a non-conflict Patch
// error may still have committed. These four tests cover its three-way switch and its cancellation branch.
//
// First: the write landed and only the response was lost, which must resolve to acquired rather than
// abandoning a node that now carries our markers.
func TestResolveAmbiguousAcquireAcceptsAPatchWhoseResponseWasLost(t *testing.T) {
	n := node(nil, nil)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			// The patch committed; the caller never learns that.
			return lostResponseErr()
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-lost", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("a committed patch with a lost response must resolve to acquired: %v", err)
	}
	if j.TxID != "tx-lost" {
		t.Fatalf("journal txid = %q, want tx-lost", j.TxID)
	}
}

// The P0 of this pass: a single free-looking read is not proof the write is dead.
//
// A linearizable read orders only COMPLETED operations, and the write being resolved is precisely the one
// whose completion is unknown — a timed-out or disconnected request can still be in the API server's apply
// path and commit after the read returns. The resolver used to conclude "failed and did not land" from that
// first observation and then discard the transaction id, so the markers appearing a moment later belonged to
// nobody: no run could acquire the node and no recovery mode could name what to release.
//
// The interceptor reproduces exactly that: the patch reports a lost response without applying, and the write
// commits after the resolve loop's first read. The resolver must see it and resolve to acquired.
func TestResolveAmbiguousAcquireDoesNotCallAWriteDeadFromOneFreeRead(t *testing.T) {
	n := node(nil, nil)
	var (
		inFlight *corev1.Node // the write that was accepted but whose response was lost
		gets     int
	)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			gets++
			// gets 1 is the acquire loop's own read; gets 2 is the resolve loop's first re-read, which sees a
			// free node because the in-flight write has not committed yet. It commits immediately afterwards.
			if gets == 2 && inFlight != nil {
				pending := inFlight
				inFlight = nil
				var current corev1.Node
				if gerr := c.Get(ctx, client.ObjectKey{Name: "platform-worker"}, &current); gerr != nil {
					return gerr
				}
				current.Labels = pending.Labels
				current.Annotations = pending.Annotations
				current.Spec.Taints = pending.Spec.Taints
				if uerr := c.Update(ctx, &current); uerr != nil {
					return uerr
				}
			}
			return err
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// Accepted by the server, never applied yet, and the caller is told only that the connection died.
			inFlight = obj.(*corev1.Node).DeepCopy()
			return lostResponseErr()
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-late", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("a write that commits after the first re-read must resolve to acquired, got: %v", err)
	}
	if j.TxID != "tx-late" {
		t.Fatalf("journal txid = %q, want tx-late", j.TxID)
	}
	if gets < 3 {
		t.Fatalf("the test did not reach a second re-read (gets=%d), so it proved nothing", gets)
	}

	// The node really is held by us: had the resolver concluded "did not land", these markers would be on a
	// node nothing could name.
	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[journalKey] == "" || got.Labels[workerLabelKey] != "r1" {
		t.Fatalf("the test did not produce the late-commit state it exists for: %+v %+v", got.Labels, got.Annotations)
	}
}

// The companion direction: a free read that arrives AFTER a failed one must not conclude anything either.
//
// The failed read is the point. It leaves a hole in the window — a moment nobody observed, which is where a
// write in flight could have committed and been undone by nothing — so the free observation that follows it
// no longer stands for an unbroken window, and the outcome must stay UNRESOLVED with the transaction id and
// the operator's next command intact.
//
// This discriminates: against the pre-fix resolver the free read at attempt 1 returns "failed and did not
// land" on the spot, before the cancellation below is ever reached. The cancellation is only how the test
// leaves the loop cheaply; the full-window exhaustion of the freeThroughout gate is covered by
// TestResolveAmbiguousAcquireRefusesAPatchThatDidNotLand.
func TestResolveAmbiguousAcquireWillNotCallAWriteDeadOnAFreeReadAfterAFailedOne(t *testing.T) {
	n := node(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gets int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(gctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			gets++
			switch gets {
			case 1:
				// The acquire loop's own read, which has to succeed or nothing reaches the patch.
				return c.Get(gctx, key, obj, opts...)
			case 2:
				// The resolve loop's first re-read fails, so this attempt observed nothing at all.
				return apierrors.NewInternalError(fmt.Errorf("apiserver is not answering"))
			default:
				// And now the node reads FREE. The pre-fix resolver called the write dead right here.
				cancel()
				return c.Get(gctx, key, obj, opts...)
			}
		},
		Patch: func(pctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			return lostResponseErr()
		},
	}).Build()

	_, err := acquireWorker(ctx, fc, "platform-worker", acquisitionSeed("tx-blind2", "r1", "A-honor"))
	if err == nil {
		t.Fatal("a resolution window with a hole in it must refuse")
	}
	if gets < 3 {
		t.Fatalf("the test never reached the free read after the failed one (gets=%d), so it proved nothing", gets)
	}
	if strings.Contains(err.Error(), "did not land") {
		t.Fatalf("a free read following an unobserved gap must not conclude the write is dead: %v", err)
	}
	if !strings.Contains(err.Error(), "UNRESOLVED") || !strings.Contains(err.Error(), "tx-blind2") {
		t.Fatalf("the refusal must be UNRESOLVED and keep the transaction id, got: %v", err)
	}
}

// Second: the error was real and the write never committed. The node is free for the whole resolution
// window, so there is nothing of ours to undo and the refusal must say so rather than leaving the operator
// to wonder.
//
// This is the one test that pays the full resolveAttempts * resolveInterval in real sleep, and that cost is
// the fix: "did not land" is now a conclusion the window has to earn rather than one the first read may
// announce.
func TestResolveAmbiguousAcquireRefusesAPatchThatDidNotLand(t *testing.T) {
	n := node(nil, nil)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			return lostResponseErr()
		},
	}).Build()

	_, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-nolanding", "r1", "A-honor"))
	if err == nil {
		t.Fatal("a patch that did not land must refuse acquisition")
	}
	if !strings.Contains(err.Error(), "did not land") || !strings.Contains(err.Error(), "tx-nolanding") {
		t.Fatalf("the refusal must say it did not land and name the transaction, got: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	// The claim is that THIS TRANSACTION wrote nothing, which is narrower than the node being bare — and the
	// difference stopped being academic when the worker started carrying a termination qualification, an
	// annotation that is a property of the machine rather than of anybody's transaction and that was on this
	// fixture before acquisition was attempted. Asserting an empty annotation map would fail on it and would
	// be asserting something acquisition never promised.
	if len(got.Labels) != 0 || len(got.Spec.Taints) != 0 {
		t.Fatalf("nothing may have been written: %+v %+v", got.Labels, got.Spec.Taints)
	}
	for _, k := range []string{journalKey, quarantineKey, residueKey} {
		if v, ok := got.Annotations[k]; ok {
			t.Fatalf("annotation %s was written by an acquisition that did not land: %q", k, v)
		}
	}
}

// Third: another transaction holds the node by the time we look, so our patch demonstrably lost the race
// and the refusal must name the holder instead of resolving to acquired.
func TestResolveAmbiguousAcquireRefusesWhenAnotherTransactionHoldsTheNode(t *testing.T) {
	foreign := testJournal()
	foreign.TxID = "tx-other"
	foreign.RunID = "r9"
	foreign.Installed = installedTuple{LabelValue: "r9", TaintValue: "r9", TaintEffect: corev1.TaintEffectNoSchedule}
	foreignRaw, err := encodeJournal(foreign)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	n := node(nil, nil)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// Our patch never reaches the API server, and by the time we look a second run has acquired.
			var other corev1.Node
			if err := c.Get(ctx, client.ObjectKey{Name: "platform-worker"}, &other); err != nil {
				return err
			}
			other.Labels = map[string]string{workerLabelKey: "r9"}
			other.Annotations = map[string]string{journalKey: foreignRaw}
			other.Spec.Taints = []corev1.Taint{
				{Key: workerTaintKey, Value: "r9", Effect: corev1.TaintEffectNoSchedule},
			}
			if err := c.Update(ctx, &other); err != nil {
				return err
			}
			return lostResponseErr()
		},
	}).Build()

	if _, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-loser", "r1", "A-honor")); err == nil {
		t.Fatal("acquisition must refuse when another transaction holds the node")
	} else if !strings.Contains(err.Error(), "tx-other") {
		t.Fatalf("the refusal must name the holding transaction, got: %v", err)
	}
}

// Fourth, and the regression this whole path most needs: our journal is present under our own TxID but an
// installed value was altered — a mutating webhook is the realistic actor — and that must NOT resolve to
// acquired. Treating "journal present with our TxID" as acquired is the exact shortcut the design forbids,
// because the run would then hold a node that no longer routes work the way the flavor expects.
//
// The context is cancelled once the resolve loop has seen the state, which both bounds the test and covers
// the cancellation branch: it must report UNRESOLVED and hand the node to the operator modes by name.
func TestResolveAmbiguousAcquireRefusesAPartiallyLandedPatch(t *testing.T) {
	n := node(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gets int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			gets++
			if gets >= 2 {
				// The resolve loop has now observed the partial state once; cancelling here stands in for the
				// operator's Ctrl-C and keeps the test off the full resolve bound.
				cancel()
			}
			return err
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// A mutating webhook keeps our journal and rewrites the label the flavor selects on.
			if nd, ok := obj.(*corev1.Node); ok {
				nd.Labels[workerLabelKey] = "someone-else"
			}
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			return lostResponseErr()
		},
	}).Build()

	j, err := acquireWorker(ctx, fc, "platform-worker", acquisitionSeed("tx-partial", "r1", "A-honor"))
	if err == nil {
		t.Fatal("a partially landed patch must never resolve to acquired")
	}
	if j.TxID != "" {
		t.Fatalf("a refused acquisition must return no journal, got %+v", j)
	}
	if !strings.Contains(err.Error(), "UNRESOLVED") || !strings.Contains(err.Error(), "tx-partial") {
		t.Fatalf("the refusal must be UNRESOLVED and name the transaction, got: %v", err)
	}
	if !strings.Contains(err.Error(), "-inspect-worker -worker platform-worker") {
		t.Fatalf("the refusal must hand the operator a runnable command, got: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[journalKey] == "" || got.Labels[workerLabelKey] != "someone-else" {
		t.Fatalf("the test did not produce the partial state it exists for: %+v %+v", got.Labels, got.Annotations)
	}
}

// Finding 1 of the branch review: -release-stale against a node that already carries nothing for the named
// transaction used to pass the zero journal (its Node field is "") straight to releaseAcquired, which then
// tried to Get an empty node name and failed with a confusing API error — exactly the mode an operator reaches
// for right after a crash, plausibly twice. releaseStale must instead recognise releaseAlreadyDone itself,
// report it plainly, and never call releaseAcquired at all.
func TestReleaseStaleOnAnAlreadyCleanNodeReportsAlreadyReleasedRatherThanFailing(t *testing.T) {
	n := node(nil, nil)
	var patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	if err := releaseStale(context.Background(), fc, "platform-worker", "tx-noop"); err != nil {
		t.Fatalf("releaseStale against an already-clean node must succeed, got: %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("want 0 patches against an already-clean node, got %d — releaseAcquired must never be called "+
			"with the zero journal", patchCalls)
	}
}

// Finding 3 of the branch review: releaseAcquired used to return success the instant Patch reported no error,
// without re-reading, even though acquisition has verifyAcquired for exactly this reason. This is the ordinary
// path: it must actually perform the post-release read rather than trust the status code alone.
func TestReleaseVerifiesRestorationAfterThePatchOnTheOrdinaryPath(t *testing.T) {
	n := node(nil, nil)
	var getCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			getCalls++
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-clean", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	getCalls = 0 // only count reads from the release path onward.

	if _, err := releaseAcquired(context.Background(), fc, j); err != nil {
		t.Fatalf("release: %v", err)
	}
	if getCalls < 2 {
		t.Fatalf("want at least 2 gets (release's own decide read plus the post-patch verification read), "+
			"got %d — verifyReleased did not run", getCalls)
	}
}

// The failing direction of Finding 3: our own markers are somehow still on the node after our release patch
// committed (a retried write landing late is a realistic cause). Verification must catch this rather than
// report a clean release, because the whole branch exists to stop a run that looked fine from being allowed
// to count.
func TestReleaseFailsVerificationWhenOurOwnMarkersReappearAfterThePatch(t *testing.T) {
	n := node(nil, nil)
	var patchCalls int
	var j journal
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if patchCalls == 2 {
				// The release patch (call 2, after acquire's call 1) committed and removed our markers; a
				// late-landing retry of our own earlier write puts the exact same values straight back.
				var got corev1.Node
				if err := c.Get(ctx, client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
					return err
				}
				if got.Labels == nil {
					got.Labels = map[string]string{}
				}
				got.Labels[workerLabelKey] = j.Installed.LabelValue
				got.Spec.Taints = append(got.Spec.Taints, corev1.Taint{
					Key: workerTaintKey, Value: j.Installed.TaintValue, Effect: j.Installed.TaintEffect,
				})
				return c.Update(ctx, &got)
			}
			return nil
		},
	}).Build()

	var err error
	j, err = acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-reappear", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if _, err := releaseAcquired(context.Background(), fc, j); err == nil {
		t.Fatal("release must fail verification when our own markers reappear after the patch")
	} else if !strings.Contains(err.Error(), "did not verify") {
		t.Fatalf("want a restoration-did-not-verify error, got: %v", err)
	}
}

// The legitimate direction Finding 3 warns must not become a false failure: another transaction acquires the
// node in the gap between our release patch committing and our verification read. verifyClean must prove OUR
// markers are gone, not that the node is free, so a different transaction's markers must not be mistaken for
// ours having survived.
func TestReleaseVerifiesCleanWhenAnotherTransactionAcquiresRightAfterOurRelease(t *testing.T) {
	n := node(nil, nil)
	var patchCalls int
	var j journal
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if patchCalls == 2 {
				other := testJournal()
				other.TxID = "tx-other"
				other.RunID = "r9"
				other.Installed = installedTuple{
					LabelValue: "r9", TaintValue: "r9", TaintEffect: corev1.TaintEffectNoSchedule,
				}
				raw, err := encodeJournal(other)
				if err != nil {
					return err
				}
				var got corev1.Node
				if err := c.Get(ctx, client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
					return err
				}
				got.Labels = map[string]string{workerLabelKey: "r9"}
				got.Annotations = map[string]string{journalKey: raw}
				got.Spec.Taints = []corev1.Taint{{Key: workerTaintKey, Value: "r9", Effect: corev1.TaintEffectNoSchedule}}
				return c.Update(ctx, &got)
			}
			return nil
		},
	}).Build()

	var err error
	j, err = acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-mine", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if _, err := releaseAcquired(context.Background(), fc, j); err != nil {
		t.Fatalf("release must succeed when a different, legitimate transaction's markers are present, got: %v", err)
	}
}

// The same legitimate race as the test above, with the one detail that used to break it: the foreign
// transaction reuses OUR RUN ID. The installed label and taint values are derived from the run id, so the
// node now carries values byte-identical to the ones we installed, under a journal that names a different
// transaction. Verification must read the journal, not the values, or a run whose worker was genuinely
// restored is invalidated by a coincidence it has no way to influence.
func TestReleaseVerifiesCleanWhenTheNextTransactionReusesTheSameRunID(t *testing.T) {
	n := node(nil, nil)
	var patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if patchCalls == 2 {
				// A second run acquires in the gap, under a fresh transaction but the same run id.
				other := testJournal()
				other.TxID = "tx-second"
				other.RunID = "r1"
				other.Installed = installedTuple{
					LabelValue: "r1", TaintValue: "r1", TaintEffect: corev1.TaintEffectNoSchedule,
				}
				raw, err := encodeJournal(other)
				if err != nil {
					return err
				}
				var got corev1.Node
				if err := c.Get(ctx, client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
					return err
				}
				got.Labels = map[string]string{workerLabelKey: "r1"}
				got.Annotations = map[string]string{journalKey: raw}
				got.Spec.Taints = []corev1.Taint{{Key: workerTaintKey, Value: "r1", Effect: corev1.TaintEffectNoSchedule}}
				return c.Update(ctx, &got)
			}
			return nil
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-first", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := releaseAcquired(context.Background(), fc, j); err != nil {
		t.Fatalf("release must succeed: the values match by run-id coincidence, the journal names tx-second: %v", err)
	}
}

// verifyClean is the pure invariant Finding 3 adds, tested directly against every direction named in the
// finding: it must prove OUR markers are gone (label, taint, journal, node identity), and must not demand a
// free node.
func TestVerifyClean(t *testing.T) {
	j := testJournal() // NodeUID uid-node, Installed{LabelValue: r7, TaintValue: r7, NoSchedule}

	cases := []struct {
		name    string
		obs     ownership
		wantErr bool
	}{
		{
			name:    "clean node with nothing on it",
			obs:     ownership{NodeUID: "uid-node"},
			wantErr: false,
		},
		{
			name:    "our label still present",
			obs:     ownership{NodeUID: "uid-node", HasLabel: true, LabelValue: "r7"},
			wantErr: true,
		},
		{
			name: "our taint still present",
			obs: ownership{NodeUID: "uid-node",
				Taints: []corev1.Taint{{Key: workerTaintKey, Value: "r7", Effect: corev1.TaintEffectNoSchedule}}},
			wantErr: true,
		},
		{
			name:    "our journal still present under our txID",
			obs:     ownership{NodeUID: "uid-node", JournalRaw: "x", Journal: j},
			wantErr: true,
		},
		{
			// Minor review finding: the journal invariant is "absent, or present but naming a different
			// txID" — an unreadable journal satisfies neither, so it must not read as clean.
			name:    "journal present but undecodable",
			obs:     ownership{NodeUID: "uid-node", JournalRaw: "{not valid json", JournalErr: fmt.Errorf("decode journal: unexpected end of JSON input")},
			wantErr: true,
		},
		{
			name:    "node UID no longer matches: a recreated node",
			obs:     ownership{NodeUID: "uid-different"},
			wantErr: true,
		},
		{
			name:    "a different transaction's label is present",
			obs:     ownership{NodeUID: "uid-node", HasLabel: true, LabelValue: "r9"},
			wantErr: false,
		},
		{
			name: "a different transaction's taint is present",
			obs: ownership{NodeUID: "uid-node",
				Taints: []corev1.Taint{{Key: workerTaintKey, Value: "r9", Effect: corev1.TaintEffectNoSchedule}}},
			wantErr: false,
		},
		{
			name: "a different transaction's journal is present",
			obs: ownership{NodeUID: "uid-node", JournalRaw: "x",
				Journal: journal{TxID: "tx-other"}},
			wantErr: false,
		},
		{
			// The finding this row exists for: the installed values are derived from the RUN ID, so a foreign
			// transaction that legitimately acquired after our release and happens to carry the same run id
			// installs byte-identical ones. Comparing values alone reads that as our own markers having
			// survived, and invalidates a run whose restoration actually succeeded. The journal's txID is the
			// authority, and it names somebody else.
			name: "a different transaction holds it with the same run-id-derived values",
			obs: ownership{
				NodeUID: "uid-node", JournalRaw: "x", Journal: journal{TxID: "tx-other"},
				HasLabel: true, LabelValue: "r7",
				Taints: []corev1.Taint{{Key: workerTaintKey, Value: "r7", Effect: corev1.TaintEffectNoSchedule}},
			},
			wantErr: false,
		},
		{
			// The fail-safe direction that must survive the row above: with no journal to name an owner,
			// nothing vouches for these markers, so values carrying ours are treated as ours.
			name: "our values are present with no journal to name an owner",
			obs: ownership{
				NodeUID: "uid-node", HasLabel: true, LabelValue: "r7",
				Taints: []corev1.Taint{{Key: workerTaintKey, Value: "r7", Effect: corev1.TaintEffectNoSchedule}},
			},
			wantErr: true,
		},
		{
			// And the other fail-safe direction: an unreadable journal names nobody, so it cannot license the
			// markers beside it either.
			name: "our values are present under an unreadable journal",
			obs: ownership{
				NodeUID: "uid-node", JournalRaw: "{not valid json",
				JournalErr: fmt.Errorf("decode journal: unexpected end of JSON input"),
				HasLabel:   true, LabelValue: "r7",
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyClean(tc.obs, j)
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// Release had no counterpart to resolveAmbiguousAcquire, so a non-conflict Patch error whose write actually
// landed — a proxy timeout, a connection reset after the commit — made the run print "worker not restored"
// and exit non-zero for a node that was already clean. That is a false invalidation of a valid run, which
// this lab pays for in GPU hours.
func TestReleaseResolvesAPatchWhoseResponseWasLost(t *testing.T) {
	n := node(nil, nil)
	var patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if patchCalls == 2 {
				// The release patch (call 2, after acquire's call 1) committed; the caller never learns that.
				return lostResponseErr()
			}
			return nil
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-lostrelease", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	act, err := releaseAcquired(context.Background(), fc, j)
	if err != nil {
		t.Fatalf("a release whose write landed must resolve to restored, got: %v", err)
	}
	if act != releaseRestore {
		t.Fatalf("release action = %v, want releaseRestore", act)
	}
	// releaseOwned is the caller that decides whether the run may publish, so assert the whole path, not just
	// the primitive: this is the run that must NOT be invalidated.
	if err := releaseOwned(context.Background(), fc, j); err != nil {
		// The node is clean by now, so releaseOwned's own read reports already-done; what matters is that the
		// ambiguous release above did not itself invalidate.
		var r *refusal
		if !asRefusal(err, &r) || r.Reason != reasonOwnershipLost {
			t.Fatalf("unexpected second-release error: %v", err)
		}
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := got.Labels[workerLabelKey]; ok {
		t.Fatalf("the test did not produce the landed-write state it exists for: %+v", got.Labels)
	}
}

// The direction of failure the read-back must keep safe: the patch genuinely did not land, our markers are
// still installed, and the run must still invalidate rather than treat the lost response as restoration.
func TestReleaseStillInvalidatesWhenTheReadBackFindsOurMarkersInstalled(t *testing.T) {
	n := node(nil, nil)
	var patchCalls int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			if patchCalls == 2 {
				// The release patch never reaches the API server, so the node keeps our markers.
				return lostResponseErr()
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-notlanded", "r1", "A-honor"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if _, err := releaseAcquired(context.Background(), fc, j); err == nil {
		t.Fatal("a release that did not land must not be resolved as restored")
	} else if !strings.Contains(err.Error(), "read-back could not prove restoration") {
		t.Fatalf("the refusal must say the read-back failed to prove restoration, got: %v", err)
	}

	var got corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[journalKey] == "" {
		t.Fatalf("the test did not produce the still-held state it exists for: %+v", got.Annotations)
	}
}

// The resolve loop used to discard every failed re-read and report only the patch cause, so a persistent
// authorization failure, a deleted node or a sustained API outage all surfaced as a generic unresolved write
// — and the -inspect-worker command the refusal prints would then fail for the same unreported reason.
//
// The cancellation branch is exercised rather than the full bound because both render the kept error through
// the same resolveReadNote, and reaching exhaustion costs resolveAttempts * resolveInterval of real sleep.
func TestResolveAmbiguousAcquireReportsWhyItCouldNotReadTheNode(t *testing.T) {
	n := node(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gets int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(gctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			gets++
			if gets == 1 {
				// The acquire loop's own read has to succeed, or the transaction never reaches the patch.
				return c.Get(gctx, key, obj, opts...)
			}
			// Every re-read inside the resolve loop is refused, the shape a missing RBAC binding takes.
			cancel()
			return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, key.Name,
				fmt.Errorf("node reader is not bound"))
		},
		Patch: func(pctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			return lostResponseErr()
		},
	}).Build()

	_, err := acquireWorker(ctx, fc, "platform-worker", acquisitionSeed("tx-blind", "r1", "A-honor"))
	if err == nil {
		t.Fatal("an acquisition whose outcome could never be read must refuse")
	}
	if !strings.Contains(err.Error(), "Last re-read failed") {
		t.Fatalf("the refusal must report that the re-reads themselves failed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "node reader is not bound") {
		t.Fatalf("the refusal must carry the read error itself, not just that one happened, got: %v", err)
	}
	if !strings.Contains(err.Error(), "UNRESOLVED") {
		t.Fatalf("the refusal must still be UNRESOLVED, got: %v", err)
	}
}

// The complement: when the last re-read succeeded, the refusal has to say so, or a reader assumes the reads
// must have failed because the outcome is unknown and goes looking for a connectivity problem that is not
// there.
func TestResolveReadNoteDistinguishesUnreadableFromUnresolved(t *testing.T) {
	if note := resolveReadNote(nil); !strings.Contains(note, "The last re-read succeeded") {
		t.Fatalf("a clean last read must be stated, got %q", note)
	}
	if note := resolveReadNote(fmt.Errorf("boom")); !strings.Contains(note, "boom") {
		t.Fatalf("the read error must be carried verbatim, got %q", note)
	}
}

// The kept read error is the MOST RECENT one, not the worst one ever seen. A transient failure early in the
// resolve loop followed by reads that succeed but do not resolve used to leave the stale error in place, so
// the refusal claimed "Last re-read failed" about a node the operator could in fact reach — the exact
// opposite of what the note exists to tell them.
//
// This costs one resolveInterval of real sleep, which is unavoidable: the bug needs a failed read AND a
// later successful one, and the loop always waits between attempts unless the context is already done.
func TestResolveAmbiguousAcquireReportsTheLastReadNotAStaleFailure(t *testing.T) {
	n := node(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gets int
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(gctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			gets++
			if gets == 2 {
				// One transient blip, on the resolve loop's first re-read, that then clears.
				return apierrors.NewInternalError(fmt.Errorf("transient blip that has since cleared"))
			}
			if gets >= 3 {
				// The loop has now read the unresolvable state successfully; end the test here.
				cancel()
			}
			return c.Get(gctx, key, obj, opts...)
		},
		Patch: func(pctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// A mutating webhook keeps our journal and rewrites the label, which is a state the resolve
			// switch matches no case for, so the loop keeps re-reading instead of concluding.
			if nd, ok := obj.(*corev1.Node); ok {
				nd.Labels[workerLabelKey] = "someone-else"
			}
			if err := c.Patch(pctx, obj, patch, opts...); err != nil {
				return err
			}
			return lostResponseErr()
		},
	}).Build()

	_, err := acquireWorker(ctx, fc, "platform-worker", acquisitionSeed("tx-blip", "r1", "A-honor"))
	if err == nil {
		t.Fatal("a partially landed patch must never resolve to acquired")
	}
	if gets < 3 {
		t.Fatalf("the test did not reach a successful read after the failed one (gets=%d)", gets)
	}
	if strings.Contains(err.Error(), "transient blip that has since cleared") {
		t.Fatalf("the refusal carried a stale read error the operator can no longer reproduce: %v", err)
	}
	if !strings.Contains(err.Error(), "The last re-read succeeded") {
		t.Fatalf("the refusal must report the most recent read, got: %v", err)
	}
}

// quotedOrNone is the only thing standing between a node annotation and an operator's terminal, and it was
// evidenced by a live cluster run alone: reverting it to a raw %s would have failed nothing here.
//
// The payload is what an attacker would actually write — erase the line, recolour, then print the reassuring
// word this tool would otherwise have printed for a free node — so the test fails if any of those bytes
// reach the output unescaped.
func TestQuotedOrNoneEscapesNodeControlledContent(t *testing.T) {
	if got := quotedOrNone(""); got != "(none)" {
		t.Fatalf("an absent annotation must read as (none), got %q", got)
	}

	payload := "{\"x\":\x1b[2K\x1b[31mFREE.\a"
	got := quotedOrNone(payload)
	for _, raw := range []string{"\x1b", "\a", "\n", "\r"} {
		if strings.Contains(got, raw) {
			t.Fatalf("control byte %q survived into the output %q", raw, got)
		}
	}
	if !strings.Contains(got, `\x1b[2K`) {
		t.Fatalf("the escape sequence must still be VISIBLE, just inert; got %q", got)
	}
	if got[0] != '"' || got[len(got)-1] != '"' {
		t.Fatalf("the rendered annotation must be delimited so its extent is unambiguous, got %q", got)
	}

	// A journal that is merely surprising rather than hostile must stay readable enough to act on: trailing
	// whitespace is exactly the kind of corruption an operator needs to SEE rather than have trimmed away.
	if got := quotedOrNone("{} "); !strings.HasSuffix(got, ` "`) {
		t.Fatalf("trailing whitespace must remain visible, got %q", got)
	}
}

// The operator modes deliberately run on a context no signal can cancel, which is what stops a Ctrl-C from
// half-applying a break. Unbounded, that same choice turns a hung API server into a process that never
// returns — the stranded-marker outcome moved from "Ctrl-C" to "hung apiserver" rather than prevented, which
// is the argument releaseCleanupTimeout already exists for.
func TestOperatorModeContextIsBoundedAndNotSignalCancellable(t *testing.T) {
	ctx, cancel := operatorModeContext(modeInspect)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the operator modes must not run on an unbounded context")
	}
	if remaining := time.Until(deadline); remaining > operatorModeTimeout {
		t.Fatalf("deadline is %v out, want at most operatorModeTimeout (%v)", remaining, operatorModeTimeout)
	}
	// Independent of any signal handling: it derives from Background, so nothing the run's own context does
	// can cancel a half-applied break out from under it.
	if err := ctx.Err(); err != nil {
		t.Fatalf("a fresh operator-mode context must be live, got %v", err)
	}
}

// The canary is the one mode whose legitimate work is minutes long: it starts two containers, waits out a
// grace period and reads what happened. On the recovery modes' one-minute bound it would be cut off
// mid-probe, every time, leaving two Pods holding its finalizer and no verdict — and the failure would look
// like a cluster that could not stop a Pod rather than like a timeout.
//
// Mutation that turns this red: drop the mode parameter and give every mode operatorModeTimeout again.
func TestTheCanaryModeGetsABudgetItsOwnWorkFitsIn(t *testing.T) {
	ctx, cancel := operatorModeContext(modeTerminationCanary)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the canary must not run on an unbounded context either")
	}
	// The bound has to exceed what the mode's own budgets can legitimately spend, or the context becomes the
	// thing that decides — and a context expiry cannot say which probe was still running.
	if remaining := time.Until(deadline); remaining <= canaryStartBudget+canaryStopBudgetMax {
		t.Fatalf("the canary's context expires %v out, which is inside its own start (%v) plus stop (%v) "+
			"budgets: the loop that can report why it gave up would never get to",
			remaining, canaryStartBudget, canaryStopBudgetMax)
	}
}

// heldWithResidue builds a node this transaction holds, carrying a residue record.
func heldWithResidue(t *testing.T) (journal, *corev1.Node) {
	t.Helper()
	j := journal{
		Schema: journalSchema, TxID: "tx-1", RunID: "r7", Arm: "reclaim-on",
		Kind: ownerRun, Study: "reclaim", Variant: "reclaim-on", Namespace: "queuelab-r7",
		Node: "platform-worker", NodeUID: "uid-node", TakenAt: "t0",
		Installed: installedTuple{LabelValue: "r7", TaintValue: "r7", TaintEffect: corev1.TaintEffectNoSchedule},
	}
	jraw, err := encodeJournal(j)
	if err != nil {
		t.Fatalf("encode journal: %v", err)
	}
	rraw, err := encodeResidue(residueRecord{
		Schema: residueSchema, TxID: "tx-1", RunID: "r7", LeftAt: "2026-08-13T01:07:29Z",
		Left: []residueLeft{{Kind: "Namespace", Name: "queuelab-r7", Absence: "present"}},
	})
	if err != nil {
		t.Fatalf("encode residue: %v", err)
	}
	// Explicit rather than ourTaint(): verifyInstalled compares this value against j.Installed.TaintValue.
	n := node(map[string]string{workerLabelKey: "r7"},
		map[string]string{journalKey: jraw, residueKey: rraw},
		corev1.Taint{Key: workerTaintKey, Value: "r7", Effect: corev1.TaintEffectNoSchedule})
	return j, n
}

func residueOn(t *testing.T, c client.Client) (string, bool) {
	t.Helper()
	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	raw, ok := got.Annotations[residueKey]
	return raw, ok
}

// The explanation goes with the thing it explains. A record surviving a release would be quoted by no
// refusal — nothing refuses a free node — and read by a human as current long after the worker it described
// was handed back.
func TestReleaseClearsTheResidueRecord(t *testing.T) {
	j, n := heldWithResidue(t)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	if _, err := releaseAcquired(context.Background(), fc, j); err != nil {
		t.Fatalf("release: %v", err)
	}
	if raw, ok := residueOn(t, fc); ok {
		t.Fatalf("the residue record survived the release as %q", raw)
	}
}

// -release-stale is the command inspectWorker tells an operator to run, and it deletes the residue record in
// the same patch that removes the markers. That makes its own output the last moment anyone can see what was
// left, so it has to say what it is about to destroy — otherwise the explanation this whole feature exists to
// deliver dies at the one step most likely to be copied without reading.
//
// The mutation that turns this red: delete the residue warning block from releaseStale, leaving only the
// "restoring node ..." line it had before.
func TestReleaseStaleWarnsBeforeItDeletesTheResidueRecord(t *testing.T) {
	_, n := heldWithResidue(t)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	var rerr error
	out := captureStdout(t, func() { rerr = releaseStale(context.Background(), fc, "platform-worker", "tx-1") })
	if rerr != nil {
		t.Fatalf("release-stale: %v", rerr)
	}
	// The name of the object matters more than the word "residue": an operator scanning this output is
	// looking for something they recognise as still standing on their cluster.
	for _, want := range []string{"residue", `"queuelab-r7"`, "does not delete those objects"} {
		if !strings.Contains(out, want) {
			t.Errorf("the release said nothing about %s before deleting the record; got:\n%s", want, out)
		}
	}
	if raw, ok := residueOn(t, fc); ok {
		t.Fatalf("the record survived the release as %q, so this test proved nothing about the warning", raw)
	}
}

// An unreadable record is still a record being destroyed, and silence about it is the same loss. It says so
// rather than printing the raw document, which decideAcquire's own unreadable branch already establishes as
// the right shape.
//
// The mutation that turns this red: drop the ResidueErr case from releaseStale's switch, which sends an
// unreadable record down the readable path and prints a zero-length object list.
func TestReleaseStaleWarnsAboutAResidueRecordItCannotRead(t *testing.T) {
	_, n := heldWithResidue(t)
	n.Annotations[residueKey] = "{not json"
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	var rerr error
	out := captureStdout(t, func() { rerr = releaseStale(context.Background(), fc, "platform-worker", "tx-1") })
	if rerr != nil {
		t.Fatalf("release-stale: %v", rerr)
	}
	if !strings.Contains(out, "could not be read") {
		t.Errorf("the release destroyed an unreadable residue record without saying so; got:\n%s", out)
	}
}

// forceQuarantine deliberately does NOT clear it. It is the explanation an operator most needs at the moment
// they are breaking a hold by hand, and it cannot mislead while it sits there: decideAcquire refuses a
// quarantined node on QuarantineRaw before it ever reaches the foreign-owner branch that quotes it.
func TestForceQuarantineLeavesTheResidueRecordInPlace(t *testing.T) {
	_, n := heldWithResidue(t)
	want := n.Annotations[residueKey]
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	if err := forceQuarantine(context.Background(), fc, "platform-worker", "uid-node"); err != nil {
		t.Fatalf("force: %v", err)
	}
	got, ok := residueOn(t, fc)
	if !ok || got != want {
		t.Fatalf("residue record is %q (present=%v), want it untouched; forcing is exactly when the operator "+
			"needs to know why the worker was held", got, ok)
	}
}

// clearQuarantine is the deliberate end of that state, so it is where the explanation ends too.
func TestClearQuarantineRemovesTheResidueRecord(t *testing.T) {
	_, n := heldWithResidue(t)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()
	if err := forceQuarantine(context.Background(), fc, "platform-worker", "uid-node"); err != nil {
		t.Fatalf("force: %v", err)
	}
	var forced corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &forced); err != nil {
		t.Fatalf("get node: %v", err)
	}
	q, err := decodeQuarantine(forced.Annotations[quarantineKey])
	if err != nil {
		t.Fatalf("decode quarantine: %v", err)
	}

	if err := clearQuarantine(context.Background(), fc, "platform-worker", q.QuarantineID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if raw, ok := residueOn(t, fc); ok {
		t.Fatalf("the residue record survived the quarantine being cleared as %q", raw)
	}
}

// The stamp goes on a node this transaction still holds, or nowhere. Between teardown and this write another
// actor can take the node over, and stamping our residue onto theirs is the same lie the UID preconditions
// in teardown_apply.go exist to prevent.
func TestStampResidueRefusesANodeThisTransactionNoLongerHolds(t *testing.T) {
	j := journal{
		Schema: journalSchema, TxID: "tx-1", RunID: "r7", Arm: "reclaim-on",
		Kind: ownerRun, Study: "reclaim", Variant: "reclaim-on", Namespace: "queuelab-r7",
		Node: "platform-worker", NodeUID: "uid-node", TakenAt: "t0",
		Installed: installedTuple{LabelValue: "r7", TaintValue: "r7", TaintEffect: corev1.TaintEffectNoSchedule},
	}
	// Someone else's markers, under our node's name.
	n := node(map[string]string{workerLabelKey: "r9"}, nil,
		corev1.Taint{Key: workerTaintKey, Value: "r9", Effect: corev1.TaintEffectNoSchedule})
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	left := []residue{{Observation: observation{Target: target{Kind: "Namespace", Name: "queuelab-r7"}},
		Absence: absencePresent}}
	if err := stampResidue(context.Background(), fc, j, left, "t", ""); err == nil {
		t.Fatal("stamped a node whose markers are another transaction's")
	}
	if raw, ok := residueOn(t, fc); ok {
		t.Fatalf("wrote %q onto a node this transaction no longer holds", raw)
	}
}

func TestStampResidueWritesTheRecordOnANodeWeStillHold(t *testing.T) {
	j, n := heldWithResidue(t)
	delete(n.Annotations, residueKey) // start clean; this test is about writing it
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	left := []residue{
		{Observation: observation{Target: target{Kind: "Namespace", Name: "queuelab-r7"}}, Absence: absencePresent},
		{Observation: observation{Target: target{Kind: "ResourceFlavor", Name: "queuelab-gpu-r7"}}, Absence: absenceUnknown},
	}
	if err := stampResidue(context.Background(), fc, j, left, "2026-08-13T01:07:29Z", "rec.json"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	raw, ok := residueOn(t, fc)
	if !ok {
		t.Fatal("no residue record was written")
	}
	rec, err := decodeResidue(raw)
	if err != nil {
		t.Fatalf("the record this code wrote cannot be read back by its own decoder: %v", err)
	}
	if rec.TxID != "tx-1" || rec.RunID != "r7" || rec.RecordPath != "rec.json" || len(rec.Left) != 2 {
		t.Fatalf("record is %+v, want tx-1/r7/rec.json with two entries", rec)
	}
	// Literal strings, not absenceName(absencePresent) / absenceName(absenceUnknown): comparing the
	// function's output against itself is circular and pins nothing — renaming a spelling changes both
	// sides at once and the test stays green. The persisted spellings are a wire format (see record.go's
	// absenceName), so what belongs here is what the record actually says on disk.
	if rec.Left[0].Absence != "present" || rec.Left[1].Absence != "unknown" {
		t.Fatalf("verdicts are %q/%q, want %q/%q: the node's residue record and the run record must spell "+
			"verdicts identically", rec.Left[0].Absence, rec.Left[1].Absence, "present", "unknown")
	}
}

// A record naming nothing explains nothing, and decodeResidue already refuses to read one back — so writing
// it would put a document on the node that residueNote can only report as unreadable, which is strictly worse
// than the bare refusal an operator would otherwise get. No caller reaches it today — tearDownBeforeRelease's
// one hold with an empty residue returns before the stamp branch, and the surviving call site is past a
// `len(result.Residue) == 0` return — so this pins defense in depth rather than a live path.
func TestStampResidueWritesNoRecordThatNamesNothing(t *testing.T) {
	j, n := heldWithResidue(t)
	delete(n.Annotations, residueKey)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	if err := stampResidue(context.Background(), fc, j, nil, "t", "rec.json"); err == nil {
		t.Fatal("stamped a record with nothing in it")
	}
	if raw, ok := residueOn(t, fc); ok {
		t.Fatalf("wrote %q for a residue that names nothing; decodeResidue refuses such a record, so it would "+
			"reach the next operator only as \"a residue record that could not be read\"", raw)
	}
}

// verifyInstalled cannot tell this transaction's node from one another transaction re-acquired under the same
// run id, and stampResidue's own doc comment promises exactly that it can.
//
// Both installed values ARE the run id, so the label, the taint and the node UID are byte-identical across the
// two — only the journal's txID differs. A reused run id is the confound newTxID exists to defeat, and
// verifyClean's comment already names this same value coincidence as the reason the journal is the authority.
// Stamping through it writes our residue onto somebody else's hold, so the next operator is refused with an
// explanation belonging to a run that is not the one holding the node — the lie the UID preconditions in
// teardown_apply.go exist to prevent, arriving by the one door they do not cover.
//
// Mutation: put verifyInstalled back in place of verifyObserved in stampResidue, and the stamp lands on the
// foreign transaction's node.
func TestStampResidueRefusesANodeReacquiredByAnotherTransactionUnderTheSameRunID(t *testing.T) {
	ours, n := heldWithResidue(t)
	delete(n.Annotations, residueKey)
	// Somebody else acquired it between our teardown and this write, under the same run id. Everything
	// verifyInstalled compares is unchanged, because everything it compares is derived from that run id.
	theirs := ours
	theirs.TxID = "tx-2"
	jraw, err := encodeJournal(theirs)
	if err != nil {
		t.Fatalf("encode journal: %v", err)
	}
	n.Annotations[journalKey] = jraw
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	// The markers really are indistinguishable: if this were false the test would be proving something else.
	if err := verifyInstalled(observe(n), ours); err != nil {
		t.Fatalf("the fixture's markers already diverge (%v), so this would pass without the journal check", err)
	}

	left := []residue{{Observation: observation{Target: target{Kind: "Namespace", Name: "queuelab-r7"}},
		Absence: absencePresent}}
	if err := stampResidue(context.Background(), fc, ours, left, "t", "rec.json"); err == nil {
		t.Fatal("stamped a node whose journal names another transaction")
	}
	if raw, ok := residueOn(t, fc); ok {
		t.Fatalf("wrote %q onto a hold belonging to tx-2; the next operator would read our run's explanation "+
			"for somebody else's worker", raw)
	}
}

// inspectWorker is where residueNote's own last line sends the operator, and until now it was the one reader
// of this node that never mentioned the record — while advising the single command that must not be run.
//
// -release-stale on a residue hold strips the dedication label and the NoSchedule taint from a node whose
// namespace may still be running that run's GPU Pods, and deletes the record on the way past. The advice is
// not removed, because the operator does eventually need it; what changes is the condition attached to it.
//
// Mutations: drop the printResidueDetail call in inspectWorker and the objects are never named; collapse the
// held branch's switch to its default and the unconditional "If that process is gone" line comes back.
func TestInspectWorkerNamesAResidueHoldAndQualifiesTheReleaseAdvice(t *testing.T) {
	// The record is node-controlled like everything else this tool prints, and it is printed a few lines above
	// a command the operator is invited to copy.
	payload := "\x1b[2K\x1b[32m queuelabrun -force-release\a"
	_, n := heldWithResidue(t)
	rraw, err := encodeResidue(residueRecord{
		Schema: residueSchema, TxID: "tx-1", RunID: "r7", LeftAt: "2026-08-13T01:07:29Z",
		RecordPath: "queuelabrun-record.json",
		Left: []residueLeft{{
			Kind: "Namespace", Name: "queuelab-r7" + payload, Absence: "present",
		}},
	})
	if err != nil {
		t.Fatalf("encode residue: %v", err)
	}
	n.Annotations[residueKey] = rraw
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	var ierr error
	out := captureStdout(t, func() { ierr = inspectWorker(context.Background(), fc, "platform-worker") })
	// Still not an error: this record decides nothing, and the run path does not refuse differently for it
	// either. A diagnostic that started exiting non-zero on a deliberate hold would break every script that
	// reads the code to tell a healthy worker from a broken one.
	if ierr != nil {
		t.Fatalf("inspecting a held node with a residue record must succeed: %v", ierr)
	}
	if !strings.Contains(out, "HELD by run") {
		t.Fatalf("the held branch did not run:\n%s", out)
	}
	// The record is surfaced, decoded, not merely dumped as JSON.
	if !strings.Contains(out, "left by run") || !strings.Contains(out, "queuelab-r7") {
		t.Fatalf("the residue record was not surfaced; an operator cannot see what is still standing:\n%s", out)
	}
	if !strings.Contains(out, "full record:") {
		t.Fatalf("the record path was not printed:\n%s", out)
	}
	// The wrong advice, verbatim as it stood before this: an unconditional release of a node that is held on
	// purpose.
	if strings.Contains(out, "If that process is gone") {
		t.Fatalf("a residue hold was advised as if it were a crashed process's leftovers:\n%s", out)
	}
	if !strings.Contains(out, "DELIBERATE") || !strings.Contains(out, "do NOT strip") {
		t.Fatalf("the hold was not named as deliberate, or the finalizer warning is missing:\n%s", out)
	}
	// The release command is still offered, under the condition that makes it safe.
	if !strings.Contains(out, "Once those objects are gone") ||
		!strings.Contains(out, "-release-stale -worker platform-worker") {
		t.Fatalf("the operator is left without a complete command for the state they will reach:\n%s", out)
	}
	for _, control := range []string{"\x1b", "\a"} {
		if strings.Contains(out, control) {
			t.Fatalf("control byte %q reached the terminal:\n%q", control, out)
		}
	}
	if !strings.Contains(out, `\x1b[2K`) {
		t.Fatalf("the escape sequence must remain visible, just inert:\n%s", out)
	}
}

// An unreadable record is not "no record". The annotation being there at all is evidence a teardown ended
// without removing everything, so the advice must not fall back to the unconditional release line with
// nothing on screen to warn against it.
//
// It stays a printed warning rather than a returned error, unlike the unreadable QUARANTINE record: that one
// is the state this tool has no remaining move for, while this document decides nothing and an informational
// field that invents a new failure mode is worse than no field at all — the same rule decideAcquire follows.
//
// Mutation: drop the obs.ResidueErr case from inspectWorker's held branch and "If that process is gone"
// returns; make the branch return a refusal and the exit-code assertion fails.
func TestInspectWorkerSaysSoWhenAResidueRecordCannotBeRead(t *testing.T) {
	_, n := heldWithResidue(t)
	n.Annotations[residueKey] = "{not json"
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).Build()

	var ierr error
	out := captureStdout(t, func() { ierr = inspectWorker(context.Background(), fc, "platform-worker") })
	if ierr != nil {
		t.Fatalf("an unreadable residue record must not become a failure mode of its own: %v", ierr)
	}
	if !strings.Contains(out, "UNREADABLE") || !strings.Contains(out, "could not read") {
		t.Fatalf("the tool stayed silent about a record it tried and failed to read:\n%s", out)
	}
	if strings.Contains(out, "If that process is gone") {
		t.Fatalf("a node carrying an unreadable residue record was advised as an ordinary stale hold:\n%s", out)
	}
	if !strings.Contains(out, "-release-stale -worker platform-worker") {
		t.Fatalf("the operator is left with no command at all:\n%s", out)
	}
}

// The non-conflict release arm must tell a free node from a stolen one, because its old argument does not
// hold and the two outcomes are opposite.
//
// That argument ran: a concurrent -release-stale or -force-release would have moved the resourceVersion, the
// optimistic lock would have conflicted, and we would be in the IsConflict arm — so a clean read-back here is
// our own write. It needs the patch to have REACHED the apiserver, which is the one thing this arm exists to
// doubt: it is written for a proxy timeout or a connection reset, and a request that never landed had no
// opportunity to conflict. The absence of a 409 therefore proves nothing about who removed our markers.
//
// Both halves are asserted together because each one alone is satisfied by breaking the other. Refusing
// whenever the patch errors would reinstate the false invalidation this arm was written to remove — the lab
// pays for those in runs. Accepting whenever the read-back is clean is what let a run report a tidy release
// over a worker another transaction had taken mid-run.
//
// Mutation that turns this red: drop the obs.JournalRaw check and return releaseRestore unconditionally.
func TestReleaseOnANonConflictFailureSeparatesAFreeNodeFromAStolenOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stolenBy   string
		wantRefuse bool
	}{
		{"nobody holds it, so restoration is real however the patch fared", "", false},
		{"another transaction holds it and our patch may never have landed", "tx-thief", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failRelease := false
			fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node(nil, nil)).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch,
						opts ...client.PatchOption) error {
						if !failRelease {
							return c.Patch(ctx, obj, p, opts...)
						}
						// A write whose fate the caller cannot know: it sees a transport error, while the node
						// independently reaches the state under test.
						var cur corev1.Node
						if err := c.Get(ctx, client.ObjectKey{Name: "platform-worker"}, &cur); err != nil {
							return err
						}
						delete(cur.Labels, workerLabelKey)
						delete(cur.Annotations, journalKey)
						cur.Spec.Taints = nil
						if tc.stolenBy != "" {
							// A READABLE foreign journal, because verifyClean already fails closed on an
							// unreadable one and would never reach the branch under test. This is the state a
							// real -force-release plus a fresh acquire leaves behind.
							thief, jerr := encodeJournal(journal{
								Schema: journalSchema, TxID: tc.stolenBy, RunID: "r-thief", Arm: "A-honor",
								Kind: ownerRun, Study: "reclaim", Variant: "reclaim-on", Namespace: "queuelab-r-thief",
								Node: "platform-worker", NodeUID: string(cur.UID), TakenAt: "2026-08-15T00:00:00Z",
								Installed: installedTuple{
									LabelValue: tc.stolenBy, TaintValue: tc.stolenBy,
									TaintEffect: corev1.TaintEffectNoSchedule,
								},
							})
							if jerr != nil {
								return jerr
							}
							cur.Labels[workerLabelKey] = tc.stolenBy
							cur.Annotations[journalKey] = thief
						}
						if err := c.Update(ctx, &cur); err != nil {
							return err
						}
						return errors.New("net/http: request canceled while awaiting headers")
					},
				}).Build()

			j, err := acquireWorker(context.Background(), fc, "platform-worker", acquisitionSeed("tx-ours", "r1", "A-honor"))
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			failRelease = true

			_, err = releaseAcquired(context.Background(), fc, j)
			var r *refusal
			gotRefuse := err != nil && asRefusal(err, &r) && r.Reason == reasonOwnershipLost
			if tc.wantRefuse && !gotRefuse {
				t.Fatalf("the node was taken by %s while this run's release may never have landed, and the run "+
					"was told nothing: err=%v", tc.stolenBy, err)
			}
			if !tc.wantRefuse && err != nil {
				t.Fatalf("the node ended free, so this is the false invalidation the arm exists to prevent: %v", err)
			}
			if tc.wantRefuse && !strings.Contains(err.Error(), j.TxID) {
				t.Fatalf("the invalidation must name the transaction that lost the worker: %v", err)
			}
		})
	}
}

// acquisitionSeed builds the seed acquisition now takes, so a spec that only cares about the transaction, run and
// arm does not have to spell out the recovery fields the journal carries.
func acquisitionSeed(txID, runID, arm string) ownerIdentity {
	return seed{
		Schema:    teardownSeedSchema,
		TxID:      txID,
		RunID:     runID,
		Arm:       arm,
		Study:     queuelab.StudyReclaim,
		Variant:   "reclaim-on",
		Namespace: "queuelab-" + runID,
	}.identity()
}
