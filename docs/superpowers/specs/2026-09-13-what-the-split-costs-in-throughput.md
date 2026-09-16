# What the split costs in throughput — pre-registration

**Written 2026-09-13, after the ninth pilot answered the other half of the question and before any card time
was bought for this one.** Nothing on this page may be edited from the moment the first cell of this ladder
is bought, except to record what happened.

This page amends `2026-09-10-does-splitting-the-card-buy-protection.md` in the open. It does not change that
page's bars, arms, readings or the order they are evaluated in — those are frozen and the run that answered
them is scored. It adds the instrument that page could not carry.

## Why this exists

The frozen page asks two things: **does giving each tenant its own engine protect the premium tail, and what
does the premium tenant pay in capacity for it?** The ninth pilot answered the first — `timeSlicing` recorded
a 46.8% lower premium TTFT p99 than the whole-card control and still missed the 2.0x bar by a factor of
seven, which is reading 5, a registered outcome.

**It could not have answered the second, and the reason is structural rather than a shortage of repetitions.**
Every arm was offered the same fixed trace and every arm finished all of it:

| per repetition | `R1` | `shared` | `timeSlicing` |
| --- | ---: | ---: | ---: |
| premium offers | 4,655 | 4,655 | 4,655 |
| premium completions | 4,655 | 4,655 | 4,655 |
| premium output tokens | 297,920 | 297,920 | 297,920 |
| contender completions | — | 139/139 | 139/139 |

Identical delivered work is what you get whether an arm had ample headroom, saturated briefly, or built a
backlog and drained it afterwards. The numerator is a constant; only the elapsed-time denominator moves, and
in `timeSlicing` 3.31 s of that denominator is the **contender's** engine finishing after premium's last
completion. Every throughput figure the ninth pilot reported (589.3 / 586.9 / 584.0 tok/s) is that constant
divided by three slightly different denominators.

**No re-reading of that evidence will produce a capacity answer.** Two independent reviews on 2026-09-13
reached that conclusion separately, and the capacity claims the frozen page had derived from reciprocals of
latency are withdrawn there. What is missing is a definition and a ladder, and both are cheap.

## What this page may take from the answered run, and what it may not

**May:** the trace shape, the model, the engine settings, the cache budgets, the arm topologies, the harness,
and the measured R1 premium TTFT p99 of **69.524 ms** — a figure two repetitions agreed on to 0.179 ms, on
an arm whose premium concurrency peaked at 26 against 64 sequence slots and therefore was nowhere near its
own limits.

**May not:** any capacity, utilisation or service-rate number, because the frozen page withdrew all of them;
and the 2.0x tail bar as a *verdict*, because that verdict is delivered and this page does not re-open it.

The 2.0x bar reappears below as a **threshold in milliseconds**, not as a bar this run passes or fails. That
distinction is what keeps the ladder cheap: an absolute 139.0 ms target means R1 does not have to be bought
at every rung.

## The question

> **How much premium load can each topology sustain while still meeting the premium latency target, and how
> much of that does the split cost?**

## Capacity, defined before the run

> **Sustainable premium rate** = the highest offered premium arrival rate at which, over a 505-second
> arrival window, the arm's premium TTFT p99 is **≤ 139.0 ms** and fewer than **1%** of premium requests are
> censored by the client timeout.

139.0 ms is 2.00 x the ninth pilot's pooled R1 premium TTFT p99 of 69.524 ms. It is written here as an
absolute number because that is what makes the ladder affordable, and it is written **before** any rung is
run so that it cannot be moved to sit above or below whatever the rungs produce.

Three things this definition is not, said plainly so that a later reader does not have to infer them:

- **It is not maximum throughput.** An arm can complete more work than this at a worse tail. A different
  question — maximum completed tokens per second — would need a different definition and would answer a
  different decision. This page picks the service-qualified one because the study it amends is about a
  premium tenant's latency.
- **It is not proof of indefinite stability.** It is a finite-duration operating criterion over 505 seconds.
  Anything beyond that is extrapolation and this page does not make it.
- **It is not a per-tenant guarantee.** The contender's own service is recorded at every rung and is not part
  of the criterion.

## Arms

| arm | what it is | at which rungs |
| --- | --- | --- |
| `shared` | one whole-card engine serving both tenants | every rung |
| `timeSlicing` | two engines, one per tenant, time-slicing one card | every rung |
| `R1` | premium alone on a whole-card engine | **the highest rung reached, once** |

`R1` runs once rather than at every rung, and the reason is worth stating because it is where most of the
saving comes from. The criterion is an absolute 139.0 ms, already established on an R1 that was not close to
its own limits, so a denominator is not needed per rung. What R1 **is** needed for is the top of the ladder:
without it, "the split ran out of capacity" and "a single engine of this model on this card ran out of
capacity" are the same observation. One cell distinguishes them.

`mps` is not in this page. It failed to engage on this card in the eighth pilot and reading 4c refused it;
nothing here changes that.

## The ladder

Four rungs, at roughly 1x, 2x, 3.5x and 5x the premium load the ninth pilot offered. **The contender is held
fixed in absolute terms at every rung** — not by tenant weight, which is the mistake that killed the seventh
pilot: raising total rate while keeping a weight raises both tenants together and changes two things at once.

The generator takes a total rate and per-tenant weights, so holding one tenant fixed while the other climbs
means solving for the pair. That was done offline, for $0, against the real `gen-trace` at `--seed 11`, and
the realised counts below are **measured from the generated traces, not predicted**:

| rung | premium target | `RATE` | `NOISY_WEIGHT` | premium offers | contender offers |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 1.0x | 9.4045 | 0.0260 | 4,655 | 139 |
| 2 | 2.0x | 18.706076 | 0.01316608 | 9,310 | 139 |
| 3 | 3.5x | 32.492348 | 0.00839311 | 16,293 | 139 |
| 4 | 5.0x | 46.185689 | 0.00611822 | 23,271 | 141 |

`PREMIUM_WEIGHT=1`, `PROBE_WEIGHT=0`, `DURATION_MS=505000`, `--seed 11` at every rung.

**Rung 1 is the ninth pilot's own parameters, and it regenerates that run's trace byte for byte.** That was
checked, not assumed. It costs two cells and buys the tie between the two experiments: rung 1 should
reproduce the ninth pilot's TTFT p99s, and if it does not, the difference is a session or card effect and the
rest of the ladder is read in that light.

The contender lands at 139 offers at three rungs and 141 at the fourth; a search over the generator's
parameter space did not find an exact 139 at rungs 3 and 4. **The registered tolerance is 139 ± 2 offers**,
recorded here rather than discovered afterwards. Every rung clears the frozen page's reading 4b floor of 100
contender requests per repetition.

## Order

**Odd rungs run `shared → timeSlicing`. Even rungs run `timeSlicing → shared`.**

The frozen page's fixed order makes every one of its results a difference between (arm, position) pairs, and
it says so. Here the counterbalance costs nothing: the two contended arms each occupy the first and second
position of a rung exactly twice across four rungs. The `R1` cell runs last, after both arms of its rung.

This does not remove carryover between rungs, and this page does not claim it does. Each cell starts from a
fresh engine rollout and the plugin state its arm requires, which is what the matrix already does.

## Repetitions

**One repetition per cell.** This is a deliberate departure from the frozen page's two, and the reason is
that the two experiments are measuring different things. Readings 3 and 5 judge an improvement against the
control's repetition-to-repetition spread, so they need two. This ladder judges a p99 against a fixed
139.0 ms threshold, which needs none.

What two repetitions bought on the ninth pilot is recorded: the control moved 1.375 ms and R1 moved
0.179 ms across repetitions of the same trace. An instrument whose repeat noise is three orders of magnitude
below the quantity being bracketed does not need a second repetition at every rung more than it needs a
fifth rung. **If a rung lands within 10% of 139.0 ms, that rung — and only that rung — is repeated**, because
that is where repeat noise could decide the answer. This rule is registered now so that it cannot be invoked
selectively later.

## Stopping rule

Climb from rung 1. **Stop at the first rung where both arms have breached** — p99 above 139.0 ms or censoring
at or above 1% — and do not buy the remaining cells. Buy the single `R1` cell at that rung.

If rung 4 is reached with either arm still meeting the criterion, the ladder stops there anyway: the budget
is one session. The result is then reported as a **lower bound** — "both topologies sustain at least 46.1
premium requests per second under this criterion" — and not extrapolated to a limit that was not located.

A client timeout establishes failure at that offered rate. It does not estimate a capacity, and no rate is
inferred from a reciprocal of any latency on this page. That is the error the frozen page had to withdraw.

## Outcome space, registered in advance

| what the ladder shows | what it means | what it does not mean |
| --- | --- | --- |
| `timeSlicing` breaches at a **higher** rung than `shared` | the split buys headroom as well as a better tail; the sizing statement is "the split sustains N% more premium load at this target" | nothing about a card of a different size, or about MIG |
| both breach at the **same** rung | the split is a latency-distribution trade at a fixed capacity: it buys the region beyond two seconds and costs the contender 3.2x on completion p99 | that capacity is equal — only that the ladder's rungs could not separate them |
| `timeSlicing` breaches **first** | the split costs capacity, and the ninth pilot's 884.6 ms improvement was bought out of headroom; **the platform recommendation reverses** | that the tail improvement was not real |
| `R1` also breaches at the top rung | the ladder found a single engine's limit, not a topology's; the bracket that was established is reported and lower rungs are what is missing | that either topology reached its own limit |
| **no rung meets the criterion, including rung 1** | no qualified operating point was found at or above rung 1 | that capacity is zero — only that the ladder searched upward and the answer is below where it started |

The last row is the outcome this page is most likely to get wrong under pressure, so it is written down: if
rung 1 breaches, **the definition does not change and the ladder does not turn around mid-run.** It is
reported as a ladder that searched in the wrong direction, and searching downward is a separate purchase.

## Budget

| line | cost | note |
| --- | ---: | --- |
| ~~implementation and rehearsal~~ | $0 | **done, 2026-09-13, and it bought three defects.** See below |
| rung 1 and 2, four cells | ~$0.55 | at the ninth pilot's demonstrated $0.68/h and ~8.3 min per cell plus ~25 min of fixed bring-up |
| rungs 3 and 4 if reached, four cells | ~$0.45 | not spent if the stopping rule fires earlier |
| the one `R1` cell | ~$0.10 | at whichever rung the ladder stops |
| **total if the ladder runs to the top** | **~$1.10 to ~$1.65** | one session, and the range is not hedging — see below |

**Two numbers because two things are being estimated.** $1.10 is nine cells at the ninth pilot's *measured*
8.3 minutes per cell. $1.65 is what the runner's own credential check demands before it will start: it
asks for **143 minutes** for this ladder, from 25 minutes of bring-up, 1.5 per engine rollout, a replay
minute derived from `DURATION_MS` plus one for the client's drain, and 15 for evidence and teardown. That
check is deliberately pessimistic — a credential margin is the wrong place to be optimistic — and it is the
number to budget against. The measured figure is what to expect.

Both are upper bounds in one respect: **the stopping rule can end the climb at rung 2 or 3**, and the cells
above it are not bought.

Nine pilots of the frozen page cost about $4.85. This page's ladder is priced against the same measured rate
and does not assume a cheaper instance than the one that produced those figures.

## What the free rehearsal bought, before any card

Added 2026-09-13, after building the instrument and running it end to end on a kind cluster with stub
engines. The ladder's five cells ran, in the counterbalanced order, and the readings answered **L6** — a
lower bound, which is the only answer a cluster with no card can legitimately produce.

Three defects were bought for nothing. All three would have surfaced on a rented instance, and two of them
only after every cell had been paid for.

**1. The report refused the ladder's own evidence.** `refuseIfTracesDisagree` requires every contended arm
to carry one trace checksum, which is correct for every study that offers a single load. The ladder offers
a different load per rung **on purpose** — that is what a rung is — so the report ended the run with
`arm rung02-shared replayed a different trace than the other contended arms`. On a card that refusal
arrives **after the last cell**, with the whole session spent. The identity now holds within a comparison
group, which is the whole study for every other experiment and the rung for this one.

**2. The baseline would not have been a baseline.** `gen-trace` filters the contending tenant out of the
isolated arm's trace, and it decided whether to do that by asking `arm == "R1"`. This ladder's baseline is
called `rung02-R1`, so the literal would have looked straight past it and produced a "baseline" **carrying
the contender** — the contended case wearing the denominator's name, with nothing anywhere saying so. The
question is now asked of a function that knows every study's baseline, and the rehearsal checks the
tenants in the baseline's own rows rather than trusting it.

**3. The runner died on a banner.** `RATE` and `NOISY_WEIGHT` do not exist in ladder mode, and the line
that prints the load named them anyway. Under `set -u` that ends the script **after** the cluster and both
images are built and **before** the first cell. Free here; on a card it is the bring-up time.

The first two are the same shape as the defects the frozen page's journal records: a guard that exists, is
correct where it was written, and was never told about the case that now matters. Neither announced itself
as a bug — one refused correct evidence, the other would have accepted wrong evidence silently.

**What the rehearsal still cannot reach** is the stopping rule's STOP branch: no stub is ever slow enough
to breach a 139 ms target, so on a free cluster the ladder always climbs. That branch is pinned where it is
decided instead — in `internal/bench`'s tests, and in `hack/test/check-ladder-refusals.sh`, which builds a
breaching cell from the ninth pilot's own rows with the first-token waits rewritten and checks that the
verdict says STOP, exits the code the runner breaks on, and is told apart from the exit an unscorable rung
produces.

## What the ladder answered, and why the answer was decided before it ran

Added 2026-09-13 after the first cells were bought. Nothing above this line has been edited since, which is
what this page promised.

**The answer is reading L5: no qualified operating point at or above the bottom rung.** The two cells of
rung 1 were measured on a fresh instance and a fresh card, and both breached the 139.0 ms target:

| rung 1 cell | premium TTFT p50 | p90 | p99 | target |
| --- | ---: | ---: | ---: | ---: |
| `rung01-shared` | 59.1 ms | 927.6 ms | **1,896.0 ms** | 139.0 ms |
| `rung01-timeSlicing` | 106.8 ms | 161.1 ms | **1,013.2 ms** | 139.0 ms |

**That result was determined before any card was rented, and this page did not notice.** The ninth pilot
measured exactly this load at 1,892.2 / 1,893.5 ms and 1,001.9 / 1,014.2 ms. The target is 139.0 ms. Both
topologies therefore miss the bottom rung by a factor of seven, in numbers this page cites in its own second
paragraph — so a ladder that CLIMBS from there could return nothing but L5, whatever the higher rungs did.

The outcome space anticipated the shape and said the honest thing about it: "the ladder searched upward and
the answer is below where it started". What it did not do is check, before spending, whether the bottom rung
was already below the answer. **A search direction is part of a design, and this one was chosen without
looking at the evidence that decided it.** Two cells were bought to re-confirm a number the study already
had.

**What the two cells are worth anyway.** They are a third independent replication of the ninth pilot's
control and split, on a different instance and a different physical card: the control lands 2.5 ms and
3.8 ms from its two earlier measurements and the split 11.3 ms and 1.0 ms from its own. That bounds the
session-and-card effect on this comparison at a few milliseconds against a difference of 883 ms, which is
the tie this page's rung 1 was designed to buy — and it is the only part of it that was not already known.

**And a defect in the runner nearly hid the answer.** The verdict command scored rung 1 correctly, wrote
`LADDER: STOP` and exited 10. The runner read the status with `$?` **after an `if`**, which is the `if`
statement's status and not the command's — 0 when the condition failed and no `else` ran. So a correct stop
arrived as 0 and the session ended reporting the rung *unscorable*, which is the one thing it was not.

The rehearsal could not have caught it: a stub answers in milliseconds, so on a free cluster every rung
CONTINUEs and the stopping branch never executes. The exit code had been pinned — of the **command**, not of
the script's reading of it. `hack/test/rehearse-m5c-matrix.sh` now drives the real runner through that
branch with a shim that answers only `ladder-verdict`, and the defect was confirmed to fail it before the
fix was confirmed to pass it.

**Cost of the day: about $0.40** — $0.05 for a session the host killed under memory pressure four minutes in
(its exit trap terminated the instance correctly), and about $0.35 for the two cells above.

**What a ladder that could answer the capacity question looks like is a different page**, because this one
is frozen from the moment its first cell was bought. It has to search **downward** from 9.22 premium
requests per second, and its rungs have to be chosen after looking at what the answered run already says
about where the target lies.

## What this run will not be able to say

- **Anything about maximum throughput.** The criterion is service-qualified. An arm that breaches 139.0 ms
  can still be completing more work than one that does not.
- **Whether the batch cap or the topology moved the result.** The premium engine runs `--max-num-seqs=64`
  whole-card and `32` split, and the frozen page now records that as an unmeasured alternative explanation.
  The ladder inherits it unchanged: both topologies keep the settings the answered run used, so this page
  compares the same two configurations at more rates and separates nothing new about why they differ.
- **Anything about the contender's own capacity.** Its load is held fixed precisely so that it is not a
  variable here, which means its limits are not located either.
- **Sustained stability.** 505 seconds per cell, once.
- **A general claim about multi-tenant GPU sizing.** One A10G, one model, one prompt mix, one contender rate.

## What was decided before any data

The definition of capacity, the 139.0 ms threshold and where it comes from, the four rungs and their exact
generator parameters, the contender's fixed 139 ± 2 offers, the counterbalanced order, one repetition with
the single registered exception at a 10% margin, the stopping rule, the outcome space including the two rows
that would reverse or invalidate the recommendation, and the budget.

The rung parameters were solved offline against the real generator and their realised counts measured, which
is why this page prints counts rather than rates alone. None of it was decided from a result of this ladder,
because this ladder has none.
