# Queuelab termination-contract experiment — design of record (2026-08-02)

Supersedes `2026-08-02-queuelab-latency-decomposition-design.md`, which a third adversarial round returned
NO-GO on. That document's rejection reasons are kept here because they constrain this one.

## Revision 2 (2026-08-05) — the experiment as written was confounded

A fourth adversarial round attacked the implementation outline for the runner and found that the outline
started too low in the stack: the runner's plumbing was being fixed while the *experiment definition* was
still wrong. Four corrections, each verified against the code and the recorded ledger rather than argued:

1. **The `a1` job confounds the primary endpoint.** `ReclaimScenario` gives all three rows the same
   `DurationSec`, and the schedule submits the borrower only after `a1` is Ready — so `a1` always finishes
   *first* and releases a GPU before the victim does. In the recorded run `a1` stopped at 42.607 s, the
   victim at 42.638 s, and the owner became Ready at 43.550 s: the two releases are 31 ms apart, so nothing
   in the data can attribute the owner's execution start to the victim. Revision 1 noticed this in passing
   and failed to carry it into the design as a thing to fix. **`a1` must outlive the entire owner-restoration
   window** so the victim's release is the only one that can enable the owner's placement.
2. **The dose was 49 s, not the 40 s this document claimed.** With `D = 60`, `ReclaimScenario` sets the
   owner offset to `60000 − 10000 = 50000 ms` and the barrier subtracts the borrower's 1000 ms offset,
   giving 49 s. The age must be encoded explicitly, not derived from trace offsets.
3. **The live runner cannot select a termination contract.** The pure renderer gained
   `RenderMLTrainingJobWithContract`, but `cmd/queuelabrun` still calls the compatibility wrapper that
   always chooses `IgnoresSIGTERM`. As written, the A-honor arm does not exist.
4. **"Call `AssertOneKnobDiff` per run" is not executable.** It compares two variants; one run applies one
   variant. The check moves to a pair-level certification step, and it is the wrong check for the primary
   pair anyway — A-honor and A-ignore must have *zero* policy differences, not one.

One further cross-watch causality violation was found beyond the two Revision 1 recorded: `openAttemptAt`
pairs a Workload preemption to a Pod attempt by comparing their arrival timestamps, so a promptly-honoring
victim whose Pod stop is delivered before the Workload preemption is rejected as "already stopped".

## What changed, and why this milestone exists

While reviewing the latency-decomposition spec, the reviewer noticed that the published reclaim result
recorded the victim's stop as `reason=Succeeded` — meaning `pod.Status.Phase == PodSucceeded`, an exit-0
completion. A preempted Pod should not succeed. Reproduction and a controlled test established why:

**The lab's workload cannot be preempted.** `RenderMLTrainingJob` runs `sh -c "sleep N"`, which execs
`sleep` as PID 1. A container's PID 1 receives no default signal disposition, so it ignores SIGTERM. A
controlled test (`kubectl run` a `sleep 60` Pod, then delete it) showed the Pod surviving 34 seconds — the
full 30-second grace period — and dying only to SIGKILL.

In the reproduced run (ledger persisted, `runid v1`):

- The victim was Ready at 3.506 s, marked `Preempted` (`InCohortReclamation`) at 34.076 s, and **ran to
  natural completion at 42.638 s with exit 0**. SIGTERM arrived with ~9 s of service left, inside the
  30-second grace, so SIGKILL never came.
- The owner's Workload was admitted at 34.156 s — **112 ms** after its create — but the owner's **Pod Ready
  was 43.550 s**, a gap of **9.394 s**, because the un-killable victim still held the device.
- The victim's successful attempt was not credited (its Job was suspended), so it was re-admitted at
  44.610 s and **re-executed its full service** (45.556 s → 86.667 s). A 40-second job occupied 83 seconds.
- At 42.607 s the unrelated `a1` also completed naturally, so it is not established that the preemption is
  what freed a device for the owner at all.

The published headline ("`Any` admits the owner in ~120 ms and discards ~40 GPU-seconds of borrower work")
is therefore wrong in mechanism. The waste figure survives numerically — the work genuinely was redone —
but its cause is "a workload that could not be stopped had its completed run discarded", not "in-flight
work was cut short". A correction has already been published rather than deferred to this milestone.

The pure measurement layer did not catch this after three adversarial rounds because `openAttemptAt` in
`ledger.go` charges waste on the inference *a preemption was decided, and the attempt running at that
moment later stopped, therefore that attempt's work was discarded by the preemption*. There is no causal
check. The refuting evidence (`reason=Succeeded`) was recorded in the ledger and never read by any rule.

**So the milestone is no longer a latency decomposition. It is: fix the fixture and the causal accounting,
then measure what the workload's termination contract does to reclamation.**

## The claim this milestone can support

*Preemption is only as effective as the workload's termination contract.* Under an identical
`reclaimWithinCohort: Any` policy, a workload that honors SIGTERM and one that ignores it produce
materially different reclamation outcomes: when it works, discarded work and the owner's execution start
are both governed by the victim stopping; when it does not, the platform's preemption decision has no
effect on execution until the grace period or natural completion resolves it.

That is a platform lesson, not a scheduler benchmark, and it is the empirical motivation for the
checkpoint/resume milestone that follows.

## Constraints inherited from the third review (do not re-litigate)

1. **No component latency decomposition.** `metav1.Time` serializes as RFC3339 and discards fractional
   seconds, so Kubernetes condition timestamps have a one-second floor; sub-second components are not
   identifiable from them. Watch-arrival timestamps have nanosecond resolution but not nanosecond accuracy,
   carrying API persistence, transport, and client scheduling delay. **One measurement authority: the
   runner's monotonic observation clock**, and every reported interval is labelled a *client-observed
   propagation gap*, never attributed to a named controller.
2. **Cross-watch reordering is legal.** Four independent watches do not form a globally ordered stream, so
   an observed order that violates causal expectation is not proof of an impossible execution.
   `Reconstruct` must not invalidate a run for observer-time ordering alone; it may still invalidate on
   evidence that is impossible regardless of ordering (an unknown job, a missing submission, a duplicate).
3. **Two causal DAGs, not one shared chain.** Under `Never`, quota is released through natural completion →
   Job `Complete` → Workload `Finished`. Under `Any`, Kueue marks the victim `Evicted`/`Preempted`,
   suspends its Job, and clears the victim's quota reservation once the Job is inactive; the victim is then
   requeued, not finished. **`Workload Finished` is not the `Any` path's quota-release milestone** and the
   spec must not order it before owner admission.
4. **Pod Ready is an execution-start proxy, not service restoration.** A busybox sleeper has no application
   readiness contract.
5. **"Observed values and range across N runs", never "distributions".** N in single digits does not
   characterize a distribution; no inferential CI, no p95, no p99.
6. **One borrower age.** Multiple ages reintroduce the killed boundary problem and confound age with
   cluster uptime unless fully interleaved.

## Gate 0 — validity work that must land first

Confirmed defects in the live runner (each verified in code):

1. Collectors start raw watches with no initial list and no synchronization barrier, and submission begins
   immediately. On watch failure only Pods are relisted, and relisted objects are not fed through the
   ledger, so a missed `Admitted`/`Preempted`/`Completed` can vanish. Fix: synchronized initial list plus
   resourceVersion-continuous watch for all four kinds; block submission until every collector is synced;
   audit that no unresolved UID-chain transitions remain at shutdown.
2. Watches cover a shared namespace with no run selector, job names are fixed (`a1`, `a2-borrow`), and the
   controller does not propagate run labels to the child Job. Fix: a dedicated namespace per arm.
3. No cleanup; the only deferred action removes the worker label and taint, and a `NoSchedule` taint does
   not evict Pods already present. Fix: full cleanup with a zero-residual audit over namespaced and
   cluster-scoped fixtures.
4. `AlreadyExists` fixtures are accepted without verifying the existing spec. Fix: reject them.
5. `AssertOneKnobDiff` has zero callers. Fix: call it on the server-read pair, and verify ClusterQueue and
   LocalQueue `Active=True`.
6. Invalidated and reconstruction-failed runs return `nil` and exit 0. Fix: structured validity artifact
   and a non-zero exit.
7. The Ready clock starts from a two-second poll of the derived MLTrainingJob `Running` phase, which only
   means `Job.Status.Active > 0`. Fix: event-driven Pod Ready.
8. Ledger persistence is optional. Fix: a run without a persisted ledger does not count as a result.

Added experimental controls (these are controls, not bug fixes, and are labelled as such): pre-pulled
digest-verified images on the dedicated worker.

**New Gate 0 items from this finding:**

9. **A terminable sleeper fixture.** The workload must exit promptly on SIGTERM. Verified by a fixture
   self-test that deletes a Pod and asserts termination well inside the grace period — not by assumption.
10. **Causal waste attribution.** A preemption may only be charged with discarding an attempt's work when
    the attempt's stop is attributable to it. A stop observed as `Succeeded` is natural completion and must
    be reported separately from preemption-caused loss, never silently folded into waste. Where the cause
    cannot be established from Pod state alone, report it as unattributed rather than as waste.
11. **Re-execution accounting.** Record whether a victim was re-admitted and re-executed, and report the
    total occupancy against the job's service time. The published run hid a full re-execution.

**Further Gate 0 items from Revision 2.** Items 1–11 are necessary and were not sufficient.

12. **The protocol comes before the plumbing.** The trace (long-lived `a1`), the explicit 40-second dose,
    and the per-row arm-to-contract mapping land *first*. A perfectly instrumented run of a confounded
    trace is still a confounded run.
13. **Watch continuity uses `RetryWatcher`, and `410 Gone` is fatal.** Initial `List` capturing its
    `resourceVersion`, then `client-go`'s `watchtools.RetryWatcher` from that version — it resumes across
    ordinary disconnects while preserving edges, and deliberately surfaces `410 Gone` and stops. A `410`
    invalidates the run. Do **not** recover from it by re-listing and pretending the lost transitions were
    recovered, and do **not** use an informer as the ledger source: informer recovery collapses intermediate
    transitions, which is exactly what this ledger must not lose.
14. **No causality inferred from timestamp order across different watches.** Workload, Job and Pod arrive on
    independent watches with independent delivery latency. Three checks currently violate this and must go:
    `completed before admitted`, `admitted before submitted`, and `openAttemptAt`'s comparison of the
    preemption instant against attempt ready/stop instants. The victim attempt is identified from the
    protocol — the borrower attempt established Ready before the owner's submission — and unexpected
    cardinality is rejected instead.
15. **Causal evidence for a stop, recorded not assumed.** The ledger must carry the Pod's observed
    `deletionTimestamp` and the container's terminated exit code and reason. A bare `Failed` phase is not
    proof a preemption stopped the attempt; an unrelated crash produces the same phase. For this fixture the
    honoring arm must show the expected deletion observation followed by exit 143.
16. **Environment gate before every arm.** Assert the worker advertises exactly the expected GPU capacity;
    assert no non-system GPU-consuming Pod is on it; verify the pinned images are present by digest; and run
    the terminable-fixture self-test (item 9) with its evidence persisted.
17. **Node state is captured and restored, not deleted.** The runner currently overwrites any existing label
    or taint on the worker and then deletes them on release. Capture the exact prior state and restore it,
    and refuse to start when the worker already carries another run's markers — which also covers the
    concurrent-invocation case that a cluster-wide Lease would guard. A Lease is optional, not required, for
    a single operator running pairs sequentially.
18. **A fixed monotonic horizon instant.** The runner passes `col.elapsed()` *after* cancelling the
    collector, so the horizon is whatever time shutdown happened to take. Stamp the horizon as an absolute
    monotonic instant decided in advance.
19. **Fatal barrier errors propagate immediately.** The barrier loop currently swallows API errors until the
    horizon expires. An error that cannot be retried must fail the run at once.
20. **Shutdown integrity is the full cache, not an empty queue.** Asserting `pending == 0` is not enough: a
    corrupt cached object that produced no pending transition still passes. The final full cache must
    resolve its UID chain successfully, and expected object and attempt cardinality must hold.

Cut for this milestone: FIFO study support; generic `study`/`variant` combinations, replaced by the three
closed arms; storing whole Kubernetes objects in the run artifact, replaced by canonical effective specs,
UIDs, resourceVersions, conditions and checksums; a discovery-wide label-based garbage collector, replaced
by deleting and auditing the exact namespace, fixture UIDs, worker state and GPU-consuming Pods.

Cut (adjudicated, do not add back): Kueue restart between arms; a fresh cluster per block; run-label
propagation for isolation (keep it only as provenance); a `backoffLimit` field on `MLTrainingJobSpec` —
instead detect and invalidate any unexpected successor attempt for either victim or owner; a formal A/A
stage (two engineering shakedown pairs only, which detect gross contamination and establish no noise
floor); a pre-invented submission tolerance — derive it from the maximum acceptable exposure bias, not from
a p99 that two shakedown pairs cannot estimate.

## Experiment

Three-job reclaim fixture on a two-unit pool, one-GPU borrower and owner.

**The trace, with the confound closed.** `a1` (tenant-a's own unit) must **outlive the entire
owner-restoration window** — its service is set well beyond the borrower's service plus the grace period
plus startup margin, so it never releases a GPU during the measured interval. That leaves the victim's
release as the *only* release that can let the owner run, which is the whole premise of the endpoint. `a1`
will therefore still be running at the horizon and appear as unfinished; that is expected and the report
must say so rather than treating it as a defect.

- `a1` — tenant-a, 1 GPU, long-lived.
- `a2-borrow` — tenant-a, 1 GPU, service `D = 60 s`. **The victim.**
- `b1-owner` — tenant-b, 1 GPU, returns at **borrower age 40 s**.

**The dose is encoded explicitly, not derived from trace offsets.** 40 s is measured from the borrower's
authoritative Pod Ready to the owner's Create request start, scheduled on an absolute monotonic timer at
`borrowerReady + 40 s`. It is chosen so the remaining service (~20 s) sits clearly inside the 30-second
grace period: the ignoring fixture then runs to natural completion without ever reaching SIGKILL, while the
honoring fixture stops promptly. It also stays far from the completion boundary the second review showed is
not well defined.

**Realized dose is recorded and bounded.** Each run records the realized borrower age at the owner's
request start. An acceptable exposure error is pre-registered — derived from the maximum endpoint bias the
experiment tolerates, not from a pilot — and a run outside it is invalidated. Runs are never matched on
realized dose after the fact; that is post-treatment matching.

**Arms are a closed enum, and the treatment is the victim's contract alone.**

| Arm | Policy | `a1` | `a2-borrow` (victim) | `b1-owner` |
|---|---|---|---|---|
| `A-honor` | `Any` | honoring | **honoring** | honoring |
| `A-ignore` | `Any` | honoring | **ignoring** | honoring |
| `N-ref` | `Never` | honoring | honoring | honoring |

The contract is per row, not per arm: between `A-honor` and `A-ignore` only the victim's command differs,
so an arm-wide switch — which would change three manifests when the treatment is one workload's behaviour —
is not permitted.

Primary contrast is **A-honor versus A-ignore**. **5 counterbalanced pairs**, arms adjacent within a pair,
pair order alternating, run index and cluster uptime recorded with every outcome. **3 unpaired N-ref runs.**

One frozen dedicated kind cluster with verified reset between arms. "Frozen" means the software is pinned;
etcd size, caches, and controller uptime still evolve, which is why run index and uptime are reported.

**Pair certification.** A single run's artifact is `candidate-valid` at best; treatment isolation cannot be
proven from one arm. A pair is certified only when:

- *A-honor vs A-ignore* — effective queue fixtures **equal**, trace equal, `a1` and owner manifests equal,
  and the victim differing **only** in its termination command.
- *A-honor vs N-ref* — workload manifests equal, effective policy differing only in `reclaimWithinCohort`.

`AssertOneKnobDiff` serves only the second comparison, and even there it covers only its ClusterQueue
policy projection — not ResourceFlavor, LocalQueue, workload manifests or node state, which the pair check
must compare separately. Only certified pairs enter the analysis.

## Endpoints

Per run, from the runner's monotonic observation clock, all labelled client-observed:

- `Q_admit` — owner Create response → first observed owner Workload admission.
- `Q_ready` — owner Create response → first observed owner Pod Ready.

  Both are anchored on the Create **response**. The Create **request start** is recorded separately and is
  the anchor for the realized dose and for causal ordering — a watch observation can legitimately precede
  the response, so one timestamp must not serve both the endpoint and the impossibility check.

- `G = Q_ready − Q_admit` — the owner's admission-to-execution gap, **per arm**. `G_A-ignore` is the direct
  correction to the published "~120 ms" claim.
- Victim: preemption observed → Pod `deletionTimestamp` observed → Pod terminal observed, plus the terminal
  reason, plus whether a successor attempt was admitted and re-executed.
- Preemption-attributable discarded work, reported separately from naturally-completed occupancy.

Report all four `Q` values (two arms × two endpoints) and each arm's `G`. Do not make the difference of two
deltas the headline: `ΔQ_admit − ΔQ_ready = G_A − G_N` is a difference of gaps, not the correction.

Diagnostics per run: preemption carried exactly `InCohortReclamation`; victim terminal reason; successor
attempts for victim and owner; invalid-run count with reasons; run index and cluster uptime.

## Freeze list

Kueue image, version, configuration and feature gates; kind node image digest and Kubernetes version;
operator commit and image digest; fake-device-plugin image and advertised capacity; **both** sleeper image
digests and their exact commands; `terminationGracePeriodSeconds`; restart policy; node topology, worker
labels and taints; controller replicas and concurrency; trace, the single age, horizon, and the
shakedown-derived submission tolerance; reset procedure; randomization and analysis seeds; host identity
and a background-load snapshot; invalidation rules; analysis code commit.

## Regression test

An automated check that every generated report prints owner admission and owner Pod Ready side by side, and
fails any report that exposes admission alone. This is the mechanical guard against repeating the published
overclaim.

## Known residual overclaims to state in the report

- `AssertOneKnobDiff` proves one difference only within its ClusterQueue policy projection. It does not
  prove identical controller state, Kueue configuration, node conditions, LocalQueues, or caches.
- "Exact waste" is exact only under the ledger's observation model — modelled quota occupancy, not GPU work.
- The pure layer rejects internally inconsistent evidence; it does not make watch-observation timestamps
  accurate, and until Gate 0 item 10 lands it did not verify causation either.
- Without in-workload instrumentation, an interval between deletion and terminal state may be termination
  latency or remaining natural service. Only the terminable fixture's stop is attributable to the signal.

## Out of scope

Production latency estimates; GPU utilization or throughput claims; multi-node, gang, or distributed
behavior; MIG, topology, NUMA, or time-slicing; elastic workloads; heavy-tailed service times; unequal job
values, priorities, quotas, or borrowing caps; multiple cohorts, flavors, or admission checks; controller
behavior under API pressure or leader failover; seeded trace families and Poisson traffic; dashboards and
fairness extensions; real-GPU testing. Checkpoint/resume is the **next** milestone, not this one.

## Honest headline

> On one fixed kind fixture running Kueue v0.18.3, in-cohort reclamation made a quota reservation for the
> returning owner within milliseconds of its request, but the owner did not begin executing until the
> victim's Pod actually released the device. With a workload that ignores SIGTERM — a container running
> `sleep` as PID 1 — the victim was never stopped by the preemption at all: it ran to completion, its
> completed run was not credited because its Job had been suspended, and it re-executed in full. With a
> workload that honors SIGTERM the same policy stopped it promptly. These are client-observed measurements
> on one harness across a single-digit number of runs; they characterize the interaction between Kueue's
> preemption and a workload's termination contract, not production behavior or an optimal policy.
