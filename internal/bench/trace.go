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
}

// TraceParams are the inputs GenerateTrace turns into a deterministic open-loop trace.
type TraceParams struct {
	// Seed makes generation deterministic: the same seed and params always produce the same trace, hence the same checksum.
	Seed int64
	// DurationMs is the wall-clock span of arrivals the trace covers.
	DurationMs int64
	// RatePerSec is the mean total arrival rate across all tenants, realized as a Poisson process.
	RatePerSec float64
	// Tenants describes each tenant's share and request shape; at least one is required.
	Tenants []TenantSpec
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
	if params.RatePerSec <= 0 {
		return nil, fmt.Errorf("ratePerSec must be positive, got %f", params.RatePerSec)
	}
	if len(params.Tenants) == 0 {
		return nil, fmt.Errorf("at least one tenant is required")
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
		if int64(offset) >= params.DurationMs {
			break
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
