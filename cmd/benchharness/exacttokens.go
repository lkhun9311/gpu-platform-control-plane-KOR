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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// stampExactTokens measures each distinct prompt length in a trace against the running engine and writes the
// counts back into the trace.
//
// The design defines the admission-match criterion over the served tokenizer's own input-token count. Three
// paid runs scored it on ceil(chars/4) instead -- a quantity this project's calibration records as 36 percent
// low on a 200-character prompt and 23 percent high on a 40,000-character one -- so the pre-registered
// criterion has never been evaluated, only a proxy for it.
//
// The engine is asked rather than a tokenizer imported, for two reasons. It is the authority: whatever its
// tokenizer and chat template do IS the number the criterion means. And a tokenizer vendored here would be a
// second copy of a decision that lives in the served model, drifting the first time either moved.
//
// A trace has a handful of distinct prompt lengths, so this costs one request each and runs once per session,
// before any arm does.
func stampExactTokens(args []string) error {
	fs := flag.NewFlagSet("stamp-exact-tokens", flag.ExitOnError)
	tracePath := fs.String("trace", "", "trace file to measure and rewrite in place (required)")
	gatewayURL := fs.String("gateway-url", "", "gateway base URL (required)")
	model := fs.String("model", "", "model name the engine serves (required)")
	apiKeys := fs.String("api-keys", "", "tenant=key pairs, comma separated")
	tenant := fs.String("tenant", "", "which tenant to measure as; must be one admission never refuses")
	timeout := fs.Duration("timeout", 60*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tracePath == "" || *gatewayURL == "" || *model == "" {
		return fmt.Errorf("-trace, -gateway-url and -model are all required")
	}

	rows, err := readTrace(*tracePath)
	if err != nil {
		return err
	}
	lengths := map[int]bool{}
	for _, r := range rows {
		lengths[r.PromptLenChars] = true
	}
	distinct := make([]int, 0, len(lengths))
	for n := range lengths {
		distinct = append(distinct, n)
	}
	sort.Ints(distinct)

	keys := map[string]string{}
	for pair := range strings.SplitSeq(*apiKeys, ",") {
		if k, v, ok := strings.Cut(pair, "="); ok {
			keys[k] = v
		}
	}
	sender := bench.NewHTTPSender(*gatewayURL, *model, keys, *timeout,
		bench.SenderConn{MaxIdleConnsPerHost: 2, DrainForReuse: true})

	measured := map[int]int{}
	for _, n := range distinct {
		// One output token, because the prompt count is what is being measured and generation is only a cost.
		probe := bench.TraceRow{Index: -1, Tenant: *tenant, PromptLenChars: n, MaxOutputTokens: 1}
		res := sender.Send(context.Background(), probe, time.Now().UnixNano())
		if res.PromptTokens <= 0 {
			return fmt.Errorf("the engine reported no prompt-token count for a %d-character prompt (status %d, error %q). Without it the admission-match criterion cannot be evaluated in the units the design defines it in, and scoring it on the estimate is what left it unevaluated for three paid runs", n, res.HTTPStatus, res.ErrorKind)
		}
		measured[n] = res.PromptTokens
		fmt.Printf("%7d chars -> %6d tokens (estimate %d)\n", n, res.PromptTokens, bench.EstInputTokensForChars(n))
	}

	for i := range rows {
		rows[i].ExactInputTokens = measured[rows[i].PromptLenChars]
	}
	return writeTrace(*tracePath, rows)
}

// readTrace loads a JSON Lines trace.
func readTrace(path string) ([]bench.TraceRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	var rows []bench.TraceRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r bench.TraceRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("parse trace row: %w", err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s has no rows", path)
	}
	return rows, nil
}

// writeTrace rewrites the trace, via a temporary file so a failure cannot leave a half-written one.
func writeTrace(path string, rows []bench.TraceRow) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, r := range rows {
		enc, merr := json.Marshal(r)
		if merr != nil {
			f.Close() //nolint:errcheck // already failing
			return fmt.Errorf("marshal trace row: %w", merr)
		}
		w.Write(enc)      //nolint:errcheck // flush reports
		w.WriteByte('\n') //nolint:errcheck // flush reports
	}
	if err := w.Flush(); err != nil {
		f.Close() //nolint:errcheck // already failing
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}
