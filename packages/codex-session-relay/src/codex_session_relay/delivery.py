"""Handing an event to a recipient, exactly once, and only when that is allowed.

There is one path to a transport call and it goes through one atomic claim. The claim decides
authorization, staging and eligibility in a single statement, so a pause committed while we
were reading the host cannot be overtaken by a send that was already half-decided.

Everything before the claim is an optimisation. Everything after it is recorded before its
side effect.
"""

import json

from .errors import DeliveryRefused, RefusalReason
from .currency import STALE_GENERATION, SUPERSEDED as SUPERSEDED_REVISION, head_revision
from .identity import request_id as derive_request_id
from .lifecycle import UNKNOWN as LIFECYCLE_UNKNOWN, hold_reason_for, observe, record as record_lifecycle
from .policy import RetryPolicy
from .scope import assert_assignment_delivery, check_recipient
from .transport import (
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    QUEUED,
    SUPERSEDED,
    WITHHELD_PRE_SEND,
    assert_attempt_invariants,
    attempt_record,
    classify_operation_receipt,
)
from .policy import PUSH_CHANNEL_CLOSED, SUPERSEDED as SUPERSEDED_HOLD

COMPLETION = "completion_event"
REVISION = "revision_request"
CLAIMABLE = (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND)
SENDING = "sending"
MANIFEST_LINES = 10
NEWLINE = chr(10)


class DeliveryService:
    def __init__(self, store, registry, intake, clock, *, policy=None,
                 require_lifecycle_evidence: bool = True):
        self.store = store
        self.registry = registry
        self.intake = intake
        self.clock = clock
        self.policy = policy or RetryPolicy()
        self.require_lifecycle_evidence = require_lifecycle_evidence

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
        if recipient_task_id is None:
            recipient_task_id = (
                relationship["child"]["taskId"] if kind == REVISION
                else relationship["parent"]["taskId"]
            )
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
            return dict(existing)
        with self.store.transaction() as db:
            self.enqueue_in(
                db, event_id, relationship_id=event["relationship_id"], kind=kind,
                recipient_task_id=recipient_task_id,
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
        delay = min(
            self.policy.presend_max_seconds,
            self.policy.presend_base_seconds * (2 ** max(0, attempts - 1)),
        )
        db.execute(
            "INSERT INTO delivery_intent (event_id, relationship_id, kind, recipient_task_id,"
            " attempts, next_retry_at, last_error, noted_at) VALUES (?,?,?,?,?,?,?,?)"
            " ON CONFLICT(event_id) DO UPDATE SET attempts = excluded.attempts,"
            " next_retry_at = excluded.next_retry_at, last_error = excluded.last_error",
            (event_id, relationship_id, kind, recipient_task_id, attempts, now + delay,
             str(error), self.clock.iso()),
        )

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

    def _render_for(self, row, record, request) -> str:
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
        """
        if row["kind"] == REVISION:
            return self._render_revision(row, record, request)
        return self._render_completion(row, record, request)

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
        return self._render_for(row, record, request)

    def render_message(self, event_id: str) -> str:
        """Kept as the preview alias so no caller can mean 'what was sent' by accident."""
        return self.preview_message(event_id)

    def _render_completion(self, row, record, request) -> str:
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
            if len(manifest) > MANIFEST_LINES:
                lines.append(
                    f"  ... {len(manifest) - MANIFEST_LINES} more; see"
                    f" 'codex-session-relay show --event {row['event_id']}'"
                )
        else:
            lines.append("deliverables: none (execution-only outcome)")
        if record.get("manifestRef"):
            lines.append(f"manifestRef: {record['manifestRef']}")
        criteria = record.get("criteria")
        if criteria:
            lines.append("criteria claimed by the child:")
            for item in criteria[:MANIFEST_LINES]:
                lines.append(f"  {item.get('id')}: {item.get('verdict')}")
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

    def _render_revision(self, row, record, request) -> str:
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
                    f"  {item.get('id')}: {item.get('verdict')}" + (f" — {note}" if note else "")
                )
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
            parent: list(self.eligible_for_parent(
                parent, now=now, limit=share, offset=(offsets or {}).get(parent, 0),
            ))
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
            message = self._render_for(row, record, request_id)
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
            # Capacity is reserved in the SAME transaction as the claim. Counting after the
            # send let two interleaved callers both pass a cap of one.
            self._count_send(db, recipient, now)
            window = int(now // 3600) * 3600
            used = db.execute(
                "SELECT sends FROM recipient_rate WHERE recipient_task_id = ? AND window_start = ?",
                (recipient, window),
            ).fetchone()
            if used and used["sends"] > self.policy.max_sends_per_recipient_per_hour:
                raise _NotClaimable()
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
            cwd=relationship["parent"].get("cwd") if row["kind"] == COMPLETION else None,
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
            settings = self._settings_for(recipient)
        except DeliveryRefused as refusal:
            self._withhold_settings(event_id, now, refusal, attempts=row["attempt_count"])
            return None

        known_turns = set(adapter.list_turn_ids(row["recipient_thread_id"], limit=25))
        try:
            attempt_no, request_id, message = self._claim(
                event_id, now=now, owner=owner, recipient=recipient
            )
        except _NotClaimable:
            return None
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

    def _settings_for(self, task_id: str):
        """The recorded settings, validated. Absence and incompleteness both refuse."""
        from .registry import load_settings

        settings = load_settings(self.store, task_id)
        if settings is None:
            raise DeliveryRefused(
                RefusalReason.SETTINGS_UNAVAILABLE,
                f"no authorized settings recorded for {task_id!r}; register them from the"
                " creation result before a send can preserve them",
            )
        settings.require_usable()
        return settings

    def _withhold_settings(self, event_id: str, now: float, refusal, *, attempts: int) -> None:
        """Withheld before any transport call, naming what is missing.

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
        window = int(now // 3600) * 3600
        row = self.store.one(
            "SELECT sends, last_send_at FROM recipient_rate WHERE recipient_task_id = ?"
            " AND window_start = ?",
            (recipient, window),
        )
        if row is None:
            return False
        if row["sends"] >= self.policy.max_sends_per_recipient_per_hour:
            return True
        last = row["last_send_at"]
        return last is not None and (now - last) < self.policy.min_send_interval_seconds

    def _count_send(self, db, recipient: str, now: float) -> None:
        window = int(now // 3600) * 3600
        db.execute(
            "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
            " VALUES (?,?,1,?)"
            " ON CONFLICT(recipient_task_id, window_start) DO UPDATE SET"
            " sends = sends + 1, last_send_at = excluded.last_send_at",
            (recipient, window, now),
        )

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

    def observation_health(self, *, now=None, stale_after=900.0) -> dict:
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
                "SELECT event_id, turn_id, staged_at, first_seen_at FROM events"
                " WHERE stage = 'staged' ORDER BY first_seen_at"
            )
        ]
        anchors, backlog = {}, {}
        for row in self.store.all(
            "SELECT p.relationship_id, p.turn_id, p.last_polled_at, p.last_error"
            "  FROM poll_observations p"
            "  JOIN relationships r ON r.relationship_id = p.relationship_id"
            " WHERE p.execution_generation = r.execution_generation"
        ):
            anchors[row["relationship_id"]] = {
                "turnId": row["turn_id"], "lastPolledAt": row["last_polled_at"],
                "ageSeconds": age(row["last_polled_at"]), "lastError": row["last_error"],
            }
        for row in self.store.all(
            "SELECT relationship_id, COUNT(*) AS n FROM events WHERE stage = 'staged'"
            " GROUP BY relationship_id"
        ):
            backlog[row["relationship_id"]] = row["n"]
        oldest = max([s["ageSeconds"] or 0.0 for s in staged], default=0.0)
        never = [rid for rid, a in anchors.items() if a["lastPolledAt"] is None]
        stale = [rid for rid, a in anchors.items()
                 if a["ageSeconds"] is not None and a["ageSeconds"] > stale_after]
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

    def _supersession_reason(self, db, event_id: str):
        """Is this still the thing the assignment stands on? Read inside the caller's write.

        Two rules, and the first is the one the 2026-09-16 reproduction needs: a generation
        that has moved on invalidates every outcome of the previous one - ready, blocked,
        failed, manifest or not - whether or not the new generation has produced a revision
        yet. JUN-119 g2 events delivered after g3 opened and JUN-100 g5 delivered after g7
        were all rejected downstream as stale_generation; suppressing before the send is the
        fix, and a new generation having nothing in it yet is not a reason to send the old.
        """
        event = db.execute(
            "SELECT relationship_id, execution_generation, outcome, event_id FROM events"
            "  WHERE event_id = ?", (event_id,),
        ).fetchone()
        if event is None:
            return None
        relationship = db.execute(
            "SELECT execution_generation FROM relationships WHERE relationship_id = ?",
            (event["relationship_id"],),
        ).fetchone()
        if relationship is None:
            return None
        if event["execution_generation"] < relationship["execution_generation"]:
            return STALE_GENERATION
        head = head_revision(
            db, event["relationship_id"], event["execution_generation"],
        )
        if not head["eventId"] or head["eventId"] == event_id:
            # A null head means the generation has no reviewable revision at all, which is
            # what an execution-only failure looks like in its OWN current generation. That
            # is not evidence anything replaced it.
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
                " operation_observation, recipient_scan FROM attempts WHERE event_id = ?"
                " ORDER BY attempt_no",
                (row["event_id"],),
            )
            ack = self.store.one("SELECT * FROM acks WHERE event_id = ?", (row["event_id"],))
            verdict = self.store.one(
                "SELECT verdict FROM verdicts WHERE event_id = ?", (row["event_id"],)
            )
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
                "phase": _phase(row, attempts, ack),
                "lastFailedOperation": self._last_failure(row["event_id"]),
                "nextRetryAt": row["next_eligible_at"],
                "supersededNote": self._supersession_note(row["event_id"]),
            })
        return {"deliveries": items}


def _message_status(row, record) -> str:
    """How far the persisted bytes actually got."""
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



def _phase(row, attempts, ack) -> str:
    """Which stage a delivery is actually stuck at.

    withheld_pre_send used to mean five different things at once: the receipt has not been
    collected, the parent is mid-turn, the host would not confirm the authorized settings, a
    turn was started, or the acknowledgement is outstanding. An operator reading one word
    could not tell which, and the cause is the only part that suggests an action.
    """
    if ack is not None and ack["verified"] == "verified" and ack["accepted"]:
        return "acknowledged"
    if row["state"] == SUPERSEDED:
        return "superseded"
    if row["state"] == INBOX_ONLY or row["hold_reason"] == PUSH_CHANNEL_CLOSED:
        return "channel_closed"
    if row["state"] == DISPATCHED:
        return "awaiting_ack"
    if row["state"] == DEFERRED_BUSY:
        return "parent_busy"
    if row["state"] == HELD_UNCERTAIN:
        return "turn_accepted"
    settled = [a for a in attempts if a["internal_state"] == "settled"]
    if row["state"] == WITHHELD_PRE_SEND and settled:
        return "settings_rejected"
    if row["hold_reason"]:
        return f"held:{row['hold_reason']}"
    return "awaiting_receipt"
