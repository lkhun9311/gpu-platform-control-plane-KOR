# Service control policies — a threat/control contract

An SCP is the only control in this account that a compromised administrator cannot lift. It is also the only
control that can stop a teardown while a GPU bills by the hour. Both facts are why this directory exists
before the organization does: the policy is written, argued and simulated first, and the account is created
second.

Nothing here is attached yet. There is no organization.

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
than saving it. It now names `eks:CreateNodegroup` only.

The general form of the finding: **a deny that cannot fire is not free if it is on the cleanup path.** It
survives review by being harmless in theory, and every future reader has to re-derive that it is harmless.

And the fix exposed a second one in the checker itself. `simulate.sh` had the watched action list written
twice — once in the policy, once hardcoded in the script — so after the policy dropped
`UpdateNodegroupConfig` the script kept warning about it. It reads the actions out of the JSON now.

That is the sixth time this week a value has been carried by hand across two files with nothing failing when
they drift. The first version of a checker is as prone to it as the thing being checked.
