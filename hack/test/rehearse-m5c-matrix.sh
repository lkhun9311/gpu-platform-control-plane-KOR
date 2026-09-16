#!/usr/bin/env bash
#
# Runs hack/m5c-matrix.sh -- the real script, unmodified -- on a local kind cluster.
#
# WHY THIS EXISTS
#
# hack/m5c-matrix.sh had never executed anywhere but on a rented A10G. Three paid runs found nine defects in
# it and two of those, the missing R1 arm and the probe tenants nobody had a key for, were plain in the
# script and were bought with a card anyway. Static tests now catch those two. What they cannot reach is the
# part that only runs: the arm loop, gen-trace's R1 filtering, the replay's credentials, the report, and the
# pre-registered readings evaluating the evidence the run actually wrote.
#
# So this drives the real script. Not a reimplementation of its steps -- hack/test/rehearse-m5c-deploy.sh is
# that, and it proved the routing chain works, which is a different and smaller claim.
#
# WHAT IS SUBSTITUTED, AND WHY THAT IS HONEST
#
# Two things need hardware and are replaced in a THROWAWAY COPY of the tree, never in the tracked one:
#
#   * the engines. Real vLLM needs a card, so the copy's manifests deploy `benchharness stub-serve` under
#     the same names, the same Service names and the same nvidia.com/gpu request. What the matrix does with
#     them -- roll them out, wait, route a tenant to each -- is unchanged.
#   * the device plugins. The real ones enumerate devices through NVML, so the copy's overlays deploy
#     config/device-plugin's simulator with FAKE_GPU_COUNT set to what each arm's topology advertises, and
#     the stub engines carry the CUDA_MPS_PIPE_DIRECTORY a real plugin would set on an MPS client. Nothing
#     here is MPS; what is exercised is that the matrix ASKS, not that the answer means anything.
#
# Nothing about the matrix is altered. It is read from the copy only so the substitutions have somewhere to
# live, and the copy is made from the working tree so that what runs here is what is about to be committed.
#
# WHAT THIS STILL CANNOT SAY
#
# Every number. A stub answers in milliseconds and a real engine answers in seconds, so nothing printed here
# is a measurement and nothing here can tell whether two engines fit on one card. It answers "does the
# instrument run end to end and produce the evidence its readings expect", which is the question three paid
# runs were spending money on.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT=$(pwd)

CLUSTER="${CLUSTER:-m5c-matrix-rehearse}"
KCTX="kind-$CLUSTER"
OPERATOR_NS="gpu-platform-control-plane-system"
GW_IMAGE="${GW_IMAGE:-gateway:m5c}"
STUB_IMAGE="${STUB_IMAGE:-benchstub:m5c-rehearse}"
SIM_IMAGE="gpu-simulator:latest"
KEEP="${KEEP:-0}"
WORK="$(mktemp -d)"
SRC="$WORK/src"

# Short compared with the real run's 420 s, but LONG ENOUGH FOR THE READINGS TO BE EVALUABLE.
#
# The first version used six seconds at six a second, which produced about two dozen premium completions --
# below the hundred a nearest-rank p99 needs to be anything but the slowest request, so every reading came
# back not-evaluable and the rehearsal called that a pass. A rehearsal that accepts evidence its own readings
# cannot read is testing that the pipe is connected, not that anything flows through it.
#
# At 12/s for 40 s with the contender at a third of arrivals, premium gets about 360 requests. Stubs answer
# in milliseconds, so this costs seconds.
RATE="${RATE:-12}"
DURATION_MS="${DURATION_MS:-40000}"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'REHEARSAL FAILED: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$KCTX" "$@"; }

for b in kind kubectl docker go; do
  command -v "$b" >/dev/null || fail "$b is not on PATH"
done
docker info >/dev/null 2>&1 || fail "the docker daemon is not reachable"

cleanup() {
  if [ "$KEEP" = "1" ]; then
    say "KEEP=1: cluster $CLUSTER and $WORK left in place"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# A control plane and a worker, matching the shape hack/m5c-gpu-session.sh builds on the instance.
#
# A single-node cluster is not a smaller version of that: the matrix derives its GPU node as the one that is
# NOT the control plane, so on a single-node cluster that selector matches nothing.
cat > "$WORK/kind.yaml" <<KINDEOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
KINDEOF
say "cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml" --wait 120s >/dev/null || fail "kind create cluster"

say "CRDs and the gateway's ClusterRole"
kubectl kustomize config/crd | k apply -f - >/dev/null || fail "apply the CRDs"
k create ns "$OPERATOR_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
kubectl kustomize config/gateway > "$WORK/gw-all.yaml" || fail "render config/gateway"
awk 'BEGIN{RS="\n---\n"} /(^|\n)kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding|ServiceAccount)(\n|$)/ {print "---"; print $0}' \
  "$WORK/gw-all.yaml" > "$WORK/gw-rbac.yaml"
grep -q "name: gateway-role" "$WORK/gw-rbac.yaml" || fail "no gateway-role in the rendered overlay"
k apply -f "$WORK/gw-rbac.yaml" >/dev/null || fail "apply the gateway RBAC"

say "build the gateway, the engine stub and the GPU simulator"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY gateway /gateway\nUSER 65532:65532\nENTRYPOINT ["/gateway"]\n' > "$WORK/Dockerfile"
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build the gateway image"
# busybox rather than distroless, because the matrix ASKS the engine whether it is an MPS client and a
# distroless image has no shell to answer with. The real vLLM image has one. Using distroless here made the
# matrix refuse the mps arm for the right reason on the wrong evidence -- "could not ask" rather than "not a
# client" -- which is a distinction the runner now draws and this rehearsal should not blur.
printf 'FROM busybox:1.36\nCOPY benchharness /benchharness\nENTRYPOINT ["/benchharness","stub-serve"]\n' > "$WORK/Dockerfile"
docker build -q -t "$STUB_IMAGE" "$WORK" >/dev/null || fail "build the stub image"
docker build -q -t "$SIM_IMAGE" -f Dockerfile.gpu-simulator . >/dev/null || fail "build the simulator image"
for img in "$GW_IMAGE" "$STUB_IMAGE" "$SIM_IMAGE"; do
  kind load docker-image "$img" --name "$CLUSTER" >/dev/null || fail "kind load $img"
done

# ---------------------------------------------------------------- the throwaway copy
#
# Taken from the WORKING TREE rather than from HEAD, because the point is to run what is about to be
# committed. The matrix resolves everything relative to its own directory, so hack/ and config/ are all it
# needs beside it.
say "copy the tree, then substitute the two things that need hardware"
mkdir -p "$SRC"
cp -r hack config "$SRC/" || fail "copy the tree"

# The engines. Same names, same Service names, same nvidia.com/gpu request, a stub behind them.
stub_engine_manifest() {
  local name="$1" out="$2"
  cat > "$out" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  labels: {app.kubernetes.io/component: vllm-shared}
spec:
  replicas: 1
  selector: {matchLabels: {engine: $name}}
  template:
    metadata:
      labels: {engine: $name, app.kubernetes.io/component: vllm-shared}
    spec:
      containers:
        - name: vllm
          image: $STUB_IMAGE
          imagePullPolicy: IfNotPresent
          args: ["--addr=:8000"]
          # The marker the matrix's MPS client check looks for, on a directory that exists.
          #
          # The real plugin sets this on a client container at allocation. The simulator does not, so without
          # it the mps arm would refuse here -- correctly, since a stub is not an MPS client. Supplying it
          # exercises that check's PASS path; its refusal path is mutation-tested rather than rehearsed,
          # because a rehearsal that could not complete four arms would stop covering everything after them.
          env:
            - {name: CUDA_MPS_PIPE_DIRECTORY, value: /tmp}
          ports: [{containerPort: 8000, name: http}]
          resources:
            limits:
              nvidia.com/gpu: 1
---
apiVersion: v1
kind: Service
metadata:
  name: $name
spec:
  selector: {engine: $name}
  ports: [{name: http, port: 8000, targetPort: http}]
EOF
}
stub_engine_manifest vllm-qwen25-3b "$SRC/config/vllm/deployment.yaml"
: > "$SRC/config/vllm/service.yaml"   # the Service is in the file above; this one must stay applyable
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: m5c-rehearse-noop\ndata: {}\n' > "$SRC/config/vllm/service.yaml"
stub_engine_manifest vllm-shared-a "$SRC/config/vllm-shared/engine-a.yaml"
stub_engine_manifest vllm-shared-b "$SRC/config/vllm-shared/engine-b.yaml"

# The device plugins. One simulator per overlay, named what the matrix waits for, advertising what the arm's
# topology advertises.
sim_overlay() {
  local dir="$1" ds="$2" count="$3"
  rm -rf "$SRC/config/$dir"; mkdir -p "$SRC/config/$dir"
  sed -e "s/^  name: gpu-simulator$/  name: $ds/" \
      -e "s/app.kubernetes.io\/component: gpu-simulator/app.kubernetes.io\/component: $ds/" \
      -e "s/^          value: \"1\"$/          value: \"$count\"/" \
      -e "s/^        - name: gpu-simulator$/        - name: gpu-simulator/" \
      config/device-plugin/daemonset.yaml > "$SRC/config/$dir/daemonset.yaml"
  # FAKE_GPU_COUNT sits at a different indentation than the sed above assumes in some versions, so it is
  # set explicitly and checked rather than hoped for.
  python3 - "$SRC/config/$dir/daemonset.yaml" "$count" <<'PY'
import re,sys
p,count=sys.argv[1],sys.argv[2]
s=open(p).read()
s=re.sub(r'(name: FAKE_GPU_COUNT\n(\s*)value: )"[0-9]+"', lambda m: m.group(1)+'"%s"'%count, s)
s=s.replace('image: gpu-simulator:latest','image: gpu-simulator:latest\n        imagePullPolicy: IfNotPresent')
open(p,'w').write(s)
PY
  grep -q "value: \"$count\"" "$SRC/config/$dir/daemonset.yaml" \
    || fail "the $dir stub does not advertise $count devices, so the matrix's exact-count wait would never pass"
  printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources:\n  - daemonset.yaml\n' \
    "$OPERATOR_NS" > "$SRC/config/$dir/kustomization.yaml"
}
sim_overlay nvidia-device-plugin-whole-card  nvidia-device-plugin-whole-card  1
sim_overlay nvidia-device-plugin-timeslicing nvidia-device-plugin-timeslicing 2
sim_overlay nvidia-device-plugin-mps         nvidia-device-plugin-mps         2

# The MPS arm additionally waits for a control daemon, which has no simulator. A no-op DaemonSet under that
# name is what lets the arm proceed; it stands for the daemon's PRESENCE and for nothing about MPS.
cat >> "$SRC/config/nvidia-device-plugin-mps/daemonset.yaml" <<EOF
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: nvidia-mps-control-daemon
spec:
  selector: {matchLabels: {app: mps-control-stub}}
  template:
    metadata: {labels: {app: mps-control-stub}}
    spec:
      tolerations: [{operator: Exists}]
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
EOF

say "run the REAL hack/m5c-matrix.sh against this cluster"
# Derived WITHOUT discarding stderr, which is how the first version of this line failed silently.
#
# On a single-node cluster the selector matched nothing, jsonpath on the empty list made kubectl exit
# non-zero, `2>/dev/null` removed the only clue, and `set -e` ended the script between two say lines with no
# message at all. This repository's own rules say not to send a script's stderr to /dev/null for exactly
# that reason, and the failure it predicts is the one that happened.
GPU_NODE=$(k get nodes -l '!node-role.kubernetes.io/control-plane' \
  -o go-template='{{if .items}}{{(index .items 0).metadata.name}}{{end}}') \
  || fail "could not list the cluster's non-control-plane nodes"
[ -n "$GPU_NODE" ] \
  || fail "this cluster has no worker node, so the matrix has nowhere to put an engine and its own node derivation would find nothing"
say "  card node $GPU_NODE"
OUT_DIR="$WORK/run"

# One variable decides what the matrix runs AND what this script expects, so the two cannot disagree.
ARMS_UNDER_TEST="${ARMS:-R1 shared timeSlicing mps}"

# LADDER rehearses the capacity ladder instead of the frozen matrix, on the same cluster.
#
# It is the same script under test either way. What differs is the plan it builds -- rungs of different
# loads in a counterbalanced order, and one isolated-baseline cell at whichever rung it ends on -- and a
# rehearsal that could only drive the matrix would leave every line of that plan unexecuted until a card was
# paying for it, which is what this file exists to stop.
#
# A stub answers in milliseconds, so every rung MEETS the registered target and the ladder climbs to the
# top. That is the CONTINUE path and the baseline purchase, end to end. The STOP path cannot be reached with
# a stub that is never slow, and it is covered where it is decided: TestLadderStoppingRule in
# internal/bench, and the ladder-verdict exit code checked in hack/test/check-m5c-static.sh.
LADDER_UNDER_TEST="${LADDER:-}"
# LADDER_FORCE_STOP drives the REAL runner through the branch a free cluster cannot reach.
#
# A stub answers in milliseconds, so every rung meets the target and the ladder always climbs. The stopping
# branch therefore never executes here -- and the first paid ladder died in it: the runner read $? after an
# `if`, which is the if statement's status and not the command's, so a verdict that correctly said STOP and
# exited 10 arrived as 0 and was reported as an unscorable rung. The exit code had been pinned; the script's
# READING of it had not.
#
# The shim forwards every subcommand to the real binary and answers only ladder-verdict, so what runs is the
# real matrix, the real cells and the real evidence -- with one verdict replaced.
LADDER_FORCE_STOP="${LADDER_FORCE_STOP:-}"
# LADDER=default uses rungs solved offline against the real gen-trace for THIS rehearsal's trace length.
#
# They are not the pre-registration's rungs and must not be mistaken for them: those offer 4,655 premium
# requests over 505 seconds, which is eight minutes of stub per cell for no information. What these
# reproduce is the SHAPE the ladder's own refusals check -- the contender pinned at exactly 139 offers at
# both rungs while the premium count doubles, and a premium tail above the registered 500-sample floor. A
# rehearsal whose cells the evaluator would refuse is a rehearsal of a refusal.
if [ "$LADDER_UNDER_TEST" = default ]; then
  LADDER_UNDER_TEST="14.560234:0.18426137 26.350037:0.09098256"
  DURATION_MS="${LADDER_DURATION_MS:-60000}"
fi

# A recorder in place of the instance's S3 uploader, so the per-cell hook is EXERCISED rather than assumed.
#
# On the instance this hook copies each cell's raw file to the bucket the moment the cell completes, which
# is what stops a Spot interruption taking every cell before it. Unset it and the matrix behaves exactly as
# it did; that is deliberate and it is also how a hook quietly stops being called. This proves it is.
cat > "$WORK/cell-hook" <<'HOOK'
#!/bin/bash
printf '%s %s %s\n' "$2" "$3" "$(basename "$1")" >> "$CELL_HOOK_LOG"
HOOK
chmod +x "$WORK/cell-hook"
export CELL_HOOK_LOG="$WORK/cells-seen.txt"
: > "$CELL_HOOK_LOG"

set +e
BH_FOR_MATRIX="$WORK/benchharness"
if [ -n "$LADDER_FORCE_STOP" ]; then
  cat > "$WORK/bh-shim" <<SHIM
#!/bin/bash
if [ "\$1" = ladder-verdict ]; then
  echo "LADDER: STOP"
  echo "forced by the rehearsal, to drive the runner's stopping branch" >&2
  exit 10
fi
exec "$WORK/benchharness" "\$@"
SHIM
  chmod +x "$WORK/bh-shim"
  BH_FOR_MATRIX="$WORK/bh-shim"
fi

if [ -n "$LADDER_UNDER_TEST" ]; then
  # RATE, ARMS, REPS and NOISY_WEIGHT are deliberately NOT passed: the script refuses a run that was given
  # both a ladder and a single load, and passing them here would rehearse a refusal instead of a ladder.
  ( cd "$SRC" && PLATFORM=kind KCTX="$KCTX" GPU_NODE="$GPU_NODE" \
      DEADLINE_EPOCH=$(( $(date +%s) + 3600 )) \
      GATEWAY_BIN="$WORK/gateway" BENCHHARNESS_BIN="$BH_FOR_MATRIX" \
      ENGINE_PIN_WAIVED=1 \
      DURATION_MS="$DURATION_MS" PREMIUM_WEIGHT=1 PROBE_WEIGHT=0 \
      LADDER="$LADDER_UNDER_TEST" LADDER_STUDY="${LADDER_STUDY:-}" OUT="$OUT_DIR" \
      CELL_DONE_HOOK="$WORK/cell-hook" CELL_HOOK_LOG="$CELL_HOOK_LOG" \
      bash hack/m5c-matrix.sh ) 2>&1 | tee "$WORK/matrix.log"
else
  ( cd "$SRC" && PLATFORM=kind KCTX="$KCTX" GPU_NODE="$GPU_NODE" \
      DEADLINE_EPOCH=$(( $(date +%s) + 3600 )) \
      GATEWAY_BIN="$WORK/gateway" BENCHHARNESS_BIN="$WORK/benchharness" \
      RATE="$RATE" DURATION_MS="$DURATION_MS" ENGINE_PIN_WAIVED=1 \
      PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.5 PROBE_WEIGHT=0 \
      REPS="${REPS:-1}" ARMS="$ARMS_UNDER_TEST" OUT="$OUT_DIR" \
      CELL_DONE_HOOK="$WORK/cell-hook" CELL_HOOK_LOG="$CELL_HOOK_LOG" \
      bash hack/m5c-matrix.sh ) 2>&1 | tee "$WORK/matrix.log"
fi
rc=${PIPESTATUS[0]}
set -e
[ "$rc" = "0" ] || { tail -25 "$WORK/matrix.log"; fail "the matrix exited $rc -- the log above is what it said"; }

# ---------------------------------------------------------------- what the run must have produced
#
# The expectation is ARMS x REPS, and it is built BEFORE looking at the directory.
#
# The first version of this section listed four arm names and repetition 1, and the per-cell check derived
# what it expected from `ls raw-*.jsonl` -- so a run that silently produced no second repetition had no
# repetition-2 files AND no repetition-2 callbacks, both sides shrank together, and the comparison passed.
# A check whose expectation comes from the thing it is checking cannot fail. The arms come from ARMS for the
# same reason: the next paid run is three arms, and a rehearsal that can only rehearse four is not
# rehearsing it.
say "check what the matrix wrote"
want_cells=""
# tenants_in is defined HERE, above every caller.
#
# It used to sit two hundred lines below the forced-STOP branch that now calls it, which in bash is not a
# forward declaration -- it is "tenants_in: command not found" and a failed assertion that looks like a
# failed run. The same shape cost a paid session earlier today, in the wrapper, where a refusal was placed
# above the definition of fail().
# R1 IS THE POINT OF THIS CHECK. It is the isolated baseline, so its trace must carry the premium tenant and
# nothing else -- gen-trace filters the contender out of the same trace when the arm is R1. A run whose R1
# carried the contender would be measuring the contended case and dividing by it.
tenants_in() {
  python3 -c "
import json,sys,collections
c=collections.Counter()
for line in open(sys.argv[1]):
    c[json.loads(line).get('tenant','?')]+=1
print(' '.join(sorted(c)))
" "$1"
}

if [ -n "$LADDER_FORCE_STOP" ]; then
  # The forced verdict says STOP at the FIRST rung, so the run must be exactly three cells: the rung's two
  # topologies and the isolated baseline bought at that rung. Nothing above it may exist.
  #
  # This is the assertion the first paid ladder needed and did not have. It ran the two cells of rung 1, got
  # a correct STOP, misread the exit code and ended the session reporting the rung unscorable.
  for want in rung01-shared rung01-timeSlicing rung01-R1; do
    [ -s "$OUT_DIR/raw-$want-1.jsonl" ] \
      || fail "a forced STOP at rung 1 must leave $want measured; the runner did not act on the stopping verdict"
  done
  if compgen -G "$OUT_DIR/raw-rung02-"'*.jsonl' >/dev/null; then
    fail "the runner climbed past a rung it was told to stop at: $(cd "$OUT_DIR" && ls raw-rung02-*.jsonl | tr '\n' ' ')"
  fi
  grep -q "which is the registered stopping point" "$WORK/matrix.log" \
    || fail "the runner stopped without saying it was the registered rule; on a paid run that message is the only thing distinguishing a stop from a failure"
  grep -q "could not score rung" "$WORK/matrix.log" \
    && fail "the runner read the stopping verdict as an unscorable rung -- which is exactly the defect that ended the first paid ladder"
  # THE ASSERTIONS THAT ARE NOT ABOUT STOPPING RUN HERE TOO.
  #
  # This branch used to exit before them, so breaking the baseline's tenant filter or the per-cell handover
  # SPECIFICALLY on the stopping path left this mode green -- and the stopping path is the one a real ladder
  # takes at its last rung, which is where the baseline is bought. A review pointed that out; the exit is
  # now after them rather than before.
  r1_tenants=$(tenants_in "$OUT_DIR/raw-rung01-R1-1.jsonl")
  [ "$r1_tenants" = "premium-1" ] \
    || fail "the baseline bought at the stopping rung carries tenants [$r1_tenants] and must carry only premium-1"
  seen_cells=$(sort "$CELL_HOOK_LOG" 2>/dev/null || true)
  for want in rung01-shared rung01-timeSlicing rung01-R1; do
    printf '%s' "$seen_cells" | grep -q "^$want 1 raw-$want-1.jsonl$" \
      || fail "the per-cell hook never saw $want. On the instance that hook is what puts each cell in the bucket while the card is still running, and the stopping path is where the last cell of a real ladder is bought"
  done
  say "  the baseline carries only premium-1, and all three cells were handed over as they completed"
  say "REHEARSAL PASSED: a STOP verdict stopped the ladder at rung 1 and bought its baseline there, and nothing above it ran."
  exit 0
fi
if [ -n "$LADDER_UNDER_TEST" ]; then
  # Derived from the LADDER SPEC, not from the files, for the reason the comment above gives. The order is
  # derived too: odd rungs run the control first and even rungs the split, and a rehearsal that accepted any
  # order would let the counterbalance -- the one thing this design buys over the frozen matrix -- silently
  # stop happening.
  want_order=""
  reh_rung=0
  reh_first=0
  reh_last=0
  for entry in $LADDER_UNDER_TEST; do
    reh_rung=$(( reh_rung + 1 ))
    # A skipped rung holds its position and buys nothing, so it is neither expected nor counted as the
    # first or last purchase. This loop counted positions, so a skip-led ladder -- which is what a
    # repetition of rungs 2 and 3 is -- demanded rung-1 files that were deliberately not bought.
    [ "$entry" != skip ] || continue
    [ "$reh_first" != 0 ] || reh_first=$reh_rung
    reh_last=$reh_rung
    if [ $(( reh_rung % 2 )) -eq 1 ]; then reh_order="shared timeSlicing"; else reh_order="timeSlicing shared"; fi
    for reh_topology in $reh_order; do
      reh_label=$(printf 'rung%02d-%s' "$reh_rung" "$reh_topology")
      [ -s "$OUT_DIR/raw-$reh_label-1.jsonl" ] \
        || fail "no raw evidence for $reh_label. The ladder reported success, so this is a cell that ran and wrote nothing"
      want_cells="$want_cells$reh_label 1 raw-$reh_label-1.jsonl
"
      want_order="$want_order$reh_label
"
    done
  done
  # And the isolated baseline, once, at the rung the ladder ended on. With a stub every rung meets the
  # target, so the ladder climbs to the top and the baseline belongs to the last rung.
  [ "$reh_last" != 0 ] || fail "the ladder under test buys no rung at all; every entry is skip"
  LADDER_TOP_LABEL=$(printf 'rung%02d-R1' "$reh_last")
  [ -s "$OUT_DIR/raw-$LADDER_TOP_LABEL-1.jsonl" ] \
    || fail "the ladder climbed every rung and never bought its isolated baseline. Without it, 'the split ran out of capacity' and 'one engine ran out of capacity' are the same observation"
  want_cells="$want_cells$LADDER_TOP_LABEL 1 raw-$LADDER_TOP_LABEL-1.jsonl
"
  want_order="$want_order$LADDER_TOP_LABEL
"
  got_order=$(grep -oE 'cell [0-9]+/[0-9]+: rung[0-9]+-[A-Za-z0-9]+' "$WORK/matrix.log" | sed 's/.*: //')
  [ "$(printf '%s' "$want_order")" = "$got_order" ] \
    || fail "the ladder did not run its cells in the counterbalanced order.
  wanted: $(printf '%s' "$want_order" | tr '\n' ';')
  ran:    $(printf '%s' "$got_order" | tr '\n' ';')"
  say "  the rungs alternated which topology went first, which is the counterbalance"
else
  for arm in $ARMS_UNDER_TEST; do
    for rep in $(seq 1 "${REPS:-1}"); do
      [ -s "$OUT_DIR/raw-$arm-$rep.jsonl" ] \
        || fail "no raw evidence for $arm repetition $rep. The matrix reported success, so this is a cell that ran and wrote nothing"
      want_cells="$want_cells$arm $rep raw-$arm-$rep.jsonl
"
    done
  done
fi
want_cells=$(printf '%s' "$want_cells" | sort)
# And nothing EXTRA: a file for an arm or repetition nobody asked for is a run that is not the one requested.
got_files=$(for f in "$OUT_DIR"/raw-*.jsonl; do
  b=$(basename "$f" .jsonl); b=${b#raw-}
  printf '%s %s %s\n' "${b%-*}" "${b##*-}" "$(basename "$f")"
done | sort)
[ "$want_cells" = "$got_files" ] \
  || fail "the evidence on disk is not ARMS x REPS.
  wanted: $(printf '%s' "$want_cells" | tr '\n' ';')
  found:  $(printf '%s' "$got_files" | tr '\n' ';')"

# EVERY repetition, not the first. A repetition replayed from a different trace is a defect the runner
# guards against elsewhere, and a rehearsal that only ever looked at repetition 1 was taking the guard's
# word for it rather than checking the evidence.
if [ -n "$LADDER_UNDER_TEST" ]; then
  # The ladder's baseline is called rung04-R1, and gen-trace's contender filter used to ask `arm == "R1"` --
  # a literal that would have looked straight past it and produced a "baseline" carrying the contender.
  # bench.IsIsolatedBaseline is what fixed that, and this is the cell that proves it on real evidence.
  r1_tenants=$(tenants_in "$OUT_DIR/raw-$LADDER_TOP_LABEL-1.jsonl")
  [ "$r1_tenants" = "premium-1" ] \
    || fail "the ladder's baseline $LADDER_TOP_LABEL carries tenants [$r1_tenants] and must carry only premium-1. A baseline that includes the contender is the contended case wearing the baseline's name"
  say "  the ladder's baseline carries only premium-1, under a name that is not the literal R1"
else
  for rep in $(seq 1 "${REPS:-1}"); do
    r1_tenants=$(tenants_in "$OUT_DIR/raw-R1-$rep.jsonl")
    [ "$r1_tenants" = "premium-1" ] \
      || fail "the R1 arm's repetition $rep carries tenants [$r1_tenants] and must carry only premium-1. R1 is the isolated baseline both bars divide by, and one that includes the contender is the contended case wearing the baseline's name"
  done
  say "  R1 carries only premium-1, in every repetition"
fi

# EVERY cell must have been handed over, and the check compares WHICH ONES rather than how many.
#
# The first version of this compared totals only, so four callbacks for R1 and none for the other three
# would have passed -- a check for "once per cell" that could not tell one cell from another. The hook is
# handed (file, arm, repetition), so the multiset of those triples is what has to match the files on disk.
seen_cells=$(sort "$CELL_HOOK_LOG" 2>/dev/null || true)
if [ "$want_cells" != "$seen_cells" ]; then
  fail "the per-cell hook did not see exactly the cells that were written. On the instance that hook is what
  puts each cell in the bucket while the card is still running, so a cell it does not see is a cell an
  interruption takes with it.
  expected: $(printf '%s' "$want_cells" | tr '\n' ';')
  seen:     $(printf '%s' "$seen_cells" | tr '\n' ';')"
fi
say "  every cell was handed over as it completed, by (arm, repetition): $(printf '%s' "$seen_cells" | tr '\n' ';')"

# The contended cell these two checks read: the matrix's `shared` arm, or the ladder's first rung.
if [ -n "$LADDER_UNDER_TEST" ]; then
  # The FIRST rung this ladder bought, which is not rung 1 when the ladder is skip-led.
  CONTENDED_CELL="raw-$(printf 'rung%02d' "$reh_first")-shared-1.jsonl"
else
  CONTENDED_CELL="raw-shared-1.jsonl"
fi
shared_tenants=$(python3 -c "
import json,sys,collections
c=collections.Counter()
for line in open(sys.argv[1]):
    c[json.loads(line).get('tenant','?')]+=1
print(' '.join(sorted(c)))
" "$OUT_DIR/$CONTENDED_CELL")
[ "$shared_tenants" = "premium-1 standard-noisy" ] \
  || fail "the shared arm's evidence carries [$shared_tenants]; it must carry both tenants and no others, or the trace is not the one this study registered"
say "  shared carries both tenants and no unauthenticated third"

# Nothing may have been refused on credentials. This is the defect spot-run.sh has recorded three times.
refused=$(python3 -c "
import json,sys
n=0
for line in open(sys.argv[1]):
    d=json.loads(line)
    if d.get('status') in (401,403) or d.get('httpStatus') in (401,403): n+=1
print(n)
" "$OUT_DIR/$CONTENDED_CELL")
[ "$refused" = "0" ] \
  || fail "$refused requests were refused on credentials. Every tenant the trace sends must have a key, and this is the fourth time that has not been true"
say "  no request was refused on credentials"

# ---------------------------------------------------------------- and the readings must run
say "evaluate the pre-registered readings over the evidence the matrix wrote"
args=()
for f in "$OUT_DIR"/raw-*.jsonl; do args+=(--raw "$f"); done

if [ -n "$LADDER_UNDER_TEST" ]; then
  # The ladder's readings are its own, and what a stub produces is knowable in advance: it answers in
  # milliseconds, so every rung meets the target, no bracket is closed, and the registered outcome is L6 --
  # a lower bound on both topologies and explicitly not an extrapolation.
  #
  # Asserting the ANSWER here is safe in a way it is not for the sharing matrix, because this is not a
  # measurement. It is the one reading a cluster with no card can legitimately produce, and if the ladder
  # produced any other one over this evidence the instrument is not doing what the pre-registration says.
  set +e
  go run ./cmd/benchharness report "${args[@]}" > "$WORK/report.txt" 2>"$WORK/report.err"
  report_rc=$?
  set -e
  [ "$report_rc" = "0" ] || { tail -20 "$WORK/report.txt"; cat "$WORK/report.err"; fail "the ladder report exited $report_rc over its own evidence"; }
  grep -q "CAPACITY LADDER" "$WORK/report.txt" \
    || { tail -20 "$WORK/report.txt"; fail "the report did not evaluate the ladder's readings over the ladder's own evidence"; }
  grep -qE '^\s*ANSWER: L6$' "$WORK/report.txt" \
    || { sed -n '/CAPACITY LADDER/,$p' "$WORK/report.txt"; fail "a stub meets the target at every rung, so the registered answer is L6 -- a lower bound. Anything else means the criterion or the stopping rule is not the one the pre-registration describes"; }
  # ANCHORED ON THE CELL TABLE, because the word INVALID also appears in reading L0's NAME -- which is
  # printed whether or not it fired. The first version of this line grepped the whole report and failed a
  # ladder whose five cells were all scored and whose answer was exactly the registered L6.
  #
  # (An earlier version of this comment also claimed `grep -q X && { ... }` would trip `set -e` here. It
  # does not: bash exempts every command of an && list but the last, and `set -e; false && true; echo ok`
  # prints ok. Checked rather than remembered, because a comment that states an invariant has to be true.)
  if grep -qE '^ +rung[0-9]+-[A-Za-z0-9]+ .*INVALID' "$WORK/report.txt"; then
    sed -n '/CAPACITY LADDER/,$p' "$WORK/report.txt"
    fail "a ladder cell was refused as unscorable; the rehearsal's own cells must be scorable or it is rehearsing a refusal"
  fi
  sed -n '/CAPACITY LADDER/,$p' "$WORK/report.txt" | sed 's/^/  /'
  say "REHEARSAL PASSED: the real script ran the ladder end to end, in the counterbalanced order, bought its baseline at the rung it ended on, and its readings answered L6."
  say "What this did NOT cover: every number, the STOP path (no stub is ever slow), and whether two engines fit on one card."
  exit 0
fi
# A NON-ZERO EXIT IS EXPECTED HERE, and distinguishing it from a crash is the point.
#
# An INVALID reading now exits non-zero, so that automation writing `report ... || fail` cannot accept a run
# the readings have declared unusable. Stub engines answer in milliseconds and therefore produce no
# contention at all, which is reading 4's INVALID -- the correct answer for this evidence. What this
# rehearsal must tell apart is "the report said the run is invalid", which is a working instrument, from
# "the report could not read the evidence", which is not.
set +e
go run ./cmd/benchharness report "${args[@]}" > "$WORK/report.txt" 2>"$WORK/report.err"
report_rc=$?
set -e
if ! grep -q "PRE-REGISTERED READINGS" "$WORK/report.txt"; then
  tail -20 "$WORK/report.txt"; cat "$WORK/report.err"
  fail "the report exited $report_rc without evaluating any readings over this study's own evidence"
fi
# ANCHORED on the reading id, because "[FIRED] 4" also matches 4b and 4c and those are different verdicts.
if [ "$report_rc" != "0" ] && ! grep -qE '^\s*\[FIRED\] (4|4b|4c) ' "$WORK/report.txt"; then
  cat "$WORK/report.err"
  fail "the report exited $report_rc and no INVALID reading fired, so the non-zero status is a failure rather than a verdict"
fi
sed -n '/PRE-REGISTERED READINGS/,$p' "$WORK/report.txt" | sed 's/^/  /'

# THE READINGS MUST HAVE BEEN ABLE TO LOOK, which is not the same as having run.
#
# Every reading here can come back not-evaluable, deliberately, because a reading that reports "did not
# fire" when it could not be computed is indistinguishable from one that looked and found nothing. That
# design makes this check necessary: the first version of this rehearsal asserted only that the readings
# printed, and passed on evidence so thin that reading 4 declined and every reading after it was skipped.
#
# Which readings FIRE is not asserted and must not be. That is the study's answer and it belongs to the
# card, not to a stub that replies in a millisecond.
if sed -n '/PRE-REGISTERED READINGS/,$p' "$WORK/report.txt" | grep -qE '^\s*\[ N/E \] 4 '; then
  fail "reading 4 could not be evaluated over the matrix's own evidence, so nothing below it was evaluated either. The instrument ran and could not read what it wrote, which is the state this rehearsal exists to distinguish from a pass"
fi

# WHAT THIS CAN AND CANNOT SAY, stated because the line it replaces said more than it had.
#
# It used to print "the readings below it were reached". They are not: a fired reading 4 returns from the
# evaluator immediately, so an INVALID verdict -- which is the correct verdict for stub evidence -- means
# readings 1, 2, 3 and 5 were never evaluated at all. Claiming a verification that did not happen is worse
# than not claiming one, because it is the line a reader trusts instead of scrolling.
#
# So the claim is the one this rehearsal can support: reading 4 had evidence it could read. Whether the
# readings below it work is the business of the unit tests, which construct the summaries they need.
if sed -n '/PRE-REGISTERED READINGS/,$p' "$WORK/report.txt" | grep -qE '^\s*\[FIRED\] 4'; then
  say "  reading 4 (or 4b/4c) fired INVALID, which short-circuits the evaluator -- the readings below it were NOT reached"
  say "  that is the correct verdict for stub evidence, and it is all this rehearsal can say about them"
else
  say "  reading 4 was evaluable and did not fire, so the readings below it were reached"
fi

say "REHEARSAL PASSED: the real matrix ran $ARMS_UNDER_TEST at ${REPS:-1} repetition(s) end to end and its readings were evaluated."
say "What this did NOT cover: every number, and whether two engines fit on one card."
