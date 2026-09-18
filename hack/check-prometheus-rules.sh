#!/usr/bin/env bash
# Validates the PrometheusRule's PromQL with promtool.
#
# It exists because the rules were committed once with the expressions only structurally checked — parentheses
# balanced, YAML well-formed — and that is not the same as valid PromQL. An alert rule that silently never
# fires is worse than no alert at all, because the absence of pages reads as the absence of trouble.
#
# promtool takes a plain rules file, not a PrometheusRule custom resource, so the spec is lifted out first.
set -euo pipefail
cd "$(dirname "$0")/.."

PROMTOOL="${PROMTOOL:-promtool}"
command -v "$PROMTOOL" >/dev/null || {
  echo "promtool not found. Install it from a prometheus release, or set PROMTOOL=/path/to/promtool" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for f in config/prometheus/*rules*.yaml; do
  [ -e "$f" ] || continue
  python3 -c "
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
if doc.get('kind') != 'PrometheusRule':
    sys.exit(0)
yaml.safe_dump({'groups': doc['spec']['groups']}, open(sys.argv[2], 'w'), sort_keys=False, allow_unicode=True)
" "$f" "$tmp/rules.yaml"
  [ -s "$tmp/rules.yaml" ] || continue
  echo "== $f"
  "$PROMTOOL" check rules "$tmp/rules.yaml"

  # promtool validates the EXPRESSION, never the data. It reported SUCCESS on four rules of which two
  # referenced metrics that had no series in Prometheus at all, and a rule whose series does not exist
  # evaluates to an empty vector forever: permanently inactive, permanently green, indistinguishable from
  # a system that is simply healthy.
  #
  # This cannot be a hard failure. A series that is legitimately absent because the feature producing it is
  # switched off is the normal case here, not a defect. So the check reports which names Prometheus has
  # never seen and leaves the judgement to a reader, who knows what is meant to be running.
  if [ -n "${PROM:-}" ]; then
    echo "-- metric names this rule file references, against $PROM"
    grep -ohE 'gpuaas_[a-z_]+' "$tmp/rules.yaml" | sort -u | while read -r m; do
      n=$(curl -sG "http://$PROM/api/v1/query" --data-urlencode "query=count(${m})" 2>/dev/null \
            | python3 -c "import json,sys
try: r=json.load(sys.stdin)['data']['result']
except Exception: r=[]
print(r[0]['value'][1] if r else '0')" 2>/dev/null)
      if [ "${n:-0}" = "0" ]; then
        echo "   ABSENT  $m  (any rule over this name can never fire)"
      else
        echo "   present $m  ($n series)"
      fi
    done
  else
    echo "-- set PROM=localhost:9090 to also check that each referenced metric actually has series"
  fi
done
