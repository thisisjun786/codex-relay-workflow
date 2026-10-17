
## Session calls

A run whose configuration names a session host (`engine.Config.Session`: the host, the step bus, the run's owner thread) runs every
agent call as a session call (CRW-1316). The owner thread - the child (project session, relay role `parent`, [the level table](../../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels)) - runs the call's turns on role subagents it
spawns, and the engine starts no thread and no turn of its own: it publishes a step, binds the subagent the owner reports, reads
its answer from the host and records it in the rows relay acceptance reads. Everything around a turn is the engine's as for any
call: the `prepared` row and its allocation, the gate, the schema validation and its one repair, the terminal rows, the pending
tree and the effect proofs.

The run's `started` row records `backend: session` (`app-server` for a run on the App Server; a run that records none is an App
Server run). A resume runs a journal on the backend it records: a resume on the other backend is refused with the reason before
any row is written. `crw flow shadow` replays either from the trace and names the recorded backend in its report (`backend`).

### The step

- Every role's call runs in a directory P of its own, made by the placer after the `prepared` row as for any call: the attempt
  worktree of an executor, a call-private checkout at the `head` option for every other role, the scratch worktree of a QA capture
  call. P is the call's cwd for every purpose (`pending_tree`, `p_held`, `qa_source`); the cwd the host reports for a subagent is
  its parent's, and is recorded only as `host_cwd`.
- A turn is a step. Its intent row `step_published` (k, `op` `<call>/t<k>`, `marker` `<call>/t<k>-a<n>`, `attempt` n, `action`,
  `input`, `input_digest`, `packet` and `packet_digest`, `role`, `agent_type`, `model`, `effort`, `workspace` P, `agent_id` of a
  `send_input`, the `note` about earlier attempts, `previous_answer`, the answer a repair turn repairs (a later attempt of the turn on a new
  subagent names it in its packet, after a restart too), for a `send_input` `tokens_seen` and `call_tokens`, what the turns before it
  counted, and the turn's place in the call's conversation: `conversation_turn`, the conversation turn it is (the call's task turn 1,
  its repair 2, on whatever subagent, attempt and k it runs), and `replaces_turn`, the turn it replaces when it is a turn run again;
  for a spawn step of a gated executor role in a run with an evidence cwd, `evidence_assignment`, the flow evidence record of the
  step, `evidence_cwds`, the directories it was written under before the row, and `evidence_ignored`)
  is committed before the packet `steps/<call>/packet-t<k>-a<n>.md` and the step
  `steps/<call>/t<k>-a<n>.json` are written under `<state>/flow/<run>/` (`engine.FileStepBus`). A step is `spawn` (a first turn, the
  attempt after a lost one, a continuation, a repair whose subagent is closed) or `send_input` (the repair turn on the open
  subagent); `close` steps (`steps/<call>/close-a<n>-<agent>.json`, one file per subagent, so two closes of one attempt never
  overwrite each other) name a subagent to close. A step whose packet or file cannot be written (a full disk) was sent to nobody:
  the run writes the flow warning `step_unwritten` (`marker`, `k`, `attempt`, `action`, `input_state: not_delivered`, `error`)
  and writes the step again at the poll until it can or the turn ends; the call is never ended unknown for it. A failure that does
  not pass shows how long in the record: the warning is written at the 1st, 10th, 100th, ... failure with `failures` and the
  `first_at` and `last_at` times, and a closing one whenever a warning was written: `written: false` when the turn ends
  unwritten, `written: true` when the step is written, after any number of failures.
- The spawn message is short; the packet holds the rest:

  ```
  crw-flow-op: <call>/t<k>-a<n>
  CRW-ROLE: <role>

  Your shell starts in a directory that is NOT your workspace. Your workspace is <P>: run every command with workdir <P> and edit only by absolute paths under <P>.
  Read the task packet <packet> in full and follow it exactly. You are a leaf subagent: never spawn agents.
  Your final message is exactly the JSON object the packet names, and nothing else.
  ```

  A `send_input` message is the marker line and the turn's input. The packet is self-contained: where the subagent works (only
  under the absolute path P; a role that writes nothing changes nothing; nobody runs `git commit`, `add`, `branch`, `reset` or
  `checkout`), the repository tools when the run has them (CRW-1488: a `Repository tools` section with the go of
  `verifier.go_root`, the file that sets it, and the prefix `PATH=<go_root>/bin:"$PATH" GOROOT=<go_root> GOTOOLCHAIN=local` that
  every command running go takes, because the subagent's shell is the owner session's and spawn_agent takes no environment;
  CRW-1570: the run's recorded TMPDIR, `env.TMPDIR` of its run.json and never the TMPDIR of the shell that relaunched the run, is
  a tool of its own, first: a fact that names the directory and run.json, `TMPDIR=<dir>` first in the prefix, which then goes in
  front of every command the subagent runs, and `TMPDIR=<dir>` first in the entries; a configuration with no go gives the TMPDIR
  alone, a spec without env the tools of before; on the session backend the packet is its only carrier), the
  role's overlay of `internal/flow/roles` without its native-spawn section, the earlier attempts, for a continuation the packet of
  the call it continues and a file with that call's answer, the task, and the output contract (the schema the final message
  satisfies). An App Server call's turn text ends with the same section (after the task; the journaled input is the script's), and
  its thread also gets the entries in its shell: the thread/start and thread/fork carry them as the leaf config keys
  `shell_environment_policy.set.<NAME>`, which Codex applies key by key over the server's own overrides, so the policy the server
  set stays. A thread resumed after an App Server restart, which the host rebuilds from its own configuration, gets them again in
  its shell: the backend's thread/resume for the call's turn (or for following a turn an earlier process started) carries the
  same leaf keys, taken from that call's run and kept nowhere (CRW-1559), and the turn text still names them.
- The owner reports what it spawned with a dispatch report in `<state>/flow/<run>/inbox/` and reads the run's answer from
  `replies/` (the commands are the CLI's). The run binds a subagent only when the host shows it is the owner thread's
  (`parentThreadId`), carries the marker in a turn's first user message and is configured with the role's pair. The answers:

  | Reply | When |
  | --- | --- |
  | `bound` | the subagent is the step's; its rows are written |
  | `pair_mismatch` | the subagent runs another pair: close it and spawn again with the role's pair (named); the step stays |
  | `retry` | the host or the marker turn cannot be read yet |
  | `refused` | another run, a report without a subagent for a step the run did not publish, or not a subagent of the owner thread |
  | `close` | the report names an attempt that ended (answered, lost), a step the run did not publish (an early report), or a second subagent for a bound attempt |

  A role whose pair the role table leaves to the host is bound without the pair check (`pair_unchecked`); an `agentRole` other
  than the expected agent type is recorded (`session_agent_role_mismatch`) and bound. A role that names a model and leaves its
  effort undecided is bound on its model alone: `thread_started` records the effort the host applied, with `effort_unchecked`, as
  the call's pair. A continuation is judged by the same rule: only the decided parts of the role's pair are compared with the pair
  the call's subagent was bound with, so a call bound `pair_unchecked` or `effort_unchecked` continues on a new subagent whose bind
  records the pair it runs, and a decided part that differs is refused (`pair_refused`); an explicit `effort` on a continuation of a
  role whose effort is undecided is compared with the effort the call's subagent ran. While a step waits, the run also looks the
  marker up on the host - the subagent that carries it for a spawn, the turn that carries it on the subagent for a `send_input` -
  so a subagent spawned, or a repair sent, and never reported is bound all the same.
- A bind writes `thread_started` (the subagent's `thread_id`, the host-read `model` and `effort`, `cwd` P, `host_cwd`, for a step
  with a flow evidence record `evidence_cwd`, the cwd the stop hook claims its receipt under,
  `parent_thread_id`, `agent_role`, `attempt`, `via: session`, `rollout_path`, `marker`, and `prior_pairs`, the pairs earlier
  attempts of the call were bound with when they differ) and `turn_started` (k, `op`, `turn_id`, `attempt`, `marker`, and the
  `conversation_turn` and `replaces_turn` of its step) in one batch;
  a `send_input` turn writes `turn_started` only. The relay reads the call's pair from its newest `thread_started` row and an
  author call's earlier pairs from `prior_pairs`. A bind whose host cwd no row of the call's newest generation names writes
  `evidence_placed` for it before it writes the step's flow evidence record there and before `thread_started`; a record that cannot
  be placed there leaves the subagent unbound, and the call is held `evidence_assignment_failed`.

### The harvest

- The run reads each bound subagent until its marker turn ends. The answer is the turn's last `final_answer` message, or from a
  provider that sets no phase the last message without one when no commentary came before it; a commentary message is no
  answer. When prose precedes the JSON, the object that ends the message is taken out of it: the completed row keeps the canonical
  `result`, its `result_digest`, and the message as it was (`raw_text`, `extracted`). The rest is the engine's: schema validation,
  one repair, `null_model`, `completed`.
- A turn that ended `failed` or `interrupted` with an error is `TurnOutcome.Failed` with its `codexErrorInfo`: a usage or rate limit
  pauses the run, an exceeded context window is a `null_model`, a host failure holds the call, as for any call.
- Tokens are the rollout's last `token_count` (thread/read `path`), less what the subagent's earlier turns counted, in all its parts: the input and its
  cached part, the output and its reasoning part, the total (`tokens`: `input`, `cached_input`, `output`, `reasoning_output`, `total`; a part that is 0 is left
  out of the row, and a row written before the parts reads them as 0). A recovered `send_input` turn takes that and the call's tokens, in the same parts, from
  its `step_published` row (`tokens_seen`, `call_tokens`), so it counts only what it adds. The call's tokens it takes are in no terminal row (the process that
  counted them died before it ended the call), so the resumed process counts them against the run's figure when it takes them, and the call's terminal row
  carries them: `RunSpent` and the process's `spent()` count them once. The App Server backend records the same parts from
  `thread/tokenUsage/updated`. A rollout that cannot be read leaves the turn unmeasured: the `tokens_unmeasured` row says `pauses: false`,
  and a session run's token ceiling counts the calls it measured.
- A pause, a cancellation or a deadline interrupts the bound subagent's running turns and reads the thread until it is no longer
  active; only then is the call ended `held` (or `superseded`). A call that waited for a dispatch retracts its step. A step whose
  subagent is known (a `send_input`, an adopted one) and was not bound yet has the turn that subagent started for it bound first
  (`turn_started`), so the rows name the turn the interrupt reaches; a spawn step no subagent was bound to has no turn to interrupt.
  The rows before an interrupt, that `turn_started` among them, are stored before it is sent: a journal that batches its rows
  commits them first, so a process that dies once the interrupt is sent leaves the turn the `interrupt_requested` row decides.
- An interrupt goes with the turn it was asked for. In the process that asked for it, that is the turn in flight. For a process
  that resumes the run it is the turn the rows bound to the step and attempt of the `interrupt_requested` row (its `op`, the step
  published last before it): that turn, if the resumed call goes on with it, ends `held` for the row's reason whatever it came to. Every
  other turn starts with no intent: a new attempt after a lost one, a step the resumed process publishes again, a turn the host
  started for a step after the interrupt (a `send_input` the owner had not taken). The intent is compared where the outcome is
  read; it is never moved onto another turn.
- Write isolation is recorded, never enforced (the host gives a subagent its parent's sandbox). With `SessionConfig.Trace` the run
  takes digests of P, of the owner's checkout and of the run's shared state when it binds and when it harvests, and writes a flow
  warning when they differ: `read_role_wrote` (a role that changes nothing), `executor_wrote_outside`, `shared_state_changed`.
- An executor's `HEAD` is compared with the commit its worktree is to be at (the node's last `crw.commit` after the allocation the
  worktree was made or restored for, else that allocation's start). A subagent that committed moved it: the commit is kept on `refs/crw/flow/<run>/stray/<node>/a<attempt>-<call>` (the
  executor calls of one allocation, a build and its fix rounds, each keep their own; the ref is created by a compare-and-swap
  against no ref, and moved only by a fast-forward to a descendant, the same call's later `HEAD`, with the commit it holds as the
  compare-and-swap's old value) and the
  completed row records `executor_head_moved` (`start`, `head`, `ref`); `crw.commit` commits the pending tree on `start` with the
  observed `HEAD` as the compare-and-swap's old value, and the proof takes the recorded stray `HEAD` whose tree is the pending tree
  as the call's state ([journal.md](../journal.md#the-proofs)). The subagent is closed when the call has its terminal row.

### Attempts, restarts and continuations

- An attempt is lost only when its marker turn is not completed, its thread is `notLoaded`, `systemError` or unknown to the host,
  and the turn ended interrupted or failed without an error, or has not changed for `SessionConfig.LostAfter` (ten minutes); the
  thread is read once more before. A thread the host cannot be asked about is not judged, and an active one is waited for.
  `session_attempt_lost` (`attempt`, `agent_id`, `k`, `turn_id`, `reason`, `host_status`, `turn_status`) is written, the old
  subagent is closed, and the next attempt spawns in the same allocation; its packet says the previous attempt was cut off, and
  an executor's says to read `git -C <P> status` and the diff first.
- One allocation serves every attempt of a call: the App Server reconcile, the live resend and the retirement of a worktree are not
  used for session calls. A resumed run goes on with an open session call in place, from its last row:

  | Last row | The resumed call |
  | --- | --- |
  | `prepared` | publishes the first step (the directory an earlier process made is the call's) |
  | `step_published` | answers first a dispatch report the owner wrote that no process answered (it waits in the inbox), then looks the step up on the host - the subagent that carries the marker for a spawn, the marker turn on the subagent for a `send_input` (a repair is never sent twice) - and binds what it finds; else publishes the same step again |
  | `thread_started` | adopts the subagent the row names and writes only its `turn_started` (nothing is spawned or published again; one the host does not know, or shows `notLoaded` or `systemError`, for `LostAfter` loses the attempt, and the gate decides the next; a read the host cannot answer judges nothing and leaves that time as it is, as in the harvest), then reads it as after `turn_started` |
  | `turn_started` | retracts the step file the dead process left and reads the bound subagent: harvests a completed turn, waits for a running one, loses a lost one |
  | `session_attempt_lost` | closes the old subagent and spawns the next attempt |

- A held session call goes on in place after the subagents of its held attempts are shown not running (a running turn is
  interrupted and the thread read until it is not active): a delivered turn is taken again by a new attempt whose packet says why
  the previous one stopped (a call held in its repair turn goes on with that repair: the packet names the task once, the answer
  repaired and the repair instruction, and the call has no second repair, as on the App Server path); a step that was published and not bound is looked up by its marker first.
- A new attempt of a node supersedes the open session calls of the previous process only after every subagent they were bound to,
  or that took a step they published, runs nothing.
- A continuation (`resume: handle`) runs on a new subagent and writes its own `thread_started` row, so the handle chain and the pair
  the relay reads name it. Inside a resume's suffix it never forks: it takes the checkpoint branch (`checkpoint_started`), and its
  packet carries the journal's record of the handle's turns, each with the input of the step or request it took: for a session
  subagent the conversation it goes on from, the turns of the calls its handle continues before its own and the call's conversation
  before the turn its subagent started at. A session call's conversation is its turns at the conversation turn each records
  (`conversation_turn` of its `turn_started` row), each conversation turn once: a turn run again (the next attempt of a lost turn;
  the turn a held call goes on with in place on a new subagent, at the next k) is recorded at the conversation turn of the turn it
  replaces and names that turn (`replaces_turn`), and supersedes it, and the packet is built from that list alone. The place is
  recorded when the step is published and the turn bound, never read back from held rows or k; a `turn_started` row of a journal
  written before the place was recorded (no `conversation_turn`) takes it from the `repair` member of the `step_published` row at
  its k (the task turn 1, the repair 2), and a later such turn at that place replaces the earlier one. The continuation's input digest keeps the handle's own thread chain: a position replays only after every one before it.

| Row | In a session call |
| --- | --- |
| `prepared` | as for any call, with `backend: session` and the allocation every attempt shares |
| `step_published` | the intent of a step, committed before the step file and the packet; `conversation_turn` and `replaces_turn` record the turn's place in the call's conversation |
| `thread_started` | a subagent bound to an attempt (one row per attempt and per continuation) |
| `turn_started` | the turn of a step the subagent took, with the place its step records (`conversation_turn`, `replaces_turn`) |
| `interrupt_requested` | the engine's interrupt of the turn in flight (`reason`, `op`); a resumed process applies it only to the turn bound to that step and attempt |
| `session_attempt_lost` | an attempt whose subagent will not finish its turn |
| `evidence_placed` | the intent to write a step's flow evidence record under a directory no row of the call's newest generation (its rows from the newest `prepared` on) names yet: `evidence_assignment`, `evidence_cwd`, `attempt`, `marker`; committed before the record is written, so the release of a superseded or prepared-again call and the restore of a resumed one find every copy from the rows; it changes no reading of the call's state |
| terminal rows | as for any call |
| `session_wake` (flow row) | a wake of the owner thread the run staged: `batch`, `wake_id`, `renotify`, `ops`, `owner`, `owner_status`, `waiting_seconds`, `staged` (and `error`, or `reason` when the relay already held the message and staged nothing) |
| `session_host_warning` (flow row) | what the session host adapter's marker search could not tie as it should: `op`, `warnings` (`no_ledger_line`, `ledger_op_differs`, `several_spawns`, `window_ended`) |
| `role_store_changed` (flow row) | written by no command: the run's process is the one party that records the pairs a run runs on and their change (its started row, `policy_repinned`); the journal of an older run may hold one (`pinned_digest`, `store_digest`) |
| `escalation` (flow row) | `reason: session_unresponsive`: the steps waited `UnresponsiveAfter` without a dispatched report (`owner`, `ops`, `batch`, `waiting_seconds`); the run goes on |
| `step_unwritten` (flow row) | a step whose packet or file could not be written: nothing was sent (`input_state: not_delivered`, `marker`, `error`, `failures`, `first_at`, `last_at`, and `written` on the closing row); the run writes it again |

### The owner's commands

A session run is started, stepped and resumed from its owner thread with the commands of `internal/flow/cli`; every one reads the
run's directory `<state>/flow/<run>/` and none writes the journal, which only the run process writes.

- `crw flow start WORKFLOW [--backend session|app-server]` starts a session run by default; `--backend app-server` opens a thread
  per call, for the tests and shadow runs. The spec records the `backend`, and the run's temporary location: `env.TMPDIR` in
  `run.json` (`crw-flow-run/1`; an optional member, absent from a spec written before it, which reads as before) is what the one temp
  function (`internal/tmploc`, `DirFrom`) answers for the starting environment, as written, also when that is its default. It is asked
  once, at the start, and no other variable of the caller's environment is recorded (no `PATH`, `GOFLAGS` or `GOTOOLCHAIN`); a TMPDIR
  that is not absolute, or a directory that is missing, not a directory or not writable, stops the start (exit 1, with tmploc's
  reason) before the spec is written and before any process starts or owner is bound. A session run's pool is
  `min(pool, agents.max_threads - 1)` (the owner keeps a slot for a helper of its own; `agents.max_threads` is read from `<CODEX_HOME>/config.toml`, default 6), the
  preview lists the spawn format of each of the six roles (`spawn_agent(agent_type, message, model, reasoning_effort,
  fork_context=false)`) and the six session facts below, and the answer names the command the steps are taken with
  (`next`). Before the process starts the owner thread is bound (below).
- `crw flow next RUN [--wait S] [--json]` answers the run's `state` (`running`, `waiting_session`, `waiting_decision`, `paused`,
  `ended` with the `ended` status and state, `starting` below), its `steps`, the `slots` (`limit`: `agents.max_threads`, `open`: the subagents of
  calls not ended and of close steps not taken, `agents`: their ids), the `spawn_formats` and a `hint`; with `--wait` it waits up to S seconds for a
  step or a change of state. Each step carries the step file's members (call, k, attempt, op, role, action, agent_type, model,
  reasoning_effort, message, packet, workspace, agent_id) and the host call to make with the live argument names: a spawn step is
  `spawn_agent` and then `crw flow dispatched`, a `send_input` step is `send_input(target, message)` and then `crw flow
  dispatched`, a close step is `close_agent(target)`; the hint says to alternate `wait_agent(targets, timeout_ms)` with `next
  --wait` (its `targets` are the open subagents' ids), which subagents to close first and when the slots are full (`agent thread limit reached`; the step stays). A close
  step's file is removed once next has handed it out. A run no process holds that is neither paused nor over is resumed as
  `crw flow resume RUN --detach` does, and the hint says so (`resumed_pid`). next writes neither the owner's orchestrator line nor a
  row of the run (CRW-1372): while a process holds the run, that process is the one party that records the pairs it runs on (the
  `role_pairs` and `policy_digest` of its started row, which names its process lock as `process`) and writes the owner's line from
  them right after that row (below), and next reads both. The spawn formats are the pairs of the holder's started row: the newest
  started row when the process lock it names is held, or a newest row that names no process (one of a process of a crw before the
  process lock, which bound before it recorded its row) when no process lock of the run is held. A run whose holder has not recorded
  its started row yet (the newest row names a process lock that was let go, or names none while a process lock of the run is held:
  the holder is coming up, or a command that writes a row holds the run lock for a moment: an answer holds it from the end of the
  process it drained until its row is stored, and the resume it starts takes the lock after it), or whose start holds its
  start lock, is `starting`: next reads it again, up to 15 seconds whatever `--wait` says, never takes an earlier process's pins for
  the holder's and resumes nothing; still starting then, it answers `starting` with no spawn formats and a hint to ask again (at once
  without `--wait`, else when the state changes or `--wait` runs out). A holder that took its process lock less than 15 seconds ago,
  recorded its started row and has not bound the owner's line under that row's digest yet is read again the same way, and its line
  is then reported as it is. A run no process holds answers the role store's spawn formats
  (a resume reads the store). next warns, and records nothing, when the owner's last line is not the holder's (another live run's,
  another digest's, or none: the hook gives the owner's spawns no pair of the run until the run's process, or its next process when
  it is resumed, binds it), when the role store reads other pairs than the holder pinned (the process runs on its pins; a resume
  takes the new ones and records them in its started row and the change in a `policy_repinned` row) and, for a run next resumed,
  when that process repinned (its `policy_repinned` row: it runs on the role store as it reads now, not on the pairs the earlier
  process pinned). A run that recorded no pairs answers the store's, as before.
- `crw flow dispatched RUN CALL --agent ID --k K --attempt N [--wait S] [--json]` writes the dispatch report into the inbox (a
  temporary name and a rename), waits up to S seconds (120) for the run's reply and prints it (`bound` exits 0, the other replies
  exit 1). A report of a run no process holds, or one the run has not answered in time, is answered `queued`: it stays in the inbox
  and the run answers it when it reaches the step (a resumed run included).
- A resume (`crw flow resume RUN`, the process `next` starts, and the engine's `crw flow resume RUN SCRIPT` through
  `journal.ResumeEnv`) goes on with the backend the run's started row records (`engine.StartedBackend`): a session run gets its
  session environment again (the session host adapter of `internal/flow/sessionhost`, the run's `FileStepBus`, the owner of its
  record). A spec that names another backend than the started row is a mixed resume, refused with the reason as an input error
  (exit 2); a run whose first process stored no started row goes on with its spec's. Every one of them holds a session run alike
  (the foreground command through `engine.ResumeHold`): at most `agents.max_threads - 1` calls in flight whatever `--pool` says,
  and the owner thread bound to the run while the resume runs and its line closed when the run ends.
- Every detached launch of the run's process (the start, `crw flow answer`, `crw flow resume RUN --detach` and the resume `next`
  starts) gives the process the caller's environment with the variables the spec records in place (`env`, today `TMPDIR`; exactly
  one entry each), so a run relaunched from a shell with another TMPDIR, or none, runs below the TMPDIR its start recorded; a spec
  written before the record leaves the environment the caller's, as before. A recorded TMPDIR that is missing, not a directory or not
  writable is refused before anything of the run changes: `crw flow answer` (without `--no-resume`) refuses with `run_env_unusable`
  (exit 1, nothing appended, nothing drained, no process started), and the same answer applies once and resumes the run once the
  directory is made again; `crw flow resume RUN --detach` exits 1 naming the directory; `next` resumes nothing and warns "its resume
  did not start" with the reason. `--no-resume` starts nothing and is not asked; the later relaunch refuses. The spec is never
  rewritten: the directory is made again.
- A run that works in the process that reads its spec (the run process `crw flow _run`, the foreground `crw flow resume RUN` and
  the engine's `crw flow resume RUN SCRIPT`) puts the env its spec records into that process's environment before it takes a lock
  and puts back what was there when the run is done, so everything the run reads from its own environment (the land-head ci local,
  the integration batch's TMPDIR, the temporary files of its operations) is below the recorded TMPDIR. A recorded TMPDIR that is
  missing, not a directory or not writable refuses that process (exit 2, the reason naming the directory) before any lock or row.
- The owner thread is bound to a session run by the `orchestrator` line of its flow-bound file (`internal/flow/bound`
  `BindOrchestrator`: `kind: orchestrator`, `run_id`, the `policyDigest` of the role table and the `helperPairs` of the six roles
  architect, executor, executor-t1, reviewer, explorer, critic; `Line.Validate` takes no orchestrator line without the first five;
  the critic is optional, so the open line of a run an earlier crw bound stays readable and its critic spawns are undecided, and a
  pair an earlier crw wrote for the retired role key of the `crw.reviewerFinal` op is read and never applied).
  A pair carries the decided parts of its role only: an undecided role has the empty pair, a role that names a model and leaves its
  effort undecided has the empty effort, and an effort without a model is no role's pair.
  The start writes it before any process of the run exists. While a process holds the run, that process is the one party that writes
  it (CRW-1372): right after its started row is in the journal it writes the line again with the table that row pinned when it is
  not the last line (`engine.SessionConfig.Started`; the foreground `crw flow resume RUN SCRIPT` through `engine.ResumeHold`), and it
  closes it when the run ends (`CloseOrchestrator`). `crw flow next` only reads it, so a line that is missing while a process holds
  the run (its bind failed, or a process of an older crw left it) is reported and stays missing until the run's next process, a
  resume, binds it after its own started row. A session run's process holds its process lock, `<state>/flow/<run>/process.<token>.lock`,
  from before it takes the run lock until after it lets the run lock go, and its started row names the token (`process`); it
  removes the file when it ends (the file of a process that died stays, held by nobody). A process lock that cannot be taken is
  warned about and the process runs with a started row that names no process. A live run is one whose process holds its run lock, or whose start holds its
  start lock (`<state>/flow/<run>/start.lock`, held from before the start binds until the run's process holds the run lock, since the
  start binds before its process exists): the open line of a run with neither is closed before the new one is written, and a thread
  holds the line of one live run, so a second start while the first lives, or is still starting, warns and does not bind. The read
  of the thread's last line and the append are one step under a lock of the flow-bound directory, so two near-simultaneous starts of
  one thread cannot both bind, and neither can their two run processes. A thread whose binding cannot be written is warned about; the run starts.
- The spawn hook decides the owner thread's spawns by that line. It refuses two: a spawn by a subagent (a leaf) and a full-history
  fork (`full_history_fork_refused`: the fork keeps the parent's pair). Every other spawn is allowed with `model` and
  `reasoning_effort` overwritten from the decided parts of the pair of its role (an undecided effort keeps the `reasoning_effort` the
  spawn was sent, and an undecided role keeps the whole spawn, with the warning `role_undecided`): the role the first `CRW-ROLE:` line of the message header names when it
  is exactly one of the six (a parser of the flow-bound path; the ordinary path's oracle parser is unchanged), else the role of
  the `agent_type` (worker is executor), else explorer. The ledger line (`decisions/<owner>.jsonl`) records the `role`, the `op` of
  the `crw-flow-op:` first line, and a `warning` where the table has one: `role_agent_type_mismatch` (the header role does not pair
  with the agent_type: executor-t1 with executor, critic with reviewer, the others with their own name), `unrecorded_spawn`
  (no step marker) or `unrecorded_author` (no marker, an executor's or architect's type). A ledger that cannot be written allows the
  spawn and says so on stderr, and a flow-bound directory or file that cannot be trusted is read as no binding, with a stderr
  warning: no spawn is refused for it. The run joins the ledger's `op` with the owner turn's `collabAgentToolCall` items to find the
  subagent of a published step after a restart ([session-host.md](../session-host.md)).
- The run wakes its owner. When it publishes a step and the owner thread is idle or notLoaded (the host's `thread/read`), it stages
  a `session_step_waiting` message through `run:<id>` (`journal.StageWake`), which the supervisor channel delivers to the run's
  owner (`flow_runs.actor_task_id`): a new turn on an idle thread, a resume and a turn on a notLoaded one, after the turn on a busy
  one. The logical id is `(run, process, batch)`, the batch being the set of waiting steps, so a wake of the same steps staged again
  is one message: the run's n-th process (its n-th started row) wakes under `<batch>/e<n>` (the first under `<batch>`), because the
  relay keeps a message id for good while a process counts its re-notifications from 0 again, so a resume stages new messages. A
  wake the relay already held stages nothing, and its row says `staged: false` with the relay's `reason`. The message's delivery is
  a `message_delivery` row the daemon's settle writes ([reporting.md](../reporting.md#through-the-supervisor-channel)). While steps wait without a dispatched report the run notifies again after `WakeAfter` (15 minutes), each interval twice
  the one before up to `RenotifyMax` (an hour), whatever the owner's status; after `UnresponsiveAfter` (2 hours) it writes the
  `session_unresponsive` escalation once and goes on waiting. Each wake is a `session_wake` row. A session run's started row pins
  its role pairs (`role_pairs`), and the session host adapter's warnings are `session_host_warning` rows.
- With `SessionConfig.Trace` (the session backend's `Trace`) the digests are: the workspace, P's `git status --porcelain` and
  `HEAD`; outside, the owner's working directory's when it is a git checkout other than P; shared, the refs under `refs/crw/` of the
  run's repository other than the run's own `refs/crw/flow/<run>/` (the run moves those itself) and the binding files of the
  flow-bound directory (the hook's ledger and claims are left out: the owner's spawns write them while a subagent works).
- The session facts of the preview are warnings, never a refusal (CRW-1318): `owner_bound` (the owner can be bound: a thread id, a
  readable binding, no other live run, live as the binding reads it: its process holds the run lock or its start holds the start lock), `role_models` (the six pairs, a role without one or without an effort; same-thread review is warning-only when observed),
  `owner_state_writable` (the run directory, the flow-bound directory, the control socket and the repository), `subagent_slots`
  (`agents.max_threads` leaves a slot for a call) and `multi_agent_v1` (`features.multi_agent_v2` is off: under V2 the protocol
  stops); `verifier_prerequisites`, the sixth, is judged for every run (CRW-1319).
