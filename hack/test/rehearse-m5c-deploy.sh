#!/usr/bin/env bash
#
# Runs the GPU-free half of the M5-c matrix on a development machine.
#
# WHY THIS EXISTS
#
# hack/m5c-matrix.sh had never been run. Reading it found three defects that would each have ended a paid
# session -- both sharing overlays rendering into a namespace nothing creates, the control arm having no
# device plugin at all, and a `-ge` device count that passes on the outgoing arm's advertisement. Those were
# found by reading, which is not a method that scales: the remaining risk is in the parts where reading says
# "this should work" and only a cluster can disagree.
#
# So this stands up a real kind cluster and drives the matrix's DEPLOYMENT half against it: CRDs, the
# gateway's ClusterRole, the fake device plugin, the gateway image built and side-loaded, two namespaces,
# two GPUQuotaPolicy, the InferenceDeployment routing records, and one request sent through the gateway to
# a stub standing where vLLM would be. If a tenant's request comes back from the engine its policy points
# at, the routing this matrix is built on works.
#
# WHAT IT DOES NOT REHEARSE, stated so it is not mistaken for covered
#
#   * Anything about a card. No driver, no real device plugin, no time-slicing, no MPS, no vLLM. The fake
#     plugin (config/device-plugin) advertises nvidia.com/gpu on a node with no cards, which is what lets
#     the engine Pods schedule at all.
#   * The `shared` vs split TOPOLOGY. Both stub engines are ordinary Pods; what is being checked is that the
#     gateway routes each tenant to the engine its policy names, which is the mechanism the arms vary.
#   * Any number. Nothing here measures latency, and nothing it prints belongs in a write-up.
#
# It is the counterpart of hack/test/rehearse-bringup.sh, which covers the cluster bring-up of the device
# session for the same reason and after the same kind of loss.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1
cd .. || exit 1

CLUSTER="${CLUSTER:-m5c-rehearse}"
KCTX="kind-$CLUSTER"
NS_A="${NS_A:-m5c-a}"
NS_B="${NS_B:-m5c-b}"
MODEL="Qwen/Qwen2.5-3B-Instruct"
GW_IMAGE="${GW_IMAGE:-gateway:m5c-rehearse}"
STUB_IMAGE="${STUB_IMAGE:-benchstub:m5c-rehearse}"
# The tag config/device-plugin/daemonset.yaml names, because the manifest is what selects the image and a
# rehearsal-specific tag would leave the DaemonSet pulling a `latest` that is not on this node. m6-kind-e2e
# builds the same tag for the same reason.
SIM_IMAGE="${SIM_IMAGE:-gpu-simulator:latest}"
OPERATOR_NS="gpu-platform-control-plane-system"
KEEP="${KEEP:-0}"
WORK="$(mktemp -d)"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'REHEARSAL FAILED: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$KCTX" "$@"; }

for b in kind kubectl docker go; do
  command -v "$b" >/dev/null || fail "$b is not on PATH, and this rehearsal runs the real thing rather than a stub of it"
done
docker info >/dev/null 2>&1 || fail "the docker daemon is not reachable"

cleanup() {
  if [ "$KEEP" = "1" ]; then
    say "KEEP=1: leaving cluster $CLUSTER up. Delete it with: kind delete cluster --name $CLUSTER"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

say "cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null || fail "kind create cluster"

# CRDs applied with kustomize directly, NOT `make install`.
#
# That target depends on `manifests`, which runs controller-gen and rewrites generated files -- which this
# repository's conventions forbid doing casually, because in the Korean mirror it destroys hand-translated
# annotations. The CRDs are committed; building them is all that is needed.
say "CRDs"
kubectl kustomize config/crd | k apply -f - >/dev/null || fail "apply the CRDs"

# The gateway's ClusterRole. hack/m5c-matrix.sh binds to it and does not create it, because on EKS it
# arrives with the gateway through GitOps. A binding to a missing ClusterRole is accepted by Kubernetes and
# fails at request time, so the matrix refuses when it is absent and this is what satisfies that refusal.
#
# Through the overlay, not the bare file. config/gateway/rbac.yaml says `namespace: system`, which is
# kubebuilder's placeholder; config/gateway/kustomization.yaml is what rewrites it to the operator's
# namespace. Applying the file directly is refused with `namespaces "system" not found` -- which is how this
# rehearsal opened, and it is the same defect that had both device-plugin sharing overlays un-appliable.
#
# Only the RBAC kinds. The overlay also carries the gateway Deployment, pinned to an ECR digest this cluster
# cannot pull, and the matrix deploys its own gateway from a locally built image anyway.
say "gateway RBAC"
k create ns "$OPERATOR_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
kubectl kustomize config/gateway > "$WORK/gateway-all.yaml" || fail "render config/gateway"
awk 'BEGIN{RS="\n---\n"} /(^|\n)kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding|ServiceAccount)(\n|$)/ {print "---"; print $0}' \
  "$WORK/gateway-all.yaml" > "$WORK/gateway-rbac.yaml"
grep -q "name: gateway-role" "$WORK/gateway-rbac.yaml" \
  || fail "the RBAC filter kept no ClusterRole named gateway-role; config/gateway's shape has changed and this rehearsal would go on to prove nothing"
k apply -f "$WORK/gateway-rbac.yaml" >/dev/null || fail "apply the gateway RBAC"

say "build the gateway, the engine stub and the GPU simulator"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY gateway /gateway\nUSER 65532:65532\nENTRYPOINT ["/gateway"]\n' > "$WORK/Dockerfile"
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build the gateway image"
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY benchharness /benchharness\nUSER 65532:65532\nENTRYPOINT ["/benchharness","stub-serve"]\n' > "$WORK/Dockerfile"
docker build -q -t "$STUB_IMAGE" "$WORK" >/dev/null || fail "build the stub image"
docker build -q -t "$SIM_IMAGE" -f Dockerfile.gpu-simulator . >/dev/null || fail "build the gpu-simulator image"

# The same side-load the kind branch of the matrix uses. If this cannot hand an image to the node, neither
# can the paid run.
say "side-load the images"
for img in "$GW_IMAGE" "$STUB_IMAGE" "$SIM_IMAGE"; do
  kind load docker-image "$img" --name "$CLUSTER" >/dev/null || fail "kind load $img"
done

# The FAKE device plugin, and it must be the fake one: this machine has no card and the engines below ask
# for nvidia.com/gpu. config/device-plugin advertises the resource on a node that has none, which is the one
# substitution that makes the deployment path runnable here.
#
# Two devices, because the split arms put two engines on one node and each asks for one. The first version
# of this rehearsal skipped the simulator's own image build entirely, so the DaemonSet sat in
# ImagePullBackOff, nothing advertised anything, and the engines were Pending -- which is exactly the shape
# the paid run's own preflight exists to catch, arriving here for free instead.
say "fake device plugin, two devices"
kubectl kustomize config/device-plugin | k apply -f - >/dev/null || fail "apply the simulator device plugin"
k -n "$OPERATOR_NS" set env ds/gpu-simulator FAKE_GPU_COUNT=2 >/dev/null || fail "set FAKE_GPU_COUNT"
k -n "$OPERATOR_NS" patch ds gpu-simulator --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null 2>&1 || true
k -n "$OPERATOR_NS" rollout status ds/gpu-simulator --timeout=180s >/dev/null \
  || fail "the simulator device plugin never became ready: $(k -n "$OPERATOR_NS" get pod -l app.kubernetes.io/component=gpu-simulator -o wide 2>&1 | tail -2)"

say "wait for the node to advertise nvidia.com/gpu"
for _ in $(seq 1 30); do
  adv=$(k get nodes -o jsonpath='{.items[0].status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
  [ "${adv:-0}" -ge 2 ] 2>/dev/null && break
  sleep 2
done
[ "${adv:-0}" -ge 2 ] || fail "the node advertises ${adv:-0} nvidia.com/gpu after the simulator rolled out; the engines below would sit Pending to their timeout, which is what a paid run would have discovered on the card"
say "  node advertises $adv"

k create ns "$NS_A" >/dev/null 2>&1 || true
k create ns "$NS_B" >/dev/null 2>&1 || true

# One stub engine per namespace, named the way the gateway resolves them.
#
# internal/gateway/router.go builds http://<InferenceDeployment name>.<namespace>.svc:<port>, so the Service
# must carry the InferenceDeployment's name. That is the coupling this rehearsal is here to exercise: it is
# stated in a comment in router.go and in nothing that fails when it stops being true.
stub_engine() {
  local ns="$1" name="$2"
  k apply -f - >/dev/null <<EOF || fail "stub engine $name in $ns"
apiVersion: apps/v1
kind: Deployment
metadata: {name: $name, namespace: $ns}
spec:
  replicas: 1
  selector: {matchLabels: {engine: $name}}
  template:
    metadata:
      labels: {engine: $name}
    spec:
      containers:
        - name: stub
          image: $STUB_IMAGE
          imagePullPolicy: IfNotPresent
          args: ["--addr=:8000"]
          ports: [{containerPort: 8000, name: http}]
          resources:
            limits:
              nvidia.com/gpu: 1
---
apiVersion: v1
kind: Service
metadata: {name: $name, namespace: $ns}
spec:
  selector: {engine: $name}
  ports: [{name: http, port: 8000, targetPort: http}]
EOF
  k rollout status "deploy/$name" -n "$ns" --timeout=180s >/dev/null \
    || fail "stub engine $name never became ready in $ns; $(k get pod -n "$ns" -l "engine=$name" -o wide 2>&1 | tail -2)"
}

# The routing record, byte for byte the shape hack/m5c-matrix.sh applies.
routing_record() {
  local ns="$1" name="$2"
  k apply -f - >/dev/null <<EOF || fail "routing record in $ns"
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata: {name: $name, namespace: $ns}
spec:
  model: {name: $MODEL, storageUri: "hf://$MODEL"}
  image: registry.k8s.io/pause:3.9
  gpuCount: 0
  replicas: 0
  port: 8000
EOF
}

say "two stub engines, one per namespace"
stub_engine "$NS_A" vllm-shared-a
stub_engine "$NS_B" vllm-shared-b
routing_record "$NS_A" vllm-shared-a
routing_record "$NS_B" vllm-shared-b

# The split arm's routing: one policy per tenant, each pointing at a different namespace. This is the whole
# of what the time-slicing arm varies, and it is the thing most worth proving works before renting a card.
say "policies: premium -> $NS_A, standard -> $NS_B"
k apply -f - >/dev/null <<EOF || fail "policies"
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5c-premium
  annotations: {platform.lkhun9311.github.io/tier: premium}
spec: {tenant: premium-1, targetNamespace: $NS_A, gpuClass: a10g, limits: {gpuCount: 1}}
---
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata: {name: m5c-standard}
spec: {tenant: standard-noisy, targetNamespace: $NS_B, gpuClass: a10g, limits: {gpuCount: 1}}
EOF

k create secret generic gateway-api-keys -n "$NS_A" \
  --from-literal=premium-key=premium-1 --from-literal=standard-key=standard-noisy \
  --dry-run=client -o yaml | k apply -f - >/dev/null
k create serviceaccount gateway -n "$NS_A" --dry-run=client -o yaml | k apply -f - >/dev/null
k create clusterrolebinding m5c-rehearse-gateway --clusterrole=gateway-role \
  --serviceaccount="$NS_A:gateway" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<EOF || fail "secret-reader role"
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: gateway-secret-reader, namespace: $NS_A}
rules: [{apiGroups: [""], resources: ["secrets"], verbs: ["get","list","watch"]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: gateway-secret-reader, namespace: $NS_A}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: gateway-secret-reader}
subjects: [{kind: ServiceAccount, name: gateway, namespace: $NS_A}]
EOF

say "gateway"
k apply -f - >/dev/null <<EOF || fail "gateway"
apiVersion: apps/v1
kind: Deployment
metadata: {name: gateway, namespace: $NS_A}
spec:
  replicas: 1
  selector: {matchLabels: {app: m5c-gateway}}
  template:
    metadata:
      labels: {app: m5c-gateway}
    spec:
      serviceAccountName: gateway
      containers:
        - name: gateway
          image: $GW_IMAGE
          imagePullPolicy: IfNotPresent
          args: ["-admission-mode=off"]
          env:
            - {name: GATEWAY_NAMESPACE, value: $NS_A}
            - {name: GATEWAY_API_KEY_SECRET, value: gateway-api-keys}
          ports: [{containerPort: 8080, name: http}]
EOF
k rollout status deploy/gateway -n "$NS_A" --timeout=180s >/dev/null \
  || fail "the gateway never became ready: $(k logs -n "$NS_A" deploy/gateway --tail=20 2>&1)"

PF_PID=""
k port-forward -n "$NS_A" deploy/gateway 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
# shellcheck disable=SC2064
trap "kill $PF_PID 2>/dev/null; cleanup" EXIT
sleep 3

# THE ASSERTION. Each tenant's key must reach the engine its policy names, and the two must not be the same
# engine -- that difference IS the split arm.
#
# The stub answers with the model it was asked for, so a 200 proves the chain resolved: key -> tenant ->
# GPUQuotaPolicy -> targetNamespace -> InferenceDeployment -> same-named Service -> Pod. A failure anywhere in
# it is a 4xx or 5xx here rather than a puzzle on a rented card.
probe() {
  local key="$1" code
  code=$(curl -s -o "$WORK/body-$key.json" -w '%{http_code}' \
    -X POST "http://127.0.0.1:18080/v1/chat/completions" \
    -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
    --max-time 20 \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":4}" 2>/dev/null)
  printf '%s' "$code"
}

say "routing probe"
failed=0
for key in premium-key standard-key; do
  code=$(probe "$key")
  if [ "$code" = "200" ]; then
    say "  $key -> HTTP $code"
  else
    say "  $key -> HTTP $code   $(head -c 300 "$WORK/body-$key.json" 2>/dev/null)"
    failed=1
  fi
done

if [ "$failed" != "0" ]; then
  say "gateway logs:"
  k logs -n "$NS_A" deploy/gateway --tail=30 2>&1 | sed 's/^/  /'
  fail "a tenant did not reach its engine through the gateway. On a rented card this is every request of every arm, discovered after the engines have loaded."
fi

# Two 200s are NOT the assertion, and stopping there would have been the same defect this rehearsal exists
# to catch: both tenants routed to ONE engine also returns two 200s, and that is the `shared` topology
# wearing the split arm's name. What has to be true is that each request landed on the engine its own policy
# names, so the engines are asked how many requests they served.
say "which engine served which tenant"
served() {
  local ns="$1" name="$2" n pf port
  # Over a port-forward rather than from inside: the stub image is distroless, so there is no shell and no
  # curl to exec. Port 0 lets the kernel choose, which is what keeps two of these from colliding.
  k port-forward -n "$ns" "deploy/$name" 0:8000 >"$WORK/pf-$name" 2>&1 &
  pf=$!
  sleep 2
  port=$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\).*/\1/p' "$WORK/pf-$name" | head -1)
  if [ -n "$port" ]; then
    n=$(curl -s --max-time 5 "http://127.0.0.1:$port/stats" | sed -n 's/.*"requestsServed":\([0-9]*\).*/\1/p')
  else
    n=""
  fi
  kill "$pf" 2>/dev/null
  printf '%s' "${n:-unknown}"
}
a_served=$(served "$NS_A" vllm-shared-a)
b_served=$(served "$NS_B" vllm-shared-b)
say "  $NS_A/vllm-shared-a served $a_served"
say "  $NS_B/vllm-shared-b served $b_served"

case "$a_served:$b_served" in
  1:1) say "each tenant reached its own engine" ;;
  unknown:*|*:unknown)
    fail "could not read one engine's request count, so this rehearsal cannot say the tenants were separated. Two 200s alone are also what a single shared engine returns." ;;
  *)
    fail "the two tenants did not land one per engine: $NS_A served $a_served and $NS_B served $b_served against one request each. If both landed on one engine the split arms would be the shared arm under another name, and every latency number would be attributed to a topology that was never deployed." ;;
esac

say "REHEARSAL PASSED: both tenants routed to the engine their policy names, on a real cluster."
say "What this did NOT cover: the card, the sharing plugins, vLLM, and every number."
