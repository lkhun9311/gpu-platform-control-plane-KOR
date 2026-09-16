# Does a queue label on an InferenceService reach the Deployment KServe generates?

Scripted in [`kserve-queue-label-propagation.sh`](kserve-queue-label-propagation.sh). Runs on kind with
KServe in RawDeployment mode. No GPU.

## Why this was the only question left

The plan was a quota unification layer: a controller making KServe GPU consumption visible to Kueue so
training and inference draw on one tenant budget.

[The previous experiment](kueue-deployment-integration.md) showed Kueue already charges a labelled
Deployment's pods against the same ClusterQueue a training Job uses. That left one thing unknown: KServe owns
the generated Deployment and reconciles it continuously, so does a queue label declared on the
`InferenceService` reach that Deployment, and does it stay there.

## Survival is the point, not arrival

A label that appears once and is stripped on the next reconcile is worse than one that never appears. Quota
holds until something triggers a reconcile, then stops, with no error anywhere. So the label is checked
twice, with a spec change in between to force KServe to rewrite the Deployment.

## Result

```
KServe generated       deployment.apps/label-probe-predictor
label after create     klt-lq
label after reconcile  klt-lq
Kueue Workloads        1  (before and after)
```

The label propagates and survives. Kueue creates a Workload for the serving pod, which means the serving
replica is drawing on the same ClusterQueue a training Job queues against.

**Unifying the budget across serving and training is configuration, not code.**

No GPU is needed because Kueue reserves quota at admission, before the scheduler looks for a node. The pod
staying `Pending` on a device-less cluster does not affect what is measured.

## What this closed

The controller was removed from the design before any of it was written. Building it would have created a
second quota ledger beside Kueue's, and that ledger would have been wrong the first time a replica scaled, a
rollout overlapped, or a transformer container appeared beside the predictor.

Two scripts, both runnable on a laptop, replaced an implementation.

## What is still open

The label was set on both `InferenceService.metadata.labels` and `spec.predictor.labels`, so this run does
not establish which of the two KServe honours. A follow-up should set only one at a time. That matters for
documentation, not for the decision: either way the answer is configuration.

Also untested: multiple predictor components (transformer, explainer), where each generates its own
Deployment and each would need the label; and scale-to-zero, where the Workload's lifecycle against a
zero-replica Deployment is unverified.
