#!/usr/bin/env bash
# How long does a device-holder keep its device after it is told to stop?
#
# The reclaim experiment established that a workload ignoring SIGTERM defeats quota reclaim while its
# remaining service fits inside the termination grace period, and is cut off at grace once it does not. That
# gives a model:
#
#     held = min(remaining service, grace period)
#
# and the model matters because grace is set by the tenant being preempted. It is what the admission cap in
# internal/webhook/v1/gpupod_webhook.go is for, so it should be measured rather than assumed.
#
# WHY NOT THROUGH THE QUEUELAB
#
# MLTrainingJob has no grace field, so the lab cannot vary the axis this tests. More to the point, the lab's
# TerminationContractTrace REFUSES any dose whose remaining service does not match the declared regime — it
# encodes grace=30 as an axiom and rejects every configuration that would probe it. This runs plain Jobs so
# the parameter is actually free.
#
# WHAT IS MEASURED
#
# Wall time from the delete call returning to the Pod object being gone. That includes the API round trips at
# both ends, so it is an upper bound on the container's own shutdown; the quantity of interest here is
# seconds and the round trips are milliseconds.
set -uo pipefail

NS=${NS:-grace-probe}
REMAINING=${REMAINING:-45}
GRACES=${GRACES:-"10 20 30 60 120"}
IMAGE=${IMAGE:-busybox:1.36}
OUT=${OUT:-./ex/grace-holds.json}

say () { echo "  $*"; }

cleanup () { kubectl delete ns "$NS" --ignore-not-found --wait=false >/dev/null 2>&1; }
trap cleanup EXIT

kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1

say "=== remaining service ${REMAINING}s, sweeping the grace period ==="
say "the workload is 'sleep' as PID 1, which ignores SIGTERM without a handler — the arm that defeats reclaim"
echo

rows=""
for G in $GRACES; do
  NAME="hold-$G"
  kubectl -n "$NS" delete pod "$NAME" --ignore-not-found --wait=false >/dev/null 2>&1
  cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata: {name: $NAME, namespace: $NS}
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: $G
  containers:
    - {name: c, image: "$IMAGE", command: ["sh","-c","sleep $REMAINING"]}
EOF
  ready=""
  for _ in $(seq 40); do
    [ "$(kubectl -n "$NS" get pod "$NAME" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && { ready=1; break; }
    sleep 1
  done
  [ -z "$ready" ] && { say "grace=${G}s: the Pod never ran"; continue; }

  # Deleted at a fixed point into its service so every case has the same amount left.
  sleep 2
  t0=$(date +%s%N)
  kubectl -n "$NS" delete pod "$NAME" --wait=false >/dev/null 2>&1
  held=""
  for _ in $(seq 400); do
    kubectl -n "$NS" get pod "$NAME" >/dev/null 2>&1 || { held=$(( ($(date +%s%N) - t0) / 1000000 )); break; }
    sleep 0.5
  done
  [ -z "$held" ] && held=-1
  remaining_at_delete=$(( REMAINING - 2 ))
  predicted=$(( G < remaining_at_delete ? G : remaining_at_delete ))
  say "grace=${G}s  remaining=${remaining_at_delete}s  ->  held ${held}ms   (min model predicts ${predicted}s)"
  rows="$rows{\"graceSec\":$G,\"remainingSec\":$remaining_at_delete,\"heldMs\":$held,\"predictedSec\":$predicted},"
done

mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<EOF
{
  "experiment": "how long a device-holder keeps its device after being told to stop",
  "workload": "sleep as PID 1, which ignores SIGTERM without a handler",
  "model": "held = min(remaining service, grace period)",
  "remainingServiceSec": $REMAINING,
  "deletedAfterSec": 2,
  "rows": [${rows%,}],
  "measures": "wall time from the delete call returning to the Pod object being gone; includes an API round trip at each end, so it is an upper bound on the container's own shutdown",
  "whyItMatters": "grace is set by the tenant being preempted, so it bounds how long a borrower keeps a device its owner has already reclaimed. internal/webhook/v1 caps it at 120s for device holders."
}
EOF
echo
say "record written to $OUT"
