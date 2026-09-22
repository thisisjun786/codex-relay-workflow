"""The durable store. One writer, one schema, one transaction helper.

Two rules shape everything here. Every state transition is committed before its side
effect, so a crash leaves a recoverable state rather than an ambiguous one. And a failed
transition is never a success: the transaction helper rolls back on any exception,
including KeyboardInterrupt, so a partial record cannot survive.

The schema is written once, by this module, with every column the delivery, reconciliation
and acknowledgement layers will need, so no later phase has to migrate it.
"""

import hashlib
import json
import os
import sqlite3
import tempfile
import uuid
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

SCHEMA_VERSION = 1

DDL = """
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
    updated_at        TEXT NOT NULL
);

-- One transport attempt at one of those messages, with the bytes that attempt froze. The bytes
-- live here rather than beside the packet because they are per attempt: the request id is
-- rendered into them, so attempt 2 does not say what attempt 1 said, and lost-response
-- reconciliation searches a recipient for the token the message actually carried.
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
    observed_at    TEXT NOT NULL,
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


"""


# Applied one at a time, AFTER the schema script, because these are the two invariants an
# existing store may already violate - which is the exact case the ambiguity-aware linkage
# readers were written for. Inside the script, a store holding duplicate live rows failed to
# OPEN, so the diagnostics that exist to describe it could never run and an operator got an
# IntegrityError where an answer was owed. A store that cannot take one keeps the rule in
# Python and says which index is missing.
GUARD_INDEXES = (
    ("scope_bindings_one_live_owner",
     "CREATE UNIQUE INDEX IF NOT EXISTS scope_bindings_one_live_owner ON scope_bindings"
     " (scope_kind, scope_key, role)"
     " WHERE status IN ('active','paused') AND superseded_by IS NULL"),
    ("scope_links_one_live_edge",
     "CREATE UNIQUE INDEX IF NOT EXISTS scope_links_one_live_edge ON scope_links"
     " (link_kind, upper_kind, upper_key, lower_kind, lower_key)"
     " WHERE status IN ('active','paused') AND superseded_by IS NULL"),
    ("merge_turns_one_live_holder",
     "CREATE UNIQUE INDEX IF NOT EXISTS merge_turns_one_live_holder ON merge_turns"
     " (target_key)"
     " WHERE state IN ('holding','merging','unknown')"),
    ("merge_turns_one_live_claim",
     "CREATE UNIQUE INDEX IF NOT EXISTS merge_turns_one_live_claim ON merge_turns"
     " (target_key, holder_task_id)"
     " WHERE state IN ('waiting','holding','merging','unknown')"),
    ("execution_slots_one_live_subject",
     "CREATE UNIQUE INDEX IF NOT EXISTS execution_slots_one_live_subject ON execution_slots"
     " (subject_kind, subject_key)"
     " WHERE state = 'held'"),
    ("edit_agreements_one_live_per_region",
     "CREATE UNIQUE INDEX IF NOT EXISTS edit_agreements_one_live_per_region"
     " ON edit_agreements (region_id, left_project, right_project)"
     " WHERE state IN ('proposed','agreed','reopened') AND superseded_by IS NULL"),
)


STATE_ENV = "CODEX_SESSION_RELAY_STATE"
PRECEDENCE = ("flag", "env", "xdg", "home")


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def socket_scope(socket_path) -> str:
    if not socket_path:
        return "default"
    # Canonicalised the same way ScopeRegistry.key does. Hashing the spelling as supplied
    # gave a relative path and a symlink alias for one socket two different default state
    # directories, so the second invocation opened a different store and was then refused
    # by the scope registry as a foreign owner instead of joining the service already there.
    return hashlib.sha256(canonical_socket(socket_path).encode()).hexdigest()[:16]


def canonical_socket(socket_path) -> str:
    """One spelling for one socket, the same way ScopeRegistry.key resolves it."""
    return str(Path(socket_path).expanduser().absolute().resolve())


def legacy_socket_scope(socket_path) -> str:
    """What socket_scope produced before it canonicalised, for finding an existing store."""
    if not socket_path:
        return "default"
    return hashlib.sha256(str(Path(socket_path).expanduser()).encode()).hexdigest()[:16]


@dataclass(frozen=True)
class StateSelection:
    """Which rule chose the state directory, and the exact value that won.

    Four rules can decide where the store lives, and a participant that cannot say which one
    applied to it cannot be compared with another participant. The reason travels with the
    path for that reason alone.
    """

    path: Path
    source: str
    detail: str
    socket_scope: str | None
    # Every store that already records this socket, when there is more than one of them and
    # no canonical store exists yet. Empty for every ordinary selection. It travels ON the
    # selection because the decision not to create a store here has to reach whoever holds
    # the path they would otherwise have created one at.
    ambiguous: tuple = ()
    # Stores that record NO socket at all, when this selection is about to create a new one
    # beside them. They predate provenance, their directory hash cannot be inverted, and one
    # of them may be this socket's - so creating here may be hiding real assignments.
    unidentified: tuple = ()

    @property
    def db_path(self) -> Path:
        return self.path / "relay.sqlite3"

    def to_record(self) -> dict:
        return {
            "path": str(self.path), "dbPath": str(self.db_path), "source": self.source,
            "detail": self.detail, "socketScope": self.socket_scope,
            "precedence": list(PRECEDENCE), "ambiguous": list(self.ambiguous),
            "unidentified": list(self.unidentified),
        }


def resolve_state_dir(explicit=None, socket_path=None) -> StateSelection:
    """Runtime state lives outside every repository, mirroring the bridge's convention.

    Highest precedence first: an explicit --state, then the environment override, then
    XDG_STATE_HOME, then the home default. Only the last two carry a socket scope; an
    explicit directory is used exactly as given, because the caller has already decided
    which participants share it.
    """
    if explicit:
        return StateSelection(
            Path(explicit).expanduser().absolute(), "flag", f"--state {explicit}", None
        )
    override = os.environ.get(STATE_ENV)
    if override:
        return StateSelection(
            Path(override).expanduser().absolute(), "env", f"{STATE_ENV}={override}", None
        )
    xdg = os.environ.get("XDG_STATE_HOME")
    if xdg:
        base, source, detail = Path(xdg).expanduser(), "xdg", f"XDG_STATE_HOME={xdg}"
    else:
        base, source = Path.home() / ".local" / "state", "home"
        detail = f"default under {Path.home() / '.local' / 'state'}"
    scope = socket_scope(socket_path)
    chosen = (base / "codex-session-relay" / scope).absolute()
    if (chosen / "relay.sqlite3").exists():
        return StateSelection(chosen, source, detail, scope)
    # An existing store keeps its directory. Canonicalising the socket changed this hash, so
    # a relative or symlinked socket that had been running would otherwise point at a fresh
    # empty database while its assignments, generations and pending deliveries sat in the
    # old one, invisible. The new name is used for anything new; the old one wins only when
    # it actually holds a store and the new one does not.
    legacy = legacy_socket_scope(socket_path)
    # Whether the canonical DATABASE exists, not whether its directory does. A directory is
    # created by any command that writes beside the store - a stop request is enough - and
    # testing for the directory let one such command hide a legacy store holding real
    # assignments behind an empty folder.
    if legacy != scope:
        previous = (base / "codex-session-relay" / legacy).absolute()
        if (previous / "relay.sqlite3").exists():
            return StateSelection(
                previous, source,
                f"{detail}; kept the directory this socket was already using",
                legacy,
            )
    # The legacy hash only helps when THIS invocation used the old spelling. A first
    # post-upgrade command that happens to use the absolute path has legacy == scope, so the
    # comparison above never looks at the store the relative spelling created - and creating
    # a canonical database here would hide it for good, because afterwards even the old
    # spelling finds the new one. So before creating anything, ask the stores themselves.
    # Only reached when no canonical database exists yet, which is the one moment it matters.
    claims = stores_claiming_socket(base / "codex-session-relay", socket_path, skip=scope)
    if len(claims) == 1:
        adopted = Path(claims[0])
        return StateSelection(
            adopted, source,
            f"{detail}; adopted the store already recorded for this socket",
            adopted.name,
        )
    if len(claims) > 1:
        # Returning the canonical directory with nothing to say about the conflict is how a
        # THIRD store gets made. The first command to write here creates it, and from that
        # moment the canonical-exists branch at the top of this function wins every later
        # resolution, so both of the real stores - with their assignments, generations and
        # pending deliveries - are invisible. Choosing between them would be just as wrong in
        # a quieter way. So the ambiguity travels with the path and the caller refuses.
        return StateSelection(
            chosen, source,
            f"{detail}; {len(claims)} stores already record this socket",
            scope, tuple(claims),
        )
    # Nothing claims this socket, so a store is about to be created here. A store that
    # predates provenance records no socket at all and its directory hash cannot be inverted,
    # so if one of them IS this socket's - created from a spelling we cannot reconstruct -
    # creating a canonical database now hides it permanently, exactly the way a third store
    # would. Reporting them through doctor alone was not enough, because ordinary commands do
    # not run doctor. Only when we would CREATE: an existing canonical store has already
    # answered the question and returned above.
    unidentified = stores_without_provenance(base / "codex-session-relay", skip=scope)
    if unidentified:
        return StateSelection(
            chosen, source,
            f"{detail}; {len(unidentified)} stores here record no socket",
            scope, (), tuple(unidentified),
        )
    return StateSelection(chosen, source, detail, scope)


def store_socket(db_path) -> str | None:
    """The canonical socket a store recorded for itself, or None if it never recorded one."""
    try:
        connection = sqlite3.connect(f"{Path(db_path).as_uri()}?mode=ro", uri=True, timeout=5)
    except (OSError, sqlite3.Error, ValueError):
        return None
    try:
        row = connection.execute(
            "SELECT value FROM schema_meta WHERE key = 'socket_path'"
        ).fetchone()
    except sqlite3.Error:
        return None
    finally:
        connection.close()
    return row[0] if row else None


def discover_store_for_socket(root, socket_path, *, skip=None):
    """The directory holding the store this socket already has, under any spelling.

    Provenance rather than arithmetic: a hash cannot be inverted, so a store created under a
    spelling we cannot guess is only findable if it says which socket it belongs to. Stores
    record that from now on; one created before it did says nothing and is reported by doctor
    instead of being adopted on a guess.

    Exactly one, because two stores claiming one socket is an ambiguity rather than a choice.
    This answers the narrow question "is there a single store to adopt". A caller that has to
    ACT on the difference between none and several reads stores_claiming_socket, which is the
    one walk both of them share.
    """
    claims = stores_claiming_socket(root, socket_path, skip=skip)
    return Path(claims[0]) if len(claims) == 1 else None


def stores_claiming_socket(root, socket_path, *, skip=None) -> list:
    """Every store recording this socket. More than one is an ambiguity, not a choice."""
    if not socket_path:
        return []
    try:
        candidates = sorted(p for p in Path(root).iterdir() if p.is_dir())
    except OSError:
        return []
    wanted = canonical_socket(socket_path)
    return [
        str(directory) for directory in candidates
        if (skip is None or directory.name != skip)
        and (directory / "relay.sqlite3").exists()
        and store_socket(directory / "relay.sqlite3") == wanted
    ]


def stores_without_provenance(root, *, skip=None) -> list:
    """Store directories that never recorded which socket they serve.

    They cannot be matched to a socket by anything but their directory hash, so a command
    that creates a fresh canonical database beside one of them may be hiding real data.
    Reported rather than adopted: adopting on a guess is how the wrong store gets served.
    """
    try:
        candidates = sorted(p for p in Path(root).iterdir() if p.is_dir())
    except OSError:
        return []
    found = []
    for directory in candidates:
        if skip is not None and directory.name == skip:
            continue
        database = directory / "relay.sqlite3"
        if database.exists() and store_socket(database) is None:
            found.append(str(directory))
    return found
def state_dir(socket_path: str | None = None) -> Path:
    """The directory the environment alone would choose. Kept for callers that have no flag."""
    return resolve_state_dir(None, socket_path).path


class Store:
    def __init__(self, path, socket_path=None):
        path = Path(path)
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_CREAT | os.O_RDWR, 0o600)
        os.close(descriptor)
        self.path = path
        self.db = sqlite3.connect(path, timeout=30, isolation_level=None)
        self.db.row_factory = sqlite3.Row
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA synchronous=FULL")
        self.db.execute("PRAGMA foreign_keys=ON")
        self.db.executescript(DDL)
        # Never fatal. See GUARD_INDEXES: a store that already breaks one of these is the
        # store the contention reporting was written for, and refusing to open it would hide
        # the very state an operator has to see.
        self.unenforced_indexes = []
        for name, statement in GUARD_INDEXES:
            try:
                self.db.execute(statement)
            except sqlite3.IntegrityError as fault:
                self.unenforced_indexes.append({"index": name, "detail": str(fault)})
        self.db.execute(
            "INSERT OR IGNORE INTO schema_meta VALUES ('version', ?)", (str(SCHEMA_VERSION),)
        )
        # Minted once and never rewritten, so reopening a store - or restarting the daemon on
        # it - cannot look like a new store. It is deliberately NOT proof on its own: copying
        # the file copies the identifier too, which is what store_challenge exists for.
        self.db.execute(
            "INSERT OR IGNORE INTO schema_meta VALUES ('store_id', ?)", (uuid.uuid4().hex,)
        )
        self.db.execute(
            "INSERT OR IGNORE INTO schema_meta VALUES ('store_created_at', ?)", (_now_iso(),)
        )
        # An existing store already holds terminal observations, and the scheduler and the
        # health block ask assignment_settlements instead. Leaving it empty on upgrade would
        # make every historical turn look unsettled, so a current turn that can no longer be
        # read would leave a previously settled assignment stalled forever and spending
        # polling budget. Backfilled from the rows that name their relationship; rows written
        # before that column existed name nobody and cannot be attributed to one.
        self.db.execute(
            "INSERT OR IGNORE INTO assignment_settlements (relationship_id, thread_id,"
            " turn_id, terminal_status, settled_at)"
            " SELECT relationship_id, thread_id, turn_id, terminal_status, observed_at"
            "   FROM observations WHERE relationship_id IS NOT NULL"
        )

        if socket_path:
            # Provenance, so this store is findable by the socket it serves rather than only
            # by the hash of whichever spelling created it. INSERT OR IGNORE: the first
            # recording wins, so re-opening through a different spelling never rewrites it.
            self.db.execute(
                "INSERT OR IGNORE INTO schema_meta VALUES ('socket_path', ?)",
                (canonical_socket(socket_path),),
            )
        # Tests set this to prove a transition rolls back; nothing in production assigns it.
        self.fault_hook = None
        # How many composing() scopes are open. Zero is the ordinary store, where opening a
        # transaction inside a transaction is the mistake it has always been.
        self._composing = 0

    # ------------------------------------------------------------------ identity

    def meta(self, key: str):
        row = self.one("SELECT value FROM schema_meta WHERE key = ?", (key,))
        return row["value"] if row is not None else None

    @property
    def identity(self):
        return self.meta("store_id")

    def locate(self) -> dict:
        """The physical facts a same-store comparison needs, not just the path we were given.

        The device and inode come from the RESOLVED path, so a symlink or a bind mount that
        reaches the same bytes compares equal while two genuinely different files do not.

        The number of names the inode has travels with them, because the pair alone cannot
        say whether another participant opened THIS name or another one for the same file.
        `compare_store` is where that is graded.

        The log location travels with them for the same reason, and it is what answers the
        question the count cannot: SQLite writes the write-ahead log beside the pathname a
        connection opened, so two participants share one log when their opened pathnames share
        a directory entry. This one is measured from the pathname THIS live connection actually
        opened, which `PRAGMA database_list` reports, rather than from a held descriptor -
        `Store` has an open connection and no descriptor to bind to, so like the device and
        inode above it is path-measured. `probe` and `nonce_lookup` measure it the stricter way.
        """
        try:
            real = self.path.resolve()
            info = os.stat(real)
            device, inode, real_path = info.st_dev, info.st_ino, str(real)
            links = info.st_nlink
        except OSError:
            device = inode = real_path = None
            links = None
        log = _log_location_of(_opened_pathname(self.db)) or {
            "logDevice": None, "logInode": None, "logName": None,
        }
        return {
            "exists": True,
            "storeId": self.identity,
            "createdAt": self.meta("store_created_at"),
            "dbPath": str(self.path),
            "realPath": real_path,
            "device": device,
            "inode": inode,
            "links": links,
            "logDevice": log["logDevice"],
            "logInode": log["logInode"],
            "logName": log["logName"],
            "schemaVersion": self.meta("version"),
        }

    def write_challenge(self, *, actor: str) -> dict:
        """Leave a value only a participant reading THIS file can find."""
        nonce = uuid.uuid4().hex
        written_at = _now_iso()
        with self.transaction() as db:
            db.execute(
                "INSERT INTO store_challenge (nonce, written_by, written_at) VALUES (?,?,?)",
                (nonce, actor, written_at),
            )
        return {"nonce": nonce, "writtenBy": actor, "writtenAt": written_at}

    def read_challenge(self, nonce: str) -> dict:
        row = self.one("SELECT * FROM store_challenge WHERE nonce = ?", (nonce,))
        if row is None:
            return {"nonce": nonce, "found": False, "writtenBy": None, "writtenAt": None}
        return {
            "nonce": nonce, "found": True, "writtenBy": row["written_by"],
            "writtenAt": row["written_at"],
        }

    @contextmanager
    def transaction(self):
        """BEGIN IMMEDIATE, then commit or roll back. Never a partial record.

        Inside a composing() scope this JOINS the transaction that scope opened instead of
        opening one of its own: it yields the same connection and leaves the commit, the
        rollback and the fault hook to the opener. Everywhere else it is what it was, and
        that includes opening a transaction inside a transaction, which SQLite refuses on one
        connection and which stays an error here rather than becoming a silent join. The
        difference matters because joining changes what a failure costs -- a joined scope that
        raises leaves its writes in somebody else's transaction, and a refusal raised through
        one takes any evidence it wrote down with it. See composing().
        """
        if self._composing and self.db.in_transaction:
            yield self.db
            return
        self.db.execute("BEGIN IMMEDIATE")
        try:
            yield self.db
            if self.fault_hook is not None:
                self.fault_hook()
            self.db.execute("COMMIT")
        except BaseException:
            # COMMIT itself can fail, so it lives inside the protected block. Rolling back
            # is conditional because a failed COMMIT may already have ended the transaction,
            # and a second ROLLBACK would raise over the original error.
            if self.db.in_transaction:
                self.db.execute("ROLLBACK")
            raise

    @contextmanager
    def composing(self):
        """Make every transaction opened inside this block ONE transaction.

        For a command that is one fact written by several existing writers. cmd_register is
        the first: it holds the relationship and the execution settings a later send has to
        preserve, and writing them separately left an interval in which a worker dying
        between the two commits published a live assignment for a task whose settings nobody
        had recorded. Neither writer has to know it was composed, which is what keeps
        registry.register's contract exactly where CRW-127 left it.

        Deliberate, not automatic. Outside this block a nested transaction still raises, so
        the composition is a named act somebody chose rather than a property the store
        quietly acquired.

        The counter is released before the transaction ends, so a caller reading
        in_transaction from the except branch around this block sees the rollback finished
        and can write what had to outlive it.
        """
        with self.transaction() as db:
            self._composing += 1
            try:
                yield db
            finally:
                self._composing -= 1

    @property
    def in_transaction(self) -> bool:
        """Is a transaction open on this store's connection right now?

        Asked by a writer deciding whether what it just wrote is durable. Inside a composed
        registration the answer is yes and the write belongs to somebody else's transaction;
        after that transaction ends the answer is no.
        """
        return self.db.in_transaction

    def journal(self, kind: str, subject: str = "", detail="", *, at: str = "") -> None:
        self.db.execute(
            "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)",
            (at, kind, subject, detail if isinstance(detail, str) else json.dumps(detail)),
        )

    def one(self, sql: str, params=()):
        return self.db.execute(sql, params).fetchone()

    def all(self, sql: str, params=()):
        return self.db.execute(sql, params).fetchall()

    def close(self) -> None:
        self.db.close()


PROVEN, UNPROVEN, MISMATCH = "proven", "unproven", "mismatch"

# The assumption scope.py already makes for I-11. A descriptor can be named, so a read can be
# bound to the file it came from instead of to a name somebody else controls.
PROC_FD = "/proc/self/fd"

# A directory can be opened for its identity alone. O_PATH asks only for search permission on
# the way in and never reads the directory, which is the same Linux assumption PROC_FD makes.
# Absent, the open falls back to an ordinary read-only one rather than failing the measurement.
O_PATH = getattr(os, "O_PATH", 0)


def _opened_pathname(connection):
    """The pathname this connection actually opened, or None.

    Asked rather than assumed, because SQLite resolves the name it is given - a symlinked
    database reaches its target, and the log is then written beside the TARGET. A caller that
    inferred the log's directory from the path it passed in would be wrong in exactly that case.
    """
    try:
        rows = connection.execute("PRAGMA database_list").fetchall()
    except sqlite3.Error:
        return None
    return next((row[2] for row in rows if row[1] == "main" and row[2]), None)


def _log_location_of(pathname):
    """Where a connection on this pathname writes its log: the directory entry, not the string.

    The directory is identified by `device:inode`, which is one number for one directory object
    however many pathnames reach it - the same value from every mount namespace on this kernel.
    The name travels with it because the log is `<name>-wal`, and the name is only fixed before
    resolution: a database reached through a symlink resolves to its target's name.

    Returns the three fields or None. Never raises.
    """
    if not pathname:
        return None
    directory, name = os.path.split(pathname)
    try:
        info = os.stat(directory or ".")
    except OSError:
        return None
    return {"logDevice": info.st_dev, "logInode": info.st_ino, "logName": name}


def _expected_path(db_path):
    """The pathname this store is addressed by, fully resolved, or None.

    Every diagnostic below contracts that failure becomes a field rather than an exception, and
    `Path.resolve` is not documented as total across the supported range (`>=3.11`). On this
    checkout's 3.13 and 3.14 a symlink loop resolves without raising, but the breadth is taken
    rather than argued about, because one escaping exception would break the contract for every
    caller at once.
    """
    try:
        return str(db_path.resolve())
    except (OSError, RuntimeError, ValueError):
        return None


def _hold_database(db_path):
    """Hold the database open, so an answer can name the file it actually came from.

    A pathname cannot carry that claim. Measuring the identity at the path before and after a
    read catches a replacement that PERSISTS and misses one reverted inside the window: both
    observations then report the original inode while the rows came from the interloper.

    Holding it moves the claim onto the descriptor, and the descriptor is then required to still
    name this store. Both halves are needed. Measured on this host on 2026-09-22:

    - normally, SQLite resolves `/proc/self/fd/N` and reports the real path in
      `PRAGMA database_list`, so `-wal` and `-shm` land beside the real file;
    - after a rename OVER the path the held inode has no name, and the open fails closed with
      `OperationalError: unable to open database file` whether or not a file is restored;
    - after a rename that moves the held inode to ANOTHER name, the open succeeds under that
      name - and with a live write-ahead log that read raised `disk I/O error` and CREATED a
      stray `<newname>-wal`, because the log follows the pathname. A diagnostic that promises to
      write nothing must not reach that state, so a descriptor that no longer names this store
      is refused before a connection is opened - every relocation that already happened, which
      is the reachable case, though not one timed inside the call itself.

    Returns `(fd, expected, None)` or `(None, None, detail)`. Never raises.

    Linux-specific, and deliberately the same assumption `scope.AuthorizedFile` already makes.
    Without `/proc/self/fd` a read cannot be bound to the file it came from at all, so it is
    refused rather than answered on the weaker measurement.
    """
    expected = _expected_path(db_path)
    if expected is None:
        return None, None, "the database path could not be resolved, so a read cannot be bound to it"
    if not os.path.isdir(PROC_FD):
        return None, None, (
            f"{PROC_FD} is unavailable, so a read cannot be bound to the database it came from"
        )
    try:
        fd = os.open(db_path, os.O_RDONLY)
    except OSError as error:
        return None, None, f"{type(error).__name__}: {error}"
    moved = _relocation(fd, expected)
    if moved is not None:
        os.close(fd)
        return None, None, moved
    return fd, expected, None


def _relocation(fd, expected):
    """Why the held file is no longer this store, or None while it still is.

    Asked immediately before EVERY connection rather than once at the open. `probe` opens a
    read connection and then a write probe through one descriptor, and a rename between them
    reaches the relocated-log case above, with its failed read and its stray sidecar.

    An observation, not a lock. A rename landing between this answer and SQLite's own open is
    not caught - the same residual as SQLite resolving the descriptor and then opening the name
    it found. What the check removes is every relocation that happened before it was asked,
    which is the reachable case; what it cannot remove is a rename timed inside one call.
    """
    try:
        actual = os.readlink(f"{PROC_FD}/{fd}")
    except OSError as error:
        return f"the database this process holds open cannot be named: {error}"
    if actual != expected:
        return (
            f"the database this process holds open is no longer the file at {expected}, so"
            " nothing was read from it"
        )
    return None


def _held_identity(fd):
    """Device, inode and name count of the file we HOLD, or None if it cannot be measured.

    `st_nlink` is the held inode's own count and equals what `stat` reports for any of its
    names. It is carried for the reason it always was: a shared device and inode cannot say
    which name another participant opened.

    It is no longer the general answer to that, and `compare_store` says so. A file bind mount
    reaches one inode at a second pathname without changing this count, which was measured on
    this host on 2026-09-22. What the count still catches is a name NO participant in a
    comparison accounted for - a third reader, or a hardlinked backup of the state directory -
    which is worth keeping because it needs nothing from the peer.
    """
    try:
        info = os.fstat(fd)
    except OSError:
        return None
    return {"device": info.st_dev, "inode": info.st_ino, "links": info.st_nlink}


def _held_log_location(fd, expected):
    """Where SQLite will write this file's log, bound to the file we HOLD.

    SQLite appends `-wal` to the pathname a connection opened, so the log is created in the
    directory holding that pathname under that pathname's name. Two participants compute one
    log pathname when their opened databases share a directory entry, which is what
    `compare_store` compares - because neither the inode nor the name count can see a second
    pathname for one inode.

    Measured on this host on 2026-09-22 with a real file bind mount, in a private mount
    namespace: one inode, `st_nlink` 1, two pathnames, and the second directory grew its own
    `-wal` and `-shm` while a frame written live through the first name was unreadable through
    the second. The opposite case is a whole-directory bind mount, where two pathname strings
    reach one directory object and the live frame WAS readable through both - so the pathname
    string is not the discriminator and comparing it would refuse a genuinely shared store.

    Bound to the descriptor through the directory entry: the directory is opened for its
    identity alone, and its entry under this basename must be the very inode we hold, so this
    is not a second independent observation of a path somebody else controls. What it cannot
    exclude: Linux offers no descriptor-to-parent link, so the directory is reached by the name
    the kernel reported for the descriptor, and an ancestor renamed inside that bracket is
    I-09's existing residual rather than something this closes.

    Nor does it reach an aliased SIDECAR, and that limit is worth stating plainly because it is
    this function's own hazard one level down. A bind mount over `<name>-wal` leaves this
    directory, this inode and this basename untouched while SQLite opens a different log, so
    two participants can agree here and still write into separate logs. It is not closable by
    the same means: a `-wal` exists only while a connection is open and is unlinked on a clean
    close, so its identity is not a stable thing two independent invocations could compare -
    measuring it would refuse the ordinary case, where each participant's log is a different
    file simply because each opened and closed its own connection. Closing it needs a mechanism
    this one does not have, not a stricter reading of this one.

    Returns the three fields or None. Never raises.
    """
    directory, name = os.path.split(expected)
    try:
        dir_fd = os.open(directory or ".", os.O_RDONLY | os.O_DIRECTORY | O_PATH)
    except OSError:
        return None
    try:
        held = os.fstat(fd)
        where = os.fstat(dir_fd)
        # follow_symlinks=False: `expected` is already resolved, so a final component that is
        # a symlink now is a change under the read, and an entry that is not the held inode
        # means this directory is not where this file's log would go.
        entry = os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
    except OSError:
        return None
    finally:
        os.close(dir_fd)
    if (entry.st_dev, entry.st_ino) != (held.st_dev, held.st_ino):
        return None
    return {"logDevice": where.st_dev, "logInode": where.st_ino, "logName": name}


def _held_uri(fd, mode) -> str:
    """A URI naming the descriptor rather than the path.

    Nothing renameable appears in it, and unlike `Path.as_uri` there is no name to
    percent-encode: a database whose own name contains `?` or `#` reaches SQLite as a number.
    """
    return f"file:{PROC_FD}/{fd}?mode={mode}"


def _opened_elsewhere(connection, expected):
    """Which file SQLite actually opened, when it is not the one we asked for.

    The descriptor is not what SQLite reads through. It resolves `/proc/self/fd/N` to a real
    pathname and opens THAT, on its own descriptor - which is what puts the write-ahead log
    beside the real file, and what leaves a window between our check and its open. So the
    connection is asked which file it opened, and the answer is compared with the pathname this
    store is addressed by. A connection that resolved to any other name is not this store's.

    `PRAGMA database_list` is the only view of that decision `sqlite3` exposes; it reports the
    main database's filename as SQLite computed it. A reading that cannot be obtained is itself
    a refusal, because an unverifiable open is not a verified one.
    """
    try:
        rows = connection.execute("PRAGMA database_list").fetchall()
    except sqlite3.Error as error:
        return f"the database this connection opened could not be named: {error}"
    opened = next((row[2] for row in rows if row[1] == "main"), None)
    if opened != expected:
        return (
            f"this connection opened {opened!r} rather than the database at {expected}, so"
            " nothing was read from it"
        )
    return None


def probe(selection: StateSelection) -> dict:
    """Describe the selected state WITHOUT constructing a Store.

    Store.__init__ creates the directory, opens the file O_RDWR, switches on WAL and runs the
    schema script. Constructing one in order to find out whether that works fails before it
    can report anything, on exactly the host that needed the report. Every step below owns its
    error and becomes a field instead, so this function never raises.
    """
    directory, db_path = selection.path, selection.db_path
    notes = []
    access = {
        "directoryExists": False, "directoryReadable": False, "directoryWritable": False,
        "dbExists": False, "dbReadable": False, "dbWritable": False, "detail": None,
    }
    store = {
        "exists": False, "storeId": None, "createdAt": None, "dbPath": str(db_path),
        "realPath": None, "device": None, "inode": None, "links": None,
        "logDevice": None, "logInode": None, "logName": None, "schemaVersion": None,
    }

    try:
        access["directoryExists"] = directory.is_dir()
    except OSError as error:
        notes.append(f"directory stat failed: {type(error).__name__}: {error}")
    if access["directoryExists"]:
        access["directoryReadable"] = os.access(directory, os.R_OK | os.X_OK)
        # os.access answers for the REAL uid and can disagree with the kernel under a
        # privileged runner or an unusual mount, so writability is measured by writing.
        try:
            with tempfile.NamedTemporaryFile(dir=directory, prefix=".probe-"):
                pass
            access["directoryWritable"] = True
        except OSError as error:
            notes.append(f"directory write failed: {type(error).__name__}: {error}")

    try:
        os.stat(db_path)
        access["dbExists"] = store["exists"] = True
        store["realPath"] = _expected_path(db_path)
        # Deliberately NOT the identity. This stat answers one question - is a file here - and
        # it measured whatever the PATH reached. compare_store grades device and inode as
        # conclusive when they DIFFER, so letting a pathname observation stand in for the
        # identity turns "this read could not be bound to a file" into "this is definitely a
        # different store". The identity comes from the held descriptor below or not at all.
    except OSError as error:
        if access["directoryExists"]:
            notes.append(f"database stat failed: {type(error).__name__}: {error}")

    if access["dbExists"]:
        # The stat above decides only that a file is HERE. It answers for a mode-000 database
        # that os.open refuses, and calling that one absent would be worse than calling it
        # unreadable. Everything this report then CLAIMS about the file comes from one
        # descriptor held across the rest of the section, so nothing the report can observe
        # leaves the identity, the read and the write probe describing different files. The
        # windows that remain are SQLite's own, and _hold_database names them.
        fd, expected, refused = _hold_database(db_path)
        if fd is None:
            notes.append(f"the database could not be held open: {refused}")
        else:
            try:
                held = _held_identity(fd)
                if held is None:
                    # Fail closed rather than carry on. Reading the store id through a
                    # descriptor we cannot identify would hand back an identity with nothing
                    # to attach it to, and the physical identity stays absent - which
                    # compare_store grades unproven rather than as a definite mismatch.
                    notes.append(
                        "the held database could not be identified, so it was not read"
                    )
                else:
                    store.update(held)
                    # Equal to the descriptor's own readlink: _hold_database refused unless
                    # they matched, so this is the path OF THE HELD FILE rather than a second
                    # observation that could have been paired with another file's inode.
                    store["realPath"] = expected
                    # Where a connection on this file writes its log. Assigned one key at a
                    # time on purpose: the trial preflight's field inventory reads this
                    # function for dict literals and subscript assignments, and a merged
                    # update would leave these three invisible to it.
                    located = _held_log_location(fd, expected)
                    if located is None:
                        notes.append(
                            "the log location of the held database could not be measured"
                        )
                    else:
                        store["logDevice"] = located["logDevice"]
                        store["logInode"] = located["logInode"]
                        store["logName"] = located["logName"]

                    moved = _relocation(fd, expected)
                    if moved is not None:
                        notes.append("database read failed: " + moved)
                    else:
                        try:
                            connection = sqlite3.connect(
                                _held_uri(fd, "ro"), uri=True, timeout=5)
                            try:
                                connection.row_factory = sqlite3.Row
                                elsewhere = _opened_elsewhere(connection, expected)
                                if elsewhere is not None:
                                    notes.append("database read failed: " + elsewhere)
                                else:
                                    access["dbReadable"] = True
                                    for key, field in (
                                        ("store_id", "storeId"),
                                        ("store_created_at", "createdAt"),
                                        ("version", "schemaVersion"),
                                    ):
                                        row = connection.execute(
                                            "SELECT value FROM schema_meta WHERE key = ?",
                                            (key,),
                                        ).fetchone()
                                        # A store written before identity existed has no row
                                        # here. Absence is reported as absence and never
                                        # defaulted, because a default could later compare
                                        # equal to another store's and be read as proof.
                                        store[field] = row["value"] if row is not None else None
                            finally:
                                connection.close()
                        except (OSError, sqlite3.Error, TypeError, ValueError) as error:
                            notes.append(
                                f"database read failed: {type(error).__name__}: {error}")

                    # Asked again, and it closes two things. A rename between the two
                    # connections would put the write probe on a relocated name, where SQLite
                    # was measured to fail AND leave a stray log behind. It is also the closing
                    # question for the read that just happened: a store that moved during it
                    # leaves an identity a caller reads as "the store at this path", so what
                    # that read published is withdrawn rather than reported.
                    moved = _relocation(fd, expected)
                    if moved is not None:
                        notes.append("the database moved while it was being read: " + moved)
                        access["dbReadable"] = False
                        store["storeId"] = store["createdAt"] = None
                        store["schemaVersion"] = None
                        store["device"] = store["inode"] = store["links"] = None
                        store["logDevice"] = store["logInode"] = store["logName"] = None
                    else:
                        try:
                            connection = sqlite3.connect(
                                _held_uri(fd, "rw"), uri=True, timeout=5, isolation_level=None
                            )
                            try:
                                connection.execute("BEGIN IMMEDIATE")
                                connection.execute("ROLLBACK")
                                elsewhere = _opened_elsewhere(connection, expected)
                                if elsewhere is not None:
                                    notes.append("database write probe failed: " + elsewhere)
                                else:
                                    # Acquiring a write transaction is evidence that this
                                    # process can write NOW. It is not a promise that a later
                                    # commit succeeds; a full disk still fails.
                                    access["dbWritable"] = True
                            finally:
                                connection.close()
                        except (OSError, sqlite3.Error) as error:
                            notes.append(
                                f"database write probe failed: {type(error).__name__}: {error}")
            finally:
                os.close(fd)

    access["detail"] = "; ".join(notes) or None
    return {"stateSelection": selection.to_record(), "store": store, "access": access}


def read_only_rows(selection: StateSelection, sql: str, params=()) -> dict:
    """Answer a question about the store without creating or migrating one.

    Store.__init__ opens the file O_RDWR, switches on WAL and runs the whole schema script,
    so any command that reaches for it to READ leaves a fully formed relay database behind.
    For diagnosis that is a side effect the command promised not to have: pointing it at an
    empty, legacy or unrelated file would silently adopt it. Every error becomes a field.

    The identity returned with the rows is the identity of the file this read HELD OPEN, and
    the read is refused unless that file is still the one at this store's pathname. A caller
    that stat'd the path earlier cannot otherwise tell that the rows arrived from a
    replacement: comparing the store id does not settle it, because the id is minted once and
    travels with a copy of the bytes. `_hold_database` states what the descriptor catches and
    what it does not.
    """
    db_path = selection.db_path

    unknown = {"device": None, "inode": None, "links": None}
    fd, expected, refused = _hold_database(db_path)
    if fd is None:
        return {**unknown, "readable": False, "rows": [], "detail": refused}
    try:
        opened = _held_identity(fd)
        if opened is None:
            return {**unknown, "readable": False, "rows": [],
                    "detail": "the database could not be identified while it was being read"}
        moved = _relocation(fd, expected)
        if moved is not None:
            return {**unknown, "readable": False, "rows": [], "detail": moved}
        try:
            connection = sqlite3.connect(_held_uri(fd, "ro"), uri=True, timeout=5)
        except (OSError, sqlite3.Error, ValueError) as error:
            return {**unknown, "readable": False, "rows": [],
                    "detail": f"{type(error).__name__}: {error}"}
        try:
            connection.row_factory = sqlite3.Row
            # Before the statement runs: which file did SQLite itself open?
            elsewhere = _opened_elsewhere(connection, expected)
            if elsewhere is not None:
                return {**unknown, "readable": False, "rows": [], "detail": elsewhere}
            rows = [dict(row) for row in connection.execute(sql, params).fetchall()]
        except sqlite3.Error as error:
            # Ask why before reporting what. A store moved out from under the read fails the
            # statement too, and calling that a readable database whose query failed would
            # describe the symptom while hiding the cause.
            moved = _relocation(fd, expected)
            if moved is not None:
                return {**unknown, "readable": False, "rows": [], "detail": moved}
            return {**unknown, "readable": True, "rows": [],
                    "detail": f"{type(error).__name__}: {error}"}
        finally:
            connection.close()
        # Asked once more, now that the read is over. The pre-connect answer says the file was
        # this store when the read started; without this one, a rename during the read would
        # still return rows and an identity a caller reads as "the store at this path". The
        # claim these two make together is that the file was the one at this pathname for the
        # whole read.
        moved = _relocation(fd, expected)
        if moved is not None:
            return {**unknown, "readable": False, "rows": [], "detail": moved}
        # Both of this read's own observations, carried as the larger count, for the reason
        # nonce_lookup has always carried it: a second name present at the open and unlinked
        # before the close is the same hazard as one that stayed, because a peer that already
        # opened the removed alias keeps writing through its own write-ahead log.
        closed = _held_identity(fd)
        links = max(seen["links"] for seen in (opened, closed) if seen is not None)
        return {**opened, "links": links, "readable": True, "rows": rows, "detail": None}
    finally:
        os.close(fd)


def nonce_lookup(selection: StateSelection, nonce: str) -> dict:
    """Look for a challenge nonce read-only, so a comparison never writes to the store.

    The identity of the file it read comes back with the answer, because this is the only
    evidence `compare_store` grades as proof. That identity is the HELD file's, and the read
    is refused unless the held file is still the one at this store's pathname. Without it, a
    nonce found in a database that replaced the measured one satisfies the one proving
    mechanism there is, and a copy carries the challenge row with the bytes, so the
    replacement need not even be crafted.

    `_hold_database` states what that refusal reaches and what it does not; `compare_store`
    grades an unattributable answer as unproven, never as absence.
    """
    db_path = selection.db_path
    unknown = {"device": None, "inode": None, "links": None,
               "logDevice": None, "logInode": None, "logName": None}
    fd, expected, refused = _hold_database(db_path)
    if fd is None:
        return {**unknown, "nonce": nonce, "found": False, "readable": False,
                "detail": refused}
    try:
        opened = _held_identity(fd)
        if opened is None:
            return {**unknown, "nonce": nonce, "found": False, "readable": False,
                    "detail": "the database could not be identified while the nonce was"
                              " being read"}
        # Unreadable rather than absent, for the reason the query failure below gives: an
        # answer that cannot be attributed to a file is not an answer about any store.
        moved = _relocation(fd, expected)
        if moved is not None:
            return {**unknown, "nonce": nonce, "found": False, "readable": False,
                    "detail": moved}
        try:
            connection = sqlite3.connect(_held_uri(fd, "ro"), uri=True, timeout=5)
        except (OSError, sqlite3.Error) as error:
            return {**unknown, "nonce": nonce, "found": False, "readable": False,
                    "detail": f"{type(error).__name__}: {error}"}
        try:
            connection.row_factory = sqlite3.Row
            elsewhere = _opened_elsewhere(connection, expected)
            if elsewhere is not None:
                return {**unknown, "nonce": nonce, "found": False, "readable": False,
                        "detail": elsewhere}
            row = connection.execute(
                "SELECT written_by, written_at FROM store_challenge WHERE nonce = ?", (nonce,)
            ).fetchone()
        except sqlite3.Error as error:
            # NOT readable. A locked, malformed or momentarily unavailable database answers no
            # question, and calling it readable turns "we could not look" into "it is not
            # there", which compare_store then grades as a definite store mismatch.
            moved = _relocation(fd, expected)
            if moved is not None:
                return {**unknown, "nonce": nonce, "found": False, "readable": False,
                        "detail": moved}
            return {**unknown, "nonce": nonce, "found": False, "readable": False,
                    "detail": f"{type(error).__name__}: {error}"}
        finally:
            connection.close()
        # The same closing question the row leg asks, and it matters more here: this answer is
        # the only evidence compare_store grades as proof, so it must not survive the store
        # moving out from under it mid-read.
        moved = _relocation(fd, expected)
        if moved is not None:
            return {**unknown, "nonce": nonce, "found": False, "readable": False,
                    "detail": moved}
        # Both of this read's own observations, carried as the larger count. A second name
        # present at the open and unlinked before the close leaves the closing count at one,
        # and a peer that already opened the removed alias can hold that connection and keep
        # writing through its own write-ahead log. Reporting only the closing count kept half
        # of what was measured.
        closed = _held_identity(fd)
        # The log location of the file this answer was read through, so compare_store can ask
        # whether the nonce came from the same log the store it is grading writes into. These
        # are two independent opens - probe holds its own descriptor - so the answer is not
        # trivially the probe's, which is the same reason the device and inode travel here.
        located = _held_log_location(fd, expected) or {
            "logDevice": None, "logInode": None, "logName": None}
        seen = {**opened, **located,
                "links": max(count["links"] for count in (opened, closed) if count is not None)}
        if row is None:
            return {**seen, "nonce": nonce, "found": False, "readable": True, "detail": None}
        return {**seen, "nonce": nonce, "found": True, "readable": True, "detail": None,
                "writtenBy": row["written_by"], "writtenAt": row["written_at"]}
    finally:
        os.close(fd)


def compare_store(store: dict, *, expect_store=None, expect_inode=None, expect_log=None,
                  nonce=None) -> dict:
    """Grade the evidence that this participant and another share ONE store.

    Conflicting evidence is decided before agreeing evidence, so an easier comparison that
    happened to succeed can never talk a mismatch down. Absence is never agreement: a store
    that cannot state its identity is unproven, not proven, because the criterion is that a
    different database must never be reported as healthy.

    What each piece of evidence can carry differs, and the grades follow that. A store id is
    minted once and copied with the bytes, so agreement is never proof. A device and inode
    pair is conclusive when it DIFFERS and insufficient when it agrees, because one inode can
    be reached at more than one pathname and SQLite derives the write-ahead log from the
    pathname a connection opens. What answers THAT is the log location: the directory entry a
    connection's `-wal` would be created under, which each participant measures for itself and
    reports, and which two participants agree on exactly when they compute one log pathname.
    A nonce is the only live evidence here - the peer's write is readable in the file being
    read - and it is what the contract designates as proof.

    Agreement on the log location is a SUFFICIENT condition for one log rather than an
    equivalence, and the grades follow that difference. A disagreement is unproven, not a
    mismatch: it does not establish that the two logs are different FILES, because the sidecars
    can themselves be aliased, an overlay merged path and its upperdir can differ as directories
    while the log entry is one file, and SQLite documents the `-wal` suffix as what it usually
    appends rather than a guarantee. Read under the default unix VFS with unaliased sidecars -
    and the converse of that assumption is a real gap rather than a formality: a participant
    with a bind mount over its own `-wal` agrees with this comparison and writes elsewhere.
    `_held_log_location` says why measuring the log file itself does not fix it.

    Being the only proof, it has to be evidence about THIS file. The answer carries the
    identity of the file it was read from, because it comes from a second open of the path,
    and a found nonce is graded as proof only when that matches the store being compared. An
    answer that cannot be attributed is unproven rather than a mismatch: it says nothing about
    whether two participants share a store, only that this reading is not about the one here.

    And being live is not the same as being current. What a found nonce says is that the file
    read here contains a write that was made to the writer's file at some earlier moment - a
    copy taken AFTER the challenge was written carries it with the bytes, and stays stable for
    a whole invocation, so nothing looking for a replacement or a second name sees anything
    wrong. Whether it is still one file is what the physical identity answers. So proof takes
    all three: a found, attributed nonce AND an agreeing device and inode AND an agreeing log
    location. None of them alone is graded as proof, and each is unproven for its own reason.
    The store id is not one of the three: supplied and disagreeing it is a mismatch, supplied
    and agreeing it is agreement, and absent it was not asked - the role it already had.

    The name count is graded beside all of that rather than folded into any of it, and it no
    longer carries the second-pathname question alone. `st_nlink` counts hardlink names, and a
    file bind mount adds a pathname without changing it: measured on this host on 2026-09-22,
    where exactly that mount reached `proven` on an agreeing pair and a found nonce while the
    two names kept separate logs. The log location is the general answer; the count is the
    residual guard for a name NO participant in this comparison accounted for, and the only one
    left to a caller that sends no log location at all.
    """
    reasons = []
    if expect_store is not None:
        if store.get("storeId") is None:
            reasons.append((UNPROVEN, "this store states no identity, so it cannot be compared"))
        elif store["storeId"] != expect_store:
            reasons.append((MISMATCH, f"store id {store['storeId']} is not {expect_store}"))
        else:
            reasons.append((None, "store id matches"))
    if expect_inode is not None:
        want = str(expect_inode).split(":")
        here = (store.get("device"), store.get("inode"))
        physical = None
        if len(want) != 2 or None in here:
            reasons.append((UNPROVEN, "physical identity is not comparable here"))
        elif (str(here[0]), str(here[1])) != (want[0], want[1]):
            physical = False
            reasons.append((
                MISMATCH, f"device:inode {here[0]}:{here[1]} is not {expect_inode}",
            ))
        else:
            physical = True
            # Agreement, not proof. Two pathnames for one inode agree here and still keep
            # separate write-ahead logs, and this side cannot see the second pathname.
            reasons.append((None, (
                "device and inode match, which does not say both participants opened the"
                " same pathname for that inode"
            )))
    else:
        # Not compared at all, which is not the same as compared and agreeing.
        physical = None
    log_location = None
    if expect_log is not None:
        # <device>:<inode>:<name>, split at most twice so a database whose own name contains a
        # colon survives the parse.
        want = str(expect_log).split(":", 2)
        mine = (store.get("logDevice"), store.get("logInode"), store.get("logName"))
        if len(want) != 3 or None in mine:
            reasons.append((UNPROVEN, "the log location is not comparable here"))
        elif (str(mine[0]), str(mine[1]), mine[2]) != (want[0], want[1], want[2]):
            log_location = False
            reasons.append((UNPROVEN, (
                f"this database's write-ahead log is written beside {mine[0]}:{mine[1]}/{mine[2]}"
                f" and the other participant reported {expect_log}, so the two were not shown to"
                " write into one log"
            )))
        else:
            log_location = True
            reasons.append((None, (
                "both participants write their write-ahead log under the same directory entry"
            )))
    if nonce is not None:
        if nonce.get("readable") is False:
            # Not being able to read is not the same as the nonce being absent. Calling it a
            # mismatch would tell an operator two participants use different stores when the
            # truth is that this one merely could not look.
            reasons.append((UNPROVEN, f"the nonce could not be read here: {nonce.get('detail')}"))
        elif nonce.get("found"):
            read_from = (nonce.get("device"), nonce.get("inode"))
            here = (store.get("device"), store.get("inode"))
            # The same attribution question one level along. `probe` and `nonce_lookup` are two
            # independent opens, so an answer can carry this store's device and inode and still
            # have been read through a pathname whose log is a different file. Both sides must
            # have been measured, and an absent one is refused rather than read as agreement -
            # the rule the device and inode above already follow, and the one this module
            # states: absence is never agreement. The two are separate answers, so they are
            # reported separately: could not look, and looked somewhere else.
            read_log = (nonce.get("logDevice"), nonce.get("logInode"), nonce.get("logName"))
            mine_log = (store.get("logDevice"), store.get("logInode"), store.get("logName"))
            if None in read_from or None in here or read_from != here:
                reasons.append((UNPROVEN, (
                    f"the nonce was read from device:inode {read_from[0]}:{read_from[1]}, and"
                    f" this comparison is about {here[0]}:{here[1]}"
                )))
            elif log_location is True and (None in read_log or None in mine_log):
                reasons.append((UNPROVEN, (
                    "the log location the nonce was read through could not be measured, so the"
                    " nonce cannot be attributed to the log this comparison is about"
                )))
            elif log_location is True and read_log != mine_log:
                reasons.append((UNPROVEN, (
                    "the nonce was read through a pathname whose write-ahead log is not the one"
                    " this comparison is about"
                )))
            elif physical is True and log_location is True:
                reasons.append((
                    PROVEN,
                    "a nonce written by another participant is readable here, in the file this"
                    " comparison is about, whose write-ahead log is written where that"
                    " participant reported writing its own",
                ))
            else:
                # Only name what was never supplied. An expectation that WAS supplied and
                # disagreed has already refused above, and asking for it again would tell an
                # operator to send something they just sent.
                wanted = [flag for flag, seen in (
                    ("--expect-inode", physical), ("--expect-log", log_location),
                ) if seen is None]
                if wanted:
                    reasons.append((UNPROVEN, (
                        "a nonce written by another participant is readable here, which does not"
                        " say the two are one live store: a copy taken after the challenge was"
                        " written carries the nonce with the bytes, and one inode reached at a"
                        " second pathname keeps a write-ahead log of its own. Supply the other"
                        f" participant's {' and '.join(wanted)}"
                    )))
        else:
            reasons.append((MISMATCH, "a nonce written by another participant is not here"))

    if reasons:
        # One detected case of a second pathname, refused outright. Measured on this host on
        # 2026-09-17: with a store open on one name, a read through a hardlinked second name
        # failed with `OperationalError: disk I/O error` while the first connection's log was
        # live, and after that connection closed and checkpointed the second name grew its own
        # -wal and -shm. Two participants can therefore agree on device, inode AND store id,
        # and read a nonce one of them wrote, while still not writing into one live store - so
        # this refuses even a found nonce. It is not the general answer: a bind mount reaches
        # one inode at a second pathname without changing st_nlink, which is why an agreeing
        # device and inode is graded as agreement rather than proof above. That last part is
        # the documented behaviour of a mount entry rather than something measured here - this
        # host refuses an unprivileged mount namespace - and the grading above does not depend
        # on it: an agreeing pair is not proof whether or not the extra pathname is countable.
        #
        # Every count that was measured is consulted, not just the caller's. The nonce answer
        # carries the count seen at ITS read, and grading only the earlier one took half of a
        # fresher measurement and left the other half: a hardlink created between the two
        # leaves device and inode untouched, so a stale count of one could not veto a nonce
        # found through the original name. A second name at either moment is the same hazard.
        counted = [count for count in (store.get("links"), (nonce or {}).get("links"))
                   if count is not None]
        if not counted:
            reasons.append((
                UNPROVEN, "the number of names this database has could not be measured",
            ))
        elif max(counted) > 1:
            reasons.append((UNPROVEN, (
                f"this database has {max(counted)} names, so a shared device and inode cannot"
                " say which one the other participant opened, and each name carries its own"
                " write-ahead log"
            )))

    if not reasons:
        return {"sameStore": UNPROVEN, "detail": "no expectation was supplied to compare against"}
    for verdict in (MISMATCH, UNPROVEN, PROVEN):
        matched = [detail for grade, detail in reasons if grade == verdict]
        if matched:
            if verdict is PROVEN and any(g == UNPROVEN for g, _ in reasons):
                continue
            return {"sameStore": verdict, "detail": "; ".join(matched)}
    # Every expectation agreed and none of them was live evidence. A copy of the file carries
    # the store id, and an agreeing device and inode does not say the two participants opened
    # one pathname for it, so neither is proof however they are combined.
    agreed = [detail for grade, detail in reasons if grade is None]
    return {
        "sameStore": UNPROVEN,
        "detail": (
            f"{'; '.join(agreed)}. Neither a store id nor a device and inode pair is live"
            " evidence, so supply a nonce for proof"
        ),
    }
