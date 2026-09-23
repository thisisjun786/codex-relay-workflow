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
#   identityScanMs how long reading the transcript took
NOT_COMPARED = ("configuration", "journalledAs", "at", "elapsedMs", "guardElapsedMs",
                "identityScanMs")


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
with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "calls"), "a") as log:
    log.write("1\\n")
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
    """Every row under a journal root, with the day directory that held it.

    Only the shapes a row takes: a day directory and a 32-hex name. The accepted records live
    beside them under accepted/ and are read by ledger_records, never as rows.
    """
    found = []
    base = Path(root)
    if not base.is_dir():
        return found
    for day in sorted(base.iterdir()):
        if not day.is_dir() or not DAY.match(day.name):
            continue
        for entry in sorted(day.iterdir()):
            if not JOURNAL_NAME.match(entry.name):
                continue
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

    def test_a_faults_only_policy_still_records_an_answer_given_without_an_identity_in_both(self):
        """faults_only leaves out answers to events this hook identified (see EventAcceptanceAgrees).
        This payload names no transcript, so the answer was given without an identity, and that is
        reported under every policy that keeps rows: no accepted record covers it."""
        left, right = self.run_case(behaviour="verdict_release", policy=completion.FAULTS_ONLY)
        self.assertEqual(differences(left, right), [])
        self.assertEqual([entry["record"]["acceptance"] for entry in left["written"]],
                         [completion.UNESTABLISHED])

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
        found = self.assert_noticed(self.mutated(journal=lambda config, record, *slot: None),
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

            def stripped(config, record, *slot):
                record.pop("stopHookActive", None)
                return keep(config, record, *slot)

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


# ----------------------------------------------------------------- one accepted record per Stop event
#
# CRW-212. The copies must agree on which Stop event an invocation answers, on whether it may
# claim that event, and on what they write about it. Each case below drives both copies through
# the same sequence of invocations against the same transcript, each copy into its own journal
# root, and compares the answers, the rows, the accepted records and how often the stub guard was
# asked. Rows are compared in a stable order because their names are random.

FIXTURE = (ROOT / "packages" / "codex-session-relay" / "tests" / "fixtures"
           / "stop_event_r1.json")
LEDGER_NAME = re.compile(r"^[0-9a-f]{64}\.json$")
OUTCOME_NAME = re.compile(r"^[0-9a-f]{64}\.outcome\.json$")
# Wall-clock and process values in the accepted records. The attempt row each one names is checked
# by relation instead: it must be the position of a row that copy actually wrote.
LEDGER_NOT_COMPARED = ("claimedAt", "at", "claimedBy", "attemptRow")


def r1():
    return json.loads(FIXTURE.read_text(encoding="utf-8"))


def distinct_third_answer(document):
    """A copy of the fixture whose third Stop reported "DONE." rather than "DONE"."""
    changed = json.loads(json.dumps(document))
    last = changed["stops"][4]["linesAtStop"] - 1
    line = json.loads(changed["transcriptLines"][last])
    line["payload"]["item"]["content"][0]["text"] = "DONE."
    changed["transcriptLines"][last] = json.dumps(line)
    for index in (4, 5):
        changed["stops"][index]["payload"]["last_assistant_message"] = "DONE."
    return changed


def stop_bytes(stop, transcript, **changes):
    payload = dict(stop["payload"])
    payload["transcript_path"] = str(transcript)
    payload["cwd"] = str(transcript.parent)
    payload.update(changes)
    return json.dumps(payload).encode("utf-8")


def ledger_records(root):
    directory = Path(root) / "accepted"
    found = []
    if directory.exists() and directory.is_dir():
        for entry in sorted(directory.iterdir()):
            try:
                body = json.loads(entry.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                body = None
            found.append({"name": entry.name, "body": body})
    return found


def calls_made(home):
    try:
        return len((home / "calls").read_text(encoding="utf-8").splitlines())
    except FileNotFoundError:
        return 0


def drive_steps(adapter, home, steps, document, *, journal=True, prepare=None):
    """Run one copy over a sequence of (payload, transcript lines) against its own journal root."""
    root = home / adapter_label(adapter)
    root.mkdir(parents=True, exist_ok=True)
    journal_root = root / "journal"
    settings = root / "settings.json"
    filled = dict(document)
    if journal:
        filled["journalRoot"] = str(journal_root)
    else:
        filled.pop("journalRoot", None)
    settings.write_text(json.dumps(filled, indent=2, sort_keys=True), encoding="utf-8")
    if prepare:
        prepare(journal_root)
    before = calls_made(home)
    returned = []
    for payload, lines in steps:
        if lines is not None:
            (home / "rollout.jsonl").write_text("".join(line + "\n" for line in lines),
                                                encoding="utf-8")
        returned.append(adapter.run(payload, settings=str(settings)))
    return {"returned": returned, "written": journal_records(journal_root),
            "ledger": ledger_records(journal_root), "calls": calls_made(home) - before}


def row_order(entry):
    record = entry["record"]
    return (str(record.get("eventKey")), str(record.get("acceptance")),
            str(record.get("adapterOutcome")), json.dumps(record.get("eventIdentity"),
                                                         sort_keys=True))


def ledger_relations(answer):
    """Every accepted record names a row this copy wrote, or names none because none was written."""
    rows = {entry["day"] + "/" + entry["name"] for entry in answer["written"]}
    broken = []
    for entry in answer["ledger"]:
        body = entry["body"] or {}
        if OUTCOME_NAME.match(entry["name"]):
            row = body.get("attemptRow")
            if row is not None and row not in rows:
                broken.append(entry["name"] + " names a row that is not there: " + repr(row))
        elif LEDGER_NAME.match(entry["name"]):
            if body.get("eventKey") + ".json" != entry["name"]:
                broken.append(entry["name"] + " carries another key")
        else:
            broken.append("a foreign file in accepted/: " + entry["name"])
    return broken


def event_differences(left, right):
    found = []
    if left["returned"] != right["returned"]:
        found.append("returned: checkout " + repr(left["returned"]) + " vs packaged "
                     + repr(right["returned"]))
    if left["calls"] != right["calls"]:
        found.append("guard calls: checkout " + str(left["calls"]) + " vs packaged "
                     + str(right["calls"]))
    found += differences({"returned": None, "written": sorted(left["written"], key=row_order)},
                         {"returned": None, "written": sorted(right["written"], key=row_order)})
    if [e["name"] for e in left["ledger"]] != [e["name"] for e in right["ledger"]]:
        found.append("accepted records: checkout " + repr([e["name"] for e in left["ledger"]])
                     + " vs packaged " + repr([e["name"] for e in right["ledger"]]))
    else:
        for one, two in zip(left["ledger"], right["ledger"]):
            first = {k: v for k, v in (one["body"] or {}).items() if k not in LEDGER_NOT_COMPARED}
            second = {k: v for k, v in (two["body"] or {}).items() if k not in LEDGER_NOT_COMPARED}
            if first != second:
                found.append(one["name"] + ": checkout " + repr(first) + " vs packaged "
                             + repr(second))
    found += ["checkout: " + line for line in ledger_relations(left)]
    found += ["packaged: " + line for line in ledger_relations(right)]
    return found


class EventAcceptanceAgrees(unittest.TestCase):
    """Each acceptance value and each identity reason, produced and recorded alike by both."""

    def run_steps(self, steps_for, *, behaviour="verdict_release", packaged=PACKAGED, policy=None,
                  journal=True, prepare=None):
        with tempfile.TemporaryDirectory() as raw:
            home = Path(raw)
            stub = write_stub(home / "relay.py", behaviour, 0)
            document = settings_document(relay=stub, marker=home / "marker",
                                         journal=home / "unused", policy=policy)
            steps = steps_for(home)
            left = drive_steps(completion, home, steps, document, journal=journal,
                               prepare=prepare)
            right = drive_steps(packaged, home, steps, document, journal=journal,
                                prepare=prepare)
            return left, right

    def assert_agrees(self, steps_for, **kwargs):
        left, right = self.run_steps(steps_for, **kwargs)
        found = event_differences(left, right)
        self.assertEqual(found, [], "the two adapters answered differently: " + repr(found))
        return left

    def replay(self, index=0, **changes):
        document = r1()
        stop = document["stops"][index]
        lines = document["transcriptLines"][:stop["linesAtStop"]]
        return lambda home: [(stop_bytes(stop, home / "rollout.jsonl", **changes), lines)] * 2

    def acceptances(self, answer):
        return sorted(str(entry["record"].get("acceptance")) for entry in answer["written"])

    def test_a_replayed_event_is_accepted_once_in_both(self):
        answer = self.assert_agrees(self.replay())
        self.assertEqual(self.acceptances(answer), ["accepted", "duplicate"])
        self.assertEqual(answer["calls"], 1)
        self.assertEqual(len(answer["ledger"]), 2)
        duplicate = [e["record"] for e in answer["written"]
                     if e["record"]["acceptance"] == "duplicate"][0]
        self.assertEqual(duplicate["adapterOutcome"], completion.DUPLICATE_INVOCATION)
        self.assertFalse(duplicate["guardInvoked"])
        self.assertIsNone(duplicate["guardDecision"])

    def test_three_stops_of_one_turn_are_three_events_in_both(self):
        document = distinct_third_answer(r1())

        def steps(home):
            return [(stop_bytes(document["stops"][i], home / "rollout.jsonl"),
                     document["transcriptLines"][:document["stops"][i]["linesAtStop"]])
                    for i in (0, 2, 4)]

        answer = self.assert_agrees(steps, behaviour="verdict_block")
        self.assertEqual(answer["calls"], 3)
        self.assertEqual(self.acceptances(answer), ["accepted"] * 3)
        self.assertEqual(len([r for r in answer["returned"] if r]), 3)

    def test_a_late_retry_of_an_earlier_stop_is_that_stops_duplicate_in_both(self):
        """Stops of distinct texts, then Stop 2 delivered again after Stop 3's answer: it can only
        be Stop 2, so both copies call it Stop 2's duplicate and ask nothing."""
        document = distinct_third_answer(r1())

        def steps(home):
            played = [(stop_bytes(document["stops"][i], home / "rollout.jsonl"),
                       document["transcriptLines"][:document["stops"][i]["linesAtStop"]])
                      for i in (0, 2, 4)]
            return played + [(stop_bytes(document["stops"][2], home / "rollout.jsonl"), None)]

        answer = self.assert_agrees(steps)
        self.assertEqual(answer["calls"], 3)
        self.assertEqual(self.acceptances(answer), ["accepted"] * 3 + ["duplicate"])

    def test_a_stop_that_repeats_an_earlier_stops_text_is_unestablished_in_both(self):
        """The fixture's Stop 3 reported what Stop 2 reported, under the same stop_hook_active; a
        late delivery of Stop 2 would be indistinguishable from it, so neither claims."""
        document = r1()

        def steps(home):
            return [(stop_bytes(document["stops"][i], home / "rollout.jsonl"),
                     document["transcriptLines"][:document["stops"][i]["linesAtStop"]])
                    for i in (0, 2, 4)]

        answer = self.assert_agrees(steps, behaviour="verdict_block")
        self.assertEqual(self.acceptances(answer), ["accepted", "accepted", "unestablished"])
        self.assertEqual({e["record"]["eventIdentity"]["reason"] for e in answer["written"]
                          if e["record"]["acceptance"] == "unestablished"},
                         {"answer_text_ambiguous"})
        self.assertEqual(len([r for r in answer["returned"] if r]), 3)

    def test_every_reason_an_identity_is_unestablished_is_the_same_in_both(self):
        document = r1()
        first = document["stops"][0]
        prefix = document["transcriptLines"][:first["linesAtStop"]]
        answer_line = json.loads(prefix[-1])
        started = [line for line in prefix if "task_started" in line]
        unnamed = json.loads(prefix[-1])
        unnamed["payload"]["item"].pop("id")

        def filler():
            return prefix + ['{"type":"response_item","payload":{"filler":"' + "x" * 1000 + '"}}'
                             ] * 200

        threadless = json.loads(prefix[-1])
        threadless["payload"].pop("thread_id")
        prompt = document["transcriptLines"][first["linesAtStop"]]
        turn = first["payload"]["turn_id"]
        garbled = ('{"type":"event_msg","payload":{"type":"item_completed","turn_id":"' + turn
                   + '","item":{"type":"UserMessage",')

        def write_raw(home, text):
            (home / "rollout.jsonl").write_text(text, encoding="utf-8")
            return None

        cases = {
            "identity_fields_incomplete": lambda h: (
                json.dumps({"session_id": "s", "turn_id": "t"}).encode(), None),
            "transcript_path_missing": lambda h: (
                stop_bytes(first, h / "rollout.jsonl", transcript_path=None), None),
            "transcript_path_relative": lambda h: (
                stop_bytes(first, h / "rollout.jsonl", transcript_path="rollout.jsonl"), None),
            "transcript_absent": lambda h: (
                stop_bytes(first, h / "rollout.jsonl", transcript_path=str(h / "gone.jsonl")),
                None),
            "transcript_unreachable": lambda h: (
                stop_bytes(first, h / "rollout.jsonl",
                           transcript_path=str(h / "relay.py" / "rollout.jsonl")), None),
            "transcript_not_regular": lambda h: (
                stop_bytes(first, h / "rollout.jsonl", transcript_path=str(h)), None),
            "no_answer_item_for_turn": lambda h: (
                stop_bytes(first, h / "rollout.jsonl"), started),
            "answer_item_unidentified": lambda h: (
                stop_bytes(first, h / "rollout.jsonl"), prefix[:-1] + [json.dumps(unnamed)]),
            "answer_precedes_latest_input": lambda h: (
                stop_bytes(document["stops"][2], h / "rollout.jsonl"),
                document["transcriptLines"][:document["stops"][2]["linesAtStop"] - 1]),
            "answer_text_mismatch": lambda h: (
                stop_bytes(first, h / "rollout.jsonl", last_assistant_message="something else"),
                prefix),
            "session_mismatch": lambda h: (
                stop_bytes(first, h / "rollout.jsonl", session_id="another-session"), prefix),
            "scan_bound_exceeded": lambda h: (stop_bytes(first, h / "rollout.jsonl"), filler()),
            "session_mismatch (no thread recorded)": lambda h: (
                stop_bytes(first, h / "rollout.jsonl"), prefix[:-1] + [json.dumps(threadless)]),
            "transcript_tail_incomplete": lambda h: (
                stop_bytes(first, h / "rollout.jsonl"),
                write_raw(h, "".join(line + "\n" for line in prefix) + prompt[:len(prompt) // 2])),
            "transcript_line_unreadable": lambda h: (
                stop_bytes(first, h / "rollout.jsonl"), prefix + [garbled]),
            "turn_start_not_found": lambda h: (
                stop_bytes(first, h / "rollout.jsonl"),
                [line for line in prefix if "task_started" not in line]),
        }
        self.assertIn("AgentMessage", json.dumps(answer_line))
        saved = (completion.SCAN_MAX_BYTES, PACKAGED.SCAN_MAX_BYTES)
        for reason, step in cases.items():
            with self.subTest(reason=reason):
                if reason == "scan_bound_exceeded":
                    # Scaled down with the bound so the case needs a small transcript.
                    completion.SCAN_MAX_BYTES = PACKAGED.SCAN_MAX_BYTES = 1 << 16
                try:
                    answer = self.assert_agrees(lambda home, step=step: [step(home)] * 2)
                finally:
                    completion.SCAN_MAX_BYTES, PACKAGED.SCAN_MAX_BYTES = saved
                self.assertEqual(self.acceptances(answer), ["unestablished"] * 2)
                self.assertEqual({e["record"]["eventIdentity"]["reason"]
                                  for e in answer["written"]}, {reason.split(" ")[0]})
                self.assertEqual(answer["calls"], 2, "an unestablished identity is asked every time")
                self.assertEqual(answer["ledger"], [])

    def test_a_scan_out_of_time_is_unestablished_in_both(self):
        saved = (completion.SCAN_MAX_SECONDS, PACKAGED.SCAN_MAX_SECONDS)
        completion.SCAN_MAX_SECONDS = PACKAGED.SCAN_MAX_SECONDS = -1
        try:
            answer = self.assert_agrees(self.replay())
        finally:
            completion.SCAN_MAX_SECONDS, PACKAGED.SCAN_MAX_SECONDS = saved
        self.assertEqual({e["record"]["eventIdentity"]["reason"] for e in answer["written"]},
                         {"scan_timed_out"})
        self.assertEqual(answer["calls"], 2)

    def test_settings_without_a_journal_root_cannot_claim_in_both(self):
        answer = self.assert_agrees(self.replay(), journal=False)
        self.assertEqual(answer["written"], [])
        self.assertEqual(answer["ledger"], [])
        self.assertEqual(answer["calls"], 2)

    def test_a_ledger_that_cannot_be_created_fails_the_claim_in_both(self):
        def occupied(journal_root):
            journal_root.mkdir(parents=True, exist_ok=True)
            (journal_root / "accepted").write_text("not a directory", encoding="utf-8")

        answer = self.assert_agrees(self.replay(), prepare=occupied)
        self.assertEqual(self.acceptances(answer), ["claim_failed"] * 2)
        self.assertEqual(answer["calls"], 2)

    def test_no_journal_still_claims_and_records_the_outcome_in_both(self):
        answer = self.assert_agrees(self.replay(), policy=completion.NO_JOURNAL)
        self.assertEqual(answer["written"], [])
        self.assertEqual(answer["calls"], 1)
        outcome = [e["body"] for e in answer["ledger"] if OUTCOME_NAME.match(e["name"])][0]
        self.assertIsNone(outcome["attemptRow"])

    def test_faults_only_leaves_the_duplicate_out_of_the_journal_in_both(self):
        answer = self.assert_agrees(self.replay(), policy=completion.FAULTS_ONLY)
        self.assertEqual(answer["written"], [])
        self.assertEqual(answer["calls"], 1)
        self.assertEqual(len(answer["ledger"]), 2)

    def test_an_owner_that_faults_after_claiming_records_the_fault_as_the_outcome_in_both(self):
        def boom(config, payload):
            raise RuntimeError("the guard call itself failed")

        saved = (completion.invoke_guard, PACKAGED.invoke_guard)
        completion.invoke_guard = PACKAGED.invoke_guard = boom
        try:
            answer = self.assert_agrees(self.replay())
        finally:
            completion.invoke_guard, PACKAGED.invoke_guard = saved
        outcome = [e["body"] for e in answer["ledger"] if OUTCOME_NAME.match(e["name"])][0]
        self.assertEqual(outcome["adapterOutcome"], completion.ADAPTER_FAULTED)
        self.assertEqual(self.acceptances(answer), ["accepted", "duplicate"])
        faulted = [e["record"] for e in answer["written"]
                   if e["record"]["acceptance"] == "accepted"][0]
        self.assertEqual(faulted["adapterOutcome"], completion.ADAPTER_FAULTED)
        self.assertEqual(answer["calls"], 0)


class EventMutationNoticed(unittest.TestCase):
    """Each way a copy could stop keeping one accepted record per event, proved noticed."""

    def noticed(self, copy, steps_for, **kwargs):
        runner = EventAcceptanceAgrees("test_a_replayed_event_is_accepted_once_in_both")
        left, right = runner.run_steps(steps_for, packaged=copy, **kwargs)
        found = event_differences(left, right)
        self.assertNotEqual(found, [], "a mutation in the packaged adapter went unnoticed")
        return found

    def three_stops(self):
        document = r1()
        return lambda home: [(stop_bytes(document["stops"][i], home / "rollout.jsonl"),
                              document["transcriptLines"][:document["stops"][i]["linesAtStop"]])
                             for i in (0, 2, 4)]

    def test_a_key_built_from_session_and_turn_is_noticed(self):
        copy = load_packaged()
        original = copy.event_identity

        def per_turn(stop, started=None):
            key, identity = original(stop, started)
            if key is None:
                return key, identity
            return copy.hashlib.sha256((stop["session_id"] + stop["turn_id"]).encode()
                                       ).hexdigest(), identity

        copy.event_identity = per_turn
        found = self.noticed(copy, self.three_stops())
        self.assertTrue(any("guard calls" in line or "accepted records" in line
                            for line in found), found)

    def test_an_identity_minted_per_invocation_is_noticed(self):
        copy = load_packaged()
        original = copy.event_identity

        def minted(stop, started=None):
            key, identity = original(stop, started)
            return (copy.uuid.uuid4().hex * 2 if key else key), identity

        copy.event_identity = minted
        found = self.noticed(copy, EventAcceptanceAgrees("run").replay())
        self.assertTrue(any("guard calls" in line for line in found), found)

    def test_a_copy_that_stops_claiming_is_noticed(self):
        copy = load_packaged()
        copy.claim_event = lambda config, key, identity, stop, slot: (copy.ACCEPTED, None)
        found = self.noticed(copy, EventAcceptanceAgrees("run").replay())
        self.assertTrue(any("guard calls" in line for line in found), found)

    def test_a_copy_that_asks_the_guard_about_a_duplicate_is_noticed(self):
        copy = load_packaged()
        original = copy.claim_event

        def leaky(config, key, identity, stop, slot):
            acceptance, where = original(config, key, identity, stop, slot)
            return (copy.CLAIM_FAILED if acceptance == copy.DUPLICATE else acceptance), where

        copy.claim_event = leaky
        found = self.noticed(copy, EventAcceptanceAgrees("run").replay())
        self.assertTrue(any("guard calls" in line for line in found), found)


class TheGuardCommandAgrees(unittest.TestCase):
    """The command each copy builds is behaviour too, because it is what the host actually runs.

    Driven directly rather than through run(): the stub runtime above answers the same way whatever
    it is handed, so a difference in the argv would pass every case in AgreementTests. The socket
    option is the reason this class exists - it is what lets the guard refuse a state directory
    whose store belongs to another App Server, and a copy that dropped it would silently give that
    back on one of the two registration paths.
    """

    BARE = {"relayExecutable": "/opt/relay", "markerRoot": "/markers"}
    SERVED = dict(BARE, socketPath="/run/app-server.sock")
    EVERYTHING = dict(SERVED, dbPath="/state/relay.sqlite3", mode=completion.HOLD)

    def both(self):
        return (completion, PACKAGED)

    def test_both_copies_build_the_same_command(self):
        for config in (self.BARE, self.SERVED, self.EVERYTHING):
            self.assertEqual(
                completion.guard_argv(config), PACKAGED.guard_argv(config), repr(config),
            )

    def test_a_configured_socket_is_passed_as_a_global_option_in_both(self):
        for adapter in self.both():
            argv = adapter.guard_argv(self.SERVED)
            self.assertIn("--socket", argv)
            self.assertEqual(argv[argv.index("--socket") + 1], "/run/app-server.sock")
            # Global options come before the subcommand, and the relay's parser only takes it
            # there: after guard-evaluate it is an unrecognised option and the call is rejected.
            self.assertLess(argv.index("--socket"), argv.index(completion.GUARD_COMMAND))

    def test_no_configured_socket_passes_none_in_both(self):
        for adapter in self.both():
            self.assertNotIn("--socket", adapter.guard_argv(self.BARE))

    def test_a_socket_that_is_not_an_absolute_path_is_malformed_in_both(self):
        document = {"configVersion": completion.CONFIG_VERSION, "event": completion.EVENT,
                    "relayExecutable": "/opt/relay", "markerRoot": "/markers",
                    "mode": completion.OBSERVE, "socketPath": "run/app-server.sock"}
        for adapter in self.both():
            self.assertIn("socketPath must be an absolute path", adapter.complaints(document))

    def test_a_socket_that_is_not_a_string_is_malformed_in_both(self):
        document = {"configVersion": completion.CONFIG_VERSION, "event": completion.EVENT,
                    "relayExecutable": "/opt/relay", "markerRoot": "/markers",
                    "mode": completion.OBSERVE, "socketPath": 7}
        for adapter in self.both():
            self.assertIn(
                "socketPath must be a non-empty string when it is present at all",
                adapter.complaints(document),
            )

    def test_a_document_written_without_a_socket_carries_no_such_key(self):
        """Byte-identity for a host that installed before the key existed, which keeps a
        reinstall UNCHANGED rather than refusing settings that say something else."""
        document = completion.configuration(relay="/opt/relay", marker_root="/markers",
                                            codex_home="/home/someone/.codex", environ={})
        self.assertNotIn("socketPath", document)
        served = completion.configuration(relay="/opt/relay", marker_root="/markers",
                                          codex_home="/home/someone/.codex", environ={},
                                          socket="/run/app-server.sock")
        self.assertEqual(served["socketPath"], "/run/app-server.sock")
        self.assertEqual(completion.complaints(served), [])


if __name__ == "__main__":
    unittest.main()
