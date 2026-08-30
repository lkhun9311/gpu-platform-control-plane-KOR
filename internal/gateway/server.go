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

// Package gateway implements the tenant-aware OpenAI-compatible serving gateway.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ModelNameIndex is the cache field-index key over InferenceDeployment.spec.model.name.
//
// Routing resolves a requested model to its Service via this index, with no CR field selector and no per-request apiserver call.
const ModelNameIndex = ".spec.model.name"

// Server holds the gateway's shared dependencies and HTTP handlers.
type Server struct {
	// Client reads InferenceDeployment, GPUQuotaPolicy, and the api-keys Secret from the scoped cache.
	Client client.Client
	// Namespace and APIKeySecret locate the api-keys Secret used to resolve tenants.
	Namespace    string
	APIKeySecret string
	// ready flips true once the cache has synced, gating readiness.
	ready atomic.Bool
	// buckets holds the per-tenant token buckets, populated by cmd/gateway/main.go at assembly.
	//
	// Design rationale (design spec Components section): rate limiting only means anything if the remaining-token state survives across requests.
	//
	// A bucket built per request would always look full and never limit anything.
	//
	// So one process-wide registry holds them all.
	buckets *bucketRegistry
	// mode records which admission-control mode admitter implements, purely for the mode label on the admission metrics.
	//
	// It travels alongside admitter rather than being derived from it, since the two are set together by SetAdmitter and a type switch on admitter would need one arm per implementation anyway.
	//
	// The zero value "" is treated as AdmissionOff at the call site, matching admitter's nil default below.
	mode AdmissionMode
	// admitter runs the admission-control stage in chatCompletions, between backend resolution and the proxy handoff.
	//
	// nil (always so unless SetAdmitter is called) makes chatCompletions default to an offAdmitter, so a gateway that never configures admission control keeps its pre-admission-guard behavior exactly.
	admitter Admitter
	// backendOverride swaps out model-to-backend resolution in tests.
	//
	// nil (always so in production) makes resolveBackend use the real backendFor.
	//
	// Why the hook exists (plan Task 6).
	//
	// backendFor builds an in-cluster DNS address of the form http://<name>.<ns>.svc:<port>.
	//
	// No test process can reach that address.
	//
	// Overriding just the resolved address lets the whole pipeline run for real against an httptest server.
	//
	// The production path runs unchanged whenever the hook is nil.
	//
	// So this never bypasses production behavior.
	//
	// The hook returns a bare *url.URL rather than a *BackendRef, since tests only need the pipeline to reach an httptest server; resolveBackend wraps it into a BackendRef carrying just URL and Model.
	backendOverride func(model string) *url.URL
	// responseHeaderTimeout bounds the wait for upstream response headers.
	//
	// Zero selects proxy.go's defaultResponseHeaderTimeout (30s), so production leaves it unset.
	//
	// Why a field rather than a constant.
	//
	// This value directly sets how long a 504 takes to surface.
	//
	// Pinned at 30s, any spec covering the 504 mapping would have to wait 30s, so no such spec gets written and the branch goes unverified.
	//
	// An unverified 504 branch is the dangerous kind, since deleting it degrades silently to 502 rather than failing.
	//
	// A field lets tests drive the same code path with a short bound.
	responseHeaderTimeout time.Duration
	// transport is the single outbound Transport every proxied request reuses, and transportOnce builds it on first use.
	//
	// The connection pool lives inside the Transport, so it is only a pool at all while one Transport outlives many requests.
	//
	// Construction is deferred rather than done at assembly because responseHeaderTimeout is set on the struct after it is built (production leaves it zero, tests shorten it), and a Transport built too early would capture the wrong bound.
	transport     http.RoundTripper
	transportOnce sync.Once
}

// sharedTransport returns the process-wide outbound Transport, building it the first time it is asked for.
//
// sync.Once rather than a plain nil check because chatCompletions runs concurrently, and two requests racing to build a Transport would leave one of them with a pool nobody else uses.
func (s *Server) sharedTransport() http.RoundTripper {
	s.transportOnce.Do(func() {
		s.transport = newTransport(s.responseHeaderTimeout)
	})
	return s.transport
}

// markReady marks the gateway ready to serve.
func (s *Server) markReady() { s.ready.Store(true) }

// MarkReady is the exported entry point for the binary to flip readiness after the cache's first sync.
func (s *Server) MarkReady() { s.markReady() }

// InitRateLimiter installs the per-tenant token bucket registry.
//
// Why this method exists.
//
// Both the buckets field and bucketRegistry are unexported, so cmd/gateway/main.go cannot populate them via a struct literal.
//
// Keeping the type unexported means the bucket internals can change without touching the assembly code.
func (s *Server) InitRateLimiter() { s.buckets = newBucketRegistry() }

// SetAdmitter installs the admission-control implementation and the mode label it reports on metrics.
//
// Why this method exists (mirrors InitRateLimiter above): admitter is unexported, so cmd/gateway/main.go cannot populate it via a struct literal.
//
// mode travels through the same call so the two can never drift out of step with each other.
func (s *Server) SetAdmitter(mode AdmissionMode, a Admitter) {
	s.mode = mode
	s.admitter = a

	// Publish the mode here rather than in main, for the same reason mode travels through this call at
	// all: this is the one place that knows which Admitter is actually installed, so the series cannot
	// claim a mode the gateway is not running.
	admissionModeActive.Reset()
	admissionModeActive.WithLabelValues(string(mode)).Set(1)
}

// readyz returns 200 only once the cache has synced, else 503 so the Pod stays out of Service endpoints.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "cache not synced", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// fail ends the request with the given status code and records it.
//
// Why route this through one helper.
//
// Every failure branch must both write a code and count it.
//
// Routing them all through one helper is what keeps a branch from silently going unobserved.
//
// model may be empty: the auth, policy, and rate-limit stages run before the body is parsed, so no model is known yet.
//
// The empty label records exactly that.
//
// The response body is the OpenAI-style JSON envelope from writeJSONError, not plaintext, so every gateway failure looks the same to a client regardless of which pipeline stage produced it.
func (s *Server) fail(w http.ResponseWriter, tenant, model string, code int) {
	requests.WithLabelValues(tenant, model, strconv.Itoa(code)).Inc()
	writeJSONError(w, code, http.StatusText(code))
}

// unresolvedModelLabel is the fixed model label recorded when the requested model never resolved to a backend.
//
// Why a sentinel and not the requested name (design spec Observability section, "Unbounded-cardinality values ... are never labels").
//
// On the 404 and 502 routing paths the model is an arbitrary string lifted straight out of the request body, so an authenticated tenant looping over random names mints one new time series per name in requests_total.
//
// A counter's series are never reclaimed, so that walks the gateway's /metrics response and the scraping Prometheus into the ground, and it takes an authenticated client rather than an attacker to do it by accident.
//
// Every other model label in this file is bounded: the pre-routing stages pass an empty string, and the post-routing stages only run once resolveBackend has matched a configured InferenceDeployment.
//
// The leading underscore keeps the sentinel from ever colliding with a real model name, which must be a valid CR field value.
const unresolvedModelLabel = "_unresolved"

// failUnresolvedModel ends a request whose model never resolved to a backend, recording it under the sentinel label rather than the requested name.
//
// The requested name is not lost, only kept out of the label set: it goes into the caller's error body here and into the log line at each call site, both of which cost nothing per distinct value.
func (s *Server) failUnresolvedModel(w http.ResponseWriter, tenant, model string, code int) {
	requests.WithLabelValues(tenant, unresolvedModelLabel, strconv.Itoa(code)).Inc()
	// %q rather than %s so an empty or whitespace-only name is still visible to whoever has to debug it.
	writeJSONError(w, code, fmt.Sprintf("%s: model %q", http.StatusText(code), model))
}

// failReason ends the request with status and an explicit machine-readable reason, then records it.
//
// Why this cannot just be fail(): the admission guard's reasons ("input_rate_limit" here, "kv_cache_pressure" in Task 3) are not derivable from the HTTP status the way fail()'s errorCode mapping assumes, since both share status 429.
//
// Retry-After is set to 5s (design spec Config and API section) because an admission-guard 429 is expected to clear once bucket capacity or backend pressure recovers, unlike the RPM limiter's 429, which carries no such hint.
func (s *Server) failReason(w http.ResponseWriter, tenant, model string, code int, reason string) {
	requests.WithLabelValues(tenant, model, strconv.Itoa(code)).Inc()
	w.Header().Set("Retry-After", "5")
	writeJSONErrorCode(w, code, reason, http.StatusText(code))
}

// resolveBackend resolves a model to its backend.
//
// It prefers the test hook over the real backendFor.
func (s *Server) resolveBackend(ctx context.Context, policy *platformv1.GPUQuotaPolicy, model string) ([]*BackendRef, error) {
	if s.backendOverride != nil {
		// The hook only fabricates a URL; Namespace/Name/Port stay zero-valued, since tests using it only need the pipeline to reach an httptest server, not a real backend's identity.
		return []*BackendRef{{URL: s.backendOverride(model), Model: model}}, nil
	}
	return s.backendsFor(ctx, policy, model)
}

// chatCompletions serves the OpenAI-compatible chat completions pipeline.
//
// The ordering is the point (design spec Request flow section).
//
//  1. request id: every log line and the upstream call share one id, or tracing breaks.
//  2. auth: an unidentified request stops here (401).
//  3. policy: identified but unauthorized stops here (403).
//  4. rate limit: over its share stops here (429).
//  5. body parse: extract the model (400).
//  6. routing: resolve the model to a backend (404).
//  7. admission: off/static-cap/kv-aware decide whether the request proceeds (429).
//  8. proxy: hand off upstream (502/504, or whatever the upstream returns).
//
// Why auth precedes rate limiting.
//
// The limiter keys on tenant and cannot judge without one.
//
// Limiting anonymous traffic would also let an attacker drain someone else's bucket and stall that tenant.
//
// Why rate limiting precedes body parsing.
//
// Parsing buffers up to 1MB.
//
// Reading the body of a request destined for rejection would let a flooding client keep spending gateway memory.
//
// So rejections happen before that cost is paid.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. request id: reuse a caller-supplied id when present.
	//
	// Why an incoming id is reused.
	//
	// Keeping it is what stitches the caller's traces and ours together.
	//
	// Always minting a new one would sever that link.
	rid := r.Header.Get("X-Request-Id")
	if rid == "" {
		rid = newRequestID()
	}
	// Set it on both the response and the upstream request.
	//
	// Response-only leaves the request unfindable in upstream logs, and request-only leaves the client unable to quote its own id.
	w.Header().Set("X-Request-Id", rid)
	r.Header.Set("X-Request-Id", rid)

	// 2. Resolve the API key to a tenant.
	tenant, ok, err := s.resolveTenant(ctx, r)
	if err != nil {
		// 503, not 401. The gateway could not establish whether this key is valid, and answering
		// Unauthorized asserts that it did — for every tenant at once, since they all read the same Secret.
		// An operator seeing a fleet of 401s rotates credentials; one seeing 503s looks at the cluster.
		log.FromContext(ctx).Error(err, "cannot read the api-keys secret", "request_id", rid)
		s.fail(w, "", "", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		s.fail(w, "", "", http.StatusUnauthorized)
		return
	}

	// 3. Find the tenant's GPUQuotaPolicy.
	//
	// errors.Is separates "no policy" (an ordinary outcome) from a broken read.
	//
	// Conflating them would show an apiserver outage as a 403 and send operators hunting through policy config.
	policy, err := s.policyForTenant(ctx, tenant)
	if errors.Is(err, ErrNoPolicy) {
		s.fail(w, tenant, "", http.StatusForbidden)
		return
	}
	if err != nil {
		// The lookup itself failed.
		//
		// This is not the client's fault.
		log.FromContext(ctx).Error(err, "policy lookup failed", "tenant", tenant, "request_id", rid)
		s.fail(w, tenant, "", http.StatusBadGateway)
		return
	}

	// 4. Take a token from the tenant's bucket.
	// An unfinished Server is answered 503, not 429. Both are refusals, but 429 tells the caller they exceeded
	// a budget that in this state was never being applied, and the retry it invites will be refused the same
	// way forever.
	if !s.buckets.configured() {
		log.FromContext(ctx).Error(nil, "rate limiter was never initialised; refusing rather than serving unlimited",
			"request_id", rid)
		s.fail(w, tenant, "", http.StatusServiceUnavailable)
		return
	}
	if !s.buckets.Allow(tenant, policy.Spec.RateLimit) {
		// Counted separately from requests{code="429"}; the two answer different questions.
		rateLimited.WithLabelValues(tenant).Inc()
		s.fail(w, tenant, "", http.StatusTooManyRequests)
		return
	}

	// 5. Extract admission metadata and recover the body.
	//
	// Malformed JSON, a missing model, and an oversized body all land here.
	//
	// All three are the client's to fix.
	body, meta, err := readRequestMeta(r)
	if err != nil {
		// A body over the cap is not malformed, and 400 told the caller to fix their JSON when the JSON was
		// fine. MaxBytesReader reports the case as a distinguishable type precisely so it can be separated.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.fail(w, tenant, "", http.StatusRequestEntityTooLarge)
			return
		}
		s.fail(w, tenant, "", http.StatusBadRequest)
		return
	}
	// Put the body back, or the upstream receives an empty one.
	//
	// GetBody is set from the same factory, which is what lets the shared connection pool recover from the one stale-connection case Go will retry for a POST.
	//
	// When the Transport picks an idle connection the upstream has already closed and the write fails having sent nothing, http.Transport retries on a fresh connection if and only if it can rewind the body.
	//
	// A proxied request has no GetBody of its own, since the server side never sets one, so without this line that case reaches the client as a 502 despite nothing having been sent upstream.
	//
	// It does not make the pool immune to a stale connection: once bytes are on the wire, Go refuses to replay a POST at all (see readRequestMeta).
	r.GetBody = body
	// The factory reads from a buffer already in memory, so it has no failure mode and there is no error path to take here.
	r.Body, _ = r.GetBody()

	// 6. Resolve the model to a backend.
	targets, err := s.resolveBackend(ctx, policy, meta.Model)
	if errors.Is(err, ErrNoRoute) {
		// An ordinary "no such model" outcome, so Info rather than Error.
		//
		// It is logged at all because the metric now records the sentinel label, and without this line the requested name would survive nowhere the operator can reach it.
		log.FromContext(ctx).Info("no backend for model", "tenant", tenant, "model", meta.Model, "request_id", rid)
		s.failUnresolvedModel(w, tenant, meta.Model, http.StatusNotFound)
		return
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "backend lookup failed", "tenant", tenant, "model", meta.Model, "request_id", rid)
		s.failUnresolvedModel(w, tenant, meta.Model, http.StatusBadGateway)
		return
	}

	// The reachable set is decided ONCE, here, and every stage below sees the same slice.
	//
	// tryBackends caps attempts at maxBackendAttempts and used to apply that cap itself, on its own copy. That
	// was invisible while admission only ever consulted targets[0]. Once admission started asking every
	// candidate, a third backend the request could never reach could reject it on its own pressure — a refusal
	// attributable to a machine that was never going to serve. Registration has the same problem in the other
	// direction: a scraper started for a backend outside the reachable set is telemetry nothing can act on.
	targets = capBackendAttempts(targets)

	// 7. Admission control: decide whether the request may proceed.
	//
	// Design rationale (design spec Pipeline placement section): this sits after backend resolution and before the proxy handoff, so an unroutable model (404, step 6) never consumes admission budget, while every request that will actually reach a backend is metered before it does.
	//
	// tier comes from the policy already fetched in step 3, not from the request, since tier is a property of the tenant's contract rather than something a caller can assert about itself.
	tier := tierForPolicy(policy)
	admitter := s.admitter
	if admitter == nil {
		// SetAdmitter was never called, so behave exactly as if the guard did not exist.
		admitter = offAdmitter{}
	}
	// kv-aware is the only mode that needs to learn about a backend as soon as it is routed
	// (design spec Config and API section, "the gateway registers backends as they are first
	// routed"); off and static-cap don't implement backendRegistrar, so this is a no-op for
	// them.
	//
	// scraperManager.Register is idempotent, so calling it on every request only actually
	// starts a scraper the first time a given backend is seen.
	mode := s.mode
	if mode == "" {
		mode = AdmissionOff
	}
	admit, reason := admitCandidates(ctx, admitter, meta, targets, tenant, tier)
	decision := "admit"
	if !admit {
		decision = "reject"
	}
	// Recorded for every request, admitted or not, so the admit rate and admitted-vs-offered token fraction can both be read straight off these two series without diffing against requests_total.
	admissionDecisions.WithLabelValues(string(mode), tenant, meta.Model, decision, reason).Inc()
	admissionInputTokens.WithLabelValues(string(mode), tenant, decision).Add(float64(meta.EstInputTokens))
	if !admit {
		// A request larger than the bucket can ever hold is refused permanently, so it must not carry the
		// retry hint failReason attaches: a client obeying it would retry an arithmetically impossible request
		// forever. 413 rather than 429 for the same reason — the caller's action is a smaller prompt, not a
		// later one.
		if reason == reasonInputExceedsBurst {
			s.fail(w, tenant, meta.Model, http.StatusRequestEntityTooLarge)
			return
		}
		s.failReason(w, tenant, meta.Model, http.StatusTooManyRequests, reason)
		return
	}

	// 8. From here the response is the upstream's, passed through rather than composed.
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	// Each candidate is tried until one answers, and the two conditions below are what make that safe rather
	// than merely useful.
	//
	// A retry is only possible while NOTHING has reached the client. Once the upstream has written a status
	// line or a token, the client is mid-response and a second backend cannot take over — it would splice two
	// answers together. att.wrote is that latch, and for a streaming response it closes on the first token.
	//
	// It also needs the request body back. A POST is not replayable on its own, and this is exactly what
	// r.GetBody was already installed for one stage earlier: the factory reads from a buffer held in memory,
	// so rewinding costs nothing and cannot fail. Without it there would be no second attempt to make.
	// The LAST failure code seen, kept so a request that ended without any backend answering is not published
	// as the recorder's seeded 200. That happened on every cancelled request: the callback only wrote rec.code
	// on a final attempt, the guards inside tryBackends stopped the loop before any final attempt ran, and a
	// request nobody served went into requests_total as a success.
	lastFailure := 0
	urls := make([]*url.URL, 0, len(targets))
	for _, t := range targets {
		urls = append(urls, t.URL)
	}
	advanced := tryBackends(rec, r, urls, s.sharedTransport(), func(code int, final bool) {
		upstreamErrors.WithLabelValues(tenant, meta.Model).Inc()
		lastFailure = code
		if final {
			rec.code = code
		}
	})
	// Nothing ever reached the client, so no backend answered. rec.code is still its default 200 and would
	// publish a failed request as a success; the last failure code is what actually happened.
	//
	// The rec.answered guard is load-bearing rather than defensive: a request whose FIRST attempt failed and
	// whose retry SUCCEEDED also leaves lastFailure set, and without the guard that genuine 200 would be
	// overwritten by the failure that was recovered from.
	if !rec.answered && lastFailure != 0 {
		rec.code = lastFailure
	}
	// advanced is tryBackends reporting that it REALLY tried another candidate, not the failure callback
	// guessing. The callback fires before the retry guards run, so latching on it counted a fallback whenever
	// a non-final attempt failed — including the cancelled requests where no retry ever happened, which the
	// benchmark harness produces on every timeout.
	//
	// A successful status is still required on top: a fallback that also failed is not a rescue, and counting
	// it as one inverts what the ratio means. The range is 2xx and 3xx rather than everything below 500,
	// because a 4xx from the spare means the request reached it and was refused on its own merits — the
	// fallback path carried the request but never served an answer, and the metric's name says "served".
	if advanced && rec.code >= 200 && rec.code < 400 {
		backendFallbacks.WithLabelValues(tenant, meta.Model).Inc()
	}

	// ServeHTTP returning means the response finished sending (for a stream, that the stream closed).
	//
	// This therefore measures the whole request rather than time-to-first-byte.
	requests.WithLabelValues(tenant, meta.Model, strconv.Itoa(rec.code)).Inc()
	requestDuration.WithLabelValues(tenant, meta.Model).Observe(time.Since(start).Seconds())
}

// Handler is the serving mux on :8080.
//
// Why the method is part of the pattern (design spec Error codes section).
//
// Since Go 1.22 a ServeMux pattern may name a method.
//
// The mux therefore answers 405 for a right-path/wrong-method request and 404 for an unregistered path, without either being hand-written.
//
// Registering the bare path would let a GET fall into the pipeline and surface as 401 or 400 where 405 belongs.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// MetricsHandler is the observability mux on :8081.
//
// Design rationale (design spec Components section): user traffic (:8080) and observability (:8081) are split across ports so /metrics stays cluster-internal.
//
// Per-tenant usage metrics would let a user infer other tenants' activity, so the split is mandatory.
//
// Both muxes serve /readyz so a probe on either port gets the same answer.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHTTPHandler())
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// NewCache builds a scoped cache over the gateway's read set plus a delegating client that reads the cache and writes the apiserver.
//
// It registers the model-name field indexer used for routing.
func NewCache(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme, namespace string) (cache.Cache, client.Client, error) {
	ca, err := cache.New(cfg, cache.Options{
		Scheme:           scheme,
		DefaultTransform: cache.TransformStripManagedFields(),
		// Watch Secrets only in the namespace the gateway runs in.
		//
		// Why the scope matters (it is what the design spec's Components section means by "scoped cache").
		//
		// controller-runtime caches every namespace when no scope is given (cache.Options docs: "An empty map ... means that all namespaces will be cached").
		//
		// Unscoped, the gateway would hold every Secret in the cluster in memory.
		//
		// That includes other components' credentials and TLS private keys.
		//
		// It is the one component taking external traffic directly, so a compromise would leak all of it.
		//
		// It needs exactly one Secret, so the watch is confined to that Secret's namespace.
		//
		// This must stay in step with RBAC.
		//
		// config/gateway/rbac.yaml grants secrets reads through a Role in the gateway namespace only.
		//
		// Without this scope the cache tries to list/watch secrets in every namespace, is denied, and never syncs, so readiness never opens.
		//
		// The gateway then serves nothing.
		//
		// InferenceDeployment is deliberately absent.
		//
		// Tenants live in different namespaces and the gateway must serve any of them, so its scope cannot be narrowed ahead of time.
		//
		// GPUQuotaPolicy is cluster-scoped and has no namespace to begin with.
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					namespace: {},
				},
			},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("new cache: %w", err)
	}
	// Index InferenceDeployment by its served model name for routing lookups.
	if err := ca.IndexField(ctx, &platformv1.InferenceDeployment{}, ModelNameIndex, func(o client.Object) []string {
		return []string{o.(*platformv1.InferenceDeployment).Spec.Model.Name}
	}); err != nil {
		return nil, nil, fmt.Errorf("index %s: %w", ModelNameIndex, err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: ca}})
	if err != nil {
		return nil, nil, fmt.Errorf("new delegating client: %w", err)
	}
	return ca, cl, nil
}

// admitCandidates registers every backend that could serve this request and meters the one that will be
// tried first.
//
// The two halves take different sets, and that asymmetry is the point rather than an inconsistency.
//
// EVERY candidate is registered, because registration starts a telemetry scraper and a backend that can serve
// traffic has to be observable before it does. Registering only the head meant that when the head went down
// its scraper hit the same dead Service, its snapshot went stale, and the kv-aware guard — which fails OPEN on
// staleness — admitted everything, while the spare absorbing all of the traffic had no scraper at all. The
// guard went blind exactly when fallback made it matter, and nothing reported it, because every request still
// succeeded. Register is idempotent, so this starts a scraper only the first time a backend is seen.
//
// WHICH candidates are ASKED depends on whether asking costs anything.
//
// Asking only the head was wrong in the same shape as registering only the head, and for the same reason.
// Admission runs before any attempt, so it cannot know which backend will serve; the head is merely the one
// that will be TRIED first. When the head is unreachable its telemetry goes stale, the kv-aware guard
// bypasses on staleness — correctly, since refusing traffic on a number nobody could read would be worse —
// and the request then travels to a spare that was never consulted. The guard is at its most permissive
// exactly when the backend under real pressure is the one about to receive the request.
//
// So a stateless admitter is asked about every candidate, and one dissent rejects. It is the conservative
// reading and deliberately so: a request is admitted only if every backend it could reach would take it. The
// cost is that a loaded spare can now shed a long standard-tier request the head would have served. That
// costs something only while a spare is genuinely under KV pressure, which on this routing table means it is
// already absorbing traffic.
//
// A STATEFUL admitter is still asked once, about the head. static-cap's Admit spends EstInputTokens from a
// per-backend limiter, so asking it about each candidate would bill one request several times and the arm
// would stop measuring offered load. One request, one charge, decided up front.
func admitCandidates(ctx context.Context, admitter Admitter, meta RequestMeta,
	targets []*BackendRef, tenant, tier string) (bool, string) {
	if reg, ok := admitter.(backendRegistrar); ok {
		for _, t := range targets {
			reg.RegisterBackend(t)
		}
	}
	if _, stateless := admitter.(statelessAdmitter); !stateless {
		return admitter.Admit(ctx, meta, targets[0], tenant, tier)
	}
	for _, t := range targets {
		if ok, reason := admitter.Admit(ctx, meta, t, tenant, tier); !ok {
			return false, reason
		}
	}
	return true, ""
}
