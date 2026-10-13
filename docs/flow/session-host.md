# The session host: reading role subagents

A session call (CRW-1316) runs a role call as a subagent the project orchestrator spawns with `spawn_agent` (multi-agent V1).
The run does not start that thread; it reads it on the host. `internal/flow/sessionhost` is that reader. It goes through the
App Server client (`internal/bridge/appserver`), keeps plain types, and imports neither the engine nor a backend: the engine
defines the interface it consumes.

## Reads

| Function | Request | What it gives |
| --- | --- | --- |
| `ReadAgent(ctx, thread)` | `thread/read`, `includeTurns: false` | `ThreadID`, `ParentThreadID`, `Model`, `Effort` (the configured pair; null reads `""`), `Cwd`, `AgentRole`, `Status` (`idle`, `active`, `notLoaded`, `systemError`), `RolloutPath` (`Thread.path`), and `ThreadSource`, `Nickname`, `CLIVersion` |
| `Turns(ctx, thread, limit, full)` | `thread/turns/list`, `sortDirection: desc`, `itemsView` `summary` or `full` | the turns newest first, at most `limit` (0: every turn, page by page); for each `ID`, `Status`, `ItemsView`, `FirstUserText`, `Marker`, `FinalText`, `FinalPhase`, `Answered`, `ErrorInfo` (message and `CodexErrorInfo` variant), `UpdatedAt` (completed, else started) |
| `Interrupt(ctx, thread, turn)` | `turn/interrupt` | the host's acknowledgement; the turn ends `interrupted` later |
| `FindByMarker(ctx, owner, op)` | `thread/turns/list` of the owner, `itemsView: full` | the subagent the owner spawned for the op |
| `Tokens(rolloutPath)` | none: the rollout file, read only | the `total_token_usage` of the last `token_count` event |

- A full read the host answers without the items (`itemsView` `summary` or `notLoaded`) is completed with `thread/items/list`. When
  that fails, the turn carries `ReadError` and keeps what the list gave: it is unreadable, not empty.
- An answer saying the thread is not there (`not found`, `no rollout`) wraps `ErrNotFound`. A transport failure never does.
- The summary view is enough to poll a subagent: it holds the turn's first `userMessage` and its last `agentMessage`, so the
  marker and the answer of a finished turn are read without the full items.

## The answer, the marker and the tokens

- `FinalAnswer(items)` is the last `agentMessage` of phase `final_answer`. From a provider that sets no phase (0.154
  `MessagePhase` is nullable) it is the last message whose phase is null. A `commentary` message is never the answer: a turn
  with only commentary has none. `PickMessage`, the App Server backend's choice of a turn's text (the last final answer, else the
  last message whatever its phase), lives beside it and the backend calls it, so both read the same messages
  (`ItemMessages`).
- `ParseMarker(text)` reads the first line `crw-flow-op: <call>/t<k>-a<n>` of a step's message, and the App Server backend's
  `crw-flow-op: <call>/t<k>` (attempt 0). The call is a flow-bound call id (`[A-Za-z0-9][A-Za-z0-9_-]{0,63}`); `k` and `n`
  are positive numbers without leading zeros. Trailing white space on the line is ignored; any other first line names no
  operation. `ParseOp` reads the operation alone and `Marker.Op` writes it back. A turn's `Marker` is its first user
  message's `clientId` when that is an operation, else its first line.
- `Tokens` reads the rollout line by line and keeps the last `event_msg` `token_count` whose `info` is set. A rollout that is
  absent, or has no such event, is unmeasured (no error); a line that does not decode is skipped; what follows the last line
  feed (a write the host had not finished) is left out. An error is a rollout that exists and cannot be read.

## Finding the subagent of a step

`FindByMarker` joins the flow-bound decision ledger of the owner (`<flow-bound>/decisions/<owner>.jsonl`, written by the spawn
hook: `tool_use_id`, the decision and, when the hook records it, the `op`) with the `collabAgentToolCall` items (tool
`spawnAgent`) of the owner's `OwnerTurns` most recent turns (20 by default). The item whose `id` is the tool use id names the
thread it created in `receiverThreadIds` and carries the spawn message in `prompt`. This is the join
`ops.ImportNativeSpawns` makes, and both decode the item the same way (`sessionhost.CollabCall`).

| Case | Result (`Spawn.Via`) | Warning |
| --- | --- | --- |
| The hook's allowed decision for the tool use names the op | `ledger` | `ledger_op_differs` when the prompt names another op |
| The allowed decision names no op; the prompt's first line does | `prompt` | none |
| No allowed decision covers the tool use; the prompt names the op | `unrecorded` | `no_ledger_line`, or `refused_decision` when the hook recorded a refusal |
| The ledger cannot be read | the prompts still decide (`unrecorded`) | `ledger_unreadable` |
| Several spawns name the op | the newest, ledger-tied spawns first; the rest in `Others` | `several_spawns` |

A warning is recorded, never a refusal: a binding that went missing does not hide a subagent that runs. A spawn that created no
thread is skipped with `no_receiver`. An op that is not an operation is an input error, and an owner the host does not have
wraps `ErrNotFound`.

## Host facts

Read-only probe of the live host on 2026-10-14 (CRW-1332): `thread/read` and `thread/turns/list` of one finished subagent thread,
and the tool declarations of a recent rollout. No thread was started, no turn sent, nothing resumed, archived or written. The
answers are fixtures in `internal/bridge/appserver/testdata/fixtures/subagent-*.json` (provenance beside them), and
`TestLiveSubagentAnswers` reads them through this package.

- Version: the App Server on the host and every `codex` binary there is codex-cli 0.154.0, the version of the schema copy
  (`testdata/schema-0.154`). The facts below are 0.154.0's.
- `thread/read` of a subagent: `parentThreadId`, `threadSource: subagent`, `source.subAgent.thread_spawn` (`parent_thread_id`,
  `depth`, `agent_path`, `agent_nickname`, `agent_role`), `agentRole`, `agentNickname`, `model`, `reasoningEffort`, `cwd` (the
  parent turn's), `path` (the rollout) and `historyMode: paginated`. A finished subagent reads `notLoaded`, or `idle` while
  its parent session keeps it loaded.
- `thread/turns/list`: the default view is `summary` and the default order is newest first. The summary of a finished turn
  holds its first `userMessage` and its last `agentMessage` (phase `final_answer`); the full view holds every item
  (`userMessage`, `reasoning`, `agentMessage` with phase `commentary` or `final_answer`, `commandExecution`).
- Differences from the 0.154 schema copy: the live `Thread` carries four members the copy does not list:
  `canAcceptDirectInput`, `daybreakEnabled`, `environments` and `extra`. The copy does not forbid extra members, so the live
  answers validate. The `Turn` and the item members match the copy.
- Tool declarations (`multi_agent_v1__*`, code mode):
  - `spawn_agent(agent_type?, fork_context?, items?, message?, model?, reasoning_effort?)` returns `agent_id` and `nickname`.
    There is no cwd, sandbox or name argument.
  - `send_input(target, message?, items?, interrupt?)` returns `submission_id`.
  - `close_agent(target)` returns `previous_status`.
  - `wait_agent(targets, timeout_ms?)` returns `status` (by agent id) and `timed_out`.
  - The agent argument of `send_input` and `close_agent` is `target`, and `wait_agent` takes `targets`.
- In code mode a spawn runs inside an `exec` cell. The `collabAgentToolCall` item's id is `exec-<uuid>`, and the hook's
  `tool_use_id` for the spawn is the same id (the 10-13 ledgers show it).
- A rollout records the same call as an `item_completed` event with a `CollabAgentToolCall` item in snake case (`spawn_agent`,
  `receiver_thread_ids`); the App Server's items are the schema's camel case.
- A running subagent does not survive an App Server restart: the 10-13 21:29 restart left two subagent rollouts ending in a
  `token_count` without `task_complete` or `turn_aborted`.
- Not probed: the status a 0.154 host reports for a turn an App Server restart cut, and the error text for a thread id it
  does not know.

## The fake host's subagents

`fakehost.Threads.Spawn(SubagentSpec)` makes a subagent of an owner thread the way the host shows one: the members above, the
parent's cwd and pair unless the spec names its own, a first turn run by a `TurnPlan` (no connection follows it), and a
`collabAgentToolCall` item (`id` the spec's tool use id, `receiverThreadIds`, `prompt` the spawn message) in the owner's turn
in progress, else its last turn. A subagent reads `active` while its turn runs and `notLoaded` after it, unless `StayLoaded`.
It is left out of a `thread/list` that names no source kind and listed under `subAgentThreadSpawn`. Its summary view is the
live host's; other threads keep the fake's earlier summary (no items).

`Threads.Restart()` is an App Server restart. Every turn in progress is lost: it never ends, and reads `interrupted` (or the
status `LostTurnStatus` sets, for example `inProgress`). Every thread reads `notLoaded` until a resume or a new turn loads it,
no connection follows a thread, and every connection is dropped (`Server.DropConnections`). Finished turns and their items
stay.
