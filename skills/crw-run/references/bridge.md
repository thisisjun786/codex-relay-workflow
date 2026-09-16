# codex-thread-bridge launch and recovery

Read only when this MCP is the selected transport. Discover current schemas and
call `get_capabilities`; the tool contract overrides this dated reference.
The observations below were checked on 2026-09-14 against bridge 0.1.0 and
App Server 0.154.0. Do not assume future versions share every limitation.

## Capability and transport boundary

The bridge connects to the running App Server on the same host. It is an
alternative MCP interface, not a restored native Desktop tool. Backend project
IDs and Desktop saved-project IDs are not interchangeable.

Observed capabilities: creation, messaging, read/list/wait, and goal reads.
The bridge does not set a persistent goal, guarantee Desktop project membership,
or handle client-side dynamic tools/interactive approvals. CXC availability must
be established inside the created task, not inferred from this capability list.

The task must use the agreed sandbox and approval policy. Do not widen them to
make a launch succeed. Prefer a supported native path if the bridge cannot
represent the required environment.

## Choose the creation path

Before proposing the official Codex managed worktree instead of either path below,
consult [native-worktree-verification.md](native-worktree-verification.md): bridge
capability flags describe this transport, not the desktop client's own feature. Run its
probe only when the current scope authorizes the task creation, resume, writes, pushes,
relay mutations and retention cleanup it performs; otherwise reuse still-applicable
evidence or report the path as unverified.

**Existing task-owned worktree:** `create_thread` accepts an existing absolute
cwd, title, model, reasoning_effort, sandbox, expected_sandbox_policy,
runtime_workspace_roots, optional prompt, and stable request_id. It creates no
worktree. Reuse the coordinator-prepared checkout. Omitting a setting uses the App
Server configuration and checks nothing; supply the ones the assignment depends on,
because an omitted setting is reported but never verified.

**New retained worktree:** `create_worktree_thread` accepts a source checkout,
full immutable commit, absent absolute destination with an existing parent,
explicit `worktree_mode: bridge-managed-retained`, model, reasoning_effort,
sandbox, and the complete expected_sandbox_policy. Honor the tool's explicit
ownership, permission, and prompt authorization requirements; reuse an earlier
authorization that actually covers these choices.

This second path creates a detached, locked checkout, not a branch or
Desktop-managed worktree. Arrange any needed branch according to project policy
before implementation, without changing another worktree's checked-out branch.
Do not create another worktree just to compensate for an unsuitable creation API
when an existing owned checkout is available.

### Placement and ownership of a retained worktree

Place it at `/home/jun/code-worktrees/<original-project>/<task>`, taking the
project segment from the original repository's project name rather than the
directory name of whatever checkout is currently open, and using a short
kebab-case task segment with the branch `codex/<task>`. Reuse the existing
checkout for the same task instead of creating a second one, and preserve any
dirty work and local forks already in it.

The path says where the work is, not who is answerable for it, so record these
separately and never infer one from the other. This is the same set of columns
as OPS-5.2 in [Operations contract](operations.md), which owns the rule.

| Field | Meaning |
|---|---|
| Checkout path | Absolute path of the working tree |
| Created by | Which mechanism created it, such as `bridge-managed-retained` or the coordinator |
| Editing owner | The task id that edits source in it |
| Git metadata owner | Who may create branches and commits for it |
| Retention owner | Who decides how long it stays |
| Cleanup authorization | Which cleanup is authorized, and `none automatic` when none is |

A `bridge-managed-retained` checkout is not Desktop-managed and is not bound to
a Desktop project. Nothing deletes or archives it automatically, and retention
belongs to the parent that asked for it.

Creating a worktree does not change what the task inside it may do. A child
created with the capability to commit owns its branch, its commits, its push and
its pull request. Where a running task cannot write git metadata, the coordinator
prepares the branch before dispatch and makes the commits while that child edits
source and returns a frozen diff. Neither path widens a running task's
permissions. Capability at creation is OPS-5.5 and delivery ownership is OPS-9 in
[Operations contract](operations.md).

## Model and settings verification

Pass exact model/effort overrides only where the current schema supports them.
Do not invent an argument or change global configuration. For an explicit effort
on an API with no override, use an already-permitted path or effective
configuration that actually applies the requested values. If none does, report the
concrete unsupported capability and settle it before creating the task; never
launch a task already known to carry the wrong setting, and never silently
downgrade it. This is not a readiness handshake: once the settings can be applied,
create with the full work prompt, and reconcile a mismatch observed after creation
on that same task.

Compare actual cwd, workspace roots, model, reasoningEffort, approvalPolicy, and
the full sandbox/permission response. Do not infer OCX routing from a model ID
or the generic modelProvider label. Missing served-provider proof stays unknown.

These same returned settings are what a relay records as a task's authorized
execution settings, so a later delivery preserves them instead of inheriting a host
default. Record BOTH sides from their own receipts; do not ask a task for them. Most
fields sit at the top level of the response, but the environments selection is nested
under the created thread rather than beside them, and the reported active permission
profile is recorded as the profile a later resume is expected to match. Field names
and the exact record shape are in [codex-session-relay](relay.md).

Create with the full work prompt so the assignment is dispatched once. Use
`send_message_to_thread` for a later correction or recovery, never to resend the
same assignment: it is a separate intentional mutation with its own request_id, it
refuses active tasks, and it takes an optional `expected_settings` carrying the
authorized cwd, sandbox, expected_sandbox_policy, model, reasoning_effort and
runtime_workspace_roots. Supply it: the resume then carries those settings and is
read as an observation, and a difference or a setting the host does not report
withholds the message instead of dispatching it. Omitting it keeps the old
behaviour, where nothing is checked and the receipt says `not_requested`. An
unrecognised key is refused rather than ignored. Verify the returned settings on
each mutation and reconcile a mismatch on the same task.

The `settings` receipt describes what the host reported at creation or at the
resume, not a guarantee about the dispatched turn: no host-side exclusivity is
held, so it says `observed_at_creation` or `observed_at_resume` rather than
verified.

### Observed empty-task failure

Creating with no initial prompt returned an ID/settings but left no source
rollout. Subsequent `send_message_to_thread` failed at `thread/resume`:

```text
-32600 invalid paginated history lineage ... missing source rollout
```

Read/turn listing also failed for those empty IDs. A later create with an initial
prompt yielded a recorded user message/turn and appeared in Desktop. This proves
creation and dispatch, not successful CXC execution or universal follow-up support.

On that version, prefer creation with the full work prompt.
Do not recommend “empty creation, then resume” as a verified workflow.

## Receipts and safe recovery

Choose one stable request_id per intended mutation and keep it across uncertainty.
Persist the exact arguments privately if needed for replay. Reusing an ID with
changed arguments is an error; a new ID is not a harmless retry.

- `accepted`: requested API steps returned; the task may still be running.
- `failed`: inspect the actual rejection and any retained worktree/task/turn IDs.
- `outcome_unknown` or `in_progress_or_unknown`: reconcile before another mutation.

Use `get_operation`, backend reads/listing, and actual worktree state to identify
what occurred. If a writer may still be active or the delivery outcome is unknown,
do not start a replacement. Reuse an existing delivery when it can complete the
assignment. After reconciliation proves no prior writer remains, recover within
the assignment and host permissions as described in [Integrations](../../crw-plan/references/integrations.md#completion-follow-up-in-an-existing-execution-workflow).
A permitted replacement uses a new ID; record why and preserve earlier work and
receipts. A read failure alone does not prove the task never existed.
Do not delete receipts, reset shared work, or edit a session database to retry.

Read/list/wait are observational and never resume a task. Bridge waiting uses the
exact returned thread_id and turn_id, at most 50 seconds per call; timeout leaves
work running. Paginate with the exact returned cursor. A completed turn may have
failed; inspect status/error and returned artifacts.

Check app listing independently for visibility/project association. If Desktop
tools cannot find the task, retain the backend ID and disclose the limitation;
never substitute a guessed Desktop project ID.
