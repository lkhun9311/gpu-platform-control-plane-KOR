# Queuelab reclaim latency decomposition — design of record (2026-08-02) — SUPERSEDED

> **SUPERSEDED the same day by `2026-08-02-queuelab-termination-contract-design.md`.** A third adversarial
> round returned NO-GO: the eleven-component decomposition is not identifiable (Kubernetes condition
> timestamps have a one-second floor), the spec ordered `Workload Finished` before owner admission which is
> wrong for the `Any` path, and `ΔQ_admit − ΔQ_ready` is a difference of gaps rather than the correction it
> was claimed to be. That same round also noticed the victim's stop reason was `Succeeded`, which led to the
> discovery that the lab's workload ignores SIGTERM and therefore could not be preempted at all. This
> document is retained for the record; do not implement it.

Supersedes the "statistics layer" follow-up implied by `2026-08-01-queuelab-design.md`. That document's
statistical design (paired repetitions, bootstrap CIs over a trace family) is **not** what this milestone
builds, for the reasons recorded below.

## Why this replaces the obvious next step

The obvious next step after the 2026-08-02 live runs was "add repetitions and bootstrap confidence
intervals". Two adversarial review rounds killed that and its successor:

**Round 1 killed the dose-response / crossover study.** With a one-GPU borrower of service duration `D`
preempted at borrower age `x`, the ledger's own definition gives waste `W_Any = g_b (x + L_stop)` and the
owner's wait under `Never` is `Q_Never = max(D-x, 0) + L_release`. Ignoring overhead these are `x` and
`D-x`, so an "equal cost" crossing sits at `D/2` — arithmetic imposed by the fixture, not a discovered
scheduler threshold. The two quantities also carry different units and different meanings (a queued GPU
request is deferred work; a preempted attempt is destroyed modelled progress). Any scalarization
`J = c_w W + c_q Q` needs weights that encode job value and SLO penalties; choosing them after seeing the
curves means choosing the answer. There is no universal crossover, only a family of crossovers indexed by
business weights. A confidence interval around a mechanism whose sign is deterministic by construction
answers the wrong question.

**Round 2 killed the timing-boundary / race-probability study that replaced it.** The proposal was to hunt
the point where the borrower's natural completion beats Kueue's reclamation. That boundary is
`x* ≈ S_Ready→finish + R_completion→release - R_reclaim`, and with reclamation taking a few hundred
milliseconds a one-second level grid at 52/56/58 s would return 100% preemption at every level: 60 paired
blocks spent confirming a deterministic outcome. Worse, the boundary is not even well defined today — the
borrower runs `sleep D` from container start while the protocol measures age from Pod Ready, so service
from Ready is `D` minus container-start-to-Ready latency, and that uncertainty lands exactly where
sub-second resolution would be needed. A boundary hunt would also require a pilot to place its levels, and
ten trials at a proportion near 0.5 give a ±0.3 interval (≈43 runs for ±0.15, ≈96 for ±0.10).

## What this milestone actually is

A **latency decomposition** of in-cohort reclamation, plus the runner-validity surgery that makes any
measurement from this harness admissible.

It exists to correct a specific overclaim in the 2026-08-02 result. That report's headline was "`Any`
admits the returning owner in ~120 ms". Admission is a *quota reservation*, not service restoration: the
preempted borrower held the fake GPU for roughly nine more seconds. The operationally meaningful quantity
is owner **Pod Ready**, and the interval between the two is exactly the thing nobody publishes.

Claim scope: this characterizes **this harness's control-plane and termination latencies** on one pinned
fixture. It identifies no optimal policy, no general race-probability curve, and no production behavior.

## Gate 0 — validity surgery (no data is collected until all of this lands)

Round 1 and 2 found that the pure `internal/queuelab` layer is defensive but the live composition around it
is not. Each item below is a confirmed defect in the current runner, not hygiene.

Required:

1. **Synchronized initial list + resourceVersion-continuous watch** for MLTrainingJob, Job, Workload, and
   Pod. Submission blocks until every collector reports synchronized. Today `collector.go` launches four
   raw watches and submission begins immediately; watch errors are silently retried, only Pods get a
   partial relist, and relisted objects are not fed through the ledger. A missed `Admitted`, `Preempted`,
   or `Completed` transition can therefore vanish silently, which contradicts the fail-closed contract that
   justifies the exact-waste and censoring logic.
2. **Dedicated namespace per arm.** Watches currently cover a whole shared namespace with no run selector,
   and descendant filtering is not available: the controller builds the child Job with only the Kueue queue
   label and does not propagate the MLTrainingJob's run labels. Fixed job names (`a1`, `a2-borrow`,
   `long1`, …) collide across runs — verified live, where a prior run's five MLTrainingJobs were still
   present hours later. Do not pretend run-label filtering of descendants exists.
3. **Complete cleanup with a zero-residual audit.** The only current cleanup is a deferred removal of the
   worker label and taint. A `NoSchedule` taint does not evict Pods already on the dedicated worker, and
   leftover Pods hold real device-plugin capacity.
4. **Reject `AlreadyExists` fixtures.** They are currently accepted without verifying the existing spec.
5. **Server-read effective policy.** Read fixtures back, verify ClusterQueue and LocalQueue `Active=True`,
   and **actually call `AssertOneKnobDiff`** on the server-read pair. It has zero callers today: the
   "effective policy verification" that the 2026-08-01 design calls a core discipline has never executed on
   the live path.
6. **Structured validity artifact and non-zero exit on invalid runs.** Collector invalidation and
   reconstruction failure currently print text and return `nil`, so automation cannot distinguish a valid
   result from an invalidated run.
7. **Unresolved-transition check at shutdown.** Pending UID-chain resolutions are kept indefinitely and
   never audited at run end.
8. **Event-driven Pod Ready timing.** The dose clock currently starts when a two-second poll observes the
   derived MLTrainingJob `Running` phase, which merely means `Job.Status.Active > 0` — not Pod Ready. The
   effective timing uncertainty is about two seconds.
9. **Pre-pulled, digest-verified images** on the dedicated worker, so image pull never lands inside a
   measured interval.

Explicitly **cut** from Gate 0 (each was considered and rejected):

- Immutable run-label propagation, made unnecessary by dedicated namespaces.
- Restarting Kueue between arms — adds cold-start and leader-election noise. Restart only after a failed
  reset, and invalidate that block.
- A fresh kind cluster per paired block — injects cluster startup, image-cache, controller-election, and
  node-load variation. One frozen dedicated cluster with verified resets instead.
- Adding `backoffLimit` to `MLTrainingJobSpec`. The field is absent today and the controller omits it from
  the child Job, so `ClassifyJob`'s comment claiming "a lab Job runs with backoffLimit 0 (set by the
  runner)" is **false and must be corrected**. But a retry after a deliberate preemption is downstream of
  owner admission and irrelevant to this experiment. Close the confound by invalidating any uncaused Pod
  failure or replacement attempt, not by extending the product API.
- A formal A/A null-control stage. Counterbalancing balances order effects without measuring them, and A/A
  would expose them — but a defensible noise-floor quantification needs roughly the replication of a real
  level. Run two engineering shakedown pairs (one Any/Any, one Never/Never) before freezing to detect gross
  carryover; claim nothing quantitative from them.

## Ledger vocabulary expansion

The current vocabulary (`Submitted`, `Admitted`, `PodReady`, `Preempted`, `AttemptStopped`, `Completed`)
cannot express the decomposition. Add the milestones below, each stamped from the authoritative transition:

- Owner API request start and response, plus the server-side creation timestamp.
- Child Job creation.
- Workload creation.
- Borrower preemption / eviction condition.
- Borrower Job suspension or Pod deletion initiation.
- Borrower Pod terminal.
- Borrower Job `Complete`.
- Borrower Workload `Finished` (the quota-release milestone Kueue actually acts on).
- Owner Workload quota reservation / admission.
- Owner Job unsuspension.
- **Owner Pod Ready.**

`Reconstruct`'s existing refusal semantics (error rather than a number on impossible evidence) extend to
the new milestones: an ordering that could not have happened invalidates the run.

## Experiment

Reclaim study only. The FIFO study stays exactly as it is — a deterministic mechanism demonstration. It is
not converted into a second pseudo-statistical curve.

- Fixed three-job fixture, one-GPU borrower and owner, borrower duration `D = 60 s`.
- Two owner-return ages: **5 s** (early return, near-free preemption) and **55 s** (late return, near a
  full job of discarded work). Age is measured from the borrower's authoritative **Pod Ready** transition.
  These are representative operating points, not boundary probes, so the well-known gap between Ready-based
  age and `sleep` progress does not have to be resolved — but it must be measured and reported: the
  container-start-to-Ready interval is recorded per run, and the report states plainly that Ready-based age
  understates elapsed service by that amount. This is also why 55 s is chosen rather than something nearer
  60 s: the level must stay clear of the completion boundary that Round 2 showed is not well defined.
- **10 valid paired blocks per age**, counterbalanced 5 in `Any→Never` order and 5 in `Never→Any`. Ten
  rather than six because machine time is not a constraint here and the extra pairs improve the component
  distributions without changing any claim; it remains far too few for tail inference, so no p95 or p99 is
  reported.
- One frozen dedicated kind cluster, verified reset between arms.
- Record the **realized** borrower age at owner API acceptance and at the preemption decision, and pair on
  the realized value. Do not analyse against the nominal target — that is exposure error.
- Paired-arm targeting tolerance is **measured in shakedown, not invented beforehand**. With the current
  two-second poll a 250 ms tolerance would reject most pairs; after event-driven Ready scheduling and
  absolute timers it should be conservative on local kind. Establish the targeting-error p99 in shakedown
  and pre-register the resulting tolerance.
- Pre-register the maximum attempted blocks per cell. Report every invalid attempt and its reason. Never
  silently rerun until the data look clean.

## Endpoints and reporting

Primary: the **component latency distributions** of the decomposition above, per arm and per age —
owner acceptance → Workload created → preemption decision → borrower Pod stop → borrower Workload
`Finished` → owner admission → owner Job unsuspend → **owner Pod Ready**.

Reported alongside, never collapsed into a scalar winner:

- `ΔQ(x) = Q_Never,owner(x) − Q_Any,owner(x)` for both the *admission* and the *Pod Ready* definitions of
  `Q`. The gap between those two ΔQ values is the correction this milestone exists to publish.
- `W_Any(x)`, the borrower's discarded modelled occupancy, as a separately reported price.

Show every paired point, paired medians, ranges, and full timelines. **No inferential confidence interval**
is reported for this mechanism experiment. Do not present measurement-versus-identity residuals as a
finding: with `W = g_b (x + R_decision + L_termination)` the residual is zero by construction, so it is a
ledger consistency check and is reported as such.

Diagnostics reported per run: preemption occurred with exactly `InCohortReclamation`; borrower completed
before reclamation (yes/no); successor Pod attempts; invalid-run count with reasons.

## Freeze list

Freeze and hash before confirmatory collection: Kueue image, version, configuration and feature gates;
kind node image digest and Kubernetes version; operator commit and image digest; fake-device-plugin image
and advertised capacity; sleeper image digest; Job restart policy and termination grace period; node
topology, worker labels and taints; controller replicas and concurrency; trace, the two timing levels,
horizon, and the shakedown-derived submission tolerance; reset procedure; randomization and analysis seeds;
host identity and a background-load snapshot; invalidation rules; analysis code commit.

## Decision rule

None is offered. A policy recommendation would require an operator to supply, before seeing data, a maximum
acceptable owner admission latency `L`, a maximum acceptable discarded borrower work `B`, and a minimum
worthwhile owner-wait reduction `δ`. Absent an external workload policy, report the two-dimensional
tradeoff and make no recommendation.

## Known residual overclaim

`AssertOneKnobDiff` proves a single difference only within its ClusterQueue policy projection. It does not
prove identical controller state, Kueue configuration, node conditions, LocalQueues, or caches. Say so in
the report rather than letting "only one knob differs" stand unqualified.

## Follow-through on the published result

`storage/.../engineering-journal/2026-08-02-queuelab-live-reclaim-result.md` currently reports "owner
admitted in ~120 ms" without the qualification that the borrower held the unit for roughly nine seconds
more. When this milestone lands, that document gets a correction section rather than a silent edit: the
original number was a quota reservation, the decomposition shows what service restoration actually cost,
and the correction is part of the story. The two-round adversarial record that killed the crossover and
boundary designs is retained for the same reason.

The two live runs that produced that result were executed without `-out`, so their ledgers were never
persisted and the millisecond-resolution latency components are not recoverable from them. Every run in
this milestone persists its ledger JSONL as a precondition for being counted.

## Out of scope

Production latency estimates; GPU utilization or throughput claims; large-cluster or multi-node behavior;
gang scheduling; distributed training; MIG, topology, NUMA, time-slicing, or device-allocation behavior;
checkpoint-aware preemption; elastic workloads; heavy-tailed service times; unequal job values, tenant
priorities, quotas, lending limits, or borrowing caps; multiple cohorts, flavors, or admission checks;
fragmentation beyond a two-unit pool; controller behavior under API pressure or leader failover; seeded
trace families and Poisson traffic; Prometheus, dashboards, fairness and demand-satisfaction extensions;
real-GPU testing.

Fake GPUs are acceptable evidence for Kueue quota mechanics. They are not acceptable evidence for
operational economics.

## Honest headline

> On one fixed kind cluster running Kueue v0.18.3, in-cohort reclamation reserved quota for a returning
> owner quickly after the owner's request, while stopping the borrowed Pod took materially longer; without
> reclamation, owner admission waited for natural completion and its propagation through Job and Workload
> state. These measurements characterize this harness's control-plane and termination latencies. They do
> not identify an optimal policy, a general race-probability curve, or production behavior.
