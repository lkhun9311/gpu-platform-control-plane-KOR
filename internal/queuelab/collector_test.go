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

package queuelab

import (
	"testing"
	"time"
)

func TestLedgerBuilderEmitsOnlyTransitions(t *testing.T) {
	b := NewLedgerBuilder()
	ready := ObservedState{Event: EventPodReady, Reason: "Ready"}
	// The same Ready observed three times (relists, no-op modifies) must produce ONE event.
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ready, 10)
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ready, 11)
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ready, 12)
	if err := b.Err(); err != nil {
		t.Fatal(err)
	}
	if n := len(b.Events()); n != 1 {
		t.Fatalf("repeated Ready should emit one event, got %d", n)
	}
	if b.Events()[0].ElapsedNs != 10 {
		t.Fatalf("first transition time should be recorded, got %d", b.Events()[0].ElapsedNs)
	}
}

func TestLedgerBuilderInvalidatesOnBadClassification(t *testing.T) {
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, kindWorkload, "wl-1", "a1", ObservedState{Invalid: true, InvalidReason: "unexpected eviction"}, 5)
	if b.Err() == nil {
		t.Fatalf("an Invalid classification must invalidate the run")
	}
}

func TestLedgerBuilderReadyPodDeletedWithoutTerminalInvalidates(t *testing.T) {
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ObservedState{Event: EventPodReady}, 10)
	// The delete carries a still-Running (non-terminal) object: a force delete or lost terminal update.
	b.Observe(DeltaDelete, kindPod, "pod-1", "a1", ObservedState{}, 20)
	if b.Err() == nil {
		t.Fatalf("a Ready pod deleted without a terminal state must invalidate the run")
	}
}

func TestLedgerBuilderGracefulStopIsNotInvalid(t *testing.T) {
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ObservedState{Event: EventPodReady}, 10)
	// The delete carries a terminal object, so AttemptStopped is recorded and the tombstone check passes.
	b.Observe(DeltaDelete, kindPod, "pod-1", "a1", ObservedState{Event: EventAttemptStopped, Reason: "Failed"}, 20)
	if err := b.Err(); err != nil {
		t.Fatalf("a gracefully terminated pod must not invalidate: %v", err)
	}
	var stops int
	for _, e := range b.Events() {
		if e.Type == EventAttemptStopped {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("the terminal delete should record one AttemptStopped, got %d", stops)
	}
}

func TestLedgerBuilderMarkVanished(t *testing.T) {
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ObservedState{Event: EventPodReady}, 10)
	b.MarkVanished("pod-1")
	if b.Err() == nil {
		t.Fatalf("a Ready pod vanishing on relist must invalidate the run")
	}
}

func TestLedgerBuilderStoppedPodMayVanish(t *testing.T) {
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ObservedState{Event: EventPodReady}, 10)
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ObservedState{Event: EventAttemptStopped}, 20)
	b.MarkVanished("pod-1") // already stopped, so its disappearance is expected
	if err := b.Err(); err != nil {
		t.Fatalf("a stopped pod may vanish without invalidating: %v", err)
	}
}

func TestLedgerBuilderDesyncInvalidates(t *testing.T) {
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, kindWorkload, "wl-1", "a1", ObservedState{Event: EventAdmitted}, 5)
	b.Desync("410 Gone, resourceVersion too old")
	if b.Err() == nil {
		t.Fatalf("an unrecoverable watch desync must invalidate the run")
	}
}

func TestLedgerBuilderFeedsReconstruct(t *testing.T) {
	// End-to-end at the unit level: the builder's ledger reconstructs into the expected outcome, proving the
	// collector output and Reconstruct input agree.
	b := NewLedgerBuilder()
	b.Observe(DeltaUpsert, "MLTrainingJob", "mltj-1", "a1", ObservedState{Event: EventSubmitted}, 0)
	b.Observe(DeltaUpsert, kindWorkload, "wl-1", "a1", ObservedState{Event: EventAdmitted}, 1*sec)
	b.Observe(DeltaUpsert, kindPod, "pod-1", "a1", ObservedState{Event: EventPodReady}, 2*sec)
	b.Observe(DeltaUpsert, "Job", "job-1", "a1", ObservedState{Event: EventCompleted}, 50*sec)
	if err := b.Err(); err != nil {
		t.Fatal(err)
	}
	trace := []TrainingTraceRow{{Index: 0, Name: "a1", OffsetMs: 0, Tenant: "tenant-a", GPUCount: 1, DurationSec: 60}}
	res, err := Reconstruct("Any", trace, b.Events(), 100*sec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Admitted != 1 || res.Completed != 1 {
		t.Fatalf("reconstructed result = admitted %d completed %d, want 1/1", res.Admitted, res.Completed)
	}
}

// Every persisted event must be datable in absolute terms, and the wall times must not disagree with the
// order the events happened in.
//
// The schema said WallUnixNanos was "kept for provenance" and no live producer ever set it, so every ledger
// this harness wrote could only be read relative to a t0 the document did not carry.
//
// Mutation that turns this red: drop the AnchorWallClock call, or sample time.Now() per event instead of
// deriving from the anchor.
func TestEventsCarryAWallClockDerivedFromOneAnchor(t *testing.T) {
	anchor := time.Unix(1_700_000_000, 0)
	b := NewLedgerBuilder()
	b.AnchorWallClock(anchor)

	b.Observe(DeltaUpsert, "MLTrainingJob", "uid-1", "a1",
		ObservedState{Event: EventSubmitted}, 0)
	b.Observe(DeltaUpsert, "Pod", "uid-2", "a1",
		ObservedState{Event: EventPodReady}, 5_000_000_000)
	b.Observe(DeltaUpsert, "Pod", "uid-2", "a1",
		ObservedState{Event: EventAttemptStopped, Reason: StopReasonSucceeded}, 9_000_000_000)

	events := b.Events()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	for _, e := range events {
		if e.WallUnixNanos == 0 {
			t.Fatalf("%s carries no wall clock; the record cannot be dated", e.Type)
		}
		if want := anchor.UnixNano() + e.ElapsedNs; e.WallUnixNanos != want {
			t.Fatalf("%s wall=%d, want %d (anchor + elapsed)", e.Type, e.WallUnixNanos, want)
		}
	}
	// Derived from one anchor, so wall order can never contradict elapsed order — which is the whole reason
	// not to sample the clock per event.
	for i := 1; i < len(events); i++ {
		if events[i].WallUnixNanos < events[i-1].WallUnixNanos {
			t.Fatalf("event %d is dated before its predecessor", i)
		}
	}

	// The control: without an anchor the field stays zero rather than inventing a time. A builder that made
	// one up would be worse than one that admits it has none.
	plain := NewLedgerBuilder()
	plain.Observe(DeltaUpsert, "MLTrainingJob", "uid-1", "a1", ObservedState{Event: EventSubmitted}, 0)
	if got := plain.Events()[0].WallUnixNanos; got != 0 {
		t.Fatalf("an unanchored builder dated an event %d; it has no wall clock to date it with", got)
	}
}
