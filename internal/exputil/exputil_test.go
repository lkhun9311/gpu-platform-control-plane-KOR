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

package exputil

import (
	"bytes"
	"testing"
)

type sample struct {
	N int    `json:"n"`
	S string `json:"s"`
}

func TestChecksumIsStableAndSensitive(t *testing.T) {
	a := Checksum([]byte("hello"))
	b := Checksum([]byte("hello"))
	c := Checksum([]byte("hellp"))
	if a != b {
		t.Fatalf("checksum not stable: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("checksum insensitive to a changed byte")
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	rows := []sample{{1, "a"}, {2, "b"}, {3, "c"}}
	var buf bytes.Buffer
	if err := WriteJSONL(&buf, rows); err != nil {
		t.Fatal(err)
	}
	got, err := ReadJSONL[sample](bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows))
	}
	for i := range rows {
		if got[i] != rows[i] {
			t.Fatalf("row %d: got %+v want %+v", i, got[i], rows[i])
		}
	}
	// A changed byte in the serialized evidence must change the checksum.
	if Checksum(buf.Bytes()) == Checksum(append(buf.Bytes(), 'x')) {
		t.Fatalf("evidence checksum not sensitive")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := map[float64]float64{0.50: 5, 0.90: 9, 0.99: 10}
	for q, want := range cases {
		if got := Percentile(s, q); got != want {
			t.Fatalf("Percentile(%.2f)=%v want %v", q, got, want)
		}
	}
	if got := Percentile(nil, 0.5); got != 0 {
		t.Fatalf("Percentile(empty)=%v want 0", got)
	}
}

func TestBootstrapCI(t *testing.T) {
	if ci := BootstrapCI([]float64{42}, 1000, 7, 0.05); ci.Lo != 42 || ci.Hi != 42 {
		t.Fatalf("single-value CI = %+v, want {42,42}", ci)
	}
	vals := []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19} // mean 14.5
	a := BootstrapCI(vals, 2000, 99, 0.05)
	b := BootstrapCI(vals, 2000, 99, 0.05)
	if a != b {
		t.Fatalf("bootstrap not reproducible: %+v vs %+v", a, b)
	}
	if !(a.Lo < 14.5 && a.Hi > 14.5) {
		t.Fatalf("CI %+v does not bracket the mean 14.5", a)
	}
}

// TestPairedDeltaCIExcludesZero checks the pattern the resumable-training A/B relies on: a paired
// improvement whose interval excludes zero.
func TestPairedDeltaCIExcludesZero(t *testing.T) {
	// Per-repetition deltas (resume goodput minus restart goodput), all clearly positive.
	deltas := []float64{0.34, 0.37, 0.36, 0.39, 0.35}
	ci := BootstrapCI(deltas, 3000, 1, 0.05)
	if ci.Lo <= 0 {
		t.Fatalf("expected the improvement CI to exclude zero, got %+v", ci)
	}
}
