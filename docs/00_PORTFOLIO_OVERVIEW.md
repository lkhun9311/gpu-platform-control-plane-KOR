# Kubernetes-native GPUaaS Control Plane

> **Status (2026-08-07).** This document surveys the whole project; the areas it covers are at different
> stages, matched here to the README's evidence table — this banner does not upgrade or soften any of them.
> **Built:** `NodeHealth` (node readiness — but the CR is hand-created and there is no GPU-specific fault
> detection: no DCGM, Xid, or ECC anywhere in the Go tree), `GPUQuotaPolicy` (quota), `InferenceDeployment`
> (serving), `MLTrainingJob` + Kueue (training admission — the only milestone with live end-to-end
> evidence). **Built and unit-tested, never deployed:** the gateway. **Built and unit-tested, never run on
> a GPU** (its vLLM metrics fixture is synthetic, so the engage/release thresholds are unvalidated): the
> M5-b admission guard and benchmark harness. **Designed only — no CRD, no code:** `GpuSharingBenchmark` /
> performance isolation, failure & recovery (M7), the SQLite ledger, the `platformctl` CLI. **Code written
> and offline-validated, never applied to AWS:** the AWS hosting path. **Retracted:** the published queuelab
> reclaim result — there are currently zero valid queuelab experimental numbers. No GPU in this project is
> real; every GPU is simulated by a fake device plugin.

A Kubernetes-native control plane for multi-tenant GPU inference workloads — GPU quota, node readiness, LLM serving, performance isolation, noisy-neighbor benchmarking, and observability-driven operations.

This project is **not** a vLLM demo. It treats GPU inference workloads as declarative Kubernetes platform resources and builds the control plane around them.

## What this is / is not

|                      |                                                                                              |
|----------------------|----------------------------------------------------------------------------------------------|
| **What this is**     | A Kubernetes-native GPUaaS control plane                                                     |
| **What this is not** | An LLM demo, a data platform, a full MLOps stack, or a scene-retrieval/vector-index platform |
| **Killer feature**   | Multi-tenant GPU performance isolation & contention-aware control                            |
| **Core demo**        | tenant A/B → quota admission → vLLM serving → noisy-neighbor → metrics → recovery            |
| **Evidence**         | CRDs, controllers, and the gateway (code and unit tests) exist today; benchmark reports, Grafana dashboards, failure reports, and an operations ledger are planned evidence types, not yet produced |

## The main contribution

A multi-tenant GPUaaS control plane that:

1. admits GPU workloads through layered quota control,
2. validates GPU nodes before they serve traffic,
3. routes LLM traffic through a tenant-aware gateway,
4. measures noisy-neighbor effects under GPU sharing,
5. records failures, benchmarks, and lifecycle events as evidence.

## Core CRDs

| CRD                   | Role                                         | Status (2026-07)                                                                                                     |
|-----------------------|----------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `InferenceDeployment` | declare a model-serving intent               | type + serving reconciler (Deployment/Service, phase ladder) — M4-a merged                                           |
| `GPUQuotaPolicy`      | per-tenant GPU quota / rate limit            | type + reconciler (ResourceQuota sync, drift recovery) — M3 merged; `rateLimit` feeds the M4-b gateway — **M4-b merged, gateway built and unit-tested, never deployed** |
| `NodeHealth`          | GPU node intake and operational state        | type + reconciler (observe + taint, finalizer, drift recovery) — M2/M3 merged; **no GPU-specific fault detection** (no DCGM, Xid, or ECC in the Go tree) |
| `GpuSharingBenchmark` | declare a noisy-neighbor / sharing benchmark | designed — spec `2026-07-04-gpusharingbenchmark-crd-design.md`; no code yet (M5)                                     |
| `WorkloadRun`         | record a workload execution                  | sketched (doc 02) only; no spec or code yet (M7)                                                                     |
| `MLTrainingJob`       | Kueue-admitted training job                  | type + full reconciler — translates to a `batch/v1` Job admitted through Kueue, two-tenant cohort borrowing/reclaim preemption, run end-to-end on kind (`hack/m6-kind-e2e.md`) — **M6 merged, built, only milestone with live end-to-end evidence** |

Milestone numbering is unified across all docs and the README: M1 skeleton/CRDs · M2 NodeHealth reconciliation contract · M3 enforcement (taint + ResourceQuota) · M4 serving (M4-a InferenceDeployment, M4-b gateway) · M5-a AWS hosting (Terraform/CI/GitOps, operator on EKS) · M5-b real-GPU flagship (benchmark + admission guard) · M5-c depth (cost/fairness frontier + sharing-mode matrix) · M5-d technical write-up · M6 training admission (Kueue — promoted from stretch 2026-07-04) · M7 failure/evidence (`WorkloadRun`). Older drafts that used other numberings defer to this.

## Execution boundary (honest framing)

The control-plane logic (CRDs, controllers, admission, quota, gateway routing) runs and is tested locally against a kind cluster with **simulated GPU capacity**. Anything that requires real hardware — DCGM metrics, MPS / time-slicing, eBPF runqueue correlation, measured p99 under contention — is marked clearly and is executed only on a **real GPU node** (e.g. AWS a10g). Where a result has not been measured on real hardware, the docs say so rather than inventing numbers. Methodology and null results are reported honestly.

## Document map

| Doc                                     | Contents                                                                           |
|-----------------------------------------|------------------------------------------------------------------------------------|
| `01_REFERENCE_ARCHITECTURE.md`          | one-page architecture, non-goals, execution boundary                               |
| `02_CONTROL_PLANE_API.md`               | CRD family, 4-layer admission                                                      |
| `03_GPU_NODE_LIFECYCLE.md`              | NodeHealth, intake runbook, lifecycle                                              |
| `04_GPU_GOVERNANCE_AND_ISOLATION.md`    | **killer feature**: isolation + noisy-neighbor benchmark                           |
| `05_LLM_SERVING_GATEWAY.md`             | tenant-aware gateway, Open WebUI boundary                                          |
| `06_OBSERVABILITY_BENCHMARK_FAILURE.md` | observability layers, failure reports                                              |
| `07_OPERATIONS_LEDGER_AND_EVIDENCE.md`  | ledger schema, evidence matrix                                                     |
| `08_INTERVIEW_DEFENSE.md`               | interview Q&A (internal/public)                                                    |
| `09_AWS_INFRA_ARCHITECTURE.md`          | M5-a/M5-b AWS architecture: Terraform states, network, OIDC, GitOps, cost/teardown |

## README vs this doc

The repository `README.md` should be a 30-second summary — the positioning line, the demo path, and links to evidence — and link here for detail. This document is the full overview; the README is the front door.

## One-line positioning

> Kubernetes-native GPUaaS control plane with multi-tenant performance isolation: declare GPU inference workloads as `InferenceDeployment`, govern resources and nodes with `GPUQuotaPolicy` and `NodeHealth`, route tenant LLM traffic through a gateway, and quantify p99 interference under GPU sharing with a noisy-neighbor benchmark.
