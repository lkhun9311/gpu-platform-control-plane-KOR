# M6 kind end-to-end: two-tenant Kueue cohort borrowing and quota reclaim (fake GPU)

This procedure proves the M6 training-admission path end to end on a local `kind`
cluster, with **no real GPU and no AWS**. The fake GPU device plugin advertises
`nvidia.com/gpu` capacity, Kueue is the real admission engine, and the operator
provides the `MLTrainingJob` abstraction, the suspended Job it admits, the
`GPUQuotaPolicy` to Kueue quota sync, and the status translation on top.

> **Terminology (precise on purpose).** What this demonstrates is **cohort
> borrowing** (an idle tenant's nominal quota is lent within the cohort) and
> **quota reclaim** through classic preemption (`reclaimWithinCohort: Any`). It
> is deliberately NOT Kueue's weighted **Fair Sharing** feature, which needs the
> global `fairSharing` configuration (left disabled here) and adds weighted
> dominant-resource shares and fair-sharing preemption strategies. "Borrowing +
> reclaim" is the accurate name for this evidence; calling it "Fair Sharing"
> would overclaim a mechanism this run does not enable.

The whole run is scripted in [`m6-kind-e2e.sh`](m6-kind-e2e.sh); this document
explains what it does and records the observed evidence.

## What is being demonstrated

Two tenants, `tenant-a` and `tenant-b`, each get a `GPUQuotaPolicy` with
`trainingQuota: true` and a GPU ceiling of 1. The operator syncs each into a
per-tenant Kueue `ClusterQueue` (`gpu-tenant-a`, `gpu-tenant-b`) that share one
cohort `gpu-platform`, plus a `LocalQueue` in each tenant namespace. The shared
cohort has a total nominal quota of 2 `nvidia.com/gpu` (1 + 1).

- **Fair sharing (borrowing):** while `tenant-b` is idle, `tenant-a` submits two
  jobs of 1 GPU each. One fits `tenant-a`'s own nominal unit; the second borrows
  `tenant-b`'s idle unit from the cohort. Both reach `Running` — `tenant-a`
  runs past its own nominal quota because the cohort has spare capacity.
- **Preemption (reclaim):** `tenant-b` then submits a 1-GPU job. Its
  `ClusterQueue` reclaims its nominal unit via `reclaimWithinCohort: Any`, so
  Kueue preempts **one of** the two `tenant-a` workloads — which one is not
  fixed, see the preemption section below. That job's `MLTrainingJob` returns to
  `Pending` (its Job is re-suspended), and `tenant-b`'s job is admitted and
  reaches `Running`.

Priorities are deliberately **not** used here: `reclaimWithinCohort: Any`
reclaims borrowed quota regardless of the borrower's priority, which is exactly
the fairness guarantee being shown. The sample jobs run a long `sleep 600` so the
borrowing job is still occupying quota when the owner reclaims, making the
preemption deterministic rather than a race against a short-lived job.

## Prerequisites

- `docker` running, plus the vendored `bin/` tools (`kind`, `kubectl`, `kustomize`).
- Network access to pull the `kind` node image, the Kueue release manifest, and
  the `busybox` image.
- Go 1.26 toolchain for the image builds (`GOTOOLCHAIN=go1.26.0`, handled by the script).

## Procedure

1. **Create the cluster** — a 3-node `kind` cluster from `hack/kind-config.yaml`
   (1 control-plane, 2 workers).
2. **Install Kueue v0.18.3** —
   `kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.3/manifests.yaml`,
   then wait for `kueue-controller-manager` to be `Available`.
3. **Build and load images** — `make docker-build` (operator) and
   `make docker-build-gpu-simulator`, then `kind load docker-image` both into the
   cluster. Their pull policy is patched to `IfNotPresent` so the kubelet uses the
   loaded images instead of trying to pull `:latest` from a registry.
4. **Deploy** — `make install` (CRDs), the operator via `kustomize build
   config/operator`, and the fake GPU device plugin via `kustomize build
   config/device-plugin` with `FAKE_GPU_COUNT=2` so each node advertises
   `nvidia.com/gpu: 2` (physical capacity comfortably above the cohort's nominal 2).
5. **Apply the fixtures** — the `gpu` `ResourceFlavor`, the `tenant-a`/`tenant-b`
   namespaces, and the two `GPUQuotaPolicy`. The operator creates both
   `ClusterQueue`s and `LocalQueue`s. (The reference `LocalQueue` in
   `config/kueue/localqueue-example.yaml` is **not** applied here, because the
   operator owns the real one and would flag a hand-applied duplicate as a
   conflict.)
6. **Fair sharing** — submit `a1` and `a2` in `tenant-a`; observe both reach
   `Running` (borrowing).
7. **Preemption** — submit `b1` in `tenant-b`; observe one `tenant-a` job return
   to `Pending` and `b1` reach `Running` (reclaim).

Run it:

```bash
./hack/m6-kind-e2e.sh
# evidence is written to hack/m6-e2e-evidence.log
# tear down when done:
kind delete cluster --name platform
```

## Observed evidence

Captured from a real run on a 3-node `kind` cluster with Kueue v0.18.3, the
operator, and the fake GPU device plugin (`FAKE_GPU_COUNT=2`).

### The operator synced the Kueue quota

The two `GPUQuotaPolicy` (ceiling 1 each) produced two `ClusterQueue`s in one
shared cohort with reclaim enabled:

```
NAME           COHORT         NOMINAL   RECLAIM
gpu-tenant-a   gpu-platform   1         Any
gpu-tenant-b   gpu-platform   1         Any
```

`cohortName` and `reclaimWithinCohort: Any` confirm the operator's v1beta1 sync
round-tripped correctly through Kueue's real v1beta2 storage and webhook.

Because both policies set `trainingQuota: true`, no namespace `ResourceQuota`
caps GPUs — the ceiling lives only in the ClusterQueue, so the same GPUs are not
counted twice. Verified against the cluster after the run:

```
+ k get resourcequota -A
No resources found
```

> **Why this line is called out.** An earlier revision of this document showed a
> transcript of this check that had never been run. The capture beside it came
> from a cluster the script had *reused* — `cluster platform already exists;
> reusing it` — carrying a stale `gpuquota-tenant-a-quota` from an earlier
> experiment, and that log contains 27 `exceeded quota` events flatly
> contradicting the claim. It is committed as `m6-e2e-evidence-contaminated.log`
> so the contradiction can be read rather than taken on trust. The claim was
> correct and the evidence was not, which is the worse way round: the conclusion
> survived by luck. The script now runs this check itself, so the line above is
> from the log rather than from prose.

### Fair sharing (borrowing)

With `tenant-b` idle, `tenant-a` submitted two 1-GPU jobs and both reached
`Running`. The borrowing is a property of the ClusterQueue, not of either job:
`gpu-tenant-a` has a nominal quota of 1 and was admitted 2, so one of those two
units is **borrowed** from `tenant-b`'s idle nominal through the cohort
(`gpu(Fit;borrow=1)`). Which job "is" the borrowed one is not a question Kueue
answers, and the preemption section below is where that matters.

```
tenant-a/a1 reached phase Running after 3s
tenant-a/a2 reached phase Running after 0s

NS         NAME   PHASE
tenant-a   a1     Running
tenant-a   a2     Running        <- running past tenant-a's own nominal of 1

NAME           NOMINAL   ADMITTED
gpu-tenant-a   1         2        <- 2 admitted against a nominal of 1 (1 borrowed)
gpu-tenant-b   1         0
```

### Preemption (reclaim)

`tenant-b` then submitted its own 1-GPU job. Its ClusterQueue reclaimed its
nominal unit via `reclaimWithinCohort: Any`, so Kueue preempted one of the two
`tenant-a` workloads. The operator translated the lost admission back to
`Pending`.

```
tenant-b/b1 reached phase Running after 3s
[EVIDENCE] preemption: tenant-a/a2 was reclaimed back to Pending after b1 admitted

NS         NAME   PHASE
tenant-a   a1     Running
tenant-a   a2     Pending        <- preempted, back to Pending
tenant-b   b1     Running        <- reclaimed its own nominal unit
```

> **Which job gets preempted is not a property you can name in advance.** This
> fixture has now been run three times. The first and third runs preempted `a2`;
> the second preempted `a1`. An earlier revision of this document read the first
> run's `a2` as designed behaviour and said Kueue "preempted the borrowed
> `tenant-a` job". All three runs are correct: quota inside a
> ClusterQueue is fungible, so there is no such object as "the borrowed job" —
> `gpu-tenant-a` was simply admitted 2 against a nominal of 1, and when
> `tenant-b` reclaimed, Kueue returned one unit by evicting one of tenant-a's
> workloads. Reading a specific victim as designed behaviour attributes an
> identity to fungible quota, and a claim written that way survives only until
> the next run. What is reproducible is the mechanism the event names — reclaim
> within the cohort, preemptor `gpu-tenant-b`, preemptee `gpu-tenant-a` — not
> which of `a1` or `a2` pays for it.

Kueue's own event names the reclaim explicitly:

```
Normal  Preempted  workload/job-a2-de0c0
  Preempted to accommodate a workload ... due to reclamation within the cohort;
  preemptor path: /gpu-platform/gpu-tenant-b; preemptee path: /gpu-platform/gpu-tenant-a
```

This is the full fair-sharing and preemption story — borrowing under slack,
reclaim under contention — proven on kind with a simulated GPU, no real
hardware and no AWS. Priorities are not used: `reclaimWithinCohort: Any`
reclaims borrowed quota regardless of priority (both jobs here have priority 0),
which is exactly the fairness guarantee under test.

> A quirk worth noting: `kubectl get clusterqueue -o ...spec.cohort` prints
> `<none>` because Kueue serves the stored object as v1beta2, where the field is
> `cohortName`; the cohort is correctly set (see the `cohortName` column above
> and the preemptor/preemptee paths in the event).

## Notes and caveats

- **Physical vs. quota capacity.** The node advertises more `nvidia.com/gpu` than
  the cohort's nominal quota, so admission is bounded by Kueue's quota (the thing
  under test), not by physical scheduling. The pods are CPU-only `busybox sleep`,
  so nothing needs a real GPU.
- **`suspend` ownership.** When Kueue preempts, it re-suspends the borrowed Job.
  The operator sets `suspend` only on create and never reconciles it, so it does
  not fight Kueue in a re-suspend loop; the phase translation reads the Job plus
  its Workload and reports `Pending` for the preempted job.
- **envtest vs. kind.** The unit tests (`internal/controller`) exercise the
  controller logic against vendored Kueue CRDs with no running Kueue. This kind
  run is where the real Kueue admission, borrowing, and preemption are proven.
- **Deferred.** Real GPU runtime and real EKS (needs AWS), gang/topology
  scheduling, and multi-cluster are out of scope for M6.
