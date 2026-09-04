# Which layer holds the fix — a two-request microtest

Date: 2026-09-04 · Pre-registered **before** the run. Nothing here may be edited afterwards.

## The one question

M5-b's guard failed, and the reason is now understood well enough to state:

- Premium TTFT is a staircase in the number of long prefills in flight when the request arrives: 53.8 ms at
  zero, then roughly 944 ms per additional one, straight from zero to seven.
- The engine was already chunking those prefills. vLLM 0.27.1 resolves `max_num_batched_tokens` to 2,048 on
  this device, so a 7,695-token prompt runs as about four chunks.
- A premium request behind a long one still waits for all of them, because the scheduler serves the waiting
  queue head first. Chunking splits the work; it does not let a later arrival overtake.
- The guard watched KV occupancy with an engage threshold of 0.85, which needs roughly 42 concurrent
  requests to reach. Damage starts at one. 84 percent of its refusals landed while the engine was idle.

All of that came from source and arithmetic over evidence already paid for. **None of it was measured on a
running engine**, and the remaining question decides which layer a successor milestone belongs in:

> Can the engine be made to protect a waiting premium request by configuration alone — scheduling policy or
> batch budget — or must something above it limit how many long prefills are in flight at once?

## Why it is worth buying

Not to choose between remedies; source reading already favours one. It buys three things.

**It decides the layer.** If a scheduling policy fixes this, the gateway's admission control is not the
answer to this problem and M5-b's control-plane contribution does not stand. If it does not, the gateway
limiting concurrent long prefills is a real contribution. That changes what gets built next.

**It turns an inference into a measurement.** The current conclusion is source reading plus observational
data. This project's repeated lesson is that those two are not the same as running it.

**It records the engine's resolved configuration**, which no paid run has ever done.

## Design

One GPU node, one engine, no gateway, no arms. Two requests per cell:

1. Send one 40,000-char prompt. Wait until the engine reports it as running.
2. Send one 200-char premium prompt. Record its TTFT.

Six cells: `max_num_batched_tokens` in {512, 2048, 8192} × scheduling policy in {fcfs, priority}. Ten
repetitions each, which at two requests apiece is a few minutes of card time.

Captured every cell: the engine's startup configuration lines, both requests' full timings, and the engine's
own `prompt_tokens` for each.

## Pre-registered readings

The premium TTFT in step 2, against the same engine's uncontended TTFT measured first in each cell.

### A — the batch budget is the lever

Premium TTFT at 512 is at or below **2×** uncontended while at 8192 it is above **5×**.

Then chunk size governs, my original reading was right, and the fix is one engine flag. **M5-b closes as a
negative result**: the control plane was the wrong layer for this problem.

### B — the scheduling policy is the lever

Premium TTFT is above 5× uncontended at every budget under `fcfs`, and at or below 2× under `priority`.

Then the engine can protect the tail but only when told to prioritise, and the open question becomes whether
priority preempts a *running* prefill or merely reorders the waiting queue. **A successor milestone is
registered for that**, and it is still a data-plane finding.

### C — neither is the lever

Premium TTFT stays above 5× uncontended in all six cells.

Then the engine cannot protect a waiting request by configuration, and something above it must limit
concurrent long prefills. **The control-plane contribution stands**, with a new hypothesis: admission on
in-flight long-prefill count rather than on observed occupancy.

### D — inconclusive

The uncontended baseline in a cell exceeds 150 ms, or the startup lines do not report a resolved batch
budget, or the two requests do not overlap. The cell is void and re-run once. If it voids twice, the
microtest is abandoned and the layer question is answered from source alone, stated as such.

## The condition this run has to meet

**Every reading above names a different next piece of work.** If that stops being true — if two readings
would lead to the same build — this experiment is instrumentation rather than an experiment, and it is
cancelled rather than run.

## No retuning

Whichever reading fires, no threshold, policy, or budget is adjusted and re-run inside this microtest. The
reading is recorded and the next milestone is registered separately.
