# Observing the device — pre-registration

Date: 2026-09-05 · Pre-registered **before** the hardware is rented. Nothing here may be edited once the
instance is running.

## Why this exists

`queuelabrun -compare` prints a banner on every comparison this lab has ever produced:

    device: NOT OBSERVED -- every GPU-second below is a second of RESERVATION

That sentence is the largest caveat attached to the largest body of work in this repository — 16,731 lines
across `cmd/queuelabrun` and `internal/queuelab`, against 4,580 for the inference harness. It qualifies the
strongest result the lab has: over four interleaved runs on real Kueue, a tenant whose workload ignores
SIGTERM made the quota owner wait **29.0 seconds** longer to get its own quota back and left **30.0 more
GPU-seconds** unused, against a 5.906 second resolution floor.

Every one of those GPU-seconds is a second of *reservation*. Nothing observed a card.

The code to remove the banner is already written and has never been run:

| piece | where |
| --------------------------------- | ---------------------------------------------------- |
| refuses a run with no device evidence | `cmd/queuelabrun/main.go:261,347` (`-require-device`) |
| the observer's inputs | `-device-metrics` (DCGM `/metrics`), `-device-observer` |
| per-worker exporter routing, canary, session directory | `hack/gpu-session.sh`, 594 lines |
| the device loop itself | `internal/queuelab/submit.go:119` — PTX via `cuModuleLoadData` |
| the kernel's compile attestation | `hack/verify-ptx.sh`, targets sm_75 / sm_86 / sm_89 / sm_90 |
| the pre-spend gate | `cmd/queuelabrun/device_preflight.go` |
| the series read | `DCGM_FI_DEV_GPU_UTIL` |

This run buys the one thing none of that can produce on its own: a driver and a card.

**The workload is not a witness.** `internal/queuelab/device.go` states the rule this run has to satisfy:
the trace workload falls back to CPU arithmetic wherever the driver is absent, and its iteration counter
stays healthy while every operation runs on the CPU. Scheduling those Pods onto real hardware does not, by
itself, change the verdict. The observation has to come from something the workload cannot write to.

## What is being bought, and why it is not a $0.65 instance

The reclaim trace is three rows, each requesting one `nvidia.com/gpu`: `a1` holds one for the whole run,
`a2-borrow` takes a second beyond tenant A's quota, and `b1-owner` arrives to reclaim that second one. Two
Pods hold two distinct cards concurrently, which is why the protocol's gate demands `REQUIRED=2` devices on
the worker under test.

A single-GPU instance cannot run it. Time-slicing could advertise two devices from one card, and doing that
would void the run: under time-slicing DCGM cannot say whose work a busy SM was, and the exclusivity clause
refuses to attribute it. The point of this session is attribution.

AWS has no two-GPU instance in the G family, so the smallest that fits is a four-GPU one, with the surplus
two held by an occupier Pod as `hack/gpu-session.sh` already does.

**Amended after the quota case closed.** It was filed at 19:50 on 2026-09-05 and approved by 20:51: the
Spot G and VT quota is **48 vCPU**, up from 8. That was the decision this section deferred, and it is made
now — with one correction the quota did not fix.

| path | instance | cards | $/h | score | SCP | note |
| ------------------ | ----------------- | -------: | ----: | ----: | :-: | ---------------------------------------- |
| Spot, today | g5.2xlarge | 1 | ~0.45 | — | ok | **cannot run the protocol** |
| Spot, today | g4dn.12xlarge | 4 × T4 | 2.22 | **1** | ok | cheapest, and AWS says the capacity is not there |
| Spot, today | g6.12xlarge | 4 × L4 | 2.56 | 3 | **DENIED** | refused by `deny-instance-family` in every zone |
| **Spot, today** | **g5.12xlarge** | **4 × A10G** | **3.27** | **3** | ok | 48 vCPU, exactly the quota. `sm_86`, which the PTX targets |
| On-demand fallback | g5.12xlarge | 4 × A10G | 6.97 | — | ok | 48 vCPU against a 52 vCPU on-demand quota |

**A quota is not capacity, and capacity is not permission.** Two constraints, neither of which the other
knows about, and the plan changed twice.

The cheapest instance, `g4dn.12xlarge`, scores **1 out of 10** on Spot placement in every availability zone
of this region, which is AWS saying the request will probably not be filled. `g6.12xlarge` scores 3, so it
was chosen. **Every zone then refused it with `UnauthorizedOperation`**: the organisation's
`deny-instance-family` service control policy allows only `t3.*`, `g4dn.*` and `g5.*`, and an SCP is not
something an account administrator can override.

So the instance is **`g5.12xlarge`** — allowed by the policy, scoring 3 like the g6, 48 vCPU which is
exactly the Spot quota, and an A10G at `sm_86`, one of the four targets `hack/verify-ptx.sh` compiles the
kernel for. It is offered in three availability zones rather than the g6's two, all three scoring 3, so the
runner has one more zone to try before it gives up.

**The price is the measured one, not the list one.** Spot history at the time of writing reads $3.2696 in
`2a`, $3.2680 in `2c` and $3.1802 in `2d`. The table above carries the highest of the three, because a
budget written from the cheapest zone is a budget that only holds if the launch is lucky. It costs $3.27 an
hour rather than the g6's $2.56, and that difference is the price of a guardrail working, which is a good
reason to pay it.

**A score of three changes what the session must do.** A placement score of 3 is still low,
so this run should expect to be interrupted rather than merely tolerate it. Every artifact therefore leaves
the box **as each run completes**, not once at the end: an interruption after six of eight runs must cost
six runs' worth of nothing, and the original plan would have lost all of them. The occupier Pod holding the
surplus two cards is unaffected either way.

The 48 vCPU quota is exactly this instance's size, so nothing else in the G family can run beside it.

## What this session does NOT include

**M5-c is not in it.** The sharing matrix needs two vLLM engines on one card, and `hack/m5c-sharing-sizing.md`
records that a T4 gives each engine 3,652 MiB less than it needs — 284 KV tokens against a 7,695-token
contender prompt — so `SharingPlan.Validate` refuses it. It needs an A10G.

That is the smaller reason. The larger one is that the sharing modes require a time-slicing or MPS
device-plugin configuration, and the sizing page is explicit that the exclusive configuration is what
queuelab's device evidence depends on: a session that reconfigures it in place spends hardware to produce
"not attributable" on every run. The two studies want opposite things from the same component and do not
belong in one session.

## The run

One worker, four cards, two held by an occupier. Kueue is the admission engine, the operator provides
`MLTrainingJob` and the quota sync, and `hack/gpu-session.sh` prepares the exporter route and takes the
termination canary before any measurement.

**Factor.** One knob, the same one the kind study varied: whether the victim's workload honours SIGTERM.
Everything else — trace, dose, queue policy, cohort, worker — is identical, and `sameMechanism` refuses a
fixture that differs in any field defining the experiment.

**Arms.** `A-honor` and `A-ignore`.

**Dose.** Grace-bounded only. The kind study ran both regimes and the grace-bounded one produced its
strongest separation; buying the second regime doubles the bill for a comparison this run is not making.

**Repetitions.** Four per arm, interleaved, which is `hack/gpu-session.sh`'s own default and its stated
reason: with n=2 a cell's spread is one number and cannot be told from its own noise.

**Eight runs at roughly three minutes each**, plus cluster preparation and the preflight.

## The gate that runs before any measurement

Charged first because it is the failure that wastes the whole session, and it costs minutes.

1. `nvidia-smi` reports four cards and their memory, and the memory is what the plan assumed.
2. The real device plugin — not `config/device-plugin`, which is the kind cluster's fake one — advertises
   four `nvidia.com/gpu` on the worker.
3. A probe Pod runs the shipped PTX kernel through the CUDA driver and completes without
   `ptx-load-failed`, `ctx-failed`, `no-device` or `alloc-failed`.
4. `DCGM_FI_DEV_GPU_UTIL` scraped through the per-Pod forward shows non-zero utilisation on the card that
   probe Pod held, and zero on a card no Pod held.

**If any of the four fails, the session terminates and buys nothing else.** Item 4 is the one that matters:
the first three can all pass on a machine where attribution is still impossible, and attribution is the
deliverable.

## Pre-registered readings

Evaluated against records `-require-device` accepted. A record it refused is not evidence and is not
scored.

### 1. The banner comes off — POSITIVE, and this is the deliverable

Every run in the set carries a device observation from a source the workload cannot write to, and
`queuelabrun -compare` prints its comparison **without** the `device: NOT OBSERVED` line.

The reported quantity changes meaning with it: GPU-seconds become observed device-seconds rather than
seconds of reservation. That is the whole purchase.

### 2. Reservation and occupancy disagree — POSITIVE, and more interesting than reading 1

The kind study's numbers are reservation. If the observed device-seconds differ from the reserved
GPU-seconds by more than the run's own floor, then reservation was never a proxy for use, and every figure
this lab has published — including the 30.0 GPU-second difference between the arms — described something
other than what it was read as.

Report both quantities side by side for both arms. Do not replace one with the other.

### 3. The arms still separate — the result survives its own instrument

The A-honor and A-ignore difference in owner wait, measured on real hardware, remains larger than the sum
of the two arms' floors. The kind study measured 29.0 seconds against 5.906.

A smaller separation is a finding, not a failure: it would mean the kind result was partly an artifact of
the fake plugin's scheduling, which is exactly what an unobserved device leaves open.

### 4. Nothing is attributable — INVALID, and the session says so

If DCGM cannot attribute utilisation to a Pod on this driver and AMI, no reading above is decidable. Record
what was tried, publish the negative, and do not re-buy this instance type for this purpose.

## Budget and the stop rule

At the g5's measured Spot price of $3.27 an hour:

| stage | wall clock | cost | gate |
| ------------------------------ | ---------: | ----: | ------------------------------- |
| instance up, driver, cluster | 25 min | $1.36 | — |
| preflight, the four checks | 10 min | $0.55 | all four must pass |
| eight runs, interleaved | 30 min | $1.64 | `-require-device` accepts each, and each uploads before the next begins |
| evidence off the box, teardown | 10 min | $0.55 | — |
| **total** | **75 min** | **$4.09** | |
| hard stop | 120 min | $6.54 | terminate regardless |

`MAX_SPOT_PRICE` stays at $3.90, which is above all three zones' current price and below the on-demand rate,
so a price spike loses the instance rather than quietly buying it at any price.

**The hard stop is a timer, not a judgement.** At $3.27 an hour a session that is going badly costs five
cents a minute to keep thinking about, which is cheap enough to be tempting and is exactly why the limit is
written down before the instance exists.

The on-demand fallback is $6.97 an hour and doubles every figure above. It is not authorised here. If Spot
will not fill, that is a decision to take again rather than a fallback to reach for at 2am.

Every artifact leaves the box before teardown. An instance terminated with the only copy of its records on
it has spent the money and bought nothing, which is the same outcome as reading 4 and more annoying.

## What this run cannot say

It observes one card model — an A10G — on one driver on one AMI. It does not measure inference, it does not compare
sharing modes, and it says nothing about whether reservation tracks occupancy for any workload other than
this kernel.

It also cannot make the kind study's twelve records retroactively device-observed. Those runs measured what
they measured. If reading 2 fires, the honest consequence is that the earlier figures are relabelled as
reservation rather than corrected — they were never wrong about reservation.

---

# Results

Appended 2026-09-06. **Nothing above this line was edited after the first instance launched.** The
pre-registration stands as written; this section reports against it.

Run `qlgpu-20260906-032038`, instance `i-04f8cbf7c116a8ea1`, g5.12xlarge Spot in ap-northeast-2c, from commit
`e56f7a9`. Eight runs, four per arm, interleaved, grace-bounded dose. All eight were accepted by
`-require-device`.

## Reading 1 — the banner comes off. MET.

`queuelabrun -compare` prints its comparison with **no** `device: NOT OBSERVED` line. Every run carries
`validity.deviceEvidence: device-work-observed` from a source the workload cannot write to: a DCGM exporter
naming the Pod's UID on the card it held.

The reported quantity changes meaning with it. GPU-seconds in this session are observed device-seconds, not
seconds of reservation.

## Reading 2 — reservation and occupancy AGREE. Does not fire.

| arm | reserved GPU-s | observed device-s | busy fraction | difference |
| ---------- | -------------: | ----------------: | ------------: | ---------: |
| A-honor | 20.643 | 20.137 | 0.976 | 0.506 |
| A-ignore | 50.605 | 50.480 | 0.998 | 0.125 |

The pre-registered test was whether the two differ by more than the run's own floor. The floor is 3.295 s per
arm; the differences are 0.506 and 0.125. They do not.

Method, because this reading is an analysis rather than something the tool certified: reserved is the record's
own `wastedGPUSeconds`, which is `gpuCount * (stop - ready)` for the preempted attempt. Observed is that same
interval scaled by the fraction of the victim's samples in it that showed the card working, from the record's
own `deviceObservation`. Sampling was about every half second -- 41 samples across A-honor's 20.5 s window and
101 across A-ignore's 50.5 s -- so the fraction is an estimate with roughly half-second granularity, which is
well below the difference it would have taken to fire.

**So the earlier figures were not misdescribing this workload.** The kind study's GPU-seconds were
reservation, and for this kernel reservation tracked use. That is a narrower statement than "reservation is a
good proxy": the trace workload computes continuously by construction. It says nothing about a workload that
holds a card and idles, which is the case the distinction exists for.

**Answered on 2026-09-06.** A workload that holds a card and idles was built, and it was measured: two sets
of runs reserved 51.084 and 50.802 GPU-seconds -- the same to within a twentieth of their floor -- while
using the card for 48.115 and 12.193 observed device-seconds. Reservation and use come apart by a factor of
four with every reservation-based figure unchanged. See
[does reservation track use](2026-09-06-does-reservation-track-use.md); this paragraph's caution was the
right one, and the successor is what settles it.

## Reading 3 — the result survived its own instrument. MET.

| | kind cluster, fake plugin | this session, four A10Gs |
| ------------------------- | ------------------------: | -----------------------: |
| owner wait difference | 29.0 s | **28.8 s** |
| GPU-second difference | 30.0 | **30.0** |
| resolution floor | 5.906 s | 6.546 s |

    A-honor   n=4  waste mean=20.643  ownerWait mean=2.862   min=2.488  max=3.658
    A-ignore  n=4  waste mean=50.605  ownerWait mean=31.706  min=31.582 max=31.823

Both differences remain larger than the sum of the two arms' floors. The kind result was not an artifact of
the fake device plugin's scheduling, which is what an unobserved device had left open.

## Reading 4 — not reached. Attribution worked.

DCGM attributed utilisation to Pods on this driver and AMI. Across the session's own sampling, 34 Pods were
named; the two cards the protocol used were busy in 541 and 559 of 663 samples, and the two held by the
surplus occupier were busy in 0 of 663 -- allocated and idle, which is what the occupier is for and what an
exclusivity clause needs to be able to see.

## What it cost, and what went wrong on the way

Nine sessions, about $3.90 against a pre-registered $4.09. Eight of them bought a defect rather than a
measurement, and the pre-registration's own claim -- that the preflight gate is charged first because it is
the failure that wastes the session -- is what kept each one cheap.

The one worth recording is the eighth. `-require-device` selected the victim's samples over the HOLD, which is
the owner's admission to the victim's stop. In the arm that honours SIGTERM that window was 216 milliseconds
while the exporter scrapes about once a second, so it could not contain a sample, and the run was refused
while carrying 39 busy samples of exactly the Pod it was asking about. The gate was asymmetric by
construction: the ignoring arm computes through a thirty-second hold and passes trivially. Half the
experiment could never have been valid, and the contrast between those halves is the result. The USE question
now reads the attempt; exclusivity and continuity still read the hold.

## What this run still cannot say

Everything the pre-registration already listed, unchanged: one card model, one driver, one AMI, no inference,
no sharing modes. And one thing it can now say more precisely -- reservation tracked occupancy **for a kernel
that never stops computing**, which is the easiest case for them to agree.
