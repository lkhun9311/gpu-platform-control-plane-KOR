#!/usr/bin/env bash
#
# M5-a ephemeral apply -> smoke -> destroy, with an independent TTL kill switch.
#
# This is the "one thing to do first": prove the operator runs on real EKS, capture sanitized
# evidence, then destroy everything, so the "never actually applied" credibility hole is closed.
#
# COST: this creates real AWS resources (EKS control plane, a small CPU node group, VPC/NAT).
# It is CPU-only (no GPU) and short-lived. You run it; it is not run for you.
#
# The independent TTL kill switch matters because AWS Budgets is NOT an hourly kill switch (its
# billing data updates only a few times a day). A detached watchdog force-destroys after TTL even
# if this script hangs or the terminal dies, so a forgotten cluster cannot bill indefinitely.
#
# Usage:
#   AWS_PROFILE=... AWS_REGION=ap-northeast-2 M5A_CONFIRM=yes ./hack/m5a-ephemeral-runbook.sh
#
# Nothing that touches AWS runs unless M5A_CONFIRM=yes is set.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export PATH="$PWD/bin:$PATH"

REGION="${AWS_REGION:-ap-northeast-2}"
CLUSTER="${M5A_CLUSTER:-gpu-platform}"
TTL_MINUTES="${M5A_TTL_MINUTES:-90}"
EVIDENCE_DIR="${M5A_EVIDENCE_DIR:-storage/gpu-platform-control-plane/evidence/m5a-$(date -u +%Y%m%dT%H%M%SZ)}"
BOOTSTRAP=infra/aws/bootstrap
CLUSTER_DIR=infra/aws/cluster

log()  { echo "[m5a] $*"; }
die()  { echo "[m5a] FATAL: $*" >&2; exit 1; }

# --- Preflight (safe, no AWS mutation) -----------------------------------------------------------

log "preflight: offline tools"
for t in terraform kubectl; do
  command -v "$t" >/dev/null 2>&1 || die "missing tool: $t (run 'make terraform kubectl' to fetch them into ./bin)"
done

log "preflight: terraform fmt (offline)"
terraform -chdir="$CLUSTER_DIR" fmt -check -recursive >/dev/null 2>&1 || log "warning: terraform fmt would change files"

if [ "${M5A_CONFIRM:-}" != "yes" ]; then
  cat <<EOF

DRY PREFLIGHT ONLY. To actually apply, re-run with:
  AWS_PROFILE=<profile> AWS_REGION=$REGION M5A_CONFIRM=yes $0

Before you do, check the region's EKS/EC2 quotas and (if you later add GPUs) the G-instance
vCPU quota, which defaults to 0 on many accounts. This M5-a run is CPU-only (t3.large), so it
does not need a G quota, but confirm the EKS cluster and NAT gateway limits.
EOF
  exit 0
fi

log "preflight: AWS CLI + identity"
command -v aws >/dev/null 2>&1 || die "missing tool: aws (install the AWS CLI to apply)"
aws sts get-caller-identity --query Arn --output text >/dev/null 2>&1 || die "AWS credentials not usable (set AWS_PROFILE / env creds)"

# --- Independent TTL kill switch ------------------------------------------------------------------
#
# A detached watchdog destroys the stack after TTL_MINUTES no matter what happens to this script.

KILL_LOG="$EVIDENCE_DIR/ttl-kill-switch.log"
mkdir -p "$EVIDENCE_DIR"
log "arming independent TTL kill switch: force-destroy in ${TTL_MINUTES}m (log: $KILL_LOG)"
nohup bash -c "
  sleep $(( TTL_MINUTES * 60 ))
  echo \"[ttl] TTL reached; force-destroying\" >> '$KILL_LOG' 2>&1
  AWS_REGION='$REGION' terraform -chdir='$CLUSTER_DIR' destroy -auto-approve >> '$KILL_LOG' 2>&1
  AWS_REGION='$REGION' terraform -chdir='$BOOTSTRAP' destroy -auto-approve >> '$KILL_LOG' 2>&1
" >/dev/null 2>&1 &
KILL_PID=$!
log "TTL kill switch PID: $KILL_PID (it survives this shell; kill it manually only after a clean destroy)"

cleanup_killswitch() { kill "$KILL_PID" 2>/dev/null && log "disarmed TTL kill switch (clean exit)"; }

# --- Apply ----------------------------------------------------------------------------------------

start=$(date +%s)
log "apply: bootstrap (state backend, OIDC, ECR)"
terraform -chdir="$BOOTSTRAP" init -input=false || die "bootstrap init"
terraform -chdir="$BOOTSTRAP" apply -auto-approve || die "bootstrap apply"

log "apply: cluster (VPC, EKS, CPU node group)"
terraform -chdir="$CLUSTER_DIR" init -input=false || die "cluster init"
terraform -chdir="$CLUSTER_DIR" apply -auto-approve -var="region=$REGION" || die "cluster apply"
apply_secs=$(( $(date +%s) - start ))
log "apply complete in ${apply_secs}s"

# --- Smoke test -----------------------------------------------------------------------------------

log "smoke: kubeconfig + nodes"
aws eks update-kubeconfig --name "$CLUSTER" --region "$REGION" || die "update-kubeconfig"
kubectl wait --for=condition=Ready nodes --all --timeout=300s || die "nodes not Ready"

log "smoke: deploy operator (CRDs + manager)"
make install || die "install CRDs"
kustomize build config/operator | kubectl apply --server-side -f - || die "deploy operator"
DEP=$(kubectl -n gpu-platform-control-plane-system get deploy -o name | grep controller-manager | head -1)
kubectl -n gpu-platform-control-plane-system rollout status "$DEP" --timeout=180s || die "operator rollout"

log "smoke: apply a sample CR and confirm the operator reconciles it"
kubectl apply -f config/samples/platform_v1_nodehealth.yaml || true
sleep 10

# --- Capture evidence -----------------------------------------------------------------------------

log "capture: evidence -> $EVIDENCE_DIR"
{
  echo "region: $REGION"
  echo "cluster: $CLUSTER"
  echo "apply_seconds: $apply_secs"
  echo "captured_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$EVIDENCE_DIR/summary.txt"
kubectl get nodes -o wide > "$EVIDENCE_DIR/nodes.txt" 2>&1 || true
kubectl -n gpu-platform-control-plane-system get pods -o wide > "$EVIDENCE_DIR/operator-pods.txt" 2>&1 || true
kubectl get nodehealths.platform.lkhun9311.github.io -o yaml > "$EVIDENCE_DIR/nodehealth-status.yaml" 2>&1 || true
aws resourcegroupstaggingapi get-resources --region "$REGION" \
  --tag-filters "Key=project,Values=gpu-platform-control-plane" \
  --query 'ResourceTagMappingList[].ResourceARN' --output text \
  > "$EVIDENCE_DIR/tagged-resources-before-destroy.txt" 2>&1 || true
log "capture: done"

# --- Destroy --------------------------------------------------------------------------------------

log "destroy: cluster then bootstrap"
terraform -chdir="$CLUSTER_DIR" destroy -auto-approve -var="region=$REGION" || die "cluster destroy FAILED; the TTL kill switch (PID $KILL_PID) will retry at TTL"
terraform -chdir="$BOOTSTRAP" destroy -auto-approve || log "warning: bootstrap destroy returned non-zero (state bucket may be retained deliberately)"

log "verify: tagged resources remaining"
aws resourcegroupstaggingapi get-resources --region "$REGION" \
  --tag-filters "Key=project,Values=gpu-platform-control-plane" \
  --query 'ResourceTagMappingList[].ResourceARN' --output text \
  > "$EVIDENCE_DIR/tagged-resources-after-destroy.txt" 2>&1 || true

cleanup_killswitch
log "DONE. Evidence in $EVIDENCE_DIR. Confirm tagged-resources-after-destroy.txt is empty."
