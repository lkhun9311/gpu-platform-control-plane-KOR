#!/usr/bin/env bash
# Validates the Grafana dashboard's queries two ways, because a broken dashboard does not look broken.
#
# A panel whose PromQL is invalid, or whose metric this platform never exports, renders as an empty graph.
# Empty reads as "no traffic" — the most reassuring thing a dashboard can say — so the failure mode of a
# wrong dashboard is a calm one. Neither check below is optional for that reason.
#
#   1. every expression parses as PromQL      (promtool, by wrapping each expr in a throwaway rules file)
#   2. every gpuplatform_/gpuaas_ metric it names exists in the Go source
#
# The second is the one that catches renames. promtool is happy to parse a query for a metric that has not
# existed since the constant was edited.
set -euo pipefail
cd "$(dirname "$0")/.."

PROMTOOL="${PROMTOOL:-./bin/promtool}"
command -v "$PROMTOOL" >/dev/null 2>&1 || [ -x "$PROMTOOL" ] || {
  echo "promtool not found. Set PROMTOOL=/path/to/promtool" >&2
  exit 1
}

DASH="${DASH:-config/prometheus/operator_dashboard.json}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# ---- 1. PromQL parses -------------------------------------------------------
python3 - "$DASH" "$tmp/rules.yaml" <<'PY'
import json, sys

dash = json.load(open(sys.argv[1]))
rules = []
for panel in dash.get("panels", []):
    for i, target in enumerate(panel.get("targets", [])):
        expr = target.get("expr")
        if not expr:
            continue
        # A recording rule is the smallest container promtool will type-check an expression inside.
        rules.append({"record": "dash:panel%d:%d" % (len(rules), i), "expr": expr})

if not rules:
    sys.exit("the dashboard declares no queries at all")

with open(sys.argv[2], "w") as fh:
    json.dump({"groups": [{"name": "dashboard", "rules": rules}]}, fh)
print("  %d expressions extracted" % len(rules))
PY

"$PROMTOOL" check rules "$tmp/rules.yaml" >/dev/null || {
  echo "FAIL: at least one panel expression is not valid PromQL" >&2
  "$PROMTOOL" check rules "$tmp/rules.yaml" >&2
  exit 1
}
echo "  every panel expression parses as PromQL"

# ---- 2. every metric it names is one this platform exports ------------------
# The Go source builds names as a prefix constant plus a suffix, so the literal never appears whole. The
# prefixes are resolved here and the suffixes matched against the string literals in the metric definitions.
python3 - "$DASH" <<'PY'
import json, re, subprocess, sys

dash = json.load(open(sys.argv[1]))

exprs = [t["expr"] for p in dash.get("panels", []) for t in p.get("targets", []) if t.get("expr")]
referenced = set()
for e in exprs:
    referenced |= set(re.findall(r"\b(gpuplatform_[a-z_]+|gpuaas_[a-z_]+)\b", e))

def defined(path, prefix):
    src = open(path).read()
    return {prefix + m for m in re.findall(r'Name:\s*metricPrefix\s*\+\s*"([a-z_]+)"', src)} | \
           {prefix + m for m in re.findall(r'Name:\s*"([a-z_]+)"', src)}

exported = defined("internal/controller/metrics.go", "gpuplatform_") \
         | defined("internal/gateway/metrics.go", "gpuaas_gateway_")

# Prometheus appends _bucket, _sum and _count to a histogram's series; the Go source only names the base.
# A first version of this check flagged gpuaas_gateway_request_duration_seconds_bucket as missing while that
# series was being queried successfully on a live cluster. A check that cries wolf on correct input gets
# muted, and a muted check is not a check.
#
# The suffix is allowed ONLY for metrics that really are histograms. Stripping it unconditionally was the
# first fix, and it let gpuaas_gateway_requests_total_bucket through — a counter with a histogram's suffix,
# a series that does not exist. Loosening a check to stop a false positive is how a check stops catching the
# true ones, so the relaxation is bounded by the metric's actual type.
def histograms(path, prefix):
    src = open(path).read()
    names = set()
    for block in re.findall(r"(?:Histogram|Summary)(?:Vec)?Opts\{(.*?)\n\t*\}", src, re.S):
        for m in re.findall(r'Name:\s*metricPrefix\s*\+\s*"([a-z_]+)"', block):
            names.add(prefix + m)
        for m in re.findall(r'Name:\s*"([a-z_]+)"', block):
            names.add(prefix + m)
    return names

histogram_metrics = histograms("internal/controller/metrics.go", "gpuplatform_") \
                  | histograms("internal/gateway/metrics.go", "gpuaas_gateway_")

HISTOGRAM_SUFFIXES = ("_bucket", "_sum", "_count")

def resolves(name):
    if name in exported:
        return True
    for suffix in HISTOGRAM_SUFFIXES:
        base = name[: -len(suffix)]
        if name.endswith(suffix) and base in histogram_metrics:
            return True
    return False

missing = sorted(m for m in referenced if not resolves(m))
if missing:
    print("FAIL: the dashboard queries metrics this platform does not export:", file=sys.stderr)
    for m in missing:
        print("  %s" % m, file=sys.stderr)
    print("  (these panels would render empty, which reads as 'no traffic')", file=sys.stderr)
    sys.exit(1)

print("  all %d platform metrics referenced are exported by the Go source" % len(referenced))
for m in sorted(referenced):
    print("    %s" % m)
PY

echo "dashboard checks passed"
