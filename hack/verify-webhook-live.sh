#!/usr/bin/env bash
# Checks the links envtest cannot: the ones envtest replaces.
#
# envtest installs the real ValidatingWebhookConfiguration but rewrites clientConfig to dial a local process,
# so it proves the rules, operations and handler path are right and proves nothing about the Service routing
# to the manager Pod, cert-manager issuing the Secret, the CA landing in caBundle, the manager mounting the
# certificate, or the apiserver's TLS verification accepting the Certificate's DNS names.
#
# Requires: cert-manager, and `kubectl apply -k config/webhook-enabled` already applied.
set -uo pipefail

NS=${NS:-default}
SYS=${SYS:-gpu-platform-control-plane-system}
WHC=gpu-platform-control-plane-validating-webhook-configuration
MGR=deploy/gpu-platform-control-plane-controller-manager

pass=0; fail=0
ok()  { echo "OK    $1"; pass=$((pass+1)); }
bad() { echo "FAIL  $1"; fail=$((fail+1)); }

cleanup() {
  kubectl -n "$NS" delete mltrainingjob live-ok --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete job live-ok --ignore-not-found >/dev/null 2>&1
}
trap cleanup EXIT

CA=$(kubectl get validatingwebhookconfiguration "$WHC" -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null)
[ -n "$CA" ] && ok "caBundle injected (${#CA} bytes)" || bad "caBundle is empty; cainjector never ran"

# The control, and the most important check here. failurePolicy is Fail, so a broken Service, a wrong
# caBundle, or a TLS name mismatch ALL surface as a rejection — identical in shape to the rejections below.
# Without this, a completely non-functional webhook scores full marks.
OUT=$(kubectl apply -f - <<'EOF' 2>&1
apiVersion: platform.lkhun9311.github.io/v1
kind: MLTrainingJob
metadata: {name: live-ok, namespace: default}
spec: {queue: team-a, image: "trainer:v1", gpuCount: 1, parallelism: 1, completions: 1}
EOF
)
if grep -qE "created|configured" <<<"$OUT"; then
  ok "a runnable spec is accepted (so the rejections below are the webhook's, not a wiring failure)"
else
  bad "a runnable spec was refused, so nothing below is attributable: $(head -1 <<<"$OUT")"
fi

OUT=$(kubectl apply -f - <<'EOF' 2>&1
apiVersion: platform.lkhun9311.github.io/v1
kind: MLTrainingJob
metadata: {name: live-blank, namespace: default}
spec: {queue: team-a, image: "", gpuCount: 1, parallelism: 1, completions: 1}
EOF
)
if grep -q "spec.image" <<<"$OUT"; then ok "blank image refused, and refused FOR the image"
elif grep -qE "created|configured" <<<"$OUT"; then bad "blank image accepted; the webhook was not consulted"
else bad "refused for another reason: $(head -1 <<<"$OUT")"; fi

kubectl -n "$NS" create job live-ok --image=busybox >/dev/null 2>&1
OUT=$(kubectl -n "$NS" patch mltrainingjob live-ok --type=merge -p '{"spec":{"image":"trainer:v9"}}' 2>&1)
if grep -q "spec.image" <<<"$OUT"; then ok "image edit refused once the Job exists"
elif grep -q "patched" <<<"$OUT"; then bad "the edit was stored; the silent no-op is still there"
else bad "unexpected response: $(head -1 <<<"$OUT")"; fi

LOGS=$(kubectl -n "$SYS" logs "$MGR" --tail=300 2>/dev/null)
# Matching the word "certificate" here would flag the manager's own startup line about watching one, which is
# what a first version of this script did. Only actual failure strings count.
TLSERR=$(grep -iE 'tls: (handshake|bad|failed)|x509:|certificate (signed by unknown|has expired|is not valid)|remote error' <<<"$LOGS" | head -1)
[ -z "$TLSERR" ] && ok "no TLS failures in the manager log" || bad "TLS failure: $TLSERR"

if grep -q "admission validation is NOT running" <<<"$LOGS"; then
  bad "the manager started without a certificate, so validation is disabled"
else
  ok "the manager started with its certificate"
fi

echo "---- pass=$pass fail=$fail ----"
[ "$fail" -eq 0 ]
