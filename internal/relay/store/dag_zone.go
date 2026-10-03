package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// The additive DAG zone (D-01 of the DAG execution contract, docs/relay/dag-plans.md).
//
// A writable open validates the frozen v1 tables first (ValidateOwnershipSchema, which reads only
// relay-sqlite.sql: a store missing one of them is refused and never repaired) and creates the
// v1 objects, and then creates the zone below with the same CREATE ... IF NOT EXISTS statements.
// The zone is not part of relay-sqlite.sql, so it is not part of what an open validates:
//
//   - a store that predates the zone (every store that exists today) opens, keeps every row, and gains
//     the zone;
//   - a runtime without the zone validates only the frozen tables, so it opens a store that carries
//     the zone and never reads or writes it;
//   - SchemaVersion stays "1"; no record is rewritten and no v1 object changes.
//
// A command that declares itself read-only never creates the zone (ReadOnlyCommand): the zone arrives
// with the first write open, and a reader of a store without it finds no plan.
//
// The statements are a LEDGER. The swap gate compares the sqlite_master text of every object a
// store holds with the text a candidate declares (runtime/swapgate), so a shipped statement is
// never edited: a column that a later issue needs arrives as an appended ALTER TABLE ADD COLUMN step
// that every open runs (on a fresh store and on an upgraded one alike, so both read the same text
// afterwards), and a new table arrives as an appended CREATE. testdata/dag_zone_shipped.json holds
// the text each shipped object had when it shipped and the tests fail when one changes. Foreign
// keys point inside the zone only: PRAGMA foreign_keys=ON holds on every connection, and a key into a
// v1 table would make a v1 write depend on a zone row.
//
// What is append-only is enforced here and not only by the writer: triggers abort UPDATE and DELETE
// of a plan and of a revision, and a node or an edge row may change once, from live to retired.

// DAGZoneStatements are the zone's statements in the order an open runs them: what the swap gate
// declares as the zone's schema (swapgate.DeclaredSchema) and what installDAGZone executes.
func DAGZoneStatements() []string { return slices.Clone(dagZone) }

// installDAGZone creates the zone on the writable database db, after the v1 script. Every step is
// idempotent, so a crash between two steps is completed by the next open.
func installDAGZone(ctx context.Context, db *sql.DB) error {
	for i, statement := range dagZone {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize DAG zone (step %d): %w", i+1, err)
		}
	}
	return nil
}

var dagZone = []string{
	// The plan: its identity and the Linear project it belongs to, written once with revision 1. The
	// head revision is MAX(revision_no) of the log, never a column here, so nothing about a plan is updated.
	`CREATE TABLE IF NOT EXISTS dag_plans (
    plan_id            TEXT PRIMARY KEY,
    project_key        TEXT NOT NULL CHECK (project_key <> ''),
    created_by_task_id TEXT NOT NULL,
    created_at         TEXT NOT NULL
)`,

	// The revision log: one row per committed change set, an append-only chain. parent_revision_no is
	// NOT NULL (0 for the first revision) because SQLite treats NULLs as distinct in a UNIQUE, so a nullable
	// parent would admit two first revisions. UNIQUE(plan_id, parent_revision_no) is what keeps a chain
	// from branching; UNIQUE(plan_id, request_id) is what makes a repeated request one revision.
	`CREATE TABLE IF NOT EXISTS dag_plan_revisions (
    plan_id            TEXT NOT NULL REFERENCES dag_plans (plan_id),
    revision_no        INTEGER NOT NULL CHECK (revision_no >= 1),
    parent_revision_no INTEGER NOT NULL,
    request_id         TEXT NOT NULL CHECK (request_id <> ''),
    request_digest     TEXT NOT NULL,
    change_json        TEXT NOT NULL,
    state_digest       TEXT NOT NULL,
    coordinator_epoch  INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    author_task_id     TEXT NOT NULL,
    recorded_at        TEXT NOT NULL,
    PRIMARY KEY (plan_id, revision_no),
    UNIQUE (plan_id, request_id),
    UNIQUE (plan_id, parent_revision_no),
    CHECK (parent_revision_no = revision_no - 1)
)`,

	// The fold of the log, node by node. A row is one VERSION of a node: when a revision changes a node's
	// spec or its incoming edges the old row is retired at that revision and a new row is introduced, so
	// the plan as of any revision is a query (introduced_rev <= R AND (retired_rev IS NULL OR retired_rev > R)).
	// slice_digest covers the node's spec and its incoming edges only (contract 4.2), never the whole plan.
	`CREATE TABLE IF NOT EXISTS dag_nodes (
    plan_id             TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id             TEXT NOT NULL,
    introduced_rev      INTEGER NOT NULL,
    retired_rev         INTEGER,
    slice_digest        TEXT NOT NULL,
    issue_key           TEXT NOT NULL,
    node_kind           TEXT NOT NULL CHECK (node_kind IN ('implementation', 'non_pr')),
    title               TEXT,
    criteria_set_digest TEXT NOT NULL,
    supersedes_node_id  TEXT,
    PRIMARY KEY (plan_id, node_id, introduced_rev),
    FOREIGN KEY (plan_id, introduced_rev) REFERENCES dag_plan_revisions (plan_id, revision_no),
    FOREIGN KEY (plan_id, retired_rev) REFERENCES dag_plan_revisions (plan_id, revision_no),
    CHECK (retired_rev IS NULL OR retired_rev > introduced_rev)
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_nodes_live ON dag_nodes (plan_id, node_id) WHERE retired_rev IS NULL`,

	// The edges of the fold. An edge is never edited (retire it and add another); the CHECKs repeat the
	// edge-integrity rejections of contract 2.4 so a validator bug still cannot store such an edge.
	`CREATE TABLE IF NOT EXISTS dag_edges (
    plan_id            TEXT NOT NULL REFERENCES dag_plans (plan_id),
    edge_id            TEXT NOT NULL,
    introduced_rev     INTEGER NOT NULL,
    retired_rev        INTEGER,
    from_node_id       TEXT NOT NULL,
    to_node_id         TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('artifact_verified', 'integrated', 'decision')),
    target_repository  TEXT,
    target_base_ref    TEXT,
    pins_code_head     INTEGER NOT NULL DEFAULT 0 CHECK (pins_code_head IN (0, 1)),
    decision_subject   TEXT,
    decision_digest    TEXT,
    required_authority TEXT,
    PRIMARY KEY (plan_id, edge_id),
    FOREIGN KEY (plan_id, introduced_rev) REFERENCES dag_plan_revisions (plan_id, revision_no),
    FOREIGN KEY (plan_id, retired_rev) REFERENCES dag_plan_revisions (plan_id, revision_no),
    CHECK (retired_rev IS NULL OR retired_rev > introduced_rev),
    CHECK (from_node_id <> to_node_id),
    CHECK (kind <> 'integrated' OR (target_repository IS NOT NULL AND target_base_ref IS NOT NULL)),
    CHECK (pins_code_head = 0 OR (target_repository IS NOT NULL AND target_base_ref IS NOT NULL)),
    CHECK (kind <> 'decision' OR (decision_subject IS NOT NULL AND decision_digest IS NOT NULL
                                  AND required_authority IS NOT NULL AND required_authority <> '[]'))
)`,
	`CREATE INDEX IF NOT EXISTS dag_edges_incoming ON dag_edges (plan_id, to_node_id, retired_rev)`,

	// What a node consumed (contract 4.2): content-addressed by the manifest digest, which has no plan id,
	// so the same node, slice and inputs in two plans is one row.
	`CREATE TABLE IF NOT EXISTS dag_input_manifests (
    manifest_digest   TEXT PRIMARY KEY,
    node_id           TEXT NOT NULL,
    body_json         TEXT NOT NULL,
    rule_version_json TEXT NOT NULL,
    coordinator_epoch INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    created_at        TEXT NOT NULL
)`,

	// Rows the first writers (CRW-184 and CRW-185) fill; the shapes are the contract's (4.5, 4.3, 2.2,
	// 2.3, 6.2). plan_id is added to every table keyed by a node because node ids are plan-local, and
	// each uniqueness 4.5 states over node_id is scoped by it. Nothing in this package writes them.
	`CREATE TABLE IF NOT EXISTS dag_node_executions (
    plan_id              TEXT NOT NULL,
    node_id              TEXT NOT NULL,
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    manifest_digest      TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('initial', 'correction', 'redefinition', 'parent_handover', 'child_replacement')),
    managed_request_id   TEXT,
    PRIMARY KEY (relationship_id, execution_generation)
)`,
	`CREATE INDEX IF NOT EXISTS dag_node_executions_node ON dag_node_executions (plan_id, node_id)`,
	`CREATE TABLE IF NOT EXISTS dag_releases (
    plan_id            TEXT NOT NULL,
    node_id            TEXT NOT NULL,
    manifest_digest    TEXT NOT NULL,
    managed_request_id TEXT NOT NULL,
    coordinator_epoch  INTEGER NOT NULL CHECK (coordinator_epoch >= 0),
    decided_at         TEXT NOT NULL,
    PRIMARY KEY (plan_id, node_id, manifest_digest)
)`,
	`CREATE TABLE IF NOT EXISTS dag_acceptances (
    acceptance_id            TEXT PRIMARY KEY,
    plan_id                  TEXT NOT NULL,
    node_id                  TEXT NOT NULL,
    manifest_digest          TEXT NOT NULL,
    relationship_id          TEXT NOT NULL,
    execution_generation     INTEGER NOT NULL,
    event_id                 TEXT NOT NULL,
    revision_hash            TEXT NOT NULL,
    criteria_set_digest      TEXT NOT NULL,
    verdict                  TEXT NOT NULL CHECK (verdict = 'verified'),
    head_sha                 TEXT,
    repository               TEXT,
    pr_number                INTEGER,
    output_manifest_ref      TEXT,
    evidence_digest          TEXT,
    ack_tier                 TEXT NOT NULL CHECK (ack_tier <> 'unverified'),
    verdict_turn_id          TEXT NOT NULL,
    rule_version_json        TEXT NOT NULL,
    accepted_by_task_id      TEXT NOT NULL,
    coordinator_epoch        INTEGER NOT NULL CHECK (coordinator_epoch >= 0),
    accepted_at              TEXT NOT NULL,
    supersedes_acceptance_id TEXT,
    state                    TEXT NOT NULL CHECK (state IN ('active', 'superseded', 'revoked'))
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_acceptances_active ON dag_acceptances (plan_id, node_id) WHERE state = 'active'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_acceptances_effect ON dag_acceptances (relationship_id, execution_generation, revision_hash)`,
	`CREATE TABLE IF NOT EXISTS dag_integration_observations (
    observation_id TEXT PRIMARY KEY,
    acceptance_id  TEXT NOT NULL,
    repository     TEXT NOT NULL,
    base_ref       TEXT NOT NULL,
    subject_sha    TEXT NOT NULL,
    tip_sha        TEXT NOT NULL,
    is_ancestor    INTEGER NOT NULL CHECK (is_ancestor IN (0, 1)),
    method         TEXT NOT NULL,
    merge_turn_id  TEXT,
    observed_seq   INTEGER NOT NULL,
    reverted_by    TEXT,
    observed_at    TEXT NOT NULL,
    UNIQUE (acceptance_id, repository, base_ref, observed_seq)
)`,
	`CREATE TABLE IF NOT EXISTS dag_decisions (
    decision_id         TEXT PRIMARY KEY,
    plan_id             TEXT NOT NULL,
    subject             TEXT NOT NULL,
    digest              TEXT NOT NULL,
    disposition         TEXT NOT NULL,
    authority_kind      TEXT NOT NULL,
    authority_ref       TEXT NOT NULL,
    revision            INTEGER NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('active', 'superseded', 'revoked')),
    recorded_by_task_id TEXT NOT NULL,
    coordinator_epoch   INTEGER NOT NULL CHECK (coordinator_epoch >= 0),
    recorded_at         TEXT NOT NULL
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_decisions_active ON dag_decisions (plan_id, subject) WHERE state = 'active'`,
	`CREATE TABLE IF NOT EXISTS dag_coordinator_claims (
    plan_id          TEXT NOT NULL,
    epoch            INTEGER NOT NULL CHECK (epoch >= 1),
    binding_id       TEXT NOT NULL,
    binding_revision INTEGER NOT NULL,
    task_id          TEXT NOT NULL,
    session_nonce    TEXT NOT NULL,
    claimed_at       TEXT NOT NULL,
    PRIMARY KEY (plan_id, epoch)
)`,
	`CREATE TABLE IF NOT EXISTS dag_cap_basis (
    limit_id       TEXT NOT NULL,
    limit_revision INTEGER NOT NULL,
    w_minutes      REAL NOT NULL,
    w_source       TEXT NOT NULL,
    s_minutes      REAL NOT NULL,
    s_source       TEXT NOT NULL,
    decided_by     TEXT NOT NULL,
    decided_at     TEXT NOT NULL,
    PRIMARY KEY (limit_id, limit_revision)
)`,

	// Append-only, enforced where the rows live. A plan and a revision are never updated or deleted; a node
	// or an edge row is never deleted and may change once, from live (retired_rev NULL) to retired. A
	// revision's parent must already exist (parent 0 is the empty plan).
	`CREATE TRIGGER IF NOT EXISTS dag_plans_no_update BEFORE UPDATE ON dag_plans
BEGIN SELECT RAISE(ABORT, 'dag_plans rows are append-only: never updated'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_plans_no_delete BEFORE DELETE ON dag_plans
BEGIN SELECT RAISE(ABORT, 'dag_plans rows are append-only: never deleted'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_plan_revisions_no_update BEFORE UPDATE ON dag_plan_revisions
BEGIN SELECT RAISE(ABORT, 'dag_plan_revisions rows are append-only: never updated'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_plan_revisions_no_delete BEFORE DELETE ON dag_plan_revisions
BEGIN SELECT RAISE(ABORT, 'dag_plan_revisions rows are append-only: never deleted'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_plan_revisions_parent BEFORE INSERT ON dag_plan_revisions
WHEN NEW.parent_revision_no > 0 AND NOT EXISTS (
    SELECT 1 FROM dag_plan_revisions WHERE plan_id = NEW.plan_id AND revision_no = NEW.parent_revision_no)
BEGIN SELECT RAISE(ABORT, 'dag_plan_revisions: the parent revision does not exist'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_nodes_retire_only BEFORE UPDATE ON dag_nodes
WHEN OLD.retired_rev IS NOT NULL OR NEW.retired_rev IS NULL
  OR NEW.plan_id IS NOT OLD.plan_id OR NEW.node_id IS NOT OLD.node_id OR NEW.introduced_rev IS NOT OLD.introduced_rev
  OR NEW.slice_digest IS NOT OLD.slice_digest OR NEW.issue_key IS NOT OLD.issue_key OR NEW.node_kind IS NOT OLD.node_kind
  OR NEW.title IS NOT OLD.title OR NEW.criteria_set_digest IS NOT OLD.criteria_set_digest
  OR NEW.supersedes_node_id IS NOT OLD.supersedes_node_id
BEGIN SELECT RAISE(ABORT, 'dag_nodes rows are immutable except for their one retirement'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_nodes_no_delete BEFORE DELETE ON dag_nodes
BEGIN SELECT RAISE(ABORT, 'dag_nodes rows are never deleted'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_edges_retire_only BEFORE UPDATE ON dag_edges
WHEN OLD.retired_rev IS NOT NULL OR NEW.retired_rev IS NULL
  OR NEW.plan_id IS NOT OLD.plan_id OR NEW.edge_id IS NOT OLD.edge_id OR NEW.introduced_rev IS NOT OLD.introduced_rev
  OR NEW.from_node_id IS NOT OLD.from_node_id OR NEW.to_node_id IS NOT OLD.to_node_id OR NEW.kind IS NOT OLD.kind
  OR NEW.target_repository IS NOT OLD.target_repository OR NEW.target_base_ref IS NOT OLD.target_base_ref
  OR NEW.pins_code_head IS NOT OLD.pins_code_head OR NEW.decision_subject IS NOT OLD.decision_subject
  OR NEW.decision_digest IS NOT OLD.decision_digest OR NEW.required_authority IS NOT OLD.required_authority
BEGIN SELECT RAISE(ABORT, 'dag_edges rows are immutable except for their one retirement'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_edges_no_delete BEFORE DELETE ON dag_edges
BEGIN SELECT RAISE(ABORT, 'dag_edges rows are never deleted'); END`,

	// CRW-184, the scheduler (docs/relay/dag-scheduler.md). Appended statements only: a shipped statement is
	// never edited (the swap gate compares stored text, so an ALTER would read as a changed object), and a table
	// arrives in the change that first queries it (TestDAGZoneEveryTableHasAQueryOrAPendingWriter).
	//
	// dag_merge_checks is the history of what the relay observed of an accepted pull request at merge time: one row
	// per observation that differs from the previous one (check_seq is max+1 per acceptance, "latest" is the highest),
	// so the retry round of a required check, a stale head and an eviction are all readable afterwards.
	// evidence_json is the canonical body checks_digest was taken over, so the digest can be recomputed (B-13).
	`CREATE TABLE IF NOT EXISTS dag_merge_checks (
    check_id             TEXT PRIMARY KEY,
    acceptance_id        TEXT NOT NULL REFERENCES dag_acceptances (acceptance_id),
    check_seq            INTEGER NOT NULL CHECK (check_seq >= 1),
    head_sha             TEXT NOT NULL,
    observed_head_sha    TEXT NOT NULL,
    base_tip_sha         TEXT NOT NULL,
    checks_base_sha      TEXT,
    checks_digest        TEXT NOT NULL,
    evidence_json        TEXT NOT NULL,
    failed_required_json TEXT NOT NULL,
    round_no             INTEGER NOT NULL CHECK (round_no BETWEEN 1 AND 2),
    outcome              TEXT NOT NULL CHECK (outcome IN ('eligible', 'retry_same_sha', 'evicted', 'checks_pending', 'stale_head', 'stale_base', 'stale_criteria', 'predecessor_not_landed')),
    reason               TEXT NOT NULL,
    recorded_at          TEXT NOT NULL,
    UNIQUE (acceptance_id, check_seq)
)`,

	// Re-verification of an accepted output under re-registered criteria (contract E-11). dag_acceptances_effect allows one
	// row per relationship, generation and revision, so the same output cannot be accepted twice; the re-verification is an
	// ordered history here (reval_seq), the latest row is the acceptance's effective criteria digest, and the acceptance keeps its identity.
	`CREATE TABLE IF NOT EXISTS dag_acceptance_revalidations (
    revalidation_id     TEXT PRIMARY KEY,
    acceptance_id       TEXT NOT NULL REFERENCES dag_acceptances (acceptance_id),
    criteria_set_digest TEXT NOT NULL,
    event_id            TEXT NOT NULL,
    verdict_turn_id     TEXT NOT NULL,
    reval_seq           INTEGER NOT NULL CHECK (reval_seq >= 1),
    revalidated_by      TEXT NOT NULL,
    revalidated_at      TEXT NOT NULL,
    UNIQUE (acceptance_id, reval_seq)
)`,

	// The forge identity of an accepted implementation node: dag_acceptances.repository is the target of the edges (a local
	// path is allowed for local ancestry), while the pull request reader needs the owner/name slug and the number.
	`CREATE TABLE IF NOT EXISTS dag_acceptance_forge (
    acceptance_id    TEXT PRIMARY KEY REFERENCES dag_acceptances (acceptance_id),
    forge_repository TEXT NOT NULL CHECK (forge_repository <> ''),
    pr_number        INTEGER NOT NULL CHECK (pr_number >= 1)
)`,

	// dag_passes records every scheduler pass someone asked to keep (dag-ready --record): what was ready, how many slots were free, and which limit
	// decided the order of the candidates it cut. A pass is a fact, never a decision: a duplicate wake records another row and the release path
	// does not read it (release idempotency lives in dag_releases). order_json is the ready node ids in release order, dispositions_json the
	// reading's node list; input_digest is the digest of everything the reading read.
	`CREATE TABLE IF NOT EXISTS dag_passes (
    plan_id           TEXT NOT NULL REFERENCES dag_plans (plan_id),
    pass_seq          INTEGER NOT NULL CHECK (pass_seq >= 1),
    plan_revision     INTEGER NOT NULL,
    input_digest      TEXT NOT NULL,
    ready_count       INTEGER NOT NULL CHECK (ready_count >= 0),
    free_slots        INTEGER NOT NULL CHECK (free_slots >= 0),
    ceiling           INTEGER NOT NULL CHECK (ceiling >= 0),
    held              INTEGER NOT NULL CHECK (held >= 0),
    deciding_limit    TEXT NOT NULL CHECK (deciding_limit IN ('none','no_capacity','edit_overlap','capacity_unmeasured')),
    order_json        TEXT NOT NULL,
    dispositions_json TEXT NOT NULL,
    recorded_by       TEXT NOT NULL,
    recorded_at       TEXT NOT NULL,
    PRIMARY KEY (plan_id, pass_seq)
)`,

	// The edit regions a node declares before it is released (contract 7.2). A declaration is the set of rows sharing one declaration_seq, the
	// latest sequence of a node is its declaration, and a node with no row has none: its regions are unknown and count as overlapping everything.
	// exclusive is set for a rename, a delete and the hotspots (lockfiles, workflow files, schemas), which conflict with any other change in the
	// repository.
	`CREATE TABLE IF NOT EXISTS dag_node_regions (
    plan_id         TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id         TEXT NOT NULL,
    declaration_seq INTEGER NOT NULL CHECK (declaration_seq >= 1),
    repository      TEXT NOT NULL CHECK (repository <> ''),
    path            TEXT NOT NULL CHECK (path <> ''),
    region_kind     TEXT NOT NULL CHECK (region_kind IN ('tree','file','symbol')),
    region_key      TEXT NOT NULL DEFAULT '',
    change          TEXT NOT NULL CHECK (change IN ('edit','rename','delete')),
    exclusive       INTEGER NOT NULL CHECK (exclusive IN (0,1)),
    declared_by     TEXT NOT NULL,
    declared_at     TEXT NOT NULL,
    PRIMARY KEY (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key)
)`,

	// The release of a node, frozen with its intent (dag_releases): the exact managed-start request bytes and the selectors they were fingerprinted
	// with. managed.Start fingerprints the whole request and refuses another body under one request id, while the manifest digest leaves out fields that
	// are still in the prompt, so a replay of an intent whose child was not created sends these bytes and never rebuilds the request.
	`CREATE TABLE IF NOT EXISTS dag_release_requests (
    plan_id         TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id         TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    request_sha256  TEXT NOT NULL,
    request_json    TEXT NOT NULL,
    marker_root     TEXT NOT NULL,
    socket          TEXT NOT NULL,
    state_selector  TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    PRIMARY KEY (plan_id, node_id, manifest_digest)
)`,

	// How many files git cannot merge between the heads of two parallel branches of a plan (git merge-tree --write-tree), recorded for the measurement criterion
	// c7. The nodes are stored in sorted order (left < right, the heads follow them), so asking either way round is one row; nothing in the scheduler's reading
	// waits for it. The unique index makes a repeat of the same branches and base a replay.
	`CREATE TABLE IF NOT EXISTS dag_conflict_observations (
    observation_id TEXT PRIMARY KEY,
    plan_id        TEXT NOT NULL REFERENCES dag_plans (plan_id),
    left_node_id   TEXT NOT NULL,
    right_node_id  TEXT NOT NULL,
    repository     TEXT NOT NULL CHECK (repository <> ''),
    left_head      TEXT NOT NULL,
    right_head     TEXT NOT NULL,
    base_sha       TEXT NOT NULL,
    conflict_count INTEGER NOT NULL CHECK (conflict_count >= 0),
    method         TEXT NOT NULL,
    observed_by    TEXT NOT NULL,
    observed_at    TEXT NOT NULL,
    CHECK (left_node_id < right_node_id)
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_conflict_observations_pair ON dag_conflict_observations (plan_id, left_node_id, right_node_id, left_head, right_head, base_sha)`,
	// CRW-282: how a release whose managed start was released before it created a child is ended and released again. A managed request id that was released is refused forever and dag_releases / dag_release_requests are unique on
	// (plan, node, manifest digest), so the recovery has its own rows. A 'closed' row ends the abandoned intent explicitly (the slot is returned in the same transaction; slot_id, slot_released and copy_path record what
	// that did, so the same close answers the same). A 'rereleased' row is the next release of a digest whose newest intent is closed: it carries the successor request id and the exact request bytes and selectors the managed start is
	// fingerprinted with, as dag_release_requests does for the first one. The node's open intent is the dag_releases row or rereleased row whose request has no closed row.
	`CREATE TABLE IF NOT EXISTS dag_release_recoveries (
    plan_id              TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id              TEXT NOT NULL,
    manifest_digest      TEXT NOT NULL,
    abandoned_request_id TEXT NOT NULL,
    action               TEXT NOT NULL CHECK (action IN ('closed', 'rereleased')),
    successor_request_id TEXT,
    request_sha256       TEXT,
    request_json         TEXT,
    marker_root          TEXT,
    socket               TEXT,
    state_selector       TEXT,
    slot_id              TEXT,
    slot_released        INTEGER NOT NULL DEFAULT 0 CHECK (slot_released IN (0, 1)),
    copy_path            TEXT,
    reason               TEXT NOT NULL CHECK (reason <> ''),
    recorded_by          TEXT NOT NULL,
    coordinator_epoch    INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    recorded_at          TEXT NOT NULL,
    PRIMARY KEY (plan_id, node_id, manifest_digest, abandoned_request_id, action),
    CHECK (action <> 'rereleased' OR (successor_request_id IS NOT NULL AND request_sha256 IS NOT NULL AND request_json IS NOT NULL
                                      AND marker_root IS NOT NULL AND socket IS NOT NULL AND state_selector IS NOT NULL)),
    CHECK (action <> 'closed' OR (successor_request_id IS NULL AND request_sha256 IS NULL AND request_json IS NULL))
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_release_recoveries_successor ON dag_release_recoveries (successor_request_id) WHERE successor_request_id IS NOT NULL`,

	// CRW-283, the project summary outbox (docs/relay/dag-outbox.md). The relay holds no Linear credential: a plan's parent writes its summary with its own connector and this table is
	// the durable, ordered queue it drains. A stream is a plan and a document; seq counts up within it, so the entry with the highest seq is the summary the plan owes the document and
	// every older one is superseded. subject_digest is the decision the entry states (the digest of the progress reading it was made from) and summary_id is the digest of the key
	// (plan, document, plan revision, subject, seq). A claim holds a token (and only a claim does), so a claimant that was overtaken cannot confirm or fail an entry it no longer owns.
	// Only confirmed and superseded are final. The rules below are enforced where the rows live, not only by the writer: an entry is appended pending with the next seq and a plan revision that
	// does not go back, one open entry per stream, legal moves only, nothing claimed or confirmed once a newer entry exists, nothing deleted.
	`CREATE TABLE IF NOT EXISTS dag_summary_outbox (
    summary_id        TEXT PRIMARY KEY CHECK (summary_id <> ''),
    plan_id           TEXT NOT NULL REFERENCES dag_plans (plan_id),
    project_key       TEXT NOT NULL CHECK (project_key <> ''),
    document          TEXT NOT NULL CHECK (document <> ''),
    plan_revision     INTEGER NOT NULL CHECK (plan_revision >= 1),
    seq               INTEGER NOT NULL CHECK (seq >= 1),
    subject_digest    TEXT NOT NULL CHECK (length(subject_digest) = 64),
    state_digest      TEXT NOT NULL,
    summary           TEXT NOT NULL CHECK (summary <> ''),
    summary_sha256    TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('pending', 'claimed', 'confirmed', 'failed', 'superseded')),
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error        TEXT,
    claim_token       TEXT,
    claimed_by        TEXT,
    claimed_at        TEXT,
    readback          TEXT,
    confirmed_at      TEXT,
    enqueued_by       TEXT NOT NULL,
    coordinator_epoch INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE (plan_id, document, seq),
    CHECK ((state = 'claimed') = (claim_token IS NOT NULL)),
    CHECK ((state = 'confirmed') = (confirmed_at IS NOT NULL))
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_summary_outbox_open ON dag_summary_outbox (plan_id, document) WHERE state IN ('pending', 'claimed', 'failed')`,
	`CREATE TRIGGER IF NOT EXISTS dag_summary_outbox_order BEFORE INSERT ON dag_summary_outbox
WHEN NEW.state <> 'pending' OR NEW.attempts <> 0
  OR NEW.seq <> COALESCE((SELECT MAX(seq) FROM dag_summary_outbox WHERE plan_id = NEW.plan_id AND document = NEW.document), 0) + 1
  OR NEW.plan_revision < COALESCE((SELECT MAX(plan_revision) FROM dag_summary_outbox WHERE plan_id = NEW.plan_id AND document = NEW.document), 0)
BEGIN SELECT RAISE(ABORT, 'dag_summary_outbox: an entry is appended pending, with the next sequence number of its stream and a plan revision that does not go back'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_summary_outbox_immutable BEFORE UPDATE ON dag_summary_outbox
WHEN OLD.state IN ('confirmed', 'superseded')
  OR NEW.summary_id IS NOT OLD.summary_id OR NEW.plan_id IS NOT OLD.plan_id OR NEW.project_key IS NOT OLD.project_key OR NEW.document IS NOT OLD.document
  OR NEW.plan_revision IS NOT OLD.plan_revision OR NEW.seq IS NOT OLD.seq OR NEW.subject_digest IS NOT OLD.subject_digest OR NEW.state_digest IS NOT OLD.state_digest
  OR NEW.summary IS NOT OLD.summary OR NEW.summary_sha256 IS NOT OLD.summary_sha256 OR NEW.enqueued_by IS NOT OLD.enqueued_by
  OR NEW.coordinator_epoch IS NOT OLD.coordinator_epoch OR NEW.created_at IS NOT OLD.created_at
BEGIN SELECT RAISE(ABORT, 'dag_summary_outbox entries keep their identity, and a confirmed or superseded entry never changes'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_summary_outbox_transition BEFORE UPDATE ON dag_summary_outbox
WHEN NEW.state <> OLD.state AND NOT (
     (OLD.state = 'pending' AND NEW.state IN ('claimed', 'superseded'))
  OR (OLD.state = 'claimed' AND NEW.state IN ('pending', 'failed', 'confirmed', 'superseded'))
  OR (OLD.state = 'failed' AND NEW.state IN ('pending', 'superseded')))
BEGIN SELECT RAISE(ABORT, 'dag_summary_outbox: that is not a legal move of an entry'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_summary_outbox_newest BEFORE UPDATE ON dag_summary_outbox
WHEN NEW.state IN ('claimed', 'confirmed')
  AND EXISTS (SELECT 1 FROM dag_summary_outbox WHERE plan_id = NEW.plan_id AND document = NEW.document AND seq > NEW.seq)
BEGIN SELECT RAISE(ABORT, 'dag_summary_outbox: an older summary is never claimed or confirmed over a newer one'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_summary_outbox_no_delete BEFORE DELETE ON dag_summary_outbox
BEGIN SELECT RAISE(ABORT, 'dag_summary_outbox entries are never deleted'); END`,

	// CRW-409: the grade of a declared edit region. A side table of dag_node_regions keyed like it, appended because a shipped statement is never edited (an ALTER TABLE ADD COLUMN would
	// rewrite the shipped text of dag_node_regions): a declaration made before grades existed has no row here and reads as independent. A mechanical grade names the rule that settles its
	// overlaps (union, renumber or regenerate:<command>) and no other grade names one.
	`CREATE TABLE IF NOT EXISTS dag_node_region_grades (
    plan_id         TEXT NOT NULL,
    node_id         TEXT NOT NULL,
    declaration_seq INTEGER NOT NULL CHECK (declaration_seq >= 1),
    repository      TEXT NOT NULL,
    path            TEXT NOT NULL,
    region_kind     TEXT NOT NULL,
    region_key      TEXT NOT NULL DEFAULT '',
    grade           TEXT NOT NULL CHECK (grade IN ('independent','mechanical','local','exclusive')),
    rule            TEXT NOT NULL DEFAULT '',
    CHECK ((grade = 'mechanical' AND (rule IN ('union','renumber') OR rule GLOB 'regenerate:?*')) OR (grade <> 'mechanical' AND rule = '')),
    PRIMARY KEY (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key),
    FOREIGN KEY (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key) REFERENCES dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key)
)`,

	// CRW-409: the files git could not merge between the heads of an observation (dag_conflict_observations keeps only how many), under each name the observed checkout is known by: its path
	// with links resolved and, when its origin remote names owner/name on the forge the relay talks to, that slug. A region is matched on the repository name it was declared with.
	`CREATE TABLE IF NOT EXISTS dag_conflict_observation_files (
    observation_id TEXT NOT NULL REFERENCES dag_conflict_observations (observation_id),
    repository     TEXT NOT NULL CHECK (repository <> ''),
    path           TEXT NOT NULL CHECK (path <> ''),
    PRIMARY KEY (observation_id, repository, path)
)`,

	// CRW-410: how a head conflicts with the tip of the branch it lands on, per node (dag_conflict_observations is for two nodes and a tip is not one). A node's head, the tip and their merge base are
	// one row; head_source says where the head came from (the operator's word, the node's current acceptance, or the child's own checkout). The files git could not merge are in the table after it, under
	// each name the observed checkout is known by, as dag_conflict_observation_files keeps them.
	`CREATE TABLE IF NOT EXISTS dag_tip_conflict_observations (
    observation_id TEXT PRIMARY KEY,
    plan_id        TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id        TEXT NOT NULL CHECK (node_id <> ''),
    repository     TEXT NOT NULL CHECK (repository <> ''),
    head           TEXT NOT NULL,
    head_source    TEXT NOT NULL CHECK (head_source IN ('explicit','acceptance','child_checkout')),
    tip_ref        TEXT NOT NULL,
    tip_sha        TEXT NOT NULL,
    base_sha       TEXT NOT NULL,
    conflict_count INTEGER NOT NULL CHECK (conflict_count >= 0),
    method         TEXT NOT NULL,
    observed_by    TEXT NOT NULL,
    observed_at    TEXT NOT NULL,
    UNIQUE (plan_id, node_id, head, tip_sha, base_sha)
)`,
	`CREATE TABLE IF NOT EXISTS dag_tip_conflict_observation_files (
    observation_id TEXT NOT NULL REFERENCES dag_tip_conflict_observations (observation_id),
    repository     TEXT NOT NULL CHECK (repository <> ''),
    path           TEXT NOT NULL CHECK (path <> ''),
    PRIMARY KEY (observation_id, repository, path)
)`,

	// CRW-410: declaration drift. A node whose declared regions do not cover a path it conflicted on (with another node's head, or with the tip) has a row for that path, written with the observation. observation_id
	// names a row of dag_conflict_observations (prefix dco-) or of dag_tip_conflict_observations (prefix dto-), so it carries no foreign key.
	`CREATE TABLE IF NOT EXISTS dag_conflict_drift (
    observation_id TEXT NOT NULL CHECK (observation_id <> ''),
    node_id        TEXT NOT NULL CHECK (node_id <> ''),
    path           TEXT NOT NULL CHECK (path <> ''),
    PRIMARY KEY (observation_id, node_id, path)
)`,

	// CRW-410: the sweep ledger. One row per sweep, as dag_passes keeps one per pass: what prompted it (a landing, a receipt, or a command), the node and the reference it rested on (the landing's integration observation id,
	// the accepted receipt's acceptance id) and the checkout it measured in. The partial unique index makes the sweep of one landing or one acceptance a single row, so a second writer of the same trigger finds the first; a manual
	// sweep repeats freely.
	`CREATE TABLE IF NOT EXISTS dag_conflict_sweeps (
    plan_id      TEXT NOT NULL REFERENCES dag_plans (plan_id),
    sweep_seq    INTEGER NOT NULL CHECK (sweep_seq >= 1),
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('landing','receipt','manual')),
    trigger_node TEXT NOT NULL DEFAULT '',
    trigger_ref  TEXT NOT NULL DEFAULT '',
    repository   TEXT NOT NULL CHECK (repository <> ''),
    observed_by  TEXT NOT NULL,
    observed_at  TEXT NOT NULL,
    PRIMARY KEY (plan_id, sweep_seq)
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dag_conflict_sweeps_trigger ON dag_conflict_sweeps (plan_id, trigger_kind, trigger_ref) WHERE trigger_kind <> 'manual' AND trigger_ref <> ''`,

	// CRW-410: what each sweep measured. A member is a pair of nodes (stored in sorted order, as dag_conflict_observations stores them) or one node against a tip, and it was observed (a new observation row), replayed (the same heads
	// and base were recorded before: the existing row) or left unmeasured with a reason from the closed set. The member rows are what orders the measurements of a pair: the highest sweep_seq is the latest one, even when the
	// observation behind it is an old row that the same heads replayed.
	`CREATE TABLE IF NOT EXISTS dag_conflict_sweep_members (
    plan_id           TEXT NOT NULL,
    sweep_seq         INTEGER NOT NULL,
    member_seq        INTEGER NOT NULL CHECK (member_seq >= 1),
    kind              TEXT NOT NULL CHECK (kind IN ('pair','tip')),
    left_node_id      TEXT NOT NULL CHECK (left_node_id <> ''),
    right_node_id     TEXT NOT NULL DEFAULT '',
    left_head         TEXT NOT NULL DEFAULT '',
    right_head        TEXT NOT NULL DEFAULT '',
    left_head_source  TEXT NOT NULL DEFAULT '' CHECK (left_head_source IN ('','explicit','acceptance','child_checkout')),
    right_head_source TEXT NOT NULL DEFAULT '' CHECK (right_head_source IN ('','explicit','acceptance','child_checkout')),
    status            TEXT NOT NULL CHECK (status IN ('observed','replayed','unmeasured')),
    reason            TEXT NOT NULL DEFAULT '' CHECK (reason IN ('','head_unknown','checkout_mismatch','commit_missing','no_common_ancestor','tip_unreadable')),
    observation_id    TEXT NOT NULL DEFAULT '',
    conflicts         INTEGER NOT NULL DEFAULT 0 CHECK (conflicts >= 0),
    CHECK ((status = 'unmeasured') = (reason <> '')),
    CHECK ((status = 'unmeasured') = (observation_id = '')),
    CHECK ((kind = 'pair' AND right_node_id <> '' AND left_node_id < right_node_id) OR (kind = 'tip' AND right_node_id = '')),
    PRIMARY KEY (plan_id, sweep_seq, member_seq),
    FOREIGN KEY (plan_id, sweep_seq) REFERENCES dag_conflict_sweeps (plan_id, sweep_seq)
)`,

	// CRW-431: whether the declarer stated that a declared edit region holds the whole repository. A side table of dag_node_regions like dag_node_region_grades, appended because a shipped statement is never
	// edited: dag_node_regions.exclusive is set for a rename, a delete and a hotspot as well as for the declarer's word, so it cannot say which. Every region of a declaration made since this table has a row, stated
	// 1 when the declarer said it and 0 when not; a declaration made before has none, which is how the scheduler tells the two apart (dagsched.loadDeclarations).
	`CREATE TABLE IF NOT EXISTS dag_node_region_holds (
    plan_id         TEXT NOT NULL,
    node_id         TEXT NOT NULL,
    declaration_seq INTEGER NOT NULL CHECK (declaration_seq >= 1),
    repository      TEXT NOT NULL,
    path            TEXT NOT NULL,
    region_kind     TEXT NOT NULL,
    region_key      TEXT NOT NULL DEFAULT '',
    stated          INTEGER NOT NULL CHECK (stated IN (0,1)),
    PRIMARY KEY (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key),
    FOREIGN KEY (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key) REFERENCES dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key)
)`,
	// CRW-430: a base refresh of an accepted node. The acceptance stays as it is (its id is the digest of its head, generation, event and revision, and every node that consumed it names that id), and this
	// record says the same acceptance also stands on a LATER generation of its relationship whose head the relay proved to differ from the accepted head only by merges of the base branch (proof_json: the
	// chain of merge commits, each with the tree git merges from its parents, and the files a hand resolved conflict touched). Integration is judged on the newest valid record's head and generation; the table is
	// append-only, and the row digest in refresh_id lets a reader ignore a row that was written by hand.
	`CREATE TABLE IF NOT EXISTS dag_base_refreshes (
    refresh_id           TEXT PRIMARY KEY CHECK (refresh_id <> ''),
    acceptance_id        TEXT NOT NULL REFERENCES dag_acceptances (acceptance_id),
    refresh_seq          INTEGER NOT NULL CHECK (refresh_seq >= 1),
    relationship_id      TEXT NOT NULL CHECK (relationship_id <> ''),
    execution_generation INTEGER NOT NULL CHECK (execution_generation >= 1),
    event_id             TEXT NOT NULL CHECK (event_id <> ''),
    revision_hash        TEXT NOT NULL CHECK (revision_hash <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    base_repository      TEXT NOT NULL CHECK (base_repository <> ''),
    base_ref             TEXT NOT NULL CHECK (base_ref <> ''),
    base_tip_sha         TEXT NOT NULL CHECK (base_tip_sha <> ''),
    proof_json           TEXT NOT NULL CHECK (proof_json <> ''),
    resolved_paths_json  TEXT NOT NULL,
    recorded_by_task_id  TEXT NOT NULL,
    coordinator_epoch    INTEGER NOT NULL CHECK (coordinator_epoch >= 0),
    recorded_at          TEXT NOT NULL,
    UNIQUE (acceptance_id, refresh_seq),
    UNIQUE (acceptance_id, event_id, head_sha)
)`,
	`CREATE TRIGGER IF NOT EXISTS dag_base_refreshes_no_update BEFORE UPDATE ON dag_base_refreshes
BEGIN SELECT RAISE(ABORT, 'dag_base_refreshes rows are append-only: never updated'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_base_refreshes_no_delete BEFORE DELETE ON dag_base_refreshes
BEGIN SELECT RAISE(ABORT, 'dag_base_refreshes rows are append-only: never deleted'); END`,

	// CRW-411: the release policy of a plan, the results the parent records for landed and discarded work, and the policy a recorded pass kept. All three are side tables, appended because a shipped statement is never
	// edited. dag_release_policy is the ledger of the values the scheduler reads when it decides whether local-optimistic release stays on (the last window_size landings, a landing is slow above handling_seconds, red_merges
	// red or reverted landings in the window switch it off, clean_run clean landings in a row switch it on again); the latest policy_seq of a plan is in force and a plan with no row has no policy. dag_landing_results
	// holds what the parent states about a node's pull request after the fact (dev_green, dev_red, reverted) or about the work itself (duplicate, discarded); the id is a digest of what is stated, so the same statement
	// again is one row. dag_pass_release_policy is the policy state a recorded pass saw, written only for a plan that has a policy.
	`CREATE TABLE IF NOT EXISTS dag_release_policy (
    plan_id          TEXT NOT NULL REFERENCES dag_plans (plan_id),
    policy_seq       INTEGER NOT NULL CHECK (policy_seq >= 1),
    window_size      INTEGER NOT NULL CHECK (window_size BETWEEN 1 AND 64),
    handling_seconds INTEGER NOT NULL CHECK (handling_seconds >= 1),
    red_merges       INTEGER NOT NULL CHECK (red_merges >= 1),
    clean_run        INTEGER NOT NULL CHECK (clean_run >= 1),
    recorded_by      TEXT NOT NULL CHECK (recorded_by <> ''),
    coordinator_epoch INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    recorded_at      TEXT NOT NULL,
    CHECK (red_merges <= window_size AND clean_run <= window_size),
    PRIMARY KEY (plan_id, policy_seq)
)`,
	`CREATE TABLE IF NOT EXISTS dag_landing_results (
    result_id   TEXT PRIMARY KEY,
    plan_id     TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id     TEXT NOT NULL CHECK (node_id <> ''),
    kind        TEXT NOT NULL CHECK (kind IN ('dev_green','dev_red','reverted','duplicate','discarded')),
    commit_sha  TEXT NOT NULL DEFAULT '',
    evidence    TEXT NOT NULL CHECK (evidence <> ''),
    recorded_by TEXT NOT NULL CHECK (recorded_by <> ''),
    coordinator_epoch INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    recorded_at TEXT NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS dag_landing_results_node ON dag_landing_results (plan_id, node_id)`,
	`CREATE TABLE IF NOT EXISTS dag_pass_release_policy (
    plan_id     TEXT NOT NULL,
    pass_seq    INTEGER NOT NULL,
    policy_json TEXT NOT NULL,
    PRIMARY KEY (plan_id, pass_seq),
    FOREIGN KEY (plan_id, pass_seq) REFERENCES dag_passes (plan_id, pass_seq)
)`,
	// CRW-446: the withdrawal of a generation the coordinator opened by hand and never bound or sent to the child. The generations row stays (a number is never reused: the next generation takes the number
	// after the highest one the relationship ever held); this record says the generation is closed and which generation the relationship stands on again. It is append-only, one row per withdrawn generation.
	`CREATE TABLE IF NOT EXISTS dag_generation_withdrawals (
    relationship_id      TEXT NOT NULL CHECK (relationship_id <> ''),
    execution_generation INTEGER NOT NULL CHECK (execution_generation >= 2),
    plan_id              TEXT NOT NULL REFERENCES dag_plans (plan_id),
    node_id              TEXT NOT NULL CHECK (node_id <> ''),
    dispatch_request_id  TEXT NOT NULL CHECK (dispatch_request_id <> ''),
    opened_reason        TEXT NOT NULL,
    restored_generation  INTEGER NOT NULL CHECK (restored_generation >= 1),
    reason               TEXT NOT NULL CHECK (reason <> ''),
    withdrawn_by_task_id TEXT NOT NULL CHECK (withdrawn_by_task_id <> ''),
    coordinator_epoch    INTEGER NOT NULL DEFAULT 0 CHECK (coordinator_epoch >= 0),
    withdrawn_at         TEXT NOT NULL,
    PRIMARY KEY (relationship_id, execution_generation),
    CHECK (restored_generation < execution_generation)
)`,
	`CREATE TRIGGER IF NOT EXISTS dag_generation_withdrawals_no_update BEFORE UPDATE ON dag_generation_withdrawals
BEGIN SELECT RAISE(ABORT, 'dag_generation_withdrawals rows are append-only: never updated'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_generation_withdrawals_no_delete BEFORE DELETE ON dag_generation_withdrawals
BEGIN SELECT RAISE(ABORT, 'dag_generation_withdrawals rows are append-only: never deleted'); END`,
	// CRW-468: the host memory bound a recorded pass saw (dag-ready --record), a side table because a shipped statement is never edited and dag_passes.deciding_limit has a CHECK of four values: state is the
	// verdict (within, deferring, unmeasured), reading_limit the limit the reading decided by (host_memory among the five), host_json the object pass.host_memory prints (the sample, the limits and where they
	// came from). A row exists for every recorded pass of a scheduler that carried the bound.
	`CREATE TABLE IF NOT EXISTS dag_pass_host_memory (
    plan_id       TEXT NOT NULL,
    pass_seq      INTEGER NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('within','deferring','unmeasured')),
    reading_limit TEXT NOT NULL CHECK (reading_limit IN ('none','no_capacity','edit_overlap','capacity_unmeasured','host_memory')),
    host_json     TEXT NOT NULL,
    PRIMARY KEY (plan_id, pass_seq),
    FOREIGN KEY (plan_id, pass_seq) REFERENCES dag_passes (plan_id, pass_seq)
)`,
}
