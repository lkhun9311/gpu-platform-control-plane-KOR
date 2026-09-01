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

package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTPSender", func() {
	It("records first-token, end, and output tokens from a streaming response", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.Header.Get("Authorization")).To(Equal("Bearer premium-key"))
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			for range 3 {
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
				f.Flush()
				time.Sleep(2 * time.Millisecond)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			f.Flush()
		}))
		defer srv.Close()

		sender := NewHTTPSender(srv.URL, "llama-3-8b", map[string]string{"premium-1": "premium-key"}, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		res := sender.Send(context.Background(), TraceRow{Tenant: "premium-1", PromptLenChars: 100, MaxOutputTokens: 8}, time.Now().UnixNano())

		Expect(res.HTTPStatus).To(Equal(200))
		Expect(res.ErrorKind).To(BeEmpty())
		Expect(res.OutputTokens).To(Equal(3))
		Expect(res.FirstTokenUnixNanos).To(BeNumerically(">", 0))
		Expect(res.EndUnixNanos).To(BeNumerically(">=", res.FirstTokenUnixNanos))
	})

	It("replays a real captured vLLM stream and does not let the role frame claim first-token", func() {
		// The bytes a vLLM 0.27.1 server actually wrote; see testdata/PROVENANCE.txt.
		//
		// Its first frame is "delta":{"role":"assistant","content":""} -- text-free, and emitted before the
		// model has produced anything. Serving it, then pausing, then serving the six token frames means a
		// sender that stamped first-token on any frame would report a TTFT from before the pause. The pause
		// is the assertion: it is what makes the two behaviours distinguishable in the number.
		raw, err := os.ReadFile("testdata/vllm_sse_stream.txt")
		Expect(err).NotTo(HaveOccurred())
		frames := strings.SplitAfter(string(raw), "\n\n")

		const prefillPause = 40 * time.Millisecond
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			for i, frame := range frames {
				if frame == "" {
					continue
				}
				if i == 1 {
					time.Sleep(prefillPause)
				}
				_, _ = fmt.Fprint(w, frame)
				f.Flush()
			}
		}))
		defer srv.Close()

		sender := NewHTTPSender(srv.URL, "Qwen/Qwen2.5-0.5B-Instruct", nil, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		start := time.Now()
		res := sender.Send(context.Background(), TraceRow{Tenant: "premium-1", PromptLenChars: 20, MaxOutputTokens: 6}, start.UnixNano())

		Expect(res.HTTPStatus).To(Equal(200))
		Expect(res.ErrorKind).To(BeEmpty())
		// Six token frames for max_tokens: 6. The role frame is not one of them, and neither is [DONE].
		Expect(res.OutputTokens).To(Equal(6))
		Expect(time.Unix(0, res.FirstTokenUnixNanos)).To(BeTemporally(">=", start.Add(prefillPause)))
	})

	It("puts the corpus text on the wire, which is the only place the payload can be checked", func() {
		// PromptText being correct proves nothing on its own: the sender could still build its own payload,
		// and for the whole life of this harness it did -- strings.Repeat("x", n), which a byte-pair
		// tokenizer collapses by a factor that depends on the served model's vocabulary. Reverting the
		// sender to that passed every other test in this package, including all of PromptText's own, so
		// this reads the body the server actually received.
		var gotContent string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
			Expect(req.Messages).To(HaveLen(1))
			gotContent = req.Messages[0].Content
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		const promptChars = 40000
		sender := NewHTTPSender(srv.URL, "m", nil, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		sender.Send(context.Background(), TraceRow{Tenant: "standard-noisy", PromptLenChars: promptChars, MaxOutputTokens: 16}, time.Now().UnixNano())

		Expect(gotContent).To(Equal(PromptText(promptChars)))
	})

	It("records a 429 as a rejection, not a completed request", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":{"code":"kv_cache_pressure"}}`)
		}))
		defer srv.Close()

		sender := NewHTTPSender(srv.URL, "m", nil, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		res := sender.Send(context.Background(), TraceRow{Tenant: "standard-noisy", PromptLenChars: 40000, MaxOutputTokens: 8}, time.Now().UnixNano())

		Expect(res.HTTPStatus).To(Equal(429))
		Expect(res.ErrorKind).To(Equal("rejected"))
		Expect(res.FirstTokenUnixNanos).To(BeZero())
	})

	It("records a slow response past the timeout as a timeout", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		sender := NewHTTPSender(srv.URL, "m", nil, 30*time.Millisecond, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		res := sender.Send(context.Background(), TraceRow{Tenant: "premium-1", PromptLenChars: 100, MaxOutputTokens: 8}, time.Now().UnixNano())

		Expect(res.ErrorKind).To(Equal("timeout"))
	})

	// The pool is what keeps the instrument's own TCP handshakes out of the latency it reports, so the
	// specs below pin that it is actually installed and actually sized from the run rather than guessed.
	It("reuses one connection across sequential requests instead of dialling per request", func() {
		var conns int
		// Unstarted, because ConnState has to be installed before the listener accepts anything.
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t\"}}]}\n\n")
			f.Flush()
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			f.Flush()
		}))
		// ConnState counts accepted connections, which is the only place the difference between a pooled
		// and an unpooled client is visible at all; the request count is identical either way.
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns++
			}
		}
		srv.Start()
		defer srv.Close()

		sender := NewHTTPSender(srv.URL, "m", nil, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		for range 5 {
			res := sender.Send(context.Background(), TraceRow{Tenant: "premium-1", PromptLenChars: 10, MaxOutputTokens: 1}, time.Now().UnixNano())
			Expect(res.HTTPStatus).To(Equal(200))
		}
		Expect(conns).To(Equal(1))
	})

	// The spec above pools only the path where the request succeeded, which is not the path these arms are
	// about. An admission guard protects capacity BY returning 429, so a run that actually exercises the
	// guard is mostly rejections — and the sender used to return on a non-200 without reading the body,
	// which marks the connection unusable. Every rejection then cost a fresh handshake, inside the latency
	// the harness reports: precisely the instrument artifact the pool was introduced to remove, present on
	// the arm it was introduced to measure. Nothing caught it because the evidence trace behind the
	// 431-to-6 claim recorded zero rejections in every arm.
	//
	// Mutation that turns this red: drop the drain from the non-200 branch of Send, and conns becomes 5.
	It("reuses one connection across sequential rejections, not only across successful streams", func() {
		var conns int
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":{"code":"kv_cache_pressure"}}`)
		}))
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns++
			}
		}
		srv.Start()
		defer srv.Close()

		sender := NewHTTPSender(srv.URL, "m", nil, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 8, DrainForReuse: true})
		for range 5 {
			res := sender.Send(context.Background(), TraceRow{Tenant: "standard-noisy", PromptLenChars: 10, MaxOutputTokens: 1}, time.Now().UnixNano())
			Expect(res.ErrorKind).To(Equal("rejected"))
		}
		Expect(conns).To(Equal(1))
	})

	// The legacy mode is what the before/after arms of hack/m5b-gateway-path.sh replay, so it has to
	// actually behave like the old client; a "before" that quietly carried the fix would make the whole
	// comparison meaningless.
	It("dials per request in legacy mode, reproducing the client this replaced", func() {
		var conns int
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t\"}}]}\n\n")
			f.Flush()
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			f.Flush()
			// A remainder too large to have been buffered already, and it is what makes this spec
			// deterministic rather than a coin flip.
			//
			// readStream breaks at [DONE] without consuming what follows, so whether the connection can be
			// reused turns on whether the transport happened to hold the rest already. With only a two-byte
			// tail that is genuinely a race with the kernel, and the spec failed 5 times in 12 plain runs
			// observing conns == 1 — the erratic behaviour its own comment describes, asserted as though it
			// were reliable. A tail this size cannot be sitting in a buffer, so an undrained body always
			// leaves unread bytes and the connection is always discarded, which is the causal claim the
			// before/after arms rest on.
			_, _ = fmt.Fprint(w, ": "+strings.Repeat("x", 64<<10)+"\n\n")
			f.Flush()
		}))
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns++
			}
		}
		srv.Start()
		defer srv.Close()

		legacy, err := SenderConnForMode(SenderModeLegacy, nil, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		sender := NewHTTPSender(srv.URL, "m", nil, 5*time.Second, legacy)
		for range 5 {
			Expect(sender.Send(context.Background(), TraceRow{Tenant: "premium-1", PromptLenChars: 10, MaxOutputTokens: 1}, time.Now().UnixNano()).HTTPStatus).To(Equal(200))
		}
		// More than one connection for five sequential requests is the defect being reproduced.
		//
		// The exact count is not pinned because it depends on whether the transport happened to have already
		// buffered the stream terminator when the body was closed, which is precisely why the old client's
		// reuse was erratic rather than absent.
		Expect(conns).To(BeNumerically(">", 1))
	})

	It("resolves each sender mode to the connection handling it names", func() {
		trace := []TraceRow{{Index: 0, OffsetMs: 0}, {Index: 1, OffsetMs: 1000}}

		legacy, err := SenderConnForMode(SenderModeLegacy, trace, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(legacy).To(Equal(SenderConn{}))

		drainOnly, err := SenderConnForMode(SenderModeDrainOnly, trace, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(drainOnly).To(Equal(SenderConn{DrainForReuse: true}))

		pooled, err := SenderConnForMode(SenderModePooled, trace, 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(pooled.DrainForReuse).To(BeTrue())
		Expect(pooled.MaxIdleConnsPerHost).To(BeNumerically(">=", http.DefaultMaxIdleConnsPerHost))

		// A typo must stop the run rather than silently replay with the wrong instrument.
		_, err = SenderConnForMode("poooled", trace, 30*time.Second)
		Expect(err).To(HaveOccurred())
	})

	It("derives the pool from the trace's own arrival rate and timeout, not from a constant", func() {
		// 20 rows spread over 1000ms is 20/s; at a 30s timeout the open-loop in-flight ceiling is 600,
		// but a trace cannot have more in flight than it has rows, so the row count binds first.
		trace := make([]TraceRow, 20)
		for i := range trace {
			trace[i] = TraceRow{Index: i, OffsetMs: int64(i * 50)}
		}
		Expect(PoolSizeForTrace(trace, 30*time.Second)).To(Equal(20))

		// The same rate over a longer trace lets the rate x timeout ceiling bind instead of the row count.
		long := make([]TraceRow, 2000)
		for i := range long {
			long[i] = TraceRow{Index: i, OffsetMs: int64(i * 50)}
		}
		// 2000 rows over 99950ms is ~20.01/s, so the ceiling is just above 600 and well under 2000.
		Expect(PoolSizeForTrace(long, 30*time.Second)).To(BeNumerically("~", 601, 2))
	})

	It("never sizes the pool below Go's own default", func() {
		// An empty or single-request trace must not produce a pool of 0 or 1, which would be worse than
		// the http.DefaultTransport behaviour this replaced.
		Expect(PoolSizeForTrace(nil, 30*time.Second)).To(Equal(http.DefaultMaxIdleConnsPerHost))
		Expect(PoolSizeForTrace([]TraceRow{{Index: 0}}, 30*time.Second)).To(Equal(http.DefaultMaxIdleConnsPerHost))
	})
})
