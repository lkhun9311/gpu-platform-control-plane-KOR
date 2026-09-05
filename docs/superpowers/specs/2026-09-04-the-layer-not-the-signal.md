# The guard failed on the wrong axis: it was the layer, not the signal

Date: 2026-09-04 · Analysis of evidence already bought. **No GPU time was spent on any of this.**

M5-b's guard was reported as badly aimed — watching an occupancy signal that moves after the damage. That is
true and it is not the whole finding. Re-analysing the same raw rows shows two things the earlier reading
missed, and the second one changes what a successor can hope for.

## 1. The KV condition never fired, and could not have

The guard engages when KV-cache occupancy crosses 0.85 **or** the waiting queue exceeds 8. Its 274 refusals
have been read as evidence that the occupancy signal was late. They are not: the occupancy condition was
unreachable by construction.

The engine reported a KV cache of **386,912 tokens**. A noisy prompt is 7,695. Reaching 0.85 therefore needs
about **43 concurrent long requests**. The most this trace ever had in flight at once was **13**, which is
**25.9%** of the cache.

Every one of the 274 refusals came from the waiting-queue condition. **The signal the milestone was named
for was never once consulted in anger.**

That is a stronger statement than "the signal is late", and it is measured rather than argued.

## 2. A signal that separates perfectly, computable at the gateway

Count the long requests that have been admitted but have not yet produced a first token. Call it `k_pre`.
The gateway can compute it from the streams it is already proxying — no engine scrape, no metric endpoint,
no staleness window.

Premium requests in the `off` arm, bucketed by `k_pre` at the moment they were sent, against the
pre-registered bar of 1.25 x R1 = 102.8 ms:

| k_pre | n | p50 (ms) | p99 (ms) | max (ms) | over the bar |
|---:|---:|---:|---:|---:|---:|
| 0 | 530 | 54.4 | 83.7 | 95.9 | 0.0% |
| 1 | 418 | 603.6 | 1043.6 | 1051.9 | 96.7% |
| 2 | 344 | 1503.8 | 2057.8 | 2076.2 | 100.0% |
| 3+ | 548 | 4307.5 | 10879.9 | 11502.5 | 100.0% |

At `k_pre = 0`, **530 of 530 premium requests cleared the bar**, with a worst case of 95.9 ms — under
decode load, with the engine busy. At `k_pre = 1` almost none do. The separation is not statistical; it is
categorical.

This is what the guard should have watched. It is also the signal codex's literature review arrived at
independently, from theory: occupancy describes state that already exists, while a 7,695-token prompt that
just arrived is work that has been admitted and not yet materialised.

## 3. And it still would not have been enough

This is the part that changes the milestone's ending.

An FCFS prefill-queue counterfactual — calibrated at the measured uncontended prefill rate of 9,790 tok/s,
premium baselines taken per request from R1, validated within 7% at the tail against the `off` arm, the
`static-cap` arm, and a replay of the guard's own admit/refuse decisions — gives the frontier an ORACLE
gateway could reach:

| policy | premium p99 | admitted work |
|---|---:|---:|
| cap at 1 concurrent long | 12.5 x R1 | 58% |
| cap at 2 | 23 x R1 | 81% |
| random 5% admission | 9.1 x R1 | — |

Meeting the pre-registered 1.25 x requires admitting **about 1.4% of the eligible work**. One admitted long
request opens a damage window of roughly 1.02 seconds, and the entire p99 budget is 102.8 ms. A gateway that
refuses at exactly the right moments still cannot make a 1.02-second window smaller than a 0.10-second
budget.

**The frontier is a cliff, not a slope.** "The guard watched the wrong signal" implies a better signal would
have rescued it. At this load, on this engine, no signal would.

Two smaller results fall out of the same model. A `k_pre` cap of 1 would have produced a premium p99 of
about **1,029 ms against the guard's measured 6,882** at comparable or less shed work — a tenfold
improvement that still misses the bar tenfold. And the shipped guard underperformed **random** 85% admission
(7,351 ms against 5,900): its refusals were anti-correlated with harm.

## What this leaves

The milestone's registered claim is withdrawn, and now for a reason that generalises past the choice of
signal: **at this load the lever is not at the admission layer.** The microtest already showed the engine
has one, and the literature says that lever is the well-trodden Sarathi/priority-scheduling ground rather
than a contribution.

What the gateway can still do is narrower and real. vLLM takes `priority` as a **client-asserted field**;
a tenant can name its own. This repository's gateway already resolves tier from an authenticated identity
and a `GPUQuotaPolicy` annotation, and `internal/gateway/admission.go` states in its own comment that tier
"is a property of the tenant's contract rather than something a caller can assert about itself". Binding the
engine's priority to that is a trust boundary the engine cannot enforce alone.

## The limits of this page

Everything here is a counterfactual over one trace on one card, and the queue model is a model. It was
validated against three independent arms of the run it is modelling, which is the strongest check available
without buying more time, and it is not a substitute for measuring. The frontier numbers should be read as
"an oracle is not close" rather than as precise values.
