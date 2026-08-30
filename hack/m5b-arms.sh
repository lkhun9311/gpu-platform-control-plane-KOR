#!/usr/bin/env bash
#
# The four arms, replayed against the engine hack/m5b-gpu-session.sh is holding up.
#
# Split from that script on purpose: the session owns the expensive, slow, one-time half (a node, an
# engine, a capacity check, a measured rate) and this owns the half that gets re-run when an arm is
# botched. Re-running this costs minutes; re-running the session costs the node's warmup again.
#
# R1 is the uncontended premium baseline, off is the contended one, static-cap is the admission-matched
# control, kv-aware is the arm under test. They share ONE trace: the report asserts a single checksum
# across the contended arms, because arms that replay different traffic are not comparable however good
# their numbers look.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

NS="${NS:-m5b}"
KCTX="${KCTX:-$(kubectl config current-context 2>/dev/null)}"
ENGINE=vllm-qwen25-3b
MODEL="Qwen/Qwen2.5-3B-Instruct"
GW_IMAGE="${GW_IMAGE:-gateway:m5b}"
# Reps: the block bootstrap cannot bound its own variance from one repetition -- the interval degenerates
# to the point estimate and the report says so. Two is the floor, not a target.
# Four, matching hack/gpu-session.sh and the design.
#
# This defaulted to 2, and the session script it is meant to complement defaults to 4. The scripts do not
# read each other, so a re-run bought half the repetitions the study was designed around -- silently, and in
# the direction that weakens it. Two independent reviews scored the design's statistical power at 30% when
# n was 2 per cell and named the run count as the binding limit; that is the number this default was quietly
# restoring every time an arm was re-run.
#
# Below four the incremental interval is a bootstrap over very few blocks. Two is a floor the report will
# tolerate, not a target anything argued for.
REPS="${REPS:-4}"
OUT="${OUT:-hack/m5b-run-$(date +%Y%m%d-%H%M%S)}"
LOG="$OUT/evidence.log"

k() { kubectl --context "$KCTX" "$@"; }
say() { echo "== $*" | tee -a "$LOG"; }
fail() { echo "ARMS FAILED: $*" | tee -a "$LOG" >&2; exit 1; }

[ -n "${RATE:-}" ] || fail "RATE is unset. It must come from hack/m5b-gpu-session.sh's prefill measurement on THIS card, not from a default: the harness default of 20/s demands 3.8x an A10G's theoretical peak, and 7.3x a T4's, and a run at it censors every arm's tail and is disqualified by EvaluateChecks after the card has been paid for."

mkdir -p "$OUT" || fail "cannot create $OUT"
: > "$LOG"
say "rate ${RATE}/s, ${REPS} repetitions, output $OUT"

WORK="$(mktemp -d)"
PF_PID=""
cleanup() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"

# Duration is derived from the rate so each arm reaches a tail worth reporting. MinTailSamples is 100:
# below it a nearest-rank p99 is just the slowest request, and the report disqualifies the run. 500 premium
# completions is the target; premium is half the arrivals.
DURATION_MS=$(python3 -c "print(int(500 / (float('$RATE')/2) * 1000))")
say "duration per arm: $((DURATION_MS/1000))s, targeting 500 premium completions (min is 100)"

say "build and push the gateway image"
cat > "$WORK/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static:nonroot
COPY gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
EOF
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build gateway image"
if [ -n "${REGISTRY:-}" ]; then
  docker tag "$GW_IMAGE" "$REGISTRY/$GW_IMAGE" && docker push "$REGISTRY/$GW_IMAGE" >/dev/null \
    || fail "push gateway image to $REGISTRY"
  GW_IMAGE="$REGISTRY/$GW_IMAGE"
  # Resolve the tag to the digest that was just pushed, and use THAT everywhere after this line.
  #
  # The tag is how the push is addressed; it is not what the record may claim. gateway:m5b names whatever was
  # pushed under it most recently, so a manifest carrying it identifies nothing after the next build -- and
  # the manifest fields for this (gatewaySHA, imageDigests) had never been filled by anything at all.
  GW_DIGEST_REF="$(docker inspect --format='{{index .RepoDigests 0}}' "$GW_IMAGE" 2>/dev/null || true)"
  case "$GW_DIGEST_REF" in
    *@sha256:*) GW_IMAGE="$GW_DIGEST_REF" ;;
    *) fail "could not resolve $GW_IMAGE to a digest after pushing it; the run would record a tag, which names nothing after the next build" ;;
  esac
else
  fail "REGISTRY is unset. A kind cluster can side-load an image; EKS cannot, so the gateway image must be pushed somewhere the nodes can pull from (ECR). Set REGISTRY=<account>.dkr.ecr.<region>.amazonaws.com"
fi

say "identity and policy"
k get ns "$NS" >/dev/null 2>&1 || fail "namespace $NS does not exist; run hack/m5b-gpu-session.sh first"
k create secret generic gateway-api-keys -n "$NS" \
  --from-literal=premium-key=premium-1 \
  --from-literal=standard-key=standard-noisy --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<EOF || fail "apply policies"
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5b-premium
  annotations:
    platform.lkhun9311.github.io/tier: premium
spec:
  tenant: premium-1
  targetNamespace: $NS
  gpuClass: t4
  limits:
    gpuCount: 1
---
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5b-standard
spec:
  tenant: standard-noisy
  targetNamespace: $NS
  gpuClass: t4
  limits:
    gpuCount: 1
EOF
k create serviceaccount gateway -n "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null
k create clusterrolebinding "m5b-gateway-role" --clusterrole=gateway-role \
  --serviceaccount="$NS:gateway" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<EOF || fail "apply secret-reader role"
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: gateway-secret-reader, namespace: $NS}
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: gateway-secret-reader, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: gateway-secret-reader}
subjects:
  - {kind: ServiceAccount, name: gateway, namespace: $NS}
EOF

# One gateway, redeployed per arm, because the admission mode is a startup flag. The trace is generated
# ONCE and reused, so every contended arm replays identical traffic and the report's checksum assertion
# has something true to assert.
deploy_gateway() {
  local mode="$1" extra="$2"
  k apply -f - >/dev/null <<EOF || fail "apply gateway ($mode)"
apiVersion: apps/v1
kind: Deployment
metadata: {name: gateway, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: m5b-gateway}}
  template:
    metadata:
      labels: {app: m5b-gateway}
      annotations: {arm: "$mode-$extra"}
    spec:
      serviceAccountName: gateway
      containers:
        - name: gateway
          image: $GW_IMAGE
          args: ["-admission-mode=$mode"$extra]
          env:
            - {name: GATEWAY_NAMESPACE, value: $NS}
            - {name: GATEWAY_API_KEY_SECRET, value: gateway-api-keys}
          ports: [{containerPort: 8080, name: http}]
EOF
  k rollout status deploy/gateway -n "$NS" --timeout=180s >/dev/null || fail "gateway ($mode) never became ready"
}

# The build the gateway binary came from, and the engine image the numbers are produced against.
#
# GW_SHA is the working tree's commit, with -dirty when it is not clean: a paid run built from uncommitted
# code is a legitimate thing to do and an illegitimate thing to hide.
GW_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
git diff --quiet 2>/dev/null || GW_SHA="$GW_SHA-dirty"

ENGINE_IMAGE="$(k get deploy -n "$NS" "$ENGINE" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
case "$ENGINE_IMAGE" in
  *@sha256:*) ;;
  *) fail "the engine deployment runs image '${ENGINE_IMAGE:-<none>}', which is not digest-pinned; the record could not name what produced its latencies" ;;
esac
say "provenance: gateway $GW_SHA / $GW_IMAGE"
say "            engine  $ENGINE_IMAGE"

say "generate the shared trace once"
"$WORK/benchharness" gen-trace --seed 7 --duration-ms "$DURATION_MS" --rate "$RATE" \
  --arm off --gateway-url "http://127.0.0.1:18080" \
  --gateway-sha "$GW_SHA" --gateway-image "$GW_IMAGE" --engine-image "$ENGINE_IMAGE" \
  --trace-out "$OUT/trace.jsonl" --manifest-out "$OUT/manifest-off.yaml" || fail "gen-trace"

for rep in $(seq 1 "$REPS"); do
  for arm in R1 off static-cap kv-aware; do
    say "rep $rep arm $arm"
    case "$arm" in
      R1|off)      deploy_gateway off "" ;;
      static-cap)  deploy_gateway static-cap ", \"-admission-static-rate=${RATE}\", \"-admission-long-threshold=4096\"" ;;
      kv-aware)    deploy_gateway kv-aware ", \"-admission-long-threshold=4096\"" ;;
    esac

    [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
    k port-forward -n "$NS" deploy/gateway 18080:8080 >/dev/null 2>&1 &
    PF_PID=$!
    sleep 3

    "$WORK/benchharness" gen-trace --seed 7 --duration-ms "$DURATION_MS" --rate "$RATE" \
      --arm "$arm" --gateway-url "http://127.0.0.1:18080" \
      --gateway-sha "$GW_SHA" --gateway-image "$GW_IMAGE" --engine-image "$ENGINE_IMAGE" \
      --trace-out "$OUT/trace-$arm-$rep.jsonl" --manifest-out "$OUT/manifest-$arm-$rep.yaml" \
      || fail "gen-trace $arm"
    "$WORK/benchharness" replay --manifest "$OUT/manifest-$arm-$rep.yaml" \
      --require-provenance \
      --target "http://127.0.0.1:18080" \
      --api-keys "premium-1=premium-key,standard-noisy=standard-key" \
      --raw-out "$OUT/raw-$arm-$rep.jsonl" || fail "replay $arm"
    [ -s "$OUT/raw-$arm-$rep.jsonl" ] || fail "no raw evidence for $arm rep $rep"
    say "  $(wc -l < "$OUT/raw-$arm-$rep.jsonl") rows recorded"
  done
done

say "report"
args=()
for f in "$OUT"/raw-*.jsonl; do args+=(--raw "$f"); done
"$WORK/benchharness" report "${args[@]}" --out "$OUT/report.txt" || fail "report"
cat "$OUT/report.txt" | tee -a "$LOG"

grep -q "VERDICT:" "$OUT/report.txt" || fail "the report carries no verdict"
say "ARMS DONE. Evidence in $OUT. Scale the GPU node group to 0 now."
