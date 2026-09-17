---
name: crw-loop
description: "Keep one Linear project's fixed parent coordinating its agreed scope: fill independent ready capacity, observe issue children, verify and integrate deliveries, and continue with successors. Use for a project coordination loop or resuming that run; crw-run supplies dispatch and review operations. Children keep their own implementation workflow."
---

# CRW Loop

One parent coordinates one project; one independent child owns one issue and its
one delivery PR. This skill owns the parent's repetition and completion decision.
Use [crw-run](../crw-run/SKILL.md) for operation selection, ownership, parallel
scheduling, task packets, delivery verification and authorized integration. Read
its linked [shared rules](../crw-plan/references/integrations.md) and applicable
[operations](../crw-run/references/operations.md). These are operations within this
same parent, not a request to start another coordinator or recursively invoke skills.

## Enter or resume

A submitted `$crw-loop <Linear project link>` execution request has the same scope
and authority as the project shorthand in `crw-run`: designate or restore the fixed
parent with [crw-focus](../crw-focus/SKILL.md), then coordinate the agreed scope.
Status, explanation, quoted examples, automatic skill discovery and unsubmitted UI
prompts do not start execution. Explicit batch, milestone, no-create, no-goal,
no-merge, pause, model and resource limits survive routing and resumption.

Read the existing project coordination record and live ownership before acting.
Restore project/parent IDs, agreed issue scope and finish boundary, permissions,
child/turn IDs, worktrees, PRs and revisions, observation mode, pending receipts,
blockers and next actions. Reuse compatible children and reconcile uncertain sends;
a missing transcript or elapsed wait does not permit a second writer. Keep future
backlog outside this run unless the user expands its scope.

The default parent uses this CRW coordination loop, without initializing a CXC
implementation goalplan or FSM. Children retain their effective workflow, normally
CXC Loop; load CXC development skills for development/review work as applicable.
Parent delivery evidence comes from verified scoped results and integrations,
not a source diff in the parent's checkout or a child's phase state.

An explicitly requested CXC parent workflow still follows its installed lifecycle.
If this parent already has an active or blocked CXC goal/FSM, inspect it and preserve
its binding, goalplan, evidence and recovery record. Do not reset it, edit phases,
fabricate a source delta, mark it complete, or silently switch owners to avoid a
guard. Reconcile an authorized transition using that workflow's supported lifecycle;
if unsupported, record the concrete transition blocker before starting CRW execution.
This skill does not initialize a host goal merely because its name contains Loop.
Any explicitly requested host goal remains subject to host completion/blocked rules
and the actual hooks governing it; do not claim an independent lifecycle if those
hooks still require CXC phases.

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
   issue state within existing authority, then release newly ready successors.
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
but does not make the whole project complete. Cancellation is not successful delivery.

Otherwise stop only for a user stop, a stated resource limit, or a concrete blocker
leaving no authorized action available. Preserve unfinished issues, owners, artifacts,
receipts, blocker evidence and the exact resume step in the same record. Continue
independent scoped work before declaring the whole run blocked.

This is an agent-followed loop, not a daemon or a new wake mechanism. If the host ends
the turn or no usable wait/resume path remains, record an interrupted run and the
manual resume step; do not promise unattended progress. On resumption, refresh the
same record and ownership, then continue the first actionable obligation.
