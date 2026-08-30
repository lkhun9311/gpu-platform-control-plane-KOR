package queuelab

import (
	"testing"
	"time"
)

// held builds a ledger event for a row at an arrival time.
func held(job string, ev EventType, elapsedNs int64) LifecycleEvent {
	return LifecycleEvent{Job: job, Type: ev, ElapsedNs: elapsedNs, ObjectUID: job + "-uid"}
}

// The hold runs from the owner's ADMISSION to the victim's terminal phase, which is the quantity
// held = min(remaining service, grace) is actually about.
//
// Mutation that turns this red: start the interval at the victim's Ready, or end it at the owner's Ready --
// either one turns the hold back into the owner's wait, which is what needed a borrowed platform-cost term
// to test at all.
func TestDeviceHoldRunsFromTheOwnersAdmissionToTheVictimsStop(t *testing.T) {
	ev := []LifecycleEvent{
		held(VictimRow, EventPodReady, 2_000_000_000),
		held(OwnerRow, EventSubmitted, 23_000_000_000),
		held(OwnerRow, EventAdmitted, 24_100_000_000),
		held(VictimRow, EventAttemptStopped, 54_150_000_000),
		held(OwnerRow, EventPodReady, 56_700_000_000),
	}
	got := DeviceHoldNs(ev)
	if got == nil {
		t.Fatal("a ledger carrying both endpoints produced no hold")
	}
	if *got != 30_050_000_000 {
		t.Fatalf("hold = %d ns, want 30.050s (owner admitted 24.100 -> victim stopped 54.150)", *got)
	}
	// The owner's own readiness is 2.6 s later and must not be the endpoint: that interval is the owner's
	// wait, and it contains scheduling and container start that the hold does not.
	if *got == 32_600_000_000 {
		t.Fatal("the hold ran to the owner's readiness")
	}
}

// The delivered dose is not the declared one, and this is what makes the difference visible.
func TestAchievedDoseIsMeasuredRatherThanDeclared(t *testing.T) {
	ev := []LifecycleEvent{
		held(VictimRow, EventPodReady, 2_000_000_000),
		held(OwnerRow, EventSubmitted, 23_286_000_000),
	}
	got := AchievedDoseNs(ev)
	if got == nil {
		t.Fatal("a ledger carrying both endpoints produced no dose")
	}
	if *got != 21_286_000_000 {
		t.Fatalf("dose = %d ns, want 21.286s; the protocol declared 20 and the barrier's poll delivered more",
			*got)
	}
}

// A ledger missing either endpoint says nothing rather than zero. Zero would be the strongest claim the
// interval can make -- an instant release, or a dose delivered the moment the victim was ready.
func TestHoldAndDoseRefuseToInventAnInterval(t *testing.T) {
	onlyAdmit := []LifecycleEvent{held(OwnerRow, EventAdmitted, 24_000_000_000)}
	if h := DeviceHoldNs(onlyAdmit); h != nil {
		t.Fatalf("a ledger with no victim stop reported a hold of %d", *h)
	}
	if d := AchievedDoseNs(onlyAdmit); d != nil {
		t.Fatalf("a ledger with no victim readiness reported a dose of %d", *d)
	}

	// A stop observed before the admission would make the interval a different quantity, not a small one.
	inverted := []LifecycleEvent{
		held(OwnerRow, EventAdmitted, 24_000_000_000),
		held(VictimRow, EventAttemptStopped, 23_000_000_000),
	}
	if h := DeviceHoldNs(inverted); h != nil {
		t.Fatalf("a release observed before the decision reported a hold of %d ns", *h)
	}
}

// A re-executed row delivers its attempts out of order, so the earliest arrival is the endpoint rather than
// the first one folded.
func TestHoldTakesTheEarliestStopOfAReExecutedRow(t *testing.T) {
	ev := []LifecycleEvent{
		held(OwnerRow, EventAdmitted, 24_000_000_000),
		held(VictimRow, EventAttemptStopped, 60_000_000_000),
		held(VictimRow, EventAttemptStopped, 54_000_000_000),
	}
	got := DeviceHoldNs(ev)
	if got == nil || *got != 30_000_000_000 {
		t.Fatalf("hold = %v, want 30s from the EARLIEST stop; the ledger is observation order, not time order", got)
	}
}

// The window a device observation must cover is the HOLD, not the run: whether the card the borrower held did
// work while its owner waited for it.
func TestTheDeviceWindowIsTheHold(t *testing.T) {
	ev := []LifecycleEvent{
		held(VictimRow, EventPodReady, 2_000_000_000),
		held(OwnerRow, EventAdmitted, 24_000_000_000),
		held(VictimRow, EventAttemptStopped, 54_000_000_000),
		held(OwnerRow, EventPodReady, 56_000_000_000),
	}
	from, to, ok := DeviceHoldWindow(ev)
	if !ok {
		t.Fatal("a ledger carrying both endpoints produced no window")
	}
	if from != 24_000_000_000 || to != 54_000_000_000 {
		t.Fatalf("window = %d..%d, want the owner's admission to the victim's stop", from, to)
	}
	if _, _, ok := DeviceHoldWindow(ev[:1]); ok {
		t.Fatal("a ledger with no admission produced a window")
	}
}

// The attempt a device sample is attributed to is the one whose stop ENDS the hold. A re-executed row has
// several, and the later ones ran after the owner already had its capacity back — evidence about those is
// evidence about a Pod nobody was waiting for.
//
// Mutation that turns this red: take the latest stop, or the first one folded.
func TestTheAttributedAttemptIsTheOneThatEndedTheHold(t *testing.T) {
	first := held(VictimRow, EventAttemptStopped, 54_000_000_000)
	first.ObjectUID = "held-the-device"
	later := held(VictimRow, EventAttemptStopped, 120_000_000_000)
	later.ObjectUID = "ran-after-the-owner-was-back"
	// Folded in the wrong order, as a reordered watch would deliver them.
	if got := VictimAttemptUID([]LifecycleEvent{later, first}); got != "held-the-device" {
		t.Fatalf("attributed to %q; a device sample would then be evidence about a Pod the owner was never "+
			"waiting for", got)
	}
	if got := VictimAttemptUID(nil); got != "" {
		t.Fatalf("an empty ledger named the attempt %q", got)
	}
}

// Both readings of the hold must be built from the SAME pair of events.
//
// A re-executed row has several stops. If the arrival figure takes the earliest and the stamp figure takes
// the latest, their disagreement measures which events were picked rather than how the clocks differ — and
// the gate built on that disagreement would fire on healthy runs and stay silent on a real clock step.
//
// Mutation that turns this red: take the latest arrival in firstStamp.
func TestBothReadingsComeFromTheSameEvents(t *testing.T) {
	stampAt := func(e LifecycleEvent, ns int64) LifecycleEvent {
		v := ns
		e.ComponentStampUnixNanos = &v
		return e
	}
	const base = int64(1_700_000_000_000_000_000)
	ev := []LifecycleEvent{
		stampAt(held(OwnerRow, EventAdmitted, 24_000_000_000), base),
		// The attempt that ended the hold: earliest arrival, and its stamp is 30 s after the admission.
		stampAt(held(VictimRow, EventAttemptStopped, 54_000_000_000), base+30_000_000_000),
		// A later attempt, which ran after the owner already had its capacity back.
		stampAt(held(VictimRow, EventAttemptStopped, 120_000_000_000), base+96_000_000_000),
	}
	arrival := DeviceHoldNs(ev)
	stamp := DeviceHoldStampNs(ev)
	if arrival == nil || stamp == nil {
		t.Fatal("a ledger carrying both endpoints produced no reading")
	}
	if *arrival != 30_000_000_000 {
		t.Fatalf("arrival hold = %d, want the earliest stop's 30s", *arrival)
	}
	if *stamp != 30_000_000_000 {
		t.Fatalf("stamp hold = %d ns, want 30s from the SAME attempt; a later attempt's stamp makes the two "+
			"readings describe different intervals, and the gate then measures which events were picked",
			*stamp)
	}
	// And they therefore agree, which is what a healthy run must look like to the gate.
	if bad, gap, tol := ClocksDisagree(ev, int64(1500*time.Millisecond)); bad {
		t.Fatalf("a healthy run was reported as disagreeing by %s against %s", time.Duration(gap), time.Duration(tol))
	}
}
