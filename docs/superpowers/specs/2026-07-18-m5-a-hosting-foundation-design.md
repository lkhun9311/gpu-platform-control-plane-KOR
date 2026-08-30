# M5-a Increment 1 — AWS hosting foundation (design spec, v1)

> Status: designed, code-only. No `terraform apply`, no AWS resources provisioned.
> Design of record it implements: `docs/09_AWS_INFRA_ARCHITECTURE.md` and
> an internal integration design (v3.2), kept as a working document and not published.

## Purpose

Write the first, foundational slice of the M5-a hosting layer as reviewable
infrastructure code: the Terraform that stands up the state backend and the EKS
cluster, plus the CI wiring that builds the operator image and plans/applies the
infrastructure. Nothing is provisioned. The deliverable is code that validates
offline and is ready to `apply` later, on demand.

The operator is cloud-agnostic; its only AWS dependency is hosting. This
increment builds that hosting foundation and stops before GitOps, observability,
and GPU, which are later increments.

## Scope

In scope (Increment 1):

- Terraform `infra/aws/bootstrap/`: S3 state bucket, DynamoDB lock table, GitHub
  OIDC provider, ECR repositories.
- Terraform `infra/aws/cluster/`: VPC (public subnets in two AZs), EKS (pinned
  version) with managed add-ons, a CPU managed node group pinned to one AZ, node
  IAM, and EKS access entries.
- `.github/workflows/ci.yml`: `go test` (envtest) then build and push the
  operator image to ECR via the OIDC image-push role.
- `.github/workflows/infra.yml`: `terraform plan` on PR (plan role), `terraform
  apply` on merge behind a manual-approval environment (apply role).
- Offline verification tooling and a documented verification procedure.

Out of scope (later increments), stated so the boundary is explicit:

- Any real provisioning. This increment is code-only by decision.
- Terraform `infra/aws/argo-bootstrap/`, the Argo CD app-of-apps manifests,
  `gpu.yml`, and `destroy.yml`. These are Increment 2.
- The GPU node group and device-plugin simulator. These are M5-b.
- Operator custom metrics (quota-reject / reconcile-outcome counters). The
  design of record calls this out as a hidden code-backlog prerequisite for the
  observability profile (build-order step 5). It is operator code, not infra,
  and is tracked separately.
- The gateway image build in CI. Its Dockerfile now exists (M4-b), so a later
  change can add the gateway build to `ci.yml`; this increment builds only the
  operator image to keep the slice small.

## Repository scope

Infrastructure code lands in the English edition (`gpu-platform-control-plane`)
only. The Korean edition receives a translated `docs/09` and this design later;
its annotated-comment convention applies to the Go/Kubernetes control plane, not
to Terraform/HCL and workflow YAML.

## Directory layout

```
infra/aws/
  bootstrap/
    main.tf          # S3 state bucket, DynamoDB lock, OIDC provider, ECR
    variables.tf
    outputs.tf
    versions.tf      # terraform + provider version pins
    README.md        # the local-state -> migrate bootstrapping procedure
  cluster/
    main.tf          # VPC + EKS + managed add-ons + CPU node group
    iam.tf           # node IAM, EKS access entries, CI apply-role entry
    variables.tf
    outputs.tf
    versions.tf
    backend.tf       # S3 backend, pointed at the bootstrap bucket
.github/workflows/
  ci.yml
  infra.yml
```

## Terraform design

### State layout

Three separate states, of which this increment writes two (`bootstrap` and
`cluster`); `argo-bootstrap` is Increment 2. Separate states isolate blast
radius: routine plan/apply cycles touch only `cluster`.

`bootstrap` is the chicken-and-egg root. It begins on local state and runs
`terraform init -migrate-state` once into the S3 bucket it just created. Its
`README.md` documents that one-time procedure. Because nothing is provisioned in
this increment, the migration is documented, not executed.

### Module choices

- VPC and EKS use the community modules `terraform-aws-modules/vpc/aws` and
  `terraform-aws-modules/eks/aws`, version-pinned. They reduce surface area and
  align with the design's pinned managed add-ons (VPC CNI, CoreDNS, kube-proxy).
- `bootstrap` is hand-written: it is small (S3, DynamoDB, one OIDC provider,
  ECR) and using a module would obscure the state controls the design requires.
- IAM roles for the OIDC principals are hand-written for auditability.

Version pins: `terraform >= 1.9`, `hashicorp/aws ~> 5.x`, module versions pinned
to exact releases in `versions.tf`.

### S3 state controls (design of record §1)

Versioning; SSE-KMS with a scoped key policy; public-access block; a bucket
policy denying non-TLS and non-KMS writes; object ownership enforced;
least-privileged state roles. The DynamoDB lock table is encrypted, with
point-in-time recovery and deletion protection.

### GitHub OIDC and roles

Trust policy pins `aud=sts.amazonaws.com` and `sub` to the repo plus
ref/environment. Separate roles and trust for the PR `plan` path and the
protected `apply` path; applies from forked-PR contexts are denied.

Two v3.2 constraints are encoded as comments and variables so they are not lost:

- `job_workflow_ref` is not an AWS IAM trust condition key. Workflow identity, if
  pinned, is encoded through GitHub's customized `sub` template, not an IAM
  condition.
- Immutable owner/repo-ID `sub` claims require opt-in for repositories created
  before 2026-07-15 (this one). A variable toggles whether the immutable-ID
  claim form is used, defaulting off with a comment explaining the opt-in.

### Network (design of record §1, apply-blocker fix)

VPC with public subnets in two AZs, because EKS `CreateCluster` rejects fewer
than two AZs. All node groups pin to one AZ to avoid cross-AZ workload traffic;
the second subnet exists only to satisfy the control-plane requirement.
`map_public_ip_on_launch=true`, an internet gateway route, and a public EKS
endpoint let public-subnet nodes bootstrap without a NAT gateway. No NAT is
created. The threat model is demo-only and the compensating controls (tight
security groups, IMDSv2, no public SSH, minimal node IAM) are the stated price.
Region `us-east-1`.

### Node group and identity

One CPU managed node group pinned to a single AZ. IMDSv2 with
`httpTokens=required` and `httpPutResponseHopLimit=1`. No operator IRSA; the
manager calls only the Kubernetes API. Cluster access via EKS access entries: a
dev admin entry and a CI apply-role entry, the latter required so `destroy.yml`
(Increment 2) can run ordered teardown.

The GPU node group is not written in this increment; a comment in the node-group
file marks where it will attach in M5-b (`desired=0`, taint
`nvidia.com/gpu=present:NoSchedule`, On-Demand `g5.xlarge`).

## CI/CD design

### ci.yml

Triggered on push and PR. Runs `go test` (envtest), then builds the operator
image and pushes it to ECR using the OIDC image-push role. Deployment is by
digest: the design's PR-bump flow (CI opens a PR bumping the image digest in
`config/`) is the eventual delivery path, but the digest-bump automation itself
is Increment 2 (it depends on Argo CD being the deployer). This increment's
`ci.yml` builds and pushes; it does not yet open digest-bump PRs.

Permissions follow the existing workflows' least-privilege style
(`permissions: {}` at top, scoped per job) and pin actions by commit SHA, as
`lint.yml` does. The push-to-ECR job requires `id-token: write` for OIDC.

### infra.yml

Triggered on PR (plan) and merge (apply). On PR it runs `terraform plan` with
the plan role; on merge it runs `terraform apply` behind a manual-approval
GitHub environment with the apply role. Forked-PR applies are denied by
requiring the environment and checking the trigger context.

Because no AWS roles exist until `bootstrap` is applied, these jobs are written
but will not execute successfully until provisioning happens. That is expected
for a code-only increment and is noted in the workflow comments.

## Verification (code-only, offline)

No `terraform plan` or `apply` runs, because both need AWS credentials. The code
is verified structurally and offline:

- Install the terraform binary into `bin/` (a Makefile target, matching how the
  repo already vendors `kustomize`, `controller-gen`, etc. into `bin/`).
- `terraform fmt -check -recursive infra/aws` must be clean.
- In each state directory, `terraform init -backend=false` then `terraform
  validate` must pass. `-backend=false` avoids needing the S3 backend or
  credentials; provider and module downloads need network but not AWS access.
- Install `actionlint` via `go install` and run it over `.github/workflows` to
  validate the new workflow YAML.

A Makefile target (for example `make infra-validate`) wraps these so the
verification is one command and can later be added to a CI check.

## Testing

Infrastructure code has no unit tests in the Go sense. The verification above is
the test: `terraform validate` catches type and reference errors, `fmt` enforces
formatting, and `actionlint` catches workflow errors. The design explicitly does
not run `plan` (no credentials, no provisioning), so misconfigurations that only
surface at plan time are out of reach for this increment and are accepted as the
cost of the code-only boundary.

## Non-goals

- Multi-AZ HA, private subnets/NAT, multi-region. This is an ephemeral demo.
- Production-grade EKS posture beyond the listed compensating controls.
- Any resource that costs money before the user explicitly provisions.
