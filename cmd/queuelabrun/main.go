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

// Command queuelabrun executes one queuelab arm against a live Kueue cluster: it applies the study's
// dedicated fixtures, labels and taints a dedicated worker, submits the trace on observed-state barriers,
// watches the Workload/Pod/Job/MLTrainingJob lifecycle into a fail-closed ledger, and reconstructs the
// censoring-aware result.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

func main() {
	var (
		armFlag  = flag.String("arm", "", "arm: A-honor | A-ignore | N-ref")
		runID    = flag.String("runid", "", "unique run id (required, no default: a reused id can confound a run)")
		worker   = flag.String("worker", "platform-worker", "node to dedicate to this run")
		doseFlag = flag.String("dose", string(queuelab.DoseSelfCompleting),
			"dose regime: self-completing (an ignoring victim finishes its own service) | grace-bounded "+
				"(an ignoring victim is cut short at the termination grace period)")
		horizonFlag = flag.Duration("horizon", 0,
			"observation horizon; zero means the selected dose regime's own window, which is also its minimum")
		// -out is deliberately not required, even for a non-preview run: -runid has no safe default because
		// ANY default (a reused id, e.g. "r1") collides with a prior run's cluster-scoped fixtures, a defect no
		// amount of printing can undo. An unnamed -out has no equivalent hazard once recordPathFor's default is
		// collision-free and this file prints wherever it wrote — see recordPathFor and reportRun. Requiring it
		// anyway would be a flag every invocation must remember to pass for a safety property the default
		// already has, which is friction without a matching gain.
		// The help text warns about replacement because naming a fixed path is how an operator reintroduces the
		// very defect recordPathFor's default was reshaped to remove: writeRecord replaces by rename, so a
		// wrapper script passing `-out record.json` destroys the previous invocation's record on the next run.
		out = flag.String("out", "", "path to write this invocation's run record; an existing file there is "+
			"replaced, so a fixed path loses the previous run's record (default: a per-invocation name in the "+
			"working directory, which never collides)")
		// The help text no longer says "run without the validity gates": every gate runs either way, and a flag
		// described as waiving checks it does not waive is how an operator learns to reach for it when a real
		// gate refuses them. What it actually does is declare the output uncountable — the record carries a
		// count instead of the ledger and can never be admissible — which is a smaller and true claim.
		preview = flag.Bool("preview", false, "declare this invocation a smoke check: it runs and checks exactly "+
			"as a real run does, but withholds the ledger and its record can never be admissible")

		// Four of these five modes recover from a crash: they inspect, release, or break the Node marker this
		// package's transaction leaves behind. The fifth qualifies the worker. They are flags rather than
		// subcommands only because this CLI was already flag-shaped; see dispatchOperatorMode for why they all
		// run before anything a run needs.
		// -inspect-worker names no node of its own: it reads -worker like every other mode here, so that every
		// command this tool prints as a hint is runnable exactly as printed, whichever mode it points at.
		inspectWorkerFlag = flag.Bool("inspect-worker", false, "read-only: report -worker's ownership state and exit")
		// Not a recovery mode, but it belongs with them for the same reason they are here: it is not a run, and
		// it has to work on a node no run can be allowed on — qualifyWorker refuses a worker with no recorded
		// canary, so a canary that could only be taken by a run could never be taken at all.
		requireDeviceFlag = flag.Bool("require-device", false,
			"declare that device evidence is this run's deliverable: refuse the invocation without an "+
				"observer, and INVALIDATE the run if the observation does not establish that a device did "+
				"the victim's work. Without it a run whose observer was never wired up completes normally "+
				"and records device-not-observed, which on rented hardware is a CPU run that cost money")
		devicePreflightFlag = flag.Bool("device-preflight", false,
			"read-only about the study, but it applies one Pod: run the workload on -worker WITH a device "+
				"request and report whether it reached the CUDA driver. It is the check the termination canary "+
				"cannot make, because the canary strips the device request from its probes")
		terminationCanaryFlag = flag.Bool("termination-canary", false,
			"qualify -worker for the one thing the arms differ by: that a Pod asked to stop actually stops. "+
				"Records the result on the Node; runs consult it and refuse without one")
		releaseStaleFlag     = flag.Bool("release-stale", false, "release -worker's journal for -txid, after confirming the prior process is gone")
		txidFlag             = flag.String("txid", "", "transaction id to release with -release-stale")
		forceReleaseFlag     = flag.Bool("force-release", false, "break -worker's stuck marker into a quarantine record; never frees the node in one step")
		nodeUIDFlag          = flag.String("node-uid", "", "the Node UID to confirm as the -force-release target")
		acceptDivergenceFlag = flag.Bool("accept-divergence", false, "attest that you accept forcing -worker despite not having tool-verified its installed values")
		clearQuarantineFlag  = flag.Bool("clear-quarantine", false, "clear -worker's quarantine record named -quarantine-id")
		quarantineIDFlag     = flag.String("quarantine-id", "", "the quarantine record to clear")
		confirmOwnerDeadFlag = flag.Bool("confirm-owner-dead", false, "attest that the process which held -worker is confirmed dead (required by -release-stale and -clear-quarantine)")

		compareFlag = flag.String("compare", "", "offline: comma-separated record paths or globs to compare, "+
			"reporting whether their between-arm differences exceed what the runs could resolve")
		compareOutFlag = flag.String("compare-out", "", "path to write the -compare document; without it the "+
			"comparison is printed and not persisted, which makes the conclusion unciteable")
		deviceObserverFlag = flag.String("device-observer", "", "the exporter's image digest or version, "+
			"recorded as this observation's provenance. It is DECLARED, not verified: nothing here can tell a "+
			"DCGM exporter from a text file served over HTTP, so the trust comes from where the endpoint was "+
			"deployed and who can reach it. Required whenever -device-metrics is given, and it must not be "+
			"the URL")

		deviceMetricsFlag = flag.String("device-metrics", "", "URL of a DCGM exporter's /metrics for this run's "+
			"worker. Without it the run establishes that a device was RESERVED and nothing about whether it "+
			"was used, which is what every record here says today. The endpoint has to be reachable from "+
			"wherever this binary runs -- a port-forward or a NodePort -- because the runner talks to the "+
			"apiserver and not to the cluster network")

		compareModeFlag = flag.String("mode", "", "with -compare: \"dose\" or \"node\" holds the ARM fixed and "+
			"varies that factor, reporting whether the quota owner's wait responds to it; \"baseline\" pools "+
			"one arm's runs into the restoration figure a later session differences against; \"model\" tests "+
			"held = min(remaining service, grace) against both regimes at once. Empty compares the arms")
		printHorizonFlag = flag.Bool("print-horizon", false, "offline: print this -dose regime's observation "+
			"window in seconds and exit. It exists so a wrapper can budget a session against the same "+
			"derivation the run uses, instead of copying the constants into a shell script where nothing "+
			"would fail when the two disagree")
	)
	flag.Parse()

	// -print-horizon dispatches with -compare, before anything that touches a cluster, because it answers a
	// question about the protocol rather than about a run. hack/gpu-session.sh asks it to learn how long one
	// run can take before deciding whether the session fits inside its deadline; a number it hardcoded
	// instead would be right until someone changed a constant here.
	if *printHorizonFlag {
		p, err := doseProtocolFor(*doseFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		h, err := horizonFor(*horizonFlag, p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		fmt.Println(int(h.Seconds()))
		return
	}

	// -compare dispatches before anything else, including the record path below, because it touches no
	// cluster, holds no worker and produces no RUN record. Putting it here is what lets a conclusion be
	// re-derived from files on a machine that has no access to the lab at all, which is the whole point of
	// making the conclusion an artifact: a reviewer with the ex/ directory can reproduce it.
	if *compareFlag != "" {
		if err := runCompare(*compareFlag, *compareOutFlag, *compareModeFlag); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// The window a record reports has to open before the first refusal can fire, because the refusals below
	// are records too: a run that is refused is precisely the run that used to leave nothing behind, and the
	// more this tool refuses — which is its entire purpose — the more invocations vanished undiagnosably.
	started := time.Now()
	recordPath := recordPathFor(*out, started, os.Getpid())
	// refuseInvocation records a refusal and NEVER RETURNS.
	//
	// Every caller below depends on that: they refuse on values they have not finished validating, so a
	// version of this that returned would let a refused invocation fall through into a run holding a
	// zero-valued arm, namespace or horizon.
	//
	// The caller prints its own message rather than having this do it. That used to be forced by gateRefusal,
	// whose multi-line explanation could not be squeezed behind the "ERROR:" prefix; with it gone the five
	// remaining messages are uniform one-liners, so the split is now a redundancy rather than a requirement.
	// It is left where it is because folding it in here would rewrite the refusal path in the change that
	// lifts a refusal, and those two edits must be separately reviewable. Whichever end it lives at, the
	// ordering is what matters: the cause reaches the operator BEFORE the record is attempted, so a write that
	// fails cannot also swallow the reason the invocation was refused.
	refuseInvocation := func(err error) {
		rec := buildRecord(outcome{Disposition: dispRefusedBeforeCluster, Reason: err.Error()},
			// The dose is left empty on purpose: this refusal fires before the flags are resolved, so no regime
			// has been chosen and naming one would put a claim in the record that no run stood behind.
			nil, nil, nil, nil, nil, nil, recordIdentity{RunID: recordRunID(*runID), Arm: *armFlag}, nil, *preview,
			started, time.Now())
		if werr := writeRecord(recordPath, rec); werr != nil {
			// The record that failed to persist cannot report its own failure, so this is the one outcome that
			// exists only on stderr. It says the record is unproven rather than that nothing changed: a
			// post-rename failure has already put the new content at the path.
			fmt.Fprintln(os.Stderr, "ERROR: run record not persisted:", werr)
		} else {
			// The default path carries a timestamp and a pid, so the operator is told where the record went
			// rather than left to guess at a name this process generated.
			fmt.Fprintln(os.Stderr, "  run record:", recordPath)
			// The read-back cannot change this exit code — a refusal already exits 1 — so what it adds here is
			// that the operator is not handed a path as though the file at it were usable. A refusal record is
			// the one this tool exists to stop losing, and losing it to an unreadable document reads exactly
			// like not writing it at all.
			if verr := verifyRecordReadable(recordPath, *preview); verr != nil {
				fmt.Fprintln(os.Stderr, "ERROR: that record cannot be read back by this build:", verr)
			}
		}
		os.Exit(1)
	}

	// Checked before anything dispatches, because the invocation this catches is one that would otherwise
	// run happily against the wrong node.
	if err := refuseExtraArgs(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		// A stray positional argument is refused before this tool knows whether the invocation was a run or a
		// recovery, so it is recorded: a record naming a refused invocation is worth more than the ambiguity
		// costs, where staying silent would lose the one class of mistake this check exists to catch.
		refuseInvocation(err)
	}

	// None of these modes is a run, so they dispatch before anything a run needs and each exits directly with
	// its own status rather than falling through to run(). Dispatching first is what makes them usable on a
	// worker no run can currently be allowed on — a node held by a dead process, or one with no canary yet.
	// Fields are named, not positional, so the flag that fills each one is visible at the call site.
	if fired, err := dispatchOperatorMode(newClusterClient, operatorModeArgs{
		Arm:    *armFlag,
		Worker: *worker,

		// Which run-only flags were actually TYPED, not which hold a non-zero value: -horizon has a default,
		// so its value alone cannot distinguish "left alone" from "configured", and an operator who set it on
		// a recovery invocation must be told it does nothing rather than left believing it took effect.
		RunOnlyFlags: suppliedRunOnlyFlags(flag.CommandLine),

		Inspect: *inspectWorkerFlag,

		TerminationCanary: *terminationCanaryFlag,
		DevicePreflight:   *devicePreflightFlag,
		DeviceMetrics:     *deviceMetricsFlag,
		DeviceObserver:    *deviceObserverFlag,

		ReleaseStale: *releaseStaleFlag,
		TxID:         *txidFlag,

		ForceRelease:     *forceReleaseFlag,
		NodeUID:          *nodeUIDFlag,
		AcceptDivergence: *acceptDivergenceFlag,

		ClearQuarantine: *clearQuarantineFlag,
		QuarantineID:    *quarantineIDFlag,

		ConfirmOwnerDead: *confirmOwnerDeadFlag,
	}); fired {
		// A recovery mode is not a run, so it leaves no run record: writing one would put a document in the
		// record's place describing something the record's schema does not describe.
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Refused before anything touches the cluster, for the reason decideOperatorMode gives for its own
	// checks: an operator who declared the expensive case and forgot the endpoint must be told that, not a
	// kubeconfig error, and not two minutes of protocol later.
	if *requireDeviceFlag && (*deviceMetricsFlag == "" || *deviceObserverFlag == "") {
		err := errors.New("-require-device declares that device evidence is this run's deliverable and no " +
			"observer is configured; give -device-metrics and -device-observer, which hack/gpu-session.sh " +
			"prints for the node it verified. Without them the run would complete, write a well-formed " +
			"record, and say device-not-observed")
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		refuseInvocation(err)
	}
	if err := requireRunID(*runID); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		refuseInvocation(err)
	}

	// The arm and the namespace are resolved before anything touches the cluster, so a bad flag is refused
	// up front instead of after fixtures are already applied to some namespace.
	arm, err := parseArm(*armFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		refuseInvocation(err)
	}
	namespace, err := namespaceFor(*runID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		refuseInvocation(err)
	}
	protocol, err := doseProtocolFor(*doseFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		refuseInvocation(err)
	}
	horizon, err := horizonFor(*horizonFlag, protocol)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		refuseInvocation(err)
	}

	if *preview {
		fmt.Println(previewBanner)
	}
	// NotifyContext alone would suppress the default terminate-on-signal behaviour, so every wait below is
	// cancellable too; without that pairing a Ctrl-C would look ignored while the worker stayed dedicated.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// run publishes nothing and persists nothing: its deferred teardown and emergency release both amend the
	// outcome AFTER the return value has been chosen, so anything written from inside it could be
	// contradicted a moment later. By the time these seven values exist here, every defer has finished
	// amending them.
	o, events, res, left, qual, win, obs, deviceObs := run(ctx, newClusterClient, arm, *runID, namespace,
		*worker, protocol, horizon, *deviceMetricsFlag, *deviceObserverFlag, recordPath, os.Stderr,
		time.Now, time.Sleep)

	// The measurement is computed once, here, because -require-device has to judge it before the record is
	// built and this is the first point at which every value is final: run()'s deferred teardown and its
	// device-observer goroutine both amend their outputs after the function body returns.
	m := measurementOf(res, horizon.Nanoseconds(), events, deviceObs)
	o = requireDeviceEvidence(o, m, *requireDeviceFlag, os.Stderr)

	os.Exit(reportRun(os.Stdout, os.Stderr, writeRecord, verifyRecordReadable, runReport{
		Outcome: o,
		Events:  events,
		Result:  res,
		Record: buildRecord(o, events, left, qual, win, obs, deviceObs,
			recordIdentity{RunID: *runID, Arm: string(arm), Dose: string(protocol.Regime)},
			m, *preview, started, time.Now()),
		Path:    recordPath,
		Preview: *preview,
	}))
}

// requireDeviceEvidence turns a run that produced no device evidence into a failed one, when the operator
// declared that evidence was the point.
//
// Device observation is an OPTIONAL axis everywhere else, deliberately: a run with no exporter is still a
// valid control-plane measurement, and refusing it would make every kind run impossible. On rented hardware
// that default inverts. A run whose observer was never wired up, or whose exporter died three scrapes into
// the protocol, completes normally, writes a well-formed record, and says device-not-observed -- which this
// lab's own pages call "a CPU run that cost money". The harness was very good at preserving the wrong
// deliverable.
//
// So the requirement is declared rather than inferred. Nothing the run can see distinguishes a real card
// from the fake plugin's advertisement -- both report allocatable devices -- so the harness cannot decide
// for itself that this is the expensive case. The operator says so, and then the harness holds them to it.
//
// It amends the outcome rather than refusing to write a record: the record is how anyone finds out what
// went wrong, and a run that spent its window has evidence about the cluster even when it has none about a
// device.
func requireDeviceEvidence(o outcome, m *measurement, required bool, stderr io.Writer) outcome {
	if !required || m == nil || m.Workload.DeviceUseEstablished {
		return o
	}
	// A run that already failed keeps its own reason. The first failure is the one an operator acts on, and
	// overwriting it with this one would hide a desync behind a missing observer.
	if o.Disposition != dispChecksPassed {
		return o
	}
	why := m.Workload.WhyNot
	if why == "" {
		why = "the record establishes no device use and gives no reason, which is itself a defect"
	}
	_, _ = fmt.Fprintf(stderr, "\nRUN INVALIDATED: -require-device was given and this run establishes no "+
		"device work: %s\n", why)
	return phaseFailure(dispDeviceNotEstablished, "device evidence was required", errors.New(why))
}

// runReport is everything the publish-or-not decision needs, in named fields.
//
// The fields are named for the same reason operatorModeArgs' are: two adjacent bools and three slices in
// positional order is an argument list a caller can silently transpose, and this one decides whether a
// result gets published.
type runReport struct {
	Outcome outcome
	Events  []queuelab.LifecycleEvent
	Result  *queuelab.LabResult
	Record  any
	Path    string
	Preview bool
}

// recordWriter is how reportRun persists, so a test can drive the real ordering against a failing write.
type recordWriter func(path string, v any) error

// recordVerifier is how reportRun reads back what it wrote, injected for the same reason recordWriter is: the
// tests that pin the persist-before-publish ordering hand in a writer that touches no disk, so a read-back
// wired straight to the filesystem would fail on a path those tests never created and turn the ordering rule's
// own coverage into a filesystem test.
type recordVerifier func(path string, preview bool) error

// reportRun persists the record, reads it back, and then, only if the write succeeded, publishes the run; it
// returns the exit code rather than calling os.Exit so the ordering it enforces is testable.
//
// That ordering is the point of the whole task: a non-zero exit cannot retract a number that has already
// been printed, so nothing countable may exist before the record of it is durable. It lived inline in main
// until a reviewer observed that no test can call main, which left the one rule this plan exists to
// establish covered by a manual run of the binary alone.
func reportRun(stdout, stderr io.Writer, write recordWriter, verify recordVerifier, r runReport) int {
	// The same substitution buildRecord applies, so the terminal and the record cannot give two accounts of one
	// run: without it a zero disposition prints as "ERROR: :" — the blank field dispUnclassified exists to
	// replace with a failure that names itself — while the file on disk correctly says it was a bug in run().
	// classified is idempotent, so applying it here does not change what buildRecord already decided.
	o := classified(r.Outcome)
	writeErr := write(r.Path, r.Record)
	// The read-back runs before anything is published, and it decides WHICH CHANNEL the run's numbers go to.
	//
	// The asymmetry with writeErr is still deliberate. A write failure means there may be no durable record at
	// all, so a countable result would sit on a terminal with nothing on disk to answer for it, and nothing
	// prints. An unreadable record is a different state: the bytes are there and durable, and what failed is
	// this build's ability to read them as evidence.
	//
	// An earlier revision drew the wrong conclusion from that difference — it published everything to stdout
	// and relied on a stderr line telling the reader not to quote it. That asks a reader to un-see what a shell
	// redirect has already captured, and it contradicts the sentence the whole artifact layer rests on: the
	// numbers are quotable ONLY because a reader holding the file can re-derive the verdict from the fields
	// beside them. A file this build refuses leaves nothing on stdout with a warrant to be there.
	//
	// Diverting rather than withholding is what keeps the original objection answered, because that objection
	// was right about the facts even though it reached for the wrong remedy. An unreadable record really can be
	// a run's only complete account — a truncated write loses the tail that carried the ledger — and deleting
	// that account to guard against misquotation would be evidence destroyed in the name of protecting
	// evidence. Nothing here is destroyed. It stops arriving on the channel a redirect captures, and arrives
	// under a line saying what it is instead.
	var verifyErr error
	// publish is declared out here because the closing banner below sits outside this block and must follow the
	// output it closes: a banner left on stdout over content that went to stderr labels an empty channel.
	publish := stdout
	if writeErr == nil {
		verifyErr = verify(r.Path, r.Preview)
		_, _ = fmt.Fprintln(stderr, "  run record:", r.Path)
		if verifyErr != nil {
			publish = stderr
			_, _ = fmt.Fprintln(stderr, "ERROR: the record this run wrote cannot be read back by this build:", verifyErr)
			_, _ = fmt.Fprintln(stderr, "  the record is the deliverable, so what follows is not evidence and is not "+
				"on stdout: a document this build's own reader refuses cannot answer for the numbers under it. "+
				"They are kept because they may be this run's only remaining account, not because they can be "+
				"quoted.")
		}
		// Gated on Preview for the same reason the ledger below is, and it was NOT — which made the two
		// accounts of one guarantee disagree. buildRecord omits measurement from a preview because "handing
		// back the numbers directly would return exactly what the withholding protects", and this line then
		// printed wastedGPUSeconds, totalOccupancyGPUSeconds, admittedWaitP95 and every per-row figure to
		// stdout, where a shell redirect captures them just as well as a file would. The banner was the only
		// thing separating those numbers from a real run's.
		switch {
		case r.Result != nil && o.Disposition == dispChecksPassed && !r.Preview:
			_, _ = fmt.Fprint(publish, "\n"+queuelab.RenderResult(*r.Result))
		case r.Result != nil && o.Disposition == dispChecksPassed:
			_, _ = fmt.Fprintln(publish, "\n  (withheld: this invocation was declared a smoke check, and its "+
				"numbers read the same as a run's once they are out of this process)")
		}
		_, _ = fmt.Fprintf(publish, "\nledger: %d events\n", len(r.Events))
		// A preview record deliberately carries a count and no events, so that an invocation its own author
		// declared uncountable cannot emit anything reconstructable. Printing the ledger would hand back
		// exactly that through a shell redirect, which makes the record's structural guarantee decorative, so
		// the events are withheld from a preview's output for the same reason they are withheld from its
		// record. The withheld line no longer claims the gates were skipped — they were not, and a preview
		// whose stated reason for withholding is untrue invites the reader to discount the withholding too.
		if r.Preview {
			_, _ = fmt.Fprintln(publish, "  (withheld: this invocation was declared a smoke check, and a printed "+
				"ledger reconstructs just as well as a written one)")
		} else {
			printEvents(publish, r.Events)
		}
	}
	// The closing banner has to print even when the run failed or its record could not be persisted, or
	// output already on the terminal is left under an opening banner alone and could be mistaken for output
	// nobody flagged.
	//
	// It goes to stdout, NOT to publish, and the difference is the whole point of the paragraph above. main
	// prints the opening banner to stdout unconditionally, so following publish to stderr on a refused
	// read-back left stdout holding an opening banner and nothing else — precisely the state this banner
	// exists to prevent, reintroduced by the change that diverted the ledger. A banner is a property of the
	// channel it opened on; where the content went is a separate question, and the refusal on stderr is what
	// answers it.
	if r.Preview {
		_, _ = fmt.Fprintln(stdout, previewBanner)
	}
	if writeErr != nil {
		// The record that failed to persist cannot report its own failure, so this is the one outcome that
		// exists only on stderr, and nothing above rendered: an unrecorded result must not be published.
		// It says the record is unproven rather than that the previous one survived, because a post-rename
		// failure has already replaced it.
		_, _ = fmt.Fprintln(stderr, "ERROR: run record not persisted:", writeErr)
		_, _ = fmt.Fprintf(stderr, "  the outcome was %s: %s\n", o.Disposition, o.Reason)
		return 1
	}
	// Both failures still reach an operator who suffered both, which is what this branch is for: the
	// disposition is the account of the RUN and the read-back failure is an account of the ARTIFACT, and a
	// reader needs to know which of the two they are holding. What changed is the order — the read-back
	// failure now prints above, because it has to introduce the diverted output rather than explain it
	// afterwards — so this no longer reports the disposition first, and saying it did would be a comment
	// crediting an ordering the code stopped having.
	if o.Disposition != dispChecksPassed {
		_, _ = fmt.Fprintf(stderr, "ERROR: %s: %s\n", o.Disposition, o.Reason)
	}
	if o.Disposition != dispChecksPassed || verifyErr != nil {
		return 1
	}
	return 0
}

// recordPathFor names the record, and the default names it per invocation.
//
// A single fixed default is what made a refusal destructive: every record this build can produce landed on
// one path, writeRecord replaces by rename, and so a mistyped invocation in the working directory silently
// took the previous run's record with it — a refusal erasing evidence, in the task written to stop refusals
// erasing evidence. The timestamp orders the files and the pid separates two invocations within the same
// second, so two records cannot collide by construction rather than by convention.
//
// It is not derived from the run id, because a refusal can fire before -runid has been read at all and a
// record that only existed for invocations identified enough to name a file would miss exactly the refusals
// this record was added to make visible.
func recordPathFor(out string, started time.Time, pid int) string {
	if out != "" {
		return out
	}
	return fmt.Sprintf("queuelabrun-record-%s-%d.json", started.UTC().Format("20060102T150405Z"), pid)
}

// unidentifiedRunID stands in for the run id of an invocation refused before it supplied one.
//
// decodeRunRecord requires a non-empty run id, and the refusal that fires when -runid is missing still has
// to leave a readable record; runIDPattern forbids parentheses, so this can never be mistaken for, or
// collide with, a run id somebody actually passed.
const unidentifiedRunID = "(unidentified)"

func recordRunID(runID string) string {
	if runID == "" {
		return unidentifiedRunID
	}
	return runID
}

// clusterClientFunc is how dispatchOperatorMode obtains its client.
//
// It is a parameter rather than a direct call to newClusterClient so a test can drive the REAL dispatch path
// — including the context it builds and hands to each mode — against a fake cluster. Without that seam the
// only thing a test could reach is the context helper in isolation, and a regression restoring an unbounded
// context at the dispatch site would pass the whole suite.
type clusterClientFunc func() (client.WithWatch, error)

// dispatchOperatorMode runs at most one of the five non-run modes and reports whether one was requested.
//
// None of them is a run, so the caller must invoke this before anything a run needs: an operator needs
// -inspect-worker and the release/quarantine modes to work precisely on a worker no run can be allowed on,
// otherwise a crashed process could orphan a Node with no in-tool way to see or clear it — and the
// termination canary has to run there too, since qualifyWorker refuses a node that has no recorded canary,
// so a canary that could only be taken by a run would be unreachable on every node needing one. All argument
// validation happens in decideOperatorMode, entirely before connect below: a malformed invocation must be
// refused before it needs a kubeconfig, not after failing to reach one.
func dispatchOperatorMode(connect clusterClientFunc, args operatorModeArgs) (fired bool, err error) {
	mode, err := decideOperatorMode(args)
	if mode == modeNone && err == nil {
		return false, nil
	}
	if err != nil {
		// decideOperatorMode only returns an error once a mode was actually requested, so this is always a
		// fired, refused attempt, never the "nothing requested" case above.
		return true, err
	}

	c, err := connect()
	if err != nil {
		return true, err
	}
	// A signal firing mid-patch is exactly what could leave a mutation half applied, and there is no wait
	// in any of these modes worth making cancellable, so they all run on a context no signal can cancel out
	// from under them — but bounded, not indefinite, for the reason spelled out at operatorModeTimeout.
	ctx, cancel := operatorModeContext(mode)
	defer cancel()

	switch mode {
	case modeInspect:
		return true, inspectWorker(ctx, c, args.Worker)
	case modeTerminationCanary:
		// The real clock, unlike the teardown executor's: this mode's budgets are the grace period and a
		// container start, both of which are things happening on a cluster rather than intervals a test needs to
		// skip past. The injection exists so the tests can drive the loops without spending them.
		return true, terminationCanary(ctx, c, args.Worker, time.Now, time.Sleep, os.Stdout)
	case modeDevicePreflight:
		// The real clock, like the canary's: this mode's budget is an image pull and a driver context, both of
		// which are things happening on a cluster rather than intervals a test needs to skip past.
		return true, devicePreflight(ctx, c, args.Worker, args.DeviceMetrics, args.DeviceObserver,
			time.Now, time.Sleep, os.Stdout)
	case modeReleaseStale:
		return true, releaseStale(ctx, c, args.Worker, args.TxID)
	case modeForceRelease:
		return true, forceQuarantine(ctx, c, args.Worker, args.NodeUID)
	case modeClearQuarantine:
		return true, clearQuarantine(ctx, c, args.Worker, args.QuarantineID)
	default:
		// decideOperatorMode's own switch is exhaustive over the same six modes, so reaching this means the
		// two switches drifted apart, not that the operator did anything wrong.
		return true, fmt.Errorf("internal error: unhandled operator mode %d", mode)
	}
}

// newClusterClient builds the same scheme and kubeconfig-derived client for run() and the operator modes,
// so a recovery action and an ordinary run see the cluster identically.
func newClusterClient() (client.WithWatch, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	for _, add := range []func(*runtime.Scheme) error{platformv1.AddToScheme, kueuev1beta2.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	return c, nil
}

// previewBanner marks preview output so it cannot be mistaken for a countable result.
//
// A preview is not a weaker run — it executes the same gates and records the same evidence — it is an
// invocation whose author declared in advance that its output is not to be counted, and that declaration is
// the only thing separating the two on a terminal. So the banner has to bracket the output rather than appear
// once where scrolled-past text could miss it: the reader deciding whether to quote a number is the one who
// has to see it.
const previewBanner = "==================== PREVIEW: SMOKE CHECK ONLY, NOT EVIDENCE ===================="

// teardownBudget bounds one run's teardown, and it is a constant for the same reason the horizon is: the
// deadline decides whether a run reports a clean teardown or residue, and a knob that could shorten it is a
// knob that can buy a clean-looking record by not waiting for the answer.
//
// The namespace phase is what sizes it. Deleting the namespace waits on kube-controller-manager to remove
// its contents, and the Pods inside carry terminationGraceSec (30s) on top of however long kubelet takes to
// get to them; the two Kueue phases behind it clear in seconds once nothing references them. Three minutes
// is several times over that shape, and it is also the ceiling on how long a stuck teardown holds this
// process before it records the residue and hands the operator a still-dedicated worker.
const teardownBudget = 3 * time.Minute

// teardownContextTimeout is deliberately LARGER than teardownBudget, and the gap is the whole point.
//
// The expiry that has to be reported as residue is the poll loop's own, measured on the injected clock. Were
// the context to expire first, every read and delete would start failing with a cancellation, the executor
// would classify those as absenceUnknown, and a teardown that simply ran out of time would be
// indistinguishable from an orderly shutdown. This context exists only as a backstop against an apiserver
// that never answers at all, so it is bounded but never the binding constraint.
const teardownContextTimeout = teardownBudget + time.Minute

// teardownContext is derived from Background rather than from the run's context, for the same reason
// cleanupContext is: a Ctrl-C landing mid-teardown must not abandon a half-deleted namespace and then
// release the worker on top of it. Containment work runs to completion.
func teardownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), teardownContextTimeout)
}

// tearDownBeforeRelease deletes everything this run created and reports whether the worker must be kept.
//
// It is a function rather than inline code because run() has to do this twice — once on the path that
// returns normally and once in the deferred path covering every early return — and two copies of a decision
// this consequential is two places for them to drift apart.
//
// hold is true for residue this run is answerable for and for a teardown that could not run at all, and true
// is what HOLDS THE WORKER. Releasing it strips the dedication label and the NoSchedule taint from a node
// whose namespace may still be running this run's GPU Pods, and there is no marker that keeps a scheduler off
// a node while letting the run be over — forceQuarantine's annotation means nothing to the scheduler. So a
// teardown that did not finish sacrifices worker availability to preserve isolation. That is a stated choice,
// and the operator is handed the recovery command rather than left to find it.
//
// The disposition and the hold are decided separately, and only the hold turns on whose objects these are.
// Anything left standing at one of this run's names is amended into the outcome, because the record's residue
// and its disposition must not disagree: a record saying "checks passed" while carrying a residue array is
// two accounts of one run, and this program's whole shape is a refusal to emit those.
//
// j and recordPath are here only for the stamp on the held branch below. The journal is what proves the node
// this writes on is still the one this transaction acquired, and recordPath is threaded down from main rather
// than recomputed because recordPathFor's default carries a timestamp and a pid: a second call would name a
// different file, and a record pointing at a file nobody wrote is worse than one carrying no path at all.
//
// stderr is threaded rather than taken as os.Stderr, for the reason reportResidue itself takes a writer: the
// TEARDOWN INCOMPLETE lines below are the ones this file has twice shipped a false sentence in, and a claim
// written to a package-level stream is a claim no test can fail. run() passes its own writer down.
func tearDownBeforeRelease(c client.Client, s seed, j journal, worker, recordPath string, stderr io.Writer,
	now func() time.Time, sleep func(time.Duration), o outcome) (outcome, []residue, bool) {
	tctx, cancel := teardownContext()
	defer cancel()
	result, err := deleteTargets(tctx, c, s, s.TxID, now, sleep, teardownBudget)
	if err != nil {
		// A teardown that could not even compute its target set deleted nothing and proved nothing, so for
		// every decision below it is the worst case: hold the worker, and name the cause in the record rather
		// than letting the run look like it cleaned up after itself.
		//
		// This is the one residue-left with nothing in its residue, and after recoverTargets stopped refusing
		// per batch it is no longer reachable from cluster state at all — a txID that disagrees with the seed,
		// a seed enumerate refuses, a target kind with no reader. Each is a bug in this program, which is why
		// the reason carries the error text: there is no object to name.
		o = o.amend(dispResidueLeft, fmt.Sprintf("teardown could not run: %v", err))
		// Nothing is stamped on the worker here, and that is not an oversight: this is the one hold whose
		// residue is empty, and a record naming no object explains nothing — decodeResidue refuses to read one
		// back, so the next refusal would quote it as unreadable rather than say nothing. The reason above still
		// reaches the run record, which is where a cause with no object to name belongs.
		reportResidue(stderr, worker, nil, true)
		return o, nil, true
	}
	if len(result.Residue) == 0 {
		return o, nil, false
	}
	// amend rather than assignment, exactly as the deferred emergency release does: a run that failed
	// reconstruction AND then failed to clean up is not the same event as one that only failed to clean up,
	// and the record is the only account of either that survives the process.
	o = o.amend(dispResidueLeft, fmt.Sprintf("teardown left %d object(s) after %s",
		len(result.Residue), result.Elapsed.Round(time.Second)))
	hold := residueHoldsWorker(result.Residue)
	reportResidue(stderr, worker, result.Residue, hold)
	if hold {
		// Written only when the worker is actually held: a released worker is refused by nothing, so a record
		// there would be quoted by no refusal and would outlive the hold it describes. releaseAcquired deletes
		// it on the way past anyway, which makes a stamp on that branch pure waste as well as wrong.
		//
		// On cleanupContext for the same reason every release-path write is: this runs after teardown has
		// already spent its budget, and a Ctrl-C landing here must not be what decides whether the operator is
		// told why their worker is held.
		sctx, scancel := cleanupContext()
		serr := stampResidue(sctx, c, j, result.Residue, time.Now().UTC().Format(time.RFC3339), recordPath)
		scancel()
		if serr != nil {
			// NOT an outcome change. What contains the GPU Pods is the label and the taint, and they are already
			// installed and were never what failed here; amending the disposition on this would misreport a run
			// that did exactly what it decided to do. What is lost is narrower and worth naming on its own.
			_, _ = fmt.Fprintf(stderr, "  could not record why on the worker itself: %v\n"+
				"  the next run will be refused this worker without being told what was left\n", serr)
		}
	}
	return o, result.Residue, hold
}

// residueHoldsWorker decides whether what teardown could not remove is a reason to keep the worker dedicated.
//
// The worker is held to contain GPU Pods, and only this run's own objects can be holding any. absenceForeign
// says the name is held under a stamp or a UID this run never recorded, which means nothing of ours is there:
// either we never created anything at that name (a leftover from a previous attempt under the same run id —
// the ordinary rerun, since only the namespace is ever cleaned up by hand), or ours was deleted and something
// else took the name, which frees a namespace's contents on the way. Holding for that contains nothing, and
// releasing restores the node to exactly the state this run acquired it in — the leftovers were already there
// and were not keeping it busy then either.
//
// The condition is deliberately "nothing left is ours" rather than "something foreign is here": one foreign
// flavor alongside a namespace of ours that will not go away says nothing about that namespace's Pods, so any
// entry that is not foreign holds the worker. absenceUnknown holds it too, by the same rule that makes it the
// zero value — a target nobody could classify must fail toward "still here".
//
// Both the worker and the record carry the fact onward, and they carry different halves of it. Holding true
// stamps residueKey on the node, so the next acquisition's foreign-owner refusal can say which objects are
// still standing instead of quoting a transaction id at somebody; the run record carries the full residue
// either way, held or released, so a reader who has the file still learns what a released run left behind.
func residueHoldsWorker(left []residue) bool {
	for _, r := range left {
		if r.Absence != absenceForeign {
			return true
		}
	}
	return false
}

// reportResidue tells the operator what is still standing at this run's names, and what happened to their
// worker as a result.
//
// The residue also reaches the record, so this is not the only account of it — that is the difference
// between this and the WORKER NOT RESTORED line, which for a long time was printed and then lost. What only
// exists here is the advice, and it differs by case because the two cases need opposite things: a stuck
// object of our own must not have its finalizer stripped, and a name another transaction holds is not ours to
// clear at all.
//
// w is a seam for the same reason reportRun already takes injected writers: without one, nothing this
// function prints is assertable, and a false claim in it — this file shipped one, twice, before a review
// caught it on a real cluster — has nothing to fail a test against. Call sites pass os.Stderr.
func reportResidue(w io.Writer, worker string, left []residue, held bool) {
	if held {
		_, _ = fmt.Fprintf(w, "TEARDOWN INCOMPLETE: worker %s stays dedicated; its GPUs may still be in use\n",
			worker)
	} else {
		// held is false only when residueHoldsWorker found every entry absenceForeign (see its own comment),
		// so what follows is provably true of THIS branch: every name below is held under somebody else's
		// stamp, not this run's own. "nothing this run created is still on the cluster" used to stand here
		// instead, and it is a claim this code cannot make: since the Terminating/foreign classification
		// landed, a namespace this run itself created and deleted can still be observed Terminating and
		// classified absenceForeign (a different object can take a terminating name before it finally frees),
		// so the run's own object can be exactly what is named below. What holds regardless is narrower —
		// nothing below carries this run's stamp — and that is the claim this line now makes.
		//
		// "not under this run's stamp" rather than "under somebody else's": absenceForeign is reached by two
		// routes, a UID that is not the one recovery recorded and a recoverTargets stamp check that also
		// refuses an object carrying NO stamp at all. Naming a foreign owner would presuppose a stamp that
		// may not exist, which is the same shape of unprovable claim this line was rewritten to stop making.
		//
		// Saying the worker stays dedicated when it does not would send the operator to -force-release for a
		// node that is already free, and the objects named below are not theirs to delete on this run's say-so.
		_, _ = fmt.Fprintf(w, "TEARDOWN INCOMPLETE: worker %s was released; nothing left at these names carries "+
			"this run's stamp\n", worker)
	}
	for _, r := range left {
		_, _ = fmt.Fprintf(w, "  %s %s: %s\n", r.Observation.Target.Kind, r.Observation.Target.Name,
			absenceName(r.Absence))
	}
	if held {
		_, _ = fmt.Fprintf(w, "  do NOT strip a stuck namespace's finalizer: that orphans its contents, and "+
			"every absence check afterwards reports clean over objects that are still running\n")
		_, _ = fmt.Fprintf(w, "  run: queuelabrun -inspect-worker -worker %s\n", worker)
		return
	}
	_, _ = fmt.Fprintf(w, "  rerun under a run id of its own, or clear those objects first once you have "+
		"established the transaction that created them is gone\n")
}

// phaseFailure is classifyPhaseFailure with the cause folded into the reason.
//
// The record has no field for an error and main prints only the reason, so a reason naming the phase alone
// would leave every refusal saying what the run was doing and never what went wrong — the diagnosability
// the record was added to provide, lost at the point it is written. classifyReleaseFailure already folds its
// cause in the same way.
func phaseFailure(phase disposition, what string, err error) outcome {
	return classifyPhaseFailure(phase, fmt.Sprintf("%s: %v", what, err), err)
}

// run executes one arm and reports what happened to it, publishing and persisting nothing.
//
// The outcome is a NAMED return because the deferred teardown and emergency release below both run after the
// return value has been chosen: a record written from inside this function could be contradicted by a
// TEARDOWN INCOMPLETE or WORKER NOT RESTORED line moments later, so the caller persists only what the defers
// have finished amending. left is named for the same reason and carries the residue out to the record —
// residue that only reached stderr would be printed and lost, which is the failure the record exists to end.
// Every return path sets o; a zero disposition reaching the record would be a silent lie about what happened.
// qual is named for the same reason again, and carries what the worker was found to be BEFORE this run
// created anything on it — including on the path where that observation is what refused the run, which is the
// path whose evidence would otherwise exist only as a sentence on somebody's terminal.
// win is the last of them and carries the continuous evidence: what the worker did for the whole window
// between acquisition and release, and what the Node looked like on either side of that release. It is
// written by a defer like the others, because the restoration audit only exists once the release has run and
// the release runs after this function has chosen what to return.
// obs is the newest of them and carries the observation's own evidence: what each stream's baseline
// established, how much of the window establishment cost, and how every stream ended. It is a defer too, and
// for a reason the others do not share — the endings are only final once the consumers have been joined, and
// each of the many returns below joins them in its own way, so a capture written out at every return site
// would be the one thing in this function most likely to be forgotten at the next one added.
//
// connect is a parameter rather than a direct newClusterClient call for the same reason dispatchOperatorMode
// takes one: without that seam the amendment this function is shaped around is reachable only by reading it,
// and a regression that stopped the defer amending would leave the suite green.
//
// stderr is an injected writer for the reason reportRun and reportResidue already take theirs: everything this
// function tells an operator — the two reportRestoration call sites, the failed residue stamp, TEARDOWN
// INCOMPLETE, WORKER NOT RESTORED — used to go straight to os.Stderr, where no test could assert it, and a
// false sentence in one of them survived two reviews on exactly that footing. It sits beside now and sleep
// because it is the same kind of thing: a dependency injected so the real path can be driven, not a value the
// run is configured with. Stdout is deliberately left alone; this seam is for the claims, not for the log.
//
// now and sleep are the teardown executor's clock, injected for the same reason and nothing else: teardown
// polls to a three-minute budget, and a test that had to spend three real minutes to see a run report residue
// would be a test nobody runs. Everything else in this function reads the real clock directly.
//
// recordPath is passed in rather than derived here because main already named the file: recordPathFor's
// default embeds a timestamp and a pid, so recomputing it would produce a second name, and the residue record
// this run may stamp on the worker would invite the next operator to open a file nobody wrote. It sits after
// horizon rather than beside worker deliberately — runID, namespace and worker are already three adjacent
// strings a caller can transpose in silence, and a fourth would make that worse.
func run(ctx context.Context, connect clusterClientFunc, arm queuelab.Arm, runID, namespace, worker string,
	protocol doseProtocol, horizon time.Duration, deviceMetricsURL, deviceObserverIdentity string,
	recordPath string, stderr io.Writer,
	now func() time.Time, sleep func(time.Duration)) (o outcome, events []queuelab.LifecycleEvent,
	res *queuelab.LabResult, left []residue, qual *qualification, win *ownershipWindow,
	obs *observationEvidence, device *queuelab.DeviceObservation) {
	study := queuelab.StudyReclaim
	c, err := connect()
	if err != nil {
		o = phaseFailure(dispClientFailed, "connecting to the cluster", err)
		return o, events, res,
			left, qual, win, obs, device
	}

	// The corrected trace gives the victim its own duration instead of sharing one with the co-tenant, so the
	// co-tenant's release cannot be mistaken for the reclamation under test.
	trace, err := queuelab.TerminationContractTrace(protocol.VictimServiceSec, protocol.DoseSec, protocol.Regime)
	if err != nil {
		o = phaseFailure(dispProtocolBuildFailed, "building the trace", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	// ValidateTrace still runs because the corrected trace must satisfy the reclaim study's semantic rules,
	// not just the termination-contract shape.
	if err := queuelab.ValidateTrace(study, trace); err != nil {
		o = phaseFailure(dispProtocolBuildFailed, "trace invalid", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	// The SAME resolved protocol the trace came from, not the package constant. Passing doseSec here while the
	// trace was built from protocol.DoseSec is the exact disagreement the schedule's own guard refuses, and it
	// refused six live runs before this line was corrected — the guard working as designed, and the reason the
	// dose and the horizon are resolved together rather than read from wherever each caller happens to look.
	schedule, err := queuelab.TerminationContractSchedule(trace, protocol.DoseSec)
	if err != nil {
		o = phaseFailure(dispProtocolBuildFailed, "building the schedule", err)
		return o, events, res,
			left, qual, win, obs, device
	}

	// Everything teardown needs is computed BEFORE the worker is acquired, because the journal that takes
	// ownership is also what carries it.
	//
	// These two used to sit below ensureNamespace, which was a hole: the namespace could exist with no seed
	// naming it. Moving them above the first Create fixed that half. The remaining half was that the seed was
	// only ever a local variable — so a crash after acquisition left a marked node and fixtures on the
	// cluster with nothing durable saying what to delete, and enumerate refuses a seed missing any field
	// rather than guessing. Neither call does any I/O, so nothing is paid for computing them this early, and
	// an invalid arm or an unbuildable fixture set now fails before the run touches the cluster at all.
	policyVariant, err := arm.PolicyVariant()
	if err != nil {
		o = phaseFailure(dispSetupFailed, "resolving the arm's policy variant", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	txID := newTxID()
	fs, err := queuelab.BuildFixtures(study, policyVariant, queuelab.FixtureIdentity{
		TxID: txID, RunID: runID, Namespace: namespace,
	})
	if err != nil {
		o = phaseFailure(dispSetupFailed, "building fixtures", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	s := seed{
		Schema:    teardownSeedSchema,
		TxID:      txID,
		RunID:     runID,
		Arm:       string(arm),
		Study:     study,
		Variant:   policyVariant,
		Namespace: namespace,
	}

	// Ownership is taken before any namespace or fixture exists, so a refused run leaves nothing of its own
	// behind for the next one to trip over.
	j, err := acquireWorker(ctx, c, worker, s.identity())
	if err != nil {
		o = phaseFailure(dispAcquisitionRefused, fmt.Sprintf("acquire worker %s", worker), err)
		return o, events, res,
			left, qual, win, obs, device
	}
	releaseAttempted := false
	// workerHeldForResidue is the deliberate refusal to release, not a release that failed. Teardown sets it
	// when it could not prove this run's objects gone, and it suppresses the emergency release below for the
	// same reason the explicit release is skipped on that path: a node whose namespace may still be running
	// GPU Pods must keep its label and its NoSchedule taint, or the next run acquires a worker that only
	// looks free. It is a second flag rather than a reuse of releaseAttempted purely for readability at each
	// setting site: neither flag is read anywhere except the single OR below, and nothing that prints to the
	// operator — reportResidue, WORKER NOT RESTORED — consults which one fired. There is no behavioural
	// difference to preserve here; collapsing the two into one flag changes no decision this function makes.
	workerHeldForResidue := false
	// The emergency release covers the paths that return early; the run's own release below sets
	// releaseAttempted before it runs, whatever it returns, so this defer stays a no-op for every path that
	// reached it. It must key on ATTEMPTED rather than succeeded: if the explicit release below fails with
	// ownership-lost (a node observed FREE, not one this defer could do anything more for), re-running it
	// here would print a second, misleading "WORKER NOT RESTORED" for a release that already ran and already
	// invalidated the run.
	defer func() {
		if releaseAttempted || workerHeldForResidue {
			return
		}
		relCtx, relCancel := cleanupContext()
		defer relCancel()
		// The audit is attached to the window this function already published, because this defer runs after
		// the one that published it.
		//
		// A nil window here does NOT mean there is nothing to attach. This defer only runs at all on a path
		// that acquired the worker, so the node carries this run's label and taint either way; the window is
		// nil when the sentinel could not be started, which refuses the run but leaves those markers behind.
		// Dropping the audit there discarded the only evidence of whether they came off, and clusterRestored
		// — which demands proof only where a window exists — then read that silence as containment holding.
		// So a carrier is synthesized, marked as one.
		audit, rerr := auditedRelease(relCtx, c, j)
		if win == nil {
			win = &ownershipWindow{Node: worker, TxID: txID, NeverOpened: true}
		}
		win.Restoration = audit
		// Printed here as well as at the inline release, because this defer is the release EVERY early return
		// takes — including the run this gate invalidates. A drifted or unreadable restoration that reported
		// itself only on the happy path would be silent on exactly the runs an operator is already staring at.
		reportRestoration(stderr, worker, audit)
		if rerr != nil {
			_, _ = fmt.Fprintf(stderr, "WORKER NOT RESTORED: %v\n  run: queuelabrun -inspect-worker -worker %s\n",
				rerr, worker)
			// This defer runs after the return value was chosen, so amending it is the only thing that stops
			// the record being written and then contradicted by the line just printed. amend keeps whatever
			// was originally decided as the cause, because a run that failed reconstruction AND lost its
			// worker is not the same event as one that only lost its worker.
			o = o.amend(dispWorkerNotRestored, fmt.Sprintf("emergency release: %v", rerr))
		}
	}()
	fmt.Printf("  worker %s acquired: tx=%s (if this process dies, run: queuelabrun -inspect-worker -worker %s)\n",
		worker, txID, worker)

	// The ownership window opens as early as it possibly can: the journal exists (there is no tuple to compare
	// a Node against before that) and nothing else has happened yet.
	//
	// It used to open after qualifyWorker, which was wrong in a way that only measurement shows. Nothing here
	// depends on qualification, and qualifyWorker does a Node Get plus an unfiltered cluster-wide Pod List —
	// the most expensive call in setup, seconds on a real cluster — all of it inside the unwatched sliver
	// between acquire's verify and this baseline. Opening first shrinks that sliver to one Get-plus-List, puts
	// the qualification reads inside the window rather than before it, and gives an environment-unqualified run
	// a window in its record instead of nothing.
	//
	// It refuses the run when it cannot be opened, for the same reason col.start does: nothing downstream can
	// compensate for a view that was never opened, and a run that measures without one is back to proving its
	// exclusivity at two instants and asserting it for everything in between.
	sentinel, serr := startOwnershipSentinel(ctx, c, j)
	if serr != nil {
		_, _ = fmt.Fprintf(stderr, "OWNERSHIP WINDOW NOT OPENED: %v\n", serr)
		o = phaseFailure(dispSetupFailed, "opening the continuous ownership view of the worker", serr)
		return o, events, res,
			left, qual, win, obs, device
	}
	// releaseAudit is set by whichever release ran INLINE; the deferred emergency release attaches its own to
	// the published window instead, because it runs after this defer has already published it.
	var releaseAudit *restorationAudit
	// Registered between the emergency release above and the teardown below, which puts it SECOND in the LIFO
	// order: teardown, then this, then the emergency release. Two orderings matter and this satisfies both.
	//
	// It must run before any release, because a release removes this transaction's markers on purpose and a
	// view still open through one would record the run's own restoration as the divergence this gate refuses.
	// Every release is either inline above this defer's own execution or in the defer registered before it, so
	// that holds. It must also run before the emergency release so the window exists for that release to
	// attach its audit to.
	//
	// What this defer actually covers differs by path, and both halves are worth stating because the previous
	// version of this comment claimed the second one for every run and that was false.
	//
	// A run that reaches the horizon does NOT close its window here. It closes inline below, before teardown
	// and before the release, and that ordering is a requirement rather than a convenience: the release strips
	// this transaction's label, taint and journal on purpose, Close drains what the view has already delivered,
	// and a window still open across it would fold the run's OWN restoration as an installed-values-diverged
	// violation — inventing the exact failure this gate exists to detect, on a run that did everything right.
	// So on the happy path, and on every return past the inline close (cancelled, invalidated, reconstruct- and
	// cardinality-refused), this defer finds the window already closed and only publishes it.
	//
	// It is the pre-horizon failures — setup, qualification, establishment, submit, a barrier — where this
	// runs as a close, and there it runs AFTER teardown. That is safe rather than merely tolerated: teardown
	// deletes namespaces and fixtures and at most writes the residue ANNOTATION on the Node, while
	// verifyInstalled compares the label, the taint and the UID, so nothing teardown does can trip the window.
	// It also means those runs keep watching for a third party across teardown's whole budget; see
	// ownershipWindow's note on what that does and does not mean in the record.
	defer func() {
		sentinel.Close()
		w := sentinel.Window()
		w.Restoration = releaseAudit
		win = &w
	}()
	fmt.Printf("  ownership window open on %s from resourceVersion %s (tx=%s)\n",
		worker, sentinel.Window().BaselineResourceVersion, txID)

	teardownAttempted := false
	// Registered AFTER the emergency release above, and that is the requirement, not an accident of where the
	// code happened to land: defers run LIFO, so registering this one later is what makes it run first.
	// Inverted, the worker's dedication label and NoSchedule taint would come off while this run's namespace
	// still held its GPUs, and the next run would acquire a node that only looks free. (The window's own defer
	// now sits between the two and runs between them; it changes nothing here, since teardown never touches the
	// markers the window compares.)
	//
	// It covers every early return from here down; the path that returns normally calls the same function
	// inline, because an inline release later in the body would otherwise beat a defer registered here.
	defer func() {
		if teardownAttempted {
			return
		}
		teardownAttempted = true
		var hold bool
		o, left, hold = tearDownBeforeRelease(c, s, j, worker, recordPath, stderr, now, sleep, o)
		if hold {
			workerHeldForResidue = true
		}
	}()

	// The worker is qualified here: after acquisition, so nothing new can land on it while this looks, and
	// before the run's first Create, so a refusal leaves nothing of ours behind to clean up.
	//
	// It sits BELOW the teardown defer rather than above it even though it creates nothing, so that everything
	// from this point down is covered by one blanket guarantee. The cost is a teardown pass over objects that
	// were never created on the refusal path, which observes them absent and reports no residue; the gain is
	// that an edit which later adds a Create to this stretch cannot fall outside the containment by accident.
	//
	// It runs for a preview too, because run() takes no preview flag and should not: -preview decides how the
	// invocation's output is LABELLED, never what it does on the cluster. This protects the premise a
	// measurement is about rather than the admissibility of its result, and a smoke check of a machine that is
	// not the one under test is not a weaker smoke check but a different one.
	// The trace is passed alongside the fixtures because the quota sum is only one of the two lower bounds:
	// a Pod is scheduled whole onto one node, so a single row larger than the node advertises can never be
	// scheduled at all no matter how much aggregate quota the queues hold. See requiredGPU.
	req, err := requiredGPU(fs, trace)
	if err != nil {
		o = phaseFailure(dispEnvironmentUnqualified, "sizing the worker against this run's fixtures and trace", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	var qerr error
	// The observation is assigned to the named return before the error is inspected, so the refusal path
	// records what it saw rather than only that it refused.
	//
	// The contract is rendered here, at the call site, for the reason req is derived here: both are what this
	// run would actually do, and the qualification is the comparison of that against what the machine has been
	// shown to support. harnessTerminationContract reads the measurement package's own renderer, so the
	// combination qualified is the combination submitted rather than a description of it.
	contract, cerr := harnessTerminationContract()
	if cerr != nil {
		_, _ = fmt.Fprintf(stderr, "ENVIRONMENT NOT QUALIFIED: %v\n", cerr)
		o = phaseFailure(dispEnvironmentUnqualified, "building the termination contract", cerr)
		return o, events, res,
			left, qual, win, obs, device
	}
	qual, qerr = qualifyWorker(ctx, c, worker, req, contract)
	if qerr != nil {
		_, _ = fmt.Fprintf(stderr, "ENVIRONMENT NOT QUALIFIED: %v\n", qerr)
		o = phaseFailure(dispEnvironmentUnqualified, fmt.Sprintf("qualifying worker %s", worker), qerr)
		return o, events, res,
			left, qual, win, obs, device
	}
	fmt.Printf("  worker %s qualified: %d allocatable %s (this arm needs %d, bound by the %s), %d pod(s) on "+
		"the node and none holding a device\n", worker, qual.AllocatableGPU, gpuResourceName, qual.RequiredGPU,
		qual.RequiredBoundBy, qual.PodsOnNode)
	// Which qualification the run stood on, printed with the separation it established rather than with its id
	// alone: the id says a document was consulted, and these two numbers are what that document actually
	// established about the mechanism the arms differ by.
	//
	// The reference cannot be nil on this line. qualify attaches it only when the consult returned one, and
	// appends a failure on every route by which it did not — so a nil reference and a nil qerr cannot both
	// hold, and this line is past the qerr check.
	fmt.Printf("    termination canary %s (taken %s): honouring probe stopped after %dms, ignoring probe "+
		"after %dms\n", qual.TerminationCanary.CanaryID, qual.TerminationCanary.QualifiedAt,
		qual.TerminationCanary.HonorStoppedAfterMs, qual.TerminationCanary.IgnoreStoppedAfterMs)

	if err := ensureNamespace(ctx, c, namespace, txID); err != nil {
		o = phaseFailure(dispSetupFailed, fmt.Sprintf("ensuring namespace %s", namespace), err)
		return o, events, res,
			left, qual, win, obs, device
	}
	if err := applyFixtures(ctx, c, fs, policyVariant, txID); err != nil {
		o = phaseFailure(dispSetupFailed, "applying fixtures", err)
		return o, events, res,
			left, qual, win, obs, device
	}

	fmt.Printf("run %s: arm=%s ns=%s worker=%s horizon=%s\n",
		runID, arm, namespace, worker, horizon)

	// The horizon is stamped here, once, as an absolute instant on the collector's own clock: the barrier
	// loop, the observation wait and the reconstruction's censoring boundary all read that one instant, so
	// none of them can be a different number than the others or than the horizon this run was configured with.
	col := newCollector(c, namespace, runID, horizon)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// establishedIn is stamped where establishment finishes rather than read off the clock inside the defer,
	// which would measure everything the run did afterwards as well: the horizon was wrong in exactly that way
	// once (see horizonNs), and a "cost of establishment" that grew with the length of the run would be the
	// same defect in a smaller field. The line below prints this value rather than taking a second
	// col.elapsed(), so what the operator reads and what the record carries cannot be two different numbers.
	var (
		established   bool
		establishedIn time.Duration
	)
	// Registered after the teardown, window and release defers, which puts it FIRST in the LIFO order — before
	// all three, not after them. Nothing here depends on that: this reads and decides nothing, and none of
	// those three touches a stream. What the defer actually buys is a capture that cannot be forgotten at the
	// next return added to this function, which is how the streams' endings would otherwise stop reaching the
	// record.
	defer func() { obs = col.evidence(established, establishedIn) }()
	// The streams take the run's own context, never a bounded one: startWatchStream's doc comment explains
	// that a deadline handed to the streams makes every later death read as an ordinary cancellation, which is
	// how a run that stopped observing would report itself as having shut down cleanly. The bound on
	// establishment lives inside awaitEstablished instead.
	if err := col.start(cctx); err != nil {
		// col.start has already stopped and joined whatever it opened, so the ledger is quiescent and readable
		// here. The events are carried out for the same reason the path below carries them: a refused run with an
		// empty record is undiagnosable. A baseline refusal leaves the ledger desynced with no events at all,
		// which is itself the fact worth recording.
		events = col.builder.Events()
		o = phaseFailure(dispSetupFailed, "opening the observation streams", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	// Nothing is submitted until every stream is proven open, and that ordering is the point rather than
	// tidiness. This used to be col.start(cctx) followed immediately by the submit loop: a watch that had not
	// been established yet — or was retrying a failure forever, which the old loop did in silence — held up
	// nothing at all, so the run submitted its trace, spent its whole window, and reported whatever fraction
	// of the lifecycle it happened to catch as if it were the whole of it.
	if err := col.awaitEstablished(cctx, establishBudget); err != nil {
		// Every return past col.start joins the consumers before the deferred teardown runs, exactly as the
		// paths below do; the events are carried out for diagnosability even though nothing has been submitted
		// yet and the ledger is expected to be empty.
		cancel()
		col.wait()
		events = col.builder.Events()
		o = phaseFailure(dispSetupFailed, "establishing the observation streams", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	// The device observer starts HERE and not earlier, and the ordering is the same argument the submit loop
	// makes above. Its samples are attributed through the collector's own registry of Pod names, so a scrape
	// landing before the streams are established resolves nothing and counts as unattributed -- an observation
	// that looks partial for a reason that is about start-up rather than about the cluster.
	//
	// Nothing here can fail the run. An observer that will not start leaves the observation nil, which is what
	// every record says today, and the reason travels into the record through EstablishesDeviceWork rather
	// than stopping a run that is otherwise fine. A device claim is an addition to what this lab establishes,
	// not a precondition for it.
	var deviceDone chan struct{}
	if deviceMetricsURL != "" {
		scraper := &queuelab.DeviceScraper{
			// Declared by configuration, not verified here, and the observation carries that admission.
			Observer: queuelab.ObserverDCGM,
			Identity: deviceObserverIdentity,
			Endpoint: deviceMetricsURL,
			Interval: deviceScrapeInterval,
			Resolve:  col.DevicePods(),
			Elapsed:  func() int64 { return col.elapsed().Nanoseconds() },
			Fetch:    func(fctx context.Context) ([]byte, error) { return fetchMetrics(fctx, deviceMetricsURL) },
		}
		deviceDone = make(chan struct{})
		dctx, dcancel := context.WithCancel(cctx)
		defer dcancel()
		go func() {
			defer close(deviceDone)
			o, missed, derr := scraper.Observe(dctx)
			device = o
			if derr != nil {
				_, _ = fmt.Fprintln(stderr, "  device observer:", derr)
			}
			if missed > 0 {
				_, _ = fmt.Fprintf(stderr, "  device observer: %d sample(s) named Pods this run never saw\n", missed)
			}
		}()
		defer func() {
			dcancel()
			<-deviceDone
		}()
	}

	// What establishment cost is printed because it is spent out of the observation window and nothing else
	// reports it: t0 is stamped when the collector is built, so a cluster that took ten seconds to accept four
	// watches leaves ten seconds less window for the schedule to finish in, and the operator would otherwise
	// meet that only as an unexplained barrier miss near the horizon. Measured with col.elapsed() rather than a
	// timer of its own so the number printed is the offset the ledger and the censoring boundary use.
	established, establishedIn = true, col.elapsed()
	fmt.Printf("  observation established at t=%s (out of a %s window; budget %s)\n",
		establishedIn.Round(time.Millisecond), horizon, establishBudget)

	// Execute the barrier-staged schedule.
	for i, step := range schedule {
		if err := waitBarriers(ctx, c, namespace, step.After, col); err != nil {
			if ctx.Err() != nil {
				// A signal reached the barrier wait; recording this as a Desync would misread an operator's
				// Ctrl-C as the arm failing to reach its protocol barrier, so it is reported as cancellation.
				cancel()
				col.wait()
				// The events are carried out even though the run is cancelled: they are not a result and
				// nothing renders them, but a cancelled run with an empty record is undiagnosable, which is
				// the invisibility this record exists to end.
				events = col.builder.Events()
				o = phaseFailure(dispCancelled,
					fmt.Sprintf("observation cancelled while waiting for a barrier before step %d (%s)",
						i, step.Row.Name), err)
				return o, events, res,
					left, qual, win, obs, device
			}
			// col.desync rather than col.builder.Desync: the four watch goroutines are still running and still
			// calling Observe, and the builder has no locking of its own.
			col.desync(fmt.Sprintf("barrier before step %d (%s): %v", i, step.Row.Name, err))
			break
		}
		if err := submit(ctx, c, col, arm, step.Row, namespace); err != nil {
			// Every return past col.start must cancel the streams and join their consumers, the same as the two
			// paths around it. Returning straight out would run the deferred release while four goroutines were
			// still writing to the builder and still observing a namespace the run has abandoned, and nothing
			// would ever reach wg.Wait().
			cancel()
			col.wait()
			events = col.builder.Events()
			o = phaseFailure(dispSetupFailed, fmt.Sprintf("submit %s", step.Row.Name), err)
			return o, events, res,
				left, qual, win, obs, device
		}
		fmt.Printf("  submitted %s (t=%s)\n", step.Row.Name, col.elapsed().Round(time.Second))
	}

	// Observe until the horizon.
	werr := waitForHorizon(ctx, col.deadline)
	cancel()
	col.wait()

	// The window closes HERE, inline, and this line is what makes the deferred close a no-op for every run that
	// gets this far. It has to precede the teardown and the release below rather than being left to that defer:
	// the release strips this transaction's label, taint and journal deliberately, and a view still open across
	// it would record the run's own restoration as an installed-values-diverged violation — a run that did
	// everything right, invalidated by the gate that was watching it. Close drains, so what Window reports
	// afterwards is what happened up to this instant and nothing later.
	//
	// Its verdict is folded into the ledger rather than kept beside it. A run whose worker was shared for part
	// of the measurement must not print a number, and builder.Err() is already the one thing that decides that:
	// a parallel "sort of invalid" state would be a second gate for a caller to forget.
	//
	// It is desynced here rather than from inside the sentinel because the ledger did not exist when the window
	// opened, and because col.wait() above has joined every consumer, which is what makes this the first moment
	// the builder can be touched from this goroutine at all.
	sentinel.Close()
	closed := sentinel.Window()
	if reason := closed.invalidation(); reason != "" {
		_, _ = fmt.Fprintf(stderr, "\nWORKER NOT EXCLUSIVE: %s\n", reason)
		col.desync(reason)
	} else {
		// The count is printed with the verdict for the reason the qualification line prints its Pod count: a
		// negative that was never given a denominator is indistinguishable from a view that was watching the
		// wrong object, and this is the line an operator will read to decide whether the gate was doing
		// anything at all.
		fmt.Printf("  worker %s stayed this run's for the whole window: %d node version(s) observed, none diverged\n",
			worker, closed.NodeVersionsObserved)
	}

	// Reading the builder directly is safe only from here down: col.wait() above joined every watch
	// goroutine, so nothing else can touch it again for the rest of the run.
	events = col.builder.Events()
	if werr != nil {
		// waitForHorizon has exactly one failure — the window was cancelled — so this is named cancelled here
		// rather than through some observation-failure disposition that no code path could ever produce.
		o = phaseFailure(dispCancelled, "observing until the horizon", werr)
		return o, events, res,
			left, qual, win, obs, device
	}

	// Validity is decided before anything is published, because a non-zero exit cannot retract a number
	// that has already been printed or a record that has already been written.
	if err := col.builder.Err(); err != nil {
		_, _ = fmt.Fprintf(stderr, "\nRUN INVALIDATED: %v\n", err)
		o = phaseFailure(dispCollectorDesync, "run invalidated", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	result, err := reconstructAtHorizon(col, arm, trace, events)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "\nRECONSTRUCT ERROR: %v\n", err)
		// Same reasoning as the invalidation path above: a failed reconstruction is not a result either.
		o = phaseFailure(dispReconstructRefused, "reconstruct failed", err)
		return o, events, res,
			left, qual, win, obs, device
	}
	// The victim is identified by ordinal position now, not by cross-watch causal inference, so an unexpected
	// preemption count means that pairing was not actually unambiguous and this reconstruction must be refused
	// rather than printed as if it were.
	if err := arm.AssertCardinality(result); err != nil {
		_, _ = fmt.Fprintf(stderr, "\nRUN INVALIDATED: %v\n", err)
		o = phaseFailure(dispCardinalityRefused, "cardinality check failed", err)
		return o, events, res,
			left, qual, win, obs, device
	}

	// Teardown runs here, INLINE and above the release, rather than being left to the defer registered
	// earlier. A defer runs after every inline statement in the function body, so it cannot get ahead of the
	// release call below however it is registered: the ordering the early-return paths get for free has to be
	// written out explicitly on the path that returns normally.
	//
	// teardownAttempted is set before the call for the same reason releaseAttempted is below: whatever this
	// returns, the deferred copy must not run the whole thing a second time.
	teardownAttempted = true
	var holdWorker bool
	o, left, holdWorker = tearDownBeforeRelease(c, s, j, worker, recordPath, stderr, now, sleep, o)
	if holdWorker {
		// The worker is deliberately kept, so the deferred emergency release must not undo that.
		workerHeldForResidue = true
	} else {
		// The worker is restored, and proven restored, before the result exists anywhere a reader could find
		// it: a run that lost its exclusive worker mid-flight has no claim on the number it computed.
		//
		// releaseOwned rather than releaseAcquired, because a node found already free here is that very case:
		// this run installed the markers and never removed them, so their absence is a lost worker, not a
		// release that had already happened.
		//
		// Set before the call, not after checking its error: the deferred emergency release above must skip
		// because this release ran, regardless of whether it succeeded or invalidated the run.
		releaseAttempted = true
		relCtx, relCancel := cleanupContext()
		releaseAudit, err = auditedRelease(relCtx, c, j)
		relCancel()
		reportRestoration(stderr, worker, releaseAudit)
		if err != nil {
			// This is the one path in the program that can leave a node genuinely held with no further attempt
			// coming: releaseAttempted is already true, so the deferred emergency release above is a no-op, and
			// a real failure here (conflict-bound exhaustion, a non-conflict Patch error, or a verification
			// failure where our markers truly are still installed) means the worker is still marked. The
			// operator needs the same runnable next step the deferred path always printed, not just the reason
			// it failed.
			_, _ = fmt.Fprintf(stderr, "\nRUN INVALIDATED: worker %s not restored: %v\n  run: queuelabrun -inspect-worker -worker %s\n",
				worker, err, worker)
			// classifyReleaseFailure, never classifyPhaseFailure: this release ran on cleanupContext rather
			// than the signal-cancelled run context, so a cancellation surfacing beneath it does not mean the
			// run was cancelled — it means restoration could not be proven, which is the more serious of the
			// two.
			o = classifyReleaseFailure(err)
			return o, events, res,
				left, qual, win, obs, device
		}
	}

	// The result is handed back only under an outcome nothing has amended: restoration proven, and teardown
	// with nothing to report. Testing o rather than re-testing the two conditions is what keeps this from
	// drifting out of step with them — a residue that held the worker and a residue that let it go have
	// already been folded into o, and either way this run did not leave the cluster as it found it, so the
	// number it computed is not published under a disposition that says the checks passed.
	if o.Disposition != "" {
		return o, events, res,
			left, qual, win, obs, device
	}
	res = &result
	o = outcome{Disposition: dispChecksPassed}
	return o, events, res,
		left, qual, win, obs, device
}

// reconstructAtHorizon reconstructs the run's result against the horizon the collector stamped before the
// observation window opened.
//
// It is its own function so the censoring boundary is testable at the CALL SITE rather than only as a value:
// the horizon used to be read off the clock right here, after the watches had been cancelled and joined, and
// a test of the stamped instant alone could not have caught that. Everything this needs is already joined and
// final by the time it runs, so it takes no clock of its own.
func reconstructAtHorizon(col *collector, arm queuelab.Arm, trace []queuelab.TrainingTraceRow,
	events []queuelab.LifecycleEvent) (queuelab.LabResult, error) {
	return queuelab.Reconstruct(string(arm), trace, events, col.horizonNs())
}

// waitForHorizon blocks until the observation deadline or ctx is cancelled, whichever comes first.
//
// A cancelled observation window is an incomplete run, so the signal has to reach this wait and the run
// must then invalidate rather than report whatever it happened to see; the caller classifies this error
// before any result is reconstructed, so a cancellation can never fall through to publication.
//
// These two returns — a cancellation-wrapped error and nil — are the complete set, which is why the caller
// names the failure cancelled outright: there is no non-cancellation way for the observation window to end
// early, and a disposition for one would describe a path no code can take.
func waitForHorizon(ctx context.Context, deadline time.Time) error {
	if remaining := time.Until(deadline); remaining > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("observation cancelled before the horizon: %w", ctx.Err())
		case <-time.After(remaining):
		}
	}
	return nil
}

// printEvents takes its destination rather than writing to stdout directly, so the one caller that must be
// able to withhold the ledger — a preview, whose record carries a count and no events — is a decision a
// test can observe instead of a package-level side effect.
func printEvents(w io.Writer, events []queuelab.LifecycleEvent) {
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "  t=%-8s %-14s %-14s job=%-10s reason=%s\n",
			time.Duration(e.ElapsedNs).Round(time.Second), e.Kind, e.Type, e.Job, e.Reason)
	}
}

// deviceScrapeInterval is how often the device observer polls.
//
// Half a second, which leaves room under the two-second gap a run may blink through: an interval at the limit
// turns one slow response into an uncovered window that the run only discovers when its verdict is computed.
const deviceScrapeInterval = 500 * time.Millisecond

// fetchMetrics reads one scrape from a metrics endpoint.
//
// Plain HTTP with the run's context and nothing else -- no retry, no backoff. The scraper above already
// decides what a failure means, and a transport that quietly retried would hide the very gap the verdict is
// meant to catch.
func fetchMetrics(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device metrics endpoint returned %s", resp.Status)
	}
	// Bounded, because an endpoint that streams forever would otherwise consume the run rather than a scrape.
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
