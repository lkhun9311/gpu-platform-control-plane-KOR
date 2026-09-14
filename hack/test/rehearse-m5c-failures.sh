#!/usr/bin/env bash
#
# Makes hack/m5c-matrix.sh FAIL, five ways, and reads what it says.
#
# WHY THIS EXISTS
#
# hack/test/rehearse-m5c-matrix.sh drives the matrix down its happy path. Everything it exercises works.
# What it never touches is the code that runs when something does not, and that is most of what the last
# three paid runs actually met:
#
#   engine_diagnosis     runs when an engine never becomes ready. The SECOND paid run died exactly there,
#                        printed "engine b never became ready" and nothing else, and the whole of defect 12
#                        was adding the part that says why. That addition has never executed.
#   arm_refused          records a registered refusal so `benchharness report` can read it as reading 4c.
#                        Until it runs, 4c's only evidence path is untested.
#   cell_deadline_check  stops the matrix on a cell boundary rather than being cut mid-cell.
#
# This repository's own rule is that a refusal nobody has executed is a refusal nobody knows the wording of,
# and every one of these is a refusal an operator reads at the end of a paid session. The point here is not
# that the matrix fails -- it is what it hands the reader when it does.
#
# WHAT IT ASSERTS
#
# Not exit codes. Those are already pinned by the characterization goldens. What it asserts is that the
# OUTPUT names the fault: which Pod, what the node had left, which arm was refused and why, how many cells
# were done. A refusal that does not say why is a refusal that has to be bought twice, and the second time
# costs a card.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1

CLUSTER="${CLUSTER:-m5c-fail-rehearse}"
KCTX="kind-$CLUSTER"
OPERATOR_NS="gpu-platform-control-plane-system"
GW_IMAGE="${GW_IMAGE:-gateway:m5c}"
STUB_IMAGE="${STUB_IMAGE:-benchstub:m5c-rehearse}"
SIM_IMAGE="gpu-simulator:latest"
KEEP="${KEEP:-0}"
WORK="$(mktemp -d)"
SRC="$WORK/src"
failures=0
# ONLY lets one scenario be run, which is how a scenario that behaves differently in the suite than on its
# own gets looked at without paying for the other two.
ONLY="${ONLY:-}"
want() { [ -z "$ONLY" ] || [ "$ONLY" = "$1" ]; }

say()  { printf '== %s\n' "$*"; }
# A failed assertion prints the log it was reading, because a check that says only "not pinned" is the
# thing this file exists to argue against. The log is named so the tail can be found without it.
bad()  {
  printf '   NOT PINNED: %s\n' "$*"
  [ -n "${CURRENT_LOG:-}" ] && [ -f "$CURRENT_LOG" ] \
    && { printf '     --- last 12 lines of %s ---\n' "$(basename "$CURRENT_LOG")"; tail -12 "$CURRENT_LOG" | sed 's/^/     /'; }
  failures=$((failures + 1))
}
ok()   { printf '   ok  %s\n' "$*"; }
fail() { printf 'REHEARSAL FAILED: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$KCTX" "$@"; }

for b in kind kubectl docker go; do command -v "$b" >/dev/null || fail "$b is not on PATH"; done
docker info >/dev/null 2>&1 || fail "the docker daemon is not reachable"

cleanup() {
  if [ "$KEEP" = "1" ]; then say "KEEP=1: cluster $CLUSTER and $WORK left in place"
  else kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; rm -rf "$WORK"; fi
}
trap cleanup EXIT

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

kubectl kustomize config/crd | k apply -f - >/dev/null || fail "apply the CRDs"
k create ns "$OPERATOR_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
kubectl kustomize config/gateway > "$WORK/gw-all.yaml" || fail "render config/gateway"
awk 'BEGIN{RS="\n---\n"} /(^|\n)kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding|ServiceAccount)(\n|$)/ {print "---"; print $0}' \
  "$WORK/gw-all.yaml" > "$WORK/gw-rbac.yaml"
k apply -f "$WORK/gw-rbac.yaml" >/dev/null || fail "apply the gateway RBAC"

say "build and load the images"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY gateway /gateway\nUSER 65532:65532\nENTRYPOINT ["/gateway"]\n' > "$WORK/Dockerfile"
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build the gateway image"
printf 'FROM busybox:1.36\nCOPY benchharness /benchharness\nENTRYPOINT ["/benchharness","stub-serve"]\n' > "$WORK/Dockerfile"
docker build -q -t "$STUB_IMAGE" "$WORK" >/dev/null || fail "build the stub image"
docker build -q -t "$SIM_IMAGE" -f Dockerfile.gpu-simulator . >/dev/null || fail "build the simulator"
for img in "$GW_IMAGE" "$STUB_IMAGE" "$SIM_IMAGE"; do
  kind load docker-image "$img" --name "$CLUSTER" >/dev/null || fail "kind load $img"
done

GPU_NODE=$(k get nodes -l '!node-role.kubernetes.io/control-plane' \
  -o go-template='{{if .items}}{{(index .items 0).metadata.name}}{{end}}') || fail "list nodes"
[ -n "$GPU_NODE" ] || fail "no worker node"

# ---------------------------------------------------------------- the throwaway copy
#
# Same substitution as the happy-path rehearsal: stub engines and simulated devices, in a copy, never in the
# tracked tree. Each scenario below then breaks ONE more thing on top.
prepare_copy() {
  rm -rf "$SRC"; mkdir -p "$SRC"
  cp -r hack config "$SRC/"
  stub_engine "$SRC/config/vllm/deployment.yaml" vllm-qwen25-3b "$STUB_IMAGE"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: m5c-noop\ndata: {}\n' > "$SRC/config/vllm/service.yaml"
  stub_engine "$SRC/config/vllm-shared/engine-a.yaml" vllm-shared-a "$STUB_IMAGE"
  stub_engine "$SRC/config/vllm-shared/engine-b.yaml" vllm-shared-b "$STUB_IMAGE"
  sim_overlay nvidia-device-plugin-whole-card  nvidia-device-plugin-whole-card  1
  sim_overlay nvidia-device-plugin-timeslicing nvidia-device-plugin-timeslicing 2
  sim_overlay nvidia-device-plugin-mps         nvidia-device-plugin-mps         2
  cat >> "$SRC/config/nvidia-device-plugin-mps/daemonset.yaml" <<EOF
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: nvidia-mps-control-daemon}
spec:
  selector: {matchLabels: {app: mps-control-stub}}
  template:
    metadata: {labels: {app: mps-control-stub}}
    spec:
      tolerations: [{operator: Exists}]
      containers: [{name: pause, image: "registry.k8s.io/pause:3.9"}]
EOF
}

stub_engine() {
  local out="$1" name="$2" image="$3"
  cat > "$out" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  labels: {app.kubernetes.io/component: vllm-shared}
spec:
  replicas: 1
  # A short progress deadline, because a scenario that waits the real fifteen minutes to prove a message is
  # a scenario nobody runs. The matrix's own rollout timeout is longer; this is what makes the Deployment
  # give up first, which is exactly what happened on the card.
  progressDeadlineSeconds: 40
  selector: {matchLabels: {engine: $name}}
  template:
    metadata:
      labels: {engine: $name, app.kubernetes.io/component: vllm-shared}
    spec:
      containers:
        - name: vllm
          image: $image
          imagePullPolicy: IfNotPresent
          args: ["--addr=:8000"]
          env:
            - {name: CUDA_MPS_PIPE_DIRECTORY, value: /tmp}
          ports: [{containerPort: 8000, name: http}]
          resources:
            limits:
              nvidia.com/gpu: 1
---
apiVersion: v1
kind: Service
metadata: {name: $name}
spec:
  selector: {engine: $name}
  ports: [{name: http, port: 8000, targetPort: http}]
EOF
}

sim_overlay() {
  local dir="$1" ds="$2" count="$3"
  rm -rf "$SRC/config/$dir"; mkdir -p "$SRC/config/$dir"
  sed -e "s/^  name: gpu-simulator$/  name: $ds/" \
      -e "s|app.kubernetes.io/component: gpu-simulator|app.kubernetes.io/component: $ds|" \
      config/device-plugin/daemonset.yaml > "$SRC/config/$dir/daemonset.yaml"
  python3 - "$SRC/config/$dir/daemonset.yaml" "$count" <<'PY'
import re,sys
p,count=sys.argv[1],sys.argv[2]
s=open(p).read()
s=re.sub(r'(name: FAKE_GPU_COUNT\n(\s*)value: )"[0-9]+"', lambda m: m.group(1)+'"%s"'%count, s)
s=s.replace('image: gpu-simulator:latest','image: gpu-simulator:latest\n        imagePullPolicy: IfNotPresent')
open(p,'w').write(s)
PY
  printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources:\n  - daemonset.yaml\n' \
    "$OPERATOR_NS" > "$SRC/config/$dir/kustomization.yaml"
}

run_matrix() {
  local arms="$1" deadline_s="$2" log="$3"
  CURRENT_LOG="$log"
  rm -rf "$WORK/run"
  set +e
  ( cd "$SRC" && PLATFORM=kind KCTX="$KCTX" GPU_NODE="$GPU_NODE" \
      DEADLINE_EPOCH=$(( $(date +%s) + deadline_s )) \
      GATEWAY_BIN="$WORK/gateway" BENCHHARNESS_BIN="$WORK/benchharness" \
      ENGINE_PIN_WAIVED=1 \
      RATE=12 DURATION_MS=8000 PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.5 PROBE_WEIGHT=0 \
      REPS=1 ARMS="$arms" OUT="$WORK/run" \
      bash hack/m5c-matrix.sh ) > "$log" 2>&1
  local rc=$?
  set -e
  # $WORK/run is NOT removed here: a scenario that refuses an arm writes its reason there and the assertions
  # after the call have to read it. Each scenario clears it before its own run instead.
  return $rc
}

# ---------------------------------------------------------------- 1. an engine that never becomes ready
if want 1; then
say "1. an engine that never becomes ready -- does the diagnosis say WHY?"
prepare_copy
# An image that does not exist. The Pod goes ImagePullBackOff, which is one of the four faults the
# diagnosis was written to tell apart, and the one a paid run would most plausibly hit after a registry
# problem. The card's own failure was a different one; what is being pinned is that the runner reports the
# Pod's state at all.
stub_engine "$SRC/config/vllm-shared/engine-b.yaml" vllm-shared-b "there-is-no-such-image:never"
CURRENT_LOG="$WORK/fail-engine.log"
# Exiting ZERO is correct here, and asserting otherwise was this script's own staleness.
#
# An arm whose engine cannot start is a registered outcome -- reading 4c, INVALID for that arm -- so the
# matrix records the refusal and carries on with the arms beside it. With `timeSlicing` as the only arm
# there is nothing beside it, and a run that refused everything it was asked for still finished doing what
# it was told. What must be true is that the refusal is announced and recorded, which is what the
# assertions below actually check; the exit status is the goldens' business.
run_matrix "timeSlicing" 3600 "$WORK/fail-engine.log" || true
# The arm is REFUSED and recorded, not fatal. The 2026-09-12 pilot ended its session here instead, so the
# three cells it had already bought were all it had to show and the reason lived in a log the report does
# not read.
grep -q "REFUSED timeSlicing" "$WORK/fail-engine.log" \
  && ok "the arm was refused rather than ending the session" || bad "an unstartable engine ended the run instead of refusing its own arm"
[ -s "$WORK/run/refused-timeSlicing.txt" ] \
  && ok "the reason was written where the report reads it" || bad "no refused-timeSlicing.txt, so reading 4c has nothing to fire on"
grep -q "why m5c-b/vllm-shared-b never became ready" "$WORK/fail-engine.log" \
  && ok "the diagnosis ran and named the engine" || bad "no diagnosis block for the failed engine"
grep -qE "ImagePullBackOff|ErrImagePull|Failed to pull" "$WORK/fail-engine.log" \
  && ok "it names the actual fault (image pull)" || bad "the diagnosis does not name why the Pod is not running"
grep -q "why m5c-a/vllm-shared-a never became ready" "$WORK/fail-engine.log" \
  && ok "the healthy engine beside it is described too" || bad "only the failing engine was described, and on one card what the other reserved is half the explanation"
grep -q "what the node had left to give" "$WORK/fail-engine.log" \
  && ok "the node's allocatable is reported" || bad "the node's remaining capacity is absent"

fi

# ---------------------------------------------------------------- 2. an mps arm whose engines are not clients
if want 2; then
say "2. an mps arm whose engines are not MPS clients -- is it REFUSED and RECORDED?"
prepare_copy
# The marker the plugin would set is removed, which is what a client that never connected looks like.
python3 - "$SRC/config/vllm-shared/engine-a.yaml" "$SRC/config/vllm-shared/engine-b.yaml" <<'PY'
import sys
for p in sys.argv[1:]:
    s=open(p).read()
    s=s.replace('            - {name: CUDA_MPS_PIPE_DIRECTORY, value: /tmp}\n','')
    open(p,'w').write(s)
PY
rm -rf "$WORK/run"
set +e
( cd "$SRC" && PLATFORM=kind KCTX="$KCTX" GPU_NODE="$GPU_NODE" \
    DEADLINE_EPOCH=$(( $(date +%s) + 3600 )) \
    GATEWAY_BIN="$WORK/gateway" BENCHHARNESS_BIN="$WORK/benchharness" \
    ENGINE_PIN_WAIVED=1 \
    RATE=12 DURATION_MS=8000 PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.5 PROBE_WEIGHT=0 \
    REPS=1 ARMS="shared mps" OUT="$WORK/run" \
    bash hack/m5c-matrix.sh ) > "$WORK/fail-mps.log" 2>&1
set -e
# The failure diagnostic has to name THIS scenario's log. Without this line bad() kept printing the tail of
# the previous scenario's file, so a reader chasing an mps failure was handed the engine scenario's output.
CURRENT_LOG="$WORK/fail-mps.log"
grep -q "REFUSED mps" "$WORK/fail-mps.log" \
  && ok "the arm was refused by name" || bad "no refusal was announced for the mps arm"
grep -q "not an MPS client" "$WORK/fail-mps.log" \
  && ok "the refusal says what was missing" || bad "the refusal does not say why"
[ -s "$WORK/run/refused-mps.txt" ] \
  && ok "refused-mps.txt was written where the report reads it" || bad "no refused-mps.txt, so reading 4c has no evidence to fire on"
compgen -G "$WORK/run/raw-shared-"'*.jsonl' >/dev/null \
  && ok "the arm beside it still produced evidence" || bad "a refused arm took the whole run down with it"

# THE REPORT MUST FIND THE FILE. Whether reading 4c then FIRES cannot be shown here, and asserting it was
# this script's own mistake on its first run.
#
# Stub engines answer in milliseconds, so the control produces no contention and reading 4 fires INVALID --
# which short-circuits the evaluator before 4c is evaluated at all. That is the correct verdict for this
# evidence and it makes the firing unreachable from a rehearsal. The firing is a unit test's business
# (TestARecordedRefusalIsWhatMakesFourCReachable), and what is left for here is the half a unit test cannot
# reach: that the file the runner wrote is the file the report reads, from a directory it discovered itself.
if compgen -G "$WORK/run/raw-"'*.jsonl' >/dev/null; then
  args=(); for f in "$WORK/run"/raw-*.jsonl; do args+=(--raw "$f"); done
  set +e; go run ./cmd/benchharness report "${args[@]}" > "$WORK/refused-report.txt" 2>&1; set -e
  CURRENT_LOG="$WORK/refused-report.txt"
  grep -qE '^\s*\[( N/E |FIRED)\] 4c .*no recorded refusal' "$WORK/refused-report.txt" \
    && bad "the report says there is no recorded refusal while refused-mps.txt sits beside the raw files it was given" \
    || ok "the report did not claim the refusal was absent"
fi
rm -rf "$WORK/run"

fi

# ---------------------------------------------------------------- 4. a split plugin that advertises wrongly
#
# THE SEVENTH PILOT DIED HERE, with three arms already measured and paid for.
#
# The device-count check called `fail`, which exits, so the session ended before it could run its own report
# and the mps arm left no refusal for reading 4c. The caller's `apply_device_plugin "$arm" || return 1` had
# been unreachable code since it was written. Nothing in this rehearsal covered the path, which is why a
# green suite and a dead session were consistent with each other.
#
# The count is forced wrong by asking the time-slicing ConfigMap for a number the simulator will not
# advertise, which is the same shape as a plugin that cannot honour its config on a real card.
if want 4; then
say "4. a split arm whose plugin advertises the wrong device count -- REFUSED, or does it end the session?"
prepare_copy
# ONLY the matrix's expectation moves. The ConfigMap is left alone on purpose: the failure being rehearsed
# is "the node does not advertise what this arm needs", so the two have to DISAGREE. The first version of
# this scenario changed both to 7, which made them agree, and the check passed -- a rehearsal of a failure
# that was not failing.
python3 - "$SRC/hack/m5c-matrix.sh" <<'PY2'
import sys
p=sys.argv[1]; s=open(p).read()
s=s.replace('timeSlicing) keep=config/nvidia-device-plugin-timeslicing; ds=nvidia-device-plugin-timeslicing; want=2 ;;',
            'timeSlicing) keep=config/nvidia-device-plugin-timeslicing; ds=nvidia-device-plugin-timeslicing; want=7 ;;')
open(p,'w').write(s)
PY2
rm -rf "$WORK/run"
set +e
( cd "$SRC" && PLATFORM=kind KCTX="$KCTX" GPU_NODE="$GPU_NODE" \
    DEADLINE_EPOCH=$(( $(date +%s) + 3600 )) \
    GATEWAY_BIN="$WORK/gateway" BENCHHARNESS_BIN="$WORK/benchharness" \
    ENGINE_PIN_WAIVED=1 \
    RATE=12 DURATION_MS=8000 PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.5 PROBE_WEIGHT=0 \
    REPS=1 ARMS="shared timeSlicing" OUT="$WORK/run" \
    bash hack/m5c-matrix.sh ) > "$WORK/fail-devcount.log" 2>&1
set -e
CURRENT_LOG="$WORK/fail-devcount.log"
grep -q "REFUSED timeSlicing" "$WORK/fail-devcount.log" \
  && ok "the arm was refused rather than ending the session" || bad "the wrong device count ended the session; the arms beside it were paid for"
[ -s "$WORK/run/refused-timeSlicing.txt" ] \
  && ok "refused-timeSlicing.txt was written where the report reads it" || bad "no refusal file, so reading 4c has nothing to fire on"
compgen -G "$WORK/run/raw-shared-"'*.jsonl' >/dev/null \
  && ok "the control beside it still produced evidence" || bad "the refused arm took the measured arm down with it"
grep -q "what the plugin and the node report" "$WORK/fail-devcount.log" \
  && ok "the diagnosis ran, so the refusal names evidence" || bad "the arm was refused with no diagnosis; the card is gone by the time anyone reads this"
grep -q "ignoring CONFIG_FILE" "$WORK/fail-devcount.log" \
  && bad "the refusal still asserts a cause a device count cannot establish" \
  || ok "it reports the count without inventing the cause"
rm -rf "$WORK/run"

fi

# ---------------------------------------------------------------- 3. a deadline that cannot hold the cells
if want 3; then
say "3. a deadline too short for the remaining cells -- does it stop on a boundary and say where?"
prepare_copy
# Enough deadline for the FIRST cell and not for the three, which is what this scenario is about: the
# measured projection, which only exists once a cell has been timed.
#
# It used to be 120 seconds. The matrix now also refuses BEFORE the first cell when the remaining time is
# below one replay -- a second, unmeasured guard -- and at DURATION_MS=8000 that floor is two minutes, so a
# 120-second deadline started firing the new refusal instead of this one. The scenario is about the measured
# stop, so it is given room to reach it. Scenario 5 covers the unmeasured one.
run_matrix "R1 shared timeSlicing mps" 150 "$WORK/fail-deadline.log" && bad "the matrix ran every cell against a deadline that could not hold them"
grep -q "STOPPING" "$WORK/fail-deadline.log" \
  && ok "it stopped rather than being cut mid-cell" || bad "no boundary stop; the run would have been cut mid-cell"
grep -qE "of [0-9]+ cells are complete" "$WORK/fail-deadline.log" \
  && ok "it says how much was bought before stopping" || bad "the stop does not say how many cells are complete"
fi

# ---------------------------------------------------------------- 5. a deadline that cannot hold even one
if want 5; then
say "5. a deadline shorter than a single replay -- does it refuse BEFORE rolling anything out?"
prepare_copy
# The measured projection does not exist yet at cell one, and "no projection" was read as "enough time":
# the matrix started its first cell against an already-expired deadline, rolled out the engines, replayed,
# and was cut mid-cell with nothing archived. No measurement is needed to know that a cell cannot finish
# faster than its own replay.
run_matrix "shared" 20 "$WORK/fail-first-cell.log" && bad "the matrix started a cell against a deadline shorter than one replay"
grep -q "STOPPING before the first cell" "$WORK/fail-first-cell.log" \
  && ok "it refuses before the first cell rather than after it" || bad "no pre-first-cell stop: $(tail -2 "$WORK/fail-first-cell.log" | tr '\n' ' ')"
grep -q "Nothing has been rolled out" "$WORK/fail-first-cell.log" \
  && ok "and it says nothing is half-bought" || bad "the refusal does not say what state the run is in"
if compgen -G "$WORK/run/raw-*.jsonl" >/dev/null; then
  bad "the run wrote raw evidence despite refusing before its first cell"
else
  ok "and no cell evidence was written"
fi
fi

echo
if [ "$failures" = "0" ]; then
  say "ALL FIVE FAILURE PATHS PINNED: each one ran, and each one said what a reader needs."
else
  fail "$failures assertion(s) failed above. A refusal that does not say why is one that has to be bought twice."
fi
