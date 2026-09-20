"""The packaged adapter and the checkout adapter, driven over the same invocations.

There are two copies of the Stop adapter's runtime path right now: scripts/crw_runtime/completion.py
owns the one a user-owned registration runs, and codex_session_relay.stopadapter is the one an
installed runtime carries for the plugin-declared registration. Two copies of a rule do not stay in
agreement by themselves, so this is the test that makes the duplicate honest rather than merely
temporary.

What it compares is behaviour, not shape. Matching names, matching signatures or a re-exported
symbol would let the two diverge in what they actually DO while this file stayed green, so every
case drives both adapters over the same payload against the same stub runtime and compares three
things: the exact text returned to the host, the record each one wrote, and where it wrote it.

And it carries its own proof that it would notice. MutationNoticedTests breaks the packaged copy's
runtime behaviour on purpose and requires the comparison to fail, because a green agreement test is
not evidence that a red one is reachable.

Neither adapter is imported through the relay package here. stopadapter.py imports nothing but the
standard library, so this test loads it from its file and runs in the ordinary offline job, with no
uv, no install and no relay on PATH.
"""

import importlib.util
import json
import os
import re
import stat
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion  # noqa: E402

PACKAGED_PATH = (ROOT / "packages" / "codex-session-relay" / "src" / "codex_session_relay"
                 / "stopadapter.py")

JOURNAL_NAME = re.compile(r"^[0-9a-f]{32}\.json$")
DAY = re.compile(r"^[0-9]{8}$")

# What the two copies legitimately disagree about, and why each one is excluded. Everything else in
# the record is compared, so a field added to one copy and not the other is a failure here.
#
#   configuration  each adapter is pointed at its own settings file, because they need separate
#                  journal roots to be read back independently
#   journalledAs   the path inside that separate root
#   at             a wall-clock second that can tick between the two runs
#   elapsedMs      how long each run took
#   guardElapsedMs the same, for the guard call
NOT_COMPARED = ("configuration", "journalledAs", "at", "elapsedMs", "guardElapsedMs")


def load_packaged():
    spec = importlib.util.spec_from_file_location("crw_packaged_stopadapter", PACKAGED_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


PACKAGED = load_packaged()

# A stub runtime per case. It is a real program with a real exit status, because the outcome this
# adapter reports is derived from the pair (how the process ended, what it said) and a fake that
# only returned text would leave half of that unexercised.
STUB = """#!/usr/bin/env python3
import json, os, sys
sys.stdin.buffer.read()
behaviour = {behaviour!r}
if behaviour == "verdict_block":
    sys.stdout.write(json.dumps({{"decision": "block", "state": "declared_not_verified",
        "assignmentId": "a-1", "recordedAs": "observation",
        "hook_output": {{"decision": "block", "reason": "verify the child", "continue": True}}}}))
elif behaviour == "verdict_release":
    sys.stdout.write(json.dumps({{"decision": "release", "state": "unmanaged",
        "hook_output": {{}}}}))
elif behaviour == "verdict_disagrees":
    sys.stdout.write(json.dumps({{"decision": "release",
        "hook_output": {{"decision": "block", "reason": "r", "continue": True}}}}))
elif behaviour == "error_record":
    sys.stdout.write(json.dumps({{"error": "the request was refused"}}))
elif behaviour == "garbage":
    sys.stdout.write("this is not json")
elif behaviour == "not_an_object":
    sys.stdout.write("[1, 2, 3]")
elif behaviour == "signal_self":
    os.kill(os.getpid(), 9)
elif behaviour == "sleep":
    import time
    time.sleep(30)
raise SystemExit({code})
"""


def write_stub(path, behaviour, code=0):
    path.write_text(STUB.format(behaviour=behaviour, code=code), encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return path


def settings_document(*, relay, marker, journal, timeout=5, mode="observe", policy=None,
                      version=completion.CONFIG_VERSION, owner=None, adapter=True):
    document = {
        "configVersion": version,
        "event": "Stop",
        "relayExecutable": str(relay),
        "markerRoot": str(marker),
        "dbPath": None,
        "mode": mode,
        "timeoutSeconds": timeout,
        "journalRoot": str(journal),
        "journalPolicy": policy or completion.EVERY_INVOCATION,
        "installedBy": "CRW-115",
        "isolationAssertedBy": None,
    }
    if owner is not None:
        # The production path for a plugin installation, and the one the first draft of this file
        # never exercised: every fixture omitted owner, so the branch that requires the adapter
        # fields was compared in neither copy.
        document["owner"] = owner
        document["adapterInterpreter"] = str(sys.executable) if adapter else None
        document["adapterEntryPoint"] = str(PACKAGED_PATH) if adapter else None
    return document


def journal_records(root):
    """Every record under a journal root, with the day directory that held it."""
    found = []
    base = Path(root)
    if not base.is_dir():
        return found
    for day in sorted(base.iterdir()):
        if not day.is_dir():
            continue
        for entry in sorted(day.iterdir()):
            found.append({"day": day.name, "name": entry.name,
                          "record": json.loads(entry.read_text(encoding="utf-8"))})
    return found


def drive(adapter, home, payload, *, document=None, settings_name="settings.json",
          settings_is_directory=False, absent=False, raw=None, settings_path=None):
    """Run one adapter once, and return what the host would have seen plus what was written."""
    root = home / adapter_label(adapter)
    root.mkdir(parents=True, exist_ok=True)
    journal = root / "journal"
    settings = Path(settings_path) if settings_path else root / settings_name
    if settings_path:
        pass
    elif settings_is_directory:
        settings.mkdir()
    elif raw is not None:
        settings.write_bytes(raw)
    elif not absent:
        filled = dict(document)
        filled["journalRoot"] = str(journal)
        settings.write_text(json.dumps(filled, indent=2, sort_keys=True), encoding="utf-8")
    returned = adapter.run(payload, settings=str(settings))
    return {"returned": returned, "written": journal_records(journal)}


def adapter_label(adapter):
    return "packaged" if getattr(adapter, "__name__", "") == PACKAGED.__name__ else "checkout"


def differences(left, right):
    """Every way the two answers are not the same answer."""
    found = []
    if left["returned"] != right["returned"]:
        found.append("returned: checkout " + repr(left["returned"])
                     + " vs packaged " + repr(right["returned"]))
    if len(left["written"]) != len(right["written"]):
        found.append("record count: checkout " + str(len(left["written"]))
                     + " vs packaged " + str(len(right["written"])))
        return found
    for one, two in zip(left["written"], right["written"]):
        if not DAY.match(one["day"]) or not DAY.match(two["day"]):
            found.append("journal day directory: " + one["day"] + " / " + two["day"])
        if not JOURNAL_NAME.match(one["name"]) or not JOURNAL_NAME.match(two["name"]):
            found.append("journal record name: " + one["name"] + " / " + two["name"])
        if one["day"] != two["day"]:
            found.append("different day directories: " + one["day"] + " vs " + two["day"])
        first = {k: v for k, v in one["record"].items() if k not in NOT_COMPARED}
        second = {k: v for k, v in two["record"].items() if k not in NOT_COMPARED}
        if set(first) != set(second):
            found.append("record fields: only in checkout "
                         + repr(sorted(set(first) - set(second)))
                         + ", only in packaged " + repr(sorted(set(second) - set(first))))
        for field in sorted(set(first) & set(second)):
            if first[field] != second[field]:
                found.append(field + ": checkout " + repr(first[field])
                             + " vs packaged " + repr(second[field]))
    return found


def reading_of(adapter, path):
    """What each copy's settings reader DECIDED, which the journal cannot show.

    A settings failure writes no record and returns nothing, because the record's location is what
    could not be read. So driving run() over a broken settings file compares two silences and would
    call any divergence agreement. The decision itself is the observable behaviour here, and this is
    where it is compared.
    """
    if adapter is completion:
        document, outcome, _detail, _found = completion.read_configuration(path)
    else:
        document, outcome, _detail = adapter.read_settings(path)
    return {"outcome": outcome, "document": document is not None}


def reading_differences(path, packaged=None):
    left = reading_of(completion, path)
    right = reading_of(packaged or PACKAGED, path)
    if left == right:
        return []
    return ["settings reading: checkout " + repr(left) + " vs packaged " + repr(right)]

class AgreementTests(unittest.TestCase):
    """One case per invocation class the adapter has to classify."""

    def run_case(self, packaged=PACKAGED, **case):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            stub = None
            behaviour = case.get("behaviour")
            if behaviour is not None:
                stub = write_stub(home / "relay.py", behaviour, case.get("exitCode", 0))
            document = settings_document(
                relay=stub if stub else case.get("relay", home / "absent" / "relay"),
                marker=home / "marker", journal=home / "unused",
                timeout=case.get("timeout", 5), mode=case.get("mode", "observe"),
                policy=case.get("policy"), version=case.get("version",
                                                            completion.CONFIG_VERSION),
                owner=case.get("owner"), adapter=case.get("adapter", True))
            shared = {"document": document, "absent": case.get("absent", False),
                      "settings_is_directory": case.get("settingsIsDirectory", False),
                      "raw": case.get("raw"), "settings_path": case.get("settingsPath")}
            payload = case.get("payload", json.dumps({
                "session_id": "s-1", "turn_id": "t-1", "cwd": str(home),
                "stop_hook_active": False}).encode("utf-8"))
            left = drive(completion, home, payload, **shared)
            right = drive(packaged, home, payload, **shared)
            return left, right

    def assert_agrees(self, **case):
        left, right = self.run_case(**case)
        found = differences(left, right)
        self.assertEqual(found, [], "the two adapters answered differently: " + repr(found))
        return left

    def test_a_held_verdict_is_the_same_hold_in_both(self):
        answer = self.assert_agrees(behaviour="verdict_block")
        self.assertIsNotNone(answer["returned"])
        self.assertEqual(json.loads(answer["returned"])["decision"], "block")
        self.assertTrue(answer["written"][0]["record"]["held"])

    def test_a_released_verdict_prints_nothing_in_both(self):
        answer = self.assert_agrees(behaviour="verdict_release")
        self.assertIsNone(answer["returned"])
        self.assertEqual(answer["written"][0]["record"]["adapterOutcome"],
                         completion.GUARD_ANSWERED)

    def test_a_verdict_that_disagrees_with_itself_is_incomplete_in_both(self):
        answer = self.assert_agrees(behaviour="verdict_disagrees")
        self.assertEqual(answer["written"][0]["record"]["adapterOutcome"],
                         completion.GUARD_VERDICT_INCOMPLETE)

    def test_an_error_record_at_each_exit_code_is_its_own_outcome_in_both(self):
        for code, expected in ((completion.GUARD_EXIT_REFUSED, completion.GUARD_REFUSED),
                               (completion.GUARD_EXIT_HOST, completion.GUARD_HOST_ERROR),
                               (completion.GUARD_EXIT_USAGE, completion.GUARD_USAGE_ERROR),
                               (7, completion.GUARD_ENDED_UNEXPECTEDLY)):
            with self.subTest(code=code):
                answer = self.assert_agrees(behaviour="error_record", exitCode=code)
                self.assertEqual(answer["written"][0]["record"]["adapterOutcome"], expected)

    def test_silence_at_two_and_at_zero_are_different_outcomes_in_both(self):
        for code, expected in ((completion.GUARD_EXIT_REFUSED,
                                completion.GUARD_REJECTED_THE_CALL),
                               (completion.GUARD_EXIT_OK, completion.GUARD_SAID_NOTHING),
                               (9, completion.GUARD_ENDED_UNEXPECTEDLY)):
            with self.subTest(code=code):
                answer = self.assert_agrees(behaviour="silent", exitCode=code)
                self.assertEqual(answer["written"][0]["record"]["adapterOutcome"], expected)

    def test_unreadable_output_is_not_a_verdict_in_both(self):
        for behaviour in ("garbage", "not_an_object"):
            with self.subTest(behaviour=behaviour):
                answer = self.assert_agrees(behaviour=behaviour)
                self.assertEqual(answer["written"][0]["record"]["adapterOutcome"],
                                 completion.GUARD_OUTPUT_UNREADABLE)

    def test_a_runtime_that_cannot_be_run_is_unreachable_in_both(self):
        answer = self.assert_agrees()
        self.assertEqual(answer["written"][0]["record"]["adapterOutcome"],
                         completion.GUARD_UNREACHABLE)

    def test_a_signalled_runtime_is_signalled_in_both(self):
        answer = self.assert_agrees(behaviour="signal_self")
        self.assertEqual(answer["written"][0]["record"]["adapterOutcome"],
                         completion.GUARD_SIGNALLED)

    def test_a_runtime_past_its_budget_times_out_in_both(self):
        answer = self.assert_agrees(behaviour="sleep", timeout=1)
        self.assertEqual(answer["written"][0]["record"]["adapterOutcome"],
                         completion.GUARD_TIMED_OUT)

    def test_absent_settings_are_absent_in_both(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "nothing.json"
            self.assertEqual(reading_differences(path), [])
            self.assertEqual(reading_of(PACKAGED, path)["outcome"], completion.CONFIG_ABSENT)

    def test_malformed_settings_are_malformed_in_both(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.write_text(json.dumps(settings_document(relay="/bin/true", marker="/tmp",
                                                         journal="/tmp/j", version=99)),
                            encoding="utf-8")
            self.assertEqual(reading_differences(path), [])
            self.assertEqual(reading_of(PACKAGED, path)["outcome"], completion.CONFIG_MALFORMED)

    def test_settings_that_are_a_directory_are_unreadable_in_both(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.mkdir()
            self.assertEqual(reading_differences(path), [])
            self.assertEqual(reading_of(PACKAGED, path)["outcome"], completion.CONFIG_UNREADABLE)

    def test_settings_that_are_not_utf8_are_unreadable_in_both(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.write_bytes(b"\xff\xfe not text")
            self.assertEqual(reading_differences(path), [])
            self.assertEqual(reading_of(PACKAGED, path)["outcome"], completion.CONFIG_UNREADABLE)

    def test_every_payload_failure_is_the_same_failure_in_both(self):
        for payload, expected in ((b"not json at all", completion.STDIN_NOT_JSON),
                                  (b"[1,2,3]", completion.STDIN_NOT_OBJECT),
                                  (None, completion.STDIN_UNREADABLE)):
            with self.subTest(payload=payload):
                answer = self.assert_agrees(behaviour="verdict_release", payload=payload)
                self.assertEqual(answer["written"][0]["record"]["adapterOutcome"], expected)

    def test_a_faults_only_policy_records_nothing_for_an_answer_in_both(self):
        left, right = self.run_case(behaviour="verdict_release", policy=completion.FAULTS_ONLY)
        self.assertEqual(differences(left, right), [])
        self.assertEqual(left["written"], [])

    def test_a_no_journal_policy_records_nothing_at_all_in_both(self):
        left, right = self.run_case(behaviour="garbage", policy=completion.NO_JOURNAL)
        self.assertEqual(differences(left, right), [])
        self.assertEqual(left["written"], [])

    def test_a_plugin_owned_document_is_the_same_answer_in_both(self):
        """The production path for a plugin installation, which the first draft never drove."""
        answer = self.assert_agrees(behaviour="verdict_block", owner=completion.OWNER_PLUGIN)
        self.assertEqual(json.loads(answer["returned"])["decision"], "block")

    def test_a_plugin_owned_document_missing_its_adapter_fields_is_malformed_in_both(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.write_text(json.dumps(settings_document(
                relay="/bin/true", marker="/tmp", journal="/tmp/j",
                owner=completion.OWNER_PLUGIN, adapter=False)), encoding="utf-8")
            self.assertEqual(reading_differences(path), [])
            self.assertEqual(reading_of(PACKAGED, path)["outcome"], completion.CONFIG_MALFORMED)

    def test_a_plugin_owned_budget_at_the_launcher_ceiling_is_malformed_in_both(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.write_text(json.dumps(settings_document(
                relay="/bin/true", marker="/tmp", journal="/tmp/j",
                owner=completion.OWNER_PLUGIN,
                timeout=completion.LAUNCHER_CEILING_SECONDS)), encoding="utf-8")
            self.assertEqual(reading_differences(path), [])
            self.assertEqual(reading_of(PACKAGED, path)["outcome"], completion.CONFIG_MALFORMED)

    def test_a_settings_path_that_is_a_device_is_unreadable_in_both(self):
        self.assertEqual(reading_differences(Path("/dev/null")), [])
        self.assertEqual(reading_of(PACKAGED, Path("/dev/null"))["outcome"],
                         completion.CONFIG_UNREADABLE)

    def test_a_settings_path_that_is_a_named_pipe_is_refused_without_opening_it_in_both(self):
        """The regression this case exists for: opening a FIFO blocks until somebody writes.

        A Stop blocked here is killed by the host, and a killed adapter records nothing, which is
        the one outcome this adapter is built to avoid. Each call runs on its own thread with a
        bound, so a copy that goes back to opening the path fails this instead of hanging the suite.
        """
        import threading
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            fifo = home / "settings.fifo"
            os.mkfifo(fifo)
            answers = {}

            def call(label, adapter):
                answers[label] = adapter.run(b"{}", settings=str(fifo))

            for label, adapter in (("checkout", completion), ("packaged", PACKAGED)):
                worker = threading.Thread(target=call, args=(label, adapter), daemon=True)
                worker.start()
                worker.join(10)
                self.assertFalse(worker.is_alive(),
                                 label + " opened a named pipe and blocked on it")
            self.assertEqual(answers["checkout"], answers["packaged"])
            self.assertIsNone(answers["packaged"])
            self.assertEqual(reading_differences(fifo), [])
            self.assertEqual(reading_of(PACKAGED, fifo)["outcome"],
                             completion.CONFIG_UNREADABLE)

    def test_a_record_is_written_only_to_the_owner_and_only_readable_by_it(self):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            stub = write_stub(home / "relay.py", "verdict_release", 0)
            document = settings_document(relay=stub, marker=home / "marker",
                                         journal=home / "unused")
            written = drive(PACKAGED, home, json.dumps({"session_id": "s"}).encode(),
                            document=document)["written"]
            self.assertEqual(len(written), 1)
            path = (home / "packaged" / "journal" / written[0]["day"] / written[0]["name"])
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)


class MutationNoticedTests(unittest.TestCase):
    """The agreement test, proved red.

    A safeguard nobody has seen fail is a safeguard nobody has tested. Each case here breaks the
    packaged copy's RUNTIME BEHAVIOUR in a different place and requires the comparison above to
    report a difference. If a mutation stops being noticed, this fails and says which one.
    """

    def mutated(self, **changes):
        copy = load_packaged()
        for name, value in changes.items():
            setattr(copy, name, value)
        return copy

    def assert_noticed(self, copy, **case):
        runner = AgreementTests("test_a_held_verdict_is_the_same_hold_in_both")
        left, right = runner.run_case(packaged=copy, **case)
        found = differences(left, right)
        self.assertNotEqual(found, [], "a mutation in the packaged adapter went unnoticed")
        return found

    def test_a_changed_exit_code_mapping_is_noticed(self):
        found = self.assert_noticed(self.mutated(GUARD_EXIT_REFUSED=99),
                                    behaviour="error_record",
                                    exitCode=completion.GUARD_EXIT_REFUSED)
        self.assertTrue(any("adapterOutcome" in line for line in found), found)

    def test_a_hold_turned_into_a_release_is_noticed(self):
        found = self.assert_noticed(self.mutated(hook_output=lambda verdict: None),
                                    behaviour="verdict_block")
        self.assertTrue(any(line.startswith("returned:") or "held" in line for line in found),
                        found)

    def test_a_journal_that_stops_being_written_is_noticed(self):
        found = self.assert_noticed(self.mutated(journal=lambda config, record: None),
                                    behaviour="verdict_release")
        self.assertTrue(any("record count" in line for line in found), found)

    def test_a_field_dropped_from_the_record_is_noticed(self):
        original = PACKAGED.run

        def thinner(payload, **keywords):
            answer = original(payload, **keywords)
            return answer

        copy = self.mutated()
        # Drop one field the record is supposed to carry, in the copy only.
        inner = copy.run

        def without_session(payload, **keywords):
            keep = copy.journal

            def stripped(config, record):
                record.pop("stopHookActive", None)
                return keep(config, record)

            copy.journal = stripped
            try:
                return inner(payload, **keywords)
            finally:
                copy.journal = keep

        copy.run = without_session
        found = self.assert_noticed(copy, behaviour="verdict_release")
        self.assertTrue(any("record fields" in line for line in found), found)
        self.assertIsNotNone(thinner)


    def test_a_dropped_plugin_owner_requirement_is_noticed(self):
        """The mutation the first draft of this file missed, because no fixture set an owner."""
        copy = self.mutated(OWNER_PLUGIN="not-the-plugin")
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.write_text(json.dumps(settings_document(
                relay="/bin/true", marker="/tmp", journal="/tmp/j",
                owner=completion.OWNER_PLUGIN, adapter=False)), encoding="utf-8")
            self.assertNotEqual(reading_differences(path, copy), [],
                               "dropping the plugin-owner requirement went unnoticed")

    def test_a_reader_that_stops_refusing_a_non_regular_file_is_noticed(self):
        copy = self.mutated(observe=lambda path: None)
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "settings.json"
            path.mkdir()
            self.assertNotEqual(reading_differences(path, copy), [],
                               "a reader that stopped refusing a directory went unnoticed")


if __name__ == "__main__":
    unittest.main()
