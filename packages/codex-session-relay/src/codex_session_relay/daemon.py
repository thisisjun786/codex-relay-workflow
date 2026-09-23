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
import json
import os
from dataclasses import dataclass, field
from pathlib import Path

from .delivery import COMPLETION, REVISION
from .admission import BOUND_ADMISSION_SQL
from .errors import (
    DeliveryRefused, RefusalReason, RegistrationError, RelayError, ScopeError,
)
from .models import TurnRef
from .policy import RetryPolicy
from .receipts import ObservationOutcome, classify_observation
from .scope import assert_assignment_delivery
from .transport import DEFERRED_BUSY, DISPATCHED, HELD_UNCERTAIN, WITHHELD_PRE_SEND


# Who a claim the daemon makes on a supervisor message belongs to. A claim is owned by its
# attempt number and this name together, so a parent sending by hand beside it cannot settle it.
DAEMON_OWNER = "relay-daemon"


@dataclass
class TickReport:
    observed: int = 0
    reconciled: int = 0
    delivered: int = 0
    deferred: int = 0
    skipped: int = 0
    acksVerified: int = 0
    anchorsBound: int = 0
    requeued: int = 0
    faultsRecorded: int = 0
    supervisorStaged: int = 0
    supervisorSent: int = 0
    quiet: bool = True
    notes: list = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "observed": self.observed, "reconciled": self.reconciled,
            "delivered": self.delivered, "deferred": self.deferred,
            "skipped": self.skipped, "acksVerified": self.acksVerified,
            "anchorsBound": self.anchorsBound,
            "requeued": self.requeued,
            "faultsRecorded": self.faultsRecorded,
            "supervisorStaged": self.supervisorStaged,
            "supervisorSent": self.supervisorSent,
            "quiet": self.quiet, "notes": self.notes,
        }


class SingleInstance:
    """One daemon per state directory. A second one exits rather than racing the first.

    A supervised worker ADOPTS the descriptor its supervisor already holds rather than taking a
    second lock. flock belongs to the open file description, so the supervisor and its worker
    share one and the lock stays held while either of them lives - which is what stops a
    replacement from starting beside an orphaned worker.

    That sharing is also why a shared holder releases by CLOSING and never by LOCK_UN:
    unlocking through any duplicate descriptor releases it for every holder at once.
    """

    def __init__(self, directory, *, shared: bool = False, adopt_fd=None):
        self.path = Path(directory) / "daemon.lock"
        self.shared = bool(shared or adopt_fd is not None)
        self.adopted = adopt_fd is not None
        self._adopt_fd = adopt_fd
        self._handle = None

    def __enter__(self):
        if self._adopt_fd is not None:
            # Already locked by the supervisor through this same description. Re-acquiring
            # would be a second lock on a file we already hold.
            self._handle = os.fdopen(self._adopt_fd, "r+", closefd=True)
            return self
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        self._handle = open(self.path, "a+")
        try:
            fcntl.flock(self._handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as error:
            self._handle.close()
            self._handle = None
            raise RuntimeError(f"another relay daemon already holds {self.path}") from error
        self._handle.seek(0)
        self._handle.truncate()
        self._handle.write(str(os.getpid()))
        self._handle.flush()
        if self.shared:
            os.set_inheritable(self._handle.fileno(), True)
        return self

    def fileno(self):
        return self._handle.fileno() if self._handle is not None else None

    def __exit__(self, *_exc):
        if self._handle is not None:
            if not self.shared:
                fcntl.flock(self._handle, fcntl.LOCK_UN)
            self._handle.close()
            self._handle = None


class RelayDaemon:
    def __init__(self, store, registry, intake, delivery, ack, reconciler, adapter, *,
                 policy=None, clock=None, log=None, faults=None, fault_scope=None,
                 supervisor_channel=None):
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
        # Optional, and absent means the pass does not run. A daemon that could not be given
        # a ledger still ticks exactly as it did.
        self.faults = faults
        self.fault_scope = fault_scope or {}
        # Optional too, and absent means the supervisor pass does not run. Given one, a parent
        # that never stages or sends still has its reports go up (CRW-215).
        self.supervisor_channel = supervisor_channel
        # The last project key the supervisor pass served, where its rotation resumes. In memory
        # on purpose: see _report_upward.
        self._supervisor_after = None
        self._last_refusal = None

    # ------------------------------------------------------------------ tick

    def tick(self, *, now=None) -> TickReport:
        now = self.clock.now() if now is None else now
        report = TickReport()
        self._bind_anchors(report)
        self._observe(report, now)
        self._sweep_faults(report)
        self._requeue_missing(report, now)
        self._reconcile(report, now)
        # Again, because reconciliation is what promotes a held_uncertain revision to
        # dispatched, and binding ran before it. A revision promoted in this tick would
        # otherwise stay anchor_pending until the next one, and a child that emits its
        # completion in that interval has it refused as unbound_generation even though the
        # dispatch evidence is already committed.
        self._bind_anchors(report)
        self._verify_acks(report, now)
        self._deliver(report, now)
        # After the parent-child deliveries, which share each recipient's budget with it: a
        # task that is both a parent and a supervisor hears its children first.
        self._report_upward(report, now)
        report.quiet = not (report.observed or report.reconciled or report.delivered
                            or report.deferred or report.acksVerified or report.anchorsBound
                            or report.requeued or report.faultsRecorded
                            or report.supervisorStaged or report.supervisorSent)
        return report

    def _sweep_faults(self, report) -> None:
        """Record what the store currently shows is broken, so nobody has to notice first.

        Counts only what was NEWLY recorded. A stuck row read again on the next tick produces
        the same occurrence key and records nothing, which is what keeps a steady-state
        failure from making every tick look busy and holding the loop at its fastest cadence
        forever.

        This pass never writes to Linear and never can: it queues what a credential holder
        will write, and the relay holds no credential.
        """
        if self.faults is None:
            return
        try:
            from . import faultsweep

            batch = faultsweep.sweep(self.store, scope=self.fault_scope,
                                     policy=self.policy)
            answer = faultsweep.record_all(self.faults, batch, store=self.store)
            report.faultsRecorded += answer["recorded"]
            for gap in answer["gaps"]:
                report.notes.append(f"fault reading unusable: {gap['reason']}")
        except Exception as error:  # noqa: BLE001 - a tick never dies on one pass
            report.notes.append(f"fault sweep failed: {error}")

    def _bind_anchors(self, report) -> None:
        """Repair any generation left anchor_pending by a dispatch this loop did not make.

        First in the tick on purpose: a receipt arriving during this same tick is then
        accepted rather than refused as unbound.
        """
        try:
            # Added, not assigned. tick() runs this pass twice - once before reconciliation
            # and once after the pass that can promote a revision - and assigning let the
            # second pass erase what the first repaired, so a tick that bound a durable
            # anchor reported anchorsBound 0 and even quiet.
            report.anchorsBound += len(self.ack.bind_pending_anchors())
        except Exception as error:  # noqa: BLE001 - a tick never dies on one pass
            report.notes.append(f"anchor recovery failed: {error}")

    def _requeue_missing(self, report, now) -> None:
        """Queue a final event that has no delivery row.

        A refusal at enqueue time can be perfectly legitimate - a relationship paused between
        selection and queuing - and the observation that produced the event is still true. So
        the obligation is derived from state and retried here, instead of the event being lost
        because the turn it came from will never look new again.
        """
        try:
            candidates = self.delivery.pending_intents(
                now=now, limit=self.policy.max_sends_per_tick,
            )
        except Exception as error:  # noqa: BLE001
            report.notes.append(f"requeue scan failed: {error}")
            return
        for row in candidates:
            event_id = row["event_id"]
            try:
                self.delivery.enqueue(
                    event_id, kind=row["kind"], recipient_task_id=row["recipient_task_id"],
                )
            except Exception as error:  # noqa: BLE001 - still refused; back this one off so
                # it cannot hold a recovery slot against events that would succeed.
                report.notes.append(f"requeue refused for {event_id}: {error}")
                with self.store.transaction() as db:
                    self.delivery.record_intent_in(
                        db, event_id, relationship_id=row["relationship_id"],
                        kind=row["kind"], recipient_task_id=row["recipient_task_id"],
                        error=error, now=now,
                    )
                continue
            # enqueue() clears the intent inside the transaction that inserts the delivery, so
            # there is nothing left to clear here.
            report.requeued += 1

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
        relationships = self._active_relationships()
        if not relationships:
            return
        budget = self.policy.max_turn_reads_per_tick
        # How many relationships this tick can serve properly. Serving ALL of them would mean
        # promising every current anchor a read, which stops being possible the moment the
        # relationship count passes the budget. Rotating which ones are served keeps the
        # promise finite instead of impossible.
        served = max(1, min(len(relationships),
                            budget // max(1, self.policy.min_relationship_share)))
        start = self._cursor("relationships", len(relationships))
        order = [relationships[(start + offset) % len(relationships)]
                 for offset in range(len(relationships))]
        share = max(1, budget // served)
        reads = 0
        for relationship in order[:served]:
            thread = relationship["child"]["taskId"]
            for turn_id in self._turns_to_poll(relationship, share):
                if reads >= budget:
                    break
                reads += 1
                try:
                    turn = self.adapter.read_turn(thread, turn_id)
                except Exception as error:
                    report.notes.append(f"turn read failed for {turn_id}: {error}")
                    self._record_poll(relationship, turn_id, status=None, error=error)
                    continue
                self._record_poll(
                    relationship, turn_id,
                    status=turn.status if turn is not None else "absent",
                    # An absent turn is not a successful poll. Recording it as one refreshed
                    # last_polled_at on every tick, and observation_health reads only poll
                    # freshness and settlement - so an anchor the host says is gone, which can
                    # never settle, reported healthy forever.
                    error=None if turn is not None else "the host reports this turn absent",
                )
                if turn is None or turn.status not in ("completed", "failed", "interrupted"):
                    continue
                reference = TurnRef(thread, turn.turn_id, turn.status)
                # Already observed is not already finished. A receipt written just after the
                # completion was seen still has to be resolved, so the observation alone is
                # no longer enough to skip the turn.
                if self._already_observed(
                    reference, relationship["relationshipId"],
                ) and not self.intake.staged_events(
                    thread_id=thread, turn_id=turn_id,
                    relationship_id=relationship["relationshipId"],
                ):
                    continue
                self._settle_turn(relationship, reference, report)
        # Advanced whether or not anything was read. Advancing only on a read would let a
        # window of relationships with nothing to do pin the cursor, and every relationship
        # behind them would wait forever - the same starvation one level up.
        self._advance_cursor("relationships", served, len(relationships))

    def _turns_to_poll(self, relationship, share: int) -> list:
        """The current anchor, plus a rotating slice of everything else worth reading.

        The old version collected every anchor oldest-first, sliced to the budget, and left
        _observe to discard the already-observed ones AFTER the slice. Past eight generations
        that slice was permanently the first eight, all of them already observed, and the
        current generation was never selected again - which is how a live daemon inside its
        time bound delivered nothing for JUN-100 generation 11.

        So candidates are filtered BEFORE the budget, the current anchor is reserved, and the
        rest rotate through a persistent cursor so a backlog larger than the share is covered
        in a finite number of ticks rather than re-read from the same end every time.
        """
        rid = relationship["relationshipId"]
        thread = relationship["child"]["taskId"]
        current = None
        history = []
        for generation in relationship["generations"]:
            turn_id = generation["dispatchTurnId"]
            if not turn_id:
                continue
            if generation["executionGeneration"] == relationship["executionGeneration"]:
                current = turn_id
            else:
                history.append(turn_id)
        # Scoped to THIS assignment. A child thread can serve several, and collecting their
        # staged turns together put a paused assignment's work into an active assignment's
        # ring and let one assignment settle another's claim.
        staged = [
            row["turn_id"]
            for row in self.intake.staged_events(thread_id=thread, relationship_id=rid)
        ]
        # An explicitly admitted business or continuation turn may end without
        # reporting. It has no staged event, so polling only anchors and receipts
        # would never settle that omission. Include this assignment's admissions
        # from every generation, like historical anchors, until they settle.
        page, ceiling = self._admission_page(rid, thread, max(1, share - 1))
        admitted = [row["turn_id"] for row in page if row["eligible"]]
        ring = [
            turn_id for turn_id in dict.fromkeys(staged + admitted + history)
            if turn_id != current and self._worth_polling(thread, turn_id, rid)
        ]
        selected = []
        if current and self._worth_polling(thread, current, rid):
            selected.append(current)
        remaining = share - len(selected)
        if remaining <= 0 and ring and self._alternate(rid):
            # A share of one cannot give the anchor and the ring a read in the same tick, so
            # it alternates between them. Advancing the ring cursor without reading its
            # candidate would be skipping work, not scheduling it.
            selected, remaining = [], 1
        if remaining > 0 and ring:
            taken = min(remaining, len(ring))
            start = self._cursor(f"ring:{rid}", len(ring))
            selected.extend(ring[(start + offset) % len(ring)] for offset in range(taken))
            self._advance_cursor(f"ring:{rid}", taken, len(ring))
        # Commit only the consumed prefix. Moving past an eligible candidate that
        # lost this tick's ring slot would silently starve it behind later pages.
        consumed = None
        for row in page:
            turn = row["turn_id"]
            if row["eligible"] and turn != current and turn not in selected:
                break
            consumed = row["admission_row"]
        if consumed is not None:
            with self.store.transaction() as db:
                db.execute(
                    "INSERT INTO discovery_cursors (task_id,listing,cursor,updated_at)"
                    " VALUES ('scheduler',?,?,?) ON CONFLICT(task_id,listing) DO UPDATE"
                    " SET cursor=excluded.cursor,updated_at=excluded.updated_at",
                    (f"admitted:{rid}", json.dumps({"after": consumed, "through": ceiling}), self.clock.iso()),
                )
        return selected

    def _admission_page(self, rid, thread, limit):
        """Bound raw work, including settled and foreign admissions.

        Writers only INSERT or UPDATE this table; rowid fixes insertion order even
        when a new turn sorts before an old turn lexically. A frozen upper rowid
        prevents ongoing inserts from extending the current pass. The rowid range
        scans at most limit rows before filtering ownership/eligibility; searching
        for limit eligible rows first would scan unlimited settled history.
        This trades latency across a large shared store for a fixed per-tick cost.
        """
        row = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE task_id='scheduler' AND listing=?",
            (f"admitted:{rid}",),
        )
        key, ceiling = 0, None
        try:
            saved = json.loads(row["cursor"]) if row else {}
            if type(saved.get("after")) is int and type(saved.get("through")) is int:
                key, ceiling = saved["after"], saved["through"]
        except (ValueError, TypeError, AttributeError):
            pass
        if ceiling is None or key >= ceiling:
            key = 0
            ceiling = self.store.one("SELECT COALESCE(MAX(rowid),0) AS last FROM generation_turns")["last"]
        sql = (
            "SELECT t.rowid AS admission_row,t.turn_id,"
            " CASE WHEN t.relationship_id=? THEN"
            " (EXISTS (SELECT 1 FROM generations g WHERE g.relationship_id=t.relationship_id"
            " AND g.execution_generation=t.execution_generation AND " + BOUND_ADMISSION_SQL + ")"
            " AND NOT EXISTS (SELECT 1 FROM assignment_settlements s"
            " WHERE s.relationship_id=t.relationship_id AND s.thread_id=? AND s.turn_id=t.turn_id))"
            " ELSE 0 END AS eligible FROM generation_turns t"
            " WHERE t.rowid > ? AND t.rowid <= ? ORDER BY t.rowid LIMIT ?"
        )
        page = self.store.all(sql, (rid, thread, key, ceiling, limit))
        if not page and key:
            page = self.store.all(sql, (rid, thread, 0, ceiling, limit))
        return page, ceiling

    def _record_poll(self, relationship, turn_id, *, status, error) -> None:
        """That we LOOKED, which an observations row cannot tell anyone.

        observations records terminal turns only, so a healthy long-running anchor has
        no entry there at all and would read as stale forever. A failed read updates the
        attempt time but never the success time: an anchor whose first read failed has
        never been polled, and saying otherwise is the one lie that matters here.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO poll_observations (relationship_id, execution_generation,"
                " turn_id, last_status, last_polled_at, last_attempt_at, last_error)"
                " VALUES (?,?,?,?,?,?,?)"
                " ON CONFLICT(relationship_id, execution_generation, turn_id) DO UPDATE"
                "   SET last_status = excluded.last_status,"
                "       last_polled_at = COALESCE(excluded.last_polled_at,"
                "                                 poll_observations.last_polled_at),"
                "       last_attempt_at = excluded.last_attempt_at,"
                "       last_error = excluded.last_error",
                (relationship["relationshipId"], relationship["executionGeneration"],
                 turn_id, status, None if error else now, now,
                 None if error is None else f"{type(error).__name__}: {error}"),
            )

    def _worth_polling(self, thread, turn_id, relationship_id=None) -> bool:
        """Is there anything left to learn from this turn, for THIS assignment?

        Scoped for the same reason _already_observed is: two assignments can share a child
        turn, and asking globally meant one assignment's observation made the turn look
        finished to the other, which then never settled it at all.
        """
        if self.intake.staged_events(
            thread_id=thread, turn_id=turn_id, relationship_id=relationship_id,
        ):
            return True
        if relationship_id is not None:
            return self.store.one(
                "SELECT 1 FROM assignment_settlements WHERE thread_id = ? AND turn_id = ?"
                "   AND relationship_id = ?",
                (thread, turn_id, relationship_id),
            ) is None
        return self.store.one(
            "SELECT 1 FROM observations WHERE thread_id = ? AND turn_id = ?",
            (thread, turn_id),
        ) is None

    def _cursor(self, listing: str, size: int) -> int:
        if size <= 0:
            return 0
        row = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = ?",
            (listing,),
        )
        try:
            return int(row["cursor"]) % size if row and row["cursor"] is not None else 0
        except (TypeError, ValueError):
            return 0

    def _advance_cursor(self, listing: str, by: int, size: int) -> None:
        """Persisted, so a restart resumes the rotation instead of starting from one end."""
        if size <= 0:
            return
        position = (self._cursor(listing, size) + max(1, by)) % size
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at)"
                " VALUES ('scheduler',?,?,?)"
                " ON CONFLICT(task_id, listing) DO UPDATE SET cursor = excluded.cursor,"
                " updated_at = excluded.updated_at",
                (listing, str(position), self.clock.iso()),
            )

    def _alternate(self, rid: str) -> bool:
        """Toggle whose turn it is when the share is one."""
        listing = f"alt:{rid}"
        turn = self._cursor(listing, 2)
        self._advance_cursor(listing, 1, 2)
        return turn == 1

    def _already_observed(self, reference: TurnRef, relationship_id=None) -> bool:
        """Per assignment, because two assignments can legitimately share a child turn.

        Asking globally meant the first assignment's settlement closed the turn for every
        other one: the second never reached _synthesize, so a failed shared turn left its
        other parents with no terminal outcome at all.
        """
        if relationship_id is not None:
            return self.store.one(
                "SELECT 1 FROM assignment_settlements WHERE thread_id = ? AND turn_id = ?"
                " AND terminal_status = ? AND relationship_id = ?",
                (reference.thread_id, reference.turn_id, reference.turn_status,
                 relationship_id),
            ) is not None
        return self.store.one(
            "SELECT 1 FROM observations WHERE thread_id = ? AND turn_id = ?"
            " AND terminal_status = ?",
            (reference.thread_id, reference.turn_id, reference.turn_status),
        ) is not None

    def _settle_turn(self, relationship, reference, report) -> None:
        """Finalize, record and queue as ONE commit, with a rule for each kind of failure.

        Recording the observation first and queuing after is what lost events: a refusal at
        the queue left a final event with no delivery row, and the next tick skipped the turn
        because it had already been observed.

        A DURABLE refusal - a paused relationship, an unauthorized recipient - is a legitimate
        answer, so the observation stands and _requeue_missing picks the event up once the
        refusal no longer applies. Anything else is transient and nothing is known, so the
        whole transaction rolls back and the next tick re-observes cleanly.
        """
        outcome = classify_observation(reference.turn_status, None)
        synthesized, failed = self._synthesize(relationship, reference, report)
        if failed:
            # Recording the observation now would bury the failure: the turn would never look
            # new again, the staged claim would be suppressed, and nothing would be left for
            # recovery to find. Leave the turn untouched and try again next tick.
            return
        try:
            self._commit_settlement(relationship, reference, outcome, synthesized, queue=True)
        except (DeliveryRefused, ScopeError, RegistrationError) as refusal:
            report.notes.append(f"enqueue refused for {reference.turn_id}: {refusal}")
            self._last_refusal = refusal
            self._commit_settlement(
                relationship, reference, outcome, synthesized, queue=False,
            )
        except Exception as error:  # noqa: BLE001 - transient: keep nothing, retry next tick
            report.notes.append(f"settlement rolled back for {reference.turn_id}: {error}")
            return
        report.observed += 1

    def _synthesize(self, relationship, reference, report):
        """An execution-only receipt for a turn that failed with no claim of its own.

        Written before the settlement transaction because it is a durable fact in its own
        right and opens its own writes. If queuing it then fails, _requeue_missing finds it,
        which is why storing it separately does not lose it.
        """
        if reference.turn_status not in ("failed", "interrupted"):
            return None, False
        # A staged claim on this turn is no reason to skip: a failed or interrupted ending
        # SUPPRESSES that claim rather than finalizing it, so without a synthesized receipt
        # the parent is left waiting on a verdict that can never arrive.
        try:
            return self.intake.daemon_observation(
                relationship["relationshipId"], reference,
            )["eventId"], False
        except RelayError as refusal:
            if refusal.reason == RefusalReason.RELATIONSHIP_NOT_ACTIVE:
                # NOT a decision about this turn. The scheduler selected an active assignment
                # and the relationship paused while the host read was in flight, so the pause
                # says nothing about what the turn did. Recording a settlement here would
                # retire the turn - _worth_polling drops it - while the only carrier of the
                # outcome, this synthesized receipt, was never written. A resume would then
                # find nothing left to observe and the parent would wait forever. So this is
                # transient like any other: keep nothing and look again once it is active.
                report.notes.append(
                    f"observation deferred, {relationship['relationshipId']} is not active:"
                    f" {refusal}"
                )
                return None, True
            # Any other refusal IS a decision - this daemon may not assert anything about that
            # turn - so settlement proceeds and records what it did observe.
            report.notes.append(f"daemon observation refused: {refusal}")
            return None, False
        except Exception as error:  # noqa: BLE001 - transient: nothing is known yet
            report.notes.append(f"daemon observation failed: {error}")
            return None, True

    def _commit_settlement(self, relationship, reference, outcome, synthesized, *, queue):
        with self.store.transaction() as db:
            # This assignment's claims only. Settling every claim on a shared child's turn
            # suppressed the other assignments' events without synthesizing their receipts.
            resolved = self.intake.resolve_staged_in(
                db, reference, relationship["relationshipId"],
            )
            self.intake.record_observation_in(
                db, reference, outcome, relationship_id=relationship["relationshipId"],
                event=synthesized,
            )
            queueable = list(resolved["finalized"])
            if synthesized:
                queueable.append(synthesized)
            for event_id in queueable:
                # Ownership comes from the EVENT, never from the relationship we happened to
                # be polling. Staged claims are selected by thread and turn, and two
                # assignments can share a child, so assuming the polled relationship would
                # queue B's event to A's parent.
                owner = self.intake.row(event_id)["relationship_id"]
                # This event has just become final, so anything of its generation that was
                # already in flight is no longer what the generation stands on. Done in the
                # same transaction that finalized it, so the two facts cannot disagree.
                self.delivery.annotate_predecessors_in(db, event_id)
                if not queue:
                    # Delivery WAS wanted here. Recording that is what lets recovery retry
                    # this event and only this event, instead of guessing from the absence
                    # of a delivery row.
                    self.delivery.record_intent_in(
                        db, event_id, relationship_id=owner, kind=COMPLETION,
                        recipient_task_id=self.registry.get(owner)["parent"]["taskId"],
                        error=self._last_refusal, now=self.clock.now(),
                    )
                    continue
                # enqueue_in does not validate and enqueue does, so the authorization the old
                # path got for free has to be asked for here, inside the same transaction.
                record = self.registry.require_active(owner)
                recipient = record["parent"]["taskId"]
                assert_assignment_delivery(
                    record, kind=COMPLETION, recipient_task_id=recipient,
                    event_relationship_id=owner,
                )
                # Storing a receipt is not telling anyone. A parent waiting for a verdict has
                # to learn that the child failed, so a synthesized observation is queued like
                # any other event.
                self.delivery.enqueue_in(
                    db, event_id, relationship_id=owner, kind=COMPLETION,
                    recipient_task_id=recipient,
                )

    # ------------------------------------------------------------- reconcile

    def _reconcile(self, report, now) -> None:
        budget = self.policy.max_reconciles_per_tick
        parents = self.reconciler.open_parents()
        if not parents:
            return
        # Same starvation, one layer over. Slicing a global prefix meant one parent's
        # unchanged attempts occupied every reconciliation slot - and a gate skip still
        # consumed its place - so another parent's revision never reached dispatched and its
        # anchor never bound.
        cursor = self._cursor("reconcile_parents", len(parents))
        order = parents[cursor:] + parents[:cursor]
        self._advance_cursor("reconcile_parents", 1, len(parents))
        share = max(1, budget // len(order))
        queues = [list(self._attempts_for(parent, share)) for parent in order]
        dealt = []
        while len(dealt) < budget and any(queues):
            for queue in queues:
                if len(dealt) >= budget:
                    break
                if queue:
                    dealt.append(queue.pop(0))
        # Advanced by what was actually DEALT, never by what was merely selected. Advancing
        # inside the selection moved a parent's cursor past attempts this tick then dropped
        # on the budget, and with more parents than budget the parent rotation and the
        # attempt cursors stepped over the same attempts together - permanently, which is
        # the starvation the per-parent cursor was added to remove.
        for parent in order:
            taken = sum(1 for row in dealt if row["parent_task_id"] == parent)
            if taken:
                self._advance_cursor(
                    f"reconcile:{parent}", taken,
                    self.reconciler.open_attempt_count(parent),
                )
        for attempt in dealt:
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

    def _attempts_for(self, parent, share) -> list:
        """One parent's slice, taken from a rotating position rather than the head.

        The parent order already has a cursor; the attempts inside a parent did not. A
        parent with more unresolved attempts than its share re-read the same leading ones
        every tick, and because _gate skips an attempt whose fingerprint is unchanged while
        it still holds its place, the ones behind them were never reconciled at all - so a
        later revision could sit unresolved and its anchor never bind.
        """
        total = self.reconciler.open_attempt_count(parent)
        if not total:
            return []
        want = min(share, total)
        start = self._cursor(f"reconcile:{parent}", total)
        taken = list(self.reconciler.open_attempts(
            limit=want, parents=[parent], offset=start,
        ))
        if len(taken) < want:
            # Wrapped past the end, so the remainder comes from the front. Without this a
            # cursor near the end would return a short slice and waste the budget.
            seen = {row["request_id"] for row in taken}
            for row in self.reconciler.open_attempts(limit=want, parents=[parent]):
                if len(taken) >= want:
                    break
                if row["request_id"] not in seen:
                    taken.append(row)
        return taken

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
        parents = self.delivery.eligible_parents(now=now)
        if not parents:
            return
        cursor = self._cursor("delivery_parents", len(parents))
        # Where each parent's own window STARTS. The parent rotation decides who goes first;
        # without this the window inside a parent was always its oldest rows, so a delivery
        # that raises before changing its own state - and therefore stays eligible and stays
        # oldest - blocked every later delivery for that parent on every subsequent tick.
        totals = {parent: self.delivery.eligible_count(parent, now=now) for parent in parents}
        offsets = {
            parent: self._cursor(f"deliver:{parent}", totals[parent])
            for parent in parents if totals[parent]
        }
        eligible = self.delivery.eligible(
            now=now, limit=self.policy.max_sends_per_tick, cursor=cursor, offsets=offsets,
        )
        # Moved on by ONE position after every window, whatever the outcomes were. Every
        # eligible parent is dealt from, so the rotation is not about who is included - it
        # decides who goes FIRST, and therefore who gets the odd slot when the budget does
        # not divide evenly. Advancing by the parent count would wrap to the same head and
        # hand that slot to the same parent forever.
        self._advance_cursor("delivery_parents", 1, len(parents))
        struggling = set()
        attempted = {}
        for row in eligible:
            parent = row["parent_task_id"]
            if parent in struggling:
                # Skipped for the REST OF THIS TICK only. It reserves no capacity, opens no
                # attempt and creates no hold, so the next tick reconsiders this parent
                # normally; it simply cannot spend the whole budget failing.
                report.skipped += 1
                continue
            attempted[parent] = attempted.get(parent, 0) + 1
            try:
                record = self.delivery.attempt(row["event_id"], self.adapter, now=now)
            except Exception as error:
                report.notes.append(f"delivery refused for {row['event_id']}: {error}")
                struggling.add(parent)
                continue
            if record is None:
                # A busy parent or a withheld send is a returned outcome, not an exception,
                # and it is exactly the case that used to consume a whole tick.
                struggling.add(parent)
                report.deferred += 1
                continue
            if record["deliveryState"] in (HELD_UNCERTAIN, DEFERRED_BUSY, WITHHELD_PRE_SEND):
                struggling.add(parent)
            if record.get("withheldReason"):
                # Looked at, decided and persisted. Counting it as skipped would let a tick
                # that actually withheld a delivery still report itself quiet, and a tick's
                # own quiet flag is part of the evidence a parent reads before it agrees to
                # wait idle on this service.
                report.deferred += 1
                continue
            if record.get("sendAttempted") == "no":
                # Suppressed before any transport call. Counting it as delivered reports a
                # delivery that never reached the recipient, which is the opposite of what
                # this counter is read for.
                report.skipped += 1
            else:
                report.delivered += 1
            # Only a revision anchors a generation. Expressed as "not a completion" this was
            # the same set while there were two kinds; with a third it would try to bind an
            # anchor for a merge-turn grant, which opens no generation and has none to bind.
            if row["kind"] == REVISION and record["deliveryState"] == DISPATCHED:
                try:
                    self.ack.bind_dispatched_revision(row["event_id"])
                except Exception as error:
                    report.notes.append(f"anchor binding failed: {error}")

        # Advanced by what was ATTEMPTED, never by what was selected. A row the budget
        # dropped, or one skipped because its parent was already struggling, was never looked
        # at - moving the cursor past it is how the reconcile path previously skipped work
        # permanently. Wrapping on the count taken before the tick keeps the window inside a
        # parent moving without ever stepping over an unread row.
        for parent, taken in attempted.items():
            if taken and totals.get(parent):
                self._advance_cursor(f"deliver:{parent}", taken, totals[parent])

    # ----------------------------------------------------------- upward

    def _report_upward(self, report, now) -> None:
        """What each project owes the level above, staged and sent with nobody asking.

        The same two steps a parent takes by hand - staging and SupervisorChannel.attempt - so
        every rule the channel enforces holds here unchanged: one obligation is one message,
        what goes out is re-derived where the transport starts (I-247), the recipient's budget
        is shared with parent-child traffic, a paused, archived or unreachable supervisor is
        withheld rather than woken and keeps the obligation, and a message another caller is
        sending is not claimable. A parent that also stages or sends by hand converges on the
        same ids and cannot cause a second wake.

        An omission - a turn that ended with no report - is staged here too, from what this
        store derives (SupervisorChannel.store_readings): the declarations the child's own relay
        recorded beside its marker, read through the same predicate reporting-show uses, once
        the grace has passed. The daemon still reads no marker file. A child that claimed
        without recording that it writes declarations here is a legacy admission and nothing is
        derived for it.

        Bounded in projects and sends like every pass here: max_supervisor_projects_per_tick
        project keys are read and staged and max_supervisor_sends_per_tick messages attempted per
        tick, and a project whose messages have all gone out costs reads and no write. Within a
        project, staging reads the project's whole history - the read supervisor-standing makes -
        so one project's length is not bounded here.
        """
        channel = self.supervisor_channel
        if channel is None:
            return
        cap = self.policy.max_supervisor_projects_per_tick
        if cap <= 0:
            window = []
        else:
            # A page of project keys after the last one served, wrapping to the start: the cap
            # bounds what is READ, not only what is staged, so a store with many projects costs
            # one bounded query per tick. The position is kept in this process's memory rather
            # than in discovery_cursors, because a durable cursor was a write on every tick,
            # owed or not; losing it on a restart changes only where the next rotation starts.
            listed = ("SELECT DISTINCT project_key FROM relationship_scope"
                      " WHERE project_key IS NOT NULL")
            try:
                if self._supervisor_after is None:
                    window = [row["project_key"] for row in self.store.all(
                        listed + " ORDER BY project_key LIMIT ?", (cap,))]
                else:
                    window = [row["project_key"] for row in self.store.all(
                        listed + " AND project_key > ? ORDER BY project_key LIMIT ?",
                        (self._supervisor_after, cap))]
                    if len(window) < cap:
                        window += [row["project_key"] for row in self.store.all(
                            listed + " AND project_key <= ? ORDER BY project_key LIMIT ?",
                            (self._supervisor_after, cap - len(window)))
                            if row["project_key"] not in window]
            except Exception as error:  # noqa: BLE001 - a tick never dies on one pass
                report.notes.append(f"supervisor pass could not list projects: {error}")
                return
            if window:
                self._supervisor_after = window[-1]
        for project in window:
            try:
                answer = channel.stage_unsent(project)
            except Exception as error:  # noqa: BLE001
                report.notes.append(f"supervisor staging failed for {project}: {error}")
                continue
            for one in answer["staged"]:
                if one.get("staged") or one.get("readdressed") or one.get("restated"):
                    report.supervisorStaged += 1
        self._send_upward(channel, report, now)

    def _send_upward(self, channel, report, now) -> None:
        from .supervisorchannel import CLAIMABLE, SENDING

        budget = self.policy.max_supervisor_sends_per_tick
        if budget <= 0:
            return
        # Each recipient's OLDEST eligible message, oldest first. The claim lets only that one go
        # anyway, and reading every eligible row let one recipient's backlog - withheld again on
        # every recheck - fill the whole window each tick, so a report to anybody else was never
        # read at all. A claim whose lease ran out is eligible, because attempting it is what
        # recovers it: queued again when its transport never started, held uncertain when it may
        # have.
        eligible = ("((%(m)s.state IN (?,?,?) AND %(m)s.hold_reason IS NULL"
                    "  AND (%(m)s.next_eligible_at IS NULL OR %(m)s.next_eligible_at <= ?))"
                    " OR (%(m)s.state = ? AND %(m)s.lease_until IS NOT NULL"
                    "     AND %(m)s.lease_until <= ?))")
        rows = self.store.all(
            "SELECT m.message_id, m.recipient_task_id FROM supervisor_messages m"
            " WHERE " + eligible % {"m": "m"}
            + "   AND NOT EXISTS (SELECT 1 FROM supervisor_messages o"
              "                    WHERE o.recipient_task_id = m.recipient_task_id"
              "                      AND " + eligible % {"m": "o"}
            + "                      AND (o.staged_at < m.staged_at OR (o.staged_at ="
              "                           m.staged_at AND o.message_id < m.message_id)))"
              " ORDER BY m.staged_at, m.message_id LIMIT ?",
            (*CLAIMABLE, now, SENDING, now, *CLAIMABLE, now, SENDING, now, budget * 4))
        struggling = set()
        attempted = 0
        for row in rows:
            if attempted >= budget:
                break
            recipient = row["recipient_task_id"]
            if recipient in struggling:
                report.skipped += 1
                continue
            attempted += 1
            try:
                record = channel.attempt(row["message_id"], self.adapter, now=now,
                                         owner=DAEMON_OWNER)
            except Exception as error:  # noqa: BLE001 - a refusal is an answer, not a crash
                report.notes.append(
                    f"supervisor report {row['message_id']} not sent: {error}")
                struggling.add(recipient)
                # Not quiet: the attempt may have held the message (hierarchy_unresolved) on
                # its way out, and the deferral below writes. Counted where nothing was sent.
                report.deferred += 1
                # Out of the head of the queue for a recheck, so a message that faults on every
                # attempt cannot spend every tick's budget ahead of the reports behind it.
                try:
                    channel.defer_after_fault(row["message_id"], now, error)
                except Exception as deferral:  # noqa: BLE001
                    report.notes.append(
                        f"supervisor report {row['message_id']} not deferred: {deferral}")
                continue
            if record is None:
                # Busy, withheld, paced or recovered: nothing went out, and the row says when
                # it may be tried again.
                struggling.add(recipient)
                report.deferred += 1
                continue
            if record["deliveryState"] == DISPATCHED:
                report.supervisorSent += 1
            else:
                struggling.add(recipient)
                report.deferred += 1

    # ----------------------------------------------------------------- state

    def _active_relationships(self) -> list:
        rows = self.store.all(
            "SELECT relationship_id FROM relationships WHERE status = 'active'"
            " AND superseded_by IS NULL"
        )
        return [self.registry.get(row["relationship_id"]) for row in rows]
