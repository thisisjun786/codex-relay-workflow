"""The Stop adapter this package now carries, on its own terms.

The agreement test in the repository's scripts/ci/tests compares this adapter with the checkout
copy. This one asks the questions that only matter once the package is installed: that the console
script's entry point is reachable, that it cannot fail a turn, and that what it writes is written
where the settings said and readable only by its owner.
"""

import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
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


# ----------------------------------------------------------------- one accepted record per Stop event
#
# CRW-212. Everything below runs the adapter the way the packaged launcher does, as a program:
# [interpreter, stopadapter.py, settings] with the host's payload on stdin. The oracles are files
# (the stub guard's call log, the rows, the accepted records) rather than module attributes, so a
# copy of this adapter that predates event identity fails these on an assertion about behaviour
# rather than on a missing name.

FIXTURE = Path(__file__).resolve().parent / "fixtures" / "stop_event_r1.json"


def r1():
    """Three Stops of one turn from an isolated Codex 0.154.0 run, each seen by two registrations.

    Derived, not copied: only the transcript lines that carry identity are kept and every path is a
    placeholder. Stops 2 and 3 arrived with byte-identical payloads; only the transcript tells them
    apart, which is the case a payload digest or a (session, turn) key gets wrong.
    """
    return json.loads(FIXTURE.read_text(encoding="utf-8"))


def counting_guard(directory, *, decision="release", die_first=False):
    """A stub runtime that appends one line per call, so 'asked once' is a count of lines."""
    body = [
        "import json, os, sys",
        "raw = sys.stdin.buffer.read()",
        "calls = %r" % str(directory / "guard-calls"),
        "with open(calls, 'a', encoding='utf-8') as log:",
        "    log.write(json.dumps(json.loads(raw.decode('utf-8'))) + '\\n')",
    ]
    if die_first:
        # The owner of a claim dying before it can record an outcome: the adapter is this stub's
        # parent, and the first call ends it with SIGKILL, the way a host deadline would.
        body += ["if sum(1 for _ in open(calls)) == 1:", "    os.kill(os.getppid(), 9)",
                 "    raise SystemExit(0)"]
    if decision == "block":
        body.append("sys.stdout.write(json.dumps({'decision': 'block', 'state': 'declared',"
                    " 'hook_output': {'decision': 'block', 'reason': 'verify the child',"
                    " 'continue': True}}))")
    else:
        body.append("sys.stdout.write(json.dumps({'decision': 'release', 'state': 'unmanaged',"
                    " 'hook_output': {}}))")
    path = directory / "relay"
    path.write_text("#!/usr/bin/env python3\n" + "\n".join(body) + "\n", encoding="utf-8")
    path.chmod(0o755)
    return path


def guard_calls(directory):
    try:
        return [json.loads(line) for line in (directory / "guard-calls").read_text().splitlines()]
    except FileNotFoundError:
        return []


def write_transcript(path, lines):
    path.write_text("".join(line + "\n" for line in lines), encoding="utf-8")


def stop_payload(stop, transcript, cwd):
    payload = dict(stop["payload"])
    payload["transcript_path"] = str(transcript)
    payload["cwd"] = str(cwd)
    return json.dumps(payload).encode("utf-8")


def fire(settings_path, payload):
    """One invocation through the entry point the launcher runs."""
    return subprocess.run([sys.executable, str(Path(stopadapter.__file__).resolve()),
                           str(settings_path)],
                          input=payload, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          timeout=60)


def fire_together(settings_path, payload, count=2):
    """Several invocations of one Stop, started before any of them is handed its payload."""
    started = [subprocess.Popen([sys.executable, str(Path(stopadapter.__file__).resolve()),
                                 str(settings_path)],
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE) for _ in range(count)]
    answers = [None] * count
    barrier = threading.Barrier(count)

    def feed(index):
        barrier.wait()
        answers[index] = started[index].communicate(payload, timeout=60)

    threads = [threading.Thread(target=feed, args=(index,)) for index in range(count)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    return [(process.returncode, out, err) for process, (out, err) in zip(started, answers)]


def rows(home):
    found = []
    journal = home / "journal"
    for day in sorted(journal.iterdir()) if journal.is_dir() else []:
        if day.is_dir() and re.match(r"^[0-9]{8}$", day.name):
            for entry in sorted(day.iterdir()):
                if re.match(r"^[0-9a-f]{32}\.json$", entry.name):
                    found.append(json.loads(entry.read_text(encoding="utf-8")))
    return found


def ledger(home):
    directory = home / "journal" / "accepted"
    names = sorted(p.name for p in directory.iterdir()) if directory.is_dir() else []
    return ([n for n in names if re.match(r"^[0-9a-f]{64}\.json$", n)],
            [n for n in names if re.match(r"^[0-9a-f]{64}\.outcome\.json$", n)])


class StopEventAcceptanceTests(unittest.TestCase):
    """The same Stop is accepted once; two Stops of one turn are two events."""

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.home = Path(raw)
        self.transcript = self.home / "rollout.jsonl"
        self.fixture = r1()

    def arrange(self, *, decision="release", die_first=False, **kwargs):
        return settings(self.home, counting_guard(self.home, decision=decision,
                                                  die_first=die_first), **kwargs)

    def at_stop(self, index):
        stop = self.fixture["stops"][index]
        write_transcript(self.transcript, self.fixture["transcriptLines"][:stop["linesAtStop"]])
        return stop_payload(stop, self.transcript, self.home)

    def test_the_same_stop_replayed_is_accepted_once_and_asked_about_once(self):
        path = self.arrange()
        payload = self.at_stop(0)
        first, second = fire(path, payload), fire(path, payload)
        self.assertEqual((first.returncode, second.returncode), (0, 0))
        self.assertEqual((first.stderr, second.stderr), (b"", b""))
        self.assertEqual(len(guard_calls(self.home)), 1,
                         "a replayed Stop event asked the guard again")
        written = rows(self.home)
        self.assertEqual(len(written), 2, "every invocation still leaves a row")
        self.assertEqual(sorted(str(r.get("acceptance")) for r in written),
                         ["accepted", "duplicate"])
        claims, outcomes = ledger(self.home)
        self.assertEqual((len(claims), len(outcomes)), (1, 1))

    def test_two_registrations_answering_one_stop_at_once_accept_it_once(self):
        payload = self.at_stop(0)
        for round_number in range(10):
            with self.subTest(round=round_number):
                journal = self.home / "journal"
                shutil.rmtree(journal, True)
                (self.home / "guard-calls").unlink(missing_ok=True)
                path = self.arrange()
                answers = fire_together(path, payload)
                self.assertEqual([code for code, _o, _e in answers], [0, 0])
                self.assertEqual(len(guard_calls(self.home)), 1,
                                 "two registrations on one Stop both asked the guard")
                self.assertEqual(sorted(str(r.get("acceptance")) for r in rows(self.home)),
                                 ["accepted", "duplicate"])
                self.assertEqual(len(ledger(self.home)[0]), 1)

    def test_distinct_stops_of_one_turn_are_each_accepted_and_answered(self):
        """The positive control. The guard holds on every call, so each event's hold must reach
        the host from exactly one of its two registrations: a continuation is never suppressed."""
        path = self.arrange(decision="block")
        stops = self.fixture["stops"]
        self.assertEqual(stops[2]["rawSha256"], stops[4]["rawSha256"],
                         "the fixture's Stops 2 and 3 arrived with byte-identical payloads")
        for index in range(0, 6, 2):
            payload = self.at_stop(index)
            answers = fire_together(path, payload)
            held = [json.loads(out) for _code, out, _err in answers if out]
            self.assertEqual(len(held), 1, "exactly one registration answers each event")
            self.assertEqual(held[0]["decision"], "block")
        self.assertEqual(len(guard_calls(self.home)), 3)
        claims, outcomes = ledger(self.home)
        self.assertEqual((len(claims), len(outcomes)), (3, 3))
        written = rows(self.home)
        self.assertEqual(sorted(str(r.get("acceptance")) for r in written),
                         ["accepted"] * 3 + ["duplicate"] * 3)
        self.assertEqual(len({r.get("eventKey") for r in written}), 3)
        self.assertEqual({r.get("turnId") for r in written}, {stops[0]["payload"]["turn_id"]})

    def test_an_identity_it_cannot_establish_is_asked_about_every_time(self):
        path = self.arrange()
        payload = json.dumps({"session_id": "s", "turn_id": "t", "stop_hook_active": False,
                              "last_assistant_message": "done"}).encode("utf-8")
        fire(path, payload)
        fire(path, payload)
        self.assertEqual(len(guard_calls(self.home)), 2)
        written = rows(self.home)
        self.assertEqual([r.get("acceptance") for r in written], ["unestablished"] * 2)
        self.assertEqual({(r.get("eventIdentity") or {}).get("reason") for r in written},
                         {"transcript_path_missing"})
        self.assertEqual(ledger(self.home), ([], []))

    def test_a_claim_whose_owner_died_is_not_answered_twice(self):
        path = self.arrange(die_first=True)
        payload = self.at_stop(0)
        killed = fire(path, payload)
        self.assertEqual(killed.returncode, -9)
        again = fire(path, payload)
        self.assertEqual(again.returncode, 0)
        self.assertEqual(again.stdout, b"")
        self.assertEqual(len(guard_calls(self.home)), 1)
        claims, outcomes = ledger(self.home)
        self.assertEqual((len(claims), len(outcomes)), (1, 0),
                         "the claim stays and names no outcome")
        self.assertEqual([r.get("acceptance") for r in rows(self.home)], ["duplicate"])

    def test_a_newer_input_with_no_answer_leaves_the_identity_unestablished(self):
        """A transcript whose newest item for the turn is a continuation prompt does not show this
        Stop's answer; taking the previous answer would merge two events."""
        path = self.arrange()
        stop = self.fixture["stops"][2]
        lines = self.fixture["transcriptLines"][:stop["linesAtStop"] - 1]
        self.assertIn("HookPrompt", lines[-1])
        write_transcript(self.transcript, lines)
        fire(path, stop_payload(stop, self.transcript, self.home))
        written = rows(self.home)
        self.assertEqual((written[0].get("eventIdentity") or {}).get("reason"),
                         "answer_precedes_latest_input")
        self.assertEqual(len(guard_calls(self.home)), 1)


if __name__ == "__main__":
    unittest.main()
