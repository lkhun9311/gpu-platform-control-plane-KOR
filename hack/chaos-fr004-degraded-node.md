# FR-004 kind chaos: a node degrades and the platform stops scheduling onto it

This procedure runs the FR-004 failure scenario from
[`docs/06_OBSERVABILITY_BENCHMARK_FAILURE.md`](../docs/06_OBSERVABILITY_BENCHMARK_FAILURE.md), which has
been listed as kind-feasible since that document was written and had never been run. No real GPU and no AWS.

The whole run is scripted in [`chaos-fr004-degraded-node.sh`](chaos-fr004-degraded-node.sh); this document
explains what it does, records the observed evidence, and is explicit about which number belongs to this
platform and which does not.

> **Three defects were found by running this.** Two produced plausible numbers; the third left the cluster
> degraded and would have invalidated every later run. They are
> written up below rather than quietly fixed, because the second one — a controller reaction time that had
> never been measured — is the kind of figure that reads well on a resume and would not have survived a
> single sceptical question.

## The fault is injected by stopping kubelet, not by editing status

Writing `NotReady` into the Node's status directly is easier and measures nothing: the live kubelet answers
within a heartbeat, so the experiment would be timing how quickly the injection was overwritten. Stopping
kubelet makes the apiserver arrive at `NotReady` the way it does in production — by the heartbeat going
stale.

## Two latencies, and only one of them is this platform's

```
inject ─────────────────► NotReady ─────────────────► taint applied
        Kubernetes'                    the NodeHealth
   node-monitor-grace-period            controller
```

Reporting the total would credit this platform with Kubernetes' detection window, which is roughly three
orders of magnitude larger than the operator's own reaction. Anyone who knows the kubelet heartbeat would
hear "we quarantined the node in 35 seconds" and stop believing the rest of the presentation.

## The steady state is a gate, not a preamble

Six preconditions abort the run rather than warn:

| precondition | what its absence would mean |
|---|---|
| the node is `Ready` | there is no degradation left to inject |
| the node carries no unhealthy taint | a taint seen later would not be a reaction |
| the node advertises GPU capacity | a previous run left the device plugin unregistered, so the node is Ready but unusable |
| the operator has a ready replica | nothing would apply a taint |
| **a canary pod is running on the node** | "no pod landed there afterwards" is equally explained by the node never having been a candidate |
| `NodeHealth` reached `Ready` | the controller is not tracking this node at all |

The canary row is the one usually left out of chaos runs, and it is what makes the avoidance result mean
anything. The GPU-capacity row was added after defect 3 below, and exists because a previous run of THIS
script could put the cluster into the state it rejects.

## Observed evidence

kind `platform` (3 nodes, v1.31.0), operator deployed from `config/webhook-enabled`, target
`platform-worker2`. Three runs of the corrected harness agree: detection landed at 34.6 s, 35.9 s and 36.9 s,
and the operator's own reaction was below the harness resolution every time.

```json
{
  "experiment": "FR-004 degraded node",
  "node": "platform-worker2",
  "injection": "systemctl stop kubelet",
  "steadyStateEstablished": true,
  "harnessResolutionMs": { "injection": 95.9, "recovery": 83.6 },
  "latenciesMs": {
    "injectToNotReady": 34588.9,
    "notReadyToTaint": "below the harness resolution of 95.9ms",
    "recoverToReady": 267.6,
    "readyToUntaint": "below the harness resolution of 83.6ms"
  },
  "attribution": {
    "injectToNotReady": "kubernetes node-monitor-grace-period, NOT this platform",
    "notReadyToTaint": "this platform's NodeHealth controller, bounded above by harnessResolutionMs"
  },
  "quarantinePhase": "Quarantine",
  "faultSignal": "node-not-ready",
  "schedulingAvoided": "true"
}
```

What this establishes:

- the operator moved `NodeHealth` to `Quarantine` with `faultSignal: node-not-ready`
- the unhealthy taint was applied, and a pod created afterwards landed on `platform-worker` instead
- restarting kubelet returned the node to `Ready` and the taint was withdrawn without intervention

What it does **not** establish: a reaction time. Both of the operator's transitions completed faster than
this harness can observe, so they are reported as bounds. That is the honest ceiling of a shell script
polling an apiserver, and closing it would mean instrumenting the controller itself.

## Instrument defect 1 — the units were wrong by six orders of magnitude

`date +%s%3N` was assumed to truncate the nanosecond field to three digits. This system's `date` is
[uutils coreutils](https://github.com/uutils/coreutils), where `%3N` emits the full nine. A helper named
`now_ms` therefore returned nanoseconds, and because the *name* asserted milliseconds, every call site
labelled its result `ms`.

The first run reported a controller reaction of **30,646,819 ms** — eight and a half hours — and a detection
window of **466 days**.

This one is harmless because it is absurd. The interesting property is the shape: a name claimed a contract
the code never met, and the callers trusted the name.

## Instrument defect 2 — a believable number that had never been measured

The first version polled for `NotReady` to completion, and only then began looking for the taint. By the
time it looked, the taint was always already there. It reported the difference — **30.6 ms** — as the
controller's reaction time.

That figure is the round-trip cost of one `kubectl get`. The controller reacted somewhere inside a
one-second polling window that the harness could not resolve. The number is plausible, quotable, and was
never measured.

Both transitions are now watched in a single 50 ms loop that computes its own per-observation cost, and any
interval at or under that cost is reported as a bound instead of a number. **Refusing to quantify is the
correct output when the instrument cannot resolve the thing** — the same rule this repository's conventions
already state for `internal/queuelab` and `internal/bench`.

## The record caught the half-finished correction

The fix was applied to the injection path and not the recovery path. The next run's record said:

```json
"harnessResolutionMs": 95.9,
"readyToUntaint": 37.9
```

A measurement smaller than the resolution that produced it. Nothing failed; the document simply contradicted
itself in a way a reader could see, because **the resolution is recorded alongside the measurements**. Had
the harness reported only latencies, `37.9ms` would have looked like the best number in the run.

This is the same principle as the validity gates on the queuelab run records: persist the fields a verdict
derives from, and the artifact can be checked instead of trusted.

## Defect 3 — the experiment degraded the cluster it ran on

Restarting kubelet was not enough to restore the node. A device plugin registers with kubelet over a socket,
and that registration does not survive the restart: the node returned `Ready` while advertising
`nvidia.com/gpu: 0`. The first version stopped at "kubelet is running again".

The trap is quiet, because the next run would have passed. Its canary requests no GPU, so it still schedules;
the steady state still holds; `schedulingAvoided: true` still comes out. The experiment would have gone green
against a node no real workload could land on.

This is a different class from the first two. Those made the numbers wrong. This one makes the experiment
**not repeatable** — the second run does not start from the same conditions as the first, which is the one
property a chaos experiment cannot do without.

Closed from both directions, because either alone is insufficient: restore now recreates the plugin pod and
waits for capacity to actually return (warning loudly if it does not), and the steady state refuses to start
on a node advertising no GPU. Restoration can fail, so the gate is needed; the gate cannot repair an already
degraded cluster, so the restoration is needed.

Confirmed on the following run, which ended with `GPU capacity restored on platform-worker2 (gpu=2)` and left
no `NodeHealth` behind.

## Running it

```bash
# Requires: kind cluster `platform`, operator deployed, kubectl context set.
OUT=./ex/chaos-fr004.json ./hack/chaos-fr004-degraded-node.sh
```

`platform-worker2` is the default target because `platform-worker` is pinned by the queuelab runs and the
only workloads on worker2 are DaemonSets. kubelet is restarted on exit, including on failure or interrupt.
