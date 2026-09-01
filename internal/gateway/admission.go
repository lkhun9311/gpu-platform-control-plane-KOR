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
	"fmt"
	"sync"
	"time"

	// Prefer Google's vetted token-bucket limiter over a hand-rolled one, same as bucketRegistry in ratelimit.go.
	"golang.org/x/time/rate"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// AdmissionMode selects which admission control runs on the inference path.
//
// Design rationale (design spec Config and API section): "--admission-mode=off|static-cap|kv-aware" replaces the old v1 boolean "--admission-guard" flag, since a boolean cannot express a third arm.
type AdmissionMode string

const (
	// AdmissionOff runs no admission control; every request that reaches this stage is admitted.
	//
	// This is the default, so a gateway that never sets a mode keeps its pre-admission-guard behavior exactly.
	AdmissionOff AdmissionMode = "off"
	// AdmissionStaticCap runs the pressure-blind weighted input-token bucket (this task).
	AdmissionStaticCap AdmissionMode = "static-cap"
	// AdmissionKVAware runs the KV-cache/queue-depth-driven guard.
	//
	// It reads the backend's KV-cache pressure and rejects standard long-context requests only while the backend is engaged.
	AdmissionKVAware AdmissionMode = "kv-aware"
)

// Admitter decides, synchronously on the request path, whether a request is admitted.
//
// Design rationale (design spec Pipeline placement section): all three modes implement one Admit signature so chatCompletions calls exactly one method regardless of which mode is configured.
//
// Admit must never block on I/O or scrape a backend.
//
// The request path runs Admit inline while holding the client's connection open, so any I/O here would turn a per-request decision into a per-request network call, defeating the point of a synchronous guard.
type Admitter interface {
	// Admit returns (true, "") to admit, or (false, reason) to reject with a 429.
	//
	// reason is empty on admission and a stable machine-readable string on rejection, reported as the JSON error body's "code" field.
	Admit(ctx context.Context, meta RequestMeta, backend *BackendRef, tenant, tier string) (bool, string)
}

// tierAnnotation is the annotation key on GPUQuotaPolicy that opts a tenant into the premium tier.
//
// It lives on the policy, not the request, since tier is a property of the tenant's contract, not something a caller can assert about itself.
const tierAnnotation = "platform.lkhun9311.github.io/tier"

// tierPremium and tierStandard are the only two tier values the gateway recognizes.
const (
	tierPremium  = "premium"
	tierStandard = "standard"
)

// tierForPolicy resolves policy's tier from its tier annotation.
//
// Design rationale (design spec Arm C section, "Tier errors resolve to standard"): only the exact value "premium" grants the premium tier.
//
// Any other value, an empty string, a missing annotation, or a nil policy all resolve to standard.
//
// A typo or case mismatch (e.g. "PREMIUM") must never grant premium, since that would let a tenant bypass admission control by accident rather than by an operator's deliberate choice.
func tierForPolicy(policy *platformv1.GPUQuotaPolicy) string {
	if policy == nil {
		return tierStandard
	}
	if policy.Annotations[tierAnnotation] == tierPremium {
		return tierPremium
	}
	return tierStandard
}

// reasonInputRateLimit is the 429 reason staticCapAdmitter reports when its bucket is exhausted.
//
// Design rationale (design spec Config and API section): distinct from the RPM limiter's "rate_limited" code, since the two answer different questions (request rate vs offered input-token volume) and a client needs to tell them apart to know what to back off on.
const reasonInputRateLimit = "input_rate_limit"

// reasonInputExceedsBurst is reported when a request's estimated input can never fit the bucket at all.
//
// rate.Limiter.AllowN refuses n > burst unconditionally: the bucket never holds that many tokens, so no
// amount of waiting changes the answer. Reporting that as input_rate_limit told the caller their request was
// momentarily over budget and gave them a five-second Retry-After, so a well-behaved client retried a request
// that was arithmetically impossible to admit, forever. It is a size problem, not a timing one, and the only
// action that resolves it is a smaller prompt.
const reasonInputExceedsBurst = "input_exceeds_burst"

// offAdmitter always admits.
//
// It is the implementation behind AdmissionOff, the default mode, so a gateway that never configures admission control behaves exactly as it did before this guard existed.
type offAdmitter struct{}

// Admit always returns (true, "").
func (offAdmitter) Admit(_ context.Context, _ RequestMeta, _ *BackendRef, _, _ string) (bool, string) {
	return true, ""
}

// NewOffAdmitter returns the Admitter behind AdmissionOff.
//
// Why an exported constructor for a zero-field type: offAdmitter itself is unexported, like bucketRegistry in ratelimit.go, so cmd/gateway/main.go cannot name the type directly and needs a constructor to obtain one.
func NewOffAdmitter() Admitter { return offAdmitter{} }

// staticCapAdmitter enforces a per-backend weighted input-token bucket over the eligible population.
//
// Design rationale (design spec Arm B section): this is the "pressure-blind, admission-matched control".
//
// It reads no backend telemetry, unlike the kv-aware guard (Task 3), which is what makes it an honest experimental control rather than a second, cheaper version of the same mechanism.
//
// The eligible population is standard-tier requests at or above the long-input threshold; premium always passes, and standard-short always passes, so the bucket only ever meters the traffic the kv-aware guard would also meter.
//
// The bucket is weighted: AllowN consumes meta.EstInputTokens tokens per request rather than a flat one, so a single large request costs proportionally more capacity than a small one.
//
// A per-request count would let ten small requests and one huge request look identical to the limiter, even though the huge one is the one actually pressuring the backend's KV cache.
type staticCapAdmitter struct {
	// rate is the sustained refill rate, in tokens per second.
	rate float64
	// burst is the bucket's maximum capacity, in tokens.
	burst int
	// longThreshold is the minimum EstInputTokens a standard-tier request needs to enter the eligible population.
	longThreshold int

	// mu guards limiters against concurrent access, mirroring bucketRegistry in ratelimit.go.
	mu sync.Mutex
	// limiters maps a backend key (namespace/name) to that backend's bucket.
	//
	// Design rationale (design spec Arm B section, "Per backend / KV pool (not global)"): a single global bucket would let contention on one backend throttle traffic to an unrelated one, which is not what the guard is meant to measure or protect.
	limiters map[string]*rate.Limiter
}

// newStaticCapAdmitter returns a staticCapAdmitter with the given fixed tokensPerSec rate, burst (tokens), and long-input threshold (tokens).
//
// The parameter is not named "rate" despite the field it fills: that name would shadow the golang.org/x/time/rate package import for the rest of the function body.
func newStaticCapAdmitter(tokensPerSec float64, burst int, longThreshold int) *staticCapAdmitter {
	return &staticCapAdmitter{
		rate:          tokensPerSec,
		burst:         burst,
		longThreshold: longThreshold,
		limiters:      make(map[string]*rate.Limiter),
	}
}

// backendKey identifies a backend's bucket.
//
// Namespace and name, not the URL, since two BackendRefs pointing at the same namespace/name always share one KV pool, and comparing URLs would additionally split traffic that changes port or scheme without changing which serving pool receives it.
func backendKey(b *BackendRef) string {
	return fmt.Sprintf("%s/%s", b.Namespace, b.Name)
}

// limiterFor returns the bucket for key, lazily creating one the first time key is seen.
//
// Mirrors bucketRegistry.Allow's lock-then-lift-the-pointer pattern in ratelimit.go: the map lock stays held only long enough to find or create the limiter, and the limiter itself (safe for concurrent use on its own) is consulted outside the lock.
func (a *staticCapAdmitter) limiterFor(key string) *rate.Limiter {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.limiters[key]
	if !ok {
		l = rate.NewLimiter(rate.Limit(a.rate), a.burst)
		a.limiters[key] = l
	}
	return l
}

// Admit reports whether the request may proceed under the backend's weighted input-token bucket.
//
// Premium requests and standard requests below longThreshold are outside the eligible population and always pass, matching the design spec's Arm B/C population definition exactly.
func (a *staticCapAdmitter) Admit(_ context.Context, meta RequestMeta, backend *BackendRef, _, tier string) (bool, string) {
	eligible := tier == tierStandard && meta.EstInputTokens >= a.longThreshold
	if !eligible {
		return true, ""
	}

	// Checked before AllowN rather than inferred from its refusal, because the two are indistinguishable
	// afterwards: AllowN returns false both for a bucket that is momentarily empty and for a request larger
	// than the bucket will ever hold.
	if meta.EstInputTokens > a.burst {
		return false, reasonInputExceedsBurst
	}

	limiter := a.limiterFor(backendKey(backend))
	// Consume meta.EstInputTokens tokens rather than one, so cost tracks offered input volume rather than request count.
	if limiter.AllowN(time.Now(), meta.EstInputTokens) {
		return true, ""
	}
	return false, reasonInputRateLimit
}

// NewStaticCapAdmitter returns the Admitter behind AdmissionStaticCap, built from newStaticCapAdmitter.
//
// Why an exported wrapper: staticCapAdmitter is unexported, so cmd/gateway/main.go needs this to assemble one from its own admission-mode flags.
func NewStaticCapAdmitter(tokensPerSec float64, burst int, longThreshold int) Admitter {
	return newStaticCapAdmitter(tokensPerSec, burst, longThreshold)
}
