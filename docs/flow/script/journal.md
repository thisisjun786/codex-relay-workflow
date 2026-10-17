
## Journal

`crw flow run` keeps the journal in memory; a run that must survive its process writes it to the relay store's flow tables
([journal.md](../journal.md) says how, and what a worktree proof reads from it). Each call journals the
rows `prepared` (written before anything is sent, with the role, pair, node, `stepDecision` and, for a continuation, the
thread it continues and what it inherited), `thread_requested`, `thread_started`, `turn_requested`, `turn_started` and one
terminal row (`completed`, `null_model`, `held`, `refused`, `outcome_unknown` or `superseded`); a session call writes `step_published`
in place of `thread_requested` and `turn_requested`, `session_attempt_lost` for an attempt it lost, and `evidence_placed` before it
writes a flow evidence record under a directory no row of the call's newest generation names yet ([Session calls](session-calls.md#session-calls)).
A held row records whether the
call's packet reached the host (`input_state`: `delivered` or `not_delivered`) and the `codexErrorInfo` verbatim; on a backend
that places calls an executor's held row also carries `p_held`, the working snapshot of its worktree when the failure was
classified, and its completed row `pending_tree`, the working snapshot when the turn ended. The run record holds the workflow name, the script digest, the helper digests, the digest of `args`, the meta, and
the path of the helper role store with the canonical digest of the seven configured pairs (the six agent roles and unit-check) (`policy_source`, `policy_digest`),
the role table's warnings (`policy_warnings`) when it has any, and the `backend` the run's calls run on (`session` or
`app-server`).

The `prepared` row also carries what the resume walk compares: `input_digest` (the prompt, schema, role, pair, node, options,
context boundary and the joins before the position), `prompt_digest`, `context` and, with `Config.InputSnapshot`, `input_snapshot`.
A join or `workflow()` position writes `join_opened` and `join_settled` flow rows, the latter with the `digest` of its item paths'
results; a resume writes `hold_cleared`, `worktree_preserved`, `worktree_restored` and `checkpoint_started` flow rows
([journal.md](../journal.md#the-resume-walk)).

A backend that places calls adds to a call's rows: the `allocation` member of `prepared` (below), `fork_requested` and
`fork_started` in place of `thread_requested` and `thread_started` for a continuation that runs in a restored worktree, and
`interrupt_requested` before a turn the engine interrupts. Flow rows named `escalation` carry a `reason` (`quota_or_rate`,
`restore_snapshot_mismatch`, `dispatch_unknown`) with the call and node they stopped.

The run surface (`crw flow`, [flow.md](../../../plugins/crw/skills/crw-canon/references/flow/command-table.md#the-crw-flow-command-table)) adds the rows it writes
and reads. A pause asked for from outside the engine (`crw flow pause`, a run deadline, the drain of `crw flow answer`) is one `run_paused` flow row (`by`, `reason`)
and then the calls that would send end held `engine_paused`. The operator's pause (SIGUSR1) and a deadline interrupt the turns in flight;
the drain of an answer to a run a process holds (SIGUSR2, `by` `answer`) lets them end and holds each call's next request, and is
asked once (a second drain, or a drain after a pause, writes no row). A process the operator or an answer paused (`by` `operator` or
`answer`) that would end completed ends with a `run/paused` row (`status` `paused`, and the `by` and `reason` of the last such ask)
instead of the `finished` row:
the engine's status is `paused`, the run process exits 0 and the hook tells no completion; a deadline, a token budget or a quota
pause ends with the `finished` row as before. A pause asked once the process has decided its end (while it writes its end row or tells
its completion) is refused and writes no row, so the `run_paused` rows and the end row agree. `crw flow show`, the digest and `crw flow
next` say who paused a run that ended paused from the `by` and `reason` of its `run/paused` row, not from a later deadline ask. An answer to a wait is the `decision_applied` flow row `crw flow answer`
appends under the run lock: `ask` (the question's id), `node`, `run`, `revision`, `attempt`, `plan_basis`, `scope`, `target`, `action`,
`class` (`escalation` or `jun_decision`), `applied_by` and the `decision` (`id`, `option`, `text`, `by`, `via`, `answered_at`,
`authority`, `authority_ref`); `crw.decide` reads `ask` and `node`. A run cancelled while no process held it ends with a `finished` row
of error kind `cancelled` written by `crw flow cancel`. The `finished` row of a completed run whose script returned an object with a
string `state` carries `outcome`: its `state`, `reason`, `source`, `escalation` and `milestone` (each one line, at most 120 runes) and
`lanes`, the lanes of its `lanes` array that did not land (`node`, `state`, `reason`, at most 16, with `lanes_more`). A value whose
`milestone` member is itself an object with a string `state` (a night batch returns `{state: closed|incomplete, milestone: <the project
lane's result>, lanes}`) carries the outcome of that object, and its own state as `run_state`, so a night batch tells the milestone's
`held`, `escalated` or `stopped` and its reason. A `milestone` member that carries an `id` is a milestone (what `crw.milestone` hands
a script), not a result, and is not read as one. The outcome state is read in this order: the `milestone` object without an `id`
and with a non-empty string `state` is the wrapped result, and its state wins; a `milestone` member that carries an `id`, or that
has no non-empty string `state`, is not read as one, and otherwise the top-level string `state` is the outcome state. A value whose
own `state` is not a non-empty string has no outcome. The top-level string `state` of a completed script is its outcome
whatever that state means to the script: the engine does not restrict its values (the repository's workflows return the
milestone's state). It is told in place of
`status=completed` (a script that wants `status=completed` returns no `state`). The completion notice and `crw flow ps` read the outcome from there ([reporting.md](../reporting.md)). The state of a run (`running`, `paused`, `dead`, `held`, `completed`,
`blocked`, `cancelled`, `failed`) is not a row: it is read from the run lock, the last `started`, `finished` and `run/paused` rows (a
run no process holds whose last process ended with `run/paused` is `paused`, not `dead`), the calls' rows
(`internal/flow/callstate`), the questions no `decision_applied` row has answered and, for `blocked`, the lane's own rows. A run meant
to start the nodes of a plan (a run of `project-lane` or `night-batch`, an old run of a `child` session's lane, or a run whose arguments name a `plan`) whose last
process finished `completed` with nothing left to go on with is `blocked` when no process of it started a node (no completed
`crw.release` row released one, no `crw.releaseChild` row an older build recorded released a child, no `crw.integrate` row landed one) and items of its joins threw
in its last process (`item_error` rows: a lane of `crw.dag` whose `crw.release` failed is one). The engine's status of the run stays
`completed`. A node the scheduler held back unreleased in the last process (capacity, a region in use: a wait a resume asks again) keeps the
run `completed`, and so does a node that started. Another script run (a judge panel, a scout) that names no plan never meant to start a
node: an item of it that threw is shown, and the run stays `completed`. The run views keep superseded questions as history and do not count them as waiting. Escalations with no ask appear separately from answerable questions; a finished escalated outcome keeps the run held without adding a waiting question. A later completion of the same escalation operation can make its recorded question current again.

The intent rows say what a later process needs to find the request again. `thread_requested` and `fork_requested` carry the
`cwd` the thread is to run in, the `send` ordinal of the request (1, then one more each time reconcile proved the previous
request absent and sent it again), `at`, the wall time of the intent in milliseconds, and `owner`, the run process that
holds the connection the request goes out on; `fork_requested` also carries the source thread, `through_turn_id` and the
`prefix` the fork must hold (the whole history of the source through the boundary, the turns it inherited when it was itself a fork
included). `turn_requested` carries the turn's input (without its marker line) and `resend` on a turn that
is sent again.
