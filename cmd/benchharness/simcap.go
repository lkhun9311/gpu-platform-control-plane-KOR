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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/gateway"
)

// simCap replays a trace's arrival times through the real static-cap admitter and reports what fraction of
// the eligible offered tokens a given rate and burst would admit.
//
// This is the design spec's Arm B tuning procedure, which asks for the bucket to be simulated against a
// pilot's arrival trace and the values frozen before the confirmatory run. It had never been run. The first
// confirmatory run instead received the trace's REQUEST rate in a flag measured in tokens per second, with
// the burst left at a default below the largest prompt, and arm B refused every eligible request for three
// hours -- reporting cleanly the whole time, because nothing compared the tuning against the traffic.
//
// It drives internal/gateway's own admitter through an injected clock rather than reimplementing the bucket.
// A simulator that agreed with the gateway today would be one more value held in two places, which is the
// shape behind every defect this experiment has produced.
func simCap(args []string) error {
	fs := flag.NewFlagSet("sim-cap", flag.ExitOnError)
	tracePath := fs.String("trace", "", "trace file to simulate against (required)")
	rate := fs.Float64("rate", 0, "bucket refill rate in tokens/sec (required)")
	burst := fs.Int("burst", 0, "bucket capacity in tokens (required)")
	longThreshold := fs.Int("long-threshold", 4096, "minimum estimated input tokens for the eligible population")
	premium := fs.String("premium-tenants", "", "comma-separated tenants the gateway resolves to the premium tier; every other tenant is standard")
	target := fs.Float64("target-admitted-fraction", 0, "when set, exit non-zero unless the simulated fraction lands within -tolerance of it")
	tolerance := fs.Float64("tolerance", 0.05, "RELATIVE tolerance on -target-admitted-fraction, matching the design spec's |B-C|/C contract")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tracePath == "" || *rate <= 0 || *burst <= 0 {
		return fmt.Errorf("-trace, -rate and -burst are all required")
	}

	var premiumTenants []string
	if *premium != "" {
		premiumTenants = strings.Split(*premium, ",")
	}

	f, err := os.Open(*tracePath)
	if err != nil {
		return fmt.Errorf("open trace: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	// The clock is the trace's own arrival offsets, so a ten-minute replay simulates in milliseconds.
	//
	// These are the SCHEDULED offsets, not the times requests actually reached the gateway: the replay client
	// dispatches open-loop and the recorded sendUnixNanos are not perfectly ordered by index. So this is the
	// intended arrival pattern through the real bucket, not a reconstruction of one run's true spacing.
	origin := time.Unix(0, 0)
	var at time.Time
	admitter := gateway.NewStaticCapAdmitterAtClock(*rate, *burst, *longThreshold, func() time.Time { return at })
	backend := &gateway.BackendRef{Namespace: "sim", Name: "engine", Port: 8000, URL: &url.URL{Scheme: "http", Host: "sim"}}

	var offered, admitted int64
	var eligible, refused int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row bench.TraceRow
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("parse trace row: %w", err)
		}
		tier := "standard"
		if slices.Contains(premiumTenants, row.Tenant) {
			tier = "premium"
		}
		est := bench.EstInputTokensForChars(row.PromptLenChars)
		if tier != "standard" || est < *longThreshold {
			continue
		}
		at = origin.Add(time.Duration(row.OffsetMs) * time.Millisecond)
		eligible++
		offered += int64(est)
		ok, _ := admitter.Admit(context.Background(), gateway.RequestMeta{Model: "sim", EstInputTokens: est}, backend, row.Tenant, tier)
		if ok {
			admitted += int64(est)
		} else {
			refused++
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read trace: %w", err)
	}
	if offered == 0 {
		return fmt.Errorf("no eligible requests in %s at a threshold of %d tokens; the simulation has nothing to say about a bucket that would never be consulted", *tracePath, *longThreshold)
	}

	fraction := float64(admitted) / float64(offered)
	fmt.Printf("eligible=%d refused=%d offered=%d admitted=%d fraction=%.4f\n", eligible, refused, offered, admitted, fraction)
	if *target > 0 {
		// Relative, because the pre-registered admission-match check is |B-C|/C and an absolute band of the
		// same width is a different, looser contract: at a target of 0.8456 an absolute 0.05 admits 0.796,
		// which is 5.9 percent off relative and would fail the check this is meant to pre-empt.
		rel := (fraction - *target) / *target
		if rel > *tolerance || rel < -*tolerance {
			return fmt.Errorf("rate %.0f tok/s with burst %d admits %.4f of the eligible offered tokens against a frozen target of %.4f, which is %.1f percent off relative and outside the %.1f percent allowed. The tuning and the traffic disagree, so arm B would not be admission-matched to C and the incremental-value comparison would measure the mismatch instead of the guard", *rate, *burst, fraction, *target, rel*100, *tolerance*100)
		}
	}
	return nil
}
