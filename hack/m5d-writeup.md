# M5-d: the write-up, with the numbers left out

Every measured figure this milestone will carry comes from a run that has not happened. This page is the
half that does not depend on them — what is being measured, why it is measured that way, and what the
result will and will not support — written **before** the run rather than after, so the reasoning cannot be
fitted to whatever the card produces.

Where a number belongs, there is a marker: `[[TTFT_P99_R1]]`. A marker still present when this page claims
to be finished is a bug, and `internal/bench` refuses a report whose arms are not comparable for the same
reason.

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

| | R1 | off | static-cap | kv-aware |
|---|---|---|---|---|
| TTFT p99 (ms) | `[[TTFT_P99_R1]]` | `[[TTFT_P99_OFF]]` | `[[TTFT_P99_STATIC]]` | `[[TTFT_P99_KV]]` |
| premium completions | `[[TAIL_N_R1]]` | `[[TAIL_N_OFF]]` | `[[TAIL_N_STATIC]]` | `[[TAIL_N_KV]]` |
| 429s | — | `[[REJ_OFF]]` | `[[REJ_STATIC]]` | `[[REJ_KV]]` |
| admitted-work fraction | — | — | `[[WORK_STATIC]]` | `[[WORK_KV]]` |

Checks: absolute `[[CHECK_ABS]]` · incremental `[[CHECK_INC]]` · admission match `[[CHECK_MATCH]]`

Threshold probes, four characters apart: `[[PROBE_UNDER]]` rejected of `[[PROBE_UNDER_N]]` sent at
est=4095, `[[PROBE_OVER]]` of `[[PROBE_OVER_N]]` at est=4096.

Sharing matrix (M5-c): `[[MATRIX_SHARED]]` · `[[MATRIX_TIMESLICING]]` · `[[MATRIX_MPS]]`

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

## What the result will not support

- **Anything about a rate the card cannot serve.** The harness default of 20/s demands 3.8x an A10G's
  theoretical fp16 peak; the run's rate is derived from a measured prefill on the card itself.
- **A comparison across milestones on different silicon.** M5-b and M5-c share one card class, one model
  and one dtype, and a contract test enforces the last two.
- **Per-engine GPU utilisation under sharing.** Under time-slicing and MPS a busy SM belongs to no single
  Pod; the matrix reports what clients observed and the sharing node deliberately runs no observer.
- **Anything about cold starts.** Every arm serves a warm engine.

## Provenance

- Engine: `config/vllm/deployment.yaml`, digest-pinned, sized in `hack/m5b-vllm-sizing.md`.
- Fixtures: real captures from that image, not written from documentation
  (`internal/bench/testdata/PROVENANCE.txt`).
- The chain has carried a request end to end against a real vLLM on CPU, returning a
  `kv_cache_pressure` rejection (`hack/m5b-chain-live-evidence.log`). The KV-usage arm of the engage
  condition is the half that needs the card.
- Runbooks: `hack/m5b-gpu-session.sh`, `hack/m5b-arms.sh`, `hack/m5c-matrix.sh`.
