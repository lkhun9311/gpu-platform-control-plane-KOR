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
	"strings"
	"testing"
	"time"
)

// observationFor builds an observation where the victim computes for its attempt and then, from holdAt,
// sits allocated and idle while it is terminated.
//
// That is the shape of the arm that HONOURS SIGTERM: the hold is the tail of the attempt during which the
// Pod is shutting down, so the card it still holds reads zero.
func observationFor(holdAt, endAt int64, idleDuringHold bool) *DeviceObservation {
	o := &DeviceObservation{
		Observer:         ObserverDCGM,
		ObserverIdentity: "nvcr.io/nvidia/k8s/dcgm-exporter@sha256:abc",
		Declared:         true,
		StartedNs:        0,
		EndedNs:          endAt + int64(time.Second),
	}
	for at := int64(0); at <= endAt; at += int64(time.Second) {
		util := 96
		if at >= holdAt && idleDuringHold {
			util = 0
		}
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-1234", PodUID: "victim-uid", UtilisationPercent: util,
		})
	}
	return o
}

// The fix, stated as the thing that used to fail.
//
// The busyness clause now reads the victim's ATTEMPT and the exclusivity, coverage and continuity clauses
// still read the HOLD. Before the split every clause read the hold, and in this arm the hold is ~2 s of a
// Pod that has already stopped computing -- so the gate refused the honouring arm for behaving as the arm
// is defined to behave, while the ignoring arm computed through its ~31 s hold and passed. -require-device
// removed the short arm, kept the long one, and left no contrast to form.
func TestTheHonouringArmEstablishesDeviceWorkFromItsAttemptRatherThanItsHold(t *testing.T) {
	const podReady = int64(0)
	const holdFrom = 20_000_000_000
	const stopped = holdFrom + 2_177_000_000 // the 2.177 s hold this lab recorded

	o := observationFor(holdFrom, stopped, true)

	ok, why := EstablishesDeviceWork(o, DeviceClaim{
		PodUID:     "victim-uid",
		WorkFromNs: podReady, WorkToNs: stopped,
		HoldFromNs: holdFrom, HoldToNs: stopped,
	})
	if !ok {
		t.Fatalf("the honouring arm was refused: %v", why)
	}

	// Asking the old question of the same observation still refuses, which is what makes the split the fix
	// rather than a relabelling: the behaviour changed because the QUESTION changed.
	if okOld, whyOld := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", holdFrom, stopped)); okOld {
		t.Error("collapsing the two intervals accepted this arm too, so this test no longer demonstrates " +
			"anything about the split")
	} else if !strings.Contains(whyOld, "allocated and idle") {
		t.Errorf("the collapsed question refused for an unexpected reason: %v", whyOld)
	}
}

// The clause still bites: a Pod that never made the card busy AT ALL is still refused.
//
// Widening the busyness window must not have widened it into vacuity, and this is the case that says so --
// the same shape as above with the card idle for the whole attempt rather than only during the hold.
func TestAPodThatNeverUsedTheCardIsStillRefused(t *testing.T) {
	const holdFrom = 20_000_000_000
	const stopped = holdFrom + 2_177_000_000

	o := &DeviceObservation{
		Observer: ObserverDCGM, ObserverIdentity: "dcgm@sha256:abc", Declared: true,
		StartedNs: 0, EndedNs: stopped + int64(time.Second),
	}
	for at := int64(0); at <= stopped; at += int64(time.Second) {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-1234", PodUID: "victim-uid", UtilisationPercent: 0,
		})
	}

	ok, why := EstablishesDeviceWork(o, DeviceClaim{
		PodUID:     "victim-uid",
		WorkFromNs: 0, WorkToNs: stopped,
		HoldFromNs: holdFrom, HoldToNs: stopped,
	})
	if ok {
		t.Fatal("a card that was idle for the whole attempt established device work; the busyness clause has " +
			"been widened into vacuity")
	}
	if !strings.Contains(why, "allocated and idle") {
		t.Errorf("refused for an unexpected reason: %v", why)
	}
}

// The hold's own clauses did not move, and this is the one that would be quietest if they had.
//
// Exclusivity is about the hold: another Pod's label on the same device WHILE the owner waited makes the
// utilisation unattributable. If the split had moved this clause to the attempt as well, a second tenant
// during the hold would stop being fatal.
func TestExclusivityStillReadsTheHoldAfterTheSplit(t *testing.T) {
	const holdFrom = 20_000_000_000
	const stopped = holdFrom + 2_177_000_000

	o := observationFor(holdFrom, stopped, false)
	// A neighbour appears on the same card, inside the hold.
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: holdFrom + int64(time.Second), DeviceUUID: "GPU-1234",
		PodRef: "other/neighbour", PodUID: "neighbour-uid", UtilisationPercent: 40,
	})

	ok, why := EstablishesDeviceWork(o, DeviceClaim{
		PodUID:     "victim-uid",
		WorkFromNs: 0, WorkToNs: stopped,
		HoldFromNs: holdFrom, HoldToNs: stopped,
	})
	if ok {
		t.Fatal("a second Pod on the same device during the hold no longer refuses; the exclusivity clause " +
			"has followed the busyness clause onto the attempt, where it does not belong")
	}
	if !strings.Contains(why, "as well as") {
		t.Errorf("refused for an unexpected reason: %v", why)
	}
}

// A ledger with a hold but no attempt cannot support the use half, and must not be read as an idle card.
func TestAMissingAttemptIntervalIsSaidRatherThanReadAsIdle(t *testing.T) {
	const holdFrom = 20_000_000_000
	const stopped = holdFrom + 2_177_000_000
	o := observationFor(holdFrom, stopped, false)

	ok, why := EstablishesDeviceWork(o, DeviceClaim{
		PodUID:     "victim-uid",
		WorkFromNs: 0, WorkToNs: 0, // VictimWorkWindow found nothing
		HoldFromNs: holdFrom, HoldToNs: stopped,
	})
	if ok {
		t.Fatal("a claim with no attempt interval established device work")
	}
	if !strings.Contains(why, "attempt's own interval is empty") {
		t.Errorf("a missing attempt was reported as something else, which sends a reader to the workload "+
			"instead of to the ledger: %v", why)
	}
}

// Exclusivity must not widen with the busyness clause, and this is the case that says which way it errs.
//
// A neighbour on the same card EARLY in the victim's attempt -- long before the owner was admitted, and
// outside the stale-label margin -- is serial reallocation, not ambiguity: by the time the hold opens the
// card is the victim's alone, which is what the claim is about. Scanning exclusivity across the whole
// attempt would refuse this run, so the mutation that makes the gate stricter is caught here rather than by
// the test that only proves it still refuses a genuine overlap.
func TestANeighbourBeforeTheHoldDoesNotRefuseTheRun(t *testing.T) {
	const holdFrom = 20_000_000_000
	const stopped = holdFrom + 2_177_000_000

	o := observationFor(holdFrom, stopped, true)
	// Well inside the attempt, well before the hold and its margin.
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 2_000_000_000, DeviceUUID: "GPU-1234",
		PodRef: "other/earlier-tenant", PodUID: "earlier-uid", UtilisationPercent: 55,
	})

	ok, why := EstablishesDeviceWork(o, DeviceClaim{
		PodUID:     "victim-uid",
		WorkFromNs: 0, WorkToNs: stopped,
		HoldFromNs: holdFrom, HoldToNs: stopped,
	})
	if !ok {
		t.Fatalf("a tenant that was gone long before the hold refused the run, so exclusivity is being "+
			"scanned across the attempt rather than the hold: %v", why)
	}
}

// Busyness must be about the card the hold was about, not any card the Pod ever touched.
//
// A Pod whose held card sat idle, while a DIFFERENT device carried its label and was busy, has not shown
// that the held card did work -- and that is exactly what a stale label or a second allocation elsewhere
// looks like. Counting it would let the idle card be certified by activity on another one.
func TestBusynessOnAnotherCardDoesNotEstablishTheHeldOne(t *testing.T) {
	const holdFrom = 20_000_000_000
	const stopped = holdFrom + 2_177_000_000

	o := &DeviceObservation{
		Observer: ObserverDCGM, ObserverIdentity: "dcgm@sha256:abc", Declared: true,
		StartedNs: 0, EndedNs: stopped + int64(time.Second),
	}
	// The held card: present across the hold so it is the device the claim is about, and idle throughout.
	for at := int64(0); at <= stopped; at += int64(time.Second) {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-HELD", PodUID: "victim-uid", UtilisationPercent: 0,
		})
	}
	// Another card carrying the same Pod's label, busy, entirely before the hold so it never joins the
	// hold's device set.
	for at := int64(1_000_000_000); at < holdFrom; at += int64(time.Second) {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-ELSEWHERE", PodUID: "victim-uid", UtilisationPercent: 91,
		})
	}

	ok, why := EstablishesDeviceWork(o, DeviceClaim{
		PodUID:     "victim-uid",
		WorkFromNs: 0, WorkToNs: stopped,
		HoldFromNs: holdFrom, HoldToNs: stopped,
	})
	if ok {
		t.Fatal("activity on a different card established that the HELD card did work; the busyness clause " +
			"is no longer restricted to the device the hold identified")
	}
	if !strings.Contains(why, "allocated and idle") {
		t.Errorf("refused for an unexpected reason: %v", why)
	}
}
