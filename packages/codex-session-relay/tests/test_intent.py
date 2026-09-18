"""Management intent: the facts a coordinator publishes before the task exists, and the state
those facts derive to.

The trace ids in the test names are the interleavings named in hook-contract.md. They are the cases
that behave differently; permutations that behave identically are not repeated here.
"""

import shutil
import tempfile
import unittest
from pathlib import Path

from codex_session_relay import intent, marker
from codex_session_relay.errors import RefusalReason, RegistrationError, RelayError
from codex_session_relay.store import Store

T0 = "2026-01-01T00:00:00+00:00"
T5 = "2026-01-01T00:05:00+00:00"
T40 = "2026-01-01T00:40:00+00:00"

DISPATCH = "dispatch-request-1"
SESSION = "01child-session"
TASK = "01child-task"


class IntentTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-intent-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.root = Path(self.tmp) / "markers"
        self.workspace = Path(self.tmp) / "work"
        self.workspace.mkdir()
        self.assignment = marker.assignment_id(DISPATCH)
        # Registration is confirmed against the relay, so these tests need a real store holding a
        # real generation for the pair rather than a marker on its own.
        self.store = Store(str(Path(self.tmp) / "state" / "relay.sqlite3"))
        self.addCleanup(self.store.close)

    def open_generation(
        self, relationship_id="rel-0123456789abcdef", dispatch=DISPATCH, *, generation=1,
        current=None,
    ):
        """A generation of a relationship, as the relay actually stores it.

        Both rows, because a generation without its relationship is not a state the relay can be
        in, and registration now asks whether the generation this dispatch opened is still the
        relationship's current one. Pass current= to move the relationship on and leave this
        dispatch behind, which is the stale case.
        """
        self.store.db.execute(
            "INSERT OR IGNORE INTO relationships (relationship_id, issue_key, status,"
            " parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation,"
            " artifact_roots, allowed_recipients, created_at, updated_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                relationship_id, "REL-1", "active", "01parent-task", "host-a", "01child-task",
                "host-a", generation if current is None else current, "[]", "[]", T0, T0,
            ),
        )
        if current is not None:
            self.store.db.execute(
                "UPDATE relationships SET execution_generation = ? WHERE relationship_id = ?",
                (current, relationship_id),
            )
        self.store.db.execute(
            "INSERT OR IGNORE INTO generations (relationship_id, execution_generation,"
            " dispatch_request_id, anchor_state, opened_at) VALUES (?,?,?,?,?)",
            (relationship_id, generation, dispatch, "bound", T0),
        )

    # ------------------------------------------------------------- helpers

    def declare(self, **kw):
        return intent.declare_intent(
            self.root,
            workspace=self.workspace,
            dispatch_request_id=kw.pop("dispatch_request_id", DISPATCH),
            issue_key=kw.pop("issue_key", "REL-1"),
            declared_at=kw.pop("declared_at", T0),
            **kw,
        )

    def facts(self):
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        found, _unreadable = marker.read_assignment(directory)
        return found

    def state(self, now=T5):
        return intent.derive_assignment_state(self.facts(), now)

    def attempt(self, outcome, task_id=None, at=T0):
        return intent.record_attempt(
            self.root, workspace=self.workspace, assignment=self.assignment,
            outcome=outcome, task_id=task_id, at=at,
        )

    def claim(self, session=SESSION, dispatch=DISPATCH, at=T0):
        return intent.publish_claim(
            self.root, workspace=self.workspace, assignment=self.assignment,
            session_id=session, dispatch_request_id=dispatch, first_turn_id="turn-1", at=at,
        )

    def bind(self, session=SESSION, task=TASK, at=T0):
        return intent.bind(
            self.root, workspace=self.workspace, assignment=self.assignment,
            session_id=session, task_id=task, at=at,
        )

    def register(self, relationship_id="rel-0123456789abcdef", dispatch=DISPATCH, opened=True,
                 *, generation=1, current=None):
        if opened:
            self.open_generation(
                relationship_id, dispatch, generation=generation, current=current
            )
        return intent.register_relationship(
            self.root, workspace=self.workspace, assignment=self.assignment,
            relationship_id=relationship_id, dispatch_request_id=dispatch, at=T0,
            db_path=str(self.store.path),
        )


class DerivedState(IntentTestCase):
    def test_t0_the_normal_progression(self):
        self.declare()
        self.assertEqual(self.state(), intent.INTENT_DECLARED)
        self.attempt("accepted", TASK)
        self.assertEqual(self.state(), intent.CREATION_ACCEPTED)
        self.bind()
        self.assertEqual(self.state(), intent.IDENTITY_BOUND)
        self.register()
        self.assertEqual(self.state(), intent.RELATIONSHIP_REGISTERED)

    def test_t2_two_accepted_task_ids_are_ambiguous_rather_than_resolved_by_arrival(self):
        self.declare()
        self.attempt("accepted", "task-a")
        self.attempt("accepted", "task-b")
        self.assertEqual(self.state(), intent.AMBIGUOUS_IDENTITY)

    def test_t3_a_lost_creation_response_is_unknown_and_not_a_failure(self):
        self.declare()
        self.attempt("unknown")
        self.assertEqual(self.state(), intent.CREATION_UNKNOWN)

    def test_a_failed_creation_is_neither_unknown_nor_accepted(self):
        self.declare()
        self.attempt("failed")
        self.assertEqual(self.state(), intent.INTENT_DECLARED)

    def test_t8_expiry_is_anchored_on_declaredat_and_nothing_else(self):
        self.declare()
        self.attempt("accepted", TASK, at=T40)
        # An acceptance-derived anchor would move with the later fact and revive an expired intent.
        self.assertEqual(self.state(now=T40), intent.INTENT_EXPIRED)

    def test_an_unknown_creation_still_expires(self):
        """Acceptance is exactly what never arrived, so an acceptance anchor could never reach it."""
        self.declare()
        self.attempt("unknown")
        self.assertEqual(self.state(now=T40), intent.INTENT_EXPIRED)

    def test_ambiguity_outranks_expiry_because_it_needs_a_decision_either_way(self):
        self.declare()
        self.attempt("accepted", "task-a")
        self.attempt("accepted", "task-b")
        self.assertEqual(self.state(now=T40), intent.AMBIGUOUS_IDENTITY)

    def test_a_late_fact_cannot_move_the_state_backwards(self):
        self.declare()
        self.attempt("accepted", TASK)
        self.bind()
        self.register()
        self.attempt("accepted", TASK, at=T5)
        self.assertEqual(self.state(), intent.RELATIONSHIP_REGISTERED)


class Binding(IntentTestCase):
    def test_binding_is_idempotent_on_replay(self):
        self.declare()
        self.assertEqual(self.bind()["outcome"], intent.BOUND)
        self.assertEqual(self.bind()["outcome"], intent.UNCHANGED)
        # No conflict was recorded at all: a replay of the same identity is not a contest.
        self.assertEqual(self.facts().get("conflicts") or [], [])

    def test_t4_a_losing_binder_records_a_conflict_and_the_winner_stands(self):
        self.declare()
        self.bind()
        losing = self.bind(session="other-session", task="other-task")
        self.assertEqual(losing["outcome"], intent.CONFLICT)
        self.assertEqual(losing["boundSessionId"], SESSION)
        facts = self.facts()
        self.assertEqual(facts["bound"]["sessionId"], SESSION)
        self.assertEqual(len(facts["conflicts"]), 1)
        self.assertEqual(facts["conflicts"][0]["attemptedSessionId"], "other-session")

    def test_t9_identity_is_immutable_under_every_writer(self):
        self.declare()
        self.bind()
        self.bind(session="second", task="second-task")
        self.assertEqual(self.facts()["bound"]["sessionId"], SESSION)

    def test_a_bind_naming_nothing_is_refused_rather_than_published(self):
        self.declare()
        with self.assertRaises(RelayError) as caught:
            self.bind(session="", task=TASK)
        self.assertEqual(caught.exception.reason, RefusalReason.UNBOUND_GENERATION)

    def test_an_intent_needs_an_exact_dispatch_request_id(self):
        with self.assertRaises(RelayError) as caught:
            self.declare(dispatch_request_id="  ")
        self.assertEqual(caught.exception.reason, RefusalReason.UNBOUND_GENERATION)

    def test_a_replay_that_corrects_a_semantic_field_is_a_conflict_not_unchanged(self):
        """The file is create-once, so the correction does NOT land. Saying unchanged told a
        coordinator its fix had taken while the guard went on reading the stale store."""
        first = self.declare(db_path="/first/relay.sqlite3")
        self.assertEqual(first["outcome"], marker.PUBLISHED)
        again = self.declare(db_path="/corrected/relay.sqlite3")
        self.assertEqual(again["outcome"], intent.CONFLICT)
        self.assertEqual(self.facts()["intent"]["dbPath"], "/first/relay.sqlite3")

    def test_an_identical_replay_is_still_unchanged(self):
        self.declare(db_path="/first/relay.sqlite3")
        self.assertEqual(
            self.declare(db_path="/first/relay.sqlite3")["outcome"], intent.UNCHANGED
        )

    def test_every_semantic_field_is_compared(self):
        baseline = {"criteria_source": "doc-a", "baseline_revision": "rev-1", "issue_key": "REL-1"}
        self.declare(**baseline)
        for field, value in (
            ("criteria_source", "doc-b"),
            ("baseline_revision", "rev-2"),
            ("issue_key", "REL-9"),
        ):
            with self.subTest(field=field):
                self.assertEqual(
                    self.declare(**{**baseline, field: value})["outcome"], intent.CONFLICT
                )

    def test_the_dispatch_request_id_is_stored_only_as_its_hash(self):
        self.declare()
        published = self.facts()["intent"]
        self.assertNotIn(DISPATCH, str(published))
        self.assertEqual(published["dispatchRequestIdHash"], marker.assignment_id(DISPATCH))


class Registration(IntentTestCase):
    def test_a_relationship_dispatched_under_another_request_id_is_refused(self):
        """T15 becomes unenforceable if the assignment will accept any relationship at all."""
        self.declare()
        self.bind()
        with self.assertRaises(RelayError) as caught:
            self.register(dispatch="a-different-dispatch")
        self.assertEqual(caught.exception.reason, RefusalReason.RELATIONSHIP_CONFLICT)
        self.assertNotIn("relationship", self.facts())

    def test_an_unrelated_relationship_carrying_the_right_dispatch_id_is_refused(self):
        """The hash check only proves the CALLER restated the right dispatch id.

        Pairing a relationship the relay never opened under this dispatch passes every check the
        filesystem can make, and its receipts would then satisfy this assignment's guard.
        """
        self.declare()
        self.bind()
        with self.assertRaises(RelayError) as caught:
            self.register(relationship_id="rel-ffffffffffffffff", opened=False)
        self.assertEqual(caught.exception.reason, RefusalReason.RELATIONSHIP_CONFLICT)
        self.assertNotIn("relationship", self.facts())

    def test_a_dispatch_whose_generation_has_advanced_is_refused_as_stale(self):
        """Finding the row proves SOME generation used this dispatch, not the live one.

        generations keeps one row per generation, so a relationship that has moved on still carries
        the older dispatch. lookup_receipt computes the head over the relationship's current
        execution_generation, so registering the stale assignment anyway would let its guard release
        turns on the strength of work belonging to a later generation.
        """
        self.declare()
        self.bind()
        with self.assertRaises(RelayError) as caught:
            self.register(generation=1, current=2)
        self.assertEqual(caught.exception.reason, RefusalReason.STALE_GENERATION)
        self.assertNotIn("relationship", self.facts())

    def test_stale_is_a_different_answer_from_never_opened(self):
        """The contract asks for an old generation to be distinguished, not folded into absent."""
        self.declare()
        self.bind()
        with self.assertRaises(RelayError) as stale:
            self.register(generation=1, current=2)
        self.assertEqual(stale.exception.reason, RefusalReason.STALE_GENERATION)
        with self.assertRaises(RelayError) as absent:
            self.register(relationship_id="rel-ffffffffffffffff", opened=False)
        self.assertEqual(absent.exception.reason, RefusalReason.RELATIONSHIP_CONFLICT)
        self.assertNotEqual(stale.exception.reason, absent.exception.reason)

    def test_the_current_generation_still_registers(self):
        """The control: the ordinary case must still pass the comparison it now makes."""
        self.declare()
        self.bind()
        self.register()
        self.assertIn("relationship", self.facts())

    def test_registration_is_refused_when_the_relay_cannot_be_read(self):
        """Refusing to claim, rather than taking the caller's word, is the conservative side."""
        self.declare()
        self.bind()
        with self.assertRaises(RelayError) as caught:
            intent.register_relationship(
                self.root, workspace=self.workspace, assignment=self.assignment,
                relationship_id="rel-0123456789abcdef", dispatch_request_id=DISPATCH, at=T0,
                db_path=str(Path(self.tmp) / "no-such-store.sqlite3"),
            )
        self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_RELATIONSHIP)

    def test_registration_is_create_once_and_names_a_contradiction_as_one(self):
        """A replay and a different relationship must not report the same word.

        One is a retry safe to ignore; the other is a coordinator publishing over a registration
        that already stands, and somebody has to settle it.
        """
        self.declare()
        self.bind()
        self.assertEqual(self.register()["outcome"], marker.PUBLISHED)
        self.assertEqual(self.register()["outcome"], intent.UNCHANGED)
        self.assertEqual(
            self.register(relationship_id="rel-ffffffffffffffff")["outcome"], intent.CONFLICT
        )
        self.assertEqual(self.facts()["relationship"]["relationshipId"], "rel-0123456789abcdef")

    def test_a_malformed_assignment_id_is_refused_rather_than_published(self):
        self.declare()
        for bad in ("../escape", "not-hex", "", "A" * 64):
            with self.assertRaises(RelayError) as caught:
                intent.record_attempt(
                    self.root, workspace=self.workspace, assignment=bad,
                    outcome="accepted", task_id=TASK, at=T0,
                )
            self.assertEqual(caught.exception.reason, RefusalReason.UNKNOWN_GENERATION)


class Claims(IntentTestCase):
    def test_the_claimant_comes_from_the_path_and_the_body_must_agree(self):
        self.declare()
        self.claim()
        claim = self.facts()["claims"][0]
        self.assertEqual(intent.claimant(claim), SESSION)

    def test_t17_a_claim_naming_the_bound_session_in_its_own_body_owns_nothing(self):
        """The variant that defeats reading the path alone."""
        self.declare()
        self.claim()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(
            directory / "claims" / "impostor" / "claim.json",
            {"dispatchRequestId": DISPATCH, "sessionId": SESSION, "at": T0},
        )
        impostor = next(c for c in self.facts()["claims"] if "impostor" in c["factId"])
        self.assertIsNone(intent.claimant(impostor))

    def test_t19_a_path_only_claim_owns_nothing_but_still_exists(self):
        self.declare()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(directory / "claims" / SESSION / "claim.json", {"at": T0})
        claim = self.facts()["claims"][0]
        self.assertIsNone(intent.claimant(claim))
        self.assertEqual(len(self.facts()["claims"]), 1)

    def test_a_claim_naming_another_dispatch_is_refused_where_it_is_published(self):
        """An assignment id IS the hash of a dispatch request id, so the two are derivable.

        A claim is what the guard requires before it will hold a session the coordinator bound, so
        a claim nobody correlated must not be able to satisfy it. register_relationship has always
        made this check; the claim path was its unmade sibling.
        """
        self.declare()
        with self.assertRaises(RegistrationError) as caught:
            self.claim(dispatch="not-the-dispatch-id")
        self.assertIn("different assignment", str(caught.exception))
        self.assertEqual(self.facts().get("claims", []), [])

    def test_correlation_requires_the_preimage_the_intent_hashed(self):
        """Defence in depth: the reader still checks, for a claim that did not come through the API.

        The refusal above closes the publishing path. This one holds the property at the reading
        end, where a file written by anything at all is what actually gets judged, so it publishes
        the claim directly rather than through publish_claim.
        """
        self.declare()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(
            directory / "claims" / SESSION / "claim.json",
            {"dispatchRequestId": "not-the-dispatch-id", "sessionId": SESSION, "at": T0},
        )
        self.assertFalse(intent.correlated(self.facts(), SESSION))
        marker.publish(
            directory / "claims" / "second" / "claim.json",
            {"dispatchRequestId": DISPATCH, "sessionId": "second", "at": T0},
        )
        self.assertTrue(intent.correlated(self.facts(), "second"))


class Contest(IntentTestCase):
    def adjudicated(self, *facts):
        return [{"factId": f["factId"], "digest": marker.fact_digest(f)} for f in facts]

    def test_t7_a_second_claim_after_a_bind_contests_the_assignment(self):
        self.declare()
        self.claim()
        self.bind()
        self.assertFalse(intent.identity_contested(self.facts()))
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(
            directory / "claims" / "second" / "claim.json",
            {"dispatchRequestId": DISPATCH, "sessionId": "second", "at": T5},
        )
        self.assertTrue(intent.identity_contested(self.facts()))

    def test_t16_a_competing_claim_that_leaves_its_session_blank_still_competes(self):
        """Read from the body it vanished and the contest went false, which is the one thing that
        lets a verdict settle over unadjudicated evidence."""
        self.declare()
        self.claim()
        self.bind()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(
            directory / "claims" / "blank" / "claim.json",
            {"dispatchRequestId": DISPATCH, "sessionId": "", "at": T5},
        )
        self.assertTrue(intent.identity_contested(self.facts()))

    def test_a_resolution_clears_a_contest_only_by_naming_the_bound_identity(self):
        self.declare()
        self.claim()
        self.bind()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(
            directory / "claims" / "second" / "claim.json",
            {"dispatchRequestId": DISPATCH, "sessionId": "second", "at": T5},
        )
        competing = next(c for c in self.facts()["claims"] if "second" in c["factId"])
        # A resolution choosing the COMPETITOR covers the same evidence and must not clear it.
        intent.publish_resolution(
            self.root, workspace=self.workspace, assignment=self.assignment,
            chosen_task_id=TASK, chosen_session_id="second", reason="wrong winner", at=T5,
            adjudicated=self.adjudicated(competing),
        )
        self.assertTrue(intent.identity_contested(self.facts()))
        intent.publish_resolution(
            self.root, workspace=self.workspace, assignment=self.assignment,
            chosen_task_id=TASK, chosen_session_id=SESSION, reason="bind stands", at=T5,
            adjudicated=self.adjudicated(competing),
        )
        self.assertFalse(intent.identity_contested(self.facts()))

    def test_coverage_needs_the_digest_to_match_the_content(self):
        self.declare()
        self.claim()
        self.bind()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(
            directory / "claims" / "second" / "claim.json",
            {"dispatchRequestId": DISPATCH, "sessionId": "second", "at": T5},
        )
        competing = next(c for c in self.facts()["claims"] if "second" in c["factId"])
        intent.publish_resolution(
            self.root, workspace=self.workspace, assignment=self.assignment,
            chosen_task_id=TASK, chosen_session_id=SESSION, reason="stale digest", at=T5,
            adjudicated=[{"factId": competing["factId"], "digest": "0" * 64}],
        )
        # The quantity the name is about. identity_contested is true for several reasons - no
        # resolution, one naming somebody else, an unidentified fact - so on its own it cannot
        # say the digest is what refused coverage. Forcing covered() to answer False for every
        # input leaves the line below green and this one red.
        self.assertFalse(
            intent.covered(competing, intent._resolutions(self.facts())),
            "a resolution carrying the wrong digest covered the fact anyway",
        )
        self.assertTrue(intent.identity_contested(self.facts()))

    def test_pre_bind_ambiguity_survives_resolutions_that_disagree(self):
        self.declare()
        self.attempt("accepted", "task-a")
        self.attempt("accepted", "task-b")
        self.claim()
        self.claim(session="second")
        facts = self.facts()
        everything = [a for a in facts["attempts"]] + list(facts["claims"])
        for chosen in ("task-a", "task-b"):
            intent.publish_resolution(
                self.root, workspace=self.workspace, assignment=self.assignment,
                chosen_task_id=chosen, chosen_session_id=SESSION, reason="disagreeing", at=T5,
                adjudicated=self.adjudicated(*everything),
            )
        self.assertEqual(self.state(), intent.AMBIGUOUS_IDENTITY)

    def test_a_resolution_naming_an_unaccepted_task_cannot_clear_ambiguity(self):
        self.declare()
        self.attempt("accepted", "task-a")
        self.attempt("unknown", "task-b")
        self.claim()
        self.claim(session="second")
        facts = self.facts()
        everything = [a for a in facts["attempts"]] + list(facts["claims"])
        intent.publish_resolution(
            self.root, workspace=self.workspace, assignment=self.assignment,
            chosen_task_id="task-b", chosen_session_id=SESSION, reason="unconfirmed", at=T5,
            adjudicated=self.adjudicated(*everything),
        )
        self.assertEqual(self.state(), intent.AMBIGUOUS_IDENTITY)


class Shape(IntentTestCase):
    def test_t18_a_fact_that_is_a_bare_string_is_malformed(self):
        self.declare()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        (directory / "claims" / SESSION).mkdir(parents=True)
        (directory / "claims" / SESSION / "claim.json").write_text('"bare"', encoding="utf-8")
        self.assertEqual(intent.malformed(self.facts()), "claims")

    def test_t25_an_array_identity_is_malformed_before_any_set_is_built(self):
        self.declare()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(directory / "attempts" / "0.json",
                       {"outcome": "accepted", "taskId": ["a", "b"]})
        self.assertEqual(intent.malformed(self.facts()), "attempts.taskId")

    def test_t26_a_present_null_chosen_identity_is_malformed(self):
        self.declare()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(directory / "resolutions" / "0.json",
                       {"chosenTaskId": None, "chosenSessionId": SESSION, "adjudicated": []})
        self.assertEqual(intent.malformed(self.facts()), "resolutions.chosenTaskId")

    def test_t23_a_nested_adjudication_that_is_not_a_record_is_malformed(self):
        self.declare()
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        marker.publish(directory / "resolutions" / "0.json",
                       {"chosenTaskId": TASK, "chosenSessionId": SESSION,
                        "adjudicated": ["not-a-record"]})
        self.assertEqual(intent.malformed(self.facts()), "resolutions.adjudicated")

    def test_t24_a_negative_or_null_counter_is_corruption_not_a_fresh_budget(self):
        self.assertIsNone(intent.malformed_counters({"holdsThisTurn": 0}))
        self.assertIsNone(intent.malformed_counters(None))
        self.assertEqual(intent.malformed_counters({"holdsThisTurn": None}), "counters.holdsThisTurn")
        self.assertEqual(intent.malformed_counters({"holdsThisTurn": -1}), "counters.holdsThisTurn")
        self.assertEqual(intent.malformed_counters({"holdsThisTurn": "1"}), "counters.holdsThisTurn")

    def test_a_traversing_identity_is_refused_at_every_writer(self):
        """A session or turn id becomes a directory name, so naming something is not enough."""
        self.declare()
        for bad in ("../escape", "a/b", "..", ".", "", "  "):
            with self.assertRaises(RelayError) as caught:
                intent.publish_claim(
                    self.root, workspace=self.workspace, assignment=self.assignment,
                    session_id=bad, dispatch_request_id=DISPATCH, first_turn_id="turn-1", at=T0,
                )
            self.assertEqual(caught.exception.reason, RefusalReason.UNBOUND_GENERATION, bad)
            with self.assertRaises(RelayError):
                intent.publish_disposition(
                    self.root, workspace=self.workspace, assignment=self.assignment,
                    session_id=bad, turn_id="turn-1", outcome="interrupted", at=T0,
                )
            with self.assertRaises(RelayError):
                intent.publish_disposition(
                    self.root, workspace=self.workspace, assignment=self.assignment,
                    session_id=SESSION, turn_id=bad, outcome="interrupted", at=T0,
                )
        directory = marker.assignment_dir(self.root, self.workspace, self.assignment)
        self.assertFalse((directory / "claims").exists())
        self.assertFalse((directory / "dispositions").exists())

    def test_nothing_is_written_outside_the_assignment_directory(self):
        self.declare()
        with self.assertRaises(RelayError):
            intent.publish_claim(
                self.root, workspace=self.workspace, assignment=self.assignment,
                session_id="../../escaped", dispatch_request_id=DISPATCH,
                first_turn_id="turn-1", at=T0,
            )
        self.assertFalse((Path(self.tmp) / "escaped").exists())
        self.assertFalse((self.root / "escaped").exists())

    def test_a_disposition_outside_the_vocabulary_is_refused(self):
        self.declare()
        with self.assertRaises(RelayError) as caught:
            intent.publish_disposition(
                self.root, workspace=self.workspace, assignment=self.assignment,
                session_id=SESSION, turn_id="turn-1", outcome="done", at=T0,
            )
        self.assertEqual(caught.exception.reason, RefusalReason.OUTCOME_INCONSISTENT)


class Selection(IntentTestCase):
    def other(self, dispatch):
        return intent.declare_intent(
            self.root, workspace=self.workspace, dispatch_request_id=dispatch,
            issue_key="REL-2", declared_at=T5,
        )

    def test_an_assignment_with_no_published_intent_is_not_selectable(self):
        directory = marker.assignment_dir(
            self.root, self.workspace, marker.assignment_id("half-built")
        )
        (directory / "attempts").mkdir(parents=True)
        self.declare()
        found, facts, _ = intent.select_assignment(self.root, self.workspace, SESSION)
        self.assertEqual(found.name, self.assignment)

    def test_t13_a_claim_is_consulted_before_recency(self):
        """A later assignment declared for the path must not capture a running earlier child."""
        self.declare()
        self.claim()
        self.other("dispatch-request-2")
        found, facts, _ = intent.select_assignment(self.root, self.workspace, SESSION)
        self.assertEqual(found.name, self.assignment)

    def test_without_a_claim_the_newest_declaration_wins(self):
        self.declare()
        later = self.other("dispatch-request-2")
        found, _facts, _ = intent.select_assignment(self.root, self.workspace, "stranger")
        self.assertEqual(found.name, later["assignmentId"])

    def test_recency_compares_instants_rather_than_printed_strings(self):
        """ISO 8601 sorts chronologically only when the offsets match.

        01:00+02:00 is 23:00 the previous day, so a lexical max picks it over a later 00:30+00:00
        and selects the wrong assignment for every decision that follows.
        """
        intent.declare_intent(
            self.root, workspace=self.workspace, dispatch_request_id="offset-earlier",
            issue_key="REL-1", declared_at="2026-01-02T01:00:00+02:00",
        )
        later = intent.declare_intent(
            self.root, workspace=self.workspace, dispatch_request_id="offset-later",
            issue_key="REL-2", declared_at="2026-01-02T00:30:00+00:00",
        )
        found, _facts, _unreadable = intent.select_assignment(
            self.root, self.workspace, "stranger"
        )
        self.assertEqual(found.name, later["assignmentId"])

    def test_an_unmanaged_workspace_selects_nothing(self):
        found, facts, unreadable = intent.select_assignment(
            self.root, Path(self.tmp) / "work", SESSION
        )
        self.assertIsNone(found)
        self.assertIsNone(facts)


if __name__ == "__main__":
    unittest.main()
