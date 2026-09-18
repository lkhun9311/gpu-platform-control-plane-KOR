#!/usr/bin/env bash
#
# M6 kind end-to-end: two-tenant Kueue fair-sharing and preemption with a fake GPU.
#
# This script creates a kind cluster, installs Kueue, deploys the operator and the
# fake GPU device plugin, and then drives two tenants through borrowing and reclaim
# so the fair-sharing and preemption behaviour can be observed with no real GPU.
#
# It captures evidence to hack/m6-e2e-evidence.log and does not tear the cluster down,
# so the final state can be inspected; run `kind delete cluster --name platform` when done.
set -uo pipefail

cd "$(dirname "$0")/.."
export PATH="$PWD/bin:$PATH"
export GOTOOLCHAIN=go1.26.0

CLUSTER=platform
KCTX=kind-$CLUSTER
NS=gpu-platform-control-plane-system
LOG=hack/m6-e2e-evidence.log
: > "$LOG"

log()  { echo -e "$*" | tee -a "$LOG"; }
step() { echo -e "\n===== $* =====" | tee -a "$LOG"; }
run()  { echo "+ $*" | tee -a "$LOG"; "$@" >>"$LOG" 2>&1; }
cap()  { echo "+ $*" | tee -a "$LOG"; "$@" 2>&1 | tee -a "$LOG"; }
die()  { echo "FAILED at: $*" | tee -a "$LOG"; exit 1; }

k() { kubectl --context "$KCTX" "$@"; }

phases() {
  echo "--- MLTrainingJob phases ---" | tee -a "$LOG"
  k get mltrainingjob -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase 2>&1 | tee -a "$LOG"
  echo "--- Workloads (admitted) ---" | tee -a "$LOG"
  k get workloads -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,QUEUE:.spec.queueName,ADMITTED:'.status.conditions[?(@.type=="Admitted")].status' 2>&1 | tee -a "$LOG"
  echo "--- ClusterQueue usage ---" | tee -a "$LOG"
  # cohortName, not cohort: v1beta2 renamed the field, and querying the old one made every
  # ClusterQueue in the evidence log read COHORT <none> even though the cohort was set.
  k get clusterqueue -o custom-columns=NAME:.metadata.name,COHORT:.spec.cohortName,PENDING:.status.pendingWorkloads,ADMITTED:.status.admittedWorkloads 2>&1 | tee -a "$LOG"
}

# wait_phase NS NAME WANT TIMEOUT
wait_phase() {
  local ns=$1 name=$2 want=$3 timeout=${4:-90} i
  for ((i=0; i<timeout; i+=3)); do
    local got
    got=$(k -n "$ns" get mltrainingjob "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$got" = "$want" ] && { log "  $ns/$name reached phase $want after ${i}s"; return 0; }
    sleep 3
  done
  log "  TIMEOUT: $ns/$name never reached $want (last: $(k -n "$ns" get mltrainingjob "$name" -o jsonpath='{.status.phase}' 2>/dev/null))"
  return 1
}

submit_job() {
  local name=$1 ns=$2 queue=$3
  cat <<EOF | k apply -f - >>"$LOG" 2>&1
apiVersion: platform.lkhun9311.github.io/v1
kind: MLTrainingJob
metadata:
  name: $name
  namespace: $ns
spec:
  queue: $queue
  image: busybox:1.36
  command: ["sh","-c","sleep 600"]
  gpuCount: 1
  parallelism: 1
  completions: 1
EOF
  log "+ submitted MLTrainingJob $ns/$name (queue $queue)"
}

step "1. create kind cluster ($CLUSTER)"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "cluster $CLUSTER already exists; reusing it (delete it first for a clean run)"
else
  kind create cluster --name "$CLUSTER" --config hack/kind-config.yaml >>"$LOG" 2>&1 || die "kind create cluster"
fi
cap k cluster-info

step "2. install Kueue v0.18.3"
if k -n kueue-system get deploy kueue-controller-manager >/dev/null 2>&1; then
  log "Kueue already installed; reusing it"
else
  k apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.3/manifests.yaml >>"$LOG" 2>&1 || die "kueue apply"
fi
log "waiting for kueue-controller-manager to become Available..."
k -n kueue-system wait --for=condition=Available deploy/kueue-controller-manager --timeout=300s >>"$LOG" 2>&1 || die "kueue not Available"
cap k -n kueue-system get pods

step "3. build and load operator + gpu-simulator images"
run make docker-build IMG=controller:latest || die "operator image build"
run make docker-build-gpu-simulator GPU_SIMULATOR_IMG=gpu-simulator:latest || die "simulator image build"
run kind load docker-image controller:latest --name "$CLUSTER" || die "kind load operator"
run kind load docker-image gpu-simulator:latest --name "$CLUSTER" || die "kind load simulator"

step "4. install CRDs, deploy operator and fake GPU device plugin"
run make install || die "install CRDs"
kustomize build config/operator | k apply --server-side -f - >>"$LOG" 2>&1 || die "deploy operator"
DEP=$(k -n "$NS" get deploy -o name | grep controller-manager | head -1)
log "operator deployment: $DEP"
k -n "$NS" patch "$DEP" --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >>"$LOG" 2>&1 || true

kustomize build config/device-plugin | k apply -f - >>"$LOG" 2>&1 || die "deploy device-plugin"
k -n "$NS" set env ds/gpu-simulator FAKE_GPU_COUNT=2 >>"$LOG" 2>&1 || die "set FAKE_GPU_COUNT"
k -n "$NS" patch ds gpu-simulator --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >>"$LOG" 2>&1 || true

# Force both workloads to pick up the freshly loaded images.
#
# The images use the :latest tag, so re-applying an unchanged manifest would not restart the pods, and a rerun would keep the previous binary.
k -n "$NS" rollout restart "$DEP" >>"$LOG" 2>&1 || true
k -n "$NS" rollout restart ds/gpu-simulator >>"$LOG" 2>&1 || true

log "waiting for operator and device plugin rollouts..."
k -n "$NS" rollout status "$DEP" --timeout=180s >>"$LOG" 2>&1 || die "operator rollout"
k -n "$NS" rollout status ds/gpu-simulator --timeout=180s >>"$LOG" 2>&1 || die "device-plugin rollout"

log "waiting for a node to advertise nvidia.com/gpu..."
for i in $(seq 1 40); do
  cap_gpu=$(k get nodes -o jsonpath='{range .items[*]}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' 2>/dev/null | grep -v '^$' | head -1)
  [ -n "$cap_gpu" ] && { log "  node advertises nvidia.com/gpu=$cap_gpu"; break; }
  sleep 3
done
cap k get nodes -o custom-columns=NODE:.metadata.name,GPU:.status.allocatable.nvidia\\.com/gpu

step "5. apply gpu ResourceFlavor, tenant namespaces, and two GPUQuotaPolicy"
run k apply -f config/kueue/namespaces.yaml || die "namespaces"
run k apply -f config/kueue/resourceflavor.yaml || die "resourceflavor"
# The per-tenant ClusterQueues and LocalQueues are created by the operator from these policies.
#
# The ceiling is 1 GPU per tenant so the two ClusterQueues share one cohort with total nominal 2, which is the deterministic sizing the borrowing and reclaim demo needs.
apply_policy() {
  local tenant=$1
  cat <<EOF | k apply -f - >>"$LOG" 2>&1
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: ${tenant}-quota
spec:
  tenant: ${tenant}
  targetNamespace: ${tenant}
  gpuClass: l40s
  limits:
    gpuCount: 1
  trainingQuota: true
EOF
  log "+ applied GPUQuotaPolicy ${tenant}-quota (ceiling 1)"
}
apply_policy tenant-a || die "policy a"
apply_policy tenant-b || die "policy b"

log "waiting for the operator to create both ClusterQueues with nominal quota 1..."
for i in $(seq 1 40); do
  na=$(k get clusterqueue gpu-tenant-a -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota}' 2>/dev/null)
  nb=$(k get clusterqueue gpu-tenant-b -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota}' 2>/dev/null)
  [ "$na" = "1" ] && [ "$nb" = "1" ] && { log "  both ClusterQueues present, nominal quota 1 each, cohort total 2"; break; }
  sleep 3
done
cap k get clusterqueue -o custom-columns=NAME:.metadata.name,COHORT:.spec.cohortName,NOMINAL:.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota,RECLAIM:.spec.preemption.reclaimWithinCohort
cap k get localqueue -A
# Captured rather than asserted in prose: with trainingQuota true the GPU ceiling must live only in the
# ClusterQueue, and a stale namespace ResourceQuota left by an earlier run is exactly what would silently
# double-count it, so the absence has to be in the evidence log.
cap k get resourcequota -A

step "6. FAIR SHARING: two tenant-a jobs while tenant-b is idle"
log "clearing any MLTrainingJobs from a previous run so the demo starts from an empty cohort..."
k -n tenant-a delete mltrainingjob --all >>"$LOG" 2>&1 || true
k -n tenant-b delete mltrainingjob --all >>"$LOG" 2>&1 || true
for i in $(seq 1 20); do
  n=$(k get workloads -A --no-headers 2>/dev/null | wc -l)
  [ "$n" = "0" ] && { log "  all Workloads cleared"; break; }
  sleep 3
done
log "a1 uses tenant-a's own nominal unit; a2 borrows tenant-b's idle unit from the cohort."
submit_job a1 tenant-a gpu-tenant-a
submit_job a2 tenant-a gpu-tenant-a
wait_phase tenant-a a1 Running 120 || true
wait_phase tenant-a a2 Running 120 || true
log "\n[EVIDENCE] fair sharing: tenant-a borrows past its nominal (a1 + a2 both Running)"
phases

step "7. PREEMPTION: tenant-b reclaims its nominal unit"
log "b1 makes tenant-b claim its own unit; reclaimWithinCohort=Any preempts the borrowed tenant-a job."
submit_job b1 tenant-b gpu-tenant-b
wait_phase tenant-b b1 Running 120 || true
# one of the tenant-a jobs must be pushed back out of Running by the reclaim
log "waiting for a borrowed tenant-a job to be preempted back to Pending..."
preempted=""
for ((i=0; i<120; i+=3)); do
  for j in a1 a2; do
    p=$(k -n tenant-a get mltrainingjob "$j" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [ "$p" = "Pending" ]; then preempted="$j"; break; fi
  done
  [ -n "$preempted" ] && break
  sleep 3
done
if [ -n "$preempted" ]; then
  log "  [EVIDENCE] preemption: tenant-a/$preempted was reclaimed back to Pending after b1 admitted"
else
  log "  NOTE: no tenant-a job observed in Pending within the window; see phases below"
fi
log "\n[EVIDENCE] preemption result"
phases
log "\n--- recent Kueue / preemption events ---"
cap k get events -A --field-selector reason=Preempted
cap k -n tenant-a get events --sort-by=.lastTimestamp

step "DONE"
log "Cluster '$CLUSTER' left running for inspection. Tear down with: kind delete cluster --name $CLUSTER"
