# The queue load test that finally loaded the queue

The previous attempt did not measure what it said. It created objects with `kubectl apply` in batches and
reached **75 per second**, while the operator retired **143 reconciles per second** — so nothing ever waited,
the recorded peak queue depth was **0**, and what the run actually measured was the client. It was written
down as a failed stress test rather than a result, which is the only reason it is being redone rather than
quoted.

Two things changed.

**The client is concurrent and in-process.** No process start per object, no repeated TLS handshake, one
client shared by 32 writers. That is what makes it possible to offer work faster than the operator retires
it.

**Queue depth is read from the operator's own `/metrics`, not from Prometheus.** Prometheus scrapes on its
own schedule — 15s here — so sampling it every 5s returns the same scraped value three times and cannot see a
spike that opens and closes between scrapes. Reading the endpoint directly makes the interval the caller's,
and the harness records the interval it actually achieved rather than the one it asked for.

## Result

| | previous | this run |
|---|---|---|
| creates | 75/sec | **2,457/sec** |
| peak queue depth | 0 | **600** |
| drain | 10,054 ms | 11,570 ms |
| reconciles | 1,437 | 1,857 |
| reconciles/sec | 143.7 | **160.5** |
| sampling | 5s poll of a 15s scrape | 203 ms, measured |

600 MLTrainingJobs, 32 concurrent writers, `MaxConcurrentReconciles=1` (controller-runtime's default, unset
in this operator).

The depth trace shows the queue filling faster than it can be read: `0, 479, 599, 599, 599, …`. The first
sample after creation began already held 479 items, so the fill is faster than the 203 ms sampling interval
and the harness cannot say how quickly it got there — only that it was full within one sample.

## What it establishes

**The operator is the bottleneck, and its absorption rate is about 160 reconciles per second** on this
cluster with one concurrent reconcile. That is the number to know before running it anywhere real: at 600
objects it takes about 11.6 seconds to work through the backlog, and the relationship is linear in the number
of objects while the per-item cost stays flat.

**The per-item cost is flat**, which is the point of the field index added earlier. Draining 600 items took
11.6 s where 400 took 10.1 s: 1.5× the objects for 1.15× the time, not 2.25× as a quadratic per-item cost
would give. This is consistent with the index working and is not proof the previous code was quadratic — that
would need running a knowingly slower operator, which is a separate exercise.

## What it does not establish

- **The peak is a lower bound.** 600 is the queue holding every object created, which is what a 2,457/sec
  offer against a 160/sec service rate produces, but a spike opening and closing inside one 203 ms sample is
  invisible. The record carries the achieved interval so the bound is legible.
- **One run.** No variance, no distribution, nothing about behaviour under sustained rather than burst load.
- **Nothing about GPUs.** These MLTrainingJobs request one device each against a fake device plugin; they are
  admitted and reconciled, not executed.
- **`MaxConcurrentReconciles` is unset**, so this is the single-worker rate. Raising it is the obvious next
  measurement and would change the number by construction.

## A wrong number caught before it was written down

The first run of this harness reported **0 reconciles** while draining 600 items. The two metric families
label the same controller differently — workqueue series carry `name=`, controller-runtime's reconcile
counter carries `controller=` — and the filter required `name=` on all three.

It was caught because zero is obviously wrong for a queue that visibly drained. A filter that had been wrong
by ten percent instead of by everything would have been written down.

## Reproducing

    kubectl create clusterrolebinding queueload-metrics-reader \
      --clusterrole=gpu-platform-control-plane-metrics-reader \
      --serviceaccount=gpu-platform-control-plane-system:gpu-platform-control-plane-controller-manager
    kubectl -n gpu-platform-control-plane-system create token \
      gpu-platform-control-plane-controller-manager --duration=2h > /tmp/metrics.token
    kubectl -n gpu-platform-control-plane-system port-forward \
      deploy/gpu-platform-control-plane-controller-manager 18443:8443 &

    go run ./cmd/queueload -count 600 -concurrency 32 -sample 200ms \
      -token-file /tmp/metrics.token -out ./ex/queue-load.json

The harness refuses to start if the workqueue already holds anything: a run that inherits somebody else's
backlog cannot attribute its peak to itself.
