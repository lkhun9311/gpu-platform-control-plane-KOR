# The tenant GPU quota was a convention, not a boundary

`GPUQuotaPolicy.spec.trainingQuota` makes the Kueue ClusterQueue the single authority over a namespace's GPU
budget, and the controller deliberately stops the namespace `ResourceQuota` from capping GPUs so the same
device is not charged twice. Kueue only sees what enters its queues.

So: what stops a tenant from creating a Pod directly?

## Before: nothing

    kubectl apply -f - <<'EOF'
    apiVersion: v1
    kind: Pod
    metadata: {name: bypass-probe, namespace: serving}
    spec:
      containers:
        - {name: c, image: busybox:1.36, command: ["sh","-c","sleep 30"],
           resources: {limits: {"nvidia.com/gpu": "2"}}}
    EOF

    pod/bypass-probe created
    bypass-probe   Running   platform-worker2   2
    $ kubectl -n serving get workloads
    (none)

Created, scheduled, Running, holding two devices, with no Kueue Workload anywhere. Nothing admitted it and
nothing is counting it. "Multi-tenant GPU quota" was a convention the platform's own controller followed.

## The first guard, and how it was broken

The first version read the Pod's `ownerReference` and trusted it: a Pod whose controller was a queued Job was
admitted. It passed ten unit specs and four live cases, and it was wrong.

    # create a real Job in a real queue
    kubectl apply -f (Job "decoy", labelled kueue.x-k8s.io/queue-name: gpu-premium)

    # then claim it as your Pod's controller
    kubectl apply -f (Pod, ownerReferences: [{kind: Job, name: decoy, uid: <decoy's uid>, controller: true}],
                      resources: {limits: {"nvidia.com/gpu": "2"}})

    pod/forged-owner created
    forged-owner   1/1   Running

`ownerReferences` is supplied by whoever creates the object. Kubernetes does not check that the named owner
had anything to do with it. **Everything on a Pod is written by the party the guard is trying to constrain.**

The same was true of the exemption: a tenant can annotate their own Pod, so an exemption honoured for anyone
is the bypass with an extra step.

## The guard

A validating webhook on Pod CREATE, scoped by namespace label, `failurePolicy: Fail`.

The rule is **not** "the Pod carries a queue label". That was the first design and it is wrong: `BuildJob`
puts the queue label on the JOB's metadata, and the Job controller creates Pods from `spec.template`, which
carries no such label. A Pod-level check would refuse every training Pod this platform creates.

The rule has two halves, and the second is the one that matters: the Pod's **controller** is a `batch/v1` Job
carrying the queue label, **and the request comes from the Job controller** —
`system:serviceaccount:kube-system:job-controller`. The requester is the only field in an admission request a
tenant cannot write, so it is the only thing an ownerReference can be believed on. The exemption is honoured
only for `kube-system` service accounts for the same reason. Only a Job, because
in this cluster that is the only shape Kueue governs — its `pod` and `deployment` integrations are not among
the enabled frameworks, so a Pod owned by a ReplicaSet is invisible to Kueue however it is labelled.
Admitting it on a label Kueue never reads would be a guard that checks a string rather than a fact.

An ownerReference naming a Job that does not exist is refused: it is a field anyone can write.

## After: four cases on the live cluster

| case | result |
|---|---|
| bare Pod, 2 GPUs, unlabelled namespace | created — the guard is scoped, and says so |
| **forged ownerReference to a real queued Job** | **Forbidden**, naming the requester |
| **tenant annotating its own Pod exempt** | **Forbidden**, naming the reserved annotation |
| bare Pod, 2 GPUs, governed namespace | **Forbidden**, with the remedy in the message |
| Pod from a Job labelled `kueue.x-k8s.io/queue-name` | **admitted**, no warning |
| Pod annotated `quota-exempt` | admitted **with a warning naming the annotation** |

The refusal:

    Error from server (Forbidden): admission webhook "vgpupod.kb.io" denied the request:
    pods "bypass-probe2" is forbidden: this namespace's GPU budget is held by a Kueue ClusterQueue,
    and this Pod asks for 2 nvidia.com/gpu without belonging to a queue, so nothing would charge it:
    submit an MLTrainingJob, or label the owning Job kueue.x-k8s.io/queue-name=<queue>, or annotate
    the Pod platform.lkhun9311.github.io/quota-exempt to take the device outside the budget on purpose

## A claim in this document was false, and the guard was built on it

The paragraph above said Kueue's `pod` and `deployment` integrations were not enabled here, so only a Job
could be governed and a serving Pod was invisible to Kueue however it was labelled.

That is wrong. Seventeen frameworks are enabled, including `pod`, `deployment` and `statefulset`. The
conclusion came from reading the first twelve lines of a truncated `grep`, and it survived into the guard's
design, its code comments and this write-up.

What it changes is what the label MEANS. It is not a permission a tenant grants itself; it is a bill:

    # a bare Pod, no owner, labelled with a queue
    kubectl apply -f (Pod, labels: {kueue.x-k8s.io/queue-name: gpu-premium}, limits: {nvidia.com/gpu: 1})

    $ kubectl -n serving get workloads
    pod-forged-both-64bfd   gpu-premium   gpu-premium   True   8s     <- admitted, charged

Forging the label does not obtain free capacity. It obtains METERED capacity, which is the objective. So the
guard accepts a labelled Pod outright, and the serving path works: a Deployment labelled with a queue has the
label propagated onto its Pods by Kueue's own webhook, along with `managed=true`, and gets an admitted
Workload.

`managed=true` is deliberately NOT the signal. A Pod carrying it with no queue name runs with no Workload at
all — checked, not assumed.

The Job path is different again and needs the owner check: Kueue governs a Job through the JOB object and
does not propagate the label down, so a Pod the Job controller created for a queued Job carries neither
label. Also checked.

## Putting serving on the quota path

Serving Pods were denied because nothing charged them, so the fix is to charge them.

The `InferenceDeployment` controller now stamps `kueue.x-k8s.io/queue-name` on the Deployment it renders,
derived from the namespace's `GPUQuotaPolicy` rather than taken from the InferenceDeployment spec. That
choice is the point: a tenant naming its own queue could name ANOTHER tenant's and spend somebody else's
budget. Serving does not get to choose which budget it spends.

Measured end to end after deploying:

    Deployment queue-label=gpu-premium
    quota-serving-79c86774f-r8p4r   Running   queue=gpu-premium   managed=true
    WL: pod-quota-serving-...-5c1eb   gpu-premium   gpu-premium   True     <- admitted

Then with the guard turned ON in that namespace: the serving Pod is admitted (zero denial events), and a bare
GPU Pod is still Forbidden. The two now coexist, which they did not before.

The order matters: existing serving Deployments do not carry the label until their controller reconciles
them, and turning the guard on first denies their Pods in the window between. So the sequence is reconcile
serving, then label the namespace.

**The guard is now on** in `serving`, and both directions hold there:

    serving Pod recreated          -> admitted, 0 admission denials on the ReplicaSet
    bare Pod, 1 nvidia.com/gpu     -> Forbidden

## What it broke before that, and how it was found

An `InferenceDeployment` — a first-class API of this platform — could not create its Pods in a governed
namespace. Verified rather than reasoned about:

    Warning  FailedCreate  replicaset-controller
      Error creating: admission webhook "vgpupod.kb.io" denied the request:
      "system:serviceaccount:kube-system:replicaset-controller" is asking for 1 nvidia.com/gpu directly
      rather than through a queue

Serving Pods are owned by a ReplicaSet, and the guard accepts only a Job. That is not an oversight in the
rule; it is the rule being right about a platform that is wrong. In this cluster Kueue's `deployment` and
`pod` integrations are not among the enabled frameworks, so a serving Pod is invisible to Kueue **however it
is labelled** — there is no queue for it to belong to. Admitting it would mean accepting a label Kueue never
reads, which is the string check this guard exists to replace.

So the guard is correct and **the `serving` namespace has been unlabelled again**. No namespace in this
repository is currently governed by it. That is the honest state: the enforcement mechanism is built, tested
against forgery, and cannot be turned on until serving genuinely has a quota to be charged against.

The next step is not to weaken the guard. It is to put serving on the quota path — Kueue's `deployment`
integration already charges a labelled serving Deployment against the same ClusterQueue a training Job uses,
which `hack/kueue-deployment-integration.md` measured before any of this was written.

## What it costs

**Availability.** `failurePolicy: Fail` means that while the webhook is down, no GPU Pod can be created in a
governed namespace at all. That is the correct direction for a quota boundary — refusing work is recoverable,
unaccounted GPU consumption is not — but the webhook's availability is now part of the platform's.

**A named hole instead of an unnamed one.** The exemption annotation lets a device plugin, or the serving
path, hold a device outside the budget. It is deliberate: serving genuinely cannot go through Kueue in this
configuration. What makes it different from the bypass is that it is written down, warned about at the moment
it is used, and enumerable with one query.

**Scope.** The webhook is bound to a namespace label rather than applied cluster-wide. A Pod webhook that can
refuse `kube-system` is how a cluster becomes unbootable.

## Found while testing this

The `gpu-premium` ClusterQueue the policy controller creates references a ResourceFlavor named `gpu` that
nothing creates, so it sat `Active=False reason=FlavorNotFound` and admitted nothing. Training admission had
never worked end to end in that namespace. The flavor was created by hand to finish this verification; the
controller not creating it is a separate defect and is not fixed here.
