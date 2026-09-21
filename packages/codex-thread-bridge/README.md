# codex-thread-bridge

An independent MCP server that lets an agent create and message Codex sessions
through the **running App Server on the same host**.

Built for this workflow: an agent in an SSH-backed Desktop task creates another
session on the remote machine, and you open and continue that session in Desktop
on your local computer. The transport and Desktop continuation were demonstrated
with Codex 0.153.4 on Linux. This package provides a reusable MCP interface to that
transport; it does not restore or impersonate OpenAI's native Desktop tools.

## Install

Requires Python 3.11+ (and Git for isolated launches), [uv](https://docs.astral.sh/uv/),
and a running, authenticated Codex App Server with a Unix WebSocket control socket.
Run the bridge as the same
user, on the same host as that App Server. Linux is the validated host platform;
the Unix transport is not a Windows-native transport.

From a clone of this repository:

```sh
uv sync --frozen --no-dev
.venv/bin/codex-thread-bridge --help
codex app-server daemon version
```

Register the server **on the remote Codex host**, replacing the absolute path:

```sh
codex mcp add codex-thread-bridge -- /absolute/path/codex-thread-bridge/.venv/bin/codex-thread-bridge
```

Equivalent entry in that host's `~/.codex/config.toml`:

```toml
[mcp_servers.codex-thread-bridge]
command = "/absolute/path/codex-thread-bridge/.venv/bin/codex-thread-bridge"
tool_timeout_sec = 60
```

Start a fresh SSH-backed task and verify the MCP tool inventory. Loading newly
configured MCP tools into an already-running task depends on the client. The
bridge itself never edits Codex configuration or starts/restarts a daemon.

The default socket is `$CODEX_HOME/app-server-control/app-server-control.sock`,
with `CODEX_HOME` defaulting to `~/.codex`. Override it with `--socket /path/to.sock`.
This socket uses a WebSocket handshake, not newline-delimited JSON. No additional
API key is needed; the existing App Server owns its authentication and model usage.

## Tools

| Tool | Behavior |
| --- | --- |
| `get_capabilities` | Connect and report server identity and bridge limitations |
| `create_thread` | Create one durable session in an existing directory; optionally name it and send its initial prompt |
| `create_worktree_thread` | Create a locked, retained bridge-managed Git worktree at an explicit commit and start a task; no Desktop-managed lifecycle |
| `send_message_to_thread` | Resume an explicitly selected idle thread under stated settings, then send one message |
| `get_active_turn` | Report a thread's status and the turn id a steer would guard, without resuming it |
| `steer_thread` | Put an instruction into the turn a thread is already running, guarded by that turn id |
| `pause_goal` | Pause an active Goal by status alone, without touching its objective or budget |
| `list_threads` | Read a page of unarchived backend thread summaries |
| `read_thread` | Read metadata and a paginated history without resuming, naming what of it could not be received |
| `wait_thread` | Wait up to 50 seconds for the supplied recent turn ID |
| `get_goal` | Read persistent Goal state |
| `get_operation` | Recover a mutation receipt after a lost response or client restart |

Text limits apply to display content such as messages, previews, and summaries.
Pagination cursors, IDs, paths, and other protocol fields are returned unchanged.

### Reading a thread that is larger than the transport

The App Server bounds no response by size. It builds a whole response before it writes any of
it, and one page of ten turns in its full item view measured 754 MB on a real thread, taking the
host 96.7 seconds whether or not the client accepts a byte of it. Reducing the turn limit does
not help, because one turn's full view was 82 MB on its own. So `read_thread` never asks for
that view. It reads metadata without turns, then a page of turns in the host's summary view —
each turn's user and agent messages — and then the newest turn's most recent real items in a
bounded page, asked for newest-first and returned in the order they happened, so that a turn
larger than one page keeps its latest activity readable rather than its oldest.
When an oversized frame closes the connection anyway, it narrows and asks again, and it reports
what it ended up with rather than raising.

Every response carries an `observation` block naming the view its turns actually carry
(`summary`, `notLoaded`, or none at all), how many turns item detail was requested for, and how
many of those it actually arrived for — asking is not seeing, and the two are counted separately
so a host that refuses the item read cannot be reported as one that answered it. Each turn
carries an `itemsDetailStatus`: `not_requested`, `complete`, `partial`, `narrowed`,
`not_observed`, `method_unavailable`, or `refused`. None of these fails the read and none is a
claim about the thread — a page this bridge could not receive never means a task finished,
stalled, or must be run again. Where an item page will not arrive even one item at a time,
`not_observed` says so plainly instead of offering a narrower query that does not exist.

A response frame past the client's 16 MiB limit raises `ResponseTooLarge`, which names the frame
size, the limit and what was in flight — but never which request it belonged to. A frame is
refused from its header, before any id in it is read, and it may be a notification that never
carried one.

For the first page, omit `cursor`. For later pages, pass `nextCursor` as the exact
string returned, even when it looks like JSON. The MCP cursor argument accepts a
string, not `null`; an empty string also selects the first page. Restart the MCP
server and rediscover its schemas after upgrading to this cursor handling.

Client-rendered tool names include the configured MCP server namespace. Tool
arguments and receipts are this bridge's API, not a drop-in copy of native Desktop
schemas. Clients should discover the tools and use their declared input schemas.

Example tool arguments (these are MCP calls, not shell commands):

```json
{
  "request_id": "demo-create-001",
  "cwd": "/absolute/path/to/project",
  "title": "Bridge validation",
  "sandbox": "read-only",
  "model": "anthropic/claude-opus-5",
  "reasoning_effort": "xhigh",
  "prompt": "Do not use tools or edit files. Reply exactly: BRIDGE_READY"
}
```

Pass that object to `create_thread`. Keep the returned `threadId` and `turnId`;
use them with `wait_thread`. After checking the session in Desktop, use
`send_message_to_thread` with a **new** request ID for the intentional follow-up:

```json
{
  "request_id": "demo-message-001",
  "thread_id": "<returned threadId>",
  "message": "Do not use tools or edit files. Reply exactly: BRIDGE_FOLLOWUP_OK",
  "expected_settings": {"model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
}
```

Creation defaults to `read-only` and approval policy `never`. `workspace-write`
and `danger-full-access` are explicit options; obtain authorization for the
chosen environment before calling. `model` and `reasoning_effort` are required on the three
mutations that start a turn, `create_thread`, `create_worktree_thread` and
`send_message_to_thread`, and are never inherited from the server's configuration.
`steer_thread` and `pause_goal` start no turn and take neither.

## Which model a task may run on

A creation that omits its model does not fail. `thread/start` has no required model
parameter, so the App Server starts the task on its own configured default and paid
inference begins before anyone can see which model is answering. This bridge refuses that
instead, and it keeps two questions apart.

**Was a pair stated?** Enforced on every host, with no way to switch it off. A creation or
a resume that does not carry both `model` and `reasoning_effort` is refused before any
request is sent. The failure this prevents is an omission, and a guard you can forget to
enable is not a guard against forgetting.

**Was the pair approved?** Only a host that configured a policy file can answer that, so
the allowlist is compared only where one exists. `get_capabilities` reports which of the two
modes is in force, and so does the receipt of every mutation that selects a model, so nobody
assumes the stronger one. `steer_thread` and `pause_goal` select none: they put input into a
turn that is already running, or change a goal's status, so they take no pair, authorize
nothing, and carry no authorization record that might imply otherwise.


**Was it this role's pair?** The two questions above can both be answered correctly by a task
that is still on the wrong model. Callers that run several levels of work give each level its own
pair, and nothing related a task's role to the pair it was started on: a project parent created
on another level's model passed presence and the allowlist and was still wrong. So a caller may
name the role it is creating for, through `role`, and a named role is compared against the pair
this host declares for it.

The values live in the file and nowhere else. This package ships no pair for any role, because a
pair written into code would compete with the operator's file and would answer the approval
question on a host that configured nothing. A role the policy does not declare is refused rather
than defaulted, and `get_capabilities` reports the declared roles so a caller can state the pair
this host expects instead of one it remembered.

Naming a role is opt-in and the argument is appended to a request only when supplied, so a caller
that names none behaves exactly as it did and every retained receipt still replays. `allowed` is
optional once `roles` is declared: an allowlist constrains every task on the host, so requiring
one in order to declare roles would narrow unrelated work as a side effect.

`supervisor` is the one role that declares no pair. Its model is the user's own selection, so it
carries `{"expectation": "record"}` and its authorization is whatever was recorded for it; a file
that tries to pin it does not load, and no other role may declare `record` and exempt itself from
the check. An exception may also name a `role`, and then only a request naming that role may cite
it — a directory is not a task identity, so without this an exception written for one task could
be cited by anything else working in the same place.

One further rule applies when the host reports a thread as `notLoaded`. A resume transmits the
settings, and on a thread the host has to materialize first it may apply them, so an agreeing
echo cannot be told apart from the host repeating the request; the receipt records
`statusBeforeResume` and `echoIndependence: "not_established"` for that case. Where a role was
named and its pair was NOT compared against a declared role pair — a supervisor, or any request
citing an exception, which exists to skip that comparison — the send is refused instead, because
transmitting such a pair could restore a value the user has since changed. A send that names no
role is not covered: this package reads no role binding of any kind, so it cannot tell an unnamed
supervisor from a task with no role, and refusing both would stop unrelated work. A caller that
can resolve the binding owns that refusal.

Point `CODEX_THREAD_BRIDGE_EXECUTION_POLICY` at a JSON file to configure one:

```json
{
  "allowed": [
    {"model": "anthropic/claude-opus-5", "efforts": ["xhigh"]},
    {"model": "openai/gpt-5.6-sol", "efforts": ["high"]},
    {"model": "devin/swe-2", "efforts": ["max"]}
  ],
  "roles": {
    "supervisor": {"expectation": "record"},
    "parent": {"model": "devin/swe-2", "reasoningEffort": "max"},
    "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}
  },
  "exceptions": {
    "one-task": {
      "model": "openai/gpt-6-astra",
      "reasoningEffort": "high",
      "cwd": ["/absolute/path/to/that/checkout"],
      "role": "parent",
      "reason": "why you allowed it; never returned to a caller"
    }
  }
}
```

Efforts are scoped to their model, so this file approves `opus/xhigh` and `sol/high` and
refuses `opus/high`, which nobody wrote down. It is read once at startup from this
process's environment; a file that is configured and cannot be used stops the server
rather than degrading to presence-only. Directories are compared by exact string
equality, so an entry that is absolute but not canonical is refused at startup rather
than loading cleanly and then matching nothing.

A declared role pair is still asked the allowlist question — only an exception skips that — so
where both sections are present, `allowed` has to approve every pair `roles` declares. A file
that declares `parent` on a model its own allowlist omits describes a role nobody could create,
and it is refused at startup rather than at the first creation attempt. Neither section is made
to win: letting `roles` authorize its own pair would make editing it a way to widen the
allowlist, and letting `allowed` win would silently unmake a role.

An exception is a **name, not a value**. The operator writes the id, its one model, its
one effort and the directories it covers; a caller may cite that id through
`policy_exception` and nothing else. An id nobody wrote is refused, and an id whose pair
or directory does not match is refused. That is why the allowlist lives in a file rather
than a tool argument: a caller has no parameter through which to widen its own allowance,
and a caller that can approve itself has not been checked by anyone.

Because the exception is bound to directories, a request citing one must state the directory it
applies to. Creation always does. `send_message_to_thread` otherwise treats `cwd` as optional, so
a send citing `policy_exception` must put `cwd` in its `expected_settings` too, and is refused
before the thread is read when it does not.

Refusals happen before any request is sent and before the ledger records anything, so a
refused call consumes no `request_id`: fix the arguments and reuse the same one. A receipt
retained from before these arguments were required cannot be replayed through the tools,
because its fingerprint no longer matches; read it with `get_operation` and never start a
replacement under a new ID.

This constrains what this bridge requests and what the host reports back. It is not
evidence that a provider served the model, because the host echoes an arbitrary effort
string unchanged. Anyone who can rewrite the policy file or this process's environment
can change the allowlist, and another client of the same App Server is not covered at all.

## Settings, and what the host lets you check

`create_thread`, `create_worktree_thread` and `send_message_to_thread` all take the
execution settings explicitly, transmit them, and return what the host actually
reported under `settings`:

| Setting | How it is transmitted |
| --- | --- |
| `cwd`, `model`, `runtime_workspace_roots` | Their own protocol parameters |
| `reasoning_effort` | `config.model_reasoning_effort`; the protocol has no effort parameter |
| sandbox kind | The `sandbox` mode string |
| `expected_sandbox_policy` write roots and flags | `config.sandbox_workspace_write.*`; the mode string cannot carry them |
| `approval_policy` | Nothing. It is DECLARED, never transmitted: the resume carries no `approvalPolicy`, so a thread's own policy is preserved and then judged against the declaration |

The `settings` receipt reports `requested`, the host's `actual` values, which
fields were `verified`, and a `verification` state. Anything omitted is absent
from `requested`: it is reported but never claimed about, so silence is never
proof of preservation.

For the same reason `send_message_to_thread` rejects an unrecognised
`expected_settings` key instead of ignoring it. Writing `reasoningEffort` where
`reasoning_effort` was meant would otherwise request nothing at all, and the
message would go out under a `not_requested` receipt while the caller believed
the setting had been enforced.

Replaying a `request_id` returns its retained receipt from the ledger and makes
no host call, so recovery works against a server that is offline and a stored
observation is never overwritten by a later one. The exception is a receipt that
records that nothing was begun: a `not_attempted` request has no outcome to
return, so replaying it makes the attempt it never made.

Four outcomes are kept apart, because each needs a different response:

- `setting_untransmittable` — this protocol cannot carry the request, so it is
  refused locally before any call is made. A read-only policy asking for network
  access is the current example; no config spelling applies it.
- `settings_not_preserved` — the host reported a value different from the request.
- `setting_unobservable` — the host reported no value, so nothing says whether the
  setting was applied. This withholds the prompt or message; it is never a warning
  attached to a success.
- A transport or RPC failure stays on the delivery path and lands as `failed`,
  `outcome_unknown` or `not_attempted`: whether the request arrived at all is a
  different question from what the host did with one that did.

Two limits are real and are reported rather than worked around. `turn/start`
returns only the turn, so the bridge binds no setting there and instead verifies
at creation or resume and withholds when the answer disagrees. And the bridge
holds no host-side exclusivity, so `verification` says `observed_at_creation` or
`observed_at_resume` rather than "verified": another client can change a thread's
settings between the observation and dispatch. Where the protocol allows it, a
best-effort `settingsAfterDispatch` re-reads model, effort and cwd after an
accepted turn and flags `concurrentChange`; it covers nothing else and can never
downgrade a turn that was already accepted.

A matching value means the host recorded what was asked for. It is not evidence
that a provider honours it: a deliberately invalid effort string is echoed back
unchanged.

## Delivery and recovery

All mutation tools require a stable `request_id`. The bridge records its intent
before calling the App Server. Repeating that ID with identical arguments returns
the retained receipt; using it with different arguments fails before any action.
Creation fingerprints the supplied directory path before filesystem resolution.
Replaying a retained request therefore works after that directory is removed or
its symlink target changes. The existing-directory requirement applies to new
creations; an intentional new action needs its own request ID.

| Receipt status | Meaning |
| --- | --- |
| `accepted` | Requested API steps returned successfully; a turn may still be running |
| `failed` | A known Git/API rejection or environment mismatch; inspect retained artifacts and IDs |
| `outcome_unknown` | Transport/client failure; some or all effects may have happened |
| `not_attempted` | No state-changing request and no local effect began, so nothing can have happened; the same request ID may be used again |
| `in_progress_or_unknown` | Operation is running, or the process stopped before recording its outcome |

`retrySafe: false` means **do not issue a new request ID to repeat the action**.
Reusing the same ID is safe while the ledger is retained. The bridge never retries
a sent mutation or automatically continues a partially completed create. If the
server created a thread but its response was lost, even its ID may be unknown.
This is conservative deduplication, not an exactly-once guarantee across the
server and the local ledger.

`retrySafe: true` appears only on `not_attempted`, and it reports something
narrower than it sounds: this request began nothing, so repeating it cannot
repeat an effect.

### What counts as an attempt

Every receipt carries `attemptedEffects`, the state-changing steps this process
actually began, in order. It is recorded where those steps happen rather than
where the code that asks for them sits: immediately before a frame is written to
the socket, and immediately before a worktree directory is reserved, created or
checked out. A frame that was begun counts even if `send` then raised, because a
partial write cannot be proven not to have arrived. A method that only asks a
question — `thread/read`, `thread/list`, `thread/turns/list`, `thread/items/list`,
`thread/goal/get`, `project/read`, `initialize` — is not an attempt; anything else is,
including a
method this bridge has not classified, so a mutation added later cannot read as
nothing having happened. `thread/resume` is deliberately treated as a change: it
transmits `cwd`, `model`, sandbox and config, and that the tested host adopts
none of them is an observation rather than a protocol guarantee.

Three limits come with this and are reported rather than worked around.

- A row left behind by a killed process stays `in_progress_or_unknown` and is
  never retried. What a request began is held in memory and dies with it, so the
  row cannot say which side of the send it stopped on.
- A rejection is an answer, so it keeps its request ID. A `failed` receipt that
  began nothing — a preliminary read the host refused, a checkout the Git
  contract rejected — still replays that refusal rather than asking again. Only
  the absence of an answer refunds the ID.
- A state written before it could be known is corrected once it can be.
  `create_worktree_thread` records `initialPrompt: outcome_unknown` before
  dispatching the first turn, so that a process killed mid-dispatch cannot leave a
  receipt claiming the prompt was withheld. When the operation ends, that guess is
  replaced by what actually happened: `not_sent` when `turn/start` never reached
  the socket, `rejected` when the host refused it, and `outcome_unknown` only when
  the frame went out and the answer did not come back.

A re-armed request keeps its history. `attempt` counts the tries and
`priorAttempts` retains the last five, each with the status and error that ended
it.

Receipts persist in `$XDG_STATE_HOME/codex-thread-bridge` (default
`~/.local/state/codex-thread-bridge`), in an endpoint-scoped SQLite database. Use
`--state-dir` to select a stable alternative. Keep this directory across restarts;
deleting it discards deduplication history. Newly created state files are private
to the current user. Receipts may contain conversation metadata/content; the
bridge adds no telemetry and does not publish them.

Socket paths are canonicalized, so symlink aliases to the same socket share a
ledger. When upgrading from a version that hashed the unresolved socket path,
stop older bridge processes and first start the updated bridge with each
previously configured socket spelling and the same state directory. This imports
that spelling's legacy ledger into the canonical ledger without deleting it.
Only then switch to another socket alias. Unknown historical spellings cannot
be recovered from hashed filenames. Conflicting receipts stop startup for manual
inspection rather than choosing one or dispatching again.

Earlier creation fingerprints used a resolved directory path. Legacy receipts
also accept the matching resolved path, so an unchanged symlink continues to
work. If that old symlink has already been removed or retargeted, the original
input spelling cannot be reconstructed and replay may report an argument
conflict. Use `get_operation` with the original request ID to inspect it; do not
create a new ID to retry delivery. New receipts use strict supplied-argument
matching and do not apply this legacy fallback.

Reading, listing, waiting, and Goal inspection never resume or modify a thread.
Messaging explicitly calls `thread/resume` without configuration overrides before
`turn/start`. It refuses a thread observed active, and it refuses one whose approval
policy is not the policy the caller declared in `expected_settings.approval_policy`.
Omitting that key declares `never`, so an interactive thread is still refused unless the
caller names its policy. The resume itself carries no `approvalPolicy` at all, so this
bridge cannot set or change the policy of a thread it did not create; measured on
codex-cli 0.154.0 in both directions, omitting it reports the thread's own policy and
does not inherit the `CODEX_HOME` config default.
Declaring an interactive policy buys delivery, not approval servicing. Unsupported
client-side tool and approval requests receive an explicit error, so this bridge grants
none of them, and it holds no route to the thread's own approver: the protocol has no
method by which a second client hands an approval request to the client that owns the
thread. That is a statement about this bridge, and it stops there: whether the host
independently surfaces the same request to the owning client is not established, so
nothing here claims the approver saw it or that they did not. What is certain is that
nothing this bridge does turns such a request into an approval; continue those tasks in
the client that owns the thread.
Concurrent external clients can still change a thread between those
steps; the App Server remains authoritative. Nothing steers or interrupts on its own.

## Reaching a thread that is already working

Four ways of reaching a task are different actions, and the bridge keeps them apart:

| Situation | Action | What it does |
| --- | --- | --- |
| The thread is idle | `send_message_to_thread` | Resumes it and starts a new turn |
| A turn is already running | `steer_thread` | Adds input to that exact turn; starts nothing |
| You need the turn to stop | not exposed | The host has `turn/interrupt`; this bridge withholds it |
| You need the goal to stop | `pause_goal` | Changes goal status; stops no turn |

Ordinary communication therefore never interrupts a peer and never changes its goal.

Steering is guarded by the turn it targets. The host's active status carries
`activeFlags` and no turn id, so `get_active_turn` derives the id from the newest
turn still reported in progress; pass that exact id to `steer_thread`, which the
host rejects if the active turn has moved on. A rejection means the turn changed,
so read the thread again and reclassify rather than retrying. A thread that is
`idle`, `notLoaded` or in `systemError` is refused by name, because only `idle`
has a delivery path and a system error is not a quiet peer.

An accepted steer says the host took the input into that turn. It does not say the
peer read it, and it does not say the peer acted on it; the receipt records this as
`accepted_not_applied`. Each steer is sent with `clientUserMessageId` of
`steer:<request_id>`, so after an uncertain response you replay the same request id
and read the turn's recorded items for that id instead of sending the instruction
again. Carry your own identity and the issue or revision in the message text; a
steer changes no acknowledgement, receipt or verification contract.

`pause_goal` sends only `status`, so it cannot rewrite an objective or restore a
stale one, and it compares the goal the host returns against the one it read a
moment earlier. It refuses a thread with no goal, returns `already_paused` without
calling the host, and refuses any other status naming what it saw. Two limits are
reported rather than worked around: the protocol offers no expected-status
precondition on `thread/goal/set`, so those refusals rest on the status read a
moment earlier and narrow rather than close the window in which a goal that ended
could be overwritten as paused, and the goal should be read again afterwards; and
pausing a goal does not stop a turn already in flight. To stop
work, pause the goal, re-read the active turn, and steer that turn to finish
safely. "Goal paused" and "turn stopped" stay separate claims.

`get_capabilities` answers two different questions separately. `exposure` is what
this bridge offers. `hostSupport` names the host version these paths were built
against and the server actually connected; when they disagree it reports
`unknown_host_version` rather than letting this tool list stand in for a statement
about that host. A `-32601` means the connected host lacks that method, not that
the capability is missing everywhere, and it never triggers a fallback.

## Desktop compatibility

The backend and Desktop project registries can differ. In the observed SSH setup,
the backend returned no projects and `projectId: null` for threads that Desktop
correctly placed in its saved project. Supply `app_server_project_id` only when
that ID actually exists in `project/read`; the bridge never imports projects or
guesses Desktop project identities.

Verify the actual Desktop listing and UI for each launch. An empty session may
not appear until it receives an initial prompt. Existing-checkout visibility has
been demonstrated. `create_worktree_thread` implements **bridge-managed Git
worktrees**, with explicit ownership and manual cleanup. Desktop-managed worktree
creation is not supported. Bridge-managed isolated creation, follow-up messaging,
Desktop project listing, manual Desktop continuation and preservation passed
live checks on 0.153.4. Retention evidence covers the observed run.
No archive/delete, general shell-execution or Goal-setting tool is exposed.

The bridge targets the public/experimental API shape observed in 0.153.4. It
uses experimental paginated reads and fails with the actual API error on
incompatible servers. Tool discovery and Desktop visibility require a separate
installed-MCP acceptance test. The inspected App Server protocol exposes creation
in an existing directory, but no worktree-creation method; isolated launches use
local Git preparation followed by `thread/start`.

## Isolated launches

Use `create_worktree_thread` only after approval of the bridge-managed retention
contract, repository, commit, placement, permissions and exact prompt. Git creates
a detached, locked worktree; the bridge never archives, prunes or removes it.
There is no implicit dirty-file carryover, setup script, fetch or Goal creation.
It creates no branch, runs no stash/reset, and does not initialize submodules.
Git hooks, filesystem-monitor programs and checkout filters are disabled during
preparation, so LFS files remain pointers. The worktree shares the repository's
Git object store; the task sandbox does not restrict the bridge's Git preparation.

```json
{
  "request_id": "isolated-readiness-001",
  "source_repository": "/absolute/path/to/project",
  "starting_revision": "FULL_COMMIT_OBJECT_ID",
  "destination": "/absolute/path/to/retained-checkout",
  "worktree_mode": "bridge-managed-retained",
  "sandbox": "read-only",
  "model": "anthropic/claude-opus-5",
  "reasoning_effort": "xhigh",
  "expected_sandbox_policy": {"type": "readOnly", "networkAccess": false}
}
```

Replace the revision placeholder with a full lowercase commit ID (for example,
from `git rev-parse HEAD`), not a branch or abbreviated SHA. The commit must already
be available locally. The source must be an existing non-bare checkout root. Both
paths must be canonical and absolute; the destination must be absent with an
existing parent, outside repositories and Git metadata. This example creates a
readiness-only task without a model turn.
`model` and `reasoning_effort` are required and are authorized before any Git work
happens, so a refused launch leaves no worktree behind; `prompt` and `title` stay
optional. For other sandboxes,
supply the complete expected policy, including all roots and network/temp flags.
The bridge verifies the response rather than silently accepting different settings.

Receipts contain `worktree`, `threadId`, `creation`, `permissionReceipt`,
`initialPrompt`, `phase`, and `recoveryRequired`. Partial receipts may lack task or
turn IDs. Reusing the same request ID replays its historical receipt without
recreating the worktree, starting a second task, or sending another message.
An environment or placement mismatch withholds the prompt and retains artifacts.
The caller owns further readiness checks and any later Goal handoff. Verify actual
Desktop project membership separately; backend project IDs do not establish it.

Failed or interrupted launches may leave a reservation directory, incomplete
checkout, Git metadata, task or dispatched turn. Inspect `get_operation`,
`git worktree list --porcelain` and the actual task listing before manual recovery;
never use a new request ID to blindly retry. The caller owns retention and cleanup.
A worktree lock protects against ordinary pruning, not external deletion, and a
replayed receipt is historical evidence rather than proof of current existence.

## Development

```sh
uv sync --frozen --group dev
uv run pytest
uv run ty check src
uv run ruff check .
uv run ruff format --check .
uv build
```

To check the real connection without creating a session:

```sh
uv run python scripts/check_connection.py
```

Tests use a fake App Server over a real Unix WebSocket and an MCP stdio client.
They do not start models or touch your Codex sessions. Implementation details and
contribution guidance are in [CONTRIBUTING.md](CONTRIBUTING.md).
If the system temporary directory is inside a Git checkout, pass pytest a
`--basetemp` outside that repository for isolated-worktree tests.

## References

- [Codex App Server protocol](https://learn.chatgpt.com/docs/app-server)
- [Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp)
- [Official Python MCP SDK](https://github.com/modelcontextprotocol/python-sdk)
- [Missing SSH-hosted Desktop tools report](https://github.com/openai/codex/issues/40865)

Independent project, not affiliated with or endorsed by OpenAI. MIT licensed.
