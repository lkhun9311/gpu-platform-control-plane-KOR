# GPU Governance, Performance Isolation Benchmarking, and Noisy-Neighbor Analysis

This is the killer feature. Everything else (CRDs, gateway, node lifecycle) exists so that this question can be asked and answered honestly.

> **Status (2026-08-07).** Mixed, and the difference matters. **Built and unit-tested:** the KV-cache-aware
> admission guard (`internal/gateway/kvguard.go`, wired into the gateway) and the open-loop benchmark
> harness. Neither has ever run on a GPU, and the guard's vLLM metrics fixture is synthetic — it says so in
> the fixture — so its engage/release thresholds are unvalidated guesses. **Designed only, no code:** the
> `GpuSharingBenchmark` CRD and the sharing-mode matrix. **Not measured:** everything numeric below. Every
> figure in this document is a target or an example and is labeled as such; no real-GPU number exists.

## The core question

When multiple tenants share the same GPU pool, can the platform:

1. enforce GPU quota,
2. detect runtime contention,
3. reproduce noisy-neighbor effects,
4. explain p99 latency spikes with metrics,
5. protect a premium tenant with a runtime admission guard — and report what that protection costs?

## Why this is the differentiator

A "GPUaaS" that only schedules pods is common. What is rare in a portfolio is **measuring** what happens when tenants actually contend for one GPU — and explaining the p99 spike with aligned system metrics rather than asserting a winner. Noisy neighbor, sharing-mode comparison, and p99 root-cause turn "just GPUaaS" into "GPUaaS that understands isolation."

Crucially, this is framed to stay credible even when the result is boring: a **null result** (no measurable difference) is reported with its conditions and likely cause, not hidden. That honesty is the point.

## Two distinct contention topologies (do not conflate)

| Topology                                                                     | What contends                                       | Where it appears        |
|------------------------------------------------------------------------------|-----------------------------------------------------|-------------------------|
| **Shared instance** — both tenants hit ONE vLLM instance through the gateway | the shared KV-cache pool and the vLLM waiting queue | **M5 flagship (R0–R4)** |
| **Shared GPU** — separate vLLM pods on one GPU via time-slicing / MPS        | GPU time / SM occupancy; KV caches are separate     | S2 / S3                 |

"KV cache pressure" is only a valid mechanism in the shared-instance topology. The flagship uses a single shared vLLM instance with the gateway multiplexing tenants in front of it — the realistic GPUaaS serving shape. S2/S3 answer a different question (sharing-mode trade-offs) and are reported separately.

## GpuSharingBenchmark CRD

Declares one A/B contention experiment and records its result. Formal design: `docs/superpowers/specs/2026-07-04-gpusharingbenchmark-crd-design.md` (canonical schema — field names here follow it: `reportUri`, decimal-string ratios, `load` spec).

```yaml
apiVersion: platform.lkhun9311.github.io/v1
kind: GpuSharingBenchmark
metadata:
  name: premium-vs-longcontext-shared
  namespace: platform-system
spec:
  gpuClass: a10g
  sharingMode: sharedInstance       # exclusive | timeSlicing | mps | sharedInstance
  baseline:                         # latency-sensitive victim
    tenant: tenant-premium
    model: llama3-8b
    qps: "2.0"
    inputTokens: 256
    outputTokens: 128
  contender:                        # long-context noisy neighbor
    tenant: tenant-standard
    model: llama3-8b
    qps: "6.0"
    inputTokens: 8192
    outputTokens: 256
  repetitions: 5                    # >=5; p99 from 3 reps is noise
  warmupRequests: 50
  minRequestsPerRun: 1000
  load:
    mode: openLoop                  # Poisson arrivals; closed-loop hides queueing (coordinated omission)
    generator: "genai-perf"         # pinned tool + version in the report
    streaming: true
    timeoutMs: 60000
    retries: 0                      # retries silently repair tail latency
# status.result (baselineP99Ms, colocatedP99Ms, interferenceRatio, p99CI95, reportUri) is
# written ONLY by a real-GPU run — no placeholder example here, by policy (see below).
```

Implementation order: CRD + sample + benchmark scripts + report skeleton first; a thin status writer to record results; a deeper controller is optional and not required to publish.

## Experiment matrix

| Scenario                             | Tenant A                   | Tenant B | Topology                  | Measures                            |
|--------------------------------------|----------------------------|----------|---------------------------|-------------------------------------|
| S1 Baseline                          | low QPS, latency-sensitive | off      | exclusive                 | p50/p95/p99 saturation curve        |
| S2 Noisy neighbor                    | low QPS                    | high QPS | shared GPU (time-slicing) | p99 spike                           |
| S3 MPS                               | low QPS                    | high QPS | shared GPU (MPS)          | throughput/latency trade-off        |
| S4 Quota reject                      | over-limit request         | —        | any                       | 429 + event + ledger row            |
| S5 Recovery                          | B off                      | off      | any                       | A latency recovery time             |
| S6 eBPF root-cause (**exploratory**) | A+B                        | A+B      | selected                  | runqueue / syscall / IO correlation |

S6 is explicitly an **exploratory layer**, not primary evidence: GPU contention mostly does not surface in CPU runqueue latency. The primary root-cause signals are the vLLM queue and KV-cache metrics; eBPF is included to check for (and honestly report) host-side effects or their absence.

## Metrics

Serving (vLLM): TTFT p95, TPOT p95, latency p99, tokens/sec, waiting-queue depth, KV-cache usage — the primary contention signals. (These are conceptual signals; the literal Prometheus series names are version-dependent and pinned per image in the guard spec.) GPU (DCGM): utilization, memory used, temperature. System (exploratory): runqueue latency, syscall/ioctl, IO wait — time-aligned to the spike window; `/proc`/cgroup/node-exporter as fallback where eBPF is unavailable. Derived: interference ratio (`colocatedP99 / baselineP99`) with a bootstrap 95% CI, and an **estimated** cost per 1k tokens (`cost_per_1k_tokens_estimated`) — labeled estimate because the cost model's assumptions are explicit and approximate.

## Load protocol (applies to every run)

- **Open-loop arrivals (Poisson).** A closed-loop client waits for each response before sending the next request, so queueing delay suppresses its own measurement (coordinated omission). Tail latency claims from closed-loop load are invalid; the harness enforces open-loop.
- **Pinned generator** (tool + version recorded in the report), streaming on, request timeout recorded as an error, **retries = 0**.
- **Sample size**: ≥ 1000 requests per run for the victim tenant; a p99 needs the tail populated.
- **Repetitions**: ≥ 5 per condition; report median-of-runs and a bootstrap 95% CI, not a single best run.
- **QPS calibration**: contender QPS points are set at ~30/60/90% of the measured single-tenant saturation QPS (from the R0 sweep; S1 plays the same role in the sharing-mode track), not chosen by feel. The saturation sweep is committed as evidence.

## Minimum publishable result

- vLLM workload runs on one Kubernetes GPU node.
- Two tenant workloads are benchmarked under the same protocol (same warmup, repetitions, token shapes).
- The tenant-B ON/OFF comparison is **completed** — if no measurable effect appears, the null result is published with its conditions and likely explanation (publishing is not contingent on finding interference).
- Results include p50/p95/p99 (+ CI), TTFT, TPOT, tokens/sec, GPU util.
- vLLM queue / KV-cache metrics are time-aligned with the latency-spike window.
- The report explains the most likely bottleneck **without overstating causality**.

## Execution reality and cost (commitment, not a hedge)

This benchmark requires a **real GPU node**. Locally we build the CRD, the harness, the metric wiring, and the report skeleton; measured numbers, once the real-GPU run happens, will be committed under `evidence/benchmark-reports/` — that directory is empty today (no run has occurred).

The real-GPU run is **planned and required for M5's definition of done** — not an optional extension. Cost basis: `g5.xlarge` (1× A10G) is ~$1.0/hr on-demand in `us-east-1` (region recorded in the run report; ap-northeast-2 is ~40% higher). The full flagship (calibration sweep + R1–R4 × 5 repetitions) is an estimated 6–10 **instance-hours** ≈ $6–12 before storage, data transfer, and failed runs — **budget cap $30**, enforced by the M5 AWS design's teardown controls (TTL tags, scheduled destroy, budget alarm). At that price, "if real-GPU time is unavailable" is not a defensible fallback. Until the run happens, status numbers stay absent (never invented) and this doc says "designed, not yet measured."

## Interview one-liner

> The core of this project is not claiming a winning GPU-sharing strategy. It is reproducing tenant-to-tenant GPU contention and explaining the p99 latency spike with metrics and a repeatable benchmark — including admitting when the effect is small and why.

## M5 Flagship Experiment — KV-cache-aware Noisy Neighbor

Design-of-Record. The flagship benchmark **tests whether** a long-context noisy neighbor degrades a premium tenant's p99 latency on a **single shared vLLM instance**, and whether the gateway's KV-cache-aware admission guard protects the premium tenant — at what cost to the standard tenant. It does not claim perfect GPU isolation.

This is an **M5 target**. Order: NodeHealth → GPUQuotaPolicy → InferenceDeployment → Gateway (M4-b) → admission guard + GpuSharingBenchmark → real-GPU run. As of 2026-09-05 everything through the admission guard is built and the real-GPU run has happened: four paid repetitions on 2026-09-03 and an engine-level scheduler microtest on 2026-09-04. The gateway is still unit-tested and never deployed. `GpuSharingBenchmark` has no CRD, though its sizing arithmetic and run script exist and the matrix has never run. The guard's own result is a negative one — it missed a pre-registered 1.25x premium-tail target at 83.7x, and the run was declared invalid rather than reported.

### Topology

One vLLM instance serving one 8B-class model; both tenants reach it through the gateway (shared KV-cache pool). This is what makes "KV cache pressure" the plausible interference mechanism — and it is a realistic GPUaaS shape: one popular model, many tenants. Distinct from S2/S3 (separate pods, shared GPU), which are reported separately.

The run protocol **pins the full runtime**: exact model ID, dtype/quantization, `max-model-len` (must accommodate the 8k-token neighbor on 24 GB A10G — likely a quantized 8B or reduced `gpu-memory-utilization`), `max-num-seqs`, and the vLLM image tag. All recorded in the report; "llama3-8b" in the samples is shorthand, not the pin.

### Independent variable — the admission guard

Designed in `docs/superpowers/specs/2026-07-04-m5-admission-guard-design.md` and implemented in the gateway (`internal/gateway/kvguard.go`) — **built and unit-tested, never exercised against a real GPU**, and its vLLM metrics fixture is synthetic, so the thresholds below are unvalidated: the gateway scrapes vLLM's `gpu_cache_usage_perc` / `num_requests_waiting`, and while pressure is engaged (hysteresis-guarded thresholds) it selectively rejects **standard-tier long-context** requests with 429 `kv_cache_pressure`. Premium requests always pass. Static token bucket (M4-b) stays on in all gateway runs.

### Runs

| Run | Gateway                  | Guard   | Description                                                                                                                                                                                                                                  |
|-----|--------------------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| R0  | on                       | off     | **calibration only**: single-tenant saturation sweep (varying QPS) — feeds QPS points and the guard's W threshold; not part of the protection comparison                                                                                     |
| R1  | on                       | off     | premium tenant alone at **fixed** QPS, short interactive workload — the baseline the protection criterion compares against                                                                                                                   |
| R2  | **off** (direct to vLLM) | —       | premium + long-context neighbor, no gateway — **raw-serving reference only** (the direct path differs from the gateway path in more than the bucket: auth, parsing, routing, connection handling — so R2 is context, not an isolation claim) |
| R3  | on                       | **off** | same colocation, token bucket only — **tests whether** the configured static limit protects p99 (a stricter limit might; only this configuration is tested)                                                                                  |
| R4  | on                       | **on**  | same colocation, guard engaged — the A/B against R3                                                                                                                                                                                          |

The guard's effect is isolated by **R3 vs R4** (identical path, one flag). R0 is excluded from all latency comparisons — a sweep's QPS distribution is deliberately not the baseline's. (Earlier drafts conflated calibration into R1 and had R2 ≈ R3; this table is the corrected design.)

### Hypothesis

The colocated long-context workload increases vLLM KV-cache pressure and queue depth, which correlates with premium TTFT/TPOT p99 spikes. KV cache pressure is treated as an **operational signal, not a proven root cause** — analysis wording stays at "correlates with" / "is consistent with" / "most likely bottleneck," never causal proof from time-aligned metrics alone.

### Pre-registered success criteria (fixed before the run)

- **Protection claim allowed only if**: R4 premium p99 ≤ **1.25 ×** R1 premium p99 (median of ≥5 reps, CI reported).
- **Cost reported alongside**: standard tenant's guard-429 rate and throughput loss in R4 vs R3. Suppressing tenant B entirely is not "protection"; the fairness cost is part of the result.
- If the threshold is missed, the report states the guard did not protect at these settings — a null/negative result is published, not reworded.

### Metrics

| Layer     | Metrics                                                                                                                    |
|-----------|----------------------------------------------------------------------------------------------------------------------------|
| Gateway   | request count, 429 count (split: `rate_limited` vs `kv_cache_pressure`), duration, guard-engaged gauge, tenant, request_id |
| vLLM      | TTFT, TPOT, running/waiting requests, KV cache usage                                                                       |
| GPU       | utilization, memory used, power, temperature                                                                               |
| Benchmark | p95/p99 (+ bootstrap CI), error rate, per-tenant throughput                                                                |

### Minimum evidence

`saturation-curve.csv`, `r1-baseline.csv`, `r2-colocated.csv`, `r3-bucket-only.csv`, `r4-guard-on.csv`, `p99-comparison.png`, `kv-cache-pressure.png`, `gateway-429.log`, `analysis.md` — the intended artifact set, to be committed under `evidence/benchmark-reports/noisy-neighbor/` once the real-GPU run happens. Neither the run nor that directory exists yet.

### Non-goals

- Proving hard GPU isolation
- Implementing a custom scheduler
- Modifying vLLM's internal scheduler or KV cache manager
- Claiming causal proof from time-aligned metrics alone
