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
	"strings"
	"testing"
)

// Cancellation outranks a phase only when that phase terminated because it observed cancellation.
//
// The explicit worker release runs on cleanupContext, not the signal-cancelled run context, so a signal
// arriving while restoration is in flight must not relabel the run: restoration has already begun and its
// outcome is the one that matters.
func TestCancellationPrecedenceIsCausalNotIncidental(t *testing.T) {
	cancelled := context.Canceled

	// A phase that ended because it saw cancellation is cancelled.
	got := classifyPhaseFailure(dispSetupFailed, "applying fixtures", cancelled)
	if got.Disposition != dispCancelled {
		t.Fatalf("a phase that observed cancellation is cancelled, got %s", got.Disposition)
	}

	// A phase that failed on its own terms keeps its own disposition even if a signal is pending.
	got = classifyPhaseFailure(dispSetupFailed, "applying fixtures", errors.New("clusterqueue rejected"))
	if got.Disposition != dispSetupFailed {
		t.Fatalf("a phase that failed on its own terms keeps its disposition, got %s", got.Disposition)
	}

	// Restoration that fails is worker-not-restored, never cancelled, even when the cause is a cancelled
	// context somewhere beneath it.
	got = classifyReleaseFailure(cancelled)
	if got.Disposition != dispWorkerNotRestored {
		t.Fatalf("a failed release is worker-not-restored, got %s", got.Disposition)
	}
}

// The deferred emergency release runs after run() has chosen its return value, so it must be able to
// amend the outcome rather than only print, or the record can be contradicted the instant after it is
// written.
func TestAmendReplacesTheDispositionAndKeepsTheOriginalReason(t *testing.T) {
	o := outcome{Disposition: dispReconstructRefused, Reason: "cardinality"}
	a := o.amend(dispWorkerNotRestored, "emergency release failed")
	if a.Disposition != dispWorkerNotRestored {
		t.Fatalf("amend must replace the disposition, got %s", a.Disposition)
	}
	// A reason that merely differs from the original (e.g. because the new reason string happens to be
	// different) is not proof the original survived — it could equally mean amend discarded it outright.
	// Asserting containment of both strings is the only way to tell "replaced but chained" from "replaced
	// entirely."
	if !strings.Contains(a.Reason, o.Reason) {
		t.Fatalf("amend must keep the original reason inside the amended one, got %q, want it to contain %q", a.Reason, o.Reason)
	}
	if !strings.Contains(a.Reason, "emergency release failed") {
		t.Fatalf("amend must record why it amended, not silently keep the old reason alone, got %q", a.Reason)
	}
}
