---
name: linear-plan
description: "Turn a product's repository, decisions, and delivery history into canonical Linear planning documents and a roadmap, or reconcile changes into an existing plan. Use for initiative/project setup, milestones, PR-sized issues, and dependencies; use linear-run for dispatch and linear-check for implementation drift."
---

# Linear Plan

Build a usable product plan from what exists and what the user wants next. Linear holds the canonical product, planning, and decision documents; repositories hold implementation and reproducible evidence.

## Connect the workflow

Read [Integrations](references/integrations.md) for Linear access, document authority, CXC ownership, and Paperthin invocation rules. Load relevant installed skills and use their outputs in the plan; naming a skill is not using it.

- Use `cxc-recall` to recover missing decisions and `catchup` when the product's current state is unclear. Verify old state against current sources.
- Use `readchk` to resolve a bundled request before splitting it. Ask only about a material fork the evidence cannot settle.
- Use `cxc-dev` to assess development scope, repository boundaries, and meaningful verification. Planning does not start a CXC Loop or execution tasks.
- Use `ssotize` in audit mode if roadmap facts conflict or are duplicated, `mandela` when success criteria could reward their own assumptions, and `re0` to make revised descriptions read as the current plan.
- Hand executable work to [linear-run](../linear-run/SKILL.md). Use [linear-check](../linear-check/SKILL.md) for intent-versus-implementation uncertainty and [linear-logic](../linear-logic/SKILL.md) for contradictions within the proposed plan.

## Establish the baseline

Read the user's goal, existing Linear initiative/project/milestones/issues, full acceptance criteria, dependencies, canonical documents, and relevant accepted decisions. Inspect repository guidance, branch/worktrees, dirty changes, and relevant code/PR evidence. Paginate scoped results before concluding an item is absent.

Distinguish proposed, implemented, reviewed, merged, deployed, and behaviorally verified work. A Done label or local commit cannot establish every later state. If evidence disagrees, preserve the source links and record the disagreement.

Identify the durable product and finishable outcome. Reuse existing initiative/project IDs and team conventions. Do not infer product identity from a checkout folder name or merge distinct products because their repositories are related.

## Shape the plan

- **Initiative:** product or longer-term direction, following the workspace convention.
- **Project:** a finishable outcome. Prefer the established `Product · Outcome` naming when the workspace uses it.
- **Milestone:** an observable result or coherent delivery boundary. Follow explicit user grouping, such as one milestone per character or module.
- **Issue:** one reviewable delivery, often one PR, with scope, acceptance criteria, canonical document links, dependencies, and meaningful verification.

Use the smallest structure that makes the next action clear. Do not invent dates, owners, status transitions, or a team per product. Record unknowns plainly.

Split by deliverable and shared contract, not file count. Derive order from dependency edges and overlapping edit surfaces. A schema/API contract may precede several apparently independent issues. Avoid dependency cycles and distinguish speculative backlog ideas from approved requirements.

Make criteria observable: user behavior, data/state that must survive, and important failure cases. Do not weaken criteria to match code already written. Reuse completed work as evidence or a prerequisite instead of reopening it as a new implementation task.

## Reconcile and apply

For an existing plan, compute a compact change set: reuse, create, update, or leave unresolved. Match stable IDs and semantic scope before titles. Re-running the same request should converge on the same items.

A request to create or update the Linear plan authorizes the corresponding document/item writes. Prepare concrete changes, apply them within scope, and read back the resulting documents, items, and relations. Do not add a second approval step for routine authorized writes. A draft-only request stays a draft. Deletion, archival, issue closure, messages to others, or a material change to agreed scope need authorization covering that action.

Preserve unrelated content, labels, history, and human edits. Refresh before updating if another actor may have changed an item. After an uncertain write, look up the existing result before retrying; report partial completion with actual IDs.

Link issues to canonical Linear documents instead of copying the whole specification into every issue. Include the acceptance criteria needed to act. Local drafts remain explicitly unsynced until the Linear write is verified; do not create a parallel permanent planning source.

## Deliver

Return Linear document/item links, meaningful changes, unresolved decisions, and the next ready issue or batch with prerequisites. Check requested scope coverage, duplicate work, coherent dependencies, and read-back evidence for reported writes.

Provide a standalone handoff to `linear-run` with source IDs and embedded criteria when worker connector access is unproven. Do not launch work unless execution was requested.
