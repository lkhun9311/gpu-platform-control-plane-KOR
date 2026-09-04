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

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// minimalValidMetrics is a small, valid vLLM-shaped exposition body: low usage, low waiting, well
// under either engage threshold used across these specs.
const minimalValidMetrics = `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.3
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1
`

// overThresholdMetrics is a small, valid vLLM-shaped exposition body whose values clear the
// default 0.85/8 engage thresholds used across these specs.
const overThresholdMetrics = `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.95
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 20
`

// newTestRef builds a BackendRef whose URL points at an httptest server, for scraper specs that
// need a real (loopback) HTTP round trip.
func newTestRef(namespace, name, rawURL string) *BackendRef {
	u, err := url.Parse(rawURL)
	Expect(err).NotTo(HaveOccurred())
	return &BackendRef{Namespace: namespace, Name: name, Port: 8080, URL: u, Model: "kvguard-test-model"}
}

// baseScraperConfig returns a scraperConfig with the design spec's v1 default thresholds and a
// fixed clock, for specs that only care about one or two tick() calls and set clock explicitly
// where they need to.
func baseScraperConfig(clock clockFunc) scraperConfig {
	return scraperConfig{
		engageUsage:    0.85,
		releaseUsage:   0.75,
		waitingThresh:  8,
		releaseSustain: 30 * time.Second,
		scrapeInterval: 2 * time.Second,
		maxStaleness:   6 * time.Second,
		httpTimeout:    2 * time.Second,
		clock:          clock,
	}
}

// --- Exposition parser -------------------------------------------------------

var _ = Describe("parseKVMetrics", func() {
	It("parses the two series it reads out of a real captured vLLM scrape", func() {
		f, err := os.Open("testdata/vllm_metrics_golden.txt")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = f.Close() }()

		sample, err := parseKVMetrics(f)
		Expect(err).NotTo(HaveOccurred())
		// The exact bytes a real 0.27.1 server wrote, not values chosen to suit the parser.
		Expect(sample.CacheUsage).To(Equal(0.0058629534628068525))
		Expect(sample.Waiting).To(Equal(32))
	})

	It("reads vllm:num_requests_waiting and not the by_reason family whose name extends it", func() {
		// 0.27.1 added vllm:num_requests_waiting_by_reason. Its name has the series the guard reads as a
		// strict prefix, and in the captured scrape its "capacity" series carries the same 32 the guard
		// wants -- so a prefix-matching reader would look correct here and be wrong the moment the two
		// diverge. Give the guard only the by_reason family and it must refuse rather than answer 32.
		text := `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.5
# HELP vllm:num_requests_waiting_by_reason x
# TYPE vllm:num_requests_waiting_by_reason gauge
vllm:num_requests_waiting_by_reason{engine="0",reason="capacity"} 32.0
vllm:num_requests_waiting_by_reason{engine="0",reason="deferred"} 0.0
`
		_, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).To(MatchError(ContainSubstring("missing vllm:num_requests_waiting")))
	})

	It("falls back to the V0 alias when only vllm:gpu_cache_usage_perc is present", func() {
		text := `# HELP vllm:gpu_cache_usage_perc x
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{engine="0"} 0.5
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 3
`
		sample, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(sample.CacheUsage).To(Equal(0.5))
		Expect(sample.Waiting).To(Equal(3))
	})

	It("prefers the V1 series over the V0 alias when both are present", func() {
		text := `# HELP vllm:gpu_cache_usage_perc x
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{engine="0"} 0.1
# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.9
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1
`
		sample, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(sample.CacheUsage).To(Equal(0.9))
	})

	It("aggregates multiple series for one logical backend: max usage, sum waiting", func() {
		text := `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.3
vllm:kv_cache_usage_perc{engine="1"} 0.6
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 2
vllm:num_requests_waiting{engine="1"} 4
`
		sample, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(sample.CacheUsage).To(Equal(0.6))
		Expect(sample.Waiting).To(Equal(6))
	})

	It("ignores unrelated series", func() {
		text := `# HELP unrelated_thing x
# TYPE unrelated_thing counter
unrelated_thing 999
# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.1
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1
`
		sample, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(sample.CacheUsage).To(Equal(0.1))
		Expect(sample.Waiting).To(Equal(1))
	})

	It("errors on malformed exposition text", func() {
		_, err := parseKVMetrics(strings.NewReader("vllm:kv_cache_usage_perc{engine=\"0\"\n"))
		Expect(err).To(HaveOccurred())
	})

	It("errors when usage is above the [0,1] range", func() {
		text := `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 1.5
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1
`
		_, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).To(HaveOccurred())
	})

	It("errors when usage is below the [0,1] range", func() {
		text := `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} -0.1
# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1
`
		_, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).To(HaveOccurred())
	})

	It("errors when the usage series (both V1 and V0) is missing entirely", func() {
		text := `# HELP vllm:num_requests_waiting x
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1
`
		_, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).To(HaveOccurred())
	})

	It("errors when the waiting series is missing entirely", func() {
		text := `# HELP vllm:kv_cache_usage_perc x
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.1
`
		_, err := parseKVMetrics(strings.NewReader(text))
		Expect(err).To(HaveOccurred())
	})

	It("bounds an oversized response: a valid series placed past maxMetricsBytes is never reached", func() {
		padding := strings.Repeat("# padding\n", 200000) // ~2,000,000 bytes, well past maxMetricsBytes (1MiB).
		body := padding + minimalValidMetrics
		_, err := parseKVMetrics(strings.NewReader(body))
		// Without the bound, this would parse cleanly since minimalValidMetrics is valid; the
		// bound truncates before reaching it, so parsing fails instead.
		Expect(err).To(HaveOccurred())
	})
})

// --- Pressure state machine ---------------------------------------------------

var _ = Describe("pressureState", func() {
	cfg := pressureConfig{
		engageUsage:    0.85,
		releaseUsage:   0.75,
		waitingThresh:  8,
		releaseSustain: 30 * time.Second,
	}
	base := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	It("does not engage on a single over-threshold sample", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		Expect(p.Engaged()).To(BeFalse())
	})

	It("engages after 2 consecutive fresh over-threshold samples", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())
	})

	It("engages on waiting depth alone, without cache usage crossing its threshold", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.1, Waiting: 9}, base)
		p.observe(kvSample{CacheUsage: 0.1, Waiting: 9}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())
	})

	It("resets the consecutive count on an intervening under-threshold sample", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(2*time.Second))
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(4*time.Second))
		Expect(p.Engaged()).To(BeFalse())
	})

	It("does not release before the sustain window elapses", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(4*time.Second))
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(20*time.Second))
		Expect(p.Engaged()).To(BeTrue())
	})

	It("releases once under both thresholds for the full sustain window", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(4*time.Second))
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(4*time.Second+30*time.Second))
		Expect(p.Engaged()).To(BeFalse())
	})

	It("restarts the sustain window when the under-threshold streak breaks", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())

		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(4*time.Second))
		// The under-threshold streak (started at t=4s) breaks here, at t=29s.
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(29*time.Second))
		Expect(p.Engaged()).To(BeTrue())

		// A fresh under-threshold streak starts at t=34s.
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(34*time.Second))
		Expect(p.Engaged()).To(BeTrue())
		// Only 25s after the restart point (34s): if the old streak from t=4s had survived, this
		// (59s, i.e. 55s after t=4s) would already have released; it must not have.
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(59*time.Second))
		Expect(p.Engaged()).To(BeTrue())
		// 30s after the restart point (34s): now it releases.
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(64*time.Second))
		Expect(p.Engaged()).To(BeFalse())
	})

	It("does not flap at the boundary between release and engage thresholds (asymmetric hysteresis)", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())
		// 0.8 is below engage (0.85) but above release (0.75): neither condition holds, so the
		// state must not change no matter how long this persists.
		for i := 1; i <= 10; i++ {
			p.observe(kvSample{CacheUsage: 0.8, Waiting: 0}, base.Add(time.Duration(i)*10*time.Second))
		}
		Expect(p.Engaged()).To(BeTrue())
	})

	It("resetConsecutive clears an in-progress engage count without touching engaged", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		Expect(p.Engaged()).To(BeFalse())
		p.resetConsecutive()
		// Without the reset, this would be "sample 2 of 2" and engage; with it, it is "sample 1
		// of 2" and must not.
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeFalse())
	})

	It("resetConsecutive restarts an in-progress release sustain window without touching engaged", func() {
		p := newPressureState(cfg)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base)
		p.observe(kvSample{CacheUsage: 0.9, Waiting: 0}, base.Add(2*time.Second))
		Expect(p.Engaged()).To(BeTrue())
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(4*time.Second))
		p.resetConsecutive()
		// Without the reset, base.Add(4s+30s) would already satisfy the window that started at
		// 4s; with it, the window can only start at whichever under-sample lands next.
		p.observe(kvSample{CacheUsage: 0.5, Waiting: 0}, base.Add(34*time.Second))
		Expect(p.Engaged()).To(BeTrue())
	})
})

// --- Scraper hardening ---------------------------------------------------------

var _ = Describe("backendScraper", func() {
	It("counts a scrape error and leaves telemetry not-fresh when the backend returns 500", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer up.Close()
		ref := newTestRef("kvguard-500-ns", "kvguard-500-backend", up.URL)
		b := newBackendScraper(*ref, baseScraperConfig(time.Now))

		before := testutil.ToFloat64(backendScrapeErrors.WithLabelValues(ref.Namespace, ref.Name))
		b.tick(context.Background())
		after := testutil.ToFloat64(backendScrapeErrors.WithLabelValues(ref.Namespace, ref.Name))
		Expect(after).To(Equal(before + 1))

		snap := b.snap.Load()
		Expect(snap.fresh).To(BeFalse())
		Expect(snap.engaged).To(BeFalse())
	})

	It("refuses to follow a redirect from the scrape target", func() {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer target.Close()
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/metrics", http.StatusFound)
		}))
		defer up.Close()
		ref := newTestRef("kvguard-redirect-ns", "kvguard-redirect-backend", up.URL)
		b := newBackendScraper(*ref, baseScraperConfig(time.Now))

		_, err := b.scrape(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(targetHits.Load()).To(Equal(int32(0)))
	})

	It("never forwards an Authorization or API-key header to the scrape target", func() {
		var gotAuth, gotAPIKey string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer up.Close()
		ref := newTestRef("kvguard-noauth-ns", "kvguard-noauth-backend", up.URL)
		b := newBackendScraper(*ref, baseScraperConfig(time.Now))

		_, err := b.scrape(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(gotAuth).To(BeEmpty())
		Expect(gotAPIKey).To(BeEmpty())
	})

	It("resets the consecutive engage count after a stale gap, so a lone post-gap sample does not instantly engage", func() {
		h := &switchableHandler{mode: "engage"}
		up := httptest.NewServer(h)
		defer up.Close()
		ref := newTestRef("kvguard-reset-ns", "kvguard-reset-backend", up.URL)

		base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		now := base
		cfg := baseScraperConfig(func() time.Time { return now })
		cfg.maxStaleness = 6 * time.Second
		b := newBackendScraper(*ref, cfg)

		// t=0: first over-threshold sample; 1 of 2 needed to engage.
		b.tick(context.Background())
		Expect(b.snap.Load().engaged).To(BeFalse())

		// Scrape failures long enough to exceed maxStaleness (6s) since the t=0 success.
		h.setMode("fail")
		for _, d := range []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second} {
			now = base.Add(d)
			b.tick(context.Background())
		}
		Expect(b.snap.Load().fresh).To(BeFalse())

		// Resume with a single over-threshold sample. Without the reset this would be "sample 2
		// of 2" (the t=0 sample plus this one) and engage immediately; with the reset it is
		// "sample 1 of 2" and must not engage yet.
		h.setMode("engage")
		now = base.Add(10 * time.Second)
		b.tick(context.Background())
		Expect(b.snap.Load().engaged).To(BeFalse())

		// A second consecutive fresh over-threshold sample now does engage.
		now = base.Add(12 * time.Second)
		b.tick(context.Background())
		Expect(b.snap.Load().engaged).To(BeTrue())
	})
})

// switchableHandler serves either overThresholdMetrics ("engage") or a 500 ("fail"), switchable
// mid-test via setMode, guarded by a mutex since it may be hit from the scraper's own goroutine
// in the race spec below as well as ticked directly in-line elsewhere.
type switchableHandler struct {
	mu   sync.Mutex
	mode string
}

func (h *switchableHandler) setMode(m string) {
	h.mu.Lock()
	h.mode = m
	h.mu.Unlock()
}

func (h *switchableHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	m := h.mode
	h.mu.Unlock()
	if m == "fail" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(overThresholdMetrics))
}

// --- Admit matrix ---------------------------------------------------------------

// managerWithSnapshot returns a scraperManager holding one backendScraper whose published
// snapshot is exactly snap, without starting any goroutine or performing any HTTP -- the Admit
// matrix specs below only care what kvAwareAdmitter.Admit does with a given snapshot, not how
// the snapshot came to be.
func managerWithSnapshot(key string, snap *backendSnapshot) *scraperManager {
	m := newScraperManager(scraperConfig{clock: time.Now})
	b := &backendScraper{}
	b.snap.Store(snap)
	m.scrapers[key] = b
	return m
}

var _ = Describe("kvAwareAdmitter.Admit", func() {
	ctx := context.Background()
	backend := &BackendRef{Namespace: "kvguard-admit-ns", Name: "kvguard-admit-backend"}
	longMeta := RequestMeta{EstInputTokens: 4096}
	shortMeta := RequestMeta{EstInputTokens: 100}

	It("admits when the backend has never been registered", func() {
		m := newScraperManager(scraperConfig{clock: time.Now})
		a := newKVAwareAdmitter(m, 4096)
		ok, reason := a.Admit(ctx, longMeta, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonBackendUnregistered))
	})

	It("admits when telemetry is stale, even if the backend was previously engaged (fail-open bypass, not fail-closed retain)", func() {
		m := managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: true, fresh: false})
		a := newKVAwareAdmitter(m, 4096)
		ok, reason := a.Admit(ctx, longMeta, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonTelemetryStale))
	})

	It("admits when fresh but not engaged", func() {
		m := managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: false, fresh: true})
		a := newKVAwareAdmitter(m, 4096)
		ok, reason := a.Admit(ctx, longMeta, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonNotEngaged))
	})

	It("admits a premium request even when engaged and fresh", func() {
		m := managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: true, fresh: true})
		a := newKVAwareAdmitter(m, 4096)
		ok, reason := a.Admit(ctx, longMeta, backend, "t", tierPremium)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonPremiumTier))
	})

	It("rejects a standard-long request when engaged and fresh", func() {
		m := managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: true, fresh: true})
		a := newKVAwareAdmitter(m, 4096)
		ok, reason := a.Admit(ctx, longMeta, backend, "t", tierStandard)
		Expect(ok).To(BeFalse())
		Expect(reason).To(Equal(reasonKVCachePressure))
	})

	It("admits a standard-short request even when engaged and fresh", func() {
		m := managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: true, fresh: true})
		a := newKVAwareAdmitter(m, 4096)
		ok, reason := a.Admit(ctx, shortMeta, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonBelowThreshold))
	})
})

// --- Scraper manager lifecycle ---------------------------------------------------

var _ = Describe("scraperManager idle eviction", func() {
	// Unregister existed with no production caller, so every backend ever routed to kept a goroutine, an HTTP
	// client, a scrape every interval, and a live gauge until the process exited. A deleted or renamed model
	// left admission_guard_engaged=1 on a machine that no longer exists — a reading of something that is not
	// there, which is worse than losing the series.
	//
	// The clock is injected and sweep is driven directly, so nothing here waits on a ticker.
	//
	// Mutation that turns this red: make sweep a no-op, or stamp lastRouted only when a scraper is created.
	It("stops a scraper nothing has routed to, and removes its gauges", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer up.Close()

		now := time.Now()
		m := newScraperManager(scraperConfig{
			scrapeInterval: time.Hour, maxStaleness: time.Hour, httpTimeout: time.Second,
			idleTimeout: 30 * time.Minute,
			clock:       func() time.Time { return now },
		})
		defer m.Stop()

		// The count is taken against a baseline rather than zero: backendTelemetryFresh is a package-level Vec
		// shared by every spec in this suite.
		//
		// And the wait is on the METRIC, not on snapshotFor. snapshotFor answers true as soon as Register puts
		// the scraper in the map, which is before its first scrape has published anything — a first version of
		// this spec waited on it, evicted a backend whose gauge had never been created, and passed while
		// asserting nothing.
		baseline := testutil.CollectAndCount(backendTelemetryFresh)

		ref := newTestRef("ns", "b1", up.URL)
		m.Register(ref)
		Eventually(func() int {
			return testutil.CollectAndCount(backendTelemetryFresh)
		}, 2*time.Second, 10*time.Millisecond).Should(Equal(baseline+1),
			"the scraper never published a gauge, so there is nothing for eviction to remove")

		// Still inside the window: a sweep must leave it alone, or the eviction is just a timer that fires.
		now = now.Add(20 * time.Minute)
		m.sweep()
		_, ok := m.snapshotFor(backendKey(ref))
		Expect(ok).To(BeTrue(), "a backend routed to 20 minutes ago was evicted under a 30-minute timeout")

		// Routing to it again must postpone eviction, which is the whole reason Register stamps before its
		// early return rather than after.
		m.Register(ref)
		now = now.Add(20 * time.Minute)
		m.sweep()
		_, ok = m.snapshotFor(backendKey(ref))
		Expect(ok).To(BeTrue(), "re-routing to a live backend did not postpone its eviction")

		now = now.Add(31 * time.Minute)
		m.sweep()
		_, ok = m.snapshotFor(backendKey(ref))
		Expect(ok).To(BeFalse(), "a backend idle past the timeout kept its scraper")
		Expect(testutil.CollectAndCount(backendTelemetryFresh)).To(Equal(baseline),
			"the evicted backend's gauge is still being reported for a machine nothing is scraping")
	})
})

// The race the first version of sweep had: it read the stale set, released the lock, and only then called
// Unregister. A request arriving in that window re-registered the backend — stamping lastRouted and returning
// early because the scraper still existed — and the eviction then stopped a scraper serving live traffic and
// deleted its gauges.
//
// Driven with -race and a Register hammering concurrently, so the assertion is about the invariant rather
// than about winning a timing lottery: a backend routed to throughout must never lose its scraper.
//
// Mutation that turns this red: collect the stale set under the lock, release it, then Unregister.
// The race the first version of sweep had: it read the stale set, released the lock, and only then called
// Unregister. A request arriving in that window re-registered the backend — stamping lastRouted and returning
// early because the scraper still existed — and the eviction then stopped a scraper serving live traffic and
// deleted its gauges.
//
// The interleaving is forced rather than hoped for. Each trial makes the backend genuinely stale, then runs
// one sweep against a Register spinning beside it. Under the bug the Register lands in the gap, succeeds
// against the still-present entry, and is undone by the Unregister that follows — leaving NO scraper, which
// is a state the fixed code cannot reach: there, whichever of the two wins the lock, the backend ends up
// registered.
//
// Mutation that turns this red: collect the stale set under the lock, release it, then Unregister.
var _ = Describe("scraperManager eviction under concurrent routing", func() {
	It("never leaves a routed-to backend unregistered", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer up.Close()

		for trial := range 200 {
			// Atomic because the backend's own scrape loop reads this clock on its own goroutine, so a plain
			// variable advanced from here is a data race -race reports.
			var nowNs atomic.Int64
			nowNs.Store(time.Now().UnixNano())
			m := newScraperManager(scraperConfig{
				scrapeInterval: time.Hour, maxStaleness: time.Hour, httpTimeout: time.Second,
				idleTimeout: time.Minute,
				clock:       func() time.Time { return time.Unix(0, nowNs.Load()) },
			})
			ref := newTestRef("ns", "hot", up.URL)

			m.Register(ref)
			// Past the idle timeout, so the sweep below genuinely wants to evict it.
			nowNs.Add(int64(2 * time.Minute))

			var wg sync.WaitGroup
			wg.Add(2)
			start := make(chan struct{})
			go func() { defer wg.Done(); <-start; m.sweep() }()
			go func() { defer wg.Done(); <-start; m.Register(ref) }()
			close(start)
			wg.Wait()

			_, ok := m.snapshotFor(backendKey(ref))
			m.Stop()
			Expect(ok).To(BeTrue(),
				"trial %d: a Register that landed between the stale check and the removal was undone, leaving "+
					"a backend that is being routed to with no scraper and no gauges", trial)
		}
	})
})

// Shutdown has to survive being called twice and has to end registration, because on this gateway Register
// runs on the request path: a completion draining after Stop would otherwise start a scraper that outlives
// every other goroutine in the process.
//
// Mutation that turns either of these red: drop the stopped check from Register, or the early return from Stop.
var _ = Describe("scraperManager shutdown", func() {
	newStopped := func() (*scraperManager, *BackendRef, *httptest.Server) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		m := newScraperManager(scraperConfig{
			scrapeInterval: time.Hour, maxStaleness: time.Hour, httpTimeout: time.Second,
			idleTimeout: time.Hour, clock: time.Now,
		})
		return m, newTestRef("ns", "late", up.URL), up
	}

	It("starts no scraper for a request that arrives after Stop", func() {
		m, ref, up := newStopped()
		defer up.Close()
		m.Stop()

		m.Register(ref)
		_, ok := m.snapshotFor(backendKey(ref))
		Expect(ok).To(BeFalse(),
			"a request draining during shutdown started a scraper that nothing will ever stop")
	})

	It("can be stopped twice", func() {
		m, _, up := newStopped()
		defer up.Close()
		m.Stop()
		// The second close of janitorStop panicked, which turns an ordinary double-shutdown — a signal
		// handler and a defer, say — into a crash on the way out.
		Expect(func() { m.Stop() }).NotTo(Panic())
	})
})

var _ = Describe("scraperManager lifecycle", func() {
	It("Register is idempotent: a second call for the same backend does not start a second scraper", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer up.Close()
		ref := newTestRef("kvguard-idem-ns", "kvguard-idem-backend", up.URL)
		m := newScraperManager(scraperConfig{scrapeInterval: time.Hour, maxStaleness: time.Hour, httpTimeout: time.Second, clock: time.Now})
		defer m.Stop()

		m.Register(ref)
		m.Register(ref)
		Expect(m.scrapers).To(HaveLen(1))
	})

	It("Unregister stops the scraper and removes it from lookup", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer up.Close()
		ref := newTestRef("kvguard-unreg-ns", "kvguard-unreg-backend", up.URL)
		m := newScraperManager(scraperConfig{scrapeInterval: 5 * time.Millisecond, maxStaleness: time.Hour, httpTimeout: time.Second, clock: time.Now})
		m.Register(ref)
		Eventually(func() bool {
			_, ok := m.snapshotFor(backendKey(ref))
			return ok
		}).Should(BeTrue())

		m.Unregister(ref)
		_, ok := m.snapshotFor(backendKey(ref))
		Expect(ok).To(BeFalse())

		// A second Unregister for a backend that is no longer registered must be a harmless
		// no-op, not a panic.
		m.Unregister(ref)
	})

	It("Stop stops every running scraper", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalValidMetrics))
		}))
		defer up.Close()
		refA := newTestRef("kvguard-stop-ns-a", "kvguard-stop-backend-a", up.URL)
		refB := newTestRef("kvguard-stop-ns-b", "kvguard-stop-backend-b", up.URL)
		m := newScraperManager(scraperConfig{scrapeInterval: 5 * time.Millisecond, maxStaleness: time.Hour, httpTimeout: time.Second, clock: time.Now})
		m.Register(refA)
		m.Register(refB)
		Eventually(func() bool {
			_, okA := m.snapshotFor(backendKey(refA))
			_, okB := m.snapshotFor(backendKey(refB))
			return okA && okB
		}).Should(BeTrue())

		m.Stop()
		Expect(m.scrapers).To(BeEmpty())
	})
})

// --- Metrics ---------------------------------------------------------------------

var _ = Describe("kv-aware metrics", func() {
	It("flips admission_guard_engaged and backend_telemetry_fresh, and increments backend_scrape_errors_total", func() {
		h := &switchableHandler{mode: "engage"}
		up := httptest.NewServer(h)
		defer up.Close()
		ref := newTestRef("kvguard-metrics-ns", "kvguard-metrics-backend", up.URL)

		base := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
		now := base
		cfg := baseScraperConfig(func() time.Time { return now })
		b := newBackendScraper(*ref, cfg)

		Expect(testutil.ToFloat64(backendTelemetryFresh.WithLabelValues(ref.Namespace, ref.Name))).To(Equal(0.0))

		b.tick(context.Background())
		now = base.Add(2 * time.Second)
		b.tick(context.Background())
		Expect(testutil.ToFloat64(admissionGuardEngaged.WithLabelValues(ref.Namespace, ref.Name, ref.Model))).To(Equal(1.0))
		Expect(testutil.ToFloat64(backendTelemetryFresh.WithLabelValues(ref.Namespace, ref.Name))).To(Equal(1.0))

		errsBefore := testutil.ToFloat64(backendScrapeErrors.WithLabelValues(ref.Namespace, ref.Name))
		h.setMode("fail")
		now = base.Add(20 * time.Second) // well past maxStaleness (6s)
		b.tick(context.Background())
		Expect(testutil.ToFloat64(backendScrapeErrors.WithLabelValues(ref.Namespace, ref.Name))).To(Equal(errsBefore + 1))
		Expect(testutil.ToFloat64(backendTelemetryFresh.WithLabelValues(ref.Namespace, ref.Name))).To(Equal(0.0))
	})
})

// --- Race: scraper goroutine vs request-path reads --------------------------------

var _ = Describe("scraper vs request-path concurrency", func() {
	It("scrapes concurrently with Admit reads without racing (meaningful under go test -race)", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(overThresholdMetrics))
		}))
		defer up.Close()
		ref := newTestRef("kvguard-race-ns", "kvguard-race-backend", up.URL)

		m := newScraperManager(scraperConfig{
			engageUsage: 0.85, releaseUsage: 0.75, waitingThresh: 8,
			releaseSustain: 30 * time.Second,
			scrapeInterval: time.Millisecond, maxStaleness: time.Second,
			httpTimeout: time.Second, clock: time.Now,
		})
		a := newKVAwareAdmitter(m, 4096)
		a.RegisterBackend(ref)
		defer m.Stop()

		var wg sync.WaitGroup
		wg.Add(2)
		deadline := time.Now().Add(100 * time.Millisecond)
		for range 2 {
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					a.Admit(context.Background(), RequestMeta{EstInputTokens: 5000}, ref, "t", tierStandard)
				}
			}()
		}
		wg.Wait()
	})
})

// --- Pipeline wiring ---------------------------------------------------------------
//
// Reuses newAdmissionServer/eligibleLongBody/authedRequest from admission_pipeline_test.go.

var _ = Describe("kv-aware admission pipeline placement", func() {
	It("admits an eligible standard-long request when the backend has just been registered and has no fresh telemetry yet", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Not a valid vLLM /metrics body; the guard must still admit rather than block the
			// request on it, since Admit only ever reads an already-published snapshot.
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()

		admitter, stop := NewKVAwareAdmitter(KVAwareConfig{
			EngageUsage: 0.85, ReleaseUsage: 0.75, WaitingThresh: 8,
			ReleaseSustain: 30 * time.Second,
			ScrapeInterval: time.Hour,
			MaxStaleness:   time.Hour,
			HTTPTimeout:    time.Second,
			LongThreshold:  4096,
		})
		defer stop()

		s := newAdmissionServer(up.URL, tierStandard, AdmissionKVAware, admitter)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(eligibleLongBody))
		Expect(rr.Code).To(Equal(http.StatusOK))
	})
})

// recordingAdmitter observes what admitCandidates registers and what it meters.
type recordingAdmitter struct {
	registered []string
	metered    []string
}

func (r *recordingAdmitter) RegisterBackend(b *BackendRef) {
	r.registered = append(r.registered, b.Name)
}

func (r *recordingAdmitter) Admit(_ context.Context, _ RequestMeta, b *BackendRef, _, _ string) (bool, string) {
	r.metered = append(r.metered, b.Name)
	return true, ""
}

// statelessRecorder is a recordingAdmitter that declares its Admit free of side effects, and rejects for
// exactly one named backend so the specs below can put the rejection anywhere in the candidate list.
type statelessRecorder struct {
	recordingAdmitter
	rejectFor string
}

func (s *statelessRecorder) AdmitIsStateless() {}

func (s *statelessRecorder) Admit(_ context.Context, _ RequestMeta, b *BackendRef, _, _ string) (bool, string) {
	s.metered = append(s.metered, b.Name)
	if b.Name == s.rejectFor {
		return false, "kv_cache_pressure"
	}
	return true, ""
}

// Registration covers every candidate. Which candidates are ASKED depends on whether asking costs anything.
//
// Registering only the head meant that when the head went down its scraper hit the same dead Service, its
// snapshot went stale, and the kv-aware guard — which fails OPEN on staleness — admitted everything, while
// the spare absorbing all the traffic had no scraper at all. The guard went blind exactly when fallback made
// it matter, and nothing reported it: every request still succeeded.
//
// Asking only the head was the same shape of mistake one level up. The head is the backend TRIED first, not
// the one that will serve, so a stale head bypassing the guard let the request reach a spare nobody
// consulted. A stateless admitter is now asked about every candidate and one dissent rejects; a stateful one
// is still asked once, because static-cap spends tokens per call and billing one request several times would
// make the arm measure something other than offered load.
//
// Mutations that turn these red: register targets[0] only; ask targets[0] only for a stateless admitter; ask
// every candidate regardless of statelessness; or take the last verdict rather than the first dissent.
var _ = Describe("admitCandidates", func() {
	It("registers every candidate and meters only the first, for a stateful admitter", func() {
		rec := &recordingAdmitter{}
		targets := []*BackendRef{
			{Name: "head", Namespace: "ns", Model: "m"},
			{Name: "spare", Namespace: "ns", Model: "m"},
		}

		admit, reason := admitCandidates(context.Background(), rec, RequestMeta{Model: "m"}, targets, "t1", "premium")

		Expect(admit).To(BeTrue())
		Expect(reason).To(BeEmpty())
		Expect(rec.registered).To(Equal([]string{"head", "spare"}),
			"a backend that can serve traffic was left without a telemetry scraper")
		Expect(rec.metered).To(Equal([]string{"head"}),
			"one request was charged against more than one backend's budget")
	})

	It("asks a stateless admitter about every candidate", func() {
		rec := &statelessRecorder{}
		targets := []*BackendRef{
			{Name: "head", Namespace: "ns", Model: "m"},
			{Name: "spare", Namespace: "ns", Model: "m"},
		}

		admit, _ := admitCandidates(context.Background(), rec, RequestMeta{Model: "m"}, targets, "t1", "standard")

		Expect(admit).To(BeTrue())
		Expect(rec.metered).To(Equal([]string{"head", "spare"}))
	})

	// The defect this closes: the head bypasses (stale telemetry, or simply healthy), the request travels to a
	// spare that is under pressure, and nothing ever asked the spare.
	It("rejects when a candidate other than the head refuses", func() {
		rec := &statelessRecorder{rejectFor: "spare"}
		targets := []*BackendRef{
			{Name: "head", Namespace: "ns", Model: "m"},
			{Name: "spare", Namespace: "ns", Model: "m"},
		}

		admit, reason := admitCandidates(context.Background(), rec, RequestMeta{Model: "m"}, targets, "t1", "standard")

		Expect(admit).To(BeFalse())
		Expect(reason).To(Equal("kv_cache_pressure"))
	})

	// Asking stops at the first dissent, so a rejecting head is never overruled by a permissive spare behind
	// it. Without this a "take the last verdict" implementation would satisfy the two specs above.
	// The regression the all-candidate change introduced: forwarding tries at most maxBackendAttempts, so a
	// candidate past that cap can never serve, and letting it vote meant a request could be refused on the
	// pressure of a machine it would never have reached. The caller caps once and every stage shares the
	// slice; this asserts the shared slice is what admission sees.
	It("never consults a candidate the request could not reach", func() {
		rec := &statelessRecorder{rejectFor: "third"}
		resolved := []*BackendRef{
			{Name: "head", Namespace: "ns", Model: "m"},
			{Name: "spare", Namespace: "ns", Model: "m"},
			{Name: "third", Namespace: "ns", Model: "m"},
		}

		admit, _ := admitCandidates(context.Background(), rec, RequestMeta{Model: "m"},
			capBackendAttempts(resolved), "t1", "standard")

		Expect(admit).To(BeTrue())
		Expect(rec.metered).To(Equal([]string{"head", "spare"}))
		Expect(rec.registered).To(Equal([]string{"head", "spare"}),
			"a scraper was started for a backend no request can reach")
	})

	It("stops at the first candidate that refuses", func() {
		rec := &statelessRecorder{rejectFor: "head"}
		targets := []*BackendRef{
			{Name: "head", Namespace: "ns", Model: "m"},
			{Name: "spare", Namespace: "ns", Model: "m"},
		}

		admit, _ := admitCandidates(context.Background(), rec, RequestMeta{Model: "m"}, targets, "t1", "standard")

		Expect(admit).To(BeFalse())
		Expect(rec.metered).To(Equal([]string{"head"}))
	})

	// An admitter that does not register at all must still be metered — off and static-cap do not implement
	// backendRegistrar, and a type assertion that silently skipped admission would disable those arms.
	It("meters an admitter that cannot register", func() {
		plain := admitterFunc(func(context.Context, RequestMeta, *BackendRef, string, string) (bool, string) {
			return false, "over_budget"
		})
		admit, reason := admitCandidates(context.Background(), plain, RequestMeta{Model: "m"},
			[]*BackendRef{{Name: "only", Model: "m"}}, "t1", "premium")
		Expect(admit).To(BeFalse())
		Expect(reason).To(Equal("over_budget"))
	})
})

// admitterFunc adapts a function to Admitter without implementing backendRegistrar.
type admitterFunc func(context.Context, RequestMeta, *BackendRef, string, string) (bool, string)

func (f admitterFunc) Admit(ctx context.Context, m RequestMeta, b *BackendRef, t, tier string) (bool, string) {
	return f(ctx, m, b, t, tier)
}

// An admit says why, because three different admits mean three different things about the guard.
//
// Admit returned (true, "") when the backend was never registered, when its telemetry had gone stale, when
// there was no pressure, and when the caller was premium. In the evidence and in the decisions metric those
// are one outcome, so an arm C that spent a whole run blind -- scraper never succeeding, guard bypassed on
// every request -- is indistinguishable from an arm C that watched a calm backend and correctly let
// everything through. The first is a broken run reported as a scientific FAIL; the second is a result.
//
// The 2026-09-03 run cannot be told apart on this axis at all. Its evidence carries only statuses.
var _ = Describe("why the kv-aware guard admitted a request", func() {
	ctx := context.Background()
	backend := &BackendRef{Namespace: "why-ns", Name: "why-backend"}
	long := RequestMeta{EstInputTokens: 8192}
	short := RequestMeta{EstInputTokens: 100}

	It("distinguishes a guard that could not see from a guard that saw no pressure", func() {
		blind := newKVAwareAdmitter(managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: true, fresh: false}), 4096)
		ok, reason := blind.Admit(ctx, long, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonTelemetryStale))

		calm := newKVAwareAdmitter(managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: false, fresh: true}), 4096)
		ok, reason = calm.Admit(ctx, long, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonNotEngaged))

		Expect(reasonTelemetryStale).NotTo(Equal(reasonNotEngaged))
	})

	It("names a backend it has no telemetry for at all", func() {
		a := newKVAwareAdmitter(newScraperManager(scraperConfig{clock: time.Now}), 4096)
		ok, reason := a.Admit(ctx, long, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonBackendUnregistered))
	})

	It("separates the two ways of being outside the gated population", func() {
		engaged := newKVAwareAdmitter(managerWithSnapshot(backendKey(backend), &backendSnapshot{engaged: true, fresh: true, cacheUsage: 0.9}), 4096)

		ok, reason := engaged.Admit(ctx, long, backend, "t", tierPremium)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonPremiumTier))

		ok, reason = engaged.Admit(ctx, short, backend, "t", tierStandard)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(reasonBelowThreshold))

		ok, reason = engaged.Admit(ctx, long, backend, "t", tierStandard)
		Expect(ok).To(BeFalse())
		Expect(reason).To(Equal(reasonKVCachePressure))
	})
})

// What the guard SAW, not only what it decided.
//
// A request now records why it was admitted, which separates a guard that was blind from one that saw a calm
// backend. It does not say how calm. "not_engaged" is the same string whether the cache was at 0.10 and the
// load never pressured the engine, or at 0.83 against a threshold of 0.85 -- and those call for opposite
// conclusions: the first says the experiment failed to create the condition it is testing, the second says
// the threshold is set past where the damage happens.
//
// C/R1 came back at 83.747 in the last paid run with the guard rejecting 15.3% of eligible traffic, and the
// evidence cannot distinguish those two explanations. That is the question the pilot exists to answer, so
// the numbers the decision was made on travel with the request that made it.
var _ = Describe("the pressure the guard was looking at", func() {
	backend := &BackendRef{Namespace: "saw-ns", Name: "saw-backend"}

	It("reports the snapshot the decision used", func() {
		a := newKVAwareAdmitter(managerWithSnapshot(backendKey(backend),
			&backendSnapshot{engaged: false, fresh: true, cacheUsage: 0.83, waiting: 7}), 4096)
		state, ok := a.Observed(backend)
		Expect(ok).To(BeTrue())
		Expect(state.CacheUsage).To(BeNumerically("~", 0.83, 1e-9))
		Expect(state.Waiting).To(Equal(7))
		Expect(state.Engaged).To(BeFalse())
		Expect(state.Fresh).To(BeTrue())
	})

	It("says it has nothing rather than reporting zeros for a backend it never scraped", func() {
		a := newKVAwareAdmitter(newScraperManager(scraperConfig{clock: time.Now}), 4096)
		_, ok := a.Observed(backend)
		Expect(ok).To(BeFalse())
	})
})
