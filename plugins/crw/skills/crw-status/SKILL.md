---
name: crw-status
description: "Read-only report of where work stands, by layer from the parent to each issue, with the evidence, and the one next action when asked. Use for '현재 어디까지 됐어?', '상황 알려줘', '다음에 뭐 하지?'. crw-check audits delivery against criteria; crw-run executes."
---

# CRW Status

Here the parent (repository session) and the child (project session) are layers of [the level table](../crw-canon/references/hierarchy.md#the-levels).

Report what is true right now and how it is known, and choose the one action that follows when
Jun asks for it. He asks for this as '현재 어디까지 됐어?', as '전체 프로젝트 어떻게 돼가?' or as
'다음에 뭐 하지?'. Standing on its own, that asks for a short grounded report rather than a plan,
an audit, or a recommendation he did not ask for.

This skill is read-only. It writes no tracking record, creates or resumes no task, sends no message,
wakes no child or `child` session (a code id, not a layer), installs nothing and changes no schedule, whatever
the state of the work it reports on. Reporting that a project is behind does not authorize starting
it. A one-word 중간점검 inside the parent (repository session) is not a read: its owner is
[the parent's section of crw-run](../crw-run/SKILL.md#the-parent-the-repository-session), and when this task is no parent the
request is a status read and is answered here. The layers and their names are
[the level table](../crw-canon/references/hierarchy.md#the-levels).

## Keep the read operations apart

Status reports the current situation and, when asked, chooses one action to take.
[crw-check](../crw-check/SKILL.md) compares delivery against accepted criteria obligation by
obligation, finds contradictions between specifications and evidence, and judges progress against
the agreed schedule. Where a report surfaces a question one of those owns, name the owner and the
question; do not attach a full requirement audit or a contradiction hunt to a user who asked how
things are going, and never start work the standing approval does not already cover. A next action
belongs in the report when the user asked for one, and is not a required section otherwise.

## Connect the workflow

Read the shared [tracking model](../crw-canon/references/tracking-model.md) and
[delivery](../crw-canon/references/delivery.md) for canonical documents, tracking
access and which operation owns what, and
[scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level)
for the levels and the names a Korean report gives them. Write the report in an
evidence-first, short-brief form, without assuming that the reader has been away.
When the request bundles several questions or its referent is unclear, restate it and resolve
silently when the context settles it. Use `crw-recall` only for history the current records no
longer carry; it does not establish current state.

Progress against the agreed schedule is a default part of a status report rather than an extra a
user has to request: one short verdict with its reason, taken from
[Schedule judgment](../crw-check/references/schedule-judgment.md), which `crw-check` owns. The
same inputs give the same verdict from either side, and this skill defines none of the verdicts.
Where a verdict disagrees with check's, or the evidence is too thin to decide, the route is
[crw-check](../crw-check/SKILL.md) rather than a deeper investigation here. A verdict changes no
date and starts no execution; a schedule change belongs to [crw-plan](../crw-plan/SKILL.md) and new
work to [crw-run](../crw-run/SKILL.md).

The morning report of a night batch is written by `crw manage batch report` in the form of
[Night batch report](references/night-batch-report.md).

## Resolve what to report on

Take the target from the current request, then its explicit links, then this task's verified
binding, and say which one you used. Resolve names to stable IDs under the shared
[target rules](../crw-plan/references/integrations.md#resolve-the-project-target). A milestone
ref, a Codex task ID, a Desktop project ID and a repository are four different identities, and a
title, a folder or a branch is none of them.

Reading another project because somebody asked about it changes no binding. That holds at every
layer: a temporary question does not repoint a child (project session), and a task that answers
one question about a neighbouring initiative still leads the initiative it was designated for.
A binding changes only by an explicit designation or switch.

Where the conversation already fixes the target, as 'CRW 상황 알려줘' does, state the scope you are
reporting on and go. Where it does not, do not settle an ambiguous target by picking the most
recent, the most convenient, or the closest title match. Ask one question only when the choice
between candidates would change the answer materially and the evidence in hand cannot decide it,
and name the candidates in that question.

## Report by level

Read and report from the top down, in this order, and stop at the level the question reaches: the
parent, then each initiative it orders, then each child, then each issue.

1. **Parent.** The repository's management seat (with the store seat read through it): the capacity and caps it holds, the decisions waiting for Jun, and
   what it has handed to its children.
2. **Initiative.** Per initiative the parent orders, its approved project set, the order across projects, the
   finish condition and whether it holds.
3. **Child.** Per project, its state, its blocker, its schedule verdict and its
   evidence pointer.
4. **Issue.** Per issue, its step, subagent or `child` session, its delivery and the evidence at the delivery level
   its criteria require.

A gap found at a level is reported there and sent one level down, to the owner of that level, never
past it and never to the reader's own convenience: a gap in an issue goes to its child, a gap that
spans projects goes to the parent that orders both, and a gap in the order across projects stays
with that parent. Where the projects belong to different repositories, a requirement for one of them
goes to that project's own parent, and only a genuinely shared decision with no common owner goes
to Jun through the top session. A standalone issue keeps its issue-scoped owner and no child is
invented for it. All of this follows the
[scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level); reading a level does
not give this skill that level's authority.

## Read a project's run

A child whose plan has a flow run is reported from the run's own records, which need no wake. `crw flow ps` lists the runs that are not over (`running`, `paused`, `dead` or `held`); `crw flow show RUN` gives the tree of one run (its phases, each agent call with its role, node, status and reason, the review verdicts, the acceptance, the integration and any `recovery_hold`); `crw flow watch RUN --after SEQ` gives the rows after a cursor; and `crw relay journal-tail` reads the relay's own journal the same way. The state of a run is taken from those records, never from the status line its owner thread prints.

Read the reports that wait with `crw flow reports --unread`: an unread report is a milestone report or a blocker that has reached its parent and not been read, and the status says to whom it went, how long it has waited and what the last delivery said. Status reads it and does not acknowledge it: `crw flow report-ack` appends a row of its own, so the mark is the parent's act. It starts, answers, pauses and resumes no run. A gap the run's records show goes to the child that owns the run, as any gap at that level does.

## Choose the depth before reading

The width of the question sets the width of the report, not its depth. '전체 프로젝트 어떻게
돼가?' is answered with every project's one-line status and the main blockers first. Never quietly
answer a portfolio question with one project, and never let a detailed dive on one project stand in
for the others.

When you do narrow the detail, say which projects you went deeper on, why those, and how deep you
actually went. The projects you did not examine closely are reported at the depth you actually
reached; describing them as checked, verified, or fine is a false claim about work not done.

Read outward from the cheap sources. Listings, project and issue summaries, the linked coordination
record, and the current delivery state usually answers the question. Go further only for the
evidence the report actually needs. Do not expand by reflex into every comment on an issue, the
whole repository, or every task's full transcript: a check that reads everything costs more than
the work it is checking. Where a task's own record must be read, use a compact snapshot and its
returned cursor rather than a full replay, and read the final answer alone when the conclusion is
what matters. Pagination is the exception to frugality: where a review, a check list or a comment
thread matters to the report, page it to the end, because an unread page is an unseen finding.

## Keep the evidence states apart

Five things get collapsed into 'done' and they are five separate claims: the source exists, its
checks passed, it actually merged into the intended target, it is installed or deployed, and it has
been observed working. An issue closed as completed, a summary written last week, and an agent turn that
ended are none of them. Use [Implementation Done](../crw-canon/references/tracking-model.md#implementation-done)
for what a real landing requires and [Merge readiness](../crw-run/references/merge-readiness.md)
for what current CI and review evidence establish.

Give the time you checked and the one link that carries each load-bearing claim. A read that
failed, a source you cannot access, and a listing you only partly paged are unverified, and they
are reported as unverified with the reason. Unknown is a usable answer; a confident guess is not.
Where the head moved while you were reading, say so rather than reporting a mixture of revisions
as one state.

## Write the report

Lead with the conclusion: what this work is trying to achieve, where it has actually reached
against that, and the one thing most in the way. The goal is part of the conclusion rather than
context the reader is assumed to carry, because a position with nothing to measure it against is
not a status. Then one short status per project, including its schedule verdict. Then what is
waiting, split between what an internal coordinator will settle and what genuinely needs a new
decision from Jun. Then the evidence you checked and the items still unverified.

What is waiting separates a normal dependency wait from a decision somebody owes. Failed and
unverified deliveries go with the other unverified items.

Write it in Korean, short, and in the order above. Keep each project's line to its state, its
blocker and its evidence pointer. Never write that something was handled, fixed or completed unless
the records show that it was. Something sent is not something done, and reporting a
request as progress is the failure this skill exists to prevent.

## Choose one next action

Asked what to do next, choose one action from the scoped evidence above and return it with the
evidence, the reason and the observable completion condition. Do not impose a cycle vocabulary on
the user or require that the project adopt a workflow.

Gather only the evidence the choice needs: the product goal and accepted decisions, the relevant
issue criteria and `Blocked by:` lines, active ownership, and the current repository or delivery state.
Refresh stale facts and page the relevant results before claiming absence. An empty backlog does
not prove a fresh start, and a closed issue or a completed agent turn does not prove the outcome is
delivered. Where repository identity affects readiness, apply the shared
[repository resolution](../crw-canon/references/tracking-model.md#resolve-the-implementation-repository).

**Where to start.** Establish the intended user outcome, what already exists, and the constraint
that prevents progress. Inspect existing plans, code and open work before proposing a new project
or a duplicate issue. If the outcome is unclear, choose the smallest discovery step that would
change the decision. Use [crw-define](../crw-define/SKILL.md) when a milestone and its issues need
settling before they are registered. If it is clear, choose the smallest useful first delivery with ready prerequisites, and
use [crw-plan](../crw-plan/SKILL.md) only when missing scope or dependencies actually prevent
execution.

**What follows finished work.** Read the delivered artifact and its completion evidence against the
agreed criterion and delivery level, reusing valid proof; read
[Merge readiness](../crw-run/references/merge-readiness.md) only when readiness or integration
decides the action. If a consequential gap remains, choose its correction or missing verification
and route it one level down as above. Do not start a successor whose prerequisite is unverified.
If the required result is verified, choose the next ready dependency that advances the outcome. When
the whole outcome is complete, say so and recommend stopping instead of inventing more work.

Honor explicit priorities and deadlines, then choose the action that removes the most consequential
constraint or closes the nearest useful delivery. Do not rank by issue number alone or invent a
score. Account for active ownership: a recommendation must not create a duplicate writer. Lead with
one concrete action in plain Korean, link the decisive issue or evidence, and give one
recommendation, not a menu. When Jun has no action to take while authorized work proceeds, say that
and name the next result to observe.

A "what next?" is advisory: selection launches no work and mutates no tracking record. During an active
authorized run, answer briefly and return control to the existing owner. When execution is also
requested, hand the chosen action and evidence to [crw-run](../crw-run/SKILL.md), the child's
procedure or the parent's section in it, under the existing scope.

## Return control, and report what was done

A status question that arrives during an authorized run is a question, not a cancellation. Answer
it and return control to the task that owns the execution, which keeps every obligation it already
had; a parent or a child does not shed its continuing duties because someone
asked for a progress report. Where the user asks for action beyond what the standing approval
covers, hand the scoped request to the owner that already has it: [crw-run](../crw-run/SKILL.md) for execution, [crw-check](../crw-check/SKILL.md)
for verification, [crw-plan](../crw-plan/SKILL.md) for a schedule or scope change.

Do not set up recurring or background reporting. Each call is asked for and answered once.
