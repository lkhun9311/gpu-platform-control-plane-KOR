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
	"flag"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// The protocol's fixed parameters, stated here rather than accepted from flags.
//
// They are constants because the design of record fixes them and because the previous CLI let the duration
// be chosen freely, which is how the dose silently became 49 seconds instead of the specified 40.
const (
	victimServiceSec = 60
	doseSec          = 40

	// graceBoundedDoseSec returns the owner early enough that the victim still has more service left than the
	// grace period, so an ignoring victim is cut short by the grace period rather than finishing on its own.
	//
	// 20 s leaves 40 s of remaining service against a 30 s grace. It is a second CONSTANT rather than a free
	// flag for the reason the pair above is: the previous CLI let the duration be chosen freely and the dose
	// silently became 49 s instead of 40. A closed set of two named regimes keeps the arithmetic pinned while
	// letting the experiment reach the side of the grace period it could not reach at all before.
	graceBoundedDoseSec = 20

	// terminationGraceSec mirrors internal/queuelab's private terminationGraceSec (the Pod termination grace
	// period the fixture runs with). It is duplicated rather than imported because that package must not
	// change for this fix and does not export the constant.
	terminationGraceSec = 30
	// startupMarginSec covers Pod scheduling and container-start latency on each of the schedule's three
	// sequential Ready waits (a1, victim, owner), none of which the protocol's timing accounts for on its own.
	startupMarginSec = 20
)

// horizonSec is the fixed observation window, derived from the protocol's own timing rather than typed in.
//
// A free -horizon flag is how a previous run silently truncated itself and still exited 0: 60 s cuts the run
// off before the owner is ever Ready. The window has to cover the dose (the wait before the owner returns),
// the victim's full service (so an unpreempted victim in N-ref still has time to run out its service), the
// termination grace period (the victim's worst-case shutdown once preempted), and the startup margin above,
// so it sums all four rather than approximating one of them away.
const horizonSec = doseSec + victimServiceSec + terminationGraceSec + startupMarginSec

// doseProtocol is the fixed timing of one dose regime, resolved together so the horizon cannot be computed
// from one regime's dose while the trace is built from another's.
//
// That pairing is the point. The horizon is DERIVED from the dose, so a second regime with its own dose and
// a horizon still summed from the first would truncate the run — the failure horizonSec exists to rule out,
// reintroduced by the change that added the regime.
type doseProtocol struct {
	Regime           queuelab.DoseRegime
	VictimServiceSec int
	DoseSec          int
}

// HorizonSec sums the same four terms horizonSec does, for this regime's own dose.
func (p doseProtocol) HorizonSec() int {
	return p.DoseSec + p.VictimServiceSec + terminationGraceSec + startupMarginSec
}

// doseProtocolFor resolves a regime name against the closed set, refusing anything else.
//
// An unknown name is an error rather than a fallback to the default, for the reason SenderConnForMode gives
// for the same shape: silently running the regime the operator did not ask for produces a number nobody
// would think to question, and the two regimes measure different things.
func doseProtocolFor(name string) (doseProtocol, error) {
	switch queuelab.DoseRegime(name) {
	case queuelab.DoseSelfCompleting:
		return doseProtocol{queuelab.DoseSelfCompleting, victimServiceSec, doseSec}, nil
	case queuelab.DoseGraceBounded:
		return doseProtocol{queuelab.DoseGraceBounded, victimServiceSec, graceBoundedDoseSec}, nil
	default:
		return doseProtocol{}, fmt.Errorf("unknown dose regime %q; want %q or %q",
			name, queuelab.DoseSelfCompleting, queuelab.DoseGraceBounded)
	}
}

// horizonFor returns the observation horizon for local iteration, refusing anything short of the protocol's
// fixed window.
//
// A flag that could go below the window would reintroduce the exact truncation the derivation exists to rule
// out, so requested only widens the window and never narrows it. Zero means "this regime's own window",
// which is the default: a fixed default computed from one regime's dose would silently truncate the other.
func horizonFor(requested time.Duration, p doseProtocol) (time.Duration, error) {
	minHorizon := time.Duration(p.HorizonSec()) * time.Second
	if requested == 0 {
		return minHorizon, nil
	}
	if requested < minHorizon {
		return 0, fmt.Errorf("-horizon %s is below the %s window %s; "+
			"the horizon cannot be shortened without truncating the run", requested, p.Regime, minHorizon)
	}
	return requested, nil
}

// runIDPattern is the shape a run id must take, since it becomes a namespace name.
var runIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// parseArm resolves the CLI's arm argument to the closed set the experiment defines.
//
// The old CLI took a free-form study and variant, so a combination the experiment never defined could be
// requested and would run; refusing anything outside the three arms is what makes the executable unable to
// produce a result the protocol does not describe.
func parseArm(s string) (queuelab.Arm, error) {
	switch queuelab.Arm(s) {
	case queuelab.ArmAHonor:
		return queuelab.ArmAHonor, nil
	case queuelab.ArmAIgnore:
		return queuelab.ArmAIgnore, nil
	case queuelab.ArmNRef:
		return queuelab.ArmNRef, nil
	default:
		return "", fmt.Errorf("arm must be one of %s, %s, %s; got %q",
			queuelab.ArmAHonor, queuelab.ArmAIgnore, queuelab.ArmNRef, s)
	}
}

// requireRunID refuses an empty run id.
//
// The flag used to default to "r1", which made colliding with a previous run's cluster-scoped fixtures
// (ResourceFlavor, ClusterQueue) the default behaviour rather than a rare mistake; there is no safe default,
// so the id must be supplied explicitly.
func requireRunID(runID string) error {
	if runID == "" {
		return fmt.Errorf("-runid is required and has no default: a reused or shared id lets a stale " +
			"cluster-scoped fixture from a different arm silently answer for this run")
	}
	return nil
}

// refusePreviewOut is gone, and the guarantee it stood for moved rather than disappeared.
//
// It refused -preview with -out because -out wrote a bare ledger JSONL, and the trace and dose being
// compile-time constants made such a file enough on its own to reconstruct a number offline. -out now names
// the run record, and previewRecord has no field a ledger can be decoded out of at all, so the guarantee is
// a property of the type rather than of a flag combination — and a flag check that no longer guards
// anything is worse than none, because it stops a preview naming where its own record goes and forces every
// record onto one path that the next refusal overwrites.

// variantLabelKey mirrors internal/queuelab's private variantLabel, duplicated for the same reason as
// terminationGraceSec above: that package must not change and does not export it.
const variantLabelKey = "queuelab.gpu-platform/variant"

// checkFlavorVariant refuses a run whose cluster-scoped ResourceFlavor was left behind by a prior run under
// a different policy variant.
//
// ResourceFlavor and ClusterQueue are cluster-scoped and named from the run id alone, so reusing an id after
// deleting only the namespace leaves them in place; without this check the run would proceed under the old
// arm's mechanism while printing this arm's label.
func checkFlavorVariant(existingLabels map[string]string, wantVariant string) error {
	if got := existingLabels[variantLabelKey]; got != wantVariant {
		return fmt.Errorf("resource flavor already exists with variant %q, this arm requires %q "+
			"(run id likely reused from a different arm)", got, wantVariant)
	}
	return nil
}

// namespaceFor derives this run's namespace from its run id.
//
// It is derived rather than taken from a flag because two runs sharing a namespace is what allowed a
// previous run's leftover objects to satisfy this run's barriers, and fixed trace job names made the
// collision certain.
func namespaceFor(runID string) (string, error) {
	if !runIDPattern.MatchString(runID) {
		return "", fmt.Errorf("run id %q must be a lowercase DNS label", runID)
	}
	ns := "queuelab-" + runID
	if len(ns) > 63 {
		return "", fmt.Errorf("run id %q makes the namespace name too long", runID)
	}
	return ns, nil
}

// operatorModeArgs bundles the operator-mode flags into named fields.
//
// dispatchOperatorMode used to take these eleven values as positional bool/string parameters; two adjacent
// bools (accept-divergence, clear-quarantine) could be swapped at the call site and the compiler would not
// notice, silently rewiring the attestation gate for the most destructive path onto the wrong mode. A
// struct with named fields turns that mistake from silent into a field name that visibly does not match
// what it is assigned.
type operatorModeArgs struct {
	Arm    string
	Worker string

	// Inspect names no node of its own: like every other mode here it acts on Worker, so a hint this tool
	// prints for one mode cannot be right while the same hint for another is not runnable as printed.
	Inspect bool

	// TerminationCanary is the one mode here that is not recovery. It sits with them because it is not a run
	// either — it produces no result, leaves no run record, and above all it has to be runnable on a worker no
	// run can yet be allowed on: qualifyWorker refuses a node with no recorded canary, so if taking one required
	// a run the two would deadlock on each other.
	TerminationCanary bool

	// DevicePreflight sits beside TerminationCanary for the same reason, and answers the question the canary
	// structurally cannot: the canary's probes have their device request stripped, so nothing it reports is
	// about a card. This one keeps the request and asks whether the workload reaches the driver at all.
	DevicePreflight bool

	// DeviceMetrics and DeviceObserver configure the preflight's observer check, and belong to that mode
	// alone. They are refused beside the others for the same reason the run-only flags are: an invocation
	// that names an exporter and then never scrapes it reads to its author as configured.
	DeviceMetrics  string
	DeviceObserver string

	ReleaseStale bool
	TxID         string

	ForceRelease     bool
	NodeUID          string
	AcceptDivergence bool

	ClearQuarantine bool
	QuarantineID    string

	// ConfirmOwnerDead is shared by -release-stale and -clear-quarantine because both turn on the same
	// judgement — that the process which held the worker is gone — and nothing this tool can observe makes
	// that judgement for the operator.
	ConfirmOwnerDead bool

	// RunOnlyFlags names the run-configuration flags the operator actually supplied alongside a mode.
	//
	// It is the flag NAMES rather than their values because that is the only thing that distinguishes a flag
	// left at its default from one typed with the same value as its default, and -horizon has a default.
	RunOnlyFlags []string
}

// runOnlyFlagNames are the flags that configure a run and mean nothing to a recovery mode.
//
// -worker is not among them: every mode acts on it. -arm is not either, because it has its own refusal that
// says the specific thing worth saying — a mode is not a run.
var runOnlyFlagNames = map[string]bool{"runid": true, "out": true, "preview": true, "horizon": true}

// suppliedRunOnlyFlags reports which run-only flags were present on the command line, in a stable order.
//
// flag.Visit walks only the flags actually set, which is what makes this a report of what the operator typed
// rather than of what the defaults happen to be. It takes the FlagSet rather than reading the global one so
// the refusal it feeds can be tested without a process-wide command line.
func suppliedRunOnlyFlags(fs *flag.FlagSet) []string {
	var names []string
	fs.Visit(func(f *flag.Flag) {
		if runOnlyFlagNames[f.Name] {
			names = append(names, "-"+f.Name)
		}
	})
	sort.Strings(names)
	return names
}

// refuseExtraArgs refuses leftover positional arguments, of which this executable accepts none.
//
// -inspect-worker becoming a bool is what makes them dangerous rather than merely untidy: the old
// `-inspect-worker platform-worker2` invocation still parses cleanly, with the node name silently demoted
// to a positional argument, and would report on -worker's default instead of the node the operator named —
// an operator deciding whether to break a stuck node from a report about a different machine.
func refuseExtraArgs(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument(s) %v: this tool takes flags only, and -inspect-worker now "+
			"names its node with -worker like the other operator modes", args)
	}
	return nil
}

// operatorMode names which recovery mode decideOperatorMode selected, or that none was requested.
type operatorMode int

const (
	modeNone operatorMode = iota
	modeInspect
	modeReleaseStale
	modeForceRelease
	modeClearQuarantine
	modeTerminationCanary
	modeDevicePreflight
)

// decideOperatorMode is the pure validation layer for the six non-run modes — the four recovery ones, the
// termination canary and the device preflight: it decides which mode (if any) was requested and whether the
// invocation is well-formed, entirely without touching the cluster.
//
// This has to run, in full, before anything that needs a kubeconfig: an operator on a box with no cluster
// access who mistypes a flag combination must see the flag-combination refusal, not a "kubeconfig: ..."
// error that has nothing to do with what they actually got wrong.
//
// modeNone is returned both when no mode was requested (err is nil; the caller falls through to the
// ordinary run) and when a requested mode is malformed (err is non-nil; the caller must still stop rather
// than fall through). The two are distinguished by err, not by the mode value alone.
func decideOperatorMode(a operatorModeArgs) (operatorMode, error) {
	requested := 0
	for _, on := range []bool{a.Inspect, a.ReleaseStale, a.ForceRelease, a.ClearQuarantine, a.TerminationCanary,
		a.DevicePreflight} {
		if on {
			requested++
		}
	}
	if requested == 0 {
		return modeNone, nil
	}
	if requested > 1 {
		return modeNone, fmt.Errorf(
			"only one of -inspect-worker, -release-stale, -force-release, -clear-quarantine, " +
				"-termination-canary, -device-preflight may be given at a time")
	}
	// An operator mode is a recovery tool, not a run, so combining one with -arm would let "recover the
	// node" and "run an arm" be read as a single invocation.
	if a.Arm != "" {
		return modeNone, fmt.Errorf("an operator mode cannot be combined with -arm: it is a recovery tool, not a run")
	}
	// The same argument as -arm, for the flags that were merely ignored rather than refused. An invocation
	// that names a run id, an output path, a preview or a horizon and then quietly does none of those things
	// reads to its author as configured, which is the worst kind of no-op: they will believe the recovery
	// they just ran was the one they described.
	// The device flags are the preflight's own, and mean nothing to the five other modes.
	if !a.DevicePreflight && (a.DeviceMetrics != "" || a.DeviceObserver != "") {
		return modeNone, fmt.Errorf(
			"-device-metrics and -device-observer belong to -device-preflight and to a run; no other " +
				"operator mode scrapes anything, so this invocation would have looked configured while " +
				"doing nothing of the kind")
	}
	if len(a.RunOnlyFlags) > 0 {
		return modeNone, fmt.Errorf(
			"an operator mode cannot be combined with %s: a recovery mode ignores every run-only flag, so "+
				"this invocation would have looked configured while doing nothing of the kind",
			strings.Join(a.RunOnlyFlags, ", "))
	}

	switch {
	case a.Inspect:
		return modeInspect, nil
	case a.TerminationCanary:
		// No attestation of its own. The three destructive modes require one because nothing this tool can
		// observe tells the operator whether the previous process is dead; this mode asks nobody to judge
		// anything — it takes the worker through the ordinary transaction, which refuses a node somebody else
		// holds, and everything it creates it names and deletes itself.
		return modeTerminationCanary, nil
	case a.DevicePreflight:
		// An endpoint with no identity would be scraped, parsed, and then refused by EstablishesDeviceWork for
		// a reason about provenance rather than about the card -- after the Pod ran. Refusing here costs the
		// operator a second instead of a minute, and says the thing worth saying.
		if a.DeviceMetrics != "" && a.DeviceObserver == "" {
			return modeNone, fmt.Errorf(
				"-device-metrics requires -device-observer: an observation whose source cannot be named is " +
					"refused by the same gate a run uses, so scraping without it can only ever fail")
		}
		// No attestation, for the canary's reason: it takes the worker through the ordinary transaction, which
		// refuses a node somebody else holds, and the one Pod it creates it names and deletes itself.
		return modeDevicePreflight, nil
	case a.ReleaseStale:
		if a.TxID == "" {
			return modeNone, fmt.Errorf("-release-stale requires -txid")
		}
		// A matching -txid proves the operator identified the right transaction; it proves nothing about
		// whether that transaction's process is dead, and that is the judgement that actually matters here.
		// Releasing a live run's journal leaves it holding a worker it no longer owns, so this is the mode
		// most likely to cost a run its exclusivity — and, being the one -inspect-worker recommends first,
		// also the most reached for. It is attested like the other two destructive modes.
		if !a.ConfirmOwnerDead {
			return modeNone, fmt.Errorf(
				"-release-stale requires -confirm-owner-dead: a -txid match identifies the transaction but " +
					"cannot show its process is gone, and releasing a live run's journal costs that run its worker")
		}
		return modeReleaseStale, nil
	case a.ForceRelease:
		if a.NodeUID == "" {
			return modeNone, fmt.Errorf(
				"-force-release requires -node-uid: it is the operator's confirmation of the target")
		}
		// Nothing can prove the previous process is dead, so forcing requires the operator's explicit
		// attestation rather than proceeding on the node-uid confirmation alone.
		if !a.AcceptDivergence {
			return modeNone, fmt.Errorf(
				"-force-release requires -accept-divergence: nothing can prove the previous process is dead, " +
					"so this attests it was the operator's judgement")
		}
		return modeForceRelease, nil
	default: // a.ClearQuarantine
		if a.QuarantineID == "" {
			return modeNone, fmt.Errorf("-clear-quarantine requires -quarantine-id")
		}
		if !a.ConfirmOwnerDead {
			return modeNone, fmt.Errorf(
				"-clear-quarantine requires -confirm-owner-dead: clearing is a separate, deliberate act and " +
					"this attests the operator has established the previous owner is dead")
		}
		return modeClearQuarantine, nil
	}
}

// gateRefusal and unimplementedGates are gone, and what they stood for moved into the record rather than
// disappearing.
//
// gateRefusal refused every non-preview invocation and named four missing gates as its reason. All four now
// exist and run on every invocation, preview or not: the streams are opened with resourceVersion continuity
// and their endings recorded, the worker is qualified against this run's own fixtures and against a recorded
// termination canary, the exclusive hold is watched continuously and audited across the release, and the
// record carries all three plus a verdict derived from those fields. A refusal whose stated reason has become
// false is worse than no refusal at all: it teaches its reader that the tool refuses for reasons that are not
// true, which is how the next real refusal gets waived by reflex.
//
// What did NOT move here is the judgement. Nothing decides up front whether an invocation may count — the
// record decides, afterwards, from what the run actually observed, which is why deriveValidity reads only
// persisted fields. A flag could never have made that judgement anyway; it could only have deferred it.
//
// The record's own statement of what it cannot speak for is recordUnchecked, in record.go, and it is
// deliberately not a copy of the roadmap this refusal used to print. See its comment for why one list could
// not serve both.

// selfCompletingProtocol is the default regime, resolved once so callers cannot assemble a doseProtocol whose
// dose and horizon disagree by hand.
func selfCompletingProtocol() doseProtocol {
	return doseProtocol{queuelab.DoseSelfCompleting, victimServiceSec, doseSec}
}
