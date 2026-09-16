//go:build queuelabkind

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

// Everything in this file needs a real apiserver with Kueue installed, which is why it is behind a build tag
// rather than -short: the rest of the package runs against a fake client that has no finalizer controller at
// all, so its "the namespace is gone" is a tautology — nothing removed the namespace, the fake simply never
// held anything back. Every phase-order claim in teardown.go is an ARGUMENT about finalizer behaviour, and
// this file is the only place any of it is checked against the thing being argued about.
//
// Two mechanisms in particular are unreachable from the fake and reachable only here. The fake ignores
// client.Preconditions outright (its Delete checks ResourceVersion and nothing else), so deleteTarget's UID
// precondition — the single thing standing between teardown and destroying a replacement object it does not
// own — has never executed. And the budget has only ever expired on an injected clock, so the gap between
// teardownBudget and teardownContextTimeout has never had a real cancellation on the other side of it.
//
// Run by hand, with a cluster:
//
//	go test -tags queuelabkind ./cmd/queuelabrun/ -run TestKind -count=1 -v
//
// These tests only ever touch names derived from their own per-run id, plus the namespace they create. That
// is the real safety property, not the build tag: a stray invocation against the wrong cluster still cannot
// name an object it did not make.

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// kindPodFinalizer is the finalizer this file hangs on a Pod to make a namespace refuse to finish
// terminating.
//
// A Pod that ignores SIGTERM under a long terminationGracePeriodSeconds would also produce a stuck namespace,
// and it would be closer to what the lab's own GPU Pods do. It is rejected here for one reason: the test must
// be able to release it on EVERY exit path, and a grace period cannot be shortened once deletion is under way
// — only a force-delete escapes it, and a force-delete on a real cluster tells the apiserver the container is
// gone when it may not be. A finalizer is held and released by this test alone, takes effect whether or not
// the Pod ever schedules or its image ever pulls, and leaves nothing running behind it.
const kindPodFinalizer = "queuelab.gpu-platform/kind-test-hold"

// kindRunID mints a run id unique to this invocation.
//
// Every object these tests create is cluster-scoped or a namespace, on a cluster that stays up between runs,
// so a fixed name would mean each run inherits the previous run's leftovers — and a stale ClusterQueue under
// a reused name is not a tidiness problem: applyFixtures refuses a name it finds under another transaction's
// stamp, so the second run fails on setup and blames the code.
func kindRunID() string { return "k" + strconv.FormatInt(time.Now().UnixNano(), 36) }

// kindSeed builds the seed for one invocation, with the reclaim study because it is the one that produces two
// ClusterQueues — a phase with a single member cannot show that a phase waits for ALL of its targets.
func kindSeed() seed {
	run := kindRunID()
	return seed{
		Schema: teardownSeedSchema, TxID: "tx-" + run, RunID: run, Arm: "A-honor",
		Study: queuelab.StudyReclaim, Variant: "Any", Namespace: "queuelab-" + run,
	}
}

// kindClient returns the same client the runner itself builds, and proves the cluster answers before any test
// gets the chance to blame a finalizer for what is really a missing kubeconfig.
func kindClient(t *testing.T) client.WithWatch {
	t.Helper()
	c, err := newClusterClient()
	if err != nil {
		t.Fatalf("build cluster client (these tests need a kubeconfig pointing at a kind cluster with Kueue): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var flavors kueuev1beta2.ResourceFlavorList
	if err := c.List(ctx, &flavors); err != nil {
		t.Fatalf("list ResourceFlavors (is Kueue installed?): %v", err)
	}
	return c
}

// kindFixtures creates exactly what a run creates, through the run's own creation functions rather than
// through hand-built objects: the stamp ensureNamespace writes and the stamp recoverTargets reads have to be
// the same stamp, and a test that wrote its own labels would pass even if those two drifted apart.
func kindFixtures(ctx context.Context, t *testing.T, c client.Client, s seed) *queuelab.FixtureSet {
	t.Helper()
	if err := ensureNamespace(ctx, c, s.Namespace, s.TxID); err != nil {
		t.Fatalf("ensure namespace %s: %v", s.Namespace, err)
	}
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	if err := applyFixtures(ctx, c, fs, s.Variant, s.TxID); err != nil {
		t.Fatalf("apply fixtures: %v", err)
	}
	return fs
}

// kindReserveQuota creates a Workload against one of the run's LocalQueues and waits until Kueue has actually
// reserved quota for it, because an unadmitted Workload holds nothing.
//
// It is a bare Workload rather than a Job because the property under test is the ClusterQueue's
// resource-in-use finalizer, and that finalizer keys on Kueue's cache of RESERVING workloads — which a
// directly-created Workload enters just as a Job-owned one does, without needing a Job controller, a
// schedulable node, or a GPU that this cluster does not have.
func kindReserveQuota(ctx context.Context, t *testing.T, c client.Client, s seed, localQueue string) *kueuev1beta2.Workload {
	t.Helper()
	wl := &kueuev1beta2.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ql-hold",
			Namespace: s.Namespace,
			Labels:    map[string]string{queuelab.TxLabel: s.TxID},
		},
		Spec: kueuev1beta2.WorkloadSpec{
			QueueName: kueuev1beta2.LocalQueueName(localQueue),
			PodSets: []kueuev1beta2.PodSet{{
				Name:  "main",
				Count: 1,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "hold",
						Image: "registry.k8s.io/pause:3.9",
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							"nvidia.com/gpu": *resource.NewQuantity(1, resource.DecimalSI),
						}},
					}},
				}},
			}},
		},
	}
	if err := c.Create(ctx, wl); err != nil {
		t.Fatalf("create reserving workload: %v", err)
	}
	// Waited on rather than assumed: if admission were to fail — a flavor whose node taints the podset cannot
	// tolerate, a ClusterQueue that never went Active — every finalizer assertion below would pass vacuously,
	// because an unreserved ClusterQueue deletes immediately and would look exactly like a correct teardown.
	kindWaitFor(ctx, t, 60*time.Second, "workload to reserve quota", func() (bool, string) {
		var got kueuev1beta2.Workload
		if err := c.Get(ctx, client.ObjectKeyFromObject(wl), &got); err != nil {
			return false, err.Error()
		}
		for _, cond := range got.Status.Conditions {
			if cond.Type == "QuotaReserved" && cond.Status == metav1.ConditionTrue {
				return true, ""
			}
		}
		return false, fmt.Sprintf("conditions=%v", got.Status.Conditions)
	})
	return wl
}

// kindWaitFor polls until want reports true, failing with the last thing it saw rather than with a bare
// timeout: on a cluster the interesting part of a wait that expired is always what it was looking at.
func kindWaitFor(ctx context.Context, t *testing.T, limit time.Duration, what string, want func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for {
		ok, detail := want()
		if ok {
			return
		}
		last = detail
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out after %s waiting for %s; last saw: %s", limit, what, last)
		}
		// The caller's context is checked as well as the local limit, because a dead context turns every poll
		// below into the same connection error and the wait would otherwise burn its whole limit reporting it.
		if err := ctx.Err(); err != nil {
			t.Fatalf("context ended while waiting for %s (%v); last saw: %s", what, err, last)
		}
		time.Sleep(time.Second)
	}
}

// kindPresent reports whether a cluster-scoped object of this target's kind is still readable, and whether it
// carries a deletionTimestamp. Both halves matter separately: a target with a deletionTimestamp is one whose
// Delete the apiserver ACCEPTED and a finalizer is holding, which is the exact state that distinguishes
// "blocked" from "never asked".
func kindPresent(ctx context.Context, t *testing.T, c client.Client, tg target) (found, terminating bool, finalizers []string) {
	t.Helper()
	obj, err := emptyObjectFor(tg)
	if err != nil {
		t.Fatalf("no reader for %s: %v", tg.Kind, err)
	}
	switch err := c.Get(ctx, client.ObjectKey{Name: tg.Name}, obj); {
	case apierrors.IsNotFound(err):
		return false, false, nil
	case err != nil:
		t.Fatalf("get %s %s: %v", tg.Kind, tg.Name, err)
	}
	return true, obj.GetDeletionTimestamp() != nil, obj.GetFinalizers()
}

// kindCleanup removes everything one test created, on every exit path including a failed one, and then FAILS
// the test if anything is still standing.
//
// The check at the end is not belt-and-braces. A leaked ResourceFlavor or ClusterQueue is cluster-scoped and
// this cluster is reused, so the cost of a silent leak is not paid by the test that leaked it — it is paid by
// the next run, as a setup failure in a completely different test. Anything this file deliberately wedges
// (a Pod finalizer, a stripped namespace) it also releases here, in the order the finalizers require.
func kindCleanup(t *testing.T, c client.Client, s seed) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Pods first: a Pod this file wedged is what holds the namespace, and the namespace is what holds the
	// Workloads that hold the ClusterQueues. Releasing it in any other order just means waiting.
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(s.Namespace)); err == nil {
		for i := range pods.Items {
			p := &pods.Items[i]
			if len(p.Finalizers) == 0 {
				continue
			}
			p.Finalizers = nil
			if err := c.Update(ctx, p); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup: release finalizer on pod %s/%s: %v", p.Namespace, p.Name, err)
			}
		}
	}

	// Workloads are deleted by name rather than left to the namespace, because one test deliberately strips
	// the namespace's own finalizer and the Workloads it contained then outlive it — orphaned, unreachable by
	// any namespace delete, and still reserving quota in Kueue's cache. A LIST scoped to a namespace that no
	// longer exists still returns them, which is the only reason this recovery is possible at all.
	var wls kueuev1beta2.WorkloadList
	if err := c.List(ctx, &wls, client.InNamespace(s.Namespace)); err == nil {
		for i := range wls.Items {
			w := &wls.Items[i]
			if len(w.Finalizers) > 0 {
				w.Finalizers = nil
				if err := c.Update(ctx, w); err != nil && !apierrors.IsNotFound(err) {
					t.Errorf("cleanup: release finalizer on workload %s/%s: %v", w.Namespace, w.Name, err)
				}
			}
			if err := c.Delete(ctx, w); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup: delete workload %s/%s: %v", w.Namespace, w.Name, err)
			}
		}
	}

	targets, err := enumerate(s)
	if err != nil {
		t.Fatalf("cleanup: enumerate %v", err)
	}
	for _, tg := range targets {
		obj, err := deleteObjectFor(tg)
		if err != nil {
			t.Errorf("cleanup: %v", err)
			continue
		}
		// No UID precondition here, deliberately: cleanup wants the name free whatever is sitting on it, and
		// unlike the executor it is allowed that authority because every one of these names was minted by this
		// invocation and cannot be anyone else's.
		if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("cleanup: delete %s %s: %v", tg.Kind, tg.Name, err)
		}
	}

	for _, tg := range targets {
		tg := tg
		kindWaitFor(ctx, t, 2*time.Minute, fmt.Sprintf("cleanup of %s %s", tg.Kind, tg.Name), func() (bool, string) {
			obj, err := emptyObjectFor(tg)
			if err != nil {
				return true, ""
			}
			gerr := c.Get(ctx, client.ObjectKey{Name: tg.Name}, obj)
			if apierrors.IsNotFound(gerr) {
				return true, ""
			}
			if gerr != nil {
				return false, gerr.Error()
			}
			return false, fmt.Sprintf("still present, finalizers=%v deletionTimestamp=%v",
				obj.GetFinalizers(), obj.GetDeletionTimestamp())
		})
	}
}

// TestKindOutOfOrderDeleteBlocksSilently is the assertion the whole phase order rests on.
//
// deleteTargets breaks out of its phase loop rather than pressing on, and the reason given at
// teardown_apply.go's break is that deleting a ClusterQueue while the namespace holding its Workloads is
// still there "does not fail loudly — it blocks on the resource-in-use finalizer". Every other test in this
// package takes that on trust, because a fake client has no finalizers to block on. If it were wrong — if an
// out-of-order Delete returned an error the executor could see — the phase machinery would be an elaborate
// way to avoid an error message, and the honest design would be to delete everything at once and read the
// failures.
//
// So this test deletes IN THE WRONG ORDER on purpose and reports what the apiserver actually does.
func TestKindOutOfOrderDeleteBlocksSilently(t *testing.T) {
	s := kindSeed()
	c := kindClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() { kindCleanup(t, c, s) })

	fs := kindFixtures(ctx, t, c, s)
	kindReserveQuota(ctx, t, c, s, fs.LocalQueue[0].GetName())

	targets, err := enumerate(s)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	var cqs []target
	var flavor target
	for _, tg := range targets {
		switch tg.Kind {
		case "ClusterQueue":
			cqs = append(cqs, tg)
		case "ResourceFlavor":
			flavor = tg
		}
	}
	if len(cqs) != 2 {
		t.Fatalf("the reclaim study is expected to produce two ClusterQueues, got %d; the second half of this "+
			"test needs a sibling to hold the flavor after the first ClusterQueue clears", len(cqs))
	}
	cq := cqs[0]

	// Phase 1 out of order: the namespace is untouched, so a Workload still reserves this ClusterQueue.
	cqObj, err := deleteObjectFor(cq)
	if err != nil {
		t.Fatalf("delete object: %v", err)
	}
	start := time.Now()
	if err := c.Delete(ctx, cqObj); err != nil {
		t.Fatalf("out-of-order ClusterQueue delete returned %v — if a real apiserver refuses this, the phase "+
			"order could be replaced by reading the error, and teardown_apply.go's break is wrong about why it exists", err)
	}
	t.Logf("out-of-order ClusterQueue Delete returned nil in %s", time.Since(start).Round(time.Millisecond))

	// Phase 2 out of order too, and for a different reason: the flavor is referenced by a ClusterQueue that
	// is now terminating but still very much present, and Kueue counts a terminating ClusterQueue as a user.
	flavorObj, err := deleteObjectFor(flavor)
	if err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if err := c.Delete(ctx, flavorObj); err != nil {
		t.Fatalf("out-of-order ResourceFlavor delete returned %v, want nil", err)
	}

	// Long enough that a delete which was merely slow would have finished; the cascade below, once unblocked,
	// completes the whole set in a handful of seconds.
	time.Sleep(20 * time.Second)

	found, terminating, fins := kindPresent(ctx, t, c, cq)
	if !found {
		t.Fatalf("ClusterQueue %s is gone 20s after an out-of-order delete; the resource-in-use finalizer did "+
			"not hold it, and phaseClusterQueue's whole justification is void", cq.Name)
	}
	if !terminating {
		t.Errorf("ClusterQueue %s carries no deletionTimestamp; the Delete was not accepted at all, which is a "+
			"different story from the one the executor tells", cq.Name)
	}
	t.Logf("20s later: ClusterQueue %s still present, terminating=%v finalizers=%v", cq.Name, terminating, fins)

	found, terminating, fins = kindPresent(ctx, t, c, flavor)
	if !found {
		t.Fatalf("ResourceFlavor %s is gone while a terminating ClusterQueue still references it; "+
			"phaseResourceFlavor's ordering would then be unnecessary", flavor.Name)
	}
	t.Logf("20s later: ResourceFlavor %s still present, terminating=%v finalizers=%v", flavor.Name, terminating, fins)

	// The counterpart the first half only implies: the block is not permanent damage, it is a wait. Removing
	// the namespace releases the Workload, and the delete the apiserver accepted 20 seconds ago completes on
	// its own. This is what makes "does not fail loudly" the right description rather than "fails later".
	if err := c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace}}); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	unblocked := time.Now()
	kindWaitFor(ctx, t, 90*time.Second, fmt.Sprintf("ClusterQueue %s to finish deleting", cq.Name),
		func() (bool, string) {
			found, terminating, fins := kindPresent(ctx, t, c, cq)
			return !found, fmt.Sprintf("terminating=%v finalizers=%v", terminating, fins)
		})
	t.Logf("ClusterQueue %s cleared %s after the namespace delete, with no second Delete issued",
		cq.Name, time.Since(unblocked).Round(time.Second))

	// And now the SECOND ordering claim, which the namespace has nothing to do with. The flavor's Delete was
	// accepted 20 seconds ago and the ClusterQueue that was reserving quota is gone — but its sibling still
	// references the flavor, and teardown.go says a flavor's finalizer clears "only once every ClusterQueue
	// that references it is gone". Every, not any. This is where a phase that deleted one ClusterQueue and
	// called the phase settled would look correct right up until the flavor never went away.
	if found, _, fins := kindPresent(ctx, t, c, flavor); !found {
		t.Fatalf("ResourceFlavor %s went away while ClusterQueue %s still references it; the flavor phase would "+
			"then not need to wait for the whole ClusterQueue phase, only for the reserving one", flavor.Name, cqs[1].Name)
	} else {
		t.Logf("ResourceFlavor %s still held by its remaining referencer %s, finalizers=%v",
			flavor.Name, cqs[1].Name, fins)
	}

	sibling, err := deleteObjectFor(cqs[1])
	if err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if err := c.Delete(ctx, sibling); err != nil {
		t.Fatalf("delete the sibling ClusterQueue: %v", err)
	}
	released := time.Now()
	for _, tg := range []target{cqs[1], flavor} {
		tg := tg
		kindWaitFor(ctx, t, 90*time.Second, fmt.Sprintf("%s %s to finish deleting", tg.Kind, tg.Name),
			func() (bool, string) {
				found, terminating, fins := kindPresent(ctx, t, c, tg)
				return !found, fmt.Sprintf("terminating=%v finalizers=%v", terminating, fins)
			})
	}
	t.Logf("ResourceFlavor %s cleared %s after its last referencing ClusterQueue went away",
		flavor.Name, time.Since(released).Round(time.Second))
}

// TestKindTeardownOrderClearsRealFinalizers runs the executor itself, unmodified, against the finalizers.
//
// The fake client's version of this test proves that deleteTargets issues deletes in phase order. It cannot
// prove the order is sufficient, because nothing in the fake ever refuses. Here a residue of zero means every
// Kueue finalizer actually cleared, in the order enumerate declares, inside a real budget.
func TestKindTeardownOrderClearsRealFinalizers(t *testing.T) {
	s := kindSeed()
	c := kindClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() { kindCleanup(t, c, s) })

	fs := kindFixtures(ctx, t, c, s)
	kindReserveQuota(ctx, t, c, s, fs.LocalQueue[0].GetName())

	// The UIDs recovery learns are the apiserver's own, not a value a test double stamped on. Checking one is
	// cheap and it pins the input every later assertion depends on: deleteTarget arms its precondition with
	// WantUID, so a recovery that learned the wrong UID would turn every Delete below into a 409.
	obs, err := recoverTargets(ctx, c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: s.Namespace}, &ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	for _, o := range obs {
		if !o.Found {
			t.Fatalf("recovery did not find %s %s, which this test just created", o.Target.Kind, o.Target.Name)
		}
		if o.WantUID != o.UID || o.WantUID == "" {
			t.Fatalf("%s %s recovered UID=%q WantUID=%q; a stamped object must be adopted under its own UID",
				o.Target.Kind, o.Target.Name, o.UID, o.WantUID)
		}
	}
	if got := observationFor(t, obs, s.Namespace); got.WantUID != string(ns.UID) {
		t.Fatalf("recovery adopted namespace UID %q, apiserver assigned %q", got.WantUID, ns.UID)
	}

	// A budget well under teardownBudget, because what is measured here is whether the order clears at all —
	// not what the production ceiling should be. On this cluster the whole cascade settles in seconds, so a
	// three-minute wait would only make a failure slower to see.
	res, err := deleteTargets(ctx, c, s, s.TxID, time.Now, time.Sleep, 90*time.Second)
	if err != nil {
		t.Fatalf("deleteTargets: %v", err)
	}
	for _, r := range res.Residue {
		t.Errorf("residue: %s %s absence=%d found=%v terminating=%v err=%v",
			r.Observation.Target.Kind, r.Observation.Target.Name, r.Absence,
			r.Observation.Found, r.Observation.Terminating, r.Observation.Err)
	}
	if len(res.Residue) != 0 {
		t.Fatalf("teardown left %d object(s) behind against real Kueue finalizers", len(res.Residue))
	}
	t.Logf("teardown clean in %s (namespace, %d ClusterQueues, 1 ResourceFlavor, one reserving Workload)",
		res.Elapsed.Round(time.Millisecond), len(fs.ClusterQueue))
}

// TestKindDeletePreconditionRefusesAStaleUID is the first execution, ever, of deleteTarget's precondition.
//
// The fake client ignores client.Preconditions entirely — its Delete compares ResourceVersion and nothing
// else — so a Delete carrying a deliberately wrong UID returns nil there and removes the object anyway. Every
// existing test in this package is therefore blind to the mechanism in both directions: a WantUID that was
// always wrong would turn every real teardown into full residue, and a precondition that was never armed
// would let teardown destroy a replacement object it does not own. Neither is visible without an apiserver.
//
// The refusal is the assertion that matters. A test that only checked the correct UID would pass just as well
// against a client that dropped the precondition on the floor.
func TestKindDeletePreconditionRefusesAStaleUID(t *testing.T) {
	s := kindSeed()
	c := kindClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	t.Cleanup(func() { kindCleanup(t, c, s) })

	// The ResourceFlavor ALONE, and the "alone" is load-bearing rather than frugal. Kueue holds a flavor's
	// resource-in-use finalizer for as long as any ClusterQueue references it, so creating the run's full
	// fixture set here would make the correct-UID delete below block on the ordering this test is not about,
	// and the takeover half would never be reached. The other targets simply read NotFound in recovery, which
	// costs nothing: this test asks about identity, not about phases.
	fs, err := queuelab.BuildFixtures(s.Study, s.Variant, queuelab.FixtureIdentity{TxID: s.TxID, RunID: s.RunID, Namespace: s.Namespace})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	if err := createOwned(ctx, c, fs.Flavor, s.TxID); err != nil {
		t.Fatalf("create flavor: %v", err)
	}
	flavor := target{Phase: phaseResourceFlavor, Kind: "ResourceFlavor", Name: fs.Flavor.GetName()}

	obs, err := recoverTargets(ctx, c, s, s.TxID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	ours := observationFor(t, obs, flavor.Name)
	if ours.WantUID == "" {
		t.Fatalf("recovery learned no UID for %s", flavor.Name)
	}

	// A UID that is well-formed and belongs to nothing. This is the shape of the hazard deleteTarget's comment
	// names: between the read that learned the UID and the Delete, the name changed hands.
	stale := ours
	stale.WantUID = "00000000-0000-0000-0000-000000000000"
	err = deleteTarget(ctx, c, stale)
	if err == nil {
		t.Fatalf("a Delete carrying a UID that is not this object's SUCCEEDED; the precondition is not armed, "+
			"and teardown would destroy whatever happened to hold %s at the time", flavor.Name)
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("stale-UID delete refused with %T (%v), expected a Conflict; the executor records the refusal "+
			"verbatim, so the class it arrives in is what a human reads off the residue record", err, err)
	}
	t.Logf("stale-UID delete refused: %v", err)
	if found, _, _ := kindPresent(ctx, t, c, flavor); !found {
		t.Fatalf("%s was removed by a refused delete", flavor.Name)
	}

	// The takeover, reproduced literally rather than simulated with an invented UID: delete the object, let a
	// different one take the name, and try the delete the executor would have had queued. The name matches,
	// the kind matches, and only the UID says it is not ours. Whether a real apiserver enforces THAT is the
	// question the whole absenceForeign path is built on.
	if err := deleteTarget(ctx, c, ours); err != nil {
		t.Fatalf("delete with the correct UID: %v", err)
	}
	kindWaitFor(ctx, t, 60*time.Second, "the flavor to go away", func() (bool, string) {
		found, terminating, fins := kindPresent(ctx, t, c, flavor)
		return !found, fmt.Sprintf("terminating=%v finalizers=%v", terminating, fins)
	})
	t.Log("delete with the correct UID: accepted, object gone")

	replacement := fs.Flavor.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = ""
	replacement.Finalizers = nil
	replacement.Labels[queuelab.TxLabel] = "someone-else"
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatalf("recreate the flavor under a different owner: %v", err)
	}
	if string(replacement.UID) == ours.WantUID {
		t.Fatalf("the replacement was assigned the same UID; the apiserver did not actually reissue one")
	}
	err = deleteTarget(ctx, c, ours)
	if err == nil {
		t.Fatalf("the delete this run had queued destroyed the REPLACEMENT object at %s — the precondition is "+
			"the only thing standing between teardown and another actor's live state, and it did not stand", flavor.Name)
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("takeover delete refused with %v, expected a Conflict", err)
	}
	t.Logf("takeover delete refused (ours UID=%s, replacement UID=%s): %v", ours.WantUID, replacement.UID, err)
	if found, _, _ := kindPresent(ctx, t, c, flavor); !found {
		t.Fatalf("the replacement at %s was removed anyway", flavor.Name)
	}
}

// TestKindStuckNamespaceExpiresToResidue drives a real budget expiry with a real stuck namespace, and checks
// the two things an expiry has to get right.
//
// First, that expiry produces residue that says PRESENT — the executor's whole contract with the next run is
// that a target it could not remove is reported as still there, not as an unknown the next run has to guess
// about. Second, that the phase gate holds on a cluster: with phase 0 stuck, no ClusterQueue may carry a
// deletionTimestamp when the budget runs out. The fake can be made to show the first; only a cluster can
// stall a phase for real while the objects behind it sit there deletable.
//
// The second subtest is the measurement the ledger records as never having been taken: the gap between
// teardownBudget and teardownContextTimeout, with the context on the wrong side of it.
func TestKindStuckNamespaceExpiresToResidue(t *testing.T) {
	s := kindSeed()
	c := kindClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	t.Cleanup(func() { kindCleanup(t, c, s) })

	fs := kindFixtures(ctx, t, c, s)
	kindReserveQuota(ctx, t, c, s, fs.LocalQueue[0].GetName())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ql-stuck", Namespace: s.Namespace,
			Finalizers: []string{kindPodFinalizer},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptrInt64(0),
			Containers: []corev1.Container{{
				Name: "hold", Image: "registry.k8s.io/pause:3.9",
			}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create stuck pod: %v", err)
	}

	targets, err := enumerate(s)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	// Budgets here are deliberately far below teardownBudget. The property is "expiry produces present-residue
	// and holds the later phases shut", and that property does not depend on the number: at three minutes each
	// subtest would cost three real minutes to prove the same thing, and the tests would stop being run.
	t.Run("budget expiry reports present, not unknown", func(t *testing.T) {
		// The context is given the same shape the production pair has — strictly more than the budget — because
		// this subtest exists to show what that gap buys.
		bctx, bcancel := context.WithTimeout(ctx, 60*time.Second)
		defer bcancel()
		res, err := deleteTargets(bctx, c, s, s.TxID, time.Now, time.Sleep, 25*time.Second)
		if err != nil {
			t.Fatalf("deleteTargets: %v", err)
		}
		t.Logf("elapsed=%s residue=%d", res.Elapsed.Round(time.Second), len(res.Residue))
		if len(res.Residue) == 0 {
			t.Fatalf("a namespace held by a Pod finalizer produced no residue")
		}
		nsr := residueFor(t, res.Residue, s.Namespace)
		if nsr.Absence != absencePresent {
			t.Errorf("namespace residue absence=%d, want absencePresent(%d): the next run reads this to decide "+
				"whether anything is still standing, and unknown is a weaker claim than the executor could make",
				nsr.Absence, absencePresent)
		}
		if nsr.Observation.Err != nil {
			t.Errorf("namespace residue carries err=%v; the budget expired, nothing failed", nsr.Observation.Err)
		}
		if !nsr.Observation.Terminating {
			t.Errorf("namespace residue says Terminating=false, but its Delete was accepted and a Pod finalizer is holding it")
		}

		// The phase gate, on a real cluster. Both ClusterQueues are sitting there perfectly deletable — nothing
		// on the apiserver would refuse — and the only reason they are untouched is that the executor never
		// reached phase 1. This is the assertion the fake cannot make, because in the fake the namespace would
		// have gone away and the phase would have advanced.
		//
		// "Perfectly deletable" is measured, not assumed, and it CORRECTS the reason teardown_apply.go gives
		// for its break. That comment says deleting a ClusterQueue while the namespace holding its Workloads is
		// still there blocks on the resource-in-use finalizer. On this cluster it does not: a namespace that is
		// merely Terminating has already had its Workloads reaped — measured at ~6s, with reservingWorkloads
		// dropping 1 -> 0 while the namespace stayed Terminating indefinitely behind a Pod finalizer. What
		// blocks a ClusterQueue is a Workload still RESERVING it, and namespace absence is a sufficient but far
		// from necessary condition for that. The phase order is still SAFE — a namespace that is gone provably
		// has no Workloads — but in exactly the state the gate exists for it is conservatism rather than
		// protection, and the cost is visible in the residue count logged above: four targets reported, of
		// which three were removable at the moment teardown gave up, on a disposition that holds the worker.
		// Left as an observation on purpose; changing what the executor does with it is not this task's job.
		zeroReserving, totalCQ := 0, 0
		for _, tg := range targets {
			if tg.Kind != "ClusterQueue" {
				continue
			}
			totalCQ++
			var cq kueuev1beta2.ClusterQueue
			if err := c.Get(ctx, client.ObjectKey{Name: tg.Name}, &cq); err == nil && cq.Status.ReservingWorkloads == 0 {
				zeroReserving++
			}
		}
		t.Logf("at expiry: %d of the %d ClusterQueues reserve nothing and would have deleted immediately, "+
			"yet all of them are in the residue", zeroReserving, totalCQ)

		for _, tg := range targets {
			if tg.Phase == phaseNamespace {
				continue
			}
			found, terminating, _ := kindPresent(ctx, t, c, tg)
			if !found {
				t.Errorf("%s %s was deleted while phase 0 was still stuck", tg.Kind, tg.Name)
				continue
			}
			if terminating {
				t.Errorf("%s %s carries a deletionTimestamp while the namespace phase never settled; the phase "+
					"gate did not hold", tg.Kind, tg.Name)
			}
			r := residueFor(t, res.Residue, tg.Name)
			if r.Absence != absencePresent {
				t.Errorf("%s %s residue absence=%d, want absencePresent(%d)", tg.Kind, tg.Name, r.Absence, absencePresent)
			}
		}
	})

	t.Run("a context inside the budget collapses residue to unknown", func(t *testing.T) {
		// This is the state teardownContextTimeout's comment says must never happen, run on purpose so the cost
		// is a measurement rather than an argument: the context expires first, every read and delete comes back
		// cancelled, and a teardown that merely ran out of time becomes indistinguishable from one that could
		// not talk to the apiserver at all. Nothing is asserted about the executor being wrong here — it is
		// behaving exactly as designed. What is asserted is that the gap is load-bearing.
		bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
		defer bcancel()
		res, err := deleteTargets(bctx, c, s, s.TxID, time.Now, time.Sleep, 20*time.Second)
		if err != nil {
			t.Fatalf("deleteTargets: %v", err)
		}
		t.Logf("elapsed=%s residue=%d", res.Elapsed.Round(time.Second), len(res.Residue))
		nsr := residueFor(t, res.Residue, s.Namespace)
		if nsr.Absence != absenceUnknown {
			t.Fatalf("with the context expiring before the budget, the namespace residue is absence=%d; the gap "+
				"between teardownBudget and teardownContextTimeout would then buy nothing", nsr.Absence)
		}
		if nsr.Observation.Err == nil {
			t.Fatalf("residue classified unknown but carries no error to explain it")
		}
		t.Logf("context-before-budget residue for the namespace: absence=unknown err=%v", nsr.Observation.Err)
	})
}

// TestKindStrippedNamespaceFinalizerLeavesResidue covers the thing a human does to a stuck namespace.
//
// Removing the `kubernetes` finalizer through the finalize subresource is the standard advice, and it works
// in the sense that the namespace disappears. What it does NOT do is delete the contents: they stay in etcd
// under a namespace that no longer exists, permanently, reachable only by name. The plan's Global Constraints
// forbid the executor from ever printing this as a recovery step, and the risk that makes it worth forbidding
// is that the audit might then read CLEAN over live objects — the namespace target really is NotFound, and a
// teardown that only asked about its own three names could stop there.
//
// So the question is whether the residue is zero. It is not, and the reason is worth stating: the orphaned
// Workload is still in Kueue's cache, still reserving, so the ClusterQueue's resource-in-use finalizer never
// clears and phase 1 never settles. The audit survives this by accident of the phase order rather than by
// looking for orphans, which is exactly the kind of containment the ledger says must not be relied on
// silently — hence this test, which pins it.
func TestKindStrippedNamespaceFinalizerLeavesResidue(t *testing.T) {
	s := kindSeed()
	c := kindClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	t.Cleanup(func() { kindCleanup(t, c, s) })

	fs := kindFixtures(ctx, t, c, s)
	wl := kindReserveQuota(ctx, t, c, s, fs.LocalQueue[0].GetName())

	if err := c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace}}); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	var ns corev1.Namespace
	kindWaitFor(ctx, t, 60*time.Second, "the namespace to start terminating", func() (bool, string) {
		if err := c.Get(ctx, client.ObjectKey{Name: s.Namespace}, &ns); err != nil {
			return false, err.Error()
		}
		return ns.DeletionTimestamp != nil, string(ns.Status.Phase)
	})
	// The finalize subresource, not an Update: the `kubernetes` finalizer lives in spec.finalizers, and the
	// apiserver only accepts a change to it through this endpoint. Reached through SubResource rather than a
	// second clientset because it is a PUT of the namespace body to .../finalize and nothing more.
	ns.Spec.Finalizers = nil
	if err := c.SubResource("finalize").Update(ctx, &ns); err != nil {
		t.Fatalf("strip the namespace finalizer: %v", err)
	}
	kindWaitFor(ctx, t, 60*time.Second, "the namespace to vanish", func() (bool, string) {
		var got corev1.Namespace
		err := c.Get(ctx, client.ObjectKey{Name: s.Namespace}, &got)
		return apierrors.IsNotFound(err), fmt.Sprintf("%v", err)
	})

	// The orphan, stated as a fact rather than assumed: this is the object the standard fix leaves behind, and
	// if the apiserver were garbage-collecting it there would be nothing to hold the ClusterQueue and the rest
	// of this test would prove nothing.
	var orphan kueuev1beta2.Workload
	if err := c.Get(ctx, client.ObjectKeyFromObject(wl), &orphan); err != nil {
		t.Fatalf("the Workload was expected to outlive its namespace, got: %v", err)
	}
	t.Logf("namespace %s is NotFound; Workload %s/%s survives it (uid=%s)",
		s.Namespace, orphan.Namespace, orphan.Name, orphan.UID)

	res, err := deleteTargets(ctx, c, s, s.TxID, time.Now, time.Sleep, 25*time.Second)
	if err != nil {
		t.Fatalf("deleteTargets: %v", err)
	}
	if len(res.Residue) == 0 {
		t.Fatalf("the audit reported CLEAN after a stripped namespace finalizer, while Workload %s/%s is still "+
			"on the cluster reserving quota; a residue of zero over orphaned content is the exact outcome the "+
			"no-strip constraint exists to prevent", orphan.Namespace, orphan.Name)
	}
	var heldCQ bool
	for _, r := range res.Residue {
		t.Logf("residue: %s %s absence=%d found=%v terminating=%v err=%v",
			r.Observation.Target.Kind, r.Observation.Target.Name, r.Absence,
			r.Observation.Found, r.Observation.Terminating, r.Observation.Err)
		if r.Observation.Target.Kind == "Namespace" {
			t.Errorf("the namespace is in the residue, but it really is NotFound; the audit would be claiming " +
				"something the cluster contradicts")
		}
		if r.Observation.Target.Kind != "ClusterQueue" {
			continue
		}
		heldCQ = true
		// Present, not unknown, is the specific claim. A residue that could only say "I do not know" about the
		// object the orphan is holding would leave the next run unable to tell this from an apiserver it could
		// not reach, and this is the case where the difference is the whole point: something IS there.
		if r.Absence != absencePresent {
			t.Errorf("ClusterQueue %s residue absence=%d, want absencePresent(%d)",
				r.Observation.Target.Name, r.Absence, absencePresent)
		}
		if !r.Observation.Terminating {
			t.Errorf("ClusterQueue %s carries no deletionTimestamp; teardown did not even ask", r.Observation.Target.Name)
		}
	}
	if !heldCQ {
		t.Fatalf("the residue names no ClusterQueue, so nothing in it is caused by the orphaned Workload; " +
			"whatever this test proved, it was not that")
	}
}

func ptrInt64(v int64) *int64 { return &v }
