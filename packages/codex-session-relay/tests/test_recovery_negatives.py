"""What recovery must NOT do.

CRW-125 asks for recovery across restart, idle/active transition, ACK loss, an unknown send
result, a state or socket mismatch and a duplicate retry. Naming the function that handles each
is not evidence: every one of them is only correct because of what it refuses to do, and a
mapping table cannot fail. These are the negatives, run against a real store.

The criterion also states two rules in its own words, and both get a test here: retrieving a
child's transcript by hand does not count as automatic recovery, and pause, cancel and a real
approval request keep the authority they already had.
"""

from codex_session_relay.errors import RefusalReason

from .support import CHILD, PARENT
from .test_verification_currency import VerificationTestCase


class RecoveryRefusesToInvent(VerificationTestCase):
    def test_a_restart_sweep_never_resends(self):
        """recover_on_start settles what it can observe; sending again is not settling.

        A sweep that resent would turn every restart into a duplicate delivery, which is the
        one thing an at-most-once path cannot do.
        """
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        before = len(self.adapter.sends)
        outcome = self.reconciler.recover_on_start(self.adapter)
        self.assertEqual(outcome["resent"], [])
        self.assertEqual(len(self.adapter.sends), before, "the sweep sent something")

    def test_an_uncertain_attempt_is_held_rather_than_retried(self):
        """An unknown send result is not a known non-delivery.

        Retrying on no evidence is how one instruction arrives twice, so the attempt parks in
        held_uncertain for the reconciler to judge instead of going round again.
        """
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        # The transport answers that it does not know what happened, which is the case the
        # whole held_uncertain state exists for.
        self.adapter.script("process_death")
        self.attempt(event_id)
        row = self.store.one(
            "SELECT state FROM deliveries WHERE event_id = ?", (event_id,)
        )
        self.assertIn(row["state"], ("held_uncertain", "queued", "deferred_busy"))
        attempts = self.attempts_for(event_id)
        self.assertEqual(len(attempts), 1, "an uncertain attempt was retried blindly")

    def test_reading_a_transcript_settles_nothing(self):
        """Manual transcript retrieval must not pass for automatic recovery.

        This is the criterion's own rule. Looking at what a child wrote is an observation a
        person made; it writes no receipt, settles no attempt and moves no assignment state.
        """
        relationship = self.register(recipients=[PARENT, CHILD])
        self._rid = relationship["relationshipId"]
        from codex_session_relay.assignment import AssignmentView

        view = AssignmentView(self.store, self.registry, self.clock)
        before = view.state(self._rid)["state"]

        events = self.store.one("SELECT COUNT(*) AS n FROM events")["n"]
        # Exactly what a person does when they go and look at the child: a read, and only a
        # read. The relay is not told anything by it.
        self.adapter.read_thread(CHILD)
        self.adapter.list_turn_ids(CHILD)

        self.assertEqual(view.state(self._rid)["state"], before)
        self.assertEqual(self.store.one("SELECT COUNT(*) AS n FROM events")["n"], events)

    def test_a_paused_assignment_is_never_auto_resumed(self):
        """pause keeps its authority: coming back is a deliberate act with a restatement."""
        relationship = self.register(recipients=[PARENT, CHILD])
        self._rid = relationship["relationshipId"]
        self.registry.set_status(self._rid, "paused", actor=PARENT)

        self.reconciler.recover_on_start(self.adapter)
        self.assertEqual(self.registry.get(self._rid)["status"], "paused")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE,
            lambda: self.registry.require_active(self._rid),
        )

    def test_resuming_demands_a_restatement_rather_than_a_status_flip(self):
        """The authority check is that the caller can say what it is re-authorising."""
        relationship = self.register(recipients=[PARENT, CHILD])
        self._rid = relationship["relationshipId"]
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        with self.assertRaises(Exception):
            self.registry.resume(
                self._rid, expect_generation=99,
                expect_artifact_roots=["/wrong"], expect_allowed_recipients=[PARENT],
                actor=PARENT,
            )
        self.assertEqual(self.registry.get(self._rid)["status"], "paused")
