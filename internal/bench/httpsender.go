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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/gateway"
)

// HTTPSender replays a trace row as a streaming OpenAI chat-completions request against the gateway.
//
// It records first-token and end timestamps on the client's own clock, which is the only honest source for TTFT because the gateway's request histogram does not start until the proxy handoff.
type HTTPSender struct {
	client     *http.Client
	gatewayURL string
	model      string
	// apiKeys maps a trace tenant to the API key the gateway resolves it from, so one sender can drive premium and standard tenants through the real identity chain.
	apiKeys map[string]string
	// priorities maps a trace tenant to the scheduling priority sent with its requests; absent means none.
	priorities map[string]int
	// timeout bounds a single request; on expiry the row is recorded as a timeout rather than dropped.
	timeout time.Duration
	// drain reports whether the unread tail of each response is consumed so its connection can be pooled.
	//
	// It is a field rather than always-on so a run can reproduce the client this replaced and measure the difference; see SenderConn.
	drain bool
	// now is injected only by tests; production uses the wall clock.
	now func() time.Time
}

// SenderConn configures how a sender handles the connections it makes.
//
// The two fields travel together because neither is worth anything alone: a sized pool has nothing to pool
// if every response body is abandoned before EOF, and draining still throws connections away if the pool
// only holds two of them.
type SenderConn struct {
	// MaxIdleConnsPerHost sizes the idle pool kept per host.
	//
	// Zero means "no Transport of our own", which makes http.Client fall back to http.DefaultTransport and
	// its default of 2 (net/http's DefaultMaxIdleConnsPerHost).
	MaxIdleConnsPerHost int
	// DrainForReuse reads the unread tail of each response so its connection can go back to the pool.
	DrainForReuse bool
}

// The sender modes name whole generations of this client's connection handling, not individual knobs.
//
// They exist so the cost of the change can be measured rather than asserted: SenderModeLegacy reproduces
// exactly the client this replaced, SenderModePooled is what a real run uses, and SenderModeDrainOnly sits
// between them so the improvement can be attributed to the drain and the pool separately instead of being
// reported as one lump.
const (
	// SenderModeLegacy is the client as it was: http.DefaultTransport, and no drain.
	SenderModeLegacy = "legacy"
	// SenderModeDrainOnly keeps Go's default two-connection pool but drains, isolating the drain's share.
	SenderModeDrainOnly = "drain-only"
	// SenderModePooled is the current client: a pool sized from the run, plus the drain that lets it be used.
	SenderModePooled = "pooled"
)

// SenderConnForMode resolves a mode name against the trace about to be replayed.
//
// An unrecognized mode is an error rather than a fallback: silently replaying with the wrong client would
// fold the instrument's own TCP handshakes into a latency number nobody would think to question.
func SenderConnForMode(mode string, trace []TraceRow, timeout time.Duration) (SenderConn, error) {
	switch mode {
	case SenderModeLegacy:
		return SenderConn{}, nil
	case SenderModeDrainOnly:
		return SenderConn{DrainForReuse: true}, nil
	case SenderModePooled:
		return SenderConn{MaxIdleConnsPerHost: PoolSizeForTrace(trace, timeout), DrainForReuse: true}, nil
	default:
		return SenderConn{}, fmt.Errorf("unknown sender mode %q; want %q, %q or %q",
			mode, SenderModeLegacy, SenderModeDrainOnly, SenderModePooled)
	}
}

// PoolSizeForTrace returns how many idle connections a replay of trace should keep warm per host.
//
// Why the sender needs this at all.
//
// The client used to be built with no Transport, so it used http.DefaultTransport and its
// MaxIdleConnsPerHost of 2 (net/http's DefaultMaxIdleConnsPerHost). The gateway's own outbound pool was
// raised off that same default for exactly the reason that matters here (internal/gateway/proxy.go): under
// open-loop dispatch many requests to one host are in flight at once, and a cap of 2 closes all but two of
// those connections the moment a wave completes, so the next wave handshakes again.
//
// In the client that cost lands inside the latency the harness reports rather than in the system under test.
// It was measured (hack/m5b-gateway-path-evidence.log): replaying an 884-request trace with the old client
// opened 431 connections, exactly 2.0 requests per connection, which is Go's default cap reproduced to the
// decimal. The same trace with this pool and the drain opened 6.
//
// Where the number comes from.
//
// It is derived per run rather than being a constant, because the harness's in-flight ceiling is a property
// of the run's own parameters, not of the harness. Dispatch is open-loop (Replay never waits for a response),
// so in-flight requests accumulate at the trace's arrival rate and leave only by completing or timing out,
// giving a hard ceiling of rate x timeout. That is the same shape of derivation the gateway uses, and
// deliberately not a copy of the gateway's 600: 600 is what that formula happens to give for one particular
// pair of flag values.
//
// The result is additionally capped at len(trace), since a trace cannot have more requests in flight than it
// contains, and floored at Go's default of 2 so this can never be worse than what it replaces.
//
// What is assumed rather than derived.
//
// This is the ceiling, not the steady state. Observation puts real concurrency far below it: peak in-flight
// was 6 at rate 20/s against a millisecond-scale backend and 97 at rate 40/s against a two-second backend
// (same log). The ceiling is used anyway for the same reason the gateway uses one: this setting never opens
// a connection, it only decides whether an already-established one is kept when it goes idle, so it cannot
// push the socket count above the concurrency the process already reached.
func PoolSizeForTrace(trace []TraceRow, timeout time.Duration) int {
	if len(trace) == 0 {
		return http.DefaultMaxIdleConnsPerHost
	}
	// The trace's own offsets are the arrival schedule, so the offered rate is read off them rather than
	// taken from a flag the replayer does not see.
	spanMs := trace[len(trace)-1].OffsetMs - trace[0].OffsetMs
	ceiling := len(trace)
	if spanMs > 0 {
		rate := float64(len(trace)) / (float64(spanMs) / 1000.0)
		ceiling = int(math.Ceil(rate * timeout.Seconds()))
	}
	// A trace fired entirely at one offset, or one whose ceiling exceeds its own length, is bounded by how
	// many requests exist at all.
	ceiling = min(ceiling, len(trace))
	return max(ceiling, http.DefaultMaxIdleConnsPerHost)
}

// NewHTTPSender builds a sender targeting gatewayURL for model, resolving each tenant through apiKeys.
//
// conn selects the connection handling; pass SenderConnForMode(SenderModePooled, ...) for a real run.
// SetPriorities configures the per-tenant scheduling priority the sender attaches to each request.
//
// Keyed by tenant rather than carried on the trace row, because priority is a property of the tenant's
// contract -- the same rule internal/gateway states for tier, that it is "not something a caller can assert
// about itself". A benchmark whose rows named their own priority would measure a mechanism no deployment
// would ship. A tenant absent from the map sends no priority field at all.
func (h *HTTPSender) SetPriorities(p map[string]int) { h.priorities = p }

func NewHTTPSender(gatewayURL, model string, apiKeys map[string]string, timeout time.Duration, conn SenderConn) *HTTPSender {
	return &HTTPSender{
		client: &http.Client{
			// No redirects: the gateway answers directly, and a redirect would point somewhere unmeasured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     newSenderTransport(conn.MaxIdleConnsPerHost),
		},
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		model:      model,
		apiKeys:    apiKeys,
		timeout:    timeout,
		drain:      conn.DrainForReuse,
		now:        time.Now,
	}
}

// maxDrainBytes bounds how much of an unread response body is consumed to make its connection reusable.
//
// After "data: [DONE]" only the stream terminator remains, so the normal case is a handful of bytes; the
// bound is what stops a server that keeps writing from holding the sender open on a body nobody wants.
const maxDrainBytes = 8 << 10 // 8KB

// drainForReuse reads whatever is left of a response body so its connection can return to the idle pool.
//
// This is not an optimization, it is what makes the pool a pool at all. http.Transport returns a connection
// to the idle pool only once the body has been read to EOF; closing a body early marks the connection
// unusable and closes it, so a sized MaxIdleConnsPerHost would have nothing to keep.
//
// The reader breaks out of the stream at "[DONE]" without consuming the terminator, which is correct for
// measurement (the last token is what TTFT and end-time are about) but leaves exactly that unread remainder.
// The "reuses one connection across sequential requests" spec in httpsender_test.go fails without this line,
// observing a fresh dial where the pool should have served, which is the pin holding it in place.
//
// Of the two changes this is the larger one by some way. Measured on one 884-request trace
// (hack/m5b-gateway-path-evidence.log), draining alone took the client from 431 connections to 53, and
// sizing the pool took it from 53 to 6: the drain accounts for 378 of the 425 connections no longer opened.
//
// Read that trace's scope before quoting those numbers: every arm in it recorded ZERO rejections, so they
// describe the all-200 path only. Send's non-200 branch returned without draining until this was noticed, and
// the same method run on an 884-rejection trace measured pooled mode — the mode every real run uses — opening
// 884 connections, one handshake per request, indistinguishable from legacy and worse than the 431 that
// trace's "before" arm reported. Draining the rejection body takes it to 22.
//
// A drain that hits the bound without reaching EOF simply forfeits reuse for that one connection, so the
// failure mode is a lost optimization rather than a wrong number. The request's own deadline bounds it too.
func drainForReuse(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
}

// newSenderTransport returns the one Transport a sender uses for every request it makes.
//
// A nil return means http.Client falls back to http.DefaultTransport, which is what SenderModeLegacy and
// SenderModeDrainOnly select; that path is kept only so the before/after of introducing the pool can be
// measured. It named a GoDefaultConnPool constant that does not exist in this package, which would send an
// operator looking for a fourth mode beside the three the flag actually accepts.
//
// One Transport per sender, and one sender per replay, is what makes the pool a pool: a Transport owns the
// idle-connection pool, so one built per request would pool nothing at all.
func newSenderTransport(maxIdleConnsPerHost int) http.RoundTripper {
	if maxIdleConnsPerHost <= 0 {
		return nil
	}
	// Cloned from http.DefaultTransport rather than built from a zero value, because the arms this sender
	// compares differ ONLY in pooling. A fresh &http.Transport{} silently drops the defaults the other arm
	// runs with — the proxy resolver, the dialer's timeout and keep-alive, TLS handshake timeout, expect-100
	// handling and HTTP/2 — so "before" and "after" would differ in half a dozen ways at once and the measured
	// difference could not be attributed to MaxIdleConnsPerHost.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	// MaxIdleConns is set back to zero, which the clone does NOT bring: http.DefaultTransport caps the total
	// at 100, and a shared total would let traffic to one host evict the connections of another. In a
	// two-tenant noisy-neighbour benchmark that is precisely the coupling the run exists to detect, and the
	// harness must not introduce it itself.
	t.MaxIdleConns = 0
	// MaxConnsPerHost stays zero, as the clone leaves it. That one blocks dispatch when the limit is reached,
	// which would turn this open-loop sender into a closed-loop one and reintroduce the coordinated omission
	// the design goes to some trouble to avoid.
	//
	// IdleConnTimeout keeps the clone's 90s. Zero would mean "never close an idle connection", and a harness
	// that leaks sockets on a long run measures its own file-descriptor pressure.
	return t
}

// chatRequest is the minimal OpenAI chat-completions body the harness sends.
type chatRequest struct {
	Model     string       `json:"model"`
	Messages  []chatReqMsg `json:"messages"`
	MaxTokens int          `json:"max_tokens"`
	Stream    bool         `json:"stream"`
	// StreamOptions asks the engine to append a usage chunk carrying its own count of the prompt.
	//
	// That count is the served tokenizer's, which is the unit the design's admission-match criterion is
	// defined in and the unit no run has ever measured. It arrives on admitted requests only -- a refusal
	// never reaches the engine -- so it checks the trace's per-prompt-length measurement rather than
	// replacing it.
	StreamOptions streamOptions `json:"stream_options"`
	// Priority is vLLM's per-request scheduling priority, omitted entirely when the tenant has none.
	//
	// Lower is more urgent. It is read only under --scheduling-policy=priority, and it is the one axis that
	// moved the tail in the microtest: at a 512-token batch budget it took a short request behind a long one
	// from 13.79x its uncontended time to 1.57x.
	//
	// A pointer so the field disappears from the body rather than defaulting to 0. Sending 0 for every
	// tenant would make every request equally urgent under a policy that reads the field, which is a control
	// arm that is not a control.
	Priority *int `json:"priority,omitempty"`
}

// streamOptions is the OpenAI streaming extension vLLM implements for usage reporting.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatReqMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Send dispatches one streaming request and returns its raw timing result.
//
// A non-200 status maps to a result with that status and an error kind, so admission rejections (429) are recorded as rejections rather than as completed requests; a deadline maps to a timeout; a transport failure maps to a transport error.
func (h *HTTPSender) Send(ctx context.Context, row TraceRow, sendUnixNanos int64) SendResult {
	reqCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	body := chatRequest{
		Model:         h.model,
		Messages:      []chatReqMsg{{Role: "user", Content: PromptText(row.PromptLenChars)}},
		MaxTokens:     row.MaxOutputTokens,
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
	}
	if p, ok := h.priorities[row.Tenant]; ok {
		body.Priority = &p
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return SendResult{ErrorKind: "encode"}
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.gatewayURL+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return SendResult{ErrorKind: "transport"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := h.apiKeys[row.Tenant]; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return SendResult{ErrorKind: "timeout"}
		}
		return SendResult{ErrorKind: "transport"}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drained for the same reason the streaming path drains, and this is the path where it matters most.
		//
		// 429 is not an edge case here, it is the outcome the admission arms exist to produce: an arm that
		// protects capacity does so BY rejecting, so the rejection-heavy runs are exactly the ones whose
		// numbers the pool was introduced to clean up. Returning without reading the body marks the connection
		// unusable, so every rejection cost a fresh handshake and put it back inside the latency the harness
		// reports — reproducing, on the measured path, the instrument artifact drainForReuse's own comment
		// says it removes. The 431-to-6 evidence quoted above cannot have covered this: every arm in that log
		// recorded zero rejections.
		//
		// No ordering care is needed, unlike readStream: a non-200 result carries no EndUnixNanos, because a
		// rejection is counted rather than timed, so there is no measurement here for the drain to fall into.
		if h.drain {
			drainForReuse(resp.Body)
		}
		// Both refusals are admission decisions and belong in the same bucket; labelling only 429 left the
		// report to infer a 413's meaning from a status code, and for one paid run it inferred wrong.
		kind := "http"
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestEntityTooLarge {
			kind = "rejected"
		}
		return h.withAdmissionDecision(resp, SendResult{HTTPStatus: resp.StatusCode, ErrorKind: kind})
	}

	return h.withAdmissionDecision(resp, h.readStream(reqCtx, resp))
}

// withAdmissionDecision copies the gateway's reported tier and refusal reason onto a result.
//
// One function rather than a pair of assignments on each return path, because the path that needs them most
// is the refusal path -- the one a streaming reader never touches, and the one that would be forgotten.
//
// The header names come from internal/gateway rather than string literals here: they are the contract between
// the two packages, and a copy of a contract is how this project has produced most of its defects. Empty when
// the gateway predates reporting them, which every consumer reads as "not recorded" rather than as a value.
func (h *HTTPSender) withAdmissionDecision(resp *http.Response, res SendResult) SendResult {
	res.Tier = resp.Header.Get(gateway.HeaderAdmissionTier)
	res.AdmissionReason = resp.Header.Get(gateway.HeaderAdmissionReason)
	res.BackendState = resp.Header.Get(gateway.HeaderBackendState)
	return res
}

// readStream consumes the server-sent-events response, recording first-token and end times and counting output tokens.
//
// vLLM's OpenAI-compatible stream emits one "data: {json}" line per token chunk and a final "data: [DONE]".
//
// Each chunk carrying a non-empty content delta counts as one output token, which is an approximation the report labels as such; an exact count needs the tokenizer, deferred to the paid run.
func (h *HTTPSender) readStream(ctx context.Context, resp *http.Response) SendResult {
	res := SendResult{HTTPStatus: http.StatusOK}
	// Deferred, so it runs on every return path and always AFTER that path has stamped EndUnixNanos.
	//
	// Ordering is the whole point: draining is bookkeeping for the next request, not part of this one's
	// latency, and stamping after it would fold the drain into the measurement.
	if h.drain {
		defer drainForReuse(resp.Body)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			// The usage chunk arrives last, with an empty choices list, so it contributes no output token.
			Usage *struct {
				PromptTokens int `json:"prompt_tokens"`
				// The engine's own output count, which the frame tally below is not.
				//
				// OutputTokens counts SSE frames carrying content, one per frame. That equals the token
				// count only while the server emits one token per frame, and nothing in the protocol
				// promises it: a server that batched two tokens into a frame would halve every throughput
				// number, every tenant share and every inter-token time this harness reports, without
				// changing a single token the GPU produced. The request already asks for usage, so this
				// count was arriving and being discarded.
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// A malformed chunk mid-stream is a stream error, but any first token already observed still stands.
			res.ErrorKind = "stream"
			res.EndUnixNanos = h.now().UnixNano()
			return res
		}
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				res.PromptTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				res.EngineOutputTokens = chunk.Usage.CompletionTokens
			}
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if res.FirstTokenUnixNanos == 0 {
				res.FirstTokenUnixNanos = h.now().UnixNano()
			}
			res.OutputTokens++
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.ErrorKind = "timeout"
		} else {
			res.ErrorKind = "stream"
		}
	}
	res.EndUnixNanos = h.now().UnixNano()
	return res
}
