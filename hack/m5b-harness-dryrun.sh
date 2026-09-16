#!/usr/bin/env bash
#
# M5-b harness dry run: exercise the whole gen -> replay -> report path with no GPU and no cluster.
#
# It builds benchharness, starts a stub streaming backend, generates a trace and manifest per arm,
# replays each arm against the stub recording raw evidence, and renders the pre-registered report.
#
# This proves the harness plumbing so the paid GPU run only swaps the stub for a real gateway + vLLM.
set -uo pipefail

cd "$(dirname "$0")/.."
export GOTOOLCHAIN=go1.26.0

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null' EXIT

echo "== build benchharness =="
go build -o "$WORK/benchharness" ./cmd/benchharness || exit 1

echo "== start stub backend =="
"$WORK/benchharness" stub-serve --addr 127.0.0.1:8091 --tokens 8 --ttft-ms 3 --itl-ms 1 &
STUB_PID=$!
sleep 1

fail() { echo "DRY RUN FAILED: $*"; exit 1; }

for arm in R1 off static-cap kv-aware; do
  echo "== arm $arm: gen-trace + replay =="
  "$WORK/benchharness" gen-trace \
    --seed 7 --duration-ms 3000 --rate 40 \
    --arm "$arm" --gateway-url "http://127.0.0.1:8091" \
    --trace-out "$WORK/trace-$arm.jsonl" --manifest-out "$WORK/manifest-$arm.yaml" || fail "gen-trace $arm"
  "$WORK/benchharness" replay \
    --manifest "$WORK/manifest-$arm.yaml" \
    --target "http://127.0.0.1:8091" \
    --api-keys "premium-1=premium-key,standard-noisy=standard-key" \
    --raw-out "$WORK/raw-$arm.jsonl" || fail "replay $arm"
  [ -s "$WORK/raw-$arm.jsonl" ] || fail "no raw evidence for $arm"
done

echo "== render report =="
"$WORK/benchharness" report \
  --raw "$WORK/raw-R1.jsonl" \
  --raw "$WORK/raw-off.jsonl" \
  --raw "$WORK/raw-static-cap.jsonl" \
  --raw "$WORK/raw-kv-aware.jsonl" \
  --out "$WORK/report.txt" || fail "report"

echo
cat "$WORK/report.txt"
echo
grep -q "M5-b benchmark report" "$WORK/report.txt" || fail "report missing header"
grep -q "VERDICT:" "$WORK/report.txt" || fail "report missing verdict"

echo "DRY RUN OK: gen -> replay (4 arms) -> report completed with no GPU and no cluster."
