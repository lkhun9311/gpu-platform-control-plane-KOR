# M3 NodeHealth Enforcement Design

- Date: 2026-06-23
- Milestone: M3 (taint/cordon unhealthy nodes; sync per-tenant quota)
- Target: NodeHealth enforcement (first of two M3 sub-projects; GPUQuotaPolicy quota sync follows separately)
- Author: lkhun9311

## Background

M2 gave NodeHealth an observation reconciler: it reads the target Node's readiness and reflects it
into status (Pending / Ready / Degraded), with a finalizer (no-op cleanup), idempotent status
writes, and a Node watch.

M3 adds enforcement. When the target Node is not ready, the reconciler taints it so the scheduler
stops placing GPU workloads there; when the Node recovers, the taint is removed. The finalizer now
does real cleanup — it removes the taint on NodeHealth deletion.

This is readiness-driven (chosen over a fault-signal trigger; fault injection is M6) and uses a
taint (chosen over cordon). The grace-period variant was declined in favor of the simple,
deterministic model: a not-ready node is quarantined immediately.

## Goals / Non-goals

Goals
- Taint the target Node `platform.lkhun9311.github.io/unhealthy=true:NoSchedule` when it is not ready.
- Remove that taint when the Node becomes ready again.
- Drive the phase to Quarantine while tainted, Ready when healthy, Pending when the Node is absent.
- Record `status.faultSignal` (source `node-not-ready`) while quarantined; clear it otherwise.
- Finalizer cleanup removes the taint on NodeHealth deletion.
- Manage only our own taint; never touch other taints.
- envtest coverage for taint apply/remove, phase transitions, faultSignal, idempotency, and finalizer cleanup.

Non-goals
- Cordon (`spec.unschedulable`) — taint only.
- Grace period / flap damping — not-ready quarantines immediately.
- The Intake and Degraded phases — unused here (Degraded's M2 usage is replaced by Quarantine).
- Fault-signal-driven quarantine — fault injection is M6.
- GPUQuotaPolicy quota sync — the second M3 sub-project, designed separately.

## Design

### Taint

- Key: `platform.lkhun9311.github.io/unhealthy`
- Value: `true`
- Effect: `NoSchedule`

The reconciler manages only this taint. A toleration lets explicitly-opted workloads still schedule.

### Phase transitions (readiness-driven)

| Target Node state | Phase | Taint | faultSignal |
|---|---|---|---|
| Not found | `Pending` | (node gone — nothing to manage) | cleared |
| Ready | `Ready` | ensure removed | cleared |
| Not ready | `Quarantine` | ensure present | `{source: "node-not-ready"}` |

The Ready condition from M2 is retained: True when Ready, False otherwise (reasons
`reasonNodeReady` / `reasonNodeNotReady` / `reasonNodeNotFound`).

### Reconcile flow (extends M2)

```
Reconcile(ctx, req):
  1. Get NodeHealth. NotFound -> return.
  2. If being deleted:
       - if finalizer present:
           - Get the target Node; if found, remove our taint and Update the node
           - RemoveFinalizer, Update NodeHealth
       - return
  3. Ensure finalizer present (add + Update + return if it was missing).
  4. Get the target Node:
       - NotFound  -> phase Pending, clear faultSignal, Ready=False/NodeNotFound
       - err       -> return err
       - Ready     -> phase Ready,     ensure taint removed, clear faultSignal, Ready=True/NodeReady
       - Not ready -> phase Quarantine, ensure taint present, faultSignal{node-not-ready}, Ready=False/NodeNotReady
  5. If the node's taints changed, Update the node (enforcement). Errors return for retry.
  6. If the NodeHealth status changed (DeepEqual), Status().Update (idempotent).
  7. return ctrl.Result{}, nil
```

### Helpers (extend `internal/controller/reconcile_helpers.go`)

- `const unhealthyTaintKey = "platform.lkhun9311.github.io/unhealthy"`
- `const faultSourceNodeNotReady = "node-not-ready"`
- Phase constants: keep `phasePending`, `phaseReady`; add `phaseQuarantine = "Quarantine"`; remove
  the now-unused `phaseDegraded` (the `unused` linter would flag it).
- `ensureUnhealthyTaint(node *corev1.Node) bool` — adds the taint if absent; returns whether it changed.
- `removeUnhealthyTaint(node *corev1.Node) bool` — removes our taint if present; returns whether it changed.

`setReadyCondition`, `setPhase`, `isNodeReady`, and the finalizer/condition/reason constants from M2
stay as-is.

### RBAC

Extend the Node rule to allow taint writes:

```go
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
```

Run `make manifests` to regenerate `config/rbac/role.yaml`.

### Watch / drift recovery

Keep the M2 Node watch (`Watches(&corev1.Node{}, ...mapNodeToNodeHealth)`). A Node going not-ready
enqueues the matching NodeHealth, which applies the taint; recovery removes it. A manual taint
removal on a still-unhealthy node is re-applied on the next reconcile (the Node update event also
triggers one).

### Idempotency

The node is updated only when `ensure*`/`remove*` reports a change. The status is updated only when
`apiequality.Semantic.DeepEqual` reports a difference (as in M2). A steady state performs no writes.

## Testing (envtest)

Extend `internal/controller/nodehealth_controller_test.go`:

- Not-ready node: reconcile; assert the node carries the unhealthy taint, `phase == Quarantine`,
  and `faultSignal.source == "node-not-ready"`.
- Recovery: flip the node to Ready; reconcile; assert the taint is gone, `phase == Ready`,
  `faultSignal` is nil.
- Idempotency: with a not-ready node at steady state, reconcile again; assert the node has exactly
  one unhealthy taint (no duplicate) and the NodeHealth resourceVersion is unchanged.
- Other-taint safety: pre-put an unrelated taint on the node; quarantine then recover; assert the
  unrelated taint survives throughout.
- Finalizer cleanup: not-ready node (tainted), delete the NodeHealth, reconcile; assert the node's
  unhealthy taint is removed and the NodeHealth is gone.

The M2 cases (Pending when absent, Ready path, drift recovery, map func) remain.

## Scope / branch

This sub-project covers NodeHealth enforcement only. GPUQuotaPolicy quota sync is the second M3
sub-project, brainstormed and built separately into the same milestone branch.

Branching (per the per-milestone convention): create `milestone/m3-enforcement` off `main`, work on
`feat/m3-nodehealth-enforcement`, PR into `milestone/m3-enforcement`. The quota-sync sub-project PRs
into the same milestone branch; a final integration PR merges it into `main`.