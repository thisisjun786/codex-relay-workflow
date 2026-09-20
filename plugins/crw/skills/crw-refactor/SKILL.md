---
name: crw-refactor
description: "Diagnose structural debt after an initiative cycle, project or sprint, using verified delivery and actual code changes. Recommend up to three bounded improvements that reduce the next change's cost while preserving behavior. Use for post-cycle refactoring, duplication or temporary-compatibility cleanup; default to diagnosis and planning, and execute selected candidates only within the user's authorized scope."
---

# CRW Refactor

Find what the completed cycle made harder to change, and propose the smallest useful repair. Success means reducing a demonstrated maintenance cost while preserving the features and external contracts the user wants to keep. A tidy-looking diff or a new abstraction is not evidence of that benefit.

Read [Integrations](../crw-plan/references/integrations.md) for source authority, role ownership and available tools. Use [crw-check](../crw-check/SKILL.md) to establish the delivery boundary, [crw-plan](../crw-plan/SKILL.md) for authorized issue planning, and [crw-run](../crw-run/SKILL.md) for managed implementation. Keep one owner for each operation; this skill adds no runtime, hook, analyzer or completion loop.

## Establish the requested mode

An initiative-cycle, project or sprint refactoring request starts with diagnosis and a plan unless the user already authorized implementation. “Diagnosis only” and “no merge/deploy” remain binding. Reuse explicit authorization from the conversation; do not ask again for a selected implementation that was already requested.

At the end of a verified cycle, offer this bounded pass as the next step when it would serve the user's roadmap. A completed cycle alone does not launch a refactoring task. Default diagnosis reads evidence and returns recommendations; it does not edit code or Linear, wake workers, create tasks/goals or activate a Loop. An authorized ongoing execution elsewhere continues under its owner.

Infer the target cycle from the current assignment, linked Linear records and delivery history. Ask only when different plausible targets would materially change the diagnosis. Continue useful reads while a necessary answer is pending.

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
| Repeated co-change | Related fixes or PRs changing the same sites for one cause; distinguish true coupling from incidental formatting or generated output |
| Difficult testing or diagnosis | A reproducible boundary that requires excessive setup, obscures a failure's source or prevents a focused control |

Search the affected scope for sibling instances and state the search boundary. Do not claim complete coverage from a few examples. Separate debt introduced in the cycle from older debt the cycle exposed; keep older, unrelated opportunities outside the recommendations unless they block the agreed next work.

Exclude taste-only renaming, speculative future abstractions, rewrites without a demonstrated cost and deletion based only on apparent age. Temporary compatibility may still serve live consumers. If usage or ownership is unknown, propose the smallest measurement that could settle it, not deletion.

## Recommend at most three candidates

Rank by concrete next-change cost reduced, evidence strength and size of the required change. Return zero to three; do not fill a quota. Use the [candidate record](references/candidate-record.md) for the compact evidence and validation fields. Each candidate must state its actual code/PR evidence, cost if left alone, smallest repair, preserved behavior, affected scope and exclusions.

Separate work so each candidate can be checked and reverted on its own when that is technically true. Where two repairs share an invariant or depend on the same migration, state the dependency and keep the coherent repair together instead of promising false independence. Keep one issue/task/PR per coherent result under the existing workflow.

Before suggesting execution, define baseline checks and the observations that would demonstrate reduced cost. Label known failures with their revision, environment and reproduction; distinguish observed failures from reports you could not reproduce. Specify what will count as an introduced regression and which unaffected evidence can be reused. Preserve required coverage and external contracts; a green test with its assertion removed proves nothing.

Return the verified scope and remaining gaps, the ranked candidates, why the first is worth doing now, and meaningful deferred items. This is a recommendation, not an implementation or installation claim. When Linear updates are authorized, deduplicate existing issues, preserve the original design document, record the findings as comments or linked work, and read back mutations. Otherwise return a reviewable proposal without writes.

## Carry a selected candidate into execution

When the user selects a candidate, restore its evidence and compare it with the current code, active assignments and roadmap. Refresh only what changed. An invalidated candidate returns the concrete discrepancy before dependent edits; stable authorization does not need reconfirmation.

An initiative supervisor coordinates the diagnostic handoff and user report; the project owner supplies technical judgment. Hand the selected repair to its existing project parent and issue task through Run, or implement in this task when the user explicitly chooses direct implementation. Carry the candidate's scope, evidence, contracts, baseline failures, same-cause search boundary and finish conditions together. A new task requires authorization under the host's task-creation rules. Follow the owning repository and CXC workflow without creating a second coordinator or changing another task's goal.

Implement the smallest agreed repair and inspect its sibling instances within that scope. Newly discovered unrelated improvements remain follow-up candidates. Preserve dirty and unfinished work; do not reset, stash, rebase or overwrite another task's changes to make the repair fit. A changed external contract requires a separate decision, not a refactoring label.

Verify preserved behavior and the intended maintenance benefit against the pinned baseline. Report introduced regressions, known failures and remaining unknowns separately, with the actual commands and revisions. Keep implementation, review, merge, installation and runtime proof distinct. The issue task carries checks and review resolution to the parent; the parent owns acceptance and authorized integration. An explicit no-merge or no-deploy limit remains in force throughout.
