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
	"io"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The ownership transaction proves the worker was ours at two instants and says nothing about the interval
// between them. acquireWorker installs the markers and verifyAcquired reads them back; releaseOwned's
// decideRelease reads them once more on the way out. Nothing looks in between — so a third party can strip
// the NoSchedule taint, let other work land, and put the taint back before release, and every check this
// program performs still passes while the measured schedule ran on a shared machine. A Node deleted and
// recreated under the same name is the same shape: a new UID that only the two endpoints would notice.
//
// This file closes that window. It watches the worker Node continuously from a baseline taken before the run
// creates anything, compares every version it sees against the tuple the journal says was installed, and
// audits what the Node looked like on either side of the release.

// windowViolationCap bounds how many violations the record carries.
//
// A node whose markers are being fought over can produce an event per write, and a record with ten thousand
// near-identical entries is one nobody reads. The COUNT is kept in full regardless (see
// ownershipWindow.ViolationsObserved), so the cap loses detail and never loses the fact.
const windowViolationCap = 20

// The two window verdicts that are not one of ownership.go's refusal reasons.
//
// They are constants for the same reason those are: the record persists them and a reader classifies on
// them, so a reworded string would silently become a different verdict. Everything else this file can
// conclude — a diverged marker, a replaced node — is verifyInstalled's own verdict and is carried under
// verifyInstalled's own name rather than under a second vocabulary for the same fact.
const (
	// reasonWorkerDeleted is the Node itself going away under a run that holds it. It is separate from a
	// diverged marker because no marker survives the object, and an operator reading "the label changed" for a
	// deleted node would go looking for a mutating webhook that was never involved.
	reasonWorkerDeleted = "worker-node-deleted"
	// reasonWindowLost is the view ending before the run closed it. It is a violation rather than a warning
	// because what it costs is the claim itself: from that point on nothing was watching, so exclusivity for
	// the rest of the window is unobserved rather than established, and this gate exists to stop absence of
	// evidence being read as evidence of absence.
	reasonWindowLost = "ownership-view-lost"
)

// ownershipViolation is one moment at which the worker stopped being provably this run's.
//
// The observed marker state travels with it because the reason alone does not tell an operator what to fix:
// "the taint is gone" and "the taint carries another run's value" are the same verdict from verifyInstalled
// and completely different situations on the cluster.
type ownershipViolation struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// HasLabel and ObservedLabel are the label as it stood, and the bool is not redundant: an absent label and
	// one present with an empty value are different cluster states that would both persist as "".
	HasLabel      bool   `json:"hasLabel"`
	ObservedLabel string `json:"observedLabel"`
	// ObservedTaints is the ownership-key taints, rendered by quotedTaints for the reason that function
	// exists: a taint value is node-controlled, this text is printed to a terminal beside recovery commands,
	// and decoding it out of the cluster does not make it safe to splice into instructions.
	ObservedTaints string `json:"observedTaints"`
	NodeUID        string `json:"nodeUID,omitempty"`
}

// nodeMarkers is one side of the restoration audit: what the Node carried at a single read.
//
// Unrelated taints are carried by KEY and sorted, because they are what the audit is actually about. The
// release patch replaces spec.taints wholesale, so a bug in withoutOwnershipTaint would delete the
// operator's own taints as a side effect of restoring ours, and an audit that recorded only our own markers
// would report that as a clean release.
type nodeMarkers struct {
	// Observed says the read behind this side succeeded. A false one with a zero everything else is why the
	// audit has an Unavailable field rather than silently describing a Node nobody read.
	Observed   bool   `json:"observed"`
	NodeUID    string `json:"nodeUID,omitempty"`
	HasLabel   bool   `json:"hasLabel"`
	LabelValue string `json:"labelValue,omitempty"`
	// OwnershipTaintValues is the value of each taint on the ownership key, which is what the comparison
	// turns on: the release removes the key, so a value of ours still standing here is a marker that did not
	// come off. It is values rather than a rendered taint because the audit has to compare the two sides, and
	// a formatted string is something to parse rather than something to check.
	OwnershipTaintValues []string `json:"ownershipTaintValues,omitempty"`
	OtherTaintKeys       []string `json:"otherTaintKeys,omitempty"`
	HasJournal           bool     `json:"hasJournal"`
}

// restorationAudit is what the Node looked like on either side of this run's release, and what changed.
//
// The release already proves its own markers came off — verifyReleased does that and its verdict is what
// decides the run. What was missing is the comparison itself: an assertion that something was restored,
// with no record of the before and after, is not an audit. This is the record.
type restorationAudit struct {
	Before nodeMarkers `json:"before"`
	After  nodeMarkers `json:"after"`
	// OurMarkersRemoved is the audit's own reading of the two sides, deliberately computed here rather than
	// copied from the release's verdict: two accounts derived from one source agree by construction and
	// establish nothing, and the point of reading the Node again is to have a second look at it.
	OurMarkersRemoved bool `json:"ourMarkersRemoved"`
	// Drift names unrelated taints that did not survive the release, which is damage to somebody else's
	// cluster state rather than a failure of this run's restoration. It is recorded and never invalidating:
	// it happens after the measurement, so it cannot bear on the number, and a run refused for it would be a
	// run refused for something that could not have touched what it measured.
	Drift []string `json:"drift,omitempty"`
	// Unavailable is why a side could not be read. The audit is evidence, not a gate, so a read that failed
	// says so and leaves the release's own verdict standing.
	Unavailable string `json:"unavailable,omitempty"`
}

// ownershipWindow is the run's evidence that its worker stayed its own for the whole measurement.
//
// It is persisted whether the window held or not, the same way the qualification block is, and the refusal
// path is again the more valuable of the two: a run invalidated for a stripped taint is precisely the run
// whose evidence would otherwise be a line on somebody's terminal.
//
// READ ViolationsObserved, NOT THE RECORD'S REASON, to tell an exclusivity failure from an observation
// failure. Both land on the collector-desync disposition — deliberately, since builder.Err() is the one
// thing that decides whether a number may exist — but LedgerBuilder.Desync keeps the FIRST reason it is
// given, and this verdict is folded in after the streams have been joined. So on a run where a collector
// stream also desynced, the record's `reason` names that stream and the exclusivity failure survives only
// here. `disposition == collector-desync && window.violationsObserved > 0` is the discriminator that holds
// in both orders; the reason text is the human's account of whichever came first.
type ownershipWindow struct {
	Node    string `json:"node"`
	NodeUID string `json:"nodeUID"`
	TxID    string `json:"txID"`
	// BaselineResourceVersion is the point the continuous view resumes from, and it is the field that says
	// this is a window rather than a sample: a watch started at "now" cannot speak for the interval before it
	// attached, and that interval is where the fixtures are created and the first rows are submitted.
	BaselineResourceVersion string `json:"baselineResourceVersion"`
	OpenedAt                string `json:"openedAt"`
	// ClosedAt is where the covered interval ends, and it does not mean the same thing on every run.
	//
	// A run that reached the horizon closes its window there, before teardown and before the release, because
	// a view open across a release would record that release as a violation. A run that failed BEFORE the
	// horizon closes it after teardown instead, which can be three minutes later — so on those records the
	// window covers a stretch during which no measurement was happening, and a third party mutating the node
	// (or the view dying) in it is recorded here as a violation with nothing to mark it as post-run. It cannot
	// change a verdict: the ledger is only ever told about this window at the horizon, which those runs never
	// reached, and they already carry a failing disposition of their own. Read it as noise in the record of a
	// run that measured nothing, not as evidence about a measurement.
	ClosedAt string `json:"closedAt,omitempty"`
	// NodeVersionsObserved is the denominator for an empty Violations list, for the reason PodsOnNode is the
	// denominator for an empty GPUConsumers list: "nothing deviated" from a view that observed nothing is
	// indistinguishable from a view pointed at the wrong object. The opening read is one of these, so a window
	// that held is always at least 1 rather than 0.
	NodeVersionsObserved int `json:"nodeVersionsObserved"`
	// Ending is what could be observed about how the view stopped, in endText's spelling for a lost one.
	Ending string `json:"ending"`
	// ViolationsObserved is the total; Violations is capped at windowViolationCap.
	ViolationsObserved int                  `json:"violationsObserved"`
	Violations         []ownershipViolation `json:"violations,omitempty"`
	Restoration        *restorationAudit    `json:"restoration,omitempty"`
	// NeverOpened marks a window that exists only to carry a restoration audit.
	//
	// There is one state where a worker was acquired and no view over it could be established: the sentinel
	// failed to start. The run is refused, but the node has already been labelled and tainted, so a
	// restoration audit exists and is the only evidence of whether those markers came off.
	//
	// Attaching it needed somewhere to attach it TO. Without this, the audit was dropped whenever the window
	// was nil, and clusterRestored — which asks for proof only where a window exists — then reported
	// containment held on the strength of no evidence at all. A synthesized window is the smaller lie than a
	// silently discarded audit, and this field is what keeps it from being read as a view that was held:
	// every other field on it is zero, and NodeVersionsObserved of 0 already fails exclusivityHeld.
	NeverOpened bool `json:"neverOpened,omitempty"`
}

// ownershipSentinel is the continuous view of one Node for the length of a run.
//
// It is a separate stream rather than a fifth kind on the collector because their windows are different: the
// collector opens at t0 and closes at the horizon, while exclusivity has to be provable from before the
// first Create until the release, and folding a Node into the collector would either shorten this window to
// the collector's or lengthen the collector's censoring boundary to this one.
type ownershipSentinel struct {
	j      journal
	stream *watchStream

	mu     sync.Mutex
	window ownershipWindow

	stopOnce sync.Once
	done     chan struct{}
}

// startOwnershipSentinel opens the window and takes its first reading.
//
// The order is the point. The stream's baseline List fixes the resume version FIRST, so every change from
// that version onward is delivered; the Get that follows is then free of a gap of its own, because anything
// that happened between the list and the read is already on its way through the watch. Reading first and
// listing second would leave exactly the hole this gate exists to close.
//
// What it cannot close is the sliver between acquireWorker's own verification and this baseline, and the
// bound on it is narrower than "anything could happen in there". The opening read compares against the
// journal like every other version, so a strip STILL IN EFFECT when the window opens is caught; what is
// invisible is only a complete strip-and-restore that begins and ends inside the sliver. Nothing short of
// resuming from the resourceVersion of the acquiring patch itself would see that.
//
// The caller keeps the sliver small by where it calls this: run() opens the window immediately after
// acquireWorker rather than after qualifyWorker, whose unfiltered cluster-wide Pod List is the most
// expensive call in setup and would otherwise sit inside the gap. What remains is one Get-plus-List against
// the apiserver. The window this proves is [baseline, close], and acquire's own verify is what stands
// before it.
func startOwnershipSentinel(ctx context.Context, c client.WithWatch, j journal) (*ownershipSentinel, error) {
	stream, err := startWatchStream(ctx, c, clusterScope(), func() client.ObjectList { return &corev1.NodeList{} })
	if err != nil {
		return nil, fmt.Errorf("open the continuous ownership view of node %s: %w", j.Node, err)
	}
	s := &ownershipSentinel{
		j:      j,
		stream: stream,
		done:   make(chan struct{}),
		window: ownershipWindow{
			Node:                    j.Node,
			NodeUID:                 j.NodeUID,
			TxID:                    j.TxID,
			BaselineResourceVersion: stream.Baseline().ResourceVersion,
			OpenedAt:                nowStamp(),
		},
	}

	// The consumer starts before the opening read, not after it, because Close joins this goroutine: a
	// failure below has to be able to shut the stream down, and a Close that waited on a goroutine nobody had
	// launched would hang the run at the exact moment it was trying to refuse.
	go s.consume()

	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: j.Node}, &n); err != nil {
		// Refused rather than noted. The opening reading is what makes the first stretch of the window an
		// observation instead of an assumption, and a run that could not read its own worker at the moment it
		// starts measuring has not established the premise it is about to measure under — the same reason
		// qualifyWorker refuses a Pod list it could not complete.
		s.Close()
		return nil, fmt.Errorf("read node %s to open the ownership window: %w", j.Node, err)
	}
	s.observe(&n, watch.Modified)

	// The view must be ATTACHED before the run creates anything, not merely requested, and the three arms are
	// collector.awaitEstablished's for the same reasons. startWatchStream returns a nil error before its first
	// watch has necessarily succeeded, and a permanent pre-establishment failure — a Forbidden on nodes, the
	// realistic one here, since this is the only cluster-scoped watch the runner opens — ends the stream having
	// established nothing. Without this arm the run would submit its trace, spend its whole window, and refuse
	// at the horizon over an RBAC gap that was knowable in the first second.
	//
	// The wait costs no observation time at all, which is what separates this from the collector's budget: the
	// collector is not built yet, so t0 is unstamped and the horizon has not started.
	select {
	case <-s.stream.Established():
	case <-s.stream.End():
		// "before the run could rely on it" rather than "before it ever established", because a stream that
		// established and died in the same instant makes both arms ready and the choice between them is the
		// runtime's — the same ambiguity collector.awaitEstablished documents. Both refuse: this one refuses
		// here, and the other lets consume record the lost view a moment later. Only the wording could be
		// wrong, so it claims the thing that is true either way.
		end := s.stream.Ended()
		s.Close()
		return nil, fmt.Errorf("the continuous view of node %s ended before the run could rely on it: %s",
			j.Node, endText(end))
	case <-time.After(establishBudget):
		s.Close()
		return nil, fmt.Errorf("the continuous view of node %s did not establish a watch within %s, and a run "+
			"that measures under an unestablished watch cannot say the worker stayed its own", j.Node, establishBudget)
	case <-ctx.Done():
		s.Close()
		return nil, fmt.Errorf("opening the continuous view of node %s: %w", j.Node, ctx.Err())
	}
	return s, nil
}

// nowStamp is the one spelling of a timestamp this evidence uses, matching the journal's and the record's.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// consume folds every Node version the stream delivers, then decides what the ending meant.
//
// The ending rule is the collector's, deliberately: an ending the caller did not cause is a lost view, and a
// forwarded terminal status is a loss the stream actually witnessed even alongside a cancellation. Anything
// else would be a second, weaker reading of streamEnd's own asymmetry living in this package twice.
func (s *ownershipSentinel) consume() {
	defer close(s.done)
	for ev := range s.stream.ResultChan() {
		switch ev.Type {
		case watch.Error:
			// No verdict here, for the reason collector.consume gives: RetryWatcher forwards nothing for the
			// errors it retries, so an Error that arrives is one it has given up on and forward has already kept
			// the status as the ending's evidence. The ending below is what decides.
			continue
		}
		// There is deliberately no watch.Bookmark arm here, and the absence is worth a line because a bookmark
		// looks like it would be dangerous: it carries a resource version on an otherwise empty object, so
		// comparing one against the journal would read as a stripped label.
		//
		// It cannot arrive. RetryWatcher consumes bookmarks to advance its own resume version and never
		// forwards them — client-go v0.36.1, tools/watch/retrywatcher.go: `if event.Type != watch.Bookmark {
		// ok = rw.send(ctx, event) }` — so nothing downstream of it, this consumer or the collector's four,
		// ever sees one. The collector has no such arm for the same reason. A guard here would be code with no
		// caller wearing a comment about a mechanism nobody had measured, which is the exact defect this
		// lineage keeps finding.
		n, ok := ev.Object.(*corev1.Node)
		if !ok {
			continue
		}
		if n.Name != s.j.Node {
			// The scope is every Node in the cluster (see clusterScope), so most of what arrives here is another
			// machine changing and says nothing about this run's worker.
			continue
		}
		s.observe(n, ev.Type)
	}
	end := s.stream.Ended()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case end.LastStatus != nil:
		s.window.Ending = fmt.Sprintf("lost: %s", statusText(end.LastStatus))
		s.recordLocked(ownershipViolation{
			At: nowStamp(), Reason: reasonWindowLost,
			Detail: fmt.Sprintf("the continuous view of node %s ended on a %s, so exclusivity after that point "+
				"is unobserved rather than established", s.j.Node, statusText(end.LastStatus)),
		})
	case !end.Cancelled && !end.Stopped:
		s.window.Ending = fmt.Sprintf("lost: %s", endText(end))
		s.recordLocked(ownershipViolation{
			At: nowStamp(), Reason: reasonWindowLost,
			Detail: fmt.Sprintf("the continuous view of node %s ended on its own while the run still held it "+
				"(%s), so exclusivity after that point is unobserved rather than established", s.j.Node, endText(end)),
		})
	case end.Stopped:
		s.window.Ending = "closed by the run"
	default:
		s.window.Ending = "cancelled with the run"
	}
}

// observe compares one Node version against the tuple this transaction installed.
//
// The comparison is verifyInstalled's and nothing else, which is what keeps benign drift benign: unrelated
// labels and taints — the operator's own unhealthy taint above all — are not this run's markers and must
// never invalidate a run for existing. A second rule written here would be a second opinion about the same
// question, and the two would eventually disagree.
func (s *ownershipSentinel) observe(n *corev1.Node, et watch.EventType) {
	obs := observe(n)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window.NodeVersionsObserved++

	if et == watch.Deleted {
		s.recordLocked(ownershipViolation{
			At: nowStamp(), Reason: reasonWorkerDeleted,
			Detail: fmt.Sprintf("node %s was deleted while this run held it under tx %s; a node recreated under "+
				"the same name is a different machine", s.j.Node, s.j.TxID),
			HasLabel: obs.HasLabel, ObservedLabel: obs.LabelValue,
			ObservedTaints: quotedTaints(obs.Taints), NodeUID: obs.NodeUID,
		})
		return
	}
	err := verifyInstalled(obs, s.j)
	if err == nil {
		return
	}
	// The reason travels as the refusal's own name — installed-values-diverged, journal-names-another-node —
	// so the record classifies this exactly as inspectWorker and decideRelease already classify the same
	// states, rather than under a third vocabulary invented here.
	reason := reasonInstalledDiverged
	var r *refusal
	if asRefusal(err, &r) {
		reason = r.Reason
	}
	s.recordLocked(ownershipViolation{
		At: nowStamp(), Reason: reason,
		Detail:   fmt.Sprintf("node %s no longer carries what tx %s installed: %v", s.j.Node, s.j.TxID, err),
		HasLabel: obs.HasLabel, ObservedLabel: obs.LabelValue,
		ObservedTaints: quotedTaints(obs.Taints), NodeUID: obs.NodeUID,
	})
}

// recordLocked counts every violation and keeps the earliest windowViolationCap of them.
//
// The earliest rather than the latest, because the ones that end the claim are worth more than the ones that
// happen to a run already invalid — an operator needs to know when exclusivity broke, not what it looked
// like once it had.
//
// "Earliest observed", not "earliest in time": consume() starts before the opening read, so a watch event
// delivered in that instant can be folded ahead of it. Every entry carries its own At, which is what a
// reader should order by; retention order is arrival order and nothing more.
func (s *ownershipSentinel) recordLocked(v ownershipViolation) {
	s.window.ViolationsObserved++
	if len(s.window.Violations) < windowViolationCap {
		s.window.Violations = append(s.window.Violations, v)
	}
}

// Close ends the view and joins its consumer, so what Window reports afterwards is final rather than still
// being written.
//
// It is idempotent and must be called before the release: the release removes this transaction's markers on
// purpose, and a view still running through it would report the run's own restoration as the very
// divergence this gate refuses.
func (s *ownershipSentinel) Close() {
	s.stopOnce.Do(func() {
		s.stream.Stop()
		<-s.done
		s.mu.Lock()
		s.window.ClosedAt = nowStamp()
		s.mu.Unlock()
	})
}

// Window is the evidence, safe to read at any time and final once Close has returned.
func (s *ownershipSentinel) Window() ownershipWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.window
	w.Violations = append([]ownershipViolation(nil), s.window.Violations...)
	return w
}

// invalidation is the sentence the ledger is desynced with, or "" when the window held.
//
// It quotes ONE violation rather than summarising all of them, and calls it what it is — the first one
// recorded, which is the first one observed and not necessarily the earliest by clock (see recordLocked).
// The count beside it says how much else there was, and the record carries every retained entry with its own
// timestamp for anyone who needs the order.
func (w *ownershipWindow) invalidation() string {
	if w == nil || w.ViolationsObserved == 0 {
		return ""
	}
	// windowViolationCap is above zero, so a counted violation is always a retained one and this stands for a
	// state no build can reach. It is a fallback rather than an index because this string is the run's own
	// refusal: a panic here would destroy the refusal along with the run it was refusing.
	quoted := "no violation was retained to quote"
	if len(w.Violations) > 0 {
		quoted = fmt.Sprintf("%s: %s (at %s)", w.Violations[0].Reason, w.Violations[0].Detail, w.Violations[0].At)
	}
	return fmt.Sprintf("the worker was not exclusively this run's for the whole window: %d violation(s) "+
		"observed across %d node version(s); the first recorded was %s",
		w.ViolationsObserved, w.NodeVersionsObserved, quoted)
}

// readMarkers is one side of the restoration audit.
func readMarkers(ctx context.Context, c client.Client, node string) (nodeMarkers, error) {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return nodeMarkers{}, fmt.Errorf("read node %s for the restoration audit: %w", node, err)
	}
	obs := observe(&n)
	var other []string
	for _, t := range obs.AllTaints {
		if t.Key != workerTaintKey {
			other = append(other, t.Key)
		}
	}
	// Sorted so the two sides are comparable and so the record does not change shape with the order the
	// apiserver happened to return.
	sort.Strings(other)
	var ours []string
	for _, t := range obs.Taints {
		ours = append(ours, t.Value)
	}
	return nodeMarkers{
		Observed:             true,
		NodeUID:              obs.NodeUID,
		HasLabel:             obs.HasLabel,
		LabelValue:           obs.LabelValue,
		OwnershipTaintValues: ours,
		OtherTaintKeys:       other,
		HasJournal:           obs.JournalRaw != "",
	}, nil
}

// auditedRelease releases the worker and records what the Node looked like on either side of it.
//
// The release's own verdict is returned untouched: releaseOwned decides whether the run kept its worker, and
// this adds evidence rather than a second opinion. A failed audit read therefore never converts a good
// release into a bad one — it says which side could not be read and leaves the verdict alone.
func auditedRelease(ctx context.Context, c client.Client, j journal) (*restorationAudit, error) {
	audit := &restorationAudit{}
	before, err := readMarkers(ctx, c, j.Node)
	if err != nil {
		audit.Unavailable = err.Error()
	}
	audit.Before = before

	relErr := releaseOwned(ctx, c, j)

	after, err := readMarkers(ctx, c, j.Node)
	if err != nil {
		// The two sides are appended rather than one replacing the other, because a release whose before AND
		// after are both unreadable is a different state from one where only the second read failed, and an
		// audit that reported the last failure alone would erase the difference.
		if audit.Unavailable != "" {
			audit.Unavailable += "; "
		}
		audit.Unavailable += err.Error()
	}
	audit.After = after
	audit.summarise(j)
	return audit, relErr
}

// reportRestoration says what the audit found, and says nothing when there is nothing to act on.
//
// The audit is evidence; the release's own verifyReleased is what decides the run. So this prints only the
// two things a human has to do something about — a side that could not be read, and unrelated taints that
// did not survive the release — and stays silent on a clean one, because a line printed after every
// successful run is a line nobody reads on the run where it matters.
//
// w is a seam for the same reason reportResidue's is: a claim printed to a package-level stderr is a claim
// no test can fail, and this file's ancestors shipped two false ones that way.
func reportRestoration(w io.Writer, worker string, a *restorationAudit) {
	if a == nil {
		return
	}
	if a.Unavailable != "" {
		_, _ = fmt.Fprintf(w, "RESTORATION NOT AUDITED: worker %s: %s\n", worker, a.Unavailable)
	}
	if len(a.Drift) > 0 {
		// Not an outcome change, for the reason a failed residue stamp is not one either: what this run had to
		// restore is its own label and taint, and verifyReleased has already proven those came off. A taint of
		// somebody else's that did not survive is damage to the cluster rather than to the measurement, and it
		// happened after the horizon, so it cannot bear on the number — but nobody else is watching for it.
		_, _ = fmt.Fprintf(w, "RESTORATION DRIFT: worker %s carried taint(s) %v before this run released it and does "+
			"not now; the release replaces spec.taints wholesale, so something dropped them\n", worker, a.Drift)
	}
}

// summarise reads the two sides against each other.
//
// It concludes nothing from a side that was not observed: OurMarkersRemoved stays false when the after read
// failed, because "we could not look" must never persist as "it was clean".
func (a *restorationAudit) summarise(j journal) {
	if !a.After.Observed {
		return
	}
	// The node UID is checked first for verifyClean's reason: a node recreated under the same name is a
	// different machine, and finding nothing of ours on it is not evidence that ours came off.
	if a.After.NodeUID != j.NodeUID {
		return
	}
	labelGone := !a.After.HasLabel || a.After.LabelValue != j.Installed.LabelValue
	taintGone := true
	for _, v := range a.After.OwnershipTaintValues {
		if v == j.Installed.TaintValue {
			taintGone = false
		}
	}
	a.OurMarkersRemoved = labelGone && taintGone && !a.After.HasJournal
	if !a.Before.Observed {
		return
	}
	after := map[string]bool{}
	for _, k := range a.After.OtherTaintKeys {
		after[k] = true
	}
	for _, k := range a.Before.OtherTaintKeys {
		if !after[k] {
			a.Drift = append(a.Drift, k)
		}
	}
}
