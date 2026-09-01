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
| Training admission     | Translate `MLTrainingJob` into queued `batch/v1` Jobs admitted through Kueue (M6)                | Built |
| Gateway                | Tenant-aware serving gateway: API key → tenant, token bucket, model routing, proxy, metrics      | Built and unit-tested; **never deployed** |
| Admission guard        | KV-cache-aware three-arm admission guard and open-loop benchmark harness (M5-b)                  | Built, never run on a GPU |
| Performance isolation  | Measure multi-tenant noisy-neighbor p99 contention under GPU sharing                             | **Designed only** — no `GpuSharingBenchmark` CRD or code exists |
| Failure & recovery     | Inject failure scenarios and validate the response path                                          | **Designed only** — M7, no code |
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
| [M5-b](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m5-b-admission-guard) | Three-arm KV-cache-aware admission guard and open-loop benchmark harness, with pre-registered checks that refuse to call load shedding a win | GPU-free half done and tested; **no GPU run yet** |
| M5-c | Cost/fairness frontier and sharing-mode matrix (exclusive / time-slicing / MPS) — hardens the M5-b evidence | Card chosen by arithmetic; all three arms' manifests and the run script written and tested; **never run** ([sizing](hack/m5c-sharing-sizing.md)) |
| M5-d | Technical write-up with the measured numbers | Reasoning, pre-registered checks and stated limits written BEFORE the run so they cannot be fitted to it; every figure is still a marker ([draft](hack/m5d-writeup.md)) |
| [M6](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/m6-training-admission) | Training admission: `MLTrainingJob` → Job + Kueue Workload; two-tenant cohort borrowing and quota-reclaim preemption, run end to end on kind | Done ([evidence](hack/m6-kind-e2e.md)) |
| [queuelab](https://github.com/lkhun9311/gpu-platform-control-plane/releases/tag/queuelab) | Queue-policy measurement lab: censoring-aware list/watch lifecycle ledger replayed against real Kueue | Withdrawn once, then re-measured: twelve runs the runner's own gates accept ([result](hack/queuelab-reclaim-first-result.md)). **Simulated GPU** |
| M7 | Inject failure scenarios and record an operational evidence trail (`WorkloadRun`) | CRD, controller and a single-controller driver, tested on envtest; the trail refuses rather than concludes when it has a hole. `hack/m7-evidence-trail.sh` **has been run**: a real Pod deletion produced Ready → Pending → Ready in a trail nobody wrote by hand, and the run exposed a defect envtest could not (recovery credited to the healthy state the run began in). Two of three chaos scenarios are recordable: **DegradedNode** fits but needs a machine whose disruption nobody minds, and **BackendFallback** was removed from the type because its injection scales a backend to zero, which reports Ready |

**What has not been exercised.** Every GPU in this project is simulated by a fake device plugin. Nothing
here has ever run against real hardware, and the State and Status columns above say so per row rather than
leaving it to be inferred. Two distinctions worth stating plainly, because they are easy to blur:

- The admission guard and its benchmark harness have **never seen a GPU**, and that is now the only thing
  missing rather than the whole of it. The metrics fixture is a real capture from the pinned vLLM image and
  replaced a synthetic one whose assumptions it falsified; the guard has been driven through engage and
  release against a running vLLM; and the whole chain — harness, gateway, engine — has carried a request and
  returned a `kv_cache_pressure` rejection ([evidence](hack/m5b-chain-live-evidence.log)). All of that was on
  a CPU build, where the engine queues before its cache fills, so the WAITING arm of the engage condition is
  exercised and the **KV-usage arm is not**. That arm is what the paid run is for.
- The contention benchmark, the SQLite ledger and `platformctl` are **not coded at all**. They are design
  documents. Earlier revisions of this README described them as if they existed; that was wrong.

**Flagship benchmark:** KV-cache-aware noisy-neighbor p99 protection — a real-GPU benchmark that compares premium tenant latency under baseline, colocated long-context noisy-neighbor, and Gateway admission-guard modes. The harness, the guard and the pre-registered checks are written and tested; **it has never been run on a GPU, so there are no numbers.**

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
