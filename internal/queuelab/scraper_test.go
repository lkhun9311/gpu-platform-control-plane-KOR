package queuelab

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock advances by a fixed step each time it is read, so a test can drive the scraper's sense of time
// without sleeping through a run.
type fakeClock struct {
	now  int64
	step int64
}

func (c *fakeClock) elapsed() int64 {
	v := c.now
	c.now += c.step
	return v
}

// scraperOver builds a scraper whose transport returns the given bodies in order, then blocks the run.
func scraperOver(t *testing.T, clock *fakeClock, bodies ...string) (*DeviceScraper, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	i := 0
	s := &DeviceScraper{
		Observer: ObserverDCGM,
		Identity: "dcgm@sha256:abc",
		Interval: 500 * time.Millisecond,
		Resolve:  resolver,
		Elapsed:  clock.elapsed,
		Fetch: func(context.Context) ([]byte, error) {
			if i >= len(bodies) {
				cancel()
				return []byte(bodies[len(bodies)-1]), nil
			}
			b := bodies[i]
			i++
			return []byte(b), nil
		},
	}
	return s, ctx, cancel
}

// The observation must carry its own coverage bounds, on the COLLECTOR's clock, or nothing can check that it
// covered the interval it is asked about.
func TestObserveRecordsWhatItActuallyCovered(t *testing.T) {
	// The clock starts well after zero, because a run's collector has already been going when the observer
	// attaches. An observation that stamped StartedNs at zero would claim to have covered from the beginning
	// of the run, and EstablishesDeviceWork's coverage check -- obs.StartedNs > fromNs -- would then never
	// fire for an observer that attached late.
	clock := &fakeClock{now: int64(30 * time.Second), step: int64(100 * time.Millisecond)}
	s, ctx, cancel := scraperOver(t, clock, renderedScrape, renderedScrape, renderedScrape)
	defer cancel()
	obs, unattributed, err := s.Observe(ctx)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obs.EndedNs <= obs.StartedNs {
		t.Fatalf("coverage bounds %d..%d do not describe an interval", obs.StartedNs, obs.EndedNs)
	}
	if obs.StartedNs != int64(30*time.Second) {
		t.Fatalf("StartedNs = %d, want the collector's clock at the moment the observer attached (%d). A "+
			"fixed or zero start makes the observation claim coverage it does not have, and the late-attach "+
			"check never fires", obs.StartedNs, int64(30*time.Second))
	}
	// And that claim must actually be refused: an interval beginning before the observer attached is not
	// covered, whatever the samples inside it show.
	if ok, why := EstablishesDeviceWork(obs, SameWindowClaim("victim-uid", 0, obs.EndedNs)); ok {
		t.Fatalf("an interval starting before the observer attached was reported as covered (%s)", why)
	}
	if obs.ObserverIdentity == "" || obs.Observer != ObserverDCGM {
		t.Fatalf("the observation does not say who produced it: %+v", obs)
	}
	if len(obs.Samples) < 4 {
		t.Fatalf("samples = %d over several scrapes of a two-Pod payload", len(obs.Samples))
	}
	if unattributed != 0 {
		t.Fatalf("unattributed = %d", unattributed)
	}
	// Every sample must be stamped on the collector's clock, not the scraper's own.
	for _, smp := range obs.Samples {
		if smp.AtNs < obs.StartedNs || smp.AtNs > obs.EndedNs {
			t.Fatalf("a sample at %d ns falls outside the coverage %d..%d", smp.AtNs, obs.StartedNs, obs.EndedNs)
		}
	}
}

// An exporter that stops answering ends the observation rather than letting it keep its bounds while seeing
// nothing -- which would read as coverage, and leave an operator unable to tell a down exporter from an idle
// card.
func TestARepeatedlyFailingExporterEndsTheObservation(t *testing.T) {
	clock := &fakeClock{step: int64(100 * time.Millisecond)}
	s := &DeviceScraper{
		Observer: ObserverDCGM, Identity: "dcgm@sha256:abc", Interval: 10 * time.Millisecond,
		Resolve: resolver, Elapsed: clock.elapsed,
		Fetch: func(context.Context) ([]byte, error) { return nil, errors.New("connection refused") },
	}
	// Bounded, so a regression that removes the failure limit FAILS here instead of wedging the suite. Observe
	// blocks until its context ends or the limit trips -- that is correct for a loop meant to run for a whole
	// run -- and a test that passes an unbounded context is asserting on a loop it has not given an exit.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	obs, _, err := s.Observe(ctx)
	if err == nil {
		t.Fatal("an exporter that never answered produced a clean observation: the failure limit is what " +
			"separates a down exporter from an idle card, and without it the observation keeps its coverage " +
			"bounds while seeing nothing")
	}
	if obs == nil {
		t.Fatal("the partial observation was thrown away; a run could then not say whether the exporter was " +
			"present at all")
	}
	if !strings.Contains(err.Error(), "in a row") {
		t.Fatalf("the error does not say the failures were consecutive: %v", err)
	}
	// And what it did cover must not establish anything.
	if ok, _ := EstablishesDeviceWork(obs, SameWindowClaim("victim-uid", obs.StartedNs, obs.EndedNs)); ok {
		t.Fatal("an observation from an exporter that never answered established device work")
	}
}

// A scrape the parser cannot read is not a gap: the exporter answered and said something this build does not
// understand, and reporting that as absence would send an operator to the wrong place.
func TestAMalformedScrapeIsReportedRatherThanTreatedAsAGap(t *testing.T) {
	clock := &fakeClock{step: int64(100 * time.Millisecond)}
	s := &DeviceScraper{
		Observer: ObserverDCGM, Identity: "dcgm@sha256:abc", Interval: 10 * time.Millisecond,
		Resolve: resolver, Elapsed: clock.elapsed,
		Fetch: func(context.Context) ([]byte, error) {
			return []byte("DCGM_FI_DEV_GPU_UTIL{UUID=\"G\",namespace=\"queuelab-r1\",pod=\"a2-borrow-x7k2p\"} 4000\n"), nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := s.Observe(ctx); err == nil {
		t.Fatal("a scrape reporting 4000% utilisation was accepted")
	}
}

// The interval must leave room under the gap the verdict allows, or one slow response is an uncovered window
// that the run only discovers afterwards.
func TestAnIntervalAtOrOverTheGapLimitIsRefused(t *testing.T) {
	clock := &fakeClock{step: 1}
	for _, d := range []time.Duration{0, -time.Second, maxObserverGap, maxObserverGap + time.Second} {
		s := &DeviceScraper{
			Observer: ObserverDCGM, Identity: "x", Interval: d, Resolve: resolver, Elapsed: clock.elapsed,
			Fetch: func(context.Context) ([]byte, error) { return []byte(renderedScrape), nil },
		}
		if _, _, err := s.Observe(context.Background()); err == nil {
			t.Errorf("interval %s was accepted", d)
		}
	}
}

// The scraper is not allowed to invent any of its three dependencies.
func TestAScraperNeedsATransportAResolverAndTheCollectorsClock(t *testing.T) {
	base := func() *DeviceScraper {
		return &DeviceScraper{
			Observer: ObserverDCGM, Identity: "x", Interval: 10 * time.Millisecond,
			Resolve: resolver, Elapsed: (&fakeClock{step: 1}).elapsed,
			Fetch: func(context.Context) ([]byte, error) { return []byte(renderedScrape), nil },
		}
	}
	for name, break_ := range map[string]func(*DeviceScraper){
		"no transport": func(s *DeviceScraper) { s.Fetch = nil },
		"no resolver":  func(s *DeviceScraper) { s.Resolve = nil },
		"no clock":     func(s *DeviceScraper) { s.Elapsed = nil },
	} {
		s := base()
		break_(s)
		if _, _, err := s.Observe(context.Background()); err == nil {
			t.Errorf("%s: the scraper ran anyway", name)
		}
	}
}
