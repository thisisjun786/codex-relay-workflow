"""The create-once write protocol, which the contract left specified but unexercised.

hook-contract.md says in so many words that no fixture creates a file, races a link() or kills a
writer, so the atomicity argument there is reasoned from the system call rather than tested. These
tests are the part that actually runs it.

Every test builds its own marker root under a temporary directory and passes it explicitly, so none
of them can reach the user's real state: nothing here calls resolve_marker_root without first
pinning the environment it reads.
"""

import json
import os
import shutil
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

from codex_session_relay import marker


class MarkerTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-marker-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.root = Path(self.tmp) / "markers"
        self.workspace = Path(self.tmp) / "work"
        self.workspace.mkdir()


class Publication(MarkerTestCase):
    def test_the_first_writer_wins_and_the_second_is_told_it_lost(self):
        target = self.root / "a" / "intent.json"
        self.assertEqual(marker.publish(target, {"one": 1}), marker.PUBLISHED)
        self.assertEqual(marker.publish(target, {"two": 2}), marker.EXISTS)
        self.assertEqual(json.loads(target.read_text(encoding="utf-8")), {"one": 1})

    def test_concurrent_publication_produces_exactly_one_winner(self):
        """link() is the whole reason first-publication-wins is not a convention."""
        target = self.root / "a" / "bound.json"
        target.parent.mkdir(parents=True)
        barrier = threading.Barrier(8)
        outcomes = []
        lock = threading.Lock()

        def race(index):
            barrier.wait()
            outcome = marker.publish(target, {"writer": index})
            with lock:
                outcomes.append(outcome)

        threads = [threading.Thread(target=race, args=(i,)) for i in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        self.assertEqual(outcomes.count(marker.PUBLISHED), 1, outcomes)
        self.assertEqual(outcomes.count(marker.EXISTS), 7, outcomes)
        # And the survivor is a whole record, never a torn one.
        self.assertIn("writer", json.loads(target.read_text(encoding="utf-8")))

    def test_publication_leaves_no_temp_behind(self):
        target = self.root / "a" / "intent.json"
        marker.publish(target, {"one": 1})
        marker.publish(target, {"two": 2})
        leftovers = [p.name for p in target.parent.iterdir() if p.name != "intent.json"]
        self.assertEqual(leftovers, [])

    def test_an_orphan_temp_is_not_a_fact(self):
        """A writer that dies mid-write leaves a temp and no target, so readers see absence."""
        directory = marker.assignment_dir(self.root, self.workspace, "assign")
        (directory / "attempts").mkdir(parents=True)
        (directory / "attempts" / ".0.json.tmp.999.deadbeef").write_text("{}", encoding="utf-8")
        facts, unreadable = marker.read_assignment(directory)
        self.assertEqual(facts.get("attempts"), [])
        self.assertEqual(unreadable, [])

    def test_a_retry_after_an_orphan_temp_succeeds(self):
        target = self.root / "a" / "intent.json"
        target.parent.mkdir(parents=True)
        (target.parent / ".intent.json.tmp.1.abc").write_text("{}", encoding="utf-8")
        self.assertEqual(marker.publish(target, {"one": 1}), marker.PUBLISHED)


class Digest(MarkerTestCase):
    def test_the_contract_vector_reproduces_byte_for_byte(self):
        """An independent writer must reproduce this exactly, so the spelling is pinned."""
        fact = {"factId": "conflicts/0", "at": "2026-01-01T00:06:00+00:00"}
        self.assertEqual(
            marker.fact_digest(fact),
            "30250e28118e703a042e74d53844e078bbd318ae45a4479eac217c385a5c284a",
        )

    def test_factid_is_excluded_so_the_reader_may_assign_it(self):
        self.assertEqual(
            marker.fact_digest({"factId": "a", "x": 1}),
            marker.fact_digest({"factId": "b", "x": 1}),
        )


class Identity(MarkerTestCase):
    def test_nothing_names_nothing(self):
        for value in (None, "", "   ", 3, [], {}, True):
            self.assertFalse(marker.named(value), repr(value))

    def test_two_records_naming_nothing_never_match(self):
        self.assertFalse(marker.same_identity(None, None))
        self.assertFalse(marker.same_identity("", ""))
        self.assertFalse(marker.same_identity("  ", "  "))
        self.assertFalse(marker.same_identity("a", None))
        self.assertTrue(marker.same_identity("a", "a"))


class Reading(MarkerTestCase):
    def test_the_reader_assigns_the_factid_from_the_path_it_walked(self):
        directory = marker.assignment_dir(self.root, self.workspace, "assign")
        marker.publish(directory / "intent.json", {"issueKey": "REL-1"})
        marker.publish(directory / "attempts" / "0.json", {"outcome": "accepted"})
        marker.publish(directory / "claims" / "sess" / "claim.json", {"sessionId": "sess"})
        facts, unreadable = marker.read_assignment(directory)
        self.assertEqual(facts["intent"]["factId"], "intent")
        self.assertEqual(facts["attempts"][0]["factId"], "attempts/0")
        self.assertEqual(facts["claims"][0]["factId"], "claims/sess/claim.json")
        self.assertEqual(unreadable, [])

    def test_an_unreadable_fact_is_reported_and_never_read_as_absent(self):
        directory = marker.assignment_dir(self.root, self.workspace, "assign")
        directory.mkdir(parents=True)
        (directory / "intent.json").write_text("{not json", encoding="utf-8")
        facts, unreadable = marker.read_assignment(directory)
        self.assertNotIn("intent", facts)
        self.assertEqual(unreadable, ["intent"])

    def test_a_fact_that_is_not_a_record_reaches_the_reader_as_the_wrong_shape(self):
        directory = marker.assignment_dir(self.root, self.workspace, "assign")
        marker.publish(directory / "claims" / "sess" / "claim.json", {"sessionId": "sess"})
        (directory / "claims" / "sess" / "claim.json").unlink()
        (directory / "claims" / "sess" / "claim.json").write_text('"bare"', encoding="utf-8")
        facts, _ = marker.read_assignment(directory)
        self.assertEqual(facts["claims"], ["bare"])

    def test_a_symlinked_workspace_reaches_the_same_assignment(self):
        link = Path(self.tmp) / "alias"
        os.symlink(self.workspace, link)
        self.assertEqual(marker.workspace_key(link), marker.workspace_key(self.workspace))

    def test_a_disposition_is_read_at_the_path_the_stop_identity_derives(self):
        directory = marker.assignment_dir(self.root, self.workspace, "assign")
        marker.publish(
            directory / "dispositions" / "sess" / "turn-1.json",
            {"sessionId": "sess", "turnId": "turn-1", "outcome": "interrupted"},
        )
        found, readable = marker.read_disposition(directory, "sess", "turn-1")
        self.assertTrue(readable)
        self.assertEqual(found["outcome"], "interrupted")
        # Another turn's declaration can never answer for this one.
        other, readable = marker.read_disposition(directory, "sess", "turn-2")
        self.assertTrue(readable)
        self.assertIsNone(other)

    def test_an_unnamed_identity_reads_no_disposition_rather_than_a_stray_path(self):
        directory = marker.assignment_dir(self.root, self.workspace, "assign")
        found, readable = marker.read_disposition(directory, "sess", "")
        self.assertIsNone(found)
        self.assertTrue(readable)


class RootResolution(MarkerTestCase):
    def setUp(self):
        super().setUp()
        # Never read the real home or the real XDG state during a test, even to compute a default.
        patcher = mock.patch.dict(
            os.environ, {"HOME": str(Path(self.tmp) / "home")}, clear=False
        )
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop(marker.MARKER_ENV, None)
        os.environ.pop("XDG_STATE_HOME", None)

    def test_precedence_is_flag_then_env_then_xdg_then_home(self):
        self.assertEqual(marker.resolve_marker_root("/explicit").source, "flag")
        os.environ[marker.MARKER_ENV] = "/from-env"
        self.assertEqual(marker.resolve_marker_root().source, "env")
        os.environ.pop(marker.MARKER_ENV)
        os.environ["XDG_STATE_HOME"] = str(Path(self.tmp) / "xdg")
        self.assertEqual(marker.resolve_marker_root().source, "xdg")
        os.environ.pop("XDG_STATE_HOME")
        self.assertEqual(marker.resolve_marker_root().source, "home")

    def test_the_marker_root_is_not_inside_the_relay_state_directory(self):
        """The contract gives the relay daemon no access to the marker at all."""
        os.environ["XDG_STATE_HOME"] = str(Path(self.tmp) / "xdg")
        chosen = marker.resolve_marker_root().path
        self.assertNotIn("codex-session-relay", chosen.parts)


if __name__ == "__main__":
    unittest.main()
