
## Reconcile and recovery

A request that was sent and got no answer, or a run process that died after an intent row or after a send, leaves a call whose
last row is an intent: `thread_requested`, `fork_requested`, `turn_requested` or `turn_started`. A call whose last row is
terminal - a held call included - is never looked up on the host. `Reconcile` (`internal/flow/engine`) looks up every other
call of the run, and the calls whose `outcome_unknown` it left standing, before a resume goes on and before a node starts a new
attempt. It finds a call only by what the 0.154 host returns: the exact `cwd` and the `appServer` source of `thread/list`
(every page), `Thread.parentThreadId` null, `Thread.createdAt` not earlier than the intent minus five minutes, for a fork
`Thread.forkedFromId`, and for a turn the `clientId` of its `userMessage` item (then the first input line) read with
`thread/turns/list` (`itemsView: full`, every page) and `thread/items/list`. `developerInstructions` and `threadSource` are not
identity: the host does not return them.

- **`thread_requested` without `thread_started`.** A candidate that holds any turn is foreign: no turn of the call is sent
  before `thread_started` is recorded. Candidates without a turn are interchangeable containers; the one with the earliest
  `createdAt` (then the lowest id) is adopted - `thread_started` with `recovered: true` and the evidence - and the others are
  archived, each with an `orphan_thread` flow row.
- **`fork_requested` without `fork_started`.** The same with candidates whose `forkedFromId` is the recorded source thread. A
  fork carries the copied prefix, so a candidate is adopted only when its turns are exactly the recorded prefix: the same
  number, the same order, each matching by turn id or marker, all ended and all read in full. A turn after the boundary, a
  reordered or different prefix, or a prefix turn that cannot be read in full is foreign or unreadable.
- **No candidate.** The request is absent only when it is proven finished: the connection that carried it is closed (the
  backend's own record of the connection the frame went out on, or the death of the run process that held it) and a second
  complete listing taken at least two minutes after that close - the creation grace of `internal/relay/managed` - is still
  empty. Reconcile records when it first saw the connection closed (`creation_closed_seen`) and waits for the grace. Then, and
  only then, the call sends the request again under the same call id and cwd with the next `send` ordinal
  (`creation_resendable` records the proof); a thread of the first request that shows up later still holds no turn and is
  archived as an orphan at the next pass. A request that was never written needs no grace. While the connection is open and the
  answer pending the pass waits and sends nothing; if the answer is still missing when the wait ends the call stays open.
- **`turn_requested(k)` without `turn_started`.** A turn whose marker is `<call_id>/t<k>` is adopted (`turn_started`, recovered).
  Only when every turn examined is read in full, none carries the marker and the thread is idle is the turn sent again with the
  same identity; a turn that stays `notLoaded` or `summary`, or an active thread whose running turn has no marker, is not an
  absence.
- **`turn_started`.** A running turn is attached to again (the call waits for it, no new turn), an ended turn is read and
  harvested, a failed or interrupted one is classified by `codexErrorInfo`, and a turn the host does not list or cannot read is
  unknown.

What the pass cannot establish - a listing or a history that cannot be completed, a foreign or unreadable candidate, an absence
without the proof, a running turn without the marker - appends `outcome_unknown` (reason `dispatch_unknown`), writes the
`dispatch_unknown` escalation naming the node and puts the node in `recovery_hold`: no request is sent again, no new attempt
starts, no tier step is taken. Each resume runs the pass again over the calls whose outcome is unknown; a pass that establishes the
outcome appends `recovery_cleared` (mode `reconcile`, with the evidence) and then the recovered rows after `outcome_unknown`, and
the hold ends: the call is open again, the resumed call goes on from that outcome and the unknown one is not replayed. This holds
for an outcome that needs no row of its own (a turn already recorded, or a request proven absent) as much as for an adopted thread.

`crw flow recover CALL --adopt THREAD|TURN | --absent` is the only manual way out of a hold. `--absent` reads the host again
with a complete listing and history and accepts the call only when the proofs above hold (turnless candidates only, an
exact-prefix fork, a closed connection with the grace passed, or for a turn no marker and an idle thread); it appends
`recovery_cleared` with the evidence and then the rows the proof establishes, and exits 1 without appending anything but the
sighting of a closed connection when it does not hold. `--adopt` names the thread (at the call's cwd, a root thread of the
backend's source - the `appServer` source reconcile lists by - for a fork a fork of the recorded source) or the turn (on the
call's thread, carrying the call's marker when it has one) that is the call's, and records that an operator chose it; the chosen
turn is written as a `turn_started` row (`replaces` names the turn the call had recorded), so the next resume follows it. The command works on the run's stored journal and its host: the binary opens the relay store (`--state DIR`, else
`CODEX_SESSION_RELAY_STATE`, else the relay's discovery for the control socket) and connects to the App Server control socket
(`--socket PATH`, else the default one); `--run RUN` names the run, and without it the run that holds the call is used. A store
that does not exist is an error, never a new store.

A new attempt starts after the turn of the attempt it replaces has ended: a call the previous process left running on the node
(one whose position the resumed script has not taken over) is interrupted with `turn/interrupt`, the interruption is confirmed with `thread/read`, and only then is its call ended
`superseded` (reason `superseded_by_attempt`) and the new thread started. A turn that cannot be confirmed interrupted holds the
new call. A call is either taken over by its position of the resumed script or ended superseded by a new attempt, never both:
the two decisions are made under one lock, the first stands, and a position that is reached after its call was superseded runs
as a call of its own (the same call id, a new `prepared` row), so a call has one terminal row for each attempt. A new attempt
keeps the call id of its position and so its `<call_id>/t<k>` operation marker; one thread never receives a marker twice, and
two threads that carry the same marker are two attempts.

A run resumes a journal with `Config.Resume` (`ResumeOptions` set the grace and polling timings of the reconcile pass), and
`crw flow resume RUN SCRIPT` runs it on the stored journal and the App Server. The pass runs before the script; the script then
runs again and each position replays or runs live by the rule of [journal.md](../journal.md#the-resume-walk): a position replays
only when every earlier position of its path did, its input digest, input snapshot and effect proof are the recorded ones and its
last event is not `held`; a call that reconcile decided goes on from where it stopped (the adopted thread gets its first turn, a
creation proven absent is sent again, an adopted turn is waited for or harvested and then validated and repaired like any turn);
a held call whose conditions hold goes on in place, with `hold_cleared` written. The first position that does not replay and
the rest of its path are the suffix: its calls end `superseded` with the reason `superseded_by_resume`.

`crw flow resume` goes on with every call that is not settled and not in recovery hold (an intent nobody answered, a hold that was
cleared, a call that ended held); `crw flow recover` settles a call in recovery hold, and a call it does not accept is a call
`crw flow resume` takes.

### Old runs of the retired review paths

The project lane judges a node by its unit check, closes a milestone on its crosscheck and audits the acceptances before its report,
where an old lane called a reviewer for the node, a reviewer for the milestone and archived the reviewer threads after its audit
(CRW-1429). Over an old run's journal the current script would not replay those positions: it would run them live, and every later
position of their path after them (the report would be sent twice). So a run whose journal holds a row of those paths is refused
with a reason (`engine.LegacyJournal`, the code `legacy_issue_child_run` of every legacy refusal) and its records are read. Within the
rows of its run id, a run is one when its journal holds

- anywhere, a `crw.review` or `crw.reviewerFinal` row: no operation of either name is registered, so only an old script wrote it;
- anywhere, an agent call of the retired role key of the `crw.reviewerFinal` op, from a lane or from a user workflow: the key names no
  role, so only an old build called it, and the role is part of a call's input digest, so the call does not replay in any script;
- in the lane scope, an agent call of role `reviewer`, a completed `crw.spawnDecision` row of a retired rule (`SP-REV-1`, `SP-REV-2`,
  `SP-REVF-1`, `SP-EXPL-3`: what an old lane wrote before the record of the review, when its process ended in between), or a
  `crw.archive` row. A `reviewer` call labelled `design-audit:` is the design check of a build before the critic, which ran it with
  role `reviewer`; the current lane calls the critic at that position;
- anywhere, the settle shape of the old close: a `crw.archive` row whose path holds a `crw.audit` call at the position right before it,
  or a `crw.escalate` call there and a `crw.audit` call at the position before that (the audit, the `audit_mismatch` escalation on a
  mismatch, the archive). It finds an old lane nested under any name and a script that imported `crw:lib/close` and settled that way.

The lane scope is the rows an old built-in lane wrote: the whole run when its started row names `project-lane`, `land-head` or
`night-batch` (`engine.LegacyLaneWorkflows`), and the subtree of every lane the run nested with `workflow()`: a `workflow_opened` row
whose label names one of those workflows, `workflow:<name>`, or a `{scriptPath}` whose base name without `.js` is one (a script nests
a sibling file by its name). The subtree is every row whose path is the child path of that position (`<parent>/<position>:workflow`)
or lies under it; a nested lane writes under its parent's run id and has no started row of its own. Outside the lane scope the role
key `reviewer` and the `crw.archive` op stay usable: a user workflow's reviewer call, one under a nested workflow that is not a lane,
and an archive that does not follow an audit on its path in that way make no run legacy. A workflow of the user that runs
`crw.audit` and then `crw.archive` on one path is the settle shape and is refused: start a new run, and its records stay readable.

The reason names the row (its op or role, its call id, its node or milestone, the label of an agent call that has no node, and for a
nested lane its `workflow_opened` label and call id), the retired path it belongs to (a node's old reviewer path, the path of the
`crw.reviewerFinal` op, the old `reviewer` call of the design check with CRW-1453, or the review-thread archive of the old close) with
CRW-1429, and ends with "the run is not resumed and nothing is written to it; its records stay read only". Every resume refuses it
before a row is written (`crw flow resume`, `crw flow resume RUN SCRIPT`, the resumes of `crw flow answer` and `crw flow next`), and so
does every command that would write a row to it: `crw flow cancel`, `pause`, `recover` (it appends no `recovery_cleared` row),
`dispatched` and the run's report up; `crw flow audit`
prints its results and records no row, and `--no-record` is unchanged. Every read path reads the old rows as they are: `crw flow show`,
the summary and the run readers, the proofs of the old ops, the trace export and `crw flow audit --no-record`. A role table that
changed since the run started is a repin on resume, never a refusal: the refusal comes from the rows.

What the rows cannot show: an old lane nested under a name that is none of those workflows, whose process ended after a reviewer
call and before the row that recorded its review, leaves no row a marker matches. Its old copy imports composites that `crw:lib/lane`
does not export, so the resumed child does not load, and its `workflow()` position fails without running any op live.

A call that ended held has one resolution command whatever the reason (a spent budget, a refused `thread/start`, a failed turn, an
engine pause, a flow binding that could not be written): `crw flow resume RUN SCRIPT`. A held row asks no operator input
(`--adopt`, `--absent`); what the call needs is the condition outside the run gone and a process that goes on with it. That
process proves on the host what the hold left open before it sends anything, and the proof decides what happens:

- A turn of the call may still run: a pause whose `turn/interrupt` the host ignored, or whose end did not come in time, ends the
  call held `engine_paused` all the same. Every thread a held attempt of the call sent a turn on is read (`thread/read`); a running
  turn of the call is interrupted and its end confirmed with `thread/read`, as a supersede does. While a turn still runs, the
  thread cannot be read or it runs a turn that is not the call's, the call stays held as it is: nothing is sent on its thread,
  and no new attempt starts on its node (no worktree is kept or restored, another call of the node ends held `recovery_hold`).
- A refused request (`thread/start`, `turn/start`) made nothing only if the host shows it: the listing of the call's cwd, or the
  marker of its operation on its thread. When that proof fails (the listing is not complete, a thread with turns stands at the
  cwd, a turn carries the marker), nobody can prove the outcome: the call ends `outcome_unknown` (`dispatch_unknown`) with an
  escalation. It is then in recovery hold, and dag-restart names `crw flow recover` for it.
- The resumed process binds every engine turn as the first process does (`host_config_flow_binding` is written before the turn);
  while the flow-bound directory is still unsafe the call ends held for it again with nothing sent.

When the proof holds, the call goes on in place (the original packet before a thread existed, the continuation instruction after a
delivered turn) and `hold_cleared` is written; where it cannot go on in place (a worktree failure, files that are not the hold
snapshot, an attempt that waited for an older turn and made nothing) the call runs again as a new attempt. A resume while the
condition outside the run still holds (the budget, the host's availability) ends the call held again.
