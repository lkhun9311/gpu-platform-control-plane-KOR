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
# What a trap cannot do, stated so it is not mistaken for solved: it does not run after the laptop dies,
# loses power, or has its shell killed with SIGKILL, and it does not run usefully once the SSO session
# expires -- the scale-down is itself an AWS call, so the credential that stops the billing dies with the
# session.
#
# That is why this script registers a deadline with EventBridge Scheduler before it goes any further, and
# refuses to continue if it cannot. See hack/lib/gpu-ttl.sh. The nightly destroy workflow is the second
# backstop behind it, and as of 2026-08-31 it has run once, on an empty account.
gpu_scale_down() {
  # The deadline is NOT dropped here, and the ordering is the whole point.
  #
  # Removing it first means a scale-down that then fails leaves the account with a billing GPU and no
  # remaining backstop -- the one arrangement worse than either failure alone. The schedule costs nothing
  # while it waits and does nothing once the node is already at zero, so there is no reason to trade it away
  # before the thing it protects against is known not to have happened. It comes off at the bottom of this
  # function, after desiredSize has been read back as 0, and nowhere else.
  #
  # KEEP_NODE=1 keeps the node for a re-run of hack/m5b-arms.sh against a warm engine. It is not a request
  # to bill forever: the deadline stays armed, so a forgotten re-run still ends. Those were entangled before
  # -- one flag disabled both the scale-down and the thing that catches a forgotten scale-down.
  if [ "${KEEP_NODE:-}" = "1" ]; then
    echo "KEEP_NODE=1: leaving $NODEGROUP up for a re-run. The TTL deadline stays armed; it bills until then." >&2
    return 0
  fi

  CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"

  # Ask EKS what is running, rather than trusting a flag this function may never have seen set.
  #
  # SCALED_UP was set after the preflight checks, and the checks run before the node group is even
  # identified -- so the likely first-session sequence was: scale up by hand, re-run, fail on a missing CRD,
  # and exit through a cleanup that believed nothing had been started. The node billed anyway.
  #
  # A flag records what this process did. The desired size records what the account is being charged for,
  # and that is the question being asked.
  if [ -z "$NODEGROUP" ] && [ -n "$CLUSTER" ]; then
    for ng in $(aws eks list-nodegroups --cluster-name "$CLUSTER" \
                  --query 'nodegroups[?starts_with(@, `gpu`)]' --output text 2>/dev/null); do
      size=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
               --query 'nodegroup.scalingConfig.desiredSize' --output text 2>/dev/null)
      if [ -n "$size" ] && [ "$size" != "0" ] && [ "$size" != "None" ]; then
        echo "found $CLUSTER/$ng at desiredSize=$size without this session having recorded it" >&2
        # No break. Taking the first one and stopping meant a second running group was discovered, ignored,
        # and left billing -- by the function whose entire job is to find what is billing and stop it. The
        # first is adopted as NODEGROUP so the code below reads it back; the rest are scaled down here,
        # because a group nobody is tracking is exactly the one that will not be noticed.
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
  [ -n "$NODEGROUP" ] || return 0
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
  # stderr is kept. It used to go to /dev/null with stdout, so the banner below said the call failed and
  # threw away the sentence saying why -- expired credentials, a denied action and a missing node group all
  # produced the same blank failure, at the one moment the operator needs to know which.
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
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null
  gpu_scale_down
  rm -rf "$WORK"
}
# HUP is in the list because a closed terminal or a dropped SSH connection sends it, and that is not an
# exotic way for a session to end -- it is the ordinary one. The header used to claim this covered "every
# exit path" while omitting the signal most likely to arrive.
#
# It still does not cover SIGKILL, the machine sleeping, or the SSO session expiring, and it cannot: all
# three end the process or its credentials before any handler runs. That is what the TTL registered with
# EventBridge Scheduler is for, and why it is armed before the GPU starts rather than after.
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

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
# Available replicas, not the existence of a name.
#
# This was `get deploy -A -o name | grep -q controller-manager`, which passes on a Deployment that exists in
# ImagePullBackOff -- and that is precisely what a fresh cluster has, because config/manager/manager.yaml
# still names controller:latest and nothing has ever pushed an operator image. It also matched any
# deployment whose name happens to contain the string.
#
# The gateway resolves backends from the CR directly, so nothing downstream would have noticed the operator
# never ran: the session would spend a card and produce numbers from a control plane that was not running.
op_ns=$(k get deploy -A -o jsonpath='{range .items[?(@.metadata.name=="gpu-platform-controller-manager")]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | head -1)
[ -n "$op_ns" ] || op_ns=$(k get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null \
  | awk '$2 ~ /controller-manager$/ {print $1; exit}')
[ -n "$op_ns" ] || fail "no operator controller-manager Deployment exists on this cluster"
op_ready=$(k get deploy -n "$op_ns" -o jsonpath='{range .items[?(@.status.availableReplicas>0)]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
  | grep -c 'controller-manager$' || true)
[ "${op_ready:-0}" -ge 1 ] \
  || fail "the operator controller-manager exists in namespace $op_ns but has no available replica. A Deployment stuck in ImagePullBackOff satisfies a name check and reconciles nothing; look at: kubectl -n $op_ns get pods"

# The engine must land on a GPU node, and the node must advertise a device. Both are the operator's
# problem in production and neither is checked anywhere else, so they are checked here where a wrong
# answer costs money rather than a test run.
# The deadline is registered BEFORE the card exists, and that ordering is the reason this script scales the
# node group up itself instead of telling the operator to.
#
# It used to refuse when no GPU node was present, print the aws command, and wait to be re-run. So the
# sequence was: a person scales up, the node bills, the node takes minutes to join, the person re-runs, and
# only then is a deadline registered. Every minute of that is a billing card with no backstop, and it ends
# badly in the ordinary way -- the re-run is forgotten, or the laptop closes between the two steps.
#
# Naming the group before a node exists means it cannot be read off a node label, so it comes from a default
# that is verified against EKS rather than assumed. describe-nodegroup failing is the honest stop: a name
# that does not resolve would produce a schedule that is accepted and then fires into nothing.
CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"
[ -n "$CLUSTER" ] || fail "could not determine the cluster name from the kubeconfig context"
# Named into a candidate first, and promoted to NODEGROUP only once EKS confirms it exists.
#
# Assigning NODEGROUP before the check meant that when the check refused, the EXIT trap's scale-down saw a
# node group name, skipped discovery, and called update-nodegroup-config against a group just proven not to
# exist -- then printed THE NODE IS STILL BILLING for a session that had started nothing. A commit claimed
# no refusal path calls update-nodegroup-config; this was the path that did.
NG_CANDIDATE="${NODEGROUP:-gpu_single}"
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
ttl_arm "$CLUSTER" "$NODEGROUP" "${TTL_MINUTES:-120}" \
  || fail "could not register the TTL scale-down; refusing to start a card with no deadline"
note "deadline registered for $CLUSTER/$NODEGROUP before anything was started"

# The nightly teardown is a second clock, and it is not the deadline this session just armed.
#
# .github/workflows/destroy.yml runs at 03:00 UTC, which is noon in KST. It terraform-destroys the whole
# cluster, not just the node group, so a session started on a Korean morning is deleted underneath itself --
# node, engine, records in flight and all -- by a workflow that has nothing to do with this run and cannot
# see it. The TTL deadline does not protect against this: it scales a node group down, while this removes
# the cluster the node group is in.
#
# Cron drift is real here (this workflow has been observed firing between 03:53 and 14:57 UTC), so the check
# is a warning band rather than a precise boundary, and it refuses only when the collision is certain.
destroy_utc_hour=3
now_min=$(( $(date -u '+%H') * 60 + $(date -u '+%M') ))
destroy_min=$(( destroy_utc_hour * 60 ))
[ "$destroy_min" -le "$now_min" ] && destroy_min=$(( destroy_min + 1440 ))
until_destroy=$(( destroy_min - now_min ))
note "the nightly destroy fires in about ${until_destroy} min (03:00 UTC, noon KST)"
if [ "$until_destroy" -lt 120 ]; then
  [ "${IGNORE_NIGHTLY_DESTROY:-}" = "1" ] \
    || fail "the nightly destroy workflow fires in about ${until_destroy} min and removes this whole cluster, not just the node group -- the TTL deadline does not cover that. Start after it has run, disable the schedule for today, or set IGNORE_NIGHTLY_DESTROY=1 if you have confirmed it will not fire."
  note "IGNORE_NIGHTLY_DESTROY=1: continuing into the destroy window"
fi

gpu_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu=true' -o name 2>/dev/null | wc -l)
if [ "$gpu_nodes" -eq 0 ]; then
  say "no GPU node present; scaling $NODEGROUP to 1 -- the card starts billing here, and the deadline is already registered"
  aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
    --scaling-config minSize=0,maxSize=1,desiredSize=1 >/dev/null \
    || fail "could not scale $NODEGROUP up"
  SCALED_UP=1
  say "wait for the node to join (this takes a few minutes)"
  for i in $(seq 1 60); do
    gpu_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu=true' -o name 2>/dev/null | wc -l)
    [ "$gpu_nodes" -gt 0 ] && break
    [ "$i" = 60 ] && fail "the node never joined after ten minutes. It may still be billing: the deadline will scale it down, or scale it now with aws eks update-nodegroup-config --cluster-name $CLUSTER --nodegroup-name $NODEGROUP --scaling-config minSize=0,maxSize=1,desiredSize=0"
    sleep 10
  done
fi

# The node group name comes from the node the session just qualified, not from a constant.
#
# EKS stamps eks.amazonaws.com/nodegroup on every managed-node-group member, so the node this session is
# about names the group that must be scaled back down. A hardcoded "gpu_single" would be wrong the moment a
# group is renamed, and wrong silently: the scale-down would return a ResourceNotFoundException that nobody
# reads, on the exit path of the script that spends money.
# Cross-check rather than re-derive. The deadline already names a group; if the node that actually joined
# belongs to a different one, the schedule would scale down a group this session is not using while the one
# it is using bills on. That is a worse state than either name being wrong on its own, so it stops here.
node_ng=$(k get node -l 'platform.lkhun9311.github.io/gpu=true' \
  -o jsonpath='{.items[0].metadata.labels.eks\.amazonaws\.com/nodegroup}' 2>/dev/null)
if [ -n "$node_ng" ] && [ "$node_ng" != "$NODEGROUP" ]; then
  fail "the deadline names $NODEGROUP but the GPU node that joined belongs to $node_ng. The schedule would scale down a group this session is not using. Re-run with NODEGROUP=$node_ng."
fi
[ -n "$node_ng" ] || note "the node carries no nodegroup label; the deadline still names $NODEGROUP"
note "GPU nodes: $gpu_nodes"

# Register the deadline with AWS before anything else, because everything after this point can be cut short
# in a way no handler here survives: the laptop sleeping, the shell being SIGKILLed, the SSO session
# expiring mid-wait. The trap below covers the exits this process gets to observe; the schedule covers the
# ones it does not.
#
# Refusing to continue when the deadline cannot be registered is the point of the ordering. A GPU with no
# deadline is the state this whole file exists to prevent, and it is exactly what a warn-and-continue would
# produce.
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

# Clear a previous attempt's verdict before waiting on this one.
#
# `kubectl apply` on an unchanged spec is a no-op, so a Deployment left by an earlier session keeps its
# status -- including Progressing=False/ProgressDeadlineExceeded. `rollout status` reads that condition and
# returns the failure IMMEDIATELY, so a session that had just scaled up a fresh node was refused one minute
# in, with a message telling the operator to look at engine logs that were three hours old.
#
# Restarting the rollout resets the condition and starts a new ReplicaSet against the node this session
# brought up, which is the thing being measured. A session is a fresh measurement or it is nothing.
if k get deploy "$ENGINE" -n "$NS" >/dev/null 2>&1; then
  prev=$(k get deploy "$ENGINE" -n "$NS" \
    -o jsonpath='{range .status.conditions[?(@.type=="Progressing")]}{.status}{" "}{.reason}{end}' 2>/dev/null)
  case "$prev" in
    *ProgressDeadlineExceeded*|*False*)
      note "the engine deployment carries a previous attempt's failure (${prev}); restarting it so this session is measured on its own node"
      k rollout restart deploy/"$ENGINE" -n "$NS" >/dev/null 2>&1 || true ;;
  esac
fi

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

# This used to print a warning and carry on, which contradicted the rule stated at the top of this file:
# refuse rather than improvise. The warning was also the wrong shape of answer. Everything this session
# produces is read against a concurrency, and the concurrency at which the 0.85 threshold engages is derived
# from this number -- so an engine with a different KV capacity than the one the sizing page assumed does not
# make the run slightly less precise, it makes every concurrency figure describe a card that was not rented.
#
# Both log formats are parsed because vLLM has printed both: a token count directly, and a block count that
# has to be multiplied by the block size. Guessing which one is present is how a check quietly reads 0.
kv_tokens=""
case "$blocks" in
  *"KV cache size"*) kv_tokens=$(printf '%s' "$blocks" | grep -oE '[0-9,]+' | tr -d ',') ;;
  *"GPU blocks"*)    kv_tokens=$(( $(printf '%s' "$blocks" | grep -oE '[0-9]+' | tail -1) * ${KV_BLOCK_SIZE:-16} )) ;;
esac
[ -n "$kv_tokens" ] && [ "$kv_tokens" -gt 0 ] 2>/dev/null \
  || fail "could not read the engine's KV capacity from its log. Every concurrency this session reports is derived from it, so there is nothing to report without it. Look at: kubectl logs -n $NS deploy/$ENGINE"

# A factor rather than a percentage, because the prediction is a sizing estimate and being 20% out is
# ordinary. Being half or double is the case the header calls "nothing like" the prediction.
tol="${KV_TOLERANCE:-2}"
lo=$(( PREDICTED_KV_TOKENS / tol )); hi=$(( PREDICTED_KV_TOKENS * tol ))
note "engine KV capacity: ${kv_tokens} tokens (accepted band ${lo}-${hi})"
if [ "$kv_tokens" -lt "$lo" ] || [ "$kv_tokens" -gt "$hi" ]; then
  fail "the engine allocated ${kv_tokens} tokens of KV cache but the sizing page predicted ${PREDICTED_KV_TOKENS}. The concurrency at which the 0.85 threshold engages is derived from this number, so every figure this run would produce describes a card other than the one being paid for. Fix the sizing prediction or the engine's gpu-memory-utilization before spending the card, or set KV_TOLERANCE if the band is genuinely too tight."
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

# A unique prefix, so this measures prefill rather than a cache lookup.
#
# config/vllm now passes --no-enable-prefix-caching and that is the real fix. This is the second one,
# because the measurement that was wrong was wrong SILENTLY: the engine returned 200 with a completion in
# 93.7 ms, every check in this probe passed, and the derived rate came out ten times too high. Nothing here
# could tell a computed prefill from a cache lookup.
#
# The prefix costs a handful of tokens against 7,695 and removes the dependency on a flag set in another
# file. If that flag is ever dropped again, this still measures work. The length assertion above stays
# correct because it reads ${#prompt} after this line.
prompt="m5b-$(date -u +%s%N) $prompt"
note "prefill probe prompt is ${#prompt} chars, prefixed so a warm cache cannot answer it"
# Built by a JSON encoder, not by printf. The corpus this prompt is tiled from ends each tile with a
# newline, so a 40,000-character prompt carries 24 of them, and a raw newline inside a JSON string is an
# invalid control character. Every prefill probe this repository has ever sent was malformed.
#
# It was invisible because of what it broke into. vLLM answers 400, and the probe used to read only the
# time to first byte -- so the measurement did not fail, it succeeded with the response time of an error
# and derived an arrival rate thousands of times too high. Adding the status check turned the same defect
# into a refusal, which is better and still wrong: the refusal arrives after the node is up and the engine
# is warm, and its message blames the engine.
python3 -c '
import json, sys
model, prompt = sys.argv[1], sys.stdin.read()
json.dump({"model": model,
           "messages": [{"role": "user", "content": prompt}],
           "max_tokens": 1, "stream": False}, open(sys.argv[2], "w"))
' "$MODEL" "$WORK/prefill.json" < <(printf '%s' "$prompt") || fail "could not build the prefill request body"
# Check the body that will be SENT, not the string it was built from. A here-string would have appended a
# newline and made the request one character longer than the length asserted above -- small enough not to
# matter to the token count, and exactly the kind of gap between what is verified and what is transmitted
# that this file keeps finding elsewhere.
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
sent = len(d["messages"][0]["content"])
if sent != int(sys.argv[2]):
    raise SystemExit(f"the request body carries {sent} characters, not {sys.argv[2]}")
' "$WORK/prefill.json" "${#prompt}" \
  || fail "the prefill request body is not valid JSON, or does not carry the prompt that was measured; the engine would answer 400 and there would be nothing to measure"
# NOT streamed, and time_total rather than time_starttransfer.
#
# The premise here was that a request's time to first token is its prefill time. It is not, for a streamed
# response: time_starttransfer is time to first BYTE, and vLLM emits the opening SSE chunk when it schedules
# the request rather than when the prefill finishes. Measured on the card, unique prompt, caching off:
#
#   stream=true    TTFB 0.057 s   total 1.006 s
#   stream=false   TTFB 1.003 s   total 1.003 s
#
# So the probe read a scheduling latency of about 60 ms as if it were a second of prefill, and derived an
# arrival rate seventeen times too high -- which would have censored every tail and disqualified the run
# after the card was paid for. With max_tokens=1 the non-streamed total is the prefill plus one decode step,
# which is the number this measurement is for.
#
# curl reports a time for a 404 as readily as for a completion, and an error comes back fast. Discarding the
# body to /dev/null and never reading the status meant a failed probe did not lose the measurement -- it
# produced one several times too high, which then set the arrival rate for every arm, oversubscribed the
# card, censored every tail, and got the run disqualified by EvaluateChecks after the card had been paid for.
#
# So the body is kept and three things are checked: that curl itself succeeded, that the engine answered
# 200, and that what came back is a completion rather than an error object that happens to be valid JSON.
probe=$(curl -s -o "$WORK/prefill.out" -w '%{http_code} %{time_total}' -m 300 \
  -X POST http://127.0.0.1:18000/v1/chat/completions \
  -H 'Content-Type: application/json' -d @"$WORK/prefill.json")
rc=$?
[ "$rc" -eq 0 ] || fail "the prefill probe did not complete (curl exit $rc). A timeout or a dropped port-forward here would otherwise be measured as a prefill time."
http_code=${probe%% *}; ttft=${probe##* }
[ "$http_code" = "200" ] || fail "the prefill probe returned HTTP $http_code, and an error's response time is not a prefill time: $(head -c 300 "$WORK/prefill.out" 2>/dev/null)"
grep -q '"choices"' "$WORK/prefill.out" 2>/dev/null \
  || fail "the prefill probe returned 200 but no completion. The engine answered something other than the request that was sent: $(head -c 300 "$WORK/prefill.out" 2>/dev/null)"
note "one contender prefill (${PREDICTED_CONTENDER_TOKENS} tokens) took ${ttft}s on an idle engine"
RATE=$(python3 -c "
t=float('$ttft')
if t<=0: print('0'); raise SystemExit
per=1.0/t                       # contenders per second the card can prefill
print(round(2*per*0.8, 3))      # both tenants, held at 80% of capacity so the queue is bursty, not divergent
")
# The guard was "$RATE" = "0", which only catches the one case the python prints. A non-numeric ttft makes
# float() raise, python exits with a traceback on stderr, and RATE is left EMPTY -- which is not "0", so the
# run continued with no rate at all.
case "$RATE" in
  ''|*[!0-9.]*) fail "the prefill measurement produced no usable rate (ttft was '${ttft}'); refusing to guess one" ;;
esac
[ "$RATE" = "0" ] && fail "the prefill measurement returned no time; refusing to guess a rate"
note "derived total arrival rate: ${RATE}/s  (the harness default of 20/s demands 7.3x this card's peak)"

# Written down, because the alternative is a human retyping it into another shell.
#
# hack/m5b-arms.sh only checks that RATE is non-empty. Typing 11.4 for 1.14 passes every guard,
# oversubscribes the card about tenfold, censors every tail, and disqualifies the run after the card has
# been paid for -- and the number is transcribed across a shell boundary by hand at the one moment the
# operator is busiest.
RATE_FILE="${RATE_FILE:-$PWD/.m5b-rate}"
printf '%s\n' "$RATE" > "$RATE_FILE" || fail "could not write the measured rate to $RATE_FILE"
note "measured rate written to $RATE_FILE; hack/m5b-arms.sh reads it, so it does not have to be retyped"

# Extend the deadline to cover the run that was just sized, because until this point nobody could size it.
#
# The deadline is a spending bound, not a scientific one. It was armed at a default of 120 minutes before
# the card existed, and how long the arms actually take is decided here: 16 replays of 500/(RATE/2) seconds
# each, plus a gateway rollout apiece. At the rate an A10G really gives -- about 1.14/s, which is what the
# prefill above measures -- that is 282 minutes. The session would therefore warm the engine, hand off to
# hack/m5b-arms.sh, and be refused by its fit guard with the card already paid for and no way to re-arm
# without ending the session and scaling the node away.
#
# So the deadline is re-armed here for what the run needs. MAX_TTL_MINUTES is the ceiling that keeps this
# from becoming an open tab: past it the session refuses instead, because a deadline long enough to hide a
# forgotten node is not a deadline.
# The replay count is read from hack/m5b-arms.sh rather than written here. It was a literal 16 -- four arms
# times the default four repetitions -- which is right only while that default is four and while nobody sets
# REPS. Set REPS=2 and this asked for twice the deadline the run needed; raise it and the deadline would
# have been too short for the run it was sizing, which is the direction that costs a session.
#
# This is the eighth or ninth time in this repository that a value carried by hand across two call sites
# has been found disagreeing with itself, and it was introduced yesterday by the change that added this
# block. A unit test now pins it.
arms_reps="${REPS:-$(sed -n 's/^REPS="${REPS:-\([0-9]*\)}"$/\1/p' hack/m5b-arms.sh)}"
[ -n "$arms_reps" ] || fail "could not read the repetition count from hack/m5b-arms.sh, so the deadline cannot be sized for the run"
# The completions target is read from hack/m5b-arms.sh for the same reason the repetition count is: it is
# the other half of the replay length, and it sat as a literal 500 in both files with nothing failing when
# they disagreed. Lowering it in the harness to shorten a run -- which is the cheaper of the two ways to buy
# a shorter session -- would otherwise have left this sizing the deadline for the old, longer run.
arms_target=$(sed -n 's/.*print(int(\([0-9]*\) \/ (float.*/\1/p' hack/m5b-arms.sh)
[ -n "$arms_target" ] || fail "could not read the per-replay completion target from hack/m5b-arms.sh, so the deadline cannot be sized for the run"
arms_min=$(python3 -c "
rate = float('$RATE')
replay = $arms_target / (rate / 2)   # seconds per replay, the same derivation hack/m5b-arms.sh uses
replays = 4 * $arms_reps             # four arms, the same product hack/m5b-arms.sh computes
print(int((replays * (replay + 180)) / 60) + 1)")
needed_min=$(( arms_min * 12 / 10 + 20 ))   # a fifth of headroom, plus the handoff and teardown
# 420, not 360. At the rate an A10G is expected to give, the design's own default asks for 358 minutes --
# two minutes under the old ceiling -- so a card measuring below about 1.13/s would have been refused at the
# re-arm step with the engine already warm. A ceiling that refuses the configuration the repository ships is
# the same defect as the matrix budgeting 456 minutes against a 240-minute deadline, one file over.
#
# It is still a ceiling and still refuses: 420 minutes of g5.xlarge is about $8.7, which is the honest price
# of this design at four repetitions, and anything past it means the card is slow enough that the bill has
# become the point.
max_min="${MAX_TTL_MINUTES:-420}"
note "the four arms at ${arms_reps} repetitions need about ${arms_min} min at ${RATE}/s; asking for a ${needed_min} min deadline"
if [ "$needed_min" -gt "$max_min" ]; then
  # No dollar figure. This node group is Spot (infra/aws/cluster/vpc.tf:58), so its price is a market
  # number between roughly $0.36 and $0.42 rather than the $1.237 On-Demand rate this message used to
  # multiply by -- which overstated the bill about threefold at the exact moment the operator decides
  # whether to raise the ceiling. Hours are what this script actually knows.
  fail "this card is slow enough that the arms need about ${needed_min} min -- $(( needed_min / 60 ))h$(( needed_min % 60 ))m -- over the ${max_min} min ceiling. Raise MAX_TTL_MINUTES deliberately, or lower REPS in hack/m5b-arms.sh."
fi
if [ "$needed_min" -gt "${TTL_MINUTES:-120}" ]; then
  TTL_REPLACE=1 ttl_arm "$CLUSTER" "$NODEGROUP" "$needed_min" \
    || fail "could not extend the deadline to ${needed_min} min. The arms would be refused by their own fit guard, so this session stops here rather than warming a card it cannot use."
  note "deadline extended to ${needed_min} min"
fi
if [ "$needed_min" -gt 240 ]; then
  note "WARNING: ${needed_min} min is longer than one SSO session. The deadline is registered with AWS and"
  note "         does not need this laptop, but the scale-down at the end of hack/m5b-arms.sh does -- run"
  note "         aws sso login again before it finishes, or let the deadline do the teardown."
fi

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
