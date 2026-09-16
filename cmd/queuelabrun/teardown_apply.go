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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// emptyObjectFor returns the zero-value object Get should decode a target's Kind into. It errors on a kind
// enumerate never returns rather than falling back to some default: a new target kind added to enumerate
// without a matching case here must fail loudly, not silently read into the wrong type.
func emptyObjectFor(tg target) (client.Object, error) {
	switch tg.Kind {
	case kindNamespace:
		return &corev1.Namespace{}, nil
	case kindClusterQueue:
		return &kueuev1beta2.ClusterQueue{}, nil
	case kindResourceFlavor:
		return &kueuev1beta2.ResourceFlavor{}, nil
	default:
		return nil, fmt.Errorf("recover: no reader registered for target kind %q", tg.Kind)
	}
}

// recoverTargets re-reads every target enumerate names and learns each one's UID from what is actually on
// the cluster, rather than accepting a UID as input. A caller that could hand in UIDs could hand in stale or
// invented ones, which is exactly the residue-printer failure mode this pass exists to rule out: after a
// partial create, the only trustworthy source for "what did THIS run actually make" is a read of the name it
// used, checked against the stamp it wrote at Create.
//
// Ownership is decided here, once, by that stamp — the same test ensureNamespace applies at create time. An
// object found under this run's name but stamped by a different transaction (or not stamped at all) is
// refused, and the refusal is that TARGET's alone: it becomes an observation marked Foreign, which
// classifyAbsence reads as absenceForeign, so no Delete is ever issued against it, it does not hold its phase
// open, and it survives into the residue for the record to name.
//
// Refusing the whole batch instead is what this used to do, and it inverted the design's own guarantee. The
// state that triggers it is the ordinary rerun — only the namespace is ever cleaned up by hand, so a
// cluster-scoped fixture from a previous attempt under the same run id is the common leftover — and the
// result was a teardown that issued not one Delete, left the run's OWN namespace on the cluster, and returned
// an error rather than a residue, so the record named nothing while a namespace was still running.
//
// What the old comment here argued was narrower and is still true, and is why the fact is carried explicitly
// rather than expressed as a UID: absenceForeign's other route is a UID comparison against a UID this run
// recorded for an object it created, and there is no such UID for an object it never created. Inventing one
// would be exactly the lie this design refuses to tell — and worse than a lie in a report, because WantUID is
// what deleteTarget arms its precondition with. So WantUID stays empty and observation.Foreign carries the
// verdict. The UID comparison keeps the case a create-time stamp cannot see: our object deleted and a
// different one recreated under our name between this pass and a later poll.
//
// It still returns an error, but only for a caller or seed bug — a txID that disagrees with the seed, a seed
// enumerate refuses, a target kind with no reader. None of those are cluster state, and after this change no
// cluster state reaches the caller as an error at all.
func recoverTargets(ctx context.Context, c client.Client, s seed, txID string) ([]observation, error) {
	// txID is a caller-supplied parameter, s.TxID is the durable record this run wrote at Create; nothing
	// stops a caller from passing the two out of sync. Left unguarded, txID == "" would match an unstamped
	// object's absent label (both compare equal to ""), collapsing the unstamped-leftover refusal below, and
	// any other txID would let the caller adopt whatever transaction it names instead of this seed's own. The
	// same reasoning that makes enumerate refuse an empty s.TxID at teardown.go:77 applies here — an
	// unguarded value does not mean "recover less," it means "recover under a different, wrong transaction."
	if txID != s.TxID {
		return nil, fmt.Errorf("recover: txID %q does not match the seed's own TxID %q", txID, s.TxID)
	}
	targets, err := enumerate(s)
	if err != nil {
		return nil, err
	}
	out := make([]observation, 0, len(targets))
	for _, tg := range targets {
		obj, err := emptyObjectFor(tg)
		if err != nil {
			return nil, err
		}
		// Every path below appends exactly once, the foreign one included — it continues only AFTER appending.
		// Leaving a target unobserved — on a read error especially — would drop it out of the audit entirely,
		// and "no residue" would then read as clean while the object is still there. That is the batch-level
		// form of unclassified-reads-as-absence, and no coverage check placed afterwards can see it, because
		// the missing observation was never made.
		gerr := c.Get(ctx, client.ObjectKey{Name: tg.Name}, obj)
		switch {
		case apierrors.IsNotFound(gerr):
			out = append(out, observation{Target: tg})
		case gerr != nil:
			out = append(out, observation{Target: tg, Err: gerr})
		default:
			uid := string(obj.GetUID())
			if got := obj.GetLabels()[queuelab.TxLabel]; got != txID {
				// The observed UID is carried so the record can name WHICH object holds the name, and WantUID
				// is deliberately left empty: this run recorded no UID here, and the empty one is what keeps
				// the delete precondition unarmed even if the gate above it were ever widened.
				out = append(out, observation{Target: tg, Found: true, UID: uid, Foreign: true})
				continue
			}
			out = append(out, observation{
				Target: tg, Found: true, UID: uid, WantUID: uid,
				Terminating: obj.GetDeletionTimestamp() != nil,
			})
		}
	}
	return out, nil
}

// teardownPollInterval is how long the executor waits between rounds of re-reads.
//
// It is a constant rather than a knob because the caller already controls the only input that changes the
// outcome — the budget. A namespace deletion on a real cluster settles in seconds, gated by kube-controller
// -manager's own cleanup, so a tighter interval would add API calls without finishing any sooner.
const teardownPollInterval = 2 * time.Second

// teardownResult is what one teardown attempt proved.
//
// Residue is a result, not a failure to compute one: a run whose budget expired with objects still present
// has established a fact the next run must refuse to start on, and returning it as an error would collapse
// that fact back into "teardown failed" with nothing left to persist.
type teardownResult struct {
	Residue []residue // empty means proven clean
	Elapsed time.Duration
}

// deleteObjectFor builds the object a Delete is issued against: this target's kind and name, and nothing
// else, so no stale spec read earlier in the run can travel into the delete.
//
// It carries no TypeMeta. The typed client resolves the kind from the scheme, not from the object body, so
// stamping a GroupVersionKind here would change nothing on the wire — anything that needs to name what was
// deleted resolves the kind the same way the client does.
func deleteObjectFor(tg target) (client.Object, error) {
	obj, err := emptyObjectFor(tg)
	if err != nil {
		return nil, err
	}
	obj.SetName(tg.Name)
	return obj, nil
}

// deleteTarget issues the one Delete this run is entitled to issue for a target.
//
// The UID precondition is not a nicety. Between the read that learned the UID and this call, another actor
// can delete the name and recreate it, and an unconditioned delete-by-name would then destroy the
// replacement — an object this run never created and has no authority over. The precondition makes the
// apiserver refuse instead of leaving that outcome to timing. o.WantUID is non-empty by construction here:
// classifyAbsence returns absenceUnknown when it is empty, so a target with no recovered UID never reaches a
// delete at all.
//
// WantUID and UID are equal at every call site today, because the only classification that reaches a delete
// is absencePresent and that classification requires them to match. WantUID is still the right field to read:
// it is what this run recorded, so it stays correct if the gate below it ever widens, whereas o.UID is
// whatever the last read happened to see.
func deleteTarget(ctx context.Context, c client.Client, o observation) error {
	obj, err := deleteObjectFor(o.Target)
	if err != nil {
		return err
	}
	uid := types.UID(o.WantUID)
	return c.Delete(ctx, obj, client.Preconditions{UID: &uid})
}

// observeTarget re-reads one target and reports what that read found, against the UID recovery established.
//
// It deliberately does not re-check the transaction stamp: ownership was decided once, by recoverTargets, and
// the question during polling is the narrower one — is the object we established still the object at this
// name. classifyAbsence answers that from the UID alone, which is exactly what catches our object being
// deleted and a different one recreated under our name mid-teardown, stamped or not.
func observeTarget(ctx context.Context, c client.Client, tg target, wantUID string) observation {
	obj, err := emptyObjectFor(tg)
	if err != nil {
		return observation{Target: tg, WantUID: wantUID, Err: err}
	}
	switch gerr := c.Get(ctx, client.ObjectKey{Name: tg.Name}, obj); {
	case apierrors.IsNotFound(gerr):
		return observation{Target: tg, WantUID: wantUID}
	case gerr != nil:
		return observation{Target: tg, WantUID: wantUID, Err: gerr}
	default:
		return observation{
			Target: tg, Found: true, UID: string(obj.GetUID()), WantUID: wantUID,
			Terminating: obj.GetDeletionTimestamp() != nil,
		}
	}
}

// phasesIn returns the distinct phases the observations carry, ascending.
//
// The order comes from the phase constants rather than from the order the observations happen to arrive in,
// so a target list that reached here shuffled still deletes the namespace first; and a phase added to
// enumerate later is picked up here without a second ordered list to keep in sync with teardown.go.
func phasesIn(obs []observation) []teardownPhase {
	seen := map[teardownPhase]bool{}
	var out []teardownPhase
	for _, o := range obs {
		if seen[o.Target.Phase] {
			continue
		}
		seen[o.Target.Phase] = true
		out = append(out, o.Target.Phase)
	}
	slices.Sort(out)
	return out
}

// phaseTargetSettled reports whether a target has stopped holding its phase open.
//
// Absent is the only thing that proves the object is gone. Foreign settles the phase too, but for the
// opposite reason: the name is held by an object under a different UID, so ours is no longer there, and
// continuing to poll would be waiting on a deletion this run does not control — the outcome teardown.go
// refuses when it declines to report a foreign object as present. It is not evidence of a clean teardown
// either, so residual keeps it in the residue; it simply stops holding the next phase hostage.
func phaseTargetSettled(o observation) bool {
	switch classifyAbsence(o, o.WantUID) {
	case absenceAbsent, absenceForeign:
		return true
	default:
		return false
	}
}

// settlePhase deletes one phase's targets and then polls them to absence, updating their observations in
// place so the caller always holds the freshest read of every target. It reports whether the phase settled;
// false means the budget ran out with something still there.
//
// A Delete that the apiserver refused is recorded in deleteErrs, keyed by the target itself rather than by
// its name: kind is part of what identifies a target, and a name-only key would cross-wire one kind's refusal
// onto another kind's residue the moment enumerate returns two kinds under one name. It is not reachable
// today — the three names enumerate computes cannot collide — but the map has no reason to know that. The read that follows in the same round is better evidence of whether the object is
// still there, and would overwrite anything written onto the observation before any caller could see it —
// but the read says nothing about WHY the object survived, and "the apiserver refused" is the single most
// useful thing a residue record can carry. deleteTargets folds these back in at the end, for targets that
// never settled.
func settlePhase(ctx context.Context, c client.Client, latest []observation, phase teardownPhase,
	now func() time.Time, sleep func(time.Duration), deadline time.Time, deleteErrs map[target]error) bool {
	for {
		for i := range latest {
			o := &latest[i]
			if o.Target.Phase != phase {
				continue
			}
			// Terminating is what separates "deletion already requested, keep polling" from "issue the
			// Delete". Re-issuing against an object that already carries a deletionTimestamp buys nothing —
			// the request was accepted and a finalizer, not a missing Delete, is what holds it. Re-issuing
			// against one that is present and NOT terminating is the opposite: evidence the previous attempt
			// did not take, which is the case worth another try.
			if classifyAbsence(*o, o.WantUID) != absencePresent || o.Terminating {
				continue
			}
			err := deleteTarget(ctx, c, *o)
			switch {
			case err == nil || apierrors.IsNotFound(err):
				// Already gone is not a failure, and a successful request clears any earlier refusal so a
				// transient one cannot haunt a target that did in the end come away.
				//
				// This clearing is defended twice — here, and by the settled check on the fold-back in
				// deleteTargets — so neither line alone can be shown red by a test: removing either leaves the
				// other holding the property. TestDeleteClearsARefusalOnceTheRetrySucceeds removes both at once,
				// which is the only mutation that separates the behaviour from its absence. Kept deliberately
				// rather than reduced to one: a refusal that has been superseded is stale data, and the cheapest
				// place to stop carrying it is where it is superseded.
				delete(deleteErrs, o.Target)
			default:
				// A Forbidden here is teardown discovering that this run's delete authority was never
				// verified, and that is a fact to report, not a reason to crash and abandon the rest of the
				// set. It is held aside rather than written onto the observation so it neither gets erased by
				// this round's own read nor stops the next round from trying the Delete again.
				deleteErrs[o.Target] = err
			}
		}

		// Read after deleting, so the loop always exits on evidence rather than on the assumption that an
		// accepted Delete completed. An accepted Delete is a request; only a read proves the result.
		for i := range latest {
			o := &latest[i]
			if o.Target.Phase != phase || phaseTargetSettled(*o) {
				continue
			}
			*o = observeTarget(ctx, c, o.Target, o.WantUID)
		}

		done := true
		for _, o := range latest {
			if o.Target.Phase == phase && !phaseTargetSettled(o) {
				done = false
				break
			}
		}
		if done {
			return true
		}
		// The budget is checked here, against the injected clock, and never against ctx. A context deadline
		// would surface expiry as an ordinary cancellation indistinguishable from the caller giving up, and
		// expiry is the one path that must report residue.
		if !now().Before(deadline) {
			return false
		}
		sleep(teardownPollInterval)
	}
}

// deleteTargets deletes everything this run created, in the order Kueue's finalizers allow, and reports what
// would not go away.
//
// The deletion set comes only from enumerate via recoverTargets — no List, no label selector, no DeleteAllOf.
// A selector deletes whatever currently matches it, which is not the same set as "what THIS run created": a
// concurrent run's objects can match, and the seed exists precisely to remove that ambiguity.
func deleteTargets(ctx context.Context, c client.Client, s seed, txID string,
	now func() time.Time, sleep func(time.Duration), budget time.Duration) (teardownResult, error) {
	start := now()
	deadline := start.Add(budget)

	// Ownership is decided once, here, by recoverTargets — which marks an object stamped by another
	// transaction foreign rather than adopting it. Nothing below re-derives it, so there is exactly one place
	// that can be wrong about whose objects these are. An error from it is a caller or seed bug, never cluster
	// state: cluster state comes back as observations, including the ones this run may not touch.
	latest, err := recoverTargets(ctx, c, s, txID)
	if err != nil {
		return teardownResult{}, err
	}

	deleteErrs := map[target]error{}
	for _, phase := range phasesIn(latest) {
		if !settlePhase(ctx, c, latest, phase, now, sleep, deadline, deleteErrs) {
			// A phase that did not settle stops teardown here rather than advancing. An out-of-order delete
			// does not fail loudly — measured against a real apiserver, the Delete returned nil in 7ms and
			// the ClusterQueue was still there twenty seconds later, terminating, holding
			// kueue.x-k8s.io/resource-in-use. Pressing on would bury which object is actually stuck behind a
			// row of accepted deletes that changed nothing.
			//
			// Note what this costs, because the kind test measured it and enumerate's comment argues it: a
			// namespace can sit Terminating long after Kueue has reaped the Workloads that were reserving,
			// so a phase held open here can be holding targets that would delete immediately. Stopping
			// reports them as residue, and residue holds the worker. That is the accepted price of gating on
			// a fact this run can read for itself rather than on Kueue's view of who reserves what.
			break
		}
	}

	// Fold the refusals back in, last, and only onto targets that never settled. A residue record saying
	// "still present" and nothing else reads as a slow finalizer, so the next run refuses to start with no
	// clue why; carrying the refusal makes the difference between "waiting" and "not allowed" legible. It is
	// applied here rather than during the loop because a target that did eventually come away has nothing to
	// explain.
	//
	// It lands on DeleteRefusal rather than on Err, and that is the whole correction. Err is the read's own
	// failure, so classifyAbsence answers absenceUnknown for it; a refusal carried there downgraded a target
	// this run had positively OBSERVED present to "nobody could tell", and the record then said
	// absence:"unknown" next to found:true. The verdict is evidence the run actually has, and explaining why
	// the object could not be removed must not cost it.
	for i := range latest {
		o := &latest[i]
		if o.Err == nil && !phaseTargetSettled(*o) {
			if derr := deleteErrs[o.Target]; derr != nil {
				o.DeleteRefusal = derr
			}
		}
	}
	return teardownResult{Residue: residual(latest), Elapsed: now().Sub(start)}, nil
}
