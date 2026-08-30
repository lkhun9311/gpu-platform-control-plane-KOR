# Queue-policy replay & regression lab (design spec, v1) — GPU-free second story

Date: 2026-08-01 · Reconciled from a codex cold architecture review (2026-08-01). Builds on M6 (Kueue training admission) without pretending to write a scheduler. GPU-free on kind with the fake device plugin.

## Purpose

Replay deterministic MLTrainingJob traces against the REAL Kueue controller on kind, sweep ONE queue-policy knob per study, and produce a comparative report that answers "what happens if I change production queue policy?". This turns "I configured Kueue" into "I can evaluate and safely operate a shared accelerator fleet."

## Four corrections the review forced

1. **Do not fight the operator.** The GPUQuotaPolicy controller reconstructs `resourceGroups` and forces `reclaimWithinCohort: Any` / `withinClusterQueue: LowerPriority` on the queues it owns (`internal/controller/kueue_quota.go`). The lab therefore creates its OWN dedicated, directly-managed Kueue v1beta2 ClusterQueues/LocalQueues with unique per-run names, and submits through the real path `MLTrainingJob → operator-created Job → Kueue Workload`. It bypasses ONLY `GPUQuotaPolicy → ClusterQueue` provisioning (M6 tests that separately). It does NOT patch operator-owned queues, pause the operator, or add experimental fields to GPUQuotaPolicy.
2. **Name the mechanism precisely.** M6 proves cohort borrowing + quota reclaim via classic preemption, NOT Kueue's weighted Fair Sharing (global `fairSharing` left disabled). The lab studies borrowing/reclaim/FIFO, and never calls them Fair Sharing. (M6 docs corrected accordingly.)
3. **No GPU claims.** Fake-GPU busybox sleepers model restart-from-zero service. Defensible metric names only: quota-pool occupancy, requested-GPU execution occupancy, simulated completed GPU-seconds, restart-from-zero discarded GPU-seconds. NEVER hardware GPU utilization / useful training throughput / GPU efficiency.
4. **Raw lifecycle ledger is authoritative.** A list/watch collector records every transition of MLTrainingJob, Job, Workload, Pod (observed time, resourceVersion, UID, conditions + transition times, pod start/ready/termination). Workload + Pod state are primary evidence; MLTrainingJob phase is a derived cross-check; Kubernetes Events are supporting only. Never derive per-workload p99/waste from Prometheus histograms; Kueue metrics are for health/counter-cross-check/queue snapshots only.

## The two studies (MVP)

### Study 1 (first): reclaim — `reclaimWithinCohort: Never` vs `Any`

Identical nominal quotas, unlimited borrowing. Trace: A occupies its nominal → A borrows B's idle → B submits after the borrowed job has meaningful runtime → observe owner (B) waiting vs borrower (A) restart waste. Repeat with EARLY and LATE owner return (late return, preempting a nearly-complete restart-from-zero job, is where the tradeoff stops being obvious).

### Study 2: FIFO — `StrictFIFO` vs `BestEffortFIFO`

Head-of-line blocking trace: capacity 2 units; a long 1-GPU job holds one unit; a 2-GPU job queues at the head and cannot fit; several 1-GPU jobs arrive behind it. StrictFIFO leaves a unit idle protecting the large job's position; BestEffortFIFO fills it with small jobs (better near-term occupancy, possibly longer large-job wait). Separate the critical arrivals clearly and verify actual Workload creation order (async MLTrainingJob→Job→Workload can reorder close arrivals).

## Metric definitions (fixed horizon H, quota-pool capacity C)

- Initial admission latency: MLTrainingJob acceptance → first Workload `Admitted=True`.
- Kueue-only queue wait: Workload creation → first `Admitted=True`.
- Quota occupancy: ∫(admitted × requestedGPU)dt / (C×H). Execution occupancy: same over Running/Ready pods.
- Simulated useful work: `gpuCount × durationSec` for jobs completed by H.
- Preemption waste: per preempted attempt, `gpuCount × (preemption − podReady)`, counting ALL discarded attempts.
- Tenant demand satisfaction: completed simulated GPU-seconds / offered `gpuCount × durationSec`.
- Fairness: per-tenant satisfaction + min + (max−min) disparity; Jain over demand-normalized satisfaction is secondary, never a replacement.
- Waste ratio: discarded GPU-seconds / total execution GPU-seconds.
- **Starvation needs censoring**: report deadline-miss rate, right-censored time-to-first-admission, restricted mean time-to-admission ≤ H, count + offered work of unfinished jobs at H, longest continuously backlogged interval, observed max wait among admitted, lower-bound for censored. p99 ONLY if ≥99% admitted with adequate n; else use median/p95/timelines (small-trace p99 is theater).

Keep `parallelism=1`, `completions=1` (else gpuCount is per-pod and demand = gpuCount×parallelism).

## Statistical design

Paired comparisons: same scenario trace, same seed, same repetition block, different policy, seeded counterbalanced policy order. Bootstrap PAIRED per-repetition deltas (not pooled workloads, not independent arm means — workloads in one live run are correlated). Bootstrap unit = `(trace seed, repetition)` block. Pre-generate several immutable trace seeds for cross-demand claims. ≥5 reps/cell (10 better); show all raw repetitions; don't oversell a narrow CI from 2–3 reps.

## Determinism & run validity

Pin/freeze: Kueue v0.18.3 image+manifest checksum, kind node image digest/k8s version, operator SHA+digest, sleeper image digest (preloaded on every worker), fake-device-plugin digest + per-node capacity, Kueue Configuration + feature gates, rendered ClusterQueue/LocalQueue fixtures + checksums, trace checksum + gen params, observation horizon, controller replica/concurrency, policy-order seed, report/bootstrap seed.

Per arm: delete previous run namespace + lab queues → wait Pods/Jobs/Workloads gone → **restart Kueue** (clear scheduler cache + process-local metrics) → fresh run-specific namespace/queue names → apply fixture → **read back after conversion/defaulting and compare effective spec to frozen expected** → wait ClusterQueues/LocalQueues Active → start collectors + confirm watches synced → submit trace.

Invalidate on: controller restart during measurement, unrecoverable watch gap, ClusterQueue inactive, image pull, pod unschedulable for non-policy reasons, job failure, unexpected workload count, submission lateness beyond tolerance, arm-dependent admitted-to-running lag, effective-policy mismatch.

Do NOT recreate kind per variant (wasteful + adds bootstrap noise). One clean cluster per repetition block (or whole suite), reset between arms + restart Kueue. Recreate only on version/topology change or cleanup-validation failure.

Prefer HANDCRAFTED deterministic traces for mechanism experiments; add seeded Poisson burst stress only after diagnostics work (random traffic poorly exercises late reclaim / FIFO head-of-line).

## Minimum implementation

- Validated lab manifest (hashes + pinned environment provenance).
- Immutable training trace format (arrival → requested capacity → service duration → optional restart; record scheduled dispatch AND apiserver acceptance; invalidate on lateness > tolerance).
- Open-loop MLTrainingJob submitter (arrivals exogenous; correct queueing model).
- List/watch event ledger for MLTrainingJob/Job/Workload/Pod.
- State-reconstruction/report package WITH censoring.
- Direct v1beta2 Kueue policy fixtures.
- One reproducible runner.
- Markdown + machine-readable JSON report.
- Exact single-run REGRESSION assertions + repeated comparative results, kept SEPARATE.

Reuse from M5-b the DISCIPLINE, not the inference schema: extract small generic helpers (checksum, JSONL, scheduling clock, percentile, bootstrap, invalid-run handling) into a shared experiment-util package; put lab-specific code in a new isolated `internal/queuelab`. Do NOT extend the inference `TraceRow`/`RawRow` with unrelated fields.

## Regression assertions (deterministic, separate from statistics)

- `Any` produces an `InCohortReclamation` preemption and admits the owner; `Never` does not preempt.
- StrictFIFO leaves a fitting younger workload pending behind an unfittable older one; BestEffortFIFO admits it.
- Borrowing never exceeds its configured limit.

## What the output must show (hiring-quality)

Effective policy (not input YAML) · one causal workload timeline (borrow → owner arrival → preemption → restart) · paired effect sizes with CIs · censored backlog + deadline misses · quota occupancy vs execution occupancy · discarded restart work · per-tenant demand satisfaction.

## Cut initially

New CRD · priority (MLTrainingJob has no priority field; later add optional `workloadPriorityClassName` → Job `kueue.x-k8s.io/priority-class` label, never mutate the Job post-create) · fair-sharing weights · Admission Fair Sharing · Poisson generator · Prometheus install/dashboard · full factorial · real checkpoint simulation · kind recreation per arm · p99 claims from small traces.

## The one thing to get right

A trustworthy, censored, list/watch **lifecycle ledger** joined to a frozen, effective-policy-verified run — one causal timeline that a skeptic believes. Everything else (the two studies, the paired stats) hangs off that ledger being authoritative.
