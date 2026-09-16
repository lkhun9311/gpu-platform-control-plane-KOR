# Does reservation track use — pre-registration

Date: 2026-09-06 · Pre-registered **before** the hardware is rented. Nothing here may be edited once the
instance is running.

## Why this exists

The device-observation session answered its own first reading and could not answer its second.

    A-honor    reserved 20.643   observed 20.137   difference 0.506
    A-ignore   reserved 50.605   observed 50.480   difference 0.125
    floor 3.295 s per arm

Reserved GPU-seconds and observed device-seconds agreed to within a tenth of the run's floor. Read quickly
that says reservation is a good proxy for use. It is not evidence of it.

The trace workload computes continuously by construction: it launches a kernel, synchronises, and launches
again until its service ends. A card allocated to it is a card doing work, so agreement was **the only answer
that session could produce**. The reading exists for the opposite case — a tenant that holds a card and does
not use it — and the lab had no way to be one.

It has one now. A trace row can declare a duty cycle, the workload keeps its context and its allocation for
the whole of its service and computes for a known fraction of it, and it reports the fraction it actually
ran at in its own termination message. That report travels through the ledger into the record, and the
record is refused if the ledger does not support it.

So this session does not ask whether the two quantities happen to agree. It **plants a difference and asks
whether the instrument recovers it**.

## The experiment

One axis. Two arms. Everything else constant.

| | a1 | a2-borrow (the victim) | b1-owner |
| ----------- | ------ | ---------------------- | -------- |
| D-full | duty 1 | **duty 1** | duty 1 |
| D-quarter | duty 1 | **duty 0.25** | duty 1 |
| contract | honour | **ignore, in both arms** | honour |
| policy | | `reclaimWithinCohort: Any`, both arms | |

**The termination contract is the constant, and it is the ignoring one.** A honouring victim stops within
milliseconds of the preemption decision, so its hold is shorter than a scrape interval and the observer has
nothing inside it — the defect that invalidated a measured run in the previous session. The ignoring arm
holds its card for tens of seconds, which is many scrapes.

**The duty is on the victim alone.** Idling the owner or the co-tenant would change three manifests when one
is meant to differ, and their occupancy is not what any reading here is about.

**Dose:** grace-bounded only. **Repetitions:** three per arm, interleaved, six runs. Two per arm would give
each cell a spread of one number, which cannot be told from its own noise.

## What the arms should do, and why

Reserved GPU-seconds are `gpuCount * (stop - ready)` for the preempted attempt. Kubernetes allocates the card
for that whole interval and never consults the workload about it, so **the two arms should reserve the same
amount**. Observed device-seconds are what DCGM saw the card doing inside that same interval, and the victim
computes for a quarter of it in one arm and all of it in the other.

If that is right, the arms are a controlled test of whether the instrument can see the difference between a
card that is held and a card that is used.

## Pre-registered readings

Evaluated only against records `-require-device` accepted. A refused record is not evidence and is not
scored. The comparison is `queuelabrun -compare 'gpu-grace-bounded-D-*.json'`, which refuses to fold records
disagreeing on duty into one arm and prints a notice when the arms differ.

### 1. Reserved seconds agree across the arms — the control

Mean reserved GPU-seconds differ by **less than the sum of the two arms' floors**.

They should, because reservation does not consult the workload. If they do not, something other than the duty
moved between the arms and every reading below is about that instead. This is the reading that can invalidate
the session, and it is first for that reason.

### 2. Observed device-seconds separate by about four to one — the deliverable

Mean observed device-seconds in D-quarter are **near a quarter of D-full's**, and the two differ by **more
than the sum of the arms' floors**.

Method, fixed here so it cannot be chosen after the numbers are in. Observed device-seconds is the **mean
utilisation over the victim's samples inside the reserved interval, as a fraction, times that interval**,
taken from each record's own `deviceObservation`.

It is the mean utilisation and NOT the fraction of samples above zero, and the difference is the whole
reading. `DCGM_FI_DEV_GPU_UTIL` is NVML's percentage of the last sample period during which a kernel was
executing, so a workload computing for a quarter of every second reports about 25 on **every** sample rather
than 100 on a quarter of them. Counting busy samples would return a fraction near 1.0 for both arms and
report that the instrument saw nothing — which would be a conclusion about the method, published as a
conclusion about the hardware.

The previous session's samples say why this was worth catching before the run rather than after: 1,089 of
them read 98 and 1,552 read 0, with six values in between. There is no middle there because that workload
never idles. A quarter-duty victim lives in that middle, and only an integral can read it.

"About a quarter" is bounded: the workload's period is one second and its service is tens of them, so
boundary effects are a few percent, and DCGM's own collection interval is also one second. A ratio between
**0.15 and 0.40** counts as recovered. Outside that range the instrument saw a difference it could not size,
which is reading 4.

### 3. Reservation is not a proxy for use — the consequence

If 1 and 2 both hold, then two runs that reserved the same GPU-seconds used the card for amounts differing
by a factor near four, **measured**. That settles what the previous session could only leave open, and it
does so with a difference whose size was chosen before it was measured rather than discovered in the data.

The honest scope stays narrow: it says reservation and use come apart for *this* workload at *this* duty on
*this* card. It does not say by how much they come apart for anything else.

### 4. The instrument cannot size the difference — INVALID, and the session says so

If the observed seconds do not separate, or separate by a ratio outside 0.15–0.40, then DCGM's attribution
cannot resolve a card held at quarter duty on this driver and AMI. Publish that. It is a fact about the
instrument, and a more useful one than a number nobody can check.

A D-quarter run refused by `-require-device` for want of busy samples belongs here too, and is a statement
about the gate's sensitivity rather than about the workload: at a quarter of a fifty-second hold the card is
busy for roughly twelve seconds, and a gate that cannot see that is a gate that cannot measure idling.

That refusal is not expected, and the reason is worth writing down because it is the same fact the method
above turns on. The gate counts samples whose utilisation is above zero, and a quarter-duty card reports
about 25 rather than 0, so its samples are busy samples. The gate should pass comfortably. If it does not,
the exporter is reporting something other than what this document assumes it reports, and every reading here
is void rather than merely negative.

## Budget and the stop rule

At g5.12xlarge Spot, priced from the most expensive of the three zones offering it — $3.41 an hour at the
time of writing, against $3.36 in the cheapest. A budget written from the cheapest only holds if the launch
is lucky.

| stage | wall clock | cost | gate |
| ------------------------------ | ---------: | --------: | ------------------------------ |
| instance up, driver, cluster | 12 min | $0.68 | |
| preflight, the four checks | 5 min | $0.28 | all four must pass |
| six runs, interleaved | 14 min | $0.80 | each uploads before the next |
| evidence off the box, teardown | 4 min | $0.23 | |
| **total** | **35 min** | **$1.99** | |
| hard stop | 60 min | $3.41 | terminate regardless |

The instance backstop is 140 minutes, which is the belt-and-braces case where the local process dies before
its trap runs. That costs $7.96 and is not a budget, it is a bound.

This is cheaper than the observation session because it buys one dose, one node and six runs rather than two
doses and eight, and because the cluster it needs is one this repository has now brought up nine times.

## What this run cannot say

One card model, one driver, one AMI, one workload, one duty. It does not measure inference, it does not
compare sharing modes, and it says nothing about whether reservation tracks use for a workload whose idling
is bursty rather than periodic.

It also cannot make the reclaim session's figures wrong. Those runs measured what they measured; if this one
fires reading 3, the consequence is that their agreement between reserved and observed is a fact about a
continuously computing workload rather than a general property of reservation.

---

# Results

Appended 2026-09-06. **Nothing above this line was edited after the instance launched.**

Run `qlgpu-20260906-103327`, instance `i-019e274661589ff33`, g5.12xlarge Spot in ap-northeast-2a, from commit
`d2e38af`. Two runs of a planned six: the session stops on a failed run, and the second run failed.

**Reading 4 fired.** The instrument could not size the difference, and it could not see it at all.

## Reading 1 — the control. HOLDS.

| arm | reserved GPU-s | floor |
| --------- | -------------: | ----: |
| D-full | 50.488 | 2.284 |
| D-quarter | 50.520 | 3.111 |

A difference of **0.032 s** against a summed floor of 5.396. Reservation did not consult the workload, as
predicted. The experiment was controlled; what follows is about the observer and not about the arms.

## The workload did what it was told, and its own counter says so

| arm | kernel launches | kind / device / duty, as the container reported them |
| --------- | --------------: | ---------------------------------------------------- |
| D-full | 89,223 | `cuda-fma / ok / 1` |
| D-quarter | 22,093 | `cuda-fma / ok / 0.25` |

**22,093 / 89,223 = 0.2476** against a declared 0.25. The quarter-duty victim loaded its PTX, launched real
kernels through the CUDA driver, and did almost exactly a quarter of the work.

## Reading 2 — the observer saw none of it. REFUSED.

| arm | victim's utilisation collections |
| --------- | -------------------------------- |
| D-full | 98 on 50 of 52 |
| D-quarter | **0 on all 52** |

Not "about 25", which is what this document predicted and wrote down. Zero, on every collection, while the
card was executing 22,093 kernels.

These are collections, not scrapes, and an earlier version of this page said scrapes. The exporter collects
once a second (`config/dcgm-exporter/daemonset.yaml`, `-c 1000`) and serves a cached snapshot in between,
while the run scrapes twice a second (`deviceScrapeInterval`, `cmd/queuelabrun/main.go`). Every collection is
therefore read about twice, which is visible in the persisted series as values that repeat in pairs. The
verdict is unaffected — `internal/queuelab/device.go` counts distinct timestamps — and so are every ratio and
mean below. What was wrong was the amount of independent evidence claimed: the counts were doubled.

    RUN INVALIDATED: the device held by Pod 0c99a041-... was observed working in 0 of 101 samples
    across the attempt; a card that is allocated and idle is the state this whole axis exists to
    distinguish from one that is computing

The exporter was working. In the SAME run it reported 98 for `a1`, the co-tenant, which computes
continuously — and in both runs it named every Pod on every card. This is not attribution failing. It is
attribution succeeding and reporting zero.

## What the evidence establishes, and what it does not

**Established:** a tenant that used an A10G for a quarter of every second, in bursts of a quarter second,
was reported by DCGM as using it not at all, on 104 consecutive samples, while the same exporter in the same
run correctly reported a continuously computing neighbour.

**Not established:** why. The leading explanation is that the two periods are equal by construction and
nothing here is measuring the phase between them. `internal/queuelab/submit.go` sets the workload's
`PERIOD` to exactly 1.0 second; `config/dcgm-exporter` passes `-c 1000`, a collection interval of exactly
1000 milliseconds. Two processes at the same frequency do not drift through each other's phase, so a sample
that lands in the 0.75 s idle window lands there again, and again. The drawing supports it: the victim's
card is 0 or 98 with no value between, in either arm, which is what an instantaneous read produces and not
what an average over a second would.

That is a hypothesis with a name and a test, not a finding. The test is cheap: give the workload a period
incommensurate with the sampler's -- 0.37 s, say -- and the zeros should break up. If they do not, the
explanation is somewhere else and this document should not have guessed.

## What it cost, and what it bought

14 minutes, about **$0.80** against a budgeted $1.99. The session stopped early because `gpu-session.sh`
halts on a failed run, which is the behaviour that kept it cheap.

It bought a sharper result than the one it was chasing. The question was whether reservation tracks use.
The answer here is that **this lab cannot currently tell**, and the reason is not that reservation and use
agree -- the workload's own counter separates them by a factor of four. It is that the observer this lab
trusts to see use reports zero for a tenant it can plainly attribute.

That is worth more than reading 3 would have been. Reading 3 would have said reservation and use come apart
for one workload on one card. This says the instrument used to establish `device-work-observed` on every run
in this repository has a blind spot, and that the blind spot is shaped like a duty cycle.

An earlier version of this paragraph went on to say that a run inside the blind spot is "refused rather than
mismeasured -- which is the one property that makes the refusal worth having". That claim does not survive
the second session, and withdrawing it matters more than the finding it was attached to.

At PERIOD = 1.0 s the sampler visits ONE phase of the workload's cycle and stays there, so what it reports is
decided by where that single phase falls. It fell in the idle stretch, and the run was refused. Had it fallen
inside the burst, every collection would have read 98, and a card computing at a quarter duty would have
passed `-require-device` with observed device-seconds close to reserved -- the plausible wrong number this
repository's rules put above every other failure. The refusal was a draw, not a property.

The second session is what shows this, because it makes the phase structure visible. At PERIOD = 2.6 s the
sampler steps through 13 phases spaced 0.2 s apart (13 collections = 5 periods), and the run's own edge
values drift monotonically as the workload's true period misses 2.6 s by a few milliseconds: 12, 18, 24, 30
across four cycles in `dq1`, and in `dq3` one edge climbing 68, 74, 79 while the other falls 78, 73, 67 with
their sum conserved near 146. Those numbers are a phase sweep. At 1.0 s there is no sweep, only one draw.

Counting the collections turned up a second defect, in the gate rather than in the instrument. `minBusySamples`
required two busy samples and the code counted them at distinct TIMESTAMPS, which the comment glossed as
"spaced across the interval". It is not the same thing when the observer scrapes faster than its source
updates: one cached collection arrives at two distinct timestamps, so **a single collection satisfied the
gate**, which is the "reading rather than a state" its own refusal message says it prevents. The gate now
requires busy samples to be at least `maxObserverGap` apart, which no single snapshot can span because the
exporter's collection interval is already required to stay under it. Replaying every paid series through the
new rule changes no verdict -- the thinnest run has nine separated instants against a threshold of two -- so
this closes a hole rather than revising a result. It would have mattered exactly in the blind spot, where one
phase landing inside the burst is the whole of the evidence.

## What this run cannot say

It observed one card model on one driver on one AMI, at one duty, with one workload period. It does not
measure how wide the blind spot is, and it says nothing about bursty workloads whose period is not one
second.

It does now establish the period hypothesis, which this sentence originally denied. The 13-phase lattice and
the drifting edge values described above are in the persisted series, and they are what a burst walking
through a sampler's phase produces.

What is still open is the sampler's averaging window. Fitting a rectangular burst against a backward
averaging window to each `dq` run, with period, duty and phase free, profiles like this (RMS in percentage
points, lower is better):

| window | `dq1` | `dq2` | `dq3` |
| -----: | ----: | ----: | ----: |
| 0.08 s | 1.88 | **1.63** | 7.84 |
| 0.15 s | **0.18** | 1.80 | 7.70 |
| 0.25 s | 4.08 | 5.04 | 7.84 |

`dq1` has a sharp minimum at 0.15 s, close to NVML's documented sample period. `dq2` is shallow and prefers a
smaller value. `dq3` is flat at about 7.7 everywhere, which means the model does not fit that run at all
rather than that its window is large -- an unexplained difference between runs of one arm. Three runs of the
same workload therefore do not agree, so **this page names no window.** It should be settled by an experiment
designed for it, not read off these three.

It also does not weaken the reclaim results. Those runs' victims computed continuously and were observed
computing continuously; nothing about a blind spot at 0.25 duty touches a measurement taken at 1.0.

---

# Results, second session

Appended 2026-09-06 after `qlgpu-20260906-125451`, instance `i-074815420581e90a8`, from commit `70d8c35`.
**Nothing in the pre-registration or in the first session's results was edited.**

The first session fired reading 4 and named a hypothesis: the workload's duty cycle had a period of exactly
1.0 second and the exporter collects every 1000 ms, so a sample landing in the idle part of the cycle landed
there every time. The document said that needed a test rather than a paragraph. The period is 2.6 seconds
now, and this is the test.

## The hypothesis was right

| | PERIOD = 1.0 | PERIOD = 2.6 |
| --------------------------- | ------------------: | -------------------: |
| D-quarter verdict | refused | **admissible** |
| device evidence | device-not-observed | **device-work-observed** |
| victim collections above zero | **0 of 52** | **15 of 51** |
| mean utilisation | 0 | **22.8** |
| values seen | 0 only | 98, 30, 24, 18, 12, 0 |

The zeros broke up and intermediate values appeared, which is what a burst walking through the sampler's
phase produces and what a phase-locked one cannot. Nothing else changed: same card model, same driver, same
AMI, same duty, same exporter configuration.

Six runs, none refused.

## Reading 1 — the control. HOLDS.

Mean reserved GPU-seconds: **51.084** (D-full) against **50.802** (D-quarter), a difference of **0.282 s**
against a summed floor of 5.814. Reservation did not consult the workload.

## Reading 2 — the deliverable. MET.

| arm | n | reserved | mean utilisation | observed device-s | launches |
| --------- | -: | -------: | ---------------: | ----------------: | -------: |
| D-full | 3 | 51.084 | 94.2 | **48.115** | 89,231 |
| D-quarter | 3 | 50.802 | 23.9 | **12.193** | 22,667 |

Observed device-seconds differ by **35.922 s** against a floor of 5.814, at a ratio of **0.253** — inside the
0.15–0.40 band this document fixed before the run.

The workload's own counter agrees without being asked to: 22,667 launches against 89,231 is **0.2540**, and
the observer's independent answer is 0.253. Two instruments that do not consult each other returned the same
quarter.

## Reading 3 — the consequence. ESTABLISHED.

Two sets of runs reserved the same GPU-seconds to within a twentieth of their floor and used the card for
amounts differing by a factor of four. **Reservation is not a proxy for use**, measured, with a difference
whose size was chosen before it was measured.

The scope stays where the pre-registration put it: this is one workload at one duty on one card model. It
does not say by how much reservation and use come apart for anything else. What it does settle is that they
CAN come apart while every reservation-based figure stays identical — which is what the first hardware
session could not decide, and what its agreement between the two quantities was mistaken for.

## Reading 4 — not reached this time, and that is the second finding

The instrument sized the difference. But the first session establishes something the second cannot unsay:
**a duty cycle synchronised with the sampler is invisible to it**, and invisible in a specific way —
attributed correctly, reported as zero, and refused rather than mismeasured.

The refusal is what makes that survivable. A gate that had reported 0% utilisation as a measurement would
have published a card doing 22,093 kernel launches as an idle one. It refused instead, and the run that
proved the point cost eighty cents.

## What it cost

| session | period | outcome | cost |
| ---------------------- | -----: | ---------------------------------- | ----: |
| qlgpu-20260906-103327 | 1.0 s | reading 4, stopped after two runs | $0.80 |
| qlgpu-20260906-125451 | 2.6 s | readings 1, 2 and 3, six runs | $1.35 |

$2.15 against a budgeted $1.99 for one session, for an answer plus the reason the first attempt could not
give one.

## What these runs cannot say

One card model, one driver, one AMI, one duty, two periods. They do not measure how wide the blind spot is —
only that 1.0 s is inside it and 2.6 s is outside. A workload whose idling is bursty rather than periodic is
not represented here at all, and neither is any duty between 0.25 and 1.

They also do not weaken the reclaim results. Those victims computed continuously and were observed computing
continuously; a blind spot at a synchronised quarter duty does not touch a measurement taken at full duty.
