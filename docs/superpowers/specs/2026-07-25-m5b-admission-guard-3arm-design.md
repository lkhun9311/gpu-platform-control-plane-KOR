# M5-b — KV-aware admission guard, 3-arm experiment (design spec, v2)

Date: 2026-07-25 · Milestone: M5-b (primary killer) · Supersedes the v1 2-arm guard design (`2026-07-04-m5-admission-guard-design.md`).

Reconciled from a codex cold architecture review (2026-07-25). v1 was a 2-arm A/B (guard off vs on); a guard that lowers p99 by rejecting more work is just load shedding, so v2 adds a **pressure-blind, admission-matched control arm**. This spec is the design of record for the GPU-free build; the paid GPU run only captures numbers.

## The experiment: four conditions

| Condition | Traffic | Control | Purpose |
| --- | --- | --- | --- |
| **R1** premium alone | premium only | token bucket | uncontended latency baseline |
| **A** off | premium + noisy contender | token bucket only | shows the contention problem |
| **B** static work cap | same | pressure-blind weighted input-token cap | the honest control |
| **C** KV-aware | same | KV/queue-driven guard | our contribution |

R1/A/B/C share one recorded open-loop trace. The old direct-to-vLLM condition (R2) is optional context, cut from minimum publishable scope.

## Arm B — pressure-blind, admission-matched control

Arm B is a **weighted token bucket over estimated input tokens** (reuses the project's `x/time/rate` pattern), NOT a concurrency cap. Concurrency is rejected as the control because it depends on output length/duration, is hard to match to an integer cap, needs lease-release after a possibly long stream, and adds a completion-feedback mechanism (a second confound).

- Per backend / KV pool (not global).
- Applies to the **exact same eligible population as C**: `standard && estimatedInputTokens >= 4096`. Premium and standard-short pass in both B and C.
- Fixed rate and burst throughout the confirmatory run. No vLLM telemetry.

### "Admission-matched" defined precisely (no circularity)

Over the valid, routed, standard-long candidate population:

```
admitted_work_fraction = (exact target-tokenizer input tokens ADMITTED)
                       /  (exact target-tokenizer input tokens OFFERED)
```

Match B to C within a **pre-registered ±5% relative** tolerance. Also report request admission fraction as a sanity check.

Tuning procedure (avoids circularity):
1. Run a **C pilot** on pilot-only traces; obtain C's candidate admitted-work fraction.
2. Simulate the static token bucket offline on the pilot arrival trace; pick ONE fixed B rate; freeze burst independently.
3. Freeze all configuration.
4. Run confirmatory repetitions on DIFFERENT pre-generated traces.
5. If B and C miss the match tolerance in confirmatory data, declare the matched comparison invalid. **Never retune on confirmatory results.**

Do NOT match on: global admission rate (premium dilutes denominator), completion throughput (an outcome), rejection count without a fixed trace, or output-token throughput (unknown at admission, an outcome).

## Arm C — KV-aware guard (from v1, with fixes)

Mechanism unchanged from v1 (scrape vLLM `/metrics` every 2s for KV-cache usage + waiting depth; per-backend pressure state machine, engage 0.85/2-scrape, release 0.75/30s hysteresis; while engaged, reject `standard && est_input_tokens >= 4096`, premium always passes). Fixes from the review:

- **Fail-open is a staleness bypass, not "state unchanged".** Retaining `ENGAGED` after scrape failure keeps rejecting — that is fail-CLOSED. Bypass C entirely whenever `now - lastSuccessfulScrape > maxStaleness`. Reset the consecutive-sample timers after staleness. Hysteresis counts only **fresh, successful** samples on **monotonic** time.
- **Single KV pool assertion.** Scraping a Service with multiple vLLM pods samples an arbitrary pod while requests hit another. The flagship asserts exactly one vLLM pod / one KV pool. (InferenceDeployment currently permits arbitrary replicas; the benchmark topology pins replicas=1.)
- **Cold registration / prewarm.** An on-demand scraper has no state for the first requests; register and prewarm during benchmark warmup and record telemetry freshness before measurement begins.
- **Request-path never scrapes and never holds a mutex during HTTP.** The scraper runs in the background, publishes an immutable snapshot atomically; the request path reads the snapshot lock-free.
- **Scraper hardening:** bounded timeout, bounded response body, no redirects, status-code validation, finite/range validation of parsed values, no forwarded API credentials.
- **Metric aggregation:** if exposition has multiple engine/model series, take max cache usage and sum waiting for the one logical backend, or reject the unexpected shape.
- **Estimator honesty:** `len(content)/4` is not universally conservative or model-independent. Scope M5 to **text-only** prompts, use **ceiling**, calibrate on the exact tokenizer + chat template, and report false-positive/false-negative behavior around 4096.
- **Tier errors resolve to standard.** Absent or invalid `.../tier` annotation → standard. A typo must never grant premium bypass.
- **Cut the metrics-URL annotation override** (SSRF primitive if tenants can annotate InferenceDeployment) for M5, or restrict it to the resolved backend host.

## Pipeline placement

One 3-way decision point AFTER body-parse and backend-resolve, BEFORE proxy. Keep the cheap token bucket before body buffering (it rejects floods before reading up to 1 MiB). Arm B sits at the same stage as C even though it needs no telemetry, so invalid/unroutable requests never consume B's budget without entering C's population.

```
request id → tenant → policy → RPM token bucket
  → parse model + admission metadata (raw bytes + RequestMeta, byte-for-byte body restore)
  → resolve BackendRef{namespace,name,port,url,model}
  → admission: off | static-work-cap | kv-aware   (--admission-mode)
  → streaming proxy
```

Refactors: body-parse returns raw bytes + `RequestMeta` (not just model); routing returns `BackendRef` (not just `*url.URL`) so the scraper can manage lifecycle and metric labels; all three modes implement one synchronous `Admit(ctx, RequestMeta, BackendRef) (bool, reason)` interface; `off`/`static-cap` start no scrapers. Also fix the existing error-contract drift: `fail()` uses `http.Error` (plain text) while the design promises OpenAI-style JSON — make all gateway errors consistent JSON, not just the two new 429s.

## Config and API

- `--admission-mode=off|static-cap|kv-aware` (replaces the v1 boolean `--admission-guard`). Thresholds/rate via flags.
- 429 bodies: `{"error":{"code":"kv_cache_pressure",...}}` (arm C) and `{"error":{"code":"input_rate_limit",...}}` (arm B), `Retry-After: 5`, distinct from token-bucket `rate_limited`.

## Metrics

```
gpuaas_gateway_admission_decisions_total{mode,tenant,model,decision,reason}
gpuaas_gateway_admission_input_tokens_total{mode,tenant,decision}     # REQUIRED to prove admission matching
gpuaas_gateway_admission_guard_engaged{namespace,backend,model}       # gauge 0/1 (arm C)
gpuaas_gateway_backend_kv_cache_usage{namespace,backend,model}
gpuaas_gateway_backend_waiting_requests{namespace,backend,model}
gpuaas_gateway_backend_telemetry_fresh{namespace,backend}             # 0/1 staleness
gpuaas_gateway_backend_scrape_errors_total{namespace,backend}
```

Decision counts alone cannot prove admission matching; the input-token counter is mandatory.

## Pre-registered success criteria (before the run)

- **Absolute protection:** C premium **TTFT p99** ≤ 1.25 × R1.
- **Incremental value vs the honest control:** C/B premium TTFT p99 ≤ 0.90, with the bootstrap CI upper bound below 1.0.
- **Admission-work match:** B and C within ±5% relative.
- Primary endpoint is **TTFT p99** (queueing/prefill pressure hits TTFT); E2E p99 and TPOT are secondary. Choosing the endpoint now prevents post-hoc metric selection.
- Repetition/block-aware bootstrap, not a naive bootstrap over pooled requests. If >1% of requests time out, ordinary p99 is censored; report it as at-least-the-timeout, never silently drop.

## Benchmarking harness (the reproducible driver)

The single most important artifact: **replay an identical, immutable open-loop request trace across frozen arms and preserve raw per-request evidence.** The gateway's own request histogram is NOT the benchmark source (its timer starts just before proxy and stops after the stream closes, so it is not TTFT). Use **client-side raw timestamps**.

- Frozen **run manifest** (versioned, validated): arms, trace checksum, image digests, model/tokenizer revision, gateway SHA, thresholds, B rate/burst, endpoint, tolerances.
- **Open-loop** load generation only (Poisson arrivals; a closed-loop client hides queueing via coordinated omission). Pin the generator by container digest. Note: GenAI-Perf is being phased out in favor of **AIPerf**, which supports Poisson rate + exact fixed-schedule replay; pin its digest and replay the same pre-generated trace across arms.
- **Raw per-request artifact** (one row per request): request id, scheduled/send/first-token/end timestamps, tenant, exact + estimated input tokens, output tokens, response/error code, arm.
- Report generation: bootstrap CIs + plots from the raw artifacts (developed against synthetic datasets GPU-free).

## GPU-free vs paid split

**Build and test now (GPU-free, kind/CPU):** mode parsing + startup validation; off/static/KV admission implementations; weighted-token limiter with a fake clock; body estimator + CPU tokenizer calibration (needs the target tokenizer + chat template, NOT a GPU); Prometheus exposition parser against golden/synthetic fixtures; pressure state machine with an injected clock; staleness/fail-open; race tests (scraper snapshot vs request decision); fixed open-loop trace generation + replay; streaming/timeout/error + raw per-request artifact collection; bootstrap/plot/report from synthetic data; a one-command kind/stub end-to-end dry run.

**Requires a real vLLM/GPU (later paid session):** capturing the golden `/metrics` fixture from the exact image digest; confirming labels/aggregation for that engine config; tuning W, cache thresholds, and the static-cap rate; demonstrating pressure is actually induced; TTFT/TPOT/E2E/throughput/DCGM and the C-vs-B result; confirming the scrape reaches the same single KV pool requests use.

## Scope cut: no GpuSharingBenchmark CRD now

The v1 CRD records a 2-arm A/B (`baselineP99Ms`/`colocatedP99Ms`/one ratio) and cannot represent arms, static-cap tuning, the admission-match tolerance/observed, C/B ratio, per-arm admissions/errors/cost, or the primary endpoint. No controller executes it, so it is a YAML result envelope plus an RBAC/status path — little hiring signal on a repo that already has real CRDs/reconcilers. Use a **versioned, validated benchmark run manifest owned by the harness** now. Revisit a CRD only if it will actually orchestrate Jobs and own lifecycle, redesigned with immutable `spec.arms[]` and structured `status.armResults[]`.

## Build order (GPU-free)

1. Gateway enabler refactor: body-parse → raw bytes + `RequestMeta`; routing → `BackendRef`; `fail()` → consistent OpenAI JSON.
2. `Admit` interface + `off` + `static-work-cap` (weighted input-token bucket over the eligible population) + `admission_decisions_total` / `admission_input_tokens_total` metrics.
3. `kv-aware` mode: exposition parser (golden fixture) + pressure state machine (injected clock, staleness/fail-open) + selective admission + engaged/telemetry_fresh/scrape_errors metrics + scraper hardening + atomic snapshot.
4. Benchmark harness: run manifest + open-loop trace gen/replay + raw per-request artifact + report/bootstrap from synthetic data.
5. One-command kind/stub dry run producing the final report layout.

The one thing to get right: #1 harness discipline (immutable trace across frozen arms + raw evidence). If that is wrong, no guard code or CRD rescues the paid run.

## Before the paid run (final-review follow-ups)

The GPU-free build is complete and merge-ready; the whole-branch review's two merge-blockers (premium-only tail; invalidate degenerate/censored runs) are fixed. These remaining items must be closed before the paid confirmatory run so the numbers stay defensible:

- **I2 — cross-arm trace identity + tolerance from manifest.** Stamp the trace sha256 into each RawRow and have `report` assert the off/static-cap/kv-aware arms share one checksum (they must replay identical traffic). Source the admission-match tolerance from the frozen manifest, not the `report --match-tolerance` CLI default, so the pre-registered tolerance cannot be loosened post-hoc.
- **I3 — eligible-population threshold from the manifest.** `report`'s `eligibleLongThreshold` is hardcoded 4096; if the paid pilot tunes `--admission-long-threshold`, stamp it into the manifest/RawRow and read it there, so admitted-work is measured over the same population the guard gated.
- **I4 — warn on a vacuous incremental CI.** When the static-cap and kv-aware repetition counts differ, the CLI leaves the C/B CI at {0,0}; print an explicit warning rather than silently reducing the check to the point estimate.
- **Minor (a)** wire `scraperManager.Unregister` to InferenceDeployment deletion if the gateway is ever run long-lived against a churning backend set (not needed for the frozen replicas=1 benchmark topology).
- **Minor (b)** `parseUsage` validates the aggregated max, not each series (a negative series only lowers the max, so benign).
- **Minor (c)** output-token count is the SSE delta count, not a tokenizer count; keep it labelled approximate (it feeds only secondary metrics; the primary TTFT endpoint is unaffected).
- **Golden fixture** replace `internal/gateway/testdata/vllm_metrics_golden.txt` with a capture from the digest-pinned vLLM image before relying on the parser against the real engine.
