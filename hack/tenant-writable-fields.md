# Every guarantee is bounded by the fields the tenant can write

Three separate defences in this platform were broken the same way, and each was found by attacking it rather
than by reading it. They are collected here because the pattern is the finding, not any one of them.

| the guarantee | the field the tenant writes | what it bought the attacker | measured |
|---|---|---|---|
| GPU quota is enforced | `ownerReferences` | 2 devices, unaccounted | one `kubectl apply` |
| GPU quota is enforced | `quota-exempt` annotation | any number of devices | one annotation |
| a reclaimed device comes back | `terminationGracePeriodSeconds` | 301 s of a reclaimed device | 30 s → 32 s held; 300 s → 301 s |
| the budget is available to those who can use it | a Job whose Pods are refused | the whole budget, indefinitely | used=2 for 240 s, `failed` stuck at 0 |

## 1. An ownerReference is a claim, not a fact

The first quota guard admitted a Pod whose controller was a Job carrying the queue label. It passed ten unit
specs and four live cases.

    kubectl apply -f (Job "decoy", labelled kueue.x-k8s.io/queue-name: gpu-premium)
    kubectl apply -f (Pod, ownerReferences: [{kind: Job, name: decoy, uid: <decoy's uid>, controller: true}],
                      limits: {"nvidia.com/gpu": "2"})
    pod/forged-owner created
    forged-owner   1/1   Running

`ownerReferences` is supplied by whoever creates the object, and Kubernetes does not check that the named
owner had anything to do with it.

**Fix.** Believe the reference only when the Job controller presents it — `req.UserInfo.Username` is the one
field in an admission request a tenant cannot write. That required a raw `admission.Handler`; the typed
`Validator` interface is handed only the object, and the object is exactly what cannot be trusted.

## 2. An exemption anyone can grant is not an exemption

The same guard honoured `platform.lkhun9311.github.io/quota-exempt` on any Pod. Infrastructure needs it —
a device plugin holds a device outside any tenant budget — but a tenant can annotate its own Pod, so it was
the bypass with an extra step.

**Fix.** Honour it only for `kube-system` service accounts, which a tenant cannot assume. Admitted Pods
still emit a warning naming the annotation, so a device leaving the budget announces itself.

## 3. The reclaim guarantee is bounded by a number the victim chooses

Quota reclaim works by preemption, and preemption waits for termination. Termination waits for
`terminationGracePeriodSeconds`, which is set by the Pod being preempted.

Measured directly: a device-holder deleted with grace 30 held its device for 32 seconds; the same Pod with
grace 300 held it for 301. Sweeping the parameter against a fixed 43 seconds of remaining work gives the
model, and it holds — `held = min(remaining, grace)`, with the knee where predicted
([grace-holds-the-device.md](grace-holds-the-device.md)).

The queuelab measured the same boundary from the other side: an unresponsive workload defeats reclaim
entirely while its remaining service fits inside grace, and is cut off at exactly grace once it does not
([queuelab-grace-boundary.md](queuelab-grace-boundary.md)).

**Fix.** Cap it at 120 s for device holders. Not 30, because a training step that checkpoints on SIGTERM
needs longer, and refusing that pushes tenants to ignore SIGTERM — which the measurement shows is what costs
the owner most.

## What they have in common

Each guarantee was expressed in terms of a value the adversary supplies. Not a bug in the check — the checks
were correct about what they read. The mistake was reading something the tenant writes and treating it as
evidence about the tenant.

The three fixes take different forms, and the difference is instructive:

- **1** replaced the untrusted input with a trusted one. The requester is authenticated; the object is not.
- **2** restricted who may supply the input, to a set the tenant cannot join.
- **3** could do neither — the grace period genuinely belongs to the workload — so it **bounded** the damage
  instead. That is the weakest of them and the honest one: reclaim is still not immediate, and no Kubernetes
  mechanism makes it so.
- **4** moved the check to where the commitment is made. The rule had not changed; it was being applied after
  the thing it was meant to prevent had already happened.

## 4. A reservation made for Pods that can never exist

Found while testing the third fix, and it is the same shape again: refusing the Pod was too late.

Kueue admits a Job by looking at the Job. A Job whose Pods are then rejected keeps its admission — the Job
controller retries, every Pod is refused, and the Workload sits admitted holding quota. Measured:

    t=20s   used=2 admitted=2  job.status.failed=0
    t=240s  used=2 admitted=2  job.status.failed=0

`backoffLimit` cannot end it. An admission rejection is not a Pod failure, so `failed` never leaves 0 and the
retry never stops. A tenant submitting such Jobs holds a budget indefinitely while running nothing.

**Fix.** Apply the rules that decide a Pod's fate to the template that would produce it, at the point where
the reservation is made. A Job asking for devices is now refused if its template's grace exceeds the cap, or
if it carries no queue label — the latter checked at the Job because that is where Kueue reads it, and
refusing at the Pod would tell a user to fix a field their Pod does not have.

Re-attacked after deploying: both Jobs Forbidden, `used` unchanged at 1, and an ordinary queued Job with
grace 90 still runs.

## What the four have in common
