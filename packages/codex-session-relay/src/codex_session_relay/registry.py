"""Relationships, generations and anchors.

Identity here is the pair of actual task ids. There is no name field on the routing path,
so "the most recent session" is not expressible, let alone selectable.

An anchor binds a generation to the exact turn a dispatch receipt reported. It is never
inferred from whichever turn appears next, because an unrelated user turn arriving after
registration is not the execution anyone authorized.
"""

import json

from .errors import RefusalReason, RegistrationError
from .identity import relationship_id
from .models import Endpoint

ACTIVE = "active"
STATUSES = ("active", "paused", "cancelled", "archived")
DEACTIVATIONS = ("paused", "cancelled", "archived")
ANCHOR_BOUND = "bound"
ANCHOR_PENDING = "anchor_pending"
REASONS = ("initial_assignment", "needs_changes_revision")


def validated_turn_id(value):
    """An anchor turn id is a non-empty, non-blank string or nothing at all.

    Every path that can bind an anchor goes through here, so inline creation during
    registration cannot accept something bind_anchor would refuse.
    """
    if value is None:
        return None
    if not isinstance(value, str) or not value.strip():
        raise RegistrationError(
            RefusalReason.UNBOUND_GENERATION,
            f"an anchor needs an exact dispatch turn id, not {value!r}",
        )
    return value


class Registry:
    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    # ---------------------------------------------------------------- reading

    def get(self, rid: str) -> dict:
        row = self.store.one("SELECT * FROM relationships WHERE relationship_id = ?", (rid,))
        if row is None:
            raise RegistrationError(
                RefusalReason.UNREGISTERED_RELATIONSHIP, f"no relationship {rid!r}"
            )
        return self._row_to_record(row)

    def require_active(self, rid: str) -> dict:
        record = self.get(rid)
        if record["status"] != ACTIVE:
            raise RegistrationError(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                f"relationship {rid!r} is {record['status']!r} and is never auto-resumed",
            )
        return record

    def generations(self, rid: str) -> list:
        rows = self.store.all(
            "SELECT * FROM generations WHERE relationship_id = ? ORDER BY execution_generation",
            (rid,),
        )
        return [self._generation_record(row) for row in rows]

    def generation(self, rid: str, number: int) -> dict | None:
        self.get(rid)  # the generation invariant is enforced on every public read
        row = self.store.one(
            "SELECT * FROM generations WHERE relationship_id = ? AND execution_generation = ?",
            (rid, number),
        )
        return self._generation_record(row) if row else None

    # ---------------------------------------------------------------- writing

    def register(
        self,
        *,
        parent: Endpoint,
        child: Endpoint,
        issue_key: str,
        artifact_roots,
        allowed_recipients,
        dispatch_request_id: str,
        scope_ref: str | None = None,
        dispatch_turn_id: str | None = None,
        supersedes: str | None = None,
    ) -> dict:
        """Deterministic and idempotent.

        Re-registering the same pair with the same scope returns the existing record and
        opens no new generation, so an uncertain response can simply be repeated. Re-using
        the identity with a DIFFERENT scope is a conflict, not a silent overwrite.
        """
        roots = [str(r) for r in artifact_roots]
        recipients = [str(r) for r in allowed_recipients]
        if not roots or not recipients:
            raise RegistrationError(
                RefusalReason.SCOPE_ESCAPE,
                "a relationship needs at least one artifact root and one allowed recipient",
            )
        rid = relationship_id(parent.task_id, child.task_id, issue_key)
        dispatch_turn_id = validated_turn_id(dispatch_turn_id)
        existing = self.store.one("SELECT * FROM relationships WHERE relationship_id = ?", (rid,))
        if existing is not None:
            record = self._row_to_record(existing)
            same = (
                record["authorizedScope"]["artifactRoots"] == roots
                and record["authorizedScope"]["allowedRecipients"] == recipients
                and existing["parent_host_id"] == parent.host_id
                and existing["child_host_id"] == child.host_id
            )
            if not same:
                raise RegistrationError(
                    RefusalReason.RELATIONSHIP_CONFLICT,
                    f"{rid!r} already exists with a different scope or hosts",
                )
            return record
        now = self.clock.iso()
        with self.store.transaction() as db:
            # One issue, one responsible child, decided in the SAME transaction as the insert.
            # Checked beforehand, two connections could both see no rival and then insert
            # different children; BEGIN IMMEDIATE serialises writers, so the second one sees
            # the first one's row here.
            #
            # A PAUSED assignment still owns its child. A pause is a temporary state of an
            # existing assignment, never permission to open a second one, so only an archived,
            # cancelled or superseded assignment releases the issue.
            rival = db.execute(
                "SELECT relationship_id, child_task_id, status FROM relationships"
                "  WHERE issue_key = ? AND status IN ('active','paused')"
                "    AND superseded_by IS NULL AND child_task_id != ?",
                (issue_key, child.task_id),
            ).fetchone()
            if rival is not None and rival["relationship_id"] != supersedes:
                raise RegistrationError(
                    RefusalReason.DUPLICATE_ASSIGNMENT,
                    f"issue {issue_key!r} is already assigned to child "
                    f"{rival['child_task_id']!r} under {rival['relationship_id']!r} "
                    f"({rival['status']}); reuse that assignment, or pass supersedes to "
                    "replace it deliberately",
                )
            db.execute(
                "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id,"
                " parent_host_id, parent_cwd, parent_cxc_session, child_task_id, child_host_id,"
                " child_cwd, child_cxc_session, execution_generation, artifact_roots,"
                " allowed_recipients, scope_ref, supersedes, superseded_by, created_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
                (
                    rid, issue_key, ACTIVE,
                    parent.task_id, parent.host_id, parent.cwd, parent.cxc_session,
                    child.task_id, child.host_id, child.cwd, child.cxc_session,
                    1, json.dumps(roots), json.dumps(recipients), scope_ref, supersedes,
                    now, now,
                ),
            )
            db.execute(
                "INSERT INTO generations (relationship_id, execution_generation,"
                " dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at,"
                " bound_at) VALUES (?,?,?,?,?,?,?,?)",
                (
                    rid, 1, dispatch_request_id,
                    ANCHOR_BOUND if dispatch_turn_id else ANCHOR_PENDING,
                    dispatch_turn_id, "initial_assignment", now,
                    now if dispatch_turn_id else None,
                ),
            )
            if supersedes:
                db.execute(
                    "UPDATE relationships SET superseded_by = ?, status = 'archived',"
                    " updated_at = ? WHERE relationship_id = ?",
                    (rid, now, supersedes),
                )
            self.store.journal("relationship_registered", rid, {"issueKey": issue_key}, at=now)
        return self.get(rid)

    def open_generation(
        self, rid: str, *, dispatch_request_id: str, reason: str, dispatch_turn_id: str | None = None
    ) -> dict:
        """A replay of the same dispatch request id returns its generation, never a new one."""
        if reason not in REASONS:
            raise RegistrationError(RefusalReason.UNKNOWN_GENERATION, f"bad reason {reason!r}")
        dispatch_turn_id = validated_turn_id(dispatch_turn_id)
        record = self.require_active(rid)
        replay = self.store.one(
            "SELECT * FROM generations WHERE relationship_id = ? AND dispatch_request_id = ?",
            (rid, dispatch_request_id),
        )
        if replay is not None:
            return self._generation_record(replay)
        with self.store.transaction() as db:
            number = self.open_generation_in(
                db, rid, dispatch_request_id=dispatch_request_id, reason=reason,
                dispatch_turn_id=dispatch_turn_id,
            )
        return self.generation(rid, number)

    def open_generation_in(self, db, rid, *, dispatch_request_id, reason, dispatch_turn_id=None):
        """Open a generation inside a caller's transaction.

        Exists so that opening a generation and whatever the caller does because of it can be
        one rollback-safe operation. Advancing the execution and then failing to record why
        would leave a task working on a correction nobody can find.
        """
        if reason not in REASONS:
            raise RegistrationError(RefusalReason.UNKNOWN_GENERATION, f"bad reason {reason!r}")
        dispatch_turn_id = validated_turn_id(dispatch_turn_id)
        replay = db.execute(
            "SELECT execution_generation FROM generations WHERE relationship_id = ?"
            " AND dispatch_request_id = ?",
            (rid, dispatch_request_id),
        ).fetchone()
        if replay is not None:
            return replay["execution_generation"]
        current = db.execute(
            "SELECT execution_generation, status, superseded_by FROM relationships"
            " WHERE relationship_id = ?",
            (rid,),
        ).fetchone()
        if current is None:
            raise RegistrationError(
                RefusalReason.UNREGISTERED_RELATIONSHIP, f"no relationship {rid!r}"
            )
        if current["status"] != ACTIVE or current["superseded_by"]:
            raise RegistrationError(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                f"relationship {rid!r} is not active",
            )
        number = current["execution_generation"] + 1
        now = self.clock.iso()
        db.execute(
            "INSERT INTO generations (relationship_id, execution_generation,"
            " dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at,"
            " bound_at) VALUES (?,?,?,?,?,?,?,?)",
            (
                rid, number, dispatch_request_id,
                ANCHOR_BOUND if dispatch_turn_id else ANCHOR_PENDING,
                dispatch_turn_id, reason, now, now if dispatch_turn_id else None,
            ),
        )
        db.execute(
            "UPDATE relationships SET execution_generation = ?, updated_at = ?"
            " WHERE relationship_id = ?",
            (number, now, rid),
        )
        self.store.journal("generation_opened", rid, {"generation": number, "reason": reason}, at=now)
        return number

    def bind_anchor(self, rid: str, number: int, *, dispatch_turn_id: str, source: str) -> dict:
        """Bind only from a dispatch receipt, and only to the exact turn it reported."""
        if source != "dispatch_receipt":
            raise RegistrationError(
                RefusalReason.UNBOUND_GENERATION,
                f"an anchor binds only from a dispatch receipt, not from {source!r}",
            )
        if validated_turn_id(dispatch_turn_id) is None:
            raise RegistrationError(
                RefusalReason.UNBOUND_GENERATION, "an anchor needs an exact dispatch turn id"
            )
        current = self.generation(rid, number)
        if current is None:
            raise RegistrationError(
                RefusalReason.UNKNOWN_GENERATION, f"{rid!r} has no generation {number}"
            )
        if current["anchorState"] == ANCHOR_BOUND:
            if current["dispatchTurnId"] == dispatch_turn_id:
                return current
            raise RegistrationError(
                RefusalReason.ANCHOR_ALREADY_BOUND,
                f"generation {number} is already bound to {current['dispatchTurnId']!r}",
            )
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "UPDATE generations SET anchor_state = ?, dispatch_turn_id = ?, bound_at = ?"
                " WHERE relationship_id = ? AND execution_generation = ?",
                (ANCHOR_BOUND, dispatch_turn_id, now, rid, number),
            )
            self.store.journal("anchor_bound", rid, {"generation": number}, at=now)
        return self.generation(rid, number)

    def set_status(self, rid: str, status: str, *, actor: str) -> dict:
        """Deactivation only.

        Reactivating is not a status flip: it has to restate the generation and the scope it
        is re-authorizing, which is what resume() is for. Allowing "active" here would have
        made every one of those checks optional.
        """
        if status not in DEACTIVATIONS:
            raise RegistrationError(RefusalReason.RELATIONSHIP_NOT_ACTIVE, f"bad status {status!r}")
        return self._write_status(rid, status, actor=actor)

    def _write_status(self, rid: str, status: str, *, actor: str) -> dict:
        if status not in STATUSES:
            raise RegistrationError(RefusalReason.RELATIONSHIP_NOT_ACTIVE, f"bad status {status!r}")
        self.get(rid)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?",
                (status, now, rid),
            )
            self.store.journal("status_changed", rid, {"status": status, "actor": actor}, at=now)
        return self.get(rid)

    def resume(
        self,
        rid: str,
        *,
        expect_generation: int,
        expect_artifact_roots,
        expect_allowed_recipients,
        actor: str,
    ) -> dict:
        """Resuming requires restating the generation and scope being re-authorized.

        This is what stops a pause, an archive or a parent replacement from being undone by
        a process that simply flips a status back. The caller has to say what it believes it
        is resuming, and it has to be right.

        The restatement and the reactivation are ONE operation. They used to be two: the
        comparison ran against a record read before the write, and the write then set active
        unconditionally, so every condition here was a preflight. Two things followed. A caller
        holding generation-1 authorization could resume, watch another caller open generation 2
        and pause it, and still write active, returning generation 2 ACTIVE over a newer user's
        pause. And a cancelled relationship whose issue had since been reassigned could resume
        straight back alongside its replacement, leaving one issue with two registered owners.

        So the whole decision now happens inside the write transaction, including whether this
        issue is still free. A refusal raises before anything is written and rolls back, which is
        why a refused resume leaves BOTH relationships exactly as they were rather than quietly
        rearranging the other one.
        """
        roots = [str(r) for r in expect_artifact_roots]
        recipients = [str(r) for r in expect_allowed_recipients]
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM relationships WHERE relationship_id = ?", (rid,)
            ).fetchone()
            if row is None:
                raise RegistrationError(
                    RefusalReason.UNREGISTERED_RELATIONSHIP, f"no relationship {rid!r}"
                )
            mismatches = []
            if row["execution_generation"] != expect_generation:
                mismatches.append(
                    f"generation is {row['execution_generation']}, not {expect_generation}"
                )
            if json.loads(row["artifact_roots"]) != roots:
                mismatches.append("artifact roots differ from the restated scope")
            if json.loads(row["allowed_recipients"]) != recipients:
                mismatches.append("allowed recipients differ from the restated scope")
            if row["superseded_by"]:
                mismatches.append(f"superseded by {row['superseded_by']}")
            if mismatches:
                raise RegistrationError(
                    RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                    "resume refused: " + "; ".join(mismatches),
                )
            # Reactivation is only allowed into an issue that is still free for this
            # relationship. Cancelling or archiving RELEASES an issue, which is what lets a
            # replacement be registered, so coming back afterwards is not a status flip: it is a
            # second owner. register() enforces this on the insert path; resume is the other way
            # in, and it used to be unguarded.
            owner = db.execute(
                "SELECT relationship_id, child_task_id, status FROM relationships"
                "  WHERE issue_key = ? AND relationship_id != ?"
                "    AND status IN ('active','paused') AND superseded_by IS NULL",
                (row["issue_key"], rid),
            ).fetchone()
            if owner is not None:
                raise RegistrationError(
                    RefusalReason.DUPLICATE_ASSIGNMENT,
                    f"issue {row['issue_key']!r} is now assigned to child "
                    f"{owner['child_task_id']!r} under {owner['relationship_id']!r} "
                    f"({owner['status']}); resuming {rid!r} would leave the issue with two "
                    "owners. Replace that assignment deliberately instead",
                )
            db.execute(
                "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?",
                (ACTIVE, now, rid),
            )
            self.store.journal(
                "status_changed", rid, {"status": ACTIVE, "actor": actor}, at=now
            )
        return self.get(rid)

    def supersede(self, old_rid: str, *, new_relationship_id: str) -> None:
        self.get(old_rid)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "UPDATE relationships SET superseded_by = ?, status = 'archived', updated_at = ?"
                " WHERE relationship_id = ?",
                (new_relationship_id, now, old_rid),
            )
            self.store.journal("superseded", old_rid, {"by": new_relationship_id}, at=now)

    # ---------------------------------------------------------------- records

    def _row_to_record(self, row) -> dict:
        record = {
            "relationshipId": row["relationship_id"],
            "parent": {
                "taskId": row["parent_task_id"],
                "hostId": row["parent_host_id"],
                "cwd": row["parent_cwd"],
            },
            "child": {
                "taskId": row["child_task_id"],
                "hostId": row["child_host_id"],
                "cwd": row["child_cwd"],
            },
            "issueKey": row["issue_key"],
            "status": row["status"],
            "createdAt": row["created_at"],
            "executionGeneration": row["execution_generation"],
            "generations": [
                {k: v for k, v in g.items() if k != "relationshipId"}
                for g in self.generations(row["relationship_id"])
            ],
            "authorizedScope": {
                "artifactRoots": json.loads(row["artifact_roots"]),
                "allowedRecipients": json.loads(row["allowed_recipients"]),
                "scopeRef": row["scope_ref"],
            },
        }
        if row["supersedes"]:
            record["supersedes"] = row["supersedes"]
        # Harness bindings are carried for audit and never written back to any harness.
        record["_bindings"] = {
            "parentCxcSession": row["parent_cxc_session"],
            "childCxcSession": row["child_cxc_session"],
            "supersededBy": row["superseded_by"],
        }
        record["supersededBy"] = row["superseded_by"]
        self._assert_generation_invariant(record)
        return record

    @staticmethod
    def _generation_record(row) -> dict:
        return {
            "relationshipId": row["relationship_id"],
            "executionGeneration": row["execution_generation"],
            "dispatchRequestId": row["dispatch_request_id"],
            "anchorState": row["anchor_state"],
            "dispatchTurnId": row["dispatch_turn_id"],
            "openedAt": row["opened_at"],
            "boundAt": row["bound_at"],
            "reason": row["reason"],
        }

    @staticmethod
    def _assert_generation_invariant(record: dict) -> None:
        numbers = {g["executionGeneration"] for g in record["generations"]}
        if record["executionGeneration"] not in numbers:
            raise RegistrationError(
                RefusalReason.UNKNOWN_GENERATION,
                f"{record['relationshipId']!r} points at generation "
                f"{record['executionGeneration']} which is not retained",
            )


def contract_record(record: dict) -> dict:
    """The relationship as the frozen schema defines it, without internal fields."""
    clean = {k: v for k, v in record.items() if not k.startswith("_") and k != "supersededBy"}
    if clean.get("supersedes") is None:
        clean.pop("supersedes", None)
    scope = dict(clean["authorizedScope"])
    if scope.get("scopeRef") is None:
        scope.pop("scopeRef", None)
    clean["authorizedScope"] = scope
    clean["generations"] = [
        {k: v for k, v in g.items() if k != "relationshipId"} for g in clean["generations"]
    ]
    return clean


def project_key(record: dict) -> str:
    """Which project an assignment belongs to, for grouping a shared service's work.

    Grouping information, never authorization: the cross-delivery refusal is decided from the
    relationship's own endpoints, not from this. A parent with no recorded cwd falls back to
    its host, so every assignment lands under some key.
    """
    parent = record.get("parent") or {}
    cwd = parent.get("cwd")
    if cwd:
        import posixpath

        return posixpath.normpath(cwd)
    return f"host:{parent.get('hostId')}"


def record_settings(store, clock, task_id: str, settings: dict, *, source: str) -> dict:
    """Record the execution settings a task was actually created with.

    This is the interface JUN-92 populates from the creation result Run already receives. It
    reuses creation evidence and asks nothing new of the host; it is not a first-turn handshake.
    Recording is validated up front so an unusable record is refused at registration rather than
    discovered at send time.
    """
    from .settings import TaskSettings

    candidate = TaskSettings(settings)
    candidate.require_usable()
    payload = json.dumps(settings, sort_keys=True)
    with store.transaction() as db:
        db.execute(
            "INSERT INTO authorized_settings (task_id, settings, source, recorded_at)"
            " VALUES (?,?,?,?)"
            " ON CONFLICT(task_id) DO UPDATE SET settings = excluded.settings,"
            "   source = excluded.source, recorded_at = excluded.recorded_at",
            (task_id, payload, source, clock.iso()),
        )
        store.journal("settings_recorded", task_id, {"source": source}, at=clock.iso())
    return {"taskId": task_id, "source": source, "settings": settings}


def load_settings(store, task_id: str):
    """The recorded settings, or None. None is an absence, and absence withholds."""
    from .settings import TaskSettings

    row = store.one("SELECT settings FROM authorized_settings WHERE task_id = ?", (task_id,))
    return TaskSettings(json.loads(row["settings"])) if row else None
