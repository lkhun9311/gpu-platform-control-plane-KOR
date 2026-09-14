# Reference Architecture

> **Status (2026-08-07).** This architecture diagram and boundary table mix built and designed-only pieces
> in one picture; the execution-boundary table below is the accurate breakdown. In short: CRDs/controllers
> for `InferenceDeployment`, `GPUQuotaPolicy`, and `NodeHealth` are **built**; `MLTrainingJob` + Kueue is
> **built** (M6, the only milestone with live end-to-end evidence); the gateway (routing, auth, rate limit,
> proxy, metrics) is **built and unit-tested but never deployed**; the M5-b admission guard and benchmark
> harness are **built, and MEASURED on a paid GPU**: four repetitions on 2026-09-03 and an engine-level scheduler microtest on 2026-09-04. The guard failed — 83.7x against a pre-registered 1.25x premium-tail target — and the harness declared the run invalid rather than reporting a protection claim; `GpuSharingBenchmark` and its "thin status
> writer" are **designed only — no CRD, no code**. eBPF and Nsight are **not implemented at all**, and
> neither is Xid or ECC fault detection. DCGM is a narrower case and the blanket claim about it was wrong:
> a reader for `DCGM_FI_DEV_GPU_UTIL` exists (`internal/queuelab/dcgm.go`), fourteen Go files reference
> DCGM, and `config/dcgm-exporter/` deploys the exporter. What does NOT exist is any GPU fault detection in
> the health path — the DCGM code answers "did this card do work, and whose Pod was it", which the queuelab
> uses to tell reserved GPU-seconds from observed ones, and it has never been pointed at a real card. Every
> GPU in the kind clusters is simulated by a fake device plugin. The GPUs in the two paid EC2 sessions were
> real.

## One-page view

```
User / Open WebUI / SDK / platformctl
        |
        v
Gateway-lite
  - API key -> tenant
  - model routing
  - token bucket (RPM / burst)
  - latency / 429 metrics
        |
        v
Kubernetes API  (group: platform.lkhun9311.github.io/v1)
  - InferenceDeployment
  - GPUQuotaPolicy
  - NodeHealth
  - GpuSharingBenchmark
  - WorkloadRun
        |
        v
Controllers
  - InferenceDeployment controller   (built, M4-a)
  - GPUQuotaPolicy controller        (built, M3)
  - NodeHealth controller            (built, M2/M3)
  - GpuSharingBenchmark thin status writer (designed only — no code yet, M5; deliberately not a heavy controller)
        |
        v
Data plane
  - vLLM Pod
  - KEDA ScaledObject
  - ResourceQuota
  - DCGM exporter
  - Prometheus / Grafana
        |
        v
Evidence
  - noisy-neighbor report
  - failure report
  - benchmark CSV
  - operations ledger
```

The API group in code is `platform.lkhun9311.github.io/v1` (kubebuilder domain). Pasted designs elsewhere may show a shorter `platform.ai/v1` — that is illustrative; the implemented group is the one above.

## Control loop

Each CRD encodes an operator intent. A controller reconciles it toward the desired state by creating and owning native objects (Deployment, Service, ConfigMap, ResourceQuota, and a KEDA ScaledObject — *planned*) through owner references, and reflects observed state back into `status` (phase + conditions + observedGeneration). The reconciliation contract — idempotent writes, finalizers, drift recovery — was established on `NodeHealth` first and is reused across controllers. KEDA, DCGM, and vLLM in the diagram are target data-plane components; what is wired today is listed in the execution-boundary table below.

## Execution boundary

| Layer                                       | Runs locally (kind, simulated GPU)                               | Requires a real GPU node  |
|---------------------------------------------|------------------------------------------------------------------|---------------------------|
| CRDs + controllers                          | yes (envtest, kind)                                              | —                         |
| Admission / quota / status                  | yes                                                              | —                         |
| Gateway routing + rate limit                | **built** — M4-b merged, unit-tested (envtest/httptest); binary and manifests exist but the gateway has **never been deployed** | —                         |
| Gateway admission guard (M5)                | **built and measured on a paid GPU.** Four repetitions 2026-09-03. The guard missed its 1.25x target at 83.7x and the run was declared invalid rather than reported. Its engage/release thresholds were the failure: the gateway cannot observe the pressure it gates on | yes (real latency effect) |
| vLLM serving                                | smoke only (no real inference throughput)                        | yes (real serving)        |
| DCGM / GPU metrics                          | **partly implemented, never run on a card.** A `DCGM_FI_DEV_GPU_UTIL` reader, a Pod-attribution resolver, an exporter deployment and a pre-spend gate exist (14 Go files, `config/dcgm-exporter/`). No Xid or ECC. No GPU fault detection feeds NodeHealth | yes (real metrics)        |
| Noisy-neighbor p99, MPS, time-slicing, eBPF | no `GpuSharingBenchmark` CRD, though its sizing arithmetic and run script exist and the matrix has never run. The separate M5-b harness is **built and has produced paid-GPU evidence** | yes (measured evidence)   |

The killer-feature benchmark (doc 04) produces **measured** evidence only when run on a real GPU node. Locally it produces the M5-b benchmark harness and the report skeleton; the `GpuSharingBenchmark` CRD itself is **designed only and not yet implemented**. Numbers are filled from a real-GPU run, and any unmeasured claim is labeled as such.

## Non-goals

This project does **not** implement:

- a custom LLM serving engine (vLLM is the engine; this is the control plane around it),
- production-grade fractional-GPU virtualization (Backend.AI-style fGPU),
- a data lake, vector-index serving, or autonomous-driving scene-retrieval platform,
- a full MLflow-based MLOps platform,
- a custom Kubernetes scheduler replacement,
- multi-cluster GPU federation,
- measured multi-node IB/RoCE results (methodology only).

Cutting these deliberately is what keeps the GPUaaS message sharp.
