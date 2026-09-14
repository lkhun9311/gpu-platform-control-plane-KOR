#!/usr/bin/env bash
#
# The run pre-registered in docs/superpowers/specs/2026-09-05-the-price-of-protection.md.
#
# It asks what protection costs: is there an engine configuration that holds the premium tail WITHOUT
# deleting the contending tenant's work, and what does the protected tenant pay for it. The factors are
# vLLM's chunked-prefill token budget and its scheduling policy. There is no gateway in the path -- M5-b's
# own evidence says the gateway cannot observe the pressure it gated on, so a control plane layered on a
# misconfigured engine measures the wrong thing.
#
# WHAT MAKES THIS DIFFERENT FROM hack/m5b-scheduler-microtest.sh
#
# That test sent two requests and reported a median. This replays a real open-loop trace through the Go
# harness, which is where the fault-injected measurement lives: per-tenant output share, aggregate
# throughput over summed per-repetition spans, TPOT over sorted samples, and a refusal when the evidence
# cannot answer the question. Reimplementing any of that in the user-data Python would be a second
# measurement nobody has adversarially reviewed.
#
# So the benchharness binary is built here, checksummed, and shipped to the instance. The instance verifies
# the checksum before running it, because a truncated download that still executes is the kind of failure
# that produces numbers rather than errors.
#
# COST
#
# One Spot g5.xlarge in a DEFAULT PUBLIC SUBNET, about $0.65/h effective, with no NAT data charge. The
# pre-registration is explicit that the pilot's price has NOT been re-derived for its current four-arm
# scope, so the operator sets ARMS and knows what they are buying.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

REGION="${AWS_REGION:-ap-northeast-2}"
INSTANCE_TYPE="${INSTANCE_TYPE:-g5.xlarge}"
MAX_SPOT_PRICE="${MAX_SPOT_PRICE:-0.80}"
BACKSTOP_SECONDS="${BACKSTOP_SECONDS:-7200}"
REPS="${REPS:-1}"

# The offered load, derived in docs/superpowers/specs/2026-09-08-the-load-needs-an-upper-gate.md.
#
# It is set here rather than left to gen-trace's defaults because those defaults are calibrated against a
# stub backend that costs nothing to serve -- the flag's own help says so -- and the 2026-09-07 pilot ran
# them straight at a GPU. Measured against that engine's own sustained prefill throughput, the trace offered
# TEN TIMES the prefill it can do, and 95.9% of the control timed out. Every reading below reading 4 was
# then a ratio over a remnant.
#
# The protected tenant is unchanged at about 9.25/s, because it was 5% of capacity and none of the
# contention came from it. The contending tenant drops eighteen-fold and the probe pair with it: at 3,171
# tokens each those two carried 78% of capacity while their flag help called them a small population. What
# is left sits at roughly 60% of measured capacity, where one long prefill is in flight about half the time
# -- which is a contended engine by a wide margin, since a single concurrent long prefill has already been
# measured at fifteen times R1's premium tail.
#
# The duration follows from reading 4b rather than from taste. 100 contender completions is the floor. At the
# realised 0.45/s a 300 s trace offers 135, which clears the floor only if better than three quarters of them
# complete -- too thin a margin for the gate that voids the whole run, and Poisson arrivals scatter around
# the mean besides. 420 s offers about 190. Lengthening is the safe direction to add margin in: raising the
# rate instead would push utilisation back toward the saturation that voided the first pilot.
RATE="${RATE:-9.85}"                  # total arrivals per second across all tenants
DURATION_MS="${DURATION_MS:-420000}"  # 420 s of arrivals
PREMIUM_WEIGHT="${PREMIUM_WEIGHT:-1}"
NOISY_WEIGHT="${NOISY_WEIGHT:-0.054}" # 0.50/s against premium's 9.25/s
PROBE_WEIGHT="${PROBE_WEIGHT:-0.0054}"

OUT="${OUT:-hack/pop-$(date -u +%Y%m%d-%H%M%S)}"
STACK="m5b-pop"
STUDY="price-of-protection-2026-09-05"

# The arms to run, as "name:budget:policy". An empty budget means "pass no budget flag at all", which is
# how the control is defined: the pre-registration deliberately does not name a number, because nothing in
# the evidence establishes what this image defaults to.
#
# The pilot is these four. The confirmatory run is all ten. Both are this script with a different ARMS.
ARMS="${ARMS:-R1::,default-fcfs::,mbt-0512-fcfs:512:fcfs,mbt-0512-priority:512:priority}"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

spot_say()  { say "$@"; }
spot_fail() { fail "$@"; }
# shellcheck source=hack/lib/spot-run.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib/spot-run.sh"

ACCOUNT=$(spot_account) || fail "not authenticated"

# Refuse to launch on credentials that expire before the run does.
#
# Being authenticated NOW is not the question. This script's cleanup trap terminates the instance by calling
# the AWS API, so credentials that die mid-run leave a GPU instance billing with nothing able to stop it: the
# only backstop left is the timer inside the instance, and that is two hours of spend nobody chose.
#
# It happened on 2026-09-08. A ten-arm run was launched with fifty-four minutes of credential left against a
# hundred minutes of work, and the operator caught it rather than the script. The margin below is the run's
# own estimate -- 15 min of fixed cost, 1.5 per arm, 7 per arm-repetition, fitted to three paid runs -- plus
# half an hour, because an estimate that is exactly right is still no margin at all.
#
# The expiry is the LATEST live credential rather than the earliest of all cached ones. Checking the earliest
# is how I read a fresh twelve-hour login as already expired: the cache also holds files for profiles this
# run does not use, and theirs had lapsed hours before.
require_credential_margin() {
  local arms reps need
  arms=$(awk -F, '{print NF}' <<<"$ARMS")
  reps="$REPS"
  need=$(( 15 + arms*3/2 + arms*reps*7 + 30 ))
  python3 - "$need" <<'PY' || fail "not enough credential left to finish this run and terminate its instance"
import datetime, glob, json, os, sys
need = int(sys.argv[1])
now = datetime.datetime.now(datetime.timezone.utc)
best = None
cache = os.environ.get("AWS_CLI_CACHE_DIR") or os.path.expanduser("~/.aws/cli/cache")
for f in glob.glob(f"{cache}/*.json"):
    try:
        exp = (json.load(open(f)).get("Credentials") or {}).get("Expiration")
    except Exception:
        continue
    if not exp:
        continue
    t = datetime.datetime.fromisoformat(str(exp).replace("Z", "+00:00"))
    if t > now and (best is None or t > best):
        best = t
if best is None:
    print("no live cached credentials; run aws sso login before a paid session", file=sys.stderr)
    sys.exit(1)
left = (best - now).total_seconds() / 60
print(f"== credentials live for {left:.0f} min, this run needs about {need} min including margin")
if left < need:
    print(f"credentials expire in {left:.0f} min but this run needs about {need}; "
          f"its cleanup would be unable to terminate the instance", file=sys.stderr)
    sys.exit(1)
PY
}
require_credential_margin

BUCKET="${BUCKET:-$STACK-$ACCOUNT}"
RUN_ID="$(basename "$OUT")"

# The engine and model come from the manifest, not from defaults here, so this measures the same engine the
# arms measured. A default would let this answer a question about a different scheduler without anything
# saying so.
ENGINE_IMAGE=$(grep -oP '^\s*(- )?image:\s*\Kvllm/vllm-openai@sha256:\S+' config/vllm/deployment.yaml | head -1)
[ -n "$ENGINE_IMAGE" ] || fail "no digest-pinned vLLM image in config/vllm/deployment.yaml"
MODEL=$(python3 -c "
import re
args=re.search(r'args:\n((?:\s*(?:-|#).*\n)+)', open('config/vllm/deployment.yaml').read())
for line in (args.group(1).splitlines() if args else []):
    v=line.strip()
    if v.startswith('- ') and not v.startswith('- --'):
        print(v[2:].strip()); break
")
[ -n "$MODEL" ] || fail "could not read the served model from config/vllm/deployment.yaml"

mkdir -p "$OUT"
say "study  $STUDY"
say "engine $ENGINE_IMAGE"
say "model  $MODEL"
say "arms   $ARMS"
say "output $OUT"

# ---------------------------------------------------------------- the harness binary
#
# Built statically, because the deep-learning AMI's libc is not this host's and a dynamically linked binary
# would fail on the instance after the image and weights had already been pulled -- twenty minutes into a
# paid hour.
say "building the harness for the instance"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/benchharness" ./cmd/benchharness \
  || fail "could not build benchharness"
HARNESS_SHA=$(sha256sum "$OUT/benchharness" | cut -d' ' -f1)
say "harness sha256 $HARNESS_SHA"

# ---------------------------------------------------------------- AWS scaffolding
spot_ensure_bucket "$BUCKET" "$REGION" 30 || fail "could not prepare the results bucket $BUCKET"

# This runner READS as well as writes: the instance downloads the harness binary it was sent. The microtest
# needs PutObject only, which is why the policy is the caller's rather than the library's.
spot_ensure_profile "$STACK" \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[\"s3:PutObject\",\"s3:GetObject\"],\"Resource\":\"arn:aws:s3:::$BUCKET/*\"}]}" \
  20 || fail "could not prepare the instance profile $STACK"

say "uploading the harness"
aws s3 cp "$OUT/benchharness" "s3://$BUCKET/$RUN_ID/bin/benchharness" >/dev/null \
  || fail "could not upload the harness"

AMI=$(spot_resolve_ami "$REGION" \
  /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id) \
  || fail "could not resolve a GPU AMI"

ZONES=$(spot_zones_offering "$REGION" "$INSTANCE_TYPE")
[ -n "$ZONES" ] || fail "$INSTANCE_TYPE is offered in no availability zone of $REGION"
say "$INSTANCE_TYPE is offered in: $ZONES"

# ---------------------------------------------------------------- what the instance runs
MEASURE=$(mktemp)
cat > "$MEASURE" <<'PYEOF'
"""Run one cell per arm: launch the engine, prove it is configured as claimed, replay, keep the evidence.

The measurement itself is the Go harness. This orchestrates it, and its one piece of judgement is the
guard below: an arm is only an arm if the engine agrees it is.
"""

import json, os, re, subprocess, sys, time, urllib.request

IMAGE = os.environ["IMAGE"]; MODEL = os.environ["MODEL"]
STUDY = os.environ["STUDY"]; REPS = int(os.environ["REPS"])
OUT = "/tmp/evidence"; BASE = "http://127.0.0.1:8000"
HARNESS = "/usr/local/bin/benchharness"

# name:budget:policy, an empty budget meaning "pass no flag".
ARMS = [a.split(":") for a in os.environ["ARMS"].split(",") if a]
# Derived from the same list the trace generator uses, so the map cannot drift from the tenants.
PRIORITIES = "premium-1=0,standard-noisy=5,standard-probe-under=5,standard-probe-over=5"


def wait_healthy(deadline=900):
    end = time.time() + deadline
    while time.time() < end:
        try:
            urllib.request.urlopen(BASE + "/health", timeout=5).read()
            return True
        except Exception:
            time.sleep(3)
    return False


def engine_config(name):
    """Return the engine's own resolved startup lines, and save them AND the whole log beside the evidence.

    The whole log is kept because the filtered version cannot answer the question the control arm exists to
    answer. B0 -- the batch budget an engine picks when nobody sets one -- is by definition a DEFAULT, and
    the filter's most informative pattern is `non-default args`, which excludes defaults by construction.
    The engine also prints `Chunked prefill is enabled with max_num_batched_tokens=N` only when the flag was
    passed. So on the 2026-09-07 pilot the control's budget was absent from everything kept, the full log was
    not saved, and the instance was gone before anyone noticed: B0 was unrecoverable from a run bought to
    resolve it.

    Both streams are read for the same reason. docker logs writes the container's stdout and stderr
    separately and this only took stdout, so anything the engine logged to stderr was discarded unseen.
    """
    p = subprocess.run(["docker", "logs", "vllm"], capture_output=True)
    logs = (p.stdout.decode("utf-8", "replace") or "") + (p.stderr.decode("utf-8", "replace") or "")
    with open(f"{OUT}/engine-log-{name}.txt", "w") as f:
        f.write(logs)
    kept = [l.strip() for l in logs.splitlines()
            if any(k in l for k in ("non-default args", "Chunked prefill", "scheduling", "KV cache",
                                    "max_num_batched_tokens", "SchedulerConfig", "EngineArgs", "VllmConfig"))]
    with open(f"{OUT}/engine-config-{name}.txt", "w") as f:
        f.write("\n".join(kept) + "\n")
    return "\n".join(kept)


def fingerprint(logs_text):
    """Two numbers the engine reports for every arm, whether or not it states its batch budget.

    B0 -- the budget an engine picks when nobody sets one -- is not printed. vLLM logs
    "Chunked prefill is enabled with max_num_batched_tokens=N" ONLY when N was passed, and the control's log
    carries no scheduler line at all even though chunked prefill is on. Two paid pilots confirmed it: the
    whole log, both streams, and the number is not in it.

    But the budget leaves marks. The compiler's range endpoint tracks it, and the KV cache the engine ends up
    with is a deterministic function of the activation memory the budget reserves. Across two sessions on two
    instances those marks were identical: unset gave (2048, 369,680) and 512 gave (512, 386,912), every time.

    So B0 is resolved by CONSTRUCTION rather than by parsing: run an arm that states a budget, and if its
    fingerprint matches the control's, the control was configured the same way. That is a match between two
    engines this run started, not an inference about a field nobody documented.
    """
    ep = re.search(r"compile_ranges_endpoints': \[(\d+)\]", logs_text)
    kv = re.search(r"GPU KV cache size: *([\d,]+)", logs_text)
    return {"compileRangeEndpoint": int(ep.group(1)) if ep else None,
            "kvCacheTokens": int(kv.group(1).replace(",", "")) if kv else None}


def agrees(config, budget, policy):
    """Refuse an arm the engine does not agree it is.

    A name is not evidence. `mbt-0512-priority` pointed at an engine running 1024, or at one that silently
    ignored the policy flag, produces a full set of healthy-looking rows under a label that is false -- and
    a string check on the arm name cannot catch it, because the typo names another VALID cell.

    vLLM reports its resolved settings as a Python dict, `'scheduling_policy': 'priority'` with a colon, so
    the separator is matched as one to four non-alphanumeric characters rather than assuming an equals sign.
    """
    if budget:
        if not re.search(r"max_num_batched_tokens[^0-9]{1,4}%s\b" % budget, config):
            return f"the engine does not report max_num_batched_tokens={budget}"
    if policy == "priority":
        if not re.search(r"scheduling[-_]policy[^a-zA-Z0-9]{1,4}priority", config, re.I):
            return "the engine does not report the priority scheduling policy"
    elif policy == "fcfs":
        if re.search(r"scheduling[-_]policy[^a-zA-Z0-9]{1,4}priority", config, re.I):
            return "the engine reports the priority policy for an arm that must be fcfs"
    return None


os.makedirs(OUT, exist_ok=True)
manifest = {"study": STUDY, "image": IMAGE, "model": MODEL, "arms": [], "reps": REPS}

for name, budget, policy in ARMS:
    subprocess.run(["docker", "rm", "-f", "vllm"], capture_output=True)
    args = ["docker", "run", "-d", "--name", "vllm", "--gpus", "all", "--network", "host",
            "-v", "/hf:/root/.cache/huggingface", "--shm-size", "8g", IMAGE,
            "--model", MODEL, "--dtype", "half", "--max-model-len", "16384",
            "--max-num-seqs", "64", "--gpu-memory-utilization", "0.90",
            "--no-enable-prefix-caching", "--port", "8000"]
    if budget:
        args += ["--max-num-batched-tokens", budget]
    if policy:
        args += ["--scheduling-policy", policy]
    subprocess.run(args, check=False, capture_output=True)

    entry = {"arm": name, "requested_budget": budget or "engine default", "requested_policy": policy or "engine default"}
    if not wait_healthy():
        entry["error"] = "engine never became healthy"
        manifest["arms"].append(entry); continue

    config = engine_config(name)
    entry["fingerprint"] = fingerprint(open(f"{OUT}/engine-log-{name}.txt").read())
    # The resolved budget, recorded whether or not we asked for one. For the control this is B0, the number
    # the pre-registration deliberately refuses to guess.
    resolved = re.search(r"max_num_batched_tokens[^0-9]{1,4}(\d+)", config)
    entry["resolved_budget"] = resolved.group(1) if resolved else None
    # An arm that asked for no budget IS the control, and an unread B0 means the study's own gate has not
    # been met. The arm still runs -- its rows are paid for and worth having -- but the run must not end
    # quietly, so this is carried to the exit status below rather than left as a null in a column.
    if not budget and entry["resolved_budget"] is None:
        entry["b0_unresolved"] = True

    why = agrees(config, budget, policy)
    if why:
        entry["error"] = f"refused before replay: {why}"
        manifest["arms"].append(entry)
        print(f"REFUSED {name}: {why}", file=sys.stderr)
        continue

    # The trace arm name is the study's, and gen-trace validates it against the study registry.
    for rep in range(1, REPS + 1):
        trace = f"{OUT}/trace-{name}-{rep}.jsonl"
        mani = f"{OUT}/manifest-{name}-{rep}.yaml"
        raw = f"{OUT}/raw-{name}-{rep}.jsonl"
        gen = [HARNESS, "gen-trace", "--seed", "7", "--study", STUDY, "--arm", name,
               "--model", MODEL, "--gateway-url", BASE, "--engine-image", IMAGE,
               # The load is passed rather than defaulted. gen-trace's defaults are stub-calibrated and the
               # first pilot ran them at a GPU at ten times its prefill capacity.
               "--rate", os.environ["RATE"], "--duration-ms", os.environ["DURATION_MS"],
               "--premium-weight", os.environ["PREMIUM_WEIGHT"],
               "--noisy-weight", os.environ["NOISY_WEIGHT"],
               "--probe-weight", os.environ["PROBE_WEIGHT"],
               "--trace-out", trace, "--manifest-out", mani]
        r = subprocess.run(gen, capture_output=True)
        if r.returncode != 0:
            entry["error"] = "gen-trace: " + r.stderr.decode("utf-8", "replace")[-400:]
            break
        rep_cmd = [HARNESS, "replay", "--manifest", mani, "--target", BASE, "--raw-out", raw]
        if policy == "priority":
            rep_cmd += ["--priorities", PRIORITIES]
        r = subprocess.run(rep_cmd, capture_output=True)
        if r.returncode != 0:
            entry["error"] = "replay: " + r.stderr.decode("utf-8", "replace")[-400:]
            break
        entry.setdefault("raw", []).append(os.path.basename(raw))
    manifest["arms"].append(entry)

subprocess.run(["docker", "rm", "-f", "vllm"], capture_output=True)
with open(f"{OUT}/run.json", "w") as f:
    json.dump(manifest, f, indent=2)

# The exit status is what gates the completion marker. An arm that produced no rows is not a result.
served = [a for a in manifest["arms"] if a.get("raw")]
# B0 by construction: an arm that STATED its budget and produced the control's fingerprint was configured
# the way the control configures itself. This is the only route the engine leaves open -- see fingerprint().
stated = [a for a in manifest["arms"] if a.get("resolved_budget") and a.get("fingerprint")]
for a in manifest["arms"]:
    if not a.get("b0_unresolved") or not a.get("fingerprint"):
        continue
    if a["fingerprint"].get("compileRangeEndpoint") is None or a["fingerprint"].get("kvCacheTokens") is None:
        continue
    for m in stated:
        if m["fingerprint"] == a["fingerprint"]:
            a["resolved_budget"] = m["resolved_budget"]
            a["b0_resolved_by"] = f"fingerprint match with {m['arm']}, which stated {m['resolved_budget']}"
            a.pop("b0_unresolved", None)
            print(f"B0 RESOLVED for {a['arm']}: {a['b0_resolved_by']} ({a['fingerprint']})", file=sys.stderr)
            break

unresolved = [a["arm"] for a in manifest["arms"] if a.get("b0_unresolved")]
if unresolved:
    # Loud, and on stderr, because this is the pre-registration's own pilot gate: "reading 4 must not fire,
    # and B0 resolved". A control whose batch budget nobody can read is not the control the study defined,
    # and the 2026-09-07 pilot reported it as a null in a table and moved on.
    print(f"B0 UNRESOLVED for {', '.join(unresolved)}: the engine's own log does not state the batch budget "
          f"it chose. The rows are still evidence, but the study's control is undefined and the "
          f"confirmatory run must not be bought on this. See engine-log-*.txt.", file=sys.stderr)
print(json.dumps(manifest, indent=2))
sys.exit(0 if served else 1)
PYEOF

RUNSCRIPT=$(mktemp)
cat > "$RUNSCRIPT" <<'USERDATA'
#!/bin/bash
exec > >(tee /var/log/pop.log) 2>&1
set -x
( sleep BACKSTOP_SECONDS_PLACEHOLDER; shutdown -h now ) &

IMAGE="ENGINE_IMAGE_PLACEHOLDER"
MODEL="MODEL_PLACEHOLDER"
BUCKET="BUCKET_PLACEHOLDER"
PREFIX="RUN_ID_PLACEHOLDER"
HARNESS_SHA="HARNESS_SHA_PLACEHOLDER"

upload() { aws s3 cp "$1" "s3://$BUCKET/$PREFIX/$2" || true; }
trap 'upload /var/log/pop.log log.txt; shutdown -h now' EXIT

# The harness is verified before it is trusted. A truncated download that still executes produces numbers
# rather than an error, which is the failure this whole study exists to avoid.
aws s3 cp "s3://$BUCKET/$PREFIX/bin/benchharness" /usr/local/bin/benchharness
chmod +x /usr/local/bin/benchharness
got=$(sha256sum /usr/local/bin/benchharness | cut -d' ' -f1)
if [ "$got" != "$HARNESS_SHA" ]; then
  echo "harness checksum mismatch: expected $HARNESS_SHA, got $got"
  exit 1
fi

docker pull "$IMAGE"
mkdir -p /hf /tmp/evidence

rc=0
python3 /usr/local/bin/pop.py > /tmp/run-stdout.json 2>/tmp/pop.err || rc=$?
tar -czf /tmp/evidence.tgz -C /tmp evidence
upload /tmp/evidence.tgz evidence.tgz
upload /tmp/run-stdout.json run.json
upload /tmp/pop.err stderr.txt

# DONE means the run produced usable evidence, not that the script reached its last line.
if [ "$rc" -eq 0 ] && [ -s /tmp/evidence.tgz ]; then
  touch /tmp/DONE && upload /tmp/DONE DONE
else
  echo "no arm produced rows (exit $rc); DONE withheld"
fi
USERDATA

UD=$(mktemp)
{
  echo "#!/bin/bash"
  echo "mkdir -p /usr/local/bin"
  echo "cat > /usr/local/bin/pop.py <<'MEASUREEOF'"
  cat "$MEASURE"
  echo "MEASUREEOF"
  echo "export IMAGE='$ENGINE_IMAGE' MODEL='$MODEL' STUDY='$STUDY' ARMS='$ARMS' REPS='$REPS'"
  # The load travels with the run. Leaving these to gen-trace's stub-calibrated defaults is what the first
  # pilot did, and the instance is where that decision actually takes effect.
  echo "export RATE='$RATE' DURATION_MS='$DURATION_MS' PREMIUM_WEIGHT='$PREMIUM_WEIGHT' NOISY_WEIGHT='$NOISY_WEIGHT' PROBE_WEIGHT='$PROBE_WEIGHT'"
  sed -e "s|BACKSTOP_SECONDS_PLACEHOLDER|$BACKSTOP_SECONDS|" \
      -e "s|RUN_ID_PLACEHOLDER|$RUN_ID|" \
      -e "s|ENGINE_IMAGE_PLACEHOLDER|$ENGINE_IMAGE|" \
      -e "s|MODEL_PLACEHOLDER|$MODEL|" \
      -e "s|BUCKET_PLACEHOLDER|$BUCKET|" \
      -e "s|HARNESS_SHA_PLACEHOLDER|$HARNESS_SHA|" "$RUNSCRIPT" | tail -n +2
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
cp "$MEASURE" "$OUT/pop.py"

# ---------------------------------------------------------------- launch
say "launching $INSTANCE_TYPE spot (max \$$MAX_SPOT_PRICE/h)"
TAGS="ResourceType=instance,Tags=[{Key=Name,Value=$STACK},{Key=purpose,Value=price-of-protection}]"
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
        "$MAX_SPOT_PRICE" 200 "$UD" "$TAGS" 2>>"$OUT/launch-errors.txt") \
    && [ -n "$IID" ] && [ "$IID" != "None" ] && break
  IID=""
done
[ -n "$IID" ] || fail "no zone would launch $INSTANCE_TYPE; see $OUT/launch-errors.txt"
echo "$IID" > "$OUT/instance-id"
say "instance $IID"


say "waiting for results (the engine has an image and weights to pull first)"
done_seen=0
ended_early=""
marker_rc=0
# The watcher has to outlast the instance's own backstop, or it terminates a run that was still working.
# It was a fixed 160 polls at 30 s -- 80 minutes -- against a BACKSTOP_SECONDS of two hours, so the two
# disagreed by forty minutes and the shorter one owned the trap. The pre-registration's own runtime estimate
# puts the four-arm pilot near seventy minutes, which is inside that gap: the run would have been killed and
# the card time spent for nothing. Deriving the count from the backstop is what keeps them from drifting
# apart again, and the margin is for the upload the instance does after its backstop fires.
POLL_INTERVAL=30
POLL_ATTEMPTS=$(( BACKSTOP_SECONDS / POLL_INTERVAL + 10 ))
say "watching for up to $(( POLL_ATTEMPTS * POLL_INTERVAL / 60 )) min against a $(( BACKSTOP_SECONDS / 60 )) min backstop"
ended_early=$(spot_wait_for_marker "$REGION" "$BUCKET" "$RUN_ID/DONE" "$IID" "$POLL_ATTEMPTS" "$POLL_INTERVAL") || marker_rc=$?
case "$marker_rc" in
  0) say "results are up"; done_seen=1 ;;
  2) say "instance ended before writing DONE" ;;
esac

for k in evidence.tgz run.json log.txt stderr.txt; do
  aws s3 cp "s3://$BUCKET/$RUN_ID/$k" "$OUT/$k" >/dev/null 2>&1 || true
done

if [ "$done_seen" -eq 0 ]; then
  if [ -n "$ended_early" ]; then
    fail "the instance was $ended_early before it wrote its completion marker; $OUT/log.txt is whatever it managed to upload"
  fi
  fail "no completion marker after 160 polls; the run either is still going or hung, and $IID has been terminated"
fi

[ -s "$OUT/evidence.tgz" ] || fail "no evidence archive was written; $OUT/log.txt may say why"
tar -xzf "$OUT/evidence.tgz" -C "$OUT" && say "evidence unpacked to $OUT/evidence"

say "resolved engine configuration per arm"
python3 - "$OUT/run.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(f"{'arm':<20} {'asked':>16} {'resolved':>10}  {'reps':>4}  note")
for a in d.get("arms", []):
    note = a.get("error", "")
    if a.get("b0_unresolved"):
        note = (note + "  " if note else "") + "B0 UNRESOLVED -- this arm is the control and its budget was not readable"
    print(f"{a['arm']:<20} {a.get('requested_budget',''):>16} {str(a.get('resolved_budget')):>10}"
          f"  {len(a.get('raw',[])):>4}  {note}")
# The pilot's gate, restated where the operator is looking. A run that cannot say what its control was
# configured as cannot buy the confirmatory run, however good the numbers above look.
if any(a.get("b0_unresolved") for a in d.get("arms", [])):
    print("\nPILOT GATE NOT MET: B0 is unresolved, so the control is undefined. The measurements stand;\n"
          "the confirmatory run does not follow from them.", file=sys.stderr)
PY

say "the report is NOT run here: it needs every arm of the study and this may be a pilot"
say "when the arms are complete:  benchharness report --raw '$OUT/evidence/raw-*.jsonl' -json-out ..."
