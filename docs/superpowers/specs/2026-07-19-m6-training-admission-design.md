# M6 — Kueue training admission (design spec, v1)

> Status: designed. Implemented and verified locally on kind with the fake GPU device plugin (no real GPU, no AWS).
> Design of record: README M6 row, `docs/00_PORTFOLIO_OVERVIEW.md`, and the MLTrainingJob CRD design (`docs/superpowers/specs/2026-06-21-mltrainingjob-crd-design.md`).

## Purpose

Turn the stub `MLTrainingJob` reconciler into a real training-admission path: translate an `MLTrainingJob` into a Kueue-admitted `batch/v1` Job, and translate the Job and its Kueue Workload back into the `MLTrainingJob` status phases. The operator does not reimplement a scheduler; Kueue is the admission engine, and the operator provides the `MLTrainingJob` abstraction, the Job it admits, and the status translation on top.

Quota for training GPUs is owned by Kueue, not a namespace `ResourceQuota`, so `GPUQuotaPolicy` also syncs to a Kueue `ClusterQueue`/`ResourceFlavor` to avoid double-counting the same GPUs.

The whole path is demonstrated end to end on kind using the fake GPU device plugin (from the M5-a GPU-free work) to advertise `nvidia.com/gpu` capacity, so two-tenant fair sharing and preemption are shown with no real GPU and no AWS.

## Scope

In scope:

- **MLTrainingJob controller (rich)**: create an owned, initially-suspended `batch/v1` Job carrying the `kueue.x-k8s.io/queue-name` label and the `nvidia.com/gpu` request from the spec, then translate the Job plus its Kueue `Workload` into the phase ladder (Pending, Admitted, Running, Succeeded, Failed). It imports the Kueue `v1beta1` API to read the Workload's admission and quota-reservation status. Idempotent, finalizer, drift recovery, owned-object conflict guard, envtest.
- **GPUQuotaPolicy to Kueue sync**: for a tenant whose policy opts into training quota, sync the GPU limit into a Kueue `ClusterQueue` (with a `ResourceFlavor` for `nvidia.com/gpu`) instead of double-counting it in a namespace `ResourceQuota`. Owned by the operator, single-owner, drift recovery.
- **Kueue fixtures**: `ResourceFlavor`, `ClusterQueue`, and per-tenant `LocalQueue` manifests under `config/kueue/`, plus sample `MLTrainingJob`s for two tenants.
- **kind E2E**: install Kueue, apply the fixtures, advertise `nvidia.com/gpu` via the fake device plugin, submit two-tenant training jobs, and observe fair sharing and preemption. Real evidence, fake GPU.

Out of scope (deferred, stated):

- Real GPU training and real EKS (needs AWS; the fake GPU makes the admission/quota/fair-sharing evidence available locally, not the runtime GPU behavior).
- Gang scheduling / topology-aware scheduling and multi-cluster (Kueue features beyond single-cluster admission and preemption).
- Job retry/backoff policy beyond `batch/v1`'s own.

## Kueue integration model

Kueue admits work by watching suspended Jobs. The idiomatic pattern, which this design follows:

1. The operator creates the `batch/v1` Job with `spec.suspend: true` and the label `kueue.x-k8s.io/queue-name: <MLTrainingJob.spec.queue>`. The pod template requests `nvidia.com/gpu: <spec.gpuCount>` and runs the spec's image/command with the spec's parallelism/completions.

   **Ownership of `suspend`**: the operator sets `suspend: true` only on create and never reconciles it afterward. Kueue unsuspends the Job to admit it, so drift recovery must exclude the `suspend` field entirely; comparing and resetting it would fight Kueue in a re-suspend loop. The operator's drift recovery covers the immutable spec-derived fields (image, command, requests, parallelism, completions, labels) but treats `suspend` as Kueue-owned.
2. Kueue's job controller sees the suspended, labeled Job, creates a `Workload`, and admits it against the `LocalQueue` -> `ClusterQueue` quota. When admitted, Kueue unsuspends the Job. Under contention, Kueue queues or preempts per the ClusterQueue's cohort and priorities.
3. The operator watches its owned Job and the Kueue `Workload` and derives the phase:
   - Job suspended and Workload not admitted -> **Pending** (queued for admission).
   - Workload admitted (quota reserved) and Job unsuspended, no active pods yet -> **Admitted**.
   - Job has active pods -> **Running**.
   - Job complete -> **Succeeded**; Job failed (backoff limit) -> **Failed**.

The operator reads the Workload's `Admitted` condition and quota reservation for the Pending/Admitted distinction, which the Job's `suspend` flag alone cannot always disambiguate cleanly during transitions.

## Controller design

`MLTrainingJobReconciler`:

- Adds a finalizer; on delete, removes the owned Job (owner reference handles cascade, the finalizer guards ordered cleanup and status).
- Builds the desired `batch/v1` Job from the spec (name derived from the MLTrainingJob, owner reference set, `suspend: true` on create, queue-name label, `nvidia.com/gpu` request, image/command/parallelism/completions).
- Uses create-or-update with an owned-object conflict guard (refuse to adopt an unowned same-name Job), matching the InferenceDeployment controller's pattern.
- Lists/gets the Kueue `Workload` for the Job (Kueue names the Workload deterministically from the Job UID, or the operator matches by owner/label) to read admission status.
- Computes the phase from Job + Workload, writes status only on change (LastTransitionTime on transition), and sets a `Conditions` entry (e.g. `Admitted`) mirroring Kueue.
- Emits the operator custom metrics pattern: a counter for admission transitions (`gpuplatform_mltrainingjob_phase_total{phase}`) at the status write, consistent with the M5-a metrics work.

`GPUQuotaPolicy` Kueue sync (new path, either in the existing controller behind a spec opt-in or a focused second controller):

- When a policy opts into training quota, ensure a per-tenant `ClusterQueue` whose nominal `nvidia.com/gpu` quota equals the policy's GPU limit, and a `LocalQueue` in the tenant namespace bound to it. All these ClusterQueues share one `cohort` and one `ResourceFlavor` for `nvidia.com/gpu`.
- The shared cohort is what makes two-tenant fair sharing coherent: each tenant has its own ClusterQueue (its own nominal quota), and within the cohort an idle tenant's quota can be borrowed and then reclaimed by preemption when its owner submits. A single shared ClusterQueue would give the tenants no distinct quota to be fair about; separate cohorts would let neither borrow. Per-tenant ClusterQueue in one cohort is the only shape that both isolates quota and demonstrates fair sharing plus preemption.
- Single-owner (owner reference / server-side apply), drift recovery, and a clear rule that training GPU quota lives in the ClusterQueue while serving/other quota stays in the namespace ResourceQuota, so the same GPUs are not counted twice.

## GPU-free verification on kind

The fake GPU device plugin (`cmd/gpu-simulator`, `config/device-plugin`) advertises `nvidia.com/gpu` on the kind node (a few fake units). Kueue's `ResourceFlavor` targets `nvidia.com/gpu`, the training Jobs request it, and their pods are CPU-only (busybox sleep) so nothing needs a real GPU. Two tenants each get a `ClusterQueue` in one shared cohort and a `LocalQueue` in their namespace. Submitting jobs that together exceed one tenant's nominal quota but fit the cohort shows borrowing and fair sharing; a higher-priority job in the owner tenant reclaims its lent quota, showing preemption. This is the same "prove it on kind without a GPU" approach the device plugin used.

## Verification

- `go build`, `go test ./internal/...` (envtest: Job creation, spec mapping, phase translation given simulated Job/Workload states, quota-to-ClusterQueue sync, drift, conflict guard). Envtest does not run Kueue, so the controller's admission-status reads are tested against Workload objects the test creates, and the real admission is exercised in the kind E2E.
- `make lint` clean.
- `make infra-validate` still green (Kueue fixtures build via kustomize).
- kind E2E: Kueue installed, fake GPU advertised, two-tenant jobs admitted, fair sharing and preemption observed and captured.

## Testing

TDD with envtest for the controller and the quota sync. The Kueue admission engine itself is not unit-tested (it is Kueue's own code); the operator's contract with it (create a suspended labeled Job, read the Workload, translate status) is tested against fixtures, and the integrated behavior is proven in the kind E2E.

## Korean edition

The controller and quota-sync code are core product code and are mirrored to the Korean edition with Korean comments, as the operator custom metrics were. The Kueue fixtures and the kind demo are English-edition only, consistent with the M5-a infrastructure decision.

## Non-goals

- Real GPU runtime, real EKS, gang/topology scheduling, multi-cluster.
- Reimplementing any scheduling or preemption logic. Kueue owns admission; the operator owns the abstraction and the status translation.
