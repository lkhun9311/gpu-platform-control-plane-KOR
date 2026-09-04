# Which layer holds the fix — what the microtest actually showed

Date: 2026-09-04 · Result of the test pre-registered in `2026-09-04-scheduler-microtest.md`.
Cost: about $0.40, 37 minutes, one Spot g5.xlarge, no NAT data charge.
Raw results: `data/2026-09-04-scheduler-microtest-results.json`.

## No reading fired, and that is the first finding

| batch budget | policy | uncontended (ms) | contended median (ms) | contended range | long TTFT (ms) | ratio |
| ---: | --- | ---: | ---: | ---: | ---: | ---: |
| 512 | fcfs | 48.7 | 671.9 | 668.1–672.8 | 685 | 13.79x |
| 512 | priority | 48.5 | 76.0 | 73.2–76.8 | 684 | 1.57x |
| 2048 | fcfs | 48.8 | 609.4 | 606.6–611.1 | 623 | 12.48x |
| 2048 | priority | 48.6 | 235.5 | 232.4–235.9 | 628 | 4.85x |
| 8192 | fcfs | 48.7 | 575.3 | 573.3–595.9 | 588 | 11.81x |
| 8192 | priority | 48.6 | 575.3 | 572.4–575.9 | 588 | 11.83x |

Ten repetitions per cell. In all sixty the engine reported the long request running before the short one was
sent, so every measurement is of an actual overlap rather than a race the client lost.

Reading B required "above 5x under `fcfs` at every budget, **and at or below 2x under `priority`**". Under
`priority` the ratios are 1.57x, **4.85x and 11.83x**: one budget of three clears 2x, so B did not fire.
Reading A required 512 at or below 2x without naming a policy, and 512 is 13.79x under `fcfs` and 1.57x under
`priority`, so A is not evaluable as written. Reading C required above 5x in all six cells, and one cell is
1.57x. D does not apply — every baseline is about 48 ms and every cell overlapped ten times out of ten.

**The two factors interact, and the readings were written as though they were independent.** That is a defect
in the pre-registration rather than a result to be selected from it, and picking the nearest reading would be
exactly the post-hoc choosing the pre-registration exists to prevent. It is recorded as not covered.

## What the six cells do support

**Chunk size alone changes nothing.** Under `fcfs` the ratio is 12–14x at every budget, including 512, where
a 7,695-token prompt is cut into about fifteen pieces. Splitting the work does not let a later arrival
overtake it: the scheduler serves the waiting queue in order. That is the mechanism two cold reviews read out
of the source, and it is now measured.

It also closes my own earlier hypothesis, which had the prompt running as one indivisible step because the
default budget exceeded it. The engine's startup line settles that:

```
Chunked prefill is enabled with max_num_batched_tokens=512.
```

**Priority alone changes nothing either.** At 8192 the ratio is 11.83x with priority against 11.81x without.
A 7,695-token prompt fits inside an 8192-token budget, occupies one whole step, and leaves no boundary for a
higher-priority request to enter at.

**One cell of six cleared the band, and the pattern between them is not explained.** 512 with priority gives
1.57x; 2048 with priority gives 4.85x. Both have chunking and both have priority, so "the two together" is
too simple a story for these numbers. What is established is narrower: **on this model, this GPU and this
prompt size, only a 512-token budget with priority scheduling held the short request inside 2x.** Why the
relationship with budget size is not monotone in the way the mechanism suggests is unexplained here.

**No cost to the long request was detected in its own TTFT** — 685 ms under `fcfs` against 684 ms under
`priority` at a budget of 512. That is the only quantity measured for it. Completion time, output-token
latency and throughput were not, so the claim is that ten repetitions detected no TTFT cost, not that there
is none. Its prefill is 7,695 tokens against the short request's 68, so a cost near the noise is what the
arithmetic predicts.

## What follows, and what does not

**The layer below has a lever on this failure.** One engine configuration held a short request behind a long
one to 1.57x its uncontended time with no gateway involved. A successor milestone therefore has to establish
what a gateway adds ON TOP of a correctly configured engine, rather than instead of one.

**That is not the same as saying M5-b's control-plane contribution does not stand.** This measured one long
request and one short one, at a median over ten samples. M5-b's load is an arrival process with many
concurrent prefills, queue build-up, output generation, and a p99 rather than a median. A two-request test
cannot retire a claim about that.

The 2026-09-03 guard remains badly aimed on its own evidence: 84 percent of its refusals fired while no long
prefill was running. Not while the engine was idle — by that stricter reading it is 37.6 percent, since a
request occupies the engine until it completes rather than until its first token.

M5-b's performance hypothesis is still falsified on the earlier evidence, independent of today:

- Reactive KV-occupancy shedding did not protect premium TTFT p99 at this load: 83.7x against a
  pre-registered 1.25x, reproduced across four repetitions with an isolation control at 1.0x.
- The tail rises with the number of long prefills in flight when the request arrives. The increments are not
  constant — roughly 552, 896, 934, 867, 1194, 1305 and 858 ms from zero to seven — so it is a rising
  staircase rather than a fixed step, with a linear slope near 977 ms per concurrent prefill.

## Deliberately not concluded

Whether 512 is the right budget, why 2048 with priority lands in the middle, and whether priority preempts a
running prefill or only reorders the waiting queue. The no-retuning rule holds: these need a new
pre-registration rather than adjustments to this one.

## Limits that bound all of the above

Three baseline samples per cell and ten contended ones, so these are medians and not tails. Cells ran in a
fixed order with one engine restart each, on one instance, so a systematic drift across the sequence is not
separable from the factor being varied. Priority was set per request rather than derived from a tenant
policy. Both requests asked for four output tokens, so nothing here observes decode-phase interference. And
the claim that chunked prefill is always on in this version comes from reading the release, not from this
run, which shows only that it was on here.
