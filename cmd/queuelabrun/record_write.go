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
	"fmt"
	"os"
	"path/filepath"
)

// writeRecord persists the record so a crash leaves either the previous state or a complete document.
//
// Temp-file-plus-rename makes the replacement atomic within the directory, and the directory fsync is what
// makes the rename itself durable: without it a crash can leave the rename unrecorded even though the file
// contents were flushed.
//
// A non-nil return does not always mean the previous record survived: if the failure is one of the two
// post-rename steps (opening the directory or syncing it), the rename has already happened and the new
// content is already at path, so callers must not treat this error as proof that nothing changed — only
// that the new record's durability against a crash is unproven.
func writeRecord(path string, v any) error {
	b, err := encodeRecord(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("create temp record in %s: %w", dir, err)
	}
	tmp := f.Name()
	// Any failure past this point must not leave the temporary file behind for a reader to find.
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temp record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename record into place: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open record directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync record directory: %w", err)
	}
	return nil
}

// verifyRecordReadable asks the one question nothing in this binary asked before: is the file this run just
// wrote a file this build can read?
//
// The record IS the deliverable — the run's numbers are quotable only because a reader holding the file can
// re-derive the verdict from the fields beside them — and until now the entire validation layer that decides
// whether a document is readable as evidence (the schema check, the verdict re-derivation, the qualification,
// canary, window and resume-point guards) was reachable only from tests. Every property it checks is enforced
// by the writers, which is why no record has been unreadable yet; but "the writer and the reader agree" is
// exactly the kind of claim that stays true until it quietly does not. This project has already paid for that
// once: residue's observation carried an `error`, which encodes and does not decode, and the defect was
// caught by a person reasoning during a design round rather than by the tool noticing the record it had just
// written could not be read. A write-then-read-back would have caught it for free, on the first run.
//
// It re-reads the file rather than checking the bytes it was about to write, because the bytes are not the
// artifact. A truncated write, a rename that landed somewhere else, a directory sync that failed after the
// content was already replaced — all leave a caller with err == nil and a file nobody can use, and only a
// read of the path proves what a later reader will actually find there.
//
// The preview arm inverts the question rather than skipping it, so that no record this build writes passes
// unexamined. previewRecord's whole shape is a promise that a preview cannot be read as a run — it carries
// eventCount and note, which runRecord has never heard of, so decodeRunRecord's DisallowUnknownFields refuses
// it structurally. A preview that DID decode as a run record would be a preview whose fields had drifted into
// a run's, which is the one failure the type was built to make impossible; asserting the refusal is what turns
// that structural promise into something an invocation checks.
func verifyRecordReadable(path string, preview bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back record: %w", err)
	}
	_, decErr := decodeRunRecord(b)
	if preview {
		if decErr == nil {
			return fmt.Errorf("read back record: the preview record at %s decodes as a run record; a preview's "+
				"fields have drifted into a run's, and its output is not evidence", path)
		}
		return nil
	}
	if decErr != nil {
		return fmt.Errorf("read back record: %w", decErr)
	}
	return nil
}
