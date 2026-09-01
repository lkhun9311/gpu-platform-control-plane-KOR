#!/usr/bin/env bash
# FR-002b — the HEAD backend goes away and a second one absorbs the request.
#
# WHY THIS IS A SEPARATE EXPERIMENT FROM FR-002
#
# FR-002 kills the pod behind the only backend, so every failure reaches the client and
# backend_fallbacks_total stays 0. That is the correct reading of that run and it leaves the retry path
# completely unexercised — the counter existing and reading zero proves nothing about whether it can move.
#
# A "backend" here is an InferenceDeployment, not a pod. backendsFor lists every InferenceDeployment serving
# the model, oldest first, and the tail is what tryBackends walks. Adding pod replicas does NOT exercise
# this: that is Service load balancing, one list entry down. The first attempt at this experiment scaled
# replicas to 2 and would have measured nothing.
#
# The head is taken away by scaling it to zero rather than by deleting a pod. A deleted pod races with its
# replacement, so a fallback might or might not be needed on any given request; a head with no endpoints
# fails deterministically, every time, until it is scaled back.
#
# WHAT MUST BE TRUE FOR THE RESULT TO MEAN ANYTHING
#
#   before   two backends serve the model, requests succeed, fallbacks counter is at a known value
#   after    requests STILL succeed, and the fallback counter moved
#
# The second half alone is not enough. A gateway that failed every request would leave the counter at zero
# too, and a run that only checked "the counter moved" would pass a gateway that fell back on every single
# request including the healthy ones.
set -uo pipefail

NS=${NS:-gpu-platform-control-plane-system}
SERV=${SERV:-serving}
HEAD=${HEAD:-stub-llm}
SPARE=${SPARE:-stub-llm-spare}
GW=${GW:-localhost:8080}
PROM=${PROM:-localhost:9090}
KEY=${KEY:-premium-1}
MODEL=${MODEL:-demo-llm}
OUT=${OUT:-./ex/chaos-fr002b.json}

say () { echo "  $*"; }
die () { echo "ABORT: $*" >&2; exit 1; }
now_ns () { date +%s%N; }
ms () { echo "scale=1; $1 / 1000000" | bc; }

req () {
  curl -s -o /dev/null -m 3 -w '%{http_code}' -X POST "http://$GW/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" 2>/dev/null
}

promq () {
  curl -sG "http://$PROM/api/v1/query" --data-urlencode "query=$1" 2>/dev/null | python3 -c "
import json,sys
try: r=json.load(sys.stdin)['data']['result']
except Exception: print('0'); raise SystemExit
print(r[0]['value'][1] if r else '0')"
}

endpoints () { kubectl -n "$SERV" get endpoints "$1" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null; }

restore () {
  echo
  say "restoring $HEAD"
  kubectl -n "$SERV" patch inferencedeployment "$HEAD" --type=merge -p '{"spec":{"replicas":1}}' >/dev/null 2>&1
  for _ in $(seq 60); do [ -n "$(endpoints "$HEAD")" ] && { say "$HEAD serving again"; return; }; sleep 2; done
  say "WARNING: $HEAD never came back; the next run's steady state will refuse to start"
}
trap restore EXIT

# ---------------------------------------------------------------- steady state
say "=== steady state ==="

for d in "$HEAD" "$SPARE"; do
  kubectl -n "$SERV" get inferencedeployment "$d" >/dev/null 2>&1 || die "InferenceDeployment $d does not exist"
  [ -n "$(endpoints "$d")" ] || die "$d has no endpoints; there is no working pair to fall back between"
done
say "both backends have endpoints"

# The two must serve the SAME model, or the gateway never considers them alternatives and this measures
# nothing at all.
for d in "$HEAD" "$SPARE"; do
  m=$(kubectl -n "$SERV" get inferencedeployment "$d" -o jsonpath='{.spec.model.name}' 2>/dev/null)
  [ "$m" = "$MODEL" ] || die "$d serves model '$m', not '$MODEL'; they are not alternatives"
done
say "both serve model $MODEL"

# Oldest-first is the routing order, so the head must actually be the older of the two.
H_TS=$(kubectl -n "$SERV" get inferencedeployment "$HEAD" -o jsonpath='{.metadata.creationTimestamp}')
S_TS=$(kubectl -n "$SERV" get inferencedeployment "$SPARE" -o jsonpath='{.metadata.creationTimestamp}')
[[ "$H_TS" < "$S_TS" ]] || die "$HEAD ($H_TS) is not older than $SPARE ($S_TS); the head is not the one being removed"
say "$HEAD is the head (older): $H_TS < $S_TS"

ok=0
for _ in $(seq 5); do [ "$(req)" = "200" ] && ok=$((ok+1)); done
[ "$ok" -eq 5 ] || die "only $ok of 5 pre-fault requests succeeded"
say "5/5 pre-fault requests returned 200"

FALLBACKS_BEFORE=$(promq 'sum(gpuaas_gateway_backend_fallbacks_total)')
say "backend_fallbacks_total before: $FALLBACKS_BEFORE"

# ---------------------------------------------------------------- inject
echo
say "=== inject: scaling the head to zero ==="
T0=$(now_ns)
kubectl -n "$SERV" patch inferencedeployment "$HEAD" --type=merge -p '{"spec":{"replicas":0}}' >/dev/null 2>&1 \
  || die "scale down failed"

GONE=""
for _ in $(seq 120); do
  [ -z "$(endpoints "$HEAD")" ] && { GONE=$(now_ns); break; }
  sleep 0.5
done
[ -n "$GONE" ] || die "$HEAD still has endpoints; the head was never actually removed"
say "head lost its endpoints $(ms $(( GONE - T0 )))ms after the scale-down"

# ---------------------------------------------------------------- effect
echo
say "=== effect: do requests still succeed, and did the gateway fall back ==="
OK=0; FAIL=0; CODES=""
for _ in $(seq 20); do
  C=$(req); CODES="$CODES$C "
  [ "$C" = "200" ] && OK=$((OK+1)) || FAIL=$((FAIL+1))
  sleep 0.2
done
say "$OK of 20 requests succeeded with the head gone"
say "codes: $(echo "$CODES" | tr ' ' '\n' | sort | uniq -c | tr '\n' ' ')"

sleep 20   # one scrape at least
FALLBACKS_AFTER=$(promq 'sum(gpuaas_gateway_backend_fallbacks_total)')
DELTA=$(echo "$FALLBACKS_AFTER - $FALLBACKS_BEFORE" | bc 2>/dev/null || echo 0)
say "backend_fallbacks_total after:  $FALLBACKS_AFTER  (delta $DELTA)"

SERVED=false;  [ "$OK" -ge 18 ] && SERVED=true
FELL_BACK=false; [ "$(echo "$DELTA > 0" | bc 2>/dev/null)" = "1" ] && FELL_BACK=true

if [ "$SERVED" = true ] && [ "$FELL_BACK" = true ]; then
  say "the spare absorbed the traffic AND the counter recorded it"
elif [ "$SERVED" = true ]; then
  say "requests succeeded but the fallback counter did not move — the gateway is serving them some other way, and the metric an operator would rely on is silent"
else
  say "requests FAILED with a healthy spare one list entry away — the fallback path did not engage"
fi

# ---------------------------------------------------------------- record
mkdir -p "$(dirname "$OUT")"
cat > "$OUT" <<EOF
{
  "experiment": "FR-002b head backend removed, spare must absorb",
  "model": "$MODEL",
  "head": "$SERV/$HEAD",
  "spare": "$SERV/$SPARE",
  "injection": "scale the head InferenceDeployment to zero replicas",
  "why": "a backend is an InferenceDeployment, not a pod; adding pod replicas exercises Service load balancing rather than the gateway's retry path",
  "steadyStateEstablished": true,
  "headEndpointsGoneMs": $(ms $(( GONE - T0 ))),
  "requests": { "sent": 20, "succeeded": $OK, "failed": $FAIL },
  "backendFallbacks": { "before": $FALLBACKS_BEFORE, "after": $FALLBACKS_AFTER, "delta": $DELTA },
  "verdict": {
    "stillServed": $SERVED,
    "counterMoved": $FELL_BACK,
    "note": "both are required. Requests succeeding alone could mean the head never really left; the counter moving alone could mean every request is falling back, including ones that should not."
  }
}
EOF
echo
say "record written to $OUT"
[ "$SERVED" = true ] && [ "$FELL_BACK" = true ]
