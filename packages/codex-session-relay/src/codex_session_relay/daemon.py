"""The bounded loop that makes invocation automatic.

This is the part that acts with nobody watching, so every dimension it can consume is bounded:
how many attempts it reconciles, how many turns it observes, how many messages it sends, how many
ticks it runs and for how long. An unbounded run is not merely discouraged, it is inexpressible —
run() requires a tick count or a deadline, and a stop callable is only ever an additional early
exit.

Quiet is a property, not a hope. A tick that learns nothing new writes nothing at all, which is
why the gate below asks whether there is anything to learn before invoking anything that journals.
"""

import fcntl
import os
from dataclasses import dataclass, field
from pathlib import Path

from .delivery import COMPLETION
from .models import TurnRef
from .policy import RetryPolicy
from .receipts import ObservationOutcome, classify_observation
from .transport import DISPATCHED, HELD_UNCERTAIN


@dataclass
class TickReport:
    observed: int = 0
    reconciled: int = 0
    delivered: int = 0
    deferred: int = 0
    skipped: int = 0
    acksVerified: int = 0
    quiet: bool = True
    notes: list = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "observed": self.observed, "reconciled": self.reconciled,
            "delivered": self.delivered, "deferred": self.deferred,
            "skipped": self.skipped, "acksVerified": self.acksVerified,
            "quiet": self.quiet, "notes": self.notes,
        }


class SingleInstance:
    """One daemon per state directory. A second one exits rather than racing the first."""

    def __init__(self, directory):
        self.path = Path(directory) / "daemon.lock"
        self._handle = None

    def __enter__(self):
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        self._handle = open(self.path, "w")
        try:
            fcntl.flock(self._handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as error:
            self._handle.close()
            self._handle = None
            raise RuntimeError(f"another relay daemon already holds {self.path}") from error
        self._handle.write(str(os.getpid()))
        self._handle.flush()
        return self

    def __exit__(self, *_exc):
        if self._handle is not None:
            fcntl.flock(self._handle, fcntl.LOCK_UN)
            self._handle.close()
            self._handle = None


class RelayDaemon:
    def __init__(self, store, registry, intake, delivery, ack, reconciler, adapter, *,
                 policy=None, clock=None, log=None):
        self.store = store
        self.registry = registry
        self.intake = intake
        self.delivery = delivery
        self.ack = ack
        self.reconciler = reconciler
        self.adapter = adapter
        self.policy = policy or RetryPolicy()
        self.clock = clock or delivery.clock
        self.log = log or (lambda _message: None)

    # ------------------------------------------------------------------ tick

    def tick(self, *, now=None) -> TickReport:
        now = self.clock.now() if now is None else now
        report = TickReport()
        self._observe(report, now)
        self._reconcile(report, now)
        self._verify_acks(report, now)
        self._deliver(report, now)
        report.quiet = not (report.observed or report.reconciled or report.delivered
                            or report.deferred or report.acksVerified)
        return report

    def _verify_acks(self, report, now) -> None:
        """Complete acknowledgements a parent authored without a host.

        This is the process that holds host access, so it is where recorded intent becomes
        verified evidence. It re-checks disposition as it goes: an intent whose generation
        advanced or whose relationship paused while the evidence was missing is left exactly as
        the parent wrote it and reports why, rather than being promoted on turn evidence alone.
        """
        try:
            results = self.ack.verify_pending_acks(self.adapter, now=now)
        except Exception as error:  # noqa: BLE001 - a tick never dies on one pass
            report.notes.append(f"pending acknowledgement pass failed: {error}")
            return
        report.acksVerified = sum(1 for r in results if r.get("outcome") == "verified")
        for result in results:
            if result.get("outcome") == "withheld":
                report.notes.append(
                    f"acknowledgement {result['eventId']} not promoted: {result['reason']}"
                )

    def run(self, *, max_ticks=None, deadline=None, stop=None, sleep=None) -> list:
        """Bounded by construction.

        A stop callable is an ADDITIONAL early exit, never the bound: stop=lambda: False would
        otherwise be an unbounded loop wearing a bound's clothing. The wall-clock granularity is
        one adapter call, because a synchronous request already in flight cannot be cancelled.
        """
        if max_ticks is None and deadline is None:
            raise ValueError(
                "run() needs max_ticks or deadline; a stop callable is not a bound because it "
                "may never return True"
            )
        reports = []
        ticks = 0
        while True:
            if max_ticks is not None and ticks >= max_ticks:
                break
            if deadline is not None and self.clock.now() >= deadline:
                break
            if stop is not None and stop():
                break
            reports.append(self.tick())
            ticks += 1
            if sleep is not None and (max_ticks is None or ticks < max_ticks):
                sleep(self.policy.poll_interval_seconds)
        return reports

    # --------------------------------------------------------------- observe

    def _observe(self, report, now) -> None:
        """Detect terminal turns and settle what they decide, without re-reporting old news."""
        for relationship in self._active_relationships():
            for turn_id in self._turns_to_poll(relationship):
                thread = relationship["child"]["taskId"]
                try:
                    turn = self.adapter.read_turn(thread, turn_id)
                except Exception as error:
                    report.notes.append(f"turn read failed for {turn_id}: {error}")
                    continue
                if turn is None or turn.status not in ("completed", "failed", "interrupted"):
                    continue
                reference = TurnRef(thread, turn.turn_id, turn.status)
                if self._already_observed(reference):
                    continue
                self._settle_turn(relationship, reference, report)

    def _turns_to_poll(self, relationship) -> list:
        """The anchor, plus any turn carrying a staged claim.

        Polling only the anchor would leave a claim staged on a later admitted turn unresolved
        forever, which is exactly the multi-turn case a loop produces.
        """
        turns = []
        for generation in relationship["generations"]:
            if generation["dispatchTurnId"]:
                turns.append(generation["dispatchTurnId"])
        for row in self.intake.staged_events(thread_id=relationship["child"]["taskId"]):
            if row["turn_id"] not in turns:
                turns.append(row["turn_id"])
        return turns[: self.policy.max_reconciles_per_tick]

    def _already_observed(self, reference: TurnRef) -> bool:
        return self.store.one(
            "SELECT 1 FROM observations WHERE thread_id = ? AND turn_id = ?"
            " AND terminal_status = ?",
            (reference.thread_id, reference.turn_id, reference.turn_status),
        ) is not None

    def _settle_turn(self, relationship, reference, report) -> None:
        resolved = self.intake.resolve_staged(reference)
        outcome = classify_observation(reference.turn_status, None)
        event_id = None
        synthesized = None
        if reference.turn_status in ("failed", "interrupted") and not resolved["finalized"]:
            try:
                receipt = self.intake.daemon_observation(
                    relationship["relationshipId"], reference
                )
                event_id = receipt["eventId"]
                synthesized = event_id
            except Exception as error:
                report.notes.append(f"daemon observation refused: {error}")
        self.intake.record_observation(
            reference, outcome, relationship_id=relationship["relationshipId"], event=event_id
        )
        # Storing an execution-only receipt is not telling anyone. A parent that is waiting for
        # a verdict has to learn that the child failed, so a synthesized observation is queued
        # like any other event; persistence and notification are separate outcomes and are
        # reported separately.
        for queueable in list(resolved["finalized"]) + ([synthesized] if synthesized else []):
            try:
                self.delivery.enqueue(queueable)
            except Exception as error:
                report.notes.append(f"enqueue refused for {queueable}: {error}")
        report.observed += 1

    # ------------------------------------------------------------- reconcile

    def _reconcile(self, report, now) -> None:
        budget = self.policy.max_reconciles_per_tick
        for attempt in self.reconciler.open_attempts()[:budget]:
            request_id = attempt["request_id"]
            decision, fingerprint = self._gate(attempt)
            if not decision:
                report.skipped += 1
                continue
            try:
                outcome = self.reconciler.reconcile_attempt(request_id, self.adapter, now=now)
            except Exception as error:
                self._mark_gate(request_id, None, retry=True, error=str(error))
                report.notes.append(f"reconcile failed for {request_id}: {error}")
                continue
            complete = self._reads_were_complete(outcome)
            self._mark_gate(
                request_id, fingerprint if complete else None, retry=not complete,
                error=None if complete else "reads incomplete",
            )
            report.reconciled += 1

    @staticmethod
    def _reads_were_complete(outcome) -> bool:
        observation = outcome.get("operationObservation", "")
        scan = outcome.get("recipientScan", "")
        if "unreadable" in observation or "unreadable" in scan:
            return False
        if scan.startswith("not scanned"):
            return outcome.get("evidence") != "none"
        return True

    def _gate(self, attempt):
        """Is there anything new to learn? If not, invoke nothing and write nothing.

        The fingerprint is read FIRST and returned even when the decision was already made by
        the debt flag, because a clean pass has to be able to store it. Without that, the first
        reconciliation leaves no baseline and every later tick looks like a change.
        """
        row = self.store.one(
            "SELECT * FROM reconcile_gate WHERE request_id = ?", (attempt["request_id"],)
        )
        delivery = self.delivery.find(attempt["event_id"])
        fingerprint = None
        try:
            receipt = self.adapter.get_operation(attempt["request_id"])
            status = (receipt or {}).get("status", "missing")
            content = self.adapter.recipient_fingerprint(delivery["recipient_thread_id"])
            fingerprint = f"{status}|{content}"
        except Exception as error:
            # A read that failed is not evidence of anything, so it records a debt and forces
            # reconciliation rather than being cached as a reading.
            self._mark_gate(attempt["request_id"], None, retry=True, error=str(error))
            return True, None
        if row is None or row["retry_required"]:
            # Owed work is recorded, not inferred. A failure last tick means we reconcile now
            # even if every reading looks identical.
            return True, fingerprint
        return fingerprint != row["fingerprint"], fingerprint

    def _mark_gate(self, request_id, fingerprint, *, retry, error) -> None:
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO reconcile_gate (request_id, fingerprint, retry_required, last_error,"
                " updated_at) VALUES (?,?,?,?,?)"
                " ON CONFLICT(request_id) DO UPDATE SET"
                " fingerprint=COALESCE(excluded.fingerprint, reconcile_gate.fingerprint),"
                " retry_required=excluded.retry_required, last_error=excluded.last_error,"
                " updated_at=excluded.updated_at",
                (request_id, fingerprint, int(bool(retry)), error, self.clock.iso()),
            )

    # ---------------------------------------------------------------- deliver

    def _deliver(self, report, now) -> None:
        eligible = self.delivery.eligible(now=now, limit=self.policy.max_sends_per_tick)
        for row in eligible:
            try:
                record = self.delivery.attempt(row["event_id"], self.adapter, now=now)
            except Exception as error:
                report.notes.append(f"delivery refused for {row['event_id']}: {error}")
                continue
            if record is None:
                report.deferred += 1
                continue
            report.delivered += 1
            if row["kind"] != COMPLETION and record["deliveryState"] == DISPATCHED:
                try:
                    self.ack.bind_dispatched_revision(row["event_id"])
                except Exception as error:
                    report.notes.append(f"anchor binding failed: {error}")

    # ----------------------------------------------------------------- state

    def _active_relationships(self) -> list:
        rows = self.store.all(
            "SELECT relationship_id FROM relationships WHERE status = 'active'"
            " AND superseded_by IS NULL"
        )
        return [self.registry.get(row["relationship_id"]) for row in rows]
