# Goal mode

Goal mode is the one optional execution mode of [crw-run](../SKILL.md): the fixed project parent holds a
native host goal, and the goal plus the host's automatic continuation carry the run across turns. It is
entered only where the user explicitly asks for it. Since CRW-165's 2026-09-21 decision a project parent
runs goal-free by default: it ends its turn when only waiting remains and a delivered relay event resumes
it. Loading this reference does not make the goal the default again, and an ordinary project execution
request is Run's, not goal mode's.

Two terms are kept apart throughout. A **loop** is the PABCD completion loop of one task, owned by
`crw-loop` and `crw-pabcd`. **Goal mode** is this
mode of the project parent. A request that says "loop" and names a Linear project to coordinate is
goal mode's, not a loop's, and the answer says so: `$crw-loop` with a project scope and coordination
intent starts no loop and hands the request to Run goal mode. A request that asks for goal mode in
other words, such as a parent goal for the project, is the same request. The loop skills also say
"goal mode" of a task whose own host goal is active; that is a task's goal, a different thing from
this mode, which belongs to the project parent.

Three rules follow, and every section below keeps them.

1. **Explicit request only.** A parent goal exists only where the user asked for one: a submitted
   goal-mode request, the `$crw-loop` handoff above, or the restoration of a goal the parent already
   holds. Binding-only, status, explanation, quoted examples, automatic skill discovery and unsubmitted
   UI prompts authorize neither a goal nor an execution.
2. **The parent never follows the loop procedure.** The project parent does not follow the crw-loop or
   crw-pabcd procedure, with or without a goal: it creates no implementation goalplan or FSM, enters no
   PABCD phase, and runs no `crw pabcd orchestrate` or `crw pabcd loop init` for itself. The parent's
   native goal tracks verified results and integrations for the agreed scope and needs no source diff in
   the parent checkout.
3. **A child's workflow is named `crw-loop`.** Children keep their own implementation workflow, the
   PABCD loop, and a packet names it `crw-loop`. The parent coordinates children; it does not adopt a
   child's goal or FSM.

One parent coordinates one project; one independent child owns one issue and its delivery, one issue per
delivery today under the repository's [work-unit rules](../../../../../POLICY.md#work-units-review-and-integration),
under the shared
[supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope).
Run and goal mode have the same project scope. Goal mode adds the parent's host goal, automatic
continuation and goal completion decision. Use [crw-run](../SKILL.md) for operation selection, project
binding, ownership, parallel scheduling, task packets, delivery verification and authorized integration,
with its linked [shared rules](../../crw-plan/references/integrations.md) and applicable
[operations](operations.md). These are operations within this same parent, not a request to start
another coordinator or recursively invoke skills.

## Enter or resume

A submitted `$crw-run <Linear project link>` execution request that asks for a goal, or the `$crw-loop`
handoff above, establishes the project parent's native goal and automatic execution of the agreed scope.
That request is what opens the goal, since the parent default opens none, and it is also the only
occasion for one. Designate or restore the fixed parent using
[Project parent binding](../../crw-plan/references/integrations.md#project-parent-binding), then follow
[Parent goal lifecycle](#parent-goal-lifecycle) before dispatch. Run alone already advances through
in-scope successors; goal mode adds the goal and automatic host continuation. A goal-mode run may also
own an explicitly limited milestone or batch while keeping one project parent.

For binding-only requests, perform the shared binding procedure and return. Explicit no-goal,
read-only, no-create, no-merge, pause, model and resource limits survive routing. No-goal prevents
activation of goal mode: report that limit, and continue as the ordinary goal-free Run, which the
default already authorizes. Do not silently substitute Run and call it goal mode, and do not report the
default as a degraded goal mode when it is simply the normal mode.

A parent already holding a goal is not migrated by pausing it. A paused goal makes the parent
undeliverable, so pausing to reach the default is what strands the assignment; the supported routes are
in [Retiring a goal a parent already holds](#retiring-a-goal-a-parent-already-holds).

An initiative is not a goal-mode target. A designation to execute an initiative's approved projects
binds at that level through [Initiative supervision](initiative-supervision.md), and this lifecycle
stays the project parent's: it creates no supervisor goal, and no goal for any other task.

Read the existing project coordination record and live ownership before acting. Restore project and
parent IDs, agreed issue scope and finish boundary, permissions, child and turn IDs, worktrees, PRs and
revisions, observation mode, pending receipts, blockers and next actions. Reuse compatible children and
reconcile uncertain sends; a missing transcript or elapsed wait does not permit a second writer. Keep
future backlog outside this run unless the user expands its scope.

That record includes the determined execution mode, and a resume reads it back rather than deciding
again from scratch: re-deciding after a context loss is how one assignment acquires two routes. Where it
is missing, determine it once under
[Determine the execution mode](../SKILL.md#determine-the-execution-mode) and record it before
dispatching or correcting anything. An unavailable relay is recorded as such, never as a silent fallback
to direct, and direct sends are never counted as relay completion evidence.

A goal is not what makes the role: the parent is the task bound to the project ID under
[supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope),
so opening or closing one changes what wakes this task rather than which level it is. Load the `crw-dev*`
skills for development and review work as applicable.

The goal lifecycle below owns preflight, create or reuse, blocked recovery and completion. Inspect
existing `.crw/` state and effective hooks before activation. An unsupported transition or a hook that
forces implementation phases is a compatibility blocker, not permission to reset state, bypass a guard
or claim goal mode is active.

## Repeat within the agreed scope

1. **Refresh and fill capacity.** Read current issue dependencies, ownership and delivery evidence.
   Actively find independent ready issues under `crw-run`'s parallel scheduling rules and dispatch up to
   supported capacity. Do not wait for a whole batch when a slot becomes free. A blocked issue holds its
   dependents; keep unrelated authorized work moving. Existing assignments take precedence.
2. **Observe.** Before dispatch or registration, select and record the compatible path in
   [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting). The active-parent path uses bounded
   task and turn transport waits with actual child IDs. After a timeout, refresh observations, use
   available capacity and wait again. A quiet child or an empty ready queue while owned work runs is not
   a blocker or completion. Do not resend work or create replacements merely to obtain a response.
3. **Verify and correct.** Inspect the reported revision, criteria, CI and review findings using
   `crw-run`. Send in-scope corrections to the existing child. For a relay-owned assignment, reconcile
   its actual receipt, ACK and verdict through the relay; observing a transcript is not delivery. Record
   remaining obligations explicitly. Route a correction by what the child is doing rather than waiting
   for it to go idle: an idle child takes the ordinary message path, and a child mid-turn is steered into
   its verified active turn. Re-read and reclassify when the turn changes underneath. An explicit stop is
   the supported goal pause followed by steering the observed turn to finish safely; ordinary corrections
   neither interrupt a child nor change its goal. Acceptance of an instruction is not a read, an effect,
   a receipt, an ACK or a verdict.
4. **Integrate and advance.** Serialize authorized merges into each shared target, recheck candidate and
   base and verify landing. Update the coordination record and issue state within existing authority,
   then release newly ready successors. Each Run pass returns to goal mode; it does not end the parent
   objective. Keep one implementation issue per delivery today (a task branch and its head, or a pull
   request where one is named), under the repository's
   [work-unit rules](../../../../../POLICY.md#work-units-review-and-integration); a child report or green
   CI alone is not Done.

Keep the record current after meaningful transitions, with the latest evidence and next action rather
than a growing transcript. Give concise progress updates under host communication rules. User steering
refines the run; status questions alone do not cancel it. Never automatically resume a user-paused or
cancelled task.

## Wait, finish or hand off

Return idle as an event-driven handoff once every readiness fact under OPS-8.1 holds, and use active
bounded observation where one of them does not. A goal-mode parent reaches that idle only after its own
bounded Stop budget releases, which is a cost the goal-free default does not pay and which a usage
comparison between the two modes is measuring. Record the pending action and how the parent will resume
before yielding. Preserve existing registered delivery paths; a busy-parent relay incompatibility is a
concrete blocker, not a reason to fake ACK or bypass it.

Finish when every obligation in the agreed scope meets its verified delivery boundary and no owned work,
correction, receipt or integration remains pending. For implementation scope that includes integration,
verify each required delivery landed on its integration branch; non-PR work requires its agreed result
evidence. A no-merge or batch boundary can finish that limited run, but does not make the whole project
complete. Then close the matching parent goal under [its lifecycle](#parent-goal-lifecycle).
Cancellation is not successful delivery.

Otherwise stop only for a user stop, a stated resource limit, or a concrete blocker leaving no
authorized action available. Host goal status changes must follow [the goal lifecycle](#parent-goal-lifecycle),
including its blocked threshold; do not mark a goal blocked on the first failed wait. Preserve
unfinished issues, owners, artifacts, receipts, blocker evidence and the exact resume step in the same
record. Continue independent scoped work before declaring the whole run blocked.

The host goal supplies persistence; bounded waits and a supported host continuation or verified relay
path supply execution. Goal mode installs no daemon or wake mechanism. If the host ends the turn or no
usable wait or resume path remains, record an interrupted run and the manual resume step; do not promise
unattended progress. On resumption, refresh the same record and ownership, then continue the first
actionable obligation.

## Parent goal lifecycle

This is the lifecycle for the fixed project parent's native host goal. It uses no implementation
goalplan and borrows no child's goal. Read the exposed tool schemas and effective hook behavior;
unsupported actions stay unsupported.

This lifecycle is the project parent's. A supervisor's goal is not defined here, and nothing in this file
transfers to initiative scope; the roles themselves are in
[Supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope).

## Preflight before starting workers

1. Establish the current parent thread, project, agreed issue scope, finish boundary and allowed actions
   from the coordination record. Inspect its existing `.crw/` state and source binding read-only.
   Preserve active or blocked state, unfinished goalplans and unresolved evidence; use their owning
   workflow's supported transition before starting a CRW goal. An unsupported transition blocks
   activation.
2. Inspect the exposed native goal tools and hooks for create, continuation and completion. On hosts
   exposing `get_goal`, `create_goal` and `update_goal`, use those tools in this parent. A bridge's
   read-only goal API is not goal-write support. A hook that prevents a goal from activating at all is a
   compatibility blocker even for a fresh parent. A hook that lets it activate and then directs an
   active goal into PABCD implementation phases is the measured goal-idle case instead: activation
   stands, the directive is declined, and [Record the start adjudication](#record-the-start-adjudication)
   holds that narrowing. Record the installed version and path, the observed rule and the required
   supported fix. Do not disable the hook, replay hook events, create fake PABCD evidence or initialize
   a placeholder implementation cycle.
3. Verify the observation and continuation path for this run under OPS-8.1 before dispatch. Keep goal
   activation, direct task waits, host automatic continuation and relay delivery and resume as separate
   facts. A created goal alone does not prove wake-up. If a required capability is absent or unknown,
   report the gap; do not launch a goal-free substitute or claim automation. Scope-authorized read-only
   diagnosis may continue while activation is blocked.

## Record the start adjudication

The preflight above runs at start, on recovery and after an App Server restart, and its outcome is
recorded rather than repeated as a new user decision in every session. Write it into the coordination
record as the start-policy record: `run_mode` and `host_compatibility` are the fields this lifecycle
owns, and the delivery-path and approval-policy results land beside them in `observation_path` and
`approval_policy`, together with the installed identities each was read at and the issue that owns an
unresolved blocker. Restore it from there on the next entry. A later session re-reads what those values
stand on and decides again only where one differs, under the trigger list in
[Start policy](start-policy.md).

The recorded mode says which of these actually holds, in the words it will be reported in:

| Recorded mode | What it means |
| --- | --- |
| `goal-free-run` | The project parent default. No parent goal. Run carries the agreed scope. What brings the parent back is recorded separately in `observation_path`, because this field answers only whether a goal exists. |
| `goal` | A native parent goal is active under this lifecycle, because the user explicitly asked for one. |
| `blocked` | The parent can neither hold a goal it needs nor proceed without one. No child is created. |

`goal-free-run` is the default rather than a substitute, and it is never reported as goal mode. It does
carry a claim of unattended continuation, but only the conditional one the readiness facts in
[Start policy](start-policy.md#before-a-parent-may-wait-idle) establish: where those facts hold the
parent waits idle and a delivered event resumes it, and where they do not it keeps bounded observation
and claims nothing. Where goal mode was explicitly requested, its goal cannot activate, and the user
declines the default in its place, the mode is `blocked`, the unresolved blocker is cited against the
issue that owns it, and no child is created first. For the goal and Stop-hook conflict that issue is
[CRW-29](https://linear.app/jun786/issue/CRW-29); its resolution is what clears the blocker, and until it
is recorded there the blocker still stands.

Disabling a hook, replaying hook events, manufacturing PABCD evidence and force-completing an existing
goal are forbidden above. They are also not offered to the user as options, because presenting one as a
choice is how it becomes an approved plan.

Since CRW-165's decision of 2026-09-21 a project parent opens no native goal by default. It waits idle
with no goal and a delivered relay event resumes it, while still building no goalplan or FSM. That
supersedes the 2026-09-20 role decision, under which the parent created or reused its own goal as the
default and its continuation was the bounded Stop nudge recorded below.

Both superseded steps are kept rather than deleted, because a reader meeting a parent that still holds a
goal has to tell a carried-over arrangement from a current answer. The order was: a parent ran goal-free
unless a goal was separately requested; then the parent goal became the default; now the parent is
goal-free again, with the difference that this time the waiting is carried by a delivery path with
recorded readiness rather than by nothing at all.

A goal still exists where the user explicitly asks for one. It opens on no completed project and no
unapproved backlog item, it never closes on a fabricated source change, and an explicit user no-goal
limit now agrees with the default rather than fighting it.

Preflight above classifies a hook that routes an active native goal into PABCD implementation phases as a
compatibility blocker. For the measured goal-idle behaviour that rule is narrowed rather than ignored,
because activation and continuation fail differently: the goal does activate and reads back active, and
what the block degrades is durable continuation, which is recorded as a bounded nudge. So this behaviour
does not block activation, while a hook that actually prevents a goal from activating, or that cannot be
declined without violating this contract, remains a blocker under that step. Step 2 above now carries the
same split, so the two are read together rather than against each other.

Activation is not continuation. On the measured installation the goal activates, and the
Stop-continuation that follows is a bounded nudge carrying an unconditional PABCD directive that a
coordination parent declines. [Start policy](start-policy.md) records that mechanism and its budget, the
three compatibility facts including approval-policy compatibility, the five pieces of evidence that must
stay separate, and what is returned to CRW-29 when the host cannot support the goal at all. The table
below still governs an existing paused, blocked or differently scoped goal.

## Create or reuse the parent's goal

Read `get_goal` first, then choose the matching case:

| Observed state | Action |
| --- | --- |
| No goal, or previous goal actually complete | Create the project parent's goal with `create_goal`, then read it back. |
| Matching active goal | Reuse it with the same scope; refresh the coordination record and live child ownership. |
| Matching blocked/paused goal | Preserve it. Use an exposed, authorized resume action if supported, then verify active status; otherwise report the exact manual resume requirement. |
| Different unfinished goal | Report the ownership/scope conflict. Never overwrite it, call it complete or create a duplicate to make room. |
| Unreadable/uncertain goal state or mutation result | Read/reconcile the existing goal before retrying. Do not assume absence or submit a second create. |

A useful objective identifies the project and scope snapshot, the delivery boundary, coordination record,
and explicit limits. For example: “Coordinate project <id>'s agreed issues <ids/milestone snapshot>: reuse
one child per issue, dispatch independent ready work, verify results and integrate permitted deliveries
into their intended targets. Finish when all scoped obligations and receipts are reconciled. Preserve
<limits>.” State in plain words that this is a coordination goal, a project parent goal that carries no
implementation loop, so that a later session reads it before anything else. Keep credentials and raw
transcripts out. Do not grow scope from a later backlog scan.

Use `objective` and only fields allowed by the tool. Set `token_budget` only when the user explicitly
supplies a token budget and the host supports it; never invent one. An incompatible budget guard is a
reported conflict, not a reason to discard the limit. Record the returned identity and status where
available and verify the objective and active status. Wording in a document is not a created goal. Do not
call `crw pabcd loop init` or enter PABCD merely to support this parent goal. A child's loop setup remains
separate.

## Retiring a goal a parent already holds

A parent created before the 2026-09-21 decision may still hold an active goal. It is carried over rather
than wrong, and it is retired at a boundary rather than switched off mid-flight.

What decides the procedure is that the relay reads goal status as an input to deliverability.
`codex_session_relay.lifecycle` treats `paused`, `usageLimited` and `budgetLimited` as blocking, so a
delivery for a paused recipient is withheld with `recipient_paused`. An absent goal, an active one, a
blocked one and a completed one are all deliverable when the task is idle.

| Goal state | Ends its turn cleanly | Deliverable when idle | Use |
| --- | --- | --- | --- |
| absent | yes, there is no active goal for the Stop hook to block on | yes | the default every new parent starts in |
| `active` | no, the Stop behaviour blocks first and spends a turn per block | yes | a carried-over goal-mode parent, until its scope boundary |
| `blocked` | yes | yes | an interim, only where the host's own threshold is genuinely met |
| `paused` | yes | **no** | a real user stop, and never a migration step |

**Pausing is not the transition.** It looks like the supported move and it is the one that strands the
parent: every event for its assignments is withheld, nothing wakes it, and the assignment goes quiet
without anything reporting a fault. The pause guard is correct and stays exactly as it is — it protects a
task a person deliberately stopped — and it is simply not the tool for this.

So the supported routes are these, and no other. A parent whose agreed scope is genuinely delivered
completes its goal honestly and starts goal-free the next time. A parent mid-scope keeps its active goal,
runs as `goal`, and pays the Stop cost until that boundary. A parent may record `blocked` where the host's
actual threshold is met, which both ends its turn and stays deliverable, but that is an interim carrying a
reason and an ending condition rather than a destination, because a parent waiting on a dispatched child
is working as designed and calling that blocked misinforms whoever reads it next.

Where a parent is found already paused, nothing here resumes it: that is a human decision under
[OPS-8.2](operations.md#ops-82-busy-paused-cancelled-and-archived-parents). Its pending relationships are
preserved, its withheld deliveries wait rather than expire, and the report names the paused state, the
relationships waiting on it, and the exact resume the user has to perform.

## Repeat, complete or recover

The existing goal remains active across Run passes. Refresh scope, child ownership, delivery revisions,
receipts and next actions in the same coordination record. Returning from a Run pass does not complete
the goal. On host continuation or authorized resume, read the goal and record first, then continue the
next available in-scope action.

Only call `update_goal(status="complete")` after all agreed obligations are verified and no owned child
work, correction, integration or receipt remains pending. Re-read and verify the result; a rejected
mutation leaves the goal unfinished. Record the actual reason and supported recovery rather than altering
unrelated state.

Use `update_goal(status="blocked")` only under the host's actual threshold. On the current native tool
contract this requires the same blocker across at least three consecutive goal turns with no meaningful
authorized progress possible; a resumed blocked goal starts a fresh three-turn audit. One child waiting or
blocked is not a project blocker while independent scoped work remains. Do not manufacture turns or rapid
retries to reach a count; use real continuations and record concrete evidence.

User stop or pause and resource limits are not successful completion or automatic blocked status. Honor
them immediately using the host's supported controls; `update_goal` is not a pause, resume or budget API.
If the needed control is unavailable, report the required user action and preserve pending work. Never
resume a user-paused task automatically.

Record goal ID and status, scope and limits, pending owners, results and receipts, the last meaningful
transition, continuation mode and exact resume step. A hook rejection, expired wait or host interruption
is not completion. Preserve that record even if unattended continuity cannot be maintained, and
distinguish interruption from the host goal's actual status.
