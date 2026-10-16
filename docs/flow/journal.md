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

- A dispatch intent (`prepared`, `thread_requested`, `fork_requested`, `turn_requested`, `interrupt_requested`, and the
  `step_published` and `evidence_placed` rows of a session call) is committed in a transaction of its own before `Append` returns,
  and so before the request it announces is sent (for a session call, before its step file and packet are written, and before a
  flow evidence record is written under the directory an `evidence_placed` row names). The rows that wait before it
  are committed first, in their own transaction, so the order on disk is the order of the run.
- Every other row is written by the one run process in a batch transaction. It waits in memory until the next intent, the end
  of a call (its terminal row), the run's `started` and end rows (`finished`, or `run/paused`), or `Flush`, and is committed then, in order. A crash
  loses at most the result rows after the last intent, and the reconcile pass finds those on the host from the intent that is
  stored. A session call that stops a subagent's turn (a pause, a cancellation, a deadline) flushes before it sends the interrupt,
  so the `turn_started` it bound after its `interrupt_requested` row (the turn that row decides on a resume) is stored before the
  interrupt reaches the subagent.
- A journal that cannot store a row keeps the first error (`Err`) and keeps its rows in memory only. The engine sends nothing
  whose intent row could not be stored, and ends the run with that error. The run's own end row is written with the rows
  still buffered, so the error is read again after it: a run whose last write failed ends with the journal's error, not as completed
  or paused.
- A process ends the run with one KindRun end row. It is `finished` (`status`, `error_kind`, and `outcome` when a completed script
  returned a milestone state, [script.md](script.md)) unless the operator paused the run (`crw flow pause`, `by` `operator`) or
  an answer drained it (`crw flow answer` to a run a process holds, `by` `answer`) and it would have ended completed: then it is a
  `run/paused` row (`status` `paused`, and the `by` and `reason` of the last such ask) and no `finished` row, and no completion is
  told.
  A run no process holds whose last end row after its last `started` row is `run/paused` reads `paused`, not `dead`; its resume
  writes a new `started` row. A deadline, a token budget or a quota pause ends with `finished`.
- A process whose run ends cancelled (`error_kind` `cancelled`: a SIGTERM, `crw flow cancel` of a run it holds) while registered
  ops (`crw.<name>`) are still running writes one `cancel_waiting` flow row before it waits for them: `calls`, one
  `{call_id, op, node}` per op in flight (`node` is the op's first argument when that is a string, or its `node` member when it
  is an object, else `""`), sorted by call id. Each op it names writes its own terminal row after it, and the `finished` row
  comes last. No op in flight, or another end (a script error, a pause, a deadline or budget stop), writes no such row. A cancelled run does not resume,
  so no replay reads it.
- A run process whose run ends cancelled gives up the run's executions itself (CRW-1573): after its world is closed, still under
  its run lock, it settles the releases the run's rows show it did not end, as the cancel of an ended run does, and prints
  `N execution(s) of run R given up, M slot(s) returned`; a failed settle is told and changes no exit status. So one cancel is
  enough, whether the cancel's wait ran out, the cancel was killed, or a plain SIGTERM ended the run; the settle of the cancel that
  sees the lock go finds nothing more (it is replay-safe). A `crw flow cancel` whose `--wait` ends while the process still holds
  the run reads the run's rows again and names what the run is ending - the ops of its latest `cancel_waiting` row as
  `<op> (call <id>, node <n>)` and the agent calls whose last row is a start row as `<label> (call <id>)`, or "no call in flight
  is known" - says that the run gives up its executions itself when they end (no second cancel), and exits 1.
- A run no process holds is ended by `crw flow cancel` itself: under the run lock it appends one `finished` row (`status`
  `script_error`, `error_kind` `cancelled`, `by` `operator`, `executions_closed`, `slots_returned`), and `dead: true` only when
  the rows it reads under the lock show a `started` row with no end row after it (a run whose last end is `run/paused`, or a held
  run, had no process because it ended, not because its process died). The member records what the cancel saw; readers derive
  the state of a run from its rows and do not read the `dead` member. A cancel that finds the run over under the lock appends
  nothing, so two cancels of one run write one end row, and still gives up the executions the ended run released and did not end
  (their slots go back), as a cancel of a run it read over from the start does.
- A binary before CRW-1487 knows no `run/paused` end: it reads a run that ended paused as `dead` (it resumes nothing; its pump
  raises `run_dead` after `run_dead_seconds`, [reporting.md](reporting.md)). A rollback past CRW-1487 leaves such runs to be
  resumed (`crw flow resume`) or cancelled by hand; a binary cannot be fixed after the fact.
- `flow_runs` holds the identity of the run: the workflow, the digests of the script, its helpers and its arguments, and what the
  caller gives the journal (`Identity`: the plan and its revision, the actor task, the epoch nonce, the run it resumes). It is
  written with the first `started` row; a resumed run reuses it: a journal that is opened again (`journal.Open` with no `Identity`,
  as `crw flow resume` and `crw flow recover` do) reads the plan, its revision, the actor, the epoch and the resumed run back
  from the run record, and a call the resumed process reaches for the first time on a node whose earlier calls were associated
  with an execution is associated with the same execution, so the relay finds its rows by `plan_id`, `node_id` and `execution_id`
  like the rows of the calls before it.
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
  A member of Data never starts with an underscore. A session run adds two kinds there, with no call: `session_wake`, each wake of
  its owner thread it staged (the supervisor message itself is in `supervisor_messages`, obligation kind `session_step_waiting`), and
  the `escalation` of `reason` `session_unresponsive` ([script.md](script.md#the-owners-commands)); and one with its call:
  `step_unwritten`, a step whose packet or file could not be written, so nothing was sent for it (`input_state` `not_delivered`).
- A session run's exchange with its owner thread is not in the store: the steps, packets, dispatch reports and replies are files
  under `<state>/flow/<run>/` (`engine.FileStepBus`), and the owner's binding and the spawn hook's ledger (with each spawn's `role`
  and `op`) are the flow-bound files of `internal/flow/bound`. What the relay reads of a session call is its rows: the newest
  `thread_started` (the subagent, the host-read pair, `cwd` P) and the terminal row.
- A session call's `step_published` and `turn_started` rows record the turn's place in the call's conversation in `detail_json`
  (no column holds it): `conversation_turn`, the conversation turn the turn is (the call's task turn 1, its repair 2), and for a turn
  run again (the next attempt of a lost turn, the turn a held call goes on with in place) `replaces_turn`, the turn it replaces. The
  checkpoint packet of a continuation reads the call's conversation from them, each conversation turn once. A row of a journal written
  before the place was recorded carries neither: its turn takes the place its step recorded, the `repair` member every
  `step_published` row has (the task turn 1, the repair 2), a later turn at that place replacing the earlier one, so an older journal
  is read from recorded members too and never from its held rows or k ([script.md](script.md#attempts-restarts-and-continuations)).
- A session call's `evidence_placed` row (`detail_json`: `evidence_assignment`, `evidence_cwd`, `attempt`, `marker`) names a
  directory the engine is about to write the step's flow evidence record under when no other row of the call's newest generation
  names it (CRW-1496). It has no column of its own and no reader of a call's state counts it; the release and the restore read the
  directories from it.

- A command that writes into a run which may be alive (`report-up`, `report-ack`, `report-delivered`;
  [reporting.md](reporting.md)) is not the run's process and numbers its `flow_events` rows from `engine.ForeignSeqBase` (`1 << 40`).
  A run's own `Seq` stays below it: a journal that is opened again counts after the largest number below the base, so the two writers
  never meet a number, and the run reads the commands' rows back as flow rows of kinds it does not act on. The journal tells a commit
  notifier (`engine.CommitNotifier`) after each batch it committed, outside its lock, which is how the engine's upward hook
  (`engine.UpwardHook`) learns of a batch.

A row reads back as it was appended: the journal keeps its rows in the form the store returns (the JSON round trip of Data), so a
row reads the same live and after a restart.

## Reading a run

`journal.Load` reads a whole run. `journal.LoadLean` reads it without the results of its calls (the shape of the run is what `crw flow ps`
and `show` and the GUI need, and a result can be large), and `journal.LoadAfter(run, after, limit)` reads one page of rows after a
sequence number, lean, with `more` set when a row beyond the page exists. `crw flow watch` and the GUI's event stream keep the `seq` of
the last row they delivered and ask for the rows after it, as `crw relay journal-tail` does with the relay journal: a reader that was away
resumes exactly where it stopped, and no row is delivered twice. A page holds the run's own rows (below `engine.ForeignSeqBase`): a
cursor that took the number of a command's row would pass every row the live run writes after it, so the commands' rows are read with
`crw flow reports` (and are in `LoadLean`). Only the run process (and a command that holds the run lock) writes the run's own rows; a
reader opens the store read-only and creates nothing.

## The replayed CallResult

`journal.ReadCallResult` (and `engine.RestoreCallResult`, which it calls) restores the CallResult of a call from its `flow_calls`
rows and from nothing else: `output` is the stored canonical result bytes, `status` and `reason` are those of the last terminal
event of the call's newest attempt (a held call too, whose `output` is `null`), and `meta` is the references the rows carry (role, requested and configured
pair, thread, turns, repair, tokens, and the sequence numbers of the first and the last row). The host is not asked and no other
table is read. A call has a result only while it is ended: a `prepared` row starts an attempt, and a thread or turn intent, a
started thread or turn, or a `recovery_cleared` row after a terminal row makes the call open again, so the terminal row of an
earlier attempt is never returned with the thread and sequence numbers of a later one (`ok` is false until the new attempt ends).
A resumed run replays the calls it replays through the same function; [the resume walk](#the-resume-walk) says which calls those are.

## The working snapshot

A worktree's working snapshot is the tree of the files it holds now. `ops.Snapshot` makes it in a private index file
(`GIT_INDEX_FILE`) that starts from `HEAD` (`read-tree HEAD`), takes in every change of the working files (`add -A --sparse -- .`)
and writes the tree (`write-tree`). It holds tracked changes, deletions, mode changes and the untracked files that no ignore
rule hides; a file that is ignored and not tracked is in neither the snapshot nor a commit of it. The worktree's real index is
neither read for content nor written: it holds only what was staged, and an executor leaves nothing staged. (The one thing read
from it is which entries are skip-worktree and absent from disk, so that a sparse checkout is not a deletion.)

The snapshot reads the directory it is given, exactly as given: a worktree whose name ends in a space is a worktree, and no path
that git prints is used in its place. `ops.Scope` asks git for `--name-status -z` records, so a changed path that begins with a
space keeps it.

A completed or held executor call records the snapshot of its worktree on its terminal row, as `pending_tree` (P) or `p_held`
(P_held). An executor turn that ended but whose worktree cannot be snapshotted is not completed: the call is held for
`worktree_failed`, its row carries `pending_tree_error` (and `p_held` when a second attempt at the snapshot succeeds), and the
script sees a held call instead of a success no commit or proof can stand on. `crw.commit` commits that recorded tree as it is: `ops.Commit` makes the commit with `commit-tree` on the recorded tree
and the recorded parent, moves `HEAD` with a compare-and-swap `update-ref` (a `HEAD` that is no longer the parent refuses the
commit), and then sets the real index alone to the new `HEAD` (the working files are not touched, so whatever differs from the
snapshot stays visible as a change). The path that a snapshot changes against its base and that lies outside the declared edit
regions is recorded, not refused: `ops.Scope` lists them for the commit row and the reviewer packet.

## The proofs

A proof is a function of the journal rows and, for the effects in a worktree, of what git says about it now. This page defines the
functions; [the resume walk](#the-resume-walk) says which positions a resumed run replays on them.

**Build and fix.** `ops.Prover.Node` (`engine.Prover` is its interface) proves the executor calls of one node against the worktree of its current attempt and
reads no other directory: the attempt is the node's last `prepared` row with a fresh or restore allocation, and the worktree of
an attempt that was retired (a `null_model` attempt leaves its uncommitted edits there) is never read. The recorded commits of
the node (`crw.commit` rows) after that row form a chain that starts at the attempt's start commit. A dirty worktree is not foreign
as such:

- The `HEAD` must be the last recorded commit of the chain. A `HEAD` that is not, because someone committed or reset, is a
  foreign commit: the calls a recorded commit on `HEAD`'s history covers stay proven, and the first call after the last recorded
  commit that is on the chain, and every later call, are not.
- A recorded commit covers the calls whose state it carries: its row names the tree it committed, and that tree is the pending
  tree (or P_held) of the latest call that recorded it before the commit; the calls up to and including that call are covered.
  A call recorded after it, or any call when the committed tree is no call's pending tree, is not covered by that commit,
  whatever the sequence order says.
- The calls that no valid commit covers are judged by the snapshot of the worktree: it is valid when it is the tree of the last
  recorded commit, the pending tree P of a build or fix journaled after that commit, or the P_held of an executor call held after
  it. A call is proven when the snapshot is its own P (or an earlier call's, which leaves the later calls unproven). A snapshot
  that is none of them breaks the proof from the latest build or fix after the last valid commit.
- A call of a retired attempt (the node went on in a fresh worktree: a tier step, a rebuild) is judged without its worktree, which is
  never read. A call that a recorded commit of the node made in that attempt covers, by the rule above over that attempt's calls, is
  proven while the current `HEAD` descends from that commit (its work is in the history the node goes on from) and not otherwise. A
  call no recorded commit covers left only uncommitted work, which a fresh attempt of the node prepared after it abandoned (a fresh
  attempt starts at the node's last commit and takes none of an earlier attempt's working files: the node lane's tier step before
  any commit, a rebuild after a checked fix): nothing reads its effect again, and it is proven as the recorded fact it is. A call of
  an attempt no fresh one followed is the current attempt's.
- A split node (CRW-1459) runs its sub-tasks as turns of its one executor thread: each next one is a `subtask:<node>:<tier>:s<k>` call, an executor
  call of the node as a fix is, and the checkpoint commit of each sub-task before the last is one more `crw.commit` row of the chain, made
  against the sub-task's `edit_regions`. A split node makes one checkpoint `crw.commit` per sub-task before the last and is judged once:
  no `crw.verify` or `crw.land` row of the node precedes the last sub-task's commit, and verify and land read the node's last commit, as
  they read the last of a fix round's. No `crw.unitCheck` row of the node precedes the last sub-task's commit either: after the
  verification of a commit of the node a committed `crw.unitCheck` row judges the node's whole change, `base..commit` from the
  verification's base, and the node's `crw.accept` rests on it alone (its `reviewer_call`). A fresh attempt (a rebuild, a tier step) starts at that last commit, so the sub-tasks it holds stay
  proven while the current `HEAD` descends from it, by the rule above.
- A worktree a resume restored (below) holds the pending tree P of the last build or fix of the replayed prefix. It is the node's
  current worktree from the call that adopts it on, and it proves the calls of the prefix - the call P belongs to and the calls
  before it, which ran in the worktree of an earlier attempt - while its files are P.
- A session executor whose subagent committed left `HEAD` at a commit of its own. Its completed row records it
  (`executor_head_moved`: the `start` it was to be at, the `head` and the stray `ref` that keeps it): a `HEAD` that is that recorded
  head, grew from the last recorded commit and whose tree is the call's pending tree is on the chain, and the call is proven by
  its pending tree as above; `crw.commit` then commits the pending tree on `start` (its row names the `stray_head` it replaced).
  Any other `HEAD` off the chain is a foreign commit.
- The proof is of the state of the files, not a flag: restoring the files restores the proof. Another actor moving the
  integration target does not touch a worktree and does not undo a call that ended.

**Other effects.** Verify: a record of the same head and command that ran (`crw.verify`); a verification that could not run (the verifier is missing, it left no
record) is asked again on a resume. The base it names is the merge-base of the head and the integration tip, the commit the node's
work grew from, which the acceptance of the head uses too. A failed verification's row and result carry `failed_tests`, one entry
for each step of its record that failed and gives a reason (not a step skipped after an earlier failure): `job`, `step`, the `tests` (`--- FAIL: <Test>`) and `packages` (`FAIL\t<pkg>`)
its reason names and `tail`, at most 20 lines and 2048 bytes of the reason: a reason that fits is its own tail; one that leads with
crw-dev ci local's marked block (the failing lines of the output before its last lines, each `--- FAIL` line with its indented
message lines, then a `...` line) keeps the block's first 10 lines (at most 1024 bytes), then `...`, then the reason's last lines
(CRW-1569); any other keeps its last lines. A reason with no test or package line has no test and no package, only the tail. The member is additive (a row without it names no failed test), and the executor's fix packet and a fresh
thread's packet render it (CRW-1469). A node sized for a child, a bundle split or a split with no sub-tasks keeps its unreleased release row (`crw.release`); a split with sub-tasks is released as an executor node is (CRW-1459). Review: a record of the same head and
criteria digest (`crw.review`). Accept: the recorded acceptance is active, not stale, of the same head (the relay says which).
Integrate: the recorded batch commit is an ancestor of the integration ref; the target moving on keeps it an ancestor. Gate: a
passing record of the current integration head, the same command set (the plan's required commands joined with `;`) and the same
range (`crw.finalGate`; its row holds the judgement of each piece of evidence, fresh, missing or stale, and the gate row of a head the
target has left is not replayed: the gate judges the new head). Reviewer-final: a record of the same integration range and criteria
digest (`crw.reviewerFinal`; the shipped close makes none, its review is the crosscheck below). The test evidence of the gate is a `crw.verify` row with no node (the verification of the integrated head,
which names the milestone), and the QA evidence a `crw.qaReceipt` row (it holds the identity of the tree the capture call ended in, which the completed row of a QA call records as `qa_source`); each is judged fresh by the identity of the head it stands on
(`source.Compare`).

**Unit check.** A row `crw.unitCheck` is one unit check of a node's work, run by the run itself (the op runs the review tool through
the review adapter; no agent call, no reviewer thread). Its data: `node`; `mode` (`uncommitted`: the changes the judged executor call left in
the node's worktree, or `committed`: the range `base..commit` checked out in it); `call` (the executor call it judged, uncommitted mode) or
`commit` and `base` (committed mode; a committed check of a run's node, the check of a split node's whole change, also names the `call`
whose receipt the range ends on, and `range_receipts`, oldest first, the `call` and `receipt` of every other executor call a `crw.commit`
row of the range was made from; a receipt of such a call that is not taken holds the check as the judged call's would, the row naming it
`hold_call`); `round` and `final` as the lane passed them; `execution_id` and `criteria_digest` of the node's release
row; `tree` (the fingerprint the check saw, the snapshot `crw.commit` commits as its `tree`); `role` `unit-check`; `state`, `verdict` and
`findings` (their count); `retried` and `first_state` `invalid` when an invalid answer was asked once more and the second record kept;
`fallback_from` (the name of the first profile, such as `codex-review`), `fallback_state` (`quota`), `fallback_model` and, when that
quota came on the retry of an invalid answer, `fallback_retried`, when the first tool's quota was spent and the check was asked once more
of the fallback profile (Bugbot; `retried` and `first_state` are then about the fallback's calls, and the first profile's record is the
result's `fallback`); `hold` (`unit_check_quota`, `unit_check_no_answer`, or `receipt_missing`, `receipt_unverified`, `receipt_not_copied` when the executor's
evidence receipt could not be read) when the lane is to hold the node; `receipt` (the executor's evidence receipt as the check read it:
`state` `accepted` with its `sha256`, `copy` and `ignored`, or, when the backend could not copy it to the step bus or the copy cannot be
read, with the `path` it was read from in place of the copy (where the hook accepted it, the bytes still hashing to the `sha256`) and the
`copy_error`; an accepted receipt also names `handed`, true when the tool that judged (the row's `result`) was given the prompt that
holds the receipt and false for the Codex review profiles, which take no prompt (`crw.accept` then names the receipt as read and kept,
not handed to the tool; a row with no `handed` was written when every unit check took the prompt and reads as handed); `not_assigned` when the call was given none, or `no_executor` for a committed check that names no call); and `result`, the `reviewadapter.Result` of the call that answered last (state, verdict, findings with their line-free ids, engine family, model, effort, guide
revision, prompt digest, both fingerprints), with `fallback`, the first profile's quota record, when the check fell back. `crw.accept` and the relay take a row of role `unit-check` whose result state is `ok` and that is
on the head (its `commit`, or its `tree` equal to the head's tree) as the judgment of the head. A check that was not run names no role: when
the worktree is no longer the judged call's pending tree the row has `state` `tree_moved`, `tree_moved` true, `pending_tree` and `tree`
(the tool is not run, since `crw.commit` commits the pending tree); when the worktree's `HEAD` is not the commit the node's attempt is
at (the `start` the judged call's row records under `executor_head_moved`, else the start of the attempt's worktree or the node's last
`crw.commit` since), as when the executor committed its work itself on any backend, the row has `state` `head_moved` and `head_moved`
(`start`, `head`), since the uncommitted changes are not the change `crw.commit` commits on that start; a committed check whose checkout is not the commit's tree has
`state` `tree_moved`, `commit_tree` and `tree`; a check whose tool judged another tree than the one the op snapped for it (the worktree
changed after the snap, before the call or before the retry of an invalid answer: the tool's fingerprint, or the worktree after the call,
is not the snapped tree) is no judgment either and has `state` `tree_moved`, `tree_moved` true, the snapped tree (`commit_tree`, or
`pending_tree`) and the `tree` it saw; a check during which the worktree's `HEAD` moved (it is read before and after each call, also one
that failed, the first and the retry of an invalid answer; a commit of the same tree moves it too, and empties the uncommitted diff the tool reads) is
no judgment either and has `state` `head_moved`, `head_moved` (`start`, the `HEAD` the check is of, and `head`) and the snapped tree
(`commit_tree`, or `pending_tree`); and a receipt that could not be read leaves `receipt` and `hold` with no
`tree` and no `commit`; neither reader takes such a row. A check is proven unless its row names a `hold`: a resume runs the check that held the
node again (it asks the tool, or reads the receipt, again), and the hold itself is the `crw.hold` row the lane writes after it (`source` `unit_check`), as for a
verification that could not run. An error of the tool's runner (a bad request, a fingerprint that could not be taken) writes no row.

**Crosscheck.** A row `crw.crosscheck` is one round of the crosscheck of a milestone at its close (CRW-1473), run by the run itself: the
op runs the review engine through the review adapter, as the unit check does, with no agent call and no reviewer thread. Its data:
`milestone`, `plan`, `head` (the head of the gate the close passed), `round` (1: the op serves round 1 only, and a prior verdict or another
round is refused `crosscheck_prior_invalid`), `evidence_round` (0), `state` (`verdict`, `held`, `refused`, `failed`, `no_verdict`),
`reason` (of a refused, failed or no_verdict round), `detail`, `hold` (of a held round: `crosscheck_quota` and `crosscheck_no_answer` when a
pass gave no judgment, `crosscheck_receipt_missing`, `crosscheck_receipt_unverified`, `crosscheck_receipt_not_copied` and
`crosscheck_receipt_mismatch` with the `node` when an executor receipt could not be handed over), `engine_family` and `model` once an engine
was chosen, `passes` (each attempt of each pass: `pass` `A` or `B`, `attempt`, `state`, `verdict` (the pass's own word, `verified` or
`needs_changes`, of an `ok` answer; absent for an attempt that gave none), `elapsed_ms`, `model`, `prompt_digest`, `voided`
for an attempt that read no repository, `retried` for the one retry of an invalid answer, `detail`), `cleanup` when a checkout could not be
removed, and `verdict`, the verdict V of a round that judged (null otherwise): the word (`verified` or `needs_changes`; `verified` only
when no merged finding blocks and every pass's own `verdict` is verified), the merged
`findings`, `carried` and `follow_ups` (the findings that do not block, by id and as one line each), `guide_rev`, `engine_family`,
`engine_families`, `model`, `round`, `evidence_round`, `milestone`, `plan`, `head`, `base`, `run`, `author_families` and `authors` (each
author with its `source` `role` or `call`, its `name`, `model` and `family`), `receipts` (each node's receipt `state` and `sha256`),
`receipts_file` and `receipts_sha256`, and `passes`. A crosscheck is proven (`ops.CrosscheckProof`) by a completed row of the same
milestone, head, round and evidence round with a state other than held; a row of another head is no proof, and a held row proves
nothing, so a resume runs the round again.

**Close.** A row `crw.milestoneClose` is the close of a milestone over its evidence `{gate, crosscheck}`: the gate that passed and the
crosscheck's V. The op refuses, writing no completed row (reason `close_unverified`), evidence whose `crosscheck` is not a verified V of
the gate's milestone and head, evidence with no `crosscheck` whose `final` (the reviewer-final review of a journal of a close made before
the crosscheck) is not a verified verdict, and evidence with neither. The row names the range, the tip the close judged and whether the
target moved on after the gate (`moved`); the answer carries the gate, the crosscheck V (`crosscheck`, null when absent) and `final` as
they were given. The report of the close (`crw.reportUp`) stores a body that names the integrated range and, one line each, `Final gate:`,
`Crosscheck:` (V on one line), `Reflection:` (the reflection's alignment: `ALIGNED`, `MISALIGNED` or `NOT_ASSESSED`), `Final review:` and
`Audit:`, with `none` for a piece the close did not make ([reporting.md](reporting.md)).

**The design ledger and the signals.** A row `crw.designLedger` is a revision of the milestone's ledger (`revision`, `digest`, `design`, `answers`,
`architect_call` and `architect_handle`, the `signal` that made it, `base_tip` - the head the DESIGN ran at: for `r0` the start commit of the architect's call-private checkout as the architect's `prepared` row records it (reached through `resume_of` from a design fix that went on in that directory), never the integration tip read after the audit, and `""` when no placement recorded one; for a later revision the head its re-plan pinned - and `slices`, the slice digest of every node of the plan it was made
against: for a revision after the first the plan the architect and the auditor evaluated, as the decision read it). Revision ids are used once: the next is named after every ledger row of the milestone, the ones a resume superseded included. The ledger that stands is the newest
row no `superseded` row came after; the handle of the architect thread is that of the newest row that stands among the revisions and the `crw.reflection` rows. A release is proven
while its revision stands; a release under a revision a resume superseded is proven when the node's slice (`slice_digest`, the slice of the plan when the node was released) is the
one the revision that stands was made against, and a flow row `release_rebind` (node, `from_revision`, `to_revision`) says the replay was kept. The flow row `revision_superseded`
(milestone, revision, signal) names a revision whose signal a resume did not replay. A signal is the rows `crw.signalBind` (milestone, `k`, `prev`, `members` as (kind, call, node,
digest), `attempt` (`live:<n>` for a recorded signal that could not replay, `n` one more than the attempt of the row it did not replay), `started`, `unstarted` and `blocked` nodes, `tip`, `plan_digest` and `revision_read`), `crw.signalDone` (`outcome`, `revision`, `reasons`, `digest`) and, at the end of the path,
`crw.signalClose`. The nodes are the milestone's, in the scheduler's order, from the run's `crw.milestone` row (CRW-1439): a node is `started` from its release on, from the design row that sized it for a split or a child, or when its newest `crw.release` row that stands released nothing and either sized it (`kind` `child`, `split`, `bundle_split`) or found the plan had dropped it (`node_not_in_plan`) or the epoch lost (`epoch_lost`); a newest `plan_changed`, `relay_plan_changed`, `node_not_designed`, `design_*` or hold row leaves it unstarted. A node that has not started is `blocked` when a node of the milestone it waits for (`after`) ended so, was given up (released, then a `crw.escalate` of it with `closed` true and no later `crw.integrate`), or is blocked itself; a held predecessor or one waiting on an open escalation blocks nothing. The rest are `unstarted`. A row recorded before CRW-1439 has no `blocked` and replays as recorded. A member is a RECEIPT (kind `receipt`: its call and the digest of its result) or a plan revision a lane read before its release (kind `revision`: call `plan:<occurrence>:<digest>`, the digest; the row of `crw.planRead` with `changed`, `plan_digest`, `occurrence`, `base`, `judging`). `base` is the plan the audited DESIGN stands on: the plan the current revision was made for (its `milestone_digest`), or the plan a later `crw.replanSignals` row of a signal judged when that signal's `crw.signalDone` is `continue` (the signal kept the revision for it). A read whose plan is the one a bound and not yet decided signal read is no trigger: `judging` names that signal and the lane's release waits for it. An occurrence is `after:<seq>`: the journal sequence of the newest standing observation of another digest (`crw.planRead`, `crw.signalBind`, or the `milestone_digest` a `crw.replanSignals` row of a signal judged), or the first DESIGN ledger row when no such observation exists (`initial` without a ledger). Reads of the same uninterrupted occurrence coalesce even across a signal bind of the same digest; a return to an earlier digest after another observed digest has its own trigger and release wait. The recorded read preserves its occurrence on replay, and only a standing signal that took that occurrence proves a changed read. Older reads without an occurrence keep their recorded `plan:<digest>` key. The engine reads three things for the signal path and writes none of them: `crw.callFacts`, `crw.recordedSignal` and `crw.priorRelease` (script.md). Every release of a lane compares the plan with that plan in the flow, before `Relay.Release`, and the relay's release commit compares the node's slice of that plan again (CRW-1288): the `crw.release` row of a pinned release records `plan_digest` (the plan the relay had when the flow read it for the comparison) and `pinned_plan`, and when they differ, or when the relay finds another slice of the node at its commit, the node is not released (`released` false, `reason` `plan_changed`); that row is proven as the decision of its moment, and the lane's next positions are a new `crw.planRead` and a new release. A relay refusal while the plan the flow reads again is still the pinned one is `reason` `relay_plan_changed` and raises the run-level hold `plan_superseded` (CRW-1291): the release writes a flow row `escalation` (`reason` and `ask` `plan_superseded`, the node, `release_call`, and `evidence`: `plan`, `node`, `pinned_revision`, `head_revision`, `held_execution`, `held_run`, `slot` - `returned` for an execution of the run, which the release closes with the reason `plan_superseded`, `owned_by_run` for another run's, `none`), and the row names it (`escalation`, `hold`, and `slot_released` when it closed the execution). While a hold no `decision_applied` row answers stands, every `crw.release` of the run answers `plan_superseded` (`hold`) and asks the relay nothing; the call that raised a hold, run again, answers that hold as its own and closes its execution again (two releases refused at once raise one hold each). The release that raised a hold is proven for good; a release the hold answered is proven while it stands. The `slice_digest` of a released row is the slice of the manifest the relay committed; the relay answers the replay of a release only to a request that expects the slice it was committed under, so a release committed before a plan change and not recorded in the journal is `plan_changed` for a request under the new slice, not a release; an unreleased row's is the slice the flow asked for.

**Audit.** A row `crw.audit` (the call row of the audit, written by `crw.audit` in a run and by `crw flow audit` beside it) holds the
audit results of acceptances: `results`, each with the acceptance id, the node, the head, the reviewer call, the thread and turn it read,
the checks and the state (`ok`, `mismatch`, `unobserved`). The newest row that audits an acceptance decides whether it passed
(`audit.Passed`); a reviewer thread is archived only for an acceptance that passed (`crw.archive`). The completed `crw.accept` row names what the acceptance rests
on: `basis` (`review`, or `unit_check`) and `reviewer_call` (the reviewer's call, or the call of the `crw.unitCheck` row), besides
`node`, `head`, `acceptance_id`, `premerge_digest` and `plan`; an older row names neither and is audited on the newest verified
`crw.review` of its head. An acceptance on a unit check rests on no reviewer thread: its audit checks that row (`unit_check_row`: a
completed check of the node with role `unit-check`, state `ok`, verdict `verified` over no blocking finding, on the head by its `commit` or by the head's recorded tree, under the
criteria digest of the node's release) and the executor pair, reads nothing from the host and names no thread, and `crw.archive`
neither archives nor keeps anything for it. A land-head acceptance has no executor call, so its executor pair and its result are
`unobserved`.

**Recording calls.** `escalate`, `reportUp`, `hold`, `tierStep`, `spawnDecision`, `stepRecord`, `exploreRoute`, `exploreRecord`, `designAudit`, `designLedger`, `qaReceipt`, `qaCapture`, `reviewerFinal`, `audit`, `archive`, `replanSignals`, `currentDesign`, `reflection`, `signalBind`, `signalDone`, `signalClose`, `designHead` and `releasedPlans` only record a fact. `releaseChild` was the old project lane's start of a node's `child` session (a code id, not a layer); no
script calls it and no lane installs it now (CRW-1415). A run whose journal holds a `crw.releaseChild` row is a legacy `child`-session run, and
it is not resumed: the current lane sends a node sized for a child up as a `size_split` escalation at the position where the old lane
decided and started the child, so a resume would not replay that start - it would escalate the node live, leave the child running and run
every later position of the parent's path live. `engine.Run` refuses the resume of such a run before it writes a row (and the resume of a
run whose spec or script names the retired lane's workflow, even before its first row), `engine.Recover` appends no recovery row to it, and
`crw flow resume RUN --detach`, `crw flow answer` and `crw flow next` ask the same before they start a run process. Every command that would
write to the run's journal, or make its process write, refuses it with `legacy_issue_child_run` before it writes or signals, whether a
process holds the run or not: `crw flow cancel` (no finished row, no execution given up), `crw flow pause`, `crw flow recover`, `crw flow
dispatched` (no report in the run's inbox), `crw flow answer` and `crw flow report-up`; `crw flow audit` prints its results and records no
row. The delivery bookkeeping of a report already on record (`crw flow report-ack`, `crw flow report-delivered`) records the delivery and
not the run's progress, and is kept. The refusal names the legacy `child`-session path and the `crw.releaseChild` row. The run's records stay
read only: `crw flow show`, the summary and the run readers read them. A call with a completed row
of its own operation at its call id is not sent again (a completed call of another operation at the same position, such as an
agent call of the earlier script, is not its record), whatever else is true; a call without one is executed live under the same call id and the same idempotent
request id (`ops.RequestID`).

## `crw flow recover`

The binary installs `engine.RecoverEnv`: it opens the relay store (`--state DIR`, else `CODEX_SESSION_RELAY_STATE`, else the
relay's discovery for the control socket) and connects to the App Server control socket (`--socket PATH`, else the default one).
It finds the run that holds the call (or takes `--run`), opens its journal, runs the recovery of [script.md](script.md#journal)
and commits the rows of the recovery together, in one transaction, when it ends. The command exits 0 when it recovered the call, 1 when the proof does not hold
(nothing is appended but the sighting of a closed connection), and 2 for a command line, a store or a call it cannot use.

## The resume walk

A run resumes with `Config.Resume`; `crw flow resume RUN SCRIPT` is the command. The journal is read in this order:

1. The pass of [reconcile](script.md#reconcile-and-recovery) looks up on the host every call whose last row is a dispatch intent,
   and every call whose outcome was left unknown and not cleared. What it cannot prove holds the node.
2. The script runs again from its top. Every position it opens - an `agent()` or `agent.call`, a `crw.*` call, a join, a
   `workflow()` - is looked up at its own `(path, position)` in the rows the earlier processes wrote, and replays or runs live.

There is no reuse across positions. A result belongs to the `(path, position)` it was recorded at, within the journal of one run;
no call is found by its prompt, and no earlier lineage's result is a cache (this is the deliberate departure from Claude's
`resumeFromRunId`, which matches calls by key and occurrence across the whole run and asks no host: here the host is reconciled
first, the walk is per path, the conversation a call starts from is part of what must match, and nothing is global).

A position replays only when all of these hold:

- (a) every earlier position of its path replayed;
- (b) every position of the parent path before the call that created this path replayed. Whether that call - a join - replays
  is not asked: a join replays only when all its item paths do, so asking would make one lane's suffix run every sibling lane;
- (c) its input digest is the recorded one. The digest covers the canonical prompt, the schema, the role and its pair, the node,
  step decision, head and directory options, the digests of the joins settled before it on its path, and the context boundary:
  `{fresh, packet digest}` for a call that starts a thread; for a continuation the handle's thread, its `through_turn_id` and the
  digest chain of the turns up to it with the result digest of the call each belongs to, so a continuation whose conversation
  would hold turns outside the replayed prefix never matches. The prepared row has it as `input_digest`, with `prompt_digest` and
  the `context`;
- (d) its input snapshot is the recorded one. `Config.InputSnapshot` reads the facts only an external act changes (the node's
  slice of the plan revision, the criteria digest, the acceptance and integration ids of its dependencies); the prepared row
  keeps them as `input_snapshot`. Git facts are never compared with live HEAD: a start state is rebuilt from the journal, so a commit the run made later
  never invalidates an earlier call;
- (e) the effect it produced is still there: for an executor, the pending tree P of its completed row is carried by a recorded
  commit or is the state of the node's current worktree (`engine.Prover`, which `ops.Prover` and the App Server backend
  implement; a run with no prover has nothing to prove against); for an op, `Op.Proven`. A call that is not an executor's has no
  effect to prove (a reviewer's, an explorer's);
- (f) when it comes after a join on its path, the join replayed;
- (g) its last event is not `held` (below).

A completed, `null_model`, `refused` or `outcome_unknown` call replays through `RestoreCallResult`; a call refused before it
opened (policy, pair, schema) replays when it was refused for the same reason. A call whose outcome is unknown and not cleared is
never run again, whatever the walk says: its result stays unknown and its node stays in `recovery_hold`.

A join - `parallel`, `pipeline` - settles with a digest: the canonical digest of its item paths' final results ordered by path
key, written on the `join_settled` row. It replays when no position under it ran live and the digest is the recorded one; the
digest joins the chain every later input digest of the path carries, so the positions after a join whose items changed run live
though their own arguments did not. A `workflow()` position is a join of its one child path. The order the items finish in, and
the order the array lists them in, are not identity: items are keyed by their explicit key or their index.

### Ops

`crw.<name>` calls with an effect outside the engine - `commit`, `verify`, `review`, `accept`, `integrate`, `hold`, a report to the
relay - are `engine.Op`s registered in `Config.Ops` (`ops.NewLane(...).Ops()` registers the operations of the project lane, listed
with what each replays on in [script.md](script.md#the-project-lane)). An op takes a position like a call. Its completed row (`crw.<name>`, with the
`input_digest` of its arguments, its `request_id`, its `result` and what its proofs read: the `node`, `commit` and `tree` of a
commit, the `acceptance_id` of an acceptance) replays when the position's earlier positions replayed, the arguments are the same
and `Op.Proven` says the effect is there. An op that ran and did not write its row runs again under the same call id and the
same `RequestID`, so whoever it asked sees one request. An op that runs live at a position whose earlier attempt the journal
ended (a completed row, or a refusal of other arguments) because its arguments changed or a position before it did not replay
is a request of its own: its `RequestID` is `AttemptRequestID(run, call, n)` for the n-th ended attempt, so a receiver that
deduplicates by request id does not answer it with the earlier attempt's result. `crw.tierStep`, `crw.clock` and `crw.classify` are the engine's own: a
resumed script gets the step decision, the time and the state it got before.

### The suffix

The first position that does not replay, every later position of its path and every path a later position of it opened are the
suffix. The calls the earlier processes ended there are ended `superseded` (reason `superseded_by_resume`, with the lineage of
the resume: 0 for the first process of the run, one more for each resume). A turn an earlier process left running there is first
interrupted and the interruption confirmed with `thread/read`; the call it belongs to is ended after that. Every other path -
the sibling items of a join included - is untouched and replays. A node whose accepted result has another spec or packet is not
rebuilt: the call is refused (`accepted_node_changed`) and the refusal names the route, a plan revision with `update_node` (which
makes the acceptance stale) or `dag-correct` for an oversized child.

The first executor call of the suffix on a node that had been worked on rebuilds the boundary before its `prepared` row:

1. The old worktree's HEAD and working snapshot are kept as one commit on `refs/crw/flow/<run>/superseded/<lineage>/<path>`; the
   path is one percent-encoded component followed by `@<node>`, and a flow row `worktree_preserved` names the ref. The state a
   held call recorded (`p_held`) is kept too, on `...-held-<call>`, when it is not the snapshot. The old worktree is never written
   to or proven again.
2. A new worktree `<state>/flow/<run>/wt/<node>/a<attempt>-l<lineage>` is made with `git worktree add --detach` at C, the last
   commit of the positions that held (the node's creation base when none). When they end with a build or fix whose pending tree P
   was proven, P is put on top with `read-tree -m -u C P` in a temporary index, so the files are P and the worktree's own index
   stays at C. The snapshot is checked against the boundary (the tree of C, or P) before any call runs.
3. A flow row `worktree_restored` records the node, lineage, attempt, path, `C`, `P` (or null) and the verified `snapshot_digest`.

This worktree is the node's restore allocation: the suffix's first executor call on the node - a new thread, a fork or a
checkpoint thread - adopts it as its cwd, and its `prepared` row carries the same C, P and digest; the allocator never makes
another or resets a file. A process that dies after the restore leaves a call with no result, which the next resume meets as its
first failing position: it keeps the old restored worktree on a ref of its own lineage and rebuilds the same C and P in a
worktree of the new lineage, verified against the same digest.

A continuation of a thread the earlier process started never sends a turn to that thread inside a suffix, even when the thread's
last turn already is its `through_turn_id`: its cwd is the retired worktree, and moving it with `TurnStartParams.cwd` would
leave the thread's recorded cwd, its `thread/list` identity and the effect proofs on the old allocation. It runs on
`thread/fork {threadId, lastTurnId: through_turn_id, cwd, model and config of the role, ephemeral false, excludeTurns true}`, with
`cwd` the restored worktree for an executor and the call's own directory
(`<state>/flow/<run>/calls/<call_id>/wt`) for any other role. `thread/rollback` is not used: it changes the history only, leaves
the files and edits the original thread. The answer carries no history; the fork's turns are read with `thread/turns/list`
(`itemsView` full) to the end of every page. A fork whose `Thread.cwd` is not the cwd it was forked into, or whose last turn is
not the through turn (found by id or by the turn's `clientId` marker) and has ended, is archived and the call refused; a fork
whose configured pair is not the role's is refused for its pair (`pair_refused`). A fork whose answer was lost is found by reconcile through its cwd,
`Thread.forkedFromId` and a history that is exactly the recorded prefix. When the source thread or the turn cannot be read, the
continuation starts a new thread of the same role in the same directory, with a checkpoint packet built from the journal's
record of the turns up to the through turn (a flow row `checkpoint_started` has its digest and the packet itself). The first turn
of such a thread carries the packet before the call's input, also when the creation was recovered: the thread whose answer was
lost and that the next process adopts, and the creation the next process proves absent and sends again, get the packet from the
`checkpoint_started` row of the call's newest attempt.

Every continuation of the suffix on a thread of the earlier process forks, not only the first: the node's worktree is then the
restored one the first executor call adopted, so a later continuation's `prepared` row records the same path (`continued`, `fork`)
and its thread is forked into it. A call that is not an executor's and runs again takes the next private directory of its own
(`calls/<call_id>/wt`, then `wt-1`, `wt-2`): a path belongs to one allocation, and the directory of its earlier attempt is not
made over. The directory of an architect turn that names its `head` is the call's own and goes with the call's end: the terminal
row of the call (any but `outcome_unknown`) names it (`own_directory`) - the rows of the call's own run and the `superseded` rows a
resume writes for an attempt an earlier process left, decided by the same rule from the attempt's `prepared` row; reconcile writes
only `outcome_unknown`, which names none - the engine removes it after that row with a context the
run's cancellation does not end, and a removal that fails is a flow row `own_directory_kept` (call, path, error). A resume removes,
before the script runs, every directory a terminal row of an earlier process names that is still there, except the one a call is
still at work in (a call whose newest state is no terminal row or an unknown outcome).

### The signals

A signal replays under the rule of the walk above: its `crw.signalBind` position replays when its arguments are the recorded ones. The arguments are the signal's input - its triggers
as (call, result digest) and the result digest of the signal before it - so a signal replays only when every trigger position replayed and answered the same result digest (the
script hands the bind the recorded triggers only then, and marks any other bind of a recorded signal `live` so that its arguments are not the recorded ones even when every trigger answers the digest it answered; `crw.callFacts` says whether a call was replayed) and the signal before it replayed. The first signal that does not replay
is the first position of a suffix of the path `signals:<milestone>`: the rows of that signal and of every later one - the calls they made and the revisions they recorded - are
ended superseded, the revisions get a `revision_superseded` row, and the triggers that replayed re-enter the first signal that runs, in the order of the journal, with the number after
the last signal that replayed. A lane released under such a revision waits for that signal to be decided and then keeps its replay (`release_rebind`), is released again, or is refused,
by the slice of its node ([script.md](script.md#the-architects-continuity-and-the-re-plan)).

### Held calls

A position whose last event is `held` never replays: neither its held `CallResult` nor the `crw.hold` record after it is reused.
When (a) to (d) and (f) hold for it, and its effect proof does (for an executor: the live snapshot of its worktree is its
`p_held`, compared with the live snapshot of the call's own worktree, not with the cumulative proof of the node's calls; a hold
whose snapshot could not be made, `p_held_error`, has no state to compare, so its files are not known to be the call's; for
`host_infra_activeTurnNotSteerable` also: the thread reads idle), it goes on in place: the call keeps its call id,
cwd and pair, a flow row `hold_cleared` (call id, lineage, reason, input state) is written, and the records after it are the suffix
and so superseded. What it sends follows `input_state` of the held row:

- `delivered` (a turn carrying the packet started): the next turn `<call_id>/t<k+1>` of the same thread carries the marker and a
  fixed instruction - the previous turn stopped for the reason, continue the same task, the output schema is unchanged;
- `not_delivered` (stopped before sending, `thread/start` refused, the packet turn's `turn/start` refused): the host is asked
  first whether the refused request made anything. For a creation, `thread/list` at the call's cwd: a thread with no turn is
  adopted, a thread with turns is an unknown outcome, and none lets `thread_requested` be written again at the same cwd with the
  next send ordinal (the answer proves the request finished, so no grace is waited for). For a turn, the marker
  `<call_id>/t<k>` on the thread: a turn that carries it is an unknown outcome and nothing is sent; with every turn read in full
  and none carrying it, the original packet is sent as `<call_id>/t<k+1>`.

A turn the retry sends is an ordinary turn, whatever its ordinal; the `turn_requested` row says so (`repair`: false) and a
recovered retry turn is not taken for the call's schema repair: the call keeps its one repair turn, and `repair_used` is true
only when it was sent. A call held again on the retry has a second held row and its node holds again. When any condition fails, the held position is an
ordinary first failing position: it is superseded, the boundary is restored, `p_held` is kept on its ref, and the call runs with
its full packet in the restored worktree.

### `crw flow resume`

`crw flow resume [--state DIR] [--socket PATH] [--repo DIR] [--args JSON | --args-file FILE] RUN SCRIPT` opens the run's stored
journal and the App Server host, checks that the script and the arguments are the run's (their digests are on the run's `started`
row; an edited script is not a resume of the run), and runs the walk above. It prints the result as `crw flow run` does and exits
0 when the script completed, 1 when it ended in `script_error`, 2 for a command line, a store or a script it cannot use. It goes on
with every call that is not settled and not in recovery hold; the calls in recovery hold stay until `crw flow recover` settles
them. `dag-restart` names the command by the same reading of a call's rows (`internal/flow/callstate`): `crw flow recover` for a
call in recovery hold, `crw flow resume` for every other unsettled call.
