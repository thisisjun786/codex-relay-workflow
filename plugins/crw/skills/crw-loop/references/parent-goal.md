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
   A hook that routes any active native goal into CXC implementation phases is a
   compatibility blocker even for a fresh parent. Record the installed version/path,
   observed rule and required supported fix. Do not disable the hook, replay hook
   events, create fake CXC evidence or initialize a placeholder implementation cycle.
3. Verify the observation/continuation path for this run under OPS-8.1 before dispatch.
   Keep goal activation, direct task waits, host automatic continuation and relay
   delivery/resume as separate facts. A created goal alone does not prove wake-up.
   If a required capability is absent or unknown, report the gap; do not launch a
   goal-free substitute or claim automation. Scope-authorized read-only diagnosis may
   continue while activation is blocked.

## Record the start adjudication

The preflight above is performed once and its outcome is recorded, not repeated as a new user
decision in every session. Write it into the coordination record as the `run_mode` and
`host_compatibility` fields of [Start policy](../../crw-run/references/start-policy.md), together
with the installed identities it was read at and the issue that owns an unresolved blocker, and
restore it from there on the next entry. A later session re-reads those identities; it decides
again only where one of them differs.

The recorded mode says which of these actually holds, in the words it will be reported in:

| Recorded mode | What it means |
| --- | --- |
| `loop` | A native parent goal is active under this lifecycle. |
| `goal-free-run` | No parent goal. Run carries the agreed scope, and no automatic resume is claimed or proven. |
| `blocked` | Activation is blocked and no substitute is approved. No child is created. |

A goal-free substitute is recorded and reported as `goal-free-run`. It is never reported as a Loop,
and it never carries a claim of unattended continuation. Where preflight blocks activation and the
request does not separately cover goal-free Run, the mode is `blocked`, the unresolved blocker is
cited against the issue that owns it, and no child is created first. For the goal and Stop-hook
conflict that issue is [CRW-29](https://linear.app/jun786/issue/CRW-29); its resolution is what
clears the blocker, and until it is recorded there the blocker still stands.

Disabling a hook, replaying hook events, manufacturing CXC evidence and force-completing an
existing goal are forbidden above. They are also not offered to the user as options, because
presenting one as a choice is how it becomes an approved plan.


Since the 2026-09-20 role decision a project parent creates or reuses its own goal for the approved
project scope as its default and uses automatic continuation, while still building no CXC goalplan
or FSM and never fabricating a source change to close it. That supersedes the temporary arrangement
in which a parent ran goal-free unless a Loop was separately requested, and it carries activation
authority for a parent already running. It opens no goal on a completed project or an unapproved
backlog item, and an explicit user no-goal limit is a different thing that still wins.

Preflight above classifies a hook that routes an active native goal into CXC implementation phases
as a compatibility blocker. For the measured goal-idle behaviour that rule is narrowed rather than
ignored, because activation and continuation fail differently: the goal does activate and reads
back active, and what the block degrades is durable continuation, which is recorded as a bounded
nudge. So this behaviour does not block activation, while a hook that actually prevents a goal from
activating, or that cannot be declined without violating this contract, remains a blocker under
that step. Recording this narrowing here does not rewrite the step itself; the two are read
together and the wording of the step still needs its own coordinated update.

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
| No goal, or previous goal actually complete | Create the explicitly requested new goal with `create_goal`, then read it back. |
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
