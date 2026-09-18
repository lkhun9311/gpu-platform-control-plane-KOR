#!/usr/bin/env bash
# How does the reconcile queue behave when a namespace fills up?
#
# Two documents say this is unmeasured, and both name it first. The benchmark that found the O(N) lookup
# measured that lookup ISOLATED, in envtest, one call at a time. It did not show the queue behaviour the
# O(N^2) claim rests on: every reconciler here runs at MaxConcurrentReconciles=1, so a per-item cost
# proportional to namespace size drains the queue in O(N^2) IF the per-item cost is really proportional.
#
# WHAT THIS MEASURES
#
#   drain time      wall clock from the last create until the workqueue is empty again
#   throughput      reconciles per second sustained during the drain
#   depth peak      how far the queue backed up
#   p99 latency     per-reconcile duration under load, from the operator's own histogram
#
# WHAT IT CANNOT SETTLE
#
# The index fix is already in. So this measures the queue with an O(1) lookup, and a flat drain here is
# consistent with the fix working rather than proof the old code was quadratic. Reverting the index to
# measure the contrast would mean redeploying a knowingly slower operator, which is a separate exercise.
# What this run does establish is the CURRENT ceiling: how many objects this operator absorbs per second,
# which is the number an operator would need before running it anywhere real.
set -uo pipefail

NS=${NS:-queue-load}
COUNT=${COUNT:-400}
BATCH=${BATCH:-50}
PROM=${PROM:-localhost:9090}
SYS=${SYS:-gpu-platform-control-plane-system}
OUT=${OUT:-./ex/queue-drain.json}

say () { echo "  $*"; }
die () { echo "ABORT: $*" >&2; exit 1; }
now_ns () { date +%s%N; }
ms () { echo "scale=1; $1 / 1000000" | bc; }

promq () {
  curl -sG "http://$PROM/api/v1/query" --data-urlencode "query=$1" 2>/dev/null | python3 -c "
import json,sys
try: r=json.load(sys.stdin)['data']['result']
except Exception: print('0'); raise SystemExit
print(r[0]['value'][1] if r else '0')"
}

cleanup() {
  echo
  say "removing the load"
  kubectl delete ns "$NS" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

# ---------------------------------------------------------------- steady state
say "=== steady state ==="

kubectl -n "$SYS" get deploy gpu-platform-control-plane-controller-manager >/dev/null 2>&1 \
  || die "the operator is not deployed; there is nothing to load"
[ "$(kubectl -n "$SYS" get deploy gpu-platform-control-plane-controller-manager -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" -ge 1 ] \
  || die "the operator has no ready replica"

BASE=$(promq 'sum(controller_runtime_reconcile_total{controller="mltrainingjob"})')
[ "$BASE" != "0" ] || say "note: reconcile counter reads 0; if it stays there, Prometheus is not scraping"
promq 'sum(workqueue_depth)' >/dev/null || die "Prometheus is not answering; the drain could not be observed"
say "operator ready, Prometheus answering, reconciles so far: $BASE"

# A queue that is already backed up would make the drain time meaningless.
START_DEPTH=$(promq 'sum(workqueue_depth{name="mltrainingjob"})')
say "workqueue depth before the load: ${START_DEPTH:-0}"

kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1

# ---------------------------------------------------------------- load
echo
say "=== creating $COUNT MLTrainingJobs in batches of $BATCH ==="

T_START=$(now_ns)
created=0
while [ "$created" -lt "$COUNT" ]; do
  {
    for i in $(seq 1 "$BATCH"); do
      n=$((created + i))
      [ "$n" -gt "$COUNT" ] && break
      cat <<EOF
---
apiVersion: platform.lkhun9311.github.io/v1
kind: MLTrainingJob
metadata:
  name: load-$n
  namespace: $NS
spec:
  queue: team-a
  image: busybox:1.36
  command: ["sh","-c","sleep 1"]
  gpuCount: 1
  parallelism: 1
  completions: 1
EOF
    done
  } | kubectl apply -f - >/dev/null 2>&1
  created=$((created + BATCH))
  [ "$created" -gt "$COUNT" ] && created=$COUNT
  printf "\r    created %d/%d" "$created" "$COUNT"
done
echo
T_CREATED=$(now_ns)
say "all $COUNT created in $(ms $(( T_CREATED - T_START )))ms"

# ---------------------------------------------------------------- watch the drain
echo
say "=== watching the queue drain ==="

PEAK=0
SAMPLES=0
EMPTY_STREAK=0
T_DRAINED=""
DEADLINE=$(( $(date +%s) + 900 ))

while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  D=$(promq 'sum(workqueue_depth{name="mltrainingjob"})')
  D=${D%%.*}; D=${D:-0}
  SAMPLES=$((SAMPLES + 1))
  [ "$D" -gt "$PEAK" ] && PEAK=$D
  if [ "$D" -le 0 ]; then
    EMPTY_STREAK=$((EMPTY_STREAK + 1))
    # Three consecutive empty samples, because one can land between two bursts of requeues.
    if [ "$EMPTY_STREAK" -ge 3 ]; then T_DRAINED=$(now_ns); break; fi
  else
    EMPTY_STREAK=0
  fi
  printf "\r    depth=%-6s peak=%-6s" "$D" "$PEAK"
  sleep 5
done
echo

[ -n "$T_DRAINED" ] || say "the queue never emptied within the deadline"

AFTER=$(promq 'sum(controller_runtime_reconcile_total{controller="mltrainingjob"})')
RECONCILES=$(echo "$AFTER - $BASE" | bc 2>/dev/null || echo 0)
DRAIN_MS=$( [ -n "$T_DRAINED" ] && ms $(( T_DRAINED - T_CREATED )) || echo null )
P99=$(promq 'histogram_quantile(0.99, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{controller="mltrainingjob"}[5m])))')
ERRORS=$(promq 'sum(controller_runtime_reconcile_errors_total{controller="mltrainingjob"})')

THROUGHPUT=null
if [ -n "$T_DRAINED" ] && [ "$(echo "$DRAIN_MS > 0" | bc 2>/dev/null)" = "1" ]; then
  THROUGHPUT=$(echo "scale=1; $RECONCILES / ($DRAIN_MS / 1000)" | bc 2>/dev/null || echo null)
fi

echo
say "objects created        $COUNT"
say "reconciles performed   $RECONCILES"
say "peak queue depth       $PEAK"
say "drain time             ${DRAIN_MS}ms"
say "throughput             ${THROUGHPUT} reconciles/sec"
say "reconcile p99          ${P99}s"
say "reconcile errors       ${ERRORS:-0}"

# ---------------------------------------------------------------- record
mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<EOF
{
  "experiment": "reconcile queue drain under a namespace fill",
  "objects": $COUNT,
  "batchSize": $BATCH,
  "concurrency": "MaxConcurrentReconciles=1 (controller-runtime default, unset in this operator)",
  "samplingIntervalSec": 5,
  "createMs": $(ms $(( T_CREATED - T_START ))),
  "drainMs": $DRAIN_MS,
  "reconciles": ${RECONCILES:-0},
  "peakQueueDepth": $PEAK,
  "throughputPerSec": ${THROUGHPUT},
  "reconcileP99Sec": ${P99:-0},
  "reconcileErrors": ${ERRORS:-0},
  "caveat": "Measured with the field index already in place, so a flat drain is consistent with the fix rather than proof the previous code was quadratic. What it establishes is this operator's current absorption rate.",
  "resolutionNote": "Queue depth is sampled every 5s from Prometheus, which itself scrapes at 15s, so the peak is a lower bound: a spike between two scrapes is invisible."
}
EOF
echo
say "record written to $OUT"
[ -n "$T_DRAINED" ]
