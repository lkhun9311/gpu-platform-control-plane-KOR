package queuelab

import (
	"slices"
	"time"
)

// stampQuantisationNs is the granularity of the kubelet's own timestamp.
//
// metav1.Time serialises to RFC3339 with SECOND precision, so finishedAt arrives with its nanoseconds
// zeroed. This was checked rather than assumed: every observed value in the recorded runs ends in nine
// zeros. It is a constant here rather than a literal at the point of use because it is a property of the
// apiserver's wire format, not a tuning knob, and a reader who changes it is changing what the harness
// believes about Kubernetes.
const stampQuantisationNs = int64(time.Second)

// minimumSpreadSamples is how many observations it takes before a spread bounds anything.
//
// One sample has a spread of zero, and zero is the most dangerous possible answer: it would let a run
// declare that its intervals are resolved to the quantisation floor on the strength of a single reading
// that had nothing to be spread against.
const minimumSpreadSamples = 2

// ObservationSpread is how finely the intervals reconstructed from a run's events can be read.
//
// Every interval this package reports is a difference of ARRIVAL times — when a watch event reached the
// collector, not when the thing it describes happened. So an interval's error is the DIFFERENCE between its
// two endpoints' lags, and NOT their size. A harness uniformly two seconds behind the cluster measures every
// interval exactly. That is the whole reason FloorNs is built from the spread rather than from MedianNs,
// and it is worth stating because the median is the number a reader reaches for first.
type ObservationSpread struct {
	// Samples is how many events carried their component's own timestamp to compare against.
	//
	// It used to be stop events alone, and the narrowness was the defect rather than the wording: a bound
	// sampled only where containers STOP was being applied to an interval running from a Kueue admission to
	// a kubelet readiness, neither of which is a stop. Admissions and readiness transitions now carry their
	// components' stamps too, so this counts every endpoint kind the intervals actually use.
	Samples int
	// MinNs, MedianNs and MaxNs describe the observed skew. They are a BOUND, not a delivery time: the two
	// stamps come from unsynchronised clocks on different machines and one of them is truncated, so the gap
	// mixes propagation, clock offset and truncation with nothing here able to separate them.
	MinNs    int64
	MedianNs int64
	MaxNs    int64
	// QuantisationNs is the granularity of the stamp the skews were computed against.
	QuantisationNs int64
	// FloorNs is the magnitude below which this run's intervals are not resolved.
	//
	// It is (MaxNs-MinNs) + QuantisationNs, and the ADDITION is the correction of a real error. This field
	// used to be max(spread, quantisation), on the argument that a spread coming out smaller than a second
	// could not be evidence of a tighter bound because the skews it came from each absorbed up to a second of
	// truncation. The argument had the right premise and drew the wrong conclusion.
	//
	// Write the observed skew as trueLag + frac, where frac lands in [0, 1s) because the kubelet's stamp is
	// truncated DOWNWARD. Then
	//
	//	observed spread  =  (lag_max + frac_max) - (lag_min + frac_min)
	//	                 =  true lag spread + (frac_max - frac_min)
	//
	// and that last term ranges over (-1s, +1s). Truncation can therefore COMPRESS the observed spread just
	// as easily as widen it, so the true lag spread is bounded by observed + 1s and not by either alone.
	// Worked: true lags {0, 1.9s} with fractions {0.95, 0.05} give observed skews {0.95, 1.95}, a spread of
	// exactly 1.0s — and the old rule returned 1.0s for an interval that can carry 1.9s of error, declaring a
	// 1.5s effect resolved when it sits inside the instrument. That is the same failure the whole file exists
	// to prevent, in the line that was supposed to prevent it.
	//
	// A difference smaller than this is not a small effect. It is an effect this harness cannot see, and no
	// number of repetitions changes that — a resolution limit is not a noise level.
	//
	// It bounds ONE interval. A difference between two separately measured quantities carries both of their
	// errors, and they can be opposite-signed, so a caller comparing two groups must ADD their floors rather
	// than take the larger. cmd/queuelabrun's comparison does; nothing here can enforce it for a caller that
	// does not.
	FloorNs int64
}

// SpreadOf summarises the skew the events carried, or nil when they cannot bound anything.
//
// nil is the honest answer rather than a zero-valued struct, because "no event reported its component's
// timestamp" and "this harness is perfectly prompt" are opposite states and only one of them is good news. A
// caller holding nil knows it has no bound; a caller holding zeros would believe it had the best possible one.
//
// It pools every endpoint kind rather than reporting per-kind spreads, which is a simplification worth
// naming: the Workload watch and the Pod watch are separate streams with separate lag processes, and this
// treats their extremes as one population. That is conservative for a bound — the pooled spread is at least
// as wide as either — but it is not a per-endpoint characterisation, and a caller wanting the lag of one
// specific stream will not find it here.
func SpreadOf(events []LifecycleEvent) *ObservationSpread {
	return SpreadOfMatching(events, func(*LifecycleEvent) bool { return true })
}

// SpreadOfMatching is SpreadOf over a chosen subset of the events.
//
// It exists because pooling every endpoint kind into one population over-charges every interval by the worst
// behaviour of an endpoint it does not contain. In all twelve recorded runs the widest skew belongs to the
// OWNER'S OWN COMPLETION -- an endpoint of no published interval -- and it sets the floor for all of them.
// Restricting to the endpoints an interval actually uses roughly halves it: 1.20-1.96 s against a pooled
// 2.20-3.22 s, and for the two Workload-watch endpoints of the device hold the observed run-to-run scatter is
// under twenty MILLISECONDS.
//
// A caller that passes a filter is asserting which events its interval is built from, so the filter and the
// interval must be edited together. That coupling is the cost of the accuracy, and it is why this is a
// parameter rather than a heuristic that guesses from the numbers.
func SpreadOfMatching(events []LifecycleEvent, keep func(*LifecycleEvent) bool) *ObservationSpread {
	skews := make([]int64, 0, len(events))
	for i := range events {
		e := &events[i]
		if e.ObservedSkewNs != nil && keep(e) {
			skews = append(skews, *e.ObservedSkewNs)
		}
	}
	if len(skews) < minimumSpreadSamples {
		return nil
	}
	slices.Sort(skews)
	spread := skews[len(skews)-1] - skews[0]
	floor := spread + stampQuantisationNs
	return &ObservationSpread{
		Samples:        len(skews),
		MinNs:          skews[0],
		MedianNs:       skews[len(skews)/2],
		MaxNs:          skews[len(skews)-1],
		QuantisationNs: stampQuantisationNs,
		FloorNs:        floor,
	}
}

// Resolves says whether a magnitude is large enough for this run to have seen it.
//
// It is a method rather than a comparison at each call site because the comparison has a direction that is
// easy to invert, and inverting it turns "the harness cannot see this" into "the harness measured this".
// That inversion is not hypothetical, and it has happened twice. A residual of 0.94 seconds was published
// from this lab as the control plane's own cost while the run's spread ran into the seconds. And the model
// check printed its honouring-arm control -- about 41 ms, against a floor of about a second -- as evidence
// that the measured interval contained no platform work, which is this function's exact subject.
//
// The second time, this method could not have prevented it: nothing outside tests calls it. A rule that
// lives in a helper nobody invokes is a rule the code does not have.
func (s *ObservationSpread) Resolves(magnitudeNs int64) bool {
	if s == nil {
		return false
	}
	return ResolvesAgainst(magnitudeNs, s.FloorNs)
}

// ResolvesAgainst is the comparison itself, for a caller holding a floor this package did not derive.
//
// It exists because the method above was not enough. Every consumer that actually needed the rule had a
// floor of its own -- a pooled one, or the sum of two arms' -- so none of them could call a method on a
// spread, and each reimplemented `magnitude > floor` inline. That is how a rule with a helper and a comment
// ends up not being applied to the one figure it most mattered for.
//
// The direction is the whole point and it is easy to invert: a magnitude LARGER than the floor is resolved.
// Anything at or below it is a quantity this harness cannot see, which is not the same as a quantity it
// measured to be small. An absent bound resolves nothing, which is why callers pair this with their own
// `bounded`.
//
// The sign is taken off the magnitude because a difference may be computed in either order and its
// resolvability does not depend on which.
func ResolvesAgainst(magnitudeNs, floorNs int64) bool {
	if magnitudeNs < 0 {
		magnitudeNs = -magnitudeNs
	}
	return magnitudeNs > floorNs
}

// SecondsToNs converts a floor or a magnitude a comparison layer holds in seconds.
//
// The comparison layer works in float seconds because that is what it publishes; the rule works in
// nanoseconds because that is what the ledger carries. This is the one conversion between them, so a caller
// cannot quietly compare a magnitude in one unit against a floor in the other.
func SecondsToNs(v float64) int64 { return int64(v * float64(time.Second)) }
