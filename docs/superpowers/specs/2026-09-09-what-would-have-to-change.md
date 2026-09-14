# What would have to change — the successor's constraints, pre-registered

Date: 2026-09-09 · Written **after** the price-of-protection run and **before** any successor is designed or
bought. It pre-registers the feasibility arithmetic and what a successor must settle before it may spend.
It does not pre-register a study, because the numbers that would set that study's thresholds are exactly the
numbers this page refuses to choose while the last result is fresh.

## What is settled

Two layers have now been measured and neither protects the premium tail at this load.

**Admission, M5-b.** A reactive KV-occupancy guard missed the 1.25x bar at 83.7x. Re-analysis then found the
occupancy limb was unreachable by construction and never fired at all; every refusal came from the waiting
queue, and the refusals were anti-correlated with harm
(`2026-09-04-the-layer-not-the-signal.md`).

**Engine scheduling, the price-of-protection run.** Batch budget crossed with scheduling policy, eight cells
plus a control and an isolated baseline, three repetitions, no timeouts. The best cell holds the premium
tail at 20.7x an isolated baseline against a 2x bar, and its MEDIAN misses by 2.8x
(`2026-09-08-the-load-needs-an-upper-gate.md`). Nine cells, nine times two-of-four, and always the same two.

## The arithmetic that closed it

| quantity | value | where from |
| ------------------------------------- | --------: | ---------- |
| contending prompt, real tokens | 7,744 | probe calibration, 3,171 tokens at 16,380 chars |
| engine sustained prefill | ~7,500 tok/s | the control's own completions under load |
| **time one contending prompt occupies the engine** | **~1.03 s** | the two above |
| isolated premium tail p99 | 67.3 ms | R1, three repetitions |
| **premium tail budget at the 2x bar** | **0.135 s** | the bar applied to that baseline |
| ratio | **~7.6x** | |

Chunked prefill divides the second into pieces and priority lets a premium request enter at a piece
boundary. That is why those knobs move anything, and it is why they move it about fivefold rather than not
at all. It is not why they fail: a premium request still waits for the piece in flight and for whatever the
scheduler has already admitted, and no ordering of a shared engine makes the machine into two machines.

**So the feasibility condition is a ratio, not a policy.** For a bar of `k` times an isolated baseline `T`
to be reachable while a contending tenant is served, the time a contending unit occupies the engine has to
come down to the order of `k·T`. At `k = 2` and `T = 67.3 ms` that is about 135 ms against the 1.03 s
measured here.

## What a successor must settle before it spends

Three levers can move that ratio, and each is a different study. This page names them and refuses to pick.

1. **Shrink the contending unit.** A shorter contending prompt, or a cap on admitted prefill work per unit
   time, changes the numerator directly. The arithmetic above says how far it must come down; nothing here
   says such a workload is the one worth measuring.
2. **Widen the budget.** A bar other than 2x. This is the lever this page most refuses to touch: every cell
   missed by an order of magnitude, and choosing a reachable bar now, with those numbers in hand, is the
   post-hoc threshold-moving that the `static-cap` arm was invented to forbid. A successor may set a
   different bar only from a stated service objective that does not read this run's results.
3. **Separate physically.** Disaggregated prefill and decode, an engine per tenant class, or a partitioned
   card. The literature reports these working, on multi-GPU testbeds and at 90% attainment rather than at a
   premium p99 against an isolated baseline; two processes on one unpartitioned card do not reproduce that
   isolation. This is the axis M5-c was scoped for.

A successor is a pre-registration of its own, with its own readings, its own budget, and a pilot gate. It
does not inherit this page's.

**Lever 3 was picked on 2026-09-10** and pre-registered as
`2026-09-10-does-splitting-the-card-buy-protection.md`. It keeps the 2x tail and 1.25x TPOT bars rather than
setting new ones, on the grounds this page gives: carrying them forward is the only choice that cannot be
accused of fitting, and no service objective independent of these results has been stated. It also closes
the outcome-space gap named below, in advance, as a reading for a cell that improves on the control without
reaching the bar.

## Two things this page fixes on its way past

**The readings did not cover their own outcome space.** Reading 2 requires some cell to have met the tail
bar; reading 3 requires no cell to have beaten the control. The evidence landed between them — every cell
beat the control by about two seconds against a three-millisecond spread, and none came within an order of
magnitude of the bar — so nothing fired. A successor's readings must be checked for exhaustiveness before
they are registered, by writing down what the evidence would look like if each reading failed to apply.

**M5-c's deferral has been justified by the wrong reason.** `README.md` and earlier notes defer the sharing
matrix as though device attribution were the obstacle; `hack/m5c-matrix.sh` deliberately runs no observer on
the sharing node and never depended on that. The real reason it was deferred was that its `shared` arm is
M5-b's topology on an engine whose budget and policy were unsettled. **They are settled now** — B₀ is 2048,
the policy axis is measured, and lever 3 above is what M5-c was for. That removes the deferral's stated
grounds, which is a thing to notice rather than a licence to buy it.

## What this page costs

Nothing. It is written from evidence already bought, at $3.98 for the confirmatory run and $1.06 for the
pilots that preceded it.
