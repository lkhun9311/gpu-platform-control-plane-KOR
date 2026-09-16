#!/usr/bin/env bash
#
# Copy the public images a GPU node pulls into ECR, so the bytes come off the free S3 gateway endpoint
# instead of the NAT gateway at $0.059/GB.
#
# The first three paid sessions spent $5.43 on NAT data processing -- about 92 GB, the largest line on the
# bill and more than every instance-hour combined -- because the engine image comes from Docker Hub and the
# two NVIDIA components from nvcr.io. docs/09 worked this out on 2026-08-27 and recorded that the mirror had
# not been built. This is that mirror.
#
# The images and their digests are read out of config/, never typed here. A list in this file would be a
# second place to pin a version, and the manifests would go on being the ones that decide what actually runs.
set -euo pipefail

REGISTRY="${REGISTRY:-}"
CRANE="${CRANE:-}"
DRY_RUN="${DRY_RUN:-0}"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -n "$REGISTRY" ] || fail "REGISTRY is unset. Set it to the account's ECR host, e.g. REGISTRY=<account>.dkr.ecr.ap-northeast-2.amazonaws.com"

cd "$(dirname "$0")/.."

# crane rather than docker, because docker pull/tag/push does not guarantee the manifest survives byte for
# byte, and a mirror whose digest differs from upstream cannot be pinned by the digest the manifests carry.
if [ -z "$CRANE" ]; then
  CRANE="$(pwd)/bin/crane"
  if [ ! -x "$CRANE" ]; then
    say "installing crane into bin/"
    GOBIN="$(pwd)/bin" go install github.com/google/go-containerregistry/cmd/crane@latest \
      || fail "could not install crane; set CRANE=<path> to use one already on the machine"
  fi
fi

# Repository name -> upstream image, the same map as infra/aws/bootstrap/variables.tf.
#
# The two agree by test (internal/bench's mirror contract test reads both), not by discipline.
declare -A MIRROR=(
  ["mirror/vllm-openai"]="vllm/vllm-openai"  # the reference in config/ carries no registry host
  ["mirror/k8s-device-plugin"]="nvcr.io/nvidia/k8s-device-plugin"
  ["mirror/dcgm-exporter"]="nvcr.io/nvidia/k8s/dcgm-exporter"
)

# Every DIGEST-PINNED image reference the shipped manifests carry.
#
# Pinned only, because a mirror is verified by digest and an unpinned reference has none. config/samples
# carries "vllm/vllm-openai:v0.6.0" as an example value, and it sorts ahead of the pinned reference (":" is
# 0x3a and "@" is 0x40), so a first match over all references picked the sample and refused to mirror the
# image the engine actually runs.
refs="$(grep -rhoP '^\s*(- )?image:\s*\K\S+@sha256:\S+' config/ --include='*.yaml' | sort -u)"

aws ecr get-login-password --region "${AWS_REGION:-ap-northeast-2}" \
  | "$CRANE" auth login "$REGISTRY" --username AWS --password-stdin >/dev/null \
  || fail "could not authenticate to $REGISTRY"

mirrored=0
for repo in "${!MIRROR[@]}"; do
  upstream="${MIRROR[$repo]}"
  # Match the manifest reference for this image, whatever tag and digest it carries.
  ref="$(printf '%s\n' "$refs" | grep -E "^${upstream}[:@]" | head -1 || true)"
  [ -n "$ref" ] || fail "no manifest in config/ references $upstream, so this mirror has nothing to pin to. Remove it from the map here and from infra/aws/bootstrap/variables.tf."

  digest="${ref##*@}"
  case "$digest" in
    sha256:*) ;;
    *) fail "config/ references $upstream without a digest ($ref). A mirror can only be verified against a digest, and an unpinned image is a different problem to fix first." ;;
  esac

  dest="$REGISTRY/$repo@$digest"
  say "$upstream -> $repo"
  say "  digest $digest"

  if [ "$DRY_RUN" = "1" ]; then
    say "  DRY_RUN: not copying"
    continue
  fi

  if "$CRANE" digest "$REGISTRY/$repo@$digest" >/dev/null 2>&1; then
    say "  already mirrored"
  else
    "$CRANE" copy "$ref" "$REGISTRY/$repo:$(printf '%s' "$digest" | cut -c8-19)" \
      || fail "copy failed for $ref"
  fi

  # The whole point, checked rather than assumed: the mirrored manifest must be the same bytes as upstream,
  # or the digest the deployment pins is not the image the deployment gets.
  got="$("$CRANE" digest "$REGISTRY/$repo@$digest" 2>/dev/null || true)"
  [ "$got" = "$digest" ] || fail "mirrored $repo does not resolve to $digest (got '${got:-nothing}'). The copy did not preserve the manifest, so the digest in config/ would pin something this registry does not hold."
  say "  verified $dest"
  mirrored=$((mirrored + 1))
done

say "$mirrored image(s) mirrored and verified"
say "point the cluster at them with: MIRROR_REGISTRY=$REGISTRY hack/m5b-gpu-session.sh"
