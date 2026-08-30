package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// The controller writes the label in Go and the ValidatingWebhookConfiguration selects on it in YAML, and
// nothing else makes the two agree. A pair of values that must match with no check between them is the shape
// of the defect this whole change exists to close: the guard's coverage was correct in both files and
// connected by nothing but a person remembering.
//
// This reads the manifest rather than a copy of it, so a rename on either side is a failing test rather than
// a namespace that quietly stops being guarded.
//
// Mutation that turns this red: change either constant in api/v1/labels.go, or the selector in
// config/webhook/manifests.yaml.
func TestTheEnforcementLabelMatchesTheWebhookSelector(t *testing.T) {
	path := filepath.Join("..", "..", "config", "webhook", "manifests.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	manifest := string(b)

	want := platformv1.QuotaEnforcedLabel + ": " + quoted(platformv1.QuotaEnforcedValue)
	if !strings.Contains(manifest, want) {
		t.Fatalf("no webhook selects on %q with value %q; the controller would label namespaces nothing reads",
			platformv1.QuotaEnforcedLabel, platformv1.QuotaEnforcedValue)
	}

	// Both device guards must carry it, not just one. A selector present on the Pod guard alone would leave
	// the Job guard applying everywhere, and vice versa -- and the two were written to close halves of the
	// same bypass.
	//
	// Each entry is split at its own "- name:" line, which is where the entries actually begin. An earlier
	// version of this loop delimited on the wrong key, so Index returned -1, the slice ran to end-of-file,
	// and every hook was checked against every OTHER hook's selector. It passed while one guard had no
	// selector at all -- caught by mutating the manifest, which is the only reason this comment exists.
	entries := strings.Split(manifest, entryDelimiter)
	for _, hook := range []string{"vgpupod.kb.io", "vgpujob.kb.io"} {
		var entry string
		for _, e := range entries {
			if strings.HasPrefix(e, hook) {
				entry = e
				break
			}
		}
		if entry == "" {
			t.Fatalf("the manifest defines no webhook named %s", hook)
		}
		if !strings.Contains(entry, platformv1.QuotaEnforcedLabel) {
			t.Fatalf("%s does not scope on %s, so it applies to every namespace including kube-system",
				hook, platformv1.QuotaEnforcedLabel)
		}
	}
}

// quoted quotes the value the way the manifest writes it, so the containment check above cannot pass on a
// prefix -- "true" must not match a hypothetical "truent".
func quoted(v string) string { return "\"" + v + "\"" }

// entryDelimiter is where one webhook entry ends and the next begins in the rendered manifest.
const entryDelimiter = "  - name: "
