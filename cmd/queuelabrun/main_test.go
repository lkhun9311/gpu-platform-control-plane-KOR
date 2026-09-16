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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// A signal landing during the observation window must be seen immediately, not after the wait times out on
// its own; a poll-to-deadline bug here would make Ctrl-C during a long horizon look hung for the remainder
// of the run.
func TestWaitForHorizonCancelledContextReturnsErrorPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Far enough out that only a genuine ctx.Done() short-circuit, not the deadline itself, could return
	// this quickly.
	deadline := time.Now().Add(time.Hour)

	start := time.Now()
	err := waitForHorizon(ctx, deadline)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want an error wrapping context.Canceled, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("waitForHorizon took %v to return on an already-cancelled context; it must not wait for the deadline", elapsed)
	}
}

// The ordinary path — the deadline is reached and nothing cancelled — must still return nil so the caller
// falls through to publication exactly as before this task's change.
func TestWaitForHorizonReturnsNilOnceDeadlinePasses(t *testing.T) {
	deadline := time.Now().Add(50 * time.Millisecond)
	if err := waitForHorizon(context.Background(), deadline); err != nil {
		t.Fatalf("want nil once the deadline has passed, got %v", err)
	}
}

// A deadline already in the past must not block at all, cancelled context or not: there is nothing left to
// observe.
func TestWaitForHorizonPastDeadlineReturnsImmediately(t *testing.T) {
	start := time.Now()
	if err := waitForHorizon(context.Background(), time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("want nil for a deadline already in the past, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("waitForHorizon took %v for an already-past deadline", elapsed)
	}
}

// Gate 0 item 18, and the reason this is a P0 rather than a tidiness point: the horizon the reconstruction
// censors against used to be col.elapsed(), read AFTER the collector had been cancelled and joined. That is
// the observation window plus however long shutdown took — a different boundary on every run, and a wider
// one exactly when the API server is slow, so two runs of the same protocol would be censored at two
// different instants and their numbers would not be comparable.
//
// The test drives reconstructAtHorizon, which is the real call site, twice with a shutdown-shaped delay
// between them. Both the value and its independence from that delay are asserted: a horizon that moved with
// the clock would give the first call a boundary of a few microseconds, which is before every event here.
func TestReconstructHorizonIsTheStampedInstantNotWhateverShutdownTook(t *testing.T) {
	const horizon = 30 * time.Second
	const submittedAt = 2 * time.Second

	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	col := newCollector(fc, "ns", "r1", horizon)

	// One offered row that is submitted and then never admitted, because its censored wait is exactly
	// horizon - submitted: the boundary itself, readable straight off the result.
	trace := []queuelab.TrainingTraceRow{
		{Index: 0, Name: "victim", DurationSec: 40},
	}
	events := []queuelab.LifecycleEvent{
		{ElapsedNs: int64(submittedAt), Kind: kindMLTrainingJob, Type: queuelab.EventSubmitted, Job: "victim"},
	}

	first, err := reconstructAtHorizon(col, queuelab.ArmNRef, trace, events)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if got, want := first.Outcomes[0].CensoredWaitNs, int64(horizon-submittedAt); got != want {
		t.Fatalf("censored wait = %v, want %v: the horizon is not the stamped instant",
			time.Duration(got), time.Duration(want))
	}

	// Shutdown takes a while — cancelling the watches, joining four goroutines, a slow API server closing
	// them. None of that may move the boundary.
	time.Sleep(150 * time.Millisecond)

	second, err := reconstructAtHorizon(col, queuelab.ArmNRef, trace, events)
	if err != nil {
		t.Fatalf("reconstruct after the shutdown delay: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("the reconstruction moved with shutdown duration:\n first %+v\nsecond %+v", first, second)
	}
	if col.horizonNs() != int64(horizon) {
		t.Fatalf("horizonNs = %v, want the configured horizon %v", time.Duration(col.horizonNs()), horizon)
	}
}

// dispatchOperatorMode must resolve every one of these without ever reaching the cluster client: if the
// validation-before-client ordering regressed, an operator on a box with no kubeconfig would see a
// "kubeconfig: ..." error instead of the flag-combination message that actually explains their mistake.
// This exercises dispatchOperatorMode itself, not just decideOperatorMode, so it also proves the wiring
// between the two, not only the pure decision in isolation.
//
// The connect function fails the test if it is ever called, which turns "without touching the cluster" from
// something this test relied on incidentally into something it asserts.
func TestDispatchOperatorModeRefusesWithoutTouchingTheCluster(t *testing.T) {
	cases := []struct {
		name      string
		args      operatorModeArgs
		wantFired bool
		wantErr   bool
	}{
		{name: "no mode requested falls through", args: operatorModeArgs{}, wantFired: false, wantErr: false},
		{
			name:      "two modes at once",
			args:      operatorModeArgs{Inspect: true, ReleaseStale: true, TxID: "t"},
			wantFired: true, wantErr: true,
		},
		{
			name:      "mode combined with -arm",
			args:      operatorModeArgs{Arm: "A-honor", Inspect: true},
			wantFired: true, wantErr: true,
		},
		{
			name:      "release-stale missing -confirm-owner-dead",
			args:      operatorModeArgs{ReleaseStale: true, TxID: "tx-1"},
			wantFired: true, wantErr: true,
		},
		{
			name:      "force-release missing -accept-divergence",
			args:      operatorModeArgs{ForceRelease: true, NodeUID: "uid-1"},
			wantFired: true, wantErr: true,
		},
		{
			name:      "clear-quarantine missing -confirm-owner-dead",
			args:      operatorModeArgs{ClearQuarantine: true, QuarantineID: "q1"},
			wantFired: true, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connect := func() (client.WithWatch, error) {
				t.Fatal("a refused invocation must never reach the cluster client")
				return nil, nil
			}
			fired, err := dispatchOperatorMode(connect, tc.args)
			if fired != tc.wantFired {
				t.Fatalf("fired = %v, want %v", fired, tc.wantFired)
			}
			if tc.wantErr && err == nil {
				t.Fatal("want a refusal, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// The bound on the operator modes is only worth anything if the DISPATCH PATH is what applies it, and
// testing operatorModeContext in isolation cannot show that: a regression restoring context.Background() at
// the dispatch site would leave the helper untouched and the suite green.
//
// So this drives the real dispatchOperatorMode against a fake cluster and captures the context the mode
// function is actually handed. It asserts the two properties the design argues for together: the context is
// BOUNDED, so a hung API server cannot hang a recovery indefinitely, and it is LIVE and derived from
// Background rather than from any signal handling, so a Ctrl-C cannot cut a break in half between its Get
// and its Patch.
// Both properties are sampled INSIDE the client call rather than from a captured context afterwards:
// dispatchOperatorMode cancels on the way out, as it must, so a context inspected after it returns is
// always cancelled and would prove the opposite of what this test is for.
func TestDispatchOperatorModeRunsTheModeOnABoundedUncancellableContext(t *testing.T) {
	n := node(nil, nil)
	var (
		called      bool
		deadline    time.Time
		hasDeadline bool
		liveErr     error
	)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			called = true
			deadline, hasDeadline = ctx.Deadline()
			liveErr = ctx.Err()
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	fired, err := dispatchOperatorMode(
		func() (client.WithWatch, error) { return fc, nil },
		operatorModeArgs{Worker: "platform-worker", Inspect: true},
	)
	if !fired {
		t.Fatal("-inspect-worker must fire")
	}
	if err != nil {
		t.Fatalf("inspecting a free node must succeed, got %v", err)
	}
	if !called {
		t.Fatal("the mode never reached the cluster, so this test proved nothing")
	}

	if !hasDeadline {
		t.Fatal("the dispatch path handed the mode an unbounded context; a hung apiserver would hang recovery")
	}
	if remaining := time.Until(deadline); remaining > operatorModeTimeout {
		t.Fatalf("deadline is %v out, want at most operatorModeTimeout (%v)", remaining, operatorModeTimeout)
	}
	if liveErr != nil {
		t.Fatalf("the mode's context must be live while it runs, got %v", liveErr)
	}
}

// This is the acceptance condition the whole change rests on, driven end to end rather than asserted on
// amend in isolation: run() must choose one disposition, its deferred emergency release must change that
// choice after the return value has already been picked, and the record the caller persists must carry the
// amended one.
//
// If it failed, the durable record would say the run failed at setup while the operator's terminal said the
// worker was never restored — two accounts of the same run, with the more serious one missing from the only
// account that survives the process.
//
// The failure is staged through the real client: the namespace Create fails, which is what makes run()
// decide setup-failed, and it poisons the Node reads so the deferred release cannot prove restoration.
func TestRunDeferredEmergencyReleaseAmendsThePersistedRecord(t *testing.T) {
	poisoned := false
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: fakeNodeList,
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					poisoned = true
					return fmt.Errorf("namespace quota exhausted")
				}
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Node); ok && poisoned {
					return fmt.Errorf("apiserver unreachable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	tdNow, tdSleep := fakeClock(time.Unix(0, 0))
	o, events, res, _, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker", selfCompletingProtocol(), time.Duration(horizonSec)*time.Second,
		"", "", "", io.Discard, tdNow, tdSleep)

	if res != nil {
		t.Fatal("a run that never reconstructed anything must hand back no result to render")
	}
	if o.Disposition != dispWorkerNotRestored {
		t.Fatalf("the defer must amend the outcome it found, got %s: %s", o.Disposition, o.Reason)
	}
	if !strings.Contains(o.Reason, "ensuring namespace") {
		t.Fatalf("the amended outcome must keep what run() originally decided as the cause, got %q", o.Reason)
	}

	// The record is what actually survives, so the assertion is made against the bytes on disk rather than
	// against the outcome value the test already holds.
	path := t.TempDir() + "/record.json"
	started := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	if err := writeRecord(path, buildRecord(o, events, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: string(queuelab.ArmAHonor)}, nil, false,
		started, started.Add(90*time.Second))); err != nil {
		t.Fatalf("persist: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	rr, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("the persisted record must decode: %v", err)
	}
	if rr.Disposition != string(dispWorkerNotRestored) {
		t.Fatalf("the persisted record must carry the amended disposition, got %s", rr.Disposition)
	}
	if !strings.Contains(rr.Reason, "ensuring namespace") {
		t.Fatalf("the persisted reason must name both the amendment and its cause, got %q", rr.Reason)
	}
	t.Logf("persisted record:\n%s", b)
}

// This covers exactly two of run()'s seventeen returns — the connect failure and the acquisition refusal —
// and its name says so. An earlier version claimed to cover every path, which it could not: the other
// fifteen need a live cluster, and a reviewer proved the gap by deleting four `o = ...` assignments and
// watching go vet stay silent and the suite stay green. The totality invariant is enforced in
// buildRecord's classified() instead; see TestBuildRecordRefusesAZeroDisposition.
//
// These two are still worth pinning by hand because they bracket the emergency-release defer: the connect
// failure returns before it is registered, and the acquisition refusal is the last return before it is.
func TestRunSetsADispositionOnTheConnectAndAcquisitionPaths(t *testing.T) {
	// A connect failure is the earliest return in run(), before anything is acquired or built.
	tdNow, tdSleep := fakeClock(time.Unix(0, 0))
	o, _, res, _, _, _, _, _ := run(context.Background(),
		func() (client.WithWatch, error) { return nil, fmt.Errorf("kubeconfig: no such file") },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker", selfCompletingProtocol(), time.Duration(horizonSec)*time.Second,
		"", "", "", io.Discard, tdNow, tdSleep)
	if o.Disposition != dispClientFailed {
		t.Fatalf("a failed connect is client-failed, got %q", o.Disposition)
	}
	if res != nil {
		t.Fatal("a run that never connected must hand back no result")
	}

	// An acquisition refusal is the last return before the emergency-release defer is even registered, which
	// is the boundary a future edit is most likely to get wrong.
	held := node(map[string]string{workerLabelKey: "someone-else"}, nil)
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(held).Build()
	o, _, res, _, _, _, _, _ = run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker", selfCompletingProtocol(), time.Duration(horizonSec)*time.Second,
		"", "", "", io.Discard, tdNow, tdSleep)
	if o.Disposition != dispAcquisitionRefused {
		t.Fatalf("a refused acquisition is acquisition-refused, got %q: %s", o.Disposition, o.Reason)
	}
	if res != nil {
		t.Fatal("a run that never acquired its worker must hand back no result")
	}
	if o.Reason == "" {
		t.Fatal("a refusal with no reason tells the operator nothing the record can act on")
	}
}

// The preview note is the one free-text field in either record, so a future writer could fold run data into
// it and hand a preview the reconstructable evidence previewRecord has no field for. It must therefore be the
// same constant whatever the run did.
func TestPreviewRecordNoteIsAConstantNotDerivedFromTheRun(t *testing.T) {
	quiet := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r1", Arm: "A-honor"}, nil, true,
		time.Now(), time.Now()).(previewRecord)
	busy := buildRecord(outcome{Disposition: dispCancelled, Reason: "observing until the horizon"},
		[]queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}}, nil, nil, nil, nil, nil, recordIdentity{RunID: "r2", Arm: "N-ref"}, nil, true,
		time.Now(), time.Now()).(previewRecord)

	if quiet.Note != previewNote || busy.Note != previewNote {
		t.Fatalf("the note must be the fixed constant, got %q and %q", quiet.Note, busy.Note)
	}
	if quiet.Note != busy.Note {
		t.Fatal("the note varied with the run, which is the smuggling path a constant exists to close")
	}
}

// The note is persisted into every preview record, so it is a durable statement read by somebody who has only
// the file — and it said the validity gates were not enforced, which was false of every build that wrote it.
// run() takes no preview flag, so no gate has ever been waived for a preview. That is the same defect the
// UnimplementedGates field was already fixed for, and it fails the same way: a reader who checks the claim and
// finds it false learns to discount the rest of the note, conclusion included.
//
// The assertions are on the SHAPE of the claim, not on the exact wording, because the wording may be improved
// and the two properties may not: the note must not say checking was weakened, and it must name the ledger it
// actually withholds.
//
// Mutation that turns this red: restore
// "preview: the validity gates were not enforced, so this is a smoke check and not evidence".
func TestThePreviewNoteNamesTheWithheldLedgerAndNoWaivedGate(t *testing.T) {
	// Every spelling the old note and its neighbours reached for. A preview differs from a run in what it
	// PERSISTS, so any of these describes the build wrongly.
	for _, falsehood := range []string{"not enforced", "gates were", "without the validity gates", "not checked"} {
		if strings.Contains(previewNote, falsehood) {
			t.Fatalf("the note claims checking was weakened (%q), which no build has ever done to a preview: %q",
				falsehood, previewNote)
		}
	}
	// The difference that does exist, and the half a reader acts on.
	if !strings.Contains(previewNote, "ledger") {
		t.Fatalf("the note must name what a preview actually withholds — the events ledger — or it explains "+
			"the withholding with something other than the reason for it: %q", previewNote)
	}
	if !strings.Contains(previewNote, "not evidence") {
		t.Fatalf("the note must still reach the conclusion an operator acts on: %q", previewNote)
	}
}

// A refusal that fired before -runid was read still has to leave a record a reader can open, and
// decodeRunRecord requires a non-empty run id. The sentinel only works if it can never be confused with a
// run id somebody actually passed, which is a property of runIDPattern rather than of the constant.
func TestRefusalRecordIsReadableEvenWithoutARunID(t *testing.T) {
	if runIDPattern.MatchString(unidentifiedRunID) {
		t.Fatalf("%q is an acceptable run id, so a real run could collide with the sentinel", unidentifiedRunID)
	}

	err := errors.New("-runid is required")
	rec := buildRecord(outcome{Disposition: dispRefusedBeforeCluster, Reason: err.Error()},
		nil, nil, nil, nil, nil, nil, recordIdentity{RunID: recordRunID(""), Arm: ""}, nil, false, time.Now(), time.Now())
	b, encErr := encodeRecord(rec)
	if encErr != nil {
		t.Fatalf("encode: %v", encErr)
	}
	got, decErr := decodeRunRecord(b)
	if decErr != nil {
		t.Fatalf("a refusal's record must decode, or the refusals this record exists to make visible are "+
			"written and then unreadable: %v", decErr)
	}
	if got.Disposition != string(dispRefusedBeforeCluster) {
		t.Fatalf("a refusal is refused-before-cluster, got %q", got.Disposition)
	}
	if got.RunID != unidentifiedRunID {
		t.Fatalf("an unidentified invocation must say so rather than carry an empty id, got %q", got.RunID)
	}
}

// -out names the record now that the bare ledger is gone, and an invocation that names no path must still
// leave one: a record written only when asked for would be absent exactly when an operator is trying to work
// out what a surprising refusal did.
//
// The default must also differ per invocation. It used to be one fixed name, which composed into a real
// defect: writeRecord replaces by rename, so a mistyped command in the same working directory destroyed the
// previous run's record — a refusal erasing evidence, inside the change written to stop refusals erasing
// evidence.
func TestRecordPathNamesEveryInvocationSeparately(t *testing.T) {
	if got := recordPathFor("/tmp/rec1.json", time.Now(), 1234); got != "/tmp/rec1.json" {
		t.Fatalf("-out must name the record, got %q", got)
	}

	at := time.Date(2026, 8, 8, 1, 10, 27, 0, time.UTC)
	first := recordPathFor("", at, 1234)
	if first == "" || !strings.HasSuffix(first, ".json") {
		t.Fatalf("an unnamed record path must still name a file, got %q", first)
	}

	// Two invocations in the same second are separated by the pid, and two at the same pid by the clock, so
	// no pair of live invocations can land on one path by construction.
	if sameSecond := recordPathFor("", at, 5678); sameSecond == first {
		t.Fatalf("two invocations in the same second collided on %q", first)
	}
	if samePID := recordPathFor("", at.Add(time.Second), 1234); samePID == first {
		t.Fatalf("two invocations at the same pid collided on %q", first)
	}
}

// A zero disposition is the silent lie this task exists to prevent, and no test can walk the seventeen
// returns that could produce one: fifteen need a live cluster. So the invariant is enforced where every
// record is built rather than audited where only some are reachable — a reviewer deleted four `o = ...`
// assignments and neither go vet nor the suite noticed.
func TestBuildRecordRefusesAZeroDisposition(t *testing.T) {
	rr, ok := buildRecord(outcome{}, nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now()).(runRecord)
	if !ok {
		t.Fatal("a non-preview invocation must build a runRecord")
	}
	if rr.Disposition != string(dispUnclassified) {
		t.Fatalf("an unset disposition must be named, not written as an empty string, got %q", rr.Disposition)
	}
	if !strings.Contains(rr.Reason, "bug in run()") {
		t.Fatalf("the record must say this is a bug rather than an outcome of the run, got %q", rr.Reason)
	}

	// The preview branch builds a different type, so it needs its own proof rather than inheriting this one.
	pr, ok := buildRecord(outcome{}, nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, true, time.Now(), time.Now()).(previewRecord)
	if !ok {
		t.Fatal("a preview invocation must build a previewRecord")
	}
	if pr.Disposition != string(dispUnclassified) {
		t.Fatalf("the preview branch must substitute too, got %q", pr.Disposition)
	}

	// The substitution must not touch an outcome that already has one, or it would rewrite real dispositions.
	kept := buildRecord(outcome{Disposition: dispChecksPassed, Reason: "x"}, nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false,
		time.Now(), time.Now()).(runRecord)
	if kept.Disposition != string(dispChecksPassed) || kept.Reason != "x" {
		t.Fatalf("a classified outcome must pass through untouched, got %q / %q", kept.Disposition, kept.Reason)
	}
}

// readsBackFine stands in for a read-back that found the record readable, for the tests below that are about
// the publish ordering rather than about the artifact. They inject a writer that touches no disk, so the real
// verifier would fail on a path they never created and every one of them would be measuring the filesystem.
func readsBackFine(string, bool) error { return nil }

// neverVerified fails if the read-back is attempted at all, which is the assertion the failed-write test
// needs: there is nothing at the path to read, so reading it would report a missing file as an unreadable
// record and bury the write failure that actually happened under a second, derived one.
func neverVerified(t *testing.T) recordVerifier {
	t.Helper()
	return func(path string, _ bool) error {
		t.Fatalf("the record must not be read back when the write failed, got a read of %q", path)
		return nil
	}
}

// The persist-before-publish ordering is the rule this whole task establishes, and it used to live inline
// in main(), which no test can call. If a persistence failure still rendered, an unrecorded result would
// reach the terminal and a non-zero exit could not retract it.
func TestReportRunPublishesNothingWhenTheRecordCannotBePersisted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	res := queuelab.LabResult{Arm: "A-honor"}
	code := reportRun(&stdout, &stderr, func(string, any) error { return errors.New("disk full") },
		neverVerified(t), runReport{
			Outcome: outcome{Disposition: dispChecksPassed},
			Events:  []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
			Result:  &res,
			Record:  runRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/nowhere/run.json",
		})

	if code == 0 {
		t.Fatal("a run whose record was not persisted must exit non-zero")
	}
	if stdout.Len() != 0 {
		t.Fatalf("nothing may be published when the record is not durable, got stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not persisted") {
		t.Fatalf("the persistence failure exists only on stderr, so it must be there: %q", stderr.String())
	}
}

// The mirror of the test above: a successful write must publish the RESULT, which is the one thing a
// non-zero exit cannot retract once it is on the terminal.
//
// It asserts on the rendered result rather than on a ledger line for a reason a reviewer proved empirically:
// deleting reportRun's rendering block left the suite green, because an events line is printed by printEvents
// and satisfies any assertion that only looks for one.
func TestReportRunPublishesOnceTheRecordIsDurable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	res := queuelab.LabResult{Arm: "A-honor"}
	wrote := ""
	code := reportRun(&stdout, &stderr, func(path string, _ any) error { wrote = path; return nil },
		readsBackFine, runReport{
			Outcome: outcome{Disposition: dispChecksPassed},
			Events:  []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
			Result:  &res,
			Record:  runRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/tmp/run.json",
		})

	if code != 0 {
		t.Fatalf("a run that passed its checks and persisted its record exits 0, got %d", code)
	}
	if wrote != "/tmp/run.json" {
		t.Fatalf("reportRun must persist to the path it was given, got %q", wrote)
	}
	if !strings.Contains(stdout.String(), queuelab.RenderResult(res)) {
		t.Fatalf("a durable record must be followed by the rendered result, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "job=a1") {
		t.Fatalf("a non-preview run publishes its ledger, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "/tmp/run.json") {
		t.Fatalf("the operator must be told where the record went, got %q", stderr.String())
	}
}

// The other half of the rendering rule: a durable record is necessary to publish a result, not sufficient.
//
// run() hands back a nil Result on every path that failed, so this scenario needs the caller to be wrong
// about that — a Result surviving alongside a failing disposition — and the check that must catch it is the
// disposition gate, not the nil check. Rendering here would put a number on the terminal for a run whose own
// collector desynced, which is exactly the "a run that looked fine was allowed to count" failure this lab
// published once already.
func TestReportRunRendersNoResultUnlessTheChecksPassed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	res := queuelab.LabResult{Arm: "A-honor"}
	code := reportRun(&stdout, &stderr, func(string, any) error { return nil }, readsBackFine, runReport{
		Outcome: outcome{Disposition: dispCollectorDesync, Reason: "watch gap"},
		Events:  []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
		Result:  &res,
		Record:  runRecord{SchemaVersion: recordSchemaVersion},
		Path:    "/tmp/run.json",
	})

	if code == 0 {
		t.Fatal("a run that did not pass its checks must exit non-zero")
	}
	if strings.Contains(stdout.String(), queuelab.RenderResult(res)) {
		t.Fatalf("a result must not be rendered for a disposition other than %s, got %q",
			dispChecksPassed, stdout.String())
	}
	// The ledger still publishes: withholding it would leave the failed run undiagnosable, which is the
	// invisibility this record exists to end — only the countable result is withheld.
	if !strings.Contains(stdout.String(), "job=a1") {
		t.Fatalf("a failed non-preview run still publishes its ledger, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), string(dispCollectorDesync)) {
		t.Fatalf("stderr must name the disposition that stopped publication, got %q", stderr.String())
	}
}

// reportRun applies the same substitution buildRecord does, so the terminal and the record cannot give two
// accounts of one run.
//
// Without it a zero disposition — a bug in run(), not an outcome of it — printed as "ERROR: :", the blank
// field dispUnclassified exists precisely to replace, while the record on disk named it correctly.
func TestReportRunNamesAnUnclassifiedOutcomeOnStderrToo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := reportRun(&stdout, &stderr, func(string, any) error { return nil }, readsBackFine, runReport{
		Outcome: outcome{},
		Record:  runRecord{SchemaVersion: recordSchemaVersion},
		Path:    "/tmp/run.json",
	})

	if code == 0 {
		t.Fatal("an unclassified outcome is not a passing run")
	}
	if !strings.Contains(stderr.String(), string(dispUnclassified)) {
		t.Fatalf("stderr must name the unclassified failure rather than print a blank field, got %q",
			stderr.String())
	}
	if !strings.Contains(stderr.String(), "bug in run()") {
		t.Fatalf("stderr must carry the same reason the record does, got %q", stderr.String())
	}
}

// previewRecord carries a count and no events so an invocation declared uncountable cannot emit anything
// reconstructable, and a printed ledger reconstructs exactly as well as a written one:
// `queuelabrun -preview ... > ledger.txt` would otherwise produce the artifact the record's whole shape
// exists to deny.
func TestReportRunWithholdsTheLedgerFromAPreview(t *testing.T) {
	var stdout, stderr bytes.Buffer
	events := []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Type: queuelab.EventPodReady, Job: "a1"}}
	reportRun(&stdout, &stderr, func(string, any) error { return nil }, readsBackFine, runReport{
		Outcome: outcome{Disposition: dispChecksPassed},
		Events:  events,
		Record:  previewRecord{SchemaVersion: recordSchemaVersion},
		Path:    "/tmp/run.json",
		Preview: true,
	})

	out := stdout.String()
	if strings.Contains(out, "job=a1") || strings.Contains(out, string(queuelab.EventPodReady)) {
		t.Fatalf("a preview must not print its ledger, got %q", out)
	}
	if !strings.Contains(out, "ledger: 1 events") {
		t.Fatalf("a preview still reports how many events it saw, got %q", out)
	}
	if !strings.Contains(out, previewBanner) {
		t.Fatalf("preview output must stay bracketed by the banner, got %q", out)
	}
}

// The other half of the same guarantee, and the half that was missing.
//
// buildRecord omits measurement from a preview because handing back the numbers directly returns exactly what
// the withheld ledger protects. The rendered result was not gated the same way, so a preview printed
// wastedGPUSeconds, the occupancy total and the wait percentile to stdout — where a shell redirect captures
// them as well as a file does. Withholding one channel while the other prints the same figures is not a
// guarantee, it is a formality.
//
// Mutation that turns this red: render the result for a preview as before.
func TestReportRunWithholdsTheNumbersFromAPreview(t *testing.T) {
	var stdout, stderr bytes.Buffer
	res := &queuelab.LabResult{TotalWastedGPUSeconds: 41.5}
	reportRun(&stdout, &stderr, func(string, any) error { return nil }, readsBackFine, runReport{
		Outcome: outcome{Disposition: dispChecksPassed},
		Result:  res,
		Record:  previewRecord{SchemaVersion: recordSchemaVersion},
		Path:    "/tmp/run.json",
		Preview: true,
	})

	out := stdout.String()
	if strings.Contains(out, "41.5") {
		t.Fatalf("a preview printed the very number its record withholds, got %q", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Fatalf("a preview that prints no numbers must say so, or the absence reads as a run that measured "+
			"nothing, got %q", out)
	}

	// The control: a real run must still print them, or the change has replaced a leak with a blackout.
	var runOut, runErr bytes.Buffer
	reportRun(&runOut, &runErr, func(string, any) error { return nil }, readsBackFine, runReport{
		Outcome: outcome{Disposition: dispChecksPassed},
		Result:  res,
		Record:  previewRecord{SchemaVersion: recordSchemaVersion},
		Path:    "/tmp/run.json",
	})
	if !strings.Contains(runOut.String(), "41.5") {
		t.Fatalf("a real run no longer reports its own measurement, got %q", runOut.String())
	}
}

// The record is the run's deliverable, so a record this build cannot read back is a failed deliverable and
// must change the exit code: exit 0 on an unreadable artifact is exactly how a number gets quoted out of a
// document nobody can re-derive it from.
//
// What it must NOT do is withhold the output. That is the asymmetry with a write failure, and the reason for
// it is that the two are different states: a failed write may mean nothing durable exists, while an unreadable
// record is durable bytes this build's reader refuses. Suppressing the ledger there would delete the last
// usable account of the run at the moment the file stopped being one.
//
// Mutation that turns this red: drop `|| verifyErr != nil` from reportRun's return condition, so an
// unreadable record is announced on stderr and still exits 0.
func TestReportRunFailsARunWhoseRecordCannotBeReadBack(t *testing.T) {
	var stdout, stderr bytes.Buffer
	res := queuelab.LabResult{Arm: "A-honor"}
	code := reportRun(&stdout, &stderr, func(string, any) error { return nil },
		func(string, bool) error { return errors.New("schema 7 is not 6") }, runReport{
			Outcome: outcome{Disposition: dispChecksPassed},
			Events:  []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
			Result:  &res,
			Record:  runRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/tmp/run.json",
		})

	if code == 0 {
		t.Fatal("a run whose record cannot be read back has not delivered a record, and must not exit 0")
	}
	if !strings.Contains(stderr.String(), "cannot be read back") {
		t.Fatalf("the read-back failure must name itself, or it reads as the run having failed: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "schema 7 is not 6") {
		t.Fatalf("the reader's own reason must reach the operator, got %q", stderr.String())
	}
	// The evidence must survive. Destroying it here is one failure this branch was written to avoid, and
	// leaving it on stdout is the other: a non-zero exit cannot retract a number a shell redirect already
	// captured, so an unreadable record must not leave anything countable on the channel a caller quotes.
	// Both halves are asserted together on purpose — either one alone is satisfied by a change that breaks
	// the other, and this function has already been wrong in each direction once.
	if !strings.Contains(stderr.String(), "job=a1") {
		t.Fatalf("an unreadable record is a reason to fail the run, not to destroy the only other account "+
			"of it, got %q", stderr.String())
	}
	for _, countable := range []string{"job=a1", "ledger:", "RESULT"} {
		if strings.Contains(stdout.String(), countable) {
			t.Fatalf("stdout carried %q after the record was refused; the exit code cannot retract it, got %q",
				countable, stdout.String())
		}
	}
}

// A banner closes on the channel it opened on, whatever happened to the content between.
//
// This is a regression the diversion introduced and a review caught. main prints the OPENING banner to stdout
// unconditionally; the closing one followed publish, so a refused read-back sent it to stderr and left stdout
// holding an opening banner with nothing under it — which is, word for word, the state the closing banner's
// own comment says it exists to prevent. The fix that protected one property broke the other.
//
// Mutation that turns this red: print the closing banner to publish again.
func TestReportRunClosesThePreviewBannerOnTheChannelItOpenedOn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := reportRun(&stdout, &stderr, func(string, any) error { return nil },
		func(string, bool) error { return errors.New("this preview decodes as a run record") }, runReport{
			Outcome: outcome{Disposition: dispChecksPassed},
			Events:  []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
			Record:  previewRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/tmp/run.json",
			Preview: true,
		})

	if code == 0 {
		t.Fatal("a preview whose record fails its own read-back must not exit 0")
	}
	if !strings.Contains(stdout.String(), previewBanner) {
		t.Fatalf("the ledger diverted and took the closing banner with it, so an operator reading stdout is "+
			"left under an opening banner alone: stdout=%q", stdout.String())
	}
	// The content still diverts — otherwise this test would be satisfied by undoing the diversion entirely.
	if strings.Contains(stdout.String(), "job=a1") {
		t.Fatalf("the refused record's ledger is back on stdout: %q", stdout.String())
	}
}

// A readable record puts the same output back on stdout, which is the half that stops the diversion from
// being a way to make every run quiet.
//
// Mutation that turns this red: set publish = stderr unconditionally in reportRun.
func TestReportRunPublishesToStdoutWhenTheRecordReadsBack(t *testing.T) {
	var stdout, stderr bytes.Buffer
	res := queuelab.LabResult{Arm: "A-honor"}
	code := reportRun(&stdout, &stderr, func(string, any) error { return nil },
		func(string, bool) error { return nil }, runReport{
			Outcome: outcome{Disposition: dispChecksPassed},
			Events:  []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
			Result:  &res,
			Record:  runRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/tmp/run.json",
		})

	if code != 0 {
		t.Fatalf("a passing run with a readable record must exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "job=a1") {
		t.Fatalf("a readable record's ledger belongs on stdout, got %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "job=a1") {
		t.Fatalf("nothing diverted the output, yet the ledger reached stderr too: %q", stderr.String())
	}
}

// A passing run and an unreadable record are two different facts, and an operator holding both needs both:
// one says what the run did, the other says the document cannot answer for it.
//
// Mutation that turns this red: return 1 from the disposition branch as it did before, so the read-back
// failure never prints for a run that also failed its checks.
func TestReportRunNamesBothTheFailedRunAndTheUnreadableRecord(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := reportRun(&stdout, &stderr, func(string, any) error { return nil },
		func(string, bool) error { return errors.New("verdict is unreadable") }, runReport{
			Outcome: outcome{Disposition: dispCollectorDesync, Reason: "watch gap"},
			Record:  runRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/tmp/run.json",
		})

	if code == 0 {
		t.Fatal("neither failure alone permits exit 0")
	}
	if !strings.Contains(stderr.String(), string(dispCollectorDesync)) {
		t.Fatalf("the run's own outcome must still be named, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "verdict is unreadable") {
		t.Fatalf("the record's failure must not be swallowed by the run's, got %q", stderr.String())
	}
}

// The verifier has two arms and they ask opposite questions — a run record must decode, a preview record must
// be refused — so handing it the wrong flag inverts the check into one that passes on precisely the documents
// it exists to catch.
//
// Mutation that turns this red: pass `false` instead of `r.Preview` at reportRun's verify call.
func TestReportRunTellsTheReadBackWhetherItWroteAPreview(t *testing.T) {
	var stdout, stderr bytes.Buffer
	got := false
	reportRun(&stdout, &stderr, func(string, any) error { return nil },
		func(_ string, preview bool) error { got = preview; return nil }, runReport{
			Outcome: outcome{Disposition: dispChecksPassed},
			Record:  previewRecord{SchemaVersion: recordSchemaVersion},
			Path:    "/tmp/run.json",
			Preview: true,
		})

	if !got {
		t.Fatal("the read-back was told this was a run record when a preview was written, which inverts " +
			"the check it performs")
	}
}

// fullScheme extends testScheme with the CRDs a full protocol run touches.
//
// Every test that reaches run()'s teardown now needs it, not just the ones that drive the whole protocol:
// teardown enumerates a ClusterQueue and a ResourceFlavor and reads both, and against a scheme without them
// every read fails, every target classifies absenceUnknown, and the run reports residue for a reason that
// has nothing to do with what it is testing. testScheme stays as it is for the tests that never reach a
// cluster at all.
func fullScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := testScheme(t)
	if err := platformv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform/v1 to scheme: %v", err)
	}
	if err := kueuev1beta2.AddToScheme(scheme); err != nil {
		t.Fatalf("add kueue v1beta2 to scheme: %v", err)
	}
	return scheme
}

// fakeSchedulerCreate stands in for the Kueue scheduler this fake cluster has none of. It stamps every
// MLTrainingJob straight to Running — the fact the barrier-staged schedule depends on — so an uncontested
// arm falls through to run()'s own explicit release exactly as it would against a real, empty cluster with
// nothing else competing for the worker.
//
// The UID it used to set by hand now comes from stampUIDOnCreate, which does it for every kind rather than
// for MLTrainingJob alone. The collector's UID-keyed cache always needed one; teardown needs one on the
// namespace and the fixtures too, and a fake cluster that assigned UIDs to exactly one kind would report
// residue for every run() test purely as an artefact of the double.
func fakeSchedulerCreate(ctx context.Context, c client.WithWatch, obj client.Object,
	opts ...client.CreateOption) error {
	if mltj, ok := obj.(*platformv1.MLTrainingJob); ok {
		mltj.Status.Phase = phaseRunning
		if err := stampUIDOnCreate(ctx, c, obj, opts...); err != nil {
			return err
		}
		// Admission is two things, and this double used to do one of them.
		//
		// A real Kueue marks the workload admitted AND charges its request against the ResourceFlavor, which
		// is what ClusterQueue.Status.FlavorsUsage reports. Modelling only the phase left the double able to
		// satisfy a barrier that reads phases and unable to satisfy one that reads Kueue's accounting — and
		// the barrier's implementation used to read phases. The double was not merely incomplete; it was the
		// shape the implementation had been fitted to.
		return chargeFakeFlavorUsage(ctx, c, mltj)
	}
	return stampUIDOnCreate(ctx, c, obj, opts...)
}

// chargeFakeFlavorUsage adds an admitted job's GPU request to every ClusterQueue's flavor usage, the way
// Kueue does when it admits.
//
// Every ClusterQueue in this fake cluster belongs to the one run under test, so charging all of them needs no
// cohort arithmetic — what matters is that the usage appears against the flavor the fixtures named.
func chargeFakeFlavorUsage(ctx context.Context, c client.WithWatch, mltj *platformv1.MLTrainingJob) error {
	var cqs kueuev1beta2.ClusterQueueList
	if err := c.List(ctx, &cqs); err != nil {
		return err
	}
	for i := range cqs.Items {
		cq := &cqs.Items[i]
		flavor := ""
		for _, rg := range cq.Spec.ResourceGroups {
			for _, f := range rg.Flavors {
				flavor = string(f.Name)
			}
		}
		if flavor == "" {
			continue
		}
		charged := false
		for fi := range cq.Status.FlavorsUsage {
			if string(cq.Status.FlavorsUsage[fi].Name) != flavor {
				continue
			}
			for ri := range cq.Status.FlavorsUsage[fi].Resources {
				r := &cq.Status.FlavorsUsage[fi].Resources[ri]
				if r.Name == gpuResourceName {
					r.Total.Add(*resource.NewQuantity(int64(mltj.Spec.GPUCount), resource.DecimalSI))
					charged = true
				}
			}
		}
		if !charged {
			cq.Status.FlavorsUsage = append(cq.Status.FlavorsUsage, kueuev1beta2.FlavorUsage{
				Name: kueuev1beta2.ResourceFlavorReference(flavor),
				Resources: []kueuev1beta2.ResourceUsage{{
					Name:  gpuResourceName,
					Total: *resource.NewQuantity(int64(mltj.Spec.GPUCount), resource.DecimalSI),
				}},
			})
		}
		// A plain Update rather than Status().Update: the fake tracker stores the whole object, and the status
		// subresource is only registered for the kinds these tests already needed it for. This is double code,
		// so writing the field directly is the shortest faithful way to say "Kueue charged it".
		if err := c.Update(ctx, cq); err != nil {
			return err
		}
	}
	return nil
}

// fakeSchedulerList stamps a resource version on every list, which a real apiserver always does and the fake
// client's in-memory tracker never does.
//
// Without it no full run can start at all: each of the collector's four streams takes its baseline from a
// List and refuses an empty resource version, because a watch resumed from one starts at "now" and the gap
// between the list and the watch is then invisible. That refusal is the correct behaviour against a real
// cluster and an artefact of the double against this one, so the double is what changes.
//
// The value is fixed rather than incremented because nothing here reads it back: the fake client's Watch
// ignores the resume version entirely, and what these tests exercise is run()'s ordering around the streams,
// not client-go's resumption.
func fakeSchedulerList(ctx context.Context, c client.WithWatch, list client.ObjectList,
	opts ...client.ListOption) error {
	if err := c.List(ctx, list, opts...); err != nil {
		return err
	}
	list.SetResourceVersion("1")
	return nil
}

// fakeNodeList stamps a resource version on Node lists and on nothing else.
//
// The ownership window takes its baseline from a List of Nodes and refuses an empty resource version, for the
// same reason the collector's four streams do: a watch resumed from one starts at "now", and the interval
// before it attached — the one in which the fixtures are applied and the first rows submitted — is invisible.
// That refusal is correct against a real apiserver and an artefact of the fake client's tracker, which stamps
// nothing, so the double is what changes.
//
// Nodes ALONE, deliberately. The tests that use this are about what run() does after an early return, and they
// reach those returns because the collector's namespaced streams refuse their own unstamped baseline. Stamping
// every list would send them down the full protocol instead and they would stop testing the path they name.
func fakeNodeList(ctx context.Context, c client.WithWatch, list client.ObjectList,
	opts ...client.ListOption) error {
	if err := c.List(ctx, list, opts...); err != nil {
		return err
	}
	if _, ok := list.(*corev1.NodeList); ok {
		list.SetResourceVersion("1")
	}
	return nil
}

// stubWatch never delivers an event; it exists only to close once ctx is cancelled, which the fake client's
// real Watch — a thin wrapper over an in-memory tracker with no notion of context at all — never does on its
// own. It predates the streams: collector.watch read the raw watch channel and could only rejoin its
// goroutines when that channel closed, so without this a full run() against a bare fake client hung in
// col.wait() forever. RetryWatcher no longer needs it — it selects on the context and gives up on the
// underlying watch — but the stub stays because it is also what keeps these tests deterministic.
//
// It costs nothing in fidelity here: every fact this file's full-run tests depend on — an MLTrainingJob's
// Submitted event and its Status.Phase for the barrier checks — is written directly rather than observed off
// a watch (see fakeSchedulerCreate and collector.submitObserved), and classify() has no case for
// MLTrainingJob in the first place, so a real relay of watch events would not change what either test sees.
type stubWatch struct{ ch chan watch.Event }

func newStubWatch(ctx context.Context) *stubWatch {
	w := &stubWatch{ch: make(chan watch.Event)}
	go func() {
		<-ctx.Done()
		close(w.ch)
	}()
	return w
}

func (w *stubWatch) Stop()                          {}
func (w *stubWatch) ResultChan() <-chan watch.Event { return w.ch }

// fakeSchedulerWatch pairs with fakeSchedulerCreate to complete the fake-cluster double: see stubWatch.
func fakeSchedulerWatch(ctx context.Context, c client.WithWatch, obj client.ObjectList,
	opts ...client.ListOption) (watch.Interface, error) {
	return newStubWatch(ctx), nil
}

// run()'s own release — at the very end, after reconstruction and cardinality have already passed, not the
// deferred emergency one that covers early returns — is the last thing standing between a computed result
// and calling the worker restored. If it fails, run() must reach classifyReleaseFailure, not the phase
// classifier every earlier failure in this function uses, because this release ran to completion rather than
// being cut short partway through: a failure here means the worker is still held, in full, with nothing left
// to retry, which is what dispWorkerNotRestored says and dispCancelled does not.
//
// Driving this requires run() to actually complete its protocol, so the Node patch this test fails is
// counted rather than the first one intercepted: the first Node patch is acquireWorker's own, and only the
// second is the run's own release.
//
// This test is real time, not simulated: it drives run() through the actual doseSec=40 protocol wait (see
// spine.go), which is a protocol constant the experiment's validity rests on and is deliberately not made
// configurable for tests — a test-only override would be exactly the seam that later gets used in
// production. -short skips it for that reason: `go test -short` does NOT check that an explicit release
// failure is recorded as worker-not-restored with nothing rendered. Only a full `go test` (no -short)
// exercises this acceptance condition.
func TestRunExplicitReleaseFailureRecordsWorkerNotRestored(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() through the full 40-second protocol dose to reach the explicit-release path; " +
			"this and the cancellation test below are the only two that drive run() past col.start, so -short " +
			"leaves submit, the barriers, the collector and the explicit release entirely unexercised — in " +
			"particular it does not check that an explicit release failure is recorded as worker-not-restored")
	}
	var nodePatches int
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					nodePatches++
					// A non-conflict error is not retried, which is what makes it land on the explicit
					// release's own failure path rather than acquisition's conflict-retry loop.
					if nodePatches == 2 {
						return fmt.Errorf("apiserver unreachable")
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	tdNow, tdSleep := fakeClock(time.Unix(0, 0))
	o, events, res, _, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r8", "queuelab-r8", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, tdNow, tdSleep)

	if nodePatches != 2 {
		t.Fatalf("want exactly 2 node patches (acquire + the run's own release), got %d — this test proved "+
			"nothing about the explicit release if the run never reached it", nodePatches)
	}
	if res != nil {
		t.Fatal("a run whose own release failed must hand back no result to render, or a caller could " +
			"publish a number computed under a worker that is still held")
	}
	if o.Disposition != dispWorkerNotRestored {
		t.Fatalf("an explicit release failure is worker-not-restored, got %s: %s", o.Disposition, o.Reason)
	}

	// The record is what actually survives, so the assertion is made against the bytes on disk rather than
	// the in-memory outcome the test already holds.
	path := t.TempDir() + "/record.json"
	started := time.Now()
	if err := writeRecord(path, buildRecord(o, events, nil, nil, nil, nil, nil, recordIdentity{RunID: "r8", Arm: string(queuelab.ArmNRef)}, nil, false,
		started, started.Add(45*time.Second))); err != nil {
		t.Fatalf("persist: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	rr, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("the persisted record must decode: %v", err)
	}
	if rr.Disposition != string(dispWorkerNotRestored) {
		t.Fatalf("the persisted record must carry worker-not-restored, got %s", rr.Disposition)
	}
}

// This is the acceptance condition the whole disposition design turns on. run()'s own release executes on
// cleanupContext — a bounded context deliberately derived from context.Background() rather than the
// signal-cancelled run context, exactly so a Ctrl-C landing while restoration is in flight cannot be
// mistaken for the reason restoration failed (see cleanupContext's and classifyReleaseFailure's comments).
//
// This drives that scenario for real: it cancels the run's own context from inside the release-phase Patch,
// the instant restoration is in flight, and checks two things. First, that the context the release actually
// ran on was still live the moment cancellation landed elsewhere — proving cleanupContext is genuinely
// independent rather than only documented as such, since a regression that handed the release the run's own
// context would show Canceled here immediately, before the injected failure below even matters. Second, that
// the disposition which follows names the release failure and never cancellation: the wrong implementation
// this guards against is routing the release failure through the PHASE classifier (classifyPhaseFailure)
// instead of classifyReleaseFailure, which is why the injected failure itself wraps context.Canceled — the
// phase classifier treats that as cancellation outright and would relabel this run cancelled, exactly the
// misreading two accounts of one run — a worker still held, reported as merely interrupted — that this
// design exists to prevent.
//
// This test is real time, not simulated: it drives run() through the actual doseSec=40 protocol wait (see
// spine.go), which is a protocol constant the experiment's validity rests on and is deliberately not made
// configurable for tests — a test-only override would be exactly the seam that later gets used in
// production. -short skips it for that reason: `go test -short` does NOT check the acceptance condition
// above — that a cancellation arriving while restoration is in flight never relabels the run cancelled. Only
// a full `go test` (no -short) exercises it.
func TestRunCancellationWhileRestoringNeverRelabelsAsCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() through the full 40-second protocol dose to reach the explicit-release path; " +
			"this and the release-failure test above are the only two that drive run() past col.start, so " +
			"-short leaves submit, the barriers, the collector and the explicit release entirely unexercised " +
			"— in particular it does not check that a cancellation during restoration never relabels the run " +
			"cancelled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var nodePatches int
	var releaseCtxErr error
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
			Patch: func(pctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					nodePatches++
					if nodePatches == 2 {
						cancel()
						releaseCtxErr = pctx.Err()
						return fmt.Errorf("release patch: %w", context.Canceled)
					}
				}
				return c.Patch(pctx, obj, patch, opts...)
			},
		}).Build()

	tdNow, tdSleep := fakeClock(time.Unix(0, 0))
	o, events, res, _, _, _, _, _ := run(ctx, func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r9", "queuelab-r9", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, tdNow, tdSleep)

	if nodePatches != 2 {
		t.Fatalf("want exactly 2 node patches (acquire + the run's own release), got %d — this test proved "+
			"nothing about restoration if the run never reached it", nodePatches)
	}
	if releaseCtxErr != nil {
		t.Fatalf("the release ran on a context the run's own cancellation could reach; want an independent "+
			"one derived from cleanupContext, got err %v on it", releaseCtxErr)
	}
	if res != nil {
		t.Fatal("a run whose own release failed must hand back no result to render")
	}
	if o.Disposition != dispWorkerNotRestored {
		t.Fatalf("a cancellation arriving while restoration is in flight must not relabel the run cancelled; "+
			"want worker-not-restored, got %s: %s", o.Disposition, o.Reason)
	}

	path := t.TempDir() + "/record.json"
	started := time.Now()
	if err := writeRecord(path, buildRecord(o, events, nil, nil, nil, nil, nil, recordIdentity{RunID: "r9", Arm: string(queuelab.ArmNRef)}, nil, false,
		started, started.Add(45*time.Second))); err != nil {
		t.Fatalf("persist: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	rr, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("the persisted record must decode: %v", err)
	}
	if rr.Disposition != string(dispWorkerNotRestored) {
		t.Fatalf("the persisted record must not carry cancelled, got %s", rr.Disposition)
	}
}

// runCall is one mutating call run() made against the cluster, in the order it made it.
//
// The property these tests are about — teardown happens before the worker's markers come off — is a
// property of the ORDER of calls, and no final state can show it: the node ends up released and the
// namespace ends up gone whichever order they happened in. This is the same reason Task 1's phase-order
// test had to assert on a call log rather than on what the cluster looked like afterwards.
type runCall struct {
	Op   string // "delete" | "patch"
	Kind string
	Name string
	// Dedicated says whether a patched Node still carried the dedication label at the moment it was written.
	// It exists because "the second Node patch" stopped being a synonym for "the release": a held worker now
	// gets a residue record patched onto it as well, and that patch — like the acquisition's — leaves the label
	// on, while only a release takes it off. Meaningless on any call that is not a Node patch.
	Dedicated bool
}

// recordRunCalls wraps a client so every delete and patch is captured in order.
//
// It wraps with interceptor.NewClient rather than adding to the builder, because WithInterceptorFuncs is a
// single slot whose last call wins and every test below needs its own Create behaviour as well as the log.
// The mutex is not decorative: run() has four watch goroutines live for most of its body, and a test that
// only happens not to trip -race today would be a poor thing to leave for the next reader.
func recordRunCalls(t *testing.T, inner client.WithWatch) (client.WithWatch, func() []runCall) {
	t.Helper()
	var (
		mu  sync.Mutex
		got []runCall
	)
	kindOf := func(obj client.Object) string {
		gvk, err := apiutil.GVKForObject(obj, inner.Scheme())
		if err != nil {
			t.Fatalf("no GroupVersionKind for %T: %v", obj, err)
		}
		return gvk.Kind
	}
	add := func(c runCall) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, c)
	}
	c := interceptor.NewClient(inner, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object,
			opts ...client.DeleteOption) error {
			add(runCall{Op: "delete", Kind: kindOf(obj), Name: obj.GetName()})
			return cl.Delete(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			call := runCall{Op: "patch", Kind: kindOf(obj), Name: obj.GetName()}
			_, call.Dedicated = obj.GetLabels()[workerLabelKey]
			add(call)
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	return c, func() []runCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]runCall(nil), got...)
	}
}

// stampUIDOnCreate gives every created object a UID, which a real apiserver does and the fake client does
// not.
//
// Without it these tests would exercise a cluster no teardown can ever meet: recoverTargets learns the UID
// from the object it reads, classifyAbsence returns absenceUnknown for an empty one, and the executor would
// therefore never issue a single Delete — every run() test would "prove" residue for the wrong reason.
func stampUIDOnCreate(ctx context.Context, c client.WithWatch, obj client.Object,
	opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("uid-" + obj.GetName()))
	}
	return c.Create(ctx, obj, opts...)
}

// firstIndexOf reports where a call appears in the log, or -1. The tests assert on indices rather than on
// mere presence, because presence is what both the right and the wrong order have in common.
func firstIndexOf(calls []runCall, op, kind string, nth int) int {
	seen := 0
	for i, c := range calls {
		if c.Op == op && c.Kind == kind {
			seen++
			if seen == nth {
				return i
			}
		}
	}
	return -1
}

// releasePatchIndex is where the run's own release of the worker appears, identified by the one thing a
// release does that no other Node patch does: take the dedication label off.
//
// It used to count — the SECOND Node patch, because the first was acquireWorker installing the markers and a
// test looking merely for "a Node patch" would be satisfied by the acquisition alone and could never fail.
// That arithmetic broke when a held worker started getting its residue record stamped on: the stamp is a
// second Node patch on precisely the path where the correct answer is "the worker was never released", so
// every held-worker test would have read the explanation of the hold as the release it is asserting did not
// happen.
func releasePatchIndex(calls []runCall) int {
	for i, c := range calls {
		if c.Op == "patch" && c.Kind == "Node" && !c.Dedicated {
			return i
		}
	}
	return -1
}

// The ordering this whole task exists to establish, on the path that returns early — which is every failure
// between acquiring the worker and reconstructing the result, and therefore the common case.
//
// If the worker's label and NoSchedule taint came off first, the next run would acquire a node whose GPUs
// this run's namespace is still holding, and it would look perfectly free while doing it. The assertion is
// on the index of the namespace delete against the index of the release patch, because both calls happen in
// either order and only their sequence distinguishes the two implementations.
func TestRunTearsDownBeforeTheEmergencyReleaseOnAnEarlyReturn(t *testing.T) {
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: fakeNodeList,
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				// The flavor is the first fixture applyFixtures creates, so failing it returns the run early
				// with the namespace already on the cluster: the exact state teardown exists for.
				if _, ok := obj.(*kueuev1beta2.ResourceFlavor); ok {
					return fmt.Errorf("resource flavor quota exhausted")
				}
				return stampUIDOnCreate(ctx, c, obj, opts...)
			},
		}).Build()
	c, calls := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, now, sleep)

	if res != nil {
		t.Fatal("a run that failed setup must hand back no result")
	}
	if o.Disposition != dispSetupFailed {
		t.Fatalf("a clean teardown must leave the original disposition alone, got %s: %s", o.Disposition, o.Reason)
	}
	if len(left) != 0 {
		t.Fatalf("teardown had one namespace to remove and it was removable; residue %+v", left)
	}

	got := calls()
	del := firstIndexOf(got, "delete", "Namespace", 1)
	rel := releasePatchIndex(got)
	if del < 0 {
		t.Fatalf("the namespace this run created was never deleted; calls: %+v", got)
	}
	if rel < 0 {
		t.Fatalf("the worker was never released; calls: %+v", got)
	}
	if del > rel {
		t.Fatalf("the worker was released at call %d, before the namespace was deleted at call %d: the next "+
			"run would acquire a node this run's namespace still holds; calls: %+v", rel, del, got)
	}
}

// The same ordering on the path that returns normally, which a defer cannot get right on its own: an inline
// release always runs before a defer registered later, so the happy path has to call teardown explicitly and
// in the right place. Nothing about the deferred path can show that.
//
// This test is real time, not simulated: it drives run() through the actual doseSec=40 protocol wait, the
// same as the two release tests above and for the same reason — the dose is a protocol constant the
// experiment's validity rests on and is deliberately not configurable. -short therefore does NOT check the
// happy-path half of this ordering.
func TestRunTearsDownBeforeItsOwnReleaseOnTheHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() through the full 40-second protocol dose to reach the happy path; -short leaves " +
			"the inline teardown-before-release ordering unexercised, and only the deferred path is checked")
	}
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
		}).Build()
	c, calls := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmNRef, "r8", "queuelab-r8", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, now, sleep)

	if o.Disposition != dispChecksPassed {
		t.Fatalf("an uncontested N-ref run against a clean cluster must pass, got %s: %s", o.Disposition, o.Reason)
	}
	if res == nil {
		t.Fatal("a passing run must hand back a result, or this test never reached the happy path at all")
	}
	if len(left) != 0 {
		t.Fatalf("teardown left residue on a cluster that deletes on request: %+v", left)
	}

	got := calls()
	del := firstIndexOf(got, "delete", "Namespace", 1)
	rel := releasePatchIndex(got)
	if del < 0 {
		t.Fatalf("the namespace this run created was never deleted; calls: %+v", got)
	}
	if rel < 0 {
		t.Fatalf("the run never released its worker; calls: %+v", got)
	}
	if del > rel {
		t.Fatalf("the happy path released the worker at call %d before deleting the namespace at call %d; a "+
			"defer registered after the release cannot fix this, the call has to be inline and earlier; "+
			"calls: %+v", rel, del, got)
	}
	// The fixtures are cluster-scoped and outlive the namespace, so a teardown that stopped at the namespace
	// would leave the next run under this id colliding with a ResourceFlavor built for a different arm.
	if firstIndexOf(got, "delete", "ResourceFlavor", 1) < 0 {
		t.Fatalf("the run's ResourceFlavor was never deleted; calls: %+v", got)
	}
}

// The rerun a used run id actually produces: a cluster-scoped ResourceFlavor from a previous attempt is still
// there (only the namespace is ever cleaned up by hand), so applyFixtures refuses and the run returns early
// with its own namespace already created.
//
// Everything this test asserts used to be the other way round. Recovery refused the whole batch on the first
// foreign target, so teardown issued no Delete at all, the run's own namespace stayed on the cluster, the
// record carried disposition residue-left with an EMPTY residue array — naming nothing, which a next-run gate
// keying on len(Residue) > 0 waves straight through — and the worker was held forever for a run whose only
// cluster write it could not undo was one it had not made.
//
// The worker going back is the deliberate half, and the disposition still reporting residue is the other:
// every object still standing here is one this run did not create, so holding contains nothing — the taint
// and label go back exactly as they were found, which is the state the previous attempt's leftovers were
// already sitting in — but the name is taken, the next run under this id collides with it just the same, and
// the record is what carries that.
func TestRunTearsDownAroundAStaleFixtureFromAPreviousAttempt(t *testing.T) {
	variant, err := queuelab.ArmAHonor.PolicyVariant()
	if err != nil {
		t.Fatalf("policy variant: %v", err)
	}
	// The flavor is built by the real builder so its name is whatever teardown will enumerate, and it carries
	// the variant this arm requires — so applyFixtures fails on the TRANSACTION check rather than on the
	// variant check, which is the case that reaches teardown with a foreign object at one of its own names.
	fs, err := queuelab.BuildFixtures(queuelab.StudyReclaim, variant, queuelab.FixtureIdentity{TxID: "tx-previous", RunID: "r7", Namespace: "queuelab-r7"})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	stale := fs.Flavor.DeepCopy()
	stale.SetUID("rf-uid-previous")
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil), stale).
		WithInterceptorFuncs(interceptor.Funcs{Create: stampUIDOnCreate, List: fakeNodeList}).Build()
	c, calls := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, now, sleep)

	if res != nil {
		t.Fatal("a run that failed setup must hand back no result")
	}
	if o.Disposition != dispResidueLeft {
		t.Fatalf("a name this run's teardown could not clear is %s, got %s: %s",
			dispResidueLeft, o.Disposition, o.Reason)
	}
	// The collision this run actually failed on has to survive the amendment, or the record says a name was
	// left behind without saying it is the same name that refused the run in the first place.
	if !strings.Contains(o.Reason, "applying fixtures") {
		t.Fatalf("the amended outcome dropped what run() originally decided as the cause, got %q", o.Reason)
	}
	if len(left) != 1 || left[0].Observation.Target.Name != stale.GetName() {
		t.Fatalf("residue is %+v; it must name the one object teardown refused to touch, so the record says "+
			"which name is taken rather than carrying an empty array", left)
	}
	if left[0].Absence != absenceForeign {
		t.Errorf("the stale flavor classified %v, want foreign", left[0].Absence)
	}

	got := calls()
	for _, call := range got {
		if call.Op == "delete" && call.Name == stale.GetName() {
			t.Fatalf("deleted %s %q, which a previous transaction created and may still be using",
				call.Kind, call.Name)
		}
	}
	del := firstIndexOf(got, "delete", "Namespace", 1)
	if del < 0 {
		t.Fatalf("this run's own namespace was never deleted; one foreign target must not strand the objects "+
			"the run really did create; calls: %+v", got)
	}
	rel := releasePatchIndex(got)
	if rel < 0 {
		t.Fatalf("the worker was never released though nothing this run created is still on the cluster; "+
			"calls: %+v", got)
	}
	if del > rel {
		t.Fatalf("released the worker at call %d before deleting the namespace at call %d; calls: %+v",
			rel, del, got)
	}
	// Asserted on the node, not only on the call log: "a patch happened" and "the markers are gone" are
	// different claims, and the next run acquires on the second one.
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &n); err != nil {
		t.Fatalf("read the worker back: %v", err)
	}
	if n.Labels[workerLabelKey] != "" || len(n.Spec.Taints) != 0 {
		t.Fatalf("the worker kept its dedication (labels %v, taints %v) for a run that created nothing still "+
			"standing; the next run finds a node that only looks busy", n.Labels, n.Spec.Taints)
	}
}

// residueHoldsWorker is the whole of that decision, so the mixed case is pinned here rather than through a
// second full run: one foreign object among objects this run really did leave behind must still hold the
// worker. Holding is decided by what is NOT foreign, not by whether anything foreign is present.
func TestResidueHoldsTheWorkerUnlessEverythingLeftIsSomebodyElses(t *testing.T) {
	foreign := residue{Observation: observation{Target: target{Kind: "ResourceFlavor", Name: "rf"}, Found: true,
		UID: "theirs", Foreign: true}, Absence: absenceForeign}
	ours := residue{Observation: observation{Target: target{Kind: "Namespace", Name: "ns"}, Found: true,
		UID: "u", WantUID: "u"}, Absence: absencePresent}
	unknown := residue{Observation: observation{Target: target{Kind: "Namespace", Name: "ns"},
		Err: errors.New("etcdserver: leader changed")}, Absence: absenceUnknown}

	for _, tc := range []struct {
		name string
		left []residue
		want bool
	}{
		{"nothing left", nil, false},
		{"only somebody else's names", []residue{foreign}, false},
		{"ours is still there", []residue{ours}, true},
		{"ours cannot be read", []residue{unknown}, true},
		// The one a "does any foreign object appear?" test cannot separate: our namespace may still be
		// running GPU Pods, and a foreign flavor alongside it says nothing about that.
		{"one of each", []residue{foreign, ours}, true},
		{"unreadable alongside a foreign one", []residue{foreign, unknown}, true},
	} {
		if got := residueHoldsWorker(tc.left); got != tc.want {
			t.Errorf("%s: residueHoldsWorker = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Residue is not a failure to compute a result — it is a fact about the cluster, and two things must follow
// from it: the outcome says so without losing what the run was already failing at, and the worker STAYS
// DEDICATED. Releasing it would strip the NoSchedule taint from a node whose namespace is still there, and
// an annotation cannot contain a GPU Pod; a teardown timeout deliberately sacrifices worker availability to
// preserve isolation.
//
// The namespace delete is refused rather than merely slow, because that is the shape that also proves the
// reason survives: a residue entry saying only "still present" reads as a slow finalizer.
func TestRunTeardownResidueAmendsTheOutcomeAndHoldsTheWorker(t *testing.T) {
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: fakeNodeList,
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				if _, ok := obj.(*kueuev1beta2.ResourceFlavor); ok {
					return fmt.Errorf("resource flavor quota exhausted")
				}
				return stampUIDOnCreate(ctx, c, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
						obj.GetName(), errors.New("teardown may not delete namespaces"))
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	c, calls := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, now, sleep)

	if res != nil {
		t.Fatal("a run that failed setup must hand back no result")
	}
	if o.Disposition != dispResidueLeft {
		t.Fatalf("a run whose teardown left objects behind is %s, got %s: %s",
			dispResidueLeft, o.Disposition, o.Reason)
	}
	// amend, not assignment: this run failed at setup AND then failed to clean up, and those are not the
	// same event. The record is the only account of it that survives the process.
	if !strings.Contains(o.Reason, "applying fixtures") {
		t.Fatalf("the amended outcome must keep what run() originally decided as the cause in its (after …) "+
			"chain, got %q", o.Reason)
	}
	if len(left) == 0 {
		t.Fatal("run() reported no residue for a namespace it was refused permission to delete; residue that " +
			"never leaves run() cannot reach the record")
	}
	found := false
	for _, r := range left {
		if r.Observation.Target.Name == "queuelab-r7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the residue must name the namespace that is still there, got %+v", left)
	}

	got := calls()
	if rel := releasePatchIndex(got); rel >= 0 {
		t.Fatalf("the worker was released at call %d despite residue; the next run would schedule onto a node "+
			"whose namespace is still there; calls: %+v", rel, got)
	}
	// Asserted on the node itself as well as on the call log, because "no second patch" and "the markers are
	// still installed" are different claims and the operator's recovery depends on the second one.
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &n); err != nil {
		t.Fatalf("read the worker back: %v", err)
	}
	if n.Labels[workerLabelKey] == "" {
		t.Fatal("the worker lost its dedication label while this run's namespace was still on the cluster")
	}
	if len(n.Spec.Taints) == 0 {
		t.Fatal("the worker lost its NoSchedule taint while this run's namespace was still on the cluster")
	}
}

// The same containment on the path that MEASURED SOMETHING, which is the case the whole teardown-before-
// release ordering exists for and the only one where the worker is holding live GPU Pods this run put there.
//
// Every other residue test in this package drives an early return: the fixture Create is refused, so run()
// never gets past setup and the inline branch on the happy path — the one that decides both that the worker
// stays and that no result is handed back — is reached by nothing. Deleting that branch outright and
// replacing it with `_ = holdWorker` left the entire package green, which is why this test exists rather than
// a stronger assertion somewhere cheaper.
//
// This test is real time, not simulated: it drives run() through the actual doseSec=40 protocol wait, the
// same as the other full-run tests here and for the same reason — the dose is a protocol constant the
// experiment's validity rests on and is deliberately not configurable. -short therefore does NOT check that a
// successful run holds its worker when teardown leaves residue.
func TestRunHoldsTheWorkerWhenTheHappyPathLeavesResidue(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() through the full 40-second protocol dose to reach the happy path; -short leaves " +
			"the happy-path worker hold and the no-result-on-residue rule unexercised")
	}
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
			// Refused rather than merely slow, so the residue also carries WHY: a record saying only "still
			// present" reads as a finalizer taking its time.
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
						obj.GetName(), errors.New("teardown may not delete namespaces"))
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	c, calls := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, events, res, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmNRef, "r8", "queuelab-r8", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, now, sleep)

	// The run must genuinely have completed its protocol, or this test is another early-return test wearing a
	// longer sleep. The owner row is submitted only after the victim has been Ready for the whole 40-second
	// dose, so an event for it is evidence the schedule ran to its last step; and the outcome carrying no
	// "(after …)" chain is evidence run() reached teardown with nothing already decided against it, which
	// only the path through reconstruction and the cardinality check does.
	submittedOwner := false
	for _, e := range events {
		if e.Job == queuelab.OwnerRow {
			submittedOwner = true
		}
	}
	if !submittedOwner {
		t.Fatalf("no event for the %q row; this run never reached the end of the schedule, so it cannot be "+
			"testing the happy path's teardown at all: %+v", queuelab.OwnerRow, events)
	}
	if strings.Contains(o.Reason, "(after ") {
		t.Fatalf("the outcome carries an earlier failure in its chain (%q); this run returned early and the "+
			"happy path's own branch is still untested", o.Reason)
	}

	if o.Disposition != dispResidueLeft {
		t.Fatalf("a run that measured cleanly and then could not remove its namespace is %s, got %s: %s",
			dispResidueLeft, o.Disposition, o.Reason)
	}
	// A sound measurement is still withheld, by the same rule the release-failure path follows: the number is
	// handed back only once the run is over, and this one is not over — its namespace is still on the cluster
	// and its worker is still dedicated to it.
	if res != nil {
		t.Fatal("a run holding its worker for residue handed back a result; nothing downstream distinguishes " +
			"that number from one produced by a run that finished")
	}
	found := false
	for _, r := range left {
		if r.Observation.Target.Name == "queuelab-r8" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the residue must name the namespace that is still there, got %+v", left)
	}

	got := calls()
	if firstIndexOf(got, "delete", "Namespace", 1) < 0 {
		t.Fatalf("teardown never even attempted the namespace delete; calls: %+v", got)
	}
	if rel := releasePatchIndex(got); rel >= 0 {
		t.Fatalf("the worker was released at call %d though this run's namespace — and the GPU Pods it holds "+
			"— are still there; calls: %+v", rel, got)
	}
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &n); err != nil {
		t.Fatalf("read the worker back: %v", err)
	}
	if n.Labels[workerLabelKey] == "" {
		t.Fatal("the worker lost its dedication label while this run's namespace was still on the cluster")
	}
	if len(n.Spec.Taints) == 0 {
		t.Fatal("the worker lost its NoSchedule taint while this run's namespace was still on the cluster; " +
			"an annotation means nothing to the scheduler and a GPU Pod would land on it")
	}
}

// A run that leaves residue and keeps the worker is exactly the case the next operator needs explained: the
// next acquisition refuses that node, and without this record the refusal has nothing to quote but a
// transaction id.
//
// The harness is TestRunTeardownResidueAmendsTheOutcomeAndHoldsTheWorker's — the fixture Create is refused so
// run() returns early, and the namespace Delete is forbidden so teardown has something of this run's own it
// cannot prove gone, which is what makes residueHoldsWorker hold.
func TestRunStampsTheResidueRecordWhenItHoldsTheWorker(t *testing.T) {
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: fakeNodeList,
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				if _, ok := obj.(*kueuev1beta2.ResourceFlavor); ok {
					return fmt.Errorf("resource flavor quota exhausted")
				}
				return stampUIDOnCreate(ctx, c, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
						obj.GetName(), errors.New("teardown may not delete namespaces"))
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	c, _ := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	// A named record path, because the path is the half of this record a test can pin exactly: main computes
	// it once and run() must carry that same name down, or the record invites the operator to open a file
	// nobody wrote.
	o, _, _, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "queuelabrun-record-r7.json", io.Discard, now, sleep)

	if o.Disposition != dispResidueLeft || len(left) == 0 {
		t.Fatalf("this harness must reach a residue that holds the worker, got %s: %s with %+v",
			o.Disposition, o.Reason, left)
	}
	raw, ok := residueOn(t, c) // node "platform-worker"
	if !ok {
		t.Fatal("the worker is held for residue and carries no record of why; the next run is refused with " +
			"nothing but a transaction id")
	}
	rec, err := decodeResidue(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rec.Left) == 0 || rec.RunID == "" {
		t.Fatalf("record is %+v, want it to name the run and what it left", rec)
	}
	// The record on the node and the record run() hands to the run record are two accounts of one teardown,
	// and a reader who has both must not be able to find them disagreeing.
	if len(rec.Left) != len(left) {
		t.Fatalf("the node says %d object(s) were left and the run record says %d", len(rec.Left), len(left))
	}
	if rec.RecordPath != "queuelabrun-record-r7.json" {
		t.Fatalf("record path is %q; run() must carry down the name main already gave the file, since "+
			"recomputing it names a file nobody wrote", rec.RecordPath)
	}
}

// A foreign-only residue releases the worker on purpose, so no refusal will ever quote a record. Writing one
// would leave a marker on a node whose release path has already run.
//
// The harness is TestRunTearsDownAroundAStaleFixtureFromAPreviousAttempt's: a cluster-scoped flavor from a
// previous attempt under the same run id, which applyFixtures refuses and teardown then classifies foreign.
//
// The attempt is watched at the patch rather than only in the final state, and that is not belt-and-braces:
// releaseAcquired deletes residueKey on its way past, so a stamp written a moment earlier would already be
// gone by the time this test read the node back, and an assertion on the node alone would pass over exactly
// the bug it is here for.
func TestRunDoesNotStampWhenTheWorkerIsReleased(t *testing.T) {
	variant, err := queuelab.ArmAHonor.PolicyVariant()
	if err != nil {
		t.Fatalf("policy variant: %v", err)
	}
	fs, err := queuelab.BuildFixtures(queuelab.StudyReclaim, variant, queuelab.FixtureIdentity{TxID: "tx-previous", RunID: "r7", Namespace: "queuelab-r7"})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	stale := fs.Flavor.DeepCopy()
	stale.SetUID("rf-uid-previous")

	var (
		mu      sync.Mutex
		stamped []string
	)
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil), stale).
		WithInterceptorFuncs(interceptor.Funcs{
			List:   fakeNodeList,
			Create: stampUIDOnCreate,
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				if raw, ok := obj.GetAnnotations()[residueKey]; ok {
					mu.Lock()
					stamped = append(stamped, raw)
					mu.Unlock()
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	c, _ := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, _, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "queuelabrun-record-r7.json", io.Discard, now, sleep)

	if o.Disposition != dispResidueLeft || len(left) == 0 {
		t.Fatalf("this harness must reach a residue, got %s: %s with %+v", o.Disposition, o.Reason, left)
	}
	for _, r := range left {
		if r.Absence != absenceForeign {
			t.Fatalf("this harness must reach a residue that is entirely somebody else's, got %+v", left)
		}
	}
	mu.Lock()
	got := append([]string(nil), stamped...)
	mu.Unlock()
	if len(got) > 0 {
		t.Fatalf("stamped %q onto a worker this run released; nothing will ever quote it", got)
	}
	if raw, ok := residueOn(t, c); ok {
		t.Fatalf("stamped %q onto a worker this run released; nothing will ever quote it", raw)
	}
}

// reportResidue used to write straight to os.Stderr, which made its text unassertable and is exactly how a
// false sentence in the released branch (see the next test) survived two reviews. This pins the held branch
// the same way, so a future change to either has to answer to a literal string instead of to nothing.
func TestReportResidueHeldMessage(t *testing.T) {
	var buf bytes.Buffer
	left := []residue{{Observation: observation{Target: target{Kind: "Namespace", Name: "queuelab-r7"}},
		Absence: absencePresent}}
	reportResidue(&buf, "platform-worker", left, true)
	got := buf.String()
	if !strings.Contains(got, "TEARDOWN INCOMPLETE: worker platform-worker stays dedicated; its GPUs may "+
		"still be in use") {
		t.Fatalf("held message changed or missing, got:\n%s", got)
	}
	if !strings.Contains(got, "Namespace queuelab-r7: present") {
		t.Fatalf("residue line missing or misformatted, got:\n%s", got)
	}
	if !strings.Contains(got, "do NOT strip a stuck namespace's finalizer") {
		t.Fatalf("finalizer warning missing from the held branch, got:\n%s", got)
	}
	if !strings.Contains(got, "-inspect-worker -worker platform-worker") {
		t.Fatalf("recovery command missing from the held branch, got:\n%s", got)
	}
}

// This is the finding itself: since 62e74c4, a namespace THIS RUN created and deleted can still be observed
// Terminating and classified absenceForeign — so the released branch's old text ("nothing this run created
// is still on the cluster") is false in exactly the window an operator is most likely to read it. This test
// drives that scenario — a residue entry that is foreign, which is the only way reportResidue is ever
// called with held=false (residueHoldsWorker's own contract) — and pins that the message no longer asserts
// the negative it cannot know, while still giving the released-branch advice.
//
// Mutation this catches: reverting the released-branch Fprintf back to "nothing this run created is still
// on the cluster, but these names are held by another transaction" makes the first assertion below fail,
// because that old text is exactly what it checks is absent.
func TestReportResidueReleasedMessageAssertsNoNegativeItCannotKnow(t *testing.T) {
	var buf bytes.Buffer
	// A namespace this run itself created: WantUID set, but observed under a different UID because a
	// different object took the name while this run's own was going Terminating — classifyAbsence's
	// absenceForeign case, reached from residue this run is directly implicated in.
	left := []residue{{
		Observation: observation{
			Target: target{Kind: "Namespace", Name: "queuelab-r7"}, Found: true, Terminating: true,
			UID: "someone-elses-uid", WantUID: "our-uid",
		},
		Absence: absenceForeign,
	}}
	reportResidue(&buf, "platform-worker", left, false)
	got := buf.String()
	if strings.Contains(got, "nothing this run created is still on the cluster") {
		t.Fatalf("the released branch still asserts a negative it cannot know: since a namespace this run "+
			"created can itself be observed Terminating and classified foreign, this claim can be false on "+
			"the exact residue driving this test, got:\n%s", got)
	}
	if !strings.Contains(got, "TEARDOWN INCOMPLETE: worker platform-worker was released; nothing left at "+
		"these names carries this run's stamp") {
		t.Fatalf("released message changed unexpectedly, got:\n%s", got)
	}
	// Not "somebody else's stamp": absenceForeign also covers an object carrying no stamp at all, so naming a
	// foreign owner would presuppose a stamp that may not exist — the same unprovable shape this branch was
	// rewritten to stop asserting, one size smaller.
	if strings.Contains(got, "does not own") {
		t.Fatalf("the released branch presupposes a foreign stamp on names that may carry none, got:\n%s", got)
	}
	if !strings.Contains(got, "Namespace queuelab-r7: foreign") {
		t.Fatalf("residue line missing or misformatted, got:\n%s", got)
	}
	if !strings.Contains(got, "rerun under a run id of its own") {
		t.Fatalf("released-branch advice missing, got:\n%s", got)
	}
	if strings.Contains(got, "do NOT strip a stuck namespace's finalizer") {
		t.Fatalf("the held-branch finalizer warning must not appear on the released branch, got:\n%s", got)
	}
}

// The stamp is written on a path that is already reporting failure, and the fact that matters — the worker is
// held — is carried by the label and the taint, which are already installed. Failing the run on it would
// misreport that: it would turn a run that did exactly what it decided into a run that decided something
// else, and no next-run gate keys on this annotation.
func TestAFailedResidueStampChangesNoOutcome(t *testing.T) {
	inner := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: fakeNodeList,
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				if _, ok := obj.(*kueuev1beta2.ResourceFlavor); ok {
					return fmt.Errorf("resource flavor quota exhausted")
				}
				return stampUIDOnCreate(ctx, c, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
						obj.GetName(), errors.New("teardown may not delete namespaces"))
				}
				return c.Delete(ctx, obj, opts...)
			},
			// Only the write that sets residueKey is rejected. Acquisition patches the same Node and must still
			// succeed, or this test would be about a run that never got a worker at all.
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				if _, ok := obj.GetAnnotations()[residueKey]; ok {
					return fmt.Errorf("apiserver unreachable")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	c, calls := recordRunCalls(t, inner)
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return c, nil },
		queuelab.ArmAHonor, "r7", "queuelab-r7", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "queuelabrun-record-r7.json", io.Discard, now, sleep)

	if o.Disposition != dispResidueLeft {
		t.Fatalf("disposition is %q, want %q: a failed annotation must not change what the run decided",
			o.Disposition, dispResidueLeft)
	}
	if res != nil {
		t.Fatal("a run holding its worker for residue handed back a result")
	}
	if len(left) == 0 {
		t.Fatal("the residue must still reach the run record; the annotation is the copy, not the original")
	}
	if raw, ok := residueOn(t, c); ok {
		t.Fatalf("the stamp was rejected and yet the node carries %q; this test proved nothing", raw)
	}
	// The worker must still be held: label and taint are what contain, and they were never the thing that
	// failed. Asserted on the node itself and on the call log, the way the existing residue tests do.
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &n); err != nil {
		t.Fatalf("read the worker back: %v", err)
	}
	if n.Labels[workerLabelKey] == "" {
		t.Fatal("the worker lost its dedication label because an explanatory annotation could not be written")
	}
	if len(n.Spec.Taints) == 0 {
		t.Fatal("the worker lost its NoSchedule taint because an explanatory annotation could not be written")
	}
	if rel := releasePatchIndex(calls()); rel >= 0 {
		t.Fatalf("the worker was released at call %d after the stamp failed; a write that explains a hold must "+
			"not be able to end one", rel)
	}
}

// Submission is the point of no return: work offered to the cluster runs, competes for the worker's GPUs and
// finishes whether or not anything was watching, and no later read can reconstruct the transitions it made in
// between. So run() must prove its four streams open BEFORE it submits, and refuse the run when it cannot.
//
// This is the run-level half of the barrier that awaitEstablished implements. Its collector-level tests can
// only show the wait returning an error; this one shows what the run does with it, which is the property that
// actually matters — the previous implementation called col.start and fell straight into the submit loop, so
// a watch that never established held up precisely nothing.
//
// The Pod watch is refused with a WRAPPED Forbidden because that is the case with no other signal in it:
// RetryWatcher terminates the stream having forwarded no event and no status, and startWatchStream has
// already returned a nil error by then, so nothing but the barrier's own End() arm can notice.
//
// Mutation that turns this red: delete the col.awaitEstablished block from run(). The run then submits its
// whole trace under a Pod stream that does not exist, spends the full horizon, and this test counts the
// submissions it was not supposed to make.
func TestRunSubmitsNothingWhenAStreamCannotBeEstablished(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "",
		errors.New("no watch permission"))
	var (
		mu        sync.Mutex
		submitted int
	)
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				if _, ok := obj.(*platformv1.MLTrainingJob); ok {
					// Counted under a mutex because run() has live consumer goroutines for the three streams that
					// did establish, and a counter this test's verdict rests on must not be the one racy thing in it.
					mu.Lock()
					submitted++
					mu.Unlock()
				}
				return fakeSchedulerCreate(ctx, c, obj, opts...)
			},
			List: fakeSchedulerList,
			Watch: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption) (watch.Interface, error) {
				if isPodList(list) {
					return nil, fmt.Errorf("watch pods: %w", forbidden)
				}
				return newStubWatch(ctx), nil
			},
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, _, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r10", "queuelab-r10", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, now, sleep)

	mu.Lock()
	n := submitted
	mu.Unlock()
	if n != 0 {
		t.Fatalf("the run submitted %d job(s) with no Pod stream open; that work runs and finishes entirely "+
			"unobserved, and the readiness and termination transitions it makes are gone", n)
	}
	if res != nil {
		t.Fatal("a run that never established its observation must hand back no result")
	}
	if o.Disposition != dispSetupFailed {
		t.Fatalf("a run refused before it could observe is %s, got %s: %s", dispSetupFailed, o.Disposition, o.Reason)
	}
	// Naming the kind is what tells the operator whether they are looking at an RBAC gap on Pods or at a
	// cluster that is failing every watch; all four streams share one namespace and would otherwise read alike.
	if !strings.Contains(o.Reason, kindPod) {
		t.Fatalf("the refusal %q does not name the view of the run that could not be opened", o.Reason)
	}
}

// run() must hand the streams its own cancellable context and bound establishment separately, and no
// collector-level test can see that: what is being checked is how run() BUILT the context it passed, which
// only a test driving run() itself can observe.
//
// The mutation this exists for is the natural wrong implementation — col.start(context.WithTimeout(cctx,
// establishBudget)) — and it is the one that must not be able to ship green. It is silent by construction:
// the streams die a budget into the window, every ending reads Cancelled because the caller's own context
// expired, no consumer desyncs, and the run reports checks-passed over a window it observed almost none of.
// Every other test in this package stays green under it, which is precisely why this one asserts on the
// context rather than on any consequence of it.
//
// The Pod watch is refused permanently so the run returns in milliseconds. What this test reads — the
// contexts the four streams were opened with — all exists before that refusal is noticed, so nothing about
// the assertion depends on how the run ends.
func TestRunHandsTheStreamsAnUnboundedContext(t *testing.T) {
	spy := &deadlineSpy{}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "",
		errors.New("no watch permission"))
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			List:   fakeSchedulerList,
			Watch: spy.watch(func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption) (watch.Interface, error) {
				if isPodList(list) {
					return nil, fmt.Errorf("watch pods: %w", forbidden)
				}
				return newStubWatch(ctx), nil
			}),
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r11", "queuelab-r11", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, now, sleep)

	calls, bounded := spy.observed()
	if calls < 4 {
		t.Fatalf("the run opened %d watch(es); it never got as far as the four stream contexts this test "+
			"inspects, so a green result here would mean nothing", calls)
	}
	if len(bounded) > 0 {
		t.Fatalf("run() handed its streams bounded contexts (%v): the establishment budget has become the "+
			"observation's deadline, and a run that stops observing when it expires reports an orderly shutdown "+
			"instead of a lost stream", bounded)
	}
}

// The gate this whole file's ownership machinery could not close: the label and the NoSchedule taint reserve
// the worker against FUTURE placement and evict nothing, so a GPU Pod that was already running keeps its
// device for the entire run. run() went from acquireWorker straight to its first Create, and the run then
// measured a machine with half its devices already spoken for and said nothing about it.
//
// This asserts three separate things about the refusal, and each is a different way for the check to be
// present and useless: it has to actually refuse, it has to refuse BEFORE the first Create (so nothing of
// this run's is left on a cluster it never should have touched), and it has to give the worker back (so the
// next attempt is not blocked by a run that never started).
//
// Mutation that turns this red: delete the qualifyWorker block from run(). That is the mutation that
// matters most here — the component would still exist, still be correct and still be tested in isolation,
// which is precisely the defect the previous gate shipped and had to be caught live.
func TestRunRefusesToCreateAnythingOnAContaminatedWorker(t *testing.T) {
	var (
		mu      sync.Mutex
		created []string
	)
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).
		WithObjects(node(nil, nil), gpuPod("tenant-a", "train-7", "platform-worker", 1)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				mu.Lock()
				created = append(created, fmt.Sprintf("%T %s", obj, obj.GetName()))
				mu.Unlock()
				return fakeSchedulerCreate(ctx, c, obj, opts...)
			},
			List:  fakeSchedulerList,
			Watch: fakeSchedulerWatch,
		}).Build()
	tdNow, tdSleep := fakeClock(time.Unix(0, 0))

	o, _, res, _, qual, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r12", "queuelab-r12", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, tdNow, tdSleep)

	if o.Disposition != dispEnvironmentUnqualified {
		t.Fatalf("a run on a worker already holding somebody else's GPU Pod is %s, got %s: %s",
			dispEnvironmentUnqualified, o.Disposition, o.Reason)
	}
	if res != nil {
		t.Fatal("a run that refused its environment must hand back no result")
	}
	mu.Lock()
	madeObjects := append([]string(nil), created...)
	mu.Unlock()
	if len(madeObjects) != 0 {
		t.Fatalf("the run created %v before deciding the worker was unusable; a refusal that leaves objects "+
			"behind is a refusal the next run has to clean up after", madeObjects)
	}
	if qual == nil || len(qual.GPUConsumers) != 1 || qual.GPUConsumers[0].Name != "train-7" {
		t.Fatalf("the refusal must carry what it saw into the record, got %+v", qual)
	}

	// The worker has to go back. A refusal that holds the node is indistinguishable from a crash to the next
	// operator, and there is nothing of this run's on it to contain.
	var n corev1.Node
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "platform-worker"}, &n); err != nil {
		t.Fatalf("read the node back: %v", err)
	}
	if _, held := n.Labels[workerLabelKey]; held {
		t.Fatal("the worker is still labelled after a refusal that created nothing on it")
	}
	if _, journalled := n.Annotations[journalKey]; journalled {
		t.Fatal("the ownership journal survived a refusal, so the next run refuses foreign-owner on a node " +
			"nothing is using")
	}
	for _, tt := range n.Spec.Taints {
		if tt.Key == workerTaintKey {
			t.Fatal("the NoSchedule taint survived a refusal, so the node stays reserved for a run that " +
				"never started")
		}
	}
	t.Logf("refusal reason:\n%s", o.Reason)
}

// The requirement is derived from the fixtures at the call site, not just inside requiredGPU, and this is
// what pins that wiring: the node advertises one device and the reclaim arm's two ClusterQueues need two, so
// a run() that passed a smaller (or hard-coded, or zero) requirement through would let it past.
//
// Mutation that turns this red: pass a literal 1 — or `int64(len(fs.ClusterQueue))` on the FIFO shape, or 0 —
// to qualifyWorker in run() instead of requiredGPU(fs)'s result.
func TestRunSizesTheWorkerAgainstItsOwnFixtures(t *testing.T) {
	small := node(nil, nil)
	small.Status.Allocatable[gpuResourceName] = *resource.NewQuantity(1, resource.DecimalSI)
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(small).
		WithInterceptorFuncs(interceptor.Funcs{Create: fakeSchedulerCreate, List: fakeSchedulerList,
			Watch: fakeSchedulerWatch}).Build()
	tdNow, tdSleep := fakeClock(time.Unix(0, 0))

	o, _, _, _, qual, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r13", "queuelab-r13", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, tdNow, tdSleep)

	if o.Disposition != dispEnvironmentUnqualified {
		t.Fatalf("a one-device node cannot produce the borrow-then-reclaim contrast the arm is named after, "+
			"so the run must refuse rather than complete; got %s: %s", o.Disposition, o.Reason)
	}
	if qual == nil || qual.RequiredGPU != 2 {
		t.Fatalf("the requirement must be the fixtures' own total, got %+v", qual)
	}
	if qual.RequiredFrom == "" || !strings.Contains(qual.RequiredFrom, "ClusterQueue") {
		t.Fatalf("the record must say where the requirement came from, got %+v", qual)
	}
}

// The qualification a PASSING run records, which is the case the later validity-bearing-artifact gate is
// actually built on and the one both refusal tests above leave uncovered.
//
// The gap that motivated it is narrow and real: every other run() test discards the fifth return, so a
// regression that recorded the observation only on the refusal path — a qualification block moved inside an
// `if err != nil`, or made conditional on there being Pods to look at — would leave the entire package green
// while every admissible run in the archive carried no evidence of the machine it ran on.
//
// It drives the same 45-second N-ref shape as the teardown-ordering test above rather than the full protocol
// dose, and asserts on the bytes of the record rather than on the returned struct, because the artifact is
// what survives the process.
//
// Mutation that turns this red: move the qualifyWorker call inside the refusal branch, or drop the
// `Qualification: qual` assignment from buildRecord's non-preview branch.
func TestAQualifiedRunRecordsWhatItsWorkerWas(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() to a passing disposition, which takes the 45-second observation window")
	}
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	o, events, res, left, qual, _, obs, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r14", "queuelab-r14", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, now, sleep)

	if o.Disposition != dispChecksPassed {
		t.Fatalf("this test is only meaningful on a run that passed, got %s: %s", o.Disposition, o.Reason)
	}
	if res == nil {
		t.Fatal("a passing run must hand back a result, or this never reached the qualified happy path")
	}
	if qual == nil {
		t.Fatal("a run that passed every check recorded nothing about the machine it measured on")
	}

	rec := buildRecord(o, events, left, qual, nil, obs, nil, recordIdentity{RunID: "r14", Arm: string(queuelab.ArmNRef)}, nil, false,
		time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a passing run's record must decode: %v\n%s", err, b)
	}
	q := got.Qualification
	if q == nil {
		t.Fatalf("the passing run's record carries no qualification, so the artifact says a number was "+
			"measured and nothing about what it was measured on:\n%s", b)
	}
	if q.Node != "platform-worker" || q.NodeUID != "uid-node" {
		t.Fatalf("the record must identify the machine, got %+v", q)
	}
	if q.AllocatableGPU != 2 || q.RequiredGPU != 2 || q.RequiredBoundBy != boundByQuotaSum {
		t.Fatalf("the record must carry the capacity claim and which bound decided it, got %+v", q)
	}
	if !strings.Contains(q.RequiredFrom, "ClusterQueue") {
		t.Fatalf("the record must say where the requirement came from, got %q", q.RequiredFrom)
	}
	if !q.Ready || !q.Schedulable || len(q.GPUConsumers) != 0 {
		t.Fatalf("a passing run's worker was Ready, schedulable and uncontended, got %+v", q)
	}
	t.Logf("qualification persisted by a passing run: %+v", *q)
}

// The window a PASSING run records, which is what the later validity-bearing-artifact gate is built on and
// what no unit test of the sentinel can establish: every one of those drives the component directly, so a
// sentinel that was never started from run(), or a window never carried into the record, would leave the
// whole package green while every admissible run in the archive claimed exclusivity it had not watched for.
// That is the exact failure this lineage has already shipped once — a correct component, reviewed, with no
// caller.
//
// Mutation that turns this red: delete the startOwnershipSentinel call and its defer from run(), or drop the
// `Window: win` assignment from buildRecord's non-preview branch.
func TestAPassingRunRecordsTheWindowItHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() to a passing disposition, which takes the 45-second observation window")
	}
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	o, events, res, left, qual, win, obs, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r15", "queuelab-r15", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, now, sleep)

	if o.Disposition != dispChecksPassed {
		t.Fatalf("this test is only meaningful on a run that passed, got %s: %s", o.Disposition, o.Reason)
	}
	if res == nil {
		t.Fatal("a passing run must hand back a result, or this never reached the happy path")
	}
	if win == nil {
		t.Fatal("a run that published a number recorded nothing about whether its worker stayed its own")
	}
	if win.ViolationsObserved != 0 {
		t.Fatalf("an uncontested worker produced violations: %+v", win.Violations)
	}
	if win.NodeVersionsObserved < 1 {
		t.Fatal("a window that compared no node version at all is not evidence of anything; the count is the " +
			"denominator for its empty violation list")
	}
	// The restoration audit only exists once the release has run, and the release runs after run() has chosen
	// what to return — so this is also the assertion that the audit reaches the record at all.
	if win.Restoration == nil || !win.Restoration.OurMarkersRemoved {
		t.Fatalf("the passing run recorded no audited restoration: %+v", win.Restoration)
	}

	rec := buildRecord(o, events, left, qual, win, obs, nil, recordIdentity{RunID: "r15", Arm: string(queuelab.ArmNRef)}, nil, false,
		time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a passing run's record must decode: %v\n%s", err, b)
	}
	if got.Window == nil || got.Window.Node != "platform-worker" || got.Window.BaselineResourceVersion == "" {
		t.Fatalf("the artifact does not carry a window naming the machine and the point it opened from:\n%s", b)
	}
	t.Logf("window persisted by a passing run: %+v", *got.Window)
}

// The gate itself, end to end and through the real run(): the worker's NoSchedule taint is stripped while the
// measurement is in flight and put back before the release, so acquire's verify and release's decideRelease
// both see a correct tuple and both pass — which is precisely the run that used to publish a number measured
// on a shared machine.
//
// The Node's stored state is never actually changed, only what the view observes. That is deliberate: it
// isolates what this test is about (the continuous view is the only thing that can see this) and leaves the
// release path exercising the same clean node every other run() test does.
//
// Mutation that turns this red: delete the `col.desync(reason)` fold in run(). The window still records the
// violation, the record still carries it, and the run prints its number anyway — the split between having
// evidence and enforcing it, which is the whole failure mode this gate is against.
func TestRunRefusesToPublishAWorkerThatWasSharedMidRun(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() through its 45-second observation window to reach the invalidation")
	}
	// The label value and the taint value are the RUN id (see decideAcquire), so these versions can be built
	// without knowing the transaction id this run will generate.
	stripped := node(map[string]string{workerLabelKey: "r16"}, nil)
	stripped.ResourceVersion = "9001"
	restored := node(map[string]string{workerLabelKey: "r16"}, nil,
		corev1.Taint{Key: workerTaintKey, Value: "r16", Effect: corev1.TaintEffectNoSchedule})
	restored.ResourceVersion = "9002"

	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			List:   fakeSchedulerList,
			Watch: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption) (watch.Interface, error) {
				if _, isNodes := list.(*corev1.NodeList); !isNodes {
					return fakeSchedulerWatch(ctx, c, list, opts...)
				}
				w := watch.NewFakeWithChanSize(2, false)
				w.Modify(stripped)
				w.Modify(restored)
				return w, nil
			},
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	o, _, res, _, _, win, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmNRef, "r16", "queuelab-r16", "platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, now, sleep)

	if res != nil {
		t.Fatal("a run whose worker was shared for part of its window published a result")
	}
	if o.Disposition != dispCollectorDesync {
		t.Fatalf("disposition is %q (%s), want %q: the window's verdict has to reach the ledger, because "+
			"builder.Err() is the one thing that decides whether a number may exist",
			o.Disposition, o.Reason, dispCollectorDesync)
	}
	if win == nil || win.ViolationsObserved == 0 {
		t.Fatalf("the run was invalidated but its record says nothing about why: %+v", win)
	}
	if win.Violations[0].Reason != reasonInstalledDiverged {
		t.Fatalf("the violation was classified %q, want %q", win.Violations[0].Reason, reasonInstalledDiverged)
	}
	// The worker still goes back: this run lost its claim to the number, not its obligation to leave the
	// cluster as it found it.
	if win.Restoration == nil || !win.Restoration.OurMarkersRemoved {
		t.Fatalf("an invalidated run must still restore and audit its worker: %+v", win.Restoration)
	}
}

// The window opens before qualification, not after it, and an unqualified run is where that ordering is
// visible: the run is refused by the machine it was given, and the record still has to say what that machine
// was doing while it was being inspected.
//
// The ordering is not cosmetic. qualifyWorker does a Node Get and an unfiltered cluster-wide Pod List — the
// most expensive call in setup — and every millisecond of it used to sit inside the one interval this gate
// does not watch, between acquire's own verify and the window's baseline. Nothing in the sentinel depends on
// qualification, so the cost bought nothing.
//
// Mutation that turns this red: move the startOwnershipSentinel call and its defer back below qualifyWorker.
// This run then refuses before the window is ever opened, the record carries no window for it, and the
// unwatched sliver grows by however long a cluster-wide Pod List takes.
func TestAnUnqualifiedRunStillRecordsTheWindowItOpened(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).
		WithObjects(node(nil, nil), gpuPod("tenant-a", "train-7", "platform-worker", 1)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			List:   fakeSchedulerList,
			Watch:  fakeSchedulerWatch,
		}).Build()
	tdNow, tdSleep := fakeClock(time.Unix(0, 0))

	o, _, _, _, qual, win, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r17", "queuelab-r17", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, tdNow, tdSleep)

	if o.Disposition != dispEnvironmentUnqualified || qual == nil {
		t.Fatalf("this test is only meaningful on a run refused at qualification, got %s: %s", o.Disposition, o.Reason)
	}
	if win == nil {
		t.Fatal("a run refused at qualification carries no window, so the window was opened after the " +
			"qualification reads and those reads happened inside the interval nothing was watching")
	}
	if win.NodeVersionsObserved < 1 || win.BaselineResourceVersion == "" {
		t.Fatalf("the window exists but established nothing: %+v", win)
	}
	// The worker went back on this path, and the audit is the record of it — the deferred emergency release is
	// what runs here, so this also pins that the audit reaches the window from that release and not only from
	// the inline one.
	if win.Restoration == nil || !win.Restoration.OurMarkersRemoved {
		t.Fatalf("the emergency release on the refusal path recorded no audited restoration: %+v", win.Restoration)
	}
}

// A run refused at establishment is the truncated observation this gate exists to make legible, and it is the
// record no reader could previously tell apart from a complete one: same disposition, same free-text reason,
// and nothing anywhere saying which of four views had died or that none of them had been proven open.
//
// The harness is TestRunSubmitsNothingWhenAStreamCannotBeEstablished's, and the wrapped Forbidden is chosen
// for the same reason it is there: RetryWatcher terminates having forwarded no event and no status, so the
// Pod stream ends with neither Cancelled nor Stopped and with nothing else to give it away. That ending is
// exactly the shape the record has to carry, because it is the only thing distinguishing a lost stream from
// an orderly one.
//
// Mutation that turns this red: delete the `obs = col.evidence(...)` defer from run(). Every assertion below
// then has nothing to read, and the record of a run that observed almost nothing goes back to looking like
// the record of a run that observed everything.
func TestARefusedEstablishmentRecordsWhichStreamDiedAndThatNothingWasEstablished(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "",
		errors.New("no watch permission"))
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			List:   fakeSchedulerList,
			Watch: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption) (watch.Interface, error) {
				if isPodList(list) {
					return nil, fmt.Errorf("watch pods: %w", forbidden)
				}
				return newStubWatch(ctx), nil
			},
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	o, events, _, left, qual, win, obs, _ := run(context.Background(),
		func() (client.WithWatch, error) { return fc, nil }, queuelab.ArmNRef, "r18", "queuelab-r18",
		"platform-worker", selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", io.Discard, now, sleep)

	if o.Disposition != dispSetupFailed {
		t.Fatalf("this test is only meaningful on a run refused at establishment, got %s: %s", o.Disposition, o.Reason)
	}
	if obs == nil {
		t.Fatal("a run that refused because it could not observe recorded nothing about its observation; the " +
			"one fact worth keeping about this run exists only as a sentence on somebody's terminal")
	}
	if obs.Established {
		t.Fatalf("the record says every stream was proven open on a run that was refused for the opposite: %+v", obs)
	}
	if len(obs.Streams) != 4 {
		t.Fatalf("the collector opens four views and the record carries %d: %+v", len(obs.Streams), obs.Streams)
	}
	var pod *streamEvidence
	for i := range obs.Streams {
		if obs.Streams[i].Kind == kindPod {
			pod = &obs.Streams[i]
		}
		if obs.Streams[i].BaselineResourceVersion == "" {
			t.Fatalf("the %s stream recorded no resume point, so nothing says what interval it could speak for",
				obs.Streams[i].Kind)
		}
	}
	if pod == nil {
		t.Fatalf("the record names no Pod view at all: %+v", obs.Streams)
	}
	// The ending, and it is the whole point: Cancelled or Stopped would say the caller ended this stream, and
	// this one ended on its own before the caller had asked for anything.
	if !pod.Ended || pod.Cancelled || pod.Stopped {
		t.Fatalf("the Pod stream died before it ever established a watch, and the record reports it as an "+
			"ending the run itself caused: %+v", *pod)
	}

	rec := buildRecord(o, events, left, qual, win, obs, nil, recordIdentity{RunID: "r18", Arm: string(queuelab.ArmNRef)}, nil, false,
		time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a refused run's record must decode: %v\n%s", err, b)
	}
	if got.Validity.Verdict != verdictRefused {
		t.Fatalf("verdict = %q, want %q:\n%s", got.Validity.Verdict, verdictRefused, b)
	}
	if !slices.Contains(got.Validity.Failures, failureObservation) {
		t.Fatalf("the verdict does not name the claim this run actually failed: %+v", got.Validity)
	}
	t.Logf("observation persisted by a truncated run: %+v", *got.Observation)
}

// The whole gate end to end, and the assertion no unit test can make: a passing run's stored record carries
// the observation it made and states, in a field, that its own evidence supports the number beside it.
//
// It is the counterpart to the refused case above, and it is the one that pins the wiring rather than the
// derivation — a deriveValidity that was correct in isolation but never reached from run(), or an observation
// captured and then dropped at buildRecord, would leave every other test in this package green while every
// admissible record in the archive said nothing about how it had been observed.
//
// Mutation that turns this red: delete the `obs = col.evidence(...)` defer from run(). The run is unchanged
// and the number is unchanged, and the record then says a run that observed its whole window observed
// nothing — which deriveValidity correctly refuses, so the verdict flips with it.
//
// What this cannot reach is main's own buildRecord call, for the reason reportRun was extracted in the first
// place: no test can call main. Passing nil for obs there would leave this test green, and the only thing
// standing between that and a shipped archive of observation-less records is that the argument has a name at
// the call site and a live run prints the block it wrote.
func TestAPassingRunRecordsTheObservationAndCallsItselfAdmissible(t *testing.T) {
	if testing.Short() {
		t.Skip("drives run() to a passing disposition, which takes the 45-second observation window")
	}
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: fakeSchedulerCreate,
			Watch:  fakeSchedulerWatch,
			List:   fakeSchedulerList,
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))

	o, events, res, left, qual, win, obs, _ := run(context.Background(),
		func() (client.WithWatch, error) { return fc, nil }, queuelab.ArmNRef, "r19", "queuelab-r19",
		"platform-worker", selfCompletingProtocol(), 45*time.Second, "", "", "", io.Discard, now, sleep)

	if o.Disposition != dispChecksPassed || res == nil {
		t.Fatalf("this test is only meaningful on a run that passed, got %s: %s", o.Disposition, o.Reason)
	}
	if obs == nil || !obs.Established {
		t.Fatalf("a run that published a number recorded no established observation: %+v", obs)
	}
	// Establishment is spent INSIDE the window, so both numbers have to be there: the cost on its own is not
	// readable without the window it came out of, which is the whole argument establishBudget makes.
	if obs.EstablishedNs <= 0 || obs.HorizonNs != int64(45*time.Second) {
		t.Fatalf("the record cannot say how much window establishment cost: %+v", obs)
	}
	if obs.EstablishedNs >= obs.EstablishBudgetNs {
		t.Fatalf("establishment took %v against a %v budget, so this run was already the failure case",
			time.Duration(obs.EstablishedNs), time.Duration(obs.EstablishBudgetNs))
	}
	for _, s := range obs.Streams {
		// Cancelled, because the ordinary shutdown cancels the observation context at the horizon. Neither flag
		// would be a stream that died mid-run, and this run published a number.
		if !s.Ended || !s.Cancelled || s.LastStatus != "" {
			t.Fatalf("the %s stream of a passing run ended as %+v", s.Kind, s)
		}
	}

	rec := buildRecord(o, events, left, qual, win, obs, nil, recordIdentity{RunID: "r19", Arm: string(queuelab.ArmNRef)}, nil, false,
		time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a passing run's record must decode: %v\n%s", err, b)
	}
	if got.Validity.Verdict != verdictAdmissible || len(got.Validity.Failures) > 0 {
		t.Fatalf("a run with every implemented gate's evidence intact is %q, got %+v:\n%s",
			verdictAdmissible, got.Validity, b)
	}
	// Exactly recordUnchecked, and not a roadmap of work still to do. Something is still genuinely outside
	// what this build can check — the canary probes a Pod it creates itself, so the MLTrainingJob-to-Job-to-Pod
	// path a run's workload actually takes is not what was qualified — and the record has to keep saying so.
	// What it must not do is assert this build lacks the very gates whose evidence it carries, which tells its
	// reader to discount that evidence.
	if !reflect.DeepEqual(got.Validity.UnimplementedGates, recordUnchecked(workloadProvenance{})) {
		t.Fatalf("the record's unchecked list is %v, want exactly %v",
			got.Validity.UnimplementedGates, recordUnchecked(workloadProvenance{}))
	}
	t.Logf("verdict persisted by a passing run: %+v", got.Validity)
}

// The claim this file has shipped false twice, now assertable for the first time through run() itself.
//
// The worker is HELD here — this run's own namespace could not be deleted — and the line an operator reads
// has to say that, because the two branches send them to opposite places: a held worker needs
// -inspect-worker and patience, a released one needs nothing at all. Every previous test of this text drove
// reportResidue directly, which cannot show that run() passes the right `held` down to it.
//
// Mutation that turns this red: pass `false` instead of `hold` at tearDownBeforeRelease's reportResidue call.
// The residue, the disposition and the held node are all unchanged — every other test stays green — and the
// operator is told their worker went back while it is still labelled and still tainted.
func TestRunTellsItsWriterTheWorkerIsHeldWhenItIsHeld(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: fakeNodeList,
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.CreateOption) error {
				if _, ok := obj.(*kueuev1beta2.ResourceFlavor); ok {
					return fmt.Errorf("resource flavor quota exhausted")
				}
				return stampUIDOnCreate(ctx, c, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
						obj.GetName(), errors.New("teardown may not delete namespaces"))
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))
	// A plain buffer is safe here because this run is refused at applyFixtures and never starts a collector,
	// so nothing but the main goroutine ever writes to it.
	var stderr bytes.Buffer

	o, _, _, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r20", "queuelab-r20", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", &stderr, now, sleep)

	if o.Disposition != dispResidueLeft || len(left) == 0 {
		t.Fatalf("this harness must reach a residue that holds the worker, got %s: %s with %+v",
			o.Disposition, o.Reason, left)
	}
	got := stderr.String()
	if !strings.Contains(got, "TEARDOWN INCOMPLETE: worker platform-worker stays dedicated") {
		t.Fatalf("the run held its worker and never said so to the writer it was given:\n%s", got)
	}
	if strings.Contains(got, "was released") {
		t.Fatalf("the run told the operator their worker went back while it is still held:\n%s", got)
	}
	// The advice differs by branch and is the half that exists nowhere else — the residue itself reaches the
	// record, this does not.
	if !strings.Contains(got, "-inspect-worker") {
		t.Fatalf("a held worker's report gives the operator no runnable next step:\n%s", got)
	}
}

// The other branch, and the one whose sentence was rewritten twice for claiming more than the code could
// know. Every name left standing here belongs to a previous attempt, so the worker goes back — and the line
// may say only that nothing left carries THIS run's stamp, never that nothing this run created is still on
// the cluster, which a terminating namespace of our own can falsify.
//
// The harness is TestRunTearsDownAroundAStaleFixtureFromAPreviousAttempt's, driven for the text rather than
// for the call ordering.
//
// Mutation that turns this red: pass `true` instead of `hold` at tearDownBeforeRelease's reportResidue call.
// The worker is still released — residueHoldsWorker is what decides that — and the operator is sent to
// -force-release for a node that is already free.
func TestRunTellsItsWriterTheWorkerWentBackWhenItDid(t *testing.T) {
	variant, err := queuelab.ArmAHonor.PolicyVariant()
	if err != nil {
		t.Fatalf("policy variant: %v", err)
	}
	fs, err := queuelab.BuildFixtures(queuelab.StudyReclaim, variant, queuelab.FixtureIdentity{TxID: "tx-previous", RunID: "r21", Namespace: "queuelab-r21"})
	if err != nil {
		t.Fatalf("build fixtures: %v", err)
	}
	stale := fs.Flavor.DeepCopy()
	stale.SetUID("rf-uid-previous")
	fc := fake.NewClientBuilder().WithScheme(fullScheme(t)).WithObjects(node(nil, nil), stale).
		WithInterceptorFuncs(interceptor.Funcs{Create: stampUIDOnCreate, List: fakeNodeList}).Build()
	now, sleep := fakeClock(time.Unix(0, 0))
	var stderr bytes.Buffer

	o, _, _, left, _, _, _, _ := run(context.Background(), func() (client.WithWatch, error) { return fc, nil },
		queuelab.ArmAHonor, "r21", "queuelab-r21", "platform-worker",
		selfCompletingProtocol(), time.Duration(horizonSec)*time.Second, "", "", "", &stderr, now, sleep)

	if o.Disposition != dispResidueLeft || len(left) == 0 {
		t.Fatalf("this harness must reach a foreign-only residue, got %s: %s with %+v",
			o.Disposition, o.Reason, left)
	}
	for _, r := range left {
		if r.Absence != absenceForeign {
			t.Fatalf("this harness must reach a residue that is entirely somebody else's, got %+v", left)
		}
	}
	got := stderr.String()
	if !strings.Contains(got, "worker platform-worker was released") {
		t.Fatalf("the run released its worker and never said so to the writer it was given:\n%s", got)
	}
	if !strings.Contains(got, "nothing left at these names carries this run's stamp") {
		t.Fatalf("the released-worker line does not make the narrow claim it is allowed to make:\n%s", got)
	}
	// The two negatives this branch may not assert: a namespace of this run's own can be observed terminating
	// and classified foreign, so neither of these is knowable here.
	for _, forbidden := range []string{"nothing this run created", "stays dedicated"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("the released-worker line claims %q, which this code cannot know:\n%s", forbidden, got)
		}
	}
}

// A run bought for device evidence that returns none is a failed run.
//
// Device observation is an optional axis everywhere else, and that default is right: a run with no exporter
// is still a valid control-plane measurement, and requiring one would make every kind run impossible. On
// rented hardware the default inverts. A run whose observer was never wired up, or whose exporter died three
// scrapes in, completes normally and writes a well-formed record saying device-not-observed -- which these
// pages call "a CPU run that cost money". The harness was very good at preserving the wrong deliverable.
//
// Mutations that turn this red: ignore the flag; overwrite an earlier failure with this one; invalidate a
// run that DID establish device work.
func TestARunBoughtForDeviceEvidenceFailsWithoutIt(t *testing.T) {
	established := &measurement{Workload: workloadProvenance{DeviceUseEstablished: true}}
	absent := &measurement{Workload: workloadProvenance{WhyNot: "no device observer ran"}}
	passed := outcome{Disposition: dispChecksPassed}

	if got := requireDeviceEvidence(passed, absent, false, io.Discard); got.Disposition != dispChecksPassed {
		t.Fatalf("a run that did not declare device evidence as its deliverable was failed anyway: %v", got)
	}
	got := requireDeviceEvidence(passed, absent, true, io.Discard)
	if got.Disposition != dispDeviceNotEstablished {
		t.Fatalf("a run bought for device evidence returned none and still passed: %v", got)
	}
	if !strings.Contains(got.Reason, "no device observer ran") {
		t.Fatalf("the failure does not carry the gate's own reason, so an operator cannot tell a missing "+
			"exporter from a card that was idle: %q", got.Reason)
	}
	if got := requireDeviceEvidence(passed, established, true, io.Discard); got.Disposition != dispChecksPassed {
		t.Fatalf("a run that established device work was failed by the requirement it satisfied: %v", got)
	}

	// An earlier failure keeps its own reason. The first failure is what an operator acts on, and a desync
	// hidden behind "no device evidence" sends them to the exporter for a broken watch stream.
	desynced := outcome{Disposition: dispCollectorDesync, Reason: "the Pod stream ended on its own"}
	if got := requireDeviceEvidence(desynced, absent, true, io.Discard); got.Disposition != dispCollectorDesync {
		t.Fatalf("a run that had already failed had its reason replaced: %v", got)
	}
}
