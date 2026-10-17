---
name: crw-work
description: "Use for class-scaled work and completion evidence on one task. Explicit explanation, interview-only, plan-only, and read-only limits win; loading the skill does not activate execution or continuation. Triggers: task work, 단계별로."
---

# Task work (formerly PABCD)

Carry one authorized task through the current development flow: **수령 → 탐색 →
설계 → 비평 → 구현 → 단위 점검 → 크로스체크 → PR → 머지** (intake, explore,
design, critic, implement, unit check, crosscheck, PR, merge). Work class and the
requested scope determine which work is needed. The stored task-state letters
are compatibility values, not names for these flow stages.

`crw-dev` owns work class, the C0/C1 fast path, fresh proof and safety. A small
in-place repair does not acquire a full project workflow merely by loading this
skill. `crw-plan` decomposes agreed goals into milestones and issues; `crw-check`
audits delivered work against its canonical intent. Scoped persistence belongs to
[crw-continue](../crw-continue/SKILL.md).

Which level does the work and owns its goal is defined by [hierarchy](../crw-canon/references/hierarchy.md#scope-of-each-level), whose [level table](../crw-canon/references/hierarchy.md#the-levels) names the layers.

## Intent and ownership

Explanation, review, interview-only, plan-only, read-only, no-goal, no-FSM and
no-delegation limits remain in force. Loading a skill or approving an outline
does not authorize implementation, publication or a host goal. A bare execution
request for `crw-continue` selects scoped HOTL; ordinary task work does not.

Determine the actual repository root before planning. Resolve source and test
paths against it. Do not invent a tracking issue for standalone work with no
tracking issue or CRW execution binding; the user's request and repository
decisions remain its scope.

A flow-managed node follows the run's packet and
[control ownership](../crw-canon/references/flow/control.md#who-owns-control-in-a-flow-managed-run).
The run owns its design, critic, executor calls, unit checks, crosscheck and
evidence. An executor or oversized child starts no second task-state machine,
goal or continuation loop. [spawn policy](../crw-canon/references/spawn-policy.md)
owns the run's calls; this skill does not add another coordinator.

Unclear registration goes back to `crw-define`. Ambiguity during a run becomes a
decision request to the session above the project session, lifted with
`crw flow report-up` and the evidence file. Continue unblocked work on recorded
assumptions. The default flow starts no synchronous interview; explicit
clarification remains subject to the current request and host capabilities.

## Work through the current flow

| Stage | Work and completion evidence |
| --- | --- |
| 수령 / intake | Read the request, binding, criteria, owner, work class, edit scope and delivery grant. Resolve contradictory authority before dependent work. |
| 탐색 / explore | Inspect the relevant code, tests and prior evidence. Separate observation, hypothesis and confirmed cause. Record the affected surfaces and unanswered questions. |
| 설계 / design | Produce a scoped executable design from that evidence. For a managed run, follow its architect call and approved DESIGN. Planning alone changes no implementation. |
| 비평 / critic | Obtain the required design review before implementation. Resolve or rebut findings with evidence; the later crosscheck does not replace this review. |
| 구현 / implement | Implement the agreed design in bounded changes. Keep scope and ownership intact and surface deviations. |
| 단위 점검 / unit check | Run the relevant tests and the separately invoked review required by the run/repository before its commit. Fix required findings and bind the resulting proof to the actual candidate. |
| 크로스체크 / crosscheck | Check the integrated range and acceptance coverage with the required independent evidence. The run owns its final gate; an executor's assertion is not its receipt. |
| PR | Follow the repository's configured delivery path. A PR-unit project opens its milestone PR; direct landing opens none. A task does not acquire publication permission from this stage name. |
| 머지 / merge | The authorized integration owner verifies and lands the candidate under repository policy. Report the actual landed revision or the pending/blocked outcome, with evidence. |

**Implementation and delivery authority:** Push only as `crw-dev` DEV-GIT-PUSH-01 defines. Local checkpoints follow DEV-GIT-COMMIT-01; if the selected delivery path and design declare a PR stack, use DEV-STACK-02 in `crw-dev` references/stacked-prs.md.

Scale detail and review to the `crw-dev` classifier. C0/C1 keeps its fast path;
C2 needs a compact plan and targeted proof; C3 follows its persistence and
cross-domain risks; C4 keeps independent review, full relevant gates and a durable risk/evidence
record. The flow's high-risk lane supplies those gates when it owns execution.
An evidence-free state transition cannot substitute for the work.

For C2+ unit records or a multi-part roadmap, read
[Implementation units](references/implementation-units.md). These records are
workspace-local execution evidence that quotes the tracking canon when there is
one, or the request/repository source for standalone work. They are not a second
product source of truth. `crw-dev` owns the C0/C1 record exemptions.

## Select supporting guidance

Read the owner needed for the current action, not the whole reference graph.
For tool composition or response projection use
[native execution](../crw-dev/references/native-execution.md), respecting the
available host tools and their output limits.

- Planning detail and scoped outline assessment:
  [Plan guidance](references/phase-plan.md),
  [Plan output](references/plan-output.md), and
  [Assessment guidance](references/phase-audit.md). Their stored state codes
  describe compatibility records; they do not rename the current flow stages.
- Reachable verification, render evidence and conditional paths:
  [Validation guidance](references/phase-check.md). Passing an unrelated check
  does not establish the requested result.
- Repeated failures and optimization:
  [Loop engineering](references/loop-engineering.md) and, only for optimization
  or mechanism comparison, [Optimization rules](references/optimization.md).
  Preserve the existing repair/replan boundaries and accept/rebut synthesis;
  do not turn a rejected result into success or weaken its criteria.

## Delegation

Start from the binding under
[DISPATCH-MANAGED-01](references/dispatch-surfaces.md#dispatch-managed-01-strict--start-from-the-binding-not-from-the-mechanism).
A managed issue/node stays with its owning flow or supported `crw-run` path.
Duplicate-assignment or capacity refusal is not permission to create a thread
by hand. An unresolved binding does not establish unmanaged scope.

For standalone work with no tracking issue and no CRW execution binding, and for
bounded helper work, choose the actual surface before dispatch. A subagent is a
leaf in the current checkout: no host goal, task-state machine or further spawning
by default. A separate task needing its own branch/worktree or merge/CI lane uses
the supported task/thread surface with its own scope. Concurrent writers need
disjoint edit scopes and never perform branch-level Git operations concurrently.

Read [Dispatch surfaces](references/dispatch-surfaces.md) and
[Delegation](references/delegation.md) before an authorized dispatch. The main
session keeps its own plan and transitions. Explicit no-delegation and scoped
read/write restrictions win. For independently user-owned peers use
[peer collaboration](../crw-dev/references/peer-collaboration.md); contact neither
transfers authority nor reactivates another task's goal.

## Existing task-state records

The public command namespace is `crw work`: `state` inspects or controls task
state, `outline` handles task plans, `assessment` handles assessment-round
records, and `continue` handles authorized continuation. Other verb names stay
as listed in the canonical command map. These command spellings require the
corresponding runtime; changing this skill does not install or activate it.

For an authorized existing-state operation read
[State control](references/phase-control.md), which owns session identity,
chat/CLI differences, attestations and edge-specific artifacts. SESSION-IDENTITY-01
requires the current native binding, not a copied parent or historical id.
Preserve native-cwd and foreign-session refusals. Hooks may guard an operation;
they do not perform the work or advance a state on the agent's behalf.

Legacy values `IDLE`, `I`, `P`, `A`, `B`, `C`, `D` remain stored values until the
explicit migration. Their human record labels are inactive, clarify, outline, assess,
change, validate and finish. Never reinterpret them as flow journal stages,
rewrite old receipts, or claim that a source rename migrated live state.

The storage sentences below remain unchanged pending the state migration:

The following historical hook/storage description retains its original command
spelling for that migration; new command examples use the namespace above.

> State lives in `.crw/sessions/<sessionId>.json` plus `.crw/ledger.jsonl`; transitions are driven by the `pabcd-state` hook component, the chat-side `orchestrate` command (human free-pass), and the live `crw pabcd orchestrate` terminal CLI (agent-gated).

- `.crw/sessions/<sessionId>.json` — current phase (IDLE/I/P/A/B/C/D), derived flags, injection dedupe, and bounded interview tracker.
- `.crw/ledger.jsonl` — append-only audit trail of transitions.
- `.crw/interviews/<sessionId>.jsonl` — shipped append-only Interview Q/A capture (and scan-evidence) ledger, written by the PostToolUse `request_user_input` hook.

## Finish or hand back

Use fresh, relevant proof for the actual scope; report failed, missing and
unobserved evidence separately. Record the outcome for a reader who was not in
the task, following [Reader documents](../crw-dev/references/reader-documents.md).
Say what changed, what was verified, what remains and which action is next.
Completion, publication, integration and live activation remain distinct facts.
