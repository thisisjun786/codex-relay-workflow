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
from . import mergeevidence, report

PROJECT = "project"
PARENT = "parent"
# linkage.ACTIVE, spelled here for the same reason PROJECT and PARENT are: this module reads
# the linkage through the object it was given rather than importing it. paused is LIVE
# ownership there and stays live ownership here - a paused parent keeps its claim and its
# place in the queue. This constant is read only where ACTING is the question.
ACTIVE = "active"

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
GRANT = "grant"
GRANT_ACKNOWLEDGED = "grant_acknowledged"
READINESS_DECLARED = "readiness_declared"
READINESS_WITHDRAWN = "readiness_withdrawn"

# What this module writes, and therefore what nobody else may write through attest().
#
# attest() takes any evidence kind and any idempotency key, and the ledger converges with
# ON CONFLICT DO NOTHING. Without this, a caller could write one of these keys onto a turn
# first and the engine's own later write would be discarded in silence - the transition, the
# grant or the acknowledgement simply absent, with nothing anywhere saying so. Reserved as a
# CLASS rather than as the two names this issue adds, because the hole was never specific to
# them. transport_accepted and return_requested stay open: they are what the surface is for.
RESERVED_KINDS = (
    "claim", "close", "candidate_head_changed", "took_free_target", "promoted",
    "currency_confirmed", "outcome_unknown",
    GRANT, GRANT_ACKNOWLEDGED, READINESS_DECLARED, READINESS_WITHDRAWN,
)
RESERVED_PREFIXES = (
    "request:", "close:", "head:", "take:", "promote:", "merging:", "unknown:", "ready:",
    GRANT + ":", GRANT_ACKNOWLEDGED + ":",
)

# What a stored refusal means for somebody asking why a target is not moving. Each of these
# asks the holder for something different - finish the review, refresh the evidence, restate
# the head it actually means - so they are not one word. Anything this map has no word for is
# reported as a refused restatement rather than guessed at, and lastRefusal carries the
# reason verbatim either way.
REFUSAL_CAUSES = {
    RefusalReason.MERGE_CURRENCY_STALE.value: "required_evidence_not_current",
    RefusalReason.MERGE_REVIEW_INCOMPLETE.value: "review_not_finished",
    RefusalReason.MERGE_CANDIDATE_MOVED.value: "candidate_moved",
}


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


def grant_id(turn, tenure, sequence):
    """One grant on one turn, named from what defines it.

    A turn IS one tenure - turn_id derives from it - and within that tenure a parent can be
    granted the target once and then granted each candidate it restates. So a grant is the
    turn, its tenure, and WHICH grant this is: the sequence counts grants already recorded on
    this turn, so successive ones cannot collide however the candidate moves.

    Two earlier spellings of this key were wrong in the same way, which is why it is spelled
    this way now. Deriving it from the write's timestamp made one grant look like two to two
    readers. Deriving it from the candidate made A then B then back to A collide with the
    grant A already had, so the ledger's consistency check refused the restatement and the
    holder could not restore a candidate it had used before.

    So nothing RECOMPUTES a grant's identity from the turn's current state. It is written
    once, with the sequence the ledger gave it, and every reader resolves the current grant by
    reading the newest one recorded rather than by deriving a key and hoping it agrees.
    """
    return derive("mtg", turn, str(tenure), str(sequence))


def reserved(evidence_kind, idempotency_key):
    """Whether a caller is reaching for something only this module may write."""
    if evidence_kind in RESERVED_KINDS:
        return "evidence kind " + repr(evidence_kind)
    for prefix in RESERVED_PREFIXES:
        if str(idempotency_key).startswith(prefix):
            return "idempotency key " + repr(idempotency_key) + ", which is in the " \
                   + repr(prefix) + " namespace"
    return ""


def grant_envelope(entry, turn, tenure):
    """One ledger entry read as a grant this module wrote, or None when it is not one.

    Every reader of a grant goes through here, because "evidence_kind is grant" does not mean
    "this module wrote it". attest() accepted ANY kind and any text until that namespace was
    reserved in this change, and this store has no migration path, so a perfectly valid
    existing store can hold a row whose kind is grant and whose evidence is a sentence
    somebody typed. Parsing that as an envelope raised out of turn() - which every mutator
    calls AFTER its transaction commits, so the write landed and the caller got a host fault
    for an operation that had already succeeded.

    Shape is not identity, and shape alone was not enough. A legacy row whose evidence happened
    to be well-formed JSON with a big sequence became the CURRENT grant and the engine's own
    grant was then refused as stale. So the envelope has to be the one this module would have
    written for THIS turn: it names this turn and this tenure, its id is the id derived from
    those and its own sequence, and its key is that id's key. A forger has to reproduce the
    digest, and reproducing it means naming the same grant.

    Nothing here raises. A reader that can raise on stored data is the failure this exists to
    remove, not a smaller version of it.
    """
    if entry["evidenceKind"] != GRANT:
        return None
    try:
        envelope = json.loads(entry["evidence"])
    except (TypeError, ValueError):
        return None
    if not isinstance(envelope, dict):
        return None
    sequence = envelope.get("sequence")
    if not isinstance(sequence, int) or isinstance(sequence, bool):
        return None
    if envelope.get("turnId") != turn or envelope.get("tenure") != tenure:
        return None
    try:
        derived = grant_id(turn, tenure, sequence)
    except CoordinationError:
        return None
    if envelope.get("grantId") != derived:
        return None
    if entry["idempotencyKey"] != GRANT + ":" + derived:
        return None
    if not isinstance(envelope.get("recipientTaskId"), str) or not envelope["recipientTaskId"]:
        return None
    if not isinstance(envelope.get("candidateHead"), str) or not envelope["candidateHead"]:
        return None
    return envelope


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

    def __init__(self, store, clock, linkage, *, delivery=None):
        self.store = store
        self.clock = clock
        self.linkage = linkage
        self.conflicts = Conflicts(store)
        # Optional for the reason DeliveryService's own linkage is optional: absent, every
        # caller written before this keeps its exact behaviour, down to the bytes of the grant
        # envelope. Supplied, a grant that a parent did NOT cause is also pushed to it through
        # the relay's existing queue. It is never used to read or write anything about an
        # assignment's work - only to address this notice.
        self.delivery = delivery

    # ---------------------------------------------------------------- reading

    def turn(self, turn):
        row = self.store.one("SELECT * FROM merge_turns WHERE turn_id = ?", (turn,))
        if row is None:
            return None
        record = self._record(row)
        record["ledger"] = self.ledger(turn)
        record["grant"] = self._grant_record(record["ledger"], turn, row["tenure"])
        # Named rather than silently skipped. A row this module cannot read as its own grant
        # is somebody else's attestation or a damaged one, and either way an operator asking
        # why a turn reports no grant deserves to see it rather than infer it.
        record["unreadableGrants"] = [
            entry["idempotencyKey"] for entry in record["ledger"]
            if entry["evidenceKind"] == GRANT
            and grant_envelope(entry, turn, row["tenure"]) is None
        ]
        return record

    def outstanding(self, task_id):
        """Every live claim one task holds, for a parent that has just come back.

        A parent that restarted, or whose context was compacted, knows its own task id and
        nothing else: not a turn, not a target. Every other reader here needs one of those,
        which is why coming back used to end with somebody asking a human which window they
        had. An unresolved outcome is reported as unresolved rather than as a free target, and
        targetFree says whether a waiting claim could be taken right now - which is the one
        thing a resumed parent has to know, because nothing acquires a target on its behalf.
        """
        exact(task_id, "a task id")
        answer = []
        for row in self.store.all(
                "SELECT turn_id, target_key, state FROM merge_turns"
                "  WHERE holder_task_id = ? AND state IN ('waiting','holding','merging',"
                "'unknown') ORDER BY requested_at, turn_id",
                (task_id,)):
            record = self.turn(row["turn_id"])
            held_back = (self._acquirable(record) if record["state"] == WAITING
                         else "already " + record["state"])
            record["targetFree"] = not held_back
            record["heldBackBy"] = held_back or None
            answer.append(record)
        return answer

    @staticmethod
    def _grant_record(entries, turn, tenure):
        """The turn's current grant, and whether its recipient has answered it.

        Read out of the ledger rather than stored beside the turn, because the ledger is what
        makes it durable and convergent in the first place. A claim made before grants were
        recorded answers None, which is the truth about it and not a fault.

        The NEWEST grant recorded, found by reading rather than by deriving a key. Every
        earlier one stays in the ledger as what it is: a grant for something this turn has
        since moved past. Resolving the current one by re-deriving a key is what made two
        successive grants collide, so no reader does it.

        Newest by the sequence the grant itself carries, never by row order. The ledger orders
        on recorded_at and then on an entry id that is a digest, so two grants written in the
        same second have no meaningful order at all - and an injected clock writes every one of
        them in the same second.
        """
        grants = [(entry, grant_envelope(entry, turn, tenure)) for entry in entries]
        grants = [(entry, envelope) for entry, envelope in grants if envelope is not None]
        if not grants:
            return None
        notice, envelope = max(grants, key=lambda pair: pair[1]["sequence"])
        identifier = envelope["grantId"]
        answered = next(
            (entry for entry in entries
             if entry["idempotencyKey"] == GRANT_ACKNOWLEDGED + ":" + identifier), None)
        envelope["recordedAt"] = notice["recordedAt"]
        envelope["acknowledgedAt"] = answered["recordedAt"] if answered else None
        envelope["acknowledgedBy"] = answered["actorTaskId"] if answered else None
        return envelope

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
        # The same sequence _promote_in walks, in the same order. Reporting every waiter that
        # declared readiness named a parent the promotion would skip, so a holder was told to
        # hand the turn to somebody who could not take it.
        ready = [w for w in waiters if w["declaredReady"] and not self._withheld(w)]
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
            holder["grant"] = self._grant_record(
                self.ledger(holder["turnId"]), holder["turnId"], holder["tenure"])
        answer["blocked"] = self._blocked_report(holder, waiters)
        return answer

    def _blocked_report(self, holder, waiters):
        """Why this target is not moving, and who is behind it.

        The observation this was written for could not tell a holder that was MERGING from one
        restating the same candidate against checks that had not finished. Both read as "a
        parent has the turn", so three parents waited on a human to reassign the window. The
        cause is named from the turn's own state and from the check rows it wrote, and the
        ready peers behind it are named with it.

        Nothing here times anything out. A report is what a caller needs in order to decide,
        and a deadline is the thing this module refuses to have.

        checkSnapshots counts distinct RESTATEMENTS, never polls: check_id folds the head, the
        base, the declared required set and both digests, and the row is written ON CONFLICT DO
        UPDATE, so resubmitting identical evidence converges on one row. A rerun changes the
        attempt inside checks_digest, so a re-poll against NEW evidence does open a new one.

        Only rows about the CURRENT candidate. Check rows outlive a head change, so the newest
        one could describe a candidate this turn no longer means to merge - and once readiness
        came back on the new head, that stale refusal decided the reported cause. A candidate
        nobody has restated reads as unrestated, which is what it is.

        readyPeers is who would actually be promoted, not who declared readiness. _promote_in
        re-checks ownership and skips a paused owner, so listing every ready waiter told a
        holder to return the turn for a peer that cannot take it. The rest are reported beside
        them with the reason they are held back.

        The cause of a refused restatement is read from the refusal that was stored, not
        assumed. begin_merge writes a check row for EVERY refusal it reaches - an unfinished
        review, a base that moved, a candidate that moved - so calling all of them unfinished
        checks would put a confident wrong word on three different problems.
        """
        if holder is None:
            return None
        rows = self.store.all(
            "SELECT * FROM merge_turn_checks WHERE turn_id = ?"
            "  ORDER BY recorded_at DESC, check_id DESC",
            (holder["turnId"],))
        # Two selections over one ordered read, because a check row records the head its
        # CALLER restated and not the candidate it was judged against. A restatement of the
        # current candidate always decides; absent one, a recorded candidate movement is the
        # newest thing that happened to this turn and is reported rather than dropped.
        current = [row for row in rows if row["head_sha"] == holder["candidateHead"]]
        moved = [row for row in rows
                 if row["refusal_reason"] == RefusalReason.MERGE_CANDIDATE_MOVED.value]
        latest, tied = self._settled(current if current else moved)
        if holder["state"] == MERGING:
            cause = "merge_in_flight"
        elif holder["state"] == UNKNOWN:
            cause = "outcome_unknown"
        elif self._withheld(holder) == "not_the_project_owner":
            # The project changed hands under this holder. Asking _owner_status directly here
            # answered about the REPLACEMENT, which is active, so a stale holder that can
            # neither restate nor merge was reported as merely not having restated - and the
            # remedy a reader took from that was the wrong one. Every other path on this
            # surface establishes that its subject is the owner before asking whether that
            # owner can act; this one now asks the same predicate about the same subject.
            cause = "holder_no_longer_owns_the_project"
        elif self._withheld(holder) == "owner_paused":
            # The holder itself is not running. Every other answer would send a peer to wait
            # on a candidate nobody is advancing.
            cause = "holder_paused"
        elif not holder["declaredReady"]:
            cause = "candidate_not_ready"
        elif tied:
            cause = "restatements_disagree"
        elif latest is not None and latest["result"] == "refused":
            cause = REFUSAL_CAUSES.get(latest["refusal_reason"], "restatement_refused")
        else:
            cause = "candidate_not_restated"
        peers = [(waiter, self._withheld(waiter))
                 for waiter in waiters if waiter["declaredReady"]]
        return {
            "cause": cause, "turnId": holder["turnId"],
            "holderTaskId": holder["holderTaskId"],
            "candidateHead": holder["candidateHead"], "prNumber": holder["prNumber"],
            "checkSnapshots": len(current),
            "lastResult": latest["result"] if latest is not None else None,
            "lastRefusal": latest["refusal_reason"] if latest is not None else None,
            "lastCheckedHead": latest["head_sha"] if latest is not None else None,
            "ambiguousRestatements": list(tied),
            "readyPeers": [
                {"turnId": waiter["turnId"], "holderTaskId": waiter["holderTaskId"],
                 "candidateHead": waiter["candidateHead"], "prNumber": waiter["prNumber"]}
                for waiter, why in peers if not why],
            "withheldPeers": [
                {"turnId": waiter["turnId"], "holderTaskId": waiter["holderTaskId"],
                 "candidateHead": waiter["candidateHead"],
                 "prNumber": waiter["prNumber"], "reason": why}
            for waiter, why in peers if why],
        }

    @staticmethod
    def _settled(rows):
        """The newest verdict among the rows sharing the newest instant, or nothing decided.

        merge_turn_checks records recorded_at to the second and check_id is a digest, so two
        restatements written inside one second carry no order to read. Ordering by the digest
        is not a chronology; it is a coin that lands the same way every time, which is worse
        than no answer because it looks like one.

        Where the rows sharing the newest instant agree, that IS the verdict whichever is
        picked. Where they disagree, nothing stored here knows which came last, and the report
        says so instead of choosing.
        """
        if not rows:
            return None, ()
        newest = [row for row in rows if row["recorded_at"] == rows[0]["recorded_at"]]
        verdicts = {(row["result"], row["refusal_reason"]) for row in newest}
        if len(verdicts) > 1:
            return None, tuple(sorted(row["check_id"] for row in newest))
        return newest[0], ()

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

    def _owner_status(self, project_key):
        """The single live parent's binding status, or None when the walk cannot name one.

        Asked only where ACTING is the question, never where owning is. A paused parent owns
        its project and keeps its merge claim; what it cannot do is hold a target it is not
        running to use, which is how a pause turned into a wedge that only a human noticed.
        """
        held = [record for record in self.linkage.owners(PROJECT, project_key)
                if record["role"] == PARENT]
        if len(held) != 1:
            return None
        return held[0]["status"]

    def _withheld(self, record):
        """Why this waiter would NOT be promoted, or "" when it would.

        The conditions _promote_in applies, asked by the readers that report who is next. A
        report that called every waiter with declared_ready ready-to-proceed was telling a
        holder to return the turn for a peer the promotion would skip, which frees the target
        and moves nobody.
        """
        held = [owner["taskId"] for owner in self.linkage.owners(PROJECT, record["projectKey"])
                if owner["role"] == PARENT]
        if held != [record["holderTaskId"]]:
            return "not_the_project_owner"
        if self._owner_status(record["projectKey"]) != ACTIVE:
            return "owner_paused"
        return ""

    def _acquirable(self, record):
        """Why this waiting claim could not take its target right now, or "" when it could.

        Occupancy is one condition of three. Reporting it alone advertised a target that
        declare_ready would refuse, which sends a parent that just came back to do the one
        thing that cannot work.
        """
        occupant = self.store.one(
            "SELECT turn_id FROM merge_turns"
            "  WHERE target_key = ? AND state IN ('holding','merging','unknown')",
            (record["targetKey"],))
        if occupant is not None:
            return "target_occupied"
        return self._withheld(record)

    def _unanswered_grant(self, db, row, actor):
        """A grant nobody answered is an audit entry, not a gate.

        The point of recording who was given the turn is that a parent stops acting on what it
        remembers, so a merge that walks past its own grant leaves the notice doing no work at
        all. Acknowledging is what says this candidate was re-checked against the store.

        A turn with NO grant is not held to this. One that reached holding before grants were
        recorded has nothing to answer, and this store has no migration path, so demanding an
        answer that cannot exist would wedge that turn permanently with no supported repair.
        """
        current = self._current_grant_in(db, row["turn_id"], row["tenure"])
        if current is None:
            return None
        answered = db.execute(
            "SELECT recorded_at FROM merge_turn_ledger"
            "  WHERE turn_id = ? AND idempotency_key = ?",
            (row["turn_id"], GRANT_ACKNOWLEDGED + ":" + current),
        ).fetchone()
        if answered is not None:
            return None
        return Refusal(
            RefusalReason.MERGE_TURN_NOT_HELD,
            "turn " + repr(row["turn_id"]) + " was granted " + repr(current) + " and has not"
            " acknowledged it, so nothing records that this candidate was re-checked against"
            " the store rather than against what its holder remembers. Acknowledge the grant,"
            " then merge",
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=current, challenger=actor)

    @staticmethod
    def _current_grant_in(db, turn, tenure):
        """The newest grant recorded on a turn, read rather than re-derived.

        By the sequence the grant carries, because the ledger's own order is recorded_at and
        then a digest, which says nothing about which of two grants written in one second came
        second.

        Through the same recogniser the readers use, so a legacy attestation under this kind
        cannot make a mutator raise from inside its own transaction.
        """
        rows = db.execute(
            "SELECT evidence, evidence_kind, idempotency_key FROM merge_turn_ledger"
            "  WHERE turn_id = ? AND evidence_kind = ?",
            (turn, GRANT),
        ).fetchall()
        envelopes = [
            envelope for envelope in (
                grant_envelope({"evidenceKind": row["evidence_kind"],
                                "idempotencyKey": row["idempotency_key"],
                                "evidence": row["evidence"]}, turn, tenure)
                for row in rows)
            if envelope is not None
        ]
        if not envelopes:
            return None
        return max(envelopes, key=lambda envelope: envelope["sequence"])["grantId"]

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
        if evidence_kind not in RESERVED_KINDS:
            return
        # A reserved write reads itself back. The insert above converges on conflict, which is
        # what makes a replay one fact - and is also what would make a key somebody else wrote
        # first look like a successful write of ours. attest() refuses this namespace, so a
        # row of the wrong kind here means the ledger already disagrees with itself. Raising
        # rolls the whole operation back, because a transition whose record was silently
        # dropped is worse than an operation that did not happen.
        #
        # Every field, not just the kind. A row that agrees about the kind and disagrees about
        # the actor, the evidence or the states it moved between is still not the row this
        # call meant to write, and every reader downstream would consume the other one.
        seen = db.execute(
            "SELECT kind, from_state, to_state, evidence_kind, actor_task_id, evidence"
            "  FROM merge_turn_ledger"
            "  WHERE turn_id = ? AND idempotency_key = ?",
            (turn, idempotency_key),
        ).fetchone()
        if seen is None:
            return
        wrote = (kind, from_state, to_state, evidence_kind, actor, evidence)
        if tuple(seen[column] for column in (
                "kind", "from_state", "to_state", "evidence_kind", "actor_task_id",
                "evidence")) != wrote:
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                "turn " + repr(turn) + " already holds " + repr(idempotency_key) + " as "
                + repr(seen["evidence_kind"]) + " by " + repr(seen["actor_task_id"])
                + ", so recording " + repr(evidence_kind) + " under it would be discarded"
                " without anything saying so; this ledger is inconsistent and the operation is"
                " rolled back rather than half written")

    def _grant_in(self, db, *, turn, tenure, target, repository, base_ref, recipient, head,
                  state, granted_from, at, relationship_id=None, project_key=None,
                  wake=False):
        """The notice that a parent now holds this target, written WITH the acquisition.

        Addressed, not broadcast. actor_task_id carries the RECIPIENT rather than whoever's
        release freed the target, because the question a parent asks this ledger when it comes
        back is which rows are addressed to it. The recipient is the owner verified inside this
        same transaction, so a project that changed hands between a release and the promotion
        that followed it is never notified at an address that is already stale.

        The ledger row itself is still not a push. It is durable, its identity is logical and
        it converges, so a retry, a restart and a duplicate reading all describe one grant, and
        a parent finds it by re-reading its own claims on entry.

        That is enough for a parent that comes back, and a parent that did not CAUSE this grant
        may not come back at all. So wake also hands the notice to the relay's existing queue,
        and ONLY a promotion sets it: a claim and a late-ready acquisition are the recipient's
        own call, and messaging a parent about something it has just done is the heartbeat this
        contract family refuses.

        The wake is resolved BEFORE the ledger row is written, never after. _write_ledger
        compares a reserved row's stored evidence field by field on readback, so an envelope
        amended once it is written is a contradiction that rolls the whole promotion back -
        and the promotion is somebody's merge turn. Every input to the event's identity is
        known here, so nothing has to be amended.

        An address that cannot be found is not an error. The grant is written either way and
        the envelope says which happened, so a reader of the turn can tell a notice that was
        never addressable from one that was sent.
        """
        sequence = db.execute(
            "SELECT COUNT(*) AS seen FROM merge_turn_ledger"
            "  WHERE turn_id = ? AND evidence_kind = ?",
            (turn, GRANT),
        ).fetchone()["seen"] + 1
        grant = grant_id(turn, tenure, sequence)
        envelope = {
            "kind": "merge_turn_grant", "grantId": grant, "turnId": turn,
            "tenure": tenure, "sequence": sequence,
            "targetKey": target, "repository": repository,
            "baseRef": base_ref, "recipientTaskId": recipient, "candidateHead": head,
            "grantedFrom": granted_from,
        }
        queued = None
        if wake and self.delivery is not None:
            channel = self.delivery.grant_channel_in(
                db, relationship_id=relationship_id, recipient_task_id=recipient,
                grant=grant, project_key=project_key)
            if channel.get("eventId"):
                envelope["wake"] = {"eventId": channel["eventId"]}
                queued = channel
            else:
                envelope["wake"] = {"refused": channel["refused"]}
                self.store.journal(
                    "merge_turn_wake_unaddressed", turn,
                    {"grantId": grant, "recipientTaskId": recipient,
                     "reason": channel["refused"]}, at=at)
        evidence = json.dumps(
            envelope, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
        self._write_ledger(
            db, turn, kind=ATTESTATION, from_state=state, to_state=None,
            evidence_kind=GRANT, actor=recipient,
            evidence=evidence,
            idempotency_key=GRANT + ":" + grant, at=at)
        if queued is not None:
            # After the ledger write, so the notice a recipient reads and the row this store
            # keeps are the same bytes, and inside the same transaction, so a promotion that
            # rolls back takes its wake with it.
            self.delivery.queue_grant_in(
                db, event_id=queued["eventId"], relationship_id=queued["relationshipId"],
                recipient_task_id=recipient, receipt=evidence, grant=grant, at=at)
            self.store.journal(
                "merge_turn_wake_queued", turn,
                {"grantId": grant, "eventId": queued["eventId"],
                 "recipientTaskId": recipient}, at=at)
        return grant

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
        refusal, turn, replayed = None, None, None
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
                    # The SAME answer the first request gave, not a thinner one. A caller
                    # retrying because that response was lost needs the grant it never saw:
                    # without it there is no acknowledgement to make, and the merge gate then
                    # refuses a turn this parent legitimately holds.
                    replayed = live["turn_id"]
            if refusal is None and replayed is None:
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
                # A paused parent QUEUES even when the target is free. It owns the project, so
                # refusing the claim would take its place away; it is not running, so handing
                # it the target would occupy the branch against every peer until somebody
                # noticed. Waiting is the only answer that costs neither.
                idle = self._owner_status(project_key) != ACTIVE
                state = WAITING if (occupied is not None or idle) else HOLDING
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
                if state == HOLDING:
                    self._grant_in(
                        db, turn=turn, tenure=tenure, target=target, repository=repository,
                        base_ref=base_ref, recipient=owner, head=candidate_head,
                        state=HOLDING, granted_from="claim", at=now)
                self.store.journal(
                    "merge_turn_requested", turn,
                    {"targetKey": target, "state": state, "holder": holder.task_id}, at=now)
            elif refusal is not None:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        if replayed is not None:
            answer = self.turn(replayed)
            answer["alreadyClaimed"] = True
            return answer
        return self.turn(turn)

    def declare_ready(self, turn, *, actor, ready, candidate_head=None, cause=""):
        """Record readiness, and take the target if it happens to be free.

        Never refuses on occupancy. A waiter saying it is ready is stating a fact about itself
        rather than asking permission, so an occupied target is REPORTED through blockedBy
        instead of raised. Refusing here would also have left a hole: a waiter that became
        ready after the target was already free could never have been granted it, because the
        only other route to holding is a promotion inside somebody else's release.

        A changed head resets readiness. Without that, a holder could restate its head and then
        pass the currency check unchallenged, which is the check's whole purpose.

        A head is the only thing this package can notice for itself. A base that moved and a
        finding that arrived are equally fatal to a readiness claim and equally invisible from
        here - nothing in this package contacts a forge - so the caller says so through cause,
        and the change is recorded either way. Before this, readiness could go from true to
        false with nothing in the ledger saying it ever had, which is how a peer reading the
        record could not tell a candidate that was never ready from one that stopped being it.
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
                    if state == HOLDING:
                        # A restatement while holding is a new candidate on a target this
                        # parent already has, so it gets its own grant to answer. Without one,
                        # the only grant named a head that no longer existed: a holder that had
                        # not answered it could never answer it, and one that had could merge a
                        # candidate it had acknowledged nothing about.
                        self._grant_in(
                            db, turn=turn, tenure=row["tenure"], target=row["target_key"],
                            repository=row["repository"], base_ref=row["base_ref"],
                            recipient=actor, head=head, state=HOLDING,
                            granted_from="candidate_restated", at=now)
                if flag != row["declared_ready"]:
                    # Readiness can die, be restored and die again on ONE head, and each of
                    # those is a separate fact with its own cause. Keying on the head alone
                    # made the second withdrawal converge onto the first and kept reporting
                    # the stale reason. A replay cannot reach this write at all - the flag has
                    # to have actually changed - so a sequence here counts real changes only.
                    sequence = db.execute(
                        "SELECT COUNT(*) AS seen FROM merge_turn_ledger"
                        "  WHERE turn_id = ? AND evidence_kind IN (?,?)",
                        (turn, READINESS_DECLARED, READINESS_WITHDRAWN),
                    ).fetchone()["seen"] + 1
                    self._write_ledger(
                        db, turn, kind=TRANSITION, from_state=state, to_state=state,
                        evidence_kind=READINESS_DECLARED if flag == 1
                        else READINESS_WITHDRAWN,
                        actor=actor,
                        evidence=cause or (
                            "the head moved to " + head if moved
                            else ("declared ready on " + head if flag == 1
                                  else "readiness withdrawn on " + head)),
                        idempotency_key="ready:" + str(row["tenure"]) + ":" + str(sequence),
                        at=now)
                if state == WAITING and flag == 1:
                    occupant = db.execute(
                        "SELECT turn_id, state FROM merge_turns"
                        "  WHERE target_key = ? AND state IN ('holding','merging','unknown')",
                        (row["target_key"],),
                    ).fetchone()
                    if occupant is not None:
                        blocked = {"state": occupant["state"], "turnId": occupant["turn_id"]}
                    elif self._owner_status(row["project_key"]) != ACTIVE:
                        # Free target, ready candidate, and an owner that is not running to
                        # use it. Taking it here would hold the branch against every peer on
                        # behalf of a parent that cannot act; the claim keeps its place and
                        # this says why, so a resumed parent knows the one call that acquires.
                        blocked = {"state": "owner_paused", "turnId": None}
                    else:
                        state, held_at = HOLDING, now
                        self._write_ledger(
                            db, turn, kind=TRANSITION, from_state=WAITING, to_state=HOLDING,
                            evidence_kind="took_free_target", actor=actor,
                            evidence="declared ready while the target was free",
                            idempotency_key="take:" + str(row["tenure"]), at=now)
                        self._grant_in(
                            db, turn=turn, tenure=row["tenure"], target=row["target_key"],
                            repository=row["repository"], base_ref=row["base_ref"],
                            recipient=actor, head=head, state=HOLDING,
                            granted_from="late_ready", at=now)
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

        It refuses the names this module writes for itself. The ledger converges on conflict,
        so a caller that reached one of those keys first would not overwrite the engine's
        record - it would make the engine's record silently not happen, which is the one
        outcome an append-only ledger must not be able to produce.
        """
        squatted = reserved(evidence_kind, idempotency_key)
        if squatted:
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                squatted + " belongs to the merge turn itself. Attesting is for facts that"
                " reached a turn from outside it, such as a transport accepting a message;"
                " the turn's own transitions, grants and acknowledgements are written by the"
                " operations that cause them")
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

    def acknowledge_grant(self, turn, *, actor, grant, evidence):
        """A parent acting on a grant it read, checked against what is true NOW.

        A grant is not authority carried forward. Between the promotion and the parent reading
        it, the tenure can have closed, the project can have changed hands or been paused, and
        the candidate can have moved. A parent that acted on the payload alone would be acting
        on evidence that expired while it was away, which is the remembered-message failure
        this whole module exists to replace with a record.

        So the caller NAMES the grant it read and it is compared with this tenure's own. Every
        one of those cases is refused here, before the parent spends a merge attempt on it. A
        second acknowledgement of the same grant converges, because a duplicate delivery of one
        notice is one notice.
        """
        if not str(evidence or "").strip():
            raise CoordinationError(
                RefusalReason.MERGE_EVIDENCE_REQUIRED,
                "acknowledging a grant states what was read; a bare acknowledgement is exactly"
                " the remembered message this replaces")
        exact(grant, "a grant id")
        now = self.clock.iso()
        refusal = None
        with self.store.transaction() as db:
            row = self._row_in(db, turn)
            current = self._current_grant_in(db, turn, row["tenure"])
            if row["holder_task_id"] != actor:
                refusal = self._not_holder(row, actor, "acknowledge a grant on")
            elif row["state"] != HOLDING:
                refusal = self._wrong_state(row, actor, "acknowledging a grant")
            elif current is None:
                refusal = Refusal(
                    RefusalReason.MERGE_TURN_NOT_HELD,
                    "turn " + repr(turn) + " records no grant, so there is nothing here to"
                    " acknowledge; a claim made before grants were recorded has none and"
                    " needs none",
                    domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                    incumbent=row["holder_task_id"], challenger=actor)
            elif grant != current:
                refusal = Refusal(
                    RefusalReason.MERGE_TURN_NOT_HELD,
                    "grant " + repr(grant) + " is not the grant for tenure "
                    + str(row["tenure"]) + " of turn " + repr(turn) + ", which is "
                    + repr(current) + "; the grant you read was returned before you acted"
                    " on it",
                    domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
                    incumbent=current, challenger=grant)
            if refusal is None:
                owner, refusal = self._project_owner(
                    db, row["project_key"], row["target_key"], actor)
                if refusal is None and owner != actor:
                    refusal = self._stale_owner(row, owner, actor)
            if refusal is None and self._owner_status(row["project_key"]) != ACTIVE:
                refusal = self._paused(row, actor, "act on a grant")
            if refusal is None:
                self._write_ledger(
                    db, turn, kind=ATTESTATION, from_state=row["state"], to_state=None,
                    evidence_kind=GRANT_ACKNOWLEDGED, actor=actor, evidence=evidence,
                    idempotency_key=GRANT_ACKNOWLEDGED + ":" + current, at=now)
            else:
                self.conflicts.record_in(db, refusal, at=now)
        if refusal is not None:
            raise refusal.error()
        return self.turn(turn)

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

    @staticmethod
    def _paused(row, actor, what):
        """Owning a project and running are two facts, and a pause separates them.

        Not a stale owner and not a stranger: this task IS the parent and keeps the turn. The
        binding says it is not running, and a target held by somebody who is not running is
        the wedge a peer cannot do anything about.
        """
        return Refusal(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            "task " + repr(actor) + " owns project " + repr(row["project_key"]) + " with a"
            " paused binding, so it keeps turn " + repr(row["turn_id"]) + " and cannot "
            + what + " under it; resume the binding, or return the turn so a ready peer can"
            " proceed",
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
                if self._owner_status(candidate["project_key"]) != ACTIVE:
                    # A paused owner keeps its claim and its place, and is not handed a target
                    # it is not running to use. The walk tries the next ready waiter; if every
                    # one of them is paused the target is simply left free, with no grant
                    # written and nothing woken. Nothing promotes on a status change either -
                    # a resumed parent reads its own claims and declares readiness.
                    continue
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
                    idempotency_key="promote:" + str(candidate["tenure"]), at=at)
                self._grant_in(
                    db, turn=candidate["turn_id"], tenure=candidate["tenure"], target=target,
                    repository=candidate["repository"], base_ref=candidate["base_ref"],
                    recipient=owner, head=candidate["candidate_head"], state=HOLDING,
                    granted_from="promotion", at=at,
                    # The one grant its recipient did not ask for. Every other grant answers a
                    # call that parent just made, so it is awake by construction; this one is
                    # handed to a parent whose last act was to wait, and waiting is exactly
                    # what an idle task looks like.
                    relationship_id=candidate["relationship_id"],
                    project_key=candidate["project_key"], wake=True)
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
            if refusal is None and self._owner_status(row["project_key"]) != ACTIVE:
                # The same question the acquisition paths ask, asked again at the write that
                # actually lands work. A binding paused after the turn was granted leaves a
                # parent holding a target it is not running to use.
                refusal = self._paused(row, actor, "begin a merge")
            if refusal is None:
                refusal = self._unanswered_grant(db, row, actor)
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

        The rules themselves live in mergeevidence, because CRW-128 asks the same question on
        the child's side of the handoff: the child establishes that its candidate is green
        before offering it, and this establishes it again before landing. Written twice they
        drift, and the drift is invisible because each side stays green on its own tests. The
        wrapper keeps this method's reason, incumbent and short-circuit exactly as they were.
        """
        problems = mergeevidence.checks_problems(head_sha, required, checks)
        if not problems:
            return None
        # One problem, by that contract: these rules are sequential and stop at the first.
        problem = problems[0]
        return Refusal(
            RefusalReason.MERGE_CURRENCY_STALE, problem.detail,
            domain=DOMAIN_MERGE_TARGET, subject=row["target_key"],
            incumbent=problem.incumbent, challenger=actor)

    @staticmethod
    def _review_refusal(row, actor, review):
        """Every page read, every thread seen, nothing unresolved."""
        problems = mergeevidence.review_problems(review)
        if not problems:
            return None
        return Refusal(
            RefusalReason.MERGE_REVIEW_INCOMPLETE,
            "; ".join(mergeevidence.details(problems)),
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
        # The CURRENT submission of every event only. work_reports retains every submission and
        # generation, so distinct historical heads are ordinary evidence of revision rather than
        # competing candidates; reading them all turned a normal correction into a permanent
        # ambiguity that no candidate could ever pass. Current is decided per event, because
        # submission numbers count per event: one maximum across the generation hid every event
        # not resubmitted as often, and let a resubmitted event's head merge past another event's
        # current report naming a different head. report.current_reports is that selection, the
        # same one the grant notice proposes its required checks from.
        _generation, current = report.current_reports(db, row["relationship_id"])
        heads = sorted({r["head_sha"] for r in current})
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


# Why a queued grant notice is no longer worth sending. Read by the delivery layer, which owns
# no opinion about merge turns and should not acquire one.
MERGE_TURN_ABSENT = "merge_turn_absent"
MERGE_TURN_CLOSED = "merge_turn_closed"
MERGE_TURN_REGRANTED = "merge_turn_regranted"
MERGE_TURN_GRANT_ANSWERED = "merge_turn_grant_answered"
# The notice itself could not be read as one. This store wrote it, so this means the row has
# been damaged or hand-edited; it is named rather than folded into absent, because the repair
# is not the same one.
MERGE_TURN_GRANT_UNREADABLE = "merge_turn_grant_unreadable"


def grant_supersession_in(db, turn, grant):
    """Whether a grant notice still has anything to tell its recipient, read inside a write.

    A grant's currency is its OWN turn. The assignment's execution generation says nothing
    about who may merge into a shared branch, so measuring this notice against it both
    suppressed wakes that were still true and let stale ones through. This is the one place
    that rule lives, so the pre-send check, the re-check inside the atomic claim and the
    operator-facing report cannot disagree about it.

    Four answers, and between them they cover every state a turn can legally be in. A grant
    already acknowledged has been acted on, and stays acknowledged when its turn later closes:
    landing is how an acknowledged grant ordinarily ends, and answering closed for it described
    the success path as a notice something had overtaken. A turn that no longer OCCUPIES its
    target without that has nothing to hand over. A turn whose newest grant is a different one
    has moved to another candidate, and the newer notice is the one worth delivering. And a
    turn this store cannot find is fail-closed on purpose: there is no acknowledgement and no
    return the message could ask for, so sending it would ask a parent to act on something
    nobody can read.

    None means it is still current, which is the only answer that sends anything.
    """
    row = db.execute(
        "SELECT state, tenure FROM merge_turns WHERE turn_id = ?", (turn,)).fetchone()
    if row is None:
        return MERGE_TURN_ABSENT
    answered = db.execute(
        "SELECT 1 FROM merge_turn_ledger WHERE turn_id = ? AND idempotency_key = ?",
        (turn, GRANT_ACKNOWLEDGED + ":" + grant)).fetchone()
    if answered is not None:
        return MERGE_TURN_GRANT_ANSWERED
    if row["state"] not in OCCUPYING:
        return MERGE_TURN_CLOSED
    current = MergeTurn._current_grant_in(db, turn, row["tenure"])
    # A turn with no readable grant at all does not make this one stale. That is a store whose
    # ledger cannot be read as this module's own, and the recogniser already reports it; it is
    # not evidence that some newer grant replaced this notice.
    if current is not None and current != grant:
        return MERGE_TURN_REGRANTED
    return None
