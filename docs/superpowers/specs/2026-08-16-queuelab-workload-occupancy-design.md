# queuelab workload: make occupancy real, then evidenced, then recoverable

**Status:** design, not yet implemented. Written before the real-GPU session, because the defect it addresses
is invisible on the CPU-simulated cluster and becomes uninterpretable-and-expensive on real hardware.

## The problem in one sentence

The trace's workload is `sleep`, so a run's headline number measures **discarded reservation**, not
**discarded work** — and on the simulated cluster nothing can tell the two apart.

### Why it is invisible today

The kind cluster advertises `nvidia.com/gpu: 2` from `gpu-simulator`, a DaemonSet with no devices behind it.
A Pod that holds the resource and computes nothing is indistinguishable from one that saturates a device,
because there is no device. "Pod alive" and "device busy" are the same proposition here, so
`wastedGPUSeconds` is correct by construction.

On real hardware they come apart. `sh -c "sleep 60"` holds an allocation and leaves the SMs idle. The run
still completes, the gates still pass, the record still reads `admissible`, and the number reported as GPU
seconds of discarded work is GPU seconds of discarded *booking*. **Nothing in the harness refuses this**, which
is what separates it from the other real-GPU risks:

| risk | what happens on real hardware |
|---|---|
| grace period differs | the canary refuses — it compares the apiserver-stored value against the protocol's |
| environment changed | the canary key (image digest, node UID, kubelet, runtime, template hash) forces a re-take |
| capacity differs | gate 2 reads the node's real allocatable |
| startup far longer | the barrier misses its deadline and the run is refused at the horizon |
| startup moderately longer | `measurement.censored` records that the waste figure is a floor |
| **workload does no GPU work** | **nothing. Plausible numbers, no error, no flag.** |

## What is already established

The contract translation was measured before this document was written, because the whole staging rests on it.

Today's arms differ by one shell command:

```
honour:  sh -c "trap 'exit 143' TERM; sleep N & wait"
ignore:  sh -c "sleep N"
```

The ignoring arm ignores **because PID 1 has no handler and the kernel drops the default disposition** — not
because of anything the command does. Measured on this cluster with a 15 s grace period:

| arm | Python form | time to disappear after SIGTERM |
|---|---|---|
| ignore | no handler installed at all | **16 s** — survived to the grace boundary, then SIGKILL |
| honour | `signal.signal(SIGTERM, …exit(143))` | **2 s** |

This matters for the translation. The tempting Python spelling of the ignoring arm is an explicit
`signal.signal(SIGTERM, SIG_IGN)`, and it would be **wrong** — not in behaviour but in provenance. It would
ignore for a different reason than the shell form does, and the arms would then differ by two things rather
than one. Installing nothing preserves the mechanism exactly.

## Stage A — occupancy that is real, and evidenced rather than assumed

Replace the sleeper with a compute loop.

The instinct is to stop there: a loop that multiplies matrices does occupy the device, so the number becomes
meaningful. That instinct repeats the defect. **An unverified compute loop is the same category of claim the
sleeper was** — the harness would still be asserting occupancy rather than observing it, and a loop that
silently fell back to CPU, or that a thermal-throttled device barely advanced, would produce the same
plausible numbers with the same absence of a signal.

So Stage A has two halves, and the second is the point:

1. **The workload computes.** A fixed-shape matmul in a loop, sized so one iteration is short relative to the
   grace period, so termination latency stays a property of the signal handling rather than of the iteration.
2. **The workload reports progress**, and the run records it. Periodic `iters=N` on stdout, and a final line
   on either exit path. Then "40.8 GPU-seconds discarded" is backed by "N iterations discarded", and a
   workload that did nothing reports `iters=0` — visibly, in the artifact, rather than silently.

### CPU-first is forced, not chosen

The termination canary strips the GPU limit from its probe Pods so they can be scheduled on a node whose
devices are all spoken for. A workload that requires CUDA to start would therefore **break the canary**, which
is the gate every run depends on.

So the workload must degrade to CPU when no device is present. That constraint is what makes the whole thing
developable and testable on this cluster before any GPU exists — and it is a constraint the design already
has, not one being added for convenience.

### Decisions Stage A must make

- **Image.** The arms currently run a busybox-class image. Python is needed. Side-loading into kind is
  established practice here (`gpu-simulator`, `benchharness:evidence`), and the `:latest` trap is documented
  in `hack/m5b-gateway-path.sh` — a `:latest` tag sends the kubelet to a registry that does not exist for a
  side-loaded image.
- **What computes.** numpy matmul for Stage A. It keeps torch out of the dependency set until the GPU stage
  actually needs it, and numpy exercises the same structure.
- **Iteration size.** Short enough that a terminating iteration does not dominate the honouring arm's
  measured latency, which today is ~2.8 s and would stop being about the signal if an iteration took seconds.

### What Stage A does NOT buy

It does not make the CPU numbers comparable to GPU numbers, and it does not validate any GPU-specific
behaviour. Its whole purpose is that the number stops being uninterpretable the moment real hardware appears.

## Stage B — progress in the artifact

**Status: implemented.** The two open questions below were settled by measurement rather than by reasoning,
and one of them changed the design.

Carry the iteration count into the ledger and the record, beside the measurement block that already carries
the horizon and the censoring flag.

### The transport: termination message, not `pods/log`

This document originally proposed reading container logs. A review proposed the cheaper route — the workload
writes its progress to `/dev/termination-log`, and the kubelet copies it into
`ContainerStateTerminated.Message` — which needs no log API and no extra RBAC.

That route rested on an **unverified inference**: that the message survives for a container the kubelet
SIGKILLs, when the Pod is then removed by graceful deletion. Exit codes were already arriving from such
containers, and the message travels in the same struct, so it should arrive too — but "should" is not
evidence, and the whole stage was built on it.

Measured on the decisive combination, `A-ignore` × `grace-bounded`, the only arm where the borrower is
actually SIGKILLed:

```
job=a2-borrow  reason=Failed     exit=137   iterations=21540
job=b1-owner   reason=Succeeded  exit=0     iterations=25973
```

It arrives. The route is confirmed and `pods/log` is not needed.

### Discarded ≠ stopped — what that same run also showed

The first implementation summed every stopped attempt, so the owner's **completed** 25973 iterations were
added to the borrower's genuinely discarded 21540 and reported as 47513 discarded. A number that would have
been quoted as waste was, for more than half its magnitude, the opposite of waste.

The filter is a `Completed` event in the ledger — the ledger's own statement that the row's work was
credited — and its **absence** is what makes an attempt's iterations discarded. That is deliberately wider
than "the container failed": the self-completing regime's victim exits 0 and is still re-executed from zero,
because Kueue does not credit a suspended Job's finished attempt. Judging by exit code would call that work
saved when losing it is the entire subject of the lab.

Two things are worth carrying forward from how this was caught. It was not caught by the suite — the fixture
had no completed row in it, so the missing filter was unobservable. And it was caught on the **first live run
of the new instrument**, because that arm is the first to put a completed row and a killed row in one run with
numbers attached. A new measurement's first real run is there to have its numbers read, not to be watched for
a green light.

Once it is there, the waste figure has a denominator: a row that was preempted after 40 s of a 60 s service
discarded a known number of iterations, and a reader can divide. Without it, Stage A's progress reporting
lives only in container logs, which teardown deletes.

This is deliberately a separate stage from A. A is worth doing even if B never happens, because a human
reading the logs during a run can still see `iters=0`; B is what makes it survive into the artifact, which is
the thing this project keeps insisting is the deliverable.

## Stage C — checkpoint and resume, as a new arm

Today both arms discard everything: `A-honor` loses 40.8 GPU-seconds of progress **even though it stopped
politely**, because the row re-executes from zero. That number is the argument for this stage and it now
exists as a measurement rather than a guess.

A resuming arm writes progress to a volume and restarts from it. The interesting property is that it needs an
oracle: the run must be able to say the resumed work is the *same* work, not merely work of the same shape.
A final hash over the computed result gives that — an arm that resumed correctly produces the hash a run that
was never preempted produces.

This is a study, not a fix, and it changes what the lab measures rather than making an existing measurement
honest. It should not block the real-GPU session.

## Sequencing and what each stage costs

| stage | blocks the GPU session? | testable without a GPU |
|---|---|---|
| A — occupancy real + progress reported | **yes** | fully |
| B — progress into the record | no | fully |
| C — resume arm + hash oracle | no | mostly |

Only A blocks. B and C are worth doing GPU-free because they are cheaper here, but a real-GPU session can
proceed with A alone.

## Risks

- **The canary is re-taken when this lands**, because the image digest and the rendered template both change.
  That is the mechanism behaving correctly, and the re-take is itself a check: if it does not happen, the key
  is not covering what it claims to.
- **Stage A changes what every historical number means.** The 25 runs recorded so far measured reservation.
  They should not be compared against post-A runs, and the record's schema version is what marks the boundary
  — this is the same argument every earlier bump made.
- **A compute loop makes runs sensitive to node load** in a way `sleep` never was. Two runs on a busy machine
  will not agree the way the current ones do (spreads of 0.1–1.2 s). That is not a regression; it is the
  variance that was always there and was being hidden by a workload that did nothing.
