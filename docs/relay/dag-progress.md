# DAG progress

`dag-progress --plan P` answers how far a DAG plan has come: how many of its nodes stand in which stage, how much has been accepted and integrated, what the denominator was at every plan
revision and whether it changed, why every node that is not moving is not moving, and where each node's relationship, thread and pull request are. It reads the relay store and nothing else, and
it writes nothing. It follows the DAG execution contract (CRW-182) sections 1.3, 3.1, 7.4, 8 and 8.6; the plan is [the plan store](dag-plans.md) and the nodes' states are
[the scheduler's reading](dag-scheduler.md), which this page projects and never derives again.

The relay's log is the canonical plan and the execution records are the relay's (contract 1.3, decision D-13). This command is a read-only projection of them. Linear is a projection the later
summary issue builds from this one: nothing here reads Linear.

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
| `creation_unknown` | a child's creation was armed and its outcome is not known |
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

## What is not here

* Cursor and snapshot reconstruction of the progress view, and the Linear summary, are later issues that call the functions above.
* The metrics contract 8.6 records for the evaluation (makespan, idle slot minutes, wake latencies) are not derived here, and no figure here is a target.
* The byte checks of the artifacts are `dag-ready`'s ([the scheduler](dag-scheduler.md#input-checks)).
