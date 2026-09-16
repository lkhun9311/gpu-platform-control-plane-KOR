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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// acquireAttempts bounds the optimistic-concurrency retry.
//
// A 409 proves only that something wrote the Node — the kubelet, the device plugin and this repository's
// own nodehealth controller all do routinely — so a conflict is retried after re-deciding from the fresh
// Node, and only exhausting the bound refuses.
const acquireAttempts = 5

// resolveAttempts bounds the re-reads that decide whether a patch whose response was lost actually landed.
const resolveAttempts = 6

const resolveInterval = 2 * time.Second

// verifyAttempts bounds the retry on the read that confirms a patch which the API server already reported
// as committed, so one transient Get error immediately afterward is not mistaken for a verification failure
// and does not, on its own, cause the node to be abandoned still carrying our markers.
const verifyAttempts = 3

const verifyInterval = 500 * time.Millisecond

// releaseCleanupTimeout bounds every release-path context that deliberately runs on context.Background()
// instead of the run's own signal-cancellable context.
//
// That choice (see acquireWorker's self-release comment below) protects a half-applied restoration from
// being cut off by Ctrl-C, but an unbounded context would let a stuck API server hang the process forever at
// exactly the moment it is trying to clean up — the very stranded-marker outcome the background context
// exists to prevent, just moved from "Ctrl-C" to "hung apiserver" as the cause.
//
// The worst case this bounds is 13 sequential API calls: releaseAcquired's loop retries up to acquireAttempts
// (5) times on conflict, and a retry that reaches the patch spends 1 Get + 1 Patch, so 5 attempts is at most
// 10 round trips; a successful patch then runs verifyReleased, which reads up to verifyAttempts (3) more
// times, spaced verifyInterval (500ms) apart. 10 + 3 = 13 calls. client-go issues no timeout of its own per
// call, so a degraded-but-still-responding API server is bounded only by this context, not by anything
// smaller — budgeting a generous 10s for each of those 13 calls (plus the ~1s of deterministic verifyInterval
// sleep already inside the 13) is 131s of worst-case sequential work, so 3 minutes leaves real margin above
// that instead of the 30s bound this replaces, which left only about 2.2s per call.
//
// The ambiguous-release read-back added since does not change that 13: it runs in the non-conflict branch,
// which returns instead of looping, and it costs the same up-to-3 reads verifyReleased already cost on the
// success branch. The two branches are mutually exclusive, so the worst case is still 10 + 3.
const releaseCleanupTimeout = 3 * time.Minute

// cleanupContext returns a context bounded by releaseCleanupTimeout for release-path work that must run to
// completion even after the run's own context was cancelled.
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), releaseCleanupTimeout)
}

// operatorModeTimeout bounds the four RECOVERY modes, which run on their own uncancellable context for the
// same reason the release path does: a signal arriving between a mode's Get and its Patch is exactly what
// could leave a break half applied.
//
// It is far shorter than releaseCleanupTimeout because far less work runs on it. Each mode spends at most a
// Get and a Patch on this context — releaseStale's actual restoration hands off to cleanupContext, and
// inspectWorker only reads — so at the same generous 10s-per-call budget releaseCleanupTimeout is derived
// from, 2 calls need 20s and one minute leaves triple that margin. Bounding it at all is the point: an
// operator running these modes is already recovering from a stuck node, and a tool that hangs indefinitely
// against a degraded API server gives them nothing to act on and no way to tell a hang from slow progress.
const operatorModeTimeout = time.Minute

// operatorModeContext returns the bounded, signal-independent context a mode runs on.
//
// The bound is per-mode because one of them is not shaped like the others at all. The four recovery modes
// spend a Get and a Patch and are done; the termination canary starts two containers, waits out a grace
// period and reads what happened, which is minutes of legitimate work that a one-minute bound would cut off
// mid-probe — leaving two Pods holding a finalizer and no verdict to show for it. The reason for making it
// uncancellable is the same for both, and it is stronger for the canary: a signal landing between the delete
// and the finalizer cleanup is exactly what strands a probe.
func operatorModeContext(mode operatorMode) (context.Context, context.CancelFunc) {
	if mode == modeTerminationCanary {
		return context.WithTimeout(context.Background(), canaryModeTimeout)
	}
	if mode == modeDevicePreflight {
		return context.WithTimeout(context.Background(), preflightModeTimeout)
	}
	return context.WithTimeout(context.Background(), operatorModeTimeout)
}

// newTxID generates the ownership identity.
//
// It is generated rather than derived from the run id because a reused run id is already a known confound
// and must never be able to authorise the release of somebody else's transaction.
func newTxID() string { return string(uuid.NewUUID()) }

// acquireWorker takes exclusive ownership of the worker or refuses.
//
// The label, the whole taint array and the journal go in one resource-version-preconditioned patch, so the
// API server never commits a marker without the journal that says who owns it and what to undo.
func acquireWorker(ctx context.Context, c client.Client, nodeName string, id ownerIdentity) (journal, error) {
	var lastErr error
	for range acquireAttempts {
		var n corev1.Node
		if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
			return journal{}, fmt.Errorf("get node %s: %w", nodeName, err)
		}
		obs := observe(&n)
		j, err := decideAcquire(obs, id, time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			// A refusal is a decision about the observed state, so it is returned as-is rather than retried.
			return journal{}, err
		}
		raw, err := encodeJournal(j)
		if err != nil {
			return journal{}, err
		}
		base := n.DeepCopy()
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Labels[workerLabelKey] = j.Installed.LabelValue
		n.Annotations[journalKey] = raw
		n.Spec.Taints = withOwnershipTaint(obs.AllTaints, j.Installed.TaintValue)
		err = c.Patch(ctx, &n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		switch {
		case err == nil:
			if verr := verifyAcquired(ctx, c, nodeName, j); verr != nil {
				// The patch reported success but what we can now observe does not match what it wrote, so
				// this transaction attempts to undo its own markers rather than returning them installed
				// with no error, or returning an error while leaving the node held for the next run to
				// refuse foreign-owner and a human to clear by hand.
				//
				// This self-release must run on a fresh background context, not ctx: a signal cancelling ctx
				// is exactly what can cause verifyAcquired to fail in the first place, and releasing on the
				// same cancelled context would make the Get here fail immediately, skip the release Patch
				// entirely, and leave the label, taint and journal stranded on the node with nothing left to
				// undo them — the failure this task exists to prevent.
				//
				// It is releaseAcquired rather than releaseOwned because this transaction has not been handed
				// to a run yet: finding the node clean here genuinely means there is nothing of ours to undo,
				// where for a run that has been holding the worker it would mean its markers were taken.

				relCtx, relCancel := cleanupContext()
				_, rerr := releaseAcquired(relCtx, c, j)
				relCancel()
				if rerr != nil {
					return journal{}, fmt.Errorf(
						"acquire node %s: verify failed: %v; release also failed, node may still carry tx %s: run: queuelabrun -inspect-worker -worker %s: %w",
						nodeName, verr, j.TxID, nodeName, rerr)
				}
				return journal{}, fmt.Errorf("acquire node %s: verify failed and was undone: %w", nodeName, verr)
			}
			return j, nil
		case apierrors.IsConflict(err):
			// Somebody wrote the Node between the read and the patch; re-read and re-decide from scratch.
			lastErr = err
			continue
		default:
			// The patch may still have landed with its response lost, so the outcome is resolved rather
			// than assumed.
			return resolveAmbiguousAcquire(ctx, c, nodeName, j, err)
		}
	}
	return journal{}, fmt.Errorf("acquire node %s: %d conflicts, giving up: %w", nodeName, acquireAttempts, lastErr)
}

// verifyAcquired requires the complete invariant before anything else touches the cluster.
//
// Finding our transaction id is not enough: a mutating webhook can keep the annotation and alter a marker,
// which would leave the run holding a node that no longer routes work the way the flavor expects.
func verifyAcquired(ctx context.Context, c client.Client, nodeName string, j journal) error {
	var n corev1.Node
	var err error
	for attempt := range verifyAttempts {
		if err = c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err == nil {
			break
		}
		if attempt == verifyAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			// Bare ctx.Err() here would drop the node name and the txID that identify what is still on the
			// node for the caller's self-release (see acquireWorker) to act on, the same detail every other
			// cancellation branch in this file now carries.
			return fmt.Errorf("verify node %s cancelled: tx %s: %w", nodeName, j.TxID, ctx.Err())
		case <-time.After(verifyInterval):
		}
	}
	if err != nil {
		return fmt.Errorf("verify node %s: %w", nodeName, err)
	}
	if verr := verifyObserved(observe(&n), j); verr != nil {
		return fmt.Errorf("verify node %s: %w", nodeName, verr)
	}
	return nil
}

// verifyObserved is the whole "did our write land, completely" invariant, in one place.
//
// resolveAmbiguousAcquire used to carry its own inlined copy of this check; two copies of the invariant is
// two places it can drift, and the one thing the design forbids is treating a journal carrying our TxID as
// acquired without also proving the markers are the ones we installed.
func verifyObserved(obs ownership, j journal) error {
	if obs.JournalErr != nil {
		return obs.JournalErr
	}
	if obs.Journal != j {
		return fmt.Errorf("journal is not the one this run wrote")
	}
	return verifyInstalled(obs, j)
}

// verifyReleased requires proof our markers are gone before release is trusted, in the same bounded-retry
// shape as verifyAcquired.
func verifyReleased(ctx context.Context, c client.Client, nodeName string, j journal) (ownership, error) {
	var n corev1.Node
	var err error
	for attempt := range verifyAttempts {
		if err = c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err == nil {
			break
		}
		if attempt == verifyAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ownership{}, fmt.Errorf("verify release of node %s cancelled: tx %s: %w", nodeName, j.TxID, ctx.Err())
		case <-time.After(verifyInterval):
		}
	}
	if err != nil {
		return ownership{}, fmt.Errorf("verify release of node %s: %w", nodeName, err)
	}
	obs := observe(&n)
	// The observation is returned, not just the verdict, because "our markers are gone" and "the node is
	// free" are different facts and one caller below needs to tell them apart. verifyClean deliberately
	// answers only the first.
	return obs, verifyClean(obs, j)
}

// verifyClean is release's counterpart to verifyObserved: it proves THIS transaction's markers are gone,
// never that the node is free.
//
// Between our patch and this read, another run may legitimately have acquired the node, and demanding a free
// node would turn that ordinary race into a spurious restoration failure. So each check only fails when what
// is observed still names our own installed values, not merely when something is present.
func verifyClean(obs ownership, j journal) error {
	if obs.NodeUID != j.NodeUID {
		// A recreated node is a different node: there is nothing of ours left to find on it, but that is not
		// the invariant this function proves, so treat it as unable to verify rather than silently passing.
		return fmt.Errorf("node UID is %s, the journal named %s: this is a different node", obs.NodeUID, j.NodeUID)
	}
	if obs.JournalRaw != "" {
		switch {
		case obs.JournalErr != nil:
			// The invariant is "absent, or present but naming a different txID" — an unreadable journal
			// satisfies neither, so it cannot be waved through as clean. This cannot false-fail the legitimate
			// race either: a foreign journal is written by encodeJournal inside one atomic patch, so there is
			// no torn read of someone else's write to tolerate here, only ours failing to have been removed.
			return fmt.Errorf("journal on node %s is unreadable after release: %v", obs.NodeName, obs.JournalErr)
		case obs.Journal.TxID == j.TxID:
			return fmt.Errorf("journal for tx %s is still on the node after release", j.TxID)
		default:
			// A journal that decodes and names a DIFFERENT transaction settles the question by itself, and the
			// marker comparisons below must not run against it.
			//
			// Those comparisons match on the run-id-derived values, and a foreign transaction that happens to
			// carry the same run id installs byte-identical ones — so a legitimate acquisition after our
			// release would look like our own markers surviving, and the run would be invalidated by a value
			// coincidence. The journal's txID is the authority precisely because it cannot be defeated that
			// way: acquisition proceeds only from a free node and writes label, taint and journal in one
			// atomic patch, so a foreign journal being present is proof that ours, and the markers it was
			// written with, were gone by the time that patch was accepted.
			return nil
		}
	}
	if obs.HasLabel && obs.LabelValue == j.Installed.LabelValue {
		return fmt.Errorf("label %s still carries this transaction's value %q", workerLabelKey, obs.LabelValue)
	}
	for _, t := range obs.Taints {
		if t.Value == j.Installed.TaintValue && t.Effect == j.Installed.TaintEffect {
			return fmt.Errorf("taint %s still carries this transaction's value %q/%s",
				workerTaintKey, t.Value, t.Effect)
		}
	}
	return nil
}

// resolveWindow is how long the node must look free before a lost write is called dead.
//
// It is stated as its own value because it is what the refusal quotes and what the argument below turns on,
// not merely the product of two loop bounds — and it counts the GAPS between reads, not the reads. The first
// read is immediate, so six reads spaced two seconds apart span ten seconds, and quoting twelve would claim
// two seconds nothing observed. That trailing gap is not a rounding detail: it is exactly where the late
// commit this whole path exists to catch would land unseen, which is why the loop below does not sleep after
// its last read.
const resolveWindow = time.Duration(resolveAttempts-1) * resolveInterval

// resolveAmbiguousAcquire decides whether a patch whose response was lost actually committed.
//
// Three outcomes only: our complete tuple is observed and the run continues; the node is free or foreign
// and the run refuses with nothing of ours to undo; or it is unresolved within the bound, which refuses and
// leaves the operator modes to sort it out.
//
// A free node is the outcome that must be earned rather than read once. A linearizable read orders this Get
// against writes that have COMPLETED, and the write in question is exactly the one that did not: a request
// that timed out or lost its connection may still be sitting in the API server's apply path and commit after
// this read returns. Concluding "did not land" from a single free observation therefore discards the
// transaction id at the one moment it is still needed, and nothing afterwards can name the markers that
// appear a moment later. So the free state has to hold for the whole window before it is believed, and
// anything else — a read that failed, a state that resolved neither way — leaves the outcome unresolved,
// which keeps the txID and the operator's next command.
func resolveAmbiguousAcquire(ctx context.Context, c client.Client, nodeName string, j journal,
	cause error) (journal, error) {
	// The failed re-read is kept rather than discarded, because "we could not read the node" and "we read it
	// and it never resolved" send the operator to different places. A sustained authorization failure, a
	// deleted node or an API outage would otherwise be reported as a generic unresolved write, and the very
	// -inspect-worker command this refusal prints would fail for the same reason nobody was told about.
	//
	// It tracks the MOST RECENT read, not the worst one ever seen, which is why the success branch clears it:
	// one transient failure early in the loop followed by five clean reads is a node the operator can reach,
	// and reporting the stale error would send them after a connectivity problem that has already passed.
	var lastReadErr error
	// freeThroughout stays true only while EVERY observation in the window has succeeded and found the node
	// free. A failed read or any non-free state clears it for good, so the "did not land" conclusion below
	// rests on an unbroken window rather than on whichever state the last read happened to catch.
	freeThroughout := true
	for attempt := range resolveAttempts {
		var n corev1.Node
		if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
			lastReadErr = err
			freeThroughout = false
		} else {
			lastReadErr = nil
			obs := observe(&n)
			switch {
			case verifyObserved(obs, j) == nil:
				// The write landed after all, which is the outcome a single free read would have thrown away.
				return j, nil
			case obs.JournalErr == nil && obs.JournalRaw != "" && obs.Journal.TxID != j.TxID:
				// Another transaction holds the node, which it could only have acquired from a free one: our
				// write is provably not going to appear underneath it.
				return journal{}, fmt.Errorf("acquire node %s failed; it is now held by tx %s: %w",
					nodeName, obs.Journal.TxID, cause)
			case obs.JournalRaw == "" && !obs.HasLabel && len(obs.Taints) == 0:
				// Free, so far. controller-runtime reads straight from the API server rather than a cache, so
				// this Get cannot observe a state older than a write that has already COMPLETED — but the write
				// this is resolving is one whose completion is exactly what is unknown, so a single free
				// observation is not evidence of anything yet. Keep watching.
			default:
				// Partly ours, or ours diverged: resolved neither way.
				freeThroughout = false
			}
		}
		if attempt == resolveAttempts-1 {
			// The last read closes the window. Sleeping after it would put resolveInterval of unwatched time
			// inside the span the refusal below quotes, and a write committing in that gap is precisely the
			// outcome this loop exists to notice.
			break
		}
		select {
		case <-ctx.Done():
			// acquireWorker now runs on the signal-cancellable context, so a Ctrl-C during this retry loop
			// must not drop the txID and the inspect-worker hint the bound-exhaustion path below carries.
			return journal{}, fmt.Errorf(
				"acquire node %s is UNRESOLVED after cancellation: it may hold tx %s. Run: queuelabrun -inspect-worker -worker %s. %s. Cause: %w",
				nodeName, j.TxID, nodeName, resolveReadNote(lastReadErr), ctx.Err())
		case <-time.After(resolveInterval):
		}
	}
	if freeThroughout {
		return journal{}, fmt.Errorf(
			"acquire node %s failed and did not land; it was observed free on every re-read across %v and does not hold tx %s. Run: queuelabrun -inspect-worker -worker %s. Cause: %w",
			nodeName, resolveWindow, j.TxID, nodeName, cause)
	}
	return journal{}, fmt.Errorf(
		"acquire node %s is UNRESOLVED after %v: it may hold tx %s. Run: queuelabrun -inspect-worker -worker %s. %s. Cause: %w",
		nodeName, resolveWindow, j.TxID, nodeName,
		resolveReadNote(lastReadErr), cause)
}

// resolveReadNote renders what the LAST re-read reported, so the refusal distinguishes a node the operator
// cannot reach from a readable one that simply never showed a resolving state.
//
// It is the last read rather than any read, because that is the state the operator is about to walk into.
// A nil error here therefore means the most recent read succeeded, which is the fact worth stating: without
// it a reader assumes the reads must have failed because the outcome is unknown.
func resolveReadNote(err error) string {
	if err == nil {
		return "The last re-read succeeded and did not resolve the state"
	}
	return "Last re-read failed: " + err.Error()
}

// inspectWorker is read-only and is how an operator learns a transaction id a crashed process never
// printed anywhere.
func inspectWorker(ctx context.Context, c client.Client, nodeName string) error {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	obs := observe(&n)
	fmt.Printf("node %s (uid %s)\n", obs.NodeName, obs.NodeUID)
	fmt.Printf("  label %s = %q (present=%v)\n", workerLabelKey, obs.LabelValue, obs.HasLabel)
	fmt.Printf("  taints on %s: %s\n", workerTaintKey, quotedTaints(obs.Taints))
	fmt.Printf("  journal: %s\n", quotedOrNone(obs.JournalRaw))
	fmt.Printf("  quarantine: %s\n", quotedOrNone(obs.QuarantineRaw))
	fmt.Printf("  residue: %s\n", quotedOrNone(obs.ResidueRaw))
	// Printed above the switch rather than inside the held branch, because the held branch is not the only
	// state that can carry one: forceQuarantine deliberately preserves the record, and the moment an operator
	// is breaking a hold by hand is exactly when they need to know why it was held. A record that survived a
	// hand-stripped label reaches the stale-marker and free branches the same way, and saying so is better than
	// printing a raw document nobody reads.
	printResidueDetail(obs)
	switch {
	case obs.QuarantineRaw != "":
		q, err := decodeQuarantine(obs.QuarantineRaw)
		if err != nil {
			// A script wrapping -inspect-worker must see a non-zero exit here: an unreadable quarantine
			// record needs a human, and printing the warning while still exiting 0 would read as healthy.
			//
			// It is a named refusal rather than a bare error for the reason the reason constants exist at all:
			// this is a state, and a state that only exists as a sentence cannot be classified by anything
			// that reads it.
			fmt.Printf("\nUNREADABLE QUARANTINE RECORD: %v — manual intervention required.\n", err)
			return refuseCause(err, reasonBadQuarantine, "node %s: %v", nodeName, err)
		}
		fmt.Printf("\nQUARANTINED. To free it after establishing the previous process is dead:\n"+
			"  queuelabrun -clear-quarantine -worker %s -quarantine-id %q -confirm-owner-dead\n",
			nodeName, q.QuarantineID)
	case obs.JournalRaw != "" && obs.JournalErr != nil:
		// Without this branch a node carrying an undecodable journal and no marker fell through to FREE and
		// exited 0, while decideAcquire refuses that same node as unreadable-journal: the recovery tool
		// contradicted the runner about the node's state, and said the safe-sounding thing. A script reading
		// the exit code would then keep pointing runs at a node no run can ever take.
		//
		// It is not recoverable by the ordinary path either: -release-stale needs a txID, and the txID lives
		// in the very document that cannot be read, so the break is the only way through.
		fmt.Printf("\nUNREADABLE OWNERSHIP JOURNAL: %v\n"+
			"  found: %s\n"+
			"  No run can acquire this node (acquisition refuses it as %s) and -release-stale cannot free it,\n"+
			"  because the transaction id it would need is inside this document. Break it with:\n"+
			"    queuelabrun -force-release -worker %s -node-uid %s -accept-divergence\n",
			obs.JournalErr, quotedOrNone(obs.JournalRaw), reasonBadJournal, nodeName, obs.NodeUID)
		return fmt.Errorf("node %s: unreadable ownership journal: %w", nodeName, obs.JournalErr)
	case obs.JournalRaw != "" && obs.JournalErr == nil:
		// Every one of these came out of a Node annotation, so they are quoted for the same reason the raw
		// document is: decoding a hostile string does not make it safe, and the txID in particular is printed
		// straight into a command the operator is being invited to copy.
		fmt.Printf("\nHELD by run %q (arm %q) under tx %q since %q.\n",
			obs.Journal.RunID, obs.Journal.Arm, obs.Journal.TxID, obs.Journal.TakenAt)
		// Recovered from the journal's own fields, which exist so this question is answerable from the NODE.
		// A crash after acquisition leaves objects behind, and an operator holding only a node name previously
		// had nothing telling them where to look.
		printRecoverable(obs.Journal)
		switch {
		case obs.ResidueErr != nil:
			// An unreadable record still changes the advice, because what it fails to say is not "there is
			// nothing here" — the annotation's presence is itself evidence a teardown ended without removing
			// everything, and the ordinary -release-stale line printed under that would be the same unsafe
			// suggestion as below, just with nothing on screen to warn against it. It stays a printed warning
			// rather than a returned error, unlike the unreadable QUARANTINE record above: this document decides
			// nothing, and an informational field that invents a new failure mode is worse than no field at all.
			fmt.Printf("  It also carries a residue record this tool could not read (see above), so it cannot\n" +
				"  say what was left behind. Assume the hold is deliberate until you have established otherwise:\n" +
				"  a run whose teardown finished removes this annotation on its way out.\n")
			printResidueRelease(nodeName, obs.Journal.TxID)
		case obs.ResidueRaw != "":
			// -release-stale is the WRONG move here and is deliberately not offered unconditionally. This hold
			// is not a crashed process's leftovers: the run that installed these markers decided to keep the
			// worker because its own teardown could not prove those objects gone. Releasing strips the
			// dedication label and the NoSchedule taint from a node whose namespace may still be running that
			// run's GPU Pods — and takes the record explaining it along too, since releaseAcquired clears the
			// annotation with the markers.
			fmt.Printf("  This hold is DELIBERATE. That run's teardown could not remove the objects listed\n" +
				"  under residue: above, so the label and the taint are still installed to keep other work off\n" +
				"  GPUs that may still be busy. -release-stale is NOT the move: it strips both markers from a\n" +
				"  node whose namespace may still be running that run's Pods, and deletes the record on the way.\n" +
				"  do NOT strip a stuck namespace's finalizer: that orphans its contents, and every absence\n" +
				"  check afterwards reports clean over objects that are still running.\n" +
				"  Get those objects deleted first; the worker is only free once they are actually gone.\n")
			printResidueRelease(nodeName, obs.Journal.TxID)
		default:
			fmt.Printf("  If that process is gone: queuelabrun -release-stale -worker %s -txid %q -confirm-owner-dead\n",
				nodeName, obs.Journal.TxID)
		}
		if err := verifyInstalled(obs, obs.Journal); err != nil {
			fmt.Printf("  WARNING, the installed values have diverged: %v\n"+
				"  A stale release will refuse. The break is:\n"+
				"    queuelabrun -force-release -worker %s -node-uid %s -accept-divergence\n",
				err, nodeName, obs.NodeUID)
		}
	case obs.HasLabel || len(obs.Taints) > 0:
		fmt.Printf("\nSTALE MARKER WITH NO JOURNAL. This tool cannot release it safely.\n"+
			"  Inspect and decide by hand, or break it with:\n"+
			"    queuelabrun -force-release -worker %s -node-uid %s -accept-divergence\n",
			nodeName, obs.NodeUID)
	default:
		fmt.Printf("\nFREE.\n")
	}
	return nil
}

// printResidueDetail says what a previous teardown left, in the form a human can act on.
//
// The raw line above it is the document; this is what the document MEANS, and an operator recovering a stuck
// worker at 3am should not have to read JSON out of a terminal to learn that a namespace is still standing.
// It prints nothing at all when there is no record, so the only thing an ordinary inspection gains is the
// `residue: (none)` fact line above, which is the same shape the journal and the quarantine record already
// have there.
//
// Every decoded field is escaped for the same reason residueNote escapes them and quotedTaints escapes a taint
// value: all of it came out of a Node annotation, and it is printed a few lines above commands this tool is
// inviting the operator to copy.
func printResidueDetail(obs ownership) {
	if obs.ResidueRaw == "" {
		return
	}
	if obs.ResidueErr != nil {
		// Named, not skipped. Staying silent here would leave the operator with only the raw line above and no
		// indication that this tool tried to read it and failed — the same reason residueNote reports an
		// unreadable record rather than behaving as though there were none.
		fmt.Printf("    UNREADABLE: %v\n", obs.ResidueErr)
		return
	}
	fmt.Printf("    left by run %q under tx %q at %q, %d object(s):\n",
		obs.Residue.RunID, obs.Residue.TxID, obs.Residue.LeftAt, len(obs.Residue.Left))
	for _, l := range obs.Residue.Left {
		fmt.Printf("      %q %q (%q)\n", l.Kind, l.Name, l.Absence)
	}
	if obs.Residue.RecordPath != "" {
		// The path is an invitation to read more, never the payload: it names a file on the machine that ran,
		// which is very often not the machine this inspection is running on.
		fmt.Printf("    full record: %q\n", obs.Residue.RecordPath)
	}
}

// printResidueRelease prints the release command under the condition that makes it safe.
//
// The command is still printed in full, because every other hint this tool gives is a complete runnable one
// and an operator reconstructing it from the flag list under pressure is how the wrong node gets released. It
// is the CONDITION that changes: a residue hold ends when the objects are gone, not when the process is.
func printResidueRelease(nodeName, txID string) {
	fmt.Printf("  Once those objects are gone AND the process holding this transaction is dead:\n"+
		"    queuelabrun -release-stale -worker %s -txid %q -confirm-owner-dead\n", nodeName, txID)
}

// quotedOrNone renders an annotation this tool did not write.
//
// The journal and the quarantine record are Node annotations, so anyone who can write the Node can put
// arbitrary bytes in them, and every caller here prints them into an operator's terminal while that operator
// is deciding whether to break a node. %q escapes the terminal control sequences that could otherwise
// rewrite the recovery instructions printed around them, and it also makes trailing whitespace or an empty
// document visible instead of silently absent.
func quotedOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return fmt.Sprintf("%q", s)
}

// quotedTaints renders taints field by field with every value escaped.
//
// %+v was reaching the terminal raw, and a taint value is node-controlled in exactly the way the annotations
// are: anyone who can taint the worker can put terminal control sequences in the value, and this line is
// printed directly above the recovery instructions an operator is about to act on. The key and effect are
// escaped too rather than trusted, because nothing here has validated them against the API server's rules.
func quotedTaints(taints []corev1.Taint) string {
	if len(taints) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(taints))
	for _, t := range taints {
		parts = append(parts, fmt.Sprintf("{key:%q value:%q effect:%q}", t.Key, t.Value, string(t.Effect)))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// releaseStale is the ordinary recovery: the transaction is identified, its values are intact, and the
// operator has established the process holding it is gone.
func releaseStale(ctx context.Context, c client.Client, nodeName, txID string) error {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	obs := observe(&n)
	act, err := decideRelease(obs, txID)
	if err != nil {
		return err
	}
	if act == releaseAlreadyDone {
		// obs.Journal is the zero journal here (its Node field is ""), because decideRelease only reaches
		// releaseAlreadyDone when there was never a journal to decode in the first place. Passing that zero
		// journal to releaseAcquired would Get an empty node name and fail with a confusing API error, for an
		// operator who reached for this exact mode right after a crash and deserves a plain "already released"
		// instead.
		fmt.Printf("node %s already carries nothing for tx %s; nothing to release\n", nodeName, txID)
		return nil
	}
	// The taint value and effect are decoded out of the node's own journal, so they are escaped like every
	// other node-controlled value this tool prints.
	fmt.Printf("restoring node %s: removing label %s=%q and taint %s=%q/%q, and the journal for tx %s\n",
		nodeName, workerLabelKey, obs.LabelValue, workerTaintKey,
		obs.Journal.Installed.TaintValue, string(obs.Journal.Installed.TaintEffect), txID)
	// releaseAcquired deletes the residue record in the same patch that removes the markers, so this is the
	// last moment anyone can see it. Saying so here is not a duplicate of inspectWorker's warning: that
	// warning lives in a different command's output, and this command is the one inspectWorker tells the
	// operator to run. An operator who skimmed the prose and copied the last line would otherwise destroy the
	// only record of what was left and be told only that a label, a taint and a journal went away.
	//
	// It warns rather than refuses because this mode exists for the case where the previous process is gone
	// and the operator has attested to it; refusing would leave them with no move at all. The fields are
	// escaped for the same reason every other node-controlled value here is.
	switch {
	case obs.ResidueErr != nil:
		fmt.Printf("  WARNING: this also deletes a residue record that could not be read: %v\n", obs.ResidueErr)
	case obs.ResidueRaw != "":
		// "residue record" verbatim, because that is what inspectWorker labels it and what sent the operator
		// here; a synonym would make the two outputs look like they are about different things.
		fmt.Printf("  WARNING: this also deletes the residue record naming %d object(s) that run's teardown "+
			"could not remove:\n", len(obs.Residue.Left))
		for _, l := range obs.Residue.Left {
			fmt.Printf("    %q %q (%q)\n", l.Kind, l.Name, l.Absence)
		}
		fmt.Printf("  deleting the record does not delete those objects; confirm they are gone before you " +
			"trust this node as free\n")
	}
	// releaseAcquired always runs on a bounded, uncancelled context, the same as every other release call
	// site in this package: a half-applied restoration must be allowed to finish even if the operator's own
	// signal handling (if any wraps ctx) fires mid-patch, but the wait still cannot be indefinite.
	//
	// It is releaseAcquired rather than releaseOwned because this recovery holds nothing: the read above and
	// the read inside can race a genuine release, and finding the node already clean by then is the outcome
	// the operator wanted, not a lost transaction.
	relCtx, relCancel := cleanupContext()
	defer relCancel()
	_, err = releaseAcquired(relCtx, c, obs.Journal)
	return err
}

// forceQuarantine breaks a stuck node without ever producing a free one.
//
// It cannot prove the previous process is dead, so it records everything it removes and leaves a record
// that acquisition refuses until an operator clears it in a separate, deliberate step.
func forceQuarantine(ctx context.Context, c client.Client, nodeName, nodeUID string) error {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	obs := observe(&n)
	q, err := decideForce(obs, nodeUID, string(uuid.NewUUID()), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	raw, err := encodeQuarantine(q)
	if err != nil {
		return err
	}
	fmt.Printf("forcing node %s: removing label %s=%q, %d taint(s) on %s, and the journal %s\n"+
		"  all of it is preserved in quarantine record %s\n",
		nodeName, workerLabelKey, obs.LabelValue, len(obs.Taints), workerTaintKey,
		quotedOrNone(obs.JournalRaw), q.QuarantineID)
	base := n.DeepCopy()
	delete(n.Labels, workerLabelKey)
	delete(n.Annotations, journalKey)
	// The residue record is deliberately left in place. It is the explanation an operator most needs at the
	// moment they are breaking a hold by hand, and it cannot mislead while it sits here: decideAcquire
	// refuses a quarantined node on QuarantineRaw before it ever reaches the foreign-owner branch that quotes
	// it. clearQuarantine is what removes it. Copying it into the quarantine record instead would mean
	// bumping quarantineSchema, and that record is decoded with DisallowUnknownFields, so every older binary
	// would then refuse every newer record.
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[quarantineKey] = raw
	n.Spec.Taints = withoutOwnershipTaint(obs.AllTaints)
	if err := c.Patch(ctx, &n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("force node %s (state changed under you, re-inspect): %w", nodeName, err)
	}
	// Every other hint this tool prints is a complete runnable command, and the operator who has just broken
	// a node is exactly the one who should not have to reconstruct the second step from the flag list.
	fmt.Printf("node %s is QUARANTINED as %s; no run can acquire it until it is cleared.\n"+
		"  Once you have established the previous process is dead:\n"+
		"    queuelabrun -clear-quarantine -worker %s -quarantine-id %s -confirm-owner-dead\n",
		nodeName, q.QuarantineID, nodeName, q.QuarantineID)
	return nil
}

// clearQuarantine removes only the record the operator names, after they have attested the previous owner
// is dead — a judgement no flag can make for them.
func clearQuarantine(ctx context.Context, c client.Client, nodeName, quarantineID string) error {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &n); err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	obs := observe(&n)
	if err := decideClear(obs, quarantineID); err != nil {
		return err
	}
	base := n.DeepCopy()
	delete(n.Annotations, quarantineKey)
	// The quarantine is the state that outlived the run; clearing it deliberately is where its explanation
	// ends too.
	delete(n.Annotations, residueKey)
	if err := c.Patch(ctx, &n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("clear quarantine on %s (state changed under you, re-inspect): %w", nodeName, err)
	}
	fmt.Printf("node %s quarantine %s cleared\n", nodeName, quarantineID)
	return nil
}

// releaseAcquired undoes exactly what this transaction installed, or refuses and invalidates the run.
//
// It reports which of the two release actions it carried out, because "there was nothing of mine to undo"
// means something different to each caller: to the operator modes and to acquire's self-release it is a
// legitimate success, while to the run that holds the worker it is proof its markers were taken from it.
// releaseOwned below is the caller that draws that distinction.
func releaseAcquired(ctx context.Context, c client.Client, j journal) (releaseAction, error) {
	var lastErr error
	for range acquireAttempts {
		var n corev1.Node
		if err := c.Get(ctx, client.ObjectKey{Name: j.Node}, &n); err != nil {
			return releaseRestore, fmt.Errorf("get node %s: %w", j.Node, err)
		}
		obs := observe(&n)
		act, err := decideRelease(obs, j.TxID)
		if err != nil {
			return act, err
		}
		if act == releaseAlreadyDone {
			return act, nil
		}
		base := n.DeepCopy()
		delete(n.Labels, workerLabelKey)
		delete(n.Annotations, journalKey)
		// The explanation goes with the thing it explains: once the markers are off, nothing refuses on this
		// node, so a surviving record would be quoted by no refusal and read by a human as current.
		delete(n.Annotations, residueKey)
		n.Spec.Taints = withoutOwnershipTaint(obs.AllTaints)
		err = c.Patch(ctx, &n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		switch {
		case err == nil:
			// acquireWorker has verifyAcquired for exactly this reason: a status code proves the API server
			// accepted the request, not that what it committed is what a subsequent reader will see. Release
			// now follows the same discipline and reads back before letting the caller trust restoration.
			if _, verr := verifyReleased(ctx, c, j.Node, j); verr != nil {
				return releaseRestore, fmt.Errorf("release node %s: patch succeeded but restoration did not verify: %w",
					j.Node, verr)
			}
			return releaseRestore, nil
		case apierrors.IsConflict(err):
			// Retrying is legal here because release re-verifies identity on every attempt and is not
			// racing anyone for exclusivity.
			lastErr = err
			continue
		default:
			// Acquisition has resolveAmbiguousAcquire for exactly this class of error and release had no
			// counterpart, so a non-conflict Patch failure whose write actually landed — a proxy timeout, a
			// connection reset after commit — made the run print "worker not restored" and exit non-zero for a
			// node that was already clean. That is a false invalidation, and the lab pays for those in runs.
			//
			// The direction of failure stays safe because only a positive proof passes: verifyReleased must
			// observe our label gone, our taint value gone, our journal gone and the same node UID, and it
			// fails closed on an unreadable journal or a Get it cannot complete. It deliberately tolerates
			// another transaction having acquired in the meantime, which is verifyClean's whole contract and
			// is an ordinary race rather than a restoration failure.
			//
			// The 409 argument that used to end this comment does not hold, and it is worth spelling out why
			// rather than deleting quietly. It ran: a concurrent -release-stale or -force-release would have
			// moved the resourceVersion, the optimistic lock would have conflicted, and we would be in the
			// IsConflict arm instead — so a clean read-back here is our own write. That inference needs the
			// request to have REACHED the apiserver, and this arm exists precisely for the case where it may
			// not have. If the patch never landed there was no opportunity for a 409, so the absence of one
			// proves nothing about who removed our markers.
			//
			// So the read-back is split by what it finds, which lets the two comments above compose instead of
			// contradicting each other. A node with no journal at all is the case this arm was written for:
			// nobody holds it, restoration is real however it happened, and invalidating would be the false
			// invalidation the lab pays for in runs. A node held by ANOTHER transaction is not that case. It
			// is either our write landing and somebody acquiring after, or our write vanishing and somebody
			// force-releasing us mid-run, and nothing on the node distinguishes them — which is the definition
			// releaseOwned already gives for invalidating: the run lost exclusive use somewhere it cannot
			// bound. Reporting a clean release there would be the "looked fine, was allowed to count" outcome
			// the whole transaction exists to stop.
			obs, verr := verifyReleased(ctx, c, j.Node, j)
			if verr != nil {
				return releaseRestore, fmt.Errorf(
					"release node %s: %w (the read-back could not prove restoration either: %v)", j.Node, err, verr)
			}
			if obs.JournalRaw != "" {
				return releaseRestore, refuse(reasonOwnershipLost,
					"worker %s: the release patch failed with %v and the read-back finds this run's markers gone "+
						"but the node held by another transaction; whether that patch landed cannot be told from "+
						"here, so the point at which tx %s stopped holding the node exclusively is unbounded",
					j.Node, err, j.TxID)
			}
			return releaseRestore, nil
		}
	}
	return releaseRestore, fmt.Errorf("release node %s: %d conflicts, giving up: %w",
		j.Node, acquireAttempts, lastErr)
}

// stampResidue records on the worker itself why its markers are still installed.
//
// The record CONTAINS nothing. The dedication label and the NoSchedule taint are what keep Pods off the node,
// and an annotation the scheduler does not understand cannot hold a GPU Pod back for a moment — the same trap
// forceQuarantine's comment names. What this buys is that the next operator, refused this worker, is told
// which objects a finished run could not remove instead of being handed a transaction id alone.
//
// It re-reads and re-checks ownership with verifyObserved before writing, for the same reason releaseOwned
// does: a status code proves the API server accepted a request, not that the node is still the one this
// transaction acquired. Between teardown and this call another actor can have taken it over, and stamping our
// residue onto their markers is the same lie the UID preconditions in teardown_apply.go exist to prevent.
//
// verifyObserved rather than verifyInstalled alone, because verifyInstalled cannot make that promise. It
// compares the label value, the taint value and effect, and the node UID — and both marker values are the RUN
// id, not the transaction id, so a node re-acquired by a DIFFERENT transaction under the same run id carries
// byte-identical markers and would pass. A reused run id is the confound the txID exists to defeat (see
// newTxID), and verifyClean's comment already names this exact value coincidence as the reason the journal's
// txID is the authority. decideRelease closes it by checking obs.Journal.TxID before it calls verifyInstalled;
// verifyObserved is the same check plus the whole tuple, so this path gets it by reusing rather than repeating.
func stampResidue(ctx context.Context, c client.Client, j journal, left []residue, leftAt, recordPath string) error {
	// Defense in depth, not a live defect: no caller can currently reach this. tearDownBeforeRelease's one
	// hold with an empty residue — a teardown that could not compute its target set at all — returns before the
	// stamp branch, and the surviving call site is past a `len(result.Residue) == 0` return. The guard stays
	// because of what it would cost to be wrong: decodeResidue refuses an empty Left, so a record naming
	// nothing would reach the next operator only as "a residue record that could not be read", which is worse
	// than the bare refusal they would otherwise get.
	if len(left) == 0 {
		return fmt.Errorf("stamp residue on %s: nothing was left to name", j.Node)
	}
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: j.Node}, &n); err != nil {
		return fmt.Errorf("get node %s: %w", j.Node, err)
	}
	obs := observe(&n)
	if err := verifyObserved(obs, j); err != nil {
		return fmt.Errorf("stamp residue on %s: %w", j.Node, err)
	}
	rec := residueRecord{
		Schema: residueSchema, TxID: j.TxID, RunID: j.RunID, LeftAt: leftAt, RecordPath: recordPath,
	}
	for _, r := range left {
		rec.Left = append(rec.Left, residueLeft{
			Kind: r.Observation.Target.Kind,
			Name: r.Observation.Target.Name,
			// absenceName, never a literal: the run record spells its verdicts with the same function, and two
			// spellings of one verdict is exactly the disagreement between accounts this program refuses to emit.
			Absence: absenceName(r.Absence),
		})
	}
	raw, err := encodeResidue(rec)
	if err != nil {
		return err
	}
	base := n.DeepCopy()
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[residueKey] = raw
	if err := c.Patch(ctx, &n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("stamp residue on %s (state changed under you, re-inspect): %w", j.Node, err)
	}
	return nil
}

// releaseOwned is the release of a worker this process is still holding, and it is the only release for
// which a free node is a failure.
//
// decideRelease cannot make this call on its own: reading a node with no journal and no markers, it cannot
// tell "already released" from "released out from under a live run", and both operator recovery and
// acquire's own self-release legitimately meet the first. The run does know — it installed those markers
// and never removed them — so the distinction is drawn here, at the one call site that has that knowledge.
//
// A free worker at this point means someone ran -release-stale or the force/clear break-glass pair against
// a live transaction, or the node was deleted and recreated. In every one of those the run lost exclusive
// use of the GPU somewhere it cannot bound, which is exactly the "looked fine, was allowed to count"
// failure the whole transaction exists to stop, so it invalidates rather than reporting a clean release.
func releaseOwned(ctx context.Context, c client.Client, j journal) error {
	act, err := releaseAcquired(ctx, c, j)
	if err != nil {
		return err
	}
	if act == releaseAlreadyDone {
		return refuse(reasonOwnershipLost,
			"worker %s no longer carries this run's markers; ownership was lost mid-run under tx %s",
			j.Node, j.TxID)
	}
	return nil
}

// printRecoverable names what the holder of this worker left on the cluster, by the route its kind recovers
// through.
//
// The two are genuinely different and printing one list for both is what produced the defect this replaces.
// A run's objects regenerate through the fixture builder from study, variant and namespace. The canary builds
// no fixtures: it makes two Pods named from its id, inside a namespace it shares with every other canary — so
// the namespace is listed as context and explicitly marked as not-to-delete, because an operator reading a
// deletion list top to bottom is exactly who would remove it.
func printRecoverable(j journal) {
	switch j.Kind {
	case ownerRun:
		targets, err := enumerate(seedFromJournal(j))
		if err != nil {
			// Reported rather than swallowed: a journal that decodes but cannot rebuild a deletion set is a
			// state worth naming, and the operator would otherwise see a HELD node with no explanation of why
			// the object list is missing.
			fmt.Printf("  Its objects could not be regenerated from this journal: %v\n", err)
			return
		}
		fmt.Printf("  That run's objects, regenerated from this journal:\n")
		for _, t := range targets {
			fmt.Printf("    %s %s\n", t.Kind, t.Name)
		}
	case ownerCanary:
		honor, ignore := canaryProbeSpecs(j.CanaryID, canaryContract{})
		fmt.Printf("  That canary's Pods, in namespace %s:\n", j.Namespace)
		fmt.Printf("    Pod %s\n", honor.name)
		fmt.Printf("    Pod %s\n", ignore.name)
		fmt.Printf("  The namespace is SHARED by every canary and must not be deleted.\n")
	}
}
