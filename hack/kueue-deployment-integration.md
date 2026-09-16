# Does Kueue already unify the training and inference budget?

Scripted in [`kueue-deployment-integration.sh`](kueue-deployment-integration.sh). Runs on kind, needs no GPU.

## Why this ran before any code was written

The plan was to replace this project's `InferenceDeployment` with KServe and keep a "quota unification
layer": a controller making KServe GPU consumption visible to Kueue, so training jobs and serving replicas
draw on one tenant budget.

A review pointed out that Kueue v0.18.3, the version already in `go.mod`, ships a `deployment` integration
that does exactly this, and that the whole controller would collapse into setting one label. It also named
the first question an interviewer would ask: *why did you build a controller before testing the built-in
integration?*

So the integration was tested first.

## What was measured

A `ClusterQueue` with nominal `nvidia.com/gpu: 1`. One Deployment carrying
`kueue.x-k8s.io/queue-name`, standing in for the Deployment KServe generates in Standard mode, requesting
one GPU. One training Job requesting the same GPU.

```
workloads   pod-serving-5fcf6dbc68-srlsl-5d21b    admitted: True
            job-training-6b139                    admitted: (pending)

ClusterQueue kit-cq:   used=1   admitted=1   pending=1
```

**One budget, shared, with no custom code.** The serving pod holds the single GPU and the training Job
waits behind it.

No GPU is needed for this because Kueue reserves quota at ADMISSION, before the scheduler looks for a node.
The admitted pod stays `Pending` on a cluster with no real device, and that is irrelevant to what is being
measured.

The `deployment` and `pod` integrations are opt-in and were not enabled on this cluster; they were added to
the Kueue config for the run. `manageJobsWithoutQueueName` stays false, so pods without the queue label are
ignored and nothing else on the cluster changes behaviour.

## Verdict

The proposed controller is unnecessary on this path. Building it would have duplicated Kueue's accounting
and gone wrong the first time a replica scaled, a rollout overlapped, or a transformer container appeared.

## What is actually left

KServe owns the generated Deployment and reconciles it continuously, so whether the queue label survives is
a **field-ownership** question rather than an accounting one. This repository already has that problem
elsewhere: the reconciler writes a Job's pod template only on create, because Kueue owns `suspend` after
admission and the template is immutable anyway.

If the label does not propagate from the `InferenceService`, the legitimate contribution is label
propagation or an admission mutation, not a second ledger competing with Kueue's. That question is smaller
and more honest than the layer originally planned, and it is the next thing to test.

## Reproducing

```bash
# Enable the integrations (opt-in), then:
./hack/kueue-deployment-integration.sh
```

The script creates its own namespace, flavor and queues, and removes them on exit.
