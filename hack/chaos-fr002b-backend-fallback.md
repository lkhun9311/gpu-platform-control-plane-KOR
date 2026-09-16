# FR-002b: the head backend is removed and a second one absorbs the traffic

Scripted in [`chaos-fr002b-backend-fallback.sh`](chaos-fr002b-backend-fallback.sh). No GPU.

## Why this is separate from FR-002

[`chaos-fr002-serving-pod-killed.md`](chaos-fr002-serving-pod-killed.md) kills the pod behind the only
backend, so every failure reaches the client and `backend_fallbacks_total` stays at 0. That reading is
correct for that run, and it leaves the retry path **completely unexercised** — a counter that exists and
reads zero proves nothing about whether it can move.

## The first attempt at this measured nothing

Scaling the backend Deployment to 2 replicas looked like the obvious way to give the gateway an alternative.
It is not. A **backend here is an `InferenceDeployment`, not a pod**: `backendsFor` lists every
InferenceDeployment serving the model, sorted oldest-first, and `tryBackends` walks that list. Extra pods
are Service load balancing, one layer below the thing under test — the request would never reach the retry
path at all.

Two InferenceDeployments serving the same model name are what create the pair.

## The head is removed by scaling to zero, not by deleting a pod

A deleted pod races its replacement, so a fallback might or might not be needed on any given request. A head
with no endpoints fails deterministically, every request, until it is scaled back — which is what makes the
counter delta readable.

## Both halves of the verdict are required

| checked | why alone it is not enough |
|---|---|
| requests still succeed | a gateway that never actually lost the head would also pass |
| the fallback counter moved | a gateway falling back on **every** request, including healthy ones, would also pass |

## Observed evidence

```json
{
  "experiment": "FR-002b head backend removed, spare must absorb",
  "head": "serving/stub-llm", "spare": "serving/stub-llm-spare",
  "injection": "scale the head InferenceDeployment to zero replicas",
  "steadyStateEstablished": true,
  "headEndpointsGoneMs": 67.3,
  "requests": { "sent": 20, "succeeded": 20, "failed": 0 },
  "backendFallbacks": { "before": 0, "after": 20, "delta": 20 },
  "verdict": { "stillServed": true, "counterMoved": true }
}
```

Twenty requests, twenty successes, and a counter delta of exactly twenty. The retry path engages, and the
metric an operator would rely on records it.

The steady state also asserts what makes the two objects alternatives at all: both have endpoints, both
serve the same model name, and the head really is the older of the two — oldest-first is the routing order,
so an experiment that removed the *younger* one would be removing the spare and measuring nothing.

## What this evidence depended on

At the time this run was taken, `backend_fallbacks_total` could be incremented for a request that was never
retried. The failure callback set the fallback flag before the guards inside `tryBackends` decided whether a
retry was possible, so a cancelled request recorded a fallback that did not happen — and, because the status
recorder still held its seeded 200, was also published as a success.

**This run is unaffected**, and the reason is checkable rather than hopeful: all twenty requests returned
200 from the spare, none were cancelled, and the delta was exactly twenty against twenty successes. The
defective path needs a cancellation, and there was none.

It is recorded here anyway, because "the counter moved by 20" is only evidence if the counter could not have
moved for another reason. `tryBackends` now reports whether it actually advanced to another candidate rather
than leaving the caller to infer it, so a later reader does not have to reconstruct that argument.

## Together with FR-002

| run | backends | outcome | `backend_fallbacks_total` |
|---|---|---|---|
| FR-002 | one | 9 of 10 requests failed | 0 — correct, nothing to fall back to |
| FR-002b | two | 20 of 20 succeeded | +20 |

The pair is what makes either number mean something: the first shows failures reaching the client when there
is no alternative, the second shows them absorbed when there is, and the counter separates the two cases
exactly as its help text claims.
