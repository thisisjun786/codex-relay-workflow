# What goes up from a flow run

A flow run (a project run, the issue-lane of an oversized child) works for the thread that started it, the orchestrator, and the
orchestrator works for the management session above its project. This page says what travels upward, to whom, by which command, and
which row of the relay store's `flow_events` records it. The rows are described in [journal.md](journal.md); the relay commands the paths
end in are in the [relay README](../relay/README.md). The reports, the escalations and the completion digest go through the relay's
supervisor channel as direct messages ([supervisor-channel.md](../relay/supervisor-channel.md#direct-messages), CRW-1380); the plan summary
notice goes through `crw manage send-parent`.

| What | From | To | By | Recorded as | Pump kind |
| --- | --- | --- | --- | --- | --- |
| an escalation of the run (`dispatch_unknown`, `tiers_exhausted`, `blocked_scope`, `size_split`, `restore_snapshot_mismatch`, a configuration fault, the run-level hold `plan_superseded` with the pinned and head revisions and the node, CRW-1291) | the run | the level above the run's project (management seat, else lead, else store seat); a run whose plan names no project: the orchestrator thread | the supervisor channel (`flow_escalation`); else `crw manage send-parent` | `escalation`, `escalation_notice`, `message_delivery` | `run_escalation` |
| the completion digest | the run | as an escalation | the supervisor channel (`flow_completion`); else `crw manage send-parent` | `completion_notice`, `message_delivery` | |
| a run that stopped writing | nobody | the manager thread | the pump | | `run_dead` |
| the plan summary that is owed | the run, `crw flow apply` | the orchestrator thread | `crw manage send-parent` | `summary_document`, `summary_notice`, `summary_notice_settled` | |
| the receipt of an oversized issue | the child | the orchestrator | relay delivery, then `crw flow apply` | `apply_applied`, `apply_refused`, `apply_override` | |
| a milestone report | the run | the management session above the project (else the initiative lead, else the store seat) | the writer of `crw flow report-up` through the supervisor channel (`flow_milestone_report`) | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a run that started no node (`project_blocked`, CRW-1320) | the run | as a milestone report | the writer of `crw flow report-up` through the supervisor channel (`flow_project_blocked`) | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a project-level blocker or decision request | the orchestrator | as a milestone report | `crw flow report-up` through the supervisor channel (`flow_project_blocked`, `flow_project_decision_request`) | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a decision only Jun can make | the orchestrator | the management session above the project (else the store seat, then the manager) | `crw flow report-up` through the supervisor channel (`flow_jun_decision_request`); under a legacy lead it stands | `report`, `report_delivery` | `jun_decision` |
| a step that waits for the owner (a session run, CRW-1348) | the run | the run's owner | the supervisor channel (`session_step_waiting`, `run:<id>`) | `session_wake`, `message_delivery` | |

## Through the supervisor channel

The run (and `crw flow report-up`) hands a report, an escalation and the completion digest to `report.RelaySender`, which stages one
direct message on the relay's supervisor channel: the relationship is `project:<the project of the run's plan>`, the kind is the
report's (`flow_milestone_report`, `flow_project_blocked`, `flow_project_decision_request`, `flow_jun_decision_request`) or the notice's
(`flow_escalation`, `flow_completion`), and the subject is `<run>/<report id>` or `<run>/<logical id>`, so the message id is
deterministic and the same report staged again is the same message. The channel addresses it to the level above the project - the
management seat's owner, else a legacy lead, else the single store seat, which is woken like any other recipient - and the relay daemon
delivers it: a new turn on an idle thread, a resume and a turn on an unloaded one, after the turn on a busy one.

Staging writes only `queued`: the `report_delivery` row the writer appends from the staging says `queued`, with the message id as its
request id, and an `escalation_notice` or `completion_notice` says `queued` too. What the transport came to is the daemon's to write, in
the transaction of its settle of the message: a message the host took (dispatched) appends `report_delivery` `accepted` (a report) or
`message_delivery` `accepted` (any other flow message, a step wake included, with its `wake_id`), and a message held at a cap appends
`refused` with the hold as its reason: `attempt_cap` or `busy_cap` held by the settle, and `busy_cap` held by the busy deferral, which
holds a message to a recipient that stayed busy before any transport call, in the transaction of that deferral. Each row is written once
for its report (or message) and result, in the run's `flow_events` from `1 << 40`; a daemon that dies before its settle commits writes neither the settle nor the row
(the send is unknown and the channel does not repeat it), one that dies after it leaves both, and neither a second daemon nor a resume of
the run writes a second row. The daemon may settle the message before the writer has appended its `queued` row, so a `queued` row can
follow the `accepted` one; a `queued` row never supersedes an `accepted` row before it, and the report reads `accepted` (not standing) for
`crw flow reports`, a replay and the pump. A staging the channel refuses (nobody above the project, a hierarchy that moved) writes no row: the report
stands, with the refusal as its reason, and a replay stages it again. A decision only Jun can make is not staged for a legacy lead (a
lead does not decide for Jun): it stands for the store seat's pump. A notice the channel has no kind for - the plan summary notice, and an
escalation or completion of a run whose plan names no project - goes through `crw manage send-parent` as described below.

## Escalations and the completion digest

The run tells the orchestrator thread that started it about each escalation row it wrote, once, with `crw manage send-parent --thread
<orchestrator thread> --logical-id esc-<escalation id> --queue`. A run cannot prove that a send wakes a thread, so it does not look at the
thread first and does not send: the notice goes into the parent queue and the pump delivers it (steered into an active turn, or sent once
the oldest notice has waited `max_queue_seconds`). A hook that can prove the wake-up sends directly. The run records the notice as an
`escalation_notice` row (the escalation id, the result, the request id), and a notice that got no answer is sent again, under the same
logical id, by the next reconcile. The completion digest (the run, its workflow, its status and a digest of its completed calls) goes the
same way when the run finishes, as a `completion_notice` row; a completion that row names (the same run, status and digest of calls, as when a
resume changes nothing) is not sent again, because the queue path of `send-parent` would write a notice the pump already flushed anew.

The manager hears the same escalations from the pump: a source reads the escalation rows of the relay store, read-only, and raises the
urgent kind `run_escalation`. The first read only notes where the rows end, so the history of a store is not sent. A run that started,
did not finish (the newest process of the run: the finish of an earlier process of a resumed run does not count) and has written no row of its own for `run_dead_seconds` (default 1800; rows a command wrote into it do not count) is the
urgent kind `run_dead`; it is told again only when it was resumed and stopped again. An agent turn writes no row of the run while it is in
flight, so a run with an open agent call (a call that began and has no terminal row) is judged by the longer of `run_dead_seconds` and six
hours.

The run's escalations go to the run's actor. For the project run that is the orchestrator thread. A child's issue-lane run is resumed with
the child task as its actor, so its escalations go to the child's thread; the orchestrator learns of that child's trouble from the receipt
and from `crw flow apply` (a refusal of integrity is also an `escalation` row of the child's run), and the manager hears it from the pump
whichever thread the run told.

## The plan summary

A run that was started with `crw flow run --summary-document D` records the document in its journal (a `summary_document` row). It keeps
the plan summary of its plan in that document, in the order of work below, which it repeats when it starts (a resume too) and after every
committed batch, and which `crw flow apply` repeats every time it runs on an event the relay holds, whatever it came to (a verdict recorded, a
refusal, or a wait that ended first):

1. If the newest entry of the plan's stream for the document is not up to date (`SummaryStatus`, judged by the subject digest of the
   relay: the stage, the newest verdict and its revision, the findings, the acceptance and the integration of every issue), the run asks
   for an entry with `EnqueueSummary` under the orchestrator's task id. Only the project's registered parent can enqueue and claim; the
   store seat and the manager are refused `scope_role_mismatch`, so the run enqueues and never claims. The call is not tied to a change
   of stage: a batch that changes only a verdict, a criterion's ruling, a revision or the findings is another subject and another entry,
   and the same batch again is the entry the relay already holds (`Replayed`).
2. Telling the orchestrator thread is judged by rows, not by `Replayed`. For the newest entry, when it is owed (`pending`, `claimed` or
   `failed`) and no `summary_notice` row names its summary id, the run writes one (the summary id, the plan, the document, the logical id
   `summary-<summary id>`); the row is written once for a summary id, whether the entry is new or the relay answered one it held.
3. Every `summary_notice` of the stream without a `summary_notice_settled` row is delivered: when its entry is confirmed or superseded
   first, the settlement is `obsolete` and nothing is sent (the new entry has an intent of its own); otherwise
   `crw manage send-parent --thread <orchestrator thread> --logical-id summary-<summary id>` carries the `summary_owed` notice (the summary
   id, the plan, the document) and the settlement is `accepted` or `queued`. A refusal settles `refused` and the run escalates
   (`summary_notice_refused`). An outcome nobody knows, or no answer, writes no settlement, and the next reconcile sends the notice again
   under the same logical id. A run that died between any two of these steps finds its place in the rows when it is resumed, and the
   next `crw flow apply` does the same, so an owed entry is not left without a notice.

The logical id makes the notice idempotent in `crw manage`. A direct send replays the settled record of the delivery ledger. The
`--queue` path of an id that starts with `summary-` writes the notice only when the parent queue holds none of it: not pending, not in a
batch the pump pinned, not in `sent/`, not refused. The answer names the state (`state`: `pending`, `in_flight`, `sent`, `refused`) and
the run records it as the settlement. The pump leaves a summary notice that `sent/` already holds out of its batches, and leaves a
settlement file for each summary notice it flushed (`<state_dir>/summary-settled/<thread>/<logical id>.json`). The queue handling of
every other notice is unchanged.

The orchestrator thread that receives the notice claims the entry with `dag-summary-claim`, writes the document with its own Linear
connector, reads it back and completes it (an entry that was superseded meanwhile: the newest entry of `dag-summary-status` is drained).
See [dag-outbox.md](../relay/dag-outbox.md).

## Applying the receipt of an oversized issue

An issue the architect sized for a child is released as a relay child that only coordinates: it runs the built-in `issue-lane` workflow,
whose executor calls (Claude) make every commit, and whose reviewer (another model family) rules on the head.
Its receipt reaches the orchestrator thread as a relay delivery, a model turn. In that turn:

```
crw flow apply --event E --turn T [--run RUN] [--base COMMIT] [--wait 10m] [--override REASON] [--manifest-request FILE]
```

`--turn` is the orchestrator's own turn id. Apply reads the relay event (the head, the relationship, the child task, the criteria set it
is judged under), finds the child's newest `issue-lane` run (`--run` names it; a run named that way must be an `issue-lane` run of the event's child task,
and of the relationship's plan when both name one, or the answer is `run_mismatch`: another child's journal that holds the same head and
criteria digest is no proof of this receipt), and waits, reading the journal of that run every two seconds, for a completed `crw.review`
row that ruled `verdict` on the head of the event. A run that returned for good without such a row (its newest run row is a finish with
status `completed`, it holds no `crw.hold` since its last start, it was not resumed after, and a completed `crw.commit` row names the head of the event as its own commit: the rounds of the lane ended in
`verification_failed`, or it escalated) will never write one, so the wait ends at once with the refusal `run_ended_unverified`; it is
written as an `apply_refused` row and an `escalation` row (class `integrity`) of the run, which the manager hears as a `run_escalation`,
and nothing is claimed. A held run waits for `crw flow resume`, and a head the run did not make (a commit on top of its own, the head of a later run of the child) is waited for as before. It then checks, in this order:

1. the criteria digest the reviewer saw is the digest of the relationship's criteria set (`criteria_digest_mismatch`);
2. every commit of `base..head` was made by an executor call this run recorded: a completed `crw.commit` row names the commit and the
   call, and the call is a completed call of the executor family (`author_unattributed`). The base is `--base`, else the event's, else
   the commit the run's first executor worktree started from. The refusal is escalated: an `escalation` row (class `integrity`) is
   written into the run, and the manager hears it as a `run_escalation`;
3. the reviewer's model family differs from the family of every author pair the run observed (`independence_refused`; a pair that was
   never read back proves nothing, `independence_unproven`). The coordinator's own pair is no author pair: a child on a SOL pair that
   coordinates Claude executors keeps a GPT reviewer independent. `--override` with a reason lets the application go on over this
   check only, and the reason is recorded in an `apply_override` row and in the verdict's reason; a criteria digest that differs and an
   unattributed commit never pass.

With the checks done apply writes the verdict file and runs `crw relay receive` (claim, acknowledgement proved from the orchestrator's own
turn, verdict) once. A `needs_changes` verdict carries the `dag-correct --prepare` instruction line (with the dispatch request id) as the
restoration block of one finding; `--manifest-request` is the document that command takes. After the relay recorded the verdict an
`apply_applied` row is written into the child's run. A wait that ends before the verdict is journaled writes no row of the application
and answers `next_turn`: apply it again in a later turn. Every time apply runs on an event the relay holds, whichever of these it comes to,
it then brings the plan summary up to date (the section above); a failure of that is told in `summary_error` and changes neither the state
nor the exit status. The exit status is 0 for an applied verdict,
1 for a refusal, 3 for `next_turn` and 2 for a command line or a failure of the store or the relay.

A commit the child thread makes itself is refused by the second check. An issue that is not oversized takes the executor path of the
project lane, and a child of a relationship outside a flow run is verified by the relay's own ack, verdict and premerge chain.

## Milestone reports and project-level reports

`crw.reportUp` closes a milestone: the report goes to the initiative lead. Its body names the integrated range and the evidence of the
close (the final gate, the review of reviewer-final and the audit of the reviews, CRW-1206; a piece the close did not make is `none`).
The project's orchestrator lifts a project-level blocker or a decision request the initiative decides:

```
crw flow report-up --kind project_blocked|project_decision_request|jun_decision_request --run RUN --evidence FILE [--subject TEXT] [--milestone M]
```

What an agent node cannot do or decide is not reported by the node: its RECEIPT is classified by the run, the run escalates, the
escalation reaches the orchestrator thread, and the orchestrator reports up.

A run meant to start the nodes of a plan (a run of `project-lane`, `issue-lane` or `night-batch`, or a run whose arguments name a `plan`)
that finished `completed` and started no node is `blocked` in `crw flow show` and `ps` (no `crw.release` released a node, no child was
released, nothing landed, and items of its joins threw: [script.md](script.md)). When it finishes so, the run itself reports
`project_blocked` to the initiative lead, through the same writer, recipient and delivery as `crw flow report-up`: the subject is `no node
started` and the body names each item that failed with its error. The report id comes from the run alone, so the run reports it once: a
resume that ends the same way finds the report row and makes no other, and a delivery is asked again only as for any report. A run whose
nodes the scheduler held back for capacity, a run where a node started, and a script run that names no plan and is no lane workflow (a
judge panel, a scout) make no such report.

**The row comes first.** A report is a `report` row in the `flow_events` of the run (the report id, the kind, the run, the milestone, the
subject, the digest of the body and the path of the body artifact, the recipient and the logical id of the delivery), written before any
delivery. The first report of a milestone is delivered under `<run>:<milestone>:report`. The milestone closed again over a tip that moved
on is a new report (its id comes from the tip) with another body, and the delivery layer refuses other text under a logical id it holds:
the revised report is delivered under `<run>:<milestone>:report-<head of its report id>`, the id its row records, and a replay of either
report keeps the id its row records. The delivery follows as a `report_delivery` row (`accepted`, `queued`, `refused` or `unknown`, the request id, the observation
of the thread and the reason). A report whose row exists is not delivered again once a delivery row says `accepted`, `queued` or
`refused`; an `unknown` delivery is asked again under the same logical id.

**The recipient** is found when the report is made, by the supervisor package's linkage walk, read-only and with no new row, in the
order [the supervisor channel](../relay/supervisor-channel.md#the-level-above-a-project) uses for a project: the owner of the management
seat directly above the project's live parent (`recipient_state` `management`: the management session bound the project owner, see
[linkage](../relay/linkage.md#management-seats)), else the live supervisor of the initiative above it (`initiative`, the legacy lead), else
the single store seat (`store`, with its thread). A handover between two reports sends the second to the new owner. A report to the
management session or to a lead is delivered; a report the store seat holds is not sent through the bridge, and when nobody stands above
the project, when the management seat above it has no live owner, or when the linkage cannot be read or is contested, the report stays
standing: it has a row and no delivery, and `crw flow reports --unread` and the pump (below) show it. A `jun_decision_request` goes to the
same management session when one stands above the project, and is delivered; otherwise it is addressed to the store seat, as it always
was, and is never delivered through the bridge (a lead does not decide for Jun). A manager that registered its management seat (`crw
manage supervisor register`) laid its store seat down, and the store seat is then read through that management seat
([linkage](../relay/linkage.md#management-seats)), so the report still names the manager's thread. The notice of a report to a
management session names `crw flow reports --project P --unread` for its project, so two management sessions each read their own
projects' reports and not the other's; the notice of any other report with no initiative names `crw flow reports --unread`.

**The delivery** is `crw manage send-parent --role supervisor --thread <initiative lead's thread> --logical-id <run>:<milestone>:report
--report <report id> --initiative <initiative>`. The role is an option of `crw manage`, not of the relay; the default role (parent) is not
changed. The settings of the send are the authorization recorded for the supervision binding of that thread in the relay (the model and
the reasoning effort), read when the notice is sent and again when the pump flushes it; the `crw manage` configuration has only the
management and parent blocks. When the report was written in a relay the operator selected (`crw flow report-up --state S --socket P`, or
a run resumed in that store), the notice names it (`send-parent --state S --socket P`): the binding is read there, the queued notice
remembers the pair, and the pump reads the same settings and has `crw flow report-delivered` open the same store. Before anything is sent the command asks the thread for its active turn, with the probe the pump uses:

- idle: the notice is sent with the role `supervisor` and the binding's settings;
- active (busy), or any other observation, `notLoaded` first among them: nothing is sent, no delivery record is opened (so the retry
  budget of the delivery core is untouched, the core answering `unknown` for a thread it cannot confirm idle), and the notice is written
  into the supervisor queue, `<state_dir>/supervisor-queue/<thread>/<logical id>.json`, with its role, the task of the binding the
  settings come from, the report and the initiative. The answer is class `queued` with the observation, the delivery row says `queued`,
  and the report stays standing;
- a binding that records no authorization: nothing is sent and the notice is queued with the reason `settings_unavailable`;
- a thread that unloads between the probe and the send: the bridge refuses (`unverified_pair_for_unloaded_thread`, the last defence; the
  supervisor is a record role, so its pair is not pinned). The delivery row says `refused` with that reason, and the notice is queued with
  the ordinal of its next attempt.

**The pump flushes the supervisor queue** before it delivers the management batch, with the gating of the parent queue: an active thread
is steered, an idle thread is sent the notice after `max_queue_seconds`, anything else leaves the notice where it is and raises one urgent
`report_undelivered` event for the report (the report, the initiative, the thread, the observation, how long it waited), recorded in the
pump state so it is raised once. When the thread is loaded the same flush delivers with the role `supervisor` and the settings of the
binding as they are then, moves the notice to `sent/` and calls `crw flow report-delivered --report ID --result accepted --request-id R`;
the flow command is the one writer of `flow_events`, the pump reads the relay store and writes none of it, and a call that fails is made
again next round. The obligation to make that call is saved in the pump state before the notice leaves the queue, so a pump that dies in
between restarts with the notice still pending and the delivery record answers it (nothing is sent twice). A refusal of the unloaded
thread whose answer was lost, or whose ordinal the pump never kept, is read again from the receipt the delivery core reconciles for the
same id and advances the ordinal. The unload race is not a refusal of the notice: it is not counted against the refusal limit, and the next attempt goes
out under `<logical id>-l<n>`, so the settled refusal of the earlier id is not replayed. Any other refusal counts; the third moves the
notice to `refused/` and tells the flow rows. The parent queue's flush is not changed.

**Reading.** A management session reads the reports of each project it holds with `crw flow reports --project P --unread`. The
initiative lead reads its reports when a turn starts, `crw flow reports --initiative I --unread`, and acknowledges each
in that turn, `crw flow report-ack ID --turn T`, which appends a `report_read` row. A report nobody read for `low_priority_seconds` is
raised to the manager as the low kind `report_unread` (the report, its age, the last delivery result). A decision for Jun is the urgent
kind `jun_decision`; the manager records the question and Jun's answer with `crw manage decisions`. The relay's decision record of a
question raised from a project a management session bound names that session's seat, and only the seat's current owner records the
answer ([linkage](../relay/linkage.md#management-seats), CRW-1371).

## Rows

All of these are rows of `flow_events` (`kind` is the first column of the table, the rest are members of the payload).

| kind | members |
| --- | --- |
| `report` | `report_id`, `report_kind`, `run`, `milestone`, `subject`, `plan`, `body_digest`, `body_path`, `logical_id`, `recipient_state` (`management`, `initiative`, `store`, `unregistered_scope`, `unreadable`), `recipient_thread`, `recipient_initiative`, `recipient_project`, `recipient_reason`, `recipient_detail` |
| `report_delivery` | `report_id`, `result`, `request_id`, `observation`, `reason`, `logical_id`; written by the daemon's settle: `report_id`, `result` (`accepted`, `refused`), `request_id`, `message_id`, `turn_id`, `kind`, `subject`, `observation` (the message's state), `reason` (the hold) |
| `message_delivery` | `message_id`, `kind`, `subject`, `result` (`accepted`, `refused`), `request_id`, `turn_id`, `observation`, `reason`, and `wake_id` for a step wake (the daemon's settle) |
| `report_read` | `report_id`, `turn` |
| `summary_document` | `document` |
| `summary_notice` | `summary_id`, `plan`, `document`, `logical_id`, `run` |
| `summary_notice_settled` | `summary_id`, `plan`, `document`, `result` (`accepted`, `queued`, `obsolete`, `refused`), `request_id`, `logical_id`, `run` |
| `escalation_notice` | `escalation_id`, `result`, `request_id`, `thread` |
| `completion_notice` | `notice_key`, `status`, `result`, `request_id` |
| `apply_applied`, `apply_refused`, `apply_override` | `event`, `head`, `verdict` or `reason`, and what the check found |

A run process appends its rows to its own journal, so they carry the journal's sequence numbers. A command (`crw flow apply`, `report-up`,
`report-ack`, `report-delivered`) is another process, possibly beside a run that is alive: its rows are numbered from `1 << 40`
(`engine.ForeignSeqBase`), a range the run's journal never reaches, and a journal that is opened again counts after the largest number it
wrote itself. A row of a kind that is written once for a key (a summary id, an escalation id, a report id) is checked in the transaction
that writes it, across runs.

## Commands

```
crw flow apply --event E --turn T [--run RUN] [--base COMMIT] [--wait 10m] [--override REASON] [--manifest-request FILE] [--repo DIR] [--state DIR] [--socket PATH]
crw flow report-up --kind K --run RUN --evidence FILE [--subject TEXT] [--milestone M] [--state DIR] [--socket PATH]
crw flow reports [--initiative I] [--unread] [--json] [--project P] [--state DIR] [--socket PATH]
crw flow report-ack REPORT --turn T [--state DIR] [--socket PATH]
crw flow report-delivered --report ID --result accepted|queued|refused|unknown --request-id R [--observation O] [--state DIR] [--socket PATH]
crw manage send-parent --role supervisor --thread T --message-file F --logical-id ID [--report ID] [--initiative I] [--state DIR] [--socket PATH] [--queue]
```
