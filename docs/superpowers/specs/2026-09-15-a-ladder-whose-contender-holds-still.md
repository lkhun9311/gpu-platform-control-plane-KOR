# A ladder whose contender holds still — pre-registration

**Status: approved by the user on 2026-09-16. Written 2026-09-15, and no card time has been bought against it yet.** From the moment its first cell is bought, nothing on this page may be edited except to record what happened.

This page registers `throughput-ladder-independent-2026-09-15`. It changes **only how the traces are generated**. The question, the criterion, the arms, the readings, the counterbalanced order, the repetition rule, the stopping rule and the rungs' premium rates are those of `2026-09-13-the-ladder-has-to-search-downward.md`, unchanged, and the instrument that evaluates them is the same code.

## Why this exists

The down ladder held the contender at 139 offers by solving a contender **weight** for every rung. Under the weighted arrival model every gap is drawn at the total rate and each arrival's tenant from the same stream, so changing the premium rate redraws the contender's arrival times too.

**The count held and the schedule did not.** Each rung faced a different contender schedule, so a rung-to-rung comparison moved the premium rate and the contention it was measured against at once. The down ladder's own journal named this as the thing to change if the ladder were bought again: the trace, not the rungs.

Under independent arrivals each tenant is its own Poisson process on a stream keyed by its name. The contender's rows are **byte-identical at every rung**, and that is pinned by tests rather than asserted here: `internal/bench/trace_test.go` and `cmd/benchharness/gen_trace_test.go`.

## What it takes from the pages above it, and what it may not

**May:** the 139.0 ms target and where it comes from; the model, engine settings, cache budgets and trace shape other than the arrival model; the harness and readings; the three premium rates; and the down ladder's measured result as the comparison this study exists to make.

**May not:** any capacity number, because this study has measured none; and the target itself.

## The question, unchanged

> **How much premium load can each topology sustain while still meeting the premium latency target, and how much of that does the split cost?**

## The rungs

`LADDER` entries are `PREMIUM_RATE:NOISY_RATE` for this study. `hack/m5c-matrix.sh` reads the arrival model from the registry, and `gen-trace` refuses weighted flags for this study.

The contender rate was solved offline against the real `gen-trace` at `--seed 11` and `DURATION_MS=505000`: scanning 0.260 to 0.320 req/s in steps of 0.0005, every rate tried from 0.289 to 0.294 gave exactly 139 offers and no other did, and **0.289** is used. The counts below are **measured by the matrix's own plan check (`PLAN_ONLY`), not predicted**:

| rung | `PREMIUM_RATE` | `NOISY_RATE` | premium offers | contender offers |
| ---: | ---: | ---: | ---: | ---: |
| 1    | 1.16           | 0.289        | 620            | 139              |
| 2    | 2.31           | 0.289        | 1,238          | 139              |
| 3    | 4.61           | 0.289        | 2,435          | 139              |

`PREMIUM_WEIGHT=1` and `PROBE_WEIGHT=0` are still passed, and the matrix refuses any other values for this study, because they describe a weighted mix that would be ignored. The isolated baseline at rung 3 carries 2,435 premium rows and no contender rows.

The premium offers differ from the down ladder's (585, 1,164, 2,327) because the premium arrivals are drawn from a different stream. Every rung stays above the 500-completion sample floor.

## Order, repetitions, stopping rule — unchanged

Odd rungs run `shared → timeSlicing`, even rungs the reverse. One repetition per cell, and a rung landing within a tenth of the target is repeated. Climb until **both** topologies breach; buy the single `R1` cell at the rung the ladder ends on.

## What each result would mean

The capacity statements are the down ladder's table, unchanged. What this study adds is the comparison with the down ladder, rung by rung:

| against the down ladder | what it says |
| --- | --- |
| each topology meets and breaches at the same rungs | the moving contender schedule did not change the bracket at these three loads |
| a topology meets or breaches at a different rung | the down ladder's bracket depended on the contender schedule it happened to draw, and its capacity statement has to be read with that |
| the ladder is invalid at a rung the down ladder scored | an instrument difference, to be diagnosed before either result is compared |

**This study does not discharge the repetition the down ladder owes its rungs 2 and 3.** Those cells are repeated on that study's traces, and a cell here is a different trace.

## What this run will not be able to say

- **Whether the contender's schedule or the premium stream moved a result.** Both differ from the down ladder: the contender is now fixed across rungs, and the premium arrivals are a different draw.
- **Anything below about 1 req/s**, for the down ladder's sample-floor reason.
- **Maximum throughput, or sustained stability**, for the reasons the down ladder gives.

## Budget

| line               | cost        | note                                                                                  |
| ------------------ | ----------: | ------------------------------------------------------------------------------------- |
| up to seven cells  | ~$1.10      | scaled from the down ladder's **actual** $1.07 for seven cells, not from its estimate |
| **total**          | **~$1.10**  | one session; less if the stopping rule fires early                                    |

## THE ANSWER

Added 2026-09-16 after the run. Nothing above this line has been edited since the first cell was bought.

```
ANSWER: L1 -- the topologies sustain different loads
```

**The bracket the down ladder found does not move when the contender's schedule is held still.** Every rung's contender was byte-identical to every other rung's, which is what this study changed, and each topology met and breached at exactly the rungs it did under the weighted model.

| rung | premium offered | `shared` premium TTFT p99 | `timeSlicing` premium TTFT p99 | down ladder's `timeSlicing` |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 1.16 req/s | 1,569.0 ms **BREACH** | **123.7 ms met** | 123.8 ms met |
| 2 | 2.31 req/s | 1,744.9 ms **BREACH** | **130.3 ms met** | 130.4 ms met |
| 3 | 4.61 req/s | 1,727.2 ms **BREACH** | 142.4 ms **BREACH** | 143.2 ms BREACH |

`timeSlicing` lands within 0.8 ms of the down ladder at every rung. `shared` is 1,569 / 1,745 / 1,727 ms against 1,283 / 1,695 / 2,303 ms — the same order of magnitude and the same verdict at every rung, and **not monotone in the premium rate** in either run.

- **The contender completed 139 of 139 in every contended cell**, with **no shed and no timeout in any cell**.
- **The isolated baseline measured 62.9 ms** at 4.61 req/s with 2,435 premium rows and no contender rows, so reading L4 is correctly silent: rung 3 located a topology's limit, not this model's on this card.
- **The sustainable premium rate:** `timeSlicing` met the target at 2.31 req/s and missed it at 4.61; `shared` missed it at every rate this ladder offered.

### What is soft about this result

- **Rungs 2 and 3 owe a repetition**, by the registered rule: 130.3 ms and 142.4 ms sit 8.7 and 3.4 ms from the target. The instrument printed `rungs the pre-registration requires a repetition of: [2 3]`. This run does not discharge the down ladder's owed repetitions either, and they are not the same cells.
- **The eligibility threshold was not tested.** `PROBE_WEIGHT=0` removes the probe tenants, so any threshold in a wide range produces these same arms. The report says so itself.
- **The manifests name no build.** All seven carry `gatewaySHA: unknown`: the instance unpacks a source tarball with no `.git`, and the wrapper never passed the commit through, so `m5c-matrix.sh` fell back to the literal string. `replay --require-provenance` ran on every cell — the log carries no waiver warning — and **accepted it, because the guard only refuses an empty value.** The commit is recorded beside the evidence in `commit.txt` (`b1c1818`) and in the S3 prefix, so the run is traceable; what failed is the check that exists to make that automatic.

### What it cost

| line | value |
| --- | ---: |
| instance | g5.2xlarge spot, ap-northeast-2c, $0.678/h |
| wall clock | 89.7 min (03:27:22 → 04:57:04 UTC) |
| **actual** | **$1.01** |
| estimated on this page | ~$1.10 |

Seven cells, one repetition each, terminated by the wrapper's own exit path; no orphaned instance or volume.

## What was decided before any data

The arrival model, the contender rate and why 0.289 rather than another rate in the 139-offer range, that the premium rates are the down ladder's so the comparison changes one thing, the measured offer counts, that `PREMIUM_WEIGHT` and `PROBE_WEIGHT` are fixed at 1 and 0, and that this study does not stand in for the down ladder's owed repetition.
