---
name: crw-status
description: "Report where work actually stands: the goal, the current position, what is moving, what is blocked, what needs Jun's decision, and what is unverified, with investigation depth scaled to the question. Use for 중간점검, '현재 어디까지 됐어?', '상황 알려줘' and whole-portfolio status; use crw-next to choose an action, crw-check to verify delivery against criteria, and crw-logic for contradictions."
---

# CRW Status

Report what is true right now and how it is known. Jun asks for this as 중간점검, as
'현재 어디까지 됐어?', or as '전체 프로젝트 어떻게 돼가?', and what he wants back is a short
grounded report rather than a plan, an audit, or a recommendation he did not ask for.

## Keep the four read operations apart

Status reports the current situation. [crw-next](../crw-next/SKILL.md) chooses one action to take.
[crw-check](../crw-check/SKILL.md) compares delivery against accepted criteria obligation by
obligation. [crw-logic](../crw-logic/SKILL.md) investigates a suspected contradiction. Where a
report surfaces a question one of those owns, name the owner and the question; do not attach a
full requirement audit, a contradiction hunt, or an execution to a user who asked how things are
going. A next action belongs in the report when the user asked for one, and is not a required
section otherwise.

## Connect the workflow

Read [Integrations](../crw-plan/references/integrations.md) for canonical documents, connector
access, the supervisor, parent and child roles, and which operation owns what. Reuse the
evidence-first, short-brief form of installed `catchup` for the writing itself, without inheriting
its assumption that the reader has been away: the same form serves a routine midpoint check.
Use `readchk` when the request bundles several questions or its referent is unclear, and resolve
silently when the context settles it. Use `cxc-recall` only for history the current records no
longer carry; it does not establish current state.

Two entries have their own procedures. A supervisor's one-word midpoint check over an initiative
is [Midpoint check](references/midpoint-check.md). Progress against the agreed schedule is
[Schedule progress](references/schedule-progress.md), and it is a default part of a midpoint or
status report rather than an extra a user has to request.

## Resolve what to report on

Take the target from the current request, then its explicit links, then this task's verified
binding, and say which one you used. Resolve names to stable IDs under the shared
[target rules](../crw-plan/references/integrations.md#resolve-the-project-target). A Linear project
ID, a Codex task ID, a Desktop project ID and a repository are four different identities, and a
title, a folder or a branch is none of them.

Reading another project because somebody asked about it changes no binding. That holds at every
level: a temporary question does not repoint a project parent, and a supervision task that answers
one question about a neighbouring initiative still supervises the initiative it was designated for.
A binding changes only by an explicit designation or switch.

Where the conversation already fixes the target, as 'CRW 상황 알려줘' does, state the scope you are
reporting on and go. Where it does not, do not settle an ambiguous target by picking the most
recent, the most convenient, or the closest title match. Ask one question only when the choice
between candidates would change the answer materially and the evidence in hand cannot decide it,
and name the candidates in that question.

## Choose the depth before reading

The width of the question sets the width of the report, not its depth. '전체 프로젝트 어떻게
돼가?' is answered with every project's one-line status and the main blockers first. Never quietly
answer a portfolio question with one project, and never let a detailed dive on one project stand in
for the others.

When you do narrow the detail, say which projects you went deeper on, why those, and how deep you
actually went. The projects you did not examine closely are reported at the depth you actually
reached; describing them as checked, verified, or fine is a false claim about work not done.

Read outward from the cheap sources. Listings, project and issue summaries, the linked coordination
record, and the current pull request state usually answer the question. Go further only for the
evidence the report actually needs. Do not expand by reflex into every comment on an issue, the
whole repository, or every task's full transcript: a check that reads everything costs more than
the work it is checking. Where a task's own record must be read, use a compact snapshot and its
returned cursor rather than a full replay, and read the final answer alone when the conclusion is
what matters. Pagination is the exception to frugality: where a review, a check list or a comment
thread matters to the report, page it to the end, because an unread page is an unseen finding.

## Keep the evidence states apart

Five things get collapsed into 'done' and they are five separate claims: the source exists, its
checks passed, it actually merged into the intended target, it is installed or deployed, and it has
been observed working. A Linear status of Done, a summary written last week, and an agent turn that
ended are none of them. Use [Implementation Done](../crw-plan/references/integrations.md#implementation-done)
for what a real landing requires and [Merge readiness](../crw-run/references/merge-readiness.md)
for what current CI and review evidence establish.

Give the time you checked and the one link that carries each load-bearing claim. A read that
failed, a source you cannot access, and a listing you only partly paged are unverified, and they
are reported as unverified with the reason. Unknown is a usable answer; a confident guess is not.
Where the head moved while you were reading, say so rather than reporting a mixture of revisions
as one state.

## Write the report

Lead with the conclusion: where the work stands overall and the one thing most in the way. Then
one short status per project, including its schedule verdict. Then what is waiting, split between
what an internal coordinator will settle and what genuinely needs a new decision from Jun. Then
the evidence you checked and the items still unverified.

Write it in Korean, short, and in the order above. Keep each project's line to its state, its
blocker and its evidence pointer. Never write that something was handled, fixed or completed unless
this check produced a new fact that it was; a report is not an action, and describing it as one is
the failure this skill exists to prevent.

## Status reads, and returns control

An independent status call is read-only. It does not write Linear, create or resume a task, send a
message, wake a parent or a child, install anything, or change a schedule. Reporting that a project
is behind does not authorize starting work on it.

A status question that arrives during an authorized run is a question, not a cancellation. Answer
it and return control to the task that owns the execution, which keeps every obligation it already
had; a supervisor or parent does not shed its continuing duties because someone asked for a
progress report. Where the user asks for action as well as a report, hand the scoped request to the
existing owner: [crw-run](../crw-run/SKILL.md) for execution, [crw-check](../crw-check/SKILL.md)
for verification, [crw-plan](../crw-plan/SKILL.md) for a schedule or scope change. Status itself
creates no authority those owners do not already hold.

Do not set up recurring or background reporting. Each check is asked for and answered once.
