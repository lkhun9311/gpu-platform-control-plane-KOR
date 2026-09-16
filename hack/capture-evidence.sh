#!/usr/bin/env bash
#
# Turns the measurements a session left behind into saved captures, and says what it could not capture.
#
# Run directories are gitignored and get deleted; the numbers in them are the expensive part of this
# repository. This copies the ones worth keeping into docs/evidence/, which is tracked, and draws the device
# series while it is still there to draw.
#
# The logic lives here rather than in .claude/hooks/ because .claude/ is gitignored: a capture step that
# nobody can review or test is exactly the kind of automation this repository does not trust. The hook is a
# caller; this is the thing it calls.
#
# It is deliberately picky about what counts. A run directory with no device series, no records and no card
# listing is a session that failed before it measured anything, and manufacturing a capture for it would put
# a picture of nothing next to pictures of something.
#
# Wiring, written down here because the hook that calls this lives in .claude/ and .claude/ is gitignored --
# so on a fresh clone the automation is absent and only this comment says it ever existed. In
# .claude/settings.local.json:
#
#   "Stop": [{"hooks": [{"type": "command",
#             "command": "bash <repo>/.claude/hooks/capture-evidence.sh"}]}]
#
# The wrapper must not end in `2>/dev/null` or `|| true`. A hook that discards its stderr cannot be told
# apart from a hook that never ran.
set -uo pipefail

REPO="${REPO:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$REPO" || { echo "capture-evidence: cannot enter $REPO" >&2; exit 1; }

EVIDENCE="${EVIDENCE:-docs/captures}"
PLOT="${PLOT:-hack/plot-device-observation.py}"
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

captured=0
skipped=0
problems=0

note() { printf 'capture-evidence: %s\n' "$*"; }
warn() { printf 'capture-evidence: %s\n' "$*" >&2; problems=$((problems + 1)); }

# The globs that name a run. Each is a directory a runner created and .gitignore hides.
shopt -s nullglob
RUNS=(hack/qlgpu-*/ hack/m5b-run-*/ hack/pop-*/ hack/microtest-*/ hack/m5c-run-*/)
shopt -u nullglob

if [ "${#RUNS[@]}" -eq 0 ]; then
  note "no run directories; nothing to capture"
  exit 0
fi

for run in "${RUNS[@]}"; do
  id=$(basename "$run")
  dest="$EVIDENCE/$id"

  # What makes a run worth capturing. Both are things only a real measurement produces.
  series=""
  for cand in "$run/device-util.tsv.gz" "$run/device-util.tsv"; do
    [ -s "$cand" ] && series="$cand" && break
  done
  # .jsonl as well as .json, which is not a detail.
  #
  # The first version of this glob matched only *.json and silently skipped every M5-b run in the tree --
  # the price-of-protection and scheduler measurements, which are the most expensive numbers here. They are
  # written as raw-*.jsonl and trace-*.jsonl. A capture step that quietly omits the best evidence is worse
  # than no capture step, because the directory it produces looks complete.
  #
  # evidence/ is in the list for the second half of that same lesson. The price-of-protection runner ships
  # its rows home as an archive and unpacks them into a subdirectory, so the 2026-09-07 pilot captured its
  # one-line run.json and none of the four raw files the run was bought for -- and the capture it produced
  # said "1 record(s)" rather than saying anything was missing.
  shopt -s nullglob
  records=("$run"/runs/*.json "$run"/session/*.json "$run"/evidence/*.jsonl "$run"/*.json "$run"/*.jsonl)
  shopt -u nullglob

  # nvidia-smi on real cards is a measurement too, and it is sometimes the only one a session got far enough
  # to make. Keeping it is how a failed session still says what hardware it saw.
  cards=""
  [ -s "$run/preflight-nvidia-smi.csv" ] && cards="$run/preflight-nvidia-smi.csv"

  if [ -z "$series" ] && [ -z "$cards" ] && [ "${#records[@]}" -eq 0 ]; then
    skipped=$((skipped + 1))
    continue
  fi

  # Up to date already. Compared against the series rather than the directory, because a directory's mtime
  # moves when anything in it is touched and that would redraw the same picture forever.
  newest_src="${series:-${cards:-${records[0]}}}"
  if [ -f "$dest/numbers.md" ] && [ "$dest/numbers.md" -nt "$newest_src" ]; then
    skipped=$((skipped + 1))
    continue
  fi

  if [ "$CHECK_ONLY" = "1" ]; then
    note "WOULD capture $id (series=${series:-none}, cards=${cards:-none}, records=${#records[@]})"
    captured=$((captured + 1))
    continue
  fi

  mkdir -p "$dest" || { warn "cannot create $dest"; continue; }

  # The numbers, first, because they survive a drawing that fails.
  {
    echo "# $id"
    echo
    # Stamped from the EVIDENCE, not from the clock.
    #
    # `date -u` here made every recapture of an unchanged run produce different bytes, so the Stop hook
    # dirtied the working tree on any session that rebuilt a capture -- and the session runner refuses to
    # launch from a dirty tree, which means the automation could block the very thing it exists to record.
    # A capture of evidence that has not changed must not change.
    echo "captured $(date -u -r "$newest_src" +%FT%TZ) from $run (stamped from the evidence, not the clock)"
    [ -s "$run/commit.txt" ] && echo "session commit: $(cat "$run/commit.txt")"
    [ -s "$run/instance-id" ] && echo "instance: $(cat "$run/instance-id")"
    echo
    if [ -s "$run/preflight-nvidia-smi.csv" ]; then
      echo "## cards"; echo; cat "$run/preflight-nvidia-smi.csv"; echo
    fi
    if [ "${#records[@]}" -gt 0 ]; then
      echo "## records"; echo
      echo "${#records[@]} record(s):"
      for r in "${records[@]}"; do echo "  $(basename "$r")"; done
      echo
    fi
    if [ -s "$run/preflight-why.txt" ]; then
      echo "## why a preflight refused"; echo
      head -40 "$run/preflight-why.txt"
      echo
    fi
  } > "$dest/numbers.md"

  if [ -n "$series" ]; then
    # Not silenced. A drawing that fails must say so here rather than leave the previous run's picture in
    # place looking current -- which is the failure mode this whole directory exists to make impossible.
    if python3 "$PLOT" "$series" -o "$dest/device.svg" --png --title "$id" > "$dest/.plot.log" 2>&1; then
      note "captured $id: $(head -1 "$dest/.plot.log")"
      captured=$((captured + 1))
    else
      warn "could not draw the series for $id:"
      sed 's/^/  /' "$dest/.plot.log" >&2
      # A refused drawing is a fact about the run, kept beside the numbers rather than only in a log.
      cp "$dest/.plot.log" "$dest/device-NOT-DRAWN.txt"
      rm -f "$dest/device.svg" "$dest/device.png"
    fi
    rm -f "$dest/.plot.log"
  else
    note "captured $id: numbers only, no device series in this run"
    captured=$((captured + 1))
  fi
done

# The index is rewritten from what is on disk, never appended to, so a capture that was deleted stops being
# listed instead of becoming a line pointing at nothing.
if [ "$CHECK_ONLY" = "0" ] && [ -d "$EVIDENCE" ]; then
  {
    echo "# Captured evidence"
    echo
    echo "Written by \`hack/capture-evidence.sh\`. Each entry is what a session left behind, kept because the"
    echo "run directory it came from is gitignored and does not survive."
    echo
    shopt -s nullglob
    any=0
    for d in "$EVIDENCE"/*/; do
      any=1
      n=$(basename "$d")
      drawn="no device series"
      [ -f "$d/device.svg" ] && drawn="[device.svg]($n/device.svg)"
      [ -f "$d/device-NOT-DRAWN.txt" ] && drawn="**series present but not drawable** — see \`$n/device-NOT-DRAWN.txt\`"
      echo "- \`$n\` — [numbers]($n/numbers.md) · $drawn"
    done
    shopt -u nullglob
    [ "$any" = "0" ] && echo "_Nothing captured yet._"
  } > "$EVIDENCE/INDEX.md"
fi

note "$captured captured, $skipped skipped, $problems problem(s)"
[ "$problems" -eq 0 ]
