#!/usr/bin/env bash
#
# The sharing matrix: does giving each tenant its own engine beat giving them one to share?
#
# That is the question, and it is the same mechanism M5-b measures seen from the other side. In the shared
# arm two tenants multiplex one vLLM, so they share a KV cache and the long-context tenant's occupancy is
# what costs the other one latency -- the pressure the admission guard exists to relieve. In the
# time-sliced arm each tenant gets its own engine with its own cache on half the card, so the KV coupling is
# gone and what remains is contention for the SMs. Neither is obviously better, which is why it is worth
# measuring rather than asserting.
#
#   shared        one engine, whole card, both tenants routed to it        (M5-b's topology)
#   timeSlicing   two engines, half card each, one tenant routed to each
#   mps           the same two engines, sharing through MPS instead
#
# MPS is not a third topology, it is a second mechanism for the same one, and the difference is worth
# stating because it is the reason both arms exist. Time-slicing interleaves kernels and does NOT partition
# memory -- the two engines draw on one pool and only convention keeps their utilizations summing below 1.
# MPS runs their kernels concurrently in one context AND caps each client's memory: the pinned control
# daemon issues set_default_device_pinned_mem_limit, read out of the binary rather than the documentation.
# So the arms differ in whether the tenants contend for SM time serially or concurrently, and in whether
# their memory ceiling is enforced or agreed.
#
# The routing is built from mechanisms this control plane already has: the gateway resolves a backend from
# the requesting tenant's GPUQuotaPolicy targetNamespace, so putting each engine in its own namespace and
# pointing one policy at each is all it takes. No new code, and the arm is a deployment difference rather
# than a code path.
#
# What this does NOT report is per-engine GPU utilisation. Under time-slicing a busy SM belongs to no single
# Pod and DCGM cannot attribute it -- config/nvidia-device-plugin/daemonset.yaml says so, and it is why the
# sharing node deliberately has no observer. The matrix reports what the CLIENTS measured, which is
# unambiguous and is also what a tenant actually experiences.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

# Where the card comes from, which is the only thing about this matrix that is not the experiment.
#
# eks is the original: a managed cluster with a GPU node group this script scales up and down, and an
# EventBridge deadline as the backstop for a shell that dies. kind is one rented Spot instance that is
# itself the node, with a kind cluster on it -- hack/queuelab-gpu-session.sh already builds exactly that on
# real A10Gs, and hack/m5c-gpu-session.sh is the wrapper that does it for this matrix.
#
# The reason both exist is that the cluster half of the AWS path has never been applied, so the eks branch
# below cannot be run today and cannot be tested. Its lines are therefore left exactly as they were and
# merely moved inside a function: a refactor of a paid-run path nobody can exercise is how a working script
# becomes a broken one silently. Everything the two platforms share -- the arms, the routing, the cells --
# is one copy, because a second written from memory of the first is the failure this repository has already
# paid for twice.
PLATFORM="${PLATFORM:-eks}"
KCTX="${KCTX:-$(kubectl config current-context 2>/dev/null)}"
NS_A="${NS_A:-m5c-a}"
NS_B="${NS_B:-m5c-b}"
MODEL="Qwen/Qwen2.5-3B-Instruct"
OUT="${OUT:-hack/m5c-run-$(date +%Y%m%d-%H%M%S)}"
LOG="$OUT/evidence.log"
GW_IMAGE="${GW_IMAGE:-gateway:m5c}"
# Whether the CALLER set these, recorded before the defaults below overwrite the answer.
#
# The ladder mode further down refuses a run that was given both a ladder and a single load, and it cannot
# ask that question after ARMS and REPS have been defaulted -- every run would look as though the operator
# had set them. This is the whole reason the snapshot exists, and it has to stay above the defaults.
ARMS_FROM_CALLER="${ARMS+set}"
REPS_FROM_CALLER="${REPS+set}"
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
# The arms, in the order they are run.
#
# R1 FIRST, and it is not a formality: it is the isolated baseline both bars are ratios against, so a run
# that is cut short after one cell should have the denominator rather than a numerator with nothing to
# divide by. It was absent from this default until the first paid run, whose readings then declined to
# evaluate anything for want of it.
#
# A session that only has time for a subset should say which rather than silently getting the default.
ARMS="${ARMS:-R1 shared timeSlicing mps}"

k() { kubectl --context "$KCTX" "$@"; }
# Both tee to the evidence log ONCE IT EXISTS, and only print before that.
#
# The preflight refusals run before $OUT is created, so tee'ing unconditionally printed
# `evidence.log: No such file or directory` underneath every one of them. A refusal that arrives with a
# spurious error beside it is how people learn to read past errors, which is the opposite of what a refusal
# is for.
say() { if [ -d "$OUT" ]; then echo "== $*" | tee -a "$LOG"; else echo "== $*"; fi; }
fail() { if [ -d "$OUT" ]; then echo "MATRIX FAILED: $*" | tee -a "$LOG" >&2; else echo "MATRIX FAILED: $*" >&2; fi; exit 1; }

case "$PLATFORM" in
  eks|kind) ;;
  *) fail "PLATFORM is ${PLATFORM@Q}; it must be eks or kind. Refusing rather than picking one: the two differ in what stops the card billing." ;;
esac

# LADDER turns this script into the capacity ladder of
# docs/superpowers/specs/2026-09-13-what-the-split-costs-in-throughput.md, and unset it changes nothing.
#
# It is a mode of this script rather than a second script because everything below the load -- acquiring the
# card, swapping the device plugin, rolling out one engine or two, the port-forward that is proved before it
# is used, the per-cell handover, the deadline projection -- is the same instrument, and a copy of it would
# be a copy that drifts. What differs is only which loads are offered and in which order.
#
# The format is one rung per whitespace-separated entry, two numbers joined by a colon, in climbing order.
# What the numbers mean is the STUDY's arrival model, read from internal/bench's registry: "RATE:NOISY_WEIGHT"
# for a weighted ladder, "PREMIUM_RATE:NOISY_RATE" for one registered with independent arrivals. The values
# are the pre-registration's, solved offline against the real gen-trace so that every rung offers the
# contender 139 requests give or take two while the premium rate climbs. They are passed rather than
# defaulted for the reason RATE is: a load this script chose for itself is a load nobody derived.
LADDER="${LADDER:-}"
if [ -n "$LADDER" ]; then
  # RATE and the ladder both describe the load, and a run that was given both would obey one of them
  # silently. Which one is not something an operator should have to read this file to find out.
  [ -z "${RATE:-}" ] || fail "RATE and LADDER are both set. The ladder carries a rate per rung, so a single RATE is either ignored or overrides them -- refusing rather than picking."
  [ -z "$ARMS_FROM_CALLER" ] || fail "ARMS and LADDER are both set. The ladder's arms are its two topologies plus one isolated baseline cell at the rung it stops on, which is not known until it stops."
  # The frozen matrix pools repetitions of one load. The ladder's rungs are DIFFERENT loads, and a run that
  # repeated them would write two files an arm summary would pool into a p99 for a load never offered.
  # A borderline rung is repeated by re-running that rung, which the pre-registration says and this refuses
  # to do by accident.
  [ -z "$REPS_FROM_CALLER" ] || fail "REPS and LADDER are both set. Ladder rungs are different loads rather than repetitions of one, and pooling two of them would report a p99 for a load that was never offered."
fi

[ -n "${RATE:-}" ] || [ -n "$LADDER" ] || fail "RATE is unset. Measure it from a single contender prefill on THIS card, the way hack/m5b-gpu-session.sh does; the harness default of 20/s demands 3.8x an A10G's theoretical peak and would censor every arm."

# The whole load, passed rather than defaulted -- and RATE alone was never enough.
#
# hack/m5b-price-of-protection.sh says it in one line: "gen-trace's defaults are stub-calibrated and the
# first pilot ran them at a GPU at ten times its prefill capacity." This script asked for RATE and left the
# tenant MIX at those defaults, which is the larger half of the same mistake.
#
# The arithmetic, because it is the reason this is a refusal and not a default. gen-trace defaults to
# premium 1, noisy 1 and two probe tenants at 0.1, so the contender takes 45% of arrivals. Its prompt is
# 40,000 characters, about 7,744 tokens, which the paid evidence measured at roughly 1.03 s of engine each.
# At the price-of-protection run's RATE of 9.85 that is 4.5 contender arrivals a second against a card that
# can serve about one -- four to five times capacity, and every arm censored.
#
# AND NO RATE FIXES IT. Bringing the contender down to the ~0.5/s that run used means RATE near 1.1, which
# leaves the premium tenant at about 0.5/s too: fewer than a hundred premium requests in a trace of this
# length, under the MinTailSamples floor that reading 4b exists to enforce. The mix has to move, not the
# rate. The run that measured a workable one used premium 1, noisy 0.054, probe 0.0054.
#
# DURATION_MS is here for the same reason. It used to be derived as 500/(RATE/2), an arithmetic that assumes
# the two tenants split arrivals evenly -- true of the defaults above and false of any calibrated mix, so it
# would have sized the trace from a premise the run had just abandoned.
# In ladder mode the contender's load is a property of the RUNG, because that is what holds the contender at a
# fixed absolute count while the premium rate climbs. A single NOISY_WEIGHT beside a ladder would be silently
# ignored, so it is refused.
REQUIRED_LOAD_VARS="PREMIUM_WEIGHT NOISY_WEIGHT PROBE_WEIGHT DURATION_MS"
if [ -n "$LADDER" ]; then
  [ -z "${NOISY_WEIGHT:-}" ] || fail "NOISY_WEIGHT and LADDER are both set. The ladder carries the contender's load per rung -- a weight, or a rate under a study registered with independent arrivals -- and that is how it holds the contender fixed in absolute terms while the premium rate climbs, so a single weight here would be ignored."
  REQUIRED_LOAD_VARS="PREMIUM_WEIGHT PROBE_WEIGHT DURATION_MS"
fi
for v in $REQUIRED_LOAD_VARS; do
  [ -n "${!v:-}" ] || fail "$v is unset. RATE alone does not describe this load: gen-trace's default mix puts the 40,000-character contender at 45% of arrivals, which is four to five times an A10G's prefill capacity at any rate this study could use, and lowering RATE to compensate starves the premium tail below the MinTailSamples floor. Derive the mix on the card and pass all four. hack/m5b-price-of-protection.sh measured RATE=9.85 PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.054 PROBE_WEIGHT=0.0054 DURATION_MS=420000 for ONE engine with the whole card; this run gives each engine half of one, so it is a starting point and not an answer."
done
# The engine build every arm runs, taken from the manifests rather than written here.
#
# The paid manifests carried no image identity at all: gen-trace has accepted --engine-image since it was
# written and nothing ever passed it, so a run's numbers named no build. They are digest-pinned in the
# YAML, which is the only place that can be true, and reading it here is what puts it in the record.
#
# The three files must AGREE. Two topologies running different engine builds is not a comparison of
# topologies, and nothing else in this script would notice: each arm applies its own manifest.
# WHATEVER the manifests pin, not a particular repository -- but it has to be a DIGEST.
#
# This matched `vllm/vllm-openai@sha256:` by name, which is the image the paid run uses and not the only
# image this script ever runs: the kind rehearsal substitutes a stub engine in a throwaway copy, and a check
# written around the production repository refused the rehearsal instead of the thing it was guarding
# against. What matters is that the reference is immutable and that the three manifests agree, neither of
# which is a statement about who publishes the image.
engine_image_in() { grep -m1 -oE 'image: *[^ ]+' "$1" | sed 's/image: *//'; }
# Unquoted where it is used, because empty must expand to NO argument rather than to an empty one.
PROVENANCE_FLAG="--require-provenance"
ENGINE_IMAGE=$(engine_image_in config/vllm/deployment.yaml)
[ -n "$ENGINE_IMAGE" ] \
  || fail "config/vllm/deployment.yaml names no engine image, so this run could not record which build produced its numbers"
# The waiver exists for ONE caller and cannot reach a paid run.
#
# hack/test/rehearse-m5c-matrix.sh substitutes a stub engine it builds locally, and a locally built image
# has no registry digest a kubelet can resolve -- so the rehearsal's tree genuinely violates the invariant
# this check enforces. The alternatives were to weaken the check for everyone or to let the rehearsal stop
# covering the ladder. This is the third: an explicitly named waiver that says what it costs, and that
# hack/m5c-gpu-session.sh -- the only thing in this repository that rents a card -- refuses to pass on.
case "${ENGINE_PIN_WAIVED:-}${ENGINE_IMAGE}" in
  1*) say "WARNING: the engine image pin is WAIVED. This run's evidence cannot name the engine build that produced it, and no paid run may carry this waiver"
      PROVENANCE_FLAG="" ;;
  *@sha256:????????????????????????????????????????????????????????????????) ;;
  *) fail "config/vllm/deployment.yaml pins the engine to ${ENGINE_IMAGE@Q}, which is a tag rather than a digest. A tag names whatever was pushed under it most recently, so the record would identify nothing after the next build" ;;
esac
# The agreement check is waived with the pin, and for the same reason.
#
# hack/test/rehearse-m5c-failures.sh makes an engine unstartable by pointing ONE manifest at an image that
# does not exist -- which is a deliberate disagreement, and refusing it here meant the scenario could never
# reach the diagnosis it exists to pin. A waived tree is a tree whose engine references are not the
# production ones, so this file does not judge them; the wrapper refuses to pass the waiver on to anything
# that rents a card.
if [ -z "${ENGINE_PIN_WAIVED:-}" ]; then
  for m in config/vllm-shared/engine-a.yaml config/vllm-shared/engine-b.yaml; do
    other=$(engine_image_in "$m")
    [ "$other" = "$ENGINE_IMAGE" ] \
      || fail "$m pins ${other:-no engine image} and config/vllm/deployment.yaml pins $ENGINE_IMAGE. Two topologies on two engine builds is not a comparison of topologies"
  done
fi
# The gateway's identity is the COMMIT this tree is at. Its image is not digest-pinned -- the matrix builds
# a binary and loads it into the node -- so --gateway-image is deliberately not passed rather than filled
# with something that looks like a digest and is not one.
SOURCE_COMMIT="${SOURCE_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"

# The arrival model this run's study registered, and the gen-trace load flags for one cell under it.
#
# A rung entry is two numbers whose meaning the STUDY decides, so the model comes from internal/bench's
# registry through `benchharness study-arrivals` rather than from a variable here that could disagree with
# it. gen-trace refuses the other model's flags for any study that registered one, so a mismatch stops at
# generation instead of reaching a replay. Resolved once the binary exists, which is later on each path.
ARRIVALS=""
resolve_arrivals() {
  # The frozen sharing matrix registered no arrival model. It has only ever been generated weighted, and this
  # keeps it so rather than asking a registry that has no answer for it.
  if [ -z "$LADDER" ]; then ARRIVALS=weighted; return; fi
  ARRIVALS=$("$WORK/benchharness" study-arrivals --study "$STUDY") \
    || fail "could not read study $STUDY's arrival model from the registry"
  # Independent arrivals have no mix to weight and this ladder has no probes, so the two weights the load
  # still requires must say exactly that rather than describe a mix that would be ignored.
  if [ "$ARRIVALS" = independent ] && { [ "$PREMIUM_WEIGHT" != 1 ] || [ "$PROBE_WEIGHT" != 0 ]; }; then
    fail "study $STUDY registered independent arrivals, where each rung carries the premium and contender RATES and the probes are off; PREMIUM_WEIGHT=$PREMIUM_WEIGHT PROBE_WEIGHT=$PROBE_WEIGHT describe a weighted mix that would be ignored, so pass 1 and 0"
  fi
}
set_load_flags() {
  case "$ARRIVALS" in
    weighted)    LOAD_FLAGS=(--rate "$1" --premium-weight "$PREMIUM_WEIGHT" --noisy-weight "$2" --probe-weight "$PROBE_WEIGHT") ;;
    independent) LOAD_FLAGS=(--premium-rate "$1" --noisy-rate "$2" --probe-rate 0) ;;
    *) fail "no arrival model was resolved for study $STUDY before generating a trace" ;;
  esac
}

CELLS=()
if [ -n "$LADDER" ]; then
  # Which ladder this is. They carry the same arm names and the same criterion and differ in where their rungs
  # sit or in how their traces are generated, so the study id is what tells a reader -- and a report -- which
  # experiment a row belongs to. Defaulted to the first so an existing caller keeps working.
  STUDY="${LADDER_STUDY:-throughput-ladder-2026-09-13}"
  case "$STUDY" in
    throughput-ladder-2026-09-13|throughput-ladder-down-2026-09-13|throughput-ladder-independent-2026-09-15) ;;
    *) fail "LADDER_STUDY is ${STUDY@Q}; internal/bench registers throughput-ladder-2026-09-13, throughput-ladder-down-2026-09-13 and throughput-ladder-independent-2026-09-15. This refusal is the one that stops it: gen-trace does NOT check the arm against the study -- it writes a manifest for any string -- and the check that does is in replay's manifest validation, which fires on the rented card after the engines are up" ;;
  esac
  ladder_rung=0
  for entry in $LADDER; do
    ladder_rung=$(( ladder_rung + 1 ))
    # `skip` holds a rung's POSITION without buying it.
    #
    # A repetition of rungs 2 and 3 must write rung02-* and rung03-*, not rung01-* and rung02-*: the arm name
    # carries the rung so that two different offered loads can never pool into one summary, and a run that
    # renumbered them would file 2.31 req/s under the name the first run gave 1.16 req/s. Dropping the entry
    # instead of marking it would shift every rung below it by one.
    case "$entry" in
      skip) continue ;;
      *:*) ;;
      *) fail "LADDER entry ${entry@Q} is not two numbers joined by a colon (RATE:NOISY_WEIGHT, or PREMIUM_RATE:NOISY_RATE under independent arrivals) or the word skip" ;;
    esac
    rung_rate="${entry%%:*}"; rung_weight="${entry##*:}"
    [ -n "$rung_rate" ] && [ -n "$rung_weight" ] || fail "LADDER entry ${entry@Q} is missing one of its two numbers"
    # Odd rungs run the control first, even rungs run the split first.
    #
    # This is the counterbalance, and it is the whole reason the ladder can say anything about the topology
    # rather than about the position: across the rungs each contended arm occupies the first and the second
    # slot of a rung equally often. The frozen matrix cannot do this -- it registered a fixed order and says
    # so -- and here it costs nothing.
    if [ $(( ladder_rung % 2 )) -eq 1 ]; then rung_order="shared timeSlicing"; else rung_order="timeSlicing shared"; fi
    for topology in $rung_order; do
      CELLS+=("$topology|$(printf 'rung%02d' "$ladder_rung")-$topology|1|$rung_rate|$rung_weight|$ladder_rung")
    done
  done
  [ "${#CELLS[@]}" -gt 0 ] || fail "LADDER is set but describes no rungs to buy -- every entry was skip, or the list is empty"
  # One more cell than the rungs describe: the isolated baseline, bought once at whichever rung the ladder
  # stops on. It is counted here so the deadline projection does not discover it at the end.
  cells_total=$(( ${#CELLS[@]} + 1 ))
else
  STUDY=sharing-matrix-2026-09-10
  for rep in $(seq 1 "$REPS"); do
    for arm in $ARMS; do
      CELLS+=("$arm|$arm|$rep|$RATE|$NOISY_WEIGHT|0")
    done
  done
  cells_total=${#CELLS[@]}
fi

# THE PLAN IS PRINTED BEFORE THE CARD IS TOUCHED.
#
# It used to be built after the cluster was acquired, so the first thing an operator saw of what they were
# buying was cell 1 of N already running. Everything here reads environment variables and nothing else, so
# there is no reason for it to wait -- and a plan printed before the first billable second is a plan that can
# be refused.
say "plan: $cells_total cell(s): $(for c in "${CELLS[@]}"; do printf '%s ' "${c#*|}" | cut -d'|' -f1; done | tr '\n' ' ')"

# Only EKS needs one. A kind node loads the image from the host daemon, and demanding a registry there would
# push an operator into standing one up for a cluster that can side-load.
[ "$PLATFORM" != eks ] || [ -n "${REGISTRY:-}" ] \
  || fail "REGISTRY is unset. EKS nodes cannot side-load an image, so the gateway must be pushed where they can pull it."

mkdir -p "$OUT" || fail "cannot create $OUT"
: > "$LOG"

WORK="$(mktemp -d)"
# Removed on every exit from here until the full cleanup trap below replaces this one.
#
# That trap is armed only after the functions it calls exist, and PLAN_ONLY exits long before then, as does
# every fail in between. So each plan check left this directory behind with a 34 MB benchharness in it, in
# /tmp, which is tmpfs here -- and 18 GB of them had accumulated by 2026-09-15.
trap 'rm -rf "$WORK"' EXIT
PF_PID=""
NODEGROUP=""

# PLAN_ONLY generates every planned trace and asks whether each cell could EVER be scored, then stops.
#
# Every refusal it applies already existed, and every one of them fired on the rented card: `replay`
# validates the arm against its study after the engines are up, and the cell floors are applied by the
# readings after the replay has finished. A mistyped study, a rung the registry does not admit, or a load
# whose counts the readings would refuse therefore cost a bring-up each time -- about 25 minutes of a
# billing instance, for a question that can be answered here in seconds.
#
# It runs the REAL gen-trace and asks the REAL bench.LadderPlanRefusal, so nothing about the registered
# floors is restated in this file. What it cannot check is anything that depends on results: whether an
# engine starts, whether the card fits two of them, or what any latency will be.
if [ -n "${PLAN_ONLY:-}" ]; then
  [ -n "$LADDER" ] || fail "PLAN_ONLY is only implemented for a ladder; the frozen matrix's plan is its arm list"
  if [ -n "${BENCHHARNESS_BIN:-}" ]; then
    [ -x "$BENCHHARNESS_BIN" ] || fail "BENCHHARNESS_BIN=$BENCHHARNESS_BIN is not an executable file"
    cp "$BENCHHARNESS_BIN" "$WORK/benchharness" || fail "could not take the shipped benchharness binary"
  else
    command -v go >/dev/null || fail "PLAN_ONLY needs either a Go toolchain or BENCHHARNESS_BIN"
    go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
  fi
  resolve_arrivals
  plan_top=0
  for spec in "${CELLS[@]}"; do
    IFS='|' read -r _ _ _ _ _ cell_rung <<<"$spec"
    [ "$cell_rung" -le "$plan_top" ] || plan_top=$cell_rung
  done
  plan_failures=0
  # The baseline is checked too, at the rung the ladder would end on if it ran every rung. That is the
  # latest it can be bought, and the earliest this file can name it.
  for spec in "${CELLS[@]}" "BASELINE"; do
    if [ "$spec" = BASELINE ]; then
      for s2 in "${CELLS[@]}"; do
        IFS='|' read -r _ _ _ cell_rate cell_weight cell_rung <<<"$s2"
        [ "$cell_rung" = "$plan_top" ] || continue
        cell_topology=R1; cell_label="$(printf 'rung%02d' "$plan_top")-R1"
        break
      done
    else
      IFS='|' read -r cell_topology cell_label _ cell_rate cell_weight cell_rung <<<"$spec"
    fi
    set_load_flags "$cell_rate" "$cell_weight"
    "$WORK/benchharness" gen-trace --seed 11 --duration-ms "$DURATION_MS" "${LOAD_FLAGS[@]}" \
      --study "$STUDY" --arm "$cell_label" --model "$MODEL" --gateway-url "http://127.0.0.1:18080" \
      --trace-out "$WORK/plan-$cell_label.jsonl" --manifest-out "$WORK/plan-$cell_label.yaml" >/dev/null \
      || { echo "PLAN REFUSED: gen-trace could not build $cell_label's trace" >&2; plan_failures=$(( plan_failures + 1 )); continue; }
    if ! out=$("$WORK/benchharness" ladder-plan-check --trace "$WORK/plan-$cell_label.jsonl" --study "$STUDY" --arm "$cell_label" 2>&1); then
      echo "PLAN REFUSED: $out" >&2
      plan_failures=$(( plan_failures + 1 ))
      continue
    fi
    say "  $out"
  done
  [ "$plan_failures" = 0 ] \
    || fail "$plan_failures planned cell(s) could not be scored as specified. Nothing was rented. Fix the load or the study and re-check -- this is the refusal that used to arrive after a bring-up"
  say "PLAN OK: every planned cell generates a trace the readings can score. Nothing was rented."
  exit 0
fi

# Scaling the node group back to zero, which this file's header claimed and this function did not do.
#
# It printed a banner. A banner is not a control: it depends on a human reading a terminal that may have
# scrolled, or that no longer exists because the laptop closed. The node group keeps desiredSize=1, the ASG
# keeps a GPU node, and this matrix's g5.xlarge bills $1.237/hour with nothing scheduled on it.
#
# The names are derived rather than asked for, because a prompt in a trap is a prompt nobody answers:
# the cluster comes from the kubeconfig context's EKS ARN and the node group from the node's own
# eks.amazonaws.com/nodegroup label. If either cannot be derived the function says so and prints the manual
# command -- degrading to the old behaviour rather than failing silently.
#
# KEEP_NODE=1 skips it, for a re-run against a node that is already warm rather than paying for a fresh
# warmup. The matrix has no separate re-run script, so this matters less here than in m5b-gpu-session.sh.
#
# What this does NOT solve, stated so it is not mistaken for solved: a trap cannot run after the laptop dies,
# loses power, or has its shell killed with SIGKILL. The backstop for that is the nightly destroy workflow,
# and the deadline this script registers with EventBridge Scheduler is what covers those.
# See hack/lib/gpu-ttl.sh.
gpu_scale_down() {
  # The deadline is NOT dropped here, and the ordering is the whole point.
  #
  # Removing it first means a scale-down that then fails leaves the account with a billing GPU and no
  # remaining backstop -- the one arrangement worse than either failure alone. The schedule costs nothing
  # while it waits and does nothing once the node is already at zero, so there is no reason to trade it away
  # before the thing it protects against is known not to have happened. It comes off at the bottom of this
  # function, after desiredSize has been read back as 0, and nowhere else.
  #
  # KEEP_NODE=1 keeps the node up; the deadline stays armed, so a session someone walks away from still ends.
  if [ "${KEEP_NODE:-}" = "1" ]; then
    echo "KEEP_NODE=1: leaving $NODEGROUP up. The TTL deadline stays armed; it bills until then." >&2
    return 0
  fi

  CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"

  # Ask EKS what is billing rather than trusting a flag set after the preflight checks. The same reasoning is
  # written out in m5b-gpu-session.sh: the refusals that run before the node group is identified used to exit
  # through a cleanup that believed nothing had been started.
  if [ -z "$NODEGROUP" ] && [ -n "$CLUSTER" ]; then
    for ng in $(aws eks list-nodegroups --cluster-name "$CLUSTER" \
                  --query 'nodegroups[?starts_with(@, `gpu`)]' --output text 2>/dev/null); do
      size=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
               --query 'nodegroup.scalingConfig.desiredSize' --output text 2>/dev/null)
      if [ -n "$size" ] && [ "$size" != "0" ] && [ "$size" != "None" ]; then
        echo "found $CLUSTER/$ng at desiredSize=$size without this session having recorded it" >&2
        # No break. The same fix went into hack/m5b-gpu-session.sh and this file was left with the old
        # shape, so a claim that "every stray group is scaled down" was true of one script and not the
        # other. Taking the first and stopping leaves the rest billing, in the function whose only job is
        # to find what is billing and stop it.
        if [ -z "$NODEGROUP" ]; then
          NODEGROUP="$ng"
        else
          aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
            --scaling-config minSize=0,maxSize=1,desiredSize=0 >/dev/null 2>&1 \
            && echo "also scaled $CLUSTER/$ng to zero" >&2 \
            || echo "WARNING: could not scale $CLUSTER/$ng down. IT IS STILL BILLING." >&2
        fi
      fi
    done
  fi

  if [ -z "$CLUSTER" ] || [ -z "${NODEGROUP:-}" ]; then
    echo
    echo "############################################################"
    echo "#  COULD NOT DERIVE cluster/nodegroup. SCALE TO 0 BY HAND. #"
    echo "#  It bills by the hour with nothing scheduled on it.      #"
    echo "#    aws eks update-nodegroup-config \\"
    echo "#      --cluster-name <cluster> --nodegroup-name <ng> \\"
    echo "#      --scaling-config minSize=0,maxSize=1,desiredSize=0  #"
    echo "############################################################"
    return 0
  fi

  echo "scaling $CLUSTER/$NODEGROUP to desiredSize=0" >&2
  # stderr is kept, so a failed scale-down says why rather than only that.
  if ! aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
        --scaling-config "minSize=0,maxSize=1,desiredSize=0" >/dev/null; then
    echo
    echo "############################################################"
    echo "#  SCALE-DOWN CALL FAILED. THE NODE IS STILL BILLING.      #"
    echo "#  Run this now:                                           #"
    echo "#    aws eks update-nodegroup-config --cluster-name $CLUSTER \\"
    echo "#      --nodegroup-name $NODEGROUP \\"
    echo "#      --scaling-config minSize=0,maxSize=1,desiredSize=0  #"
    echo "############################################################"
    return 0
  fi

  # Confirm it took. An accepted API call that left desiredSize at 1 is the failure this whole function
  # exists to make impossible, and it is silent unless something reads the value back.
  DESIRED=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
    --query 'nodegroup.scalingConfig.desiredSize' --output text 2>/dev/null)
  if [ "$DESIRED" = "0" ]; then
    echo "$CLUSTER/$NODEGROUP is at desiredSize=0" >&2
    # Only now. The deadline has nothing left to protect, and this is the single place that knows that.
    command -v ttl_disarm >/dev/null 2>&1 && ttl_disarm
  else
    echo "WARNING: $CLUSTER/$NODEGROUP reports desiredSize=$DESIRED after the scale-down. It is billing." >&2
    echo "The TTL deadline is left armed on purpose. It is what remains." >&2
  fi
}

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
  k delete namespace "$NS_A" "$NS_B" --wait=false >/dev/null 2>&1
  k delete gpuquotapolicy m5c-premium m5c-standard >/dev/null 2>&1
  k delete clusterrolebinding m5c-gateway-role >/dev/null 2>&1
  rm -rf "$WORK"
  release_card
}

# What stops the card billing, which is the one thing the two platforms cannot share.
#
# Under eks this is gpu_scale_down above, unchanged.
#
# Under kind the card IS the rented instance and this script does not own it: hack/m5c-gpu-session.sh rents
# it, arms a shutdown backstop inside it, and terminates it from outside. There is no node group to scale,
# and printing the COULD NOT DERIVE banner for a cluster that was never meant to have one would teach an
# operator to read past the banner on the day it means something. So it says plainly what it is not doing
# and which thing is.
release_card() {
  case "$PLATFORM" in
    eks) gpu_scale_down ;;
    kind)
      echo "the card is the rented instance, so nothing is scaled down here: hack/m5c-gpu-session.sh terminates it, and the instance's own shutdown backstop covers a shell that dies in this script" >&2
      ;;
  esac
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

say "preflight"

# The EKS card: scale a managed node group up, and register the deadline before it exists.
#
# Every line of this function is the code that was here before, moved and indented and otherwise
# untouched. `git diff -w` on the commit that introduced it shows only the wrapper, which is the point:
# the cluster half of the AWS path has never been applied, so nothing here can be exercised, and a
# path nobody can run is a path nobody can prove a refactor kept working.
acquire_card_eks() {
  case "$KCTX" in kind-*) fail "context $KCTX is a kind cluster; this is the paid matrix" ;; esac

  # The sharing node, and it must be the sharing one: the exclusive plugin advertises one device per card, so
  # the second engine would sit Pending and the arm would silently become a one-engine arm with half the memory.
  # The deadline is registered BEFORE the card exists, which is why this script scales the group up itself.
  #
  # It used to refuse when no sharing node was present, print the aws command, and wait to be re-run -- so a
  # person scaled up, the node billed and took minutes to join, and only a re-run registered a deadline. This
  # is the longest run in the repository and therefore the one most likely to outlive the shell that started
  # it; starting it with an unbacked card was the wrong end to be careless at.
  #
  # The name cannot come off a node label before a node exists, so it comes from a default verified against
  # EKS. A name that does not resolve would produce a schedule that is accepted and then fires into nothing.
  CLUSTER="${CLUSTER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].name}' 2>/dev/null | sed 's|.*cluster/||')}"
  [ -n "$CLUSTER" ] || fail "could not determine the cluster name from the kubeconfig context"
  # Named into a candidate first, and promoted to NODEGROUP only once EKS confirms it exists.
  #
  # Assigning NODEGROUP before the check meant that when the check refused, the EXIT trap's scale-down saw a
  # node group name, skipped discovery, and called update-nodegroup-config against a group just proven not to
  # exist -- then printed THE NODE IS STILL BILLING for a session that had started nothing. A commit claimed
  # no refusal path calls update-nodegroup-config; this was the path that did.
  NG_CANDIDATE="${NODEGROUP:-gpu_shared}"
  aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$NG_CANDIDATE" >/dev/null 2>&1 \
    || fail "no node group $NG_CANDIDATE in $CLUSTER. The deadline must name a group that exists, or it fires into nothing. Set NODEGROUP if the name is different."
  NODEGROUP="$NG_CANDIDATE"

  # One schedule names one node group, so a second GPU group left running is not covered by anything this
  # session registers. The deadline would fire, scale down the group it names, and leave the other billing --
  # and nothing in the run would look wrong.
  #
  # Refusing here rather than trying to cover both is deliberate. A session that finds another card already
  # running does not know whose it is: it may be another session mid-run, and scaling it down would destroy
  # someone else's paid work. Naming it and stopping is the only answer that is right in both cases.
  # Every AWS call here is checked, because the previous shape failed OPEN. It ran the listing inside a
  # command substitution, threw away stderr and the exit status, and iterated the result -- so expired
  # credentials, a permissions error, or a network failure all produced an empty list, which reads exactly
  # like "no other card is running". The same held one level down: a describe that failed left sz empty,
  # which the case treated as zero.
  #
  # That is the wrong direction for this particular guard. It exists for the moments when something is wrong
  # with the account, and those are the moments an unchecked call returns nothing.
  if ! ng_list=$(aws eks list-nodegroups --cluster-name "$CLUSTER" \
                   --query 'nodegroups[?starts_with(@, `gpu`)]' --output text 2>&1); then
    fail "could not list the node groups in $CLUSTER, so this session cannot tell whether another card is already running: $ng_list"
  fi
  others=""
  for ng in $ng_list; do
    [ "$ng" = "$NODEGROUP" ] && continue
    if ! sz=$(aws eks describe-nodegroup --cluster-name "$CLUSTER" --nodegroup-name "$ng" \
                --query 'nodegroup.scalingConfig.desiredSize' --output text 2>&1); then
      fail "could not read the size of $CLUSTER/$ng: $sz. An unreadable node group is not a stopped one."
    fi
    case "$sz" in
      0|None) ;;
      ''|*[!0-9]*) fail "the size of $CLUSTER/$ng came back as ${sz@Q}, which is not a number. Refusing rather than reading it as zero." ;;
      *) others="$others $ng($sz)" ;;
    esac
  done
  [ -z "$others" ] || fail "another GPU node group is already running:$others. This session's deadline names only $NODEGROUP, so that card would keep billing after the deadline fires. Scale it to zero, or if another session owns it, wait for it."

  TTL_ROLE_ARN="${TTL_ROLE_ARN:-$(terraform -chdir=infra/aws/bootstrap output -raw ttl_scaledown_role_arn 2>/dev/null)}"
  export TTL_ROLE_ARN
  # shellcheck source=hack/lib/gpu-ttl.sh
  . "$(dirname "$0")/lib/gpu-ttl.sh"
  if true; then
    ttl_arm "$CLUSTER" "$NODEGROUP" "${TTL_MINUTES:-240}" \
      || fail "could not register the TTL scale-down; refusing to start a card with no deadline"

    # What is checked here, and what is not.
    #
    # The first shape of this check multiplied the per-cell TIMEOUT budget by the cell count and refused if
    # the product exceeded the deadline. At the shipped defaults that is 456 minutes against 240, so the
    # design's own repetition count -- four, which a unit test pins because below it the incremental interval
    # is a bootstrap over very few blocks -- could never start. Lowering REPS to make the arithmetic work was
    # the wrong repair: it traded a statistical property for a scheduling one, quietly.
    #
    # Those 1800 seconds are two rollout TIMEOUTS, not two expected rollouts. A budget is what the script
    # waits before giving up, and refusing a run because the worst case does not fit refuses most runs that
    # would have finished. So the pessimistic product is gone and only the floor stays here: a deadline that
    # cannot hold even one cell is a session with nothing to gain. The real bound is measured per cell, in
    # the loop, where a slow matrix stops on a cell boundary with everything before it intact.
    ttl_min="${TTL_MINUTES:-240}"
    cell_floor_min=$(( (1800 + 180 + ${REPLAY_SECONDS:-300} + 59) / 60 ))
    if [ "$ttl_min" -lt "$cell_floor_min" ]; then
      ttl_disarm
      fail "the deadline is ${ttl_min} min but one cell can take up to ${cell_floor_min} min, so this session could not finish even a single cell. Raise TTL_MINUTES."
    fi
  fi

  # The card starts here, after the deadline exists.
  shared_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' -o name 2>/dev/null | wc -l)
  if [ "$shared_nodes" -eq 0 ]; then
    say "no sharing node present; scaling $NODEGROUP to 1 -- the card starts billing here, and the deadline is already registered"
    aws eks update-nodegroup-config --cluster-name "$CLUSTER" --nodegroup-name "$NODEGROUP" \
      --scaling-config minSize=0,maxSize=1,desiredSize=1 >/dev/null || fail "could not scale $NODEGROUP up"
    say "wait for the node to join (this takes a few minutes)"
    for i in $(seq 1 60); do
      shared_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' -o name 2>/dev/null | wc -l)
      [ "$shared_nodes" -gt 0 ] && break
      [ "$i" = 60 ] && fail "the node never joined after ten minutes. The deadline will scale it down; to do it now: aws eks update-nodegroup-config --cluster-name $CLUSTER --nodegroup-name $NODEGROUP --scaling-config minSize=0,maxSize=1,desiredSize=0"
      sleep 10
    done
  fi

  # Cross-check, not re-derive: a node from a different group means the deadline would scale down a group this
  # matrix is not using while the one it is using bills on.
  node_ng=$(k get node -l 'platform.lkhun9311.github.io/gpu-sharing=true' \
    -o jsonpath='{.items[0].metadata.labels.eks\.amazonaws\.com/nodegroup}' 2>/dev/null)
  if [ -n "$node_ng" ] && [ "$node_ng" != "$NODEGROUP" ]; then
    fail "the deadline names $NODEGROUP but the sharing node belongs to $node_ng. Re-run with NODEGROUP=$node_ng."
  fi

  [ "$shared_nodes" -eq 1 ] || fail "$shared_nodes sharing nodes are up; two engines on two nodes are not sharing a card, and nothing downstream could tell that apart from a sharing result"
}

# The kind card: one rented Spot instance that IS the node, with a kind cluster on it.
#
# hack/m5c-gpu-session.sh rents it, installs the driver and the container toolkit, builds the cluster and
# runs this script inside it. Nothing here scales anything, because there is nothing to scale: the card
# arrived with the instance and leaves with it.
acquire_card_kind() {
  case "$KCTX" in
    kind-*) ;;
    *) fail "PLATFORM=kind but the context is ${KCTX@Q}. This path labels nodes and expects to own the cluster it is pointed at, and finding out afterwards that it was a real one is not a way to learn it." ;;
  esac

  # The node whose card this is. kind's control plane carries its own NoSchedule taint and the session
  # script deliberately leaves the worker untainted, so the worker is where every engine runs.
  GPU_NODE="${GPU_NODE:-$(k get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)}"
  [ -n "$GPU_NODE" ] || fail "no non-control-plane node in $KCTX; hack/m5c-gpu-session.sh builds a cluster with one worker and the engines have nowhere else to run"

  # Nobody applies this label on kind, and the failure it causes is silent and expensive.
  #
  # Under EKS it comes off the node group (infra/aws/cluster/eks.tf). Here the cluster was built minutes ago
  # by the session script and the label is this script's to set. Without it every device-plugin overlay sits
  # Pending, the node advertises nothing, and the first arm fails at its 900-second rollout timeout having
  # billed for the card throughout. hack/queuelab-gpu-session.sh learned exactly this about the exclusive
  # plugin's own label and says so at the line that applies it.
  k label node "$GPU_NODE" platform.lkhun9311.github.io/gpu-sharing=true --overwrite >/dev/null \
    || fail "could not label $GPU_NODE; without platform.lkhun9311.github.io/gpu-sharing=true no device plugin will schedule and the node will advertise nothing"

  shared_nodes=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' -o name 2>/dev/null | wc -l)
  [ "$shared_nodes" -eq 1 ] || fail "$shared_nodes nodes carry the sharing label; two engines on two nodes are not sharing a card, and nothing downstream could tell that apart from a sharing result"
  say "card node $GPU_NODE labelled for sharing"

  # The deadline is the INSTANCE's, and this script only reads it.
  #
  # Under EKS the backstop is an EventBridge schedule this script registers, because the thing that bills is
  # a node group that outlives the shell. Here the thing that bills is the instance, the session script arms
  # its shutdown before handing over, and a second deadline registered here would be a second opinion about
  # when the money stops. So this refuses to run without being told when that is, rather than defaulting to
  # a number and reporting cell budgets against a deadline nobody set.
  [ -n "${DEADLINE_EPOCH:-}" ] || fail "DEADLINE_EPOCH is unset. hack/m5c-gpu-session.sh sets it to the instance's shutdown time; without it the per-cell budget check has no deadline to measure against and would let the matrix be cut mid-cell."
}

# Minutes left before the card stops, however this platform knows that.
#
# Both branches print nothing and return non-zero when they cannot tell, because cell_deadline_check treats
# an unreadable deadline as "carry on" -- a budget check that refused on a transient API error would end a
# paid run for no reason.
deadline_remaining_minutes() {
  case "$PLATFORM" in
    eks) ttl_remaining_minutes "$CLUSTER" "$NODEGROUP" ;;
    kind)
      local now
      now=$(date +%s)
      [ -n "${DEADLINE_EPOCH:-}" ] || return 1
      echo $(( (DEADLINE_EPOCH - now) / 60 ))
      ;;
  esac
}

# The card, from whichever platform this session is on. Everything below is the experiment and is one copy.
say "acquiring the card on $PLATFORM"
case "$PLATFORM" in
  eks)  acquire_card_eks ;;
  kind) acquire_card_kind ;;
esac

# The gateway's ClusterRole has to already exist, and this is checked because the failure is silent.
#
# Below, this script runs `kubectl create clusterrolebinding --clusterrole=gateway-role`. Kubernetes accepts
# a binding to a ClusterRole that does not exist -- it is a forward reference, not an error -- so the binding
# is created, the gateway starts, and every request then fails authorization when it tries to list the
# policies it routes from. Nothing before the first replay would look wrong.
#
# It is not applied here because it is not this run's to own: config/gateway/rbac.yaml is deployed with the
# gateway, by GitOps on EKS and by hack/m5c-gpu-session.sh on kind. Naming the file in the refusal is what
# turns this from a puzzle into a command.
k get clusterrole gateway-role >/dev/null 2>&1 \
  || fail "no ClusterRole gateway-role in this cluster. The gateway would start and then fail to list the GPUQuotaPolicy and InferenceDeployment it routes from, on every request, with nothing before the first replay looking wrong. Apply it: kubectl apply -f config/gateway/rbac.yaml"


# Every overlay here selects the SAME node, so exactly one may be applied at a time: two plugins registering
# nvidia.com/gpu against one kubelet socket is not a configuration worth debugging on a rented card.
# Deleting the others first is the mechanism; remembering is not.
#
# THREE modes, not two, and the third is why the `shared` arm could never have run.
#
# `shared` needs ONE device advertised on the sharing node, and nothing advertised one. The exclusive plugin
# selects a label the sharing node group deliberately does not carry, and the two sharing overlays advertise
# two. So the control arm's engine would have sat Pending to its 900-second timeout on a fresh node -- or,
# worse, scheduled onto a leftover replica from the previous arm and produced numbers for a control running
# on half a card. config/nvidia-device-plugin-whole-card is the plugin that was missing.
PLUGIN_NS="${PLUGIN_NS:-gpu-platform-control-plane-system}"
# plugin_diagnosis prints what the device plugin and the node actually reported.
#
# A refusal that names no evidence sends the next reader back to the card to find out what happened, and the
# card is gone. The seventh pilot refused an arm on a device count and recorded nothing about the DaemonSet,
# the Pods, the events or the node's own allocatable -- so "0 devices" was all anyone would ever know.
plugin_diagnosis() {
  local ds="$1"
  say "--- $PLUGIN_NS/$ds: what the plugin and the node report ---"
  k get ds "$ds" -n "$PLUGIN_NS" -o wide 2>&1 | sed 's/^/  /' | tee -a "$LOG" >&2
  k get pods -n "$PLUGIN_NS" -o wide 2>&1 | sed 's/^/  /' | tee -a "$LOG" >&2
  k logs -n "$PLUGIN_NS" "ds/$ds" --tail=40 2>&1 | sed 's/^/  /' | tee -a "$LOG" >&2
  k get events -n "$PLUGIN_NS" --sort-by=.lastTimestamp 2>&1 | tail -20 | sed 's/^/  /' | tee -a "$LOG" >&2
  k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' \
    -o jsonpath='{range .items[*]}  node {.metadata.name} allocatable nvidia.com/gpu={.status.allocatable.nvidia\.com/gpu} capacity={.status.capacity.nvidia\.com/gpu}{"\n"}{end}' 2>&1 \
    | tee -a "$LOG" >&2
}

apply_device_plugin() {
  # label is the arm name a refusal is recorded under, which in ladder mode names the rung as well as the
  # topology. mode stays the topology, because it is what selects the overlay.
  local mode="$1" label="${2:-$1}" keep ds want other
  case "$mode" in
    shared)      keep=config/nvidia-device-plugin-whole-card;  ds=nvidia-device-plugin-whole-card;  want=1 ;;
    timeSlicing) keep=config/nvidia-device-plugin-timeslicing; ds=nvidia-device-plugin-timeslicing; want=2 ;;
    mps)         keep=config/nvidia-device-plugin-mps;         ds=nvidia-device-plugin-mps;         want=2 ;;
    *) fail "apply_device_plugin: unknown mode $mode" ;;
  esac
  for other in config/nvidia-device-plugin-whole-card config/nvidia-device-plugin-timeslicing config/nvidia-device-plugin-mps; do
    [ "$other" = "$keep" ] && continue
    k delete -k "$other" --ignore-not-found --wait=true >/dev/null 2>&1
  done
  # A split arm's setup failure is a REFUSAL OF THAT ARM. The control's is still fatal.
  #
  # `fail` calls exit, so every path below used to end the session -- and the caller's
  # `apply_device_plugin "$arm" || return 1` at the split-arm site was unreachable code. The seventh pilot
  # died on the device-count check with R1, shared and timeSlicing already measured and paid for: the
  # session stopped before it could run its own report, and the mps arm left no refusal for reading 4c to
  # read. R1 and `shared` keep `fail` because they are the baseline and the control, and nothing below them
  # can be scored without both.
  plugin_gone() {
    if [ "$mode" = shared ]; then fail "$1"; fi
    arm_refused "$label" "$1"
    return 1
  }

  k apply -k "$keep" >/dev/null || { plugin_gone "the $mode plugin could not be applied"; return 1; }

  # The KEPT plugin has to be running before its advertisement is believed, and this is not belt-and-braces.
  #
  # Time-slicing and MPS both advertise two, so a count check alone is satisfied by the outgoing arm's stale
  # advertisement: switching timeSlicing -> mps would have passed on the devices the time-slicing plugin was
  # still registering, and the MPS arm would have begun as time-slicing under another name. Waiting for THIS
  # DaemonSet is what tells the two apart, and the count check below then confirms what it registered.
  k rollout status "ds/$ds" -n "$PLUGIN_NS" --timeout=180s >/dev/null \
    || { plugin_diagnosis "$ds"; plugin_gone "the $mode device plugin never became ready in $PLUGIN_NS; whatever the node is advertising belongs to the previous arm"; return 1; }

  say "wait for the card to advertise exactly $want device(s) under $mode"
  for i in $(seq 1 60); do
    adv=$(k get nodes -l 'platform.lkhun9311.github.io/gpu-sharing=true' \
      -o jsonpath='{.items[0].status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
    # Exactly, not at least. `-ge` reads the previous arm's larger number as success, which is how a
    # one-device arm would have started on a card the last arm had already split in two.
    [ "${adv:-0}" -eq "$want" ] 2>/dev/null && break
    # The COUNT is the evidence; the cause is not. This line used to assert "the plugin is ignoring
    # CONFIG_FILE", which a device count cannot establish -- zero devices is equally consistent with an
    # unhealthy device, a kubelet that has not re-registered, and a driver that went away. Naming a cause the
    # ledger does not support is the thing this repository puts above every other failure, and the seventh
    # pilot printed exactly that sentence about a card whose state it had not looked at.
    if [ "$i" = 60 ]; then
      plugin_diagnosis "$ds"
      plugin_gone "the node advertises ${adv:-0} device(s) after applying a $mode config that asks for $want, so this arm would run a topology other than the one it is labelled with. The diagnosis above says what the plugin and the node reported; this line does not claim to know why."
      return 1
    fi
    sleep 10
  done
  say "node advertises $adv device(s) for one physical card under $mode"

  # MPS has a second failure that time-slicing does not: the control daemon can be absent or unreachable
  # while the plugin still advertises, and every client then silently runs WITHOUT MPS. An arm that fell
  # back that way is the time-slicing arm under another name, and nothing downstream could tell.
  if [ "$mode" = mps ]; then
    # RECORDED and refused, not fatal. This is the same registered outcome as a client that never connected
    # -- reading 4c, "INVALID for that arm" -- and ending the session here would discard the arms beside it
    # and leave no evidence the report could read the refusal from.
    if ! k rollout status ds/nvidia-mps-control-daemon -n "$PLUGIN_NS" --timeout=180s >/dev/null; then
      arm_refused mps "the MPS control daemon never became ready, so clients would fall back to running without MPS and this arm would be time-slicing under another name"
      return 1
    fi
  fi
}

# Built here, or shipped in. The GPU AMI carries a driver, not a toolchain.
#
# `go build` on the instance fails with `go: command not found`, and it fails AFTER the driver, the cluster
# and the device plugin have all been paid for -- which is precisely the loss hack/queuelab-gpu-session.sh
# describes and answers by building on the laptop and shipping the binary. This is the same answer, and
# building stays the default so a run driven from a development machine needs nothing extra.
say "the gateway and the harness"
if [ -n "${GATEWAY_BIN:-}" ]; then
  [ -x "$GATEWAY_BIN" ] || fail "GATEWAY_BIN=$GATEWAY_BIN is not an executable file"
  cp "$GATEWAY_BIN" "$WORK/gateway" || fail "could not take the shipped gateway binary"
  say "  gateway: shipped, $(sha256sum "$WORK/gateway" | cut -c1-12)"
else
  command -v go >/dev/null || fail "no Go toolchain and GATEWAY_BIN is unset. On a rented GPU instance there is no compiler: build the binaries on the machine that has one and pass GATEWAY_BIN and BENCHHARNESS_BIN."
  CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"
fi
if [ -n "${BENCHHARNESS_BIN:-}" ]; then
  [ -x "$BENCHHARNESS_BIN" ] || fail "BENCHHARNESS_BIN=$BENCHHARNESS_BIN is not an executable file"
  cp "$BENCHHARNESS_BIN" "$WORK/benchharness" || fail "could not take the shipped benchharness binary"
  say "  benchharness: shipped, $(sha256sum "$WORK/benchharness" | cut -c1-12)"
else
  go build -o "$WORK/benchharness" ./cmd/benchharness || fail "build benchharness"
fi
resolve_arrivals
printf 'FROM gcr.io/distroless/static:nonroot\nCOPY gateway /gateway\nUSER 65532:65532\nENTRYPOINT ["/gateway"]\n' > "$WORK/Dockerfile"
# The image ID is CAPTURED, because it is the only thing that can name the gateway build in the record.
#
# `docker build -q` prints sha256:<64 hex> -- the digest of the image's own config, which covers its layers
# and therefore the base it was built on as well as the binary copied into it. It went to /dev/null, and the
# paid manifests consequently named no gateway at all.
#
# What it is NOT is a registry digest. Nobody can pull this reference; it identifies the image that ran on
# the machine that ran it. That is the strongest true statement available here, because this image is built
# on the instance and pushed nowhere, and a reference that looked pullable would be worse than one that does
# not pretend to be.
GW_IMAGE_ID=$(docker build -q -t "$GW_IMAGE" "$WORK") || fail "build gateway image"
case "$GW_IMAGE_ID" in
  sha256:????????????????????????????????????????????????????????????????) ;;
  *) fail "docker build returned ${GW_IMAGE_ID@Q} where an image ID was expected, so this run could not name the gateway build that produced its numbers" ;;
esac
GATEWAY_IMAGE_REF="$GW_IMAGE@$GW_IMAGE_ID"
say "  gateway image $GW_IMAGE_ID"

# How the node gets the image, which is the second thing the two platforms cannot share.
#
# EKS nodes pull, so the image has to exist somewhere they can reach. A kind node is a container on the same
# daemon that just built it, so `kind load` hands it over directly -- and the Deployment must then NOT say
# `imagePullPolicy: Always`, or the kubelet would go looking for a registry that was never involved. The tag
# is unqualified on purpose for the same reason: a `docker.io/` prefix would send it to a real registry.
case "$PLATFORM" in
  eks)
    say "push the gateway to $REGISTRY"
    docker tag "$GW_IMAGE" "$REGISTRY/$GW_IMAGE" && docker push "$REGISTRY/$GW_IMAGE" >/dev/null || fail "push gateway image"
    GW_IMAGE="$REGISTRY/$GW_IMAGE"
    ;;
  kind)
    say "load the gateway into the kind cluster"
    kind load docker-image "$GW_IMAGE" --name "${KCTX#kind-}" >/dev/null \
      || fail "could not load $GW_IMAGE into kind cluster ${KCTX#kind-}; the node cannot pull it from anywhere else"
    ;;
esac

# One routing record per engine namespace, replicas 0 and a no-op image for the reason
# hack/m5b-gpu-session.sh gives: InferenceDeploymentSpec has no args and no volumes, so it cannot describe a
# vLLM container. The engine Deployment must exist FIRST or the operator takes the name.
routing_record() {
  local ns="$1" name="$2"
  k apply -f - >/dev/null <<EOF || fail "routing record in $ns"
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata: {name: $name, namespace: $ns}
spec:
  model: {name: $MODEL, storageUri: "hf://$MODEL"}
  image: registry.k8s.io/pause:3.9
  gpuCount: 0
  replicas: 0
  port: 8000
EOF
}

# What an engine that never became ready was actually doing, captured before the refusal.
#
# `MATRIX FAILED: engine b never became ready` is what the first paid run of this matrix printed, and the
# whole log said nothing else about it. Whether the Pod was Pending for want of a device, CrashLooping on a
# CUDA error, still pulling fifteen gigabytes of image, or killed for memory are four different faults with
# four different fixes, and telling them apart cost a card.
#
# It goes to stdout so it lands in the run log the instance uploads, which is the only thing that outlives
# the machine. Everything here is best-effort: this function runs on the failure path, so a kubectl that
# also fails must not replace the diagnosis with its own error.
# What the engine ACTUALLY allocated, asked of the engine rather than computed.
#
# internal/bench/sharing.go sizes this run and says of its own arithmetic: "ESTIMATED, and the only
# estimated input here. vLLM prints the block count it actually allocated; compare KVTokensPerEngine against
# it at session start rather than trusting this." Nothing was doing the comparing.
#
# It matters most for the split arms and it is cheap everywhere, so it runs for every engine. Two engines at
# --gpu-memory-utilization=0.475 claim 21,877 of an A10G's 23,028 MiB and leave 1,151 for two CUDA contexts
# and the driver's reserve, which is the leading hypothesis for the engine b that would not start on
# 2026-09-11 -- and a hypothesis is all it is, because that run captured nothing that could settle it. These
# lines are what settle it next time, whether the arm succeeds or fails.
# Whether the ENGINES are actually MPS clients, which the daemon being ready does not establish.
#
# config/nvidia-device-plugin-mps/daemonset.yaml says the failure in its own words: "the control daemon can
# be absent or unreachable while the plugin still advertises, and every client then silently runs WITHOUT
# MPS", and "the control daemon and every client must share one IPC namespace, or the clients cannot reach
# the daemon's pipe". The only thing this runner checked was that the daemon had rolled out. That is the
# server side of a two-sided arrangement.
#
# It matters more than it looks. An mps arm whose clients never connected IS the timeSlicing arm, so the
# matrix would compare two mechanisms and have measured one of them twice -- and every number would look
# ordinary. Reading 4c exists for precisely this outcome and had no evidence it could act on.
#
# WHAT IS CHECKED, and what is not. CUDA_MPS_PIPE_DIRECTORY is what the plugin sets on a client container at
# allocation, and its presence plus a reachable pipe directory is what distinguishes a client from a process
# that merely has the card. It does NOT prove kernels are being routed through the MPS server; only nvidia-smi
# on the host can show that, and this runner deliberately puts no observer on the sharing node.
#
# It REFUSES rather than warns. An arm mislabelled as a mechanism it did not use is the one result this study
# must not produce, and hack/m5c-matrix.sh's own header says MPS and time-slicing differing "is the reason
# both arms exist".
# Returns non-zero and RECORDS WHY, rather than ending the run.
#
# "The sharing mode did not engage" is reading 4c, and its own name says INVALID *for that arm*. A refused
# MPS arm says nothing about the time-slicing arm beside it, and ending the session would throw away
# measurements that were made and paid for. The reason goes to $OUT/refused-mps.txt, which is where
# `benchharness report` looks: the readings run over raw files and would otherwise never learn that an arm
# was declined, because the reason lived only in a log nothing reads back.
mps_clients_connected() {
  local ns_a="$1" dep_a="$2" ns_b="$3" dep_b="$4" ns dep out rc
  for pair in "$ns_a:$dep_a" "$ns_b:$dep_b"; do
    ns="${pair%%:*}"; dep="${pair##*:}"
    out=$(k exec -n "$ns" "deploy/$dep" -- sh -c 'echo "PIPE=${CUDA_MPS_PIPE_DIRECTORY:-unset}"' 2>&1); rc=$?
    if [ "$rc" != "0" ]; then
      # A container with no shell is a DIFFERENT FACT from a client that did not connect, and calling the
      # first the second would report a mechanism failure nothing established. vLLM's image has a shell.
      arm_refused mps "could not ask $ns/$dep whether it is an MPS client, so this arm cannot be told apart from time-slicing: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
      return 1
    fi
    case "$out" in
      *PIPE=unset*)
        arm_refused mps "$ns/$dep has no CUDA_MPS_PIPE_DIRECTORY, so it is not an MPS client and this arm would be the time-slicing arm under another name. The control daemon being ready is the server half only; config/nvidia-device-plugin-mps/daemonset.yaml says clients fall back silently."
        return 1 ;;
    esac
    say "  $ns/$dep is an MPS client ($(printf '%s' "$out" | head -1))"
  done
}

# arm_refused records a registered refusal where the readings can find it.
arm_refused() {
  local arm="$1" why="$2"
  printf '%s\n' "$why" > "$OUT/refused-$arm.txt"
  say "REFUSED $arm: $why"
  say "  recorded in $OUT/refused-$arm.txt, which benchharness report reads as reading 4c"
}

engine_kv_report() {
  local ns="$1" deploy="$2"
  echo "--- $ns/$deploy: what the engine says it allocated ---"
  k logs -n "$ns" "deploy/$deploy" --tail=400 2>/dev/null \
    | grep -iE "KV cache|GPU blocks|gpu_memory_utilization|memory profiling|Available KV cache" \
    | tail -8 | sed 's/^/  /' \
    || echo "  (the engine printed no line naming its KV cache, which is itself worth knowing)"
}

engine_diagnosis() {
  local ns="$1" deploy="$2"
  echo "=== why $ns/$deploy never became ready ==="
  k get pods -n "$ns" -o wide 2>&1 | sed 's/^/  /'
  echo "--- deployment conditions ---"
  k get deploy "$deploy" -n "$ns" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>&1 | sed 's/^/  /'
  echo "--- pod events, which name a scheduling or image failure ---"
  k get events -n "$ns" --sort-by=.lastTimestamp 2>&1 | tail -20 | sed 's/^/  /'
  echo "--- the engine's own output, which names a CUDA or memory failure ---"
  k logs -n "$ns" "deploy/$deploy" --tail=40 --all-containers 2>&1 | sed 's/^/  /'
  k logs -n "$ns" "deploy/$deploy" --tail=40 --all-containers --previous 2>/dev/null | sed 's/^/  [previous] /'
  echo "--- what the node had left to give ---"
  k get node "${GPU_NODE:-}" -o jsonpath='{.status.allocatable}' 2>&1 | sed 's/^/  /'

  # THE PLUGIN AND THE MPS DAEMON, which live in another namespace and were the missing half.
  #
  # The 2026-09-12 pilot's mps arm failed with `Allocate failed due to no healthy devices present`. That is
  # the kubelet quoting the device plugin, so the answer to "why" is in the plugin's own log and in the MPS
  # control daemon's -- and this function captured neither, because it only ever looked in the engine's
  # namespace. It bought the symptom and not the cause.
  echo "--- the device plugins, which are what told the kubelet the devices are unhealthy ---"
  k get pods -n "$PLUGIN_NS" -o wide 2>&1 | sed 's/^/  /'
  for ds in nvidia-device-plugin-whole-card nvidia-device-plugin-timeslicing nvidia-device-plugin-mps nvidia-mps-control-daemon; do
    if k get ds "$ds" -n "$PLUGIN_NS" >/dev/null 2>&1; then
      echo "  --- $ds ---"
      k logs -n "$PLUGIN_NS" "ds/$ds" --tail=30 --all-containers 2>&1 | sed 's/^/    /'
    fi
  done
  echo
  echo "=== end of diagnosis ==="
}

deploy_arm() {
  # arm is the TOPOLOGY -- what to deploy. label is the arm name the evidence carries, which in ladder mode
  # also names the rung. They are the same string in the frozen matrix and differ in the ladder, and keeping
  # them separate is what lets one deployment path serve both: a refusal has to be recorded under the name
  # the readings look for, and the readings look for the arm in the rows.
  local arm="$1" label="${2:-$1}"
  k delete namespace "$NS_A" "$NS_B" --wait=true >/dev/null 2>&1
  # The policies go too, and they are NOT covered by deleting the namespaces.
  #
  # GPUQuotaPolicy is cluster-scoped, so it outlives both namespaces, and its targetNamespace is immutable:
  # api/v1/gpuquotapolicy_types.go carries `XValidation: self == oldSelf` with the message "targetNamespace
  # is immutable", because a policy enforces quota in exactly one namespace for its lifetime.
  #
  # Pointing one policy at each engine's namespace is this matrix's whole routing mechanism, and moving the
  # contending tenant from NS_A to NS_B is exactly what the split arms do. Re-applying over a surviving
  # object is therefore refused by the API, every time, on the first sharing arm -- `MATRIX FAILED: policies
  # for timeSlicing`. The exclusive arms never hit it because both of their policies name NS_A.
  #
  # The paid run of 2026-09-11 did not reach this: its engine b timed out first, one step earlier in this
  # same function. hack/test/rehearse-m5c-matrix.sh found it on a kind cluster for nothing.
  k delete gpuquotapolicy m5c-premium m5c-standard --ignore-not-found --wait=true >/dev/null 2>&1
  k create ns "$NS_A" >/dev/null; k create ns "$NS_B" >/dev/null
  case "$arm" in
    R1|shared)
      # One engine with the whole card. Both policies point at the same namespace, so both tenants resolve
      # to it -- which is exactly M5-b's topology and exactly what makes their KV caches one cache.
      #
      # R1 IS THIS TOPOLOGY, and it differs only in the trace. `benchharness gen-trace --arm R1` filters the
      # contending tenant out of the SAME trace, so premium arrives on the identical schedule it has in the
      # contended arms and the baseline is not inflated by running premium at twice its share. The matrix
      # already passes --arm "$arm", so nothing else here has to know.
      #
      # It was missing entirely. deploy_arm had cases for shared and for the sharing pair and none for R1,
      # so the runner could not produce the isolated baseline that is the DENOMINATOR of both of this
      # study's bars. The first paid run made that concrete: the readings declined to evaluate anything,
      # correctly, for want of an R1 the matrix had no way to measure.
      #
      # The plugin comes first and is not optional. This arm's engine asks for one nvidia.com/gpu, and until
      # config/nvidia-device-plugin-whole-card existed nothing advertised one on this node -- so the arm
      # either timed out Pending or inherited the previous arm's split card.
      apply_device_plugin shared "$label"
      k apply -f config/vllm/deployment.yaml -n "$NS_A" >/dev/null || fail "apply the exclusive engine"
      k apply -f config/vllm/service.yaml -n "$NS_A" >/dev/null || fail "apply the exclusive service"
      k rollout status deploy/vllm-qwen25-3b -n "$NS_A" --timeout=900s >/dev/null \
        || { engine_diagnosis "$NS_A" vllm-qwen25-3b; fail "the exclusive engine never became ready -- the diagnosis above says what it was doing"; }
      engine_kv_report "$NS_A" vllm-qwen25-3b
      routing_record "$NS_A" vllm-qwen25-3b
      PREMIUM_NS="$NS_A"; STANDARD_NS="$NS_A"
      ;;
    timeSlicing|mps)
      # The plugin step can now REFUSE the arm rather than end the run -- an MPS control daemon that never
      # became ready is reading 4c's business, not a reason to discard the arms beside it.
      apply_device_plugin "$arm" "$label" || return 1
      # A split engine that cannot be APPLIED refuses its arm, for the same reason one that cannot START
      # does: the arms beside it were measured and a manifest this arm could not apply says nothing about
      # them. These two were the last `fail` calls left inside the split-arm branch.
      k apply -f config/vllm-shared/engine-a.yaml -n "$NS_A" >/dev/null || {
        arm_refused "$label" "engine a's manifest could not be applied to $NS_A, so this arm never had two engines"; return 1; }
      k apply -f config/vllm-shared/engine-b.yaml -n "$NS_B" >/dev/null || {
        arm_refused "$label" "engine b's manifest could not be applied to $NS_B, so this arm never had two engines"; return 1; }
      # Both are diagnosed on failure, and BOTH are diagnosed when either fails.
      #
      # The engines share one card, so the one that came up is half the explanation for the one that did
      # not: what engine a reserved is what engine b did not get. Reporting only the failing Pod would leave
      # the reader with the symptom and not the arithmetic.
      # An engine that cannot start is a REGISTERED OUTCOME for its own arm, not the end of the session.
      #
      # The 2026-09-12 pilot met this: under mps every Pod came back
      # `Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu`
      # -- the plugin advertises and the kubelet will not allocate, which is the MPS engagement failure
      # reading 4c exists for. The session ended there, so the arm's refusal existed only in a log the report
      # does not read, and the R1 and shared and timeSlicing cells that had already been bought were the only
      # thing to show for it.
      #
      # Now the reason is written where `benchharness report` finds it and the matrix moves on. The
      # diagnosis still runs first: what the Pods were doing is the evidence for the refusal, not a
      # substitute for it.
      if ! k rollout status deploy/vllm-shared-a -n "$NS_A" --timeout=900s >/dev/null; then
        engine_diagnosis "$NS_A" vllm-shared-a
        arm_refused "$label" "engine a never became ready. The diagnosis in this run's log says what its Pods were doing; on one card, why one engine could not start is also why the other could not."
        return 1
      fi
      if ! k rollout status deploy/vllm-shared-b -n "$NS_B" --timeout=900s >/dev/null; then
        engine_diagnosis "$NS_B" vllm-shared-b
        engine_diagnosis "$NS_A" vllm-shared-a
        arm_refused "$label" "engine b never became ready while engine a did. On one card what a came to hold is what b did not get, so both diagnoses are in this run's log."
        return 1
      fi
      # Both engines must be on the SAME node or they are not sharing a card. max_size 1 should guarantee
      # it; checking is cheap and the failure is invisible in the numbers.
      na=$(k get pod -n "$NS_A" -l app.kubernetes.io/component=vllm-shared -o jsonpath='{.items[0].spec.nodeName}')
      nb=$(k get pod -n "$NS_B" -l app.kubernetes.io/component=vllm-shared -o jsonpath='{.items[0].spec.nodeName}')
      if [ -z "$na" ] || [ "$na" != "$nb" ]; then
        arm_refused "$label" "the two engines are on different nodes (${na:-none}, ${nb:-none}), so this arm is two engines on two cards and not a shared one. Nothing it measured would be about sharing"
        return 1
      fi
      if [ "$arm" = mps ] && ! mps_clients_connected "$NS_A" vllm-shared-a "$NS_B" vllm-shared-b; then
        return 1
      fi
      engine_kv_report "$NS_A" vllm-shared-a
      engine_kv_report "$NS_B" vllm-shared-b
      routing_record "$NS_A" vllm-shared-a
      routing_record "$NS_B" vllm-shared-b
      PREMIUM_NS="$NS_A"; STANDARD_NS="$NS_B"
      ;;
  esac

  k apply -f - >/dev/null <<EOF || fail "policies for $arm"
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5c-premium
  annotations: {platform.lkhun9311.github.io/tier: premium}
spec: {tenant: premium-1, targetNamespace: $PREMIUM_NS, gpuClass: a10g, limits: {gpuCount: 1}}
---
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata: {name: m5c-standard}
spec: {tenant: standard-noisy, targetNamespace: $STANDARD_NS, gpuClass: a10g, limits: {gpuCount: 1}}
EOF

  k create secret generic gateway-api-keys -n "$NS_A" \
    --from-literal=premium-key=premium-1 --from-literal=standard-key=standard-noisy \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k create serviceaccount gateway -n "$NS_A" --dry-run=client -o yaml | k apply -f - >/dev/null
  k create clusterrolebinding m5c-gateway-role --clusterrole=gateway-role \
    --serviceaccount="$NS_A:gateway" --dry-run=client -o yaml | k apply -f - >/dev/null
  k apply -f - >/dev/null <<EOF || fail "secret-reader role"
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: gateway-secret-reader, namespace: $NS_A}
rules: [{apiGroups: [""], resources: ["secrets"], verbs: ["get","list","watch"]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: gateway-secret-reader, namespace: $NS_A}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: gateway-secret-reader}
subjects: [{kind: ServiceAccount, name: gateway, namespace: $NS_A}]
EOF

  # Admission OFF in both arms. The matrix varies the TOPOLOGY; leaving the guard on would vary two things
  # and hand the difference to whichever one the reader already believed.
  k apply -f - >/dev/null <<EOF || fail "gateway for $arm"
apiVersion: apps/v1
kind: Deployment
metadata: {name: gateway, namespace: $NS_A}
spec:
  replicas: 1
  selector: {matchLabels: {app: m5c-gateway}}
  template:
    metadata:
      labels: {app: m5c-gateway}
      annotations: {arm: "$label"}
    spec:
      serviceAccountName: gateway
      containers:
        - name: gateway
          image: $GW_IMAGE
          args: ["-admission-mode=off"]
          env:
            - {name: GATEWAY_NAMESPACE, value: $NS_A}
            - {name: GATEWAY_API_KEY_SECRET, value: gateway-api-keys}
          ports: [{containerPort: 8080, name: http}]
          # A READINESS PROBE, because without one "rollout status" means only that the container started.
          #
          # Quoted with "" and not with backticks, which is not a style note: this heredoc is unquoted so
          # that $arm, $GW_IMAGE and $NS_A expand, and an unquoted heredoc expands backticks too. This line
          # used to run "rollout status" as a command on every gateway deploy and print "rollout: command
          # not found" to stderr four times a run. It was harmless only by luck -- the substitution landed
          # inside a YAML comment -- and it put spurious errors in the log an operator reads for real ones.
          #
          # The first draft of this very comment quoted the offending command in backticks and put the
          # defect straight back. internal/bench's TestNoUnquotedHeredocExecutesItsOwnProse caught that.
          #
          # The gateway serves /readyz and flips it only once its Kubernetes cache has synced -- it cannot
          # route before it can list the policies and deployments. Without the probe the rollout returns on a
          # process that is running and not yet listening, the port-forward accepts TCP because the kubelet
          # holds the socket, and the replay pours every request into a connection the pod refuses.
          #
          # That is not hypothetical. On 2026-09-12 the R1 cell sent 3882 requests and completed ZERO, all of
          # them errorKind=transport, and the port-forward log holds the reason: "failed to connect to
          # localhost:8080 inside namespace ... connection refused". An independent review had named this
          # exact gap the day before and it was read and not acted on.
          readinessProbe:
            httpGet: {path: /readyz, port: http}
            periodSeconds: 2
            failureThreshold: 60
EOF
  k rollout status deploy/gateway -n "$NS_A" --timeout=180s >/dev/null || fail "gateway never became ready for $arm"
}

# The load is REPORTED here, not derived here. It arrives whole from the caller and is refused if it does
# not, for the reasons written out beside that refusal.
#
# This line used to recompute DURATION_MS as 500/(RATE/2), which silently overwrote whatever was passed --
# so a caller that had derived a trace length on the card would have had it replaced by an arithmetic that
# assumes an even tenant split.
if [ -n "$LADDER" ]; then
  # RATE and NOISY_WEIGHT do not exist in ladder mode -- they are per rung, and the refusals above make sure
  # nobody passed one. Under `set -u` naming them here is not a cosmetic difference: the first ladder
  # rehearsal died on this line with "RATE: unbound variable", after building the cluster and both images.
  say "load: a ladder of $(printf '%s\n' $LADDER | wc -l | tr -d ' ') rungs, ${DURATION_MS}ms per cell, weights premium=$PREMIUM_WEIGHT probe=$PROBE_WEIGHT"
  say "      rungs: $LADDER (read under study $STUDY's registered arrival model)"
  say "run:  the two contended topologies at every rung, counterbalanced, plus one isolated baseline cell, on $PLATFORM, output $OUT"
else
  say "load: rate ${RATE}/s, ${DURATION_MS}ms per arm, weights premium=$PREMIUM_WEIGHT noisy=$NOISY_WEIGHT probe=$PROBE_WEIGHT"
  say "run:  ${REPS} repetitions of [$ARMS] on $PLATFORM, output $OUT"
fi

# Measured, like the M6 wrapper's: after the first cell, the elapsed time IS the budget, and it knows the
# node's real speed and how long the rollouts actually took rather than how long they were allowed to take.

# The cell budget, measured rather than assumed: after the first cell the elapsed time IS the budget, and it
# knows the node's real speed and how long the rollouts actually took rather than how long they were allowed
# to take.
#
# The plan the projection divides by is built much further up, before the card is touched; the comment that
# described it used to sit here, orphaned above these counters, describing a block that had moved.
cell_secs=0; cells_done=0; cell_n=0
cell_deadline_check() {
  local remain per projected floor
  # BEFORE the first cell there is no measured rate to project from -- but there is still a deadline, and
  # "no projection" is not "enough time".
  #
  # This returned 0 unconditionally, so a matrix handed an already-expired deadline started its first cell
  # anyway: it rolled out the engines, replayed, and was cut mid-cell with nothing archived. A lower bound
  # is available without any measurement at all, because no cell can finish faster than its own replay.
  if [ "$cells_done" -eq 0 ]; then
    remain=$(deadline_remaining_minutes 2>/dev/null) || return 0
    [ -n "$remain" ] || return 0
    floor=$(( (DURATION_MS + 59999) / 60000 + 1 ))
    if [ "$remain" -lt "$floor" ]; then
      echo >&2
      echo "STOPPING before the first cell: the deadline fires in ${remain} min and one cell's replay alone" >&2
      echo "  is ${floor} min. Nothing has been rolled out, so nothing is half-bought. Re-arm with a longer" >&2
      echo "  TTL_MINUTES." >&2
      return 1
    fi
    return 0
  fi
  remain=$(deadline_remaining_minutes 2>/dev/null) || return 0
  [ -n "$remain" ] || return 0
  per=$(( (cell_secs + cells_done - 1) / cells_done ))
  # A fifth of headroom, because cells differ by arm -- the sharing arms roll out two engines and the
  # exclusive arm rolls out one -- and a projection that only just fits is one slow cell from being cut.
  projected=$(( ((cells_total - cells_done) * per * 12 / 10 + 59) / 60 ))
  if [ "$projected" -ge "$remain" ]; then
    echo >&2
    echo "STOPPING: $(( cells_total - cells_done )) cells left at ~$(( per / 60 )) min each needs about ${projected} min," >&2
    echo "  and the deadline fires in ${remain} min. Being cut mid-cell would waste that cell's rollouts and" >&2
    echo "  leave a matrix missing one of the topologies it exists to compare, so it stops on a boundary." >&2
    echo "  ${cells_done} of ${cells_total} cells are complete. Re-arm with a longer TTL_MINUTES to continue." >&2
    return 1
  fi
  return 0
}


# run_cell measures ONE cell: deploy the topology, prove the tunnel, generate this cell's load, replay it,
# and hand the evidence over.
#
# It is a function rather than the inside of a loop because the ladder calls it from two places -- the rungs
# it climbs, and the single isolated-baseline cell it buys at whichever rung it stopped on. A second copy of
# this body for the second caller is the failure this repository has paid for more than once.
run_cell() {
  local arm="$1" label="$2" rep="$3" RATE_CELL="$4" NOISY_CELL="$5"
  cell_n=$(( cell_n + 1 ))
  cell_deadline_check || exit 1
  CELL_T0=$(date +%s)
  say "cell $cell_n/$cells_total: $label (rep $rep) at load $RATE_CELL:$NOISY_CELL under $ARRIVALS arrivals"
  if ! deploy_arm "$arm" "$label"; then
    say "skipping the rest of cell $cell_n: $label was refused as a registered outcome, and the arms beside it stand"
    # The time it TOOK to be refused counts too.
    #
    # cells_done went up and cell_secs did not, so the budget divided real elapsed time by a cell count
    # that included cells which contributed none of it -- and the projection for the cells still to come
    # came out low. A refusal is not free: the mps arm spent ten minutes waiting for a device count before
    # declining, which is most of a cell.
    cell_secs=$(( cell_secs + $(date +%s) - CELL_T0 ))
    cells_done=$(( cells_done + 1 ))
    continue
  fi
  # The tunnel every request of this cell goes through, replaced between cells and then PROVED.
  #
  # This was `kill $PF_PID` followed immediately by a new port-forward and `sleep 3`. kill does not wait,
  # so the new forward raced the old one's release of 18080, lost, and exited -- and because its output
  # went to /dev/null nothing said so. The replay then sent every request of that cell into a port nothing
  # was listening on and recorded them all with httpStatus 0, which the report describes as a censored
  # tail: a plumbing failure wearing a load failure's name.
  #
  # It alternated, which is the signature. Cell 1 bound cleanly, cell 2 lost the race and died, cell 3
  # found the port free because cell 2's forward was already gone, cell 4 lost it again. Two of four arms
  # produced nothing. hack/test/rehearse-m5c-matrix.sh saw R1 and timeSlicing complete every request while
  # shared and mps completed none; the paid runs never reached a second cell, so it had never been visible.
  # And the kill has to reach KUBECTL, which is why this one line does not go through `k`.
  #
  # `k` is a shell function. Backgrounding a function runs it in a SUBSHELL, and bash only replaces that
  # subshell with the command when it has no traps to run -- this script installs three. So `$!` was the
  # subshell's pid, `kill` killed the subshell, `wait` reaped it and returned, and kubectl went on living
  # as an orphan holding 18080. The fix for the race could not have worked: it was waiting on a process
  # that was never the one holding the port.
  #
  # Measured on 2026-09-12, on a rented card. R1 replayed 3,882 rows cleanly, the `shared` cell asked for
  # the same port and got `bind: address already in use`, and the session ended with one arm of four. It
  # had passed on earlier runs because the old forward usually dies on its own when its target pod is
  # replaced -- usually, which is the word that makes it a race rather than a bug that shows up.
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null
    wait "$PF_PID" 2>/dev/null
  fi
  # Then PROVE the port is free, rather than assuming the kill above was enough.
  #
  # This is the same rule the readiness probe below follows, applied to the other end: the previous check
  # here assumed its own success, and an assumption is exactly what a race defeats. If something still
  # holds the port after ten seconds, say so with the holder named -- that is a different morning from a
  # gateway that never became ready, and the old message conflated them.
  port_free=0
  for _ in $(seq 1 10); do
    if ! (exec 3<>/dev/tcp/127.0.0.1/18080) 2>/dev/null; then port_free=1; break; fi
    sleep 1
  done
  if [ "$port_free" != "1" ]; then
    say "  18080 is still held after the previous cell's forward was killed. What holds it:"
    (ss -lptn 'sport = :18080' 2>/dev/null || lsof -i :18080 2>/dev/null || echo "  (no ss or lsof to ask)") | tee -a "$LOG" >&2
    fail "port 18080 was still in use when $label rep $rep asked for it, so this cell's tunnel could not be built. The previous cell's port-forward outlived the kill that was meant to end it."
  fi
  # stderr is kept, because "why is the tunnel not up" is unanswerable without it.
  kubectl --context "$KCTX" port-forward -n "$NS_A" deploy/gateway 18080:8080 >"$OUT/port-forward-$label-$rep.log" 2>&1 &
  PF_PID=$!
  # Proved rather than slept for. A fixed sleep is a guess about a machine's speed, and the failure it
  # misses is silent.
  # Proved by asking the GATEWAY, not by opening a socket.
  #
  # A TCP connect succeeds the moment kubectl holds the local port, whether or not anything answers at the
  # other end -- so the previous check passed while the pod was still refusing connections. /readyz is the
  # gateway's own answer and is false until its cache has synced.
  pf_up=0
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:18080/readyz" 2>/dev/null; then pf_up=1; break; fi
    kill -0 "$PF_PID" 2>/dev/null || break
    sleep 1
  done
  # There is NO TCP fallback any more, and removing it is the point.
  #
  # It set pf_up=1 when a plain connect to 127.0.0.1:18080 succeeded, which proves only that kubectl holds
  # the local port -- exactly the thing defect 39 established is worthless, because the kubelet holds that
  # socket whether or not the pod behind it answers. Keeping it as a "weaker guarantee" meant a cell could
  # begin against a gateway that was not ready and record every request as transport failure, which the
  # report then describes as a censored tail. The gateway this script deploys serves /readyz and carries a
  # readinessProbe on it, so there is no build here for the fallback to be tolerant of: it could only ever
  # turn a clear refusal into eleven minutes of plumbing errors wearing a workload's name.
  [ "$pf_up" = "1" ] || fail "the gateway never answered /readyz and its port never accepted a connection for $label rep $rep. Every request of this cell would have been recorded with no HTTP status at all, and the report would have called the result a censored tail. See $OUT/port-forward-$label-$rep.log"

  # The arm is the SHARING MODE, and it is now spelled that way in the manifest.
  #
  # It used to be spelled "off" -- the admission vocabulary's name for a disabled guard -- because that was
  # the only arm name the harness would accept here, and the run then had to ship a README telling readers
  # never to run `benchharness report` over its own evidence, since pooling would collapse three topologies
  # into one row. internal/bench now carries a study whose arms ARE the topologies, so the evidence says
  # what it is and the pre-registered readings can be evaluated by the code that was written for them.
  # --model is not optional, and its absence would have been silent until the first request.
  #
  # gen-trace defaults to "llama-3-8b" and writes it into the manifest; replay sends it as the requested
  # model; internal/gateway resolves a backend by matching that name against the InferenceDeployment index
  # in the tenant's target namespace. The routing records this script writes serve Qwen2.5-3B, so every
  # request of every arm would have come back ErrNoRoute -- after both engines had loaded.
  set_load_flags "$RATE_CELL" "$NOISY_CELL"
  "$WORK/benchharness" gen-trace --seed 11 --duration-ms "$DURATION_MS" "${LOAD_FLAGS[@]}" \
    --study "$STUDY" --arm "$label" --model "$MODEL" --gateway-url "http://127.0.0.1:18080" \
    --engine-image "$ENGINE_IMAGE" --gateway-image "$GATEWAY_IMAGE_REF" --gateway-sha "$SOURCE_COMMIT" \
    --trace-out "$OUT/trace-$label-$rep.jsonl" --manifest-out "$OUT/manifest-$label-$rep.yaml" || fail "gen-trace $label"
  # --require-provenance, now that there is provenance to require.
  #
  # RunManifest has carried gatewaySHA and imageDigests since it was written and nothing ever filled them;
  # the guard that demands them has existed just as long and nothing ever asked for it. A paid run's numbers
  # belong to a build, and after the cluster is gone the manifest is the only place that association lives.
  #
  # A WAIVED run does not ask for it, because there is nothing to ask for: the waiver exists precisely for a
  # tree whose engine cannot be digest-pinned, and demanding provenance from it would be demanding that the
  # rehearsal fail. The waiver cannot reach a paid run -- hack/m5c-gpu-session.sh refuses to pass it -- so
  # the demand is on wherever it matters.
  "$WORK/benchharness" replay --manifest "$OUT/manifest-$label-$rep.yaml" \
    $PROVENANCE_FLAG \
    --target "http://127.0.0.1:18080" \
    --api-keys "premium-1=premium-key,standard-noisy=standard-key" \
    --raw-out "$OUT/raw-$label-$rep.jsonl" || fail "replay $label"
  [ -s "$OUT/raw-$label-$rep.jsonl" ] || fail "no raw evidence for $label rep $rep"
  say "  $(wc -l < "$OUT/raw-$label-$rep.jsonl") rows"
  # This cell is BOUGHT. Hand it to the caller now rather than at the end of the matrix.
  #
  # Everything this script writes goes up as one archive after the whole matrix returns, which is fine for
  # a matrix that fails -- the wrapper still archives what exists -- and worthless for an instance that
  # STOPS. A Spot interruption on the last cell takes every cell before it, and at two repetitions a run
  # is six cells and about ninety minutes of rented card.
  #
  # A hook rather than an uploader, because this script also runs on a local kind cluster in three
  # rehearsals where there is no bucket and nothing to upload to. Unset, nothing happens and the behaviour
  # is exactly what it was. A failing hook does NOT fail the cell: the evidence is already on disk, and a
  # transient S3 error is not a reason to throw away a measurement that was paid for.
  if [ -n "${CELL_DONE_HOOK:-}" ]; then
    # BOUNDED, because a hook that hangs costs card time the cell budget has already promised elsewhere.
    #
    # It cannot fail the cell -- the evidence is on local disk either way -- but without a limit a stalled
    # upload blocks every cell behind it until the hard stop, and a merely slow one inflates the next
    # cell's projection and can stop the matrix on a boundary it would otherwise have cleared.
    timeout "${CELL_DONE_HOOK_TIMEOUT:-120}" "$CELL_DONE_HOOK" "$OUT/raw-$label-$rep.jsonl" "$label" "$rep" \
      || say "  WARNING: CELL_DONE_HOOK failed or timed out for $label rep $rep; the cell is still on local disk and will go up with the rest"
  fi
  cell_secs=$(( cell_secs + $(date +%s) - CELL_T0 ))
  cells_done=$(( cells_done + 1 ))
}

for spec in "${CELLS[@]}"; do
  IFS='|' read -r cell_topology cell_label cell_rep cell_rate cell_weight cell_rung <<<"$spec"
  run_cell "$cell_topology" "$cell_label" "$cell_rep" "$cell_rate" "$cell_weight"
  [ -n "$LADDER" ] || continue
  # The stopping rule is asked of the INSTRUMENT, once both cells of a rung exist.
  #
  # The threshold it compares against lives in internal/bench beside the criterion that defines it. A copy
  # of 139.0 ms in this file would be a second place for the pre-registration's number to be edited, and the
  # pre-registration's whole claim is that the number was fixed before any rung ran.
  [ $(( cell_n % 2 )) -eq 0 ] || continue
  ladder_raws=()
  for f in "$OUT"/raw-rung*.jsonl; do [ -e "$f" ] && ladder_raws+=(--raw "$f"); done
  verdict_out="$OUT/ladder-verdict-rung$cell_rung.txt"
  # The status is captured BESIDE the call, not read back afterwards.
  #
  # This was `if benchharness ladder-verdict ...; then ... fi` followed by `verdict_code=$?`, and $? after an
  # `if` is the IF STATEMENT's status -- which is 0 when the condition failed and no else ran. So a verdict
  # that correctly said STOP and exited 10 reached this script as 0 and fell through to the refusal below,
  # which reported the rung unscorable and ended a paid session at its first stopping point. The verdict file
  # said "LADDER: STOP" the whole time; nothing read it.
  #
  # The rehearsal could not have caught it: a stub answers in milliseconds, so on a free cluster every rung
  # CONTINUEs and this branch never runs. The exit code was pinned in hack/test/check-ladder-refusals.sh --
  # of the COMMAND, not of this script's reading of it. That gap is now closed there too.
  verdict_code=0
  "$WORK/benchharness" ladder-verdict "${ladder_raws[@]}" --rung "$cell_rung" >"$verdict_out" 2>>"$LOG" || verdict_code=$?
  if [ "$verdict_code" -eq 0 ]; then
    grep -qx "LADDER: CONTINUE" "$verdict_out" \
      || fail "ladder-verdict exited 0 for rung $cell_rung without saying CONTINUE. Its exit code and its line disagree, and this script will not guess which one meant to climb. See $verdict_out"
    say "rung $cell_rung: at least one topology still meets the target, so the ladder climbs"
    continue
  fi
  if [ "$verdict_code" -eq 10 ]; then
    grep -qx "LADDER: STOP" "$verdict_out" \
      || fail "ladder-verdict exited 10 for rung $cell_rung without saying STOP. See $verdict_out"
    say "rung $cell_rung: both topologies breached the target, which is the registered stopping point"
    LADDER_STOPPED_AT="$cell_rung"
    # The budget has to learn that the rungs above this one will not be bought.
    #
    # cells_total still counted every planned cell, so after a STOP at rung 1 of a four-rung ladder the
    # deadline projection asked for seven more cells when only the baseline remained -- and would refuse to
    # buy it with twenty minutes in hand. The stopping rule cancelled the purchases; nothing cancelled the
    # estimate of them.
    cells_total=$(( cells_done + 1 ))
    break
  fi
  fail "ladder-verdict could not score rung $cell_rung (exit $verdict_code). The cells it refused are named in $verdict_out and in this run's log; a rung that cannot be scored is not a rung that breached, and climbing past it would build the bracket on it."
done

# The isolated baseline, bought once, at the rung the ladder stopped on.
#
# Without it "the split ran out of capacity" and "one engine of this model on this card ran out of capacity"
# are the same observation. It is one cell because the criterion is an absolute millisecond figure fixed
# before the run, which is what makes a per-rung baseline unnecessary -- see the pre-registration.
if [ -n "$LADDER" ]; then
  # The fallback is the highest rung that HAS CELLS, not the last position in the list.
  #
  # $ladder_rung counts every entry including `skip`, so a ladder ending in a skip made ladder_top name a
  # rung with no cells: the loop below matched nothing, bought no baseline, and the `say` line above it
  # announced a purchase that did not happen. Nothing downstream said so until the session's final check,
  # which is a long way from the decision.
  ladder_planned_top=0
  for spec in "${CELLS[@]}"; do
    IFS='|' read -r _ _ _ _ _ cell_rung <<<"$spec"
    [ "$cell_rung" -le "$ladder_planned_top" ] || ladder_planned_top=$cell_rung
  done
  ladder_top="${LADDER_STOPPED_AT:-$ladder_planned_top}"
  say "buying the isolated baseline at rung $ladder_top, the rung this ladder ended on"
  ladder_baseline_bought=0
  for spec in "${CELLS[@]}"; do
    IFS='|' read -r cell_topology cell_label cell_rep cell_rate cell_weight cell_rung <<<"$spec"
    [ "$cell_rung" = "$ladder_top" ] || continue
    run_cell R1 "$(printf 'rung%02d' "$ladder_top")-R1" 1 "$cell_rate" "$cell_weight"
    ladder_baseline_bought=1
    break
  done
  # A loop that can find nothing must say so. Without this the run ends looking complete and the readings
  # lose the cell that tells a topology's limit apart from this model's on this card.
  [ "$ladder_baseline_bought" = 1 ] \
    || fail "the ladder ended at rung $ladder_top and no cell of that rung is in the plan, so its isolated baseline was never bought. Without it, 'the split ran out of capacity' and 'one engine ran out of capacity' are the same observation"
fi


# The raw files are NOT report inputs, and saying so here is cheaper than the confusion. Both arms replay
# with --arm off, because the harness's arm vocabulary is the ADMISSION arms and the sharing mode is this
# run's variable; feeding both files to `benchharness report` would pool them into one arm and produce a
# number for a comparison that was never made.
cat > "$OUT/README.txt" <<EOF
Raw evidence from the M5-c sharing matrix.

The variable is the SHARING MODE, and every row now carries it as its arm: R1, shared, timeSlicing or mps,
under study sharing-matrix-2026-09-10. Admission was off in every arm, which is a constant of this run
rather than its variable, so it is not what the arm field names.

  for f in $OUT/raw-*.jsonl; do args="\$args --raw \$f"; done; benchharness report \$args

The study is read from the evidence rows themselves, not passed as a flag, so the arm names above are what
select the readings. That evaluates the pre-registered readings from
docs/superpowers/specs/2026-09-10-does-splitting-the-card-buy-protection.md, in the registered order, and
prints the first that fires. Do not evaluate them by hand: a run whose readings are read off a table by a
person is not the instrument the pre-registration describes.

This file used to say the opposite -- that these files must never be passed to \`benchharness report\` --
because every arm was replayed as "off" and pooling would have collapsed three topologies into one row.
EOF

say "MATRIX DONE. Raw evidence in $OUT (see its README.txt before analysing)."
say "The comparison is premium TTFT p99 across the sharing modes at equal offered load."
say "Per-engine GPU utilisation is deliberately absent: under time-slicing nothing can attribute it."
