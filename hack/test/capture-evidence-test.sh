#!/usr/bin/env bash
#
# Checks that the capture step keeps what was measured and admits what it could not draw.
#
# The interesting cases are the negative ones. A capture directory that quietly omits a run, or that leaves
# an old drawing in place when a new series could not be drawn, is worse than having no captures: it looks
# complete. Each case below is run against a repository laid out in a temporary directory, so the real
# docs/captures is never touched.
set -uo pipefail

SCRIPT="${SCRIPT:-$PWD/hack/capture-evidence.sh}"
PLOT="${PLOT:-$PWD/hack/plot-device-observation.py}"
WORK=$(mktemp -d /tmp/capture-test-XXXXXX)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()   { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }

# A repository shape with four runs: one that measured a series, one that only saw cards, one that wrote
# records, and one that produced nothing at all.
mkdir -p "$WORK/hack"
cd "$WORK"

mkdir -p hack/qlgpu-withseries
{
  for i in 0 1 2 3 4; do
    printf '%s\tDCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-a",Hostname="w1",namespace="ql",pod="a2-borrow"} %s\n' \
      "$((1788000000 + i * 2))" "$((70 + i))"
  done
} > hack/qlgpu-withseries/device-util.tsv
echo "i-0abc" > hack/qlgpu-withseries/instance-id

mkdir -p hack/qlgpu-cardsonly
printf 'index, name, memory.total [MiB]\n0, NVIDIA A10G, 23028 MiB\n' > hack/qlgpu-cardsonly/preflight-nvidia-smi.csv

mkdir -p hack/m5b-run-records
echo '{"arm":"kv-aware"}' > hack/m5b-run-records/raw-kv-aware-1.jsonl

# The price-of-protection runner unpacks its rows into evidence/ rather than leaving them at the top level,
# so a run's most expensive files can sit one directory deeper than every earlier study put them.
mkdir -p hack/pop-nested/evidence
echo '{"arm":"default-fcfs"}' > hack/pop-nested/evidence/raw-default-fcfs-1.jsonl
echo '{"arm":"mbt-0512-priority"}' > hack/pop-nested/evidence/raw-mbt-0512-priority-1.jsonl
echo '{"study":"price-of-protection"}' > hack/pop-nested/run.json

mkdir -p hack/qlgpu-nothing
echo '#!/bin/bash' > hack/qlgpu-nothing/user-data.sh

mkdir -p hack/qlgpu-unreadable
printf '1788000000\tSCRAPE_FAILED\n' > hack/qlgpu-unreadable/device-util.tsv

# A drawing left over from when this run's series WAS readable.
#
# Without it the "no drawing left in place" check below is vacuous: the fixture never had a device.svg, so
# the assertion passed whether or not the capture removes one. Confirmed by deleting the removal line in
# hack/capture-evidence.sh -- the suite stayed green. The stale drawing is the entire case that line exists
# for, because the capture never clears the destination directory and its up-to-date check keys on
# numbers.md alone. numbers.md is dated into the past so the capture does not skip the run as up to date.
mkdir -p captures/qlgpu-unreadable
printf 'stale drawing from a run whose series used to parse\n' > captures/qlgpu-unreadable/device.svg
printf '# qlgpu-unreadable\n' > captures/qlgpu-unreadable/numbers.md
touch -d '2000-01-01' captures/qlgpu-unreadable/numbers.md captures/qlgpu-unreadable/device.svg

run() { PLOT="$PLOT" EVIDENCE="$WORK/captures" REPO="$WORK" bash "$SCRIPT" "$@" 2>"$WORK/err.txt"; }

run > "$WORK/out.txt"; rc=$?

# 1. A run that measured nothing is left alone, rather than given an empty capture.
[ -d "$WORK/captures/qlgpu-nothing" ] && bad "a run that measured nothing was captured anyway" \
  || ok "a run that measured nothing is skipped"

# 2. Each kind of measurement is kept.
[ -s "$WORK/captures/qlgpu-withseries/numbers.md" ] && ok "a series run is captured" \
  || bad "a series run produced no numbers.md"
[ -s "$WORK/captures/qlgpu-cardsonly/numbers.md" ] && ok "a run that only saw cards is captured" \
  || bad "a run with nvidia-smi output was skipped"
[ -s "$WORK/captures/m5b-run-records/numbers.md" ] && ok "a .jsonl record run is captured" \
  || bad "a run whose records are .jsonl was skipped -- this omitted every M5-b measurement once already"

# Counting them, not just capturing the run: the pilot WAS captured, and its capture said "1 record(s)"
# because it found run.json and none of the four raw files underneath evidence/.
if grep -q 'raw-default-fcfs-1.jsonl' "$WORK/captures/pop-nested/numbers.md" 2>/dev/null \
   && grep -q 'raw-mbt-0512-priority-1.jsonl' "$WORK/captures/pop-nested/numbers.md" 2>/dev/null; then
  ok "rows under evidence/ are captured, not just the run.json beside them"
else
  bad "a run whose rows live in evidence/ was captured without them -- this is the 2026-09-07 pilot's capture"
fi

# 3. The drawing happens, and carries the attribution.
if [ -s "$WORK/captures/qlgpu-withseries/device.svg" ] \
   && grep -q 'a2-borrow' "$WORK/captures/qlgpu-withseries/device.svg"; then
  ok "the series is drawn and names the pod that held the card"
else
  bad "the series was not drawn, or the drawing lost the pod label"
fi

# 4. A series that cannot be read is admitted, not silently dropped, and never leaves a drawing behind.
if [ -s "$WORK/captures/qlgpu-unreadable/device-NOT-DRAWN.txt" ] \
   && [ ! -e "$WORK/captures/qlgpu-unreadable/device.svg" ]; then
  ok "an undrawable series is recorded as undrawable, with no drawing left in place"
else
  bad "an undrawable series left no explanation, or left a drawing"
fi
grep -q 'not drawable' "$WORK/captures/INDEX.md" \
  && ok "the index says which capture could not be drawn" \
  || bad "the index does not distinguish a drawn capture from an undrawable one"
[ "$rc" -ne 0 ] && ok "a capture with problems exits nonzero" \
  || bad "a capture that could not draw a series still exited zero"
grep -q 'could not draw' "$WORK/err.txt" \
  && ok "the problem is reported on stderr" \
  || bad "nothing was written to stderr about the failure"

# 5. The index is rewritten from disk, not appended to.
#
# The first version of this check deleted only the capture and expected it to vanish from the index. It came
# back, correctly: the run directory was still there, so the capture was simply remade. What the index must
# not do is keep a line for something that is gone, so both the capture and the run it came from go.
rm -rf "$WORK/captures/qlgpu-cardsonly" hack/qlgpu-cardsonly
touch hack/qlgpu-withseries/device-util.tsv
run > /dev/null
grep -q 'qlgpu-cardsonly' "$WORK/captures/INDEX.md" \
  && bad "the index still lists a capture that is no longer on disk" \
  || ok "the index is rebuilt from disk rather than appended to"

# And a capture whose run is gone is still kept: the run directory is the thing that does not survive, which
# is the entire reason this step exists.
[ -s "$WORK/captures/m5b-run-records/numbers.md" ] \
  && ok "a capture outlives the run directory it came from" \
  || bad "the capture was removed when its run directory was"

# 6. Capturing the same evidence twice produces the same bytes.
#
# It did not, and that was not cosmetic: the stamp came from the clock, so the Stop hook dirtied the working
# tree whenever it rebuilt a capture -- and the session runner refuses to launch from a dirty tree. The
# automation could block the very thing it exists to record.
rm -rf "$WORK/captures" "$WORK/again"
run > /dev/null
cp -r "$WORK/captures" "$WORK/again"
rm -rf "$WORK/captures"
run > /dev/null
if diff -r "$WORK/again" "$WORK/captures" > "$WORK/idem.txt" 2>&1; then
  ok "capturing unchanged evidence twice produces identical bytes"
else
  bad "a second capture of the same evidence differs from the first"
  sed 's/^/        /' "$WORK/idem.txt" | head -4
fi

printf 'capture-evidence: %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
