#!/usr/bin/env bash
#
# The four arms, replayed against the engine hack/m5b-gpu-session.sh is holding up.
#
# Split from that script on purpose: the session owns the expensive, slow, one-time half (a node, an
# engine, a capacity check, a measured rate) and this owns the half that gets re-run when an arm is
# botched. Re-running this costs minutes; re-running the session costs the node's warmup again.
#
# R1 is the uncontended premium baseline, off is the contended one, static-cap is the admission-matched
# control, kv-aware is the arm under test. They share ONE trace: the report asserts a single checksum
# across the contended arms, because arms that replay different traffic are not comparable however good
# their numbers look.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

NS="${NS:-m5b}"
KCTX="${KCTX:-$(kubectl config current-context 2>/dev/null)}"
# ENGINE and MODEL are read from the cluster further down, not set here.
#
# Both used to be assigned literals at this line and then overwritten by the cluster-derived values below,
# so the literals were dead code that read as the authority -- the shape that has cost this project two paid
# runs. Whatever the InferenceDeployment actually says is what the gateway routes on, so that is the only
# answer worth having.
# gpu-platform-gateway, not gateway: that is the repository this account actually has, and ECR does not
# create one on push. The old default named a repository that has never existed anywhere.
#
# The tag carries a timestamp because this repository has IMMUTABLE tags, so a second run of this script
# cannot reuse the first one's:
#
#   error from registry: The image tag 'm5b' already exists in the 'gpu-platform-gateway' repository
#   and cannot be overwritten because the tag is immutable.
#
# That failure lands after the GPU node is up and the engine is warm, which is the expensive place for it.
# A unique tag is also the honest one: the push is resolved to a digest a few lines below and the digest is
# what the record cites, so the tag is only an address -- and an address that names one build rather than
# whichever ran most recently.
GW_IMAGE="${GW_IMAGE:-gpu-platform-gateway:m5b-$(date -u +%Y%m%d-%H%M%S)}"
# Reps: the block bootstrap cannot bound its own variance from one repetition -- the interval degenerates
# to the point estimate and the report says so. Two is the floor, not a target.
# Four, matching hack/gpu-session.sh and the design.
#
# This defaulted to 2, and the session script it is meant to complement defaults to 4. The scripts do not
# read each other, so a re-run bought half the repetitions the study was designed around -- silently, and in
# the direction that weakens it. Two independent reviews scored the design's statistical power at 30% when
# n was 2 per cell and named the run count as the binding limit; that is the number this default was quietly
# restoring every time an arm was re-run.
#
# Below four the incremental interval is a bootstrap over very few blocks. Two is a floor the report will
# tolerate, not a target anything argued for.
REPS="${REPS:-4}"

# How calm the engine must be before the next arm starts, and how long to wait for it.
#
# The usage bound is the guard's own release threshold: below it the guard considers the backend calm, so a
# backend under it is one the next arm can be measured on without inheriting this one's pressure.
WASHOUT_USAGE="${WASHOUT_USAGE:-0.75}"
WASHOUT_TIMEOUT_S="${WASHOUT_TIMEOUT_S:-180}"

# The arms, named once. The order they RUN in is rotated per block; see armsForBlock below.
ARMS_ORDER="R1 off static-cap kv-aware"

# armsForBlock prints the arm order for one repetition block, rotated so position is not confounded with arm.
#
# Every block used to run R1, off, static-cap, kv-aware in that order, so R1 was always measured first on a
# cold engine and kv-aware always last on one that had been serving for an hour. Any drift over a block --
# prefix cache filling, thermal behaviour, a Spot neighbour arriving -- lands on the arms in a fixed pattern
# and is indistinguishable from the effect being measured. That is the confound, and no amount of extra
# instrumentation removes it after the fact.
#
# A Latin square fixes it exactly: with four arms and four blocks, rotating by the block index puts each arm
# in each position exactly once, so position effects cancel in the arm means instead of accumulating.
#
# It is worth saying what this does not fix. Order is balanced, not randomised, so a trend that happens to
# align with the rotation still lands unevenly; and carry-over between adjacent arms is handled by the
# washout below rather than by the square.
armsForBlock() {
  local block="$1" arms i n
  read -r -a arms <<< "$ARMS_ORDER"
  n=${#arms[@]}
  for ((i = 0; i < n; i++)); do
    printf '%s ' "${arms[$(((i + block - 1) % n))]}"
  done
}

# Arm B's frozen tuning, in tokens -- which is what the flags measure and what the last run did not give them.
#
# The runner used to pass "-admission-static-rate=${RATE}", where RATE is the trace's REQUEST rate, 1.568/s.
# The flag is tokens per second. It never passed -admission-static-burst at all, so the burst stayed at the
# gateway's 8,192 default while every noisy prompt in the trace estimates at 10,000 tokens -- 40,000 chars,
# a number written in the generator's own flag help. A request larger than the burst can never fit any
# bucket, so the gateway refused all 447 of them with 413, in all four repetitions. Arm B admitted nothing.
# It was not a badly tuned competitor; it was a second isolation arm, which is why C/B came out equal to
# C/R1 and all three pre-registered checks failed for a reason that had nothing to do with the guard.
#
# The design spec's Arm B section asks for these to come from an OFFLINE SIMULATION against a C pilot's
# admitted-work fraction, frozen before the confirmatory run. That simulation had never been run. The first
# attempt at these numbers skipped it too and divided C's admitted tokens by the replay's duration --
# 3.78M over 635s, 5,957 tokens/sec -- which is the average a matched bucket sustains, not the rate that
# produces it. Simulated against the real trace, 5,957 tok/s at a burst of 12,288 admits 52.5 percent, not
# 84.6: a 10,000-token withdrawal needs 1.68 seconds to refill and the arrivals do not wait for it.
#
# So these come from the simulation, on the 2026-09-03 pilot's own trace, via "benchharness sim-cap":
#
#   burst  = 3 x the largest eligible prompt, so a short run of long requests can pass without the bucket
#            behaving like a per-request gate. Anything at or below 10,000 makes arm B degenerate outright.
#   rate   = solved at that burst for the pilot's admitted fraction: 8,000 tok/s admits 0.8444 against the
#            pilot's 0.8456, within 0.12 percentage points.
#
# The check below re-runs that simulation on the trace this run actually generated, so the pair has to keep
# agreeing with the traffic rather than with the day it was chosen.
STATIC_RATE=8000
STATIC_BURST=30000
# The C pilot's admitted fraction of eligible offered tokens, the target arm B is matched to.
#
# Provisional, and the tolerance says so. The pilot measured 0.8456 over standard-noisy alone, because its
# two probe tenants were refused with 403 before admission and are excluded from the population entirely.
# The simulation scores the population a CORRECTLY configured run will have, which includes the probes above
# the threshold -- so the two fractions are over different populations and the agreement between them is an
# estimate, not a match. The real admission-match check is |B-C|/C on the run's own evidence, where both
# terms are measured over the same rows; this is only a pre-flight that stops a bucket which is off by half.
PILOT_ADMITTED_FRACTION=0.8456
PILOT_MATCH_TOLERANCE=0.10

# For ttl_remaining_minutes: this script spends the money in a shell that never armed the deadline.
. "$(dirname "$0")/lib/gpu-ttl.sh"
OUT="${OUT:-hack/m5b-run-$(date +%Y%m%d-%H%M%S)}"
LOG="$OUT/evidence.log"

k() { kubectl --context "$KCTX" "$@"; }
say() { echo "== $*" | tee -a "$LOG"; }
fail() { echo "ARMS FAILED: $*" | tee -a "$LOG" >&2; exit 1; }

# Read the measured rate from the file the session wrote, so it does not cross the shell boundary by hand.
# An explicit RATE still wins -- re-running one arm against a rate that was measured earlier is a real
# workflow -- but it is checked against the file when both exist, because the failure being prevented is a
# typo that no other guard can see.
RATE_FILE="${RATE_FILE:-$PWD/.m5b-rate}"
if [ -f "$RATE_FILE" ]; then
  file_rate=$(head -1 "$RATE_FILE" 2>/dev/null | tr -d '[:space:]')
  if [ -z "${RATE:-}" ]; then
    RATE="$file_rate"
    say "rate ${RATE}/s read from $RATE_FILE"
  elif [ -n "$file_rate" ] && [ "$RATE" != "$file_rate" ]; then
    fail "RATE=$RATE was given but $RATE_FILE says $file_rate. One of them is a typo, and a rate that is wrong by a factor of ten oversubscribes the card, censors every tail, and disqualifies the run after it is paid for. Delete the file to override deliberately."
  fi
fi
case "${RATE:-}" in
  ''|*[!0-9.]*) ;;
  *) awk -v r="$RATE" 'BEGIN{ if (r <= 0 || r > 50) exit 1 }' \
       || fail "RATE=$RATE is outside anything this card can do. An A10G prefills about 1.1/s on this prompt; 50/s is past every card in the family." ;;
esac
[ -n "${RATE:-}" ] || fail "RATE is unset. It must come from hack/m5b-gpu-session.sh's prefill measurement on THIS card, not from a default: the harness default of 20/s demands 3.8x an A10G's theoretical peak, and 7.3x a T4's, and a run at it censors every arm's tail and is disqualified by EvaluateChecks after the card has been paid for."

mkdir -p "$OUT" || fail "cannot create $OUT"
: > "$LOG"
say "rate ${RATE}/s, ${REPS} repetitions, output $OUT"

WORK="$(mktemp -d)"
PF_PID=""
# A signal must also END the script, and these traps did not.
#
# `trap cleanup INT` runs cleanup and then RESUMES where the signal arrived. During the ten-minute wait for
# a node to join, Ctrl-C therefore scaled the node group to zero, dropped the deadline, and went back to
# waiting for the node it had just cancelled -- for another ten minutes, on a card the operator had just
# asked to stop. The cost was cleaned up; the session was not.
#
# EXIT still runs cleanup for ordinary exits and for `fail`. The signal handlers do their own cleanup and
# exit with the conventional 128+signal status, and the guard makes the second call a no-op so the EXIT
# trap that follows does not repeat the work.
CLEANED=0
cleanup() {
  [ "$CLEANED" = "1" ] && return 0
  CLEANED=1
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  rm -rf "$WORK"
  return 0
}
# HUP: a closed terminal sends it, and this script runs for over an hour on a rented card.
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"

# Duration is derived from the rate so each arm reaches a tail worth reporting. MinTailSamples is 100:
# below it a nearest-rank p99 is just the slowest request, and the report disqualifies the run. 500 premium
# completions is the target; premium is half the arrivals.
DURATION_MS=$(python3 -c "print(int(500 / (float('$RATE')/2) * 1000))")
say "duration per arm: $((DURATION_MS/1000))s, targeting 500 premium completions (min is 100)"

# Refuse a run that cannot finish before the deadline cuts the node out from under it.
#
# This is the one check that has to live here rather than in hack/m5b-gpu-session.sh, because the length of
# the run is decided here: it comes from RATE, which is measured on the actual card, times REPS, times the
# four arms. The session script arms the deadline long before any of those numbers exist.
#
# Getting this wrong does not produce a warning. It produces a paid run that is killed partway through the
# last arm, and a comparison missing one of the four things it exists to compare -- the money is spent and
# the evidence is unusable. Refusing costs nothing by comparison: raise TTL_MINUTES, lower REPS, or accept
# that the card is too slow for this shape of run.
#
# The 180 is the gateway rollout timeout used below, counted once per replay because the gateway is
# redeployed for each arm of each repetition.
# Derived here, the same way hack/m5b-gpu-session.sh derives them, because this script never had them: the
# check above was written against ${CLUSTER:-} and ${NODEGROUP:-}, which are empty in this shell, so it took
# the warning branch every time and could not have refused anything. A guard that cannot fire is worse than
# no guard, because it reads like coverage.
CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"
NODEGROUP="${NODEGROUP:-$(kubectl get node -l 'platform.lkhun9311.github.io/gpu=true' \
  -o jsonpath='{.items[0].metadata.labels.eks\.amazonaws\.com/nodegroup}' 2>/dev/null)}"

if REMAIN=$(ttl_remaining_minutes "$CLUSTER" "$NODEGROUP" 2>/dev/null) && [ -n "$REMAIN" ]; then
  replays=$(( 4 * REPS ))
  worst_min=$(( replays * (DURATION_MS / 1000 + 180) / 60 ))
  say "worst case ${worst_min} min over ${replays} replays; ${REMAIN} min left on the deadline"
  [ "$worst_min" -le "$REMAIN" ] || fail "this run needs up to ${worst_min} min but the TTL deadline fires in ${REMAIN} min. The node would be scaled out from under the last arm and the comparison would be incomplete. Re-arm with a longer TTL_MINUTES, or lower REPS (currently ${REPS})."
else
  # A warning was the wrong answer. This script puts an hour of real load on a rented card in a shell that
  # is not the one holding the session open, so "no remote backstop" is the state it least wants to be in --
  # and the same branch is reached by an AWS error, a mis-derived cluster name, and a genuinely absent
  # schedule, which are not equally acceptable but were treated identically.
  #
  # NO_TTL=1 is the deliberate override, for replaying against a node that is not on EKS at all.
  if [ "${NO_TTL:-}" = "1" ]; then
    say "NO_TTL=1: running without a remote deadline. Nothing here will stop the node."
  else
    fail "no TTL deadline found for ${CLUSTER:-?}/${NODEGROUP:-?}. This script loads a rented card for about an hour from a shell that does not own the session, so it will not start without a deadline that outlives this laptop. Check that hack/m5b-gpu-session.sh armed one, or set NO_TTL=1 if this node is not on EKS."
  fi
fi

say "build and push the gateway image"
cat > "$WORK/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static:nonroot
COPY gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
EOF
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build gateway image"
if [ -n "${REGISTRY:-}" ]; then
  # Log in before pushing, and check the repository exists first.
  #
  # Neither was here. The push went to $REGISTRY/gateway:m5b while the repository in this account is called
  # gpu-platform-gateway, and ECR does not create repositories on push; there was also no
  # `aws ecr get-login-password | docker login` anywhere in this file, so the push would have failed on
  # authentication even against the right name. Both failures land at the same place: after the GPU node is
  # up, the engine is warm, and the session shell is holding a port-forward.
  #
  # The repository name is derived from the image name rather than assumed, so GW_IMAGE stays the one place
  # that decides what this pushes.
  ecr_repo="${GW_IMAGE%%:*}"
  ecr_registry_host="${REGISTRY%%/*}"
  aws ecr describe-repositories --repository-names "$ecr_repo" >/dev/null 2>&1 \
    || fail "no ECR repository named $ecr_repo in this account. The push would fail after the card is warm; create it, or set GW_IMAGE to a repository that exists."
  aws ecr get-login-password | docker login --username AWS --password-stdin "$ecr_registry_host" >/dev/null 2>&1 \
    || fail "could not log in to $ecr_registry_host. docker push authenticates against ECR with a token that expires; without it the push fails with the GPU already billing."
  docker tag "$GW_IMAGE" "$REGISTRY/$GW_IMAGE" && docker push "$REGISTRY/$GW_IMAGE" >/dev/null \
    || fail "push gateway image to $REGISTRY"
  GW_IMAGE="$REGISTRY/$GW_IMAGE"
  # Resolve the tag to the digest that was just pushed, and use THAT everywhere after this line.
  #
  # The tag is how the push is addressed; it is not what the record may claim. gateway:m5b names whatever was
  # pushed under it most recently, so a manifest carrying it identifies nothing after the next build -- and
  # the manifest fields for this (gatewaySHA, imageDigests) had never been filled by anything at all.
  GW_DIGEST_REF="$(docker inspect --format='{{index .RepoDigests 0}}' "$GW_IMAGE" 2>/dev/null || true)"
  case "$GW_DIGEST_REF" in
    *@sha256:*) GW_IMAGE="$GW_DIGEST_REF" ;;
    *) fail "could not resolve $GW_IMAGE to a digest after pushing it; the run would record a tag, which names nothing after the next build" ;;
  esac
else
  fail "REGISTRY is unset. A kind cluster can side-load an image; EKS cannot, so the gateway image must be pushed somewhere the nodes can pull from (ECR). Set REGISTRY=<account>.dkr.ecr.<region>.amazonaws.com"
fi

say "identity and policy"
k get ns "$NS" >/dev/null 2>&1 || fail "namespace $NS does not exist; run hack/m5b-gpu-session.sh first"

# The model the engine actually serves, read from the routing record rather than defaulted.
#
# gen-trace's --model defaults to llama-3-8b, which is a STUB name from hack/m5b-gateway-path.sh, and this
# script never passed the flag. So the first paid run asked a real Qwen engine for a model no
# InferenceDeployment served: the router returned ErrNoRoute and the gateway answered 404 to 907 of every
# 999 requests. Three hours of card time recorded zero completions, and the only thing that noticed was the
# report, at the end.
#
# Reading it from the cluster is what makes it agree by construction: the router resolves a request by
# matching this same field, so a name taken from anywhere else can drift from what will actually route.
# The engine's own name, read from the same record the model name comes from.
#
# The washout polls this Service's /metrics, and the provenance step reads this Deployment's image digest.
ENGINE=$(k get inferencedeployment -n "$NS" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -n "$ENGINE" ] || fail "no InferenceDeployment in $NS to read the engine name from; run hack/m5b-gpu-session.sh first"
MODEL=$(k get inferencedeployment -n "$NS" -o jsonpath='{.items[0].spec.model.name}' 2>/dev/null)
[ -n "$MODEL" ] || fail "no InferenceDeployment in $NS to read the served model from; run hack/m5b-gpu-session.sh first"
say "model: $MODEL (read from the routing record the gateway matches on)"
# One tenant list, because four hand-kept copies of it drifted twice.
#
# The trace generator names the tenants, and this list has to agree with it in three places at once: the API
# key secret, the --api-keys flag the replay client authenticates with, and the GPUQuotaPolicy each tenant
# needs to clear authorisation. The first paid run had two of the four in the secret, so a quarter of every
# replay came back 401. The second had all four keyed but only two given a policy, so the two probe tenants
# -- the ones that exist to measure where the eligibility threshold fires -- came back 403 and the report
# presented their silence as "rejected=0". Both times the list was right in one place and wrong in another.
#
# Deriving all three from here removes the copies. The check after gen-trace is what makes the list binding
# rather than merely central: a tenant the generator invents and this list has not heard of stops the run
# before any card time is spent on it.
#
# Fields are tenant:key:tier, and an empty tier means the policy carries no tier annotation.
TENANTS=(
  "premium-1:premium-key:premium"
  "standard-noisy:standard-key:"
  "standard-probe-over:probe-over-key:"
  "standard-probe-under:probe-under-key:"
)

secret_args=()
api_keys=""
premium_tenants=""
policies=""
for entry in "${TENANTS[@]}"; do
  tenant="${entry%%:*}"; rest="${entry#*:}"; key="${rest%%:*}"; tier="${rest#*:}"
  secret_args+=("--from-literal=$key=$tenant")
  api_keys="${api_keys:+$api_keys,}$tenant=$key"
  if [ "$tier" = "premium" ]; then
    premium_tenants="${premium_tenants:+$premium_tenants,}$tenant"
  fi
  annotations=""
  if [ -n "$tier" ]; then
    annotations="
  annotations:
    platform.lkhun9311.github.io/tier: $tier"
  fi
  policies="$policies
---
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5b-$tenant$annotations
spec:
  tenant: $tenant
  targetNamespace: $NS
  gpuClass: t4
  limits:
    gpuCount: 1"
done

k create secret generic gateway-api-keys -n "$NS" \
  "${secret_args[@]}" --dry-run=client -o yaml | k apply -f - >/dev/null
printf '%s\n' "$policies" | k apply -f - >/dev/null || fail "apply policies"
say "identity for ${#TENANTS[@]} tenants: key and policy for each"

k create serviceaccount gateway -n "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null
k create clusterrolebinding "m5b-gateway-role" --clusterrole=gateway-role \
  --serviceaccount="$NS:gateway" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<EOF || fail "apply secret-reader role"
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: gateway-secret-reader, namespace: $NS}
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: gateway-secret-reader, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: gateway-secret-reader}
subjects:
  - {kind: ServiceAccount, name: gateway, namespace: $NS}
EOF

# One gateway, redeployed per arm, because the admission mode is a startup flag. The trace is generated
# ONCE and reused, so every contended arm replays identical traffic and the report's checksum assertion
# has something true to assert.
deploy_gateway() {
  local mode="$1" extra="$2"
  k apply -f - >/dev/null <<EOF || fail "apply gateway ($mode)"
apiVersion: apps/v1
kind: Deployment
metadata: {name: gateway, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: m5b-gateway}}
  template:
    metadata:
      labels: {app: m5b-gateway}
      # The flags, with the JSON punctuation stripped, because $extra is a JSON ARRAY FRAGMENT.
      #
      # It is built to be spliced into the args list on the line below -- `, "-admission-static-rate=1.565",
      # "-admission-long-threshold=4096"` -- so interpolating it into a quoted YAML scalar put raw quotes
      # and commas inside the quotes and broke the document:
      #
      #   error converting YAML to JSON: yaml: line 9: did not find expected ',' or '}'
      #
      # R1 and off pass an empty $extra, so the first two replays of the paid run succeeded and the third
      # failed -- twenty minutes of card time in, with a message that said "apply gateway (static-cap)" and
      # not why.
      #
      # Nothing reads this annotation; it is here so a human looking at the Pod can tell which arm's gateway
      # is running. Stripping the punctuation keeps that legible and cannot break the parse.
      annotations:
        arm: "$mode"
        armFlags: "$(printf '%s' "$extra" | tr -d '",' | tr -s ' ' | sed 's/^ *//')"
    spec:
      serviceAccountName: gateway
      containers:
        - name: gateway
          image: $GW_IMAGE
          args: ["-admission-mode=$mode"$extra]
          env:
            - {name: GATEWAY_NAMESPACE, value: $NS}
            - {name: GATEWAY_API_KEY_SECRET, value: gateway-api-keys}
          ports: [{containerPort: 8080, name: http}]
EOF
  k rollout status deploy/gateway -n "$NS" --timeout=180s >/dev/null || fail "gateway ($mode) never became ready"
}

# The build the gateway binary came from, and the engine image the numbers are produced against.
#
# GW_SHA is the working tree's commit, with -dirty when it is not clean: a paid run built from uncommitted
# code is a legitimate thing to do and an illegitimate thing to hide.
GW_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
git diff --quiet 2>/dev/null || GW_SHA="$GW_SHA-dirty"

ENGINE_IMAGE="$(k get deploy -n "$NS" "$ENGINE" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
case "$ENGINE_IMAGE" in
  *@sha256:*) ;;
  *) fail "the engine deployment runs image '${ENGINE_IMAGE:-<none>}', which is not digest-pinned; the record could not name what produced its latencies" ;;
esac
say "provenance: gateway $GW_SHA / $GW_IMAGE"
say "            engine  $ENGINE_IMAGE"

# The engine's own resolved configuration, into the evidence directory.
#
# The 2026-09-03 evidence records the engine's image digest and nothing else about how it was configured.
# So when the premium tail turned out to be a clean staircase of one full prefill per concurrent long
# request -- 944.6 ms observed against 951 ms of arithmetic -- the obvious next question was whether vLLM
# had been given a batch token budget large enough to swallow a 7,695-token prompt whole, and the evidence
# could not answer it. The question is about the run, the run is over, and the cluster is gone.
#
# Both halves are captured. The container's argv is what we asked for; the startup log is what vLLM decided,
# including the defaults it filled in for everything we did not ask about, which is exactly the part that
# turned out to matter. Failure here does not stop the run: this is provenance, not a precondition.
{
  echo "== engine deployment argv"
  k get deploy -n "$NS" "$ENGINE" -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null
  echo
  echo "== engine resolved configuration, as the engine reported it at startup"
  k logs -n "$NS" "deploy/$ENGINE" --tail=400 2>/dev/null \
    | grep -iE "engineargs|chunked|max_num_batched|max_num_seqs|max_model_len|cache_config|scheduler|kv cache|gpu blocks" \
    || echo "(no matching startup lines; the pod may have been restarted since)"
} > "$OUT/engine-config.txt" 2>&1 || true
say "engine configuration recorded to engine-config.txt"

say "generate the shared trace once"
"$WORK/benchharness" gen-trace --seed 7 --duration-ms "$DURATION_MS" --rate "$RATE" \
  --model "$MODEL" --arm off --gateway-url "http://127.0.0.1:18080" \
  --gateway-sha "$GW_SHA" --gateway-image "$GW_IMAGE" --engine-image "$ENGINE_IMAGE" \
  --trace-out "$OUT/trace.jsonl" --manifest-out "$OUT/manifest-off.yaml" || fail "gen-trace"

# The trace is what actually names the tenants, so it is the authority the list above is checked against.
#
# Without this the list is just a fourth copy in a nicer shape. A generator that adds a tenant -- a third
# probe, a second premium -- would hand it no key and no policy, and the run would record its 401s and 403s
# as ordinary outcomes for three hours before the report said anything.
missing=$(python3 -c "
import json,sys
known = set(sys.argv[2].split(','))
seen = {json.loads(l)['tenant'] for l in open(sys.argv[1]) if l.strip()}
print(','.join(sorted(seen - known)))" "$OUT/trace.jsonl" "$(printf '%s' "${TENANTS[*]}" | tr ' ' '\n' | cut -d: -f1 | paste -sd,)")
[ -z "$missing" ] || fail "the trace uses tenants this script has no key or policy for: $missing. Add them to TENANTS in this file; a tenant without both is refused before admission and its requests measure nothing."
say "every tenant in the trace has a key and a policy"

# The trace is stamped with the engine's own count for each distinct prompt length, before any arm runs.
#
# The design defines the admission-match criterion over the served tokenizer's input-token count, and three
# paid runs scored it on ceil(chars/4) -- 36 percent low on a 200-character prompt, 23 percent high on a
# 40,000-character one, per this repository's own calibration. The criterion has never been evaluated.
#
# It has to be on the TRACE rather than only on responses, because a refused request never reaches the engine
# and refused requests are the denominator of the fraction. The measurement uses the premium tenant, whose
# requests no admission mode refuses, so a probe cannot be shed before it is counted.
"$WORK/benchharness" stamp-exact-tokens -trace "$OUT/trace.jsonl" \
  -gateway-url "http://127.0.0.1:18080" -model "$MODEL" -api-keys "$api_keys" -tenant "${premium_tenants%%,*}" \
  || fail "the engine would not report its own input-token counts, so the admission-match criterion cannot be evaluated in the units the design defines it in"

# A burst below the largest prompt makes arm B refuse every eligible request, which is not a tuning error
# that shows up as a weak result -- it is a degenerate arm that reports cleanly and answers a question nobody
# asked. The last run spent three hours and a card on it. The trace knows the number, so it is asked.
max_est=$(python3 -c "
import json,sys,math
print(max(math.ceil(json.loads(l)['promptLenChars'] / 4) for l in open(sys.argv[1]) if l.strip()))" "$OUT/trace.jsonl")
# Strictly greater, matching the admitter: it refuses permanently on EstInputTokens > burst, so a prompt
# exactly equal to the burst is admissible from a full bucket and this must not reject that configuration.
if [ "$max_est" -gt "$STATIC_BURST" ]; then
  fail "the largest prompt in the trace estimates at $max_est tokens and STATIC_BURST is $STATIC_BURST. A request larger than the burst can never fit the bucket, so arm B would refuse every one of them with 413 and admit nothing -- the exact degenerate arm the 2026-09-03 run produced. Raise STATIC_BURST above $max_est or shrink the noisy prompt."
fi
say "arm B tuning: $STATIC_RATE tok/s, burst $STATIC_BURST, against a largest prompt of $max_est tokens"

# The tuning procedure itself, run against the trace this run will actually replay.
#
# The burst check above only rules out the degenerate case. This one asks the question the design spec asks:
# would this bucket admit the same share of eligible work that C admitted? It drives the gateway's own
# admitter through the trace's arrival times, so it answers with the real bucket rather than a model of one.
"$WORK/benchharness" sim-cap -trace "$OUT/trace.jsonl" \
  -rate "$STATIC_RATE" -burst "$STATIC_BURST" -long-threshold 4096 \
  -premium-tenants "$premium_tenants" \
  -target-admitted-fraction "$PILOT_ADMITTED_FRACTION" -tolerance "$PILOT_MATCH_TOLERANCE" \
  || fail "arm B's frozen tuning does not match the C pilot on this trace. Re-solve it with: benchharness sim-cap -trace $OUT/trace.jsonl -rate <r> -burst <b> -premium-tenants $premium_tenants"

# washout waits until the engine has actually drained, and refuses to continue if it never does.
#
# Rotating the arm order removes the confound between arm and POSITION. It does nothing about carry-over:
# the arm that runs after "off" starts against a KV cache that a thousand long prompts just filled, and a
# guard measured on that backend is being measured on the previous arm's residue.
#
# Sleeping a fixed number of seconds would be the same assumption in a different costume. This reads the
# engine's own counters -- the same series the guard scrapes -- and waits for the queue to empty and the
# cache to fall below the level the guard treats as calm. A backend that never drains is a fact about the
# run worth stopping for: whatever the next arm measured would be the tail of this one.
ENGINE_PF_PID=""
washout() {
  local why="$1" deadline=$((SECONDS + WASHOUT_TIMEOUT_S)) usage waiting
  if [ -z "$ENGINE_PF_PID" ] || ! kill -0 "$ENGINE_PF_PID" 2>/dev/null; then
    k port-forward -n "$NS" "svc/$ENGINE" 18081:8000 >/dev/null 2>&1 &
    ENGINE_PF_PID=$!
    sleep 2
  fi
  while [ "$SECONDS" -lt "$deadline" ]; do
    local body
    body=$(curl -sf -m 5 "http://127.0.0.1:18081/metrics" 2>/dev/null) || { sleep 2; continue; }
    waiting=$(printf '%s\n' "$body" | awk '/^vllm:num_requests_waiting/ {v=$NF} END {print (v==""?"":v)}')
    usage=$(printf '%s\n' "$body" | awk '/^vllm:kv_cache_usage_perc/ {v=$NF} END {print v}')
    [ -n "$usage" ] || usage=$(printf '%s\n' "$body" | awk '/^vllm:gpu_cache_usage_perc/ {v=$NF} END {print v}')
    if [ -z "$waiting" ] || [ -z "$usage" ]; then
      fail "the engine's /metrics carries neither a waiting-queue nor a cache-usage series, so a washout cannot be observed and every arm after the first would start on the previous one's residue"
    fi
    if awk -v w="$waiting" -v u="$usage" -v t="$WASHOUT_USAGE" 'BEGIN{exit !(w+0==0 && u+0<=t)}'; then
      say "  washed out before $why (waiting 0, cache $usage <= $WASHOUT_USAGE)"
      return 0
    fi
    sleep 3
  done
  fail "the engine did not drain within ${WASHOUT_TIMEOUT_S}s before $why (waiting=$waiting, cache=$usage). Whatever the next arm measured would be the previous arm's tail, so this stops rather than recording it as a result."
}

for rep in $(seq 1 "$REPS"); do
  say "block $rep order: $(armsForBlock "$rep")"
  for arm in $(armsForBlock "$rep"); do
    say "rep $rep arm $arm"
    washout "rep $rep arm $arm"
    case "$arm" in
      R1|off)      deploy_gateway off "" ;;
      static-cap)  deploy_gateway static-cap ", \"-admission-static-rate=${STATIC_RATE}\", \"-admission-static-burst=${STATIC_BURST}\", \"-admission-long-threshold=4096\"" ;;
      kv-aware)    deploy_gateway kv-aware ", \"-admission-long-threshold=4096\", \"-admission-report-backend-state\"" ;;
    esac

    [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
    k port-forward -n "$NS" deploy/gateway 18080:8080 >/dev/null 2>&1 &
    PF_PID=$!

    # Wait for the forward to ANSWER, rather than sleeping and hoping.
    #
    # `sleep 3` assumed three seconds was enough and that the forward would stay up. Neither holds. A
    # rollout has just finished, so the old Pod may still be terminating and port-forward can attach to it;
    # the forward then dies the moment that Pod goes, and every request in the replay fails with a transport
    # error rather than an HTTP status.
    #
    # In the second paid arms run that is exactly what happened, twice: static-cap recorded 999 rows in each
    # of two repetitions and every one of them was status=0/transport, while the arms either side of it
    # completed normally. The gateway was Running with zero restarts throughout -- it was never the gateway.
    for _pf in $(seq 1 30); do
      curl -sf -o /dev/null -m 2 "http://127.0.0.1:18080/healthz" 2>/dev/null && break
      curl -s -o /dev/null -m 2 "http://127.0.0.1:18080/" 2>/dev/null && break
      sleep 1
    done
    curl -s -o /dev/null -m 3 "http://127.0.0.1:18080/" 2>/dev/null \
      || fail "the port-forward to the gateway is not answering for arm $arm; a replay against it would record transport errors for every row and look like a result"

    "$WORK/benchharness" gen-trace --seed 7 --duration-ms "$DURATION_MS" --rate "$RATE" --model "$MODEL" \
      --arm "$arm" --gateway-url "http://127.0.0.1:18080" \
      --gateway-sha "$GW_SHA" --gateway-image "$GW_IMAGE" --engine-image "$ENGINE_IMAGE" \
      --trace-out "$OUT/trace-$arm-$rep.jsonl" --manifest-out "$OUT/manifest-$arm-$rep.yaml" \
      || fail "gen-trace $arm"
    "$WORK/benchharness" replay --manifest "$OUT/manifest-$arm-$rep.yaml" \
      --require-provenance \
      --target "http://127.0.0.1:18080" \
      --api-keys "$api_keys" \
      --raw-out "$OUT/raw-$arm-$rep.jsonl" || fail "replay $arm"
    [ -s "$OUT/raw-$arm-$rep.jsonl" ] || fail "no raw evidence for $arm rep $rep"
    say "  $(wc -l < "$OUT/raw-$arm-$rep.jsonl") rows recorded"

    # Did anything actually complete? Ask after the FIRST replay, not in the report.
    #
    # The first paid run recorded 999 rows per replay for three hours and every one of them was an HTTP
    # error: 404 because the trace asked for a model no InferenceDeployment served, 401 because two of the
    # four tenants were missing from the API key secret. "rows recorded" counted them all, because a row is
    # written whatever the response was -- which is correct for the record and useless as a signal.
    #
    # Nothing noticed until `report` refused at the end, by which time the card was paid for. One check
    # against the first replay would have cost eleven minutes instead of three hours.
    # EVERY replay, not the first of each arm.
    #
    # This checked only R1's first replay, so static-cap could record 999 transport errors in rep 1, again
    # in rep 2, and nothing said anything until the report. Widening it to each arm's first repetition was
    # still not enough: R1's SECOND repetition also recorded 460 transport errors in the same run, and a
    # check that only looks at rep 1 would have passed R1 and missed it.
    #
    # A replay that completes nothing is dead whichever arm and whichever repetition it is, and there is no
    # repetition whose emptiness is acceptable -- the report needs equal counts across arms, so one dead
    # replay costs the whole run anyway. Checking all sixteen costs one pass over a file each time.
    # Every replay is checked as it lands, by the same code the report uses.
    #
    # The rules are not repeated here. A copy of "usable" in bash would be a second definition, and the two
    # would drift the first time either moved -- the shape that has already cost this project two paid runs.
    # benchharness check-replay runs bench.Summarize and its thresholds, so the runner refuses exactly what
    # the report would refuse, at the replay that broke instead of three hours later.
    #
    # What it stops on, all of which have actually happened here: a replay that completed nothing; any 401 or
    # 403, which is decided before admission control and so measures nothing about the guard; eligible
    # requests whose admission verdict was lost, which is what a port-forward dying mid-replay looks like; and
    # a premium tail that lost more than one percent, since the tail is the primary endpoint.
    "$WORK/benchharness" check-replay -raw "$OUT/raw-$arm-$rep.jsonl" -label "$arm replay $rep" \
      || fail "replay $rep of arm $arm is unusable, so the run stops here rather than paying for the rest of it"
  done
done

say "report"
args=()
for f in "$OUT"/raw-*.jsonl; do args+=(--raw "$f"); done
# The report exits non-zero when the run is invalid, and `|| fail "report"` swallowed that into a generic
# message -- which also made the "run invalid" branch below unreachable, a guard that cannot fire inside
# the change that was about reading the verdict. The status is kept and the file is read either way,
# because an invalid run still wrote the document that says why.
report_rc=0
"$WORK/benchharness" report "${args[@]}" --out "$OUT/report.txt" || report_rc=$?
[ -s "$OUT/report.txt" ] || fail "the report exited $report_rc and wrote nothing; there is no evidence document for a paid run"
cat "$OUT/report.txt" | tee -a "$LOG"

# Read the verdict, do not merely count it. The check was `grep -q "VERDICT:"`, which passes on the one
# outcome that means the paid run produced nothing usable -- and then printed ARMS DONE and exited 0.
#
# The three outcomes are not equivalent and only one of them is a failure of this script:
#   "all checks passed"     - the guard protected the tail. A result.
#   "not all checks passed" - it did not. Also a result, and the one worth reporting honestly.
#   "run invalid"           - the evidence does not support any claim. The card was paid for and there is
#                             nothing to say, which is the case a zero exit status must not describe.
verdict=$(grep -m1 "VERDICT:" "$OUT/report.txt") || fail "the report carries no verdict"
case "$verdict" in
  *"run invalid"*)
    fail "the run is invalid and no claim can be made from it (report exit $report_rc): $verdict. The card has been paid for; look at $OUT/report.txt before spending another one." ;;
  *"not all checks passed"*)
    say "ARMS DONE, and the guard did not protect the tail on this card: $verdict"
    say "That is a result, not a failure of this script. Evidence in $OUT." ;;
  *)
    [ "$report_rc" -eq 0 ] || fail "the report exited $report_rc but its verdict reads ${verdict@Q}. Those disagree, and a paid run is not the place to guess which one is right."
    say "ARMS DONE. $verdict"
    say "Evidence in $OUT." ;;
esac

# The deadline is still armed and the node is still billing until it fires or the session shell exits.
if REMAIN=$(ttl_remaining_minutes "$CLUSTER" "$NODEGROUP" 2>/dev/null) && [ -n "$REMAIN" ]; then
  say "the GPU node is still up. The deadline scales it down in ${REMAIN} min; stop the session shell to do it now."
else
  say "Scale the GPU node group to 0 now."
fi
