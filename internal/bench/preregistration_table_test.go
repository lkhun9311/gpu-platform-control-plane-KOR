package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestThePreRegistrationTableMatchesTheEvidenceItCitesFrom ties the spec's numbers to the report code.
//
// The paid 2026-09-03 raw evidence is 7 MB and deliberately gitignored, so a table typed into a design
// document has, by default, nothing in the repository comparing it to anything. That is the exact shape of
// every overclaim this project has had to withdraw: a value living in two places with no ruler between
// them. The report now emits a machine-readable summary beside its text, that file is committed, and this
// test asserts the document agrees with it digit for digit.
//
// It cannot prove the JSON came from the evidence -- only re-running the report against the raw files does
// that. What it does prove is that nobody can adjust a number in the write-up without the derivation
// disagreeing, which is the failure mode that actually happened.
func TestThePreRegistrationTableMatchesTheEvidenceItCitesFrom(t *testing.T) {
	const (
		specPath = "../../docs/superpowers/specs/2026-09-05-the-price-of-protection.md"
		dataPath = "../../docs/superpowers/specs/data/2026-09-03-arm-summaries.json"
	)
	blob, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("read summaries: %v", err)
	}
	var summaries []ArmSummary
	if err := json.Unmarshal(blob, &summaries); err != nil {
		t.Fatalf("decode summaries: %v", err)
	}
	byArm := map[string]ArmSummary{}
	for _, s := range summaries {
		byArm[s.Arm] = s
	}

	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	// Cells are split on the pipe rather than matched with one regular expression.
	//
	// The table is pipe-aligned for the author's editor, so column widths change whenever a value does, and
	// a regex that encodes the padding breaks on formatting rather than on a wrong number -- which is a
	// check that fails for the wrong reason and gets relaxed until it stops failing at all.
	type specRow struct {
		arm, p99, rate, premium, noisy, tpot string
	}
	var rows []specRow
	for line := range strings.SplitSeq(string(spec), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		var cells []string
		for c := range strings.SplitSeq(strings.Trim(strings.TrimSpace(line), "|"), "|") {
			cells = append(cells, strings.TrimSpace(c))
		}
		if len(cells) != 6 || !strings.HasSuffix(cells[1], " ms") {
			continue
		}
		// "R1 (isolated)" names arm R1. The parenthetical is for the reader.
		arm := strings.Fields(cells[0])[0]
		rows = append(rows, specRow{arm, strings.TrimSuffix(cells[1], " ms"), cells[2], cells[3], cells[4], cells[5]})
	}
	if len(rows) != 4 {
		t.Fatalf("found %d evidence rows in %s, want 4 (R1, off, static-cap, kv-aware)", len(rows), specPath)
	}
	for _, r := range rows {
		s, ok := byArm[r.arm]
		if !ok {
			t.Fatalf("the spec cites arm %q, which is not in %s", r.arm, dataPath)
		}
		wantP99 := mustFloat(t, strings.ReplaceAll(r.p99, ",", ""))
		if fmt.Sprintf("%.1f", s.TTFTMsP99) != fmt.Sprintf("%.1f", wantP99) {
			t.Errorf("%s: spec says p99 %.1f ms, the evidence summary says %.1f", r.arm, wantP99, s.TTFTMsP99)
		}
		wantRate := mustFloat(t, r.rate)
		if fmt.Sprintf("%.1f", s.OutputTokensPerSecond) != fmt.Sprintf("%.1f", wantRate) {
			t.Errorf("%s: spec says %.1f output tok/s, the evidence summary says %.1f",
				r.arm, wantRate, s.OutputTokensPerSecond)
		}
		if got := sharePct(s, "premium-1"); fmt.Sprintf("%.0f%%", got) != r.premium {
			t.Errorf("%s: spec says premium share %s, the evidence summary says %.0f%%", r.arm, r.premium, got)
		}
		// The PREMIUM tenant's TPOT, not the arm-wide one.
		//
		// Reading 1 fails a cell on the protected tenant's stream, and the arm-wide figure pools every
		// tenant: on kv-aware they are 265.1 and 209.4, so reconciling against the wrong one would let the
		// document quote a number that describes the contender.
		wantTPOT := mustFloat(t, strings.TrimSuffix(r.tpot, " ms"))
		gotTPOT, ok := s.TPOTMsP99ByTenant[PremiumTenant]
		if !ok {
			t.Errorf("%s: the evidence summary carries no premium TPOT to compare against", r.arm)
		} else if fmt.Sprintf("%.1f", gotTPOT) != fmt.Sprintf("%.1f", wantTPOT) {
			t.Errorf("%s: spec says premium TPOT p99 %.1f ms, the evidence summary says %.1f",
				r.arm, wantTPOT, gotTPOT)
		}
		// The noisy column is the one the whole document turns on: an arm that "protected" the tail by
		// deleting the contending tenant reads 0% here.
		//
		// Only R1 may write an em dash, and only because its trace has no contending tenant to serve. For
		// every other arm a missing map entry MEANS zero and has to be written as zero. The first version of
		// this check skipped whenever the tenant was absent and the spec said "—", which permitted exactly
		// the erasure the comment claimed to prevent: static-cap's 0% could have been quietly written as an
		// em dash and this test would have passed.
		if r.arm == ArmR1 {
			if r.noisy != "—" {
				t.Errorf("R1 has no contending tenant, so its noisy share must be an em dash, not %q", r.noisy)
			}
			continue
		}
		if got := fmt.Sprintf("%.0f%%", sharePct(s, "standard-noisy")); got != r.noisy {
			t.Errorf("%s: spec says noisy share %s, the evidence summary says %s", r.arm, r.noisy, got)
		}
	}
}

func sharePct(s ArmSummary, tenant string) float64 {
	if s.OutputTokens == 0 {
		return 0
	}
	return 100 * float64(s.OutputTokensByTenant[tenant]) / float64(s.OutputTokens)
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}
