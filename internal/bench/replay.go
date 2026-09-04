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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// RawRow is the durable per-request evidence recorded during replay.
//
// Every latency number a report makes comes from these client-side timestamps, never from a server-side metric, because the gateway's own histogram starts just before the proxy handoff and so is not TTFT (design spec "Benchmarking harness" section).
//
// A timed-out or failed request is still recorded, with ErrorKind set and the missing timestamps left zero, so a censored tail can be reported honestly instead of silently dropping the slow requests that matter most.
type RawRow struct {
	// Index ties this row back to its TraceRow.
	Index int `json:"index"`
	// Arm is the condition this replay measured, copied from the manifest so a merged raw file keeps arms separable.
	Arm string `json:"arm"`
	// Tenant is the sending tenant, copied from the trace.
	Tenant string `json:"tenant"`
	// IsNoisy marks contender traffic, copied from the trace so the report can split victim from neighbor.
	IsNoisy bool `json:"isNoisy"`
	// ScheduledOffsetMs is the trace's intended arrival offset, kept so a report can check the sender did not fall behind schedule.
	ScheduledOffsetMs int64 `json:"scheduledOffsetMs"`
	// SendUnixNanos is when the request was actually dispatched.
	//
	// The scheduler stamps it at dispatch and never waits for prior responses, so a slow backend cannot push this later than the schedule (which would be coordinated omission).
	SendUnixNanos int64 `json:"sendUnixNanos"`
	// FirstTokenUnixNanos is when the first response token arrived; zero if the request failed before any token.
	FirstTokenUnixNanos int64 `json:"firstTokenUnixNanos,omitempty"`
	// EndUnixNanos is when the response finished; zero if the request never completed.
	EndUnixNanos int64 `json:"endUnixNanos,omitempty"`
	// EstInputTokens is the gateway-style conservative estimate for this prompt, recorded so the report can measure admitted vs offered work for admission matching.
	EstInputTokens int `json:"estInputTokens"`
	// OutputTokens is the number of tokens the response produced.
	OutputTokens int `json:"outputTokens"`
	// HTTPStatus is the response status; 0 when no response was received (a transport error or timeout).
	HTTPStatus int `json:"httpStatus"`
	// ErrorKind names the failure when the request did not complete normally, and is empty on success.
	//
	// It is a small closed vocabulary ("timeout", "transport", "rejected", "stream") so a report can bucket failures without parsing free text.
	ErrorKind string `json:"errorKind,omitempty"`
	// TraceChecksum is the sha256 of the trace this row replayed, copied from the frozen manifest.
	//
	// The report asserts the contended arms all carry one checksum, so it can prove they replayed identical traffic rather than trusting operator discipline.
	TraceChecksum string `json:"traceChecksum,omitempty"`
	// LongThreshold is the eligible-population token threshold in effect, copied from the manifest.
	//
	// The report measures admitted-work over this same threshold, so if the paid run tuned it the report cannot silently score a different population than the guard gated.
	LongThreshold int `json:"longThreshold,omitempty"`
	// ExactInputTokens is the trace's measured count for this prompt, carried through so a refused request
	// still contributes to the offered side of the admitted-work fraction.
	ExactInputTokens int `json:"exactInputTokens,omitempty"`
	// EngineInputTokens is what the engine itself reported for this request, when it answered one.
	//
	// It exists to check ExactInputTokens rather than to replace it: the trace's value is measured once per
	// prompt length, and this is measured on every admitted request, so a disagreement means the trace was
	// stamped against a different tokenizer or a different prompt than the one that ran.
	EngineInputTokens int `json:"engineInputTokens,omitempty"`
	// BackendState is the pressure reading the guard's decision was made from, verbatim as the gateway
	// reported it: "kv=0.834,waiting=7,engaged=0,fresh=1".
	//
	// Kept as the gateway's own string rather than parsed into fields here, so the evidence records what was
	// said rather than this package's reading of it, and a format change shows up as an unparseable value
	// instead of silently becoming zeros.
	BackendState string `json:"backendState,omitempty"`
	// Tier and AdmissionReason are what the GATEWAY decided, read off its response rather than assumed here.
	//
	// AdmissionReason is recorded for admits as well as refusals, because arm C admits for four different
	// reasons and two of them mean the guard was not working: a backend it never registered, and telemetry
	// too stale to read. A run spent entirely in that bypass is arm A wearing arm C's name.
	//
	// Both were absent from the 2026-09-03 evidence and both had to be reconstructed months later from
	// configuration the evidence did not contain. Tier decides membership of the eligible population -- the
	// gateway gates on tier == standard AND the threshold, while a report with no tier could only read the
	// threshold. AdmissionReason separates a bucket that is momentarily empty from a request larger than the
	// bucket can ever hold, which is the difference between a tuning that is tight and one that is broken.
	//
	// Empty on evidence written before the gateway reported them, and every consumer treats empty as
	// "not recorded" rather than as a value, so old runs keep scoring the way they did.
	Tier            string `json:"tier,omitempty"`
	AdmissionReason string `json:"admissionReason,omitempty"`
	// MatchTolerance is the pre-registered admission-match tolerance, copied from the manifest.
	//
	// The report reads it from here rather than a CLI default, so the frozen tolerance cannot be loosened after the fact.
	MatchTolerance float64 `json:"matchTolerance,omitempty"`
}

// TTFTNanos returns the time to first token, or (0,false) when no first token was recorded.
func (r RawRow) TTFTNanos() (int64, bool) {
	if r.SendUnixNanos == 0 || r.FirstTokenUnixNanos == 0 {
		return 0, false
	}
	return r.FirstTokenUnixNanos - r.SendUnixNanos, true
}

// E2ENanos returns the end-to-end latency, or (0,false) when the request never completed.
func (r RawRow) E2ENanos() (int64, bool) {
	if r.SendUnixNanos == 0 || r.EndUnixNanos == 0 {
		return 0, false
	}
	return r.EndUnixNanos - r.SendUnixNanos, true
}

// SendResult is what a Sender reports back about one dispatched request.
type SendResult struct {
	// FirstTokenUnixNanos is when the first token arrived; zero if none did.
	FirstTokenUnixNanos int64
	// EndUnixNanos is when the response finished; zero if it never did.
	EndUnixNanos int64
	// OutputTokens is the response length in tokens.
	OutputTokens int
	// PromptTokens is the engine's own count of the prompt, zero when it reported none.
	PromptTokens int
	// BackendState is the gateway's report of the pressure its decision used, empty when it reported none.
	BackendState string
	// HTTPStatus is the response status; 0 for a transport error or timeout.
	HTTPStatus int
	// ErrorKind names the failure, empty on success.
	ErrorKind string
	// Tier and AdmissionReason are what the gateway reported about its own admission decision.
	//
	// Empty against a gateway that does not report them, which is how evidence written before it did is
	// distinguished from a gateway that decided "no tier".
	Tier            string
	AdmissionReason string
}

// Sender dispatches one request and reports its raw result.
//
// It is an interface so the open-loop scheduler can be tested against an in-memory stub, including a deliberately slow one, without a real gateway or backend.
type Sender interface {
	// Send performs the request described by row and returns its timing result.
	//
	// The stamp argument is the moment the scheduler dispatched the request, so a Sender that cannot itself observe first-token time can still return timestamps on the same clock the scheduler used.
	Send(ctx context.Context, row TraceRow, sendUnixNanos int64) SendResult
}

// ReplayOptions configure a replay run.
type ReplayOptions struct {
	// Arm is copied into every RawRow.
	Arm string
	// TraceChecksum, LongThreshold, and MatchTolerance are the frozen manifest provenance stamped into every RawRow, so the report can enforce trace identity and read the pre-registered knobs from the evidence itself.
	TraceChecksum  string
	LongThreshold  int
	MatchTolerance float64
	// EstInputTokens estimates a prompt's input tokens the same way the gateway does, so admitted-work can be measured; when nil, a default ceiling-of-chars/4 estimate is used.
	EstInputTokens func(promptLenChars int) int
	// clock and sleepUntil are injected only by tests; production uses the wall clock.
	clock      func() time.Time
	sleepUntil func(ctx context.Context, t time.Time)
}

// Replay sends every request in trace against sender at its scheduled offset and returns the raw evidence.
//
// The load is open-loop: the scheduler sleeps until each request's offset and then dispatches it on its own goroutine, never waiting for earlier responses to return.
//
// A slow or hung backend therefore delays only its own row's completion timestamps, not the send times of the requests that follow it, which is the property that keeps a p99 measurement honest.
func Replay(ctx context.Context, sender Sender, trace []TraceRow, opts ReplayOptions) []RawRow {
	clock := opts.clock
	if clock == nil {
		clock = time.Now
	}
	sleepUntil := opts.sleepUntil
	if sleepUntil == nil {
		sleepUntil = wallSleepUntil
	}
	estInput := opts.EstInputTokens
	if estInput == nil {
		estInput = defaultEstInputTokens
	}

	rows := make([]RawRow, len(trace))
	var wg sync.WaitGroup
	start := clock()

	for i, tr := range trace {
		fireAt := start.Add(time.Duration(tr.OffsetMs) * time.Millisecond)
		sleepUntil(ctx, fireAt)
		if ctx.Err() != nil {
			// The run was cancelled; leave the remaining rows as recorded so far.
			break
		}

		wg.Add(1)
		go func(i int, tr TraceRow) {
			defer wg.Done()
			sendNanos := clock().UnixNano()
			res := sender.Send(ctx, tr, sendNanos)
			rows[i] = RawRow{
				Index:               tr.Index,
				Arm:                 opts.Arm,
				Tenant:              tr.Tenant,
				IsNoisy:             tr.IsNoisy,
				ScheduledOffsetMs:   tr.OffsetMs,
				SendUnixNanos:       sendNanos,
				FirstTokenUnixNanos: res.FirstTokenUnixNanos,
				EndUnixNanos:        res.EndUnixNanos,
				EstInputTokens:      estInput(tr.PromptLenChars),
				ExactInputTokens:    tr.ExactInputTokens,
				EngineInputTokens:   res.PromptTokens,
				BackendState:        res.BackendState,
				OutputTokens:        res.OutputTokens,
				HTTPStatus:          res.HTTPStatus,
				ErrorKind:           res.ErrorKind,
				Tier:                res.Tier,
				AdmissionReason:     res.AdmissionReason,
				TraceChecksum:       opts.TraceChecksum,
				LongThreshold:       opts.LongThreshold,
				MatchTolerance:      opts.MatchTolerance,
			}
		}(i, tr)
	}

	wg.Wait()

	// Rows are indexed by position, so a cancelled run leaves zero-value rows at the tail; drop those.
	out := rows[:0]
	for _, r := range rows {
		if r.SendUnixNanos != 0 {
			out = append(out, r)
		}
	}
	return out
}

// wallSleepUntil blocks until t, or until ctx is done, using the real clock.
func wallSleepUntil(ctx context.Context, t time.Time) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// defaultEstInputTokens mirrors the gateway's conservative ceiling-of-bytes/4 estimate.
func defaultEstInputTokens(promptLenChars int) int {
	return EstInputTokensForChars(promptLenChars)
}

// EstInputTokensForChars is the gateway's ceiling-of-bytes/4 input estimate, exported for the offline
// simulation that freezes arm B's bucket tuning.
//
// It is exported rather than copied because the simulation decides which rows the guard would have called
// eligible, and a second copy of this arithmetic would answer that question its own way the first time either
// side changed.
func EstInputTokensForChars(promptLenChars int) int {
	return (promptLenChars + 3) / 4
}

// WriteRawRows serializes rows as JSON Lines, the durable evidence file.
func WriteRawRows(w io.Writer, rows []RawRow) error {
	bw := bufio.NewWriter(w)
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("marshal raw row %d: %w", r.Index, err)
		}
		if _, err := bw.Write(b); err != nil {
			return fmt.Errorf("write raw row %d: %w", r.Index, err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("write raw row %d newline: %w", r.Index, err)
		}
	}
	return bw.Flush()
}

// ReadRawRows parses a JSON Lines raw-evidence file produced by WriteRawRows.
func ReadRawRows(r io.Reader) ([]RawRow, error) {
	var rows []RawRow
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row RawRow
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("parse raw row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read raw rows: %w", err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Index < rows[j].Index })
	return rows, nil
}
