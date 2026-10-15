---
name: crw-check
description: "Check delivered work against the canonical tracking documents and issue criteria, find contradictions between specifications and evidence, and judge progress against the schedule. Use for requirement drift, completion and invariant audits; crw-run executes, crw-qa drives a surface by hand."
---

# CRW Check

Here the parent (repository session) and the child (project session) are layers of [the level table](../crw-canon/references/hierarchy.md#the-levels).

Check delivered work against what was agreed, using the tracking repository as the canonical source of what was agreed, and find the
claims or behaviors that cannot both hold. The check is read-only until an existing execution workflow authorizes
more, and it runs in one of three ways. Read the section for the way this call is made; the sections before them are
the rules all three share.

| Actor | Called as | Section |
|---|---|---|
| The user, or a parent or child asking for an audit | a standalone check | [Standalone audit](#standalone-audit) |
| A child or parent completing a delivery inside an authorized execution workflow | the in-workflow auditor | [In-workflow auditor](#in-workflow-auditor) |
| A task that called this skill as a sub-task | a bounded helper | [Bounded helper](#bounded-helper) |

## Connect the workflow

Read the shared [tracking model](../crw-canon/references/tracking-model.md) and [delivery](../crw-canon/references/delivery.md), including its [completion-follow-up rule](../crw-canon/references/delivery.md#completion-follow-up-in-an-existing-execution-workflow). Use `gh` for current issues and decisions, `crw-recall` for missing historical decisions, and `crw-dev` plus the relevant review/verification owner (`crw-dev-code-reviewer`, `crw-qa` for a driven surface) for code evidence. A standalone audit stays read-only; an existing execution task retains its authorized scope when this skill is used to check delivery.

When the comparison is ambiguous, restate it and confirm the reading. Where documents state the same fact differently, [where each document type is canonical](../crw-canon/references/tracking-model.md#where-each-document-type-is-canonical) decides which one is the rule. Check that acceptance evidence does not just confirm the implementation's own assumptions. For an actual contradiction, use [Find contradictions](#find-contradictions) below; it returns findings without replacing this requirement audit. When a substantial handoff needs a fresh reader, give it to a context-free reviewer through helper delegation (`crw role helper dispatch`).

## Pin the comparison

Identify the milestone and its issues, canonical documents, original intent, subsequently accepted changes, and implementation revision. Read full criteria and relevant decisions, not just list summaries. Record exact refs, source dates/revisions, baseline/final commits or a frozen diff bundle, and the observed delivery environment.

Whether a relay holds the assignment is determined and recorded once, not decided again here: see [Whether a relay holds this assignment](../crw-canon/references/delivery.md#whether-a-relay-holds-this-assignment). Where one does, pin the comparison to the revision it reports as current and register the issue's criteria as the canonical set; [Check records in a relay store](../crw-run/references/check-records.md) holds how a review binds to a revision and to that set.

Treat memory and assistant proposals as locators. A newer document does not automatically override a user decision. A later explicit user correction or accepted decision can supersede the original requirement; cite that evidence. If authority is unclear, preserve both readings as unresolved rather than selecting whichever matches the implementation. Which side is canonical for the fact being compared, and whether a disagreement is a requirements change or an implementation mismatch, follows [Keep the two sides consistent](../crw-canon/references/tracking-model.md#keep-the-two-sides-consistent).

Inspect relevant code, tests, and result artifacts. Separate source existence, passing checks, integration, deployment, and observed behavior. If a live check is outside the authorized environment, leave that part unverified.

When the claim includes PR readiness or integration, assess CI and review evidence using [Merge readiness](../crw-run/references/merge-readiness.md). Entering review and being merge ready are different states owned by OPS-9.1 in [Operations contract](../crw-run/references/operations.md); judge the claim against that clause rather than reading a pull request's draft or ready flag as a verdict. Reuse valid coordinator evidence and return any gaps to that owner; a bounded audit does not take over the merge.

For implementation completion, use the shared
[Implementation Done](../crw-canon/references/tracking-model.md#implementation-done).
Match the issue to its current delivery, intended integration target and actual
merge evidence, or the reconciled legacy PR set and its combined issue coverage.
Preserve existing accepted operational criteria. Contrast partial, reference/replacement, wrong-branch and
closed-unmerged cases with a complete intended merge. An implementation merge does
not prove deployment; a verified non-PR result needs no placeholder pull request.

## Compare criterion by criterion

Break compound criteria into observable obligations and classify each:

| Classification | Required basis |
|---|---|
| Satisfied | Evidence at the relevant revision and required delivery level |
| Agreed change | Traceable accepted change and evidence for the revised obligation |
| Explicitly deferred | Recorded deferral and remaining condition or later scope |
| Missing or regressed | In-scope obligation contradicted by code or reproduced behavior |
| Unverified | Insufficient access/evidence, ambiguous authority, or an untested required surface |

Include significant implementation additions without agreed scope. Explain their impact without declaring every addition a defect. Do not count deferred work as completed, call absent evidence a proven defect, or rewrite criteria to make the result pass.

When recording the result, the three recordable per-criterion dispositions are verified, needs_changes, and unverified, and every classification above maps to one of them. Satisfied maps to verified. An agreed change maps to verified too, but only once the registered criterion itself carries the accepted wording. Missing, regressed, and unagreed-scope findings map to needs_changes with a note; insufficient access or authority maps to unverified with the reason. An explicitly deferred obligation is recorded as unverified with its deferral, never as verified. Leaving a required criterion unmapped is not neutral: a managed `verified` verdict needs every required criterion recorded verified and is refused otherwise. Stopping an assignment is aborted, which is an overall verdict recorded with its reason and never a per-criterion disposition. How a relay store binds a review to the criteria set, repeats a claim and refuses a verdict is in [Check records in a relay store](../crw-run/references/check-records.md#map-the-dispositions-to-the-registered-criteria).

## Verdict words and the reviewer grammar

The reviewer's `LAUNCH` and `VERDICT` lines are defined in `crw-dev-code-reviewer` and consumed by the PABCD observer; this skill's dispositions are the vocabulary of a criteria audit. One table says how they correspond, so a reviewer's line and a check's disposition are never read as two different answers to the same obligation.

| Check disposition | Reviewer grammar | Meaning |
|---|---|---|
| verified | `VERDICT: PASS` | Every obligation in scope is met at the reviewed revision and delivery level |
| needs_changes | `VERDICT: FAIL`, or `VERDICT: GO-WITH-FIXES (blockers=N)` for the criteria its N blockers name | An in-scope obligation is missing, regressed or outside the agreed scope; the criteria carrying no blocker stay verified |
| unverified | no `VERDICT` line, or a reviewer that could not read the evidence | Access, authority or evidence is missing, or the obligation was deferred; it is never promoted to `VERDICT: PASS` |

The schedule verdict is a separate field and never a row of this table. A code review's `VERDICT` does not certify delivery against the tracking issue: it is evidence this skill reads, as a `crw-qa` pass is.

## Find contradictions

Actors: the top session, the parent, the child ([the level table](../crw-canon/references/hierarchy.md#the-levels)). Each reads and reports; a task that runs this as a sub-task returns its findings to its caller, which writes any record (see [Bounded helper](#bounded-helper)).

Find claims or behaviors that cannot both hold under the same stated conditions. Anchor the rules and accepted decisions in canonical tracking documents, and return reproducible contradictions or precise evidence gaps, not a list of dislikes.

If only a local file or pasted artifact is supplied, audit that bounded input and label its canonical tracking link unknown. Missing tracking access can yield a partial audit; it cannot establish cross-document consistency. Do not create or migrate documents simply to run the check.

Run the checks that answer the question at hand:

| Question | Check |
|---|---|
| Did we understand the request and its scope? | When materially ambiguous, restate the request and its scope from the conversation and records and confirm the reading before auditing; ask only when the evidence cannot settle it |
| Do documents disagree about the same fact/rule? | List every document that states it, then settle which one is the rule by [where each document type is canonical](../crw-canon/references/tracking-model.md#where-each-document-type-is-canonical); an accepted decision recorded on the item supersedes the body it changes, and the others are stale copies or a difference of scope. If two sources that are each canonical for their own scope still require incompatible behavior, that is the finding: report both sources and do not pick one |
| Is a factual premise unsupported or false? | Trace the premise to its source and verify it there; use `crw-search` for current/public sources, and report an unverified premise as an evidence gap |
| Does an evaluation independently test its claim? | Ask whether the implementation and the scorer could agree without any outside truth; acceptance evidence must come from something other than the implementation's own assumption |
| Would a fresh reader recover the same rule? | When a blind comprehension check is needed and delegation is available, give the text without its surrounding context to a context-free reviewer through helper delegation (`crw role helper dispatch`) and compare the rule it recovers with the intended one |

Do not relabel a same-context read as an independent check. For code, use `crw-dev` and the relevant verification owner; `crw-loop`, `crw-pabcd` and the helper role policy own authorized implementation, loops and helper routing, and this audit starts neither a loop nor a competing workflow.

Pin assumptions, units, time windows, population/scope, state/version and decision authority before judging. Distinguish approved requirements from proposals and implementation choices, and derive invariants from those contracts or independently justified facts, not from the function being tested. State a suspected contradiction as: **given these conditions, rule A requires X, while claim or behavior B requires not-X**. If conditions differ, investigate that mismatch before declaring a contradiction. Missing evidence, uncertainty, a tradeoff, a documented exception and a changed requirement are distinct outcomes. Use [Counterexample patterns](references/counterexamples.md) when selecting a numerical, state-transition, evaluation or cross-document probe, and apply only the patterns the actual contract justifies.

Try to disprove the suspicion: find the smallest case that distinguishes a violation from a valid exception, inspect or run it in the authorized environment, and record input, expected relation, actual result and source or artifact. A static code proof may establish a violation but must not be reported as live execution. Keep diagnostic writes in a task-owned temporary location when permitted. Pair a suspected failure with a valid contrasting case, and consider whether a correction would reject normal retries, erase uncertainty, clamp unrelated quantities or change historical semantics. A passing pre-existing suite does not prove an untested invariant, and a second model agreeing with the first is not physical or business ground truth. If inputs, access or observations are missing, report the evidence gap and the cheapest resolving check; do not manufacture a failure to satisfy the audit.

Report consequential findings ranked by impact, each with the conflicting rules and tracking source anchors, the shared conditions, the minimal counterexample, the valid contrast, the consequence and the smallest correction or next check. Mark the evidence as **reproduced violation**, **static contradiction** or **unverified hypothesis**, and keep these apart from an apparent contradiction resolved by a difference of scope or an accepted change. When none is found, state the examined scope and the remaining evidence limits: missing verification cannot become a universal pass. Stop when the bounded questions are answered; more loops need a new question or invalidated evidence.

Reuse valid proof; do not rerun a broad suite just to make a report look thorough.

## Judge progress against the schedule

Where the checked subjects, which the contract defines as projects, milestones and issues, carry agreed dates, compare them with delivery evidence as part of the same check. Pin the Schedule baseline, the Current target, the observation time, and the Timezone alongside the identifiers already pinned above, reading them from the [Schedule baseline contract](../crw-canon/references/schedule-contract.md#schedule-baseline-contract) that [crw-plan](../crw-plan/SKILL.md) owns, under the field names and value words it fixes, and keep no second copy of any date. Mark what the schedule comparison covered and what it did not, for the whole request rather than only for the results that turned out to be decidable.

Classify each result as ahead, on plan, at risk, late, or undecidable, deciding by the reference's first-match order rather than by the order those names are listed in, so the same inputs always produce the same verdict. Ahead needs real early achievement at the delivery level that result's criteria require, not a fast source landing under criteria that require installation or live behavior, and never an issue count, a Done ratio, or a finished agent turn. On plan states only that results due by the observation are met; being before a deadline promises nothing about meeting it. A target date that has passed with the result unachieved is late, where a day target runs to the end of that day in its own timezone so today is never late early. Judge every result against both the baseline and the current date, show the source of a replan, a scope change, or a pause, and never let a moved date erase a delay already recorded. A Schedule baseline recorded `unconfirmed`, meaning a target exists and no Baseline record establishing it can be found, makes that axis alone undecidable and the scope partially checked; a subject that entered scope later still has its own Schedule baseline, set by the record that first gave it a target, and is judged on both axes with its Change source showing when it entered. Where the evidence needed to decide was not read at this observation and the result was not already settled when it was last read, that verdict is undecidable too. A result that already completed is different, but only as far as its evidence is a settled event: a landing that happened is reused without being gathered again, while achievement resting on a current state, such as an installation still being present, is observed again for a current verdict or the result is undecidable with the earlier observed-at. The verdict itself is always recomputed from that evidence against the current dates, because a target moved to before the achievement turns an earlier ahead into late. The schedule verdict is recorded in its own field and is never a fourth per-criterion disposition: a late schedule does not turn a satisfied criterion into `needs_changes`.

[Schedule judgment](references/schedule-judgment.md) holds the decision order, the date and timezone arithmetic, the closed list of risks with their wait classes, the aggregation rule, predecessor-based impact, the reusable judgment record, the full routing of a mismatch by the level that owns it, and the judgment cases. Read it before routing anything the two sentences above do not plainly cover.

Lead a progress or interim report with the schedule: the overall verdict with the query and judgment times, then the notable lead and delay with baseline dates and actual evidence, then the affected next steps, then internal coordination waits and user decisions. A completion check keeps the lead described below and places that block immediately after it.

Judging the schedule by default neither widens nor narrows this skill's authority. Return needed schedule changes to [crw-plan](../crw-plan/SKILL.md) and already accepted corrections to the existing owner; do not move a date, change a Done state, take over execution or a merge, build a second path that resumes work, or repeat a notification. A passed or approaching deadline never lowers a completion criterion or a review requirement. Equally, a checkpoint inside an approved initiative or project execution keeps the parent confirmation and already-approved successor work that approval covers, so being called here does not turn an approved continuation into an independent read-only audit: route the mismatch to the level that owns the ordering and settle continuation there, within the approval it already holds: a mismatch contained in one project goes to that project's parent, while one that orders work across projects goes through the supervisor owning both, or, where the projects share no supervisor or answer to different ones, through direct coordination between the parents with the unsettled pair-level decision raised to the user, because a project parent never decides whether another project proceeds and no supervisor acquires authority over a pair it does not own. Explicit report-only, read-only, pause, and no-contact limits still win. In the report, keep the correction request sent, the parent actually resuming, and a result actually verified as three separate facts.

## Standalone audit

A standalone check without an established execution assignment is read-only. It returns findings and a proposed record; it does not create a worker, assume permission to change code, or rewrite requirements. Explicit read-only, report-only or no-contact instructions take precedence. Route authorized roadmap changes to [crw-plan](../crw-plan/SKILL.md), and verified delivery to the coordinator's [delivery and integration](../crw-canon/references/delivery.md#default-dev-integration). A passing audit alone does not authorize requirement changes, issue closure or deployment.

Use a separate audit document only when prior authorization or an explicit recording request covers that document. Keep sensitive raw evidence local and link a suitable summary. Lead the report with the verification result, add a concise criterion/evidence table when useful, and finish with the checks in [Report the result](#report-the-result).

## In-workflow auditor

For a completion check in an established execution workflow, send evidence-backed in-scope corrections to the existing responsible task through [crw-run](../crw-run/SKILL.md#return-corrections-to-the-existing-task). Include the violated criterion, reviewed revision, reproducible evidence, required outcome and recheck. Do not make Jun relay the packet or ask again whether to send it. A finding is returned by the caller's own level to the level below it, per [scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level): the parent carries a project's finding in the [coordination message](../crw-run/references/task-packet.md#coordination-message) to that project's child and never to its `child` sessions (a code id, not a layer), a standalone issue keeps its existing owner, and a check never sends a correction into another project's `child` sessions.

Recheck the corrected revision against the original criteria and the reproduced failure. Missing evidence calls for a scoped verification request, not an invented defect or a speculative fix. Reuse valid evidence and keep the same correction scope until verified or a concrete blocker requires a decision. Lead the report with the verification result and the action actually taken: correction sent, awaiting delivery, recheck passed, integrated, or blocked.

Under an existing management assignment, write the audit summary to the coordination issue of the milestone and read it back; a separate audit issue needs prior authorization or an explicit recording request, and authorization already granted is not asked for again. Where a relay holds the assignment, the summary is queued, the verdicts are recorded against its registered criteria and a quiet child is not a finding: all of that is in [Check records in a relay store](../crw-run/references/check-records.md).

## Bounded helper

A sub-task or bounded helper called from here writes no tracking record itself: it returns the issue ref, the revision it read, the reason, the smallest sufficient change and its evidence, and this assignment's own writer applies and reads it back, under [record writes and returned proposals](../crw-canon/references/tracking-model.md#record-writes-and-returned-proposals). A bounded helper returns the correction packet to the coordinator, which owns dispatch, and returns its findings to its caller rather than contacting an owner itself.

## Report the result

Before finishing, verify every verdict has a source, accepted changes were considered, deferrals remain distinct from completion, and unverified delivery levels are explicit.

### Report delivery reach in the check result

Where the audited work is something a reader installs or runs, carry the chain from [Delivery reach and current usability](../crw-canon/references/delivery.md#delivery-reach-and-current-usability) into the result: the stages the delivery has evidence for in chain order, any first stage that is missing or unobserved, and the two closing lines. A per-criterion disposition and that chain answer different questions, since verified says the obligation was met at the reviewed revision and the chain says how far that revision travelled, so a verdict never promotes a stage. A criterion can be verified on a revision nobody has installed.

A check reads installation and runtime state; it does not install, start or restart anything to supply evidence it is missing. Record the missing state as unverified with the observation that would settle it, and carry it in the report and its closing next step rather than as a finding. Missing reach is not a criterion failure and the correction path above needs one; [Check records in a relay store](../crw-run/references/check-records.md#missing-reach-is-not-a-failed-criterion) holds how a relay-held assignment treats that.
