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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// The trace must carry the served tokenizer's count for every prompt length, including the lengths that only
// ever appear on requests the guard refuses -- those are the denominator of the admitted-work fraction, and
// a refused request never reaches the engine to be counted.
func TestStampExactTokens(t *testing.T) {
	// A stand-in engine that reports a count no estimate would produce, so a fallback to ceil(chars/4) is
	// visible in the result rather than plausible.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		chars := 0
		if len(body.Messages) > 0 {
			chars = len(body.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n") //nolint:errcheck // test stub
		f.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d}}\n\n", chars/5) //nolint:errcheck // test stub
		f.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n") //nolint:errcheck // test stub
		f.Flush()
	}))
	defer engine.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	var b strings.Builder
	for i, chars := range []int{200, 200, 40000, 16384} {
		enc, err := json.Marshal(bench.TraceRow{Index: i + 1, Tenant: "premium-1", PromptLenChars: chars, MaxOutputTokens: 8})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	if err := stampExactTokens([]string{
		"-trace", path, "-gateway-url", engine.URL, "-model", "m", "-tenant", "premium-1",
	}); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	rows, err := readTrace(path)
	if err != nil {
		t.Fatalf("re-read trace: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("trace has %d rows after stamping, want 4", len(rows))
	}
	for _, r := range rows {
		want := r.PromptLenChars / 5
		if r.ExactInputTokens != want {
			t.Errorf("a %d-character prompt was stamped %d tokens, want %d (the engine's own count); an estimate would have said %d",
				r.PromptLenChars, r.ExactInputTokens, want, bench.EstInputTokensForChars(r.PromptLenChars))
		}
	}
}

// An engine that reports no count leaves the criterion unevaluable, and saying so is the whole point.
func TestStampExactTokensRefusesAnEngineThatReportsNoCount(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n") //nolint:errcheck // test stub
		f.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n") //nolint:errcheck // test stub
		f.Flush()
	}))
	defer engine.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	enc, err := json.Marshal(bench.TraceRow{Index: 1, Tenant: "premium-1", PromptLenChars: 200, MaxOutputTokens: 8})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, append(enc, '\n'), 0o600); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	err = stampExactTokens([]string{"-trace", path, "-gateway-url", engine.URL, "-model", "m", "-tenant", "premium-1"})
	if err == nil {
		t.Fatal("an engine that reported no prompt-token count was accepted; the run would proceed to score the admission match on estimates, which is the state this exists to end")
	}
	if !strings.Contains(err.Error(), "prompt-token count") {
		t.Errorf("the refusal does not say what was missing: %v", err)
	}
}
