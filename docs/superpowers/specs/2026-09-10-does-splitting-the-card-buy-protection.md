# Does splitting the card buy protection — pre-registration

Date: 2026-09-10 · Pre-registered **before** any card time is bought for it. Nothing here may be edited from
the moment its pilot is bought.

## Why this exists

Two layers have been measured and neither protects the premium tail at this load.

**Admission (M5-b).** A KV-occupancy guard missed a 1.25x bar at 83.7x, and re-analysis found the occupancy
limb was unreachable by construction and never fired at all.

**Engine scheduling (the price-of-protection run).** Batch budget crossed with scheduling policy, eight
cells, three repetitions, no timeouts. The best cell held the premium tail at **20.7x** an isolated baseline
against a 2x bar, and its **median** missed by 2.8x. All nine configurations scored two of the four bars and
always the same two: the contending tenant kept its work and the machine kept its throughput, while the tail
and the stream were missed everywhere.

`2026-09-09-what-would-have-to-change.md` derived why, as a ratio rather than a policy. A contending prompt
occupies the engine for about **1.03 s**; the premium tail budget at the 2x bar is **0.135 s**. Chunked
prefill divides that second and priority lets a short request enter at a piece boundary, which is why those
knobs are worth about fivefold — against a gap of twenty. Dividing the work does not divide the machine.

That page named three levers and refused to pick one. This picks the third: **divide the machine.**

## The question

> **Does giving each tenant its own engine on a shared card protect the premium tail, and what does the
> premium tenant pay in capacity for it?**

The mechanism is different from everything measured so far, and that is the point. Under `shared`, a premium
request waits in a queue behind an admitted long prefill. Under `timeSlicing` and `mps` there is no such
queue: each tenant has its own engine with its own cache, and the contention moves from queueing to the SMs.
A premium request should then run *slower* but never *behind*.

**It is not obvious this helps.** Half a card is half a card, and a premium request that runs at half speed
on an uncontended engine may land in the same place as one that queues briefly on a fast one. That is what
makes it worth measuring rather than assuming.

## Corrected on 2026-09-10, before any card time was bought

This page was written on the strength of a sentence that turned out to be false: that the three arms were
**"already built and tested in `hack/m5c-matrix.sh`"**. They were built. Nothing had ever run them, and
`docs/00_PORTFOLIO_OVERVIEW.md` already said so of the whole cluster half of the AWS path — *"offline-validated,
never applied to AWS"*. Reading the runner, rehearsing it on a real cluster, writing the code for its own
readings, and finally running the runner itself on a cluster found **fourteen** defects, each of which would
have ended or silently corrupted a paid session.

**Nine were found before any card was rented. Three more were bought by pilots, for $2.03 in total, and two
were then found by running the matrix on a free cluster.** Nothing in this section reads a result, because
there are none: every entry below is a thing that stopped the instrument or falsified what it recorded.

That
is why editing this page now is legitimate: its freeze clause binds from the moment its pilot is bought, and
the pilot is not bought. Nothing here reads a result, because there are no results.

| # | what was wrong | what it would have cost |
| --- | --- | --- |
| 1 | The split engines did not pass `--no-enable-prefix-caching`; the exclusive one did | vLLM V1 caches by default and this trace repeats one prompt shape, so the split arms would have beaten `shared` by a wide margin **because of a cache**, and this page would have called it separation |
| 2 | Both sharing overlays rendered `namespace: system`, a kubebuilder placeholder nothing creates | every apply refused with `namespaces "system" not found`; **neither sharing arm could ever deploy** |
| 3 | Nothing advertised a device for the `shared` arm | the sharing node deliberately lacks the exclusive plugin's label, so the control's engine either sat Pending to a 900 s timeout or **inherited the previous arm's split card and reported numbers** |
| 4 | The device count was checked with `-ge` | `timeSlicing` → `mps` both want 2, so the check passed on the outgoing arm's stale advertisement and MPS could begin as time-slicing |
| 5 | `config/gateway/rbac.yaml` has the same placeholder namespace, and the matrix binds `gateway-role` without creating it | Kubernetes accepts a binding to a missing ClusterRole, so the gateway starts and **every request fails authorization**, with nothing looking wrong until the first replay |
| 6 | No tenant weights were passed to `gen-trace` | the default mix puts the 40,000-character contender at 45% of arrivals — four to five times an A10G's prefill capacity — and **no choice of `RATE` fixes it**, because lowering it starves the premium tail below the sample floor |
| 7 | No `--model` was passed to `gen-trace` | the default is `llama-3-8b`, the engines serve Qwen2.5-3B, and the gateway routes by model name: **every request `ErrNoRoute`**, after both engines had loaded |
| 8 | This study's readings were not implemented anywhere in `internal/bench` | the readings below would have been evaluated by hand, which is not the pre-registered instrument |
| 9 | **Reading 2 was unreachable.** Its condition — meets both bars *and* starves the contender — was a strict subset of reading 1's, and reading 1 is evaluated first | an arm that bought its tail by refusing the other tenant's work would have been reported as the **deliverable**, and the reading that exists to catch exactly that would never have run |
| 10 | The matrix had **no R1 arm**. `deploy_arm` had cases for `shared` and the sharing pair and none for the isolated baseline | R1 is the denominator of both bars, so the readings declined to evaluate anything. Found by a pilot |
| 11 | The trace sent four tenants and the replay carried keys for **two** | all 41 probe requests refused by the gateway, both probe rows VOID. `hack/lib/spot-run.sh` records this same failure twice before, so this was the third. Found by a pilot |
| 12 | An engine that never became ready said only that | Pending, crash-looping, still pulling, or killed for memory are four faults with four fixes, and the run log held none of it. Found by a pilot |
| 13 | `GPUQuotaPolicy` is cluster-scoped and its `targetNamespace` is **immutable**; the matrix moves the contender between namespaces every arm | the API refuses it on the first sharing arm. **This is the matrix's entire routing mechanism.** Invisible to reading, because the immutability and the mutation are in different files |
| 14 | The port-forward was replaced between cells with a `kill` that does not wait, then slept at for three seconds | the new forward lost the race for the port and died with its output in `/dev/null`. **Two of four arms would have completed nothing**, recorded with no HTTP status, and the report would have called it a censored tail — a plumbing failure wearing a load failure's name |

**Defect 14 is the one to read twice.** The other thirteen stop the run. That one lets it finish and hands
back a table, and the table's own wording — a censored tail — points at the load rather than at the tunnel.
A reader would have re-derived the load and bought another card.

All fourteen are fixed. **Thirteen more followed on 2026-09-11**, from a second independent review and from
a mutation battery run against the scorer's own tests, and they are listed in the section below this one.
Each is pinned by a test that was deliberately broken to confirm it goes red, or by
`hack/test/rehearse-m5c-matrix.sh`, which runs the matrix itself on a kind cluster with stub engines and
simulated devices and asserts that its readings could be *evaluated* over the evidence it wrote — not merely
that they printed. Defects 13 and 14 were found by that rehearsal on its first pass and are not reachable by
reading: in 13 the immutability and the mutation live in different files, and in 14 the symptom only appears
from the second cell onward, which no paid run had ever reached. This is also
defect 9, whose test asserts that an arm meeting both bars with 170 of the contender's 300 requests *rejected*
is reported NEGATIVE and not POSITIVE. Defect 5 is fixed as a refusal that names the file to apply.

Defect 8's fix is `internal/bench/sharing_matrix.go`: all seven readings, evaluated in the registered order,
dispatched from `benchharness report` on a study whose arms are the topologies. That last part removed
something else — the run used to replay every arm as `off` and ship a README saying its evidence must never
be given to `benchharness report`, because pooling would collapse three topologies into one row. Evidence
that arrives with a warning against using the tool that reads it is one step from being read wrong.

**Defect 9 is the one worth dwelling on**, because it was found by writing the code for defect 8 and by
nothing else. Reading the page did not surface it; two readings that overlap look fine in prose and only
collide when something has to decide which fires first. That is the argument for the gate: implementing the
readings is not paperwork ahead of the pilot, it is the last review the design gets.

Seven of the nine were found by reading or by implementing. Two — 5 and the confirmation that the gateway
needs no operator —
came from `hack/test/rehearse-m5c-deploy.sh`, which stands up a kind cluster with stub engines and proves a
request travels key → tenant → `GPUQuotaPolicy` → namespace → `InferenceDeployment` → Service → Pod. It
asserts that each tenant reached **its own** engine rather than that both got HTTP 200, because both
tenants routed to one engine also returns two 200s, and that is the `shared` topology wearing a split arm's
name.

### Thirteen more, on 2026-09-11

Two of these came from running the matrix on a free cluster, three from a mutation battery run against the
scorer's own tests, and the rest from a second independent review. **Most were in code written in the
previous two days, and the unit tests were green throughout** — the fixtures happened to use numbers where
the wrong quantity and the right one move together.

| # | what was wrong | what a reader would have seen |
| --- | --- | --- |
| 15 | Nothing checked that the `mps` arm's engines were MPS **clients** — only that the control daemon had rolled out | the server half of a two-sided arrangement. An arm that fell back IS the time-slicing arm, so the study would have compared two mechanisms having measured one twice, with every number looking ordinary |
| 16 | The session printed `SESSION DONE` naming an evidence directory it never created | the instance's uploader ends in `\|\| true` by design, so a marker can be written while the archive never arrives |
| 17 | Reading 2 measured the contender's **share** of an arm's output; the page asks for its **output** | a control at 60k+40k against an arm at 30k+20k gave the contender half as much work and reported **1.00**, because the share is 40% in both. Reading 1 would have called it the deliverable |
| 18 | Reading 1's price was the arm's **aggregate** throughput over R1's **premium-only** throughput | premium at 10 tok/s plus a contender at 10, against an R1 at 20, reads as **1.00** where premium actually got half. That number goes in the write-up's first sentence |
| 19 | Reading 4c could not fire on anything the runner produced | nothing outside the unit tests populated its refusals map. The one registered outcome meant to identify an MPS failure was unreachable |
| 20 | Any non-zero refusal counted as starvation | one broken stream among 2,879 timeouts printed "refused work, not merely late work" |
| 21 | A censored or thin **R1** was divided by | every bar is a ratio against that baseline, so a lower bound was reported as a measurement |
| 22 | An INVALID sharing run exited **zero** | automation writing `report … \|\| fail` would accept a run the readings had just declared unusable |
| 23 | **The stream bar was pinned by no test**, and closing that left an outcome with no reading at all | an arm holding the tail inside 2x while its stream runs at 10x fires none of 1, 2, 3 or 5. **The identical gap this page was written to close, one bar over** |
| 24 | Reading 2's own gate was equally unpinned | relaxing it to the tail alone would credit an arm with a tail it never delivered |
| 25 | Tokens that arrived on a stream which then **broke** counted as completed output | the guard is on HTTP status and a broken stream keeps 200, so `report.go` was not honouring its own comment. Half the contender's output read as **97%** |
| 26 | The **control's** censoring was never a precondition | R1 was fixed and `shared` was not, and `shared` is what every improvement is measured from |
| 27 | Reading 3 could fire with **no sharing arm present at all** | the guard read `scorable == 0 && len(all) > 0`, so an empty set went past it and the report concluded "splitting the card changes nothing" over evidence in which nothing was split |
| 28 | The winner was chosen by token **volume**, not throughput | `contenderRel` divides by the same control for both candidates, so an arm that took longer to drain wins with a lower rate |
| 29 | The rehearsal claimed "the readings below it were reached" | a fired reading 4 returns immediately. It claimed a verification that had not happened. One scorer test was similarly weak, logging an unexpected answer and matching `INVALID` against reading *names* |
| 30 | The refusal workflow was inconsistent | a failed MPS daemon rollout ended the session recording nothing, while the client check recorded and continued — and the session then demanded evidence from the arm it had refused |
| 31 | The matrix budgeted cells against the **instance's** backstop while the wrapper gives up sooner | a workload fitting the instance and not the wrapper was approved, then cut — and evidence is archived only after the matrix returns, so every completed cell would have left with the instance |
| 32 | The code and this document disagreed about reading 5 | recorded as an amendment above rather than left as a silent divergence |
| 33 | Starvation counted **requests** while the reading is about **output**, and counted transport failures as refusals | a contender backend whose connections fail produces hundreds of those while premium stays healthy. That is broken delivery, not a system withholding service |
| 34 | Nothing compared the commit the instance recorded against the one the session shipped | the run id is the output directory's basename and the bucket keeps objects for thirty days, so a reused name returns **a previous experiment's numbers** under this run's |
| 35 | The credential check could pass on **another profile's** expiry | an active role with 20 minutes left beside another profile's 12 hours read as 12 hours, and the run would lose download and termination partway through |
| 36 | Reading 4b's floor applied to the **pool**, which is what hides a bad block | 3000, 3000 and 50 completions clear a hundred-sample floor at 6,050 while the third block's p99 is its slowest request |
| 39 | The replay began before the gateway was **listening**: `rollout status` returns on a running container and the tunnel check proved only that the local port accepted TCP | **R1 completed 0 of 3,882 requests**, all `errorKind=transport`, and the run had no baseline. An independent review had named this exact gap the day before and it was read and not acted on |
| 41 | `--gpu-memory-utilization` is a fraction of the **whole card**, so the second split engine to start took what the first had left | identical manifests produced **3.33 GiB and 1.52 GiB** of KV. The smaller engine timed out 188 of the contender's 238 requests and reading 4b called the run INVALID — for a load that was not the problem |
| 40 | An engine that could not start ended the **session** rather than refusing its **arm** | the 2026-09-12 pilot stopped at the mps arm, so the three cells already bought were all it had, and the reason for the refusal lived only in a log `benchharness report` does not read |
| 37 | Arms repeated **unequally** were accepted | the confirmatory run is three separate sessions, so an arm that lost one is pooled from two against a spread measured over three |

### Seven more on 2026-09-12, and two of them were fixes that could not work

Found between the fifth pilot's fix and the sixth run. The first was found by reading the repository's own
test suite; 43 to 46 by a cold review of the harness against the question "what would make the next $0.75
produce no answer, or a plausible wrong one"; and 47 by **running the rehearsal and reading its stderr**,
which is the one of the three methods that costs nothing and was nearly skipped.

| # | defect | what it would have cost |
| --- | --- | --- |
| 42 | The test that validates the engines' sizing matched `--gpu-memory-utilization` **anywhere in the file**, and the defect-41 fix left the old value in a comment explaining itself | one match across two engine files, so it validated a **one-engine** plan for a two-engine deployment and compared the plugin's device count against 1 instead of 2 — in green. **My own fix manufactured this silence**, and the sibling test written the same day to guard the symmetry did not guard the count |
| 43 | Removing `--gpu-memory-utilization` in favour of the absolute cache **raised the startup gate to vLLM's 0.92 default** | `request_memory()` raises `ValueError` when free memory is below `utilization * total`, reads the fraction whether or not an absolute cache is set, and 0.92 of this card is **20.30 GiB**. The pilot measured the second engine to profile seeing **13.81 GiB**. Both split arms would have failed to start: the run would have bought R1 and `shared` and nothing the study is about |
| 44 | Reading **4c** took the ANSWER from a scored arm, and made the whole run exit non-zero | 4c means "INVALID **for that arm**", and the evaluator's own comment says a refused arm says nothing about the arm beside it. The answer was still "the first reading that fired" over a list where 4c sits ahead of readings 1, 2, 3 and 5. **MPS has already been measured failing to engage on this AMI**, so this is the expected run, not a corner |
| 45 | An arm that **lost** the contender's work without refusing it could fire reading 1 POSITIVE | `starved` needs rejections in the ledger and `starvationUnknown` needs the ledger to be absent; an arm with a full ledger showing zero rejections and half the contender's requests timing out is neither. The premium tail is calm because the load went missing, and reading 1 would have printed the collapsed contender share as the **price** |
| 48 | The port-forward teardown killed a **shell subshell** and not `kubectl`, so every cell's tunnel outlived the kill meant to end it | `k` is a function; backgrounding one runs it in a subshell, and bash keeps that subshell alive when traps are installed — three are. `$!` named the subshell, `wait` reaped it promptly, and kubectl went on holding 18080 as an orphan. **The sixth pilot died on this**: R1 replayed 3,882 rows, the `shared` cell asked for the same port and got `bind: address already in use`, and one arm of four was bought. The fix for defect 14 had been waiting on the wrong process since the day it was written |
| 47 | A YAML comment inside an **unquoted heredoc** quoted a command in backticks, and the shell ran it | the gateway manifest is built with `k apply -f - <<EOF`, unquoted so `$arm` and `$NS_A` expand — which expands backticks too. Every gateway deploy executed `rollout status` and printed **"rollout: command not found"** to stderr, four times a run, into the log an operator reads to find real faults. Harmless only by luck: the empty substitution landed inside a comment. `bash -n` is clean either way, and it took **running** the rehearsal to see it |
| 46 | Reading **4** could diagnose "the load did not create contention" from a **censored** control | censor the slow requests and the survivors' p99 is small, the ratio falls under 5x, and the reading meaning "raise the load" fires on a control that was drowning — short-circuiting 4b, the reading that would have said the opposite. The run's one instruction to its successor would have been exactly backwards |

### Six more after the seventh pilot, from a review that read the evidence rather than the prose

| # | defect | what it would have cost |
| --- | --- | --- |
| 49 | Split-arm setup failures called `fail`, which **exits**, so the caller's `apply_device_plugin "$arm" \|\| return 1` had been **unreachable code since it was written** | the seventh pilot died here with R1, `shared` and `timeSlicing` measured and paid for. The session ended before it could run its own report, and the mps arm left no `refused-mps.txt` for reading 4c. Five paths: plugin apply, plugin rollout, device count, both engine applies, and the co-location check |
| 50 | The device-count refusal asserted **"the plugin is ignoring CONFIG_FILE"** from a count | zero devices is equally consistent with an unhealthy device, a kubelet that has not re-registered, and a driver that went away. Naming a cause the ledger does not establish is what this repository puts above every other failure, and the refusal recorded no diagnosis at all — so "0 devices" was all a later reader would ever have |
| 51 | `contenderLost` was consulted by **reading 1 only** | reproduced by the review: an arm that lost half its contender work and missed both bars fired **reading 5 as protection**; the same arm meeting both bars **vetoed reading 5** for a healthy arm beside it; and reading 2 printed that every qualifying arm "kept the contender's work". A fix that touches one of four call sites is a fix that has not been made |
| 53 | The starvation test compared rejected **requests** against missing **output share**, with a factor of two on one side to make the units meet | reproduced: 400 offered, 200 completed, 100 rejected and 100 **timed out** fired reading 2 and printed "refused work, not merely late work" over a ledger where refusals explain half the loss and delay the other half. The two flags now partition one fact in one unit — either the refusals account for the missing requests, or the arm cannot be scored |
| 54 | A gate that could not be **computed** exited **zero** | reproduced with 2% premium timeouts in the control: reading 4 came back N/E, 4b passed, and the report printed no verdict and no answer with exit status 0. `benchharness report ... \|\| fail` would have accepted a censored control as a good session |
| 52 | The credential check hardcoded **seven minutes per replay**, ignoring `DURATION_MS` | the load derived above puts the trace at 505 s. The check would have approved a session on credentials that expire during it, and the first thing to fail would be the evidence download — after the card was paid for |

### Two more before the run that uses repetitions

Both were in the deferred list above, and both come off it for the same reason: **the next run is the first
to use more than one repetition**, which is what makes them live.

| # | defect | what it would have cost |
| --- | --- | --- |
| 55 | The contender's hundred-completion floor applied to the **pool**, and the premium floor exempted a **zero** | reproduced by the review: repetitions of 140, 140 and 50 contender completions, each offered 140, clear the floor at 330 pooled and fire **reading 1 POSITIVE**. The pooled completion fraction is 78.6%, above the 0.75 bar, so `contenderLost` does not catch it either — both guards read the total while the unusable block sits inside it. The premium check had a per-repetition limb since defect 36; the contender never did |
| 56 | Every cell's evidence went up **only after the whole matrix returned** | the matrix's own failure paths all reach the archive, which is why the seventh pilot's three arms survived a failed session. An **interruption** reaches nothing. At two repetitions a run is six cells and about ninety minutes of rented card, all of it on local disk until the end |

**55 carried a smaller defect inside it.** The premium floor read `MinRepetitionTail > 0 && ... < 100`, and
that exemption looked like carelessness about zero. It was not: the field cannot tell a measured zero from
one nobody attached, and the exemption was guarding the second case at the cost of the first. The
per-tenant map carries the distinction explicitly — a tenant **present with zero** was measured at zero, a
tenant **absent** was not measured — so the exemption is not needed and the zero is caught. A repetition
that served a tenant nothing carries no disposition entry for it at all, so the minimum is taken over every
repetition with a missing tenant counted as zero, or the search would skip exactly the block it is for.

**56 is a hook rather than an uploader**, because this same script runs on a local kind cluster in three
rehearsals where there is no bucket. Unset, the behaviour is exactly what it was — which is also how a hook
quietly stops being called, so `hack/test/rehearse-m5c-matrix.sh` now asserts it fired once per cell and
that assertion was checked by removing the call and watching it go red.

**49 and 51 are the same mistake in two languages.** Both are a guard that exists, is correct where it is
written, and is not consulted where it matters: an `|| return 1` the callee can never reach, and a flag
three of four readings never ask about. Neither is visible in a diff of the fix, because the fix looks
right. What found both was **executing the path** — a paid run for 49, a reproduction over saved evidence
for 51.

**Three tests written today were vacuous on their first run, and only a mutation found each one.** The
heredoc guard's regex excluded `&` so it could not see the line `... 2>&1 &` it was written for. The
reading-2 assertion looked for a phrase that appears only in a source comment. The starvation fixture gave
the contender a fifth of the control's output, where the OLD rule declined anyway — so it passed against the
defect it was written for. Each was caught by deliberately restoring the defect and finding the test still
green, which is the repository's rule applied to itself: a check that cannot tell "did not run" from
"passed" is worse than none.

**And the test written for 51 was itself vacuous on its first run.** It asserted that reading 2 does not say
"kept the contender", a phrase that appears only in a source comment and never in output. Corrected to the
sentence the code actually prints, it goes red on all three reproductions instead of two.

**45 changed an expectation this repository had already written down, and that is worth stating plainly.**
A test named `TestAContenderThatWasMerelyDelayedIsNotStarvation` held a contender that completed 120 of 300
requests with none rejected, and required reading 1 to fire POSITIVE on it — on the argument that work
which was not refused was merely late. Half of that argument was right and is kept: a smaller share
without rejections is not starvation, because the pre-registration says starvation must be shown in the
ledger. The other half was wrong. **A request that timed out was not served late, it was not served**, and
an arm that lost 60% of the contending load shows a calm premium tail for the one reason that is not
protection. The outcome for such an arm is now "cannot be scored" rather than "wins", and the test is
renamed to say so. No bar moved and no threshold was retuned; what changed is which of the three outcomes
this evidence maps to.

**43 and 48 are a pair, and the pair is the lesson.** Both are FIXES THAT COULD NOT WORK. 43 removed a flag
on the strength of what the engine's log advised and raised the startup gate it meant to lower. 48 added a
`wait` to close a race and waited on a subshell instead of on the process holding the port — for four days,
through two paid runs, while the comment above it explained in detail the race it was not closing. Neither
was visible to a reading of the code, because in both cases the code says exactly what its author intended.
What exposed them was checking the intent against the system: the pinned release's source for 43, and
`ps` against a five-line reproduction for 48.

**43 on its own.** It was introduced by the fix for 41, it was invisible to every test in the
repository, and the engine's own advice is what caused it: vLLM prints *"Replace gpu_memory_utilization
config with `--kv-cache-memory=…`"*, and the word **replace** is about sizing while the startup gate still
reads the fraction. Reading the log message was not enough; reading `request_memory()` and `cache.py` in
the pinned release is what settled it. Both flags are now passed, and the test that holds it computes what
the second engine will actually have free rather than checking that a flag is present.

**Seen in the same review and deliberately NOT fixed before this run.** Recorded so that "not mentioned"
is never mistaken for "not found", and so the next reader can weigh the judgment rather than repeat the
search:

- **Throughput and TPOT count content-bearing SSE chunks, not the engine's reported completion tokens.**
  A chunk carrying two tokens moves the statistics without moving the GPU's work. Real, and left alone on
  purpose: every published number in M5-b and the price-of-protection run uses this definition, and
  changing it now would make this study's figures incomparable with the ones it exists to be held against.
  It belongs in a change that re-derives the earlier runs, not in a patch the hour before a paid session.
- **TPOT has no sample floor of its own.** The premium completion floor of 100 protects TTFT, and TPOT
  only counts responses that produced at least two tokens, so the two can diverge. The gap is bounded by
  the fact that a completed premium response in this trace produces many tokens, and it is real.
- **Per-repetition contender COUNTS** and ~~**evidence uploaded only after the whole matrix returns**~~
  were fixed on 2026-09-12 as defects 55 and 56, once the eighth pilot established that the next run needs
  two repetitions. The reason for deferring them was that one repetition cannot trigger either, and that
  reason expired the moment the run needing repetitions became the next one to buy.

  **Per-repetition CENSORING was not fixed, and this line said it was.** A review checked and found the
  claim false: each repetition's `Censored` flag is still discarded when the summaries are built, and the
  readings check the pooled one. Marking the second control repetition's 70 slowest premium responses as
  timed out loses 1.50% of that repetition and 0.75% of the pool — the pool passes, both completion floors
  pass, and reading 5 fires on a censored control. Contender loss fractions have the same shape: 100/139
  and 139/139 clear both floors while the first repetition lost more than a quarter of its load.
- **MPS engagement is proved by the engines reporting a pipe directory**, not by finding both workers in
  the daemon's client list.

The common reason for deferring all five: **each is a change to the scorer or the runner, and the last two
defects in the table above were both introduced by a fix.** Defect 43 came from the fix for 41 and would
have wasted the entire run. Making five more scoring changes in the hour before paying for a card is the
pattern that has already cost this study two pilots, and the readings refuse rather than guess when any of
these bite — which is the property that makes deferring them safe.

**Defects 23 and 27 are the ones to dwell on**, for opposite reasons. 23 was found by a machine attacking
the tests rather than the code, and what it exposed was a hole in this document. 27 lets a reading make a
universal claim — "no sharing arm improves" — about arms that were never measured, which is the same shape
as reporting a censored tail as a p99.

**And a note on the reviews themselves.** The first pass was a cheaper model and found six; the second was
the strongest available and found eleven more, eight of which the first had walked past. Neither was
reading its own code. That is the argument for the repository's dual-review hook, demonstrated rather than
asserted.

### What the fourth pilot measured, 2026-09-12

The first three pilots bought only defects. This one bought two facts about the card, and neither is a
result of the study — both are about whether the study can be run at all.

**Two engines DO fit on one A10G.** `timeSlicing` replayed 4,120 requests and completed 3,943 of them, with
both engines at `--gpu-memory-utilization=0.475`. The arithmetic that made this look doubtful — two engines
claiming 21,877 of 23,028 MiB and leaving 1,151 for two CUDA contexts — was a hypothesis this page was
careful not to act on, and it was wrong. **Nothing was changed on the strength of it, which is why the
measurement was available to contradict it.**

**MPS does not engage on this AMI.** Every Pod of the `mps` arm came back with

> `Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu`

The plugin advertises two devices and the kubelet refuses to allocate them.
`config/nvidia-device-plugin-mps/daemonset.yaml` has said since it was written that this arrangement was
*"NOT verified … on the AL2023 NVIDIA AMI. No card was available to run it."* It is verified now, and it does
not work. That is **reading 4c**, INVALID for that arm, and the two sharing arms are no longer two: unless
the plugin's MPS configuration is repaired, this study compares `shared` against `timeSlicing` and reports
the MPS arm as a mode that could not be engaged.

**The run produced no scorable result**, because R1 completed none of its 3,882 requests: the replay began
before the gateway was listening, and every row is `errorKind=transport`. The readings refused exactly as
they should have — *"R1 has no premium tail, so there is no baseline to hold the control against"* — rather
than dividing by a baseline that was not there. That defect is fixed, and it is recorded below as 39 with
the fact that it had been named in a review the day before and not acted on.

### The fifth pilot: the first ANSWER, and it is INVALID

2026-09-12, three arms, about $0.75. The instrument ran end to end and the readings returned a verdict:

```
[     ] 4   the control's premium TTFT p99 is 112.6x R1's (7892.4 ms against 70.1 ms), against 5.0x
[FIRED] 4b  against a floor of 100: timeSlicing completed 50 standard-noisy requests

ANSWER: 4b        run invalid
```

| arm | premium done | contender done | TTFT p99 | premium TPOT p99 |
| --- | ---: | ---: | ---: | ---: |
| R1 | 3,882 / 3,882 | — | 70.1 ms | 18.2 ms |
| `shared` | 3,882 / 3,882 | 238 / 238 | 7,892.4 ms | 151.6 ms |
| `timeSlicing` | 2,700 / 3,882 | **50 / 238** | 803.4 ms (censored) | 44.0 ms |

**The numbers in that third row must not be read**, and the reading is why. `timeSlicing` looks like a large
win — a tenth of the control's tail, a third of its TPOT — and it is censored evidence from an arm that
dropped 79% of the contending tenant. Whether the tail is low because the split protects it or because
there was nothing left to contend with is exactly what this evidence cannot say. **Reading 4b refused
instead of reporting it**, which is the whole reason the floor was registered.

**And the cause was not the load.** The engines' own startup reports, captured because a defect fixed the
day before made the runner ask them:

| | KV cache | tokens | contender prompts |
| --- | ---: | ---: | ---: |
| whole card (`shared`) | 12.69 GiB | 369,680 | 47.7 |
| split engine **a** | 3.33 GiB | 97,056 | 12.5 |
| split engine **b** | **1.52 GiB** | **44,144** | **5.7** |

Two engines, identical manifests, caches differing by more than two to one. `--gpu-memory-utilization` is a
fraction of the WHOLE CARD, and the engine that starts second sees the first's allocation as memory already
consumed: it computed its own usage as 8.38 GiB against 6.56 and took what was left. The smaller engine is
the one that timed out 188 of the contender's 238 requests.

**A fraction cannot express "half the card" to a process that can see the other half.** vLLM says so in the
line the runner captured — *"Replace gpu_memory_utilization config with `--kv-cache-memory=…`"* — and both
split engines now carry an absolute budget of 3.2 GiB, derived from their own report rather than chosen:
22.06 GiB visible, 7.32 GiB per engine of weights, activation and CUDA graphs, one GiB of driver reserve,
halved. A test holds the two values equal, because the previous guard checked that the FRACTIONS agreed and
went quiet the moment the fractions were replaced: the property moved and its guard did not.

**This was not visible without a card.** The asymmetry only exists when two engines share one device, and
every rehearsal in this repository substitutes a stub for the engine. It is the first thing in five paid
runs that a free cluster could not have found.

### The platform changed, and so did the budget

The original budget — pilot ~$1.30, confirmatory ~$2.90 — was costed from the runtime model fitted to three
paid runs that each rented **one self-contained Spot instance**. `hack/m5c-matrix.sh` does not do that. It
drives an **EKS cluster with a GPU node group**, and no such cluster exists or has ever been applied. The
figure was wrong for a reason that has nothing to do with the arithmetic: it costed a shape the runner does
not have.

The runner now takes `PLATFORM=eks|kind`. The `kind` path rents one Spot instance and builds a kind cluster
on it with the real NVIDIA device plugin — the recipe `hack/queuelab-gpu-session.sh` already paid for and
proved on A10Gs. The EKS path is untouched: its lines were moved into a function and not otherwise edited,
which `git diff -w` shows.

**`g5.2xlarge`, not `g5.xlarge`.** Measured on 2026-09-10 in `ap-northeast-2`: **$0.68/h against $0.58/h**,
for 32 GiB of host memory instead of 16. A kind node, two vLLM engines and the harness on 16 GiB is the kind
of margin that is discovered at the rollout timeout, and ten cents an hour is the wrong place to economise.

## Design

Three arms, on one `g5.2xlarge` (one A10G), through `PLATFORM=kind`:

| arm | topology |
| ------------- | -------- |
| `shared` | both tenants on ONE engine — the price-of-protection control's topology |
| `timeSlicing` | one engine per tenant, each with its own cache, both **time-slicing one card** -- shared access, not a memory partition |
| `mps` | the same two engines, sharing through MPS instead |

Plus **R1**, the isolated premium baseline, which every study here measures as its ceiling.

`shared` is the control and it is not a formality: it is the arm that reproduces the previous result on this
topology, and if it does not reproduce, nothing else on the page means anything.

**What this cannot report, by construction.** Under time-slicing and MPS a busy SM belongs to no single
engine, and `internal/queuelab`'s exclusivity clause refuses to attribute it. So the matrix reports what the
CLIENTS observed — per-engine latency and throughput — and reports no per-engine GPU utilisation. The
sharing modes get their own device-plugin configuration rather than replacing the exclusive one, because
that config is what queuelab's device evidence depends on.

### What the engines may and may not differ in

The matrix varies topology. Every other engine setting has to be identical across the arms, or its effect
arrives in the result wearing topology's name — which is what defect 1 was. Three settings are allowed to
differ, and each is forced by the split rather than chosen:

| setting | exclusive | each split engine | why it may differ |
| --- | ---: | ---: | --- |
| the KV budget | `--gpu-memory-utilization=0.90` | `--kv-cache-memory=3435973836` (3.2 GiB) | time-slicing does not partition memory; two engines draw on one pool. **Amended 2026-09-12 after the fifth pilot** — this row said `0.475` each, and a fraction is a fraction of the whole card, so the second engine to start took what the first had left. See *The fifth pilot* above |
| `--max-num-seqs` | 64 | 32 | half each, so **the card admits the same total concurrency under either topology**. Per-engine 64 would offer the card twice the concurrency in the split arms and confound separation with a larger batch |
| the device-plugin overlay | whole-card, 1 device | time-slicing or MPS, 2 devices | this is the mechanism under test |

Everything else must match, and `--max-num-batched-tokens=2048` is now **stated** on all three engines
rather than left to the engine. 2048 is not a choice: the price-of-protection run established it by running
an arm that asked for 2048 explicitly and reproducing its control's throughput to 1.2 ms. Left as a default
it would have been a default resolved **from how much card the engine has**, so the split engines could
silently have run a different budget from the exclusive one — and that run measured the budget moving the
premium tail about fivefold, which is the size of effect this matrix is looking for.

`internal/bench`'s contract tests hold this: any flag the exclusive engine passes must be passed identically
by both split engines unless it is named in an allow-list with a reason, and a flag added to either side
later fails the suite until somebody decides which case it is.

**This run therefore reports nothing about what budget a half-card engine would choose for itself.** That is
a real question and it is not this one.

### The eighth pilot: the instrument worked, and the load was right

2026-09-12, `hack/m5c-20260912-084918`, commit `ce0bb8e`. Four arms, one repetition, about 52 minutes and
**$0.59**. The first session in eight to print `SESSION DONE`.

**The derived load did what the derivation said it would**, which is the only test that derivation could
have:

| | derived | measured |
| --- | ---: | ---: |
| contender requests offered | 139 | **139** |
| contender requests lost | near zero | **0**, in both arms |
| queue-delay slope, `shared` | flat | **+0.0001** |
| queue-delay slope, `timeSlicing` | flat | **+0.0025** |
| contender TTFT median, split | ~2.15 s (the measured uncontended prefill) | **2.33 s** |

The seventh pilot's contender delay climbed 5.1 s → 22.3 s and 74% of its work was abandoned. Here it is
flat and nothing is abandoned. The split arm's 2.33 s median against the 2.15 s the derivation was built on
is the prefill measurement agreeing with itself under load.

**And what actually settles the load is this run, not the derivation.** The derivation sized the rate from
an uncontended **prefill** time, which is not a request capacity — a request holds the engine past its
first token, and reciprocating the median TOTAL response time would have said 0.150 req/s where
reciprocating the prefill says 0.466. Neither is the capacity and this evidence does not locate it. What is
now established is narrower and enough: **at 0.275 req/s both arms served every contender request with a
flat queue.** The tuple is viable because it was run, not because it was derived.

**All three gates behaved as registered, for the first time:**

```
[     ] 4   the control is 27.2x R1 (1891.1 ms against 69.6 ms), against a 5.0x threshold
[     ] 4b  every arm completed at least 100 requests for both tenants
[FIRED] 4c  mps: the node advertises 0 device(s) after applying a config that asks for 2
```

**4c fired and the readings below it were still reached.** That is defects 44, 49 and 50 confirmed on a
card at once: the refusal was recorded in `refused-mps.txt` where the report reads it, the arm's diagnosis
(DaemonSet, Pods, events, node allocatable) is in the log, the refusal states the count without asserting a
cause, and the session did not end. Three arms stood. MPS has now failed to engage on this AMI three times.

**What the card did:**

| arm | premium TTFT p99 | /R1 | premium TPOT p99 | /R1 | timeouts |
| --- | ---: | ---: | ---: | ---: | ---: |
| `R1` | 69.6 ms | 1.0x | 18.2 ms | 1.0x | 0 |
| `shared` | 1,891.1 ms | 27.2x | 89.1 ms | 4.9x | 0 |
| `timeSlicing` | **1,008.7 ms** | **14.5x** | **44.1 ms** | **2.42x** | 0 |

Both split engines took **93,200 tokens** of KV, equal to each other, as the absolute budget was meant to
make them. Splitting the card roughly **halves** the control's premium tail while the contender keeps all
of its work. It remains 14.5x an isolated baseline against a 2x bar.

**And the run still has no answer, for a reason that is neither the load nor the harness:**

```
[ N/E ] 3   `shared` carries 1 per-repetition tail(s); needs at least 2
[ N/E ] 5   `shared` carries 1 per-repetition tail(s), so there is no spread
ANSWER: 4c (mps)
```

Readings 3 and 5 decide whether an improvement is real by comparing it against the control's
repetition-to-repetition spread, and a single repetition has none. They reported themselves **not
evaluable** rather than passing or failing on a spread of zero — which is what this page registered them to
do and what the previous study had no name for.

**So the next run's missing SCIENTIFIC input is `REPS`.** The load is derived and verified against a card,
the gates are quiet, and the harness carries a paid run to its own report.

**But `REPS=2` on its own buys the wrong run, and saying otherwise was this page's mistake.** An earlier
version of this paragraph said the missing input was "`REPS`, and nothing else", which is operationally
false: `hack/m5c-gpu-session.sh` still defaults to `RATE=9.85`, `NOISY_WEIGHT=0.054`,
`DURATION_MS=420000` — **the seventh pilot's load, the one reading 4b rejected** — and to four arms
including `mps`, which has now failed to engage on this AMI three times. `REPS=2` alone therefore buys
**eight cells at the rejected load**, which is about as wrong as a run can be while still completing.

The eighth pilot's tuple has to be passed in full, and the arms named:

```
AWS_PROFILE=gpu-lab REPS=2 \
  ARMS="R1 shared timeSlicing" \
  RATE=9.4045 PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.0260 PROBE_WEIGHT=0 DURATION_MS=505000 \
  bash hack/m5c-gpu-session.sh
```

Six cells: about 104 minutes against the runner's credential check, plus its 30 of headroom. Dropping `mps`
is a choice and not an oversight — three refusals are enough evidence that this AMI will not engage it, and
a fourth costs a cell without adding one.

### THE ANSWER, from the ninth pilot — reading 5

2026-09-13, `hack/m5c-20260913-011031`, commit `85ae2fa`. Three arms, **two repetitions**, six cells, about
75 minutes and **$0.85**.

```
ANSWER: 5 (timeSlicing)
```

The ninth paid run is the first to produce one. Readings 4 and 4b stayed silent, 4c reported that `mps` was
absent rather than refused — it was not in `ARMS`, and absence is not a refusal — and **reading 5 fired**:

> `timeSlicing` improves the control's premium tail by **884.6 ms** against a **1.4 ms** spread, and misses
> both bars: tail **14.5x** R1 against 2.0x, stream **2.42x** against 1.25x — a real improvement that does
> not reach the bar, **with all 278 of the contender's requests served**.

| arm | premium TTFT p99 | /R1 | premium TPOT p99 | /R1 | contender | timeouts |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `R1` | 69.5 ms | 1.0x | 18.2 ms | 1.0x | — | 0 |
| `shared` | 1,892.2 ms | 27.2x | 89.1 ms | 4.9x | 278/278 | 0 |
| **`timeSlicing`** | **1,007.5 ms** | **14.5x** | **44.1 ms** | **2.42x** | **278/278** | 0 |

**Splitting the card halves the premium tail. The contender keeps all of its work and pays for it in
latency**, which the sentence above this one omitted until a review put the numbers beside it:

| | `shared` | `timeSlicing` | |
| --- | ---: | ---: | ---: |
| contender TTFT median | 1,050.6 ms | **2,329.7 ms** | 2.2x |
| contender completion median | 1.660 s | **4.697 s** | 2.8x |
| contender completion p99 | 5.111 s | **16.251 s** | **3.2x** |
| premium TTFT median | 59.1 ms | **108.0 ms** | 1.8x |
| premium completion median | 1.257 s | **2.169 s** | 1.7x |
| premium completion p99 | 6.712 s | **3.685 s** | **0.55x** |

"No requests refused, none lost" is a statement about COUNTS. Reading 2 is the reading that asks whether
work was refused, and it correctly did not fire. But a reader deciding whether to run this topology needs
the rest: **the contender's tail triples**, and the premium tenant trades a worse median for a better tail.
The headline improvement is in the premium p99 and it is real; it is not free to anyone.

The premium tail is still 14.5x an isolated baseline against a 2x bar.

**Why the two repetitions were what this run was for.** Readings 3 and 5 judge an improvement against the
control's own repetition-to-repetition spread, and one repetition has none — which is why the eighth pilot
could measure this and not score it.

| arm | repetition 1 | repetition 2 | spread |
| --- | ---: | ---: | ---: |
| `R1` | 69.5 ms | 69.6 ms | **0.2 ms** |
| `shared` | 1,892.2 ms | 1,893.5 ms | **1.4 ms** |
| `timeSlicing` | 1,001.9 ms | 1,014.2 ms | 12.3 ms |

884.6 ms exceeds the control's 1.375 ms range, and reading 3 did not fire for that reason. An earlier
version of this line quoted the quotient as a multiple — **643x** — which is a three-significant-figure
ratio to a range of two numbers. It is dropped rather than corrected: the registered rule asks whether the
improvement clears the range, not by how many multiples, and the multiple invites the reading the next
paragraph exists to refuse.

**What that range is and is not.** Two repetitions of the same trace, in the same order, on the same
instance give a **range of two numbers** — not a confidence interval, not a bound on systematic error, and
not a sample of workload variation. The registered rule asks whether the improvement exceeds the control's
repetition range, and it does, by a wide margin. That is what fired. It is not a statistical test, and the
split arm's own range (12.27 ms) is nine times the control's. An earlier version of this paragraph said
"far outside the noise it would have to hide in", which claims more than a two-point range can carry.

The eighth pilot, a different instance on a different card, measured the same control at **1,891.1 ms** —
1.02 ms and 2.40 ms below the two repetitions here. (This page said "within a millisecond of both", then
"within a millisecond of one". Neither is true: 1.02 ms is not within a millisecond either. The two
differences are what they are, and the useful fact is that the control reproduces on a different card.)

**What this study can now say.** For this model, this card and this load, the `timeSlicing` arm **recorded a
46.8% lower premium TTFT p99 than the control**, with both tenants completing every offered request, and
does not come close to a 2x bar. An earlier version of this line said the topology "halves the premium
tail". Two corrections are folded in: the sentence claimed a cause the fixed arm order cannot establish,
and "a time-sliced half of the card" describes an enforced partition that time-slicing does not provide —
it is shared access to the whole card, alternating.

**And "halves" understates the arm at every percentile but the one the bar reads.** The p99 is the
registered statistic, so it is what the readings score; it is also the least favourable single number the
distribution has to offer. Recomputed from the same six raw files:

| premium TTFT | `R1` | `shared` | `timeSlicing` |
| --- | ---: | ---: | ---: |
| p90 | 61.1 ms | 925.4 ms | **161.4 ms** |
| p95 | 63.9 ms | 1,143.5 ms | 396.8 ms |
| p99 | 69.5 ms | 1,892.2 ms | 1,007.5 ms |
| p99.9 | 78.6 ms | 3,296.6 ms | 1,477.8 ms |
| max | 387.0 ms | 3,782.0 ms | 1,723.3 ms |
| share over the 2x bar (139 ms) | 0.08% | 29.18% | 17.81% |
| share over 500 ms | 0% | 20.61% | **3.86%** |
| share over 2 s | 0% | 0.77% (72 requests) | **0%** |

The arm does not shift the tail uniformly. It **collapses the p90 by 5.7x, cuts the share over half a
second by 5.3x, and empties the region beyond two seconds entirely**, while the p99 only halves. A reader
choosing a topology is usually choosing against the 2-second region rather than against a percentile, and
that region goes from 72 requests to none. This is free evidence from the run already bought — it is
reported here because the registered statistic alone understates what was measured, not because the bar
moved. **The bar did not move, and the arm still misses it.**

**What three studies say together, stated carefully.** Admission (M5-b) missed its bar at 83.7x. Engine
scheduling (the price-of-protection run) reached 20.7x at best. This study reaches **14.5x**. Each missed
its own bar, and that is the whole of the comparison.

It is **not** a progression of mechanisms, and an earlier version of this paragraph said it was. The three
studies ran different loads: this one deliberately cut the contender's arrival rate, and its own control
moved from about **113x** R1 in the seventh pilot to **27x** here. Holding 20.7x beside 14.5x therefore
compares two different amounts of contention, not two mechanisms against a fixed one. What the three share
is that each failed to reach its own bar; the size of any incremental benefit between them is unmeasured.

**The limitation this page registered in advance still applies**, and it is the reason reading 5's price is
not the mechanism's price: two engines carry two copies of the weights, so the split arms hold about half
the control's KV cache. That is inseparable from **this arm set** — a whole-card control deliberately given
a 6.4 GiB cache would hold the total constant, and was never budgeted. An earlier version of this sentence
said "no arm could hold it constant", which is false and is corrected in the limits section below.

### What this evidence still cannot decide, and what would

Added 2026-09-13 after a review of the ninth pilot's own conclusions.

**The capacity half of this page's question is not answered.** Every arm delivered the same 595,840 premium
output tokens, because the trace asks for a fixed amount of work and every arm finished it. Equal totals are
consistent with ample headroom, with brief saturation, and with a backlog that drained afterwards — they do
not distinguish the three. And the premium tenant's client concurrency reaches **52 against 32 engine
sequence slots for about 38 seconds** in the split arm, so "the premium side was never saturated" is not
available either.

The reported premium throughputs (589.3 / 586.9 / 584.0 tok/s) also divide by the WHOLE ARM's elapsed time,
and in the split arm the contender finishes **3.31 s after** the premium tenant. Measured against premium's
own last completion the split reads **587.8 tok/s**, so even the apparent 0.9% gap partly charges the
premium tenant for another tenant's drain. **Neither topology's maximum was located, so no percentage of
capacity lost can be stated.**

What the drains do support, measured and reproducible to a hundredth of a second across both repetitions:

| | premium's own drain after its last arrival |
| --- | ---: |
| `R1` | **1.03 s** |
| `shared` | **3.14 s** |
| `timeSlicing` | **2.35 s** |

The split leaves the premium tenant *less* backlog than the control does. The arm's total drain is longer
(5.66 s) and that tail is the contender's engine finishing on its own half, which does not block premium.

**What would answer it** is a registered definition of capacity — maximum completed throughput, or the
maximum rate that still meets a stated service requirement — and several premium rates to bracket it, with
the contender's arrival rate held fixed in absolute terms rather than by weight. One higher rate is not
enough: it can overload both arms, neither, or leave the client timeout deciding the answer.

**And the ordering confound is not broken by the reversed run this page first proposed** — but it is
cheaper to break than that draft claimed, and the claim was wrong in a way worth recording.

Reversing the whole sequence, `R1 → shared → timeSlicing` to `timeSlicing → shared → R1`, leaves **`shared`
in the middle in both**, so its position never varies and the contrast that matters stays entangled with
position. From that this page concluded a single reversed session could not counterbalance and that blocks
were required. **That does not follow.** Swap only the two contended arms — **`R1 → timeSlicing → shared`**
— and R1 keeps position 1, which is the position it holds for a reason this page already states
(a session cut short after one cell still holds the denominator both bars divide by), while `shared` and
`timeSlicing` each occupy positions 2 and 3 exactly once across the two sessions. That is a proper
counterbalance of the pair under test, for one more session rather than a block design.

What a second session still mixes in is session effects: a different instance, a different physical card,
a different Spot placement. The eighth pilot bounds that for the control alone — 1,891.1 ms on another card
against 1,892.2 and 1,893.5 ms here — and bounds it for no other arm.

**No numerical attribution of the 884.6 ms to position is available from this evidence.** The two
repetitions show the difference reproduces (890.2 ms and 879.3 ms) and that R1 moves 0.179 ms when it meets
a card the other arms have already used. That weakens an explanation built on simple drift. It does not
bound the position contribution, which could be zero, all of it, or larger than all of it with the topology
pulling the other way.

## The bars do not move, and that is deliberate

**Premium TTFT p99 at or below 2x R1. Premium TPOT p99 at or below 1.25x R1.** The same numbers the
price-of-protection run used, and the same ones M5-b's 1.25x descends from.

They are carried forward rather than chosen, and carrying them forward is the only choice that cannot be
accused of fitting. Every cell in the last run missed the tail bar by an order of magnitude; a bar picked
now, with those numbers in hand, would be picked for reachability. `2026-09-09-what-would-have-to-change.md`
says a successor may set a different bar only from a stated service objective that does not read that run's
results, and no such objective has been stated, so the bar stays where it was.

The share and throughput bars change, because the quantity they were about changes. Splitting a card is
supposed to cost capacity — that is the trade being measured, not a defect — so the aggregate-throughput
clause is replaced by reading 1b's job of reporting the price rather than gating on it.

## Pre-registered readings

Evaluated in this order. **The first that fires is the answer.**

### 4. The load did not create contention — INVALID

If `shared`'s premium TTFT p99 is under 5x R1's, this trace is not producing the interference the run exists
to study. Same threshold and same reason as the previous study's reading 4.

### 4b. The load was too high to measure — INVALID

If any arm completed fewer than `MinTailSamples` premium requests, or fewer than `MinTailSamples` of the
contender's, the run is INVALID. `MinTailSamples` is 100 and is derived in `internal/bench/report.go` from
the nearest-rank percentile, not chosen here.

**Applied to every arm, not only the control.** The previous study put this floor on the control alone,
because the control was the denominator of every ratio. Here a split card gives each engine less to work
with, so an arm collapsing is a live outcome for the arms themselves and not just for the baseline.

**And to every REPETITION, not to the pool.** Added 2026-09-11 alongside the reading 5 amendment, and for
the same reason: an independent review pointed out that pooling hides the thing the floor exists to catch.
Three repetitions of 3000, 3000 and 50 premium completions clear a hundred-sample floor with 6050 pooled,
while the third block's "p99" is that block's slowest request — and the table prints `reps=3` and a healthy
count with nothing saying a paid block was unusable.

**A matrix whose arms were repeated unequally is INVALID too.** The confirmatory run is three separate
single-repetition sessions pooled at report time, so an arm that lost a session is pooled from two while
the others come from three. Reading 3's threshold is the control's spread across its repetitions, and an arm
measured on two instances held against a spread measured over three weights instance variation differently
by arm — while the report prints a perfectly ordinary POSITIVE or NEGATIVE. Neither of these moves a
threshold; both refuse evidence that cannot support the readings below them.

### 4c. The sharing mode did not engage — INVALID for that arm

If the MPS arm ran without the control daemon reachable, it is the time-slicing arm under another name.
`hack/m5c-matrix.sh` already refuses this rather than reporting it; the reading exists so the refusal is a
registered outcome rather than a script detail.

### 1. Separation protects — POSITIVE, and this is the deliverable

A sharing arm fires this if **all three** hold against R1:

- premium TTFT p99 at or below **2x**, and
- premium TPOT p99 at or below **1.25x**, and
- **reading 2 does not hold for that arm** — the tail was not bought by starving the contender.

If both sharing arms fire, the answer is the one with the higher contender throughput. Exact ties go to
`timeSlicing`, because MPS needs a control daemon that can be absent, and the simpler mechanism is the
smaller claim.

**The third clause was added on 2026-09-10, before any card, and it is a correction rather than a
tightening.** This reading first listed only the two bars. Reading 2's condition — meets both bars *and*
starves the contender — is then a strict subset of this one, and "the first that fires is the answer" would
have reported an arm that bought its tail by taking the other tenant's work as POSITIVE, with reading 2
never reached. **Reading 2 was unreachable.** That is the same defect class this page was written to close:
the previous study's readings did not cover their own outcome space, and here two of them overlapped instead
of leaving a gap. Found by implementing them, which is why implementation is a gate on renting a card.

The price-of-protection evaluator does not have this problem because its reading 1 gates on the contender's
share directly. This page deliberately removed that bar — splitting a card is *supposed* to cost capacity —
and removing it is what left the overlap.

**The price is reported, not gated on.** The write-up's first sentence must carry the premium tenant's
throughput under the winning arm as a fraction of R1's. Protection that costs half the machine is a real
answer and a different product from one that costs a tenth, and a pass/fail line would report them
identically.

### 2. Separation protects only by starving the contender — NEGATIVE

If a sharing arm meets both bars while the contender's completed output falls below **75%** of its output
under `shared`, then the tail was bought by taking the other tenant's work rather than by dividing the card.

**Starvation must be shown, not inferred from a smaller number.** The run records, per tenant, completed
responses, admission rejections, client-side timeouts, streams that broke after their first token, and
requests still outstanding when the window closed. A share that fell because work was delayed is a different
finding from one that fell because work was refused, and only the second is this reading.

### 3. Splitting the card changes nothing that matters — INCONCLUSIVE

If no sharing arm improves premium TTFT p99 over `shared` by more than `shared`'s own
repetition-to-repetition spread, then dividing the machine does not move this load and the next step is
neither this mechanism nor another sweep of it.

### 5. It protects but not to the bar — the outcome the last study had no name for

If a sharing arm improves premium TTFT p99 over `shared` by more than the spread, but no arm meets **both**
bars, **that is this reading and it fires.**

**"Both bars" is an amendment made on 2026-09-11, and the freeze clause at the top of this page had already
begun.** It is recorded here rather than made quietly, and what follows is the argument for it. Disagree
with the argument and the amendment should be reverted, not kept because it is already in the code.

This reading first said "no arm meets the 2x bar" — the tail bar alone. Reading 1 gates on two bars, and so
does reading 2. So an arm that holds the tail inside 2x while its stream runs at ten times R1's fires
**nothing**: not 1 or 2, which want both bars, not 3, which wants no improvement over the control, and not
this reading, which turned it away for having met the tail bar. **That is the identical gap this page was
written to close**, one bar over from where the last study left it.

Three things make the amendment legitimate rather than convenient:

- **No paid run has produced a scorable result.** Three pilots have been bought and every one of them
  ended in a defect; the only evidence ever scored came from stub engines in a rehearsal. There is no
  outcome this change could be fitted to, because there are no outcomes.
- **It moves no threshold.** 2x and 1.25x are untouched, and so is every other reading's condition. What
  changes is which reading claims a region of the outcome space that currently belongs to none of them.
- **It was found by a machine, not by a preference.** A mutation battery against the scorer showed the
  stream bar was pinned by no test; writing that test produced the arm above, and the arm had no reading.

With the amendment the four readings partition the improved-arm space: met both bars with the contender's
work intact is 1, met both by starving it is 2, improved on the control without meeting both is this
reading, and failed to improve beyond the control's own noise is 3.

**What this does not claim.** The readings are still not mutually exclusive ACROSS ARMS — time-slicing can
satisfy reading 1 while MPS satisfies reading 2 — and a large enough control spread can satisfy 1 and 3 at
once. First-match ordering picks the answer; it does not make the predicates disjoint. That was true before
this amendment and remains true after it.

It exists because the previous study's readings did not cover their own outcome space. Reading 2 there
required some cell to have met the bar; reading 3 required no cell to have beaten the control; the evidence
landed between them and nothing fired. That gap is closed here in advance rather than after seeing which way
the numbers went. The write-up reports the improvement and the remaining distance, and the milestone closes
on it as a measured partial result.

## The load has to be re-derived before this is bought

The price-of-protection load was derived against ONE engine with the whole card. This run gives each tenant
an engine with half of it, so the same offered rate may saturate what it now has.

The derivation is the same shape as `2026-09-08-the-load-needs-an-upper-gate.md`: measure the sustained
prefill throughput of a single engine on half a card, hold the premium tenant's rate fixed because it is a
few percent of capacity and moving it changes what the result means, and set the contender's rate so the
offered prefill sits near 60% of what the split engine can do. Reading 4b is the backstop if that derivation
is wrong.

**And the mix, not only the rate.** Defect 6 above is the reason this is spelled out. `gen-trace` defaults
to premium 1 / noisy 1 / two probes at 0.1, which puts the 40,000-character contender at 45% of arrivals; at
about 1.03 s of engine per contender prompt that is four to five times an A10G's prefill capacity at any
rate this study could use. Lowering `RATE` until the contender fits drops the premium tenant below the
hundred-sample floor reading 4b enforces, so **no value of `RATE` alone produces a valid trace**. The
price-of-protection run measured `RATE=9.85 PREMIUM_WEIGHT=1 NOISY_WEIGHT=0.054 PROBE_WEIGHT=0.0054
DURATION_MS=420000` for one engine with a whole card; that is a starting point for the derivation, not its
answer, because each engine here has half a card. The runner refuses to start without all five.

**This is a pilot's job, not the confirmatory run's.** No confirmatory time may be bought until a pilot has
cleared readings 4, 4b and 4c and produced a load whose derivation is written down.

### The derivation, from the seventh pilot — and its first two versions were wrong

The seventh pilot completed three arms on one A10G:

| arm | contender offered | completed | timed out | premium TTFT p99 | /R1 |
| --- | ---: | ---: | ---: | ---: | ---: |
| `R1` | — | — | — | 69.9 ms | 1.0x |
| `shared` | 238 | **238** | 0 | 7,911.5 ms | 113.1x |
| `timeSlicing` | 238 | **61** | 177 | 839.3 ms | 12.0x |

**Two wrong derivations were written here before the right one, and both are described rather than deleted.**

**The first** read 61 completions over 420 s as 0.145 req/s of capacity. Bucketing shows why that is not a
capacity: counted by when they finished, the split arm completed 23 requests in the first 70 s and 26 in
the next, then **zero across the 210 seconds from 140 s to 350 s** while 114 more were offered, none from
the 48 offered after that, and 12 during the drain once arrivals stopped. (An earlier version of this
sentence said 30 and 19 and "280 seconds". Those counts are by the time each request was SENT, which is the
right clock for offers and the wrong one for completions; mixing the two in one sentence is how "114 offers"
came to sit beside an interval that is 210 seconds long.) An engine at its
limit still completes work, only more slowly. This is queue delay growing without bound until it crosses the
replay client's **30-second timeout**, after which requests are abandoned before they can finish. The
completion count measures the timeout. It also divided by 420 s while counting completions that landed at
430–437 s, after the trace had ended.

**The second** inferred capacity from how fast the queue grew — `W(t) = (λ/μ − 1)·t`, fitted on the window
before the first abandonment, since abandonment censors exactly the slow requests the slope is made of. That
gave μ ≈ 0.503 req/s, and it is a sounder method than counting, but it is still an inference.

**The third measures rather than infers, but it measures PREFILL and that is not the same as capacity.**
Among the contender's own rows there are requests that arrived while **no other contender request was in
flight**, and their time to first token is that engine's prefill time for this prompt, read off the wire:

| topology | contender client TTFT with no other contender in flight at arrival | n |
| --- | ---: | ---: |
| `shared`, one whole-card engine | **1.05 s** (1.03–1.34) | 60 |
| `timeSlicing`, two engines time-slicing the card | **2.15 s** (2.10–2.21) | 4 |

The reciprocal column this table used to carry is gone; see below for why.

**What that last column is not — and the reciprocal is not a capacity, so it is withdrawn.**

Two reviews took this apart and the second went further. `1/TTFT` is not even a clean prefill rate: client
TTFT includes scheduling and delivery, the premium tenant is running throughout, and **three of the four
"uncontended" requests receive another contender arrival before their first token**. Stricter isolation
leaves **one** request. Reciprocals of a handful of selected latencies do not bracket a batched engine's
sustainable throughput in any direction, so **0.466 req/s, 0.150 req/s, the "between them" bracket and the
49% ratio are all withdrawn.** They are reciprocals of latencies and nothing more.

The small-sample convention needs stating too, because the quoted median is not the conventional one. The
four split TTFTs are **2.10058, 2.10421, 2.14531, 2.21089 s**: conventional median 2.12476 s, nearest-rank
median 2.10421 s. This page quoted 2.15, the upper middle observation.

**What survives is a sizing heuristic and its provenance.** A contender prompt's client TTFT on this card
is about **1.05 s** with one engine and about **2.1 s** with two time-slicing it. That is what the load was
sized from, and the eighth and ninth pilots then ran that load successfully — which is the evidence that
the sizing worked, not evidence that an engine serves 0.466 requests per second.

Everything downstream inherits the withdrawal: **"122% of capacity", "ρ = 0.59", "29% busy" and "the queue
does not diverge" are not established**, because the denominator they divide by was never a capacity. What
IS established is what the runs delivered: every contender request served with no losses, a contender TTFT
slope of about **0.0025 s/s** (1.26 s of growth across 505 s, which bursty arrivals can produce without
unstable queueing), and the drains recorded above. Sustained stability at this rate is not established by
two runs of 505 seconds.

The whole-card figure independently reproduces the **1.03 s** that
`2026-09-09-what-would-have-to-change.md` derived from a different run, which is the check that the method
is measuring what it claims. The split engine takes **2.05x** as long, which is what time-slicing a card two
ways should cost and had never been measured here.

The seventh pilot offered **0.567 req/s** of contender load. The reciprocal of the split engine's median
first-token wait is below that and the whole card's is above it. **Those reciprocals are not service rates**
— the section above withdraws them as such, and this comparison is repeated here only as the sizing
heuristic that chose the next run's load. One arm's queue diverged and the other's did not, and the
heuristic ordered the two arms the same way the outcome did; it did not establish the cause, because a
request holds the engine past its first token and this evidence does not say by how much. `n = 4` is thin;
the slope method's 0.503 is the nearest independent check and the two agree to 8%.

**The load.** The 0.6 is quoted from the paragraph above this section, written before any card was bought.

| | |
| --- | --- |
| the sizing heuristic | **1 / 2.15 s**, used as a rate to size from. It is NOT a measured capacity — see the withdrawal above — and the load it produced was then validated by running it |
| × the registered 0.6 | **0.279 req/s** |
| trace duration | **`DURATION_MS=505000`** |

**The parameters are the whole tuple, and they were preflighted rather than predicted.** `RATE` is the
**total** arrival rate across tenants, so changing a weight alone moves the premium rate too — and the trace
generator takes a fixed `--seed 11`, which makes every offered count deterministic and checkable before a
card is rented. Running `gen-trace` at the proposed tuple gives:

```
RATE=9.4045  PREMIUM_WEIGHT=1  NOISY_WEIGHT=0.0260  PROBE_WEIGHT=0  DURATION_MS=505000
  -> premium-1      4655 offers  (9.218/s, against the 9.243/s being held)
  -> standard-noisy  139 offers  (0.275/s, 0.59 of the sizing heuristic -- not a measured utilisation)
```

139 contender offers against reading 4b's floor of 100 leaves room to lose **39 of them**. Whether the
queue would stay stable was a prediction from the heuristic, not a derivation from a measured utilisation;
the eighth and ninth pilots then lost none. This replaces an earlier argument on this page
that reasoned about Poisson variance around a mean of 140: the draw is not random once the seed is fixed,
and a count that can be computed should not be argued about.

**The corridor was checked, not assumed.** Lowering the contender's rate also lowers the contention the
control produces, and reading 4 invalidates the run below 5x. At 0.279 req/s the whole-card engine is busy
with contender work about **29%** of the time by the same heuristic -- an occupancy estimate, not a
measurement -- against the 1% a p99 needs before it lands inside such a busy
period. The margin is large but the inference is **not proven**: occupancy above 1% does not by itself put
the p99 above 5x, since an arrival late in a busy period waits only for its remainder. What supports it is
the control's own rows at the old load, where **1,869 of 3,882** premium requests exceeded one prefill time
and **1,414** exceeded two — queueing, not isolated collisions. Queueing makes a no-queue estimate
pessimistic about the control's tail, so the direction is safe. It is still the one number in this
derivation that the next run tests rather than confirms.

**What this costs.** Three arms at 505 s of replay is 25 minutes, plus four engine starts, the cluster and
the driver: about **48 minutes and $0.54**. Four arms is about 60 minutes and $0.68. The runner's credential
check now derives its replay minutes from `DURATION_MS` instead of assuming seven; at this trace it asks for
86 minutes rather than 74.

**~~One measurement worth keeping whatever the next run says.~~ Withdrawn 2026-09-13.** This paragraph said
the whole card "serves the contender at 0.955 req/s" and the time-sliced half at "0.466 — **49%**, almost
exactly half, which is what the topology should cost". Those are the reciprocals of four selected client
TTFTs, and the section above withdraws them: **a reciprocal of a latency is not a service rate**, and this
run located no arm's maximum. The withdrawal was written into that section and this paragraph was left
standing, under a header promising it would survive every future run — so a reader skimming for the
takeaway met the withdrawn number first and the withdrawal second. Two independent reviews found it here.

What the seventh pilot's four requests are still good for is stated where they are derived: a **sizing
heuristic** that told the next run what load to offer, and that was then validated by the run landing.
The premium tail falling from 7,911 ms to 839 ms in that pilot is also not usable as protection evidence:
**74% of the contending work never landed**, which is what reading 4b fired to refuse.

## Budget

Corrected on 2026-09-10 for the platform change, before any card time was bought. The old figures assumed a
self-contained Spot instance the runner did not use; the new ones assume the one it now does.

| stage | cost | gate |
| ----------------------------------------- | ----: | ---- |
| ~~prerequisite: implement this page's readings~~ | $0 | **done.** `internal/bench/sharing_matrix.go` evaluates 4, 4b, 4c, 1, 2, 3 and 5 in that order, dispatched from `benchharness report`; each was deliberately failed to confirm it fires. Writing it is what found defect 9 |
| ~~prerequisite: `hack/m5c-gpu-session.sh`~~ | $0 | **done.** Nine characterization scenarios recorded and replayed, and its GPU-free bring-up rehearsed end to end on a real kind cluster through `hack/test/rehearse-bringup.sh` |
| ~~prerequisite: run the matrix itself off a card~~ | $0 | **done.** `hack/test/rehearse-m5c-matrix.sh` runs all four arms end to end on kind and evaluates the readings over what they wrote. It found defects 13 and 14 |
| already spent | **about $4.85** | NINE pilots, not three and not eight. The first three are the $2.03 this row used to hold: one bought nothing (a defect in this session's own runner), one bought defects 10-12 and a complete `shared` measurement, one was cancelled on a credential margin. The fourth to ninth are **estimated** from each instance's own log span at $0.68/h and are not billed figures. The eighth bought a measurement it could not score; **the ninth is the one that answered the question**, at 75 minutes and about $0.85 |
| pilot: R1, `shared`, `timeSlicing`, `mps`, 1 rep | ~$0.90 | 4, 4b and 4c must not fire, and the load derived and written down |
| — what the eighth pilot actually met | $0.59 | 4 and 4b silent, the load derived and written down. **4c fired for `mps` only**, which this row did not anticipate: it says "must not fire" of a reading whose own name ends "INVALID **for that arm**". A refusal of one arm is a registered outcome for that arm and leaves the other three standing, which is how the run was scored. What it did NOT meet is readings 3 and 5, and only because one repetition has no spread |
| ~~confirmatory: the same four arms x 3 reps~~ | ~~$2.10~~ | **Superseded 2026-09-13.** It budgets an arm set that no longer includes `mps` and a repetition count the answering run did not use. The run that answered this page was **three arms x 2 repetitions**, and it cost $0.85 |
| what remains, if the capacity half is to be bought | ~$1.10 | One session. Not this page's business: the bars, arms and readings here may not change, so a rate ladder needs its own dated pre-registration |
| unspent reserve | ~$1.00 | — |

**~$0.90, and it is lower than the original $1.30 rather than higher.** `g5.2xlarge` Spot is $0.68/h against
the $0.58/h `g5.xlarge` this page first costed, but the EKS control plane at $0.100/h and the NAT gateway
the cluster path implies are both gone. The runtime model is the one fitted to three paid runs in
`2026-09-08-the-load-needs-an-upper-gate.md` — 15 min fixed, 1.5 min per arm, 7 min per arm-repetition at a
420 s trace — with the fixed cost raised to about 25 min, because a kind session installs a driver, a
container toolkit and a cluster where a `docker run` session installs nothing. Four arms at one repetition
is then about 70 min; at three repetitions about 130 min.

**130 minutes does not fit a two-hour backstop, and the confirmatory run is bought as three separate runs of
one repetition for that reason as well as the original one** — an interruption costs one repetition rather
than everything, and repetitions on separate instances put instance-to-instance variation inside the spread
reading 3 uses as its threshold.

The pilot's real gate is not its price. It is that a rented card can only tell us things a cluster cannot,
and everything a cluster could tell us has now been asked: the deployment path is rehearsed, the routing is
proved, and the eight defects above are closed. What is left needs an A10G.

**Spot prices, read from `ec2 describe-spot-price-history` on 2026-09-10 in `ap-northeast-2`, all three
zones:** `g5.2xlarge` $0.680–0.689/h, `g5.xlarge` $0.574–0.596/h. Both in a default public subnet, so
inbound crosses an internet gateway rather than a NAT gateway — which is where earlier paid runs lost
$0.059/GB on 15.6 GB of image and weights. The runner refuses to launch on credentials that would expire
before the run finishes.

## What this run will not be able to say

- **Anything about per-engine GPU utilisation.** Under sharing a busy SM belongs to no engine, and the
  instrument says so rather than guessing.
- **Anything about a bigger card, or two cards.** One A10G, split two ways.
- **Anything about MIG.** The A10G does not support it; a partitioned card is a different mechanism from
  either of these and is not on this page.
- **A general claim that separation cannot protect a tail**, if it does not. One model, one card, one
  arrival trace, two mechanisms.
- **That the difference between the arms is the topology rather than the batch cap.** Added 2026-09-13,
  from a review, and it is the largest alternative explanation this page had not written down.

  The premium tenant's engine runs `--max-num-seqs=64` on the whole card (`config/vllm/deployment.yaml:83`)
  and `--max-num-seqs=32` when split (`config/vllm-shared/engine-a.yaml:67`). The design section argues
  that choice at the level of the **card** — 32 each keeps total admitted concurrency equal across the
  topologies, and per-engine 64 would have given the split arms twice the card's concurrency. That argument
  is correct and it is not the whole story: from the **premium tenant's own side**, the batch cap halved at
  the same moment the topology changed, and the price-of-protection study measured the batch budget moving
  the premium tail by about fivefold — larger than the 1.88x this study's split delivered.

  The direction is arguable and is **not measured**. Premium concurrency peaks at **89 against 64 slots**
  under `shared` (over the cap for 5.8 s) and **52 against 32** under `timeSlicing` (over it for 38.2 s),
  so the split engine is the more oversubscribed of the two relative to its own cap, which would make a
  smaller cap work *against* the split arm rather than for it. That is an argument from two peak counts,
  not a measurement, and this page's own rule is that a quantity may not be described by a cause the ledger
  does not establish. **A `shared` arm at `--max-num-seqs=32` would separate the two and was not run.**

  One thing this does not touch: **R1 peaks at 26 concurrent requests against its 64 slots and never
  reaches the cap**, so the baseline both bars divide by is not affected by the difference either way.
- **Anything about what this costs to operate.** Every cost here is capacity and tenant share, measured on
  Spot for under two hours. It is not an operating cost model.
- **How much of reading 5's price is the mechanism and how much is the duplicated weights.** Added
  2026-09-12, from the fifth pilot's own measurements rather than from reasoning.

  The whole-card engine holds **12.69 GiB of KV, 369,680 tokens, 47.7 contender prompts**. The split pair
  holds 3.2 GiB each — about **6.4 GiB and 24 prompts between them**, half the control's. The card did not
  shrink: each engine carries its own copy of the 5.9 GiB of weights and its own activation and graph
  memory, so splitting spends roughly 7.3 GiB twice where the control spends it once, and what is left over
  for cache is halved.

  The duplication is inseparable from **this arm set**, which is not the same as inseparable in principle:
  a review pointed out that an additional whole-card control deliberately given a 6.4 GiB cache would hold
  the total constant while varying only the topology. That arm was never run and is not budgeted, so the
  contribution is **unmeasured** rather than impossible to measure. An earlier version of this page said
  "no arm could hold it constant", which was wrong.

  Nor is the direction established. This page previously argued that less cache "can only make protection
  harder", so reading 1 was safe from it — but a smaller cache also limits how much contender work is
  resident, which changes the interference itself. The sign of that is not known here. **A reading 5 result
  states the price this topology charged at this cache split, and how much of it is the second copy of the
  weights is not measured.**
- **That a difference between the arms is caused by the topology rather than by the order they ran in.**
  This one is a correction, and it is the clearest thing an independent review told this page that it had
  got wrong.

  Every run measures the arms in one fixed order, always. R1 therefore meets the coldest card of any arm and
  the last arm meets one that has been under load for the best part of an hour, with the device plugin
  swapped between them.

  **At two repetitions that description needed correcting**: R1 in repetition 2 does NOT meet a freshly
  built cluster. It meets the cluster and the card the first repetition left behind, with only its engine
  restarted. The two R1 repetitions differ by 0.179 ms, which says the arm is insensitive to that
  difference and says nothing about whether the other arms are. Any thermal drift, clock behaviour or teardown
  residue is **perfectly confounded with the arm**.

  An earlier draft answered this by pointing at the confirmatory run's three separate instances. That answer
  was about the wrong question. Separate instances widen the *spread* reading 3 compares against, which is an
  estimate of variability; they do nothing about the ordering, because each of the three repeats the same
  sequence. The confounding survives all three.

  It is left in place rather than randomised, and the reason is a trade the page should state rather than
  hide. R1 runs first so that a session cut short after one cell holds the denominator both bars divide by
  instead of a numerator with nothing under it, and the plugin switches are cheaper in a fixed order than in
  a shuffled one. What that buys is recoverability; what it costs is this. **A result here is a difference
  between (arm, position) pairs, and a reader who wants the topology alone would need the order reversed in
  a second run** — which this page does not budget for and does not pretend to.

## What was decided before any data

The bars, the arm set, the reading order, the outcome space including reading 5, the floor applied to every
arm rather than the control alone, the requirement that a pilot derive the load, and the decision to run the
arms in a fixed order with what that costs written down beside what it buys. Recorded here so that
what a later reader compares the results against is this page rather than a memory of it.

**And, from 2026-09-10 and still before any card:** the platform, the instance type, the corrected budget,
which engine settings may differ between the arms and which may not, that the trace's tenant mix is derived
rather than defaulted, and that the readings must exist in code before anything is rented.

Every one of those was decided from arithmetic, from the previous run's evidence, or from a rehearsal on a
free cluster. **None of them was decided from a result of this study, because this study has none.** The
distinction is the whole value of a page like this, so it is worth being explicit: the corrections above are
what a pre-registration is *supposed* to absorb — they were bought by reading and rehearsing rather than by
a card, and they arrived while editing was still allowed. What may not happen after the pilot is bought is
a change to the bars, the readings, or the order they are evaluated in.
