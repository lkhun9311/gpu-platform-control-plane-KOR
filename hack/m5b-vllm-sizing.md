# Why the M5-b engine is a 3B model on one A10G

Every number in `config/vllm/deployment.yaml` comes from here, and every number here is labelled
**measured**, **derived**, **vendor-reported**, or **estimated**. The distinction matters because the last
two are the only places this page can be wrong, and the run has a cheap way to check both.

**This page described a T4 until M5-c forced the card.** Two of these engines leave 10 MiB of KV cache each
on a T4 — 284 tokens against a 7,695-token prompt — so the sharing matrix cannot run there
(`internal/bench/sharing.go` refuses the plan). The matrix's exclusive arm is this exact configuration, one
engine with the card to itself, so putting the two milestones on one card class means that arm is measured
once instead of twice. `g5.xlarge` is four vCPU, the same as `g4dn.xlarge`, so the granted quota is
unaffected.

One consequence is not a number: an A10G is **sm_86 and accepts bfloat16**, so `--dtype=half` stops being a
startup condition and becomes a choice. It stays fp16 because the matrix and the flagship must not differ in
dtype, and a contract test now enforces that rather than the vendor limit it used to assert.

## What the card gives you

| | value | source |
|---|---|---|
| A10G memory | 23,028 MiB | **vendor-reported** — confirm with `nvidia-smi` at session start |
| `--gpu-memory-utilization=0.90` budget | 20,725 MiB | derived |
| Qwen2.5-3B-Instruct weights, fp16 | 5,886 MiB | measured — sum of the safetensors sizes on the Hub |
| non-KV overhead (activations, CUDA graphs, allocator) | ~1,400 MiB | **estimated** |
| KV cache left over | ~13,439 MiB | derived from the three above |

## What the KV cache holds

Qwen2.5-3B has 36 layers, 2 key/value heads and a head dimension of 128 (**measured**, read from the model
config). Grouped-query attention is what makes this viable: two KV heads instead of sixteen.

    KV per token = 2 (K and V) x 36 layers x 2 kv_heads x 128 dims x 2 bytes = 36,864 B = 36 KiB

    capacity = 13,439 MiB / 36 KiB ~ 382,000 tokens     (derived, inherits both soft inputs)

A contender prompt is **7,695 tokens** (measured; see `internal/bench/testdata/tokenizer_calibration.json`).
So roughly **50 concurrent contenders fill the cache**, and the guard's 0.85 engage threshold is crossed at
about **42**. That is the number the experiment lives or dies on: the pressure the arms are supposed to
differ under has to be reachable, and it is — though it needs twice the concurrency a T4 would have, which
is the cost of the roomier card.

`--max-num-seqs` is **64**, and moving the card is what changed it. It was 32, which sat comfortably above
the 24 concurrent contenders that filled a T4's cache; on an A10G the cache takes 50, so 32 would have made
the SEQUENCE CAP the thing that stops admission — the block manager would never run out, the KV arm of the
engage condition could never fire, and the guard would have been reacting to a queue depth the manifest
chose. A contract test now derives the floor from the plan instead of leaving it to be noticed.

## Why not 7B

Qwen2.5-7B-Instruct is 15.2 GB of fp16 weights. One fits on an A10G; two do not, and the sharing matrix
needs two. Quantising to fit would put a quantisation confound inside an experiment whose whole claim is
that the difference between arms comes from the admission policy.

## The estimate, and how the run checks it

The ~1,400 MiB overhead is the one thing above that was not measured, and the KV capacity inherits its
error; the card's own reported memory is vendor-published rather than read off the device. vLLM prints the
real answer at startup — the number of GPU blocks it allocated.

**The run must record that line and compare it against 382,000 tokens.** If the real capacity is far lower,
42 concurrent contenders is an overestimate and the guard engages sooner than planned; if far higher, the
trace's arrival rate may never reach the threshold and every arm returns the same result for the boring
reason. Either way it is a five-second check against a number that was written down first, which is the
only form of prediction that counts. `hack/m5b-gpu-session.sh` does it.

## The arrival rate the card can actually take

`cmd/benchharness` defaults to `--rate 20` and `--duration-ms 60000`. Those are calibrated for
`hack/m5b-harness-dryrun.sh`, whose stub backend answers instantly. Against a T4 they are not merely
optimistic, they are impossible, and the arithmetic does not depend on any efficiency assumption:

Half of 20/s is contender traffic, each contender is 7,695 measured prompt tokens, so the offered prefill
load is **76,950 tokens/s**. A forward pass costs about `2 x params` FLOPs per token, so that is

    2 x 3.09e9 x 76,950 = 4.76e14 FLOP/s = 476 TFLOPS

against an A10G's fp16 tensor-core peak of **125 TFLOPS** (vendor-published, not measured). The default
demands **3.8x the card's theoretical maximum**, before decode and before any overhead. It was 7.3x on a
T4; a roomier card does not make an impossible rate possible. What would actually happen is an unbounded queue, every
premium request eventually hitting the 30-second timeout, more than 1% censored — and `EvaluateChecks`
disqualifying every arm. The run would cost a full GPU session and certify nothing.

The ceiling, and a workable setting:

| assumption | prefill tokens/s | contender/s | total rate |
|---|---|---|---|
| 100% of peak (unreachable) | 20,227 | 2.63 | 5.26/s |
| 45% MFU (realistic) | 9,102 | 1.18 | **2.37/s** |

At 1.18 contenders/s and equal tenant weights, premium also arrives at ~1.18/s, so reaching
`MinTailSamples` — the 100 premium completions below which a nearest-rank p99 is just the slowest request —
takes under two minutes, and a defensible **500 completions takes about 7 minutes per arm**: roughly
**28 minutes for four arms at one repetition**, doubled for the two repetitions the block bootstrap needs.
Budget about an hour of card time for the confirmatory run, plus model load and warmup — half what the T4
would have taken, which offsets most of the higher hourly rate.

The 45% figure is the one estimate here. Check it in the session's first minute: a single contender request
against an otherwise idle engine has a TTFT that *is* the prefill time, and 7,695 / TTFT gives the real
prefill rate. Set `--rate` from that measurement, not from this table.

## What the trace does not test

Every contender estimates at 10,000 tokens and every premium request at 50, against a guard threshold of
4,096. Nothing in the trace lands anywhere near it, so **any threshold between 51 and 10,000 produces
identical behaviour in every arm**. The experiment measures the engage/release policy and the tier split;
it does not measure the threshold, and the false-positive band recorded in
`internal/bench/testdata/tokenizer_calibration.json` (a request scored 4,096 measures 3,171 real tokens) is
never exercised. Testing the threshold would need a third tenant sending prompts near it, which is a design
change rather than a parameter change.
