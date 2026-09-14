#!/usr/bin/env bash
#
# Checks that the device plotter draws what it was given and refuses what it was not.
#
# The refusals are the part worth testing. A plotter that emits an empty chart for an empty capture produces
# a picture of a card that did nothing, which is indistinguishable from a picture of a card nobody watched --
# and this session exists precisely to stop reservation being read as use.
set -euo pipefail

PLOT="${PLOT:-hack/plot-device-observation.py}"
WORK=$(mktemp -d /tmp/plot-test-XXXXXX)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

# A refusal is checked by its MESSAGE, not only by its exit code.
#
# Making the plotter skip its own guard did not make this suite fail: without the guard it reached the
# drawing code, threw on an empty sequence, and exited nonzero anyway -- so a check that only reads the exit
# code called a traceback a refusal. They are not the same thing. One tells the reader that the capture is
# empty; the other tells them the tool is broken, and only the first is a result.
check() {
  local name=$1 want=$2; shift 2
  local got=0
  "$@" >"$WORK/out.txt" 2>&1 || got=$?
  if [ "$got" != "$want" ]; then
    printf '  FAIL  %s (exit %s, wanted %s)\n' "$name" "$got" "$want"
    sed 's/^/        /' "$WORK/out.txt" | head -3
    fail=$((fail + 1))
    return
  fi
  if [ "$want" != "0" ]; then
    if ! grep -q '^FAIL: ' "$WORK/out.txt"; then
      printf '  FAIL  %s exited %s without saying why -- that is a crash, not a refusal\n' "$name" "$got"
      sed 's/^/        /' "$WORK/out.txt" | tail -3
      fail=$((fail + 1))
      return
    fi
    if grep -q 'Traceback (most recent call last)' "$WORK/out.txt"; then
      printf '  FAIL  %s refused with a traceback attached\n' "$name"
      fail=$((fail + 1))
      return
    fi
  fi
  printf '  ok    %s\n' "$name"; pass=$((pass + 1))
}

# A capture in the exposition shape the exporter actually emits, including one card with no pod label --
# which is the shape that matters, because an unattributed sample must not silently become an attributed one.
{
  for i in $(seq 0 9); do
    ts=$((1788000000 + i * 2))
    printf '%s\tDCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-aaaa",device="nvidia0",modelName="NVIDIA A10G",Hostname="w1",container="trainer",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} %s\n' "$ts" "$((80 + i))"
    printf '%s\tDCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-bbbb",device="nvidia1",modelName="NVIDIA A10G",Hostname="w1"} 0\n' "$ts"
  done
} > "$WORK/good.tsv"

printf '1788000000\tSCRAPE_FAILED\n1788000002\tSCRAPE_FAILED\n' > "$WORK/allfailed.tsv"
printf 'not an exposition line at all\n' > "$WORK/garbage.tsv"
: > "$WORK/empty.tsv"

check "a real capture is drawn"            0 python3 "$PLOT" "$WORK/good.tsv"      -o "$WORK/good.svg"
check "a missing file is refused"          1 python3 "$PLOT" "$WORK/nowhere.tsv"   -o "$WORK/x.svg"
check "an empty file is refused"           1 python3 "$PLOT" "$WORK/empty.tsv"     -o "$WORK/x.svg"
check "a capture of failed scrapes is refused" 1 python3 "$PLOT" "$WORK/allfailed.tsv" -o "$WORK/x.svg"
check "unreadable content is refused"      1 python3 "$PLOT" "$WORK/garbage.tsv"   -o "$WORK/x.svg"

# The drawing has to carry the attribution, not just the numbers: a chart that loses which Pod held the card
# answers a different question from the one the session asked.
if grep -q 'a2-borrow-x7k2p' "$WORK/good.svg"; then
  printf '  ok    the owning pod is named in the drawing\n'; pass=$((pass + 1))
else
  printf '  FAIL  the drawing does not name the pod that held the card\n'; fail=$((fail + 1))
fi
if grep -q 'no pod named on any sample' "$WORK/good.svg"; then
  printf '  ok    an unattributed card says so rather than looking owned\n'; pass=$((pass + 1))
else
  printf '  FAIL  the card with no pod label is not marked unattributed\n'; fail=$((fail + 1))
fi

printf 'plot-device-observation: %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
