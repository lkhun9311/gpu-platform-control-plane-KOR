#!/usr/bin/env bash
#
# The sharing matrix: does giving each tenant its own engine beat giving them one to share?
#
# That is the question, and it is the same mechanism M5-b measures seen from the other side. In the shared
# arm two tenants multiplex one vLLM, so they share a KV cache and the long-context tenant's occupancy is
# what costs the other one latency -- the pressure the admission guard exists to relieve. In the
# time-sliced arm each tenant gets its own engine with its own cache on half the card, so the KV coupling is
# gone and what remains is contention for the SMs. Neither is obviously better, which is why it is worth
# measuring rather than asserting.
#
#   shared        one engine, whole card, both tenants routed to it        (M5-b's topology)
#   timeSlicing   two engines, half card each, one tenant routed to each
#   mps           the same two engines, sharing through MPS instead
#
# MPS is not a third topology, it is a second mechanism for the same one, and the difference is worth
# stating because it is the reason both arms exist. Time-slicing interleaves kernels and does NOT partition
# memory -- the two engines draw on one pool and only convention keeps their utilizations summing below 1.
# MPS runs their kernels concurrently in one context AND caps each client's memory: the pinned control
# daemon issues set_default_device_pinned_mem_limit, read out of the binary rather than the documentation.
# So the arms differ in whether the tenants contend for SM time serially or concurrently, and in whether
# their memory ceiling is enforced or agreed.
#
# The routing is built from mechanisms this control plane already has: the gateway resolves a backend from
# the requesting tenant's GPUQuotaPolicy targetNamespace, so putting each engine in its own namespace and
# pointing one policy at each is all it takes. No new code, and the arm is a deployment difference rather
# than a code path.
#
# What this does NOT report is per-engine GPU utilisation. Under time-slicing a busy SM belongs to no single
# Pod and DCGM cannot attribute it -- config/nvidia-device-plugin/daemonset.yaml says so, and it is why the
# sharing node deliberately has no observer. The matrix reports what the CLIENTS measured, which is
# unambiguous and is also what a tenant actually experiences.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

KCTX="${KCTX:-$(kubectl config current-context 2>/dev/null)}"
NS_A="${NS_A:-m5c-a}"
NS_B="${NS_B:-m5c-b}"
MODEL="Qwen/Qwen2.5-3B-Instruct"
OUT="${OUT:-hack/m5c-run-$(date +%Y%m%d-%H%M%S)}"
LOG="$OUT/evidence.log"
GW_IMAGE="${GW_IMAGE:-gateway:m5c}"
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
# Which sharing mechanism the shared arm uses. Both are run in a full matrix; a session that only has time
# for one should say which rather than silently getting the default.
ARMS="${ARMS:-shared timeSlicing mps}"

k() { kubectl --context "$KCTX" "$@"; }
say() { echo "== $*" | tee -a "$LOG"; }
fail() { echo "MATRIX FAILED: $*" | tee -a "$LOG" >&2; exit 1; }

[ -n "${RATE:-}" ] || fail "RATE is unset. Measure it from a single contender prefill on THIS card, the way hack/m5b-gpu-session.sh does; the harness default of 20/s demands 3.8x an A10G's theoretical peak and would censor every arm."
[ -n "${REGISTRY:-}" ] || fail "REGISTRY is unset. EKS nodes cannot side-load an image, so the gateway must be pushed where they can pull it."

mkdir -p "$OUT" || fail "cannot create $OUT"
: > "$LOG"

WORK="$(mktemp -d)"
PF_PID=""
NODEGROUP=""

# Scaling the node group back to zero, which this file's header claimed and this function did not do.
#
# It printed a banner. A banner is not a control: it depends on a human reading a terminal that may have
# scrolled, or that no longer exists because the laptop closed. The node group keeps desiredSize=1, the ASG
# keeps a GPU node, and this matrix's g5.xlarge bills $1.237/hour with nothing scheduled on it.
#
# The names are derived rather than asked for, because a prompt in a trap is a prompt nobody answers:
# the cluster comes from the kubeconfig context's EKS ARN and the node group from the node's own
# eks.amazonaws.com/nodegroup label. If either cannot be derived the function says so and prints the manual
# command -- degrading to the old behaviour rather than failing silently.
#
# KEEP_NODE=1 skips it, for a re-run against a node that is already warm rather than paying for a fresh
# warmup. The matrix has no separate re-run script, so this matters less here than in m5b-gpu-session.sh.
#
# What this does NOT solve, stated so it is not mistaken for solved: a trap cannot run after the laptop dies,
# loses power, or has its shell killed with SIGKILL. The backstop for that is the nightly destroy workflow,
# and the deadline this script registers with EventBridge Scheduler is what covers those.
# See hack/lib/gpu-ttl.sh.
gpu_scale_down() {
  # The deadline is NOT dropped here, and the ordering is the whole point.
  #
  # Removing it first means a scale-down that then fails leaves the account with a billing GPU and no
  # remaining backstop -- the one arrangement worse than either failure alone. The schedule costs nothing
  # while it waits and does nothing once the node is already at zero, so there is no reason to trade it away
  # before the thing it protects against is known not to have happened. It comes off at the bottom of this
  # function, after desiredSize has been read back as 0, and nowhere else.
  #
  # KEEP_NODE=1 keeps the node up; the deadline stays armed, so a session someone walks away from still ends.
  if [ "${KEEP_NODE:-}" = "1" ]; then
    echo "KEEP_NODE=1: leaving $NODEGROUP up. The TTL deadline stays armed; it bills until then." >&2
    return 0
  fi

  CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"

  # Ask EKS what is billing rather than trusting a flag set after the preflight checks. The same reasoning is
  # written out in m5b-gpu-session.sh: the refusals that run before the node group is identified used to exit
  # through a cleanup that believed nothing had been started.
  if [ -z "$NODEGROUP" ] && [ -n "$CLUSTER" ]; then
    for ng in $(aws eks list-nodegroups --cluster-name "$CLUSTER" \
                  --query 'nodegroups[?starts_with(@, `gpu`)]' --output text 2>/dev/null); do
      size=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
               --query 'nodegroup.scalingConfig.desiredSize' --output text 2>/dev/null)
      if [ -n "$size" ] && [ "$size" != "0" ] && [ "$size" != "None" ]; then
        echo "found $CLUSTER/$ng at desiredSize=$size without this session having recorded it" >&2
        # No break. The same fix went into hack/m5b-gpu-session.sh and this file was left with the old
        # shape, so a claim that "every stray group is scaled down" was true of one script and not the
        # other. Taking the first and stopping leaves the rest billing, in the function whose only job is
        # to find what is billing and stop it.
        if [ -z "$NODEGROUP" ]; then
          NODEGROUP="$ng"
        else
          aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
            --scaling-config minSize=0,maxSize=1,desiredSize=0 >/dev/null 2>&1 \
            && echo "also scaled $CLUSTER/$ng to zero" >&2 \
            || echo "WARNING: could not scale $CLUSTER/$ng down. IT IS STILL BILLING." >&2
        fi
      fi
    done
  fi

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
  # stderr is kept, so a failed scale-down says why rather than only that.
  if ! aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
        --scaling-config "minSize=0,maxSize=1,desiredSize=0" >/dev/null; then
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
    # Only now. The deadline has nothing left to protect, and this is the single place that knows that.
    command -v ttl_disarm >/dev/null 2>&1 && ttl_disarm
  else
    echo "WARNING: $CLUSTER/$NODEGROUP reports desiredSize=$DESIRED after the scale-down. It is billing." >&2
    echo "The TTL deadline is left armed on purpose. It is what remains." >&2
  fi
}

# A signal must also END the script, and these traps did not.
#
# `trap cleanup INT` runs cleanup and then RESUMES where the signal arrived. During the ten-minute wait for
# a node to join, Ctrl-C therefore scaled the node group to zero, dropped the deadline, and went back to
# waiting for the node it had just cancelled -- for another ten minutes, on a card the operator had just
# asked to stop. The cost was cleaned up; the session was not.
#
# EXIT still runs cleanup for ordinary exits and for `fail`. The signal handlers do their own cleanup and
# exit with the conventional 128+signal status, and the guard makes the second call a no-op so the EXIT
# trap that follows does not repeat the work.
CLEANED=0
cleanup() {
  [ "$CLEANED" = "1" ] && return 0
  CLEANED=1
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  k delete namespace "$NS_A" "$NS_B" --wait=false >/dev/null 2>&1
  k delete gpuquotapolicy m5c-premium m5c-standard >/dev/null 2>&1
  k delete clusterrolebinding m5c-gateway-role >/dev/null 2>&1
  rm -rf "$WORK"
  gpu_scale_down
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

say "preflight"
case "$KCTX" in kind-*) fail "context $KCTX is a kind cluster; this is the paid matrix" ;; esac

# The sharing node, and it must be the sharing one: the exclusive plugin advertises one device per card, so
# the second engine would sit Pending and the arm would silently become a one-engine arm with half the memory.
# The deadline is registered BEFORE the card exists, which is why this script scales the group up itself.
#
# It used to refuse when no sharing node was present, print the aws command, and wait to be re-run -- so a
# person scaled up, the node billed and took minutes to join, and only a re-run registered a deadline. This
# is the longest run in the repository and therefore the one most likely to outlive the shell that started
# it; starting it with an unbacked card was the wrong end to be careless at.
#
# The name cannot come off a node label before a node exists, so it comes from a default verified against
# EKS. A name that does not resolve would produce a schedule that is accepted and then fires into nothing.
CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"
[ -n "$CLUSTER" ] || fail "could not determine the cluster name from the kubeconfig context"
# Named into a candidate first, and promoted to NODEGROUP only once EKS confirms it exists.
#
# Assigning NODEGROUP before the check meant that when the check refused, the EXIT trap's scale-down saw a
# node group name, skipped discovery, and called update-nodegroup-config against a group just proven not to
# exist -- then printed THE NODE IS STILL BILLING for a session that had started nothing. A commit claimed
# no refusal path calls update-nodegroup-config; this was the path that did.
NG_CANDIDATE="${NODEGROUP:-gpu_shared}"
aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$NG_CANDIDATE" >/dev/null 2>&1 \
  || fail "no node group $NG_CANDIDATE in $CLUSTER. The deadline must name a group that exists, or it fires into nothing. Set NODEGROUP if the name is different."
NODEGROUP="$NG_CANDIDATE"

# One schedule names one node group, so a second GPU group left running is not covered by anything this
# session registers. The deadline would fire, scale down the group it names, and leave the other billing --
# and nothing in the run would look wrong.
#
# Refusing here rather than trying to cover both is deliberate. A session that finds another card already
# running does not know whose it is: it may be another session mid-run, and scaling it down would destroy
# someone else's paid work. Naming it and stopping is the only answer that is right in both cases.
# Every AWS call here is checked, because the previous shape failed OPEN. It ran the listing inside a
# command substitution, threw away stderr and the exit status, and iterated the result -- so expired
# credentials, a permissions error, or a network failure all produced an empty list, which reads exactly
# like "no other card is running". The same held one level down: a describe that failed left sz empty,
# which the case treated as zero.
#
# That is the wrong direction for this particular guard. It exists for the moments when something is wrong
# with the account, and those are the moments an unchecked call returns nothing.
if ! ng_list=$(aws eks list-nodegroups --cluster-name "$CLUSTER" \
                 --query 'nodegroups[?starts_with(@, `gpu`)]' --output text 2>&1); then
  fail "could not list the node groups in $CLUSTER, so this session cannot tell whether another card is already running: $ng_list"
fi
others=""
for ng in $ng_list; do
  [ "$ng" = "$NODEGROUP" ] && continue
  if ! sz=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
              --query 'nodegroup.scalingConfig.desiredSize' --output text 2>&1); then
    fail "could not read the size of $CLUSTER/$ng: $sz. An unreadable node group is not a stopped one."
  fi
  case "$sz" in
    0|None) ;;
    ''|*[!0-9]*) fail "the size of $CLUSTER/$ng came back as ${sz@Q}, which is not a number. Refusing rather than reading it as zero." ;;
    *) others="$others $ng($sz)" ;;
  esac
done
[ -z "$others" ] || fail "another GPU node group is already running:$others. This session's deadline names only $NODEGROUP, so that card would keep billing after the deadline fires. Scale it to zero, or if another session owns it, wait for it."

TTL_ROLE_ARN="${TTL_ROLE_ARN:-$(terraform -chdir=infra/aws/bootstrap output -raw ttl_scaledown_role_arn 2>/dev/null)}"
export TTL_ROLE_ARN
# shellcheck source=hack/lib/gpu-ttl.sh
. "$(dirname "$0")/lib/gpu-ttl.sh"
if true; then
  ttl_arm "$CLUSTER" "$NODEGROUP" "${TTL_MINUTES:-240}" \
    || fail "could not register the TTL scale-down; refusing to start a card with no deadline"

  # What is checked here, and what is not.
  #
  # The first shape of this check multiplied the per-cell TIMEOUT budget by the cell count and refused if
  # the product exceeded the deadline. At the shipped defaults that is 456 minutes against 240, so the
  # design's own repetition count -- four, which a unit test pins because below it the incremental interval
  # is a bootstrap over very few blocks -- could never start. Lowering REPS to make the arithmetic work was
  # the wrong repair: it traded a statistical property for a scheduling one, quietly.
  #
  # Those 1800 seconds are two rollout TIMEOUTS, not two expected rollouts. A budget is what the script
  # waits before giving up, and refusing a run because the worst case does not fit refuses most runs that
  # would have finished. So the pessimistic product is gone and only the floor stays here: a deadline that
  # cannot hold even one cell is a session with nothing to gain. The real bound is measured per cell, in
  # the loop, where a slow matrix stops on a cell boundary with everything before it intact.
  ttl_min="${TTL_MINUTES:-240}"
  cell_floor_min=$(( (1800 + 180 + ${REPLAY_SECONDS:-300} + 59) / 60 ))
  if [ "$ttl_min" -lt "$cell_floor_min" ]; then
    ttl_disarm
    fail "the deadline is ${ttl_min} min but one cell can take up to ${cell_floor_min} min, so this session could not finish even a single cell. Raise TTL_MINUTES."
  fi
fi

# The card starts here, after the deadline exists.
shared_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' -o name 2>/dev/null | wc -l)
if [ "$shared_nodes" -eq 0 ]; then
  say "no sharing node present; scaling $NODEGROUP to 1 -- the card starts billing here, and the deadline is already registered"
  aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
    --scaling-config minSize=0,maxSize=1,desiredSize=1 >/dev/null || fail "could not scale $NODEGROUP up"
  say "wait for the node to join (this takes a few minutes)"
  for i in $(seq 1 60); do
    shared_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' -o name 2>/dev/null | wc -l)
    [ "$shared_nodes" -gt 0 ] && break
    [ "$i" = 60 ] && fail "the node never joined after ten minutes. The deadline will scale it down; to do it now: aws eks update-nodegroup-config --cluster-name $CLUSTER --nodegroup-name $NODEGROUP --scaling-config minSize=0,maxSize=1,desiredSize=0"
    sleep 10
  done
fi

# Cross-check, not re-derive: a node from a different group means the deadline would scale down a group this
# matrix is not using while the one it is using bills on.
node_ng=$(k get node -l 'platform.lkhun9311.github.io/gpu-sharing=true' \
  -o jsonpath='{.items[0].metadata.labels.eks\.amazonaws\.com/nodegroup}' 2>/dev/null)
if [ -n "$node_ng" ] && [ "$node_ng" != "$NODEGROUP" ]; then
  fail "the deadline names $NODEGROUP but the sharing node belongs to $node_ng. Re-run with NODEGROUP=$node_ng."
fi

[ "$shared_nodes" -eq 1 ] || fail "$shared_nodes sharing nodes are up; two engines on two nodes are not sharing a card, and nothing downstream could tell that apart from a sharing result"

# The two sharing overlays select the SAME node, so exactly one may be applied at a time: two plugins
# registering nvidia.com/gpu against one kubelet socket is not a configuration worth debugging on a rented
# card. Deleting the other one first is the mechanism; remembering is not.
apply_sharing_plugin() {
  local mode="$1" other keep
  case "$mode" in
    timeSlicing) keep=config/nvidia-device-plugin-timeslicing; other=config/nvidia-device-plugin-mps ;;
    mps)         keep=config/nvidia-device-plugin-mps;         other=config/nvidia-device-plugin-timeslicing ;;
    *) fail "apply_sharing_plugin: unknown mode $mode" ;;
  esac
  k delete -k "$other" --ignore-not-found --wait=true >/dev/null 2>&1
  k apply -k "$keep" >/dev/null || fail "apply the $mode plugin"

  say "wait for the card to advertise two devices under $mode"
  for i in $(seq 1 60); do
    adv=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' \
      -o jsonpath='{.items[0].status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
    [ "${adv:-0}" -ge 2 ] 2>/dev/null && break
    [ "$i" = 60 ] && fail "the node advertises ${adv:-0} device(s) after applying a $mode config that asks for 2: the plugin is ignoring CONFIG_FILE, and both engines would compete for one device -- which is a one-engine arm wearing a two-engine label"
    sleep 10
  done
  say "node advertises $adv devices for one physical card under $mode"

  # MPS has a second failure that time-slicing does not: the control daemon can be absent or unreachable
  # while the plugin still advertises, and every client then silently runs WITHOUT MPS. An arm that fell
  # back that way is the time-slicing arm under another name, and nothing downstream could tell.
  if [ "$mode" = mps ]; then
    k rollout status ds/nvidia-mps-control-daemon -n system --timeout=180s >/dev/null \
      || fail "the MPS control daemon never became ready; clients would fall back to running without MPS and the arm would be time-slicing under another name"
  fi
}

say "build and push the gateway"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"
go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY gateway /gateway\nUSER 65532:65532\nENTRYPOINT ["/gateway"]\n' > "$WORK/Dockerfile"
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build gateway image"
docker tag "$GW_IMAGE" "$REGISTRY/$GW_IMAGE" && docker push "$REGISTRY/$GW_IMAGE" >/dev/null || fail "push gateway image"
GW_IMAGE="$REGISTRY/$GW_IMAGE"

# One routing record per engine namespace, replicas 0 and a no-op image for the reason
# hack/m5b-gpu-session.sh gives: InferenceDeploymentSpec has no args and no volumes, so it cannot describe a
# vLLM container. The engine Deployment must exist FIRST or the operator takes the name.
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

deploy_arm() {
  local arm="$1"
  k delete namespace "$NS_A" "$NS_B" --wait=true >/dev/null 2>&1
  k create ns "$NS_A" >/dev/null; k create ns "$NS_B" >/dev/null
  case "$arm" in
    shared)
      # One engine with the whole card. Both policies point at the same namespace, so both tenants resolve
      # to it -- which is exactly M5-b's topology and exactly what makes their KV caches one cache.
      k apply -f config/vllm/deployment.yaml -n "$NS_A" >/dev/null || fail "apply the exclusive engine"
      k apply -f config/vllm/service.yaml -n "$NS_A" >/dev/null || fail "apply the exclusive service"
      k rollout status deploy/vllm-qwen25-3b -n "$NS_A" --timeout=900s >/dev/null || fail "the exclusive engine never became ready"
      routing_record "$NS_A" vllm-qwen25-3b
      PREMIUM_NS="$NS_A"; STANDARD_NS="$NS_A"
      ;;
    timeSlicing|mps)
      apply_sharing_plugin "$arm"
      k apply -f config/vllm-shared/engine-a.yaml -n "$NS_A" >/dev/null || fail "apply engine a"
      k apply -f config/vllm-shared/engine-b.yaml -n "$NS_B" >/dev/null || fail "apply engine b"
      k rollout status deploy/vllm-shared-a -n "$NS_A" --timeout=900s >/dev/null || fail "engine a never became ready"
      k rollout status deploy/vllm-shared-b -n "$NS_B" --timeout=900s >/dev/null || fail "engine b never became ready"
      # Both engines must be on the SAME node or they are not sharing a card. max_size 1 should guarantee
      # it; checking is cheap and the failure is invisible in the numbers.
      na=$(k get pod -n "$NS_A" -l app.kubernetes.io/component=vllm-shared -o jsonpath='{.items[0].spec.nodeName}')
      nb=$(k get pod -n "$NS_B" -l app.kubernetes.io/component=vllm-shared -o jsonpath='{.items[0].spec.nodeName}')
      [ -n "$na" ] && [ "$na" = "$nb" ] || fail "the two engines are on different nodes ($na, $nb); that is not sharing a card"
      routing_record "$NS_A" vllm-shared-a
      routing_record "$NS_B" vllm-shared-b
      PREMIUM_NS="$NS_A"; STANDARD_NS="$NS_B"
      ;;
  esac

  k apply -f - >/dev/null <<EOF || fail "policies for $arm"
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5c-premium
  annotations: {platform.lkhun9311.github.io/tier: premium}
spec: {tenant: premium-1, targetNamespace: $PREMIUM_NS, gpuClass: a10g, limits: {gpuCount: 1}}
---
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata: {name: m5c-standard}
spec: {tenant: standard-noisy, targetNamespace: $STANDARD_NS, gpuClass: a10g, limits: {gpuCount: 1}}
EOF

  k create secret generic gateway-api-keys -n "$NS_A" \
    --from-literal=premium-key=premium-1 --from-literal=standard-key=standard-noisy \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k create serviceaccount gateway -n "$NS_A" --dry-run=client -o yaml | k apply -f - >/dev/null
  k create clusterrolebinding m5c-gateway-role --clusterrole=gateway-role \
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

  # Admission OFF in both arms. The matrix varies the TOPOLOGY; leaving the guard on would vary two things
  # and hand the difference to whichever one the reader already believed.
  k apply -f - >/dev/null <<EOF || fail "gateway for $arm"
apiVersion: apps/v1
kind: Deployment
metadata: {name: gateway, namespace: $NS_A}
spec:
  replicas: 1
  selector: {matchLabels: {app: m5c-gateway}}
  template:
    metadata:
      labels: {app: m5c-gateway}
      annotations: {arm: "$arm"}
    spec:
      serviceAccountName: gateway
      containers:
        - name: gateway
          image: $GW_IMAGE
          args: ["-admission-mode=off"]
          env:
            - {name: GATEWAY_NAMESPACE, value: $NS_A}
            - {name: GATEWAY_API_KEY_SECRET, value: gateway-api-keys}
          ports: [{containerPort: 8080, name: http}]
EOF
  k rollout status deploy/gateway -n "$NS_A" --timeout=180s >/dev/null || fail "gateway never became ready for $arm"
}

DURATION_MS=$(python3 -c "print(int(500 / (float('$RATE')/2) * 1000))")
say "rate ${RATE}/s, duration $((DURATION_MS/1000))s per arm, ${REPS} repetitions, output $OUT"

# Measured, like the M6 wrapper's: after the first cell, the elapsed time IS the budget, and it knows the
# node's real speed and how long the rollouts actually took rather than how long they were allowed to take.
cells_total=0; for _a in $ARMS; do cells_total=$(( cells_total + REPS )); done
cell_secs=0; cells_done=0; cell_n=0
cell_deadline_check() {
  local remain per projected
  [ "$cells_done" -eq 0 ] && return 0
  remain=$(ttl_remaining_minutes "$CLUSTER" "$NODEGROUP" 2>/dev/null) || return 0
  [ -n "$remain" ] || return 0
  per=$(( (cell_secs + cells_done - 1) / cells_done ))
  # A fifth of headroom, because cells differ by arm -- the sharing arms roll out two engines and the
  # exclusive arm rolls out one -- and a projection that only just fits is one slow cell from being cut.
  projected=$(( ((cells_total - cells_done) * per * 12 / 10 + 59) / 60 ))
  if [ "$projected" -ge "$remain" ]; then
    echo >&2
    echo "STOPPING: $(( cells_total - cells_done )) cells left at ~$(( per / 60 )) min each needs about ${projected} min," >&2
    echo "  and the deadline fires in ${remain} min. Being cut mid-cell would waste that cell's rollouts and" >&2
    echo "  leave a matrix missing one of the topologies it exists to compare, so it stops on a boundary." >&2
    echo "  ${cells_done} of ${cells_total} cells are complete. Re-arm with a longer TTL_MINUTES to continue." >&2
    return 1
  fi
  return 0
}

for rep in $(seq 1 "$REPS"); do
  for arm in $ARMS; do
    cell_n=$(( cell_n + 1 ))
    cell_deadline_check || exit 1
    CELL_T0=$(date +%s)
    say "rep $rep arm $arm  (cell $cell_n/$cells_total)"
    deploy_arm "$arm"
    [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
    k port-forward -n "$NS_A" deploy/gateway 18080:8080 >/dev/null 2>&1 &
    PF_PID=$!
    sleep 3

    # arm is spelled "off" in the manifest because the harness's allowed arms are the ADMISSION arms; the
    # sharing mode is this run's variable and is recorded in the output path, not in the manifest's arm.
    "$WORK/benchharness" gen-trace --seed 11 --duration-ms "$DURATION_MS" --rate "$RATE" \
      --arm off --gateway-url "http://127.0.0.1:18080" \
      --trace-out "$OUT/trace-$arm-$rep.jsonl" --manifest-out "$OUT/manifest-$arm-$rep.yaml" || fail "gen-trace $arm"
    "$WORK/benchharness" replay --manifest "$OUT/manifest-$arm-$rep.yaml" \
      --target "http://127.0.0.1:18080" \
      --api-keys "premium-1=premium-key,standard-noisy=standard-key" \
      --raw-out "$OUT/raw-$arm-$rep.jsonl" || fail "replay $arm"
    [ -s "$OUT/raw-$arm-$rep.jsonl" ] || fail "no raw evidence for $arm rep $rep"
    say "  $(wc -l < "$OUT/raw-$arm-$rep.jsonl") rows"
    cell_secs=$(( cell_secs + $(date +%s) - CELL_T0 ))
    cells_done=$(( cells_done + 1 ))
  done
done

# The raw files are NOT report inputs, and saying so here is cheaper than the confusion. Both arms replay
# with --arm off, because the harness's arm vocabulary is the ADMISSION arms and the sharing mode is this
# run's variable; feeding both files to `benchharness report` would pool them into one arm and produce a
# number for a comparison that was never made.
cat > "$OUT/README.txt" <<EOF
Raw evidence from the M5-c sharing matrix.

The variable is the SHARING MODE, recorded in each filename, not in the manifests: every row carries
arm="off" because the harness's arm field names admission modes and admission was off in both.

Do NOT pass these to \`benchharness report\`. It would pool both modes into one arm.
Compare raw-shared-*.jsonl against raw-timeSlicing-*.jsonl on premium TTFT instead.
EOF

say "MATRIX DONE. Raw evidence in $OUT (see its README.txt before analysing)."
say "The comparison is premium TTFT p99 across the sharing modes at equal offered load."
say "Per-engine GPU utilisation is deliberately absent: under time-slicing nothing can attribute it."
