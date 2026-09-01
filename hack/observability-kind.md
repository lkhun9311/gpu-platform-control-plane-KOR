# Observability on kind: Prometheus scraping this operator, and a dashboard that is checked

The metrics, the ServiceMonitor and the alert rules were all written before any Prometheus existed to consume
them. This document is the run that finally pointed one at them, and it found that two of the three did not
work.

## What was already there, and what was actually true about it

| artifact | claimed | found |
|---|---|---|
| 5 controller metrics + 7 gateway metrics | exported | true |
| `config/prometheus/gateway_rules.yaml` | 4 alerts | true — `promtool check rules` passes |
| `config/prometheus/monitor.yaml` ServiceMonitor | scrapes the operator | **the target came up DOWN** |
| operator metrics endpoint | serving on :8443 | **nothing was listening** |

## The endpoint was not serving, and the cause was in the webhook overlay

`config/webhook-enabled/manager_webhook_patch.yaml` set the manager's `--webhook-cert-path` with a
strategic-merge patch. `containers[].args` is a list of plain strings with no merge key, so a strategic merge
**replaces** it. The patch deleted `--metrics-bind-address=:8443` and `--health-probe-bind-address=:8081`
that the base and `config/default` had set.

Everything about that looked healthy. `kustomize build` succeeded, `kubectl apply` succeeded, the Deployment
rolled out, the webhook worked, and the live webhook verification passed 6/6 — because none of those touch
the metrics port. The only symptom was a Prometheus target that could not connect, and there was no
Prometheus until now.

The fix is a JSON6902 `add`, which is the form `config/default/manager_metrics_patch.yaml` already used for
exactly this reason. `hack/check-webhook-overlay.sh` now asserts the rendered Deployment still carries both
flags; reverting to the strategic merge makes it name both losses.

## The dashboard is validated two ways, and the second one is the one that matters

`hack/check-dashboard.sh`:

1. every panel expression parses as PromQL — each `expr` is wrapped in a throwaway recording rule and run
   through `promtool check rules`
2. every `gpuplatform_`/`gpuaas_` metric the dashboard names is defined in the Go source

The second check exists because **the first cannot catch a rename**. `gpuplatform_nodehealth_taints_total`
is perfectly valid PromQL for a metric that has never existed. promtool passes it, Grafana renders an empty
graph, and an empty graph reads as *no traffic* — the most reassuring thing a dashboard can say. Injecting
that exact typo makes check 1 pass and check 2 fail.

## Two panel defects the screenshot exposed

**A count that cannot be true.** The first render showed `Reconcile errors (5m): 2.26`. `increase()`
extrapolates across scrape boundaries, so counting discrete events with it produces fractions. The true count
was 2. A displayed value that is impossible undermines every other number on the page, so the stat now shows
the cumulative counter (exact, integral) and the rate lives in its own panel — the stat says how many, the
rate says whether it is still happening, and neither can answer the other's question.

**`No data` where the answer is zero.** "Failed jobs by reason" rendered `No data` in large green text. No
failures and no data are different facts, and only one of them is good news. Every stat panel now sets
`noValue: "0"`.

## Evidence

kind `platform`, kube-prometheus-stack, operator deployed from `config/webhook-enabled`.

```
target  gpu-platform-control-plane-controller-manager-metrics-service   health=up

sum by (action) (gpuplatform_nodehealth_taint_total)      applied=1  removed=1
sum by (phase)  (gpuplatform_mltrainingjob_phase_total)   Pending=6
sum by (controller) (controller_runtime_reconcile_total)  mltrainingjob=32  nodehealth=13
sum(controller_runtime_reconcile_errors_total)            2
```

The taint pair comes from `hack/chaos-fr004-degraded-node.sh`, so the chaos experiment and the dashboard
corroborate each other: the run reported one quarantine and one recovery, and the counter agrees.

The two reconcile errors are optimistic-concurrency conflicts on a status update — the object changed between
read and write, controller-runtime requeued. Expected, and worth leaving visible rather than filtering, since
the panel's job is to show that errors are non-zero and let the reader decide.

## Reproducing

```bash
helm upgrade --install kps prometheus-community/kube-prometheus-stack \
  -n monitoring --create-namespace \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false

kubectl apply -f config/prometheus/monitor.yaml -n gpu-platform-control-plane-system
kubectl -n monitoring create configmap gpu-platform-operator-dashboard \
  --from-file=operator_dashboard.json=config/prometheus/operator_dashboard.json \
  --dry-run=client -o yaml | kubectl label -f - --local -o yaml grafana_dashboard=1 | kubectl apply -f -
```

`monitor.yaml` carries `namespace: system`, which kustomize rewrites and a direct `kubectl apply` does not —
apply it with `-n gpu-platform-control-plane-system` or through the overlay.

## Not covered

The gateway. Its 7 metrics, its PodMonitor and all 4 alert rules are validated statically and have never been
scraped, because the gateway is not deployed on this cluster. The two components also disagree on prefix —
the operator uses `gpuplatform_` and the gateway `gpuaas_gateway_` — which is worth reconciling before either
name appears in a dashboard someone else maintains.
