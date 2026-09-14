#!/usr/bin/env bash
#
# Runs the GPU-free half of the device session's user-data on a development machine.
#
# The session's user-data is a heredoc that nothing executes until an instance exists, so the first thing
# ever to run those lines is a $3.18/hour g5.12xlarge. That is how a kubeconfig path the script had asserted
# without checking survived into a paid run: kind built the cluster, wrote its config somewhere else, and the
# session died three minutes in having produced no records.
#
# Cluster bring-up needs no card. kind, Kueue, the CRDs and the operator all install the same way on a
# laptop as on the rented box, so the span between the REHEARSABLE markers in hack/queuelab-gpu-session.sh
# runs here for nothing. What is left unrehearsed after this is the device plugin, DCGM and preflight checks
# 3 and 4 -- the parts that genuinely need hardware, and the only parts the instance should be discovering.
#
# This is not a stub of the user-data. It is the user-data, extracted verbatim, so a line that works here
# and fails there is a difference between the two machines rather than between two copies of a script.
set -euo pipefail

RUNNER="${RUNNER:-hack/queuelab-gpu-session.sh}"
# CLUSTER is read out of the span below, not written here.
#
# It was the literal `qlgpu`, which was true of the one runner this rehearsal started with and silently
# false of the second. hack/m5c-gpu-session.sh builds `m5cgpu`, so the cleanup would have deleted a cluster
# that did not exist, reported success, and left a real one running with the developer's /tmp/kubeconfig
# pointing at it -- a rehearsal whose whole subject is cluster bring-up leaking a cluster.
CLUSTER=""
KUBECONFIG_PATH="${KUBECONFIG_PATH:-$(mktemp -u /tmp/rehearse-kubeconfig-XXXXXX)}"
KEEP="${KEEP:-0}"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -f "$RUNNER" ] || fail "$RUNNER not found; run this from the repository root"
for b in kind kubectl docker make; do
  command -v "$b" >/dev/null || fail "$b is not on PATH, and this rehearsal runs the real thing rather than a stub"
done
docker info >/dev/null 2>&1 || fail "the docker daemon is not reachable"

# The span is taken by marker rather than by line number, so editing the runner above it cannot silently
# shift what gets rehearsed.
#
# Both markers are counted BEFORE anything is extracted, because the obvious awk is unsafe: with the closing
# marker deleted the range never ends, awk happily returns the whole rest of the file, and a length check
# passes because the result is longer rather than shorter. Deliberately deleting the marker to see the
# failure is what found that -- the rehearsal did not refuse, it executed the tail of the runner.
opens=$(grep -c '^# >>> REHEARSABLE' "$RUNNER" || true)
closes=$(grep -c '^# <<< REHEARSABLE' "$RUNNER" || true)
[ "$opens" = "1" ] && [ "$closes" = "1" ] \
  || fail "$RUNNER has $opens opening and $closes closing REHEARSABLE markers; exactly one of each is required"
first=$(grep -n '^# >>> REHEARSABLE' "$RUNNER" | cut -d: -f1)
last=$(grep -n '^# <<< REHEARSABLE' "$RUNNER" | cut -d: -f1)
[ "$first" -lt "$last" ] || fail "the closing REHEARSABLE marker (line $last) precedes the opening one (line $first)"

SPAN=$(mktemp /tmp/rehearse-span-XXXXXX.sh)
sed -n "$((first + 1)),$((last - 1))p" "$RUNNER" > "$SPAN"
lines=$(wc -l < "$SPAN")
[ "$lines" -gt 20 ] || fail "extracted only $lines lines between the REHEARSABLE markers"

# The cluster's name comes from the kind config the span itself writes, so it cannot drift from the runner.
CLUSTER=$(sed -n 's/^name: \([a-z0-9][a-z0-9-]*\)$/\1/p' "$SPAN" | head -1)
[ -n "$CLUSTER" ] || fail "could not read a cluster name from the span's kind config; without it this rehearsal would build a cluster it cannot clean up"
say "cluster $CLUSTER"
say "extracted $lines lines from $RUNNER (lines $((first + 1))-$((last - 1)))"

# The one substitution this rehearsal makes, and it is announced rather than hidden.
#
# The span exports KUBECONFIG itself, to /tmp/kubeconfig -- the same literal path the instance uses. Left
# alone it would overwrite whatever a developer has at that path between runs. Nothing else is rewritten:
# the cluster name, the manifest URLs, the image tag and the node label are all the instance's own.
sed -i "s|^export KUBECONFIG=/tmp/kubeconfig$|export KUBECONFIG=$KUBECONFIG_PATH|" "$SPAN"
grep -q "export KUBECONFIG=$KUBECONFIG_PATH" "$SPAN" \
  || fail "the span no longer exports KUBECONFIG=/tmp/kubeconfig, so this rehearsal cannot redirect it"

cleanup() {
  rc=$?
  if [ "$KEEP" = "1" ]; then
    say "KEEP=1, leaving cluster $CLUSTER and $KUBECONFIG_PATH in place"
  else
    say "deleting cluster $CLUSTER"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -f "$KUBECONFIG_PATH"
  fi
  rm -rf "$SPAN" "${WORKDIR:-}"
  exit $rc
}
trap cleanup EXIT INT TERM

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" \
  && fail "a kind cluster named $CLUSTER already exists; delete it first (kind delete cluster --name $CLUSTER)"

# Run with HOME UNSET, because that is the environment that cost the paid session.
#
# cloud-init runs user-data with no HOME, and kind with no HOME writes .kube/config relative to the working
# directory. Setting HOME to some unwritable path would be a different test: the fallback would be absolute
# and the original bug would not reproduce. Unsetting it is the faithful one.
#
# The working directory is a checkout of the source, because that is what the instance runs from.
#
# user-data extracts the uploaded archive to /src and cds there, so `make docker-build` and
# `kubectl apply -f config/crd/bases/` are relative to a source tree. A bare scratch directory made the
# rehearsal fail for a reason the instance would never hit, which is a rehearsal reporting its own defect as
# the runner's.
#
# It is a COPY rather than the repository itself, and that is not incidental: with HOME unset kind writes
# .kube/config relative to the working directory, so running here would dirty the tree -- and the session's
# provenance guard refuses a dirty tree, meaning a rehearsal would block the session it precedes.
#
# `git stash create` gives a tree that includes uncommitted work, falling back to HEAD when there is none.
# The instance always ships HEAD; this ships what you are about to commit, which is the thing worth checking
# before renting hardware. Which one it used is printed.
WORKDIR=$(mktemp -d /tmp/rehearse-cwd-XXXXXX)
TREEISH=$(git stash create 2>/dev/null || true)
if [ -n "$TREEISH" ]; then
  say "source: working tree including uncommitted changes (stash $(git rev-parse --short "$TREEISH"))"
else
  TREEISH=HEAD
  say "source: HEAD $(git rev-parse --short HEAD), working tree clean -- exactly what the instance would ship"
fi
git archive --format=tar "$TREEISH" | tar -x -C "$WORKDIR" \
  || fail "could not lay out the source in $WORKDIR"
[ -f "$WORKDIR/Makefile" ] || fail "the extracted source has no Makefile; the archive is not what it should be"

say "running the span with HOME unset, from $WORKDIR"
start=$(date +%s)
set +e
( cd "$WORKDIR" && env -u HOME bash -x "$SPAN" )
rc=$?
set -e
elapsed=$(( $(date +%s) - start ))

# What kind did with no HOME is asserted rather than assumed, because it is the whole point of the fix.
if [ -e "$WORKDIR/.kube/config" ]; then
  fail "kind still wrote a kubeconfig relative to the working directory ($WORKDIR/.kube/config), so the span is not using the explicit --kubeconfig path"
fi

if [ "$rc" -ne 0 ]; then
  fail "the span exited $rc after ${elapsed}s -- this is a failure the instance would have paid for"
fi

# Exiting zero is not the same as having built the thing. The span's own guards are `|| exit 1`, so a
# command that succeeds while producing nothing passes them, and asserting the result separately is the
# difference between "did not fail" and "worked".
export KUBECONFIG="$KUBECONFIG_PATH"
kubectl get nodes >/dev/null 2>&1 || fail "the span exited zero but its cluster is unreachable through $KUBECONFIG_PATH"
# WHAT THE SPAN WAS SUPPOSED TO BUILD, per runner.
#
# These were one flat list, which was true of the one runner this rehearsal started with. The second one
# builds a deliberately smaller cluster -- no Kueue, no operator -- because hack/test/rehearse-m5c-deploy.sh
# proved on a real cluster that the gateway resolves its backends by reading the CRs itself. Run against it,
# the flat list failed on "Kueue is not installed", which is not a defect in the span: it is this file
# asserting another runner's shape. Selected by runner for the same reason
# hack/test/spot-lifecycle/characterize.sh selects its scenarios by suite -- holding a runner to another
# runner's expectations reports the difference as a regression.
assertions_queuelab_gpu_session() {
  kubectl -n kueue-system get deploy kueue-controller-manager >/dev/null 2>&1 || fail "Kueue is not installed"
  kubectl get crd mltrainingjobs.platform.lkhun9311.github.io >/dev/null 2>&1 || fail "the MLTrainingJob CRD was not applied"
  label=$(kubectl get node "$CLUSTER-worker" -o jsonpath='{.metadata.labels.platform\.lkhun9311\.github\.io/gpu}' 2>/dev/null || true)
  [ "$label" = "true" ] || fail "the worker does not carry the label both DaemonSets select on (got '${label:-none}')"

  # The operator has to RUN, not merely be applied. `make docker-build` and `kind load` both succeed for an
  # image whose binary crashes on start, and `kubectl apply` reports success for a Deployment that never
  # becomes Available. Without this the rehearsal would pass on a broken operator, which is the failure mode
  # it exists to prevent.
  kubectl -n gpu-platform-control-plane-system rollout status \
    deploy/gpu-platform-control-plane-controller-manager --timeout=180s \
    || fail "the operator was applied but never became Available; the image built and loaded, so look at the Pod"
  say "queuelab shape verified: Kueue up, MLTrainingJob CRD applied, worker labelled, operator Available"
}

assertions_m5c_gpu_session() {
  # The CRDs the gateway reads to route. Without InferenceDeployment there is no backend to resolve and
  # without GPUQuotaPolicy there is no tenant to resolve it for.
  for crd in inferencedeployments gpuquotapolicies; do
    kubectl get crd "$crd.platform.lkhun9311.github.io" >/dev/null 2>&1 \
      || fail "the $crd CRD was not applied; the gateway resolves every route by listing these"
  done

  # THE ONE THAT MATTERS HERE. hack/m5c-matrix.sh binds to this ClusterRole and does not create it, and
  # Kubernetes accepts a binding to a ClusterRole that does not exist -- so the gateway starts and every
  # request fails authorization, with nothing looking wrong until the first replay. The matrix refuses when
  # it is absent; this is the line that proves the session satisfies that refusal.
  kubectl get clusterrole gateway-role >/dev/null 2>&1 \
    || fail "the ClusterRole gateway-role is absent, so hack/m5c-matrix.sh would refuse -- and if it did not, every request of every arm would fail authorization"

  # Deliberately NOT asserted: Kueue, the operator, and the gpu/gpu-sharing node labels. The first two this
  # run does not need and does not install; the label is applied by the matrix at run time, on the node it
  # has just been given, rather than by the bring-up.
  kubectl -n kueue-system get deploy kueue-controller-manager >/dev/null 2>&1 \
    && fail "Kueue is installed. This session does not need it and does not install it, so its presence means the span is doing more than this file describes"
  say "m5c shape verified: routing CRDs applied, gateway-role present, and deliberately no Kueue and no operator"
  return 0
}


# The device mount is checkable without a device, and it is the half of the recipe that was missing.
#
# accept-nvidia-visible-devices-as-volume-mounts only means something if something is actually mounted under
# /var/run/nvidia-container-devices/, and a session had the setting without the mount: the toolkit was
# configured to honour a request nobody made, and the plugin advertised 0 of 4 cards. Whether the mount
# EXISTS is a property of the kind config, so it is verifiable here; whether it then yields cards is not.
docker exec "$CLUSTER-worker" test -e /var/run/nvidia-container-devices/all \
  || fail "the worker node has no /var/run/nvidia-container-devices/all, so the container runtime is never asked for the cards"

# The node's OWN runtime, which is a second configuration from the host's and the one a Pod actually uses.
#
# The host's docker is what puts the cards into the kind node; the containerd inside that node is what runs
# the Pods, and a session got as far as four A10Gs visible in the node while the device plugin died on
# "NVML doesn't exist on this system". Both halves are checkable here without a card: whether the toolkit is
# installed in the node, and whether containerd was told to use it.
docker exec "$CLUSTER-worker" test -x /usr/bin/nvidia-ctk \
  || fail "nvidia-ctk is not installed inside the worker node, so its containerd was never configured"
docker exec "$CLUSTER-worker" test -e /sbin/ldconfig.real \
  || fail "the worker node has no /sbin/ldconfig.real, which the container runtime hook on this host calls"
# Asked of the RUNNING containerd, not of a file.
#
# The first version of this check grepped /etc/containerd/config.toml and failed, while the configuration had
# in fact worked: nvidia-ctk writes a drop-in to /etc/containerd/conf.d/ because kind's config.toml imports
# that directory. Checking the file that happened to be named was a check of the wrong thing, and it would
# have condemned a working fix. crictl reports what containerd actually loaded, which is the only version of
# this question a Pod cares about.
docker exec "$CLUSTER-worker" crictl info 2>/dev/null | grep -q '"defaultRuntimeName": "nvidia"' \
  || fail "the worker node's containerd is not running with nvidia as its default runtime, so Pods get no NVML"

# The summary names only what THIS run checked.
#
# It used to read "cluster reachable, Kueue up, CRD applied, worker labelled, operator Available" for every
# runner, and against hack/m5c-gpu-session.sh -- which installs no Kueue, no operator and no label -- it
# printed all five as though they had been verified. A line that claims a verification which did not happen
# is worse than no line, because it is the one a reader trusts instead of scrolling. The per-runner block at
# the bottom prints what it actually asserted.
say "bring-up rehearsed in ${elapsed}s: cluster reachable through the explicit kubeconfig"
say "in-node runtime configured: nvidia-ctk installed, ldconfig.real present, containerd knows nvidia"
say "device mount present in the node container (whether it yields cards needs hardware)"
say "NOT rehearsed, and only a real card can: the device plugin, DCGM, and preflight checks 3 and 4"

# The per-runner half, after the checks both runners share.
#
# Placed last so that the shared assertions above -- the device mount and the node's own container runtime,
# which both spans cover -- run for every runner regardless of which one this is.
case "$(basename "$RUNNER" .sh)" in
  queuelab-gpu-session) assertions_queuelab_gpu_session ;;
  m5c-gpu-session)      assertions_m5c_gpu_session ;;
  *) fail "no post-span assertions defined for $(basename "$RUNNER" .sh); a rehearsal that checks nothing after the span reports 'did not fail' as 'worked'" ;;
esac
