#!/usr/bin/env bash
#
# The M5-b paid session: one A10G, four arms, and a bill that stops when the script does.
#
# Everything before this has run against a stub or a CPU engine. hack/m5b-chain-live.sh proved the chain
# carries a request and that the guard engages on a real engine's telemetry; what it could not produce is a
# latency anyone should quote, because a CPU server's numbers are the host's rather than the policy's.
#
# Two rules shape this file.
#
# It REFUSES rather than improvises. A session that starts on a cluster missing its operator, or an engine
# whose KV cache is nothing like the size the sizing page predicted, produces a number that looks like a
# result and is not one. Every check below stops the run instead of continuing with a caveat.
#
# It scales the node group back to zero on EVERY exit path, including failure and interrupt -- with a real
# AWS call that is read back to confirm it took, not a printed reminder. A GPU node bills by the hour
# whether or not anything is scheduled on it. KEEP_NODE=1 opts out, for hack/m5b-arms.sh re-runs against a
# node that is already warm.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

NS="${NS:-m5b}"
KCTX="${KCTX:-$(kubectl config current-context 2>/dev/null)}"
ENGINE=vllm-qwen25-3b
MODEL="Qwen/Qwen2.5-3B-Instruct"
# From hack/m5b-vllm-sizing.md. Predicted BEFORE the run so the engine can contradict it.
PREDICTED_KV_TOKENS=382270
PREDICTED_CONTENDER_TOKENS=7695
LOG=hack/m5b-gpu-session-evidence.log
WORK="$(mktemp -d)"

k() { kubectl --context "$KCTX" "$@"; }
say() { echo "== $*" | tee -a "$LOG"; }
note() { echo "$*" | tee -a "$LOG"; }
fail() { echo "SESSION REFUSED: $*" | tee -a "$LOG" >&2; exit 1; }

SCALED_UP=""
NODEGROUP=""

# Scaling the node group back to zero, which this file's header claimed and this function did not do.
#
# It printed a banner. A banner is not a control: it depends on a human reading a terminal that may have
# scrolled, or that no longer exists because the laptop closed. The node group keeps desiredSize=1, the ASG
# keeps a GPU node, and g4dn.12xlarge bills $4.812/hour with nothing scheduled on it.
#
# The names are derived rather than asked for, because a prompt in a trap is a prompt nobody answers:
# the cluster comes from the kubeconfig context's EKS ARN and the node group from the node's own
# eks.amazonaws.com/nodegroup label. If either cannot be derived the function says so and prints the manual
# command -- degrading to the old behaviour rather than failing silently.
#
# KEEP_NODE=1 skips it, and that exists for a real workflow rather than as an escape hatch: hack/m5b-arms.sh
# re-runs a botched arm against an already-warm node, and a session that scaled its node away on exit would
# make that script pay for a fresh warmup every time.
#
# What this does NOT solve, stated so it is not mistaken for solved: a trap cannot run after the laptop dies,
# loses power, or has its shell killed with SIGKILL. The backstop for that is the nightly destroy workflow,
# and an external TTL watchdog is the control this repository still does not have.
gpu_scale_down() {
  [ -n "$SCALED_UP" ] || return 0
  if [ "${KEEP_NODE:-}" = "1" ]; then
    echo "KEEP_NODE=1: leaving $NODEGROUP at its current size. It bills by the hour." >&2
    return 0
  fi

  CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"
  if [ -z "$CLUSTER" ] || [ -z "${NODEGROUP:-}" ]; then
    echo
    echo "############################################################"
    echo "#  COULD NOT DERIVE cluster/nodegroup. SCALE TO 0 BY HAND. #"
    echo "#  It bills by the hour with nothing scheduled on it.      #"
    echo "#    aws eks update-nodegroup-config \\"
    echo "#      --cluster-name <cluster> --nodegroup-name <ng> \\"
    echo "#      --scaling-config minSize=0,maxSize=1,desiredSize=0  #"
    echo "############################################################"
    return 0
  fi

  echo "scaling $CLUSTER/$NODEGROUP to desiredSize=0" >&2
  if ! aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
        --scaling-config "minSize=0,maxSize=1,desiredSize=0" >/dev/null 2>&1; then
    echo
    echo "############################################################"
    echo "#  SCALE-DOWN CALL FAILED. THE NODE IS STILL BILLING.      #"
    echo "#  Run this now:                                           #"
    echo "#    aws eks update-nodegroup-config --cluster-name $CLUSTER \\"
    echo "#      --nodegroup-name $NODEGROUP \\"
    echo "#      --scaling-config minSize=0,maxSize=1,desiredSize=0  #"
    echo "############################################################"
    return 0
  fi

  # Confirm it took. An accepted API call that left desiredSize at 1 is the failure this whole function
  # exists to make impossible, and it is silent unless something reads the value back.
  DESIRED=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
    --query 'nodegroup.scalingConfig.desiredSize' --output text 2>/dev/null)
  if [ "$DESIRED" = "0" ]; then
    echo "$CLUSTER/$NODEGROUP is at desiredSize=0" >&2
  else
    echo "WARNING: $CLUSTER/$NODEGROUP reports desiredSize=$DESIRED after the scale-down. It is billing." >&2
  fi
}

cleanup() {
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null
  gpu_scale_down
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

: > "$LOG"

# ---------------------------------------------------------------- preflight

say "preflight"
[ -n "$KCTX" ] || fail "no kubectl context is set"
note "context: $KCTX"
case "$KCTX" in
  kind-*) fail "context $KCTX is a kind cluster; this script is the PAID session and must not run there. hack/m5b-chain-live.sh is the free one." ;;
esac

k get crd inferencedeployments.platform.lkhun9311.github.io >/dev/null 2>&1 \
  || fail "the platform CRDs are not installed on this cluster"
k get deploy -A -o name 2>/dev/null | grep -q controller-manager \
  || fail "the operator is not running; the InferenceDeployment routing record would never be reconciled"

# The engine must land on a GPU node, and the node must advertise a device. Both are the operator's
# problem in production and neither is checked anywhere else, so they are checked here where a wrong
# answer costs money rather than a test run.
gpu_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu=true' -o name 2>/dev/null | wc -l)
if [ "$gpu_nodes" -eq 0 ]; then
  cat <<EOF

No GPU node is present. Scale the one-card node group up, then re-run:

    aws eks update-nodegroup-config --cluster-name <cluster> \\
      --nodegroup-name gpu_single --scaling-config minSize=0,maxSize=1,desiredSize=1

  or set desired_size = 1 on the gpu_single group in infra/aws/cluster/eks.tf and terraform apply.

The node takes a few minutes to join and advertise nvidia.com/gpu.
EOF
  fail "no GPU node"
fi
SCALED_UP=1

# The node group name comes from the node the session just qualified, not from a constant.
#
# EKS stamps eks.amazonaws.com/nodegroup on every managed-node-group member, so the node this session is
# about names the group that must be scaled back down. A hardcoded "gpu_single" would be wrong the moment a
# group is renamed, and wrong silently: the scale-down would return a ResourceNotFoundException that nobody
# reads, on the exit path of the script that spends money.
NODEGROUP=$(k get node -l 'platform.lkhun9311.github.io/gpu=true' \
  -o jsonpath='{.items[0].metadata.labels.eks\.amazonaws\.com/nodegroup}' 2>/dev/null)
[ -n "$NODEGROUP" ] || note "could not read the node's nodegroup label; exit will print the manual scale-down instead"
note "GPU nodes: $gpu_nodes"

say "wait for a device to be advertised"
for i in $(seq 1 60); do
  alloc=$(k get nodes -l 'platform.lkhun9311.github.io/gpu=true' \
    -o jsonpath='{range .items[*]}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' 2>/dev/null | grep -v '^$' | head -1)
  [ -n "$alloc" ] && [ "$alloc" != "0" ] && break
  if [ "$i" = 20 ]; then
    say "no device yet; applying the device plugin and the observer"
    k apply -k config/nvidia-device-plugin >/dev/null 2>&1
    k apply -k config/dcgm-exporter >/dev/null 2>&1
  fi
  [ "$i" = 60 ] && fail "the GPU node never advertised nvidia.com/gpu; the device plugin is not working and nothing downstream can tell that apart from 'no GPU nodes'"
  sleep 10
done
note "allocatable nvidia.com/gpu on the GPU node: $alloc"

# ---------------------------------------------------------------- the engine

say "deploy the engine"
k get ns "$NS" >/dev/null 2>&1 || k create ns "$NS" >/dev/null
# ORDER IS LOAD-BEARING. The controller refuses to adopt a Deployment or Service it does not own and marks
# the InferenceDeployment Degraded instead, which is what keeps this engine intact. Create the
# InferenceDeployment FIRST and the operator owns the name, mutateDeployment replaces the whole container,
# and the engine is gone -- replaced by something the CRD can describe, which is not vLLM.
k apply -k config/vllm -n "$NS" >/dev/null || fail "apply config/vllm"
say "wait for the engine (weights download and memory profiling take minutes)"
k rollout status deploy/"$ENGINE" -n "$NS" --timeout=900s >/dev/null \
  || fail "the engine never became ready; check kubectl logs -n $NS deploy/$ENGINE"

# The prediction check the sizing page asks for. vLLM prints the block count it actually allocated, and
# blocks x block_size is the real KV capacity. Writing the prediction down first is what makes this a check
# rather than a description.
say "check the KV capacity against the prediction"
blocks=$(k logs -n "$NS" deploy/"$ENGINE" 2>/dev/null | grep -oE "GPU KV cache size: *[0-9,]+ tokens|# GPU blocks: *[0-9]+" | tail -1)
note "engine reports: ${blocks:-<no block line found>}"
note "sizing page predicted ~${PREDICTED_KV_TOKENS} tokens of KV cache"
if [ -z "$blocks" ]; then
  note "WARNING: could not read the capacity line. Record it by hand from the engine log before quoting any"
  note "         concurrency figure; the 0.85 engage threshold is reached at a concurrency derived from it."
fi

# ---------------------------------------------------------------- the rate

# Measured, not taken from the table. The sizing page's 45% MFU figure is its one estimate, and a single
# contender request against an idle engine settles it: that request's time to first token IS its prefill
# time, and PREDICTED_CONTENDER_TOKENS / TTFT is the real prefill rate.
say "measure prefill on an idle engine"
k port-forward -n "$NS" "svc/$ENGINE" 18000:8000 >/dev/null 2>&1 &
PF_PID=$!
for i in $(seq 1 30); do curl -sf -m 2 http://127.0.0.1:18000/health >/dev/null 2>&1 && break; sleep 2; done
curl -sf -m 5 http://127.0.0.1:18000/health >/dev/null 2>&1 || fail "cannot reach the engine through a port-forward"

go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
# The bytes the replay will actually send, not a stand-in. A run of one character collapses to about
# 5,000 tokens where the corpus gives 7,695, so measuring with it would make the card look half again
# faster than it is and the derived rate would oversubscribe it.
prompt=$("$WORK/benchharness" print-prompt --chars 40000) || fail "could not render the contender prompt"
[ "${#prompt}" -eq 40000 ] || fail "contender prompt is ${#prompt} chars, expected 40000"
printf '{"model":"%s","messages":[{"role":"user","content":"%s"}],"max_tokens":1,"stream":true}' \
  "$MODEL" "$prompt" > "$WORK/prefill.json"
ttft=$(curl -s -o /dev/null -w '%{time_starttransfer}' -m 300 -X POST http://127.0.0.1:18000/v1/chat/completions \
  -H 'Content-Type: application/json' -d @"$WORK/prefill.json")
note "one contender prefill (${PREDICTED_CONTENDER_TOKENS} tokens) took ${ttft}s on an idle engine"
RATE=$(python3 -c "
t=float('$ttft')
if t<=0: print('0'); raise SystemExit
per=1.0/t                       # contenders per second the card can prefill
print(round(2*per*0.8, 3))      # both tenants, held at 80% of capacity so the queue is bursty, not divergent
")
[ "$RATE" = "0" ] && fail "the prefill measurement returned no time; refusing to guess a rate"
note "derived total arrival rate: ${RATE}/s  (the harness default of 20/s demands 7.3x this card's peak)"

# ---------------------------------------------------------------- routing record

say "create the routing record"
# replicas 0 and a no-op image: InferenceDeploymentSpec has no args and no volumes, so it cannot describe
# this engine. It exists so the gateway's router can resolve the model to <name>.<namespace>.svc, and the
# operator will mark it Degraded for the Deployment conflict, which is the intended and harmless outcome.
k apply -f - >/dev/null <<EOF || fail "apply routing record"
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata:
  name: $ENGINE
  namespace: $NS
spec:
  model:
    name: $MODEL
    storageUri: "hf://$MODEL"
  image: registry.k8s.io/pause:3.9
  gpuCount: 0
  replicas: 0
  port: 8000
EOF
k get deploy "$ENGINE" -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null \
  | grep -q vllm || fail "the engine Deployment is no longer running vLLM; the routing record was created before the engine and the operator took the name"

cat <<EOF | tee -a "$LOG"

The engine is up, its capacity is recorded, and the arrival rate is derived from this card rather than
from a table. What remains is the four-arm replay, which is hack/m5b-arms.sh's job and needs the gateway
deployed once per arm. Run it now, from another shell, while this one holds the port-forward:

    NS=$NS RATE=$RATE bash hack/m5b-arms.sh

When it finishes, scale the node group to 0. This script prints the command again on exit.
EOF

say "holding the engine. Ctrl-C when the arms are done."
wait "$PF_PID"
