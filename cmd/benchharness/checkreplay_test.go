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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/bench"
)

// A replay is checked as it lands, not three hours later when the report runs.
//
// The runner's per-replay check asked one question -- did anything at all complete -- and a replay that lost
// a third of its traffic answered yes. The report can now refuse such a run, but only after every remaining
// replay has been paid for. These cases are the ones that must stop the run where they happen.
func writeReplay(t *testing.T, dir string, rows []bench.RawRow) string {
	t.Helper()
	path := filepath.Join(dir, "raw-static-cap-1.jsonl")
	var b strings.Builder
	for _, r := range rows {
		enc, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func premiumRow(i int, status int, errKind string) bench.RawRow {
	base := int64(1_000_000_000 + i*1_000_000)
	r := bench.RawRow{
		Index: i, Arm: "static-cap", Tenant: "premium-1", SendUnixNanos: base,
		EstInputTokens: 50, OutputTokens: 8, HTTPStatus: status, ErrorKind: errKind,
		TraceChecksum: "T", LongThreshold: 4096, MatchTolerance: 0.05,
	}
	if status == 200 {
		r.FirstTokenUnixNanos = base + 50_000_000
		r.EndUnixNanos = base + 60_000_000
	}
	return r
}

func noisyRow(i int, status int, errKind string) bench.RawRow {
	r := premiumRow(i, status, errKind)
	r.Tenant = "standard-noisy"
	r.IsNoisy = true
	r.EstInputTokens = 10000
	return r
}

func healthy() []bench.RawRow {
	var rows []bench.RawRow
	for i := 1; i <= 200; i++ {
		rows = append(rows, premiumRow(i, 200, ""))
	}
	for i := 201; i <= 400; i++ {
		rows = append(rows, noisyRow(i, 200, ""))
	}
	return rows
}

func TestCheckReplay(t *testing.T) {
	cases := []struct {
		name  string
		rows  func() []bench.RawRow
		wants string // empty means the replay must be accepted
	}{
		{"a healthy replay", healthy, ""},
		{
			// The only case the old check caught.
			"a replay that completed nothing",
			func() []bench.RawRow {
				rows := healthy()
				for i := range rows {
					rows[i].HTTPStatus = 0
					rows[i].ErrorKind = "transport"
					rows[i].FirstTokenUnixNanos, rows[i].EndUnixNanos = 0, 0
				}
				return rows
			},
			"completed nothing",
		},
		{
			// 92 of these per replay survived a whole paid run.
			"a replay with a single forbidden request",
			func() []bench.RawRow {
				rows := healthy()
				rows[0].HTTPStatus = 403
				return rows
			},
			"401 or 403",
		},
		{
			// The reproduced hole: a third of the eligible traffic gone, and the old check said yes.
			"a replay whose eligible traffic partly died in transport",
			func() []bench.RawRow {
				rows := healthy()
				for i := 200; i < 260; i++ {
					rows[i].HTTPStatus = 0
					rows[i].ErrorKind = "transport"
					rows[i].FirstTokenUnixNanos, rows[i].EndUnixNanos = 0, 0
				}
				return rows
			},
			"admission verdict",
		},
		{
			// The tail is the primary endpoint, so premium losses end the run for the same reason.
			"a replay whose premium tail lost more than one percent",
			func() []bench.RawRow {
				rows := healthy()
				for i := range 3 {
					rows[i].HTTPStatus = 0
					rows[i].ErrorKind = "transport"
					rows[i].FirstTokenUnixNanos, rows[i].EndUnixNanos = 0, 0
				}
				return rows
			},
			"premium",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeReplay(t, t.TempDir(), tc.rows())
			err := checkReplay([]string{"-raw", path})
			if tc.wants == "" {
				if err != nil {
					t.Fatalf("a usable replay was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("an unusable replay was accepted; the run would have continued and paid for every remaining replay")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusal does not say why:\n  got:  %v\n  want it to mention: %s", err, tc.wants)
			}
		})
	}
}
