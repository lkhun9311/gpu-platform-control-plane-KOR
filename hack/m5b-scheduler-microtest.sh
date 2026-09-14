#!/usr/bin/env bash
#
# The two-request test from docs/superpowers/specs/2026-09-04-scheduler-microtest.md.
#
# It answers one question: can the engine protect a waiting short request by configuration alone, or must
# something above it limit how many long prefills are in flight. That decides which layer a successor to
# M5-b belongs in, so it is worth buying before designing one.
#
# It does NOT stand up the cluster. The question is about vLLM's scheduler -- no gateway, no operator, no
# Kubernetes -- and the cluster path costs about $2.70 for an answer this does not need. A single Spot
# instance in a DEFAULT PUBLIC SUBNET costs about $0.57, and the difference is mostly one line: inbound data
# transfer is free over an internet gateway, while the same 15.6 GB of image and weights crossed the NAT
# gateway at $0.059/GB in every paid run so far.
#
# No inbound ports are opened. Everything runs from user-data and the results go to S3; the instance then
# terminates itself. A backstop timer inside the instance shuts it down even if the test hangs, and this
# script terminates it as well, because one of the two will be awake.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

REGION="${AWS_REGION:-ap-northeast-2}"
INSTANCE_TYPE="${INSTANCE_TYPE:-g5.xlarge}"
MAX_SPOT_PRICE="${MAX_SPOT_PRICE:-0.80}"
# 90 minutes of instance life is about three times the measurement, and the cheapest possible protection
# against a hung run: a Spot g5.xlarge left running for a day would cost more than every paid session so far.
BACKSTOP_SECONDS="${BACKSTOP_SECONDS:-5400}"
OUT="${OUT:-hack/microtest-$(date -u +%Y%m%d-%H%M%S)}"
STACK="m5b-microtest"
# Every object this run writes lives under its own prefix, and the completion marker is one of them.
#
# The keys used to be fixed at the bucket root -- results.json, DONE -- against a bucket that keeps
# objects for 30 days. So the second run of this script found the FIRST run's DONE on its opening
# poll, downloaded the first run's results, printed them as its own readings and exited 0, having
# launched and paid for a GPU instance that it then terminated seconds later without waiting for it.
# A stale marker is indistinguishable from a fast one, and the recorded transcript for that case is
# hack/test/spot-lifecycle/golden/stale-done.txt.
#
# Derived from the run directory rather than generated, so the local evidence and the S3 objects carry
# the same name and either can be found from the other.
RUN_ID="$(basename "$OUT")"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# The EC2 lifecycle is shared with the price-of-protection runner and lives in its own file.
#
# Defined BEFORE sourcing, because the library only supplies defaults for names the caller has not
# provided. Without this its diagnostics would go to stderr while this script's go to stdout, and the two
# would interleave differently in a captured log.
spot_say()  { say "$@"; }
spot_fail() { fail "$@"; }
# BASH_SOURCE, not $0: inside a sourced file $0 is still this script, so locating the library by $0 would
# work here and break the moment anything else sourced it.
# shellcheck source=hack/lib/spot-run.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib/spot-run.sh"

ACCOUNT=$(spot_account) || fail "not authenticated"
BUCKET="${BUCKET:-$STACK-$ACCOUNT}"

# The engine image, read from the manifest rather than typed here, so the microtest measures the same engine
# the arms measured. A different digest would answer a question about a different scheduler.
ENGINE_IMAGE=$(grep -oP '^\s*(- )?image:\s*\Kvllm/vllm-openai@sha256:\S+' config/vllm/deployment.yaml | head -1)
[ -n "$ENGINE_IMAGE" ] || fail "no digest-pinned vLLM image in config/vllm/deployment.yaml"
# vLLM takes the model as its first positional argument, so that is where the manifest carries it -- not a
# --model flag, and not a default here. A default would let this measure a different engine than the arms
# did without anything saying so, which is how one paid run spent three hours asking a Qwen engine for a
# model no deployment served.
MODEL=$(python3 -c "
import re,sys
args=re.search(r'args:\n((?:\s*(?:-|#).*\n)+)', open('config/vllm/deployment.yaml').read())
for line in (args.group(1).splitlines() if args else []):
    v=line.strip()
    if v.startswith('- ') and not v.startswith('- --'):
        print(v[2:].strip()); break
")
[ -n "$MODEL" ] || fail "could not read the served model from config/vllm/deployment.yaml; it is vLLM's first positional argument"

mkdir -p "$OUT"
say "engine $ENGINE_IMAGE"
say "model  $MODEL"
say "output $OUT"

# ---------------------------------------------------------------- results path
spot_ensure_bucket "$BUCKET" "$REGION" 30 || fail "could not prepare the results bucket $BUCKET"

# ---------------------------------------------------------------- instance role
# This measurement only ever writes. A runner that also ships a binary to the instance needs GetObject, and
# passing the policy in is what lets the two differ without the library knowing about either.
spot_ensure_profile "$STACK" \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[\"s3:PutObject\"],\"Resource\":\"arn:aws:s3:::$BUCKET/*\"}]}" \
  20 || fail "could not prepare the instance profile $STACK"

# ---------------------------------------------------------------- placement
AMI=$(spot_resolve_ami "$REGION" \
  /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id) \
  || fail "could not resolve a GPU AMI"

# A default subnet, because default subnets assign public IPs and therefore reach the internet through an
# internet gateway rather than a NAT gateway. That is the whole cost argument: inbound is free there.
#
# The zone has to be one that actually offers this instance type, which is not every zone in the region: the
# first attempt took Subnets[0], landed in ap-northeast-2b, and was refused because g5 is not offered there.
# AWS will say which zones do, so it is asked rather than assumed.
ZONES=$(spot_zones_offering "$REGION" "$INSTANCE_TYPE")
[ -n "$ZONES" ] || fail "$INSTANCE_TYPE is offered in no availability zone of $REGION"
say "$INSTANCE_TYPE is offered in: $ZONES"

SUBNET=""
for z in $ZONES; do
  if SUBNET=$(spot_subnet_in_zone "$REGION" "$z"); then
    say "using $SUBNET in $z"
    break
  fi
  SUBNET=""
done
[ -n "$SUBNET" ] || fail "no default public subnet in any zone offering $INSTANCE_TYPE; this test relies on one for free ingress"

say "ami $AMI in $SUBNET"

RUNSCRIPT=$(mktemp)
cat > "$RUNSCRIPT" <<'USERDATA'
#!/bin/bash
exec > >(tee /var/log/microtest.log) 2>&1
set -x
( sleep BACKSTOP_SECONDS_PLACEHOLDER; shutdown -h now ) &

IMAGE="ENGINE_IMAGE_PLACEHOLDER"
MODEL="MODEL_PLACEHOLDER"
BUCKET="BUCKET_PLACEHOLDER"

PREFIX="RUN_ID_PLACEHOLDER"

# Keys are relative to this run's prefix, so no call site can write to the bucket root by forgetting.
upload() { aws s3 cp "$1" "s3://$BUCKET/$PREFIX/$2" || true; }
trap 'upload /var/log/microtest.log log.txt; shutdown -h now' EXIT

docker pull "$IMAGE"
mkdir -p /hf

rc=0
python3 /usr/local/bin/microtest.py > /tmp/results.json 2>/tmp/microtest.err || rc=$?
upload /tmp/results.json results.json
upload /tmp/microtest.err stderr.txt

# DONE is written only if the measurement actually produced parseable results.
#
# This script runs under set -x and NOT set -e, deliberately: a failed measurement still has to upload
# its log and its stderr, which is exactly what an early exit would prevent. The cost of that choice
# was that the next line ran unconditionally, so a Python process that died on its first cell still
# announced success, and the host saw a completed run. The exit status is captured instead, and the
# marker is gated on it together with results that parse -- an empty or truncated file is a failure
# that looks like a success at every layer above it.
if [ "$rc" -eq 0 ] && [ -s /tmp/results.json ] \
   && python3 -c 'import json,sys; json.load(open("/tmp/results.json"))' 2>/dev/null; then
  touch /tmp/DONE && upload /tmp/DONE DONE
else
  echo "measurement did not produce usable results (exit $rc); DONE withheld"
fi
USERDATA

# The measurement itself, kept as its own file so the shell wrapper stays about lifecycle and this stays
# about the experiment.
MEASURE=$(mktemp)
cat > "$MEASURE" <<'PYEOF'
import json, os, subprocess, threading, time, urllib.request, statistics

IMAGE = os.environ["IMAGE"]; MODEL = os.environ["MODEL"]
BASE = "http://127.0.0.1:8000"
LONG_CHARS, SHORT_CHARS = 40000, 200
CELLS = [(b, p) for b in (512, 2048, 8192) for p in ("fcfs", "priority")]
REPS = int(os.environ.get("REPS", "10"))

def post_stream(chars, priority=None, timeout=180):
    body = {"model": MODEL, "stream": True, "max_tokens": 4,
            "messages": [{"role": "user", "content": "a" * chars}]}
    if priority is not None:
        body["priority"] = priority
    req = urllib.request.Request(BASE + "/v1/chat/completions",
                                 data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=timeout) as r:
        for raw in r:
            if raw.startswith(b"data: ") and b'"content"' in raw:
                return (time.perf_counter() - t0) * 1000
    return None

def running_count():
    try:
        with urllib.request.urlopen(BASE + "/metrics", timeout=5) as r:
            for line in r.read().decode().splitlines():
                if line.startswith("vllm:num_requests_running"):
                    return float(line.rsplit(" ", 1)[1])
    except Exception:
        pass
    return 0.0

def wait_healthy(deadline=900):
    end = time.time() + deadline
    while time.time() < end:
        try:
            urllib.request.urlopen(BASE + "/health", timeout=5).read()
            return True
        except Exception:
            time.sleep(3)
    return False

out = {"image": IMAGE, "model": MODEL, "cells": []}
for budget, policy in CELLS:
    subprocess.run(["docker", "rm", "-f", "vllm"], capture_output=True)
    args = ["docker", "run", "-d", "--name", "vllm", "--gpus", "all", "--network", "host",
            "-v", "/hf:/root/.cache/huggingface", "--shm-size", "8g", IMAGE,
            "--model", MODEL, "--dtype", "half", "--max-model-len", "16384",
            "--max-num-seqs", "64", "--gpu-memory-utilization", "0.90",
            "--no-enable-prefix-caching", "--port", "8000",
            "--max-num-batched-tokens", str(budget), "--scheduling-policy", policy]
    subprocess.run(args, check=False, capture_output=True)
    cell = {"max_num_batched_tokens": budget, "scheduling_policy": policy}
    if not wait_healthy():
        cell["error"] = "engine never became healthy"
        out["cells"].append(cell); continue

    logs = subprocess.run(["docker", "logs", "vllm"], capture_output=True).stdout.decode("utf-8", "replace")
    cell["startup"] = [l.strip() for l in logs.splitlines()
                       if any(k in l for k in ("Chunked prefill", "max_num_batched_tokens",
                                               "scheduling", "max_num_seqs", "KV cache"))][:20]

    # The uncontended baseline for THIS cell, because a per-cell reading is what the readings compare against.
    base = [post_stream(SHORT_CHARS, 0 if policy == "priority" else None) for _ in range(3)]
    base = [b for b in base if b]
    cell["baseline_ms"] = statistics.median(base) if base else None

    contended = []
    for _ in range(REPS):
        holder = {}
        t = threading.Thread(target=lambda: holder.setdefault("long", post_stream(LONG_CHARS, 1 if policy == "priority" else None)))
        t.start()
        # Wait until the engine actually has it, rather than assuming a sleep is long enough.
        deadline = time.time() + 30
        while running_count() < 1 and time.time() < deadline:
            time.sleep(0.02)
        overlapped = running_count() >= 1
        short = post_stream(SHORT_CHARS, 0 if policy == "priority" else None)
        t.join(timeout=180)
        contended.append({"short_ttft_ms": short, "long_ttft_ms": holder.get("long"), "overlapped": overlapped})
    cell["contended"] = contended
    ok = [c["short_ttft_ms"] for c in contended if c["short_ttft_ms"] and c["overlapped"]]
    cell["contended_median_ms"] = statistics.median(ok) if ok else None
    if cell["baseline_ms"] and cell["contended_median_ms"]:
        cell["ratio"] = cell["contended_median_ms"] / cell["baseline_ms"]
    out["cells"].append(cell)

subprocess.run(["docker", "rm", "-f", "vllm"], capture_output=True)
print(json.dumps(out, indent=2))
PYEOF

# Assembled here rather than templated inside the heredoc, so the measurement file stays valid Python that
# can be read and run on its own.
UD=$(mktemp)
{
  echo "#!/bin/bash"
  echo "mkdir -p /usr/local/bin"
  echo "cat > /usr/local/bin/microtest.py <<'MEASUREEOF'"
  cat "$MEASURE"
  echo "MEASUREEOF"
  echo "export IMAGE='$ENGINE_IMAGE' MODEL='$MODEL'"
  sed -e "s|BACKSTOP_SECONDS_PLACEHOLDER|$BACKSTOP_SECONDS|" \
      -e "s|RUN_ID_PLACEHOLDER|$RUN_ID|" \
      -e "s|ENGINE_IMAGE_PLACEHOLDER|$ENGINE_IMAGE|" \
      -e "s|MODEL_PLACEHOLDER|$MODEL|" \
      -e "s|BUCKET_PLACEHOLDER|$BUCKET|" "$RUNSCRIPT" | tail -n +2
} > "$UD"

# The FIRST thing checked about the payload, because it is the one `bash -n` cannot see.
#
# The stripping above begins with `tail -n +2`, which drops the heredoc's `#!/bin/bash`, and the line that
# re-emits it is one line in a block nobody reads twice. cloud-init executes user-data as a script ONLY when
# it begins with `#!`. A payload without one parses perfectly and does nothing: the instance boots, cloud-init
# declines to run it, and the machine bills until its backstop with no log at all, because the trap that
# uploads one lives inside the script that never ran.
#
# hack/m5c-gpu-session.sh was written from the shape of this file and dropped that line. Its first paid run
# on 2026-09-11 held a g5.2xlarge for 145 minutes, about $1.64, and produced nothing. This guard is here so
# the next omission costs a refusal instead.
head -1 "$UD" | grep -q '^#!' \
  || fail "the generated user-data does not begin with a shebang, so cloud-init would not execute it and the instance would boot, do nothing, and bill until its backstop. See $OUT/user-data.sh"
cp "$UD" "$OUT/user-data.sh"
cp "$MEASURE" "$OUT/microtest.py"

say "launching $INSTANCE_TYPE spot (max \$$MAX_SPOT_PRICE/h)"
# Retried across zones, because "no capacity right now" is a normal Spot answer rather than a fault, and a
# single-zone attempt turns it into an aborted run.
TAGS="ResourceType=instance,Tags=[{Key=Name,Value=$STACK},{Key=purpose,Value=m5b-scheduler-microtest}]"
# The trap is armed BEFORE the launch loop, not after it.
#
# It used to sit below `say "instance $IID"`, which left a window: run-instances had returned an id and
# nothing would terminate it yet. Under `set -euo pipefail` a failed write of the instance-id file, a
# SIGPIPE on stdout because this script was piped to something that had exited, or a Ctrl-C in that window
# all exit with a GPU instance running and no terminator. spot_terminate returns 0 on an empty id, so
# arming it early costs nothing and closes the window.
cleanup() { spot_terminate "$REGION" "$IID"; }
IID=""
trap cleanup EXIT INT TERM
for z in $ZONES; do
  SUBNET=$(spot_subnet_in_zone "$REGION" "$z") || continue
  say "trying $z ($SUBNET)"
  IID=$(spot_launch "$REGION" "$AMI" "$INSTANCE_TYPE" "$SUBNET" "$STACK" \
        "$MAX_SPOT_PRICE" 150 "$UD" "$TAGS" 2>>"$OUT/launch-errors.txt") \
    && [ -n "$IID" ] && [ "$IID" != "None" ] && break
  IID=""
done
[ -n "$IID" ] || fail "no zone would launch $INSTANCE_TYPE; see $OUT/launch-errors.txt"
echo "$IID" > "$OUT/instance-id"
say "instance $IID"

# The trap stays here rather than in the library. Traps do not stack -- the last one for a signal replaces
# the earlier one -- so a library that armed its own would silently discard whatever the caller installed,
# and the thing being discarded is what stops an idle GPU instance from billing all night.

say "waiting for results (the engine has an image and weights to pull first)"
# The three endings are distinguished by exit status, because "the loop ended" has three causes and only
# one of them is a result. The library echoes the instance's terminal state on the second.
done_seen=0
ended_early=""
marker_rc=0
ended_early=$(spot_wait_for_marker "$REGION" "$BUCKET" "$RUN_ID/DONE" "$IID" 120 30) || marker_rc=$?
case "$marker_rc" in
  0) say "results are up"; done_seen=1 ;;
  2) say "instance ended before writing DONE" ;;
esac

for k in results.json log.txt stderr.txt; do
  aws s3 cp "s3://$BUCKET/$RUN_ID/$k" "$OUT/$k" >/dev/null 2>&1 || true
done

# Said before the results are read, and separately from them, because the two failures need different
# answers: an instance that died has a log worth reading, and a run that timed out is probably still
# alive and worth watching. Both used to arrive as the same "no results were written".
if [ "$done_seen" -eq 0 ]; then
  if [ -n "$ended_early" ]; then
    fail "the instance was $ended_early before it wrote its completion marker; $OUT/log.txt is whatever it managed to upload"
  fi
  fail "no completion marker after 120 polls; the run either is still going or hung, and $IID has been terminated"
fi

if [ -s "$OUT/results.json" ]; then
  say "readings"
  python3 - "$OUT/results.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print(f"{'budget':>8} {'policy':>9} {'baseline':>10} {'contended':>10} {'ratio':>8}")
for c in d["cells"]:
    b=c.get("baseline_ms"); m=c.get("contended_median_ms"); r=c.get("ratio")
    print(f"{c['max_num_batched_tokens']:>8} {c['scheduling_policy']:>9} "
          f"{(f'{b:.1f}' if b else '-'):>10} {(f'{m:.1f}' if m else '-'):>10} {(f'{r:.2f}x' if r else '-'):>8}")
PY
else
  fail "no results were written; $OUT/log.txt may say why"
fi
