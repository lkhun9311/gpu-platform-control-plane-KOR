# The load needs an upper gate — an amendment, written after the pilot

Date: 2026-09-08 · **Written with the pilot's results in hand.** That is the thing to know about this page
before reading it, and the reason it is a separate document.

`2026-09-05-the-price-of-protection.md` says in its own header that nothing may be edited from the moment
the pilot is bought. The pilot was bought on 2026-09-07. So that document is closed, and this one amends it
in the open rather than editing it quietly — which is exactly the post-hoc revision the freeze rule exists
to prevent.

## What the pilot showed

Its reading 4 asks whether the load produced contention, and fires INVALID when the control's premium TTFT
p99 is under 5x R1's. On the pilot it was **194x**, so reading 4 passed comfortably.

It passed on a run where the control completed **17 of 555** premium requests and **26 of 544** of the
contending tenant's, everything else timing out at 30 s. Every reading below reading 4 is a ratio of tails
and output shares. On that evidence they are ratios over a remnant, and nothing in the pre-registration
would have said so — reading 4 guards only the side where there is too little contention.

The pilot's own numbers do not appear anywhere below this section, and none of them set the threshold.

## The amendment: reading 4b

> **4b. The load was too high to measure — INVALID, and the trace's rate is what changes.**
>
> If the control completed fewer than `MinTailSamples` premium requests, or fewer than `MinTailSamples` of
> the contending tenant's, the run is INVALID. It is evaluated immediately after reading 4 and, like reading
> 4, it stops the readings rather than being one of the outcomes they choose between.

It sits beside reading 4 rather than at the end because the two are the same axis. Reading 4 says the load
was too gentle to create the problem; 4b says it was too violent for the instrument to see the problem. Both
mean the trace, not the configurations, is what has to change.

## Where the threshold comes from, and where it does not

`MinTailSamples` is **100**, and it was not chosen for this amendment. It is derived in
`internal/bench/report.go` from the percentile the report uses: nearest-rank puts the p99 at index
`ceil(0.99*n)-1`, clamped to `n-1`, and solving `ceil(0.99*n)-1 < n-1` gives 100 as the smallest integer
where the p99 stops being simply the largest observation. Below it, a "p99" is the slowest request wearing a
percentile's authority.

Three things follow, and they are why this is an amendment I am willing to write after seeing data:

- The constant predates the pilot and predates this document. `EvaluateChecks` has invalidated the M5-b
  study's arms on it since it was written. This amendment applies an existing floor to a second study; it
  does not introduce a number.
- It is derived from the arithmetic of the statistic, not from a judgement about what a good completion rate
  looks like. There was no freedom to place it above or below what the pilot happened to produce.
- It is applied to the **control** only, because the control is what every ratio in readings 1, 1b, 2 and 3
  is taken against. A cell that collapses is a result about that cell. A control that collapses is a missing
  denominator.

The contending tenant gets the same floor for the same reason: readings 1 and 1b compare that tenant's
output share against **its share under the control**, and reading 2 asks what became of the work it lost.
A share computed over a dozen completions is not a share, and the pre-registration's own text for reading 2
insists that a share loss must be explained by the ledger rather than assumed.

## What this does NOT change

No bar in readings 1, 1b, 2, 3 or 4 moves. Not the 2x tail, not the 1.25x TPOT, not the 75% share, not the
95% throughput, not the 5x contention floor. This amendment adds one INVALID outcome and touches nothing
that decides between the others — because those are the thresholds the pilot's numbers could actually
inform, and are therefore the ones I have no business adjusting now.

The pilot itself is INVALID under this amendment, which is the honest reading of it: it bought three harness
defects and this gap, and it did not buy a measurement of protection.

## What has to happen before the confirmatory run

1. **Set the trace's rate from a completion target, not from the stub calibration.** The generator's default
   is 20/s, and `cmd/benchharness/main.go` already warns in its own flag help that this figure is calibrated
   against a stub backend that costs nothing to serve. Nothing in the pilot's configuration read that
   warning. The rate must be chosen so the control clears reading 4b with margin, and 4b is what refuses the
   run when it was not.

   ### How far past capacity the pilot was, and what fits instead

   This is a design input rather than a criterion, so unlike the threshold above it is derived from the
   pilot — that is what a pilot is for. The engine's sustained prefill throughput is measured from the
   control's own completions: 64 of them carrying about 247,800 real prompt tokens over 33.0 s, which is
   **about 7,500 tok/s**. It is achieved under thrashing and is therefore a lower bound on what a calm
   engine would do. Real token costs come from the probe calibration the report already prints, 3,171
   tokens at 16,380 characters.

   | tenant | offered | tokens each | prefill demand | of capacity |
   | -------------------- | ------: | ----------: | -------------: | ----------: |
   | `premium-1` | 9.25/s | 39 | 358 tok/s | 5% |
   | `standard-noisy` | 9.07/s | 7,744 | 70,209 tok/s | **934%** |
   | `standard-probe-over` | 0.88/s | 3,172 | 2,802 tok/s | 37% |
   | `standard-probe-under` | 0.97/s | 3,171 | 3,065 tok/s | 41% |
   | | | | **76,434 tok/s** | **1016%** |

   The engine was offered **ten times** the prefill it can do. That is the whole of why 95.9% of the control
   timed out, and it is not a subtle miscalibration.

   Two things follow. **The protected tenant's load does not change** — premium is 5% of capacity, so
   nothing about the contention is coming from it, and altering the load whose tail the study protects would
   change what the result means. And the **probe pair is not the small population its flag help claims**: at
   3,171 tokens each they carry 78% of capacity between them, against premium's 5%. Their arrival share has
   to fall with the contender's.

   Holding premium fixed and putting the rest at 60% of measured capacity gives **noisy at 0.50/s and each
   probe at 0.05/s** — an eighteen-fold cut in the contender's rate. At that rate 100 noisy completions need
   about 202 s of arrivals, so the trace duration goes from 60 s to **420 s**.

   300 s was the first figure here and it was too tight. It realises about 135 contender arrivals, so it
   clears reading 4b's floor of 100 only if better than three quarters of them complete — too thin a margin
   for the gate that voids the whole run, and Poisson arrivals scatter around the mean besides. 420 s offers
   about 187, which needs 53%. Lengthening is the safe direction to add margin in: raising the rate instead
   would push utilisation back toward the saturation being fixed. The run at 420 s completed all 187.

   The contention survives the cut, which is the thing that could have made this unworkable. A noisy prefill
   occupies the engine for about 1.03 s, so at 0.50/s at least one is in flight about half the time — and
   `2026-09-04-the-layer-not-the-signal.md` measured premium TTFT p99 at 1,043 ms with a single concurrent
   long prefill, against reading 4's floor of 5x R1. One is enough. The load has to come down by an order of
   magnitude to be measurable and stays contended throughout.

   **This lengthens the confirmatory run**, and by how much is no longer a guess: the section below fits a
   runtime model to three paid runs and costs it. It is inside the $3.45 line but it is a different shape of
   spend.
2. **B₀ must resolve.** The original pre-registration's pilot gate is still unmet, and
   `hack/m5b-price-of-protection.sh` now keeps the whole engine log and says loudly when the control's batch
   budget cannot be read from it.
3. **Both gates are the confirmatory run's precondition**, and neither is satisfied by the 2026-09-07
   evidence.

## Both gates are now met, and the confirmatory run is costed — 2026-09-08

Written before the confirmatory run is bought, which is the only time this section can honestly be written.

**Reading 4b passes.** `hack/pop-20260908-015929` at the derived load completed 3,976 of 3,976 premium and
187 of 187 contender requests, with **no timeouts in any arm**, against 17 of 555 before. Reading 4 still
does not fire either: the control's premium tail is 51.5x R1's, so an order of magnitude off the load left
the engine plainly contended, as the derivation predicted.

**B₀ is 2048, established by construction.** vLLM never prints it — two paid pilots, whole logs, both
streams. `hack/pop-20260908-030550` ran the control beside an engine configured explicitly at 2048 and the
two were indistinguishable: same compile range endpoint, same 369,680-token KV cache, and premium tails
3,439.4 ms against 3,441.7 ms. That is 0.07% apart, from the workload rather than from the fingerprint logic.

### The runtime model, fitted to three paid runs

Runs 2 and 3 differ only in arm count, which separates the fixed cost from the per-arm cost:

| | |
| ------------------------------- | ------: |
| fixed (boot, image pull) | 15.0 min |
| per arm (engine restart) | 1.5 min |
| per arm-repetition (420 s replay) | 7.0 min |

Checked against run 1, which was not used to fit it: predicted 25 min against 25 measured, at a different
trace length. Ten arms at three repetitions is then **240 min in one run**, about $2.40.

### It is bought as three runs of one repetition, not one run of three

**One four-hour run cannot survive its own backstop.** `BACKSTOP_SECONDS` is 7,200 s, so the instance would
shut down at 120 minutes — and evidence uploads only at the end, so the whole spend would return nothing.
Raising the backstop fixes that and leaves the real objection: four hours of Spot exposure with a single
delivery point at the end, where an interruption in hour three costs everything.

Three runs of ten arms at one repetition are 100 min each, inside the existing backstop untouched, and an
interruption costs one repetition. They total about 300 min and **$2.90 to $3.25**, against the
pre-registration's $3.45 line. The extra over a single run is the fixed cost paid three times.

**Splitting repetitions across instances makes reading 3 stronger, not weaker.** Its threshold is the
control's repetition-to-repetition spread, and repetitions on separate instances put instance-to-instance
variation inside that spread where it belongs. The two control repetitions already in hand — different
instances, different hours — differ by **0.8 ms** at the premium tail, so the threshold does not become
uselessly wide by being made honest. Pooling them is what the report already does: one raw file is one
repetition, and the traces are byte-identical across runs because the generator's seed is fixed, which is
what makes them repetitions rather than three different experiments.

---

This page may not be edited once the confirmatory run is bought, on the same terms as the document it
amends.

---

# Results

Three repetitions, ten arms, thirty raw files, 12,627 requests per contended arm and **no timeouts in any
arm of any repetition**. `hack/pop-20260908-044443`, `-070534`, `-113422`, one repetition each, on three
separate instances. Everything above this heading was written before the run was bought and is unedited.

## Both gates held in all three repetitions

| repetition | control tail / R1 | premium completions | contender completions |
| ---------- | ----------------: | ------------------: | --------------------: |
| 1 | 51.2x | 3,976 | 187 |
| 2 | 50.9x | 3,976 | 187 |
| 3 | 51.1x | 3,976 | 187 |

B₀ resolved inside every repetition, by fingerprint match against that repetition's own 2048 cell.

## No reading fired, and that is the finding

The pre-registration's readings cover five outcomes. The evidence is none of them.

```
4   the load did not create contention        did not fire   (control is 51.0x R1)
4b  the load was too high to measure          did not fire   (11,928 and 561 completions)
1   protection without deletion               did not fire   (no cell met all four bars)
1b  protection, bought with throughput        did not fire   (same tail bar)
2   protection only by deletion               did not fire   (requires a cell that met the p99 bar)
3   no cell beats the control                 did not fire   (the best beats it by 2,040 ms)
```

Reading 2 requires that **some** cell met the tail bar. Reading 3 requires that **no** cell beat the
control. The evidence sits between them: the cells beat the control enormously and none comes close to the
bar. That region has no reading, and the honest thing to record is that the design did not cover it rather
than to name it now. A sixth reading written after seeing which way the numbers went would be the post-hoc
decision this whole document exists to refuse.

## What was measured

R1's premium tail is 67.3 ms, so reading 1's bar is 134.6 ms and the TPOT bar is 22.9 ms.

| cell | tail ms | / R1 | premium TPOT ms | / R1 | bars met |
| ---------------------- | ------: | -----: | --------------: | ----: | -------: |
| `default-fcfs` control | 3,435.4 | 51.0x | 139.1 | 7.6x | 2 of 4 |
| `mbt-0256-fcfs` | 7,243.0 | 107.6x | 46.9 | 2.6x | 2 of 4 |
| `mbt-0256-priority` | 1,619.7 | 24.1x | 46.5 | 2.5x | 2 of 4 |
| `mbt-0512-fcfs` | 4,856.6 | 72.2x | 80.4 | 4.4x | 2 of 4 |
| `mbt-0512-priority` | 1,476.9 | 21.9x | 81.1 | 4.4x | 2 of 4 |
| `mbt-1024-fcfs` | 3,972.8 | 59.0x | 127.4 | 7.0x | 2 of 4 |
| **`mbt-1024-priority`** | **1,395.3** | **20.7x** | 112.5 | 6.1x | 2 of 4 |
| `mbt-2048-fcfs` | 3,434.2 | 51.0x | 139.3 | 7.6x | 2 of 4 |
| `mbt-2048-priority` | 1,480.9 | 22.0x | 117.7 | 6.4x | 2 of 4 |

Every cell scores exactly two of four, and the same two: the contending tenant keeps its work and the
machine keeps its throughput in all nine, while the tail bar and the stream bar are missed in all nine.
**Nothing here protects anything.** The question this run asked was what protection costs, and the answer is
that at this load, on this card, no configuration of these two knobs buys any.

Three structures are worth naming, and all three are measured rather than argued.

**`priority` beats `fcfs` at every budget** — 107.6x to 24.1x, 72.2x to 21.9x, 59.0x to 20.7x, 51.0x to
22.0x. The two-request microtest predicted this interaction and the open-loop trace reproduces it.

**The budget moves the tail and the stream in opposite directions.** Down the `fcfs` column the premium
stream improves monotonically as the budget shrinks, 139.3 to 46.9 ms, while the tail degrades monotonically,
3,434 to 7,243 ms. A smaller chunk gives the decode loop more turns and gives a queued prompt fewer tokens
per turn. Any single-number reading of "better" would have to pick one of them.

**`mbt-2048-fcfs` reproduces the control to 1.2 ms** — 3,434.2 against 3,435.4. B₀ is 2048 and the default
policy is fcfs, so that cell IS the control, configured explicitly rather than by omission. It was included
to resolve B₀ by construction and it also serves as the run's own internal control on itself.

## What this cannot say

One model, one card, one load, one arrival trace, two knobs. The frontier this maps is a frontier of
`max_num_batched_tokens` crossed with scheduling policy, and nothing here touches the other levers the
literature reports — prefill/decode disaggregation, separate engines per tenant class, or admission at the
gateway on a signal that leads the harm rather than follows it.

It is also silent on whether a 2x bar was the right bar. The bar was pre-registered and is not moving, but a
run in which every cell misses it by an order of magnitude says more about the distance than about the cells.

## What it cost

| | |
| ------------------------------------------ | ----: |
| three pilots, including two that bought defects rather than numbers | $1.06 |
| confirmatory, three repetitions | $2.91 |
| one launch aborted on expiring credentials | $0.01 |
| **total** | **$3.98** |

Against a budgeted $1.30 for the pilot and $3.45 for the confirmatory run.
