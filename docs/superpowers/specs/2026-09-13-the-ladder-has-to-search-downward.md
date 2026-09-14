# The ladder has to search downward — pre-registration

**Written 2026-09-13, after the first ladder returned L5, and before any card time was bought for this one.**
Nothing on this page may be edited from the moment its first cell is bought, except to record what happened.

This page amends `2026-09-13-what-the-split-costs-in-throughput.md` in the open. It changes **only where the
rungs sit**. The criterion, the arms, the readings, the stopping rule, the counterbalanced order and the
outcome space are that page's, unchanged, and the instrument that evaluates them is the same code.

## Why this exists

The first ladder placed its bottom rung at the load the sharing matrix answered — 9.22 premium requests per
second — and climbed. Both topologies miss the 139.0 ms target at that load by a factor of seven, in numbers
the first page cites in its own second paragraph. **A ladder that climbs from there can return nothing but
"no qualified operating point at or above the bottom rung", and it did.** Two cells were bought to
re-confirm a figure the study already had.

The error was not the criterion and not the stopping rule. It was the **search direction**, chosen without
checking the evidence that already decided it. This page fixes that and nothing else.

## What it takes from the two runs above it, and what it may not

**May:** the 139.0 ms target and where it comes from; the trace shape, model, engine settings and cache
budgets; the harness, the readings and the counterbalance; and the measured fact that at 9.22 premium
requests per second **both topologies breach**, three times over on three instances and **two recorded
cards** — the two ninth-pilot repetitions share one GPU UUID, which an adversarial review checked and this
page had asserted wrongly.

**May not:** any capacity number, because none has been measured; and the target itself, which was fixed
before any rung of either ladder ran and is not recomputed from this one's evidence.

## The question, unchanged

> **How much premium load can each topology sustain while still meeting the premium latency target, and how
> much of that does the split cost?**

## Capacity, defined before the run — unchanged

> **Sustainable premium rate** = the highest offered premium arrival rate at which, over a 505-second
> arrival window, the arm's premium TTFT p99 is **≤ 139.0 ms** and fewer than **1%** of premium requests are
> censored by the client timeout.

## The rungs

Three, below the answered load, **ascending in rate** so that the registered stopping rule applies exactly
as written: climb until both topologies breach, and the highest rung each one met is its bracket.

"Searching downward" is about where the rungs sit, not about the direction the runner walks them. Inverting
the walk would have inverted the stopping rule and the readings' notion of "highest met rung", and a
direction flag threaded through a scorer is a second way for the same criterion to mean two things.

The parameters were solved offline against the real `gen-trace` at `--seed 11`, and the counts below are
**measured from the generated traces, not predicted**:

| rung | premium | `RATE` | `NOISY_WEIGHT` | premium offers | contender offers |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 1.16 req/s | 1.431979 | 0.23386882 | 585 | 139 |
| 2 | 2.31 req/s | 2.564749 | 0.11403366 | 1,164 | 139 |
| 3 | 4.61 req/s | 4.847585 | 0.05746316 | 2,327 | 139 |

`PREMIUM_WEIGHT=1`, `PROBE_WEIGHT=0`, `DURATION_MS=505000`, `--seed 11` at every rung. The contender lands
at exactly **139 offers at all three rungs**, which is the count every rung of the first ladder held and the
count the sharing matrix ran.

**Why the bottom rung is 1.16 and not lower.** The readings refuse a premium tail resting on fewer than 500
completed requests, because a nearest-rank p99 over fewer than that moves with one slow response. At 505
seconds, 500 requests is about 1 per second. **The ladder cannot search below roughly 1 req/s without
changing the trace length, and this page does not change it** — a longer trace at a lower rate is a
different experiment with a different amount of contender work in it.

**Why there is no fourth rung.** 9.22 req/s is already measured, three times, and both topologies breach
there. It is the ladder's **ceiling by prior measurement rather than by purchase**, so a bracket that closes
at rung 3 is closed against it for free.

## Order, repetitions, stopping rule — unchanged

Odd rungs run `shared → timeSlicing`, even rungs the reverse. One repetition per cell, with the registered
exception that a rung landing within a tenth of the target is repeated. Climb until **both** topologies
breach; buy the single `R1` cell at the rung the ladder ends on.

## What each result would mean

| the ladder shows | the sustainable-rate statement |
| --- | --- |
| both meet at rung 3 | both topologies sustain at least 4.61 req/s and breach by 9.22; the bracket is [4.61, 9.22] for both and the ladder did not separate them |
| `timeSlicing` meets at a higher rung than `shared` | the split sustains more premium load at this target — the capacity cost of separation is **negative** at this operating point |
| `shared` meets at a higher rung | the split **costs** capacity, and the tail improvement the sharing matrix measured was bought out of headroom |
| neither meets at rung 1 | no qualified point at or above 1.16 req/s. The target is then unreachable under this contender load at any rate this trace length can measure, which is itself the answer and needs no further rung |

The last row is the one this page is most likely to get wrong under pressure, so it is written down: **if
rung 1 breaches, the target does not move and the trace does not lengthen.** That would say the 139 ms
premium requirement is not satisfiable against 139 contending prompts of 40,000 characters on one A10G,
under either topology — a finding, not a failed run.

## Budget

| line | cost | note |
| --- | ---: | --- |
| three rungs, six cells | ~$0.65 | at the demonstrated $0.68/h; fewer if the stopping rule fires at rung 1 or 2 |
| the one `R1` cell | ~$0.10 | at whichever rung the ladder ends on |
| **total** | **~$0.75** | one session |

Spent on this study so far: about **$0.40** — a session the host killed four minutes in, and the two cells
that returned L5.

**What it actually cost: about $1.07**, for 94 minutes of instance. The estimate used 8.3 minutes per cell,
which is what the sharing matrix measured for cells that roll out ONE engine; five of this ladder's seven
roll out two. Recorded here rather than quietly corrected, because a budget that is only ever right in
hindsight is not a budget. **Total across both ladders and the killed session: about $1.47.**

## THE ANSWER

Added 2026-09-13 after the run. Nothing above this line has been edited since the first cell was bought.

```
ANSWER: L1 -- the topologies sustain different loads
```

**The split sustains premium load at a target the whole card does not meet at any rate this ladder offered.**

| rung | premium offered | `shared` premium TTFT p99 | `timeSlicing` premium TTFT p99 |
| ---: | ---: | ---: | ---: |
| 1 | 1.16 req/s | 1,282.5 ms **BREACH** | **123.8 ms met** |
| 2 | 2.31 req/s | 1,694.7 ms **BREACH** | **130.4 ms met** |
| 3 | 4.61 req/s | 2,303.3 ms **BREACH** | 143.2 ms **BREACH** |
| — | 9.22 req/s, measured three times before this ladder | 1,892 / 1,894 / 1,896 ms BREACH | 1,002 / 1,013 / 1,014 ms BREACH |

The contender was held at **139 offers in every CONTENDED cell**, and completed **139 of 139** in every
one of them. The isolated-baseline cell carries 2,327 premium rows and no contender rows, by construction —
an earlier version of this line said "every cell" and swept the baseline in with the rest.

**The sustainable premium rate, as this page defined it before the run:**

- **`timeSlicing`: met the target at 2.31 req/s and missed it at 4.61.**
- **`shared`: missed it at every rate this ladder offered**, the lowest being 1.16 req/s. Where it would
  qualify was not located, and could not be: the sample floor puts the bottom rung at about 1 req/s for this
  trace length.

**Those are pass/fail results at four sampled loads, not a proof that nothing between or above them
qualifies.** The evidence forbids the stronger reading: `shared`'s tail is **not monotone in the premium
rate** — 1,282 ms at 1.16, 1,695 at 2.31, 2,303 at 4.61 and **1,896 at 9.22**. It peaks in the middle of the
range and comes back down. Nothing measured says a rate between 2.31 and 4.61, or above 9.22, cannot behave
differently again. What the ladder establishes is the four points and the ordering between the two
topologies at each of them.

That is the capacity answer the sharing matrix could not produce, and it points the opposite way to the
question's framing. The question asked what the premium tenant **pays** in capacity for separation. At this
contender load the answer is that it **pays nothing and gains**: the whole card has no qualified operating
point in the measured range and the split has one.

**Why offering the control less did not rescue it, over the range measured.** Its p99 falls as the premium
rate falls — 2,303 ms at 4.61 to 1,282 ms at 1.16 — and **stays an order of magnitude over the target the
whole way**. The tail is not made of premium queueing behind premium; it is made of premium queueing behind
a contender prefill, and there are 139 of those whatever the premium rate is. That is the mechanism this
study registered in its first page, and this is the first time it has been seen at the bar.

An earlier version of this paragraph said "lowering premium load **cannot** fix a tail the other tenant
produces". That is an extrapolation below the sampled range and this page does not have it: **the lowest
rate measured is 1.16 req/s**, the trend over the four points is toward improvement as premium load falls,
and where it would cross 139.0 ms is exactly what the sample floor prevented this ladder from looking for.

**The isolated baseline says the engine was not the limit.** `rung03-R1` measured **64.0 ms** at 4.61 req/s
uncontended, so reading L4 is correctly silent: what rung 3 located is a topology's limit, not this model's
on this card.

### What is soft about this result, stated before anyone quotes it

**Both cells that set the split's bracket are within a tenth of the target, and the registered repeat rule
fired for both rungs.** 130.4 ms and 143.2 ms sit either side of 139.0 ms by 8.6 and 4.2 ms, and the
instrument's own repeat noise on a control was 1.4 ms across repetitions of one trace. That is a margin of
three to six times the measured noise, not a comfortable one. **The upper edge of [2.31, 4.61) rests on
those two cells and a repetition of rungs 2 and 3 is owed.** The pre-registration asked for it in advance
precisely so that it could not be skipped once the numbers were known.

Nothing about the *direction* of the result is soft: `shared` misses by a factor of nine to seventeen at
every rung, which no repetition moves.

### What the split costs, since it is not free

These ratios are **rung 1's**, not the ladder's. At rung 3 the same comparison is about 3.60x on the
median and 3.45x on the p99, so the penalty is of one size rather than one number.

| at rung 1 (1.16 req/s) | `shared` | `timeSlicing` | |
| --- | ---: | ---: | ---: |
| premium TTFT median | 50.0 ms | 83.2 ms | **1.7x worse** |
| premium TTFT p99 | 1,282.5 ms | 123.8 ms | **10.4x better** |
| contender completion median | 1.284 s | 4.163 s | **3.2x worse** |
| contender completion p99 | 3.942 s | 15.370 s | **3.9x worse** |

The same trade the sharing matrix found, at the same shape: **premium buys its tail with its median, and the
contender pays for both.** It loses no work — 139 of 139 everywhere — and it waits three to four times as
long. A platform that runs this topology is choosing that, and should say so to the tenant it is charging
for it.

## What this run will not be able to say

- **Anything about rates below about 1 req/s**, for the sample-floor reason above.
- **Whether the batch cap or the topology moved the result.** Inherited unchanged from the sharing matrix,
  which records it: the premium engine runs `--max-num-seqs=64` whole-card and `32` split.
- **Maximum throughput.** The criterion is service-qualified; an arm that breaches can still be completing
  more work than one that does not.
- **Sustained stability.** 505 seconds per cell, once.

## What was decided before any data

The three rungs and their exact generator parameters, measured offline; that the rungs ascend so the frozen
stopping rule and readings apply unchanged; that 9.22 req/s is the ceiling by prior measurement rather than
by purchase; the sample-floor argument that sets the bottom rung; that the target does not move if rung 1
breaches; and the budget.

None of it was decided from a result of this ladder, because this ladder has none. What it *was* decided
from is the first ladder's result, and this page says so in its title.
