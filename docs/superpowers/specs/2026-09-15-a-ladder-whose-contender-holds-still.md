# A ladder whose contender holds still — pre-registration DRAFT

**Status: DRAFT, written 2026-09-15. Not approved for purchase.** No card time may be bought against this page until the user approves it, and from the moment its first cell is bought nothing on it may be edited except to record what happened.

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

## What was decided before any data

The arrival model, the contender rate and why 0.289 rather than another rate in the 139-offer range, that the premium rates are the down ladder's so the comparison changes one thing, the measured offer counts, that `PREMIUM_WEIGHT` and `PROBE_WEIGHT` are fixed at 1 and 0, and that this study does not stand in for the down ladder's owed repetition.
