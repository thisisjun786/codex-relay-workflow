-- SCHEMA_VERSION = 1
-- Observed PRAGMAs after opening a temporary Store
-- PRAGMA journal_mode=wal
-- PRAGMA synchronous=2
-- PRAGMA foreign_keys=1

CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS relationships (
    relationship_id     TEXT PRIMARY KEY,
    issue_key           TEXT NOT NULL,
    status              TEXT NOT NULL,
    parent_task_id      TEXT NOT NULL,
    parent_host_id      TEXT NOT NULL,
    parent_cwd          TEXT,
    parent_cxc_session  TEXT,
    child_task_id       TEXT NOT NULL,
    child_host_id       TEXT NOT NULL,
    child_cwd           TEXT,
    child_cxc_session   TEXT,
    execution_generation INTEGER NOT NULL,
    artifact_roots      TEXT NOT NULL,
    allowed_recipients  TEXT NOT NULL,
    scope_ref           TEXT,
    supersedes          TEXT,
    superseded_by       TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS generations (
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    dispatch_request_id  TEXT NOT NULL,
    anchor_state         TEXT NOT NULL,
    dispatch_turn_id     TEXT,
    reason               TEXT,
    opened_at            TEXT NOT NULL,
    bound_at             TEXT,
    PRIMARY KEY (relationship_id, execution_generation),
    UNIQUE (relationship_id, dispatch_request_id)
);

CREATE TABLE IF NOT EXISTS events (
    event_id             TEXT PRIMARY KEY,
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    revision_hash        TEXT NOT NULL,
    outcome              TEXT NOT NULL,
    producer             TEXT NOT NULL,
    attempt              INTEGER,
    turn_thread_id       TEXT NOT NULL,
    turn_id              TEXT NOT NULL,
    turn_status          TEXT NOT NULL,
    receipt              TEXT NOT NULL,
    manifest_ref         TEXT,
    path_binding_mode    TEXT,
    -- A child emitting from inside its own turn can only observe inProgress, so its claim
    -- is STAGED. Only an independent observation of that turn ending normally makes it
    -- final and therefore deliverable; a failed or interrupted ending suppresses it.
    stage                TEXT NOT NULL DEFAULT 'final',
    staged_at            TEXT,
    finalized_at         TEXT,
    finalizing_status    TEXT,
    suppressed_reason    TEXT,
    first_seen_at        TEXT NOT NULL,
    last_seen_at         TEXT NOT NULL,
    observation_count    INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS observations (
    thread_id       TEXT NOT NULL,
    turn_id         TEXT NOT NULL,
    terminal_status TEXT NOT NULL,
    relationship_id TEXT,
    classification  TEXT NOT NULL,
    event_id        TEXT,
    observed_at     TEXT NOT NULL,
    PRIMARY KEY (thread_id, turn_id, terminal_status)
);

-- Which ASSIGNMENT has settled a turn, which observations cannot answer: its key is the
-- turn alone, so when two assignments share a child turn only the first records a row and
-- every other one looks permanently unsettled. Kept as a separate table rather than by
-- re-keying observations, because this store has no migration path and an existing database
-- would silently keep the old key. New databases and old ones both gain this on open.
CREATE TABLE IF NOT EXISTS assignment_settlements (
    relationship_id TEXT NOT NULL,
    thread_id       TEXT NOT NULL,
    turn_id         TEXT NOT NULL,
    terminal_status TEXT NOT NULL,
    settled_at      TEXT NOT NULL,
    PRIMARY KEY (relationship_id, thread_id, turn_id, terminal_status)
);

CREATE TABLE IF NOT EXISTS refusals (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    at              TEXT NOT NULL,
    relationship_id TEXT,
    event_id        TEXT,
    reason          TEXT NOT NULL,
    detail          TEXT,
    payload         TEXT
);

CREATE TABLE IF NOT EXISTS deliveries (
    event_id            TEXT PRIMARY KEY,
    relationship_id     TEXT NOT NULL,
    kind                TEXT NOT NULL,
    recipient_task_id   TEXT NOT NULL,
    recipient_thread_id TEXT NOT NULL,
    state               TEXT NOT NULL,
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    next_eligible_at    REAL,
    hold_reason         TEXT,
    lease_owner         TEXT,
    lease_until         REAL,
    dispatch_evidence   TEXT,
    dispatch_turn_id    TEXT,
    provenance          TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS attempts (
    request_id            TEXT PRIMARY KEY,
    event_id              TEXT NOT NULL,
    attempt_no            INTEGER NOT NULL,
    kind                  TEXT NOT NULL,
    internal_state        TEXT NOT NULL,
    state                 TEXT,
    record                TEXT,
    sealed                INTEGER NOT NULL DEFAULT 0,
    operation_observation TEXT,
    recipient_scan        TEXT,
    affirmative_evidence  TEXT,
    reconciled_at         TEXT,
    -- When the send was STARTED, not when it settled. An acknowledging turn is compared
    -- against this, because a slow transport response would otherwise make the dispatch turn
    -- itself look older than its own delivery.
    sent_at               TEXT,
    observed_at           TEXT NOT NULL,
    UNIQUE (event_id, attempt_no)
);

-- The exact bytes sent for one attempt, frozen when that attempt was allocated.
--
-- A separate table rather than a column on attempts, because the schema is applied with
-- CREATE TABLE IF NOT EXISTS on every open: that adds a table to an existing store but it
-- would never add a column. An attempt predating this table therefore reports its bytes as
-- unavailable, which is the truth, instead of being re-rendered into a plausible guess.
CREATE TABLE IF NOT EXISTS attempt_messages (
    request_id  TEXT PRIMARY KEY,
    event_id    TEXT NOT NULL,
    attempt_no  INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    message     TEXT NOT NULL,
    rendered_at TEXT NOT NULL,
    UNIQUE (event_id, attempt_no)
);

-- The execution settings a task was actually created with, as reported by the host at creation
-- and recorded by whoever registered the relationship. This is what JUN-92 populates from Run's
-- creation result; it is not a separate handshake and asks for nothing new from the host.
CREATE TABLE IF NOT EXISTS authorized_settings (
    task_id     TEXT PRIMARY KEY,
    settings    TEXT NOT NULL,
    source      TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

-- A settings violation learned about a dispatch that already reached a turn. The canonical
-- delivery state stays 'dispatched': reconcile treats only that as receipt-based delivery and
-- ack admits only that or inbox_only, so reclassifying a real delivery would strip it of the
-- recovery it most needs. This annotates the dispatch; it never replaces its state.
CREATE TABLE IF NOT EXISTS attempt_settings_violations (
    request_id  TEXT PRIMARY KEY,
    event_id    TEXT NOT NULL,
    findings    TEXT NOT NULL,
    observed_at TEXT NOT NULL
);

-- Which revision declares which predecessor, inside one generation, as STATED by the child in
-- its own emit. Nothing here is an ordering by arrival: a declaration is an owner assertion
-- about lineage, and where the declared graph is ambiguous the head is ambiguous too.
CREATE TABLE IF NOT EXISTS revision_lineage (
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    event_id             TEXT NOT NULL,
    revision_hash        TEXT NOT NULL,
    supersedes_hash      TEXT,
    declared_by          TEXT NOT NULL,
    recorded_at          TEXT NOT NULL,
    PRIMARY KEY (relationship_id, execution_generation, event_id)
);

-- The canonical criteria a relationship is judged against. A table rather than fields on the
-- verdict, because verification-verdict.json is frozen with additionalProperties false.
CREATE TABLE IF NOT EXISTS canonical_criteria (
    relationship_id TEXT NOT NULL,
    criterion_id    TEXT NOT NULL,
    title           TEXT NOT NULL,
    required        INTEGER NOT NULL DEFAULT 1,
    source_ref      TEXT,
    set_digest      TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    PRIMARY KEY (relationship_id, criterion_id)
);

-- managed or legacy, stored rather than inferred from whether a set exists, because an absent
-- set on a managed assignment is exactly the case that must refuse a verified completion.
CREATE TABLE IF NOT EXISTS verification_mode (
    relationship_id TEXT PRIMARY KEY,
    mode            TEXT NOT NULL,
    recorded_at     TEXT NOT NULL
);

-- The criteria set as it stood when the review STARTED. verification_claims cannot carry it:
-- this schema is applied with CREATE TABLE IF NOT EXISTS, which adds a table to an existing
-- store but never a column.
CREATE TABLE IF NOT EXISTS claim_context (
    event_id   TEXT PRIMARY KEY,
    set_digest TEXT,
    bound_at   TEXT NOT NULL
);

-- Everything a verdict establishes that contract v1 has no room for. The record in
-- verdicts.record stays exactly what the frozen schema allows.
CREATE TABLE IF NOT EXISTS verdict_context (
    event_id      TEXT PRIMARY KEY,
    set_digest    TEXT,
    coverage      TEXT NOT NULL,
    findings      TEXT,
    reason        TEXT,
    currency      TEXT NOT NULL,
    head_event_id TEXT,
    head_revision TEXT,
    ack_evidence  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL
);

-- What a delivered message leads with: which pull request this is about, which commit it is
-- about, what was checked, what is still open and what to do next. A table rather than fields
-- on the receipt, because completion-receipt.json is frozen with additionalProperties false.
-- Bound to the event, generation and revision it describes, so a later push cannot inherit an
-- earlier report, and the repository is stored beside the number so the same pull request
-- number on two projects stays two pull requests.
CREATE TABLE IF NOT EXISTS work_reports (
    event_id             TEXT NOT NULL,
    submission_no        INTEGER NOT NULL DEFAULT 1,
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    revision_hash        TEXT NOT NULL,
    repository           TEXT NOT NULL,
    pr_number            INTEGER,
    pr_url               TEXT,
    pr_state             TEXT,
    base_ref             TEXT,
    base_sha             TEXT,
    head_sha             TEXT,
    criteria_digest      TEXT,
    cxc_status           TEXT NOT NULL,
    cxc_reason           TEXT NOT NULL,
    contract_version     TEXT NOT NULL,
    summary              TEXT NOT NULL,
    evidence             TEXT,
    unresolved           TEXT,
    next_action          TEXT NOT NULL,
    review               TEXT,
    restore              TEXT,
    recorded_at          TEXT NOT NULL,
    -- Keyed on the submission too, so recording a later one preserves the earlier row. An
    -- earlier message may have elided part of its report and told its recipient to read the
    -- rest with show; overwriting the only full copy would break that promise for anyone
    -- still holding the older message.
    PRIMARY KEY (event_id, submission_no)
);

-- CRW-128: the merge-readiness evidence a child hands to its parent, beside the report it
-- belongs to rather than inside it. A separate table rather than new columns on work_reports,
-- because this store has no migration path and CREATE TABLE IF NOT EXISTS never alters an
-- existing one: added columns would exist only on databases created afterwards, and every
-- insert naming them would fail on the stores already out there. A new table is created on
-- both.
--
-- Its absence for an event is meaningful and is not a defect: a report that names no pull
-- request has no merge readiness to state, and a correction travelling the other way is the
-- parent's judgment rather than the child's evidence.
CREATE TABLE IF NOT EXISTS work_report_handoffs (
    event_id           TEXT NOT NULL,
    submission_no      INTEGER NOT NULL DEFAULT 1,
    is_draft           INTEGER NOT NULL,
    base_verified_at   TEXT,
    required_declared  TEXT NOT NULL,
    checks             TEXT NOT NULL,
    review_coverage    TEXT NOT NULL,
    thread_dispositions TEXT NOT NULL,
    criterion_evidence TEXT,
    limitations        TEXT,
    recorded_at        TEXT NOT NULL,
    PRIMARY KEY (event_id, submission_no)
);

-- Which report submission the bytes frozen for one attempt were rendered from. The message
-- itself says so, which is what a recipient needs, but the relay needs it programmatically:
-- a submission that has never been frozen into an attempt can still be corrected in place,
-- and one that has cannot. An attempt with no row here predates the work report contract and
-- carries the pre-contract message.
CREATE TABLE IF NOT EXISTS attempt_report_submissions (
    request_id    TEXT PRIMARY KEY,
    event_id      TEXT NOT NULL,
    submission_no INTEGER NOT NULL,
    frozen_at     TEXT NOT NULL
);

-- How an acknowledgement's own turn was established. host_read is an App Server read of the
-- recipient's real turn list; unverified is recorded intent still awaiting that read. There is
-- deliberately no tier derived from what the relay itself sent: a stored dispatch proves a send
-- was accepted, never that the parent observed anything.
CREATE TABLE IF NOT EXISTS ack_evidence (
    event_id      TEXT PRIMARY KEY,
    tier          TEXT NOT NULL,
    detail        TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_reason   TEXT,
    fingerprint   TEXT,
    next_check_at REAL,
    observed_at   TEXT NOT NULL
);

-- A merge is the one assignment fact the relay cannot observe, so it is the one that is marked
-- rather than derived. Bound to the exact event, generation and revision it is about: keyed on
-- the relationship alone, one old merge would have labelled every later generation merged.
CREATE TABLE IF NOT EXISTS assignment_marks (
    relationship_id      TEXT NOT NULL,
    mark                 TEXT NOT NULL,
    event_id             TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    revision_hash        TEXT NOT NULL,
    evidence             TEXT NOT NULL,
    actor                TEXT NOT NULL,
    marked_at            TEXT NOT NULL,
    PRIMARY KEY (relationship_id, mark, event_id)
);

-- Where a relationship's coordination summaries go. Per relationship rather than global,
-- because one host runs many assignments against different documents.
CREATE TABLE IF NOT EXISTS sync_targets (
    relationship_id TEXT NOT NULL,
    target          TEXT NOT NULL,
    target_ref      TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    PRIMARY KEY (relationship_id, target)
);

-- The outbox. Enqueued inside the transaction that decided the thing it describes, so the
-- summary cannot be lost, and keyed on that decision's identity INCLUDING the target document,
-- so the same verdict owed to two documents is two jobs. claim_token fences complete and fail:
-- an old claimant cannot undo what a newer one already confirmed.
CREATE TABLE IF NOT EXISTS sync_outbox (
    sync_id              TEXT PRIMARY KEY,
    relationship_id      TEXT NOT NULL,
    issue_key            TEXT NOT NULL,
    target               TEXT NOT NULL,
    target_ref           TEXT NOT NULL,
    subject_kind         TEXT NOT NULL,
    event_id             TEXT,
    execution_generation INTEGER,
    revision_hash        TEXT,
    verdict              TEXT,
    identity_digest      TEXT NOT NULL,
    summary              TEXT NOT NULL,
    state                TEXT NOT NULL,
    attempts             INTEGER NOT NULL DEFAULT 0,
    next_attempt_at      REAL,
    last_error           TEXT,
    lease_owner          TEXT,
    lease_until          REAL,
    claim_token          TEXT,
    external_ref         TEXT,
    readback             TEXT,
    written_at           TEXT,
    confirmed_at         TEXT,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS acks (
    event_id         TEXT PRIMARY KEY,
    record           TEXT NOT NULL,
    ack_turn_id      TEXT NOT NULL,
    accepted         INTEGER NOT NULL,
    verified         TEXT NOT NULL,
    rejection_reason TEXT,
    ack_at           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS verdicts (
    event_id        TEXT PRIMARY KEY,
    record          TEXT NOT NULL,
    verdict         TEXT NOT NULL,
    next_generation INTEGER,
    verdict_turn_id TEXT NOT NULL,
    decided_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS verification_claims (
    event_id      TEXT PRIMARY KEY,
    claim_turn_id TEXT,
    claimed_at    TEXT NOT NULL
);

-- Which turns are admitted to a generation's execution, and on what evidence. The anchor in
-- generations stays immutable; this is a separate record, because "where the execution
-- started" and "which turns belong to it" are different questions with different proofs.
CREATE TABLE IF NOT EXISTS generation_turns (
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    turn_id              TEXT NOT NULL,
    evidence             TEXT NOT NULL,
    actor                TEXT,
    detail               TEXT,
    admitted_at          TEXT NOT NULL,
    PRIMARY KEY (relationship_id, execution_generation, turn_id)
);

CREATE TABLE IF NOT EXISTS recipient_rate (
    recipient_task_id TEXT NOT NULL,
    window_start      REAL NOT NULL,
    sends             INTEGER NOT NULL DEFAULT 0,
    last_send_at      REAL,
    PRIMARY KEY (recipient_task_id, window_start)
);

-- Host lifecycle, observed from the App Server rather than inferred from our own registry.
-- A user can archive or pause a task without ever touching this relay, so the relationship
-- status is our authorization record and this table is what the host actually reports.
-- thread/read carries runtime status only and has no archived flag; archived comes from the
-- thread/list archived filter, and paused / usageLimited / budgetLimited come from the
-- thread goal status. Nothing here is ever written back to the host.
CREATE TABLE IF NOT EXISTS recipient_lifecycle (
    task_id          TEXT PRIMARY KEY,
    runtime_status   TEXT,
    archived         INTEGER,
    goal_status      TEXT,
    can_accept_input INTEGER,
    deliverable      TEXT NOT NULL,
    withhold_reason  TEXT,
    detail           TEXT,
    observed_at      TEXT NOT NULL
);

-- Archive discovery resumes where it stopped instead of re-reading one prefix, so a task past
-- the bound is still reached within a bounded number of ticks. It lives here rather than in
-- recipient_lifecycle because that table's detail field is rewritten on every observation.
CREATE TABLE IF NOT EXISTS discovery_cursors (
    task_id    TEXT NOT NULL,
    listing    TEXT NOT NULL,
    cursor     TEXT,
    exhausted  INTEGER NOT NULL DEFAULT 0,
    scanned    INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (task_id, listing)
);

-- Whether reconciliation is worth invoking. retry_required records that work is OWED, which a
-- fingerprint cannot: a failed read followed by an unchanged reading would otherwise silently
-- drop the reconciliation the failure owed.
CREATE TABLE IF NOT EXISTS reconcile_gate (
    request_id     TEXT PRIMARY KEY,
    fingerprint    TEXT,
    retry_required INTEGER NOT NULL DEFAULT 1,
    last_error     TEXT,
    updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS journal (
    seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT NOT NULL,
    kind    TEXT NOT NULL,
    subject TEXT,
    detail  TEXT
);
-- Read by the fault sweep's refusal source, which pages delivery_withheld rows by sequence and
-- asks, per delivery, whether anything later ended the refusal streak. Each index also carries
-- the sequence (the rowid), so both reads are range scans; without them every sweep scanned the
-- whole journal.
CREATE INDEX IF NOT EXISTS journal_kind ON journal (kind);
CREATE INDEX IF NOT EXISTS journal_subject ON journal (subject);
-- Only the creation-stage rows a managed start journals (faultsweep.CREATION_ANSWER_INDEX): the
-- fault sweep reads a request's newest creation answer through it, one probe however many other
-- rows the request's retries journaled; journal_subject walks every one of them. The query
-- repeats this predicate word for word so the planner can use it. The CASE keeps a detail that
-- is not JSON away from json_extract, which would otherwise fail the insert of that row.
CREATE INDEX IF NOT EXISTS journal_managed_creation ON journal (subject)
    WHERE CASE WHEN kind = 'managed_start_observed' AND json_valid(detail)
               THEN json_extract(detail, '$.stage') = 'creation' END;

CREATE TABLE IF NOT EXISTS store_challenge (
    nonce      TEXT PRIMARY KEY,
    written_by TEXT NOT NULL,
    written_at TEXT NOT NULL
);

-- Delivery was WANTED for this event and refused for a reason that may not last. Absence of
-- a delivery row cannot carry that meaning: an event emitted with --no-enqueue and an event
-- stranded by an old generation look identical to one whose queuing was refused.
CREATE TABLE IF NOT EXISTS delivery_intent (
    event_id          TEXT PRIMARY KEY,
    relationship_id   TEXT NOT NULL,
    kind              TEXT NOT NULL,
    recipient_task_id TEXT NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_retry_at     REAL,
    last_error        TEXT,
    noted_at          TEXT NOT NULL
);

-- This delivery is no longer what the assignment stands on. Kept separate from the delivery
-- state on purpose: an outstanding send must keep its state so reconciliation can still
-- settle it, and an already dispatched one must keep its history.
CREATE TABLE IF NOT EXISTS delivery_supersession (
    event_id TEXT PRIMARY KEY,
    reason   TEXT NOT NULL,
    noted_at TEXT NOT NULL,
    applied  INTEGER NOT NULL DEFAULT 0
);

-- The most recent failure per (subject, operation), so an operator reads a cause rather
-- than a state word. Keyed, not appended, so it cannot grow without bound.
CREATE TABLE IF NOT EXISTS failed_operations (
    scope_key       TEXT NOT NULL,
    operation       TEXT NOT NULL,
    relationship_id TEXT,
    parent_task_id  TEXT,
    detail          TEXT NOT NULL,
    error_code      TEXT,
    difference      TEXT,
    retry_safe      INTEGER,
    occurred_at     TEXT NOT NULL,
    next_retry_at   REAL,
    PRIMARY KEY (scope_key, operation)
);

-- Whether we have actually LOOKED at an anchor lately, which an observations row cannot
-- answer: that table records terminal turns only, so a healthy long-running anchor has no
-- entry at all. last_polled_at stays NULL until a poll genuinely succeeds.
CREATE TABLE IF NOT EXISTS poll_observations (
    relationship_id      TEXT NOT NULL,
    execution_generation INTEGER NOT NULL,
    turn_id              TEXT NOT NULL,
    last_status          TEXT,
    last_polled_at       TEXT,
    last_attempt_at      TEXT NOT NULL,
    last_error           TEXT,
    PRIMARY KEY (relationship_id, execution_generation, turn_id)
);

-- Three-level execution linkage. relationships binds one parent to one child per ISSUE, so
-- nothing in it says that parent owns a PROJECT, and the initiative level has no row at all.
-- These tables add the two missing levels to the SAME store rather than to a second one: the
-- schema is applied with CREATE TABLE IF NOT EXISTS on every open, which reaches an existing
-- database with a new table and never with a new column.
--
-- Nothing here is an assignment. A supervision carries no receipt, no acknowledgement, no
-- verdict, no generation and no artifact scope, and a peer link carries less than that.

-- Which Linear scope an execution task owns, and at which level. The task id is part of the
-- key deliberately: a binding is one task's claim on one scope, so replacing the owner
-- produces a NEW binding rather than rewriting who the old one was.
CREATE TABLE IF NOT EXISTS scope_bindings (
    binding_id    TEXT PRIMARY KEY,
    role          TEXT NOT NULL,
    scope_kind    TEXT NOT NULL,
    scope_key     TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    host_id       TEXT NOT NULL,
    cwd           TEXT,
    cxc_session   TEXT,
    status        TEXT NOT NULL,
    revision      INTEGER NOT NULL,
    supersedes    TEXT,
    superseded_by TEXT,
    handover_note TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- An edge between two scopes. execution carries ownership, reference never does, and peer is
-- not hierarchy at all: every walk filters link_kind = 'execution', so a reference or a peer
-- row can never lengthen a chain or introduce a second owner.
--
-- The KEY is the two scopes and the kind, and never a task id. Keying on the owner would mean
-- a handover changed the identity of an unchanged relationship, and a later re-registration
-- would derive a different id and create a duplicate. The task columns are the owners as they
-- stood when the edge was written, kept for drift detection and deliberately outside identity.
CREATE TABLE IF NOT EXISTS scope_links (
    link_id       TEXT PRIMARY KEY,
    link_kind     TEXT NOT NULL,
    upper_kind    TEXT NOT NULL,
    upper_key     TEXT NOT NULL,
    upper_task_id TEXT NOT NULL,
    lower_kind    TEXT NOT NULL,
    lower_key     TEXT NOT NULL,
    lower_task_id TEXT NOT NULL,
    status        TEXT NOT NULL,
    revision      INTEGER NOT NULL,
    superseded_by TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Which project an issue assignment belongs to. A separate table for the reason above: an
-- assignment registered before this work has no row here and is reported as unscoped, which is
-- the truth, rather than as missing or as belonging to whichever project happens to ask.
CREATE TABLE IF NOT EXISTS relationship_scope (
    relationship_id TEXT PRIMARY KEY,
    project_key     TEXT NOT NULL,
    recorded_at     TEXT NOT NULL
);

-- An instruction that reached a scope, by digest and origin. Detecting two initiatives that
-- name different parents does not cover a conflict of INSTRUCTIONS: two supervisors can agree
-- about who the parent is and still instruct it differently. Dispositions accumulate and never
-- rewrite the directive they settle, so the instruction that lost stays readable. This records
-- that an instruction exists and what it is a digest of; it is not a channel.
CREATE TABLE IF NOT EXISTS scope_directives (
    directive_id   TEXT PRIMARY KEY,
    scope_kind     TEXT NOT NULL,
    scope_key      TEXT NOT NULL,
    from_task_id   TEXT NOT NULL,
    from_scope_key TEXT NOT NULL,
    link_id        TEXT NOT NULL,
    link_kind      TEXT NOT NULL,
    digest         TEXT NOT NULL,
    reference      TEXT,
    revision       INTEGER NOT NULL,
    disposition    TEXT,
    decided_by     TEXT,
    decided_at     TEXT,
    recorded_at    TEXT NOT NULL
);

-- A contested or contradictory linkage attempt, retained. A refusal that only raises leaves the
-- contest invisible to every later reader, which is the failure intent.bind already solved by
-- publishing its conflict rather than swallowing it. Written INSIDE the same transaction that
-- decided the refusal, which is safe because validation precedes every mutation: at that moment
-- the transaction has written nothing else, so it commits the contest alone and the refusal is
-- raised after it closes. There is no second transaction and no crash gap.
--
-- incumbent and challenger are NOT NULL because SQLite treats NULLs as distinct in a UNIQUE
-- index, so a nullable column would let a replayed refusal insert a second row instead of
-- converging on one.
CREATE TABLE IF NOT EXISTS linkage_conflicts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_key  TEXT NOT NULL,
    reason     TEXT NOT NULL,
    incumbent  TEXT NOT NULL DEFAULT '',
    challenger TEXT NOT NULL DEFAULT '',
    detail     TEXT,
    UNIQUE (scope_kind, scope_key, reason, incumbent, challenger)
);

CREATE INDEX IF NOT EXISTS deliveries_state ON deliveries (state, next_eligible_at);
-- Per-parent selection reads one parent's oldest eligible rows at a time, which is a
-- different access pattern from deliveries_state. Declaring it is not proof it is used:
-- the query plan is inspected in the fairness tests rather than assumed.
CREATE INDEX IF NOT EXISTS deliveries_relationship_created ON deliveries
    (relationship_id, created_at);
CREATE INDEX IF NOT EXISTS attempts_open ON attempts (internal_state);
CREATE INDEX IF NOT EXISTS events_relationship ON events (relationship_id, execution_generation);
CREATE INDEX IF NOT EXISTS events_stage ON events (stage, turn_id);
CREATE INDEX IF NOT EXISTS lineage_generation ON revision_lineage
    (relationship_id, execution_generation);
CREATE INDEX IF NOT EXISTS relationships_issue ON relationships (issue_key, status);
CREATE INDEX IF NOT EXISTS scope_bindings_scope ON scope_bindings
    (scope_kind, scope_key, status);
CREATE INDEX IF NOT EXISTS scope_bindings_task ON scope_bindings (task_id, status);
CREATE INDEX IF NOT EXISTS scope_links_lower ON scope_links
    (lower_kind, lower_key, link_kind, status);
CREATE INDEX IF NOT EXISTS scope_links_upper ON scope_links
    (upper_kind, upper_key, link_kind, status);
CREATE INDEX IF NOT EXISTS scope_directives_scope ON scope_directives
    (scope_kind, scope_key, disposition);
-- One live owner per scope and role, and one live edge per kind and scope pair, enforced by
-- the database rather than only by the code that writes it. A partial unique index because
-- superseded and archived rows are retained deliberately and must not compete.
--
-- An index CAN be added to an existing store, unlike a CHECK constraint, which only ever
-- reaches a database created after it. So the invariants that matter most are indexes and the
-- vocabulary checks stay in Python, rather than being written where half the stores would
-- never get them.
CREATE INDEX IF NOT EXISTS sync_ready ON sync_outbox (state, next_attempt_at);
-- Coordination between parents under one supervision: whose turn it is to merge into a shared
-- target, how much concurrent execution a scope is using against what it declared, and what
-- two peer projects agreed about a shared edit region. Appended as one block at the END of the
-- script, so a sibling adding tables elsewhere and this work cannot produce an overlapping
-- hunk. Every statement is CREATE TABLE IF NOT EXISTS for the reason stated above: the script
-- runs on every open, which reaches an existing database with a new table and never with a new
-- column.

-- A contested coordination attempt, retained after it was refused. Separate from
-- linkage_conflicts because the domains and the reader belong to the coordination modules;
-- writing a merge target into linkage's table would make Linkage.conflicts answer about a
-- vocabulary it does not own. incumbent and challenger are NOT NULL because SQLite treats
-- NULLs as distinct in a unique index, so a nullable column would let a replayed refusal
-- insert a second row instead of converging on one.
CREATE TABLE IF NOT EXISTS coordination_conflicts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    domain     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    reason     TEXT NOT NULL,
    incumbent  TEXT NOT NULL DEFAULT '',
    challenger TEXT NOT NULL DEFAULT '',
    detail     TEXT,
    UNIQUE (domain, subject, reason, incumbent, challenger)
);

-- One parent's claim on one merge target, waiting or holding. The target is a repository and
-- the base ref a pull request lands on, not a project: one project can own work in several
-- repositories and two projects can share one base branch, so keying on the project would
-- serialise work that never contends and fail to serialise work that does.
--
-- holder_task_id is part of the KEY because a claim is one parent's claim, the same reason a
-- scope binding keys with its task. tenure is in the key because the same parent taking the
-- turn again later is a second tenure rather than a replay of the first. project_key is
-- deliberately outside it: a handover changes who owns the project without changing which
-- claim this is.
CREATE TABLE IF NOT EXISTS merge_turns (
    turn_id           TEXT PRIMARY KEY,
    target_key        TEXT NOT NULL,
    repository        TEXT NOT NULL,
    base_ref          TEXT NOT NULL,
    project_key       TEXT NOT NULL,
    holder_task_id    TEXT NOT NULL,
    holder_host_id    TEXT NOT NULL,
    relationship_id   TEXT,
    pr_number         INTEGER,
    candidate_head    TEXT NOT NULL,
    declared_ready    INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL,
    tenure            INTEGER NOT NULL,
    checked_base_sha  TEXT,
    landed_sha        TEXT,
    observed_base_sha TEXT,
    close_reason      TEXT,
    requested_at      TEXT NOT NULL,
    held_at           TEXT,
    merging_at        TEXT,
    closed_at         TEXT,
    updated_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS merge_turns_target ON merge_turns (target_key, state);

-- Every transition and every attestation about a turn, append-only. state cannot carry both:
-- a transport accepting a message ABOUT a turn is not the holder acting on it, and folding the
-- first into the second is the confusion this table exists to prevent. The unique key makes a
-- replayed notification converge on one row rather than recording a second fact.
CREATE TABLE IF NOT EXISTS merge_turn_ledger (
    entry_id        TEXT PRIMARY KEY,
    turn_id         TEXT NOT NULL,
    kind            TEXT NOT NULL,
    from_state      TEXT,
    to_state        TEXT,
    evidence_kind   TEXT NOT NULL,
    actor_task_id   TEXT NOT NULL,
    evidence        TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    UNIQUE (turn_id, idempotency_key)
);

-- What the holder restated immediately before merging. A REFUSED check is stored too, because
-- it is the evidence for the safe return that follows it: a refusal that only raises leaves
-- the next reader no way to learn why the turn came back.
CREATE TABLE IF NOT EXISTS merge_turn_checks (
    check_id       TEXT PRIMARY KEY,
    turn_id        TEXT NOT NULL,
    head_sha       TEXT NOT NULL,
    base_sha       TEXT NOT NULL,
    required       TEXT NOT NULL,
    checks_digest  TEXT NOT NULL,
    checks         TEXT NOT NULL,
    review_digest  TEXT NOT NULL,
    review         TEXT NOT NULL,
    result         TEXT NOT NULL,
    refusal_reason TEXT,
    recorded_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS merge_turn_checks_turn ON merge_turn_checks (turn_id, recorded_at);
-- One lease on one execution subject. There is no stored counter anywhere here: a count is
-- always COUNT(*) over held rows, so there is nothing to decrement twice and nothing to leak
-- when a process dies between a decrement and the row that was supposed to explain it.
--
-- tenure is part of the key, so releasing and re-reserving one subject retains the released
-- row and opens a second one. Overwriting instead would destroy the evidence a duplicate or
-- contradictory release is detected against.
CREATE TABLE IF NOT EXISTS execution_slots (
    slot_id        TEXT PRIMARY KEY,
    subject_kind   TEXT NOT NULL,
    subject_key    TEXT NOT NULL,
    parent_task_id TEXT NOT NULL,
    project_key    TEXT NOT NULL,
    initiative_key TEXT,
    tenure         INTEGER NOT NULL,
    state          TEXT NOT NULL,
    reserved_by    TEXT NOT NULL,
    reserved_at    TEXT NOT NULL,
    released_at    TEXT,
    released_by    TEXT,
    release_reason TEXT,
    detail         TEXT
);
CREATE INDEX IF NOT EXISTS execution_slots_held ON execution_slots (state, parent_task_id);

-- A declared bound, with the dimension it bounds. 'runs' is the one dimension this store can
-- count for itself. Every other - file descriptors, model spend - is a fact about a host or an
-- account that no number of rows here measures, which is why a value for one can only come
-- from execution_usage. Deriving an FD limit from a task count is the error this separation
-- exists to make structurally impossible rather than merely discouraged.
CREATE TABLE IF NOT EXISTS execution_limits (
    limit_id    TEXT PRIMARY KEY,
    scope_kind  TEXT NOT NULL,
    scope_key   TEXT NOT NULL,
    dimension   TEXT NOT NULL,
    unit        TEXT NOT NULL,
    ceiling     REAL NOT NULL,
    enforce     INTEGER NOT NULL DEFAULT 1,
    declared_by TEXT NOT NULL,
    source      TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    declared_at TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- An observation of a dimension, by whom and by what method. The only way a non-runs dimension
-- acquires a current value. A missing row is unmeasured, never zero: treating an absent
-- measurement as no usage is the same unproven inference with more steps.
CREATE TABLE IF NOT EXISTS execution_usage (
    scope_kind  TEXT NOT NULL,
    scope_key   TEXT NOT NULL,
    dimension   TEXT NOT NULL,
    observed    REAL NOT NULL,
    observed_by TEXT NOT NULL,
    method      TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (scope_kind, scope_key, dimension)
);
-- A place in a specific tree, not a file name. region_kind separates a whole file from a symbol
-- or a data region inside it, which is what stops one shared file from blocking every parent
-- with business in another part of it. base_revision is part of the region because an agreement
-- about a place is an agreement about that place in that tree.
CREATE TABLE IF NOT EXISTS edit_regions (
    region_id       TEXT PRIMARY KEY,
    repository      TEXT NOT NULL,
    base_revision   TEXT NOT NULL,
    path            TEXT NOT NULL,
    region_kind     TEXT NOT NULL,
    region_key      TEXT NOT NULL DEFAULT '',
    region_class    TEXT NOT NULL,
    regenerate_from TEXT,
    recorded_at     TEXT NOT NULL
);

-- Two peer projects' agreement about one region. It confers nothing: no merge permission, no
-- widened artifact scope, no authority to instruct. It records what both sides said they would
-- accept and who is expected to act next. The project keys are stored in the sorted order the
-- identity hashes, so either side proposing converges on one record.
CREATE TABLE IF NOT EXISTS edit_agreements (
    agreement_id      TEXT PRIMARY KEY,
    region_id         TEXT NOT NULL,
    repository        TEXT NOT NULL,
    base_revision     TEXT NOT NULL,
    left_project      TEXT NOT NULL,
    right_project     TEXT NOT NULL,
    peer_link_id      TEXT NOT NULL,
    proposer_task_id  TEXT NOT NULL,
    issue_key         TEXT,
    constraint_text   TEXT NOT NULL,
    left_condition    TEXT,
    right_condition   TEXT,
    left_accepted_at  TEXT,
    right_accepted_at TEXT,
    next_owner        TEXT,
    state             TEXT NOT NULL,
    tenure            INTEGER NOT NULL,
    supersedes        TEXT,
    superseded_by     TEXT,
    close_reason      TEXT,
    proposed_at       TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    closed_at         TEXT
);
CREATE INDEX IF NOT EXISTS edit_agreements_region ON edit_agreements (region_id, state);
CREATE INDEX IF NOT EXISTS edit_agreements_tree ON edit_agreements
    (repository, base_revision, state);

-- What the two sides agreed should happen NEXT, kept apart from the agreement so that lifting
-- the constraint does not drop the work it implied. A null assignee is the unassigned state and
-- is never aggregated under any parent.
CREATE TABLE IF NOT EXISTS edit_followups (
    followup_id      TEXT PRIMARY KEY,
    agreement_id     TEXT NOT NULL,
    trigger_text     TEXT NOT NULL,
    acceptance_text  TEXT NOT NULL,
    issue_ref        TEXT,
    assignee_task_id TEXT,
    assignee_project TEXT,
    accepted_at      TEXT,
    state            TEXT NOT NULL,
    close_reason     TEXT,
    recorded_by      TEXT NOT NULL,
    recorded_at      TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- Which revision a repository's agreements are now stated against, append-only. One successor
-- per revision, so a chain A->B->C leaves every earlier revision with an outgoing mark and only
-- the newest without one. That is what lets a settlement ask whether its OWN revision was
-- superseded, instead of asking which mark is newest - a question with no answer when two marks
-- share an injected clock's instant.
CREATE TABLE IF NOT EXISTS edit_revision_marks (
    mark_id       TEXT PRIMARY KEY,
    repository    TEXT NOT NULL,
    from_revision TEXT NOT NULL,
    to_revision   TEXT NOT NULL,
    actor         TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    UNIQUE (repository, from_revision)
);

-- CRW-237. What a reaffirmation carried onto the successor it created: one row per successor,
-- written in the transaction that retires the predecessor and inserts the successor, so no
-- successor exists without the record of what it carries. The three *_revision columns name the
-- revision each text was WRITTEN against. A constraint or condition carried forward keeps the
-- revision it came from, so a line number inside it is read against that tree rather than the
-- successor's; a condition the carrying side restates, or a decline writes, is stated on the
-- successor's own revision. A new table rather than columns on edit_agreements, because CREATE
-- TABLE IF NOT EXISTS reaches an existing store and an added column would not.
CREATE TABLE IF NOT EXISTS edit_reaffirmations (
    agreement_id             TEXT PRIMARY KEY,
    predecessor_id           TEXT NOT NULL,
    actor                    TEXT NOT NULL,
    actor_project            TEXT NOT NULL,
    from_revision            TEXT NOT NULL,
    to_revision              TEXT NOT NULL,
    constraint_revision      TEXT NOT NULL,
    left_condition_revision  TEXT,
    right_condition_revision TEXT,
    recorded_at              TEXT NOT NULL
);

-- One managed admission request for one issue, on THIS physical store only. A new table
-- rather than columns on relationships: CREATE TABLE IF NOT EXISTS reaches an existing
-- store, and adding a column would not. The request keeps its own row through reserved,
-- create_armed, attached and released so a crash can be read back without inventing a
-- second database. Ownership of a live assignment stays on relationships; this row only
-- holds the issue while it is still reserved or armed, which is what the partial unique
-- index below enforces. A released row is a tombstone and must not be reused.
CREATE TABLE IF NOT EXISTS managed_start_requests (
    request_id              TEXT PRIMARY KEY,
    issue_key               TEXT NOT NULL,
    request_fingerprint     TEXT NOT NULL,
    fingerprint_version     TEXT NOT NULL,
    workspace               TEXT NOT NULL,
    marker_root             TEXT NOT NULL,
    socket_identity         TEXT NOT NULL,
    create_request_id       TEXT NOT NULL,
    dispatch_request_id     TEXT NOT NULL,
    state                   TEXT NOT NULL,
    revision                INTEGER NOT NULL,
    child_task_id           TEXT,
    standby_turn_id         TEXT,
    relationship_id         TEXT,
    execution_generation    INTEGER,
    receipt_status          TEXT,
    release_reason          TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS managed_start_requests_issue
    ON managed_start_requests (issue_key, state);
-- One pending admission per issue. Attached and released rows are retained and must not
-- compete: attached ownership has already moved to the relationship, and released is the
-- tombstone that forbids reusing that request id.
CREATE UNIQUE INDEX IF NOT EXISTS managed_start_one_pending_issue
    ON managed_start_requests (issue_key)
    WHERE state IN ('reserved', 'create_armed');

-- What a parent owes the level above, staged before it is sent, and what came back.
--
-- Three tables rather than a kind on deliveries. A supervisor message is not an event: it has
-- no receipt, no manifest, no generation of its own and - for a turn that ended without
-- reporting - no events row anywhere to be keyed on. Putting it in deliveries would mean
-- loosening the claim statement that exists to refuse exactly those things, and the two queues
-- would then be one queue with a discriminator. Appended at the END of the script for the
-- reason the block above gives: a sibling adding tables elsewhere and this work cannot produce
-- an overlapping hunk.
--
-- The message id is the envelope's, derived from the direction, the relation, the purpose and
-- the subject, so one fact staged twice converges on one row instead of waking a supervisor
-- twice. The packet is frozen HERE, before any send: what a crash between deciding and sending
-- must not lose is the decision, and re-deriving it later would compose it out of rows that
-- have moved on.
CREATE TABLE IF NOT EXISTS supervisor_messages (
    message_id        TEXT PRIMARY KEY,
    obligation_id     TEXT NOT NULL,
    obligation_kind   TEXT NOT NULL,
    relationship_id   TEXT NOT NULL,
    project_key       TEXT,
    purpose           TEXT NOT NULL,
    kind              TEXT NOT NULL,
    sender_task_id    TEXT NOT NULL,
    recipient_task_id TEXT NOT NULL,
    subject           TEXT NOT NULL,
    packet            TEXT NOT NULL,
    state             TEXT NOT NULL,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    next_eligible_at  REAL,
    hold_reason       TEXT,
    lease_owner       TEXT,
    lease_until       REAL,
    staged_at         TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    -- The event and the work-report submission the packet was composed from, NULL for an
    -- omission, which has neither. report.record refuses to change a report once a message
    -- names its event, so the packet and the evidence it points at stay one fact.
    event_id          TEXT,
    submission_no     INTEGER,
    -- The reporting-observation/1 reading an omission was staged from, exactly as staged, NULL
    -- for an event. The observer's own answer can change afterwards - a late report reaches
    -- the turn - so an omission's packet points at supervisor-show, which prints this frozen
    -- reading, rather than at a command that re-reads the turn now.
    reading           TEXT
);

-- One transport attempt at one of those messages, with the bytes that attempt froze. The bytes
-- live here rather than beside the packet because they are per attempt: the request id is
-- rendered into them, so attempt 2 does not say what attempt 1 said, and lost-response
-- reconciliation searches a recipient for the token the message actually carried.
--
-- sent_at is when the attempt was CLAIMED and its bytes frozen. transport_started_at is the
-- instant immediately before the transport was called, committed before the call, and NULL
-- when the transport never started; a readback's chronology is measured from it and from
-- nothing else. It was added before this table was ever released, so no store holds the table
-- without it.
CREATE TABLE IF NOT EXISTS supervisor_attempts (
    request_id     TEXT PRIMARY KEY,
    message_id     TEXT NOT NULL,
    attempt_no     INTEGER NOT NULL,
    message        TEXT NOT NULL,
    state          TEXT NOT NULL,
    send_attempted TEXT NOT NULL,
    retry_safe     INTEGER NOT NULL DEFAULT 0,
    turn_id        TEXT,
    record         TEXT NOT NULL,
    sent_at        TEXT NOT NULL,
    transport_started_at TEXT,
    observed_at    TEXT NOT NULL,
    -- What a readback looks for in the recipient's transcript: the request id and a random part
    -- drawn inside the claim, rendered into these bytes and nowhere else. The request id alone is
    -- derived from the message and the attempt number, so a copy of it could be written into the
    -- recipient's thread before the send and found there after a lost response.
    delivery_token TEXT,
    UNIQUE (message_id, attempt_no)
);

-- The recipient saying it read one, from inside its own turn. One row per message, because a
-- second reading of the same message is the same fact; the proof and the turn are kept so a
-- later reader can recompute the first rather than trust that somebody checked it.
CREATE TABLE IF NOT EXISTS supervisor_readbacks (
    message_id   TEXT PRIMARY KEY,
    read_turn_id TEXT NOT NULL,
    proof        TEXT NOT NULL,
    verified     TEXT NOT NULL,
    request_id   TEXT,
    detail       TEXT,
    read_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS supervisor_messages_eligible ON supervisor_messages
    (state, next_eligible_at);
CREATE INDEX IF NOT EXISTS supervisor_messages_recipient ON supervisor_messages
    (recipient_task_id, staged_at);
CREATE INDEX IF NOT EXISTS supervisor_messages_event ON supervisor_messages (event_id);
-- One fault notification is one supervisor message (supervisorchannel.NOTICE): its obligation
-- id is the notification id, and a second staging of the same notification - another process,
-- a restart, another relationship addressing it - finds this row instead of adding one.
CREATE UNIQUE INDEX IF NOT EXISTS supervisor_messages_one_notice
    ON supervisor_messages (obligation_id) WHERE obligation_kind = 'fault_notification';

-- What a child's own relay recorded here beside the marker facts it writes (declarations.py).
-- The relay daemon reads no marker file, so a turn that ended without a report is invisible
-- to it unless the declarations a child DID make are in this store as well. Both are written by
-- the command that writes the marker fact, after it, and mirror the fact the marker stands on.
--
-- reporting_sessions is the cut-over. A session whose relay records its declarations here says
-- so once, when it claims its assignment; a turn whose session has no row is a legacy admission
-- whose declarations may exist only in the marker, and omitted.derive derives nothing for it.
CREATE TABLE IF NOT EXISTS reporting_sessions (
    assignment_id       TEXT NOT NULL,
    session_id          TEXT NOT NULL,
    dispatch_request_id TEXT NOT NULL,
    marker_root         TEXT NOT NULL,
    workspace           TEXT NOT NULL,
    -- The issue the intent was declared for, so the store reader checks the registration
    -- against the same declared identity the marker reader does.
    issue_key           TEXT,
    capability          TEXT NOT NULL,
    recorded_at         TEXT NOT NULL,
    PRIMARY KEY (assignment_id, session_id)
);

-- A turn's declared outcome, create-once like dispositions/<session>/<turn>.json.
CREATE TABLE IF NOT EXISTS turn_declarations (
    assignment_id TEXT NOT NULL,
    session_id    TEXT NOT NULL,
    turn_id       TEXT NOT NULL,
    outcome       TEXT NOT NULL,
    declared_at   TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    PRIMARY KEY (assignment_id, session_id, turn_id)
);

-- Operational faults: the machinery failing to do its job, as opposed to a child failing at
-- its task. One row per distinct BREAKAGE and never one per incident, which is what makes
-- the difference between a record somebody reads and a Linear project nobody can.
--
-- Separate tables rather than a second subject_kind on sync_outbox. That outbox writes into
-- a document whose reference is already known and makes every write a conditional
-- replacement of an owned container; a fault's first write CREATES the thing it will later
-- be addressed by, so it cannot borrow either property.
CREATE TABLE IF NOT EXISTS fault_ledger (
    fault_id         TEXT PRIMARY KEY,
    product          TEXT NOT NULL,
    fault_class      TEXT NOT NULL,
    component        TEXT NOT NULL,
    severity         TEXT NOT NULL,
    signature        TEXT NOT NULL,
    scope            TEXT NOT NULL,
    scope_key        TEXT NOT NULL,
    state            TEXT NOT NULL,
    cycle            INTEGER NOT NULL DEFAULT 1,
    -- Which uncleared EPISODE this fault is in. Occurrence identity carries it, so repeated
    -- sweeps inside one episode converge as they must, while the same underlying fact
    -- observed after a clear is a new occurrence rather than a familiar one - which is what
    -- lets a resolved fault reopen when its cause comes back under the key it always had.
    episode          INTEGER NOT NULL DEFAULT 1,
    occurrence_count INTEGER NOT NULL DEFAULT 0,
    reopen_count     INTEGER NOT NULL DEFAULT 0,
    detail           TEXT,
    suppression      TEXT,
    external_ref     TEXT,
    first_seen_at    TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL,
    cleared_at       TEXT,
    published_at     TEXT,
    resolved_at      TEXT,
    updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS fault_ledger_scope ON fault_ledger (scope_key, state);

-- Append-only, one row per distinct occurrence. The unique key is what stops a sweep that
-- reads the same stuck row on every tick from counting thousands of occurrences an hour and
-- escalating a fault past every threshold on nothing.
--
-- evidence is a SNAPSHOT of what was observed, not a pointer to it. The rows a fault is read
-- from are mutable - a poll row's error is overwritten on its next attempt, a failed
-- synchronisation row changes state when it finally succeeds - so a pointer followed later
-- can contradict the record it was filed as evidence for.
--
-- Order is by rowid. recorded_ts is this store's own clock and measures elapsed windows;
-- observed_at belongs to whoever observed it and decides nothing.
CREATE TABLE IF NOT EXISTS fault_occurrences (
    occurrence_id   TEXT PRIMARY KEY,
    fault_id        TEXT NOT NULL,
    -- The episode is part of the key, not only of the id. Without it the surviving
    -- (fault_id, occurrence_key) uniqueness silently rejected every post-recovery row while
    -- the caller had already been told the occurrence was new, so counts and timelines grew
    -- on every sweep and a tick could never go quiet again.
    episode         INTEGER NOT NULL DEFAULT 1,
    occurrence_key  TEXT NOT NULL,
    severity        TEXT NOT NULL,
    cleared         INTEGER NOT NULL DEFAULT 0,
    detail          TEXT,
    evidence        TEXT NOT NULL,
    evidence_digest TEXT NOT NULL,
    truncated       INTEGER NOT NULL DEFAULT 0,
    observed_at     TEXT,
    recorded_at     TEXT NOT NULL,
    recorded_ts     REAL NOT NULL,
    UNIQUE (fault_id, episode, occurrence_key)
);
-- On the fault alone. Order is by rowid, which SQLite cannot be asked to index because the
-- table is already stored in it.
CREATE INDEX IF NOT EXISTS fault_occurrences_fault ON fault_occurrences (fault_id);

-- Fixes and reverifications, append-only and per cycle. A fix alone resolves nothing; what
-- resolves a fault is a reverification recorded AFTER the newest fix with no occurrence
-- after it. Both comparisons are made on fault_timeline.seq, because rowids are per-table
-- and these rows have to be ordered against occurrences in another one.
CREATE TABLE IF NOT EXISTS fault_remediations (
    remediation_id TEXT PRIMARY KEY,
    fault_id       TEXT NOT NULL,
    cycle          INTEGER NOT NULL,
    kind           TEXT NOT NULL,
    ref            TEXT NOT NULL,
    method         TEXT,
    outcome        TEXT,
    detail         TEXT,
    recorded_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS fault_remediations_fault ON fault_remediations (fault_id);

-- Where a scope's fault issues are filed. An unconfigured scope is not an error: the fault
-- stays recorded and its publication waits, because filing into a guessed project is worse.
-- Where each source got to last time. Without it every sweep re-read the same first page,
-- so with more persistent faults than one page the ones past it were never observed again -
-- the starvation shape the delivery window already keeps a per-parent cursor to avoid.
--
-- Shipped (installed at 0ffcc4d0): the text between CREATE and its closing parenthesis,
-- in-body comments included, is stored verbatim by SQLite and compared by the runtime swap
-- gate, so it never changes - even a comment. What changed since is said here instead:
-- position now holds JSON {"at", "until"}, where the source's rotation stopped and the upper
-- key it captured when the rotation started (a plain value written before rotations were
-- bounded reads as a position with no bound yet); pages is no longer read, since counting
-- full pages before a forced wrap starved every row past them, and it is kept because this
-- store adds tables and never drops columns.
CREATE TABLE IF NOT EXISTS fault_cursors (
    source     TEXT PRIMARY KEY,
    position   TEXT,
    -- Consecutive FULL pages taken since this source last wrapped. These keys are not
    -- monotonic insertion sequences, so a cursor that only wrapped on a short page would
    -- never come back for a row behind it while full pages kept arriving.
    pages      INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS fault_targets (
    scope_key   TEXT PRIMARY KEY,
    tracker_ref TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

-- The outbox. issued_at is the column this table exists for: once a create has been handed
-- out, an expiring lease can never return the row to pending, because a create whose
-- response was lost is indistinguishable from one that never happened and retrying it
-- blindly is how one fault becomes two issues.
CREATE TABLE IF NOT EXISTS fault_publications (
    publication_id  TEXT PRIMARY KEY,
    fault_id        TEXT NOT NULL,
    kind            TEXT NOT NULL,
    trigger_key     TEXT NOT NULL,
    -- The cycle this write was QUEUED in, not the one the fault is in when somebody gets
    -- round to writing it. A fault that reopens in between moves to a new cycle, and
    -- rendering a queued comment against the live cycle made the block disagree with the
    -- identity digest that was computed when it was queued.
    cycle           INTEGER NOT NULL DEFAULT 1,
    tracker_ref     TEXT,
    external_ref    TEXT,
    summary         TEXT NOT NULL,
    identity_digest TEXT NOT NULL,
    state           TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at REAL,
    claim_token     TEXT,
    lease_owner     TEXT,
    lease_until     REAL,
    issued_at       TEXT,
    last_error      TEXT,
    external_result TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    confirmed_at    TEXT
);
CREATE INDEX IF NOT EXISTS fault_publications_ready
    ON fault_publications (state, next_attempt_at);

-- One order for everything that happens to a fault. An occurrence lives in one table and a
-- remediation in another, and SQLite rowids are per-table, so "was this reverification
-- recorded after that fix" has no answer without a shared sequence. AUTOINCREMENT because a
-- reused sequence number would silently reorder the history this decides resolution from.
--
-- kind is occurrence, cleared, fix or reverification. Cleared is kept apart from occurrence
-- so that a clearing observation cannot be read as the recurrence that refuses a resolution.
--
-- recorded_ts is here and not only on the occurrence, because this table is the one that is
-- never pruned. Counting a threshold from the evidence rows would mean an operator pruning
-- old evidence could lower a count and change what the next observation decides.
CREATE TABLE IF NOT EXISTS fault_timeline (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    fault_id    TEXT NOT NULL,
    cycle       INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    ref_id      TEXT,
    detail      TEXT,
    recorded_at TEXT NOT NULL,
    recorded_ts REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS fault_timeline_fault ON fault_timeline (fault_id, kind);

-- The corrected contract (docs/faults.md, "Corrected contract"). Every table below is NEW
-- because this store has no migration path: CREATE TABLE IF NOT EXISTS runs on every open and
-- nothing ever alters an existing table, so a column added to one of the tables above would
-- simply be missing from every store created before it.
--
-- Which product set a target, and the project its issues are filed in. A target row above
-- that has no row here was written before targets had owners, and nothing is issued through it.
CREATE TABLE IF NOT EXISTS fault_target_projects (
    scope_key   TEXT PRIMARY KEY,
    product     TEXT NOT NULL,
    project_ref TEXT,
    recorded_at TEXT NOT NULL
);

-- What a queued write aims at beyond its tracker: the project an issue create files into, and
-- a registered or update kind's payload. Kept in step with fault_publications.tracker_ref.
-- target_mode is the kind's target requirement when the write was queued - 'team',
-- 'team+project' or 'none' - so re-pointing a write never depends on whether the process doing
-- it registered that kind. A row written before it existed is one of the built-in kinds, which
-- every process registers.
CREATE TABLE IF NOT EXISTS fault_publication_payloads (
    publication_id TEXT PRIMARY KEY,
    project_ref    TEXT,
    payload        TEXT,
    hold_reason    TEXT,
    updated_at     TEXT NOT NULL,
    target_mode    TEXT
);

-- Whether the issue a fault owns sits in the project its scope targets. revision is part of
-- every set_project write's identity, so a later target always gets its own write.
CREATE TABLE IF NOT EXISTS fault_links (
    fault_id             TEXT PRIMARY KEY,
    external_ref         TEXT NOT NULL,
    project_ref          TEXT,
    observed_project_ref TEXT,
    state                TEXT NOT NULL,
    revision             INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT NOT NULL
);

-- An existing issue a recorded fault adopts. Pending until suppression opens the record.
CREATE TABLE IF NOT EXISTS fault_adoptions (
    fault_id     TEXT PRIMARY KEY,
    external_ref TEXT NOT NULL,
    scope        TEXT NOT NULL,
    state        TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

-- An id that names a fault kept under another id: a workspace a fault moved into out of
-- unassigned, or a workspace-bearing id for a fault recorded before workspace joined identity.
CREATE TABLE IF NOT EXISTS fault_aliases (
    alias_id   TEXT PRIMARY KEY,
    fault_id   TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- One row per claim, append-only. The first claimant is the write's owner; a takeover is a
-- recorded act; ended marks an issued request somebody attested can no longer land.
CREATE TABLE IF NOT EXISTS fault_publication_attempts (
    attempt_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    publication_id TEXT NOT NULL,
    attempt        INTEGER NOT NULL,
    owner          TEXT NOT NULL,
    takeover       INTEGER NOT NULL DEFAULT 0,
    claimed_at     TEXT NOT NULL,
    claimed_ts     REAL NOT NULL,
    issued_at      TEXT,
    issued_ts      REAL,
    outcome        TEXT,
    error          TEXT,
    ended          INTEGER NOT NULL DEFAULT 0,
    ended_at       TEXT
);
CREATE INDEX IF NOT EXISTS fault_publication_attempts_publication
    ON fault_publication_attempts (publication_id);

-- A per-product sliding window per kind of write or notification. A use is consumed once per
-- ref; what a spent budget holds stays pending and is never dropped.
CREATE TABLE IF NOT EXISTS fault_budget_uses (
    use_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    product TEXT NOT NULL,
    kind    TEXT NOT NULL,
    ref     TEXT NOT NULL,
    used_at TEXT NOT NULL,
    used_ts REAL NOT NULL,
    UNIQUE (product, kind, ref)
);
CREATE INDEX IF NOT EXISTS fault_budget_uses_window ON fault_budget_uses (product, kind, used_ts);

CREATE TABLE IF NOT EXISTS fault_limits (
    product        TEXT NOT NULL,
    kind           TEXT NOT NULL,
    max_count      INTEGER NOT NULL,
    window_seconds REAL NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (product, kind)
);

-- What the level above is told: a broken fault opened, a decision somebody must make, a fault
-- resolved. Eligibility is decided at reservation; a lapsed reservation is uncertain.
CREATE TABLE IF NOT EXISTS fault_notifications (
    notification_id TEXT PRIMARY KEY,
    fault_id        TEXT NOT NULL,
    product         TEXT NOT NULL,
    kind            TEXT NOT NULL,
    reason          TEXT,
    cycle           INTEGER NOT NULL,
    ref             TEXT,
    state           TEXT NOT NULL,
    token           TEXT,
    owner           TEXT,
    lease_until     REAL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    delivered_at    TEXT,
    ack_ref         TEXT,
    -- The order in which reservation last examined this notification: a sequence, never a
    -- time, so no two tie. Candidates are taken least recently examined first, so one withheld
    -- or held at the head of the queue cannot hide the eligible ones behind it.
    examined_seq    INTEGER
);
CREATE INDEX IF NOT EXISTS fault_notifications_state ON fault_notifications (state, product);
CREATE INDEX IF NOT EXISTS fault_notifications_examined ON fault_notifications (examined_seq);

-- Deliveries the send path's own rule (delivery.supersession_reason) found overtaken, as the
-- fault sweep read them. Every such answer is permanent - a later generation, an answered or
-- replaced revision, a regranted merge turn never come back - so a verdict is recorded once and
-- excluded in SQL afterwards. That is what bounds the sweep's existence question: each call asks
-- the live rule about a bounded number of deliveries it has not judged before, instead of all of
-- them. Written only by the fault sweep, and never read as delivery state.
CREATE TABLE IF NOT EXISTS fault_overtaken_deliveries (
    event_id TEXT PRIMARY KEY,
    reason   TEXT NOT NULL,
    noted_at TEXT NOT NULL
);

-- A product's adjustment of a class's suppression, prospective and journaled.
CREATE TABLE IF NOT EXISTS fault_policies (
    product        TEXT NOT NULL,
    fault_class    TEXT NOT NULL,
    severity       TEXT NOT NULL,
    threshold      INTEGER,
    window_seconds REAL,
    reason         TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (product, fault_class, severity)
);
-- CRW-206 product routing. The fault ledger owns identity, suppression, targets, limits and the
-- outbox; these rows hold only what routing read back from Linear and what it decided, so none
-- of them can become a second copy of anything the ledger decides.
--
-- One validated registry record per product: workspace, team, family label, repositories, the
-- surfaces it watches and how, its triage project and its test target.
CREATE TABLE IF NOT EXISTS product_registry (
    product_key TEXT PRIMARY KEY,
    record      TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

-- Projects and issues as a credential holder read them back from Linear. A snapshot, replaced
-- whole on every bind; routing never writes Linear state here on its own account.
CREATE TABLE IF NOT EXISTS product_bindings (
    product_key TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref         TEXT NOT NULL,
    record      TEXT NOT NULL,
    observed_at TEXT,
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (product_key, kind, ref)
);

-- The explicit project creation policy. Absent means no project is ever created by routing.
CREATE TABLE IF NOT EXISTS routing_policy (
    policy_key  TEXT PRIMARY KEY,
    record      TEXT NOT NULL,
    basis       TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

-- One row per routed fault: where routing decided it belongs and why, what it is waiting for,
-- and the snapshot the digest last reported. The fault itself lives in the ledger.
-- checked_seq is, for an outstanding project proposal, when the digest last checked its create,
-- as a sequence: each digest checks the least recently checked ones first, so no proposal waiting
-- on a slow create can keep a later one from being bound. Explanations stay above the CREATE
-- keyword: SQLite keeps a CREATE's text verbatim, and a shipped object's text never changes.
CREATE TABLE IF NOT EXISTS incident_routes (
    fault_id         TEXT PRIMARY KEY,
    product_key      TEXT NOT NULL,
    workspace        TEXT NOT NULL,
    disposition      TEXT NOT NULL,
    stage            TEXT NOT NULL,
    target           TEXT NOT NULL,
    origin           TEXT NOT NULL,
    claimed_severity TEXT NOT NULL,
    goal             TEXT,
    classification   TEXT,
    superseded_by    TEXT,
    reported         TEXT,
    detail           TEXT,
    checked_seq      INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS incident_routes_stage ON incident_routes (product_key, stage);

-- The newest incidents per route, which a later binding decides again from and a classification
-- replays. Routing's input, not ledger occurrences: the ledger's episode rules decide whether a
-- replayed occurrence is new.
CREATE TABLE IF NOT EXISTS route_incidents (
    incident_id  TEXT PRIMARY KEY,
    fault_id     TEXT NOT NULL,
    record       TEXT NOT NULL,
    recorded_at  TEXT NOT NULL,
    recorded_seq INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS route_incidents_fault ON route_incidents (fault_id, recorded_seq);
-- GUARD_INDEXES executed separately after the DDL
CREATE UNIQUE INDEX IF NOT EXISTS scope_bindings_one_live_owner ON scope_bindings (scope_kind, scope_key, role) WHERE status IN ('active','paused') AND superseded_by IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS scope_links_one_live_edge ON scope_links (link_kind, upper_kind, upper_key, lower_kind, lower_key) WHERE status IN ('active','paused') AND superseded_by IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS merge_turns_one_live_holder ON merge_turns (target_key) WHERE state IN ('holding','merging','unknown');
CREATE UNIQUE INDEX IF NOT EXISTS merge_turns_one_live_claim ON merge_turns (target_key, holder_task_id) WHERE state IN ('waiting','holding','merging','unknown');
CREATE UNIQUE INDEX IF NOT EXISTS execution_slots_one_live_subject ON execution_slots (subject_kind, subject_key) WHERE state = 'held';
CREATE UNIQUE INDEX IF NOT EXISTS edit_agreements_one_live_per_region ON edit_agreements (region_id, left_project, right_project) WHERE state IN ('proposed','agreed','reopened') AND superseded_by IS NULL;
-- schema_meta seeds and assignment_settlements backfill (runtime values shown as placeholders)
INSERT OR IGNORE INTO schema_meta VALUES ('version', '1');
INSERT OR IGNORE INTO schema_meta VALUES ('store_id', '<runtime-value>');
INSERT OR IGNORE INTO schema_meta VALUES ('store_created_at', '<runtime-value>');
INSERT OR IGNORE INTO schema_meta VALUES ('socket_path', '<runtime-value>');
INSERT OR IGNORE INTO assignment_settlements (relationship_id, thread_id, turn_id, terminal_status, settled_at) SELECT relationship_id, thread_id, turn_id, terminal_status, observed_at   FROM observations WHERE relationship_id IS NOT NULL;
