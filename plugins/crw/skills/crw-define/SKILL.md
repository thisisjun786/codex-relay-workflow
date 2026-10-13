---
name: crw-define
description: "Settle a milestone and its issues before they are registered in Linear: purpose, desired change, success evidence, scope and open decisions, and whether a question is really needed. Hand the agreed definition to crw-plan. Triggers: 마일스톤 정의, 요구사항 정리, 아이디어 정리."
---

# CRW Define

Make a milestone and its issues clear enough to register before anything runs them.
This is the step that registers a milestone and its issues: the top session (today Sion)
and the parent (repository session) use it when they put new work into Linear. It
organizes purpose, desired change, success evidence, scope and open decisions, finds what
is still unclear, and decides whether that needs an exchange with a person.
Read the shared [Linear model](../crw-canon/references/linear-model.md) for source
of truth, write authority, classification, and tool access.
Defining a milestone starts no run and moves no project's ownership; the levels are in
[scope of each level](../crw-canon/references/hierarchy.md#scope-of-each-level).

## Find what is unclear

Read the request, accepted decisions, and the existing project, milestones, issues and
linked documents. Reuse stable IDs and distinguish the user's words, accepted choices,
suggestions, and unresolved assumptions. Inspect relevant product or repository evidence
only where it can change the definition. Do not replace the user's goal with a guessed
“real problem.” Use `crw-recall` for missing history, not as live proof.

Whether anything needs an exchange with a person is decided here, before registration;
a run does not ask later. Settle a gap from evidence, or record it as an open decision.
Ask only when an answer could materially change the goal, scope or design and no evidence
settles it, and then ask one question at a time. When a question is needed, the top
session holds that conversation with Jun; the parent sends its question to the top session
instead of asking a person itself. Prefer the consequential hidden premise or missing
perspective over a fixed questionnaire. A clear intent gets no ritual interview: do not
force clever questions, preset binary choices, recommendations, or exhaustive branches.
Accept an unexpected answer, “I don't know,” a correction, or a stop request; update the
working understanding instead of steering back to a preferred answer. Continue
independent work while a question is pending.

Ambiguity that appears after registration, during a run, is not this step's: the run
lifts it as a decision request to the session above the project session
([crw-run](../crw-run/SKILL.md)).

## Define the milestone

The definition is written in the milestone's description and the part each issue needs
is carried into that issue. Until the milestone is registered there is no description to
write: the definition is a draft returned to the caller, and crw-plan creates the
milestone with it as its description. An existing milestone gets the definition in its
own description. Keep a compact definition with:

- The original idea, problem or opportunity, and why it matters.
- The desired change and who benefits.
- Observable success evidence: the integrated result that finishes the milestone and the
  check that shows it.
- Included scope, explicit exclusions, accepted direction and reasons.
- The issues the milestone needs and what each delivers. Mark them as proposals until
  they are registered.
- Considered perspectives, deferred choices and unresolved questions that affect planning;
  an issue whose design question stays open carries it under its open decisions.

Do not invent dates, owners, numerical targets, validated demand, or user answers.
Distinguish evidence from assumptions. Record unknowns as unknowns rather than
filling later design stages with plausible detail. Reuse an existing milestone or issue
when the goal matches; related products or repositories alone do not establish it.
When the milestone serves a Linear initiative, read the initiative body as context
under the shared [initiative body standard](../crw-canon/references/linear-model.md#initiative-body-standard)
and return a needed change to it as a proposal.

## Size rules

A milestone is one observable result and each of its issues a concept bundle; their
numbers are crw-plan's [Size rules](../crw-plan/SKILL.md#size-rules). When the milestones
belong to a Linear initiative of several projects, the initiative is one outcome. Its
definition lists its projects, the artifacts one project hands to another, which projects
run in parallel and in what order, and an observable finish condition for each project:
which integrated result, and which check shows it. Recommended: at most 8 projects in
flight at the same time; beyond that, split the initiative or stagger its projects. The
levels these sizes belong to are in
[hierarchy.md](../crw-canon/references/hierarchy.md#sizes-at-each-level).

## Carry design forward

Use Strategy → Scope → Structure → Skeleton → Surface as design lenses, not as
Linear levels or mandatory headings. Keep the milestone definition in its description;
linked documents carry requested or necessary supporting detail, not a duplicate.
Where the definition names a repository contract, a procedure or a research
result, link it under [where each document type is canonical](../crw-canon/references/linear-model.md#where-each-document-type-is-canonical)
instead of copying it into the description.
The milestone definition usually supplies Strategy and Scope. Let the issues develop
Structure, Skeleton and Surface when they become knowable; CLI/API work need not invent
screens. Mark unsupported stages open. Preserve the original idea, newly considered
perspective, chosen direction and why, deferred alternatives, and material open questions.

A material contradiction returns to the relevant question; it does not start an
interview. Do not claim that a simulated conversation proves real question quality or
product demand. Real-use evaluation remains separate.

## Apply and hand off

Advice, exploration and draft-only requests produce a proposal without Linear
writes. A request to define or update an existing milestone in Linear covers that
milestone's description, not automatic issues, projects or a separate definition document.
A milestone that does not exist yet is not created here: a request to create it continues
to crw-plan with the draft definition as the milestone's description.
Create supporting documents only when the requested scope covers that detail.
Respect an already accepted broader write request without asking at every level.
Read current state before writing, preserve unrelated content and history, and
read back IDs, content and links. Reconcile uncertain writes before retrying.

Return the agreed goal, success evidence, scope, open decisions and source links.
If the request also covers registering the issues, continue to
[crw-plan](../crw-plan/SKILL.md) in the same operation with that definition and its
write scope, or to [crw-add-issue](../crw-add-issue/SKILL.md) for a single issue.
Definition-only ends here. If scope is advisory, the downstream plan also remains a
proposal. Creating either artifact does not launch execution tasks.
