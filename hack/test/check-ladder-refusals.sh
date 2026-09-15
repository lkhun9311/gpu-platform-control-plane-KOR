#!/usr/bin/env bash
#
# Pins the capacity ladder's refusals and its stopping rule, with no cluster and no card.
#
# WHY THIS EXISTS
#
# hack/test/rehearse-m5c-matrix.sh drives the ladder down the path a stub can reach: every rung meets the
# target, so the ladder climbs, buys its baseline and answers L6. Two things it CANNOT reach are the two a
# paid run will:
#
#   the STOP path        no stub is ever slow enough to breach a 139 ms target, so the rung where both
#                        topologies breach -- the one the whole ladder is climbing to find -- never happens
#                        on a free cluster.
#   the INVALID path     a cell the evaluator cannot score must stop the ladder for a different reason than
#                        a breach does, and a runner that confused the two would either climb past a hole or
#                        report a bracket it never established.
#
# Both are decided in internal/bench and both are reachable from a file, so they are pinned here from the
# ninth pilot's own rows rather than left to the day a card is paying.
#
# WHAT IT SUBSTITUTES, AND WHY THAT IS HONEST
#
# The fixtures are real paid rows with their first-token timestamps rewritten, which makes them a TEST
# INPUT and not a measurement. Nothing here reports a latency. What is being checked is which branch the
# instrument takes for a given tail, and a tail that was manufactured is the only way to ask that question
# without buying one.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
failures=0
say() { echo "== $*"; }
ok()  { echo "   ok: $*"; }
bad() { echo "   FAIL: $*" >&2; failures=$(( failures + 1 )); }

# The paid rows are gitignored, so the verdict checks that need them can be absent. THE SHELL CHECKS ARE
# NOT ALLOWED TO BE.
#
# This script used to exit 0 the moment that file was missing -- which on a fresh clone is always -- so
# deleting the production refusals it exists to pin left it green. A suite whose coverage depends on a
# gitignored artefact is a suite that silently stops covering anything.
SRC_ROWS=hack/m5c-20260913-011031/m5c-run/raw-shared-1.jsonl
HAVE_ROWS=1
[ -s "$SRC_ROWS" ] || HAVE_ROWS=0

go build -o "$WORK/benchharness" ./cmd/benchharness

# fixture <out> <arm> <divisor> rewrites every premium first-token wait to 1/divisor of what it was.
#
# Dividing rather than assigning keeps the SHAPE of the distribution, so the p99 the evaluator computes is
# still a p99 over 9,310 different numbers and not over one repeated.
fixture() {
  python3 - "$SRC_ROWS" "$1" "$2" "$3" <<'PY'
import json,sys
src,out,arm,div=sys.argv[1],sys.argv[2],sys.argv[3],float(sys.argv[4])
with open(out,"w") as w:
    for line in open(src):
        r=json.loads(line)
        r["arm"]=arm
        r["study"]="throughput-ladder-2026-09-13"
        ft,st=r.get("firstTokenUnixNanos"),r.get("sendUnixNanos")
        if ft and st and not r.get("isNoisy"):
            r["firstTokenUnixNanos"]=int(st+(ft-st)/div)
        w.write(json.dumps(r)+"\n")
PY
}

if [ "$HAVE_ROWS" = 0 ]; then
  say "1-3. SKIPPED: $SRC_ROWS is not in this tree, so the verdict scenarios have no rows to build from."
  say "     TestLadder* in internal/bench covers the same decisions without evidence. The shell checks below still run."
fi
if [ "$HAVE_ROWS" = 1 ]; then
say "1. both topologies breach -- does the verdict say STOP, and with the code the runner acts on?"
fixture "$WORK/raw-rung01-shared-1.jsonl"      rung01-shared      1
fixture "$WORK/raw-rung01-timeSlicing-1.jsonl" rung01-timeSlicing 1
set +e
out=$("$WORK/benchharness" ladder-verdict --raw "$WORK/raw-rung01-shared-1.jsonl" --raw "$WORK/raw-rung01-timeSlicing-1.jsonl" --rung 1 2>"$WORK/err1")
code=$?
set -e
[ "$code" = "10" ] && ok "exit 10, which is the stop the runner breaks on" || bad "exit $code, want 10 -- the runner would have climbed past the bracket"
[ "$out" = "LADDER: STOP" ] && ok "and it says STOP" || bad "it printed ${out@Q}"
grep -q "registered stopping point" "$WORK/err1" && ok "and names the rule it applied" || bad "the reason does not name the registered rule: $(cat "$WORK/err1")"

say "2. one topology still meets the target -- does it CONTINUE?"
fixture "$WORK/raw-rung02-shared-1.jsonl"      rung02-shared      1
fixture "$WORK/raw-rung02-timeSlicing-1.jsonl" rung02-timeSlicing 40
set +e
out=$("$WORK/benchharness" ladder-verdict --raw "$WORK/raw-rung02-shared-1.jsonl" --raw "$WORK/raw-rung02-timeSlicing-1.jsonl" --rung 2 2>"$WORK/err2")
code=$?
set -e
[ "$code" = "0" ] && ok "exit 0" || bad "exit $code, want 0 -- a bracket that is still open was reported closed"
[ "$out" = "LADDER: CONTINUE" ] && ok "and it says CONTINUE" || bad "it printed ${out@Q}"

say "3. a rung with one cell -- refused, and NOT mistaken for a breach?"
set +e
out=$("$WORK/benchharness" ladder-verdict --raw "$WORK/raw-rung01-shared-1.jsonl" --rung 1 2>"$WORK/err3")
code=$?
set -e
[ "$code" = "1" ] && ok "exit 1, which the runner tells apart from the stop's 10" || bad "exit $code, want 1"
[ "$out" = "LADDER: INVALID" ] && ok "and it says INVALID rather than STOP" || bad "it printed ${out@Q}"
grep -q "no timeSlicing cell" "$WORK/err3" && ok "and names the missing cell" || bad "the refusal does not name what is missing: $(cat "$WORK/err3")"

fi

say "4. does the runner refuse a load described twice?"
check_refusal() {
  local what="$1" want="$2"; shift 2
  set +e
  out=$(env "$@" PLATFORM=kind LADDER="1:1" bash hack/m5c-matrix.sh 2>&1)
  code=$?
  set -e
  [ "$code" != "0" ] || { bad "$what was accepted"; return; }
  printf '%s' "$out" | grep -q "$want" && ok "$what is refused, naming $want" || bad "$what: the refusal does not say why: $(printf '%s' "$out" | head -3)"
}
check_refusal "LADDER beside RATE"         "RATE and LADDER are both set"         RATE=9
check_refusal "LADDER beside REPS"         "REPS and LADDER are both set"         REPS=2
check_refusal "LADDER beside ARMS"         "ARMS and LADDER are both set"         ARMS=R1
check_refusal "LADDER beside NOISY_WEIGHT" "NOISY_WEIGHT and LADDER are both set" NOISY_WEIGHT=0.02

say "5. does the SESSION WRAPPER refuse a load described twice, before it rents anything?"
check_session_refusal() {
  local what="$1" want="$2"; shift 2
  set +e
  out=$(env "$@" LADDER="1:1" OUT="$WORK/sess" bash hack/m5c-gpu-session.sh 2>&1)
  code=$?
  set -e
  [ "$code" != "0" ] || { bad "$what was accepted by the session wrapper"; return; }
  printf '%s' "$out" | grep -q "$want" && ok "$what is refused before anything is rented" || bad "$what: $(printf '%s' "$out" | head -3)"
}
# The wrapper carries its own copy of these refusals on purpose: it EXPORTS into the matrix, so when the two
# disagree the wrapper wins and the matrix's refusal is dead text. A run that reached the instance with both
# a ladder and a single load would be refused on the card, after the bring-up was paid for.
check_session_refusal "LADDER beside RATE"         "RATE and LADDER are both set"         RATE=9
check_session_refusal "LADDER beside REPS"         "REPS and LADDER are both set"         REPS=2
check_session_refusal "LADDER beside ARMS"         "ARMS and LADDER are both set"         ARMS=R1
check_session_refusal "LADDER beside NOISY_WEIGHT" "NOISY_WEIGHT and LADDER are both set" NOISY_WEIGHT=0.02

say "6. does a skipped rung hold its position rather than renumbering the ones below it?"
# The whole point of `skip`: a repetition of rungs 2 and 3 must write rung02-* and rung03-*. If skip merely
# dropped the entry, those cells would be named rung01-* and rung02-* and would pool with the first run's
# cells at completely different offered loads.
# The plan is printed before the card is touched, so this needs no cluster and no money.
out=$(env PLATFORM=kind LADDER="skip 2:0.1 3:0.05" PREMIUM_WEIGHT=1 PROBE_WEIGHT=0 DURATION_MS=1000 \
      OUT="$WORK/skiprun" KCTX=no-such-context bash hack/m5c-matrix.sh 2>&1 || true)
plan=$(printf '%s' "$out" | grep '^== plan:' || true)
[ -n "$plan" ] || bad "the runner did not print a plan before acquiring a card"
printf '%s' "$plan" | grep -q "rung01-" \
  && bad "a skipped rung 1 still produced rung01 cells, which would pool with the previous run's 1.16 req/s under that name: $plan"
printf '%s' "$plan" | grep -q "rung02-shared" && printf '%s' "$plan" | grep -q "rung03-shared" \
  && ok "the plan is rung02 and rung03, so skip held the positions" \
  || bad "the plan does not carry rung02 and rung03: $plan"
printf '%s' "$plan" | grep -q "5 cell" \
  && ok "and it is 5 cells: two rungs times two topologies plus the baseline" \
  || bad "the plan is not five cells: $plan"

say "7. does the session wrapper expect only the arms a skip-led ladder was told to buy?"
# The wrapper's end-of-session check is a different copy of "what should exist" from the runner's plan, and
# the two disagreed: the repetition of rungs 2 and 3 bought and downloaded every cell, then failed demanding
# rung01-shared and rung01-timeSlicing. The evidence was fine; the session's last word was not.
# THE WRAPPER'S OWN LINES ARE EXTRACTED AND EXECUTED, not retyped here.
#
# The first version of this check reconstructed the loop in this file and asserted on the reconstruction,
# then grepped the real script for the guard. Commenting the guard out in the real script left both green:
# the private copy stayed correct and grep found the disabled line inside the comment. A test that passes
# because it is testing itself is the defect class this whole suite exists for.
extracted=$(sed -n '/^  _rung=0$/,/^  done$/p' hack/m5c-gpu-session.sh)
[ -n "$extracted" ] || bad "could not find the wrapper's expected-arms loop to execute"
expected=$(LADDER="skip 2:0.1 3:0.05" bash -c "expected_arms=\"\"
$extracted
printf '%s' \"\$expected_arms\"")
printf '%s' "$expected" | grep -q rung01 \
  && bad "the wrapper's own loop still expects rung01 arms from a ladder whose first rung is skip: $expected" \
  || ok "the wrapper's own loop expects only rung02 and rung03:$expected"
printf '%s' "$expected" | grep -q rung02-shared && printf '%s' "$expected" | grep -q rung03-timeSlicing \
  && ok "and it names both topologies of both bought rungs" \
  || bad "the wrapper's loop does not name both rungs' topologies: $expected"

say "8. does the local plan check refuse, before launch, what used to be refused on the card?"
# Each of these already had a guard. Each guard fired on the rented instance: replay validates the arm after
# the engines are up, and the cell floors are applied after the replay finishes. The point of the plan check
# is not new refusals, it is the same ones at $0.
#
# TMPDIR is pointed at a directory of its own so the leftover check after the cases can see what the matrix
# left behind, and so a leak lands in this script's WORK rather than in /tmp.
mkdir -p "$WORK/plan-tmp"
#
# plan_case <what> <want> <ladder> <duration-ms> [study] [VAR=value ...]. The study defaults to the down
# ladder, and trailing assignments override the load the case passes to the matrix.
plan_case() {
  local what="$1" want="$2" ladder="$3" dur="$4" study="${5:-throughput-ladder-down-2026-09-13}"
  shift $(( $# < 5 ? $# : 5 ))
  set +e
  out=$(env TMPDIR="$WORK/plan-tmp" PLAN_ONLY=1 PLATFORM=kind KCTX=none BENCHHARNESS_BIN="$WORK/benchharness" \
        LADDER="$ladder" LADDER_STUDY="$study" \
        PREMIUM_WEIGHT=1 PROBE_WEIGHT=0 DURATION_MS="$dur" OUT="$WORK/plan-$RANDOM" "$@" \
        bash hack/m5c-matrix.sh 2>&1)
  local code=$?
  set -e
  if [ "$want" = ok ]; then
    [ "$code" = 0 ] && ok "$what is accepted" || bad "$what was refused: $(printf '%s' "$out" | grep 'PLAN REFUSED' | head -1)"
    return
  fi
  [ "$code" != 0 ] || { bad "$what was accepted and would have been refused on the card"; return; }
  printf '%s' "$out" | grep -q "$want" && ok "$what is refused before launch" || bad "$what: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
}
plan_case "the registered downward ladder" ok "1.431979:0.23386882 2.564749:0.11403366 4.847585:0.05746316" 505000
plan_case "a trace too short for the sample floor" "below 500 completed" "1.431979:0.23386882" 420000
plan_case "a rung the registry does not admit"     "is not one of study"  "skip skip skip skip 4.847585:0.05746316" 505000
plan_case "a contender the rungs do not hold fixed" "varied two things at once" "2.564749:0.5" 505000
# The independent-arrivals ladder: the same premium rungs, and ONE contender rate that holds 139 offers at every
# rung, because the contender's schedule no longer depends on the premium rate at all.
IND=throughput-ladder-independent-2026-09-15
plan_case "the independent-arrivals ladder" ok "1.16:0.289 2.31:0.289 4.61:0.289" 505000 "$IND"
# The numbers alone cannot say which model they were solved for, so the contender count is what catches this.
plan_case "a weighted ladder's entries read as rates" "varied two things at once" "1.431979:0.23386882" 505000 "$IND"
plan_case "a probe weight under independent arrivals" "registered independent arrivals" "1.16:0.289" 505000 "$IND" PROBE_WEIGHT=0.1
# Every case above leaves the matrix from inside PLAN_ONLY, which exits before the full cleanup trap is armed.
#
# Each work directory holds a copy of the 34 MB benchharness, and /tmp is tmpfs here, so an unremoved one is
# memory. 18 GB of them had accumulated by 2026-09-15.
leftover=$(find "$WORK/plan-tmp" -mindepth 1 -maxdepth 1 | wc -l)
[ "$leftover" = 0 ] && ok "the plan checks left no work directory behind" \
  || bad "the plan checks left $leftover work director(ies) behind; each holds a benchharness copy, and /tmp is tmpfs"

say "9. does the manifest the matrix now writes satisfy --require-provenance, and one without it fail?"
# THE ARTEFACT, not the wiring.
#
# Every GPU-free path has to waive the engine pin -- a stub engine built locally has no registry digest a
# kubelet can resolve -- so no rehearsal can drive `replay --require-provenance` down its accepting path.
# What CAN be checked here is the thing the matrix produces: a manifest carrying the engine digest, the
# gateway image id and the commit. If that satisfies the guard, the only untested link is the flag itself,
# and that is named as a limitation rather than assumed away.
ENGINE_REF="vllm/vllm-openai@sha256:0a51ea5b4ae2dc5d81890e5173f54203d2a3ae0cfffe51b8fd2afd4391bfd967"
GW_REF="gateway:m5c@sha256:$(printf 'a%.0s' $(seq 64))"
"$WORK/benchharness" gen-trace --seed 11 --duration-ms 505000 --rate 1.431979   --study throughput-ladder-down-2026-09-13 --arm rung01-shared --model m   --premium-weight 1 --noisy-weight 0.23386882 --probe-weight 0   --engine-image "$ENGINE_REF" --gateway-image "$GW_REF" --gateway-sha deadbeef   --trace-out "$WORK/prov.jsonl" --manifest-out "$WORK/prov.yaml" >/dev/null 2>&1   || bad "gen-trace could not build a manifest carrying provenance"
set +e
out=$("$WORK/benchharness" replay --manifest "$WORK/prov.yaml" --require-provenance --target http://127.0.0.1:1       --api-keys "premium-1=k,standard-noisy=k" --raw-out "$WORK/prov-raw.jsonl" 2>&1)
set -e
printf '%s' "$out" | grep -qE "provenance|imageDigests|gatewaySHA"   && bad "the matrix's own manifest was refused by the provenance guard: $(printf '%s' "$out" | head -1)"   || ok "the manifest the matrix writes satisfies --require-provenance"

"$WORK/benchharness" gen-trace --seed 11 --duration-ms 505000 --rate 1.431979   --study throughput-ladder-down-2026-09-13 --arm rung01-shared --model m   --premium-weight 1 --noisy-weight 0.23386882 --probe-weight 0   --trace-out "$WORK/bare.jsonl" --manifest-out "$WORK/bare.yaml" >/dev/null 2>&1
set +e
out=$("$WORK/benchharness" replay --manifest "$WORK/bare.yaml" --require-provenance --target http://127.0.0.1:1       --api-keys "premium-1=k,standard-noisy=k" --raw-out "$WORK/bare-raw.jsonl" 2>&1)
code=$?
set -e
[ "$code" != 0 ] && printf '%s' "$out" | grep -q "gatewaySHA"   && ok "and a manifest without it is refused, naming what is missing"   || bad "a manifest carrying no provenance was accepted by --require-provenance"

echo
if [ "$failures" = "0" ]; then
  say "LADDER REFUSALS PINNED: the stop, the continue, the unscorable rung, and the double-described load in BOTH scripts."
else
  echo "FAILED: $failures assertion(s) above." >&2
  exit 1
fi
