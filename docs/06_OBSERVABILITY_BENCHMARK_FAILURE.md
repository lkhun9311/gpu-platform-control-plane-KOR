# Observability, Benchmark, and Failure

> **Status (2026-08-17).** Parts of this document are design-of-record and parts are now running; the two
> are marked separately below because an earlier version of this header claimed less than was true and a
> reader has no way to tell an unbuilt plan from an unmeasured one.
>
> **Deployed and scraped on kind:** the operator's five metrics and the gateway's twelve, through the
> ServiceMonitor and PodMonitor in `config/prometheus/`, into kube-prometheus-stack. A Grafana dashboard
> (`config/prometheus/operator_dashboard.json`) renders them, and `hack/check-dashboard.sh` asserts every
> panel's PromQL parses AND that every metric it names exists in the Go source — an unexported metric renders
> an empty graph, and an empty graph reads as "no traffic". The four alert rules pass `promtool check rules`
> via `hack/check-prometheus-rules.sh`.
>
> **Run against a real cluster:** FR-002 and FR-004, with their evidence and their instrument defects written
> up in `hack/chaos-fr002-serving-pod-killed.md`, `hack/chaos-fr002b-backend-fallback.md` and
> `hack/chaos-fr004-degraded-node.md`. The gateway IS deployed (`config/gateway-kind`), serving a stub
> backend through an `InferenceDeployment`; the admission webhook is deployed and verified end to end
> (`hack/verify-webhook-live.sh`).
>
> **Built, and run on a paid GPU:** the M5-b admission-guard benchmark harness. Four repetitions on
> 2026-09-03 and an engine-level scheduler microtest on 2026-09-04. The guard missed its pre-registered
> 1.25x premium-tail target at 83.7x, and the harness declared the run invalid rather than reporting a
> protection claim. The thresholds were not merely unvalidated — the run showed the gateway cannot observe
> the pressure they gate on.
>
> **Designed only — no code:** Xid and ECC (nothing in the Go tree). The DCGM exporter layer is NOT in this
> category and the claim that it was is corrected here: `config/dcgm-exporter/` deploys it and
> `internal/queuelab/dcgm.go` reads `DCGM_FI_DEV_GPU_UTIL` from it. It has now been pointed at real cards:
> session `qlgpu-20260906-032038` ran eight runs on four A10Gs and every one carries
> `deviceEvidence: device-work-observed`. Also designed only:
> eBPF layer, Nsight profiling, the SQLite/Postgres operations ledger, FR-001, FR-003, FR-005, and the
> `evidence/` report tree shown below.
>
> **Which GPUs were real, and which were not.** Every kind cluster here advertises simulated
> `nvidia.com/gpu` capacity through a fake device plugin, and the chaos runs measure control-plane reaction
> rather than device behaviour. The paid EC2 sessions used real cards, and one of them attributed device
> utilisation to Pods: `queuelabrun -compare` prints its comparison for `qlgpu-20260906-032038` with no
> `device: NOT OBSERVED` line. That is one card model on one driver on one AMI, and it does not
> retroactively make the kind runs device-observed -- those measured what they measured.

This is the evidence center of the project — the proof that the platform actually operates workloads, not just defines types.

## Observability layers

| Layer                | Tool            | Sees                                                                                                          |
|----------------------|-----------------|---------------------------------------------------------------------------------------------------------------|
| GPU                  | DCGM exporter   | util, memory, XID, ECC, temp — util is a periodic sample, see the blind spot below                            |
| Serving              | vLLM metrics    | TTFT, TPOT, queue depth, KV cache                                                                             |
| Gateway              | Prometheus      | latency, 429, request count                                                                                   |
| Kubernetes           | events / logs   | pod kill, pending, OOM                                                                                        |
| System (exploratory) | eBPF            | runqueue, syscall/ioctl, IO wait — secondary signal; GPU contention mostly does not surface here (doc 04, S6) |
| Profiling            | Nsight Systems  | CUDA timeline (optional)                                                                                      |
| Ledger               | Postgres/SQLite | workload_runs, benchmark_runs                                                                                 |

**What DCGM_FI_DEV_GPU_UTIL cannot see.** It is NVML's percentage of the last sample period during which a kernel was executing, and the exporter here collects once a second. A tenant whose work arrives in bursts with a period near the collection interval does not drift through the sampler's phase, so a sample that lands in an idle stretch lands there every time. Measured, not reasoned: in session `qlgpu-20260906-103327` a Pod executing 22,093 real kernels at a quarter duty on a one-second period was reported as working in **zero of 52 collections**, while the same exporter in the same run read 98 for a continuously computing neighbour and named every Pod on every card. Attribution was not failing; attribution succeeded and reported idle. Changing the workload's period to 2.6 s -- nothing else -- turned that into 15 of 51 collections above zero at a mean of 22.8 against a declared 25 (`qlgpu-20260906-125451`). Counts here are collections, not scrapes: the exporter collects once a second and serves a cached snapshot in between, while the run scrapes twice a second, so each collection is read about twice and the raw sample counts are double these.

**The gate did not save the first run; its phase did.** At a one-second period the sampler visits one phase of the workload's cycle and stays there. That phase fell in an idle stretch, so the run was refused. Had it fallen inside the burst, every collection would have read 98 and a quarter-duty card would have passed as fully busy. Do not read the refusal as a property of the instrument. A dashboard has no gate at all, and a utilisation panel is a periodic sampler with the same failure mode -- but the gate's protection here is narrower than it looks, and `docs/superpowers/specs/2026-09-06-does-reservation-track-use.md` carries the withdrawal in full. Counting collections rather than scrapes also closed a hole in the gate itself: it required two busy samples at distinct timestamps, and one cached collection read twice supplied both. It now requires them `maxObserverGap` apart, which no single snapshot can span.

The eBPF and Nsight layers require a real GPU node and are **not implemented at all today**, and neither is Xid or ECC. The DCGM layer is different and this sentence has been wrong twice about it. It once said no code existed; the reader, the Pod-attribution resolver, the exporter deployment and the pre-spend gate all do. It then said no card had been rented; one has. In session `qlgpu-20260906-032038` the exporter named Pods on four A10Gs, the two cards the protocol used were busy in 541 and 559 of 663 samples, and the two held by the surplus occupier in 0 of 663 -- so a GPU-second in that session is an observed device-second. Everything earlier remains a second of reservation, and is labelled as one. Unmeasured layers are labeled, not faked.

## Failure reports (5)

| ID     | Scenario           | Expected evidence                                            |
|--------|--------------------|--------------------------------------------------------------|
| FR-001 | quota exceeded     | HTTP 429, event, `workload_runs.failure_reason=quota_exceed` |
| FR-002 | serving pod killed | gateway error spike, recovery time                           |
| FR-003 | GPU OOM            | pod failure, NodeHealth / workload failure                   |
| FR-004 | degraded node      | scheduling reject / avoidance (taint)                        |
| FR-005 | noisy neighbor     | p99 increase + metric timeline (doc 04)                      |

## Local vs real-GPU feasibility

| Scenario                    | Local (kind, simulated GPU)           | Needs real GPU            |
|-----------------------------|---------------------------------------|---------------------------|
| quota exceeded (FR-001)     | not yet run; HTTP 429 is exercised in gateway unit tests, and the gateway is now deployed on kind so the scenario is reachable | no                        |
| serving pod killed (FR-002) | **RUN** — see `hack/chaos-fr002-serving-pod-killed.md`; the fallback path has its own run in `hack/chaos-fr002b-backend-fallback.md` | real vLLM recovery timing |
| GPU OOM (FR-003)            | hard                                  | yes                       |
| degraded node (FR-004)      | **RUN** — see `hack/chaos-fr004-degraded-node.md`; kubelet is stopped for real, and the operator's own reaction is reported as a bound because it is faster than the harness can resolve | DCGM-based signal         |
| noisy neighbor (FR-005)     | harness only                          | yes (measured p99)        |

Numbers from real-GPU scenarios are committed only after a real run; locally we ship the harness and say so.

## M5 flagship evidence matrix (KV cache + admission guard)

| Scenario               | Local / kind                           | Real GPU required       | Evidence                                                                   |
|------------------------|----------------------------------------|-------------------------|----------------------------------------------------------------------------|
| Quota exceeded         | yes                                    | no                      | HTTP 429 log, ResourceQuota event                                          |
| Pod kill recovery      | partial                                | for real vLLM timing    | restart timeline, Gateway error metric                                     |
| GPU OOM                | no                                     | yes                     | pod event, DCGM memory, failure report                                     |
| Degraded node          | simulated                              | for DCGM-based signal   | NodeHealth phase transition                                                |
| Noisy neighbor p99     | harness only                           | yes                     | R1 baseline vs R2/R3 colocated p99 chart (+ CI)                            |
| KV cache pressure      | metric schema only                     | yes                     | vLLM KV cache usage + p99 time-align                                       |
| Admission guard off/on | envtest with stub metrics (guard spec) | for real latency impact | R3 vs R4 p99 comparison + standard-tenant cost (429 rate, throughput loss) |

## Report tree

This is the **target** layout — none of it exists yet. The `evidence/` directory in this repository today
contains only empty placeholder subdirectories (`command-logs/`, `incident-reports/`, `manifests/`,
`screenshots/`, `benchmark-reports/`), each holding a `.gitkeep` and nothing else.

```
evidence/
  benchmark-reports/
    noisy-neighbor/
      baseline.csv  colocated.csv  timeslicing.csv  mps.csv
      result-analysis.md  grafana-p99-spike.png  ebpf-timeline.png
  failures/
    fr-001-quota-exceed.md ... fr-005-noisy-neighbor.md
```

## To fill

- Prometheus/Grafana manifests + dashboard JSON
- failure-injection scripts (`scripts/demo-06-failure-injection.sh`)
- which FRs were actually run locally vs on real GPU (honest table)
