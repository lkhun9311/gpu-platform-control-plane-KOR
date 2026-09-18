# EKS cluster cycle, 2026-09-18: applied 96 resources, destroyed 96, and checked by ID

The `cluster` root was applied and torn down once, deliberately, to answer one question the repository
could not answer before: **does the teardown actually remove what was created?** Every earlier teardown run
in this account had nothing to remove.

All times are **UTC**. Where a fact came from a command the operator ran rather than from a workflow log or
an API response quoted here, it is marked **operator-reported**.

## Before

The `cluster` state was `serial 111`, `resources: []`. No EKS cluster, no VPC tagged `gpu-platform-vpc`, and
no NAT gateway, unassociated EIP or EC2 instance tagged `project=gpu-platform-control-plane` (operator-reported).

This is **not** the same as "the account was empty": `bootstrap` was applied and stayed applied throughout —
state bucket, KMS key, OIDC provider, CI roles, ECR. Those are the retained cost, and they are still there.

## Plan and apply

```
Plan: 96 to add, 0 to change, 0 to destroy
```

Checked against the values reviewed before spending: `api_public_access_cidrs=["<operator egress>/32"]`,
node group `cpu` desired 1 (t3.large), `gpu` / `gpu_shared` / `gpu_single` desired 0. An earlier saved plan
was discarded because the state had moved under it — two no-op teardown runs had bumped the serial — and a
fresh plan was taken and re-checked against the same values before applying.

```
Apply complete! Resources: 96 added, 0 changed, 0 destroyed.
```

Cluster `createdAt` `2026-09-18T12:08:01+09:00` = **03:08:01Z**. No errors. The SCP failure recorded in
`infra/aws/org/scp/README.md` — *"The first `terraform apply` on the cluster root failed on all four node
groups"*, `ec2:CreateLaunchTemplate ... explicit deny` — did not recur.

## What existed while it existed

| | observed |
|---|---|
| cluster | `gpu-platform`, `ACTIVE`, k8s `1.35` |
| endpoint | `endpointPrivate=true`, `endpointPublic=true`, public CIDR = the operator's `/32` only |
| node groups | `cpu` desired **1** ACTIVE t3.large; `gpu` (g4dn.12xlarge), `gpu_shared` and `gpu_single` (g5.xlarge) all ACTIVE at desired **0** |
| EC2 | exactly one instance, `i-03271b2f0dd693887`, t3.large, running. **No GPU instance was ever created** |
| NAT / EIP | `nat-0595fc0e33fb202c2` available; `eipalloc-086d9ea6b10b525b8` **associated** (so not the idle-EIP charge) |
| add-ons | coredns, kube-proxy, vpc-cni |
| Kubernetes | `kubectl get nodes` from the operator's laptop returned `ip-10-0-78-201` **Ready** (`v1.35.8-eks-a887778`, AL2023); `aws-node`, two `coredns` and `kube-proxy` all Running |

The `kubectl` result is the evidence that the `/32` endpoint decision worked **from the operator's network**.
It says nothing about any other network — see the teardown below, where it mattered.

## Inventory taken before the teardown, by real ID

Recorded so the teardown could be checked against identities rather than against tags, which is the weaker
test the workflow itself already makes:

```
vpc-056404e681b867967
vol-09d1f6de029932658   (20 GiB, in-use)
eni-0ec02188c522727bb  eni-0e72711f1e3a34f66  eni-06dc663a44dd9241c
eni-0590f1c981cd49142  eni-0d595d09d8b5653c1
8 subnets
/aws/eks/gpu-platform/cluster
```

## Teardown

Dispatched by hand rather than left to the nightly cron, because the cron's firing time drifts by hours
(`hack/m5b-gpu-session.sh` records it "observed firing between 03:53 and 14:57 UTC") and an unattended
teardown is a worse record than a watched one.

Run `35302920119`, **03:21:49Z → 03:35:24Z** (13 m 35 s).

| step | result |
|---|---|
| Check whether the cluster still exists | success |
| Check whether the Kubernetes API is reachable from this runner | success (the step ran) |
| Delete Argo CD applications | **SKIPPED** |
| Destroy argo-bootstrap state | **SKIPPED** |
| Destroy cluster state | success |
| Residue check | success |

The two skips follow from the probe, which wrote `reachable=false`:

```
03:22:30Z ##[warning]Kubernetes API is not reachable from this runner.
          The cluster endpoint is private and this runner is outside the VPC.
```

**That warning's stated cause is wrong for this run.** The endpoint was not private-only; its public half
was open to one `/32`, which does not include a GitHub-hosted runner. The probe turns every non-zero exit of
`kubectl get --raw /readyz` into `reachable=false` and discards the error, so it cannot distinguish a CIDR
refusal from an auth failure, a DNS failure or a readiness failure. The two warnings after it are accurate
and were the useful ones: run from a VPC-attached runner, or open the endpoint to a known CIDR first.

Nothing was lost today because no Argo CD was installed. Had it been, both ordered steps would have been
skipped the same way, and `destroy.yml` states what that leaves behind: *"the argo-bootstrap STATE FILE
describing resources that no longer exist"*.

```
03:34:50Z Destroy complete! Resources: 96 destroyed.
```

## Residue check — and what it did not check

```
03:34:57Z VPC:               <none>
          EC2 instances:     <none>
          EBS volumes:       <none>
          NAT gateways:      <none>
          Unassociated EIPs: <none>
          Load balancers:    <none>
          Network ifaces:    <none>
          no residue
```

**Two of those lines are not query results.** `LB` and `ENI` are initialised empty and only queried inside
`if [ -n "$VPC" ]`; with the VPC already gone, both printed their initial value. Reading them as "AWS was
asked and answered none" would be wrong, and it is the same shape of mistake this workflow was corrected for
earlier the same day.

The check is also scoped: one region, tag-based for EC2/EBS/NAT, name-tag-based for the VPC, and
`AssociationId==null` for EIPs — which means an EIP that is still **associated** is invisible to it. It does
not look at EKS itself, snapshots, S3, ECR, CloudWatch or Kubernetes PVCs.

## Verification after the teardown, by ID and exit code

Run separately from the workflow, so a query failure could not read as an absence:

| resource | result |
|---|---|
| EKS `gpu-platform` | **absent** — `ResourceNotFoundException` |
| EC2 `i-03271b2f0dd693887` | `terminated` |
| NAT `nat-0595fc0e33fb202c2` | `deleted` |
| EIP `eipalloc-086d9ea6b10b525b8` | **absent** — `InvalidAllocationID.NotFound` |
| VPC `vpc-056404e681b867967` | **absent** — `InvalidVpcID.NotFound` |
| EBS `vol-09d1f6de029932658` | **absent** — `InvalidVolume.NotFound` |
| all five ENIs | **absent** — `InvalidNetworkInterfaceID.NotFound`, each queried individually |
| log group `/aws/eks/gpu-platform/*` | prefix listing returned an empty list (rc 0) |

The EIP line is the one that matters most, because the workflow's own residue check could not have caught
that resource: it was associated while it existed, and the check only looks for unassociated ones.

State: `serial 126`, `resources: 0`, lineage unchanged. The object shrank **106,851 → 86,734 → 57,313 →
1,361 bytes** between 03:23:51Z and 03:34:51Z. The 2026-09-03 history shows the same shape; today's run is
evidence that a destroy produces that shape, **not** evidence about what happened on 2026-09-03.

## What is still there, and therefore still costs

One S3 bucket (`gpu-platform-tfstate-007635145730`, 3 objects, 80,757 bytes), the KMS key
`alias/gpu-platform-tf-state`, and two ECR repositories (19 and 24 images). `aws eks list-clusters` returns 0.

The claim this run supports is **"the cluster's usage ended and the retained pieces are these"** — not
"the account bills nothing". It also does not undo the cost already incurred by the run itself.

## Not exercised

This cycle's cluster had **no load balancer, no Argo CD, no GPU node and no PVC**, so the failure modes the
teardown was written for went untested:

- Argo re-creating children mid-teardown, and the cascade finalizer removing a LoadBalancer that would
  otherwise block the VPC destroy
- an ENI left by a load balancer or VPC endpoint blocking subnet and VPC deletion
- the residue check **failing** the job on a surviving resource — it has still only ever passed
- a denied or throttled query being refused rather than read as "none"
- GPU node termination, GPU quota, dynamic EBS from PVCs
- the nightly cron firing unattended, the cancel path, and the TTL watchdog

One clean cycle on the smallest possible cluster is what this is. It is not a validated teardown path.
