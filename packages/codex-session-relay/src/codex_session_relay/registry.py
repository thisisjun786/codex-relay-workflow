"""Relationships, generations and anchors.

Identity here is the pair of actual task ids. There is no name field on the routing path,
so "the most recent session" is not expressible, let alone selectable.

An anchor binds a generation to the exact turn a dispatch receipt reported. It is never
inferred from whichever turn appears next, because an unrelated user turn arriving after
registration is not the execution anyone authorized.
"""

import json

from .errors import RefusalReason, RegistrationError, RelayError
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
        self._linkage = None

    @property
    def linkage(self):
        """The three-level linkage, built on demand.

        Lazy because linkage imports assignment, which imports criteria, and registry is
        constructed by almost everything. Nothing here changes unless a caller supplies a
        project.
        """
        if self._linkage is None:
            from .linkage import Linkage

            self._linkage = Linkage(self.store, self.clock)
        return self._linkage

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
        project_key: str | None = None,
    ) -> dict:
        """Deterministic and idempotent.

        Re-registering the same pair with the same scope returns the existing record and
        opens no new generation, so an uncertain response can simply be repeated. Re-using
        the identity with a DIFFERENT scope is a conflict, not a silent overwrite.

        project_key is optional and additive. Supplied, the transaction that inserts the
        relationship also writes its whole lower level through linkage.attach_in, so an
        assignment and the project it belongs to are one atomic fact rather than two that can
        disagree after a crash. Omitted, every byte of this method's behaviour is what it was.
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
        if project_key is None and supersedes:
            # A replacement takes over the SAME issue, so it belongs to the same project. Left
            # to the caller, superseding a scoped assignment without restating the project
            # archived the outgoing child binding and the project-to-issue edge and attached
            # no successor, so the issue silently lost its level.
            inherited = self.store.one(
                "SELECT s.project_key FROM relationship_scope s"
                "  JOIN relationships r ON r.relationship_id = s.relationship_id"
                " WHERE s.relationship_id = ? AND r.issue_key = ?",
                (supersedes, issue_key))
            project_key = inherited["project_key"] if inherited else None
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
            if project_key is None:
                return record
            # An existing relationship never reaches the inserts below: they are
            # unconditional, so falling through would collide on its own primary key. It gets
            # its own path, and attach_in decides for itself which of its three facts are
            # missing - so a fully attached relationship writes nothing and a partially
            # attached one is completed rather than reported as already done.
            recorded = self.store.one(
                "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", (rid,)
            )
            if recorded is not None and recorded["project_key"] != project_key:
                raise RegistrationError(
                    RefusalReason.RELATIONSHIP_CONFLICT,
                    f"{rid!r} is already scoped to project "
                    f"{recorded['project_key']!r}, not {project_key!r}",
                )
            refusal = None
            with self.store.transaction() as db:
                # Re-read inside the transaction. The row above was read before BEGIN
                # IMMEDIATE, so another writer could have archived or superseded it in
                # between, and attaching from a stale row would bind a child to an
                # assignment that no longer owns its issue.
                fresh = db.execute(
                    "SELECT * FROM relationships WHERE relationship_id = ?", (rid,)
                ).fetchone()
                if fresh is None:
                    raise RegistrationError(
                        RefusalReason.UNREGISTERED_RELATIONSHIP, f"no relationship {rid!r}")
                refusal = self.linkage.attach_in(db, fresh, project_key)
                if refusal is not None:
                    self.linkage.record_conflict_in(db, refusal, at=self.clock.iso())
            if refusal is not None:
                raise refusal.error()
            return self.get(rid)
        now = self.clock.iso()
        self._raced = None
        if project_key is not None:
            # Decide the lower level BEFORE anything is inserted, in a transaction that writes
            # only the contest if there is one. Inserting the relationship first and then
            # discovering its project is foreign would roll the relationship back AND lose the
            # conflict row with it, which is the one thing a refusal must not do.
            # A replacement takes over the issue scope from the assignment it supersedes, so
            # that outgoing child is not a rival. Named explicitly rather than inferred, so
            # anyone ELSE holding the scope is still a refusal.
            pending = None
            with self.store.transaction() as db:
                # Decided under the same lock as the write it authorizes, and only when the
                # predecessor is still live and still holds the binding. Read beforehand, it
                # could name a child that had already released the issue and reclaimed it
                # directly, which excuses the wrong rival.
                outgoing = self.linkage.replaceable_child_in(db, supersedes)
                candidate = {
                    "relationship_id": rid, "issue_key": issue_key, "status": ACTIVE,
                    "superseded_by": None, "parent_task_id": parent.task_id,
                    "child_task_id": child.task_id, "child_host_id": child.host_id,
                    "child_cwd": child.cwd, "child_cxc_session": child.cxc_session,
                    # Carried so the pre-check can tell a genuine successor from an unrelated
                    # registration; attach_refusal relaxes the parent rule only for the first.
                    "supersedes": supersedes,
                }
                _plan, pending = self.linkage.attach_refusal(
                    db, candidate, project_key, replacing=outgoing)
                if pending is not None:
                    self.linkage.record_conflict_in(db, pending, at=now)
            if pending is not None:
                raise pending.error()
        try:
            return self._register_in_transaction(
                rid, parent, child, issue_key, roots, recipients, scope_ref,
                dispatch_request_id, dispatch_turn_id, supersedes, project_key, now)
        except RelayError:
            raced = getattr(self, "_raced", None)
            if raced is not None:
                self._raced = None
                with self.store.transaction() as db:
                    self.linkage.record_conflict_in(db, raced, at=now)
            raise

    def _register_in_transaction(self, rid, parent, child, issue_key, roots, recipients,
                                 scope_ref, dispatch_request_id, dispatch_turn_id,
                                 supersedes, project_key, now):
        with self.store.transaction() as db:
            # Re-decided inside THIS transaction rather than carried in: the pre-check ran in
            # its own, and between them the predecessor can have been cancelled and its issue
            # claimed directly.
            outgoing = self.linkage.replaceable_child_in(db, supersedes)
            # One issue, one responsible child, decided in the SAME transaction as the insert.
            # Checked beforehand, two connections could both see no rival and then insert
            # different children; BEGIN IMMEDIATE serialises writers, so the second one sees
            # the first one's row here.
            #
            # A PAUSED assignment still owns its child. A pause is a temporary state of an
            # existing assignment, never permission to open a second one, so only an archived,
            # cancelled or superseded assignment releases the issue.
            #
            # Keyed on the relationship, not on the child. Excluding rows that share this
            # child was meant to let a caller restate its own assignment, but an identical
            # restatement derives the SAME id and returns long before this point - so the only
            # thing the child exclusion actually admitted was the same child being registered
            # for the same issue under a SECOND parent. That is two live assignments, two
            # parents authorized to deliver, and a project whose owner matches neither.
            rival = db.execute(
                "SELECT relationship_id, child_task_id, parent_task_id, status"
                "  FROM relationships"
                "  WHERE issue_key = ? AND status IN ('active','paused')"
                "    AND superseded_by IS NULL AND relationship_id != ?",
                (issue_key, rid),
            ).fetchone()
            if rival is not None and rival["relationship_id"] != supersedes:
                raise RegistrationError(
                    RefusalReason.DUPLICATE_ASSIGNMENT,
                    f"issue {issue_key!r} is already assigned to child "
                    f"{rival['child_task_id']!r} under {rival['relationship_id']!r} "
                    f"({rival['status']}, parent {rival['parent_task_id']!r}); reuse that "
                    "assignment, or pass supersedes to replace it deliberately",
                )
            if supersedes:
                # BEFORE the successor is inserted. The lifecycle guard asks whether this
                # relationship is still the live assignment for its issue, and once the
                # replacement exists the answer for the predecessor is no - so archiving it
                # afterwards would skip releasing the issue scope and the successor would
                # collide with a binding nobody let go of.
                outgoing_before = db.execute(
                    "SELECT status FROM relationships WHERE relationship_id = ?",
                    (supersedes,),
                ).fetchone()
                db.execute(
                    "UPDATE relationships SET superseded_by = ?, status = 'archived',"
                    " updated_at = ? WHERE relationship_id = ?",
                    (rid, now, supersedes),
                )
                self.linkage.apply_relationship_status_in(
                    db, supersedes, "archived",
                    previous_status=(outgoing_before["status"]
                                     if outgoing_before is not None else None))
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
            self.store.journal("relationship_registered", rid, {"issueKey": issue_key}, at=now)
            if project_key is not None:
                fresh = db.execute(
                    "SELECT * FROM relationships WHERE relationship_id = ?", (rid,)
                ).fetchone()
                # Re-decided inside the transaction that did the inserts, so a racing writer
                # cannot slip between the pre-check and the write. A refusal here raises and
                # rolls the whole registration back, which is correct: the contest was already
                # recorded by the pre-check below, in a transaction that wrote nothing else.
                refusal = self.linkage.attach_in(db, fresh, project_key, at=now,
                                                 replacing=outgoing if supersedes else None)
                if refusal is not None:
                    # This one arose only in the window between the pre-check and this
                    # transaction, so its contest has not been recorded. Raising here rolls
                    # the whole registration back, which is right, and takes any conflict row
                    # written in this transaction with it - so it is re-recorded afterwards,
                    # in its own transaction, rather than lost with the rollback.
                    self._raced = refusal
                    raise refusal.error()
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
        # Anything still outstanding for an earlier generation is history from this moment on.
        # It is ANNOTATED, never rewritten: a send whose response was lost still has to be
        # reconciled, and a terminal superseded aggregate cannot be. Without this a delivery
        # that was sending or held_uncertain when the generation advanced reconciled to
        # dispatched with no note at all, and status presented it as an ordinary current one.
        db.execute(
            "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied)"
            " SELECT d.event_id, 'stale_generation', ?, 0 FROM deliveries d"
            "  JOIN events e ON e.event_id = d.event_id"
            " WHERE d.relationship_id = ? AND e.execution_generation < ?"
            # dispatched belongs here too: its acknowledgement will be refused as
            # stale_generation, so leaving it unannotated meant status showed awaiting_ack
            # for an obligation that can no longer be met. Annotating does not rewrite the
            # delivery, so the history of what was actually sent is untouched.
            # deferred_busy and withheld_pre_send belong here for a stronger reason: once
            # either has reached its attempt cap, hold_reason is set and attempt() returns
            # before the pre-send supersession check, so generation advance is the ONLY
            # occasion on which they can ever be annotated. Without them a capped delivery
            # reports a current-looking cap forever.
            # inbox_only belongs with them: it is terminal, attempt() cannot revisit it, and
            # its acknowledgement is refused as stale - so without this it reports
            # channel_closed as though it were still current.
            # queued belongs here for a reason of the same shape: _claim does suppress a stale
            # queued row, but attempt() can return BEFORE _claim - rate limiting, a busy
            # recipient, an unavailable host - and a recipient that is never free means _claim
            # is never reached at all. Generation advance already knows the row is stale, so
            # leaving it unannotated let it keep retrying while reporting as current.
            "   AND d.state IN ('queued','sending','held_uncertain','dispatched',"
            "                  'deferred_busy','withheld_pre_send','inbox_only')"
            " ON CONFLICT(event_id) DO NOTHING",
            (now, rid, number),
        )
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

    def bind_anchor_in(self, db, rid: str, number: int, *, dispatch_turn_id, source) -> str:
        """The same binding, against a transaction the caller already owns.

        This exists for the writer that PROMOTES a revision to dispatched. Binding in a
        separate transaction afterwards leaves an interval in which the dispatch evidence is
        durable and the generation is still anchor_pending, and a child emitting from another
        process inside that interval has a perfectly valid completion refused as
        unbound_generation. Nothing about the binding needs a transport call - the turn id is
        already in hand before the transaction opens - so there is no reason for it to travel
        separately.

        It returns an outcome instead of raising, because raising here would roll back the
        promotion it travelled with over a disagreement about a DIFFERENT fact:

          bound       the generation was pending and now names this turn
          unchanged   it already names this turn
          conflict    it names another turn, and is left exactly as it is
          ineligible  nothing to bind from, or no such generation

        A conflict is journalled HERE rather than left for the repair pass. That pass selects
        anchor_pending generations only, so a generation that is already bound is never
        looked at again and the disagreement would simply disappear.
        """
        if source != "dispatch_receipt" or validated_turn_id(dispatch_turn_id) is None:
            return "ineligible"
        # Read INSIDE the caller's transaction, so the decision and the write cannot be
        # separated by another writer, and so the outcome comes from the row rather than from
        # an update count.
        current = db.execute(
            "SELECT anchor_state, dispatch_turn_id FROM generations"
            " WHERE relationship_id = ? AND execution_generation = ?",
            (rid, number),
        ).fetchone()
        if current is None:
            return "ineligible"
        now = self.clock.iso()
        if current["anchor_state"] == ANCHOR_BOUND:
            if current["dispatch_turn_id"] == dispatch_turn_id:
                return "unchanged"
            self.store.journal(
                "anchor_conflict", rid,
                {"generation": number, "boundTo": current["dispatch_turn_id"],
                 "offered": dispatch_turn_id},
                at=now,
            )
            return "conflict"
        db.execute(
            "UPDATE generations SET anchor_state = ?, dispatch_turn_id = ?, bound_at = ?"
            " WHERE relationship_id = ? AND execution_generation = ?",
            (ANCHOR_BOUND, dispatch_turn_id, now, rid, number),
        )
        self.store.journal("anchor_bound", rid, {"generation": number}, at=now)
        return "bound"

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
            # Read BEFORE the write. The lower level has to know whether this relationship was
            # still live, and after the UPDATE below that fact is gone.
            before = db.execute(
                "SELECT status FROM relationships WHERE relationship_id = ?", (rid,)
            ).fetchone()
            db.execute(
                "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?",
                (status, now, rid),
            )
            # The lower level moves with the assignment, in the same transaction. A no-op for
            # a relationship with no recorded project, which is every relationship registered
            # without one.
            self.linkage.apply_relationship_status_in(
                db, rid, status,
                previous_status=before["status"] if before is not None else None)
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
            self.linkage.apply_relationship_status_in(
                db, rid, ACTIVE, previous_status=row["status"])
            self.store.journal(
                "status_changed", rid, {"status": ACTIVE, "actor": actor}, at=now
            )
        return self.get(rid)

    def supersede(self, old_rid: str, *, new_relationship_id: str) -> None:
        self.get(old_rid)
        now = self.clock.iso()
        with self.store.transaction() as db:
            # Inside the transaction, immediately before the write. Read outside it, another
            # writer could cancel this relationship - releasing its issue - and the same child
            # could reclaim that issue directly, while this call still believed it was moving a
            # live assignment and archived the new claim on its way past.
            before = db.execute(
                "SELECT status FROM relationships WHERE relationship_id = ?", (old_rid,)
            ).fetchone()
            db.execute(
                "UPDATE relationships SET superseded_by = ?, status = 'archived', updated_at = ?"
                " WHERE relationship_id = ?",
                (new_relationship_id, now, old_rid),
            )
            self.linkage.apply_relationship_status_in(
                db, old_rid, "archived",
                previous_status=before["status"] if before is not None else None)
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
