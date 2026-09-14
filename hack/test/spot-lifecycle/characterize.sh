#!/usr/bin/env bash
#
# Pins the observable behaviour of a single-instance GPU runner so a refactor can be shown to have
# changed nothing.
#
# WHY THIS EXISTS
#
# hack/m5b-scheduler-microtest.sh and the price-of-protection runner share about 150 lines of EC2
# lifecycle: results bucket, instance profile, AMI and subnet discovery, Spot launch with cross-zone
# retry, completion polling, teardown. That is being extracted into hack/lib/spot-run.sh. The obvious
# check -- diff the generated user-data before and after -- proves only that the remote workload is
# unchanged, and the remote workload is not the part being moved. Launch arguments, zone retry, stale
# completion markers and teardown are, and they are the parts that cost money when they break.
#
# So each scenario runs the real script with `aws` and `sleep` replaced by recording stubs, and
# compares the resulting call transcript against a golden file. A refactor that preserves behaviour
# produces no diff.
#
# WHAT IT CANNOT DO
#
# Everything past the AWS API boundary: cloud-init, IAM propagation delay, real Spot capacity, GPU
# readiness, whether the instance can actually reach S3. A green run here is a necessary condition for
# a safe refactor and not a substitute for one real launch.
set -uo pipefail

cd "$(dirname "$0")/../../.." || exit 1
ROOT=$(pwd)
HERE="hack/test/spot-lifecycle"

# The script under test. Overridable so the same driver can pin more than one runner.
TARGET="${TARGET:-hack/m5b-scheduler-microtest.sh}"
# One golden directory per runner, named after it. Two runners sharing a directory would collide on
# scenario names that mean different things, and a stale golden from the other runner would either fail
# for the wrong reason or, worse, pass.
SUITE=$(basename "$TARGET" .sh)
GOLDEN="$HERE/golden/$SUITE"
mkdir -p "$GOLDEN"

# Without the stubs this script runs the REAL aws against whatever credentials are loaded.
#
# It only prepends a directory to PATH, so a missing or non-executable stub is not an error, it is a
# silent change of subject: every scenario would launch real instances or fail with real API errors, and
# the transcript would be empty rather than wrong. The stubs also live under a path that .gitignore's
# "bin/" rule swallowed, so "the files are simply absent" is a state this has actually been in.
for stub in aws sleep; do
  [ -x "$(dirname "$0")/bin/$stub" ] || {
    printf 'FAIL: %s/bin/%s is missing or not executable; refusing to run against the real one\n' \
      "$(dirname "$0")" "$stub" >&2
    exit 2
  }
done

UPDATE=0
# Whether this --update run has already refreshed the shared user-data golden; see run_scenario.
ud_shared_refreshed=0
ONLY=""
for a in "$@"; do
  case "$a" in
    --update) UPDATE=1 ;;
    --only=*) ONLY="${a#--only=}" ;;
    *) printf 'usage: %s [--update] [--only=<scenario>]\n' "$0" >&2; exit 2 ;;
  esac
done

pass=0; fail=0
# Counted so that --only with a misspelled scenario name cannot report "0 passed, 0 failed" and exit 0,
# which is the same sentence a completely green run would print if there were no scenarios left.
selected=0

# Each scenario is a name followed by the stub environment that defines it.
#
# The names say what condition is being pinned, not what the script is expected to do -- the golden
# file is what says that, and it was recorded from the script rather than written by hand.
run_scenario() {
  local name="$1"; shift
  if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then return 0; fi
  selected=$((selected + 1))

  local work; work=$(mktemp -d)
  local out="$work/run"
  mkdir -p "$out"

  export STUB_STATE="$work/state"
  export STUB_TRANSCRIPT="$work/transcript.txt"
  export STUB_OUT="$out"
  : > "$STUB_TRANSCRIPT"

  # The stubs shadow the real binaries by sitting first on PATH.
  local rc=0
  (
    export PATH="$ROOT/$HERE/bin:$PATH"
    export OUT="$out"
    export AWS_REGION=ap-northeast-2
    export BUCKET=stub-bucket
    # A credential cache with a long life, because the runners refuse to launch on credentials that would
    # expire before the run finishes, and that refusal reads the cache on disk rather than the stubbed CLI.
    #
    # Supplied here rather than weakened there. A skip switch inside a check that exists to stop a GPU
    # billing unattended is a switch that eventually gets set in the wrong place. AWS_CLI_CACHE_DIR only
    # says WHERE to look; the check still runs and still refuses. HOME is left alone because the runners
    # build Go binaries and the build cache lives under it.
    export AWS_CLI_CACHE_DIR="$work/awscache"
    mkdir -p "$AWS_CLI_CACHE_DIR"
    printf '{"Credentials":{"Expiration":"%s"}}\n' \
      "$(date -u -d '+12 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+12H +%Y-%m-%dT%H:%M:%SZ)" \
      > "$AWS_CLI_CACHE_DIR/stub.json"
    "$@" >"$work/stdout.txt" 2>"$work/stderr.txt"
  ) || rc=$?

  # The shipped binary's checksum changes with every Go edit, so it is normalized out of the user-data
  # golden and the property it exists for is asserted directly: the checksum the instance will verify must
  # be the checksum of the binary that was actually uploaded. Goldening the digit string instead would make
  # any change to cmd/benchharness break this suite for no behavioural reason, and a golden that breaks for
  # no reason is one that gets regenerated without being read.
  if [ -f "$out/user-data.sh" ] && [ -f "$out/benchharness" ]; then
    local embedded built
    embedded=$(grep -oE 'HARNESS_SHA="[0-9a-f]{64}"' "$out/user-data.sh" | head -1 | cut -d'"' -f2)
    built=$(sha256sum "$out/benchharness" | cut -d' ' -f1)
    if [ -n "$embedded" ] && [ "$embedded" = "$built" ]; then
      printf 'harness checksum: user-data matches the uploaded binary\n' >> "$STUB_TRANSCRIPT"
    else
      printf 'harness checksum: MISMATCH (user-data %s, binary %s)\n' "${embedded:-none}" "$built" >> "$STUB_TRANSCRIPT"
    fi
    sed -i 's/HARNESS_SHA="[0-9a-f]\{64\}"/HARNESS_SHA="<SHA>"/' "$out/user-data.sh"
  fi

  # The same property for the gateway, which hack/m5c-gpu-session.sh also builds here and ships.
  #
  # Added after its digit string landed in the goldens unnormalized, which would have broken this suite on
  # every unrelated change to cmd/gateway. What is worth pinning is that the checksum the instance will
  # verify is the checksum of the binary that was uploaded, not the value itself.
  if [ -f "$out/user-data.sh" ] && [ -f "$out/gateway" ]; then
    local gembedded gbuilt
    gembedded=$(grep -oE 'GATEWAY_SHA="[0-9a-f]{64}"' "$out/user-data.sh" | head -1 | cut -d'"' -f2)
    gbuilt=$(sha256sum "$out/gateway" | cut -d' ' -f1)
    if [ -n "$gembedded" ] && [ "$gembedded" = "$gbuilt" ]; then
      printf 'gateway checksum: user-data matches the uploaded binary\n' >> "$STUB_TRANSCRIPT"
    else
      printf 'gateway checksum: MISMATCH (user-data %s, binary %s)\n' "${gembedded:-none}" "$gbuilt" >> "$STUB_TRANSCRIPT"
    fi
    sed -i 's/GATEWAY_SHA="[0-9a-f]\{64\}"/GATEWAY_SHA="<SHA>"/' "$out/user-data.sh"
  fi

  # The same property for the other runner's binary: queuelab ships queuelabrun rather than building it on a
  # box that has no Go, and what the instance verifies must be what was uploaded.
  if [ -f "$out/user-data.sh" ] && [ -f "$out/queuelabrun" ]; then
    local rembedded rbuilt
    rembedded=$(grep -oE 'RUNNER_SHA="[0-9a-f]{64}"' "$out/user-data.sh" | head -1 | cut -d'"' -f2)
    rbuilt=$(sha256sum "$out/queuelabrun" | cut -d' ' -f1)
    if [ -n "$rembedded" ] && [ "$rembedded" = "$rbuilt" ]; then
      printf 'queuelabrun checksum: user-data matches the uploaded binary\n' >> "$STUB_TRANSCRIPT"
    else
      printf 'queuelabrun checksum: MISMATCH (user-data %s, binary %s)\n' "${rembedded:-none}" "$rbuilt" >> "$STUB_TRANSCRIPT"
    fi
    sed -i 's/RUNNER_SHA="[0-9a-f]\{64\}"/RUNNER_SHA="<SHA>"/' "$out/user-data.sh"
  fi

  # A commit hash and the checksum of an archive of it change with every commit, and neither is behaviour.
  # The goldens recorded before this normalization existed encoded the tree state of the machine that
  # recorded them, so a clean checkout failed all nine scenarios twice over -- a golden that fails for a
  # reason unrelated to what it pins is one that gets regenerated without being read.
  if [ -f "$out/user-data.sh" ]; then
    sed -i -e 's/SOURCE_SHA="[0-9a-f]\{64\}"/SOURCE_SHA="<SHA256>"/' \
           -e 's/COMMIT="[0-9a-f]\{40\}"/COMMIT="<COMMIT>"/' "$out/user-data.sh"
  fi

  # What the script SAID is part of the golden, not only what it called and what it returned.
  #
  # Without this, replacing the results check `[ -s ]` with `[ -f ]` -- accepting an empty results file
  # as a result -- produced no diff at all: the run still exited non-zero, because the reading printer
  # then died on the empty file instead. Same exit status, completely different meaning, and the run
  # would have printed a table from nothing on any input that merely parsed. An exit code is a very
  # coarse description of what a script did.
  #
  # Volatile substrings are replaced so two runs of one scenario agree.
  # The shipped binary's checksum is elided here for the third and last time: it is in the user-data, in
  # the upload line, and in what the runner prints. It changes with every Go edit and is not behaviour, and
  # the property it stands for -- that the checksum the instance verifies is the checksum of the binary
  # that was uploaded -- is asserted directly above and stays in the golden.
  #
  # Anchored to the line that carries it, NOT to the shape of a hex digest. The first version matched any
  # 64 hex characters and erased the engine image digest out of the microtest's messages, which is the one
  # string in that log identifying what was measured. The other suite failed immediately, which is the
  # harness catching an over-broad normalization in itself.
  sed -e "s#$out#<OUT>#g" \
      -e "s#${TMPDIR:-/tmp}/tmp\.[A-Za-z0-9]*#<TMP>#g" \
      -e "s#/tmp/tmp\.[A-Za-z0-9]*#<TMP>#g" \
      -e "s#harness sha256 [0-9a-f]\{64\}#harness sha256 <SHA256>#g" \
      -e "s#^== queuelabrun .*, sha256 [0-9a-f]\{12\}\$#== queuelabrun <SIZE>, sha256 <SHA12>#" \
      -e "s#^==   gateway [0-9a-f]\{12\}, benchharness [0-9a-f]\{12\}\$#==   gateway <SHA12>, benchharness <SHA12>#" \
      -e "s#^== source [0-9a-f]\{40\}.*#== source <COMMIT> <TREE STATE>#" \
      "$work/stdout.txt" "$work/stderr.txt" > "$work/messages.txt"

  # The transcript alone would hide a script that recorded every AWS call correctly and then exited
  # non-zero, or one that stopped writing its run directory. Both are behaviour.
  {
    # Repeating poll cycles are folded to one copy plus a count, so a 120-poll timeout is readable in
    # a diff without losing the count itself.
    python3 "$ROOT/$HERE/collapse.py" < "$STUB_TRANSCRIPT"
    printf -- '--- exit %s\n' "$rc"
    printf -- '--- messages\n'
    cat "$work/messages.txt"
    printf -- '--- files\n'
    ( cd "$out" && find . -type f | LC_ALL=C sort )
  } > "$work/actual.txt"

  local golden="$GOLDEN/$name.txt"
  if [ "$UPDATE" = "1" ]; then
    cp "$work/actual.txt" "$golden"
    printf 'RECORDED %s\n' "$name"
  elif [ ! -f "$golden" ]; then
    printf 'FAIL     %s (no golden; run with --update to record)\n' "$name"
    fail=$((fail + 1))
  elif diff -u "$golden" "$work/actual.txt" > "$work/diff.txt"; then
    printf 'ok       %s\n' "$name"
    pass=$((pass + 1))
  else
    printf 'FAIL     %s\n' "$name"
    sed -n '1,60p' "$work/diff.txt"
    fail=$((fail + 1))
  fi

  # The generated user-data is goldened separately, because it is the one artifact whose exact bytes
  # reach the instance, and because the transcript deliberately reduces it to a placeholder path.
  #
  # One file for every scenario, not one per scenario: the remote workload does not depend on which
  # zone answered or whether the bucket already existed, so eight copies would only mean eight places
  # for a real change to hide in a diff nobody reads.
  #
  # Except when a scenario changes the workload ON PURPOSE. The idling study renders a different STUDY into
  # the user-data, and under one shared golden that is indistinguishable from a regression: the suite failed
  # with "user-data changed" and the change was the thing being tested. A scenario that produces different
  # bytes gets its own file, named after it, and every scenario that does not keeps sharing one -- so the
  # property the comment above describes survives for the scenarios it was written about.
  if [ -f "$out/user-data.sh" ]; then
    local udg="$GOLDEN/user-data.sh"
    if [ -f "$GOLDEN/user-data-$name.sh" ]; then
      udg="$GOLDEN/user-data-$name.sh"
    fi
    if [ "$UPDATE" = "1" ]; then
      # A scenario whose user-data differs from the shared golden records its own, and one that matches does
      # not -- so a scenario stops having a private golden the moment it stops needing one.
      #
      # THE FIRST SCENARIO OF AN UPDATE REFRESHES THE SHARED GOLDEN instead, and that exception is what makes
      # the rule work. Without it, a change to the runner leaves the shared golden stale, every scenario then
      # differs from it, and every scenario gets a private copy -- seven near-identical files plus an orphan
      # nothing reads. That happened twice while this suite was being written, and both times the stale file
      # was the one an operator would have opened to check the payload.
      if [ "$udg" = "$GOLDEN/user-data.sh" ] && [ -f "$udg" ] \
         && ! diff -q "$udg" "$out/user-data.sh" >/dev/null; then
        if [ "$ud_shared_refreshed" = "0" ]; then
          ud_shared_refreshed=1
          rm -f "$GOLDEN"/user-data-*.sh
        else
          udg="$GOLDEN/user-data-$name.sh"
        fi
      fi
      cp "$out/user-data.sh" "$udg"
    elif [ ! -f "$udg" ]; then
      # Absent is not "nothing to compare". Deleting the golden used to turn this check off without
      # saying so, which is the difference between a check that passed and a check that did not run.
      printf 'FAIL     %s (no user-data golden; run with --update to record)\n' "$name"
      fail=$((fail + 1))
    elif ! diff -u "$udg" "$out/user-data.sh" > "$work/ud.diff"; then
      printf 'FAIL     %s (user-data changed)\n' "$name"
      sed -n '1,40p' "$work/ud.diff"
      fail=$((fail + 1))
    fi
  fi

  rm -rf "$work"
}

# --- the scenarios ---------------------------------------------------------------------------------
#
# One set per runner. Selected by suite rather than run unconditionally, because the scenarios encode what
# a particular runner does: pointing TARGET at something else and running the microtest's scenarios against
# it would compare a runner to another runner's expectations and call the difference a regression.

scenarios_microtest() {
# Nothing exists yet: the bucket and the instance profile are both created on the way through.
STUB_BUCKET_EXISTS=0 STUB_PROFILE_EXISTS=0 STUB_DONE_AFTER=2 \
  STUB_PRESENT_KEYS="results.json log.txt stderr.txt" \
  run_scenario fresh bash "$TARGET"

# The steady state after the first run: both already exist, so neither is touched.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
  STUB_PRESENT_KEYS="results.json log.txt stderr.txt" \
  run_scenario existing bash "$TARGET"

# Spot has no capacity in the first zone. "No capacity right now" is a normal answer, not a fault, and
# the run has to continue into the next zone rather than abort.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
  STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a" STUB_PRESENT_KEYS="results.json log.txt stderr.txt" \
  run_scenario zone-retry bash "$TARGET"

# The terminate call is refused, the way it is when credentials lapse mid-run.
#
# spot_terminate used to end in `>/dev/null 2>&1 || true`, so this scenario would have printed "terminating
# i-0stub" and said nothing else while the instance went on billing. That happened on 2026-09-08 with a real
# instance. The golden transcript is what makes the fix's failure branch executed rather than asserted.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
  STUB_TERMINATE_FAILS=1 STUB_PRESENT_KEYS="results.json log.txt stderr.txt" \
  run_scenario terminate-refused bash "$TARGET"

# No zone will take it. This must fail loudly and must not leave an instance behind.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 \
  STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a ap-northeast-2c" \
  run_scenario all-zones-fail bash "$TARGET"

# One of the zones offers the instance type but has no default public subnet, so it is skipped.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
  STUB_NO_SUBNET_ZONES="ap-northeast-2a" STUB_PRESENT_KEYS="results.json log.txt stderr.txt" \
  run_scenario no-subnet-in-zone bash "$TARGET"

# The instance dies before it writes its completion marker -- a reclaimed Spot node.
# Only the log made it up, via the remote trap. There are no results to download, and a harness that
# invented some would report a dead instance as a completed run.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 STUB_TERMINATED_AFTER=2 \
  STUB_PRESENT_KEYS="log.txt" \
  run_scenario instance-died bash "$TARGET"

# The marker arrives but the results file is empty. The run produced nothing and must say so.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 STUB_RESULTS_EMPTY=1 \
  STUB_PRESENT_KEYS="results.json log.txt stderr.txt" \
  run_scenario empty-results bash "$TARGET"

# The marker never arrives and the instance stays up: the poll loop runs to exhaustion.
#
# Added because none of the other scenarios reaches the end of the loop, so its length was untested --
# replacing 120 attempts with 2 left every transcript identical while killing a real run that was still
# pulling fifteen gigabytes of weights.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 \
  STUB_PRESENT_KEYS="log.txt" \
  run_scenario poll-exhausted bash "$TARGET"

# THE ONE THAT MATTERS: the completion marker is already in the bucket from a PREVIOUS run.
#
# The results bucket keeps objects for 30 days and the keys are fixed at the root, so a second run of
# this script finds the first run's DONE on its first poll. Whatever this scenario records is what the
# script does today; it is pinned here so that fixing it is a visible, deliberate diff.
STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_PRESENT_AT_START=1 \
  run_scenario stale-done bash "$TARGET"
}

scenarios_price_of_protection() {
  # The pilot's four arms, everything present, results returned.
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_PRESENT_KEYS="evidence.tgz run.json log.txt stderr.txt" \
    run_scenario pilot bash "$TARGET"

  # First run in a fresh account: bucket and profile are created, and the profile must carry GetObject
  # because this runner's instance downloads the binary it was sent.
  STUB_BUCKET_EXISTS=0 STUB_PROFILE_EXISTS=0 STUB_DONE_AFTER=2 \
    STUB_PRESENT_KEYS="evidence.tgz run.json log.txt stderr.txt" \
    run_scenario fresh bash "$TARGET"

  # Spot has no capacity in the first zone.
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a" \
    STUB_PRESENT_KEYS="evidence.tgz run.json log.txt stderr.txt" \
    run_scenario zone-retry bash "$TARGET"

  # The instance dies before finishing. Only the log made it up, so there is no archive to unpack and the
  # run must refuse rather than report on an empty directory.
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 STUB_TERMINATED_AFTER=2 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario instance-died bash "$TARGET"

  # The marker never arrives and the instance stays up: the poll runs to exhaustion.
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario poll-exhausted bash "$TARGET"

  # The marker arrives but the archive is empty: the instance said it finished and sent nothing usable.
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 STUB_EMPTY_ARCHIVE=1 \
    STUB_PRESENT_KEYS="evidence.tgz run.json log.txt stderr.txt" \
    run_scenario empty-archive bash "$TARGET"

  # A previous run's marker sits at the bucket root. This runner scopes its keys, so it must not see it.
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_PRESENT_AT_START=1 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario stale-done bash "$TARGET"
}

scenarios_m5c_gpu_session() {
  # The archive this runner unpacks holds one raw file per arm, and the session refuses a run whose arms did
  # not all come back. The stub is told which arms to produce so that it makes what the instance makes.
  export STUB_EVIDENCE_ARMS="R1 shared timeSlicing mps"
  # The commit the session will ship, so its evidence-identity check has something that matches.
  STUB_COMMIT=$(git -C "$ROOT" rev-parse HEAD); export STUB_COMMIT

  # This runner rents ONE card and builds a kind cluster on it, so its scenarios are about the lifecycle
  # around that: what it refuses before spending, and what it does when the instance does not come back.
  #
  # REPS is set on every scenario because the runner refuses without it -- a pilot is one repetition and a
  # confirmatory run is three, and neither is a thing to arrive at by forgetting a variable. The refusal
  # itself is the first scenario, because it is the only guard that runs before AWS is touched at all.
  run_scenario no-reps bash "$TARGET"

  # The provenance guard, made to fire rather than assumed to. A probe file makes the tree dirty and is
  # removed whatever happens; without it this scenario means nothing on a clean checkout.
  local probe="$ROOT/.characterize-dirty-probe"
  printf 'written by the characterization harness to make the tree dirty on purpose\n' > "$probe"
  REPS=1 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 \
    run_scenario dirty-tree bash "$TARGET"
  rm -f "$probe"

  # The pilot: one repetition of every arm, everything present, evidence comes back.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_PRESENT_KEYS="evidence.tgz log.txt commit.txt nodes.txt" \
    run_scenario pilot bash "$TARGET"

  # A fresh account: the bucket and the profile are created, and the profile must carry GetObject because
  # this instance downloads the source archive and both binaries it was sent.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=0 STUB_PROFILE_EXISTS=0 STUB_DONE_AFTER=2 \
    STUB_PRESENT_KEYS="evidence.tgz log.txt commit.txt nodes.txt" \
    run_scenario fresh bash "$TARGET"

  # No capacity in the first zone, which is a normal answer and must not abort the run.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a" \
    STUB_PRESENT_KEYS="evidence.tgz log.txt commit.txt nodes.txt" \
    run_scenario zone-retry bash "$TARGET"

  # The terminate call is refused, the way it is when credentials lapse mid-run. The transcript is what
  # makes the SHOUTING branch executed rather than asserted; on 2026-09-08 the silent version of this let a
  # real instance bill until somebody noticed.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_TERMINATE_FAILS=1 STUB_PRESENT_KEYS="evidence.tgz log.txt commit.txt nodes.txt" \
    run_scenario terminate-refused bash "$TARGET"

  # The instance is reclaimed before it writes its marker. Only the log made it up, so there is no archive
  # to unpack and the run must refuse rather than report on an empty directory.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 \
    STUB_TERMINATED_AFTER=2 STUB_PRESENT_KEYS="log.txt" \
    run_scenario instance-died bash "$TARGET"

  # The marker never arrives and the instance stays up: the poll runs to exhaustion. A 25-minute bring-up
  # means this loop is longer here than anywhere else, and its length has to be exercised rather than
  # trusted -- shortening it would kill a run that is still pulling fifteen gigabytes of weights.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario poll-exhausted bash "$TARGET"

  # A previous run's marker at the bucket root. This runner scopes its keys under the run id, so it must
  # not see it -- the failure it protects against is downloading the previous run's evidence and reporting
  # it as this run's.
  REPS=1 REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_PRESENT_AT_START=1 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario stale-done bash "$TARGET"
}

scenarios_queuelab_gpu_session() {
  # Every scenario below dirty-tree sets REQUIRE_CLEAN_TREE=0, so none of them depends on whether the
  # person running the suite has uncommitted work. The provenance line they print is normalized for the
  # same reason: what it says about a commit belongs to the run, not to this suite.

  # The refusal that protects the provenance, asserted first because it is the one thing this runner does
  # before it touches AWS at all. Everything below overrides it, and the override prints a line saying so.
  #
  # The scenario MAKES the tree dirty rather than assuming it is. The first version of this suite was
  # recorded on a working tree that happened to be dirty, so the goldens encoded that fact and a clean
  # checkout failed all nine of them -- and this scenario in particular cannot mean anything on a clean
  # tree, because there would be nothing for the guard to refuse. A probe file is written into the
  # repository and removed whatever happens.
  #
  # WHAT THIS SCENARIO CANNOT DISTINGUISH. The probe is UNTRACKED, and the guard's two candidate
  # implementations differ only on untracked files: `git diff --quiet` does not see them, `git status
  # --porcelain` does. So this scenario separates the two only when the probe is the tree's ONLY change.
  # Run it while tracked files are also modified -- which is most of the time during development -- and
  # both implementations refuse, for different reasons, and the golden cannot tell them apart. The weaker
  # implementation shipped and was found by hand rather than here.
  local probe="$ROOT/.characterize-dirty-probe"
  printf 'written by the characterization harness to make the tree dirty on purpose\n' > "$probe"
  STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 \
    run_scenario dirty-tree bash "$TARGET"
  rm -f "$probe"

  # The pilot: everything present, records come back.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_PRESENT_KEYS="session.tgz commit.txt log.txt runs" \
    run_scenario pilot bash "$TARGET"

  # A fresh account. The instance profile must carry GetObject, because this instance downloads the source
  # archive it was sent.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=0 STUB_PROFILE_EXISTS=0 STUB_DONE_AFTER=2 \
    STUB_PRESENT_KEYS="session.tgz commit.txt log.txt runs" \
    run_scenario fresh bash "$TARGET"

  # Spot has no capacity in the first zone, which at a placement score of 3 is the expected answer rather
  # than a fault.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 \
    STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a" \
    STUB_PRESENT_KEYS="session.tgz commit.txt log.txt runs" \
    run_scenario zone-retry bash "$TARGET"

  # THE ONE THIS RUNNER EXISTS TO SURVIVE: the instance is reclaimed partway. The watcher shipped records
  # as they landed, so the refusal must report how many were recovered rather than treating the session as
  # a total loss.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 STUB_TERMINATED_AFTER=2 \
    STUB_PRESENT_KEYS="log.txt runs" \
    run_scenario interrupted-with-records bash "$TARGET"

  # Reclaimed with nothing shipped at all.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 STUB_TERMINATED_AFTER=2 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario interrupted-with-nothing bash "$TARGET"

  # The marker never arrives and the instance stays up: the hard stop is what ends this.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=-1 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario poll-exhausted bash "$TARGET"

  # The marker arrives and the archive is empty.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_AFTER=2 STUB_EMPTY_ARCHIVE=1 \
    STUB_PRESENT_KEYS="session.tgz commit.txt log.txt runs" \
    run_scenario empty-archive bash "$TARGET"

  # The idling study's own path, pinned so a paid session is not the first thing to execute it.
  #
  # It changes the banner, the arms gpu-session.sh will run, and the STUDY value embedded in the user-data.
  # None of those is exercised by the default session, and the last one is the kind of substitution that
  # silently renders an empty value.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUDY=idling \
    run_scenario idling-study bash "$TARGET"

  # An unknown study is refused before anything is uploaded or launched.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUDY=sideways \
    run_scenario unknown-study bash "$TARGET"

  # The user-data outgrew what EC2 accepts, and the runner has to say so before it calls RunInstances.
  #
  # It did not, once: comments pushed the encoded script past 25600 bytes, three zones each answered
  # InvalidParameterValue, and the refusal that fired said only that the errors named neither authorization
  # nor capacity. True, and no help. UD_LIMIT drives the guard here rather than inflating the script.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 UD_LIMIT=100 \
    run_scenario user-data-too-large bash "$TARGET"

  # No zone will take it, for the two reasons that end the same way and call for opposite responses. The
  # real session hit the first of these and the refusal named the second, attributing a policy denial to
  # Spot capacity. Both are pinned now.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 \
    STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a ap-northeast-2c" STUB_LAUNCH_FAILURE=unauthorized \
    run_scenario all-zones-unauthorized bash "$TARGET"

  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 \
    STUB_LAUNCH_FAIL_ZONES="ap-northeast-2a ap-northeast-2c" STUB_LAUNCH_FAILURE=capacity \
    run_scenario all-zones-no-capacity bash "$TARGET"

  # A previous session's marker at the bucket root. This runner scopes its keys and must not see it.
  REQUIRE_CLEAN_TREE=0 STUB_BUCKET_EXISTS=1 STUB_PROFILE_EXISTS=1 STUB_DONE_PRESENT_AT_START=1 \
    STUB_PRESENT_KEYS="log.txt" \
    run_scenario stale-done bash "$TARGET"
}

# The cleanup trap must be armed BEFORE anything can launch an instance.
#
# This is a source-order assertion rather than a scenario, and deliberately so: the window it guards is the
# one where the script DIES between run-instances returning an id and the trap being armed, and a golden
# transcript cannot see it. Moving the trap back below the launch leaves every scenario passing -- the AWS
# calls and the messages are identical -- while a SIGPIPE, a failed write of the instance-id file, or a
# Ctrl-C in that window strands a GPU instance with nothing to terminate it. So the order is checked where
# the order lives.
trap_armed_before_launch() {
  local f="$ROOT/$TARGET" t l
  t=$(grep -n '^trap cleanup EXIT INT TERM' "$f" | head -1 | cut -d: -f1)
  l=$(grep -n 'spot_launch "$REGION"' "$f" | head -1 | cut -d: -f1)
  if [ -z "$t" ] || [ -z "$l" ]; then
    printf 'FAIL     trap-before-launch (no trap or no launch found in %s)\n' "$TARGET"
    fail=$((fail + 1))
  elif [ "$t" -lt "$l" ]; then
    printf 'ok       the cleanup trap is armed before the launch\n'
    pass=$((pass + 1))
  else
    printf 'FAIL     the cleanup trap is armed at line %s, AFTER the launch at line %s; an exit in between strands the instance\n' "$t" "$l"
    fail=$((fail + 1))
  fi
}
trap_armed_before_launch

case "$SUITE" in
  m5b-scheduler-microtest)  scenarios_microtest ;;
  m5b-price-of-protection)  scenarios_price_of_protection ;;
  queuelab-gpu-session)     scenarios_queuelab_gpu_session ;;
  m5c-gpu-session)          scenarios_m5c_gpu_session ;;
  *) printf 'FAIL: no scenarios defined for %s\n' "$SUITE" >&2; exit 2 ;;
esac

if [ "$selected" -eq 0 ]; then
  printf 'FAIL: --only=%s matched no scenario\n' "$ONLY" >&2
  exit 2
fi

printf '\n%s: %d passed, %d failed\n' "$SUITE" "$pass" "$fail"
[ "$fail" -eq 0 ]
