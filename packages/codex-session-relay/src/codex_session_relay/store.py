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
CREATE INDEX IF NOT EXISTS sync_ready ON sync_outbox (state, next_attempt_at);
"""


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

    @property
    def db_path(self) -> Path:
        return self.path / "relay.sqlite3"

    def to_record(self) -> dict:
        return {
            "path": str(self.path), "dbPath": str(self.db_path), "source": self.source,
            "detail": self.detail, "socketScope": self.socket_scope,
            "precedence": list(PRECEDENCE),
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
    adopted = discover_store_for_socket(base / "codex-session-relay", socket_path, skip=scope)
    if adopted is not None:
        return StateSelection(
            adopted, source,
            f"{detail}; adopted the store already recorded for this socket",
            adopted.name,
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
    """
    if not socket_path:
        return None
    try:
        candidates = sorted(p for p in Path(root).iterdir() if p.is_dir())
    except OSError:
        return None
    wanted = canonical_socket(socket_path)
    matches = []
    for directory in candidates:
        if skip is not None and directory.name == skip:
            continue
        database = directory / "relay.sqlite3"
        if not database.exists():
            continue
        if store_socket(database) == wanted:
            matches.append(directory)
    if len(matches) == 1:
        return matches[0]
    # Zero is the ordinary case. More than one means a copied store or separate explicit-state
    # runs left two databases claiming the same socket, and picking whichever sorts first
    # would silently operate on one set of assignments today and the other after a rename.
    # Adopting nothing sends the caller to a fresh canonical store, which is wrong but VISIBLE:
    # doctor reports the candidates and --state resolves it.
    return None


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
        """
        try:
            real = self.path.resolve()
            info = os.stat(real)
            device, inode, real_path = info.st_dev, info.st_ino, str(real)
        except OSError:
            device = inode = real_path = None
        return {
            "exists": True,
            "storeId": self.identity,
            "createdAt": self.meta("store_created_at"),
            "dbPath": str(self.path),
            "realPath": real_path,
            "device": device,
            "inode": inode,
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
        """BEGIN IMMEDIATE, then commit or roll back. Never a partial record."""
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
        "realPath": None, "device": None, "inode": None, "schemaVersion": None,
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
        info = os.stat(db_path)
        access["dbExists"] = store["exists"] = True
        store["realPath"] = str(db_path.resolve())
        store["device"], store["inode"] = info.st_dev, info.st_ino
    except OSError as error:
        if access["directoryExists"]:
            notes.append(f"database stat failed: {type(error).__name__}: {error}")

    if access["dbExists"]:
        try:
            connection = sqlite3.connect(f"{db_path.as_uri()}?mode=ro", uri=True, timeout=5)
            try:
                connection.row_factory = sqlite3.Row
                access["dbReadable"] = True
                for key, field in (
                    ("store_id", "storeId"), ("store_created_at", "createdAt"),
                    ("version", "schemaVersion"),
                ):
                    row = connection.execute(
                        "SELECT value FROM schema_meta WHERE key = ?", (key,)
                    ).fetchone()
                    # A store written before identity existed has no row here. Absence is
                    # reported as absence and never defaulted, because a default could later
                    # compare equal to another store's and be read as proof.
                    store[field] = row["value"] if row is not None else None
            finally:
                connection.close()
        except (OSError, sqlite3.Error, TypeError, ValueError) as error:
            notes.append(f"database read failed: {type(error).__name__}: {error}")

        try:
            connection = sqlite3.connect(
                f"{db_path.as_uri()}?mode=rw", uri=True, timeout=5, isolation_level=None
            )
            try:
                connection.execute("BEGIN IMMEDIATE")
                connection.execute("ROLLBACK")
                # Acquiring a write transaction is evidence that this process can write NOW.
                # It is not a promise that a later commit succeeds; a full disk still fails.
                access["dbWritable"] = True
            finally:
                connection.close()
        except (OSError, sqlite3.Error) as error:
            notes.append(f"database write probe failed: {type(error).__name__}: {error}")

    access["detail"] = "; ".join(notes) or None
    return {"stateSelection": selection.to_record(), "store": store, "access": access}


def read_only_rows(selection: StateSelection, sql: str, params=()) -> dict:
    """Answer a question about the store without creating or migrating one.

    Store.__init__ opens the file O_RDWR, switches on WAL and runs the whole schema script,
    so any command that reaches for it to READ leaves a fully formed relay database behind.
    For diagnosis that is a side effect the command promised not to have: pointing it at an
    empty, legacy or unrelated file would silently adopt it. Every error becomes a field.
    """
    db_path = selection.db_path
    try:
        connection = sqlite3.connect(f"{db_path.as_uri()}?mode=ro", uri=True, timeout=5)
    except (OSError, sqlite3.Error, ValueError) as error:
        return {"readable": False, "rows": [],
                "detail": f"{type(error).__name__}: {error}"}
    try:
        connection.row_factory = sqlite3.Row
        rows = [dict(row) for row in connection.execute(sql, params).fetchall()]
    except sqlite3.Error as error:
        connection.close()
        return {"readable": True, "rows": [], "detail": f"{type(error).__name__}: {error}"}
    connection.close()
    return {"readable": True, "rows": rows, "detail": None}


def nonce_lookup(selection: StateSelection, nonce: str) -> dict:
    """Look for a challenge nonce read-only, so a comparison never writes to the store."""
    db_path = selection.db_path
    try:
        connection = sqlite3.connect(f"{db_path.as_uri()}?mode=ro", uri=True, timeout=5)
    except (OSError, sqlite3.Error) as error:
        return {"nonce": nonce, "found": False, "readable": False,
                "detail": f"{type(error).__name__}: {error}"}
    try:
        connection.row_factory = sqlite3.Row
        row = connection.execute(
            "SELECT written_by, written_at FROM store_challenge WHERE nonce = ?", (nonce,)
        ).fetchone()
    except sqlite3.Error as error:
        # NOT readable. A locked, malformed or momentarily unavailable database answers no
        # question, and calling it readable turns "we could not look" into "it is not there",
        # which compare_store then grades as a definite store mismatch.
        return {"nonce": nonce, "found": False, "readable": False,
                "detail": f"{type(error).__name__}: {error}"}
    finally:
        connection.close()
    if row is None:
        return {"nonce": nonce, "found": False, "readable": True, "detail": None}
    return {"nonce": nonce, "found": True, "readable": True, "detail": None,
            "writtenBy": row["written_by"], "writtenAt": row["written_at"]}


def compare_store(store: dict, *, expect_store=None, expect_inode=None, nonce=None) -> dict:
    """Grade the evidence that this participant and another share ONE store.

    Conflicting evidence is decided before agreeing evidence, so an easier comparison that
    happened to succeed can never talk a mismatch down. Absence is never agreement: a store
    that cannot state its identity is unproven, not proven, because the criterion is that a
    different database must never be reported as healthy.
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
        if len(want) != 2 or None in here:
            reasons.append((UNPROVEN, "physical identity is not comparable here"))
        elif (str(here[0]), str(here[1])) != (want[0], want[1]):
            reasons.append((
                MISMATCH, f"device:inode {here[0]}:{here[1]} is not {expect_inode}",
            ))
        else:
            reasons.append((PROVEN, "same device and inode"))
    if nonce is not None:
        if nonce.get("readable") is False:
            # Not being able to read is not the same as the nonce being absent. Calling it a
            # mismatch would tell an operator two participants use different stores when the
            # truth is that this one merely could not look.
            reasons.append((UNPROVEN, f"the nonce could not be read here: {nonce.get('detail')}"))
        elif nonce.get("found"):
            reasons.append((PROVEN, "a nonce written by another participant is readable here"))
        else:
            reasons.append((MISMATCH, "a nonce written by another participant is not here"))

    if not reasons:
        return {"sameStore": UNPROVEN, "detail": "no expectation was supplied to compare against"}
    for verdict in (MISMATCH, UNPROVEN, PROVEN):
        matched = [detail for grade, detail in reasons if grade == verdict]
        if matched:
            if verdict is PROVEN and any(g == UNPROVEN for g, _ in reasons):
                continue
            return {"sameStore": verdict, "detail": "; ".join(matched)}
    # Every expectation agreed, but none of them was physical or live evidence: an identical
    # store id alone is satisfied by a copy of the file, so this is not proof.
    return {
        "sameStore": UNPROVEN,
        "detail": "only the store id was compared, and copying a database copies it too;"
                  " supply --expect-inode or a nonce for proof",
    }
