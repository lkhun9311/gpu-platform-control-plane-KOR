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
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// streamScope is which objects one stream covers: the selector handed to its List and to every watch that
// resumes it, plus how that selector reads in a refusal.
//
// It replaces the bare namespace this file used to take, because the ownership window watches the worker
// Node and a Node is cluster-scoped. Threading a scope rather than writing a sibling keeps the parts that
// decide whether a view is continuous — the resume version, the baseline refusal, the ending semantics —
// as one implementation with one set of tests behind it. A second copy would be a hundred lines that start
// identical and drift silently, and continuity is not a property to hold in two places.
//
// The change is deliberately confined to the selector. namespaceScope produces exactly the option and
// exactly the wording the namespaced callers had before it existed, so a namespaced stream is byte-for-byte
// the stream it was.
type streamScope struct {
	// desc is how the scope reads inside the two refusals below, phrased to follow the kind: "of PodList in
	// ns-a". It is not derived from opts, because a caller that selects nothing still has to say so —
	// "cluster-wide" is a scope, and an empty string there would produce a refusal that names no scope at all.
	desc string
	opts []client.ListOption
}

// namespaceScope is what the collector's four streams watch: one kind inside the run's own namespace.
func namespaceScope(ns string) streamScope {
	return streamScope{desc: "in " + ns, opts: []client.ListOption{client.InNamespace(ns)}}
}

// clusterScope is the whole of a cluster-scoped kind, with no selector at all.
//
// The single-object case — the ownership window wants one Node — filters by name in its own consumer rather
// than through a metadata.name field selector, for the reason qualifyWorker already gives about Pods: the
// local comparison is what decides either way, so a server-side selector this code cannot verify would still
// have to be re-checked against everything it returned. It cannot narrow the RBAC either, since watch on
// nodes is cluster-scoped whether or not a selector is attached, and on a lab cluster the difference is a
// handful of objects.
func clusterScope() streamScope { return streamScope{desc: "cluster-wide"} }

// watchAdapter presents controller-runtime's client as the cache.WatcherWithContext that RetryWatcher
// needs.
//
// A second client-go client would also satisfy RetryWatcher, but it would duplicate the scheme, the REST
// config and the decoding path for the two CRDs this runner watches, for no behaviour the adapter cannot
// provide.
type watchAdapter struct {
	c       client.WithWatch
	scope   streamScope
	newList func() client.ObjectList

	once        sync.Once
	established chan struct{}
}

func newWatchAdapter(c client.WithWatch, scope streamScope, newList func() client.ObjectList) *watchAdapter {
	return &watchAdapter{c: c, scope: scope, newList: newList, established: make(chan struct{})}
}

// WatchWithContext starts one underlying watch at the resume version RetryWatcher asks for.
//
// The options are passed as Raw because that is the only path by which resourceVersion and the bookmark
// request reach the API server through controller-runtime; dropping them would silently restart the stream
// from now and lose every edge in the gap.
func (a *watchAdapter) WatchWithContext(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	// A fresh slice per call rather than one built once at construction: these are handed to a client that may
	// hold them, and every reconnect passes a different Raw, so a shared backing array would let one watch's
	// resume version reach another's request.
	lopts := make([]client.ListOption, 0, len(a.scope.opts)+1)
	lopts = append(lopts, a.scope.opts...)
	lopts = append(lopts, &client.ListOptions{Raw: &opts})
	w, err := a.c.Watch(ctx, a.newList(), lopts...)
	if err != nil {
		return nil, err
	}
	// Signalling here rather than at construction is the point: RetryWatcher builds before its first watch
	// has necessarily succeeded, so a barrier keyed on construction can release while the watch is still
	// failing and the run would burn experimental time unobserved.
	a.once.Do(func() { close(a.established) })
	return w, nil
}

// Established closes once at least one underlying watch has been successfully established.
//
// It does not mean the watch is currently healthy: an established watch can close immediately afterwards,
// and RetryWatcher will reconnect from the same resource version. Promising liveness would be promising
// something this type cannot observe.
func (a *watchAdapter) Established() <-chan struct{} { return a.established }

// streamBaseline is what the initial List established, and deliberately nothing more.
//
// The count is a plain number: deciding which of those objects carry lifecycle meaning is the integration
// slice's rule, and a component that answered it here would have decided a question this slice defers.
type streamBaseline struct {
	ResourceVersion string
	Objects         int
}

// streamEnd is what can actually be observed about why a stream stopped.
//
// RetryWatcher exposes only forwarded events, the closure of its channels, the caller's own ctx.Err() and
// whether the caller asked to stop. Some permanent establishment errors close without forwarding anything,
// cancellation races with forwarding, and a terminal error event can be lost when the internal send selects
// the cancelled context — so "410 versus another permanent reason" is not a distinction this type can
// always make.
//
// Cancelled and Stopped are the two endings the caller itself caused, and an ending with neither set is
// always a loss: the stream ended on its own, so the gap since the last delivered version can never be
// closed, whether or not LastStatus names a cause. Reading "not cancelled" alone as a lost stream would
// condemn every orderly shutdown, which is why Stopped exists.
//
// But either flag being set proves only that the ending is not yet shown to be a loss — never that it was
// orderly. Do not read this struct as licensing `orderly := end.Cancelled || end.Stopped`; Ended explains
// why that is weaker than it looks and where a real verdict has to come from.
//
// Stopped says the caller asked to stop before the stream finished ending, not that the stop was the cause,
// because a stop can race with a stream that was already dying.
//
// LastStatus is independent evidence when it is present, but its absence proves nothing: the wrapped
// Forbidden path terminates permanently while forwarding no event at all, so a stop racing that loss reads
// as Stopped with no status. These three fields can therefore confirm a loss and can never confirm an
// orderly ending; Ended documents that asymmetry and what an adopter has to do about it.
type streamEnd struct {
	Cancelled  bool
	Stopped    bool
	LastStatus *metav1.Status
}

// watchStream is one kind's continuous view: a baseline List, then a RetryWatcher resuming from it.
type watchStream struct {
	baseline streamBaseline
	adapter  *watchAdapter
	rw       *watchtools.RetryWatcher

	out  chan watch.Event
	done chan struct{}

	// stop is a second way out of the forwarding select, because cancelling RetryWatcher only closes the
	// channel forward reads from and cannot reach forward while it is parked on a send nobody is receiving.
	stopOnce sync.Once
	stop     chan struct{}

	mu  sync.Mutex
	end streamEnd
}

// startWatchStream opens one kind's continuous view: a baseline List, then a watch resuming from exactly
// the point that List established.
//
// ctx governs the whole stream and not merely the baseline List, which is the trap worth stating outright.
// A caller bounding establishment with context.WithTimeout hands that deadline to the stream as well, so
// every stream dies when it expires and the ending reads as Cancelled — the caller's own shutdown
// signature. A run that stopped observing thirty seconds in would then be reported as a clean shutdown.
// Give the stream the run's own context and bound establishment by selecting on Established() against a
// timer instead.
func startWatchStream(ctx context.Context, c client.WithWatch, scope streamScope,
	newList func() client.ObjectList) (*watchStream, error) {
	list := newList()
	// Every stream the collector opens watches the same namespace, so the scope alone identifies none of them
	// and all four would fail with byte-identical text; the list type is what tells them apart in a log.
	kind := fmt.Sprintf("%T", list)
	if err := c.List(ctx, list, scope.opts...); err != nil {
		return nil, fmt.Errorf("baseline list of %s %s: %w", kind, scope.desc, err)
	}
	rv := list.GetResourceVersion()
	// RetryWatcher rejects both, and for the same reason this component must: an empty version starts the
	// watch from now, and "0" starts it from an arbitrary cached point, so neither gives a known baseline
	// that a later gap can be measured against.
	if rv == "" || rv == "0" {
		return nil, fmt.Errorf("baseline list of %s %s returned resource version %q, which is not a resumable point", kind, scope.desc, rv)
	}
	// ExtractList rather than meta.LenList because LenList reports 0 for anything it cannot walk, and a
	// baseline that silently reads as empty is exactly the kind of unobserved claim this component refuses.
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, fmt.Errorf("count baseline %s objects %s: %w", kind, scope.desc, err)
	}
	n := len(items)

	a := newWatchAdapter(c, scope, newList)
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, rv, a)
	if err != nil {
		return nil, fmt.Errorf("start watch of %s %s from %s: %w", kind, scope.desc, rv, err)
	}

	s := &watchStream{
		baseline: streamBaseline{ResourceVersion: rv, Objects: n},
		adapter:  a,
		rw:       rw,
		out:      make(chan watch.Event),
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
	}
	go s.forward(ctx)
	return s, nil
}

// forward republishes the watcher's events and records what could be observed about the ending.
//
// The last forwarded error status is kept because that is where a 410 shows up when one is delivered at
// all; it is evidence when present and absent otherwise, never a claim about the cause.
func (s *watchStream) forward(ctx context.Context) {
	defer close(s.done)
	defer close(s.out)
	for ev := range s.rw.ResultChan() {
		if ev.Type == watch.Error {
			if st, ok := ev.Object.(*metav1.Status); ok {
				s.mu.Lock()
				s.end.LastStatus = st
				s.mu.Unlock()
			}
		}
		select {
		case s.out <- ev:
		case <-ctx.Done():
			// The caller is going away; stop republishing rather than blocking on a channel nobody reads.
		case <-s.stop:
			// Stop has to be sufficient on its own, because a caller that stops between reads leaves an event
			// parked here and would otherwise wait on an End that this goroutine can no longer reach.
		}
	}
	// Read the caller's context and the stop request only after the watcher has actually stopped, so an
	// ending the caller caused while the stream was already ending for another reason is not mistaken for
	// the reason.
	s.mu.Lock()
	s.end.Cancelled = ctx.Err() != nil
	select {
	case <-s.stop:
		s.end.Stopped = true
	default:
	}
	s.mu.Unlock()
}

func (s *watchStream) Baseline() streamBaseline       { return s.baseline }
func (s *watchStream) ResultChan() <-chan watch.Event { return s.out }
func (s *watchStream) End() <-chan struct{}           { return s.done }

// Established closes once an underlying watch has succeeded, and may never close at all.
//
// A permanent pre-establishment failure — a Forbidden that arrives wrapped is the one this package has
// reproduced — ends the stream having never established anything, and startWatchStream has already returned
// a nil error by then. A barrier written as a bare receive over several streams therefore waits forever on
// the one that died; select it against End() and treat that arm as a failed stream, not as a slow one.
func (s *watchStream) Established() <-chan struct{} { return s.adapter.Established() }

// Stop ends the stream and is enough on its own: after it returns, End closes whatever forward was doing.
//
// The close is guarded because Stop is idempotent by contract and a caller racing a stream that is already
// ending on its own must not panic on a second close.
func (s *watchStream) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.rw.Stop()
}

// Ended reports what was observed about the ending, and is only meaningful after End() is closed.
//
// The reading is one-sided on purpose, because what is here can confirm a loss and can never confirm the
// absence of one:
//
//   - Neither Cancelled nor Stopped means the stream ended on its own, which is always a lost gap.
//   - A non-nil LastStatus is a terminal status the stream actually saw, which is also always a lost gap.
//   - Cancelled or Stopped with no status is only an absence of evidence, because a permanent failure that
//     forwards nothing can arrive concurrently with the shutdown that set either flag.
//
// No check the caller can perform closes that last hole, and sampling End() before stopping actively makes
// things worse. If the stream had already ended on its own, the flags alone already reject it and the
// sample adds nothing; if it is ending concurrently, End() has not closed yet and the sample permits
// exactly the case it was meant to catch; and on an ordinary cancelled shutdown, where End() has
// legitimately closed first, the sample condemns a run that was fine.
//
// An adopter that needs a real orderliness verdict has to source it from outside this type — a final List
// once the stream has ended, comparing that resourceVersion against the last one delivered. This package's
// own adopter, the collector, deliberately does NOT do that, and the reasons belong here so the next one
// does not spend an afternoon reaching them again. A list's resourceVersion is the store's current position
// and advances on writes anywhere in the cluster, so on a healthy run it stands above the last delivered
// version and proves nothing; and the collector's streams end at the observation horizon, past which every
// change is censored by design, so a Pod that final List found absent is an ordinary post-horizon
// termination rather than a missed stop. It refuses the endings this type cannot vouch for instead, which is
// a verdict it can actually make.
//
// What this type does guarantee, for an adopter whose window really does end where its stream does, is a
// property rather than an intention: forward defers close(done) before close(out), so LIFO closes out first,
// and End() being closed already implies ResultChan() is closed and no further event can arrive. A List
// issued after End() therefore cannot race a terminal event still in flight — which the collector's former
// in-loop relist could, and did, when it marked a Pod vanished.
func (s *watchStream) Ended() streamEnd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}
