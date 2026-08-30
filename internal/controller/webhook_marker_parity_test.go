package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// The deployed ValidatingWebhookConfiguration is hand-written and the kubebuilder markers generate a second
// one beside it, so the two can disagree with nothing failing -- the same defect shape this repository has
// now met seven times.
//
// The split itself is forced: controller-gen has no marker for a namespaceSelector, and the Pod and Job
// guards must not apply to kube-system. So the hand-written file is authoritative for what markers cannot
// say, and this test holds it to the markers for everything they can: which operations are admitted, on
// which resources and API groups, at which handler path, and under which failure policy.
//
// Mutation that turns this red: change a +kubebuilder:webhook marker in internal/webhook/v1 and re-run
// `make manifests` without carrying the change into config/webhook/manifests.yaml.
func TestHandWrittenWebhookMatchesTheMarkers(t *testing.T) {
	deployed := readWebhooks(t, filepath.Join("..", "..", "config", "webhook", "manifests.yaml"))
	generated := readWebhooks(t, filepath.Join("..", "..", "config", "webhook", "generated", "manifests.yaml"))

	if len(generated) == 0 {
		t.Fatal("the generated configuration declares no webhooks; this test would pass on an empty file")
	}

	for name, want := range generated {
		got, ok := deployed[name]
		if !ok {
			t.Errorf("the markers declare webhook %q and the deployed configuration does not; a guard that "+
				"exists in Go and not in the cluster refuses nothing", name)
			continue
		}
		// The Service name and namespace are deliberately different -- the deployed file spells them out in
		// full because no kustomize transformer reaches it -- so only the path is compared. The path is what
		// routes an admission request to a handler, and a mismatch there is a webhook the apiserver calls at
		// a URL nothing serves.
		if p, q := pathOf(want), pathOf(got); p != q {
			t.Errorf("webhook %q: the markers serve it at %q and the deployed configuration points at %q",
				name, p, q)
		}
		if !equalRules(want.Rules, got.Rules) {
			t.Errorf("webhook %q: the markers admit %v and the deployed configuration admits %v",
				name, renderRules(want.Rules), renderRules(got.Rules))
		}
		if policy(want.FailurePolicy) != policy(got.FailurePolicy) {
			t.Errorf("webhook %q: the markers set failurePolicy %s and the deployed configuration sets %s; "+
				"this is the difference between a guard that holds when the webhook is down and one that opens",
				name, policy(want.FailurePolicy), policy(got.FailurePolicy))
		}
	}

	// The deployed file may not carry a guard the markers do not know about, because a webhook with no marker
	// has no handler registered and the apiserver would call a path that 404s -- with failurePolicy: Fail,
	// that refuses every governed write in the cluster.
	for name := range deployed {
		if _, ok := generated[name]; !ok {
			t.Errorf("the deployed configuration declares webhook %q and no marker generates it; the "+
				"apiserver would call a path this build does not serve", name)
		}
	}
}

// readWebhooks parses a ValidatingWebhookConfiguration and returns its webhooks by name.
func readWebhooks(t *testing.T, path string) map[string]admissionv1.ValidatingWebhook {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg admissionv1.ValidatingWebhookConfiguration
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]admissionv1.ValidatingWebhook{}
	for _, w := range cfg.Webhooks {
		out[w.Name] = w
	}
	return out
}

func pathOf(w admissionv1.ValidatingWebhook) string {
	if w.ClientConfig.Service == nil || w.ClientConfig.Service.Path == nil {
		return ""
	}
	return *w.ClientConfig.Service.Path
}

func policy(p *admissionv1.FailurePolicyType) string {
	if p == nil {
		return "(unset)"
	}
	return string(*p)
}

// equalRules compares rules by content rather than by order, since neither file's order is meaningful.
func equalRules(a, b []admissionv1.RuleWithOperations) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, r := range a {
		seen[renderRule(r)]++
	}
	for _, r := range b {
		k := renderRule(r)
		if seen[k] == 0 {
			return false
		}
		seen[k]--
	}
	return true
}

func renderRule(r admissionv1.RuleWithOperations) string {
	var b strings.Builder
	for _, o := range r.Operations {
		b.WriteString(string(o) + ",")
	}
	b.WriteString("|")
	for _, g := range r.APIGroups {
		b.WriteString(g + ",")
	}
	b.WriteString("|")
	for _, v := range r.APIVersions {
		b.WriteString(v + ",")
	}
	b.WriteString("|")
	for _, res := range r.Resources {
		b.WriteString(res + ",")
	}
	return b.String()
}

func renderRules(rs []admissionv1.RuleWithOperations) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, renderRule(r))
	}
	return out
}
