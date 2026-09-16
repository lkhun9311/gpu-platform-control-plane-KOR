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

### What it cost, which this page did not have when it was written

Every number above is a tail. That is half an answer, and the half that was missing is what the tails were
bought with. The report was extended to compute it from the same raw files, at no further spend:

| | R1 | off | static-cap | kv-aware |
|---|---|---|---|---|
| output tokens/s | 46.4 | **57.3** | 46.3 | 55.7 |
| premium share | 100% | 80.5% | 100% | 82.9% |
| contender share | — | 19.5% | **0%** | 17.1% |
| premium TPOT p99 (ms) | 16.0 | 266.0 | 16.0 | **209.4** |
| contender TPOT p99 (ms) | — | 267.3 | — | 265.5 |

Three things follow that the tail table cannot show.

**The unprotected arm served the most.** `off` delivered 24% more tokens per second than the arm whose tail
looked best. Reading the first table alone, `static-cap` is the result of this study.

**The stream is the harder half, and nothing passed it.** A premium tail of 82.2 ms with a TPOT p99 of
266.0 ms is a fast first token followed by a stream six times slower than isolation's. Every arm that left
the contending tenant alive misses 1.25x the isolated baseline — 20.0 ms — by more than ten times. The guard
moved premium TPOT from 266.0 to 209.4, a 21% improvement on a metric that needed 93%.

**These numbers were wrong once.** TPOT was computed over unsorted samples and its first printing reported
16.0 ms for `kv-aware`, whose real p99 is 209.4. The fingerprint was in the committed derivation: a p99 of
15.98 ms below its own p50 of 17.41. The samples are sorted now, streams that broke are excluded from the
tail because a stream killed at its deadline reports the deadline as an inter-token time, and the figures
are split by tenant because the criterion is about the protected one while the pooled figure is not.

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
threshold is a KV-cache occupancy of 0.85, which this workload needs about 43 concurrent requests to reach;
the damage starts at one.

An earlier version of this paragraph ended "the signal is downstream of the harm and the instrument's time
constants are longer than the load's". That sentence is withdrawn, and the reason is that the real finding is
both simpler and stronger. Re-analysing the same rows —
`docs/superpowers/specs/2026-09-04-the-layer-not-the-signal.md`, no card time — shows the guard engages on
occupancy **or** a waiting queue above 8, and that the occupancy limb was unreachable by construction: the
engine reported a 386,912-token cache, a noisy prompt is 7,695 tokens, and the most this trace ever had in
flight at once was 13, or 25.9 percent. **All 274 refusals came from the waiting-queue branch.** The signal
the milestone was named for was never once consulted.

"Downstream" was a story fitted to a negative result. It cannot be falsified on this evidence, because
falsifying it would need a load that reaches 0.85 and this one never approaches it — and a claim this
evidence cannot test is not a claim this evidence established. Two external reviews arrived at the same
objection independently on 2026-09-07. The accurate sentence is the one that page already uses: at this
load, the occupancy condition was unreachable.

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

## And then the engine layer was measured at load, and it does not hold either

The sentence above — "a configuration below the control plane did" — came from a microtest of two requests.
It was true of two requests and it is not true of the workload. Ten arms, three repetitions on three
instances, 12,627 requests per contended arm and no timeouts anywhere
(`docs/superpowers/specs/2026-09-08-the-load-needs-an-upper-gate.md` carries the full result):

| cell | premium tail | / isolated | premium TPOT | / isolated |
| ----------------------- | -----------: | ---------: | -----------: | ---------: |
| control, engine default | 3,435.4 ms | 51.0x | 139.1 ms | 7.6x |
| best cell, 1024/priority | **1,395.3 ms** | **20.7x** | 112.5 ms | 6.1x |
| the pre-registered bar | 134.6 ms | 2x | 22.9 ms | 1.25x |

Every one of the nine configurations scores two of the four bars, and the same two: the contending tenant
keeps its work and the machine keeps its throughput in all nine, while the tail and the stream are missed in
all nine. Scheduling policy is worth about five times on the tail at every budget, which is a real effect and
reproduces the microtest's direction. It is worth five times against a gap of twenty.

**And the miss is not a tail phenomenon.** The best cell's premium MEDIAN is 370.7 ms against a bar of
134.6, so the typical premium request — not the unlucky one percent — pays 320 ms it does not pay in
isolation. The whole distribution moves.

The arithmetic says why, and it is not subtle. A contending prompt occupies the engine for about **1.03
seconds**. The premium tail budget is **0.135 seconds**. Chunked prefill divides that second into thirty
pieces and priority lets a premium request enter at a piece boundary, which is exactly why those two knobs
move anything at all — but a premium request still waits behind the piece in flight and behind whatever the
scheduler already admitted. **Dividing the work does not divide the machine.**

So the milestone closes on a measured negative with a mechanism: at this load, on this card, protecting the
premium tail is not available at the admission layer (M5-b measured that) and not available at the engine's
scheduling layer either (this run measured that). What is left is physical separation — disaggregated
prefill and decode, an engine per tenant class, a partitioned card — and that is a different budget and a
different question. The successor's constraints are pre-registered in
`docs/superpowers/specs/2026-09-09-what-would-have-to-change.md`.

None of the five pre-registered readings fired, and that is recorded as no reading having fired rather than
as the nearest one winning — the same discipline the microtest's own result got, two sections up, and for
the same reason.

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
  prefills and a p99. That caveat has since been settled by measurement rather than left standing: the
  section above ran the same knobs at load and they do not fix it.
- **A general claim that no configuration can protect a tail.** What the confirmatory run establishes is
  narrower and is the whole of what it establishes: two knobs, one model, one card, one arrival trace, and a
  contention unit about eight times the budget meant to survive it. A load whose contending prompts were
  short enough, or a budget loose enough, is a different arithmetic and this run says nothing about it.

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
