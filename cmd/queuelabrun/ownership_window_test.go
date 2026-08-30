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
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// operatorTaint is the platform's own unhealthy taint, spelled exactly as internal/controller writes it.
//
// It stands here for benign drift, and it is the realistic case rather than an invented one: this
// repository's nodehealth controller taints a not-ready node while a run holds it, and a gate that refused
// that would invalidate runs for the platform doing its job.
func operatorTaint() corev1.Taint {
	return corev1.Taint{
		Key:    "platform.lkhun9311.github.io/unhealthy",
		Value:  "true",
		Effect: corev1.TaintEffectNoSchedule,
	}
}

// heldWorker is the worker as it stands the moment the window opens: this transaction's label, its taint and
// its journal, at a resource version, because RetryWatcher aborts on an object that carries none.
func heldWorker(t *testing.T, rv string, taints ...corev1.Taint) *corev1.Node {
	t.Helper()
	j := testJournal()
	raw, err := encodeJournal(j)
	if err != nil {
		t.Fatalf("encode journal: %v", err)
	}
	n := node(map[string]string{workerLabelKey: j.Installed.LabelValue},
		map[string]string{journalKey: raw},
		append([]corev1.Taint{ourTaint()}, taints...)...)
	n.ResourceVersion = rv
	return n
}

// sentinelOnScript opens a real ownership window whose baseline and opening read come from a fake cluster
// but whose watches come from a script.
//
// The List stamp is the same double the collector's stream tests need and for the same reason: the fake
// client's tracker returns lists with an empty resource version, which is precisely the unresumable point
// the component refuses, so without it no test here gets as far as a watch.
func sentinelOnScript(ctx context.Context, t *testing.T, seed *corev1.Node,
	calls ...func(metav1.ListOptions) (watch.Interface, error)) (*ownershipSentinel, *scriptedWatcher) {
	t.Helper()
	s := &scriptedWatcher{calls: calls}
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(seed).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := cl.List(ctx, list, opts...); err != nil {
					return err
				}
				list.SetResourceVersion(baselineRV)
				return nil
			},
			Watch: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
				lo := &client.ListOptions{}
				lo.ApplyOptions(opts)
				raw := metav1.ListOptions{}
				if lo.Raw != nil {
					raw = *lo.Raw
				}
				return s.WatchWithContext(ctx, raw)
			},
		}).Build()

	sent, err := startOwnershipSentinel(ctx, c, testJournal())
	if err != nil {
		t.Fatalf("start ownership sentinel: %v", err)
	}
	return sent, s
}

// feed is a watch that delivers a fixed script of Node versions and then holds open.
func feed(versions ...*corev1.Node) func(metav1.ListOptions) (watch.Interface, error) {
	return func(metav1.ListOptions) (watch.Interface, error) {
		w := watch.NewFakeWithChanSize(len(versions), false)
		for _, v := range versions {
			w.Modify(v)
		}
		return w, nil
	}
}

// awaitVersions blocks until the sentinel has folded n Node versions, so an assertion never races the
// consumer goroutine that is still delivering them.
func awaitVersions(t *testing.T, s *ownershipSentinel, n int) {
	t.Helper()
	waitFor(t, func() bool { return s.Window().NodeVersionsObserved >= n })
}

// The whole gate, in the shape the hole actually takes: a third party strips the NoSchedule taint, other
// work lands, and the taint is back before release. Both endpoint checks — verifyAcquired at the start and
// decideRelease at the end — see a correct tuple and pass, so nothing before this saw anything at all.
//
// Mutation that turns this red: delete the `err := verifyInstalled(obs, s.j)` comparison from
// ownershipSentinel.observe (or make it return before recording). The window then counts node versions,
// reports the hold as unbroken, and the run publishes a number measured on a shared machine.
func TestOwnershipWindowSeesATaintStrippedAndRestored(t *testing.T) {
	ctx := t.Context()

	stripped := heldWorker(t, "1001")
	stripped.Spec.Taints = nil
	restored := heldWorker(t, "1002")
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"), feed(stripped, restored))
	defer s.Close()

	// Three: the opening read plus the two versions the script delivers.
	awaitVersions(t, s, 3)
	s.Close()

	w := s.Window()
	if w.ViolationsObserved != 1 {
		t.Fatalf("a taint stripped and restored mid-run produced %d violation(s), want 1: %+v",
			w.ViolationsObserved, w.Violations)
	}
	if w.Violations[0].Reason != reasonInstalledDiverged {
		t.Fatalf("the stripped taint was classified %q, want %q; a reason the record cannot classify sends the "+
			"operator looking for the wrong thing", w.Violations[0].Reason, reasonInstalledDiverged)
	}
	// The invalidation sentence is what reaches the ledger, so an empty one would leave the violation recorded
	// and the run publishable — the exact split between evidence and enforcement this gate exists to close.
	if !strings.Contains(w.invalidation(), reasonInstalledDiverged) {
		t.Fatalf("the invalidation %q does not name what went wrong", w.invalidation())
	}
}

// Benign drift must stay benign. The platform's own nodehealth controller taints a not-ready worker while a
// run holds it, and an unrelated label can be written by anything; neither is this run's marker, and a gate
// that refused them would invalidate runs for the cluster behaving normally.
//
// Mutation that turns this red: widen the comparison in ownershipSentinel.observe to refuse taints this run
// did not install, e.g. by following verifyInstalled with
// `if err == nil && len(obs.AllTaints) != len(obs.Taints) { err = refuse(reasonInstalledDiverged, "…") }`.
// Every run on a node the operator's own controller touches is then invalid.
func TestOwnershipWindowIgnoresUnrelatedLabelsAndTaints(t *testing.T) {
	ctx := t.Context()

	drifted := heldWorker(t, "1001", operatorTaint())
	drifted.Labels["someone-elses/label"] = "true"
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"), feed(drifted))
	defer s.Close()

	awaitVersions(t, s, 2)
	s.Close()

	w := s.Window()
	if w.ViolationsObserved != 0 {
		t.Fatalf("benign drift invalidated the run: %+v", w.Violations)
	}
	if w.invalidation() != "" {
		t.Fatalf("benign drift produced an invalidation: %q", w.invalidation())
	}
}

// A Node deleted and recreated under the same name is a different machine, and the markers on the recreated
// one can be byte-identical: only the UID says so. verifyInstalled already makes that comparison, which is
// why this file makes it by calling verifyInstalled rather than by writing a second rule beside it.
//
// Mutation that turns this red: in ownershipSentinel.observe, blank the journal's NodeUID before comparing
// (`j := s.j; j.NodeUID = obs.NodeUID; verifyInstalled(obs, j)`) — a plausible-looking simplification that
// keeps every marker check and silently stops noticing that the machine changed underneath the run.
func TestOwnershipWindowSeesTheWorkerReplacedUnderTheSameName(t *testing.T) {
	ctx := t.Context()

	recreated := heldWorker(t, "1001")
	recreated.UID = types.UID("uid-a-different-machine")
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"), feed(recreated))
	defer s.Close()

	awaitVersions(t, s, 2)
	s.Close()

	w := s.Window()
	if w.ViolationsObserved != 1 {
		t.Fatalf("a replaced node produced %d violation(s), want 1: %+v", w.ViolationsObserved, w.Violations)
	}
	if w.Violations[0].Reason != reasonWrongNode {
		t.Fatalf("a replaced node was classified %q, want %q", w.Violations[0].Reason, reasonWrongNode)
	}
}

// A deleted worker is the case where every marker check passes and the claim is still gone: the Deleted
// event carries the object's last state, markers and all, so verifyInstalled has nothing to object to.
//
// Mutation that turns this red: delete the `if et == watch.Deleted` block from ownershipSentinel.observe.
// The deletion then folds as an ordinary version, passes, and the window reports an unbroken hold on a node
// that no longer exists.
func TestOwnershipWindowSeesTheWorkerDeleted(t *testing.T) {
	ctx := t.Context()

	gone := heldWorker(t, "1001")
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"),
		func(metav1.ListOptions) (watch.Interface, error) {
			w := watch.NewFakeWithChanSize(1, false)
			w.Delete(gone)
			return w, nil
		})
	defer s.Close()

	awaitVersions(t, s, 2)
	s.Close()

	w := s.Window()
	if w.ViolationsObserved != 1 || w.Violations[0].Reason != reasonWorkerDeleted {
		t.Fatalf("a deleted worker produced %+v, want one %s violation", w.Violations, reasonWorkerDeleted)
	}
}

// The view is cluster-wide, because the Node kind is (see clusterScope), so most of what arrives belongs to
// other machines — and another node carries none of this transaction's markers, so folding it in would make
// every run on a cluster with more than one node invalid.
//
// Mutation that turns this red: delete the `n.Name != s.j.Node` guard from ownershipSentinel.consume. The
// other node's version — which carries none of this transaction's markers — then invalidates every run on a
// cluster with more than one worker.
//
// The bookmark in the script pins nothing about this package and is here as documentation of why there is no
// bookmark case in consume: RetryWatcher consumes bookmarks to advance its resume version and never forwards
// them (client-go v0.36.1, tools/watch/retrywatcher.go), so one cannot reach the consumer at all. An earlier
// revision of this file guarded against them and claimed the worker-name guard as its backstop; the guard is
// never reached either, and the review that measured it is why the arm is gone. This test cannot say more
// than that: running against this client-go, it cannot tell a bookmark swallowed inside RetryWatcher from one
// forwarded and dropped by the name guard, so it asserts only that feeding one produces no violation.
func TestOwnershipWindowIgnoresOtherNodesAndBookmarks(t *testing.T) {
	ctx := t.Context()

	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "platform-worker2", UID: types.UID("uid-other"), ResourceVersion: "1001"}}
	bookmark := &corev1.Node{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1002"}}
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"),
		func(metav1.ListOptions) (watch.Interface, error) {
			w := watch.NewFakeWithChanSize(3, false)
			w.Modify(other)
			w.Action(watch.Bookmark, bookmark)
			// A version of the real worker follows both, so the assertion below cannot pass merely because
			// nothing was ever delivered.
			w.Modify(heldWorker(t, "1003"))
			return w, nil
		})
	defer s.Close()

	awaitVersions(t, s, 2)
	s.Close()

	w := s.Window()
	if w.ViolationsObserved != 0 {
		t.Fatalf("another node's version or a bookmark was read as this worker deviating: %+v", w.Violations)
	}
	if w.NodeVersionsObserved != 2 {
		t.Fatalf("the window folded %d versions, want 2 (the opening read and the worker's own event); "+
			"anything else means it is counting objects it must not compare", w.NodeVersionsObserved)
	}
}

// A view that ends before the run closes it stops being evidence at that point: nothing was watching
// afterwards, so exclusivity for the rest of the window is unobserved rather than established. That is a
// violation and not a warning, for the same reason the collector refuses a stream that ended on its own.
//
// The two endings are separate cases because each pins one arm and NEITHER pins the other, which is a trap
// worth naming: a forwarded 410 also has both caller flags clear, so deleting the status arm alone leaves it
// caught by the other one and a test asserting only "some violation, ending reads as lost" would stay green
// under the mutation its own comment claimed. So the status case asserts the text only statusText produces,
// and the silent case is one no status arm could ever catch.
//
// Mutations that turn this red, one per case: delete the `case end.LastStatus != nil` arm (the 410 then ends
// up under the generic wording and the operator is never told a status was seen), or delete the
// `case !end.Cancelled && !end.Stopped` arm (a view that died silently then reports an unbroken hold).
func TestOwnershipWindowInvalidatesWhenTheViewEndsOnItsOwn(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "",
		errors.New("no watch permission"))
	// Every case here establishes first and dies afterwards, because a view that never attaches at all is
	// refused up front now and is the test below rather than this one.
	established := func(metav1.ListOptions) (watch.Interface, error) {
		w := watch.NewFake()
		go w.Stop()
		return w, nil
	}
	for name, tc := range map[string]struct {
		open      []func(metav1.ListOptions) (watch.Interface, error)
		wantInEnd string
	}{
		// A 410 says the resume version aged out: the gap can no longer be closed, and the status is the one
		// piece of evidence about why.
		"a forwarded 410": {
			open: []func(metav1.ListOptions) (watch.Interface, error){
				// The 410 arrives on the RECONNECT rather than on the first watch, and that is not decoration:
				// delivered on the first, the stream establishes and dies in the same instant, and the
				// establishment select in startOwnershipSentinel can legitimately take either arm — this test
				// failed exactly that way under -race. Both arms refuse the run, so the ambiguity is safe in
				// production and merely unusable in a test that wants one of them.
				established,
				func(metav1.ListOptions) (watch.Interface, error) {
					gone := watch.NewFakeWithChanSize(1, false)
					gone.Error(&metav1.Status{
						Status: metav1.StatusFailure, Code: http.StatusGone,
						Reason: metav1.StatusReasonExpired, Message: "too old resource version",
					})
					return gone, nil
				},
			},
			wantInEnd: "terminal watch error",
		},
		// A wrapped Forbidden terminates permanently while forwarding nothing at all, which is the ending with
		// no evidence in it — and continuity is just as lost. It arrives on the RECONNECT here, after a watch
		// that did establish, which is the shape a credential revoked mid-run takes.
		"a permanent failure that forwards nothing": {
			open: []func(metav1.ListOptions) (watch.Interface, error){
				established,
				func(metav1.ListOptions) (watch.Interface, error) {
					return nil, fmt.Errorf("watch nodes: %w", forbidden)
				},
			},
			wantInEnd: "forwarded no status",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()

			s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"), tc.open...)
			defer s.Close()

			waitFor(t, func() bool { return s.Window().ViolationsObserved > 0 })
			w := s.Window()
			if w.Violations[0].Reason != reasonWindowLost {
				t.Fatalf("a view that ended on its own was classified %q, want %q",
					w.Violations[0].Reason, reasonWindowLost)
			}
			if !strings.Contains(w.Ending, "lost") || !strings.Contains(w.Ending, tc.wantInEnd) {
				t.Fatalf("the ending %q does not report this loss as what it was (%q)", w.Ending, tc.wantInEnd)
			}
		})
	}
}

// The other half of the rule above, and the reason it needs stating separately: the run itself closes this
// view on every path, so an implementation that treated every ending as a loss would invalidate every run
// including the ones that held their worker throughout.
//
// Mutation that turns this red: make ownershipSentinel.consume record reasonWindowLost for any ending rather
// than only for the two the caller did not cause.
func TestOwnershipWindowClosedByTheRunIsNotAViolation(t *testing.T) {
	ctx := t.Context()

	held := watch.NewFake()
	defer held.Stop()
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"),
		func(metav1.ListOptions) (watch.Interface, error) { return held, nil })

	s.Close()
	w := s.Window()
	if w.ViolationsObserved != 0 {
		t.Fatalf("the run closing its own window was read as losing it: %+v", w.Violations)
	}
	if w.Ending != "closed by the run" {
		t.Fatalf("ending %q, want the orderly close", w.Ending)
	}
	if w.ClosedAt == "" {
		t.Fatal("a closed window must say when, or the record cannot show what interval it covers")
	}
}

// The window has to open from a baseline the run's own writes come after, and the opening read is what makes
// the first stretch of it an observation rather than an assumption.
//
// Mutation that turns this red: delete the `s.observe(&n, watch.Modified)` opening read in
// startOwnershipSentinel. NodeVersionsObserved is then 0 on a healthy run, which is a window that compared
// nothing reporting that nothing deviated.
func TestOwnershipWindowOpensFromTheBaselineAndReadsTheWorkerOnce(t *testing.T) {
	ctx := t.Context()

	held := watch.NewFake()
	defer held.Stop()
	s, script := sentinelOnScript(ctx, t, heldWorker(t, "1000"),
		func(metav1.ListOptions) (watch.Interface, error) { return held, nil })
	defer s.Close()

	w := s.Window()
	if w.BaselineResourceVersion != baselineRV {
		t.Fatalf("the window opened at %q, want the list's own %q", w.BaselineResourceVersion, baselineRV)
	}
	if w.NodeVersionsObserved != 1 {
		t.Fatalf("the opening read folded %d versions, want 1", w.NodeVersionsObserved)
	}
	if w.Node != "platform-worker" || w.TxID != testJournal().TxID {
		t.Fatalf("the window names node %q under tx %q, which is not what the journal says", w.Node, w.TxID)
	}
	// The watch has to resume from that same version, or the window describes a point the view never
	// attached at and the gap between them is invisible.
	waitFor(t, func() bool { script.mu.Lock(); defer script.mu.Unlock(); return len(script.seen) > 0 })
	script.mu.Lock()
	first := script.seen[0]
	script.mu.Unlock()
	if first != baselineRV {
		t.Fatalf("the view resumed from %q but the window claims %q", first, baselineRV)
	}
}

// A worker this run cannot read at the moment it starts measuring is a premise it has not established, and
// an unopened window must refuse the run rather than be noted and stepped over — which is the shape of the
// substitution (absence of evidence for evidence of absence) the whole gate exists to refuse.
//
// Mutation that turns this red: in startOwnershipSentinel, log the Get failure and return the sentinel with
// a nil error.
func TestStartOwnershipSentinelRefusesAWorkerItCannotRead(t *testing.T) {
	ctx := t.Context()

	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(heldWorker(t, "1000")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := cl.List(ctx, list, opts...); err != nil {
					return err
				}
				list.SetResourceVersion(baselineRV)
				return nil
			},
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "platform-worker",
					errors.New("no get permission"))
			},
			Watch: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
				return watch.NewFake(), nil
			},
		}).Build()

	// The call must also RETURN: Close joins the consumer goroutine, so a sentinel that shut down before
	// launching it would hang here rather than refuse, which is why this test has no timeout of its own to
	// hide behind.
	done := make(chan error, 1)
	go func() {
		_, err := startOwnershipSentinel(ctx, c, testJournal())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a worker whose state could not be read opened a window; the run would then measure under a " +
				"premise nothing established")
		}
		if !strings.Contains(err.Error(), "platform-worker") {
			t.Fatalf("the refusal %q does not name the node it could not read", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startOwnershipSentinel hung on the refusal path; Close must be able to join a consumer that " +
			"was started before the read that fails")
	}
}

// A view that never attaches has to refuse the run in its first second, not at the horizon. startWatchStream
// returns a nil error before its first watch has necessarily succeeded, so without this the run would submit
// its trace, spend its whole window and then refuse — over an RBAC gap that was knowable before anything was
// created. This is also the most likely way this gate fails on a real cluster: a Node watch is the only
// cluster-scoped watch the runner opens, so it is the one a credential is most likely to lack.
//
// Mutation that turns this red: delete the `select` on stream.Established()/End() from
// startOwnershipSentinel. The refusal then arrives at the end of the run instead of before it, as a lost
// window rather than as an unopened one, and this test's own call returns a healthy sentinel.
func TestStartOwnershipSentinelRefusesAViewThatNeverAttaches(t *testing.T) {
	ctx := t.Context()

	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "",
		errors.New("no watch permission"))
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(heldWorker(t, "1000")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := cl.List(ctx, list, opts...); err != nil {
					return err
				}
				list.SetResourceVersion(baselineRV)
				return nil
			},
			Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
				return nil, fmt.Errorf("watch nodes: %w", forbidden)
			},
		}).Build()

	start := time.Now()
	_, err := startOwnershipSentinel(ctx, c, testJournal())
	if err == nil {
		t.Fatal("a view that never established a watch opened a window; the run would spend its whole horizon " +
			"before discovering it was never being watched")
	}
	if !strings.Contains(err.Error(), "platform-worker") {
		t.Fatalf("the refusal %q does not name the worker it could not watch", err)
	}
	// The permanent-failure path must not sit out the establishment budget: it is a state no waiting can
	// change, and a run that took fifteen seconds to report it would be reporting it fifteen seconds late.
	if took := time.Since(start); took > establishBudget {
		t.Fatalf("the refusal took %s, which is the whole budget; a permanently failing watch is recognisable "+
			"at once and the End() arm is what recognises it", took)
	}
}

// The violation list is capped and the count is not, so a node being fought over cannot bury the record —
// and cannot lose the fact either. The first violations are the ones kept, because the first is where the
// claim ended and everything after it happened to a run that was already invalid.
//
// Mutation that turns this red: in recordLocked, append unconditionally (the record then grows without
// bound) or drop the ViolationsObserved counter (the count then agrees with the truncated list and the
// record understates what happened).
func TestOwnershipWindowCapsTheDetailAndNeverTheCount(t *testing.T) {
	ctx := t.Context()

	versions := make([]*corev1.Node, 0, windowViolationCap+5)
	for i := range windowViolationCap + 5 {
		bad := heldWorker(t, fmt.Sprintf("2%03d", i))
		bad.Spec.Taints = nil
		versions = append(versions, bad)
	}
	s, _ := sentinelOnScript(ctx, t, heldWorker(t, "1000"), feed(versions...))
	defer s.Close()

	awaitVersions(t, s, len(versions)+1)
	s.Close()

	w := s.Window()
	if w.ViolationsObserved != len(versions) {
		t.Fatalf("the window counted %d violations, want %d; the cap must lose detail and never the fact",
			w.ViolationsObserved, len(versions))
	}
	if len(w.Violations) != windowViolationCap {
		t.Fatalf("the window kept %d violations, want the cap %d", len(w.Violations), windowViolationCap)
	}
}

// ---- the restoration audit ----

// releasableWorker is a fake cluster holding the worker this transaction acquired, with an unrelated taint
// on it so the audit has something to be wrong about.
func releasableWorker(t *testing.T, extra ...corev1.Taint) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(heldWorker(t, "1000", extra...)).Build()
}

// The audit is what makes a restoration an audit rather than an assertion: the release already proves its
// own markers came off, and what was missing is any record of what the Node looked like on either side.
//
// Mutation that turns this red: delete the `audit.Before = before` assignment (or the read that fills it) in
// auditedRelease. The record then carries an "after" with nothing to compare it against, which is the state
// this gate was written to replace.
func TestAuditedReleaseRecordsBothSidesOfTheRelease(t *testing.T) {
	c := releasableWorker(t, operatorTaint())

	audit, err := auditedRelease(context.Background(), c, testJournal())
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if audit.Unavailable != "" {
		t.Fatalf("both sides were readable, yet the audit reports %q", audit.Unavailable)
	}
	if !audit.Before.Observed || !audit.Before.HasLabel || !audit.Before.HasJournal {
		t.Fatalf("the before side does not describe a held worker: %+v", audit.Before)
	}
	if len(audit.Before.OwnershipTaintValues) != 1 {
		t.Fatalf("the before side carries %v ownership taint value(s), want this run's one",
			audit.Before.OwnershipTaintValues)
	}
	if !audit.After.Observed || audit.After.HasLabel || audit.After.HasJournal ||
		len(audit.After.OwnershipTaintValues) != 0 {
		t.Fatalf("the after side does not describe a released worker: %+v", audit.After)
	}
	if !audit.OurMarkersRemoved {
		t.Fatalf("a clean release was not read as one: %+v", audit)
	}
	// The operator's own taint was there before and must be there after: the release patch replaces
	// spec.taints wholesale, so this is the property that keeps restoring our marker from deleting theirs.
	if len(audit.Drift) != 0 {
		t.Fatalf("an unrelated taint did not survive the release: %v", audit.Drift)
	}
	if len(audit.After.OtherTaintKeys) != 1 || audit.After.OtherTaintKeys[0] != operatorTaint().Key {
		t.Fatalf("the after side lost the operator's taint: %+v", audit.After)
	}
}

// An unrelated taint that does not survive the release is damage to somebody else's cluster state, and
// nothing else is watching for it: the release verifies its OWN markers came off and would report a clean
// restoration over a node it had just stripped.
//
// Mutation that turns this red: delete the Drift loop from restorationAudit.summarise.
func TestRestorationAuditNamesAnUnrelatedTaintThatDidNotSurvive(t *testing.T) {
	inner := releasableWorker(t, operatorTaint())
	// A patch that drops every taint is exactly what a wholesale spec.taints replacement gone wrong looks
	// like from the apiserver's side.
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if n, ok := obj.(*corev1.Node); ok {
				n.Spec.Taints = nil
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})

	audit, err := auditedRelease(context.Background(), c, testJournal())
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(audit.Drift) != 1 || audit.Drift[0] != operatorTaint().Key {
		t.Fatalf("the audit reports drift %v, want the operator's taint; a release that deletes somebody "+
			"else's taint would otherwise be recorded as clean", audit.Drift)
	}
}

// A side that could not be read must never persist as a side that was clean, which is the same substitution
// this whole gate refuses, applied to the audit itself.
//
// Mutation that turns this red: delete BOTH the `if !a.After.Observed` guard and the NodeUID comparison
// after it from restorationAudit.summarise. Either one alone leaves the other catching this — an unread side
// has an empty UID, which the journal's never matches — and naming just one would credit a guard with the
// other's work, which is the trap the ending test above walks into deliberately. Measured, not assumed: with
// only the Observed guard deleted this test stays green.
func TestRestorationAuditWillNotCallAnUnreadNodeRestored(t *testing.T) {
	inner := releasableWorker(t)
	var patched bool
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patched = true
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			// Only the reads AFTER the release fail, so the release itself still runs and still verifies.
			if patched {
				return apierrors.NewInternalError(errors.New("apiserver unreachable"))
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})

	audit, relErr := auditedRelease(context.Background(), c, testJournal())
	if audit.OurMarkersRemoved {
		t.Fatal("the audit called a Node it never read restored")
	}
	if audit.Unavailable == "" {
		t.Fatal("a side that could not be read must say so, or the record cannot be told from a clean one")
	}
	// The release's own verdict is what decides the run; the audit adds evidence and must not manufacture a
	// failure of its own.
	if relErr == nil {
		t.Log("the release itself verified through the same failing reads; that is its own business, not the audit's")
	}
}

// The audit reads the two sides for itself rather than copying the release's verdict, so a marker that
// survived has to show up here even though releaseOwned would have refused on its own.
//
// Mutation that turns this red: delete the taintGone loop from restorationAudit.summarise. A node still
// carrying this transaction's NoSchedule taint then reads as fully restored — the one state where the next
// run acquires a worker that only looks free.
func TestRestorationAuditWillNotCallASurvivingTaintRemoved(t *testing.T) {
	inner := releasableWorker(t)
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// The label and the journal come off, the taint does not: the shape a partially applied
			// restoration leaves behind.
			if n, ok := obj.(*corev1.Node); ok {
				n.Spec.Taints = []corev1.Taint{ourTaint()}
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})

	audit, _ := auditedRelease(context.Background(), c, testJournal())
	if audit.OurMarkersRemoved {
		t.Fatalf("a worker still carrying this transaction's taint was read as restored: %+v", audit.After)
	}
}

// reportRestoration is the operator's only sight of the audit, and it says nothing on a clean one because a
// line printed after every successful run is a line nobody reads on the run where it matters.
//
// Mutation that turns this red: print unconditionally in reportRestoration, or drop the Drift branch.
func TestReportRestorationSpeaksOnlyWhenThereIsSomethingToDo(t *testing.T) {
	var quiet strings.Builder
	reportRestoration(&quiet, "platform-worker", &restorationAudit{OurMarkersRemoved: true})
	if quiet.String() != "" {
		t.Fatalf("a clean audit printed %q", quiet.String())
	}

	var loud strings.Builder
	reportRestoration(&loud, "platform-worker", &restorationAudit{Drift: []string{"someone/taint"}})
	if !strings.Contains(loud.String(), "someone/taint") || !strings.Contains(loud.String(), "platform-worker") {
		t.Fatalf("a drifted audit printed %q, which names neither the taint nor the worker", loud.String())
	}
}
