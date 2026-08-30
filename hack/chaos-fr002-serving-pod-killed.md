# FR-002 kind chaos: the serving pod dies and the gateway has to survive it

Runs the FR-002 scenario from [`docs/06_OBSERVABILITY_BENCHMARK_FAILURE.md`](../docs/06_OBSERVABILITY_BENCHMARK_FAILURE.md).
Scripted in [`chaos-fr002-serving-pod-killed.sh`](chaos-fr002-serving-pod-killed.sh). No GPU.

Unlike FR-004, this one needs the gateway actually deployed and actually scraped, which is why it comes
after [`observability-kind.md`](observability-kind.md).

## Two observers, and the second one is the point

The client's view comes from this script's own requests. The gateway's view comes from its Prometheus
counters. The run compares them, and **the comparison is the result** — an on-call engineer sees the
counters, not the client, so a gateway whose metrics stay clean through an outage its users felt is a worse
problem than the outage. The harness fails the run if the client saw errors the counters do not show.

## What cannot be separated here, unlike FR-004

FR-004 splits Kubernetes' detection from the operator's reaction, because they are different mechanisms
observable at different moments. Here they are not separable: the gateway proxies to a Service, so it learns
about the replacement pod exactly when kube-proxy does. Endpoint propagation is inside the reported error
window and this platform does not control it. The record says so in an `attribution` field rather than
letting the number be read as the gateway's own recovery time.

## Observed evidence

kind `platform`, gateway from `config/gateway-kind`, backend an `InferenceDeployment` running the
benchharness stub. Six runs: three that exposed the comparison defect below, three after it was fixed.

```json
{
  "experiment": "FR-002 serving pod killed",
  "steadyStateEstablished": true,
  "samplingIntervalMs": 419.4,
  "disrupted": true,
  "latenciesMs": { "killToFirstError": 49.4, "killToRecovery": 1294.6, "errorWindow": 1245.1 },
  "clientView":  { "sampled": 3, "failed": 2, "neverAnswered": 1, "answeredByGateway": 1 },
  "gatewayView": { "requests5xxDelta": 1, "upstreamErrorsDelta": 1,
                   "requests5xxCumulative": 34, "agreesWithClient": "true" }
}
```

Across the three corrected runs the client's view and the gateway's delta agree exactly every time, and the
timings barely move:

| run | first error | recovery | client failed | never answered | gateway 5xx delta |
|---|---|---|---|---|---|
| 1 | 49.5ms | 1295.3ms | 2 | 1 | 1 |
| 2 | 48.6ms | 1295.5ms | 2 | 1 | 1 |
| 3 | 49.4ms | 1294.6ms | 2 | 1 | 1 |

## The agreement was not being tested at all

The first write-up of this experiment reported **"9 and 9 — every failure the client experienced appears in
the gateway's own counters"** and called that the load-bearing result. It was a coincidence.

`gpuaas_gateway_requests_total` is **cumulative**. The harness recorded only the value after the run and
compared that absolute against one run's client failures. They happened to match once. Repeating the
experiment three times is what broke it: the counter climbed 15 → 23 → 31 while every run saw nine failures.
The comparison had never been a comparison.

Two corrections followed:

- the harness records the counter **before and after** and compares the **delta**
- a client failure with code `000` is **excluded** from what the gateway must account for. `000` is curl
  never receiving an HTTP response — its own timeout, or a refused connection. Demanding that the gateway
  count a request that never reached it is an impossible standard, and it was the whole of the residual
  discrepancy: nine client failures against a delta of eight, with one `000` among them.

The agreement now holds across three runs with the deltas matched exactly, and it means something it did
not mean before.

**This is why the n=1 note in the validity table mattered.** It was written down as a weakness and the
weakness was real: closing it invalidated the conclusion the single run had produced.

## The instrument claimed a resolution it did not have

The first run reported `samplingIntervalMs: 100` — the nominal value, written as a constant — while
collecting **3 samples across 5.3 seconds**.

A failing request blocks for its curl timeout, which was `-m 5`. So the sampling interval widens precisely
when things break, which is exactly when it needs to be tight: the loop samples at 100ms while healthy and
at seconds while broken. Every latency in that record carried seconds of unstated uncertainty.

Fixed two ways. The timeout is now `-m 1`, and the interval is **measured and recorded** rather than
asserted — the corrected run reports 304.2ms over 10 samples, and `killToRecovery` is stated as
`3079.8ms +/- one sample`.

This is the same defect FR-004 had, from the opposite direction. There the resolution was recorded and the
record exposed a half-finished fix; here it was hard-coded, so a record showing 3 samples in 5.3 seconds
still said `100` beside it. **Measure the instrument every run and put the number in the artifact** —
a constant cannot contradict the data it sits next to.

## Two setup faults worth keeping

**The API key Secret is keyed by API key, not by tenant.** `resolveTenant` looks up `sec.Data[key]` and
returns the value as the tenant, so `--from-literal=premium-key=premium-1` means the key is `premium-key`
and the tenant is `premium-1`. Getting it backwards produces a clean `401` that looks like a wrong password.

**`InferenceDeployment.spec.port` does not reach the workload.** It sets the containerPort, the Service port
and the probe target, but nothing passes it to the container, so the stub kept listening on its own default
`:8090` while everything around it was configured for `8000`. This fails loudly — readiness never passes —
which is the acceptable outcome; it is recorded because the field reads like it configures the workload and
does not.

## Running it

```bash
kubectl port-forward -n gpu-platform-control-plane-system svc/gateway 8080:8080 &
kubectl port-forward -n monitoring svc/kps-kube-prometheus-stack-prometheus 9090:9090 &
OUT=./ex/chaos-fr002.json ./hack/chaos-fr002-serving-pod-killed.sh
```

The steady state aborts unless five pre-fault requests all return 200 and the gateway's counter is visible
in Prometheus. Both matter: without the first, a later non-2xx is not attributable to the kill; without the
second, the run could not compare the two observers at all.
