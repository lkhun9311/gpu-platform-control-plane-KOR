# M5-b — C-only pilot: what it measures, and when to stop

Date: 2026-09-04 · Pre-registered **before** the pilot is bought. Nothing here may be edited after the run.

## Why this exists

Three paid runs have produced no valid measurement. The last one completed all sixteen replays and
returned `C/R1 = 83.747` — the guard held the premium tail at 84 times its uncontended baseline — while
refusing 15.3% of eligible traffic. So the guard was doing something and it did not help.

Two cold reviews reached the same conclusion independently: **the evidence cannot say why**, and fixing
arm B and running the full four-arm experiment again would be paying to find out whether the thing is
possible at all. That is a tuning experiment wearing a confirmatory one's clothes.

This pilot asks the one question everything else depends on, at the smallest price that can answer it.

## The question

> At the pre-registered load, does the KV-aware guard observe the pressure it is designed to observe, act
> on it when it appears, and does the premium tail improve as a result?

Not "does it pass the checks". The checks compare arms; this asks whether there is anything to compare.

## Design

Two conditions, same trace, same engine, alternating blocks:

| condition | admission mode | purpose |
| --- | --- | --- |
| **R1** | off, premium-only traffic | uncontended baseline |
| **C** | kv-aware, premium + noisy | the guard under the load it was built for |

`off` and `static-cap` are not run. `off` is only needed to show the contention problem, which the last
run already showed at 10,595 ms; `static-cap` only exists to make `C/B` interpretable, and `C/B` is not
asked here.

Blocks alternate `R1, C` and `C, R1` so position is not confounded with condition, with the observed
washout between them. Repetitions: 3 per condition.

## What the evidence must contain

Every request already carries: the tier the gateway resolved, why it was admitted or refused, the engine's
own input-token count, and — for the C arm — the cache usage, queue depth, engaged flag and freshness the
decision was made from.

## Pre-registered readings

Evaluated in this order. The first one that fires is the answer.

### 1. The load did not create the condition — INCONCLUSIVE, do not spend again on this design

If, across C's replays, the 95th percentile of observed `kv` never reaches the engage threshold and
`waiting` stays at zero for more than 90% of decisions, then the trace does not pressure this engine.

The guard cannot be evaluated by traffic that never makes the backend struggle. The fault is the load
generator's, not the guard's, and the next step is a heavier trace — not a different guard.

### 2. The guard could not see — INVALID, fix and re-run the pilot

If more than 1% of C's eligible decisions carry `fresh=0`, or any carry `backend_unregistered`, the guard
was bypassing itself. This is a defect, the run measures nothing, and it is cheap to fix.

### 3. The guard saw pressure and did not engage — FALSIFIED AT THIS THRESHOLD

If `kv` exceeds the engage threshold on more than 5% of decisions while `engaged=0`, the state machine's
hysteresis is too slow for this workload.

**The threshold is not retuned.** It was pre-registered; moving it after seeing the data is exactly the
post-hoc tuning the control arm exists to rule out. This outcome is recorded as a limitation of the
design's chosen constants and the milestone ends.

### 4. The guard engaged, shed load, and the tail did not recover — HYPOTHESIS FALSIFIED, stop

If the guard engaged on more than 5% of decisions, refused eligible requests while engaged, and C's
premium TTFT p99 is still above **3 × R1**, then admission control on this signal does not protect the
tail at this load.

**This is a result and it is written up as one.** No retuning of thresholds, hysteresis, or the eligible
population. M5-b's contribution becomes the negative finding and the harness that established it.

### 5. The tail recovered — PROCEED

If C's premium TTFT p99 is at or below **3 × R1**, the mechanism works well enough to be worth the
four-arm confirmatory run, and arm B's frozen tuning is already in place for it.

## Why 3× and not 1.25×

1.25× is the pre-registered success criterion for the confirmatory run and stays that way. This pilot is
not judging success; it is deciding whether the confirmatory run can produce anything. A guard that pulls
84× down to 3× has a mechanism worth measuring properly. One that leaves it above 3× does not, and no
amount of arm-B correctness changes that.

## Cost and stopping

One GPU node, two conditions, three repetitions. Roughly a third of a four-arm run. If any reading from
1 to 4 fires, **no further paid M5-b run is bought** without a written change to this document, agreed
before the spend.

## The no-retuning rule, agreed before the spend

Explicitly confirmed on 2026-09-04, before any of this was bought.

Whichever of readings 1 to 4 fires, **no threshold, hysteresis constant, eligible-population definition, or
load parameter is changed and re-run**. The reading is written up as it stands.

This is the whole point of writing them down first. Without it a pilot that comes back negative becomes a
starting point for adjustments, and adjusting until it passes produces a number that means nothing -- the
failure this document exists to prevent, and one that costs money on every iteration.

Changing any constant after this point requires a new pre-registration document, agreed before the next
spend, that says what changed and why the previous reading no longer applies.

## What is not in scope

Retuning anything on this data. The point of writing the readings down first is that the pilot can come
back negative, and a negative pilot is a result rather than a starting point for adjustments.
