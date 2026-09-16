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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The guard against a real engine, rather than against text this repository wrote.
//
// Everything else in this file's package tests the guard on fixtures: a handler that returns a chosen
// exposition, a clock that advances on command. Those check the logic and cannot check the premise -- that a
// real vLLM publishes what the parser reads, often enough and shaped the way the state machine assumes.
// This test registers a real backend, drives real traffic into it, and waits for the guard to engage on
// numbers nobody here chose.
//
// It is skipped unless GPUAAS_LIVE_VLLM_URL points at a running server, so an ordinary `go test ./...`
// is unaffected. Run it with, for example:
//
//	docker run -d --name vllmcpu --shm-size=2g -p 18000:8000 \
//	  vllm/vllm-openai-cpu:v0.27.1-x86_64 Qwen/Qwen2.5-0.5B-Instruct \
//	  --dtype bfloat16 --max-model-len 1024 --max-num-seqs 8
//	GPUAAS_LIVE_VLLM_URL=http://127.0.0.1:18000 go test ./internal/gateway/ -run Live -v
//
// The engagement here comes through the WAITING arm of the engage condition rather than the cache-usage
// arm, and that is a property of the host rather than of the guard: a CPU server saturates its scheduler
// long before a KV cache sized for a GPU fills. The state machine takes either signal by design
// (cacheUsage > engageUsage OR waiting > waitingThresh), so what is exercised end to end is the same in
// both cases -- scrape, parse, threshold, two-consecutive-sample debounce, tier split, release. Only the
// magnitudes differ, and the paid GPU run is where the cache-usage arm gets its own live evidence.
func liveVLLM(t *testing.T) string {
	t.Helper()
	base := os.Getenv("GPUAAS_LIVE_VLLM_URL")
	if base == "" {
		t.Skip("GPUAAS_LIVE_VLLM_URL is unset; skipping the live-engine test")
	}
	resp, err := http.Get(strings.TrimSuffix(base, "/") + "/health")
	if err != nil {
		t.Fatalf("live vLLM at %s is not reachable: %v", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live vLLM at %s answered /health with %d", base, resp.StatusCode)
	}
	return strings.TrimSuffix(base, "/")
}

// liveModel asks the server which model it is serving, so the test never has to be told.
func liveModel(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/v1/models")
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(out.Data) == 0 {
		t.Fatal("live server reports no models")
	}
	return out.Data[0].ID
}

// pressLive holds the engine busy until the returned stop function is called.
//
// The requests are deliberately ordinary completions rather than anything crafted: the point of this test
// is that the guard reacts to a server doing its normal job, not to a payload shaped to move a gauge.
func pressLive(base, model string, concurrency int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": strings.Repeat("scheduling and admission control. ", 20)}},
		"max_tokens": 400,
		"stream":     false,
	})
	for range concurrency {
		wg.Go(func() {
			client := &http.Client{Timeout: 120 * time.Second}
			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
				if err != nil {
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		})
	}
	return func() { cancel(); wg.Wait() }
}

func TestLiveEngineDrivesTheGuardThroughEngageAndRelease(t *testing.T) {
	base := liveVLLM(t)
	model := liveModel(t, base)

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("base url %s has no usable port: %v", base, err)
	}
	// Namespace and Name are only labels on the exported gauges here; URL is what the scraper actually uses.
	ref := &BackendRef{Namespace: "live", Name: "vllm", Port: int32(port), URL: u, Model: model}

	const longThreshold = 4096
	admitter, stop := NewKVAwareAdmitter(KVAwareConfig{
		EngageUsage:  0.85,
		ReleaseUsage: 0.75,
		// The engine's own defaults decide what is reachable here. A CPU server queues before its cache
		// fills, so waiting is the arm this host can actually cross; 8 is the value cmd/gateway ships.
		WaitingThresh: 8,
		// Shortened from the shipped 30s so the release half of the state machine is exercised inside a
		// test's patience. The threshold values above are NOT shortened, because they are what is under test.
		ReleaseSustain: 3 * time.Second,
		ScrapeInterval: 500 * time.Millisecond,
		MaxStaleness:   2 * time.Second,
		HTTPTimeout:    2 * time.Second,
		LongThreshold:  longThreshold,
	})
	defer stop()

	registrar, ok := admitter.(interface{ RegisterBackend(*BackendRef) })
	if !ok {
		t.Fatal("kv-aware admitter does not expose RegisterBackend")
	}

	// Re-register on every decision, because that is the production wiring: chatCompletions registers the
	// backend it just routed to, on every routed request, and registration doubles as the "still in use"
	// stamp the idle janitor reads.
	//
	// Registering once and then only polling looks equivalent and is not. IdleTimeout defaults to ten scrape
	// intervals, so a backend that is never re-registered has its scraper stopped -- here, five seconds in.
	// The first version of this test did exactly that, and the symptom was not an error but silence: after
	// four good scrapes there was no snapshot at all, Admit took its unregistered-backend bypass, and the
	// guard admitted everything for the remaining eighty-five seconds while the engine sat visibly
	// overloaded. A guard that fails open is the right choice, and it is also the reason a wiring mistake
	// upstream of it produces a passing-looking run rather than a complaint.
	decide := func(meta RequestMeta, tenant, tier string) (bool, string) {
		registrar.RegisterBackend(ref)
		return admitter.Admit(context.Background(), meta, ref, tenant, tier)
	}

	longStandard := RequestMeta{Model: model, EstInputTokens: longThreshold + 1}
	shortStandard := RequestMeta{Model: model, EstInputTokens: 16}
	longPremium := longStandard

	// Idle: the guard has no reason to reject anything, and must not.
	waitFor(t, 10*time.Second, "guard to admit a long standard request while the engine is idle", func() bool {
		admit, _ := decide(longStandard, "standard-noisy", tierStandard)
		return admit
	})

	release := pressLive(base, model, 24)
	pressStopped := false
	defer func() {
		if !pressStopped {
			release()
		}
	}()

	var engagedReason string
	waitFor(t, 90*time.Second, "guard to engage against a really loaded engine", func() bool {
		admit, reason := decide(longStandard, "standard-noisy", tierStandard)
		engagedReason = reason
		return !admit
	})
	if engagedReason != reasonKVCachePressure {
		t.Fatalf("rejected for %q, want %q", engagedReason, reasonKVCachePressure)
	}

	// While engaged the split is the whole point of the arm: premium passes, and a standard request under
	// the length threshold passes too. A guard that rejected everything would be load shedding.
	if admit, reason := decide(longPremium, "premium-1", tierPremium); !admit {
		t.Fatalf("premium was rejected while engaged (%q); the guard is shedding rather than protecting", reason)
	}
	if admit, reason := decide(shortStandard, "standard-quiet", tierStandard); !admit {
		t.Fatalf("a short standard request was rejected while engaged (%q); only long ones are eligible", reason)
	}

	release()
	pressStopped = true

	waitFor(t, 60*time.Second, "guard to release once the engine drains", func() bool {
		admit, _ := decide(longStandard, "standard-noisy", tierStandard)
		return admit
	})
}

// waitFor polls cond until it holds or the budget runs out, failing with what it was waiting for.
func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			t.Log("observed: " + what)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", budget, what)
}
