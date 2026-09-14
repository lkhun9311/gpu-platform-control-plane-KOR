#!/usr/bin/env bash
#
# The session pre-registered in docs/superpowers/specs/2026-09-05-the-device-was-never-observed.md.
#
# It exists to remove one banner. `queuelabrun -compare` prints this on every comparison the lab has ever
# produced:
#
#     device: NOT OBSERVED -- every GPU-second below is a second of RESERVATION
#
# The code that removes it is written and has never run: -require-device invalidates a run with no device
# evidence, hack/gpu-session.sh routes the exporter per worker and takes the termination canary,
# internal/queuelab/submit.go carries a PTX kernel loaded through the CUDA driver, and
# cmd/queuelabrun/device_preflight.go gates the spend. What none of it can produce is a driver and a card.
#
# WHY THIS IS NOT hack/m6-kind-e2e.sh WITH A GPU
#
# That script builds a kind cluster with the FAKE device plugin -- config/device-plugin, the gpu-simulator
# that advertises nvidia.com/gpu on nodes with no cards. This one installs the REAL plugin
# (config/nvidia-device-plugin) plus the DCGM exporter, on a machine that has four, and the difference is
# the entire point: a fake plugin makes every GPU-second a reservation, which is the banner.
#
# WHY g5.12xlarge
#
# The reclaim protocol needs two devices held concurrently on the worker under test, and AWS has no
# two-GPU G instance, so the floor is four.
#
# Three candidates, and two constraints that do not agree. On Spot placement g4dn.12xlarge scores 1 out of
# 10 in this region -- AWS saying the request will probably not be filled -- while g5 and g6 both score 3.
# But the organisation's `deny-instance-family` service control policy allows only t3.*, g4dn.* and g5.*,
# and an SCP is not something an account administrator can override. The g6 was tried first and refused by
# it. That leaves the g5: 48 vCPU, which is exactly the Spot G quota, and an A10G at sm_86, one of the four
# targets hack/verify-ptx.sh already compiles the kernel for.
#
# THREE IS STILL A LOW SCORE
#
# So this expects interruption rather than merely tolerating it. A watcher ships every record within
# seconds of it being written, so an interruption after six of eight runs costs the seventh and the eighth
# rather than all of them. gpu-session.sh already knows how to resume from a partial set with START_AT.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

REGION="${AWS_REGION:-ap-northeast-2}"
INSTANCE_TYPE="${INSTANCE_TYPE:-g5.12xlarge}"
# Above the observed Spot price of about $3.15 with room for a rise, and below the on-demand price the
# pre-registration deliberately does not authorise.
MAX_SPOT_PRICE="${MAX_SPOT_PRICE:-3.90}"
# The pre-registration's hard stop is 120 minutes. The backstop inside the instance is that plus a margin,
# so the timer this script keeps is the one that fires first.
BACKSTOP_SECONDS="${BACKSTOP_SECONDS:-8400}"
HARD_STOP_SECONDS="${HARD_STOP_SECONDS:-7200}"
REPS="${REPS:-4}"
# Both arms always run: the study IS the contrast between them, and gpu-session.sh interleaves them.
# The dose is narrowed, because the pre-registration buys grace-bounded only.
DOSES="${DOSES:-grace-bounded}"

# STUDY picks which experiment this session buys, and it changes the banner as well as the runs.
#
# reclaim is the termination-contract study, four runs a repetition. idling is the duty study, two a
# repetition, and it exists because the reclaim session could not decide whether reserved GPU-seconds track
# used ones -- its workload computes continuously, so agreement was the only answer available.
STUDY="${STUDY:-reclaim}"
OUT="${OUT:-hack/qlgpu-$(date -u +%Y%m%d-%H%M%S)}"
STACK="queuelab-gpu"

say()  { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Validated HERE rather than beside its default, because fail() does not exist yet up there.
#
# The first version put this check next to STUDY's assignment, which reads better and does not work: an
# unknown study produced "fail: command not found" and exit 127 instead of the sentence. A characterization
# scenario for a bad value is what showed it -- the refusal was written, recorded, and was not a refusal.
case "$STUDY" in
  reclaim | idling) ;;
  *) fail "STUDY must be reclaim or idling; got '$STUDY'. reclaim is the termination-contract study, idling is the duty one" ;;
esac

spot_say()  { say "$@"; }
spot_fail() { fail "$@"; }
# shellcheck source=hack/lib/spot-run.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib/spot-run.sh"

ACCOUNT=$(spot_account) || fail "not authenticated"
BUCKET="${BUCKET:-$STACK-$ACCOUNT}"
RUN_ID="$(basename "$OUT")"

mkdir -p "$OUT"
# The banner names the study, because a session that says "reclaim" while running the idling arms would put
# the wrong sentence at the top of the only log a reader keeps.
if [ "$STUDY" = "idling" ]; then
  say "study  queuelab idling -- does a held card that is not used look different from one that is"
  say "arms   D-full and D-quarter, the termination contract held constant at the ignoring one"
else
  say "study  queuelab reclaim, with the device observed"
fi
say "doses  $DOSES   reps $REPS   (both arms, interleaved by gpu-session.sh)"
say "output $OUT"

# ---------------------------------------------------------------- the source the instance builds from
#
# git archive rather than a tarball of the working tree: the instance must build the operator image from a
# committed state, so that the record it produces names a commit somebody can check out. A dirty tree would
# produce evidence about a build that exists nowhere.
# REQUIRE_CLEAN_TREE exists so the characterization harness can drive the rest of this script, and it is
# deliberately loud rather than silent. An override that leaves no trace is an override somebody uses on a
# paid run and forgets, so the line below goes into the run log and into every golden that records one.
REQUIRE_CLEAN_TREE="${REQUIRE_CLEAN_TREE:-1}"
COMMIT=$(git rev-parse HEAD)
# --porcelain rather than `git diff`, because git diff does not see UNTRACKED files and git archive does not
# include them either. A tree carrying a new script somebody is about to add passed this guard and shipped
# an archive without it, which is precisely the mismatch the guard exists to refuse. Found because the
# characterization scenario that asserts this refusal could not make it fire.
if [ -z "$(git status --porcelain)" ]; then
  say "source $COMMIT, working tree clean"
elif [ "$REQUIRE_CLEAN_TREE" = "1" ]; then
  fail "the working tree is dirty. This session records a commit as the provenance of its numbers, and a build from uncommitted changes is provenance that names nothing. Commit, or set REQUIRE_CLEAN_TREE=0 and accept that the archive will not match the tree you are looking at"
else
  say "source $COMMIT, TREE IS DIRTY and REQUIRE_CLEAN_TREE=0 -- this archive does NOT match the working tree"
fi
git archive --format=tar.gz -o "$OUT/source.tgz" HEAD || fail "git archive"
SOURCE_SHA=$(sha256sum "$OUT/source.tgz" | cut -d' ' -f1)

# ---------------------------------------------------------------- AWS scaffolding
spot_ensure_bucket "$BUCKET" "$REGION" 30 || fail "could not prepare the results bucket $BUCKET"

# This instance reads as well as writes: it downloads the source archive it was sent.
spot_ensure_profile "$STACK" \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[\"s3:PutObject\",\"s3:GetObject\"],\"Resource\":\"arn:aws:s3:::$BUCKET/*\"}]}" \
  20 || fail "could not prepare the instance profile $STACK"

say "uploading the source archive"
aws s3 cp "$OUT/source.tgz" "s3://$BUCKET/$RUN_ID/src/source.tgz" >/dev/null \
  || fail "could not upload the source archive"

# queuelabrun is built HERE and shipped, because the instance has no Go.
#
# The deep-learning AMI carries a driver, not a toolchain, and gpu-session.sh opens by building the runner it
# is about to call. A session reached that line with everything else working -- cards advertised, DCGM
# answering -- and died on `go: command not found`, after paying for all of it.
#
# Installing a toolchain on a rented box would work and is the wrong shape. The other runner in this
# directory already ships its binary with a checksum the instance verifies, and following that is both
# cheaper and better provenance: what ran is a binary whose digest is recorded, not whatever a compiler on a
# machine nobody kept produced.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/queuelabrun" ./cmd/queuelabrun \
  || fail "could not build queuelabrun; the instance has no Go and cannot build it either"
RUNNER_SHA=$(sha256sum "$OUT/queuelabrun" | cut -d' ' -f1)
say "queuelabrun $(du -h "$OUT/queuelabrun" | cut -f1), sha256 ${RUNNER_SHA:0:12}"
aws s3 cp "$OUT/queuelabrun" "s3://$BUCKET/$RUN_ID/bin/queuelabrun" >/dev/null \
  || fail "could not upload queuelabrun"

AMI=$(spot_resolve_ami "$REGION" \
  /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id) \
  || fail "could not resolve a GPU AMI"

ZONES=$(spot_zones_offering "$REGION" "$INSTANCE_TYPE")
[ -n "$ZONES" ] || fail "$INSTANCE_TYPE is offered in no availability zone of $REGION"
say "$INSTANCE_TYPE is offered in: $ZONES"

# ---------------------------------------------------------------- what the instance runs
RUNSCRIPT=$(mktemp)
cat > "$RUNSCRIPT" <<'USERDATA'
#!/bin/bash
exec > >(tee /var/log/qlgpu.log) 2>&1
set -x
( sleep BACKSTOP_SECONDS_PLACEHOLDER; shutdown -h now ) &

BUCKET="BUCKET_PLACEHOLDER"
PREFIX="RUN_ID_PLACEHOLDER"
SOURCE_SHA="SOURCE_SHA_PLACEHOLDER"
RUNNER_SHA="RUNNER_SHA_PLACEHOLDER"
COMMIT="COMMIT_PLACEHOLDER"
REPS="REPS_PLACEHOLDER"
DOSES="DOSES_PLACEHOLDER"
STUDY="STUDY_PLACEHOLDER"

upload() { aws s3 cp "$1" "s3://$BUCKET/$PREFIX/$2" || true; }
trap 'upload /var/log/qlgpu.log log.txt; shutdown -h now' EXIT

# The source is verified before it is trusted, for the reason the harness binary is in the other runner: a
# truncated download that still extracts produces a build, and a build produces numbers.
aws s3 cp "s3://$BUCKET/$PREFIX/src/source.tgz" /tmp/source.tgz
got=$(sha256sum /tmp/source.tgz | cut -d' ' -f1)
if [ "$got" != "$SOURCE_SHA" ]; then
  echo "source checksum mismatch: expected $SOURCE_SHA, got $got"
  exit 1
fi
mkdir -p /src && tar -xzf /tmp/source.tgz -C /src
cd /src

# The runner binary, verified the same way and for the same reason as the source.
#
# QUEUELABRUN_PREBUILT is what tells gpu-session.sh not to build one. It is an explicit flag rather than a
# bare "skip the build if a file is there", because a stale binary left in a working tree would then be used
# silently -- and the whole point of shipping it is that its digest is known.
aws s3 cp "s3://$BUCKET/$PREFIX/bin/queuelabrun" /src/queuelabrun
chmod +x /src/queuelabrun
got=$(sha256sum /src/queuelabrun | cut -d' ' -f1)
if [ "$got" != "$RUNNER_SHA" ]; then
  echo "queuelabrun checksum mismatch: expected $RUNNER_SHA, got $got"
  exit 1
fi
export QUEUELABRUN_PREBUILT=1

# ---------------------------------------------------------------- the cards, before anything else
#
# Preflight check 1 of 4. A machine that does not report four cards is not the machine this session was
# costed for, and finding that out after twenty minutes of cluster bring-up is finding it out too late.
nvidia-smi --query-gpu=index,name,memory.total --format=csv > /tmp/nvidia-smi.csv || exit 1
upload /tmp/nvidia-smi.csv preflight-nvidia-smi.csv
cards=$(tail -n +2 /tmp/nvidia-smi.csv | wc -l)
if [ "$cards" -lt 4 ]; then
  echo "PREFLIGHT FAILED: $cards cards, the protocol needs 4 (2 for the trace, 2 held by the occupier)"
  exit 1
fi

# ---------------------------------------------------------------- kind, with the host's cards visible
#
# kind nodes are containers, so the GPUs reach them only if the container runtime passes them through by
# default. The toolkit is on the deep-learning AMI already; what it needs is to be the DEFAULT runtime and
# to accept the visible-devices variable as a volume mount, which is how the device plugin inside kind
# claims a card.
# Installed rather than assumed. The deep-learning AMI has shipped the toolkit for a while, and a session
# that discovers otherwise twenty minutes in has paid for the discovery.
if ! command -v nvidia-ctk >/dev/null 2>&1; then
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
  apt-get update && apt-get install -y nvidia-container-toolkit || exit 1
fi
nvidia-ctk runtime configure --runtime=docker --set-as-default
sed -i 's/^#accept-nvidia-visible-devices-as-volume-mounts.*/accept-nvidia-visible-devices-as-volume-mounts = true/' \
  /etc/nvidia-container-runtime/config.toml || true
grep -q 'accept-nvidia-visible-devices-as-volume-mounts = true' /etc/nvidia-container-runtime/config.toml \
  || echo 'accept-nvidia-visible-devices-as-volume-mounts = true' >> /etc/nvidia-container-runtime/config.toml
systemctl restart docker
sleep 5

curl -fsSLo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.24.0/kind-linux-amd64
chmod +x /usr/local/bin/kind
curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/v1.31.0/bin/linux/amd64/kubectl"
chmod +x /usr/local/bin/kubectl

# >>> REHEARSABLE -- everything to the matching marker needs no GPU.
#
# hack/test/rehearse-bringup.sh extracts exactly this span and runs it on a development machine. It exists
# because the first session to build a cluster here lost it to a kubeconfig path this script had asserted
# without checking, and the only thing that had ever executed the line was a $3.18/hour instance. The span
# ends where the real device plugin begins, which is the first thing that genuinely needs a card.
#
# Moving either marker changes what is rehearsed. Do it deliberately.

# One worker, because the protocol measures one worker and a second would only add a factor this session is
# not buying. The control plane stays separate so stopping anything on the worker cannot take the apiserver.
cat > /tmp/kind.yaml <<'KINDEOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: qlgpu
nodes:
  - role: control-plane
  # The mount is what makes accept-nvidia-visible-devices-as-volume-mounts mean anything.
  #
  # That setting tells the container runtime to read a mount under /var/run/nvidia-container-devices/ as if
  # it were NVIDIA_VISIBLE_DEVICES, so mounting anything at .../all is how a kind node -- an ordinary
  # container that nobody passes GPU environment to -- ends up with every card. The first session to get
  # this far had the setting and not the mount, which is half a recipe: the toolkit was configured to honour
  # a request that was never made, the node container saw no devices, and the plugin advertised 0 of 4.
  #
  # /dev/null is the conventional source because only the mount's PATH carries meaning; its contents are
  # never read.
  - role: worker
    extraMounts:
      - hostPath: /dev/null
        containerPath: /var/run/nvidia-container-devices/all
KINDEOF
# The kubeconfig path is told to kind rather than guessed from it.
#
# The first session to reach this line lost the cluster it had just built. kind reported success and printed
# `Set kubectl context to "kind-qlgpu"`, and the next kubectl went to localhost:8080 -- which is what kubectl
# does when the file it was pointed at does not exist.
#
# cloud-init runs user-data with HOME unset, and kind with no HOME writes `.kube/config` RELATIVE TO THE
# WORKING DIRECTORY rather than to the passwd-database home. This script does `cd /src` above, so the
# kubeconfig went to /src/.kube/config while the next line read /root/.kube/config. Measured rather than
# reasoned: with HOME unset in an empty directory, kind creates ./.kube/config there and leaves the passwd
# home's file untouched.
#
# So the path is an argument, and both sides use the same variable.
export KUBECONFIG=/tmp/kubeconfig
echo "HOME=${HOME:-<unset>} KUBECONFIG=$KUBECONFIG"
kind create cluster --config /tmp/kind.yaml --kubeconfig "$KUBECONFIG" --wait 300s || exit 1

# Checked here, and fatal here, because this is the line that was wrong. Without it the failure surfaced two
# commands later as a Kueue manifest that would not validate, which reads like a Kueue problem.
kubectl cluster-info || { echo "PREFLIGHT FAILED: the cluster is up but unreachable through $KUBECONFIG"; exit 1; }

# The toolkit is needed INSIDE the node as well, and that is a second configuration, not the same one.
#
# Everything above configures the HOST's docker, which is what puts the cards into the kind node: the last
# session proved it, with `nvidia-smi -L` inside qlgpu-worker listing four A10Gs. Pods do not run on the
# host's docker. They run on the containerd inside that node, which knows nothing about any of it, so the
# device plugin started with no NVML and died:
#
#   E factory.go:87] Incompatible strategy detected auto
#   error creating plugin manager: unable to create plugin manager: invalid device discovery strategy
#
# and the DCGM exporter agreed: "NVML doesn't exist on this system". The node had the cards and the workloads
# could not reach them.
#
# kindest/node ships neither nvidia-ctk nor /sbin/ldconfig.real -- checked, not assumed, by running the image
# locally. So both are put there: the symlink because the container runtime hook is configured on this Ubuntu
# host to call ldconfig.real, which Debian's node image does not have.
docker exec qlgpu-worker ln -sf /sbin/ldconfig /sbin/ldconfig.real || exit 1
docker exec qlgpu-worker bash -c '
  set -e
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq curl gnupg ca-certificates
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed "s#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g" \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
  apt-get update -qq
  apt-get install -y -qq nvidia-container-toolkit
  nvidia-ctk runtime configure --runtime=containerd --set-as-default
' || { echo "PREFLIGHT FAILED: could not configure the container runtime inside qlgpu-worker"; exit 1; }

# Restarting containerd takes the kubelet's runtime out from under it, so the node is waited for rather than
# assumed back. Everything applied after this point would otherwise land on a node that is still NotReady.
docker exec qlgpu-worker systemctl restart containerd || exit 1
kubectl wait --for=condition=Ready node/qlgpu-worker --timeout=300s \
  || { echo "PREFLIGHT FAILED: qlgpu-worker did not come back Ready after its containerd was restarted"; exit 1; }

# ---------------------------------------------------------------- the platform under test
kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.3/manifests.yaml
kubectl -n kueue-system wait --for=condition=Available deploy/kueue-controller-manager --timeout=300s || exit 1

# docker build inside a container, so no Go is needed on this host.
make docker-build IMG=controller:latest || exit 1
kind load docker-image controller:latest --name qlgpu || exit 1

# The CRDs are applied from the committed files rather than through `make install`.
#
# That target depends on manifests, which depends on controller-gen, which the Makefile installs with Go --
# absent from this AMI -- and which would REGENERATE the CRDs before applying them. Regenerating on a rented
# box means the cluster gets whatever this machine's controller-gen produces rather than what the commit
# says, which is the opposite of the provenance this session is trying to record. `make manifests` is a
# no-op on this tree, so the committed files already are the generated ones.
kubectl apply --server-side -f config/crd/bases/ || exit 1

# The image is built HERE, so the ECR pin must not survive into the cluster.
#
# config/manager/kustomization.yaml carries an images transform pinning the manager to an ECR digest, and
# its own comment records what happens when the override stops applying: "every e2e run failed on a Pod that
# could not pull from a registry kind has no credentials for". That is exactly what this session did. It
# built controller:latest, side-loaded it, applied a Deployment referencing ECR, and the operator sat in
# ImagePullBackOff -- while every command above it reported success.
#
# /src is a throwaway checkout of the archive, so editing it here changes nothing a reader will later read.
sed -i -e 's|^    newName: .*|    newName: controller|' \
       -e 's|^    digest: .*|    newTag: latest|' config/manager/kustomization.yaml
grep -q '^    newName: controller$' config/manager/kustomization.yaml \
  || { echo "PREFLIGHT FAILED: could not repoint the operator image at the locally built one"; exit 1; }

kubectl kustomize config/operator | kubectl apply --server-side -f - || exit 1

# A :latest tag defaults to imagePullPolicy Always, which sends kubelet to a registry for an image that only
# exists in this node's containerd because kind put it there.
kubectl -n gpu-platform-control-plane-system patch deploy gpu-platform-control-plane-controller-manager \
  --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
  || exit 1

# Applied is not running. Without this the session would carry a dead operator into the study and discover it
# as MLTrainingJobs that never reconcile, an hour and thirty dollars later.
kubectl -n gpu-platform-control-plane-system rollout status \
  deploy/gpu-platform-control-plane-controller-manager --timeout=300s \
  || { echo "PREFLIGHT FAILED: the operator never became Available"; exit 1; }

# The node label both DaemonSets select on, which nothing in this repository applies.
#
# config/nvidia-device-plugin and config/dcgm-exporter both carry
# `nodeSelector: platform.lkhun9311.github.io/gpu: "true"`, and every other reference to that label in the
# tree READS it -- m5b-arms.sh and m5b-gpu-session.sh look up the node group by it. In production it comes
# from the EKS node group. On a kind cluster nobody applies it, so both DaemonSets would sit Pending, the
# node would advertise nothing, and preflight check 2 would fail after the instance, the driver, the
# cluster and the operator had all been paid for.
#
# The plugin's own comment explains why this label and not nvidia.com/gpu.present: GPU Feature Discovery is
# not deployed here, and selecting on that one would leave the plugin unschedulable while the node reads
# downstream as "no GPU nodes" rather than as "the plugin never ran".
kubectl label node qlgpu-worker platform.lkhun9311.github.io/gpu=true --overwrite || exit 1
# <<< REHEARSABLE

# The worker is deliberately NOT tainted nvidia.com/gpu=present:NoSchedule the way the production node
# group is. This cluster has one worker and kind's control plane carries its own NoSchedule taint, so
# tainting it would leave Kueue and the operator with nowhere to run.

# The REAL device plugin, not config/device-plugin, which is the fake one that makes every GPU-second a
# reservation. This is the line the whole session is about.
kubectl kustomize config/nvidia-device-plugin | kubectl apply -f - || exit 1
kubectl kustomize config/dcgm-exporter | kubectl apply -f - || exit 1

# ---------------------------------------------------------------- preflight, checks 2 to 4
#
# Charged before any measurement because it is the failure that wastes the session, and it costs minutes.
# Check 4 is the one that matters: the first three can all pass on a machine where attribution is still
# impossible, and attribution is the deliverable.
for _ in $(seq 1 60); do
  adv=$(kubectl get node qlgpu-worker -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null || echo 0)
  [ "${adv:-0}" -ge 4 ] && break
  sleep 5
done
echo "worker advertises ${adv:-0} nvidia.com/gpu"
kubectl get nodes -o wide > /tmp/preflight-nodes.txt
upload /tmp/preflight-nodes.txt preflight-nodes.txt
if [ "${adv:-0}" -lt 4 ]; then
  # Evidence, before the instance goes away.
  #
  # The first time this check failed it uploaded the number and nothing else: five minutes of a silent loop,
  # then "0 of 4". Whether the plugin Pod was Pending, crashing, or running and finding no devices are three
  # different faults with three different fixes, and telling them apart cost another instance. A refusal that
  # does not say why is a refusal that has to be bought twice.
  {
    echo "=== node container: does it see any card at all? ==="
    docker exec qlgpu-worker nvidia-smi -L 2>&1 || echo "(nvidia-smi failed inside the node container)"
    echo
    echo "=== the mount that should have put them there ==="
    docker exec qlgpu-worker ls -la /var/run/nvidia-container-devices/ 2>&1 || echo "(no such directory in the node)"
    echo
    echo "=== docker default runtime ==="
    docker info 2>/dev/null | grep -iA2 'runtime' || true
    echo
    echo "=== pods ==="
    kubectl -n gpu-platform-control-plane-system get pods -o wide 2>&1
    echo
    echo "=== device plugin ==="
    kubectl -n gpu-platform-control-plane-system describe ds nvidia-device-plugin 2>&1 | tail -30
    kubectl -n gpu-platform-control-plane-system logs ds/nvidia-device-plugin --tail=60 2>&1
    echo
    echo "=== dcgm exporter ==="
    kubectl -n gpu-platform-control-plane-system logs ds/dcgm-exporter --tail=30 2>&1
    echo
    echo "=== node allocatable, in full ==="
    kubectl get node qlgpu-worker -o jsonpath='{.status.allocatable}' 2>&1; echo
  } > /tmp/preflight-why.txt 2>&1
  upload /tmp/preflight-why.txt preflight-why.txt
  echo "PREFLIGHT FAILED: the real device plugin advertises ${adv:-0} of 4 cards; see preflight-why.txt"
  exit 1
fi

# Preflight checks 3 and 4 are NOT written here. hack/gpu-session.sh takes the termination canary and then
# runs `queuelabrun -device-preflight`, which applies a GPU-holding Pod with the same template, placement,
# toleration and finalizer a real run gets, and reports which of the driver calls answered. A second
# preflight written into this file would be a probe invented for the occasion, which is the thing that mode
# exists to avoid being.

# ---------------------------------------------------------------- the study, shipping records as they land
#
# gpu-session.sh runs the whole study itself -- surplus occupancy, canary, per-worker exporter route,
# preflight, then the runs -- so this does not reimplement its loop. What it adds is the one thing a
# low placement score demands: an uploader watching the session directory, so a record reaches S3 within
# seconds of being written rather than at the end. An interruption after six of eight runs must cost the
# seventh and the eighth, not all of them.
# ---------------------------------------------------------------- the device series, kept as evidence
#
# queuelabrun scrapes DCGM itself and records what it derives. That derivation is the measurement, and it is
# also the only thing that survived: nothing kept the series it was derived FROM, so a reader could not plot
# the run, check a suspicious number against the raw exposition, or see what the card was doing between two
# records. This session rents the hardware once, and the exposition is the cheapest thing on the box.
#
# No Grafana. There is none deployed here -- config/prometheus carries a ServiceMonitor, a PodMonitor and a
# dashboard for a cluster that has prometheus-operator, which this kind cluster does not -- and standing one
# up would spend paid minutes to screenshot numbers that are already text. The series is captured here and
# drawn afterwards by hack/plot-device-observation.py, which runs on a laptop for nothing.
#
# Only DCGM_FI_DEV_GPU_UTIL lines are kept at the sampling interval, because they carry the Pod and namespace
# labels that make a utilisation figure attributable -- which is the whole point of this session. Two full
# scrapes bracket the study so the reader can see the exposition those lines were cut from.
# The route is built the way gpu-session.sh builds its own, because that file already learned this.
#
# The first version used a FIXED port and a Service. Both were wrong, and the session that proved it had
# already reached DEVICE USABLE: the port-forward exited at once, the loop then curled a dead port thirty
# times, and the series was lost from the one run that finally had something to show.
#
# A fixed port is the worse of the two. gpu-session.sh's own comment says why: anything already listening on
# it makes kubectl exit immediately and the checks then talk to that other process -- a local listener
# serving one plausible DCGM line would satisfy them. An ephemeral port cannot be squatted.
#
# And the port-forward itself is retried, not merely the scrape. Retrying only the curl treats a dead
# forwarder as a slow one.
SAMPLER=""
SAMPLER_PF=""
SAMPLER_URL=""
for attempt in 1 2 3 4 5; do
  dpod=$(kubectl -n gpu-platform-control-plane-system get pods \
    -l app.kubernetes.io/component=dcgm-exporter --field-selector spec.nodeName=qlgpu-worker \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  if [ -z "$dpod" ]; then sleep 5; continue; fi
  sport=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
  kubectl port-forward -n gpu-platform-control-plane-system "pod/$dpod" "$sport:9400" \
    >>/tmp/sampler-pf.log 2>&1 &
  pf=$!
  sleep 2
  if ! kill -0 "$pf" 2>/dev/null; then
    echo "sampler: port-forward to $dpod exited immediately (attempt $attempt)"
    continue
  fi
  url="http://127.0.0.1:$sport/metrics"
  for _ in $(seq 1 20); do
    curl -fsS --max-time 2 "$url" >/tmp/scrape-first.txt 2>/dev/null && break
    sleep 1
  done
  if [ -s /tmp/scrape-first.txt ]; then
    SAMPLER_PF=$pf
    SAMPLER_URL=$url
    echo "sampler: reading $dpod through $url"
    break
  fi
  kill "$pf" 2>/dev/null || true
  echo "sampler: $dpod did not answer on $url (attempt $attempt)"
done

if [ -n "$SAMPLER_URL" ]; then
  upload /tmp/scrape-first.txt device-scrape-first.txt
  (
    # Two seconds, which is finer than the study's own resolution floor of 5.906 s, so a reader can see
    # inside a record rather than only between them.
    while :; do
      ts=$(date -u +%s)
      curl -fsS --max-time 2 "$SAMPLER_URL" 2>/dev/null \
        | grep '^DCGM_FI_DEV_GPU_UTIL' | sed "s/^/$ts\t/" >> /tmp/device-util.tsv \
        || echo -e "$ts\tSCRAPE_FAILED" >> /tmp/device-util.tsv
      sleep 2
    done
  ) &
  SAMPLER=$!
else
  # Not fatal, and not silent. The study's own -require-device is the gate on whether the run is valid; this
  # sampler is evidence for the reader, and losing it must not read as losing the measurement.
  echo "WARNING: could not reach the dcgm exporter for sampling; the series will be missing from the artifacts. See /tmp/sampler-pf.log"
fi

export EXDIR=/tmp/session
mkdir -p "$EXDIR"

(
  # Ships anything new every ten seconds, and keeps shipping until the study exits. Records are small and
  # S3 puts are idempotent by key, so re-uploading an unchanged file costs nothing worth optimising.
  while :; do
    for f in "$EXDIR"/*.json; do
      [ -e "$f" ] && aws s3 cp "$f" "s3://$BUCKET/$PREFIX/runs/$(basename "$f")" >/dev/null 2>&1
    done
    sleep 10
  done
) &
UPLOADER=$!

rc=0
# The worker is a POSITIONAL argument. gpu-session.sh takes WORKERS=("$@"), and calling it with none used
# to reach `WORKERS[0]: unbound variable` after everything above had already been paid for.
# RUN_STUDY=1, without which gpu-session.sh qualifies the worker and stops.
#
# It printed exactly that -- "To run the study through these same routes: RUN_STUDY=1 ..." -- and exited
# zero, so the session paid for an instance, proved DEVICE USABLE and OBSERVER ATTRIBUTES on real hardware,
# and then wrote no records because nothing asked it to. Its own instruction was in the log the whole time.
#
# One invocation is right rather than two: the qualification warms the node, and that file's closing note
# says a run taken cold pulls its image inside its own observation window and can censor its own waste
# figure. With this set, the preflight runs first and the study follows on a warm node.
REPS="$REPS" EXDIR="$EXDIR" DOSES="$DOSES" STUDY="$STUDY" RUN_STUDY=1 \
  bash hack/gpu-session.sh qlgpu-worker >/tmp/study.log 2>&1 || rc=$?
kill "$UPLOADER" 2>/dev/null || true

# The sampler stops with the study, and its series goes up whether the study passed or not: a failed run's
# series is how a reader tells "the card was idle" from "nothing ever scraped it".
[ -n "${SAMPLER:-}" ] && kill "$SAMPLER" 2>/dev/null || true
[ -n "${SAMPLER_URL:-}" ] && curl -fsS --max-time 3 "$SAMPLER_URL" > /tmp/scrape-last.txt 2>/dev/null \
  && upload /tmp/scrape-last.txt device-scrape-last.txt
kill "$SAMPLER_PF" 2>/dev/null || true
if [ -s /tmp/device-util.tsv ]; then
  gzip -c /tmp/device-util.tsv > /tmp/device-util.tsv.gz
  upload /tmp/device-util.tsv.gz device-util.tsv.gz
  echo "device series: $(wc -l < /tmp/device-util.tsv) sampled lines"
else
  echo "device series: EMPTY -- nothing was sampled"
fi

upload /tmp/study.log study.log
tar -czf /tmp/session.tgz -C /tmp session
upload /tmp/session.tgz session.tgz
echo "$COMMIT" > /tmp/commit.txt && upload /tmp/commit.txt commit.txt
# A final sweep, because the watcher may have been killed between a write and its tick.
for f in "$EXDIR"/*.json; do
  [ -e "$f" ] && upload "$f" "runs/$(basename "$f")"
done

# DONE means records exist, not that the study exited zero. A study that produced six good records and
# then failed is worth collecting -- and a study that exited zero having written none is not.
#
# Counted with a glob into an array rather than `ls ... | wc -l`. Under errexit and pipefail, ls exits 2
# when its glob matches nothing, the pipeline inherits it, and the script dies at the count -- taking with
# it the message that was about to explain the empty case. The characterization suite caught exactly that
# on the host side of this script, where the operator was left with no refusal at all in the one run that
# most needed one.
shopt -s nullglob
records=("$EXDIR"/*.json)
accepted=${#records[@]}
shopt -u nullglob
echo "records written: $accepted, study rc=$rc"
if [ "$accepted" -gt 0 ]; then
  touch /tmp/DONE && upload /tmp/DONE DONE
else
  echo "no record was written; DONE withheld"
fi
USERDATA

UD=$(mktemp)
{
  echo "#!/bin/bash"
  sed -e "s|BACKSTOP_SECONDS_PLACEHOLDER|$BACKSTOP_SECONDS|" \
      -e "s|BUCKET_PLACEHOLDER|$BUCKET|" \
      -e "s|RUN_ID_PLACEHOLDER|$RUN_ID|" \
      -e "s|SOURCE_SHA_PLACEHOLDER|$SOURCE_SHA|" \
      -e "s|RUNNER_SHA_PLACEHOLDER|$RUNNER_SHA|" \
      -e "s|COMMIT_PLACEHOLDER|$COMMIT|" \
      -e "s|REPS_PLACEHOLDER|$REPS|" \
      -e "s|DOSES_PLACEHOLDER|$DOSES|" \
      -e "s|STUDY_PLACEHOLDER|$STUDY|" "$RUNSCRIPT" | tail -n +2 \
    | sed -e '/^#/d' -e '/^[[:space:]]*$/d'
} > "$UD"

# The comments come off on the way out, and only on the way out.
#
# EC2 caps user-data at 25600 bytes ENCODED, and this script's explanations had grown past it: 19,914 raw
# became 26,552 base64, and all three zones returned InvalidParameterValue. The comments are why anything
# here is the way it is and they stay in the tracked file; what the instance runs does not need them, and
# without them the same script encodes to about 13,000 bytes.
#
# Only column-0 comments are dropped. The indented ones inside the kind.yaml heredoc are YAML that a reader
# of the launched configuration should still see, and deleting by indentation rather than by content is the
# rule that cannot accidentally cut a line out of a string.
cp "$UD" "$OUT/user-data.sh"

# Refused here, with the number, rather than three zones later with an error that names no cause.
#
# The runner did reach its own last refusal -- "the errors name neither an authorization denial nor a
# capacity shortfall" -- which was honest and useless. A limit that is known before the first API call
# should be checked before the first API call.
# UD_LIMIT is EC2's, and is a variable only so the characterization suite can drive the refusal. A guard
# nothing has ever executed is a guard nobody knows the wording of, and this one's whole job is to be read.
# Stripping is a text transformation on a script nothing will parse until it is on a rented machine, so the
# result is parsed here. Dropping lines by indentation cannot cut a line out of the kind.yaml heredoc, whose
# comments are indented -- but "cannot" is the kind of claim this session has spent money disproving.
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
bash -n "$UD" || fail "the generated user-data does not parse after its comments were stripped; see $OUT/user-data.sh"
grep -q 'containerPath: /var/run/nvidia-container-devices/all' "$UD" \
  || fail "the generated user-data lost the device mount, so the stripping cut something that mattered"

UD_LIMIT="${UD_LIMIT:-25600}"
UD_ENCODED=$(base64 -w0 "$UD" | wc -c)
if [ "$UD_ENCODED" -gt "$UD_LIMIT" ]; then
  fail "the user-data encodes to $UD_ENCODED bytes and EC2 accepts $UD_LIMIT. It is $(wc -c < "$UD") bytes raw, in $(wc -l < "$UD") lines, after comments were stripped. Nothing was launched. Move the bulk out of the heredoc rather than trimming prose: see $OUT/user-data.sh"
fi
say "user-data: $UD_ENCODED of $UD_LIMIT encoded bytes"

# ---------------------------------------------------------------- launch
say "launching $INSTANCE_TYPE spot (max \$$MAX_SPOT_PRICE/h)"
TAGS="ResourceType=instance,Tags=[{Key=Name,Value=$STACK},{Key=purpose,Value=queuelab-device-observation}]"
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
        "$MAX_SPOT_PRICE" 300 "$UD" "$TAGS" 2>>"$OUT/launch-errors.txt") \
    && [ -n "$IID" ] && [ "$IID" != "None" ] && break
  IID=""
done
# The refusal names the cause the errors actually give, rather than assuming capacity.
#
# The first version of this line said a placement score of 3 makes an empty result "a normal answer rather
# than a fault". Every zone had in fact returned UnauthorizedOperation from a service control policy, and
# the message attributed a policy denial to Spot capacity -- describing a quantity by a cause its own
# evidence does not support, which is the defect this repository exists to avoid.
if [ -z "$IID" ]; then
  if grep -q "UnauthorizedOperation" "$OUT/launch-errors.txt" 2>/dev/null; then
    scp=$(grep -o 'service_control_policy/[a-z0-9-]*' "$OUT/launch-errors.txt" | head -1)
    fail "every zone refused $INSTANCE_TYPE with UnauthorizedOperation, not for want of capacity. An explicit deny in ${scp:-a service control policy} blocks ec2:RunInstances for this instance type, and an SCP is not something an account administrator can override. Check which families the policy allows before choosing another type. See $OUT/launch-errors.txt"
  fi
  if grep -qi "InsufficientInstanceCapacity\|capacity-not-available" "$OUT/launch-errors.txt" 2>/dev/null; then
    fail "no zone had Spot capacity for $INSTANCE_TYPE. At a placement score of 3 that is a normal answer rather than a fault: try again, or raise MAX_SPOT_PRICE. See $OUT/launch-errors.txt"
  fi
  fail "no zone would launch $INSTANCE_TYPE, and the errors name neither an authorization denial nor a capacity shortfall. See $OUT/launch-errors.txt"
fi
echo "$IID" > "$OUT/instance-id"
say "instance $IID"


say "waiting for results (driver, cluster, operator and four preflight checks come first)"
done_seen=0
ended_early=""
marker_rc=0
ended_early=$(spot_wait_for_marker "$REGION" "$BUCKET" "$RUN_ID/DONE" "$IID" \
              "$((HARD_STOP_SECONDS / 30))" 30) || marker_rc=$?
case "$marker_rc" in
  0) say "records are up"; done_seen=1 ;;
  2) say "instance ended before writing DONE" ;;
esac

for k in session.tgz commit.txt log.txt preflight.txt preflight-nodes.txt preflight-nvidia-smi.csv \
         preflight-why.txt device-util.tsv.gz device-scrape-first.txt device-scrape-last.txt; do
  aws s3 cp "s3://$BUCKET/$RUN_ID/$k" "$OUT/$k" >/dev/null 2>&1 || true
done
aws s3 cp --recursive "s3://$BUCKET/$RUN_ID/runs" "$OUT/runs" >/dev/null 2>&1 || true

if [ "$done_seen" -eq 0 ]; then
  # Partial evidence is the point of uploading per run, so it is reported rather than discarded.
  shopt -s nullglob
  recovered=("$OUT/runs"/*.json)
  partial=${#recovered[@]}
  shopt -u nullglob
  say "records recovered before the end: $partial"
  if [ -n "$ended_early" ]; then
    fail "the instance was $ended_early before it wrote its completion marker; $partial record(s) were recovered and $OUT/log.txt is whatever it managed to upload"
  fi
  fail "no completion marker within the hard stop; $partial record(s) were recovered and $IID has been terminated"
fi

[ -s "$OUT/session.tgz" ] || fail "no session archive was written; $OUT/log.txt may say why"
tar -xzf "$OUT/session.tgz" -C "$OUT" && say "records unpacked to $OUT/session"

say "the comparison is NOT run here: it belongs to whoever reads the records"
say "when they are complete:  queuelabrun -compare '$OUT/session/gpu-grace-bounded-*.json'"
say "reading 1 is whether that command prints its comparison WITHOUT the device: NOT OBSERVED line"
