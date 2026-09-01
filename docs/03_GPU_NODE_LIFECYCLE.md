# GPU Node Lifecycle

> **Status (2026-08-07): built** — but the CR is hand-created (no automated intake pipeline creates it),
> and there is **no GPU-specific fault detection**: no DCGM, Xid, or ECC anywhere in the Go tree (verified:
> zero occurrences). The reconciler observes the Kubernetes Node `Ready` condition and applies/removes a
> taint on degradation, with finalizer cleanup. The richer intake pipeline, conditions, and metrics
> described below (DCGM, fio, iperf3, NCCL) are target design only — not implemented. No GPU in this project
> is real.

A GPU node is treated not as "a server" but as an asset with an intake, operating, isolation, and recovery lifecycle, tracked by `NodeHealth`.

## Lifecycle

```
Register -> Intake -> Certify -> Serve -> Monitor -> Degrade -> Cordon -> Drain -> Re-intake -> Recover
```

- **Register / Intake / Certify** (target pipeline — not yet implemented; see "Implemented today" below): a new node is validated before it serves (driver, DCGM, storage, network, NCCL baseline) and signed off as `Ready`.
- **Serve / Monitor**: healthy node carries traffic; the controller observes readiness.
- **Degrade -> Cordon**: an unhealthy node is tainted/cordoned to stop new GPU scheduling (implemented in the NodeHealth reconciler — readiness-driven, taint `platform.lkhun9311.github.io/unhealthy=true:NoSchedule`).
- **Drain**: **not fully automated**. Draining running workloads is operator-approval / dry-run by design — the platform decides *what* to drain and surfaces it, but a human (or an explicit approval flag) triggers eviction. Maturity here is "knows the operational risk," not "auto-drains."
- **Re-intake / Recover**: a recovered node re-runs intake before returning to `Serve`.

## NodeHealth CRD

```yaml
apiVersion: platform.lkhun9311.github.io/v1
kind: NodeHealth
metadata:
  name: gpu-node-01
spec:
  nodeName: ip-10-0-1-10
  gpuClass: a10g
  intakeProfile: standard-gpu-node
status:
  phase: Ready                       # Pending | Intake | Ready | Degraded | Quarantine
  conditions:
    - { type: DriverReady, status: "True" }
    - { type: DCGMHealthy, status: "True" }
    - { type: StorageBenchmarkPassed, status: "True" }
    - { type: NetworkBenchmarkPassed, status: "True" }
    - { type: NCCLBaselinePassed, status: "True" }
  metrics: { xidCount24h: 0, eccDbeTotal: 0, gpuTempC: 61, gpuMemoryTotalMiB: 24576 }
  lastIntakeReport: reports/intake/gpu-node-01.md
```

> Implemented today: `spec.nodeName` + `gpuClass`; status `phase / observedGeneration / conditions(Ready) / faultSignal`; reconciler observes node readiness and applies/removes the unhealthy taint with finalizer cleanup. The richer intake conditions and metrics above are the target; DCGM/fio/iperf3/NCCL require a real GPU node.

## Intake runbook (target)

| Stage | Check             | Tools                             | Output                |
|-------|-------------------|-----------------------------------|-----------------------|
| 0     | pre-flight        | nvidia-smi, runtime, GPU Operator | basic state           |
| 1     | GPU health        | DCGM, gpu-burn                    | XID / ECC / temp      |
| 2     | storage / network | fio, iperf3                       | IO / network baseline |
| 3     | NCCL              | all-reduce single-node 2-GPU      | training-infra signal |
| 4     | sign-off          | NodeHealth update                 | Ready / Quarantine    |

When DCGM is unavailable (local/simulated), the controller falls back to the Kubernetes node Ready condition and records that the GPU-level checks were not run — it does not fake DCGM data.

## Scope at this milestone

Implemented: Ready/Degraded/Quarantine, taint apply/remove, finalizer cleanup, Node watch. Target: intake conditions + metrics, re-intake script, operator-approval drain. Stretch: an automated drain controller (deliberately deferred — operational risk).
