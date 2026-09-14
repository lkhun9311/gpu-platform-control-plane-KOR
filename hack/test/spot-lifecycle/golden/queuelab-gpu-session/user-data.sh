#!/bin/bash
exec > >(tee /var/log/qlgpu.log) 2>&1
set -x
( sleep 8400; shutdown -h now ) &
BUCKET="stub-bucket"
PREFIX="run"
SOURCE_SHA="<SHA256>"
RUNNER_SHA="<SHA>"
COMMIT="<COMMIT>"
REPS="4"
DOSES="grace-bounded"
STUDY="reclaim"
upload() { aws s3 cp "$1" "s3://$BUCKET/$PREFIX/$2" || true; }
trap 'upload /var/log/qlgpu.log log.txt; shutdown -h now' EXIT
aws s3 cp "s3://$BUCKET/$PREFIX/src/source.tgz" /tmp/source.tgz
got=$(sha256sum /tmp/source.tgz | cut -d' ' -f1)
if [ "$got" != "$SOURCE_SHA" ]; then
  echo "source checksum mismatch: expected $SOURCE_SHA, got $got"
  exit 1
fi
mkdir -p /src && tar -xzf /tmp/source.tgz -C /src
cd /src
aws s3 cp "s3://$BUCKET/$PREFIX/bin/queuelabrun" /src/queuelabrun
chmod +x /src/queuelabrun
got=$(sha256sum /src/queuelabrun | cut -d' ' -f1)
if [ "$got" != "$RUNNER_SHA" ]; then
  echo "queuelabrun checksum mismatch: expected $RUNNER_SHA, got $got"
  exit 1
fi
export QUEUELABRUN_PREBUILT=1
nvidia-smi --query-gpu=index,name,memory.total --format=csv > /tmp/nvidia-smi.csv || exit 1
upload /tmp/nvidia-smi.csv preflight-nvidia-smi.csv
cards=$(tail -n +2 /tmp/nvidia-smi.csv | wc -l)
if [ "$cards" -lt 4 ]; then
  echo "PREFLIGHT FAILED: $cards cards, the protocol needs 4 (2 for the trace, 2 held by the occupier)"
  exit 1
fi
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
export KUBECONFIG=/tmp/kubeconfig
echo "HOME=${HOME:-<unset>} KUBECONFIG=$KUBECONFIG"
kind create cluster --config /tmp/kind.yaml --kubeconfig "$KUBECONFIG" --wait 300s || exit 1
kubectl cluster-info || { echo "PREFLIGHT FAILED: the cluster is up but unreachable through $KUBECONFIG"; exit 1; }
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
docker exec qlgpu-worker systemctl restart containerd || exit 1
kubectl wait --for=condition=Ready node/qlgpu-worker --timeout=300s \
  || { echo "PREFLIGHT FAILED: qlgpu-worker did not come back Ready after its containerd was restarted"; exit 1; }
kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.3/manifests.yaml
kubectl -n kueue-system wait --for=condition=Available deploy/kueue-controller-manager --timeout=300s || exit 1
make docker-build IMG=controller:latest || exit 1
kind load docker-image controller:latest --name qlgpu || exit 1
kubectl apply --server-side -f config/crd/bases/ || exit 1
sed -i -e 's|^    newName: .*|    newName: controller|' \
       -e 's|^    digest: .*|    newTag: latest|' config/manager/kustomization.yaml
grep -q '^    newName: controller$' config/manager/kustomization.yaml \
  || { echo "PREFLIGHT FAILED: could not repoint the operator image at the locally built one"; exit 1; }
kubectl kustomize config/operator | kubectl apply --server-side -f - || exit 1
kubectl -n gpu-platform-control-plane-system patch deploy gpu-platform-control-plane-controller-manager \
  --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
  || exit 1
kubectl -n gpu-platform-control-plane-system rollout status \
  deploy/gpu-platform-control-plane-controller-manager --timeout=300s \
  || { echo "PREFLIGHT FAILED: the operator never became Available"; exit 1; }
kubectl label node qlgpu-worker platform.lkhun9311.github.io/gpu=true --overwrite || exit 1
kubectl kustomize config/nvidia-device-plugin | kubectl apply -f - || exit 1
kubectl kustomize config/dcgm-exporter | kubectl apply -f - || exit 1
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
REPS="$REPS" EXDIR="$EXDIR" DOSES="$DOSES" STUDY="$STUDY" RUN_STUDY=1 \
  bash hack/gpu-session.sh qlgpu-worker >/tmp/study.log 2>&1 || rc=$?
kill "$UPLOADER" 2>/dev/null || true
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
for f in "$EXDIR"/*.json; do
  [ -e "$f" ] && upload "$f" "runs/$(basename "$f")"
done
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
