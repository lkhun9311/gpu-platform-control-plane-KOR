#!/usr/bin/env bash
#
# M5-b gateway path: drive the benchmark harness through the REAL gateway on kind, with no GPU.
#
# hack/m5b-harness-dryrun.sh proves the gen -> replay -> report plumbing, but it points --target straight at
# the stub, so the gateway never sees a request. Four fixes were made to the gateway's measurement path
# (one shared outbound Transport, MaxIdleConnsPerHost=600, GetBody for a rewindable proxied POST, and a
# bounded model label on the unresolved-model paths) and every one of them is verified by unit tests only.
# This script is the load-bearing check that they hold under real traffic, because a GPU run on an
# unvalidated gateway buys a contaminated number.
#
# The path is: benchharness replay -> gateway (in kind) -> stub backend (an InferenceDeployment's Service).
#
# It captures evidence to hack/m5b-gateway-path-evidence.log and tears the cluster down at the end, since
# every number it reports is read back from a Service inside the cluster while the run is still going.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
export PATH="$PWD/bin:$PATH"
export GOTOOLCHAIN=go1.26.0

CLUSTER=platform
KCTX=kind-$CLUSTER
NS=gpu-platform-control-plane-system
SERVING_NS=serving
LOG=hack/m5b-gateway-path-evidence.log

# The stub image is tagged :evidence, not :latest, and that is load-bearing rather than cosmetic.
#
# Kubernetes defaults imagePullPolicy to Always for a :latest tag and to IfNotPresent for any other, so a
# :latest stub would send the kubelet looking for a registry that does not exist for a side-loaded image.
# The operator's and gateway's Deployments can be patched to IfNotPresent because nothing else owns them, but
# the stub's Deployment is rewritten by the InferenceDeployment controller on every reconcile
# (mutateDeployment replaces the whole container), so a patch there is reverted and the two fight in a
# rollout loop until the wait times out. Choosing the tag avoids the argument entirely.
STUB_IMG=benchharness:evidence

# Host ports published by hack/kind-config-gateway-path.yaml.
GW_PORT=30080
STUB_FAST_PORT=30081
GW_METRICS_PORT=30082
STUB_SLOW_PORT=30083

# The harness's own defaults, quoted here because the constant under test is derived from them.
#
# MaxIdleConnsPerHost=600 is documented as -rate 20/s x -timeout-ms 30s, so the main run uses exactly those
# two values; using anything else would measure a different ceiling than the one the comment claims.
RATE=20
TIMEOUT_MS=30000
# 45s rather than 60s: the shared trace is now replayed five times (two client generations through the
# gateway, three straight at the stub), so this keeps the load phase near its previous total while still
# offering ~440 premium requests per arm, which is enough for a nearest-rank p99 to land on an actually
# observed latency.
DURATION_MS=45000

# The slow-backend profile exists to push outbound concurrency up on purpose.
#
# At the harness defaults a fast stub answers in milliseconds, so in-flight requests never accumulate and the
# run would say nothing about a pool cap of 600. A backend that takes seconds is what makes concurrency
# observable, and it is the regime a real vLLM prefill actually lives in.
SLOW_RATE=40
SLOW_DURATION_MS=20000

WORK="$(mktemp -d)"
BH="$WORK/benchharness"

: > "$LOG"

log()  { echo -e "$*" | tee -a "$LOG"; }
step() { echo -e "\n===== $* =====" | tee -a "$LOG"; }
run()  { echo "+ $*" | tee -a "$LOG"; "$@" >>"$LOG" 2>&1; }
cap()  { echo "+ $*" | tee -a "$LOG"; "$@" 2>&1 | tee -a "$LOG"; }
die()  { echo "FAILED at: $*" | tee -a "$LOG"; teardown; exit 1; }

k() { kubectl --context "$KCTX" "$@"; }

# teardown removes the cluster and the scratch directory.
#
# Unlike hack/m6-kind-e2e.sh this run does NOT leave the cluster up for inspection: everything it measures is
# read out of the cluster while the load is running and written into the log, so a surviving cluster adds no
# evidence and only risks the next run reusing it. A reused cluster is what invalidated an earlier evidence
# document in this repository (docs/10_WHAT_I_GOT_WRONG.md).
teardown() {
  rm -rf "$WORK"
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "+ kind delete cluster --name $CLUSTER" | tee -a "$LOG"
    kind delete cluster --name "$CLUSTER" >>"$LOG" 2>&1
  fi
}

# stub_stats fetches one stub's counters as compact JSON.
stub_stats() { curl -sS --max-time 10 "http://127.0.0.1:$1/stats"; }

# stub_reset starts a fresh measurement window on one stub.
# POST, because the stub now refuses a GET here: a GET is what a probe or a stray curl issues, and this
# endpoint discards the connection counters this script is in the middle of collecting.
stub_reset() { curl -sS -X POST --max-time 10 "http://127.0.0.1:$1/stats/reset" >/dev/null; }

# jnum reads one number out of a stub stats JSON blob.
jnum() { echo "$1" | jq -r ".$2"; }

# gw_metrics fetches the gateway's Prometheus exposition.
gw_metrics() { curl -sS --max-time 10 "http://127.0.0.1:$GW_METRICS_PORT/metrics"; }

# requests_series counts the distinct label sets currently present on gpuaas_gateway_requests_total.
#
# This is the quantity the unresolved-model sentinel exists to bound: a counter's series are never reclaimed,
# so if the requested model name reached the label set, this number would grow once per distinct name.
requests_series() { gw_metrics | grep -c '^gpuaas_gateway_requests_total{'; }

# requests_models lists the distinct model label values on that same metric.
requests_models() {
  gw_metrics | grep '^gpuaas_gateway_requests_total{' | sed 's/.*model="\([^"]*\)".*/\1/' | sort -u
}

# arm_row prints one arm's row from a rendered report, which is where the p50/p95/p99 columns come from.
arm_row() { "$BH" report --raw "$1" 2>/dev/null | awk '$1=="off"'; }

# p_col prints one latency column of that row (6=p50, 7=p95, 8=p99).
p_col() { arm_row "$1" | awk -v c="$2" '{print $c}'; }

step "0. preflight"
for tool in docker kind kubectl kustomize jq curl go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool"; exit 1; }
done
cap docker version --format '{{.Server.Version}}'
cap go version

step "1. build benchharness on the host"
run go build -o "$BH" ./cmd/benchharness || die "build benchharness"

step "2. recreate the kind cluster ($CLUSTER)"
# Deleted unconditionally rather than reused.
#
# An earlier evidence document in this repository was invalidated because it was captured from a cluster the
# script had reused, which still carried objects from a previous experiment.
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "deleting the existing cluster $CLUSTER so this run starts from nothing"
  run kind delete cluster --name "$CLUSTER"
fi
kind create cluster --name "$CLUSTER" --config hack/kind-config-gateway-path.yaml >>"$LOG" 2>&1 || die "kind create cluster"
cap k get nodes -o wide

step "3. install Kueue v0.18.3"
# Kueue is not exercised by this run at all.
#
# It is installed because the operator's GPUQuotaPolicy and MLTrainingJob controllers watch Kueue kinds, so
# the manager fails to start its caches without those CRDs, and the InferenceDeployment controller is what
# turns the stub CR into the Deployment and Service the gateway routes to.
k apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.3/manifests.yaml >>"$LOG" 2>&1 || die "kueue apply"
k -n kueue-system wait --for=condition=Available deploy/kueue-controller-manager --timeout=300s >>"$LOG" 2>&1 || die "kueue not Available"

step "4. build and load the operator, gateway and benchharness images"
run make docker-build IMG=controller:latest || die "operator image build"
run make docker-build-gateway GATEWAY_IMG=gateway:latest || die "gateway image build"
run make docker-build-benchharness BENCHHARNESS_IMG="$STUB_IMG" || die "benchharness image build"
run kind load docker-image controller:latest --name "$CLUSTER" || die "kind load operator"
run kind load docker-image gateway:latest --name "$CLUSTER" || die "kind load gateway"
run kind load docker-image "$STUB_IMG" --name "$CLUSTER" || die "kind load benchharness"

step "5. install CRDs and deploy the operator"
# kustomize build config/crd, not make install.
#
# make install depends on the manifests target, which runs controller-gen and rewrites the generated API and
# CRD files. This script only needs the CRDs applied, so it applies what is already committed rather than
# regenerating anything on the way to an evidence run.
kustomize build config/crd | k apply --server-side -f - >>"$LOG" 2>&1 || die "install CRDs"
k create namespace "$NS" --dry-run=client -o yaml | k apply -f - >>"$LOG" 2>&1 || die "create operator namespace"
kustomize build config/operator | k apply --server-side -f - >>"$LOG" 2>&1 || die "deploy operator"
DEP=$(k -n "$NS" get deploy -o name | grep controller-manager | head -1)
log "operator deployment: $DEP"
# The images are :latest and were side-loaded, so the kubelet must be told not to go looking for a registry.
k -n "$NS" patch "$DEP" --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >>"$LOG" 2>&1 || true
k -n "$NS" rollout status "$DEP" --timeout=180s >>"$LOG" 2>&1 || die "operator rollout"

step "6. deploy the gateway"
kustomize build config/gateway | k apply --server-side -f - >>"$LOG" 2>&1 || die "deploy gateway"
k -n "$NS" patch deploy/gateway --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >>"$LOG" 2>&1 || true
# The api-keys Secret is what resolveTenant reads: each Secret KEY is an API key and its VALUE is the tenant.
#
# The two tenants are the harness's own (cmd/benchharness/main.go builds a premium-1 and a standard-noisy
# tenant into every trace), so these keys are the ones the replay's --api-keys flag hands back.
k -n "$NS" create secret generic gateway-api-keys \
  --from-literal=premium-key=premium-1 \
  --from-literal=standard-key=standard-noisy >>"$LOG" 2>&1 || die "create api-keys secret"
k -n "$NS" rollout restart deploy/gateway >>"$LOG" 2>&1 || true
k -n "$NS" rollout status deploy/gateway --timeout=180s >>"$LOG" 2>&1 || die "gateway rollout"

step "7. apply tenant policies and the stub InferenceDeployments"
run k apply -f config/kueue/resourceflavor.yaml || die "resourceflavor"
k create namespace "$SERVING_NS" --dry-run=client -o yaml | k apply -f - >>"$LOG" 2>&1 || die "create serving namespace"

# Both tenants get a policy targeting the same namespace, which is the M5-b topology: one backend, two
# tenants, one of them noisy.
#
# trainingQuota: true keeps the GPU ceiling in a Kueue ClusterQueue instead of a namespace ResourceQuota.
# That matters here for a reason unrelated to training: a ResourceQuota naming requests.nvidia.com/gpu forces
# every Pod in the namespace to declare that resource, and the stub Pods declare no GPU at all, so a
# namespace quota would block the very backend this run needs.
#
# rateLimit is deliberately unset, which bucketRegistry.Allow reads as an unlimited tenant. The measurement
# is of the proxy path; a limiter rejecting requests would replace the latency being measured with 429s.
apply_policy() {
  local tenant=$1
  cat <<EOF | k apply -f - >>"$LOG" 2>&1
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: ${tenant}-quota
spec:
  tenant: ${tenant}
  targetNamespace: ${SERVING_NS}
  gpuClass: l40s
  limits:
    gpuCount: 4
  trainingQuota: true
EOF
  log "+ applied GPUQuotaPolicy ${tenant}-quota (tenant ${tenant} -> namespace ${SERVING_NS})"
}
apply_policy premium-1 || die "policy premium-1"
apply_policy standard-noisy || die "policy standard-noisy"

# The stub's response profile travels in storageUri.
#
# The InferenceDeployment controller builds every serving container with exactly `--model <name> --model-path
# <storageUri>`, so the storage URI is the only per-deployment knob a backend has; stub-serve reads a
# "stub://" URI as its profile and ignores anything else.
#
# gpuCount is 0 on purpose: this is a CPU-only stub and there is no GPU, real or simulated, in this cluster.
apply_infd() {
  local name=$1 profile=$2
  cat <<EOF | k apply -f - >>"$LOG" 2>&1
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata:
  name: ${name}
  namespace: ${SERVING_NS}
spec:
  model:
    name: ${name}
    storageUri: "${profile}"
  image: ${STUB_IMG}
  gpuCount: 0
  replicas: 1
  port: 8090
EOF
  log "+ applied InferenceDeployment ${SERVING_NS}/${name} (${profile})"
}
apply_infd llama-3-8b      'stub://profile?tokens=8&ttft-ms=5&itl-ms=2'      || die "infd fast"
apply_infd llama-3-8b-slow 'stub://profile?tokens=4&ttft-ms=2000&itl-ms=2'   || die "infd slow"

# The Deployments are created by the operator from the CRs above, so this waits for them to appear before it
# can wait for them to roll out.
for name in llama-3-8b llama-3-8b-slow; do
  for _ in $(seq 1 40); do
    k -n "$SERVING_NS" get deploy "$name" >/dev/null 2>&1 && break
    sleep 3
  done
  if ! k -n "$SERVING_NS" rollout status "deploy/$name" --timeout=180s >>"$LOG" 2>&1; then
    # Dump what the cluster thinks before giving up, since a stub that never starts is the failure mode most
    # likely to be an image or argument problem rather than anything about the gateway.
    cap k -n "$SERVING_NS" get pods -o wide
    cap k -n "$SERVING_NS" describe deploy "$name"
    cap k -n "$SERVING_NS" logs "deploy/$name" --tail=30
    die "stub rollout $name"
  fi
done

step "8. publish the evidence NodePorts"
# These Services exist only so the harness and this script can reach the cluster from the host.
#
# They are created here rather than in config/ deliberately. config/gateway/service.yaml keeps :8081 off the
# Service because /metrics carries per-tenant usage that other tenants must not be able to scrape, and that
# reasoning is right for the deployed topology. This run has to read those same series to check their
# cardinality, so it opens the port for the lifetime of a throwaway cluster and no longer.
cat > "$WORK/evidence-services.yaml" <<EOF
apiVersion: v1
kind: Service
metadata:
  name: gateway-evidence
  namespace: ${NS}
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: gpu-platform-control-plane
    app.kubernetes.io/component: gateway
  ports:
    - name: http
      port: 8080
      targetPort: http
      nodePort: ${GW_PORT}
    - name: metrics
      port: 8081
      targetPort: metrics
      nodePort: ${GW_METRICS_PORT}
---
apiVersion: v1
kind: Service
metadata:
  name: stub-fast-evidence
  namespace: ${SERVING_NS}
spec:
  type: NodePort
  selector:
    app.kubernetes.io/instance: llama-3-8b
  ports:
    - name: http
      port: 8090
      targetPort: http
      nodePort: ${STUB_FAST_PORT}
---
apiVersion: v1
kind: Service
metadata:
  name: stub-slow-evidence
  namespace: ${SERVING_NS}
spec:
  type: NodePort
  selector:
    app.kubernetes.io/instance: llama-3-8b-slow
  ports:
    - name: http
      port: 8090
      targetPort: http
      nodePort: ${STUB_SLOW_PORT}
EOF
k apply -f "$WORK/evidence-services.yaml" >>"$LOG" 2>&1 || die "evidence services"

cap k get inferencedeployment -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,MODEL:.spec.model.name,PHASE:.status.phase,READY:.status.readyReplicas
cap k get pods -A -o wide

step "9. smoke test: one request through the gateway"
# Nothing after this point means anything if the identity chain, the routing and the proxy are not all
# working, so this is checked before any load is offered rather than inferred from the load's own numbers.
SMOKE=""
for i in $(seq 1 40); do
  SMOKE=$(curl -sS --max-time 10 -o "$WORK/smoke.txt" -w '%{http_code}' \
    -H 'Authorization: Bearer premium-key' -H 'Content-Type: application/json' \
    -d '{"model":"llama-3-8b","messages":[{"role":"user","content":"hello"}],"stream":true}' \
    "http://127.0.0.1:$GW_PORT/v1/chat/completions" 2>>"$LOG")
  [ "$SMOKE" = "200" ] && break
  sleep 3
done
log "smoke status: $SMOKE"
cap head -3 "$WORK/smoke.txt"
[ "$SMOKE" = "200" ] || die "smoke test through the gateway did not return 200 (got $SMOKE)"

step "10. generate the shared trace (rate ${RATE}/s, ${DURATION_MS}ms, timeout ${TIMEOUT_MS}ms)"
# One trace, replayed twice.
#
# The gateway run and the no-gateway baseline must offer identical traffic or their p99 difference is not the
# gateway's hop, and the manifest's checksum is what makes that identity checkable rather than asserted.
cap "$BH" gen-trace \
  --seed 7 --duration-ms "$DURATION_MS" --rate "$RATE" --timeout-ms "$TIMEOUT_MS" \
  --arm off --model llama-3-8b --gateway-url "http://127.0.0.1:$GW_PORT" \
  --trace-out "$WORK/trace.jsonl" --manifest-out "$WORK/manifest.yaml" || die "gen-trace"

# replay_arm NAME TARGET POOL RAWOUT
#
# Runs one replay and leaves the stub's counters for that window in ARM_STATS.
#
# The result comes back in a global rather than on stdout because this function tees its progress there; a
# command substitution around it would capture the whole transcript along with the JSON, and `die` inside a
# substitution runs in a subshell and would fail to stop the script.
#
# The stub is reset immediately before each arm, so every arm's connection counts describe only its own load.
ARM_STATS=""
replay_arm() {
  local name=$1 target=$2 mode=$3 rawout=$4
  stub_reset "$STUB_FAST_PORT"
  log "--- arm $name: target $target, client mode $mode ---"
  cap "$BH" replay \
    --manifest "$WORK/manifest.yaml" \
    --target "$target" \
    --conn-mode "$mode" \
    --api-keys "premium-1=premium-key,standard-noisy=standard-key" \
    --raw-out "$rawout" || die "replay $name"
  ARM_STATS=$(stub_stats "$STUB_FAST_PORT")
}

step "11. RUN A: through the gateway, client BEFORE and AFTER"
# The same trace is replayed with the sender's old client and its new one.
#
# The old client had no Transport at all, so it used http.DefaultTransport and its two idle connections per
# host, and it abandoned each response body before EOF so even those two were rarely reusable. That cost lands
# inside the latency the harness reports, which makes it the instrument's error rather than the system's.
# Measuring both is the only way to say how large it was instead of asserting it.
replay_arm "gateway/legacy" "http://127.0.0.1:$GW_PORT" legacy "$WORK/raw-gw-before.jsonl"; STATS_GW_BEFORE=$ARM_STATS
log "[EVIDENCE] stub counters, gateway path, client legacy: $STATS_GW_BEFORE"
replay_arm "gateway/pooled" "http://127.0.0.1:$GW_PORT" pooled "$WORK/raw-gateway.jsonl"; STATS_GW=$ARM_STATS
log "[EVIDENCE] stub counters, gateway path, client pooled: $STATS_GW"

step "12. RUN B: straight to the stub, client legacy / drain-only / pooled"
# On this path the stub sees the harness's own connections directly, so it is the only place the client's
# effect on connection count is observable at all; through the gateway the stub only ever sees the gateway.
#
# Three arms, not two, because the fix was two changes and reporting them as one lump would hide which one
# did the work: draining the unread stream tail is what lets a connection be pooled at all, and sizing the
# pool is what stops it being thrown away once more than two are in flight.
replay_arm "direct/legacy" "http://127.0.0.1:$STUB_FAST_PORT" legacy "$WORK/raw-direct-before.jsonl"; STATS_DIRECT_BEFORE=$ARM_STATS
log "[EVIDENCE] stub counters, direct path, client legacy:     $STATS_DIRECT_BEFORE"
replay_arm "direct/drain-only" "http://127.0.0.1:$STUB_FAST_PORT" drain-only "$WORK/raw-direct-drain.jsonl"; STATS_DIRECT_DRAIN=$ARM_STATS
log "[EVIDENCE] stub counters, direct path, client drain-only: $STATS_DIRECT_DRAIN"
replay_arm "direct/pooled" "http://127.0.0.1:$STUB_FAST_PORT" pooled "$WORK/raw-direct.jsonl"; STATS_DIRECT=$ARM_STATS
log "[EVIDENCE] stub counters, direct path, client pooled:     $STATS_DIRECT"

step "13. MEASUREMENT 1: connection reuse, gateway's pool and the harness's own"
GW_REQ=$(jnum "$STATS_GW" requestsServed)
GW_CONNS=$(jnum "$STATS_GW" chatConnections)
GW_MAXREQ=$(jnum "$STATS_GW" maxRequestsOnOneConnection)
GW_ACCEPTED=$(jnum "$STATS_GW" connectionsAccepted)
D_REQ=$(jnum "$STATS_DIRECT" requestsServed)
D_CONNS=$(jnum "$STATS_DIRECT" chatConnections)
DB_REQ=$(jnum "$STATS_DIRECT_BEFORE" requestsServed)
DB_CONNS=$(jnum "$STATS_DIRECT_BEFORE" chatConnections)
DD_REQ=$(jnum "$STATS_DIRECT_DRAIN" requestsServed)
DD_CONNS=$(jnum "$STATS_DIRECT_DRAIN" chatConnections)
log "gateway -> stub (gateway's own shared Transport): $GW_REQ requests over $GW_CONNS connections (max $GW_MAXREQ on one connection; $GW_ACCEPTED accepted in total including kubelet probes)"
if [ "$GW_CONNS" -gt 0 ] && [ "$GW_REQ" -gt 0 ]; then
  log "[EVIDENCE] gateway requests-per-connection: $(echo "scale=1; $GW_REQ / $GW_CONNS" | bc)"
fi
# A per-request Transport pools nothing, so it would show one connection per request.
# The pass condition is therefore stated against that counterfactual, not against a tuned threshold.
if [ "$GW_CONNS" -lt "$GW_REQ" ]; then
  log "  RESULT: PASS - connections ($GW_CONNS) are fewer than requests ($GW_REQ), so the shared Transport pooled."
else
  log "  RESULT: FAIL - one connection per request, which is what a per-request Transport looks like."
fi
log ""
log "harness -> stub, BEFORE     (legacy: DefaultTransport, no drain): $DB_REQ requests over $DB_CONNS connections"
log "harness -> stub, drain only (DefaultTransport + drain):         $DD_REQ requests over $DD_CONNS connections"
log "harness -> stub, AFTER      (sized pool + drain):               $D_REQ requests over $D_CONNS connections"
if [ "$DB_CONNS" -gt 0 ] && [ "$DD_CONNS" -gt 0 ] && [ "$D_CONNS" -gt 0 ]; then
  log "[EVIDENCE] harness requests-per-connection: before $(echo "scale=1; $DB_REQ / $DB_CONNS" | bc), drain-only $(echo "scale=1; $DD_REQ / $DD_CONNS" | bc), after $(echo "scale=1; $D_REQ / $D_CONNS" | bc)"
  log "[EVIDENCE] connections the instrument no longer opens: $(( DB_CONNS - D_CONNS )) of $DB_CONNS"
  log "[EVIDENCE] attribution: the drain accounts for $(( DB_CONNS - DD_CONNS )) of that, the pool size for $(( DD_CONNS - D_CONNS ))"
fi
if [ "$D_CONNS" -lt "$DB_CONNS" ]; then
  log "  RESULT: PASS - the sized client pool cut the instrument's own connections from $DB_CONNS to $D_CONNS."
else
  log "  RESULT: FAIL - the client pool did not reduce connections ($DB_CONNS -> $D_CONNS)."
fi

step "14. MEASUREMENT 4: latency, gateway vs direct, client pool before vs after"
for arm in gw-before gateway direct-before direct-drain direct; do
  log "--- report: $arm ---"
  cap "$BH" report --raw "$WORK/raw-$arm.jsonl"
done
GW_P50=$(p_col "$WORK/raw-gateway.jsonl" 6); GW_P95=$(p_col "$WORK/raw-gateway.jsonl" 7); GW_P99=$(p_col "$WORK/raw-gateway.jsonl" 8)
D_P50=$(p_col "$WORK/raw-direct.jsonl" 6);  D_P95=$(p_col "$WORK/raw-direct.jsonl" 7);  D_P99=$(p_col "$WORK/raw-direct.jsonl" 8)
GWB_P50=$(p_col "$WORK/raw-gw-before.jsonl" 6); GWB_P95=$(p_col "$WORK/raw-gw-before.jsonl" 7); GWB_P99=$(p_col "$WORK/raw-gw-before.jsonl" 8)
DB_P50=$(p_col "$WORK/raw-direct-before.jsonl" 6); DB_P95=$(p_col "$WORK/raw-direct-before.jsonl" 7); DB_P99=$(p_col "$WORK/raw-direct-before.jsonl" 8)
DD_P50=$(p_col "$WORK/raw-direct-drain.jsonl" 6); DD_P95=$(p_col "$WORK/raw-direct-drain.jsonl" 7); DD_P99=$(p_col "$WORK/raw-direct-drain.jsonl" 8)
log "[EVIDENCE] premium TTFT ms   gateway, client legacy:     p50=$GWB_P50 p95=$GWB_P95 p99=$GWB_P99"
log "[EVIDENCE] premium TTFT ms   gateway, client pooled:     p50=$GW_P50 p95=$GW_P95 p99=$GW_P99"
log "[EVIDENCE] premium TTFT ms   direct,  client legacy:     p50=$DB_P50 p95=$DB_P95 p99=$DB_P99"
log "[EVIDENCE] premium TTFT ms   direct,  client drain-only: p50=$DD_P50 p95=$DD_P95 p99=$DD_P99"
log "[EVIDENCE] premium TTFT ms   direct,  client pooled:     p50=$D_P50 p95=$D_P95 p99=$D_P99"
log "[EVIDENCE] instrument cost removed  gateway p50=$(echo "$GWB_P50 - $GW_P50" | bc) p95=$(echo "$GWB_P95 - $GW_P95" | bc) p99=$(echo "$GWB_P99 - $GW_P99" | bc) ms"
log "[EVIDENCE] instrument cost removed  direct  p50=$(echo "$DB_P50 - $D_P50" | bc) p95=$(echo "$DB_P95 - $D_P95" | bc) p99=$(echo "$DB_P99 - $D_P99" | bc) ms"
# Latency differences of a few tenths of a millisecond are not resolvable from one repetition of ~440 premium
# requests, so the reader is told the scale of the noise rather than left to over-read the digits.
#
# The connection counts above are exact counts and carry the argument; these deltas only have to not contradict them.
log "[EVIDENCE] noise check: the gateway path and the direct path differ by p99 $(echo "$GW_P99 - $D_P99" | bc) ms, and the gateway path is strictly the longer one, so any difference at or below that magnitude is at the noise floor of this run"
log "[EVIDENCE] premium TTFT ms   gateway: p50=$GW_P50 p95=$GW_P95 p99=$GW_P99"
log "[EVIDENCE] premium TTFT ms   direct : p50=$D_P50 p95=$D_P95 p99=$D_P99"
# The hop cost is taken from the two pooled-client arms, not the legacy ones.
#
# Both arms carry whatever the client costs, so the difference cancels it either way; the derived pair is used
# because it is what a real run will use, and quoting a hop cost measured on a retired instrument would be
# quoting a number nobody will ever reproduce.
log "[EVIDENCE] gateway hop cost (both arms, pooled client)  p50=$(echo "$GW_P50 - $D_P50" | bc) ms  p95=$(echo "$GW_P95 - $D_P95" | bc) ms  p99=$(echo "$GW_P99 - $D_P99" | bc) ms"
# The number that matters is the hop cost relative to the effect M5-b sets out to measure.
#
# M5-b's absolute-protection check calls a 25% rise in premium TTFT p99 the boundary of acceptable, so the
# gateway's own overhead has to be small against 25% of the baseline p99 or it would swamp the signal.
log "[EVIDENCE] M5-b's absolute-protection margin is 25% of baseline p99 = $(echo "scale=2; $D_P99 * 0.25" | bc) ms; the gateway hop costs $(echo "scale=2; $GW_P99 - $D_P99" | bc) ms of that"

step "15. RUN C: slow backend, to make outbound concurrency observable"
# rate ${SLOW_RATE}/s against a backend that takes ~2s means roughly ${SLOW_RATE} x 2 requests are in flight at
# once, which is the only way this GPU-free path can say anything about a per-host pool cap of 600.
cap "$BH" gen-trace \
  --seed 11 --duration-ms "$SLOW_DURATION_MS" --rate "$SLOW_RATE" --timeout-ms "$TIMEOUT_MS" \
  --arm off --model llama-3-8b-slow --gateway-url "http://127.0.0.1:$GW_PORT" \
  --trace-out "$WORK/trace-slow.jsonl" --manifest-out "$WORK/manifest-slow.yaml" || die "gen-trace slow"
stub_reset "$STUB_SLOW_PORT"
cap "$BH" replay \
  --manifest "$WORK/manifest-slow.yaml" \
  --target "http://127.0.0.1:$GW_PORT" \
  --api-keys "premium-1=premium-key,standard-noisy=standard-key" \
  --raw-out "$WORK/raw-slow.jsonl" || die "replay slow"
STATS_SLOW=$(stub_stats "$STUB_SLOW_PORT")
log "[EVIDENCE] slow stub counters after the gateway run: $STATS_SLOW"

step "16. MEASUREMENT 2: MaxIdleConnsPerHost = 600 against observed concurrency"
SLOW_REQ=$(jnum "$STATS_SLOW" requestsServed)
SLOW_CONNS=$(jnum "$STATS_SLOW" chatConnections)
SLOW_PEAK=$(jnum "$STATS_SLOW" peakInFlight)
SLOW_PEAKOPEN=$(jnum "$STATS_SLOW" peakOpenConnections)
GW_PEAK=$(jnum "$STATS_GW" peakInFlight)
GW_PEAKOPEN=$(jnum "$STATS_GW" peakOpenConnections)
log "cap under test: MaxIdleConnsPerHost = 600, derived as -rate 20/s x -timeout-ms 30s (internal/gateway/proxy.go)"
log "harness-default run (rate ${RATE}/s, fast backend): peak in-flight $GW_PEAK, peak open connections $GW_PEAKOPEN, distinct connections $GW_CONNS"
log "slow-backend run    (rate ${SLOW_RATE}/s, ~2s backend): $SLOW_REQ requests, peak in-flight $SLOW_PEAK, peak open connections $SLOW_PEAKOPEN, distinct connections $SLOW_CONNS"
PEAK_ANY=$(( SLOW_PEAK > GW_PEAK ? SLOW_PEAK : GW_PEAK ))
log "[EVIDENCE] highest concurrency observed anywhere in this run: $PEAK_ANY in flight against a cap of 600"
# The cap can be wrong in two directions and they are not symmetric.
#
# Too low is a correctness problem: it would evict connections that were about to be reused, which is the
# artifact the fix exists to remove, and it would show as connections roughly tracking requests.
# Too high is only a headroom claim, and it is the direction this run can actually falsify, since the cap is
# never reached at the parameters its own derivation quotes.
if [ "$PEAK_ANY" -ge 600 ]; then
  log "  RESULT: the cap of 600 WAS reached; it is a live limit under these parameters, not headroom."
else
  log "  RESULT: the cap of 600 was never approached (peak $PEAK_ANY, headroom $(echo "scale=1; 600 / $PEAK_ANY" | bc)x); it bounds nothing that this run offered, and no reuse was lost to it."
fi

step "17. MEASUREMENT 3: metric cardinality on the unresolved-model path"
BEFORE=$(requests_series)
log "gpuaas_gateway_requests_total series before: $BEFORE"
log "driving 12 requests, each naming a different model that no InferenceDeployment serves..."
for i in $(seq 1 12); do
  code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' \
    -H 'Authorization: Bearer premium-key' -H 'Content-Type: application/json' \
    -d "{\"model\":\"ghost-model-$i-$RANDOM\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
    "http://127.0.0.1:$GW_PORT/v1/chat/completions")
  log "  unknown model $i -> HTTP $code"
done
AFTER=$(requests_series)
log "gpuaas_gateway_requests_total series after:  $AFTER"
log "--- distinct model label values on gpuaas_gateway_requests_total ---"
requests_models | tee -a "$LOG"
DELTA=$(( AFTER - BEFORE ))
# 12 distinct names must not produce 12 new series.
#
# One new series is expected and correct: the 404s are the first requests to land on the sentinel label, so
# the (tenant, _unresolved, 404) combination is new. Anything approaching 12 would mean the requested name
# reached the label set.
if [ "$DELTA" -le 2 ]; then
  log "  RESULT: PASS - 12 distinct unknown model names added $DELTA series, not 12."
else
  log "  RESULT: FAIL - series grew by $DELTA for 12 distinct names, so the model label is unbounded."
fi
log "--- gateway series with the unresolved sentinel ---"
gw_metrics | grep '_unresolved' | tee -a "$LOG"

step "18. gateway resource use during the run"
# The gateway's manifest caps it at 500m CPU, so a throttled gateway would show up as latency this run would
# otherwise attribute to the proxy hop.
cap k -n "$NS" get deploy gateway -o jsonpath='{.spec.template.spec.containers[0].resources}'
log ""
cap k -n "$NS" logs deploy/gateway --tail=20

step "19. the existing GPU-free dry run, for provenance"
# hack/m5b-harness-dryrun.sh is the evidence this script exists to extend, so its numbers are captured here
# rather than quoted from memory.
#
# They are NOT the baseline for the gateway hop and must not be read as one: the dry run replays a different
# trace (rate 40/s for 3s, four arms) over loopback to an in-process stub, so it differs from the run above in
# arrival rate, duration, prompt mix and transport all at once. Step 12's no-gateway replay is the comparable
# baseline, because it differs from step 11 in exactly one thing.
cap ./hack/m5b-harness-dryrun.sh

step "DONE"
log "Every number above was read out of the running cluster and is in this log."
teardown
log "Cluster '$CLUSTER' deleted."
