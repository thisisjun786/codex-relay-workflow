"""Whose turn it is to merge into a shared target, and what has to be true first.

Parents under one supervision land work in the same repository on the same base branch. Two of
them merging at once is the failure this module prevents, and it prevents it the way the rest
of this package prevents things: by recording who holds what, refusing the second claimant, and
retaining the contest.

Three properties are worth stating before the code, because each one is a decision that could
reasonably have gone another way.

A target is a repository and a base ref, never a project. One project can own work in several
repositories and two projects can share one base branch, so keying the turn on the project
would serialise work that never contends and fail to serialise work that does. Work on a
different target is therefore parallel by construction rather than by permission.

Nothing here advances because time passed. A holder that entered merging may already have
merged, so releasing its target on a timer is exactly the race this exists to stop. An
outcome nobody can establish is recorded as unknown, it OCCUPIES the target, and the only key
is somebody restating an observation of the target. That is inconvenient on purpose: the
alternative is handing a second parent a turn while the first is mid-merge.

A message about a turn is not the turn moving. The delivery layer accepting a request to give
the turn back is recorded as an attestation in the ledger and changes no state; only the
holder's own write returns it. The same rule from the other side is that a parent asserting it
is next does not become next.
"""

import json

from .coordination import DOMAIN_MERGE_TARGET, Conflicts, Refusal, derive, exact
from .errors import CoordinationError, RefusalReason
from .identity import sha256_hex

PROJECT = "project"
PARENT = "parent"

WAITING = "waiting"
HOLDING = "holding"
MERGING = "merging"
UNKNOWN = "unknown"
LANDED = "landed"
RETURNED = "returned"
CANCELLED = "cancelled"
WITHDRAWN = "withdrawn"

# The states that hold the target against everyone else. unknown is in the set deliberately:
# an outcome nobody established is not an outcome that frees the branch.
OCCUPYING = (HOLDING, MERGING, UNKNOWN)
LIVE = (WAITING,) + OCCUPYING
CLOSED = (LANDED, RETURNED, CANCELLED, WITHDRAWN)

# release() admits these two. A landing is not a disposition a caller chooses; it is what land()
# records once the merge is confirmed.
DISPOSITIONS = (RETURNED, CANCELLED)

TRANSITION = "transition"
ATTESTATION = "attestation"
TRANSPORT_ACCEPTED = "transport_accepted"
RETURN_REQUESTED = "return_requested"


def target_key(repository, base_ref):
    return derive("tgt", exact(repository, "a repository"), exact(base_ref, "a base ref"))


def turn_id(target, holder_task_id, tenure):
    """A claim is one parent's claim on one target, for one tenure.

    holder_task_id is in the key for the reason a scope binding keys with its task: replacing
    the holder is supposed to produce a new claim rather than rewrite who the old one was.
    tenure is in it because the same parent taking the turn again later is a second tenure and
    not a replay of the first, so without it a re-request would derive the retired row's id.
    """
    return derive("mtn", target, exact(holder_task_id, "a task id"), str(tenure))


def ledger_id(turn, idempotency_key):
    return derive("mte", turn, exact(idempotency_key, "an idempotency key"))


def check_id(turn, head_sha, base_sha, checks_digest, review_digest):
    return derive("chk", turn, head_sha, base_sha, checks_digest, review_digest)


def checks_digest(required, checks):
    """The declared required set is INSIDE the digest.

    Two restatements that submit the same runs while declaring different required names are
    different claims, so they must not converge on one check record.
    """
    lines = sorted(
        "|".join((
            str(entry.get("runId", "")), str(entry.get("name", "")),
            str(entry.get("headSha", "")), str(entry.get("conclusion", "")),
            str(entry.get("attempt", "")),
        ))
        for entry in checks
    )
    payload = json.dumps(
        {"required": sorted(required), "checks": lines},
        sort_keys=True, separators=(",", ":"), ensure_ascii=False,
    )
    return sha256_hex(payload)


def review_digest(review):
    return sha256_hex(
        json.dumps(review, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    )

class MergeTurn:
    """Claims on a shared merge target, and the lifecycle of the one that holds it."""

    def __init__(self, store, clock, linkage):
        self.store = store
        self.clock = clock
        self.linkage = linkage
        self.conflicts = Conflicts(store)

    # ---------------------------------------------------------------- reading

    def turn(self, turn):
        row = self.store.one("SELECT * FROM merge_turns WHERE turn_id = ?", (turn,))
        if row is None:
            return None
        record = self._record(row)
        record["ledger"] = self.ledger(turn)
        return record

    def ledger(self, turn):
        return [
            {
                "entryId": row["entry_id"], "kind": row["kind"],
                "fromState": row["from_state"], "toState": row["to_state"],
                "evidenceKind": row["evidence_kind"], "actorTaskId": row["actor_task_id"],
                "evidence": row["evidence"], "idempotencyKey": row["idempotency_key"],
                "recordedAt": row["recorded_at"],
            }
            for row in self.store.all(
                "SELECT * FROM merge_turn_ledger"
                "  WHERE turn_id = ? ORDER BY recorded_at, entry_id",
                (turn,),
            )
        ]

    def target(self, repository, base_ref):
        """Who holds this target, who is waiting, and which waiter is ready next.

        Three facts about a return are reported separately and never collapsed:
        returnRequestedAt is somebody asking, transportAcceptedAt is a message being accepted,
        and releasedAt is the holder actually acting. Only the third releases the target.
        """
        target = target_key(repository, base_ref)
        rows = self.store.all(
            "SELECT * FROM merge_turns WHERE target_key = ?"
            "  ORDER BY requested_at, turn_id",
            (target,),
        )
        holder = next((self._record(r) for r in rows if r["state"] in OCCUPYING), None)
        waiters = [self._record(r) for r in rows if r["state"] == WAITING]
        ready = [w for w in waiters if w["declaredReady"]]
        answer = {
            "targetKey": target, "repository": repository, "baseRef": base_ref,
            "holder": holder, "waiters": waiters,
            "nextReady": ready[0] if ready else None,
            "occupied": holder is not None,
            "conflicts": self.conflicts.all(DOMAIN_MERGE_TARGET, target),
        }
        if holder is not None:
            marks = self._return_marks(holder["turnId"])
            answer["returnRequestedAt"] = marks[0]
            answer["transportAcceptedAt"] = marks[1]
            answer["releasedAt"] = holder["closedAt"]
        return answer

    def _return_marks(self, turn):
        requested, accepted = None, None
        for entry in self.ledger(turn):
            if entry["evidenceKind"] == RETURN_REQUESTED and requested is None:
                requested = entry["recordedAt"]
            if entry["evidenceKind"] == TRANSPORT_ACCEPTED and accepted is None:
                accepted = entry["recordedAt"]
        return requested, accepted

    @staticmethod
    def _record(row):
        return {
            "turnId": row["turn_id"], "targetKey": row["target_key"],
            "repository": row["repository"], "baseRef": row["base_ref"],
            "projectKey": row["project_key"], "holderTaskId": row["holder_task_id"],
            "holderHostId": row["holder_host_id"],
            "relationshipId": row["relationship_id"], "prNumber": row["pr_number"],
            "candidateHead": row["candidate_head"],
            "declaredReady": row["declared_ready"] == 1,
            "state": row["state"], "tenure": row["tenure"],
            "landedSha": row["landed_sha"], "observedBaseSha": row["observed_base_sha"],
            "closeReason": row["close_reason"], "requestedAt": row["requested_at"],
            "heldAt": row["held_at"], "mergingAt": row["merging_at"],
            "closedAt": row["closed_at"], "updatedAt": row["updated_at"],
        }

    # ------------------------------------------------------------- ownership

    def _project_owner(self, db, project_key, subject, challenger):
        """The one live parent of a project, or a refusal saying why there is not one.

        Linkage.owners returns binding RECORDS, so the task ids are read out of them rather
        than compared against them. Two owners refuses instead of sorting: a contested scope
        has no owner to act under, and picking one would turn an unknown into a settled-looking
        answer.
        """
        held = [
            record["taskId"] for record in self.linkage.owners(PROJECT, project_key)
            if record["role"] == PARENT
        ]
        if len(held) > 1:
            return None, Refusal(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                "project " + repr(project_key) + " has more than one live parent ("
                + ", ".join(repr(t) for t in sorted(held)) + "), so there is no owner to hold"
                " a merge turn under; repair the store rather than letting one of them win",
                domain=DOMAIN_MERGE_TARGET, subject=subject,
                incumbent=sorted(held)[0], challenger=challenger)
        if not held:
            return None, Refusal(
                RefusalReason.UNREGISTERED_SCOPE,
                "project " + repr(project_key) + " has no registered parent, so nobody can"
                " claim a merge turn for it",
                domain=DOMAIN_MERGE_TARGET, subject=subject, challenger=challenger)
        return held[0], None

    # ------------------------------------------------------------ ledger

    def _write_ledger(self, db, turn, *, kind, from_state, to_state, evidence_kind,
                      actor, evidence, idempotency_key, at):
        db.execute(
            "INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state,"
            " evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?)"
            " ON CONFLICT (turn_id, idempotency_key) DO NOTHING",
            (ledger_id(turn, idempotency_key), turn, kind, from_state, to_state,
             evidence_kind, actor, evidence, idempotency_key, at),
        )
    # ---------------------------------------------------------------- claiming

    def request(self, *, repository, base_ref, project_key, holder, candidate_head,
                pr_number=None, relationship_id=None, ready=False):
        """Claim the target. Holds it when free, waits when it is not.

        An occupied target queues the claim rather than refusing it, because the criterion
        asks a safe return to let another READY candidate proceed, and a store that refused
        instead of queueing would not know who those candidates are.
        """
        target = target_key(repository, base_ref)
        exact(project_key, "a project key")
        exact(holder.task_id, "a task id")
        exact(holder.host_id, "a host id")
        exact(candidate_head, "a candidate head")
        now = self.clock.iso()
        refusal, turn = None, None
        with self.store.transaction() as db:
            owner, refusal = self._project_owner(db, project_key, target, holder.task_id)
            if refusal is None and owner != holder.task_id:
                refusal = Refusal(
                    RefusalReason.SCOPE_ROLE_MISMATCH,
                    "task " + repr(holder.task_id) + " is not the registered parent of project"
                    " " + repr(project_key) + ", which is held by " + repr(owner)
                    + "; holding a merge turn is the project parent's, and saying it is your"
                    " turn is not being its parent",
                    domain=DOMAIN_MERGE_TARGET, subject=target,
                    incumbent=owner, challenger=holder.task_id)
            if refusal is None:
                # The replay branch. A second request while this parent's own claim is live
                # would derive a new tenure and collide on merge_turns_one_live_claim inside
                # this transaction - a database error with no conflict row, which is the one
                # thing the write protocol forbids.
                live = db.execute(
                    "SELECT * FROM merge_turns"
                    "  WHERE target_key = ? AND holder_task_id = ?"
                    "    AND state IN ('waiting','holding','merging','unknown')",
                    (target, holder.task_id),
                ).fetchone()
                if live is not None:
                    answer = self._record(live)
                    answer["alreadyClaimed"] = True
                    return answer
                occupied = db.execute(
                    "SELECT turn_id FROM merge_turns"
                    "  WHERE target_key = ? AND state IN ('holding','merging','unknown')",
                    (target,),
                ).fetchone()
                previous = db.execute(
                    "SELECT MAX(tenure) AS highest FROM merge_turns"
                    "  WHERE target_key = ? AND holder_task_id = ?",
                    (target, holder.task_id),
                ).fetchone()
                tenure = (previous["highest"] or 0) + 1
                state = WAITING if occupied is not None else HOLDING
                turn = turn_id(target, holder.task_id, tenure)
                db.execute(
                    "INSERT INTO merge_turns (turn_id, target_key, repository, base_ref,"
                    " project_key, holder_task_id, holder_host_id, relationship_id, pr_number,"
                    " candidate_head, declared_ready, state, tenure, requested_at, held_at,"
                    " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (turn, target, repository, base_ref, project_key, holder.task_id,
                     holder.host_id, relationship_id, pr_number, candidate_head,
                     1 if ready else 0, state, tenure, now,
                     now if state == HOLDING else None, now),
                )
                self._write_ledger(
                    db, turn, kind=TRANSITION, from_state=None, to_state=state,
                    evidence_kind="claim", actor=holder.task_id,
                    evidence="requested " + repository + " " + base_ref,
                    idempotency_key="request:" + str(tenure), at=now)
                self.store.journal(
                    "merge_turn_requested", turn,
                    {"targetKey": target, "state": state, "holder": holder.task_id}, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.turn(turn)

    def declare_ready(self, turn, *, actor, ready, candidate_head=None):
        """Record readiness, and take the target if it happens to be free.

        Never refuses on occupancy. A waiter saying it is ready is stating a fact about itself
        rather than asking permission, so an occupied target is REPORTED through blockedBy
        instead of raised. Refusing here would also have left a hole: a waiter that became
        ready after the target was already free could never have been granted it, because the
        only other route to holding is a promotion inside somebody else's release.

        A changed head resets readiness. Without that, a holder could restate its head and then
        pass the currency check unchallenged, which is the check's whole purpose.
        """
        now = self.clock.iso()
        refusal, blocked = None, None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["holder_task_id"] != actor:
                refusal = self._not_holder(row, actor, "declare readiness on")
            elif row["state"] not in (WAITING, HOLDING):
                refusal = self._wrong_state(row, actor, "declare readiness on")
            if refusal is None:
                owner, refusal = self._project_owner(
                    db, row["project_key"], row["target_key"], actor)
                if refusal is None and owner != actor:
                    refusal = self._stale_owner(row, owner, actor)
            if refusal is None:
                head = candidate_head or row["candidate_head"]
                moved = head != row["candidate_head"]
                flag = 0 if moved else (1 if ready else 0)
                state, held_at = row["state"], row["held_at"]
                if moved:
                    self._write_ledger(
                        db, turn, kind=TRANSITION, from_state=state, to_state=state,
                        evidence_kind="candidate_head_changed", actor=actor,
                        evidence=row["candidate_head"] + " -> " + head,
                        idempotency_key="head:" + head, at=now)
                if state == WAITING and flag == 1:
                    occupant = db.execute(
                        "SELECT turn_id, state FROM merge_turns"
                        "  WHERE target_key = ? AND state IN ('holding','merging','unknown')",
                        (row["target_key"],),
                    ).fetchone()
                    if occupant is None:
                        state, held_at = HOLDING, now
                        self._write_ledger(
                            db, turn, kind=TRANSITION, from_state=WAITING, to_state=HOLDING,
                            evidence_kind="took_free_target", actor=actor,
                            evidence="declared ready while the target was free",
                            idempotency_key="take:" + now, at=now)
                    else:
                        blocked = {"state": occupant["state"], "turnId": occupant["turn_id"]}
                db.execute(
                    "UPDATE merge_turns SET declared_ready = ?, candidate_head = ?, state = ?,"
                    " held_at = ?, updated_at = ? WHERE turn_id = ?",
                    (flag, head, state, held_at, now, turn),
                )
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        answer = self.turn(turn)
        answer["blockedBy"] = blocked
        return answer

    def attest(self, turn, *, evidence_kind, idempotency_key, actor, evidence):
        """Record something that HAPPENED about a turn, without moving it.

        This is where a transport accepting a message lands. It writes no state, which is the
        whole point: a delivery layer accepting a request to give the turn back is not the
        holder giving it back, and a parent asserting its turn is not its turn.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            self._write_ledger(
                db, turn, kind=ATTESTATION, from_state=row["state"], to_state=None,
                evidence_kind=exact(evidence_kind, "an evidence kind"), actor=actor,
                evidence=evidence, idempotency_key=idempotency_key, at=now)
        return self.turn(turn)

    def request_return(self, turn, *, actor, evidence):
        """Ask the holder for the turn back. Records the asking, and nothing else."""
        return self.attest(
            turn, evidence_kind=RETURN_REQUESTED,
            idempotency_key=RETURN_REQUESTED + ":" + actor, actor=actor, evidence=evidence)

    def withdraw(self, turn, *, actor):
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["holder_task_id"] != actor:
                refusal = self._not_holder(row, actor, "withdraw")
            elif row["state"] != WAITING:
                refusal = self._wrong_state(row, actor, "withdraw")
            if refusal is None:
                self._close_in(db, row, WITHDRAWN, "withdrawn by its claimant", actor, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.turn(turn)

    # ---------------------------------------------------------------- helpers

    def _row_in(self, db, turn):
        row = db.execute("SELECT * FROM merge_turns WHERE turn_id = ?", (turn,)).fetchone()
        if row is None:
            raise CoordinationError(
                RefusalReason.UNREGISTERED_SCOPE, "no merge turn " + repr(turn))
        return row

    @staticmethod
    def _not_holder(row, actor, what):
        return Refusal(
            RefusalReason.MERGE_TURN_NOT_HELD,
            "task " + repr(actor) + " does not hold turn " + repr(row["turn_id"])
            + ", which belongs to " + repr(row["holder_task_id"]) + ", so it cannot " + what
            + " it",
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=row["holder_task_id"], challenger=actor)

    @staticmethod
    def _wrong_state(row, actor, what):
        return Refusal(
            RefusalReason.MERGE_TURN_NOT_HELD,
            "turn " + repr(row["turn_id"]) + " is " + row["state"] + ", which does not admit "
            + what,
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=row["state"], challenger=actor)

    @staticmethod
    def _stale_owner(row, owner, actor):
        return Refusal(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            "task " + repr(actor) + " no longer owns project " + repr(row["project_key"])
            + ", which is held by " + repr(owner) + "; the project changed hands after this"
            " claim was made",
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=owner or "", challenger=actor)

    @staticmethod
    def _unresolved(row, actor):
        return Refusal(
            RefusalReason.MERGE_TURN_UNRESOLVED,
            "turn " + repr(row["turn_id"]) + " is " + row["state"] + " on target "
            + repr(row["target_key"]) + ". Its holder may already have merged, so no amount of"
            " waiting and no cancellation releases it. Record the outcome with land or"
            " report_unknown, then resolve_unknown with an observation of the target",
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=row["holder_task_id"], challenger=actor)

    def _supervisor_of(self, project_key):
        """The initiative supervisor above a project, or None when the walk cannot say.

        A handover, a contested scope or an unreadable store all answer None, and None means
        no supervisor authority is granted rather than authority assumed.
        """
        owners = [
            record["taskId"] for record in self.linkage.owners(PROJECT, project_key)
            if record["role"] == PARENT
        ]
        if len(owners) != 1:
            return None
        walk = self.linkage.up(task_id=owners[0], scope_key=project_key)
        if not walk.get("readable") or walk.get("state") == "ambiguous":
            return None
        for level in walk.get("levels", []):
            if level.get("scopeKind") == "initiative":
                owner = level.get("owner") or {}
                return owner.get("taskId")
        return None

    def _authority_refusal(self, row, actor, what):
        """Only the holder, or the supervisor above its project, may act on a held turn.

        Every claim path already verifies that the caller is the project's registered parent.
        The resolution paths did not, so an unrelated caller could evict a holder, wedge a
        merging target, or release an unknown one and promote somebody else behind an
        unmerged predecessor. Authority is the same question on both sides of the lifecycle.
        """
        if actor == row["holder_task_id"]:
            return None
        supervisor = self._supervisor_of(row["project_key"])
        if supervisor is not None and actor == supervisor:
            return None
        return Refusal(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            "task " + repr(actor) + " is neither the holder of turn "
            + repr(row["turn_id"]) + " nor the supervisor above project "
            + repr(row["project_key"])
            + (", which is " + repr(supervisor) if supervisor
               else ", and that project has no readable supervisor")
            + ", so it cannot " + what,
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=row["holder_task_id"], challenger=actor)

    def _close_in(self, db, row, state, reason, actor, *, at, landed_sha=None,
                  observed_base_sha=None):
        db.execute(
            "UPDATE merge_turns SET state = ?, close_reason = ?, closed_at = ?,"
            " landed_sha = COALESCE(?, landed_sha),"
            " observed_base_sha = COALESCE(?, observed_base_sha), updated_at = ?"
            " WHERE turn_id = ?",
            (state, reason, at, landed_sha, observed_base_sha, at, row["turn_id"]),
        )
        self._write_ledger(
            db, row["turn_id"], kind=TRANSITION, from_state=row["state"], to_state=state,
            evidence_kind="close", actor=actor, evidence=reason,
            idempotency_key="close:" + state, at=at)
        self.store.journal(
            "merge_turn_closed", row["turn_id"],
            {"state": state, "reason": reason, "actor": actor}, at=at)

    def _promote_in(self, db, target, at):
        """Hand the freed target to the oldest READY waiter that still owns its project.

        Ownership is re-checked here and not only at claim time: a handover between the two
        moments makes the waiter a stale owner, and promoting it would be the duplicate
        authorization the criterion forbids. A waiter that no longer owns its project is
        withdrawn with its contest retained, and the next one is tried.

        A bounded walk over the rows one query returned, never a wait: there is no loop here
        that can run longer than the queue is deep.
        """
        candidates = db.execute(
            "SELECT * FROM merge_turns"
            "  WHERE target_key = ? AND state = 'waiting' AND declared_ready = 1"
            "  ORDER BY requested_at, turn_id",
            (target,),
        ).fetchall()
        for candidate in candidates:
            owner, refusal = self._project_owner(
                db, candidate["project_key"], target, candidate["holder_task_id"])
            if refusal is None and owner == candidate["holder_task_id"]:
                db.execute(
                    "UPDATE merge_turns SET state = ?, held_at = ?, updated_at = ?"
                    " WHERE turn_id = ?",
                    (HOLDING, at, at, candidate["turn_id"]),
                )
                self._write_ledger(
                    db, candidate["turn_id"], kind=TRANSITION, from_state=WAITING,
                    to_state=HOLDING, evidence_kind="promoted",
                    actor=candidate["holder_task_id"],
                    evidence="promoted when the target was released",
                    idempotency_key="promote:" + at, at=at)
                return candidate["turn_id"]
            stale = refusal or self._stale_owner(candidate, owner, candidate["holder_task_id"])
            self._close_in(
                db, candidate, WITHDRAWN,
                "withdrawn at promotion: " + stale.reason.value,
                candidate["holder_task_id"], at=at)
            self.conflicts.record_in(db, stale, at=at)
        return None
    # ----------------------------------------------------------- pre-merge

    def begin_merge(self, turn, *, actor, head_sha, base_sha, checks, review, required=()):
        """Restate exact head and base, the checks and the review, immediately before merging.

        Every outcome writes a merge_turn_checks row, including a refused one, because a
        refused check is the evidence for the safe return that follows it. The turn stays
        holding on a refusal, so the holder's next legal move is release(returned) and the
        next ready candidate proceeds.

        What this establishes and what it does not. It verifies the restated evidence is
        internally consistent and current against what this store knows: the head has not
        moved since the claim, the base matches the last landing recorded here, every declared
        required check is present and successful on that head at its highest submitted
        attempt, and the review was paginated to the end with nothing unresolved. It does NOT
        establish what a forge requires: this package never contacts one, so the required set
        is the caller's declaration and is stored as requiredDeclared rather than discovered.
        """
        required = sorted({str(name) for name in (required or ())})
        checks = [dict(entry) for entry in (checks or [])]
        review = dict(review or {})
        digest_c, digest_r = checks_digest(required, checks), review_digest(review)
        now = self.clock.iso()
        refusal, verified_against = None, None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["holder_task_id"] != actor:
                refusal = self._not_holder(row, actor, "begin a merge on")
            elif row["state"] != HOLDING:
                refusal = self._wrong_state(row, actor, "beginning a merge")
            elif row["declared_ready"] != 1:
                # Holding a free target is not the same as saying the candidate is ready, and
                # a head rewrite deliberately resets readiness. Without this the reset could
                # be walked straight past.
                refusal = Refusal(
                    RefusalReason.MERGE_CANDIDATE_MOVED,
                    "turn " + repr(turn) + " has not declared its candidate ready, so there"
                    " is nothing saying " + repr(row["candidate_head"]) + " is the head it"
                    " means to merge",
                    domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                    incumbent=row["candidate_head"], challenger=actor)
            if refusal is None:
                # Ownership again, at the last moment it can still matter. A handover between
                # the claim and the merge leaves the former parent holding a turn for a
                # project it no longer owns, and this is the write that lands work.
                held = [
                    record["taskId"]
                    for record in self.linkage.owners(PROJECT, row["project_key"])
                    if record["role"] == PARENT
                ]
                if held != [actor]:
                    refusal = self._stale_owner(row, held[0] if held else None, actor)
            if refusal is None and head_sha != row["candidate_head"]:
                refusal = Refusal(
                    RefusalReason.MERGE_CANDIDATE_MOVED,
                    "the candidate head is " + repr(row["candidate_head"]) + " and the restated"
                    " head is " + repr(head_sha) + "; the turn was granted for the first",
                    domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                    incumbent=row["candidate_head"], challenger=head_sha)
            if refusal is None:
                landing = db.execute(
                    "SELECT observed_base_sha FROM merge_turns"
                    "  WHERE target_key = ? AND observed_base_sha IS NOT NULL"
                    "  ORDER BY closed_at DESC, turn_id DESC LIMIT 1",
                    (row["target_key"],),
                ).fetchone()
                if landing is not None and landing["observed_base_sha"] != base_sha:
                    refusal = Refusal(
                        RefusalReason.MERGE_CURRENCY_STALE,
                        "the last landing on this target observed base "
                        + repr(landing["observed_base_sha"]) + " and this restates "
                        + repr(base_sha) + "; the base moved under the candidate",
                        domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                        incumbent=landing["observed_base_sha"], challenger=base_sha)
            if refusal is None:
                refusal = self._check_refusal(row, actor, head_sha, required, checks)
            if refusal is None:
                refusal = self._review_refusal(row, actor, review)
            if refusal is None and row["relationship_id"]:
                verified_against, refusal = self._relationship_refusal(
                    db, row, actor, head_sha)
            db.execute(
                "INSERT INTO merge_turn_checks (check_id, turn_id, head_sha, base_sha,"
                " required, checks_digest, checks, review_digest, review, result,"
                " refusal_reason, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"
                " ON CONFLICT (check_id) DO UPDATE SET result = excluded.result,"
                " refusal_reason = excluded.refusal_reason, recorded_at = excluded.recorded_at",
                (check_id(turn, head_sha, base_sha, digest_c, digest_r), turn, head_sha,
                 base_sha, json.dumps(required), digest_c, json.dumps(checks), digest_r,
                 json.dumps(review), "refused" if refusal is not None else "current",
                 refusal.reason.value if refusal is not None else None, now),
            )
            if refusal is None:
                db.execute(
                    "UPDATE merge_turns SET state = ?, merging_at = ?, updated_at = ?"
                    ", checked_base_sha = ? WHERE turn_id = ?",
                    (MERGING, now, now, base_sha, turn),
                )
                self._write_ledger(
                    db, turn, kind=TRANSITION, from_state=HOLDING, to_state=MERGING,
                    evidence_kind="currency_confirmed", actor=actor,
                    evidence=check_id(turn, head_sha, base_sha, digest_c, digest_r),
                    idempotency_key="merging:" + head_sha, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        answer = self.turn(turn)
        answer["checkId"] = check_id(turn, head_sha, base_sha, digest_c, digest_r)
        answer["requiredDeclared"] = required
        answer["headVerifiedAgainst"] = verified_against
        return answer

    @staticmethod
    def _check_refusal(row, actor, head_sha, required, checks):
        """Present, successful, on this head, and at the highest attempt submitted for its run.

        The attempt rule matters because a rerun is how a red check becomes green: accepting
        any successful entry would let an older passing attempt stand for a run whose newest
        attempt failed.
        """
        def stale(detail, incumbent=""):
            return Refusal(
                RefusalReason.MERGE_CURRENCY_STALE, detail,
                domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                incumbent=incumbent, challenger=actor)

        if not checks:
            return stale("no check runs were restated, so nothing says this head is green")
        nameless = [
            entry for entry in checks
            if not str(entry.get("runId", "")).strip() or not str(entry.get("name", "")).strip()
        ]
        if nameless:
            # A conclusion with nothing identifying it cannot be checked against anything,
            # and with no declared required names it was the only evidence there was.
            return stale(
                "a restated check carries no runId or no name, so there is nothing to say"
                " which check it is or to compare against a required set")
        highest = {}
        for entry in checks:
            run = str(entry.get("runId", ""))
            highest[run] = max(highest.get(run, -1), int(entry.get("attempt", 1) or 1))
        for entry in checks:
            run = str(entry.get("runId", ""))
            if int(entry.get("attempt", 1) or 1) != highest[run]:
                continue
            # Every entry has to be ABOUT this head, because one that is not is evidence
            # about another commit and has no business in this set.
            if entry.get("headSha") != head_sha:
                return stale(
                    "check run " + repr(run) + " reports head "
                    + repr(entry.get("headSha")) + ", not " + repr(head_sha), run)
            # A conclusion is only binding for a check the caller declared required. An
            # optional lint failing alongside a green dev-gate is not a reason to refuse a
            # merge, and refusing it made the declared set mean nothing.
            if str(entry.get("name", "")) in required \
                    and entry.get("conclusion") != "success":
                return stale(
                    "required check " + repr(entry.get("name")) + " (run " + repr(run)
                    + ") concluded " + repr(entry.get("conclusion"))
                    + " on its newest attempt", run)
        present = {
            str(entry.get("name", "")) for entry in checks
            if int(entry.get("attempt", 1) or 1) == highest[str(entry.get("runId", ""))]
            and entry.get("conclusion") == "success"
        }
        missing = [name for name in required if name not in present]
        if missing:
            return stale(
                "these checks were declared required and are not present and successful in"
                " the restated set: " + repr(missing), missing[0])
        if not required and not present:
            # With nothing declared required, the set still has to contain something green on
            # this head; otherwise an all-red restatement would pass for want of a rule.
            return stale(
                "no check declared required and nothing in the restated set succeeded on "
                + repr(head_sha) + ", so nothing says this head is green")
        return None

    @staticmethod
    def _review_refusal(row, actor, review):
        """Every page read, every thread seen, nothing unresolved."""
        problems = []
        if review.get("hasNextPage"):
            problems.append("hasNextPage is still true, so the review was not enumerated")
        if int(review.get("pagesRead", 0) or 0) < 1:
            problems.append("no review page was read")
        seen = review.get("threadsSeen") or []
        total = int(review.get("totalCount", 0) or 0)
        identifiers = [str(one) for one in seen if str(one or "").strip()]
        distinct = set(identifiers)
        if len(identifiers) != len(seen):
            problems.append("threadsSeen contains a blank identifier")
        if len(distinct) != len(identifiers):
            # Counting entries does not establish that each one is a different thread. A
            # duplicated page substitutes a thread nobody read without changing the length.
            problems.append("threadsSeen repeats an identifier, so its length is not a count"
                            " of threads actually seen")
        if len(distinct) != total:
            problems.append(
                "totalCount is " + str(total) + " and " + str(len(seen))
                + " threads were seen")
        if int(review.get("unresolved", 0) or 0) != 0:
            problems.append(str(review.get("unresolved")) + " threads are unresolved")
        if not problems:
            return None
        return Refusal(
            RefusalReason.MERGE_REVIEW_INCOMPLETE, "; ".join(problems),
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"], challenger=actor)

    def _relationship_refusal(self, db, row, actor, head_sha):
        """When the claim named a relay assignment, its recorded head must agree.

        Optional on purpose: a parent's pull request is not always a registered child
        deliverable, and requiring one would make the turn unusable for the ordinary case.
        When it IS named, disagreement is a refusal and two different recorded heads are an
        ambiguity this refuses to resolve rather than picking the newest.
        """
        attachment = self.linkage.attachment(row["relationship_id"])
        if attachment is not None and attachment.get("projectKey") not in (None,
                                                                          row["project_key"]):
            return None, Refusal(
                RefusalReason.FOREIGN_SCOPE,
                "relationship " + repr(row["relationship_id"]) + " belongs to project "
                + repr(attachment.get("projectKey")) + ", not " + repr(row["project_key"]),
                domain=DOMAIN_MERGE_TARGET, subject=row["target_key"], challenger=actor)
        # The CURRENT submission only. work_reports retains every submission and generation,
        # so distinct historical heads are ordinary evidence of revision rather than competing
        # candidates; reading them all turned a normal correction into a permanent ambiguity
        # that no candidate could ever pass.
        newest = db.execute(
            "SELECT MAX(execution_generation) AS generation FROM work_reports"
            "  WHERE relationship_id = ? AND head_sha IS NOT NULL",
            (row["relationship_id"],),
        ).fetchone()
        if newest is None or newest["generation"] is None:
            return None, None
        latest = db.execute(
            "SELECT MAX(submission_no) AS submission FROM work_reports"
            "  WHERE relationship_id = ? AND execution_generation = ? AND head_sha IS NOT NULL",
            (row["relationship_id"], newest["generation"]),
        ).fetchone()
        heads = [
            r["head_sha"] for r in db.execute(
                "SELECT DISTINCT head_sha FROM work_reports"
                "  WHERE relationship_id = ? AND head_sha IS NOT NULL"
                "    AND execution_generation = ? AND submission_no = ?"
                "  ORDER BY head_sha",
                (row["relationship_id"], newest["generation"], latest["submission"]),
            ).fetchall()
        ]
        if not heads:
            return None, None
        if len(heads) > 1:
            return None, Refusal(
                RefusalReason.REVISION_AMBIGUOUS,
                "relationship " + repr(row["relationship_id"]) + " has work reports naming "
                + repr(heads) + "; which one this candidate is cannot be read off them",
                domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                incumbent=heads[0], challenger=head_sha)
        if heads[0] != head_sha:
            return None, Refusal(
                RefusalReason.MERGE_CANDIDATE_MOVED,
                "the work report for " + repr(row["relationship_id"]) + " names head "
                + repr(heads[0]) + " and this restates " + repr(head_sha),
                domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                incumbent=heads[0], challenger=head_sha)
        return heads[0], None
    # ------------------------------------------------------------- outcomes

    def land(self, turn, *, actor, landed_sha, observed_base_sha, evidence):
        """Confirm the merge landed, and close the tenure in the same transaction.

        Landing IS the return. Keeping the target occupied after a confirmed landing would
        wedge it if the holder then died, with nothing gained: a merge sha and an observed base
        are positive evidence that the merge finished. Ordering is preserved inside the write
        instead of across two calls that can lose the second one.
        """
        if not str(evidence or "").strip():
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                "a landing states what was observed; this package cannot watch a forge, so the"
                " evidence is the only thing that makes the landing a fact")
        exact(landed_sha, "a landed sha")
        exact(observed_base_sha, "an observed base sha")
        now = self.clock.iso()
        refusal, promoted = None, None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["holder_task_id"] != actor:
                refusal = self._not_holder(row, actor, "record a landing on")
            elif row["state"] != MERGING:
                refusal = self._wrong_state(row, actor, "recording a landing")
            if refusal is None:
                self._close_in(
                    db, row, LANDED, evidence, actor, at=now,
                    landed_sha=landed_sha, observed_base_sha=observed_base_sha)
                promoted = self._promote_in(db, row["target_key"], now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return {"released": self.turn(turn),
                "promoted": self.turn(promoted) if promoted else None,
                "ledger": self.ledger(turn)}

    def report_unknown(self, turn, *, actor, reason):
        """The holder or its supervisor stating the outcome cannot be established.

        This OCCUPIES the target. That is the point: an outcome nobody established is not an
        outcome that frees the branch, and handing the turn on while a merge may have completed
        is the race this module exists to prevent.
        """
        if not str(reason or "").strip():
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED, "an unknown outcome states why")
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["state"] != MERGING:
                refusal = self._wrong_state(row, actor, "reporting an unknown outcome")
            else:
                refusal = self._authority_refusal(row, actor, "report its outcome unknown")
            if refusal is None:
                db.execute(
                    "UPDATE merge_turns SET state = ?, close_reason = ?, updated_at = ?"
                    " WHERE turn_id = ?",
                    (UNKNOWN, reason, now, turn),
                )
                self._write_ledger(
                    db, turn, kind=TRANSITION, from_state=MERGING, to_state=UNKNOWN,
                    evidence_kind="outcome_unknown", actor=actor, evidence=reason,
                    idempotency_key="unknown:" + actor, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.turn(turn)

    def resolve_unknown(self, turn, *, actor, observed_base_sha, pr_state, evidence):
        """The only key to an unknown target: somebody restating what they observed.

        Not a timer, not a cancellation, not a supervisor's authority on its own. The
        observation decides the outcome rather than the caller: a merged pull request or a base
        that moved lands the turn, anything else returns it.
        """
        if not str(evidence or "").strip():
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                "resolving an unknown outcome requires the observation that resolves it;"
                " elapsed time is not one and never becomes one")
        exact(observed_base_sha, "an observed base sha")
        now = self.clock.iso()
        refusal, promoted, landed = None, None, False
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["state"] != UNKNOWN:
                refusal = self._wrong_state(row, actor, "resolving an unknown outcome")
            else:
                refusal = self._authority_refusal(row, actor, "resolve its outcome")
            if refusal is None:
                # Against the base the currency check CONFIRMED, not against observed_base_sha,
                # which is null until this very call and so made every observation look like a
                # movement. That falsely landed an open pull request and released its target.
                # With no checked base the observation cannot establish a movement at all, so
                # only the pull request's own state decides.
                # The pull request's own state decides. A base that moved says the branch
                # advanced, which any unrelated commit also does, so reading it as a landing
                # released an open candidate's target and promoted somebody behind it. It is
                # kept only as corroboration for an outcome the caller could not read.
                moved = (row["checked_base_sha"] is not None
                         and observed_base_sha != row["checked_base_sha"])
                if pr_state == "merged":
                    landed = True
                elif pr_state in ("open", "closed"):
                    landed = False
                else:
                    # Three states establish an outcome and nothing else does. Treating an
                    # unrecognised one as not-landed let a typo close the turn and promote a
                    # waiter while the merge may well have completed - and the base being
                    # unchanged says nothing either, since an unmerged candidate leaves it
                    # unchanged too.
                    raise CoordinationError(
                        RefusalReason.MERGE_EVIDENCE_REQUIRED,
                        repr(pr_state) + " does not say whether this candidate merged. Read"
                        " the pull request and resolve again with merged, open or closed"
                        + ("; the base moved from " + repr(row["checked_base_sha"]) + " to "
                           + repr(observed_base_sha) + ", which any unrelated commit also"
                           " does" if moved else ""))
                self._close_in(
                    db, row, LANDED if landed else RETURNED,
                    "resolved from an observation: pr_state=" + str(pr_state) + "; " + evidence,
                    actor, at=now,
                    landed_sha=row["candidate_head"] if landed else None,
                    observed_base_sha=observed_base_sha)
                promoted = self._promote_in(db, row["target_key"], now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return {"released": self.turn(turn), "outcome": LANDED if landed else RETURNED,
                "promoted": self.turn(promoted) if promoted else None,
                "ledger": self.ledger(turn)}

    def release(self, turn, *, actor, disposition, reason, evidence=""):
        """Give the turn back, or take it away, and hand it to the next ready candidate.

        The promotion happens in the SAME transaction as the release, so there is no window in
        which the target is free and the ready candidate is not holding it. The returned ledger
        is every entry recorded under the closed tenure, which is how a return reflects the
        control messages and results that arrived while it was held.
        """
        if disposition not in DISPOSITIONS:
            raise CoordinationError(
                RefusalReason.LINK_NOT_ACTIVE,
                "a disposition is " + " or ".join(DISPOSITIONS) + ", not " + repr(disposition)
                + "; a landing is recorded with land, not chosen here")
        if not str(reason or "").strip():
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED, "a release states why")
        if disposition == CANCELLED and not str(evidence or "").strip():
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                "taking a turn away from its holder requires evidence, because the holder is"
                " not the one saying it is finished")
        now = self.clock.iso()
        refusal, promoted = None, None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            if row["state"] in (MERGING, UNKNOWN):
                # The criterion's exact case. A holder that reached the currency check may
                # already have merged, so neither a return nor a cancellation is available
                # until somebody says what happened.
                refusal = self._unresolved(row, actor)
            elif row["state"] == WAITING:
                refusal = self._wrong_state(row, actor, "release; a waiting claim is withdrawn")
            elif row["state"] != HOLDING:
                refusal = self._wrong_state(row, actor, "release")
            elif disposition == RETURNED and row["holder_task_id"] != actor:
                refusal = self._not_holder(row, actor, "return")
            elif disposition == CANCELLED:
                # Taking a turn away is the supervisor's, not anyone's. Evidence alone was
                # never authority: an unrelated caller could evict a holder and promote a
                # different claim behind it.
                refusal = self._authority_refusal(row, actor, "take the turn away")
            if refusal is None:
                self._close_in(
                    db, row, disposition,
                    reason if disposition == RETURNED else reason + "; " + evidence,
                    actor, at=now)
                promoted = self._promote_in(db, row["target_key"], now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return {"released": self.turn(turn),
                "promoted": self.turn(promoted) if promoted else None,
                "ledger": self.ledger(turn)}
