# What goes up from a flow run

A flow run (a project run, the issue-lane of an oversized child) works for the thread that started it, the orchestrator, and the
orchestrator works for an initiative lead. This page says what travels upward, to whom, by which command, and which row of the relay
store's `flow_events` records it. The rows are described in [journal.md](journal.md); the relay commands the paths end in are in the
[relay README](../relay/README.md). The relay's own supervisor channel is a different path and carries none of this: its entrances are
the standing obligation of a relationship child and the fault notice ([supervisor-channel.md](../relay/supervisor-channel.md)).

| What | From | To | By | Recorded as | Pump kind |
| --- | --- | --- | --- | --- | --- |
| an escalation of the run (`dispatch_unknown`, `tiers_exhausted`, `blocked_scope`, `size_split`, `restore_snapshot_mismatch`, a configuration fault) | the run | the orchestrator thread | `crw manage send-parent` | `escalation`, `escalation_notice` | `run_escalation` |
| the completion digest | the run | the orchestrator thread | `crw manage send-parent` | `completion_notice` | |
| a run that stopped writing | nobody | the manager thread | the pump | | `run_dead` |
| the plan summary that is owed | the run, `crw flow apply` | the orchestrator thread | `crw manage send-parent` | `summary_document`, `summary_notice`, `summary_notice_settled` | |
| the receipt of an oversized issue | the child | the orchestrator | relay delivery, then `crw flow apply` | `apply_applied`, `apply_refused`, `apply_override` | |
| a milestone report | the run | the initiative lead | `crw flow report-up` through `send-parent --role supervisor` | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a project-level blocker or decision request | the orchestrator | the initiative lead | `crw flow report-up` | `report`, `report_delivery`, `report_read` | `report_undelivered`, `report_unread` |
| a decision only Jun can make | the orchestrator | the store seat, then the manager | `crw flow report-up` | `report` | `jun_decision` |

## Escalations and the completion digest

The run tells the orchestrator thread that started it about each escalation row it wrote, once, with `crw manage send-parent --thread
<orchestrator thread> --logical-id esc-<escalation id> --queue`. A run cannot prove that a send wakes a thread, so it does not look at the
thread first and does not send: the notice goes into the parent queue and the pump delivers it (steered into an active turn, or sent once
the oldest notice has waited `max_queue_seconds`). A hook that can prove the wake-up sends directly. The run records the notice as an
`escalation_notice` row (the escalation id, the result, the request id), and a notice that got no answer is sent again, under the same
logical id, by the next reconcile. The completion digest (the run, its workflow, its status and a digest of its completed calls) goes the
same way when the run finishes, as a `completion_notice` row.

The manager hears the same escalations from the pump: a source reads the escalation rows of the relay store, read-only, and raises the
urgent kind `run_escalation`. The first read only notes where the rows end, so the history of a store is not sent. A run that started,
did not finish and has written no row of its own for `run_dead_seconds` (default 1800; rows a command wrote into it do not count) is the
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
is judged under), finds the child's newest `issue-lane` run (`--run` names it), and waits, reading the journal of that run every two
seconds, for a completed `crw.review` row that ruled `verdict` on the head of the event. It then checks, in this order:

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

`crw.reportUp` closes a milestone: the report goes to the initiative lead. The project's orchestrator lifts a project-level blocker or a
decision request the initiative decides:

```
crw flow report-up --kind project_blocked|project_decision_request|jun_decision_request --run RUN --evidence FILE [--subject TEXT] [--milestone M]
```

What an agent node cannot do or decide is not reported by the node: its RECEIPT is classified by the run, the run escalates, the
escalation reaches the orchestrator thread, and the orchestrator reports up.

**The row comes first.** A report is a `report` row in the `flow_events` of the run (the report id, the kind, the run, the milestone, the
subject, the digest of the body and the path of the body artifact, the recipient and the logical id of the delivery), written before any
delivery. The delivery follows as a `report_delivery` row (`accepted`, `queued`, `refused` or `unknown`, the request id, the observation
of the thread and the reason). A report whose row exists is not delivered again once a delivery row says `accepted`, `queued` or
`refused`; an `unknown` delivery is asked again under the same logical id.

**The recipient** is found when the report is made, by the supervisor package's linkage walk, read-only and with no new row: the live
parent of the project, the initiative above it, the live supervisor of that initiative. A handover between two reports sends the second
to the new lead. When no initiative supervises the project, when the walk can only name the store seat (a seat is not an initiative lead
and is never sent a report through the bridge), or when the linkage cannot be read or is contested, the report stays standing: it has a
row and no delivery, and `crw flow reports --unread` and the pump (below) show it. A `jun_decision_request` is addressed to the store seat
and is never delivered through the bridge.

**The delivery** is `crw manage send-parent --role supervisor --thread <initiative lead's thread> --logical-id <run>:<milestone>:report
--report <report id> --initiative <initiative>`. The role is an option of `crw manage`, not of the relay; the default role (parent) is not
changed. The settings of the send are the authorization recorded for the supervision binding of that thread in the relay (the model and
the reasoning effort), read when the notice is sent and again when the pump flushes it; the `crw manage` configuration has only the
management and parent blocks. Before anything is sent the command asks the thread for its active turn, with the probe the pump uses:

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
again next round. The unload race is not a refusal of the notice: it is not counted against the refusal limit, and the next attempt goes
out under `<logical id>-l<n>`, so the settled refusal of the earlier id is not replayed. Any other refusal counts; the third moves the
notice to `refused/` and tells the flow rows. The parent queue's flush is not changed.

**Reading.** The initiative lead reads its reports when a turn starts, `crw flow reports --initiative I --unread`, and acknowledges each
in that turn, `crw flow report-ack ID --turn T`, which appends a `report_read` row. A report nobody read for `low_priority_seconds` is
raised to the manager as the low kind `report_unread` (the report, its age, the last delivery result). A decision for Jun is the urgent
kind `jun_decision`; the manager records the question and Jun's answer with `crw manage decisions`.

## Rows

All of these are rows of `flow_events` (`kind` is the first column of the table, the rest are members of the payload).

| kind | members |
| --- | --- |
| `report` | `report_id`, `report_kind`, `run`, `milestone`, `subject`, `plan`, `body_digest`, `body_path`, `logical_id`, `recipient_state` (`initiative`, `store`, `unregistered_scope`, `unreadable`), `recipient_thread`, `recipient_initiative`, `recipient_project`, `recipient_reason`, `recipient_detail` |
| `report_delivery` | `report_id`, `result`, `request_id`, `observation`, `reason`, `logical_id` |
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
crw flow reports [--initiative I] [--unread] [--json] [--state DIR] [--socket PATH]
crw flow report-ack REPORT --turn T [--state DIR] [--socket PATH]
crw flow report-delivered --report ID --result accepted|queued|refused|unknown --request-id R [--observation O] [--state DIR] [--socket PATH]
crw manage send-parent --role supervisor --thread T --message-file F --logical-id ID [--report ID] [--initiative I] [--queue]
```
