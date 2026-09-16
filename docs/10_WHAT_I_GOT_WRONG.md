# What I Got Wrong

> **Status (2026-08-07).** A record, not a design. Everything here is a mistake I made in this repository.
> The evidence is committed: `hack/m6-e2e-evidence.log` and `hack/m6-e2e-evidence-contaminated.log` for §5,
> the specs under `docs/superpowers/specs/` for the rest. Where a figure comes from a run whose raw ledger
> I did not keep, I say so.

On 2026-08-02 I published a measurement of Kueue quota-reclaim preemption. It was wrong. I retracted it the
same day, and then spent the following days discovering that the thing I built to replace it was also
wrong, in a different way, twice.

Two things belong up front, because they change how the rest should be read. The first is that most of
these were found by adversarial review — including AI reviewers I ran against my own branches — and not by
me on a first pass. The second is that the original runs were made without persisting their ledgers, so the
queuelab figures below come from the design record and the reports written at the time, not from a raw
capture I can hand you. That is itself one of the mistakes.

---

## 1. The refutation was in my own ledger

The claim was that switching `reclaimWithinCohort` from `Never` to `Any` admitted the quota owner about
120 ms after submission, at a cost of roughly 39 GPU-seconds of the borrower's discarded work.

My ledger recorded, for every attempt, the reason it stopped. The borrower's reason was `Succeeded`. My
accounting never looked at that field. The rule was, roughly: a preemption decision happened, and later an
attempt stopped, therefore the preemption destroyed that work.

The arithmetic is embarrassing in hindsight. The borrower became Ready at t≈3 s and ran a 40-second
workload, so a stop at t≈43 s is exactly what finishing looks like.

**What changed:** waste is never charged without an observed *failed* terminal phase. A `Succeeded` stop is
reported as unattributed occupancy plus an explicit flag saying the platform decided to reclaim and the
workload did not comply. No terminal phase at all is `AttributionUnknown`, not zero.

## 2. I assumed a container stops when you send it SIGTERM

The workload was `sh -c "sleep N"`, so `sleep` became PID 1. A container's PID 1 does not get the default
SIGTERM handler; without an explicit trap it ignores the signal and dies only to SIGKILL when the grace
period expires.

I ran both forms against the live cluster:

| workload command | outcome |
|---|---|
| `trap 'exit 143' TERM; sleep N & wait` | terminated in **1 s**, `phase=Failed`, exit code 143 |
| `sleep N` | survived the full 30 s grace, **SIGKILL at 34 s** |

So nothing in that experiment was ever preempted. The jobs ran to completion and were re-executed from
scratch, which is where the discarded GPU-seconds actually came from — real waste, opposite mechanism.

Also wrong, and separately: I had read "the owner was admitted in 120 ms" as the owner getting service.
Admission is a quota reservation. The owner's Pod became Ready 9.4 s later, because the borrower had not
released the device.

**What changed:** the termination contract became an experimental variable instead of an assumption. The
honoring and ignoring commands are two arms of the design now, not an accident of how I wrote a fixture.

## 3. I fixed the measurement and left the experiment confounded

Having corrected the accounting, I re-ran and got a clean-looking result. Review then pointed at the trace
itself.

The fixture is three jobs: the borrower meant to be reclaimed, the quota owner whose admission is the
endpoint, and `a1`, a co-tenant that is supposed to hold its own unit throughout. I had given all three the
same duration.

```
42.607s  a1        stopped   <- the co-tenant released a GPU on its own
42.638s  a2-borrow stopped   <- the alleged victim, 31 ms later
43.550s  b1-owner  Ready
```

31 milliseconds. Nothing in that run can say whether the owner ran because the victim was reclaimed or
because `a1` happened to finish. A perfectly instrumented run of a confounded trace is still a confounded
run.

**What changed:** `a1` now outlives the entire owner-restoration window, so the victim's release is the only
release that can place the owner.

## 4. The executable was not the design, twice

The design says the owner is submitted 40 seconds after the borrower becomes Ready. The executable derived
that interval by subtracting two trace offsets that were never meant to encode it, and produced **49
seconds**. Nobody typed 49 anywhere. It fell out of arithmetic on numbers that meant something else.

The design also defines three arms — honoring victim, ignoring victim, no-reclaim reference. The runner
called a helper that always rendered the SIGTERM-ignoring command, so the honoring arm existed in the
design document, in the tests of the pure layer, and nowhere a run could reach it.

**What changed:** the dose is a stated constant the schedule builder validates, and the arm is a closed enum
with a per-row termination contract. The dose is still not *delivered* correctly — it is measured from a
two-second poll of a derived phase rather than authoritative Pod Ready, and the realized value is not
recorded.

## 5. I wrote an evidence document containing a command I never ran

Not the queuelab. I found this while repairing the public record.

`hack/m6-kind-e2e.md` presented this as a transcript:

```
$ kubectl get resourcequota -A | grep gpuquota
# (nothing — the GPU ceiling is enforced only by Kueue, not double-counted)
```

That command appears in neither the script nor the captured log. And the log beside it came from a cluster
the script had reused — `cluster platform already exists; reusing it` — carrying a stale ResourceQuota from
an earlier experiment. It contains 27 `exceeded quota` events, which contradict the claim outright. It is
committed as `m6-e2e-evidence-contaminated.log`.

I re-ran on a cluster created from scratch. The claim was true: zero `exceeded quota` events,
`k get resourcequota -A` returns `No resources found`, and the check is now captured by the script instead
of asserted in prose. The conclusion had been right and the evidence had been contaminated, which is the
worse way round, because the conclusion survived by luck.

The re-run also falsified a smaller claim I had made with more confidence than I had earned. The document
said Kueue "preempted the borrowed `tenant-a` job". The fixture has now been run three times: two runs
preempted `a2`, one preempted `a1`. Quota inside a ClusterQueue is fungible, so there is no such object as
"the borrowed job" — the queue was admitted 2 against a nominal of 1, and Kueue returned one unit by
evicting one of the two. I had read the first run's outcome as designed behaviour. It is the same error as
§3, in a different subsystem, and I made it while writing up a run rather than while designing one.

---

## What I would do differently

The worst of these — §1, §3 and §5 — are one failure: a field written to a ledger and consulted by no rule,
a duration set without asking what it makes indistinguishable, a transcript written from memory instead of
capture. In each case the thing that would have caught me was already available and nothing looked at it.
The fix that generalizes is not "be more careful." It is: for every claim, name the observation that would
refute it, and make something check for it. That is why `queuelabrun` currently exits non-zero and names
four validity gates it does not have.

I also hardened the wrong thing. The lab runner, which runs on my own single-user kind cluster, has a
resource-version-preconditioned ownership transaction with crash recovery. The operator, which is the part
that would run on a real multi-tenant cluster, has no conflict handling at all. I built where review
pointed, not where failure would cost most.

And the one that still bothers me most is §1, because there was nothing to discover. The stop reason was in
the row. I had written the code that put it there.
