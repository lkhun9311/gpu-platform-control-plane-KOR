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
	"maps"
	"slices"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

const (
	kindMLTrainingJob = "MLTrainingJob"
	kindJob           = "Job"
	kindWorkload      = "Workload"
	kindPod           = "Pod"
	phaseRunning      = "Running"
)

// pendingEvent is a classified transition whose object could not yet be attributed to a trace job (its
// parent had not been observed); it is retried as more objects arrive, preserving the transition time.
type pendingEvent struct {
	kind    string
	uid     string
	delta   queuelab.DeltaType
	state   queuelab.ObservedState
	elapsed int64
}

// collector observes the four GVRs through continuous streams and drives the fail-closed LedgerBuilder,
// resolving each object to its trace job by the UID chain and classifying it. It is the live half of the
// runner; the pure builder, classifier, join, and adapter are the reviewed units it composes.
type collector struct {
	c     client.WithWatch
	ns    string
	runID string
	t0    time.Time
	// deadline is the end of the observation window as an absolute instant, stamped from t0 before any
	// barrier is waited on.
	//
	// It is one field rather than a value each caller derives because the barrier loop and the censoring
	// boundary must be the SAME instant: two separately computed deadlines can drift apart, and a
	// reconstruction censored against a different horizon than the one the schedule ran to is not a
	// reconstruction of the run that happened.
	deadline time.Time
	builder  *queuelab.LedgerBuilder

	// devicePods maps the Pod names a device exporter reports back to the identities the API guarantees.
	//
	// It is filled from this collector's own watch rather than a live lookup, because by the time a run is
	// reconstructed its Pods are gone -- and a live lookup would resolve nothing, or resolve a name that has
	// since been reused. It is the half of the device observation that the exporter is not allowed to supply:
	// DCGM says which card and which NAME, and this says which object that name was.
	devicePods queuelab.PodRegistry

	// streams is this run's view of the cluster, one per watched kind, and is written only by start and read
	// only by the main goroutine afterwards — the consumer goroutines are handed their own stream and never
	// look at the slice.
	streams []kindStream

	mu      sync.Mutex
	cache   map[string]queuelab.ObservedObject
	pending []pendingEvent
	readyAt map[string]time.Time
	wg      sync.WaitGroup
}

// kindStream pairs a stream with the kind name its events are classified under.
//
// The kind cannot be recovered from the stream: startWatchStream is handed a list constructor and keeps no
// name of its own. Without it every diagnostic below would say "a stream ended" for four different views of
// the run, and the operator could not tell a lost Pod watch — which loses readiness and termination — from a
// lost Workload watch, which loses admission and preemption.
type kindStream struct {
	kind   string
	stream *watchStream
}

// newAnchoredBuilder returns a ledger builder that can date its events in wall-clock terms.
//
// The anchor is the run's own t0, so every event's WallUnixNanos is that instant plus the event's monotonic
// offset. Sampling the wall clock at each observation instead would let two events land out of order in the
// very field meant to date them.
func newAnchoredBuilder(t0 time.Time) *queuelab.LedgerBuilder {
	b := queuelab.NewLedgerBuilder()
	b.AnchorWallClock(t0)
	return b
}

func newCollector(c client.WithWatch, ns, runID string, horizon time.Duration) *collector {
	t0 := time.Now()
	return &collector{
		c:        c,
		ns:       ns,
		runID:    runID,
		t0:       t0,
		deadline: t0.Add(horizon),
		builder:  newAnchoredBuilder(t0),
		cache:    map[string]queuelab.ObservedObject{},
		readyAt:  map[string]time.Time{},
	}
}

func (col *collector) elapsed() time.Duration { return time.Since(col.t0) }

// horizonNs is the censoring boundary the reconstruction is given, as an offset on the collector's own clock.
//
// It is derived from the deadline stamped at construction rather than read off the clock when it is asked
// for, because the run used to pass col.elapsed() AFTER the watches had been cancelled and joined: the
// horizon was then the observation window plus however long shutdown took, a different number on every run
// and a longer one whenever the API server was slow to close the watches.
//
// The subtraction is against t0 specifically because every ledger event's ElapsedNs is measured from t0; a
// boundary taken from any other origin would be offset from the very events it censors.
func (col *collector) horizonNs() int64 { return col.deadline.Sub(col.t0).Nanoseconds() }

// watchedKinds is the set of views one run opens, and it is hoisted out of start's body so that the record's
// own completeness check and the loop that opens the streams cannot name different sets.
//
// The lift changes no decision: same kinds, same order, same list constructors. What it buys is that
// observationContinuous can ask whether a persisted observation covers every kind this build watches rather
// than counting to a number written down twice — and a build that later adds a fifth view gets a record that
// refuses to read four streams as a complete observation, instead of quietly narrowing what "continuous"
// means.
func watchedKinds() []struct {
	kind    string
	newList func() client.ObjectList
} {
	return []struct {
		kind    string
		newList func() client.ObjectList
	}{
		{kindMLTrainingJob, func() client.ObjectList { return &platformv1.MLTrainingJobList{} }},
		{kindJob, func() client.ObjectList { return &batchv1.JobList{} }},
		{kindWorkload, func() client.ObjectList { return &kueuev1beta2.WorkloadList{} }},
		{kindPod, func() client.ObjectList { return &corev1.PodList{} }},
	}
}

// start opens one continuous stream per watched kind and consumes each into the ledger.
//
// A stream is a baseline List plus a watch resuming from exactly that list's resource version, and the
// difference from the loop this replaces is the whole point of the change. That loop called Watch with no
// resource version, so it began at "now": everything the apiserver had already recorded between this run
// creating its namespace and the watch being accepted was invisible, and every reconnect — it re-established
// from "now" again — opened another hole of the same kind. A run could miss an admission, a readiness, a
// preemption or a termination and still reach the disposition that says the checks passed.
//
// It returns an error rather than logging one because nothing downstream can compensate for a view that was
// never opened. The caller must refuse the run before it submits work it cannot observe.
func (col *collector) start(ctx context.Context) error {
	for _, k := range watchedKinds() {
		s, err := startWatchStream(ctx, col.c, namespaceScope(col.ns), k.newList)
		if err != nil {
			col.abort()
			return fmt.Errorf("open the %s stream: %w", k.kind, err)
		}
		col.streams = append(col.streams, kindStream{kind: k.kind, stream: s})
		if n := s.Baseline().Objects; n > 0 {
			// A non-empty baseline refuses the run, and the alternatives are close enough together to be worth
			// arguing rather than asserting.
			//
			// These four kinds are exactly what this run submits and what the platform creates in response, and
			// the namespace was created by this same run moments earlier, so anything already standing in it is
			// somebody else's: a previous attempt under this run id whose namespace was never cleaned up (only
			// the namespace is ever cleared by hand, so that is the ordinary rerun), or another actor working
			// inside a name this run believes it owns.
			//
			// Feeding them in as events would be worse than dropping them, not better. The trace job names are
			// fixed by the protocol, so a previous attempt's objects carry the SAME names this run is about to
			// use; ResolveTraceJobs would attribute a dead run's Pods to this run's jobs, at an elapsed time of
			// roughly zero, and the reconstruction would measure one run's latency against another's work. And
			// dropping them silently is the defect this whole change exists to remove — the ledger would then
			// describe a namespace it had only half observed.
			//
			// So the run is refused, at the cheapest possible moment: nothing has been submitted, no GPU time
			// has been spent, and the operator is told which kind is occupied and by how many objects.
			//
			// The ledger is desynced as well as the error returned, because the two do different jobs. The error
			// is what stops run(); the builder is the object that decides whether a number may be printed at
			// all, and a caller that mishandled the error must still not be able to get one out of it.
			col.desync(fmt.Sprintf("the %s baseline in %s held %d object(s) this run did not create", k.kind, col.ns, n))
			col.abort()
			return fmt.Errorf("the %s baseline in %s already holds %d object(s); this run created that namespace, "+
				"so they are a previous attempt's or another actor's, and neither dropping them nor folding them "+
				"into this run's ledger would describe the run that is about to happen", k.kind, col.ns, n)
		}
		col.consume(k.kind, s)
	}
	return nil
}

// abort ends every stream this collector opened and joins whatever was already consuming them.
//
// It exists for start's failure paths alone. The ordinary shutdown cancels the observation context instead,
// so the endings read as cancellations; here the caller is refusing the run outright, and an ending marked
// Stopped is what stops consume from recording a desync for a stream nobody was still relying on.
func (col *collector) abort() {
	for _, ks := range col.streams {
		ks.stream.Stop()
	}
	col.wg.Wait()
}

// establishBudget bounds how long a run waits for its four streams before refusing to start.
//
// The wait is spent INSIDE the observation window — t0 and the horizon are stamped when the collector is
// built, because the baseline lists are taken at that instant and dating events from any later one would put
// the baseline outside the window it describes — so this constant is the amount of window a slow apiserver is
// allowed to consume.
//
// It is NOT sized against slack in the horizon, because there is none to spend. horizonSec adds
// startupMarginSec on top of the protocol's own timing, and spine.go allocates that 20 seconds to three
// SEQUENTIAL Ready waits (a1, victim, owner); a run that spent 15 of them here would leave 5 for all three
// and would very likely miss its last barrier. What makes that acceptable is the direction such a run fails
// in: a barrier that is not met before the horizon desyncs the ledger, and a desynced run publishes no
// number. Truncation is therefore loud and self-reporting, while the failure this budget exists to catch — a
// watch that never establishes while the run submits and observes nothing — used to be silent.
//
// Fifteen seconds is several hundred times what four List+Watch pairs cost against a healthy apiserver, so in
// practice the budget is never approached; it is a bound on an infrastructure failure, not a latency target.
// run() prints what was actually spent, so an operator can see a window that was shortened rather than infer
// it from a barrier miss.
const establishBudget = 15 * time.Second

// awaitEstablished blocks until every stream has accepted a watch, and refuses the run if any of them cannot.
//
// The timer is deliberately NOT a deadline on the streams' own context, and startWatchStream's doc comment
// names why: a context bounded here is handed to the streams as well, so every later stream death would read
// as Cancelled — the caller's own shutdown signature — and a run that stopped observing thirty seconds in
// would be reported as an orderly one. The budget therefore lives in this select, and the streams keep the
// run's context.
//
// The End() arm is the other half, and it is not defensive padding: a stream can fail permanently BEFORE it
// ever establishes anything (a wrapped Forbidden is the case this package has reproduced), and
// startWatchStream has already returned a nil error by then. A bare receive on Established() would sit out the
// whole budget on a stream that is already dead, and then report a timeout rather than the cause.
//
// A stream that established and then died before this loop reached it can take either arm, and both refuse:
// this one reports the ending, and the other lets consume record the desync a moment later.
func (col *collector) awaitEstablished(ctx context.Context, budget time.Duration) error {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for _, ks := range col.streams {
		select {
		case <-ks.stream.Established():
		case <-ks.stream.End():
			return fmt.Errorf("the %s stream ended before it ever established a watch: %s",
				ks.kind, endText(ks.stream.Ended()))
		case <-timer.C:
			return fmt.Errorf("the %s stream did not establish a watch within %s, and a run that submits under an "+
				"unestablished watch observes nothing it can be held to", ks.kind, budget)
		case <-ctx.Done():
			return fmt.Errorf("waiting for the %s stream to establish: %w", ks.kind, ctx.Err())
		}
	}
	return nil
}

// endText renders what could be observed about an ending, for a refusal an operator has to act on.
//
// The absence of a status is reported as an absence rather than omitted, because "the stream ended and said
// nothing" is a different diagnosis from "the stream ended with a 403" and the operator needs to know which
// of the two they are looking at before they go hunting for an RBAC problem that may not exist.
func endText(e streamEnd) string {
	if e.LastStatus != nil {
		return fmt.Sprintf("%s (code %d, reason %s)", e.LastStatus.Message, e.LastStatus.Code, e.LastStatus.Reason)
	}
	return "it forwarded no status, so the cause is not recoverable from the stream itself"
}

// streamEvidence is what one stream established and how it ended, in the spelling the run record persists.
//
// The ending is carried as the three RAW facts streamEnd holds rather than as a verdict computed here, and
// that is the point rather than laziness. The reading of those facts is one-sided — streamEnd's own comment
// spells out why Cancelled or Stopped can never prove an ending was orderly — and the record's validity block
// is where that asymmetry is applied, once. A "lost" bool decided at this end would put the same rule in two
// places, and the copy a reader could check would not be the copy that decided.
type streamEvidence struct {
	Kind string `json:"kind"`
	// BaselineResourceVersion is the point this stream's watch resumed from, and it is the field that makes
	// the difference between a view and a sample: startWatchStream refuses "" and "0" precisely because
	// neither is a point a later gap could be measured against.
	BaselineResourceVersion string `json:"baselineResourceVersion"`
	// BaselineObjects is what the initial List found, and on any run that got as far as submitting it is 0 —
	// start above refuses a non-empty baseline outright. It is persisted anyway because the refusal is the
	// record worth having: it says how much of somebody else's work was standing in a namespace this run had
	// just created, which nothing else in the document would afterwards say.
	BaselineObjects int `json:"baselineObjects"`
	// Ended says End() had closed when this was captured, and it is recorded rather than assumed.
	//
	// It is not always true, which is the point. Every stream that has a CONSUMER is joined before run()
	// returns — col.abort on the start failures, cancel plus col.wait on every other — but start refuses a
	// non-empty baseline before it consumes that stream, so on that one path a stream is stopped and never
	// joined and its ending may still be being written when this is sampled. A zero streamEnd is
	// byte-identical to an ending with neither flag set, which is the shape that reads as a LOST stream, so
	// without this field that refusal would persist as "the view died under us" about a view that was merely
	// still shutting down.
	Ended     bool `json:"ended"`
	Cancelled bool `json:"cancelled"`
	Stopped   bool `json:"stopped"`
	// LastStatus is the terminal watch status the stream actually forwarded, in statusText's spelling, and
	// its absence proves nothing — a permanent failure can terminate while forwarding no event at all.
	LastStatus string `json:"lastStatus,omitempty"`
}

// observationEvidence is what the run's continuous view of its own namespace actually was.
//
// It is the last of the three gates' evidence to reach the record, and the one a reader can least
// reconstruct: qualification describes a machine and the window describes a Node, both of which an operator
// could go and look at afterwards, while a stream's baseline and its ending exist only for as long as the
// process does. Without this block a stored record cannot distinguish a complete observation from a truncated
// one, which is the single property the continuity gate exists to establish.
//
// It is written into the record directly rather than through a second projection type, for qualification's
// reason: every field here is a string, a bool or an int, so nothing encodes asymmetrically and nothing is an
// iota whose declaration order would become a wire format.
type observationEvidence struct {
	Namespace string `json:"namespace"`
	// HorizonNs is the denominator for EstablishedNs. Establishment is spent INSIDE the window (see
	// establishBudget), so "four seconds" means nothing to a later reader without the window it came out of,
	// and the horizon appears nowhere else in the record.
	HorizonNs int64 `json:"horizonNs"`
	// Established says every stream accepted a watch before anything was submitted. A false one is the
	// strongest thing this block says: the run either never opened its streams or gave up waiting for them,
	// and nothing it observed afterwards covers the interval it was supposed to.
	Established       bool             `json:"established"`
	EstablishedNs     int64            `json:"establishedNs"`
	EstablishBudgetNs int64            `json:"establishBudgetNs"`
	Streams           []streamEvidence `json:"streams,omitempty"`
}

// evidence projects what this collector's streams established and how each of them ended.
//
// Ended() is only final once a stream's End() has closed, so this SAMPLES End() rather than assuming it: see
// streamEvidence.Ended for the one path where a stream reaches here unjoined, and for why persisting its zero
// ending would invent a loss. Nothing here reads the builder or changes any decision — the ledger's verdict
// was taken long before this runs, and this is a projection of what the streams already were.
func (col *collector) evidence(established bool, establishedIn time.Duration) *observationEvidence {
	ev := &observationEvidence{
		Namespace:         col.ns,
		HorizonNs:         col.horizonNs(),
		Established:       established,
		EstablishBudgetNs: establishBudget.Nanoseconds(),
	}
	if established {
		ev.EstablishedNs = establishedIn.Nanoseconds()
	}
	for _, ks := range col.streams {
		s := streamEvidence{
			Kind:                    ks.kind,
			BaselineResourceVersion: ks.stream.Baseline().ResourceVersion,
			BaselineObjects:         ks.stream.Baseline().Objects,
		}
		select {
		case <-ks.stream.End():
			end := ks.stream.Ended()
			s.Ended = true
			s.Cancelled = end.Cancelled
			s.Stopped = end.Stopped
			if end.LastStatus != nil {
				s.LastStatus = statusText(end.LastStatus)
			}
		default:
		}
		ev.Streams = append(ev.Streams, s)
	}
	return ev
}

func (col *collector) wait() { col.wg.Wait() }

// desync invalidates the run from the main goroutine, which is the only caller that does not already hold mu.
//
// LedgerBuilder has no locking of its own — invalid, events, lastEvent and ready are plain fields — and the
// four stream consumers call Observe through flush under this mutex for the whole run, as well as calling
// this from their own goroutines when their stream ends badly. Calling Desync
// directly from the main goroutine is therefore a data race on the single field that decides whether the run
// is allowed to produce a number at all, which is the one thing this package may not get wrong. The lock
// belongs here rather than inside internal/queuelab because mu already serialises every other access to the
// builder, and a second, independent mutex would only make that ownership ambiguous.
func (col *collector) desync(reason string) {
	col.mu.Lock()
	defer col.mu.Unlock()
	col.builder.Desync(reason)
}

// consume folds one stream's events into the ledger and decides what that stream's ending means.
//
// The ending is the half that matters. An apiserver timeout no longer reaches here at all — the stream's
// RetryWatcher resumes from the last delivered resource version and the gap is closed by the server, which is
// what the old loop's comment claimed to do by re-listing and never did — so a stream that stops delivering
// has stopped for a reason no reconnection can repair, and the events in the gap are lost rather than
// delayed. The only endings this may accept are therefore the two the caller itself caused: the observation
// context cancelled at the horizon, and an explicit Stop.
//
// LastStatus condemns the run even alongside those two, per streamEnd's own asymmetry: a forwarded terminal
// status is a loss the stream actually witnessed, while a Cancelled or Stopped flag only says the caller
// arrived at the same moment and never that the ending was orderly.
func (col *collector) consume(kind string, s *watchStream) {
	col.wg.Go(func() {
		for ev := range s.ResultChan() {
			if ev.Type == watch.Error {
				// No verdict is taken here, and that is deliberate rather than an omission. RetryWatcher forwards
				// NOTHING for the errors it intends to retry — a 504, a 500, an unrecognised status all reconnect
				// in silence — so an Error that arrives here is one it has already given up on, and forward has
				// already recorded the Status as the ending's evidence. Judging it twice would put the run's
				// validity in two places that could disagree; the ending below is the one that decides.
				continue
			}
			obj, ok := ev.Object.(client.Object)
			if !ok {
				continue
			}
			col.handle(kind, ev.Type, obj)
		}
		// Ended is final only now, and that is a guarantee rather than a hope: forward records the ending under
		// its own mutex BEFORE closing the channel this range just drained, so a closed ResultChan proves the
		// verdict is written rather than still being decided.
		end := s.Ended()
		switch {
		case end.LastStatus != nil:
			col.desync(fmt.Sprintf("the %s stream ended on a %s", kind, statusText(end.LastStatus)))
		case !end.Cancelled && !end.Stopped:
			col.desync(fmt.Sprintf("the %s stream ended on its own while the run was still observing, so every "+
				"transition after that point is unobserved rather than absent", kind))
		}
	})
}

// statusText names the cause an operator can act on: a 410 means the resume point aged out of etcd and the
// run has to be rerun, a 403 means the credentials cannot watch this kind and no rerun will help.
func statusText(st *metav1.Status) string {
	return fmt.Sprintf("terminal watch error: %s (code %d, reason %s)", st.Message, st.Code, st.Reason)
}

// handle folds one watch event: it updates the object cache, classifies the object, and (re)attempts to
// attribute pending transitions to their trace jobs now that the cache has grown.
func (col *collector) handle(kind string, et watch.EventType, obj client.Object) {
	col.mu.Lock()
	defer col.mu.Unlock()

	uid := string(obj.GetUID())
	col.cache[uid] = queuelab.ObservedFrom(kind, obj)
	if kind == kindPod {
		// Noted on every event rather than only on create, because a Pod observed first through an update --
		// which a resumed watch delivers -- would otherwise never be resolvable, and every sample naming it
		// would count as unattributed for the rest of the run.
		col.devicePods.Note(obj.GetNamespace(), obj.GetName(), uid)
	}

	state := classify(kind, obj)
	delta := queuelab.DeltaUpsert
	if et == watch.Deleted {
		delta = queuelab.DeltaDelete
	}
	if state.Event != "" || state.Invalid || delta == queuelab.DeltaDelete {
		col.pending = append(col.pending, pendingEvent{kind, uid, delta, state, col.elapsed().Nanoseconds()})
	}
	col.flush()
}

// flush attributes as many pending transitions as the current cache allows, keeping the rest for later.
func (col *collector) flush() {
	objs := make([]queuelab.ObservedObject, 0, len(col.cache))
	for _, o := range col.cache {
		objs = append(objs, o)
	}
	names, err := queuelab.ResolveTraceJobs(objs)
	if err != nil {
		// The chain is incomplete (a child observed before its parent) or corrupt; keep pending and retry.
		return
	}
	kept := col.pending[:0]
	for _, pe := range col.pending {
		if name, ok := names[pe.uid]; ok {
			col.builder.Observe(pe.delta, pe.kind, pe.uid, name, pe.state, pe.elapsed)
		} else {
			kept = append(kept, pe)
		}
	}
	col.pending = kept
}

// relistCheck used to live here, and it is gone rather than repaired, because the gap it audited no longer
// exists and the audit could not be moved anywhere it would still be sound.
//
// What it did: after each watch re-establishment, list Pods and mark any previously-Ready Pod that was no
// longer present as vanished, so a stop missed during the gap between the two watches was caught. Three
// things were wrong with it and only one was fixable here. It ran for Pods alone, so a Workload admitted and
// preempted inside the same gap left no trace at all. Its own List failure returned silently, turning "the
// gap could not be audited" into "the gap is fine" — the defect this task names. And it ran INSIDE the live
// watch loop, so it could mark a Pod vanished while a terminal event for that Pod was still in flight.
//
// What replaces it is not a better audit but the removal of the thing being audited: the streams resume from
// the last delivered resource version, so an apiserver timeout is closed by the server with no gap to inspect,
// and any ending a resume cannot repair now invalidates the run outright in consume. There is nothing left
// for a relist to discover that the ledger does not already know.
//
// It is deliberately not re-sited as a final List after the streams end, which is what watchStream.Ended's
// doc suggests an adopter might do. That check would run AFTER the horizon, and everything after the horizon
// is censored by design: a Pod that was Ready at the horizon and terminated a second later is not a missed
// stop, it is an event outside the observation window, and marking it vanished would invalidate perfectly
// good runs for doing exactly what the protocol expects. The same doc's other suggestion — comparing the last
// delivered resource version against a final list's — does not survive contact with a real cluster either:
// a list's resource version is the store's current position and advances on writes anywhere in the cluster,
// so it is above the last delivered one on every healthy run and proves nothing.

// submit creates the MLTrainingJob and records the Submitted event from the create response (not a watch),
// so the offered-work clock cannot be observed after admission.
func (col *collector) submitObserved(row queuelab.TrainingTraceRow, uid string, mltj *platformv1.MLTrainingJob) {
	col.mu.Lock()
	defer col.mu.Unlock()
	col.cache[uid] = queuelab.ObservedFrom(kindMLTrainingJob, mltj)
	col.builder.Observe(queuelab.DeltaUpsert, kindMLTrainingJob, uid, row.Name,
		queuelab.ObservedState{Event: queuelab.EventSubmitted}, col.elapsed().Nanoseconds())
}

func classify(kind string, obj client.Object) queuelab.ObservedState {
	switch kind {
	case kindJob:
		return queuelab.ClassifyJob(obj.(*batchv1.Job))
	case kindWorkload:
		return queuelab.ClassifyWorkload(obj.(*kueuev1beta2.Workload))
	case kindPod:
		return queuelab.ClassifyPod(obj.(*corev1.Pod))
	default:
		return queuelab.ObservedState{}
	}
}

// ---- schedule execution (barrier polling against the live cluster) ----

// waitBarriers blocks until every barrier in after holds, polling the cluster, or errors on the deadline.
//
// The deadline is the collector's own stamped one rather than a parameter, so the window the schedule runs
// to and the horizon the reconstruction censors against cannot be two different instants.
func waitBarriers(ctx context.Context, c client.Client, ns string, after []queuelab.Barrier,
	col *collector) error {
	for _, b := range after {
		if err := waitBarrier(ctx, c, ns, b, col); err != nil {
			return err
		}
	}
	return nil
}

// errUnknownBarrier marks a barrier kind this executable has no check for.
//
// It is a sentinel rather than a bare message so waitBarrier can recognise it as terminal: polling a kind
// nothing can ever evaluate would burn the whole observation window on a programming error and then report
// it as a protocol outcome.
var errUnknownBarrier = errors.New("unknown barrier kind")

// barrierTerminal reports whether an error from barrierHolds is one that no amount of further polling can
// clear.
//
// The distinction is the point: a barrier waits for an object to APPEAR, so the ordinary failure while it
// waits is a NotFound (already folded into "not met" by phaseIs) and every transient API error is worth
// retrying. But an authorization failure, a kind the cluster or the scheme does not know, and a barrier this
// tool cannot evaluate are all states that will still hold at the deadline, and polling them until the
// horizon expires spends the entire run to report an infrastructure failure as "barrier not met".
//
// NotFound is deliberately NOT terminal: it is indistinguishable from an object that has not been created
// yet, which is exactly what a barrier exists to wait for.
func barrierTerminal(err error) bool {
	switch {
	case errors.Is(err, errUnknownBarrier):
		return true
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return true
	case meta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
		// The CRD is absent from the cluster, or its kind from this client's scheme; either way no request
		// this loop can issue will ever succeed.
		return true
	default:
		return false
	}
}

func waitBarrier(ctx context.Context, c client.Client, ns string, b queuelab.Barrier, col *collector) error {
	// The most recent check error is kept so the horizon refusal can carry it: a barrier that was failing for
	// a retryable reason right up to the deadline used to report only "not met", which tells the operator
	// nothing about why it was never met.
	var lastErr error
	for {
		ok, err := barrierHolds(ctx, c, ns, b, col)
		if err == nil && ok {
			return nil
		}
		if err == nil {
			// A check that completed and simply found the barrier unmet clears the carried error, and the
			// comment above says why without having said so: the error is kept for a barrier that was failing
			// "right up to the deadline". Without this line it is kept for one that failed ONCE, minutes
			// earlier, and then polled cleanly until the horizon — and the refusal then tells an operator "the
			// last check failed: <error>" about a check that succeeded, sending them after a connectivity
			// problem that resolved itself. The message has to be true of the last check, which is what it
			// claims to describe.
			lastErr = nil
		}
		if err != nil {
			lastErr = err
			// Cancellation is reported as itself rather than as a barrier outcome, whether it arrives through
			// the select below or through the API call this check just made; Task 3 established that and it
			// must survive this classification.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("barrier %s(job=%s) observation cancelled (last check: %v): %w",
					b.Kind, b.Job, err, ctxErr)
			}
			if barrierTerminal(err) {
				return fmt.Errorf("barrier %s(job=%s,count=%d,delay=%d) cannot be met: %w",
					b.Kind, b.Job, b.Count, b.DelaySec, err)
			}
		}
		if time.Now().After(col.deadline) {
			if lastErr != nil {
				return fmt.Errorf("barrier %s(job=%s,count=%d,delay=%d) not met before horizon; the last check failed: %w",
					b.Kind, b.Job, b.Count, b.DelaySec, lastErr)
			}
			return fmt.Errorf("barrier %s(job=%s,count=%d,delay=%d) not met before horizon", b.Kind, b.Job, b.Count, b.DelaySec)
		}
		// Cancellation is reported as itself rather than as "barrier not met", which would misread an
		// operator's Ctrl-C as a protocol failure in the ledger's desync note.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func barrierHolds(ctx context.Context, c client.Client, ns string, b queuelab.Barrier, col *collector) (bool, error) {
	switch b.Kind {
	case queuelab.BarrierAdmittedReady:
		running, err := phaseIs(ctx, c, ns, b.Job, phaseRunning)
		if err == nil && running {
			col.mu.Lock()
			if _, seen := col.readyAt[b.Job]; !seen {
				col.readyAt[b.Job] = time.Now()
			}
			col.mu.Unlock()
		}
		return running, err
	case queuelab.BarrierPending:
		return phaseIs(ctx, c, ns, b.Job, "Pending")
	case queuelab.BarrierFlavorUsage:
		// Still >=, and deliberately so despite the contract's word "equals". Usage is sampled by polling, so
		// an exact match can be stepped over between two reads and the barrier would then wait out its whole
		// deadline on a run that had already reached the state. The contract wording is corrected to say
		// "reaches" rather than "equals"; the threshold is what was always meant.
		n, err := flavorUsage(ctx, c, queuelab.FlavorName(col.runID))
		return n >= int64(b.Count), err
	case queuelab.BarrierDelayFromReady:
		col.mu.Lock()
		ready, ok := col.readyAt[b.Job]
		col.mu.Unlock()
		if !ok {
			return false, nil
		}
		return time.Since(ready) >= time.Duration(b.DelaySec)*time.Second, nil
	default:
		return false, fmt.Errorf("%w %s", errUnknownBarrier, b.Kind)
	}
}

func phaseIs(ctx context.Context, c client.Client, ns, name, want string) (bool, error) {
	var mltj platformv1.MLTrainingJob
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &mltj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return mltj.Status.Phase == want, nil
}

// flavorUsage returns the GPU total Kueue reports as used against this run's ResourceFlavor.
//
// This barrier's contract is that the flavor's OBSERVED usage reached Count, which is what establishes that
// borrowing took effect before the next step is submitted. The implementation used to sum the declared
// Spec.GPUCount of MLTrainingJobs this operator had marked Running — three steps removed from the claim:
// it read our own phase field rather than Kueue's accounting, it counted what a job ASKED for rather than
// what was charged, and it summed a whole namespace rather than one flavor. Every one of those can hold
// while Kueue has admitted nothing, and this barrier exists precisely to decide that question.
//
// Reading the ClusterQueue's status is the observation the contract already promised.
func flavorUsage(ctx context.Context, c client.Client, flavor string) (int64, error) {
	var list kueuev1beta2.ClusterQueueList
	if err := c.List(ctx, &list); err != nil {
		return 0, err
	}
	var total int64
	for i := range list.Items {
		for _, fu := range list.Items[i].Status.FlavorsUsage {
			if string(fu.Name) != flavor {
				continue
			}
			for _, r := range fu.Resources {
				if r.Name == gpuResourceName {
					total += r.Total.Value()
				}
			}
		}
	}
	return total, nil
}

// submit renders and creates the trace job under its own row's contract, then records its Submitted event.
func submit(ctx context.Context, c client.Client, col *collector, arm queuelab.Arm,
	row queuelab.TrainingTraceRow, ns string) error {
	// The contract is resolved per row rather than per arm because the treatment is the VICTIM's behaviour;
	// rendering the whole arm with one contract would change all three manifests when exactly one is meant
	// to differ.
	contract, err := arm.ContractFor(row.Name)
	if err != nil {
		return err
	}
	mltj, err := queuelab.RenderMLTrainingJobWithContract(row, ns, contract)
	if err != nil {
		return err
	}
	if err := c.Create(ctx, mltj); err != nil {
		return err
	}
	col.submitObserved(row, string(mltj.UID), mltj)
	return nil
}

// ---- cluster setup ----

// ensureNamespace creates the run's namespace, stamped, and refuses one it did not create.
//
// The stamp is in the Create body rather than a follow-up patch because a second write is a window a crash
// can land in, and an object created without its stamp is one no later teardown can recognise or delete.
func ensureNamespace(ctx context.Context, c client.Client, ns, txID string) error {
	created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{queuelab.TxLabel: txID},
	}}
	err := c.Create(ctx, created)
	if err == nil {
		// What went into the Create body is a REQUEST; recoverTargets decides ownership by what the cluster
		// actually stored. A mutating webhook or a namespace label policy that drops labels it does not
		// recognise makes those two different, and then this run's own namespace reads foreign at teardown:
		// nothing of ours is in the residue, so the worker is released — label and NoSchedule taint off — over
		// a namespace this run created and may still be running GPU Pods in.
		//
		// No second Get is needed to see it. The typed client posts the object and decodes the RESPONSE, which
		// is the persisted object, back into the very pointer it was given, having zeroed that pointer first
		// (controller-runtime's targetZeroingDecoder) — so what stands here after Create is what admission
		// stored, not what we sent, and a stale label cannot survive by having merged with the response.
		// Measured on this module's pinned versions against a stub apiserver, for a dropped label, a replaced
		// label map and a rewritten value alike. A Get would only add a round trip and a window for the answer
		// to change between the two reads.
		if got := created.Labels[queuelab.TxLabel]; got != txID {
			return deleteUnstamped(ctx, c, created, fmt.Errorf(
				"namespace %s was created but the cluster stored it under stamp %q, not this run's %q; "+
					"something is rewriting namespace labels, and a namespace whose stamp does not survive its "+
					"own Create is one no teardown of this run can recognise as ours", ns, got, txID))
		}
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// AlreadyExists is an ownership question, not a shrug: the namespace name is derived from the run id
	// alone, so the same name is exactly what a reused run id or a crashed previous attempt leaves behind.
	//
	// A delete landing between the Create above and this Get turns AlreadyExists into NotFound here and
	// fails this run closed with a slightly misleading "could not be read" rather than a clean retry. Fine:
	// the alternative is silently proceeding on an ownership question this read could not actually answer.
	var existing corev1.Namespace
	if gerr := c.Get(ctx, client.ObjectKey{Name: ns}, &existing); gerr != nil {
		return fmt.Errorf("namespace %s exists but could not be read to check ownership: %w", ns, gerr)
	}
	if got := existing.Labels[queuelab.TxLabel]; got != txID {
		return fmt.Errorf("namespace %s already exists under transaction %q, not this run's %q; "+
			"clear it before rerunning rather than running inside another attempt's namespace", ns, got, txID)
	}
	return nil
}

// deleteUnstamped removes the namespace the Create above just made, and reports why it had to.
//
// Refusing without deleting would not close anything: the namespace would still be there, still unstamped,
// still unclaimable by any teardown this run or a later one performs, and the worker release that follows a
// residue of nothing-but-foreign would be as wrong as before — the hole would only be announced. Deleting it
// leaves the cluster as this run found it, which is what makes that release correct rather than accidentally
// correct.
//
// This is safe precisely here and nowhere else. A nil from Create is the apiserver reporting that THIS request
// created the object — client-go never retries a write on a transport error (rest.Request's own
// isErrRetryableFunc returns false for anything but GET), so a nil cannot be a second POST's view of an object
// someone else made — and run() creates nothing inside the namespace until applyFixtures, which is the very
// next call. So there is nothing in there to destroy.
//
// What it does NOT do is wait. A namespace delete is asynchronous, so on a real cluster the deferred teardown
// that follows still finds the name held by a terminating, unstamped object and records it as foreign residue
// — under a stderr line saying nothing this run created is still on the cluster, which is still not literally
// true. The difference this makes is the one that matters: the worker release that follows is now correct
// rather than accidentally correct, because the namespace being reported is empty and on its way out instead
// of live and holding GPU Pods. Measured through run() both ways: with the delete settling at once there is no
// residue at all, and with it lingering the residue is one foreign namespace and the worker is released.
//
// The UID precondition is the same one every delete in teardown_apply.go carries, for the same reason: between
// the Create and this Delete another actor can remove the name and recreate it, and an unconditioned
// delete-by-name would destroy that replacement instead. The UID comes from the Create response, so it is the
// object this call made and nothing else; an empty one would make the apiserver refuse the delete rather than
// widen it to the name, which is the direction to fail in.
func deleteUnstamped(ctx context.Context, c client.Client, created *corev1.Namespace, cause error) error {
	uid := created.GetUID()
	derr := c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: created.GetName()}},
		client.Preconditions{UID: &uid})
	// NotFound is the state this function exists to reach, by someone else's hand. Anything else is a namespace
	// left on the cluster under no stamp any teardown can recognise, and that must never reach the caller as
	// the ordinary "the stamp did not land" refusal — it is the one outcome here that needs a pair of hands.
	if derr != nil && !apierrors.IsNotFound(derr) {
		return fmt.Errorf("%w; and it could not be deleted: %w — it is on the cluster under no stamp this run's "+
			"teardown can recognise and has to be removed by hand", cause, derr)
	}
	return cause
}

// applyFixtures creates the run's Kueue fixtures, first checking that a cluster-scoped ResourceFlavor left
// behind by an earlier run under this run id (only the namespace is ever cleaned up by hand) was built for
// the same policy variant this arm requires, so a reused run id cannot silently execute under a different
// arm's mechanism. That check is about the VARIANT a name was built for; createOwned below is a separate
// question about which TRANSACTION created the name, and both must hold before an existing object is
// treated as this run's own.
func applyFixtures(ctx context.Context, c client.Client, fs *queuelab.FixtureSet, wantVariant, txID string) error {
	var existing kueuev1beta2.ResourceFlavor
	err := c.Get(ctx, client.ObjectKey{Name: fs.Flavor.GetName()}, &existing)
	switch {
	case err == nil:
		if verr := checkFlavorVariant(existing.Labels, wantVariant); verr != nil {
			return fmt.Errorf("resource flavor %s: %w", fs.Flavor.GetName(), verr)
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get resource flavor %s: %w", fs.Flavor.GetName(), err)
	}

	objs := make([]client.Object, 0, 1+len(fs.ClusterQueue)+len(fs.LocalQueue))
	objs = append(objs, fs.Flavor)
	for _, cq := range fs.ClusterQueue {
		objs = append(objs, cq)
	}
	for _, lq := range fs.LocalQueue {
		objs = append(objs, lq)
	}
	for _, o := range objs {
		if err := createOwned(ctx, c, o, txID); err != nil {
			return fmt.Errorf("create %T %s: %w", o, o.GetName(), err)
		}
	}
	return nil
}

// createOwned creates o and, on AlreadyExists, refuses unless the object already there carries this run's
// own transaction stamp — the same shape as ensureNamespace, generalised over every fixture kind so a
// leftover from a different attempt under the same run id cannot silently satisfy this one.
func createOwned(ctx context.Context, c client.Client, o client.Object, txID string) error {
	err := c.Create(ctx, o)
	if err == nil {
		// A successful Create is not proof the stamp landed. controller-runtime decodes the apiserver's
		// response back into o, so what stands in o now is what admission stored — the same reason
		// ensureNamespace checks its own object here rather than issuing a second read. An admission plugin
		// that dropped or rewrote TxLabel would leave an object no teardown of this run can recognise as ours,
		// and the run would go on to measure inside it.
		//
		// A re-read would answer the same question for the cost of another round trip. This does not need one.
		if got := o.GetLabels()[queuelab.TxLabel]; got != txID {
			return deleteUnstampedObject(ctx, c, o, fmt.Errorf(
				"%T %s/%s was created carrying transaction %q rather than this run's %q; an object this "+
					"run created but cannot recognise as its own is one no teardown of this run will remove",
				o, o.GetNamespace(), o.GetName(), got, txID))
		}
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// The ownership check reads into a fresh copy, never into o itself: o is the caller's own FixtureSet
	// object, and Getting the server's copy in place would overwrite its UID and ResourceVersion whether
	// this run created it or only adopted it — leaving a FixtureSet that means two different things
	// depending on which path was taken, a distinction a future consumer recording UIDs for teardown must
	// not have to guess about.
	existing, ok := o.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("internal error: %T does not implement client.Object after DeepCopyObject", o)
	}
	// A delete landing between the Create above and this Get (another actor removing the very object we
	// just failed to create) turns AlreadyExists into NotFound here and fails this run closed with a
	// slightly misleading "could not be read" rather than a clean retry. Fine: the alternative is silently
	// proceeding on an ownership question this read could not actually answer.
	if gerr := c.Get(ctx, client.ObjectKey{Namespace: o.GetNamespace(), Name: o.GetName()}, existing); gerr != nil {
		return fmt.Errorf("exists but could not be read to check ownership: %w", gerr)
	}
	if got := existing.GetLabels()[queuelab.TxLabel]; got != txID {
		return fmt.Errorf("already exists under transaction %q, not this run's %q; "+
			"clear it before rerunning rather than running inside another attempt's object", got, txID)
	}
	// Same transaction is not the same object. The label says who created it; it says nothing about WHAT was
	// created, and what was created is the mechanism this lab exists to measure. A ClusterQueue adopted here
	// with a different reclaimWithinCohort would let the run report an arm it never executed — the exact
	// failure the variant precheck guards for ResourceFlavors, generalised to the specs that carry the knob.
	if merr := sameMechanism(o, existing); merr != nil {
		return fmt.Errorf("already exists under this transaction but %w; clear it before rerunning rather than "+
			"measuring a mechanism this arm did not declare", merr)
	}
	return nil
}

// deleteUnstampedObject removes an object whose stamp did not land, guarded by the UID this run just created.
//
// The precondition is what keeps this from deleting somebody else's object of the same name: if the thing on
// the cluster is no longer the one Create returned, the delete fails rather than widening the blast radius.
func deleteUnstampedObject(ctx context.Context, c client.Client, created client.Object, cause error) error {
	uid := created.GetUID()
	stub, ok := created.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%w; and it could not be prepared for deletion", cause)
	}
	derr := c.Delete(ctx, stub, client.Preconditions{UID: &uid})
	// NotFound is the state this function exists to reach, by someone else's hand. Anything else leaves an
	// object on the cluster under no stamp any teardown can recognise, which needs a pair of hands rather
	// than the ordinary "the stamp did not land" refusal.
	if derr != nil && !apierrors.IsNotFound(derr) {
		return fmt.Errorf("%w; and it could not be deleted: %w — it is on the cluster under no stamp this run's "+
			"teardown can recognise and has to be removed by hand", cause, derr)
	}
	return cause
}

// sameMechanism reports whether an adopted object still implements the experiment this arm declared.
//
// Not whole-spec equality: the apiserver defaults and mutates fields the experiment does not care about, and
// comparing everything would refuse every adoption for reasons unrelated to what is being measured. What is
// compared is every field that DEFINES the experiment, enumerated explicitly so that adding a study forces a
// decision about its knob rather than silently inheriting an incomplete check.
//
// The first version compared only the reclaim study's knob and the cohort. It therefore let a StrictFIFO
// queue satisfy a BestEffortFIFO arm — the exact contamination it existed to prevent, for the OTHER of the
// two studies, whose one varied knob is queueingStrategy. A check that covers one study's mechanism and not
// the other's is worse than none, because it reads as coverage.
func sameMechanism(want, got client.Object) error {
	switch w := want.(type) {
	case *kueuev1beta2.ClusterQueue:
		g, ok := got.(*kueuev1beta2.ClusterQueue)
		if !ok {
			return fmt.Errorf("is a %T where a ClusterQueue was expected", got)
		}
		return sameClusterQueue(w, g)
	case *kueuev1beta2.LocalQueue:
		g, ok := got.(*kueuev1beta2.LocalQueue)
		if !ok {
			return fmt.Errorf("is a %T where a LocalQueue was expected", got)
		}
		// A LocalQueue pointing at another ClusterQueue routes this arm's jobs into another arm's quota.
		if w.Spec.ClusterQueue != g.Spec.ClusterQueue {
			return fmt.Errorf("it points at ClusterQueue %q, not %q", g.Spec.ClusterQueue, w.Spec.ClusterQueue)
		}
		// A LocalQueue carries its own stop policy, and a stopped one holds every submission at this end
		// however healthy the ClusterQueue behind it is.
		if stopPolicy(w.Spec.StopPolicy) != stopPolicy(g.Spec.StopPolicy) {
			return fmt.Errorf("its stopPolicy is %q, not the %q this arm declared",
				stopPolicy(g.Spec.StopPolicy), stopPolicy(w.Spec.StopPolicy))
		}
	case *kueuev1beta2.ResourceFlavor:
		g, ok := got.(*kueuev1beta2.ResourceFlavor)
		if !ok {
			return fmt.Errorf("is a %T where a ResourceFlavor was expected", got)
		}
		return sameFlavor(w, g)
	}
	return nil
}

// sameClusterQueue compares the fields that decide what a ClusterQueue admits and how it preempts.
func sameClusterQueue(w, g *kueuev1beta2.ClusterQueue) error {
	// The FIFO study's ONE varied knob.
	if w.Spec.QueueingStrategy != g.Spec.QueueingStrategy {
		return fmt.Errorf("its queueingStrategy is %q, not the %q this arm declared",
			g.Spec.QueueingStrategy, w.Spec.QueueingStrategy)
	}
	// The reclaim study's one varied knob, plus the in-queue policy that decides whether a waiting workload
	// can displace a running one. Both are preemption behaviour; a run adopting either from another arm
	// measures that arm.
	wp, gp := w.Spec.Preemption, g.Spec.Preemption
	if (wp == nil) != (gp == nil) {
		return fmt.Errorf("one of the two has no preemption policy at all")
	}
	if wp != nil && gp != nil {
		if wp.ReclaimWithinCohort != gp.ReclaimWithinCohort {
			return fmt.Errorf("its reclaimWithinCohort is %q, not the %q this arm declared",
				gp.ReclaimWithinCohort, wp.ReclaimWithinCohort)
		}
		if wp.WithinClusterQueue != gp.WithinClusterQueue {
			return fmt.Errorf("its withinClusterQueue is %q, not the %q this arm declared",
				gp.WithinClusterQueue, wp.WithinClusterQueue)
		}
		// The third preemption knob. It decides whether a workload may preempt in order to keep BORROWING
		// rather than to fit inside its own nominal quota, which is a distinct behaviour from the two above
		// and the one the reclaim study's cohort arrangement exists to exercise.
		if borrowPolicy(wp.BorrowWithinCohort) != borrowPolicy(gp.BorrowWithinCohort) {
			return fmt.Errorf("its borrowWithinCohort policy is %q, not the %q this arm declared",
				borrowPolicy(gp.BorrowWithinCohort), borrowPolicy(wp.BorrowWithinCohort))
		}
	}
	// A stopped queue admits nothing at all, so an arm that adopted one would record a run in which the
	// mechanism under test never ran, with no error anywhere to say so.
	if stopPolicy(w.Spec.StopPolicy) != stopPolicy(g.Spec.StopPolicy) {
		return fmt.Errorf("its stopPolicy is %q, not the %q this arm declared",
			stopPolicy(g.Spec.StopPolicy), stopPolicy(w.Spec.StopPolicy))
	}
	// The fixture declares an empty selector, meaning every namespace may submit to this queue. A selector
	// that is absent or narrowed can exclude this run's own namespace, and Kueue reports that by simply not
	// admitting: the arm would measure a queue nothing reached.
	if selectsAllNamespaces(w) != selectsAllNamespaces(g) {
		return fmt.Errorf("it admits from %s, where this arm declared %s",
			admittedNamespaceScope(g), admittedNamespaceScope(w))
	}
	// Fungibility decides whether a workload waits for its preferred flavor or spills to the next one, which
	// changes which flavor a run's pods land on and therefore which node.
	if fungibility(w.Spec.FlavorFungibility) != fungibility(g.Spec.FlavorFungibility) {
		return fmt.Errorf("its flavorFungibility is %q, not the %q this arm declared",
			fungibility(g.Spec.FlavorFungibility), fungibility(w.Spec.FlavorFungibility))
	}
	// Cohort membership decides who can borrow from whom, which is the whole subject of the reclaim study.
	if w.Spec.CohortName != g.Spec.CohortName {
		return fmt.Errorf("it belongs to cohort %q, not %q", g.Spec.CohortName, w.Spec.CohortName)
	}
	// Quota is capacity, and capacity decides whether borrowing was ever possible. A queue adopted with a
	// different nominal quota changes what the run is able to observe before any policy applies.
	if err := sameQuota(w.Spec.ResourceGroups, g.Spec.ResourceGroups); err != nil {
		return err
	}
	return nil
}

// The four helpers below render an optional policy field as a single comparable string.
//
// Each of these is a pointer or a nested struct where "unset" and "set to the zero value" are different
// facts, and comparing the pointers directly would compare addresses. Rendering both sides through the same
// function is also what puts the same spelling in the comparison and in the error message.
func stopPolicy(p *kueuev1beta2.StopPolicy) string {
	if p == nil {
		return "None"
	}
	return string(*p)
}

func borrowPolicy(b *kueuev1beta2.BorrowWithinCohort) string {
	if b == nil {
		return "Never"
	}
	return string(b.Policy)
}

func fungibility(f *kueuev1beta2.FlavorFungibility) string {
	if f == nil {
		return "default"
	}
	return fmt.Sprintf("%s/%s", f.WhenCanBorrow, f.WhenCanPreempt)
}

// selectsAllNamespaces reports whether the queue's selector matches every namespace, which is what an empty
// (but present) selector means. A nil selector matches NOTHING, so the two are opposites rather than
// variations, and the distinction is the whole reason this is not a nil check.
func selectsAllNamespaces(cq *kueuev1beta2.ClusterQueue) bool {
	s := cq.Spec.NamespaceSelector
	return s != nil && len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
}

func admittedNamespaceScope(cq *kueuev1beta2.ClusterQueue) string {
	if selectsAllNamespaces(cq) {
		return "every namespace"
	}
	if cq.Spec.NamespaceSelector == nil {
		return "no namespace (its selector is absent)"
	}
	return fmt.Sprintf("a restricted set of namespaces (%v)", cq.Spec.NamespaceSelector)
}

// sameQuota compares covered resources, the flavor each is charged against, and the nominal amounts.
func sameQuota(want, got []kueuev1beta2.ResourceGroup) error {
	if len(want) != len(got) {
		return fmt.Errorf("it declares %d resource groups, not %d", len(got), len(want))
	}
	for i := range want {
		if !slices.Equal(want[i].CoveredResources, got[i].CoveredResources) {
			return fmt.Errorf("its covered resources are %v, not %v", got[i].CoveredResources, want[i].CoveredResources)
		}
		if len(want[i].Flavors) != len(got[i].Flavors) {
			return fmt.Errorf("it declares %d flavors, not %d", len(got[i].Flavors), len(want[i].Flavors))
		}
		for j := range want[i].Flavors {
			wf, gf := want[i].Flavors[j], got[i].Flavors[j]
			if wf.Name != gf.Name {
				return fmt.Errorf("its quota is charged against flavor %q, not %q", gf.Name, wf.Name)
			}
			if len(wf.Resources) != len(gf.Resources) {
				return fmt.Errorf("flavor %q declares %d resources, not %d", wf.Name, len(gf.Resources), len(wf.Resources))
			}
			for k := range wf.Resources {
				wr, gr := wf.Resources[k], gf.Resources[k]
				if wr.Name != gr.Name {
					return fmt.Errorf("flavor %q covers %q, not %q", wf.Name, gr.Name, wr.Name)
				}
				if wr.NominalQuota.Cmp(gr.NominalQuota) != 0 {
					return fmt.Errorf("flavor %q gives %q a nominal quota of %s, not %s",
						wf.Name, wr.Name, gr.NominalQuota.String(), wr.NominalQuota.String())
				}
			}
		}
	}
	return nil
}

// sameFlavor compares the fields that isolate one run's workloads onto its own dedicated node.
//
// The variant label on a flavor says which arm it was BUILT for and nothing about whether its isolation is
// intact. A flavor adopted without its node selector, taint or toleration lets this run's pods land beside
// another run's, which contaminates every timing the run then reports.
func sameFlavor(w, g *kueuev1beta2.ResourceFlavor) error {
	if !maps.Equal(w.Spec.NodeLabels, g.Spec.NodeLabels) {
		return fmt.Errorf("its nodeLabels are %v, not the %v that pin this run to its own worker",
			g.Spec.NodeLabels, w.Spec.NodeLabels)
	}
	if len(w.Spec.NodeTaints) != len(g.Spec.NodeTaints) {
		return fmt.Errorf("it carries %d node taints, not %d", len(g.Spec.NodeTaints), len(w.Spec.NodeTaints))
	}
	for i := range w.Spec.NodeTaints {
		wt, gt := w.Spec.NodeTaints[i], g.Spec.NodeTaints[i]
		if wt.Key != gt.Key || wt.Value != gt.Value || wt.Effect != gt.Effect {
			return fmt.Errorf("its node taint is %s=%s:%s, not %s=%s:%s",
				gt.Key, gt.Value, gt.Effect, wt.Key, wt.Value, wt.Effect)
		}
	}
	if len(w.Spec.Tolerations) != len(g.Spec.Tolerations) {
		return fmt.Errorf("it carries %d tolerations, not %d", len(g.Spec.Tolerations), len(w.Spec.Tolerations))
	}
	for i := range w.Spec.Tolerations {
		wt, gt := w.Spec.Tolerations[i], g.Spec.Tolerations[i]
		if wt.Key != gt.Key || wt.Value != gt.Value || wt.Effect != gt.Effect {
			return fmt.Errorf("its toleration is %s=%s:%s, not %s=%s:%s",
				gt.Key, gt.Value, gt.Effect, wt.Key, wt.Value, wt.Effect)
		}
	}
	return nil
}

// DevicePods is the resolver a device observer's samples are attributed through.
//
// It is exposed rather than passed at construction because the scraper starts after the streams do: the
// registry has to be filling before the first scrape lands, or the earliest samples resolve to nothing.
func (col *collector) DevicePods() queuelab.PodResolver {
	return col.devicePods.Resolve
}
