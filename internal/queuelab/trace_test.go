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

package queuelab

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/exputil"
)

const tenantA = "tenant-a"

// jsonRow renders one trace row as a JSON Lines record, so malformed-input tests can vary a single field
// without repeating the whole object literal.
func jsonRow(index int, name string, offsetMs, gpu, dur int) string {
	return fmt.Sprintf(`{"index":%d,"name":%q,"offsetMs":%d,"tenant":%q,"gpuCount":%d,"durationSec":%d}`+"\n",
		index, name, offsetMs, tenantA, gpu, dur)
}

func TestReclaimScenarioLateVsEarly(t *testing.T) {
	late := ReclaimScenario(true, 600)
	early := ReclaimScenario(false, 600)

	if len(late) != 3 || len(early) != 3 {
		t.Fatalf("reclaim scenario should have 3 rows")
	}
	// The owner (b1) returns near the end of the borrowed job under lateReturn, and early otherwise.
	if late[2].OffsetMs <= early[2].OffsetMs {
		t.Fatalf("late owner offset %d should exceed early %d", late[2].OffsetMs, early[2].OffsetMs)
	}
	if late[2].OffsetMs != 600*1000-10_000 {
		t.Fatalf("late owner offset = %d, want %d", late[2].OffsetMs, 600*1000-10_000)
	}
	if late[1].Name != jobBorrow || late[1].Tenant != tenantA {
		t.Fatalf("row 1 should be tenant-a's borrowing job")
	}
	if late[2].Tenant != "tenant-b" {
		t.Fatalf("row 2 should be the tenant-b owner")
	}
}

func TestReclaimScenarioIsDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := WriteTrace(&a, ReclaimScenario(true, 600)); err != nil {
		t.Fatal(err)
	}
	if err := WriteTrace(&b, ReclaimScenario(true, 600)); err != nil {
		t.Fatal(err)
	}
	if exputil.Checksum(a.Bytes()) != exputil.Checksum(b.Bytes()) {
		t.Fatalf("same scenario must produce the same immutable trace checksum")
	}
}

func TestFIFOHeadOfLine(t *testing.T) {
	rows := FIFOHeadOfLineScenario(600, 120)
	if len(rows) != 5 {
		t.Fatalf("FIFO scenario should have 5 rows, got %d", len(rows))
	}
	if rows[1].Name != jobHead || rows[1].GPUCount != 2 {
		t.Fatalf("row 1 should be the 2-GPU head job")
	}
	// The head job must arrive before the small jobs, so it truly sits at the head of the line.
	for i := 2; i < len(rows); i++ {
		if rows[i].GPUCount != 1 {
			t.Fatalf("small job %d should request 1 GPU", i)
		}
		if rows[i].OffsetMs <= rows[1].OffsetMs {
			t.Fatalf("small job %d must arrive after the head job", i)
		}
	}
}

func TestGeneratedScenariosPassValidation(t *testing.T) {
	// The handcrafted generators must satisfy the same rules a hand-edited trace is held to, so the study's
	// own inputs cannot silently drift out of the mechanism they are meant to exercise.
	if err := ValidateTrace(StudyReclaim, ReclaimScenario(true, 600)); err != nil {
		t.Fatalf("reclaim scenario should validate: %v", err)
	}
	if err := ValidateTrace(StudyFIFO, FIFOHeadOfLineScenario(600, 120)); err != nil {
		t.Fatalf("fifo scenario should validate: %v", err)
	}
}

func TestValidateTraceStudyRules(t *testing.T) {
	// A reclaim trace is a two-tenant borrowing story; a FIFO trace is single-tenant. Crossing the studies
	// would exercise a different mechanism than the one under test.
	reclaim := ReclaimScenario(true, 600)
	if err := ValidateTrace(StudyFIFO, reclaim); err == nil {
		t.Fatalf("a two-tenant trace must fail the single-tenant FIFO study")
	}
	// A reclaim row asking for 2 GPUs exceeds the per-tenant nominal quota of 1 and would test capacity, not
	// reclaim.
	twoGPU := ReclaimScenario(true, 600)
	twoGPU[1].GPUCount = 2
	if err := ValidateTrace(StudyReclaim, twoGPU); err == nil {
		t.Fatalf("a 2-GPU reclaim row must be rejected")
	}
}

func TestReadTraceRejectsMalformedRows(t *testing.T) {
	cases := map[string]string{
		"duplicate name":  jsonRow(0, "x", 0, 1, 60) + jsonRow(1, "x", 10, 1, 60),
		"non-contiguous":  jsonRow(0, "x", 0, 1, 60) + jsonRow(2, "y", 10, 1, 60),
		"zero gpu":        jsonRow(0, "x", 0, 0, 60),
		"negative offset": jsonRow(0, "x", -5, 1, 60),
		"zero duration":   jsonRow(0, "x", 0, 1, 0),
		"bad name":        jsonRow(0, "X_bad", 0, 1, 60),
	}
	for name, body := range cases {
		if _, err := ReadTrace(bytes.NewReader([]byte(body))); err == nil {
			t.Fatalf("%s: malformed trace must be rejected", name)
		}
	}
}

func TestReadTraceRejectsReorderedOffsets(t *testing.T) {
	good := ReclaimScenario(false, 600)
	var buf bytes.Buffer
	if err := WriteTrace(&buf, good); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrace(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("valid trace should read: %v", err)
	}

	bad := []byte(jsonRow(0, "x", 100, 1, 60) + jsonRow(1, "y", 50, 1, 60))
	if _, err := ReadTrace(bytes.NewReader(bad)); err == nil {
		t.Fatalf("a trace with decreasing offsets must be rejected")
	}
}

func TestTerminationContractTraceKeepsOwnRowAlive(t *testing.T) {
	rows, err := TerminationContractTrace(60, 40, DoseSelfCompleting)
	if err != nil {
		t.Fatalf("(60, 40) is the intended combination and must be accepted: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	byName := map[string]TrainingTraceRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	own, victim := byName[OwnRow], byName[VictimRow]

	// a1 must still be running when the owner is restored, or its natural release — not the victim's — can
	// be what freed a device, which is exactly the confound that made the recorded run uninterpretable.
	// The owner is restored at the earliest at the dose, and at the latest after the victim's full service
	// plus the 30 s grace period, so a1 must outlive that with margin.
	minimum := victim.DurationSec + 30
	if own.DurationSec <= minimum {
		t.Fatalf("a1 duration %d s must exceed victim service + grace (%d s) so it never releases first",
			own.DurationSec, minimum)
	}
	if victim.DurationSec != 60 {
		t.Fatalf("victim service = %d, want 60", victim.DurationSec)
	}
	if err := ValidateTrace(StudyReclaim, rows); err != nil {
		t.Fatalf("trace must satisfy the reclaim study rules: %v", err)
	}
}

func TestTerminationContractTraceRejectsADoseThatLeavesNothingToReclaim(t *testing.T) {
	// A dose at or past the victim's full service means the owner returns after the victim would already be
	// done, so the run would exercise ordinary sequencing rather than a mid-service reclamation.
	if _, err := TerminationContractTrace(60, 600, DoseSelfCompleting); err == nil {
		t.Fatal("a dose that exceeds the victim's service must be rejected")
	}
}

func TestTerminationContractTraceRejectsADoseOutsideTheGracePeriod(t *testing.T) {
	// A dose that leaves the victim's remaining service at or beyond the 30 s grace period does not pin the
	// return inside the grace-period restoration window the experiment is about.
	if _, err := TerminationContractTrace(60, 0, DoseSelfCompleting); err == nil {
		t.Fatal("a dose that leaves remaining service outside the grace period must be rejected")
	}
}

// The two dose regimes measure different quantities, so a dose that does not produce the regime the caller
// declared must be refused rather than quietly measuring the other one.
//
// The guard this replaces permitted only the self-completing side while its own comment described the
// grace-bounded side as the thing being excluded — so the experiment named "termination contract" could
// never reach the regime where a termination contract actually binds. An ignoring victim with less service
// left than the grace period finishes on its own; the platform's configured patience never applies, and
// could be any value without changing the result.
//
// Mutation that turns this red: swap the two comparisons in the regime switch.
func TestTerminationContractTraceRefusesADoseThatIsNotTheDeclaredRegime(t *testing.T) {
	const grace = terminationGraceSec // 30
	for _, tc := range []struct {
		name       string
		svc, dose  int
		regime     DoseRegime
		wantAccept bool
	}{
		{"self-completing with remaining inside the grace", 60, 40, DoseSelfCompleting, true},
		{"self-completing with remaining at the grace", 60, 30, DoseSelfCompleting, false},
		{"self-completing with remaining beyond the grace", 60, 20, DoseSelfCompleting, false},
		{"grace-bounded with remaining beyond the grace", 60, 20, DoseGraceBounded, true},
		{"grace-bounded with remaining exactly the grace", 60, 30, DoseGraceBounded, true},
		{"grace-bounded with remaining inside the grace", 60, 40, DoseGraceBounded, false},
		{"nothing left to reclaim, under either regime", 60, 60, DoseSelfCompleting, false},
		{"nothing left to reclaim, grace-bounded too", 60, 60, DoseGraceBounded, false},
		{"a regime the experiment never defined", 60, 40, DoseRegime("whatever"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := TerminationContractTrace(tc.svc, tc.dose, tc.regime)
			if tc.wantAccept && err != nil {
				t.Fatalf("%d s dose on a %d s victim (remaining %d, grace %d) was refused for %s: %v",
					tc.dose, tc.svc, tc.svc-tc.dose, grace, tc.regime, err)
			}
			if !tc.wantAccept {
				if err == nil {
					t.Fatalf("%d s dose on a %d s victim (remaining %d, grace %d) was accepted as %s; that dose "+
						"produces the other regime, so the run would measure a quantity it did not declare",
						tc.dose, tc.svc, tc.svc-tc.dose, grace, tc.regime)
				}
				return
			}
			if len(rows) != 3 {
				t.Fatalf("accepted trace has %d rows, want 3", len(rows))
			}
		})
	}
}
