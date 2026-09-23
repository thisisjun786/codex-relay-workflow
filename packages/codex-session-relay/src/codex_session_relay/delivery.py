"""Handing an event to a recipient, exactly once, and only when that is allowed.

There is one path to a transport call and it goes through one atomic claim. The claim decides
authorization, staging and eligibility in a single statement, so a pause committed while we
were reading the host cannot be overtaken by a send that was already half-decided.

Everything before the claim is an optimisation. Everything after it is recorded before its
side effect.
"""

import json

from .errors import DeliveryRefused, RefusalReason, RelayError
from .currency import (
    STALE_GENERATION, SUPERSEDED as SUPERSEDED_REVISION, head_revision,
)
from .identity import (
    merge_turn_grant_event_id as derive_grant_event_id, request_id as derive_request_id,
)
from .lifecycle import UNKNOWN as LIFECYCLE_UNKNOWN, hold_reason_for, observe, record as record_lifecycle
from .policy import RetryPolicy
from .registry import ACTIVE as RELATIONSHIP_ACTIVE
from .scope import assert_assignment_delivery, check_recipient
from .transport import (
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    QUEUED,
    SENDING,
    SUPERSEDED,
    WITHHELD_PRE_SEND,
    assert_attempt_invariants,
    attempt_record,
    classify_operation_receipt,
)
from .policy import PUSH_CHANNEL_CLOSED, SUPERSEDED as SUPERSEDED_HOLD
from . import NO_DELIVERABLE, envelope, restoration, rolepolicy
from .report import (
    compose_revision, read as read_work_report, render_completion, render_revision,
)

COMPLETION = "completion_event"
REVISION = "revision_request"
# The relay telling a PARENT that a shared merge target is now its turn. Relay-owned like a
# revision request, and like one it defines no contract-v1 acknowledgement: it is answered on
# the merge turn itself, which is where the authority to merge lives.
MERGE_TURN_GRANT = "merge_turn_grant"
# Outcomes the relay writes about its OWN coordination rather than about the assignment's
# work. They travel this queue and they are not a fact about the generation they ride in:
# they neither answer a correction nor compete for a revision head, and their currency is
# the subject they are about. Named as a class, because the next one will have the same
# problem and "not reviewable" has already proved to be the wrong way to say this.
RELAY_NOTICE_OUTCOMES = (MERGE_TURN_GRANT,)
# The same set as a SQL fragment, built once so the gates that must exempt these outcomes
# cannot drift apart from the tuple above. Interpolated rather than bound because these are
# module constants this package chooses, never anything a caller supplies.
NOTICE_OUTCOMES_SQL = "(" + ", ".join("'" + name + "'" for name in RELAY_NOTICE_OUTCOMES) + ")"
# What a CHILD reports when a generation ended without something to review. These are facts
# about how the execution finished, not candidates for the generation's revision head, so the
# same-generation head rule does not apply to them. Deliberately a list of outcomes rather
# than "everything that is not reviewable": a revision_request is not reviewable either, and
# it IS answered by the child's reply.
EXECUTION_ONLY_OUTCOMES = ("failed", "interrupted", "blocked_needs_input")
CLAIMABLE = (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND)

# linkage.PROJECT and linkage.ISSUE, spelled here rather than imported. linkage reaches registry
# and assignment from inside its own functions, and delivery is reached from registry, so a
# module-level import would close that ring. Two string literals cannot drift far, and a test
# asserts they still equal the linkage constants.
PROJECT_SCOPE = "project"
ISSUE_SCOPE = "issue"
MANIFEST_LINES = 10
NEWLINE = chr(10)


def _overflow_line(items, event_id, *, shown=MANIFEST_LINES, name_block=True):
    """What the cap removed, said out loud, or None when it removed nothing.

    The deliverables block has always said this and the two findings blocks did not, so a
    correction could lose its eleventh finding - and with it the restoration block a compacted
    child needs in order to resume - leaving nothing at all behind to read. A recipient
    holding nine findings and a recipient whose tenth was cut read the same message.

    The count alone is not enough either, which is why the block is named when it is one of
    the things that went: "5 more" and "5 more, one of which was the restoration block" ask
    the reader for different decisions.
    """
    hidden = list(items)[shown:]
    if not hidden:
        return None
    return (
        f"  ... {len(hidden)} more"
        f"{restoration.overflow_detail(hidden) if name_block else ''}; see"
        f" 'codex-session-relay show --event {event_id}'"
    )


class DeliveryService:
    def __init__(self, store, registry, intake, clock, *, policy=None,
                 require_lifecycle_evidence: bool = True, linkage=None):
        self.store = store
        self.registry = registry
        self.intake = intake
        self.clock = clock
        self.policy = policy or RetryPolicy()
        self.require_lifecycle_evidence = require_lifecycle_evidence
        # Optional on purpose. Every caller written before the three-level linkage existed keeps
        # its exact behaviour when this is absent, including the callers in this package's own
        # tests; supplying it turns recipient resolution from copying a frozen row into verifying
        # the live hierarchy. It is never used to WRITE linkage, only to read it.
        self.linkage = linkage

    # -------------------------------------------------------- relation resolution

    def resolve_recipient(self, relationship, kind):
        """Who this delivery goes to, read from the linkage rather than from the frozen row.

        The relationship row records the parent it was registered under, so a completion that
        arrives after the project changed hands is addressed to the owner that stepped down. The
        linkage knows who owns the scope NOW, and this asks it.

        It verifies rather than overrides. A recipient that disagrees with
        assert_assignment_delivery is not quietly substituted: the disagreement IS the finding,
        and it is refused so that a late report is never filed as the new owner's result.

        The reader's three answers stay three answers. An unreadable store is not "nothing
        found", nothing found is not "complete", and neither becomes a resolved recipient.
        """
        frozen = (relationship["child"]["taskId"] if kind == REVISION
                  else relationship["parent"]["taskId"])
        if self.linkage is None:
            return frozen, {"source": "relationship_row", "verified": False}
        rid = relationship["relationshipId"]
        reading = self.linkage.up(relationship_id=rid)
        if not reading.get("readable", False):
            raise DeliveryRefused(
                RefusalReason.RELATION_UNREADABLE,
                f"the linkage could not be read for relationship {rid!r}, so who owns its scope "
                "is unknown; the relationship row is not used as a fallback because an "
                "unreadable store has said nothing about the owner",
            )
        if reading.get("state") == "ambiguous":
            raise DeliveryRefused(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                f"the linkage reports more than one candidate for relationship {rid!r}; this "
                f"reader will not choose between them: {reading.get('contention')!r}",
            )
        # state becomes ambiguous only for competing owners, so every OTHER inconsistency the
        # walk reports arrives here with a resolved state and would otherwise be delivered
        # through. owner_drift is the one that matters most: the linkage doc says it is reported
        # at both ends and that the handover sequence passes through it on purpose, because each
        # assignment is moved to the incoming parent before the scope is. During that window the
        # project binding can still name the outgoing parent, which is exactly the frozen value
        # this method is trying not to trust - so an owner check alone would agree with the row
        # and deliver to the parent that is stepping down.
        # Only the walk's own LIVE findings, never the retained audit rows. up() folds every
        # linkage_conflicts row for the scope into the same list, and nothing ever deletes those:
        # they exist to remember a refused write. Refusing on them would let one historical
        # rejected mutation block this scope's deliveries permanently. The two are distinguishable
        # in the record rather than by guesswork - a walk finding carries a "contention" key
        # (owner_drift, competing_owners, competing_parents, scope_cycle, instruction_conflict,
        # ambiguous_scope) and an audit row carries "reason" and no "contention".
        contention = [item for item in (reading.get("contention") or [])
                      if item.get("contention")]
        if contention:
            drifting = any(item.get("contention") == "owner_drift" for item in contention)
            raise DeliveryRefused(
                RefusalReason.RELATION_OWNER_DRIFT if drifting
                else RefusalReason.LINK_CONFLICT,
                f"the linkage reports the hierarchy of relationship {rid!r} as inconsistent, so "
                f"who owns its scope is not settled: {contention!r}. A resolved state with "
                "contention is not a resolved owner, and delivery waits for the hierarchy to "
                "settle rather than picking the side that happens to match the frozen row",
            )
        # A revision travels down to the issue's own child, and a completion up to the project
        # that owns the issue. Named explicitly, because a kind that fell through to one of them
        # would resolve a recipient for a direction this contract does not define.
        if kind == REVISION:
            wanted = ISSUE_SCOPE
        elif kind == COMPLETION:
            wanted = PROJECT_SCOPE
        elif kind == MERGE_TURN_GRANT:
            # A merge target is the project's, and so is the parent that lands on it. Same
            # level as a completion, and named here for the same reason the other two are.
            wanted = PROJECT_SCOPE
        else:
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE,
                f"{kind!r} is not a delivery direction, so it has no resolvable recipient",
            )
        level = next(
            (lv for lv in reading.get("levels") or [] if lv.get("scopeKind") == wanted), None
        )
        if level is None or level.get("owner") is None:
            raise DeliveryRefused(
                RefusalReason.UNREGISTERED_SCOPE,
                f"the linkage records no live {wanted} owner for relationship {rid!r}; "
                f"gaps {reading.get('gaps')!r}. Nothing found is reported as nothing found, "
                "never as a delivery that may proceed",
            )
        current = level["owner"]["taskId"] if isinstance(level["owner"], dict) else level["owner"]
        if current != frozen:
            raise DeliveryRefused(
                RefusalReason.RELATION_OWNER_DRIFT,
                f"relationship {rid!r} names {frozen!r} but the linkage says {wanted} "
                f"{level.get('scopeKey')!r} is owned by {current!r}. A report that arrived after "
                "the relationship changed is held rather than credited to either task; "
                "re-register the assignment under the current owner and deliver that",
            )
        return current, {
            "source": "linkage",
            "verified": True,
            "scopeKind": wanted,
            "scopeKey": level.get("scopeKey"),
            "revision": level["owner"].get("revision") if isinstance(level["owner"], dict)
            else None,
        }

    # ---------------------------------------------------------------- queue

    def get(self, event_id: str):
        row = self.store.one("SELECT * FROM deliveries WHERE event_id = ?", (event_id,))
        if row is None:
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE, f"no delivery queued for event {event_id!r}"
            )
        return row

    def find(self, event_id: str):
        return self.store.one("SELECT * FROM deliveries WHERE event_id = ?", (event_id,))

    def enqueue(self, event_id: str, *, kind: str = COMPLETION, recipient_task_id=None) -> dict:
        """Queue an event for its recipient. Idempotent, and only for a final event."""
        event = self.intake.row(event_id)
        if event is None:
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE, f"event {event_id!r} was never accepted"
            )
        if event["stage"] != "final":
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE,
                f"event {event_id!r} is {event['stage']!r}; only a final event may be delivered",
            )
        relationship = self.registry.require_active(event["relationship_id"])
        resolution = None
        if recipient_task_id is None:
            # Resolved through the linkage when one is wired, which verifies the live owner
            # instead of copying the parent the relationship froze at registration.
            recipient_task_id, resolution = self.resolve_recipient(relationship, kind)
        # Refused before any transport call, and re-checked inside attempt(). Checked BEFORE
        # the idempotent early return, so re-enqueueing cannot smuggle a cross delivery past
        # a row that already exists.
        assert_assignment_delivery(
            relationship, kind=kind, recipient_task_id=recipient_task_id,
            event_relationship_id=event["relationship_id"],
            manifest_paths=_manifest_paths(event),
        )
        existing = self.find(event_id)
        if existing is not None:
            # An intent is satisfied the moment the delivery row exists, so a lingering one is
            # cleared on this path too. The requeue pass can arrive here after a crash left the
            # row inserted and the intent behind it.
            self.clear_intent(event_id)
            return dict(existing)
        with self.store.transaction() as db:
            self.enqueue_in(
                db, event_id, relationship_id=event["relationship_id"], kind=kind,
                recipient_task_id=recipient_task_id,
            )
            # In the SAME transaction as the insert. These were two commits, and a crash
            # between them left a delivery_intent row for an event that is already queued.
            # Nothing loses an event and nothing sends twice, but no pass removes the row
            # either - the queries that would find it filter on having no delivery - so they
            # accumulate for as long as the store lives.
            db.execute("DELETE FROM delivery_intent WHERE event_id = ?", (event_id,))
        if resolution is not None and resolution.get("verified"):
            # Recorded after the row exists, as a journal fact rather than a delivery column, so
            # the resolution is auditable without widening a table the frozen contract pins.
            self.store.journal(
                "delivery_recipient_resolved", event_id, resolution, at=self.clock.iso()
            )
        return dict(self.get(event_id))

    def enqueue_in(self, db, event_id, *, relationship_id, kind, recipient_task_id) -> None:
        """Queue inside a caller's transaction, so enqueueing can be atomic with its cause."""
        now = self.clock.iso()
        db.execute(
            "INSERT OR IGNORE INTO deliveries (event_id, relationship_id, kind,"
            " recipient_task_id, recipient_thread_id, state, attempt_count, next_eligible_at,"
            " created_at, updated_at) VALUES (?,?,?,?,?,?,0,NULL,?,?)",
            (
                event_id, relationship_id, kind, recipient_task_id, recipient_task_id,
                QUEUED, now, now,
            ),
        )
        self.store.journal("delivery_queued", event_id, {"kind": kind}, at=now)
        # Every route by which an event becomes deliverable passes through here, including a
        # receipt the host already reports as terminal, which never touches the settlement
        # path. So this is where a newly deliverable event announces what it replaces.
        self.annotate_predecessors_in(db, event_id)

    def grant_channel_in(self, db, *, relationship_id, recipient_task_id, grant,
                         project_key=None):
        """Whether this grant has an authorized way to reach its recipient, and by which event.

        Decided BEFORE anything is written, and decided by reading rather than by trying: the
        caller is a promotion inside land, release or resolve_unknown, and a notice that cannot
        be addressed must never take a confirmed landing down with it.

        The channel is the turn's OWN assignment and nothing else. It is tempting to reach for
        any assignment the same parent holds in the same project, and that is a cross delivery:
        assert_assignment_delivery exists to refuse exactly that, because an event belongs to
        one assignment and travels to that assignment's own endpoint. A merge turn that names
        no assignment therefore has no address in this queue, which is recorded rather than
        worked around.

        All four conditions are checked here, including the recipient list, which is the one
        that fails late: registration only requires the list to be non-empty, so a parent
        missing from its own assignment's recipients would be refused at every send attempt
        forever instead of once, here, with a reason.

        The project is checked for a different reason: not authorization, but actionability.
        mergeturn._relationship_refusal already refuses to MERGE a turn whose assignment is
        attached to another project, so a wake pushed through that assignment would file a
        notice about this target in another project's ledger and invite the recipient to do
        something begin_merge will then refuse. A relationship with no recorded attachment is
        left alone, which is the same tolerance the merge-side rule has.
        """
        if not relationship_id:
            return {"refused": "the turn names no assignment"}
        row = db.execute(
            "SELECT parent_task_id, status, superseded_by, allowed_recipients FROM"
            " relationships WHERE relationship_id = ?", (relationship_id,)).fetchone()
        if row is None:
            return {"refused": f"assignment {relationship_id!r} is not in this store"}
        if row["status"] != RELATIONSHIP_ACTIVE or row["superseded_by"] is not None:
            return {"refused": f"assignment {relationship_id!r} is not active"}
        if row["parent_task_id"] != recipient_task_id:
            return {"refused": f"assignment {relationship_id!r} is addressed to parent "
                               f"{row['parent_task_id']!r}, not to {recipient_task_id!r}"}
        scope = db.execute(
            "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
            (relationship_id,)).fetchone()
        if project_key and scope is not None and scope["project_key"] != project_key:
            return {"refused": f"assignment {relationship_id!r} is attached to project "
                               f"{scope['project_key']!r}, not to this turn's "
                               f"{project_key!r}"}
        try:
            allowed = json.loads(row["allowed_recipients"])
        except (TypeError, ValueError):
            allowed = []
        if recipient_task_id not in set(allowed or ()):
            return {"refused": f"{recipient_task_id!r} is not in the recipients assignment "
                               f"{relationship_id!r} authorizes"}
        return {"eventId": derive_grant_event_id(relationship_id, grant),
                "relationshipId": relationship_id}

    def queue_grant_in(self, db, *, event_id, relationship_id, recipient_task_id, receipt,
                       grant, at) -> None:
        """The one durable event a granted turn is delivered through, queued with its cause.

        The same shape ack.py's relay-owned revision takes, for the same reason: _claim will
        not claim anything without an events row at stage final, and one atomic claim is the
        only path to a transport call. So riding the existing queue means creating the event,
        not adding a second way to send.

        The generation it carries is the assignment's CURRENT one, which is what _claim's
        predicate compares against - and a grant is exempt from that comparison, because a
        merge target's turn has nothing to do with how many times a child has revised. The
        column is still filled honestly rather than with a placeholder nobody could interpret.

        Both writes converge: a promotion replayed against the same store finds the same event
        id derived from the same grant and inserts nothing twice.
        """
        generation = db.execute(
            "SELECT execution_generation FROM relationships WHERE relationship_id = ?",
            (relationship_id,)).fetchone()["execution_generation"]
        db.execute(
            "INSERT OR IGNORE INTO events (event_id, relationship_id, execution_generation,"
            " revision_hash, outcome, producer, attempt, turn_thread_id, turn_id, turn_status,"
            " receipt, stage, first_seen_at, last_seen_at, observation_count)"
            " VALUES (?,?,?,?,?,?,NULL,?,?,?,?, 'final', ?,?,1)",
            # No host turn produced this event: a promotion is this package's own write, made
            # while the parent it names may not be running at all. The grant is what produced
            # it, so the grant is what the turn columns say, rather than a borrowed turn id
            # belonging to somebody else's work.
            (event_id, relationship_id, generation, NO_DELIVERABLE, MERGE_TURN_GRANT, "relay",
             recipient_task_id, grant, "completed", receipt, at, at),
        )
        self.enqueue_in(
            db, event_id, relationship_id=relationship_id, kind=MERGE_TURN_GRANT,
            recipient_task_id=recipient_task_id,
        )

    def record_intent_in(self, db, event_id, *, relationship_id, kind, recipient_task_id,
                         error, now) -> None:
        """Remember that delivery was wanted here and refused for a reason that may not last.

        Written in the SAME transaction as the observation that produced the event, so the two
        facts cannot disagree. Deriving this instead - final event, no delivery row - would
        also match an event emitted with --no-enqueue and one stranded by an old generation,
        neither of which anyone asked to send.

        Each refusal backs the retry off, so a permanently unqueueable event cannot hold a
        recovery slot against events that would succeed.
        """
        row = db.execute(
            "SELECT attempts FROM delivery_intent WHERE event_id = ?", (event_id,),
        ).fetchone()
        attempts = (row["attempts"] if row else 0) + 1
        delay = self._backoff(attempts)
        db.execute(
            "INSERT INTO delivery_intent (event_id, relationship_id, kind, recipient_task_id,"
            " attempts, next_retry_at, last_error, noted_at) VALUES (?,?,?,?,?,?,?,?)"
            " ON CONFLICT(event_id) DO UPDATE SET attempts = excluded.attempts,"
            " next_retry_at = excluded.next_retry_at, last_error = excluded.last_error",
            (event_id, relationship_id, kind, recipient_task_id, attempts, now + delay,
             str(error), self.clock.iso()),
        )

    def _backoff(self, attempts: int) -> float:
        """Bounded before the exponent is evaluated, not after.

        An intent that stays legitimately unqueueable - a relationship that stays paused -
        has no cap on its attempt count. Computing base * 2 ** (attempts - 1) and then
        clamping means the 1025th refusal builds an integer too large to convert to a float,
        and the OverflowError escapes the refusal handler that was meant to absorb it. The
        transaction rolls back with the intent still due, so every later tick fails the same
        way. Once the ceiling is reached the exponent stops mattering, so stop there.
        """
        base, ceiling = self.policy.presend_base_seconds, self.policy.presend_max_seconds
        if base <= 0:
            return ceiling
        steps = max(0, attempts - 1)
        # Derived from this policy's own ratio, not a fixed step count: a small base needs
        # more doublings to reach its ceiling, and a constant would send such a policy
        # straight to the cap while its backoff still had room.
        import math

        if ceiling <= base or steps >= math.ceil(math.log2(ceiling / base)):
            return ceiling
        return min(ceiling, base * (2 ** steps))

    def pending_intents(self, *, now: float, limit: int = 4) -> list:
        """Events whose delivery was wanted, refused, and is due to be tried again."""
        return self.store.all(
            "SELECT i.* FROM delivery_intent i"
            "  LEFT JOIN deliveries d ON d.event_id = i.event_id"
            " WHERE d.event_id IS NULL"
            "   AND (i.next_retry_at IS NULL OR i.next_retry_at <= ?)"
            " ORDER BY i.next_retry_at LIMIT ?",
            (now, limit),
        )

    def clear_intent(self, event_id: str) -> None:
        with self.store.transaction() as db:
            db.execute("DELETE FROM delivery_intent WHERE event_id = ?", (event_id,))

    def _render_for(self, row, record, request, report=None) -> str:
        """Deterministic, directional, and carrying no turn id belonging to the recipient.

        The two directions are answered differently and must therefore be INSTRUCTED
        differently. A completion goes to the parent, which acknowledges with a proof over its
        own turn. A revision goes to the child, which has no acknowledgement to give: contract
        v1 defines none for that direction, and the acknowledgement path refuses it by kind. A
        revision is answered by the child's next completion receipt under the new generation.
        Telling the child otherwise would be telling it to do something that cannot succeed.

        The recipient's own turn id never appears here, because that absence is what makes an
        acknowledgement unforgeable. Determinism matters too: the transport fingerprints one
        request id against its arguments.

        Pure by construction: every input is passed in, so this cannot read a counter that has
        moved since the attempt it is rendering for.

        The work report is passed in for the same reason. Whoever owns the transaction reads
        it once and hands it over, so this stays a function of its arguments.
        """
        if row["kind"] == REVISION:
            return self._render_revision(row, record, request, report)
        if row["kind"] == MERGE_TURN_GRANT:
            return self._render_grant(row, record, request)
        return self._render_completion(row, record, request, report)

    def envelope_context(self, row) -> dict:
        """The two envelope fields this layer can read and the renderer cannot.

        Who is sending lives on the relationship and which Linear project this belongs to
        lives on the scope row, and report.py holds neither. Read here, once, from the same
        store the delivery is already using.

        An ABSENCE degrades to an empty context rather than to an exception. This runs inside
        the claim transaction, and a message that cannot name its sender is still a message
        worth sending; the envelope prints unknown for what was not read, which is the honest
        answer for a relationship this store does not hold.

        A FAULT is not an absence and is not caught. Converting every exception here meant a
        programming error or a broken database produced a message that looked fine and carried
        unknown where a real value belonged, with nothing anywhere saying why - the failure
        this package refuses everywhere else. So only the refusal taxonomy is caught, and a
        scope lookup that cannot answer a primary-key read inside an open transaction is left
        to travel: the absent-row case is the None below, not an exception.
        """
        try:
            relationship = self.registry.get(row["relationship_id"])
        except RelayError:
            return {}
        sender = (relationship["parent"] if row["kind"] == REVISION
                  else relationship["child"]).get("taskId")
        issue = relationship.get("issueKey")
        found = self.store.one(
            "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
            (row["relationship_id"],),
        )
        project = found["project_key"] if found is not None else None
        if project and issue:
            scope = f"project {project}, issue {issue}"
        elif issue:
            scope = (f"issue {issue}; no project scope is recorded for this relationship")
        else:
            scope = None
        return {"senderTaskId": sender,
                "scope": scope or envelope.absent(
                    envelope.UNKNOWN, "neither a project nor an issue scope was readable")}

    def supervisor_selection(self, event_id: str, *, recipient=None, now=None) -> dict:
        """Whether this event is news for the level above, answered from the rows here.

        A read. It sends nothing, queues nothing and records nothing, and it exists on this
        class because this is where the event, its report and its delivery already are.
        Producing the report, and recording that one was produced, belong to whoever owns the
        turn that does it.

        Most events answer no, and that is the point: an ordinary child progressing, a CI run
        changing and an acknowledgement arriving are all real state changes that the level
        above does not need a turn for.
        """
        from . import supervision

        obligation = supervision.from_event(
            self.store, event_id, read_work_report(self.store, event_id))
        if obligation is None:
            return supervision.suppressed(
                event_id,
                "this event is not a completion, a new block or a decision the user owes")
        # The clock is this service's own. Without it every selection here answered
        # contactability unmeasured, whatever the host had actually been observed to be, and
        # the reportable branch was unreachable for every caller of this method.
        return {**supervision.select(self.store, obligation, recipient=recipient,
                                     now=self.clock.now() if now is None else now),
                "obligation": obligation}

    def _render_and_account(self, row, record, request, report=None):
        """The bytes, and what became of a declared restoration block in exactly those bytes.

        Accounted from the SAME rendering the recipient gets rather than recomputed beside it,
        so the two cannot disagree, and rendered once rather than twice because this runs
        inside the claim transaction, where a second pass holds the single writer for no
        reason. The legacy renderer's rule IS its cap, which is arithmetic over the finding
        list; the composed one reports which findings its own shortening left standing.

        None means there is nothing to account for: a completion carries no correction, and a
        correction that declared no block has no claim to check.
        """
        if row["kind"] != REVISION:
            # Through _render_for rather than straight to the completion renderer. There are
            # three directions now, and "not a revision" stopped meaning "a completion" the
            # moment a third one existed - a grant rendered as a verification request would
            # have told a parent to acknowledge an event with no receipt behind it.
            return self._render_for(row, record, request, report), None
        findings = record.get("criteria") or []
        declared = restoration.declared(findings) is not None
        if report is not None:
            composed = compose_revision(row, record, request, report,
                                        context=self.envelope_context(row))
            return composed.text, (
                restoration.project_survivors(findings, composed.survivors)
                if declared else None
            )
        return self._render_revision(row, record, request, None), (
            restoration.project_cap(findings, cap=MANIFEST_LINES) if declared else None
        )

    def preview_message(self, event_id: str) -> str:
        """What the NEXT attempt would say. Never evidence of what any attempt DID say.

        Deriving historical bytes from attempt_count + 1 is how the inspection surface came to
        report the next request id as though it were the one already sent. A preview is
        therefore labelled a preview everywhere it is returned, and the sent bytes come from
        the attempt that was actually claimed.
        """
        row = self.get(event_id)
        record = self.intake.get(event_id) or {}
        request = derive_request_id(event_id, row["attempt_count"] + 1)
        return self._render_for(row, record, request, read_work_report(self.store, event_id))

    def render_message(self, event_id: str) -> str:
        """Kept as the preview alias so no caller can mean 'what was sent' by accident."""
        return self.preview_message(event_id)

    def _render_completion(self, row, record, request, report=None) -> str:
        # A report centred on a pull request needs a pull request. An event recorded before
        # this contract has none, so it renders what it has always rendered rather than being
        # dressed in a shape its own data cannot fill.
        if report is not None:
            return render_completion(row, record, request, report,
                                     context=self.envelope_context(row))
        lines = [
            "[codex-session-relay] verification request",
            f"requestId: {request}",
            f"eventId: {row['event_id']}",
            f"relationshipId: {row['relationship_id']}",
            f"executionGeneration: {record.get('executionGeneration')}",
            f"attempt: {record.get('attempt')}",
            f"outcome: {record.get('outcome')}",
            f"revisionHash: {record.get('revisionHash')}",
        ]
        manifest = record.get("manifest")
        if manifest:
            lines.append(f"deliverables: {len(manifest)}")
            for entry in manifest[:MANIFEST_LINES]:
                size = entry.get("bytes")
                lines.append(
                    f"  {entry['path']}  sha256={entry['sha256']}"
                    + (f"  bytes={size}" if size is not None else "")
                )
            overflow = _overflow_line(manifest, row["event_id"], name_block=False)
            if overflow:
                lines.append(overflow)
        else:
            lines.append("deliverables: none (execution-only outcome)")
        if record.get("manifestRef"):
            lines.append(f"manifestRef: {record['manifestRef']}")
        criteria = record.get("criteria")
        if criteria:
            lines.append("criteria claimed by the child:")
            for item in criteria[:MANIFEST_LINES]:
                # No restoration label on this direction. These are the CHILD's claims about
                # its own work, they never pass through normalise_findings, and the completion
                # receipt's schema lets an item carry any extra property - so a stray truthy
                # "restoration" would print an official-looking marker on a message that
                # carries no correction at all, contradicting the accounting one method below.
                lines.append(f"  {item.get('id')}: {item.get('verdict')}")
            overflow = _overflow_line(criteria, row["event_id"], name_block=False)
            if overflow:
                lines.append(overflow)
        lines += [
            "",
            "To respond, from inside your own turn:",
            f"  claim     --event {row['event_id']} --turn <your turn id>",
            f"  ack-proof --event {row['event_id']} --turn <your turn id>",
            f"  ack       --event {row['event_id']} --ack-turn <your turn id> --ack-proof <proof>",
            f"  verdict   --event {row['event_id']} --verdict <verified|needs_changes|"
            "unverified|aborted> --verdict-turn <your turn id>",
            "",
            "The proof is sha256(eventId|<your own turn id>). This message does not and cannot",
            "contain that turn id, which is what distinguishes acknowledging from echoing.",
            f"Full record: codex-session-relay show --event {row['event_id']}",
        ]
        return NEWLINE.join(lines)

    def _render_revision(self, row, record, request, report=None) -> str:
        if report is not None:
            return render_revision(row, record, request, report,
                                   context=self.envelope_context(row))
        lines = [
            "[codex-session-relay] revision request",
            f"requestId: {request}",
            f"eventId: {row['event_id']}",
            f"relationshipId: {row['relationship_id']}",
            f"executionGeneration: {record.get('executionGeneration')}  (new)",
            f"supersedesEvent: {record.get('supersedesEvent')}",
            f"supersedesRevisionHash: {record.get('supersedesRevisionHash')}",
            f"verdict: {record.get('verdict')}",
        ]
        findings = record.get("criteria") or []
        if findings:
            lines.append("what to change:")
            for item in findings[:MANIFEST_LINES]:
                note = item.get("note")
                lines.append(
                    f"  {item.get('id')}{restoration.label(item)}: {item.get('verdict')}"
                    + (f" — {note}" if note else "")
                )
            overflow = _overflow_line(findings, row["event_id"])
            if overflow:
                lines.append(overflow)
        else:
            lines.append("what to change: no per-criterion findings were recorded")
        lines += [
            "",
            "There is nothing to acknowledge. Contract v1 defines no acknowledgement for this",
            "direction and the relay refuses one by kind, so there is no proof to compute and",
            "no acknowledgement to send.",
            "Answer with your next completion receipt under the new generation:",
            f"  emit --relationship {row['relationship_id']}"
            f" --generation {record.get('executionGeneration')} --attempt <n>",
            "       --outcome ready_for_review --turn-thread <your task id> --turn-id <your turn>",
            "       --artifact <path> [--continues-anchor <this generation dispatch turn>]",
            "",
            f"Full record: codex-session-relay show --event {row['event_id']}",
        ]
        return NEWLINE.join(lines)

    def _render_grant(self, row, record, request, report=None) -> str:
        """The merge target is this parent's turn, and what it can actually do about it.

        Instructed differently from both other directions, because what the recipient owes is
        different. A completion asks the parent for a verdict over its own turn. A revision
        asks the child for new work. This asks the parent to re-check the candidate against
        THIS store and then merge it, and the acknowledgement it names is the merge turn's
        own - begin_merge refuses a candidate whose grant was never answered, so a parent that
        skipped it would be stopped at the write that lands work with no idea why.

        No contract-v1 acknowledgement is offered, for the same reason the revision direction
        offers none: AckService refuses this kind, so telling a recipient to compute a proof
        would be telling it to do something that cannot succeed.
        """
        lines = [
            "[codex-session-relay] merge turn granted",
            f"requestId: {request}",
            f"eventId: {row['event_id']}",
            f"grantId: {record.get('grantId')}",
            f"turnId: {record.get('turnId')}",
            f"target: {record.get('repository')} {record.get('baseRef')}",
            f"candidateHead: {record.get('candidateHead')}",
            f"grantedFrom: {record.get('grantedFrom')}",
            "",
            "The target was released and this claim was the oldest ready one that still owns",
            "its project, so the turn is yours. Nothing here expires: the target stays yours",
            "until you land it or give it back.",
            "",
            "To act on it, from inside your own turn:",
            f"  merge-turn-acknowledge --turn {record.get('turnId')}"
            f" --grant {record.get('grantId')} --actor <your task id> --evidence <what you read>",
            f"  merge-turn-check --turn {record.get('turnId')} --actor <your task id>"
            " --head-sha <head> --base-sha <base> --checks <json> --review <json>",
            f"  merge-turn-land --turn {record.get('turnId')} --actor <your task id>"
            " --landed-sha <sha> --observed-base-sha <sha> --evidence <what you observed>",
            "",
            "Or hand it on without merging:",
            f"  merge-turn-release --turn {record.get('turnId')} --actor <your task id>"
            " --disposition returned --reason <why>",
            "",
            "There is nothing to acknowledge on this message itself. Contract v1 defines no",
            "acknowledgement for this direction and the relay refuses one by kind; the",
            "acknowledgement above is the merge turn's, and merging without it is refused.",
            f"Full record: codex-session-relay merge-turn-show --turn {record.get('turnId')}",
        ]
        return NEWLINE.join(lines)

    ELIGIBLE_WHERE = (
        " WHERE d.state IN (?,?,?) AND d.hold_reason IS NULL"
        "   AND (d.next_eligible_at IS NULL OR d.next_eligible_at <= ?)"
        "   AND r.status = 'active' AND r.superseded_by IS NULL AND e.stage = 'final'"
    )

    def eligible_parents(self, *, now: float) -> list:
        """Who has anything to send, decided independently of how much each of them has.

        This is the half that removes starvation. A single ORDER BY created_at LIMIT is a
        global prefix: a parent with forty thousand older rows fills it by itself and a parent
        with one newer row is never seen. Asking which PARENTS are eligible cannot be crowded
        out by row counts.
        """
        rows = self.store.all(
            "SELECT DISTINCT r.parent_task_id AS parent_task_id FROM deliveries d"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            " JOIN events e ON e.event_id = d.event_id"
            + self.ELIGIBLE_WHERE +
            " ORDER BY r.parent_task_id",
            (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now),
        )
        return [row["parent_task_id"] for row in rows]

    def eligible_for_parent(self, parent: str, *, now: float, limit: int, offset: int = 0):
        return self.store.all(
            "SELECT d.*, r.parent_task_id AS parent_task_id FROM deliveries d"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            " JOIN events e ON e.event_id = d.event_id"
            + self.ELIGIBLE_WHERE +
            "   AND r.parent_task_id = ?"
            " ORDER BY d.created_at LIMIT ? OFFSET ?",
            (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now, parent, limit, offset),
        )

    def eligible_count(self, parent: str, *, now: float) -> int:
        """How many eligible deliveries one parent has, so a cursor over them can wrap."""
        row = self.store.one(
            "SELECT COUNT(*) AS c FROM deliveries d"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            " JOIN events e ON e.event_id = d.event_id"
            + self.ELIGIBLE_WHERE +
            "   AND r.parent_task_id = ?",
            (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now, parent),
        )
        return row["c"] if row else 0

    def eligible(self, *, now: float, limit: int = 10, per_parent_limit=None, cursor: int = 0,
                 offsets=None) -> list:
        """A fair slice: every eligible parent, then a bounded share each, dealt one at a time.

        Dealt singly rather than in contiguous blocks, because a block allocation leaves the
        last parent short whenever the budget is not a multiple of the share.
        """
        parents = self.eligible_parents(now=now)
        if not parents:
            return []
        share = per_parent_limit or self.policy.max_sends_per_parent_per_tick
        start = cursor % len(parents)
        order = parents[start:] + parents[:start]
        queues = {
            parent: self._window_for(
                parent, now=now, share=share, offset=(offsets or {}).get(parent, 0),
            )
            for parent in order
        }
        selected = []
        while len(selected) < limit and any(queues[parent] for parent in order):
            for parent in order:
                if len(selected) >= limit:
                    break
                if queues[parent]:
                    selected.append(queues[parent].pop(0))
        return selected

    def _window_for(self, parent, *, now, share, offset):
        """One parent's share, taken from a rotating position and wrapped at the end.

        Without the wrap a cursor near the end of a parent's backlog returns a short window -
        five rows at offset four yields one - so the rotation that exists to stop starvation
        would quietly cost throughput every time it came round.
        """
        taken = list(self.eligible_for_parent(parent, now=now, limit=share, offset=offset))
        if len(taken) < share and offset:
            seen = {row["event_id"] for row in taken}
            for row in self.eligible_for_parent(parent, now=now, limit=share, offset=0):
                if len(taken) >= share:
                    break
                if row["event_id"] not in seen:
                    taken.append(row)
        return taken

    # ---------------------------------------------------------------- claim

    def _claim(self, event_id: str, *, now: float, owner: str, recipient: str):
        """Authorization, staging and eligibility decided in one atomic statement.

        A rowcount of 0 means not claimable, for any reason, and the caller does nothing. The
        attempt row is inserted in the SAME transaction, so there is no window in which a
        delivery is claimed but has no attempt to recover.

        The message is rendered HERE, against the attempt number this transaction just
        allocated, and persisted beside it. Rendering before the claim read a counter another
        caller could still move: a slow caller settling a busy attempt between our render and
        our claim made us send request -a2 carrying a message that said -a1, and lost-response
        reconciliation searches the recipient for the token the message actually carried.
        Allocation, token and bytes now commit together or not at all.
        """
        # Decided and recorded BEFORE the claim, in its own committed transaction: a
        # suppression written inside the claim would be rolled back by the refusal that
        # follows it. The claim below then refuses a stale generation atomically anyway,
        # so the gap between the two cannot let one through.
        superseded = self._suppress_if_superseded(event_id)
        if superseded:
            raise _Superseded(superseded)
        with self.store.transaction() as db:
            # Re-read inside the write. The generation predicate below catches an
            # advanced generation, but a newer FINAL revision of the SAME generation
            # can be committed between the check above and this statement, and that
            # would claim and send an event something had already replaced.
            late = self._supersession_reason(db, event_id)
            if late:
                raise _LateSupersession(late)
            cursor = db.execute(
                "UPDATE deliveries"
                "   SET state = ?, lease_owner = ?, lease_until = ?,"
                "       attempt_count = attempt_count + 1, updated_at = ?"
                " WHERE event_id = ?"
                "   AND state IN (?,?,?)"
                "   AND hold_reason IS NULL"
                "   AND (next_eligible_at IS NULL OR next_eligible_at <= ?)"
                "   AND EXISTS (SELECT 1 FROM relationships r"
                "                WHERE r.relationship_id = deliveries.relationship_id"
                "                  AND r.status = 'active' AND r.superseded_by IS NULL)"
                "   AND EXISTS (SELECT 1 FROM events e"
                "                WHERE e.event_id = deliveries.event_id AND e.stage = 'final')"
                # A generation that has moved on cannot be claimed at all. Checked here
                # rather than only before, because the generation can advance while the
                # host reads are in flight.
                "   AND NOT EXISTS (SELECT 1 FROM events ev"
                "                    JOIN relationships rr"
                "                      ON rr.relationship_id = ev.relationship_id"
                "                   WHERE ev.event_id = deliveries.event_id"
                # A relay coordination notice is exempt. A merge turn's currency is the turn,
                # and a child revising its work neither gives nor takes the right to land on a
                # shared branch. Without this a wake written for a parent that then opened a
                # revision was suppressed for good, because a promotion does not happen twice
                # for a parent that already holds the target.
                "                     AND ev.outcome NOT IN " + NOTICE_OUTCOMES_SQL +
                "                     AND ev.execution_generation < rr.execution_generation)",
                (
                    SENDING, owner, now + self.policy.lease_seconds, self.clock.iso(),
                    event_id, QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now,
                ),
            )
            if cursor.rowcount != 1:
                raise _NotClaimable()
            row = db.execute(
                "SELECT * FROM deliveries WHERE event_id = ?", (event_id,)
            ).fetchone()
            attempt_no = row["attempt_count"]
            request_id = derive_request_id(event_id, attempt_no)
            clash = db.execute(
                "SELECT event_id FROM attempts WHERE request_id = ?", (request_id,)
            ).fetchone()
            if clash is not None and clash["event_id"] != event_id:
                raise DeliveryRefused(
                    RefusalReason.NOT_CLAIMABLE,
                    f"request id {request_id!r} already belongs to event {clash['event_id']!r}",
                )
            record = self.intake.get(event_id) or {}
            report = read_work_report(self.store, event_id)
            message, carried = self._render_and_account(row, record, request_id, report)
            db.execute(
                "INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state,"
                " state, sent_at, observed_at) VALUES (?,?,?,?,?,?,?,?)",
                (
                    request_id, event_id, attempt_no, row["kind"], "in_flight",
                    HELD_UNCERTAIN, self.clock.iso(), self.clock.iso(),
                ),
            )
            db.execute(
                "INSERT INTO attempt_messages (request_id, event_id, attempt_no, kind,"
                " message, rendered_at) VALUES (?,?,?,?,?,?)",
                (request_id, event_id, attempt_no, row["kind"], message, self.clock.iso()),
            )
            if carried is not None:
                # Against the bytes this attempt actually froze, in the same transaction that
                # froze them. Everything measured earlier describes the attempt that was next
                # AT THAT TIME: a retry-safe attempt that never sent leaves the following
                # render one request-id digit longer, and at a byte boundary that is the
                # difference between carrying the block and dropping it. So the earlier
                # measurements stay what they are, preflight, and this is the one that
                # settles what an attempt carried.
                carried["attempt"] = attempt_no
                self.store.journal(
                    "restoration_attempted", event_id, carried, at=self.clock.iso(),
                )
            if report is not None:
                # Which submission these bytes came from. The message says so for its reader;
                # this is the same fact in a form the relay can compare against, so a
                # submission that has never been frozen stays correctable in place.
                db.execute(
                    "INSERT OR REPLACE INTO attempt_report_submissions (request_id,"
                    " event_id, submission_no, frozen_at) VALUES (?,?,?,?)",
                    (request_id, event_id, report["submissionNo"], self.clock.iso()),
                )
            # Capacity is reserved in the SAME transaction as the claim. Counting after the
            # send let two interleaved callers both pass a cap of one. The gap between sends
            # is decided here too, by the one predicate the supervisor channel's claim also
            # calls: the two share this recipient's budget, and a bound only one of its
            # writers re-checks inside its write is a bound the other one does not obey.
            if reserve_send(db, self.policy, recipient, now) is not None:
                raise _Paced()
        return attempt_no, request_id, message

    # -------------------------------------------------------------- attempt

    def attempt(self, event_id: str, adapter, *, now=None, owner: str = "relay"):
        now = self.clock.now() if now is None else now
        row = self.get(event_id)
        relationship = self.registry.get(row["relationship_id"])
        recipient = row["recipient_task_id"]

        if row["hold_reason"]:
            return None
        if row["state"] not in CLAIMABLE:
            return None
        if relationship["status"] != RELATIONSHIP_ACTIVE and not relationship["supersededBy"]:
            # Decided before any host read. The claim already refuses this atomically, so
            # nothing was ever sent either way; what was missing is the reason. Reading the
            # host here would also observe a task on behalf of an assignment somebody stopped,
            # and leave a recipient_lifecycle row calling that recipient deliverable.
            #
            # Ahead of the next_eligible_at guard on purpose. A delivery can be eligible by
            # state but still waiting out a backoff, and returning None for that reason would
            # hide a deactivation that has already happened: an operator naming the event
            # during the wait would learn nothing until the timer expired.
            #
            # Superseded relationships are deliberately NOT routed here. They are deactivated
            # too, but permanently - resume() refuses them - so they belong to the terminal
            # supersession vocabulary rather than to a status waiting to be lifted, and
            # closing them correctly means settling how every operator-facing reader renders
            # that terminal. They keep their existing behaviour until that is decided.
            return self._withhold_inactive(
                event_id, relationship, now, attempts=row["attempt_count"],
            )
        if row["next_eligible_at"] is not None and row["next_eligible_at"] > now:
            return None
        assert_assignment_delivery(
            relationship, kind=row["kind"], recipient_task_id=recipient,
            recipient_thread_id=row["recipient_thread_id"],
            # From the EVENT, not the delivery row. Comparing the delivery row against itself
            # is a tautology and would pass a row pointed at another assignment.
            event_relationship_id=(self.intake.row(event_id) or {})["relationship_id"],
            manifest_paths=_manifest_paths(self.intake.row(event_id)),
        )
        if self._rate_limited(recipient, now):
            self._reschedule(
                event_id, row["state"], now + self.policy.min_send_interval_seconds,
                attempts=row["attempt_count"],
            )
            return None

        observation = observe(
            adapter, row["recipient_thread_id"],
            # Both parent-directed kinds. The cwd is the RECIPIENT's, and a grant's recipient
            # is this assignment's parent exactly as a completion's is; reading it as "only a
            # completion" would skip the workspace check for one of the two.
            cwd=(relationship["parent"].get("cwd")
                 if row["kind"] in (COMPLETION, MERGE_TURN_GRANT) else None),
            require_evidence=self.require_lifecycle_evidence,
        )
        record_lifecycle(self.store, self.clock, observation)
        if observation.is_busy:
            # An active recipient is left strictly alone. No resume, no interruption, and no
            # attempt record, because no transport receipt exists to classify.
            self._defer_busy(event_id, row, now)
            return None
        if not observation.may_send:
            self._withhold(event_id, observation, now, attempts=row["attempt_count"])
            return None

        # The authorized settings are established BEFORE anything is claimed or sent. A send
        # that cannot say what it is preserving does not get to find that out from the host: the
        # pinned bridge's own resume carries no overrides, and on this host that returned
        # dangerFullAccess for a workspaceWrite task.
        try:
            settings = self._settings_for(recipient, observation.runtime_status)
        except DeliveryRefused as refusal:
            self._withhold_settings(event_id, now, refusal, attempts=row["attempt_count"],
                                    row=row)
            return None

        known_turns = set(adapter.list_turn_ids(row["recipient_thread_id"], limit=25))
        try:
            attempt_no, request_id, message = self._claim(
                event_id, now=now, owner=owner, recipient=recipient
            )
        except _Paced:
            # Refused on the shared budget INSIDE the claim: another sender woke this recipient
            # after the preflight read. The claim rolled back, so this is the preflight's answer
            # arriving late, and it gets the preflight's treatment - deferred by the same gap,
            # never recorded as a failure and never held.
            self._reschedule(
                event_id, row["state"], now + self.policy.min_send_interval_seconds,
                attempts=row["attempt_count"],
            )
            return None
        except _NotClaimable:
            return None
        except _LateSupersession as late:
            # The transaction rolled back, so nothing is recorded yet. Record it now,
            # through the same path the pre-claim check uses, and report it identically.
            self._suppress_if_superseded(event_id)
            return {"deliveryState": SUPERSEDED, "supersededReason": late.reason,
                    "eventId": event_id, "sendAttempted": "no"}
        except _Superseded as superseded:
            # No transport call at all: suppression writes state and journals and nothing
            # else, so a stale event cannot wake the parent or open a generation.
            return {"deliveryState": SUPERSEDED, "supersededReason": superseded.reason,
                    "eventId": event_id, "sendAttempted": "no"}

        try:
            receipt = adapter.send_message(
                request_id, row["recipient_thread_id"], message, settings,
            )
        except Exception as error:
            receipt = {
                "requestId": request_id,
                "status": "outcome_unknown",
                "error": f"{type(error).__name__}: {error}",
            }
        facts = classify_operation_receipt(receipt)
        # Read from the RAW receipt: the host reports a settings rejection as a failed
        # receipt carrying settingsFindings, and classification keeps only the code. By the
        # time _settle sees it the field-level difference is already gone.
        findings = receipt.get("settingsFindings") if isinstance(receipt, dict) else None
        if facts.failed_operation or facts.delivery_state in (
            WITHHELD_PRE_SEND, INBOX_ONLY, HELD_UNCERTAIN,
        ):
            self.record_failure(
                event_id,
                "settings_check" if findings else (facts.failed_operation or "transport"),
                detail=facts.error_text or facts.delivery_state,
                relationship_id=row["relationship_id"],
                parent_task_id=relationship["parent"]["taskId"],
                error_code=facts.rpc_error_code,
                difference=_render_findings(findings),
                retry_safe=facts.retry_safe,
            )
        # Membership in the pre-send snapshot proves this start steered a turn we had already
        # seen. Absence proves nothing: another client can open a turn after the snapshot, and
        # our start can then steer THAT one. So an unmatched id is 'not previously observed',
        # and whether we opened or steered stays unknown.
        previously_observed = bool(facts.turn_id and facts.turn_id in known_turns)
        record = attempt_record(
            facts,
            request_id=request_id, event_id=event_id, attempt_no=attempt_no,
            recipient=recipient,
            status_before=_status_for_record(observation),
            observed_at=self.clock.iso(),
        )
        self._settle(event_id, request_id, record, facts, previously_observed, now)
        result = dict(record)
        result["_turnPreviouslyObserved"] = previously_observed
        result["_turnOrigin"] = "steered_observed_turn" if previously_observed else (
            "unknown" if facts.turn_id else None
        )
        result["_lifecycle"] = observation.deliverable
        return result

    # --------------------------------------------------------------- states

    def _settings_for(self, task_id: str, runtime_status=None):
        """This send's gate, which is the module's rather than this class's.

        Delegated so a second sender cannot grow a second gate. The supervisor channel resumes
        a task too, and the bridge refuses a send with no settings precisely so nothing
        inherits a host default; two implementations of that rule would eventually disagree
        about which task may be woken under what.
        """
        return authorized_settings(self.store, task_id, runtime_status)

    def _withhold_settings(self, event_id: str, now: float, refusal, *, attempts: int,
                           row=None) -> None:
        """Withheld before any transport call, naming what the record got wrong.

        Not a permanent hold: settings that were never recorded can be recorded, and the next
        pass decides again. Nothing was claimed and nothing was sent, so there is no attempt.
        """
        when = now + self.policy.lifecycle_recheck_seconds
        with self.store.transaction() as db:
            db.execute(
                "UPDATE deliveries SET state = ?, next_eligible_at = ?, updated_at = ?"
                " WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?",
                (
                    WITHHELD_PRE_SEND, when, self.clock.iso(), event_id, QUEUED, DEFERRED_BUSY,
                    WITHHELD_PRE_SEND, attempts,
                ),
            )
            self.store.journal(
                "delivery_withheld", event_id,
                {"reason": refusal.reason.value if refusal.reason else "settings_unavailable",
                 "detail": refusal.detail},
                at=self.clock.iso(),
            )
        # Outside the transaction above, and recorded because there is no attempt to read it
        # from. Every other cause reaches an operator through the attempt record; this one
        # refused before one existed, so status reported the generic awaiting_receipt and
        # said nothing about the settings that are actually missing.
        self.record_failure(
            event_id, "settings_check",
            detail=refusal.detail,
            relationship_id=row["relationship_id"] if row is not None else None,
            error_code=refusal.reason.value if refusal.reason else "settings_unavailable",
            retry_safe=True, next_retry_at=when,
        )

    def record_settings_violation(self, request_id: str, event_id: str, findings) -> dict:
        """Annotate a dispatch that already reached a turn. It stays a dispatch.

        The frozen deliveryState enum has no room for a delivered-but-suspect state, and inventing
        one would strip a real delivery of reconciliation and acknowledgement, which are exactly
        what it needs. retrySafe stays false and the turn id is retained.
        """
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO attempt_settings_violations (request_id, event_id, findings,"
                " observed_at) VALUES (?,?,?,?)"
                " ON CONFLICT(request_id) DO UPDATE SET findings = excluded.findings,"
                "   observed_at = excluded.observed_at",
                (request_id, event_id, json.dumps(findings), self.clock.iso()),
            )
            self.store.journal(
                "dispatch_settings_violation", event_id,
                {"requestId": request_id, "findings": findings}, at=self.clock.iso(),
            )
        return {"requestId": request_id, "eventId": event_id, "findings": findings}

    def settings_violation(self, request_id: str):
        row = self.store.one(
            "SELECT findings, observed_at FROM attempt_settings_violations WHERE request_id = ?",
            (request_id,),
        )
        if row is None:
            return None
        return {"findings": json.loads(row["findings"]), "observedAt": row["observed_at"]}

    def _rate_limited(self, recipient: str, now: float) -> bool:
        """The preflight: the claim's own predicate, read before the host is.

        An optimisation and nothing more. Two callers can both pass it, so what actually
        bounds the recipient is reserve_send inside _claim.
        """
        return send_refusal(self.store.db, self.policy, recipient, now) is not None

    def _reschedule(self, event_id: str, state: str, when: float, *, attempts: int) -> None:
        # Guarded on the state and attempt count we observed. Another caller may have
        # dispatched this delivery while we were reading the host, and a stale observation
        # must never be able to drag it backwards.
        with self.store.transaction() as db:
            db.execute(
                "UPDATE deliveries SET state = ?, next_eligible_at = ?, updated_at = ?"
                " WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?",
                (
                    state, when, self.clock.iso(), event_id, QUEUED, DEFERRED_BUSY,
                    WITHHELD_PRE_SEND, attempts,
                ),
            )

    def _defer_busy(self, event_id: str, row, now: float) -> None:
        attempts = row["attempt_count"]
        hold = None
        if attempts >= self.policy.busy_max_attempts:
            hold = self.policy.cap_reason("busy")
        self.record_failure(
            event_id, "parent_busy", detail="the recipient is mid-turn and is never interrupted",
            relationship_id=row["relationship_id"],
            next_retry_at=now + self.policy.delay_for(attempts + 1, "busy"),
        )
        with self.store.transaction() as db:
            db.execute(
                "UPDATE deliveries SET state = ?, next_eligible_at = ?, hold_reason = ?,"
                " updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?",
                (
                    DEFERRED_BUSY, now + self.policy.delay_for(attempts + 1, "busy"), hold,
                    self.clock.iso(), event_id, QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND,
                    attempts,
                ),
            )
            if row["state"] != DEFERRED_BUSY:
                self.store.journal("delivery_deferred_busy", event_id, at=self.clock.iso())

    def _withhold_inactive(self, event_id: str, relationship, now: float, *, attempts: int):
        """An assignment status a person set stops the delivery here, with the reason kept.

        Deliberately NOT a hold_reason: paused, cancelled and archived are exactly the statuses
        resume() lifts, so the delivery has to stay claimable afterwards. Deliberately not a
        failed_operations row either - somebody stopping their own work is not a service
        failure, and a parent reading this scope's failures to decide whether it may wait idle
        would find one and stand down over it.

        The status is re-checked inside the write. Between the caller's read and this statement
        the relationship can be resumed, and holding an active assignment's delivery for a whole
        recheck interval on a stale reading is the one way this could delay real work.

        The retry time is resolved in the statement for the same reason. An existing backoff is
        never brought forward - whatever set it, a busy recipient or a rate limit, had its own
        reason - and comparing against a value read before the transaction would let a caller
        overwrite an extension another one committed in between. The row's own current value is
        the one that wins, read under the write.
        """
        status = relationship["status"]
        when = now + self.policy.lifecycle_recheck_seconds
        with self.store.transaction() as db:
            cursor = db.execute(
                "UPDATE deliveries SET state = ?,"
                "       next_eligible_at = MAX(?, COALESCE(next_eligible_at, 0)),"
                "       updated_at = ?"
                " WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?"
                "   AND EXISTS (SELECT 1 FROM relationships r"
                "                WHERE r.relationship_id = deliveries.relationship_id"
                "                  AND r.status = ? AND r.superseded_by IS NULL)",
                (
                    WITHHELD_PRE_SEND, when, self.clock.iso(), event_id, QUEUED, DEFERRED_BUSY,
                    WITHHELD_PRE_SEND, attempts, status,
                ),
            )
            if cursor.rowcount != 1:
                # It moved under the read. Write nothing, say nothing, and let the next
                # attempt decide on whatever is true then.
                return None
            self.store.journal(
                "delivery_withheld_inactive", event_id,
                {"relationshipId": relationship["relationshipId"], "status": status},
                at=self.clock.iso(),
            )
        return {
            "deliveryState": WITHHELD_PRE_SEND,
            "withheldReason": RefusalReason.RELATIONSHIP_NOT_ACTIVE.value,
            "relationshipStatus": status,
            "eventId": event_id,
            "sendAttempted": "no",
        }

    def _withhold(self, event_id: str, observation, now: float, *, attempts: int) -> None:
        # Deliberately NOT a hold_reason. A recipient that is archived, paused or unreadable
        # today may not be tomorrow, and a permanent hold would turn a temporary host state
        # into a delivery that never happens. The reason is recorded in recipient_lifecycle
        # and the journal, and the next observation decides again.
        when = now + self.policy.lifecycle_recheck_seconds
        self.record_failure(
            event_id, "lifecycle_read",
            detail=observation.detail or observation.withhold_reason or "not deliverable",
            error_code=observation.withhold_reason, next_retry_at=when,
        )
        with self.store.transaction() as db:
            db.execute(
                "UPDATE deliveries SET state = ?, next_eligible_at = ?, updated_at = ?"
                " WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?",
                (
                    WITHHELD_PRE_SEND, when, self.clock.iso(), event_id, QUEUED, DEFERRED_BUSY,
                    WITHHELD_PRE_SEND, attempts,
                ),
            )
            self.store.journal(
                "delivery_withheld", event_id,
                {"reason": observation.withhold_reason, "detail": observation.detail},
                at=self.clock.iso(),
            )

    def _settle(self, event_id, request_id, record, facts, previously_observed, now) -> None:
        assert_attempt_invariants(record)
        state = facts.delivery_state
        hold = None
        when = None
        if state == DEFERRED_BUSY:
            when = now + self.policy.delay_for(record["attemptNo"] + 1, "busy")
            if record["attemptNo"] >= self.policy.busy_max_attempts:
                hold = self.policy.cap_reason("busy")
        elif state == WITHHELD_PRE_SEND:
            when = now + self.policy.delay_for(record["attemptNo"] + 1, "presend")
            if record["attemptNo"] >= self.policy.max_attempts:
                hold = self.policy.cap_reason("presend")
        elif state == INBOX_ONLY:
            hold = PUSH_CHANNEL_CLOSED
        # held_uncertain and dispatched are never rescheduled: one has no evidence yet and the
        # other already reached a turn.
        with self.store.transaction() as db:
            db.execute(
                "UPDATE attempts SET internal_state = 'settled', state = ?, record = ?,"
                " observed_at = ? WHERE request_id = ?",
                (state, json.dumps(record), record["observedAt"], request_id),
            )
            db.execute(
                "UPDATE deliveries SET state = ?, next_eligible_at = ?, hold_reason = ?,"
                " dispatch_evidence = ?, dispatch_turn_id = ?, lease_owner = NULL,"
                " lease_until = NULL, updated_at = ? WHERE event_id = ?",
                (
                    state, when, hold,
                    "transport_accepted" if state == DISPATCHED else None,
                    facts.turn_id, self.clock.iso(), event_id,
                ),
            )
            self.store.journal(
                "delivery_attempted", event_id,
                {
                    "requestId": request_id, "state": state, "retrySafe": record["retrySafe"],
                    "turnPreviouslyObserved": previously_observed, "hold": hold,
                },
                at=self.clock.iso(),
            )

    def mark_superseded(self, event_id: str, *, reason: str = SUPERSEDED_HOLD) -> None:
        """Withdraw a delivery the recipient cannot already be acting on.

        The guard is part of the UPDATE rather than a preflight read: a concurrent dispatch
        between the check and the write would otherwise be overwritten.
        """
        with self.store.transaction() as db:
            cursor = db.execute(
                "UPDATE deliveries SET state = ?, hold_reason = ?, updated_at = ?"
                "  WHERE event_id = ? AND state IN (?,?,?) AND hold_reason IS NULL",
                (SUPERSEDED, reason, self.clock.iso(), event_id,
                 QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND),
            )
            if cursor.rowcount == 1:
                self.store.journal(
                    "delivery_superseded", event_id, {"reason": reason}, at=self.clock.iso(),
                )
            else:
                self._annotate_supersession_in(db, event_id, reason)

    def _suppress_if_superseded(self, event_id: str):
        """Record that this delivery is no longer current, and say so. Commits.

        An outstanding send is annotated rather than rewritten: reconciliation refuses to
        promote a terminal superseded aggregate, so rewriting one would make a lost
        response permanently unresolvable.
        """
        with self.store.transaction() as db:
            reason = self._supersession_reason(db, event_id)
            if not reason:
                return None
            cursor = db.execute(
                "UPDATE deliveries SET state = ?, hold_reason = ?, updated_at = ?"
                "  WHERE event_id = ? AND state IN (?,?,?) AND hold_reason IS NULL",
                (SUPERSEDED, reason, self.clock.iso(), event_id,
                 QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND),
            )
            if cursor.rowcount == 1:
                self.store.journal(
                    "delivery_superseded", event_id, {"reason": reason},
                    at=self.clock.iso(),
                )
            else:
                self._annotate_supersession_in(db, event_id, reason)
            return reason


    def _last_failure(self, event_id):
        rows = self.failures_for(event_id)
        return rows[0] if rows else None

    def _supersession_note(self, event_id):
        row = self.store.one(
            "SELECT reason, noted_at FROM delivery_supersession WHERE event_id = ?",
            (event_id,),
        )
        return dict(row) if row else None

    def _grant_state(self, event_row):
        """What a grant delivery's own turn now says about it, or None for any other kind.

        A pure read on the open connection. It exists because the reported phase cannot be
        derived from the delivery row alone for this direction: a grant is answered on the
        merge turn, not through an acks row, so a delivered one would report a wait for an
        acknowledgement forever with nothing anywhere to settle it.
        """
        if event_row["kind"] != MERGE_TURN_GRANT:
            return None
        event = self.store.one(
            "SELECT relationship_id, execution_generation, outcome, event_id, receipt"
            "  FROM events WHERE event_id = ?", (event_row["event_id"],))
        if event is None:
            return None
        return self._grant_supersession(self.store.db, event)

    def observation_health(self, *, now=None, stale_after=900.0, relationship_id=None) -> dict:
        """Whether the loop is actually looking, which a live process does not answer.

        The JUN-100 and JUN-101 incident had a live pid, inside its time bound, polling
        nothing useful and delivering nothing. Liveness is reported separately and is
        never counted here.
        """
        from datetime import datetime

        def age(stamp):
            if not stamp:
                return None
            try:
                seen = datetime.fromisoformat(stamp.replace("Z", "+00:00"))
            except ValueError:
                return None
            return max(0.0, (now or self.clock.now()) - seen.timestamp())

        staged = [
            {"eventId": row["event_id"], "turnId": row["turn_id"],
             "ageSeconds": age(row["staged_at"] or row["first_seen_at"])}
            for row in self.store.all(
                # Joined through relationships with the same active predicate the anchors
                # use. A paused, cancelled or superseded assignment is no longer settled by
                # the scheduler, so its staged event would age forever and hold the whole
                # health block at degraded while every active assignment was fine.
                "SELECT e.event_id, e.turn_id, e.staged_at, e.first_seen_at FROM events e"
                "  JOIN relationships r ON r.relationship_id = e.relationship_id"
                " WHERE e.stage = 'staged'"
                "   AND r.status = 'active' AND r.superseded_by IS NULL"
                + (" AND e.relationship_id = ?" if relationship_id else "")
                + " ORDER BY e.first_seen_at",
                (relationship_id,) if relationship_id else (),
            )
        ]
        anchors, backlog = {}, {}
        # Built from the ACTIVE current generations and left-joined to their polls, not from
        # poll_observations. Starting from the poll table omits an anchor that has never been
        # read at all - which is exactly the relationship a rotating scheduler has not reached
        # yet - so the aggregate could report healthy while some current anchor was untouched.
        for row in self.store.all(
            "SELECT g.relationship_id, g.dispatch_turn_id, p.last_polled_at, p.last_error"
            "     , r.child_task_id"
            "     , (SELECT COUNT(*) FROM assignment_settlements o"
            "         WHERE o.thread_id = r.child_task_id"
            "           AND o.turn_id = g.dispatch_turn_id"
            # Per assignment, like the scheduler's own check. Two assignments can share a
            # child turn, and asking globally let one assignment's observation mark the
            # other settled - excluding an assignment whose own settlement was still
            # outstanding from the very freshness check that would have shown it.
            # assignment_settlements is the per-assignment fact; observations is keyed by
            # the turn alone and can only ever name whoever settled it first.
            "           AND o.relationship_id = r.relationship_id) AS observed"
            "     , (SELECT COUNT(*) FROM events e"
            "         WHERE e.turn_thread_id = r.child_task_id"
            "           AND e.turn_id = g.dispatch_turn_id"
            # Per assignment, like the observation beside it. A staged claim belonging to
            # another assignment on a shared child turn is not this one's outstanding work,
            # and counting it unsettled a settled assignment into a false stall.
            "           AND e.relationship_id = r.relationship_id"
            "           AND e.stage = 'staged') AS staged_here"
            "  FROM relationships r"
            "  JOIN generations g ON g.relationship_id = r.relationship_id"
            "   AND g.execution_generation = r.execution_generation"
            "  LEFT JOIN poll_observations p"
            "    ON p.relationship_id = g.relationship_id"
            "   AND p.execution_generation = g.execution_generation"
            "   AND p.turn_id = g.dispatch_turn_id"
            " WHERE r.status = 'active' AND r.superseded_by IS NULL"
            + (" AND r.relationship_id = ?" if relationship_id else ""),
            (relationship_id,) if relationship_id else (),
        ):
            if row["dispatch_turn_id"] is None:
                # A generation whose anchor is still pending has no turn to poll. That is a
                # delivery phase, not a scheduler that stopped looking, and counting it as
                # never polled reported stalled for a relay behaving exactly as designed.
                anchors[row["relationship_id"]] = {
                    "turnId": None, "lastPolledAt": None, "ageSeconds": None,
                    "lastError": None, "settled": False, "anchorPending": True,
                }
                continue
            # The scheduler deliberately stops reading a turn once it is terminal and nothing
            # is staged behind it, so its last poll can never advance again. Ageing that out
            # marked every quiet, fully observed assignment stalled forever, which is the
            # opposite of the signal this exists to give.
            settled = bool(row["observed"]) and not row["staged_here"]
            anchors[row["relationship_id"]] = {
                "turnId": row["dispatch_turn_id"], "lastPolledAt": row["last_polled_at"],
                "ageSeconds": age(row["last_polled_at"]), "lastError": row["last_error"],
                "settled": settled, "anchorPending": False,
            }
        for row in self.store.all(
            # Joined through relationships with the same predicate stagedEvents uses. Reading
            # events directly made the two fields disagree the moment an assignment was
            # paused or cancelled: stagedEvents went empty while backlog still reported work
            # the scheduler will never process.
            "SELECT e.relationship_id AS relationship_id, COUNT(*) AS n FROM events e"
            "  JOIN relationships r ON r.relationship_id = e.relationship_id"
            " WHERE e.stage = 'staged'"
            "   AND r.status = 'active' AND r.superseded_by IS NULL"
            + (" AND e.relationship_id = ?" if relationship_id else "")
            + " GROUP BY e.relationship_id",
            (relationship_id,) if relationship_id else (),
        ):
            backlog[row["relationship_id"]] = row["n"]
        oldest = max([s["ageSeconds"] or 0.0 for s in staged], default=0.0)
        never = [rid for rid, a in anchors.items()
                 if a["lastPolledAt"] is None and not a["settled"]
                 and not a["anchorPending"]]
        stale = [rid for rid, a in anchors.items()
                 if not a["settled"] and not a["anchorPending"]
                 and a["ageSeconds"] is not None
                 and a["ageSeconds"] > stale_after]
        if never or stale:
            health, reason = "stalled", (
                f"{len(never)} anchors never successfully polled,"
                f" {len(stale)} not polled for over {stale_after:.0f}s"
            )
        elif oldest > stale_after:
            health, reason = "degraded", f"a staged event has waited {oldest:.0f}s"
        else:
            health, reason = "healthy", ""
        return {"stagedEvents": staged, "oldestStagedAgeSeconds": oldest,
                "anchors": anchors, "backlog": backlog,
                "health": health, "reason": reason,
                "note": "process liveness is reported separately and is not health"}

    def record_failure(self, scope_key, operation, *, detail, relationship_id=None,
                       parent_task_id=None, error_code=None, difference=None,
                       retry_safe=None, next_retry_at=None) -> None:
        """The most recent cause for one subject and one operation.

        Fed from RETURNED failure values as well as exceptions. The settings rejection that
        matters most in practice never raises: the host answers with a failed receipt and the
        transport classification keeps only a code, dropping the field-level findings.
        """
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO failed_operations (scope_key, operation, relationship_id,"
                " parent_task_id, detail, error_code, difference, retry_safe, occurred_at,"
                " next_retry_at) VALUES (?,?,?,?,?,?,?,?,?,?)"
                " ON CONFLICT(scope_key, operation) DO UPDATE SET detail=excluded.detail,"
                " error_code=excluded.error_code, difference=excluded.difference,"
                " retry_safe=excluded.retry_safe, occurred_at=excluded.occurred_at,"
                " next_retry_at=excluded.next_retry_at",
                (scope_key, operation, relationship_id, parent_task_id, str(detail),
                 error_code, difference, None if retry_safe is None else int(retry_safe),
                 self.clock.iso(), next_retry_at),
            )

    def failures_for(self, scope_key):
        rows = self.store.all(
            "SELECT * FROM failed_operations WHERE scope_key = ? ORDER BY occurred_at DESC",
            (scope_key,),
        )
        return [dict(row) for row in rows]

    @staticmethod
    def _grant_supersession(db, event):
        """Whether a queued merge-turn grant still has anything to tell its recipient.

        The rule itself belongs to the merge turn and lives there; this reads the notice's own
        receipt to find which turn and which grant it is about, and asks. Imported inside the
        call because this module is reached from registry and mergeturn reaches neither, so a
        module-level edge here would be a ring for no gain.

        A receipt this module cannot read as a grant is fail-closed rather than sent. This
        package wrote it, so an unreadable one means a damaged or hand-edited row, and a
        message asking a parent to acknowledge a grant nobody can name is worse than silence
        an operator can see in the reported phase.
        """
        from .mergeturn import MERGE_TURN_GRANT_UNREADABLE, grant_supersession_in

        try:
            envelope = json.loads(event["receipt"])
        except (TypeError, ValueError):
            return MERGE_TURN_GRANT_UNREADABLE
        if not isinstance(envelope, dict):
            return MERGE_TURN_GRANT_UNREADABLE
        turn, grant = envelope.get("turnId"), envelope.get("grantId")
        if not isinstance(turn, str) or not turn or not isinstance(grant, str) or not grant:
            return MERGE_TURN_GRANT_UNREADABLE
        return grant_supersession_in(db, turn, grant)

    def _supersession_reason(self, db, event_id: str):
        """Is this still the thing it was queued to say? Read inside the caller's write.

        Two rules, and the first is the one the 2026-09-16 reproduction needs: a generation
        that has moved on invalidates every outcome of the previous one - ready, blocked,
        failed, manifest or not - whether or not the new generation has produced a revision
        yet. JUN-119 g2 events delivered after g3 opened and JUN-100 g5 delivered after g7
        were all rejected downstream as stale_generation; suppressing before the send is the
        fix, and a new generation having nothing in it yet is not a reason to send the old.
        """
        event = db.execute(
            "SELECT relationship_id, execution_generation, outcome, event_id, receipt FROM events"
            "  WHERE event_id = ?", (event_id,),
        ).fetchone()
        if event is None:
            return None
        if event["outcome"] in RELAY_NOTICE_OUTCOMES:
            # Answered before the generation is even read. A relay coordination notice is not
            # a fact about the assignment's work, so the generation it happens to ride in
            # cannot make it stale - and measuring it there is what lost a wake permanently,
            # since a promotion does not come round twice for a parent that already holds the
            # target. Its own subject decides.
            return self._grant_supersession(db, event)
        relationship = db.execute(
            "SELECT execution_generation FROM relationships WHERE relationship_id = ?",
            (event["relationship_id"],),
        ).fetchone()
        if relationship is None:
            return None
        if event["execution_generation"] < relationship["execution_generation"]:
            return STALE_GENERATION
        if event["outcome"] == REVISION:
            # Relay-owned, and answered by whatever the child sends back for this generation -
            # reviewable or not. head_revision considers only ready_for_review receipts, so
            # routing a request through it left one answered by a failed, interrupted or
            # blocked reply reported as awaiting_child_receipt forever, even though the
            # completion it asked for had already arrived.
            # Any final event of that generation, not only a child-authored one. A revision
            # turn can fail or be interrupted without the child ever writing a receipt, and
            # the relay then records the outcome itself through daemon_observation - which is
            # exactly the answer the request was waiting for, and is what the parent receives.
            # Requiring producer = 'child' left the request current after that had happened.
            answered = db.execute(
                "SELECT 1 FROM events"
                " WHERE relationship_id = ? AND execution_generation = ?"
                "   AND stage = 'final' AND suppressed_reason IS NULL AND event_id != ?"
                # Except a relay coordination notice, which answers nothing the child was
                # asked for. Queuing a merge-turn grant into this generation would otherwise
                # read as the correction having been answered, and the child's request would
                # be suppressed before any transport call - a correction the parent decided
                # and the child never saw.
                "   AND outcome NOT IN " + NOTICE_OUTCOMES_SQL,
                (event["relationship_id"], event["execution_generation"], event_id),
            ).fetchone()
            return SUPERSEDED_REVISION if answered is not None else None
        # The head rule is one REVISION replacing another, and head_revision only ever
        # considers reviewable events. A child's EXECUTION-ONLY outcome is not competing for
        # that head: it is a different kind of fact about the same generation, and a later
        # one. Measuring it against a head that is already final suppressed it before any
        # transport call, so a generation that ended badly after producing a reviewable
        # revision never told the parent it had ended, while the generation was still current
        # and the event declared no supersession of its own. The generation rule above still
        # covers these, because a generation that has moved on invalidates every outcome of
        # the previous one whatever its shape.
        #
        # Named rather than expressed as "not reviewable". A revision_request is relay-owned
        # and is not reviewable either, but it IS answered by the child's reply - exempting it
        # left the relay's own ask reported as awaiting_child_receipt after the receipt it
        # asked for had arrived.
        if event["outcome"] in EXECUTION_ONLY_OUTCOMES:
            return None
        head = head_revision(
            db, event["relationship_id"], event["execution_generation"],
        )
        if not head["eventId"] or head["eventId"] == event_id:
            # A null head means the generation has no single reviewable revision this one
            # stands behind, which is what an execution-only failure looks like in its OWN
            # current generation. That is not evidence anything replaced it. Ambiguous
            # lineage also lands here and is deliberately NOT read as supersession: it is
            # arbitrated at acknowledgement by revision_currency, and suppressing on it
            # here would destroy the delivery chance of every independent revision in a
            # generation that simply never declared a chain.
            return None
        successor = db.execute(
            "SELECT stage FROM events WHERE event_id = ?", (head["eventId"],),
        ).fetchone()
        # A STAGED successor is a claim, not a replacement. If it later fails it is
        # suppressed, and destroying this event's only delivery chance on the strength of it
        # would be permanent.
        if successor is None or successor["stage"] != "final":
            return None
        return SUPERSEDED_REVISION

    def _annotate_supersession_in(self, db, event_id: str, reason: str) -> None:
        db.execute(
            "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied)"
            " VALUES (?,?,?,0) ON CONFLICT(event_id) DO NOTHING",
            (event_id, reason, self.clock.iso()),
        )

    def annotate_predecessors_in(self, db, event_id: str) -> None:
        """Mark outstanding deliveries this newly final event replaces within its generation.

        The pre-send check cannot reach them: attempt() returns early for a non-claimable
        state, so a revision that was already sending, held_uncertain or dispatched when its
        successor arrived left no supersession row at all. Reconciliation could then promote
        it to dispatched and status would present it as the current delivery.

        Annotation only. Rewriting an outstanding send would make a lost response
        permanently unresolvable, which is worse than the confusion it fixes.
        """
        event = db.execute(
            "SELECT relationship_id, execution_generation FROM events WHERE event_id = ?",
            (event_id,),
        ).fetchone()
        if event is None:
            return
        others = db.execute(
            "SELECT d.event_id FROM deliveries d"
            "  JOIN events e ON e.event_id = d.event_id"
            " WHERE e.relationship_id = ? AND e.execution_generation = ?"
            "   AND d.event_id != ?"
            # inbox_only is terminal and attempt() cannot revisit it, so a predecessor that
            # settled there would stay reported as channel_closed with no supersession note
            # even though acknowledgement currency already rejects it.
            # A predecessor capped in deferred_busy or withheld_pre_send is in the same
            # position: its hold makes attempt() return early, so this is its only chance.
            # queued belongs with them: _claim does suppress a stale queued predecessor, but
            # attempt() returns before _claim for a rate limit, a busy recipient or unreadable
            # settings - and a recipient that is never free means _claim is never reached at
            # all, so the predecessor keeps retrying and keeps reporting as current.
            "   AND d.state IN ('queued','sending','held_uncertain','dispatched','inbox_only',"
            "                  'deferred_busy','withheld_pre_send')",
            (event["relationship_id"], event["execution_generation"], event_id),
        ).fetchall()
        for row in others:
            # Asked per candidate rather than assumed: the successor may not in fact replace
            # it, and _supersession_reason is the one place that rule lives.
            reason = self._supersession_reason(db, row["event_id"])
            if reason:
                self._annotate_supersession_in(db, row["event_id"], reason)

    def annotate_predecessors(self, event_id: str) -> None:
        """The same annotation in its own transaction, for a caller that has none."""
        with self.store.transaction() as db:
            self.annotate_predecessors_in(db, event_id)

    # -------------------------------------------------------- observability

    def sent_message(self, request_id: str):
        """The exact bytes frozen for one request id, or None if that attempt has no record."""
        row = self.store.one(
            "SELECT message FROM attempt_messages WHERE request_id = ?", (request_id,)
        )
        return row["message"] if row else None

    def attempt_messages(self, event_id: str) -> list:
        """Every attempt's bytes with the delivery status those bytes actually reached.

        Status is read from the attempt, never inferred from the bytes existing. Persisted
        bytes prove a claim was committed; they prove nothing about a turn/start that may have
        crashed, been refused, or returned nothing. An attempt older than this table reports
        'unavailable' rather than being re-rendered from a counter.
        """
        rows = self.store.all(
            "SELECT a.request_id, a.attempt_no, a.internal_state, a.state, a.record,"
            "       a.sent_at, m.message"
            "  FROM attempts a"
            "  LEFT JOIN attempt_messages m ON m.request_id = a.request_id"
            " WHERE a.event_id = ? ORDER BY a.attempt_no",
            (event_id,),
        )
        out = []
        for row in rows:
            record = json.loads(row["record"]) if row["record"] else None
            out.append({
                "requestId": row["request_id"],
                "attemptNo": row["attempt_no"],
                "status": _message_status(row, record),
                "deliveryState": row["state"],
                "sentAt": row["sent_at"],
                "message": row["message"],
            })
        return out

    def snapshot(self, *, relationship_id=None) -> dict:
        sql = "SELECT * FROM deliveries"
        params = ()
        if relationship_id:
            sql += " WHERE relationship_id = ?"
            params = (relationship_id,)
        rows = self.store.all(sql + " ORDER BY created_at", params)
        items = []
        for row in rows:
            attempts = self.store.all(
                "SELECT request_id, attempt_no, internal_state, state, affirmative_evidence,"
                " operation_observation, recipient_scan, record FROM attempts WHERE event_id = ?"
                " ORDER BY attempt_no",
                (row["event_id"],),
            )
            ack = self.store.one("SELECT * FROM acks WHERE event_id = ?", (row["event_id"],))
            verdict = self.store.one(
                "SELECT verdict FROM verdicts WHERE event_id = ?", (row["event_id"],)
            )
            failure = self._last_failure(row["event_id"])
            superseded = self._supersession_note(row["event_id"])
            items.append({
                "eventId": row["event_id"],
                "kind": row["kind"],
                "recipient": row["recipient_task_id"],
                "state": row["state"],
                "reported": _reported_state(row, ack),
                "attempts": row["attempt_count"],
                "holdReason": row["hold_reason"],
                "nextEligibleAt": row["next_eligible_at"],
                "dispatchEvidence": row["dispatch_evidence"],
                "acknowledged": bool(ack),
                "ackVerified": ack["verified"] if ack else None,
                "verdict": verdict["verdict"] if verdict else None,
                "attemptDetail": [dict(a) for a in attempts],
                "phase": _phase(row, attempts, ack, failure, superseded,
                                grant=self._grant_state(row)),
                "lastFailedOperation": failure,
                "nextRetryAt": row["next_eligible_at"],
                "supersededNote": superseded,
            })
        # Events whose delivery was wanted and refused have no deliveries row at all, so a
        # permanently paused or unauthorized assignment had no status entry, no phase and no
        # retry time while the daemon went on retrying it. The most stuck state in the system
        # was the one status could not show.
        intents = [
            {"eventId": row["event_id"], "relationshipId": row["relationship_id"],
             "kind": row["kind"], "recipient": row["recipient_task_id"],
             "phase": "refused_pre_queue", "attempts": row["attempts"],
             "nextRetryAt": row["next_retry_at"], "lastError": row["last_error"],
             "notedAt": row["noted_at"]}
            for row in self.store.all(
                "SELECT i.* FROM delivery_intent i"
                "  LEFT JOIN deliveries d ON d.event_id = i.event_id"
                " WHERE d.event_id IS NULL"
                + (" AND i.relationship_id = ?" if relationship_id else "")
                + " ORDER BY i.noted_at",
                (relationship_id,) if relationship_id else (),
            )
        ]
        return {"deliveries": items, "pendingIntents": intents}


def send_refusal(db, policy, recipient: str, now: float):
    """Why ``recipient`` may not be woken at ``now``, or None when it may.

    ONE predicate for every sender that resumes a task. Deliveries and supervisor reports
    share one recipient_rate budget on purpose, because the bound limits how often a TASK is
    woken, and each sender reads this both as a preflight before the host and inside the claim
    that spends the budget. A copy that only one claim re-checked was a bound only one sender
    obeyed: a delivery claimed after a supervisor report never re-read last_send_at, and the
    two woke one task inside the gap.

    The gap is read across windows. last_send_at lives on the hour's row, so a read of the
    current hour alone let two sends a second apart straddle the boundary.
    """
    last = db.execute(
        "SELECT MAX(last_send_at) AS last FROM recipient_rate WHERE recipient_task_id = ?",
        (recipient,),
    ).fetchone()
    if (last is not None and last["last"] is not None
            and (now - last["last"]) < policy.min_send_interval_seconds):
        return "min_send_interval"
    window = int(now // 3600) * 3600
    used = db.execute(
        "SELECT sends FROM recipient_rate WHERE recipient_task_id = ? AND window_start = ?",
        (recipient, window),
    ).fetchone()
    if used is not None and used["sends"] >= policy.max_sends_per_recipient_per_hour:
        return "hourly_cap"
    return None


def reserve_send(db, policy, recipient: str, now: float):
    """Spend one send of ``recipient``'s budget inside the caller's claim, or say why not.

    Both claims call this inside their own BEGIN IMMEDIATE, so the read and the write are one
    serialised step and the second of two racing claims reads the first one's send. A refusal
    returns before anything is written; the caller raises, and its transaction takes the rest
    of the claim back with it.
    """
    refused = send_refusal(db, policy, recipient, now)
    if refused is not None:
        return refused
    window = int(now // 3600) * 3600
    db.execute(
        "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
        " VALUES (?,?,1,?)"
        " ON CONFLICT(recipient_task_id, window_start) DO UPDATE SET"
        " sends = sends + 1, last_send_at = excluded.last_send_at",
        (recipient, window, now),
    )
    return None


def authorized_settings(store, task_id: str, runtime_status=None):
    """The recorded settings, validated, for any sender that resumes a task.

    Absence, incompleteness, a non-string cwd, model or reasoningEffort, and an approvalPolicy
    this transport cannot carry all refuse -- the last one on the record rather than on what a
    host later reports back, because a row asking for an interactive policy settles the send
    whatever the host would have answered.

    A module function rather than a method, because this class is no longer the only thing that
    sends. The bridge refuses a send carrying no settings precisely so that nothing inherits a
    host default, and one rule about which task may be woken under what is worth more than two
    copies that agree today.
    """
    from .registry import load_settings

    settings = load_settings(store, task_id)
    if settings is None:
        raise DeliveryRefused(
            RefusalReason.SETTINGS_UNAVAILABLE,
            f"no authorized settings recorded for {task_id!r}; register them from the"
            " creation result before a send can preserve them",
        )
    settings.require_usable()
    # ---------------------------------------------------------------------- role policy
    # The record is still the thing a send verifies against; this only asks whether it has
    # fallen behind the policy for the role this task actually holds. A task bound to no scope
    # is outside the policy and nothing about it changes.
    role = rolepolicy.bound_role(store, task_id)
    if role is None:
        return settings
    if isinstance(role, rolepolicy.Contested):
        raise rolepolicy.refuse_contested(role, task_id)
    policy = rolepolicy.declared()
    if not policy:
        raise rolepolicy.refuse_unresolved(policy, role, task_id)
    finding = rolepolicy.check_record(settings, role, policy)
    if finding is not None:
        # Dispatched on the code the finding carries rather than on the assumption that a
        # finding which is not the undeclared one must be a stale record. That assumption read
        # `recorded` and `expected` off a citation finding which has neither and raised a
        # KeyError out of the gate, leaving the delivery queued instead of withheld -- a
        # revalidation path failing open on exactly the legacy records it exists to catch.
        raise DeliveryRefused(
            RefusalReason(finding["code"]),
            f"{task_id!r} is bound as {role!r}: "
            + rolepolicy.describe(finding)
            + f" (policy {finding['digest']}). Nothing was sent and no turn was started. "
            + finding.get("recovery", rolepolicy.RECOVERY),
        )
    # The bridge applies this rule on its own tool path, and a relay send does not take that
    # path: it resumes through its own transport. Applied here too, or a send reaches a thread
    # the tool surface would have refused.
    unloaded = rolepolicy.check_unloaded_transmission(settings, role, policy, runtime_status)
    if unloaded is not None:
        raise unloaded
    # The status above is the one observed before this send listed turns and claimed itself, so
    # it can be stale by the time the transport resumes. The transport takes its own read
    # immediately before that resume; this is what tells it to apply the same rule there, on
    # the state that actually holds.
    settings.refuse_when_unloaded = (
        rolepolicy.check_unloaded_transmission(settings, role, policy, "notLoaded") is not None
    )
    return settings


def _message_status(row, record) -> str:
    """How far the persisted bytes actually got.

    Read from the attempt's own settled record rather than from the bytes existing, because a
    committed claim followed by a crash, a busy refusal or a settings rejection leaves bytes
    and no turn. Calling those 'sent' would misreport exactly the cases recovery is for.
    """
    if row["message"] is None:
        # An attempt older than the attempt_messages table. The bytes are gone and are NOT
        # reconstructed: a re-render would be a guess dressed as a record.
        return "unavailable"
    if row["internal_state"] != "settled":
        return "prepared"
    if row["state"] in (DISPATCHED, "acknowledged"):
        return "dispatched"
    if record and record.get("sendAttempted") == "no":
        return "confirmed_unsent"
    return "uncertain"


def _reported_state(row, ack) -> str:
    """What an operator should read, as distinct from the raw state.

    An inbox-only event is stored and NOT woken, and saying so plainly is the point: a durable
    inbox item is not a successful wake and must never be reported as one.
    """
    if ack is not None and ack["verified"] == "verified" and ack["accepted"]:
        return "acknowledged"
    if row["state"] == INBOX_ONLY:
        return "stored_not_woken"
    if row["state"] == DISPATCHED:
        return "dispatched_awaiting_ack"
    if row["state"] == HELD_UNCERTAIN:
        return "held_uncertain_awaiting_evidence"
    if row["hold_reason"]:
        return f"held:{row['hold_reason']}"
    return row["state"]


def _status_for_record(observation) -> str:
    if observation.deliverable == "busy":
        return "active"
    if observation.runtime_status in ("idle", "active", "notLoaded", "systemError"):
        return observation.runtime_status
    return "unknown"


class _NotClaimable(Exception):
    pass


class _Paced(_NotClaimable):
    """A claim refused on the recipient's shared send budget, and on nothing else.

    Its own class so attempt() can defer the delivery the way the preflight would have. As a
    plain refusal it left the row eligible at once, for a retry the same budget refuses again.
    """


def _render_findings(findings):
    """The exact fields the host disagreed on, not just that it disagreed."""
    if not findings:
        return None
    parts = []
    for finding in findings:
        if not isinstance(finding, dict):
            continue
        field = finding.get("field", finding.get("code", "?"))
        parts.append(f"{field}: expected {finding.get('expected')!r},"
                     f" host {finding.get('returned')!r}")
    return "; ".join(parts) or None


class _LateSupersession(Exception):
    """Discovered inside the claim, after the pre-claim check had already passed."""

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


class _Superseded(Exception):
    """This delivery is no longer current, decided inside the claim."""

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def _manifest_paths(event_row):
    """The declared paths a receipt carries, or none. The manifest lives inside the receipt
    JSON rather than in a column of its own."""
    if event_row is None:
        return ()
    try:
        receipt = json.loads(event_row["receipt"])
    except (TypeError, ValueError, IndexError, KeyError):
        return ()
    entries = receipt.get("manifest") or ()
    return tuple(
        entry["path"] for entry in entries
        if isinstance(entry, dict) and isinstance(entry.get("path"), str)
    )



def _phase(row, attempts, ack, failure=None, superseded=None, grant=None) -> str:
    """Which stage a delivery is actually at, without inventing certainty.

    withheld_pre_send used to mean five different things at once, and the cause is the only
    part that suggests an action. But the cure must not overclaim either: held_uncertain
    means the transport gave no usable answer, which is NOT the same as a turn having been
    accepted, and a settled withheld_pre_send can be an ordinary thread/read failure rather
    than a settings mismatch. Both are read from the attempt record, not from the state word.
    """
    if ack is not None and ack["verified"] == "verified":
        # A verified REJECTION is just as settled as a verified acceptance: the receipt was
        # delivered and the parent answered. Recognising only the accepted case let a
        # rejection fall past every later branch to awaiting_receipt, which says the child
        # has produced nothing - the opposite of what happened.
        if ack["accepted"]:
            return "acknowledged"
        return "rejected"
    if row["state"] == SUPERSEDED:
        return "superseded"
    if superseded is not None:
        # An outstanding send that a newer generation or revision has replaced. Its state is
        # deliberately left alone so a lost response stays reconcilable, but reporting it as
        # awaiting_ack or outcome_unknown describes an obligation nothing can now meet.
        return f"superseded:{superseded['reason']}"
    if row["state"] == INBOX_ONLY or row["hold_reason"] == PUSH_CHANNEL_CLOSED:
        return "channel_closed"
    if row["state"] == DISPATCHED:
        # Only the child-to-parent direction has an acknowledgement in contract v1. A
        # revision request is answered by the child's next completion receipt, and
        # AckService refuses to acknowledge one, so calling this awaiting_ack left every
        # dispatched revision looking permanently stuck on an obligation nothing can meet.
        if row["kind"] == REVISION:
            return "awaiting_child_receipt"
        if row["kind"] == MERGE_TURN_GRANT:
            # Answered on the merge turn rather than here, so the answer is read from there
            # and passed in. Without it a grant that WAS acknowledged - the ordinary, correct
            # outcome - went on reporting a wait, which is the same false obligation the
            # revision branch above exists to remove.
            from .mergeturn import MERGE_TURN_GRANT_ANSWERED

            if grant == MERGE_TURN_GRANT_ANSWERED:
                return "grant_acknowledged"
            return "awaiting_grant_acknowledgement"
        return "awaiting_ack"
    if row["state"] == DEFERRED_BUSY:
        return "parent_busy"
    settled = [a for a in attempts if a["internal_state"] == "settled"]
    latest = settled[-1] if settled else None
    operation = latest["operation_observation"] if latest else None
    record = {}
    if latest is not None and latest["record"]:
        try:
            record = json.loads(latest["record"])
        except ValueError:
            record = {}
    failed = record.get("failedOperation")
    if row["state"] == HELD_UNCERTAIN:
        # A turn id is the only affirmative evidence that a turn exists. A failed turn/start
        # with no id means the call was REFUSED, not that its answer was lost, and reporting
        # turn_accepted for it claimed a turn on no evidence at all.
        if record.get("turnId"):
            return "turn_accepted"
        return "outcome_unknown"
    if row["state"] == WITHHELD_PRE_SEND and latest is not None:
        # thread/resume fails for ordinary connectivity and internal reasons too, and the
        # generic branch records the same operation for all of them. Naming those a settings
        # rejection hands an operator a remediation that cannot work.
        # The recorded failure is the discriminator: _settle writes settings_check only when
        # the receipt actually carried field-level findings.
        if failed == "thread/resume" and failure is not None \
                and failure["operation"] == "settings_check":
            return "settings_rejected"
        if failed:
            return f"withheld:{failed}"
        return "withheld_pre_send"
    if row["state"] == WITHHELD_PRE_SEND:
        # Refused before any attempt existed - missing or unusable authorized settings - so
        # there is no attempt record to read the cause from. The persisted failure is.
        if failure is not None:
            return f"withheld:{failure['operation']}"
        return "withheld_pre_send"
    if row["hold_reason"]:
        return f"held:{row['hold_reason']}"
    del operation
    if row["state"] == SENDING:
        # The claim committed and the process stopped, or is stopping. There IS an attempt,
        # and it may already need reconciliation, so awaiting_receipt would point at the
        # child when the open question is about a send this relay made.
        return "in_flight"
    if row["state"] == QUEUED:
        # A delivery row exists, so the receipt was already collected and accepted. What is
        # outstanding is this relay reaching the recipient, not the child producing anything.
        return "awaiting_send"
    return "awaiting_receipt"
