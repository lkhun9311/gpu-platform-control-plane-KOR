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
	"sync"
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
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// A signal landing while a barrier is unmet must be reported as itself, not swallowed by the 2-second poll:
// this proves waitBarrier returns on the first cancelled select rather than continuing to poll toward the
// horizon, which is exactly what would make a Ctrl-C during a long barrier wait look hung.
func TestWaitBarrierReturnsPromptlyOnCancelledContext(t *testing.T) {
	scheme := testScheme(t)
	if err := platformv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platformv1 to scheme: %v", err)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	// The horizon is far enough out that only the cancelled select, not the deadline, could end this quickly.
	// The job named here does not exist, so barrierHolds's first check reports "not met" (not an error) and
	// the wait would otherwise poll every 2 s until the deadline.
	col := newCollector(fc, "ns", "r1", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b := queuelab.Barrier{Kind: queuelab.BarrierPending, Job: "does-not-exist"}

	start := time.Now()
	err := waitBarrier(ctx, fc, "ns", b, col)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want an error wrapping context.Canceled, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("waitBarrier took %v to return on an already-cancelled context; it must not poll to the deadline", elapsed)
	}
}

// Gate 0 item 19: an error the barrier loop can never poll its way out of used to burn the whole observation
// window and then be reported as "barrier not met before horizon" — an infrastructure failure dressed up as a
// protocol outcome, and a run's worth of GPU time spent finding out.
//
// A missing RBAC binding is the realistic case: it holds for the entire run, and the operator needs to see it
// now rather than 150 seconds later under a different name.
func TestWaitBarrierReturnsAtOnceOnATerminalError(t *testing.T) {
	scheme := testScheme(t)
	if err := platformv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platformv1 to scheme: %v", err)
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "mltrainingjobs"}, "victim",
		fmt.Errorf("mltrainingjob reader is not bound"))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			return forbidden
		},
	}).Build()
	// An hour out, so only an immediate return can be quick: reaching the deadline is the bug.
	col := newCollector(fc, "ns", "r1", time.Hour)

	start := time.Now()
	err := waitBarrier(context.Background(), fc, "ns",
		queuelab.Barrier{Kind: queuelab.BarrierPending, Job: "victim"}, col)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an authorization failure must fail the run, not be polled")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("waitBarrier took %v; a terminal error must not be retried toward the horizon", elapsed)
	}
	// Preserved with %w, so the caller can tell an infrastructure failure from a protocol one rather than
	// having to read the sentence.
	if !apierrors.IsForbidden(err) {
		t.Fatalf("the underlying error must survive unwrapping, got %v", err)
	}
	if strings.Contains(err.Error(), "not met before horizon") {
		t.Fatalf("a terminal error must not be reported as a barrier miss: %v", err)
	}
}

// A barrier kind nothing can evaluate is a programming error, and polling it for the whole window is the
// worst possible way to report one.
func TestWaitBarrierReturnsAtOnceOnAnUnknownBarrierKind(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	col := newCollector(fc, "ns", "r1", time.Hour)

	start := time.Now()
	err := waitBarrier(context.Background(), fc, "ns", queuelab.Barrier{Kind: "NoSuchBarrier"}, col)

	if err == nil || !errors.Is(err, errUnknownBarrier) {
		t.Fatalf("an unevaluable barrier kind must fail at once as itself, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitBarrier took %v on an unknown barrier kind", elapsed)
	}
}

// The other direction, which the classification must not break: an object the barrier is WAITING to appear is
// absent, which is the ordinary state of every barrier before it holds. That must keep polling and end as a
// barrier miss at the horizon, not as a terminal failure.
func TestWaitBarrierKeepsPollingWhileTheObjectIsMerelyAbsent(t *testing.T) {
	scheme := testScheme(t)
	if err := platformv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platformv1 to scheme: %v", err)
	}
	var gets int
	fc := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			gets++
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	// A horizon short enough that the poll loop reaches it, but long enough to require more than one check.
	col := newCollector(fc, "ns", "r1", 2500*time.Millisecond)

	err := waitBarrier(context.Background(), fc, "ns",
		queuelab.Barrier{Kind: queuelab.BarrierPending, Job: "never-created"}, col)

	if err == nil {
		t.Fatal("a barrier that never holds must fail at the horizon")
	}
	if !strings.Contains(err.Error(), "not met before horizon") {
		t.Fatalf("an absent object is a barrier miss, not a terminal failure: %v", err)
	}
	if gets < 2 {
		t.Fatalf("want the barrier polled more than once (gets=%d); a NotFound must not end the wait", gets)
	}
}

// The horizon refusal says "the last check failed", so it must be describing the last check.
//
// waitBarrier carries the most recent error so a barrier still failing at the deadline can say why. It never
// cleared that error when a later check succeeded, so one transient failure early — a connection reset, a
// momentarily unavailable apiserver — followed by minutes of clean polls that simply never saw the barrier
// hold, produced a refusal blaming an error that had already resolved. An operator reads that and goes
// hunting for a connectivity problem instead of asking why the condition was never met.
//
// Mutation that turns this red: delete the `lastErr = nil` assignment in waitBarrier's err == nil branch.
func TestWaitBarrierDoesNotBlameAnErrorThatLaterChecksCleared(t *testing.T) {
	scheme := testScheme(t)
	if err := platformv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platformv1 to scheme: %v", err)
	}
	var gets int
	transient := errors.New("connection reset by peer")
	fc := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			gets++
			// Only the first check fails; every one after it completes and merely finds the barrier unmet.
			if gets == 1 {
				return transient
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	col := newCollector(fc, "ns", "r1", 2500*time.Millisecond)

	err := waitBarrier(context.Background(), fc, "ns",
		queuelab.Barrier{Kind: queuelab.BarrierPending, Job: "never-created"}, col)

	if err == nil {
		t.Fatal("a barrier that never holds must fail at the horizon")
	}
	// Asserted together: the first half alone would pass if the refusal stopped naming a genuinely failing
	// last check too, which is the other way to get this wrong.
	if strings.Contains(err.Error(), transient.Error()) {
		t.Fatalf("the refusal blames an error that %d later checks cleared: %v", gets-1, err)
	}
	if !strings.Contains(err.Error(), "not met before horizon") {
		t.Fatalf("the barrier was never met, and that is what the refusal should say: %v", err)
	}
}

// The main goroutine used to call col.builder.Desync directly while the four watch goroutines were still
// running and still calling Observe through flush under col.mu, and LedgerBuilder has no locking of its own:
// invalid, events, lastEvent and ready are plain fields. That is a data race on the single field that
// decides whether the run may produce a number at all.
//
// The whole test suite was green under -race before this fix and proved nothing, because no test ran the
// live collector with concurrent watch goroutines. This one does: one goroutine plays the watch side
// (submitObserved takes col.mu and calls Observe, exactly as flush does) while the test goroutine plays the
// main side. Run under -race it fails on the unlocked Desync and passes on the locked one.
func TestCollectorDesyncIsSerialisedWithTheWatchGoroutines(t *testing.T) {
	scheme := testScheme(t)
	if err := platformv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platformv1 to scheme: %v", err)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	col := newCollector(fc, "ns", "r1", time.Hour)

	const rounds = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range rounds {
			col.submitObserved(queuelab.TrainingTraceRow{Name: "victim"}, fmt.Sprintf("uid-%d", i),
				&platformv1.MLTrainingJob{})
		}
	}()
	for range rounds {
		col.desync("barrier before step 0 (victim): deadline")
	}
	<-done

	// Reading the builder is safe only now, which is the same discipline run() follows after col.wait().
	if col.builder.Err() == nil {
		t.Fatal("the desync must have invalidated the run; the test did not exercise what it exists for")
	}
}

// A bare namespace has no identity at all, so a later Get cannot tell ours from one someone else created
// under the same derived name — and the name is derived from the run id alone.
func TestEnsureNamespaceStampsWhatItCreates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).Build()
	if err := ensureNamespace(context.Background(), c, "queuelab-r1", "tx-1"); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	var ns corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: "queuelab-r1"}, &ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got := ns.Labels[queuelab.TxLabel]; got != "tx-1" {
		t.Fatalf("namespace carries tx stamp %q, want tx-1", got)
	}
}

// stripStampOnCreate models the cluster storing something other than what the run sent: a mutating admission
// webhook or a namespace label policy that rewrites labels it does not recognise.
//
// It mutates the object in place because that is what a real client does. controller-runtime's typed client
// posts the object and decodes the RESPONSE — the persisted object — back into the very same pointer
// (typed_client.go's Body(obj)...Into(obj)), and it zeroes that pointer before decoding
// (apiutil.targetZeroingDecoder), so nothing the caller sent survives a field the server did not send back.
// Measured against a stub apiserver on this module's pinned versions, for a dropped label, a replaced label
// map and a rewritten value alike.
func stripStampOnCreate(stored map[string]string, uid types.UID) func(context.Context, client.WithWatch,
	client.Object, ...client.CreateOption) error {
	return func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		obj.SetLabels(stored)
		// The apiserver assigns the UID and returns it; the fake's tracker does not, and without one there is
		// nothing for the delete's precondition to be armed with.
		obj.SetUID(uid)
		return c.Create(ctx, obj, opts...)
	}
}

// A stamp in the Create body is a request, not a fact. Strip it in admission and this run's OWN namespace
// reads foreign at teardown: recoverTargets decides ownership by that label alone, residueHoldsWorker sees
// nothing of ours left, and the worker's dedication label and NoSchedule taint come off over a live namespace
// this run created — under a stderr line saying nothing this run created is still on the cluster.
//
// Detecting it and returning an error does not close that. The namespace is still there, still unstamped,
// still unclaimable by any teardown, and the worker release that follows is still wrong; the hole is only
// announced. So the assertions below are deliberately not satisfiable by an error alone: the namespace this
// call created must be gone, and the delete that removed it must have been conditioned on the UID the Create
// returned, or a name freed and recreated by another actor in between is what the delete would destroy.
func TestEnsureNamespaceRemovesWhatTheClusterStoredUnstamped(t *testing.T) {
	const createdUID = types.UID("ns-uid-1")
	for _, tc := range []struct {
		name   string
		stored map[string]string // what the cluster holds, whatever the run sent
	}{
		{"stamp dropped, no labels left", nil},
		{"stamp replaced by a policy's own label", map[string]string{"policy.example/managed": "true"}},
		{"stamp rewritten to another transaction", map[string]string{queuelab.TxLabel: "tx-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletes := 0
			var gotPreconditions *metav1.Preconditions
			c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Create: stripStampOnCreate(tc.stored, createdUID),
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.DeleteOption) error {
					deletes++
					o := &client.DeleteOptions{}
					o.ApplyOptions(opts)
					// Captured rather than checked through the fake: the fake ignores preconditions outright, so
					// a delete armed with the wrong UID still removes the object there. What it was armed with is
					// the only thing this test can prove.
					gotPreconditions = o.Preconditions
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()

			err := ensureNamespace(context.Background(), c, "queuelab-r1", "tx-1")
			if err == nil {
				t.Fatal("a namespace the cluster stored without this run's stamp was accepted; teardown then " +
					"reads this run's own namespace as another transaction's and releases the worker over it")
			}
			if !strings.Contains(err.Error(), "queuelab-r1") {
				t.Fatalf("refusal %q does not name the namespace it is about", err)
			}
			var ns corev1.Namespace
			gerr := c.Get(context.Background(), client.ObjectKey{Name: "queuelab-r1"}, &ns)
			if gerr == nil {
				t.Fatalf("namespace queuelab-r1 is still on the cluster carrying %v: an error alone leaves "+
					"exactly the unclaimable namespace this refusal exists to prevent", ns.Labels)
			}
			if !apierrors.IsNotFound(gerr) {
				t.Fatalf("reading back the namespace: %v", gerr)
			}
			if deletes != 1 {
				t.Fatalf("the namespace was deleted %d times, want exactly 1", deletes)
			}
			if gotPreconditions == nil || gotPreconditions.UID == nil {
				t.Fatal("the delete carried no UID precondition: another actor that freed this name and " +
					"recreated it between the Create and this Delete would have ITS object destroyed instead")
			}
			if *gotPreconditions.UID != createdUID {
				t.Fatalf("the delete was conditioned on UID %q, want the %q the Create returned",
					*gotPreconditions.UID, createdUID)
			}
		})
	}
}

// A namespace that could not be removed after the cluster refused its stamp is the worst state in this whole
// area: it is on the cluster under no stamp any teardown can recognise, so nothing this run or a later one
// runs will ever claim it. Swallowing the delete's error would report that as an ordinary setup failure and
// leave the operator with no reason to go looking.
func TestEnsureNamespaceReportsANamespaceItCouldNotRemove(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "queuelab-r1",
		fmt.Errorf("namespace deletion is not bound"))
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: stripStampOnCreate(nil, "ns-uid-1"),
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
			opts ...client.DeleteOption) error {
			return refused
		},
	}).Build()

	err := ensureNamespace(context.Background(), c, "queuelab-r1", "tx-1")
	if err == nil {
		t.Fatal("a namespace left behind unstamped AND undeletable was reported as success")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("refusal %q does not carry the refused delete; the operator is never told a namespace was "+
			"left behind under no recoverable stamp", err)
	}
	if !strings.Contains(err.Error(), "stamp") {
		t.Fatalf("refusal %q does not say why the namespace had to go, only that it would not; both facts "+
			"are needed to know what state the cluster is in", err)
	}
}

// Swallowing AlreadyExists is how a previous run's leftovers came to satisfy a new run's barriers. An
// existing object is ours only if it carries our stamp; anything else is a refusal, not a shrug.
func TestEnsureNamespaceAdoptsOnlyItsOwnStamp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     map[string]string
		wantAdopt  bool
		wantErrSub string // required when !wantAdopt, so a wrong-reason refusal cannot pass as this one
	}{
		{"our own stamp", map[string]string{queuelab.TxLabel: "tx-1"}, true, ""},
		{"another transaction", map[string]string{queuelab.TxLabel: "tx-2"}, false, "transaction"},
		{"no stamp at all", nil, false, "transaction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "queuelab-r1", Labels: tc.labels}},
			).Build()
			err := ensureNamespace(context.Background(), c, "queuelab-r1", "tx-1")
			if tc.wantAdopt && err != nil {
				t.Fatalf("our own namespace was refused: %v", err)
			}
			if !tc.wantAdopt {
				if err == nil {
					t.Fatal("a namespace this run did not create was adopted; a previous run's leftovers can then satisfy this run's barriers")
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("refusal %q does not name the reason %q; a wrong-reason refusal must not pass", err, tc.wantErrSub)
				}
			}
		})
	}
}

// The equivalent confound for fixtures: an object left behind by a different transaction under the same run
// id (only the namespace is ever cleaned up by hand between attempts) must not be adopted, or applyFixtures
// proceeds as if this run created a quota mapping, queue, or binding it never wrote.
//
// Every fixture kind is covered, not just the ResourceFlavor: applyFixtures applies createOwned's ownership
// check to every object in its loop, and a mutation that special-cased only the first object (the Flavor)
// would leave the ClusterQueue and LocalQueue cases silently adoptive while this test still passed if they
// were untested. The "no stamp at all" case is the one a lenient reading of the ownership check (treat an
// absent label as "not yet stamped, adopt it") would pass: every fixture created before this task carries no
// TxLabel at all, so that is not a hypothetical, it is what a rerun against real leftover objects hits.
//
// checkFlavorVariant's own pre-check is exercised too ("our tx, wrong variant"), so a mutation that deleted
// that block would not sail through on the strength of the ownership check alone covering applyFixtures.
func TestApplyFixturesAdoptsOnlyItsOwnStamp(t *testing.T) {
	newFixtures := func(t *testing.T) *queuelab.FixtureSet {
		t.Helper()
		fs, err := queuelab.BuildFixtures(queuelab.StudyReclaim, "Any", queuelab.FixtureIdentity{TxID: "tx-1", RunID: "r1", Namespace: "queuelab-r1"})
		if err != nil {
			t.Fatalf("build fixtures: %v", err)
		}
		return fs
	}

	// The pre-existing object is built from the SPEC BuildFixtures produces, with only its labels varied.
	//
	// Earlier versions of these cases seeded a bare ResourceFlavor carrying labels and no spec at all. That is
	// not what a leftover looks like: anything an earlier attempt created came from BuildFixtures and carries
	// the node selector, taint and toleration that isolate the run. Adopting a spec-less flavor would give the
	// run no isolation at all, so once adoption started checking the spec these cases were asserting that a
	// contaminating object gets adopted.
	leftover := func(fs *queuelab.FixtureSet, labels map[string]string) client.Object {
		rf := fs.Flavor.DeepCopy()
		rf.Labels = labels
		return rf
	}

	for _, tc := range []struct {
		name       string
		preexist   func(fs *queuelab.FixtureSet) client.Object
		wantAdopt  bool
		wantErrSub string // required when !wantAdopt, so a wrong-reason refusal cannot pass as this one
	}{
		{
			name: "our own stamp",
			preexist: func(fs *queuelab.FixtureSet) client.Object {
				return leftover(fs, map[string]string{queuelab.TxLabel: "tx-1", variantLabelKey: "Any"})
			},
			wantAdopt: true,
		},
		{
			name: "another transaction",
			preexist: func(fs *queuelab.FixtureSet) client.Object {
				return leftover(fs, map[string]string{queuelab.TxLabel: "tx-2", variantLabelKey: "Any"})
			},
			wantAdopt:  false,
			wantErrSub: "transaction",
		},
		{
			// Every fixture BuildFixtures produced before this task carries no TxLabel, so this is what a
			// rerun against real leftovers hits, not a hypothetical.
			name: "no stamp at all",
			preexist: func(fs *queuelab.FixtureSet) client.Object {
				return leftover(fs, map[string]string{variantLabelKey: "Any"})
			},
			wantAdopt:  false,
			wantErrSub: "transaction",
		},
		{
			// Our own transaction is not enough on its own: checkFlavorVariant's pre-check must still catch
			// a same-transaction flavor built for a different arm's variant.
			name: "our tx, wrong variant",
			preexist: func(fs *queuelab.FixtureSet) client.Object {
				return &kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{
					Name: fs.Flavor.GetName(), Labels: map[string]string{queuelab.TxLabel: "tx-1", variantLabelKey: "Never"},
				}}
			},
			wantAdopt:  false,
			wantErrSub: "variant",
		},
		{
			// The Flavor is absent here, so its Create succeeds outright; only the ClusterQueue pre-exists,
			// which is what isolates createOwned's check on THIS kind rather than the Flavor's.
			name: "foreign cluster queue",
			preexist: func(fs *queuelab.FixtureSet) client.Object {
				cq := fs.ClusterQueue[0]
				return &kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{
					Name: cq.GetName(), Labels: map[string]string{queuelab.TxLabel: "tx-2"},
				}}
			},
			wantAdopt:  false,
			wantErrSub: "transaction",
		},
		{
			// Flavor and ClusterQueues are both absent, so only the LocalQueue's own ownership check can be
			// what refuses this case.
			name: "foreign local queue",
			preexist: func(fs *queuelab.FixtureSet) client.Object {
				lq := fs.LocalQueue[0]
				return &kueuev1beta2.LocalQueue{ObjectMeta: metav1.ObjectMeta{
					Name: lq.GetName(), Namespace: lq.GetNamespace(), Labels: map[string]string{queuelab.TxLabel: "tx-2"},
				}}
			},
			wantAdopt:  false,
			wantErrSub: "transaction",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFixtures(t)
			c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(tc.preexist(fs)).Build()
			err := applyFixtures(context.Background(), c, fs, "Any", "tx-1")
			if tc.wantAdopt && err != nil {
				t.Fatalf("our own fixture was refused: %v", err)
			}
			if !tc.wantAdopt {
				if err == nil {
					t.Fatal("a fixture this run did not create was adopted; a previous transaction's leftovers can then satisfy this run's barriers")
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("refusal %q does not name the reason %q; a wrong-reason refusal must not pass", err, tc.wantErrSub)
				}
			}
		})
	}
}

// ---- the observation streams ----

// streamingFake is a fake cluster a stream can actually be opened against.
//
// The List stamp is not decoration: the fake client's tracker returns lists with an empty resource version,
// and an empty one is precisely the unresumable point startWatchStream refuses, so without it no test below
// gets as far as a watch. watchFn is per-test because what these tests exercise is what the collector does
// when a watch misbehaves, and a nil one means every kind gets a watch that establishes and then says
// nothing — the quiet baseline the interesting cases are measured against.
func streamingFake(t *testing.T, seed []client.Object,
	watchFn func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error)) client.WithWatch {
	t.Helper()
	if watchFn == nil {
		watchFn = fakeSchedulerWatch
	}
	return fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(seed...).
		WithInterceptorFuncs(interceptor.Funcs{List: fakeSchedulerList, Watch: watchFn}).Build()
}

// isPodList reports whether a stream is the Pod one, which every test below rigs because it is the LAST of
// the four the collector opens: a failure there proves the barrier is not satisfied by the three that came
// before it.
func isPodList(list client.ObjectList) bool {
	_, ok := list.(*corev1.PodList)
	return ok
}

// ledgerErr reads the ledger's verdict while the stream consumers are still running, under the very mutex
// that makes those consumers safe.
//
// LedgerBuilder has no locking of its own, so run() only ever reads it after col.wait() has joined every
// consumer. A test that wants to watch an invalidation ARRIVE cannot wait that long — the point is that the
// run is still observing when it happens — so it borrows the collector's mutex instead of racing it.
func ledgerErr(col *collector) error {
	col.mu.Lock()
	defer col.mu.Unlock()
	return col.builder.Err()
}

// A namespace this run created moments ago must be empty of the four kinds it watches, so an object already
// standing in one is a previous attempt's or another actor's — and the trace job names are fixed by the
// protocol, so a previous attempt's Pods carry the very names this run is about to use. Folding them in would
// have the reconstruction measure one run against another's work; dropping them silently would leave the
// ledger describing a namespace it had only half seen. Neither is a run, so there is no run.
//
// Mutation that turns this red: delete the `if n := s.Baseline().Objects; n > 0` block in collector.start.
// The baseline is then counted and discarded, which is what startWatchStream on its own does today.
func TestCollectorRefusesANamespaceThatAlreadyHoldsWatchedObjects(t *testing.T) {
	leftover := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "victim-0", UID: types.UID("pod-from-a-previous-attempt")}}
	col := newCollector(streamingFake(t, []client.Object{leftover}, nil), "ns", "r1", time.Hour)
	ctx := t.Context()

	err := col.start(ctx)
	if err == nil {
		t.Fatal("a namespace already holding a watched object must refuse the run before it submits anything")
	}
	// The kind and the count are what make the refusal actionable: all four streams watch one namespace, so a
	// message naming only the namespace reads identically whichever view was occupied.
	if !strings.Contains(err.Error(), kindPod) || !strings.Contains(err.Error(), "1 object") {
		t.Fatalf("the refusal %q names neither the occupied kind nor how much was there", err)
	}
	// The error is what stops run(); this is what stops anything else. A caller that dropped the error must
	// still not be able to get a number out of the ledger.
	if col.builder.Err() == nil {
		t.Fatal("a refused baseline must also invalidate the ledger, or only the caller's diligence stands between a polluted namespace and a published result")
	}
}

// Nothing may be submitted until every stream has actually accepted a watch. The failure this pins is the one
// the old collector had by construction: it retried a failing watch forever, in silence, while run() went
// straight into the submit loop — so a run could spend its entire window offering work to a cluster it was
// not observing and still reach the end with a ledger it treated as complete.
//
// Mutation that turns this red: replace awaitEstablished's select with a bare `<-ks.stream.Established()`.
// The wait then never returns for a watch that is merely failing, and the test hangs until the package
// timeout rather than refusing the run.
func TestCollectorRefusesToObserveWhenAWatchNeverEstablishes(t *testing.T) {
	c := streamingFake(t, nil, func(ctx context.Context, cl client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) (watch.Interface, error) {
		if isPodList(list) {
			// A plain error is one RetryWatcher retries indefinitely — the shape of an apiserver that is merely
			// unreachable — so this stream neither establishes nor ends, and only the budget can end the wait.
			return nil, errors.New("apiserver unreachable")
		}
		return newStubWatch(ctx), nil
	})
	col := newCollector(c, "ns", "r1", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); col.wait() }()

	if err := col.start(ctx); err != nil {
		t.Fatalf("the baselines all listed cleanly, so start must succeed: %v", err)
	}
	start := time.Now()
	err := col.awaitEstablished(ctx, 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a watch that never establishes must refuse the run rather than let it submit")
	}
	if !strings.Contains(err.Error(), kindPod) {
		t.Fatalf("the refusal %q does not say which view of the run was never opened", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the wait took %v against a 300ms budget, so it is bounded by something other than the budget", elapsed)
	}
}

// deadlineSpy records every observation stream that was handed a context carrying a deadline.
//
// It exists because of how the first version of the test below failed. That version bounded establishment
// with a 150ms budget and then checked, ~900ms later, that no stream had died — which does catch the trap at
// 150ms and does NOT catch it at the production constant, because a stream killed at fifteen seconds outlives
// any test that waits under a second. The mutation could therefore be reintroduced at the value that actually
// ships with the whole suite green: the same class of silent hole this gate exists to close, one level up.
//
// A deadline is visible the moment the context arrives, whatever its value, so the check sits at the watch
// call rather than in a race against the clock.
//
// It records instead of calling t.Errorf on the spot because interceptors run on RetryWatcher's goroutines,
// which can outlive the test function, and a t.Errorf from one of those after the test has returned panics
// the whole binary. calls is counted for the same reason the assertion is not simply "nothing bounded": if no
// watch was ever opened, "no bounded context was seen" is true and means nothing.
type deadlineSpy struct {
	mu      sync.Mutex
	calls   int
	bounded []string
}

func (s *deadlineSpy) watch(inner func(context.Context, client.WithWatch, client.ObjectList,
	...client.ListOption) (watch.Interface, error)) func(context.Context, client.WithWatch, client.ObjectList,
	...client.ListOption) (watch.Interface, error) {
	return func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) (watch.Interface, error) {
		s.mu.Lock()
		s.calls++
		if dl, ok := ctx.Deadline(); ok {
			s.bounded = append(s.bounded, fmt.Sprintf("%T expires in %s", list, time.Until(dl).Round(time.Second)))
		}
		s.mu.Unlock()
		return inner(ctx, c, list, opts...)
	}
}

// observed reports how many watches were opened and which of them were handed a bounded context.
func (s *deadlineSpy) observed() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]string(nil), s.bounded...)
}

// The trap startWatchStream's doc comment names, in executable form. Bounding establishment with a
// context.WithTimeout hands that deadline to the streams themselves, so every stream dies when it expires and
// every ending afterwards reads as Cancelled — the caller's own shutdown signature. A run that stopped
// observing fifteen seconds into a 150-second window would then report itself as having shut down cleanly,
// which is the exact failure class this whole change exists to remove, reintroduced by the fix for it.
//
// The budget passed here is the REAL one, so nothing about this test depends on the constant being small.
// What kills it is the deadline itself, seen where the stream's context arrives.
//
// Mutation that turns this red: bound the streams' own context anywhere, e.g. `ctx, cancel :=
// context.WithTimeout(ctx, establishBudget)` at the top of collector.start. The run-level half of the same
// mutation — col.start(context.WithTimeout(cctx, establishBudget)) inside run() — is pinned by
// TestRunHandsTheStreamsAnUnboundedContext, because no collector-level test can see how run() built the
// context it was given.
//
// A different wrong implementation with no deadline in it at all — an establishment-scoped
// context.WithCancel whose cancel is deferred — is caught too, by whichever check the timing reaches first:
// the establishment wait itself when the cancel lands before it returns (the observed case, reported as the
// Pod stream ending before it established), and the liveness check below when it lands after.
func TestCollectorEstablishmentBudgetIsNotTheStreamsDeadline(t *testing.T) {
	spy := &deadlineSpy{}
	col := newCollector(streamingFake(t, nil, spy.watch(fakeSchedulerWatch)), "ns", "r1", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); col.wait() }()

	if err := col.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := col.awaitEstablished(ctx, establishBudget); err != nil {
		t.Fatalf("four stub watches establish immediately, so this must not fail: %v", err)
	}

	calls, bounded := spy.observed()
	if calls < 4 {
		t.Fatalf("only %d watch(es) were opened, so this test never saw the four stream contexts it exists to inspect", calls)
	}
	if len(bounded) > 0 {
		t.Fatalf("the streams were handed bounded contexts (%v): every stream then dies when the bound expires "+
			"and its ending reads Cancelled, so a run that stopped observing reports an orderly shutdown", bounded)
	}
	// Short on purpose: this is not waiting out a budget, it is checking that establishment did not take the
	// streams with it on the way out.
	time.Sleep(100 * time.Millisecond)
	for _, ks := range col.streams {
		select {
		case <-ks.stream.End():
			t.Fatalf("the %s stream ended as soon as establishment finished: %+v", ks.kind, ks.stream.Ended())
		default:
		}
	}
	if err := ledgerErr(col); err != nil {
		t.Fatalf("the run was invalidated while every stream was still live: %v", err)
	}
}

// A stream that ends while the run is still observing is a gap that can never be closed: the events in it are
// lost, not delayed, and no later list can say what they were. The old collector reconnected from "now" here
// and carried on, which is precisely how a run could miss a preemption and still print a number.
//
// The reconnect is refused with a WRAPPED Forbidden on purpose. RetryWatcher recognises the reason but cannot
// assert the concrete status type, so it terminates having forwarded nothing at all: there is no error event,
// no status, and nothing to notice except the ending itself.
//
// Mutation that turns this red: delete the `case !end.Cancelled && !end.Stopped` arm in consume. With no
// status forwarded on this path there is nothing else left to catch it, and the run stays valid with a Pod
// stream that stopped delivering.
func TestCollectorInvalidatesWhenAStreamEndsWhileTheRunIsObserving(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no watch permission"))
	first := watch.NewFake()
	var (
		mu      sync.Mutex
		podCall int
	)
	c := streamingFake(t, nil, func(ctx context.Context, cl client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) (watch.Interface, error) {
		if !isPodList(list) {
			return newStubWatch(ctx), nil
		}
		mu.Lock()
		podCall++
		n := podCall
		mu.Unlock()
		if n == 1 {
			return first, nil
		}
		return nil, fmt.Errorf("watch pods: %w", forbidden)
	})
	col := newCollector(c, "ns", "r1", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); col.wait() }()

	if err := col.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := col.awaitEstablished(ctx, 5*time.Second); err != nil {
		t.Fatalf("every stream establishes on its first watch here: %v", err)
	}
	// The apiserver closing a watch, which on its own is ordinary and recoverable; what makes it terminal is
	// that the resume fails permanently.
	first.Stop()

	waitFor(t, func() bool { return ledgerErr(col) != nil })
	err := ledgerErr(col)
	if !strings.Contains(err.Error(), kindPod) || !strings.Contains(err.Error(), "ended on its own") {
		t.Fatalf("the invalidation %q does not say which stream stopped or that it stopped by itself", err)
	}
	// Nobody cancelled or stopped anything, so an ending reported as either would be the tidy-shutdown reading
	// this test exists to rule out.
	if ctx.Err() != nil {
		t.Fatal("the run's context is still live, so nothing here may be read as a caller cancellation")
	}
}

// A 410 is the one terminal cause the apiserver states outright — the resume point has aged out of etcd — and
// the run has to carry that cause, not merely the fact that something stopped. An operator who sees "Gone"
// knows to rerun; one who sees "the stream ended" does not know whether to fix RBAC first.
//
// Mutation that turns this red: delete the `case end.LastStatus != nil` arm in consume. The run is still
// invalidated by the arm below it, so the mutation is invisible to a test that only asks whether the ledger
// refused — which is why this one asserts on the cause instead.
func TestCollectorInvalidationNamesATerminalWatchStatus(t *testing.T) {
	gone := watch.NewFakeWithChanSize(1, false)
	gone.Error(&metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusGone,
		Reason:  metav1.StatusReasonExpired,
		Message: "too old resource version: 1 (2000)",
	})
	c := streamingFake(t, nil, func(ctx context.Context, cl client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) (watch.Interface, error) {
		if isPodList(list) {
			return gone, nil
		}
		return newStubWatch(ctx), nil
	})
	col := newCollector(c, "ns", "r1", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); col.wait() }()

	if err := col.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool { return ledgerErr(col) != nil })

	err := ledgerErr(col)
	if !strings.Contains(err.Error(), kindPod) {
		t.Fatalf("the invalidation %q does not say which view of the run was lost", err)
	}
	if !strings.Contains(err.Error(), "410") || !strings.Contains(err.Error(), string(metav1.StatusReasonExpired)) {
		t.Fatalf("the invalidation %q drops the cause the apiserver stated, leaving an operator to guess at it", err)
	}
}
