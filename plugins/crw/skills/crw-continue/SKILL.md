---
name: crw-continue
description: "Use to continue one task to its authorized completion condition. Bare crw-continue means HOTL; explicit explanation, interview-only, plan-only, read-only, or HITL limits win. A milestone request goes to crw-run. Triggers: crw-continue, continue until done, HOTL, 끝까지 해줘, docs-first."
---

# crw-continue — Scoped completion

Continue the authorized task using [crw-work](../crw-work/SKILL.md) and the current
flow: **수령 → 탐색 → 설계 → 비평 → 구현 → 단위 점검 → 크로스체크 → PR → 머지**.
Persistence does not create another workflow or replace the run's journal with
a legacy task-state cycle. The legacy letters are stored compatibility values,
not names for the current flow's stages.

This skill runs one task's objective. A milestone request goes to [crw-run](../crw-run/SKILL.md); which level owns the work and its goal is defined by [hierarchy](../crw-canon/references/hierarchy.md#scope-of-each-level), whose [level table](../crw-canon/references/hierarchy.md#the-levels) names the layers.

## Intent before activation

- An explanation, review, quoted mention or skill load is not an execution request.
  Do not create a goal, mutate task state or implement merely to explain.
- Explicit interview-only, plan-only, read-only, no-writes, no-commits, no-goal,
  no-FSM, no-tests and no-delegation limits win. A planning request authorizes
  planning; it does not authorize the later implementation or delivery stages.
- A bare operative `crw-continue` request selects HOTL persistence within the
  agreed objective. Explicit HITL preserves its human decision points. Loading
  this skill does not turn an ordinary request into HOTL.
- HOTL changes persistence, not permissions. It grants no unrelated work, new
  access, publication, release, deployment, installation or destructive action.
  A standing repository grant still applies within its actual scope.

Managed execution follows the run's packet and control owner. A leaf does not
open a host goal, run `crw work state`, or dispatch another layer. An independent
task retains its own scope, native binding and evidence; no coordinator or lane
advances another task's state. Standalone work with no tracking issue or CRW
execution binding remains possible without inventing an issue.

## Discover the current owner

Read a selected SKILL.md before relying on it, then only its applicable references.
Resolve relative paths from the owning skill directory. Recover truncated output
before the governed action; exit 0 is not a complete read. After context loss
reload the needed owner rather than the whole skill family.

| Condition | Read before acting |
| --- | --- |
| Development or governed review | [crw-dev](../crw-dev/SKILL.md) and the relevant surface owners |
| Task work or a task-work plan | [crw-work](../crw-work/SKILL.md) and the selected guidance |
| Tool composition or response projection | [Native execution](../crw-dev/references/native-execution.md) |
| Continue or recover an existing standalone goal/state binding | [Runtime lifecycle](references/runtime-lifecycle.md); this is not a second state machine for a flow node |
| Register, inspect or amend a durable goalplan | [Durable goalplan](references/durable-goalplan.md) |
| Multiple outcome units or later scope discovery | [Implementation units](../crw-work/references/implementation-units.md) |
| Repeated failure or disputed evidence | [Loop engineering](../crw-work/references/loop-engineering.md) |
| Optimization, plateau or mechanism comparison | [Optimization](../crw-work/references/optimization.md) and loop engineering |
| Deliberate comparison of alternatives | [Divergence tiers](references/divergence-tiers.md) |
| Independent lanes or helper delegation | [Dispatch surfaces](../crw-work/references/dispatch-surfaces.md), starting at DISPATCH-MANAGED-01 |
| Authorized task/lane dispatch | [Lane dispatch](references/lane-dispatch.md) and [Delegation](../crw-work/references/delegation.md) |
| Waiting for work or external processes | [Waiting](references/waiting.md) |

The native installed-skill listing is discovery; external catalog search does not
prove an installed skill is available. A missing mandatory owner/capability is a
preflight limitation, not permission to synthesize receipts or imitate a hook.

## Execute and continue

Follow the current stage's owner and record its real outcome. The run owns managed
design, review, execution, verification and acceptance. Do not start a second
task-state machine for a flow-managed node. For an explicitly authorized
standalone state/goal operation, runtime lifecycle and state control retain the
existing guards and wire values; naming changes neither activate them nor migrate
their records. Do not claim continuation is armed without its actual binding and
the required runtime evidence.

SESSION-IDENTITY-01 uses the current SessionStart binding, corroborated by
`crw relay session current` where supported. Resolve a missing or conflicting
binding through the supported bind route in the verified native cwd. Never set
the native environment id, borrow another session's id or replay hook JSON.
Binding a session is not evidence that its hooks ran.

LOOP-CONTINUE-01: after a completed outcome unit or context loss, reread the bound
plan, journal/ledger and remaining criteria. Continue the next authorized unit
if work remains; do not stop merely because one command, review or context window
ended. A newly discovered in-scope unit amends the same objective
(LOOP-UNIT-CHAIN-01), while a different objective needs its own authorization.

LOOP-CONTINUITY-01: the next unit quotes the previous conclusion and direction,
and records why a changed direction is justified. Resume from durable evidence,
not transcript momentum. A state advance, an empty approval or a Stop release
does not establish completion.

LOOP-GIT-01 points to `crw-dev` §5. Keep authorized work checkpointed, choose the
repository's actual delivery path, and apply DEV-GIT-PUSH-01 to publication.
Neither persistence nor a plan-only request expands the grant.

## Multi-unit work

LOOP-DOCS-FIRST-01 remains the default for a multi-unit continuation and strict
for HOTL: establish the durable roadmap and its documents before production
implementation. Its first outcome unit is documentation only; use the current
flow's relevant planning and review stages and record the verified result.
Do not describe this as a new flow stage named with a legacy letter.

A genuinely single-unit task skips this extra unit, and `crw-dev` keeps C0/C1's
fast path. A correction generation, base refresh or separated publication step
does not become the first unit of new work. If broader scope emerges later,
settle the roadmap debt before implementing that scope. No empty skeleton or
unwritten unit document establishes a usable roadmap.

LOOP-READS-WORK-01: before multi-unit execution read the implementation-units
reference. `crw-continue` owns when the roadmap is required; `crw-work` owns
DIFFLEVEL-ROADMAP-01, PHASE-SPLIT-01, LEXICO-SPLIT-01 and UNIT-RESIDENCE-01.
The record names and state encodings in compatibility references do not rename
the current flow or authorize a second loop.

## Questions, failure and completion

Keep optional asynchronous questions independent from approval gates, under
[Async user questions](../crw-dev/references/async-questions.md). An unanswered
optional question is not a completion blocker; a required answer/approval remains
required. Continue useful work that does not depend on it.

Keep this task's work local. Do not send unsolicited notices to other tasks.
[Peer collaboration](../crw-dev/references/peer-collaboration.md) limits contact
to authorized requests or necessary confirmed blocking coordination under the
host's permissions. An incoming question is not a new execution request.

Preserve fresh-proof gates, the existing repair/replan boundaries and
accept/rebut synthesis. Do not weaken criteria to pass GOAL-COMPLETE-GATE-01.
For recovery use the relevant runtime-lifecycle or loop-engineering owner.

Report the real outcome: DONE, NOOP, BLOCKED, UNSAFE, NEEDS_HUMAN or
BUDGET_EXHAUSTED. These are report outcomes, not new flow stages, stored phases
or host goal statuses. DONE requires current proof for all applicable criteria.
Resource exhaustion requires an actual stated bound. A wait timeout, compaction
or missing capability is not success or evidence of exhausted authority.
