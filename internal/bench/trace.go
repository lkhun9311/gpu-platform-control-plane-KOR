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
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"sort"
)

// TraceRow is one scheduled request in an immutable trace.
//
// The same trace replays across every arm, so a row carries only what is fixed before a run starts: when the request arrives and what it asks for.
//
// It carries no timing results; those are recorded per request during replay in a separate RawRow.
type TraceRow struct {
	// Index is the row's position in the trace, stable across arms so raw rows can be joined back to the trace.
	Index int `json:"index"`
	// OffsetMs is the request's arrival time as a millisecond offset from the run's start.
	//
	// Offsets are non-decreasing, and the replayer fires each request at its offset regardless of whether earlier responses have returned, which is what keeps the load open-loop.
	OffsetMs int64 `json:"offsetMs"`
	// Tenant is the sending tenant, resolved by the gateway to premium or standard through its normal identity chain.
	Tenant string `json:"tenant"`
	// PromptLenChars is the prompt size in characters, which the generator draws from the tenant's distribution.
	//
	// The replayer turns it into a prompt of that length so the gateway's input-token estimate and the backend's real prefill cost both scale with it.
	PromptLenChars int `json:"promptLenChars"`
	// MaxOutputTokens caps the response length the request asks for.
	MaxOutputTokens int `json:"maxOutputTokens"`
	// IsNoisy marks the long-context contender traffic, so a report can separate the victim tenant from the neighbor that pressures the KV cache.
	IsNoisy bool `json:"isNoisy"`
	// ExactInputTokens is the served tokenizer's own count for this prompt, measured rather than estimated.
	//
	// The design defines the admission-match criterion over EXACT target-tokenizer input tokens, and every
	// run so far has computed it over ceil(chars/4) instead -- a quantity the project's own calibration
	// records as 36 percent low on a short prompt and 23 percent high on a long one. So the pre-registered
	// criterion has never actually been evaluated.
	//
	// It lives on the trace rather than only on the response because a REFUSED request never reaches the
	// engine and has no response to read a count from, and the fraction needs the refused ones: they are its
	// denominator. Measured once per distinct prompt length against the running engine, which is the only
	// authority on what its tokenizer does.
	//
	// Zero means not measured, and a report refuses the admission-match check rather than falling back to
	// the estimate, since falling back is how the criterion came to be unevaluated in the first place.
	ExactInputTokens int `json:"exactInputTokens,omitempty"`
}

// TenantSpec describes one tenant's share of a trace and the shape of its requests.
type TenantSpec struct {
	// Tenant is the identity the gateway resolves; it must map to a premium or standard tier through the gateway's policy chain.
	Tenant string
	// Weight is this tenant's relative share of total arrivals; shares are normalized across all tenants.
	Weight float64
	// PromptLenChars is the fixed prompt length for this tenant's requests.
	//
	// A fixed length (rather than a distribution) keeps the offered input-token work per tenant deterministic, which the design needs so arm B can be admission-matched to arm C without a moving denominator.
	PromptLenChars int
	// MaxOutputTokens is the output cap for this tenant's requests.
	MaxOutputTokens int
	// IsNoisy marks this tenant as the long-context contender.
	IsNoisy bool
	// RatePerSec is this tenant's own mean arrival rate, and setting it selects the independent-arrivals model.
	//
	// Under Weight, a tenant's arrival TIMES are a function of every other tenant in the trace: the gaps are
	// drawn at the total rate and the tenant of each arrival is drawn from the same stream, so changing one
	// tenant's share moves all of them. That is what the M5-c ladder ran into. It held the contender at 139
	// requests give or take two by solving a weight for every rung, and the count came out right while the
	// SCHEDULE came out different at each rung, so a rung-to-rung comparison moved two things at once.
	//
	// Under RatePerSec each tenant is its own Poisson process on its own stream, keyed by the tenant's name
	// rather than its position, so its schedule depends on nothing but its own rate, the seed and the
	// duration. Holding a contender fixed while a premium rate climbs then produces a byte-identical
	// contender schedule, which trace_test.go pins under "GenerateTrace under the independent-arrivals model".
	//
	// The two models are exclusive and mixing them is refused, because a trace generated half one way has no
	// stated arrival process at all.
	RatePerSec float64
}

// TraceParams are the inputs GenerateTrace turns into a deterministic open-loop trace.
type TraceParams struct {
	// Seed makes generation deterministic: the same seed and params always produce the same trace, hence the same checksum.
	Seed int64
	// DurationMs is the wall-clock span of arrivals the trace covers.
	DurationMs int64
	// RatePerSec is the mean total arrival rate across all tenants under the weighted model, and must be zero when tenants carry their own rates.
	RatePerSec float64
	// Tenants describes each tenant's share and request shape; at least one is required.
	Tenants []TenantSpec
}

// maxTraceRows bounds how many arrivals one trace may hold.
//
// The guards in GenerateTrace refuse the rates they can name, and this is what holds when a rate gets past
// them. On 2026-09-15 a tenant with no rate reached the loop half-written, the offset never reached the
// duration, and the test binary grew to 45 GB before the kernel's OOM killer took it and other processes on
// the machine with it. Here the same mistake fails as an error instead. A million rows is far above anything
// replayed so far -- the frozen weighted trace holds 1,199 -- and a replay could never offer that load anyway.
const maxTraceRows = 1_000_000

// tooManyRows is the refusal both arrival models return when a trace reaches maxTraceRows.
func tooManyRows(durationMs int64) error {
	return fmt.Errorf("trace reached %d rows before reaching durationMs %d; no replay offers that load, so the rate is refused rather than generated until memory runs out", maxTraceRows, durationMs)
}

// GenerateTrace produces a deterministic open-loop trace from params.
//
// Arrivals follow a Poisson process: inter-arrival gaps are exponentially distributed with the mean total rate, so the schedule is bursty like real traffic rather than evenly spaced.
//
// Each arrival is assigned to a tenant by that tenant's normalized weight, using the same seeded stream, so the whole trace is reproducible from Seed alone.
//
// Offsets are non-decreasing by construction, since each is the running sum of non-negative gaps.
func GenerateTrace(params TraceParams) ([]TraceRow, error) {
	if params.DurationMs <= 0 {
		return nil, fmt.Errorf("durationMs must be positive, got %d", params.DurationMs)
	}
	if len(params.Tenants) == 0 {
		return nil, fmt.Errorf("at least one tenant is required")
	}

	// Which arrival model this trace uses is decided by the tenants, and a trace may not be half of each.
	//
	// The refusal is deliberate rather than a fallback to one of them. A caller that sets both has two
	// different intentions on the page, and picking either silently produces a trace whose arrival process
	// cannot be stated -- which is the one thing a replayed measurement cannot afford to leave open.
	var rated, weighted []string
	for _, t := range params.Tenants {
		if t.RatePerSec > 0 {
			rated = append(rated, t.Tenant)
		}
		if t.Weight > 0 {
			weighted = append(weighted, t.Tenant)
		}
	}
	if len(rated) > 0 && len(weighted) > 0 {
		return nil, fmt.Errorf("tenants mix arrival models: %v carry ratePerSec and %v carry weight; a trace uses one model or the other", rated, weighted)
	}
	if len(rated) > 0 {
		if params.RatePerSec != 0 {
			return nil, fmt.Errorf("ratePerSec is %g and tenants carry their own rates; the total rate is meaningless under independent arrivals, so pass one or the other", params.RatePerSec)
		}
		// A tenant without a rate has an infinite mean gap, so it would silently get no arrivals at all.
		if len(rated) != len(params.Tenants) {
			return nil, fmt.Errorf("%d of %d tenants carry ratePerSec; under independent arrivals every tenant needs its own rate", len(rated), len(params.Tenants))
		}
		return generateIndependent(params)
	}

	// An infinite rate is a zero gap, so the offset never advances and the loop below never ends.
	if params.RatePerSec <= 0 || math.IsInf(params.RatePerSec, 0) {
		return nil, fmt.Errorf("ratePerSec must be positive and finite, got %f", params.RatePerSec)
	}

	var totalWeight float64
	for _, t := range params.Tenants {
		if t.Weight <= 0 {
			return nil, fmt.Errorf("tenant %q weight must be positive, got %f", t.Tenant, t.Weight)
		}
		totalWeight += t.Weight
	}

	// A fixed-seed PCG source makes every draw below reproducible.
	//
	// The two uint64 seeds are derived from the single int64 Seed so callers only track one number.
	src := rand.NewPCG(uint64(params.Seed), uint64(params.Seed)^0x9e3779b97f4a7c15)
	rng := rand.New(src)

	meanGapMs := 1000.0 / params.RatePerSec

	var rows []TraceRow
	var offset float64
	index := 0
	for {
		// Exponential inter-arrival gap: -mean * ln(U), which is the standard inverse-CDF draw for a Poisson process.
		u := rng.Float64()
		if u <= 0 {
			// Float64 can return 0 but never 1, so guard the log against -Inf.
			u = math.SmallestNonzeroFloat64
		}
		offset += -meanGapMs * math.Log(u)
		if offset >= float64(params.DurationMs) {
			break
		}
		if len(rows) == maxTraceRows {
			return nil, tooManyRows(params.DurationMs)
		}

		t := pickTenant(params.Tenants, totalWeight, rng.Float64())
		rows = append(rows, TraceRow{
			Index:           index,
			OffsetMs:        int64(offset),
			Tenant:          t.Tenant,
			PromptLenChars:  t.PromptLenChars,
			MaxOutputTokens: t.MaxOutputTokens,
			IsNoisy:         t.IsNoisy,
		})
		index++
	}

	return rows, nil
}

// generateIndependent gives every tenant its own Poisson process on its own stream, then merges them.
//
// The point of the separation is stated on TenantSpec.RatePerSec: a tenant's arrival times must be a
// function of that tenant alone, so that an experiment which varies one tenant's rate varies exactly one
// thing. Under the weighted model it varied two, and the M5-c ladder paid for four rungs before the second
// one was visible.
func generateIndependent(params TraceParams) ([]TraceRow, error) {
	seen := map[string]bool{}
	var rows []TraceRow
	for _, t := range params.Tenants {
		// Streams are keyed by name, so a duplicate name would silently replay one schedule twice.
		if seen[t.Tenant] {
			return nil, fmt.Errorf("tenant %q appears twice; under independent arrivals a name IS the stream key, so two rows of the same name would share a schedule", t.Tenant)
		}
		seen[t.Tenant] = true

		// An infinite rate is a zero gap, so this tenant's offset would never advance.
		if math.IsInf(t.RatePerSec, 0) {
			return nil, fmt.Errorf("tenant %q ratePerSec must be finite, got %f", t.Tenant, t.RatePerSec)
		}

		rng := tenantStream(params.Seed, t.Tenant)
		meanGapMs := 1000.0 / t.RatePerSec
		var offset float64
		for {
			u := rng.Float64()
			if u <= 0 {
				// Float64 can return 0 but never 1, so guard the log against -Inf.
				u = math.SmallestNonzeroFloat64
			}
			offset += -meanGapMs * math.Log(u)
			if offset >= float64(params.DurationMs) {
				break
			}
			// Counted across tenants, since the merged trace is what a replay has to hold.
			if len(rows) == maxTraceRows {
				return nil, tooManyRows(params.DurationMs)
			}
			rows = append(rows, TraceRow{
				OffsetMs:        int64(offset),
				Tenant:          t.Tenant,
				PromptLenChars:  t.PromptLenChars,
				MaxOutputTokens: t.MaxOutputTokens,
				IsNoisy:         t.IsNoisy,
			})
		}
	}

	// Merge into one non-decreasing schedule.
	//
	// The tie-break is on the tenant name rather than on the order the specs were passed in, for the same
	// reason the stream key is: reordering the slice must not reorder the trace, or the caller's argument
	// order becomes an unrecorded input to a checksummed artefact.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].OffsetMs != rows[j].OffsetMs {
			return rows[i].OffsetMs < rows[j].OffsetMs
		}
		return rows[i].Tenant < rows[j].Tenant
	})
	for i := range rows {
		rows[i].Index = i
	}
	return rows, nil
}

// tenantStream derives one tenant's generator stream from the seed and the tenant's NAME.
//
// Keying on the name rather than the position is what lets a tenant be added to or removed from a trace
// without moving anybody else's arrivals -- the property that makes the isolated baseline and the contended
// arms comparable, and the one a position-keyed stream would quietly break the first time a probe tenant
// was added.
func tenantStream(seed int64, tenant string) *rand.Rand {
	h := fnv.New64a()
	// Hash.Write never returns an error; the signature carries one only to satisfy io.Writer.
	_, _ = h.Write([]byte(tenant))
	n := h.Sum64()
	return rand.New(rand.NewPCG(uint64(seed)^n, uint64(seed)^(n*0x9e3779b97f4a7c15)))
}

// pickTenant maps a uniform draw in [0,1) to a tenant by normalized weight.
func pickTenant(tenants []TenantSpec, totalWeight, u float64) TenantSpec {
	target := u * totalWeight
	var acc float64
	for _, t := range tenants {
		acc += t.Weight
		if target < acc {
			return t
		}
	}
	// Floating-point rounding can leave target just at the top of the range; the last tenant owns it.
	return tenants[len(tenants)-1]
}

// WriteTrace serializes rows as JSON Lines, one row per line.
//
// JSON Lines is used so a trace streams row by row without loading the whole file, and so a single edited byte changes the checksum and is caught by LoadManifest.
func WriteTrace(w io.Writer, rows []TraceRow) error {
	bw := bufio.NewWriter(w)
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("marshal trace row %d: %w", r.Index, err)
		}
		if _, err := bw.Write(b); err != nil {
			return fmt.Errorf("write trace row %d: %w", r.Index, err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("write trace row %d newline: %w", r.Index, err)
		}
	}
	return bw.Flush()
}

// ReadTrace parses a JSON Lines trace produced by WriteTrace.
//
// It also verifies the offsets are non-decreasing, so a hand-edited trace that reorders arrivals is rejected rather than replayed out of order.
func ReadTrace(r io.Reader) ([]TraceRow, error) {
	var rows []TraceRow
	sc := bufio.NewScanner(r)
	// A long-context prompt row can exceed bufio.Scanner's default 64KB line cap, so raise it.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row TraceRow
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("parse trace row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].OffsetMs < rows[j].OffsetMs }) {
		return nil, fmt.Errorf("trace offsets are not non-decreasing")
	}
	return rows, nil
}
