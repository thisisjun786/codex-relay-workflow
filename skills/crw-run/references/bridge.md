# codex-thread-bridge launch and recovery

Read only when this MCP is the selected transport. Discover current schemas and
call `get_capabilities`; the tool contract overrides this dated reference.
The observations below were checked on 2026-09-14 against bridge 0.1.0 and
App Server 0.154.0. Do not assume future versions share every limitation.

## Capability and transport boundary

The bridge connects to the running App Server on the same host. It is an
alternative MCP interface, not a restored native Desktop tool. Backend project
IDs and Desktop saved-project IDs are not interchangeable.

Observed capabilities: creation, messaging, read/list/wait, goal reads, steering an
active turn, and pausing a goal. The bridge does not write a goal objective or
budget, expose an interrupt or a turn queue, guarantee Desktop project membership,
or handle client-side dynamic tools/interactive approvals. CXC availability must
be established inside the created task, not inferred from this capability list.

What this bridge exposes and what the host supports are two different facts, and
`get_capabilities` reports them in separate `exposure` and `hostSupport` blocks.
A tool missing from the list is a statement about the bridge only: the host
protocol has `turn/interrupt` and a turn queue, and this bridge withholds both on
purpose. `hostSupport` names the version these paths were built against and the
server actually connected, and reports `unknown_host_version` when they disagree
rather than inheriting the tool list. A `-32601` means the connected host lacks
that method; it is never recorded as a capability absent everywhere, and it never
justifies falling back to a different action. Never report a bridge gap as a host
gap, and never claim host support this bridge has not established.

There is a third question, and it is the one a dated reference cannot answer: which
build is actually installed. `exposure` describes the bridge whose `get_capabilities`
you called. That is not necessarily the version described here, and not necessarily
the version the host registered when it last started its MCP servers. A path this
document describes can be missing from the running server simply because the installed
build predates it, and from the caller's side that looks identical to a bridge
withholding it on purpose. So read `exposure` from the live session rather than from
this file, and when a path is unavailable separate the three causes before reporting
one: not exposed by the installed build, not supported by the connected host, or
exposed and supported but refused for this particular request. Which build is installed
is its own claim with its own evidence, classified under
[OPS-2.2](operations.md#ops-22-five-classes-and-their-rules).

## Reach a task that is already working

A running peer is instructed, not waited out. Four actions stay distinct: a
message starts a turn on an idle task, a steer adds input to a turn already
running, an interrupt stops a turn and is not exposed here, and a goal pause
changes goal status without stopping anything. Ordinary communication uses the
first and never the others, so routine coordination cannot interrupt a peer or
change its goal.

Read the task's status and active turn with `get_active_turn` before choosing.
The host's active status carries `activeFlags` and no turn id, so the id is
derived from the newest turn still reported in progress. Then:

| Observed | Action |
|---|---|
| `active` with a turn id | `steer_thread` with that exact id |
| `idle` | `send_message_to_thread` as before |
| `notLoaded` or `systemError` | Neither; read again, and treat a system error as its own problem |
| status and turn list disagree | The turn changed between reads. Read again and reclassify. |

One correction takes exactly one route. Do not send a message and a steer for the
same instruction, and do not re-send after a rejection: a refused
`expectedTurnId` means the turn moved on, so re-read and decide again. Carry the
sender's identity and the issue or revision inside the steered text, because it
arrives as ordinary input in someone else's turn.

Three claims stay separate. The receipt says the host accepted the input into the
guarded turn. That the peer read it is only visible in the task's own transcript,
and that the peer acted on it is only visible in its delivered work. A steer never
substitutes for a completion receipt, an acknowledgement, or a verification result,
and it changes no ownership: one parent still holds one project and one child one
issue.

After an uncertain send, reconcile from records rather than sending again. The
steer is recorded under `clientUserMessageId` `steer:<request_id>`; replay the same
request id for the retained receipt and read the turn's items for that id.

For an explicit stop, pause and then finish the turn. `pause_goal` sends status
only and cannot alter an objective or budget; it refuses a task with no goal,
reports `already_paused` without writing, and refuses a goal that already ended.
It is not atomic — the protocol has no expected-status precondition — so read the
goal again afterwards, and the receipt says so itself. Pausing does not stop a turn
in flight: re-read the active turn and steer it to finish safely. A paused,
cancelled or archived task is still never resumed automatically.

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
worktree. Reuse the coordinator-prepared checkout. Model and reasoning effort are
required; omitting either refuses the request before any call rather than inheriting
the App Server configuration. Omitting any other setting uses that configuration and
checks nothing, so supply the ones the assignment depends on: an omitted setting is
reported but never verified.

**New retained worktree:** `create_worktree_thread` accepts a source checkout,
full immutable commit, absent absolute destination with an existing parent,
explicit `worktree_mode: bridge-managed-retained`, model, reasoning_effort,
sandbox, and the complete expected_sandbox_policy. Honor the tool's explicit
ownership, permission, and prompt authorization requirements; reuse an earlier
authorization that actually covers these choices. The pair is authorized before any
Git work, so a refused launch leaves no worktree behind.

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

State the model and the reasoning effort on every mutation that starts a turn:
`create_thread`, `create_worktree_thread` and `send_message_to_thread`. `steer_thread` and
`pause_goal` start no turn and select no model, so they accept neither argument and need no
authorization. Do not invent an argument
or change global configuration. For an explicit effort on an API with no override, use
an already-permitted path or effective configuration that actually applies the requested
values. If none does, report the concrete unsupported capability and settle it before
creating the task; never launch a task already known to carry the wrong setting, and
never silently downgrade it. This is not a readiness handshake: once the settings can be
applied, create with the full work prompt, and reconcile a mismatch observed after
creation on that same task.

Where the host has configured an execution policy, the stated pair must also be one the
operator approved, and an unapproved one is refused before anything is created. Read
`get_capabilities` before creating: it reports whether an allowlist is in force and a
digest identifying it, so an assumption about which pairs are available is never needed.
A per-task exception is cited by id through `policy_exception`; the id, its one model,
its one effort and the directories it covers all live in the host's own file. Naming an
exception is not approving one, and a request cannot carry its own allowance. Ask the
user to declare an exception rather than proposing a different model, and never widen a
global setting to make a launch succeed.

Because an exception is bound to directories, a request citing one must also state the cwd it
applies to. Creation always does. On the resume path cwd is otherwise optional, so a send that
cites `policy_exception` has to include `cwd` in `expected_settings` as well, or it is refused
before the task is read.

A refused request sends nothing and records nothing, so correct the arguments and reuse
the same request_id; a new id for the same intent is not a retry. A receipt retained from
before these arguments were required is reconciled with `get_operation`, which returns
the same receipt bytes, rather than replayed through the tool or replaced.

The refusal for an omitted pair is `execution_setting_missing`, and it is raised before
any RPC is issued, so nothing was created and there is no ledger row to reconcile; that
is why the same request_id stays usable rather than being burned. The retained-receipt
rule has a sharper reason than tidiness. The replay lookup runs BEFORE that
authorization, so invoking the tool again under a retained id returns the old receipt
without re-checking the pair that receipt predates. Reconcile such a receipt; do not
re-invoke it.

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
refuses active tasks, and it requires `expected_settings` carrying at least the
authorized model and reasoning_effort, and optionally cwd, sandbox,
expected_sandbox_policy and runtime_workspace_roots. A turn on an existing task costs
what a new one costs, so a send with no stated pair is refused before the task is even
read. The resume carries those settings and is read as an observation, and a difference
or a setting the host does not report withholds the message instead of dispatching it.
An unrecognised key is refused rather than ignored. Verify the returned settings on each
mutation and reconcile a mismatch on the same task.

The `settings` receipt describes what the host reported at creation or at the
resume, not a guarantee about the dispatched turn: no host-side exclusivity is
held, so it says `observed_at_creation` or `observed_at_resume` rather than
verified.

The `executionPolicy` block beside it records which mode authorized the request and,
where one applied, the exception id. It says what this bridge requested and transmitted.
It is not served-provider evidence, and an installed bridge enforces this only after the
operator has updated and restarted it, which is a separate fact from the source.

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

That ID is the only identifier that exists before the task does, which is what makes it
the management marker the coordinator records. A creation that returns normally hands
back the thread ID in the same receipt, so there is no separate acceptance ID to
resolve afterwards. A creation whose response is lost is the case that matters: it can
leave even the thread ID unknown, and the request_id is then the only route back to
whatever was created.

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
