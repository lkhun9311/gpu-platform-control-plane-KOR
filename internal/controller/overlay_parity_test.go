package controller

import (
	"os/exec"
	"strings"
	"testing"
)

// The two deployment paths must render the same admission configuration, and nothing but this test makes
// them.
//
// They did not. config/webhook-enabled carried the guards and config/operator -- the overlay Argo CD
// reconciles -- did not, so a cluster built on a laptop refused an unaccounted device holder and the
// GitOps-managed one admitted it. The first attempt to fix that by adding the guards INSIDE config/operator
// then hit the trap this test also covers: that overlay sets a namePrefix, the webhook manifests spell their
// names out in full, and the result was
// gpu-platform-control-plane-gpu-platform-control-plane-webhook-service -- a Service the Certificate's
// dnsNames do not cover, so with failurePolicy Fail every governed write in the cluster would have been
// refused. It was found by diffing the two renders by hand. This is that diff, kept.
//
// Mutation that turns this red: move the webhook-component include from config/operator-webhook into
// config/operator, or drop it from either overlay.
func TestBothDeploymentPathsRenderTheSameGuards(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not on PATH, so the overlays cannot be rendered here")
	}
	gitops := renderWebhookConfig(t, "../../config/operator-webhook")
	laptop := renderWebhookConfig(t, "../../config/webhook-enabled")
	if gitops != laptop {
		t.Fatalf("the GitOps and laptop overlays disagree about admission validation.\n"+
			"operator-webhook:\n%s\n\nwebhook-enabled:\n%s", gitops, laptop)
	}
	if !strings.Contains(gitops, "vgpupod.kb.io") || !strings.Contains(gitops, "vgpujob.kb.io") {
		t.Fatalf("both paths agree and neither deploys the device guards:\n%s", gitops)
	}
	// The double-prefix bug is invisible in an equality check if BOTH overlays acquire it, so the shape is
	// asserted directly against the name the Certificate's dnsNames actually cover.
	if strings.Contains(gitops, "gpu-platform-control-plane-gpu-platform-control-plane-") {
		t.Fatal("a name carries the prefix twice; the Certificate's dnsNames will not cover the Service and " +
			"failurePolicy Fail turns that into every governed write being refused")
	}
}

// renderWebhookConfig returns the ValidatingWebhookConfiguration document an overlay produces.
func renderWebhookConfig(t *testing.T, overlay string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "kustomize", overlay).Output()
	if err != nil {
		t.Fatalf("render %s: %v", overlay, err)
	}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		if strings.Contains(doc, "kind: ValidatingWebhookConfiguration") {
			return doc
		}
	}
	t.Fatalf("%s renders no ValidatingWebhookConfiguration, so it deploys no admission guards", overlay)
	return ""
}
