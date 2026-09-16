# What the real-GPU session will measure, decided before it runs

This is written before any GPU exists, and that is the point. Everything below — the quantity, the baseline,
the resolution the instrument has, and the result that would refute the model — is fixed here so that the
session cannot be read backwards into whatever it happens to produce.

The lab has retracted two claims this month. Both were retracted after the numbers were in hand, which is the
most expensive moment to discover you were measuring the wrong thing.

## The one quantity

**How long the quota owner waits between Kueue admitting it and its Pod running**, in the arm where the
borrower honours SIGTERM.

Not the borrower's discarded seconds. Those are dose-determined — the protocol fixes the victim's service and
preempts it partway in, so the magnitude is arithmetic the trace already did — and they describe a loss the
platform never promised anybody anything about. The owner's wait is what a reclaim promise is judged on.

## The baseline, already measured

**This section was wrong once, and the correction is the reason to trust the rest of the page.** It fixed the
baseline at "2.690 s mean, 2.534-2.792 s, spread 258 ms, 8 observations" — a table assembled at a shell from
four records that were then deleted, sitting beside machine-generated figures in the same document that
reproduce to the digit. The value 2.534 appears in no record that exists. It was the last hand-typed number
in this lab and it was the one an entire session is defined against.

It is emitted by the harness now, and the whole point is that the reader can run the command:

    $ queuelabrun -compare 'ex/e17-*-A-honor-e17?h?.json' -mode baseline
    ===== BASELINE (arm A-honor) =====
    under A-honor the quota owner was running 2.349 s after admission, over 6 runs spanning 2 dose
    regime(s) and 2 node(s), with a spread of 2003 ms against a worst-run floor of 3.414 s. A session
    differencing against this must add its own floor to that one; the difference of two independently
    measured means carries both of their errors
      ownerWait mean=2.349 min=1.208 max=3.210 spread=2003ms n=6
      runs=e17gh1,e17gh2,e17wh1,e17wh2,e17sh1,e17sh2
      doses=grace-bounded,self-completing
      nodes=platform-worker,platform-worker2
      device: NOT OBSERVED -- this baseline was taken where no run established that a device did work, so
      it is a control-plane figure and not a statement about hardware

**The spread is 2003 ms, not the 258 ms the deleted table claimed.** That figure was not merely
unreproducible, it was optimistic — two runs per cell could not show the variation six runs show, which is
exactly what a reviewer said two-run means would hide. It grew again when the workload was replaced: the
same six cells under a workload that reaches for a device before falling back to the CPU spread wider than
under one that never tried, and wider again on the next taking. Two seconds of spread on a quantity whose
arm difference is twenty-nine is not a problem for the finding; it is a problem for anyone quoting the mean
alone, which is exactly what a two-run table invited.

**These twelve runs interleave all three factors**, so no line above carries a confounding warning. The set
before them blocked the regimes, and the one before that also varied the cluster's occupancy alongside the
node — the second worker could not be held exclusively until the platform's own serving workload was scaled
away, so a node result and an occupancy result were the same number. This set holds the occupancy fixed on
both workers for its whole duration and alternates arm, regime and node.

### The same interval, read off the components' own clocks

The record carries the owner's wait twice: as a difference of watch ARRIVAL times, and as a difference of the
two components' own transition stamps — Kueue's Admitted and the kubelet's Ready. The pair bounds different
error sources, and it earned its place immediately:

| run | node | arrival | component stamp |
|---|---|---|---|
| e17gi1 | worker | 31.219 s | **31.000 s** |
| e17gi2 | worker | 31.206 s | **31.000 s** |
| e17wi1 | worker2 | 31.233 s | **31.000 s** |
| e17wi2 | worker2 | 31.192 s | **32.000 s** |

The arrival figure scatters across 41 ms and the stamp figure moves by a full second inside that scatter. That
scatter is watch delivery jitter, and an earlier version of this page was about to attribute a smaller
version of it to the machine.

This set makes the stamp's other half sharper than the last one did. Four arrivals inside 41 ms produce
three stamps of 31 s and one of 32 — from a difference two orders of magnitude below the quantisation.
Truncation to the second is not a small rounding on a quantity read this finely: whether a whole second
appears depends on which side of a boundary an arrival happens to land. That is why the stamp is carried
BESIDE the arrival rather than instead of it, and why the node comparison below uses arrivals.

The stamp figure is quantised to the second, so it flips between 2 and 3 where the true value sits near a
boundary — which the honouring runs do. What it cannot carry is watch lag; what it carries instead is a
second of truncation at each end plus the offset between two components' clocks, constant for a pair of
machines and cancelling in an arm difference taken on ONE node. **It does not cancel between nodes**, so the
node comparison below uses the arrival figure only.

### It does not move with the dose, or with the node

Both are checked by the harness rather than asserted here:

    $ queuelabrun -compare 'ex/e17-grace-bounded-A-honor-e17gh?.json,ex/e17-self-completing-A-honor-*.json' -mode dose
    the owner's wait under A-honor moves 0.029 s across 2 levels of dose, INSIDE the 5.552 s floor

    $ queuelabrun -compare 'ex/e17-grace-bounded-A-honor-*.json' -mode node
    the owner's wait under A-honor moves 0.476 s across 2 levels of node, INSIDE the 5.860 s floor
      platform-worker  n=2 ownerWait mean=2.180
      platform-worker2 n=2 ownerWait mean=2.657

The ignoring arm agrees on the node (0.000 s against a 6.650 s floor) and disagrees on the dose, which is the
control: there the owner's wait IS the dose-dependent quantity, and the same check reports 11.997 s and says
EXCEEDS. A check that can only ever return "no response" establishes nothing.

**On the rented cluster this axis costs a second GPU node, and the session may decide not to buy it.** Both
figures above come from two kind workers, which are containers on one host — they vary the kubelet and the
node object, not the machine, and that is worth knowing but is not what a reader hears in "two nodes". A GPU
session that scales its node group to one delivers no node axis at all: `-compare -mode node` refuses a set
in which nothing varies, so the failure is loud rather than a number computed from one node and labelled as
two. The infrastructure permits a second node so the choice is the operator's; the burn doubles for as long
as both are up, and the node axis is the only thing it buys.

If the session runs on one node, what it returns is the arm contrast, the dose axis and the device evidence,
and the node axis remains what it is today: a control-plane result from two containers on one host, which
says nothing about a second machine's driver or its card.

Neither is a demonstration of independence. What they support is that any response is smaller than about six
seconds, which is what a baseline needs and is a weaker claim.

## What the session adds, and where the interval actually ends

The same protocol with a workload that touches the device. The restoration figure becomes

    baseline  +  driver and runtime cleanup after container exit
              +  device plugin re-advertisement of a real card

and the SESSION'S RESULT IS THAT SUM MINUS THE BASELINE.

**An earlier draft of this page added a third term — the replacement container's CUDA initialisation — and
that was wrong in a way worth spelling out, because it would have manufactured a false result rather than a
wrong one.** The interval ends at `EventPodReady`, which is `PodRunning` plus the Pod's `Ready` condition
(`internal/queuelab/provenance.go`). `BuildJob` renders a container with no readiness probe and no startup
probe (`internal/controller/mltrainingjob_controller.go`), so `Ready` fires when the container STARTS.
Initialisation happens inside the running process, after the endpoint. The first two terms land inside the
interval; the third — plausibly the largest of the three — lands structurally outside it.

Left uncorrected, the most likely session outcome was: measured term small, reported as "not resolved", and
this page had already blessed that null as a finding worth having. It would have been a sentence about a
device manufactured by an endpoint that never contained the term it named.

So the sum has two terms, and the "not resolved" outcome below means something narrower and true: the
device's SCHEDULING-side return is fast relative to the control plane's. It says nothing about
initialisation.

**If the session wants the initialisation term, it cannot simply add a readiness probe and subtract.** A
probe gated on device readiness changes what `Ready` means, and a difference between two intervals is only a
measurement when both ends are alike. Adding one requires re-taking the baseline under the same probe, on
both arms, and saying so — at which point the number below is superseded rather than differenced.

## The resolution this instrument has, stated in advance

Every interval here is a difference of watch ARRIVAL times, and the records bound how far those lag the
events they describe. Across the six baseline runs the per-run floor ran **2.430 – 3.414 s**.

So:

- a GPU-specific term **larger than about 6 s** will be resolved — the session's own floor plus this
  baseline's, because the tool's output above says in as many words that a session differencing against
  this must add its own floor to that one. This line promised 2.4 s, which is one floor where the rule
  requires two, and quoted it from a record set since replaced
- a term **smaller than that will not be**, and the session must report "not resolved" rather than a number

That second outcome is a legitimate result and is pre-registered as one. It would say the device's own return
is fast relative to the control plane's, which is worth knowing and is not a failed experiment. What it must
not become is a small number quoted as if it were measured — that is precisely the retraction this lab has
already made once, when 0.94 s of residual was published as the control plane's cost while the instrument's
own spread ran to 2.4 s.

If a finer figure is wanted, the instrument has to change before the session, not after. Kubernetes Events on
this cluster carry no `eventTime` — the kubelet writes the legacy `firstTimestamp`, quantised to the second —
so the API surface offers nothing better. A node-side clock would, at the cost of a dependency the lab does
not currently have and that does not survive a managed cluster.

## Three limits that survive, and what each would actually take

These are named here rather than fixed, because the fix for each is either impossible with what is recorded
or costs more than it buys before the session.

**The instrument cannot be sharpened from the existing ledger.** The device hold reproduces to between 5 and
85 ms within a cell — the page said 8-17 ms, which held for five of the six cells and not for the
self-completing ignoring one and is judged against a floor of about 3 s. That is a precision figure against a conservative accuracy
bound, not evidence the bound is 200x too large -- and nothing recorded can calibrate the difference. The
hold's endpoints come from two watches, so its error is their differential delivery lag, and there is no
interval in this system with independently known truth that spans the same two streams. The one same-watch
interval with a declared truth, the owner's Pod Ready to its Succeeded against a 60 s service, tests the Pod
watch against itself and identifies nothing about the Workload watch.

Reading the hold on the components' own clocks does not close it either, and the data says so: across the
twelve records the hold's two endpoint readings differ by 117 to 1004 ms, a range the second-truncation of
two stamps explains entirely — and one that reaches a full second, not the 512 ms this page used to claim. The lag is inside it and cannot be separated. What that second reading DOES buy is a check -- if
the two ever describe different intervals, neither is publishable -- and that check now exists rather than
being promised in a comment.

Closing this needs a new causal timing reference: one API transaction producing two independently watched
objects with a common reference instant, or trace timestamps at the watch boundary. Both are new instruments,
not more runs.

**Two runs per cell is not fixed by more runs.** Under an uncalibrated instrument, repetition improves an
estimate of repeatability and does not turn a sub-floor result into an accuracy-established one. What IS
worth changing is the allocation: the current set is four runs in each grace-bounded arm and two in each
self-completing one, which is weakest exactly where the model's kink is tested. Three complete arm x dose
blocks, with worker and ordering balanced across them and one block held out of any parameter fitting, would
extract more from the same twelve runs -- and would give the model an out-of-sample test it does not have.

**Cluster diversity cannot be had here at all.** kind runs three containers on one host: the two workers share
a kernel and a clock, so the inter-component offset the two-clock design reasons about is zero by
construction and untested exactly where it will matter. A second cluster on the same machine is a
contamination check, not diversity. The real answer is a second host -- which the GPU session on EKS is, with
a different kernel, a different clock and real hardware. This limit is therefore answered BY the session
rather than before it, and the session's first result should be read with that in mind: the first time these
figures are taken anywhere but one laptop is the first time the offset assumption is exercised.

## What would refute the model

The model this lab arrived at is that a preempted workload holds its device for
`min(remaining service, termination grace period)`, and that the owner's wait tracks it.

Each condition names the command that decides it, because a refutation nobody can evaluate is not one. Two
of these were unfalsifiable as first written — "stops tracking" had no arithmetic anywhere in the repository
and no tolerance, and "stop differing" asked the harness to assert an equality its own rules forbid it to
assert.

1. **The honouring arm's restoration stops being node- or dose-independent.**

       queuelabrun -compare '<session records>' -mode dose
       queuelabrun -compare '<session records>' -mode node

   Refuted when either reports the wait moving by more than the summed floor. The baseline's usefulness
   rests on it, and a real workload's shutdown may not be prompt the way a Python loop's is: a training step
   with a kernel in flight cannot stop the way the termination canary's probe stopped in 1.2 seconds.

2. **The device hold stops matching `min(remaining service, grace)`.**

       queuelabrun -compare '<session records>' -mode model

   Refuted when it prints REFUTED — that is, when either regime's residual falls outside the floor. Nothing
   is subtracted from anything, and this condition previously said the opposite: it named the owner's WAIT
   as the estimand and required the honouring arm's restoration cost to be subtracted from it. That is the
   check this lab retracted, described in the one document whose job is to be exact in advance. The
   estimand has been the device hold since the retraction; the falsification criterion had not caught up.

   Two things about this condition are weaker than they read, and both were found by review rather than
   confessed here first. The self-completing regime's prediction is built from its own achieved dose, so
   its residual reduces to an instrumentation offset and that cell cannot refute the rule. And every
   refutation reachable on a qualified node — a residual exceeding roughly three seconds — would be a
   control-plane latency fault, printed as a refutation of the termination contract. The channel is real
   and it is mislabelled. The two regimes put the victim on opposite sides
   of the grace period, so one rule has to predict a 30-second hold in one and a 20-second hold in the other;
   a model fitted to either alone misses the other by ten seconds. If the device is returned at some later
   driver event rather than at container exit, this is where it shows.

3. **The arm difference falls below the session's own floor.**

       queuelabrun -compare '<session records>'

   Stated this way rather than as "the arms stop differing", because the harness is built to refuse the
   second: an unresolved difference is not a demonstrated equality, and every other page here says so. What
   IS checkable is that a difference resolved at 29.0 s against a 5.906 s floor in the baseline stops
   clearing the session's floor. If driver cleanup dominates and both arms converge, the termination
   contract stops mattering and the 120-second cap loses the justification it was given.

   **Before that reading is taken, the node's device count must be ruled out, and this condition used to
   invite the opposite.** The arm difference IS physical card scarcity: in every recorded run the owner is
   admitted within 0.1 s of the preemption decision and becomes Ready one to two seconds after the victim's
   terminal phase, in both arms, because its Pod is waiting for a card. A node advertising even one spare
   device absorbs the owner at admission in both arms and the difference collapses — producing exactly the
   convergence this condition calls the session's most useful discovery, out of the instance type. The
   cheapest rentable instance with more than one well-supported card carries four.

   So the convergence is evidence about driver cleanup only if what the node could SCHEDULE was exactly
   what the protocol needs. The qualification refuses anything else, and the surplus is held by an occupier
   Pod it recognises — `hack/gpu-session.sh` applies one per worker and the gate counts its devices out.
   That is what makes this a mechanism rather than a caution: a session that got it wrong produces no
   records at all.

   This condition previously said the device plugin was "configured to expose only that many", which was
   the first attempt and does not work: `NVIDIA_VISIBLE_DEVICES` is read by the container runtime hook and
   the plugin enumerates through NVML over whatever its privileged container can see. The manifest carries
   the evidence against itself. The retraction had reached the manifest and not this page, which is the
   document that governs the reading.

Any of the three is a better outcome than a confirmation, because all three change what the platform should
do and a confirmation changes nothing.

## The first sixty seconds on the node

On a node with a working card the preflight passes and prints, from
`cmd/queuelabrun/device_preflight.go`, a line beginning `DEVICE USABLE:` with the launch count and the
duration. **This page previously printed that line as a transcript, under a `$` prompt, with wording the
tool cannot produce** — an extra clause naming the node and a second sentence that appears nowhere in the
tree. No GPU has run this, which the page said; what it did not say is that the quotation was invented and
attributed to the tool. It is gone rather than corrected, because a transcript of a run that has not
happened is not a transcript however accurately it is worded.

Run this before the protocol, and read its exit status. It takes one Pod and about a minute, and it answers
the one question that decides whether the session produces a number or a receipt.

**And let the script run them.** `RUN_STUDY=1 ./hack/gpu-session.sh <gpu-node> [second-node]` verifies the
node and then runs the study through the same forward it verified, in the order the comparisons need —
alternating arm on every run and alternating dose and node within each pair a comparison reads, because a
blocked sequence produces the CONFOUNDED warnings the harness prints and the study then cannot use. It stops
at the first failed run rather than spending the rest of the budget on a route that has stopped working.

With one node it runs eight and the node axis is not delivered, which is the honest whole of what a one-node
session returns.

The script used to stop after the preflight, print the flags, and tear the verified route down — leaving the
operator to open another forward and add two flags to twelve invocations by hand. A review ranked the
resulting loss the most likely avoidable one of the session.

**If you drive the runs yourself, give every one `-require-device`.** Device observation is an optional axis everywhere else, and on a
cluster with no cards that is right — a run with no exporter is still a valid control-plane measurement. On
rented hardware the default inverts: a run whose port-forward died, or which was launched without the
observer flags at all, completes normally and writes a well-formed record saying `device-not-observed`. That
is the outcome this session exists to avoid, reached by omission rather than by failure, and the harness was
better at preserving it than at preventing it. With the flag the run refuses before touching the cluster if
no observer is configured, and invalidates itself with `device-not-established` if the observation
establishes nothing. The script prints the full command.

With `-device-metrics` it answers the other half too: whether anything OUTSIDE the workload can see that work.
The route to the exporter is the part of a session that used to exist only as prose, so it is a script now:

    $ ./hack/gpu-session.sh <gpu-node>
    exporter pod : dcgm-exporter-abc12 (on <gpu-node>)
    observer id  : nvcr.io/nvidia/k8s/dcgm-exporter@sha256:...
    route up     : http://127.0.0.1:9400/metrics

It picks the exporter Pod BY NODE, and that selector is the whole reason it exists. The Service in front of
the exporter is headless on purpose -- a round-robin one would answer alternate scrapes from a different
node's card -- so there is no cluster IP to forward to, and the forward has to name a Pod. Naming the wrong
one fails silently: it scrapes cleanly, parses cleanly, and reports that this run's Pod was never seen on any
card, which reads as a device fault. It also reads the observer's identity from the RUNNING Pod's imageID
rather than from the manifest, because the manifest says what should be deployed and the declaration should
name what is.

The preflight then applies the run's own gate, `EstablishesDeviceWork`, over the interval its Pod held the
card. Using the same gate rather than a private check is the point: a preflight with weaker criteria would
pass on a node where the session then refuses.

And it reads the two answers TOGETHER, because their combination is a third one. A workload on the CPU beside
an exporter reporting its card busy is the fake-exporter shape, and it is reported as such rather than as two
failures:

    ERROR: OBSERVER CONTRADICTS THE WORKLOAD on platform-worker: the exporter says this Pod's card was
    busy, and the Pod itself reports it never reached a driver call (kind="cpu-float" dev="no-libcuda")

That was produced by serving a deliberately dishonest exporter to a preflight on the GPU-less cluster. Two
branches cannot be reached without a card -- a working device with a broken observer, and both working -- and
they are the two a GPU node reaches first.

The termination canary cannot answer it. The canary STRIPS the device request from its probes, deliberately —
a probe that needed a free card would fail on a node whose devices are legitimately held, and allocation is
not what it measures. So nothing the canary reports is about a card. The preflight is its mirror: the same
Pod template, the same placement, the same finalizer, and the device request put back.

A run cannot answer it either, not in time. It answers at the end, after the protocol has spent its minutes,
and it answers with a refused record rather than with which of the eight driver calls said no. Those are
different afternoons: `no-libcuda` is a base image or a container runtime that never injected the driver,
`no-device` is a card that was not passed through, `ptx-load-failed` is a kernel this driver would not
compile. On rented hardware the difference between naming one of those and reporting "device not established"
is the cost of the session.

### The estimand is warm-node reclaim, and the preflight is what makes it so

Running the preflight pulls the workload image onto the node. Every run after it therefore starts without a
pull, and that is not a side effect to be tolerated -- it is the thing that makes the runs comparable to each
other. A cold first run pulls inside the observation window, pushes every container stop later, and can push
one past the horizon: `measurement.censored` flips, `wastedGPUSeconds` becomes a floor rather than a value,
and nothing in the terminal says why.

So the preflight reports what it found before it changed it:

    node warmth: WARM -- the workload image is already on ip-10-0-x-y, so the runs after this one start
    without a pull

What this does and does not cost is worth being exact about. **Between-arm differences are unaffected**: the
arms alternate in time, so warmth is uniform across them and cancels in every difference this study
publishes. **The absolute baseline is affected**, and that figure is quoted as a property of the platform --
so it is a property of a WARM one, measured on a node whose layer cache already holds the image and whose
driver has already been initialised. A session quoting it against a cold node is quoting the wrong number.

The preflight's own timings are not comparable with the runs' when it reports COLD, because it is the thing
doing the pull. That is the one case where its eight seconds mean something different from theirs.

It was executed against the kind cluster, where the honest answer is a refusal, and it gave it:

    ERROR: DEVICE NOT USABLE on platform-worker: the workload fell back to the CPU loop, reporting
    dev="no-libcuda" after 3335 iterations. A run here would complete and refuse to attribute device
    work, so its GPU-seconds would be seconds of RESERVATION exactly as they are on a cluster with no
    cards

## What makes the session count at all

`validity.deviceEvidence` must read `device-work-observed`.

Every record this lab has produced reads `device-not-observed`, derived rather than asserted and refused when
blank. A GPU run that comes back with the same value has bought nothing: it is a CPU run that cost money, and
the harness will say so in the field a consumer classifies on rather than in a footnote. The observer that
moves it has to be something the workload cannot write to — a Pod reporting its own device use is evidence of
nothing, for the same reason a Pod carrying a `quota-exempt` annotation is not evidence of exemption.

The workload now DOES report on itself, and the direction of that report is the whole of why it is allowed
to. It can only ever REFUSE. A container saying it launched kernels moves nothing; a container saying it fell
back to the CPU overrules an observer that claims its card was busy, because a card busy while the only Pod
on it made no driver call is somebody else's process arriving under this Pod's label. The party with the
motive is permitted to testify against itself and not for itself, and that asymmetry is what closes the
unlabelled-intruder hole the exclusivity clause cannot see.

The observer's CONTRACT now exists, in `internal/queuelab/device.go`, and the path from an observation to the
axis is derived rather than hardcoded — so a session plugs a scraper in instead of editing a boolean. What it
requires, per Pod attempt and across the interval being measured:

| requirement | why it refuses without it |
|---|---|
| an admissible observer (a DCGM exporter; the set is closed and currently has one member) | the workload is not a witness FOR itself: a Pod reporting its own device use is a claim by the party the check exists to constrain |
| the observer's own build identity | "DCGM said so" is not provenance if nobody can say which DCGM |
| the physical device UUID, and only one of them | an observation that cannot say which card it watched cannot establish that the card this Pod held did anything |
| attribution by Pod UID, not name | the UID is what the API guarantees unique across time; a name is free for reuse the moment its Pod is deleted, and an observer labelling by name is read against whatever holds that name when the mapping is resolved |
| coverage of the whole interval, no gap over 2 s | a gap that size can hide an entire preemption |
| at least two samples showing the card working | one non-zero reading is what a driver reports while another process initialises; an allocated idle card is the exact state this axis exists to distinguish |

**That scraper now exists** — `config/dcgm-exporter/`, `internal/queuelab/scraper.go`,
`internal/queuelab/dcgm.go`, the collector step in `cmd/queuelabrun/main.go`, and the route in
`hack/gpu-session.sh`. This line said it did not for several commits after it was built, and it survived the
pass that corrected eleven stale figures: that pass verified numbers and not statements of existence, which
is worth recording as the shape of its blind spot.

What has still never happened is any of it meeting a GPU. No figure taken before the preflight passes on the
node may be published as a GPU result.

Every refusal above is under test today, on a cluster with no GPU. That is the half that can be validated
without hardware, and it is the half that decides whether the hardware buys anything.
