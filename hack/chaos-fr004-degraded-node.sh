#!/usr/bin/env bash
# FR-004 — a node degrades, and the platform must stop scheduling onto it.
#
# The fault is injected by stopping kubelet on a kind worker, not by editing a status field. Writing
# NotReady into the Node's status directly would be answered by the real kubelet within a heartbeat, so the
# experiment would be measuring how fast it was overwritten. Stopping kubelet makes the apiserver reach
# NotReady the way it does in production: by the heartbeat going stale.
#
# WHAT THIS MEASURES, AND WHAT IT DOES NOT
#
# Two latencies live between the fault and the taint, and only the second belongs to this platform:
#
#   inject -> NotReady    Kubernetes' own detection, governed by node-monitor-grace-period. Not ours.
#   NotReady -> taint     the NodeHealth controller's reaction. OURS.
#
# Reporting only the total would credit this platform with Kubernetes' detection window, which is roughly an
# order of magnitude larger. They are timed separately for that reason.
#
# THE STEADY STATE IS A GATE, NOT A PREAMBLE
#
# A chaos experiment that never established its steady state has measured nothing: if the node was already
# tainted, or the controller was not running, the taint observed afterwards is not evidence of a reaction.
# Every precondition below aborts rather than warns.
#
# Restores kubelet on exit, including on failure or interrupt.
set -uo pipefail

NODE=${NODE:-platform-worker2}
CLUSTER=${CLUSTER:-platform}
SYS=${SYS:-gpu-platform-control-plane-system}
TAINT_KEY=${TAINT_KEY:-platform.lkhun9311.github.io/unhealthy}
OUT=${OUT:-./ex/chaos-fr004.json}
DEADLINE_NOTREADY=${DEADLINE_NOTREADY:-180}
DEADLINE_TAINT=${DEADLINE_TAINT:-120}
CANARY_NS=default

say () { echo "  $*"; }
die () { echo "ABORT: $*" >&2; exit 1; }

# Nanoseconds. This system's date is uutils coreutils, where %3N does NOT truncate to three digits — it
# emits the full nine. A first version of this script labelled the results "ms" and printed a controller
# reaction of 30,646,819 ms, which is eight and a half hours. Working in ns and converting explicitly at the
# point of display is what makes that unrepresentable.
now_ns () { date +%s%N; }
ms () { echo "scale=1; $1 / 1000000" | bc; }

taint_present () {
  kubectl get node "$NODE" -o jsonpath="{.spec.taints[?(@.key=='$TAINT_KEY')].key}" 2>/dev/null | grep -q .
}
node_ready () {
  [ "$(kubectl get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "True" ]
}

# Restoring kubelet is not enough to restore the cluster.
#
# A device plugin registers with kubelet over a socket, and that registration does not survive kubelet
# restarting: the node comes back Ready while advertising nvidia.com/gpu: 0. The first version of this script
# stopped at "kubelet is running again" and left the node without its simulated GPUs — which would have
# silently failed the NEXT experiment's steady state, or worse, passed it while measuring a node no workload
# could land on. A chaos experiment that degrades the cluster it runs on is not repeatable.
restore () {
  echo
  say "restoring kubelet on $NODE"
  docker exec "${CLUSTER}-worker2" systemctl start kubelet >/dev/null 2>&1 || \
    docker exec "$NODE" systemctl start kubelet >/dev/null 2>&1 || true
  kubectl -n "$CANARY_NS" delete pod chaos-canary-pre chaos-canary-post --ignore-not-found >/dev/null 2>&1
  kubectl delete nodehealth "chaos-$NODE" --ignore-not-found >/dev/null 2>&1

  # Wait for Ready first: deleting the plugin pod while the node is still NotReady only strands the
  # replacement, since the scheduler will not place it there.
  for _ in $(seq 120); do node_ready && break; sleep 1; done

  local plugin
  plugin=$(kubectl -n "$SYS" get pods -o wide --no-headers 2>/dev/null \
    | awk -v n="$NODE" '$7==n && /gpu-simulator/{print $1}')
  if [ -n "$plugin" ]; then
    say "re-registering the device plugin on $NODE (kubelet restart drops the registration)"
    kubectl -n "$SYS" delete pod "$plugin" --wait=false >/dev/null 2>&1
    for _ in $(seq 60); do
      local gpu
      gpu=$(kubectl get node "$NODE" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
      [ -n "$gpu" ] && [ "$gpu" != "0" ] && { say "GPU capacity restored on $NODE (gpu=$gpu)"; return; }
      sleep 2
    done
    say "WARNING: $NODE still advertises no GPU; the next run's steady state will be wrong"
  fi
}
trap restore EXIT

# ---------------------------------------------------------------- steady state
say "=== steady state ==="

kubectl get node "$NODE" >/dev/null 2>&1 || die "node $NODE does not exist"
node_ready || die "node $NODE is already NotReady; there is no degradation left to inject"
taint_present && die "node $NODE already carries $TAINT_KEY; a taint seen later would not be a reaction"

# A previous run that failed to restore the device plugin leaves the node Ready but advertising no GPU. The
# canary below would still schedule (it asks for no GPU), so the run would pass while exercising a node no
# real workload could use.
GPU=$(kubectl get node "$NODE" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
[ -n "$GPU" ] && [ "$GPU" != "0" ] || die "node $NODE advertises no GPU (allocatable=${GPU:-none}); a previous run left it degraded, so its steady state is not the one this experiment assumes"

kubectl -n "$SYS" get deploy gpu-platform-control-plane-controller-manager >/dev/null 2>&1 \
  || die "the operator is not deployed; nothing would apply a taint"
READY=$(kubectl -n "$SYS" get deploy gpu-platform-control-plane-controller-manager -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
[ "${READY:-0}" -ge 1 ] || die "the operator has no ready replica"

# The canary proves the node was actually a scheduling target BEFORE the fault. Without it, "no pod landed
# there afterwards" is equally explained by the node never having been a candidate.
say "scheduling a canary onto $NODE to prove it was a candidate"
kubectl -n "$CANARY_NS" delete pod chaos-canary-pre --ignore-not-found >/dev/null 2>&1
kubectl -n "$CANARY_NS" run chaos-canary-pre --image=busybox --restart=Never \
  --overrides="{\"spec\":{\"nodeName\":\"$NODE\",\"containers\":[{\"name\":\"c\",\"image\":\"busybox\",\"command\":[\"sleep\",\"600\"]}]}}" \
  >/dev/null 2>&1 || die "could not create the pre-fault canary"
for _ in $(seq 60); do
  PHASE=$(kubectl -n "$CANARY_NS" get pod chaos-canary-pre -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$PHASE" = "Running" ] && break
  sleep 1
done
[ "${PHASE:-}" = "Running" ] || die "the pre-fault canary never ran on $NODE (phase=${PHASE:-none}); the node was not a usable target"
say "canary running on $NODE — the node was schedulable"

kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: platform.lkhun9311.github.io/v1
kind: NodeHealth
metadata:
  name: chaos-$NODE
spec:
  nodeName: $NODE
  gpuClass: l40s
EOF
[ $? -eq 0 ] || die "could not create the NodeHealth tracking $NODE"

for _ in $(seq 30); do
  PH=$(kubectl get nodehealth "chaos-$NODE" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$PH" = "Ready" ] && break
  sleep 1
done
[ "${PH:-}" = "Ready" ] || die "NodeHealth never reached Ready (phase=${PH:-none}); the controller is not tracking this node"
say "NodeHealth phase=Ready — the controller is watching and sees a healthy node"

# ---------------------------------------------------------------- inject
echo
say "=== inject: stopping kubelet on $NODE ==="
T_INJECT=$(now_ns)
docker exec "${CLUSTER}-worker2" systemctl stop kubelet >/dev/null 2>&1 \
  || docker exec "$NODE" systemctl stop kubelet >/dev/null 2>&1 \
  || die "could not stop kubelet on $NODE"

# Both transitions are watched in ONE tight loop, because they are only microseconds apart and polling them
# in sequence cannot order them. The first version polled NotReady to completion and only then began looking
# for the taint — by which time the taint was always already there, so the "reaction time" it reported was
# the round-trip cost of a single kubectl call.
#
# The interval is the harness's resolution. A reaction faster than one observation is reported as a bound,
# not as a number: this loop cannot tell 5ms from 50ms and must not pretend otherwise.
T_NOTREADY=""
T_TAINT=""
OBSERVATIONS=0
LOOP_START=$(now_ns)
for _ in $(seq $(( DEADLINE_NOTREADY * 20 ))); do
  OBSERVATIONS=$(( OBSERVATIONS + 1 ))
  if [ -z "$T_NOTREADY" ] && ! node_ready; then T_NOTREADY=$(now_ns); fi
  if [ -z "$T_TAINT" ] && taint_present; then T_TAINT=$(now_ns); fi
  [ -n "$T_NOTREADY" ] && [ -n "$T_TAINT" ] && break
  sleep 0.05
done
[ -n "$T_NOTREADY" ] || die "node never went NotReady within ${DEADLINE_NOTREADY}s"
[ -n "$T_TAINT" ] || die "taint never appeared; the controller did not react to the degradation"

# The mean cost of one observation, which is what bounds any interval this loop reports.
RESOLUTION_NS=$(( ($(now_ns) - LOOP_START) / OBSERVATIONS ))
DETECT_NS=$(( T_NOTREADY - T_INJECT ))
REACTION_NS=$(( T_TAINT - T_NOTREADY ))

say "NotReady after $(ms $DETECT_NS)ms — this is Kubernetes' detection, not ours"
if [ "$REACTION_NS" -le "$RESOLUTION_NS" ]; then
  REACTION_REPORT="below the harness resolution of $(ms $RESOLUTION_NS)ms"
  say "taint was already present the first time NotReady was observed"
  say "reaction: $REACTION_REPORT — this harness cannot resolve it further, and the raw difference is the cost of one API call rather than a measurement"
else
  REACTION_REPORT="$(ms $REACTION_NS)ms"
  say "taint applied $REACTION_REPORT after NotReady, against a resolution of $(ms $RESOLUTION_NS)ms — THIS is the platform's reaction"
fi

QPHASE=$(kubectl get nodehealth "chaos-$NODE" -o jsonpath='{.status.phase}' 2>/dev/null)
FAULT=$(kubectl get nodehealth "chaos-$NODE" -o jsonpath='{.status.faultSignal.source}' 2>/dev/null)
say "NodeHealth phase=$QPHASE faultSignal=$FAULT"

# ---------------------------------------------------------------- effect
echo
say "=== effect: does scheduling avoid the node ==="
kubectl -n "$CANARY_NS" delete pod chaos-canary-post --ignore-not-found >/dev/null 2>&1
kubectl -n "$CANARY_NS" run chaos-canary-post --image=busybox --restart=Never \
  --command -- sleep 600 >/dev/null 2>&1
AVOIDED="unknown"
for _ in $(seq 45); do
  ON=$(kubectl -n "$CANARY_NS" get pod chaos-canary-post -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  [ -n "$ON" ] && break
  sleep 1
done
if [ -z "${ON:-}" ]; then
  AVOIDED="pending"
  say "post-fault canary is still unscheduled"
elif [ "$ON" = "$NODE" ]; then
  AVOIDED="false"
  say "post-fault canary landed on $NODE ANYWAY — the taint did not repel it"
else
  AVOIDED="true"
  say "post-fault canary landed on $ON, avoiding $NODE"
fi

# ---------------------------------------------------------------- recovery
echo
say "=== recovery: restarting kubelet ==="
T_RECOVER=$(now_ns)
docker exec "${CLUSTER}-worker2" systemctl start kubelet >/dev/null 2>&1 \
  || docker exec "$NODE" systemctl start kubelet >/dev/null 2>&1 || true

# The same single tight loop as the injection path, for the same reason. An earlier version fixed only the
# injection side and left this one polling sequentially at one-second intervals, which reported the untaint
# as 37.9ms — a figure below the harness's own 95.9ms resolution, and therefore the cost of an API call
# rather than a measurement. Half a corrected instrument still produces a number nobody should quote.
T_READY=""
T_UNTAINT=""
R_OBS=0
R_START=$(now_ns)
for _ in $(seq 3600); do
  R_OBS=$(( R_OBS + 1 ))
  if [ -z "$T_READY" ] && node_ready; then T_READY=$(now_ns); fi
  if [ -z "$T_UNTAINT" ] && ! taint_present; then T_UNTAINT=$(now_ns); fi
  [ -n "$T_READY" ] && [ -n "$T_UNTAINT" ] && break
  sleep 0.05
done
R_RESOLUTION_NS=$(( ($(now_ns) - R_START) / R_OBS ))

if [ -n "$T_READY" ]; then
  say "node Ready again $(ms $(( T_READY - T_RECOVER )))ms after kubelet restarted"
else
  say "node never returned to Ready"
fi

if [ -z "$T_UNTAINT" ]; then
  UNTAINT_REPORT=null
  say "taint was NOT removed; quarantine does not lift on its own"
elif [ -z "$T_READY" ]; then
  UNTAINT_REPORT='"untainted without the node returning to Ready"'
  say "taint removed while the node was still NotReady — that is the opposite of the intended trigger"
else
  UNTAINT_NS=$(( T_UNTAINT - T_READY ))
  if [ "$UNTAINT_NS" -le "$R_RESOLUTION_NS" ]; then
    UNTAINT_REPORT="\"below the harness resolution of $(ms $R_RESOLUTION_NS)ms\""
    say "taint was already gone the first time Ready was observed — reaction below the $(ms $R_RESOLUTION_NS)ms resolution"
  else
    UNTAINT_REPORT="$(ms $UNTAINT_NS)"
    say "taint removed $(ms $UNTAINT_NS)ms after Ready, against a resolution of $(ms $R_RESOLUTION_NS)ms"
  fi
fi

# ---------------------------------------------------------------- record
mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<EOF
{
  "experiment": "FR-004 degraded node",
  "node": "$NODE",
  "injection": "systemctl stop kubelet",
  "steadyStateEstablished": true,
  "harnessResolutionMs": {
    "injection": $(ms $RESOLUTION_NS),
    "recovery": $(ms $R_RESOLUTION_NS)
  },
  "latenciesMs": {
    "injectToNotReady": $(ms $DETECT_NS),
    "notReadyToTaint": "$REACTION_REPORT",
    "recoverToReady": $( [ -n "$T_READY" ] && ms $(( T_READY - T_RECOVER )) || echo null ),
    "readyToUntaint": $UNTAINT_REPORT
  },
  "attribution": {
    "injectToNotReady": "kubernetes node-monitor-grace-period, NOT this platform",
    "notReadyToTaint": "this platform's NodeHealth controller, bounded above by harnessResolutionMs"
  },
  "quarantinePhase": "$QPHASE",
  "faultSignal": "$FAULT",
  "schedulingAvoided": "$AVOIDED"
}
EOF
echo
say "record written to $OUT"
[ "$AVOIDED" = "true" ] && [ -n "$T_UNTAINT" ]
