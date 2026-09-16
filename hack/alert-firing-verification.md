# Alert rules: what promtool proves and what it does not

`hack/check-prometheus-rules.sh` returned `SUCCESS: 4 rules found`, and that was recorded as the alerting
evidence. It is not. promtool checks the expression — valid PromQL, resolvable functions, well-formed
durations. It never looks at the data, so it returns SUCCESS just as readily for a rule over a metric that
nothing in the cluster emits. Such a rule evaluates to an empty vector on every evaluation: permanently
inactive, permanently green, and indistinguishable on a dashboard from a system that is simply healthy.

Two questions followed. Does any rule here actually fire? And do the metrics they read exist?

## What was run

`hack/alert-firing-verification.sh` against the kind cluster, gateway and Prometheus reached through
port-forwards.

The rule driven was `GatewayUpstreamErrorsReachingClients`:

```
sum by (model) (rate(gpuaas_gateway_requests_total{code=~"5.."}[5m])) > 0.1
for: 5m
```

The backend was removed and load held at roughly 2.5 requests per second until the rule changed state.

Scaling the `stub-llm` Deployment to zero does not work: the `InferenceDeployment` controller owns that
Deployment and restored the replica within one reconcile — which is the recovery behaviour FR-002 already
measures, arriving here as an obstacle. The `InferenceDeployment` CR is the level that holds.

## Result

| | |
|---|---|
| state before | `inactive`, gateway answering 200 |
| condition established | every request ends 502 at the client |
| `pending` at | 06:31:51 UTC |
| `firing` at | between 06:36:35 and 06:36:55 UTC |
| elapsed | 5m 04s ± 20s against a declared `for: 5m` |
| rate at fire time | 1.000 req/s ending 502, threshold 0.1 |
| alert value recorded | `9.999999999999999e-01` |

The ±20 s is this harness's own resolution: the alert state is polled every 25 requests, roughly every 20
seconds. The measured interval is consistent with `for: 5m` and cannot be stated more precisely than the
instrument allows.

Evidence: `ex/alert-firing.json`.

Watching `pending` appear would not have been enough. `pending` says only that the expression matched once;
a rule whose condition flickers can sit in `pending` indefinitely and never page. Firing is the state that
proves the `for:` duration was satisfied continuously.

## The finding: two of four rules could never fire

Querying Prometheus for every metric name the rule file references:

```
present gpuaas_gateway_requests_total                (5 series)
present gpuaas_gateway_backend_fallbacks_total       (1 series)
ABSENT  gpuaas_gateway_backend_telemetry_fresh
ABSENT  gpuaas_gateway_backend_scrape_errors_total
```

Both absent names are registered in `internal/gateway/metrics.go`, so they exist as far as the code is
concerned. Both are `Vec`s written only by the KV-aware admission guard, and a Prometheus `Vec` with no
observed child emits nothing at all — not a zero, no series. The gateway runs `--admission-mode=off`, the
default, and the deployment passes no arguments. So `gpuaas_gateway_backend_telemetry_fresh == 0` matches
nothing and `GatewayAdmissionGuardBypassed` is inert.

For this configuration that is arguably correct: there is no guard, so there is nothing to report on it
being bypassed. The defect is narrower and worse.

`GatewayAdmissionGuardBypassed` exists to catch the guard silently failing open. It is written over a series
the guard itself publishes. So the one failure it cannot catch is the guard not running — a scrape loop
that never started, a backend list that came back empty, a panic in the goroutine. In all of those the
series vanishes, the rule goes quiet, and the quiet reads as health. The alert is blind to its own subject's
absence.

## Fix

A configuration series that does not depend on the guard working:

```go
admissionModeActive = promauto.With(metrics.Registry).NewGaugeVec(
    prometheus.GaugeOpts{Name: metricPrefix + "admission_mode_active", ...},
    []string{"mode"},
)
```

Set once in `SetAdmitter`, which is the only place that knows which `Admitter` was actually installed, so the
series cannot claim a mode the gateway is not running. It is present from process start, before any request
or scrape.

That makes the missing case expressible:

```
(gpuaas_gateway_admission_mode_active{mode="kv-aware"} == 1) unless on() gpuaas_gateway_backend_telemetry_fresh
for: 5m
```

`unless on()` drops the left side the moment any `telemetry_fresh` series exists, so the rule speaks only
about total silence — not staleness, which the original rule still covers.

The precondition is now written into both original rules as a comment, because a reader who finds them
permanently inactive deserves to know whether that is the configuration or a fault.

## Fix to the checker

`check-prometheus-rules.sh` now takes an optional `PROM=host:port` and reports, per metric name the rule
file references, whether Prometheus has any series for it:

```
$ PROM=localhost:9090 ./hack/check-prometheus-rules.sh
  SUCCESS: 5 rules found
  ABSENT  gpuaas_gateway_backend_telemetry_fresh  (any rule over this name can never fire)
```

It reports rather than fails. A series legitimately absent because the feature producing it is switched off
is the normal case here, and turning that into a red build would teach a reader to ignore the check. The
judgement of whether an absent metric is a defect needs to know what is meant to be running, which the
script does not.

## Claim correction

The earlier record said "4 alert rules validated by promtool" and let that stand as alerting evidence. The
accurate statement:

- 5 rules parse (promtool).
- 1 rule driven from `inactive` to `firing` against live traffic, timing consistent with its `for:` clause.
- 2 rules cannot fire in this configuration; the reason is now in the rules file, and a third rule was added
  to cover the case their absence was hiding.
- 1 rule (`GatewayModelDegraded`) parses and reads a metric that has series, but has not been driven to
  firing. Its input is fallback traffic, which needs two backends configured for one model.
