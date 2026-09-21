"""Exact persisted Stop and relay settlement are different evidence registers."""

import hashlib
import json
from pathlib import Path
from unittest.mock import patch

from codex_session_relay import guard, intent, marker, omitted
from codex_session_relay.admission import admit_explicitly
from codex_session_relay.receipts import ObservationOutcome
from codex_session_relay.store import resolve_state_dir

from .support import CHILD, DISPATCH_TURN
from .test_guard import GuardTestCase, NOW, LATER


class Reporting(GuardTestCase):
    def read(self, **kw):
        return omitted.observe(
            resolve_state_dir(str(self.store.path.parent)), self.markers, self.workspace,
            kw.pop("assignment", self.assignment), kw.pop("session", CHILD),
            kw.pop("turn", DISPATCH_TURN), LATER, **kw)

    def settle(self, relation, status="completed", turn=DISPATCH_TURN):
        self.intake.record_observation(
            self.assigned_turn(status, turn=turn),
            ObservationOutcome.ORDINARY_TURN_END if status == "completed" else status,
            relationship_id=relation["relationshipId"])

    def snapshot(self):
        files = {str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in Path(self.tmp).rglob("*")
                 if p.is_file() and not p.name.endswith("-shm")}
        files.update({str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in self.markers.rglob("*") if p.is_file()})
        # SQLite readers update transient WAL read-lock slots, not application state.
        files["logical-database"] = tuple(self.store.db.iterdump())
        files["directory-entries"] = tuple(sorted(str(p) for p in Path(self.tmp).rglob("*")))
        return files

    def test_actual_omission_plus_independent_settlement_is_unreported_and_read_only(self):
        relation = self.managed()
        observed = self.evaluate()
        self.assertEqual(observed["observation"], "undeclared_turn_end")
        self.settle(relation)
        before = self.snapshot()
        result = self.read()
        self.assertEqual((result["reportingState"], result["reason"]),
                         ("unreported", "terminal_without_report"))
        self.assertEqual(result["turnAdmission"], "admitted")
        self.assertEqual(result["terminalObservation"]["status"], "completed")
        self.assertEqual(before, self.snapshot())
        self.assertEqual(self.store.all("SELECT event_id FROM events"), [])

    def test_empty_queue_is_not_a_stop_observation(self):
        relation = self.managed()
        self.settle(relation)
        self.assertEqual(self.read()["reason"], "stop_unobserved")

    def test_stop_is_not_terminal_evidence(self):
        self.managed()
        self.evaluate()
        result = self.read()
        self.assertEqual(result["reason"], "host_terminal_unobserved")
        self.assertEqual(result["stopObservation"]["record"]["observation"], "undeclared_turn_end")

    def test_duplicate_stop_and_later_declaration_preserve_original_warning(self):
        relation = self.managed()
        self.evaluate()
        self.evaluate()
        self.settle(relation)
        first = self.read()
        self.assertEqual(first["stopRecordCount"], 2)
        for outcome, expected in [("in_progress", "in_progress"),
                                  ("blocked_needs_input", "reported"),
                                  ("failed", "reported"), ("interrupted", "reported")]:
            # Each subcase owns a separate real disposition path.
            turn = "later-" + outcome
            admit_explicitly(self.store, self.clock, relation["relationshipId"], 1, turn,
                             actor="fixture")
            self.evaluate(turn_id=turn)
            self.settle(relation, turn=turn)
            self.dispose(outcome, turn=turn)
            result = self.read(turn=turn)
            self.assertEqual(result["reportingState"], expected)
            self.assertEqual(result["stopObservation"]["record"]["observation"], "undeclared_turn_end")
        self.assertEqual(first, self.read())

    def test_staged_ready_stays_staged(self):
        relation = self.managed()
        self.emit_ready(relation, status="inProgress")
        self.dispose("ready_for_review")
        self.evaluate()
        result = self.read()
        self.assertEqual(result["reportingState"], "reported")
        self.assertEqual(result["receipt"]["stage"], "staged")
        self.assertEqual(result["terminalObservation"]["status"], "unobserved")

    def test_failed_settlement_without_receipt_is_not_a_report(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation, "failed")
        self.assertEqual(self.read()["reportingState"], "unreported")
        self.intake.daemon_observation(relation["relationshipId"], self.assigned_turn("failed"))
        result = self.read()
        self.assertEqual((result["reportingState"], result["reason"]),
                         ("reported", "daemon_execution_report"))

    def test_foreign_global_observation_does_not_settle_this_assignment(self):
        relation = self.managed()
        self.evaluate()
        self.intake.record_observation(self.assigned_turn(), ObservationOutcome.ORDINARY_TURN_END,
                                       relationship_id="other-assignment")
        self.assertEqual(self.read()["reason"], "host_terminal_unobserved")
        self.settle(relation)
        self.assertEqual(self.read()["reportingState"], "unreported")

    def test_wrong_dispatch_claim_on_bound_session_is_unmeasured(self):
        self.managed()
        self.evaluate()
        path = marker.assignment_dir(self.markers, self.workspace, self.assignment) / "claims" / CHILD / "claim.json"
        fact = json.loads(path.read_text())
        fact["dispatchRequestId"] = "another-dispatch"
        path.write_text(json.dumps(fact))
        self.assertEqual(self.read()["reason"], "dispatch_uncorrelated")

    def test_exact_assignment_does_not_switch_to_more_recent_claim(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation)
        intent.declare_intent(self.markers, workspace=self.workspace,
                              dispatch_request_id="new", issue_key="OTHER", declared_at=LATER,
                              db_path=str(self.store.path))
        self.assertEqual(self.read()["reportingState"], "unreported")

    def test_unadmitted_business_is_not_inferred_from_claim_or_time(self):
        self.managed()
        self.evaluate(turn_id="unadmitted-business")
        self.assertEqual(self.read(turn="unadmitted-business")["reason"], "admission_unrecorded")

    def test_stale_generation_and_foreign_session_are_not_reports(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation)
        self.assertEqual(self.read(session="other-child")["reason"], "dispatch_uncorrelated")
        self.store.db.execute("UPDATE relationships SET execution_generation=2 WHERE relationship_id=?",
                              (relation["relationshipId"],))
        self.assertEqual(self.read()["reason"], "stale_generation")

    def test_paused_relationship_is_preserved_without_writes(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation)
        self.store.db.execute("UPDATE relationships SET status='paused' WHERE relationship_id=?",
                              (relation["relationshipId"],))
        before = self.snapshot()
        result = self.read()
        self.assertEqual(result["relationshipStatus"], "paused")
        self.assertEqual(before, self.snapshot())

    def test_marker_absent_is_unmanaged(self):
        self.assertEqual(self.read()["reportingState"], "unmanaged")

    def test_bad_stop_identity_is_not_an_omission(self):
        self.managed()
        self.evaluate()
        folder = marker.assignment_dir(self.markers, self.workspace, self.assignment) / "hook" / CHILD / DISPATCH_TURN
        path = next(p for p in folder.glob("*.json") if p.stem.isdecimal())
        record = json.loads(path.read_text())
        record["turnId"] = "another-turn"
        path.write_text(json.dumps(record))
        self.assertEqual(self.read()["reason"], "stop_identity_or_shape")

    def test_symlink_to_foreign_marker_file_is_unmeasured(self):
        self.managed()
        target = Path(self.tmp) / "elsewhere.json"
        target.write_text('{}')
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "escape").symlink_to(target)
        self.assertEqual(self.read()["reportingState"], "unmeasured")

    def test_changed_registry_during_read_does_not_mix_facts(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation)
        original = omitted._current
        def change(*args):
            result = original(*args)
            self.store.db.execute("UPDATE relationships SET parent_task_id='new-parent'")
            return result
        with patch.object(omitted, "_current", side_effect=change):
            self.assertEqual(self.read()["reason"], "registry_changed_during_read")

    def test_invalid_ids_fail_before_read(self):
        with patch.object(marker, "read_assignment", side_effect=AssertionError("read")):
            with self.assertRaises(ValueError):
                self.read(turn="../elsewhere")

    def test_corrupt_and_oversized_stop_records_are_unmeasured(self):
        self.managed()
        self.evaluate()
        folder = marker.assignment_dir(self.markers, self.workspace, self.assignment) / "hook" / CHILD / DISPATCH_TURN
        path = next(p for p in folder.glob("*.json") if p.stem.isdecimal())
        path.write_bytes(b'{invalid')
        self.assertEqual(self.read()["reportingState"], "unmeasured")
        path.write_bytes(b'x' * (omitted.MAX_BYTES + 1))
        self.assertEqual(self.read()["reason"], "marker_record_limit")

    def test_missing_managed_schema_is_not_absence_of_bootstrap(self):
        self.managed()
        self.evaluate()
        self.store.db.execute("DROP TABLE managed_start_requests")
        result = self.read()
        self.assertEqual(result["reportingState"], "unmeasured")
        self.assertIn("store_unreadable", result["reason"])

    def test_conflicting_terminal_observations_remain_unknown(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation)
        self.settle(relation, "failed")
        self.assertEqual(self.read()["reason"], "terminal_conflict")

    def test_selected_marker_cannot_alias_another_assignment(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        other = directory.with_name("b" * 64)
        directory.rename(other)
        directory.symlink_to(other, target_is_directory=True)
        self.assertEqual(self.read()["reportingState"], "unmeasured")

    def test_normal_managed_start_standby_is_not_business(self):
        # Reuse the real admission fixture without collecting its entire test class again.
        from .test_managed_start import ManagedEntry
        fixture = ManagedEntry("test_same_request_recovers_one_child_one_business_turn")
        fixture.setUp()
        try:
            start = fixture.start.run(fixture.request)
            root = fixture.start.identity["marker_root"]
            assignment = start["assignmentId"]
            dispatch = start["businessRequestId"]
            child = start["childTaskId"]
            intent.publish_claim(root, workspace=fixture.root, assignment=assignment,
                                 session_id=child, dispatch_request_id=dispatch,
                                 first_turn_id="business", at=NOW)
            selection = resolve_state_dir(str(fixture.store.path.parent))
            for turn, expected in [("standby", "bootstrap"), ("business", "terminal_without_report")]:
                guard.evaluate(root, {"cwd": fixture.root, "session_id": child, "turn_id": turn},
                               now=LATER, mode=guard.OBSERVE)
                fixture.intake.record_observation(
                    fixture.assigned_turn(thread=child, turn=turn),
                    ObservationOutcome.ORDINARY_TURN_END,
                    relationship_id=start["relationshipId"])
                result = omitted.observe(selection, root, fixture.root, assignment, child, turn, LATER)
                self.assertEqual(result["reason"], expected, result)
        finally:
            fixture.doCleanups()

    def test_store_and_issue_provenance_are_independent_of_same_session(self):
        relation = self.managed()
        self.evaluate()
        self.settle(relation)
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        path = directory / "intent.json"
        original = path.read_text()
        fact = json.loads(original)
        fact["dbPath"] = str(Path(self.tmp) / "other-store.sqlite3")
        path.write_text(json.dumps(fact))
        self.assertEqual(self.read()["reason"], "marker_selector_mismatch")
        path.write_text(original)
        self.store.db.execute("UPDATE relationships SET issue_key='OTHER'")
        self.assertEqual(self.read()["reason"], "registry_identity_mismatch")

    def test_missing_selected_database_stays_missing(self):
        self.managed()
        self.evaluate()
        self.store.close()
        self.store.path.unlink()
        result = self.read()
        self.assertIn("store_unreadable", result["reason"])
        self.assertFalse(self.store.path.exists())

    def test_unregistered_warning_is_visible_without_claiming_omitted_assignment(self):
        self.declare()
        self.claim()
        self.bind()
        self.evaluate()
        result = self.read()
        self.assertEqual(result["reason"], "registration_unresolved")
        self.assertEqual(result["stopObservation"]["record"]["observation"], "managed_unregistered")

    def test_reader_requires_actual_omission_not_only_an_ended_turn(self):
        relation = self.managed()
        self.dispose("in_progress")
        self.evaluate()
        self.settle(relation)
        self.assertEqual(self.read()["reportingState"], "in_progress")

    def test_more_than_bounded_stop_history_does_not_pick_a_convenient_subset(self):
        self.managed()
        self.evaluate()
        folder = marker.assignment_dir(self.markers, self.workspace, self.assignment) / "hook" / CHILD / DISPATCH_TURN
        source = next(p for p in folder.glob("*.json") if p.stem.isdecimal()).read_bytes()
        for n in range(omitted.MAX_RECORDS + 1):
            (folder / (str(n) + ".json")).write_bytes(source)
        self.assertEqual(self.read()["reason"], "stop_history_invalid")

    def test_special_marker_file_is_refused_before_a_blocking_read(self):
        import os
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        path = directory / "intent.json"
        path.unlink()
        os.mkfifo(path)
        with patch.object(marker, "read_assignment", side_effect=AssertionError("must not open FIFO")):
            self.assertEqual(self.read()["reason"], "marker_not_regular")
