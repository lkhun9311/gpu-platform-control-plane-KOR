package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every printed queuelabrun transcript must be a subset of what queuelabrun actually prints.
//
// This is the third recurrence of one failure. Numbers typed onto a page drift from the records they were
// taken from, the records get replaced, and the page keeps quoting a set that no longer exists -- while the
// page ITSELF narrates having fixed exactly that. A review recomputing every figure by hand found eleven
// live instances, a command that could not run as printed, and a transcript quoting wording this build
// cannot emit.
//
// The citation test that already guards these pages says in its own comment that it cannot check whether a
// number in a table came from the records it cites. This checks the part it can: a block presented as
// output must consist of lines the tool really produces, in order, from the command printed above it.
//
// Abridgement is allowed -- a page may drop lines from a long output. What is not allowed is a line the tool
// does not print: a stale number, an edited phrase, an invented sentence, or a command that fails. Every one
// of the defects that prompted this test is in that class.
//
// Prose figures outside transcripts are still unguarded, and no test here can fix that. What this does is
// make the transcripts trustworthy enough that prose has somewhere to be checked against.
func TestEveryPrintedTranscriptMatchesWhatTheToolPrints(t *testing.T) {
	bin := buildRunner(t)
	docs, err := filepath.Glob(filepath.Join("..", "..", "hack", "*.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// The command line as a page prints it, indented under a $ prompt.
	prompt := regexp.MustCompile(`(?m)^\s*\$ queuelabrun (.+)$`)
	checked := 0
	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			m := prompt.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := filepath.Base(doc)
			args, err := splitCommand(m[1])
			if err != nil {
				t.Errorf("%s:%d prints a command this shell could not parse (%v): %s", name, i+1, err, m[1])
				continue
			}
			// A run mode needs a cluster; only the offline modes are reproducible from a checkout, and they
			// are the ones that carry figures.
			if !hasFlag(args, "-compare") {
				continue
			}
			out, runErr := runRunner(t, bin, args)
			if runErr != nil {
				t.Errorf("%s:%d prints a command that does not run as printed (%v):\n    %s\n%s",
					name, i+1, runErr, m[1], out)
				continue
			}
			checked++
			assertTranscriptSubset(t, name, i+1, transcriptAfter(lines, i), out)
		}
	}
	if checked == 0 {
		t.Fatal("no transcripts were checked, so this test passes by finding nothing; the shape it looks " +
			"for has moved and it must be rewritten rather than left green")
	}
	t.Logf("checked %d transcript(s) against the tool", checked)
}

// buildRunner compiles the tool once, so the pages are checked against this tree rather than against a
// binary somebody left on the path.
func buildRunner(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "queuelabrun")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build the runner: %v\n%s", err, out)
	}
	return bin
}

// runRunner executes the tool from the repository root, which is where a page's relative globs resolve.
func runRunner(t *testing.T, bin string, args []string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func hasFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

// transcriptAfter collects the indented block a page prints beneath its command.
func transcriptAfter(lines []string, cmdLine int) []string {
	var block []string
	for _, l := range lines[cmdLine+1:] {
		if strings.TrimSpace(l) == "" {
			break
		}
		if !strings.HasPrefix(l, "    ") {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(l), "$ ") {
			break
		}
		block = append(block, l)
	}
	return block
}

// assertTranscriptSubset requires every printed line to appear in the tool's output, in order.
//
// Line by line, with whitespace collapsed. A page wraps output to its own column width, so one output
// sentence becomes several page lines -- but each of those lines is still a CONTIGUOUS fragment of what the
// tool printed, so each must appear in the collapsed output. A stale number, an edited phrase or an
// invented sentence is not a fragment of anything.
//
// The search cursor only moves forward, so a page cannot reorder the tool's output either. Abridgement is
// what is deliberately allowed: dropping lines from a long block is a presentation choice, and every defect
// this test exists for is an addition or an alteration rather than an omission.
func assertTranscriptSubset(t *testing.T, doc string, line int, block []string, out string) {
	t.Helper()
	flat := collapse(out)
	cursor := 0
	for n, raw := range block {
		frag := collapse(raw)
		if frag == "" {
			continue
		}
		at := strings.Index(flat[cursor:], frag)
		if at < 0 {
			if strings.Contains(flat, frag) {
				t.Errorf("%s:%d prints the tool's output out of order at block line %d:\n  page: %s",
					doc, line, n+1, frag)
				return
			}
			t.Errorf("%s:%d prints a transcript line this build does not produce, at block line %d:\n"+
				"  page: %s\nA block under a $ prompt is a claim about what the tool said.",
				doc, line, n+1, frag)
			return
		}
		cursor += at + len(frag)
	}
}

// collapse normalises whitespace so page wrapping is not compared as content.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// splitCommand parses a printed command line the way a shell would, honouring single quotes.
//
// It refuses a backslash line-continuation inside quotes, which is not a parse failure in a shell -- it is
// worse. The shell passes the backslash and the newline through as part of the quoted string, the glob
// matches nothing, and the tool refuses with a message about files. One page shipped that for a week.
func splitCommand(s string) ([]string, error) {
	if strings.HasSuffix(strings.TrimRight(s, " "), `\`) {
		return nil, errContinuedInsideQuotes
	}
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		return nil, errUnbalancedQuote
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args, nil
}

var (
	errContinuedInsideQuotes = errors.New("the command ends in a backslash continuation; inside single " +
		"quotes a shell passes it through and the glob matches nothing")
	errUnbalancedQuote = errors.New("unbalanced single quote")
)
