# Kubernetes-native GPUaaS Control Plane

> **Status (2026-08-07).** This document surveys the whole project; the areas it covers are at different
> stages, matched here to the README's evidence table — this banner does not upgrade or soften any of them.
> **Built:** `NodeHealth` (node readiness — but the CR is hand-created and no GPU fault signal reaches it:
> nothing Xid or ECC exists at all, and the DCGM code that does exist is a utilisation reader the queuelab
> uses, not a health input), `GPUQuotaPolicy` (quota), `InferenceDeployment` (serving), `MLTrainingJob` +
> Kueue (training admission). **Built, unit-tested and deployed on kind, never on EKS:** the gateway. **Built and MEASURED
> on a paid GPU:** the M5-b admission guard and benchmark harness — four repetitions on 2026-09-03 and an
> engine-level scheduler microtest on 2026-09-04. The guard failed: 83.7x against a pre-registered 1.25x
> premium-tail target, and the harness declared the run invalid rather than reporting a protection claim.
> **Built, and run for real on kind:** failure & recovery (M7) — a `WorkloadRun` CRD, a controller and a
> driver, with a recorded run in which deleting a serving Pod produced a recovery trail nobody wrote by
> hand. **Designed only — no CRD, no code:** `GpuSharingBenchmark` / performance isolation (though its
> sizing arithmetic and run script exist), the SQLite ledger, the `platformctl` CLI. **Applied and destroyed in one cycle, not
> applied now:** the `cluster` half of the AWS hosting path — its first apply, on 2026-08-31, failed on all
> four node groups against an SCP deny; on 2026-09-18 a full apply of 96 resources succeeded and a
> hand-dispatched teardown removed all 96, verified by resource ID
> (`hack/eks-cluster-cycle-20260918.md`). That cluster carried no GPU node, no load balancer and no Argo CD,
> so the teardown's hard cases are still untested. **Withdrawn once,
> then re-measured, then observed on hardware:** the queuelab reclaim result. Twelve runs on a kind cluster
> carried the banner `device: NOT OBSERVED`; a $3.90 session then reproduced it on four A10Gs, eight runs
> accepted by `-require-device`, and the banner is gone. Owner wait separates by 28.8 s there against 29.0 s
> on kind, so the result survived its own instrument. A second study then showed reservation and use coming
> apart by a factor of four while every reservation-based figure stayed the same, and found that a duty cycle
> synchronised with the exporter's collection interval is reported by whichever single phase the sampler
> happens to sit on -- zero in the run that was measured, but as plausibly a full 98. Every GPU in the kind
> clusters is still simulated by a fake device plugin, and those runs are still reservation.
>
> **Built, and the reason the paid numbers are worth reading:** a characterization harness that pins what
> each GPU-renting script does before it may be run against a card. Three runners, 25 scenarios, each
> driving the real script with recording stubs and diffing the AWS calls, the exit status, what the operator
> was told and what the run directory held. It fixes the host side only, which is stated where it is used
> rather than left to be assumed.

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
| `GPUQuotaPolicy`      | per-tenant GPU quota / rate limit            | type + reconciler (ResourceQuota sync, drift recovery) — M3 merged; `rateLimit` feeds the M4-b gateway — **M4-b merged, gateway built, unit-tested and deployed on kind, never on EKS** |
| `NodeHealth`          | GPU node intake and operational state        | type + reconciler (observe + taint, finalizer, drift recovery) — M2/M3 merged; **no GPU fault signal reaches it** — nothing Xid or ECC exists, and the DCGM code that does exist reads utilisation for the queuelab rather than health for this controller |
| `GpuSharingBenchmark` | declare a noisy-neighbor / sharing benchmark | designed — spec `2026-07-04-gpusharingbenchmark-crd-design.md`; no code yet (M5)                                     |
| `WorkloadRun`         | record a workload execution                  | type + reconciler + driver — `internal/controller/workloadrun_controller.go` (320 lines). A Pod kill was recorded automatically as `Ready → Pending → Ready` with recovery at 20 s on kind (`hack/m7-evidence-trail.log`). It is not a general-purpose ledger |
| `MLTrainingJob`       | Kueue-admitted training job                  | type + full reconciler — translates to a `batch/v1` Job admitted through Kueue, two-tenant cohort borrowing/reclaim preemption, run end-to-end on kind (`hack/m6-kind-e2e.md`) — **M6 merged and built**. It is not the only milestone with a live run record: M7 (`hack/m7-evidence-trail.log`), the gateway chain and the three chaos scenarios have theirs |

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
| `08_INTERVIEW_DEFENSE.md` — **not in this repository** | interview Q&A. `.gitignore` publishes `docs/00`–`07` and `09` only, so this file is untracked and exists on the author's machine alone. This row promised a reader a document they cannot open |
| `09_AWS_INFRA_ARCHITECTURE.md`          | M5-a/M5-b AWS architecture: Terraform states, network, OIDC, GitOps, cost/teardown |

## README vs this doc

The repository `README.md` should be a 30-second summary — the positioning line, the demo path, and links to evidence — and link here for detail. This document is the full overview; the README is the front door.

## One-line positioning

> Kubernetes-native GPUaaS control plane with multi-tenant performance isolation: declare GPU inference workloads as `InferenceDeployment`, govern resources and nodes with `GPUQuotaPolicy` and `NodeHealth`, route tenant LLM traffic through a gateway, and quantify p99 interference under GPU sharing with a noisy-neighbor benchmark.
