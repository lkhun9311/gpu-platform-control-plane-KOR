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
#
# The throwaway cluster deploys the operator, so the distinction the paragraph above draws no longer favours
# adopting: the trail records transitions this script did not author in either mode.
#
# That fixed ServingPodKilled in throwaway mode, which had never worked. The cluster installed CRDs and no
# operator, nothing published the InferenceDeployment's status.phase, and the wait for the target to report
# Ready could not pass. The script had been writing that phase itself from the Deployment's readyReplicas --
# derived rather than asserted, but still the recorder's own author answering its own question. Standing the
# operator up costs about ninety seconds and removes both.
# SCENARIO selects which injected failure this run records.
#
# ServingPodKilled is what this script has always driven. DegradedNode was declared in the CRD's enum and
# never driven, and the type says why: "the injection stops a kubelet, which is disruptive to whatever else
# is on the cluster, and the script adopts a cluster it does not own. Driving it needs a machine whose
# disruption nobody minds."
#
# A throwaway kind cluster IS such a machine, and this script already builds one. What it did not build was
# a cluster with a second node -- stopping the control plane's kubelet takes the apiserver with it, and a
# recorder cannot watch a cluster it can no longer reach. DegradedNode therefore gets a worker, and the
# kubelet that stops is the worker's.
SCENARIO="${SCENARIO:-ServingPodKilled}"
case "$SCENARIO" in
  ServingPodKilled | DegradedNode) ;;
  *)
    echo "M7 FAILED: SCENARIO must be ServingPodKilled or DegradedNode; got '$SCENARIO'" >&2
    exit 1
    ;;
esac

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
  if [ "$SCENARIO" = DegradedNode ]; then
    # A worker, because the fault is a stopped kubelet and stopping the control plane's takes the apiserver
    # with it. The recorder polls the apiserver, so a run that killed it would record nothing and blame the
    # platform for the script's choice of node.
    cat > "$WORK/kind.yaml" <<KINDEOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $CLUSTER
nodes:
  - role: control-plane
  - role: worker
KINDEOF
    kind create cluster --config "$WORK/kind.yaml" --wait 120s >/dev/null 2>&1 || fail "create cluster"
  else
    kind create cluster --name "$CLUSTER" --wait 120s >/dev/null 2>&1 || fail "create cluster"
  fi
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

# The operator is deployed for BOTH scenarios, and that is a change in what this script's evidence is.
#
# It used to run only for DegradedNode, because a NodeHealth phase has no honest local substitute. The
# serving path meanwhile derived its target's phase from the Deployment's readyReplicas and wrote it itself,
# which the header calls the weaker evidence -- and which also meant ServingPodKilled could not work in a
# throwaway cluster at all: the wait for the target to report Ready ran before anything published a phase.
#
# Standing the operator up costs about ninety seconds and removes both problems. The trail now records
# transitions this script did not author in either scenario, on a cluster it owns.

if [ -z "$ADOPTED" ]; then
  # Kueue first, because the operator does not start without it.
  #
  # Measured rather than assumed: without these CRDs the manager exits 1 at startup with
  #   Failed to create controller {"controller": "mltrainingjob",
  #     "error": "index workloads by job ref: no matches for kind \"Workload\" in version
  #      \"kueue.x-k8s.io/v1beta1\""}
  # It builds a field index over Kueue Workloads before it serves anything, so a cluster without that kind
  # gives a CrashLoopBackOff and nothing to publish the NodeHealth phase. The NodeHealth controller itself
  # needs no Kueue; the binary they ship in does.
  say "install Kueue, which the operator indexes at startup"
  k apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.3/manifests.yaml >/dev/null \
    || fail "install Kueue"
  k -n kueue-system wait --for=condition=Available deploy/kueue-controller-manager --timeout=300s >/dev/null \
    || fail "Kueue never became Available"

  say "build and load the operator"
  make docker-build IMG=controller:m7 >/dev/null 2>&1 || fail "build the operator image"
  kind load docker-image controller:m7 --name "$CLUSTER" >/dev/null 2>&1 || fail "load the operator image"
  # The manifests pin an ECR digest, and this cluster has no credentials for it. Repointing at the
  # side-loaded tag is the same fix hack/queuelab-gpu-session.sh makes, for the same reason.
  sed -i -e 's|^    newName: .*|    newName: controller|' \
         -e 's|^    digest: .*|    newTag: m7|' config/manager/kustomization.yaml
  say "deploy the operator"
  k apply --server-side -k config/operator >/dev/null || fail "deploy the operator"
  git checkout -- config/manager/kustomization.yaml
  k -n gpu-platform-control-plane-system patch deploy gpu-platform-control-plane-controller-manager \
    --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
    >/dev/null || fail "patch the operator's pull policy"
  k -n gpu-platform-control-plane-system rollout status \
    deploy/gpu-platform-control-plane-controller-manager --timeout=180s >/dev/null \
    || fail "the operator never became Available, so nothing would publish the target's phase"
fi

# The namespace holds the WorkloadRun in both scenarios, so it is created before the branch. It used to be
# created inside the serving path, which is where the only run lived.
say "create the run's namespace"
k create ns "$NS" >/dev/null || fail "create namespace"

if [ "$SCENARIO" = DegradedNode ]; then
  # The operator runs, and that is the point rather than a convenience.
  #
  # This script's own header says an adopted cluster with the operator deployed is BETTER evidence, because
  # the platform publishes the target's phase and the trail records transitions the script did not author.
  # For a NodeHealth target there is no honest alternative: deriving the phase here would mean this script
  # deciding whether the node it just broke counts as broken, which is the recorder judging its own work.

  NODE="$CLUSTER-worker"
  k get node "$NODE" >/dev/null 2>&1 || fail "no node named $NODE to degrade"
  say "create the NodeHealth target for $NODE"
  k apply -f - >/dev/null <<EOF || fail "create the NodeHealth"
apiVersion: platform.lkhun9311.github.io/v1
kind: NodeHealth
metadata: {name: m7-$NODE}
spec:
  nodeName: $NODE
  gpuClass: l40s
EOF
  for i in $(seq 1 60); do
    ph=$(k get nodehealth "m7-$NODE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$ph" = Ready ] && break
    [ "$i" = 60 ] && fail "NodeHealth never reached Ready (phase ${ph:-<none>}); a run starting anywhere else records no recovery"
    sleep 2
  done
  note "NodeHealth phase: $ph -- published by the operator, not by this script"
else

say "create the target"

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


fi

say "create the WorkloadRun"
if [ "$SCENARIO" = DegradedNode ]; then
  # A longer window than the pod scenario's, because the observable is slower and not by this platform's
  # doing. A stopped kubelet becomes NotReady only after Kubernetes' own node-monitor-grace-period, which is
  # tens of seconds on a default cluster; the NodeHealth controller's own reaction is a fraction of a second
  # after that. hack/chaos-fr004-degraded-node.md separates the two latencies and only the second is ours.
  k apply -f - >/dev/null <<EOF || fail "apply the WorkloadRun"
apiVersion: platform.lkhun9311.github.io/v1
kind: WorkloadRun
metadata: {name: fr004, namespace: $NS}
spec:
  scenario: DegradedNode
  target: {kind: NodeHealth, name: m7-$NODE}
  observationWindowSeconds: 240
  recoversWithinSeconds: 200
EOF
  RUN_NAME=fr004
else
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
  RUN_NAME=fr002
fi

say "build and start the recorder"
go build -o "$WORK/workloadrunctl" ./cmd/workloadrunctl || fail "build workloadrunctl"
KUBECONFIG_CTX="$KCTX" kubectl config use-context "$KCTX" >/dev/null 2>&1
# One second rather than the controller's shipped five. A Pod on kind is replaced in a couple of seconds,
# and a five-second poll steps straight over the outage: the run then reports Recovered at 0s having never
# seen the target leave Ready, which is a trail about a constant. The gap tolerance is a multiple of the
# poll, so tightening one tightens the other and the hole check stays proportionate.
"$WORK/workloadrunctl" -name "$RUN_NAME" -namespace "$NS" -poll 1s -timeout 6m \
  > "$WORK/driver.out" 2>"$WORK/driver.err" &
DRIVER_PID=$!

# Keep the target's published phase honest while the window is open. Without this the run would watch a
# field nobody updates and record a platform that never changed -- which is the shape of the hand-written
# page this milestone replaces.
# No publisher. The operator publishes the target's phase in both scenarios now, on a cluster this script
# owns, so there is nothing for it to keep honest on the platform's behalf. Keeping a writer here beside a
# running controller would put two authors on one status field, and the trail could not say which it recorded.

sleep 8
if [ "$SCENARIO" = DegradedNode ]; then
  say "inject: stop the kubelet on $NODE"
  # Stopped, not written. Setting NotReady in the Node's status measures nothing -- the live kubelet answers
  # within a heartbeat, so the experiment would time how quickly the injection was overwritten.
  # hack/chaos-fr004-degraded-node.md establishes that, and this uses the same injection for the same reason.
  docker exec "$NODE" systemctl stop kubelet >/dev/null 2>&1 || fail "could not stop the kubelet on $NODE"
  note "kubelet stopped; Kubernetes' node-monitor-grace-period decides when NotReady arrives, not this platform"

  # Wait for the degradation to become visible, then undo it. The recorder is watching throughout; this only
  # decides when the fault ends, and a fault that never ends produces a window with no recovery in it.
  for i in $(seq 1 90); do
    ph=$(k get nodehealth "m7-$NODE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$ph" = Quarantine ] && break
    sleep 2
  done
  if [ "${ph:-}" != Quarantine ]; then
    docker exec "$NODE" systemctl start kubelet >/dev/null 2>&1 || true
    fail "NodeHealth never left Ready (phase ${ph:-<none>}) within 180s of the kubelet stopping; either the grace period is longer than this window or the controller is not reacting"
  fi
  note "NodeHealth phase: Quarantine -- the platform saw the degradation"
  say "recover: start the kubelet again"
  docker exec "$NODE" systemctl start kubelet >/dev/null 2>&1 || fail "could not restart the kubelet on $NODE"
else
  say "inject: delete the serving Pod"
  # The operator labels the Pods it builds with app.kubernetes.io/instance, not with whatever this script
  # would have chosen. Selecting on the wrong label found nothing and reported "no serving Pod to delete",
  # which reads as a platform that never started rather than as a query that never matched.
  # One selector, because there is one thing building these Pods now.
  #
  # This used to branch on ADOPTED and look for app=m7-target in a throwaway cluster, which was right while
  # nothing but this script created the Deployment there. The operator runs in both modes now, so the Pods
  # carry its label in both, and the old override found nothing and reported "no serving Pod to delete" --
  # which reads as a platform that never started rather than as a query that never matched.
  sel="app.kubernetes.io/instance=m7-target"
  victim=$(k get pod -n "$NS" -l "$sel" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -n "$victim" ] || fail "no serving Pod to delete"
  note "deleting $victim"
  k delete pod "$victim" -n "$NS" --wait=false >/dev/null || fail "delete the serving Pod"
fi

wait "$DRIVER_PID"
cat "$WORK/driver.out" | tee -a "$LOG"
# The recorder's stderr is READ, not merely redirected. It was written to a file nobody opened, so when the
# driver was pointed at a run that did not exist it said so into the void and the script reported a trail
# with one observation as though the recorder had simply seen nothing happen.
if [ -s "$WORK/driver.err" ]; then
  say "the recorder wrote to stderr"
  sed 's/^/  /' "$WORK/driver.err" | tee -a "$LOG"
fi

say "the trail"
k get workloadrun "$RUN_NAME" -n "$NS" -o jsonpath='{.status.phase} verdict={.status.verdict} recoveredAt={.status.recoveredAtSeconds}s{"\n"}' | tee -a "$LOG"
k get workloadrun "$RUN_NAME" -n "$NS" -o jsonpath='{range .status.observations[*]}  {.elapsedSeconds}s {.state} healthy={.healthy}{"\n"}{end}' | tee -a "$LOG"

phase=$(k get workloadrun "$RUN_NAME" -n "$NS" -o jsonpath='{.status.phase}')
obs=$(k get workloadrun "$RUN_NAME" -n "$NS" -o jsonpath='{.status.observations[*].state}' | wc -w)
[ "$phase" = Complete ] || fail "the run ended in phase $phase rather than Complete; a trail that refuses is correct behaviour but is not the end-to-end evidence this script exists to produce"
# More than one state, or the recorder watched something that never moved and this proves only that it can
# poll. The injected failure is here precisely so the trail has a transition to carry.
[ "$obs" -ge 2 ] || fail "the trail carries $obs state(s); the injected failure produced no observed transition, so this run is evidence about a constant"

if [ "$SCENARIO" = DegradedNode ]; then
  say "M7 OK: a stopped kubelet produced a real transition in a NodeHealth trail the operator published."
else
  say "M7 OK: a real Pod deletion produced a real transition in a trail nobody wrote by hand."
fi
