# The first admissible queuelab result

The runner has existed for a long time without producing a number its own gates would accept. Every earlier
attempt was refused, and the refusals were the point: a run that cannot prove it held its worker exclusively,
qualified its environment, and observed continuously must not publish a figure. This is the first run where
all four claims held.

    verdict: admissible-under-implemented-gates
    failures: []

Twelve runs, two per arm in each of two dose regimes and on each of two workers, on a three-node kind cluster with a fake
`nvidia.com/gpu` device plugin.

**These are not the runs this page first reported, and the reason is worth recording.** The original four
were written at record schema 10, before the harness could measure its own resolution at all: their events
carry no kubelet timestamp, so the 0.4-2.4 second bound quoted for them below came from two OTHER runs taken
six hours later. Two schema bumps since then mean no build in this tree can decode any of them. This page
claimed the figures could be re-derived from the files rather than taken from the page, and for those files
that had become false. They were re-run on a build that can read them back.

**These twelve are the second generation of this record set, and the first was replaced rather than
re-interpreted.** Commit `1af01be` deleted twelve `e16-*` records in the same commit that added these,
because the workload changed underneath them: until then it never reached for a device at all, so a run on
real hardware would have come back byte-identical to a run on kind. Records produced by a different
workload describe a different experiment and were not worth keeping beside these.

That is worth stating because `ex/` shows twelve files and no history, and a reader entitled to ask whether
this is simply the take that came out clean cannot answer it from the directory. The refusal gates select on
PROCEDURE rather than outcome — a run is refused for failing to qualify its worker, not for producing an
inconvenient number — so a re-take cannot be steered toward a result. But nothing in the record set records
how many takes there were, and the honest answer is: this is the second, the first was superseded by a
workload change, and the schema-10 originals before it are the ones the paragraph above describes.

## What the two arms differ in

One knob: whether the victim's workload honours SIGTERM. Everything else — the trace, the dose, the queue
policy, the cohort, the worker — is identical, and `sameMechanism` refuses to adopt a fixture that differs in
any field that defines the experiment.

The trace is three rows. `a1` and `a2-borrow` belong to tenant A, which borrows beyond its nominal quota;
`b1-owner` belongs to tenant B and arrives to reclaim what it owns partway through the victim's service -- at t=24s in the grace-bounded regime and t=44s in the self-completing one, since the arrival is placed relative to the dose rather than fixed. Eight of the twelve runs are grace-bounded, so the single figure this line used to quote described the minority. Kueue preempts `a2-borrow`
under `reclaimWithinCohort`.

## Result

| dose | arm | run | node | owner: arrival / stamp | discarded GPU-s | that run's floor |
|---|---|---|---|---|---|---|
| self-completing | A-honor | e17sh1 | worker | 1.208 / 1.000 s | 40.871 | 2.489 s |
| self-completing | A-honor | e17sh2 | worker | 3.210 / 3.000 s | 40.888 | 3.106 s |
| self-completing | A-ignore | e17si1 | worker | **19.209 / 19.000 s** | **0.000** | 2.473 s |
| self-completing | A-ignore | e17si2 | worker | **19.222 / 19.000 s** | **0.000** | 3.050 s |
| grace-bounded | A-honor | e17gh1 | worker | 2.177 / 2.000 s | 20.901 | 2.446 s |
| grace-bounded | A-honor | e17gh2 | worker | 2.184 / 2.000 s | 20.888 | 2.430 s |
| grace-bounded | A-ignore | e17gi1 | worker | **31.219 / 31.000 s** | **50.901** | 3.338 s |
| grace-bounded | A-ignore | e17gi2 | worker | **31.206 / 31.000 s** | **50.906** | 3.460 s |
| grace-bounded | A-honor | e17wh1 | worker2 | 2.171 / 3.000 s | 20.892 | 3.156 s |
| grace-bounded | A-honor | e17wh2 | worker2 | 3.143 / 3.000 s | 20.917 | 3.414 s |
| grace-bounded | A-ignore | e17wi1 | worker2 | 31.233 / 31.000 s | 50.870 | 3.056 s |
| grace-bounded | A-ignore | e17wi2 | worker2 | 31.192 / 32.000 s | 50.922 | 3.190 s |

**Twelve runs, all admissible, no failures, and all three factors interleaved** — arm, dose regime and node
alternate through the sequence, so nothing in the comparisons below carries a confounding warning. The
cluster's occupancy is held fixed for the whole set on both workers, which the previous node comparison did
not do: the second worker could not be held exclusively until the platform's own serving workload was scaled
away, so the node and the occupancy varied together and a node result was also an occupancy result.

**The owner's wait is read on two clocks.** The left figure is a difference of watch ARRIVAL times; the right
is a difference of the two components' own transition stamps — Kueue's Admitted and the kubelet's Ready. Look
at the four ignoring grace-bounded runs: the arrival figure scatters across 41 ms over two nodes while the
stamp figure is 31.000 on three of them and 32.000 on the fourth — a whole second appearing from a 41 ms
spread, because the truncation boundary happens to fall inside it. **The scatter is watch jitter and the
stamp's jump is quantisation.** An earlier version of this page said 313 ms and "31.000 on every one",
which were true of a record set that has since been replaced twice; the figures here are computed from the
twelve records this page cites.

The stamp figure is quantised to the second, which is why the honouring runs flip between 2 and 3 — their
true value sits near a boundary. It carries no watch lag at all; it carries truncation at each end plus the
offset between two components' clocks, which is constant for a pair of machines and cancels in an arm
difference on ONE node. It does not cancel between nodes.

**The floors are two to three times what this page once claimed**, because the bound was `max(spread,
quantisation)` where it should have been their sum, and because it was sampled only at container stops while
the interval above runs from an admission to a readiness. Neither endpoint had ever been compared against the
component that produced it.

## The conclusion is an artifact## The conclusion is an artifact## The conclusion is an artifact

The runs were always reproducible. The SENTENCE they were gathered to support lived in this file and in
arithmetic typed at a shell, so nobody could re-derive it from anything. `queuelabrun -compare` now reads a
named set of records and answers the three questions those records can settle:

    $ queuelabrun -compare 'ex/e17-grace-bounded-*-e17g??.json'
    ===== COMPARISON (dose grace-bounded) =====
    worstRunFloor=3.460s -- orientation only; each finding below is tested against the SUM of its two
      arms' floors, because a difference carries both of their errors
    interleaved: the arms alternated in time
    no effect is claimed: this tool reports resolution and confounding, not inference
    device: NOT OBSERVED -- every GPU-second below is a second of RESERVATION
      A-honor   n=2 waste mean=20.895 min=20.888 max=20.901 runs=e17gh1,e17gh2
        ownerWait mean=2.180 min=2.177 max=2.184 over 2 of 2 runs
      A-ignore  n=2 waste mean=50.904 min=50.901 max=50.906 runs=e17gi1,e17gi2
        ownerWait mean=31.213 min=31.206 max=31.219 over 2 of 2 runs
      [wastedGPUSeconds] A-honor discarded 20.9 GPU-s and A-ignore discarded 50.9, a difference of
        30.0 s against a 5.906 s floor, over n=2 and n=2 runs
      [ownerAdmitToReadySeconds] the quota owner was running 2.180 s AFTER KUEUE ADMITTED IT under
        A-honor and 31.213 s under A-ignore -- a difference of 29.0 s against a 5.906 s floor

It refuses rather than excludes: a record whose own gates failed, a second dose regime, a single arm — each
stops the comparison instead of quietly shrinking it, because a document whose file list does not describe
its evidence is the reproducibility it exists to provide. Returning "not resolved" is its main job, not its
failure mode.

**The grace-bounded difference is the strongest number this lab has produced.** Thirty seconds of discarded
work between the arms and 29.0 seconds of the owner's waiting, against a 5.906 second floor, over four
interleaved runs.

**This sentence used to end "that difference IS the termination grace period, recovered from the experiment
rather than assumed by it", and that was false.** The grace period is not recovered here.

**The correction that replaced it was also false, and a hostile review caught it.** It said
`internal/queuelab/trace.go` sets `terminationGraceSec` on the Pods. It does not. `grep -rn
TerminationGracePeriodSeconds --include=*.go` finds no setter on the victim's path at all — only the
termination canary's own probes and a read-only cap check in the admission webhook. `trace.go`'s constant
says so in its own comment: *"mirrored here so the trace can reason about the worst-case restoration
window."* The victim runs on the **API server default**, which is thirty seconds.

So the mechanism is one step less direct than either sentence claimed, and the arithmetic is unchanged: the
default equals the constant the harness assumes, `cmd/queuelabrun/spine.go` builds the horizon and the
regimes from that assumption, and a difference landing near thirty seconds is the harness reading back a
value it did not set but did assume. What the runs support is that the difference is **consistent with** the
grace period in force and far larger than the floor. `hack/queuelab-grace-boundary.md` had it right the
whole time — "the Kubernetes default, 30 seconds, verified per node by a canary" — and two committed
documents disagreeing about the provenance of the one constant the result reads back is the defect, not the
number.

**And the magnitude was in hand before the first run.** Every record's qualification block carries the
termination canary's own measurement: `ignoreStoppedAfterMs` of 31,043 to 31,193, taken at `07:40:27Z`
against runs that started at `09:15` and later. A pod that ignores SIGTERM outlasting its deletion by
thirty-one seconds was therefore measured by the gate an hour and a half before the experiment measured the
owner waiting thirty-one seconds for the device it held.

That is not a coincidence and it is not a discovery. What the runs add to the canary is narrower and worth
stating exactly: the reclaiming owner's wait is that *whole* contract — nothing releases the device early,
nothing overlaps, and Kueue admits the owner about 0.1 s after the preemption decision so the wait is spent
against the device rather than in the queue. The self-completing regime adds the other half, that a victim
which finishes first releases first. Those two sentences are the result; the thirty seconds is the setting.

And it is no longer only a difference. `-mode model` turns the claim into arithmetic and tests both regimes
against one rule:

    $ queuelabrun -compare 'ex/e17-self-completing-*.json,ex/e17-grace-bounded-*-e17g??.json' -mode model
    ===== MODEL: held = min(remaining service, grace), tested on the DEVICE HOLD =====
    the device hold in every regime is CONSISTENT WITH held = min(remaining service, 30 s grace), to
    within the 2.940 s floor and with nothing subtracted from anything. The honouring arm's hold over
    4 runs measures 0.041 s, which is far BELOW this floor and therefore unresolved -- it lies
    somewhere in [0, 2.940 s], and reading it as a measured near-zero would be the inversion this
    harness's resolution rule exists to prevent. What the arms support is that their difference is far
    larger than the floor. Two runs per cell, evaluated on the runs that produced them; the
    self-completing cell's prediction is built from its own achieved dose, so its residual is an
    instrumentation offset rather than a test of the rule -- this is consistency, and weaker than
    validation
      protocol: victimService=60s grace=30s
      control: the honouring arm held the device 0.041 s over 4 runs (nothing is subtracted)
      grace-bounded    dose declared=20s achieved=20.731 -> remaining=39.269 binds on termination grace
        predicted=30.000 observed=30.054 residual=+0.054 INSIDE (n=2)
      self-completing  dose declared=40s achieved=40.737 -> remaining=19.263 binds on remaining service
        predicted=19.263 observed=18.512 residual=-0.751 INSIDE (n=2)
      CONTRAST self-completing -> grace-bounded: predicted=10.737 observed=11.542 residual=+0.805 INSIDE
        (the kink; anything common to both regimes cancels here)
      residuals judged against a 2.940 s floor, restricted to the hold's own endpoints (the owner's
        admission and the victim's stop) rather than pooled over every event kind

The two regimes put the victim on opposite sides of the grace period, so the same rule has to predict a
thirty-second hold in one and a nineteen-second hold in the other. A model fitted to either alone would miss
the other by eleven seconds. It can also print REFUTED, and a test proves it can — a refutation nobody can
trigger is not one.

**Three qualifications a review forced, none of which this page arrived at on its own.**

The self-completing cell tests nothing. Its prediction is `victimService − achievedDose`, and the achieved
dose is measured from the same run's own events, so expanding the algebra cancels `min`, the grace period
and preemption alike: what remains is the victim's observed Ready-to-terminal span minus its declared
service, plus the owner's submit-to-admit latency. The published `residual=-0.751` reconstructs exactly that
way from the two records. It is an instrumentation offset printed under the heading of a model test.

The CONTRAST is not independent of the two levels. Both its predicted and observed values are differences of
the same two cells, so its residual is identically `+0.054 − (−0.751) = +0.805` — no new degree of freedom,
and already bounded by twice the floor before any datum is read. "Anything common to both regimes cancels"
holds for an ADDITIVE, dose-invariant confound and not for a dose-dependent one, and the two predictions are
asymmetric — one a compiled constant, one a measurement — so the self-completing cell's offset enters the
contrast with a single sign.

Every refutation reachable on a qualified node is a latency fault. Both regimes require a residual above
about three seconds, which on this cluster means Kueue's admit-after-preempt lag or the delete-to-terminal
chain blowing out. That would be printed as a refutation of the termination contract, which it is not.

**This page printed the previous version of that check until a review found it.** The old one tested the
model against the owner's WAIT and reached it by subtracting a platform-cost term borrowed from the other
arm; its residuals were −1.715 and −2.628 s against a 6.424 s floor. Both the estimand and the arithmetic
changed, the artifact changed with them, and this page did not. The citation test that guards these documents
checks that cited records exist and decode — it says in its own comment that it cannot check whether a number
in a table came from them, and this is what that gap looks like.

## It ran on a second node, with the cluster's occupancy held fixed

    $ queuelabrun -compare 'ex/e17-grace-bounded-A-honor-*.json' -mode node
    the owner's wait under A-honor moves 0.476 s across 2 levels of node, INSIDE the 5.860 s floor
      platform-worker  n=2 ownerWait mean=2.180
      platform-worker2 n=2 ownerWait mean=2.657

The ignoring arm agrees: 0.000 s across the two nodes against a 6.650 s floor. Neither is a demonstration of
independence — what they support is that any node response is smaller than about six seconds.

**The first attempt at this comparison was worse than unresolved, it was confounded**, and saying so is the
point of keeping this section. `platform-worker2` had one of its two devices held by the platform's own
serving workload, which the ownership taint does not evict, so the run wrote a record naming the Pod and
stopped rather than measuring on a node it could not hold exclusively. Scaling the Deployment down did not
help: this repository's own InferenceDeployment controller reconciled the replica count straight back, which
is the platform working as designed. The `InferenceDeployment` had to be scaled instead — and because that
was done only for the second worker, the node and the cluster's occupancy varied together, and the number
attributed to the node was also a number about occupancy.

Nothing in the record could have said so, because the qualification looked only at the worker. It now records
the device holders elsewhere in the cluster, and this set holds the occupancy fixed on both nodes throughout.

## What is measured and what is arithmetic

The protocol fixes the victim's service at 60 seconds and preempts it 40 seconds in
(`cmd/queuelabrun/spine.go`). So before any run happened, the trace already determined that a responsive
victim would discard about 40 seconds and that an unresponsive one would hold the device about 20 seconds
longer. **Both headline magnitudes are constructed, not discovered.**

Subtracting what the protocol set leaves what the cluster actually contributed:

| quantity | protocol says | observed | residual | that run's floor |
|---|---|---|---|---|
| A-honor discarded, self-completing | 40.0 | 40.871, 40.888 | +0.871, +0.888 | 2.489, 3.106 |
| A-honor discarded, grace-bounded | 20.0 | 20.901, 20.888 | +0.901, +0.888 | 2.446, 2.430 |

I called the first residual the control plane's own cost — about 0.94 seconds from Kueue deciding to preempt
to the container being gone. **That was wrong, and the harness now refuses to let it be said again.**

Every time in this ledger is `col.elapsed()`: the collector's clock when a watch event ARRIVED, not when the
event happened. So each interval carries however far behind those arrivals are, and that had never been
bounded. The record bounds it by capturing the kubelet's own `finishedAt` beside the collector's observation
time — and, since the retraction, DERIVES a `resolvedToNs` from it and prints it beside every magnitude. Put
the two columns above side by side and every residual is inside its own run's floor. That is not a close
call to be argued about; it is the same statement the record now makes as a field.

**This is a bound, not a delivery time, and the distinction is the point.** The two stamps come from
unsynchronised clocks on different machines, and `metav1.Time` serialises with SECOND precision so the
kubelet's value arrives with its nanoseconds zeroed — checked rather than assumed: every observed value ends
in nine zeros. So each figure mixes propagation, clock offset and up to a second of truncation, and nothing
available here separates them. Saying "the watch is 900 ms late" would claim a measurement this harness
cannot make without clock synchronisation or distributed tracing.

What it does support is one statement, and it is enough: **an interval whose endpoints carry a gap of this
size is not resolved below it.** The residual was under a second. Across the twelve records the gap runs from 0.028 to 2.675 seconds and
differs at each endpoint — the page carried "0.4 to 2.4" from two runs of the schema-10 era long after
those records were deleted. The residual is therefore not resolved by this harness, and no number of
repetitions changes that — a resolution problem is not a noise problem.

What survives is what the arms differ in, which is an order of magnitude larger than the floor: 40.9 seconds
against 0 in the self-completing regime, and 29.0 seconds of the owner's waiting between the arms in the
grace-bounded one. Those
differences are real, and `-compare` is what says so from the files rather than from this page. The sub-second residual inside them is
not something this harness can see, and no number of repetitions fixes that — it is a resolution problem, not
a noise problem.

## What it says

**Honouring SIGTERM discards work.** The victim stops when told to, and the 41 GPU-seconds it had spent are
thrown away — 16,857 and 17,846 iterations across the two runs, which is what makes the discarded seconds a
discarded quantity of something rather than an interval.

**Ignoring SIGTERM discards nothing and defeats the reclaim.** The victim runs to completion 19 seconds past
the preemption, so no work is lost — and the quota owner waits 19.2 seconds to start instead of 2.2. The
ledger shows why: `Preempted` at t=44s, `AttemptStopped reason=Succeeded` at t=1m3s. The reclamation was
issued and did not reclaim anything for nineteen seconds.

That is the trade the lab was built to show, and neither arm is simply better. A platform that guarantees its
quota owners a bounded time-to-start has to make preemption effective, and making it effective means
someone's partial work is destroyed.

What the runs support is the SHAPE of that trade, not a magnitude anyone should quote. "41 GPU-seconds per
preemption" would be quoting the trace back at itself: a job preempted one minute in would discard a minute.
And the sub-second residual is inside the harness's own delivery lag, so it is not a transferable number
either.

What transfers is categorical: an unresponsive victim converts the whole of its remaining service into the
owner's waiting time, and the reclamation is recorded as ineffective while it does. Measuring how long a
preemption takes to take effect needs an instrument at least an order of magnitude finer than this one.

`preemptionIneffective` is the field that separates them, and it is derived rather than asserted: the
reconstruction pairs the preemption decision with the attempt that was supposed to end and checks whether it
did.

## What this does not say

- **Nothing about GPU behaviour.** The workload is pure Python float arithmetic and the device plugin is
  fake, so a Pod that dropped its `nvidia.com/gpu` request would compute the same iterations at the same
  rate. This is no longer only a sentence on a page: `validity.deviceEvidence` reads `device-not-observed`
  on every record, it is DERIVED from the measurement rather than written, a blank value is refused on
  decode, and the comparison carries the weakest contributor's axis rather than the strongest. It exists
  because a consumer reading `verdict` saw `admissible-under-implemented-gates` and had no machine-readable
  way to learn that these are GPU-*seconds of reservation*. A run on real hardware would read identically
  until something moves that field, which is the whole reason the field is there.
- **Two runs per arm is two runs per arm.** The reproduction is encouraging and is not a distribution. No
  variance is claimed and none should be quoted, and no arm EFFECT is established: `-compare` says so in the
  document itself rather than leaving it to a reader's memory — it reports resolution and confounding, not
  inference, because these runs are not a sample of anything and their variance has never been characterised.
- **The magnitudes are the trace's.** 41 and 21 are the dose and the remaining service, to within a floor.
  The one number that is NOT the trace's is the 29.0 s between the grace-bounded arms, which is the grace
  period the protocol sets on its Pods and reads back. An earlier version of this line said the cluster
  defaults to it "and the protocol never sets" it; that is the claim retracted two sections above, which
  survived here because the retraction was written where the claim was noticed rather than everywhere it
  appears.
- **The residuals are inside each run's own floor.** Compare the two right-hand columns of the residual table
  above: every residual is smaller than the floor of the run it came from. Nothing sub-second here is
  resolved, and this harness cannot be made to resolve it by running more of the same — a resolution limit is
  not a noise level.
- **Both regimes are here now.** `self-completing`, where an ignoring victim finishes its own service, and
  `grace-bounded`, where the grace period cuts it short — and the same arm gives OPPOSITE answers in the two.
  See [queuelab-grace-boundary.md](queuelab-grace-boundary.md). That contrast, not this page's magnitudes, is
  what the experiment turned out to be for. `-compare` refuses to pool them, because two regimes measuring
  different quantities produce a difference that answers no single question.

## Reproducing

    go build -o queuelabrun ./cmd/queuelabrun
    ./queuelabrun -inspect-worker -worker platform-worker        # must print FREE
    ./queuelabrun -preview -arm A-honor -runid pv1 -worker platform-worker -out ./ex/preview.json
    ./queuelabrun -termination-canary -worker platform-worker    # must print QUALIFIED
    ./queuelabrun -dose self-completing -arm A-honor  -runid h1 -worker platform-worker -out ./ex/h1.json
    ./queuelabrun -dose self-completing -arm A-ignore -runid i1 -worker platform-worker -out ./ex/i1.json
    ./queuelabrun -compare 'ex/h*.json,ex/i*.json' -compare-out ./ex/comparison.json

Take the arms alternately. The comparison checks that they did and says CONFOUNDED on the finding itself when
they did not — a block of one arm followed by a block of the other moves with everything else that changed
between the two blocks, and no arithmetic separates them afterwards.

The canary must be re-taken whenever the harness commit changes: the qualification is keyed to it, so a
reading from an earlier build refuses rather than silently qualifying a different mechanism.

The preview runs and checks exactly as a real run does and withholds both the ledger and the numbers, so it
is the right way to find out whether the cluster is in a state that can produce evidence before spending a
run on it. Its record can never be admissible.

Each record carries the verdict, the claims that produced it, the ownership window, the qualification and the
full ledger, so the figures above can be re-derived from the file rather than taken from this page.
