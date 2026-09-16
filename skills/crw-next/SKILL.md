---
name: crw-next
description: "Choose one concrete next action from a Linear product's goals, repository, and execution evidence. Use for '다음에 뭐 하지?', uncertainty about where to start, or what follows a completed task; use crw-run to execute an authorized choice. Formerly linear-next."
---

# CRW Next

Help Jun choose one action when he does not know where to start or what to do
after finishing work. Ground the choice in the product outcome and live evidence.

## Read the request and state

Read [Integrations](../crw-plan/references/integrations.md) for canonical
documents, access, inherited scope, and workflow ownership. Resolve the target
from the current assignment, linked Linear items, and repository identity. Use
the active project when the context identifies it; compare projects only when
the request covers that broader scope.

Use installed `readchk` when the target, meaning of "done", or intended scope is
ambiguous. Read its current instructions, cross-check the conversation and known
context, and proceed silently when resolved. Ask one focused question only for a
material choice the evidence cannot settle. Do not ask Jun to confirm a clear read.

Gather only the evidence needed to choose: product goal and accepted decisions,
relevant Linear criteria/dependencies, active task ownership, and current repository
or delivery state. Start with the linked coordination record and scoped reads;
refresh stale facts and paginate relevant results before claiming absence. Do not
audit the whole workspace or launch every helper just to select an action.

Distinguish these two situations. An empty backlog does not prove a fresh start;
a Done label or completed agent turn does not prove the outcome is delivered.

When repository identity affects readiness, apply the shared
[repository resolution](../crw-plan/references/integrations.md#resolve-the-implementation-repository).
Read the issue's actual target and existing assignment; do not choose all project
repository labels. Distinguish resuming preserved work from a new baseline.

## When the user does not know where to start

Establish the intended user outcome, what already exists, and the constraint or
uncertainty that currently prevents progress. Inspect existing plans, code, and
open work before proposing a new project or duplicate issue.

If the outcome is unclear, choose the smallest discovery step that would change
the decision: a focused user question, an external observation, or a bounded
experiment. Use [crw-define](../crw-define/SKILL.md) when the initiative goal
itself needs definition. If it is clear, choose the smallest useful first delivery with ready
prerequisites. Missing documents alone do not justify a full planning exercise;
use [crw-plan](../crw-plan/SKILL.md) when missing scope or dependencies
actually prevent execution. Prefer a step that produces evidence or user value
over setup work that merely makes the backlog look organized.

## When the user does not know what follows finished work

Read the delivered artifact and completion evidence against the agreed criterion
and required delivery level. Separate implemented, reviewed, merged, deployed,
and observed behavior. Reuse valid proof; read
[Merge readiness](../crw-run/references/merge-readiness.md) only when PR
readiness or integration determines the next action.

If the claimed result still has a consequential gap, choose its correction or
missing verification and route it to the existing owner. Do not start a successor
whose prerequisite is unverified. If the required result is verified, choose the
next ready dependency or milestone step that advances the product outcome.
Do not repeatedly audit completed work or demand deployment when the agreed
delivery stops earlier. When the whole outcome is complete, say so; choose an
appropriate handoff or real-world validation only if it serves an outstanding
goal. If nothing remains, recommend stopping instead of inventing more work.

## Select and return one action

Read and use installed `nba` with the scoped evidence above: current outcome,
starting/finished state, binding constraint, ready candidates, and existing owner.
It supplies the next-action judgment within the completion and ownership boundaries
above; a completed goal or an active owner does not require another cycle.
Keep its one-action, evidence, reason,
and observable completion contract; do not impose its cycle vocabulary on the user
or require that the project adopt that workflow. Use `readchk` for interpretation,
not as a recurring approval gate. If a helper is unavailable, disclose that fact
and make the bounded selection from available evidence without claiming it ran.

Honor explicit priorities and deadlines, then choose the action that removes the
most consequential constraint or closes the nearest useful delivery. Do not rank
by issue number alone or create a numerical score without evidence. Account for
active ownership and ready dependencies; a recommendation must not create a
duplicate writer. If crucial evidence is unavailable, identify the gap and choose
the smallest way to resolve it rather than inventing readiness or asking for a
full history recap.

Lead with one concrete action in plain Korean. Briefly explain the current state,
why this action matters now, and what observable result finishes it. Link the
decisive Linear item or evidence and identify the owner or execution route when
useful. Give one recommendation, not a menu of skills or projects. When Jun has no
action to take while authorized work proceeds, say that and identify the next
meaningful result to observe instead of manufacturing a task for him.

A standalone "what next?" is advisory: selection does not launch work or mutate
Linear. In an active authorized run, answer briefly and return control to the
existing coordinator; the question does not pause execution or require renewed
approval. When execution is also requested, hand the chosen action and evidence
to [crw-run](../crw-run/SKILL.md) or the established owner under the existing
scope. Preserve explicit report-only limits and the shared integration rules.
