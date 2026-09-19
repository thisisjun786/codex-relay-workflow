"""The Stop adapter this package now carries, on its own terms.

The agreement test in the repository's scripts/ci/tests compares this adapter with the checkout
copy. This one asks the questions that only matter once the package is installed: that the console
script's entry point is reachable, that it cannot fail a turn, and that what it writes is written
where the settings said and readable only by its owner.
"""

import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from codex_session_relay import stopadapter


def settings(directory, relay, *, timeout=5, policy=None):
    document = {
        "configVersion": stopadapter.CONFIG_VERSION,
        "event": stopadapter.EVENT,
        "relayExecutable": str(relay),
        "markerRoot": str(directory / "marker"),
        "dbPath": None,
        "mode": stopadapter.OBSERVE,
        "timeoutSeconds": timeout,
        "journalRoot": str(directory / "journal"),
        "journalPolicy": policy or stopadapter.EVERY_INVOCATION,
        "installedBy": "CRW-115",
        "isolationAssertedBy": None,
        "owner": stopadapter.OWNER_PLUGIN,
        "adapterInterpreter": sys.executable,
        "adapterEntryPoint": str(Path(stopadapter.__file__).resolve()),
    }
    path = directory / stopadapter.CONFIG_NAME
    path.write_text(json.dumps(document, indent=2, sort_keys=True), encoding="utf-8")
    return path


def guard(directory, *, decision="release", code=0):
    """A stub runtime, because the outcome is derived from how the process ended as well."""
    body = "import json, sys\nsys.stdin.buffer.read()\n"
    if decision == "block":
        body += ("sys.stdout.write(json.dumps({'decision': 'block', 'state': 'declared',"
                 " 'hook_output': {'decision': 'block', 'reason': 'verify the child',"
                 " 'continue': True}}))\n")
    elif decision == "release":
        body += ("sys.stdout.write(json.dumps({'decision': 'release', 'state': 'unmanaged',"
                 " 'hook_output': {}}))\n")
    body += "raise SystemExit(%d)\n" % code
    path = directory / "relay"
    path.write_text("#!/usr/bin/env python3\n" + body, encoding="utf-8")
    path.chmod(0o755)
    return path


class StopAdapterTests(unittest.TestCase):
    def test_the_recorded_entry_point_is_this_module(self):
        """What the settings record has to be able to name, so the launcher can run it."""
        self.assertTrue(Path(stopadapter.__file__).is_file())
        self.assertTrue(callable(stopadapter.main))
        self.assertTrue(callable(stopadapter.run))

    def test_a_held_turn_prints_exactly_the_stop_json_the_host_accepts(self):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            path = settings(home, guard(home, decision="block"))
            answer = stopadapter.run(json.dumps({"session_id": "s", "turn_id": "t"}).encode(),
                                     settings=str(path))
            self.assertEqual(json.loads(answer),
                             {"decision": "block", "reason": "verify the child",
                              "continue": True})

    def test_a_released_turn_prints_nothing(self):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            path = settings(home, guard(home))
            self.assertIsNone(stopadapter.run(json.dumps({"session_id": "s"}).encode(),
                                              settings=str(path)))

    def test_the_record_is_written_where_the_settings_said_and_only_for_its_owner(self):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            path = settings(home, guard(home))
            stopadapter.run(json.dumps({"session_id": "s"}).encode(), settings=str(path))
            days = sorted((home / "journal").iterdir())
            self.assertEqual(len(days), 1)
            records = sorted(days[0].iterdir())
            self.assertEqual(len(records), 1)
            self.assertRegex(records[0].name, stopadapter.JOURNAL_NAME)
            self.assertEqual(stat.S_IMODE(records[0].stat().st_mode), 0o600)
            written = json.loads(records[0].read_text(encoding="utf-8"))
            self.assertEqual(written["adapterOutcome"], stopadapter.GUARD_ANSWERED)
            self.assertEqual(written["event"], stopadapter.EVENT)

    def test_a_payload_it_cannot_parse_is_recorded_rather_than_lost(self):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            path = settings(home, guard(home))
            self.assertIsNone(stopadapter.run(b"not json", settings=str(path)))
            record = next(next((home / "journal").iterdir()).iterdir())
            written = json.loads(record.read_text(encoding="utf-8"))
            self.assertEqual(written["adapterOutcome"], stopadapter.STDIN_NOT_JSON)

    def test_absent_settings_release_in_silence(self):
        with tempfile.TemporaryDirectory() as raw:
            self.assertIsNone(stopadapter.run(b"{}", settings=str(Path(raw) / "nothing.json")))

    def test_the_entry_point_exits_zero_and_says_nothing_on_stderr(self):
        """Run as the console script does, because exit 2 is the host's blocking code."""
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            path = settings(home, guard(home, decision="block"))
            done = subprocess.run(
                [sys.executable, "-c",
                 "from codex_session_relay import stopadapter; stopadapter.main()", str(path)],
                input=json.dumps({"session_id": "s"}).encode(),
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60,
                env={**os.environ, "PYTHONPATH": str(Path(stopadapter.__file__).parents[1])},
            )
            self.assertEqual(done.returncode, 0)
            self.assertEqual(done.stderr, b"")
            self.assertEqual(json.loads(done.stdout)["decision"], "block")

    def test_a_broken_runtime_never_holds_the_turn(self):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            path = settings(home, home / "does-not-exist")
            self.assertIsNone(stopadapter.run(b"{}", settings=str(path)))
            record = next(next((home / "journal").iterdir()).iterdir())
            written = json.loads(record.read_text(encoding="utf-8"))
            self.assertEqual(written["adapterOutcome"], stopadapter.GUARD_UNREACHABLE)
            self.assertFalse(written["held"])


if __name__ == "__main__":
    unittest.main()

