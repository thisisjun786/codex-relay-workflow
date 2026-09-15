"""The durable store: a failed transition is never a success."""

import os
import unittest

from codex_session_relay.store import SCHEMA_VERSION, Store, state_dir

from .support import RelayTestCase

EXPECTED_TABLES = {
    "acks", "attempts", "deliveries", "events", "generations", "journal", "observations",
    "recipient_lifecycle", "recipient_rate", "refusals", "relationships", "schema_meta",
    "verdicts", "verification_claims",
}


class Schema(RelayTestCase):
    def test_schema_v1_carries_every_table_the_later_phases_need(self):
        names = {r[0] for r in self.store.all("SELECT name FROM sqlite_master WHERE type='table'")}
        self.assertTrue(EXPECTED_TABLES <= names, EXPECTED_TABLES - names)
        self.assertEqual(
            self.store.one("SELECT value FROM schema_meta WHERE key='version'")["value"],
            str(SCHEMA_VERSION),
        )

    def test_columns_the_delivery_and_ack_layers_need_exist_now(self):
        def columns(table):
            return {r["name"] for r in self.store.all(f"PRAGMA table_info({table})")}

        self.assertTrue(
            {"kind", "recipient_task_id", "recipient_thread_id", "lease_owner", "lease_until",
             "hold_reason", "dispatch_evidence", "provenance"} <= columns("deliveries")
        )
        self.assertTrue(
            {"internal_state", "sealed", "operation_observation", "recipient_scan",
             "affirmative_evidence"} <= columns("attempts")
        )
        self.assertIn("verified", columns("acks"))
        self.assertTrue(
            {"archived", "goal_status", "can_accept_input", "deliverable", "withhold_reason"}
            <= columns("recipient_lifecycle")
        )

    def test_the_database_file_is_private(self):
        self.assertEqual(os.stat(self.store.path).st_mode & 0o777, 0o600)


class Atomicity(RelayTestCase):
    def test_an_exception_inside_a_transaction_leaves_no_partial_row(self):
        try:
            with self.store.transaction() as db:
                db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k','s','d')")
                raise RuntimeError("interrupted mid-write")
        except RuntimeError:
            pass
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM journal")["c"], 0)

    def test_a_fault_after_the_body_still_rolls_back(self):
        def fault():
            raise RuntimeError("storage failed at commit time")

        self.store.fault_hook = fault
        try:
            with self.store.transaction() as db:
                db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k','s','d')")
        except RuntimeError:
            pass
        finally:
            self.store.fault_hook = None
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM journal")["c"], 0)

    def test_a_failed_registration_is_not_a_registration(self):
        self.store.fault_hook = lambda: (_ for _ in ()).throw(RuntimeError("disk full"))
        with self.assertRaises(RuntimeError):
            self.register()
        self.store.fault_hook = None
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM relationships")["c"], 0)
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM generations")["c"], 0)

    def test_state_survives_reopening_the_database(self):
        relationship = self.register()
        self.store.db.close()
        reopened = Store(self.store.path)
        self.addCleanup(reopened.close)
        row = reopened.one(
            "SELECT * FROM relationships WHERE relationship_id = ?",
            (relationship["relationshipId"],),
        )
        self.assertIsNotNone(row)


class StateDirectory(unittest.TestCase):
    def test_explicit_override_wins(self):
        os.environ["CODEX_SESSION_RELAY_STATE"] = "/tmp/relay-state-test"
        try:
            self.assertEqual(str(state_dir()), "/tmp/relay-state-test")
        finally:
            del os.environ["CODEX_SESSION_RELAY_STATE"]

    def test_a_socket_gets_its_own_endpoint_directory(self):
        os.environ.pop("CODEX_SESSION_RELAY_STATE", None)
        first = state_dir("/run/one.sock")
        second = state_dir("/run/two.sock")
        self.assertNotEqual(first, second)
        self.assertIn("codex-session-relay", str(first))


if __name__ == "__main__":
    unittest.main()
