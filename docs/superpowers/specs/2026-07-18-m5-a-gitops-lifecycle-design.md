# M5-a Increment 2 — GitOps and teardown lifecycle (design spec, v1)

> Status: designed, code-only. No `terraform apply`, no cluster, no Argo CD installed.
> Design of record it implements: `docs/09_AWS_INFRA_ARCHITECTURE.md` and
> an internal integration design (v3.2), kept as a working document and not published.
> Builds on Increment 1 (`docs/superpowers/specs/2026-07-18-m5-a-hosting-foundation-design.md`).

## Purpose

Add the GitOps delivery layer and the enforced-teardown lifecycle for the M5-a
hosting layer, as reviewable code with nothing provisioned. Increment 1 built the
state backend, the cluster, and the image/plan/apply CI. This increment adds:
the one-time Argo CD install (its own Terraform state), the Argo CD app-of-apps
that deploys the operator and gateway from Git, and the scheduled destroy
workflow that makes "ephemeral" a control rather than an intention.

Nothing is provisioned. The deliverable validates offline and is ready to apply
later, on demand.

## Scope

In scope (Increment 2):

- Terraform `infra/aws/argo-bootstrap/`: a separate state that installs the Argo
  CD Helm chart once, and nothing else. Separate state so routine cluster
  plan/apply never fights Argo CD's in-cluster drift.
- Argo CD app-of-apps manifests under `config/argocd/`: a root Application that
  orders `crds` then `operator`/`gateway`, plus the profile Applications
  (`observability`, `device-plugin`, `samples`) wired with the ownership rules
  from the design (ServerSideApply on CRDs; chart CRDs Prune=false/Replace=false;
  samples manual-sync only; profiles disabled until the operator path and metrics
  exist).
- `.github/workflows/destroy.yml`: nightly cron plus `workflow_dispatch` teardown
  that deletes the Argo apps, destroys the `argo-bootstrap` state before the
  `cluster` state, runs a residue check, and on failure retries once then falls
  back to a documented break-glass path.
- Verification: extend `make infra-validate` to cover `argo-bootstrap`, add a
  `kustomize build` check for the Argo manifests, and `actionlint` for the new
  workflow.

Out of scope (deferred, stated so the boundary is explicit):

- Any real provisioning. Code-only by decision, carried over from Increment 1.
- `gpu.yml` and the GPU node group. `gpu.yml` is the switch that scales the GPU
  node group 0 to 1, and that node group is M5-b (Increment 1 left only a marker
  comment for it). Writing `gpu.yml` now would reference a `gpu_desired` variable
  and a node group that do not exist, so both move to M5-b together. The design's
  build order also places GPU last (step 7).
- The slim Prometheus/Grafana chart contents and the operator custom metrics that
  gate the observability profile. The `observability` Application is written but
  points at a chart/values path and stays disabled; implementing the chart and
  the metrics is separate code-backlog work (design build-order step 5).
- The NVIDIA device plugin / simulator manifests themselves. The `device-plugin`
  Application is written as a profile shell; the simulator DaemonSet is M5-b.

## Repository scope

English edition only, same as Increment 1. The Korean edition receives the
translated design doc separately.

## Directory layout

```
infra/aws/argo-bootstrap/
  versions.tf      # terraform + aws/helm/kubernetes provider pins
  variables.tf
  backend.tf       # S3 backend (partial, its own state key)
  main.tf          # helm_release for argo-cd, pinned chart version
  outputs.tf
  README.md        # run-once install/teardown ownership note
config/argocd/
  root.yaml            # the app-of-apps root Application
  crds.yaml            # Application: config/crd, ServerSideApply
  operator.yaml        # Application: config/ (manager + RBAC)
  gateway.yaml         # Application: config/gateway
  observability.yaml   # Application: slim prom/grafana, disabled
  device-plugin.yaml   # Application: device-plugin profile, disabled
  samples.yaml         # Application: config/samples, manual sync only
  kustomization.yaml   # lists the Application manifests for kustomize build
.github/workflows/
  destroy.yml
```

## Terraform design (argo-bootstrap)

A separate root module and state. It declares the Helm and Kubernetes providers
(configured from the cluster's EKS endpoint and auth, read via data sources or
passed as variables) and one `helm_release` for the upstream `argo-cd` chart,
pinned to an exact chart version, with `skip_crds` handling consistent with the
design (chart CRDs are not owned by routine applies).

Ownership boundary, enforced by construction: this state installs Argo CD once.
It is never run on routine cluster plan/apply. Its README documents that the
release lives here so Terraform does not fight Argo CD after handoff. The Helm
and Kubernetes provider blocks validate offline; `terraform validate
-backend=false` does not connect to a cluster.

Provider pins: `terraform >= 1.9`, `hashicorp/aws ~> 5.60`, `hashicorp/helm
~> 2.x`, `hashicorp/kubernetes ~> 2.x`, chart version pinned in `variables.tf`.

## Argo CD app-of-apps design

A root Application (app-of-apps) points at `config/argocd` and creates the child
Applications. Ordering and ownership follow the design of record:

- `crds`: source `config/crd` (Kustomize), syncOptions `ServerSideApply=true`, so
  the operator CRDs have exactly one owner. Sync wave orders it before workloads.
- `operator`: source `config/` (Kustomize), the manager and RBAC. Image digest
  comes from Git (the PR-bump flow). This increment does not build the digest-bump
  automation; it consumes whatever digest `config/` pins.
- `gateway`: source `config/gateway` (Kustomize), the M4-b deliverable.
- `observability`: slim Prometheus/Grafana, `Prune=false` + `Replace=false` on
  chart CRDs, `skipCrds` pinned, access by port-forward only (no public
  LoadBalancer). Disabled (a manual profile toggle) until the operator exposes
  custom metrics, so dashboards are not empty.
- `device-plugin`: the NVIDIA plugin or simulator, its own profile, disabled and
  mutually exclusive with the real GPU node group (flipped in the same PR as the
  M5-b `gpu.yml` scale-up).
- `samples`: source `config/samples`, `syncPolicy` manual only, never
  auto-synced, since samples are demo actions not desired state.

The repo is public, so Argo CD reads it anonymously; there is no deploy
credential to create or rotate. Argo CD does not manage its own chart (installed
by `argo-bootstrap`, then left alone).

"Disabled" is expressed as an Application with automated sync turned off (manual
sync policy) and a comment, not by omitting the Application, so the profile is
visible and one toggle away.

## destroy.yml design

Triggered by a nightly cron and `workflow_dispatch`. Uses the OIDC apply role.
Steps in order, matching the design's teardown ordering:

1. Delete the Argo CD Applications (so Argo stops reconciling as resources go
   away), waiting for LoadBalancers and PVCs to release.
2. Destroy the `argo-bootstrap` state before the `cluster` state. Destroying only
   `cluster` would orphan the Argo Helm release state.
3. Residue check: inventory EC2, ELB, EBS, ECR, CloudWatch, and orphaned TF
   state.
4. Destroy the `cluster` state.

On failure: retry once, then produce a residue inventory and stop at a documented
break-glass manual runbook step, with the budget-alarm escalation noted. The
workflow encodes the ordering and the retry-once-then-inventory control; the
break-glass runbook itself is a documented manual step, not automation.

Because no AWS roles or cluster exist in this increment, the workflow is written
but does not execute successfully until provisioning happens. That is expected
for code-only and is noted in the workflow comments. Backend config for the
terraform destroy steps comes from the same repo variables Increment 1 wired
(`TF_STATE_BUCKET`, `TF_LOCK_TABLE`, `TF_STATE_KMS_KEY`).

## Verification (code-only, offline)

No `terraform plan`/`apply`, no `kubectl`, no cluster. Structural and offline:

- `make infra-validate` extended to also run `terraform init -backend=false` and
  `terraform validate` in `infra/aws/argo-bootstrap`.
- A `kustomize build config/argocd` check (bin/kustomize already vendored) that
  the Argo manifests assemble. Add it to `make infra-validate` or a sibling
  target. This validates YAML structure and kustomize wiring, not Argo CD CRD
  schemas (no cluster to validate against).
- `actionlint` over `.github/workflows` (already in `infra-validate`) covers
  `destroy.yml`.

The Argo CD Application manifests reference `argoproj.io/v1alpha1`; kustomize does
not schema-validate custom resources, so the check confirms assembly and field
shape, not admission. This limit is the same class as `terraform validate` not
reaching AWS, and is accepted for the code-only boundary.

## Testing

Infrastructure and manifest code has no Go-style unit tests. The verification
above is the test: `terraform validate` for the Helm install, `kustomize build`
for the Argo manifests, `actionlint` for the workflow. Runtime behaviors that
only appear against a real cluster or AWS account (Argo sync ordering, Helm
release health, destroy ordering) are out of reach for this increment and are
accepted as the cost of the code-only boundary, with the destroy ordering encoded
declaratively so it is reviewable.

## Non-goals

- The GPU node group, `gpu.yml`, the simulator DaemonSet, and the real
  observability chart/metrics (M5-b and code-backlog).
- The digest-bump PR automation (a later CI enhancement).
- Any resource that costs money before the user explicitly provisions.
