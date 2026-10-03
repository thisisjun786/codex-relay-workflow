# DAG progress

`dag-progress --plan P` answers how far a DAG plan has come: how many of its nodes stand in which stage, how much has been accepted and integrated, what the denominator was at every plan
revision and whether it changed, why every node that is not moving is not moving, and where each node's relationship, thread and pull request are. It reads the relay store and nothing else, and
it writes nothing. It follows the DAG execution contract (CRW-182) sections 1.3, 3.1, 7.4, 8 and 8.6; the plan is [the plan store](dag-plans.md) and the nodes' states are
[the scheduler's reading](dag-scheduler.md), which this page projects and never derives again.

The relay's log is the canonical plan and the execution records are the relay's (contract 1.3, decision D-13). This command is a read-only projection of them. Linear is a projection that
[the summary outbox](dag-outbox.md) builds from this one: nothing here reads Linear.

## The command

```
crw relay [--state DIR] dag-progress --plan P
```

JSON on stdout, exit 0. A plan or revision the store does not hold is the refusal `unregistered_scope` and a state directory with no store is `store_absent` (exit 2), as for `dag-ready`;
a missing `--plan` is the parser's (exit 2, usage on stderr); a stored plan that disagrees with itself, or a projection that cannot be printed honestly (below), is the host's failure (exit 3).
No refusal reason is added.

### What it reads, and what it does not

* It opens the store mode=ro with `query_only` on, never through the writer's opener: a store that still owes a repair the writer's open makes (a settlement backfilled from an observation, an
  index that was dropped) is left exactly as it is, and no store is ever created.
* It reads no Linear and no forge, runs no process, reads no clock, reads no artifact bytes and no artifact's metadata, and does not resolve the relay's executable. It marks its read store-only:
  with the mark the manifest the stale judgement rebuilds does not stat an artifact whose receipt declared no size, and the assignment view spells its recovery commands (which the reading
  discards) with a fixed program name. `dag-ready`, which does not set the mark, is unchanged. The one printed value that depended on that size, the digest of the manifest rebuilt now for a stale
  node, is not printed (`dag-ready` still prints it). The only filesystem work left is what SQLite needs to open and read the store.
* So the document is a function of the store: deleting every artifact file, emptying PATH or moving the executable changes no byte, and two queries of one store state print the same document and
  the same `digest`. A node whose artifact bytes changed on disk reads as the store says (ready, say); the byte checks of contract 4.4 (B-03 to B-05, B-17's file half) belong to
  `dag-ready` and the release, which re-hash the files before a node is given to a child.
* Activity is never progress: a poll of a child's turn, an observation of its lifecycle, a turn admitted, a token count for a dimension nobody limited are not read, and no clock is, so no stage
  moves because time passed or a child was merely alive. An overdue state would have to come from a durable event, and none is derived here.
* A resource fact is not activity. A limit someone declared and a usage someone recorded against it (`limit-declare`, `usage-observe`) decide whether a node that has not started reads
  `ready` or `waiting_resource`, as in `dag-ready`. They never move a node that started.

## The document

`dag-progress/1`, in this order:

| Key | Meaning |
| --- | --- |
| `ok`, `schema` | `true`, `dag-progress/1` |
| `plan_id`, `project_key`, `plan_revision`, `state_digest` | the plan, its project, the head revision the reading was taken at and that revision's state digest |
| `plan_state` | `paused` while the plan is paused, else null |
| `denominator` | the denominator at the head ([below](#the-denominator-per-plan-revision)) |
| `revisions` | the denominator at every revision, oldest first |
| `stages` | the stage distribution, a partition of the live nodes |
| `blocked` | the blocked overlay |
| `cumulative` | accepted and integrated, kept apart from the stages |
| `outside_denominator` | what the revisions took out of the plan |
| `nodes` | one entry per live node, by node id |
| `digest` | the sha256 of the printed document without this key |

## Stages

The stages partition the live nodes: the counts of `stages` add up to `denominator.nodes`, and a node is in exactly one. Every stage is listed, with `nodes` (the count, zero included) and
`node_ids`. An owned node keeps the stage of its derived state; an unowned node is placed by its disposition and reason.

| Stage | A node is here when |
| --- | --- |
| `waiting_predecessor` | unowned, and its first unsatisfied incoming edge is not satisfied (`wait:edge:<edge_id>`), or an input check or an edge fact blocks it (any `blocked:` reason except `blocked:decision_mismatch`) |
| `waiting_decision` | unowned, and a decision edge has no decision with the plan's digest from a required authority (`defer:authority_pending`, `blocked:decision_mismatch`) |
| `waiting_resource` | unowned, every edge satisfied, and held back by capacity, an unmeasured ceiling, edit regions, a merge window, an unverified owner, or another owner of the issue (`defer:*`, `skip:already_owned`) |
| `ready` | released next (the `ready` disposition of `dag-ready`, without the artifact byte checks) |
| `releasing` | a release is decided and its child is not yet bound to the node |
| `creation_unknown` | a child's creation was armed and its outcome is not known; a repeat of the release observes the App Server and continues, creates again or says why ([dag-scheduler.md](dag-scheduler.md#a-creation-whose-outcome-is-unknown)) |
| `running` | the relationship is open and nothing has been reported |
| `reported` | the child reported and nobody has judged (or the child's newest report is `blocked_needs_input`) |
| `verifying` | a report is under review, or was ruled verified and not yet accepted or merged |
| `correcting` | a correction generation is open |
| `accepted` | the parent accepted the result, and it has not landed and is not stale |
| `integrated` | the accepted head is contained in every target it has to land on and the parent marked it merged (`done:integrated`) |
| `stale` | an accepted result that no longer matches the plan (`stale:slice_changed`, `stale:criteria_changed`, `stale:edge:<edge_id>`; contract 3.1, E-18, E-24) |
| `paused` | the relationship is paused (contract 7.4: it keeps its issue and its slot, and an acceptance it holds stands), or the plan paused the node, or the plan is paused and nobody owns the node (`defer:node_paused`, `defer:plan_paused`) |
| `cancelled` | the relationship was cancelled, or the plan cancelled the node whatever its execution says (`skip:node_cancelled`) |
| `archived` | the plan archived the node whatever its execution says (`skip:node_archived`) |
| `closed` | the relationship is closed or archived |
| `ambiguous` | the relationship has more than one head revision, or an execution has no relationship |

The plan's own lifecycle (pause, cancel and archive of a node, pause of the plan) is read from the plan as the reading reads it. A node the plan cancelled or archived is in that stage unless its pull request landed: the plan cannot take a landing back, so an integrated node stays `integrated`. A node the plan paused that already has a relationship keeps the stage of its execution, because the plan stops the scheduler and not the child; its `lifecycle` and its reason (`defer:node_paused`, or `defer:plan_paused` in a paused plan) say that the plan holds it.

A node whose derived state or disposition the rule does not cover is not dropped: the projection refuses it (see [What it refuses](#what-it-refuses)).

### Blocked is an overlay, not a stage

`blocked` has `entries`, one for each node whose disposition is blocked, each with its `node_id`, its `stage` (the stage it stays in), its `reason` (a closed `blocked:` reason) and its `detail`. An accepted node whose merge
turn ended with an unknown effect is `accepted` and listed as blocked (`blocked:effect_unknown`); a node whose child reported `blocked_needs_input` is `reported` and listed
(`blocked:input_unverified_at_consumption`). `blocked.nodes` is the number of `entries`. The overlay overlaps the stages and is never added to them.

### Every node that is not moving has a reason

Each entry of `nodes` names the node (`node_id`, `issue_key`, `kind`, `title`) and carries `stage`, the reading's `state`, `disposition` and `reason` (a member of the closed set of [the scheduler's reasons](dag-scheduler.md#dispositions-and-reasons))
and a `detail` sentence, and `lifecycle` (what the plan says of the node: `paused`, `cancelled`, `archived`, else null). A stale node also carries a `stale` object: `cause`, `seed_node_id`, `edge_id`, `predecessor_node_id`, `consumed_acceptance_id`, `current_acceptance_id`,
`consumed_decision`, `current_decision`, `consumed_slice_digest`, `current_slice_digest` and `consumed_manifest_digest`, as `dag-ready` prints it. The reasons of a running node
(`skip:already_owned`) and of an accepted one (`done:accepted`) are the scheduler's too, so a reader can join the two commands.

## Cumulative counts

`cumulative` has two measures, each `{nodes, of}` with `of` the denominator:

* `accepted`: the live nodes that hold an active acceptance, whatever has happened to them since. A node that is integrated, stale, paused or blocked is still counted. A node the plan cancelled or
  archived whose pull request did not land is not: the plan does not count what it produced as done. It is not the `accepted` stage, which is a node that has been accepted and not yet landed.
* `integrated`: the live nodes in the integrated state, the same nodes as the `integrated` stage. An integrated node whose result is later replaced by a new acceptance leaves it (a revision
  above a landed node makes a new node, not a rerun: contract E-20).

The two overlap (an integrated node is also accepted), so they are never added, and the document computes no completion rate, percentage, total or ratio. A reader who wants one divides one measure
by its own `of` and says which. `integrated` never exceeds `accepted`, and `accepted` never exceeds the denominator.

## The denominator per plan revision

The denominator is the number of live nodes. `revisions` has one entry per revision of the plan, from 1 to the head (revision 0 is the empty plan and has no entry):

| Key | Meaning |
| --- | --- |
| `revision`, `recorded_at`, `request_id`, `author_task_id`, `coordinator_epoch`, `state_digest` | the revision as the log recorded it |
| `nodes` | the live nodes this revision left |
| `previous_nodes`, `delta` | the count before it (0 for revision 1) and the difference |
| `denominator_changed` | true when a node entered or left the plan, even when the count is the same (a node replaced by another) |
| `ops` | the typed changes of the revision, in order (`add_node`, `pause_plan`, `cancel_node`, ...): what the plan was told to do |
| `added`, `retired` | the node ids that entered and left |
| `updated` | node ids live before and after whose version was re-introduced at this revision (the node's spec or its incoming edges changed); they do not change the denominator |

`denominator` is the head's: `revision`, `nodes`, `previous_revision`, `previous_nodes`, `delta`, `changed`, and `last_changed_revision` (the latest revision whose
`denominator_changed` is true). A reader always sees that the denominator moved and when. The counts are the stored log's (a node version is live at revision r when it was introduced at or before r
and not retired by r); the head is verified when the reading is taken, and each revision's `state_digest` is printed so any of them can be checked against `dag-plan-show --revision N --verify`.

`outside_denominator` says what the revisions took out of the plan: `nodes` and `node_ids` that are no longer live, how many of them had an acceptance (`with_acceptance`) and how many
still hold an execution slot (`holding_slot`). Their accepted work is in neither the numerator nor the denominator, and the reader can see it.

## Links

Every node carries `acceptance_id` (its active acceptance, or null), `holds_slot` (an execution slot is held for it: a paused node still holds it) and `links`. A link the store does not hold is
null; none is guessed.

| Key | Meaning |
| --- | --- |
| `relationship` | the relationship the node stands on now: `id`, `execution_generation`, `status` (`assignment-show --relationship ID` opens it) |
| `thread` | `child_task_id` and `parent_task_id` of that relationship; for a release that has no relationship yet, the child its managed start already created |
| `executions` | every relationship the plan ever bound to the node: `relationship_id`, `execution_generation`, `kind` |
| `managed_start` | for a node with no relationship, the managed start of its open intent (the release, or the successor release after a close, whose request nobody closed; a closed release owns nothing): `request_id`, `state`, `receipt_status` |
| `pull_request` | an implementation node's pull request: `repository`, `number`, `head_sha`, `url`, `source`, `event_id` |

`pull_request` has two sources. `acceptance`: the forge identity recorded with the active acceptance (repository and number from the forge row, `head_sha` the head the parent accepted;
`url` and `event_id` null). `work_report`: before any acceptance, the work report of the relationship's current head event, bound to that event, generation and revision so that a later push
cannot inherit an earlier report; a node with no head, an ambiguous head or no report has no link. This build has no writer of work reports, so the second source holds only rows an earlier build
wrote.

## What it refuses

A projection that cannot be printed honestly is the host's failure (exit 3), never a partial document: a node whose state and disposition no stage covers, a blocked, stale, waiting, deferred or
skipped node whose reason is not of the closed set (a blocked node needs a `blocked:` reason, a stale node a `stale:` reason), or revisions that do not end at the reading's own denominator.
The scheduler's reading guarantees none of these happens; the check is what makes a gap a failure instead of a blank.

## Calling it from Go

`internal/relay/dagsched`:

* `Scheduler.ReadProgress(ctx, plan)` is the query in one read transaction of the store, as `Scheduler.Read` is for `dag-ready`.
* `Scheduler.Progress(ctx, q, plan)` is the same inside a transaction the caller already holds (`ReadProgress` inside a plain `Transaction` is `store.ErrNestedTransaction`; inside `Compose`
  it joins the caller's).
* `ProjectProgress(ProgressInput)` is the pure half, with no I/O: a reading, the denominators per revision, the facts of each node. A consumer that rebuilt a reading from a snapshot passes it
  through this to get the same stages, the same guards and the same digest.
* `Progress.Object()` is the printed document; `Progress.Reading` is the reading it was built on.
* `ProgressDelta`, `ReadProgressDelta`, `ProgressSnapshotOf`, `ReadProgressSnapshot`, `ApplyProgressDelta` and `ProgressSnapshot.Project` rebuild the view from events and a cursor ([below](#rebuilding-the-view)).

## Rebuilding the view

A reader that keeps its own copy of the view (a dashboard, say; the Linear summary of [the summary outbox](dag-outbox.md) states the live view and keeps no copy) has to resume from where it stopped, and what it assembles has to be what a live query prints. The rebuild is Go in `internal/relay/dagsched` (CRW-287). It adds no command, no table, no zone statement and no refusal reason, and it writes nothing: the readers are read transactions and the fold is a pure function. [DAG plans](dag-plans.md#events-cursors-and-snapshots) defines events, cursors and snapshots for the plan; this section applies the same boundary to the whole view.

### What an event is

The view rests on the plan log and on execution rows, and they have different histories. The plan log is a true log: committed revisions, appended and never changed, each with its own position. The execution rows are not: the store updates them in place (a relationship's status, a managed start's state, a merge turn's state, a slot's state, an acceptance's state, an observation's `reverted_by`), keeps no sequence that spans tables and journals only some of the writers, so the order in which they changed cannot be recovered and there is nothing to replay. What can be replayed is what the view takes from them: the scheduler's reading of each node with the node's facts (the inputs of `ProjectProgress`) and the one aggregate of the nodes the revisions took out of the plan. Each is content-addressed by a digest of its printed fields. An event is therefore one of:

| Kind | What it says | Holds |
| --- | --- | --- |
| `plan` | the plan's identity, sent when the cursor holds no plan (or another plan) | `PlanID`, `ProjectKey` |
| `revision` | a committed revision of the plan log, folded with `dag.Replay` (which checks the state digest the revision recorded) | the `dag.Event` |
| `removed` | a node the reader holds a record of that is no longer live | `NodeID` |
| `outside` | the nodes that left the plan (the `outside_denominator` of the document), when it differs from the reader's | `ProgressOutside` |
| `node` | the record of a live node whose digest differs from the reader's, or that the reader does not hold: its reading (state, disposition, reason, detail, stale object, lifecycle) and its facts (title, acceptance, slot, links); it does not carry the node's stage, which `ProjectProgress` derives again | `ProgressRecord` |

`ProgressEvent.ID()` names an event by its plan, its kind, its subject and the digest of what it says (`p1/node/n1@<digest>`, `p1/revision/3@<state digest>`; node ids are plan-local, so the plan is part of it). It is for counting deliveries inside one read of a store that is not moving: such a read never sends one ID twice. It is not for deduplicating across reads. A record that returns to an earlier state (running, paused, running) has the earlier state's ID again, and the event that brings it back must still be folded: what a reader has seen is what its cursor says, and a reader folds every event it is given. The log metadata of a revision is covered by the hash chain of the cursor, not by the ID.

### The cursor

A `ProgressCursor` is the state the reader holds, as digests: the plan, the last revision it holds with a hash chain over the log metadata of the revisions up to it (revision, `recorded_at`, request id, author, epoch, state digest and operations of each, which a reader computes from its own revisions), the digest of its outside aggregate and the digest of every node record it holds. The zero value holds nothing and is the beginning. `ProgressSnapshot.Cursor()` is the cursor of a snapshot; `ProgressCursor.String()` is JSON with every key in a fixed order and `ParseProgressCursor` reads it back (the empty text is the beginning), so a cursor can cross a process boundary (a later command, a log). A cursor alone cannot seed a fold: the fold also needs the plan and the records the cursor describes, so a reader that lost its snapshot rebuilds from the zero cursor.

The cursor is the reader's state and not a position in a list, so a read from it is "what differs from that" and no page remembers anything. A cursor of another plan holds nothing of this one: the read starts from the beginning with the `plan` event, and a fold that holds the other plan is reset by it.

### The read

`(*Scheduler).ProgressDelta(ctx, q, plan, after, limit)` returns a `ProgressDelta` inside the caller's transaction, as `Progress` does; `ReadProgressDelta` is the same in a read transaction of its own, as `ReadProgress` is (inside a plain `Transaction` it is `store.ErrNestedTransaction`, inside `Compose` it joins the caller's). It reads the live view once, so a page is one state of the store, and sends in this order: the `plan` event when the cursor holds no plan, the `revision` events after the cursor, a `removed` event for every record the cursor holds that is no longer live, the `outside` event when the aggregate differs, and a `node` event for every live node whose digest differs, by node id. Removals come before records so a reader never holds more than the plan's limit of 64 nodes while a node is replaced. `limit` is 1 to `dag.MaxPage` and counts events of every kind. The delta carries `Events`, `Cursor` (the reader's cursor advanced over the events returned, from which the next call continues exactly there), `More` and `Fingerprint`, the digest of the live view the page was read at.

`(*Scheduler).ProgressSnapshotOf(ctx, q, plan)` and `ReadProgressSnapshot` give the live view as the state a reader holds, with the view it was taken from. A `ProgressSnapshot` is the plan as of its revision (a `dag.Snapshot`), the denominators of every revision, one record per live node and the outside aggregate.

### The fold

`ApplyProgressDelta(snapshot, delta)` is the only way a snapshot is folded. It is pure, does not change what it is given and returns the zero snapshot with an `InvariantError` (the host's failure class) for whatever a log cannot contain: an event that does not follow the one before or a revision twice (`dag.Replay`), a record or outside aggregate whose digest is not its content, a `revision` event of another plan (`dag.Replay` does not look at the plan id), a delta of another plan than the snapshot holds unless it starts with the `plan` event, and a fold whose cursor is not the delta's. It rebuilds the denominator of each revision from the plan before and after it (`revisionCountOf`: the nodes each side holds, the nodes that came and went, the nodes whose version the revision introduced), not from a copy, so the revisions of a rebuilt snapshot are the revisions read from the rows. While `More` is true the result is not complete. On the page with `More` false it projects the result and requires its digest to be the delta's `Fingerprint` before it marks the snapshot complete, so a fold that dropped an event, or whose result is not the view the page was read at, cannot say it caught up. The check is on the result: folding a page twice is harmless for records (an upsert) and refused for a revision (it does not follow the one before).

`ProgressSnapshot.Project()` is `ProjectProgress` over a reading rebuilt from the snapshot (the plan's identity, head and state from the replayed plan, the records as the readings and facts of the nodes), so the stages, the guards and the digest are those of `dag-progress`. It refuses a snapshot that is not complete: the zero value, one cut between pages, one built by hand. A snapshot cut between pages can pass the checks `ProjectProgress` has (the head revision and the node count) while two of its records are from different states of the store, and would print a hybrid.

Replaying from the beginning is a read from the zero cursor folded into the zero snapshot; a snapshot plus the events after its cursor is a read from `snapshot.Cursor()` folded into the snapshot. Both end at the document `dag-progress` prints for the store state the last page was read at, byte for byte and with its digest. The rebuilt `Progress.Reading` holds what the document prints (the plan, its head and state, the nodes' readings); the capacity side of the pass, the ready order, the ranks and the input digest of `dag-ready` are not part of the view and are not rebuilt.

### Errors

No refusal reason is added. A cursor that names a revision the plan does not have, and an unknown plan, are `unregistered_scope` (what `dag-plan-log --after` answers). A cursor whose revision chain is not the log's, whether the state digest of a revision or only its request, author, time, epoch or operations differ, is `revision_mismatch`: the reader's log is not this one and it starts over from the zero cursor. A cursor text that is not a cursor (not a JSON object, unknown keys, data after it, a revision without its digest, a digest that is not a sha256) is `malformed_receipt`; only the empty text, which is the beginning, is not. The counts a revision derives (nodes, added, retired, updated) are not part of the chain: they differ only if the fold and the rows differ, and the fingerprint reports that.

### What it guarantees, and what it does not

* **No duplicates, no omissions.** On a store that is not moving, the read from a cursor is one list; pages of any size are that list cut in order, and from the cursor a page returns, as a value or as text, the pages are the rest of the list. No (kind, subject, digest) pair, which is what `ID()` names, is sent twice.
* **A store that moves.** Nothing is lost or repeated that the reader holds: a record that changed after it was read is read again as a new event, a record that did not change is not sent again, and the fold still ends at the view of the page with `More` false. A store that keeps moving can delay that page without bound (liveness, not correctness), and only the fingerprint of the page with `More` false means anything for the fold.
* **Coalescing.** The events of the execution half are the differences between what the reader holds and the store now, so transitions between two reads are one change (running, reported, verifying is one `node` event). Only the plan's revisions are complete history; there is no "execution events after time T". The passes `dag-ready --record` keeps are the record of readings over time.
* **No durable format for snapshots or events.** They are values in the process, to be treated as read-only: they share their nested data (links, stale objects, id lists) with the view they were taken from and with one another. Only the cursor has a text form. A reader that has to keep a view across restarts keeps the printed document, or rebuilds from the zero cursor; a cursor saved without its snapshot only tells the store where the reader stood.
* **Transactions.** The reads are read transactions like `ReadProgress`: on a store opened for writing a transaction takes the write lock (`BEGIN IMMEDIATE`) though it writes nothing, and a reader that must not take it opens the store read-only, as `dag-progress` does. Through a read-only store (`mode=ro`, `query_only`) every function here works and writes nothing; the tests hash every row of every table, the journal included, before and after.


## What is not here

* The Linear summary is [the summary outbox](dag-outbox.md): it states the live view, so it reads `Scheduler.Progress` in the transaction that appends its entry and compares digests, and writes its own table. It does not use the rebuild (a summary is a whole document, not a delta). No command wraps the rebuild: a command that prints a delta would be a new document to specify and would add a command to the relay.
* The metrics contract 8.6 records for the evaluation (makespan, idle slot minutes, wake latencies) are not derived here, and no figure here is a target.
* The byte checks of the artifacts are `dag-ready`'s ([the scheduler](dag-scheduler.md#input-checks)).
