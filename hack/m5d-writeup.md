# M5-d: the write-up, with the numbers filled in — and the claim withdrawn

Every measured figure this milestone will carry comes from a run that has not happened. This page is the
half that does not depend on them — what is being measured, why it is measured that way, and what the
result will and will not support — written **before** the run rather than after, so the reasoning cannot be
fitted to whatever the card produces.

Every number below is now filled in from the run of 2026-09-03 and the microtest of 2026-09-04. Until then each one was a
doubled-bracket marker naming the figure it was waiting for, on the rule that a marker still present when
this page claimed to be finished was a bug -- the same rule `internal/bench` applies when it refuses a
report whose arms are not comparable. A test enforces it, and it caught this sentence when the sentence
still spelled a marker out as an example.

**The reasoning above the numbers has not been edited to fit them.** The pre-registered checks are quoted as
they were written, and all three failed. What changed is a section at the end recording what the failure
turned out to be, and the withdrawal of the claim this milestone was built to make.

## What is being measured

**Primary endpoint: premium-tenant TTFT p99**, over the premium tenant's completed requests only. The
contender never enters it. That is not tidiness — a guard that improves the tail by rejecting the tenant
whose long prefills dominate it would otherwise be measuring its own threshold.

Four arms over one recorded open-loop trace:

| arm | what it is | what it answers |
|---|---|---|
| R1 | premium alone, same arrival schedule | how fast is this when nobody is competing |
| off | both tenants, no admission control | what does contention cost |
| static-cap | pressure-blind token bucket, tuned to admit the same work as kv-aware | is the guard doing anything a rate limit could not |
| kv-aware | the KV-cache-aware guard | the arm under test |

`static-cap` is the arm that makes the result falsifiable. Without it, "the guard lowered p99" is
indistinguishable from "the guard shed load", and shedding load lowers p99 under any policy whatsoever.

## Pre-registered checks

Declared before the run, in `internal/bench.EvaluateChecks`:

- **absolute protection**: `kv-aware p99 / R1 p99 <= 1.25`
- **incremental value**: `kv-aware p99 / static-cap p99 <= 0.90`, with the bootstrap CI's upper bound below 1.0
- **admission match**: the two contended arms' admitted-work fractions within the frozen tolerance

A run is **disqualified before any check is read** if any compared arm completed no premium requests, has a
censored tail (more than 1% premium timeouts), or has fewer than `MinTailSamples` completions. That last
bound is derived rather than chosen: the percentile is nearest-rank, `ceil(0.99n)-1` clamps to `n-1` for
every `n` below 100, so under it the reported p99 *is* the slowest request.

## The numbers

Run of 2026-09-03. Four arms, four repetitions each, one trace, 1,840 premium completions per arm.

| | R1 | off | static-cap | kv-aware |
|---|---|---|---|---|
| TTFT p99 (ms) | 82.2 | 10,595.3 | 81.2 | **6,882.0** |
| TTFT p50 (ms) | 47.3 | 940.6 | 48.1 | 499.8 |
| premium completions | 1,840 | 1,840 | 1,840 | 1,840 |
| requests shed | — | 0 | 1,788 (413) | 274 (429) |
| admitted-work fraction | — | 100.0% | 0.0% | 84.7% |

Checks: absolute **FAIL** (C/R1 = 83.747 against ≤ 1.25) · incremental **FAIL** (C/B = 84.760,
CI [80.670, 88.211], against ≤ 0.90 with the interval below 1.0) · admission match **FAIL**
(|B−C|/C = 1.000 against ≤ 0.05).

Two of those three are void rather than failed, and the report says so. Arm B was configured with the
trace's request rate in a flag measured in tokens per second and no burst at all, so every eligible request
exceeded a bucket that could never hold it and B admitted nothing. B was therefore not a control; it was a
second isolation arm, which is why C/B came out equal to C/R1. Only the absolute check compares what it was
meant to compare, and it fails by a factor of 67.

Threshold probes, four characters apart: **not evaluated**. All 188 requests at est=4095 and all 180 at
est=4096 returned 403 — the two probe tenants had API keys but no `GPUQuotaPolicy`, so the gateway refused
them before admission control ran and the threshold never judged them. The report prints VOID for this
section rather than the `rejected=0` it used to.

Sharing matrix (M5-c): **not run.**

## Calibration, which contradicts the design spec

Measured against Qwen2.5's tokenizer and chat template
(`internal/bench/testdata/tokenizer_calibration.json`), the gateway's `ceil(len/4)` estimator is **not**
conservative, and the error changes sign with length:

| prompt | estimated | measured | |
|---|---|---|---|
| 200 chars | 50 | 68 | 36% **under** |
| 16,384 chars | 4,096 | 3,171 | |
| 40,000 chars | 10,000 | 7,695 | 30% over |

The chat template costs a fixed ~25 tokens, which dominates a short prompt. A request the guard scores at
exactly its 4,096 threshold carries 3,171 real tokens, so a 925-token band is rejected on an over-estimate.
Reported rather than corrected: the threshold was pre-registered, and moving it after measuring is the
post-hoc tuning `static-cap` exists to rule out. The probe tenants exist so the band is exercised rather
than merely known.

## What the result was, and what replaced the claim

The guard did not protect the tail. It also was not idle: it refused 274 of 1,788 eligible requests, 15.3
percent, so the decision path ran. Reading the evidence rather than the verdict established why, without
buying another run.

**The tail rises with the number of long prefills in flight when a premium request arrives.** With none, the
premium p99 is 85.0 ms against an isolated 82.2 — the same latency. The increments from zero to seven
concurrent are roughly 552, 896, 934, 867, 1194, 1305 and 858 ms, a rising staircase with a linear slope
near 977 ms per concurrent prefill. Every millisecond of the tail is time spent behind an admitted long
request.

**The guard's refusals landed where the damage was not.** 84 percent of them fired while no long prefill was
running at all, a median of 16.9 seconds after the last one had produced its first token. Its engage
threshold is a KV-cache occupancy of 0.85, which this workload needs about 42 concurrent requests to reach;
the damage starts at one. The signal is downstream of the harm and the instrument's time constants are
longer than the load's.

**A two-request microtest on one Spot instance, at about $0.40, showed the layer below has a lever.** With
the engine's batch budget at 512 tokens and its scheduling policy set to priority, a short request behind a
long one returned in 1.57x its uncontended time instead of 13.79x, and the long request's own first-token
time was unchanged at 684 ms against 685. Neither setting alone did it: at a budget of 8192 the prompt fills
one scheduler step and priority has no boundary to enter at (11.83x against 11.81x), and under first-come
ordering no budget helped (12–14x throughout).

That microtest's own pre-registration did not cover its result — priority cleared the band at one budget of
three, and the readings had been written as though budget and policy were independent. It is recorded as no
reading having fired, not as the nearest reading winning.

**So arm C's claim is withdrawn.** A reactive occupancy signal did not protect premium TTFT at this load,
and a configuration below the control plane did. What a gateway adds on top of a correctly configured engine
is a different question, and this milestone does not answer it.

## What the result will not support

- **Anything about a rate the card cannot serve.** The harness default of 20/s demands 3.8x an A10G's
  theoretical fp16 peak; the run's rate is derived from a measured prefill on the card itself.
- **A comparison across milestones on different silicon.** M5-b and M5-c share one card class, one model
  and one dtype, and a contract test enforces the last two.
- **Per-engine GPU utilisation under sharing.** Under time-slicing and MPS a busy SM belongs to no single
  Pod; the matrix reports what clients observed and the sharing node deliberately runs no observer.
- **Anything about cold starts.** Every arm serves a warm engine.
- **A general claim that admission control cannot protect a tail.** This falsifies one signal — reactive KV
  occupancy with a 0.85 engage threshold and a 30-second release — on one workload, one model and one card.
- **A general claim that the engine settings fix it.** The microtest measured one long request and one short
  one, at a median over ten repetitions per cell. The load here is an arrival process with many concurrent
  prefills and a p99.

## Provenance

- Engine: `config/vllm/deployment.yaml`, digest-pinned, sized in `hack/m5b-vllm-sizing.md`.
- Fixtures: real captures from that image, not written from documentation
  (`internal/bench/testdata/PROVENANCE.txt`).
- The chain has carried a request end to end against a real vLLM on CPU, returning a
  `kv_cache_pressure` rejection (`hack/m5b-chain-live-evidence.log`). The KV-usage arm of the engage
  condition is the half that needs the card.
- Runbooks: `hack/m5b-gpu-session.sh`, `hack/m5b-arms.sh`, `hack/m5c-matrix.sh`,
  `hack/m5b-scheduler-microtest.sh`.
- Raw evidence: `hack/m5b-run-20260903-091356/` for the arms,
  `docs/superpowers/specs/data/2026-09-04-scheduler-microtest-results.json` for the microtest.
- The reasoning that produced these conclusions, including the two hypotheses of mine that adversarial
  review falsified: `docs/superpowers/specs/2026-09-04-scheduler-microtest.md` (pre-registered before the
  spend) and `2026-09-04-scheduler-microtest-result.md`.

## What this page cost

Three paid sessions produced the arms, at roughly ten dollars of GPU and cluster time between them and no
valid measurement on the first two. Everything after that — the mechanism, the staircase, the phase lag, the
falsification of two of my own explanations — came out of evidence already bought. The microtest that found
the lever cost about $0.40, because the question had been narrowed to one that two requests could answer and
because it ran on a single instance in a public subnet where inbound transfer is free.
