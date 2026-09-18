#!/usr/bin/env bash
# Does an alert rule actually fire, or does it only parse?
#
# The rules were recorded as verified because `promtool check rules` returned SUCCESS on all four. promtool
# checks the EXPRESSION: valid PromQL, resolvable functions, well-formed duration. It never touches the data,
# so it returns SUCCESS just as happily for a rule over a metric that no process in the cluster emits. Such a
# rule evaluates to an empty vector on every scrape: permanently inactive, permanently green, and
# indistinguishable from a system that is simply healthy.
#
# WHAT THIS DRIVES
#
# GatewayUpstreamErrorsReachingClients, expr sum by (model) (rate(requests_total{code=~"5.."}[5m])) > 0.1,
# for 5m. The backend is removed so every request ends 5xx at the client, then load is held long enough for
# the rule to go inactive -> pending -> firing. Watching pending appear is not enough: pending only says the
# expression matched once, and a rule can sit in pending forever if the condition flickers.
#
# WHAT IT CANNOT DRIVE
#
# The two kv-aware rules. They are written over series the guard publishes, the gateway here runs
# --admission-mode=off (the default), and a Prometheus Vec with no observed child emits no series at all. No
# amount of load creates the input. That is a property of the rules, not of this run, and the run reports it
# rather than working around it.
set -uo pipefail

GW=${GW:-localhost:8080}
PROM=${PROM:-localhost:9090}
NS=${NS:-serving}
ISVC=${ISVC:-stub-llm}
MODEL=${MODEL:-demo-llm}
KEY=${KEY:-premium-1}
ALERT=${ALERT:-GatewayUpstreamErrorsReachingClients}
RPS_SLEEP=${RPS_SLEEP:-0.4}
MAX_WAIT=${MAX_WAIT:-900}
OUT=${OUT:-./ex/alert-firing.json}

say () { echo "  $*"; }
die () { echo "ABORT: $*" >&2; exit 1; }

restore () {
  echo
  say "restoring $ISVC"
  kubectl -n "$NS" patch inferencedeployment "$ISVC" --type=merge -p '{"spec":{"replicas":1}}' >/dev/null 2>&1
}
trap restore EXIT

alert_state () {
  curl -s "http://$PROM/api/v1/alerts" 2>/dev/null | python3 -c "
import json,sys
try: a=json.load(sys.stdin)['data']['alerts']
except Exception: print('unreachable'); raise SystemExit
m=[x for x in a if x['labels'].get('alertname')=='$ALERT']
print(m[0]['state'] if m else 'inactive')"
}

hit () {
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "http://$GW/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
}

# ---------------------------------------------------------------- steady state
say "=== steady state ==="

curl -sf "http://$PROM/-/ready" >/dev/null 2>&1 || die "Prometheus is not answering at $PROM"
kubectl -n "$NS" get inferencedeployment "$ISVC" >/dev/null 2>&1 || die "no InferenceDeployment $ISVC in $NS"

# A rule already firing when the run starts would make the transition below meaningless.
S0=$(alert_state)
[ "$S0" = "inactive" ] || die "$ALERT is already '$S0'; the transition cannot be attributed to this run"

C0=$(hit)
[ "$C0" = "200" ] || die "the gateway answers $C0 before anything was broken; fix that first"
say "$ALERT inactive, gateway answering 200"

# ---------------------------------------------------------------- break it
echo
say "=== removing the backend ==="

# Scaling the Deployment is not enough: the InferenceDeployment controller owns it and puts the replica back
# within one reconcile, which is the recovery behaviour FR-002 already measures. The CR is the level that
# holds.
kubectl -n "$NS" patch inferencedeployment "$ISVC" --type=merge -p '{"spec":{"replicas":0}}' >/dev/null 2>&1 \
  || die "could not scale the InferenceDeployment down"

for _ in $(seq 20); do
  c=$(hit); case "$c" in 5*) break;; esac
  sleep 3
done
case "$c" in 5*) say "requests now end $c at the client";; *) die "requests still end $c; nothing to alert on";; esac

# ---------------------------------------------------------------- hold the load
echo
say "=== holding load until $ALERT fires ==="

T0=$(date +%s)
DEADLINE=$(( T0 + MAX_WAIT ))
SEEN_PENDING=""
STATE=inactive
REQ=0
ERR=0

while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  c=$(hit); REQ=$((REQ+1)); case "$c" in 5*) ERR=$((ERR+1));; esac

  # The state is polled far less often than requests are sent, since each poll is a round trip and the rule
  # only re-evaluates on Prometheus's own interval anyway.
  if [ $(( REQ % 25 )) -eq 0 ]; then
    STATE=$(alert_state)
    printf "\r    %s  requests=%-5s 5xx=%-5s state=%-8s" "$(date +%H:%M:%S)" "$REQ" "$ERR" "$STATE"
    [ "$STATE" = "pending" ] && [ -z "$SEEN_PENDING" ] && SEEN_PENDING=$(date +%s)
    [ "$STATE" = "firing" ] && break
  fi
  sleep "$RPS_SLEEP"
done
echo

T_FIRED=$(date +%s)
if [ "$STATE" != "firing" ]; then
  say "$ALERT never reached firing within ${MAX_WAIT}s (last state: $STATE)"
fi

# ---------------------------------------------------------------- record
mkdir -p "$(dirname "$OUT")"
BODY=$(curl -s "http://$PROM/api/v1/alerts" 2>/dev/null | python3 -c "
import json,sys
a=[x for x in json.load(sys.stdin)['data']['alerts'] if x['labels'].get('alertname')=='$ALERT']
print(json.dumps(a[0] if a else {}, indent=2))")

PENDING_AFTER=null
[ -n "$SEEN_PENDING" ] && PENDING_AFTER=$(( SEEN_PENDING - T0 ))
FIRED_AFTER=null
[ "$STATE" = "firing" ] && FIRED_AFTER=$(( T_FIRED - T0 ))

cat > "$OUT" <<EOF
{
  "experiment": "drive one alert rule from inactive to firing",
  "alert": "$ALERT",
  "reachedFiring": $( [ "$STATE" = "firing" ] && echo true || echo false ),
  "secondsToPending": $PENDING_AFTER,
  "secondsToFiring": $FIRED_AFTER,
  "requestsSent": $REQ,
  "requestsEnding5xx": $ERR,
  "alertPayload": $BODY,
  "whyPendingIsNotEnough": "pending says the expression matched once. A rule whose condition flickers sits in pending indefinitely and never pages. Only firing proves the for: duration was satisfied.",
  "notCoveredHere": "GatewayAdmissionGuardBypassed and GatewayBackendScrapeFailing read series that only exist under --admission-mode=kv-aware. This gateway runs the default (off), so those series are absent and no load can create them."
}
EOF
echo
say "record written to $OUT"
[ "$STATE" = "firing" ]
