# M5 — KV-cache-aware admission guard (design spec, v1)

Date: 2026-07-04 · Milestone: M5 (real-GPU flagship) · Author: lkhun9311

This is the independent variable of the M5 flagship experiment (doc 04, R3 vs R4). It extends the M4-b gateway (`docs/superpowers/specs/2026-06-28-m4-b-gateway-design.md`); it does not replace the token bucket. Reconciled from the Claude + codex portfolio review (2026-07-04): both reviews independently converged on this mechanism.

## Problem

The M4-b gateway enforces a **static** per-tenant token bucket. A static RPM limit cannot protect a premium tenant's p99 from a *within-limit* long-context noisy neighbor: the neighbor stays under its RPM budget while saturating the shared vLLM instance's KV cache and waiting queue. The guard adds a **dynamic, backend-pressure-driven** admission layer so that "KV-cache-aware" is a mechanism, not a label.

## Topology assumption (load-bearing)

The guard protects tenants that share **one vLLM instance** (one shared KV-cache pool), with the gateway multiplexing tenants in front of it. This is the realistic GPUaaS serving shape and the only topology where "KV cache pressure" is the actual interference mechanism. Two separate vLLM pods time-slicing one GPU contend on GPU *time*, not KV cache — that is experiment S2/S3 territory (doc 04), not this guard.

## Mechanism

```
scrape (2s)  vLLM /metrics:  KV-cache usage + waiting-queue depth (exact series pinned below)
   |
   v
pressure state machine (per backend)
   ENGAGE  when  cache_usage > 0.85  OR  waiting > W        for 2 consecutive scrapes
   RELEASE when  cache_usage < 0.75  AND waiting < W/2      sustained for 30s
   |
   v
admission decision (only while ENGAGED)
   tenant tier == premium                      -> pass (token bucket still applies)
   tenant tier == standard AND est. input tokens >= 4096 -> 429, reason=kv_cache_pressure
   otherwise                                   -> pass
```

- **Metric names are pinned, not assumed**: vLLM has renamed these series across versions (V0 `vllm:gpu_cache_usage_perc` → V1 `vllm:kv_cache_usage_perc`; waiting queue `vllm:num_requests_waiting`). The design pins **one vLLM image** for M5; a captured `/metrics` **golden fixture** from that exact image is committed with the harness, and the scraper reads the fixture-verified names (with the V0/V1 pair as configurable aliases). Building against unpinned metric names is how the guard silently reads nothing.
- **Metrics endpoint discovery**: vLLM serves Prometheus metrics on the **same HTTP port as the OpenAI API** — the scrape URL is the routed backend URL + `/metrics` (`http://<infd-name>.<ns>.svc:<port>/metrics`), so no new `InferenceDeployment` field is needed. An annotation override (`platform.lkhun9311.github.io/metrics-url`) covers non-vLLM backends. Scrape-URL construction is httptest-covered.
- **Signal**: a background goroutine scrapes each routed backend every 2s. Scrape failure = pressure state unchanged (fail-open; the guard degrades to plain token bucket).
- **Hysteresis**: engage/release thresholds are deliberately asymmetric (0.85 / 0.75, 2-scrape engage / 30s release) so the guard does not flap at the boundary.
- **Selectivity**: only *standard-tier long-context* requests are rejected. Estimated input tokens = `len(string message content) / 4` — a deliberately **approximate, conservative heuristic**: non-string content parts count via their JSON length, and the threshold is calibrated offline with the target model's tokenizer in the harness. The gateway never claims exact token counts. Premium requests are never guard-rejected; short standard requests pass.
- **W** (waiting-queue threshold): **default 8 is for tests/dev only** — queue depth is model/hardware/`max-num-seqs` dependent. The real value is calibrated from the saturation sweep (doc 04, R0), and the run report records both the calibrated W and the vLLM scheduler settings it was derived under.

## Tenant tier

- v1: annotation on `GPUQuotaPolicy`: `platform.lkhun9311.github.io/tier: premium|standard` (absent → `standard`). No CRD change needed to start.
- v2 (if promoted): `spec.tier` field with kubebuilder enum validation, replacing the annotation.

## API surface

- Rejection response: HTTP 429, body `{"error":{"code":"kv_cache_pressure","message":...}}`, `Retry-After: 5`. Distinct from token-bucket 429 (`code=rate_limited`) so the two layers are distinguishable in logs and in the R3/R4 comparison.
- Config: guard on/off via gateway flag `--admission-guard` (the R3/R4 switch), thresholds via flags with the defaults above.

## Metrics

```
gpuaas_gateway_admission_guard_decisions_total{tenant,decision,reason}       # decision=pass|reject
gpuaas_gateway_admission_guard_engaged{namespace,backend,model}              # gauge 0/1
gpuaas_gateway_backend_kv_cache_usage{namespace,backend,model}               # last scraped value
gpuaas_gateway_backend_waiting_requests{namespace,backend,model}             # last scraped value
```

Backend-scoped series carry `{namespace,backend,model}` labels — without them multiple routed backends collapse into one gauge.

## Pre-registered success criteria (stated before the run, doc 04)

- **Protection**: R4 premium p99 ≤ 1.25 × R1 premium p99.
- **Cost honesty**: report the standard tenant's guard-429 rate and throughput loss alongside — killing tenant B to save tenant A is not "protection" unless the cost is shown.
- If the protection threshold is not met, the report says the guard did not protect at these settings — the word "protects" is not used.

## Non-goals

- Modifying vLLM's scheduler or KV-cache manager (scrape-only; consistent with doc 04 non-goals).
- Predicting *output* length (unknowable at admission; input-length heuristic only).
- Distributed guard state (single-replica gateway, per M4-b ADR).
- Claiming causal proof — the guard acts on correlated pressure signals, and the analysis says so.

## Testing

envtest/httptest, same harness as M4-b: stub backend serves the **golden `/metrics` fixture** (captured from the pinned vLLM image) with controllable KV-cache/waiting values; specs cover scrape-URL construction, alias fallback (V0/V1 names), engage (2-scrape), release (30s hysteresis), premium passthrough, standard long-context 429 with `kv_cache_pressure` reason, short-request passthrough under pressure, scrape-failure fail-open, and the decision metrics. Real-latency effect is only measurable on real GPU (M5 run).
