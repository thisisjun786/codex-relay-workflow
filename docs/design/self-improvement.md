# Session-driven harness improvement

Status: proposed design, CRW-1544. The coordinator reviews this design before any
automation is implemented or activated. This document changes no collector,
instruction, model setting, stored format or running service.

The loop is **capture → analyse → propose → ordinary delivery → measure → keep or
revert**. Every observed problem, including a minor one, enters the evidence inbox
when it is noticed. Frequency and cost determine when to investigate it, not
whether to retain it. A successful proposal or a merged patch does not establish
an improvement; the loop remains open until the installed change has observations
and a recorded decision.

## What LOOP1017 established

The first manual pass provides examples and counterexamples, not a measured
before/after experiment. The source set was frozen on 2026-10-17 at 05:13 UTC.
Private source locators and SHA-256 digests are retained with the CRW-1544
coordination report. The following aliases identify those snapshots; only
sanitized descriptions belong in this repository.

| Citation | Source and scope |
| --- | --- |
| `L-scan` | `sessscan-1.txt`, the saved parent/child scan, and `sessscan.py`, the scanner source. The saved file's mtime is 13:25 KST; that is not a recorded event-coverage cutoff. Its storage directory and the script's default threads alone do not identify which trial was scanned. |
| `L-r4/O<n>` | R4 observations in `findings.md`. O1–O15 are the brief's first-pass cases; O16–O18 are explicitly later follow-up observations in the captured file. |
| `L-route` | The LOOP1017 coordination note and the captured issue descriptions for CRW-1609, CRW-1610 and CRW-1614. |
| `L-review` | CRW-1552's review-audit draft, including its coverage limitations and its separate counts of intervals, verdicts and findings. |
| `L-deviations` | The rehearsal deviations ledger, cited below by section/time rather than by private session identifiers. |

### What the scan actually found

`L-scan` records 349 child command-execution items, three nonzero exits and 3,696
seconds of summed command durations. Its largest normalized command group contains
162 `flow next` invocations. Another three-invocation group also contains `flow
next`; **162 is a group count, not the total number of polls**. The normalization
keeps single-digit numbers, replaces longer numbers and hexadecimal strings, and
cuts commands to 160 characters. It can both split equivalent commands and merge
different commands. The saved parent scan reports 145 command items and seven
seconds of summed durations. These are observations about this snapshot, not CPU
time, elapsed lane time, wasted time or a population failure rate.

The child scan also exposes repeated status/CI probes, repeated answer/watch
commands, and waits among the slowest commands. Two of its three nonzero exits
are `flow dispatched` reporting that binding is not ready yet. R4-O7 identifies
that retry as documented behavior. The third is a compound shell command; its
printed output tail does not isolate a product cause.

Both scans report zero shell `sleep` commands, although their item-type summaries
contain native sleep items: nine for the parent and three for the child. A rule
that reads only shell text misses those waits. The scanner sums a missing
duration as zero, inspects only top groups/slow commands/first failures, and prints
raw error tails. It is a useful discovery tool, not a complete cost ledger,
privacy filter or causal classifier.

### What required session and contract review

| Evidence | Finding and qualification | First-pass disposition / measurement target |
| --- | --- | --- |
| `L-r4/O1` | Repository absence was discovered after preflight and preview. The caller also omitted the documented repository argument. | CRW-1613; distinguish caller error from late validation, measure avoidable starts before rejection. |
| `L-r4/O2`, `O6`, `O10`; `L-scan` | Progress language differed from the packet; short waits and a custom driver repeated polling. The claimed host wait limit was not verified. | CRW-1614; measure actual host wait behavior before prescribing a replacement driver. CRW-1616 is the later execution-mode measurement follow-up, not evidence that a fix already worked. |
| `L-r4/O4–O5` | The first three completed explorers required repair for missing `verified`. The packet declared it required. Missing host schema enforcement was a hypothesis, not established by these observations. | CRW-1611; first-turn schema compliance and repair attempts, separated by adapter/model/schema digest. The reported repair token volume was mostly cached; it is not a monetary cost estimate. |
| `L-r4/O9`, `O15` | An already-read and acknowledged report was delivered again, costing another parent turn. | CRW-1612; join report identity, acknowledgement, delivery and consuming turn. Repeated prose alone cannot prove duplicate delivery. |
| `L-r4/O11` | Close-fix requested research on an executed path; its packet promised a continuation while the close handler stopped. | CRW-1610; reproduce the packet/handler mismatch, then measure close-stage research completion and escalation. |
| `L-r4/O12` | A gate before publication was asked to prove remote integration and issue closure that occur after publication. The reader also lacked the required remote evidence. | CRW-1609; classify criterion timing and evidence availability, rather than weakening the reviewer until it approves. |
| `L-r4/O3`, `O8` | Reused trial setup and an already-green behavior in a red-first criterion caused preparation conflicts. The parent's policy answer was a normal product path. | Improve trial/issue preparation; do not label the setup conflict a runtime defect or the parent answer a human intervention. |
| `L-r4/O7`, `O8` follow-up, `O10` | Binding retry and draining outstanding work were documented behavior. | Negative examples for the detectors. A nonzero exit or a long wait is insufficient to open a defect. |
| `L-r4/O13–O18` | A claimed drain duration lacked timing evidence. The initial no-intervention observation later changed to an operator intervention; rerunning close exposed receipt-reuse and delivery-evidence gaps. | Preserve the original claim and later correction. An unfinished run's interim zero is not a completed, intervention-free success. Later findings do not retroactively become findings of the first scan. |

The first pass produced CRW-1609–1614 and the measurement follow-up CRW-1616.
That is evidence of collection, triage and routing. It did not establish savings,
fix effectiveness, a final r4 success, or coverage of Claude Code and Hermes.
The saved scan mostly sees the parent's and child's tools; nested agent calls,
commands inside a driver, semantic contract mismatches, repair cost, and empty
reviews require other evidence.

`L-deviations` adds two important failures: the 17:29 critic entry records
approvals without repository reading and notes that a turn-count cutoff also
rejected legitimate one-turn reviews; the 19:28 validation-pass entry records
unsupported claims about searches and commits. The R1 08:38 entry cites nonexistent
files/tests. `L-review` consequently keeps `NO_VERDICT`, tool failures, repeated
findings, confirmed fixes and unlinked evidence separate. The loop must not
optimize approval rate or token volume by making those distinctions disappear.

## Existing components and the new work

The implementation remains in the existing Go CRW stack. The manual Python
scanner is a source of patterns and fixture expectations, not a new runtime
dependency.

| Existing asset | Reuse | Boundary / required addition |
| --- | --- | --- |
| [`improve collect`](../../internal/manage/improve_collect.go) | `crw-improve-bundle/1`; configured relay/DAG, audit, intervention, draft and exported-issue inputs; source identity and output-collision checks. | It does not already collect every agent transcript. Configure job-owned consistent snapshots; add agent adapters and a separate, versioned observation envelope through an approved implementation. Missing and unreadable inputs remain distinguishable. |
| [`improve propose`](../../internal/manage/improve_propose.go) | Audit draft fingerprints, origin/sighting deduplication, issue-list suppression, locks, `max_new_drafts`, dry-run report and residual candidates. | Current ranking is count, then impact, then estimated cost; that cost is affected-project count, not tokens or money. A singleton can already be a candidate. New session findings need an explicit adapter into this machinery, not a second incompatible draft index. |
| [`improve run`](../../internal/manage/improve_run.go) | Collect/propose/roadmap at a milestone or project boundary, with a boundary/ref lock. | It starts no timer and performs no patch, PR, deployment or experiment. A repeated boundary/ref creates no new drafts; handle `remaining` through the saved bundle's propose path and the budget. Do not invent refs to reset the cap. |
| [Trace, shadow and measure](../flow/measure.md) | Offline `crw-flow-trace/1`, `crw-flow-measure/1`, existing acceptance/schema/recovery/delivery measurements, and replay comparisons. | Export only from a consistent SQLite backup or `VACUUM INTO` copy. Keep live-path/alias refusal. Shadow's invented calls and everything after them are not effectiveness evidence. Add missing cohort, activation, cache-usage and intervention joins separately. |
| Rehearsal JSONL, deviations and review audit | Seed cases, expected retries, known false positives, review-to-fix links, correction history. | These are private historical inputs. Their different units and coverage cannot be pooled into one detection rate. |
| Publication policy and ordinary delivery | [`publishpolicy`](../../internal/publishpolicy/scrub.go), publication preparation, independent review, the configured verifier, integration and installation authority. | Add a cycle coordinator, source checkpoints, campaign/decision receipts and an authorized proposal publisher. None of those additions are activated by this document. |

Keep the existing schemas unchanged in this work. The observation and campaign
records below describe proposed new contracts; current commands do not accept
these new records or hypothetical flags. Their versioning and migration are
part of the separately approved implementation.

## Collection and privacy

Collectors take allowlisted source descriptors, adapter/version, an input cursor
and a job snapshot destination. They return immutable snapshots, normalized
events, coverage diagnostics and a next cursor. Commit the cursor only after
durable snapshot/manifest writes. Retry, file rotation, partial final JSONL lines,
compaction and an interrupted collection must not duplicate observations or
silently discard an unread suffix. A malformed completed record is quarantined
with its location and a coverage error; it is not a successful empty session.

| Collector | Read and correlate | Limits to preserve |
| --- | --- | --- |
| Codex | Authorized rollout JSONL or read-only App Server `thread/read` results; turn/item/call identity, tool start/result, command duration/exit, assistant output, schema repair and usage. Record source form explicitly. | App Server items and rollout events are different formats. Link tool calls/results and continuations; do not create or steer turns to collect them. Pending commands are neither success nor failure. |
| Claude Code | Configured session JSONL; message/parent lineage, assistant usage, tool-use/tool-result ids, subagent relationships, model and completion. | Streaming copies and cumulative usage are not additional calls. Compacted/forked history needs origin identity; a repeated transcript is not a new billed request. |
| Hermes | A versioned adapter for the installed session export/store, selected by the source descriptor. Extract the available role, message/tool identity, timestamps, model and usage into the same envelope. | The deployed format must first be identified and covered by fixture samples. Do not assume a Codex-shaped JSONL or guess a live database schema. For SQLite use a consistent backup; unsupported fields remain unknown. |
| CRW flow | Trace export from a store copy plus explicit call, stage, report/delivery/ack and acceptance relationships. | Join by recorded identity, not similar text or process names. Trace scopes differ: store-wide delivery rows are not automatically a run's rows. |
| Reviews and integration | Verdict body, pass/round, reviewed head/tree, criteria/schema digest, tool-read evidence, fixes and CI run/attempt/job/leaf failures. | Distinguish missing verdict, tool failure, superseded review and a valid zero-findings verdict. Preserve first failed attempts after successful reruns; multiple failure lines need not be multiple defects. |
| Operator interventions | Explicit management/control-plane records: actor, authority, action, reason, before/after state and affected run/node. | `userMessage` does not prove a human acted. Normal parent policy decisions, pre-run setup, required approvals and out-of-band rescue are separate classes. Record human versus coordinator actors. |

Each proposed observation carries: stable observation id; source adapter/version;
snapshot digest and byte/line/sequence reference; private origin identity and a
safe alias; role and task/run/node/attempt relationships; source and observation
timestamps; installed runtime, workflow, instruction and model-profile digests;
event kind/result; measured values with units and availability; redaction
version/counts; and links to corrections.

Observation identity uses an original event/request id where available. Otherwise
use a persisted source-generation id plus record offset and content digest. The
whole snapshot digest is provenance, not the identity of every event: appending a
log must not make all earlier observations new. Two identical commands at distinct
positions are separate events. An ambiguous cross-source match is a coverage gap,
not permission to guess and merge.

There are three visibility levels:

1. **Private capture.** Raw snapshots and the alias map stay outside the repository
   in owner-only storage. Store source roots by configuration, never by copied
   host-specific paths. Snapshot the relay using its supported consistent-copy
   route; no plain copy of a changing SQLite/WAL set.
2. **Analysis capsule.** Before any model call, select the relevant evidence and
   scrub credentials, private paths/hosts, session/thread/call ids, identities and
   unrelated user text. Build the redaction context from the approved source
   roots and private identities of every contributing host; the collecting
   machine's host context alone is insufficient for remote or historical logs.
   Join identities privately first; use stable keyed aliases
   within the approved scope so replacing every id with one placeholder does not
   erase relationships. Keep the reverse map private. Secret detection quarantines
   the affected capsule; metadata-only diagnostics may continue for other cases.
3. **Publication.** Prepare titles, bodies and evidence excerpts with the installed
   publication policy before an authorized issue/PR write. Scan the patch itself
   too. Preparation failure blocks publication; no raw stderr tail is a fallback.
   Publish only sufficient sanitized examples and public code/issue references.
   `L-scan`'s normalization is never used as the scrubber.

Captured text is untrusted data. A session can contain instructions to change the
analyser or bypass a gate; the analyser has no write credentials, and a patch
worker receives an explicit target/authority manifest rather than commands taken
from the transcript. Enforce path and capability restrictions outside the model.
Reject workspace escapes and edits to active linked skill sources. Log failures
with aliases, categories and offsets, not matched secret values.

Retention applies only to collector-owned copies. Pin evidence while its proposal,
measurement, incident or appeal is open. A proposed default is eligibility for raw
copy deletion 30 days after those holds end, and 90 days for unpinned analysis
capsules; retain decision manifests and sufficient sanitized evidence. Original
session stores are never deleted by this collector. Activation must authorize
retention separately and account for any stricter owner policy.

## Analysis and proposal rules

The deterministic pass runs on every newly captured event. Model analysis is a
bounded second step for unresolved interpretation. Every finding distinguishes
**observed behavior**, **candidate cause**, **verified cause**, **expected behavior**
and **insufficient evidence**, with the source locations and the check that could
resolve an uncertainty. Severity does not turn a hypothesis into a fact.

| Rule | Candidate trigger | Required discrimination before proposing a fix |
| --- | --- | --- |
| Repetition without progress | The same parsed operation/target occurs three times with unchanged authoritative state. Keep every occurrence and the operation's full private identity. | Exclude documented wait/binding retry and scheduled health checks. Compare event/sequence progress; inspect nested driver calls separately. A normalized 160-character prefix is only a search hint. |
| Failure and slow-stage triage | Any terminal nonzero command, failed call, or stage among that scope's slowest recorded stages. A single minor event enters the inbox. | Use the operation's documented exit semantics. Attribute time to active work, waiting, queueing or unknown; a long CI wait is not evidence of a slow model or CPU waste. |
| Repair churn | Any schema repair, grouped by schema/adapter/model and the missing or invalid field. | Inspect the first answer, actual delivered contract and validation/repair event. Correct normalization only when it preserves the contract; never make a required field optional merely to reduce repairs. |
| Duplicate reports / answer lag | Same report delivered after a recorded acknowledgement, or an answer accepted without a matching application event. | Join the exact report/decision/run/revision; check event ordering and authoritative state. Accepted is not applied. Missing timestamps mean duration unknown, as in O13. |
| Instruction or contract divergence | Action differs from the instruction/packet actually available to that actor, or packet continuation disagrees with the executed handler. | Separate unread/undelivered instructions, setup errors, expected behavior and model choice. Verify the relevant code path before declaring a runtime cause. Language analysis excludes code and quotations. |
| Hollow verdict or unsupported evidence | Approval/rejection without required source proof, placeholder verdicts, or citations that cannot be resolved at the reviewed revision. | Check supplied evidence as well as tool reads. One turn or few tokens is not inherently shallow. Validate cited files/tests/lines and the actual criteria/head; keep `NO_VERDICT` separate from valid zero findings and substantive revise rounds. |
| Criterion timing / escaped defect | A criterion requires a later stage, or CI/operation contradicts a review's claimed result. | Distinguish absent evidence from false behavior. A later diff or a different root cause is not a miss by the earlier review. Read all failed jobs/leaves, not only the first failure. |

A proposal records one falsifiable hypothesis, its target (instruction, skill,
configuration, code or storage), affected versions, positive and negative examples,
verified evidence and remaining uncertainty, expected primary effect, protected
quality conditions, exact edit scope, owner, verification plan, exposure plan and
rollback target. Trace the originating observation through issue/draft, reviewed
candidate, integration commit, installed digest and measurement decision.

Keep repeated sightings behind the existing draft fingerprint; do not count every
rescan, duplicate wrapper result or reviewer rephrasing as a new defect. A posted
draft's historical content stays historical, as documented in
[CRW-1055](../port-cxc/known-defects/CRW-1055.md); new evidence can grow its private
lineage. Retain capped candidates with their continuation path. Confirmed urgent
safety failures go directly to the authorized coordinator path, rather than waiting
behind a high-frequency nuisance in the ordinary ranking.

## Authority and application

“Automatic” describes preparation within a previously authorized campaign scope.
It does not grant the analyser release, installation, tracking-write or policy
authority. The following ceilings are checked from the actual change, not its
filename or an author's claim that it is wording only.

| Change | Automation ceiling before a new coordinator decision |
| --- | --- |
| Clarification of existing instruction/skill wording, examples or navigation, with unchanged obligations and authority | Prepare an isolated patch, run ordinary checks and independent review, and open a draft PR under the authorized repository/project scope. No automatic merge, installation or expansion of live exposure. |
| Runtime code, configuration, model routing, thresholds, stored formats or migrations | Produce a grounded proposal first. Coordinator approval of the edit scope, test and rollback plan precedes patch execution. Then use ordinary delivery. |
| Approval rules, permissions, security boundaries, required review/verification, this loop's rules, sample selection, metrics, scoring or budget controls—even in Markdown | Always the preceding coordinator-approval path. The loop cannot improve its score by weakening its judge or increasing its own authority. |

An uncertain classification takes the approval path. Code/schema work must not be
smuggled through a skill script or an “editorial” edit. Ownership conflicts are
queued rather than fixed in another task's files.

Use [repository policy](../../POLICY.md) and its configured integration route.
For an automatic PR here, configure the improvement campaign as a PR-unit project:
the owning project session prepares its milestone branch and opens the review
artifact; the dev queue retains integration authority. A direct-landing project
does not acquire a second PR or a new gate merely from this design. The coordinator
must choose an authorized PR-unit scope before the automatic-PR path is enabled.
Failed checks and unresolved required findings never become approvals to meet a
cost budget.

Patch in a worktree that is not an installed skill's active source. Record source
integration and live activation separately. A campaign starts measuring only after
an authorized installation/activation receipt identifies what new sessions really
read. Mixed versions, mid-session instruction changes and sessions pinned to the
old runtime are reported separately, not assigned to treatment by branch HEAD.

Persist the cycle transitions and their receipts:

```text
captured → triaged → proposed → authorized scope → patch/review
         → integrated → activated → measuring → keep | revert | inconclusive
```

Expected behavior and insufficient evidence also have explicit terminal/deferred
triage states. Record an external-write intent before publication and its result
afterward. On restart, reconcile the same repository, proposal fingerprint and
candidate revision before retrying; do not blindly create another issue or PR.
The cycle coordinator checkpoints this flow; existing `improve run` is not this
state machine.

## Measuring effect

Freeze a campaign manifest **before activation**: hypothesis, scope and target
stage, one primary metric, protected metrics, baseline and candidate digests,
eligibility/matching rules, enrolment order, sample sizes, time boundary, data
coverage rules, cost ceiling, approval and rollback authority. Parser, rubric or
threshold changes create a new prospective manifest; do not rewrite an old result.

The initial pilot policy below is proposed for approval, not a value inferred from
LOOP1017 or an existing product default.

- Compare 20 consecutive eligible baseline lanes with the next 20 eligible lanes
  **started** after activation. Define eligibility from the planned task/stage
  before execution, not from whether the lane eventually reaches that stage.
  Record actual exposure, but keep early failures in the enrolled cohort and
  quality denominator. Match non-target controls: workflow/task class and size,
  agent/adapter/model effort, verification mode and relevant cache/load conditions.
  The factor intentionally changed by the campaign is recorded as treatment,
  not matched away. Register other simultaneous changes. Do not select only fast
  or successful finishes.
- Use seven days to enrol each cohort and a further seven days for its last
  enrolled lane to reach a terminal outcome. This is a measurement boundary, not
  a timeout that kills work. Failure/cancellation remain outcomes; still-open
  lanes remain censored. Fewer than 20 lanes, censored outcomes, missing primary
  or mandatory protected measurements, or a material unmatched configuration
  change make the pilot inconclusive. Lane time, rounds, interventions, token
  totals and quality are protected by default; optional detail such as cached-token
  subsets may be unavailable if it was not chosen as a primary/protected measure.
  Do not fill gaps with zero or extend the window until it passes.
- Where isolated concurrent baseline/candidate execution is supported and approved,
  preassign matched lanes to the two versions independently of their results.
  Otherwise the sequential comparison is observational and confounded by time,
  workload and environment; report that limitation. Neither 20 samples nor the
  decision rule below establishes statistical significance.
- Use the same denominators on both sides. Publish counts, distributions, missing
  counts and exposure provenance alongside any ratio. No unobserved agent family
  or task class inherits the result.

| Metric | Definition and denominator |
| --- | --- |
| Lane time | From the enrolled lane's first product start to successful delivery. Report successful-lane p50/p90 with that denominator, plus all 20 outcomes and censored/failing counts. Keep queue/active/wait components separate when measured; never sum overlapping command/agent durations into elapsed time. |
| Correction rounds | Substantive revise→fix attempts per enrolled lane, including those on failed lanes. Separately report tool/schema-repair attempts and `NO_VERDICT` retries. Re-reading a verdict is not another correction round. |
| Operator intervention | Distinct out-of-band rescue episodes per enrolled lane, with actor and reason. Count a linked intervention once across messages; show human/coordinator rescue, normal parent decisions, required approvals and trial preparation separately. O8's normal parent answer does not count as rescue. |
| Tokens | Input total and output per enrolled lane, including failed/retried calls and the relevant parent/subagents; also report cohort totals per successful delivery. Normalize provider usage semantics first, retaining cached-input subsets separately. Do not sum cumulative usage snapshots or both provider and flow copies of the same call. Missing usage remains unknown. |
| Quality and target symptom | Terminal failure/cancellation rate, missing/invalid verdicts, confirmed escaped defects, publication/authority violations, and the proposal's specific symptom numerator over the opportunities that could exhibit it. Keep expected retries out of the defect numerator. |
| Loop overhead | Collector CPU/time/bytes, analysis/proposal/review calls and tokens, CI attempts, storage and operator triage time, identified separately from the measured product lanes. Subscription quota use is not a dollar invoice; calculate money only with known provider usage and a versioned tariff. |

Reuse `flow measure`'s published meanings rather than relabeling them: first
acceptance is not successful delivery; tokens per accepted issue omit the cost of
other unsuccessful nodes; thread reuse is not cached-token volume. Its trace has
no cached-input tokens, and the documented `via_ocx` grouping is a model-name
heuristic, not universal transport proof. Supplement only from an identified
source and show availability.

For a lower-is-better primary metric with a positive baseline value `B`, the pilot
recommends **keep** only if candidate `T ≤ 0.90 × B`, no new confirmed
quality/authority failure exists, the terminal failure count does not increase,
and measured non-primary time/token/correction/intervention summaries do not
exceed `1.10 ×` their baseline values. Compare p50 and p90 where distributions
are used. A zero baseline requires zero new occurrences for that protected metric;
it cannot support a relative-improvement claim. A primary baseline of zero is
inconclusive unless a different absolute criterion was registered beforehand.

After a complete, comparable pilot, no qualifying primary improvement or a
protected-metric regression recommends **revert**. Inconclusive data freezes
expansion and goes to the coordinator; it is not a keep result. The coordinator
can retain a safety fix for a recorded correctness reason rather than treat
cost reduction as its purpose. Such a disposition is not reported as measured
performance success.

A confirmed secret leak, unauthorized action, acceptance without required evidence,
or attributable correctness regression immediately pauses the campaign and new
exposure; it does not wait for 20 lanes. Preserve the failing evidence. Exercise
the approved rollback: revert the campaign's owned change through ordinary review
and verification, then restore the approved live version through its authorized
activation route. Never reset a shared branch or undo another task's changes.
Migration/data compatibility, overlapping changes or a rollback that restores a
known safety defect require coordinator resolution. The loop has no implicit
authority to downgrade data or overwrite a live installation.

## Cadence, budget and implementation order

Proposed pilot limits: incremental collection at terminal-session/boundary events
and a five-minute sweep; one collector worker; at most 64 MiB of new input per
adapter per sweep, checkpointing the remainder. A noticed minor defect enters
the inbox immediately. Collection is deterministic and needs no model. Use
consistent snapshots, not repeated full live-thread downloads.

Run deterministic triage after each capture. Batch model interpretation for at
most three unresolved candidate packets per day, with a proposed aggregate budget
of 250,000 input tokens and 25,000 output tokens for analysis/proposal work. Record
cache-aware usage without counting a subset twice; reserve a call's configured
bound before launch and pause new calls if usage is missing or a bound is exceeded.
Start with the configured inexpensive proposal profile. An independent configured
reviewer checks consequential claims; escalate only an item that fails validation
or is refuted, rather than raising the entire batch's model tier.

Start at most one patch/pilot per interacting configuration scope at a time.
Admit at most one new patch-review/CI delivery per day during this pilot; its
normal required gates and permitted failed-job retry still apply. Record that
review/CI cost separately from the analysis token budget. Budget exhaustion queues
work with its evidence and age; it never skips a required check or discards
low-severity findings. Critical failures use the existing coordinator incident
path. Keep a daily pending/coverage/cost digest and a weekly effect decision.
These limits are prospective campaign settings and need coordinator approval
before implementation or adjustment.

Deduplicate jobs by snapshot/cursor, rule version and campaign scope. Persist
pending work across restarts. Do not feed this loop's generated summaries back as
new source incidents; track its overhead explicitly. Re-render reports from the
captured data, not a later reread of mutable sources.

Implement in separately reviewed increments:

1. **Capture and privacy:** source descriptors, supported-version fixtures,
   cursors, safe snapshots, aliases, coverage errors and secret quarantine.
2. **Analysis and proposal linkage:** the rules above, positive and expected-behavior
   examples from LOOP1017, citations bound to snapshots/revisions, and reuse of
   the existing draft/index/remaining machinery.
3. **Authorized delivery:** classification and authority manifests, isolated
   worktrees, draft-PR reconciliation, ordinary verification/integration and
   separate activation receipts.
4. **Measurement and rollback:** fixed cohorts, exposure attribution, missing-data
   accounting, the protected metrics and an exercised rollback path.

The first operational proof should run one consented, bounded campaign end to end.
Its acceptance scenarios include partial JSONL/rotation and duplicate snapshots;
a secret and an injected instruction in captured text; the normal dispatched
retry; a real repair and an unsupported verdict; a cap-excluded candidate surviving
restart; a crash after remote publication; a source commit with no live activation;
mixed-version or missing-token cohorts; a regression and a rollback conflict.
Synthetic replay proves the parser and decisions in its scope. A real authorized
pilot is still required to claim reduced lane time, intervention or tokens.
