# DAG plans

A DAG plan is the relay's record of how a project's issues depend on one another: which deliverables (nodes) exist, and
what must be true of one before another may start (edges). This page is the normative description of how a plan is
defined, validated, stored and read. It does not describe how a plan is run: choosing what is ready, releasing a node,
accepting its result and observing integration belong to [the scheduler](dag-scheduler.md) that reads these plans. The model follows the
DAG execution contract (CRW-182, sections 1, 2, 4, 5 and 6.2); where this page reads the contract it says so
([the readings](#where-this-page-reads-the-contract)).

## The model

* A **plan** has an id and belongs to one project (the Linear project key). The relay's log is the canonical plan;
  Linear is a projection of it and a difference is drift (decision D-13).
* A **node** is one deliverable: an issue key, a kind, a title, and the digest of its completion criteria. Kind
  `implementation` is one issue, one child, one PR; kind `non_pr` is research, design, verification or operation with no
  PR. Node ids are plan-local.
* An **edge** orders two nodes and says what satisfies it. `artifact_verified`: the predecessor's artifact was verified
  (for an implementation node it is a code artifact and the edge pins the verified head, naming the repository and base
  ref); `integrated`: the predecessor's PR landed on a named repository and base ref; `decision`: a named decision, by digest, was
  recorded by a required authority. Which fields an edge carries depends on its kind, and nothing else is read.
* A **revision** is a list of typed changes (`add_node`, `update_node`, `replace_node`, `retire_node`, `add_edge`,
  `retire_edge`) applied to the plan the previous revision produced. The plan at any revision is the fold of the log up to it.
  `update_node` replaces a node's whole spec and keeps its id; `replace_node` introduces a new node that supersedes a live one.
  Edges are never edited: retire one and add another. Ids are never reused within a plan, so a retired id cannot come back.
* A **lifecycle change** holds or ends a node, or pauses the plan: `pause_node`, `resume_node`, `cancel_node`, `archive_node` and, for the plan, `pause_plan` and `resume_plan`
  ([pause, resume, cancel and archive](#pause-resume-cancel-and-archive)). They are changes of the same log; nothing else records them.

## The revision document

`dag-plan-revision/1`, JSON, read strictly: an unknown field, a missing field, a field of the wrong type or an object
that repeats a key is rejected, never skipped. (A reader that ignored a misplaced `nodes` list would store an empty plan;
this one names the field and says that nodes and edges are changes.)

| Field | Meaning |
| --- | --- |
| `schema` | `dag-plan-revision/1` |
| `plan_id`, `project_key`, `author_task_id` | identifiers (letters, digits, `. _ : -`, at most 128 characters). A plan belongs to the project it was first written for |
| `request_id` | names this request; the same id again is the same request |
| `expected_parent_revision` | the revision this one is applied to; 0 for the first |
| `coordinator_epoch` | optional, a whole number: the epoch the writing session holds, 0 when none is named. A plan that has been claimed ([the coordinator epoch](dag-scheduler.md#the-coordinator-epoch)) takes only its newest epoch from the task that claimed it, and refuses any other write as `stale_coordinator_epoch`; a plan nobody has claimed takes 0 |
| `changes` | the typed changes, 1 to 256 |

A node is `{node_id, issue_key, kind, criteria_set_digest, title?}` (`criteria_set_digest` is a sha256 in lowercase hex).
An edge is `{edge_id, from_node_id, to_node_id, kind}` plus, by kind: `target_repository`, `target_base_ref`, `pins_code_head`
(artifact_verified, integrated); `decision_subject`, `decision_digest`, `required_authority` (decision). `required_authority`
entries are opaque text: the format of an authority is undecided (D-09) and is not invented here.

A lifecycle change is `{"op":"pause_node","node_id":"n1"}` (likewise `resume_node`, `cancel_node`, `archive_node`: the op and the node, nothing else) or `{"op":"pause_plan"}`
(likewise `resume_plan`: the op alone). A field that is not named is rejected like any other.

## What is rejected, before anything is written

A revision is validated as the plan it would produce, so every rule is judged on the whole graph. All violations are
collected and reported together, in a fixed order, each as `[rule] path: why`; the same document against the same parent
always answers the same way. A rejected revision writes nothing.

| Rule | Rejected when |
| --- | --- |
| `empty_changes`, `empty_graph` | the revision carries no change; the plan would hold no node |
| `duplicate_node_id`, `duplicate_edge_id` | an id is added twice, is already in the plan, or was retired (ids are never reused) |
| `unknown_node`, `unknown_edge`, `conflicting_changes` | a change addresses a node or edge the parent plan does not hold, or two changes of one revision address the same one |
| `invalid_lifecycle_transition` | a lifecycle change the node or the plan is not in a state for: a pause of a paused node, a resume of one that is not paused, any change to a cancelled or archived node, a resume of an active plan. A lifecycle change addresses a node like every other change to it, so it is `unknown_node` for a node that is not in the parent plan (one added in the same revision included) and `conflicting_changes` beside another change to the same node or to the plan's lifecycle |
| `missing_predecessor`, `missing_successor`, `self_reference` | an edge starts or ends at a node that is not in the plan, or at itself |
| `cycle` | the edges form a cycle; the detail names one (`a -> b -> a`) |
| `unknown_edge_kind`, `unknown_node_kind` | a kind outside the vocabulary |
| `edge_field_missing` | contract 2.4: `integrated` without `target_repository` and `target_base_ref`; a code `artifact_verified` without `pins_code_head`, `target_repository` and `target_base_ref`; `decision` without `decision_subject`, `decision_digest` and a non-empty `required_authority` |
| `output_integrated_needs_implementation` | contract 2.4 and 1.2: an `integrated` edge leaving a node that is not an implementation node |
| `output_decision_needs_non_pr`, `edge_field_not_applicable` | the contract's reading, see below: a `decision` edge leaving an implementation node; a field that belongs to another kind (a head pin on a non-code artifact, a target on a decision) |
| `limit_exceeded` | more than 64 nodes, 256 edges or 256 changes, a document over 1 MiB, an over-long field |
| `project_mismatch` | the revision names another project than the plan's |
| `unknown_field`, `missing_field`, `wrong_type`, `empty_value`, `bad_identifier`, `bad_digest`, `bad_schema`, `bad_text`, `value_too_long`, `duplicate_value`, `duplicate_key`, `unknown_op`, `not_an_object` | the document is not of the shape above (an empty string is a missing value, never an accepted one) |

The limits bound a plan's size. They are not execution limits (how many nodes may run at once), which are the scheduler's.

## Digests

Digests are sha256 of canonical JSON (sorted keys, no whitespace), the serialization the relay's other digests use.

* A node's **slice digest** covers the node's spec and its incoming edges, sorted by id. Nothing else: no revision number,
  no other node, no digest of the whole plan. Editing a node changes that node's digest; changing the edges into a node changes
  that node's digest; no other digest moves.
* A plan's **state digest** covers its live content (plan id, project, nodes with their slice digests, edges). Two revisions
  that produce the same plan produce the same state digest.
* A request's digest covers what it asks, not how the JSON was spelled.
* A node's lifecycle is no part of its slice digest: a pause is neither a change of the node's spec nor of its incoming edges, so it moves no slice digest and invalidates no manifest or
  acceptance. The state digest covers the lifecycle only when it is not the default (a `lifecycle` key on a node that is paused, cancelled or archived, a `plan_state` key while the plan is paused), so
  every plan that never had a lifecycle change digests as it always did, and a resume returns the plan to the state digest it had before the pause.

## The log

Revisions are appended, never changed. `dag-plan-put` is one store transaction (BEGIN IMMEDIATE, the store's atomic contract):

1. a `request_id` the plan has already recorded returns that revision (`replayed: true`) when the request is the same one,
   however far the plan has moved on, and is `plan_revision_conflict` when it is another request;
2. an `expected_parent_revision` that is not the plan's head is `plan_revision_conflict`: the writer lost a race, re-read the plan and decide again;
3. the changes are folded onto the head and the result is validated;
4. only then is anything written: the plan's header (first revision), the revision row, and the rows of the fold.

Before step 1, as the first statement of the transaction, the coordinator epoch the revision names is checked (see [the coordinator epoch](dag-scheduler.md#the-coordinator-epoch)): a session that does not hold the plan's epoch is refused as `stale_coordinator_epoch`, whether or not its request was recorded, and writes nothing. A request id sent again under another epoch is another request (`plan_revision_conflict`): a restarted parent reads the log.

Commit is the only moment a revision becomes visible. A writer killed before it leaves no row of that revision in any table,
and reads return the last committed revision. The tables enforce what the writer does: triggers abort an UPDATE or DELETE of a
plan or revision, a node or edge row may change once (from live to retired) and is never deleted, and revision n has parent n-1,
which must exist, so a chain cannot branch. The fold is materialized as node versions (a node whose spec or incoming edges change
is retired and introduced again at that revision) and edges, so the plan as of any revision is one query.

## Events, cursors and snapshots

* An **event** is a committed revision: its typed changes, request id, epoch, author and the state digest it produced.
* The **cursor** is a revision number. `dag-plan-log --after C` reads the events after it.
* A **snapshot** is the plan as of a revision (`dag-plan-show --revision N`; the head by default).
* Replaying the events from the empty plan, or from any snapshot and the events after its cursor, restores the same plan; the replay
  checks each event follows the one before and reaches the state digest the event recorded.
* The progress view applies the same boundary to everything it prints: its events are plan revisions plus the changes of what the view takes from the execution rows, its cursor is the state a reader holds, its snapshot is rebuilt by the same replay ([DAG progress](dag-progress.md#rebuilding-the-view)).

A reader never trusts a recorded digest: `dag-plan-show` recomputes every slice digest and the state digest from the rows it read, in one
snapshot of the store, and a plan that does not agree with itself is the host's failure (exit 3), never a partial plan. `--verify` also
replays the whole log from the empty plan with every rule and requires the rows to be what it produces. A plan or revision that is not there,
or a store that holds no plans, is the refusal `unregistered_scope`; it is never read as an empty plan. A state directory with no store at all
is `store_absent`, the reason every read-only relay command gives (a read never creates a store), so a wrong `--state` is not mistaken for an unknown plan.

A writer does not trust them either. Inside the transaction `dag-plan-put` makes the same checks before it uses the rows: a repeated request is
answered, and the next revision is built, only from rows whose slice digests and state digest agree with the log, so a plan that was damaged
outside the product is the host's failure on a write too and never carries forward under digests that look right. The Go entry point `Repo.Put`
takes a typed revision and judges it by the rules of a revision document (`Checked`), so a caller that never wrote a document cannot store an
empty change list, an op that does not exist or a malformed digest. The early check before the writing open (`Preflight`) hands a request on
once it finds it in the log, because that request may have committed between two of its reads; a failure to read the store is returned as it is. It checks the coordinator epoch before anything else, as `Put` does, so a session that was replaced hears `stale_coordinator_epoch` and not the plan's own refusal.

## Commands

All three are relay commands (`codex-session-relay [--state DIR] <command>`); output is JSON on stdout.

| Command | Does | Exit |
| --- | --- | --- |
| `dag-plan-put --request <json or @file>` | appends a revision; answers `{ok, replayed, plan_id, project_key, revision_no, parent_revision_no, request_id, request_digest, state_digest, coordinator_epoch, author_task_id, recorded_at, node_digests}` | 0; 2 refused (`malformed_receipt` for a rejected plan, `plan_revision_conflict`, `stale_coordinator_epoch`); 3 host; 4 a document that is not JSON or cannot be read |
| `dag-plan-show --plan P [--revision N] [--verify]` | the plan as of a revision. `nodes` and `edges` are the top-level lists of the answer, with `schema`, `revision_no`, `head_revision_no`, `state_digest`, `digests_verified`, `log_verified` | 0; 2 `unregistered_scope`; 3 corrupt |
| `dag-plan-log --plan P [--after C] [--limit N]` | the events after a cursor with their typed changes, and the `cursor` to resume from | 0; 2 `unregistered_scope` |

`dag-plan-put` validates the document, and the plan it would produce when that needs no store, before it opens the store for
writing. An invalid first revision, or a revision that names a parent no plan has, on a state directory with no store creates no
store; against a store it reads in place (no sidecar is created) and refuses before the writing open when the parent is stale or the
changes break a rule. A store that cannot be read that way is the host's failure and is not handed on to the writing open. Reads never
create the zone.

### Refusal reasons

The relay's exit-code contract is unchanged except for two reasons. A rejected plan reuses `malformed_receipt` (the relay's reason for a structured document that is malformed, as the linkage envelope uses it), an unknown plan or revision reuses `unregistered_scope`, and two reasons are new (decision D-02): **`plan_revision_conflict`**, a plan write that lost to another writer (a stale parent) or that reuses a request id for a different request, and **`stale_coordinator_epoch`**, a write from a session that does not hold the plan's coordinator epoch: a newer session claimed it, the claim is another task's, or the parent binding it was made under is no longer a live parent binding of the project. Both are registered in `contract/schema/relay-exit-codes.json` and the generated Go (`internal/contract/exit_codes_generated.go`).

## Pause, resume, cancel and archive

A node, or the plan as a whole, is held or ended by a revision (CRW-281). `pause_node` and `resume_node` hold a node and let it go, `cancel_node` and `archive_node` end it, and `pause_plan`
and `resume_plan` hold every node at once. Each state a change may start from, and where it leads:

| Change | From | To |
| --- | --- | --- |
| `pause_node` | active | paused |
| `resume_node` | paused | active |
| `cancel_node` | active, paused | cancelled |
| `archive_node` | active, paused | archived |
| `pause_plan` | active | paused |
| `resume_plan` | paused | active |

Cancelled and archived are final: nothing in the plan moves a node out of them, so a cancel is never shown as reverted. The work is done again with a new node (`replace_node`). A change from
any other state is `invalid_lifecycle_transition`. The state belongs to the node id: `update_node` of a paused node leaves it paused, `replace_node` introduces a new node that is active, and
`retire_node` takes the node out of the plan with its state.

The state is the fold of these changes and is kept nowhere else: no table, no row that is updated, no command of its own (`dag-plan-put` appends the revision), and no statement added to the zone.
A reader derives it from the log up to the revision it reads. Reading checks the transition table and that the node was ever in the plan: a stored change that is not a legal transition, or that names a
node the plan never held, is the host's failure (exit 3) whatever digest the revision recorded. The other rules of a revision (two changes of one subject in a revision, a target that was live in the parent
revision) are judged by the writer and by `--verify`, which replays the log through the fold, and not by an ordinary read. `dag-plan-show` prints `lifecycle` on a node that is paused, cancelled or archived and `plan_state` while the plan is
paused; both keys are absent otherwise, so a plan with no lifecycle change reads as it always did. The plan as of an earlier revision reads as it was then.

A runtime that predates these changes cannot read a log that holds one: its strict reader refuses an operation it does not know, and the plan reads as corrupt. The store itself is unchanged, so
such a runtime still opens it; installing one over a store whose plans carry a lifecycle change is a rollback to avoid. What the scheduler does with the state is in
[the scheduler](dag-scheduler.md#pause-resume-cancel-and-archive).

## The store

The log lives in the relay's host-shared store, in tables the writable open creates after it has validated the frozen v1
tables (decision D-01):

* the open validates only the tables of the frozen v1 schema (`relay-sqlite.sql`) and refuses a store missing one of them, as before;
  it then creates the zone with `CREATE ... IF NOT EXISTS`. A store that predates the zone opens, keeps every row, and gains it;
  a runtime without the zone validates only the frozen tables, so it opens a store that has it and never reads or writes it.
  `SchemaVersion` stays `1`; no existing table or column changes;
* the zone is the twelve `dag_*` tables of the contract: `dag_plans`, `dag_plan_revisions`, `dag_nodes`, `dag_edges`,
  `dag_input_manifests`, and the tables the scheduler writes (`dag_node_executions`, `dag_releases`,
  `dag_acceptances`, `dag_integration_observations`, `dag_decisions`, `dag_cap_basis`; `dag_coordinator_claims` is written by `dag-coordinator-claim`, and the scheduler's writes check it), and seventeen tables the scheduler appended to it (`dag_merge_checks`, `dag_acceptance_revalidations`, `dag_acceptance_forge`, `dag_base_refreshes`,
  `dag_passes`, `dag_node_regions`, `dag_node_region_grades`, `dag_node_region_holds`, `dag_release_requests`, `dag_conflict_observations`, `dag_conflict_observation_files`, `dag_tip_conflict_observations`, `dag_tip_conflict_observation_files`, `dag_conflict_drift`, `dag_conflict_sweeps`, `dag_conflict_sweep_members`, `dag_release_recoveries`; see [the scheduler's store](dag-scheduler.md#the-store)), and one for the project's
  Linear summary queue (`dag_summary_outbox`, with an index and five triggers: [the summary outbox](dag-outbox.md)). Node-keyed
  tables carry `plan_id`, so node ids need only be unique within a plan;
* a command that declares itself read-only never creates the zone: it arrives with the first write open;
* the zone is an append-only ledger of statements (`internal/relay/store/dag_zone.go`). A shipped statement is never edited; a column a
  later issue needs arrives as an appended `ALTER TABLE ... ADD COLUMN` that every open runs, so a fresh store and an upgraded store read the
  same text. `testdata/dag_zone_shipped.json` holds the text each object had when it shipped.

**The schema gate.** The swap gate compares the text of the objects a store holds with the text a candidate declares
([runtime installation](../runtime-install.md#why-the-schema-reading-compares-statements-and-not-versions)). This build declares the zone, and the
gate has an answer of its own for the zone and for nothing else (the zone's objects are the ones the zone statements create). Installing it onto a
store that has no zone reads `EXTENDS_ZONE` and refuses until the install command is run with `--backup-state-to DIR`, which copies the whole state
directory (copy only, byte for byte, after the daemon and in-flight cells pass and before the swap) and records the copy: that is the OPS-4.5
backup, taken by the route itself. Returning to a build without the zone, on a store this build has opened, reads `NARROWS_ZONE` and is not refused,
since an older runtime opens a store that has the zone and ignores it. Anything else, an object defined differently (a `dag_*` object included) or
another object arriving or leaving, refuses as it always has, with the acknowledgement as without it. Opening a store with this build for writing
is itself the schema change and creates the zone, so run no write command of this build against a live state directory outside the install route:
only the install command takes the backup. The route, its backup and its refusals are written in [runtime installation](../runtime-install.md#why-the-schema-reading-compares-statements-and-not-versions).

## Where this page reads the contract

* Contract 2.4 states three rejections. This page applies them, and also reads section 1.2 (which lists the edges each kind of node can
  start) as the rules `output_decision_needs_non_pr` and `edge_field_not_applicable`: a `decision` edge leaves a `non_pr` node, and a
  `pins_code_head` or a target belongs to an edge out of an implementation node. They are separate rules so one can be relaxed without
  touching the others.
* Contract 4.5 lists the tables by `node_id`. Node ids are plan-local here, so each key that names a node is scoped by `plan_id`. The input
  manifest has no plan id by contract 4.1, so `dag_input_manifests` has exactly the contract's columns and a manifest is content-addressed: the
  same consumption in two plans is one row.
* `dag_plans` (the plan's identity and project) is an addition to the contract's table list.
* Contract 3.2 gives the plan one pause (a new plan state, resumed by a revision) and gives a node's pause and cancel as the relay's relationship states. Here a node's pause, resume, cancel
  and archive are revision changes as well (the issue asks for typed revision changes and no separate update path), and the relationship's own status stays a separate fact the scheduler also
  honours. The contract has no plan-level cancel or archive and no archive row at all; this page adds none for the plan and reads a node's archive as a final state (see
  [the scheduler's departures](dag-scheduler.md#departures-from-the-contract)).
* `dag_input_manifests` has a digest function and a minimal write/read (`internal/relay/dag`: `ManifestDigest`, `PutManifest`,
  `ReadManifest`) so the table has an owner now. A manifest is checked for the fields 4.2 requires and for their types (ids and digests are text, a
  generation or revision is a whole number, an artifact or snapshot has its uri, digest and size); whether a base is required depends on the node's
  kind, which a manifest does not carry. A null is a missing value (4.1), so a manifest that writes an absent optional field as null is the same
  record as one that omits it. Building a manifest and judging it fit to release (the blocked paths of contract 4.4) is the scheduler's.

## What a consumer reads

`internal/relay/dag`: `Repo.Snapshot` (the plan at a revision, verified), `Repo.Events` and `Replay` (the log from a cursor), `Repo.Put`, and the
`dag_plans`, `dag_plan_revisions`, `dag_nodes` (live rows: `retired_rev IS NULL`) and `dag_edges` tables. Only a committed revision is in them. A snapshot carries `PlanState` and, on each node,
`Lifecycle` (empty for an active one); `Replay` from a snapshot keeps them.
