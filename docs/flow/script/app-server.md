
## The App Server backend

`crw flow start` runs a run as session calls ([Session calls](session-calls.md#session-calls)) by default; this backend, a Codex thread of
its own for each call, is the backend of tests and shadow runs and of a run started with `--backend app-server`.
`internal/flow/backend/agent` runs a call as a persistent Codex 0.154 thread. A thread is never ephemeral: Codex refuses
`includeTurns` and `thread/turns/list` for an ephemeral thread, which would take away pull recovery and the audit of a run.

- `thread/start` carries the call's own `cwd`, the role's model, `config.model_reasoning_effort`, the role's sandbox (workspace-write
  for `executor` and `executor-t1`, read-only for every other role), `approvalPolicy: never`, `ephemeral: false` and the role
  prompt as `developerInstructions`. Nothing that names the call is written there: the 0.154 `Thread` has no such member, so
  it could never be read back. A pair's `autoCompactTokenLimit` travels in the same `config`.
- The pair the thread is configured with is read with `thread/read` (`includeTurns: false`): `Thread.model` and
  `Thread.reasoningEffort`. A different or null value ends the call `refused` with `pair_refused`, and the thread, with the pair it
  read back, stays in the journal. Every `turn/start` repeats the thread's model and effort, so nothing changes a thread's pair.
- A turn's operation is `<call_id>/t<k>`. It is sent as `clientUserMessageId` and as the first input line `crw-flow-op: <op>`; a
  turn read back from the host is identified by its `userMessage` item's `clientId` first and by that first line second. The
  call's schema is the turn's `outputSchema`.
- A turn's end comes over the notification bus. A completed turn's answer is its last `final_answer` message on the bus; from a
  provider that sets no message phase (0.154 `MessagePhase` is nullable) it is a last message without a phase when no
  `commentary` message came before it. A completed turn with neither is read in full, and the host's items decide its answer.
  When no end comes, or the feed may have lost some, the backend reads the turn it
  recorded: it pages `thread/turns/list` with `itemsView: full` up to that turn id and reads a turn the list gave only as a
  summary with `thread/items/list`. It never hydrates with `includeTurns`. A turn whose items cannot be read in full is
  unreadable, not empty: the call stays with the reconcile pass.
- An executor turn never carries a `cwd`: a turn `cwd` moves the thread's working directory for that turn and every later one, so
  an executor's directory is fixed by `thread/start` or `thread/fork` alone. An architect turn carries its call's directory.
- If a run pauses, the turns in flight are interrupted with `turn/interrupt`; the same request is sent for a cancelled run. The
  command an op of a cancelled run started (the `crw-dev ci local` of `crw.verify`, the integration batch) is killed with its whole
  process group, so the run's wait for its ops ends within `execgroup.KillGrace` (CRW-1573).
- The backend is the host side of reconcile (`engine.ReconHost`): it lists threads by cwd and `appServer` source to the end of the
  cursor, reads turns in full, archives an orphan, and says what is known about a creation request
  (`CreationState`). For every `thread/start` and `thread/fork` it records the connection the frame went out on before it is
  written (`Client.ConnectionID`) and names its own process on the intent (`Config.Owner`; by default the pid and the kernel
  start time). A request of this process is closed when that connection is no longer the client's; a request of another process
  is closed when the process is gone (`Config.OwnerAlive`; a process it cannot prove gone is alive).

### Working directories

One allocator makes every directory a call runs in. An executor's is an attempt worktree
`<state>/flow/<run>/wt/<node>/a<attempt>-l<lineage>`, where the attempt is 1 plus the allocations of the node already journaled.
There are two ways to get one, and the call's `prepared` row records which (`allocation`: `kind`, `attempt`, `lineage`, `path`,
`start_commit`):

- `fresh`: a call that creates a thread and adopts no restore - the first build, the rebuild after a `null_model` and the
  `executor-t1` build - gets a new worktree made with `git worktree add --detach` at C, the result commit of the node's last
  `crw.commit` row (else the base recorded for its first attempt). The `prepared` row is written first, the worktree is made
  after it, and `thread_requested` follows. The node's previous worktree is retired: its HEAD and working snapshot are committed
  to `refs/crw/flow/<run>/retired/<node>/a<n>`, and the edits of a `null_model` attempt stay there.
- `restore`: when a resume suffix has restored the node's worktree (a flow row `worktree_restored` with the node, lineage, attempt,
  path, `C`, `P` and the verified `snapshot_digest`), the node's first executor call adopts it as its cwd - as a fresh thread, as
  an `executor-t1` thread, or, for a continuation, by forking its handle's thread (`thread/fork` with `lastTurnId` and the
  restored `cwd`, rows `fork_requested` and `fork_started`). Nothing is made, checked out or reset. The live snapshot is compared
  with the digest before `thread_requested` (or `fork_requested`) and again before the call's first `turn_requested`; a
  difference sends nothing more, leaves the call's last row where it was, writes one `escalation` with reason
  `restore_snapshot_mismatch` and ends the call `unknown`. The node is stopped for the rest of the run: every later call of
  the node (a fresh executor call, a continuation of its handle) ends `held` with the same reason, makes no worktree and
  sends nothing; the next resume restores the boundary in a new lineage.

The `cwd` and `worktree` options of a call are journaled in `prepared` but do not choose its directory: no two calls share one, so
the allocator does. An architect turn that continues its thread and names a `head` gets a call-private detached worktree at that head
(`calls/<call_id>/wt`, a later allocation of the same call `wt-1`, ...), named as the turn's cwd; the call's terminal row names it (`own_directory`) and the backend removes it after the row with a context of its own, and a resume removes every such directory an earlier process left (journal.md); inside a resume's suffix such a turn runs on a
fork of the previous process's thread in the same kind of directory, which is its own as well (made again at its recorded head when a held fork
goes on, removed after its terminal row, kept while its outcome is unknown). A continuation that runs in no restored worktree makes nothing and keeps the directory its thread started in. Every other role
runs in `<state>/flow/<run>/calls/<call_id>/wt`, a detached worktree at the `head` option (else the node's base); a reviewer
reads it through a read-only thread. The run commits and reads the head from git (`Allocator.Commit`, `Allocator.Head`); a model
never does. Every git command of the allocator runs in the directory it is given: the variables that route git to a
repository, a work tree or an index (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the rest of `git rev-parse
--local-env-vars`) are removed from its environment. The working snapshot (`Allocator.Snapshot`) is the tree id of every file the
worktree holds that git does not ignore - a tracked file is always held, whatever an ignore rule says - computed in a throwaway
index that starts as a copy of the worktree's own. The copy's assume-unchanged flags are cleared, and so are its skip-worktree
flags on files the worktree holds, so a flag never hides a file's working content; a skip-worktree entry whose file is absent (a
sparse checkout) is held with its index content. The worktree's own index keeps its flags.
