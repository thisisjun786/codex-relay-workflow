# Parent goal lifecycle

This is the lifecycle for the fixed project parent's native host goal. It does not
use a CXC implementation goalplan or borrow any child's goal. Read the exposed tool
schemas and effective hook behavior; unsupported actions stay unsupported.

This lifecycle is the project parent's. A supervisor's goal is not defined here, and nothing in
this file transfers to initiative scope; the roles themselves are in
[Supervisor, parent and child scope](../../crw-plan/references/integrations.md#supervisor-parent-and-child-scope).

## Preflight before starting workers

1. Establish the current parent thread, project, agreed issue scope, finish boundary
   and allowed actions from the coordination record. Inspect its existing CXC state
   and source binding read-only when CXC is installed. Preserve active/blocked state,
   unfinished goalplans and unresolved evidence; use their owning workflow's supported
   transition before starting a CRW goal. An unsupported transition blocks activation.
2. Inspect the exposed native goal tools and hooks for create, continuation and
   completion. On hosts exposing `get_goal`, `create_goal` and `update_goal`, use
   those tools in this parent. A bridge's read-only goal API is not goal-write support.
   A hook that prevents a goal from activating at all is a compatibility blocker even
   for a fresh parent. A hook that lets it activate and then directs an active goal into CXC
   implementation phases is the measured goal-idle case instead: activation stands, the directive
   is declined, and [Record the start adjudication](#record-the-start-adjudication) holds that
   narrowing. Record the installed version/path,
   observed rule and required supported fix. Do not disable the hook, replay hook
   events, create fake CXC evidence or initialize a placeholder implementation cycle.
3. Verify the observation/continuation path for this run under OPS-8.1 before dispatch.
   Keep goal activation, direct task waits, host automatic continuation and relay
   delivery/resume as separate facts. A created goal alone does not prove wake-up.
   If a required capability is absent or unknown, report the gap; do not launch a
   goal-free substitute or claim automation. Scope-authorized read-only diagnosis may
   continue while activation is blocked.

## Record the start adjudication

The preflight above runs at start, on recovery and after an App Server restart, and its outcome is
recorded rather than repeated as a new user decision in every session. Write it into the
coordination record as the start-policy record: `run_mode` and `host_compatibility` are the fields
this lifecycle owns, and the delivery-path and approval-policy results land beside them in
`observation_path` and `approval_policy`, together with the installed identities each was read at
and the issue that owns an unresolved blocker. Restore it from there on the next entry. A later
session re-reads what those values stand on and decides again only where one differs, under the
trigger list in [Start policy](../../crw-run/references/start-policy.md).

The recorded mode says which of these actually holds, in the words it will be reported in:

| Recorded mode | What it means |
| --- | --- |
| `goal-free-run` | The project parent default. No parent goal. Run carries the agreed scope. What brings the parent back is recorded separately in `observation_path`, because this field answers only whether a goal exists. |
| `loop` | A native parent goal is active under this lifecycle, because the user explicitly asked for one. |
| `blocked` | The parent can neither hold a goal it needs nor proceed without one. No child is created. |

`goal-free-run` is the default rather than a substitute, and it is never reported as a Loop. It
does carry a claim of unattended continuation, but only the conditional one the readiness facts in
[Start policy](../../crw-run/references/start-policy.md#before-a-parent-may-wait-idle) establish:
where those facts hold the parent waits idle and a delivered event resumes it, and where they do
not it keeps bounded observation and claims nothing. Where an explicit Loop was requested, its goal
cannot activate, and the user declines the default in its place, the mode is `blocked`, the
unresolved blocker is cited against the issue that owns it, and no child is created first. For the
goal and Stop-hook conflict that issue is [CRW-29](https://linear.app/jun786/issue/CRW-29); its
resolution is what clears the blocker, and until it is recorded there the blocker still stands.

Disabling a hook, replaying hook events, manufacturing CXC evidence and force-completing an
existing goal are forbidden above. They are also not offered to the user as options, because
presenting one as a choice is how it becomes an approved plan.

Since CRW-165's decision of 2026-09-21 a project parent opens no native goal by default. It waits
idle with no goal and a delivered relay event resumes it, while still building no CXC goalplan or
FSM. That supersedes the 2026-09-20 role decision, under which the parent created or reused its own
goal as the default and its continuation was the bounded Stop nudge recorded below.

Both superseded steps are kept rather than deleted, because a reader meeting a parent that still
holds a goal has to tell a carried-over arrangement from a current answer. The order was: a parent
ran goal-free unless a Loop was separately requested; then the parent goal became the default; now
the parent is goal-free again, with the difference that this time the waiting is carried by a
delivery path with recorded readiness rather than by nothing at all.

A goal still exists where the user explicitly asks for one. It opens on no completed project and no
unapproved backlog item, it never closes on a fabricated source change, and an explicit user
no-goal limit now agrees with the default rather than fighting it.

Preflight above classifies a hook that routes an active native goal into CXC implementation phases
as a compatibility blocker. For the measured goal-idle behaviour that rule is narrowed rather than
ignored, because activation and continuation fail differently: the goal does activate and reads
back active, and what the block degrades is durable continuation, which is recorded as a bounded
nudge. So this behaviour does not block activation, while a hook that actually prevents a goal from
activating, or that cannot be declined without violating this contract, remains a blocker under
that step. Step 2 above now carries the same split, so the two are read together rather than
against each other.

Activation is not continuation. On the measured installation the goal activates, and the
Stop-continuation that follows is a bounded nudge carrying an unconditional PABCD directive that a
coordination parent declines. [Start policy](../../crw-run/references/start-policy.md) records that
mechanism and its budget, the three compatibility facts including approval-policy compatibility,
the five pieces of evidence that must stay separate, and what is returned to CRW-29 when the host
cannot support the goal at all. The table below still governs an existing paused, blocked or
differently scoped goal.

## Create or reuse the parent's goal

Read `get_goal` first, then choose the matching case:

| Observed state | Action |
| --- | --- |
| No goal, or previous goal actually complete | Create the project parent's goal with `create_goal`, then read it back. |
| Matching active goal | Reuse it with the same scope; refresh the coordination record and live child ownership. |
| Matching blocked/paused goal | Preserve it. Use an exposed, authorized resume action if supported, then verify active status; otherwise report the exact manual resume requirement. |
| Different unfinished goal | Report the ownership/scope conflict. Never overwrite it, call it complete or create a duplicate to make room. |
| Unreadable/uncertain goal state or mutation result | Read/reconcile the existing goal before retrying. Do not assume absence or submit a second create. |

A useful objective identifies the project and scope snapshot, the delivery boundary,
coordination record, and explicit limits. For example: “Coordinate project <id>'s
agreed issues <ids/milestone snapshot>: reuse one child per issue, dispatch independent
ready work, verify results and integrate permitted PRs into their intended targets.
Finish when all scoped obligations and receipts are reconciled. Preserve <limits>.”
Keep credentials and raw transcripts out. Do not grow scope from a later backlog scan.

Use `objective` and only fields allowed by the tool. Set `token_budget` only when the
user explicitly supplies a token budget and the host supports it; never invent one.
An incompatible budget guard is a reported conflict, not a reason to discard the limit.
Record the returned identity/status where available and verify the objective and
active status. Wording in a document is not a created goal. Do not call `cxc loop init`
or enter PABCD merely to support this parent goal. Child CXC setup remains separate.

## Retiring a goal a parent already holds

A parent created before the 2026-09-21 decision may still hold an active goal. It is carried over
rather than wrong, and it is retired at a boundary rather than switched off mid-flight.

What decides the procedure is that the relay reads goal status as an input to deliverability.
`codex_session_relay.lifecycle` treats `paused`, `usageLimited` and `budgetLimited` as blocking,
so a delivery for a paused recipient is withheld with `recipient_paused`. An absent goal, an active
one, a blocked one and a completed one are all deliverable when the task is idle.

| Goal state | Ends its turn cleanly | Deliverable when idle | Use |
| --- | --- | --- | --- |
| absent | yes, there is no active goal for the Stop hook to block on | yes | the default every new parent starts in |
| `active` | no, the Stop behaviour blocks first and spends a turn per block | yes | a carried-over Loop, until its scope boundary |
| `blocked` | yes | yes | an interim, only where the host's own threshold is genuinely met |
| `paused` | yes | **no** | a real user stop, and never a migration step |

**Pausing is not the transition.** It looks like the supported move and it is the one that strands
the parent: every event for its assignments is withheld, nothing wakes it, and the assignment goes
quiet without anything reporting a fault. The pause guard is correct and stays exactly as it is —
it protects a task a person deliberately stopped — and it is simply not the tool for this.

So the supported routes are these, and no other. A parent whose agreed scope is genuinely delivered
completes its goal honestly and starts goal-free the next time. A parent mid-scope keeps its active
goal, runs as `loop`, and pays the Stop cost until that boundary. A parent may record `blocked`
where the host's actual threshold is met, which both ends its turn and stays deliverable, but that
is an interim carrying a reason and an ending condition rather than a destination, because a parent
waiting on a dispatched child is working as designed and calling that blocked misinforms whoever
reads it next.

Where a parent is found already paused, nothing here resumes it: that is a human decision under
[OPS-8.2](../../crw-run/references/operations.md#ops-82-busy-paused-cancelled-and-archived-parents).
Its pending relationships are preserved, its withheld deliveries wait rather than expire, and the
report names the paused state, the relationships waiting on it, and the exact resume the user has
to perform.

## Repeat, complete or recover

The existing goal remains active across Run passes. Refresh scope, child ownership,
PR revisions, receipts and next actions in the same coordination record. Returning
from a Run pass does not complete the goal. On host continuation or authorized resume,
read the goal and record first, then continue the next available in-scope action.

Only call `update_goal(status="complete")` after all agreed obligations are verified
and no owned child work, correction, integration or receipt remains pending. Re-read
and verify the result; a rejected mutation leaves the goal unfinished. Record the
actual reason and supported recovery rather than altering unrelated state.

Use `update_goal(status="blocked")` only under the host's actual threshold. On the
current native tool contract this requires the same blocker across at least three
consecutive goal turns with no meaningful authorized progress possible; a resumed
blocked goal starts a fresh three-turn audit. One child waiting or blocked is not a
project blocker while independent scoped work remains. Do not manufacture turns or
rapid retries to reach a count; use real continuations and record concrete evidence.

User stop/pause and resource limits are not successful completion or automatic blocked
status. Honor them immediately using the host's supported controls; `update_goal` is
not a pause/resume/budget API. If the needed control is unavailable, report the required
user action and preserve pending work. Never resume a user-paused task automatically.

Record goal ID/status, scope/limits, pending owners/results/receipts, the last meaningful
transition, continuation mode and exact resume step. A hook rejection, expired wait or
host interruption is not completion. Preserve that record even if unattended continuity
cannot be maintained, and distinguish interruption from the host goal's actual status.
