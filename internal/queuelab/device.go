package queuelab

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// The contract a device observation has to satisfy before a run may claim its GPU did work.
//
// It exists because every record this lab has produced reads device-not-observed, and the reason is not
// modesty. The workload FALLS BACK to arithmetic that makes no driver call wherever the driver is absent,
// which is every run this lab has taken; the cluster advertises nvidia.com/gpu
// through a fake plugin, and a Pod that dropped the resource request entirely would compute the same
// iterations at the same rate. Scheduling those Pods onto real hardware would not change that verdict by
// itself — the iteration counter stays healthy while every operation runs on the CPU — so a session could
// consume scarce GPU time and add no GPU-specific evidence at all.
//
// The one rule everything here follows: THE WORKLOAD IS NOT A WITNESS. A Pod reporting its own device use is
// evidence of nothing, for the same reason a tenant setting a quota-exempt annotation is not evidence of
// exemption — both are claims by the party the check exists to constrain. The observation has to come from
// something the workload cannot write to, and Source is where that is asserted and checked.

// DeviceObserver names what an observation is DECLARED to have come from.
//
// Declared, not verified, and the distinction was security theatre until a review said so. Nothing here can
// tell a DCGM exporter from a text file served over HTTP: the payload is a format anyone can emit, and the
// harness that stamps this value is the same one reading the URL. A closed set that rejects honest new
// observers while admitting arbitrary bytes under an accepted name is worse than an open one, because it
// reads as a check.
//
// So the set stays closed for the reason a vocabulary is closed -- a reader classifies on it -- and the
// TRUST comes from where the endpoint was deployed and who can reach it, which is a property of the cluster
// and not of this code. DeviceObservation.Declared carries that admission into the record, so nobody reading
// one mistakes a name for an attestation.
type DeviceObserver string

const (
	// ObserverDCGM is NVIDIA's DCGM exporter: a DaemonSet scraping the driver directly and labelling each
	// sample with the physical device UUID and the Pod it was serving. The tenant's container cannot write to
	// it, which is the property that matters.
	ObserverDCGM DeviceObserver = "dcgm-exporter"
)

// deviceObservers held a second name, nvidia-smi, and it is deleted rather than kept for later.
//
// Nothing emitted it: the runner declares dcgm-exporter and the parser reads DCGM's format only. What it was
// in practice is a name a hand-authored record could claim, which is this file's own definition of
// worse-than-nothing -- an accepted name over arbitrary bytes reads as a check. It comes back when a pipeline
// produces it.

// deviceObservers is the set an observation may claim. A value outside it is refused rather than trusted,
// which is what stops "workload-self-report" from ever being one.
var deviceObservers = map[DeviceObserver]bool{ObserverDCGM: true}

// maxObserverGap is how long an observation may see nothing before it stops covering the interval.
//
// Two seconds because the interval this has to cover is the device hold, whose whole subject is a window of
// tens of seconds: a gap of that size can hide an entire preemption. It is deliberately shorter than any
// quantity the lab reports, so a run cannot claim continuous observation of something it blinked through.
const maxObserverGap = 2 * time.Second

// minBusySamples is how many samples must show the device working before "it did work" is a finding rather
// than a spike.
//
// One sample is a reading; two spaced across the interval are a state. This is not a statistical threshold
// and does not pretend to be — it exists so that a single stray non-zero utilisation, of the kind a driver
// reports while another process initialises, cannot carry the claim.
const minBusySamples = 2

// minBusySeparation is how far apart two busy samples must be before they count as two.
//
// "Spaced across the interval" is the whole of what minBusySamples means, and counting samples at distinct
// TIMESTAMPS does not deliver it. dcgm-exporter collects on its own cadence and serves a CACHED snapshot
// between collections, while this run scrapes faster than that on purpose -- so one collection is read
// several times, at several distinct timestamps, carrying one instant's reading each time. Counting those
// as two satisfied the gate from a single collection, which is precisely the "reading rather than a state"
// the refusal message claims to prevent. Measured: at PERIOD = 2.6 s the victim's series repeats every value
// in pairs, so its 30 busy samples are 15 busy collections.
//
// maxObserverGap is the right length because it is the bound the exporter's collection interval is already
// required to stay under (see config/dcgm-exporter/daemonset.yaml). A snapshot is therefore served for less
// than maxObserverGap, and two reads at least that far apart cannot be the same snapshot. This needs no
// knowledge of the exporter's actual cadence, which the gate does not have and should not guess.
const minBusySeparation = maxObserverGap

// staleLabelMargin is how far before the interval another Pod's label still counts against exclusivity.
//
// DCGM's Kubernetes labels come from the kubelet's pod-resources socket, which the exporter polls: when a
// container exits, its name can persist on the entity for a scrape or two while that view catches up. A
// margin shorter than the observation's own granularity would let exactly that carry into the window
// unnoticed, so it is twice the longest gap a run may blink through -- two scrapes' worth at the coarsest
// rate this build accepts.
const staleLabelMargin = 2 * maxObserverGap

// StaleLabelMargin is staleLabelMargin, exported so a writer persisting the samples this gate reads can
// bound them by the same margin the gate scans back through.
//
// It is exported rather than duplicated because a record that carried a narrower window than the gate reads
// would drop exactly the samples the exclusivity clause needs, and a reader re-running the gate over that
// record would reach a verdict the run did not.
const StaleLabelMargin = staleLabelMargin

// DeviceSample is one observation of one physical device at one instant.
// The fields carry json tags because a record PERSISTS these samples as its device evidence, and the rest
// of that document is lowerCamel. Without them Go marshals the field names and the artifact reads as two
// schemas in one file. It was free to add: schema 18 declares the block and no committed record carries a
// sample yet.
type DeviceSample struct {
	// AtNs is when the OBSERVER took the reading, on its own clock, as an offset from the run's t0.
	AtNs int64 `json:"atNs"`
	// DeviceUUID is the physical device. It is required: a sample that cannot say which card it watched
	// cannot establish that the card this Pod held did anything.
	DeviceUUID string `json:"deviceUUID"`
	// PodRef is the namespace/name the exporter labelled the sample with, kept even when it cannot be
	// resolved to a UID.
	//
	// It is here for the exclusivity check below rather than for attribution. A sample naming a Pod this run
	// never saw is useless for crediting work, and decisive for refusing it: if that Pod is on the same
	// device, the device was not exclusively the victim's and nothing about its utilisation belongs to
	// anybody in particular.
	PodRef string `json:"podRef,omitempty"`
	// PodUID ties the sample to one Pod, and it is the UID rather than the name because the UID is what the
	// API guarantees unique across TIME. It is empty for a Pod this run did not observe.
	//
	// An earlier version of this comment justified it by claiming a re-executed row produces two Pods with the
	// same name. It does not -- a Job's Pods carry random suffixes, and the recorded runs show the victim's two
	// attempts under two names and two UIDs. The real reason is narrower and survives: a name is free for reuse
	// the moment its Pod is deleted, and an observer that labels by name is read against whatever holds that
	// name when the mapping is resolved. This lab creates bare Pods on the quota-guard path where the name is
	// chosen rather than generated, so the case is reachable here rather than hypothetical.
	PodUID string `json:"podUID,omitempty"`
	// UtilisationPercent is what the observer reported, 0 to 100.
	UtilisationPercent int `json:"utilisationPercent"`
}

// DeviceObservation is everything one observer saw over one run.
type DeviceObservation struct {
	Observer DeviceObserver
	// ObserverIdentity is which build of the observer produced this -- an image digest or version string.
	//
	// It is required for the reason the operator image digest is required in the canary key: "DCGM said so"
	// is not provenance if nobody can say which DCGM. An endpoint URL is NOT an identity and is refused as
	// one: it says where the bytes came from, not what produced them, and the runner filled this field with
	// the URL until a review pointed out that it made the requirement vacuous.
	ObserverIdentity string
	// Endpoint is where the bytes came from, kept beside the identity rather than standing in for it.
	Endpoint string
	// Declared records that Observer above was asserted by whoever configured the run rather than established
	// by anything here. It is required to be true: this build has no way to verify an exporter, and a record
	// that left the field false would be claiming one.
	Declared bool
	// StartedNs and EndedNs bound what the observer was actually running for. An interval outside them is not
	// covered, whatever the samples happen to contain.
	StartedNs int64
	EndedNs   int64
	Samples   []DeviceSample
	// UnlabelledBusySamples counts rows that showed a card WORKING while naming no Pod at all.
	//
	// It exists to separate two failures that produce the same refusal. "No sample for this Pod" is what a
	// card nobody used looks like -- and it is also what a healthy card looks like when the exporter's
	// kubernetes mapping is off, its pod-resources mount is wrong, or its permissions fail: full utilisation
	// with nothing naming a tenant. The first sends an operator to the workload, the second to the exporter,
	// and without this counter the refusal sent them to the first in both cases.
	UnlabelledBusySamples int `json:"unlabelledBusySamples,omitempty"`
	// UnavailableSamples counts rows the exporter answered with "N/A" -- a field it could not read at that
	// instant. An observation that is empty because every row said N/A and one that is empty because nothing
	// was there look identical without this, and they send an operator to different places.
	UnavailableSamples int `json:"unavailableSamples,omitempty"`
}

// DeviceClaim is the question put to an observation: which Pod, over which two intervals.
//
// Two intervals rather than one, because the claim is two claims. Exclusivity, coverage and continuity are
// about the HOLD -- was this card exclusively that Pod's, watched without a blink, while the owner waited
// for it. Whether the Pod used the card at all is about its ATTEMPT, and the hold is only the tail of the
// attempt, during which the victim is being terminated.
//
// Collapsing them was a defect with a direction. In the arm that honours SIGTERM the hold is ~2 s and the
// victim has already stopped computing, so a busyness test over the hold refuses that arm for behaving as
// the arm is defined to behave; the ignoring arm computes through its ~31 s hold and passes. -require-device
// therefore removed the short arm and kept the long one, and the contrast between them is the whole result.
type DeviceClaim struct {
	// PodUID is the attempt the samples must name. A name is not enough: it is free for reuse the moment its
	// Pod is deleted, and this lab creates bare Pods whose names it chooses.
	PodUID string
	// WorkFromNs and WorkToNs are the victim attempt's own interval, from its Pod becoming Ready to it
	// stopping. The busyness clause reads this.
	WorkFromNs int64
	WorkToNs   int64
	// HoldFromNs and HoldToNs are the device hold, from the owner's admission to the victim's stop. Coverage,
	// continuity and exclusivity read this.
	HoldFromNs int64
	HoldToNs   int64
}

// SameWindowClaim builds a claim whose two intervals coincide.
//
// It is for callers that genuinely have ONE interval -- the device preflight's probe has no owner waiting on
// it, so its attempt and its hold are the same window -- and for specs written before the intervals were
// separated, where collapsing them is a faithful translation of what those specs asserted.
//
// It is NOT a convenience for the run path. A run that used it would be asking the busyness question over
// the hold, which is the defect DeviceClaim exists to remove.
func SameWindowClaim(podUID string, fromNs, toNs int64) DeviceClaim {
	return DeviceClaim{
		PodUID:     podUID,
		WorkFromNs: fromNs, WorkToNs: toNs,
		HoldFromNs: fromNs, HoldToNs: toNs,
	}
}

// EstablishesDeviceWork reports whether an observation supports the claim that the named Pod's device did
// work across the whole of an interval, and why not when it does not.
//

// admitsDeviceObserver refuses an observation before any of its numbers are read.
//
// Every clause here is about PROVENANCE rather than about the card: who produced the readings, whether they
// said which build they were, and whether the record admits the source was declared rather than verified.
// A number from an unidentified source is not a weaker measurement, it is not a measurement.
func admitsDeviceObserver(obs *DeviceObservation) (bool, string) {
	if obs == nil {
		return false, "no device observer ran: nothing outside the workload watched the card, so this run " +
			"establishes that a device was RESERVED and nothing about whether it was used"
	}
	if !deviceObservers[obs.Observer] {
		return false, fmt.Sprintf("observer %q is not one this build accepts (%q). The workload is not a "+
			"witness: a Pod reporting its own device use is a claim by the party the check exists to constrain",
			obs.Observer, ObserverDCGM)
	}
	if obs.ObserverIdentity == "" {
		return false, fmt.Sprintf("observer %q did not say which build of it produced these readings; "+
			"\"%s said so\" is not provenance if nobody can say which %s", obs.Observer, obs.Observer, obs.Observer)
	}
	if obs.Endpoint != "" && obs.ObserverIdentity == obs.Endpoint {
		return false, fmt.Sprintf("observer %q gave its endpoint %q as its identity: that says where the bytes "+
			"came from, not what produced them, and any HTTP server can be at a URL",
			obs.Observer, obs.Endpoint)
	}
	if !obs.Declared {
		return false, fmt.Sprintf("observation from %q does not record that its source was DECLARED rather "+
			"than verified. Nothing here can tell a %s from a text file served over HTTP, and a record that "+
			"omitted the admission would read as though something had checked",
			obs.Observer, obs.Observer)
	}
	return true, ""
}

// exclusiveDuringHold refuses a hold whose device carried another Pod's label during it.
func exclusiveDuringHold(obs *DeviceObservation, devices map[string]bool, podUID string, fromNs, toNs int64) (bool, string) {
	// EXCLUSIVITY, and it is the clause that turns a utilisation reading into an attribution.
	//
	// DCGM_FI_DEV_GPU_UTIL is DEVICE utilisation. The Kubernetes labels beside it come from the kubelet's
	// pod-resources socket and say which Pod the device is ALLOCATED to; they do not say which process made
	// it busy. Under time-slicing, MPS, a MIG parent, a stale label during the exit transition, or anything
	// outside Kubernetes touching the card, a positive reading under this Pod's name can be somebody else's
	// work entirely.
	//
	// Rather than infer attribution, this refuses unless the premise holds: every entity this Pod was
	// observed on must have carried no other Pod's label DURING the interval, or in the stale-label margin
	// just before it. Whatever DCGM reports as the entity is the granularity that matters, so a MIG instance
	// allocated exclusively passes and a shared parent device does not.
	//
	// The scope is the window plus a margin, and it used to be the whole observation. That was wrong in a way
	// worth writing down, because it made this clause refuse the experiment it was built to serve: the run's
	// defining event is a HANDOVER -- the borrower releases the card and the owner takes it, on a node with
	// exactly the two devices the protocol needs -- so the owner's label lands on the victim's entity a few
	// seconds after the hold ends, inside the same observation, in every arm. Every run would have been
	// refused device attribution, deterministically, by the gate meant to license it. A review found it by
	// running the experiment's own shape through this function.
	//
	// The threats the clause exists for are all CONCURRENT with the hold or immediately before it:
	// time-slicing, MPS and a MIG parent put two tenants on one entity at the same time, and a stale label
	// carries the previous tenant's name into the start of the window while the kubelet's pod-resources view
	// catches up. Serial reallocation AFTER the window is not ambiguity, it is the thing being measured.
	//
	// This is the same move the worker qualification makes: a node with another Pod's device on it is refused
	// rather than reasoned about.
	for _, s := range obs.Samples {
		if !devices[s.DeviceUUID] {
			continue
		}
		if s.PodUID == podUID {
			continue
		}
		if s.AtNs > toNs || s.AtNs < fromNs-int64(staleLabelMargin) {
			continue
		}
		other := s.PodRef
		if other == "" {
			other = s.PodUID
		}
		if other == "" {
			continue
		}
		return false, fmt.Sprintf("device %s carried Pod %s's label as well as %s's during the hold, "+
			"so its utilisation is not attributable to either: DCGM reports what the DEVICE did and the "+
			"labels say only what it was allocated to, which time-slicing, MPS, a MIG parent or a stale label "+
			"at the exit transition all make ambiguous", s.DeviceUUID, other, podUID)
	}
	return true, ""
}

// busyDuringAttempt answers whether the Pod's own card was seen working at distinct instants of the attempt.
func busyDuringAttempt(obs *DeviceObservation, devices map[string]bool, claim DeviceClaim) (bool, string) {
	podUID := claim.PodUID
	// BUSYNESS is judged over the victim's ATTEMPT, not over the hold, and that is the fix for a defect the
	// gate had in one direction.
	//
	// The question is whether this Pod used the card. The hold is the tail of the attempt during which the
	// Pod is being terminated, so in the arm that honours SIGTERM it is idle by definition -- the gate was
	// refusing that arm for doing what the arm exists to do, while the arm that ignores the signal computed
	// through its hold and passed. See DeviceClaim.
	//
	// Coverage is NOT required across the work window. The claim is "it was busy at two separate instants",
	// which partial observation can support; demanding continuity there would refuse an observer that
	// started after the Pod did, for no gain.
	//
	// Busy samples at DISTINCT times, not merely busy rows. A scrape carrying the same device-wide series
	// twice -- which DCGM does emit, and which MIG and duplicated labels make ordinary -- would otherwise
	// satisfy "two samples" from one instant, and one instant is a reading rather than a state. The comment
	// on minBusySamples said "spaced across the interval" while the code counted rows; a review found the gap.
	work := claim.WorkFromNs
	workEnd := claim.WorkToNs
	if work >= workEnd {
		// A ledger that cannot locate the attempt cannot support the use claim either, and saying so is not
		// the same as saying the card was idle.
		return false, fmt.Sprintf("the attempt's own interval is empty (%d..%d ns), so nothing can establish "+
			"that Pod %s used the card rather than merely being allocated one", work, workEnd, podUID)
	}
	// Instants at which some OTHER Pod was also on one of these devices.
	//
	// Exclusivity is checked over the HOLD, because that is the window the claim is about and a tenant that
	// left long before it is serial reallocation rather than ambiguity. Busyness reads the whole ATTEMPT,
	// which is wider -- so a busy sample could sit at an instant where a neighbour's label was on the same
	// card, outside the hold's reach, and be credited to this Pod anyway. A reading at an instant two Pods
	// share says the CARD was working. It does not say which of them was working it, and the whole point of
	// this clause is attributing work to one Pod.
	shared := map[int64]bool{}
	for _, s := range obs.Samples {
		if s.PodUID == "" || s.PodUID == podUID || !devices[s.DeviceUUID] {
			continue
		}
		shared[s.AtNs] = true
	}

	busyAt := map[int64]bool{}
	seen := 0
	for _, s := range obs.Samples {
		if s.PodUID != podUID || s.AtNs < work || s.AtNs > workEnd {
			continue
		}
		// Only the device the hold identified. A busy reading on some other card is not evidence about this one.
		if !devices[s.DeviceUUID] {
			continue
		}
		seen++
		if s.UtilisationPercent > 0 && !shared[s.AtNs] {
			busyAt[s.AtNs] = true
		}
	}
	// Distinct timestamps first, then distinct INSTANTS. The samples arrive in whatever order the observation
	// holds them, so they are sorted before spacing can mean anything.
	times := make([]int64, 0, len(busyAt))
	for at := range busyAt {
		times = append(times, at)
	}
	slices.Sort(times)
	busy := 0
	last := int64(0)
	for i, at := range times {
		if i == 0 || at-last >= int64(minBusySeparation) {
			busy++
			last = at
		}
	}
	if busy < minBusySamples {
		// Two counts, because they send a reader to two different places. A card busy at one instant and a
		// card never busy at all are both refused here, and only the first one means the observer was too
		// fast for its source.
		return false, fmt.Sprintf("the device held by Pod %s was observed working at %d separate instants "+
			"(%d busy samples of %d) across the attempt (%d..%d ns); a card that is allocated and idle is "+
			"the state this whole axis exists to distinguish from one that is computing, and busy samples "+
			"closer together than %s can all be one collection read several times, which is a reading "+
			"rather than a state", podUID, busy, len(times), seen, work, workEnd, minBusySeparation)
	}
	return true, ""
}

// The reason string is the product. A run that fails this check has to be able to tell an operator which of
// the five things went wrong -- no observer, wrong observer, the interval not covered, a gap in the middle,
// or the device idle throughout -- because those send someone to five different places, and because a run
// that comes back "not established" without saying why is indistinguishable from one nobody looked at.
func EstablishesDeviceWork(obs *DeviceObservation, claim DeviceClaim) (bool, string) {
	podUID := claim.PodUID
	fromNs, toNs := claim.HoldFromNs, claim.HoldToNs

	// Two intervals, and which one each question is asked over decides whether one arm can ever pass.
	//
	// busyDuringAttempt was moved to the attempt window for a reason DeviceClaim states: the hold is the tail
	// during which the victim is being terminated, so in the arm that honours SIGTERM it is idle by
	// definition. The move stopped one level too high. The existence check below still selected samples over
	// the HOLD, and reached "no sample names this Pod" before busyness was ever asked.
	//
	// On real hardware that is not a near miss. A measured A-honor run had a hold of 216 ms -- owner admitted
	// at 24.216 s, victim stopped at 24.432 s -- while DCGM scrapes about once a second, so the hold could not
	// contain a sample at all. The same observation carried 43 samples naming that Pod between 3.1 s and
	// 24.1 s, 39 of them busy on one card. A valid run was refused, and the refusal was not even-handed: the
	// ignoring arm computes through a thirty-second hold and passes, so the gate deleted the short arm and
	// kept the long one -- and the contrast between those arms IS the study.
	//
	// So the USE question reads the attempt, and exclusivity and continuity keep reading the hold. The
	// attempt contains the hold, so nothing here widens what the exclusivity clause scans.
	workFrom, workTo := claim.WorkFromNs, claim.WorkToNs
	if workFrom >= workTo {
		// No attempt window in the ledger. The caller already treats that as a claim it cannot fully support;
		// falling back to the hold keeps the old behaviour rather than inventing a wider one.
		workFrom, workTo = fromNs, toNs
	}

	if ok, why := admitsDeviceObserver(obs); !ok {
		return false, why
	}
	if podUID == "" {
		return false, "the interval names no Pod UID, so no sample can be attributed to the attempt that " +
			"held the device rather than to another attempt of the same row"
	}
	if fromNs >= toNs {
		return false, fmt.Sprintf("the interval to cover is empty (%d..%d ns)", fromNs, toNs)
	}
	if obs.StartedNs > fromNs || obs.EndedNs < toNs {
		return false, fmt.Sprintf("the observer ran %d..%d ns and the interval to cover is %d..%d ns, so part "+
			"of it was never watched", obs.StartedNs, obs.EndedNs, fromNs, toNs)
	}

	// attempt answers "did this Pod use a card". mine answers "was the observer watching across the hold".
	attempt := make([]DeviceSample, 0, len(obs.Samples))
	mine := make([]DeviceSample, 0, len(obs.Samples))
	holdDevices := map[string]bool{}
	attemptDevices := map[string]bool{}
	for _, s := range obs.Samples {
		if s.PodUID != podUID {
			continue
		}
		if s.AtNs >= workFrom && s.AtNs <= workTo {
			if s.DeviceUUID == "" {
				return false, fmt.Sprintf("a sample at %d ns names no device: an observation that cannot say "+
					"which card it watched cannot establish that the card this Pod held did anything", s.AtNs)
			}
			attempt = append(attempt, s)
			attemptDevices[s.DeviceUUID] = true
		}
		if s.AtNs >= fromNs && s.AtNs <= toNs {
			mine = append(mine, s)
			if s.DeviceUUID != "" {
				holdDevices[s.DeviceUUID] = true
			}
		}
	}

	// Which card the hold was about is the hold's answer to give, and the attempt's only when the hold has
	// nothing to say.
	//
	// The hold is the sharper witness: a Pod that touched two cards across its attempt but held one while its
	// owner waited is a run about that one. Taking the device set from the attempt unconditionally made a
	// fixture built on exactly that distinction refuse for ambiguity instead of for idleness, which is a
	// worse answer to a question the hold could still answer. The fallback exists for the hold that cannot:
	// 216 ms is shorter than a scrape interval, so it contains no sample and names no card.
	devices := holdDevices
	if len(devices) == 0 {
		devices = attemptDevices
	}
	if ok, why := exclusiveDuringHold(obs, devices, podUID, fromNs, toNs); !ok {
		return false, why
	}
	if len(attempt) == 0 {
		// Three different faults end here, and the refusal has to tell them apart.
		//
		// An unlabelled busy card alone does not establish a broken exporter. A session on real hardware
		// refused with "its kubernetes mapping off or its pod-resources mount broken" while the same
		// observation named nine distinct Pods across 667 of its 760 samples -- the mapping was plainly on,
		// and the message sent an operator to fix a component that was working. This package's own rule is
		// that a quantity must never be described by a cause its ledger does not establish; the accusation
		// is a cause, and only the first branch below has the evidence for it.
		named := map[string]bool{}
		for _, s := range obs.Samples {
			if s.PodUID != "" {
				named[s.PodUID] = true
			}
		}
		switch {
		// An exporter that answered every row with something this build could not read leaves an observation
		// that looks exactly like a card nobody touched. Named first, because it sends an operator to the
		// exporter's value format rather than to the workload -- and because until this counter existed the
		// skip was silent and there was nothing to name it with.
		case obs.UnavailableSamples > 0 && len(obs.Samples) == 0:
			return false, fmt.Sprintf("the observer produced no usable sample at all, and skipped %d row(s) "+
				"whose value it could not read as an integer percent. That is not a card nobody used: the "+
				"exporter answered and this build could not read what it said. Check the value format the "+
				"exporter is emitting before looking at the workload", obs.UnavailableSamples)
		case obs.UnlabelledBusySamples > 0 && len(named) == 0:
			return false, fmt.Sprintf("the observer produced no sample naming ANY Pod, and %d sample(s) showed "+
				"a card WORKING while naming no Pod at all. That is not a card nobody used: it is attribution "+
				"failing while the hardware runs, which is what an exporter with its kubernetes mapping off "+
				"or its pod-resources mount broken produces. Fix the observer, not the workload",
				obs.UnlabelledBusySamples)
		case obs.UnlabelledBusySamples > 0:
			return false, fmt.Sprintf("the observer produced no sample naming Pod %s, though it named %d other "+
				"Pod(s) across %d sample(s), so its kubernetes mapping is working. %d sample(s) showed a card "+
				"WORKING while naming no Pod at all. Why THIS Pod's work went unattributed is not established "+
				"here: it is not a card nobody used, and it is not an exporter that names nobody",
				podUID, len(named), len(obs.Samples), obs.UnlabelledBusySamples)
		}
		return false, fmt.Sprintf("the observer ran across the interval and produced no sample for Pod %s; a "+
			"device held by a Pod nothing sampled is a reservation", podUID)
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].AtNs < mine[j].AtNs })

	// Gaps are measured against the interval's own ends as well as between samples, so an observation that
	// starts late or stops early inside a window it nominally covers is caught.
	prev := fromNs
	for _, s := range mine {
		if s.AtNs-prev > int64(maxObserverGap) {
			return false, fmt.Sprintf("the observer saw nothing for %s between %d and %d ns, which is longer "+
				"than the %s a run may blink through; a gap that size can hide an entire preemption",
				time.Duration(s.AtNs-prev), prev, s.AtNs, maxObserverGap)
		}
		prev = s.AtNs
	}
	if toNs-prev > int64(maxObserverGap) {
		return false, fmt.Sprintf("the observer's last sample for Pod %s is %s before the interval ends",
			podUID, time.Duration(toNs-prev))
	}

	if ok, why := busyDuringAttempt(obs, devices, claim); !ok {
		return false, why
	}
	if len(devices) > 1 {
		return false, fmt.Sprintf("samples for Pod %s name %d different devices; the run cannot say which card "+
			"the hold was about", podUID, len(devices))
	}
	return true, ""
}
