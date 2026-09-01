# M5-b gateway path: the benchmark harness driven through the real gateway on kind (no GPU)

This procedure puts the **real gateway in the middle** of the M5-b measurement path on a local
`kind` cluster, with **no GPU and no AWS**:

```
benchharness replay  ->  gateway (in kind)  ->  stub backend (an InferenceDeployment's Service)
```

The whole run is scripted in [`m5b-gateway-path.sh`](m5b-gateway-path.sh); this document explains
what it does, records the observed evidence, and — as much to the point — records what it does
**not** show.

## Why this exists

The M5-b flagship experiment drives an open-loop benchmark harness *through the gateway* to measure
noisy-neighbour p99 latency. Four defects were found and fixed in that measurement path:

1. a fresh `http.Transport` was constructed **per request**, so no connection was ever pooled;
2. `MaxIdleConnsPerHost` was raised to **600**, derived as `-rate 20/s x -timeout-ms 30s`;
3. `GetBody` was set so a proxied POST is rewindable on the one retry Go allows for it;
4. unvalidated model names from the request body were reaching Prometheus labels on the
   unresolved-model paths, an unbounded-cardinality hole.

Every one of those was verified **by unit tests only**. The existing dry run
[`m5b-harness-dryrun.sh`](m5b-harness-dryrun.sh) proves the harness plumbing, but it points
`--target` straight at the stub on `127.0.0.1:8091`, so the gateway never sees a request. Fixes to
the flagship's measurement path had therefore never been exercised under load anywhere, and a GPU
run on an unvalidated gateway buys a contaminated number.

> **The measurement that matters is not "does it return 200".** Three of the four fixes are only
> observable from *outside* the gateway: a process cannot honestly report that its own connection
> pool worked, because the shared-Transport and the per-request-Transport versions issue exactly the
> same requests. What separates them is only visible to whoever accepts the connections, so the stub
> backend counts them and the script reads the counters back out of the running cluster.

## What the script builds

- a **single-node** `kind` cluster from [`kind-config-gateway-path.yaml`](kind-config-gateway-path.yaml),
  recreated from scratch every run;
- Kueue v0.18.3 — not exercised at all, but the operator's controllers watch Kueue kinds and will
  not start their caches without those CRDs;
- the operator (`config/operator`) and the gateway (`config/gateway`) as deployed, unmodified;
- a `gateway-api-keys` Secret mapping `premium-key -> premium-1` and `standard-key -> standard-noisy`,
  which are the two tenants `gen-trace` builds into every trace;
- two `GPUQuotaPolicy`, one per tenant, both targeting namespace `serving`;
- two `InferenceDeployment`s whose pods run `benchharness stub-serve`: a fast backend
  (`llama-3-8b`, ~5ms to first token) and a slow one (`llama-3-8b-slow`, ~2s), so outbound
  concurrency can be varied;
- NodePort Services so the harness reaches the cluster over a real TCP path.

### What had to be added to the stub, and why

`benchharness stub-serve` could not run as an `InferenceDeployment` at all before this change, and
could not report anything about connections. Four additions, all in `cmd/benchharness/helpers.go`:

- **`/health`** on the serving port. The InferenceDeployment controller hardcodes both probes to
  `GET /health` on the container's `http` port; without it the pod never becomes ready and the
  Service it backs never gets an endpoint.
- **`--model` and `--model-path` flags.** The controller builds every serving container with exactly
  `--model <name> --model-path <storageUri>`. A stub that did not accept them would fail to parse
  its own arguments and crash-loop. `--model` is accepted and ignored.
- **A `stub://` response profile read from `--model-path`.** Because the argument list is fixed, the
  storage URI is the *only* per-deployment knob a backend has. `stub://profile?tokens=4&ttft-ms=2000`
  is how the slow backend differs from the fast one out of one image. A real storage URI
  (`s3://...`, `pvc://...`) passes through untouched.
- **Connection accounting on `/stats`, cleared by `/stats/reset`.** `ConnContext` stamps an id on
  each accepted connection and the chat handler attributes its request to it, so the stub can report
  *requests served*, *distinct connections that carried them*, *max requests on one connection*, and
  *peak concurrent in-flight requests*. Probe connections are counted separately (as
  `connectionsAccepted`) and deliberately kept out of the chat-connection count: the kubelet opens a
  fresh connection per probe, which would inflate exactly the number under test.

The stub is also tagged `benchharness:evidence`, not `:latest`, and that is load-bearing. Kubernetes
defaults `imagePullPolicy` to `Always` for `:latest`, and the usual fix — patching the Deployment to
`IfNotPresent` — does not work here, because the InferenceDeployment controller replaces the whole
container on every reconcile and reverts the patch. The first attempt at this run died in exactly
that loop (`Waiting for deployment "llama-3-8b" rollout to finish` until the timeout). Choosing a
non-`latest` tag makes the default policy `IfNotPresent` and avoids the argument.

## Procedure

```bash
./hack/m5b-gateway-path.sh
# evidence is written to hack/m5b-gateway-path-evidence.log
# the cluster is deleted by the script when it finishes
```

Unlike [`m6-kind-e2e.sh`](m6-kind-e2e.md), this script **tears the cluster down**. Every number it
reports is read out of the cluster while the load is still running and written to the log, so a
surviving cluster adds no evidence and only invites the next run to reuse it — which is precisely
what invalidated an earlier evidence document here (`docs/10_WHAT_I_GOT_WRONG.md`).

It also does **not** call `make install`. That target depends on `manifests`, which runs
`controller-gen` and rewrites the generated API and CRD files; this script applies the committed
`config/crd` instead. It does call `make docker-build`, `make docker-build-gateway` and
`make docker-build-benchharness`, which is the same image-building practice `m6-kind-e2e.sh` uses.

## Observed evidence

All figures below are quoted from `hack/m5b-gateway-path-evidence.log`, captured from a single run on
a freshly created single-node `kind` cluster.

The path was proven live before any load was offered:

```
===== 9. smoke test: one request through the gateway =====
smoke status: 200
```

### Measurement 1 — connection reuse, in the gateway and in the harness itself

The same 884-request trace, replayed five times: twice through the gateway (with the harness's old
client and its new one), and three times straight at the same stub.

#### The gateway's shared Transport

```
gateway -> stub (gateway's own shared Transport): 884 requests over 6 connections
                (max 256 on one connection; 9 accepted in total including kubelet probes)
[EVIDENCE] gateway requests-per-connection: 147.3
  RESULT: PASS - connections (6) are fewer than requests (884), so the shared Transport pooled.
```

**6 connections for 884 requests.** A `Transport` built per request pools nothing, so it would have
shown roughly 884 connections; the observed number is two orders of magnitude below that, and one
connection alone carried 256 requests. The fix holds under load.

#### The harness's own client — the same defect, now fixed and measured

The first version of this run reported, as a finding, that `HTTPSender` built its `http.Client` with
**no `Transport`** and so inherited `http.DefaultTransport`'s `MaxIdleConnsPerHost` of 2. That is the
same defect class the gateway fix removed, sitting in the measurement instrument rather than in the
system under test — so every M5-b latency number would have carried the handshake cost. It has now
been fixed, and because it changes the instrument it was fixed here, against a stub, where the
before and after are measurable and no experimental result depends on it.

`replay --conn-mode` names whole generations of the client so the change can be measured rather than
asserted: `legacy` reproduces exactly what was there, `pooled` is what a real run now uses, and
`drain-only` sits between them so the two changes can be attributed separately.

```
harness -> stub, BEFORE     (legacy: DefaultTransport, no drain): 884 requests over 431 connections
harness -> stub, drain only (DefaultTransport + drain):           884 requests over  53 connections
harness -> stub, AFTER      (sized pool + drain):                 884 requests over   6 connections
[EVIDENCE] harness requests-per-connection: before 2.0, drain-only 16.6, after 147.3
[EVIDENCE] connections the instrument no longer opens: 425 of 431
[EVIDENCE] attribution: the drain accounts for 378 of that, the pool size for 47
  RESULT: PASS - the sized client pool cut the instrument's own connections from 431 to 6.
```

**431 connections down to 6, a factor of 72.** The `before` figure is worth reading closely: 884
requests over 431 connections is **2.0 requests per connection**, Go's default cap reproduced to the
decimal. The instrument was opening a new TCP connection for every other request it timed.

Two changes were needed, and the middle arm is what separates them:

- **Draining the unread stream tail** (`drainForReuse`) accounts for 378 of the 425 connections no
  longer opened. `readStream` breaks out at `data: [DONE]` without consuming the terminator, which is
  right for measurement — the last token is what TTFT is about — but `http.Transport` returns a
  connection to the idle pool *only* once its body has reached EOF. Closing early marks the
  connection unusable. Without this, a sized pool has nothing to pool.
- **Sizing the pool** accounts for the remaining 47. It is derived per run rather than being a
  constant: dispatch is open-loop, so in-flight requests accumulate at the trace's arrival rate and
  leave only by completing or timing out, giving a hard ceiling of `rate x timeout` read off the
  trace's own offsets. For this run that was **592** — deliberately *not* a copy of the gateway's
  600, which is what the same formula happens to give for one particular pair of flag values.

The drain runs after the end timestamp is stamped, so it can never be counted as response latency,
and it is bounded at 8KB so a server that keeps writing cannot stall the sender.

### Measurement 2 — `MaxIdleConnsPerHost = 600` under real concurrency

```
cap under test: MaxIdleConnsPerHost = 600, derived as -rate 20/s x -timeout-ms 30s (internal/gateway/proxy.go)
harness-default run (rate 20/s, fast backend): peak in-flight 6, peak open connections 8, distinct connections 6
slow-backend run    (rate 40/s, ~2s backend): 791 requests, peak in-flight 97, peak open connections 99, distinct connections 97
[EVIDENCE] highest concurrency observed anywhere in this run: 97 in flight against a cap of 600
  RESULT: the cap of 600 was never approached (peak 97, headroom 6.1x); it bounds nothing that this
          run offered, and no reuse was lost to it.
```

**The measurement does not contradict 600, and it does not confirm it either.** What it establishes:

- At the *exact parameters the constant is derived from* (`-rate 20`, `-timeout-ms 30000`), against a
  backend that answers in milliseconds, peak concurrency to one host was **6**. The cap is ~100x
  above anything that regime demands.
- Pushed deliberately into a slow-backend regime (40/s against a ~2s backend, which is where a real
  vLLM prefill lives), peak concurrency was **97** — still 6.1x under the cap.
- In neither regime did the number of distinct connections exceed peak in-flight (6 and 6; 97 and
  97), which is the signature of a pool that never evicted a connection it could have reused. Had
  600 been too *low*, distinct connections would have run ahead of peak in-flight.

So the comment's own claim — that 600 is a **ceiling** (`20/s x 30s`, the point beyond which the cap
provably cannot be what closed a reusable connection) and explicitly *not* an estimate of typical
load — survives contact with measurement. It is now also known to be roughly 100x above typical load
at the harness's defaults, which the comment previously could only assume. The honest reading is that
600 is over-provisioned by design and observation confirms the over-provisioning, not that 600 was
validated as the right number: reaching it would need a backend latency near the 30s timeout itself.

The constant is left at 600 rather than tuned down to what was observed — the observed figure is a
property of one stub's latency, whereas the ceiling is a property of the harness's flags, and an
over-provisioned cap costs nothing because this setting never opens a connection, it only decides
whether an established one is kept. What has changed is that `maxIdleConnsPerHost` in
`internal/gateway/proxy.go` now carries this observation in its comment, so the next reader can see
it was tested rather than guessed.

### Measurement 3 — metric cardinality on the unresolved-model path

Twelve requests, each naming a different model no `InferenceDeployment` serves:

```
gpuaas_gateway_requests_total series before: 4
  unknown model 1 -> HTTP 404
  ... (12 in total, all 404)
gpuaas_gateway_requests_total series after:  5

--- distinct model label values on gpuaas_gateway_requests_total ---
llama-3-8b
llama-3-8b-slow
_unresolved

  RESULT: PASS - 12 distinct unknown model names added 1 series, not 12.

--- gateway series with the unresolved sentinel ---
gpuaas_gateway_requests_total{code="404",model="_unresolved",tenant="premium-1"} 12
```

Twelve distinct names produced **one** new series, and that one is the `_unresolved` sentinel. The
requested names are not lost — they are in the gateway's log lines (`no backend for model ...
"model": "ghost-model-7-..."`), where a distinct value costs nothing — they are simply kept out of
the label set.

### Measurement 4 — latency: the gateway hop, and what fixing the instrument was worth

Same trace (checksum-pinned in one manifest), same NodePort transport, differing in exactly one
thing at a time.

```
[EVIDENCE] premium TTFT ms   gateway, client legacy:     p50=5.8 p95=6.1 p99=6.4
[EVIDENCE] premium TTFT ms   gateway, client pooled:     p50=5.7 p95=6.0 p99=6.7
[EVIDENCE] premium TTFT ms   direct,  client legacy:     p50=5.5 p95=5.8 p99=5.9
[EVIDENCE] premium TTFT ms   direct,  client drain-only: p50=5.4 p95=5.6 p99=5.8
[EVIDENCE] premium TTFT ms   direct,  client pooled:     p50=5.4 p95=5.5 p99=5.7
[EVIDENCE] instrument cost removed  gateway p50=.1 p95=.1 p99=-.3 ms
[EVIDENCE] instrument cost removed  direct  p50=.1 p95=.3 p99=.2 ms
```

**The p99 did not move materially, and that is itself the finding.** Fixing the instrument removed
425 of 431 TCP handshakes, and the latency it reports barely noticed: +0.2ms at p99 on the direct
path, −0.3ms on the gateway path. The direct path's three arms do fall in the right order at every
percentile (5.9 → 5.8 → 5.7 at p99), which is consistent with a small real improvement, but the
magnitude is below what one repetition of ~440 premium requests can resolve.

The reason is specific to this environment and does not generalise: **every hop here is on one
host.** The harness, the kind node, the gateway Pod and the stub Pod share a loopback interface, so a
TCP handshake costs tens of microseconds — it is real, and it is invisible against a 5.5ms response.
On a deployment where the harness runs outside the cluster, a handshake is a full network round trip
per affected request, and at 2.0 requests per connection that would land on half of them. The fix is
justified by the connection count, which is an exact count; it is *not* justified by a latency
improvement this run can demonstrate, and the write-up does not claim one.

#### The gateway hop, measured with the fixed instrument

```
[EVIDENCE] premium TTFT ms   gateway: p50=5.7 p95=6.0 p99=6.7
[EVIDENCE] premium TTFT ms   direct : p50=5.4 p95=5.5 p99=5.7
[EVIDENCE] gateway hop cost (both arms, pooled client)  p50=.3 ms  p95=.5 ms  p99=1.0 ms
[EVIDENCE] M5-b's absolute-protection margin is 25% of baseline p99 = 1.42 ms;
           the gateway hop costs 1.0 ms of that
```

The gateway adds **0.3ms at p50** and **1.0ms at p99** on this run. Read that upper figure with
care: the same script measured a p99 hop cost of 1.0ms here and, on the run committed before this
one, a *negative* p99 hop cost — the gateway path timing faster than the strictly shorter direct
path, which cannot be true and is simply what noise at this scale looks like. The p50 and p95 figures
(0.3ms, 0.5ms) are the stable ones and they are the ones to carry forward.

Whether that is small enough is a question about the effect M5-b intends to measure, so the script
answers it against M5-b's own pre-registered threshold: the absolute-protection check calls a 25%
rise in premium TTFT p99 the boundary of acceptable, which on this baseline is 1.42ms. On a 5.7ms
baseline the hop is not negligible against that margin — but the paid run's baseline will not be
5.7ms. A real prefill is hundreds of milliseconds to seconds, against which a fixed sub-millisecond
hop is noise. The finding to carry forward is that the hop is **fixed and small in absolute terms**,
so it shrinks as a fraction of the signal as the backend gets slower — the opposite of the
per-request-Transport artifact, which grew with load.

For provenance the script also runs the pre-existing dry run and captures its report. **Those
numbers are not the baseline for the gateway hop and must not be read as one**: the dry run replays a
different trace (rate 40/s for 3s, four arms) over loopback to an in-process stub, so it differs from
the runs above in arrival rate, duration, prompt mix and transport all at once. Step 12's no-gateway
replay exists precisely because a comparison needs to differ in one thing. The dry run's own verdict
line has also flipped between executions with no relevant code change, which is the same noise floor
described above showing up on 123-request arms.

## What this does NOT demonstrate

This is the section that keeps the evidence honest.

- **No GPU, and no model.** The backend is a stub that sleeps and emits fixed tokens. Nothing here
  says anything about prefill cost, KV-cache behaviour, batching, or how a real vLLM's latency
  distribution is shaped. Every latency number is a property of a sleep.
- **Nothing about the noisy-neighbour effect M5-b measures.** Both tenants ran with
  `--admission-mode off` (the default) and no `rateLimit`, against a stub that does not contend for
  anything. The 40,000-character noisy prompts cost the stub nothing, so there is no contention to
  protect anyone from. This run validates the *instrument*, not the experiment.
- **`GetBody` (fix 3) has no load evidence, and this run does not give it any.** Its reach is one
  specific case: `http.Transport` picks a pooled connection the upstream has already closed, and the
  write fails having sent nothing, at which point Go retries on a fresh connection **only if the body
  can be rewound**. Nothing in this path ever closes an idle connection from the backend side — the
  stub sets no idle timeout, `http.Server`'s own `IdleTimeout` is unset, and every arm is far shorter
  than the gateway's 90s `IdleConnTimeout` — so the branch never ran. It is worth being precise about
  what that means: the pooling fixes above make this branch *more* reachable in production, not less,
  because a pool that keeps connections warm for 90 seconds is a pool that will eventually offer the
  proxy a connection the backend has since dropped. The fix remains verified by unit tests only.

  **What would exercise it**, named here and deliberately not built: give `stub-serve` an idle-timeout
  option (`http.Server.IdleTimeout`, reachable through the same `stub://` profile as the rest), set it
  well below the gateway's 90s `IdleConnTimeout`, and drive load in two bursts separated by a gap
  longer than that timeout. The gateway would then reach into its pool for a connection the stub had
  already closed, and the run would show whether the request is retried (`GetBody` working) or
  surfaces to the client as a 502 (`GetBody` absent or ineffective). That is a self-contained change
  to one file plus one extra load phase; it is out of scope here because it belongs with a decision
  about how much this branch is worth, not with an evidence run that was asked for four other things.
- **The 600 cap was never reached**, so the code path where the cap actually evicts a connection is
  still unexercised. See measurement 2.
- **One replica, one backend, one node.** The gateway runs at `replicas: 1` (forced by in-process
  token buckets), the cluster is single-node so no cross-node network hop is involved, and all
  traffic in a run targets one Service. Nothing about multi-backend pool behaviour, cross-node
  latency, or a horizontally scaled gateway is shown.
- **The load is light.** Peak 6 in-flight at the harness's defaults. The gateway's manifest caps it
  at 500m CPU; the run never came close to needing it, so this says nothing about the gateway's
  behaviour near its own resource limit.
- **NodePort, not the deployed access path.** `config/gateway/service.yaml` is ClusterIP and
  deliberately excludes the metrics port, because `/metrics` carries per-tenant usage. The script
  creates additional NodePort Services (including one for `:8081`) for the lifetime of a throwaway
  cluster, so the transport measured is not byte-for-byte the deployed one and the metrics exposure
  is an evidence-harness affordance, not a change to the deployed topology.
- **Single run, no repetitions — and the latency numbers are at the noise floor.** Every figure above
  is one observation. The connection counts are exact counts and can be read literally; the latency
  figures cannot. The p99 hop cost came out *negative* on one execution of this same script — the
  gateway path timing faster than the strictly shorter direct path — which is impossible and is the
  clearest available statement of how much resolution one repetition of ~440 premium requests
  actually has. Anything under about a millisecond in these tables should be treated as unresolved.
  Establishing a real interval needs repetitions and a bootstrap, which this script does not do.
- **The instrument fix is justified by connection count, not by measured latency.** Removing 425 of
  431 handshakes changed the reported p99 by less than the noise floor, because every hop in this
  run is loopback. See measurement 4.

## Incidental findings

- **In-cluster DNS probably costs extra lookups per new connection — expected, not measured.**
  `backendFor` builds `http://<name>.<ns>.svc:<port>`, which has three dots, and with Kubernetes'
  default `ndots:5` a name under that threshold is tried against the pod's search domains before
  being tried as written; `llama-3-8b.serving.svc` would then only resolve via
  `...svc.cluster.local`. **This run did not measure DNS**, so it is reasoning from the resolver
  configuration rather than an observation, and it is recorded as a thing to check rather than a
  finding. If it holds, connection reuse amortises it almost entirely — 6 connections for 1210
  requests means it would have happened 6 times — which would be a second reason the pooling fix
  matters beyond the TCP handshake it obviously saves.
- **The harness's own client pooled two connections per host** (measurement 1). Found by this run,
  reported in its first version, and fixed in its second — with the before/after in the same log, so
  the fix is measured rather than asserted. The larger half of that fix turned out not to be the pool
  at all but draining the unread stream tail, without which `http.Transport` refuses to reuse the
  connection no matter how large the pool is.
