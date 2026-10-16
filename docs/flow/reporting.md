# What goes up from a flow run

Here the parent (repository session, relay role `supervisor`) is a layer of [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels).

A flow run (a project run) works for the thread that started it: the child
(project session, relay role `parent`, [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels)), which works for its parent (repository session, relay role `supervisor`). This page says what travels upward, to whom, by which command, and which row of the relay
store's `flow_events` records it. The rows are described in [journal.md](journal.md); the relay commands the paths end in are in the
[relay README](../relay/README.md). The reports, the escalations and the completion digest go through the relay's supervisor channel as direct
messages ([supervisor-channel.md](../relay/supervisor-channel.md#direct-messages), CRW-1380); the plan summary notice goes through `crw manage send-parent`.

| What | From | To | By | Recorded as | Pump kind |
| --- | --- | --- | --- | --- | --- |
| an escalation of the run (`dispatch_unknown`, `tiers_exhausted`, `blocked_scope`, `size_split`, with ask `bundle_split_request` for a node sized `bundle_split`, `restore_snapshot_mismatch`, a configuration fault, the run-level hold `plan_superseded` with the pinned and head revisions and the node, CRW-1291) | the run | the level above the run's project (the parent's management seat, else a supervisor registered at the initiative's scope, the legacy configuration, else a live store seat); a run whose plan names no project: the child's thread | the supervisor channel (`flow_escalation`); else `crw manage send-parent` | `escalation`, `escalation_notice`, `message_delivery` | `run_escalation` |
| the completion digest (`state=`, `reason=`, `lanes=` of the milestone state the run came to; none for a run the operator paused) | the run | as an escalation | the supervisor channel (`flow_completion`); else `crw manage send-parent` | `completion_notice`, `message_delivery` | |
| a run that stopped writing | nobody | the pump's management thread | the pump | | `run_dead` |
| the plan summary that is owed | the run | the child's thread | `crw manage send-parent` | `summary_document`, `summary_notice`, `summary_notice_settled` | |
| a milestone report | the run | the parent above the project (else a supervisor registered at the initiative's scope, the legacy configuration, else a live store seat) | the writer of `crw flow report-up` through the supervisor channel (`flow_milestone_report`) | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a run that started no node (`project_blocked`, CRW-1320) | the run | as a milestone report | the writer of `crw flow report-up` through the supervisor channel (`flow_project_blocked`) | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a project-level blocker or decision request (the request for a bundle split of an issue whose node escalated `size_split` with ask `bundle_split_request` included) | the child | as a milestone report | `crw flow report-up` through the supervisor channel (`flow_project_blocked`, `flow_project_decision_request`) | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a decision only Jun can make | the child | the parent above the project (else the store seat, then the parent that holds it) | `crw flow report-up` through the supervisor channel (`flow_jun_decision_request`); under a supervisor registered at the initiative's scope it stands | `report`, `report_delivery` | `jun_decision`, only for one the channel did not take |
| a step that waits for the owner (a session run, CRW-1348) | the run | the run's owner | the supervisor channel (`session_step_waiting`, `run:<id>`) | `session_wake`, `message_delivery` | |

## Through the supervisor channel

The run (and `crw flow report-up`) hands a report, an escalation and the completion digest to `report.RelaySender`, which stages one
direct message on the relay's supervisor channel: the relationship is `project:<the project of the run's plan>`, the kind is the
report's (`flow_milestone_report`, `flow_project_blocked`, `flow_project_decision_request`, `flow_jun_decision_request`,
`flow_merge_request`) or the notice's
(`flow_escalation`, `flow_completion`), and the subject is `<run>/<report id>` or `<run>/<logical id>`, so the message id is
deterministic and the same report staged again is the same message. The channel addresses it to the level above the project - the
owner of the parent's management seat, else a supervisor registered at the initiative's scope (the legacy initiative supervision), else the single store seat, which is woken like any other recipient (a store seat laid down for a management seat answers only for a
decision only Jun can make, below) - and the relay daemon
delivers it: a new turn on an idle thread, a resume and a turn on an unloaded one, after the turn on a busy one. The report row and the
message carry one resolution of the project: the row records the project the report is staged through (`recipient_project`, the
store seat's decision for Jun included), so the listing the message's evidence names, `crw flow reports --project <project> --unread`,
lists it. A store seat's report of a run whose plan names no project has no project to be staged through and does not go through `crw
manage send-parent` either: it stands for the store seat's pump, and `crw flow reports --unread` lists it.

Staging writes only `queued`: the `report_delivery` row the writer appends from the staging says `queued`, with the message id as its
request id, and an `escalation_notice` or `completion_notice` says `queued` too. What the transport came to is the daemon's to write, in
the transaction of its settle of the message: a message the host took (dispatched) appends `report_delivery` `accepted` (a report) or
`message_delivery` `accepted` (any other flow message, a step wake included, with its `wake_id`), and a message held at a cap appends
`refused` with the hold as its reason: `attempt_cap` or `busy_cap` held by the settle, and `busy_cap` held by the busy deferral, which
holds a message to a recipient that stayed busy before any transport call, in the transaction of that deferral. Each row is written once
for its report (or message) and result, in the run's `flow_events` from `1 << 40`; a daemon that dies before its settle commits writes neither the settle nor the row
(the send is unknown and the channel does not repeat it), one that dies after it leaves both, and neither a second daemon nor a resume of
the run writes a second row. The daemon may settle the message, hold it at a cap, or find its send's outcome unknown before the writer has
appended its `queued` row, so a `queued` row can follow the `accepted`, `refused` or `unknown` one; a `queued` row never supersedes a
settled row (`accepted`, `refused` or `unknown`) before it, and only a later settled row does (the readback's `accepted` after `unknown`).
The report reads what the settle wrote (`accepted`: not standing; `refused`, `unknown`: standing) for `crw flow reports`, the writer's
answer and its replay, and the pump, which all read the rows by `report.SupersedesDelivery`: a decision for Jun whose send is `unknown`
stays the pump's urgent `jun_decision` whatever `queued` row follows it. A staging the channel refuses (nobody above the project, a hierarchy that moved) writes no row: the report
stands, with the refusal as its reason, and a replay stages it again. A notice the channel has no kind for - the plan summary notice, and an
escalation or completion of a run whose plan names no project - goes through `crw manage send-parent` as described below.

A send whose outcome nobody heard back (the transport started and no answer came: the settle, or the recovery of a send stranded by a
daemon that died before its settle, moves the message to `held_uncertain`) is not repeated, and its delivery is recorded as such: the
same transaction appends `report_delivery` (or `message_delivery`) `unknown`, with the message's state as its observation, so the report
does not read `queued` while nothing will move it. The recipient's verified readback of that send (`supervisor-read`, whose token is
found in a turn that began at or after the send) settles it and appends `accepted`. A replay of the report (a resume of the run, the
same `report-up` again) finds the message as it may have gone: the channel leaves it as it is, and the writer stages nothing and writes
no `queued` row over the `unknown` one. A replay that read the `unknown` row and finds the message read (the readback settled it in
between) stages nothing and writes nothing either, and answers `accepted`, as the rows read. A notice of the run (an escalation, the
completion digest) has no delivery row the run reads: a reconcile that finds its message `held_uncertain` or read (the run process died
after the staging and before the notice row) is answered `queued`, as the staged message it is, so the run writes its `escalation_notice`
or `completion_notice` row once and does not stage it again (CRW-1406). Every row the daemon writes names the message's subject (`<run>/<report id or logical id>`, in
the `subject` column and as the payload's `_label`, as a command's row has it) and the node of the run's row it is about: the
escalation's node for an escalation, none for a report, a completion digest or a step wake.

**A decision only Jun can make under a supervisor at the initiative's scope** (the legacy configuration: a lead supervises the project
and no management seat stands above it) is a case of its own. The report row is written with the store seat as its recipient
(`recipient_state` `store`: a supervisor at the initiative's scope does not decide for Jun), but the channel resolves `project:<key>` to
that supervisor, so `report.RelaySender` stages nothing (`lead_does_not_decide_for_jun`): the report has no delivery row and stands. It is
read where every standing report is read, `crw flow reports --unread`, and the pump raises it as the urgent `jun_decision` to its
management thread, the one decision for Jun the pump still tells. A parent that binds the project's child itself (its management seat
above the project) ends the case: the next decision goes through the channel to that parent.

## Escalations and the completion digest

The run tells the level above its project about each escalation row it wrote, once: `report.RelaySender` stages it on the supervisor
channel as a `flow_escalation` direct message through `project:<the project of the run's plan>`, subject `<run>/esc-<escalation id>`,
and the relay daemon delivers it to the parent's management seat above the project (else a supervisor at the initiative's scope, else
the store seat) as it delivers a report. The notice names the run, the node, the reason and the class, then the detail, and, for an
escalation that carries an ask, ends with `The run waits for your decision (<ask>).`. An escalation that asks `bundle_split_request`
(a node the architect sized `bundle_split`, closed by the escalation) ends, in place of that line, with the request: a bundle split of
the node's issue (named from the run's plan, else the node is named by its id) is the parent's to make, and the child asks the parent
for it with `crw flow report-up --kind project_decision_request --run RUN --evidence FILE`, a project-level decision request
([below](#milestone-reports-and-project-level-reports)). The run records the notice as an `escalation_notice` row (the escalation id, the result, the
request id, and the thread the notice was addressed to: the recipient the channel staged it for), and a notice the channel did not stage
is staged again, under the same subject, by the next reconcile. A run whose plan names no project has no relationship to stage under:
its notice goes to the child's thread that started it with `crw manage send-parent --thread <child thread> --logical-id
esc-<escalation id> --queue` (a run cannot prove that a send wakes a thread, so the notice goes into the parent queue and the pump
delivers it), and the row names that thread. The completion digest (the run, its workflow, its status and a digest of its completed
calls) goes the same way when the run finishes (`flow_completion`), as a `completion_notice` row; a completion that row names (the same
run, status and digest of calls, as when a resume changes nothing) is not sent again. When the run's `finished` row carries an outcome
(the milestone state the script returned, [script.md](script.md)), the notice says that state instead of the engine status:
`[crw flow] run finished run=R workflow=W state=<state>[ reason=<reason>][ escalation=<id>][ lanes=<node>:<state>/<reason>,...] digest=D`,
with the lanes that did not land, so an escalated, incomplete or held milestone is never told `status=completed`. The state told is
read in this order: a `milestone` member that is an object without an `id` and with a non-empty string `state` is the wrapped result
(a night batch returns one), and its state wins (the value's own state is kept as `run_state`); a `milestone` member that
carries an `id` is a milestone, not a wrapper, and is not read; a `milestone` member without a non-empty string `state` is not read
either; otherwise the top-level string `state` of the completed script is the outcome state. A value whose own `state` is not a
non-empty string has no outcome. The whole outcome
(every member by its name) is part of the digest too, so a resume that ends with an outcome that differs in any member (an escalation
id, the source, the milestone) is told again. A run with no outcome keeps the
text `[crw flow] run finished run=R workflow=W status=<status> digest=D` and its digest. A run the operator paused (`crw flow pause`)
ends with a `run/paused` row and no `finished` row: nothing is told finished, the summary and the escalations are reconciled once more,
and the completion is told when a resume finishes the run.

The pump's management thread hears the same escalations from the pump: a source reads the escalation rows of the relay store,
read-only, and raises the urgent kind `run_escalation`. The first read only notes where the rows end, so the history of a store is not sent. A run that started,
did not end (no `finished` row and no `run/paused` row of the newest process of the run: the end of an earlier process of a resumed run does not count, and a run the operator paused is not dead) and has written no row of its own for `run_dead_seconds` (default 1800; rows a command wrote into it do not count) is the
urgent kind `run_dead`; it is told again only when it was resumed and stopped again. An agent turn writes no row of the run while it is in
flight, so a run with an open agent call (a call that began and has no terminal row) is judged by the longer of `run_dead_seconds` and six
hours.

Only a run whose plan names no project tells its actor's thread, the child's thread, with `crw manage send-parent`, and the pump's
management thread hears it whichever thread the run told.

## The plan summary

A run that was started with `crw flow run --summary-document D` records the document in its journal (a `summary_document` row). It keeps
the plan summary of its plan in that document, in the order of work below, which it repeats when it starts (a resume too) and after every
committed batch:

1. If the newest entry of the plan's stream for the document is not up to date (`SummaryStatus`, judged by the subject digest of the
   relay: the stage, the newest verdict and its revision, the findings, the acceptance and the integration of every issue), the run asks
   for an entry with `EnqueueSummary` under the child's task id. Only the project's registered owner (relay role `parent`, the child) can enqueue and claim; the
   store seat and the parent are refused `scope_role_mismatch`, so the run enqueues and never claims. The call is not tied to a change
   of stage: a batch that changes only a verdict, a criterion's ruling, a revision or the findings is another subject and another entry,
   and the same batch again is the entry the relay already holds (`Replayed`).
2. Telling the child's thread is judged by rows, not by `Replayed`. For the newest entry, when it is owed (`pending`, `claimed` or
   `failed`) and no `summary_notice` row names its summary id, the run writes one (the summary id, the plan, the document, the logical id
   `summary-<summary id>`, and `text`, the notice in this build's words); the row is written once for a summary id, whether the entry is
   new or the relay answered one it held. The run reads the rows again after the write, so an intent another writer stored first is the
   one whose text goes. An intent that only this second read finds, for an entry newer than the states the run read, is neither sent nor
   settled: the next reconcile reads both again and sends it.
3. Every `summary_notice` of the stream without a `summary_notice_settled` row is delivered: when its entry is confirmed or superseded
   first, the settlement is `obsolete` and nothing is sent (the new entry has an intent of its own); otherwise
   `crw manage send-parent --thread <child thread> --logical-id summary-<summary id>` carries the `summary_owed` notice (the summary
   id, the plan, the document) and the settlement is `accepted` or `queued`. Every send carries the `text` of the earliest
   `summary_notice` row of the summary id, never words composed again: the manage outbox keys `summary-<summary id>` to the sha256 of
   the first text it carried, so a retry in other words would be refused locally. A row without `text` (the rows of every build before
   CRW-1548) is sent in the words those builds composed. Two writers that both wrote the intent send the earliest row's text; only a
   second writer that sends before the earliest row is on record can still send other words. A refusal settles `refused` and the run
   escalates (`summary_notice_refused`). An outcome nobody knows, or no answer, writes no settlement, and the next reconcile sends the notice again
   under the same logical id. A run that died between any two of these steps finds its place in the rows when it is resumed, so an owed
   entry is not left without a notice.

The logical id makes the notice idempotent in `crw manage`. A direct send replays the settled record of the delivery ledger. A retry id of
a summary notice that was saved before its send and never sent (`get_operation` answers that the bridge never recorded it) is sent under that same
id when the delivery resumes, also once the retry cap is spent: nothing is keyed to the id at the bridge, so no attempt is duplicated, and no
retry is spent; any other logical id in that state settles refused. The
`--queue` path of an id that starts with `summary-` writes the notice only when the parent queue holds none of it: not pending, not in a
batch the pump pinned, not in `sent/`, not refused. The answer names the state (`state`: `pending`, `in_flight`, `sent`, `refused`) and
the run records it as the settlement. The pump leaves a summary notice that `sent/` already holds out of its batches, and leaves a
settlement file for each summary notice it flushed (`<state_dir>/summary-settled/<thread>/<logical id>.json`). The queue handling of
every other notice is unchanged.

The child's thread that receives the notice claims the entry with `dag-summary-claim`, writes the document with its own Linear
connector, reads it back and completes it (an entry that was superseded meanwhile: the newest entry of `dag-summary-status` is drained).
See [dag-outbox.md](../relay/dag-outbox.md).

## Milestone reports and project-level reports

`crw.reportUp` closes a milestone: the report goes to the parent. Its body names the integrated range and the evidence of the
close (the final gate, the crosscheck's verdict and the reflection's alignment, CRW-1473, then the review of reviewer-final, which a close that runs the crosscheck does not make, and the audit of the reviews, CRW-1206; a piece the close did not make is `none`).
The project's orchestrator lifts a project-level blocker or a decision request the initiative decides:

```
crw flow report-up --kind project_blocked|project_decision_request|jun_decision_request --run RUN --evidence FILE [--subject TEXT] [--milestone M] [--question ESC]
```

What an agent node cannot do or decide is not reported by the node: its RECEIPT is classified by the run, the run escalates, the
escalation reaches the child's thread, and the child reports up.

A `project_decision_request` that lifts a question the run asked names it with `--question <escalation id>` (CRW-1574). The command
reads the run's questions as `crw flow show` lists them and refuses, with exit 1 and nothing written, a question the run does not wait
on: an id the run never asked, a revision a later ask of the same node and ask replaced, a question a decision already answered, and a
request to send up (`bundle_split_request`). The report row then carries `question`, `revision` (its revision among the asks of its node
and ask) and `node` (`""` for a milestone's ask), so the parent's decision record whose source is that report binds the question;
the three members take no part in the report id. `--question` with any other kind is a usage error.

**A merge request** is a report of its own kind, `merge_request`, not a milestone report: a bundle that lands its milestone through one
pull request ([script.md](script.md), "The bundle's pull request") asks the parent to merge it with the `crw.mergeRequest` op. The op
judges first, and a request that fails the judgment is a result of the op with a row of its own and no report (`pr_not_open`,
`crosscheck_missing`, `head_moved`, `crosscheck_not_accepted`, `checks_failed`, `checks_pending`, `checks_none`,
`comments_unresolved`). A request that passes goes to the reporter, which writes the report row (the subject is `merge request
OWNER/NAME#N`) and delivers it as the other reports of the run are delivered (below): to the recipient of a milestone report, through
the supervisor channel under the kind `flow_merge_request` (a notice of the project, not a decision). Its report id comes from the run,
the milestone, the pull request (its name folded to lower case) and its head, so a replay finds its row, and the report of the milestone's
close keeps its own id and its own delivery. The delivery goes under `<run>:merge_request-<head of the report id>:report`. The body
names the pull request, its head and base branch, the state of the head's checks, the crosscheck at the head (the verdict with its round
and engine) and the count of unresolved review comments, or that the review bots are not connected; it holds no path, host name or id
besides the pull request and the SHAs. The request goes once per head: a second call for the same pull request at the same head
answers `repeated`, sends nothing and writes no second report, a refused request never blocks a later one, and a head that moved needs
a new crosscheck and is another request. When the channel could not stage the report (the hierarchy above the project drifted, say),
the op fails without recording the request, so a retry stages it again under the same report id.

The built-in `project-lane` does not call `crw.mergeRequest`, and the lane that `crw flow` builds is wired without a pull request
reader, so the op is a Go error there ("no pull request reader is wired"): only a script whose lane has a reader sends this report. The
run never merges the pull request; the dev queue does.

A run meant to start the nodes of a plan (a run of `project-lane` or `night-batch`, an old run of a `child` session's lane, or a run whose
arguments name a `plan`) that finished `completed` and started no node is `blocked` in `crw flow show` and `ps` (no `crw.release` released a node, no child was
released, nothing landed, and items of its joins threw: [script.md](script.md)). When it finishes so, the run itself reports
`project_blocked` to the parent, through the same writer, recipient and delivery as `crw flow report-up`: the subject is `no node
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
`refused`; an `unknown` delivery is asked again under the same logical id (through the supervisor channel, where `unknown` is the
daemon's record of a send whose outcome is not known, asking again stages nothing and writes nothing).

**The recipient** is found when the report is made, by the supervisor package's linkage walk, read-only and with no new row, in the
order [the supervisor channel](../relay/supervisor-channel.md#the-level-above-a-project) uses for a project: the owner of the management
seat directly above the project's live parent (`recipient_state` `management`: the parent that bound the child, see
[linkage](../relay/linkage.md#management-seats)), else the live supervisor of the initiative above it (`initiative`, the legacy initiative supervision), else
the single store seat (`store`, with its thread). A handover between two reports sends the second to the new owner. A report to the
parent or to an initiative-scoped supervisor is delivered; a report the store seat holds is not sent through the bridge, and when nobody stands above
the project, when the management seat above it has no live owner, or when the linkage cannot be read or is contested, the report stays
standing: it has a row and no delivery, and `crw flow reports --unread` and the pump (below) show it. A `jun_decision_request` goes to the
same parent when one stands above the project, and is delivered; otherwise it is addressed to the store seat, as it always
was, and is never delivered through the bridge (an initiative-scoped supervisor does not decide for Jun). A parent that registered its management seat (`crw
manage supervisor register`) laid its store seat down, and the store seat is then read through that management seat
([linkage](../relay/linkage.md#management-seats)): a `jun_decision_request` still names the parent's thread and is staged to it
(`flow_jun_decision_request` is user-grade), while any other report of a project no management edge stands above (a child that bound
itself) finds nobody above (`recipient_state` `unregistered_scope`) and stays standing until a management edge stands above the project or a
live store seat stands (CRW-1486, [grades](../relay/supervisor-channel.md#the-level-above-a-project)). The notice of a report to a
parent names `crw flow reports --project P --unread` for its project, so two parents each read their own
projects' reports and not the other's; the notice of any other report with no initiative names `crw flow reports --unread`.

**The delivery through `crw manage send-parent`** is the path of `report.ManageSender`, which `report.RelaySender` keeps as its fallback
for a report the channel has no kind, run or project for (the 1.0 path stages every report through the channel, above, and queues
nothing in the supervisor queue). It is `crw manage send-parent --role supervisor --thread <the initiative-scoped supervisor's thread> --logical-id <run>:<milestone>:report
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

**The pump on the 1.0 path.** The management batch the pump sends to `management_thread` (the `crw manage` configuration) is
superseded there by the supervisor channel: the relay daemon opens a turn in the parent above each project for its reports,
escalations, completion digests and decisions for Jun, and the pump is no part of that path. The batch and its sources stay for what
the channel does not carry: a run that stopped writing (`run_dead`), the escalations as a second reading (`run_escalation`), a report
nobody acknowledged (`report_unread`) and the fallback queues. A decision only Jun can make whose last delivery row is the channel's -
`queued` (the channel holds the message) or `accepted` (the daemon delivered it) - is not raised as `jun_decision` again; it is read as
any report is, and raised as `report_unread` when nobody acknowledged it for `low_priority_seconds`. One the channel did not take - no
delivery row (the legacy case above, a staging refused, a store seat's report of a run whose plan names no project), `refused` (a cap
held it) or `unknown` (the send's outcome is not known) - is still the urgent `jun_decision`.

**Reading.** A parent reads the reports of each project it holds with `crw flow reports --project P --unread`. An
initiative-scoped supervisor reads its reports when a turn starts, `crw flow reports --initiative I --unread`, and acknowledges each
in that turn, `crw flow report-ack ID --turn T`, which appends a `report_read` row. A report nobody read for `low_priority_seconds` is
raised to the pump's management thread as the low kind `report_unread` (the report, its age, the last delivery result). A decision for
Jun the channel did not take is the urgent kind `jun_decision` (above); the parent records the question and Jun's answer with `crw
manage decisions`. The relay's decision record of a
question raised from a project a parent bound names that parent's seat, and only the seat's current owner records the
answer ([linkage](../relay/linkage.md#management-seats), CRW-1371).

## Rows

All of these are rows of `flow_events` (`kind` is the first column of the table, the rest are members of the payload).

| kind | members |
| --- | --- |
| `report` | `report_id`, `report_kind`, `run`, `milestone`, `subject`, `plan`, `body_digest`, `body_path`, `logical_id`, `recipient_state` (`management`, `initiative`, `store`, `unregistered_scope`, `unreadable`), `recipient_thread`, `recipient_initiative`, `recipient_project`, `recipient_reason`, `recipient_detail`, and for a `project_decision_request` made with `--question` `question`, `revision`, `node` |
| `report_delivery` | `report_id`, `result`, `request_id`, `observation`, `reason`, `logical_id`; written by the daemon (its settle, the recovery of a stranded send, a verified readback): `report_id`, `result` (`accepted`, `refused`, `unknown`), `request_id`, `message_id`, `turn_id`, `kind`, `subject`, `observation` (the message's state), `reason` (the hold, `readback`); the row's `subject` column and `_label` are the message's subject |
| `message_delivery` | `message_id`, `kind`, `subject`, `result` (`accepted`, `refused`, `unknown`), `request_id`, `turn_id`, `observation`, `reason`, and `wake_id` for a step wake (written by the daemon as a `report_delivery` row is); the row's `node_id` is the escalation's node for an escalation |
| `report_read` | `report_id`, `turn` |
| `summary_document` | `document` |
| `summary_notice` | `summary_id`, `plan`, `document`, `logical_id`, `run`, `text` (the notice every send of the summary id carries; absent in the rows of the builds before CRW-1548, which are sent in those builds' words) |
| `summary_notice_settled` | `summary_id`, `plan`, `document`, `result` (`accepted`, `queued`, `obsolete`, `refused`), `request_id`, `logical_id`, `run` |
| `escalation_notice` | `escalation_id`, `result`, `request_id`, `thread` (the thread the notice was addressed to) |
| `completion_notice` | `notice_key`, `status` (the engine status), `result`, `request_id`, and with an outcome `state`, `reason` and `lanes` (`node:state/reason` of each lane that did not land) |
| `apply_applied`, `apply_refused`, `apply_override` | an older build's rows of a receipt application, read by nothing and written by no command now: `event`, `head`, `verdict` or `reason`, and what the check found |

A run process appends its rows to its own journal, so they carry the journal's sequence numbers. A command (`report-up`, `report-ack`,
`report-delivered`) is another process, possibly beside a run that is alive: its rows are numbered from `1 << 40`
(`engine.ForeignSeqBase`), a range the run's journal never reaches, and a journal that is opened again counts after the largest number it
wrote itself. A row of a kind that is written once for a key (a summary id, an escalation id, a report id) is checked in the transaction
that writes it, across runs.

## Commands

```
crw flow report-up --kind K --run RUN --evidence FILE [--subject TEXT] [--milestone M] [--question ESC] [--state DIR] [--socket PATH]
crw flow reports [--initiative I] [--unread] [--json] [--project P] [--state DIR] [--socket PATH]
crw flow report-ack REPORT --turn T [--state DIR] [--socket PATH]
crw flow report-delivered --report ID --result accepted|queued|refused|unknown --request-id R [--observation O] [--state DIR] [--socket PATH]
crw manage send-parent --role supervisor --thread T --message-file F --logical-id ID [--report ID] [--initiative I] [--state DIR] [--socket PATH] [--queue]
```
