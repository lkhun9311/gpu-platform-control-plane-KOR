# gpu-platform-control-plane

Kubernetes-native control plane that manages GPUs as a platform resource.

## Overview

Most GPU setups stop at running a single workload. This project treats the GPU as a shared platform resource, covering node readiness, multi-tenant quota, serving, and training through one Kubernetes-native control plane.

## Scope

The control plane is organized into the following areas:

The **State** column is the point of this table: several areas below are designed and written up but have
no code in this repository, and saying which is which is more useful to a reader than a uniform list.

| Area                   | What it does                                                                                     | State |
|------------------------|--------------------------------------------------------------------------------------------------|-------|
| Node readiness         | Mirror a Node's `Ready` condition into a `NodeHealth` CR and taint on degradation                | Built — but the CR is hand-created, and there is **no GPU-specific fault detection** (no DCGM, Xid or ECC) |
| Multi-tenant quota     | Sync per-tenant quota and isolation policy from `GPUQuotaPolicy` into namespace objects          | Built |
| Inference serving      | Manage serving workloads declaratively via `InferenceDeployment`                                 | Built |
| Training admission     | Translate `MLTrainingJob` into queued `batch/v1` Jobs admitted through Kueue (M6)                | Built. It also reports what the tenant waited: `admitToRunningSeconds` on the CR and a histogram beside it, and it **refuses to report a window it did not watch** rather than subtracting Kueue's stored stamp after the fact |
| Gateway                | Tenant-aware serving gateway: API key → tenant, token bucket, model routing, proxy, metrics      | Built and unit-tested; **never deployed** |
| Admission guard        | KV-cache-aware three-arm admission guard and open-loop benchmark harness (M5-b)                  | **Built and measured on a paid GPU.** The guard missed its pre-registered 1.25x premium-tail target at 83.7x over four repetitions, and the harness declared the run invalid rather than reporting a protection claim. Re-analysing the same rows at no further spend then found the occupancy signal the milestone was named for had been unreachable by construction and never fired at all ([write-up](hack/m5d-writeup.md), [analysis](docs/superpowers/specs/2026-09-04-the-layer-not-the-signal.md)) |
| Performance isolation  | Measure multi-tenant noisy-neighbor p99 contention under GPU sharing                             | Sizing arithmetic and the run script are built (`internal/bench/sharing.go`, `hack/m5c-matrix.sh`), and `SharingPlan.Validate` already refuses a card too small to host the matrix. **No `GpuSharingBenchmark` CRD, and the matrix has never run on a card** — reading it, rehearsing it on a kind cluster, implementing its readings, running the runner itself on a cluster and putting a second engine and a mutation battery against the result found 37 defects in 2026-09 that each would have ended or corrupted a paid session, all now fixed and pinned by tests that were deliberately broken to confirm they fire ([pre-registration](docs/superpowers/specs/2026-09-10-does-splitting-the-card-buy-protection.md), [sizing](hack/m5c-sharing-sizing.md)) |
| Paid-run harness       | Pin what a GPU-renting script does before it can be run against a card                            | Built. Four runners, 40 recorded scenarios plus a source-order assertion each: every scenario runs the real script with `aws` and `sleep` replaced by recording stubs and diffs the AWS calls, exit status, messages and run-directory contents against a golden. It pins the **host** side. What it cannot tell — whether a cluster then comes up — is covered separately by two rehearsals that run for real on a local kind cluster: `hack/test/rehearse-bringup.sh` extracts the GPU-free span of a session's user-data, and `hack/test/rehearse-m5c-matrix.sh` runs the M5-c matrix itself with stub engines and simulated devices |
| Failure & recovery     | Inject failure scenarios and record an operational evidence trail                                | Built — `WorkloadRun` CRD, controller and driver — and **run for real**: deleting a serving Pod produced a trail nobody wrote by hand, and the run exposed a defect envtest could not. Two of three scenarios are recordable ([evidence](hack/m6-kind-e2e.md)) |
| Ledger                 | A SQLite ledger projecting CR/status/events                                                      | **Designed only** — no code |
| CLI                    | A `platformctl` CLI                                                                              | **Designed only** — no code |

Training admission (M6) uses [Kueue](https://kueue.sigs.k8s.io/) as the admission engine — this project does not reimplement a scheduler; it provides the `MLTrainingJob` abstraction and the status translation on top of Kueue. For training GPUs, Kueue owns the admission quota (`GPUQuotaPolicy` syncs to ClusterQueue/ResourceFlavor rather than double-counting the same GPUs in a namespace ResourceQuota).

## Architecture

The control plane owns the CRDs and reconciles them into native cluster objects. The data plane is ordinary Kubernetes resources created and garbage-collected through owner references.

## Status

The project is built milestone by milestone.

Each finished milestone is tagged and released, so the code at any stage can be read or checked out
directly from the [Releases](https://github.com/lkhun9311/gpu-platform-control-plane/releases) page.

| Milestone | Scope | Status |
|---|---|---|
| [M1](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m1-skeleton) | Project skeleton and the four core CRDs, verified with envtest | Done |
| [M2](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m2-reconcilers) | Idempotent reconciliation with finalizers and drift recovery (NodeHealth reference) | Done |
| [M3](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m3-enforcement) | Taint unhealthy nodes (NodeHealth enforcement) and sync per-tenant quota into ResourceQuota | Done |
| [M4-a](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m4-serving) | `InferenceDeployment` → Deployment/Service with a phase ladder | Done |
| [M4-b](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m4-serving) | Tenant-aware serving gateway: API key → tenant, token bucket → 429, model routing, proxy, metrics | Done |
| [M5-a](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m5-a-hosting) | AWS hosting: Terraform state bootstrap, EKS, OIDC CI → ECR, Argo CD GitOps, ephemeral apply/destroy with a TTL kill switch | Code done and offline-validated. **`bootstrap` is applied** (state bucket, KMS key, OIDC provider, CI roles, ECR, budget); `cluster` is planned at 96 resources and not applied, so no VPC, EKS or GPU node exists |
| [M5-b](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m5-b-admission-guard) | Three-arm KV-cache-aware admission guard measured on a paid GPU, with an open-loop harness whose pre-registered checks withheld the protection claim. The guard missed its 1.25x premium-tail target at **83.7x** over four repetitions and the run was declared invalid. Re-scoring the same evidence later, at no further spend, showed the arm that matched isolation's tail had admitted **none** of the contending tenant's work — because its bucket was configured smaller than the contender's prompt, which a tail-only reading would have reported as the best result in the study | **Run.** Four paid repetitions 2026-09-03, engine-level scheduler microtest 2026-09-04. A negative result with a measured mechanism ([pre-registration](docs/superpowers/specs/2026-09-05-the-price-of-protection.md)) |
| M5-c | Cost/fairness frontier and sharing-mode matrix (exclusive / time-slicing / MPS) — hardens the M5-b evidence | Card chosen by arithmetic rather than by preference: `SharingPlan.Validate` refuses a T4, which leaves each engine 284 KV tokens against a 7,695-token prompt. All three arms' manifests and the run script are written. **Never run on a card, and "tested" was the wrong word for them** — this line said "written and tested" until 2026-09-10, when reading the runner, rehearsing it against a kind cluster, writing the code for its own readings, running the runner itself, and then attacking the result with a second engine and a mutation battery found 37 defects — among them that neither sharing arm could deploy at all (both overlays targeted a namespace nothing creates), that the control arm had no device plugin, that one pre-registered reading was unreachable because an earlier one subsumed it, and that a port-forward race left two of four arms completing nothing while the report called the result a censored tail. All 37 are fixed and each is pinned by a test that was deliberately broken to confirm it fires. Three of the last thirteen were in the readings themselves, computing a quantity the pre-registration had not asked for while every unit test stayed green. Its deferral used to be justified by device attribution, which was never the obstacle — `hack/m5c-matrix.sh` deliberately runs no observer on the sharing node. The real reason was that its `shared` arm is M5-b's topology on an engine whose budget and policy were unsettled, and those are settled now. **Pre-registered 2026-09-10** against the same bars the previous study used, carried forward rather than chosen, with the outcome space the last one left uncovered closed in advance ([pre-registration](docs/superpowers/specs/2026-09-10-does-splitting-the-card-buy-protection.md), [sizing](hack/m5c-sharing-sizing.md)) |
| M5-d | Technical write-up with the measured numbers | Reasoning, pre-registered checks and stated limits were written BEFORE the run so they could not be fitted to it, and the markers are filled from paid evidence. **Closed on a measured negative.** A successor study then took the same question to the engine's own scheduler — eight configurations, three repetitions, no timeouts — and the best of them holds the premium tail at **20.7x** an isolated baseline against a 2x bar, with its median missing by 2.8x. A contending prompt occupies the engine for about a second and the tail budget is a tenth of that, so dividing the work does not divide the machine ([write-up](hack/m5d-writeup.md), [result](docs/superpowers/specs/2026-09-08-the-load-needs-an-upper-gate.md)) |
| [M6](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m6-training-admission) | Training admission: `MLTrainingJob` → Job + Kueue Workload; two-tenant cohort borrowing and quota-reclaim preemption, run end to end on kind | Done ([evidence](hack/m6-kind-e2e.md)) |
| [queuelab](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/queuelab) | Queue-policy measurement lab: censoring-aware list/watch lifecycle ledger replayed against real Kueue | Withdrawn once, re-measured, then observed on hardware. Twelve kind runs the runner's own gates accept ([result](hack/queuelab-reclaim-first-result.md)) carried the banner `device: NOT OBSERVED`, so every GPU-second in them is a second of reservation. A $3.90 session on four A10Gs then ran eight more, all accepted by `-require-device`, and **`queuelabrun -compare` prints its comparison without that line**. Owner wait separates by 28.8 s there against 29.0 s on kind, so the result survived its own instrument. A second study then separated **reservation from use**: two sets of runs reserved 51.084 and 50.802 GPU-seconds — the same to within a twentieth of their floor — while the card did 48.115 and 12.193 observed device-seconds, a factor of four with every reservation-based figure unchanged ([observation](docs/superpowers/specs/2026-09-05-the-device-was-never-observed.md), [idling](docs/superpowers/specs/2026-09-06-does-reservation-track-use.md), [runner](hack/queuelab-gpu-session.sh)) |
| M7 | Inject failure scenarios and record an operational evidence trail (`WorkloadRun`) | CRD, controller and a single-controller driver, tested on envtest; the trail refuses rather than concludes when it has a hole. `hack/m7-evidence-trail.sh` **has been run**: a real Pod deletion produced Ready → Pending → Ready in a trail nobody wrote by hand, and the run exposed a defect envtest could not (recovery credited to the healthy state the run began in). **DegradedNode has now been recorded too**: a throwaway kind cluster with a worker is the machine whose disruption nobody minds, and stopping that worker's kubelet produced `Ready → Quarantine → Ready` at 0s, 46s and 48s with the verdict `Recovered` — every phase published by the operator rather than by the script. **BackendFallback** was removed from the type because its injection scales a backend to zero, which reports Ready |

**What has not been exercised.** Every GPU in the kind clusters is simulated by a fake device plugin, and
most of this repository has only ever met one of those. Three sessions did not: two paid EC2 runs for M5-b
and one for queuelab, and the State and Status columns above say per row which is which rather than leaving
it to be inferred. This paragraph used to say nothing here had ever run against real hardware, which
contradicted the two rows directly above it. Two distinctions worth stating plainly, because they are easy
to blur:

- The admission guard and its benchmark harness **have** seen a GPU: four paid repetitions on 2026-09-03,
  which is where the numbers in the M5-b and M5-d rows come from. The sentence here used to say they never
  had, and that was written before the run and not updated after it. What preceded the run still stands: the
  metrics fixture is a real capture from the pinned vLLM image and replaced a synthetic one whose
  assumptions it falsified; the guard has been driven through engage and release against a running vLLM; and
  the whole chain — harness, gateway, engine — has carried a request and returned a `kv_cache_pressure`
  rejection ([evidence](hack/m5b-chain-live-evidence.log)). That chain work was on a CPU build, where the
  engine queues before its cache fills, so it exercised the WAITING arm of the engage condition and not the
  KV-usage arm; the paid run is what exercised the second one.
- The contention benchmark, the SQLite ledger and `platformctl` are **not coded at all**. They are design
  documents. Earlier revisions of this README described them as if they existed; that was wrong.

**Flagship benchmark:** KV-cache-aware noisy-neighbor p99 protection — a real-GPU benchmark that compares premium tenant latency under baseline, colocated long-context noisy-neighbor, and Gateway admission-guard modes. It **has** been run on a GPU, and the numbers are a negative result the pre-registered checks refused to call a win: premium TTFT p99 of 82.2 ms isolated against **6,882.0 ms** under the guard, missing the 1.25x target at 83.7x, and the run declared invalid ([write-up](hack/m5d-writeup.md)). This line used to say there were no numbers; there are, and they say the guard did not work.

## The queuelab reclaim result: withdrawn once, and now re-measured

On 2026-08-02 this repository published a live measurement of Kueue quota-reclaim preemption. **It was wrong
and it was withdrawn.** The experiment has since produced a result the runner's own gates accept: twelve
runs, two per cell across two dose regimes, two arms and two workers, carrying
`verdict: admissible-under-implemented-gates` with no failed claims.

Honouring SIGTERM under reclaim discards the work in flight; ignoring it discards none and converts the
victim's remaining service into the quota owner's waiting time, with the preemption recorded as ineffective.
Both arms reproduce across their two runs.

The magnitudes are NOT restated here, deliberately: they were, and they drifted -- this paragraph claimed
four runs after the set had grown to twelve. The result page carries them and is re-derivable from the
records with `queuelabrun -compare`.

What the result supports is a MODEL, `held = min(remaining service, grace)`, checked in both dose regimes
and at the kink between them, rather than any single figure: the owner's wait responds to dose by twelve
seconds across two levels, so it is not a property of the platform and must not be quoted as one. The
honouring arm's own hold measures below the harness's resolution floor and is reported as unresolved rather
than as a small number. Every ledger time is when a watch event ARRIVED, and the gap to the kubelet's own
stamp bounds what is resolvable at all. The GPU is simulated, so these are seconds of RESERVATION and the
records say so. Details, and what the result does not support, are in
[hack/queuelab-reclaim-first-result.md](hack/queuelab-reclaim-first-result.md).

Three of this platform's defences were broken the same way — each expressed a guarantee in terms of a field
the tenant writes — and each was found by attacking it rather than reading it:
[hack/tenant-writable-fields.md](hack/tenant-writable-fields.md).

**The result that survived contact with review** is the other regime. An unresponsive workload defeats
quota reclaim completely while its remaining service fits inside the Pod's termination grace period — it
finishes, nothing is discarded, and the owner waits the whole of that service with the preemption recorded
as ineffective. Once remaining service exceeds grace, it is killed at exactly the grace boundary. So
`terminationGracePeriodSeconds`, set per Pod by the tenant being preempted, is the bound on how badly a
quota-restoration promise can be broken:
[hack/queuelab-grace-boundary.md](hack/queuelab-grace-boundary.md).

What follows is the account of the withdrawn one, kept because the reason it was wrong is the reason the
gates exist.

Nothing was ever preempted: the lab's workload ran `sleep` as PID 1, and a container's PID 1 ignores
`SIGTERM` without an explicit handler, so the jobs ran to completion and were re-executed. A later review
found the experiment's design confounded as well, independently of that bug.

`queuelabrun` **refuses by design to emit a countable result it cannot stand behind** — it exits non-zero and
names the validity claims that failed. The earlier result counted because a run that looked fine was allowed
to count; the runs above count because each one proves it held its worker exclusively for the whole window,
qualified the node it ran on, and observed continuously, and says so in a record a reader can re-derive the
verdict from.

The full account — five mistakes, what each one's evidence was, and what changed — is in
[docs/10_WHAT_I_GOT_WRONG.md](docs/10_WHAT_I_GOT_WRONG.md).

## Tech stack

- Go, controller-runtime, scaffolded with [kubebuilder](https://book.kubebuilder.io/)
- kind for the local cluster, envtest for controller tests
- Kueue (training admission), kube-prometheus-stack (metrics)

## Local development

Requires Docker, Go, kind, kubectl, and kubebuilder.

```bash
# create the local 3-node cluster (control-plane + 2 workers)
kind create cluster --config hack/kind-config.yaml

# generate manifests and build the controller binary
make manifests
make build

# run controller tests (envtest)
make test

# the shared Kueue fixtures — REQUIRED before any GPUQuotaPolicy with trainingQuota works
kubectl apply -k config/kueue
```

`config/kueue` is deliberately outside `config/default`. Its resources are cluster-scoped and referenced by
name — the ClusterQueue the policy controller writes points at a ResourceFlavor called exactly `gpu` — and
`config/default` applies a `namePrefix`, which would rename the flavor out from under that reference.

Applying it is easy to forget, and forgetting it used to fail silently: the ClusterQueue sits
`Active=False FlavorNotFound`, every training Job submitted to it stays suspended, and the policy still read
`Synced=True`. The policy now carries a second condition for exactly this, so the state is visible:

```bash
kubectl get gpuquotapolicy <name> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
# Synced=True QuotaSynced
# Admitting=False ClusterQueueInactive     <- the fixture is missing
```

Simulated GPU capacity on a **kind** worker node, only for end-to-end scheduling/quota-*enforcement* validation (the GPUQuotaPolicy controller itself needs no GPU capacity — it writes a `requests.nvidia.com/gpu` ResourceQuota; capacity matters only when sample pods actually request GPU):

```bash
kubectl patch node platform-worker --subresource=status --type=json \
  -p='[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"4"},
       {"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"4"}]'
```

> This node-status patch holds on kind because no device plugin reconciles GPU capacity there. On a real cluster (e.g. EKS) the kubelet/device plugin owns node status and would overwrite it, so advertise simulated capacity with a device-plugin-style DaemonSet instead.

## Repository layout

```
api/            CRD types
internal/       the substance: four reconcilers, the serving gateway,
                the admission guard and benchmark harness, the queuelab
                measurement layer
cmd/            controller manager, gateway, benchmark harness, queuelab runner
config/         kustomize manifests (CRD, RBAC, manager, Kueue fixtures)
hack/           local cluster config and the M6 end-to-end script + evidence
infra/          Terraform for the AWS hosting path (bootstrap applied; cluster planned only)
docs/           design documents and specs
test/           e2e test scaffolding
```

## License

[Apache 2.0](LICENSE)
