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
| `send_message_to_thread` | Resume an explicitly selected idle thread, optionally under stated settings, then send one message |
| `list_threads` | Read a page of unarchived backend thread summaries |
| `read_thread` | Read metadata and a paginated history without resuming |
| `wait_thread` | Wait up to 50 seconds for the supplied recent turn ID |
| `get_goal` | Read persistent Goal state |
| `get_operation` | Recover a mutation receipt after a lost response or client restart |

Text limits apply to display content such as messages, previews, and summaries.
Pagination cursors, IDs, paths, and other protocol fields are returned unchanged.
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
  "message": "Do not use tools or edit files. Reply exactly: BRIDGE_FOLLOWUP_OK"
}
```

Creation defaults to `read-only` and approval policy `never`. `workspace-write`
and `danger-full-access` are explicit options; obtain authorization for the
chosen environment before calling. Omitted model/reasoning use the server's
configured defaults and are neither transmitted nor checked.

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
| approval policy | Always `never`; this bridge cannot service interactive approvals |

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
observation is never overwritten by a later one.

Four outcomes are kept apart, because each needs a different response:

- `setting_untransmittable` — this protocol cannot carry the request, so it is
  refused locally before any call is made. A read-only policy asking for network
  access is the current example; no config spelling applies it.
- `settings_not_preserved` — the host reported a value different from the request.
- `setting_unobservable` — the host reported no value, so nothing says whether the
  setting was applied. This withholds the prompt or message; it is never a warning
  attached to a success.
- A transport or RPC failure stays on the delivery path and lands as `failed` or
  `outcome_unknown`: the request may not have arrived, which is a different
  question from what the host did with one that did.

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
| `in_progress_or_unknown` | Operation is running, or the process stopped before recording its outcome |

`retrySafe: false` means **do not issue a new request ID to repeat the action**.
Reusing the same ID is safe while the ledger is retained. The bridge never retries
a sent mutation or automatically continues a partially completed create. If the
server created a thread but its response was lost, even its ID may be unknown.
This is conservative deduplication, not an exactly-once guarantee across the
server and the local ledger.

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
`turn/start`. It refuses a thread observed active or a resumed interactive approval
policy. Concurrent external clients can still change a thread between those
steps; the App Server remains authoritative. There is no automatic steering or
interruption. Unsupported client-side tool/approval requests receive an explicit
error; continue those tasks in Desktop.

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
  "expected_sandbox_policy": {"type": "readOnly", "networkAccess": false}
}
```

Replace the revision placeholder with a full lowercase commit ID (for example,
from `git rev-parse HEAD`), not a branch or abbreviated SHA. The commit must already
be available locally. The source must be an existing non-bare checkout root. Both
paths must be canonical and absolute; the destination must be absent with an
existing parent, outside repositories and Git metadata. This example creates a
readiness-only task without a model turn.
Optional `prompt`, `title`, `model` and `reasoning_effort` are exact overrides;
omitted model/reasoning preserve configuration defaults. For other sandboxes,
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
