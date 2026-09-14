# What protection costs — pre-registration

Date: 2026-09-05 · Pre-registered **before** the run is bought. Nothing here may be edited after the run.

**Revised 2026-09-05, before any card time was bought.** An adversarial review of this document against the
raw microtest JSON found that its stated reason for choosing the factor levels misread its own evidence,
that its control was not defined by anything measurable, that its repetition count was justified by a claim
about the bootstrap that is arithmetically false, and that its readings left one outcome uncovered. A
separate defect in the harness was found at the same time: TPOT percentiles were computed over unsorted
samples, so the already-paid evidence had to be re-scored. The revisions are listed at the end. Nothing may
be edited from the moment the pilot is bought.

Supersedes `2026-09-04-m5b-pilot-stopping-rule.md`, whose question (does the KV guard see anything?) was
answered by the free re-scoring: it does not, and the threshold it needs is structurally unreachable at this
load.

## Why this exists

M5-b has one measured, reproduced result and one unmeasured half.

The measured half: reactive KV-occupancy shedding did not protect the premium tail — 83.7x against a
pre-registered 1.25x, four repetitions, isolation control at 1.0x. The engine-level microtest then showed a
scheduler configuration holding the same interference to 1.57x with no gateway involved.

The unmeasured half is what those numbers cost. Re-scoring the already-paid 2026-09-03 evidence with
throughput and per-tenant share — no new spend — gives:

| arm           | premium TTFT p99 | output tok/s | premium share | noisy share | premium TPOT p99 |
| ------------- | ---------------: | -----------: | ------------: | ----------: | ---------------: |
| R1 (isolated) |          82.2 ms |         46.4 |          100% |           — |          16.0 ms |
| off           |      10,595.3 ms |         57.3 |           80% |         20% |         266.0 ms |
| static-cap    |          81.2 ms |         46.3 |          100% |          0% |          16.0 ms |
| kv-aware      |       6,882.0 ms |         55.7 |           83% |         17% |         209.4 ms |

The TPOT column is new and it is why this table was regenerated twice. Its first printing came from a
percentile taken over unsorted samples and reported 15.8 ms for `off` and 16.0 ms for `kv-aware` -- a p99
below its own p50 in the kv-aware case, which no sorted percentile can produce. Its second printing was
correct but arm-wide, pooling every tenant, while the criterion below is about the protected one. Split by
tenant, at no additional spend, the column reads as above.

**This is the hardest bar in the whole design, and nothing has ever passed it.** Reading 1 fails a cell
whose premium TPOT p99 exceeds 1.25x the isolated baseline, which is 20.0 ms. Both arms that left the
contending tenant alive miss it by more than ten times, and the only arm that meets it is the one that
deleted the contender. On this workload, at this load, no configuration yet measured has kept the premium
STREAM healthy with a contender present -- and the guard that was supposed to protect improved it from
266.0 ms to 209.4 ms, a 21% gain on a metric that needed a 93% one.

That reframes what the run is looking for. "Protect the tail without deleting the tenant" was already the
question. The evidence says the stream is the harder half, and a cell that holds TTFT while leaving TPOT
at 200 ms has not protected anybody.

`static-cap` matched isolation's latency **and** isolation's throughput, because it did not make the engine
efficient — it served none of the long tenant's work. `off` served 24% more tokens than the arm that "won"
on tail.

**And that arm was broken, which is a different sentence and a smaller claim.** Its bucket was configured
with a burst below the contender's prompt size, so every eligible request was refused with 413 before any
rate limit applied — all 1,788 of them, in the recorded evidence. `hack/m5b-arms.sh` now refuses to start a
run in that state, and its message names this run as the one that produced it. So `static-cap` is not
evidence that deleting a tenant protects a tail. It is an arm that admitted nothing, and the useful thing
about it is what happened next: it produced a premium tail better than the isolated baseline's, and a
reading that looked only at tails would have called that the best result in the study.

An earlier draft of this paragraph said "every check the report made was a tail ratio, so this was
invisible for four paid runs". That is false, and it is worth correcting precisely because it flatters the
work that came after.

Of the three pre-registered checks, one was not a tail ratio. `admission match` compares the two contended
arms' admitted-work fractions, `|w_B − w_C| / w_C`, and it did not merely fire — it measured the deletion
exactly. `static-cap` admitted **0.000** of the eligible work against `kv-aware`'s 0.847, which is where
the reported 1.000 comes from. The run was declared INVALID and no protection claim was made.

What that check could not do is the thing this run is about. It gates the C-against-B comparison rather
than scoring `static-cap` as a candidate answer, so nothing contradicted the true and entirely misleading
sentence "static-cap held the premium tail at 81.2 ms, better than the isolated baseline's 82.2". And it
counts admitted INPUT tokens, not delivered output by tenant, so it cannot tell work that was discarded
from work that was admitted and then starved. Per-tenant output accounting is what closes that, and it
arrived afterwards rather than being part of the original design.

The question this run asks is therefore not "can the tail be protected". One arm did it by being
misconfigured into serving nobody, which demonstrates nothing about protection and a great deal about
reading tails alone. It is:

> **Is there a configuration that protects the premium tail without deleting the contending tenant's work,
> and what does it cost the tenant it protects?**

## Design

Engine-level, no gateway. The gateway's admission guard is not an arm here: its own evidence says it cannot
observe the pressure it gates on, and a milestone that adds a control plane on top of a misconfigured engine
is measuring the wrong layer. What a gateway adds on top of a *correctly configured* engine is a later
question, and this run is what would make it askable.

**Factors.** Batch budget `max_num_batched_tokens` ∈ {256, 512, 1024, 2048} × scheduling policy ∈ {fcfs,
priority}. Eight cells.

The first version of this paragraph said the microtest found the interaction "non-monotone — 1.57x at 512,
4.85x at 2048, 11.83x at 8192". Those three numbers are the priority column alone, and that column is
monotone. Both columns are, in opposite directions:

| max_num_batched_tokens |    fcfs | priority |
| ---------------------: | ------: | -------: |
|                    512 | 13.790x |   1.566x |
|                   2048 | 12.485x |   4.846x |
|                   8192 | 11.812x |  11.826x |

So the result is not a property of the budget. It is an interaction: `priority` is worth nothing at 8192
and worth roughly eight times at 512, while `fcfs` is uniformly bad and, if anything, mildly better at
larger budgets. 8192 is dropped because both policies fail the tail criterion there, which is true and is a
different sentence from the one this document used to make. 256 is added below the only cell that met 2x.

**This sweep is a published technique, and the document should say so.** The engine's own startup log reads
`Chunked prefill is enabled with max_num_batched_tokens=N`, so `max_num_batched_tokens` is the chunk size of
Sarathi-Serve's chunked prefill (arXiv:2403.02310, OSDI'24), whose entire subject is the throughput-latency
tradeoff this run is measuring. The direction observed — shorter units of work give a waiting request an
earlier scheduling opportunity — is what that work predicts. Where this run is not covered by it is the
per-tenant question: Sarathi measures the tradeoff, not who pays for it.

The mechanism is stated as an inference, not a finding. Timings alone do not establish where this engine
reorders or preempts, and under concurrent load the budget also governs batch composition, so it is not
purely one request's chunk size.

**Load.** Not the microtest's two requests. Two offered loads, both open-loop arrival processes with many
concurrent prefills, since the two-request result cannot speak to a p99:

- **L1 — the 2026-09-03 trace**, unchanged, so this run is comparable to the paid evidence above.
- **L2 — the same trace at half the noisy arrival rate**, to separate "this configuration protects" from
  "this configuration only protects when the offered load is what it happens to be".

**Control.** `default-fcfs`: the engine launched with the deployment's other arguments, `fcfs`, and **no
budget flag at all** — what an operator who configures nothing gets. It is the arm every other cell must
beat, and it is not the isolated baseline: R1 is measured too, as the ceiling.

The first version of this paragraph said "at the engine's own default budget" as though that number were
known. It is not. Every microtest cell passed the flag explicitly, so nothing in the evidence establishes
what this pinned image defaults to, and vLLM has shipped 512, 2048 and 8192 as the default across versions.
Naming a number here would have been a guess, and picking 2048 because it looks like a default would have
silently made the control one of the treated cells.

So the control is defined by a procedure rather than a value: omit the flag, and record what the engine
resolves. The pilot captures the engine's own startup configuration for this arm and the run reports the
resolved budget B₀ beside every number. If B₀ turns out to equal one of the swept budgets, the two are the
same configuration and the run must say so rather than report them as separate cells.

The arm is named `default-fcfs` and not `off`. The readings below use the word `off` in places because that
is what the first draft called it; every such use means this control. M5-b has an arm called `off` that ran
through a gateway, and evidence from the two must never be pooled — which is now enforced by a study
identifier carried on every raw row rather than by the arm name.

**Repetitions.** 3 per cell per load, and the interval this produces is the observed range, not a 95%
interval.

The first version of this paragraph claimed 3 is "the floor at which the 2.5/97.5 percentiles are not simply
the range". That is false. The bootstrap resamples repetitions with replacement and reports percentiles of
the resampled mean, so with n=3 there are 27 equally likely resamples and each extreme — all-minimum,
all-maximum — carries probability 1/27 = 3.70%. Since 3.70% > 2.5%, the 2.5 and 97.5 percentiles land inside
those extremes: the printed interval **is** min-to-max.

3 is kept anyway, because the cost of the alternative is a run this study cannot afford and because a range
over three repetitions is still worth having. But it is reported as a range, and no sentence in the write-up
may call it a 95% interval. A reader deciding on this evidence needs to know that the interval is the
weakest statement the data supports, not the strongest.

## What every arm must report

Both halves, for every cell, or the cell is not scored:

1. premium TTFT p50/p95/p99, with the tail sample size beside it
2. **aggregate output tokens per second, over the summed per-repetition spans**
3. **output tokens by tenant** — the share that separates protection from deletion
4. **TPOT p50/p99** — a guard that protects the first token and wrecks the stream after it must not pass
5. the engine's resolved startup configuration, captured from its own log, not from the flags we passed
6. **per-tenant work accounting**: completed responses, admission rejections, client-side timeouts, streams
   that broke after their first token, and requests still outstanding when the measurement window closed

Items 2–4 did not exist for the first four paid runs. Item 6 is new in this revision and it is what makes
reading 2 decidable: without it, a smaller share is equally consistent with deleted work, delayed work and
starved work, and the run would have to guess which.

Item 4 is also new in a second sense. TPOT existed but was computed over unsorted samples, so its first
printing was not a tail at all — on the already-paid evidence it reported 16.0 ms for an arm whose real p99
is 265.1 ms. The measurement is fixed, the paid evidence is re-scored above, and both the sorting and the
exclusion of broken streams from the TPOT sample are fault-injected.

## Pre-registered readings

**Reading 4 (INVALID) is evaluated first.** If the load produced no contention, every other reading is a
comparison between configurations under a workload with no problem to solve, and reading it first costs
nothing. After that, readings are evaluated in the order below against **L1**; L2 then decides whether the
finding is a configuration or a coincidence. The first reading that fires is the answer. The readings are written so that each is decidable
from the table above without reference to any other, which is the defect that made the microtest's
pre-registration unusable.

### 1. Protection without deletion — POSITIVE, and this is the deliverable

A cell fires this if **all four** hold against the `fcfs`-at-default control:

- premium TTFT p99 at or below **2x** the isolated R1 baseline, and
- noisy tenant's output-token share at or above **75% of its share under `off`**, and
- aggregate output tok/s at or above **95% of `off`'s**, and
- premium TPOT p99 no more than **1.25x** R1's.

The first two are the point: a tail held without the contending tenant losing its work. The third and fourth
exist because "protection" that costs a quarter of the machine's throughput, or that pays for a fast first
token with a slow stream, is a different trade being reported as the same one.

If more than one cell fires, the answer is the cell with the highest noisy share. Exact ties go to the
larger budget, because a larger budget is the smaller change from the control and this run has no evidence
about what any of these budgets costs to operate — the first draft broke ties toward the smaller budget on
exactly that unmeasured claim.

### 1b. Protection, bought with throughput — POSITIVE with a price attached

A cell fires this if it holds the p99 bar, the noisy-share bar and the TPOT bar, but delivers aggregate
output tok/s **below** 95% of the control's. Report the cell and the measured cost, as a percentage of the
control's throughput, in the first sentence of the write-up.

This reading exists because the first draft had no place to put it. A cell that protects the tail, leaves
the contending tenant's work intact, keeps the stream healthy and costs 8% of the machine's throughput
fails reading 1 on the throughput clause alone — and fires none of the others, because reading 2 requires
the noisy-share bar to FAIL and reading 3 requires the cell not to beat the control. The run would have
produced its best possible result and had no way to say so.

It is separated from reading 1 rather than folded into it because 95% is an acceptance threshold, not a
measurement: a configuration that costs 5% and one that costs 30% are different products, and a single
pass/fail line would report them identically.

### 2. Protection only by deletion — NEGATIVE, and M5-b closes here

This reading requires a nonempty set of cells that met the p99 bar. If no cell protected the tail at all,
the answer is reading 3, not this one.

**Deletion must be shown, not inferred from a smaller share.** A reduced noisy share is consistent with work
that was discarded, work that was delayed past the measurement window, and work that was starved but still
queued — and those are three different findings, only one of which is deletion. So this reading fires only
when the evidence separates them: the run records, per tenant, completed responses, admission rejections,
client-side timeouts, streams that broke after their first token, and requests still outstanding when the
window closed. Attributing a share loss to deletion without those counts would be describing a quantity by
a cause its ledger does not establish, which this repository's measurement rule forbids.

If every cell that meets the p99 bar fails the noisy-share bar **and the accounting shows the missing work
was rejected or discarded rather than delayed**, then on this model, this GPU and this load,
holding the premium tail requires discarding the contending tenant's work — which the gateway's static cap
already does, more simply, at the control plane. There is nothing an engine configuration adds, and the
milestone's finding is that the protection/throughput trade at this scale is not a control-plane problem.

That is a publishable negative with a measured explanation, and it is the second-best outcome here, not a
failure of the run.

### 3. No cell beats the control — INCONCLUSIVE, do not spend again on this design

If no cell improves premium p99 by more than the control's own repetition-to-repetition spread, the factors
do not have a lever on this load. The next step is a different load or a different engine, not another
sweep of the same two knobs.

### 4. The load did not create contention — INVALID, no card time on this trace again

If the control's premium p99 is under 5x R1's, the trace is not producing the interference the run exists to
study, and every cell is comparing configurations under a load that has no problem to solve.

## L2's role, stated in advance

L2 does not get its own readings. It answers one question about whatever L1 concludes: **does the winning
cell still fire reading 1 at half the noisy arrival rate?** If yes, the finding is a configuration. If no,
the finding is that the configuration is tuned to one offered load and the write-up says so in its first
sentence.

## Budget

| stage                                                                      |    cost | gate                                     |
| -------------------------------------------------------------------------- | ------: | ---------------------------------------- |
| harness development                                                        |      $0 | done — `stub-serve`, re-scored evidence   |
| pilot: R1, `default-fcfs`, `mbt-0512-fcfs`, `mbt-0512-priority`, L1, 1 rep |   $1.30 | reading 4 must not fire, and B₀ resolved |
| confirmatory: 8 cells + control + R1 × 3 reps, L1                          |   $3.45 | readings evaluated                        |
| L2: winning cell + control, 3 reps                                         |   $3.45 | only if reading 1 or 1b fired             |
| unspent reserve                                                            |   $2.45 | —                                         |

A standalone Spot g5.xlarge in a public subnet measured $0.65/h effective, with no NAT data charge.

The pilot's scope grew in this revision and **the $0.65 figure has not been re-derived for it**. The first
draft costed a pilot of "control + the 512/priority cell", but the control was undefined and neither R1 nor
a matched `fcfs` cell at the same budget was included — so the pilot could not have resolved B₀, could not
have evaluated reading 4 (which compares the control against R1), and could not have separated the budget
effect from the policy effect. Four arms at one repetition is the smallest pilot that can do those three
things. Before it is bought, one of two things must happen: a runtime estimate that shows four arms fit the
hour, or an explicit decision to buy two hours. **Do not treat $0.65 as this pilot's price.**

### The estimate, and what it decides — 2026-09-07

**Four arms do not fit the hour. The pilot is two hours, $1.30.**

The one measured input is the per-arm replay time at L1, and it is measured precisely because L1 *is* the
2026-09-03 trace. That run wrote a manifest at the start of every arm-repetition, and the sixteen intervals
between them are **10 to 11 minutes each**, stable across all four arms — the isolated arm and the
uncontended one take the same wall time as the contended ones, so arm type does not move this number.

Four arms at one repetition is therefore **about 44 minutes of replay before anything else happens**, and
three things are added on top that the 2026-09-03 run did not pay:

- **Four engine restarts.** `hack/m5b-arms.sh` held one engine across all its arms; this study varies
  `--max-num-batched-tokens` and `--scheduling-policy`, so `hack/m5b-price-of-protection.sh` does
  `docker rm -f vllm` and a fresh `docker run` per arm and then waits on `/health`. That wait is allowed
  900 s. A warm restart from the mounted `/hf` cache has never been timed in this repository, so this
  document does not claim a figure for it — but any credible value is minutes, and four of them.
- **The first arm's weight download.** A fresh Spot instance has an empty `/hf`, so arm one pays a full
  model fetch. `hack/m5b-gpu-session.sh` says as much where it waits: *"weights download and memory
  profiling take minutes"*.
- **Session setup** — instance boot and the pull of the digest-pinned vLLM image, which is large.

44 minutes of replay leaves 16 minutes for four engine restarts, a model download and the whole bring-up.
That does not close under any restart time worth writing down, so the estimate does not need one: the hour
is already spent. **Buy two.** At the measured $0.65/h that is $1.30, and it comes out of the $2.45 reserve
without touching the confirmatory or L2 lines.

This is an estimate of wall time, not a promise. If the pilot overruns two hours the run is stopped and
reported as stopped, in the same way `qlgpu-20260906-103327` was.

The confirmatory line also now names its control and ceiling explicitly. The first draft's "8 cells × 3
reps" did not include them, and neither reading 1 nor reading 3 can be evaluated without both.

## What this run cannot say

It measures one model on one GPU at one prompt-size distribution. It cannot say the winning budget is the
right budget elsewhere: the microtest's budget effect was strongly conditional on the scheduling policy —
eightfold under `priority`, nothing under `fcfs` — so a budget chosen here does not transfer to a deployment
whose policy, model or prompt distribution differs. It also measures no gateway, so it says nothing about
what a control plane adds — it only makes that question askable against a baseline that is not
misconfigured.

It also cannot say what any of this costs to operate. Every cost in this document is throughput and tenant
share measured on one card. Nothing here measures memory, tail behaviour at other concurrencies, or the
operational burden of running differently-configured replicas per tenant class.

## What was revised, and why

Recorded because a pre-registration that is quietly edited is not a pre-registration. All of these were made
before any card time was bought.

| # | what changed                                             | why                                                                                          |
| - | -------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| 1 | the factor rationale                                     | the three quoted ratios were the priority column alone, and that column is monotone           |
| 2 | prior work named                                         | the factor is Sarathi-Serve's chunked-prefill chunk size, which the document did not say      |
| 3 | control defined by procedure, renamed `default-fcfs`     | no evidence establishes this image's default budget, and `off` is an M5-b arm name            |
| 4 | repetition claim corrected                               | with n=3 the bootstrap's 2.5/97.5 percentiles are the observed range, not a 95% interval      |
| 5 | reading 1b added                                         | a cell that protects at a throughput cost fired no reading at all                             |
| 6 | reading 2 requires work accounting                       | a smaller share is equally consistent with deleted, delayed and starved work                  |
| 7 | reading 4 evaluated first                                | an invalid load makes every other reading a comparison with no problem to solve               |
| 8 | tie-break reversed                                       | the old one rested on an operating-cost claim this run does not measure                       |
| 9 | TPOT re-scored, pilot rescoped and re-costed             | the metric was computed over unsorted samples, and the pilot could not have resolved B₀       |
