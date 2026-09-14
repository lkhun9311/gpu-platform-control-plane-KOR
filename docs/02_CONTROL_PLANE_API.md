# Control Plane API

> **Status (2026-08-07).** Per CRD: `InferenceDeployment`, `GPUQuotaPolicy`, and `NodeHealth` are **built**
> (`NodeHealth` gets no GPU fault signal — nothing Xid or ECC exists, and the DCGM code that does exist is
> a utilisation reader for the queuelab rather than a health input). `MLTrainingJob`
> is **built** (M6, the only milestone with live end-to-end evidence, run on kind — never on real hardware).
> `GpuSharingBenchmark` is **designed only — no CRD**, though its sizing arithmetic and run script exist.
> `WorkloadRun` is **built and has been run for real on kind** (M7): a CRD, a controller, a driver, and a
> recorded run in which deleting a serving Pod produced a recovery trail nobody wrote by hand. The gateway
> (Layer 4, M4-b) is **built and unit-tested but never deployed**. The M5 KV-cache-aware admission guard is
> **built, and MEASURED on a paid GPU**: four repetitions on 2026-09-03 and an engine-level scheduler microtest on 2026-09-04. The guard failed — 83.7x against a pre-registered 1.25x premium-tail target — and the harness declared the run invalid rather than reporting a protection claim. Every GPU in the kind clusters is simulated by a fake device plugin. The GPUs in the two paid EC2
> sessions were real.

The control plane is a set of CRDs in API group `platform.lkhun9311.github.io/v1`, each reconciled by a controller that converges native Kubernetes objects toward the declared intent. (Some pasted designs use a shorter `platform.ai/v1` as a conceptual surface; the implemented group is the one above, and all examples here use it.)

## CRD family

| CRD                   | Role                                                               | Tier                                       | Implemented today (2026-07)                                                                                                      |
|-----------------------|--------------------------------------------------------------------|--------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------|
| `InferenceDeployment` | model-serving intent → Deployment/Service/KEDA                     | Core                                       | type + serving reconciler (Deployment/Service, phase ladder) — M4-a merged                                                       |
| `GPUQuotaPolicy`      | per-tenant GPU quota / rate limit → ResourceQuota + gateway config | Core                                       | type + reconciler (ResourceQuota sync, drift recovery) — M3 merged; `rateLimit` field consumed by the M4-b gateway — **M4-b merged, gateway built and unit-tested, never deployed** |
| `NodeHealth`          | GPU node intake & operational state                                | Core                                       | type + reconciler (observe + taint, finalizer, drift recovery) — M2/M3 merged                                                    |
| `GpuSharingBenchmark` | declarative noisy-neighbor / sharing benchmark                     | Core (killer feature)                      | designed — spec `2026-07-04-gpusharingbenchmark-crd-design.md`; no code yet (M5)                                                 |
| `WorkloadRun`         | record a workload execution as evidence                            | Evidence / CRD-lite                        | sketched below only — no spec or code yet (M7)                                                                                   |
| `MLTrainingJob`       | Kueue-admitted training job                                        | **M6 (promoted from stretch, 2026-07-04)** | type + full reconciler — Job+Kueue Workload translation, two-tenant cohort borrowing/reclaim preemption, run end-to-end on kind — **M6 merged**, the only milestone with live end-to-end evidence (`hack/m6-kind-e2e.md`) |

`MLTrainingJob` was promoted from stretch to **M6** (2026-07-04): it shows the same `GPUQuotaPolicy` can extend from inference to training. The main narrative stays inference-first GPUaaS + performance isolation; M6 is the training-admission bridge, not a second flagship.

> **Quota ownership rule (M6 design decision):** inference quota flows `GPUQuotaPolicy → ResourceQuota` (as shipped in M3). Training quota flows `GPUQuotaPolicy → Kueue ClusterQueue/ResourceFlavor` (`TrainingQuotaSynced` condition) — Kueue owns the training admission decision. The same GPUs are never counted by both mechanisms: a ResourceQuota that also counted training GPUs could block a pod Kueue already admitted (double-accounting). If a namespace ResourceQuota is kept over training, it is an intentional coarse ceiling with the documented invariant `Kueue quota ≤ ceiling`.

## 4-layer admission

Quota and policy are enforced in depth, each layer doing what it is best at:

| Layer | Mechanism                                                   | Enforces                                                                                                      |
|-------|-------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------|
| 1     | ValidatingAdmissionPolicy (VAP)                             | static policy — GPU-class allowlist, replica max, image registry                                              |
| 2     | ResourceQuota                                               | namespace GPU slots, CR counts (hard, dynamic)                                                                |
| 3     | Controller conditions                                       | `QuotaSatisfied`, `NodeClassHealthy`, `WarmCacheReady` before serving                                         |
| 4     | Gateway token bucket (+ M5: KV-cache-aware admission guard) | runtime RPM / burst → HTTP 429; under backend pressure, selective 429 for standard-tier long-context requests |

VAP rejects malformed/forbidden intent at submit time; ResourceQuota caps consumption; the controller refuses to mark a workload Ready until quota and node prerequisites hold; the gateway shapes runtime traffic. No single layer is trusted to do all of it.

> VAP is intentionally limited to static validation. It does not query cluster-wide quota state. Dynamic GPU quota is enforced through a `ResourceQuota` synchronized by the controller, not by VAP.

## InferenceDeployment (target design)

```yaml
apiVersion: platform.lkhun9311.github.io/v1
kind: InferenceDeployment
metadata:
  name: llama3-8b
  namespace: tenant-a
spec:
  tenant: tenant-a
  model:
    name: llama3-8b
    artifactUri: s3://models/llama3-8b/v1
    digest: sha256:abc...
    sizeGiB: 16
  runtime:
    engine: vllm
    image: vllm/vllm-openai:<tested-version>
    args:
      maxModelLen: 4096
      maxNumSeqs: 64
      enablePrefixCaching: true
      gpuMemoryUtilization: 0.90
  gpu:
    class: a10g
    count: 1
  replicas: { min: 1, max: 3 }
  autoscaling: { metric: queue_depth, target: 20 }
  slo: { ttftP95Ms: 1500, tpotP95Ms: 100 }
  warmCache: { enabled: true }
status:
  phase: Ready
  conditions:
    - { type: QuotaSatisfied, status: "True" }
    - { type: NodeClassHealthy, status: "True" }
    - { type: WarmCacheReady, status: "True" }
```

The controller reconciles this into a Deployment (vLLM), a Service, a ConfigMap, and a KEDA ScaledObject, owned via owner references, and gates `phase: Ready` on the conditions above.

> Implemented today (M4-a merged): the CRD type carries `model{name,storageUri}`, `image`, `gpuClass`, `gpuCount`, `replicas`, `port`, and the serving reconciler converges a Deployment + Service with a 7-step phase ladder (`phase / observedGeneration / readyReplicas / conditions`). The richer `runtime/autoscaling/slo/warmCache` fields, ConfigMap, and KEDA ScaledObject above are the target design — later milestones, not yet built. (Target field names like `artifactUri` are aspirational; the implemented name is `storageUri`.)

## GPUQuotaPolicy (target design)

```yaml
apiVersion: platform.lkhun9311.github.io/v1
kind: GPUQuotaPolicy
metadata:
  name: tenant-a-quota
  namespace: tenant-a
spec:
  tenant: tenant-a
  allowedGpuClasses: [a10g, l4, t4]
  maxConcurrentInferenceDeployments: 3
  maxReplicasPerDeployment: 3
  maxGpuPerDeployment: 1
  maxGpusInUse: 2
  rateLimit: { requestsPerMinute: 600, burst: 100 }   # implemented field names (api/v1)
status:
  phase: Active
  usage: { currentInferenceDeployments: 1, currentGpusInUse: 1, requestsLast1m: 82 }
```

The controller syncs this into a namespace `ResourceQuota` (and feeds the gateway token bucket).

> Implemented today: `tenant`, `targetNamespace`, `gpuClass`, `limits.gpuCount`, and optional `rateLimit{requestsPerMinute,burst}`, with a phase/observedGeneration/conditions status. ResourceQuota sync (the aggregate GPU ceiling, kept in sync against drift, ownership-guarded) shipped in M3; `rateLimit` is consumed by the M4-b gateway (in progress). The richer limit set (`allowedGpuClasses` and per-class scoping, replica limits) is a later target, and the M5 guard adds a tenant tier (annotation first, `spec.tier` if promoted — see the admission-guard spec).

## GpuSharingBenchmark (Core — killer feature)

Canonical schema and rationale: `docs/superpowers/specs/2026-07-04-gpusharingbenchmark-crd-design.md` (field names `gpuClass`, `reportUri`; `repetitions >= 5`; open-loop `load` spec is mandatory). Sample and protocol: doc 04.

```yaml
apiVersion: platform.lkhun9311.github.io/v1
kind: GpuSharingBenchmark
metadata:
  name: premium-vs-longcontext-shared
  namespace: platform-system
spec:
  gpuClass: a10g
  sharingMode: sharedInstance     # exclusive | timeSlicing | mps | sharedInstance
  baseline:  { tenant: tenant-premium,  model: llama3-8b, qps: "2.0", inputTokens: 256,  outputTokens: 128 }
  contender: { tenant: tenant-standard, model: llama3-8b, qps: "6.0", inputTokens: 8192, outputTokens: 256 }
  repetitions: 5
  warmupRequests: 50
  minRequestsPerRun: 1000
  load: { mode: openLoop, generator: "genai-perf", streaming: true, timeoutMs: 60000, retries: 0 }
# status.result{baselineP99Ms,colocatedP99Ms,interferenceRatio,p99CI95,reportUri} is written
# only by a real-GPU run — never placeholder numbers.
```

Starts as CRD + sample + benchmark harness + a thin status writer; a deeper controller is optional. Status numbers come from a real-GPU run (doc 04); they are not invented locally.

## WorkloadRun (new, Evidence CRD-lite)

A common record of a workload execution (inference load test, benchmark, failure injection, training). Deliberately thin — no heavy reconciler — so it stays an evidence resource, not an MLOps platform.

```yaml
apiVersion: platform.lkhun9311.github.io/v1
kind: WorkloadRun
metadata:
  name: llama3-loadtest-20260623
spec:
  tenant: tenant-a
  workloadType: inference        # inference | benchmark | failure | training
  targetRef: { kind: InferenceDeployment, name: llama3-8b }
  scenario: gateway-loadtest
status:
  phase: Completed
  startedAt: "2026-06-23T12:00:00Z"
  completedAt: "2026-06-23T12:10:00Z"
  metrics: { p95LatencyMs: 620, p99LatencyMs: 1300, throughputRps: 18, errorRate: 0.01 }
  reportUri: evidence/benchmark-reports/gateway-loadtest.md   # illustrative path only — WorkloadRun has no
                                                                # spec or code yet (M7); nothing is committed
                                                                # at this path today
```

## MLTrainingJob (M6 — promoted from stretch)

Implemented as a CRD type (queue/image/command/gpuClass/gpuCount/parallelism/completions) with a full reconciler — **M6 is merged**. It demonstrates that `GPUQuotaPolicy` can govern training as well as inference. The reconciler translates a `MLTrainingJob` into a `batch/v1` Job admitted through Kueue (LocalQueue/ClusterQueue/ResourceFlavor), with 2-tenant fair sharing, a preemption/borrowing scenario, and queue-wait/admitted/denied metrics, run end-to-end on kind (evidence: `hack/m6-kind-e2e.md`) — this is the only milestone in the project with live end-to-end evidence. It ran on simulated GPU capacity; no real hardware was used. The optional artifact-lite (one real single-GPU PyTorch fine-tune job run through the same path during an M5 GPU session) has not happened — the M5 real-GPU session itself has not happened. Quota ownership follows the rule above.
