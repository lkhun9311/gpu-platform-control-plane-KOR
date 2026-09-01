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

// An internal test: same package as the code under test, so unexported members such as chatCompletions and readModel are reachable, as are router_test.go's newSchemeForTest/policyFor helpers.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// Shared fixture coordinates.
//
// Repeating these literals per spec would let one drift out of step with another and break a lookup silently.
const (
	testGatewayNS = "gw"               // namespace holding the api-keys Secret
	testSecret    = "gateway-api-keys" // api-keys Secret name
	testTenant    = "team-vision"      // tenant the key below resolves to
	testKey       = "k1"               // a working API key
	testTenantNS  = "team-vision-ns"   // the policy's TargetNamespace
	testModel     = "llama-3"          // model the InferenceDeployment serves
)

// newProxyServer wires a Server complete enough to exercise the whole request pipeline: an api-keys Secret mapping "k1" to "team-vision", that tenant's GPUQuotaPolicy carrying TargetNamespace and rateLimit, and an InferenceDeployment serving testModel in that namespace.
//
// rpm is a parameter because only the rate-limit spec should drain its bucket; every other spec must be incapable of hitting a 429.
//
// The backendOverride hook (plan Task 6) exists because backendFor yields an in-cluster DNS address of the form http://<name>.<ns>.svc:<port>, which this process can never reach.
//
// Only the resolved address is swapped for the httptest server; a nil hook leaves the production backendFor path intact.
//
// Burst stays at 1 here, which is what makes the 429 boundary sharp for the rate-limit spec: the first request passes and the next is refused.
//
// Specs that must send several requests back to back go through newProxyServerWithBurst instead, since at burst 1 the second one would be refused before it ever reaches the stage under test.
func newProxyServer(upstream string, rpm int32) *Server {
	return newProxyServerWithBurst(upstream, rpm, 1)
}

// newProxyServerWithBurst is newProxyServer with the token bucket's burst under the caller's control.
func newProxyServerWithBurst(upstream string, rpm, burst int32) *Server {
	// The Secret tenant.go's resolveTenant reads to map a key to a tenant.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testGatewayNS},
		Data:       map[string][]byte{testKey: []byte(testTenant)},
	}
	// policyForTenant matches on Spec.Tenant; backendFor scopes its search by Spec.TargetNamespace.
	policy := &platformv1.GPUQuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "vision-policy"},
		Spec: platformv1.GPUQuotaPolicySpec{
			Tenant:          testTenant,
			TargetNamespace: testTenantNS,
			RateLimit:       &platformv1.GPUQuotaRateLimit{RequestsPerMinute: rpm, Burst: burst},
		},
	}
	// Without this, testModel resolves to no route at all.
	infd := newInfD("llama", testTenantNS, testModel, 8080, time.Now())

	s := &Server{
		Client:       newRouterClient(secret, policy, infd),
		Namespace:    testGatewayNS,
		APIKeySecret: testSecret,
		buckets:      newBucketRegistry(),
	}
	// Only hook when an upstream is given; an empty string leaves the hook nil so the real backendFor runs, which is how the unroutable-address cases are built.
	if upstream != "" {
		u, err := url.Parse(upstream)
		Expect(err).NotTo(HaveOccurred())
		s.backendOverride = func(string) *url.URL { return u }
	}
	// Readiness is orthogonal to the pipeline specs.
	s.markReady()
	return s
}

// authedRequest builds a POST carrying a valid bearer token.
func authedRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	return r
}

// expectJSONError asserts the response carries the OpenAI-style JSON error envelope every gateway failure path now returns.
//
// Design rationale (design spec Pipeline placement section): fail() used to write plaintext via http.Error, and the design promises OpenAI-style JSON errors throughout, not just for the two 429 codes a later task adds.
//
// Asserting both the content type and the decoded shape at each failure site is what pins the contract rather than just the status code.
func expectJSONError(rr *httptest.ResponseRecorder, wantCode string) {
	Expect(rr.Header().Get("Content-Type")).To(Equal("application/json"))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	Expect(json.Unmarshal(rr.Body.Bytes(), &body)).To(Succeed())
	Expect(body.Error.Code).To(Equal(wantCode))
	Expect(body.Error.Message).NotTo(BeEmpty())
}

// Specs for the pipeline's error-code mapping.
//
// The regression these prevent (design spec Error codes section).
//
// The pipeline runs auth, then policy, then rate limit, then body parse, then routing, and each stage's failure must map to a distinct code.
//
// Reorder it (rate limiting ahead of auth, say) and an unauthenticated request can drain someone else's bucket.
//
// Collapse the codes and a caller can no longer tell what to retry from what to fix.
// newTransport had no coverage at all, and was built from a zero http.Transport: no DialContext, no
// TLSHandshakeTimeout, no Proxy. ResponseHeaderTimeout cannot stand in for any of them, because it starts
// only once a connection exists — so nothing bounded the time spent REACHING a blackholed backend, and the
// gateway would hold the request for the OS-level TCP timeout while this file's own rationale claimed to be
// bounding it in seconds.
//
// Mutations that turn these red: build the Transport from &http.Transport{...} again, and DialContext is
// nil; or drop the explicit MaxIdleConns assignment, and Clone's inherited cap of 100 reintroduces exactly
// the cross-backend coupling the pool rationale rules out.
var _ = Describe("the shared outbound transport", func() {
	It("keeps the dial-time safety settings a zero Transport would have dropped", func() {
		t := newTransport(30 * time.Second)
		Expect(t.DialContext).NotTo(BeNil(), "nothing would bound the time spent reaching a backend")
		Expect(t.TLSHandshakeTimeout).To(BeNumerically(">", 0))
		Expect(t.Proxy).NotTo(BeNil(), "a deployment behind an egress proxy would silently stop using it")
	})

	It("keeps its own pool decisions on top of the cloned base", func() {
		t := newTransport(30 * time.Second)
		Expect(t.ResponseHeaderTimeout).To(Equal(30 * time.Second))
		Expect(t.MaxIdleConnsPerHost).To(Equal(maxIdleConnsPerHost))
		// Finite, and high enough that it cannot bind in any measured topology.
		//
		// This asserted 0 — unlimited — on the reasoning that a total cap makes one backend's pool depend on
		// how busy the others are, which is the coupling this gateway exists to detect. That reasoning holds
		// and rested on an unenforced premise: "the number of backends is bounded by the configured
		// InferenceDeployments" is not a bound, since users create them and churn leaves dead hosts holding
		// sockets for the full idle timeout.
		//
		// Both properties are kept by sizing the cap past any topology this is measured in: eight backends can
		// each hold a full per-host pool before eviction begins, and the largest measured case is one model
		// with a head and a spare.
		//
		// Mutation that turns this red: restore MaxIdleConns = 0, or drop the assignment so Clone's inherited
		// 100 binds silently.
		Expect(t.MaxIdleConns).To(Equal(maxIdleConnsPerHost*maxIdleBackends),
			"an unbounded idle pool is a file-descriptor exhaustion path, not a bound")
		Expect(t.MaxIdleConns).To(BeNumerically(">", maxIdleConnsPerHost),
			"the total cap must not bind before a single backend's own pool is full")
	})

	It("falls back to the default response-header timeout when given zero", func() {
		Expect(newTransport(0).ResponseHeaderTimeout).To(Equal(defaultResponseHeaderTimeout))
	})
})

var _ = Describe("chat completions pipeline", func() {
	It("returns 405 for a non-POST method on the completions path", func() {
		// Right path, wrong method.
		//
		// A mux that ignores the method sends this into the pipeline, where it becomes a 401 or 400 and fails this spec.
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
		Expect(rr.Code).To(Equal(http.StatusMethodNotAllowed))
	})

	It("returns 404 for an unknown path", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil))
		Expect(rr.Code).To(Equal(http.StatusNotFound))
	})

	It("returns 401 when the bearer token is missing", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		// Deliberately no Authorization header.
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama-3"}`))
		s.Handler().ServeHTTP(rr, r)
		Expect(rr.Code).To(Equal(http.StatusUnauthorized))
		expectJSONError(rr, "unauthorized")
	})

	It("returns 401 when the bearer token is unknown", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama-3"}`))
		// A key absent from the Secret: auth must turn on existence, not shape.
		r.Header.Set("Authorization", "Bearer nope")
		s.Handler().ServeHTTP(rr, r)
		Expect(rr.Code).To(Equal(http.StatusUnauthorized))
		expectJSONError(rr, "unauthorized")
	})

	It("returns 403 when the tenant has no policy", func() {
		// A tenant that authenticates but has no GPUQuotaPolicy planted for it.
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testGatewayNS},
			Data:       map[string][]byte{"k2": []byte("no-policy-tenant")},
		}
		s := &Server{
			Client:       newRouterClient(secret),
			Namespace:    testGatewayNS,
			APIKeySecret: testSecret,
			buckets:      newBucketRegistry(),
		}
		s.markReady()
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama-3"}`))
		r.Header.Set("Authorization", "Bearer k2")
		s.Handler().ServeHTTP(rr, r)
		// 403, not 401: identity was established and authorization was not.
		Expect(rr.Code).To(Equal(http.StatusForbidden))
		expectJSONError(rr, "no_policy")
	})

	It("returns 429 once the tenant bucket is exhausted", func() {
		// The upstream's response is irrelevant here.
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		// rpm=1, burst=1: the first request drains the bucket, and refilling once a minute leaves no chance of the next one slipping through.
		s := newProxyServer(up.URL, 1)

		first := httptest.NewRecorder()
		s.Handler().ServeHTTP(first, authedRequest(`{"model":"llama-3"}`))
		Expect(first.Code).To(Equal(http.StatusOK)) // a token was available

		second := httptest.NewRecorder()
		s.Handler().ServeHTTP(second, authedRequest(`{"model":"llama-3"}`))
		Expect(second.Code).To(Equal(http.StatusTooManyRequests)) // none left
		expectJSONError(second, "rate_limited")
	})

	It("returns 400 for a malformed JSON body", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		// Unterminated JSON.
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":`))
		Expect(rr.Code).To(Equal(http.StatusBadRequest))
		expectJSONError(rr, "bad_request")
	})

	It("returns 400 when the model field is missing", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		// Valid JSON, no model key.
		//
		// Routing depends entirely on it, so there is nowhere to go.
		s.Handler().ServeHTTP(rr, authedRequest(`{"messages":[]}`))
		Expect(rr.Code).To(Equal(http.StatusBadRequest))
		expectJSONError(rr, "bad_request")
	})

	It("returns 404 for an unknown model", func() {
		// No hook, so the real backendFor runs and the name matches no planted InferenceDeployment.
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"does-not-exist"}`))
		Expect(rr.Code).To(Equal(http.StatusNotFound))
		expectJSONError(rr, "model_not_found")
	})

	It("proxies the upstream status and body on the happy path and sets X-Request-Id", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The upstream must receive the id too; setting it only on the response makes logs unstitchable.
			Expect(r.Header.Get("X-Request-Id")).NotTo(BeEmpty())
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))

		// The upstream's status must pass through rather than be flattened to 200.
		Expect(rr.Code).To(Equal(http.StatusCreated))
		Expect(rr.Body.String()).To(Equal(`{"id":"chatcmpl-1"}`))
		Expect(rr.Header().Get("X-Request-Id")).NotTo(BeEmpty())
	})

	It("reuses an inbound X-Request-Id instead of generating a new one", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		rr := httptest.NewRecorder()
		r := authedRequest(`{"model":"llama-3"}`)
		r.Header.Set("X-Request-Id", "caller-supplied-id")
		s.Handler().ServeHTTP(rr, r)

		// A caller that already carries an id keeps it, or the trace breaks.
		Expect(rr.Header().Get("X-Request-Id")).To(Equal("caller-supplied-id"))
	})

	It("returns 502 when the upstream refuses the connection", func() {
		// Start and immediately close to obtain an address nobody listens on, so the dial is refused.
		up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := up.URL
		up.Close()
		s := newProxyServer(addr, 600)

		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))
		// 502: the gateway is fine, the upstream is unreachable.
		Expect(rr.Code).To(Equal(http.StatusBadGateway))
		// This body is written by proxy.go's ErrorHandler, not fail(), so it pins that path emits the same JSON envelope too.
		expectJSONError(rr, "bad_gateway")
	})

	// This spec pairs with the 502 one to pin both branches of proxy.go's ErrorHandler.
	//
	// Why 502 alone is not enough (design spec Error codes section).
	//
	// Deleting the timeout check (errors.As + ne.Timeout()) entirely leaves the 502 default in place, so the spec above still passes.
	//
	// The 504 mapping is therefore a behavior that can vanish silently.
	//
	// Operationally the two are different signals (502 means the backend is gone, 504 means it is alive but not answering), and losing the distinction sends incident response after the wrong thing.
	It("returns 504 when the upstream does not send response headers in time", func() {
		// An upstream that accepts the connection but sends no response headers: exactly what 504 denotes.
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			// Write nothing and hold.
			//
			// Returning would let Go write a 200 instead.
			//
			// The time.After bound is here for the same reason as in the streaming specs: if the test fails and release is never closed, this goroutine would keep up.Close() from returning and hang the whole suite.
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}))
		// Deferred calls run last-registered-first, so release closes and frees the handler before up.Close() runs and blocks on it.
		defer up.Close()
		defer close(release)

		s := newProxyServer(up.URL, 600)
		// The 30s production default would cost this one spec 30 seconds; a short bound in tests only drives the identical code path immediately.
		s.responseHeaderTimeout = 50 * time.Millisecond

		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))
		// The connection succeeded but no header arrived in time, so this is 504, not 502.
		Expect(rr.Code).To(Equal(http.StatusGatewayTimeout))
		expectJSONError(rr, "upstream_timeout")
	})
})

// Streaming specs.
//
// The regression these prevent (design spec Request flow section).
//
// stream:true emits tokens as they are generated, and a proxy that buffers hands the client everything only once generation completes, so the full latency is felt.
//
// Status-and-body assertions cannot catch this, because the final result is identical either way: only the timing differs.
//
// So these specs assert the timing directly.
var _ = Describe("streaming", func() {
	It("delivers upstream chunks progressively instead of buffering them", func() {
		// release lets the test decide when the upstream may send its second chunk.
		//
		// A struct{} channel carries no value; it signals the event itself.
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Write the first chunk and push it out immediately.
			_, _ = fmt.Fprintf(w, "data: chunk-1\n\n")
			w.(http.Flusher).Flush()
			// Block until the test confirms the first chunk arrived.
			//
			// This wait is the whole spec: if the proxy buffers, the client never sees chunk-1 and never closes release.
			//
			// The time.After arm exists so a failing test does not strand this goroutine here, which would hang httptest's Close and with it the suite.
			//
			// Failures should be fast and loud.
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return
			}
			_, _ = fmt.Fprintf(w, "data: chunk-2\n\n")
			w.(http.Flusher).Flush()
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		// A real server, not httptest.NewRecorder: a recorder is an in-memory buffer with no notion of when bytes arrived, so it cannot observe streaming at all.
		//
		// Only a real connection can show that the first chunk lands before the handler returns.
		gw := httptest.NewServer(s.Handler())
		defer gw.Close()

		// A deadline on the request, so buffering fails the spec instead of hanging it: without one the read below simply blocks forever and stalls the suite.
		//
		// With it, the read breaks at the deadline and the failure is immediate and unambiguous.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama-3","stream":true}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		// Reading the first line while the upstream still holds the second chunk, and its handler has not returned, is precisely the evidence that bytes flowed without buffering.
		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(line).To(Equal("data: chunk-1\n"))

		// The first chunk landed, so let the upstream continue.
		close(release)

		// The second must follow, showing the stream was not severed mid-flight.
		Eventually(func() string {
			l, _ := reader.ReadString('\n')
			return l
		}, 5*time.Second).Should(Or(Equal("\n"), Equal("data: chunk-2\n")))
	})

	// This spec, and only this one, pins p.FlushInterval = -1.
	//
	// Why the SSE spec above cannot.
	//
	// httputil.ReverseProxy's flushInterval() overrides the configured value and flushes immediately whenever
	//
	//   1. the response Content-Type is text/event-stream, or
	//   2. the response Content-Length is unknown (res.ContentLength == -1, i.e. chunked streaming).
	//
	// Real streaming responses always hit one of those, so with such a response the spec above passes even with p.FlushInterval reset to 0 (verified).
	//
	// It pins that statusRecorder does not mask Flusher, but not the interval itself.
	//
	// So this builds the one case where the setting is actually consulted: an upstream that declares a Content-Length and still delivers the body in pieces.
	//
	// Neither override applies, so p.FlushInterval governs, and at 0 the proxy batches the body and the first chunk misses its deadline.
	It("uses the configured flush interval when the upstream declares a content length", func() {
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// A declared Content-Length plus a non-SSE Content-Type: both are needed for ReverseProxy to consult our setting rather than override it.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "16") // "chunk-1\n" + "chunk-2\n"
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "chunk-1\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return
			}
			_, _ = fmt.Fprintf(w, "chunk-2\n")
			w.(http.Flusher).Flush()
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		gw := httptest.NewServer(s.Handler())
		defer gw.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama-3","stream":true}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		// At any interval other than -1 the first chunk stays held until the upstream handler returns, and the upstream is waiting on release, so this read hits the deadline and fails.
		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(line).To(Equal("chunk-1\n"))

		close(release)
	})
})

// Pins the exposed metric names to the design contract.
//
// The regression this prevents (docs/05 Minimum metrics section).
//
// docs/05 pins the four series to the gpuaas_gateway_ prefix.
//
// Those names are a contract, not a label: dashboards and alert rules query the exact strings.
//
// Get the prefix wrong and the gateway runs perfectly and every test still passes; it surfaces only in production as "the graphs are empty", by which point it is too late.
//
// Before this spec existed the implementation used a gateway_ prefix and all 33 specs passed.
//
// So the doc's names are transcribed here verbatim, and the code failing to match the doc now fails immediately.
var _ = Describe("metric names", func() {
	It("exposes exactly the series docs/05 pins", func() {
		// Drive three real paths so all four series carry a value.
		//
		// The traffic is necessary: a labelled CounterVec/HistogramVec appears in a scrape only once a label combination has actually been used.
		//
		// Registration alone exposes no name.

		// (1) A 200 -> requests_total, request_duration_seconds
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		ok := newProxyServer(up.URL, 600)
		ok.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))

		// (2) A 429 -> rate_limited_total
		limited := newProxyServer(up.URL, 1)
		limited.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))
		limited.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))

		// (3) A 502 -> upstream_errors_total
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := dead.URL
		dead.Close()
		broken := newProxyServer(addr, 600)
		broken.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))

		// Scrape /metrics on :8081 for real.
		rr := httptest.NewRecorder()
		ok.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		Expect(rr.Code).To(Equal(http.StatusOK))
		body := rr.Body.String()

		// Transcribed from the Minimum metrics section of docs/05.
		//
		// Changing the code without changing the doc fails right here.
		Expect(body).To(ContainSubstring("gpuaas_gateway_requests_total"))
		Expect(body).To(ContainSubstring("gpuaas_gateway_request_duration_seconds_bucket"))
		Expect(body).To(ContainSubstring("gpuaas_gateway_rate_limited_total"))
		Expect(body).To(ContainSubstring("gpuaas_gateway_upstream_errors_total"))
	})
})

// Pins that the gateway pools its outbound connections instead of dialling one per request.
//
// The regression this prevents.
//
// A ReverseProxy's Transport owns the idle-connection pool, so building the Transport inside the request handler makes every request a fresh TCP handshake and leaves the pool unreachable until the garbage collector reclaims it.
//
// Nothing about the response changes, so every status-and-body spec above passes either way: the only visible difference is latency and socket count.
//
// That is precisely the failure mode that matters here, because the M5-b harness reads gateway latency as evidence of backend contention, and a per-request dial would be measured as contention that the gateway invented.
var _ = Describe("outbound connection pooling", func() {
	It("reuses upstream connections across requests rather than dialling one per request", func() {
		// StateNew fires once per accepted connection, so this counts dials rather than requests.
		//
		// The counter is guarded because httptest's ConnState runs on the server's own goroutines.
		var mu sync.Mutex
		dials := 0

		// Unstarted, so ConnState is installed before the accept loop can read it; setting it on an already-running server is a data race that -race would flag.
		up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state != http.StateNew {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			dials++
		}
		up.Start()
		defer up.Close()

		// One Server for every request, since the pool it is meant to keep lives on the Server.
		//
		// A burst wide enough that the limiter cannot reject any of these, or the spec would measure the limiter instead.
		const requests = 5
		s := newProxyServerWithBurst(up.URL, 6000, requests)

		for range requests {
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))
			Expect(rr.Code).To(Equal(http.StatusOK))
		}

		mu.Lock()
		defer mu.Unlock()
		// Strictly fewer dials than requests is the whole claim; the requests are sequential and the backend is one host, so in practice this is 1.
		Expect(dials).To(BeNumerically("<", requests))
	})

	// Pins that the pool survives a whole concurrent wave, not just the two connections Go keeps by default.
	//
	// The regression this prevents.
	//
	// MaxIdleConnsPerHost defaults to 2, so with a shared Transport but no cap raised, a wave of concurrent requests opens many connections and then closes all but two the moment they go idle.
	//
	// The next wave handshakes again, which is the same artifact a per-request Transport produced, only smaller, and it would still be read as backend contention by a harness that dispatches open-loop.
	//
	// Why the upstream holds every request until the whole wave has arrived.
	//
	// Without that barrier the wave is free to serialise: a few connections would serve it by reuse, the pool would never exceed the default cap, and the spec would pass at any cap at all.
	//
	// Blocking until all of them are in flight forces exactly perWave connections open, so the second wave measures whether they were kept.
	It("keeps a whole concurrent wave of connections warm for the next wave", func() {
		const perWave = 12

		var mu sync.Mutex
		dials := 0
		// arrived reports each request reaching the upstream; release is swapped per wave to free them all at once.
		arrived := make(chan struct{}, perWave)
		release := make(chan struct{})

		up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			gate := release
			mu.Unlock()
			arrived <- struct{}{}
			<-gate
			w.WriteHeader(http.StatusOK)
		}))
		up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state != http.StateNew {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			dials++
		}
		up.Start()
		defer up.Close()

		s := newProxyServerWithBurst(up.URL, 600_000, perWave*2)
		h := s.Handler()

		for range 2 {
			mu.Lock()
			release = make(chan struct{})
			gate := release
			mu.Unlock()

			var wg sync.WaitGroup
			codes := make([]int, perWave)
			for i := range perWave {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					rr := httptest.NewRecorder()
					h.ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))
					codes[i] = rr.Code
				}(i)
			}
			// Every request is now in flight at the upstream, so the wave genuinely holds perWave connections at once.
			for range perWave {
				<-arrived
			}
			close(gate)
			wg.Wait()
			// Asserted on the spec's own goroutine, since a Gomega failure inside a worker would not be attributed correctly.
			for i, c := range codes {
				Expect(c).To(Equal(http.StatusOK), "request %d was not proxied", i)
			}
		}

		mu.Lock()
		defer mu.Unlock()
		// The second wave must have been served entirely from the pool: at the default cap of 2 it would have had to dial perWave-2 more.
		Expect(dials).To(Equal(perWave))
	})

	// Pins that the gateway hands the Transport a request it is able to replay.
	//
	// Why this matters once the Transport is shared.
	//
	// Pooling introduces a race a per-request Transport could not have: the upstream may close an idle connection just as the gateway reuses it.
	//
	// When that happens before any byte is written, http.Transport retries on a fresh connection, but only if it can rewind the body: shouldRetryRequest returns req.outgoingLength() == 0 || req.GetBody != nil for a nothingWrittenError (go1.25.7 net/http/transport.go).
	//
	// A server-side request never carries a GetBody of its own, so unless chatCompletions supplies one, that retry is refused and the caller is told the backend failed when nothing was ever sent to it.
	//
	// Supplying it is free here because readRequestMeta has already buffered the whole body to estimate input tokens.
	//
	// What this spec deliberately does not claim.
	//
	// It does not claim pooling can no longer produce a 502.
	//
	// Go only treats a POST as replayable at all when it carries an Idempotency-Key header (isReplayable in net/http/request.go), so the stale-connection paths where bytes did reach the wire (errServerClosedIdle, transportReadFromServerError) stay unretried whatever GetBody says.
	//
	// GetBody narrows the exposure to the subset Go will act on; it does not close it, and a spec asserting otherwise would be asserting something false.
	//
	// chatCompletions is called directly rather than through Handler() so the assertion reads the very request the pipeline mutated, with no mux in between deciding whether to hand the handler the same *http.Request.
	It("gives the forwarded request a rewindable body", func() {
		const body = `{"model":"llama-3","messages":[{"role":"user","content":"hi"}]}`

		// The upstream records what it received, so this also pins that Body itself was restored rather than merely GetBody being set.
		var upstreamGot string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			upstreamGot = string(b)
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		r := authedRequest(body)
		rr := httptest.NewRecorder()
		s.chatCompletions(rr, r)
		Expect(rr.Code).To(Equal(http.StatusOK))
		Expect(upstreamGot).To(Equal(body))

		// The rewind the Transport would perform, performed here instead.
		Expect(r.GetBody).NotTo(BeNil())
		rewound, err := r.GetBody()
		Expect(err).NotTo(HaveOccurred())
		replayed, err := io.ReadAll(rewound)
		Expect(err).NotTo(HaveOccurred())
		// Byte-for-byte, or a retried request would quietly send something other than what the caller wrote.
		Expect(string(replayed)).To(Equal(body))
	})
})

// Pins that an unresolved model never becomes a Prometheus label.
//
// The regression this prevents (design spec Observability section, "Unbounded-cardinality values ... are never labels").
//
// On the 404 and 502 routing paths the model name is an arbitrary string out of the request body, so labelling requests_total with it lets one authenticated tenant mint a new time series per request.
//
// Counter series are never reclaimed, so this degrades /metrics and the scraping Prometheus until both fall over, and no status-code spec can see it happening.
var _ = Describe("metric label cardinality", func() {
	It("records unresolved models under one sentinel series instead of one series per requested name", func() {
		// Two names no other spec uses, so finding either in the scrape can only mean this path put it there.
		const probeA = "cardinality-probe-a"
		const probeB = "cardinality-probe-b"

		// No upstream hook, so the real backendFor runs and neither name matches a planted InferenceDeployment.
		s := newProxyServerWithBurst("", 6000, 4)

		before := testutil.ToFloat64(requests.WithLabelValues(testTenant, unresolvedModelLabel, "404"))

		// Bodies are collected rather than asserted in the loop, so the cardinality claims below are what a regression trips on first.
		bodies := map[string]string{}
		for _, model := range []string{probeA, probeB} {
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, authedRequest(`{"model":"`+model+`"}`))
			Expect(rr.Code).To(Equal(http.StatusNotFound))
			bodies[model] = rr.Body.String()
		}

		// One series moving by two, rather than two series moving by one each.
		after := testutil.ToFloat64(requests.WithLabelValues(testTenant, unresolvedModelLabel, "404"))
		Expect(after - before).To(Equal(2.0))

		// And a real scrape, which is the thing Prometheus would actually ingest, carries neither name anywhere.
		rr := httptest.NewRecorder()
		s.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		Expect(rr.Code).To(Equal(http.StatusOK))
		body := rr.Body.String()
		Expect(body).NotTo(ContainSubstring(probeA))
		Expect(body).NotTo(ContainSubstring(probeB))

		// The name is kept out of the label set, not thrown away: the caller is still told which model it asked for.
		Expect(bodies[probeA]).To(ContainSubstring(probeA))
		Expect(bodies[probeB]).To(ContainSubstring(probeB))
	})
})

// Specs for readRequestMeta itself.
//
// Separate from the handler specs because those only observe a 400, never whether a consumed body is handed on intact, nor the RequestMeta fields the admission guard (a later task) reads.
//
// readRequestMeta consumes the body to peek at model and messages, and failing to restore it leaves the upstream with an empty one: a bug the proxy's 200 would hide from every spec above.
var _ = Describe("readRequestMeta", func() {
	It("restores the body so the upstream still receives it intact", func() {
		body := `{"model":"llama-3","messages":[{"role":"user","content":"hi"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

		restore, meta, err := readRequestMeta(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.Model).To(Equal("llama-3"))

		// The restored body must match the original byte for byte.
		readAll := func() string {
			rc, err := restore()
			Expect(err).NotTo(HaveOccurred())
			b, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			return string(b)
		}
		Expect(readAll()).To(Equal(body))

		// And again, from a second reader over the same buffer.
		//
		// This is the property the factory sells to http.Request.GetBody: a request the Transport rewinds must carry the identical body the first attempt did, or a retry would silently send something else.
		Expect(readAll()).To(Equal(body))
	})

	It("rejects a body that exceeds the size limit", func() {
		// A body past maxBodyBytes.
		//
		// Why the cap matters (design spec Error codes section).
		//
		// Without it a malicious client can exhaust gateway memory, and readRequestMeta (which buffers the body to find model and messages) is exactly that vector.
		big := `{"model":"llama-3","pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))

		_, _, err := readRequestMeta(r)
		Expect(err).To(HaveOccurred())

		// The type matters as much as the error. Every readRequestMeta failure used to reach the client as 400,
		// which tells a caller their JSON is malformed when the JSON was fine and only the size was wrong; they
		// would look for a syntax bug that is not there. MaxBytesReader reports the case distinguishably for
		// exactly this reason.
		//
		// Mutation that turns this red: map every readRequestMeta error to 400 again.
		var tooLarge *http.MaxBytesError
		Expect(errors.As(err, &tooLarge)).To(BeTrue(),
			"an oversized body is indistinguishable from malformed JSON, so it cannot be answered 413")
	})

	It("rejects a body with no model field", func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))

		_, _, err := readRequestMeta(r)
		Expect(err).To(MatchError(ErrNoModel))
	})

	It("estimates input tokens as the ceiling of total string content length over 4", func() {
		// "hi" is 2 chars and "hello!" is 6 chars: 8 total, an exact multiple of 4, so ceiling and floor agree here.
		body := `{"model":"llama-3","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello!"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

		_, meta, err := readRequestMeta(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.EstInputTokens).To(Equal(2))
		Expect(meta.NonTextContent).To(BeFalse())
	})

	It("rounds the estimate up rather than truncating it", func() {
		// 5 chars / 4 = 1.25; a conservative estimate must read 2, not 1, or a prompt near a threshold is undercounted.
		body := `{"model":"llama-3","messages":[{"role":"user","content":"abcde"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

		_, meta, err := readRequestMeta(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.EstInputTokens).To(Equal(2))
	})

	It("flags non-text content and still counts its raw bytes toward the estimate", func() {
		// content is an array of parts, the OpenAI multimodal shape, not a plain string.
		body := `{"model":"llama-3","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

		_, meta, err := readRequestMeta(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.NonTextContent).To(BeTrue())
		// The part is never dropped from the estimate, only imprecisely counted, so it must contribute a positive amount.
		Expect(meta.EstInputTokens).To(BeNumerically(">", 0))
	})

	It("treats a missing or null content field as contributing nothing, without flagging it as non-text", func() {
		body := `{"model":"llama-3","messages":[{"role":"assistant","tool_calls":[]},{"role":"user","content":null}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

		_, meta, err := readRequestMeta(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.EstInputTokens).To(Equal(0))
		Expect(meta.NonTextContent).To(BeFalse())
	})
})

// A compile-time assertion: if the type stops satisfying the interface, this fails to build.
var _ client.Object = &platformv1.InferenceDeployment{}

// A dead backend must not become a dead request while another backend serves the same model.
//
// backendsFor used to log a second deployment as an operator problem and discard it, so a model with a spare
// still failed whenever the one chosen backend was down. The spare is now the fallback, and these specs pin
// the two conditions that make retrying safe rather than merely useful.
var _ = Describe("backend fallback", func() {
	// A body the second attempt can replay. This is what r.GetBody was installed for one stage earlier, and
	// without it there is no second attempt to make.
	post := func(url string) *http.Request {
		r, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"model":"m","messages":[]}`))
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m","messages":[]}`)
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		r.Body, _ = r.GetBody()
		return r
	}

	It("serves the request from the next backend when the first refuses the connection", func() {
		var served int
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			body, _ := io.ReadAll(r.Body)
			// The replayed body must arrive intact, or the second backend answers a different question.
			Expect(string(body)).To(ContainSubstring(`"model":"m"`))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer up.Close()

		// A listener that is closed, so dialling it fails at once rather than hanging.
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL, err := url.Parse(dead.URL)
		Expect(err).NotTo(HaveOccurred())
		dead.Close()
		liveURL, err := url.Parse(up.URL)
		Expect(err).NotTo(HaveOccurred())

		rec := httptest.NewRecorder()
		req := post("http://gw/v1/chat/completions")
		tryBackends(rec, req, []*url.URL{deadURL, liveURL}, http.DefaultTransport, nil)

		Expect(served).To(Equal(1), "the live backend never saw the request")
		Expect(rec.Code).To(Equal(http.StatusOK), "the client was told about an attempt that another backend served")
		Expect(rec.Body.String()).To(ContainSubstring("[DONE]"))

		// The failed attempt's headers must not survive into the winning response. ErrorHandler sets
		// Content-Type: application/json before it writes, and ReverseProxy copies the upstream's headers in
		// with Add rather than Set — so a shared header map hands the client two Content-Type values with
		// application/json first, and anything that branches on it to decide whether to parse SSE mis-handles
		// a valid stream. Body and status alone do not catch this; only the header does.
		//
		// Mutation that turns this red: delete attemptWriter.Header, so every attempt shares the real map.
		ct := rec.Result().Header.Values("Content-Type")
		Expect(ct).To(HaveLen(1), "the abandoned attempt's Content-Type survived into the served response")
		Expect(ct[0]).To(ContainSubstring("text/event-stream"))
	})

	// Mutation that turns this red: drop the att.wrote check from the fallback loop's break condition.
	It("does not retry once bytes have reached the client", func() {
		var served int
		flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			// The upstream dies mid-stream; the client already holds a token.
			panic(http.ErrAbortHandler)
		}))
		defer flaky.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served += 100
			_, _ = fmt.Fprint(w, "data: second\n\n")
		}))
		defer spare.Close()
		flakyURL, _ := url.Parse(flaky.URL)
		spareURL, _ := url.Parse(spare.URL)

		rec := httptest.NewRecorder()
		tryBackends(rec, post("http://gw/v1/chat/completions"), []*url.URL{flakyURL, spareURL}, http.DefaultTransport, nil)

		Expect(served).To(Equal(1),
			"the spare was tried after the client already held a token, which splices two answers into one response")
		Expect(rec.Body.String()).To(ContainSubstring("first"))
		Expect(rec.Body.String()).NotTo(ContainSubstring("second"))
	})
})

// A client that hung up is not a backend that failed.
//
// RoundTrip returns context.Canceled, which is not a net.Error, so ErrorHandler maps it to 502 and the loop
// would walk every remaining candidate — each cloning the same already-cancelled context and failing at once.
// One disconnect then produced N upstream errors and N-1 fallbacks, poisoning the ratio the fallback counter
// exists to provide. The harness cancels every request outstanding past its timeout, so this is routine.
//
// Mutation that turns this red: delete the r.Context().Err() guard from tryBackends.
var _ = Describe("fallback and the client's own cancellation", func() {
	It("does not walk the candidate list after the client has gone", func() {
		// Atomic because the handlers run on the server's own goroutines while the spec body reads the count;
		// a plain int here is a data race that -race reports, and a counter two goroutines disagree about is
		// not evidence of anything.
		var hits atomic.Int64
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			<-r.Context().Done()
		}))
		defer slow.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(100)
		}))
		defer spare.Close()
		slowURL, _ := url.Parse(slow.URL)
		spareURL, _ := url.Parse(spare.URL)

		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		go func() { time.Sleep(120 * time.Millisecond); cancel() }()
		var failures int
		tryBackends(httptest.NewRecorder(), req, []*url.URL{slowURL, spareURL}, http.DefaultTransport,
			func(int, bool) { failures++ })

		Expect(hits.Load()).To(Equal(int64(1)), "the spare was tried after the client had already gone")
		Expect(failures).To(Equal(1), "one disconnect was counted as several upstream failures")
	})

	// The failure callback fires BEFORE the guards decide whether a retry is possible, so a caller latching on
	// it counts a fallback that never happened. tryBackends reporting whether it actually advanced is the only
	// thing that can tell the two apart, and a cancelled request is the case where they differ.
	//
	// Mutation that turns this red: set advanced = true at the top of the loop instead of past the guards.
	It("reports that it did not advance when the client hung up", func() {
		var hits int
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(400 * time.Millisecond)
		}))
		defer slow.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusOK)
		}))
		defer spare.Close()
		slowURL, _ := url.Parse(slow.URL)
		spareURL, _ := url.Parse(spare.URL)

		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		go func() { time.Sleep(120 * time.Millisecond); cancel() }()
		advanced := tryBackends(httptest.NewRecorder(), req, []*url.URL{slowURL, spareURL},
			http.DefaultTransport, func(int, bool) {})

		Expect(advanced).To(BeFalse(), "a cancelled request reported a fallback that never happened")
		Expect(hits).To(Equal(0), "the spare was tried after the client had already gone")
	})

	// The control. Without it, always returning false would satisfy the spec above and the counter would stop
	// recording the fallbacks that do happen.
	//
	// Mutation that turns this red: return false unconditionally.
	It("reports that it advanced when a spare really served the request", func() {
		var hits int
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL, _ := url.Parse(dead.URL)
		dead.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusOK)
		}))
		defer spare.Close()
		spareURL, _ := url.Parse(spare.URL)

		req, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), code: http.StatusOK}
		advanced := tryBackends(rec, req, []*url.URL{deadURL, spareURL}, http.DefaultTransport, func(int, bool) {})

		Expect(advanced).To(BeTrue(), "a real retry was not reported")
		Expect(hits).To(Equal(1))
		Expect(rec.answered).To(BeTrue(), "the spare's answer was not recorded as having reached the client")
	})

	// A request nobody answered must not be publishable as the recorder's seeded 200.
	//
	// This needs a NON-final attempt to fail and the retry to be refused, which is the only way the loop ends
	// with nothing written: on a final attempt the ErrorHandler's 502 goes straight through to the client and
	// the recorder is answered, correctly. A first version of this spec used a single target and asserted the
	// recorder still held 200 — it held 502, because that is the right answer for that case.
	//
	// Mutation that turns this red: make statusRecorder stop tracking answered.
	It("leaves the recorder unanswered when a non-final attempt fails and no retry follows", func() {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(400 * time.Millisecond)
		}))
		defer slow.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer spare.Close()
		slowURL, _ := url.Parse(slow.URL)
		spareURL, _ := url.Parse(spare.URL)

		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		go func() { time.Sleep(120 * time.Millisecond); cancel() }()
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), code: http.StatusOK}
		var last int
		tryBackends(rec, req, []*url.URL{slowURL, spareURL}, http.DefaultTransport,
			func(c int, final bool) { last = c })

		Expect(rec.answered).To(BeFalse(), "nothing reached the client, so the recorder must not claim it did")
		Expect(rec.code).To(Equal(http.StatusOK),
			"the recorder still holds its seed, which is why the handler has to substitute the failure code")
		Expect(last).NotTo(BeZero(), "the failure the handler would substitute was never reported")
	})

	// A backend answering 503 is not a proxy failure and is no longer reported as one.
	//
	// This spec used to assert the opposite, because a non-final 503 was converted into a synthetic error so
	// the loop could retry on it. With that conversion gone the 503 is simply the response: ErrorHandler never
	// runs, so nothing is counted as a failed ATTEMPT, and the status reaches the client and
	// requests_total{code="503"} where it belongs.
	//
	// Mutation that turns this red: convert a retryable upstream status into an error again.
	It("does not count a backend's own 503 as a failed attempt", func() {
		loading := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer loading.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer spare.Close()
		loadingURL, _ := url.Parse(loading.URL)
		spareURL, _ := url.Parse(spare.URL)

		req, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		var reported []int
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), code: http.StatusOK}
		advanced := tryBackends(rec, req, []*url.URL{loadingURL, spareURL}, http.DefaultTransport,
			func(c int, final bool) { reported = append(reported, c) })

		Expect(advanced).To(BeFalse(), "a non-idempotent POST was replayed after a backend answered it")
		Expect(reported).To(BeEmpty(),
			"a backend's own answer was counted as an attempt the proxy failed to make")
		Expect(rec.code).To(Equal(http.StatusServiceUnavailable),
			"the backend's status did not reach the client")
	})

	// The control: a genuine transport failure must still be 502, or the change has replaced one wrong code
	// with another.
	//
	// Mutation that turns this red: report every failure as the upstream status, defaulting to 200.
	It("still reports an unreachable backend as a proxy failure", func() {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL, _ := url.Parse(dead.URL)
		dead.Close()
		spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer spare.Close()
		spareURL, _ := url.Parse(spare.URL)

		req, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		var reported []int
		tryBackends(httptest.NewRecorder(), req, []*url.URL{deadURL, spareURL}, http.DefaultTransport,
			func(c int, final bool) { reported = append(reported, c) })

		Expect(reported).To(Equal([]int{http.StatusBadGateway}),
			"a refused connection is a proxy failure and must stay 502")
	})

	// A caller must not be able to read the cluster's topology out of a failed request.
	//
	// The whole existing suite passed while the raw transport error was being written to the client, which
	// means nothing asserted what the BODY says — only what the status code is. That gap is why the leak
	// survived: every spec was watching the half that was right.
	//
	// Mutation that turns this red: pass err.Error() to writeJSONError instead of publicUpstreamMessage.
	It("tells the client nothing about where the backend lives", func() {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL, _ := url.Parse(dead.URL)
		dead.Close()

		req, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		rec := httptest.NewRecorder()
		tryBackends(rec, req, []*url.URL{deadURL}, http.DefaultTransport, nil)

		body := rec.Body.String()
		Expect(rec.Code).To(Equal(http.StatusBadGateway), "the status still has to say which failure this was")
		Expect(body).To(ContainSubstring("could not be reached"))
		// The address the proxy failed to dial, in every form it appears in a transport error.
		Expect(body).NotTo(ContainSubstring(deadURL.Host), "the body names the backend's address")
		Expect(body).NotTo(ContainSubstring("dial tcp"), "the body carries the raw transport error")
		Expect(body).NotTo(ContainSubstring("connection refused"))
		Expect(body).NotTo(ContainSubstring("127.0.0.1"))
	})

	// Mutation that turns this red: remove the maxBackendAttempts truncation.
	It("stops after the cap however many stale backends a model has", func() {
		var dialled int
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL, _ := url.Parse(dead.URL)
		dead.Close()
		urls := []*url.URL{deadURL, deadURL, deadURL, deadURL}

		req, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())
		buf := []byte(`{"model":"m"}`)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		req.Body, _ = req.GetBody()

		tryBackends(httptest.NewRecorder(), req, urls, http.DefaultTransport, func(int, bool) { dialled++ })
		Expect(dialled).To(Equal(maxBackendAttempts),
			"a request walked every stale backend, each costing a full timeout")
	})
})

// "Up but not serving" is the ordinary shape for a model server, and it used to go entirely uncovered.
//
// att.failed was set only by ErrorHandler, which ReverseProxy calls only when the round trip itself failed.
// A backend that accepted the connection and answered 503 — loading weights, post-OOM, at capacity — was a
// successful round trip, so its status went to the client with a healthy spare one list entry away.
var _ = Describe("fallback on a retryable upstream status", func() {
	post := func() *http.Request {
		buf := []byte(`{"model":"m","messages":[]}`)
		r, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", bytes.NewReader(buf))
		Expect(err).NotTo(HaveOccurred())
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
		r.Body, _ = r.GetBody()
		return r
	}
	serving := func(code int, body string) (*url.URL, *int) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			if code != http.StatusOK {
				w.WriteHeader(code)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, body)
		}))
		DeferCleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		Expect(err).NotTo(HaveOccurred())
		return u, &hits
	}

	// This spec asserted the opposite until a review pointed out what a status actually proves. A 503 says the
	// request ARRIVED; it does not say the backend declined to work on it. A model server can accept a
	// completion, spend GPU seconds on it and fail on the way out, and replaying it there bills the tenant
	// twice and can emit a second set of tokens, with no counter anywhere recording that it happened.
	//
	// So the head's own answer goes to the client and the spare stays untried. What is given up — falling
	// through a 503 that really did refuse at the door — needs the backends to state an idempotency contract,
	// not the gateway to assume one.
	//
	// Mutation that turns this red: replay on a retryable status again, by ORing retryableStatus into the
	// att.replayable gate.
	It("does not replay a POST the head already answered, even with a 503", func() {
		headURL, headHits := serving(http.StatusServiceUnavailable, "")
		spareURL, spareHits := serving(http.StatusOK, "data: [DONE]\n\n")

		rec := httptest.NewRecorder()
		tryBackends(rec, post(), []*url.URL{headURL, spareURL}, http.DefaultTransport, nil)

		Expect(*headHits).To(Equal(1))
		Expect(*spareHits).To(Equal(0), "a non-idempotent completion was sent to a second backend")
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable),
			"the head's own answer was replaced rather than passed through")
	})

	// Trailers are written AFTER the body, which is after the attempt has committed, and they were landing in
	// the scratch map that only an uncommitted attempt should be using.
	//
	// This is not a theoretical HTTP corner for this gateway: a streaming completion reports its token usage
	// in a trailer, so the tenant's own accounting arrived nowhere while the response itself looked perfect.
	//
	// Mutation that turns this red: return the scratch map from Header() unconditionally.
	It("delivers a trailer the backend writes after the body", func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Trailer", "X-Usage-Total")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			w.Header().Set("X-Usage-Total", "512")
		}))
		DeferCleanup(upstream.Close)
		u, err := url.Parse(upstream.URL)
		Expect(err).NotTo(HaveOccurred())

		rec := httptest.NewRecorder()
		tryBackends(rec, post(), []*url.URL{u}, http.DefaultTransport, nil)

		res := rec.Result()
		defer func() { _ = res.Body.Close() }()
		_, _ = io.ReadAll(res.Body)
		Expect(res.Trailer.Get("X-Usage-Total")).To(Equal("512"),
			"the backend's trailer never left the attempt's scratch headers")
	})

	// The dangerous half, and the one a status check cannot reach.
	//
	// A backend that ACCEPTS the connection, reads the request and then dies mid-flight produces a transport
	// error, so ErrorHandler fires and the attempt is marked failed — the same state a refused connection
	// leaves behind. The difference is that this backend received the request and may have spent GPU seconds
	// on it before dying, so replaying it charges the tenant twice for one completion.
	//
	// Mutation that turns this red: make neverReachedBackend return true for anything other than a dial
	// failure, which is what "the client saw nothing, so nothing happened" amounts to.
	It("does not replay a POST the backend accepted and then dropped", func() {
		dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read the body first, so the request provably arrived in full before the connection dies.
			_, _ = io.ReadAll(r.Body)
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}))
		DeferCleanup(dropped.Close)
		droppedURL, err := url.Parse(dropped.URL)
		Expect(err).NotTo(HaveOccurred())

		spareURL, spareHits := serving(http.StatusOK, "data: [DONE]\n\n")

		rec := httptest.NewRecorder()
		advanced := tryBackends(rec, post(), []*url.URL{droppedURL, spareURL}, http.DefaultTransport, nil)

		Expect(advanced).To(BeFalse(), "a completion the backend had already received was sent again")
		Expect(*spareHits).To(Equal(0), "a non-idempotent completion reached a second backend")
	})

	// The client must be able to tell which side failed, which means the backend's status reaches them
	// unchanged rather than becoming ErrorHandler's 502.
	//
	// Mutation that turns this red: convert a retryable upstream status into a proxy error.
	It("passes a backend's own status through instead of a gateway error", func() {
		aURL, aHits := serving(http.StatusServiceUnavailable, "")
		bURL, bHits := serving(http.StatusServiceUnavailable, "")

		rec := httptest.NewRecorder()
		tryBackends(rec, post(), []*url.URL{aURL, bURL}, http.DefaultTransport, nil)

		Expect(*aHits).To(Equal(1))
		Expect(*bHits).To(Equal(0))
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable),
			"the model's 503 reached the caller as a gateway error, so they cannot tell which side failed")
	})

	// 500 is not retryable: it usually means the request broke something, and replaying it spends a second
	// backend to fail the same way while doubling the blast radius of a request that can crash an engine.
	It("does not spend a second backend on a 500", func() {
		headURL, headHits := serving(http.StatusInternalServerError, "")
		spareURL, spareHits := serving(http.StatusOK, "data: [DONE]\n\n")

		rec := httptest.NewRecorder()
		tryBackends(rec, post(), []*url.URL{headURL, spareURL}, http.DefaultTransport, nil)

		Expect(*headHits).To(Equal(1))
		Expect(*spareHits).To(Equal(0), "a 500 was replayed on another backend")
		Expect(rec.Code).To(Equal(http.StatusInternalServerError))
	})
})
