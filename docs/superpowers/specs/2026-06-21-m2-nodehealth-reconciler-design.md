# M2 NodeHealth Reconciler Design

- Date: 2026-06-21
- Milestone: M2 (idempotent reconciliation, finalizers, drift recovery)
- Target: NodeHealth reconciler as the M2 reference implementation
- Author: lkhun9311

## Background

M1 defined four CRDs with empty reconcilers. M2's goal (per the README) is reconciliation
hygiene: idempotency, finalizers, and drift recovery. The domain-specific actions for each CRD
are staged later — node taint/cordon and quota sync in M3, serving in M4, training admission in
M5.

Of the four CRDs, only NodeHealth has a real object to observe at M2 (the target Node), which makes
it the natural reference for proving idempotency and drift recovery. We build the full
reconciliation pattern once on NodeHealth and extract the reusable pieces into light helpers in the
`controller` package, so M3–M5 reuse them instead of copy-pasting.

This milestone establishes **observation**, not **enforcement**. The reconciler reflects the target
Node's readiness into NodeHealth status. It does NOT taint or cordon nodes — that enforcement, along
with the Intake/Quarantine phases, lands in M3.

## Goals / Non-goals

Goals
- A finalizer lifecycle on NodeHealth (add on create, remove on delete after cleanup).
- Observe the target Node and reflect readiness into status (phase + Ready condition + observedGeneration).
- Idempotent status updates: write only when the computed status differs from the current one.
- Drift recovery: a manual status edit or a Node change triggers a reconcile that restores the correct value.
- Light, reusable helpers (condition + finalizer constants) in the `controller` package.
- envtest coverage for finalizer, observation, idempotency, drift recovery, and deletion.

Non-goals (M3+)
- Tainting or cordoning nodes (enforcement).
- The Intake and Quarantine phases.
- Reconcilers for GPUQuotaPolicy / InferenceDeployment / MLTrainingJob (those gain the pattern with their domain logic in M3/M4/M5).
- Real finalizer cleanup work — at M2 there is no external resource to remove, so cleanup is a no-op log.

## Design

### Reconcile flow

```
Reconcile(ctx, req):
  1. Get NodeHealth by req.Name (cluster-scoped). NotFound -> return (object gone).
  2. If DeletionTimestamp is set:
       - run cleanup (M2: no external resources -> log only)
       - remove finalizer, Update, return
  3. If finalizer absent:
       - add finalizer, Update, return  (the update re-queues)
  4. Observe the target Node (Get core/v1 Node by spec.nodeName):
       - NotFound      -> phase = Pending,  Ready = False (reason NodeNotFound)
       - Node Ready    -> phase = Ready,    Ready = True  (reason NodeReady)
       - Node NotReady -> phase = Degraded, Ready = False (reason NodeNotReady)
  5. Build desired status:
       - phase (as above)
       - observedGeneration = nodehealth.Generation
       - Ready condition via the condition helper
       - lastTransitionTime updated only when phase changes
  6. If desired status != current status (apiequality.Semantic.DeepEqual):
       - Status().Update(ctx, nodehealth)
     else: no write (idempotent)
  7. return ctrl.Result{}, nil   (no periodic requeue; watches drive re-reconcile)
```

### Helpers (light, reused by M3–M5)

New file `internal/controller/reconcile_helpers.go` (package `controller`, no new package — avoids
premature abstraction):

- Finalizer name constant: `nodeHealthFinalizer = "nodehealth.platform.lkhun9311.github.io/finalizer"`.
- Condition type / reason constants:
  - `conditionReady = "Ready"`
  - reasons: `reasonNodeReady`, `reasonNodeNotReady`, `reasonNodeNotFound`.
- `setReadyCondition(status *platformv1.NodeHealthStatus, ready bool, reason, msg string, generation int64)`:
  thin wrapper over `apimachinery/pkg/api/meta.SetStatusCondition` that stamps `ObservedGeneration`.
  This is the reusable shape — M3–M5 add their own typed wrappers following it.

Finalizer add/remove uses controller-runtime's `controllerutil.AddFinalizer` /
`RemoveFinalizer` / `ContainsFinalizer` directly (no custom wrapper needed).

### RBAC

Add core Node read access to the NodeHealth controller markers:

```go
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
```

Existing `nodehealths` / `nodehealths/status` / `nodehealths/finalizers` markers stay. Run
`make manifests` to regenerate `config/rbac/role.yaml`.

### Watches and drift recovery

`SetupWithManager`:
- `For(&platformv1.NodeHealth{})` — status edits arrive as update events, so a manual status drift
  re-triggers reconcile and is corrected.
- `.Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(mapNodeToNodeHealth))` — a Node
  change enqueues every NodeHealth whose `spec.nodeName` matches, so node-side drift propagates.

`mapNodeToNodeHealth(ctx, node)` lists NodeHealths and returns reconcile requests for those whose
`spec.nodeName == node.GetName()`. It is unit-testable in isolation.

### Idempotency detail

Compute the desired status on a deep copy, then compare with `apiequality.Semantic.DeepEqual`
against the live status. Skip the API write when equal. `lastTransitionTime` only moves when `phase`
changes (handled by `meta.SetStatusCondition` for the condition, and an explicit guard for the
top-level `phase` transition), so a steady state produces no spurious diffs.

## Testing (envtest)

`internal/controller/nodehealth_controller_test.go` (extend the existing scaffolded test):

- Node present and Ready: create the Node (status Ready=True) and a NodeHealth pointing at it;
  reconcile; assert finalizer added, `phase == Ready`, Ready condition True, `observedGeneration`
  set to the object generation.
- Idempotency: reconcile a second time; assert the NodeHealth `resourceVersion` is unchanged
  (no status write).
- Drift recovery (status): set `status.phase = "Quarantine"` manually via the status subresource;
  reconcile; assert `phase` is restored to `Ready`.
- Node NotReady: update the Node condition to Ready=False; reconcile; assert `phase == Degraded`.
- Node absent: a NodeHealth whose `spec.nodeName` has no Node; reconcile; assert `phase == Pending`.
- Deletion: delete the NodeHealth (sets DeletionTimestamp); reconcile; assert the finalizer is
  removed and the object is gone.

`mapNodeToNodeHealth` unit test: given a Node and several NodeHealths, assert it returns requests
only for the matching `nodeName`.

Nodes are plain API objects in envtest (no kubelet), so they can be created and their status
conditions set directly.

## Scope / branch

This milestone covers the NodeHealth reconciler only. The other three CRDs gain the same pattern
when their domain logic lands (M3/M4/M5).

Branching follows the per-milestone convention: create `milestone/m2-reconcilers` off `main`, do the
work on `feat/m2-nodehealth-reconciler`, PR into `milestone/m2-reconcilers`, then a final
integration PR into `main`.