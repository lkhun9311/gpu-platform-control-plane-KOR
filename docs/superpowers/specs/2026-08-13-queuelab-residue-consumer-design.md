# Residue consumer — tell the next run's operator why the worker is held

**Status:** design approved, not yet implemented.
**Lineage:** closes the first item under "What this plan still leaves open" in
`docs/superpowers/plans/2026-08-12-queuelab-teardown-executor.md` — "The next run must refuse to start on a
residue record. This plan writes it; nothing reads it yet."

## The problem, stated precisely

The teardown executor now writes a residue record. Nothing reads it: `decodeRunRecord` has no production
caller, and the record goes to a per-invocation file (`queuelabrun-record-<ts>-<pid>.json`) whose name the
next run cannot predict.

But the plan's phrasing — "the next run must refuse to start" — turned out to overstate what is missing.
**The refusal already happens.** A run that leaves residue keeps the worker held (`residueHoldsWorker`), so
its ownership label, taint and journal all stay installed, and the next run against that worker is refused
by `decideAcquire` with `reasonForeignOwner`:

```
node %s is held by run %q under tx %q since %q
```

What that message does not say is **why** the owner is still there. The operator cannot tell a run that is
legitimately in flight from one that finished, failed to remove its namespace, and left GPU Pods behind —
and those two need opposite responses.

## Scope

**In:** carry the reason to the human reading the refusal.

**Explicitly out**, each declined during design:

- A distinct disposition or exit code for "refused because of residue". **Nothing changes about what a run
  decides** — the refusal is already correct and stays exactly as decisive as it is. (A new Node annotation
  is written, which is new behaviour in the literal sense; what is out of scope is any change to an
  outcome, a disposition, an exit code, or whether a run proceeds.)
- Closing the foreign-only case (residue consisting solely of objects this run did not create, where the
  worker is deliberately released and nothing refuses). Nothing is blocked there by design.
- Closing the same-run-id-on-a-different-worker case, where worker-based refusal never engages.
- A cluster-wide "is anything uncleaned here?" query. That would need a List, which the teardown design
  forbids outright.

## The artifact

A new Node annotation, `queuelab.gpu-platform/residue`, carrying a schema'd JSON document. It follows the
shape `queuelab.gpu-platform/quarantine` already established: its own key, its own schema number, its own
decoder, surfaced through `observe`.

```
{
  "schema":     1,
  "txID":       "<the transaction that left it>",
  "runID":      "<that run's id>",
  "leftAt":     "<RFC3339>",
  "recordPath": "<path the run wrote its record to>",
  "left": [ { "kind": "...", "name": "...", "absence": "present|unknown|foreign" }, ... ]
}
```

It carries **both** a compact summary and the record path, deliberately. The path alone would be useless
whenever the next run happens on a different machine or working directory — which is precisely when a human
is most lost — so the message must stand on its own; the path is an invitation to read more, not the
payload.

**This annotation contains nothing.** The label and the taint are what keep Pods off the node; this
annotation only explains. The distinction is the trap `forceQuarantine`'s comment already documents — an
annotation the scheduler does not understand cannot contain a residual GPU Pod — and the code must say so
where the annotation is written, so no later reader mistakes it for a containment mechanism.

## Lifecycle

**Written** on the residue path in `run()`, at the point that decides the worker stays held
(`residueHoldsWorker`, `main.go:484`, beside the `TEARDOWN INCOMPLETE` report). Written **only when the
worker is actually held.** When residue is foreign-only the worker is released on purpose, no refusal will
ever occur, and an annotation nobody quotes is a stale marker waiting to mislead.

The patch carries an optimistic lock like every other Node write here, and re-checks ownership with
`verifyObserved(obs, j)` before writing. Between teardown and this write another actor could have taken the
node over; stamping our residue onto someone else's node would be a lie of exactly the kind the UID
preconditions in `teardown_apply.go` exist to prevent.

An earlier draft of this design named `verifyInstalled` here. That is not enough: it proves the label value,
the taint value and effect, and the node UID — but both marker values are the **run id**, so a node
re-acquired by a *different* transaction under the *same* run id passes it. `verifyObserved` adds the check
that the journal on the node is this transaction's, which is what `decideRelease` already requires before
`releaseAcquired` will undo anything.

**Read** by `observe` (`ownership.go:179`) into a new `ownership.ResidueRaw`, and quoted by `decideAcquire`
(`ownership.go:201`). `decideAcquire` is a pure function of the observation, so the whole reading half is
testable without a cluster.

**Cleared** wherever the ownership markers come off:

- `releaseAcquired` (`ownership_apply.go:623`), beside `delete(n.Annotations, journalKey)` — the ordinary
  release. The same applies at every other site that removes `journalKey`; the implementer greps for them
  rather than working from a list here.
- `clearQuarantine` (`ownership_apply.go:583`) — the operator's deliberate clear, beside
  `delete(n.Annotations, quarantineKey)`.

`releaseStale` **warns before that deletion happens**, naming the record and the objects it lists. It does
not refuse — this mode exists for the case where the previous process is gone and the operator has attested
to it, and refusing would leave them with no move at all. The warning is load-bearing because this is the
command `inspectWorker` tells the operator to run: without it, the whole explanation dies at the one step
most likely to be copied without reading, and the output would say only that a label, a taint and a journal
went away. Deleting the record does not delete the objects, and it says that too.

`forceQuarantine` (`ownership_apply.go:543`) deliberately does **not** clear it, and does not copy it into
the quarantine record either. An earlier draft of this design said to preserve it into that record the way
the journal already is; that would require bumping `quarantineSchema`, and since the record is decoded with
`DisallowUnknownFields`, an older binary would then refuse every new quarantine record. Simply leaving the
annotation in place preserves the explanation for free. It cannot mislead while it sits there: `decideAcquire`
refuses on `QuarantineRaw` before it reaches the foreign-owner branch, so a quarantined node is refused for
being quarantined and the residue note is never reached.

A stale annotation cannot mislead. The refusal is only reached when the node is held, held implies the
journal is present, and the annotation is removed at the same sites as the journal. If someone strips the
label by hand and leaves the annotation, the next run acquires normally and never reads it.

## The message

Only `reasonForeignOwner` changes. With a residue record present it gains what remained and where to read
more; without one it is byte-for-byte what it is today.

Today:

```
node platform-worker is held by run "r7" under tx "c8b2…" since "2026-08-13T01:04:22Z"
```

With a residue record:

```
node platform-worker is held by run "r7" under tx "c8b2…" since "2026-08-13T01:04:22Z";
  that run's teardown left 2 object(s) behind at "2026-08-13T01:07:29Z", so the worker is held
  deliberately and its GPUs may still be in use:
    "Namespace" "queuelab-r7" ("present")
    "ClusterQueue" "ql-reclaim-tenant-a-r7" ("present")
  full record: "queuelabrun-record-20260813T010422Z-31288.json"
  do NOT strip a stuck namespace's finalizer; run: queuelabrun -inspect-worker -worker platform-worker
```

Every field above that came out of the annotation is **quoted**, and that is not cosmetic. Unquoted, a
crafted record can embed terminal control bytes and forge what looks like a legitimate
`queuelabrun -force-release …` line inside this very message — the command that breaks the hold this text
exists to explain. `obs.NodeName` stays unquoted because it is the one value here that was not read out of an
annotation. The quoting makes the message read heavier than it otherwise would; that trade was made
deliberately, and it matches how `inspectWorker` already treats every node-controlled value.

Unreadable record:

```
node platform-worker is held by run "r7" under tx "c8b2…" since "2026-08-13T01:04:22Z";
  it also carries a residue record that could not be read: <err>
```

The exact wording is the implementer's to settle against the surrounding style; what the design fixes is
that all three forms exist, that the middle one names the objects and repeats the finalizer warning the
teardown design forbids omitting, and that the third stays a `reasonForeignOwner` refusal.

**No new reason code.** The reason taxonomy in this file names *what state the node is in* — `decideAcquire`
says of itself that it "proceeds from exactly one state and refuses every other by name" — and the state
here is unchanged: a foreign owner holds the node. Residue is a fact about why that owner is still there,
not a different state. Adding a code would also add a machine-readable classifier, which is outside the
scope agreed above.

**A malformed residue annotation must not block acquisition.** This is deliberately unlike the journal,
where a decode failure is `reasonBadJournal` and refuses. The journal decides who owns a GPU worker and must
refuse anything it does not fully understand; this document is an explanation and carries no safety weight.
On a decode failure the refusal degrades to naming that a residue record exists and could not be read, and
remains a `reasonForeignOwner` refusal. **An informational field must not invent a new failure mode.** The
comment must state this contrast, because the neighbouring code does the opposite for good reasons.

## Failure handling

A failed annotation patch does not fail the run and does not change its outcome. It is written on a path
that is already reporting failure, and the fact that actually matters — the worker is held — is carried by
the label and the taint, which are already installed. Amending the disposition on this patch would misreport
that.

It is reported next to `TEARDOWN INCOMPLETE`, because the real loss is specific and worth naming: the next
operator will not get the explanation.

## Testing

The reading half is pure, so most of this needs no cluster.

| Property | How |
|---|---|
| A foreign-owner refusal names what remained | `decideAcquire` unit test |
| Without residue the message is unchanged | `decideAcquire` unit test |
| A malformed record degrades to a note and does **not** become a new refusal | `decideAcquire` unit test |
| `observe` surfaces the annotation | `observe` unit test |
| The ordinary release clears it | mutation: drop the `delete` in `releaseAcquired`, and a later acquire must quote stale residue |
| `forceQuarantine` leaves it in place, and a quarantined node is still refused for being quarantined | fake-client test + a `decideAcquire` test proving the quarantine branch wins |
| `clearQuarantine` removes it | mutation: drop the `delete`, and a later acquire must quote stale residue |
| A worker-holding residue run writes it | `run()`-level test |
| A foreign-only residue run does **not** write it | `run()`-level test |
| A failed patch changes no outcome but is reported | `run()`-level test with a patch interceptor |

Every test names the mutation that turns it red, per this lineage's standing rule.

## What this still leaves open

- The foreign-only and different-worker cases remain unrefused, by choice.
- Nothing yet reads the residue **record file**; this design reads a Node annotation instead. A batch-level
  consumer that walks records is a separate problem and needs the durable journal the executor plan also
  defers.
- `absence` is persisted by name here as it already is in `record.go`, so the two spellings must agree; the
  `iota`-as-wire-format question the executor plan left open is unchanged by this design.
