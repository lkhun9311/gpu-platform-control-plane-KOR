#!/bin/bash
exec > >(tee /var/log/m5c.log) 2>&1
set -x
( sleep 9000; shutdown -h now ) &
BUCKET="stub-bucket"
PREFIX="run"
SOURCE_SHA="<SHA256>"
GATEWAY_SHA="<SHA>"
HARNESS_SHA="<SHA>"
COMMIT="<COMMIT>"
REPS="1"
ARMS="R1 shared timeSlicing mps"
RATE="9.85"
PREMIUM_WEIGHT="1"
NOISY_WEIGHT="0.054"
PROBE_WEIGHT="0"
DURATION_MS="420000"
DEADLINE_EPOCH=$(( $(date +%s) + 8400 ))
upload() { aws s3 cp "$1" "s3://$BUCKET/$PREFIX/$2" || true; }
trap 'upload /var/log/m5c.log log.txt; shutdown -h now' EXIT
aws s3 cp "s3://$BUCKET/$PREFIX/src/source.tgz" /tmp/source.tgz
got=$(sha256sum /tmp/source.tgz | cut -d' ' -f1)
if [ "$got" != "$SOURCE_SHA" ]; then echo "source checksum mismatch: $SOURCE_SHA vs $got"; exit 1; fi
mkdir -p /src && tar -xzf /tmp/source.tgz -C /src
cd /src
echo "$COMMIT" > /tmp/commit.txt && upload /tmp/commit.txt commit.txt
mkdir -p /src/bin
for b in gateway benchharness; do
  aws s3 cp "s3://$BUCKET/$PREFIX/bin/$b" "/src/bin/$b"
  chmod +x "/src/bin/$b"
done
got=$(sha256sum /src/bin/gateway | cut -d' ' -f1)
if [ "$got" != "$GATEWAY_SHA" ]; then echo "gateway checksum mismatch"; exit 1; fi
got=$(sha256sum /src/bin/benchharness | cut -d' ' -f1)
if [ "$got" != "$HARNESS_SHA" ]; then echo "benchharness checksum mismatch"; exit 1; fi
nvidia-smi --query-gpu=index,name,memory.total --format=csv > /tmp/nvidia-smi.csv || exit 1
upload /tmp/nvidia-smi.csv preflight-nvidia-smi.csv
cards=$(tail -n +2 /tmp/nvidia-smi.csv | wc -l)
if [ "$cards" -ne 1 ]; then
  echo "PREFLIGHT FAILED: $cards cards; this matrix splits ONE card and two engines on two cards are not sharing"
  exit 1
fi
export DEBIAN_FRONTEND=noninteractive
if ! command -v nvidia-ctk >/dev/null; then
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
name: m5cgpu
nodes:
  - role: control-plane
  - role: worker
    extraMounts:
      - hostPath: /dev/null
        containerPath: /var/run/nvidia-container-devices/all
KINDEOF
export KUBECONFIG=/tmp/kubeconfig
kind create cluster --config /tmp/kind.yaml --kubeconfig "$KUBECONFIG" --wait 300s || exit 1
kubectl cluster-info || { echo "PREFLIGHT FAILED: the cluster is up but unreachable through $KUBECONFIG"; exit 1; }
kubectl kustomize config/crd | kubectl apply -f - || exit 1
kubectl create ns gpu-platform-control-plane-system --dry-run=client -o yaml | kubectl apply -f -
kubectl kustomize config/gateway > /tmp/gateway-all.yaml || exit 1
awk 'BEGIN{RS="\n---\n"} /(^|\n)kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding|ServiceAccount)(\n|$)/ {print "---"; print $0}' \
  /tmp/gateway-all.yaml > /tmp/gateway-rbac.yaml
grep -q "name: gateway-role" /tmp/gateway-rbac.yaml || { echo "PREFLIGHT FAILED: no gateway-role in the rendered overlay"; exit 1; }
kubectl apply -f /tmp/gateway-rbac.yaml || exit 1
docker exec m5cgpu-worker ln -sf /sbin/ldconfig /sbin/ldconfig.real || exit 1
docker exec m5cgpu-worker bash -c '
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
' || { echo "PREFLIGHT FAILED: could not configure the container runtime inside m5cgpu-worker"; exit 1; }
docker exec m5cgpu-worker systemctl restart containerd || exit 1
kubectl wait --for=condition=Ready node/m5cgpu-worker --timeout=300s \
  || { echo "PREFLIGHT FAILED: m5cgpu-worker did not come back Ready after its containerd was restarted"; exit 1; }
docker exec m5cgpu-worker nvidia-smi -L > /tmp/node-cards.txt 2>&1 || echo "(nvidia-smi failed inside the node)" >> /tmp/node-cards.txt
upload /tmp/node-cards.txt preflight-node-cards.txt
export KUBECONFIG=/tmp/kubeconfig
export PLATFORM=kind
export KCTX=kind-m5cgpu
export GPU_NODE=m5cgpu-worker
export DEADLINE_EPOCH
export GATEWAY_BIN=/src/bin/gateway
export BENCHHARNESS_BIN=/src/bin/benchharness
export RATE PREMIUM_WEIGHT NOISY_WEIGHT PROBE_WEIGHT DURATION_MS REPS ARMS
export OUT=/src/m5c-run
bash hack/m5c-matrix.sh; matrix_rc=$?
echo "matrix exited $matrix_rc"
if [ -d /src/m5c-run ]; then
  tar -czf /tmp/m5c-evidence.tgz -C /src m5c-run
  upload /tmp/m5c-evidence.tgz evidence.tgz
fi
kubectl get nodes -o wide > /tmp/nodes.txt 2>&1 || true
upload /tmp/nodes.txt nodes.txt
if [ "$matrix_rc" = "0" ]; then
  echo done > /tmp/DONE
  aws s3 cp /tmp/DONE "s3://$BUCKET/$PREFIX/DONE"
fi
