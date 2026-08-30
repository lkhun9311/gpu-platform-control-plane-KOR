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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// maxBodyBytes caps how much of a request body readRequestMeta will buffer.
//
// Design rationale (design spec Error codes section): readRequestMeta holds the body in memory to look at the model field, so an unbounded body is a memory-exhaustion vector.
//
// 1MB is far above a normal chat completions request (tens of KB) while keeping a single request unable to threaten the process.
const maxBodyBytes = 1 << 20 // 1MB

// ErrNoModel is the sentinel reported when the body carries no model field.
//
// It mirrors ErrNoPolicy in ratelimit.go and ErrNoRoute in router.go so callers can match on it exactly.
var ErrNoModel = errors.New("request body has no model field")

// RequestMeta is the admission-relevant metadata extracted from a chat-completions request body.
//
// The admission guard (a later task) reads these fields to decide whether to admit a request.
//
// This task only extracts them; nothing here yet changes what gets admitted.
type RequestMeta struct {
	// Model is the requested model name, already validated non-empty by readRequestMeta.
	Model string
	// EstInputTokens is a text-only estimate of the prompt's input tokens: the ceiling of the total prompt byte length divided by 4, never an exact count.
	//
	// It is NOT conservative, which this comment used to claim.
	//
	// The calibration is now measured rather than argued -- internal/bench/testdata/tokenizer_calibration.json, against Qwen2.5's tokenizer and chat template -- and the error changes sign with length. The chat template costs a fixed ~25 tokens, which dominates a short prompt: 200 characters estimate at 50 and measure at 68, so the estimate reads 36 percent LOW. Prose runs about 5.2 characters per token, so at 40,000 characters the same formula estimates 10,000 against a measured 7,695 and reads 30 percent high. A request scored at exactly the 4096 threshold measures 3,171 real tokens.
	//
	// So the guard rejects a band of standard requests whose true input is below its threshold, and under-counts the shortest prompts. Neither is corrected here: the threshold is pre-registered for the M5-b arms, and moving it after measuring is the post-hoc tuning the control arm exists to rule out. Byte length is still what is measured, since it equals the character count for ASCII and over-counts for multibyte UTF-8 -- that part of the original reasoning holds.
	EstInputTokens int
	// NonTextContent is true when at least one message's content was not a plain string.
	//
	// A true value means EstInputTokens is a known-approximate lower bound rather than a byte-accurate estimate, since a non-string part's true token cost cannot be read off its byte length.
	NonTextContent bool
}

// chatMessage is the shape readRequestMeta needs from each entry in messages.
//
// Content stays raw JSON rather than string because the OpenAI schema allows it to be either a plain string or an array of content parts, and readRequestMeta must tell the two apart rather than assume one.
type chatMessage struct {
	Content json.RawMessage `json:"content"`
}

// newRequestID returns a random hex identifier for a single request.
//
// crypto/rand rather than math/rand: math/rand is deterministic from its seed, so separate Pods can emit the same id.
//
// Colliding ids merge unrelated requests when logs are stitched together, which defeats the point of the id.
//
// On the (practically unreachable) error path we fall back to a timestamp-derived value, since a weaker id still beats no id at all.
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// readRequestMeta extracts admission-relevant metadata from the request body and returns a factory that replays the body.
//
// It returns the factory, the metadata, and an error.
//
// The restore is what makes this safe: r.Body is a one-shot stream, so reading it here would leave the proxy nothing to forward and the model server would see an empty body.
//
// The gateway would pass that failure through verbatim, hiding the fact that the gateway caused it.
//
// Why a factory rather than the single restored reader it used to return.
//
// The bytes are already buffered here (they have to be, to estimate input tokens), so handing out any number of readers over them costs nothing beyond the one allocation per call.
//
// That is what lets the caller populate http.Request.GetBody, which http.Transport needs to replay a request onto a fresh connection when it finds a pooled one already closed by the upstream.
//
// The reach is specifically the case where the write failed having sent nothing: shouldRetryRequest allows that retry on req.GetBody != nil alone, whereas every other stale-connection error additionally requires isReplayable, which is false for a POST without an Idempotency-Key (go1.25.7 net/http).
//
// So this narrows the exposure the shared pool introduces rather than removing it, which is worth having because it is free.
//
// maxBodyBytes already bounds the buffer, so nothing here widens the memory exposure the single-reader version had.
//
// Only model and messages[].content are decoded.
//
// The gateway routes on model and estimates admission cost from content, and must forward everything else untouched; modelling the whole schema would mean editing the gateway whenever the OpenAI API grows a field, and would silently drop any field not yet declared.
func readRequestMeta(r *http.Request) (func() (io.ReadCloser, error), RequestMeta, error) {
	// The nil first argument means MaxBytesReader will not write a response itself; the caller owns the status code.
	//
	// Exceeding the cap surfaces as a read error, which the caller maps to 400.
	limited := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, RequestMeta{}, err
	}

	var probe struct {
		Model    string        `json:"model"`
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(buf, &probe); err != nil {
		return nil, RequestMeta{}, err
	}
	if probe.Model == "" {
		return nil, RequestMeta{}, ErrNoModel
	}

	meta := RequestMeta{Model: probe.Model}
	var totalChars int
	for _, m := range probe.Messages {
		// An absent content field, or an explicit JSON null, carries no text and is not a non-text part either.
		if len(m.Content) == 0 || string(m.Content) == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			totalChars += len(text)
			continue
		}
		// content did not decode as a plain string, so it is an array, object, or number: the OpenAI multimodal shape.
		//
		// The true token cost of that part cannot be read off a character count, so flag the estimate as approximate.
		//
		// Its raw JSON byte length is still added, so the estimate is never undercounted, only ever imprecise.
		meta.NonTextContent = true
		totalChars += len(m.Content)
	}
	// Ceiling division, not floor: a conservative estimate must never read lower than the total byte length implies.
	meta.EstInputTokens = (totalChars + 3) / 4

	// Hand back a factory over the bytes just consumed; every reader it produces is byte-for-byte identical to the original.
	//
	// The error return is always nil and exists only to match http.Request.GetBody's signature, so the factory can be assigned to it directly rather than wrapped at the call site.
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }, meta, nil
}

// statusRecorder wraps a ResponseWriter to capture the status code the proxy actually wrote.
//
// http.ResponseWriter offers no way to read back the code, and metrics need it after the response is sent.
type statusRecorder struct {
	http.ResponseWriter
	// code defaults to 200 because a handler that calls Write without WriteHeader implicitly sends 200.
	code int
	// answered distinguishes "the client received 200" from "nothing reached the client and code is still its
	// default". Without it those two are the same value, and a request no backend ever answered is published
	// as a success — which is exactly what happened to every cancelled request.
	answered bool
}

// WriteHeader records the status code and forwards it to the wrapped writer.
func (rec *statusRecorder) WriteHeader(c int) {
	rec.code = c
	rec.answered = true
	rec.ResponseWriter.WriteHeader(c)
}

// Write marks the response answered, because a handler may write a body without ever calling WriteHeader and
// that still means 200 reached the client.
func (rec *statusRecorder) Write(b []byte) (int, error) {
	rec.answered = true
	return rec.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer's Flusher when it has one.
//
// This method is load-bearing (design spec Request flow section): embedding only promotes the methods declared on the embedded interface, and http.ResponseWriter has no Flush.
//
// Without this, the wrapper fails the http.Flusher check that ReverseProxy makes before streaming, and the proxy silently buffers regardless of FlushInterval.
//
// Wrapping the writer would kill streaming on its own.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// errorBody is the OpenAI-style JSON envelope every gateway error response carries.
//
// Design rationale (design spec Pipeline placement section): the design promises an OpenAI-style JSON error contract, but fail() used to write plaintext via http.Error.
//
// A later task adds two new 429 codes as JSON; leaving everything else plaintext would make those two the odd ones out instead of the norm.
type errorBody struct {
	Error errorDetail `json:"error"`
}

// errorDetail carries the machine-readable code and human-readable message inside errorBody.
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorCode maps an HTTP status to the stable code the JSON error body reports.
//
// The mapping is written out explicitly rather than derived from http.StatusText, since a machine-readable code must stay a fixed string even if the status text behind it ever changes wording.
//
// An unmapped status falls back to "internal_error"; every status this gateway actually emits is listed, so the default path is not expected to run.
func errorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "no_policy"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusRequestEntityTooLarge:
		// Added with the two 413 paths and missed at the time, so both of them — an oversized body and an
		// estimate larger than the bucket can ever hold — were answering internal_error. A client branching on
		// the code was told the gateway had broken when it had in fact refused something it can describe, and
		// the one action that resolves either case is the same: send less.
		return "payload_too_large"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusGatewayTimeout:
		return "upstream_timeout"
	case http.StatusServiceUnavailable:
		return "gateway_unavailable"
	default:
		return "internal_error"
	}
}

// writeJSONError writes the OpenAI-style JSON error envelope with the given status and message, deriving the machine-readable code from status via errorCode.
//
// Every gateway failure path whose reason follows directly from its HTTP status (fail() in server.go and the reverse proxy's ErrorHandler below) returns through here, so the response shape stays identical regardless of which pipeline stage rejected the request.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSONErrorCode(w, status, errorCode(status), message)
}

// writeJSONErrorCode writes the OpenAI-style JSON error envelope with an explicit machine-readable code, for a failure whose reason cannot be derived from the HTTP status alone.
//
// The admission guard is exactly that case (design spec Config and API section): static-cap and kv-aware both reject with 429, but carry different codes ("input_rate_limit" vs "kv_cache_pressure"), and errorCode's status-to-code mapping has room for only one string per status.
//
// failReason in server.go is the only caller today.
func writeJSONErrorCode(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding into a fixed struct cannot fail; the error is ignored deliberately rather than handled for a write that has already committed its status code.
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: message}})
}

// defaultResponseHeaderTimeout bounds the wait for upstream response headers in production.
//
// 30s is the compromise: a model server can spend seconds on prefill before the first token, so a short cap severs healthy requests, while an unbounded wait lets hung requests pile up and exhaust connections.
const defaultResponseHeaderTimeout = 30 * time.Second

// maxIdleConnsPerHost caps how many idle connections the shared Transport keeps warm per backend.
//
// Go's default is 2 (http.DefaultMaxIdleConnsPerHost), which would leave the pooling fix half-done for the case it exists to serve.
//
// The M5-b harness dispatches open-loop, so many requests to the one backend are in flight at once; when a wave completes, a cap of 2 closes all but two of those connections and the next wave handshakes again.
//
// That is the same artifact a per-request Transport produced, only smaller, and it would still be measured as backend contention.
//
// Where 600 comes from.
//
// The harness's own defaults bound how many requests can be in flight at once: it offers a mean 20 arrivals per second (cmd/benchharness/main.go, -rate) and cancels any request still outstanding after 30s (-timeout-ms).
//
// Open-loop dispatch never waits for a response (internal/bench/replay.go), so in-flight requests accumulate at the arrival rate and leave only by completing or timing out, giving a hard ceiling of 20/s x 30s = 600 to a single backend host.
//
// All of a run's traffic targets one model, hence one Service host, so that ceiling applies per host rather than being split across hosts.
//
// What is assumed rather than derived.
//
// The steady-state figure is arrival rate times *mean* latency, which is far below this ceiling.
//
// So this is deliberately the ceiling and not an estimate of typical load: it is the point beyond which the cap provably cannot be what closed a reusable connection.
//
// What has since been measured.
//
// This was a derived number with no observation beside it until hack/m5b-gateway-path.sh drove real load through the gateway on kind (hack/m5b-gateway-path-evidence.log).
//
// At the very flag values this constant is derived from, peak concurrency to one backend was 6, and against a deliberately slow two-second backend at double the rate it was 97; the cap was never approached in either regime.
//
// In both, the number of distinct connections the backend accepted equalled peak in-flight exactly, which is the signature of a pool that never evicted a connection it could have reused: were 600 too low, distinct connections would have run ahead of peak in-flight.
//
// So the ceiling reading above is confirmed, and the cap is now known to sit roughly two orders of magnitude above observed load rather than merely being assumed to.
//
// It is left at 600 rather than tuned down to what was observed: the observed figure is a property of one stub's latency, whereas the ceiling is a property of the harness's own flags, and the paragraph below is why an over-provisioned cap costs nothing.
//
// Why the ceiling is safe to use as the cap.
//
// This setting never creates a connection; it only decides whether an already-established one is kept or closed when it goes idle.
//
// So it cannot push the socket count above the peak concurrency the process already sustained, and anything it does keep is released by IdleConnTimeout below.
const maxIdleConnsPerHost = 600

// maxIdleBackends is how many distinct backends may each hold a full idle pool before the total cap binds.
//
// It converts "unbounded" into a number without making the bound reachable in practice: the measured
// topologies have one or two backends per model, so eviction across backends never happens, while a cluster
// that accumulated dead InferenceDeployments can no longer accumulate sockets without limit.
const maxIdleBackends = 8

// newTransport builds the one outbound Transport the whole process shares.
//
// A zero responseHeaderTimeout means "unset" and selects defaultResponseHeaderTimeout.
//
// Zero cannot be passed through as-is: http.Transport reads it as "no limit", which is the exact failure being bounded.
//
// Why exactly one, and why it is safe to share.
//
// A Transport owns the idle-connection pool, so a Transport built per request pools nothing: every request dials a fresh TCP connection and its pool is only reclaimed when the garbage collector gets to the Transport.
//
// Under the open-loop benchmark harness that surfaces as inflated latency and a climbing socket count, which reads exactly like backend contention while being an artifact of the gateway measuring itself.
//
// One Transport serves every backend rather than one per backend because http.Transport is safe for concurrent use and already keys its pool by host, and because responseHeaderTimeout is process configuration that does not vary by backend.
func newTransport(responseHeaderTimeout time.Duration) *http.Transport {
	if responseHeaderTimeout == 0 {
		responseHeaderTimeout = defaultResponseHeaderTimeout
	}
	// Transport tunes the outbound connection.
	//
	// Design rationale (design spec Request flow section): http.DefaultTransport has no response-header timeout, so a model server that accepts a connection and never answers pins the request forever; enough of those exhaust the gateway's connections and memory.
	//
	// There is deliberately no overall Timeout: a healthy stream legitimately runs for minutes, and an overall cap would sever it.
	//
	// Only the wait for the first response header is bounded; the time the body spends streaming is not.
	// Cloned from http.DefaultTransport rather than built from zero values, and that is a correctness point
	// rather than convention. A zero Transport has no DialContext and no TLSHandshakeTimeout, so nothing
	// bounds the time spent REACHING a backend — and ResponseHeaderTimeout below cannot cover it, because it
	// only starts once a connection exists. An unreachable or blackholed model server would then hold a
	// gateway request for the OS-level TCP timeout, minutes rather than the seconds this file's own rationale
	// says it is bounding. A zero value also drops Proxy, so a deployment behind an egress proxy would
	// silently stop using it.
	//
	// Clone carries DefaultTransport's MaxIdleConns of 100, which the block at the bottom of this function
	// argues must not exist here, so it is put back to zero explicitly below rather than inherited.
	t := http.DefaultTransport.(*http.Transport).Clone()

	// ResponseHeaderTimeout bounds the wait for response headers.
	//
	// Exceeding it becomes a timeout error, which newReverseProxy's ErrorHandler maps to 504.
	t.ResponseHeaderTimeout = responseHeaderTimeout

	// IdleConnTimeout keeps connections warm for reuse, avoiding a TCP handshake per request.
	//
	// That property only holds because this Transport outlives the request; see the sharing rationale above.
	t.IdleConnTimeout = 90 * time.Second

	// See maxIdleConnsPerHost for the derivation; the default of 2 would undo most of the reuse this Transport exists to provide.
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost

	// A total cap, sized so it cannot bind in any topology this gateway is measured in.
	//
	// This was zero — unlimited — and the reasoning for that was sound as far as it went: a total cap makes one
	// backend's pool depend on how busy the others are, so a noisy backend could evict the connections of the
	// tenant whose latency is being measured, and that coupling is the exact thing this gateway is instrumented
	// to detect. It must not be introduced by its own connection pool.
	//
	// What the reasoning leaned on was that "the number of backends is bounded by the configured
	// InferenceDeployments", and nothing enforces that. Deployments are created by users, churn leaves dead
	// hosts holding idle sockets for the full IdleConnTimeout, and 600 per host times an unbounded host count
	// is a file-descriptor exhaustion path rather than a bound.
	//
	// maxIdleBackends is the number of distinct backends that can each hold a FULL per-host pool before the
	// total starts evicting. Eight is well past any measured topology — the largest is one model with a head
	// and a spare — so the eviction coupling stays hypothetical while the socket count stays finite.
	//
	// Assigned rather than omitted either way: Clone inherits DefaultTransport's cap of 100, which would bind
	// immediately and silently.
	t.MaxIdleConns = maxIdleConnsPerHost * maxIdleBackends

	return t
}

// newReverseProxy returns a streaming-capable reverse proxy to target over the shared transport, reporting 502/504 through onErr.
//
// The ReverseProxy value itself is still built per request, and deliberately so: target differs per backend and onErr closes over that request's tenant and model labels, while transport is the one piece that must not be rebuilt.
func newReverseProxy(target *url.URL, transport http.RoundTripper, onErr func(code int, err error)) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(target)

	// FlushInterval = -1 means "flush immediately, never buffer".
	//
	// Design rationale (design spec Request flow section): stream:true emits tokens one at a time, and a proxy that batches them hands the client everything only once generation finishes, which defeats streaming.
	//
	// The setting's real reach is narrower than it looks, and it is worth knowing exactly.
	//
	// httputil.ReverseProxy's flushInterval() ignores this value and flushes immediately anyway when:
	//
	//   1. the response Content-Type is text/event-stream, or
	//   2. the response Content-Length is unknown (res.ContentLength == -1, i.e. chunked streaming).
	//
	// Real streaming responses almost always hit one of those, so for them this line changes nothing.
	//
	// It only changes behavior when an upstream declares a Content-Length and still delivers the body in pieces.
	//
	// The "uses the configured flush interval..." spec in proxy_test.go pins exactly that case.
	//
	// It stays despite the narrow reach because relying on Go's auto-detection would leave our intent unstated: this gateway buffers no response, and that intent belongs in the code.
	p.FlushInterval = -1

	p.Transport = transport

	// ErrorHandler runs when the upstream could not be reached.
	//
	// Design rationale (design spec Error codes section): the default handler always writes 502, collapsing "connection refused" and "no response in time" into one code.
	//
	// They are different operational signals: 502 means the backend Pod is gone or the Service is miswired, 504 means it is alive but hung or slow.
	//
	// They call for different responses, so they get different codes.
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Connection refused, DNS failure, and similar land here.
		code := http.StatusBadGateway
		// errors.As walks the error chain, so a timeout wrapped with %w is still detected; a plain type assertion would miss it.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			code = http.StatusGatewayTimeout
		}
		onErr(code, err)
		// The error goes to the LOG; the client gets a fixed sentence.
		//
		// A transport error names what it failed to reach: "dial tcp 10.244.2.18:8090: connect: connection
		// refused" hands a caller a Pod IP and port, and a DNS failure hands them internal Service names. This
		// used to be passed through verbatim, so the gateway published its own cluster topology to anyone who
		// could make a backend fail — and making a backend fail is not hard when one is already unhealthy.
		//
		// Nothing is lost operationally. The status code still separates the two cases the design cares about
		// (502 unreachable, 504 hung), and the detail an operator needs is in the log beside the request id.
		log.FromContext(r.Context()).Error(err, "upstream request failed",
			"code", code, "backend", target.Host, "path", r.URL.Path)
		writeJSONError(w, code, publicUpstreamMessage(code))
	}

	return p
}

// publicUpstreamMessage is what a caller is told when a backend could not answer.
//
// Two sentences rather than one, because the distinction the status code already draws is one a caller can
// act on: a timeout is worth retrying, an unreachable backend usually is not until someone looks at it.
// Neither names an address.
func publicUpstreamMessage(code int) string {
	if code == http.StatusGatewayTimeout {
		return "the model backend did not respond in time"
	}
	return "the model backend could not be reached"
}

// maxBackendAttempts caps how many backends one request may try; see tryBackends for why it is two.
const maxBackendAttempts = 2

// capBackendAttempts returns the prefix of candidates a request can actually reach.
//
// It is exported to the package so the caller can apply the cap once and hand the SAME slice to admission,
// to backend registration and to forwarding. Each stage deriving its own view of "the candidates" is how a
// backend outside the reachable set came to be able to reject a request and to acquire a scraper.
func capBackendAttempts[T any](targets []T) []T {
	if len(targets) > maxBackendAttempts {
		return targets[:maxBackendAttempts]
	}
	return targets
}

// neverReachedBackend reports whether a failed attempt provably delivered nothing to a backend.
//
// This is the only safe basis for replaying a chat-completion POST, and the distinction is not pedantic. A
// completion is not idempotent: it consumes GPU seconds, it is billed, and on a streaming client it can
// produce a second set of tokens. Replaying one that a backend already began serving charges the tenant
// twice for work they asked for once, and nothing downstream can detect that it happened.
//
// A DIAL failure is the case where no bytes were written, because there was never a connection to write them
// to. Everything else — a connection that was established and then broke, a response header timeout, an
// upstream's own 502/503/504 — is consistent with the backend having received the request and started
// generating. "The client saw nothing" is not evidence that the backend did nothing.
//
// Go's own transport draws the same line, retrying only requests it knows were not written; it does not
// export that judgement, so this reconstructs the one case that can be recognised from outside.
func neverReachedBackend(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

// attemptWriter is what lets one request try a second backend without the client seeing the first one fail.
//
// httputil.ReverseProxy has no notion of "try again elsewhere": its ErrorHandler writes a 502 straight to the
// ResponseWriter it was handed. Suppressing that write while candidates remain is the whole mechanism — the
// client is never told about an attempt that another backend went on to serve.
//
// wrote is the safety latch, and it is set on the first byte that actually leaves for the client rather than
// on success. Once a status line or a token is out, a second backend cannot take over: it would splice two
// answers into one response, and for a stream that is a token sequence from two different models. So a
// failure AFTER the first write ends the request, however many candidates are left.
//
// Suppression is keyed on failed rather than applied unconditionally, because a suppressing writer that
// swallowed a SUCCESSFUL response would be a gateway that answers nothing. ErrorHandler runs only after the
// transport gave up and before it writes, so failed is already true by the time the bytes to drop arrive.
type attemptWriter struct {
	http.ResponseWriter
	// scratch holds the headers this attempt sets, and it exists because suppressing the BODY of a failed
	// attempt was never enough. Header() used to return the real writer's live map, shared by every attempt,
	// and ErrorHandler's first act is Header().Set("Content-Type", "application/json"). WriteHeader was
	// suppressed so that map was never committed and never cleared — and ReverseProxy then copies the winning
	// upstream's headers in with Add, not Set. The client received two Content-Type values, application/json
	// first, and any client that branches on it to decide whether to parse SSE mis-handled a valid stream.
	scratch http.Header
	// suppress is true while another candidate remains to try.
	suppress bool
	// failed is set by the proxy's ErrorHandler before it writes its own response.
	failed bool
	// wrote records that bytes reached the client, which forecloses any further attempt.
	wrote bool
	// replayable records that this attempt provably delivered nothing to a backend, which is the only
	// condition under which the same non-idempotent POST may be sent somewhere else. See neverReachedBackend.
	replayable bool
}

// Header returns this attempt's own map, so nothing an abandoned attempt set can reach the client.
//
// Once the attempt has COMMITTED it returns the real writer's map instead, because the scratch map exists to
// isolate an attempt that might still be abandoned and this one no longer can be.
//
// The case that needs it is trailers. ReverseProxy copies the body and then writes the upstream's trailer
// values into rw.Header(); promote runs at WriteHeader or Write, so by then it has already happened, and
// every trailer landed in a map nothing reads again. A streaming completion announcing usage totals in a
// trailer delivered them nowhere.
func (a *attemptWriter) Header() http.Header {
	if a.wrote {
		return a.ResponseWriter.Header()
	}
	if a.scratch == nil {
		a.scratch = make(http.Header)
	}
	return a.scratch
}

// promote copies this attempt's headers onto the real writer, and runs only once the attempt is committing.
func (a *attemptWriter) promote() {
	if a.scratch == nil {
		return
	}
	maps.Copy(a.ResponseWriter.Header(), a.scratch)
	// Released so the guard above makes this run exactly once, which is what the sentence on this function
	// already claimed. Write calls promote unconditionally, so without it a streaming completion paid a
	// maps.Copy of the whole header map per chunk, forever, after the response had already committed.
	//
	// Safe because nothing reads scratch again: Header returns the real writer's map once wrote is set, and
	// wrote is set immediately after this returns on both paths that call it.
	a.scratch = nil
}

func (a *attemptWriter) WriteHeader(c int) {
	if a.failed && a.suppress {
		return
	}
	a.promote()
	a.wrote = true
	a.ResponseWriter.WriteHeader(c)
}

func (a *attemptWriter) Write(b []byte) (int, error) {
	if a.failed && a.suppress {
		// Reported as written so the proxy's own error path completes normally; nothing reaches the client.
		return len(b), nil
	}
	// A Write without a WriteHeader implies 200, so this is the other place an attempt commits.
	a.promote()
	a.wrote = true
	return a.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer, for the reason statusRecorder.Flush exists: embedding promotes only
// the methods on the embedded interface, and a proxy that cannot flush turns a stream into one late blob.
func (a *attemptWriter) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// tryBackends forwards one request to the first backend that answers it.
//
// Extracted from the handler rather than left inline, because the two conditions that make a retry SAFE are
// the whole of this function and neither is observable from outside it: nothing may have reached the client,
// and the request body must be replayable. Inline they were four lines in a four-hundred line handler with no
// way to write a spec against them.
//
// SCOPE, and it is narrower than "the backend is down" suggests. att.failed is set only by ErrorHandler,
// which ReverseProxy calls only when the round trip itself failed — a refused connection, a dial timeout, a
// reset. A backend that ACCEPTS the connection and answers 503 (a model server still loading weights, an
// engine that OOMed, one at capacity) is a successful round trip: the status reaches the client and a healthy
// spare is never tried. "Up but not serving" is the more common shape for a model server, so this covers the
// less likely half.
//
// Closing it means hooking ModifyResponse to turn a retryable status into an error, and the reason it is not
// done here is that the final attempt must then pass the ORIGINAL status through rather than the 502
// ErrorHandler would write — otherwise a genuine 503 reaches the client as a gateway error and the caller
// cannot tell which side failed.
//
// onFailure reports each failed attempt, and its final flag is what separates a failure the client will see
// from one another backend went on to absorb — the two need different counters, or a degraded model and a
// broken one look identical in the metrics.
// The bool reports whether the loop ACTUALLY advanced to another candidate.
//
// The caller cannot infer this from the failure callback. That callback fires before the guards below decide
// whether a retry is possible at all, so a caller latching on it counts a fallback for a request that was
// never retried — on a cancelled request it also leaves the status recorder holding its seeded 200, and a
// failed request is then published as a success. Reporting the fact is the only way the caller can know it.
func tryBackends(w http.ResponseWriter, r *http.Request, targets []*url.URL,
	transport http.RoundTripper, onFailure func(code int, final bool)) (advanced bool) {
	// Attempts are capped, and the cap is not tidiness. Each attempt gets the transport's full
	// ResponseHeaderTimeout plus its dial timeout, and nothing spans the loop — so a model left with four
	// stale InferenceDeployments pointing at blackholed Services would pin a goroutine, a client connection
	// and a slot in the tenant's in-flight budget for minutes where the pre-fallback worst case was one
	// timeout. The blast radius grew with operator sloppiness, and the premise of fallback is that several
	// deployments per model are NORMAL.
	//
	// Two, because the value of a third attempt is small and its cost is another full timeout: if the head and
	// one spare are both unreachable the model is down, and telling the client so quickly is better than
	// walking a list.
	//
	// The caller caps the set before admission sees it, so this is normally a no-op; it stays because
	// tryBackends is called directly by tests and must not depend on a caller having done it.
	targets = capBackendAttempts(targets)
	for i, u := range targets {
		last := i == len(targets)-1
		att := &attemptWriter{ResponseWriter: w, suppress: !last}
		proxy := newReverseProxy(u, transport, func(c int, err error) {
			att.failed = true
			att.replayable = neverReachedBackend(err)
			if onFailure != nil {
				onFailure(c, last)
			}
		})
		// This used to convert an upstream's own 502/503/504 into a retryable failure when another candidate
		// remained, on the reasoning that "up but not serving" is the ordinary shape for a model server and a
		// healthy spare was going untried.
		//
		// It is removed because that reasoning proves the wrong thing. A status is proof the request ARRIVED.
		// A model server answering 503 may have refused it at the door, or may have accepted it, spent GPU
		// seconds on it and failed on the way out; 504 in particular is most likely while generation is still
		// running. Replaying a chat completion in that state bills the tenant twice for one request and can
		// emit a second set of tokens, and no counter anywhere would show it happened. Fallback now covers
		// only the case that can be established from outside — the connection that was never made.
		//
		// What is given up is real: a backend that returns 503 without doing any work now answers the client
		// instead of falling through to a healthy spare. That is the safe direction of the two, and closing it
		// properly needs the backends to state an idempotency contract rather than the gateway to assume one.
		proxy.ServeHTTP(att, r)

		// Answered, or answered partially: either way this request is finished. A failure after the first byte
		// cannot be retried, because a second backend would splice its answer onto the one already in flight.
		if !att.failed || att.wrote || last {
			return advanced
		}
		// The request must provably have reached no backend. Everything below this line replays a POST that
		// consumes GPU time and is billed, so the bar is evidence rather than the absence of evidence.
		if !att.replayable {
			return advanced
		}
		// A client that hung up is not a backend that failed. RoundTrip returns context.Canceled, which is not
		// a net.Error, so ErrorHandler maps it to 502 and the loop would walk every remaining candidate —
		// each cloning the same already-cancelled context and failing at once. One disconnect then produced N
		// upstream errors and N-1 fallbacks, poisoning the very ratio backend_fallbacks_total exists to give.
		// The harness cancels every request outstanding past its timeout, so this is routine rather than exotic.
		if r.Context().Err() != nil {
			return advanced
		}
		// The body has been consumed by the attempt that just failed, so the next one needs it back. GetBody
		// reads from a buffer already in memory; a request without one cannot be replayed and ends here.
		if r.GetBody == nil {
			return advanced
		}
		body, err := r.GetBody()
		if err != nil {
			return advanced
		}
		r.Body = body
		// Past every guard, so the next iteration really is another backend being tried. Set here rather than
		// at the top of the loop: entering the loop is not advancing, and the difference is the whole point.
		advanced = true
	}
	return advanced
}
