"""Registration under contention: a coordinator that stops, and two that race.

The states a registration passes through are already covered one at a time in
test_intent.py, and what a half-registered assignment does to a Stop decision is covered
in test_guard.py. What neither covers is the sequence: a decision taken while the
creation response is still outstanding, and then the bind landing afterwards. The
coordinator keeps a pending observation for exactly that fold, and nothing asserted it
survived the bind.

The two contended cases use the construction test_registry.py already established - one
independent Store per thread, a barrier, joins with a timeout, and errors collected rather
than raised in a thread nobody is watching. What a barrier buys is honest to state: it
aligns the two starts, and the scheduler may still run either call to completion before the
other enters the contested region. So these assert invariants that must hold under EVERY
interleaving, including both serial ones - a registration publishes exactly when it was not
refused as stale, and two binds settle as one winner and one recorded conflict. Overlap
widens the set of executions reached; it is not something the assertions depend on, and
claiming it were guaranteed would be claiming more than the construction can deliver.

Criterion 4 of this work is met entirely by evidence that already exists and is named in
docs/contention-regression.md; nothing here restates it.
"""

import json
import threading
import unittest
from pathlib import Path

from codex_session_relay import guard, intent, marker
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RefusalReason, RegistrationError
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store

from .support import CHILD, DISPATCH_TURN, HOST, PARENT
from .test_guard import DISPATCH, NOW, GuardTestCase


def hold_files(directory) -> list:
    """Every hold reservation under one assignment, which is what the bounds count."""
    return sorted(Path(directory).glob("hook/*/*/hold.json"))


def observations(directory, session_id, turn_id) -> list:
    """The numbered observation records for one turn, oldest first."""
    folder = Path(directory) / "hook" / session_id / turn_id
    paths = sorted(
        (path for path in folder.glob("*.json") if path.stem.isdigit()),
        key=lambda path: int(path.stem),
    )
    return [json.loads(path.read_text(encoding="utf-8")) for path in paths]


class ALateBindFoldsIntoOneDecision(GuardTestCase):
    """The pre-bind window is kept for a fold. This is the fold actually happening."""

    def test_a_bind_arriving_after_an_unbound_release_holds_once_and_keeps_the_first_record(self):
        relationship = self.register()
        self.declare()
        self.claim()

        before = self.evaluate()

        self.assertEqual(before["observation"], "correlated_unbound")
        self.assertEqual(before["decision"], guard.RELEASE)
        self.assertEqual(
            before["record"]["pendingObservation"], "undeclared_turn_end",
            "the pre-bind window recorded nothing to fold, so the turn it covered is lost",
        )

        # The creation response arrives late. Everything the coordinator could not publish
        # before now lands at once, which is the ordinary shape of a lost-then-recovered
        # response rather than an exceptional one.
        self.bind()
        self.register_marker(relationship)

        after = self.evaluate()

        self.assertEqual(after["observation"], "undeclared_turn_end")
        self.assertEqual(after["decision"], guard.BLOCK)

        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        self.assertEqual(
            len(hold_files(directory)), 1,
            "the release before the bind spent a hold, so the turn has no budget left for the"
            " decision that actually needed one",
        )
        kept = observations(directory, CHILD, DISPATCH_TURN)
        self.assertEqual(len(kept), 2, kept)
        self.assertEqual(kept[0]["pendingObservation"], "undeclared_turn_end")
        self.assertFalse(kept[0]["held"])
        self.assertTrue(kept[1]["held"])
        self.assertNotIn(
            "pendingObservation", kept[1],
            "a decision taken on a bound identity is not a pending one",
        )


class RegistrationRacesTheGenerationItNames(GuardTestCase):
    """Registration is confirmed against the relay, and the relay can move underneath it."""

    def register_in_thread(self, relationship_id, results, errors, barrier):
        def run():
            try:
                barrier.wait(timeout=20)
                results["register"] = self.register_marker(
                    {"relationshipId": relationship_id}
                )
            except RegistrationError as error:
                errors["register"] = error
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors["register"] = error

        return threading.Thread(target=run)

    def advance_in_thread(self, relationship_id, errors, barrier):
        def run():
            store = Store(self.store.path)
            try:
                barrier.wait(timeout=20)
                Registry(store, FakeClock()).open_generation(
                    relationship_id, dispatch_request_id="revision-race",
                    reason="needs_changes_revision", dispatch_turn_id="turn-race",
                )
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors["advance"] = error
            finally:
                store.close()

        return threading.Thread(target=run)

    def test_a_registration_racing_an_advance_either_publishes_or_is_refused_but_never_both(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.declare()
        self.claim()
        self.bind()

        results, errors = {}, {}
        barrier = threading.Barrier(2)
        threads = [
            self.register_in_thread(rid, results, errors, barrier),
            self.advance_in_thread(rid, errors, barrier),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        for thread in threads:
            self.assertFalse(thread.is_alive(), "a thread never finished")

        self.assertNotIn("advance", errors, f"opening the next generation must succeed: {errors}")
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        facts, _unreadable = marker.read_assignment(directory)
        published = isinstance(facts.get("relationship"), dict)
        state, readable = intent.dispatch_generation_state(str(self.store.path), rid, DISPATCH)
        self.assertTrue(readable, "the store became unreadable, which is a third answer entirely")

        if "register" in errors:
            self.assertEqual(errors["register"].reason, RefusalReason.STALE_GENERATION)
            self.assertFalse(
                published,
                "the registration was refused as stale and published a relationship anyway, so"
                " the marker now claims a registration the store contradicts",
            )
            return

        self.assertTrue(published, f"registration returned {results.get('register')!r}")
        # Publication is NOT evidence that the dispatch was current when it landed, and this
        # test does not pretend otherwise. register_relationship reads the generation state and
        # then publishes as two operations with nothing held between them, so an advance
        # committing in that window returns success over a generation the store has already
        # moved past. Asserting the absence of that race would be asserting a property the
        # source does not have; it is recorded in docs/contention-regression.md instead.
        #
        # What must hold either way is that the disagreement stays VISIBLE. The reader answers
        # stale or current and never absent, so the next evaluation reads the contradiction out
        # of the store rather than taking the marker's word, and the published fact is left
        # intact rather than rewritten behind it.
        self.assertIn(
            state, (intent.DISPATCH_CURRENT, intent.DISPATCH_STALE),
            f"a published registration left the dispatch reading {state!r}, so nothing"
            " downstream can tell whether it is this assignment's generation",
        )
        self.assertEqual(
            facts["relationship"]["relationshipId"], rid,
            "the published relationship names something other than the one registered",
        )

    def test_a_marker_registered_before_an_advance_reads_as_stale_rather_than_absent(self):
        """Stale and absent are different repairs, and only one of them is a lost dispatch."""
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.declare()
        self.claim()
        self.bind()
        self.register_marker(relationship)

        state, readable = intent.dispatch_generation_state(str(self.store.path), rid, DISPATCH)
        self.assertTrue(readable)
        self.assertEqual(state, intent.DISPATCH_CURRENT)

        self.registry.open_generation(
            rid, dispatch_request_id="revision-after", reason="needs_changes_revision",
            dispatch_turn_id="turn-after",
        )

        state, readable = intent.dispatch_generation_state(str(self.store.path), rid, DISPATCH)
        self.assertTrue(readable, "the store became unreadable, which is a third answer entirely")
        self.assertEqual(
            state, intent.DISPATCH_STALE,
            "a dispatch whose generation moved on must not read as one that never opened",
        )


class TwoCoordinatorsOnOneAssignment(GuardTestCase):
    """bind() is a link(), so the race has exactly one winner. Proven by racing it."""

    def test_two_concurrent_binds_leave_one_winner_and_one_recorded_conflict(self):
        self.declare()
        outcomes, errors = {}, {}
        barrier = threading.Barrier(2)

        def binder(name, session, task):
            def run():
                try:
                    barrier.wait(timeout=20)
                    outcomes[name] = intent.bind(
                        self.markers, workspace=self.workspace, assignment=self.assignment,
                        session_id=session, task_id=task, at=NOW,
                    )
                except Exception as error:  # noqa: BLE001 - asserted by the caller
                    errors[name] = error

            return threading.Thread(target=run)

        threads = [
            binder("first", CHILD, CHILD),
            binder("second", "01other-session", "01other-task"),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)

        self.assertEqual(errors, {}, f"neither binder may raise: {errors}")
        self.assertEqual(len(outcomes), 2, outcomes)
        results = sorted(outcome["outcome"] for outcome in outcomes.values())
        self.assertEqual(
            results, sorted([intent.BOUND, intent.CONFLICT]),
            f"two concurrent binds must settle as one winner and one conflict: {outcomes}",
        )

        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        facts, _unreadable = marker.read_assignment(directory)
        bound = facts["bound"]
        conflicts = facts["conflicts"]
        self.assertEqual(len(conflicts), 1, conflicts)
        self.assertNotEqual(
            conflicts[0]["attemptedSessionId"], bound["sessionId"],
            "the recorded conflict names the winner, so the contest left no trace of the loser",
        )
        loser = next(
            outcome for outcome in outcomes.values() if outcome["outcome"] == intent.CONFLICT
        )
        self.assertEqual(loser["boundSessionId"], bound["sessionId"])
        # The derived state is identity_bound: a bind won, and that is what it says. The contest
        # lives in the other answer, which is the one the guard records beside every decision, and
        # it must stay true until a resolution names the identity that actually won.
        self.assertEqual(intent.derive_assignment_state(facts, NOW), intent.IDENTITY_BOUND)
        self.assertTrue(
            intent.identity_contested(facts),
            "a recorded conflict left the assignment reading as uncontested, so nobody has to"
            " adjudicate an identity two coordinators both tried to bind",
        )

        intent.publish_resolution(
            self.markers, workspace=self.workspace, assignment=self.assignment,
            chosen_task_id=bound["taskId"], chosen_session_id=bound["sessionId"],
            reason="the winning link() is the identity", at=NOW,
            adjudicated=[
                {"factId": fact["factId"], "digest": marker.fact_digest(fact)}
                for fact in conflicts
            ],
        )
        resolved, _unreadable = marker.read_assignment(directory)
        self.assertFalse(
            intent.identity_contested(resolved),
            "a resolution naming the bound identity did not settle the contest it covers",
        )


if __name__ == "__main__":
    unittest.main()
