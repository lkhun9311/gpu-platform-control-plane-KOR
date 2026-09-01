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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/queuelab"
)

// The assertion checks every field, not a sample, so a struct-copy bug or a stray json:"-" on any one
// field cannot slip through a partial check the way an earlier revision of this test allowed.
func TestRunRecordRoundTrips(t *testing.T) {
	in := runRecord{
		SchemaVersion: recordSchemaVersion,
		RunID:         "r7",
		Arm:           "A-honor",
		StartedAt:     "2026-08-08T10:00:00Z",
		EndedAt:       "2026-08-08T10:02:30Z",
		Disposition:   string(dispChecksPassed),
		Reason:        "",
		Events:        []queuelab.LifecycleEvent{{ElapsedNs: 1, Kind: "Pod", Job: "a1"}},
		// Written out rather than derived, because this test is about the WIRE and a derived block would only
		// prove buildRecord agrees with itself. The failure list is populated for the same reason the events are:
		// an omitempty slice that is never non-empty here would leave the field's encoding unexercised.
		Validity: validity{
			Verdict:            verdictRefused,
			Failures:           []string{failureObservation, failureExclusivity},
			UnimplementedGates: recordUnchecked(workloadProvenance{}),
			DeviceEvidence:     deviceNotObserved,
		},
	}
	b, err := encodeRecord(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip lost content: got %+v, want %+v", got, in)
	}
}

// A reader must refuse a schema it does not understand rather than interpreting it under today's rules.
func TestDecodeRunRecordRefusesAnUnknownSchema(t *testing.T) {
	b := []byte(`{"schemaVersion":99,"runID":"r7","arm":"A-honor","disposition":"completed-implemented-checks-passed"}`)
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("an unknown schema version must be refused")
	}
}

// refusedValidity is the smallest validity block a hand-written document can carry and still get past
// decodeRunRecord's verdict guard.
//
// Every document below that must DECODE needs one, and that is not boilerplate to skim past: a verdict is
// required of every record, so a fixture without one is refused for a reason its test did not write. The
// guard ORDER is what keeps the refusal fixtures honest in the other direction — the qualification, window
// and observation guards all run before the verdict is looked at, so each of those documents is still
// refused by the specific check its test is about rather than by a missing verdict.
// The device-evidence axis is in here for the same reason the verdict is: it is required of every record,
// and a fixture without one is refused for a reason its test did not write. device-not-observed is what a
// record with no measurement derives, so these fixtures state the value their own fields support.
const refusedValidity = `"validity":{"verdict":"refused","failures":["observation-not-continuous"],` +
	`"deviceEvidence":"device-not-observed"}`

// A field the schema does not define must be refused, not silently dropped, or a hand-edited or
// future-schema document could carry content decodeRunRecord never validated.
//
// The version is interpolated rather than written out, here and in the two documents below, because each of
// these tests asserts only that SOMETHING was refused: pinned to a literal, they would keep passing after a
// schema bump while the refusal they actually observed was the version check, and the property each was
// written for would quietly stop being covered.
func TestDecodeRunRecordRefusesAnUnknownField(t *testing.T) {
	b := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","bogusField":"x"}`, recordSchemaVersion)
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("an unrecognized field must be refused")
	}
}

// Bytes after the record must be refused, because a decoder stops at the end of the first value and would
// otherwise hand a reader a document whose tail went to nobody.
//
// The accepted-then-refused pair is the whole design of this test rather than ceremony around the assertion
// that matters. A fixture that only appended garbage and asserted a refusal would pass identically if the
// document in front of the garbage were itself unreadable — the refusal would come from the record, not from
// the tail — and this package has already shipped one assertion that proved nothing for exactly that reason.
// Decoding the same bytes first is what makes the second call's failure attributable to the appended
// document. Mutation: delete the dec.More() block in decodeRunRecord and the second call returns nil.
func TestDecodeRunRecordRefusesTrailingDataAfterAValidRecord(t *testing.T) {
	b := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",%s}`, recordSchemaVersion, refusedValidity)
	if _, err := decodeRunRecord(b); err != nil {
		t.Fatalf("the document before the trailing data must decode on its own, or this test attributes its "+
			"refusal to the wrong bytes: %v", err)
	}
	if _, err := decodeRunRecord(append(b, "\n{\"trailingGarbage\":true}\n"...)); err == nil {
		t.Fatal("a second document appended after the record must be refused")
	}
}

// A record without a run identity is not a usable record regardless of what else it claims.
func TestDecodeRunRecordRefusesEmptyRunID(t *testing.T) {
	b := fmt.Appendf(nil,
		`{"schemaVersion":%d,"runID":"","arm":"A-honor","disposition":"completed-implemented-checks-passed"}`,
		recordSchemaVersion)
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("an empty runID must be refused")
	}
}

// Preview safety is structural, not conventional: queuelab.Reconstruct takes an event slice, so anything
// decodable back into one is reconstructable no matter what the field is called or what flag sits beside
// it. The preview record therefore has no field that can carry events at all.
func TestPreviewRecordCannotCarryEvents(t *testing.T) {
	p := previewRecord{
		SchemaVersion: recordSchemaVersion,
		Preview:       true,
		RunID:         "r7",
		Arm:           "A-honor",
		Disposition:   string(dispChecksPassed),
		EventCount:    42,
		Note:          "preview: gates were not enforced, this is not evidence",
	}
	b, err := encodeRecord(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Nothing in the encoded form may decode into lifecycle events.
	var probe struct {
		Events []queuelab.LifecycleEvent `json:"events"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(probe.Events) != 0 {
		t.Fatal("a preview record must not carry anything decodable into lifecycle events")
	}
	if strings.Contains(string(b), "elapsedNs") {
		t.Fatalf("a preview record must not contain event fields: %s", b)
	}
}

// A record claiming to be a preview while carrying events is malformed and must be refused, so a
// hand-edited or future file cannot smuggle evidence through the preview branch.
func TestDecodeRunRecordRefusesPreviewWithEvents(t *testing.T) {
	b := fmt.Appendf(nil, `{"schemaVersion":%d,"preview":true,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","events":[{"elapsedNs":1,"kind":"Pod"}]}`,
		recordSchemaVersion)
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("a preview record carrying events must be refused")
	}
}

// This test verifies success-content-correctness — that a successful write leaves exactly the destination
// file behind, decodable back to the value passed in — and leftover-temp-file cleanliness. It does not
// verify the atomic replace-not-modify mechanism itself; see TestWriteRecordReplacesTheDestinationInode
// for that, because passing here is consistent with a non-atomic in-place write.
//
// A successful write must leave exactly the destination file behind, with the content actually written
// decodable back to the value passed in — not merely some decodable-but-unrelated bytes, and not a
// leftover temp file beside it.
func TestWriteRecordLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/run.json"

	want := runRecord{
		SchemaVersion: recordSchemaVersion,
		RunID:         "r7",
		Arm:           "A-honor",
		Disposition:   string(dispChecksPassed),
		// A verdict is required of every record, so this fixture carries the one its own fields support: no
		// observation, no window and no qualification means it establishes nothing, whatever its disposition
		// says.
		Validity: validity{Verdict: verdictRefused, Failures: []string{failureRunIncomplete},
			DeviceEvidence: deviceNotObserved},
	}
	if err := writeRecord(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("the written record must decode: %v", err)
	}
	// Decoding without error is not enough: a writer that ignored v and emitted some other fixed,
	// decodable record would still pass a bare decode check, so the decoded value must match what was
	// actually asked to be written.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("written record does not match what was passed in: got %+v, want %+v", got, want)
	}

	// No temporary file may survive a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the record, got %d entries", len(entries))
	}
	// The one surviving entry must be the destination itself, not some other artifact of the rename.
	if entries[0].Name() != "run.json" {
		t.Fatalf("expected the surviving entry to be run.json, got %q", entries[0].Name())
	}
}

// This test verifies failure-signalling — that a write which cannot proceed returns a non-nil error and
// leaves no stray file behind — not the atomic replace-not-modify mechanism of a successful write; see
// TestWriteRecordReplacesTheDestinationInode for that.
//
// A write into a directory that does not exist must fail loudly rather than leaving the caller believing
// the run was recorded, and it must not scribble a stray file into the parent directory as a side effect
// of the failed attempt.
func TestWriteRecordFailsLoudlyOnAnUnwritablePath(t *testing.T) {
	parent := t.TempDir()
	if err := writeRecord(parent+"/missing/run.json", runRecord{
		SchemaVersion: recordSchemaVersion, RunID: "r7", Arm: "A-honor",
		Disposition: string(dispChecksPassed),
	}); err == nil {
		t.Fatal("writing into a missing directory must fail")
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed write must leave no trace in the parent directory, got %d entries", len(entries))
	}
}

// TestWriteRecordReplacesTheDestinationInode pins the atomic replace-not-modify mechanism itself, which
// the two tests above do not: they would pass just as well against a naive os.WriteFile(path, b, 0o644)
// that opens the destination with O_TRUNC and writes into it in place.
//
// The distinguishing property is not timing, so it needs no race and cannot be flaky: os.WriteFile reuses
// the destination's inode, while temp-file-plus-rename replaces the directory entry with a new inode. A
// changed inode number between two successive writes is direct, deterministic evidence that a rename
// happened rather than an in-place modification.
func TestWriteRecordReplacesTheDestinationInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode numbers are not a meaningful concept on this platform")
	}
	dir := t.TempDir()
	path := dir + "/run.json"

	if err := writeRecord(path, runRecord{
		SchemaVersion: recordSchemaVersion, RunID: "r1", Arm: "A-honor", Disposition: string(dispChecksPassed),
	}); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 1: %v", err)
	}
	st1, ok := fi1.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("syscall.Stat_t is not available on this platform")
	}
	ino1 := st1.Ino

	if err := writeRecord(path, runRecord{
		SchemaVersion: recordSchemaVersion, RunID: "r2", Arm: "A-honor", Disposition: string(dispChecksPassed),
	}); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 2: %v", err)
	}
	st2 := fi2.Sys().(*syscall.Stat_t)
	ino2 := st2.Ino

	if ino1 == ino2 {
		t.Fatalf("destination inode did not change across two writes (both %d): the file was modified in "+
			"place rather than replaced by a rename, so a reader racing a write could observe a partial file",
			ino1)
	}
}

// A record this build writes has to be a record this build can read, and until verifyRecordReadable existed
// nothing in the binary ever asked. The fixture carries a REFUSED DELETE for the reason
// TestRunRecordCarriesTheResidueAndStillDecodes gives — an `error` encodes and does not decode, so persisting
// teardown's `residue` verbatim would produce an unreadable record on exactly the runs whose teardown was
// refused — but that test decodes bytes it holds in memory, and this one reads the FILE, which is what the
// tool now does and what a later reader will actually open.
//
// Mutation that turns this red: change recordResidue.Error from `string` to `error` and assign
// `e.Error = r.Observation.Err` in residueForRecord — the original observation.Err defect, which was caught by
// a person reasoning during a design round rather than by the tool noticing.
func TestVerifyRecordReadableAcceptsARecordThisBuildJustWrote(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "queuelab-r7",
		errors.New("teardown may not delete namespaces"))
	left := []residue{{
		Observation: observation{
			Target:  target{Phase: phaseNamespace, Kind: "Namespace", Name: "queuelab-r7"},
			Found:   true,
			UID:     "ns-uid",
			WantUID: "ns-uid",
			Err:     refused,
		},
		Absence: absenceUnknown,
	}}
	rec := buildRecord(outcome{Disposition: dispResidueLeft, Reason: "teardown left 1 object(s)"}, nil, left,
		nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())

	path := t.TempDir() + "/run.json"
	if err := writeRecord(path, rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifyRecordReadable(path, false); err != nil {
		t.Fatalf("the record this build just wrote must be one it can read: %v", err)
	}
}

// The other half: a document the reader refuses must be reported, not shrugged off. A schema one ahead of this
// build's is the cheapest way to produce one, and it is also the realistic drift — a future writer's record
// landing under a reader that has not been taught it.
//
// Mutation that turns this red: drop the `if _, err := decodeRunRecord(b); err != nil` return from
// verifyRecordReadable's non-preview arm, so the function reports only whether the file could be OPENED.
func TestVerifyRecordReadableRefusesADocumentThisBuildCannotDecode(t *testing.T) {
	path := t.TempDir() + "/run.json"
	if err := writeRecord(path, runRecord{
		SchemaVersion: recordSchemaVersion + 1, RunID: "r7", Arm: "A-honor",
		Disposition: string(dispChecksPassed),
		Validity:    validity{Verdict: verdictRefused, Failures: []string{failureRunIncomplete}},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := verifyRecordReadable(path, false)
	if err == nil {
		t.Fatal("a record this build cannot decode must be reported, or the one artifact the run delivers " +
			"passes silently in exactly the state that makes it useless")
	}
	if !strings.Contains(err.Error(), "read back record") {
		t.Fatalf("the failure must name itself as a read-back rather than read as a write failure, got %q", err)
	}
}

// The preview arm inverts the question rather than skipping it, so that no record this build writes goes
// unexamined. previewRecord's promise is that a preview cannot be read as a run — it carries eventCount and
// note, which runRecord has never heard of — and a preview document that DID decode as a run record would be
// one whose fields had drifted into a run's, which is the single failure the type exists to prevent.
//
// The second half writes a runRecord and verifies it AS a preview, which is that drift already complete.
//
// Mutation that turns this red: delete the `if preview` block from verifyRecordReadable, so a preview is
// checked against the run-record reader (or not at all) and the drift reports success.
func TestVerifyRecordReadableRefusesAPreviewWhoseFieldsDriftedIntoARuns(t *testing.T) {
	dir := t.TempDir()

	good := dir + "/preview.json"
	if err := writeRecord(good, buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, true, time.Now(), time.Now())); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifyRecordReadable(good, true); err != nil {
		t.Fatalf("an ordinary preview record must pass its own check: %v", err)
	}

	drifted := dir + "/drifted.json"
	if err := writeRecord(drifted, buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifyRecordReadable(drifted, true); err == nil {
		t.Fatal("a preview document that decodes as a run record has lost the structural guarantee that a " +
			"preview cannot be read as evidence, and must not pass")
	}
}

// absenceName's spellings are a wire format: runRecord.Residue and the Node residue record both persist
// them, so a rename here silently relabels every record already written under the old name. The two other
// residue tests in this file each pin one spelling as a side effect of asserting a full record, but neither
// checks it against a literal — they check it against what buildRecord happened to produce, which is not a
// test of absenceName at all. This one is direct and literal, so a rename of any spelling, or of the
// default's "unrecognised" prefix, has nothing else to hide behind.
func TestAbsenceNameSpellings(t *testing.T) {
	cases := []struct {
		a    absence
		want string
	}{
		{absencePresent, "present"},
		{absenceAbsent, "absent"},
		{absenceForeign, "foreign"},
		{absenceUnknown, "unknown"},
		// A constant added to teardown.go without a matching case here must not fall back to "unknown" — the
		// spelling that means "nobody could tell" — because that would hide the missing case inside a value
		// the schema calls legitimate. It must name the integer instead, so the bug is visible in the record.
		{absence(99), "unrecognised(99)"},
	}
	for _, tc := range cases {
		if got := absenceName(tc.a); got != tc.want {
			t.Errorf("absenceName(%d) = %q, want %q", int(tc.a), got, tc.want)
		}
	}
}

// The residue is the one thing a teardown that did not finish leaves for anybody to act on, so it has to
// survive the round trip the record's whole contract is built on: written, and then read back by a reader
// that refuses anything it does not fully understand.
//
// The entry here carries a REFUSED DELETE on purpose. That is the case settlePhase holds a delete error
// aside for — "the apiserver said no" is the single most useful thing a residue record can say, and it is
// also the case that breaks if the record persists teardown.go's `residue` verbatim: an `error` field
// encodes as `{}` and decodes not at all, so exactly the runs whose teardown was refused would write a
// record decodeRunRecord rejects. Hence the projection this asserts against, and hence the error text
// assertion rather than a bare "the entry is there".
func TestRunRecordCarriesTheResidueAndStillDecodes(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "queuelab-r7",
		errors.New("teardown may not delete namespaces"))
	left := []residue{{
		Observation: observation{
			Target:  target{Phase: phaseNamespace, Kind: "Namespace", Name: "queuelab-r7"},
			Found:   true,
			UID:     "ns-uid",
			WantUID: "ns-uid",
			Err:     refused,
		},
		Absence: absenceUnknown,
	}}

	rec := buildRecord(outcome{Disposition: dispResidueLeft, Reason: "teardown left 1 object(s)"}, nil, left, nil, nil,
		nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a record carrying residue must decode, or the residue is written and then unreadable "+
			"exactly when it matters most: %v\n%s", err, b)
	}
	if len(got.Residue) != 1 {
		t.Fatalf("the record carries %d residue entries, want 1: residue that only reached stderr is "+
			"printed and lost, which is the failure this field exists to end\n%s", len(got.Residue), b)
	}
	e := got.Residue[0]
	if e.Kind != "Namespace" || e.Name != "queuelab-r7" {
		t.Fatalf("the entry must name the object still there, got %s %q", e.Kind, e.Name)
	}
	if e.Absence != "unknown" {
		t.Fatalf("the verdict must be persisted by name, not as the iota an inserted constant would "+
			"silently relabel, got %q", e.Absence)
	}
	if !strings.Contains(e.Error, "forbidden") {
		t.Fatalf("the refusal must survive into the record, or a residue entry reads as a slow finalizer "+
			"when it was actually a permission the run never had, got %q", e.Error)
	}
	if e.UID != "ns-uid" || !e.Found {
		t.Fatalf("the entry must carry what was observed, got %+v", e)
	}
}

// residueForRecord copies eight fields, and two of them — Terminating and WantUID — had no assertion
// anywhere in this file before this test: dropping either from the projection left the whole package
// green. Terminating is the operator's first question reading a residue entry, because it is what tells a
// finalizer stuck on an object apart from a Delete that never even landed; WantUID is what the next
// operator compares against a fresh read to tell "still ours" from "somebody else's object took the name
// after ours left". UID and WantUID are given different values so a projection that swapped the two, not
// just dropped one, would also be caught.
func TestResidueForRecordProjectsTerminatingAndWantUID(t *testing.T) {
	left := []residue{{
		Observation: observation{
			Target:      target{Kind: "Namespace", Name: "queuelab-r7"},
			Found:       true,
			Terminating: true,
			UID:         "have-uid",
			WantUID:     "want-uid",
		},
		Absence: absencePresent,
	}}
	out := residueForRecord(left)
	if len(out) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(out), out)
	}
	e := out[0]
	if !e.Terminating {
		t.Fatal("Terminating dropped by the projection: a namespace stuck on a finalizer now reads " +
			"identically to one whose Delete never landed, which is the distinction this field exists for")
	}
	if e.WantUID != "want-uid" {
		t.Fatalf("WantUID = %q, want %q: dropped, or overwritten by the UID field, in the projection",
			e.WantUID, "want-uid")
	}
	if e.UID != "have-uid" {
		t.Fatalf("UID = %q, want %q: corrupted by whatever is wrong with the WantUID projection above",
			e.UID, "have-uid")
	}
}

// A preview is the mode that generates residue TODAY — it runs the whole of run(), namespace and fixtures
// included — so dropping the residue from its record would lose it for the only mode currently producing
// it. Residue is safe to carry there for the reason previewRecord's own comment gives: the guarantee is
// structural, and recordResidue has no field a lifecycle ledger can be decoded out of.
func TestPreviewRecordCarriesResidueToo(t *testing.T) {
	left := []residue{{
		Observation: observation{
			Target: target{Phase: phaseResourceFlavor, Kind: "ResourceFlavor", Name: "queuelab-gpu-r7"},
			Found:  true, UID: "rf-uid", WantUID: "rf-uid",
		},
		Absence: absencePresent,
	}}
	pr, ok := buildRecord(outcome{Disposition: dispResidueLeft}, nil, left, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, true,
		time.Now(), time.Now()).(previewRecord)
	if !ok {
		t.Fatal("a preview invocation must build a previewRecord")
	}
	if len(pr.Residue) != 1 || pr.Residue[0].Name != "queuelab-gpu-r7" {
		t.Fatalf("a preview's record must carry what its own teardown could not remove, got %+v", pr.Residue)
	}
	if pr.Residue[0].Absence != "present" {
		t.Fatalf("verdict = %q, want present", pr.Residue[0].Absence)
	}
}

// What the run observed about its worker has to survive into the artifact, through a decoder that rejects
// anything it does not fully understand. A qualification that only reached stderr is the same loss the
// residue field was added to end: the run that refused is exactly the run whose evidence nobody kept.
//
// The refusal shape is asserted rather than the clean one, because the clean one is the case where dropping
// the field costs least — an empty consumer list read back as an empty consumer list looks identical whether
// it was persisted or not.
//
// Mutation that turns this red: delete the Qualification field from runRecord, or stop assigning it in
// buildRecord. Either leaves the whole package green apart from this test and its preview twin below.
func TestRunRecordCarriesTheQualificationAndStillDecodes(t *testing.T) {
	q := &qualification{
		Node:           "platform-worker",
		NodeUID:        "uid-node",
		AllocatableGPU: 2,
		RequiredGPU:    2,
		RequiredFrom: "nominal nvidia.com/gpu quota summed over 2 ClusterQueue(s) on flavor queuelab-gpu-r7 " +
			"= 2; largest single trace row \"head2\" = 2",
		// Carried explicitly because it is what a reader classifies on: bound by the quota sum means the node
		// could not hold the whole arm, bound by a single row means one Pod could never have been scheduled.
		RequiredBoundBy: boundByQuotaSum,
		Ready:           true,
		Schedulable:     true,
		PodsOnNode:      4,
		GPUConsumers: []gpuConsumer{
			{Namespace: "tenant-a", Name: "train-7", Phase: "Running", Terminating: true, GPUs: 2},
		},
	}
	rec := buildRecord(outcome{Disposition: dispEnvironmentUnqualified, Reason: "a GPU Pod was already there"},
		nil, nil, q, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a record carrying a qualification must decode, or the evidence is written and then "+
			"unreadable exactly on the runs that produced it: %v\n%s", err, b)
	}
	if got.Qualification == nil {
		t.Fatalf("the qualification did not survive the round trip:\n%s", b)
	}
	if !reflect.DeepEqual(*got.Qualification, *q) {
		t.Fatalf("the qualification changed on the round trip:\n got %+v\nwant %+v\n%s", *got.Qualification, *q, b)
	}
	// The denominator is the half a projection is most likely to drop, because it is the field that says
	// nothing on its own: "no foreign consumer among four Pods inspected" is a claim, and "no foreign
	// consumer" with no count is indistinguishable from having looked in the wrong place.
	if got.Qualification.PodsOnNode != 4 {
		t.Fatalf("PodsOnNode = %d, want 4", got.Qualification.PodsOnNode)
	}
	t.Logf("persisted record:\n%s", b)
}

// A run refused before it ever reached its worker — a bad flag, a refused acquisition — has observed nothing,
// and a record for it must say nothing rather than write a zero qualification claiming a node named "" was
// inspected and found fine. That is why the field is a pointer.
//
// Mutation that turns this red: make runRecord.Qualification a value rather than a pointer. The key then
// appears on every record ever written, populated with zeros, and every reader has to know that Ready:false
// on a node with no name means "not checked".
func TestARecordWithNoQualificationCarriesNoQualificationKey(t *testing.T) {
	rec := buildRecord(outcome{Disposition: dispAcquisitionRefused, Reason: "held by another run"},
		nil, nil, nil, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// The bare WORD, not merely the JSON key, and it is back to the bare word after a round trip through the
	// weaker check. When the validity block carried the executable's roadmap of work still to do, one of its
	// entries read "environment qualification (capacity, foreign GPU pods, termination canary)" and this check
	// fired on a record that correctly wrote no qualification — so it was narrowed to the key, losing the reach
	// that catches the word appearing anywhere at all. recordUnchecked names the canary without the word, so
	// the strong check stands again; a future entry that reintroduces it will fail here and should be reworded
	// rather than the check weakened a second time.
	if strings.Contains(string(b), "qualification") {
		t.Fatalf("a run that never qualified its worker wrote a qualification anyway:\n%s", b)
	}
}

// A preview runs the whole of run(), so it qualifies its worker exactly as a real run does. Withholding the
// qualification from its record would lose the observation for the only mode that currently produces one on a
// live cluster, and it is safe here for the same structural reason the residue is: it describes the machine,
// and has no field a lifecycle ledger can be decoded out of.
//
// Mutation that turns this red: drop Qualification from previewRecord or stop assigning it in buildRecord's
// preview branch.
func TestPreviewRecordCarriesTheQualificationToo(t *testing.T) {
	q := &qualification{Node: "platform-worker", NodeUID: "uid-node", AllocatableGPU: 2, RequiredGPU: 2,
		Ready: true, Schedulable: true, PodsOnNode: 3}
	pr, ok := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, q, nil, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, true,
		time.Now(), time.Now()).(previewRecord)
	if !ok {
		t.Fatal("a preview invocation must build a previewRecord")
	}
	if pr.Qualification == nil || pr.Qualification.Node != "platform-worker" {
		t.Fatalf("a preview's record must say which machine it smoke-tested, got %+v", pr.Qualification)
	}
	if pr.Qualification.PodsOnNode != 3 {
		t.Fatalf("PodsOnNode = %d, want 3", pr.Qualification.PodsOnNode)
	}
}

// The record shape this gate's first commit wrote, and it is not hypothetical: a live kind run against a
// real cluster left one at /tmp/claude-1000/e2e/rec-gate2.json, carrying schemaVersion 1, a requiredFrom
// naming only the quota sum, and no requiredBoundBy at all.
//
// That document has to be REFUSED rather than read, and the asymmetry is what makes the version the right
// instrument. A new record read by an old build already fails loudly on DisallowUnknownFields; an old record
// read by this build decodes without complaint and leaves RequiredBoundBy at "" — neither documented
// constant, and indistinguishable to a reader from a third kind of bound nobody named. A run artifact whose
// binding constraint reads as a blank is exactly the "silently applying current semantics to a document
// written under different ones" that recordSchemaVersion's own comment exists to prevent.
//
// The message must name both versions, because the operator holding this file needs to know which build
// wrote it, not merely that this one will not read it.
//
// Two mutations turn this red, and they are red for different halves of it, which is worth stating exactly
// because the two guards overlap on THIS document and a careless reading would credit one for the other's
// work. Reverting recordSchemaVersion to 1 alone still refuses the document — the bound guard below catches
// the blank — but the refusal then names no version at all, and the second assertion fails: the operator is
// told the bound is unreadable and not which build wrote the file. Reverting the version AND deleting the
// bound guard is the pre-fix build, and the document decodes clean; the first assertion then fires with the
// blank bound in hand. The version is what distinguishes the two SHAPES; the bound guard is what refuses a
// bad value inside the current shape. Neither subsumes the other.
func TestDecodeRunRecordRefusesTheShapeThatPredatesTheBoundDerivation(t *testing.T) {
	preFix := []byte(`{"schemaVersion":1,"runID":"g2a","arm":"A-honor",` +
		`"disposition":"completed-implemented-checks-passed","qualification":{` +
		`"node":"platform-worker","nodeUID":"uid-node","allocatableGPU":2,"requiredGPU":2,` +
		`"requiredFrom":"nominal nvidia.com/gpu quota summed over 2 ClusterQueue(s) on flavor queuelab-gpu-g2a",` +
		`"ready":true,"schedulable":true,"podsOnNode":6}}`)

	got, err := decodeRunRecord(preFix)
	if err == nil {
		t.Fatalf("a record written before the requirement had two bounds was read under today's rules; its "+
			"qualification came back with RequiredBoundBy = %q, which is neither %q nor %q and which a reader "+
			"classifying on that field would take as a third kind of bound nobody defined",
			got.Qualification.RequiredBoundBy, boundByQuotaSum, boundByLargestRow)
	}
	for _, want := range []string{"1", fmt.Sprint(recordSchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal %q does not name version %s; an operator holding the file has to be told "+
				"which build wrote it and which one is reading", err, want)
		}
	}
}

// The version bump closes one route into the blank bound: a document an older build wrote. This closes the
// other: a document that claims THIS version while naming a bound no build ever produced — hand-edited, or
// written by a future version that forgot the field. Without it the type still permits the state the bump
// was made to eliminate, and the bump would be a fix for one entrance to a room with two.
//
// Mutation that turns this red: delete the RequiredBoundBy check from decodeRunRecord.
func TestDecodeRunRecordRefusesAQualificationNamingNoDocumentedBound(t *testing.T) {
	for _, bound := range []string{"", "whatever-the-writer-felt-like"} {
		b := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
			`"disposition":"completed-implemented-checks-passed","qualification":{`+
			`"node":"platform-worker","nodeUID":"uid-node","allocatableGPU":2,"requiredGPU":2,`+
			`"requiredFrom":"x","requiredBoundBy":%q,"ready":true,"schedulable":true,"podsOnNode":0}}`,
			recordSchemaVersion, bound)
		if _, err := decodeRunRecord(b); err == nil {
			t.Fatalf("a qualification bound by %q was accepted; the field's whole value is that a reader can "+
				"classify on it without parsing prose", bound)
		}
	}

	// The same document naming a real bound must still decode, or the guard above is refusing the records it
	// was written to protect. It carries a verdict because every record must; see refusedValidity.
	ok := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",`+refusedValidity+`,"qualification":{`+
		`"node":"platform-worker","nodeUID":"uid-node","allocatableGPU":2,"requiredGPU":2,`+
		`"requiredFrom":"x","requiredBoundBy":%q,"ready":true,"schedulable":true,"podsOnNode":0}}`,
		recordSchemaVersion, boundByLargestRow)
	if _, err := decodeRunRecord(ok); err != nil {
		t.Fatalf("a well-formed record was refused by the bound guard: %v", err)
	}
}

// testWindow is a window that held: a view opened before the run's first Create, a handful of Node versions
// compared against the journal's tuple, nothing deviating, and an audited release on the way out.
func testWindow() *ownershipWindow {
	return &ownershipWindow{
		Node:                    "platform-worker",
		NodeUID:                 "uid-node",
		TxID:                    "tx-1111",
		BaselineResourceVersion: "1000",
		OpenedAt:                "2026-08-15T10:00:00Z",
		ClosedAt:                "2026-08-15T10:02:30Z",
		NodeVersionsObserved:    7,
		Ending:                  "closed by the run",
		Restoration: &restorationAudit{
			Before: nodeMarkers{Observed: true, NodeUID: "uid-node", HasLabel: true, LabelValue: "r7",
				OwnershipTaintValues: []string{"r7"}, OtherTaintKeys: []string{"platform.lkhun9311.github.io/unhealthy"},
				HasJournal: true},
			After: nodeMarkers{Observed: true, NodeUID: "uid-node",
				OtherTaintKeys: []string{"platform.lkhun9311.github.io/unhealthy"}},
			OurMarkersRemoved: true,
		},
	}
}

// The window is the gate's evidence, and evidence that is written and then unreadable is the failure the
// strict decoder exists to prevent — on precisely the runs that produced it.
//
// Mutation that turns this red: delete `Window: win` from buildRecord's runRecord branch. The run's own
// continuous evidence then exists only as a line on a terminal, which is the state this gate was written to
// end and the state the fourth gate would have to investigate from scratch.
func TestRunRecordCarriesTheWindowAndStillDecodes(t *testing.T) {
	w := testWindow()
	rec := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, nil, w, nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false,
		time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a record carrying an ownership window must decode: %v\n%s", err, b)
	}
	if got.Window == nil {
		t.Fatalf("the window did not survive the round trip:\n%s", b)
	}
	if !reflect.DeepEqual(*got.Window, *w) {
		t.Fatalf("the window changed on the round trip:\n got %+v\nwant %+v\n%s", *got.Window, *w, b)
	}
	// The restoration audit is the half most likely to be dropped by a projection, because it is the only
	// nested structure in the block and the only one written after the run has chosen its outcome.
	if got.Window.Restoration == nil || !got.Window.Restoration.OurMarkersRemoved {
		t.Fatalf("the restoration audit did not survive:\n%s", b)
	}
	t.Logf("persisted record:\n%s", b)
}

// A violated window has to survive the round trip too, and it is the more valuable of the two: a run
// invalidated for a stripped taint is exactly the run whose evidence would otherwise be a sentence on
// somebody's terminal.
//
// Mutation that turns this red: drop the Violations field from the encoded window (or make recordLocked stop
// filling it). The record then says a window existed and cannot say what it saw.
func TestARefusedRunRecordsWhatTheWindowSaw(t *testing.T) {
	w := testWindow()
	w.ViolationsObserved = 2
	w.Violations = []ownershipViolation{{
		At: "2026-08-15T10:01:00Z", Reason: reasonInstalledDiverged,
		Detail:         "node platform-worker no longer carries what tx tx-1111 installed",
		ObservedTaints: "(none)",
	}}
	rec := buildRecord(outcome{Disposition: dispCollectorDesync, Reason: "run invalidated"}, nil, nil, nil, w,
		nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a record carrying a violated window must decode: %v\n%s", err, b)
	}
	if got.Window.ViolationsObserved != 2 || len(got.Window.Violations) != 1 {
		t.Fatalf("the violation count and the retained detail must both survive: %+v", got.Window)
	}
	if got.Window.Violations[0].Reason != reasonInstalledDiverged {
		t.Fatalf("the violation reason changed to %q", got.Window.Violations[0].Reason)
	}
}

// A preview opens its window exactly as a run does, and a preview whose record did not say what happened to
// its worker would be a smoke check of a machine nobody watched. It is safe here for the same structural
// reason the qualification is: no field in the block is a lifecycle ledger.
//
// Mutation that turns this red: delete `Window: win` from buildRecord's previewRecord branch.
func TestPreviewRecordCarriesTheWindowToo(t *testing.T) {
	pr, ok := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, nil, testWindow(), nil, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, true, time.Now(), time.Now()).(previewRecord)
	if !ok {
		t.Fatal("a preview invocation must build a previewRecord")
	}
	if pr.Window == nil || pr.Window.NodeVersionsObserved != 7 {
		t.Fatalf("the preview record dropped the window: %+v", pr.Window)
	}
}

// A window that watched nothing cannot be read as evidence that the worker stayed this run's, and that is
// the single strongest claim this record can make. No build writes one — the opening read makes every window
// at least 1 — so a zero is a hand-edited or future-written document.
//
// Mutation that turns this red: delete the window guard from decodeRunRecord.
func TestDecodeRunRecordRefusesAWindowThatWatchedNothing(t *testing.T) {
	for name, block := range map[string]string{
		"no versions": `{"node":"platform-worker","nodeUID":"uid-node","txID":"tx-1111",` +
			`"baselineResourceVersion":"1000","openedAt":"t","nodeVersionsObserved":0,"ending":"closed by the run",` +
			`"violationsObserved":0}`,
		"no node": `{"node":"","nodeUID":"uid-node","txID":"tx-1111",` +
			`"baselineResourceVersion":"1000","openedAt":"t","nodeVersionsObserved":3,"ending":"closed by the run",` +
			`"violationsObserved":0}`,
	} {
		b := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
			`"disposition":"completed-implemented-checks-passed","window":%s}`, recordSchemaVersion, block)
		if _, err := decodeRunRecord(b); err == nil {
			t.Fatalf("%s: a window that established nothing was read as evidence that the hold held", name)
		}
	}

	// The same document with a window that actually watched something must still decode, or the guard is
	// refusing the records it was written to protect.
	ok := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",`+refusedValidity+`,"window":{"node":"platform-worker",`+
		`"nodeUID":"uid-node","txID":"tx-1111","baselineResourceVersion":"1000","openedAt":"t",`+
		`"nodeVersionsObserved":1,"ending":"closed by the run","violationsObserved":0}}`, recordSchemaVersion)
	if _, err := decodeRunRecord(ok); err != nil {
		t.Fatalf("a well-formed record was refused by the window guard: %v", err)
	}
}

// testObservation is a view that covered its run: four streams, each resumed from a known point, each ended
// by the caller at the horizon, and establishment cheap against the window it was spent out of.
func testObservation() *observationEvidence {
	ev := &observationEvidence{
		Namespace:         "queuelab-r7",
		HorizonNs:         int64(150 * time.Second),
		Established:       true,
		EstablishedNs:     int64(120 * time.Millisecond),
		EstablishBudgetNs: establishBudget.Nanoseconds(),
	}
	for i, kind := range []string{kindMLTrainingJob, kindJob, kindWorkload, kindPod} {
		ev.Streams = append(ev.Streams, streamEvidence{
			Kind:                    kind,
			BaselineResourceVersion: fmt.Sprintf("%d", 2000+i),
			Ended:                   true,
			Cancelled:               true,
		})
	}
	return ev
}

// testQualification is a worker this run could measure on.
//
// It carries a canary reference for the same reason it carries a Ready flag and a device count: a worker that
// has the machinery but has never been shown able to stop a Pod is not one a run may measure on, so a
// qualification without one is not the "everything held" fixture these tests need it to be.
func testQualification() *qualification {
	return &qualification{
		Node: "platform-worker", NodeUID: "uid-node",
		AllocatableGPU: 2, RequiredGPU: 2,
		RequiredFrom: "x", RequiredBoundBy: boundByQuotaSum,
		Ready: true, Schedulable: true, PodsOnNode: 6,
		TerminationCanary: testCanaryReference(),
	}
}

// The observation is the one gate whose evidence a reader cannot go and re-check afterwards: a Node is still
// there to inspect and a worker's Pods are still listable, but a stream's baseline and its ending exist only
// for as long as the process does. A record without this block cannot distinguish a run that watched its
// whole window from one that watched thirty seconds of it, which is the single property the continuity gate
// exists to establish.
//
// Mutation that turns this red: drop `Observation: obs` from buildRecord's runRecord branch. The run still
// observes continuously, the ledger still refuses a truncated one, and every stored record goes back to being
// silent about which of the two it came from.
func TestRunRecordCarriesTheObservationAndStillDecodes(t *testing.T) {
	obs := testObservation()
	rec := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, testQualification(), testWindow(),
		obs, nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now())
	b, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRunRecord(b)
	if err != nil {
		t.Fatalf("a record carrying an observation must decode: %v\n%s", err, b)
	}
	if got.Observation == nil {
		t.Fatalf("the observation did not survive the round trip:\n%s", b)
	}
	if !reflect.DeepEqual(*got.Observation, *obs) {
		t.Fatalf("the observation changed on the round trip:\n got %+v\nwant %+v", *got.Observation, *obs)
	}
	// This document is the one shape that reaches verdictAdmissible, so it is also the positive half of
	// checkValidity: a guard that only ever refused would be a guard nothing could distinguish from a decoder
	// that refuses everything.
	if got.Validity.Verdict != verdictAdmissible || len(got.Validity.Failures) != 0 {
		t.Fatalf("a run with every gate's evidence intact is %q, got %+v", verdictAdmissible, got.Validity)
	}
	if !reflect.DeepEqual(got.Validity.UnimplementedGates, recordUnchecked(workloadProvenance{})) {
		t.Fatalf("an admissible record must carry what this build cannot check at all, and exactly that: %v",
			got.Validity.UnimplementedGates)
	}
	t.Logf("persisted record:\n%s", b)
}

// The case that makes the verdict a FIELD rather than a reading of the prose, and it is not hypothetical: the
// re-review of the ownership gate found it.
//
// A collector stream desyncs and the ownership window is violated in the same run. Both land on
// collector-desync — deliberately, because builder.Err() is the one thing that decides whether a number may
// exist — and LedgerBuilder.Desync keeps only the FIRST reason it is given. The stream's desync is recorded
// during the run and the window's verdict is folded in after the streams are joined, so `reason` names the
// stream and the exclusivity failure survives nowhere in the text at all.
//
// Mutation that turns this red: derive the verdict from o.Reason — for instance by returning
// failureExclusivity only when the reason mentions the worker. The reason here names the Pod stream and
// nothing else, so the exclusivity failure disappears exactly as it does today.
func TestValidityKeepsTheExclusivityFailureTheReasonHasLost(t *testing.T) {
	o := outcome{
		Disposition: dispCollectorDesync,
		Reason: "run invalidated: the Pod stream ended on its own while the run was still observing, so " +
			"every transition after that point is unobserved rather than absent",
	}
	win := testWindow()
	win.ViolationsObserved = 3
	win.Violations = []ownershipViolation{{At: "t", Reason: reasonInstalledDiverged, Detail: "taint stripped"}}
	// The observation is intact in this fixture on purpose: the desync reached the ledger but the record's
	// stream evidence would show a lost Pod stream too, and leaving that in would let the exclusivity failure
	// be "found" by a derivation that only looked at the streams.
	v := deriveValidity(o, nil, testQualification(), win, testObservation(), nil, false)

	if strings.Contains(o.Reason, "worker") || strings.Contains(o.Reason, "exclusiv") {
		t.Fatalf("this fixture only proves anything while the reason is silent about exclusivity, got %q", o.Reason)
	}
	if !slices.Contains(v.Failures, failureExclusivity) {
		t.Fatalf("the verdict lost the exclusivity failure the reason never carried: %+v", v)
	}
	if v.Verdict != verdictRefused {
		t.Fatalf("verdict = %q, want %q", v.Verdict, verdictRefused)
	}
}

// Each claim is derived from its own fields, so a record failing one has to fail exactly one. A derivation
// that collapsed them — returning early, or keying every claim off the disposition — would make the block a
// second spelling of `disposition` and buy a reader nothing.
//
// Mutations that turn this red, one per clause the rows exist to reach — every one of them was run:
//
//   - make deriveValidity stop at the first failure it appends (guard each check after the disposition with
//     `len(v.Failures) == 0`). Only the two-claim row at the bottom catches this, which is why it is not
//     padding.
//   - delete `s.LastStatus != ""` from observationContinuous. Nothing else in the package caught this: the
//     clause shipped in the first round of this gate with no fixture anywhere setting a LastStatus.
//   - delete the resume-point check from observationContinuous.
//   - make observationContinuous check only the streams that are PRESENT rather than the watchedKinds set
//     (`for _, present := range obs.Streams { s, ok := seen[present.Kind] ... }`).
//   - delete the `q.Node == "" || q.RequiredGPU < 1` floor from environmentEstablished.
func TestValidityNamesTheClaimTheFieldsActuallyFail(t *testing.T) {
	pass := outcome{Disposition: dispChecksPassed}
	lostStream := testObservation()
	lostStream.Streams[3].Cancelled = false
	unestablished := testObservation()
	unestablished.Established = false
	// A loss the cancellation MASKS: this stream forwarded a terminal 410 and was then cancelled at the
	// horizon like every other. Cancelled alone would wave it through, and an expired resume point is the one
	// thing RetryWatcher cannot resume past — the likeliest way a real apiserver breaks this gate.
	maskedLoss := testObservation()
	maskedLoss.Streams[3].LastStatus = "terminal watch error: too old resource version (code 410, reason Expired)"
	unknownResume := testObservation()
	unknownResume.Streams[2].BaselineResourceVersion = "0"
	// Three healthy streams and no Workload view at all: a length check would accept this, and a record
	// missing the kind that carries admission and preemption is not a view of a reclaim run.
	missingKind := testObservation()
	missingKind.Streams = missingKind.Streams[:3]
	unnamedNode := testQualification()
	unnamedNode.Node = ""
	// The floor: 0 >= 0 satisfies the capacity test on its own, so without it a qualification that established
	// nothing about nothing reads as a worker this run could measure on.
	noRequirement := testQualification()
	noRequirement.RequiredGPU, noRequirement.AllocatableGPU = 0, 0
	contended := testQualification()
	contended.GPUConsumers = []gpuConsumer{{Namespace: "tenant-a", Name: "train-7", Phase: "Running", GPUs: 1}}
	blindWindow := testWindow()
	blindWindow.NodeVersionsObserved = 0
	unrestored := testWindow()
	unrestored.Restoration = nil

	// The state that had no row: a worker was acquired and the sentinel could not be started, so the run is
	// refused with no view over a node that is already labelled and tainted. The emergency release synthesizes
	// this carrier so its audit survives. Both outcomes are asserted below, because the failure that mattered
	// was silent — a run in this state used to derive containment held on no evidence at all.
	neverOpenedRemoved := &ownershipWindow{Node: "w1", TxID: "tx-1", NeverOpened: true,
		Restoration: &restorationAudit{OurMarkersRemoved: true}}
	neverOpenedStranded := &ownershipWindow{Node: "w1", TxID: "tx-1", NeverOpened: true,
		Restoration: &restorationAudit{OurMarkersRemoved: false}}

	for _, tc := range []struct {
		name string
		o    outcome
		left []recordResidue
		qual *qualification
		win  *ownershipWindow
		obs  *observationEvidence
		want []string
	}{
		{"everything held", pass, nil, testQualification(), testWindow(), testObservation(), nil},
		{"cancelled at the horizon", outcome{Disposition: dispCancelled}, nil, testQualification(),
			testWindow(), testObservation(), []string{failureRunIncomplete}},
		{"a stream ended on its own", pass, nil, testQualification(), testWindow(), lostStream,
			[]string{failureObservation}},
		{"never established", pass, nil, testQualification(), testWindow(), unestablished,
			[]string{failureObservation}},
		{"a 410 masked by the horizon's own cancellation", pass, nil, testQualification(), testWindow(),
			maskedLoss, []string{failureObservation}},
		{"a stream resumed from an unknown point", pass, nil, testQualification(), testWindow(), unknownResume,
			[]string{failureObservation}},
		{"a kind that was never watched", pass, nil, testQualification(), testWindow(), missingKind,
			[]string{failureObservation}},
		{"never observed at all", pass, nil, testQualification(), testWindow(), nil,
			[]string{failureObservation}},
		{"a foreign GPU pod", pass, nil, contended, testWindow(), testObservation(),
			[]string{failureEnvironment}},
		{"never qualified", pass, nil, nil, testWindow(), testObservation(), []string{failureEnvironment}},
		{"a qualification naming no node", pass, nil, unnamedNode, testWindow(), testObservation(),
			[]string{failureEnvironment}},
		{"a requirement of nothing on a node advertising nothing", pass, nil, noRequirement, testWindow(),
			testObservation(), []string{failureEnvironment}},
		{"a window that compared nothing", pass, nil, testQualification(), blindWindow, testObservation(),
			[]string{failureExclusivity}},
		{"no window at all", pass, nil, testQualification(), nil, testObservation(),
			[]string{failureExclusivity}},
		{"residue left behind", pass, []recordResidue{{Kind: "Namespace", Name: "queuelab-r7", Absence: "present"}},
			testQualification(), testWindow(), testObservation(), []string{failureContainment}},
		{"restoration never audited", pass, nil, testQualification(), unrestored, testObservation(),
			[]string{failureContainment}},
		// Exclusivity fails on both of these for the same reason "no window at all" does: nothing was ever
		// compared. What separates them is containment, and only the audit can separate them.
		{"acquired, never observed, markers came off", pass, nil, testQualification(), neverOpenedRemoved,
			testObservation(), []string{failureExclusivity}},
		{"acquired, never observed, worker left marked", pass, nil, testQualification(), neverOpenedStranded,
			testObservation(), []string{failureExclusivity, failureContainment}},
		// The row the single-failure rows above cannot cover: two independent claims failing at once. Without
		// it a derivation that stopped at the first failure would satisfy every other row here, since each of
		// them has exactly one, and the block would silently become "the first thing that went wrong" — which
		// is the free-text reason it exists to replace.
		{"two claims fail at once", pass, []recordResidue{{Kind: "Namespace", Name: "queuelab-r7", Absence: "present"}},
			testQualification(), testWindow(), lostStream, []string{failureObservation, failureContainment}},
	} {
		v := deriveValidity(tc.o, tc.left, tc.qual, tc.win, tc.obs, nil, false)
		if !reflect.DeepEqual(v.Failures, tc.want) {
			t.Errorf("%s: failures = %v, want %v", tc.name, v.Failures, tc.want)
		}
		wantVerdict := verdictAdmissible
		if len(tc.want) > 0 {
			wantVerdict = verdictRefused
		}
		if v.Verdict != wantVerdict {
			t.Errorf("%s: verdict = %q, want %q", tc.name, v.Verdict, wantVerdict)
		}
	}
}

// A preview's author declared its output uncountable in advance, so its record must not be readable as a pass
// however well every other field came out. This is the same guarantee previewRecord's missing events field
// provides, applied to the field a reader classifies on.
//
// This became the ONLY thing -preview decides when gateRefusal was removed, which raises what this test is
// worth rather than lowering it: a preview now runs every gate a run does, so its record differs from an
// admissible one in this field alone, and the `if preview` branch below is the whole of the distinction.
//
// Mutation that turns this red: drop the `if preview` branch from deriveValidity. A preview against a clean
// cluster then writes a record that says it is admissible, which is the one thing -preview exists to stop.
func TestAPreviewIsNeverAdmissibleHoweverWellItWent(t *testing.T) {
	pr, ok := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, testQualification(), testWindow(),
		testObservation(), nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, true, time.Now(), time.Now()).(previewRecord)
	if !ok {
		t.Fatal("a preview invocation must build a previewRecord")
	}
	if pr.Validity.Verdict != verdictPreview {
		t.Fatalf("a preview whose every gate passed is %q, got %q", verdictPreview, pr.Validity.Verdict)
	}
	if pr.Observation == nil {
		t.Fatal("a preview opens the same four streams a run does, and its record must say what they were")
	}
}

// The verdict's whole value is that a reader classifies on it without parsing prose, so a value that is
// neither documented name hands that reader something meaningless while looking like an answer. The blank is
// the case that matters: it is what every record written before this schema decodes into.
//
// Mutation that turns this red: delete the default arm from checkValidity.
func TestDecodeRunRecordRefusesAVerdictNoBuildEverWrote(t *testing.T) {
	for name, block := range map[string]string{
		"no verdict at all":        `{"verdict":""}`,
		"a name nobody defined":    `{"verdict":"probably-fine"}`,
		"a refusal naming nothing": `{"verdict":"refused"}`,
	} {
		b := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
			`"disposition":"completed-implemented-checks-passed","validity":%s}`, recordSchemaVersion, block)
		if _, err := decodeRunRecord(b); err == nil {
			t.Fatalf("%s: a record whose verdict cannot be read was read as though it had been judged", name)
		}
	}
}

// The forgery this guard exists for. verdictAdmissible is the strongest thing a record can say, and a
// document can simply assert it: the fields beside it are what make it evidence rather than a claim, so the
// admissible direction — and only that direction — is re-derived at decode.
//
// Mutation that turns this red: delete the deriveValidity re-check from checkValidity's verdictAdmissible
// arm. Both documents below then decode, and a reader is handed "admissible" over a worker that was shared
// mid-run and over a run that never opened a stream.
func TestDecodeRunRecordRefusesAnAdmissibleVerdictItsFieldsDoNotSupport(t *testing.T) {
	admissible := `{"verdict":"admissible-under-implemented-gates","deviceEvidence":"device-not-observed"}`
	window := `{"node":"platform-worker","nodeUID":"uid-node","txID":"tx-1111",` +
		`"baselineResourceVersion":"1000","openedAt":"t","nodeVersionsObserved":9,"ending":"closed by the run",` +
		`"violationsObserved":%d,"restoration":{"before":{"observed":true},"after":{"observed":true},` +
		`"ourMarkersRemoved":true}}`
	// The canary reference is part of what makes a record admissible now, so a forgery has to carry one too:
	// without it every document below is refused for the environment rather than for the thing it is testing,
	// and the last case — the well-formed one that must decode — could never pass.
	qual := `{"node":"platform-worker","nodeUID":"uid-node","allocatableGPU":2,"requiredGPU":2,` +
		`"requiredFrom":"x","requiredBoundBy":"nominal-quota-sum","ready":true,"schedulable":true,` +
		`"podsOnNode":6,"terminationCanary":` + canaryReferenceJSON(t) + `}`
	// All four watched kinds, because a document short of one is not a continuous view and would be refused
	// by the clause above the forgery this test is about — which would make the test pass for the wrong reason.
	stream := func(kind string) string {
		return fmt.Sprintf(`{"kind":%q,"baselineResourceVersion":"2000","baselineObjects":0,`+
			`"ended":true,"cancelled":true,"stopped":false}`, kind)
	}
	obs := `{"namespace":"queuelab-r7","horizonNs":1,"established":true,"establishedNs":1,` +
		`"establishBudgetNs":1,"streams":[` + stream(kindMLTrainingJob) + `,` + stream(kindJob) + `,` +
		stream(kindWorkload) + `,` + stream(kindPod) + `]}`

	// A window that saw the worker shared, under a verdict that says every gate passed: exactly the shape the
	// exclusivity discriminator exists to catch, forged rather than measured.
	shared := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","validity":%s,"qualification":%s,"window":`+window+
		`,"observation":%s}`, recordSchemaVersion, admissible, qual, 4, obs)
	if _, err := decodeRunRecord(shared); err == nil {
		t.Fatal("a record claiming to be admissible over a violated ownership window was accepted")
	}
	// The same document with nothing to say about its observation: an admissible verdict over a run whose
	// streams nobody recorded.
	unobserved := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","validity":%s,"qualification":%s,"window":`+window+`}`,
		recordSchemaVersion, admissible, qual, 0)
	if _, err := decodeRunRecord(unobserved); err == nil {
		t.Fatal("a record claiming to be admissible while carrying no observation at all was accepted")
	}
	// A document that claims the strongest verdict AND lists claims it did not support is contradicting
	// itself, whatever its other fields say. It is refused by its own arm rather than by the re-derivation
	// above, because the fields here DO support admissible — only the block disagrees with itself.
	//
	// Mutation that turns this red: delete the `len(r.Validity.Failures) > 0` check from checkValidity's
	// admissible arm. The document then decodes, and a reader is handed a verdict and a list of reasons not to
	// believe it in the same object.
	contradictory := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","validity":{"verdict":%q,"failures":[%q]},`+
		`"qualification":%s,"window":`+window+`,"observation":%s}`,
		recordSchemaVersion, verdictAdmissible, failureExclusivity, qual, 0, obs)
	if _, err := decodeRunRecord(contradictory); err == nil {
		t.Fatal("a record claiming to be admissible while listing a failed claim was accepted")
	}

	// And the supported one must decode, or the guard is refusing the records it was written to protect.
	ok := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","validity":%s,"qualification":%s,"window":`+window+
		`,"observation":%s}`, recordSchemaVersion, admissible, qual, 0, obs)
	if _, err := decodeRunRecord(ok); err != nil {
		t.Fatalf("a well-formed admissible record was refused: %v", err)
	}
}

// The reviewer's throwaway probe, made permanent: a forged record whose Pod stream forwarded a terminal 410,
// followed by a healthy DUPLICATE Pod stream, decoded as admissible.
//
// It is a regression test for the fix to the missing-kind finding rather than for an original gap. That fix
// keyed the streams by kind so the record's coverage could be checked against watchedKinds, and a kind-keyed
// map keeps only the LAST entry — so the loss sat in an element nothing read. Every resume point here is
// valid, which is what keeps decodeRunRecord's own all-streams guard silent and leaves the verdict as the
// only thing standing between this document and a reader treating it as evidence.
//
// It goes through decodeRunRecord rather than calling observationContinuous, deliberately: a forged document
// arrives at the decoder, and this pins the whole path checkValidity's admissible re-derivation exists for.
// The refusal is asserted to name observation-not-continuous rather than merely to be non-nil, because a
// malformed fixture would otherwise be refused by DisallowUnknownFields and the test would pass having
// proved nothing.
//
// No build writes a duplicate kind, so no real run was ever affected.
//
// Mutation that turns this red: delete the `len(obs.Streams) != len(watchedKinds())` check from
// observationContinuous. The healthy duplicate then masks the 410 and the document decodes as admissible.
func TestDecodeRunRecordRefusesAnAdmissibleVerdictHidingALossBehindADuplicateStream(t *testing.T) {
	stream := func(kind, lastStatus string) string {
		return fmt.Sprintf(`{"kind":%q,"baselineResourceVersion":"2000","baselineObjects":0,`+
			`"ended":true,"cancelled":true,"stopped":false,"lastStatus":%q}`, kind, lastStatus)
	}
	const expired = "terminal watch error: too old resource version (code 410, reason Expired)"
	forged := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed","validity":{"verdict":%q},`+
		`"qualification":{"node":"platform-worker","nodeUID":"uid-node","allocatableGPU":2,"requiredGPU":2,`+
		`"requiredFrom":"x","requiredBoundBy":"nominal-quota-sum","ready":true,"schedulable":true,`+
		`"podsOnNode":6},"window":{"node":"platform-worker","nodeUID":"uid-node","txID":"tx-1111",`+
		`"baselineResourceVersion":"1000","openedAt":"t","nodeVersionsObserved":9,`+
		`"ending":"closed by the run","violationsObserved":0,"restoration":{"before":{"observed":true},`+
		`"after":{"observed":true},"ourMarkersRemoved":true}},`+
		`"observation":{"namespace":"queuelab-r7","horizonNs":1,"established":true,"establishedNs":1,`+
		`"establishBudgetNs":1,"streams":[%s,%s,%s,%s,%s]}}`,
		recordSchemaVersion, verdictAdmissible,
		stream(kindMLTrainingJob, ""), stream(kindJob, ""), stream(kindWorkload, ""),
		stream(kindPod, expired), stream(kindPod, ""))

	_, err := decodeRunRecord(forged)
	if err == nil {
		t.Fatal("a forged record whose Pod stream forwarded a 410 decoded as admissible, because a healthy " +
			"duplicate of the same kind sits after it and only the last entry per kind is read")
	}
	if !strings.Contains(err.Error(), failureObservation) {
		t.Fatalf("the refusal does not name the claim the duplicate was hiding: %v", err)
	}
}

// The record's statement about what it could not check must describe the build that wrote it, and never the
// roadmap of work still to do that the executable's removed refusal used to print for an operator.
//
// This is the defect the first round of this gate shipped and a live run exposed: the block carried that
// roadmap, so a record holding an observation, a qualification, a window and a derived verdict asserted
// inside itself that this build had no "synchronized list+watch with resourceVersion continuity" and no
// "validity-bearing run artifact" — the block printed beside it, and the block making the assertion. A reader
// following that list would discount exactly the evidence this gate exists to add.
//
// The canary is asserted as PRESENT as well, because a list narrowed to nothing would pass a "does not name
// the implemented gates" check while quietly claiming this build checks everything. Something is genuinely
// missing and the record has to keep saying so.
//
// The three implemented gates below are still spelled the way the roadmap spelled them even though nothing
// prints that wording any more, and that is on purpose: those strings are what a reverted or re-copied list
// would contain, so pinning the old spelling is what keeps this test able to catch the reversion.
//
// Mutation that turns this red: make deriveValidity fill UnimplementedGates from a list naming the gates
// rather than from recordUnchecked — restoring the roadmap the removed refusal used to print is the concrete
// form of that, and it fails all three clauses at once.
func TestTheRecordsUncheckedListDescribesTheBuildNotTheRoadmap(t *testing.T) {
	rec, ok := buildRecord(outcome{Disposition: dispChecksPassed}, nil, nil, testQualification(), testWindow(),
		testObservation(), nil, recordIdentity{RunID: "r7", Arm: "A-honor"}, nil, false, time.Now(), time.Now()).(runRecord)
	if !ok {
		t.Fatal("a non-preview invocation must build a runRecord")
	}
	got := strings.Join(rec.Validity.UnimplementedGates, "\n")
	if !strings.Contains(got, "termination canary") {
		t.Fatalf("the record no longer says what this build still cannot check: %q", got)
	}
	// And it says the RESIDUAL rather than the gate. The canary gate now exists and every run stands on one, so
	// an entry still claiming nothing checks that the worker can stop a Pod would be exactly the defect this
	// list was created to remove — a durable statement about the document it sits in, false about the build
	// that wrote it — pointing the other way.
	if strings.Contains(got, "nothing in this build checks") {
		t.Fatalf("the record claims this build cannot check what the run it describes was refused or qualified "+
			"on: %q", got)
	}
	// Every gate this build DOES implement, named by the thing a reader would look for and spelled as the
	// removed roadmap spelled it, so a record repeating that list fails on all three at once.
	for _, implemented := range []string{"resourceVersion continuity", "validity-bearing run artifact",
		"continuous ownership evidence"} {
		if strings.Contains(got, implemented) {
			t.Fatalf("the record claims this build lacks %q while carrying that gate's own evidence beside "+
				"the claim; a reader is being told to discount the block the claim is written in: %q",
				implemented, got)
		}
	}
}

// The successor to the four tests deleted from spine_test.go with gateRefusal and unimplementedGates().
//
// Those four kept a list of missing work from decaying in three specific ways — going empty, naming something
// already built, being widened back after somebody narrowed it — and the list they guarded no longer exists.
// The list that survives is recordUnchecked(workloadProvenance{}), which is strictly more consequential: the old one was printed
// once on a terminal and could be over-broad at no cost, while this one is persisted into every record ever
// written and read by someone who has only the file. So the same three pressures move here, applied to the
// stronger list.
//
// The test asserts on recordUnchecked(workloadProvenance{}) directly rather than through a record, unlike the test above it, and
// the two are not redundant. That one proves the list REACHES the document; this one proves the list still
// SAYS the four things the residual consists of. Either alone leaves a hole: a correct list that buildRecord
// dropped, or a faithfully persisted list narrowed to a sentence that means nothing.
//
// The clauses are exactly the residual the canary gate does not close, and each names a distinct thing a
// reader would otherwise assume was covered — that a run's Pods travel MLTrainingJob, Kueue admission and the
// Job controller, and that the template probed is the one this binary renders rather than the one the
// operator image on the cluster does. Truncating any one of them is an overclaim about coverage, which is the
// direction this whole list exists to guard.
//
// Mutation that turns this red: have recordUnchecked(workloadProvenance{}) return nil, or drop any one of the four clauses below
// from its entry — for instance the last, on the grounds that the template is keyed and probed and therefore
// "covered", which is precisely the overclaim the entry was narrowed twice to avoid making.
func TestTheRecordsUncheckedListNamesTheResidualAndNothingWider(t *testing.T) {
	unchecked := recordUnchecked(workloadProvenance{})
	// Two entries, and the count is asserted so the list cannot grow into a roadmap. Each has to be justified
	// the same way the first was: verifiable in the code of the build that wrote the document, because a reader
	// holding the file has nothing else to check it against.
	if len(unchecked) != 2 {
		t.Fatalf("want exactly 2 unchecked entries, got %d: %v", len(unchecked), unchecked)
	}
	for i, e := range unchecked {
		if strings.TrimSpace(e) == "" {
			t.Fatalf("entry %d is empty; a record that claims to name what it cannot check and names nothing "+
				"claims this build checks everything, which is the strongest thing in the file and the least "+
				"supported", i)
		}
	}
	// The four clauses of the canary residual. Each is something a run does and the canary does not, so each is
	// a sentence a reader needs in order to size what the reading covers. "Job controller" rather than the bare
	// "Job", because that is a substring of MLTrainingJob above and would be satisfied by the clause before it.
	for _, want := range []string{
		"MLTrainingJob",  // nothing here submits one, so the reconcile loop is not travelled
		"Kueue",          // and therefore nothing is admitted
		"Job controller", // and none creates the Pod, so it carries no owner reference and no Job labels
		"THIS BINARY",    // and the template is rendered here, not by the operator image on the cluster
	} {
		if !strings.Contains(unchecked[0], want) {
			t.Fatalf("the canary residual no longer names %q, so a reader is told the reading covers a path it "+
				"does not travel: %q", want, unchecked[0])
		}
	}
	// The device-use entry, which is DERIVED and therefore has two shapes to check rather than one literal
	// to pin.
	//
	// It used to be a constant, and the constant went false: it said this build's workload "is pure Python
	// float arithmetic making no driver call" and kept saying it into every record after the workload gained
	// a device path. On a successful GPU run the same document would have carried
	// validity.deviceEvidence: "device-work-observed" beside a paragraph saying that was impossible.
	//
	// This test pinned "fake device plugin" and so shared the defect: that is a property of the CLUSTER a run
	// happens on, not of the build, and on the hardware this study rents the plugin is real. What is asserted
	// now is what each shape must say for the entry to do its job.
	for _, want := range []string{
		"discardedIterations", // the field it qualifies, by name
		"did NOT establish",   // the claim itself, unambiguously
		"whyNot",              // where the specific reason lives, so this is not the only place to look
		"reservation",         // what the count actually describes
	} {
		if !strings.Contains(unchecked[1], want) {
			t.Fatalf("the unestablished device-use entry no longer names %q, so an admissible verdict can be "+
				"read as saying a GPU did the counted work: %q", want, unchecked[1])
		}
	}
	// And the shape a successful session writes, which no run has produced yet and which must not read as an
	// unqualified claim. What it still cannot check is exclusivity against activity wearing no Pod label at
	// all, and saying so is the entry's whole purpose on that path.
	established := recordUnchecked(workloadProvenance{DeviceUseEstablished: true})[1]
	for _, want := range []string{
		"established",       // it says what the run did establish
		"UNLABELLED",        // and the one thing the observer structurally cannot see
		"deviceObservation", // and where the evidence for it is, so a reader can re-derive rather than trust
	} {
		if !strings.Contains(established, want) {
			t.Fatalf("the established device-use entry no longer names %q, so it claims more than the "+
				"observer can support: %q", want, established)
		}
	}
}

// The provenance travels with the number, in the record, not only in a source comment. A consumer reading
// discardedIterations without it has no way to know the count is CPU work.
//
// Mutation that turns this red: drop Workload from measurementOf.
func TestTheMeasurementSaysWhatProducedItsIterations(t *testing.T) {
	m := measurementOf(&queuelab.LabResult{}, 1, nil, nil)
	if m == nil {
		t.Fatal("no measurement was produced")
	}
	if m.Workload.DeviceUseEstablished {
		t.Fatal("this build claims device use was established; nothing in it can establish that")
	}
	if strings.TrimSpace(m.Workload.WhyNot) == "" {
		t.Fatal("device use is unestablished and the record does not say why, which reads as an oversight " +
			"rather than a stated limit")
	}
	// A measurement built from no events cannot know which loop ran, and it says so rather than naming the
	// fallback. Defaulting to the CPU kind here would publish a genuine device run under the fallback's name
	// on any run whose ledger this build could not read -- which is the same guess the literal used to be.
	if m.Workload.Kind != unreportedWorkloadKind {
		t.Fatalf("a measurement with no ledger named a workload anyway: %q", m.Workload.Kind)
	}
	if strings.TrimSpace(m.Workload.CountedUnit) == "" {
		t.Fatal("one iteration is unexplained, so the count has no unit a reader can size")
	}
}

// startWatchStream refuses "" and "0" outright, because a watch that begins at "now" or at an arbitrary
// cached point cannot speak for the interval before it attached — which is the defect the continuity gate
// replaced. So a stream recorded with either is a hand-edited or future-written document, and it must not be
// readable as the continuous view the block's existence claims.
//
// Mutation that turns this red: delete the observation guard from decodeRunRecord.
func TestDecodeRunRecordRefusesAStreamWithNoResumePoint(t *testing.T) {
	for _, rv := range []string{"", "0"} {
		b := fmt.Appendf(nil, `{"schemaVersion":%d,"runID":"r7","arm":"A-honor",`+
			`"disposition":"collector-desync",`+refusedValidity+`,"observation":{"namespace":"queuelab-r7",`+
			`"horizonNs":1,"established":true,"establishedNs":1,"establishBudgetNs":1,"streams":[`+
			`{"kind":"Pod","baselineResourceVersion":%q,"baselineObjects":0,"ended":true,"cancelled":true}]}}`,
			recordSchemaVersion, rv)
		if _, err := decodeRunRecord(b); err == nil {
			t.Fatalf("a stream recorded at resume point %q was read as a continuous view", rv)
		}
	}
}

// The record shape gate 3 shipped, refused rather than read. The asymmetry is the one recordSchemaVersion's
// comment describes and the reason the bump is the right instrument: a version-4 record read by a version-3
// build already fails loudly on DisallowUnknownFields, while a version-3 record read by this build decodes
// without complaint into a nil observation and a blank verdict — a run that "never observed anything" and was
// "never judged", said about a run that did both under rules that could not record either.
//
// Two mutations turn this red, and they are red for different halves. Reverting recordSchemaVersion to 3
// alone still refuses this document — the verdict guard catches the blank — but the refusal then names no
// version, and the second assertion fails: the operator is told the verdict is unreadable rather than which
// build wrote the file. Reverting the version AND deleting the verdict guard is the pre-fix build, and the
// document decodes clean, so the first assertion fires with a nil observation in hand.
func TestDecodeRunRecordRefusesTheShapeThatPredatesTheObservationBlock(t *testing.T) {
	preFix := []byte(`{"schemaVersion":3,"runID":"g3a","arm":"A-honor",` +
		`"disposition":"completed-implemented-checks-passed","qualification":{` +
		`"node":"platform-worker","nodeUID":"uid-node","allocatableGPU":2,"requiredGPU":2,` +
		`"requiredFrom":"x","requiredBoundBy":"nominal-quota-sum","ready":true,"schedulable":true,` +
		`"podsOnNode":6},"window":{"node":"platform-worker","nodeUID":"uid-node","txID":"tx-1111",` +
		`"baselineResourceVersion":"1000","openedAt":"t","nodeVersionsObserved":9,` +
		`"ending":"closed by the run","violationsObserved":0}}`)

	got, err := decodeRunRecord(preFix)
	if err == nil {
		t.Fatalf("a record written before the observation was recorded was read under today's rules; its "+
			"observation came back as %v and its verdict as %q, and a reader classifying on either would be "+
			"told a run watched nothing and was judged by nobody", got.Observation, got.Validity.Verdict)
	}
	for _, want := range []string{"3", fmt.Sprint(recordSchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal %q does not name version %s; an operator holding the file has to be told "+
				"which build wrote it and which one is reading", err, want)
		}
	}
}

// A record written before the dose regime was named must stop decoding, because its silence is not neutral.
//
// The two regimes measure different quantities: self-completing reports what the victim's remaining WORK
// costs the owner, grace-bounded what the platform's configured PATIENCE costs it. Every record before this
// bump ran the first, but says nothing — so a reader comparing an old document against a new grace-bounded
// one would be comparing numbers that never answered the same question, with nothing in either file to warn
// them. Refusing the old version is what makes the omission visible instead of silently favourable.
//
// Mutation that turns this red: leave recordSchemaVersion at 6 — both halves, the constant and the document.
func TestARecordFromBeforeTheDoseRegimeIsRefusedRatherThanAssumedDefault(t *testing.T) {
	if recordSchemaVersion < 7 {
		t.Fatalf("recordSchemaVersion is %d; the run now measures one of two dose regimes and a record that "+
			"cannot say which describes an experiment the reader has to guess at", recordSchemaVersion)
	}
	older := fmt.Appendf(nil, `{"schemaVersion":6,"runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",%s}`, refusedValidity)
	if _, err := decodeRunRecord(older); err == nil {
		t.Fatal("a record written before the dose regime was named decoded under today's rules, so its silence " +
			"about which regime ran reads as the default rather than as an unanswered question")
	}
	// The same document at today's version, carrying the regime, must decode — or this test would pass just as
	// well against a decoder that had stopped reading records altogether.
	current := fmt.Appendf(nil, `{"schemaVersion":%d,"dose":"self-completing","runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",%s}`, recordSchemaVersion, refusedValidity)
	if _, err := decodeRunRecord(current); err != nil {
		t.Fatalf("a record at today's version naming its regime must decode: %v", err)
	}
}

// A record from before the measurement block is refused, because the number it reported could be a value or
// a floor and the file said nothing about which.
//
// Waste is censored when an attempt's stop is observed BEYOND the horizon: the loss is attributable but its
// interval is truncated, so wastedGPUSeconds becomes a lower bound. The printed result always said so; the
// durable artifact did not, and the artifact is what outlives the terminal. On a slower cluster — a longer
// image pull pushes every stop later — censoring stops being exotic, which is why this cannot wait for the
// hardware that makes it common.
//
// The horizon is in the same block for a related reason: without it a reader holding the ledger cannot
// replay it to the same boundary, so none of the numbers could be recomputed even in principle.
//
// Mutation that turns this red: leave recordSchemaVersion at 7 — both halves, the constant and the document.
func TestARecordFromBeforeTheMeasurementBlockIsRefused(t *testing.T) {
	if recordSchemaVersion < 8 {
		t.Fatalf("recordSchemaVersion is %d; the record now carries what the run measured and the boundary it "+
			"measured to, and a document without them cannot say whether its waste is a value or a floor",
			recordSchemaVersion)
	}
	older := fmt.Appendf(nil, `{"schemaVersion":7,"dose":"self-completing","runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",%s}`, refusedValidity)
	if _, err := decodeRunRecord(older); err == nil {
		t.Fatal("a record from before the measurement block decoded under today's rules, so its silence about " +
			"censoring reads as a run whose waste was fully measured")
	}
	// The censoring flag must MEAN something in a decoded record, and after the decoder began re-deriving
	// the measurement there is exactly one way to test that: a document may carry the flag its own ledger
	// produces, and may not carry the other one.
	//
	// This used to hand-build a censored measurement and assert it round-tripped. That fixture cannot exist
	// now, and its impossibility is the feature -- a record claiming its waste is a floor, on a ledger where
	// every attempt stopped inside the horizon, is a record that has downgraded its own headline for free.
	real := committedRecord(t)
	got, err := decodeRunRecord(real)
	if err != nil {
		t.Fatalf("a record this build wrote must decode: %v", err)
	}
	if got.Measurement == nil {
		t.Fatal("the measurement block did not survive the wire")
	}
	if got.Measurement.HorizonNs == 0 {
		t.Fatal("the horizon did not survive, so a reader holding the ledger cannot replay it to the same " +
			"boundary and none of the numbers could be recomputed even in principle")
	}
	doc := committedRecordDoc(t)
	doc["measurement"].(map[string]any)["censored"] = !got.Measurement.Censored
	flipped, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := decodeRunRecord(flipped); err == nil {
		t.Fatal("a record whose censoring flag contradicts its own ledger decoded; the flag is what tells a " +
			"reader whether wastedGPUSeconds is a value or a floor, and either direction is a free rewrite " +
			"of the headline")
	}
}

// The projection is what carries the reconstruction into the artifact, so it is what has to be tested.
//
// The schema test above builds a measurement literal and round-trips it, which proves the wire format and
// nothing about whether the run's own numbers reach it — dropping the censoring flag inside measurementOf
// left that test green. A mutation that survives is a coverage hole, and this is the hole.
//
// Mutations that turn this red: hard-code Censored to false; or return a zero block instead of nil for a run
// that never reconstructed.
func TestMeasurementOfCarriesWhatTheRunActuallyMeasured(t *testing.T) {
	if m := measurementOf(nil, 150e9, nil, nil); m != nil {
		t.Fatalf("a run that never reconstructed must project no measurement, got %+v; a block of zeroes reads "+
			"as a run that measured nothing rather than one that never got to measure", m)
	}
	res := &queuelab.LabResult{
		TotalWastedGPUSeconds:          12.5,
		TotalWasteLowerBoundGPUSeconds: 9.5,
		AnyWasteCensored:               true,
		UnfinishedAtHorizon:            2,
	}
	m := measurementOf(res, 150e9, nil, nil)
	if m == nil {
		t.Fatal("a run that reconstructed must project a measurement")
	}
	if !m.Censored {
		t.Fatal("the run's waste was censored and the record says it was fully measured; a reader quoting " +
			"wastedGPUSeconds out of this file would be quoting a floor as a value")
	}
	if m.WastedGPUSeconds != res.TotalWastedGPUSeconds || m.WasteLowerBoundGPUSeconds != res.TotalWasteLowerBoundGPUSeconds {
		t.Fatalf("the two waste figures must be carried separately so a reader never infers which they hold: %+v", m)
	}
	if m.UnfinishedAtHorizon != res.UnfinishedAtHorizon || m.HorizonNs != 150e9 {
		t.Fatalf("projection lost the boundary or the unfinished count: %+v", m)
	}
}

// A record from before the iteration count is refused, because its silence about discarded WORK reads as a
// run that measured it and lost none.
//
// GPU-seconds say how long a device was held; iterations say what was thrown away. A workload that computed
// nothing produced the same waste figure as one that saturated the card, which is the ambiguity Stage A's
// counting exists to end and this field carries into the artifact.
//
// Mutation that turns this red: leave recordSchemaVersion at 8 — both halves.
func TestARecordFromAnEarlierSchemaIsRefused(t *testing.T) {
	// The version is pinned so a wire change cannot ship without someone deciding to bump it. Version 10
	// removed LifecycleEvent.tenant and .gpuCount: nothing ever wrote them, so every event in every record
	// this build had produced carried "" and 0 beneath two sentences saying they meant something. Version 12
	// added measurement.resolution.resolvedToNs, where an absent value decodes to zero and zero is the
	// strongest claim the field can make -- so every earlier record would assert perfect resolution in the
	// one field a consumer is meant to act on. Version 13 added validity.deviceEvidence, so that whether a
	// run established device WORK is a field a consumer classifies on rather than a paragraph of English in
	// unimplementedGates. Version 14 added measurement.ownerAdmitToReadyNs, where the absent value is nil --
	// which this build genuinely writes, for a run whose owner never came back. Without the bump an older
	// record reads as the experiment's worst outcome rather than as a run nobody measured it on. Version 15
	// changed what resolvedToNs MEANS (spread + quantisation, not max) and where the ledger's component
	// stamps are sampled (every endpoint kind, not stops alone), so an older record carries the same field
	// names holding different quantities.
	// Version 17 made the workload's provenance evidence rather than an assertion: events carry the kind and
	// device status the container itself reported, and measurement.workload is derived from them. A
	// version-16 record says "pure Python arithmetic" because its build hardcoded that, not because anything
	// in the run said so, and nothing in the document distinguishes the two. Version 17 made the device
	// claim a boolean derived from an observation the record did not carry; version 18 carries the
	// observation -- the observer's declared identity and endpoint, the window judged, and the samples the
	// gate read -- and re-runs the gate over them at decode. A version-17 record asserts; an 18 can be
	// refuted.
	//
	// Version 19 split the device claim into the two intervals it was always two questions about: the
	// exclusivity, coverage and continuity clauses read the HOLD, and the busyness clause reads the victim's
	// own ATTEMPT. Asking all of them over the hold refused the arm that honours SIGTERM for being idle while
	// it was being terminated -- and passed the arm that ignores it, which computes through its whole hold --
	// so -require-device removed the short arm and kept the long one, leaving no contrast to form. A
	// version-18 record that CARRIES an observation was judged by the collapsed question and is refused; one
	// that carries none is byte-identical in meaning under both, and is accepted, because every run this lab
	// has taken ran on a fake device plugin with no observer and refusing them would orphan the set the
	// committed documents quote.
	if recordSchemaVersion != 19 {
		t.Fatalf("recordSchemaVersion is %d; if the wire format changed again, bump this and say what changed",
			recordSchemaVersion)
	}
	// Both predecessors, not only the immediate one. DisallowUnknownFields makes a version-9 document fail on
	// the removed fields and a version-8 one fail on the version alone, and a decoder that accepted either
	// would be reading a document whose fields do not mean what this build thinks they do.
	for _, older := range []int{9, 10, 11, 12, 13, 14, 15, 16, 17} {
		b := fmt.Appendf(nil, `{"schemaVersion":%d,"dose":"self-completing","runID":"r7","arm":"A-honor",`+
			`"disposition":"completed-implemented-checks-passed",%s}`, older, refusedValidity)
		if _, err := decodeRunRecord(b); err == nil {
			t.Fatalf("a schema-%d record decoded under today's rules", older)
		}
	}

	// 18 is the one exception and it is conditional, so both sides of the condition are asserted here rather
	// than left to the sentence above.
	//
	// Without an observation there is no device claim for the split to have changed, and the twelve runs this
	// repository quotes are all of that kind: refusing them would orphan the set to no benefit, which has
	// happened here before.
	noObs := fmt.Appendf(nil, `{"schemaVersion":18,"dose":"self-completing","runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",%s}`, refusedValidity)
	if _, err := decodeRunRecord(noObs); err != nil {
		t.Fatalf("a schema-18 record with no device observation was refused (%v); every committed record is "+
			"of that shape and this build reads them correctly", err)
	}

	// WITH one, it was judged by the collapsed question -- busyness over the hold -- and re-running the split
	// gate over it would reach a verdict its run never took. That is a different document, not an older
	// spelling of this one.
	withObs := fmt.Appendf(nil, `{"schemaVersion":18,"dose":"self-completing","runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",`+
		`"deviceObservation":{"observer":"dcgm","identity":"dcgm@sha256:abc","endpoint":"http://x/metrics",`+
		`"declared":true,"startedNs":0,"endedNs":1,"holdFromNs":0,"holdToNs":1,"workFromNs":0,"workToNs":1,`+
		`"podUID":"victim-uid"},%s}`, refusedValidity)
	if _, err := decodeRunRecord(withObs); err == nil {
		t.Fatal("a schema-18 record carrying a device observation decoded; its claim was judged by the " +
			"collapsed question and this build asks a different one")
	} else if !strings.Contains(err.Error(), "schema 18 is not 19") {
		t.Fatalf("refused for an unexpected reason, so this is not testing the schema rule: %v", err)
	}
}

// The sum is taken off the ledger, and only from attempts that actually carried a count.
//
// Mutations that turn this red: sum every event rather than the stopped ones; or return zero instead of nil
// when nothing carried a count, which would report "nothing was discarded" for a run that could not tell.
func TestDiscardedIterationsSumsOnlyWhatTheLedgerCarried(t *testing.T) {
	n := func(v int) *int { return &v }
	if got := discardedIterations(nil); got != nil {
		t.Fatalf("an empty ledger reported %d discarded, which claims a measurement nobody made", *got)
	}
	if got := discardedIterations([]queuelab.LifecycleEvent{
		{Type: queuelab.EventPodReady, Job: "a1", Iterations: n(500)},
	}); got != nil {
		t.Fatal("a non-terminal event was counted as discarded work")
	}
	got := discardedIterations([]queuelab.LifecycleEvent{
		{Type: queuelab.EventAttemptStopped, Job: "a2-borrow", Iterations: n(40)},
		{Type: queuelab.EventAttemptStopped, Job: "b1-owner"},
		{Type: queuelab.EventAttemptStopped, Job: "a1", Iterations: n(2)},
	})
	if got == nil || *got != 42 {
		t.Fatalf("summed %v, want 42 from the two attempts that carried a count", got)
	}

	// A row that COMPLETED did not discard its work, and the first version of this function counted it anyway.
	// On a live run that turned 21540 discarded iterations into 47513 by adding the owner's 25973 completed
	// ones, which would have been quoted as waste while being its opposite.
	//
	// Mutation that turns this red: drop the credited-job filter.
	withOwner := discardedIterations([]queuelab.LifecycleEvent{
		{Type: queuelab.EventAttemptStopped, Job: "a2-borrow", Iterations: n(21540)},
		{Type: queuelab.EventAttemptStopped, Job: "b1-owner", Iterations: n(25973)},
		{Type: queuelab.EventCompleted, Job: "b1-owner"},
	})
	if withOwner == nil || *withOwner != 21540 {
		t.Fatalf("summed %v, want 21540: the owner's row completed, so its work was credited rather than "+
			"discarded", withOwner)
	}

	// Wider than "the container failed": the self-completing regime's victim exits 0 and is still re-executed
	// from zero, because Kueue does not credit a suspended Job's finished attempt. Absence of a Completed
	// event is what says the work was lost.
	exitedZero := discardedIterations([]queuelab.LifecycleEvent{
		{Type: queuelab.EventAttemptStopped, Job: "a2-borrow", Reason: "Succeeded", Iterations: n(900)},
	})
	if exitedZero == nil || *exitedZero != 900 {
		t.Fatalf("summed %v, want 900: an attempt that exited cleanly and was never credited still lost its "+
			"work", exitedZero)
	}
}

// The record's resolution block and the report's floor have to be the same number, and the way two numbers
// like that stop being the same is by being derived twice. This asserts the projection is a projection.
//
// Mutation that turns this red: compute ResolvedToNs here as MedianNs, which is the figure a reader reaches
// for first and the wrong one — a harness uniformly late measures intervals exactly.
func TestResolutionOfProjectsTheReconstructionsOwnSpread(t *testing.T) {
	skew := func(ns int64) queuelab.LifecycleEvent {
		v := ns
		return queuelab.LifecycleEvent{ObservedSkewNs: &v}
	}
	events := []queuelab.LifecycleEvent{
		skew(430 * int64(time.Millisecond)),
		skew(1200 * int64(time.Millisecond)),
		skew(2389 * int64(time.Millisecond)),
	}
	got := resolutionOf(events)
	want := queuelab.SpreadOf(events)
	if got == nil || want == nil {
		t.Fatal("three skewed events bounded nothing")
	}
	if got.ResolvedToNs != want.FloorNs {
		t.Fatalf("record says resolvedTo=%d, reconstruction says floor=%d; the two derivations have drifted",
			got.ResolvedToNs, want.FloorNs)
	}
	if got.Samples != want.Samples || got.MinNs != want.MinNs || got.MaxNs != want.MaxNs ||
		got.MedianNs != want.MedianNs || got.QuantisationNs != want.QuantisationNs {
		t.Fatalf("projection lost a field: got %+v want %+v", got, want)
	}
	if got.ResolvedToNs == 0 {
		t.Fatal("a present resolution block carrying a zero floor claims perfect resolution")
	}
}

// A run whose events bounded nothing must produce no block at all, because a zero-valued block would say
// this run resolved everything — the one reading the version-12 bump exists to prevent.
func TestResolutionOfRefusesToInventABound(t *testing.T) {
	if r := resolutionOf(nil); r != nil {
		t.Fatalf("unbounded run produced a resolution block: %+v", r)
	}
	one := int64(50)
	if r := resolutionOf([]queuelab.LifecycleEvent{{ObservedSkewNs: &one}}); r != nil {
		t.Fatalf("a single reading bounded a spread: %+v", r)
	}
}

// Every record this lab has produced establishes reservation and nothing about computation, and the axis has
// to say so in a field rather than in the prose a consumer skips.
func TestEveryRunHereReadsDeviceNotObserved(t *testing.T) {
	v := deriveValidity(outcome{Disposition: dispChecksPassed}, nil, testQualification(), testWindow(),
		testObservation(), &measurement{WastedGPUSeconds: 41.0, Workload: cpuOnlyWorkload()}, false)
	if v.Verdict != verdictAdmissible {
		t.Fatalf("the fixture must be admissible or this test proves nothing: %v", v.Failures)
	}
	if v.DeviceEvidence != deviceNotObserved {
		t.Fatalf("a fully admissible run on a fake device plugin claims %q", v.DeviceEvidence)
	}
}

// A run that got nowhere near a measurement observed no device either, and the absent case must not be
// silent: silence is the value that means nothing at all.
func TestARefusedRunStillStatesTheDeviceAxis(t *testing.T) {
	v := deriveValidity(outcome{Disposition: dispRefusedBeforeCluster}, nil, nil, nil, nil, nil, false)
	if v.Verdict != verdictRefused {
		t.Fatalf("fixture should refuse: %+v", v)
	}
	if v.DeviceEvidence != deviceNotObserved {
		t.Fatalf("a refused run left the axis %q", v.DeviceEvidence)
	}
}

// The axis is derived, so a hand-edited document cannot assert device work its own measurement does not
// support. In a build with no observer this is the ONLY way the value could appear, which makes it the check
// that matters most.
func TestARecordCannotAssertDeviceWorkItsMeasurementDoesNotSupport(t *testing.T) {
	b, err := encodeRecord(runRecord{
		SchemaVersion: recordSchemaVersion, RunID: "r7", Arm: "A-honor", Dose: "self-completing",
		Disposition: string(dispChecksPassed),
		Measurement: &measurement{WastedGPUSeconds: 41.0, Workload: cpuOnlyWorkload()},
		Validity: validity{Verdict: verdictRefused, Failures: []string{failureObservation},
			DeviceEvidence: deviceWorkObserved},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeRunRecord(b); err == nil {
		t.Fatal("a record claimed device work while its workload states no driver call was ever made")
	}

	// And a blank axis is refused rather than read as the cautious answer.
	blank := fmt.Appendf(nil, `{"schemaVersion":%d,"dose":"self-completing","runID":"r7","arm":"A-honor",`+
		`"disposition":"completed-implemented-checks-passed",`+
		`"validity":{"verdict":"refused","failures":["observation-not-continuous"]}}`, recordSchemaVersion)
	if _, err := decodeRunRecord(blank); err == nil {
		t.Fatal("a record with no device axis decoded")
	}
}

// The write path cannot produce this pairing, so a document carrying it was hand-edited or written by a build
// whose observer was misattributing -- and either way it is the exact forgery that matters here.
//
// This was reproduced, not imagined: two committed records were edited to deviceUseEstablished:true with kind
// left at the CPU loop, and `-mode baseline` printed a table with the "device: NOT OBSERVED" banner gone. The
// axis check above did not catch it, because these records set the axis consistently too -- the contradiction
// lives entirely between the workload's own two fields.
//
// Mutation that turns this red: delete the pairing check from checkValidity.
func TestARecordCannotClaimDeviceWorkForAWorkloadThatMakesNoDriverCall(t *testing.T) {
	// Built on a document the write path produced, and edited in the one place the forgery lives. The old
	// version assembled a record field by field, which the decoder's ledger replay now refuses for a reason
	// that has nothing to do with the pairing under test -- so the fixture would have failed while telling
	// nobody whether the pairing was still caught.
	doc := committedRecordDoc(t)
	w := doc["measurement"].(map[string]any)["workload"].(map[string]any)
	if w["kind"] != cpuOnlyWorkloadKind {
		t.Fatalf("this fixture only forges anything while the CPU loop is the kind on record, got %v", w["kind"])
	}
	w["deviceUseEstablished"] = true
	delete(w, "whyNot")
	doc["validity"].(map[string]any)["deviceEvidence"] = deviceWorkObserved
	forged, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := decodeRunRecord(forged); err == nil {
		t.Fatal("a record claimed observed device work for a pure-CPU workload and decoded")
	}

	// The honest pairing still decodes: refusing both would make the guard indistinguishable from a decoder
	// that rejects every measurement it is handed.
	if _, err := decodeRunRecord(committedRecord(t)); err != nil {
		t.Fatalf("the pairing every run on this cluster actually writes was refused: %v", err)
	}
}

// The owner's wait requires BOTH admission and execution, and dropping either half is the worst mutation
// this field admits -- not because the number goes wrong, but because of WHICH number it goes to.
//
// AdmitToReadyNs is documented as valid only when the row was both Admitted and Executed, so a derivation
// that checks admission alone reads the field on a run where the owner never ran and finds its zero value.
// The record would then report that the quota owner was restored INSTANTLY, on precisely the runs where it
// was never restored at all. That is the dangerous-zero pattern this file exists to keep out, arriving
// through a two-word condition.
//
// Mutation that turns this red: drop either !o.Admitted or !o.Executed from ownerAdmitToReady.
func TestTheOwnersWaitNeedsBothAdmissionAndExecution(t *testing.T) {
	res := func(admitted, executed bool, ns int64) *queuelab.LabResult {
		return &queuelab.LabResult{Outcomes: []queuelab.WorkloadOutcome{
			{Job: queuelab.OwnRow, Admitted: true, Executed: true, AdmitToReadyNs: 999},
			{Job: queuelab.OwnerRow, Admitted: admitted, Executed: executed, AdmitToReadyNs: ns},
		}}
	}

	// Admitted and never ran: Kueue reserved the quota, the borrower kept the device, and the owner's Pod
	// never started. AdmitToReadyNs is zero here and means nothing.
	if got := ownerAdmitToReady(res(true, false, 0)); got != nil {
		t.Fatalf("an owner that was admitted and never ran reported a wait of %d ns; a record saying zero "+
			"claims instant restoration for the run where restoration never happened", *got)
	}
	// Never admitted at all.
	if got := ownerAdmitToReady(res(false, false, 0)); got != nil {
		t.Fatalf("an owner that was never admitted reported a wait of %d ns", *got)
	}
	// Both: the ordinary case, and the value must be the owner's rather than another row's.
	got := ownerAdmitToReady(res(true, true, 30806000000))
	if got == nil {
		t.Fatal("a restored owner reported no wait")
	}
	if *got != 30806000000 {
		t.Fatalf("wait = %d ns, want the owner row's 30806000000 and not another row's", *got)
	}
	// Ready observed without an admission. The cluster cannot do this -- Kueue admits before a Pod runs --
	// but the RECONSTRUCTION can: a collector that misses the Workload Admitted event while the Pod Ready
	// arrives leaves admitNs at zero, and AdmitToReadyNs becomes firstReady minus nothing, which is the whole
	// elapsed run rather than a wait. The admission half of the condition is what refuses that, and this is
	// the case that makes it more than decoration.
	if got := ownerAdmitToReady(res(false, true, 63854000000)); got != nil {
		t.Fatalf("a run whose admission was never observed reported a %d ns wait; with admitNs at zero that "+
			"figure is the elapsed run, not the owner's wait", *got)
	}

	// A trace with no owner row at all is not an owner that waited zero.
	empty := &queuelab.LabResult{Outcomes: []queuelab.WorkloadOutcome{{Job: queuelab.OwnRow, Admitted: true, Executed: true}}}
	if got := ownerAdmitToReady(empty); got != nil {
		t.Fatalf("a result with no owner row reported a wait of %d ns", *got)
	}
}

// The owner's wait, read off the two components' own clocks instead of this collector's arrival times.
//
// It earned its place immediately: across four otherwise identical grace-bounded runs on two nodes the
// arrival figure scattered by 931 ms while this one did not move at all, which is how the scatter was
// identified as watch jitter rather than as the cluster behaving differently — and how a node-sensitivity
// claim built on that scatter was caught before it was published.
//
// Mutation that turns this red: drop either precondition from ownerAdmitToReadyStamp, or return the arrival
// figure from it.
func TestTheOwnersWaitIsAlsoReadOffTheComponentsClocks(t *testing.T) {
	stamp := int64(31_000_000_000)
	res := func(admitted, executed bool, s *int64) *queuelab.LabResult {
		return &queuelab.LabResult{Outcomes: []queuelab.WorkloadOutcome{
			{Job: queuelab.OwnRow, Admitted: true, Executed: true, AdmitToReadyNs: 7, AdmitToReadyStampNs: &stamp},
			{Job: queuelab.OwnerRow, Admitted: admitted, Executed: executed,
				AdmitToReadyNs: 30_687_000_000, AdmitToReadyStampNs: s},
		}}
	}
	got := ownerAdmitToReadyStamp(res(true, true, &stamp))
	if got == nil {
		t.Fatal("a restored owner whose components both stamped their transitions reported no stamp interval")
	}
	if *got != stamp {
		t.Fatalf("stamp interval = %d, want the owner row's %d and not another row's or the arrival figure",
			*got, stamp)
	}
	// Same preconditions as the arrival figure: an owner that never ran has no interval, on either clock.
	if s := ownerAdmitToReadyStamp(res(true, false, &stamp)); s != nil {
		t.Fatalf("an owner that was admitted and never ran reported a stamp interval of %d", *s)
	}
	// And a run whose components published nothing reports nothing rather than a zero interval.
	if s := ownerAdmitToReadyStamp(res(true, true, nil)); s != nil {
		t.Fatalf("an unstamped run reported a stamp interval of %d, claiming instant restoration", *s)
	}
}

// busyObservation is an impeccable observation of one card: admissible source, named build, full coverage,
// one device UUID, busy at every instant.
//
// It is shared by the tests below so that what differs between them is the WORKLOAD's report and nothing
// else. A fixture rebuilt per test drifts, and a drifting fixture turns "the workload decided this" into
// "something about these two observations differed".
func busyObservation(uuid string) *queuelab.DeviceObservation {
	obs := &queuelab.DeviceObservation{
		Observer: queuelab.ObserverDCGM, ObserverIdentity: "dcgm@sha256:abc", Declared: true,
		StartedNs: 0, EndedNs: 40_000_000_000,
	}
	for at := int64(0); at <= 40_000_000_000; at += 1_000_000_000 {
		obs.Samples = append(obs.Samples, queuelab.DeviceSample{
			AtNs: at, DeviceUUID: uuid, PodUID: "victim-uid", UtilisationPercent: 91,
		})
	}
	return obs
}

// The axis MOVES, and until the workload gained a device path nothing in this repository could show that.
//
// The old version of this test asserted the opposite -- that the axis could not move in this build -- because
// the workload made no driver call and the contradiction check refused every observation of it. That was
// honest but it left the positive path untested: a GPU session would have been the first execution ever of
// the branch that publishes device work, and a defect there would have surfaced with the meter running.
//
// Mutations that turn this red, both run: set DeviceUseEstablished without consulting the observer (the
// no-observer half catches it), and drop the reported-kind comparison so the CPU fallback also passes (the
// third half catches it).
func TestTheDeviceAxisMovesOnlyWhenBothTheWorkloadAndTheObserverSayItDid(t *testing.T) {
	gpu := workloadKinds[queuelab.KindCUDAFMA]
	gpu.DeviceStatus = queuelab.DeviceOK
	cpu := workloadKinds[queuelab.KindCPUFloat]
	cpu.DeviceStatus = "no-libcuda"

	// No observer: the workload's word alone is not evidence about the card. It is the party with the motive.
	none := workloadFrom(nil, queuelab.DeviceClaim{}, gpu)
	if none.DeviceUseEstablished {
		t.Fatal("a run with no observer established device work on the workload's say-so")
	}
	if !strings.Contains(none.WhyNot, "RESERVED") {
		t.Fatalf("the reason does not say what the run DOES establish: %s", none.WhyNot)
	}
	if deviceEvidenceOf(&measurement{Workload: none}) != deviceNotObserved {
		t.Fatal("the axis did not follow the provenance")
	}

	// Both agree: this is the one combination that publishes device work, and the session depends on it.
	obs := busyObservation("GPU-1234")
	seen := workloadFrom(obs, queuelab.SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000), gpu)
	if !seen.DeviceUseEstablished {
		t.Fatalf("a launched kernel watched by an admissible observer established nothing: %s", seen.WhyNot)
	}
	if seen.WhyNot != "" {
		t.Fatalf("an established run carries a refusal reason: %s", seen.WhyNot)
	}
	if seen.Kind != "cuda-driver-fma-kernel" {
		t.Fatalf("the record published kind %q for a device run", seen.Kind)
	}
	if deviceEvidenceOf(&measurement{Workload: seen}) == deviceNotObserved {
		t.Fatal("the axis stayed put on a run where the card demonstrably worked")
	}

	// Same observation, CPU report: the observer is consulted and then overruled by the container.
	fell := workloadFrom(obs, queuelab.SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000), cpu)
	if fell.DeviceUseEstablished {
		t.Fatal("a card reported busy while the Pod holding it never reached a driver call was credited to it")
	}
	if strings.Contains(fell.WhyNot, "no device observer ran") {
		t.Fatalf("a well-formed observation was never consulted: %s", fell.WhyNot)
	}
}

// The contradiction, which is the shape a fake or misconfigured exporter produces -- and the shape an
// UNLABELLED intruder produces, which nothing else here can catch.
//
// It was found by pointing the real path at a fake exporter reporting 94% for every device-holding Pod: the
// axis moved, and the record said in one field that the workload was Python arithmetic and in another that
// its GPU had been working.
//
// The exclusivity clause in EstablishesDeviceWork convicts a SECOND Pod label on the same card. A host
// process outside Kubernetes wears no label at all, so that clause sees one tenant and waves it through --
// the review named this as an open false-accept. The workload's own report closes it: the only Pod on that
// card says it computed nothing, so the utilisation belongs to somebody the cluster cannot see.
//
// Mutation that turns this red: drop the reported-kind comparison, or let the observation win.
func TestAnObservationCannotContradictTheWorkloadsOwnReport(t *testing.T) {
	obs := busyObservation("GPU-fake-0000")
	// The observation itself is impeccable: admissible source, named build, one card, full coverage, busy, and
	// no second label anywhere for the exclusivity clause to convict.
	if ok, why := queuelab.EstablishesDeviceWork(obs, queuelab.SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000)); !ok {
		t.Fatalf("the fixture must satisfy the observation contract, or this tests the wrong thing: %s", why)
	}
	cpu := workloadKinds[queuelab.KindCPUFloat]
	cpu.DeviceStatus = "no-libcuda"
	w := workloadFrom(obs, queuelab.SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000), cpu)
	if w.DeviceUseEstablished {
		t.Fatal("a record certified device work for a workload that made no driver call; one field would say " +
			"pure Python arithmetic while another said its GPU had been working")
	}
	if !strings.Contains(w.WhyNot, "another process's activity") {
		t.Fatalf("the refusal blames the card rather than the intruder: %s", w.WhyNot)
	}
	if !strings.Contains(w.WhyNot, "no-libcuda") {
		t.Fatalf("the refusal does not name which driver call refused, which is where it sends an operator: %s",
			w.WhyNot)
	}
	if deviceEvidenceOf(&measurement{Workload: w}) != deviceNotObserved {
		t.Fatal("the axis moved on a contradicted observation")
	}

	// A ledger that never reported at all is refused too, and NOT by falling back to the CPU kind: that would
	// let a genuine device run be published under the fallback's name.
	un := workloadFrom(obs, queuelab.SameWindowClaim("victim-uid", 10_000_000_000, 30_000_000_000),
		reportedWorkload{Kind: unreportedWorkloadKind, Unit: "unreported"})
	if un.DeviceUseEstablished {
		t.Fatal("a run whose workload said nothing about itself established device work")
	}
	if un.Kind == cpuOnlyWorkloadKind {
		t.Fatal("an unreported run was published as the CPU fallback, which is a guess wearing a measurement")
	}
}

// The provenance is read from the VICTIM's attempt, and reading any stopped container instead is a defect
// that survives every other test in this file.
//
// The device claim is about the card the victim was holding while its owner waited. Every row in the trace
// stops, and they do not have to agree: an owner that reached the driver beside a victim that did not is
// exactly the shape a partial device passthrough produces on a two-device node, and it is a condition someone
// needs to be told about rather than averaged away.
//
// The in-quota row stops FIRST here, which is what makes the fixture bite. A derivation that took the first
// stopped container it found would read a1's report, publish "the device worked", and attribute it to a
// victim that never left the CPU.
//
// Mutation that turns this red: drop the ObjectUID comparison from reportedWorkloadOf.
func TestTheProvenanceComesFromTheVictimsAttemptAndNotAnyStop(t *testing.T) {
	stopped := func(job, uid, kind, dev string, at int64) queuelab.LifecycleEvent {
		return queuelab.LifecycleEvent{
			Job: job, ObjectUID: uid, Type: queuelab.EventAttemptStopped, ElapsedNs: at,
			WorkloadKind: kind, DeviceStatus: dev,
		}
	}
	events := []queuelab.LifecycleEvent{
		stopped("a1-inquota", "a1-uid", queuelab.KindCUDAFMA, queuelab.DeviceOK, 5_000_000_000),
		stopped(queuelab.VictimRow, "victim-uid", queuelab.KindCPUFloat, "no-libcuda", 30_000_000_000),
		stopped(queuelab.OwnerRow, "owner-uid", queuelab.KindCUDAFMA, queuelab.DeviceOK, 90_000_000_000),
	}
	if uid := queuelab.VictimAttemptUID(events); uid != "victim-uid" {
		t.Fatalf("this fixture only tests anything while the victim attempt is %q, got %q", "victim-uid", uid)
	}
	got := reportedWorkloadOf(events)
	if got.Token != queuelab.KindCPUFloat {
		t.Fatalf("the provenance came from another row's container: token %q, want %q",
			got.Token, queuelab.KindCPUFloat)
	}

	// A re-executed victim row has several attempts, and the later ones ran after the owner already had its
	// capacity back. The one that ENDS the hold is the earliest, and it is the only one the claim is about.
	reexecuted := append([]queuelab.LifecycleEvent{}, events...)
	reexecuted = append(reexecuted,
		stopped(queuelab.VictimRow, "victim-retry-uid", queuelab.KindCUDAFMA, queuelab.DeviceOK, 60_000_000_000))
	if got := reportedWorkloadOf(reexecuted); got.Token != queuelab.KindCPUFloat {
		t.Fatalf("a later attempt of the victim row, which ran after the hold ended, supplied the provenance: "+
			"token %q", got.Token)
	}
}

// A ledger this build cannot read yields "unreported", and NOT the CPU fallback.
//
// The difference matters in one direction only, and it is the expensive one: a genuine device run whose
// termination message was garbled -- a two-container Pod, a truncated log, a workload from a newer build --
// would be published under the fallback's name, as arithmetic that makes no driver call. Every downstream
// reader, including the decode-time guard that refuses device work paired with that kind, would then be
// acting on a guess the record presents as the workload's own account.
//
// Mutation that turns this red: return the CPU fallback when the kind token is unknown.
func TestALedgerThatNamesNoKnownWorkloadIsUnreportedRatherThanAssumed(t *testing.T) {
	for _, tc := range []struct{ name, kind, dev string }{
		// What an unparseable termination message actually leaves behind: the parser refuses the message
		// whole, so nothing reaches the ledger at all.
		{"a message this build could not parse", "", ""},
		// And what a record from a build with a third workload looks like to this one.
		{"a kind from a build this one does not know", "cuda-graph-replay", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reportedWorkloadOf([]queuelab.LifecycleEvent{{
				Job: queuelab.VictimRow, ObjectUID: "victim-uid", Type: queuelab.EventAttemptStopped,
				WorkloadKind: tc.kind, DeviceStatus: tc.dev,
			}})
			if got.Kind != unreportedWorkloadKind {
				t.Fatalf("an unreadable report was published as %q, which is a guess wearing a measurement",
					got.Kind)
			}
			if got.Token == queuelab.KindCPUFloat || got.Token == queuelab.KindCUDAFMA {
				t.Fatalf("an unreadable report resolved to the known token %q", got.Token)
			}
		})
	}
}

// committedRecord loads a record this build's write path actually produced.
//
// Decode tests used to hand-build their fixtures, and that stopped being possible when the decoder began
// re-deriving the measurement from the ledger: a record assembled field by field carries numbers no ledger
// supports, which is precisely what the decoder now refuses. Basing them on a real document is not a
// workaround for that -- it is the same lesson the fake exporter and the stand-in exporter both taught here,
// that a fixture built to fit the code confirms only that the code agrees with itself.
func committedRecord(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "ex", "e17-self-completing-A-honor-e17sh1.json"))
	if err != nil {
		t.Fatalf("no committed record to build on, so this test verifies nothing: %v", err)
	}
	return b
}

// committedRecordDoc is committedRecord as a mutable document.
func committedRecordDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(committedRecord(t), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc["runID"] = "forged"
	return doc
}

// The headline numbers must follow from the ledger in the same document.
//
// This is the check the citation test says in its own comment it cannot make. That test proves a cited
// record exists and decodes; it cannot prove a figure in a published table came from it. So every number in
// every document this lab has produced entered through a channel where the only thing standing between a
// hand edit and a comparison document was that nobody had tried.
//
// Reproduced before it was fixed: a copy of a committed record with wastedGPUSeconds set to 999.999 and its
// events untouched produced a comparison reporting a thousand discarded GPU-seconds against a six-second
// floor, with the ledger beside it saying 40.9.
//
// The fixture is a REAL record rather than a synthesised one, because the replay must agree with what the
// write path actually produces. A hand-built record would let this test pass while every honest document
// from the field was refused -- which is exactly what happened on the first attempt, where the resolution
// block was compared by pointer and every real record failed at once.
//
// Mutation that turns this red: skip any field in replayAgreesWithRecord's table, or return nil early.
func TestTheHeadlineNumbersAreReDerivedFromTheRecordsOwnLedger(t *testing.T) {
	// Repo-relative, like the citation test's globs: the test binary runs in the package directory and the
	// evidence lives at the root. A skip here would be worthless -- the whole point is to replay a document
	// the write path actually produced.
	real, err := os.ReadFile(filepath.Join("..", "..", "ex", "e17-self-completing-A-honor-e17sh1.json"))
	if err != nil {
		t.Fatalf("no committed record to replay, so this check verifies nothing: %v", err)
	}
	if _, err := decodeRunRecord(real); err != nil {
		t.Fatalf("a record this build wrote does not survive its own replay, so the check refuses honest "+
			"documents rather than forged ones: %v", err)
	}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]any)
		wants string
	}{
		{"discarded GPU-seconds", func(m map[string]any) { m["wastedGPUSeconds"] = 999.999 },
			"wastedGPUSeconds"},
		{"the lower bound", func(m map[string]any) { m["wasteLowerBoundGPUSeconds"] = 1.0 },
			"wasteLowerBoundGPUSeconds"},
		{"the owner's wait", func(m map[string]any) { m["ownerAdmitToReadyNs"] = 1 },
			"ownerAdmitToReadyNs"},
		{"the component-stamp wait", func(m map[string]any) { m["ownerAdmitToReadyStampNs"] = 1 },
			"ownerAdmitToReadyStampNs"},
		{"the discarded work", func(m map[string]any) { m["discardedIterations"] = 1 },
			"discardedIterations"},
		{"the censoring flag", func(m map[string]any) { m["censored"] = true }, "censored"},
		{"what was still running", func(m map[string]any) { m["unfinishedAtHorizon"] = 7 },
			"unfinishedAtHorizon"},
		// The bound every other figure is judged against. Widening it is the edit that makes a difference
		// look unresolved, and narrowing it is the edit that makes noise look like a finding.
		{"the resolution", func(m map[string]any) {
			m["resolution"].(map[string]any)["resolvedToNs"] = 1
		}, "resolution"},
		{"the workload's own account", func(m map[string]any) {
			m["workload"].(map[string]any)["countedUnit"] = "furlongs"
		}, "workload.countedUnit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(real, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			doc["runID"] = "forged"
			tc.edit(doc["measurement"].(map[string]any))
			b, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			_, err = decodeRunRecord(b)
			if err == nil {
				t.Fatalf("a record whose %s does not follow from its own ledger decoded, so it would reach a "+
					"published table with nothing having checked it", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("the refusal does not name the field that disagrees (%q): %v", tc.wants, err)
			}
		})
	}

	// A measurement with no ledger cannot be re-derived at all, and that pairing is refused rather than
	// waved through: it is what a forger produces after learning that the events are what convict them.
	var doc map[string]any
	if err := json.Unmarshal(real, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc["runID"] = "forged"
	delete(doc, "events")
	b, _ := json.Marshal(doc)
	_, err = decodeRunRecord(b)
	if err == nil {
		t.Fatal("a record carrying a measurement and no ledger decoded, so stripping the evidence is a way " +
			"to keep the number")
	}
	// Refused on the NUMBER, not on the pairing. An earlier version of the decoder refused a measurement
	// with no events outright, claiming no build writes one -- and two other tests in this file build exactly
	// that. The claim was unsupported, so it is gone: stripping the ledger from a real record now fails
	// because forty discarded GPU-seconds do not follow from no events, which is a fact about this document
	// rather than an assertion about what builds do.
	if !strings.Contains(err.Error(), "no ledger") {
		t.Fatalf("a record with its ledger stripped was refused for some other reason, which sends a reader "+
			"to a number when the fact is that the evidence is gone: %v", err)
	}

	// Absent and zero must not compare equal, which is the dangerous-zero pattern this file exists to keep
	// out: a document saying 0 for the owner's wait claims the quota owner was restored INSTANTLY -- the
	// experiment's best possible outcome -- on exactly the runs where it was never restored at all.
	//
	// This fixture does NOT reach the comparison that would prove it, and saying so is worth more than a
	// green line. Reconstruct refuses a ledger carrying a stop with no readiness before it, so removing the
	// owner's PodReady fails the replay outright rather than producing a nil wait to compare against a
	// zero. The sentinel in intPtrValue is therefore defence behind a guard that fires first, and a mutation
	// flattening absent to 0 survives this suite. It is kept because the ordering is a property of today's
	// Reconstruct rather than of the record format, and the field it protects is the one where a zero is
	// the most flattering possible lie.
	if err := json.Unmarshal(real, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc["runID"] = "forged"
	doc["measurement"].(map[string]any)["ownerAdmitToReadyNs"] = 0
	kept := []any{}
	for _, e := range doc["events"].([]any) {
		ev := e.(map[string]any)
		if ev["job"] == queuelab.OwnerRow && ev["type"] == string(queuelab.EventPodReady) {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == len(doc["events"].([]any)) {
		t.Fatal("the fixture removed no owner-readiness event, so the replay would still produce a wait and " +
			"this asserts nothing")
	}
	doc["events"] = kept
	b, _ = json.Marshal(doc)
	_, err = decodeRunRecord(b)
	if err == nil {
		t.Fatal("a record claiming the quota owner was running 0 ns after admission decoded beside a ledger " +
			"that never saw it become ready, which is the experiment's best outcome asserted on a run where " +
			"it did not happen")
	}
	if !strings.Contains(err.Error(), "cannot be replayed") {
		t.Fatalf("the refusal came from somewhere other than the replay, so this fixture no longer tests "+
			"what its comment says: %v", err)
	}
}

// The device evidence must answer the LEDGER's question, not one of its own.
//
// The block records which Pod and which window the gate was asked about, and the decoder re-runs the gate
// against those recorded values. A review pointed out that nothing tied them to the ledger: a document could
// widen its own hold window, or name a Pod holding a different card, and pass — while the ledger replay
// beside it independently validated the real ones. Both checks succeeding on a record where they are
// checking different things is worse than either check being absent, because it reads as corroboration.
//
// This is also the first record in this repository, real or synthetic, that claims a device did any work.
// Every committed one carries deviceObservation: null, so the positive path had never been decoded. It is
// built from a committed record rather than from nothing, so the parts not under test — the ledger, the
// replayed measurement, the resolution block — are what the write path actually produces.
//
// Mutation that turns this red: drop the comparison binding the block to the ledger.
func TestTheDeviceEvidenceMustAnswerTheLedgersQuestion(t *testing.T) {
	// An IGNORING record, because the honouring arm's hold is 29 ms and a fixture cannot put two scrapes
	// inside it. That is the gate working -- minBusySamples wants two distinct instants -- and it is also a
	// fact worth having in a test: the arm this lab calls its zero-hold control is too short for an observer
	// running at any realistic interval to say anything about.
	raw, err := os.ReadFile(filepath.Join("..", "..", "ex", "e17-grace-bounded-A-ignore-e17gi1.json"))
	if err != nil {
		t.Fatalf("no committed record with a long hold to build on: %v", err)
	}
	base, err := decodeRunRecord(raw)
	if err != nil {
		t.Fatalf("the committed record must decode before it can be built on: %v", err)
	}
	from, to, ok := queuelab.DeviceHoldWindow(base.Events)
	if !ok {
		t.Fatal("the committed record's ledger cannot locate its hold, so this fixture cannot be built")
	}
	uid := queuelab.VictimAttemptUID(base.Events)
	workFrom, workTo, wok := queuelab.VictimWorkWindow(base.Events)
	if !wok {
		t.Fatal("the committed record's ledger cannot locate the victim's attempt, so the work window this " +
			"fixture must answer cannot be built")
	}

	// A document with a device path: the ledger's victim reports launching kernels, and an observer covering
	// the hold saw its card busy. Everything else is the committed run's.
	deviceDoc := func() map[string]any {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		doc["runID"] = "forged"
		// The forgery is a schema-19 document. Its predecessor asked the busyness question over the hold, and
		// a record carrying an observation under that question is refused rather than reinterpreted.
		doc["schemaVersion"] = recordSchemaVersion
		for _, e := range doc["events"].([]any) {
			ev := e.(map[string]any)
			if ev["type"] == string(queuelab.EventAttemptStopped) && ev["objectUID"] == uid {
				ev["workloadKind"] = queuelab.KindCUDAFMA
				ev["deviceStatus"] = queuelab.DeviceOK
			}
		}
		w := doc["measurement"].(map[string]any)["workload"].(map[string]any)
		w["kind"] = "cuda-driver-fma-kernel"
		w["countedUnit"] = "1024x256 threads x 20000 fused multiply-adds per launch"
		w["deviceUseEstablished"] = true
		delete(w, "whyNot")
		doc["validity"].(map[string]any)["deviceEvidence"] = deviceWorkObserved
		return doc
	}
	// Samples covering the window the gate reads: the hold, widened by the stale-label margin, one card,
	// one tenant, busy throughout.
	samples := func() []any {
		var out []any
		lo := min(workFrom-int64(queuelab.StaleLabelMargin), from-int64(queuelab.StaleLabelMargin))
		for at := lo; at <= to; at += int64(time.Second) {
			out = append(out, map[string]any{
				"atNs": at, "deviceUUID": "GPU-0000", "podUID": uid, "utilisationPercent": 93,
			})
		}
		return out
	}
	evidence := func(podUID string, holdFrom, holdTo int64) map[string]any {
		return map[string]any{
			"observer": string(queuelab.ObserverDCGM), "identity": "dcgm@sha256:abc",
			"endpoint": "http://127.0.0.1:9400/metrics", "declared": true,
			"startedNs": from - 2*int64(queuelab.StaleLabelMargin), "endedNs": to + int64(time.Second),
			"holdFromNs": holdFrom, "holdToNs": holdTo,
			// The attempt's interval is part of the question now, and the decode check binds it the same way
			// it binds the hold: evidence that answered a different attempt is evidence about another run.
			"workFromNs": workFrom, "workToNs": workTo,
			"podUID": podUID, "samples": samples(),
		}
	}
	decode := func(doc map[string]any) error {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = decodeRunRecord(b)
		return err
	}

	// The honest document decodes. Without this half the test could not tell a working guard from a decoder
	// that refuses every device claim — which is the shape a session would meet on its first success.
	honest := deviceDoc()
	honest["deviceObservation"] = evidence(uid, from, to)
	if err := decode(honest); err != nil {
		t.Fatalf("a record whose evidence answers its ledger's own question was refused, so the first "+
			"successful GPU run would be rejected by its own decoder: %v", err)
	}

	for _, tc := range []struct {
		name string
		doc  map[string]any
	}{
		{"a window widened past the hold", func() map[string]any {
			d := deviceDoc()
			d["deviceObservation"] = evidence(uid, from-5*int64(time.Second), to+5*int64(time.Second))
			return d
		}()},
		{"a window narrowed inside the hold", func() map[string]any {
			d := deviceDoc()
			d["deviceObservation"] = evidence(uid, from+int64(time.Second), to-int64(time.Second))
			return d
		}()},
		// The sharpest one: the gate is satisfied, on another Pod's card. Replay validates the real victim,
		// the evidence check validates a stranger, and neither notices.
		{"another Pod's card entirely", func() map[string]any {
			d := deviceDoc()
			d["deviceObservation"] = evidence("some-other-pod-uid", from, to)
			return d
		}()},
		// The attempt window is the new half of the question, and it has to be bound as tightly as the hold.
		// Evidence that judged a different attempt is evidence about a different run, and before the split
		// there was no field here for it to disagree in.
		{"an attempt window that is not this ledger's", func() map[string]any {
			d := deviceDoc()
			e := evidence(uid, from, to)
			e["workFromNs"] = workFrom - 7*int64(time.Second)
			d["deviceObservation"] = e
			return d
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := decode(tc.doc)
			if err == nil {
				t.Fatal("the evidence answered a different question from the measurement and the record " +
					"decoded, which reads as two independent checks corroborating each other")
			}
			if !strings.Contains(err.Error(), "different question") {
				t.Fatalf("the refusal does not say that the evidence and the ledger disagree about what was "+
					"judged: %v", err)
			}
		})
	}
}

// A row that was preempted, re-executed and then finished discarded the preempted attempt's work.
//
// The filter used to credit the whole ROW on seeing a Completed event and exclude every attempt belonging to
// it, so exactly the case this lab exists to measure — work thrown away by a preemption — reported zero.
// The number would have been published as "no work was lost" on a run where a full attempt was.
//
// It is invisible in every committed record because the horizon ends before a re-executed row completes.
// That is a property of a 60-second victim on a quiet kind cluster, not of the measurement, and it would
// stop holding on hardware where runs are longer and contention makes re-execution ordinary.
//
// Mutation that turns this red: credit by job again, or credit the FIRST stop instead of the last.
func TestAReExecutedRowStillDiscardsTheAttemptItLost(t *testing.T) {
	stop := func(job, uid string, at int64, iters int, reason string) queuelab.LifecycleEvent {
		n := iters
		return queuelab.LifecycleEvent{
			Job: job, ObjectUID: uid, Type: queuelab.EventAttemptStopped,
			ElapsedNs: at, Iterations: &n, Reason: reason,
		}
	}
	completed := func(job string, at int64) queuelab.LifecycleEvent {
		return queuelab.LifecycleEvent{Job: job, Type: queuelab.EventCompleted, ElapsedNs: at}
	}

	// The preempted attempt's 900 iterations are gone; the 1200 that finished the row are not.
	got := discardedIterations([]queuelab.LifecycleEvent{
		stop(queuelab.VictimRow, "first", 10_000_000_000, 900, "Failed"),
		stop(queuelab.VictimRow, "second", 50_000_000_000, 1200, "Succeeded"),
		completed(queuelab.VictimRow, 51_000_000_000),
	})
	if got == nil {
		t.Fatal("a ledger carrying two stopped attempts reported no count at all")
	}
	if *got != 900 {
		t.Fatalf("discarded = %d, want 900: the re-executed row's first attempt lost its work and the "+
			"second's was credited", *got)
	}

	// The two streams can be delivered in either order, so the credit must not depend on Completed arriving
	// after the attempt's terminal phase. Keying on "the last stop at or before Completed" would credit
	// nothing here and discard the successful attempt's work as well.
	got = discardedIterations([]queuelab.LifecycleEvent{
		stop(queuelab.VictimRow, "first", 10_000_000_000, 900, "Failed"),
		completed(queuelab.VictimRow, 49_000_000_000),
		stop(queuelab.VictimRow, "second", 50_000_000_000, 1200, "Succeeded"),
	})
	if got == nil || *got != 900 {
		t.Fatalf("discarded = %v with Completed delivered before the attempt's stop; the credit must not "+
			"depend on the order two watches deliver in", got)
	}

	// The shapes that must not move. A row that never completed discards everything, including a container
	// that exited zero — the self-completing victim does exactly that and is re-executed from zero anyway.
	got = discardedIterations([]queuelab.LifecycleEvent{
		stop(queuelab.VictimRow, "only", 10_000_000_000, 900, "Succeeded"),
	})
	if got == nil || *got != 900 {
		t.Fatalf("discarded = %v for a row that never completed; a zero exit is not a credit, which is the "+
			"whole reason this reads the ledger rather than the exit code", got)
	}
	got = discardedIterations([]queuelab.LifecycleEvent{
		stop(queuelab.OwnerRow, "only", 90_000_000_000, 25973, "Succeeded"),
		completed(queuelab.OwnerRow, 91_000_000_000),
	})
	if got == nil || *got != 0 {
		t.Fatalf("discarded = %v for a row that ran straight through; counting its work as thrown away is "+
			"the defect this filter was added for", got)
	}
}
