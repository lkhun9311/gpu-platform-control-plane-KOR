# M4-b — tenant-aware serving gateway (design spec, v2 codex-reconciled)

Date: 2026-06-28 · Milestone: M4 (serving), sub-project **M4-b** · Author: lkhun9311

Decisions: four ADRs and a runtime-tradeoffs note, kept as internal working documents and not published. v2 folds in an independent AI design review: tenant↔namespace mapping, policy 0/1/many rules, missing-policy default, model duplicate handling, streaming proxy, body restore, 502/504 criteria, bucket lifecycle, single-replica, Secret schema, request_id, RBAC, /metrics.

## Scope ("minimum done", docs/05)
Standalone OpenAI-compatible gateway: API key → tenant, per-tenant token bucket → 429, model → `InferenceDeployment` Service routing, reverse-proxy, 4 `gpuaas_gateway_*` metrics, `request_id`. **Only `POST /v1/chat/completions`** (ADR-3). Dev/test on kind with **httptest stub upstreams** (no real vLLM; real serving = M5/AWS). Deferred: `/v1/embeddings`, distributed bucket, per-model limits, Open WebUI, `platformctl`.

## Identity model (codex finding #2 — was underspecified)
`GPUQuotaPolicy` is **cluster-scoped** with `spec.tenant` and `spec.targetNamespace`. Canonical chain:

```
API key  --(Secret gateway-api-keys)-->  tenant
tenant   --(GPUQuotaPolicy where spec.tenant == tenant)-->  policy
policy.spec.targetNamespace  = namespace to route InferenceDeployments in
policy.spec.rateLimit        = this tenant's token-bucket config
```
- **0 policies** for the tenant → **403** (tenant not provisioned). [explicit, observable]
- **>1 policies** with the same `spec.tenant` → config error: pick **deterministic** (oldest `creationTimestamp`, name as tiebreak), log a warning + emit a metric; never nondeterministic limiter state.
- `policy.spec.rateLimit == nil` → **unlimited**, logged once + a `gpuaas_gateway_unlimited_tenant_total{tenant}` signal (explicit, not silent).

## CRD change
Add to `api/v1/gpuquotapolicy_types.go` (optional, backward-compatible):
```go
// +optional
RateLimit *GPUQuotaRateLimit `json:"rateLimit,omitempty"`
type GPUQuotaRateLimit struct {
    // +kubebuilder:validation:Minimum=1
    RequestsPerMinute int32 `json:"requestsPerMinute"`
    // +kubebuilder:validation:Minimum=1
    Burst int32 `json:"burst"`
}
```
Regenerate CRD + deepcopy.

## Components (`cmd/gateway/main.go`, `internal/gateway/`)
- **main** — scoped cache + delegating client; readiness gated on `WaitForCacheSync`; HTTP server on `:8080`, `/metrics` on a **separate `:8081`, unauthenticated** (Prometheus scrape).
- **`tenantResolver`** (`tenant.go`) — `Authorization: Bearer <key>` → tenant via the `gateway-api-keys` Secret. **Secret schema:** `stringData[<apiKey>] = <tenant>`. Reloaded live (Secret is in the scoped cache). Missing/unknown → **401**.
- **`bucketRegistry`** (`ratelimit.go`) — `map[tenant]*rate.Limiter` (`golang.org/x/time/rate`). Per request, look up the tenant's policy `rateLimit`; **`rate.Limit(float64(rpm)/60.0)`** (per-second; avoid the 60× bug), `Burst=int(burst)`. On policy change `SetLimit`/`SetBurst`; on delete / `rateLimit=nil` / tenant removal, drop the limiter. `Allow()` false → **429** (`rate_limited_total{tenant}`).
- **`router`** (`router.go`) — resolve the policy (identity model) → `targetNamespace`; find the `InferenceDeployment` with `spec.model.name == model` via a **field indexer on `.spec.model.name`** over the cache (no per-request apiserver call; no CR field selector). Duplicates → deterministic oldest-wins + warn. 0 → **404**. Backend = `http://<infd-name>.<ns>.svc:<port>`.
- **`proxy`** — `httputil.ReverseProxy` with **`FlushInterval` for streaming** (immediate flush so `stream:true` SSE/chunked is not buffered), explicit `Transport` timeouts, and a custom **`ErrorHandler`**: connect/refused/DNS → **502**, timeout/context-deadline → **504**; upstream HTTP 5xx **passes through as-is** (not converted).
- **`metrics`** — the 4 series; **`obs`** — structured log with tenant, model, `request_id`.

## Request flow — `POST /v1/chat/completions`
1. Method/path guard: non-POST or other path → **405/404** (do not proxy unsupported endpoints).
2. Auth: bearer → tenant → **401** if unknown.
3. Tenant provisioned: resolve policy → **403** if none.
4. Rate: `bucketRegistry.Allow(tenant)` → **429**.
5. Body: `MaxBytesReader` (≈1 MiB) → parse `model`; malformed JSON / missing `model` / too large → **400**. **Restore the body** (`io.NopCloser(bytes.NewReader(buf))`) for the upstream.
6. Route: model → `InferenceDeployment` Service → **404** if none.
7. Proxy; on transport error **502/504**; record metrics + log `request_id` (generate `X-Request-Id` if absent; forward upstream + echo in response).

## Reads (runtime-tradeoffs.md)
Scoped cache over `InferenceDeployment` + `GPUQuotaPolicy` + the api-keys `Secret` (namespace/label scope, **not** a CR field selector — needs CRD `selectableFields`). `DefaultTransform: cache.TransformStripManagedFields()`. Field **indexer** on `InferenceDeployment.spec.model.name` (via `cache.IndexField`). Readiness NotReady until synced.

## Error codes
| Condition                                       | Status               |
|-------------------------------------------------|----------------------|
| wrong method/path                               | 405 / 404            |
| missing/unknown API key                         | 401                  |
| tenant has no GPUQuotaPolicy                    | 403                  |
| token bucket exhausted                          | 429                  |
| malformed JSON / missing model / body too large | 400                  |
| no InferenceDeployment for model                | 404                  |
| upstream connect/refused/DNS                    | 502                  |
| upstream timeout/deadline                       | 504                  |
| upstream HTTP 5xx                               | passed through as-is |

## Deployment
`config/gateway/`: Deployment **`replicas: 1`** (in-memory bucket — scaling multiplies limits; documented), Service, ServiceAccount, RBAC. **RBAC:** `get;list;watch` on `inferencedeployments`, `gpuquotapolicies` (cluster-scoped), and the `gateway-api-keys` Secret (gateway namespace).

## Testing (envtest + httptest, on kind)
Stub backend = `httptest.Server`; CRDs/Secret patched to set routes/limits/keys.
1. **method/path** — non-POST/other path → 405/404.
2. **auth** — unknown/missing key → 401; known → proceeds.
3. **provisioning** — tenant with no policy → 403.
4. **token bucket** — within burst → proxied; exceed → 429; **policy update** (SetLimit) and **policy delete** (limiter dropped) covered, not just initial burst.
5. **body** — malformed/missing model → 400; valid body forwarded intact (restored) to upstream.
6. **routing** — model matches → proxied to stub; unknown → 404; duplicate models → deterministic oldest + warn.
7. **streaming** — `stream:true` chunked/SSE from the stub flushes promptly (no full buffering).
8. **upstream errors** — refused → 502; timeout → 504; stub 5xx → passthrough.
9. **request_id** — generated if absent, forwarded + echoed; present in logs.
10. **metrics** — 4 series on `:8081/metrics`.
Not tested: real vLLM inference (M5/AWS).

## File structure
- `api/v1/gpuquotapolicy_types.go` (+`rateLimit`) + regenerated CRD/deepcopy.
- `cmd/gateway/main.go`.
- `internal/gateway/`: `server.go`, `tenant.go`, `ratelimit.go`, `router.go`, `proxy.go`, `metrics.go` (+ `_test.go` each + an integration test wiring server↔stub).
- `config/gateway/`: deployment, service, serviceaccount, rbac.

## Out of scope / follow-ups
`/v1/embeddings`; distributed token bucket (Redis); per-model limits; Open WebUI / `platformctl`; Prometheus ServiceMonitor (M4 observability profile); auth on `/metrics`.

## Addendum (2026-07-04) — error response body contract

The error table above defines status codes only. The M5 admission-guard spec (`2026-07-04-m5-admission-guard-design.md`) needs the two 429 sources distinguishable, so the response **body** contract is fixed here: every gateway-generated error returns OpenAI-style JSON `{"error":{"code":"<canonical>","message":"..."}}` with canonical codes — `rate_limited` (token bucket 429), `kv_cache_pressure` (guard 429, M5), `unknown_api_key` (401), `tenant_not_provisioned` (403), `model_not_found` (404), `invalid_request` (400), `upstream_unreachable` (502), `upstream_timeout` (504). Upstream-generated responses (including 5xx) pass through unmodified.
