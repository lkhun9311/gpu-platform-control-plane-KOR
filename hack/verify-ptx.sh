#!/usr/bin/env bash
# Compile the shipped PTX and record that it compiled, against the exact text that was compiled.
#
# The test that checks this used to shell out to ptxas and SKIP when there was none. A check that disappears
# on every machine without a CUDA toolkit is not a check: a green suite proved nothing about the one thing
# the file claimed could be verified without a GPU, and the claim sat in a comment for weeks before anybody
# noticed there was no such check at all.
#
# So the verification is done here, deliberately, and its result is stored keyed to a hash of the PTX itself
# -- the same shape as the termination canary, which stores a qualification keyed to what it qualified and
# refuses when the key moves. The test then requires the stored attestation to match the PTX in the tree. It
# does not prove ptxas ran on the machine running the test; it proves that this exact text compiled somewhere
# and that nobody has edited it since. That is a weaker claim than the test used to imply and a stronger one
# than it delivered.
#
# ptxas is used from PATH when present, and otherwise from the pinned python image with NVIDIA's nvcc wheel,
# which needs no GPU.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=internal/queuelab/testdata/ptx-verification.json
TARGETS="${TARGETS:-sm_75 sm_86 sm_89 sm_90}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The PTX is written and hashed by python and never round-tripped through the shell. Command substitution
# strips trailing newlines, so `HASH=$(printf %s "$PTX" | sha256sum)` hashed a string one byte shorter than
# the kernel -- the attestation then never matched the tree and the test refused it, which is the test doing
# exactly its job on its first run.
HASH="$(python3 - "$TMP/burn.ptx" <<'PY'
import hashlib, sys
src = open('internal/queuelab/submit.go').read()
if 'PTX=b"""' not in src:
    sys.exit('no PTX found in internal/queuelab/submit.go')
body = src.split('PTX=b"""', 1)[1].split('"""', 1)[0]
open(sys.argv[1], 'w').write(body)
print(hashlib.sha256(body.encode()).hexdigest())
PY
)"

if command -v ptxas >/dev/null 2>&1; then
  VERSION="$(ptxas --version | tr '\n' ' ')"
  for A in $TARGETS; do
    ptxas -arch="$A" -o /dev/null "$TMP/burn.ptx"
    echo "  compiled for $A"
  done
else
  echo "no local ptxas; using the pinned python image with NVIDIA's nvcc wheel (no GPU needed)"
  IMG="python@sha256:2c941e860699f878900b0edc2403613c234d4b32eda3cc9fa7036991a2a63c4a"
  VERSION="$(docker run --rm -v "$TMP:/w" -w /w "$IMG" sh -c '
    pip install -q nvidia-cuda-nvcc-cu12 2>/dev/null
    P=$(find / -name ptxas -type f 2>/dev/null | head -1)
    "$P" --version | tr "\n" " "')"
  # set -e INSIDE the container, and no `&& echo`. Without both, a failed target is swallowed: the loop's
  # status is its last iteration's, so sm_50 failing and sm_75 succeeding exits 0 and this script writes an
  # attestation listing an architecture that did not compile. Verified by execution -- a PTX that fails
  # sm_50 and passes sm_75 exited 0 through the old form.
  docker run --rm -v "$TMP:/w" -w /w "$IMG" sh -c "
    set -e
    pip install -q nvidia-cuda-nvcc-cu12 2>/dev/null
    P=\$(find / -name ptxas -type f 2>/dev/null | head -1)
    for A in $TARGETS; do
      \"\$P\" -arch=\$A -o /dev/null burn.ptx
      echo \"  compiled for \$A\"
    done"
fi

python3 - "$HASH" "$VERSION" "$TARGETS" > "$OUT" <<'PY'
import json, sys
print(json.dumps({
    "note": "Written by hack/verify-ptx.sh. It attests that the PTX whose sha256 is below compiled with "
            "the named ptxas for the named targets. It does not attest that ptxas ran on whatever machine "
            "reads this; the test that reads it requires the hash to match the PTX in the tree, so an edit "
            "to the kernel invalidates the attestation and the script must be run again.",
    "ptxSHA256": sys.argv[1],
    "ptxasVersion": " ".join(sys.argv[2].split()),
    "targets": sys.argv[3].split(),
}, indent=2))
PY
echo "wrote $OUT for PTX sha256 ${HASH:0:16}..."
