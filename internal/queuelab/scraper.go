package queuelab

import (
	"context"
	"fmt"
	"time"
)

// DeviceScraper turns a metrics endpoint into an observation covering a run.
//
// The transport is a function rather than a URL so the polling logic can be driven without a server, and so
// the caller owns how it authenticates -- the operator's own metrics endpoint is already scraped over TLS
// with a bearer token elsewhere in this repository, and a second half-copy of that would drift.
type DeviceScraper struct {
	// Observer and Identity are what the observation will claim about its own provenance, and both are
	// DECLARED by whoever configured the run. Nothing here can verify either: the payload is a format anyone
	// can emit and the endpoint is a URL anyone can serve. Identity is the exporter's image digest or
	// version, and it must not be the endpoint -- that says where the bytes came from, not what produced them.
	Observer DeviceObserver
	Identity string
	// Endpoint is recorded beside the identity so a reader can see both what was claimed and where it came
	// from.
	Endpoint string
	// Interval is how often to scrape. It must leave room under maxObserverGap, because a poll that lands
	// exactly on the limit turns one slow response into an uncovered window.
	Interval time.Duration
	// Fetch returns one scrape body.
	Fetch func(context.Context) ([]byte, error)
	// Resolve maps a namespace and Pod name to a UID. The observer labels by name; this is what says which
	// object that name referred to.
	Resolve PodResolver
	// Elapsed is the COLLECTOR's clock, so a sample's time is on the same scale as the ledger's events. A
	// scraper with its own clock would produce an observation whose coverage of an interval could not be
	// checked against the interval.
	Elapsed func() int64
}

// scrapeFailureLimit is how many consecutive failed scrapes end the observation.
//
// It stops rather than continuing quietly, because the alternative is an observation that keeps its Started
// and Ended bounds while seeing nothing in between -- which reads as coverage. The gap rule in
// EstablishesDeviceWork would catch it, but only as "a gap", and an operator would be left to guess whether
// the exporter was down or the card was idle. Ending on the failure keeps the two distinguishable.
const scrapeFailureLimit = 3

// Observe polls until the context ends, and returns what it saw.
//
// It returns the observation even on error, because a partial observation is evidence about the part it
// covers and the caller has EstablishesDeviceWork to decide whether that part is the one it needed. Throwing
// it away would leave a run unable to say whether the exporter was present at all.
func (s *DeviceScraper) Observe(ctx context.Context) (*DeviceObservation, int, error) {
	if s.Fetch == nil || s.Resolve == nil || s.Elapsed == nil {
		return nil, 0, fmt.Errorf("a device scraper needs a transport, a Pod resolver and the collector's clock")
	}
	if s.Interval <= 0 || s.Interval >= maxObserverGap {
		return nil, 0, fmt.Errorf("scrape interval %s must be positive and shorter than the %s a run may "+
			"blink through, or one slow response is an uncovered window", s.Interval, maxObserverGap)
	}
	obs := &DeviceObservation{
		Observer:         s.Observer,
		ObserverIdentity: s.Identity,
		Endpoint:         s.Endpoint,
		// Always true, and it is a field rather than an assumption so that a record carries the admission: the
		// source of these samples was asserted by configuration, not established by anything in this build.
		Declared:  true,
		StartedNs: s.Elapsed(),
	}
	unattributed := 0
	failures := 0
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		// The request's start, kept only to end the observation honestly on failure.
		started := s.Elapsed()
		body, err := s.Fetch(ctx)
		// Stamped AFTER the response, not before it. A slow exporter would otherwise have its reading dated to
		// the moment the request left -- which can be seconds earlier -- so the coverage calculation would
		// describe when this binary asked rather than when the values were read, and a stall would be invisible
		// in exactly the interval it corrupted. Taking the later bound is the conservative half of the pair:
		// the true observation time lies between the two, and dating it late can only widen an apparent gap.
		at := s.Elapsed()
		if err != nil {
			failures++
			if failures >= scrapeFailureLimit {
				obs.EndedNs = started
				return obs, unattributed, fmt.Errorf("device observer failed %d scrapes in a row, last: %w",
					failures, err)
			}
		} else {
			failures = 0
			samples, missed, unlabelled, perr := ParseDCGMUtilisation(body, at, s.Resolve)
			if perr != nil {
				// A malformed scrape is not a gap: the exporter answered and said something this build cannot
				// read, which is a different problem from it being absent, and guessing which is not this
				// function's job.
				obs.EndedNs = at
				return obs, unattributed, perr
			}
			obs.Samples = append(obs.Samples, samples...)
			unattributed += missed
			obs.UnlabelledBusySamples += unlabelled
		}
		select {
		case <-ctx.Done():
			obs.EndedNs = s.Elapsed()
			return obs, unattributed, nil
		case <-tick.C:
		}
	}
}
