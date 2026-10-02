# DAG plans

A DAG plan is the relay's record of how a project's issues depend on one another: which deliverables (nodes) exist, and
what must be true of one before another may start (edges). This page is the normative description of how a plan is
defined, validated, stored and read. It does not describe how a plan is run: choosing what is ready, releasing a node,
accepting its result and observing integration belong to the scheduler that reads these plans. The model follows the
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
| `coordinator_epoch` | optional, a whole number, recorded as given. It fences nothing in this version (decisions D-07 and CRW-185) |
| `changes` | the typed changes, 1 to 256 |

A node is `{node_id, issue_key, kind, criteria_set_digest, title?}` (`criteria_set_digest` is a sha256 in lowercase hex).
An edge is `{edge_id, from_node_id, to_node_id, kind}` plus, by kind: `target_repository`, `target_base_ref`, `pins_code_head`
(artifact_verified, integrated); `decision_subject`, `decision_digest`, `required_authority` (decision). `required_authority`
entries are opaque text: the format of an authority is undecided (D-09) and is not invented here.

## What is rejected, before anything is written

A revision is validated as the plan it would produce, so every rule is judged on the whole graph. All violations are
collected and reported together, in a fixed order, each as `[rule] path: why`; the same document against the same parent
always answers the same way. A rejected revision writes nothing.

| Rule | Rejected when |
| --- | --- |
| `empty_changes`, `empty_graph` | the revision carries no change; the plan would hold no node |
| `duplicate_node_id`, `duplicate_edge_id` | an id is added twice, is already in the plan, or was retired (ids are never reused) |
| `unknown_node`, `unknown_edge`, `conflicting_changes` | a change addresses a node or edge the parent plan does not hold, or two changes of one revision address the same one |
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

## The log

Revisions are appended, never changed. `dag-plan-put` is one store transaction (BEGIN IMMEDIATE, the store's atomic contract):

1. a `request_id` the plan has already recorded returns that revision (`replayed: true`) when the request is the same one,
   however far the plan has moved on, and is `plan_revision_conflict` when it is another request;
2. an `expected_parent_revision` that is not the plan's head is `plan_revision_conflict`: the writer lost a race, re-read the plan and decide again;
3. the changes are folded onto the head and the result is validated;
4. only then is anything written: the plan's header (first revision), the revision row, and the rows of the fold.

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
once it finds it in the log, because that request may have committed between two of its reads.

## Commands

All three are relay commands (`codex-session-relay [--state DIR] <command>`); output is JSON on stdout.

| Command | Does | Exit |
| --- | --- | --- |
| `dag-plan-put --request <json or @file>` | appends a revision; answers `{ok, replayed, plan_id, project_key, revision_no, parent_revision_no, request_id, request_digest, state_digest, coordinator_epoch, author_task_id, recorded_at, node_digests}` | 0; 2 refused (`malformed_receipt` for a rejected plan, `plan_revision_conflict`); 3 host; 4 a document that is not JSON or cannot be read |
| `dag-plan-show --plan P [--revision N] [--verify]` | the plan as of a revision. `nodes` and `edges` are the top-level lists of the answer, with `schema`, `revision_no`, `head_revision_no`, `state_digest`, `digests_verified`, `log_verified` | 0; 2 `unregistered_scope`; 3 corrupt |
| `dag-plan-log --plan P [--after C] [--limit N]` | the events after a cursor with their typed changes, and the `cursor` to resume from | 0; 2 `unregistered_scope` |

`dag-plan-put` validates the document, and the plan it would produce when that needs no store, before it opens the store for
writing. An invalid first revision, or a revision that names a parent no plan has, on a state directory with no store creates no
store; against a store it reads in place (no sidecar is created) and refuses before the writing open when the parent is stale or the
changes break a rule. A store that cannot be read that way is the host's failure and is not handed on to the writing open. Reads never
create the zone.

### Refusal reasons

The relay's exit-code contract is unchanged except for one reason. A rejected plan reuses `malformed_receipt` (the relay's reason for a
structured document that is malformed, as the linkage envelope uses it), an unknown plan or revision reuses `unregistered_scope`, and
**`plan_revision_conflict`** is new: a plan write that lost to another writer (a stale parent) or that reuses a request id for a
different request (decision D-02). A stale coordinator epoch is not a reason here, because nothing fences by epoch yet.

## The store

The log lives in the relay's host-shared store, in tables the writable open creates after it has validated the frozen v1
tables (decision D-01):

* the open validates only the tables of the frozen v1 schema (`relay-sqlite.sql`) and refuses a store missing one of them, as before;
  it then creates the zone with `CREATE ... IF NOT EXISTS`. A store that predates the zone opens, keeps every row, and gains it;
  a runtime without the zone validates only the frozen tables, so it opens a store that has it and never reads or writes it.
  `SchemaVersion` stays `1`; no existing table or column changes;
* the zone is the twelve `dag_*` tables of the contract: `dag_plans`, `dag_plan_revisions`, `dag_nodes`, `dag_edges`,
  `dag_input_manifests`, and the tables whose writers come with the scheduler (`dag_node_executions`, `dag_releases`,
  `dag_acceptances`, `dag_integration_observations`, `dag_decisions`, `dag_coordinator_claims`, `dag_cap_basis`). Node-keyed
  tables carry `plan_id`, so node ids need only be unique within a plan;
* a command that declares itself read-only never creates the zone: it arrives with the first write open;
* the zone is an append-only ledger of statements (`internal/relay/store/dag_zone.go`). A shipped statement is never edited; a column a
  later issue needs arrives as an appended `ALTER TABLE ... ADD COLUMN` that every open runs, so a fresh store and an upgraded store read the
  same text. `testdata/dag_zone_shipped.json` holds the text each object had when it shipped.

**The schema gate.** The swap gate compares the text of the objects a store holds with the text a candidate declares
([runtime installation](../runtime-install.md#why-the-schema-reading-compares-statements-and-not-versions)). This build declares the zone, so
installing it onto a store that has no zone reads `EXTENDS`, and installing a build without the zone onto a store this build has opened reads
`NARROWS`; both are refusals under OPS-4.5 and each is its own decision with a copy of the state directory first. Opening a store with this
build for writing is itself a schema change, so an installed relay of this version is the one that creates the zone. Making the gate aware of an
additive zone is a follow-up decision, not made here.

## Where this page reads the contract

* Contract 2.4 states three rejections. This page applies them, and also reads section 1.2 (which lists the edges each kind of node can
  start) as the rules `output_decision_needs_non_pr` and `edge_field_not_applicable`: a `decision` edge leaves a `non_pr` node, and a
  `pins_code_head` or a target belongs to an edge out of an implementation node. They are separate rules so one can be relaxed without
  touching the others.
* Contract 4.5 lists the tables by `node_id`. Node ids are plan-local here, so each key that names a node is scoped by `plan_id`. The input
  manifest has no plan id by contract 4.1, so `dag_input_manifests` has exactly the contract's columns and a manifest is content-addressed: the
  same consumption in two plans is one row.
* `dag_plans` (the plan's identity and project) is an addition to the contract's table list.
* `dag_input_manifests` has a digest function and a minimal write/read (`internal/relay/dag`: `ManifestDigest`, `PutManifest`,
  `ReadManifest`) so the table has an owner now. A manifest is checked for the fields 4.2 requires and for their types (ids and digests are text, a
  generation or revision is a whole number, an artifact or snapshot has its uri, digest and size); whether a base is required depends on the node's
  kind, which a manifest does not carry. A null is a missing value (4.1), so a manifest that writes an absent optional field as null is the same
  record as one that omits it. Building a manifest and judging it fit to release (the blocked paths of contract 4.4) is the scheduler's.

## What a consumer reads

`internal/relay/dag`: `Repo.Snapshot` (the plan at a revision, verified), `Repo.Events` and `Replay` (the log from a cursor), `Repo.Put`, and the
`dag_plans`, `dag_plan_revisions`, `dag_nodes` (live rows: `retired_rev IS NULL`) and `dag_edges` tables. Only a committed revision is in them.
