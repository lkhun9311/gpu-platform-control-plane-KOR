# LLM Serving Gateway

> Design of record: `docs/superpowers/specs/2026-06-28-m4-b-gateway-design.md` (M4-b, codex-reconciled) plus the M5 extension `docs/superpowers/specs/2026-07-04-m5-admission-guard-design.md`. **Status (2026-08-07): built and unit-tested — never deployed.** M4-b (routing/auth/rate-limit/proxy/metrics) is merged. The M5 KV-cache-aware admission guard is also built and unit-tested, but it has never run against a real GPU, and its vLLM metrics fixture is synthetic, so its engage/release thresholds are unvalidated.

The gateway is not the main feature; it is the runtime boundary that makes the GPUaaS control plane actually usable, and it enforces Layer 4 of admission — statically via the token bucket (M4-b) and dynamically via the KV-cache-aware admission guard (M5).

## Responsibilities (M4-b "minimum done")

| Function             | Detail                                                                                                                               |
|----------------------|--------------------------------------------------------------------------------------------------------------------------------------|
| tenant resolution    | `Authorization: Bearer <key>` → tenant via the `gateway-api-keys` Secret                                                             |
| tenant provisioning  | tenant → `GPUQuotaPolicy` (`spec.tenant` match); no policy → 403                                                                     |
| token bucket         | per-tenant `rateLimit{requestsPerMinute,burst}` from the policy → 429                                                                |
| model routing        | body `model` → `InferenceDeployment` with `spec.model.name == model` in the policy's `targetNamespace` (cache field index) → Service |
| proxy                | `httputil.ReverseProxy`, streaming-safe (`FlushInterval`), 502/504 mapping, upstream 5xx passthrough                                 |
| OpenAI compatibility | `POST /v1/chat/completions` only (ADR-3); `/v1/embeddings` deferred                                                                  |
| metrics              | 4 `gpuaas_gateway_*` series on a separate `:8081`                                                                                    |
| audit                | structured logs with tenant, model, `request_id` (generated if absent, forwarded + echoed)                                           |

## Identity chain (canonical)

```
API key --(Secret gateway-api-keys)--> tenant
tenant  --(GPUQuotaPolicy spec.tenant)--> policy   (0 → 403; >1 → deterministic oldest + warn)
policy.spec.targetNamespace --> namespace to route InferenceDeployments in
policy.spec.rateLimit       --> token-bucket config (nil → unlimited, logged + metric)
```

## Error contract

| Condition                                       | Status      | canonical `error.code`      |
|-------------------------------------------------|-------------|-----------------------------|
| wrong method/path                               | 405 / 404   | —                           |
| missing/unknown API key                         | 401         | `unknown_api_key`           |
| tenant has no GPUQuotaPolicy                    | 403         | `tenant_not_provisioned`    |
| token bucket exhausted                          | 429         | `rate_limited`              |
| guard engaged, standard-tier long-context (M5)  | 429         | `kv_cache_pressure`         |
| malformed JSON / missing model / body too large | 400         | `invalid_request`           |
| no InferenceDeployment for model                | 404         | `model_not_found`           |
| upstream connect/refused/DNS                    | 502         | `upstream_unreachable`      |
| upstream timeout/deadline                       | 504         | `upstream_timeout`          |
| upstream HTTP 5xx                               | passthrough | (upstream body, unmodified) |

The two 429 sources are deliberately distinguishable — the M5 flagship's R3/R4 comparison depends on telling them apart in logs and metrics.

## M5 extension — KV-cache-aware admission guard

The token bucket is static; it cannot protect a premium tenant's p99 from a *within-limit* long-context noisy neighbor on a shared vLLM instance. The guard scrapes the backend's KV-cache usage and waiting-queue depth (pinned vLLM image, golden `/metrics` fixture), and while pressure is engaged (hysteresis-guarded) selectively rejects standard-tier long-context requests. Design, thresholds, tier model, and pre-registered success criteria: the guard spec. Flagship experiment protocol: doc 04.

## Open WebUI principle

> Open WebUI must not connect directly to vLLM. It always uses the gateway as its OpenAI-compatible base URL.

This single rule is what makes it "a platform boundary" rather than "a UI bolted onto vLLM."

## Minimum metrics (M4-b)

```
gpuaas_gateway_requests_total{tenant,model,code}
gpuaas_gateway_request_duration_seconds_bucket{tenant,model}
gpuaas_gateway_rate_limited_total{tenant}
gpuaas_gateway_upstream_errors_total{tenant,model}
```

M5 adds the guard series (`admission_guard_decisions_total`, backend pressure gauges — guard spec).

## Deployment

`config/gateway/`: Deployment `replicas: 1` (in-memory bucket — scaling multiplies limits; documented ADR), Service, ServiceAccount, minimal RBAC (`get;list;watch` on the two CRDs + the api-keys Secret). Definition of done for M4-b includes the Makefile target, a gateway Dockerfile, and these manifests — `go run` is not a deployment story.

## Deferred

`/v1/embeddings` · distributed token bucket (Redis) · per-model limits · Open WebUI wiring · `platformctl` · ServiceMonitor · auth on `/metrics`.
