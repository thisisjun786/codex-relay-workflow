"""The Stop decision, judged against receipts that actually exist in the relay store.

This is where the marker stops being a filesystem exercise. Every readiness case below emits a real
receipt through the real intake, so the guard is reading events the relay wrote rather than a
dictionary a fixture handed it.

Three separations carry their own tests because collapsing any of them is how this feature would
fail: a missing receipt is not an unreadable store, an unreadable store is not a success, and none
of the three is ever answered by writing the evidence that was missing.
"""

import os
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from codex_session_relay import guard, intent, manifest, marker

from .support import CHILD, DISPATCH_TURN, RelayTestCase

NOW = "2026-01-01T00:05:00+00:00"
LATER = "2026-01-01T00:06:00+00:00"
DISPATCH = "dispatch-1"


class GuardTestCase(RelayTestCase):
    """A registered relationship, a marker declared for the same dispatch request, and a Stop."""

    def setUp(self):
        super().setUp()
        self.markers = Path(tempfile.mkdtemp(prefix="relay-guard-"))
        self.addCleanup(shutil.rmtree, self.markers, ignore_errors=True)
        self.assignment = marker.assignment_id(DISPATCH)
        self.workspace = Path(self.root)

    # ----------------------------------------------------------- marker fixtures

    def declare(self, **kw):
        return intent.declare_intent(
            self.markers,
            workspace=self.workspace,
            dispatch_request_id=DISPATCH,
            issue_key="REL-1",
            declared_at="2026-01-01T00:00:00+00:00",
            db_path=kw.pop("db_path", str(self.store.path)),
            **kw,
        )

    def claim(self, session=CHILD):
        return intent.publish_claim(
            self.markers, workspace=self.workspace, assignment=self.assignment,
            session_id=session, dispatch_request_id=DISPATCH, first_turn_id=DISPATCH_TURN, at=NOW,
        )

    def bind(self, session=CHILD):
        return intent.bind(
            self.markers, workspace=self.workspace, assignment=self.assignment,
            session_id=session, task_id=CHILD, at=NOW,
        )

    def register_marker(self, relationship):
        return intent.register_relationship(
            self.markers, workspace=self.workspace, assignment=self.assignment,
            relationship_id=relationship["relationshipId"], dispatch_request_id=DISPATCH, at=NOW,
            db_path=str(self.store.path),
        )

    def dispose(self, outcome, *, session=CHILD, turn=DISPATCH_TURN):
        return intent.publish_disposition(
            self.markers, workspace=self.workspace, assignment=self.assignment,
            session_id=session, turn_id=turn, outcome=outcome, at=NOW,
        )

    def managed(self):
        """The ordinary case: declared, claimed, bound and registered."""
        relationship = self.register()
        self.declare()
        self.claim()
        self.bind()
        self.register_marker(relationship)
        return relationship

    def emit_ready(self, relationship, *, status="completed", turn=DISPATCH_TURN):
        path = self.artifact("out.txt", "work")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn(status, turn=turn)
        )
        return self.accept(payload)

    def stop(self, **kw):
        payload = {
            "cwd": str(self.workspace),
            "session_id": kw.pop("session_id", CHILD),
            "turn_id": kw.pop("turn_id", DISPATCH_TURN),
            "stop_hook_active": kw.pop("stop_hook_active", False),
        }
        payload.update(kw)
        return payload

    def evaluate(self, *, mode=guard.HOLD, now=LATER, record=True, **kw):
        return guard.evaluate(
            self.markers, self.stop(**kw), now=now, mode=mode, record=record
        )


class UnmanagedAndUnclaimed(GuardTestCase):
    def test_an_ordinary_session_with_no_marker_is_never_touched(self):
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "unmanaged")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertEqual(verdict["hook_output"], {})
        self.assertIsNone(verdict["assignmentId"])

    def test_t22_a_bind_without_the_child_s_own_claim_releases(self):
        relationship = self.register()
        self.declare()
        self.bind()
        self.register_marker(relationship)
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_unclaimed")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_t1_a_correlated_session_before_the_bind_releases_and_keeps_the_record(self):
        self.declare()
        self.claim()
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "correlated_unbound")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        # The pre-bind window is not blind: what the turn WOULD have read is kept for the fold.
        self.assertEqual(verdict["record"]["pendingObservation"], "undeclared_turn_end")

    def test_t6_an_uncorrelated_occupant_is_released_before_and_after_the_bind(self):
        self.declare()
        before = self.evaluate(session_id="stranger")
        self.assertEqual(before["observation"], "dispatch_uncorrelated")
        self.claim()
        self.bind()
        after = self.evaluate(session_id="stranger")
        self.assertEqual(after["observation"], "marker_claimed_by_other_session")
        self.assertEqual(after["decision"], guard.RELEASE)

    def test_t12_a_bind_naming_no_session_releases_rather_than_holding(self):
        self.declare()
        self.claim()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        marker.publish(directory / "bound.json", {"sessionId": "", "taskId": CHILD, "at": NOW})
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "bound_identity_unnamed")
        self.assertEqual(verdict["decision"], guard.RELEASE)


class Declarations(GuardTestCase):
    def test_a_receipted_readiness_releases(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_receipt_staged_inside_the_child_s_own_turn_counts(self):
        """A child emits mid-turn, so the host reports inProgress and the event is stored staged.

        Requiring 'final' would read every honest readiness as a missing receipt and hold it, and
        finalization only happens after the daemon observes the turn end, which is after this hook.
        """
        relationship = self.managed()
        accepted = self.emit_ready(relationship, status="inProgress")
        self.assertEqual(accepted.get("_stage", "staged"), "staged")
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")

    def test_readiness_without_a_receipt_is_an_omission_and_is_held(self):
        self.managed()
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["decision"], guard.BLOCK)
        self.assertEqual(verdict["hook_output"]["decision"], "block")
        self.assertTrue(verdict["hook_output"]["reason"])

    def test_a_receipt_from_another_turn_does_not_answer_for_this_one(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review", turn="turn-other")
        verdict = self.evaluate(turn_id="turn-other")
        self.assertEqual(verdict["observation"], "receipt_missing")

    def test_t15_a_receipt_earned_under_another_assignment_is_unmatched(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        # Same session, same turn, current head, and a relationship this assignment never published.
        published = directory / "relationship.json"
        published.unlink()
        marker.publish(published, {"relationshipId": "rel-ffffffffffffffff", "at": NOW})
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")

    def test_an_undeclared_turn_is_the_detection_this_contract_exists_for(self):
        self.managed()
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "undeclared_turn_end")
        self.assertEqual(verdict["decision"], guard.BLOCK)

    def test_an_outcome_outside_the_vocabulary_cannot_buy_a_release(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        marker.publish(
            directory / "dispositions" / CHILD / (DISPATCH_TURN + ".json"),
            {"sessionId": CHILD, "turnId": DISPATCH_TURN, "outcome": "redy_for_review", "at": NOW},
        )
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "undeclared_turn_end")

    def test_user_interruption_always_wins_over_a_missing_registration(self):
        """Holding a turn waiting for a person is the one trade this policy refuses to make."""
        self.register()
        self.declare()
        self.claim()
        self.bind()
        for outcome in ("blocked_needs_input", "interrupted", "in_progress", "failed"):
            self.dispose(outcome, turn="turn-" + outcome)
            verdict = self.evaluate(turn_id="turn-" + outcome)
            self.assertEqual(verdict["observation"], "declared_" + outcome)
            self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_an_unregistered_relationship_is_managed_and_held_not_ignored(self):
        self.register()
        self.declare()
        self.claim()
        self.bind()
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "managed_unregistered")
        self.assertEqual(verdict["decision"], guard.BLOCK)


class Bounds(GuardTestCase):
    def test_a_turn_takes_exactly_one_hold(self):
        self.managed()
        first = self.evaluate()
        self.assertEqual(first["decision"], guard.BLOCK)
        second = self.evaluate()
        self.assertEqual(second["decision"], guard.RELEASE)
        self.assertEqual(second["state"], "hold_in_flight")
        self.assertEqual(second["observation"], "undeclared_turn_end")

    def test_an_in_flight_continuation_releases_on_the_delivered_flag_too(self):
        self.managed()
        verdict = self.evaluate(stop_hook_active=True)
        self.assertEqual(verdict["state"], "hold_in_flight")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_the_generation_bound_records_an_unresolved_handoff(self):
        self.managed()
        for index in range(guard.MAX_HOLDS_PER_GENERATION):
            held = self.evaluate(turn_id="turn-" + str(index))
            self.assertEqual(held["decision"], guard.BLOCK)
        verdict = self.evaluate(turn_id="turn-final")
        self.assertEqual(verdict["state"], "unresolved_handoff")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_exhaustion_is_evaluated_before_the_per_turn_guard(self):
        """A terminal statement about the assignment must not be hidden behind 'not right now'."""
        self.managed()
        for index in range(guard.MAX_HOLDS_PER_GENERATION):
            self.evaluate(turn_id="turn-" + str(index))
        repeat = self.evaluate(turn_id="turn-0")
        self.assertEqual(repeat["state"], "unresolved_handoff")

    def test_a_corrupt_hold_budget_is_reported_and_never_read_as_a_fresh_one(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        marker.publish(directory / "hook" / CHILD / DISPATCH_TURN / "hold.json", "not a record")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_malformed")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_t27_a_releasing_disposition_survives_a_corrupt_budget(self):
        """Counters are weighed only when an omission would be held."""
        self.managed()
        self.dispose("interrupted")
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        marker.publish(directory / "hook" / CHILD / DISPATCH_TURN / "hold.json", "not a record")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_interrupted")
        self.assertEqual(verdict["decision"], guard.RELEASE)


class ObserveOnly(GuardTestCase):
    def test_observe_only_classifies_and_records_and_never_holds(self):
        """Holding depends on isolation the sandbox grants, not on the decision logic."""
        self.managed()
        verdict = self.evaluate(mode=guard.OBSERVE)
        self.assertEqual(verdict["observation"], "undeclared_turn_end")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertFalse(verdict["record"]["held"])
        self.assertEqual(verdict["record"]["mode"], guard.OBSERVE)
        self.assertEqual(verdict["hook_output"], {})

    def test_observe_is_the_default(self):
        self.managed()
        verdict = guard.evaluate(self.markers, self.stop(), now=LATER)
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertEqual(verdict["record"]["mode"], guard.OBSERVE)


class FailureSeparation(GuardTestCase):
    def test_a_database_error_is_not_a_missing_receipt(self):
        """The difference between these two answers is a hold."""
        relationship = self.register()
        self.declare(db_path=str(self.workspace / "no-such-store.sqlite3"))
        self.claim()
        self.bind()
        self.register_marker(relationship)
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("receipts", verdict["reason"])
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_an_unlocatable_store_is_unreadable_rather_than_empty(self):
        relationship = self.register()
        intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id=DISPATCH,
            issue_key="REL-1", declared_at="2026-01-01T00:00:00+00:00",
        )
        self.claim()
        self.bind()
        self.register_marker(relationship)
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")

    def test_an_unreadable_marker_outranks_an_absent_one(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "bound.json").unlink()
        (directory / "bound.json").write_text("{not json", encoding="utf-8")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_malformed_fact_is_reported_rather_than_read_through(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "bound.json").unlink()
        (directory / "bound.json").write_text('"bare"', encoding="utf-8")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_malformed")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_the_guard_never_manufactures_the_evidence_it_found_missing(self):
        """The whole point of the detection is that it reports, and writing a receipt would be the
        one repair that makes the report meaningless."""
        self.managed()
        self.dispose("ready_for_review")
        before = self.counts()
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(self.counts(), before)

    def test_the_guard_writes_nothing_to_the_store_on_any_path(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        before = self.counts()
        self.evaluate()
        self.evaluate(session_id="stranger")
        self.evaluate(turn_id="turn-other")
        self.assertEqual(self.counts(), before)

    def test_evaluating_never_creates_a_store_at_a_misresolved_path(self):
        """Store.__init__ writes on open, so a guard built on it would mint an empty database whose
        answer is 'no receipt', which is a hold."""
        relationship = self.register()
        missing = self.workspace / "nested" / "relay.sqlite3"
        self.declare(db_path=str(missing))
        self.claim()
        self.bind()
        self.register_marker(relationship)
        self.dispose("ready_for_review")
        self.evaluate()
        self.assertFalse(missing.exists())
        self.assertFalse(missing.parent.exists())

    def counts(self):
        return {
            table: self.store.one("SELECT COUNT(*) AS n FROM " + table)["n"]
            for table in ("events", "acks", "verdicts", "verification_claims", "deliveries",
                          "assignment_marks", "journal")
        }


class Recording(GuardTestCase):
    def test_every_observation_is_recorded_whether_or_not_it_is_held(self):
        self.managed()
        held = self.evaluate()
        self.assertTrue(held["recordedAs"].startswith("hook/"))
        released = self.evaluate()
        self.assertTrue(released["recordedAs"].startswith("hook/"))
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        # Two observations, plus the one reservation the first evaluation took. The reservation is
        # named rather than numbered so it can share the hook-owned directory without colliding.
        published = sorted(
            p for p in (directory / "hook" / CHILD / DISPATCH_TURN).glob("*.json")
            if p.stem.isdigit()
        )
        self.assertEqual(len(published), 2)

    def test_the_record_carries_the_assignment_state_and_the_contest_both_ways(self):
        self.managed()
        verdict = self.evaluate()
        self.assertEqual(verdict["record"]["assignmentState"], intent.RELATIONSHIP_REGISTERED)
        self.assertIs(verdict["record"]["identityContested"], False)

    def test_nothing_is_recorded_for_an_unmanaged_workspace(self):
        verdict = self.evaluate()
        self.assertIn("recordedAs", verdict)
        self.assertIsNone(verdict["recordedAs"])
        self.assertFalse((self.markers / marker.workspace_key(self.workspace)).exists())


class ReviewRegressions(GuardTestCase):
    """One test per defect the hosted review found, each failing before its fix."""

    def test_a_turn_that_superseded_its_own_revision_is_still_receipted(self):
        """Selecting the event by session and turn picks an arbitrary row when a turn re-emits.

        If the older one is chosen, the guard reports the revision as superseded and holds a child
        whose current receipt is sitting at the head. The head is computed first for that reason.
        """
        relationship = self.managed()
        first = self.ready_payload(
            relationship, [self.artifact("out.txt", "first")], attempt=1,
            turn=self.assigned_turn("inProgress"),
        )
        self.accept(first)
        second = self.ready_payload(
            relationship, [self.artifact("out.txt", "second")], attempt=2,
            turn=self.assigned_turn("inProgress"),
        )
        self.accept(second)
        self.store.db.execute(
            "UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?",
            (first["revisionHash"], second["eventId"]),
        )
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_malformed_intent_is_malformed_rather_than_unmanaged(self):
        """Dropping it from selection turned a corrupt marker into an ordinary session.

        Unmanaged releases and records nothing at all, so one bad byte would switch detection off
        for the workspace and leave no trace that it had.
        """
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "intent.json").unlink()
        (directory / "intent.json").write_text('"bare"', encoding="utf-8")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_malformed")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertTrue(verdict["recordedAs"].startswith("hook/"))

    def test_a_corrupt_stale_assignment_does_not_release_the_current_one(self):
        """Read problems merged across the workspace let retained state switch detection off."""
        self.managed()
        stale = intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id="older-dispatch",
            issue_key="REL-0", declared_at="2025-01-01T00:00:00+00:00",
        )
        stale_dir = marker.assignment_dir(self.markers, self.workspace, stale["assignmentId"])
        (stale_dir / "attempts").mkdir(parents=True, exist_ok=True)
        (stale_dir / "attempts" / "0.json").write_text("{not json", encoding="utf-8")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "undeclared_turn_end")
        self.assertEqual(verdict["decision"], guard.BLOCK)

    def test_a_malformed_disposition_releases_instead_of_holding(self):
        """A disposition is read outside the assignment walk, so the shape check never saw it.

        Treated as no declaration it became a holdable omission, which holds a turn that did
        publish something over a fact the reader could not read.
        """
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "dispositions" / CHILD).mkdir(parents=True)
        (directory / "dispositions" / CHILD / (DISPATCH_TURN + ".json")).write_text(
            '"bare"', encoding="utf-8"
        )
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_malformed")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_wrongly_typed_disposition_identity_is_malformed_too(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        marker.publish(
            directory / "dispositions" / CHILD / (DISPATCH_TURN + ".json"),
            {"sessionId": CHILD, "turnId": DISPATCH_TURN, "outcome": ["ready_for_review"]},
        )
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_malformed")

    def test_the_session_hold_window_spans_every_assignment_on_the_workspace(self):
        """The generation bound belongs to one assignment; the rolling hour belongs to the session.

        Counted per assignment, a session could claim a new assignment and spend a fresh hour.
        """
        self.managed()
        other = intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id="another-dispatch",
            issue_key="REL-2", declared_at="2026-01-01T00:00:00+00:00",
        )
        other_dir = marker.assignment_dir(self.markers, self.workspace, other["assignmentId"])
        for index in range(guard.MAX_HOLDS_PER_SESSION_WINDOW):
            marker.publish(
                other_dir / "hook" / CHILD / ("spent-" + str(index)) / "hold.json",
                {"sessionId": CHILD, "turnId": "spent-" + str(index), "at": NOW},
            )
        verdict = self.evaluate(turn_id="turn-fresh")
        self.assertEqual(verdict["state"], "unresolved_handoff")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertEqual(verdict["counters"]["holdsThisSessionWindow"],
                         guard.MAX_HOLDS_PER_SESSION_WINDOW)
        # The generation bound still belongs to the selected assignment alone.
        self.assertEqual(verdict["counters"]["holdsThisGeneration"], 0)

    def test_the_coordinator_recorded_store_beats_the_callers_own_resolution(self):
        """A hook run without the coordinator's state selection must not read its own store.

        Letting the caller's resolution win is how a correctly receipted child gets held: the wrong
        store has no such relationship, so readiness reads receipt_missing.
        """
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        elsewhere = str(self.workspace / "someone-elses.sqlite3")
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD,
            default_db_path=elsewhere, record=False,
        )
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_an_explicit_db_path_still_outranks_the_recorded_one(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        elsewhere = str(self.workspace / "someone-elses.sqlite3")
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD,
            db_path=elsewhere, record=False,
        )
        # Named explicitly, so the caller gets the store it asked for, and it cannot be read.
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_the_default_is_used_when_the_intent_recorded_nothing(self):
        relationship = self.register()
        intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id=DISPATCH,
            issue_key="REL-1", declared_at="2026-01-01T00:00:00+00:00",
        )
        self.claim()
        self.bind()
        self.register_marker(relationship)
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD,
            default_db_path=str(self.store.path), record=False,
        )
        self.assertEqual(verdict["observation"], "declared_ready_receipted")

    def test_the_hook_record_refuses_an_identity_that_cannot_be_a_directory_name(self):
        """The observation's path is built from the delivered Stop payload."""
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        for bad in ("../escape", "a/b", "..", ".", ""):
            self.assertIsNone(
                guard.record_observation(directory, {"sessionId": bad, "turnId": DISPATCH_TURN}),
                bad,
            )
            self.assertIsNone(
                guard.record_observation(directory, {"sessionId": CHILD, "turnId": bad}), bad
            )
        self.assertFalse((directory / "hook").exists())

    def test_an_unreadable_workspace_is_unreadable_rather_than_unmanaged(self):
        """An OSError folded into an empty listing released the turn and recorded nothing."""
        blocked = self.markers / marker.workspace_key(self.workspace)
        blocked.parent.mkdir(parents=True, exist_ok=True)
        # A regular file where the workspace directory belongs: iterdir raises NotADirectoryError,
        # which is the same OSError family as a permission or mount fault and needs no chmod.
        blocked.write_text("not a directory", encoding="utf-8")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("workspace", verdict["reason"])
        self.assertEqual(verdict["decision"], guard.RELEASE)
        # And nothing was written, because there is nowhere to write it: selection produced no
        # assignment directory, and both recording paths are guarded by having one. Asserted so the
        # invariant that says so stays checked rather than merely claimed - the previous wording
        # promised persistence unconditionally and this is the case that does not have it.
        #
        # The key must be PRESENT and null. get() would have accepted an absent field too, which is
        # how the documented shape and the real one came apart in the first place.
        self.assertIn("recordedAs", verdict)
        self.assertIsNone(verdict["recordedAs"])


class ReceiptsAreBoundToTheRevisionTheyWereComputedOver(GuardTestCase):
    """A receipt names a revision, and a revision is bytes. Existence is not validity.

    Standing at the head is a statement about lineage. It says nothing about whether the artifacts
    the receipt hashed are still those artifacts, so a receipt accepted for merely existing releases
    a turn whose deliverable has since become something nobody reviewed - which is the same shape as
    a verdict outliving the criteria it judged.

    The rule is not new here. ReceiptIntake._verify_bytes already decides what verified means, and
    relationships.artifact_roots already holds the roots intake used, so the guard re-applies the
    same standard the receipt was admitted under.
    """

    def ready(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        return relationship

    def test_unchanged_bytes_still_release_the_turn(self):
        """The compatibility half: re-verification must not hold an honest child."""
        self.ready()
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_artifacts_changed_after_intake_stop_answering_for_the_receipt(self):
        self.ready()
        self.artifact("out.txt", "rewritten after the receipt was accepted")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["receiptEvidence"], "artifacts_changed_since_receipt")
        self.assertIn("out.txt", verdict["receiptDetail"])
        self.assertEqual(verdict["decision"], guard.BLOCK)
        # And the why travels with the record, so the hold names something the child can act on.
        self.assertEqual(verdict["record"]["receiptEvidence"], "artifacts_changed_since_receipt")

    def test_a_deleted_artifact_is_a_changed_revision_not_an_absent_receipt(self):
        self.ready()
        os.remove(os.path.join(self.root, "out.txt"))
        verdict = self.evaluate()
        self.assertEqual(verdict["receiptEvidence"], "artifacts_changed_since_receipt")
        self.assertIn("out.txt", verdict["receiptDetail"])
        # A vanished artifact is a readable answer about the deliverable, not a store this process
        # failed to read. The two release differently, so the distinction is the assertion.
        self.assertEqual(verdict["observation"], "receipt_missing")

    def test_a_frozen_copy_answers_for_files_that_legitimately_moved_on(self):
        """Intake consults the frozen copy only when the live bytes disagree; so does this."""
        relationship = self.managed()
        path = self.artifact("out.txt", "work")
        entries, _bindings = manifest.build(
            [path], relationship["authorizedScope"]["artifactRoots"]
        )
        frozen = os.path.join(self.tmp, "frozen")
        manifest.freeze(entries, frozen)
        payload = self.ready_payload(relationship, [path])
        payload["manifestRef"] = frozen
        self.accept(payload)
        self.dispose("ready_for_review")
        self.artifact("out.txt", "the working tree moved on")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    @unittest.skipIf(os.geteuid() == 0, "root bypasses the permission this depends on")
    def test_an_unreadable_live_artifact_is_never_reported_as_a_changed_one(self):
        """Driven through the real path, because the previous version of this test was not.

        It raised PermissionError from verify_against_disk itself, which proves the wrapper handles
        an exception it is handed and says nothing about whether one ever arrives. It does not:
        scope turns EACCES into a refusal, verify_against_disk catches it per entry and returns it
        as text, and the outer handler never ran. Injecting at the boundary you are testing only
        ever tests the boundary.
        """
        relationship = self.managed()
        path = self.artifact("locked/out.txt", "work")
        self.accept(self.ready_payload(relationship, [path]))
        self.dispose("ready_for_review")
        locked = Path(self.root) / "locked"
        # Registered before the chmod so it is undone even if the assertions fail, and before the
        # temporary tree is removed, because cleanups run in reverse.
        self.addCleanup(os.chmod, locked, 0o700)
        os.chmod(locked, 0o000)
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("the receipt's artifacts", verdict["reason"])
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertTrue(verdict["recordedAs"].startswith("hook/"))

    def test_an_exception_that_does_escape_is_still_classified(self):
        """The boundary test, kept and labelled as one.

        This injects at the boundary on purpose: it asserts what happens to an error that is raised
        rather than returned. It is not evidence about the real call path, which is what the test
        above is for.
        """
        self.ready()
        with mock.patch(
            "codex_session_relay.guard.verify_against_disk_detailed",
            side_effect=PermissionError(13, "the artifact root cannot be read"),
        ):
            verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("the receipt's artifacts", verdict["reason"])
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_stored_receipt_that_is_not_json_is_reported_rather_than_trusted(self):
        relationship = self.ready()
        self.store.db.execute(
            "UPDATE events SET receipt = ? WHERE relationship_id = ?",
            ("{not json", relationship["relationshipId"]),
        )
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["receiptEvidence"], "stored_receipt_unreadable")


class AHoldIsOnlyIssuedWhereItCanBeRecorded(GuardTestCase):
    """A hold is reserved, counted against three bounds, and released by the record that explains
    it. An evaluation that cannot record cannot do any of those, so it must not take one."""

    def holds(self, directory):
        return sorted((directory / "hook").glob("*/*/" + guard.HOLD_FILE))

    def test_a_dry_run_never_spends_a_turns_hold(self):
        """--no-record reserved hold.json and published nothing, so a preview spent the budget."""
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD, record=False
        )
        self.assertEqual(verdict["observation"], "undeclared_turn_end")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertEqual(verdict["modeDowngraded"], "hold_requires_a_recorded_observation")
        self.assertEqual(self.holds(directory), [])
        counters, _malformed, _unreadable = guard.hold_counters(
            directory, session_id=CHILD, turn_id=DISPATCH_TURN, now=LATER,
            workspace_root=directory.parent,
        )
        self.assertEqual(counters["holdsThisTurn"], 0)

    def test_a_verdict_always_carries_the_recorded_field(self):
        """Null means there was nowhere to write, and absence would have to be guessed at.

        Every branch answers the same question in the same shape: an unmanaged session, a dry run,
        an unreadable workspace and an ordinary recorded hold all carry the key.
        """
        self.assertIn("recordedAs", self.evaluate())
        self.managed()
        dry = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD, record=False
        )
        self.assertIn("recordedAs", dry)
        self.assertIsNone(dry["recordedAs"])
        recorded = self.evaluate()
        self.assertTrue(recorded["recordedAs"].startswith("hook/"))

    def test_the_downgrade_is_named_rather_than_applied_quietly(self):
        self.managed()
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD, record=False
        )
        self.assertEqual(verdict["record"]["mode"], guard.OBSERVE)
        self.assertEqual(
            verdict["record"]["modeDowngraded"], "hold_requires_a_recorded_observation"
        )
        self.assertIn("Hold mode was not applied", verdict["reason"])

    def test_recording_still_holds_when_it_is_actually_asked_for(self):
        """The control: the same Stop with recording on does take its hold."""
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        verdict = self.evaluate()
        self.assertEqual(verdict["decision"], guard.BLOCK)
        self.assertEqual(len(self.holds(directory)), 1)

    def test_an_unpublishable_observation_is_reported_and_gives_the_hold_back(self):
        """record_observation returned None, so the caller's release path never ran.

        The hold was reserved, the observation was never published, and the verdict claimed a hold
        that no record explained. intent._publish_numbered is the same loop over the same kind of
        (index, readable) helper and already raised on both of its paths.
        """
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        with mock.patch(
            "codex_session_relay.guard._next_hook_seq", return_value=(0, False)
        ):
            verdict = self.evaluate()
        self.assertEqual(verdict["observation"], guard.FAULTED)
        self.assertIn("OSError", verdict["fault"])
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertIsNone(verdict["recordedAs"])
        self.assertEqual(self.holds(directory), [])

    def test_an_exhausted_slot_search_is_reported_rather_than_returned(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        with mock.patch("codex_session_relay.guard.publish", return_value=marker.EXISTS):
            with self.assertRaises(OSError) as caught:
                guard.record_observation(
                    directory,
                    {"sessionId": CHILD, "turnId": DISPATCH_TURN, "observation": "x"},
                )
        self.assertIn("64 attempts", str(caught.exception))


class DecodeAndFrozenAccessKeepTheirOwnAnswers(GuardTestCase):
    """Two leaks out of the same distinction, closed on the side that owns it.

    A failure the guard could not read must never arrive as a normal state, and it must never be
    reported as a defect in the guard either. Both directions matter: guard_faulted exists so a bug
    in this code cannot hide as somebody's corrupt marker, and it stops being useful the moment a
    corrupt marker starts arriving as a bug in this code.
    """

    def test_a_marker_that_is_not_utf8_is_unreadable_rather_than_a_guard_defect(self):
        """UnicodeDecodeError is a ValueError, so it walked past the OSError handler in _read_fact
        and reached the classification boundary, where a corrupt file was labelled guard_faulted."""
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "intent.json").unlink()
        (directory / "intent.json").write_bytes(b'{"dispatchRequestId": "\xff\xfe"}')
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertTrue(verdict["recordedAs"].startswith("hook/"))

    def frozen_ready(self, name):
        relationship = self.managed()
        path = self.artifact("out.txt", "work")
        entries, _bindings = manifest.build(
            [path], relationship["authorizedScope"]["artifactRoots"]
        )
        reference = os.path.join(self.tmp, name)
        manifest.freeze(entries, reference)
        payload = self.ready_payload(relationship, [path])
        payload["manifestRef"] = reference
        self.accept(payload)
        self.dispose("ready_for_review")
        self.artifact("out.txt", "the working tree moved on")
        return reference

    @unittest.skipIf(os.geteuid() == 0, "root bypasses the permission this depends on")
    def test_an_unreachable_frozen_copy_is_not_a_changed_deliverable(self):
        """verify_frozen catches its access errors and returns them as problem strings.

        They never become exceptions, so the boundary that converts exceptions cannot see them and
        the guard read "I could not open the frozen copy" as "the deliverable changed" - holding a
        child over a comparison nobody performed.
        """
        reference = self.frozen_ready("guard-frozen-unreachable")
        files = os.path.join(reference, "files")
        self.addCleanup(os.chmod, files, 0o700)
        os.chmod(files, 0o000)
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("the receipt's artifacts", verdict["reason"])
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_frozen_copy_missing_its_bytes_is_a_changed_deliverable(self):
        """Absence is definitive. Releasing on it would let a provably broken snapshot pass."""
        reference = self.frozen_ready("guard-frozen-missing-blob")
        blob = sorted((Path(reference) / "files").iterdir())[0]
        os.remove(blob)
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["receiptEvidence"], "artifacts_changed_since_receipt")
        self.assertEqual(verdict["decision"], guard.BLOCK)

    def test_a_tampered_frozen_copy_is_still_a_changed_deliverable(self):
        """The control: bytes that were read and disagree are a change, and they hold."""
        reference = self.frozen_ready("guard-frozen-tampered")
        blob = sorted((Path(reference) / "files").iterdir())[0]
        os.chmod(blob, 0o600)
        blob.write_bytes(b"tampered")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["receiptEvidence"], "artifacts_changed_since_receipt")
        self.assertEqual(verdict["decision"], guard.BLOCK)

    def test_a_readable_frozen_copy_still_releases(self):
        """The other control: the whole path still works when nothing is wrong."""
        self.frozen_ready("guard-frozen-good")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)


class AFactThatNamesNothingHasRegisteredNothing(GuardTestCase):
    """The identity, not the object.

    observe_state guards the bind record with named(bound.get("sessionId")) so a fact that exists
    but names nobody cannot stand in for an identity. The relationship fact had only truthiness on
    the record, so a published relationship.json carrying a blank or missing relationshipId read as
    registered, and the turn landed on receipt_missing: an instruction to emit a receipt that no
    receipt could satisfy while the marker names nothing.
    """

    def publish_relationship(self, value):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "relationship.json").unlink()
        marker.publish(directory / "relationship.json", value, root=self.markers)
        return directory

    def test_a_blank_relationship_id_is_unregistered_rather_than_receipt_missing(self):
        self.publish_relationship({"relationshipId": "", "at": NOW})
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "managed_unregistered")
        self.assertIn("not registered", verdict["reason"])

    def test_a_relationship_fact_with_no_id_at_all_is_unregistered(self):
        self.publish_relationship({"at": NOW})
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "managed_unregistered")

    def test_the_instruction_it_gives_is_one_the_child_can_act_on(self):
        """The defect was not the hold, it was telling a child to do something impossible."""
        self.publish_relationship({"relationshipId": "   ", "at": NOW})
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertNotEqual(verdict["observation"], "receipt_missing")
        self.assertNotIn("Emit the receipt", verdict["reason"])

    def test_a_named_relationship_still_registers_and_releases(self):
        """The control: an ordinary receipted readiness is untouched."""
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        self.assertEqual(verdict["decision"], guard.RELEASE)


if __name__ == "__main__":
    unittest.main()
