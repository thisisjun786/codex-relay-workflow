# The flow journal in the store, and the proofs that read it

A flow run writes its journal to the relay store's flow tables, and a resumed run, `crw flow recover` and the acceptance of an
agent node read it from there. This page says what is stored, when, and how an effect recorded in the journal is proven to
still be in the worktree. The events themselves (`prepared`, `thread_requested`, ... `superseded`) are described in
[script.md](script.md#journal).

## Persistence

`internal/flow/journal` is the `engine.Journal` over `flow_runs`, `flow_calls` and `flow_events`. It writes the events the
in-memory journal writes, in the same order, and keeps the engine's sequence numbers: `Seq` is run-wide, it is the `event_seq`
of a `flow_calls` row and the `seq` of a `flow_events` row, so reading a run back gives the events and the numbers the engine
saw. A row is never updated.

- A dispatch intent (`prepared`, `thread_requested`, `fork_requested`, `turn_requested`, `interrupt_requested`) is committed in a
  transaction of its own before `Append` returns, and so before the request it announces is sent. The rows that wait before it
  are committed first, in their own transaction, so the order on disk is the order of the run.
- Every other row is written by the one run process in a batch transaction. It waits in memory until the next intent, the end
  of a call (its terminal row), the run's `started` and `finished` rows, or `Flush`, and is committed then, in order. A crash
  loses at most the result rows after the last intent, and the reconcile pass finds those on the host from the intent that is
  stored.
- A journal that cannot store a row keeps the first error (`Err`) and keeps its rows in memory only. The engine sends nothing
  whose intent row could not be stored, and ends the run with that error.
- `flow_runs` holds the identity of the run: the workflow, the digests of the script, its helpers and its arguments, and what the
  caller gives the journal (`Identity`: the plan and its revision, the actor task, the epoch nonce, the run it resumes). It is
  written with the first `started` row; a resumed run reuses it.
- `flow_calls` holds every `KindCall` row. The columns the relay reads without opening `detail_json` are filled: `kind` (the
  call's op, `agent` for an agent call), `role`, `node_id`, `plan_id`, `execution_id` (when the row carries one), `commit_sha`
  (the commit of a `crw.commit` row, else the `head` a reviewer reads), `thread_id` and `turn_id` (a terminal row names the thread
  and the last turn the call ended on), `operation` (the turn operation), the allocation of a `prepared` row (`worktree_kind`,
  `worktree_path`, `start_commit`, `attempt`, `pending_tree`, `snapshot_digest`), `pending_tree` of a completed executor call,
  `held_tree` (`p_held`) of a held one, `input_state`, `tokens`, and `result_bytes` and `result_digest`. `detail_json` is the
  row's Data without the result; the label is its member `_label`. The result is stored once, as the canonical bytes the call
  returned.
- `flow_events` holds the other rows (`KindRun`, `KindFlow`: phases, logs, escalations, the creation sightings, the rows that
  rebind a resumed run); `payload_json` is the Data plus the reserved members `_kind`, `_path`, `_position`, `_op` and `_label`.
  A member of Data never starts with an underscore.

A row reads back as it was appended: the journal keeps its rows in the form the store returns (the JSON round trip of Data), so a
row reads the same live and after a restart.

## The replayed CallResult

`journal.ReadCallResult` (and `engine.RestoreCallResult`, which it calls) restores the CallResult of a call from its `flow_calls`
rows and from nothing else: `output` is the stored canonical result bytes, `status` and `reason` are those of the last terminal
event (a held call too, whose `output` is `null`), and `meta` is the references the rows carry (role, requested and configured
pair, thread, turns, repair, tokens, and the sequence numbers of the first and the last row). The host is not asked and no other
table is read; a resumed run replays the calls it replays through the same function. Which calls a resume replays and where it
starts the suffix of live calls is the resume rule (F07), not this page.

## The working snapshot

A worktree's working snapshot is the tree of the files it holds now. `ops.Snapshot` makes it in a private index file
(`GIT_INDEX_FILE`) that starts from `HEAD` (`read-tree HEAD`), takes in every change of the working files (`add -A --sparse -- .`)
and writes the tree (`write-tree`). It holds tracked changes, deletions, mode changes and the untracked files that no ignore
rule hides; a file that is ignored and not tracked is in neither the snapshot nor a commit of it. The worktree's real index is
neither read for content nor written: it holds only what was staged, and an executor leaves nothing staged. (The one thing read
from it is which entries are skip-worktree and absent from disk, so that a sparse checkout is not a deletion.)

A completed or held executor call records the snapshot of its worktree on its terminal row, as `pending_tree` (P) or `p_held`
(P_held). `crw.commit` commits that recorded tree as it is: `ops.Commit` makes the commit with `commit-tree` on the recorded tree
and the recorded parent, moves `HEAD` with a compare-and-swap `update-ref` (a `HEAD` that is no longer the parent refuses the
commit), and then sets the real index alone to the new `HEAD` (the working files are not touched, so whatever differs from the
snapshot stays visible as a change). The path that a snapshot changes against its base and that lies outside the declared edit
regions is recorded, not refused: `ops.Scope` lists them for the commit row and the reviewer packet.

## The proofs

A proof is a function of the journal rows and, for the effects in a worktree, of what git says about it now. This page defines the
functions; which positions a resumed run replays is F07's rule.

**Build and fix.** `ops.Prover.Node` proves the executor calls of one node against the worktree of its current attempt and
reads no other directory: the attempt is the node's last `prepared` row with a fresh or restore allocation, and the worktree of
an attempt that was retired (a `null_model` attempt leaves its uncommitted edits there) is never read. The recorded commits of
the node (`crw.commit` rows) after that row form a chain that starts at the attempt's start commit. A dirty worktree is not foreign
as such:

- The `HEAD` must be the last recorded commit of the chain. A `HEAD` that is not, because someone committed or reset, is a
  foreign commit: the calls a recorded commit on `HEAD`'s history covers stay proven, and the first call after the last recorded
  commit that is on the chain, and every later call, are not.
- The calls that no valid commit covers are judged by the snapshot of the worktree: it is valid when it is the tree of the last
  recorded commit, the pending tree P of a build or fix journaled after that commit, or the P_held of an executor call held after
  it. A call is proven when the snapshot is its own P (or an earlier call's, which leaves the later calls unproven). A snapshot
  that is none of them breaks the proof from the latest build or fix after the last valid commit.
- The proof is of the state of the files, not a flag: restoring the files restores the proof. Another actor moving the
  integration target does not touch a worktree and does not undo a call that ended.

**Other effects.** Verify: a passing record of the same head and command (`crw.verify`). Review: a record of the same head and
criteria digest (`crw.review`). Accept: the recorded acceptance is active, not stale, of the same head (the relay says which).
Integrate: the recorded batch commit is an ancestor of the integration ref; the target moving on keeps it an ancestor. Gate: a
passing record of the current integration head, the same command and the same range (`crw.gate`). Reviewer-final: a record of the
same integration range and criteria digest (`crw.reviewerFinal`).

**Recording calls.** `releaseChild`, `escalate`, `reportUp`, `hold` and `tierStep` only record a fact. A call with a completed row
is not sent again, whatever else is true; a call without one is executed live under the same call id and the same idempotent
request id (`ops.RequestID`).

## `crw flow recover`

The binary installs `engine.RecoverEnv`: it opens the relay store (`--state DIR`, else `CODEX_SESSION_RELAY_STATE`, else the
relay's discovery for the control socket) and connects to the App Server control socket (`--socket PATH`, else the default one).
It finds the run that holds the call (or takes `--run`), opens its journal, runs the recovery of [script.md](script.md#journal)
and commits the rows of the recovery together, in one transaction, when it ends. The command exits 0 when it recovered the call, 1 when the proof does not hold
(nothing is appended but the sighting of a closed connection), and 2 for a command line, a store or a call it cannot use.
