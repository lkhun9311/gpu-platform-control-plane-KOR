package queuelab

// The intervals this file derives are the experiment's own timing, read back out of a run's ledger rather
// than taken from the constants the protocol declared. They exist because the two disagree, and because the
// quantity the lab's central claim is ABOUT was never the one it published.
//
// held = min(remaining service, termination grace period) is a statement about how long a preempted borrower
// keeps its device after the platform has decided its owner should have it. What the lab measured instead was
// the owner's admission-to-running wait, which contains that hold and then the scheduling, the image, and the
// container start on top of it — so the model could only be tested by subtracting an estimate of those,
// borrowed from the other arm. That subtraction turned out to be wrong in kind rather than in size: the
// owner's Pod spends the grace window pending against the device, so restoration after release costs about
// 1.6 s LESS in the arm the model predicts than in the arm the estimate came from.
//
// The hold itself needs no such term, and it reproduces far more tightly: across the recorded runs its
// within-cell spread is 5 to 85 milliseconds against a floor of seconds. The wider end is the
// self-completing ignoring cell; an earlier version of this comment said "8 to 17", which was true of five
// cells out of six and of a record set that has since been replaced.
//
// The honouring arm is a zero-hold CONTROL rather than a source of a subtrahend, and its measured value --
// about 41 ms -- is far below this harness's own floor and therefore UNRESOLVED. It does not establish that
// the interval is quiet; what the two arms establish is that their difference is far larger than the floor.
// Reading the control as a measured near-zero is the inversion resolution.go exists to prevent, and it was
// published from here before a review caught it.

// DeviceHoldNs is how long the borrower kept the device after Kueue admitted its owner, or nil when the run's
// ledger cannot say.
//
// The endpoints are the owner's ADMISSION -- the moment the platform committed the capacity to the owner --
// and the victim's terminal phase, which is when the device is actually released. Deletion is deliberately
// not the endpoint: a deletionTimestamp only starts the grace period, during which the Pod goes on holding
// the device, and that window is the entire subject.
//
// Both endpoints are arrival times from THIS collector rather than component stamps, and the choice is
// deliberate. The two events come from different watches, so a component-stamp reading would carry the offset
// between Kueue's clock and the kubelet's, while the arrival reading carries only the differential delivery
// lag of two streams that the recorded runs show agreeing to within tens of milliseconds. The stamp version
// is available for corroboration; it is quantised to the second, which is coarser than this interval's own
// scatter.
func DeviceHoldNs(events []LifecycleEvent) *int64 {
	admitted := firstElapsed(events, OwnerRow, func(e *LifecycleEvent) bool { return e.Type == EventAdmitted })
	stopped := firstElapsed(events, VictimRow, func(e *LifecycleEvent) bool { return e.Type == EventAttemptStopped })
	if admitted == nil || stopped == nil {
		return nil
	}
	// A negative hold is not legal reordering to be shrugged at: it would say the borrower released the device
	// before the platform decided the owner should have it, which makes the interval a different quantity.
	// The two endpoints come from different watches, so a few milliseconds of reordering is possible, and the
	// caller is told nothing rather than a number whose sign contradicts its own definition.
	if *stopped < *admitted {
		return nil
	}
	v := *stopped - *admitted
	return &v
}

// AchievedDoseNs is the dose the run actually delivered: the victim's readiness to the owner's submission.
//
// It exists because the declared dose is a constant and the delivered one is not. The schedule gates the
// owner on the victim's observed Ready through a barrier satisfied by a two-second poll, so every recorded
// run overran its declared dose by 1.0 to 1.4 seconds, systematically and in the same direction. Nothing in
// the record said so, and the model's prediction is built from the declared value.
//
// On this cluster the margin is ten seconds and the overrun is harmless. On a slower one an overrun large
// enough to carry the victim's remaining service across the grace period would make a run measure the OTHER
// regime while carrying this regime's label -- and the label is what selects the prediction.
func AchievedDoseNs(events []LifecycleEvent) *int64 {
	ready := firstElapsed(events, VictimRow, func(e *LifecycleEvent) bool { return e.Type == EventPodReady })
	submitted := firstElapsed(events, OwnerRow, func(e *LifecycleEvent) bool { return e.Type == EventSubmitted })
	if ready == nil || submitted == nil || *submitted < *ready {
		return nil
	}
	v := *submitted - *ready
	return &v
}

// firstElapsed is the earliest arrival time of a matching event for a row, or nil when there is none.
//
// Earliest rather than first-folded, for the reason the reconstruction takes minima everywhere: the ledger is
// observation-insertion order and a re-executed row can deliver its second attempt before its first.
func firstElapsed(events []LifecycleEvent, job string, match func(*LifecycleEvent) bool) *int64 {
	var out *int64
	for i := range events {
		e := &events[i]
		if e.Job != job || !match(e) {
			continue
		}
		if out == nil || e.ElapsedNs < *out {
			v := e.ElapsedNs
			out = &v
		}
	}
	return out
}

// VictimAttemptUID is the Pod that held the device across the preemption, or "" when the ledger cannot say.
//
// It is the attempt whose terminal phase ENDS the hold, which is not the same as "the victim's Pod": a
// re-executed row has several, and the later ones ran after the owner already had its capacity back. A device
// observation attributed to one of those would be evidence about a Pod that was not holding anything the
// owner was waiting for.
func VictimAttemptUID(events []LifecycleEvent) string {
	var uid string
	var at int64
	for i := range events {
		e := &events[i]
		if e.Job != VictimRow || e.Type != EventAttemptStopped || e.ObjectUID == "" {
			continue
		}
		if uid == "" || e.ElapsedNs < at {
			uid, at = e.ObjectUID, e.ElapsedNs
		}
	}
	return uid
}

// DeviceHoldWindow is the interval a device observation has to cover, as offsets from the run's t0.
//
// It is the hold itself rather than the whole run, because that is the window the claim is about: whether
// the card the borrower was holding did work while its owner waited for it. An observation covering the run
// but not this interval has watched the wrong part.
func DeviceHoldWindow(events []LifecycleEvent) (fromNs, toNs int64, ok bool) {
	admitted := firstElapsed(events, OwnerRow, func(e *LifecycleEvent) bool { return e.Type == EventAdmitted })
	stopped := firstElapsed(events, VictimRow, func(e *LifecycleEvent) bool { return e.Type == EventAttemptStopped })
	if admitted == nil || stopped == nil || *stopped <= *admitted {
		return 0, 0, false
	}
	return *admitted, *stopped, true
}

// VictimWorkWindow is the victim attempt's own interval: its Pod becoming Ready to that Pod stopping.
//
// It exists because the device claim is really TWO questions with two intervals, and asking both over the
// hold made one of them unanswerable in one arm.
//
// "Was the card exclusively this Pod's while its owner waited" is about the HOLD. "Did this Pod use the
// card at all" is about the ATTEMPT -- and the hold is the tail of the attempt during which the victim is
// being terminated. In the arm that honours SIGTERM that tail is two seconds long and the victim has
// already stopped computing, so a busyness test over the hold refuses the arm for doing exactly what the
// arm is for. The ignoring arm keeps computing for its whole thirty-second hold and passes trivially, so
// the refusal is not even-handed: it removes the short arm and leaves the long one, and the contrast
// between them is the result.
//
// The work window contains the hold, so nothing here widens what the exclusivity clause scans.
func VictimWorkWindow(events []LifecycleEvent) (fromNs, toNs int64, ok bool) {
	ready := firstElapsed(events, VictimRow, func(e *LifecycleEvent) bool { return e.Type == EventPodReady })
	stopped := firstElapsed(events, VictimRow, func(e *LifecycleEvent) bool { return e.Type == EventAttemptStopped })
	if ready == nil || stopped == nil || *stopped <= *ready {
		return 0, 0, false
	}
	return *ready, *stopped, true
}

// DeviceHoldStampNs is the same hold read off the two components' own clocks, or nil when either published
// none.
//
// Kueue stamps its Admitted transition and the kubelet stamps the container's finish. Both are truncated to
// the second, so this figure lands on whole seconds and is coarser than the arrival reading -- and it carries
// no watch delivery lag at all, which is the half the arrival reading cannot avoid.
func DeviceHoldStampNs(events []LifecycleEvent) *int64 {
	admitted := firstStamp(events, OwnerRow, func(e *LifecycleEvent) bool { return e.Type == EventAdmitted })
	stopped := firstStamp(events, VictimRow, func(e *LifecycleEvent) bool { return e.Type == EventAttemptStopped })
	if admitted == nil || stopped == nil {
		return nil
	}
	v := *stopped - *admitted
	return &v
}

// firstStamp is the component stamp of the earliest matching event for a row, or nil when there is none.
//
// Earliest by ARRIVAL, so it names the same event firstElapsed does: the two readings of one interval have to
// come from one pair of events, or their disagreement measures which events were picked rather than how the
// clocks differ.
func firstStamp(events []LifecycleEvent, job string, match func(*LifecycleEvent) bool) *int64 {
	var at *int64
	var stamp *int64
	for i := range events {
		e := &events[i]
		if e.Job != job || !match(e) || e.ComponentStampUnixNanos == nil {
			continue
		}
		if at == nil || e.ElapsedNs < *at {
			v, s := e.ElapsedNs, *e.ComponentStampUnixNanos
			at, stamp = &v, &s
		}
	}
	return stamp
}

// ClocksDisagree reports whether a run's two readings of the device hold are further apart than their own
// bounds allow, and by how much.
//
// The check was promised in a comment beside the second reading and never written, which a review found: the
// stamp figure was recorded and consumed by nothing, so a clock step between Kueue's machine and a kubelet
// would have corrupted it silently, and watch pathology would have corrupted the arrival figure with the
// stamp fine. A second reading that nothing compares against the first is decoration.
//
// The tolerance is derived rather than chosen. Each stamp is truncated DOWNWARD to the second, so their
// difference carries up to a second of truncation; the arrival figure carries the differential delivery lag
// of two watches, which is what floorNs bounds. Anything inside that sum is the two instruments agreeing.
//
// What it does NOT do is sharpen anything. The truncation term swamps the lag in every recorded run -- the
// two readings at the hold's own endpoints differ by 117 to 1004 ms against a bound of seconds -- so this
// cannot calibrate the arrival figure, only catch it going wrong. The figure was "36 to 512" here and
// understated the top of the range by a factor of two.
func ClocksDisagree(events []LifecycleEvent, floorNs int64) (bool, int64, int64) {
	arrival := DeviceHoldNs(events)
	stamp := DeviceHoldStampNs(events)
	if arrival == nil || stamp == nil {
		return false, 0, 0
	}
	gap := *arrival - *stamp
	if gap < 0 {
		gap = -gap
	}
	tolerance := stampQuantisationNs + floorNs
	return gap > tolerance, gap, tolerance
}
