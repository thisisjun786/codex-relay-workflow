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
import stat
import subprocess
import sys
import tempfile
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
            settings(temporary, dbPath="/tmp/relay.sqlite", mode=completion.HOLD)
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
            answer = completion.run(json.dumps(STOP).encode("utf-8"), codex_home=temporary,
                                    environ={})
            records = journalled(temporary)
        self.assertIsNone(answer)
        self.assertEqual(records[0]["adapterOutcome"], completion.GUARD_TIMED_OUT)
        self.assertEqual(records[0]["processEnding"], completion.TIMED_OUT)

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
            code, payload = self._install(home, mode=completion.HOLD)
            document = json.loads(completion.configuration_path(home).read_text(encoding="utf-8"))
            registered = hooks.inventory(hooks.read(home / "hooks.json").value, completion.EVENT)
        self.assertEqual(code, 1)
        self.assertEqual(payload["settings"]["outcome"], completion.CONFIG_DIFFERS)
        self.assertIn("mode", payload["settings"]["differingFields"])
        self.assertIsNone(payload["result"], "no hook is appended when its settings were refused")
        self.assertEqual(document["mode"], completion.OBSERVE, "the mode was not changed silently")
        self.assertEqual(len(registered), 1, "and no second registration was added")

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
