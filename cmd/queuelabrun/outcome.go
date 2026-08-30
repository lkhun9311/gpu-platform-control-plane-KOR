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
)

// disposition is what happened to one invocation.
//
// The set is derived from the return graph of main and run rather than invented, because two earlier
// attempts at an asserted list did not survive a walk of the real returns.
type disposition string

const (
	dispRefusedBeforeCluster = disposition("refused-before-cluster")
	dispClientFailed         = disposition("client-failed")
	dispProtocolBuildFailed  = disposition("protocol-build-failed")
	dispAcquisitionRefused   = disposition("acquisition-refused")
	// dispEnvironmentUnqualified is the worker being the wrong machine to measure on, which is a fact about
	// the cluster rather than a failure of this program to do something.
	//
	// It is not folded into setup-failed, and the distinction is the point of having a disposition at all: a
	// setup failure is this run's own Create being refused and the operator's move is to look at this run,
	// while an unqualified environment is a leftover GPU Pod, a cordon or a node too small for the arm and the
	// operator's move is to look at the cluster. Collapsing them would make every record carrying either one
	// require its free-text reason to be read before it could be classified, which is what the disposition
	// exists to make unnecessary.
	dispEnvironmentUnqualified = disposition("environment-unqualified")
	dispSetupFailed            = disposition("setup-failed")
	dispCancelled              = disposition("cancelled")
	// There is deliberately no "observation-failed": waitForHorizon has exactly two returns, a
	// cancellation-wrapped error and nil, so an observation window can only end early by being cancelled.
	// The failures that happen DURING observation are already named — a barrier that cannot be met desyncs
	// the ledger and lands on collector-desync — so a constant for a non-cancellation observation failure
	// would be a slot for a path no code can take.
	dispCollectorDesync    = disposition("collector-desync")
	dispReconstructRefused = disposition("reconstruct-refused")
	dispCardinalityRefused = disposition("cardinality-refused")
	// dispDeviceNotEstablished is a run that completed its checks and produced no device evidence, on an
	// invocation that declared device evidence was the deliverable.
	//
	// It is a distinct disposition rather than a desync because nothing about the CLUSTER went wrong: the
	// protocol ran, the ledger is intact, and the control-plane figures in the record are as good as any
	// other run's. What failed is that the run was bought for something it did not return.
	dispDeviceNotEstablished = disposition("device-not-established")
	dispWorkerNotRestored    = disposition("worker-not-restored")
	// dispResidueLeft is a fact about the cluster, not a failure to compute one: teardown ran and something is
	// still standing at one of this run's names. Usually that is an object this run created; it can also be
	// one another transaction holds, which this run may not touch and which the next run under this id will
	// collide with just the same, so both are reported here and the record names which.
	//
	// It is separate from worker-not-restored because the worker is not restored on this path WHEN THE
	// LEFTOVER IS OURS — releasing a node whose namespace still holds GPUs is the outcome teardown exists to
	// prevent — so collapsing the two would make a chosen containment look like a failed release the operator
	// should retry. Where every leftover belongs to somebody else the worker does go back (residueHoldsWorker
	// draws that line), and this disposition then reports the collision alone.
	dispResidueLeft = disposition("residue-left")
	// dispChecksPassed is deliberately not called "valid", and it keeps that name now that the four gates it
	// was named around all exist. The residual recordUnchecked names is real — nothing here travels the
	// operator's reconcile loop, Kueue's admission or the Job controller — so the strongest statement available
	// is still that the checks this build implements passed. A rename to "valid" on the day the last gate
	// landed would have been the overclaim this spelling exists to refuse, and the wire value is read by
	// decodeRunRecord besides.
	dispChecksPassed = disposition("completed-implemented-checks-passed")
	// dispUnclassified is not reachable by design; it exists so that a bug which makes it reachable says so.
	//
	// Every return in run sets a disposition, but nothing in the language enforces that: deleting one
	// assignment leaves the zero value, which the compiler and go vet both accept and which would be written
	// as an empty disposition — a record that claims nothing while looking like a record. buildRecord
	// substitutes this instead, so the failure names itself in the artifact rather than being a blank field
	// a reader has to notice.
	dispUnclassified = disposition("unclassified-internal-error")
)

// outcome is what run returns instead of a bare error, so its deferred cleanup can amend it.
//
// There is deliberately no error field. The record has nowhere to put one, and both classifiers below
// already fold the cause into Reason, so an error carried alongside would be written by every path and read
// by none — a field with no reader is the same dead weight as a disposition with no producer.
type outcome struct {
	Disposition disposition
	Reason      string
}

// amend replaces the disposition while preserving what was originally decided as the cause.
//
// The deferred emergency release runs after run has chosen its return value, so without this the record
// could be written and then contradicted by a WORKER NOT RESTORED line moments later.
func (o outcome) amend(d disposition, reason string) outcome {
	if o.Reason != "" {
		reason = reason + " (after " + o.Reason + ")"
	}
	return outcome{Disposition: d, Reason: reason}
}

// classifyPhaseFailure applies the causal precedence rule.
//
// Cancellation outranks a phase only when that phase terminated because it observed cancellation; a phase
// that failed on its own terms keeps its own disposition even if a signal is pending elsewhere.
//
// err is inspected rather than stored: it decides the precedence, and the caller has already folded its
// text into reason.
func classifyPhaseFailure(phase disposition, reason string, err error) outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return outcome{Disposition: dispCancelled, Reason: reason}
	}
	return outcome{Disposition: phase, Reason: reason}
}

// classifyReleaseFailure is separate because restoration wins once it has begun.
//
// The release runs on cleanupContext rather than the signal-cancelled run context, so a cancellation
// surfacing beneath it does not mean the run was cancelled — it means restoration could not be proven.
func classifyReleaseFailure(err error) outcome {
	return outcome{Disposition: dispWorkerNotRestored, Reason: fmt.Sprintf("release: %v", err)}
}
