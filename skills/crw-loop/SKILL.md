---
name: crw-loop
description: "Create or restore a project parent goal and automatically repeat CRW execution through the agreed scope: dispatch independent issue children, observe, verify, integrate and advance. Use for a project coordination loop or resuming that run; crw-run supplies dispatch and review operations. Children keep their own implementation workflow."
---

# CRW Loop

One parent coordinates one project; one independent child owns one issue and its
one delivery PR. This skill owns the parent's host goal, automatic repetition and completion decision.
Use [crw-run](../crw-run/SKILL.md) for operation selection, ownership, parallel
scheduling, task packets, delivery verification and authorized integration. Read
its linked [shared rules](../crw-plan/references/integrations.md) and applicable
[operations](../crw-run/references/operations.md). These are operations within this
same parent, not a request to start another coordinator or recursively invoke skills.

## Enter or resume

A submitted `$crw-loop <Linear project link>` execution request explicitly requests
a parent coordination goal and automatic execution of the agreed scope. Designate or
restore the fixed parent with [crw-focus](../crw-focus/SKILL.md), then follow
[Parent goal lifecycle](references/parent-goal.md) before dispatch. Run alone requests
neither this goal nor automatic successor passes. A Loop may also own an explicitly
limited milestone/batch while keeping one project parent.

Status, explanation, quoted examples, automatic skill discovery and unsubmitted UI
prompts do not authorize goal creation or execution. Explicit no-goal, read-only,
no-create, no-merge, pause, model and resource limits survive routing. No-goal prevents
activation of this goal-backed Loop: report that limit, and perform goal-free Run only
if the request separately covers it. Do not silently substitute Run and call it Loop.

Read the existing project coordination record and live ownership before acting.
Restore project/parent IDs, agreed issue scope and finish boundary, permissions,
child/turn IDs, worktrees, PRs and revisions, observation mode, pending receipts,
blockers and next actions. Reuse compatible children and reconcile uncertain sends;
a missing transcript or elapsed wait does not permit a second writer. Keep future
backlog outside this run unless the user expands its scope.

The parent's native goal tracks verified results and integrations for the agreed
scope. It has no CXC implementation goalplan/FSM and needs no source diff in the
parent checkout. Children retain their effective workflow, normally CXC Loop. Load
CXC development skills for development/review work as applicable.

The goal lifecycle reference owns preflight, create/reuse, blocked recovery and
completion. Inspect existing CXC state and effective hooks before activation. An
unsupported transition or a hook that forces implementation phases is a compatibility
blocker, not permission to reset state, bypass a guard or claim the Loop is active.

## Repeat within the agreed scope

1. **Refresh and fill capacity.** Read current issue dependencies, ownership and
   delivery evidence. Actively find independent ready issues under `crw-run`'s
   parallel scheduling rules and dispatch up to supported capacity. Do not wait
   for a whole batch when a slot becomes free. A blocked issue holds its dependents;
   keep unrelated authorized work moving. Existing assignments take precedence.
2. **Observe.** Before dispatch or registration, select and record the compatible
   path in [OPS-8.1](../crw-run/references/operations.md#ops-81-parent-continuation-and-waiting).
   The active-parent path uses bounded task/turn transport waits with actual child
   IDs. After a timeout, refresh observations, use available capacity and wait again.
   A quiet child or an empty ready queue while owned work runs is not a blocker or
   completion. Do not resend work or create replacements merely to obtain a response.
3. **Verify and correct.** Inspect the reported revision, criteria, CI and review
   findings using `crw-run`. Send in-scope corrections to the existing child. For a
   relay-owned assignment, reconcile its actual receipt/ACK/verdict through the relay;
   observing a transcript is not delivery. Record remaining obligations explicitly.
4. **Integrate and advance.** Serialize authorized merges into each shared target,
   recheck candidate/base and verify landing. Update the coordination record and
   issue state within existing authority, then release newly ready successors. Each
   Run pass returns to this Loop; it does not end the parent objective.
   Keep one implementation issue per PR; a child report or green CI alone is not Done.

Keep the record current after meaningful transitions, with the latest evidence and
next action rather than a growing transcript. Give concise progress updates under
host communication rules. User steering refines the run; status questions alone do
not cancel it. Never automatically resume a user-paused or cancelled task.

## Wait, finish or hand off

Use active bounded observation by default. Returning idle is an event-driven handoff
only after the registered assignment, live service and supported parent-resume and
receipt path are verified under OPS-8.1. Record the pending action and how the parent
will resume before yielding. Preserve existing registered delivery paths; a busy-parent
relay incompatibility is a concrete blocker, not a reason to fake ACK or bypass it.

Finish when every obligation in the agreed scope meets its verified delivery boundary
and no owned work, correction, receipt or integration remains pending. For implementation
scope that includes integration, verify each required PR landed; non-PR work requires
its agreed result evidence. A no-merge or batch boundary can finish that limited run,
but does not make the whole project complete. Then close the matching parent goal
under [its lifecycle](references/parent-goal.md). Cancellation is not successful delivery.

Otherwise stop only for a user stop, a stated resource limit, or a concrete blocker
leaving no authorized action available. Host goal status changes must follow
[the goal lifecycle](references/parent-goal.md), including its blocked threshold; do not
mark a goal blocked on the first failed wait. Preserve unfinished issues, owners, artifacts,
receipts, blocker evidence and the exact resume step in the same record. Continue
independent scoped work before declaring the whole run blocked.

The host goal supplies persistence; bounded waits and a supported host continuation
or verified relay path supply execution. This skill installs no daemon or wake mechanism. If the host ends
the turn or no usable wait/resume path remains, record an interrupted run and the
manual resume step; do not promise unattended progress. On resumption, refresh the
same record and ownership, then continue the first actionable obligation.
