#!/usr/bin/env bash
# Prepare every worker a GPU session will measure on, and then run the study through the routes it verified.
#
# The runner is a process on somebody's laptop and dcgm-exporter is a DaemonSet Pod, so -device-metrics needs
# a URL that resolves from outside the cluster. The Service in front of the exporter is HEADLESS on purpose --
# a round-robin one would answer alternate scrapes from a different node's card, producing an observation
# that interleaves two machines -- so there is no cluster IP to forward to and the forward must name a POD,
# specifically the pod on the worker under test.
#
# Getting that wrong is silent: a forward to the wrong node's exporter scrapes cleanly, parses cleanly, and
# reports that the run's Pod was never seen on any card, which reads as a device fault.
#
# EVERYTHING HERE IS PER WORKER, and that is the correction a review forced. An earlier version discovered
# one exporter, occupied one node's surplus, opened one forward, and then scheduled four of its twelve runs
# on a SECOND worker through the first one's exporter -- the exact failure the paragraph above says this file
# exists to prevent, committed inside it. The second node also had no occupier, so its first run would have
# been refused anyway, after the first run's budget was spent.
#
# It also never took the termination canary, which every run's qualification requires and only
# `-termination-canary` writes. That means the study path had never completed a single run anywhere.
set -euo pipefail

cd "$(dirname "$0")/.."

NS="${NS:-gpu-platform-control-plane-system}"
REQUIRED="${REQUIRED:-2}"
START_AT="${START_AT:-1}"
# Repetitions per cell. Two was the kind study's allocation and it is the weakest thing about the design:
# with n=2 a cell's spread is one number and cannot be told from its own noise. Four is where a within-cell
# spread starts to mean something, and on rented hardware it costs about three minutes a run -- the cheapest
# axis this study can buy.
REPS="${REPS:-4}"

# Records go in a directory of this session's own, and the comparisons are scoped to it.
#
# They used to be written flat into ex/ and compared with globs like 'ex/gpu-*-A-honor-*.json'. Those globs
# match every record any previous session ever wrote there. A resumed session, a second attempt after a
# botched arm, or simply a second day would silently pool evidence from different clusters, different nodes
# and different engine builds into one comparison -- and nothing in the record identifies which session it
# came from, so nothing downstream could notice.
#
# A directory is the cheap half of the fix and it removes the accident. The other half is a session identity
# stamped INTO each record so a hand-written glob spanning two sessions is refused rather than merely
# unlikely; that needs a record schema bump and is not done.
#
# EXDIR can be set to re-enter an interrupted session deliberately, which is what START_AT is for.
EXDIR="${EXDIR:-ex/session-$(date -u +%Y%m%dT%H%M%SZ)}"

if [[ $# -lt 1 ]]; then
  cat >&2 <<USAGE
usage: $0 <worker-node> [second-worker-node]

  Verifies each worker: no fake device plugin, surplus held, termination canary, device preflight.
  Stops there unless RUN_STUDY=1, in which case it runs the study through the routes it just verified.

  RUN_STUDY=1   run the study after verifying
  START_AT=n    resume the study at run n (records before it are kept)
  REQUIRED=$REQUIRED    devices the protocol needs on each worker
  NS=$NS
USAGE
  exit 2
fi
WORKERS=("$@")
# Two workers at most, and they must differ. The sequence chooses its twelve-run form from the ARGUMENT
# COUNT, so `gpu-session.sh nodeA nodeA` used to prepare one machine twice, open two routes to the same
# exporter, run all twelve there, and still print the node-comparison command -- twelve paid runs after an
# input that could never deliver the axis they were bought for. A third worker was prepared and billed and
# then never appeared in the sequence at all.
if [[ ${#WORKERS[@]} -gt 2 ]]; then
  echo "at most two workers: the study's sequence uses two, and a third would be prepared and billed" >&2
  echo "  without contributing a record" >&2
  exit 2
fi
if [[ ${#WORKERS[@]} -eq 2 && "${WORKERS[0]}" == "${WORKERS[1]}" ]]; then
  echo "the two workers are the same node (${WORKERS[0]}); the node axis needs the node to vary" >&2
  exit 2
fi

# Per worker, filled in by prepare().
declare -A URL_OF OBSERVER_OF POD_OF OCCUPIER_OF
FORWARDS=()
OCCUPIERS=()

cleanup() {
  for pid in "${FORWARDS[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  # Occupiers are deleted rather than left: they hold devices, and a leftover one silently changes what the
  # next session's qualification computes. The gate would refuse rather than mismeasure, but refusing for a
  # reason nobody can see is a bad way to start an hour that costs money.
  for spec in "${OCCUPIERS[@]:-}"; do
    [[ -n "$spec" ]] && kubectl delete pod -n "$NS" "$spec" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# refuseFakePlugin stops a session whose node is advertising SIMULATED devices.
#
# config/device-plugin is the kind cluster's fake plugin: it advertises nvidia.com/gpu on nodes that have no
# cards, tolerates everything, and is documented as mutually exclusive with the real one but toggled by hand.
# Left running on a GPU node it fights the real plugin for the same resource name. Every outcome I can
# construct from that is a loud refusal somewhere, but it is a manual pre-step nothing checked.
refuseFakePlugin() {
  local worker="$1" sim
  sim="$(kubectl get pods -n "$NS" -l app.kubernetes.io/component=gpu-simulator \
    --field-selector "spec.nodeName=$worker" -o name 2>/dev/null | head -1 || true)"
  if [[ -n "$sim" ]]; then
    echo "$worker is running the SIMULATED device plugin ($sim)." >&2
    echo "  It advertises nvidia.com/gpu on nodes with no cards and is mutually exclusive with the real" >&2
    echo "  plugin. Remove it before measuring: kubectl delete -k config/device-plugin" >&2
    return 1
  fi
}

# occupySurplus holds the devices the protocol must NOT have.
#
# The arm contrast is physical card scarcity -- the owner's Pod waits because the victim holds the only free
# device -- so a node advertising more than the protocol needs collapses it while every other figure looks
# normal. No rentable instance carries exactly two well-supported cards and the device plugin has no
# supported way to advertise a subset, so the surplus is held by a Pod the qualification recognises.
occupySurplus() {
  local worker="$1" allocatable surplus name image phase
  allocatable="$(kubectl get node "$worker" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)"
  : "${allocatable:=0}"
  if ! [[ "$allocatable" =~ ^[0-9]+$ ]]; then
    echo "$worker advertises no readable nvidia.com/gpu capacity (${allocatable@Q})" >&2
    return 1
  fi
  if (( allocatable < REQUIRED )); then
    echo "$worker advertises $allocatable devices and the protocol needs $REQUIRED" >&2
    return 1
  fi
  (( allocatable == REQUIRED )) && return 0

  surplus=$((allocatable - REQUIRED))
  name="queuelab-surplus-occupier-$worker"
  # The workload's own pinned image, read from the source rather than restated. An unpinned tag here would be
  # the drift this repository refuses everywhere else, and a silent fallback to one would be worse than
  # failing: the occupier would still hold the cards and nothing downstream would notice.
  # `|| true` because under `set -euo pipefail` a non-matching grep kills the script at the assignment,
  # BEFORE the guard below -- so the diagnostic could never run. Verified: the old form exited 1 silently.
  image="$(grep -oE '"python:[^"]+"' internal/queuelab/submit.go | head -1 | tr -d '"' || true)"
  if [[ -z "$image" ]]; then
    echo "cannot read the pinned workload image from internal/queuelab/submit.go" >&2
    return 1
  fi
  echo "  surplus    : $allocatable advertised, $REQUIRED needed; holding $surplus"
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NS
  labels:
    # The key is the marker; the value is not read by the gate. Label values cannot carry prose -- the API
    # server rejects anything outside alphanumerics, dashes, dots and underscores -- so the explanation is an
    # annotation.
    queuelab.gpu-platform/surplus-occupier: "session"
  annotations:
    queuelab.gpu-platform/why: >-
      Holds the devices the protocol must not have, so the owner's Pod has to wait for the victim's card.
spec:
  restartPolicy: Never
  nodeName: $worker
  tolerations:
    - key: nvidia.com/gpu
      operator: Exists
      effect: NoSchedule
  # system-node-critical, and cpu/memory REQUESTS rather than a device limit alone. Without them this Pod is
  # BestEffort with restartPolicy Never and no priority: the first thing evicted under node pressure. If it
  # goes, the surplus cards free up mid-run, Kueue's admitted owner binds immediately instead of waiting for
  # the victim's card, and the arm contrast collapses -- producing exactly the plausible, internally
  # consistent record the qualification's own comment names as the undetectable failure, through the
  # mechanism added to prevent it. It is the one path in this script that yields a wrong number rather than
  # a refusal.
  priorityClassName: system-node-critical
  containers:
    - name: hold
      image: $image
      command: ["python3","-c","import signal,sys,time; signal.signal(signal.SIGTERM, lambda *_: sys.exit(0)); time.sleep(10**9)"]
      resources:
        requests:
          cpu: 10m
          memory: 32Mi
        limits:
          cpu: 100m
          memory: 64Mi
          nvidia.com/gpu: "$surplus"
EOF
  OCCUPIERS+=("$name")
  OCCUPIER_OF["$worker"]="$name"
  for _ in $(seq 1 60); do
    phase="$(kubectl get pod -n "$NS" "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [[ "$phase" == "Running" ]] && return 0
    sleep 2
  done
  echo "the surplus occupier on $worker did not start (phase ${phase:-unknown})." >&2
  echo "  Without it the qualification refuses this node, which is the correct outcome and not one to" >&2
  echo "  work around by dropping the requirement." >&2
  return 1
}

# openRoute finds the exporter on this worker and forwards to it on its own local port.
openRoute() {
  local worker="$1" pod observer container url port
  pod="$(kubectl get pods -n "$NS" -l app.kubernetes.io/component=dcgm-exporter \
    --field-selector "spec.nodeName=$worker,status.phase=Running" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -z "$pod" ]]; then
    echo "no running dcgm-exporter pod on $worker in namespace $NS." >&2
    echo "  the exporter is not part of any default overlay; deploy it with:" >&2
    echo "    kubectl apply -k config/dcgm-exporter" >&2
    return 1
  fi
  # The container name comes from the manifest rather than being hardcoded again. It was hardcoded once, to
  # the DAEMONSET's name, and matched nothing.
  container="$(kubectl get pod -n "$NS" "$pod" -o jsonpath='{.spec.containers[0].name}')"
  observer="$(kubectl get pod -n "$NS" "$pod" \
    -o jsonpath="{.status.containerStatuses[?(@.name==\"$container\")].imageID}")"
  if [[ -z "$observer" ]]; then
    echo "the exporter pod $pod reports no imageID, so nothing can name what produced its numbers." >&2
    return 1
  fi

  # A FREE port chosen by the OS, not a fixed one, and the forward's own liveness checked before its
  # answer is trusted.
  #
  # With a fixed port, anything already listening on it makes `kubectl port-forward` exit immediately --
  # and the checks below then talk to that other process. A local listener serving one plausible DCGM line
  # satisfies both of them, and the preflight and every run afterwards trust an unrelated program. That is
  # the "text server accepted as DCGM" failure this repository has already made twice inside the Go code,
  # reachable here one level above it.
  port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  kubectl port-forward -n "$NS" "pod/$pod" "$port:9400" >"/tmp/queuelab-pf-$worker.log" 2>&1 &
  local pf=$!
  FORWARDS+=("$pf")
  sleep 1
  if ! kill -0 "$pf" 2>/dev/null; then
    echo "the port-forward to $pod exited immediately; see /tmp/queuelab-pf-$worker.log" >&2
    return 1
  fi
  url="http://127.0.0.1:${port}/metrics"
  for _ in $(seq 1 30); do
    curl -sf -m 2 "$url" >/dev/null 2>&1 && break
    sleep 1
  done
  if ! curl -sf -m 2 "$url" >/dev/null 2>&1; then
    echo "the port-forward to $pod did not come up; see /tmp/queuelab-pf-$worker.log" >&2
    return 1
  fi
  # Written to a file first: `curl | grep -q` lets grep close the pipe on its first match, and under
  # `set -o pipefail` a large metrics body makes curl fail with a write error, so a healthy exporter reads as
  # an empty one.
  local body="/tmp/queuelab-metrics-$worker.txt"
  curl -sf -m 5 "$url" >"$body"
  if ! grep -q '^DCGM_FI_DEV_GPU_UTIL{' "$body"; then
    echo "the exporter at $url serves no DCGM_FI_DEV_GPU_UTIL samples." >&2
    echo "  that is a metrics-collection problem, not a device one: check the -f csv argument in" >&2
    echo "  config/dcgm-exporter/daemonset.yaml against the series internal/queuelab/dcgm.go reads." >&2
    return 1
  fi
  # On the SAME sample and non-empty. Two independent greps passed on a body whose utilisation rows carried
  # `pod=""` and whose pod label lived on an unrelated metric.
  if ! grep -qE '^DCGM_FI_DEV_GPU_UTIL\{[^}]*pod="[^"]+"' "$body"; then
    echo "the exporter at $url emits no pod labels, so nothing it reports can be attributed." >&2
    echo "  DCGM_EXPORTER_KUBERNETES must be true and the kubelet pod-resources socket must be mounted." >&2
    return 1
  fi
  POD_OF["$worker"]="$pod"
  URL_OF["$worker"]="$url"
  OBSERVER_OF["$worker"]="$observer"
}

prepare() {
  local worker="$1"
  echo "== $worker"
  refuseFakePlugin "$worker"
  occupySurplus "$worker"
  # The canary is what every run's qualification requires, and only this mode writes it. The script did not
  # take it, so the study path could not complete its first run on a fresh node.
  echo "  canary     : taking it (the qualification refuses a worker without one)"
  ./queuelabrun -termination-canary -worker "$worker" >"/tmp/queuelab-canary-$worker.log" 2>&1 \
    || { echo "the termination canary failed on $worker; see /tmp/queuelab-canary-$worker.log" >&2; return 1; }
  openRoute "$worker"
  echo "  exporter   : ${POD_OF[$worker]}"
  echo "  observer   : ${OBSERVER_OF[$worker]}"
  echo "  route      : ${URL_OF[$worker]}"
  echo "  preflight  :"
  ./queuelabrun -device-preflight -worker "$worker" \
    -device-metrics "${URL_OF[$worker]}" -device-observer "${OBSERVER_OF[$worker]}" \
    | sed 's/^/    /'
}

# The script's first act is to call ./queuelabrun, and a fresh checkout has no such file: the binary is
# gitignored and no Makefile target builds it. The entry point could not run from the state it is committed
# in, which is a poor property for the thing that spends the money.
echo "building the runner"
go build -o queuelabrun ./cmd/queuelabrun

for W in "${WORKERS[@]}"; do
  prepare "$W"
  echo
done

if [[ "${RUN_STUDY:-}" != "1" ]]; then
  echo "every worker is verified. To run the study through these same routes:"
  echo "  RUN_STUDY=1 $0 ${WORKERS[*]}"
  echo
  echo "run the protocol AFTER this, not before: the nodes are warm now, and a run taken cold pulls inside"
  echo "its own observation window and can censor its own waste figure."
  exit 0
fi

# The order is what the comparisons need, and it is written down rather than trusted: arm alternates on every
# run, and dose and node alternate within each pair a comparison reads. A blocked sequence produces the
# CONFOUNDED warnings the harness prints, and the study then cannot use its own records.
#
# With one worker the node axis is not delivered at all -- `-compare -mode node` refuses a set in which
# nothing varies -- so the eight-run form is the honest whole of what a one-node session returns.
mkdir -p ex
# The block below is ONE repetition of every cell, in the order the comparisons need. Repeating it REPS
# times keeps every alternation intact, because each block ends on the same arm the next one does not start
# with -- so arm still alternates across the join, and so do dose and node within each comparison's own
# subset. Growing n by repeating a correct block is what keeps the ordering property from having to be
# re-argued every time the allocation changes.
W1="${WORKERS[0]}"
SEQUENCE=()
for ((r = 1; r <= REPS; r++)); do
  if [[ ${#WORKERS[@]} -ge 2 ]]; then
    W2="${WORKERS[1]}"
    SEQUENCE+=(
      "self-completing A-honor  sh$r $W1" "grace-bounded   A-ignore wi$r $W2"
      "grace-bounded   A-honor  wh$r $W2" "grace-bounded   A-ignore gi$r $W1"
      "grace-bounded   A-honor  gh$r $W1" "self-completing A-ignore si$r $W1"
    )
  else
    SEQUENCE+=(
      "self-completing A-honor  sh$r $W1" "grace-bounded   A-ignore gi$r $W1"
      "grace-bounded   A-honor  gh$r $W1" "self-completing A-ignore si$r $W1"
    )
  fi
done

# A START_AT past the end skipped every run and then printed "all runs completed" -- a false success in the
# one wrapper that spends money, whose printed compare commands would then have read a PREVIOUS attempt's
# records. Range-checked here rather than tolerated.
if ! [[ "$START_AT" =~ ^[0-9]+$ ]] || (( START_AT < 1 || START_AT > ${#SEQUENCE[@]} )); then
  echo "START_AT must be between 1 and ${#SEQUENCE[@]} for this ${#WORKERS[@]}-worker session; got ${START_AT@Q}" >&2
  exit 2
fi

mkdir -p "$EXDIR" || { echo "cannot create $EXDIR" >&2; exit 1; }
echo "records for this session go in $EXDIR"
echo "running ${#SEQUENCE[@]} runs, starting at $START_AT"
echo
N=0
for SPEC in "${SEQUENCE[@]}"; do
  # shellcheck disable=SC2086
  set -- $SPEC
  DOSE=$1 ARM=$2 ID=$3 ON=$4
  N=$((N + 1))
  (( N < START_AT )) && { echo "[$N/${#SEQUENCE[@]}] $ID  skipped (START_AT=$START_AT)"; continue; }
  echo "[$N/${#SEQUENCE[@]}] $ID  $DOSE  $ARM  on $ON"
  # The route was verified once, at prepare time, and then trusted for the whole session. On EKS the forward
  # traverses an idle-timing load balancer and a worker's tunnel can sit unused for half an hour between its
  # runs. A dead tunnel does not stop the run: the device observer is explicitly non-fatal, so the full
  # protocol executes, spends its window, and is invalidated at the end by -require-device. Checking here
  # costs one HTTP request and turns that into a reconnect.
  if ! curl -sf -m 3 "${URL_OF[$ON]}" >/dev/null 2>&1; then
    echo "        the route to $ON is not answering; reopening it"
    openRoute "$ON" \
      || { echo "could not reopen the route to $ON; resume with START_AT=$N once it is back" >&2; exit 1; }
  fi
  # And the surplus must still be held. An evicted occupier frees the spare cards mid-session, and the run
  # that follows would measure a node whose scarcity has quietly gone -- the one wrong-number path here.
  if [[ -n "${OCCUPIER_OF[$ON]:-}" ]]; then
    OCC_PHASE="$(kubectl get pod -n "$NS" "${OCCUPIER_OF[$ON]}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "$OCC_PHASE" != "Running" ]]; then
      echo "the surplus occupier on $ON is ${OCC_PHASE:-gone}; the spare cards are free and the arm" >&2
      echo "  contrast would collapse without saying so. Re-prepare, then resume: START_AT=$N" >&2
      exit 1
    fi
  fi
  # No `|| true`. A run that fails has lost the thing the session is buying, and the ones after it would
  # spend node time on a route or a node that has already stopped working. START_AT is how the rest is
  # recovered once the cause is fixed, without re-running what already succeeded.
  ./queuelabrun -require-device -dose "$DOSE" -arm "$ARM" -runid "$ID" -worker "$ON" \
    -device-metrics "${URL_OF[$ON]}" -device-observer "${OBSERVER_OF[$ON]}" \
    -out "$EXDIR/gpu-$DOSE-$ARM-$ID.json" \
    || { echo; echo "run $ID failed. Fix the cause, then resume: START_AT=$N RUN_STUDY=1 $0 ${WORKERS[*]}" >&2; exit 1; }
done

# The globs are the ones the kind study uses, and they are narrow for reasons the tool enforces: an arm
# comparison refuses mixed doses, and the model check refuses records from more than one node. A glob that
# ignored either prints an ERROR rather than a document, and an earlier version of this script printed
# exactly those.
echo
echo "all runs completed. Compare them:"
echo "  ./queuelabrun -compare '$EXDIR/gpu-self-completing-*.json'"
echo "  ./queuelabrun -compare '$EXDIR/gpu-grace-bounded-*-g??.json'"
echo "  ./queuelabrun -compare '$EXDIR/gpu-self-completing-*.json,$EXDIR/gpu-grace-bounded-*-g??.json' -mode model"
echo "  ./queuelabrun -compare '$EXDIR/gpu-*-A-honor-*.json' -mode baseline"
if [[ ${#WORKERS[@]} -ge 2 ]]; then
  echo "  ./queuelabrun -compare '$EXDIR/gpu-grace-bounded-A-honor-*.json' -mode node"
fi
