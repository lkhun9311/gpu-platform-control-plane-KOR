# AWS Infrastructure Architecture (M5-a / M5-b)

> **Status (2026-08-27): `bootstrap` is applied; `cluster` is planned and not applied.** The state bucket, its KMS key, the GitHub OIDC provider, the three CI roles, the ECR repository and the budget alarms exist in the project's AWS account (`ap-northeast-2`). The `cluster` state has been planned against that account — 89 resources — and never applied, so no VPC, EKS cluster, node or NAT gateway exists and nothing is currently billing beyond a few cents of S3 and KMS. `argo-bootstrap` has never been initialised.
>
> Design of record: an internal integration design (v3.2) reconciled from two independent AI reviews that raised 20 findings between them; that working document is not published. The network design in this document supersedes v3.2's public-subnet topology — see *What is deliberately absent*.

The operator is cloud-agnostic; its only AWS dependency is hosting. Scope split, stated precisely: the AWS/GitOps **hosting layer is optional** around the operator, but the **M5-b real-GPU benchmark is required** for M5's definition of done (doc 04) — the hosting exists so that run can happen credibly. Everything is built ephemeral-first: teardown is a control, not an intention.

## One-page view

```
GitHub repo (public — Argo CD reads it anonymously; source of truth)
  ├── ci.yml      ──(OIDC: image-push role)──>  ECR  <──(pull by digest)── EKS nodes
  ├── infra.yml   ──(OIDC: plan role on PR / apply role on merge, manual gate)──> Terraform
  ├── gpu.yml     ──(workflow_dispatch: apply role, gpu_desired=0|1)──> GPU node group switch   [NOT WRITTEN]
  └── destroy.yml ──(nightly cron + workflow_dispatch: apply role)──>
        delete Argo apps -> destroy argo-bootstrap state -> residue check -> destroy cluster state

Terraform (3 separate states; bootstrap starts on LOCAL state, then migrates into its own bucket)
  ├── bootstrap:      S3 state bucket (locks on <key>.tflock) + GitHub OIDC provider + ECR
  ├── cluster:        VPC + EKS (pinned recent version) + managed add-ons + node groups + IAM + access entries
  └── argo-bootstrap: initial Argo CD install only (run once, never on routine applies)

VPC (3 AZ; nodes hold no public address)
  ├── public subnet x3 (ELB-tagged, NO instances):
  │     NAT gateway (ONE, in AZ-a) + internet gateway route
  └── private subnet x3 (no auto public IP, default route -> NAT):
        EKS control-plane ENIs
        CPU node group (pinned to AZ-a, the NAT's zone)
        [session] GPU node groups: desired=0, subnets derived from instance-type offerings,
                  taint nvidia.com/gpu=present:NoSchedule
        S3 gateway endpoint (free) -- ECR layers live in S3, so image pulls bypass the NAT

EKS (private endpoint always; public endpoint only for explicitly named CIDRs -- default none)
  auth = EKS access entries: dev admin + CI apply role
  ├── managed add-ons (Terraform-owned, pinned): VPC CNI, CoreDNS, kube-proxy
  ├── operator + gateway (Kustomize config/, deployed by Argo CD, image by digest)
  ├── Argo CD app-of-apps: crds -> operator/gateway -> [profiles: observability, device-plugin, samples]
  ├── slim Prometheus/Grafana (off until operator custom metrics exist; access = kubectl port-forward only)
  └── GPU simulator DaemonSet (off while the real GPU node group is up)

Guardrails
  ├── TTL/owner tags on everything
  ├── budget alarm $10/day -> escalation; hard review at $30 cumulative
  └── destroy failure = retry once -> residue inventory (EC2/ELB/EBS/ECR/CloudWatch/TF state) -> break-glass manual runbook
```

## Terraform state layout

| State            | Contains                                                    | Why separate                                                                                                                                                                                   |
|------------------|-------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `bootstrap`      | S3 state bucket, GitHub OIDC provider, ECR                  | Must exist before anything else; almost never changes. **Chicken-and-egg is explicit:** it begins on local state and runs `terraform init -migrate-state` once into the bucket it just created |
| `cluster`        | VPC, EKS, managed add-ons, node groups, IAM, access entries | The blast radius of routine work — plan/apply cycles touch only this                                                                                                                           |
| `argo-bootstrap` | Initial Argo CD Helm install only                           | Terraform installs Argo CD once, then Argo CD owns the cluster; keeping it in `cluster` state would make every plan fight Argo CD's drift                                                      |

S3 state controls (v3): versioning, SSE-KMS with a scoped key policy, public-access block, bucket policy denying non-TLS and non-KMS writes, least-privileged state roles. Locking is the backend's own `use_lockfile` (Terraform ≥ 1.10) rather than a DynamoDB table — see the note below.

## Network

| Decision              | Value                                                                                                      | Why                                                                                                                                                                                                              |
|-----------------------|------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Subnets               | **Three AZs, public + private in each**                                                                     | EKS `CreateCluster` rejects fewer than 2 AZs. The third exists because `g5` is offered in `2a/2c/2d` but **not** `2b`, so two zones can leave the A10G groups one home                                             |
| Node reachability     | Private subnets, **no public IP**, default route to NAT; **private EKS endpoint**                            | A node with a routable address makes its security group the only thing between the internet and a kubelet. The private endpoint is what lets addressless workers reach the API without leaving the VPC             |
| API endpoint exposure | `endpoint_private_access=true`; public half derived from `api_public_access_cidrs` (**default empty**)      | The module default is `0.0.0.0/0` and this repo never set the argument. Reaching the endpoint ≠ authenticating to it, but a world-reachable endpoint is one a leaked credential works from anywhere. `0.0.0.0/0` now fails validation |
| Node placement        | CPU group pinned to AZ-a (the NAT's zone); GPU groups derived from instance-type offerings (`placement.tf`) | Keeps CPU egress in-zone; GPU placement must follow capacity, not subnet index                                                                                                                                     |
| NAT                   | **One** gateway, not one per AZ                                                                              | Per-AZ NAT buys AZ-fault isolation, worth less than its hourly rate on a cluster whose failure mode is a re-run. Price: cross-AZ egress from two zones at $0.01/GB each way                                        |
| VPC endpoints         | **S3 gateway only.** No interface endpoints                                                                  | Free, and S3 traffic never leaves the VPC. It does **not** remove the image-pull cost — see the correction below. A fully private cluster needs ~9 interface endpoints, billed per hour per ENI — more than the single NAT they would replace |
| Node shell access     | **SSM Session Manager**, no bastion                                                                          | No inbound port at all; the agent dials out and access is an IAM decision CloudTrail records. A bastion is a permanently billed instance whose job is to hold an open SSH port                                     |
| Region                | `ap-northeast-2`                                                                                            | Where the G/VT quota was granted (52 vCPU, 2026-08-26). State bucket, cluster and registry all live beside it                                                                                                       |

### What is deliberately absent, and why

The 3-tier reference architecture this project's author built as an AWS SA puts an ALB in the public subnet,
WAF and Shield in front of it, CloudFront over an S3 origin, and a bastion in the public subnet. Each of those
is the right answer to a question this workload does not ask.

| Service            | Verdict | Reason                                                                                                                                                                                                                                                                                                              |
|--------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| ALB in front of the **EKS API** | **Cannot** | The API endpoint is AWS-managed and its TLS certificate is issued for `*.eks.amazonaws.com`. An ALB terminates TLS and presents its own, so `kubectl` fails certificate validation unless the kubeconfig is told to skip it. The private endpoint's ENI addresses are also not a documented, stable target set. Passthrough would need an NLB — and WAF does not attach to NLB |
| WAF                | **No**  | WAF inspects L7 HTTP for web-application attacks and attaches to CloudFront/ALB/API Gateway. There is no public HTTP application here. Against `kubectl` traffic its managed rule groups would produce false positives and block nothing real — the API's actual defence is SigV4 + RBAC + the CIDR allow-list |
| ALB in front of the **gateway** | **Correct — as a demo path, never as the measured path** | This *is* the true analogue of the reference architecture's WAS tier, and the public subnets carry `kubernetes.io/role/elb` so one can land. The reason it is not in the benchmark path is **attribution, not latency**: `hack/m5b-gateway-path.md` measured the existing gateway hop at p50 0.3 ms / p99 1.0 ms and argued that a fixed sub-millisecond hop shrinks to noise against a real prefill of hundreds of ms — the same argument would excuse an ALB. What it would not excuse is the ALB's own queueing, retries, idle timeouts and target-group health flapping, which are failure modes the run's record cannot tell apart from engine behaviour. Exposing the gateway behind ALB + ACM + WAF as a **demo** costs little and is worth doing; driving the benchmark through it changes what is being measured |
| CloudFront         | **No**  | A CDN for static origins. The only S3 bucket here holds Terraform state, which must never be publicly readable — fronting it with CloudFront points the wrong way |
| Bastion host       | **No**  | Replaced, not supplemented, by SSM (above) |
| RDS / ElastiCache tier | **N/A** | There is no database. The stateful thing in this system is the KV cache, which is GPU memory on the node and cannot be moved to a data tier |
| Secrets Manager    | **Deferred** | The gateway's tenant API keys are created by a session script as a Kubernetes Secret. External Secrets + Secrets Manager is the correct production shape at ~$0.40/secret/month; deferred because the only secret today is a literal `premium-key` in a cluster that lives for hours, and adding a dependency to the measurement path before the measurement runs is the wrong order |
| KMS                | **Already in use** | Terraform state bucket key, and EKS envelope encryption for Kubernetes Secrets |

### Where this lab and a production cluster diverge

The Reliability pillar is the one this design deliberately fails, and a deliberate failure has to be written
down or it is indistinguishable from an oversight. Each row states what this lab does, what a production
system would do instead, and the trigger that would move this cluster to the right-hand column.

| Concern | This lab | Production | Why the lab is where it is | What would move it |
|---|---|---|---|---|
| NAT | One gateway, one AZ | One per AZ | An AZ outage costs a re-run, not an incident. Second and third gateway = +$0.118/hr for isolation worth less than that here | Any workload whose interruption loses data rather than a measurement |
| Worker capacity | One `t3.large`, `desired=1` | ≥3 nodes across AZs, PodDisruptionBudgets, `topologySpreadConstraints` | Nothing here is serving users; the operator and gateway are the subject of measurement, not a dependency of anyone | A consumer other than the benchmark harness |
| GPU capacity | Spot for the admission arms, On-Demand for the preemption study | On-Demand or Capacity Reservations for anything user-facing | An interrupted measurement is refused; an interrupted inference request is an SLO breach | Serving traffic that is not synthetic |
| Region | Single (`ap-northeast-2`) | Multi-region DR, or single-region with documented RTO/RPO | Everything is reproducible from Git plus Terraform; the only durable artifact is the evidence directory, which lives in Git | Durable state that cannot be rebuilt from code |
| State durability | S3 versioning + KMS | Same, plus cross-region replication and AWS Backup | The Terraform state describes a cluster that is recreated, never repaired | State whose loss cannot be recovered by re-applying |
| Cluster lifecycle | Recreated on a pinned version, never upgraded | In-place control-plane upgrade with blue/green node groups and a drain plan | The cluster's lifetime is shorter than an upgrade cycle | A cluster that outlives a Kubernetes minor release |
| Observability | Prometheus/Grafana during a session, `port-forward` only | Always-on, remote-write to durable storage, alert routing with an on-call rotation | There is nobody to page, and a session is watched by the person who started it | Anyone other than the operator depending on it |
| Secrets | Kubernetes Secret created by a session script, envelope-encrypted by KMS | External Secrets Operator against Secrets Manager, with rotation | The only secret is a literal tenant key in a cluster that lives for hours | Any credential whose disclosure outlives the cluster |
| Teardown | Nightly cron + manual `destroy.yml` | Nothing to tear down; the cluster is permanent | Cost discipline is the point | Permanence |

The honest summary: **this cluster is optimised for the cost of being wrong being a re-run.** Every row above is
a bet on that sentence, and every row names the observation that would void the bet.

### The demo path, which exists as a plan rather than as code

The measurement path and a demonstration path are different products and must not be confused. The benchmark
reaches the gateway by `port-forward` because an ALB in the request path adds queueing, retries, idle timeouts
and target-group health flapping that the run's record cannot tell apart from engine behaviour.

None of that argues against having a demo path, and the reference architecture behind this repository already
demonstrated its pieces. When the M5-b and M5-c measurements are finished, the demo path is:

```
Route 53 (existing domain)
  -> CloudFront            [optional; only if a static evidence page is served alongside]
  -> AWS WAF web ACL       [managed rule groups + the geo-block rule demonstrated in 2023]
  -> ALB (public subnets, ACM certificate)     $0.0225/hr + $0.008/LCU-hr
  -> target group -> gateway Service (private subnets, AWS Load Balancer Controller Ingress)
```

The public subnets already carry `kubernetes.io/role/elb`, so the controller has somewhere to place it. Two
constraints on the day it is built: the benchmark must not be re-run through it, and it must be torn down with
the cluster like everything else.

### Four decisions taken on 2026-08-27, and what would reverse each

Settled against verified market data (AWS Pricing API and `describe-spot-price-history`, `ap-northeast-2`)
and an independent adversarial verdict. Each row names the observation that reverses it.

| # | Decision | Reversed by |
|---|---|---|
| 1 | **Do not request 52 → 96 vCPU of `L-DB2E81BA` yet.** queuelab runs on one `g4dn.12xlarge` | The two-node axis being scheduled for a specific session |
| 2 | **Spot quota requested and granted at 8 vCPU on 2026-08-27.** Every group stays On-Demand until the truncation gate is complete; then Spot for M5-b and M5-c only, never queuelab | M5-b's gate is **closed** (three fixes, injection-tested); **M5-c still has no analyzer**, so `gpu_shared` cannot move until it does |
| 3 | **Stay in `ap-northeast-2`** | A target region with a confirmed G/VT quota, re-bootstrapped state, and a destroy-tested teardown — plus GPU-hours large enough for 23% to exceed that cost |
| 4 | **Keep `g5.xlarge` (A10G) for the single-card groups** | A study not bound to the A10G preregistration; `g6.xlarge` (L4) is then the default |

**On 1.** The preregistration is explicit that a one-node session delivers the arm contrast, the dose axis and
the device evidence, and that `-compare -mode node` refuses rather than inventing an axis it does not have.
The second node doubles queuelab burn from $4.812/hr to $9.624/hr for that axis alone. There is also a reason
to keep the quota low that has nothing to do with the axis: **at 52 vCPU the account itself refuses a second
48-vCPU node at the launch API.** (It does not cap the account at one instance — 48 + 4 fits, so one
`g4dn.12xlarge` and one `g5.xlarge` can run concurrently at $6.05/hr. What it forbids is the second big one.)
A Budgets alarm is detection after spend has accrued; a service quota is prevention at the API. Until the
axis is scheduled, the unraised quota is the cheapest spending control in this account, and raising it
removes that. AWS publishes no approval-time SLA and no rule that prior usage eases an On-Demand G/VT
request — the documented usage-based auto-adjustment applies to **Spot** quotas only — so "ask after a
session and it is easier" was an assumption, not a fact.

**On 2.** A quota request creates no instance and costs nothing, so the lead time is worth removing now. What
must not follow automatically is *use*: a grant is permission, not a methodological waiver.

queuelab stays On-Demand permanently, and the mechanism is informative censoring rather than cost. A Spot
reclamation drains the node, the drain evicts the victim Pod, `AttemptStopped` arrives early, and the
`PodReady → AttemptStopped` window the held time is computed over is **shortened** — a reclaim that reads
better than it was.

For M5-b and M5-c the previous justification — "an interruption ends an arm without biasing it, and an arm
that produced no records is refused" — is **half right, and the half that is wrong is the half that matters**:

- **A missing arm IS refused.** `summ` is a map, a missing key yields the zero `ArmSummary`, and
  `EvaluateChecks` disqualifies on `TailSampleSize == 0` before any check is read. Two independent reviewers
  asserted the opposite; reading `internal/bench/report.go:319-335` settles it. The harness now also names
  which arm is absent, because the refusal message interpolates an empty `s.Arm`.
- **A TRUNCATED arm is not.** An arm interrupted after 120 premium completions clears
  `MinTailSamples = 100` and is compared against arms with four times the samples. Unequal repetition counts
  print a warning to stderr and refuse nothing. Arms run in a fixed order, so interruption lands
  preferentially on later ones — informative censoring, not a smaller sample.
- **M5-c has no analyzer at all**, so a matrix missing a cell has nothing to refuse it.

**Status of the gate.** Three of the four ways a degraded run could certify itself are now closed, each
injection-tested:

| Hole | Mechanism | Closed by |
|---|---|---|
| Missing arm | — | Already refused; `TailSampleSize == 0` disqualifies before any check. The harness now also names which arm is absent |
| Tail lost to transport errors | Censoring counted `TimedOut` only, and a vanishing node produces connection resets, not timeouts | Censoring counts every premium non-completion that is not a 429 |
| Absent confidence interval | Unequal repetitions skip the bootstrap; the zero `CI`'s `Hi = 0.0` satisfied `Hi < 1.0`, so the **absence** of an interval passed the strictest gate | `CI.Valid`, false by construction; a degenerate single-repetition interval is invalid too |
| Truncated repetition inside a healthy arm | Pooled `TailSampleSize` hides a repetition of 30 among three of 500, and the bootstrap resamples its maximum-as-p99 with equal weight | Per-repetition floor at `MinTailSamples`, wired through `report` and covered by a command-level test as well as unit specs |

**What remains before `gpu_shared` may use Spot: M5-c has no analyzer at all.** A matrix missing a cell has
nothing to refuse it, so the sharing group stays On-Demand even after the Spot quota is granted. `gpu_single`
(M5-b) is gated only on the quota now.

None of this was a Spot problem. An evicted engine Pod, an OOM kill or a network blip produce the same rows,
so until these landed **any** degraded run — On-Demand included — could print `all checks passed`.

**On 3.** The verdict stands and **its original main argument does not.** "Moving puts the first apply behind
another support case" assumed a quota wait is expensive; in practice it is at most about three days, which is
cheap. Priced at zero, that argument dies.

What carries it instead is the size of the prize. Against the preregistered session shape — queuelab is eight
~3-minute runs (0.4 GPU-hours) and M5-b/M5-c are `REPS=4` blocks — moving to `us-west-2` saves $0.900/hr on
queuelab and $0.231/hr on the single-card work: **under $3 for a whole session**, and under $15 across every
session plausibly planned. Against that sits an unexercised regional path — a new state bucket, KMS key and
registry, images re-pushed, and the teardown drill re-run somewhere it has never been run — introduced
immediately before the first paid apply.

And the sign flips if Spot is ever enabled: Seoul's single-card Spot is the cheapest of the three ($0.3522/hr
for `g5.xlarge` against $0.5086 in `us-west-2` and $0.5583 in `us-east-1`) and carries the best Spot Placement
Score (3,3,3 against 1–2 elsewhere). Moving would then **cost** money on the only instances that would ever
be Spot.

The residual risk is On-Demand `g4dn.12xlarge` capacity in Seoul, which no API measures — Spot Placement Score
covers Spot only, and no public AWS source establishes a chronic G-family On-Demand problem there. The
containment is not a region change but an **On-Demand Capacity Reservation** taken minutes before the scale-up,
which costs the same hourly rate as running the instance and converts a capacity risk into a booking.

**On 4.** `g6.xlarge` (L4) carries the identical 22,888 MiB and 4 vCPU, is offered in the same zones, and is
20% cheaper On-Demand and 41% cheaper on Spot. The M5-c sizing that rejected the T4 is pure memory arithmetic
and transfers unchanged — so the card is *feasible*. It is not *equivalent*: device-memory bandwidth is
roughly halved (A10G ≈ 600 GB/s, L4 ≈ 300 GB/s, vendor figures not independently verified here), and LLM
decode is bandwidth-bound. That sits in the causal path of token service time, queue residence and KV-pressure
duration — the quantities M5-b and M5-c measure.

**That mechanism is withdrawn as a reason.** Vendor bandwidth is a hardware fact, not a prediction of this
stack's end-to-end throughput: AWS advertises G6/L4 at up to 2x G4dn inference performance, and vLLM's own
benchmark material warns that results are highly parameter-sensitive. No controlled A10G-versus-L4 result
exists for this model, precision, prompt mix, concurrency and vLLM version, so "L4 must be slower here" was
an inference presented as a finding.

What survives is narrower and sufficient: **the session is preregistered and calibrated for A10G.** The
sizing page's predicted KV tokens, the measured-rate derivation and the warmup are all frozen against that
card, and the repository deliberately put M5-b's exclusive arm and M5-c on one card class. Substituting the
device yields an L4-specific session rather than the preregistered A10G evidence. The saving is $0.247/hr
On-Demand — roughly $1–2 across the whole campaign. `g6.xlarge` becomes the default for the next study that
is not bound to this preregistration; the scripts would not mechanically catch a substitution, so it must
stay a declared choice.

### Evidence provenance

A paid run's numbers belong to a build, and once the cluster is gone the record is the only place that
association can live. Two things had to change for that to be true rather than intended.

**CI publishes what GitOps deploys.** `config/gateway/deployment.yaml` carried `image: gateway:latest`, a name
nothing published, while each paid session script built its own copy under a local tag. `ci.yml` now builds
`Dockerfile.gateway`, pushes it to `gpu-platform-gateway`, and pins both kustomizations in **one** PR — two
would let the cluster run a gateway from one commit against an operator from another with nothing recording
which pair was live.

**The manifest carries the build, and refuses a tag.** `RunManifest.GatewaySHA` and `ImageDigests` were
declared for provenance and never set by anything. `gen-trace` accepts `--gateway-sha`, `--gateway-image` and
`--engine-image`; `replay --require-provenance` refuses a manifest that omits them, and refuses a reference
that is a tag rather than a digest — `gateway:m5b` names whatever was pushed under it most recently and
identifies nothing after the next build. The paid script resolves its own pushed image through `RepoDigests`,
reads the engine's image off the running Deployment, and stamps the working tree's commit with `-dirty` when
it is not clean: a paid run built from uncommitted code is a legitimate thing to do and an illegitimate thing
to hide.

`--require-provenance` is opt-in for the same reason `-require-device` is: a kind run against a stub has no
build worth pinning, and demanding one from every free run would push an operator toward inventing a value.

### What this document does not say

This repository is public, and the design is the point of it being so. Account-specific coordinates are not.

Account ID, state bucket name, KMS key ARN, IAM role ARNs and support case numbers stay out of the published
tree and live in the private engineering notes instead.

**None of them is a credential**, and AWS classifies an account ID as not secret, sensitive or confidential
([AWS account identifiers](https://docs.aws.amazon.com/accounts/latest/reference/manage-acct-identifiers.html)).
The security boundary is the OIDC trust policy's `sub` condition, the bucket policy and the KMS key policy —
not the secrecy of a name. What publishing the account ID buys an attacker is **targeting**: with the role
names already visible in this repository's Terraform, it makes the exact role ARNs constructible, so a
hypothetical trust-policy defect becomes immediately addressable rather than merely present.

That is a reason to leave the coordinates out. It is not a reason to treat their past publication as an
incident: an adversarial review of this exact question ranked the account ID **fifth of six** exposures in
this repository, below the CI trust boundaries it sits beside.

The rule, stated so it survives the next edit: **the reasoning is public, the coordinates are not.**

## Identity and access


| Principal            | Mechanism                                                      | Constraints                                                                                                                                                                                                                                                                                                                    |
|----------------------|----------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| CI (image push)      | GitHub OIDC → image-push role                                  | Trust pins `aud=sts.amazonaws.com` + `sub` (repo/ref/environment)                                                                                                                                                                                                                                                              |
| CI (terraform plan)  | GitHub OIDC → plan role (PR context)                           | **`terraform plan` is not read-only on untrusted input:** `init` fetches and executes providers and modules named by the pull request's own configuration, inside a job holding account-wide `ReadOnlyAccess` and state-key decrypt. The workflow therefore refuses to hand credentials to a pull request whose head is not this repository (`head.repo.full_name == github.repository`), rather than relying on whether GitHub issues `id-token: write` to fork workflows |
| CI (terraform apply) | GitHub OIDC → apply role, gated on the `infra-apply` environment | Separate role and trust from plan. The environment now exists with a deployment branch policy of `main` only, which is what makes the `sub` condition mean something. It did not until 2026-08-28: **GitHub creates a referenced-but-missing environment *without* protection rules**, so until then the condition proved only that a job had named `infra-apply`. **No required reviewer** — see below. All three trusts additionally pin `repository_id` and `repository_owner_id`, which are immutable — an earlier note here claimed AWS could match only `aud`/`sub` and that `job_workflow_ref` was unusable; AWS's current documentation lists all of these as GitHub OIDC context keys, so that note was stale |
| Operator             | **No IRSA**                                                    | The manager calls only the Kubernetes API — an AWS role would be attack surface with no function                                                                                                                                                                                                                               |
| Node instances       | IMDSv2: `httpTokens=required` + `httpPutResponseHopLimit=1`    | An **instance** option, not a pod control: it blocks IMDS from pod-network containers (extra hop), but hostNetwork pods still reach it — that exception is documented, not hidden. Node bootstrap/ECR pull must be tested at hop-limit 1                                                                                       |
| Cluster access       | **EKS access entries**: dev admin entry + CI apply-role entry  | The apply role needs kubectl to run pre-destroy (delete Argo apps) — without its access entry, `destroy.yml` cannot do ordered teardown                                                                                                                                                                                        |

### The one long-lived credential, and how it goes away

CI holds no long-lived key: GitHub OIDC exchanges a workflow identity for a short-lived role session, and the
plan and apply roles have separate trusts. Local work does not match that standard — it uses an access key
belonging to the IAM user `user-1`. That key is the only credential here that does not expire on its own, and
it is the last real gap in the Security pillar.

| Option | Verdict | Reasoning |
|---|---|---|
| **IAM Identity Center** + `aws sso login` | **The answer** | Free. Requires an AWS Organization, which for a single account costs nothing to create. Local credentials become a session with a bounded lifetime, refreshed by a browser login; there is no static secret on the laptop to leak, and every assumption is a CloudTrail event with a human identity attached. This is what a company would do. |
| MFA + `sts:AssumeRole` with a session policy | Interim | Keeps the access key but reduces it to a key that can do nothing except assume a role after an MFA prompt, with a session TTL. Strictly better than today and achievable in minutes; strictly worse than Identity Center because a static secret still exists. |
| **HashiCorp Vault** AWS secrets engine | **Over-engineering here, and it is worth knowing why** | Vault's AWS secrets engine issues dynamic IAM credentials with a TTL and revokes them on expiry — genuinely the strongest version of this control. The cost is that Vault itself becomes infrastructure: HA, unseal, storage, upgrades, audit, and a new component whose compromise is worse than the credential it protects. It earns that cost when secrets span **multiple clouds**, when they are **not IAM** (database passwords, PKI, SSH CA), or when the consumers are **outside the AWS account**. This project is one cloud, one account, one human, and one credential type. Identity Center gives the same expiry-by-default property for free. |
| IAM Roles Anywhere | Not applicable | Solves X.509-identified workloads outside AWS — an on-premises server, not a laptop with a browser. |

Sequence: disable the access key at the end of the current session, create the Organization and enable
Identity Center, and move local work to `aws sso login`. Nothing in `infra/aws/` changes — the roles and trust
policies are unaffected, because the credential is how a human reaches AWS, not what Terraform describes.

### Why the apply environment has no required reviewer

A reviewer gate was configured and then removed the same day, and the reason is worth keeping.

`destroy.yml` runs on a nightly cron and deploys to the same `infra-apply` environment, because its OIDC token
has to satisfy the apply role's `sub` condition. A required reviewer therefore does not gate only the applies
— it gates the **unattended teardown**, which would sit waiting for approval while a GPU node bills by the
hour. The control that exists to stop an overnight bill would have been disarmed by the control meant to stop
an unreviewed apply.

With one contributor the reviewer half was buying little anyway: GitHub permits self-review by default, so it
is a speed bump rather than separation of duties. The branch policy is the half that actually constrains —
only a workflow running on `main` can deploy to this environment, and that is what the apply role's `sub`
condition rests on.

If a second contributor ever exists, the right shape is the one this repository's bootstrap README already
describes: a separate `infra-destroy` environment with its own role, pinned to that environment and
reviewer-free, so the reviewer gate can go back on `infra-apply` without costing the teardown.

## Cluster add-ons (ownership explicit, same rule as CRDs)


| Add-on                           | Owner                                                | Note                                                                                                                                                    |
|----------------------------------|------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| VPC CNI, CoreDNS, kube-proxy     | Terraform (**EKS managed add-ons**, versions pinned) | Cluster-lifecycle infrastructure — never Argo's                                                                                                         |
| EBS CSI                          | Not installed                                        | No PVCs by default (slim observability runs without persistence); installing it would create the exact teardown residue the guardrails exist to prevent |
| NVIDIA device plugin / simulator | Argo CD `device-plugin` profile                      | Workload-facing, mutually exclusive modes — see GPU section                                                                                             |

## CI/CD pipelines

| Workflow      | Trigger                            | Steps                                                                                                                                                                                   |                                                                                                                           |
|---------------|------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------|
| `ci.yml`      | push / PR                          | `go test` (envtest) → build **operator** image → push to ECR. The gateway now has a Dockerfile (`Dockerfile.gateway`) and manifests (`config/gateway/`), but `ci.yml` does not yet build or push a gateway image — that CI wiring has not been added.                                                      |                                                                                                                           |
| `infra.yml`   | PR (plan) / merge (apply)          | `terraform plan` with plan role on PR; `apply` behind a manual-approval environment with apply role                                                                                     |                                                                                                                           |
| `gpu.yml`     | `workflow_dispatch` only           | Gated apply of `gpu_desired=0\                                                                                                                                                          | 1` — the **only** actor that scales the GPU node group (no autoscaler by design: one auditable switch, ephemeral windows) |
| `destroy.yml` | nightly cron + `workflow_dispatch` | Delete Argo apps → destroy `argo-bootstrap` state → residue check → destroy `cluster` state; on failure: retry once → residue inventory → break-glass runbook + budget-alarm escalation |                                                                                                                           |

Deployment is **by digest**: CI pushes the image, then opens a PR bumping the digest in `config/`; Argo CD deploys what Git says. The ECR lifecycle policy expires only untagged/PR images — never a digest referenced from the default branch (lifecycle vs Git-pin conflict resolved by construction).

## GitOps topology (Argo CD)

| Application     | Source                        | Ownership rules                                                                                                                                                                             |
|-----------------|-------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `crds`          | `config/crd` (Kustomize)      | `ServerSideApply=true`; operator CRDs have exactly one owner                                                                                                                                |
| `operator`      | `config/` (Kustomize)         | Manager + RBAC; image digest from Git                                                                                                                                                       |
| `gateway`       | `config/gateway/` (Kustomize) | M4-b deliverable; same digest-bump flow                                                                                                                                                     |
| `observability` | slim Prometheus/Grafana chart | **Disabled until operator custom metrics exist**; chart CRDs `Prune=false` + `Replace=false`, Helm `skipCrds` pinned; access = `kubectl port-forward` only (public LoadBalancer prohibited) |
| `device-plugin` | NVIDIA plugin or simulator    | Separate profile; simulator and real plugin are mutually exclusive (manual profile toggle, flipped in the same PR as `gpu.yml` scale-up)                                                    |
| `samples`       | `config/samples`              | Manual sync only, never auto — samples are demo actions, not desired state                                                                                                                  |

The repo is public, so Argo CD reads it anonymously — no deploy credential to create, rotate, or leak. Argo CD does not manage its own chart (installed by `argo-bootstrap`, then left alone). Root app-of-apps orders: crds → operator/gateway → profiles.

## GPU capacity: simulated vs real

| Mode             | Mechanism                                                               | Constraints                                                                                                                                                                                                                                                          |
|------------------|-------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Simulated (kind) | `kubectl patch node .../status/capacity`                                | Local only — holds because no device plugin reconciles capacity there                                                                                                                                                                                                |
| Simulated (EKS)  | Device-plugin-style simulator DaemonSet                                 | Registers on the kubelet socket (hostPath `/var/lib/kubelet/device-plugins`), fake `Allocate` — it makes `nvidia.com/gpu` **schedulable**, nothing more: CPU-only test pods; any CUDA/vLLM pod fails at runtime, and quota/scheduling evidence is scoped accordingly |
| Real (EKS, M5-b) | GPU node group scaled 0→1 by `hack/m5b-gpu-session.sh`, **Spot** `g5.xlarge`, tainted | Exists only during benchmark windows; simulator profile disabled while up. Spot became authoritative for M5-b on 2026-08-27, once an interrupted arm could no longer be reported as a result — see the capacity table below and the five closures recorded at `infra/aws/cluster/vpc.tf:42`. This row previously said On-Demand and called Spot non-authoritative, which contradicted that decision from the day it was made                                                                                                                                                  |

## Cost model and teardown enforcement

| Control           | Value                                                                                                                                                                                                            |
|-------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| EKS version       | Pinned recent at provisioning. Ephemeral clusters are **recreated, not upgraded** — which is also how the $0.10/hr standard-support rate stays true (extended support is $0.60/hr; this design never reaches it) |
| Idle cost         | $0.263/hr — EKS control plane $0.100 + `t3.large` $0.104 + NAT gateway $0.059. All figures `ap-northeast-2`, AWS Pricing API, 2026-08-27                                                                          |
| GPU cost          | `g5.xlarge` **Spot $0.357–0.424** (On-Demand $1.237) for M5-b/M5-c; `g4dn.12xlarge` **On-Demand $4.812** for queuelab. Existing only during a session — see the Spot split below                                  |
| Budget alarm      | $10/day with escalation; hard review at $30 cumulative                                                                                                                                                           |
| Scheduled destroy | `destroy.yml` nightly cron; failure path is operational, not just an alert: retry once → residue inventory (EC2, ELB, EBS, ECR, CloudWatch, orphaned TF state) → break-glass manual runbook                      |
| Residue controls  | TTL/owner tags, PVC/PV + LoadBalancer audit, ECR lifecycle (untagged/PR images only), CloudWatch log retention, snapshot cleanup                                                                                 |

### Why Spot for two GPU groups and not the third

Spot was excluded on the argument that a reclaimed node "does not fail the experiment cleanly". That argument
does not survive contact with this repository — telling a vanished worker apart from a real result is exactly
what the validity gates exist for, and it is one of the easier cases.

The argument that does survive is per-experiment:

| Group | Capacity | Because |
|---|---|---|
| `gpu` (queuelab, `g4dn.12xlarge`) | **On-Demand** | queuelab measures **preemption**. A Spot reclamation is a two-minute warning followed by a cordon and drain, and the drain evicts the victim Pod — truncating the `PodReady → AttemptStopped` window the held time is computed over. A truncated window reports a **shorter** hold, which flatters the headline. The failure mode is not a broken run; it is a run that quietly reads better |
| `gpu_single` (M5-b, `g5.xlarge`) | **Spot** | Measures admission under KV pressure. An interruption ends the arm without biasing it, and an arm that produced no records is refused rather than reported |
| `gpu_shared` (M5-c, `g5.xlarge`) | **Spot** | Measures what two engines do to each other on one card. An interruption ends a cell; a matrix missing a cell is refused at comparison rather than averaged over |

What Spot buys is not primarily a smaller bill. The study's weakest axis is repetition count — two reviews
scored its statistical power at 30% and said the run sequence itself encoded the limit. At 66–71% off, the
same budget buys roughly three times the repetitions.

### When an interface VPC endpoint is cheaper than a NAT gateway, and when it is not

Both charge per hour and per GB, and which wins is entirely a question of how many GB flow through that one
service. The rates (`ap-northeast-2`, Pricing API, 2026-08-27):

| | Hourly | Per GB |
|---|---|---|
| NAT gateway | $0.059 | $0.059 |
| Interface endpoint (per service, per AZ) | $0.013 | $0.010 (first 1 PB/mo) |
| **S3/DynamoDB gateway endpoint** | **$0** | **$0** |

An interface endpoint saves **$0.049/GB** and costs **$0.312/day per ENI**, and an endpoint gets one ENI per
AZ it is placed in. So the break-even depends on how many AZs it spans:

```
marginal endpoint, NAT already present:
    break-even  =  ($0.013 x 24 x ENIs) / ($0.059 - $0.010)

    1 AZ:   6.4 GB/day        2 AZ:  12.7 GB/day        3 AZ:  19.1 GB/day
```

Two cautions on using that number, both of which the first version of this section got wrong:

- The hourly cost scales with **ENI count**, not with services. Applying the 1-AZ figure to a three-AZ
  deployment understates the fixed cost by 3x.
- It is the formula for **adding** an endpoint while the NAT stays. Deciding whether the NAT can go away is a
  different comparison: the avoided `$0.059/hr` is a **single** saving shared across the whole endpoint set,
  not one that can be credited to each service in turn.

**Where it clearly wins.** A cluster shipping 500 GB/day of container logs to CloudWatch:

```
through NAT       500 GB x $0.059              = $29.50/day
through endpoint  24h x $0.013 + 500 x $0.010  =  $5.31/day     -> saves $24.19/day
```

**A correction that matters more than the arithmetic.** The S3 gateway endpoint was justified here on the
grounds that ECR stores image layers in S3, so the multi-GB engine image would bypass the NAT. **That is
false for this cluster's images.** `vllm/vllm-openai` comes from Docker Hub and both NVIDIA components from
`nvcr.io`; ECR holds only the operator and gateway images, which are Go binaries in the tens of megabytes. The
bytes that dominate a fresh node's pull cross the NAT at $0.059/GB regardless. The endpoint stays because it
is free and because S3 is genuinely used — for Terraform state and for the ECR-hosted images — not because it
saves the image pull. Mirroring the three public images into ECR is the change that would make the original
claim true, and it has not been done.

**Where it clearly loses — this cluster.** A six-hour session pulling ~12 GB of images and ~1 GB of everything
else. Note that under the correction above nearly all of that 12 GB crosses the NAT in **both** options, so
it cancels out of the comparison rather than favouring option A:

```
Option A (what this repo does): NAT + S3 gateway endpoint
  NAT hours      6 x $0.059                 = $0.354
  ECR layers     12 GB via S3 gateway       = $0.000     <- layers live in S3
  other egress   1 GB x $0.059              = $0.059
                                              -------
                                              $0.413

Option B: no NAT, nine interface endpoints in one AZ
  endpoint hours 9 x 6 x $0.013             = $0.702
  data           ~1 GB x $0.010             = $0.010
                                              -------
                                              $0.712     <- 1.7x Option A
```

And Option B does not merely cost more — **it does not work with this repository's artifact sources.** There
is no VPC endpoint that reaches public `github.com`, and Argo CD pulls a public GitHub repository as its
source of truth. The same applies to Helm chart repositories, model downloads, and any `curl` in a bootstrap
script.

That is a statement about *these sources*, not about no-NAT EKS in general, and the distinction matters
because the general claim would be false: a cluster that mirrors its Git, charts and model weights into
ECR/S3 and adds the matching AWS endpoints runs with no internet route at all. That mirroring is real work
with real ongoing cost, and it is not work this lab needs — but "we did not mirror our artifacts" is the
honest reason, not "it cannot be done".

The `$0.712` figure above is also specific to its assumptions: one AZ of endpoints, a count asserted as
"roughly nine" rather than derived from the AWS calls the pods actually make, and a six-hour window.
Endpoints bill for every cluster hour, so a longer-lived cluster shifts the ratio.

**Option C, the mixed case, is the one the reference architecture actually meant.** Keep the NAT, and add an
interface endpoint only for a service whose traffic exceeds the 6.4 GB/day line. In this cluster the largest
flow is image layers, and those already cost $0 through the free S3 gateway endpoint — so nothing else comes
close to the line. The nine-endpoint configuration is what you build to remove the NAT, and that is a
*security* posture (no internet route at all), not a cost optimisation. This lab does not need it, because
the security it buys is already bought by the private subnets.

## Execution boundary (what exists today)

| Layer                         | Status                                                                                             |
|-------------------------------|----------------------------------------------------------------------------------------------------|
| Terraform code (`infra/aws/`) | **Written** (`bootstrap`, `cluster`, `argo-bootstrap` states) and offline-validated — never `terraform apply`'d |
| GitHub workflows              | **Written** (`ci.yml`, `infra.yml`, `destroy.yml`, `lint.yml`, `test.yml`, `test-e2e.yml`) — never run against real AWS credentials or infrastructure. `gpu.yml`, the GPU node-group switch this document describes, is **designed only and does not exist** |
| Gateway image                 | **Built and pushed by `ci.yml`** to its own ECR repository, with the digest pinned into `config/gateway/kustomization.yaml` in the same PR as the operator's. Never yet run against a real cluster |
| Operator custom metrics       | **Implemented** (`internal/controller/metrics.go` — taints, degraded transitions, quota drift)     |
| Everything in this doc        | Code written per this design (v3.2) and offline-validated. **`bootstrap` is applied**; `cluster` is planned at 96 resources and never applied. The status line at the top of this document is the authority — this row contradicted it until 2026-08-29 |

## Build order (M5-a → M5-b)

```
M5-a  0. bootstrap state (local -> migrate): S3 + GitHub OIDC + ECR            <- start here
      1. cluster state: VPC (3 AZ, public + private) + NAT + EKS + add-ons + CPU node group + access entries
      2. ci.yml builds + pushes the operator image to ECR (gateway joins after M4-b)
      3. operator via Kustomize (plain apply first, then Argo CD)
      4. argo-bootstrap state + app-of-apps (crds/operator; profiles off)
      5. operator custom metrics (CODE work) -> slim observability profile
M5-b  6. [if GPU scheduling demo] simulator DaemonSet profile
      7. gpu.yml scale-up -> real GPU node group -> flagship runs               <- always last
```

Rule: observability and the NVIDIA plugin must not precede a working operator path; GPU cost must not start before everything else is proven.

## Non-goals

- Multi-AZ **high availability** (one NAT, one CPU node), multi-region, and per-AZ redundancy — this cluster is ephemeral and its failure mode is a re-run.
- Private subnets and NAT were on this list until 2026-08-27, on the argument that they were hardening theater for a four-hour cluster. Pricing the NAT ended the argument: $0.059/hour against a session that already burns G-family instances by the hour. The list keeps only the items whose cost still exceeds what they buy here.
- Cluster upgrades (recreate instead) and production-grade EKS security posture beyond the listed compensating controls.
- Autoscaling the GPU node group (Karpenter/Cluster Autoscaler) — one auditable `workflow_dispatch` switch is the point.
- Spot instances for authoritative benchmark runs (interruption invalidates latency data).
- Terraform-managed in-cluster resources beyond the initial Argo CD install (ownership boundary).
