---
name: crw-plan
description: "Decompose an agreed goal into Linear projects, useful milestones, and one-PR implementation issues in one planning operation. Use crw-define for initiative definition and intent exploration. Use for product planning, roadmap setup, and scoped plan updates; use crw-run for dispatch and crw-check for implementation drift. Formerly linear-plan."
---

# CRW Plan

Build a usable product plan from what exists and what the user wants next. Linear holds the canonical product, planning, and decision documents; repositories hold implementation and reproducible evidence.

Use an agreed initiative definition as input, then cover its requested scope through executable issues in one operation: projects → useful milestones → issues and dependencies. If the goal itself needs definition, use [crw-define](../crw-define/SKILL.md); a request for both definition and planning chains the two without another invocation. An existing project or standalone issue can supply the agreed goal without an initiative. Reuse existing levels and create missing items within that request without requiring a separate invocation at each level. A scoped project/issue update stays scoped; do not invent an initiative or extra projects just to fill the hierarchy. Consultation, plan-only, draft-only, and read-only requests do not authorize Linear writes; invoking this skill implicitly does not supply write intent.

## Connect the workflow

Read [Integrations](references/integrations.md) for Linear access, document authority, CXC ownership, and Paperthin invocation rules. Load relevant installed skills and use their outputs in the plan; naming a skill is not using it.

- Use `cxc-recall` to recover missing decisions and `catchup` when the product's current state is unclear. Verify old state against current sources.
- Use `readchk` to resolve a bundled request before splitting it. Ask only about a material fork the evidence cannot settle.
- Use `cxc-dev` to assess development scope, repository boundaries, and meaningful verification. Planning does not start a CXC Loop or execution tasks.
- Use `ssotize` in audit mode if roadmap facts conflict or are duplicated, `mandela` when success criteria could reward their own assumptions, and `re0` to make revised descriptions read as the current plan.
- Hand executable work to [crw-run](../crw-run/SKILL.md). Use [crw-check](../crw-check/SKILL.md) for intent-versus-implementation uncertainty and [crw-logic](../crw-logic/SKILL.md) for contradictions within the proposed plan.

## Establish the baseline

Read the user's goal, existing Linear initiative/project/milestones/issues, full acceptance criteria, dependencies, canonical documents, and relevant accepted decisions. Inspect repository guidance, branch/worktrees, dirty changes, and relevant code/PR evidence. Paginate scoped results before concluding an item is absent.

Distinguish proposed, implemented, reviewed, merged, deployed, and behaviorally verified work. A Done label or local commit cannot establish every later state. If evidence disagrees, preserve the source links and record the disagreement.

Identify the goal, product classification, and finishable outcome separately using the shared [Linear operating model](references/integrations.md#linear-operating-model). Reuse existing initiative/project IDs and team conventions. Do not infer product identity from a checkout folder name or merge distinct products because their repositories are related.

## Shape the plan

- **Initiative input:** the agreed goal, finish condition, scope and open decisions from `crw-define` or an existing accepted definition. Preserve its identity and link contributing projects; return material goal changes to definition within the request’s scope.
- **Project:** a finishable outcome created when the user requests a project. Name the result naturally; do not require a product prefix. A product name can appear when it helps explain the result.
- **Milestone:** an observable result or coherent delivery boundary. Follow explicit user grouping, such as one milestone per character or module.
- **Issue:** one implementation PR, with scope, acceptance criteria, canonical document links, dependencies, and meaningful verification. Apply the shared [issue-to-PR rule](references/integrations.md#issue-to-pr-mapping), including its non-PR work exception.

Use the smallest structure within the requested scope. An authorized full-plan write includes the projects needed for that agreed outcome; issue count, repository count, or estimated size alone does not authorize extra projects. Planning or updating issues can reuse a project or keep standalone issues without creating missing upper levels. A project may have no initiative or contribute to several; reuse its ID instead of duplicating the project or its issues. Do not invent dates, owners, status transitions, or a team per product. Record unknowns plainly.

Apply product-family and related-repository labels under the shared operating model. Reuse existing labels and keep descriptions short. Additional label schemes need discussion with the user; leave views and default screens to the user unless requested.

Carry the definition’s accepted direction and unresolved decisions into the plan. Develop project-specific design detail when needed; Strategy/Scope/Structure/Skeleton/Surface are document lenses, never required initiative/project/milestone/issue levels.

Split by deliverable and shared contract, not file count. Derive order from dependency edges and overlapping edit surfaces. A schema/API contract may precede several apparently independent issues. Avoid dependency cycles and distinguish speculative backlog ideas from approved requirements.

If an outcome needs several PRs, plan a separate implementation issue for each and connect prerequisites. Put the combined outcome in the project or milestone. Multiple repositories can belong to one project; code changes needing a PR in each repository require separate issues. Reading a dependency repository alone does not require another issue.

Make criteria observable: user behavior, data/state that must survive, and important failure cases. Do not weaken criteria to match code already written. Reuse completed work as evidence or a prerequisite instead of reopening it as a new implementation task.

Before handing off code issues, apply [repository resolution](references/integrations.md#resolve-the-implementation-repository):
name each issue's actual owner/repo and separate reference repositories. Project
labels do not substitute for that target. Leave a genuinely unresolved code target
as an explicit planning dependency, not an invented checkout assignment.

## Reconcile and apply

For an existing plan, compute a compact change set: reuse, create, update, or leave unresolved. Match stable IDs and semantic scope before titles. Re-running the same request should converge on the same items.

A request to create, update, or apply the full plan in Linear authorizes its scoped hierarchy and document/item writes, including needed projects; an explicit project request or already accepted proposal also authorizes that project. Resolve write intent from the full conversation and existing authorization, not from the skill name or hierarchy depth. An advisory planning request remains a proposal until an apply/create/update request covers it. A narrower update does not authorize unrelated new projects. Prepare concrete changes, apply them within scope, and read back the resulting documents, items, and relations. Do not add a second approval step for routine authorized writes. A draft-only request stays a draft. Deletion, archival, issue closure, messages to others, or a material change to agreed scope need authorization covering that action.

Preserve unrelated content, labels, history, and human edits. Refresh before updating if another actor may have changed an item. After an uncertain write, look up the existing result before retrying; report partial completion with actual IDs. If a project was created but its labels or relations failed, repair those fields on that ID. Do not repeat the project create. Resolve same-name candidates by IDs and semantic scope; ask only if the supplied target and current binding cannot distinguish them.

Link issues to canonical Linear documents instead of copying the whole specification into every issue. Include the acceptance criteria needed to act. Local drafts remain explicitly unsynced until the Linear write is verified; do not create a parallel permanent planning source.

## Deliver

Return Linear document/item links, meaningful changes, unresolved decisions, and the next ready issue or batch with prerequisites. Check requested scope coverage, duplicate work, coherent dependencies, and read-back evidence for reported writes.

For a full plan, do not stop at an initiative, project outline, or the first batch. Cover the entire agreed scope with issue-level criteria, repository/PR boundaries for implementation, dependencies, and verification. When discovery is necessary, define its question, output, and which later work it blocks instead of inventing implementation details. Report any unplanned scope or incomplete writes explicitly; an outline is not a completed plan. Creating the plan does not execute its issues.

Provide a standalone handoff to `crw-run` with source IDs and embedded criteria when worker connector access is unproven. Do not launch work unless execution was requested.
