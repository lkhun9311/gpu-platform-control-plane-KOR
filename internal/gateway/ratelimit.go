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
	"errors"
	"sync"

	// Prefer Google's vetted token-bucket limiter over a hand-rolled one (design spec Components section).
	"golang.org/x/time/rate"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// ErrNoPolicy is the sentinel reported when no GPUQuotaPolicy targets the tenant.
//
// Design rationale (design spec Identity model section): a tenant with zero policies must get a 403.
//
// This signals "policy not provisioned" rather than a read failure, so the caller translates it to 403.
var ErrNoPolicy = errors.New("no GPUQuotaPolicy for tenant")

// policyForTenant returns the single GPUQuotaPolicy governing tenant, or ErrNoPolicy if none exists.
//
// Design rationale (design spec Identity model section): GPUQuotaPolicy is cluster-scoped and nothing stops two policies from sharing a spec.tenant.
//
// Limiter state must not become non-deterministic in that case, so exactly one is chosen by a deterministic rule: the oldest creationTimestamp wins, and ties break on ascending name.
func (s *Server) policyForTenant(ctx context.Context, tenant string) (*platformv1.GPUQuotaPolicy, error) {
	var list platformv1.GPUQuotaPolicyList
	if err := s.Client.List(ctx, &list); err != nil {
		// A read failure (e.g. an API server error) propagates as-is.
		return nil, err
	}

	// Pick the oldest policy among those matching the tenant.
	var oldest *platformv1.GPUQuotaPolicy
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.Tenant != tenant {
			continue
		}
		if oldest == nil || olderPolicy(p, oldest) {
			oldest = p
		}
	}

	if oldest == nil {
		// No policy targets this tenant.
		return nil, ErrNoPolicy
	}
	return oldest, nil
}

// olderPolicy reports whether a takes precedence over b.
//
// The earlier creationTimestamp wins, and on an exact tie the lexicographically smaller name wins.
func olderPolicy(a, b *platformv1.GPUQuotaPolicy) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		// Same timestamp, so tie-break on ascending name.
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// bucketRegistry holds one token-bucket limiter per tenant.
//
// Design rationale (design spec Components section): a map[tenant]*rate.Limiter.
//
// The gateway runs a single replica, so an in-memory map suffices and distributed buckets are out of scope.
//
// Concurrent requests read and write the map, so a Mutex guards it.
type bucketRegistry struct {
	// mu guards buckets against concurrent access, since Go's built-in map is not safe for concurrent writes.
	mu sync.Mutex
	// buckets maps a tenant name to that tenant's bucket.
	buckets map[string]*trackedBucket
}

// trackedBucket pairs a limiter with the policy values (rpm, burst) it was built from.
//
// Keeping those values alongside the limiter is what lets Allow detect a changed policy and refresh the limiter.
type trackedBucket struct {
	limiter *rate.Limiter
	// rpm is the requestsPerMinute currently in effect, used to detect policy changes.
	rpm int32
	// burst is the burst currently in effect, used to detect policy changes.
	burst int32
}

// newBucketRegistry returns an empty bucketRegistry with buckets pre-initialized via make, since inserting into a nil map panics.
func newBucketRegistry() *bucketRegistry {
	return &bucketRegistry{buckets: make(map[string]*trackedBucket)}
}

// Allow reports whether tenant may issue a request now, consuming one token if so.
//
// false means the tenant is over its limit and the caller translates it to a 429 rate_limited.
//
// configured reports whether the registry was ever installed, so a caller can tell an unfinished Server from
// a tenant that is genuinely over budget. Nil-safe by design: that is the only case it exists to answer.
func (b *bucketRegistry) configured() bool { return b != nil }

// Design rationale (design spec Components and Request flow step 4).
//
//   - a nil rateLimit means an unlimited tenant, so always allow
//   - per-second rate = requestsPerMinute / 60, since feeding the per-minute value straight in would run 60× fast
//   - on a policy change, refresh via SetLimit/SetBurst instead of rebuilding, which preserves accumulated tokens
func (b *bucketRegistry) Allow(tenant string, rl *platformv1.GPUQuotaRateLimit) bool {
	// A nil REGISTRY is an assembly fault, not a runtime state: main always calls InitRateLimiter, and a
	// Server without it is one nobody finished building. It is refused rather than allowed because a limiter
	// that cannot function must not answer "within budget" — that would serve unlimited traffic under a
	// configuration that says otherwise, and nothing anywhere would say so.
	//
	// The comment on InitRateLimiter claimed the gateway guarded this case. It did not: with a non-nil policy
	// the next line dereferenced b and the request panicked.
	if b == nil {
		return false
	}
	// A nil rl means this tenant has no gateway rate limit, so it always passes.
	if rl == nil {
		return true
	}

	// Convert the per-minute policy value to a per-second refill rate, going through float64 first so integer division does not truncate the fraction.
	limit := rate.Limit(float64(rl.RequestsPerMinute) / 60.0)
	burst := int(rl.Burst)

	b.mu.Lock()
	tb, ok := b.buckets[tenant]
	if !ok {
		// First time seeing this tenant, so build and register a limiter, which starts out full.
		tb = &trackedBucket{
			limiter: rate.NewLimiter(limit, burst),
			rpm:     rl.RequestsPerMinute,
			burst:   rl.Burst,
		}
		b.buckets[tenant] = tb
	} else if tb.rpm != rl.RequestsPerMinute || tb.burst != rl.Burst {
		// The policy values changed, so update the limiter in place rather than discarding it.
		//
		// Rebuilding would refill the bucket and hand a free burst to any tenant whose policy was edited.
		tb.limiter.SetLimit(limit)
		tb.limiter.SetBurst(burst)
		tb.rpm = rl.RequestsPerMinute
		tb.burst = rl.Burst
	}
	// Lift out just the limiter pointer so the map lock stays short.
	limiter := tb.limiter
	b.mu.Unlock()

	// rate.Limiter is concurrency-safe on its own, so this runs outside the lock.
	//
	// It consumes a token and reports true, or reports false when none is available.
	return limiter.Allow()
}
