"""Criteria registration that must not replace a set it only meant to ensure.

register is the intentional replacement and stays that. ensure_registered is the replay a
recoverable admission makes: the same normalised set, digest, source and managed mode is
left exactly as stored, and anything else is refused without repairing it.
"""

import os
import shutil
import tempfile
import threading
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.criteria import MANAGED, CriteriaService, set_digest
from codex_session_relay.errors import AckRefused, RefusalReason
from codex_session_relay.store import Store


SET = [
    {"id": " c2 ", "title": " malformed requests are refused ", "required": 0},
    {"id": "c1", "title": "the endpoint returns the agreed shape"},
]
NORMALISED = [
    {"id": "c1", "title": "the endpoint returns the agreed shape", "required": True},
    {"id": "c2", "title": "malformed requests are refused", "required": False},
]
SOURCE = "https://linear.app/doc/1"


class CriteriaRegistration(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="criteria-registration-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.clock = FakeClock()
        self.store = Store(os.path.join(self.tmp, "relay.sqlite3"))
        self.addCleanup(self.store.close)
        self.criteria = CriteriaService(self.store, self.clock)

    def rows(self, relationship_id="rel-1"):
        return self.store.all(
            "SELECT criterion_id, title, required, source_ref, set_digest, recorded_at"
            " FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id",
            (relationship_id,),
        )

    def journal(self, relationship_id="rel-1"):
        return self.store.all(
            "SELECT at, kind, subject, detail FROM journal WHERE subject = ? ORDER BY rowid",
            (relationship_id,),
        )

    def test_an_empty_or_duplicate_set_is_refused_before_any_row_exists(self):
        for entries in (
            [],
            [{"id": "c1", "title": "   "}],
            [{"id": "c1", "title": "one"}, "not-an-object"],
            [{"id": "c1", "title": "one"}, {"id": " c1 ", "title": "again"}],
        ):
            with self.subTest(entries=entries):
                with self.assertRaises(AckRefused) as raised:
                    self.criteria.ensure_registered("rel-1", entries, source_ref=SOURCE)
                self.assertEqual(raised.exception.reason, RefusalReason.CRITERIA_UNREGISTERED)
        self.assertEqual(self.rows(), [])
        self.assertEqual(self.journal(), [])
        self.assertEqual(self.criteria.mode("rel-1"), "legacy")

    def test_the_first_registration_normalises_and_returns_the_managed_set(self):
        recorded = self.criteria.ensure_registered("rel-1", list(reversed(SET)), source_ref=SOURCE)

        self.assertEqual(recorded["mode"], MANAGED)
        self.assertEqual(recorded["criteria"], NORMALISED)
        self.assertEqual(recorded["setDigest"], set_digest(NORMALISED))
        self.assertEqual(recorded["sourceRef"], SOURCE)
        self.assertEqual(
            [(row["criterion_id"], row["title"], bool(row["required"])) for row in self.rows()],
            [(entry["id"], entry["title"], entry["required"]) for entry in NORMALISED],
        )
        self.assertEqual({row["set_digest"] for row in self.rows()}, {recorded["setDigest"]})
        self.assertEqual(self.criteria.mode("rel-1"), MANAGED)
        self.assertEqual(len(self.journal()), 1)

    def test_an_exact_replay_keeps_the_original_timestamp_and_writes_no_journal(self):
        self.criteria.ensure_registered("rel-1", SET, source_ref=SOURCE)
        recorded_at = self.rows()[0]["recorded_at"]
        journal_before = self.journal()
        self.clock.advance(30)

        replay = [
            {"id": "c2", "title": "malformed requests are refused", "required": False},
            {"id": "c1", "title": " the endpoint returns the agreed shape "},
        ]
        again = self.criteria.ensure_registered("rel-1", replay, source_ref=SOURCE)

        self.assertEqual(again["criteria"], [
            {"id": "c2", "title": "malformed requests are refused", "required": False},
            {"id": "c1", "title": "the endpoint returns the agreed shape", "required": True},
        ])
        self.assertEqual(again["mode"], MANAGED)
        self.assertEqual({row["recorded_at"] for row in self.rows()}, {recorded_at})
        self.assertEqual(self.journal(), journal_before)
        mode = self.store.one(
            "SELECT recorded_at FROM verification_mode WHERE relationship_id = ?", ("rel-1",)
        )
        self.assertEqual(mode["recorded_at"], recorded_at)

    def test_a_changed_title_source_or_requirement_refuses_without_rewriting(self):
        self.criteria.ensure_registered("rel-1", SET, source_ref=SOURCE)
        before = [tuple(row) for row in self.rows()]
        journal_before = self.journal()
        changed = [
            (
                [
                    {"id": "c1", "title": "the endpoint returns the agreed shape"},
                    {"id": "c2", "title": "a different refusal", "required": False},
                ],
                SOURCE,
            ),
            (SET, "https://linear.app/doc/2"),
            (
                [
                    {"id": "c2", "title": "malformed requests are refused", "required": True},
                    {"id": "c1", "title": "the endpoint returns the agreed shape"},
                ],
                SOURCE,
            ),
            (SET + [{"id": "c3", "title": "one more obligation"}], SOURCE),
        ]
        for entries, source in changed:
            with self.subTest(source=source, count=len(entries)):
                with self.assertRaises(AckRefused) as raised:
                    self.criteria.ensure_registered("rel-1", entries, source_ref=source)
                self.assertEqual(raised.exception.reason, RefusalReason.CRITERIA_SET_CHANGED)
        self.assertEqual([tuple(row) for row in self.rows()], before)
        self.assertEqual(self.journal(), journal_before)

    def test_register_still_replaces_a_set_ensure_registered_would_keep(self):
        self.criteria.ensure_registered("rel-1", SET, source_ref=SOURCE)
        self.clock.advance(5)
        replacement = [{"id": "c9", "title": "the replacement obligation", "required": False}]

        replaced = self.criteria.register("rel-1", replacement, source_ref="operator")

        self.assertEqual(replaced["criteria"], replacement)
        self.assertEqual(replaced["mode"], MANAGED)
        self.assertEqual(replaced["sourceRef"], "operator")
        self.assertEqual(
            [row["criterion_id"] for row in self.rows()], ["c9"]
        )
        self.assertEqual(len(self.journal()), 2)
        with self.assertRaises(AckRefused) as raised:
            self.criteria.ensure_registered("rel-1", SET, source_ref=SOURCE)
        self.assertEqual(raised.exception.reason, RefusalReason.CRITERIA_SET_CHANGED)
        self.assertEqual([row["criterion_id"] for row in self.rows()], ["c9"])

    def test_a_corrupt_mixed_or_non_managed_set_is_refused_and_left_in_place(self):
        digest = set_digest(NORMALISED)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)",
                ("rel-1", "c1", NORMALISED[0]["title"], 1, SOURCE, digest, now),
            )
            db.execute(
                "INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)",
                ("rel-1", "c2", NORMALISED[1]["title"], 0, "other-source", "not-the-digest", now),
            )
            db.execute(
                "INSERT INTO verification_mode VALUES (?,?,?)", ("rel-1", "legacy", now)
            )
        before = [tuple(row) for row in self.rows()]

        with self.assertRaises(AckRefused) as raised:
            self.criteria.ensure_registered("rel-1", SET, source_ref=SOURCE)
        self.assertEqual(raised.exception.reason, RefusalReason.CRITERIA_SET_CHANGED)
        self.assertEqual([tuple(row) for row in self.rows()], before)
        self.assertEqual(self.criteria.mode("rel-1"), "legacy")
        self.assertEqual(self.journal(), [])

    def test_a_required_flag_other_than_zero_or_one_is_refused_and_left_in_place(self):
        digest = set_digest(NORMALISED)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)",
                ("rel-1", "c1", NORMALISED[0]["title"], 2, SOURCE, digest, now),
            )
            db.execute(
                "INSERT INTO canonical_criteria VALUES (?,?,?,?,?,?,?)",
                ("rel-1", "c2", NORMALISED[1]["title"], 0, SOURCE, digest, now),
            )
            db.execute(
                "INSERT INTO verification_mode VALUES (?,?,?)", ("rel-1", "managed", now)
            )
        before = [tuple(row) for row in self.rows()]

        with self.assertRaises(AckRefused) as raised:
            self.criteria.ensure_registered("rel-1", SET, source_ref=SOURCE)
        self.assertEqual(raised.exception.reason, RefusalReason.CRITERIA_SET_CHANGED)
        self.assertEqual([tuple(row) for row in self.rows()], before)
        self.assertEqual(self.journal(), [])

    def test_two_connections_registering_different_sets_keep_exactly_one(self):
        barrier = threading.Barrier(2)
        errors = []
        lock = threading.Lock()

        def attempt(entries, source):
            store = Store(self.store.path)
            try:
                barrier.wait(timeout=20)
                CriteriaService(store, FakeClock()).ensure_registered(
                    "rel-1", entries, source_ref=source
                )
            except AckRefused as error:
                with lock:
                    errors.append(error.reason)
            finally:
                store.close()

        other = [{"id": "c9", "title": "a rival obligation"}]
        threads = [
            threading.Thread(target=attempt, args=(SET, SOURCE)),
            threading.Thread(target=attempt, args=(other, "rival")),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        for thread in threads:
            self.assertFalse(thread.is_alive())

        stored = [(row["criterion_id"], row["source_ref"]) for row in self.rows()]
        self.assertIn(stored, [
            [("c1", SOURCE), ("c2", SOURCE)],
            [("c9", "rival")],
        ])
        self.assertEqual(len({row["set_digest"] for row in self.rows()}), 1)
        self.assertEqual(len(errors), 1)
        self.assertEqual(errors[0], RefusalReason.CRITERIA_SET_CHANGED)
        self.assertEqual(self.criteria.mode("rel-1"), MANAGED)
        self.assertEqual(len(self.journal()), 1)

    def test_two_connections_replaying_the_same_set_keep_one_timestamp(self):
        barrier = threading.Barrier(2)
        failures = []

        def attempt():
            store = Store(self.store.path)
            clock = FakeClock()
            clock.advance(threading.get_ident() % 1000)
            try:
                barrier.wait(timeout=20)
                CriteriaService(store, clock).ensure_registered("rel-1", SET, source_ref=SOURCE)
            except Exception as error:  # noqa: BLE001 - the caller asserts there is none
                failures.append(error)
            finally:
                store.close()

        threads = [threading.Thread(target=attempt) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)

        self.assertEqual(failures, [])
        recorded = {row["recorded_at"] for row in self.rows()}
        self.assertEqual(len(recorded), 1)
        self.assertEqual(len(self.journal()), 1)
        self.assertEqual(
            [row["criterion_id"] for row in self.rows()], ["c1", "c2"]
        )


if __name__ == "__main__":
    unittest.main()
