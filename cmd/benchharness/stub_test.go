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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestApplyModelPathStubURI pins that a stub:// storage URI sets the response profile.
//
// This is the only channel through which a stub deployed as an InferenceDeployment can be configured, since
// the controller builds the container with a fixed argument list, so a silent failure here would produce a
// backend that is not the one the evidence run claims to have measured.
func TestApplyModelPathStubURI(t *testing.T) {
	p := stubProfile{tokens: 8, ttft: 5 * time.Millisecond, itl: 2 * time.Millisecond}
	if err := p.applyModelPath("stub://profile?tokens=4&ttft-ms=2000&itl-ms=7"); err != nil {
		t.Fatalf("applyModelPath returned an error: %v", err)
	}
	if p.tokens != 4 {
		t.Errorf("tokens = %d, want 4", p.tokens)
	}
	if p.ttft != 2*time.Second {
		t.Errorf("ttft = %s, want 2s", p.ttft)
	}
	if p.itl != 7*time.Millisecond {
		t.Errorf("itl = %s, want 7ms", p.itl)
	}
}

// TestApplyModelPathPartialKeepsDefaults pins that an absent parameter keeps its current value rather than
// resetting to zero, so a profile only has to name what it changes.
func TestApplyModelPathPartialKeepsDefaults(t *testing.T) {
	p := stubProfile{tokens: 8, ttft: 5 * time.Millisecond, itl: 2 * time.Millisecond}
	if err := p.applyModelPath("stub://profile?ttft-ms=50"); err != nil {
		t.Fatalf("applyModelPath returned an error: %v", err)
	}
	if p.tokens != 8 || p.itl != 2*time.Millisecond {
		t.Errorf("unnamed parameters changed: tokens=%d itl=%s", p.tokens, p.itl)
	}
	if p.ttft != 50*time.Millisecond {
		t.Errorf("ttft = %s, want 50ms", p.ttft)
	}
}

// TestApplyModelPathIgnoresRealStorageURI pins that a genuine storage URI passes through untouched, so the
// profile hook cannot change the behaviour of a stub pointed at a real model path.
func TestApplyModelPathIgnoresRealStorageURI(t *testing.T) {
	for _, path := range []string{"", "s3://bucket/model", "pvc://claim/path"} {
		p := stubProfile{tokens: 8, ttft: 5 * time.Millisecond, itl: 2 * time.Millisecond}
		if err := p.applyModelPath(path); err != nil {
			t.Fatalf("applyModelPath(%q) returned an error: %v", path, err)
		}
		if p.tokens != 8 || p.ttft != 5*time.Millisecond || p.itl != 2*time.Millisecond {
			t.Errorf("applyModelPath(%q) changed the profile to %+v", path, p)
		}
	}
}

// TestApplyModelPathRejectsNonNumeric pins that a malformed profile fails loudly.
//
// Falling back to the default would start a backend whose timing silently differs from the one the run
// intended, which is the failure mode a measurement harness can least afford.
func TestApplyModelPathRejectsNonNumeric(t *testing.T) {
	p := stubProfile{tokens: 8}
	if err := p.applyModelPath("stub://profile?tokens=eight"); err == nil {
		t.Fatal("applyModelPath accepted a non-numeric tokens value")
	}
}

// TestStubStatsAttributesRequestsToConnections is the spec behind the connection-reuse measurement.
//
// The whole claim that the gateway's shared Transport pooled rests on this counter separating "many requests
// on few connections" from "one request per connection", so it is pinned rather than trusted.
func TestStubStatsAttributesRequestsToConnections(t *testing.T) {
	s := newStubStats()
	// Two connections, one carrying three requests and one carrying a single request.
	busy := s.connContext(context.Background(), nil)
	quiet := s.connContext(context.Background(), nil)
	for range 3 {
		s.begin(busy)
		s.end()
	}
	s.begin(quiet)
	s.end()

	snap := s.snapshot()
	if snap.RequestsServed != 4 {
		t.Errorf("requestsServed = %d, want 4", snap.RequestsServed)
	}
	if snap.ChatConnections != 2 {
		t.Errorf("chatConnections = %d, want 2", snap.ChatConnections)
	}
	if snap.MaxRequestsOnOneConnection != 3 {
		t.Errorf("maxRequestsOnOneConnection = %d, want 3", snap.MaxRequestsOnOneConnection)
	}
}

// TestStubStatsPeakInFlight pins that the peak is the high-water mark and not the current value, since the
// concurrency the pool cap has to cover is only ever visible as a peak.
func TestStubStatsPeakInFlight(t *testing.T) {
	s := newStubStats()
	c := s.connContext(context.Background(), nil)
	s.begin(c)
	s.begin(c)
	s.begin(c)
	s.end()
	s.end()
	s.end()

	snap := s.snapshot()
	if snap.PeakInFlight != 3 {
		t.Errorf("peakInFlight = %d, want 3", snap.PeakInFlight)
	}
	if snap.InFlight != 0 {
		t.Errorf("inFlight = %d, want 0", snap.InFlight)
	}
}

// TestStubStatsExcludesNonChatConnections pins that a connection which never carried a chat request is
// counted as accepted but not as a chat connection.
//
// The kubelet opens a fresh connection for every probe, so without this separation the probes would inflate
// exactly the number the reuse measurement reports.
func TestStubStatsExcludesNonChatConnections(t *testing.T) {
	s := newStubStats()
	s.connState(nil, http.StateNew)
	s.connState(nil, http.StateNew)
	c := s.connContext(context.Background(), nil)
	s.begin(c)
	s.end()

	snap := s.snapshot()
	if snap.ConnectionsAccepted != 2 {
		t.Errorf("connectionsAccepted = %d, want 2", snap.ConnectionsAccepted)
	}
	if snap.ChatConnections != 1 {
		t.Errorf("chatConnections = %d, want 1", snap.ChatConnections)
	}
}

// TestStubStatsResetKeepsLiveState pins that starting a new measurement window zeroes the window's counts
// while carrying the present forward.
//
// Zeroing the open-connection count instead would send it negative the moment a connection opened before the
// reset finally closed, and a negative connection count would discredit every other number beside it.
func TestStubStatsResetKeepsLiveState(t *testing.T) {
	s := newStubStats()
	s.connState(nil, http.StateNew)
	c := s.connContext(context.Background(), nil)
	s.begin(c)

	s.reset()
	snap := s.snapshot()
	if snap.RequestsServed != 0 || snap.ChatConnections != 0 {
		t.Errorf("window counts survived the reset: %+v", snap)
	}
	if snap.OpenConnections != 1 {
		t.Errorf("openConnections = %d, want 1 (a still-open connection must carry over)", snap.OpenConnections)
	}
	if snap.InFlight != 1 || snap.PeakInFlight != 1 {
		t.Errorf("in-flight state did not carry over: inFlight=%d peakInFlight=%d", snap.InFlight, snap.PeakInFlight)
	}

	// The still-open connection is counted again the first time it carries a request in the new window,
	// which is the honest reading: it is a connection carrying this window's load.
	s.begin(c)
	if got := s.snapshot().ChatConnections; got != 1 {
		t.Errorf("chatConnections after the new window = %d, want 1", got)
	}
	s.end()
	s.end()
	s.connState(nil, http.StateClosed)
}

// A client that goes away must take its handler with it.
//
// The handler used a bare time.Sleep for the first-token and inter-token delays, which ignores r.Context(),
// so a request the harness had already timed out kept its goroutine asleep for the whole configured response
// while stats counted it as in flight. That is not a cosmetic leak: peakInFlight is what PoolSizeForTrace
// derives the sender's pool from, so a timeout-heavy arm inflated the instrument's own sizing.
//
// The assertion is on inFlight rather than on wall clock, because a handler that returned promptly but left
// the counter up would be the same defect wearing a different face.
//
// Mutation that turns this red: replace the wait helper's select with time.Sleep(d).
func TestStubAbandonsAStreamWhoseClientHasGoneAway(t *testing.T) {
	stats := newStubStats()
	// Long enough that a handler ignoring cancellation is still asleep when the assertion runs.
	mux := stubMux(stubProfile{tokens: 4, ttft: 30 * time.Second, itl: time.Second}, stats)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	// Wait for the handler to be counted before cancelling, or the test could cancel a request that never
	// arrived and pass without exercising anything.
	if !waitFor(func() bool { return stats.snapshot().InFlight == 1 }, 5*time.Second) {
		t.Fatalf("the handler never registered as in flight: %+v", stats.snapshot())
	}
	cancel()
	<-done

	if !waitFor(func() bool { return stats.snapshot().InFlight == 0 }, 5*time.Second) {
		t.Fatalf("the client is gone and the handler is still counted in flight after 5s: %+v; peakInFlight "+
			"feeds the sender's pool size, so this contaminates the instrument", stats.snapshot())
	}
}

// waitFor polls cond until it holds or the budget expires, so the assertions above are about the handler
// rather than about how fast this machine schedules a goroutine.
func waitFor(cond func() bool, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// The reset endpoint discards the counters an evidence run is collecting, and the kubelet already GETs this
// same port for /health, so an accidental GET must not be a route to it.
//
// Mutation that turns this red: drop the method check in the /stats/reset handler.
func TestStubResetRefusesAnAccidentalGET(t *testing.T) {
	stats := newStubStats()
	stats.begin(context.Background())
	stats.end()
	srv := httptest.NewServer(stubMux(stubProfile{tokens: 1}, stats))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/stats/reset")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a GET to /stats/reset returned %d, want 405", resp.StatusCode)
	}
	if got := stats.snapshot().RequestsServed; got != 1 {
		t.Fatalf("the refused GET still cleared the counters: requestsServed=%d, want 1", got)
	}

	// The POST must still work, or this guard has simply broken the evidence script.
	post, err := http.Post(srv.URL+"/stats/reset", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = post.Body.Close() }()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("POST to /stats/reset returned %d, want 200", post.StatusCode)
	}
	if got := stats.snapshot().RequestsServed; got != 0 {
		t.Fatalf("POST did not reset: requestsServed=%d, want 0", got)
	}
}

// A profile that would serve a different experiment than the manifest declares must refuse at startup.
//
// Neither bad value fails on its own — a negative token count emits nothing and a negative delay is served as
// no delay — so without this the stub stands up and the run's numbers describe a backend nobody asked for.
//
// Mutation that turns this red: return nil unconditionally from validate.
func TestStubProfileValidationRefusesWhatWouldSilentlyChangeTheExperiment(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    stubProfile
		bad  bool
	}{
		{"the default profile", stubProfile{tokens: 8, ttft: 5 * time.Millisecond, itl: 2 * time.Millisecond}, false},
		{"zero delays are legitimate", stubProfile{tokens: 1}, false},
		{"no tokens measures nothing", stubProfile{tokens: 0}, true},
		{"negative tokens emits nothing", stubProfile{tokens: -3}, true},
		{"negative ttft is served as none", stubProfile{tokens: 1, ttft: -time.Second}, true},
		{"negative itl is served as none", stubProfile{tokens: 1, itl: -time.Second}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.validate()
			if tc.bad && err == nil {
				t.Fatalf("%+v was accepted; it would serve an experiment nobody declared", tc.p)
			}
			if !tc.bad && err != nil {
				t.Fatalf("%+v was refused: %v", tc.p, err)
			}
		})
	}
}

// The one field this backend can be configured through must not accept a typo silently.
//
// "token=4" for "tokens=4" reads correctly to a human, was not a parameter this understood, and left the
// default in place — so the CR described one profile and the stub served another, with nothing downstream
// able to notice because the default is itself a valid profile. validate() cannot catch it for that exact
// reason. A malformed stub:// URI had the same shape: the parse error was folded into "not a stub URI" and
// the defaults ran.
//
// The real-storage case is asserted alongside, because refusing everything unparseable would break the
// passthrough this function exists to preserve — and that is the way to "fix" this that looks right and
// stops a real serving runtime from starting.
//
// Mutations that turn this red: drop the unknown-parameter loop; or fold the parse error back into the
// scheme check.
func TestApplyModelPathRefusesATypoInsteadOfServingTheDefaults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		wantErr   bool
		wantTokns int
	}{
		{"a correct stub URI still applies", "stub://x?tokens=4&ttft-ms=7", false, 4},
		{"a real storage URI still passes through", "s3://models/llama-3-8b", false, 8},
		{"a typo in a parameter name", "stub://x?token=4", true, 0},
		{"an unknown parameter beside correct ones", "stub://x?tokens=4&itl_ms=3", true, 0},
		{"parameters that do not parse", "stub://x?tokens=%zz", true, 0},
		// Two distinct failure points, and the earlier fixture only reached the second: "tokens=%zz" parses
		// as a URL and fails in ParseQuery, while a bad host fails url.Parse itself. Without this row the
		// parse-error branch had no test and folding it back into the scheme check killed nothing.
		{"a URI announcing our scheme that does not parse at all", "stub://%zz", true, 0},
		{"a real storage URI that does not parse is still not ours", "s3://%zz", false, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := stubProfile{tokens: 8, ttft: 5 * time.Millisecond, itl: 2 * time.Millisecond}
			err := p.applyModelPath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted and left the profile at %+v; the CR describes one backend and the "+
						"stub serves another", tc.path, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q was refused: %v", tc.path, err)
			}
			if p.tokens != tc.wantTokns {
				t.Fatalf("%q left tokens=%d, want %d", tc.path, p.tokens, tc.wantTokns)
			}
		})
	}
}

// A stub that is ready the instant it binds cannot stand in for a server that loads.
//
// This exists because M7's pod-kill scenario was unobservable without it: the replacement Pod went healthy
// inside a second, the outage fell between the recorder's polls, and successive runs disagreed about
// whether the failure had happened. A readiness delay is not test-rigging -- it is the stub being less
// unlike a real engine, which loads weights before it serves.
func TestTheStubCanReportUnreadyWhileItIsStillStarting(t *testing.T) {
	p := stubProfile{tokens: 1, readyAfter: 400 * time.Millisecond}
	srv := httptest.NewServer(stubMux(p, newStubStats()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/health answered %d while still starting; a probe would mark the Pod ready immediately and "+
			"the outage a kill produces would be shorter than anything can observe", resp.StatusCode)
	}

	time.Sleep(500 * time.Millisecond)
	resp2, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("probe after the delay: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/health still answered %d after the delay elapsed; the Pod would never become ready", resp2.StatusCode)
	}
}

// Zero keeps the behaviour every other caller depends on.
func TestTheStubIsReadyImmediatelyWhenNoDelayIsAsked(t *testing.T) {
	srv := httptest.NewServer(stubMux(stubProfile{tokens: 1}, newStubStats()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a stub with no readiness delay answered %d; every other caller expects it ready at once", resp.StatusCode)
	}
}
