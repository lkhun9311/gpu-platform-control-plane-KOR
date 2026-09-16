#!/bin/bash
mkdir -p /usr/local/bin
cat > /usr/local/bin/microtest.py <<'MEASUREEOF'
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
MEASUREEOF
export IMAGE='vllm/vllm-openai@sha256:0a51ea5b4ae2dc5d81890e5173f54203d2a3ae0cfffe51b8fd2afd4391bfd967' MODEL='Qwen/Qwen2.5-3B-Instruct'
exec > >(tee /var/log/microtest.log) 2>&1
set -x
( sleep 5400; shutdown -h now ) &

IMAGE="vllm/vllm-openai@sha256:0a51ea5b4ae2dc5d81890e5173f54203d2a3ae0cfffe51b8fd2afd4391bfd967"
MODEL="Qwen/Qwen2.5-3B-Instruct"
BUCKET="stub-bucket"

PREFIX="run"

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
