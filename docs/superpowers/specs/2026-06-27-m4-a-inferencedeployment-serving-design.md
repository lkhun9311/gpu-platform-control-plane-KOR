# M4-a — InferenceDeployment serving reconciler (design spec, codex-reconciled)

Date: 2026-06-27 · Milestone: M4 (serving), sub-project **M4-a** · Author: lkhun9311

> v2: tightened after a cold Codex review — stale-status handling, Degraded precedence, immutable-selector and collision policy, serving basics (probes/named ports), GPU requests==limits, narrowed RBAC, envtest wording.

## Scope

M4 ("manage inference workloads + route through the tenant-aware gateway") is decomposed:
- **M4-a (this spec):** the InferenceDeployment serving reconciler — sync a `Deployment` + `Service`, drive status from Deployment readiness.
- **M4-b (later spec):** the tenant-aware gateway (routing + `GPUQuotaPolicy.rateLimit` token bucket).
- **Deferred (richer CRD fields, not present today):** KEDA autoscaling, `warmCache`, SLO, cross-resource conditions `QuotaSatisfied`/`NodeClassHealthy`.

Non-goals: actual vLLM serving/inference. The local cluster is **kind with simulated GPU** — no real GPU to run a model. M4-a is verified at the object/spec level in **envtest**; real serving + benchmarks are AWS/real-GPU deliverables (M5).

## Current CRD (the contract M4-a implements)

`InferenceDeploymentSpec`: `Model{Name, StorageURI}`, `Image`, `GPUClass`, `GPUCount` (≥0), `Replicas` (≥0), `Port` (default 8080).
`InferenceDeploymentStatus`: `Phase` (`Pending;Progressing;Ready;Degraded`), `ObservedGeneration`, `ReadyReplicas`, `Conditions`.

## Architecture

`InferenceDeployment` (namespaced) reconciles into two owned objects in the **same namespace**, each with a controller owner reference:

**Labels (recommended set), applied to both objects:**
- `app.kubernetes.io/name: inferencedeployment`
- `app.kubernetes.io/instance: <infd-name>` ← **the selector key**
- `app.kubernetes.io/managed-by: gpu-platform-control-plane`
- `platform.lkhun9311.github.io/tenant: <spec.Tenant or namespace>` (collision-safe across tenants)

**Deployment** `name = <infd-name>`
- selector: `app.kubernetes.io/instance: <infd-name>` — **set once and never mutated** (a Deployment's `spec.selector` is immutable after create; changing it would make `CreateOrUpdate` fail forever).
- pod: one container `server`, `image = spec.Image`, **named container port `http` = spec.Port**.
- model wiring: `spec.Model.Name` / `spec.Model.StorageURI` as container args/env (illustrative; exact flags only matter on real GPU).
- GPU: when `spec.GPUCount > 0`, set **both `requests` and `limits` `nvidia.com/gpu` = GPUCount** (extended resources require requests == limits) as an integer `resource.Quantity`; omit entirely when 0.
- probes: readiness/liveness (+startup) HTTP GET on the `http` port (illustrative; the real check runs on GPU/AWS).
- `replicas = spec.Replicas`; `progressDeadlineSeconds` set explicitly (e.g. 600) so `Degraded` detection is deterministic.

**Service** ClusterIP, selector `app.kubernetes.io/instance: <infd-name>`, **named port `http` → spec.Port**.

**GPUClass** is recorded but **not enforced** in M4-a (would later become a nodeSelector/affinity); stated explicitly, not silently ignored.

**No finalizer** — same-namespace owner-ref children are reaped by garbage collection when the InferenceDeployment is deleted (unlike the cluster-scoped GPUQuotaPolicy owning a namespaced ResourceQuota). Tests verify the **owner reference is set** (the precondition for GC); envtest does not run the GC, so it verifies intent, not GC behavior.

## Reconcile flow

1. `Get` the InferenceDeployment; `client.IgnoreNotFound` on miss.
2. **Collision/adoption policy:** if a same-name `Deployment`/`Service` already exists and is **not** controlled by this InferenceDeployment (`metav1.IsControlledBy` false), do **not** adopt it — set `Degraded` (reason `DeploymentConflict`) and requeue. Prevents silently hijacking an unmanaged object.
3. `controllerutil.CreateOrUpdate` the **Deployment**: in the mutate fn, `SetControllerReference` and set only the **managed fields** (selector once; replicas; pod template image/port/args/env/resources/probes; progressDeadlineSeconds). Server-defaulted fields are left untouched → steady state is a no-op (no false-drift loop). `CreateOrUpdate` does **not** auto-retry conflicts; a returned conflict error is propagated and the request is requeued.
4. `controllerutil.CreateOrUpdate` the **Service** the same way.
5. Compute and write status idempotently.

Rationale for `CreateOrUpdate` over M3's explicit `Get`+`DeepEqual`+`Update`: a Deployment carries many server-defaulted fields, so whole-spec comparison yields false drift and update loops; mutating only managed fields is the stable, idiomatic approach for workload objects.

## Status / phase

**Stale-status gate:** interpret the Deployment's status only when `dep.status.observedGeneration >= dep.metadata.generation`. Otherwise the Deployment controller has not observed the latest template → report `Progressing` (never inherit a stale `Ready`/failure).

Precedence (evaluated top-to-bottom):
| # | Condition | Phase |
|---|---|---|
| 1 | `spec.Replicas == 0` | `Ready` — `Available=True, Reason=ScaledToZero` |
| 2 | `dep.status.observedGeneration < dep.metadata.generation` (stale) | `Progressing` |
| 3 | Deployment `Progressing` condition `False` with reason `ProgressDeadlineExceeded` | `Degraded` |
| 4 | `updatedReplicas < spec.Replicas` OR `readyReplicas < spec.Replicas` | `Progressing` |
| 5 | `readyReplicas == spec.Replicas` (and updated) | `Ready` |

- Failure (row 3) is checked **before** ordinary readiness, so a failed rollout is not mis-reported as `Pending`/`Progressing` indefinitely.
- `ReadyReplicas` mirrors `dep.status.readyReplicas`; `ObservedGeneration = infd.metadata.generation`.
- Conditions are written via `meta.SetStatusCondition`, which updates `LastTransitionTime` **only when the condition's status changes** — so re-reconcile does not churn timestamps and `DeepEqual` stays stable (idempotent).
- Status is written only when `equality.Semantic.DeepEqual(old, desired)` differs.
- Cross-resource conditions (`QuotaSatisfied`, `NodeClassHealthy`) are **deferred** (Layer-3 gating, richer-field territory).

## Error handling, quality & RBAC

- Errors wrapped with operation context (`fmt.Errorf("create deployment %s: %w", ...)`), distinguishing Deployment vs Service vs status updates.
- Transient API errors are returned (requeued), not reflected as `Degraded` — consistent with the M3 controllers, so the phase does not flap.
- Effective Go / Go Code Review Comments: early returns, context-first, small functions, doc comments, gofmt/vet/lint clean.
- **RBAC (narrowed from kubebuilder defaults):** `apps/deployments` and `core/services`: `get;list;watch;create;update;patch` — **no `delete`** (owner-ref GC handles deletion). `inferencedeployments`: `get;list;watch`; `/status`: `update;patch`. No `/finalizers` (no finalizer). Regenerate `config/rbac/role.yaml`.
- `SetupWithManager`: `For(&InferenceDeployment{}).Owns(&appsv1.Deployment{}).Owns(&corev1.Service{})`.

## Testing (envtest only)

**envtest** runs the API server + etcd — **no Deployment controller and no garbage collector**. So tests manually patch the Deployment's `status` (`observedGeneration`, `readyReplicas`, `updatedReplicas`, conditions) to drive phase transitions (the technique the NodeHealth tests use for node readiness). kind is reserved for e2e.

Cases:
1. **Create**: Deployment + Service exist with correct image, named `http` ports, `nvidia.com/gpu` **requests == limits**, replicas, the instance-label selector, probes, and a controller owner reference back to the InferenceDeployment.
2. **Idempotency**: a second reconcile leaves Deployment/Service/InferenceDeployment `ResourceVersion` unchanged, and condition `LastTransitionTime`s do not churn. (Useful, but not the sole proof — also assert the no-op via observed object equality.)
3. **Drift recovery**: mutate a managed field (image/replicas) → reconcile restores it; the selector is never mutated (immutable).
4. **Status transitions**: patch Deployment status incl. `observedGeneration` → assert `Pending`→`Progressing`→`Ready` and `ReadyReplicas` mirror; stale `observedGeneration` keeps `Progressing`.
5. **Degraded**: patch a `Progressing=False / ProgressDeadlineExceeded` condition → phase `Degraded`.
6. **Scale to zero**: `spec.Replicas == 0` → `Ready` with `Available=True, Reason=ScaledToZero`.
7. **Collision**: pre-create an unowned same-name Deployment → reconcile reports `Degraded (DeploymentConflict)` and does not adopt/overwrite it.
8. **GPUCount == 0**: Deployment has no `nvidia.com/gpu` request/limit.

Not tested: real vLLM startup/inference (AWS/real-GPU deliverable).

## Out of scope / follow-ups

M4-b gateway; KEDA autoscaling; `warmCache`; SLO; `QuotaSatisfied`/`NodeClassHealthy` gating; `gpuClass` enforcement (nodeSelector/affinity); ConfigMap for runtime args (when the CRD gains `runtime` fields).