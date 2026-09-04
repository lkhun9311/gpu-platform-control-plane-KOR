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

say()  { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

ACCOUNT=$(aws sts get-caller-identity --query Account --output text) || fail "not authenticated"
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
if ! aws s3api head-bucket --bucket "$BUCKET" 2>/dev/null; then
  say "creating results bucket $BUCKET"
  aws s3api create-bucket --bucket "$BUCKET" --region "$REGION" \
    --create-bucket-configuration LocationConstraint="$REGION" >/dev/null
  aws s3api put-public-access-block --bucket "$BUCKET" \
    --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  # The results are a few kilobytes and interesting for about a week; nothing here is worth paying storage
  # for indefinitely, and an expiring bucket cannot become the next thing nobody remembers deleting.
  aws s3api put-bucket-lifecycle-configuration --bucket "$BUCKET" --lifecycle-configuration \
    '{"Rules":[{"ID":"expire","Status":"Enabled","Filter":{"Prefix":""},"Expiration":{"Days":30}}]}'
fi

# ---------------------------------------------------------------- instance role
if ! aws iam get-instance-profile --instance-profile-name "$STACK" >/dev/null 2>&1; then
  say "creating instance role $STACK"
  aws iam create-role --role-name "$STACK" --assume-role-policy-document \
    '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
  aws iam put-role-policy --role-name "$STACK" --policy-name write-results --policy-document \
    "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[\"s3:PutObject\"],\"Resource\":\"arn:aws:s3:::$BUCKET/*\"}]}"
  aws iam create-instance-profile --instance-profile-name "$STACK" >/dev/null
  aws iam add-role-to-instance-profile --instance-profile-name "$STACK" --role-name "$STACK"
  # An instance profile is not usable the moment it is created, and a launch that races it fails with an
  # error that names neither the profile nor the delay.
  say "waiting for the instance profile to propagate"
  sleep 20
fi

# ---------------------------------------------------------------- placement
AMI=$(aws ssm get-parameter --region "$REGION" \
  --name /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id \
  --query Parameter.Value --output text) || fail "could not resolve a GPU AMI"

# A default subnet, because default subnets assign public IPs and therefore reach the internet through an
# internet gateway rather than a NAT gateway. That is the whole cost argument: inbound is free there.
#
# The zone has to be one that actually offers this instance type, which is not every zone in the region: the
# first attempt took Subnets[0], landed in ap-northeast-2b, and was refused because g5 is not offered there.
# AWS will say which zones do, so it is asked rather than assumed.
ZONES=$(aws ec2 describe-instance-type-offerings --region "$REGION" \
  --location-type availability-zone \
  --filters "Name=instance-type,Values=$INSTANCE_TYPE" \
  --query 'InstanceTypeOfferings[].Location' --output text)
[ -n "$ZONES" ] || fail "$INSTANCE_TYPE is offered in no availability zone of $REGION"
say "$INSTANCE_TYPE is offered in: $ZONES"

SUBNET=""
for z in $ZONES; do
  SUBNET=$(aws ec2 describe-subnets --region "$REGION" \
    --filters Name=default-for-az,Values=true Name=map-public-ip-on-launch,Values=true \
      "Name=availability-zone,Values=$z" \
    --query 'Subnets[0].SubnetId' --output text)
  [ -n "$SUBNET" ] && [ "$SUBNET" != "None" ] && { say "using $SUBNET in $z"; break; }
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

upload() { aws s3 cp "$1" "s3://$BUCKET/$2" || true; }
trap 'upload /var/log/microtest.log log.txt; shutdown -h now' EXIT

docker pull "$IMAGE"
mkdir -p /hf

python3 /usr/local/bin/microtest.py > /tmp/results.json 2>/tmp/microtest.err
upload /tmp/results.json results.json
upload /tmp/microtest.err stderr.txt
touch /tmp/DONE && upload /tmp/DONE DONE
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
      -e "s|ENGINE_IMAGE_PLACEHOLDER|$ENGINE_IMAGE|" \
      -e "s|MODEL_PLACEHOLDER|$MODEL|" \
      -e "s|BUCKET_PLACEHOLDER|$BUCKET|" "$RUNSCRIPT" | tail -n +2
} > "$UD"
cp "$UD" "$OUT/user-data.sh"
cp "$MEASURE" "$OUT/microtest.py"

say "launching $INSTANCE_TYPE spot (max \$$MAX_SPOT_PRICE/h)"
# Retried across zones, because "no capacity right now" is a normal Spot answer rather than a fault, and a
# single-zone attempt turns it into an aborted run.
launch_in() {
  aws ec2 run-instances --region "$REGION" \
    --image-id "$AMI" --instance-type "$INSTANCE_TYPE" --subnet-id "$SUBNET" \
    --iam-instance-profile "Name=$STACK" \
    --instance-initiated-shutdown-behavior terminate \
    --block-device-mappings '[{"DeviceName":"/dev/sda1","Ebs":{"VolumeSize":150,"VolumeType":"gp3","DeleteOnTermination":true}}]' \
    --instance-market-options "{\"MarketType\":\"spot\",\"SpotOptions\":{\"MaxPrice\":\"$MAX_SPOT_PRICE\",\"SpotInstanceType\":\"one-time\"}}" \
    --user-data "file://$UD" \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$STACK},{Key=purpose,Value=m5b-scheduler-microtest}]" \
    --query 'Instances[0].InstanceId' --output text
}
IID=""
for z in $ZONES; do
  SUBNET=$(aws ec2 describe-subnets --region "$REGION" \
    --filters Name=default-for-az,Values=true Name=map-public-ip-on-launch,Values=true \
      "Name=availability-zone,Values=$z" --query 'Subnets[0].SubnetId' --output text)
  [ -n "$SUBNET" ] && [ "$SUBNET" != "None" ] || continue
  say "trying $z ($SUBNET)"
  IID=$(launch_in 2>>"$OUT/launch-errors.txt") && [ -n "$IID" ] && [ "$IID" != "None" ] && break
  IID=""
done
[ -n "$IID" ] || fail "no zone would launch $INSTANCE_TYPE; see $OUT/launch-errors.txt"
echo "$IID" > "$OUT/instance-id"
say "instance $IID"

cleanup() {
  say "terminating $IID"
  aws ec2 terminate-instances --region "$REGION" --instance-ids "$IID" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

say "waiting for results (the engine has an image and weights to pull first)"
for _ in $(seq 1 120); do
  if aws s3api head-object --bucket "$BUCKET" --key DONE >/dev/null 2>&1; then
    say "results are up"
    break
  fi
  state=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$IID" \
    --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo unknown)
  case "$state" in
    terminated|shutting-down) say "instance ended before writing DONE"; break ;;
  esac
  sleep 30
done

for k in results.json log.txt stderr.txt; do
  aws s3 cp "s3://$BUCKET/$k" "$OUT/$k" >/dev/null 2>&1 || true
done

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
