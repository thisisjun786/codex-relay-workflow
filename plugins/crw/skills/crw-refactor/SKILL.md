---
name: crw-refactor
description: "Diagnose structural debt after a cycle, project or sprint from verified delivery and actual code changes. Read-only: recommends up to three bounded repairs, and a chosen one is added as a tracking issue by crw-add-issue. Use for post-cycle refactoring, duplication or compatibility cleanup."
---

# CRW Refactor

Here the parent (repository session) and the child (project session) are layers of [the level table](../crw-canon/references/hierarchy.md#the-levels).

Find what the completed cycle made harder to change, and propose the smallest useful repair. Success means reducing a demonstrated maintenance cost while preserving the features and external contracts the user wants to keep. A tidy-looking diff or a new abstraction is not evidence of that benefit.

Actors: the parent and the top session, who diagnose ([the level table](../crw-canon/references/hierarchy.md#the-levels)); this skill is read-only. Read the shared [tracking model](../crw-canon/references/tracking-model.md) for source authority and tracking access and [execution roles](../crw-canon/references/execution-roles.md) for role ownership. Use [crw-check](../crw-check/SKILL.md) to establish the delivery boundary. A repair that is chosen becomes a tracking issue through [crw-add-issue](../crw-add-issue/SKILL.md) and runs through [crw-run](../crw-run/SKILL.md) like any other issue. Keep one owner for each operation; this skill adds no runtime, hook, analyzer or completion loop.

## Establish the requested mode

An initiative-cycle, project or sprint refactoring request gets a diagnosis and a ranked recommendation, and this skill does nothing past that: it implements nothing and starts nothing. “Diagnosis only” and “no merge/deploy” remain binding wherever they were said.

At the end of a verified cycle, offer this bounded pass as the next step when it would serve the user's roadmap. A completed cycle alone does not launch a refactoring task. The diagnosis reads evidence and returns recommendations; it does not edit code or the tracking repository, wake workers or create tasks or goals. An authorized ongoing execution elsewhere continues under its owner.

Infer the target cycle from the current assignment, linked tracking records and delivery history. Ask only when different plausible targets would materially change the diagnosis. Continue useful reads while a necessary answer is pending.

## Verify and pin the cycle

Record the cycle's agreed goal, original design, scope, accepted changes and exclusions, actual start/end dates (or an open end), the repositories and delivered PRs, and a pinned baseline and delivered revision for each repository. Read repository policy and inspect branch, worktrees and dirty paths before judging current code. Use the actual PR changes and landing history; do not assume every commit in an integration range belongs to this cycle.

Reuse the owning check's acceptance evidence while its criteria, revision and environment still apply. Recheck missing or invalidated claims in read-only mode. Keep source landing, installation and observed runtime behavior separate. A missing installed measurement is a delivery-evidence gap, not proof that a module needs rewriting. Findings that violate existing criteria belong to their correction owner, with their impact and disposition; preserve accepted residuals and their rationale rather than reopening them merely because refactoring found them again.

If the cycle is unfinished or some evidence is inaccessible, name the verified subset and the missing scope. Diagnose that subset where useful, without claiming the whole cycle is complete or treating active/dirty work as abandoned. A cycle-wide conclusion requires cycle-wide evidence.

Read the next agreed work and its dependencies. Prioritize structures that concretely obstruct that work. If a port or replacement is already planned, compare the repair's useful lifetime with the cost of carrying the current structure to that boundary. Deferral or no candidate can be the best result; do not replace the roadmap with a cleanup program.

## Inspect demonstrated structural costs

Start from this cycle's changed paths and follow their relevant callers, consumers and tests. Inspect adjacent code only far enough to establish the cause and its reach. For each plausible problem, look for a concrete example:

| Signal | Evidence to look for |
|---|---|
| Duplicated logic or scattered rules | The same decision made at multiple sites, with a drift example or repeated synchronized edits |
| Mixed responsibilities | One change forcing unrelated consumers or tests to change because distinct decisions share an owner |
| Dead code or temporary compatibility | Consumer/registration search, supported-version contract and the removal trigger; a search miss alone does not prove runtime disuse |
| Repeated co-change | Related fixes or deliveries changing the same sites for one cause; distinguish true coupling from incidental formatting or generated output |
| Difficult testing or diagnosis | A reproducible boundary that requires excessive setup, obscures a failure's source or prevents a focused control |

Search the affected scope for sibling instances and state the search boundary. Do not claim complete coverage from a few examples. Separate debt introduced in the cycle from older debt the cycle exposed; keep older, unrelated opportunities outside the recommendations unless they block the agreed next work.

Exclude taste-only renaming, speculative future abstractions, rewrites without a demonstrated cost and deletion based only on apparent age. Temporary compatibility may still serve live consumers. If usage or ownership is unknown, propose the smallest measurement that could settle it, not deletion.

## Recommend at most three candidates

Rank by concrete next-change cost reduced, evidence strength and size of the required change. Return zero to three; do not fill a quota. Use the [candidate record](references/candidate-record.md) for the compact evidence and validation fields. Each candidate must state its actual code/PR evidence, cost if left alone, smallest repair, preserved behavior, affected scope and exclusions.

Separate work so each candidate can be checked and reverted on its own when that is technically true. Where two repairs share an invariant or depend on the same migration, state the dependency and keep the coherent repair together instead of promising false independence. Keep one coherent result per candidate, so that a selected candidate becomes one issue.

Before suggesting a repair, define baseline checks and the observations that would demonstrate reduced cost. Label known failures with their revision, environment and reproduction; distinguish observed failures from reports you could not reproduce. Specify what will count as an introduced regression and which unaffected evidence can be reused. Preserve required coverage and external contracts; a green test with its assertion removed proves nothing.

Return the verified scope and remaining gaps, the ranked candidates, why the first is worth doing now, and meaningful deferred items. This is a recommendation, not an implementation or installation claim. Return it as a reviewable proposal: the skill writes nothing to the tracking repository or the code, and the record of a finding that someone wants kept is the issue added under the next section.

## Hand a selected candidate to the project path

When the user selects a candidate, restore its evidence and compare it with the current code, active assignments and roadmap. Refresh only what changed. An invalidated candidate returns the concrete discrepancy before an issue is added; stable authorization does not need reconfirmation.

The repair becomes a tracking issue through [crw-add-issue](../crw-add-issue/SKILL.md). The parent drafts the issue body with it (a draft-only request, which writes nothing) and hands the draft to the child of the project that owns the code, as crw-add-issue's callers require; that child registers the issue and then runs it through [crw-run](../crw-run/SKILL.md) as it runs any issue. When the user asks for the issue to be added, the user's request is the write intent for that one issue. Deduplicate against current issues first. Reuse an undelivered issue that already owns the candidate; a new change after a landed delivery needs a new issue and delivery under [issue-to-PR mapping](../crw-canon/references/tracking-model.md#issue-to-pr-mapping), and the completed delivery is never reopened just to give the repair an owner. A defect inside the landed issue's own promise reopens that issue instead ([Late review threads](../crw-run/references/merge-readiness.md#late-review-threads)). Where writing the issue is outside the request, return the issue text as a handoff instead.

Put the candidate's scope, evidence, contracts, baseline failures, same-cause search boundary and finish conditions into the issue's criteria, so that the check of the finished issue can show the maintenance benefit and any introduced regression against the pinned baseline. Newly discovered unrelated improvements remain follow-up candidates. An explicit no-merge or no-deploy limit travels with the issue.
