"""Managed admission reservation: one issue, one pending request, one owner.

These tests use a real SQLite store and a second connection, the same way two
processes would race. Nothing here talks to a host.
"""

import os
import shutil
import sqlite3
import tempfile
import threading
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RefusalReason
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry, record_settings
from codex_session_relay.store import Store

from .support import HOST, PARENT, task_settings


ISSUE = "CRW-180"
FINGERPRINT = "fp-complete-v1"
OTHER_FINGERPRINT = "fp-other"


def identity(**overrides):
    fields = {
        "request_id": "req-1",
        "issue_key": ISSUE,
        "request_fingerprint": FINGERPRINT,
        "fingerprint_version": "1",
        "workspace": "/work",
        "marker_root": "/markers",
        "socket_identity": "sock-a",
        "create_request_id": "create-req-1",
        "dispatch_request_id": "dispatch-req-1",
    }
    fields.update(overrides)
    return fields


class ReservationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-reservation-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.clock = FakeClock()
        self.store = Store(os.path.join(self.tmp, "relay.sqlite3"))
        self.addCleanup(self.store.close)
        self.registry = Registry(self.store, self.clock)

    def assertRefused(self, reason, call, *args, **kwargs):
        from codex_session_relay.errors import RelayError

        with self.assertRaises(RelayError) as caught:
            call(*args, **kwargs)
        self.assertEqual(caught.exception.reason, reason)
        return caught.exception

    def reserve(self, **overrides):
        return self.registry.reserve_start(identity(**overrides))

    def accepted(self, thread_id="child-1", turn_id="standby-1"):
        return {
            "status": "accepted",
            "threadId": thread_id,
            "turnId": turn_id,
            "ledgerRef": "kept-on-the-bridge",
            "settings": {"cwd": "/work"},
        }

    def arm_and_accept(self, request_id="req-1", fingerprint=FINGERPRINT, **receipt):
        armed = self.registry.arm_start(request_id, fingerprint, 0)
        recorded = self.registry.record_start_receipt(
            request_id, fingerprint, self.accepted(**receipt))
        return armed, recorded

    def register_managed(self, request_id="req-1", child="child-1", turn="standby-1",
                         dispatch="dispatch-req-1", issue=ISSUE):
        return self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint(child, HOST, cwd="/work"),
            issue_key=issue,
            artifact_roots=["/work"],
            allowed_recipients=[PARENT],
            dispatch_request_id=dispatch,
            dispatch_turn_id=turn,
            managed_request_id=request_id,
        )

    def test_reserve_starts_at_revision_zero_and_replays_the_same_fingerprint(self):
        first = self.reserve()
        self.assertEqual(first["state"], "reserved")
        self.assertEqual(first["revision"], 0)
        self.assertIsNone(first["child_task_id"])
        self.assertIsNone(first["standby_turn_id"])
        again = self.reserve()
        self.assertEqual(again["request_id"], first["request_id"])
        self.assertEqual(again["revision"], 0)
        self.assertEqual(again["created_at"], first["created_at"])

    def test_same_request_with_a_different_fingerprint_is_rejected(self):
        self.reserve()
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT,
            self.reserve, request_fingerprint=OTHER_FINGERPRINT,
        )
        retained = self.registry.start_request("req-1")
        self.assertEqual(retained["state"], "reserved")
        self.assertEqual(retained["request_fingerprint"], FINGERPRINT)

    def test_a_second_request_cannot_hold_the_same_issue(self):
        self.reserve()
        self.assertRefused(
            RefusalReason.DUPLICATE_ASSIGNMENT,
            self.reserve, request_id="req-2",
        )

    def test_partial_and_unknown_receipts_publish_no_child_reference(self):
        self.reserve()
        self.registry.arm_start("req-1", FINGERPRINT, 0)
        unknown = self.registry.record_start_receipt(
            "req-1", FINGERPRINT, {"status": "unknown"})
        self.assertEqual(unknown["receipt_status"], "unknown")
        self.assertIsNone(unknown["child_task_id"])
        self.assertIsNone(unknown["standby_turn_id"])
        self.assertEqual(unknown["state"], "create_armed")
        partial = self.registry.record_start_receipt(
            "req-1", FINGERPRINT,
            {"status": "accepted", "threadId": "child-1", "extra": "ignored"},
        )
        self.assertEqual(partial["receipt_status"], "partial")
        self.assertIsNone(partial["child_task_id"])
        self.assertIsNone(partial["standby_turn_id"])

    def test_only_an_accepted_receipt_with_both_ids_can_be_attached(self):
        self.reserve()
        self.arm_and_accept()
        relationship = self.register_managed()
        attached = self.registry.start_request("req-1")
        self.assertEqual(attached["state"], "attached")
        self.assertEqual(attached["relationship_id"], relationship["relationshipId"])
        self.assertEqual(attached["execution_generation"], 1)
        self.assertEqual(attached["child_task_id"], "child-1")
        self.assertEqual(attached["standby_turn_id"], "standby-1")
        replay = self.register_managed()
        self.assertEqual(replay["relationshipId"], relationship["relationshipId"])
        self.assertEqual(self.registry.start_request("req-1")["state"], "attached")
        replayed_receipt = self.registry.record_start_receipt(
            "req-1", FINGERPRINT, self.accepted())
        self.assertEqual(replayed_receipt["state"], "attached")
        self.assertEqual(replayed_receipt["child_task_id"], "child-1")
        self.assertEqual(replayed_receipt["standby_turn_id"], "standby-1")
        self.assertEqual(replayed_receipt["revision"], attached["revision"])
        replayed_reserve = self.reserve()
        self.assertEqual(replayed_reserve["state"], "attached")
        self.assertEqual(replayed_reserve["request_id"], "req-1")
        self.assertEqual(replayed_reserve["relationship_id"], relationship["relationshipId"])
        self.assertEqual(replayed_reserve["revision"], attached["revision"])
        self.assertEqual(replayed_reserve["child_task_id"], "child-1")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT,
            self.registry.record_start_receipt,
            "req-1", FINGERPRINT,
            {"status": "accepted", "threadId": "other-child", "turnId": "standby-1"},
        )

    def test_raw_register_collides_with_a_pending_reservation(self):
        self.reserve()
        self.assertRefused(
            RefusalReason.DUPLICATE_ASSIGNMENT,
            self.registry.register,
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("other-child", HOST, cwd="/work"),
            issue_key=ISSUE,
            artifact_roots=["/work"],
            allowed_recipients=[PARENT],
            dispatch_request_id="raw-dispatch",
        )
        self.assertIsNone(
            self.store.one(
                "SELECT relationship_id FROM relationships WHERE issue_key = ?", (ISSUE,))
        )
        self.assertEqual(self.registry.start_request("req-1")["state"], "reserved")

    def test_attach_refuses_a_child_the_receipt_did_not_publish(self):
        self.reserve()
        self.arm_and_accept()
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT,
            self.register_managed, child="someone-else",
        )
        self.assertEqual(self.registry.start_request("req-1")["state"], "create_armed")
        self.assertIsNone(
            self.store.one(
                "SELECT relationship_id FROM relationships WHERE issue_key = ?", (ISSUE,))
        )

    def test_release_and_arm_of_the_same_revision_have_one_winner(self):
        self.reserve()
        path = str(self.store.path)
        barrier = threading.Barrier(2)
        results = {}

        def attempt(name, call):
            connection = Store(path)
            try:
                registry = Registry(connection, self.clock)
                barrier.wait(timeout=5)
                try:
                    results[name] = ("ok", call(registry))
                except Exception as error:
                    results[name] = (
                        "err", type(error).__name__, getattr(error, "reason", None))
            finally:
                connection.close()

        threads = [
            threading.Thread(
                target=attempt,
                args=("arm", lambda registry: registry.arm_start("req-1", FINGERPRINT, 0)),
            ),
            threading.Thread(
                target=attempt,
                args=("release", lambda registry: registry.release_unstarted(
                    "req-1", FINGERPRINT, 0, "operator")),
            ),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=10)
        self.assertFalse(any(thread.is_alive() for thread in threads))
        winners = [name for name, outcome in results.items() if outcome[0] == "ok"]
        losers = [name for name, outcome in results.items() if outcome[0] == "err"]
        self.assertEqual(len(winners), 1, results)
        self.assertEqual(len(losers), 1, results)
        self.assertEqual(results[losers[0]][2], RefusalReason.RELATIONSHIP_CONFLICT)
        retained = self.registry.start_request("req-1")
        self.assertEqual(retained["revision"], 1)
        self.assertIn(retained["state"], ("create_armed", "released"))
        if retained["state"] == "released":
            self.assertEqual(retained["release_reason"], "operator")
            self.assertRefused(RefusalReason.RELATIONSHIP_CONFLICT, self.reserve)
        else:
            self.assertRefused(
                RefusalReason.RELATIONSHIP_CONFLICT,
                self.registry.release_unstarted, "req-1", FINGERPRINT, 1, "too-late",
            )

    def test_two_connections_cannot_reserve_one_issue(self):
        path = str(self.store.path)
        barrier = threading.Barrier(2)
        results = {}

        def attempt(name, request_id):
            connection = Store(path)
            try:
                registry = Registry(connection, FakeClock())
                barrier.wait(timeout=5)
                try:
                    results[name] = (
                        "ok", registry.reserve_start(identity(request_id=request_id)))
                except Exception as error:
                    results[name] = ("err", getattr(error, "reason", None))
            finally:
                connection.close()

        threads = [
            threading.Thread(target=attempt, args=("a", "req-a")),
            threading.Thread(target=attempt, args=("b", "req-b")),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=10)
        winners = [name for name, outcome in results.items() if outcome[0] == "ok"]
        self.assertEqual(len(winners), 1, results)
        pending = self.store.all(
            "SELECT request_id FROM managed_start_requests"
            " WHERE issue_key = ? AND state IN ('reserved','create_armed')",
            (ISSUE,),
        )
        self.assertEqual(len(pending), 1)

    def test_an_active_or_paused_relationship_excludes_a_new_reservation(self):
        self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("child-live", HOST, cwd="/work"),
            issue_key=ISSUE,
            artifact_roots=["/work"],
            allowed_recipients=[PARENT],
            dispatch_request_id="live-dispatch",
            dispatch_turn_id="live-turn",
        )
        self.assertRefused(RefusalReason.DUPLICATE_ASSIGNMENT, self.reserve)
        rid = self.store.one(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?", (ISSUE,)
        )["relationship_id"]
        self.registry.set_status(rid, "paused", actor="operator")
        self.assertRefused(RefusalReason.DUPLICATE_ASSIGNMENT, self.reserve)
        self.registry.set_status(rid, "archived", actor="operator")
        reserved = self.reserve()
        self.assertEqual(reserved["state"], "reserved")

    def test_resume_cannot_pass_a_pending_reservation_for_its_issue(self):
        # Reserve first, then pause by inserting the relationship around the hold.
        # Public reserve refuses an issue that is already active or paused, which is
        # covered by the relationship exclusion test. Resume is the other acquisition,
        # so the pending row has to exist before the relationship is paused.
        reserved = self.reserve(issue_key="CRW-181")
        self.store.db.execute(
            "UPDATE managed_start_requests SET state = 'released', revision = 1,"
            " release_reason = 'make-room' WHERE request_id = ?",
            (reserved["request_id"],),
        )
        relationship = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("child-paused", HOST, cwd="/work"),
            issue_key="CRW-181",
            artifact_roots=["/work"],
            allowed_recipients=[PARENT],
            dispatch_request_id="paused-dispatch",
            dispatch_turn_id="paused-turn",
        )
        self.registry.set_status(relationship["relationshipId"], "paused", actor="operator")
        self.store.db.execute(
            "UPDATE managed_start_requests SET state = 'reserved', revision = 0,"
            " release_reason = NULL WHERE request_id = ?",
            (reserved["request_id"],),
        )
        self.assertRefused(
            RefusalReason.DUPLICATE_ASSIGNMENT,
            self.registry.resume,
            relationship["relationshipId"],
            expect_generation=1,
            expect_artifact_roots=["/work"],
            expect_allowed_recipients=[PARENT],
            actor="operator",
        )
        status = self.store.one(
            "SELECT status FROM relationships WHERE relationship_id = ?",
            (relationship["relationshipId"],),
        )["status"]
        self.assertEqual(status, "paused")

    def test_checkpoint_ids_survive_replay_of_the_same_fingerprint(self):
        self.reserve()
        armed = self.registry.arm_start("req-1", FINGERPRINT, 0)
        self.assertEqual(armed["revision"], 1)
        replayed = self.registry.arm_start("req-1", FINGERPRINT, 0)
        self.assertEqual(replayed["revision"], 1)
        self.assertEqual(replayed["state"], "create_armed")
        recorded = self.registry.record_start_receipt(
            "req-1", FINGERPRINT, self.accepted())
        again = self.registry.record_start_receipt(
            "req-1", FINGERPRINT, self.accepted())
        self.assertEqual(again["child_task_id"], recorded["child_task_id"])
        self.assertEqual(again["standby_turn_id"], "standby-1")
        self.assertEqual(again["revision"], recorded["revision"])

    def test_ensure_only_keeps_an_equal_record_and_refuses_a_different_one(self):
        original = task_settings("/parent")
        recorded = record_settings(
            self.store, self.clock, PARENT, original, source="creation_result")
        self.assertEqual(recorded["source"], "creation_result")
        ensured = record_settings(
            self.store, self.clock, PARENT, original, source="recovery", ensure_only=True)
        self.assertEqual(ensured["source"], "creation_result")
        self.assertEqual(ensured["settings"]["cwd"], "/parent")
        changed = task_settings("/parent", model="other-model")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT,
            record_settings,
            self.store, self.clock, PARENT, changed, source="recovery", ensure_only=True,
        )
        retained = self.store.one(
            "SELECT source, settings FROM authorized_settings WHERE task_id = ?", (PARENT,)
        )
        self.assertEqual(retained["source"], "creation_result")
        self.assertIn("anthropic/claude-opus-5", retained["settings"])
        absent = record_settings(
            self.store, self.clock, "task-new", original, source="recovery", ensure_only=True)
        self.assertEqual(absent["source"], "recovery")
        self.assertEqual(absent["taskId"], "task-new")

    def test_release_forbids_reusing_the_request_and_ignores_an_armed_row(self):
        self.reserve()
        released = self.registry.release_unstarted("req-1", FINGERPRINT, 0, "stopped")
        self.assertEqual(released["state"], "released")
        self.assertEqual(released["revision"], 1)
        self.assertEqual(released["release_reason"], "stopped")
        self.assertRefused(RefusalReason.RELATIONSHIP_CONFLICT, self.reserve)
        self.reserve(request_id="req-2", issue_key="CRW-182")
        self.registry.arm_start("req-2", FINGERPRINT, 0)
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT,
            self.registry.release_unstarted, "req-2", FINGERPRINT, 1, "after-arm",
        )
        self.assertEqual(self.registry.start_request("req-2")["state"], "create_armed")

    def test_unique_pending_index_is_the_database_constraint(self):
        self.reserve()
        with self.assertRaises(sqlite3.IntegrityError):
            self.store.db.execute(
                "INSERT INTO managed_start_requests (request_id, issue_key,"
                " request_fingerprint, fingerprint_version, workspace, marker_root,"
                " socket_identity, create_request_id, dispatch_request_id, state,"
                " revision, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,'reserved',0,?,?)",
                ("req-direct", ISSUE, FINGERPRINT, "1", "/work", "/markers", "sock-a",
                 "create-x", "dispatch-x", self.clock.iso(), self.clock.iso()),
            )


if __name__ == "__main__":
    unittest.main()
