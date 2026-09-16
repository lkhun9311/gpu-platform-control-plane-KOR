#!/usr/bin/env bash
#
# The M5-b chain, end to end, with a real engine: benchharness -> real gateway -> real vLLM.
#
# hack/m5b-harness-dryrun.sh points --target at a stub and never involves the gateway.
# hack/m5b-gateway-path.sh does put the real gateway in the path, and its backend is still a stub that
# answers instantly, so nothing in either script has ever exercised the one thing arm C is: a guard reading
# an engine's telemetry and rejecting on it. This does.
#
# The engine runs OUTSIDE the cluster, on the host, and is reached through a selectorless Service whose
# EndpointSlice points at the kind bridge gateway. That avoids side-loading a 7.5 GB image into a kind node
# and, more importantly, it is the only shape available: InferenceDeploymentSpec has no args and no volumes,
# so it cannot describe a vLLM container at all -- not --dtype, not the /dev/shm mount the engine dies
# without. The InferenceDeployment here is a ROUTING RECORD, nothing more. See the note at the end.
#
# It creates everything in its own namespace and deletes it on exit. It does not touch the cluster's
# existing gateway, operator, or serving namespace, and it never deletes the cluster.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export GOTOOLCHAIN=go1.26.0

NS=m5b-chain
KCTX="${KCTX:-kind-platform}"
VLLM_PORT="${VLLM_PORT:-18000}"
VLLM_MODEL="${VLLM_MODEL:-Qwen/Qwen2.5-0.5B-Instruct}"
VLLM_IMAGE="${VLLM_IMAGE:-vllm/vllm-openai-cpu:v0.27.1-x86_64}"
GW_IMAGE="${GW_IMAGE:-gateway:m5b-chain}"
WORK="$(mktemp -d)"
LOG=hack/m5b-chain-live-evidence.log

k() { kubectl --context "$KCTX" "$@"; }
say() { echo "== $*"; }
fail() { echo "CHAIN FAILED: $*" >&2; exit 1; }

cleanup() {
  if [ -n "${KEEP:-}" ]; then
    say "KEEP set: leaving namespace $NS, the policies, and the engine in place for inspection"
    [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null
    return
  fi
  say "cleanup"
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null
  k delete namespace "$NS" --wait=false >/dev/null 2>&1
  k delete gpuquotapolicy m5b-premium m5b-standard >/dev/null 2>&1
  k delete clusterrolebinding m5b-chain-gateway-role >/dev/null 2>&1
  docker rm -f m5b-vllm >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

# The host address pods reach. kind puts its nodes on a docker network of its own, so this is that
# network's gateway rather than docker0's -- reading it rather than hardcoding 172.18.0.1, because the
# subnet is allocated and a second kind cluster on the machine shifts it.
HOST_IP="$(docker network inspect kind -f '{{range .IPAM.Config}}{{if eq (len (split .Subnet ".")) 4}}{{.Gateway}}{{end}}{{end}}' 2>/dev/null)"
[ -n "$HOST_IP" ] || fail "could not read the kind network's gateway address"
say "host reachable from pods at $HOST_IP"

say "start vLLM on the host"
docker rm -f m5b-vllm >/dev/null 2>&1
# --shm-size is not optional: the engine needs 160 MiB for one worker and a container's default is 64 MiB,
# and it dies during startup rather than degrading. Bound to 0.0.0.0 so the kind bridge can reach it.
docker run -d --name m5b-vllm --shm-size=2g -p "0.0.0.0:${VLLM_PORT}:8000" \
  -v /tmp/claude-1000/hf:/root/.cache/huggingface -e VLLM_CPU_KVCACHE_SPACE=1 \
  "$VLLM_IMAGE" "$VLLM_MODEL" --dtype bfloat16 --max-model-len 8192 --max-num-seqs 8 >/dev/null \
  || fail "could not start the engine"
for i in $(seq 1 60); do
  curl -sf -m 2 "http://127.0.0.1:${VLLM_PORT}/health" >/dev/null 2>&1 && break
  [ "$i" = 60 ] && fail "engine never became healthy"
  sleep 5
done
say "engine healthy"

say "build and load the gateway"
# CGO_ENABLED=0 is not a preference. The base below is distroless/static, which has no libc, and a
# dynamically linked binary fails there as "exec /gateway: no such file or directory" -- a message that
# names the path rather than the missing loader and reads like a bad COPY.
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/gateway" ./cmd/gateway || fail "build gateway"
cat > "$WORK/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static:nonroot
COPY gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
EOF
docker build -q -t "$GW_IMAGE" "$WORK" >/dev/null || fail "build gateway image"
kind load docker-image "$GW_IMAGE" --name "${KCTX#kind-}" >/dev/null 2>&1 || fail "load gateway image"

say "apply the namespace and routing records"
k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null 2>&1
k create namespace "$NS" >/dev/null || fail "create namespace"

k apply -f - >/dev/null <<EOF || fail "apply policies"
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5b-premium
  annotations:
    platform.lkhun9311.github.io/tier: premium
spec:
  tenant: premium-1
  targetNamespace: $NS
  gpuClass: t4
  limits:
    gpuCount: 1
---
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: m5b-standard
spec:
  tenant: standard-noisy
  targetNamespace: $NS
  gpuClass: t4
  limits:
    gpuCount: 1
EOF

# The Service is created BEFORE the InferenceDeployment on purpose. The controller refuses to adopt a
# Service it does not own (markDegraded, infdReasonServiceConflict) rather than overwriting it, so creating
# it first is what keeps the engine's address pointing at the engine. The router does not filter on phase,
# so a Degraded InferenceDeployment still routes -- which is the property this whole arrangement rests on.
k apply -f - >/dev/null <<EOF || fail "apply service"
apiVersion: v1
kind: Service
metadata:
  name: vllm-live
  namespace: $NS
spec:
  ports:
    - name: http
      port: 8000
      targetPort: $VLLM_PORT
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: vllm-live
  namespace: $NS
  labels:
    kubernetes.io/service-name: vllm-live
addressType: IPv4
ports:
  - name: http
    port: $VLLM_PORT
endpoints:
  - addresses: ["$HOST_IP"]
    conditions:
      ready: true
EOF

k apply -f - >/dev/null <<EOF || fail "apply inferencedeployment"
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata:
  name: vllm-live
  namespace: $NS
spec:
  model:
    name: $VLLM_MODEL
    storageUri: "hf://$VLLM_MODEL"
  # A no-op image, deliberately, and this is the sharpest thing the script has to say.
  #
  # InferenceDeploymentSpec carries model, image, gpuClass, gpuCount, replicas and port. It has no args and
  # no volumes, so it CANNOT describe the engine M5-b measures: not --dtype=half, which a T4 refuses to
  # start without, and not the /dev/shm mount the engine dies without. Naming the vLLM image here would
  # produce a Pod that crash-loops forever while contributing nothing, which is what the first run of this
  # script did. The record exists only so the gateway's router can resolve the model to a Service, and the
  # Service is the one created above.
  image: registry.k8s.io/pause:3.9
  gpuCount: 0
  # Zero, which the CRD allows (Minimum=0), so the operator's Deployment creates no Pod at all.
  #
  # Any other value produces a Pod the operator probes on the serving port, and nothing that can be named
  # in spec.image both answers that probe and is honest about what it is -- the vLLM image crash-loops
  # without its args, and a pause container fails the probe and crash-loops too. Zero says the true thing:
  # this record routes, and it does not run the engine.
  replicas: 0
  port: 8000
EOF

k create secret generic gateway-api-keys -n "$NS" \
  --from-literal=premium-key=premium-1 \
  --from-literal=standard-key=standard-noisy >/dev/null || fail "create secret"

say "deploy the gateway in kv-aware mode"
k create serviceaccount gateway -n "$NS" >/dev/null || fail "create sa"
# gateway-role is a ClusterRole; gateway-secret-reader is a namespaced Role, and binding it as though it
# were a ClusterRole fails at RUNTIME rather than at apply time -- the binding is created against a name
# that does not exist, the gateway starts, and only its Secret watch is refused. The Pod stays Running and
# every request falls to 401, which reads like a wrong API key.
k create clusterrolebinding "m5b-chain-gateway-role" --clusterrole=gateway-role \
  --serviceaccount="$NS:gateway" >/dev/null 2>&1
k apply -f - >/dev/null <<EOF || fail "apply secret-reader role"
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gateway-secret-reader
  namespace: $NS
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gateway-secret-reader
  namespace: $NS
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: gateway-secret-reader
subjects:
  - kind: ServiceAccount
    name: gateway
    namespace: $NS
EOF
k apply -f - >/dev/null <<EOF || fail "apply gateway"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway
  namespace: $NS
spec:
  replicas: 1
  selector:
    matchLabels: {app: m5b-gateway}
  template:
    metadata:
      labels: {app: m5b-gateway}
    spec:
      serviceAccountName: gateway
      containers:
        - name: gateway
          image: $GW_IMAGE
          imagePullPolicy: IfNotPresent
          args:
            - -admission-mode=kv-aware
            # Waiting is the arm a CPU engine can actually cross: it saturates its scheduler long before a
            # KV cache fills. The usage threshold stays at its shipped value because it is what the GPU run
            # uses, and lowering it here would make this a test of a number the real run never sets.
            - -admission-kv-engage-usage=0.85
            - -admission-kv-waiting-threshold=4
            - -admission-kv-release-sustain=5s
            - -admission-kv-scrape-interval=1s
            - -admission-kv-max-staleness=4s
            - -admission-long-threshold=4096
          env:
            - name: GATEWAY_NAMESPACE
              value: $NS
            - name: GATEWAY_API_KEY_SECRET
              value: gateway-api-keys
          ports:
            - containerPort: 8080
              name: http
EOF
k rollout status deploy/gateway -n "$NS" --timeout=120s >/dev/null || fail "gateway never became ready"

k port-forward -n "$NS" deploy/gateway 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
for i in $(seq 1 20); do
  curl -sf -m 1 -o /dev/null "http://127.0.0.1:18080/v1/chat/completions" -X POST \
    -H 'Content-Type: application/json' -d '{}' 2>/dev/null && break
  sleep 1
done

: > "$LOG"
say "one request through the whole chain"
code=$(curl -s -o "$WORK/first.json" -w '%{http_code}' -m 120 -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer premium-key' \
  -d "{\"model\":\"$VLLM_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say hello\"}],\"max_tokens\":8,\"stream\":true}")
echo "first request through gateway: HTTP $code" | tee -a "$LOG"
[ "$code" = 200 ] || { cat "$WORK/first.json" | tee -a "$LOG"; fail "the chain does not carry a request"; }
grep -q "data:" "$WORK/first.json" || fail "no SSE frames came back through the gateway"
head -2 "$WORK/first.json" | tee -a "$LOG"

# The probe prompt has to satisfy two constraints at once, and only the estimator's own error makes that
# possible.
#
# To be ELIGIBLE for the guard it must estimate at or above -admission-long-threshold: the gateway scores
# ceil(len/4), so 20,000 characters scores 5,000 against a threshold of 4,096. To be SERVABLE it must fit
# the engine's context window, and a byte-pair tokenizer collapses a run of one character hard -- 20,000 of
# them measure about 2,500 tokens, comfortably inside 8,192. A prompt that estimated honestly at 5,000
# tokens would not fit, and the premium half of this check could not be made at all.
#
# The first version used --max-model-len 1024 and the premium probe came back 400 rather than 200: vLLM
# refusing a prompt longer than its window. That was not a false alarm -- it proved the 429 above came from
# the GATEWAY and not from the engine, since the standard request never reached vLLM and the premium one
# did.
say "load the engine and watch the guard reject"
# The load generators' PIDs are kept so they can be KILLED rather than waited on.
#
# The first version ended with a bare `wait`, which blocks until every one of these finishes -- up to their
# own curl timeout each, long after the only interesting moment has passed. The run was cut off by its
# outer timeout while still sitting in that wait, cleanup ran, and the script's verdict line never
# executed: three successful observations in the log and no statement about them, with the exit code
# belonging to the wrapper rather than to the script. A harness that cannot reach its own conclusion is
# not a harness, and the failure lands on the SUCCESS path, which is the one that gets quoted.
LOAD_PIDS=()
for i in $(seq 1 24); do
  curl -s -o /dev/null -m 60 -X POST http://127.0.0.1:18080/v1/chat/completions \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer standard-key' \
    -d "{\"model\":\"$VLLM_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$(head -c 400 /dev/zero | tr '\0' 'a')\"}],\"max_tokens\":400,\"stream\":true}" &
  LOAD_PIDS+=($!)
done
sleep 6

rejected=0
premium_ok=0
for i in $(seq 1 40); do
  long=$(head -c 20000 /dev/zero | tr '\0' 'b')
  code=$(curl -s -o "$WORK/probe.json" -w '%{http_code}' -m 20 -X POST http://127.0.0.1:18080/v1/chat/completions \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer standard-key' \
    -d "{\"model\":\"$VLLM_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$long\"}],\"max_tokens\":8,\"stream\":true}")
  if [ "$code" = 429 ]; then
    rejected=1
    echo "standard long request rejected: HTTP 429" | tee -a "$LOG"
    cat "$WORK/probe.json" | tee -a "$LOG"; echo | tee -a "$LOG"
    pcode=$(curl -s -o /dev/null -w '%{http_code}' -m 60 -X POST http://127.0.0.1:18080/v1/chat/completions \
      -H 'Content-Type: application/json' -H 'Authorization: Bearer premium-key' \
      -d "{\"model\":\"$VLLM_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$long\"}],\"max_tokens\":8,\"stream\":true}")
    echo "premium, same prompt, while engaged: HTTP $pcode" | tee -a "$LOG"
    [ "$pcode" = 200 ] && premium_ok=1
    break
  fi
  sleep 1
done
for pid in "${LOAD_PIDS[@]}"; do kill "$pid" 2>/dev/null; done
wait "${LOAD_PIDS[@]}" 2>/dev/null

[ "$rejected" = 1 ] || fail "the guard never rejected: it read a real engine under real load and stayed open"
[ "$premium_ok" = 1 ] || fail "premium was not admitted while the guard was engaged; that is shedding, not protection"

echo "CHAIN OK: a real request crossed benchharness-shaped traffic -> gateway -> real vLLM, the guard engaged on the engine's own telemetry, rejected a long standard request with 429, and let premium through." | tee -a "$LOG"
