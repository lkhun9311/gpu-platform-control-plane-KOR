#!/usr/bin/env bash
# Does a queue label on an InferenceService reach the Deployment KServe generates, and survive there?
#
# This is the only question left after hack/kueue-deployment-integration.sh showed that Kueue already
# charges a labelled Deployment's pods against the same ClusterQueue a training Job uses. If KServe
# propagates the label, unifying the budget is configuration. If it does not, the gap is label propagation,
# and the honest contribution is propagation or an admission mutation rather than a second quota ledger.
#
# SURVIVAL IS THE POINT, NOT ARRIVAL
#
# KServe owns the generated Deployment and reconciles it continuously. A label that appears once and is
# stripped on the next reconcile is worse than one that never appears: the quota holds until something
# triggers a reconcile, then silently stops. So the check is run twice, with a spec change in between to
# force KServe to rewrite the Deployment.
#
# No GPU is required. Kueue reserves quota at admission, before the scheduler looks for a node, so a pod
# that stays Pending on a device-less cluster does not affect what is measured.
set -uo pipefail

NS=${NS:-kserve-label-test}
ISVC=${ISVC:-label-probe}
QUEUE_LABEL=kueue.x-k8s.io/queue-name
LQ=${LQ:-klt-lq}
CQ=${CQ:-klt-cq}
FLAVOR=${FLAVOR:-klt-flavor}
OUT=${OUT:-./ex/kserve-label-propagation.json}

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

# ---------------------------------------------------------------- steady state
say "=== steady state ==="

kubectl get crd inferenceservices.serving.kserve.io >/dev/null 2>&1 \
  || die "KServe is not installed; there is nothing to propagate through"
kubectl -n kserve get deploy kserve-controller-manager >/dev/null 2>&1 \
  || die "the KServe controller is not deployed"
say "KServe present"

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
              nominalQuota: 2
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata: {name: $LQ, namespace: $NS}
spec: {clusterQueue: $CQ}
EOF
say "queue fixtures created"

# ---------------------------------------------------------------- the subject
echo
say "=== an InferenceService carrying the queue label ==="

# The label is set in three places on purpose, because KServe's propagation rules differ between them and
# the run should report which one, if any, actually reaches the Deployment.
kubectl apply -f - 2>&1 <<EOF | sed 's/^/    /'
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: $ISVC
  namespace: $NS
  labels:
    $QUEUE_LABEL: $LQ
  annotations:
    serving.kserve.io/deploymentMode: RawDeployment
spec:
  predictor:
    labels:
      $QUEUE_LABEL: $LQ
    containers:
      - name: kserve-container
        image: busybox:1.36
        command: ["sh","-c","sleep 3600"]
        resources:
          requests: {"nvidia.com/gpu": 1}
          limits: {"nvidia.com/gpu": 1}
EOF

DEP=""
for _ in $(seq 60); do
  DEP=$(kubectl -n "$NS" get deploy -o name 2>/dev/null | head -1)
  [ -n "$DEP" ] && break
  sleep 2
done
[ -n "$DEP" ] || die "KServe never generated a Deployment; check the controller's logs"
say "KServe generated $DEP"

# ---------------------------------------------------------------- first look
check_label () {
  kubectl -n "$NS" get "$DEP" -o jsonpath="{.metadata.labels.kueue\.x-k8s\.io/queue-name}" 2>/dev/null
}
count_workloads () {
  kubectl -n "$NS" get workloads -o name 2>/dev/null | wc -l | tr -d ' '
}

sleep 10
FIRST=$(check_label)
WL_FIRST=$(count_workloads)
echo
if [ -n "$FIRST" ]; then
  say "label ON the generated Deployment: $FIRST"
else
  say "label ABSENT from the generated Deployment"
fi
say "Kueue Workloads in $NS: $WL_FIRST"

# ---------------------------------------------------------------- force a reconcile
echo
say "=== forcing KServe to rewrite the Deployment ==="
kubectl -n "$NS" patch inferenceservice "$ISVC" --type=merge \
  -p '{"spec":{"predictor":{"minReplicas":1}}}' >/dev/null 2>&1 \
  || say "(patch rejected; the survival check below is then weaker)"
sleep 25

SECOND=$(check_label)
WL_SECOND=$(count_workloads)
if [ -n "$SECOND" ]; then
  say "label after reconcile: $SECOND"
else
  say "label after reconcile: ABSENT"
fi
say "Kueue Workloads after reconcile: $WL_SECOND"

# ---------------------------------------------------------------- verdict
echo
say "=== verdict ==="
if [ -n "$FIRST" ] && [ -n "$SECOND" ]; then
  VERDICT="propagates-and-survives"
  say "The label reaches the Deployment and survives a reconcile."
  say "Unifying the budget across serving and training is configuration, not code."
elif [ -n "$FIRST" ] && [ -z "$SECOND" ]; then
  VERDICT="stripped-on-reconcile"
  say "The label arrives and is then STRIPPED when KServe rewrites the Deployment."
  say "This is the worst outcome: quota holds until something triggers a reconcile, then stops silently."
elif [ -z "$FIRST" ]; then
  VERDICT="does-not-propagate"
  say "The label never reaches the generated Deployment."
  say "The gap is propagation. The contribution is a supported propagation path or an admission mutation,"
  say "not a second quota ledger competing with Kueue's."
fi

if [ "${WL_FIRST:-0}" = "0" ] && [ "${WL_SECOND:-0}" = "0" ]; then
  say "No Kueue Workload was created either time, so the serving pod is outside the tenant budget."
fi

mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<EOF
{
  "experiment": "does a queue label on an InferenceService reach the generated Deployment",
  "kserve": "RawDeployment mode",
  "labelSetOn": ["InferenceService.metadata.labels", "spec.predictor.labels"],
  "generatedDeployment": "$DEP",
  "labelOnDeployment": { "afterCreate": "${FIRST:-}", "afterReconcile": "${SECOND:-}" },
  "kueueWorkloads": { "afterCreate": ${WL_FIRST:-0}, "afterReconcile": ${WL_SECOND:-0} },
  "verdict": "$VERDICT",
  "whySurvivalMatters": "KServe owns and continuously reconciles the Deployment. A label that arrives once and is stripped later holds quota until the next reconcile and then stops without any error."
}
EOF
say "record written to $OUT"
[ "$VERDICT" = "propagates-and-survives" ]
