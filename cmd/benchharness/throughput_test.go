package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// TestPooledRepetitionsDoNotChargeTheArmForTheWashout fixes the defect that made this metric's first
// printing wrong by a factor of three.
//
// Summarize sees pooled rows and cannot know how they were split, so its span -- max(end) minus min(send)
// -- covers the washout pauses between repetitions, during which the arm sent nothing. On the paid
// 2026-09-03 evidence that reported 14.1 output tokens per second for an arm that actually sustained 46.4.
//
// The number was not merely imprecise: it was small enough that all four arms looked alike, which is
// exactly the comparison this metric exists to make. So the split is applied where it is known, and this
// test injects the fault by placing a washout an order of magnitude longer than the work either side of it.
func TestPooledRepetitionsDoNotChargeTheArmForTheWashout(t *testing.T) {
	const (
		sec      = int64(1_000_000_000)
		repSecs  = 2
		washSecs = 100
	)
	dir := t.TempDir()
	// Two repetitions of two seconds each, separated by a hundred-second washout. Ten tokens per
	// repetition over two seconds is five tokens per second; charged for the washout it would be 0.19.
	var paths []string
	for rep, start := range []int64{0, int64(repSecs+washSecs) * sec} {
		rows := make([]bench.RawRow, 0, 2)
		for i := range 2 {
			base := sec + start + int64(i)*sec
			rows = append(rows, bench.RawRow{
				Index: i, Arm: "static-cap", Tenant: "premium-1", SendUnixNanos: base,
				FirstTokenUnixNanos: base + 100_000_000, EndUnixNanos: base + sec,
				EstInputTokens: 50, OutputTokens: 5, HTTPStatus: 200,
				TraceChecksum: "T", LongThreshold: 4096, MatchTolerance: 0.05,
			})
		}
		var b strings.Builder
		for _, r := range rows {
			enc, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			b.Write(enc)
			b.WriteByte('\n')
		}
		path := filepath.Join(dir, "raw-static-cap-"+string(rune('1'+rep))+".jsonl")
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		paths = append(paths, path)
	}

	e, err := loadArmEvidence(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, byArm := e.summarize()
	s := byArm["static-cap"]

	if s.RepetitionCount != 2 {
		t.Fatalf("repetitions = %d, want 2", s.RepetitionCount)
	}
	if s.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want 20", s.OutputTokens)
	}
	// Each repetition spans from its first send to its last end: 2 seconds. Two of them is 4.
	if math.Abs(s.ActiveSeconds-4) > 0.001 {
		t.Fatalf("active seconds = %.3f, want 4 (the two repetitions, not the %d-second washout between them)",
			s.ActiveSeconds, washSecs)
	}
	if math.Abs(s.OutputTokensPerSecond-5) > 0.01 {
		t.Fatalf("output tok/s = %.2f, want 5; pooling the washout into the span would report %.2f",
			s.OutputTokensPerSecond, 20.0/float64(2*repSecs+washSecs))
	}
}

// TestReplayPutsThePriorityOnTheWire proves the flag reaches the engine rather than only the sender.
//
// The priority field is the one axis the paid microtest showed moving the premium tail, and it is a single
// JSON key: a arm that loses it between the flag and the request body is indistinguishable from the control
// in every artifact the run produces. So the assertion is made against the bytes a server received, not
// against the sender's own state.
func TestReplayPutsThePriorityOnTheWire(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]json.RawMessage{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Priority json.RawMessage `json:"priority"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		// The tenant is the API key the sender authenticated with.
		seen[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] = body.Priority
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	sender := bench.NewHTTPSender(srv.URL, "m", map[string]string{"premium-1": "kp", "standard-noisy": "kn"},
		5*time.Second, bench.SenderConn{})
	prio, err := parsePriorities("premium-1=0,standard-noisy=5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sender.SetPriorities(prio)
	bench.Replay(context.Background(), sender, []bench.TraceRow{
		{Index: 0, Tenant: "premium-1", PromptLenChars: 40, MaxOutputTokens: 4},
		{Index: 1, Tenant: "standard-noisy", PromptLenChars: 40, MaxOutputTokens: 4},
	}, bench.ReplayOptions{Arm: "prio"})

	mu.Lock()
	defer mu.Unlock()
	if got := string(seen["kp"]); got != "0" {
		t.Fatalf("premium priority on the wire = %q, want 0", got)
	}
	if got := string(seen["kn"]); got != "5" {
		t.Fatalf("noisy priority on the wire = %q, want 5", got)
	}
}

// TestReplayWithoutPrioritiesOmitsTheField keeps the control arm a control.
//
// A zero-valued int would serialise as "priority": 0, which is vLLM's MOST urgent value -- so an
// unconfigured control arm would silently ship the strongest possible treatment to every tenant. The field
// must be absent, not zero.
func TestReplayWithoutPrioritiesOmitsTheField(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	sender := bench.NewHTTPSender(srv.URL, "m", map[string]string{"premium-1": "kp"}, 5*time.Second, bench.SenderConn{})
	bench.Replay(context.Background(), sender, []bench.TraceRow{{Index: 0, Tenant: "premium-1", PromptLenChars: 40, MaxOutputTokens: 4}},
		bench.ReplayOptions{Arm: "off"})

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("got %d requests, want 1", len(bodies))
	}
	if strings.Contains(bodies[0], "priority") {
		t.Fatalf("control arm sent a priority field: %s", bodies[0])
	}
}
