package queuelab

import (
	"testing"
	"time"
)

// skewed builds an event carrying an observed skew, which is the only field these tests read.
func skewed(ns int64) LifecycleEvent {
	v := ns
	return LifecycleEvent{ObservedSkewNs: &v}
}

// A uniformly late harness measures intervals exactly, so the floor must come from the spread and never from
// the median. This is the test that fails if someone "simplifies" FloorNs to MedianNs, which is the number a
// reader reaches for first and the wrong one.
func TestSpreadOfFloorIgnoresHowLateTheHarnessIs(t *testing.T) {
	late := SpreadOf([]LifecycleEvent{skewed(9 * int64(time.Second)), skewed(9 * int64(time.Second))})
	if late == nil {
		t.Fatal("two samples must bound something")
	}
	if late.MedianNs != 9*int64(time.Second) {
		t.Fatalf("median = %d, want 9s", late.MedianNs)
	}
	// Zero observed spread still leaves a second of truncation unaccounted for on each stamp, so the floor is
	// the quantisation and not nothing.
	if late.FloorNs != int64(time.Second) {
		t.Fatalf("floor = %d, want 1s: a constant lag cancels in a difference, truncation does not", late.FloorNs)
	}
}

// The quantisation ADDS to the spread rather than losing a max() against it, because truncation of the
// kubelet's stamp can compress the observed spread as easily as widen it.
//
// This is the regression guard for a real defect. FloorNs was max(spread, quantisation) and returned 1.959 s
// here, understating by a full second, and the rule was wrong in every configuration where the spread is
// nonzero.
func TestSpreadOfFloorAddsTheQuantisationToTheSpread(t *testing.T) {
	s := SpreadOf([]LifecycleEvent{
		skewed(430 * int64(time.Millisecond)),
		skewed(1200 * int64(time.Millisecond)),
		skewed(2389 * int64(time.Millisecond)),
	})
	want := 2959 * int64(time.Millisecond) // (2389 - 430) + 1000
	if s.FloorNs != want {
		t.Fatalf("floor = %d, want %d ((2389ms - 430ms) + 1000ms of truncation)", s.FloorNs, want)
	}
}

// The counter-example that condemned the old rule, as a test.
//
// True lags of 0 and 1.9 s, observed through fractional truncations of 0.95 and 0.05, arrive as skews of 0.95
// and 1.95 — an observed spread of exactly 1.0 s. The old max(spread, quantisation) returned 1.0 s and would
// have declared a 1.5 s effect resolved while it sat inside 1.9 s of real instrument error. The corrected
// rule returns 2.0 s and refuses it.
func TestSpreadOfSurvivesTruncationCompressingTheSpread(t *testing.T) {
	s := SpreadOf([]LifecycleEvent{
		skewed(950 * int64(time.Millisecond)),
		skewed(1950 * int64(time.Millisecond)),
	})
	if s.FloorNs != 2*int64(time.Second) {
		t.Fatalf("floor = %d, want 2s", s.FloorNs)
	}
	if s.Resolves(1500 * int64(time.Millisecond)) {
		t.Fatal("a 1.5s effect resolved against lags that can differ by 1.9s; truncation compressed the " +
			"observed spread and the old rule believed it")
	}
}

// One reading has a spread of zero, and zero would be the strongest claim the struct can make. Refusing is
// the point: a single observation has nothing to be spread against.
func TestSpreadOfRefusesASingleSample(t *testing.T) {
	if s := SpreadOf([]LifecycleEvent{skewed(50)}); s != nil {
		t.Fatalf("one sample bounded a spread: %+v", s)
	}
	if s := SpreadOf(nil); s != nil {
		t.Fatalf("no samples bounded a spread: %+v", s)
	}
}

// The published mistake, as a test: a sub-second residual against the run's own 0.4-2.4s spread is not a
// measurement, and Resolves is what has to say so.
func TestResolvesRejectsTheResidualThisLabOncePublished(t *testing.T) {
	s := SpreadOf([]LifecycleEvent{
		skewed(430 * int64(time.Millisecond)),
		skewed(2389 * int64(time.Millisecond)),
	})
	if s.Resolves(2900 * int64(time.Millisecond)) {
		t.Fatal("a difference inside the corrected floor resolved")
	}
	if s.Resolves(940 * int64(time.Millisecond)) {
		t.Fatal("0.94s was reported as the control plane's cost against this spread; it must not resolve")
	}
	// The categorical difference the lab does support: 41 GPU-seconds against 0.
	if !s.Resolves(41 * int64(time.Second)) {
		t.Fatal("an order-of-magnitude difference must survive the floor, or the gate is useless")
	}
}

// Sign must not decide the answer: a residual is as unresolved when the observation runs early as when it
// runs late.
func TestResolvesIsSymmetricAboutZero(t *testing.T) {
	s := SpreadOf([]LifecycleEvent{skewed(0), skewed(3 * int64(time.Second))}) // floor 4s
	if s.Resolves(-3 * int64(time.Second)) {
		t.Fatal("a negative residual inside the floor resolved")
	}
	if !s.Resolves(-5 * int64(time.Second)) {
		t.Fatal("a negative residual beyond the floor did not resolve")
	}
}

// A nil spread means no bound was established, and the safe reading of no bound is that nothing resolves.
func TestNilSpreadResolvesNothing(t *testing.T) {
	var s *ObservationSpread
	if s.Resolves(int64(time.Hour)) {
		t.Fatal("an unbounded run resolved an hour; absent evidence must not read as good evidence")
	}
}
