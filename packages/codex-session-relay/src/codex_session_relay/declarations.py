"""What a child's own relay records in the relay store beside the marker facts it writes.

The Stop hook and CRW-180's reader read the marker, and keep reading it. The relay daemon reads
no marker file (hook-contract.md), so a turn that ended without a report is invisible to it
unless the declarations a child DID make are in the store as well: a turn whose outcome was
declared looks, from the store alone, exactly like one that declared nothing. Two records close
that, each written by the command that writes the marker fact and AFTER it, mirroring the fact
the marker then stands on rather than the caller's arguments - so a create-once conflict in the
marker cannot leave the two disagreeing.

- intent-claim records that this session's relay writes its declarations into this store. It is
  the cut-over omitted.derive relies on: a turn whose session carries no such record is a legacy
  admission, and nothing about it is derived from the store.
- intent-disposition records the turn's declared outcome.

Neither record replaces or gates the marker write. A store that cannot be written is reported
in the command's answer and its exit status, never dropped; a coordinator that recorded no store
at all is reported too, and is not a failure, because there is then no store to derive from.
"""

import sqlite3
import stat
from pathlib import Path

from .store import Store

CAPABILITY = "declarations/1"

RECORDED = "recorded"
UNCHANGED = "unchanged"
CONFLICT = "conflict"
NOT_RECORDED = "not_recorded"
FAILED = "failed"


def store_of(facts) -> str | None:
    """The store the coordinator recorded for this assignment, or None.

    A marker whose intent is not an object names no store; reading it as one crashed the
    command after its marker write and before it could say so.
    """
    declared = facts.get("intent") if isinstance(facts, dict) else None
    path = declared.get("dbPath") if isinstance(declared, dict) else None
    return path if isinstance(path, str) and path.strip() else None


def not_recorded(reason, detail, db_path=None) -> dict:
    return {"recorded": False, "state": NOT_RECORDED, "reason": reason, "store": db_path,
            "detail": detail}


def failure(reason, detail, db_path) -> dict:
    return {"recorded": False, "state": FAILED, "reason": reason, "store": db_path,
            "detail": detail + ". The marker fact stands; running the same command again"
                               " retries this record and changes nothing else"}


def _open(db_path):
    """(the store to record in, None), or (None, the answer saying why there is none)."""
    if db_path is None:
        return None, not_recorded(
            "no_store_recorded",
            "the assignment's intent names no relay store, so there is none to record this in;"
            " the relay derives nothing from a store for this assignment")
    path = Path(db_path).expanduser()
    # Asked through stat rather than a predicate that swallows the error: "there is no store"
    # is not a failure, because nothing can be derived from a store that does not exist, but
    # "the store could not be looked at" is - a declaration dropped there leaves a store that
    # derives an omission for a turn that declared.
    try:
        metadata = path.stat()
    except FileNotFoundError:
        return None, not_recorded(
            "store_absent",
            "the relay store the intent names does not exist, so nothing can be derived from"
            " it either; nothing was created", str(path))
    except OSError as error:
        return None, failure("store_unreadable", type(error).__name__ + ": " + str(error),
                             str(path))
    if not stat.S_ISREG(metadata.st_mode):
        return None, failure("store_not_a_file",
                             "the path the intent names is not a regular file", str(path))
    try:
        return Store(path), None
    except (OSError, sqlite3.Error) as error:
        return None, failure("store_unopenable", type(error).__name__ + ": " + str(error),
                             str(path))


def _record_in(db, path, select, insert, same):
    """Insert once, or compare with what stands, on a connection whose write is already open."""
    existing = db.execute(*select).fetchone()
    if existing is None:
        db.execute(*insert)
        return {"recorded": True, "state": RECORDED, "reason": None, "store": str(path),
                "detail": None}
    if same(existing):
        return {"recorded": False, "state": UNCHANGED, "reason": None, "store": str(path),
                "detail": "already recorded, identically"}
    return {"recorded": False, "state": CONFLICT, "reason": "store_disagrees",
            "store": str(path),
            "detail": "this store already holds a different record for it, and the first"
                      " one stands, as it does in the marker: " + repr(dict(existing))}


def _write(db_path, select, insert, same):
    """Insert once in a write of its own. Returns the record for the command's answer."""
    store, problem = _open(db_path)
    if store is None:
        return problem
    try:
        with store.transaction() as db:
            return _record_in(db, store.path, select, insert, same)
    except (OSError, sqlite3.Error) as error:
        return failure("store_write_failed", type(error).__name__ + ": " + str(error),
                       str(store.path))
    finally:
        store.close()


class Held:
    """The store's write lock, held across a marker publication and the record mirroring it.

    A disposition published to the marker and recorded in the store a moment later left a gap
    in which the store still said the turn declared nothing: an automatic pass reading it then
    derived an omission for a turn the child had just declared, and woke the supervisor. So the
    lock is taken FIRST, the marker is published and read back under it, the record is written
    in the same write, and only then is anything committed. Every write that could act on the
    omission - staging, a claim, a transport start - takes the same lock and so sees the
    declaration. The pattern intent-register already uses for its generation check.

    Never gates the marker write: a store that cannot be opened or locked is only the answer
    this gives, and the publication goes ahead without it.
    """

    def __init__(self, db_path):
        self.store, self.problem = _open(db_path)
        self.commit_error = None
        if self.store is not None:
            try:
                self.store.db.execute("BEGIN IMMEDIATE")
            except sqlite3.Error as error:
                self.store.close()
                self.store = None
                self.problem = failure("store_locked", type(error).__name__ + ": " + str(error),
                                       str(Path(db_path).expanduser()))

    def __enter__(self):
        return self

    def __exit__(self, kind, value, traceback):
        if self.store is None:
            return False
        try:
            if kind is None:
                try:
                    self.store.db.execute("COMMIT")
                except sqlite3.Error as error:
                    self.commit_error = error
            if self.store.db.in_transaction:
                self.store.db.execute("ROLLBACK")
        finally:
            self.store.close()
        return False

    def disposition(self, *, assignment, session_id, turn_id, outcome, declared_at, at) -> dict:
        """Record the outcome this turn declared, create-once like the marker file it mirrors."""
        if self.store is None:
            return self.problem
        try:
            return _record_in(
                self.store.db, self.store.path,
                ("SELECT outcome FROM turn_declarations WHERE assignment_id = ?"
                 " AND session_id = ? AND turn_id = ?", (assignment, session_id, turn_id)),
                ("INSERT INTO turn_declarations (assignment_id, session_id, turn_id, outcome,"
                 " declared_at, recorded_at) VALUES (?,?,?,?,?,?)",
                 (assignment, session_id, turn_id, outcome, declared_at, at)),
                lambda row: row["outcome"] == outcome)
        except sqlite3.Error as error:
            return failure("store_write_failed", type(error).__name__ + ": " + str(error),
                           str(self.store.path))

    def settled(self, record) -> dict:
        """The record as it stands once the write has ended: a commit that failed undoes it."""
        if self.commit_error is not None and record.get("state") == RECORDED:
            return failure("store_write_failed", type(self.commit_error).__name__ + ": "
                           + str(self.commit_error), record.get("store"))
        return record


def record_claim(db_path, *, assignment, session_id, dispatch_request_id, marker_root,
                 workspace, issue_key, at) -> dict:
    """Record that this session's relay writes its declarations into this store."""
    return _write(
        db_path,
        ("SELECT dispatch_request_id, marker_root, workspace, issue_key, capability FROM"
         " reporting_sessions WHERE assignment_id = ? AND session_id = ?",
         (assignment, session_id)),
        ("INSERT INTO reporting_sessions (assignment_id, session_id, dispatch_request_id,"
         " marker_root, workspace, issue_key, capability, recorded_at)"
         " VALUES (?,?,?,?,?,?,?,?)",
         (assignment, session_id, dispatch_request_id, str(marker_root), str(workspace),
          issue_key, CAPABILITY, at)),
        lambda row: (row["dispatch_request_id"], row["marker_root"], row["workspace"],
                     row["issue_key"], row["capability"]) == (
                         dispatch_request_id, str(marker_root), str(workspace), issue_key,
                         CAPABILITY),
    )
