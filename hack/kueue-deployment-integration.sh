#!/usr/bin/env bash
# Does Kueue already charge a serving Deployment against the same ClusterQueue as a training Job?
#
# This runs BEFORE any code is written, because the answer decides whether the code should exist at all.
# The plan was to build a controller making inference GPU consumption visible to Kueue's accounting. A
# review pointed out that Kueue v0.18.3 ships a `deployment` integration that already does exactly that,
# and that the whole controller would collapse into setting one label. This checks that claim.
#
# No GPU is needed. Kueue reserves quota at ADMISSION, before the scheduler ever looks for a node, so the
# admitted Pod staying Pending on a cluster with no real device is irrelevant to what is being measured.
set -uo pipefail

NS=${NS:-kueue-integration-test}
FLAVOR=kit-flavor
CQ=kit-cq
LQ=kit-lq

say () { echo "  $*"; }
die () { echo "ABORT: $*" >&2; exit 1; }

cleanup() {
  echo
  say "cleaning up"
  kubectl delete ns "$NS" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete clusterqueue "$CQ" --ignore-not-found >/dev/null 2>&1
  kubectl delete resourceflavor "$FLAVOR" --ignore-not-found >/dev/null 2>&1
}
trap cleanup EXIT

# ---------------------------------------------------------------- setup
say "=== setup: one ClusterQueue with a single GPU of nominal quota ==="

kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1

kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata: {name: $FLAVOR}
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata: {name: $CQ}
spec:
  namespaceSelector: {}
  resourceGroups:
    - coveredResources: ["nvidia.com/gpu"]
      flavors:
        - name: $FLAVOR
          resources:
            - name: "nvidia.com/gpu"
              nominalQuota: 1
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata: {name: $LQ, namespace: $NS}
spec: {clusterQueue: $CQ}
EOF
[ $? -eq 0 ] || die "could not create the queue fixtures"
say "ClusterQueue $CQ has nominal nvidia.com/gpu: 1"

# ---------------------------------------------------------------- the serving side
echo
say "=== a serving Deployment carrying the queue label ==="
say "(this stands in for the Deployment KServe generates in Standard mode)"

kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: serving
  namespace: $NS
  labels:
    kueue.x-k8s.io/queue-name: $LQ
spec:
  replicas: 1
  selector: {matchLabels: {app: serving}}
  template:
    metadata:
      labels: {app: serving}
    spec:
      containers:
        - name: c
          image: busybox:1.36
          command: ["sh","-c","sleep 3600"]
          resources:
            requests: {"nvidia.com/gpu": 1}
            limits: {"nvidia.com/gpu": 1}
EOF
[ $? -eq 0 ] || die "could not create the serving Deployment"

# A Workload appearing at all is the whole question. If Kueue ignores Deployments, none is created and the
# controller the review argued against would in fact be needed.
SERVING_WL=""
for _ in $(seq 60); do
  n=$(kubectl -n "$NS" get workloads -o name 2>/dev/null | wc -l)
  [ "$n" -ge 1 ] && { SERVING_WL=$(kubectl -n "$NS" get workloads -o name | head -1); break; }
  sleep 2
done

if [ -z "$SERVING_WL" ]; then
  say "NO Workload was created for the serving Deployment."
  say "Kueue is not managing it, so the queue label alone does not unify the budget."
  say "VERDICT: the integration does NOT cover this path as configured."
  exit 1
fi
say "Kueue created $SERVING_WL for the serving pod"

# ---------------------------------------------------------------- the training side
echo
say "=== a training Job asking for the same single GPU ==="

kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: training
  namespace: $NS
  labels:
    kueue.x-k8s.io/queue-name: $LQ
spec:
  parallelism: 1
  completions: 1
  suspend: true
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: c
          image: busybox:1.36
          command: ["sh","-c","sleep 3600"]
          resources:
            requests: {"nvidia.com/gpu": 1}
            limits: {"nvidia.com/gpu": 1}
EOF
[ $? -eq 0 ] || die "could not create the training Job"

sleep 20

# ---------------------------------------------------------------- what the ledger says
echo
say "=== Kueue's own accounting ==="
kubectl -n "$NS" get workloads -o custom-columns=\
NAME:.metadata.name,QUEUE:.spec.queueName,ADMITTED:.status.conditions[?\(@.type==\'Admitted\'\)].status \
  --no-headers 2>/dev/null | sed 's/^/    /'

USED=$(kubectl get clusterqueue "$CQ" -o jsonpath='{.status.flavorsUsage[0].resources[0].total}' 2>/dev/null)
ADMITTED=$(kubectl get clusterqueue "$CQ" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)
PENDING=$(kubectl get clusterqueue "$CQ" -o jsonpath='{.status.pendingWorkloads}' 2>/dev/null)

echo
say "ClusterQueue $CQ: used=${USED:-0} admitted=${ADMITTED:-0} pending=${PENDING:-0}"

# ---------------------------------------------------------------- verdict
echo
if [ "${ADMITTED:-0}" = "1" ] && [ "${PENDING:-0}" = "1" ]; then
  say "VERDICT: one workload holds the single GPU and the other waits."
  say "Serving and training are drawing from ONE budget, with no custom controller."
  say "The proposed quota-unification controller is unnecessary on this path."
elif [ "${ADMITTED:-0}" = "2" ]; then
  say "VERDICT: BOTH were admitted against a quota of 1."
  say "The budget is not being shared correctly, which is a finding in itself."
else
  say "VERDICT: inconclusive (admitted=${ADMITTED:-0} pending=${PENDING:-0}). Inspect the Workloads above."
fi
