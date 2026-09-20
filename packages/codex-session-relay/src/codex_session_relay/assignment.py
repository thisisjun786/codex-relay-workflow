"""One issue, one responsible child, and where that assignment actually stands.

Derived rather than stored, except for the one fact the relay cannot observe. The relay sees
registration, receipts, claims, verdicts and generations; it cannot see a merge, so merged is the
single explicit mark. Deriving the rest means the state cannot drift away from the records it is
supposed to summarise.

A mark is bound to the exact revision it is about. Keyed on the relationship alone, one old merge
would have labelled every later generation merged, which is the opposite of what a coordinator
needs from this view. A mark that no longer matches the current head is history, and history is
returned as history.
"""

import json

from .currency import AMBIGUOUS, head_revision

REQUESTED = "requested"
RECEIVED = "received"
VERIFYING = "verifying"
NEEDS_CHANGES = "needs_changes"
CORRECTED = "corrected"
VERIFIED = "verified"
MERGED = "merged"
PAUSED = "paused"
AMBIGUOUS_STATE = "ambiguous"
REREVIEW_NEEDED = "re_review_needed"
CLOSED = "closed"
ABANDONED = "abandoned"

MARKS = ("merged",)

# linkage.PROJECT, spelled here for the same reason delivery spells it: linkage reaches this
# module from inside its own functions, so a module-level import would close a ring.
PROJECT_SCOPE_KIND = "project"

PROJECT_READING_LIMITS = (
    "A reading of what this project's children report, not a completion verdict. The strongest "
    "state is complete_candidate: integration and verification are the parent's judgment and are "
    "not visible here. A child's own goal status is never consulted; each assignment's state is "
    "derived from receipts, verdicts and marks against the current head, generation and "
    "revision. unreadable, unregistered and ambiguous are three different answers and none of "
    "them means finished."
)

# What each state is waiting for. Derived from the same records, never stored, so it cannot go
# stale: this is the field that answers "what happens next" without a second lookup.
NEXT_ACTION = {
    REQUESTED: "child_emits",
    RECEIVED: "daemon_delivers",
    VERIFYING: "parent_verifies",
    NEEDS_CHANGES: "child_corrects",
    CORRECTED: "parent_verifies",
    VERIFIED: "coordinator_integrates",
    MERGED: "none",
    PAUSED: "owner_resumes",
    AMBIGUOUS_STATE: "child_declares_supersession",
    REREVIEW_NEEDED: "parent_verifies",
    CLOSED: "none",
    ABANDONED: "none",
}


class AssignmentView:
    def __init__(self, store, registry, clock, *, criteria=None, linkage=None):
        self.store = store
        self.registry = registry
        self.clock = clock
        if criteria is None:
            from .criteria import CriteriaService

            criteria = CriteriaService(store, clock)
        self.criteria = criteria

        # Optional, and read-only. linkage.outstanding() already derives its unfinished set
        # through this class's own state(), so the project-level reading below is the same
        # derivation lifted one level rather than a second opinion about it. Absent, the
        # project reading says it cannot answer instead of guessing.
        self.linkage = linkage

    def project_state(self, project_key: str) -> dict:
        """What an approved project scope's children actually say, as a reading not a verdict.

        A parent's completion is computed from its children's real state, so this returns the
        unfinished set and the basis for it and stops there. It never returns "complete": the
        strongest thing it says is complete_candidate, because integration and verification are
        the parent's judgment and this class cannot see them.

        The four answers linkage keeps apart are kept apart here, and none of them is completion:

        unreadable    the store did not answer. Not "nothing outstanding".
        unregistered  the project has no live attached assignment at all. Not "all done".
        ambiguous     more than one candidate owner. The reader will not choose.
        incomplete    at least one live assignment is unfinished, named row by row.

        Partial Done cannot close a parent, because complete_candidate requires the outstanding
        set to be EMPTY rather than small. A shared project cannot close one either: attached()
        counts every live assignment in the project, so work parked on another parent still
        counts. And a child's own goal being complete is not consulted at all - state() is
        derived from receipts, verdicts and marks against the current head, generation and
        revision, so a goal a child closed on its own is not evidence here.
        """
        if self.linkage is None:
            return {"state": "unreadable", "readable": False, "projectKey": project_key,
                    "attached": [], "outstanding": [],
                    "basis": "no linkage reader was supplied, so the project's assignments "
                             "could not be enumerated",
                    "limits": PROJECT_READING_LIMITS}
        try:
            attached = list(self.linkage.attached(project_key))
            outstanding = list(self.linkage.outstanding(project_key))
            # Inside the boundary, not after it. Owner enumeration reads the same store and is
            # required to CLASSIFY the answer, so a failure here has to produce the documented
            # unreadable shape rather than propagate a database error to a caller that asked a
            # question about completion.
            owners = self.linkage.owners(PROJECT_SCOPE_KIND, project_key)
        except Exception as error:  # noqa: BLE001 - an unreadable store is an answer, not a crash
            return {"state": "unreadable", "readable": False, "projectKey": project_key,
                    "attached": [], "outstanding": [],
                    "basis": f"the project's assignments could not be read: "
                             f"{type(error).__name__}: {error}",
                    "limits": PROJECT_READING_LIMITS}
        if len(owners) > 1:
            return {"state": "ambiguous", "readable": True, "projectKey": project_key,
                    "attached": attached, "outstanding": outstanding,
                    "competingOwners": sorted(o["taskId"] for o in owners),
                    "basis": "the project has more than one live owner, so which parent this "
                             "reading is about is not decided here",
                    "limits": PROJECT_READING_LIMITS}
        if not attached:
            return {"state": "unregistered", "readable": True, "projectKey": project_key,
                    "attached": [], "outstanding": [],
                    "basis": "no live assignment is attached to this project, which is not the "
                             "same as every assignment being finished",
                    "limits": PROJECT_READING_LIMITS}
        # Expanded here, AFTER the two classifications that owners() and attached() already
        # settle, and inside a boundary of its own. outstanding() returns ids; naming each one's
        # state reads the registry and store again, so a failure there still has to produce the
        # promised unreadable reading rather than raise at a caller asking about completion.
        # But it must not run any earlier: ambiguity is establishable from owners() alone, and
        # expanding first let an unrelated state-read failure replace a definite ambiguous
        # answer with unreadable - masking the more specific fact with the vaguer one.
        try:
            unfinished = [{"relationshipId": rid, "state": self.state(rid)["state"]}
                          for rid in outstanding]
        except Exception as error:  # noqa: BLE001 - same reason as the boundary above
            return {"state": "unreadable", "readable": False, "projectKey": project_key,
                    "attached": attached, "outstanding": outstanding,
                    "basis": f"the unfinished set could not be expanded: "
                             f"{type(error).__name__}: {error}",
                    "limits": PROJECT_READING_LIMITS}
        if unfinished:
            return {"state": "incomplete", "readable": True, "projectKey": project_key,
                    "attached": attached, "outstanding": outstanding,
                    "unfinished": unfinished,
                    "basis": f"{len(unfinished)} of {len(attached)} live assignments are "
                             "unfinished",
                    "limits": PROJECT_READING_LIMITS}
        return {"state": "complete_candidate", "readable": True, "projectKey": project_key,
                "attached": attached, "outstanding": [], "unfinished": [],
                "basis": f"all {len(attached)} live assignments in this project report a "
                         "finished state",
                "limits": PROJECT_READING_LIMITS}

    # -------------------------------------------------------------------- read

    def state(self, relationship_id: str) -> dict:
        relationship = self.registry.get(relationship_id)
        row = self.store.one(
            "SELECT * FROM relationships WHERE relationship_id = ?", (relationship_id,)
        )
        generation = relationship["executionGeneration"]
        head = head_revision(self.store.db, relationship_id, generation)
        verdict = self._verdict_for(head["eventId"]) if head["eventId"] else None
        current_digest = self._current_digest(relationship_id)
        criteria_current = self._criteria_current(verdict, current_digest)
        marks = self._marks(relationship_id)
        current_mark = (
            self._current_mark(marks, head, generation, verdict) if criteria_current else None
        )

        state = self._resolve(
            row, head, verdict, relationship_id, generation, current_mark, criteria_current
        )
        record = {
            "relationshipId": relationship_id,
            "issueKey": relationship["issueKey"],
            "parentTaskId": relationship["parent"]["taskId"],
            "childTaskId": relationship["child"]["taskId"],
            "relationshipStatus": relationship["status"],
            "state": state,
            "executionGeneration": generation,
            "head": {
                "eventId": head["eventId"],
                "revisionHash": head["revisionHash"],
                "evidence": head["evidence"],
                "competitors": head["competitors"],
                "detail": head["detail"],
            },
            "lastVerdict": verdict,
            "mark": current_mark,
            # Every mark that no longer describes the current head. Retained, and never state.
            "markHistory": [m for m in marks if m is not current_mark],
            "nextExpectedAction": NEXT_ACTION.get(state, "none"),
        }
        registered = self.criteria.get(relationship_id)
        record["criteria"] = {
            "mode": self.criteria.mode(relationship_id),
            "setDigest": current_digest,
            "registered": len(registered["criteria"]) if registered else 0,
            # The set the current verdict was actually decided against. When it differs from
            # the registered one, that verdict certifies wording nobody is judging by any more.
            "reviewedSetDigest": verdict["setDigest"] if verdict else None,
            "current": criteria_current,
        }
        record["projection"] = self._projection(
            relationship_id, generation, head, verdict, state
        )
        return record

    def _projection(self, relationship_id, generation, head, verdict, state) -> dict:
        """Five vocabularies, kept apart, each anchored to the event it was read under.

        They are not interchangeable and collapsing any two loses the distinction a caller
        needs. `staged` is an EVENT stage and never a delivery state; `acknowledged` IS a
        delivery state; and an acknowledgement settles on one axis while carrying its evidence
        on another, so a bridge receipt saying an attempt was accepted still says nothing about
        receipt, application or verification.

        Anchored, because an unanchored read pairs whatever each table happens to hold. After a
        needs_changes verdict the assignment has a completion in the previous generation and a
        queued correction in the current one; reporting the old acknowledgement beside the new
        delivery would describe a state that never existed. The two get separate anchors rather
        than one, because head_revision only ever names a ready_for_review event - so while the
        correction is the thing everyone is waiting for, it would otherwise be invisible here.
        """
        correction = self.store.one(
            "SELECT event_id FROM events"
            " WHERE relationship_id = ? AND execution_generation = ?"
            "   AND outcome = 'revision_request' AND suppressed_reason IS NULL"
            " ORDER BY event_id LIMIT 1",
            (relationship_id, generation),
        )
        return {
            "completion": self._anchored(head["eventId"], generation),
            "correction": self._anchored(
                correction["event_id"] if correction else None, generation
            ),
            # Referenced, never recomputed. state() derived both of these above and a second
            # derivation here would be a second source of truth for one fact.
            "verdict": verdict,
            "assignment": {"state": state},
        }

    def _anchored(self, event_id, generation) -> dict:
        record = {"eventId": event_id, "executionGeneration": generation,
                  "event": None, "delivery": None, "ack": None, "undeliveredReason": None}
        if event_id is None:
            # A null is an answer. A row borrowed from another generation is not.
            record["detail"] = "this generation has no such event"
            return record
        staged = self.store.one("SELECT stage FROM events WHERE event_id = ?", (event_id,))
        record["event"] = {"stage": staged["stage"]} if staged is not None else None
        delivery = self.store.one(
            "SELECT state, attempt_count, hold_reason FROM deliveries WHERE event_id = ?",
            (event_id,),
        )
        if delivery is not None:
            # The CURRENT attempt, the one deliveries.attempt_count names. An event can carry
            # several, and "the request id" with no rule is whichever row sorted first, which
            # after a retry is the wrong one.
            attempt = self.store.one(
                "SELECT request_id, attempt_no FROM attempts"
                " WHERE event_id = ? AND attempt_no = ?",
                (event_id, delivery["attempt_count"]),
            )
            record["delivery"] = {
                "state": delivery["state"],
                "requestId": attempt["request_id"] if attempt is not None else None,
                "attemptNo": attempt["attempt_no"] if attempt is not None else None,
            }
        settled = self.store.one("SELECT verified FROM acks WHERE event_id = ?", (event_id,))
        tier = self.store.one(
            "SELECT tier FROM ack_evidence WHERE event_id = ?", (event_id,)
        )
        record["ack"] = {
            "settlement": settled["verified"] if settled is not None else None,
            # A second axis, not a rewording of the first: settlement says whether the
            # acknowledgement closed, the tier says what it closed on, and no row at all is
            # unrecorded rather than unverified.
            "evidenceTier": tier["tier"] if tier is not None else "unrecorded",
        }
        record["undeliveredReason"] = self._undelivered_reason(event_id, delivery)
        return record

    def _undelivered_reason(self, event_id, delivery):
        """Copied verbatim from the row that recorded it, saying which row that was.

        Two tables can explain one event's delivery and they answer different questions, so the
        source travels with the value instead of being guessed from its shape. Most specific
        first: a hold is about THIS delivery, a refusal is about a write that was rejected.
        failed_operations is deliberately not in this chain - it is keyed by scope rather than
        by event, so attributing one to a particular delivery would be an inference, not a copy.
        """
        if delivery is not None and delivery["hold_reason"]:
            return {"source": "deliveries.hold_reason", "value": delivery["hold_reason"]}
        refusal = self.store.one(
            "SELECT reason FROM refusals WHERE event_id = ? ORDER BY id DESC LIMIT 1",
            (event_id,),
        )
        if refusal is not None:
            return {"source": "refusals.reason", "value": refusal["reason"]}
        return None

    def _resolve(self, row, head, verdict, relationship_id, generation, current_mark,
                 criteria_current=True) -> str:
        if row["status"] == "cancelled":
            return ABANDONED
        if row["status"] == "archived" or row["superseded_by"]:
            return CLOSED
        if row["status"] == "paused":
            # A paused assignment still owns its child and is not a completed one. Showing an
            # older verified here would hide the very thing an operator needs to see.
            return PAUSED
        if head["evidence"] in AMBIGUOUS:
            # Ambiguity is observable rather than masked by whatever the last verdict said.
            return AMBIGUOUS_STATE
        if verdict is not None and verdict["verdict"] == VERIFIED and not criteria_current:
            # The criteria changed after this revision was certified. The verdict is real
            # history, but it certified wording nobody is judging by now, so it is not a
            # current completion and it is not something to integrate.
            return REREVIEW_NEEDED
        if current_mark is not None:
            return current_mark["mark"]
        if verdict is not None and verdict["verdict"] == VERIFIED:
            return VERIFIED
        if head["eventId"] is not None:
            previous = self._previous_needs_changes(relationship_id, generation)
            if previous is not None:
                return CORRECTED
            if self._claimed(head["eventId"]):
                return VERIFYING
            return RECEIVED
        if self._previous_needs_changes(relationship_id, generation) is not None:
            return NEEDS_CHANGES
        if self._any_receipt(relationship_id):
            return RECEIVED
        return REQUESTED

    def for_issue(self, issue_key: str) -> dict:
        """What linear-run asks BEFORE creating anything."""
        rows = self.store.all(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?"
            " ORDER BY created_at",
            (issue_key,),
        )
        assignments = [self.state(row["relationship_id"]) for row in rows]
        owning = [
            a for a in assignments
            if a["relationshipStatus"] in ("active", "paused") and a["state"] != CLOSED
        ]
        record = {
            "issueKey": issue_key,
            "assignments": assignments,
            # The one to reuse, if there is one. A paused assignment is still the owner.
            "responsibleChild": owning[0]["childTaskId"] if owning else None,
            "responsibleRelationship": owning[0]["relationshipId"] if owning else None,
        }
        record.update(self._project_context(owning))
        record["relay"] = self._relay_provenance(bool(owning))
        return record

    def _relay_provenance(self, holds: bool) -> dict:
        """Which store this answer came from, so it can be compared with the packet's.

        for_issue can answer "responsibleRelationship: null" perfectly honestly and still be
        the wrong answer, because a mistyped state directory creates an empty store and an
        empty store has no assignments. OPS-3.4 makes the proof a conjunction - doctor
        reporting the packet's state directory AND this lookup naming the expected
        relationship - and a reading that cannot say which file it read cannot take part in
        that comparison. Carrying the provenance is what lets the caller notice it asked the
        wrong store instead of concluding the issue is unowned and opening a second writer.

        holds is the predicate itself, stated once here rather than re-derived by every
        caller from the shape of responsibleRelationship. It is scoped to THIS store, which
        is the only honest scope for the claim.

        These values are provenance, never proof. A copy of the store carries the same store
        id and the same recorded socket, which is why compare_store grades a found nonce
        against device and inode instead of trusting an identifier. The field name says
        recorded for the same reason: this is the value to compare, not the evidence that
        settles the comparison.
        """
        located = self.store.locate()
        return {
            "holds": holds,
            "store": {
                "storeId": located["storeId"],
                "dbPath": located["dbPath"],
                "realPath": located["realPath"],
                "device": located["device"],
                "inode": located["inode"],
                # First-write-wins provenance out of schema_meta, and deliberately NOT the
                # socket this process resolved: a store records the socket that created it,
                # so a participant pointing at a different socket still reads this value and
                # can see that the two disagree.
                "recordedSocket": self.store.meta("socket_path"),
            },
        }

    def _project_context(self, owning) -> dict:
        """Which project owns this issue, additively.

        scopeState tells the three answers apart. scoped means the store named a project,
        unscoped means it answered that there is none - which is every assignment registered
        before the three-level linkage existed, and a normal answer rather than an error - and
        unreadable means the store did not answer at all. A caller can distinguish them, and
        none of them is completion.
        """
        import sqlite3

        blank = {"projectKey": None, "projectParentTaskId": None, "scopeState": "unscoped"}
        if not owning:
            return blank
        try:
            scoped = self.store.one(
                "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
                (owning[0]["relationshipId"],),
            )
            if scoped is None:
                return blank
            from .linkage import Linkage, PARENT as PARENT_ROLE, PROJECT as PROJECT_SCOPE

            held = Linkage(self.store, self.clock).owners(
                PROJECT_SCOPE, scoped["project_key"])
            parent_task = owning[0]["parentTaskId"]
            if len(held) > 1:
                # Two live owners, which only a store whose unique index could not be
                # installed can hold. This view is what a coordinator reads before acting on
                # an issue, so naming whichever sorted first would hand it a guessed parent
                # wearing the same shape as a known one. The contest is the answer.
                return {
                    "projectKey": scoped["project_key"],
                    "projectParentTaskId": None,
                    "scopeState": "ambiguous",
                    "projectParentCandidates": sorted(
                        record["taskId"] for record in held),
                    "parentOwnsProject": None,
                }
            holder = held[0] if held else None
            return {
                "projectKey": scoped["project_key"],
                "projectParentTaskId": holder["taskId"] if holder else None,
                "scopeState": "scoped",
                # A parent handover moves the SCOPE and not the assignments under it, so these
                # two can legitimately disagree. Surfaced here rather than left to be noticed,
                # because this view is what a coordinator reads before acting on an issue: the
                # assignment still answers to the parent named on its own row, and the project
                # is owned by somebody else.
                "parentOwnsProject": (holder is not None
                                      and holder["taskId"] == parent_task),
            }
        except sqlite3.Error:
            return {"projectKey": None, "projectParentTaskId": None,
                    "scopeState": "unreadable"}

    # ------------------------------------------------------------------- write

    def mark(self, relationship_id: str, mark: str, *, evidence: str, actor: str,
             expected_event: str) -> dict:
        """Record the one assignment fact the relay cannot observe for itself.

        The caller states WHICH revision it integrated, and that is compared with the current
        verified head inside this write transaction. Without it, an operator who integrated an
        older revision would silently bind the merge to whatever became current in between,
        which is the opposite of a record. An event id is enough on its own: it already pins
        the generation and the revision hash.

        Every read that decides the outcome happens inside the transaction, for the same reason
        the verdict path was changed: a preflight read can be raced.
        """
        from .errors import AckRefused, RefusalReason

        if mark not in MARKS:
            raise AckRefused(RefusalReason.DISPOSITION_CONFLICT, f"unknown mark {mark!r}")
        if not str(evidence or "").strip():
            raise AckRefused(RefusalReason.FINDINGS_REQUIRED, "a mark carries its evidence")
        if not str(expected_event or "").strip():
            raise AckRefused(
                RefusalReason.STALE_MARK_CONTEXT,
                "a mark names the exact event it integrated",
            )
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM relationships WHERE relationship_id = ?", (relationship_id,)
            ).fetchone()
            if row is None:
                raise AckRefused(
                    RefusalReason.UNREGISTERED_RELATIONSHIP, f"no relationship {relationship_id!r}"
                )
            if row["status"] != "active" or row["superseded_by"]:
                raise AckRefused(
                    RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                    f"relationship {relationship_id!r} is {row['status']!r}",
                )
            generation = row["execution_generation"]
            head = head_revision(db, relationship_id, generation)
            if head["eventId"] is None or head["evidence"] in AMBIGUOUS:
                raise AckRefused(
                    RefusalReason.REVISION_AMBIGUOUS,
                    f"generation {generation} has no single current revision to mark: "
                    f"{head['detail'] or head['evidence']}",
                )
            if head["eventId"] != expected_event:
                raise AckRefused(
                    RefusalReason.STALE_MARK_CONTEXT,
                    f"this mark names {expected_event!r}, but the current revision of "
                    f"generation {generation} is {head['eventId']!r}; re-read the assignment "
                    "before recording what was integrated",
                )
            verdict = self._verdict_for(head["eventId"])
            if verdict is None or verdict["verdict"] != VERIFIED:
                raise AckRefused(
                    RefusalReason.NOT_ACKNOWLEDGED,
                    f"the current revision of generation {generation} is not verified, so it "
                    "cannot be marked merged",
                )
            current_digest = self._current_digest(relationship_id)
            if not self._criteria_current(verdict, current_digest):
                raise AckRefused(
                    RefusalReason.CRITERIA_SET_CHANGED,
                    "the canonical criteria changed after this revision was verified, so it "
                    "needs re-review before it can be recorded as integrated",
                )
            db.execute(
                "INSERT INTO assignment_marks (relationship_id, mark, event_id,"
                " execution_generation, revision_hash, evidence, actor, marked_at)"
                " VALUES (?,?,?,?,?,?,?,?)"
                " ON CONFLICT(relationship_id, mark, event_id) DO UPDATE SET"
                "   evidence = excluded.evidence, actor = excluded.actor,"
                "   marked_at = excluded.marked_at",
                (
                    relationship_id, mark, head["eventId"], generation, head["revisionHash"],
                    evidence, actor, now,
                ),
            )
            self.store.journal(
                "assignment_marked", relationship_id,
                {"mark": mark, "eventId": head["eventId"], "generation": generation}, at=now,
            )
        return self.state(relationship_id)

    # ------------------------------------------------------------------ pieces

    def _marks(self, relationship_id) -> list:
        return self._marks_from(self.store, relationship_id)

    def _current_digest(self, relationship_id):
        registered = self.criteria.get(relationship_id)
        return registered["setDigest"] if registered else None

    @staticmethod
    def _criteria_current(verdict, current_digest) -> bool:
        """Was this verdict decided against the criteria now in force?

        A verdict carries the digest of the set it actually ruled on. When that differs from
        the registered one, the verdict is history rather than a current completion: findings
        made against one wording do not certify another, even when the ids are identical.
        """
        if verdict is None:
            return True
        return verdict.get("setDigest") == current_digest

    @staticmethod
    def _marks_from(store, relationship_id) -> list:
        return [
            {
                "mark": row["mark"], "eventId": row["event_id"],
                "executionGeneration": row["execution_generation"],
                "revisionHash": row["revision_hash"], "evidence": row["evidence"],
                "actor": row["actor"], "markedAt": row["marked_at"],
            }
            for row in store.all(
                "SELECT * FROM assignment_marks WHERE relationship_id = ?"
                " ORDER BY marked_at",
                (relationship_id,),
            )
        ]

    @staticmethod
    def _current_mark(marks, head, generation, verdict):
        """A mark counts as state only where it describes the CURRENT verified head."""
        if head["eventId"] is None or verdict is None or verdict["verdict"] != VERIFIED:
            return None
        for candidate in marks:
            if (candidate["eventId"] == head["eventId"]
                    and candidate["executionGeneration"] == generation
                    and candidate["revisionHash"] == head["revisionHash"]):
                return candidate
        return None

    def _verdict_for(self, event_id):
        row = self.store.one("SELECT * FROM verdicts WHERE event_id = ?", (event_id,))
        if row is None:
            return None
        record = json.loads(row["record"])
        context = self.store.one(
            "SELECT set_digest, coverage FROM verdict_context WHERE event_id = ?", (event_id,)
        )
        return {
            "verdict": row["verdict"], "eventId": event_id,
            "executionGeneration": record.get("executionGeneration"),
            "nextExecutionGeneration": row["next_generation"],
            "decidedAt": row["decided_at"],
            "setDigest": context["set_digest"] if context else None,
            "coverage": context["coverage"] if context else None,
        }

    def _claimed(self, event_id) -> bool:
        return self.store.one(
            "SELECT 1 FROM verification_claims WHERE event_id = ?", (event_id,)
        ) is not None

    def _previous_needs_changes(self, relationship_id, generation):
        return self.store.one(
            "SELECT v.event_id FROM verdicts v JOIN events e ON e.event_id = v.event_id"
            " WHERE e.relationship_id = ? AND e.execution_generation < ?"
            "   AND v.verdict = 'needs_changes'"
            " ORDER BY e.execution_generation DESC LIMIT 1",
            (relationship_id, generation),
        )

    def _any_receipt(self, relationship_id) -> bool:
        return self.store.one(
            "SELECT 1 FROM events WHERE relationship_id = ? AND outcome = 'ready_for_review'",
            (relationship_id,),
        ) is not None
