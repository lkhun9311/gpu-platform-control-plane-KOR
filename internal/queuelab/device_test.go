package queuelab

import (
	"strings"
	"testing"
	"time"
)

// goodObservation is an observer that watched one Pod's card across an interval and saw it working.
func goodObservation() *DeviceObservation {
	o := &DeviceObservation{
		Observer:         ObserverDCGM,
		ObserverIdentity: "nvcr.io/nvidia/k8s/dcgm-exporter@sha256:abc", Declared: true,
		StartedNs: 0,
		EndedNs:   40_000_000_000,
	}
	for at := int64(0); at <= 40_000_000_000; at += int64(time.Second) {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-1234", PodUID: "victim-uid", UtilisationPercent: 97,
		})
	}
	return o
}

// The ordinary case, so every refusal below is a refusal of something and not of everything.
func TestAWatchedBusyDeviceEstablishesWork(t *testing.T) {
	ok, why := EstablishesDeviceWork(goodObservation(), SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if !ok {
		t.Fatalf("a card sampled every second at 97%% across the interval did not establish work: %s", why)
	}
	if why != "" {
		t.Fatalf("an established claim carried a reason: %s", why)
	}
}

// The rule the whole file follows: the workload is not a witness.
func TestTheWorkloadIsNotAWitness(t *testing.T) {
	o := goodObservation()
	o.Observer = "workload-self-report"
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("a Pod's own report of its device use established that the device was used")
	}
	if !strings.Contains(why, "not a witness") {
		t.Fatalf("the refusal does not say why the source is inadmissible: %s", why)
	}
}

// No observer at all is the state every record this lab has produced is in, and it must say so rather than
// come back with a bare false.
func TestNoObserverSaysWhatTheRunDoesEstablish(t *testing.T) {
	ok, why := EstablishesDeviceWork(nil, SameWindowClaim("victim-uid", 0, 10))
	if ok {
		t.Fatal("a run with no observer established device work")
	}
	if !strings.Contains(why, "RESERVED") {
		t.Fatalf("the refusal does not say what the run DOES establish: %s", why)
	}
}

// An allocated card sitting idle is the exact state this axis exists to distinguish from a working one, and
// the one a GPU session would produce if the workload never made a driver call.
func TestAnIdleCardDoesNotEstablishWork(t *testing.T) {
	o := goodObservation()
	for i := range o.Samples {
		o.Samples[i].UtilisationPercent = 0
	}
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("a card that was allocated and idle established that it did work")
	}
	if !strings.Contains(why, "allocated and idle") {
		t.Fatalf("the refusal does not name the state: %s", why)
	}

	// One busy sample is a reading, not a state: a driver reports non-zero utilisation while another process
	// initialises, and that must not carry the claim.
	// Index 20 is t=20 s, INSIDE the 10..30 s interval. An earlier version of this test made a sample at
	// t=5 s busy, outside the window, so it was filtered before the count and the mutation that accepted a
	// single busy sample survived.
	o.Samples[20].UtilisationPercent = 60
	if ok, _ := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); ok {
		t.Fatal("a single non-zero sample established that the device did work")
	}
	// And two inside the window do carry it, or the threshold would be a refusal of everything.
	o.Samples[21].UtilisationPercent = 60
	if ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); !ok {
		t.Fatalf("two busy samples across the interval did not establish work: %s", why)
	}
}

// A gap long enough to hide a preemption is not continuous observation, however good the samples around it.
func TestAGapLongEnoughToHideAPreemptionRefuses(t *testing.T) {
	o := goodObservation()
	kept := o.Samples[:0]
	for _, s := range o.Samples {
		// Drop five seconds out of the middle of the interval.
		if s.AtNs >= 18_000_000_000 && s.AtNs <= 23_000_000_000 {
			continue
		}
		kept = append(kept, s)
	}
	o.Samples = kept
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("an observation blind for five seconds in the middle established continuous work")
	}
	if !strings.Contains(why, "hide an entire preemption") {
		t.Fatalf("the refusal does not say what the gap costs: %s", why)
	}
}

// An observer that was not running for the whole interval has not covered it, whatever it saw while it was.
func TestAnObserverThatStartedLateHasNotCoveredTheInterval(t *testing.T) {
	o := goodObservation()
	o.StartedNs = 15_000_000_000
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("an observer that started inside the interval covered it")
	}
	if !strings.Contains(why, "never watched") {
		t.Fatalf("the refusal does not name the uncovered part: %s", why)
	}
}

// Attribution is to the Pod UID, not the name, because the UID is what the API guarantees unique across time:
// a name is free for reuse the moment its Pod is deleted, and an observer labelling by name is read against
// whatever holds that name when the mapping is resolved.
func TestSamplesForAnotherPodDoNotCount(t *testing.T) {
	o := goodObservation()
	for i := range o.Samples {
		o.Samples[i].PodUID = "some-other-attempt-uid"
	}
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("another attempt's device activity established this one's")
	}
	if !strings.Contains(why, "reservation") {
		t.Fatalf("the refusal does not say what an unsampled hold is: %s", why)
	}
}

// "DCGM said so" is not provenance if nobody can say which DCGM, for the same reason the canary key carries
// the operator's image digest.
func TestAnObserverMustSayWhichBuildItWas(t *testing.T) {
	o := goodObservation()
	o.ObserverIdentity = ""
	if ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); ok {
		t.Fatalf("an anonymous observer established device work (%s)", why)
	}
}

// A sample that cannot name its card, or samples naming several, cannot say the card this Pod held did
// anything.
func TestTheDeviceMustBeIdentifiedAndSingular(t *testing.T) {
	anon := goodObservation()
	anon.Samples[3].DeviceUUID = ""
	if ok, why := EstablishesDeviceWork(anon, SameWindowClaim("victim-uid", 0, 30_000_000_000)); ok {
		t.Fatalf("a sample naming no device established work (%s)", why)
	}

	two := goodObservation()
	for i := range two.Samples {
		if i%2 == 0 {
			two.Samples[i].DeviceUUID = "GPU-5678"
		}
	}
	ok, why := EstablishesDeviceWork(two, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("samples naming two different cards established which one the hold was about")
	}
	if !strings.Contains(why, "which card") {
		t.Fatalf("the refusal does not name the ambiguity: %s", why)
	}
}

// A URL is not an identity. It says where the bytes came from, not what produced them, and any HTTP server
// can be at a URL — which is exactly the attack a fake exporter demonstrated on this very code.
func TestAnEndpointIsNotAnIdentity(t *testing.T) {
	o := goodObservation()
	o.Endpoint = "http://127.0.0.1:9400/metrics"
	o.ObserverIdentity = o.Endpoint
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("an observation whose provenance was its own URL established device work")
	}
	if !strings.Contains(why, "where the bytes came from") {
		t.Fatalf("the refusal does not say what a URL is not: %s", why)
	}
}

// The observer's kind is DECLARED by whoever configured the run, and the record has to carry that admission.
// Nothing here can tell a DCGM exporter from a text file served over HTTP; a record that omitted the
// admission would read as though something had checked.
func TestAnObservationMustAdmitItsSourceWasDeclared(t *testing.T) {
	o := goodObservation()
	o.Declared = false
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("an observation that did not record its source as declared established device work")
	}
	if !strings.Contains(why, "DECLARED rather") {
		t.Fatalf("the refusal does not name the missing admission: %s", why)
	}
}

// Two busy rows from ONE instant are a reading, not a state. DCGM emits duplicated device-wide series, and
// MIG and label drift make that ordinary — so the count has to be over distinct times.
//
// Mutation that turns this red: count busy rows instead of distinct busy instants.
func TestTwoBusyRowsFromOneInstantAreNotAState(t *testing.T) {
	o := &DeviceObservation{
		Observer: ObserverDCGM, ObserverIdentity: "dcgm@sha256:abc", Declared: true,
		StartedNs: 0, EndedNs: 40_000_000_000,
	}
	// Idle samples every second so coverage holds, and two BUSY rows sharing a single timestamp.
	for at := int64(0); at <= 40_000_000_000; at += int64(time.Second) {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-1234", PodUID: "victim-uid", UtilisationPercent: 0,
		})
	}
	for range 2 {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: 20_000_000_000, DeviceUUID: "GPU-1234", PodUID: "victim-uid", UtilisationPercent: 88,
		})
	}
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("two busy rows from one scrape established that the card was working")
	}
	if !strings.Contains(why, "ONE instant") {
		t.Fatalf("the refusal does not say why one instant is not enough: %s", why)
	}

	// Spread across two instants, and it is a state.
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 25_000_000_000, DeviceUUID: "GPU-1234", PodUID: "victim-uid", UtilisationPercent: 88,
	})
	if ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); !ok {
		t.Fatalf("busy at two distinct instants did not establish work: %s", why)
	}
}

// The clause that turns a utilisation reading into an attribution.
//
// DCGM_FI_DEV_GPU_UTIL is DEVICE utilisation; the Kubernetes labels beside it say what the device is
// ALLOCATED to, not which process made it busy. Under time-slicing, MPS, a MIG parent, or a stale label at
// the exit transition, a positive reading under this Pod's name can be somebody else's work entirely.
//
// So the premise is required rather than the conclusion inferred: every entity this Pod was seen on must
// have carried no other Pod's label anywhere in the observation.
//
// Mutation that turns this red: skip the exclusivity scan, or restrict it to samples inside the interval.
func TestASharedDeviceCannotAttributeWorkToEither(t *testing.T) {
	o := goodObservation()
	// Another tenant on the same card, mid-interval: time-slicing, MPS, or a MIG parent reported whole.
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 20_000_000_000, DeviceUUID: "GPU-1234",
		PodRef: "other-tenant/greedy-abc", PodUID: "other-uid", UtilisationPercent: 71,
	})
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("a device carrying two Pods' labels attributed its utilisation to one of them")
	}
	if !strings.Contains(why, "not attributable to either") {
		t.Fatalf("the refusal does not say the reading belongs to nobody: %s", why)
	}
}

// Superseded by TestALabelInTheStaleMarginStillDefeatsAttribution.
//
// This asserted that a label ANY time before the window defeated attribution, which was the whole-observation
// scope -- and that scope refused the protocol's own handover in every arm. The replacement tests both sides
// of the derived margin instead: inside it a stale label still convicts, outside it a tenant that gave the
// card up long ago does not.

// A Pod the run never saw is the same problem wearing no identity, and it is why the parser keeps
// unattributable samples instead of dropping them.
func TestAnUnresolvablePodOnTheSameDeviceAlsoDefeatsAttribution(t *testing.T) {
	o := goodObservation()
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 22_000_000_000, DeviceUUID: "GPU-1234",
		PodRef: "somewhere-else/unknown-pod", UtilisationPercent: 50,
	})
	ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000))
	if ok {
		t.Fatal("a device shared with a Pod the run could not identify still attributed work")
	}
	if !strings.Contains(why, "somewhere-else/unknown-pod") {
		t.Fatalf("the refusal does not name the other tenant: %s", why)
	}
}

// And exclusivity on a DIFFERENT device is not this Pod's problem: a busy neighbour card says nothing about
// the one the victim held, or the check would refuse every multi-GPU node.
func TestAnotherPodOnAnotherDeviceIsNotAConflict(t *testing.T) {
	o := goodObservation()
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 20_000_000_000, DeviceUUID: "GPU-9999",
		PodRef: "other-tenant/neighbour-abc", PodUID: "neighbour-uid", UtilisationPercent: 99,
	})
	if ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); !ok {
		t.Fatalf("a busy neighbour on a different card defeated attribution: %s", why)
	}
}

// The experiment's DEFINING event is a handover: the borrower releases the card and the owner takes it, on a
// node with exactly the two devices the protocol needs. The owner's label therefore lands on the victim's
// entity a few seconds after the hold ends, in every arm.
//
// The exclusivity clause used to scan the whole observation and refused exactly that — so every run would
// have been denied device attribution, deterministically, by the gate meant to license it. Serial
// reallocation after the window is not ambiguity; it is the thing being measured.
//
// Mutation that turns this red: widen the scan back to the whole observation.
func TestTheProtocolsOwnHandoverDoesNotDefeatAttribution(t *testing.T) {
	o := goodObservation()
	o.EndedNs = 120_000_000_000
	// The owner picks up the freed card once the hold ends at 30 s.
	for at := int64(35_000_000_000); at <= 120_000_000_000; at += int64(time.Second) {
		o.Samples = append(o.Samples, DeviceSample{
			AtNs: at, DeviceUUID: "GPU-1234", PodRef: "lab/b1-owner", PodUID: "owner-uid",
			UtilisationPercent: 91,
		})
	}
	if ok, why := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); !ok {
		t.Fatalf("the protocol's own handover defeated attribution: %s", why)
	}
}

// A label inside the window is still a conflict — that is time-slicing, MPS or a MIG parent, and it is what
// the clause is for.
func TestALabelInsideTheWindowStillDefeatsAttribution(t *testing.T) {
	o := goodObservation()
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 20_000_000_000, DeviceUUID: "GPU-1234", PodRef: "lab/greedy", PodUID: "greedy-uid",
		UtilisationPercent: 71,
	})
	if ok, _ := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); ok {
		t.Fatal("two tenants on one entity during the hold attributed its utilisation to one of them")
	}
}

// And so is one in the stale-label margin just before it: when a container exits its name can persist on the
// entity for a scrape or two while the kubelet's pod-resources view catches up, and that carries the previous
// tenant's activity into the start of the window.
func TestALabelInTheStaleMarginStillDefeatsAttribution(t *testing.T) {
	o := goodObservation()
	o.Samples = append(o.Samples, DeviceSample{
		AtNs: 9_000_000_000, DeviceUUID: "GPU-1234", PodRef: "lab/previous", PodUID: "previous-uid",
		UtilisationPercent: 64,
	})
	if ok, _ := EstablishesDeviceWork(o, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); ok {
		t.Fatal("a label one second before the window left attribution intact")
	}
	// Well before it is the same handover case seen from the other side: a tenant that had the card long ago
	// and gave it up is not sharing it.
	far := goodObservation()
	far.StartedNs = 0
	far.Samples = append(far.Samples, DeviceSample{
		AtNs: 1_000_000_000, DeviceUUID: "GPU-1234", PodRef: "lab/long-gone", PodUID: "long-gone-uid",
		UtilisationPercent: 64,
	})
	if ok, why := EstablishesDeviceWork(far, SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); !ok {
		t.Fatalf("a label nine seconds before the window defeated attribution: %s", why)
	}
}
