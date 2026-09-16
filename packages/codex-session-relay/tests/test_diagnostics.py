"""One word for five situations told an operator nothing about what to do.

withheld_pre_send covered a receipt not yet collected, a parent mid-turn, a host that would
not confirm the authorized settings, a started turn, and an outstanding acknowledgement. The
cause is the only part that suggests an action, and it was the part not recorded.
"""

import json
import unittest

from codex_session_relay.models import TurnRef

from .support import CHILD, PARENT, DeliveryTestCase
from .test_daemon import DaemonTestCase


class Phases(DeliveryTestCase):
    def phase_of(self, event_id):
        for item in self.delivery.snapshot()["deliveries"]:
            if item["eventId"] == event_id:
                return item
        raise AssertionError("no such delivery")

    def test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt(self):
        """A delivery row only exists because the receipt was collected and accepted.

        awaiting_receipt pointed an operator at the child for a delay that is entirely this
        relay's: what is outstanding is reaching the recipient.
        """
        _relationship, event_id = self.queued_event()
        self.assertEqual(self.phase_of(event_id)["phase"], "awaiting_send")

    def test_a_delivery_whose_send_is_in_flight_says_so(self):
        """The claim committed and the process stopped; there IS an attempt to reconcile."""
        from codex_session_relay.delivery import _phase

        row = {"state": "sending", "kind": "completion_event", "hold_reason": None}
        self.assertEqual(_phase(row, [], None), "in_flight")

    def test_a_busy_parent_is_named_as_such_with_its_next_retry(self):
        _relationship, event_id = self.queued_event()
        self.adapter.set_status(PARENT, "active")
        self.attempt(event_id)
        item = self.phase_of(event_id)
        self.assertEqual(item["phase"], "parent_busy")
        self.assertIsNotNone(item["nextRetryAt"], "an operator needs to know when, too")
        self.assertEqual(item["lastFailedOperation"]["operation"], "parent_busy")

    def test_a_dispatched_delivery_is_awaiting_acknowledgement(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        self.assertEqual(self.phase_of(event_id)["phase"], "awaiting_ack")

    def test_a_closed_channel_is_queryable_rather_than_hidden(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("approval_policy")
        self.attempt(event_id)
        item = self.phase_of(event_id)
        self.assertEqual(item["phase"], "channel_closed")
        self.assertIsNotNone(item["lastFailedOperation"])
        self.assertEqual(item["lastFailedOperation"]["error_code"],
                         "unsupported_approval_policy")

    def test_a_settings_rejection_records_the_field_the_host_disagreed_on(self):
        """The difference is dropped by classification, so it is read from the raw receipt."""
        _relationship, event_id = self.queued_event()
        original = self.adapter.send_message

        def rejecting(request_id, thread_id, message, settings=None):
            receipt = original(request_id, thread_id, message, settings)
            receipt.update(
                status="failed",
                error="thread/resume: settings_not_preserved",
                rpcError={"code": "settings_not_preserved", "message": "mismatch"},
                resumed={"approvalPolicy": "onRequest"},
                settingsFindings=[
                    {"field": "approvalPolicy", "expected": "never", "returned": "onRequest"},
                    {"field": "reasoningEffort", "expected": "xhigh", "returned": "low"},
                ],
            )
            self.adapter.ledger[request_id] = receipt
            return dict(receipt)

        self.adapter.send_message = rejecting
        self.attempt(event_id)

        failure = self.phase_of(event_id)["lastFailedOperation"]
        self.assertEqual(failure["operation"], "settings_check")
        self.assertIn("approvalPolicy", failure["difference"])
        self.assertIn("onRequest", failure["difference"])
        self.assertIn("reasoningEffort", failure["difference"],
                      "every mismatched field, not only the first")

    def test_the_diagnosis_survives_reopening_the_database(self):
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.store import Store

        _relationship, event_id = self.queued_event()
        self.adapter.set_status(PARENT, "active")
        self.attempt(event_id)
        path = self.store.path
        self.store.close()
        reopened = Store(path)
        self.addCleanup(reopened.close)
        service = DeliveryService(reopened, self.registry, self.intake, self.clock)
        self.assertTrue(service.failures_for(event_id))


class NoAttemptPhases(Phases):
    """Some refusals happen before an attempt exists, and those had no cause to read."""

    def test_missing_settings_are_named_rather_than_reported_as_awaiting_a_receipt(self):
        """_settings_for refuses before anything is claimed, so there is no attempt record.

        Every other cause reaches an operator through the attempt. This one refused first,
        and status called it awaiting_receipt - which says the relay is waiting on the child
        when the actionable problem is settings nobody recorded.
        """
        _relationship, event_id = self.queued_event(settings=None)

        self.attempt(event_id)

        item = self.phase_of(event_id)
        self.assertEqual(self.delivery.get(event_id)["state"], "withheld_pre_send")
        self.assertEqual(item["attempts"], 0, "nothing was claimed and nothing was sent")
        self.assertNotEqual(item["phase"], "awaiting_receipt")
        self.assertEqual(item["phase"], "withheld:settings_check")
        self.assertIsNotNone(item["lastFailedOperation"])
        self.assertEqual(item["lastFailedOperation"]["operation"], "settings_check")
        self.assertIsNotNone(item["lastFailedOperation"]["next_retry_at"])


class UncertainPhases(unittest.TestCase):
    """Two phases claimed more than the record supports, read straight from _phase.

    Driven directly rather than through a fixture: what is under test is the rule, and a
    transport fixture that happens to classify a receipt one way or another would decide the
    outcome instead of the rule doing it.
    """

    def phase(self, state, record, *, kind="completion_event", failure=None,
              hold_reason=None):
        from codex_session_relay.delivery import _phase

        attempts = [{"internal_state": "settled", "operation_observation": None,
                     "record": json.dumps(record)}]
        row = {"state": state, "kind": kind, "hold_reason": hold_reason}
        return _phase(row, attempts, None, failure)

    def test_a_refused_turn_start_is_not_evidence_that_a_turn_exists(self):
        """turn_accepted was reported from the operation name alone.

        A failed turn/start with no turn id means the call was REFUSED, not that its answer
        was lost, so claiming a turn exists points an operator at a turn nobody can find.
        """
        self.assertEqual(
            self.phase("held_uncertain", {"failedOperation": "turn/start", "turnId": None}),
            "outcome_unknown",
        )

    def test_a_started_turn_whose_answer_was_lost_is_still_turn_accepted(self):
        """A turn id is affirmative evidence, and the branch must keep honouring it."""
        self.assertEqual(
            self.phase("held_uncertain",
                       {"failedOperation": "turn/start", "turnId": "turn-9"}),
            "turn_accepted",
        )

    def test_an_ordinary_resume_failure_is_not_called_a_settings_rejection(self):
        """thread/resume fails for connectivity and internal reasons too.

        Naming those settings_rejected hands an operator a remediation - re-record the
        authorized settings - that cannot possibly work.
        """
        phase = self.phase(
            "withheld_pre_send", {"failedOperation": "thread/resume"},
            failure={"operation": "transport", "error_code": "internal"},
        )
        self.assertNotEqual(phase, "settings_rejected")
        self.assertEqual(phase, "withheld:thread/resume")

    def test_a_real_settings_rejection_still_says_so(self):
        """_settle records settings_check only when the receipt carried field-level findings."""
        self.assertEqual(
            self.phase(
                "withheld_pre_send", {"failedOperation": "thread/resume"},
                failure={"operation": "settings_check", "error_code": "settings_not_preserved"},
            ),
            "settings_rejected",
        )


class RefusedBeforeTheQueue(DeliveryTestCase):
    """The most stuck state in the system was the one status could not show."""

    def test_an_event_refused_at_the_queue_is_visible_in_status(self):
        """It has no deliveries row at all, and snapshot was built only from deliveries.

        A permanently paused or unauthorized assignment therefore had no status entry, no
        phase and no retry time while the daemon went on retrying it.
        """
        relationship, event_id = self.ready_event()
        with self.store.transaction() as db:
            self.delivery.record_intent_in(
                db, event_id, relationship_id=relationship["relationshipId"],
                kind="completion_event", recipient_task_id=PARENT,
                error="relationship_not_active", now=self.clock.now(),
            )

        payload = self.delivery.snapshot()

        self.assertEqual(payload["deliveries"], [], "there is no delivery row, by design")
        self.assertEqual([i["eventId"] for i in payload["pendingIntents"]], [event_id])
        intent = payload["pendingIntents"][0]
        self.assertEqual(intent["phase"], "refused_pre_queue")
        self.assertIn("relationship_not_active", intent["lastError"])
        self.assertIsNotNone(intent["nextRetryAt"])

    def test_a_scoped_status_filters_the_pending_intents_too(self):
        relationship, event_id = self.ready_event()
        with self.store.transaction() as db:
            self.delivery.record_intent_in(
                db, event_id, relationship_id=relationship["relationshipId"],
                kind="completion_event", recipient_task_id=PARENT,
                error="relationship_not_active", now=self.clock.now(),
            )
        scoped = self.delivery.snapshot(relationship_id="rel-someone-else")
        self.assertEqual(scoped["pendingIntents"], [])


class SettledAcknowledgements(unittest.TestCase):
    """A verified rejection is as settled as a verified acceptance."""

    def phase(self, ack):
        from codex_session_relay.delivery import _phase

        row = {"state": "dispatched", "kind": "completion_event", "hold_reason": None}
        return _phase(row, [], ack)

    def test_a_verified_rejection_is_not_reported_as_awaiting_a_receipt(self):
        """Recognising only the accepted case let a rejection fall past every later branch.

        The receipt was delivered and the parent answered; awaiting_receipt says the child
        has produced nothing, which is the opposite of what happened.
        """
        phase = self.phase({"verified": "verified", "accepted": 0})
        self.assertNotEqual(phase, "awaiting_receipt")
        self.assertEqual(phase, "rejected")

    def test_a_verified_acceptance_still_reports_acknowledged(self):
        self.assertEqual(self.phase({"verified": "verified", "accepted": 1}), "acknowledged")

    def test_an_unverified_acknowledgement_settles_nothing(self):
        """Intent is not evidence; an unverified ack must not close the delivery."""
        self.assertEqual(self.phase({"verified": "unverified", "accepted": 1}), "awaiting_ack")


class RevisionPhases(DeliveryTestCase):
    """Contract v1 acknowledges the child-to-parent direction only."""

    def test_a_dispatched_revision_is_not_waiting_for_an_acknowledgement(self):
        """AckService refuses to acknowledge a revision, so awaiting_ack can never clear.

        The child answers a revision request with its next completion receipt. Reporting an
        obligation that nothing is allowed to meet left every dispatched revision looking
        permanently stuck.
        """
        from codex_session_relay import identity

        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="verdict-1",
            criteria=[{"id": "c-1", "verdict": "needs_changes", "note": "missing migration"}],
        )
        revision = self.store.one("SELECT * FROM deliveries WHERE kind = 'revision_request'")
        self.delivery.attempt(revision["event_id"], self.adapter)

        item = [d for d in self.delivery.snapshot()["deliveries"]
                if d["eventId"] == revision["event_id"]][0]

        self.assertEqual(item["state"], "dispatched")
        self.assertEqual(item["phase"], "awaiting_child_receipt")
        self.assertNotEqual(item["phase"], "awaiting_ack")

    def test_a_dispatched_completion_still_awaits_its_acknowledgement(self):
        """The branch must not swallow the direction that really is waiting on an ack."""
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        item = [d for d in self.delivery.snapshot()["deliveries"]
                if d["eventId"] == event_id][0]
        self.assertEqual(item["phase"], "awaiting_ack")


class ObservationHealth(DaemonTestCase):
    def test_a_live_loop_with_nothing_polled_is_not_healthy(self):
        """A live pid was the whole problem in the reproduced incident."""
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.adapter.fail_reads("read_turn")
        self.daemon.tick(now=self.clock.now())

        health = self.delivery.observation_health(now=self.clock.now())
        anchor = health["anchors"][relationship["relationshipId"]]
        self.assertIsNone(anchor["lastPolledAt"], "a failed first read is not a poll")
        self.assertIsNotNone(anchor["lastError"])
        self.assertEqual(health["health"], "stalled")

    def test_a_successful_poll_then_a_failure_keeps_the_last_success(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.daemon.tick(now=self.clock.now())
        first = self.delivery.observation_health(now=self.clock.now())
        polled = first["anchors"][relationship["relationshipId"]]["lastPolledAt"]
        self.assertIsNotNone(polled, "an in-progress turn was still successfully looked at")

        self.adapter.fail_reads("read_turn")
        self.clock.advance(10)
        self.daemon.tick(now=self.clock.now())
        second = self.delivery.observation_health(now=self.clock.now())
        anchor = second["anchors"][relationship["relationshipId"]]
        self.assertEqual(anchor["lastPolledAt"], polled, "a failure is not a success")
        self.assertIsNotNone(anchor["lastError"])

    def test_a_staged_backlog_is_visible_with_its_age(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        path = self.artifact("out.txt", "still going")
        payload = self.ready_payload(
            relationship, [path], turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        )
        self.accept(payload)
        self.clock.advance(7200)

        health = self.delivery.observation_health(now=self.clock.now())

        self.assertEqual(len(health["stagedEvents"]), 1)
        self.assertGreater(health["oldestStagedAgeSeconds"], 3600)
        self.assertEqual(health["backlog"][relationship["relationshipId"]], 1)
        self.assertIn("liveness", health["note"])

    def test_an_anchor_the_scheduler_has_not_reached_is_not_reported_healthy(self):
        """Starting from the poll table would omit it entirely and report healthy."""
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        # No tick at all: nothing has ever looked at this anchor.
        health = self.delivery.observation_health(now=self.clock.now())
        anchor = health["anchors"][relationship["relationshipId"]]
        self.assertIsNone(anchor["lastPolledAt"])
        self.assertEqual(
            health["health"], "stalled",
            "an anchor nothing has read is not evidence of health",
        )

    def test_a_finished_quiet_assignment_does_not_age_into_a_false_alarm(self):
        """_worth_polling stops scheduling a terminal turn with nothing staged behind it.

        Its last poll therefore can never advance again, so ageing it out marked every
        fully observed assignment stalled once stale_after had elapsed - on every status
        call, forever, with nothing wrong.
        """
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.daemon.tick(now=self.clock.now())
        self.adapter.finish_turn(CHILD, "turn-dispatch-1")
        self.daemon.tick(now=self.clock.now())

        anchor_id = relationship["relationshipId"]
        settled = self.delivery.observation_health(now=self.clock.now())
        self.assertTrue(settled["anchors"][anchor_id]["settled"])

        self.clock.advance(7200)
        later = self.delivery.observation_health(now=self.clock.now())

        self.assertGreater(later["anchors"][anchor_id]["ageSeconds"], 900)
        self.assertEqual(
            later["health"], "healthy",
            "nothing is left to learn from this turn, so nothing is being missed",
        )

    def test_a_late_staged_receipt_reopens_the_same_anchor(self):
        """Settled is a property of the work, not a latch. New staged work un-settles it."""
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.daemon.tick(now=self.clock.now())
        self.adapter.finish_turn(CHILD, "turn-dispatch-1")
        self.daemon.tick(now=self.clock.now())

        path = self.artifact("late.txt", "staged after the completion was observed")
        self.accept(self.ready_payload(
            relationship, [path], turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        ))
        self.clock.advance(7200)

        health = self.delivery.observation_health(now=self.clock.now())
        anchor_id = relationship["relationshipId"]

        self.assertFalse(health["anchors"][anchor_id]["settled"])
        self.assertEqual(health["health"], "stalled",
                         "there is staged work here and nothing has looked at it since")

    def test_an_upgraded_store_does_not_forget_what_it_had_already_settled(self):
        """assignment_settlements is new, and the scheduler asks it instead of observations.

        Leaving it empty on upgrade makes every historical turn look unsettled, so a current
        turn that can no longer be read strands a previously settled assignment forever.
        """
        from codex_session_relay.store import Store

        relationship = self.register()
        rid = relationship["relationshipId"]
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.adapter.finish_turn(CHILD, "turn-dispatch-1")
        self.daemon.tick(now=self.clock.now())
        self.assertTrue(self.store.all("SELECT * FROM assignment_settlements"))

        # Exactly the state an upgrade starts from: observations populated, the new table not.
        with self.store.transaction() as db:
            db.execute("DELETE FROM assignment_settlements")
        path = self.store.path
        self.store.close()

        reopened = Store(path)
        self.addCleanup(reopened.close)

        rows = reopened.all("SELECT * FROM assignment_settlements")
        self.assertEqual([row["relationship_id"] for row in rows], [rid])
        self.assertEqual(rows[0]["turn_id"], "turn-dispatch-1")

    def test_each_assignment_settles_a_shared_turn_for_itself(self):
        """observations is keyed by the turn alone, so it can only name who settled it first.

        Every other assignment on a shared child turn therefore looked permanently unsettled,
        was polled again on every round, reported the tick non-quiet and spent observation
        budget forever. assignment_settlements records the per-assignment fact.
        """
        import os

        from codex_session_relay.models import Endpoint
        from codex_session_relay.registry import record_settings
        from .support import HOST, task_settings

        child, turn = "01child-shared", "turn-shared-1"
        made = []
        for name in ("a", "b"):
            parent = f"01parent-{name}"
            root = os.path.join(self.root, name)
            os.makedirs(root, exist_ok=True)
            made.append(self.registry.register(
                parent=Endpoint(parent, HOST, cwd=f"/p/{name}"),
                child=Endpoint(child, HOST, cwd=root),
                issue_key=f"SHARED-{name}", artifact_roots=[root],
                allowed_recipients=[parent],
                dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=turn,
            ))
            self.adapter.add_thread(parent)
            record_settings(self.store, self.clock, parent, task_settings(f"/p/{name}"),
                            source="creation_result")
        self.adapter.add_thread(child)
        self.adapter.start_turn(child, turn_id=turn, status="inProgress")
        self.adapter.finish_turn(child, turn)
        for _tick in range(4):
            self.clock.advance(1)
            self.daemon.tick(now=self.clock.now())

        self.assertEqual(
            len(self.store.all("SELECT * FROM observations")), 1,
            "the turn table is unchanged and still holds one row",
        )
        self.assertEqual(
            {row["relationship_id"]
             for row in self.store.all("SELECT * FROM assignment_settlements")},
            {r["relationshipId"] for r in made},
            "but each assignment has settled it for itself",
        )

        self.clock.advance(7200)
        health = self.delivery.observation_health(now=self.clock.now())
        for relationship in made:
            self.assertTrue(health["anchors"][relationship["relationshipId"]]["settled"])
        self.assertEqual(health["health"], "healthy")

        # And nothing is re-polled: a settled assignment costs no further budget.
        self.clock.advance(1)
        quiet = self.daemon.tick(now=self.clock.now())
        self.assertEqual(quiet.observed, 0, "neither assignment is settled a second time")

    def test_another_assignments_staged_work_does_not_unsettle_this_one(self):
        """staged_here counted by thread and turn, so a shared anchor contaminated both.

        An assignment that has observed its turn and has nothing of its own outstanding was
        pulled back to unsettled by a claim belonging to someone else, and from there it ages
        into a stall with nothing wrong.
        """
        import os

        from codex_session_relay.models import Endpoint
        from codex_session_relay.registry import record_settings
        from .support import HOST, task_settings

        child, turn = "01child-shared", "turn-shared-1"
        made = []
        for name in ("a", "b"):
            parent = f"01parent-{name}"
            root = os.path.join(self.root, name)
            os.makedirs(root, exist_ok=True)
            made.append(self.registry.register(
                parent=Endpoint(parent, HOST, cwd=f"/p/{name}"),
                child=Endpoint(child, HOST, cwd=root),
                issue_key=f"SHARED-{name}", artifact_roots=[root],
                allowed_recipients=[parent],
                dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=turn,
            ))
            self.adapter.add_thread(parent)
            record_settings(self.store, self.clock, parent, task_settings(f"/p/{name}"),
                            source="creation_result")
        self.adapter.add_thread(child)
        self.adapter.start_turn(child, turn_id=turn, status="inProgress")
        self.adapter.finish_turn(child, turn)
        self.daemon.tick(now=self.clock.now())

        observed = self.store.one(
            "SELECT relationship_id FROM observations WHERE turn_id = ?", (turn,),
        )["relationship_id"]
        settled = next(r for r in made if r["relationshipId"] == observed)
        other = next(r for r in made if r["relationshipId"] != observed)
        self.assertTrue(
            self.delivery.observation_health(
                now=self.clock.now(),
            )["anchors"][observed]["settled"],
        )

        # A claim belonging to the OTHER assignment, on the anchor turn they share.
        path = os.path.join(self.root, "other.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("not this assignment's outstanding work")
        self.store.db.execute(
            "INSERT INTO events (event_id, relationship_id, execution_generation, attempt,"
            " revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status,"
            " receipt, stage, staged_at, first_seen_at, last_seen_at)"
            " VALUES ('shared-staged',?,1,1,'x','ready_for_review','child',?,?,'inProgress',"
            " '{}','staged',?,?,?)",
            (other["relationshipId"], child, turn,
             self.clock.iso(), self.clock.iso(), self.clock.iso()),
        )
        del path

        health = self.delivery.observation_health(now=self.clock.now())

        self.assertTrue(
            health["anchors"][observed]["settled"],
            "this assignment observed its turn and has nothing of its own left to resolve",
        )
        del settled

    def test_a_cancelled_assignments_staged_event_does_not_hold_health_down(self):
        """The scheduler drops an inactive assignment and will never settle its claim.

        Ageing that claim anyway left the whole health block degraded indefinitely while
        every active assignment was fine.
        """
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        path = self.artifact("out.txt", "still going")
        self.accept(self.ready_payload(
            relationship, [path], turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        ))
        self.daemon.tick(now=self.clock.now())
        self.clock.advance(7200)
        before = self.delivery.observation_health(now=self.clock.now())
        self.assertEqual(len(before["stagedEvents"]), 1)
        self.assertNotEqual(before["health"], "healthy")

        self.registry.set_status(
            relationship["relationshipId"], "cancelled", actor="test",
        )

        health = self.delivery.observation_health(now=self.clock.now())

        self.assertEqual(health["stagedEvents"], [],
                         "nothing is going to settle this one, so it is not a backlog")
        self.assertEqual(health["health"], "healthy")

    def test_backlog_and_staged_events_agree_after_an_assignment_is_cancelled(self):
        """stagedEvents filtered on the active relationship; backlog read events directly.

        The two fields disagreed the moment an assignment was paused or cancelled: status
        reported work the scheduler will never process.
        """
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        path = self.artifact("out.txt", "still going")
        self.accept(self.ready_payload(
            relationship, [path], turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        ))
        before = self.delivery.observation_health(now=self.clock.now())
        self.assertEqual(len(before["stagedEvents"]), 1)
        self.assertEqual(before["backlog"].get(rid), 1)

        self.registry.set_status(rid, "cancelled", actor="test")

        health = self.delivery.observation_health(now=self.clock.now())

        self.assertEqual(health["stagedEvents"], [])
        self.assertEqual(
            health["backlog"], {},
            "backlog must not report work stagedEvents has already excluded",
        )

    def test_a_generation_whose_anchor_is_not_bound_yet_is_not_a_stall(self):
        """A needs_changes verdict opens a generation before its revision is dispatched.

        Until the dispatch receipt binds the anchor there is no turn for the observation
        scheduler to poll, so counting it as never polled reported stalled for a relay doing
        exactly what it is supposed to do.
        """
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.open_generation(
            rid, dispatch_request_id="revision-1", reason="needs_changes_revision",
        )

        health = self.delivery.observation_health(now=self.clock.now())
        anchor = health["anchors"][rid]

        self.assertTrue(anchor["anchorPending"])
        self.assertIsNone(anchor["turnId"])
        self.assertEqual(
            health["health"], "healthy",
            "there is no turn to poll yet, so nothing is being missed",
        )

    def test_an_unbound_anchor_becomes_pollable_once_it_binds(self):
        """Pending is a phase, not an exemption. Once bound it is held to the same freshness."""
        relationship = self.register()
        rid = relationship["relationshipId"]
        opened = self.registry.open_generation(
            rid, dispatch_request_id="revision-1", reason="needs_changes_revision",
        )
        self.registry.bind_anchor(
            rid, opened["executionGeneration"], dispatch_turn_id="turn-revision-1",
            source="dispatch_receipt",
        )
        self.clock.advance(7200)

        health = self.delivery.observation_health(now=self.clock.now())

        self.assertFalse(health["anchors"][rid]["anchorPending"])
        self.assertEqual(health["health"], "stalled",
                         "now there is a turn, and nothing has ever read it")
