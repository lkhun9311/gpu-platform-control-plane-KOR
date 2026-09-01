# What will refuse the paid queuelab session, and what has been closed

This harness refuses rather than improvises, which is the right design and means the failure mode of a
rented card is a session that produces `refused` instead of a number. Everything that can refuse is
enumerated below. The register exists because **none of the device-evidence path has ever run against a
real device plugin**: every recorded run used the fake one and produced no DCGM observation at all.

Counts: 16 refusals in `internal/queuelab/device.go`, 5 in `cmd/queuelabrun/qualify.go`, 16 in
`cmd/queuelabrun/device_preflight.go`.

## 1. A structural defect, found by this audit and FIXED

**The busyness gate was judged over the device hold, and in the honouring arm the victim is exiting.**

`EstablishesDeviceWork` took one interval and asked every clause over it. The hold -- owner `Admitted` to
victim `AttemptStopped` -- is the tail of the attempt during which the victim is *being terminated*, and the
gate wanted two distinct instants showing the card working inside it.

In the arm that honours SIGTERM those requirements contradicted each other:

The holds below are read off the committed records, not restated from another page. Re-derive them with:

    queuelabrun -compare 'ex/e17-*-e17[gs]*.json' -mode model

| | recorded hold | victim during the hold | samples inside at 1 s scrape |
|---|---|---|---|
| A-honor | 2.171 – 3.210 s | has stopped computing | 2–3, reading idle |
| A-ignore | 31.192 – 31.233 s | still computing | ~31, reading busy |

`-require-device` did not fail evenly. It refused the short arm and kept the long one, **leaving no contrast
to form** — and the contrast is the result.

**The fix** is to ask the two questions over the two intervals they were always about. `DeviceClaim` now
carries both:

- **exclusivity, coverage, continuity, single-device** read the **hold**: was this card exclusively that
  Pod's, watched without a blink, while the owner waited for it;
- **busyness** reads the **victim's own attempt** (`VictimWorkWindow`: its Pod becoming Ready to it
  stopping): did this Pod use the card at all.

Coverage is deliberately not required across the attempt — the claim is "busy at two separate instants",
which partial observation supports, and demanding continuity there would refuse an observer that started
after the Pod did.

Six mutations hold it: putting busyness back on the hold, moving exclusivity onto the attempt, accepting an
empty attempt window, counting busyness on a card the hold never identified, and both directions of the
scope boundary. The last two were added because the first attempt at these tests did not touch the
boundaries and two mutants survived.

**Schema went to 19, with one narrow exception.** A schema-18 record carrying a device observation was
judged by the collapsed question and is refused. One carrying none is byte-identical in meaning under both,
and is accepted — every run this lab has taken ran on a fake device plugin with no observer, and refusing
them would orphan the twelve runs the committed documents quote. That has happened here before: two bumps
once left no build in the tree able to decode the set, and the page quoting it went stale unnoticed. Both
sides of the rule are asserted.

## 2. Closed by evidence, without a GPU

**The DCGM label shape the parser depends on.** Read out of the pinned exporter binary's own template
(`nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04@sha256:3d4e0dfa…`), not from documentation:

    DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="…",pci_bus_id="…",device="nvidia0",modelName="…"[,GPU_I_PROFILE,GPU_I_ID][,Hostname="…"][,<attributes>]} <value>

- `UUID` is rendered from a variable (`{{ $metric.UUID }}`) whose value is the literal `UUID` off MIG. The
  parser reads `labels["UUID"]` and is correct for T4 and A10G, neither of which supports MIG.
- `namespace`, `pod` and `container` arrive through `{{- range $k, $v := $metric.Attributes -}}`, appended
  last and in sorted key order. They exist only with `DCGM_EXPORTER_KUBERNETES=true` and the pod-resources
  socket mounted; `config/dcgm-exporter/daemonset.yaml` sets both, and a contract test holds it there.
- `modelName` carries spaces (`NVIDIA A10G`) and commas on MIG parts. `splitLabels` splits on commas outside
  quotes and already documents that case.
- `pci_bus_id` carries colons and is absent from the package's fixture. Harmless — labels are split on
  commas and the key on the first `=` — but the fixture does not match the template.
- `N/A` values are skipped and counted rather than ending the scrape, which DCGM emits around driver init,
  a GPU reset, or an XID event.

**Not verified:** `nv-hostengine` in this image exposes no fake-GPU mode, so no exposition could be captured
without a card. Everything above is read from the artifact that will run, which is better than
documentation and weaker than a capture.

## 3. Open, and only a session can close them

| # | Refusal | Why the fake plugin cannot answer it |
|---|---|---|
| 1 | The card is never observed working | The PTX FMA loop has never run on real silicon. If it does not move `DCGM_FI_DEV_GPU_UTIL` above 0, every arm refuses. |
| 2 | Allocatable devices ≠ requirement exactly | The plugin advertises what NVML enumerates; `NVIDIA_VISIBLE_DEVICES` is documented in the manifest as probably NOT restricting it. The surplus occupier is the real mechanism and has never held a real device. |
| 3 | Samples name more than one device for one Pod | Cannot occur with a simulator that has no devices. |
| 4 | Exclusivity: a device carries two Pods' labels | The occupier's devices carry ITS label. Distinct devices, so it should not fire — untested. |
| 5 | Observer coverage gaps | Real scrape latency against a real exporter under load is not the simulator's. |

## 4. Ordering that matters

- Qualification counts the surplus occupier's devices out and requires what remains to equal the
  requirement **exactly**. Too few refuses ("the contrast it never produced"); too many refuses ("collapse
  below the floor"). Both are correct and both stop the session.
- `hack/gpu-session.sh` prepares every worker, not just the first, and verifies its route before running the
  study. Neither has run against real hardware either.

## 5. What this register does not cover

The refusals in `device_preflight.go` are the preflight's own, and the preflight exists precisely to hit
them before the study does. Reaching one of those is the system working. The ones above are the ones that
would be reached *during* a paid run.
