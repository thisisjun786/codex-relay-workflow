"""The completion hook adapter, exercised against a fake relay in temporary destinations.

Nothing here reads or changes a real Codex home, an installed runtime, an MCP registration or
an operational database. The relay is a script this file writes, so no case depends on what
happens to be installed on the machine running it, and a host whose relay predates the guard is
one of the cases rather than an obstacle to running them.

What these establish: that the adapter calls the guard the way the contract fixes, that it
cannot cost a turn when anything goes wrong, and that the answers it gives about its own
failures stay distinct from each other and from the guard's. What they do not establish: that a
real Codex host invoked it, honoured its output, or delivered a hold. That is the separate
evidence the hook contract's packet and this repository's status command keep apart.
"""

import argparse
import ast
import errno
import inspect
import json
import os
from pathlib import Path
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion, firing, hooks, reading

import runtime_install

ENTRY_POINT = ROOT / "scripts" / completion.ENTRY_POINT_NAME

# A Stop payload with the nine fields the host was observed to deliver. Used as delivered, and
# never as a template a case may quietly trim: the adapter's contract is that it forwards what
# arrives, so a case that wants a different payload says so.
STOP = {
    "cwd": "/tmp/workspace",
    "hook_event_name": "Stop",
    "last_assistant_message": "I finished the task.",
    "model": "test-model",
    "permission_mode": "default",
    "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688",
    "stop_hook_active": False,
    "transcript_path": "/tmp/transcript.jsonl",
    "turn_id": "turn-1",
}

RELEASED = {"decision": "release", "state": "unmanaged", "observation": "unmanaged",
            "reason": "No marker names this workspace.", "assignmentId": None,
            "counters": {}, "recordedAs": None, "hook_output": {}}

HELD = {"decision": "block", "state": "receipt_missing", "observation": "receipt_missing",
        "reason": "This turn declared itself ready for review and no receipt names it.",
        "assignmentId": "a" * 64, "counters": {"holdsThisTurn": 0},
        "recordedAs": "hook/s/t/0.json",
        "hook_output": {"decision": "block",
                        "reason": "This turn declared itself ready for review and no receipt"
                                  " names it.", "continue": True}}


def fake_relay(directory, *, stdout="", code=0, sleep=0.0, record=None):
    """A stand-in relay that records how it was called and answers as the case requires."""
    path = Path(directory) / "codex-session-relay"
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import json, sys, time\n"
        "time.sleep(" + repr(float(sleep)) + ")\n"
        "payload = sys.stdin.buffer.read().decode('utf-8', 'replace')\n"
        "record = " + repr(str(record) if record else "") + "\n"
        "if record:\n"
        "    open(record, 'w').write(json.dumps({'argv': sys.argv[1:], 'stdin': payload}))\n"
        "sys.stdout.write(" + repr(stdout) + ")\n"
        "raise SystemExit(" + repr(int(code)) + ")\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path


def settings(directory, **overrides):
    document = completion.configuration(
        relay=str(Path(directory) / "codex-session-relay"),
        marker_root=str(Path(directory) / "marker"),
        journal_root=str(Path(directory) / "journal"),
        codex_home=str(directory), issue="CRW-37")
    document.update(overrides)
    path = completion.configuration_path(Path(directory))
    path.write_text(json.dumps(document), encoding="utf-8")
    return document


def journalled(directory):
    root = Path(directory) / "journal"
    return [json.loads(entry.read_text(encoding="utf-8"))
            for day in sorted(root.glob("*")) for entry in sorted(day.glob("*.json"))]


def register(directory, **overrides):
    """Install this adapter's registration the way cmd_hook does, so a case works against a
    registration this repository actually writes rather than one the case invented."""
    args = argparse.Namespace(
        codex_home=str(directory), event=None, hook_command=None, adapter="completion",
        dest=None, relay_command=str(Path(directory) / "codex-session-relay"),
        marker_root=str(Path(directory) / "marker"), db_path=None,
        journal_root=str(Path(directory) / "journal"), python=sys.executable,
        mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-100", apply=True)
    for name, value in overrides.items():
        setattr(args, name, value)
    emitted = []
    with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
        code = runtime_install.cmd_hook(args)
    return code, emitted[0]


def amend_settings(directory, **overrides):
    path = completion.configuration_path(Path(directory))
    document = json.loads(path.read_text(encoding="utf-8"))
    document.update(overrides)
    path.write_text(json.dumps(document), encoding="utf-8")
    return path


def break_the_target(directory):
    """Make the registered adapter name a file that is not there, keeping the file NAME, so the
    registration is still recognised as this adapter's and only its target is gone."""
    path = Path(directory) / "hooks.json"
    path.write_text(path.read_text(encoding="utf-8").replace(
        str(ENTRY_POINT), str(Path(directory) / completion.ENTRY_POINT_NAME)), encoding="utf-8")


def second_registration(directory, journal):
    """A second registration of this adapter naming its own settings file and its own journal.

    Both registrations run on every Stop, which is the host this command used to answer by
    reading neither of them. Returns the two settings paths.
    """
    first = completion.configuration_path(Path(directory))
    second = Path(directory) / "other-settings.json"
    document = json.loads(first.read_text(encoding="utf-8"))
    document["journalRoot"] = str(Path(directory) / journal)
    second.write_text(json.dumps(document), encoding="utf-8")
    path = Path(directory) / "hooks.json"
    hooks_file = json.loads(path.read_text(encoding="utf-8"))
    entry = dict(hooks_file["hooks"][completion.EVENT][0]["hooks"][0])
    entry["command"] = entry["command"].replace(str(first), str(second))
    hooks_file["hooks"][completion.EVENT][0]["hooks"].append(entry)
    path.write_text(json.dumps(hooks_file), encoding="utf-8")
    return first, second


real_read_json = reading.read_json


def why_no_record(directory):
    """The cause cell, read with a default so a command that does not answer this question at
    all fails on the assertion rather than on a missing key."""
    return completion.status(codex_home=directory, environ={}).get("firingRecordAbsence", {})


class TheCallToTheGuard(unittest.TestCase):
    """Criterion 1: the confirmed event and output contract, and the guard's own flags."""

    def test_the_payload_reaches_the_guard_unchanged(self):
        with tempfile.TemporaryDirectory() as temporary:
            seen = Path(temporary) / "seen.json"
            fake_relay(temporary, stdout=json.dumps(RELEASED), record=seen)
            settings(temporary)
            answer = completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                                    environ={})
            call = json.loads(seen.read_text(encoding="utf-8"))
        self.assertIsNone(answer, "a release prints nothing at all")
        self.assertEqual(json.loads(call["stdin"]), STOP,
                         "the guard is handed what the host delivered, not a reconstruction")
        self.assertEqual(call["argv"][0], completion.GUARD_COMMAND)

    def test_the_marker_root_is_always_named_and_the_clock_never_is(self):
        with tempfile.TemporaryDirectory() as temporary:
            seen = Path(temporary) / "seen.json"
            fake_relay(temporary, stdout=json.dumps(RELEASED), record=seen)
            settings(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            call = json.loads(seen.read_text(encoding="utf-8"))
        self.assertIn("--marker-root", call["argv"],
                      "the host process does not carry the coordinator's environment, so a root"
                      " left unnamed would resolve somewhere else and read every workspace as"
                      " unmanaged")
        self.assertNotIn("--now", call["argv"], "the time a decision is made is the guard's")
        self.assertNotIn("--db-path", call["argv"],
                         "an unconfigured database must stay unnamed, or the dbPath the"
                         " coordinator recorded in its own intent becomes unreachable")
        self.assertNotIn("--mode", call["argv"], "observe is the guard's own default")

    def test_a_configured_database_and_hold_mode_are_named(self):
        with tempfile.TemporaryDirectory() as temporary:
            seen = Path(temporary) / "seen.json"
            fake_relay(temporary, stdout=json.dumps(RELEASED), record=seen)
            settings(temporary, dbPath="/tmp/relay.sqlite", mode=completion.HOLD,
                     isolationAssertedBy="the coordinator, for this test")
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            call = json.loads(seen.read_text(encoding="utf-8"))
        self.assertIn("--db-path", call["argv"])
        self.assertEqual(call["argv"][call["argv"].index("--mode") + 1], completion.HOLD)

    def test_a_held_turn_prints_the_block_the_guard_produced(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(HELD))
            settings(temporary)
            answer = completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                                    environ={})
        self.assertEqual(json.loads(answer),
                         {"decision": "block", "reason": HELD["hook_output"]["reason"],
                          "continue": True})

    def test_a_block_carrying_no_prompt_is_not_delivered(self):
        """The host reports a block with no reason as a failed run that continues nothing, so
        delivering one would spend a turn's hold on a prompt the model never sees."""
        for answer in ({"decision": "block", "continue": True},
                       {"decision": "block", "reason": "   ", "continue": True},
                       {"decision": "allow", "reason": "x"}):
            with self.subTest(answer=answer):
                self.assertIsNone(completion.hook_output({"decision": "block",
                                                          "hook_output": answer}))


class OnlyAVerdictThatAgreesWithItselfIsActedOn(unittest.TestCase):
    """Both cells are read. Answering one question from the other's reading is how this adapter
    would deliver a hold nobody decided."""

    def test_a_verdict_that_releases_while_its_answer_holds_is_not_honoured(self):
        verdict = {"decision": "release",
                   "hook_output": {"decision": "block", "reason": "retry", "continue": False}}
        self.assertTrue(completion.verdict_complaints(verdict))
        self.assertIsNone(completion.hook_output(verdict),
                          "the nested half alone must not become a block")
        self.assertEqual(
            completion.outcome_of({"ending": completion.EXITED, "code": completion.GUARD_EXIT_OK},
                                  *completion.read_guard_stdout(json.dumps(verdict))),
            completion.GUARD_VERDICT_INCOMPLETE)

    def test_a_continuation_is_never_manufactured(self):
        verdict = {"decision": "block",
                   "hook_output": {"decision": "block", "reason": "r", "continue": False}}
        self.assertTrue(completion.verdict_complaints(verdict))
        self.assertIsNone(completion.hook_output(verdict))

    def test_a_hold_with_nothing_for_the_host_to_act_on_is_a_disagreement(self):
        self.assertTrue(completion.verdict_complaints({"decision": "block", "hook_output": {}}))

    def test_the_release_the_guard_actually_produces_still_agrees(self):
        self.assertEqual(completion.verdict_complaints(RELEASED), [])
        self.assertEqual(completion.verdict_complaints(HELD), [])
        self.assertIsNotNone(completion.hook_output(HELD))


class EveryRecordedPathIsAbsolute(unittest.TestCase):
    """This hook runs in the session's workspace, not where it was installed from."""

    def test_installation_settles_every_path_it_records(self):
        document = completion.configuration(
            relay="./bin/codex-session-relay", marker_root="./markers",
            database="./relay.sqlite", journal_root="./journal",
            codex_home="/home/x/.codex", environ={})
        for field in ("relayExecutable", "markerRoot", "dbPath", "journalRoot"):
            with self.subTest(field=field):
                self.assertTrue(os.path.isabs(document[field]), document[field])

    def test_a_relative_path_is_refused_rather_than_resolved_in_the_workspace(self):
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            environ={})
        document["relayExecutable"] = "codex-session-relay"
        found = completion.complaints(document)
        self.assertTrue(found)
        self.assertIn("absolute", found[0])

    def test_the_pointer_is_recorded_as_a_pointer_and_not_as_its_target(self):
        with tempfile.TemporaryDirectory() as temporary:
            dest = Path(temporary) / "dest"
            (dest / "runtime-a" / "bin").mkdir(parents=True)
            (dest / "runtime-a" / "bin" / "codex-session-relay").write_text("", encoding="utf-8")
            (dest / "current").symlink_to(dest / "runtime-a")
            document = completion.configuration(destination=str(dest), marker_root="/m",
                                                codex_home="/h", environ={})
        self.assertEqual(document["relayExecutable"],
                         str(dest / "current" / "bin" / "codex-session-relay"),
                         "following the link here would record today's target and leave the"
                         " next update moving a pointer nothing reads")

    def test_the_interpreter_is_settled_at_install_time(self):
        """The hook runs from each session's workspace, so a bare name resolved then could find
        a different interpreter or nothing at all."""
        settled = completion.interpreter_for("python3")
        self.assertTrue(os.path.isabs(str(settled)), settled)
        self.assertTrue(Path(settled).is_file())
        with self.assertRaises(ValueError):
            completion.interpreter_for("definitely-not-an-interpreter-crw37")
        with self.assertRaises(ValueError):
            completion.interpreter_for("")

    def test_a_registered_command_names_an_interpreter_that_can_be_found_from_anywhere(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python="python3",
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                runtime_install.cmd_hook(args)
            document = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
            command = document["hooks"][emitted[0]["event"]][0]["hooks"][0]["command"]
        words = completion.registered_argv(command)
        self.assertTrue(os.path.isabs(words[0]), words)
        self.assertEqual(Path(words[1]).name, completion.ENTRY_POINT_NAME)


class CouldNotLookIsNotNotThere(unittest.TestCase):
    """Path.is_file answers false for both, which sends the repair to the wrong place."""

    def test_the_four_states_are_four_answers(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "a-file").write_text("", encoding="utf-8")
            (home / "a-dir").mkdir()
            self.assertEqual(completion.presence(home / "a-file", "x")["value"], reading.PRESENT)
            self.assertEqual(completion.presence(home / "nothing", "x")["value"], reading.ABSENT)
            self.assertEqual(completion.presence(home / "a-dir", "x")["value"],
                             reading.UNREADABLE, "a directory where a file belongs is neither")
            self.assertEqual(
                completion.presence(home / "a-dir", "x", directory=True)["value"],
                reading.PRESENT)

    def test_a_runtime_that_cannot_be_reached_is_not_reported_as_missing(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            closed = home / "closed"
            closed.mkdir()
            (closed / "codex-session-relay").write_text("", encoding="utf-8")
            settings(temporary, relayExecutable=str(closed / "codex-session-relay"))
            closed.chmod(0o000)
            try:
                if os.access(str(closed / "codex-session-relay"), os.F_OK):
                    self.skipTest("this user can traverse a directory with no permissions")
                found = completion.status(codex_home=temporary, environ={})
            finally:
                closed.chmod(0o700)
        self.assertEqual(found["relayExecutable"]["value"], reading.ACCESS_ERROR,
                         "a runtime behind a permission wall is a different repair from one"
                         " that was never installed")
        self.assertEqual(found["guardEvaluateOffered"]["value"], completion.NOT_READ,
                         "and it is not asked, rather than being reported as not offering")


class TheSettingsVersionIsAnActualBoundary(unittest.TestCase):
    def test_a_document_from_another_version_is_malformed_rather_than_acted_on(self):
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            environ={})
        self.assertEqual(completion.complaints(document), [])
        document["configVersion"] = completion.CONFIG_VERSION + 1
        self.assertTrue(completion.complaints(document))
        del document["configVersion"]
        self.assertTrue(completion.complaints(document))


class TheJournalCountMeansInvocations(unittest.TestCase):
    def test_a_file_this_hook_did_not_write_is_not_counted_as_one(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            root = Path(temporary) / "journal"
            day = sorted(root.glob("*"))[0]
            (day / "notes.txt").write_text("someone else's", encoding="utf-8")
            (day / "readme.json").write_text("{}", encoding="utf-8")
            (root / "scratch").mkdir()
            (root / "scratch" / "x.json").write_text("{}", encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], "1",
                         "the count is labelled invocations this hook recorded, so it counts"
                         " the records this hook writes and nothing else")


class TheBudgetMarginIsShownRatherThanAsserted(unittest.TestCase):
    def test_both_numbers_and_their_margin_are_reported(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            with mock.patch.object(runtime_install, "emit"):
                runtime_install.cmd_hook(args)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["budget"]["value"], "5")
        self.assertEqual(found["budget"]["guardBudgetSeconds"], 5)
        self.assertEqual(found["budget"]["registeredTimeoutSeconds"], [10])

    def test_with_no_registration_the_margin_is_not_read_rather_than_guessed(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["budget"]["value"], completion.NOT_READ)


class NoTurnIsEverCostByThisAdapter(unittest.TestCase):
    """Criterion 4: ordinary turns, and every failure of this adapter, end normally."""

    def _entry_point(self, temporary, payload):
        return subprocess.run(
            [sys.executable, str(ENTRY_POINT)], input=payload, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, timeout=60,
            env={**os.environ, "CODEX_HOME": str(temporary)})

    def test_the_entry_point_exits_zero_and_stays_silent_when_the_relay_rejects_the_call(self):
        """The case that actually occurs: a relay built before the guard existed. Its argument
        parser exits 2 with a usage message, and exit 2 is the host's blocking code."""
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout="", code=2)
            settings(temporary)
            done = self._entry_point(temporary, json.dumps(STOP).encode("utf-8"))
        self.assertEqual(done.returncode, 0, "exit 2 from anything inside must not escape")
        self.assertEqual(done.stdout, b"", "a turn is not held because a runtime is too old")
        self.assertEqual(done.stderr, b"", "stderr is the host's other continuation channel")

    def test_the_entry_point_exits_zero_on_a_payload_that_is_not_json(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            done = self._entry_point(temporary, b"not json at all")
        self.assertEqual(done.returncode, 0)
        self.assertEqual(done.stdout, b"")
        self.assertEqual(done.stderr, b"")

    def test_the_entry_point_exits_zero_with_no_settings_at_all(self):
        with tempfile.TemporaryDirectory() as temporary:
            done = self._entry_point(temporary, json.dumps(STOP).encode("utf-8"))
        self.assertEqual(done.returncode, 0)
        self.assertEqual(done.stdout, b"")

    def test_the_entry_point_parses_no_arguments(self):
        """A hook file can carry a flag this adapter never had. argparse would exit 2 on it,
        and the host would read that as a hold with a usage message for its prompt."""
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            done = subprocess.run(
                [sys.executable, str(ENTRY_POINT), "--some-flag-from-an-older-install"],
                input=json.dumps(STOP).encode("utf-8"), stdout=subprocess.PIPE,
                stderr=subprocess.PIPE, timeout=60,
                env={**os.environ, "CODEX_HOME": str(temporary)})
        self.assertEqual(done.returncode, 0)
        self.assertEqual(done.stderr, b"")

    def test_a_guard_that_never_answers_is_killed_and_the_turn_ends(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(HELD), sleep=10)
            settings(temporary, timeoutSeconds=1)
            started = time.monotonic()
            answer = completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                                    environ={})
            elapsed = time.monotonic() - started
            records = journalled(temporary)
        self.assertIsNone(answer)
        self.assertEqual(records[0]["adapterOutcome"], completion.GUARD_TIMED_OUT)
        self.assertEqual(records[0]["processEnding"], completion.TIMED_OUT)
        self.assertLess(elapsed, 5,
                        "the whole timeout path stays near the budget, because a second full"
                        " wait is the window in which the host kills this process and the"
                        " timeout goes unrecorded")

    def test_no_answer_this_adapter_gives_by_itself_holds_a_turn(self):
        held = [outcome for outcome in completion.OUTCOMES if outcome in completion.ANSWERED]
        self.assertEqual(held, [completion.GUARD_ANSWERED],
                         "only a verdict the guard reached may reach stdout; every other"
                         " outcome is this adapter failing, and a detector that fails must"
                         " not cost the turn it failed on")


class FailuresStayApart(unittest.TestCase):
    """Invariant 1 and criterion 6: a reading that did not happen is never another's answer."""

    def test_a_refused_request_and_a_rejected_call_are_different_answers(self):
        """Both exit 2. One is the relay declining a request it understood; the other is its
        argument parser refusing before any command ran, which is what a runtime without this
        subcommand looks like. They are repaired in different places."""
        refused = completion.outcome_of(
            {"ending": completion.EXITED, "code": completion.GUARD_EXIT_REFUSED},
            *completion.read_guard_stdout(json.dumps({"error": "refused", "reason": "x"})))
        rejected = completion.outcome_of(
            {"ending": completion.EXITED, "code": completion.GUARD_EXIT_REFUSED},
            *completion.read_guard_stdout(""))
        self.assertEqual(refused, completion.GUARD_REFUSED)
        self.assertEqual(rejected, completion.GUARD_REJECTED_THE_CALL)
        self.assertNotEqual(refused, rejected)

    def test_every_way_of_failing_to_ask_has_its_own_answer(self):
        cases = [
            ({"ending": completion.NOT_STARTED}, "", completion.GUARD_UNREACHABLE),
            ({"ending": completion.TIMED_OUT}, "", completion.GUARD_TIMED_OUT),
            ({"ending": completion.SIGNALLED}, "", completion.GUARD_SIGNALLED),
            ({"ending": completion.EXITED, "code": completion.GUARD_EXIT_HOST},
             json.dumps({"error": "host"}), completion.GUARD_HOST_ERROR),
            ({"ending": completion.EXITED, "code": completion.GUARD_EXIT_USAGE},
             json.dumps({"error": "usage"}), completion.GUARD_USAGE_ERROR),
            ({"ending": completion.EXITED, "code": completion.GUARD_EXIT_OK},
             "this is not json", completion.GUARD_OUTPUT_UNREADABLE),
            ({"ending": completion.EXITED, "code": completion.GUARD_EXIT_OK}, "",
             completion.GUARD_SAID_NOTHING),
            ({"ending": completion.EXITED, "code": completion.GUARD_EXIT_OK},
             json.dumps({"decision": "release"}), completion.GUARD_VERDICT_INCOMPLETE),
            ({"ending": completion.EXITED, "code": completion.GUARD_EXIT_OK},
             json.dumps(RELEASED), completion.GUARD_ANSWERED),
        ]
        answers = []
        for ending, said, expected in cases:
            with self.subTest(expected=expected):
                found = completion.outcome_of(ending, *completion.read_guard_stdout(said))
                self.assertEqual(found, expected)
                answers.append(found)
        self.assertEqual(len(set(answers)), len(answers), "no two of these share an answer")

    def test_a_runtime_that_is_not_there_names_why(self):
        with tempfile.TemporaryDirectory() as temporary:
            settings(temporary)  # no relay was ever written
            answer = completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                                    environ={})
            records = journalled(temporary)
        self.assertIsNone(answer)
        self.assertEqual(records[0]["adapterOutcome"], completion.GUARD_UNREACHABLE)
        self.assertEqual(records[0]["errno"], "ENOENT",
                         "a runtime that is gone and one that cannot be executed are"
                         " different repairs")

    def test_settings_give_four_answers_and_not_one(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            path = completion.configuration_path(home)
            _value, absent, _detail, _found = completion.read_configuration(path)
            path.write_text("{ not json", encoding="utf-8")
            _value, unreadable, _detail, _found = completion.read_configuration(path)
            path.write_text(json.dumps({"relayExecutable": "/r", "markerRoot": "/m",
                                        "mode": "whatever"}), encoding="utf-8")
            _value, malformed, detail, _found = completion.read_configuration(path)
        self.assertEqual(absent, completion.CONFIG_ABSENT)
        self.assertEqual(unreadable, completion.CONFIG_UNREADABLE)
        self.assertEqual(malformed, completion.CONFIG_MALFORMED)
        self.assertIn("mode", detail)
        self.assertEqual(len({absent, unreadable, malformed}), 3)

    def test_the_settings_states_come_from_the_module_that_owns_them(self):
        self.assertEqual(set(completion.CONFIG_OUTCOMES), set(reading.UNUSABLE) | {reading.ABSENT})


class WhatIsRecordedAboutThisHookItself(unittest.TestCase):
    """Criterion 6: firing evidence this adapter owns, separate from the guard's records."""

    def test_an_unmanaged_workspace_still_leaves_evidence_that_the_hook_ran(self):
        """The guard records only when it selected an assignment, so on a host with no managed
        session it writes nothing. Without this, an empty firing record and a hook that never
        runs at all would look identical."""
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            records = journalled(temporary)
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0]["adapterOutcome"], completion.GUARD_ANSWERED)
        self.assertEqual(records[0]["guardState"], "unmanaged")
        self.assertIsNone(records[0]["guardRecordedAs"],
                          "the guard recorded nothing, and that is reported rather than filled in")
        self.assertFalse(records[0]["held"])

    def test_a_payload_this_hook_cannot_parse_is_still_recorded(self):
        """The one class of invocation that most needs a record was the one leaving none: the
        settings say where a record goes, so reading them second meant an unparseable payload
        was released into silence."""
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            self.assertIsNone(completion.run(b"not json at all", codex_home=temporary,
                                             environ={}))
            self.assertIsNone(completion.run(json.dumps([1]).encode("utf-8"),
                                             codex_home=temporary, environ={}))
            self.assertIsNone(completion.run(None, codex_home=temporary, environ={}))
            records = journalled(temporary)
        self.assertEqual(sorted(r["adapterOutcome"] for r in records),
                         sorted([completion.STDIN_NOT_JSON, completion.STDIN_NOT_OBJECT,
                                 completion.STDIN_UNREADABLE]))


class ATildeTargetIsNotExpandedByAnybody(unittest.TestCase):
    def test_a_tilde_adapter_path_is_relative_in_effect(self):
        """The settings path this adapter expands before opening it. Its own path is handed to
        the interpreter literally, and nothing expands a tilde on the way."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = sys.executable + " '~/completion_hook.py'"
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredCommandTarget"]["value"],
                         completion.REGISTRATION_RELATIVE_TARGET)

    def test_a_missing_absolute_target_is_reported_beside_a_relative_one(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            gone = completion.command_for(sys.executable, home / "gone" / "completion_hook.py")
            loose = sys.executable + " scripts/completion_hook.py"
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": gone, "timeout": 10}]},
                {"hooks": [{"type": "command", "command": loose, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredCommandTarget"]["value"], reading.ABSENT,
                         "one unjudgeable entry must not hide a broken copy beside it")
        self.assertEqual(found["registeredCommandTarget"]["relativeTargets"],
                         ["scripts/completion_hook.py"])

    def test_one_registration_naming_settings_and_one_not_is_ambiguous(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            named = completion.command_for(sys.executable, str(ENTRY_POINT), home / "a.json")
            silent = completion.command_for(sys.executable, str(ENTRY_POINT))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": named, "timeout": 10}]},
                {"hooks": [{"type": "command", "command": silent, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_AMBIGUOUS,
                         "one path and one silence is two different files")

    def test_the_guards_own_answer_is_carried_verbatim_and_not_re_derived(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(HELD))
            settings(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            record = journalled(temporary)[0]
        self.assertEqual(record["guardState"], HELD["state"])
        self.assertEqual(record["guardDecision"], HELD["decision"])
        self.assertEqual(record["assignmentId"], HELD["assignmentId"])
        self.assertEqual(record["guardRecordedAs"], HELD["recordedAs"])
        self.assertTrue(record["held"])
        self.assertIn("elapsedMs", record)


class RegistrationIsNotFiring(unittest.TestCase):
    """Criterion 6: the two are separate cells, and neither is derived from the other."""

    def test_a_present_runtime_that_cannot_answer_is_its_own_cell(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout="", code=2)  # a relay built before the guard existed
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["relayExecutable"]["value"], reading.PRESENT)
        self.assertEqual(found["guardEvaluateOffered"]["value"], completion.GUARD_REJECTED_THE_CALL)
        self.assertNotEqual(found["relayExecutable"]["value"],
                            found["guardEvaluateOffered"]["value"],
                            "merging these would report a hook that cannot work as working")

    def test_what_was_not_asked_is_never_reported_as_nothing_being_there(self):
        with tempfile.TemporaryDirectory() as temporary:
            found = completion.status(codex_home=temporary, environ={})
        for cell in ("hostTrust", "guardRecords", "daemon", "firingJournal"):
            with self.subTest(cell=cell):
                self.assertEqual(found[cell]["value"], completion.NOT_READ)
                self.assertTrue(found[cell]["evidence"])
        self.assertEqual(found["configuration"]["value"], completion.CONFIG_ABSENT,
                         "with no settings, the journal cell says nobody could tell where this"
                         " hook would record, which is not the same as it having recorded"
                         " nothing")

    def test_a_configured_journal_that_is_not_there_yet_is_an_answer(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], reading.ABSENT,
                         "settings name a journal and nothing has been written into it yet,"
                         " which is a different answer from not knowing where to look")

    def test_the_journal_policy_travels_with_its_count(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], "1")
        self.assertEqual(found["firingJournal"]["journalPolicy"], completion.EVERY_INVOCATION,
                         "a count read without its policy cannot be compared with anything")

    def test_a_registration_is_reported_apart_from_every_firing_question(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(code, 0)
        self.assertEqual(emitted[0]["event"], completion.EVENT,
                         "this adapter lands on Stop unless the caller says otherwise")
        self.assertEqual(found["registration"]["value"], "1")
        self.assertEqual(found["registeredCommandTarget"]["value"], reading.PRESENT)
        self.assertEqual(found["firingJournal"]["value"], reading.ABSENT,
                         "installing a hook is not the same claim as it having run")


class TheInstallerSeam(unittest.TestCase):
    """Criterion 1 and 5: one install path, its own settings, and a refusal to overwrite."""

    def _install(self, home, **overrides):
        args = argparse.Namespace(
            codex_home=str(home), event=None, hook_command=None, adapter="completion",
            dest=None, relay_command=str(Path(home) / "codex-session-relay"),
            marker_root=str(Path(home) / "marker"), db_path=None,
            journal_root=str(Path(home) / "journal"), python=sys.executable,
            mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
        for name, value in overrides.items():
            setattr(args, name, value)
        emitted = []
        with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
            code = runtime_install.cmd_hook(args)
        return code, emitted[0]

    def test_the_settings_are_written_before_the_hook_that_reads_them(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            code, payload = self._install(home)
            document = json.loads(completion.configuration_path(home).read_text(encoding="utf-8"))
        self.assertEqual(code, 0)
        self.assertEqual(payload["settings"]["outcome"], completion.CONFIG_CREATED)
        self.assertEqual(payload["result"]["outcome"], hooks.CREATED)
        self.assertEqual(document["mode"], completion.OBSERVE,
                         "holding depends on isolation this command cannot grant, so observe is"
                         " what an install writes unless it is told otherwise")

    def test_settings_that_say_something_else_are_not_overwritten_and_no_hook_is_added(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            self._install(home)
            code, payload = self._install(home, db_path=str(home / "relay.sqlite"))
            document = json.loads(completion.configuration_path(home).read_text(encoding="utf-8"))
            registered = hooks.inventory(hooks.read(home / "hooks.json").value, completion.EVENT)
        self.assertEqual(code, 1)
        self.assertEqual(payload["settings"]["outcome"], completion.CONFIG_DIFFERS)
        self.assertIn("dbPath", payload["settings"]["differingFields"])
        self.assertIsNone(payload["result"], "no hook is appended when its settings were refused")
        self.assertIsNone(document["dbPath"], "the settings were not changed silently")
        self.assertEqual(len(registered), 1, "and no second registration was added")


class HoldingNeedsTheGrantItDependsOn(unittest.TestCase):
    """The contract makes per-session write isolation a prerequisite for holding. An installer
    that sets the mode without it turns an unstated premise into an enforcement decision."""

    def test_hold_without_a_stated_grant_is_malformed(self):
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            mode=completion.HOLD, environ={})
        found = completion.complaints(document)
        self.assertTrue(found)
        self.assertIn("isolationAssertedBy", found[0])

    def test_hold_with_a_stated_grant_records_who_stated_it(self):
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            mode=completion.HOLD, isolation="CRW-37 operator",
                                            environ={})
        self.assertEqual(completion.complaints(document), [])
        self.assertEqual(document["isolationAssertedBy"], "CRW-37 operator")

    def test_observing_needs_nothing_which_is_why_it_is_the_default(self):
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            environ={})
        self.assertEqual(document["mode"], completion.OBSERVE)
        self.assertEqual(completion.complaints(document), [])


class TheMarkerRootFollowsTheRelay(unittest.TestCase):
    """A default that skips the relay's own override is not a default, it is a disagreement."""

    def test_the_environment_override_the_relay_reads_is_read_here_too(self):
        found = completion.default_marker_root({completion.MARKER_ENV: "/somewhere/else"})
        self.assertEqual(str(found), "/somewhere/else",
                         "the coordinator publishes intents under the tree it named, and a hook"
                         " looking elsewhere reads every managed turn as unmanaged")

    def test_the_override_wins_over_xdg_and_home(self):
        found = completion.default_marker_root({completion.MARKER_ENV: "/named",
                                                "XDG_STATE_HOME": "/xdg"})
        self.assertEqual(str(found), "/named")
        self.assertEqual(str(completion.default_marker_root({"XDG_STATE_HOME": "/xdg"})),
                         "/xdg/" + completion.MARKER_DIRECTORY_NAME)

    def test_an_install_under_the_override_records_that_root(self):
        document = completion.configuration(relay="/r", codex_home="/h",
                                            environ={completion.MARKER_ENV: "/named"})
        self.assertEqual(document["markerRoot"], "/named")


class AnUnusableInterpreterIsRefused(unittest.TestCase):
    def test_a_file_without_execute_permission_is_not_registered(self):
        with tempfile.TemporaryDirectory() as temporary:
            candidate = Path(temporary) / "python-ish"
            candidate.write_text("", encoding="utf-8")
            candidate.chmod(0o600)
            with self.assertRaises(ValueError) as raised:
                completion.interpreter_for(str(candidate))
        self.assertIn("executable", str(raised.exception))

    def test_a_missing_interpreter_stops_the_install(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"),
                python=str(home / "no-such-interpreter"),
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            self.assertFalse((home / "hooks.json").exists())
        self.assertEqual(code, 2)


class OneAdapterIsRegisteredOnce(unittest.TestCase):
    """Installation appends and never removes, so a changed timeout would leave two copies
    running on every Stop rather than replacing one."""

    def test_a_second_registration_that_differs_is_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)

            def install(**extra):
                args = argparse.Namespace(
                    codex_home=str(home), event=None, hook_command=None, adapter="completion",
                    dest=None, relay_command=str(home / "codex-session-relay"),
                    marker_root=str(home / "marker"), db_path=None,
                    journal_root=str(home / "journal"), python=sys.executable,
                    mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                    apply=True, isolation_asserted_by=None)
                for name, value in extra.items():
                    setattr(args, name, value)
                emitted = []
                with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                    return runtime_install.cmd_hook(args), emitted[0]

            install()
            code, payload = install(timeout=8)
            document = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
        self.assertEqual(code, 1)
        self.assertIsNone(payload["result"])
        self.assertIn("already registered", payload["error"])
        self.assertEqual(len(document["hooks"][completion.EVENT]), 1,
                         "a second copy would run on every Stop beside the first")

    def test_installing_the_identical_registration_again_is_not_a_duplicate(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                runtime_install.cmd_hook(args)
                code = runtime_install.cmd_hook(args)
        self.assertEqual(code, 0)
        self.assertEqual(emitted[1]["result"]["outcome"], hooks.LINKED)

    def test_a_refused_duplicate_writes_no_settings_for_the_existing_hook_to_pick_up(self):
        """The expensive shape: settings written, duplicate refused afterwards, and the hook
        already in the file immediately running against settings this command said it would
        not install."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command",
                            "command": "/usr/bin/python3 /elsewhere/completion_hook.py",
                            "timeout": 10}]}]}}), encoding="utf-8")
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.HOLD, guard_timeout=5, timeout=10, issue="CRW-37", apply=True,
                isolation_asserted_by="a test")
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            self.assertFalse(completion.configuration_path(home).exists(),
                             "the registration already in this file must not be handed settings"
                             " by an install that refused")
        self.assertEqual(code, 1)
        self.assertIsNone(emitted[0]["settings"])

    def test_a_second_copy_already_there_is_refused_even_when_one_of_them_matches(self):
        command = completion.command_for("/usr/bin/python3", "/a/completion_hook.py")
        document = {"hooks": {completion.EVENT: [
            {"hooks": [{"type": "command", "command": command, "timeout": 10}]},
            {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}
        found = completion.duplicate_complaints(document, completion.EVENT, command, 10)
        self.assertTrue(found, "an identical entry among two does not make two acceptable")
        self.assertIn("more than once", found[0])

    def test_an_identical_command_under_a_matcher_is_still_a_duplicate(self):
        """Installation only ever appends an unconditional group, and hooks.install treats only
        that group as already installed, so an identical command under a matcher would be
        appended beside it and both would run on a matching Stop."""
        command = completion.command_for("/usr/bin/python3", "/a/completion_hook.py")
        document = {"hooks": {completion.EVENT: [
            {"matcher": "something", "hooks": [{"type": "command", "command": command,
                                                "timeout": 10}]}]}}
        found = completion.duplicate_complaints(document, completion.EVENT, command, 10)
        self.assertTrue(found)
        self.assertIn("matcher", found[0])
        unconditional = {"hooks": {completion.EVENT: [
            {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}
        self.assertEqual(
            completion.duplicate_complaints(unconditional, completion.EVENT, command, 10), [])

    def test_an_append_that_cannot_be_found_afterwards_is_refused(self):
        """A writer that does not take this lock can replace the file after the append, and
        then the command would claim an installation nobody can find."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            real = runtime_install.hooks.read
            calls = []

            def wiped(path):
                calls.append(path)
                if len(calls) > 1:  # the read-back, after somebody else replaced the file
                    return reading.Reading(value={"hooks": {}}, state=reading.PRESENT,
                                           source=path)
                return real(path)

            emitted = []
            with mock.patch.object(runtime_install.hooks, "read", side_effect=wiped), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
        self.assertEqual(code, 1)
        self.assertIn("registered 0 times", emitted[0]["error"])

    def test_a_read_back_that_could_not_happen_is_refused_too(self):
        """The promise is exactly one registration, and a read that did not happen cannot
        establish it."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            real = runtime_install.hooks.read
            calls = []

            def flaky(path):
                calls.append(path)
                if len(calls) > 2:  # the final read-back, after somebody chmodded the file
                    return reading.Reading(state=reading.ACCESS_ERROR, source=path,
                                           exception="PermissionError", at="hooks.py:1",
                                           detail="the hook file could not be reached")
                return real(path)

            emitted = []
            with mock.patch.object(runtime_install.hooks, "read", side_effect=flaky), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
        self.assertEqual(code, 1)
        self.assertIsNone(emitted[0]["registrations"])
        self.assertEqual(emitted[0]["reading"]["state"], reading.ACCESS_ERROR)
        self.assertIn("could not be established", emitted[0]["error"])

    def test_a_registration_that_is_not_the_one_this_run_made_is_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            real = runtime_install.hooks.read
            calls = []
            impostor = completion.command_for("/usr/bin/python3", "/elsewhere/completion_hook.py")

            def swapped(path):
                calls.append(path)
                if len(calls) > 2:  # somebody replaced the file between the append and this read
                    return reading.Reading(value={"hooks": {completion.EVENT: [
                        {"hooks": [{"type": "command", "command": impostor, "timeout": 10}]}]}},
                        state=reading.PRESENT, source=path)
                return real(path)

            emitted = []
            with mock.patch.object(runtime_install.hooks, "read", side_effect=swapped), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
        self.assertEqual(code, 1)
        self.assertIn("not the one this run made", emitted[0]["error"])


class AJournalRecordIsWholeOrAbsent(unittest.TestCase):
    """A truncated record is worse than none: it survives under a name nothing will reuse and
    is counted as an invocation whose contents no longer read back."""

    def test_a_write_that_cannot_finish_leaves_nothing_behind(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            real_write = os.write

            def truncating(handle, data):
                real_write(handle, data[:5])
                raise OSError(errno.ENOSPC, "no space left on device")

            with mock.patch.object(completion.os, "write", side_effect=truncating):
                completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                               environ={})
            self.assertEqual(journalled(temporary), [],
                             "the partial record was removed rather than left to be counted")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], "0")

    def test_a_short_write_is_finished_rather_than_accepted(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            real_write = os.write

            def one_byte_at_a_time(handle, data):
                return real_write(handle, data[:1])

            with mock.patch.object(completion.os, "write", side_effect=one_byte_at_a_time):
                completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                               environ={})
            records = journalled(temporary)
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0]["adapterOutcome"], completion.GUARD_ANSWERED)

    def test_a_budget_that_is_not_a_finite_number_is_refused(self):
        """1e999 parses as a valid JSON number and arrives as inf, which means no timeout."""
        self.assertTrue(completion.budget_complaints(float("inf"), 10))
        self.assertTrue(completion.budget_complaints(float("nan"), 10))
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            environ={})
        document["timeoutSeconds"] = json.loads("1e999")
        found = completion.complaints(document)
        self.assertTrue(found)
        self.assertIn("timeoutSeconds", found[0])

    def test_an_integer_beyond_float_range_is_refused_rather_than_raising(self):
        """A guard against a bad value must not itself be one: converting an arbitrary-precision
        integer to a float raises, and the read then fails where it should have answered."""
        enormous = 10 ** 400
        self.assertTrue(completion.budget_complaints(enormous, 10))
        document = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                            environ={})
        document["timeoutSeconds"] = enormous
        found = completion.complaints(document)
        self.assertTrue(found)
        self.assertIn("timeoutSeconds", found[0])

    def test_the_seconds_test_accepts_what_it_should_and_nothing_else(self):
        for good in (1, 5, 0.5, completion.MAX_TIMEOUT_SECONDS):
            with self.subTest(good=good):
                self.assertTrue(completion.usable_seconds(good))
        for bad in (0, -1, True, False, "5", None, float("inf"), float("nan"), 10 ** 400,
                    completion.MAX_TIMEOUT_SECONDS + 1):
            with self.subTest(bad=bad):
                self.assertFalse(completion.usable_seconds(bad))

    def test_a_replacement_under_another_matcher_is_not_this_runs_work(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            expected = completion.command_for(
                sys.executable, runtime_install.ROOT / "scripts" / completion.ENTRY_POINT_NAME,
                completion.configuration_path(home))
            real = runtime_install.hooks.read
            calls = []

            def rematched(path):
                calls.append(path)
                if len(calls) > 2:
                    return reading.Reading(value={"hooks": {completion.EVENT: [
                        {"matcher": "somebody else's", "hooks": [
                            {"type": "command", "command": expected, "timeout": 10}]}]}},
                        state=reading.PRESENT, source=path)
                return real(path)

            emitted = []
            with mock.patch.object(runtime_install.hooks, "read", side_effect=rematched), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
        self.assertEqual(code, 1)
        self.assertIn("not the one this run made", emitted[0]["error"])
        self.assertIn("matcher", emitted[0]["error"])

    def test_the_process_group_is_the_one_that_was_started(self):
        """Looking the group up at kill time asks a process that may already be gone, and a
        recycled pid would name somebody else's group."""
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(HELD), sleep=10)
            settings(temporary, timeoutSeconds=1)
            killed = []

            def watching(group, signal_number):
                killed.append((group, signal_number))

            with mock.patch.object(completion.os, "killpg", side_effect=watching), \
                 mock.patch.object(completion.os, "getpgid",
                                   side_effect=AssertionError("looked the group up late")):
                completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                               environ={})
            records = journalled(temporary)
        self.assertEqual(len(killed), 1)
        self.assertEqual(killed[0][1], 9)
        self.assertEqual(records[0]["adapterOutcome"], completion.GUARD_TIMED_OUT)


class ASpellingThisCommandCannotJudgeIsSaidSo(unittest.TestCase):
    """which() resolves a spelling carrying a separator against the caller's own directory, so
    probing it answers about a program under whatever checkout the diagnosis ran from."""

    def test_a_relative_interpreter_path_is_reported_not_probed(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = "venv/bin/python " + shlex.quote(str(ENTRY_POINT))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredInterpreter"]["value"], completion.WORKSPACE_DEPENDENT)
        self.assertIn("every workspace", found["registeredInterpreter"]["evidence"])

    def test_a_bare_name_keeps_its_path_lookup(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = "python3 " + shlex.quote(str(ENTRY_POINT))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredInterpreter"]["value"], reading.PRESENT,
                         "a bare name is what the host looks up on PATH too")

    def test_relative_settings_spellings_count_toward_ambiguity(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            one = completion.command_for(sys.executable, str(ENTRY_POINT)) + " a.json"
            two = completion.command_for(sys.executable, str(ENTRY_POINT)) + " b.json"
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": one, "timeout": 10}]},
                {"hooks": [{"type": "command", "command": two, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_AMBIGUOUS,
                         "two relative spellings are two unresolved sources, not one")

    def test_one_relative_beside_one_absolute_is_ambiguous(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            one = completion.command_for(sys.executable, str(ENTRY_POINT), home / "a.json")
            two = completion.command_for(sys.executable, str(ENTRY_POINT)) + " b.json"
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": one, "timeout": 10}]},
                {"hooks": [{"type": "command", "command": two, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_AMBIGUOUS)

    def test_a_single_relative_spelling_is_still_its_own_answer(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            only = completion.command_for(sys.executable, str(ENTRY_POINT)) + " a.json"
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": only, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_RELATIVE)

    def test_a_registration_naming_relative_settings_is_reported_not_guessed_at(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "crw-hook.json").write_text("{}", encoding="utf-8")
            command = completion.command_for(sys.executable, str(ENTRY_POINT)) + " crw-hook.json"
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_RELATIVE)
        self.assertIn("each session's workspace", found["configuration"]["evidence"])
        self.assertEqual(found["relayExecutable"]["value"], completion.NOT_READ,
                         "nothing downstream is read from settings that could not be located")

    def test_a_tilde_settings_path_is_absolute_once_the_hook_opens_it(self):
        """Calling it relative here would hide a working configuration and every cell below it."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            document = completion.configuration(
                relay=str(home / "codex-session-relay"), marker_root=str(home / "marker"),
                journal_root=str(home / "journal"), codex_home=str(home), issue="CRW-37")
            named = Path.home() / ".crw37-tilde-settings-test.json"
            named.write_text(json.dumps(document), encoding="utf-8")
            try:
                command = (completion.command_for(sys.executable, str(ENTRY_POINT))
                           + " '~/.crw37-tilde-settings-test.json'")
                (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                    {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                    encoding="utf-8")
                found = completion.status(codex_home=temporary, environ={})
            finally:
                named.unlink()
        self.assertEqual(found["configuration"]["value"], reading.PRESENT)
        self.assertEqual(found["relayExecutable"]["value"], reading.PRESENT,
                         "and the cells below it were read rather than skipped")

    def test_status_probes_the_interpreter_the_registration_names(self):
        """A virtual environment that moved leaves the script in place and the interpreter gone:
        the host cannot start the adapter at all, and the registration still looks correct."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = completion.command_for(str(home / "vanished-python"), str(ENTRY_POINT))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredCommandTarget"]["value"], reading.PRESENT,
                         "the script is there")
        self.assertEqual(found["registeredInterpreter"]["value"], reading.ABSENT,
                         "and the program that has to run it is not")

    def test_a_working_registration_reports_both(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = completion.command_for(sys.executable, str(ENTRY_POINT))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredCommandTarget"]["value"], reading.PRESENT)
        self.assertEqual(found["registeredInterpreter"]["value"], reading.PRESENT)

    def test_a_bare_interpreter_name_is_resolved_the_way_the_host_resolves_it(self):
        """Reporting a PATH name absent because no file sits at that spelling would fail a
        working hook in diagnosis."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = "python3 " + shlex.quote(str(ENTRY_POINT))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredInterpreter"]["value"], reading.PRESENT)
        self.assertIn("wrapper", found["registeredInterpreter"]["evidence"],
                      "and it says what it did not follow")


class OfferingIsNotExitingZero(unittest.TestCase):
    def test_a_program_that_ignores_its_arguments_is_not_offering_the_subcommand(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout="", code=0)  # succeeds, says nothing
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["relayExecutable"]["value"], reading.PRESENT)
        self.assertEqual(found["guardEvaluateOffered"]["value"],
                         completion.GUARD_REJECTED_THE_CALL,
                         "exit 0 alone would report a subcommand it has never heard of")

    def test_a_runtime_that_describes_the_subcommand_is_offering_it(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout="usage: guard-evaluate [-h] [--marker-root ROOT]",
                       code=0)
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["guardEvaluateOffered"]["value"], completion.GUARD_COMMAND)

    def test_a_program_that_echoes_its_arguments_is_not_offering_it(self):
        """/bin/echo prints the subcommand's own name back while offering nothing."""
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=completion.GUARD_COMMAND + " --help", code=0)
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["guardEvaluateOffered"]["value"],
                         completion.GUARD_REJECTED_THE_CALL)


class ARelativeAdapterTargetAnswersForNoFile(unittest.TestCase):
    def test_a_relative_script_path_is_reported_rather_than_resolved(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            command = "python3 scripts/completion_hook.py " + shlex.quote(str(home / "c.json"))
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": command, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredCommandTarget"]["value"],
                         completion.REGISTRATION_RELATIVE_TARGET)
        self.assertIn("each session's workspace", found["registeredCommandTarget"]["evidence"])


class AmbiguousRegistrationsAnswerForNobody(unittest.TestCase):
    def test_two_registrations_naming_different_settings_are_reported_as_ambiguous(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            one = completion.command_for(sys.executable, str(ENTRY_POINT), home / "a.json")
            two = completion.command_for(sys.executable, str(ENTRY_POINT), home / "b.json")
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": one, "timeout": 10}]},
                {"hooks": [{"type": "command", "command": two, "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_AMBIGUOUS)
        self.assertIn("a.json", found["configuration"]["evidence"])
        self.assertIn("b.json", found["configuration"]["evidence"])
        self.assertEqual(found["relayExecutable"]["value"], completion.NOT_READ,
                         "every one of them runs, so none of them answers for the others")


class AnInterpreterHasToBeOne(unittest.TestCase):
    def test_an_executable_that_is_not_python_is_refused(self):
        """/bin/true is executable and exits 0. Registered, every Stop would succeed at running
        it and never reach the adapter: no guard decision, no journal entry, install reported
        as success."""
        if not Path("/bin/true").is_file():
            self.skipTest("this host has no /bin/true")
        with self.assertRaises(ValueError) as raised:
            completion.interpreter_for("/bin/true")
        self.assertIn("Python", str(raised.exception))

    def test_a_real_interpreter_passes(self):
        self.assertTrue(Path(completion.interpreter_for(sys.executable)).is_file())


class TheConfigOverrideIsSettledToo(unittest.TestCase):
    def test_a_relative_override_names_one_file_rather_than_one_per_workspace(self):
        found = completion.configuration_path(None, {completion.CONFIG_ENV: "crw-hook.json"})
        self.assertTrue(os.path.isabs(str(found)), found)
        self.assertEqual(Path(found).name, "crw-hook.json")

    def test_the_install_decides_the_file_and_the_hook_does_not_decide_it_again(self):
        """Resolving twice means resolving in two directories and under two values of
        CODEX_HOME, so the install carries the path it wrote into the registered command."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                runtime_install.cmd_hook(args)
            document = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
            command = document["hooks"][completion.EVENT][0]["hooks"][0]["command"]
            words = completion.registered_argv(command)
            self.assertEqual(len(words), 3, words)
            self.assertEqual(words[2], str(completion.configuration_path(home)))
            self.assertTrue(Path(words[2]).is_file(), "and that file is the one written")

    def test_the_carried_path_wins_over_the_environment_and_the_codex_home(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            named = home / "named-settings.json"
            self.assertEqual(
                completion.configuration_path("/some/other/home",
                                              {completion.CONFIG_ENV: "/an/override.json"},
                                              str(named)),
                named)

    def test_status_reads_the_file_the_registered_hook_reads(self):
        """An install that used an override embedded the resolved path in its command, and
        hook-status has no reason to be running under the same environment."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            named = home / "named-settings.json"
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37",
                apply=True, isolation_asserted_by=None)
            with mock.patch.object(runtime_install, "emit"), \
                 mock.patch.dict(os.environ, {completion.CONFIG_ENV: str(named)}):
                runtime_install.cmd_hook(args)
            self.assertTrue(named.is_file(), "the install wrote the override's file")
            # Deliberately without the override: a later reader has no reason to carry it.
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["configuration"], str(named))
        self.assertEqual(found["configuration"]["configurationSource"], "the registered command")
        self.assertEqual(found["configuration"]["value"], reading.PRESENT,
                         "reporting config_absent here would leave the relay, marker and"
                         " firing cells unread about a hook that is working")
        self.assertEqual(found["relayExecutable"]["value"], reading.PRESENT)

    def test_the_entry_point_uses_the_path_it_was_given(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            elsewhere = home / "elsewhere"
            elsewhere.mkdir()
            fake_relay(temporary, stdout=json.dumps(HELD))
            document = completion.configuration(
                relay=str(home / "codex-session-relay"), marker_root=str(home / "marker"),
                journal_root=str(home / "journal"), codex_home=str(home), issue="CRW-37")
            named = elsewhere / "named.json"
            named.write_text(json.dumps(document), encoding="utf-8")
            done = subprocess.run(
                [sys.executable, str(ENTRY_POINT), str(named)],
                input=json.dumps(STOP).encode("utf-8"), stdout=subprocess.PIPE,
                stderr=subprocess.PIPE, timeout=60, cwd=str(elsewhere),
                env={k: v for k, v in os.environ.items() if k != "CODEX_HOME"})
        self.assertEqual(done.returncode, 0)
        self.assertEqual(json.loads(done.stdout)["decision"], "block",
                         "with no CODEX_HOME and a different working directory, the settings"
                         " the install named are still the ones read")


class AnUnknownDecisionIsNotARelease(unittest.TestCase):
    def test_a_verdict_deciding_something_else_entirely_is_incomplete(self):
        self.assertTrue(completion.verdict_complaints({"decision": "banana",
                                                       "hook_output": {}}))
        self.assertEqual(
            completion.outcome_of({"ending": completion.EXITED, "code": completion.GUARD_EXIT_OK},
                                  *completion.read_guard_stdout(
                                      json.dumps({"decision": "banana", "hook_output": {}}))),
            completion.GUARD_VERDICT_INCOMPLETE,
            "recording an incompatible runtime as having answered is the one reading that"
            " hides the incompatibility")
        self.assertEqual(completion.verdict_complaints({"decision": completion.RELEASE,
                                                        "hook_output": {}}), [])


class TheInstallerSeamContinued(unittest.TestCase):
    """The rest of the installer seam. Same _install helper, kept beside its cases."""

    def _install(self, home, **overrides):
        args = argparse.Namespace(
            codex_home=str(home), event=None, hook_command=None, adapter="completion",
            dest=None, relay_command=str(Path(home) / "codex-session-relay"),
            marker_root=str(Path(home) / "marker"), db_path=None,
            journal_root=str(Path(home) / "journal"), python=sys.executable,
            mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True,
            isolation_asserted_by=None)
        for name, value in overrides.items():
            setattr(args, name, value)
        emitted = []
        with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
            code = runtime_install.cmd_hook(args)
        return code, emitted[0]

    def test_installing_the_same_thing_twice_settles_without_a_second_hook(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            self._install(home)
            code, payload = self._install(home)
        self.assertEqual(code, 0)
        self.assertEqual(payload["settings"]["outcome"], completion.CONFIG_UNCHANGED)
        self.assertEqual(payload["result"]["outcome"], hooks.LINKED)

    def test_naming_no_runtime_at_all_is_refused_rather_than_resolved_from_the_path(self):
        with tempfile.TemporaryDirectory() as temporary:
            code, payload = self._install(Path(temporary), relay_command=None, dest=None)
        self.assertEqual(code, 2)
        self.assertIn("PATH", payload["error"])
        self.assertFalse((Path(temporary) / "hooks.json").exists())

    def test_the_runtime_is_named_through_the_installers_own_pointer(self):
        document = completion.configuration(destination="/opt/dest", marker_root="/m",
                                            codex_home="/home", environ={})
        self.assertEqual(document["relayExecutable"],
                         "/opt/dest/current/bin/codex-session-relay",
                         "an update moves the pointer, and these settings keep naming the"
                         " runtime that is actually selected")

    def test_an_explicit_command_still_installs_without_knowing_about_adapters(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(codex_home=str(home), event="SessionStart",
                                      hook_command="/bin/true", timeout=5, issue="JUN-104",
                                      apply=True)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
        self.assertEqual(code, 0)
        self.assertEqual(emitted[0]["result"]["outcome"], hooks.CREATED)
        self.assertIsNone(emitted[0]["settings"], "no adapter settings were involved")


class TheRegisteredCommandIsArgvAndNotText(unittest.TestCase):
    """A hook file carries a command line. Writing it and reading it back are inverses.

    Both halves were wrong in the same way and it took two shapes: joining raw made a path with
    a space into two words and a path with shell syntax into syntax, and matching by substring
    made a neighbouring program's name into this adapter's registration.
    """

    def test_a_path_with_a_space_survives_the_round_trip(self):
        command = completion.command_for("/opt/my python/bin/python3",
                                         "/checkout/scripts/completion_hook.py")
        self.assertEqual(completion.registered_argv(command),
                         ["/opt/my python/bin/python3", "/checkout/scripts/completion_hook.py"],
                         "the host is handed the two words this names, not four")

    def test_shell_syntax_in_an_interpreter_path_is_not_delivered_as_syntax(self):
        """This command line runs on every Stop with the Codex user's own privileges."""
        hostile = "/bin/python3; touch /tmp/crw37-should-not-exist"
        command = completion.command_for(hostile, "/checkout/scripts/completion_hook.py")
        self.assertEqual(completion.registered_argv(command),
                         [hostile, "/checkout/scripts/completion_hook.py"],
                         "the semicolon is part of one word, not a second command")
        self.assertTrue(command.startswith("'"),
                        "a word carrying shell syntax is delivered quoted")

    def test_ordinary_paths_come_back_unchanged(self):
        self.assertEqual(completion.command_for("/usr/bin/python3", "/a/completion_hook.py"),
                         "/usr/bin/python3 /a/completion_hook.py")

    def test_a_neighbouring_program_is_not_this_adapter(self):
        self.assertIsNone(completion.names_this_adapter("/opt/not-completion_hook.py"))
        self.assertIsNone(completion.names_this_adapter("/opt/not_completion_hook.py"))
        self.assertEqual(completion.names_this_adapter("/usr/bin/python3 /a/completion_hook.py"),
                         "/a/completion_hook.py")

    def test_a_command_that_is_not_a_command_line_names_nothing(self):
        self.assertIsNone(completion.registered_argv("unbalanced 'quote"))
        self.assertIsNone(completion.names_this_adapter("unbalanced 'quote"))

    def test_status_does_not_claim_an_unrelated_hook_as_this_adapter(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            impostor = home / "not-completion_hook.py"
            impostor.write_text("", encoding="utf-8")
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": str(impostor), "timeout": 10}]}]}}),
                encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registration"]["thisAdapter"], [],
                         "a program whose name merely contains this one is a different program")
        self.assertEqual(found["registeredCommandTarget"]["value"], completion.NOT_READ,
                         "and no target of somebody else's is checked as if it were ours")

    def test_what_installation_writes_is_what_the_status_reader_identifies(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            with mock.patch.object(runtime_install, "emit"):
                runtime_install.cmd_hook(args)
            found = completion.status(codex_home=temporary, environ={})
        target = found["registration"]["thisAdapter"][0]["target"]
        self.assertEqual(Path(target).name, completion.ENTRY_POINT_NAME)
        self.assertTrue(Path(target).is_file(), "the writer and the reader agree on the path")


class TheWriterSatisfiesItsOwnReader(unittest.TestCase):
    """Settings this command can generate but its own reader rejects are refused, not written.

    Otherwise an install reports success and every Stop afterwards reads the settings it just
    wrote as malformed: a hook that is registered, inert, and says so nowhere anybody looks.
    """

    def test_settings_the_reader_would_reject_are_never_written(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / completion.CONFIG_NAME
            answer = completion.write_configuration(
                path, {"relayExecutable": "/r", "markerRoot": "/m", "mode": completion.OBSERVE,
                       "timeoutSeconds": 0}, apply=True)
        self.assertEqual(answer["outcome"], completion.CONFIG_WOULD_NOT_BE_READABLE)
        self.assertFalse(answer["wrote"])
        self.assertFalse(path.exists())
        self.assertNotIn(completion.CONFIG_WOULD_NOT_BE_READABLE, completion.CONFIG_SETTLED)

    def test_an_existing_file_that_is_not_an_object_refuses_rather_than_raising(self):
        """A modelled refusal was promised for pre-existing settings; asking a list for its
        fields is a traceback instead."""
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / completion.CONFIG_NAME
            path.write_text(json.dumps(["x"]), encoding="utf-8")
            wanted = completion.configuration(relay="/r", marker_root="/m", codex_home="/h",
                                              environ={})
            answer = completion.write_configuration(path, wanted, apply=True)
        self.assertEqual(answer["outcome"], completion.CONFIG_DIFFERS)
        self.assertIsNone(answer["differingFields"])
        self.assertIn("list", answer["detail"])
        self.assertFalse(answer["wrote"])

    def test_a_write_that_could_not_be_read_back_does_not_settle(self):
        """The write landed; what is in the file now was not confirmed to be it. Registering a
        hook against it would be the same hole the write-before-register order closes, one step
        later."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            wanted = completion.configuration(relay=str(home / "relay"),
                                              marker_root=str(home / "marker"),
                                              codex_home=str(home), environ={})
            real = reading.read_json
            calls = []

            def flaky(path, what, **kwargs):
                calls.append(path)
                if len(calls) >= 3:
                    return reading.Reading(state=reading.UNREADABLE, source=path,
                                           exception="TypeError", at="completion.py:1",
                                           detail="the readback could not be read")
                return real(path, what, **kwargs)

            with mock.patch.object(completion.reading, "read_json", side_effect=flaky):
                answer = completion.write_configuration(
                    completion.configuration_path(home), wanted, apply=True)
            self.assertTrue(completion.configuration_path(home).exists(),
                            "the write really did land, which is why it is reported as applied")
        self.assertEqual(answer["outcome"], completion.CONFIG_APPLIED_UNVERIFIED)
        self.assertTrue(answer["applied"])
        self.assertTrue(answer["wrote"])
        self.assertFalse(answer["readBack"])
        self.assertNotIn(completion.CONFIG_APPLIED_UNVERIFIED, completion.CONFIG_SETTLED)

    def test_an_unverified_write_stops_the_hook_from_being_registered(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            emitted = []
            with mock.patch.object(completion, "write_configuration",
                                   return_value={"outcome": completion.CONFIG_APPLIED_UNVERIFIED,
                                                 "applied": True, "wrote": True,
                                                 "readBack": False}), \
                 mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            self.assertFalse((home / "hooks.json").exists())
        self.assertEqual(code, 1)
        self.assertIsNone(emitted[0]["result"])

    def test_a_non_positive_budget_is_refused_before_anything_is_installed(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=0, timeout=10, issue="CRW-37", apply=True)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            self.assertFalse((home / "hooks.json").exists())
            self.assertFalse(completion.configuration_path(home).exists())
        self.assertEqual(code, 2)
        self.assertIn("positive", emitted[0]["error"])

    def test_a_budget_the_host_timeout_does_not_exceed_is_refused(self):
        """The host can kill the adapter mid-call, and the record that would have explained the
        timeout is the one the killed process was about to write."""
        self.assertEqual(completion.budget_complaints(5, 10), [])
        self.assertTrue(completion.budget_complaints(10, 10))
        self.assertTrue(completion.budget_complaints(20, 10))
        self.assertTrue(completion.budget_complaints(-1, 10))
        self.assertTrue(completion.budget_complaints(True, 10))
        self.assertTrue(completion.budget_complaints("5", 10))

    def test_a_registered_timeout_the_host_would_clamp_is_refused(self):
        """The host clamps an over-long timeout at discovery, so a large number is not the
        deadline it looks like, and the clamped value is not measured here."""
        self.assertEqual(completion.budget_complaints(5, completion.REGISTERED_TIMEOUT_SECONDS),
                         [])
        found = completion.budget_complaints(5, 100000)
        self.assertTrue(found)
        self.assertIn("clamps", found[0])
        self.assertTrue(completion.budget_complaints(99999, 100000),
                        "a pair that is ordered but beyond the evidenced bound is still refused")


class AnInterpreterHasToRunThisAdapter(unittest.TestCase):
    def test_a_python_too_old_for_this_adapter_is_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            pretender = Path(temporary) / "old-python"
            pretender.write_text("#!/bin/sh\necho '2.7'\n", encoding="utf-8")
            pretender.chmod(0o755)
            with self.assertRaises(ValueError) as raised:
                completion.interpreter_for(str(pretender))
        self.assertIn("2.7", str(raised.exception))
        self.assertIn(".".join(str(p) for p in completion.SUPPORTED_PYTHON),
                      str(raised.exception))

    def test_a_plan_does_not_run_the_program_the_caller_named(self):
        """A command that writes nothing should not execute a caller-supplied binary."""
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "it-ran"
            pretender = Path(temporary) / "loud"
            pretender.write_text("#!/bin/sh\ntouch " + str(marker) + "\necho '3.12'\n",
                                 encoding="utf-8")
            pretender.chmod(0o755)
            completion.interpreter_for(str(pretender), run=False)
            self.assertFalse(marker.exists(), "planning executed it")
            completion.interpreter_for(str(pretender), run=True)
            self.assertTrue(marker.exists(), "applying checks it")


class ADanglingLinkIsSomethingRatherThanNothing(unittest.TestCase):
    """The repository's own four-state contract puts a link with an established-missing target
    in UNREADABLE. Reported as ABSENT it says the component was never installed, when what
    happened is that its target went away, and the fact that would repair it is gone."""

    def test_a_broken_link_is_unreadable_and_not_absent(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "dangling").symlink_to(home / "never-existed")
            found = completion.presence(home / "dangling", "the configured runtime")
        self.assertEqual(found["value"], reading.UNREADABLE)
        self.assertIn("target does not exist", found["evidence"])

    def test_a_live_link_is_present(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "real").write_text("", encoding="utf-8")
            (home / "link").symlink_to(home / "real")
            self.assertEqual(completion.presence(home / "link", "x")["value"], reading.PRESENT)

    def test_status_reports_a_broken_pointer_as_broken(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "codex-session-relay").symlink_to(home / "gone")
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["relayExecutable"]["value"], reading.UNREADABLE,
                         "an update that moved the pointer is a different repair from a"
                         " runtime that was never installed")

    def test_an_event_this_adapter_has_no_decision_for_is_refused(self):
        """The guard judges a turn ending. On any other event the payload means something else
        and the output schema carries no top-level decision, so the hook would be registered,
        inert, and silent about it."""
        self.assertEqual(completion.registration_complaints(None), [])
        self.assertEqual(completion.registration_complaints(completion.EVENT), [])
        self.assertTrue(completion.registration_complaints("SessionStart"))
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            args = argparse.Namespace(
                codex_home=str(home), event="SessionStart", hook_command=None,
                adapter="completion", dest=None,
                relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            emitted = []
            with mock.patch.object(runtime_install, "emit", side_effect=emitted.append):
                code = runtime_install.cmd_hook(args)
            self.assertFalse((home / "hooks.json").exists())
            self.assertFalse(completion.configuration_path(home).exists())
        self.assertEqual(code, 2)
        self.assertIn(completion.EVENT, emitted[0]["error"])


class TheJournalPolicyReadsItsOwnField(unittest.TestCase):
    """faults_only was reading a key no record carries, so it recorded everything."""

    def test_faults_only_keeps_the_failures_and_drops_the_answers(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary, journalPolicy=completion.FAULTS_ONLY)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            self.assertEqual(journalled(temporary), [],
                             "a guard that answered is not a fault")
            os.remove(Path(temporary) / "codex-session-relay")
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            records = journalled(temporary)
        self.assertEqual([r["adapterOutcome"] for r in records],
                         [completion.GUARD_UNREACHABLE])

    def test_every_invocation_keeps_both(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            settings(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            self.assertEqual(len(journalled(temporary)), 1)


class OwnershipStaysSeparate(unittest.TestCase):
    """Criterion 5: this hook's own file, and nobody else's state."""

    def test_the_settings_are_this_hooks_own_file(self):
        self.assertEqual(completion.configuration_path("/home/x/.codex", environ={}).name,
                         completion.CONFIG_NAME)

    def test_installing_preserves_every_hook_already_registered(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "hooks.json").write_text(json.dumps({"hooks": {completion.EVENT: [
                {"hooks": [{"type": "command", "command": "/somebody/else/hook.sh",
                            "timeout": 10}]}]}}), encoding="utf-8")
            args = argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-37", apply=True)
            with mock.patch.object(runtime_install, "emit"):
                runtime_install.cmd_hook(args)
            written = json.loads((home / "hooks.json").read_text(encoding="utf-8"))
        groups = written["hooks"][completion.EVENT]
        self.assertEqual(groups[0]["hooks"][0]["command"], "/somebody/else/hook.sh",
                         "installation appends, so no existing identity is renumbered")
        self.assertEqual(len(groups), 2)

    def test_nothing_here_reads_or_writes_another_hooks_state(self):
        source = (ROOT / "scripts" / "crw_runtime" / "completion.py").read_text(encoding="utf-8")
        entry = ENTRY_POINT.read_text(encoding="utf-8")
        for forbidden in (".codexclaw", "goalplan", "ledger.jsonl", "sessions/"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, source)
                self.assertNotIn(forbidden, entry)


class TheCauseOfAnAbsence(unittest.TestCase):
    """CRW-100: an absence of firing evidence answers WHY, and says so when it cannot.

    Three hosts used to produce the same report -- a hook that was never registered, one that
    is registered and has not run, and one that has run and recorded into a journal this
    command was not reading. The operator procedure closed that honestly by writing that the
    tool does not distinguish them, and that sentence is what these cases remove.
    """

    def _host(self, temporary):
        fake_relay(temporary, stdout=json.dumps(RELEASED))
        return Path(temporary)

    def test_three_hosts_that_look_alike_answer_three_different_causes(self):
        answers = {}
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary)
            answers["never registered"] = why_no_record(temporary).get("value")
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            answers["registered, nothing recorded"] = why_no_record(temporary).get("value")
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            # Actually fired, into the journal the SECOND registration names. The first
            # registration's journal stays empty, which is the reading an operator would have
            # taken and reported as "this hook has never run".
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={},
                           settings=str(second))
            answers["recorded elsewhere"] = why_no_record(temporary).get("value")

        self.assertEqual(len(set(answers.values())), 3,
                         "three hosts with three different repairs answered " + repr(answers))
        self.assertEqual(answers["never registered"], firing.NOT_REGISTERED)
        self.assertEqual(answers["registered, nothing recorded"], firing.NOTHING_RECORDED)
        self.assertEqual(answers["recorded elsewhere"], firing.RECORDED_ON_ANOTHER_PATH)

    def test_which_of_the_two_journals_holds_the_record_does_not_change_the_answer(self):
        """No reference path, so no sort order to depend on. The reading that reports this is
        symmetric over the journals the registrations name, and a cause that changed when the
        record moved between them would be an artefact of which path sorted first."""
        seen = {}
        for label in ("first", "second"):
            with tempfile.TemporaryDirectory() as temporary:
                self._host(temporary)
                register(temporary)
                paths = dict(zip(("first", "second"),
                                 second_registration(temporary, "journal-two")))
                completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                               environ={}, settings=str(paths[label]))
                seen[label] = why_no_record(temporary).get("value")
        self.assertEqual(seen["first"], seen["second"], seen)
        self.assertEqual(seen["first"], firing.RECORDED_ON_ANOTHER_PATH)

    def test_a_policy_that_records_only_faults_names_both_candidates_rather_than_choosing(self):
        """The ambiguity this command genuinely has, expressed as an answer.

        Under faults_only an empty journal is what a hook that fired and never faulted leaves
        behind, and it is also what a hook that never fired leaves behind. Choosing either
        would be a guess, so the cell reports that the cause is unsettled and carries both.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            amend_settings(temporary, journalPolicy=completion.FAULTS_ONLY)
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.CAUSE_UNREADABLE)
        self.assertEqual(
            sorted(entry["cause"] for entry in cell.get("candidates") or []),
            sorted((firing.NOTHING_RECORDED, firing.POLICY_RECORDS_ONLY_FAULTS)),
            "an unsettled cause that does not carry what is still standing has resolved the"
            " ambiguity by omission")

    def test_an_adapter_the_host_cannot_start_is_its_own_cause(self):
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            break_the_target(temporary)
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.ADAPTER_CANNOT_RUN,
                         "an empty journal under a program the host cannot start is that"
                         " program's absence, not a second cause beside it")

    def test_two_repairs_are_reported_as_two_and_never_as_the_first_one_alone(self):
        """Whether the host can start the adapter is not downstream of the settings. A run that
        stopped at the first cause it found reported the deleted settings and said nothing
        about the deleted adapter beside them, and only one of those was going to be fixed."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            break_the_target(temporary)
            completion.configuration_path(Path(temporary)).unlink()
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.SEVERAL_CAUSES)
        self.assertEqual(
            sorted(entry["cause"] for entry in cell.get("candidates") or []),
            sorted((firing.ADAPTER_CANNOT_RUN, firing.SETTINGS_ABSENT)))

    def test_a_journal_switched_off_by_policy_is_not_an_empty_one(self):
        """journal() returns without writing on no_journal whatever root is configured, so a
        host that keeps no journal by configuration must not read as one whose journal happens
        to be empty: the first says nothing at all about firing."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            amend_settings(temporary, journalPolicy=completion.NO_JOURNAL)
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.JOURNALLING_OFF)


class OneBrokenRegistrationNeverAnswersForItsPeer(unittest.TestCase):
    """Review on PR #52 head ef4d952, found independently by both reviewers.

    Every registration runs and reads its own settings, so a peer that is fine says nothing
    about a peer that is broken. The first cut required EVERY named settings file to be absent
    before it said so, and collapsed the two adapter probes into a worst-of value, so one good
    registration hid the other's repair and one bad one claimed the host could not have run.
    """

    def _host(self, temporary):
        fake_relay(temporary, stdout=json.dumps(RELEASED))
        return Path(temporary)

    def test_a_missing_settings_file_beside_a_usable_one_is_still_reported(self):
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            second.unlink()
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.SEVERAL_CAUSES,
                         "one usable settings file answered for a registration whose settings"
                         " are gone, and that registration releases every invocation until"
                         " they come back")
        self.assertEqual(
            sorted(entry["cause"] for entry in cell.get("candidates") or []),
            sorted((firing.NOTHING_RECORDED, firing.SETTINGS_ABSENT)))

    def test_a_rejected_settings_file_beside_a_usable_one_is_still_reported(self):
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            document["mode"] = "nonsense"
            second.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.SEVERAL_CAUSES)
        self.assertIn(firing.SETTINGS_UNUSABLE,
                      [entry["cause"] for entry in cell.get("candidates") or []])

    def test_one_unstartable_registration_does_not_answer_for_a_startable_one(self):
        """The adapter and interpreter cells report their worst probe, which answers "is
        anything broken" and was read as "is everything broken". The empty journal of the
        registration that CAN start was then never reported at all."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            second_registration(temporary, "journal-two")
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            # Only the FIRST registration's adapter is gone. The second is untouched and runs.
            entries[0]["command"] = entries[0]["command"].replace(
                str(ENTRY_POINT), str(Path(temporary) / completion.ENTRY_POINT_NAME))
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.SEVERAL_CAUSES)
        self.assertEqual(
            sorted(entry["cause"] for entry in cell.get("candidates") or []),
            sorted((firing.ADAPTER_CANNOT_RUN, firing.NOTHING_RECORDED)),
            "a registration the host cannot start is a repair, and it must not silence what"
            " the registration beside it recorded")

    def test_when_nothing_can_start_the_empty_journal_is_still_not_a_second_cause(self):
        """The direction the fix must not break: with no startable registration at all, an
        empty journal is that program's absence and adapter_cannot_run is the whole repair."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            break_the_target(temporary)
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.ADAPTER_CANNOT_RUN)


    def test_an_unreachable_settings_file_is_never_reported_as_a_rejected_one(self):
        """No bytes were read, so nothing establishes that this hook's reader rejects it.
        Reporting it as unusable sends the operator to repair a file that the session opens
        perfectly well and that only this process could not reach."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            with mock.patch.object(completion.reading, "read_json", side_effect=(
                    lambda path, what, **kw: completion.reading.Reading(
                        state=completion.reading.ACCESS_ERROR, source=path,
                        detail="PermissionError: [Errno 13] Permission denied")
                    if str(path) == str(second) else real_read_json(path, what, **kw))):
                cell = why_no_record(temporary)
        established = [entry["cause"] for entry in cell.get("candidates") or []
                       if entry["standing"] == firing.ESTABLISHED]
        self.assertNotIn(firing.SETTINGS_UNUSABLE, established,
                         "a file nobody could open was ESTABLISHED as one the reader rejected,"
                         " which recommends a repair no reading supports")
        self.assertEqual(cell.get("value"), firing.CAUSE_UNREADABLE,
                         "nothing was settled about that file, so the cause is not settled")

    def test_a_registration_that_keeps_no_journal_is_reported_beside_a_peer_that_does(self):
        """A registration configured never to record can never produce firing evidence, and a
        neighbour that records normally is not evidence that it can."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalPolicy"] = completion.NO_JOURNAL
            second.write_text(json.dumps(document), encoding="utf-8")
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={},
                           settings=str(completion.configuration_path(Path(temporary))))
            cell = why_no_record(temporary)
        self.assertIn(firing.JOURNALLING_OFF,
                      [entry["cause"] for entry in cell.get("candidates") or []],
                      "one registration records and the other is configured never to, and only"
                      " the first was reported")

    def test_an_unstartable_peer_does_not_make_a_working_one_look_like_another_path(self):
        """The reverse mixed case. If A can start and holds records while B cannot start and
        its journal is therefore empty, B's emptiness is wholly explained by B, and reading it
        as 'recorded on another path' invents a second story about A."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            first, _second = second_registration(temporary, "journal-two")
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={},
                           settings=str(first))
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            entries[1]["command"] = entries[1]["command"].replace(
                str(ENTRY_POINT), str(Path(temporary) / completion.ENTRY_POINT_NAME))
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        causes = [entry["cause"] for entry in cell.get("candidates") or []]
        self.assertNotIn(firing.RECORDED_ON_ANOTHER_PATH, causes,
                         "an unstartable registration's necessarily empty journal was read as"
                         " its neighbour recording somewhere else")
        self.assertIn(firing.ADAPTER_CANNOT_RUN, causes)

    def test_a_relative_peer_does_not_suppress_a_known_path_s_own_cause(self):
        """record_path_unidentified is established when ANY registration spells its settings
        relatively. As a prerequisite it then blanked out every downstream cause for the
        registrations whose paths ARE known, so a peer's readably-absent settings file went
        unreported behind a spelling this command could not resolve."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            second.unlink()
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            # The FIRST registration now names its settings relatively; the second names an
            # absolute path this command can read, and that file is gone.
            entries[0]["command"] = entries[0]["command"].replace(
                str(completion.configuration_path(Path(temporary))), "relative-settings.json")
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        causes = sorted(entry["cause"] for entry in cell.get("candidates") or []
                        if entry["standing"] == firing.ESTABLISHED)
        self.assertEqual(causes, sorted((firing.RECORD_PATH_UNIDENTIFIED,
                                         firing.SETTINGS_ABSENT)),
                         "a path this command could not identify hid a repair it could")

    def test_registrations_that_name_nothing_readable_leave_the_settings_causes_unasked(self):
        """SUPPORT, not evidence of the defect: this passes before the fix too. It holds the
        direction the fix must not break — with no readable settings path at all there is no
        question for the settings causes to answer, and dropping the prerequisite must not put
        an unsupported candidate on the table."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entry = document["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(
                str(completion.configuration_path(Path(temporary))), "relative-settings.json")
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.RECORD_PATH_UNIDENTIFIED)

    def test_found_records_never_stand_beside_a_repair(self):
        """records_found is the one terminal answer: it says there is no absence to explain.
        Established off the subset this command could read, it appeared beside
        record_path_unidentified and presented found records as though they were a repair."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            first, second = second_registration(temporary, "journal-two")
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={},
                           settings=str(second))
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            document["hooks"][completion.EVENT][0]["hooks"][0]["command"] = (
                document["hooks"][completion.EVENT][0]["hooks"][0]["command"].replace(
                    str(first), "relative-settings.json"))
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.RECORD_PATH_UNIDENTIFIED,
                         "records that need no repair were reported as one of several causes")

    def test_a_disabled_journal_survives_its_registration_being_unstartable(self):
        """Unlike an empty journal, a disabled policy is not explained by the adapter being
        unstartable: repairing the adapter still produces no firing evidence until journalling
        is switched back on, so that is a second repair and has to be said."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            amend_settings(temporary, journalPolicy=completion.NO_JOURNAL)
            break_the_target(temporary)
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.SEVERAL_CAUSES)
        self.assertEqual(
            sorted(entry["cause"] for entry in cell.get("candidates") or []),
            sorted((firing.ADAPTER_CANNOT_RUN, firing.JOURNALLING_OFF)))

    def test_one_journal_read_serves_both_the_count_and_the_cause(self):
        """Reading the journal twice opened a window: a Stop landing between the two reads
        produced a payload whose firingJournal said ABSENT while the cause beside it said
        records_found, so the explanation contradicted the cell it was explaining."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            calls = []
            real_cell = completion._journal_cell

            def counting(config, *passed, **keywords):
                calls.append(str((config or {}).get("journalRoot")))
                return real_cell(config, *passed, **keywords)

            with mock.patch.object(completion, "_journal_cell", side_effect=counting):
                found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(len(calls), 1,
                         "the same journal was read twice in one status call, so the count and"
                         " the cause can disagree about a host that changed between them: "
                         + repr(calls))
        self.assertEqual(found["firingJournal"],
                         found["configuration"]["namedSettings"][0]["journal"],
                         "the cell and the reading the cause used are one reading")

    def test_one_settings_read_serves_the_cell_and_the_cause(self):
        """A file read twice in one call is a payload that can contradict itself: the
        configuration cell reported PRESENT from the first read while namedSettings reported
        ABSENT from the second, about one file, in one answer."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            settled = str(completion.configuration_path(Path(temporary)))
            reads = []
            real = completion.read_configuration

            def counting(path, *passed, **keywords):
                reads.append(str(path))
                return real(path, *passed, **keywords)

            with mock.patch.object(completion, "read_configuration", side_effect=counting):
                found = completion.status(codex_home=temporary, environ={})
        self.assertEqual([one for one in reads if one == settled], [settled],
                         "the settled settings file was read more than once in one status"
                         " call, so its two reports can disagree: " + repr(reads))
        self.assertEqual(found["configuration"]["namedSettings"][0]["settings"], settled)

    def test_the_adapter_is_probed_once_for_the_cell_and_the_cause(self):
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            probes = []
            real = completion.presence

            def counting(path, what, **kwargs):
                if what == "the adapter script":
                    probes.append(str(path))
                return real(path, what, **kwargs)

            with mock.patch.object(completion, "presence", side_effect=counting):
                completion.status(codex_home=temporary, environ={})
        self.assertEqual(len(probes), 1,
                         "one adapter was probed twice in one status call, so the target cell"
                         " and the cause can disagree about it: " + repr(probes))

    def test_one_journal_is_listed_once_however_many_registrations_name_it(self):
        """Two settings files can configure the same journalRoot. Listing that one directory
        twice let a Stop between the reads report two counts for one directory -- enough to
        establish 'recorded on another path' when there is only one path."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            # A second settings file naming the SAME journal as the first.
            second_registration(temporary, "journal")
            roots = []
            real = completion._journal_cell

            def counting(config, *passed, **keywords):
                roots.append(str((config or {}).get("journalRoot")))
                return real(config, *passed, **keywords)

            with mock.patch.object(completion, "_journal_cell", side_effect=counting):
                completion.status(codex_home=temporary, environ={})
        self.assertEqual(len(roots), len(set(roots)),
                         "one journal directory was listed more than once in a single status"
                         " call, so its two counts can disagree: " + repr(roots))

    def test_one_adapter_is_probed_once_however_many_registrations_run_it(self):
        """Two registrations can run one adapter with different settings arguments. Probing
        that one file twice let the payload give them different startability."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            second_registration(temporary, "journal-two")
            probes = []
            real = completion.presence

            def counting(path, what, **kwargs):
                if what == "the adapter script":
                    probes.append(str(path))
                return real(path, what, **kwargs)

            with mock.patch.object(completion, "presence", side_effect=counting):
                completion.status(codex_home=temporary, environ={})
        self.assertEqual(len(probes), len(set(probes)),
                         "one adapter target was probed more than once in a single status"
                         " call, so two registrations running it can be given different"
                         " startability: " + repr(probes))

    def test_alias_spellings_of_one_resource_are_one_reading(self):
        """The class behind three findings: a cache keyed by the raw string still read one file
        twice when two registrations spelled it differently. The host opens one file, and two
        readings of it in one status call can disagree about it."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            # The same journal as the first registration, spelled with a redundant './'. NOT a
            # trailing separator: that one requires a directory, so it is not interchangeable
            # to the kernel and this key deliberately keeps it distinct.
            document["journalRoot"] = str(Path(temporary) / "." / "journal")
            second.write_text(json.dumps(document), encoding="utf-8")
            path = Path(temporary) / "hooks.json"
            hooks_file = json.loads(path.read_text(encoding="utf-8"))
            entries = hooks_file["hooks"][completion.EVENT][0]["hooks"]
            # The same adapter and the same interpreter, spelled with a redundant './'.
            aliased = str(ENTRY_POINT.parent) + os.sep + "." + os.sep + ENTRY_POINT.name
            entries[1]["command"] = entries[1]["command"].replace(str(ENTRY_POINT), aliased)
            path.write_text(json.dumps(hooks_file), encoding="utf-8")

            journals, adapters = [], []
            real_journal, real_presence = completion._journal_cell, completion.presence
            # Keyed here with the stdlib rather than with the module's own helper, so a head
            # that has no such helper fails on the behaviour instead of on a missing name.
            same = lambda value: os.path.normpath(os.path.expanduser(str(value)))

            def counting_journal(config, *passed, **keywords):
                journals.append(same((config or {}).get("journalRoot") or ""))
                return real_journal(config, *passed, **keywords)

            def counting_presence(target, what, **kwargs):
                if what in ("the adapter script", "the registered interpreter"):
                    adapters.append(same(target))
                return real_presence(target, what, **kwargs)

            with mock.patch.object(completion, "_journal_cell", side_effect=counting_journal), \
                 mock.patch.object(completion, "presence", side_effect=counting_presence):
                completion.status(codex_home=temporary, environ={})
        self.assertEqual(len(journals), len(set(journals)),
                         "one journal directory, two spellings, two listings: " + repr(journals))
        self.assertEqual(len(adapters), len(set(adapters)),
                         "one program, two spellings, two probes: " + repr(adapters))

    def test_a_key_never_merges_two_spellings_the_kernel_keeps_apart(self):
        """The direction that matters. Sharing a reading between two spellings of ONE file
        costs a syscall; sharing it between spellings of TWO files reports another resource's
        answer. normpath cancels 'X/..', and the kernel follows X first when X is a symlink, so
        that cancellation is not this function's to make."""
        keys = completion.resource_key
        # Same file, so one key: '.' components and duplicate separators name the same path.
        self.assertEqual(keys("/tmp/./hook.py"), keys("/tmp/hook.py"))
        self.assertEqual(keys("/tmp//hook.py"), keys("/tmp/hook.py"))
        # Not provably the same file, so never one key.
        self.assertNotEqual(keys("/srv/link/../hook.py"), keys("/srv/hook.py"),
                            "'..' was cancelled, which merges two files whenever the segment"
                            " before it is a symlink")
        self.assertNotEqual(keys("/tmp/hook.py/"), keys("/tmp/hook.py"),
                            "a trailing separator requires a directory, so the two are not"
                            " interchangeable to lstat")
        self.assertNotEqual(keys("/tmp/hook.py/."), keys("/tmp/hook.py"),
                            "a trailing '.' is the same demand for a directory written"
                            " differently")
        self.assertNotEqual(completion.NO_ROOT, keys(""),
                            "a configuration naming no journal root must not key as a path;"
                            " the empty string normalises to one a configuration may name")

    def test_a_peer_nobody_could_judge_is_not_treated_as_one_that_cannot_start(self):
        """A probe this command did not judge -- a workspace-dependent spelling, or one it
        could not reach -- is neither 'starts' nor 'cannot start'. Recorded as the latter, that
        registration's journal dropped out of every question while a blocked neighbour supplied
        a settled explanation for the whole host."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            second_registration(temporary, "journal-two")
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            # One registration definitively blocked: its adapter is gone.
            entries[0]["command"] = entries[0]["command"].replace(
                str(ENTRY_POINT), str(Path(temporary) / completion.ENTRY_POINT_NAME))
            # The other unjudged: a relative interpreter resolves per workspace, so this
            # command does not probe it and cannot say whether the host can start it.
            entries[1]["command"] = "./python " + entries[1]["command"].split(" ", 1)[1]
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(cell.get("value"), firing.CAUSE_UNREADABLE,
                         "a blocked registration settled the whole host while a peer nobody"
                         " could judge was quietly counted as unable to run")
        self.assertIn(firing.ADAPTER_CANNOT_RUN,
                      [entry["cause"] for entry in cell.get("candidates") or []])

    def test_an_unjudged_peer_never_unmakes_what_was_observed(self):
        """The guard that keeps an unjudged peer visible must run AFTER each rule's own
        positive evidence. Placed first, it downgraded established causes -- a registration
        configured never to record, and a genuine holding-and-empty divergence -- into
        uncertainty because an unrelated peer could not be probed."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalPolicy"] = completion.NO_JOURNAL
            second.write_text(json.dumps(document), encoding="utf-8")
            path = Path(temporary) / "hooks.json"
            hooks_file = json.loads(path.read_text(encoding="utf-8"))
            entries = hooks_file["hooks"][completion.EVENT][0]["hooks"]
            third = dict(entries[0])
            # A third registration nobody can judge: a relative interpreter resolves per
            # workspace and is not probed from here.
            third["command"] = "./python " + entries[0]["command"].split(" ", 1)[1]
            entries.append(third)
            path.write_text(json.dumps(hooks_file), encoding="utf-8")
            cell = why_no_record(temporary)
        established = [entry["cause"] for entry in cell.get("candidates") or []
                       if entry["standing"] == firing.ESTABLISHED]
        self.assertIn(firing.JOURNALLING_OFF, established,
                      "a registration configured never to record is an established repair"
                      " whatever an unrelated peer's startability could not be established")

    def test_a_configured_policy_is_not_reopened_by_an_unjudged_peer(self):
        """Whether a registration keeps a journal is what its settings say, and no startability
        reading can change that. Letting uncertainty about a peer reopen it put journalling_off
        on the table for a host whose settings conclusively rule it out."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entry = document["hooks"][completion.EVENT][0]["hooks"][0]
            # A relative interpreter resolves per workspace, so startability is unjudged while
            # the settings still say every_invocation.
            entry["command"] = "./python " + entry["command"].split(" ", 1)[1]
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertNotIn(firing.JOURNALLING_OFF,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "the settings enable journalling, so that cause is ruled out whatever"
                         " could not be established about starting the program")
        self.assertNotIn(firing.POLICY_RECORDS_ONLY_FAULTS,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "the policy is every_invocation, which the settings settle")

    def test_a_journal_that_cannot_be_listed_is_not_an_empty_one_under_faults_only(self):
        """An unknown count is not zero. Read as zero, the faults-only candidate went on the
        table without an empty journal ever having been observed."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            # A regular file where the journal root should be: scandir raises NotADirectoryError,
            # so the count is unestablished rather than zero, and this case is about that and
            # not about any other way a listing can fail.
            blocked = Path(temporary) / "not-a-journal"
            blocked.write_text("", encoding="utf-8")
            amend_settings(temporary, journalPolicy=completion.FAULTS_ONLY,
                           journalRoot=str(blocked))
            cell = why_no_record(temporary)
        self.assertNotIn(firing.POLICY_RECORDS_ONLY_FAULTS,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "a journal nobody could list was counted as an empty one")

    def test_a_journal_path_that_cannot_name_a_file_is_a_reading_not_a_crash(self):
        """complaints() accepts any absolute string, and one carrying a NUL cannot name a path,
        so scandir raises ValueError rather than OSError. The settings read back fine, so a
        journal reading is what belongs here."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            amend_settings(temporary, journalRoot=str(Path(temporary) / "journal") + "\x00bad")
            try:
                found = completion.status(codex_home=temporary, environ={})
            except Exception as error:
                found = {"raised": type(error).__name__ + ": " + str(error)}
        self.assertNotIn("raised", found,
                         "a journal path that cannot name a file raised out of status instead"
                         " of answering: " + str(found.get("raised")))
        self.assertEqual(found["firingJournal"]["value"], reading.ACCESS_ERROR)

    def test_an_unlistable_journal_beside_an_empty_one_leaves_divergence_standing(self):
        """The unread journal may hold records, and against a journal that was read and holds
        none that is exactly recorded_on_another_path. Ruling it out because the holding side
        happens to be the one nobody could list drops a candidate the readings leave open."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            # A regular file where the second journal should be: it cannot be listed, so its
            # count is unestablished while the first is read and holds nothing.
            blocked = Path(temporary) / "not-a-journal"
            blocked.write_text("", encoding="utf-8")
            document["journalRoot"] = str(blocked)
            second.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertIn(firing.RECORDED_ON_ANOTHER_PATH,
                      [one["cause"] for one in cell.get("candidates") or []],
                      "one journal read and empty beside one nobody could list leaves this"
                      " cause standing, and it was ruled out")
        self.assertEqual(cell.get("value"), firing.CAUSE_UNREADABLE)

    def test_two_journals_nobody_could_list_leave_divergence_standing(self):
        """Neither side was read, so neither is settled: one unread journal may hold records
        while the other is empty, which is exactly this cause. Ruling it out omits a candidate
        precisely where the command promises to carry every unsettled one."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            blocked = [Path(temporary) / "not-a-journal-one", Path(temporary) / "not-a-journal-two"]
            for one in blocked:
                one.write_text("", encoding="utf-8")
            amend_settings(temporary, journalRoot=str(blocked[0]))
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalRoot"] = str(blocked[1])
            second.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertIn(firing.RECORDED_ON_ANOTHER_PATH,
                      [one["cause"] for one in cell.get("candidates") or []],
                      "neither journal was read, so this cause is unsettled rather than out")

    def test_one_journal_directory_is_listed_once_however_it_is_spelled(self):
        """A journal root is opened as a directory and _journal_cell puts it through Path, so a
        trailing separator reaches the same scandir. Keyed apart, one directory was listed
        twice and a Stop between the listings could give it two counts."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalRoot"] = str(Path(temporary) / "journal") + os.sep
            second.write_text(json.dumps(document), encoding="utf-8")
            listed = []
            real = completion._journal_cell

            def counting(config, *passed, **keywords):
                listed.append(str(Path(str((config or {}).get("journalRoot") or ""))))
                return real(config, *passed, **keywords)

            with mock.patch.object(completion, "_journal_cell", side_effect=counting):
                completion.status(codex_home=temporary, environ={})
        self.assertEqual(len(listed), len(set(listed)),
                         "one journal directory, two spellings, two listings: " + repr(listed))

    def test_two_registrations_sharing_one_unread_journal_cannot_disagree(self):
        """Registrations are not journals. Two of them can name separate settings files that
        configure one journalRoot, and one directory cannot disagree with itself, so counting
        entries reported possible divergence for a host that has a single unlistable journal."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            blocked = Path(temporary) / "not-a-journal"
            blocked.write_text("", encoding="utf-8")
            amend_settings(temporary, journalRoot=str(blocked))
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalRoot"] = str(blocked)
            second.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertNotIn(firing.RECORDED_ON_ANOTHER_PATH,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "one journal was counted as two and reported as possibly disagreeing"
                         " with itself")

    def test_an_unjudged_peer_sharing_one_journal_cannot_disagree_with_itself(self):
        """An unjudged peer only matters to this cause when it could be a SECOND journal. One
        sharing the journal its neighbour already named cannot disagree with itself, however
        its startability reads."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            second_registration(temporary, "journal")  # the SAME journal as the first
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            # The second registration's interpreter is relative, so it is unjudged.
            entries[1]["command"] = "./python " + entries[1]["command"].split(" ", 1)[1]
            path.write_text(json.dumps(document), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertNotIn(firing.RECORDED_ON_ANOTHER_PATH,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "one journal was reported as possibly disagreeing with itself because"
                         " a peer naming it could not be judged")

    def test_a_lone_unjudged_registration_is_not_two_journals_disagreeing(self):
        """No peer at all, which the shared-journal case above does not cover.

        The guard there asked whether an unjudged journal was NOVEL against the ones already
        read. On a host whose only registration is unjudged that set is empty, so novelty is
        satisfied by default and the cause stood on a host with exactly one journal. This cause
        is two journals disagreeing; one directory cannot disagree with itself.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            # The ONE registration's interpreter is relative, so this command does not probe it
            # and never establishes whether the host can start it.
            entries[0]["command"] = "./python " + entries[0]["command"].split(" ", 1)[1]
            path.write_text(json.dumps(document), encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        named = found["configuration"]["namedSettings"]
        cell = found["firingRecordAbsence"]
        self.assertEqual(len({entry.get("journalRoot") for entry in named}), 1,
                         "the fixture did not build the single-journal host this case is"
                         " about: " + repr(named))
        self.assertEqual([entry.get("startable") for entry in named], [None],
                         "the fixture did not leave the one registration unjudged")
        self.assertNotIn(firing.RECORDED_ON_ANOTHER_PATH,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "a host with one journal was told its journals may disagree, on the"
                         " strength of nobody having judged the only registration it has")

    def test_a_peer_that_keeps_no_journal_never_unsettles_a_count(self):
        """Every rule that asks about an unjudged peer is a rule about counts, and a
        registration configured never to record contributes no count whether or not the host
        can start it: repairing its startability would still leave it recording nothing. Left
        in the unjudged set it kept the count causes unsettled beside a peer whose journal had
        been read, which is uncertainty about a registration that cannot hold a record either
        way.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            document = json.loads(second.read_text(encoding="utf-8"))
            # This registration keeps no journal at all.
            document.pop("journalRoot", None)
            second.write_text(json.dumps(document), encoding="utf-8")
            path = Path(temporary) / "hooks.json"
            hooks_file = json.loads(path.read_text(encoding="utf-8"))
            entries = hooks_file["hooks"][completion.EVENT][0]["hooks"]
            # ... and its interpreter is relative, so its startability is never established.
            entries[1]["command"] = "./python " + entries[1]["command"].split(" ", 1)[1]
            path.write_text(json.dumps(hooks_file), encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.JOURNALLING_OFF), firing.ESTABLISHED,
                         "the fixture did not build the non-journalling peer this case is"
                         " about: " + repr(standings))
        self.assertEqual(standings.get(firing.NOTHING_RECORDED), firing.ESTABLISHED,
                         "the journal that WAS read holds nothing, and a peer that keeps no"
                         " journal at all left that reading unsettled")
        self.assertNotIn(firing.RECORDED_ON_ANOTHER_PATH,
                         [one["cause"] for one in cell.get("candidates") or []],
                         "a registration that records nothing by configuration was counted as"
                         " a journal that might disagree with the one that was read")

    def test_two_spellings_of_one_journal_are_not_two_journals(self):
        """resource_key is lexical and refuses to resolve, on purpose. That leaves one
        directory reachable through a symlink alias keyed twice, and the divergence rule then
        counted two unread journals for the one directory both registrations name. Same
        "one journal cannot disagree with itself" the redundant-dot and trailing-separator
        cases closed, reached through the kernel rather than through a string.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            # One real journal, named by two spellings. Listed twice it is two readings of one
            # directory, and a Stop landing between them gives one alias a count the other does
            # not have -- which this answer set reads as two journals disagreeing.
            one = Path(temporary) / "one-journal"
            one.mkdir()
            alias = Path(temporary) / "an-alias-of-one-journal"
            alias.symlink_to(one)
            amend_settings(temporary, journalRoot=str(one))
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalRoot"] = str(alias)
            second.write_text(json.dumps(document), encoding="utf-8")
            self.assertTrue(os.path.samefile(str(alias), str(one)),
                            "the fixture did not build one directory under two spellings")
            real, listed = completion._journal_cell, []

            def listing(config, *passed, **keywords):
                listed.append(config.get("journalRoot"))
                return real(config, *passed, **keywords)

            with mock.patch.object(completion, "_journal_cell", side_effect=listing):
                found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(listed, [str(one)],
                         "one directory was listed twice, once per spelling: " + repr(listed))
        named = found["configuration"]["namedSettings"]
        self.assertEqual([entry.get("journalRoot") for entry in named], [str(one), str(one)],
                         "the second spelling was answered by its own reading of the directory"
                         " the first had already read")

    def test_a_plugin_owned_registration_is_not_an_absent_one(self):
        """The hook file is deliberately empty on a plugin-owned host: that registration lives
        in the package manifest, which this command does not read. Establishing an absence from
        the one file it is deliberately not in named a repair that would put a second owner on
        one event, which is exactly what the ownership rules refuse.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(ENTRY_POINT))
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registrationOwner"]["value"], completion.OWNER_PLUGIN,
                         "the fixture did not build the plugin-owned host this case is about")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertNotEqual(standings.get(firing.NOT_REGISTERED), firing.ESTABLISHED,
                            "an empty hook file on a plugin-owned host was read as an absent"
                            " registration, in a payload whose own cell says the plugin owns"
                            " it")
        self.assertEqual(cell.get("value"), firing.CAUSE_UNREADABLE,
                         "whether this adapter is registered was not established either way,"
                         " and the answer has to say so rather than choose")

    def test_a_link_retargeted_between_two_listings_does_not_share_a_snapshot(self):
        """The identity is captured WITH the snapshot rather than re-derived from the spelling.
        Comparing a stored spelling again asks the filesystem a fresh question, so a link
        retargeted between two registrations matched its NEW target and handed back the listing
        taken from the old one -- a wrong count rather than a missing one.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            holding = Path(temporary) / "the-links-first-target"
            empty = Path(temporary) / "the-links-second-target"
            holding.mkdir()
            empty.mkdir()
            alias = Path(temporary) / "retargeted-link"
            alias.symlink_to(holding)
            # The first registration reads the LINK; the second reads the link's eventual
            # target under its own name.
            amend_settings(temporary, journalRoot=str(alias))
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalRoot"] = str(empty)
            second.write_text(json.dumps(document), encoding="utf-8")

            real, taken = completion._journal_cell, []

            def listing(config, *passed, **keywords):
                cell = real(config)
                taken.append(config.get("journalRoot"))
                if len(taken) == 1:
                    # Between the two listings, exactly the window this case is about.
                    alias.unlink()
                    alias.symlink_to(empty)
                return cell

            with mock.patch.object(completion, "_journal_cell", side_effect=listing):
                found = completion.status(codex_home=temporary, environ={})
            self.assertEqual(taken[0], str(alias),
                             "the fixture did not read the link first: " + repr(taken))
        named = found["configuration"]["namedSettings"]
        self.assertEqual([entry.get("journalRoot") for entry in named],
                         [str(alias), str(empty)],
                         "the second registration was handed the listing taken from the link's"
                         " OLD target, because identity was re-derived from the spelling after"
                         " the link had moved")

    def test_a_link_retargeted_under_the_listing_is_not_published_as_an_alias(self):
        """One identity read cannot describe the other side of a read it did not take part in.
        A link retargeted between the stat and the scandir filed the NEW target's listing under
        the OLD target's identity, so a later registration that really names the old directory
        was handed a count belonging to one it never mentioned.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            read_as = Path(temporary) / "the-identity-that-was-read"
            listed = Path(temporary) / "the-directory-actually-listed"
            read_as.mkdir()
            listed.mkdir()
            alias = Path(temporary) / "retargeted-under-the-listing"
            alias.symlink_to(read_as)
            amend_settings(temporary, journalRoot=str(alias))
            document = json.loads(second.read_text(encoding="utf-8"))
            # The second registration really names the directory the identity was read from.
            document["journalRoot"] = str(read_as)
            second.write_text(json.dumps(document), encoding="utf-8")

            real, calls = completion._journal_cell, []

            def listing(config, *passed, **keywords):
                calls.append(config.get("journalRoot"))
                if len(calls) == 1:
                    # After the identity was taken, before the directory is listed.
                    alias.unlink()
                    alias.symlink_to(listed)
                return real(config, *passed, **keywords)

            with mock.patch.object(completion, "_journal_cell", side_effect=listing):
                found = completion.status(codex_home=temporary, environ={})
            self.assertEqual(calls[0], str(alias),
                             "the fixture did not list the link first: " + repr(calls))
        named = found["configuration"]["namedSettings"]
        self.assertEqual([entry.get("journalRoot") for entry in named],
                         [str(alias), str(read_as)],
                         "a listing taken from the directory the link moved TO was published"
                         " under the identity it had moved FROM, and the registration that"
                         " really names that identity inherited it")

    def test_a_plugin_owner_survives_settings_this_reader_rejects(self):
        """A document that reads back fine and fails some other check still records who owns
        the registration. Taking the default there established an absence on a host whose
        plugin package may be registering the hook perfectly well -- and suppressed
        settings_unusable, which is the cause that would have named the real repair.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(ENTRY_POINT), mode="not-a-mode-this-reader-knows")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.CONFIG_MALFORMED,
                         "the fixture did not build the readable-but-rejected settings this"
                         " case is about")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertNotEqual(standings.get(firing.NOT_REGISTERED), firing.ESTABLISHED,
                            "settings this reader rejects took the owner default with them,"
                            " and an absence was established for a plugin-owned host")

    def test_a_link_that_points_away_and_back_does_not_publish_its_identity(self):
        """Bracketing a listing with two path lookups is not enough. A link that points away
        and back again agrees with itself across the brackets while the listing in between came
        from somewhere else, and the count was filed under an identity it never came from. The
        identity now comes from the descriptor the listing was read through, which cannot be
        retargeted.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            there = Path(temporary) / "where-the-link-points-before-and-after"
            away = Path(temporary) / "where-the-link-points-during"
            there.mkdir()
            away.mkdir()
            alias = Path(temporary) / "a-link-that-points-away-and-back"
            alias.symlink_to(there)
            amend_settings(temporary, journalRoot=str(alias))
            document = json.loads(second.read_text(encoding="utf-8"))
            # The second registration really names the directory the link points at either side
            # of the listing, and never the one it was listed from.
            document["journalRoot"] = str(there)
            second.write_text(json.dumps(document), encoding="utf-8")

            real, calls = completion._journal_cell, []

            def listing(config, *passed, **keywords):
                calls.append(config.get("journalRoot"))
                if len(calls) == 1:
                    alias.unlink()
                    alias.symlink_to(away)
                    try:
                        return real(config, *passed, **keywords)
                    finally:
                        alias.unlink()
                        alias.symlink_to(there)
                return real(config, *passed, **keywords)

            with mock.patch.object(completion, "_journal_cell", side_effect=listing):
                found = completion.status(codex_home=temporary, environ={})
            self.assertEqual(calls[0], str(alias),
                             "the fixture did not list the link first: " + repr(calls))
        named = found["configuration"]["namedSettings"]
        self.assertEqual([entry.get("journalRoot") for entry in named],
                         [str(alias), str(there)],
                         "a listing taken from the directory the link pointed at DURING the"
                         " read was published under the identity it pointed at either side of"
                         " it, and the registration that really names that identity inherited"
                         " the wrong count")

    def test_settings_nobody_could_read_leave_the_registration_unsettled(self):
        """An owner nobody could read is not the default owner. Where no document was read at
        all, nothing establishes who owns the registration, so an empty hook file establishes
        nothing either -- and taking the default there established an absence on a host whose
        plugin package may be registering the hook perfectly well.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary)
            completion.configuration_path(Path(temporary)).write_text(
                "{ this is not json", encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.CONFIG_UNREADABLE,
                         "the fixture did not build the unreadable settings this case is"
                         " about: " + repr(found["configuration"]["value"]))
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertNotEqual(standings.get(firing.NOT_REGISTERED), firing.ESTABLISHED,
                            "settings nobody could read were answered with the default owner,"
                            " and an absence was established from a hook file that may not be"
                            " where this host's registration lives at all")

    def test_an_owner_this_reader_does_not_know_is_not_the_default_owner(self):
        """An OMITTED owner is the legacy user document owner_of is written for. An owner this
        reader does not know is the opposite: somebody wrote something there, and reading it as
        the default established an absence from a hook file that may not be where this host's
        registration lives -- while suppressing settings_unusable, the cause that would have
        named the actual repair.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner="an-owner-this-reader-does-not-know")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.CONFIG_MALFORMED,
                         "the fixture did not build the rejected-owner settings this case is"
                         " about: " + repr(found["configuration"]["value"]))
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertNotEqual(standings.get(firing.NOT_REGISTERED), firing.ESTABLISHED,
                            "an owner nobody recognises was read as the user owner, and an"
                            " absence was established on its strength")

    def test_a_plugin_owned_host_names_the_settings_repair_it_can_read(self):
        """A cause this command CAN distinguish and did not.

        A plugin-owned host registers through a manifest nobody here reads, so nothing in the
        hook file names a settings file and the settings causes had no entry to read -- on
        exactly the host whose repair they exist to name. The answer stopped at
        cause_unreadable while the payload's own configuration cell already said the settings
        this command settled on are rejected.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(ENTRY_POINT), mode="not-a-mode-this-reader-knows")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.CONFIG_MALFORMED,
                         "the fixture did not build the rejected plugin settings this case is"
                         " about")
        self.assertEqual(found["configuration"]["namedSettings"], [],
                         "the fixture registered something in the hook file, so this is not the"
                         " host the case is about")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.SETTINGS_UNUSABLE), firing.ESTABLISHED,
                         "the repair the payload's own configuration cell names was left out of"
                         " the answer that exists to name repairs")
        self.assertIn(firing.SETTINGS_UNUSABLE,
                      [one["cause"] for one in cell.get("candidates") or []],
                      "an established cause that the answer does not carry is a repair the"
                      " operator never sees")

    def test_a_user_owned_host_that_registers_nothing_gains_no_settings_cause(self):
        """SUPPORT, not evidence of the defect: this passes before the change too. It holds the
        direction the change must not break. Where the hook file IS where the registration
        lives and nothing is registered there, a missing settings file is not a cause of
        anything -- registering the hook writes it -- and the settled reading must not become a
        second repair beside not_registered.

        The fixture keeps the settings file, because that is what establishes the premise. An
        ABSENT settings file cannot say who owns the registration, and the case below is the
        one that pins that; deleting it here would have been asserting this claim on a host
        that cannot support it.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary)
            found = completion.status(codex_home=temporary, environ={})
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(cell.get("value"), firing.NOT_REGISTERED,
                         "a settings cause was invented beside the one repair this host needs")
        self.assertEqual(standings.get(firing.SETTINGS_ABSENT), firing.NOT_EVALUATED,
                         "the settled reading answered a question this host does not have")

    def test_a_settings_file_that_is_not_there_names_no_owner(self):
        """File absence establishes no owner, and this answer used to read it as one.

        A plugin-owned installation whose settings file was deleted while its package remains
        installed looks exactly like a host where nothing was ever installed. Reading that
        absence as the user owner established not_registered for it, which suppressed the
        plugin-side settings and launcher diagnoses and pointed recovery at the wrong
        registration. One observation, two explanations, and no reading here separates them.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary)
            completion.configuration_path(Path(temporary)).unlink()
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], completion.CONFIG_ABSENT,
                         "the fixture did not build the missing-settings host this case is"
                         " about")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.NOT_REGISTERED), firing.NOT_RULED_OUT,
                         "a settings file that is not there was read as saying the hook file"
                         " is where this host's registration lives")
        self.assertEqual(cell.get("value"), firing.CAUSE_UNREADABLE,
                         "two explanations and no reading between them is an unsettled cause,"
                         " not a chosen one")

    def test_a_peer_sharing_a_journal_read_empty_does_not_reopen_the_count(self):
        """A peer sharing a journal already read and found EMPTY cannot change that reading.
        If the host can start it, it writes into the very directory this command listed; if it
        cannot, it is out of the journal question entirely. Counted as uncertainty anyway, the
        answer reported an unsettled count for a directory it had just listed, and hid an
        established nothing_recorded behind a peer that shares its reading.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            second_registration(temporary, "journal")  # the SAME journal as the first
            path = Path(temporary) / "hooks.json"
            document = json.loads(path.read_text(encoding="utf-8"))
            entries = document["hooks"][completion.EVENT][0]["hooks"]
            # The second registration's interpreter is relative, so it is unjudged.
            entries[1]["command"] = "./python " + entries[1]["command"].split(" ", 1)[1]
            path.write_text(json.dumps(document), encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        named = found["configuration"]["namedSettings"]
        self.assertEqual(len({entry.get("journalRoot") for entry in named}), 1,
                         "the fixture did not build the one-journal host this case is about")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.NOTHING_RECORDED), firing.ESTABLISHED,
                         "the journal was read and holds nothing, and a peer sharing that very"
                         " directory left the count unsettled")

    def test_a_settled_settings_repair_names_what_it_did_not_establish(self):
        """The settings really are rejected. What is NOT established is that anything reads
        them: the registration they record lives in a manifest this command does not open, and
        whether such a package is installed at all is not read either. A repair presented as
        the settled cause of an absence is a claim about the absence too.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(ENTRY_POINT), mode="not-a-mode-this-reader-knows")
            cell = why_no_record(temporary)
        detail = next((one["detail"] for one in cell.get("candidates") or []
                       if one["cause"] == firing.SETTINGS_UNUSABLE), None)
        self.assertIsNotNone(detail,
                             "the fixture did not put the settings repair on the table, so"
                             " there is nothing here to qualify")
        self.assertIn("not established", detail,
                      "the repair was presented without the registration it never read: "
                      + repr(detail))

    def test_a_plugin_owned_host_that_has_recorded_has_no_absence_to_explain(self):
        """A record outranks every cause that claims nothing ran.

        A plugin-owned host leaves the hook file empty by design, so no registration here names
        a journal and the count lands only in the settled one. Read as evidence about an absent
        USER registration, it left not_registered unsettled -- which blocked every rule below
        it, including the one that says there is no absence to explain. The payload then
        reported cause_unreadable beside its own count of an invocation that happened.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(ENTRY_POINT))
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], "1",
                         "the fixture did not record the invocation this case is about")
        self.assertEqual(found["registrationOwner"]["value"], completion.OWNER_PLUGIN,
                         "the fixture did not build the plugin-owned host this case is about")
        self.assertEqual(found["configuration"]["namedSettings"], [],
                         "the fixture registered something in the hook file, so the count would"
                         " not have reached the settled journal at all")
        cell = found["firingRecordAbsence"]
        self.assertEqual(cell.get("value"), firing.RECORDS_FOUND,
                         "a conclusive record stood beside an answer that could not settle"
                         " whether anything had run")
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.NOT_REGISTERED), firing.RULED_OUT,
                         "a record this hook wrote is proof something invoked it")
        self.assertEqual(standings.get(firing.ADAPTER_CANNOT_RUN), firing.RULED_OUT,
                         "a record outranks a probe that was never taken")

    def test_an_old_record_does_not_answer_whether_the_launcher_starts_now(self):
        """A journal entry says the host started the adapter ONCE. Whether it can start NOW is
        a different question, and ruling that one out from an old record hid a live repair: a
        plugin-owned installation whose recorded entry point was deleted after it last recorded
        reported records_found while the payload's own adapterEntryPoint cell read ABSENT.
        The launcher those settings record is probed instead.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            adapter = Path(temporary) / "the-recorded-adapter-entry-point.py"
            adapter.write_text("", encoding="utf-8")
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable, adapterEntryPoint=str(adapter))
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            intact = completion.status(codex_home=temporary,
                                       environ={})["firingRecordAbsence"].get("value")
            # Deleted AFTER the invocation it recorded, which is the whole point.
            adapter.unlink()
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(intact, firing.RECORDS_FOUND,
                         "the fixture did not reach records_found while the launcher was"
                         " there, so it is not showing what deleting it changes")
        self.assertEqual(found["firingJournal"]["value"], "1",
                         "the record this case is about is gone from the fixture")
        self.assertEqual(found["adapterEntryPoint"]["value"], reading.ABSENT,
                         "the fixture did not delete the recorded launcher")
        cell = found["firingRecordAbsence"]
        self.assertEqual(cell.get("value"), firing.ADAPTER_CANNOT_RUN,
                         "an old record answered a question about what is there now, and the"
                         " launcher repair went unnamed beside a cell that reads ABSENT")

    def test_a_deleted_plugin_launcher_is_named_without_an_old_record(self):
        """The same repair, and no record to expose it.

        Whether the launcher starts is a present reading, and it was reachable only when an old
        record happened to exist to rule not_registered out. With an empty journal the
        prerequisite held the probe unasked, so the answer carried "maybe it is not registered"
        while adapterEntryPoint read ABSENT beside it -- the same repair, hidden by whether the
        host had ever fired.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            adapter = Path(temporary) / "the-recorded-adapter-entry-point.py"
            adapter.write_text("", encoding="utf-8")
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable, adapterEntryPoint=str(adapter))
            adapter.unlink()
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], reading.ABSENT,
                         "the fixture recorded something, so this is the case the record"
                         " already covers rather than the one it hides")
        self.assertEqual(found["adapterEntryPoint"]["value"], reading.ABSENT,
                         "the fixture did not delete the recorded launcher")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.ADAPTER_CANNOT_RUN), firing.ESTABLISHED,
                         "a launcher this command read as ABSENT was left unasked because"
                         " nobody could rule out a registration it never reads")

    def test_a_recorded_launcher_is_probed_beside_a_hook_file_registration(self):
        """Both commands run on a host that has both. Supplying the recorded launcher only
        when the hook file named nothing let the other registration's healthy probe answer for
        a plugin launcher that is gone."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            adapter = Path(temporary) / "the-recorded-adapter-entry-point.py"
            adapter.write_text("", encoding="utf-8")
            amend_settings(temporary, owner=completion.OWNER_PLUGIN,
                           adapterInterpreter=sys.executable, adapterEntryPoint=str(adapter))
            adapter.unlink()
            found = completion.status(codex_home=temporary, environ={})
        self.assertTrue(found["configuration"]["namedSettings"],
                        "the fixture left no hook-file registration, so nothing here could"
                        " have hidden the launcher")
        cell = found["firingRecordAbsence"]
        standings = {one["cause"]: one["standing"]
                     for group in ("candidates", "ruledOut", "notEvaluated")
                     for one in (cell.get(group) or [])}
        self.assertEqual(standings.get(firing.ADAPTER_CANNOT_RUN), firing.ESTABLISHED,
                         "a healthy hook-file registration answered for a recorded launcher"
                         " that is not there")

    def test_an_open_that_yielded_no_descriptor_publishes_no_identity(self):
        """An open that failed established nothing about WHICH directory refused it. Taking the
        identity from the spelling afterwards answered about whatever it named by then, so a
        link retargeted in between published the refusal under a readable directory's identity
        -- and the next registration naming that directory inherited a failure belonging to
        something else instead of listing it.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            _first, second = second_registration(temporary, "journal-two")
            refuses = Path(temporary) / "a-regular-file-that-refuses-the-open"
            refuses.write_text("", encoding="utf-8")
            readable = Path(temporary) / "a-readable-journal"
            readable.mkdir()
            alias = Path(temporary) / "a-link-retargeted-after-the-failed-open"
            alias.symlink_to(refuses)
            amend_settings(temporary, journalRoot=str(alias))
            document = json.loads(second.read_text(encoding="utf-8"))
            document["journalRoot"] = str(readable)
            second.write_text(json.dumps(document), encoding="utf-8")

            real_open = completion.os.open

            def opening(path, *args, **kwargs):
                try:
                    return real_open(path, *args, **kwargs)
                except OSError:
                    # Exactly the window: the open has failed and nothing has looked the
                    # spelling up yet.
                    if str(path) == str(alias):
                        alias.unlink()
                        alias.symlink_to(readable)
                    raise

            with mock.patch.object(completion.os, "open", side_effect=opening):
                found = completion.status(codex_home=temporary, environ={})
            self.assertTrue(os.path.samefile(str(alias), str(readable)),
                            "the fixture did not retarget the link after the failed open")
        named = found["configuration"]["namedSettings"]
        self.assertEqual(named[0]["recordsAnswer"], firing.UNESTABLISHED,
                         "the fixture did not make the first open fail")
        self.assertEqual(named[1].get("journalRoot"), str(readable),
                         "a readable journal inherited a refusal that came from somewhere"
                         " else, because the failed open published an identity taken after it")
        self.assertEqual(named[1]["recordsAnswer"], firing.COUNTED,
                         "the second registration's own directory was never listed")

    def test_records_already_written_survive_the_registration_being_removed(self):
        """An empty hook file establishes the PRESENT. A registration removed after the hook
        had fired leaves its journal exactly where it was, and this answer used to say no
        record of an invocation could exist beside a count, in the same payload, saying one
        does."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary, environ={})
            (Path(temporary) / "hooks.json").unlink()
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["firingJournal"]["value"], "1",
                         "the fixture did not leave the record this case is about")
        cell = found["firingRecordAbsence"]
        self.assertEqual(cell.get("value"), firing.NOT_REGISTERED,
                         "the registration really is gone, so this stays the cause")
        self.assertIn("1 record(s)", cell.get("evidence") or "",
                      "the answer explained an absence the payload's own count refutes,"
                      " without ever naming the records it was reading past")


class OneFileAndOneRecordAnswerForThemselves(unittest.TestCase):
    """Review on PR #52 head e8863b2, raised by the reviewer on that head and by an independent
    audit of it at the same time.

    Two answers this branch owns were still reading past evidence the payload already held: a
    journal holding a record, and one settings file named twice.
    """

    def _document(self, root):
        return {"configVersion": 1, "relayExecutable": "/bin/true", "markerRoot": "/tmp/marker",
                "mode": completion.OBSERVE, "journalRoot": str(root)}

    def test_a_record_answers_for_a_hook_file_this_command_cannot_read(self):
        """A host holding a record is not showing an absence, whatever the hook file says.

        The rule was already written one branch below, for a plugin-owned host whose hook file
        WAS read and registers nothing: a record this hook wrote proves something invoked the
        adapter. The unreadable half of the same class had nobody asking it, so
        not_registered stayed unsettled, blocked every rule that requires it, and the payload
        reported cause_unreadable for an absence its own count refutes.
        """
        found = firing.decide({"registrationReadable": False, "adapterRegistrations": 0,
                               "registrationReadHere": True, "unregisteredRecords": 1,
                               "namedJournals": [], "namedSettings": []})
        self.assertEqual(found["value"], firing.RECORDS_FOUND,
                         "an absence cause was reported for a host whose journal holds a"
                         " record this hook wrote")

    def test_a_hook_file_nobody_could_read_is_still_unsettled_without_a_record(self):
        """SUPPORT, not evidence. The direction the fix must not break: with no record, an
        unreadable hook file establishes nothing and the cause stays unsettled rather than
        becoming the terminal answer."""
        found = firing.decide({"registrationReadable": False, "adapterRegistrations": 0,
                               "registrationReadHere": True, "unregisteredRecords": 0,
                               "namedJournals": [], "namedSettings": []})
        self.assertEqual(found["value"], firing.CAUSE_UNREADABLE)

    def test_one_settings_file_named_twice_is_read_once(self):
        """Two spellings of ONE settings file are one file, and one file has one answer.

        _settled removes lexical differences and deliberately does not resolve a symlink, so
        two absolute aliases of one file missed the cache and were read separately. The
        installer rewrites settings atomically, so a rewrite landing between those two reads
        reported the old journalRoot in one entry and the new one in another -- about one file,
        in one answer -- and recorded_on_another_path read that as two journals disagreeing on
        a host that has one.
        """
        reads = []
        real = completion.read_configuration
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            named = temporary / "settings.json"
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            named.write_text(json.dumps(self._document(first)), encoding="utf-8")
            alias = temporary / "an-alias-of-the-settings.json"
            alias.symlink_to(named)

            def rewritten_between_the_reads(path, *passed, **keywords):
                reads.append(str(path))
                if len(reads) == 2:
                    replacement = temporary / "settings.written"
                    replacement.write_text(json.dumps(self._document(second)),
                                           encoding="utf-8")
                    os.replace(replacement, named)
                return real(path, *passed, **keywords)

            with mock.patch.object(completion, "read_configuration",
                                   side_effect=rewritten_between_the_reads):
                found = completion.journals_named([
                    {"registration": "through-its-own-name", "settings": str(named),
                     "startable": True},
                    {"registration": "through-an-alias", "settings": str(alias),
                     "startable": True}])

        self.assertEqual(len({entry["journalRoot"] for entry in found}), 1,
                         "one settings file was read twice and the two readings disagreed"
                         " about which journal it names")

    def test_two_different_settings_files_keep_their_own_readings(self):
        """SUPPORT, not evidence. The negative control for the case above: sharing a reading is
        keyed on what the kernel says the file is, so two genuinely different files are still
        read separately and keep their own journals."""
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first_settings = temporary / "one.json"
            second_settings = temporary / "two.json"
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            first_settings.write_text(json.dumps(self._document(first)), encoding="utf-8")
            second_settings.write_text(json.dumps(self._document(second)), encoding="utf-8")
            found = completion.journals_named([
                {"registration": "one", "settings": str(first_settings), "startable": True},
                {"registration": "two", "settings": str(second_settings), "startable": True}])
        self.assertEqual([entry["journalRoot"] for entry in found], [str(first), str(second)])


class AnIdentityIsOnlyAKeyWhileItIsHeld(unittest.TestCase):
    """Review and audit of PR #52 head 000b83f, which is where the settings cache landed.

    Keying a cache on what the kernel calls a file closed one hole and opened two smaller ones,
    both in the reader this branch changed to answer that question.
    """

    def _document(self, root):
        return {"configVersion": 1, "relayExecutable": "/bin/true", "markerRoot": "/tmp/marker",
                "mode": completion.OBSERVE, "journalRoot": str(root)}

    def test_a_deleted_file_does_not_lend_its_identity_to_the_next_one(self):
        """An inode is recycled as soon as its last name and its last descriptor are gone.

        A settings file read and then deleted hands its (device, inode) to whatever is created
        next, so a cache keyed on that pair served the deleted file's reading for an unrelated
        file -- the exact over-merge the alias fix exists to avoid, arriving through time
        instead of through a symlink. The reading is cached only while a descriptor holds the
        object it names.

        Two regimes, and the case reports which one it met. Once the reading holds its own
        descriptor the inode CANNOT be recycled, so the collision stops being constructible at
        all -- the strongest form of the guarantee, and why a fixed head records that it was
        not reused. Without the hold it is constructible, and the assertion catches it. Where a
        filesystem never recycles an inode this establishes nothing either way, and it says so
        rather than reporting a host it never built.
        """
        order = []
        real = completion.read_configuration
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            gone = temporary / "read-then-deleted.json"
            arrives = temporary / "created-afterwards.json"
            gone.write_text(json.dumps(self._document(first)), encoding="utf-8")
            vacated = gone.stat().st_ino

            def deleting_the_first_after_reading_it(path, *passed, **keywords):
                order.append(str(path))
                answer = real(path, *passed, **keywords)
                if len(order) == 1:
                    os.unlink(gone)
                    arrives.write_text(json.dumps(self._document(second)), encoding="utf-8")
                return answer

            with mock.patch.object(completion, "read_configuration",
                                   side_effect=deleting_the_first_after_reading_it):
                found = completion.journals_named([
                    {"registration": "read-then-deleted", "settings": str(gone),
                     "startable": True},
                    {"registration": "created-afterwards", "settings": str(arrives),
                     "startable": True}])
            recycled = arrives.stat().st_ino == vacated

        self.assertEqual(found[1]["journalRoot"], str(second),
                         "a file created after another was deleted was served the deleted"
                         " file's reading, because it inherited its inode"
                         + ("" if recycled else " (note: the inode was NOT reused on this run,"
                                                " so the collision was never built)"))

    def test_one_record_reads_the_same_however_its_lines_end(self):
        """The shared reader decodes as TEXT, and that is not cosmetic.

        read_json was changed to open the file itself so it could take the identity from its
        own descriptor, and reading the bytes raw dropped the universal-newline translation
        Path.read_text had been doing. A record separated by CR or CRLF then reached the parser
        at a different offset, so the line and column a malformed one reports -- which is what
        an operator reads to find it -- moved with the file's line endings.
        """
        details = {}
        for name, ending in (("lf", b"\n"), ("cr", b"\r"), ("crlf", b"\r\n")):
            with tempfile.TemporaryDirectory() as temporary:
                path = Path(temporary) / "record.json"
                path.write_bytes(b'{"configVersion": 1,' + ending + b'  bad}')
                details[name] = reading.read_json(path, "a record").detail
        self.assertEqual(len(set(details.values())), 1,
                         "one malformed record reported a different position for each line"
                         " ending: " + repr(details))

    def test_the_callers_own_reading_is_held_before_it_is_a_key(self):
        """The one entry that skipped the rule every other entry follows.

        status() hands journals_named the reading it already took, and that entry went straight
        into the cache under its identity. The descriptor it was read through was closed before
        the call began, so that identity is exactly this recyclable too: delete the file and
        the next one created inherits it, and a later registration naming the new file is
        served the old file's configuration.
        """
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            gone = temporary / "read-by-the-caller.json"
            arrives = temporary / "named-by-a-registration.json"
            gone.write_text(json.dumps(self._document(first)), encoding="utf-8")
            carried = completion.read_configuration(gone)
            vacated = gone.stat().st_ino
            os.unlink(gone)
            arrives.write_text(json.dumps(self._document(second)), encoding="utf-8")
            recycled = arrives.stat().st_ino == vacated
            found = completion.journals_named(
                [{"registration": "named-by-a-registration", "settings": str(arrives),
                  "startable": True}],
                already_read={str(gone): carried})
        self.assertEqual(found[0]["journalRoot"], str(second),
                         "a registration's own settings file was served the caller's reading"
                         " of a deleted file that had held its inode"
                         + ("" if recycled else " (note: the inode was NOT reused on this run,"
                                                " so the collision was never built)"))

    def test_the_descriptor_that_read_the_bytes_is_the_one_held(self):
        """Reopening the path to pin it verifies the wrong thing.

        The cache held its object by opening the path a second time and comparing identities.
        That is a second lookup of the same spelling: replace the file between the read and the
        reopen with one that inherits the inode, and the identities compare EQUAL while the
        pin is on the new object and the cached bytes are the old one's. A later spelling
        reaching that inode is then served a reading that never came from it -- a wrong reading
        wearing a verified identity, which is worse than an unverified one.

        Holding the descriptor the bytes were read through removes the interval, and with it
        the ability to build this at all: the original object cannot be recycled while it is
        open, so the replacement cannot inherit its inode.
        """
        real = completion.read_configuration
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            named = temporary / "settings.json"
            named.write_text(json.dumps(self._document(first)), encoding="utf-8")
            alias = temporary / "an-alias-of-whatever-is-there-now.json"
            swapped = {}

            def replaced_after_the_read(path, *passed, **keywords):
                answer = real(path, *passed, **keywords)
                if not swapped:
                    was = named.stat().st_ino
                    os.unlink(named)
                    named.write_text(json.dumps(self._document(second)), encoding="utf-8")
                    swapped["inherited"] = named.stat().st_ino == was
                    alias.symlink_to(named)
                return answer

            with mock.patch.object(completion, "read_configuration",
                                   side_effect=replaced_after_the_read):
                found = completion.journals_named([
                    {"registration": "read-before-the-swap", "settings": str(named),
                     "startable": True},
                    {"registration": "naming-what-is-there-now", "settings": str(alias),
                     "startable": True}])

        self.assertEqual(found[1]["journalRoot"], str(second),
                         "a spelling was served a reading taken from an object that had been"
                         " replaced, because the pin was verified by reopening the path"
                         + ("" if swapped.get("inherited") else " (note: the replacement did"
                                                                " NOT inherit the inode on"
                                                                " this run, so the collision"
                                                                " was never built)"))

    def test_collapsing_sources_holds_each_identity_it_compares(self):
        """SUPPORT, not evidence: a contract pin on the sibling site, deliberately not counted.

        The defect is one class at two sites -- an identity compared without being held -- and
        the case above is its evidence, red at the parent as an AssertionError. This is the
        other site. It cannot be demonstrated red at the parent on the defect itself: the
        parent has no function here to call, the collapse is inline in status(), and reaching
        it would mean patching the very lookup the fix stops using, so the case would fail at
        the parent with an AttributeError and pass at the head for the wrong reason. Pinned as
        a contract instead, and said plainly rather than counted.

        What it pins: merging on a released identity reports ONE source where there are two.
        A file deleted after it was identified hands its inode to a later, unrelated settings
        file, which is then collapsed into it and never read -- the opposite error from the
        aliasing this collapse fixes, and the worse one.
        """
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            gone = temporary / "identified-then-deleted.json"
            gone.write_text("{}", encoding="utf-8")
            vacated = gone.stat().st_ino
            os.unlink(gone)
            arrives = temporary / "an-unrelated-file.json"
            arrives.write_text("{}", encoding="utf-8")
            recycled = arrives.stat().st_ino == vacated
            kept, _judged = completion._one_source_each([str(gone), str(arrives)])
        self.assertIn(str(arrives), kept,
                      "a settings file was collapsed into a deleted one whose inode it"
                      " inherited, so it was never read as its own source"
                      + ("" if recycled else " (note: the inode was NOT reused on this run, so"
                                             " the collision was never built)"))

    def test_a_settings_path_the_kernel_is_never_asked_about_is_a_reading(self):
        """A registration can carry a spelling no syscall will accept.

        An embedded NUL makes os.open raise ValueError rather than OSError, and the collapse
        only caught OSError -- so one malformed registration ended the whole status payload in
        a traceback. A path this command cannot identify is a reading like any other, and it
        keeps its own place.
        """
        nul = "/a-path-with-a\x00-nul.json"
        try:
            kept, judged = completion._one_source_each([nul, "/an-ordinary-path.json"])
        except ValueError as raised:
            self.fail("a spelling the kernel is never asked about ended the read in a"
                      " traceback instead of answering: " + repr(raised))
        self.assertIn(nul, kept,
                      "a spelling the kernel is never asked about was dropped instead of"
                      " keeping its own place")
        self.assertNotIn(nul, judged,
                         "a spelling nothing could identify was recorded as judged")

    def test_a_source_read_after_it_changed_is_not_the_one_that_was_merged(self):
        """Collapsing two spellings and then reading one are two lookups.

        The merge is judged on descriptors the collapse holds and then releases; the read that
        follows opens the retained spelling again. Retarget that symlink in between and the
        payload describes a file the merge was never about, while both registrations still run
        -- one source claimed on a judgement that no longer holds. The reading says it did not
        settle instead.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = Path(temporary)
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            register(temporary)
            named = completion.configuration_path(host)
            alias = host / "an-alias-of-the-settings.json"
            alias.symlink_to(named)
            hook_file = host / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = dict(written["hooks"][completion.EVENT][0]["hooks"][0])
            entry["command"] = entry["command"].replace(str(named), str(alias))
            written["hooks"][completion.EVENT][0]["hooks"].append(entry)
            hook_file.write_text(json.dumps(written), encoding="utf-8")

            elsewhere = host / "a-different-settings-file.json"
            elsewhere.write_text(named.read_text(encoding="utf-8"), encoding="utf-8")
            real = completion._one_source_each

            def retargeted_after_the_merge(spellings, *passed, **keywords):
                answer = real(spellings, *passed, **keywords)
                moved = host / "moved-settings.json"
                named.rename(moved)
                named.symlink_to(elsewhere)
                return answer

            with mock.patch.object(completion, "_one_source_each",
                                   side_effect=retargeted_after_the_merge):
                found = completion.status(codex_home=temporary, environ={})

        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_AMBIGUOUS,
                         "two registrations were reported as one source on a judgement about a"
                         " file that is no longer the one read")

    def test_a_discarded_spelling_that_moved_unsettles_the_merge_too(self):
        """The other half of the same broken judgement.

        The merge collapses two spellings into one source. Checking only the spelling that was
        RETAINED caught the case where that one moved, and missed the case where the discarded
        alias moved instead -- the same finding no longer holding, seen from the other side,
        and the same two registrations reported as one source because of it.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = Path(temporary)
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            register(temporary)
            named = completion.configuration_path(host)
            alias = host / "z-an-alias-of-the-settings.json"
            alias.symlink_to(named)
            hook_file = host / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = dict(written["hooks"][completion.EVENT][0]["hooks"][0])
            entry["command"] = entry["command"].replace(str(named), str(alias))
            written["hooks"][completion.EVENT][0]["hooks"].append(entry)
            hook_file.write_text(json.dumps(written), encoding="utf-8")

            elsewhere = host / "a-different-settings-file.json"
            elsewhere.write_text(named.read_text(encoding="utf-8"), encoding="utf-8")
            real = completion._one_source_each

            def the_discarded_alias_moves(spellings, *passed, **keywords):
                answer = real(spellings, *passed, **keywords)
                # The DISCARDED spelling is retargeted; the retained one is untouched.
                alias.unlink()
                alias.symlink_to(elsewhere)
                return answer

            with mock.patch.object(completion, "_one_source_each",
                                   side_effect=the_discarded_alias_moves):
                found = completion.status(codex_home=temporary, environ={})

        self.assertEqual(found["configuration"]["value"], completion.REGISTRATION_AMBIGUOUS,
                         "only the retained spelling was revalidated, so a merge whose"
                         " discarded side had moved was still reported as one source")

    def test_a_record_that_will_not_parse_still_says_what_it_came_from(self):
        """An unreadable file was the one reading nothing could key on.

        The holder and the identity were taken after the parse, so a file that fails to parse
        returned neither -- and two registrations naming that file through aliases could read
        it twice and disagree about it, which is the very split the identity work closes for
        every other state.
        """
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "not-json.json"
            path.write_text("{ this will not parse", encoding="utf-8")
            found = reading.read_json(path, "a record", hold=True)
            identity, holder = found.identity, found.holder
            reading.release(found)
        self.assertFalse(found.usable, "the fixture did not build an unreadable record")
        self.assertIsNotNone(identity,
                             "a record that failed to parse reported no identity, so nothing"
                             " could tell two spellings of it apart from two files")
        self.assertIsNotNone(holder,
                             "a record that failed to parse held nothing, so its identity was"
                             " not usable as a key")

    def test_a_journal_identity_is_held_while_it_is_an_alias_key(self):
        """The same rule at the journal site, which had it for the reading and not for the key.

        _journal_cell takes its identity from the descriptor it listed -- which is right -- and
        then closed it. Published as an alias key afterwards, that identity is recyclable: a
        journal directory deleted after it was listed hands its (device, inode) to whatever is
        created next, and a later registration's journal reaching that pair was handed this
        one's snapshot, count and all.
        """
        real = completion._journal_cell
        listed = []
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first = temporary / "journal-one"
            second = temporary / "journal-two"
            first.mkdir()
            (first / "2026-01-01").mkdir()
            (first / "2026-01-01" / "0001-a.json").write_text("{}", encoding="utf-8")
            settings_one = temporary / "one.json"
            settings_two = temporary / "two.json"
            settings_one.write_text(json.dumps(self._document(first)), encoding="utf-8")
            settings_two.write_text(json.dumps(self._document(second)), encoding="utf-8")
            vacated = {}

            def deleting_the_first_after_listing_it(config, *passed, **keywords):
                answer = real(config, *passed, **keywords)
                if not listed:
                    listed.append(True)
                    was = first.stat().st_ino
                    shutil.rmtree(first)
                    second.mkdir()
                    vacated["inherited"] = second.stat().st_ino == was
                return answer

            with mock.patch.object(completion, "_journal_cell",
                                   side_effect=deleting_the_first_after_listing_it):
                found = completion.journals_named([
                    {"registration": "one", "settings": str(settings_one), "startable": True},
                    {"registration": "two", "settings": str(settings_two), "startable": True}])

        self.assertEqual(found[1]["journalRoot"], str(second),
                         "a registration's own journal was given a snapshot taken from a"
                         " deleted directory whose inode it inherited"
                         + ("" if vacated.get("inherited") else " (note: the inode was NOT"
                                                                " reused on this run, so the"
                                                                " collision was never built)"))

    def test_a_rewrite_between_the_two_reads_does_not_split_one_file(self):
        """The rewrite lands BETWEEN the reads, which is the thing that broke.

        Aliases were discovered one spelling at a time: the first spelling was read, and only
        then was the second one statted. An atomic rewrite in that gap left the held OLD inode
        in the cache while the alias now resolved to the replacement, so the lookup missed and
        two registrations naming what is by then one file received two different journalRoots
        -- which recorded_on_another_path reads as two sources disagreeing.

        Every spelling is pinned before any of them is read now, and each reading is taken
        through the descriptor that pinned it, so a rewrite landing in the gap cannot move
        either registration onto a different object.
        """
        reads = []
        real = completion.read_configuration
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            named = temporary / "settings.json"
            named.write_text(json.dumps(self._document(first)), encoding="utf-8")
            alias = temporary / "an-alias-of-the-settings.json"
            alias.symlink_to(named)

            def rewritten_between_the_two_reads(path, *passed, **keywords):
                answer = real(path, *passed, **keywords)
                if len(reads) == 0:
                    reads.append(str(path))
                    # Atomic replacement: a NEW inode arrives at the same pathname, in the gap
                    # between the first reading and the next spelling's lookup.
                    replacement = temporary / "settings.replacement"
                    replacement.write_text(json.dumps(self._document(second)),
                                           encoding="utf-8")
                    os.replace(replacement, named)
                else:
                    reads.append(str(path))
                return answer

            with mock.patch.object(completion, "read_configuration",
                                   side_effect=rewritten_between_the_two_reads):
                found = completion.journals_named([
                    {"registration": "by-its-own-name", "settings": str(named),
                     "startable": True},
                    {"registration": "by-an-alias", "settings": str(alias),
                     "startable": True}])

        self.assertEqual(len({entry["journalRoot"] for entry in found}), 1,
                         "a rewrite landing between the two reads split one settings file into"
                         " two journals, which reads as two sources disagreeing")

    def test_a_replacement_between_the_snapshot_and_the_pins_is_not_two_sources(self):
        """The window between status()'s own reading and the pinning that follows it.

        status() reads the settings it settled on, revalidates the merge, and only then does
        journals_named pin the registration spellings. An atomic replacement landing in
        BETWEEN leaves the carried reading holding the old object while the pins hold the new
        one -- and the exact-path lookup took the carried value without ever comparing it to
        the pin. Two registrations aliasing one file then received the old journalRoot and the
        new one, though there was no instant at which they named different files, and
        recorded_on_another_path can establish off that: an absence given the wrong cause,
        which is worse than one given none.

        The replacement is performed at the entry to journals_named, which is exactly that
        window. Anywhere else does not test this.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = Path(temporary)
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            register(temporary)
            named = completion.configuration_path(host)
            alias = host / "an-alias-of-the-settings.json"
            alias.symlink_to(named)
            hook_file = host / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = dict(written["hooks"][completion.EVENT][0]["hooks"][0])
            entry["command"] = entry["command"].replace(str(named), str(alias))
            written["hooks"][completion.EVENT][0]["hooks"].append(entry)
            hook_file.write_text(json.dumps(written), encoding="utf-8")

            elsewhere = host / "journal-somewhere-else"
            elsewhere.mkdir()
            real = completion.journals_named

            def replaced_before_the_pins(*passed, **keywords):
                document = json.loads(named.read_text(encoding="utf-8"))
                document["journalRoot"] = str(elsewhere)
                replacement = host / "settings.replacement"
                replacement.write_text(json.dumps(document), encoding="utf-8")
                os.replace(replacement, named)
                return real(*passed, **keywords)

            with mock.patch.object(completion, "journals_named",
                                   side_effect=replaced_before_the_pins):
                found = completion.status(codex_home=temporary, environ={})

        roots = {one["journalRoot"] for one in found["configuration"]["namedSettings"]}
        self.assertEqual(len(roots), 1,
                         "two registrations aliasing ONE settings file were given different"
                         " journals, so a diagnosis can send the operator to a path that was"
                         " never another path: " + repr(sorted(roots)))

    def test_a_pinned_spelling_that_is_unlinked_is_still_read(self):
        """Asking the PATH again undoes the reason the descriptor was pinned.

        Every spelling is opened and held before any of them is read, and then the read asked
        the pathname whether anything was there. Unlink one of two aliases in between and that
        registration reported its settings ABSENT -- and settings_absent established a repair
        for it -- while the object was held open the whole time and its peer read it perfectly.
        One file, two answers, from a lookup the pin exists to make unnecessary.
        """
        removed = []
        real = completion.read_configuration
        with tempfile.TemporaryDirectory() as temporary:
            temporary = Path(temporary)
            first, second = temporary / "journal-one", temporary / "journal-two"
            first.mkdir()
            second.mkdir()
            one = temporary / "a-settings.json"
            two = temporary / "b-settings.json"
            one.write_text(json.dumps(self._document(first)), encoding="utf-8")
            two.write_text(json.dumps(self._document(second)), encoding="utf-8")

            def unlinking_the_second_after_the_first_read(path, *passed, **keywords):
                answer = real(path, *passed, **keywords)
                if not removed:
                    removed.append(str(path))
                    # The second registration's SPELLING goes away between its pin and its
                    # read. The object stays, held by the pin.
                    two.unlink()
                return answer

            with mock.patch.object(completion, "read_configuration",
                                   side_effect=unlinking_the_second_after_the_first_read):
                found = completion.journals_named([
                    {"registration": "read-first", "settings": str(one), "startable": True},
                    {"registration": "unlinked-before-its-read", "settings": str(two),
                     "startable": True}])

        self.assertEqual([entry["settingsState"] for entry in found],
                         [reading.PRESENT, reading.PRESENT],
                         "a spelling unlinked after it was pinned reported its settings absent,"
                         " although the object was held open and read perfectly")


class AnAnswerableCauseIsNotWithheld(unittest.TestCase):
    """Review of PR #52 head 000b83f. Two more answers this branch owns were being withheld
    behind a different question, on the host whose repair they name."""

    def _host(self, temporary):
        fake_relay(temporary, stdout=json.dumps(RELEASED))
        return Path(temporary)

    def _standings(self, cell):
        return {one["cause"]: one["standing"]
                for group in ("candidates", "ruledOut", "notEvaluated")
                for one in (cell.get(group) or [])}

    def test_a_plugin_owned_host_names_the_journal_policy_it_states(self):
        """A policy read from a file this command DID read is an answerable cause.

        journalling_off required not_registered to be ruled out, and a plugin-owned host cannot
        rule it out: the registration lives in a package manifest this command does not open.
        So the payload published journalPolicy no_journal in one cell and, in the cell that
        exists to explain the absence, reported only that the registration was unsettled --
        withholding a repair it could read because a different question was open. The settings
        causes and adapter_cannot_run were freed from that requirement for this exact host; the
        two policy causes were the rest of the same class.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(ENTRY_POINT),
                     journalPolicy=completion.NO_JOURNAL)
            cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.JOURNALLING_OFF), firing.ESTABLISHED,
                         "the host states it keeps no journal and the answer did not say so")

    def test_a_journal_path_that_leads_nowhere_counts_nothing(self):
        """A dangling link is not an empty journal.

        Opening the directory raises FileNotFoundError either way, and answering ABSENT for
        both settled a count of zero for a host whose journal PATH is broken. Nothing could be
        read through the link, and the hook cannot create its dated directory through it
        either, so "this hook has recorded no invocation" claims something no reading here
        established -- an unreadable path answered as an empty journal.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            nowhere = host / "a-journal-link-to-nothing"
            nowhere.symlink_to(host / "a-target-that-is-not-there")
            settings(temporary, journalRoot=str(nowhere))
            found = completion.status(codex_home=temporary, environ={})
        self.assertNotEqual(found["firingJournal"]["value"], reading.ABSENT,
                            "a journal path whose link leads nowhere was reported as an"
                            " established absence, which is a count of zero")

    def test_rejected_settings_still_probe_the_launcher_they_record(self):
        """A document that was READ answers with every field that IS readable.

        Where plugin-owned settings fail an unrelated validation -- a mode this reader does not
        know -- the configuration cells become not_read, and taking that as the answer dropped
        the launcher probe entirely. The entry point and interpreter those settings record are
        present readings whatever the mode says, so a deleted entry point hid behind an
        unrelated complaint on exactly the host whose repair the probe exists to name.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            gone = host / "an-entry-point-that-was-deleted.py"
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint=str(gone),
                     mode="not-a-mode-this-reader-knows")
            cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.ESTABLISHED,
                         "the recorded entry point is gone and the answer never probed it,"
                         " because an unrelated field of the same document failed validation")

    def test_one_readable_launcher_half_is_probed_without_the_other(self):
        """Two independent readings, joined by an 'and' that hid one of them.

        The raw-field probe required BOTH the entry point and the interpreter to be usable
        absolute paths. Where the settings record a deleted entry point and no interpreter at
        all -- or a relative one -- the readable half was dropped with the unreadable one, so a
        launcher this host cannot start went unreported because of a fact about a different
        field. The cause reads the halves separately, and so does this.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            gone = host / "an-entry-point-that-was-deleted.py"
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterEntryPoint=str(gone),
                     adapterInterpreter="a-relative-interpreter",
                     mode="not-a-mode-this-reader-knows")
            cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.ESTABLISHED,
                         "a recorded entry point that is gone went unprobed because the"
                         " interpreter beside it was not an absolute path")

    def test_a_tilde_launcher_path_is_not_a_path_the_launcher_resolves(self):
        """Judged on the expanded spelling, read on the literal one.

        The packaged launcher checks os.path.isabs on the string as WRITTEN and declines
        silently otherwise -- plugins/crw/wiring/crw_stop_hook.py -- and complaints() rejects
        the same spelling for the same reason. Accepting it here because its expanded form is
        absolute answered 'startable' from a file that launcher never reaches, which is an
        undistinguished state presented as a settled one.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            # HOME is pointed at the fixture, so the file the expanded spelling reaches is
            # inside this temporary host and nothing outside it is touched.
            named = "an-entry-point-the-launcher-will-not-resolve.py"
            (host / named).write_text("", encoding="utf-8")
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter=sys.executable,
                     adapterEntryPoint="~/" + named,
                     mode="not-a-mode-this-reader-knows")
            with mock.patch.dict(os.environ, {"HOME": str(host)}):
                cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.NOT_RULED_OUT,
                         "a spelling this command never resolved was answered as a settled"
                         " startability: accepted because its EXPANDED form is absolute and"
                         " then read as the literal string, it establishes a repair for a"
                         " path the packaged launcher would have declined outright")

    def test_an_interpreter_that_is_not_one_is_not_startable(self):
        """A file being there establishes that the path is not empty, and nothing more.

        The interpreter probe checked existence and the executable bit and then reported
        PRESENT, which every rule downstream reads as "this registration can start". An
        interpreter replaced by a program that exits quietly -- /bin/true is the whole family,
        and it is the same family _offers_guard already refuses to accept for the relay -- read
        as a working hook. The answer comes from running it now, and says it is a moment.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            # REPLACED after installation, which is the host this is about: the installer
            # refuses an interpreter that is not one, so the only way to reach this state is
            # for the program at that path to change afterwards.
            hook_file = Path(temporary) / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(sys.executable, "/bin/true", 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.ESTABLISHED,
                         "a registered interpreter that is not an interpreter was read as"
                         " startable because a file exists at its path and is executable")

    def test_a_program_that_repeats_its_arguments_is_not_an_interpreter(self):
        """Echoing the question is not answering it.

        The probe looked for a marker IN the output, and a program that repeats its arguments
        prints the source back, marker and all. "The marker appeared" and "this ran Python" are
        different claims, and they differ exactly for the family this was supposed to exclude.
        The source asks for something computed from a nonce this call invents, and the exact
        reply is compared rather than searched for.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            hook_file = Path(temporary) / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(sys.executable, "/bin/echo", 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.ESTABLISHED,
                         "a program that echoed the question back was read as having answered"
                         " it, so a registration that cannot run read as startable")

    def test_an_adapter_script_that_cannot_be_read_is_not_startable(self):
        """One step down from the interpreter, and the same sentence.

        The interpreter OPENS this file to run it, so a regular file it cannot open is a file
        it cannot run. Presence answered PRESENT for a target with no read permission and the
        registration read as startable, while every Stop died before the adapter's first line.
        Readability is the whole of what is establishable about a script from here, and it is
        now established rather than assumed.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            # Same basename, because the registration is recognised by the adapter it names.
            unreadable = Path(temporary) / "unreadable"
            unreadable.mkdir()
            target = unreadable / ENTRY_POINT.name
            target.write_text("", encoding="utf-8")
            target.chmod(0)
            if os.access(str(target), os.R_OK):
                # Running as root, where mode 000 denies nothing. The host this case is about
                # cannot be built here, and saying so is the honest answer: asserting anyway
                # would fail for a reason that is not the defect.
                self.skipTest("this user can read a file with mode 000, so the unreadable"
                              " script this case is about was never built")
            hook_file = Path(temporary) / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(str(ENTRY_POINT), str(target), 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            try:
                cell = why_no_record(temporary)
            finally:
                target.chmod(0o644)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.ESTABLISHED,
                         "an adapter script the interpreter cannot open was read as startable"
                         " because a regular file exists at its path")

    def test_a_valid_answer_survives_a_wrapper_that_replaces_the_exit_status(self):
        """Only a program that RAN the source can produce this nonce.

        Reading the exit status before the answer threw that away: a launcher that runs python
        and then exits 1 of its own accord prints the nonce and the version, and the probe
        discarded both and left startability unsettled -- so an empty journal could report
        cause_unreadable on a host whose hook is fine. The answer is evidence and the status
        is not, so the answer is read first.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            launcher = host / "a-launcher-that-exits-one"
            # No exec, so the launcher keeps control and chooses its own status after the
            # interpreter has already written the answer.
            launcher.write_text("#!/bin/sh\n" + shlex.quote(sys.executable)
                                + " \"$@\"\nexit 1\n", encoding="utf-8")
            launcher.chmod(0o755)
            register(temporary)
            hook_file = host / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(sys.executable, str(launcher), 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["registeredInterpreter"]["value"], reading.PRESENT,
                         "a launcher that answered the nonce and the version was discarded"
                         " because it chose its own exit status")

    def test_an_adapter_run_directly_is_not_judged_as_an_interpreter(self):
        """The first word is the script, so there is no interpreter to have a verdict about.

        A registration can execute the adapter directly through its shebang. Running that first
        word with an interpreter's own option makes the adapter treat the option as its
        settings path and exit 0 in silence -- which is exactly the shape the /bin/true verdict
        is for, arriving from a host whose hook works and writes records. A verdict about
        interpreters there would be about a question this command invented.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            register(temporary)
            direct = host / ENTRY_POINT.name
            direct.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            direct.chmod(0o755)
            hook_file = host / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(
                sys.executable + " " + str(ENTRY_POINT), str(direct), 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertNotEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                            firing.ESTABLISHED,
                            "a registration that runs the adapter directly was reported as"
                            " naming an interpreter the host cannot start")

    def test_a_wrapper_around_an_interpreter_is_left_unjudged(self):
        """Refusing the question and answering it wrongly are different facts.

        A registration whose first word is a wrapper -- /usr/bin/env python3 ... -- starts the
        adapter perfectly well through the words that follow, which this command deliberately
        does not follow. Running the wrapper with an interpreter's own option makes it reject
        the option and exit non-zero, and reading that as "not an interpreter" condemned a
        working registration: a definite repair for a host that has none, which is the
        strongest form of the answer this issue exists to prevent.

        The family the probe is for does the opposite -- it exits 0 and says nothing -- so the
        two are separable, and the one that cannot be established says so.
        """
        if not Path("/usr/bin/env").exists():
            self.skipTest("this host has no /usr/bin/env, so the wrapper this case is about"
                          " cannot be built")
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            hook_file = Path(temporary) / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(
                sys.executable, "/usr/bin/env " + shlex.quote(sys.executable), 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertNotEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                            firing.ESTABLISHED,
                            "a wrapper that starts the adapter was reported as a launcher the"
                            " host cannot start, because it declined an option meant for the"
                            " interpreter behind it")

    def test_a_python_below_the_floor_is_not_a_startable_interpreter(self):
        """Being a Python is not the whole question, and the installer already knew that.

        _require_python refuses to REGISTER an interpreter below SUPPORTED_PYTHON, because the
        adapter fails on every Stop before evaluating or journalling anything. Diagnosis then
        ran the same host and reported the registration startable: the writer's predicate and
        the reader's disagreed about one host, which is the stronger form of a capability
        claimed beyond what the reading established.
        """
        below = ".".join(str(part - 1 if index else part)
                         for index, part in enumerate(completion.SUPPORTED_PYTHON))
        with tempfile.TemporaryDirectory() as temporary:
            host = self._host(temporary)
            # A real interpreter, answering the probe truthfully, from a version too old.
            old = host / "an-older-python"
            old.write_text("#!/bin/sh\n"
                           + "exec " + shlex.quote(sys.executable)
                           + " -c \"import sys;src=sys.argv[1];"
                           + "sys.argv=['-c'];"
                           + "exec(src.replace('sys.version_info[0], sys.version_info[1]',"
                           + " '" + str(completion.SUPPORTED_PYTHON[0]) + ","
                           + str(completion.SUPPORTED_PYTHON[1] - 1) + "'))\" \"$2\"\n",
                           encoding="utf-8")
            old.chmod(0o755)
            register(temporary)
            hook_file = host / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = written["hooks"][completion.EVENT][0]["hooks"][0]
            entry["command"] = entry["command"].replace(sys.executable, str(old), 1)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            cell = why_no_record(temporary)
        self.assertEqual(self._standings(cell).get(firing.ADAPTER_CANNOT_RUN),
                         firing.ESTABLISHED,
                         "an interpreter the installer would refuse for its version was read"
                         " as startable by diagnosis, on the same host (" + below + ")")

    def test_every_state_the_interpreter_probe_can_answer_has_a_consumer(self):
        """SUPPORT, not evidence: the sweep for the class, derived from the probe's own source.

        The states are read out of _answers_as_an_interpreter rather than listed here, so a
        state added later is swept too. Each one has to be accounted for by BOTH consumers of
        the probe's answer: firing's CANNOT_START, which the rules read, and the startable
        mapping the journal entries carry. A state that neither classifies is the defect that
        produced this sweep -- three consumers, one richer vocabulary, and the new members
        dropped on the floor at each of them.
        """
        source = inspect.getsource(completion._answers_as_an_interpreter)
        answered = set()
        for node in ast.walk(ast.parse(textwrap.dedent(source))):
            if isinstance(node, ast.Call) and getattr(node.func, "id", None) == "_cell":
                named = node.args[0]
                if isinstance(named, ast.Attribute):
                    answered.add(getattr(getattr(completion, named.value.id), named.attr))
                elif isinstance(named, ast.Name):
                    answered.add(getattr(completion, named.id))
        self.assertTrue(answered, "no state could be read out of the probe's source, so this"
                                  " sweep would pass by finding nothing")
        for state in sorted(answered):
            with self.subTest(state=state):
                if state == reading.PRESENT:
                    self.assertIs(completion._startable_from({state}), True)
                    continue
                halves = {reading.PRESENT, state}
                if state in firing.CANNOT_START:
                    self.assertIs(completion._startable_from(halves), False,
                                  "a state the rules treat as unable to start was not carried"
                                  " into the journal entry's startable flag")
                else:
                    self.assertIn(state, completion.INTERPRETER_UNESTABLISHED,
                                  "the probe can answer a state that neither CANNOT_START nor"
                                  " INTERPRETER_UNESTABLISHED accounts for")
                    self.assertIsNone(completion._startable_from(halves))

    def test_a_recorded_interpreter_that_is_not_one_is_not_startable_either(self):
        """SUPPORT, not evidence: the sibling site, pinned so the class stays closed at both.

        The registered interpreter and the one plugin-owned settings record are two call sites
        of the same presence-then-assert shape, and closing one of two is how this repository
        keeps rediscovering a class it thought it had removed.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary, owner=completion.OWNER_PLUGIN,
                     adapterInterpreter="/bin/true",
                     adapterEntryPoint=str(ENTRY_POINT))
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["adapterInterpreter"]["value"], firing.NOT_AN_INTERPRETER)

    def test_an_unread_settings_document_names_no_owner_to_the_operator(self):
        """Four states reach one predicate, and the sentence spoke for only one of them.

        status() passes registrationReadHere=False for settings that are missing, unreadable,
        not an object, or that name an owner this reader does not know. The branch then told
        the operator that the settings record an owner whose registration lives in a package
        manifest -- a definite claim about a document nothing read, beside a registrationOwner
        cell saying not_read in the same payload.
        """
        settled = firing.decide({"registrationReadable": True, "adapterRegistrations": 0,
                                 "registrationReadHere": False, "registrationElsewhere": False,
                                 "namedJournals": [], "namedSettings": []})
        detail = {one["cause"]: one["detail"]
                  for group in ("candidates", "ruledOut", "notEvaluated")
                  for one in (settled.get(group) or [])}.get(firing.NOT_REGISTERED, "")
        self.assertNotIn("package manifest", detail,
                         "a document nothing could read was narrated as recording an owner"
                         " whose registration lives in a package manifest")

    def test_no_absence_rule_claims_an_owner_nothing_established(self):
        """SUPPORT, not evidence: the sweep for the class, derived from CAUSE_RULES.

        The rule set is taken from the source rather than listed here, so a cause added later
        is swept too. On a host where nothing about ownership was established, no rule may
        narrate the package manifest -- which is the one definite ownership claim these
        answers make.
        """
        nothing_established = {"registrationReadable": True, "adapterRegistrations": 0,
                               "registrationReadHere": False, "registrationElsewhere": False,
                               "namedJournals": [], "namedSettings": [], "startProbes": []}
        for cause, (_keys, rule) in firing.CAUSE_RULES.items():
            with self.subTest(cause=cause):
                _standing, detail = rule(nothing_established)
                self.assertNotIn("package manifest", detail or "")

    def test_a_user_owned_host_with_nothing_named_still_evaluates_nothing(self):
        """SUPPORT, not evidence. The direction the requirement change must not break: where no
        settings file was read for the question to be about, the policy causes answer
        not_evaluated rather than putting a candidate on the table no reading points at."""
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            settings(temporary)
            cell = why_no_record(temporary)
        self.assertNotEqual(self._standings(cell).get(firing.JOURNALLING_OFF),
                            firing.ESTABLISHED)

    def test_one_settings_file_named_through_an_alias_is_one_source(self):
        """Two spellings of one file are one source, and the configuration is read.

        The ambiguity check that decides whether a single file answers for the host was still
        lexical, so a second registration naming this very file through a symlink counted as a
        second source: the configuration went unread and reported the ambiguity, while the
        cause partition beside it asked the kernel, read that one file once, and answered from
        it. One payload, two answers about one file.
        """
        with tempfile.TemporaryDirectory() as temporary:
            self._host(temporary)
            register(temporary)
            named = completion.configuration_path(Path(temporary))
            alias = Path(temporary) / "an-alias-of-the-settings.json"
            alias.symlink_to(named)
            hook_file = Path(temporary) / "hooks.json"
            written = json.loads(hook_file.read_text(encoding="utf-8"))
            entry = dict(written["hooks"][completion.EVENT][0]["hooks"][0])
            entry["command"] = entry["command"].replace(str(named), str(alias))
            written["hooks"][completion.EVENT][0]["hooks"].append(entry)
            hook_file.write_text(json.dumps(written), encoding="utf-8")
            found = completion.status(codex_home=temporary, environ={})
        self.assertEqual(found["configuration"]["value"], reading.PRESENT,
                         "one settings file named through two spellings was reported as two"
                         " sources, so the configuration went unread")


class TheCausePartitionItself(unittest.TestCase):
    """Support for the cases above, not evidence of the defect. These check that the partition
    is well formed; none of them would have failed on the behaviour CRW-100 reports."""

    def test_every_rule_and_requirement_names_a_declared_cause(self):
        self.assertTrue(set(firing.CAUSE_RULES) <= set(firing.CAUSES))
        self.assertEqual(set(firing.CAUSE_RULES), set(firing.CAUSE_REQUIRES),
                         "a cause with a rule and no requirements entry, or the reverse")
        self.assertEqual(set(firing.CAUSE_ORDER), set(firing.CAUSE_RULES),
                         "a cause that is decided and never reported, or the reverse")
        for cause, required in firing.CAUSE_REQUIRES.items():
            with self.subTest(cause=cause):
                self.assertTrue(set(required) <= set(firing.CAUSE_RULES),
                                "a requirement naming a cause nothing decides could never be"
                                " ruled out, so the cause it guards would never be evaluated")

    def test_a_requirement_is_never_its_own_cause_or_downstream_of_it(self):
        order = list(firing.CAUSE_ORDER)
        for cause, required in firing.CAUSE_REQUIRES.items():
            for name in required:
                with self.subTest(cause=cause, requires=name):
                    self.assertLess(order.index(name), order.index(cause),
                                    "a cause required to be ruled out after the one it guards"
                                    " is decided is a requirement nothing can satisfy")

    def test_the_cell_is_answered_whichever_branch_of_the_settings_fork_ran(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            register(temporary)
            second_registration(temporary, "journal-two")
            ambiguous = completion.status(codex_home=temporary, environ={})
        with tempfile.TemporaryDirectory() as temporary:
            fake_relay(temporary, stdout=json.dumps(RELEASED))
            register(temporary)
            settled = completion.status(codex_home=temporary, environ={})
        for found in (ambiguous, settled):
            self.assertIn("firingRecordAbsence", found)
            self.assertIn(found["firingRecordAbsence"]["value"], firing.CAUSES)
            self.assertTrue(found["firingRecordAbsence"]["evidence"])


if __name__ == "__main__":
    unittest.main()
