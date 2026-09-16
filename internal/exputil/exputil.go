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

// Package exputil holds the experiment-harness discipline shared across the M5-b benchmark and the
// queue-policy lab: content checksums, JSON Lines evidence, tail percentiles, and a repetition-level
// bootstrap.
//
// It deliberately carries no experiment-specific schema, so the inference harness and the queue lab
// each keep their own row types and only share the mechanics that make a comparison reproducible.
package exputil

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"sort"
)

// Checksum returns the hex-encoded sha256 of b.
//
// An experiment stamps its immutable trace with this so the report can prove every arm replayed the
// same traffic rather than trusting operator discipline.
func Checksum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ChecksumFile returns the hex-encoded sha256 of the file at path, streamed so a large file need not
// fit in memory.
func ChecksumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteJSONL serializes rows as JSON Lines, one row per line.
//
// JSON Lines is used so evidence streams row by row and a single edited byte changes the checksum.
func WriteJSONL[T any](w io.Writer, rows []T) error {
	bw := bufio.NewWriter(w)
	for i, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("marshal row %d: %w", i, err)
		}
		if _, err := bw.Write(b); err != nil {
			return fmt.Errorf("write row %d: %w", i, err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("write row %d newline: %w", i, err)
		}
	}
	return bw.Flush()
}

// ReadJSONL parses a JSON Lines file produced by WriteJSONL, skipping blank lines.
//
// The scan buffer is raised because a single row (e.g. a long prompt or a captured spec) can exceed
// the default 64KB line cap.
func ReadJSONL[T any](r io.Reader) ([]T, error) {
	var rows []T
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row T
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("parse row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return rows, nil
}

// Percentile returns the q-quantile of a sorted slice using nearest-rank, or 0 for an empty slice.
//
// Nearest-rank (not interpolation) is used because a tail claim should land on an actually observed
// value, not one that never occurred.
func Percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	rank = max(rank, 0)
	rank = min(rank, len(sorted)-1)
	return sorted[rank]
}

// CI is a two-sided confidence interval.
type CI struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// BootstrapCI returns a percentile-bootstrap confidence interval for the mean of values.
//
// values are the PER-REPETITION statistics (or paired per-repetition deltas), and this resamples whole
// repetitions with replacement, because observations within one live run are correlated and a naive
// bootstrap over pooled observations would understate the interval.
//
// alpha is the two-sided error (0.05 -> 95%), iterations sets the resample count, seed makes it reproducible.
func BootstrapCI(values []float64, iterations int, seed int64, alpha float64) CI {
	if len(values) == 0 {
		return CI{}
	}
	if len(values) == 1 {
		// A single repetition cannot bound its own variance, so the interval degenerates to the point estimate.
		return CI{Lo: values[0], Hi: values[0]}
	}

	src := rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15)
	rng := rand.New(src)

	means := make([]float64, iterations)
	n := len(values)
	for i := range iterations {
		var sum float64
		for range n {
			sum += values[rng.IntN(n)]
		}
		means[i] = sum / float64(n)
	}
	sort.Float64s(means)
	return CI{
		Lo: Percentile(means, alpha/2),
		Hi: Percentile(means, 1-alpha/2),
	}
}
