#!/usr/bin/env bash
#
# M7 end to end: inject a failure, and let the trail record what it saw.
#
# The point is not that a controller compiles. It is that the evidence for a chaos scenario is ASSEMBLED
# from the cluster rather than written by hand afterwards -- which is how the pages this repository has had
# to retract came to be wrong.
#
# It builds its OWN kind cluster and deletes it. That is not politeness: driving a WorkloadRun needs a
# reconciler running against the apiserver, and pointing one at a cluster that already has an operator puts
# two reconcilers on the same objects. A throwaway cluster is the only way to run this without touching
# whatever else is deployed.
#
# The target is an ordinary web server rather than a GPU workload, and deliberately so: M7 is about the
# recorder, and using a card here would make an evidence-trail test depend on hardware it does not need.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

# Two ways to run, and the difference is which cluster owns the risk.
#
# With KCTX set, this ADOPTS an existing cluster: it installs the WorkloadRun CRD, runs in its own
# namespace, and removes both afterwards. That is safe for a specific reason -- workloadrunctl reconciles
# WorkloadRun and nothing else, and no other controller in a cluster reconciles WorkloadRun -- so unlike
# running the manager there is nothing to contend with. It is also BETTER evidence: a cluster with the
# operator deployed publishes the target's phase itself, so the trail records transitions this script did
# not author.
#
# Without KCTX it builds a throwaway cluster, which is what a machine with no cluster needs and what a
# machine at the default inotify limit cannot do.
CLUSTER="${CLUSTER:-m7-evidence}"
ADOPTED=""
if [ -n "${KCTX:-}" ]; then
  ADOPTED=1
else
  KCTX="kind-$CLUSTER"
fi
NS=m7
LOG=hack/m7-evidence-trail.log
# Not :latest, for the reason hack/m5b-gateway-path.sh gives: Kubernetes defaults imagePullPolicy to Always
# for a :latest tag, which sends the kubelet looking for a registry that does not have a side-loaded image.
STUB_IMG="${STUB_IMG:-benchharness:m7}"
WORK="$(mktemp -d)"

k() { kubectl --context "$KCTX" "$@"; }
say() { echo "== $*" | tee -a "$LOG"; }
note() { echo "$*" | tee -a "$LOG"; }
fail() { echo "M7 FAILED: $*" | tee -a "$LOG" >&2; exit 1; }

cleanup() {
  [ -n "${DRIVER_PID:-}" ] && kill "$DRIVER_PID" 2>/dev/null
  [ -n "${PUB_PID:-}" ] && kill "$PUB_PID" 2>/dev/null
  if [ -n "${KEEP:-}" ]; then
    say "KEEP set: leaving everything in place"
    rm -rf "$WORK"; return
  fi
  if [ -n "$ADOPTED" ]; then
    # Remove exactly what was added, and nothing that was already there. The CRD is cluster-scoped, so
    # leaving it behind would change a cluster this script only borrowed.
    say "remove what this run added to the adopted cluster"
    k delete namespace "$NS" --wait=false >/dev/null 2>&1
    [ -n "$INSTALLED_CRD" ] && k delete crd workloadruns.platform.lkhun9311.github.io >/dev/null 2>&1
  else
    say "delete the throwaway cluster"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

: > "$LOG"

INSTALLED_CRD=""
if [ -z "$ADOPTED" ]; then
  # Refuse to run against anything that already exists. Reusing a cluster by that name would make this
  # script's isolation a claim rather than a fact.
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    fail "a cluster named $CLUSTER already exists; this script owns its cluster and will not adopt one"
  fi
fi

# The failure this catches is real, was hit here, and its message names neither inotify nor a limit.
#
# A second kind cluster on a host whose fs.inotify.max_user_instances is the default 128 dies during node
# preparation with "could not find a log line that matches ... Multi-User System", and the cause is only
# visible inside the node that kind has already deleted: systemd reporting "Failed to create control group
# inotify object: Too many open files". Every kind node consumes instances, so an existing cluster plus an
# editor and a docker daemon is enough to exhaust them.
instances=$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)
if [ -n "$ADOPTED" ]; then instances=999999; fi
running=$(docker ps --filter label=io.x-k8s.kind.cluster --format '{{.Names}}' 2>/dev/null | wc -l)
if [ "${instances:-0}" -lt 256 ] && [ "${running:-0}" -gt 0 ]; then
  cat <<EOF

fs.inotify.max_user_instances is $instances and $running kind node(s) are already running. Creating another
cluster will fail during node preparation with a message about a missing log line, which is not what went
wrong. Raise the limit first:

    sudo sysctl fs.inotify.max_user_instances=512

To make it survive a reboot:

    echo 'fs.inotify.max_user_instances=512' | sudo tee /etc/sysctl.d/99-kind.conf

EOF
  fail "not enough inotify instances for another kind cluster"
fi

if [ -z "$ADOPTED" ]; then
  say "create the throwaway cluster"
  kind create cluster --name "$CLUSTER" --wait 120s >/dev/null 2>&1 || fail "create cluster"
  say "install the CRDs"
  k apply -k config/crd >/dev/null || fail "apply CRDs"
  INSTALLED_CRD=1
else
  say "adopt cluster context $KCTX"
  k get ns "$NS" >/dev/null 2>&1 && fail "namespace $NS already exists in the adopted cluster; this script will not adopt one it did not make"
  if ! k get crd workloadruns.platform.lkhun9311.github.io >/dev/null 2>&1; then
    say "install the WorkloadRun CRD (and remove it afterwards)"
    k apply -f config/crd/bases/platform.lkhun9311.github.io_workloadruns.yaml >/dev/null || fail "apply the WorkloadRun CRD"
    INSTALLED_CRD=1
  fi
fi
k wait --for=condition=Established crd/workloadruns.platform.lkhun9311.github.io --timeout=60s >/dev/null \
  || fail "the WorkloadRun CRD never established"

say "create the target"
k create ns "$NS" >/dev/null || fail "create namespace"

# The image is this repository's own stub, and it is the only one that satisfies the operator's contract:
# the InferenceDeployment controller passes --model and --model-path to every serving container it builds
# and probes GET /health on the named port. hashicorp/http-echo -- which the cluster's existing stub-llm
# uses, and which has sat Pending -- does neither. A stub that cannot be deployed as an InferenceDeployment
# is not standing in for one.
say "build and load the stub"
go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY benchharness /benchharness\nUSER 65532:65532\nENTRYPOINT ["/benchharness","stub-serve"]\n' > "$WORK/Dockerfile"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness (static)"
docker build -q -t "$STUB_IMG" "$WORK" >/dev/null || fail "build the stub image"
kind load docker-image "$STUB_IMG" --name "${KCTX#kind-}" >/dev/null 2>&1 || fail "load the stub image into $KCTX"

# In an ADOPTED cluster the operator builds the Deployment and Service from this record, and that is the
# point: the phase the trail reads is the platform's own. Creating the Deployment first is what the last
# attempt did, and the operator refused to adopt it and marked the record Degraded for the whole window --
# correct behaviour, and a run that recorded a constant.
k apply -n "$NS" -f - >/dev/null <<EOF || fail "apply the InferenceDeployment"
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata: {name: m7-target}
spec:
  # ready-after-ms makes the outage OBSERVABLE rather than lucky. Without it the replacement Pod is healthy
  # inside a second, the dip falls between polls, and successive runs disagreed about whether the injected
  # failure had happened at all -- one recorded Ready/Pending/Ready and the next recorded only Ready. A real
  # engine loads before it serves; this is the stub being less unlike one.
  model: {name: demo, storageUri: "stub://demo?ready-after-ms=8000"}
  image: $STUB_IMG
  gpuCount: 0
  replicas: 1
  port: 8090
EOF
say "wait for the platform to report it Ready"
for i in $(seq 1 60); do
  ph=$(k get inferencedeployment m7-target -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$ph" = Ready ] && break
  [ "$i" = 60 ] && fail "the target never reached Ready (phase ${ph:-<none>}); a run starting from Degraded would record no recovery and blame the platform for a setup failure"
  sleep 2
done
note "target phase: $ph"

# phaseOf mirrors what an operator would publish, derived from the Deployment's own readiness rather than
# asserted: a script that simply wrote "Ready" would be testing the recorder against a constant.
publish_phase() {
  # In an adopted cluster the real operator owns this field. Writing it here would be this script
  # answering its own question, so it stands aside and lets the platform report.
  if [ -n "$ADOPTED" ]; then
    k get inferencedeployment m7-target -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null
    return
  fi
  local ready
  ready=$(k get deploy m7-target -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
  local phase=Degraded
  [ "${ready:-0}" -ge 1 ] 2>/dev/null && phase=Ready
  k patch inferencedeployment m7-target -n "$NS" --subresource=status --type=merge \
    -p "{\"status\":{\"phase\":\"$phase\"}}" >/dev/null 2>&1
  echo "$phase"
}
publish_phase >/dev/null

say "create the WorkloadRun"
k apply -n "$NS" -f - >/dev/null <<EOF || fail "apply the WorkloadRun"
apiVersion: platform.lkhun9311.github.io/v1
kind: WorkloadRun
metadata: {name: fr002}
spec:
  scenario: ServingPodKilled
  target: {kind: InferenceDeployment, name: m7-target, namespace: $NS}
  observationWindowSeconds: 60
  recoversWithinSeconds: 45
EOF

say "build and start the recorder"
go build -o "$WORK/workloadrunctl" ./cmd/workloadrunctl || fail "build workloadrunctl"
KUBECONFIG_CTX="$KCTX" kubectl config use-context "$KCTX" >/dev/null 2>&1
# One second rather than the controller's shipped five. A Pod on kind is replaced in a couple of seconds,
# and a five-second poll steps straight over the outage: the run then reports Recovered at 0s having never
# seen the target leave Ready, which is a trail about a constant. The gap tolerance is a multiple of the
# poll, so tightening one tightens the other and the hole check stays proportionate.
"$WORK/workloadrunctl" -name fr002 -namespace "$NS" -poll 1s -timeout 4m > "$WORK/driver.out" 2>"$WORK/driver.err" &
DRIVER_PID=$!

# Keep the target's published phase honest while the window is open. Without this the run would watch a
# field nobody updates and record a platform that never changed -- which is the shape of the hand-written
# page this milestone replaces.
publisher() {
  while kill -0 "$DRIVER_PID" 2>/dev/null; do
    publish_phase >/dev/null
    sleep 2
  done
}
if [ -z "$ADOPTED" ]; then
  publisher &
  PUB_PID=$!
fi

sleep 8
say "inject: delete the serving Pod"
# The operator labels the Pods it builds with app.kubernetes.io/instance, not with whatever this script
# would have chosen. Selecting on the wrong label found nothing and reported "no serving Pod to delete",
# which reads as a platform that never started rather than as a query that never matched.
sel="app.kubernetes.io/instance=m7-target"
[ -z "$ADOPTED" ] && sel="app=m7-target"
victim=$(k get pod -n "$NS" -l "$sel" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -n "$victim" ] || fail "no serving Pod to delete"
note "deleting $victim"
k delete pod "$victim" -n "$NS" --wait=false >/dev/null || fail "delete the serving Pod"

wait "$DRIVER_PID"
[ -n "${PUB_PID:-}" ] && kill "$PUB_PID" 2>/dev/null
cat "$WORK/driver.out" | tee -a "$LOG"

say "the trail"
k get workloadrun fr002 -n "$NS" -o jsonpath='{.status.phase} verdict={.status.verdict} recoveredAt={.status.recoveredAtSeconds}s{"\n"}' | tee -a "$LOG"
k get workloadrun fr002 -n "$NS" -o jsonpath='{range .status.observations[*]}  {.elapsedSeconds}s {.state} healthy={.healthy}{"\n"}{end}' | tee -a "$LOG"

phase=$(k get workloadrun fr002 -n "$NS" -o jsonpath='{.status.phase}')
obs=$(k get workloadrun fr002 -n "$NS" -o jsonpath='{.status.observations[*].state}' | wc -w)
[ "$phase" = Complete ] || fail "the run ended in phase $phase rather than Complete; a trail that refuses is correct behaviour but is not the end-to-end evidence this script exists to produce"
# More than one state, or the recorder watched something that never moved and this proves only that it can
# poll. The injected failure is here precisely so the trail has a transition to carry.
[ "$obs" -ge 2 ] || fail "the trail carries $obs state(s); the injected failure produced no observed transition, so this run is evidence about a constant"

say "M7 OK: a real Pod deletion produced a real transition in a trail nobody wrote by hand."
