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
import json
import os
from pathlib import Path
import shlex
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion, hooks, reading

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


if __name__ == "__main__":
    unittest.main()
