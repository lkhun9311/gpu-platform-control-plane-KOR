# Service control policies — a threat/control contract

An SCP is the only control in this account that a compromised administrator cannot lift. It is also the only
control that can stop a teardown while a GPU bills by the hour. Both facts are why this directory exists
before the organization does: the policy is written, argued and simulated first, and the account is created
second.

All three are attached, in organization `o-vzs8elbcfp`, since 2026-08-30. See "Where each policy is
attached" below for which target carries which, and why they are not all in one place.

## What an SCP uniquely buys

Everything else in this repository is administered from inside the account it protects. An IAM policy, a
permissions boundary, a service quota and a budget alarm are all editable by whoever holds administrator
access — which is precisely who an attacker becomes. An SCP is attached in the management account and caps
what any principal in the member account may do, **including that account's own root user**.

The concrete attack it stops, and nothing else here does:

> An attacker obtains an administrator session in the lab account and performs an action that is allowed by
> every IAM policy in that account — launching accelerators in an unmonitored Region, switching to an
> instance family nobody budgeted for, disabling the trail, or removing the account from the organization to
> escape the ceiling entirely.

A budget alarm reports that after the cost data arrives, typically hours later. Identity Center shortens how
long a stolen credential lives. Neither is an authorization ceiling.

## What it must not break

This is the half that gets skipped, and it is the half that costs money. A denied `TerminateInstances` or
`DeleteNodegroup` does not fail safely: the GPU keeps billing while the cleanup path is refused.

Evidence used to enumerate the allowed paths, gathered 2026-08-29:

| Source | Finding |
|---|---|
| `terraform show -json` of the cluster plan | Namespaces created: `eks`, `iam`, `ec2`, `vpc`, `kms`, `cloudwatch`, plus NAT/EIP/IGW/route |
| CloudTrail, `ap-northeast-2`, last 50 events | `sts`, `s3`, `kms`, `ec2`, `resource-explorer-2`, `cloudcontrolapi` — **every event in `ap-northeast-2`** |
| CloudTrail, `us-east-1`, last 30 events | `freetier`, `ce`, `billing`, `organizations`, `notifications` — **billing and cost APIs only, no workload** |
| `infra/aws/cluster/variables.tf` | Instance types: `t3.large`, `g5.xlarge`, `g4dn.12xlarge` |

So the workload Region is exactly one, and `us-east-1` carries only global billing endpoints.

## The policies

### `deny-region.json` — pin the workload to one Region

Denies everything outside `ap-northeast-2` and `us-east-1`, with an exemption list for the global services
whose endpoints live in `us-east-1` or nowhere.

`us-east-1` is not exempted as a workload Region by accident: `aws pricing`, Cost Explorer, `freetier` and
Organizations are only reachable there, and this repository calls all four. The exemption list is what keeps
the deny from breaking them, and it is the part to review when a new global service is adopted.

**What this stops:** an administrator session launching accelerators in `us-west-2`, where no budget filter,
no quota request and no operator attention is pointed.

### `deny-instance-family.json` — pin the accelerator families

Denies `ec2:RunInstances` and `autoscaling:*` launches whose instance type is outside the three the study
uses, and denies changing an EKS node group's instance types to anything else.

**What this stops:** the same session switching from `g5.xlarge` at $1.237/hour to a `p5` at two orders of
magnitude more, inside the approved Region, under the approved quota family.

Note the interaction with quota: the G/VT quota already caps G-family vCPU at 52. It does **not** cap P, Trn
or Inf families, which have their own quotas and their own defaults. The SCP is what makes the family list
explicit rather than implied by which increases happened to be requested.

### `deny-escape.json` — pin the account inside the ceiling

Denies `organizations:LeaveOrganization`, `account:CloseAccount`, and disabling the trail or its logging.

**What this stops:** removing the ceiling before doing anything else. AWS recommends this pairing in its
management-account guidance, and it is the one policy here with no operational downside — the lab has no
reason to leave its own organization.

## Simulation before attachment

An SCP cannot be tested by attaching it and watching. The order is:

1. **Enumerate from evidence, not intent.** The table above, refreshed. `aws cloudtrail lookup-events` over
   the window covering a full session, plus `aws iam get-service-last-accessed-details` for each role.
2. **Dry-run every deny against the recorded calls.** For each denied action, confirm no recorded event
   matches it. A match means the deny would have broken something that already ran.
3. **Walk the teardown path by hand.** `destroy.yml`'s ordered steps, `hack/*.sh` cleanup traps, and the node
   group scale-down. Every call one of them makes must survive the policy.
4. **Attach to an empty OU first**, move nothing into it, and confirm the organization accepts the JSON.
5. **Attach to the lab account only after a complete non-GPU apply/destroy cycle has run under it.**

The order that must never happen: attach, then discover during a paid run.

## Not written yet, and why

- **A permissions ceiling on the CI apply role.** It holds `PowerUserAccess` plus `IAMFullAccess`. An SCP
  denying IAM writes would break it, so narrowing that role is an IAM problem first and an SCP problem after.
- **A deny on disabling GuardDuty/Config.** Neither is enabled; denying the disablement of something that
  does not exist is decoration.
- **Anything about data exfiltration.** The account holds one Terraform state and no data. A `aws:PrincipalOrgID`
  perimeter would be theatre here and would complicate the public-repo OIDC path.

## What the simulation changed

`simulate.sh` was written to prove the policies do not break a recorded call, and the first run found
something the policy author had not: **`eks:UpdateNodegroupConfig` was on the deny list and is also on the
teardown path.**

That call is how a session scales a GPU node group back to zero. The deny was conditional on
`eks:instanceTypes`, and the reasoning was that a scale-down sends no instance types, so the condition would
not match. That reasoning is correct and it was the wrong thing to rely on:

```
aws eks update-nodegroup-config
  --cluster-name --nodegroup-name [--labels] [--taints]
  [--scaling-config] [--update-config] [--node-repair-config] [--warm-pool-config]
```

The API does not accept instance types at all — a node group's instance types are immutable after creation.
So the statement could never deny anything, while sitting on the one path whose failure costs money rather
than saving it.

The general form of the finding: **a deny that cannot fire is not free if it is on the cleanup path.** It
survives review by being harmless in theory, and every future reader has to re-derive that it is harmless.

And the fix exposed a second one in the checker itself. `simulate.sh` had the watched action list written
twice — once in the policy, once hardcoded in the script — so after the policy dropped
`UpdateNodegroupConfig` the script kept warning about it. It reads the actions out of the JSON now.

That is the sixth time this week a value has been carried by hand across two files with nothing failing when
they drift. The first version of a checker is as prone to it as the thing being checked.

## The third defect in one statement, and the one the simulation could not see

`DenyUnapprovedInstanceTypesOnManagedNodeGroups` is gone. Organizations refuses to create a policy
containing it: `eks:instanceTypes` is not a condition key EKS publishes, and the console rejects it with
`Invalid Service Condition Key`.

That statement had already been through two rounds of review. The first found that it also named
`eks:UpdateNodegroupConfig`, a call on the teardown path that could never match its own condition. The
second found that `simulate.sh` had duplicated the policy's action list by hand and kept warning after the
policy stopped denying. **Neither round asked whether the condition key existed**, and the simulation
structurally cannot: it substitutes recorded calls into the policy and reports what would have been denied,
so a key that does not exist simply never matches and the run comes back clean.

A deny that cannot fire and a deny that cannot be *created* look identical to a simulator that only asks
what a policy would have done.

`simulate.sh` now asks the other question first, through IAM Access Analyzer:

```
aws accessanalyzer validate-policy --policy-document file://<policy> \
    --policy-type SERVICE_CONTROL_POLICY
```

which returns the same finding code the console shows. Verified by putting the rejected statement back and
watching the run fail.

### What this leaves uncovered

Managed node groups are no longer constrained by instance type at the SCP layer. Three things still stand
between the account and an unapproved family, and none of them is the guard that was removed:

- `DenyUnapprovedInstanceTypesOnRunInstances` and the launch-template statement cover the EC2 calls a node
  group's instances are launched through, **except where EKS uses a service-linked role** — SCPs do not
  apply to service-linked roles, so this is partial.
- The G/VT quota caps that family at 52 vCPU on demand and 8 on spot.
- P-family quotas are **0** in both purchasing options, so the most expensive accelerators cannot launch at
  all — which is what the removed statement was mostly protecting against.

The gap is real and worth naming rather than papering over: an instance family with a nonzero default quota,
launched by EKS through a service-linked role, is no longer denied by this directory.

## Where each policy is attached, and why not all in one place

Two of these describe **this lab**, and one describes **the organization**. Attaching them to the same
target would blur that distinction, and the distinction is what tells a later reader which line to edit.

```
Root  ──  FullAWSAccess          (AWS creates and attaches this; removing it denies everything)
      ──  deny-escape            no member account may leave the organization or close itself

Lab OU ── deny-region            this lab runs in ap-northeast-2
       ── deny-instance-family   this lab runs t3 / g4dn / g5
          │
          └── 007635145730       inherits all four
```

`deny-region` and `deny-instance-family` are statements about a workload. A second OU holding something
other than this lab should not inherit them, and putting them at Root would mean it did.

`deny-escape` is a statement about the organization: **no member may remove itself from the ceiling.** A
member account created tomorrow should be covered the day it is created, and only a Root attachment does
that. It also protects the one asset in this project that code cannot rebuild — the GPU quota, which lives
on account 007635145730 and disappears with it.

The management account carries none of these, because SCPs never apply to it. That is also the recovery
path: if a policy here breaks something, it is detached from a console that no policy here can reach.

## What was verified after attaching

Simulation against recorded calls is necessary and not sufficient, so the attached policies were exercised
in both directions on 2026-08-30.

| Direction | Call | Result |
|---|---|---|
| must deny | `ec2:DescribeInstances` in `us-west-2` | `explicit deny in a service control policy` |
| must deny | `ec2:RunInstances` with `m5.large` | `UnauthorizedOperation ... explicit deny` |
| must permit | `ec2:RunInstances` with `g5.xlarge`, `t3.large` | `DryRunOperation` — would have succeeded |
| must permit | `TerminateInstances`, `Describe*`, `eks list-*` | not denied |
| must permit | S3 state, KMS, ECR, IAM, `us-east-1` pricing and Organizations | not denied |
| must permit | `terraform plan` → `apply` → `plan` on bootstrap | converged, `No changes` |

The must-permit rows matter more than the must-deny ones. A denied `TerminateInstances` does not fail
safely: the GPU keeps billing while cleanup is refused.

The first attempt at the two `RunInstances` rows **proved nothing and looked like it passed**. It used a
malformed AMI id, and EC2 validates the id before it evaluates authorization, so the approved and
unapproved instance types returned the identical `InvalidAMIID.Malformed`. A test whose two arms fail the
same way for a reason unrelated to what it is testing is not a test.

## The launch-template statement is gone too, and the first cluster apply is what removed it

`DenyUnapprovedInstanceTypesOnLaunchTemplates` denied every `ec2:CreateLaunchTemplate` call in the account.
The first `terraform apply` on the cluster root failed on all four node groups:

```
Error: creating EC2 Launch Template (cpu-...): UnauthorizedOperation
  ec2:CreateLaunchTemplate ... explicit deny in a service control policy: p-bzjfxxsk
```

**IAM's negated condition operators evaluate to true when the key is absent.** EKS managed node groups put
the instance type on the node group, not in the launch template, so `ec2:InstanceType` was missing and
`StringNotLike` matched everything. The statement did not restrict a family; it blocked the service.

Adding `"Null": {"ec2:InstanceType": "false"}` so the deny applies only when the key is present fixed the
outage and revealed the second half of the problem: with the guard in place, a launch template carrying
`m5.large` was **also** permitted. The same key, in the same policy, denies `m5.large` on `ec2:RunInstances`
and permits it on `ec2:CreateLaunchTemplate` — `ec2:InstanceType` is simply not populated for that action.

So the statement cannot work either way: without the `Null` guard it denies everything, with it it denies
nothing. It is removed rather than left as decoration, for the reason already given above about a deny that
cannot fire.

### Verified after removal, in both directions

| Call | Result |
|---|---|
| `CreateLaunchTemplate` with no instance type (the shape EKS creates) | permitted |
| `RunInstances` `g5.xlarge` | permitted |
| `RunInstances` `m5.large` | **denied** |
| `DescribeInstances` in `us-west-2` | **denied** |

### What now guards instance family

Only `ec2:RunInstances`, and **SCPs do not apply to service-linked roles**, so a managed node group's own
launches are not covered by it. The real ceiling on accelerator spend is the quota: G/VT at 52 on-demand and
8 spot, and **P-family at 0 in both purchasing options**.

### The precondition this violated

Step 5 of "Simulation before attachment" above says to attach only after a complete non-GPU apply/destroy
cycle has run under the policy. That step was skipped: the SCPs were attached on 2026-08-30 with the cluster
root never applied, and this defect surfaced on the first paid apply instead of before attachment. The
rehearsal cost about $0.05 and ran 20 minutes; the same defect found during a GPU session would have cost
the session.

