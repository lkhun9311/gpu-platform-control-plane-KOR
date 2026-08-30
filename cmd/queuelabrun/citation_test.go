package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Committed documents must not cite evidence that is not there, and nothing but this test makes them.
//
// It exists because the failure happened twice in one day and both times through the same hole. A
// pre-registration fixed the GPU session's baseline at a figure assembled from four records that were then
// deleted; the number appears in no record on disk. The result page quoted eight runs by name from a set
// that had been replaced. Both documents also told the reader to run `queuelabrun -compare` against globs
// matching nothing.
//
// Neither was a lie and neither was caught by review, because both were true when written. That is the
// property that makes this a test rather than a habit: a document and the evidence under it drift apart
// through ordinary work — a re-run, a schema bump, a cleanup — and nobody re-reads the tables afterwards.
//
// It checks the two things a reader can actually follow: the evidence paths a document reads from, and the
// run identifiers it quotes. It cannot check that a NUMBER in a table came from the record beside it, which
// is what -mode baseline is for -- the two halves close the same hole from opposite ends.
func TestCommittedDocumentsCiteEvidenceThatExists(t *testing.T) {
	// Every hack document, not only the ones with queuelab in the name. The patterns below are narrow enough
	// that a document mentioning none of this lab's evidence contributes nothing, and scoping by filename
	// would mean the next page to quote a run is outside the check by default.
	docs, err := filepath.Glob(filepath.Join("..", "..", "hack", "*.md"))
	if err != nil {
		t.Fatalf("glob hack: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no hack documents found; this test would pass by describing nothing")
	}

	known := knownRunIDs(t)
	// Both patterns are deliberately narrow, and the narrowness is the design rather than a compromise.
	//
	// A document contains two kinds of path, and only one of them is a claim. `-out ./ex/h1.json` in a
	// reproduction block names a file the READER will create; `-compare 'ex/e15-*.json'` names evidence the
	// AUTHOR is standing on. Demanding that the first exist would flag every instruction in the repository
	// and the check would be deleted within a week for crying wolf.
	//
	// So both patterns match only this lab's evidence-naming convention -- a run record is `eNN<tag>N` and
	// its file is `ex/eNN-...json` -- and placeholders like h1, i1 or preview.json fall outside it by
	// construction. That is also why the identifier pattern requires the two-digit schema generation: it is
	// what makes a real run identifier unmistakable in prose.
	runID := regexp.MustCompile(`\be\d{2}[a-z]{2,3}\d\b`)
	exPath := regexp.MustCompile(`ex/e\d{2}-[A-Za-z0-9_*?\[\]-]*\.json`)

	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		text := string(b)
		name := filepath.Base(doc)

		for _, cited := range dedupe(exPath.FindAllString(text, -1)) {
			matches, err := filepath.Glob(filepath.Join("..", "..", cited))
			if err != nil {
				t.Fatalf("%s: bad pattern %q: %v", name, cited, err)
			}
			if len(matches) == 0 {
				t.Errorf("%s tells a reader to run against %q, which matches no file; a document whose "+
					"commands cannot be run is prose wearing a transcript", name, cited)
			}
		}

		// A document that publishes this lab's figures must cite at least one record, or it is outside both
		// checks above by simply not mentioning them. That is not hypothetical: hack/queuelab-grace-boundary.md
		// sat five schema versions stale at HEAD, publishing a nine-run table whose numbers no record produces,
		// and passed every test here because it named no path and quoted no run identifier. The guard was
		// opt-in by the document it guarded.
		//
		// The arm names are the detector because they are this lab's own vocabulary: a page that discusses
		// A-honor and A-ignore is reporting these experiments, whatever else it does.
		if strings.Contains(text, "A-honor") && strings.Contains(text, "A-ignore") &&
			len(exPath.FindAllString(text, -1)) == 0 {
			t.Errorf("%s reports this lab's arms and cites no record under ex/. A page with figures and no "+
				"provenance is outside every check here by construction, which is how one of them stayed five "+
				"schema versions stale at HEAD", name)
		}

		for _, id := range dedupe(runID.FindAllString(text, -1)) {
			if !known[id] {
				t.Errorf("%s quotes run %q, which is in no record under ex/. Either the run was deleted "+
					"after the document was written, or the figures beside it came from somewhere nobody "+
					"can check", name, id)
			}
		}
	}
}

// knownRunIDs reads every run identifier the committed records carry.
func knownRunIDs(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "ex", "*.json"))
	if err != nil {
		t.Fatalf("glob ex: %v", err)
	}
	ids := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// Decoded loosely on purpose: this test is about whether the identifier is present, and it must keep
		// working across the schema bumps that are precisely when documents go stale.
		var probe struct {
			RunID string `json:"runID"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		if probe.RunID != "" {
			ids[probe.RunID] = true
		}
	}
	if len(ids) == 0 {
		t.Fatal("no run identifiers found under ex/; every citation would fail and the message would be wrong")
	}
	return ids
}

// dedupe returns the distinct strings, sorted, so a repeated citation is reported once.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Every run record in ex/ must decode under THIS build's rules, not merely exist.
//
// The citation test above closes half the hole and this closes the other half, which was found the way these
// things are found: by re-creating the defect an hour after fixing it. The committed documents cited records
// that were present, so that test passed, while a schema bump in the same session had made every one of them
// undecodable — `-compare` refused the entire evidence set and the figures in the documents could not be
// re-derived by anyone, which is the exact state the pre-registration was corrected out of.
//
// Existence was never the property that mattered. What a reader needs is that the commands in the documents
// RUN, and a record the current binary refuses is worth no more to them than a missing file. The schema guard
// is right and stays; what this adds is that the guard cannot fire against the repository's own evidence
// without someone noticing.
//
// The consequence is deliberate and is the point: a schema bump now breaks the build until the evidence is
// re-taken. That is the coupling that was missing. Six bumps in one day each silently orphaned the previous
// run, and nothing failed.
//
// Only run records are checked. The derived documents beside them -- comparisons, baselines, model checks --
// are a different shape and decodeRunRecord would rightly refuse them; they are re-derivable from the records
// in one command, which is the whole reason they are derived.
func TestEveryRunRecordDecodesUnderThisBuild(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "ex", "e[0-9][0-9]-*.json"))
	if err != nil {
		t.Fatalf("glob ex: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no run records under ex/; the documents in hack/ quote figures from records that must be there")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := decodeRunRecord(b); err != nil {
			t.Errorf("%s does not decode under this build: %v\n"+
				"    The committed documents quote figures from this record and tell readers to run -compare\n"+
				"    against it. Re-take the evidence at the current schema, or the repository ships numbers\n"+
				"    nobody can reproduce -- including whoever wrote them.", filepath.Base(f), err)
		}
	}
}

// No committed document may claim that anything in this repository SETS the victim's grace period.
//
// Two of them did, in opposite directions, and the one that mattered was the correction: a page whose whole
// purpose is scrupulousness said internal/queuelab/trace.go sets terminationGraceSec on the Pods. Nothing
// does. The only code touching TerminationGracePeriodSeconds outside tests is the termination canary's own
// probes and a read-only cap check in the admission webhook; trace.go's constant is a MIRROR, and says so in
// its own comment. The victim runs on the API server default.
//
// The arithmetic never depended on it -- the default equals the mirrored constant -- which is precisely why
// nothing caught it. The citation test that guards figures cannot guard mechanism prose, so this guards the
// one mechanism claim that has already been wrong twice.
func TestNoDocumentClaimsThisRepositorySetsTheGracePeriod(t *testing.T) {
	// Verify the premise before enforcing it: if a setter is ever added, this test must be rewritten rather
	// than kept, because then the documents would be allowed to say so.
	var setters []string
	for _, dir := range []string{"../../internal", "../../cmd", "../../api"} {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for line := range strings.SplitSeq(string(b), "\n") {
				// An assignment TO the field, not a read of it.
				if regexp.MustCompile(`\.TerminationGracePeriodSeconds\s*=`).MatchString(line) {
					setters = append(setters, path)
				}
			}
			return nil
		})
	}
	// The canary sets it on its own probes, which is not the victim's path.
	var victimSetters []string
	for _, s := range setters {
		if !strings.Contains(s, "canary") {
			victimSetters = append(victimSetters, s)
		}
	}
	if len(victimSetters) > 0 {
		t.Fatalf("something now sets the grace period outside the canary (%v); the documents may say so, and "+
			"this test must be rewritten rather than deleted", victimSetters)
	}

	claim := regexp.MustCompile(`(?i)(trace\.go|the protocol|this lab|the harness)[^.\n]{0,60}sets\s+` +
		`(the\s+)?(termination\s*)?grace`)
	docs, gerr := filepath.Glob(filepath.Join("..", "..", "hack", "*.md"))
	if gerr != nil || len(docs) == 0 {
		t.Fatalf("no committed markdown to check: %v", gerr)
	}
	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		if m := claim.FindString(string(b)); m != "" {
			t.Errorf("%s claims %q, and nothing in this repository sets the victim's grace period; the "+
				"victim runs on the API server default", filepath.Base(doc), strings.TrimSpace(m))
		}
	}
}
