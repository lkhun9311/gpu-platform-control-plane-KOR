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
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// observationFor finds the observation for a named target, failing the test rather than returning a zero
// value: a missing observation is exactly the defect TestRecoverEmitsOneObservationPerEnumeratedTarget
// exists to catch, and a caller that silently got a zero-value observation back would test the wrong thing.
func observationFor(t *testing.T, obs []observation, name string) observation {
	t.Helper()
	for _, o := range obs {
		if o.Target.Name == name {
			return o
		}
	}
	t.Fatalf("no observation for target %q", name)
	return observation{}
}

// recoverTargets ranges over enumerate's output and emits exactly one observation per target, error paths
// included. The loop is the coverage guarantee: a target that produced no observation would silently not
// appear in the residue, and "no residue" would read as clear.
func TestRecoverEmitsOneObservationPerEnumeratedTarget(t *testing.T) {
	s := testSeed()
	want, err := enumerate(s)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).Build()
	got, err := recoverTargets(context.Background(), c, s, "tx-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("recover emitted %d observations for %d targets; an unobserved target vanishes from the residue", len(got), len(want))
	}
	seen := map[string]bool{}
	for _, o := range got {
		seen[o.Target.Name] = true
	}
	for _, tg := range want {
		if !seen[tg.Name] {
			t.Errorf("no observation for target %s %q", tg.Kind, tg.Name)
		}
	}
}

// A read that fails must still produce an observation carrying the error, because classifyAbsence turns that
// into absenceUnknown — residue. Skipping it would drop the target out of the audit entirely.
//
// The count check below is load-bearing, not decorative: every Get in this test fails, so a recoverTargets
// that silently dropped a target on read error (a `continue` instead of an append) would leave got empty,
// and the loop that follows would range over nothing and report no failure at all — the exact way a batch
// that "liked the look of nothing" would still read green.
func TestRecoverEmitsAnObservationEvenWhenTheReadFails(t *testing.T) {
	s := testSeed()
	want, err := enumerate(s)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return errors.New("etcdserver: request timed out")
		},
	}).Build()
	got, err := recoverTargets(context.Background(), c, s, "tx-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("recover emitted %d observations for %d targets after every read failed; a dropped observation would misreport the batch as clean", len(got), len(want))
	}
	for _, o := range got {
		if o.Err == nil {
			t.Fatalf("%s carries no error after a failed read; it would classify as absent", o.Target.Name)
		}
		if classifyAbsence(o, o.WantUID) != absenceUnknown {
			t.Fatalf("%s classified as %v after a failed read, want unknown", o.Target.Name, classifyAbsence(o, o.WantUID))
		}
	}
}

// The UID is learned here, not supplied. An object found without our stamp is another run's, and must reach
// the caller as foreign rather than as a deletion candidate.
func TestRecoverLearnsOurUIDAndMarksAForeignObjectForeign(t *testing.T) {
	s := testSeed()
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: "tx-1"}}},
	).Build()
	got, err := recoverTargets(context.Background(), c, s, "tx-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	ns := observationFor(t, got, s.Namespace)
	if ns.UID != "ns-uid" || ns.WantUID != "ns-uid" {
		t.Fatalf("namespace observation carries UID %q / WantUID %q, want both ns-uid: the UID is an output of this pass", ns.UID, ns.WantUID)
	}
	if classifyAbsence(ns, ns.WantUID) != absencePresent {
		t.Fatalf("our own stamped namespace classified as %v, want present", classifyAbsence(ns, ns.WantUID))
	}
}

// txID is a caller-supplied parameter alongside s, which already carries s.TxID; nothing before this test
// forced them to agree. Left unguarded, a caller passing "" would match an unstamped object's absent label
// (both are the empty string) and adopt it as this run's own, collapsing the very refusal
// TestRecoverRefusesAnObjectStampedByAnotherTransaction asserts; a caller passing a mismatched non-empty
// txID would adopt whatever that other transaction stamped instead of this run's own seed. enumerate refuses
// an empty s.TxID at teardown.go:77 on exactly this "empty is not delete less, it is delete a different,
// wrong thing" reasoning, but that guard cannot reach a parameter that routes around the seed entirely.
func TestRecoverRefusesWhenTxIDDisagreesWithTheSeed(t *testing.T) {
	s := testSeed() // s.TxID == "tx-1"

	unstamped := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace, UID: "somebody-elses"}},
	).Build()
	if _, err := recoverTargets(context.Background(), unstamped, s, ""); err == nil {
		t.Fatal("recovery accepted an empty txID, which matched an unstamped object's absent label and would adopt it as ours")
	}

	theirs := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "theirs", Labels: map[string]string{queuelab.TxLabel: "tx-99"}}},
	).Build()
	if _, err := recoverTargets(context.Background(), theirs, s, "tx-99"); err == nil {
		t.Fatal("recovery accepted a txID of tx-99 against a seed whose own TxID is tx-1, adopting another transaction's namespace")
	}
}

// Ownership is established here, by stamp, and an object carrying somebody else's stamp is refused — but the
// refusal is this TARGET's, not the batch's.
//
// A stale cluster-scoped fixture under the same run id is the ordinary rerun, not an exotic case (only the
// namespace is ever cleaned up by hand — applyFixtures' own comment says so). A batch-level refusal turns
// that into a teardown that issues no Delete at all and leaves the run's own namespace on the cluster, so
// each target is judged on its own evidence and the ones this run really did create still come away.
//
// The two assertions that matter are the verdict and the UID. Reaching absenceForeign by INVENTING a UID
// would be the lie this design refuses to tell: WantUID must stay empty, because this run recorded no UID
// for an object it never created, and foreignness here is a fact about the create-time stamp rather than a
// UID comparison it has no operand for.
func TestRecoverReportsAForeignObjectPerTargetRatherThanRefusingTheBatch(t *testing.T) {
	s := testSeed()
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	// The state a rerun under a used run id actually finds: this attempt's own namespace, and a ResourceFlavor
	// a previous attempt left behind under a transaction that is no longer this one's.
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}},
		&kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{
			Name: fs.Flavor.GetName(), UID: "theirs", Labels: map[string]string{queuelab.TxLabel: "tx-previous"}}},
	).Build()
	got, err := recoverTargets(context.Background(), c, s, s.TxID)
	if err != nil {
		t.Fatalf("one foreign target aborted the whole recovery: %v", err)
	}

	ns := observationFor(t, got, s.Namespace)
	if ns.WantUID != "ns-uid" || classifyAbsence(ns, ns.WantUID) != absencePresent {
		t.Fatalf("this run's own namespace recovered as WantUID %q / %v; a foreign object elsewhere in the "+
			"batch must not cost this run the targets it did create", ns.WantUID, classifyAbsence(ns, ns.WantUID))
	}

	rf := observationFor(t, got, fs.Flavor.GetName())
	if a := classifyAbsence(rf, rf.WantUID); a != absenceForeign {
		t.Fatalf("a ResourceFlavor stamped by another transaction classified %v, want foreign: unknown holds "+
			"the phase open for the whole budget and present would make it a deletion target", a)
	}
	if rf.WantUID != "" {
		t.Errorf("recovery recorded WantUID %q for an object this run never created; a UID invented to force a "+
			"foreign verdict would also arm deleteTarget's precondition with it", rf.WantUID)
	}
}

// An unstamped object is foreign for the same reason a differently-stamped one is: it predates stamping or
// was created by something else, and either way this run did not make it. It is called out separately because
// the empty label and an empty txID compare equal, so this is the case a missing guard reads as ours.
func TestRecoverTreatsAnUnstampedObjectAsForeign(t *testing.T) {
	s := testSeed()
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace, UID: "nobody"}},
	).Build()
	got, err := recoverTargets(context.Background(), c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	ns := observationFor(t, got, s.Namespace)
	if a := classifyAbsence(ns, ns.WantUID); a != absenceForeign {
		t.Fatalf("an unstamped namespace classified %v, want foreign; adopting it would delete an object this "+
			"run did not create", a)
	}
}

// Every target kind must actually be read back as itself, not just Namespace: every other test in this file
// seeds at most a Namespace, so a ClusterQueue or ResourceFlavor read under the wrong empty object type (a
// kind-confusion in emptyObjectFor), or looked up by the wrong name (e.g. always Get-ing s.Namespace instead
// of each target's own tg.Name), would come back NotFound against a cluster that actually holds it — and
// nothing before this test could see that, because nothing before this test put one there.
func TestRecoverReadsEveryTargetKindByItsOwnKindAndName(t *testing.T) {
	s := testSeed()
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}},
		&kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{
			Name: fs.Flavor.GetName(), UID: "rf-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}},
	)
	for i, cq := range fs.ClusterQueue {
		b = b.WithObjects(&kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{
			Name: cq.GetName(), UID: types.UID(fmt.Sprintf("cq-uid-%d", i)),
			Labels: map[string]string{queuelab.TxLabel: s.TxID}}})
	}
	wantUID := map[string]string{s.Namespace: "ns-uid", fs.Flavor.GetName(): "rf-uid"}
	for i, cq := range fs.ClusterQueue {
		wantUID[cq.GetName()] = fmt.Sprintf("cq-uid-%d", i)
	}
	got, err := recoverTargets(context.Background(), b.Build(), s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(fs.ClusterQueue) == 0 {
		t.Fatalf("fixture set built no ClusterQueue; this test needs at least one to exercise that kind")
	}
	for _, o := range got {
		if !o.Found {
			t.Errorf("%s %q not found though it was seeded on the cluster", o.Target.Kind, o.Target.Name)
			continue
		}
		// The UID must be THIS object's, not merely some non-empty string: the executor feeds WantUID to
		// client.Preconditions{UID:}, so a cross-wired or constant UID makes every delete precondition miss
		// and leaves a deletable object sitting in the residue.
		if o.UID != wantUID[o.Target.Name] || o.WantUID != o.UID {
			t.Errorf("%s %q learned UID %q / WantUID %q, want both %q", o.Target.Kind, o.Target.Name, o.UID, o.WantUID, wantUID[o.Target.Name])
		}
		// Nothing here was seeded with a deletionTimestamp. An observation that claims otherwise would make
		// an executor treat every object as already-deleting and never issue a Delete at all.
		if o.Terminating {
			t.Errorf("%s %q came back Terminating though it was seeded live", o.Target.Kind, o.Target.Name)
		}
		if classifyAbsence(o, o.WantUID) != absencePresent {
			t.Errorf("%s %q classified as %v, want present", o.Target.Kind, o.Target.Name, classifyAbsence(o, o.WantUID))
		}
	}
}

// The ownership check applies to every kind, not just Namespace: a ClusterQueue or ResourceFlavor stamped by
// another transaction is exactly as much a name collision as a Namespace is, and deleting it destroys that
// other run's live quota just as surely.
func TestRecoverMarksAForeignClusterQueueForeign(t *testing.T) {
	s := testSeed()
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	if len(fs.ClusterQueue) == 0 {
		t.Fatalf("fixture set built no ClusterQueue; this test needs at least one to exercise that kind")
	}
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}},
		&kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{
			Name: fs.ClusterQueue[0].GetName(), UID: "theirs", Labels: map[string]string{queuelab.TxLabel: "tx-2"}}},
	).Build()
	got, err := recoverTargets(context.Background(), c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	cq := observationFor(t, got, fs.ClusterQueue[0].GetName())
	if a := classifyAbsence(cq, cq.WantUID); a != absenceForeign {
		t.Fatalf("a ClusterQueue stamped by another transaction classified %v, want foreign; deleting it would "+
			"destroy another run's live quota", a)
	}
}

// emptyObjectFor must fail loudly on a kind it does not recognize, per its own comment: silently defaulting
// to some other kind's empty object would misread whatever is actually at that name.
func TestEmptyObjectForRejectsAnUnknownKind(t *testing.T) {
	if _, err := emptyObjectFor(target{Kind: "Workload", Name: "whatever"}); err == nil {
		t.Fatal("emptyObjectFor accepted an unregistered kind instead of erroring; a new target kind added to enumerate would silently misread")
	}
}

// A target absent from the cluster must classify as absent, not merely be Found:false and left for the
// caller to reclassify correctly; the case order in recoverTargets's switch (NotFound checked before the
// general error case) is what makes that distinction, and residual must actually drop it.
func TestRecoverNotFoundClassifiesAbsentAndDropsFromResidual(t *testing.T) {
	s := testSeed()
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).Build()
	got, err := recoverTargets(context.Background(), c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("recover returned no observations against an empty cluster")
	}
	for _, o := range got {
		if o.Found {
			t.Errorf("%s %q reported Found on an empty cluster", o.Target.Kind, o.Target.Name)
		}
		if classifyAbsence(o, o.WantUID) != absenceAbsent {
			t.Errorf("%s %q classified as %v on an empty cluster, want absent", o.Target.Kind, o.Target.Name, classifyAbsence(o, o.WantUID))
		}
	}
	if r := residual(got); len(r) != 0 {
		t.Errorf("residual reported %d leftovers on an empty cluster, want 0", len(r))
	}
}

// Terminating must survive from the read into the observation: the executor needs it to tell "deletion
// already requested, keep polling" from "issue the Delete," and to explain a stuck finalizer in the residue
// record. classifyAbsence does not depend on it today (a terminating object still classifies present, same
// as an ordinary one), which is exactly why dropping it silently would pass every other test in this file.
func TestRecoverCarriesTerminatingThrough(t *testing.T) {
	s := testSeed()
	now := metav1.NewTime(time.Now())
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "ns-uid", DeletionTimestamp: &now,
			Finalizers: []string{"queuelab.gpu-platform/hold"},
			Labels:     map[string]string{queuelab.TxLabel: s.TxID}}},
	).Build()
	got, err := recoverTargets(context.Background(), c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	ns := observationFor(t, got, s.Namespace)
	if !ns.Terminating {
		t.Fatal("a namespace with a deletionTimestamp did not come back Terminating")
	}
}

// enumerate documents its return value as the ordered deletion set, and the executor's plan-mandated
// interface is the slice recoverTargets returns — the natural reading is that the order survives unchanged
// so the executor can trust it without re-sorting on Target.Phase itself. A silent reorder (e.g. the
// ResourceFlavor observation ending up before the Namespace one) would invert phase-ordered deletes: an
// executor trusting the slice would issue the ResourceFlavor delete first, and it would block on its own
// finalizer because every referencing ClusterQueue is still there.
func TestRecoverPreservesEnumeratesOrder(t *testing.T) {
	s := testSeed()
	want, err := enumerate(s)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).Build()
	got, err := recoverTargets(context.Background(), c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("recover returned %d observations, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Target.Name != want[i].Name || got[i].Target.Kind != want[i].Kind {
			t.Fatalf("recover reordered position %d: got %s %q, want %s %q",
				i, got[i].Target.Kind, got[i].Target.Name, want[i].Kind, want[i].Name)
		}
	}
}

// recordedDelete is one Delete the executor issued, as the cluster saw it.
type recordedDelete struct {
	Kind string
	Name string
	UID  string // from client.Preconditions, "" if none was set
}

// recordedCall is one call the executor made against the cluster, in the order it made it.
//
// Reads are recorded alongside deletes because phase ordering is a claim about the INTERLEAVING of the two,
// not about the sequence of deletes alone: enumerate already hands its targets over in ascending phase order,
// so an executor that issues every delete at once still records them phase-monotonically. What separates a
// sequenced teardown from one simultaneous sweep is whether the earlier phase had been read absent before the
// later phase's delete went out.
type recordedCall struct {
	Op     string // "read" or "delete"
	Kind   string
	Name   string
	UID    string // delete only: from client.Preconditions, "" if none was set
	Absent bool   // read only: this read proved the object gone
}

// recordCalls wraps a client so every read and delete the executor issues is captured in order.
//
// It wraps rather than builds, so a test that needs its own Get behaviour can install that on the inner
// client and still get a full call log — interceptor.Funcs on a builder is a single slot, and the last one
// set wins.
//
// The properties these tests care about are properties of the CALLS, not of the final state, which a fake
// client would let a wrong implementation reach anyway. That is not a stylistic preference here: fake.Delete
// checks only Preconditions.ResourceVersion and accepts-and-discards Preconditions.UID (client.go:680), and
// fake.ensureTypeMeta actively clears Kind/APIVersion off every object it returns (client.go:1712), so
// neither a missing UID precondition nor the kind of the object deleted is recoverable from the state
// afterwards. The kind is resolved from the scheme, the way the typed client itself resolves it, rather than
// read off a TypeMeta the fake guarantees is empty.
func recordCalls(t *testing.T, inner client.WithWatch) (client.WithWatch, *[]recordedCall) {
	t.Helper()
	var got []recordedCall
	kindOf := func(obj client.Object) string {
		gvk, err := apiutil.GVKForObject(obj, inner.Scheme())
		if err != nil {
			t.Fatalf("no GroupVersionKind for %T: %v", obj, err)
		}
		return gvk.Kind
	}
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := cl.Get(ctx, key, obj, opts...)
			got = append(got, recordedCall{Op: "read", Kind: kindOf(obj), Name: key.Name, Absent: apierrors.IsNotFound(err)})
			return err
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			do := &client.DeleteOptions{}
			do.ApplyOptions(opts)
			uid := ""
			if do.Preconditions != nil && do.Preconditions.UID != nil {
				uid = string(*do.Preconditions.UID)
			}
			got = append(got, recordedCall{Op: "delete", Kind: kindOf(obj), Name: obj.GetName(), UID: uid})
			return cl.Delete(ctx, obj, opts...)
		},
	})
	return c, &got
}

// callRecorder is recordCalls over a plain fake cluster holding objs.
func callRecorder(t *testing.T, objs ...client.Object) (client.WithWatch, *[]recordedCall) {
	t.Helper()
	return recordCalls(t, fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(objs...).Build())
}

// deletesIn narrows a call log to the deletes, in order, for the properties that are about deletes alone.
func deletesIn(calls []recordedCall) []recordedDelete {
	var out []recordedDelete
	for _, c := range calls {
		if c.Op == "delete" {
			out = append(out, recordedDelete{Kind: c.Kind, Name: c.Name, UID: c.UID})
		}
	}
	return out
}

// seedOwned puts every enumerated target on the cluster, stamped as ours, so a teardown has something to do.
// Each carries a distinct UID on purpose: a shared UID would let a cross-wired precondition pass, and the
// precondition is the whole defence against deleting a name someone else recreated.
func seedOwned(t *testing.T, s seed) []client.Object {
	t.Helper()
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	if len(fs.ClusterQueue) == 0 {
		t.Fatalf("fixture set built no ClusterQueue; these tests need at least one to exercise that phase")
	}
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}},
	}
	for i, cq := range fs.ClusterQueue {
		objs = append(objs, &kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{
			Name: cq.GetName(), UID: types.UID(fmt.Sprintf("cq-uid-%d", i)),
			Labels: map[string]string{queuelab.TxLabel: s.TxID}}})
	}
	objs = append(objs, &kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{
		Name: fs.Flavor.GetName(), UID: "rf-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}})
	return objs
}

// fakeClock returns a now/sleep pair that advances only when the executor sleeps, so a poll loop cannot take
// real time and a budget test cannot be flaky.
func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	cur := start
	var mu sync.Mutex
	return func() time.Time { mu.Lock(); defer mu.Unlock(); return cur },
		func(d time.Duration) { mu.Lock(); defer mu.Unlock(); cur = cur.Add(d) }
}

// Phase order is forced by Kueue's finalizers: a ClusterQueue deleted while a Workload still reserves it
// blocks on resource-in-use, and the namespace deletion is what releases those Workloads. Issuing the
// deletes out of order does not fail loudly — it hangs — which is why the order is asserted on the calls
// themselves rather than inferred from the final state.
func TestDeleteIssuesDeletesInPhaseOrder(t *testing.T) {
	s := testSeed()
	c, calls := callRecorder(t, seedOwned(t, s)...)
	now, sleep := fakeClock(time.Unix(0, 0))
	if _, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, time.Minute); err != nil {
		t.Fatalf("delete: %v", err)
	}
	d := deletesIn(*calls)
	got := &d
	if len(*got) == 0 {
		t.Fatal("no deletes were issued at all")
	}
	phaseOf := map[string]int{"Namespace": 0, "ClusterQueue": 1, "ResourceFlavor": 2}
	last := -1
	for _, d := range *got {
		p, ok := phaseOf[d.Kind]
		if !ok {
			t.Fatalf("deleted an unexpected kind %q", d.Kind)
		}
		if p < last {
			t.Fatalf("%s %q (phase %d) was deleted after phase %d; a ClusterQueue removed before the namespace releases its Workloads hangs on resource-in-use", d.Kind, d.Name, p, last)
		}
		last = p
	}
	if last != 2 {
		t.Fatalf("teardown stopped at phase %d; the ResourceFlavor was never deleted", last)
	}
}

// A delete without a UID precondition races a recreate: between the Get that learned the UID and the Delete,
// another actor can remove and recreate the name, and the unconditioned Delete destroys the replacement.
func TestEveryDeleteCarriesItsUIDPrecondition(t *testing.T) {
	s := testSeed()
	c, calls := callRecorder(t, seedOwned(t, s)...)
	now, sleep := fakeClock(time.Unix(0, 0))
	if _, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, time.Minute); err != nil {
		t.Fatalf("delete: %v", err)
	}
	d := deletesIn(*calls)
	got := &d
	if len(*got) == 0 {
		t.Fatal("no deletes were issued at all")
	}
	for _, d := range *got {
		if d.UID == "" {
			t.Errorf("%s %q was deleted with no UID precondition; a recreate between the read and the delete destroys an object this run does not own", d.Kind, d.Name)
		}
	}
}

// A foreign object is a refusal, not a target. Deleting it destroys another run's live state, and this is the
// highest-stakes property in the file: every other failure here costs a run, this one costs somebody else's.
func TestDeleteNeverIssuesADeleteForAForeignTarget(t *testing.T) {
	s := testSeed()
	c, calls := callRecorder(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: s.Namespace, UID: "theirs", Labels: map[string]string{queuelab.TxLabel: "tx-someone-else"}}})
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, time.Minute)
	if err != nil {
		t.Fatalf("a name held by another transaction is cluster state to report, not a reason to abandon the "+
			"batch: %v", err)
	}
	d := deletesIn(*calls)
	got := &d
	for _, d := range *got {
		if d.Name == s.Namespace {
			t.Fatalf("issued a delete for %s %q, which this run did not create", d.Kind, d.Name)
		}
	}
	// It must also be REPORTED. A refusal that leaves no residue is the failure mode where teardown deletes
	// nothing, says nothing, and the record the next run reads names nothing.
	ns := residueFor(t, res.Residue, s.Namespace)
	if ns.Absence != absenceForeign {
		t.Errorf("%s classified %v in the residue, want foreign", s.Namespace, ns.Absence)
	}
}

// A foreign target must not cost this run the targets it did create, and must not cost it the budget either.
//
// Both halves are one behaviour of absenceForeign — phaseTargetSettled accepts it, so the phase advances —
// and both are invisible from the final cluster state, so they are asserted on the calls and on the clock.
// The alternative implementation this rules out is the obvious one: emitting the foreign target as an
// observation carrying an Err. That classifies absenceUnknown, which holds its phase open until the budget
// expires; measured on this executor it is 3m0s of wall clock and 90 poll rounds, and when the foreign target
// is the namespace it also blocks every later phase, so a teardown that had two ClusterQueues and a
// ResourceFlavor of its own to remove issues no Delete at all.
func TestDeleteRemovesOurOwnTargetsAroundAForeignOneWithoutBurningTheBudget(t *testing.T) {
	s := testSeed()
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	if len(fs.ClusterQueue) == 0 {
		t.Fatalf("fixture set built no ClusterQueue; this test needs one in a phase after the namespace's")
	}
	// The two positions that matter: last phase (the stale cluster-scoped flavor a rerun finds, which the
	// batch abort turned into zero deletes) and first phase (the leftover namespace, which additionally has
	// every other phase queued behind it).
	for _, tc := range []struct{ name, foreign string }{
		{"stale flavor from a previous attempt", fs.Flavor.GetName()},
		{"leftover namespace from a previous attempt", s.Namespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := seedOwned(t, s)
			for _, o := range objs {
				if o.GetName() == tc.foreign {
					o.SetLabels(map[string]string{queuelab.TxLabel: "tx-previous"})
				}
			}
			c, calls := callRecorder(t, objs...)
			sleeps := 0
			now, sleep := fakeClock(time.Unix(0, 0))
			budget := teardownBudget
			res, err := deleteTargets(context.Background(), c, s, s.TxID,
				now, func(d time.Duration) { sleeps++; sleep(d) }, budget)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}

			deleted := map[string]bool{}
			for _, d := range deletesIn(*calls) {
				deleted[d.Name] = true
			}
			if deleted[tc.foreign] {
				t.Fatalf("issued a delete for %q, which this run did not create", tc.foreign)
			}
			for _, o := range objs {
				if o.GetName() == tc.foreign {
					continue
				}
				if !deleted[o.GetName()] {
					t.Errorf("%q was created by this run and was never deleted; one foreign name must not "+
						"strand the rest of the set", o.GetName())
				}
			}
			if res.Elapsed >= budget {
				t.Errorf("teardown spent its whole %s budget (%s, %d poll rounds) on a target it had already "+
					"decided it must not touch", budget, res.Elapsed, sleeps)
			}
			if len(res.Residue) != 1 || res.Residue[0].Observation.Target.Name != tc.foreign {
				t.Fatalf("residue is %+v; it must name the one target teardown refused and nothing else",
					res.Residue)
			}
			if res.Residue[0].Absence != absenceForeign {
				t.Errorf("%q classified %v in the residue, want foreign", tc.foreign, res.Residue[0].Absence)
			}
		})
	}
}

// deletionTimestamp is a request, not a result: an object that is terminating has been asked to go away and
// has not gone away, so polling must continue rather than credit it as absent.
func TestDeletePollsUntilAbsentNotUntilTerminating(t *testing.T) {
	s := testSeed()
	polls := 0
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == s.Namespace {
				polls++
				if polls <= 3 {
					// Present, and terminating: the deletion has been accepted and has not completed.
					ns := obj.(*corev1.Namespace)
					*ns = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
						Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID},
						DeletionTimestamp: &metav1.Time{Time: time.Unix(0, 0)},
						Finalizers:        []string{"kubernetes"}}}
					return nil
				}
				return apierrors.NewNotFound(corev1.Resource("namespaces"), s.Namespace)
			}
			// Delegate to this same cluster, not to a second one: the other phases' targets must see the
			// executor's own deletes, or they could never be observed absent and every run would look stuck.
			return cl.Get(ctx, key, obj, opts...)
		},
	}).WithObjects(seedOwned(t, s)...).Build()
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, time.Minute)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(res.Residue) != 0 {
		t.Fatalf("reported residue %v for a namespace that did eventually go away", res.Residue)
	}
	if polls <= 3 {
		t.Fatalf("stopped polling after %d reads; a terminating namespace was credited as gone", polls)
	}
}

// Expiry is the one path that must report residue, so it must be reachable, and the budget must live in the
// loop's own timer — a context deadline would make expiry read as an ordinary cancellation instead.
func TestDeleteReportsResidueWhenTheBudgetExpires(t *testing.T) {
	s := testSeed()
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return nil // accepted, and nothing ever goes away
		},
	}).WithObjects(seedOwned(t, s)...).Build()
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, 30*time.Second)
	if err != nil {
		t.Fatalf("expiry is a result, not a failure to compute one: %v", err)
	}
	if len(res.Residue) == 0 {
		t.Fatal("the budget expired with every target still present and nothing was reported as residue")
	}
	for _, r := range res.Residue {
		if r.Absence == absenceAbsent {
			t.Fatalf("%s carries absenceAbsent yet survived into the residue", r.Observation.Target.Name)
		}
	}
	// Elapsed must come from the injected clock, which only the poll loop's own sleeps advance. Read off the
	// wall clock it would report microseconds for a teardown that spent its whole budget, and a caller sizing
	// the next run's budget from it would size it from nothing.
	if res.Elapsed < 30*time.Second {
		t.Errorf("reported Elapsed %v for a teardown that ran out a 30s budget", res.Elapsed)
	}
}

// Explaining why an object could not be removed must not cost the verdict that it is still there.
//
// A refused Delete used to be folded onto observation.Err, which is the field that means "the read failed".
// classifyAbsence answers absenceUnknown for that, so a target this run had positively READ as present was
// persisted as absence:"unknown" beside found:true — two accounts of one observation — and the node's residue
// stamp told the next operator nobody could tell about an object this one had seen.
//
// Mutation that turns this red: assign the refusal to o.Err instead of o.DeleteRefusal in deleteTargets'
// fold-back, and every absence below becomes unknown.
func TestDeleteRefusalExplainsResidueWithoutDowngradingItsVerdict(t *testing.T) {
	s := testSeed()
	refusal := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, s.Namespace,
		errors.New("admission webhook denied the request"))
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			return refusal
		},
	}).WithObjects(seedOwned(t, s)...).Build()
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, 30*time.Second)
	if err != nil {
		t.Fatalf("a refused delete is a result, not a failure to compute one: %v", err)
	}
	if len(res.Residue) == 0 {
		t.Fatal("every delete was refused and nothing was reported as residue")
	}
	for _, r := range res.Residue {
		if r.Absence == absenceUnknown {
			t.Fatalf("%s was read as present and its delete refused, yet its verdict is unknown",
				r.Observation.Target.Name)
		}
		if r.Observation.Err != nil {
			t.Fatalf("%s carries a read error, but every read here succeeded: %v",
				r.Observation.Target.Name, r.Observation.Err)
		}
	}
	// The refusal still has to reach the record, or the next operator sees "still present" and no reason.
	rec := residueForRecord(res.Residue)
	if len(rec) != len(res.Residue) {
		t.Fatalf("projection dropped residue: %d of %d", len(rec), len(res.Residue))
	}
	var explained int
	for _, e := range rec {
		if e.Absence == absenceName(absenceUnknown) {
			t.Fatalf("%s persisted as unknown", e.Name)
		}
		if strings.Contains(e.Error, "denied the request") {
			explained++
		}
	}
	if explained == 0 {
		t.Fatalf("no residue entry carried the refusal, so the record says present with no reason: %+v", rec)
	}
}

// Phase order is a claim about the interleaving of deletes and reads, not about the order of the deletes on
// their own. enumerate hands its targets over already sorted by phase, so an executor that abandons the
// phases entirely and issues every delete in one sweep still records them in monotone phase order — the
// sequence check above cannot tell the two apart. What distinguishes them is whether the earlier phase had
// been read ABSENT before the later phase's delete went out, because that absence is the thing Kueue's
// resource-in-use finalizer waits on: a ClusterQueue deleted while the namespace still holds its admitted
// Workloads does not fail, it pins, and teardown burns its budget reporting residue it could have removed.
func TestDeleteFinishesEachPhaseBeforeStartingTheNext(t *testing.T) {
	s := testSeed()
	tgs, err := enumerate(s)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	c, calls := callRecorder(t, seedOwned(t, s)...)
	now, sleep := fakeClock(time.Unix(0, 0))
	if _, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, time.Minute); err != nil {
		t.Fatalf("delete: %v", err)
	}
	phaseOf := map[string]teardownPhase{}
	for _, tg := range tgs {
		phaseOf[tg.Name] = tg.Phase
	}
	absent := map[string]bool{}
	deletes := 0
	for _, call := range *calls {
		if call.Op == "read" {
			if call.Absent {
				absent[call.Name] = true
			}
			continue
		}
		deletes++
		for _, tg := range tgs {
			if tg.Phase < phaseOf[call.Name] && !absent[tg.Name] {
				t.Fatalf("issued the delete for %s %q (phase %d) while %s %q (phase %d) had not yet been read absent; "+
					"on a real cluster that earlier object still holds the Workloads pinning this one's resource-in-use finalizer",
					call.Kind, call.Name, phaseOf[call.Name], tg.Kind, tg.Name, tg.Phase)
			}
		}
	}
	if deletes == 0 {
		t.Fatal("no deletes were issued at all")
	}
}

// A residue record that says only "still present" reads as a slow finalizer. If what actually happened is
// that the apiserver refused the delete, the operator has no way to tell those apart, and the next run
// refuses to start with no clue why. The refusal must survive into the result — which is not free, because
// the read that follows the delete in the same round is what proves presence and would otherwise overwrite
// it milliseconds later.
func TestDeleteReportsWhyATargetSurvivedWhenTheApiserverRefused(t *testing.T) {
	s := testSeed()
	c := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return apierrors.NewForbidden(corev1.Resource("namespaces"), obj.GetName(), errors.New("no delete authority"))
		},
	}).WithObjects(seedOwned(t, s)...).Build()
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, 30*time.Second)
	if err != nil {
		t.Fatalf("a refused delete is a fact to report, not a reason to crash: %v", err)
	}
	if len(res.Residue) == 0 {
		t.Fatal("every delete was refused and nothing was reported as residue")
	}
	ns := residueFor(t, res.Residue, s.Namespace)
	if ns.Observation.DeleteRefusal == nil {
		t.Fatalf("%s survived teardown with no recorded reason; the residue says 'still present', which reads as a slow finalizer rather than a refusal", s.Namespace)
	}
	if !apierrors.IsForbidden(ns.Observation.DeleteRefusal) {
		t.Errorf("%s carries %v, want the apiserver's Forbidden", s.Namespace, ns.Observation.DeleteRefusal)
	}
	// This assertion used to read `ns.Absence != absenceUnknown`, on the rationale that "a target whose delete
	// was refused is not known to be present or gone". That rationale does not survive reading the loop it
	// describes: deleteTargets reads AFTER each delete precisely so it exits on evidence, so by the time a
	// refusal is folded back the run holds a successful read saying the object is there. Present is not a
	// guess here, it is the observation — and Found below is what keeps this test honest about that. Unknown
	// would discard a fact the run actually has and, persisted, would tell the next operator nobody could
	// tell about an object this one read.
	if !ns.Observation.Found {
		t.Fatalf("%s is reported as residue but was not observed present; then unknown WOULD be the right verdict "+
			"and this test is asserting the wrong thing", s.Namespace)
	}
	if ns.Absence != absencePresent {
		t.Errorf("%s classified %v; the read that followed the refused delete found it present", s.Namespace, ns.Absence)
	}
	if ns.Observation.Err != nil {
		t.Errorf("%s carries a read error, but the read is what proved it present: %v", s.Namespace, ns.Observation.Err)
	}
}

// Not every refusal is permanent. A 429 under load, an etcd leader change, an admission webhook still coming
// up — each rejects one Delete and relents. The test above only ever sees a Forbidden that never relents, so
// it says nothing about this case, and this case is the one that decides whether an ordinary teardown on a
// busy cluster reports itself clean.
//
// Two properties are pinned together here because they are two halves of one behaviour and each is
// individually redundant: the executor retries a refused delete on the next round, and a refusal stops
// travelling with a target that did in the end come away. Without the first there is nothing to clear;
// without the second, a target read absent still carries the earlier error into residual, where a carried
// error classifies absenceUnknown — so a teardown that removed everything reports residue anyway, and the
// next run refuses to start on a cluster that is actually clean.
func TestDeleteClearsARefusalOnceTheRetrySucceeds(t *testing.T) {
	s := testSeed()
	// Refused once per name, then allowed: the narrowest shape that separates "the apiserver said no" from
	// "the object is still there", which is exactly the distinction a permanently-Forbidden fake cannot make.
	refused := map[string]bool{}
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(seedOwned(t, s)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if !refused[obj.GetName()] {
					refused[obj.GetName()] = true
					return apierrors.NewTooManyRequests("apiserver is shedding load", 1)
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	c, calls := recordCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, 30*time.Second)
	if err != nil {
		t.Fatalf("a refused delete is a fact to report, not a reason to crash: %v", err)
	}
	// The retry asserted on the calls, not inferred from the end state: an executor that never re-issued the
	// Delete would also finish with everything present, and the residue check below alone could not tell that
	// apart from a delete that was issued and did not take.
	attempts := map[string]int{}
	for _, d := range deletesIn(*calls) {
		attempts[d.Name]++
	}
	if attempts[s.Namespace] < 2 {
		t.Fatalf("the namespace's first delete was refused and it was attempted %d time(s) in total; "+
			"a refusal that is never retried turns a momentary 429 into permanent residue", attempts[s.Namespace])
	}
	if len(res.Residue) != 0 {
		t.Fatalf("every target was refused once, came away on the retry, and teardown still reported residue %+v; "+
			"that record is what the next run refuses to start on", res.Residue)
	}
}

// A read that fails proves nothing, and must not settle a phase. Crediting it would let one 503 on the
// namespace read start the ClusterQueue deletes while the namespace and its admitted Workloads are still
// there — the same resource-in-use pin as an out-of-order teardown, reached through the error path instead.
func TestDeleteKeepsAPhaseOpenWhileATargetCannotBeRead(t *testing.T) {
	s := testSeed()
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(seedOwned(t, s)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == s.Namespace {
					return apierrors.NewServiceUnavailable("etcdserver: leader changed")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	c, calls := recordCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, 30*time.Second)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, d := range deletesIn(*calls) {
		if d.Kind != "Namespace" {
			t.Fatalf("issued a delete for %s %q while the namespace read was still failing; an unreadable target proves nothing and must hold its phase open", d.Kind, d.Name)
		}
	}
	ns := residueFor(t, res.Residue, s.Namespace)
	if ns.Absence != absenceUnknown {
		t.Errorf("%s classified %v after every read of it failed, want unknown", s.Namespace, ns.Absence)
	}
}

// Our object can be deleted and a different one created under our name while teardown is running — the one
// case a create-time stamp cannot rule out, and the reason recoverTargets records a UID at all.
//
// Three separate rules meet here. The replacement is never deleted: it is a stranger's object under our name.
// It does not hold its phase open either: a different UID at our name is proof ours is gone, so the Workloads
// it held went with it and the later phases are safe to run — waiting instead would spend the whole budget on
// a deletion this run does not control. And it is still residue, because "someone else's object is here" is
// not "our teardown was clean".
func TestDeleteStopsAtOurNameOnceAnotherTransactionTakesIt(t *testing.T) {
	s := testSeed()
	nsReads := 0
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(seedOwned(t, s)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name != s.Namespace {
					return cl.Get(ctx, key, obj, opts...)
				}
				nsReads++
				ns := obj.(*corev1.Namespace)
				if nsReads == 1 {
					// Recovery sees our own namespace and records its UID.
					*ns = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
						Name: s.Namespace, UID: "ns-uid", Labels: map[string]string{queuelab.TxLabel: s.TxID}}}
					return nil
				}
				// Ours went away and another run took the name, stamp and all.
				*ns = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name: s.Namespace, UID: "stranger", Labels: map[string]string{queuelab.TxLabel: "tx-someone-else"}}}
				return nil
			},
		}).Build()
	c, calls := recordCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))
	res, err := deleteTargets(context.Background(), c, s, s.TxID, now, sleep, 30*time.Second)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	nsDeletes, sawFlavor := 0, false
	for _, d := range deletesIn(*calls) {
		if d.Name == s.Namespace {
			nsDeletes++
			if d.UID != "ns-uid" {
				t.Errorf("the namespace delete carried UID %q; the precondition must be the UID this run recorded, not whatever the latest read saw", d.UID)
			}
		}
		if d.Kind == "ResourceFlavor" {
			sawFlavor = true
		}
	}
	if nsDeletes != 1 {
		t.Errorf("issued %d deletes for %q; exactly one belongs to this run, and any after the takeover would destroy a stranger's namespace", nsDeletes, s.Namespace)
	}
	if !sawFlavor {
		t.Error("teardown never reached the ResourceFlavor; a name taken over by another run is proof ours is gone, not a reason to wait out the budget")
	}
	ns := residueFor(t, res.Residue, s.Namespace)
	if ns.Absence != absenceForeign {
		t.Errorf("%s classified %v after another transaction took the name, want foreign", s.Namespace, ns.Absence)
	}
}

// residueFor finds one target's residue record, failing rather than returning a zero value: a zero residue
// carries absenceUnknown, which several assertions here are looking for, so a silent miss would pass.
func residueFor(t *testing.T, rs []residue, name string) residue {
	t.Helper()
	for _, r := range rs {
		if r.Observation.Target.Name == name {
			return r
		}
	}
	t.Fatalf("no residue record for %q", name)
	return residue{}
}
