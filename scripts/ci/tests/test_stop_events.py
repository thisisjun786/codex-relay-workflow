"""One accepted record per Stop event, through the paths a host actually runs (CRW-212).

A Stop event is one end of one sampling sequence. Two registrations answering it, or one delivery
of it repeated, is one event handled twice; a continuation that makes the turn end again is a new
event. These cases drive the adapter only through the commands a host starts -- the plugin's
declared hook command through a shell, and the user registration's scripts/completion_hook.py --
with the payload on stdin, and they read the result from files: the stub guard's call log, the
rows and the accepted records. Nothing touches a real Codex home, runtime or store.

The Stops come from an isolated Codex 0.154.0 run (the derived fixture beside the relay's tests):
three Stops of one turn, the second and third with byte-identical payloads, so a key built from
the payload or from (session, turn) cannot tell them apart and these cases notice.

The last class is the per-event verifier, completion.stop_events and scripts/stop_events.py, which
replaces the per-(session, turn) count the CRW-116 check used.
"""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion  # noqa: E402

PACKAGED = ROOT / "packages" / "codex-session-relay" / "src" / "codex_session_relay" / "stopadapter.py"
CHECKOUT = ROOT / "scripts" / "completion_hook.py"
VERIFIER = ROOT / "scripts" / "stop_events.py"
PLUGIN_ROOT = ROOT / "plugins" / "crw"
DECLARATION = PLUGIN_ROOT / "wiring" / "hooks" / "stop-recording-completion.json"
FIXTURE = ROOT / "packages" / "codex-session-relay" / "tests" / "fixtures" / "stop_event_r1.json"
DAY = re.compile(r"^[0-9]{8}$")
ROW = re.compile(r"^[0-9a-f]{32}\.json$")
CLAIM = re.compile(r"^[0-9a-f]{64}\.json$")
OUTCOME = re.compile(r"^[0-9a-f]{64}\.outcome\.json$")


def declared_command():
    document = json.loads(DECLARATION.read_text(encoding="utf-8"))
    return document["hooks"]["Stop"][0]["hooks"][0]["command"]


class Host:
    """A temporary Codex home whose settings name the plugin as owner and a counting stub guard."""

    def __init__(self, root, *, decision="release", die_first=False, journal=None):
        self.root = root
        self.codex_home = root / "codex"
        self.codex_home.mkdir(parents=True, exist_ok=True)
        self.journal = journal or (root / "journal")
        self.calls = root / "guard-calls"
        self.transcript = root / "rollout.jsonl"
        relay = root / "relay"
        body = ["import json, os, sys", "raw = sys.stdin.buffer.read()",
                "calls = %r" % str(self.calls),
                "with open(calls, 'a', encoding='utf-8') as log:",
                "    log.write(json.dumps(json.loads(raw.decode('utf-8'))) + '\\n')"]
        if die_first:
            body += ["if sum(1 for _ in open(calls)) == 1:", "    os.kill(os.getppid(), 9)",
                     "    raise SystemExit(0)"]
        verdict = ({"decision": "block", "state": "declared",
                    "hook_output": {"decision": "block", "reason": "verify the child",
                                    "continue": True}}
                   if decision == "block" else
                   {"decision": "release", "state": "unmanaged", "hook_output": {}})
        body.append("sys.stdout.write(%r)" % json.dumps(verdict))
        relay.write_text("#!/usr/bin/env python3\n" + "\n".join(body) + "\n", encoding="utf-8")
        relay.chmod(0o755)
        document = {"configVersion": completion.CONFIG_VERSION, "event": "Stop",
                    "relayExecutable": str(relay), "markerRoot": str(root / "marker"),
                    "dbPath": None, "mode": "observe", "timeoutSeconds": 5,
                    "journalRoot": str(self.journal),
                    "journalPolicy": completion.EVERY_INVOCATION, "installedBy": "CRW-212",
                    "isolationAssertedBy": None, "owner": "plugin",
                    "adapterInterpreter": sys.executable, "adapterEntryPoint": str(PACKAGED)}
        self.settings = self.codex_home / completion.CONFIG_NAME
        self.settings.write_text(json.dumps(document, indent=2, sort_keys=True), encoding="utf-8")

    def declared(self):
        """The plugin's declared command, through a shell, the way the host runs it."""
        environment = {"PATH": os.environ["PATH"], "CODEX_HOME": str(self.codex_home),
                       "PLUGIN_ROOT": str(PLUGIN_ROOT)}
        return (["/bin/sh", "-c", declared_command()], environment)

    def checkout(self):
        """The user registration's command, as the installer writes it, started by this host:
        every registration inherits the host's environment, and so its Codex home."""
        return ([sys.executable, str(CHECKOUT), str(self.settings)],
                dict(os.environ, CODEX_HOME=str(self.codex_home)))

    def registration_with_its_own_root(self, name):
        """Another user registration of this host, whose own settings name another journal root."""
        journal = self.root / name
        document = json.loads(self.settings.read_text(encoding="utf-8"))
        document["journalRoot"] = str(journal)
        settings = self.root / (name + ".json")
        settings.write_text(json.dumps(document, indent=2, sort_keys=True), encoding="utf-8")
        return journal, ([sys.executable, str(CHECKOUT), str(settings)],
                         dict(os.environ, CODEX_HOME=str(self.codex_home)))

    def row_paths(self):
        return [entry for day in (sorted(self.journal.iterdir()) if self.journal.is_dir() else [])
                if day.is_dir() and DAY.match(day.name)
                for entry in sorted(day.iterdir()) if ROW.match(entry.name)]

    def calls_made(self):
        try:
            return len(self.calls.read_text(encoding="utf-8").splitlines())
        except FileNotFoundError:
            return 0

    def rows(self):
        found = []
        if self.journal.is_dir():
            for day in sorted(self.journal.iterdir()):
                if day.is_dir() and DAY.match(day.name):
                    for entry in sorted(day.iterdir()):
                        if ROW.match(entry.name):
                            found.append(json.loads(entry.read_text(encoding="utf-8")))
        return found

    def ledger(self):
        directory = self.journal / "accepted"
        names = sorted(p.name for p in directory.iterdir()) if directory.is_dir() else []
        return ([n for n in names if CLAIM.match(n)], [n for n in names if OUTCOME.match(n)])

    def acceptances(self):
        return sorted(str(row.get("acceptance")) for row in self.rows())


def fixture():
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


def at_stop(host, document, index):
    stop = document["stops"][index]
    lines = document["transcriptLines"][:stop["linesAtStop"]]
    host.transcript.write_text("".join(line + "\n" for line in lines), encoding="utf-8")
    payload = dict(stop["payload"])
    payload["transcript_path"] = str(host.transcript)
    payload["cwd"] = str(host.root)
    return json.dumps(payload).encode("utf-8")


def run_one(command, payload):
    argv, environment = command
    done = subprocess.run(argv, input=payload, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          env=environment, timeout=60)
    return done.returncode, done.stdout, done.stderr


def run_together(commands, payload):
    """Every command started before any is handed the payload, then fed at one barrier."""
    started = [subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=environment)
               for argv, environment in commands]
    answers = [None] * len(started)
    barrier = threading.Barrier(len(started))

    def feed(index):
        barrier.wait()
        answers[index] = started[index].communicate(payload, timeout=60)

    threads = [threading.Thread(target=feed, args=(i,)) for i in range(len(started))]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    return [(process.returncode, out, err) for process, (out, err) in zip(started, answers)]


class RealPathControls(unittest.TestCase):
    """The negative controls are red on an adapter that answers every invocation."""

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def assert_answered_once(self, host, answers):
        for code, _out, err in answers:
            self.assertEqual(code, 0)
            self.assertEqual(err, b"")
        self.assertEqual(host.calls_made(), 1, "one Stop event asked the guard more than once")
        self.assertEqual(host.acceptances(), ["accepted", "duplicate"])
        self.assertEqual(tuple(len(part) for part in host.ledger()), (1, 1))

    def test_the_declared_hook_twice_on_one_stop_asks_once(self):
        for round_number in range(10):
            with self.subTest(round=round_number):
                host = self.fresh("declared-%d" % round_number)
                payload = at_stop(host, self.document, 0)
                answers = run_together([host.declared(), host.declared()], payload)
                self.assert_answered_once(host, answers)

    def test_the_user_registration_replayed_asks_once(self):
        host = self.fresh("replay")
        payload = at_stop(host, self.document, 0)
        answers = [run_one(host.checkout(), payload), run_one(host.checkout(), payload)]
        self.assert_answered_once(host, answers)

    def test_the_user_registration_twice_at_once_asks_once(self):
        for round_number in range(10):
            with self.subTest(round=round_number):
                host = self.fresh("checkout-%d" % round_number)
                payload = at_stop(host, self.document, 0)
                answers = run_together([host.checkout(), host.checkout()], payload)
                self.assert_answered_once(host, answers)

    def test_the_plugin_and_the_user_registration_on_one_stop_ask_once(self):
        """The shape the 2026-09-20 journal carried: both registrations answering every Stop.
        The two are different copies of the adapter, so this is also the cross-copy claim."""
        for round_number in range(10):
            with self.subTest(round=round_number):
                host = self.fresh("both-%d" % round_number)
                payload = at_stop(host, self.document, 0)
                answers = run_together([host.declared(), host.checkout()], payload)
                self.assert_answered_once(host, answers)

    def test_every_stop_of_one_turn_is_accepted_and_its_hold_reaches_the_host(self):
        """The positive control, through both registrations. The guard holds on every call.

        The fixture's Stops 1 and 2 are two events of one turn and each is accepted once. Its
        Stop 3 reported the same text as Stop 2 under the same stop_hook_active, so a late
        delivery of Stop 2 would look exactly like it: that one is left unestablished and both
        registrations ask about it. Either way no continuation is suppressed."""
        host = self.fresh("positive", decision="block")
        stops = self.document["stops"]
        self.assertEqual(stops[2]["rawSha256"], stops[4]["rawSha256"])
        for index, answering in ((0, 1), (2, 1), (4, 2)):
            payload = at_stop(host, self.document, index)
            answers = run_together([host.declared(), host.checkout()], payload)
            held = [json.loads(out) for _code, out, _err in answers if out]
            self.assertEqual([h["decision"] for h in held], ["block"] * answering)
        self.assertEqual(host.calls_made(), 4)
        self.assertEqual(tuple(len(part) for part in host.ledger()), (2, 2))
        self.assertEqual(host.acceptances(),
                         ["accepted"] * 2 + ["duplicate"] * 2 + ["unestablished"] * 2)
        reasons = {(row.get("eventIdentity") or {}).get("reason") for row in host.rows()
                   if row.get("acceptance") == "unestablished"}
        self.assertEqual(reasons, {"answer_text_ambiguous"})

    def test_three_stops_that_report_different_answers_are_three_events(self):
        """The same fixture with Stop 3's answer (and the payload reporting it) changed to a text of
        its own: every Stop is told apart and each is accepted exactly once."""
        host = self.fresh("distinct", decision="block")
        document = distinct_third_answer(self.document)
        for index in (0, 2, 4):
            answers = run_together([host.declared(), host.checkout()],
                                   at_stop(host, document, index))
            self.assertEqual(len([out for _c, out, _e in answers if out]), 1)
        self.assertEqual(host.calls_made(), 3)
        self.assertEqual(host.acceptances(), ["accepted"] * 3 + ["duplicate"] * 3)


def written(document):
    """A record as the adapter's writers put it on disk: sorted keys, one line, a newline."""
    return json.dumps(document, sort_keys=True, default=str) + "\n"


def verify(*roots, **window):
    """The CLI, as an operator runs it, and the function it prints."""
    argv = [sys.executable, str(VERIFIER)]
    for root in roots:
        argv += ["--journal-root", str(root)]
    for name, value in window.items():
        argv += ["--" + name, value]
    done = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    return done.returncode, json.loads(done.stdout)


class VerifierTests(unittest.TestCase):
    """completion.stop_events and scripts/stop_events.py over journals the real paths wrote."""

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def turn_of_three(self, host):
        document = distinct_third_answer(self.document)
        for index in (0, 2, 4):
            run_together([host.declared(), host.checkout()], at_stop(host, document, index))

    def test_a_turn_of_three_stops_reads_true_where_the_old_count_read_duplicates(self):
        host = self.fresh("positive")
        self.turn_of_three(host)
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (0, "TRUE"))
        self.assertEqual(answer["events"], 3)
        self.assertEqual(answer["duplicateInvocations"], 3)
        self.assertEqual(answer["turnsWithMoreThanOneEvent"], 1)
        self.assertEqual(answer["supersededPerTurn"]["pairs"], 1)
        self.assertEqual(answer["supersededPerTurn"]["pairsWithMoreThanOneRow"], 1,
                         "the superseded per-turn count calls this one turn duplicated")
        self.assertEqual(completion.stop_events([host.journal])["verdict"], "TRUE")

    def test_an_event_whose_owner_died_before_answering_reads_unreadable(self):
        host = self.fresh("died", die_first=True)
        payload = at_stop(host, self.document, 0)
        run_one(host.declared(), payload)
        run_one(host.declared(), payload)
        self.assertEqual(host.calls_made(), 1)
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(len(answer["acceptedWithoutOutcome"]), 1)
        self.assertEqual(answer["eventsWithMoreThanOneAcceptance"], [])

    def test_one_event_accepted_in_two_journal_roots_reads_false_only_when_both_are_read(self):
        """Registrations that do not share a Codex home (two hosts handed one transcript, or an
        adapter from before host arbitration) can each accept the same Stop in their own roots;
        only a reading given both roots can see it, and that reading says FALSE."""
        first, second = self.fresh("first"), self.fresh("second")
        second.transcript = first.transcript
        payload = at_stop(first, self.document, 0)
        run_together([first.checkout(), second.checkout()], payload)
        self.assertEqual((verify(first.journal)[0], verify(second.journal)[0]), (0, 0))
        code, answer = verify(first.journal, second.journal)
        self.assertEqual((code, answer["verdict"]), (1, "FALSE"))
        self.assertEqual(len(answer["eventsWithMoreThanOneAcceptance"]), 1)

    def test_one_root_spelled_twice_is_read_once(self):
        host = self.fresh("spelled")
        self.turn_of_three(host)
        alias = self.base / "alias"
        alias.symlink_to(host.journal)
        code, answer = verify(host.journal, alias)
        self.assertEqual((code, answer["verdict"]), (0, "TRUE"))
        self.assertEqual(answer["roots"][1]["state"], "same_root_as_another_spelling")

    def test_invocations_without_an_identity_are_counted_and_keep_the_reading_from_true(self):
        host = self.fresh("unjudged")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        loose = json.dumps({"session_id": "s", "turn_id": "t", "stop_hook_active": False,
                            "last_assistant_message": "done"}).encode("utf-8")
        run_one(host.checkout(), loose)
        run_one(host.checkout(), loose)
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                         "two invocations were answered without being judged as events")
        self.assertEqual(answer["unjudgedInvocations"], {"unestablished:transcript_path_missing": 2})

    def test_rows_from_before_event_identity_are_legacy_and_never_judged(self):
        host = self.fresh("legacy")
        day = host.journal / "20260921"
        day.mkdir(parents=True)
        for index in range(2):
            (day / ("%032x.json" % index)).write_text(written(
                {"recordVersion": 1, "sessionId": "s", "turnId": "t", "at": "2026-09-21T00:00:00Z",
                 "stopHookActive": bool(index)}), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"), "nothing was judged")
        self.assertEqual(answer["legacyRows"], 2)
        self.assertEqual(answer["supersededPerTurn"]["pairsWithMoreThanOneRow"], 1)

    def test_a_torn_accepted_record_reads_unreadable(self):
        host = self.fresh("torn")
        self.turn_of_three(host)
        claim = host.journal / "accepted" / host.ledger()[0][0]
        claim.write_text("", encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["ledgerUnreadable"], [str(claim)])

    def test_an_outcome_with_no_claim_reads_unreadable(self):
        host = self.fresh("orphan")
        self.turn_of_three(host)
        key = "f" * 64
        (host.journal / "accepted" / (key + ".outcome.json")).write_text(
            written({"ledgerVersion": 1, "eventKey": key, "sessionId": "s", "turnId": "t",
                     "at": "2026-09-23T00:00:00Z", "adapterOutcome": "guard_answered",
                     "guardDecision": "release", "guardState": "unmanaged",
                     "journalPolicy": "faults_only", "held": False, "attemptRow": None}),
            encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["outcomesWithoutClaim"], [key])

    def test_an_accepted_row_whose_claim_is_gone_reads_false(self):
        """Acceptance that the ledger cannot account for is acceptance outside the ledger."""
        host = self.fresh("gone")
        self.turn_of_three(host)
        (host.journal / "accepted" / host.ledger()[0][0]).unlink()
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (1, "FALSE"))
        self.assertEqual(len(answer["acceptedRowsWithoutLedger"]), 1)


class ReviewRoundOneControls(unittest.TestCase):
    """Red-first controls for the classes the first review round found (CRW-212, PR #144).

    Each one is a way a REAL Stop event could be swallowed -- answered as a duplicate of another
    event -- or a way the verifier could vouch for a journal it did not understand. The oracle is
    always the guard's call log or the verifier's exit status, never a field name.
    """

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def test_a_late_retry_of_an_earlier_stop_does_not_take_the_next_stops_event(self):
        """Stop 2 and Stop 3 of the fixture report the same text. A retry of Stop 2 that arrives
        after Stop 3's answer is recorded cannot be told apart from Stop 3, so it must not claim
        Stop 3's event; if it did, Stop 3's own invocation would be answered as a duplicate."""
        host = self.fresh("late")
        stop_two = at_stop(host, self.document, 2)
        run_one(host.checkout(), stop_two)
        stop_three = at_stop(host, self.document, 4)
        run_one(host.checkout(), stop_two)
        before = host.calls_made()
        run_one(host.checkout(), stop_three)
        self.assertEqual(host.calls_made(), before + 1,
                         "Stop 3's own invocation was not asked about")

    def test_an_unfinished_last_line_is_not_read_past(self):
        """The host may be writing a line when the hook reads. A newer input cut off mid-write must
        not let the scan fall back to the previous answer and call a new Stop a duplicate."""
        host = self.fresh("tail")
        payload = at_stop(host, self.document, 0)
        run_one(host.checkout(), payload)
        prompt = self.document["transcriptLines"][self.document["stops"][0]["linesAtStop"]]
        self.assertIn("HookPrompt", prompt)
        with host.transcript.open("a", encoding="utf-8") as handle:
            handle.write(prompt[:len(prompt) // 2])
        run_one(host.checkout(), payload)
        self.assertEqual(host.calls_made(), 2, "a Stop behind an unfinished line was swallowed")

    def test_a_line_about_this_turn_that_does_not_parse_is_not_read_past(self):
        host = self.fresh("garbled")
        payload = at_stop(host, self.document, 0)
        run_one(host.checkout(), payload)
        turn = self.document["stops"][0]["payload"]["turn_id"]
        with host.transcript.open("a", encoding="utf-8") as handle:
            handle.write('{"type":"event_msg","payload":{"type":"item_completed","turn_id":"'
                         + turn + '","item":{"type":"UserMessage",\n')
        run_one(host.checkout(), payload)
        self.assertEqual(host.calls_made(), 2, "a Stop behind a garbled input line was swallowed")

    def test_an_answer_whose_thread_is_not_recorded_does_not_establish_an_event(self):
        host = self.fresh("threadless")
        stop = self.document["stops"][0]
        lines = list(self.document["transcriptLines"][:stop["linesAtStop"]])
        answer = json.loads(lines[-1])
        answer["payload"].pop("thread_id", None)
        lines[-1] = json.dumps(answer)
        host.transcript.write_text("".join(line + "\n" for line in lines), encoding="utf-8")
        payload = dict(stop["payload"], transcript_path=str(host.transcript), cwd=str(host.root))
        run_one(host.checkout(), json.dumps(payload).encode())
        run_one(host.checkout(), json.dumps(payload).encode())
        self.assertEqual(host.calls_made(), 2, "an unverified answer suppressed a Stop")

    def test_a_filter_does_not_hide_an_outcome_whose_claim_is_gone(self):
        """One complete event beside an outcome with no claim: unreadable with or without a window
        that covers the orphan."""
        host = self.fresh("orphan-filtered")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        stop = self.document["stops"][0]["payload"]
        key = "e" * 64
        (host.journal / "accepted" / (key + ".outcome.json")).write_text(written(
            {"ledgerVersion": 1, "eventKey": key, "sessionId": stop["session_id"],
             "turnId": stop["turn_id"], "at": "2026-09-23T00:00:00Z", "attemptRow": None,
             "adapterOutcome": "guard_answered", "guardDecision": "release",
             "guardState": "unmanaged", "journalPolicy": "faults_only", "held": False}),
            encoding="utf-8")
        for window in ({}, {"turn": stop["turn_id"]}, {"since": "2000-01-01T00:00:00Z"}):
            with self.subTest(window=window):
                code, answer = verify(host.journal, **window)
                self.assertNotEqual(code, 0, "a filtered reading vouched for an orphaned outcome")
                self.assertEqual(answer["outcomesWithoutClaim"], [key])

    def test_an_outcome_in_another_root_does_not_complete_a_claim(self):
        first, second = self.fresh("claim-root"), self.fresh("outcome-root")
        run_one(first.checkout(), at_stop(first, self.document, 0))
        claims, outcomes = first.ledger()
        (second.journal / "accepted").mkdir(parents=True)
        moved = first.journal / "accepted" / outcomes[0]
        (second.journal / "accepted" / outcomes[0]).write_bytes(moved.read_bytes())
        moved.unlink()
        code, answer = verify(first.journal, second.journal)
        self.assertNotEqual(code, 0, "an outcome in one root completed a claim in another")

    def test_a_version_two_row_the_verifier_cannot_classify_is_not_passed(self):
        host = self.fresh("malformed-row")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        day = next(p for p in host.journal.iterdir() if p.name.isdigit())
        (day / ("f" * 32 + ".json")).write_text(written(
            {"recordVersion": 2, "acceptance": "accepted", "sessionId": "s", "turnId": "t",
             "at": "2026-09-23T00:00:00Z"}), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertNotEqual(code, 0, "the verifier vouched for a row it did not understand")

    def test_invocations_without_an_identity_stay_visible_under_faults_only(self):
        host = self.fresh("faults-only")
        document = json.loads(host.settings.read_text(encoding="utf-8"))
        document["journalPolicy"] = completion.FAULTS_ONLY
        host.settings.write_text(json.dumps(document), encoding="utf-8")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        loose = json.dumps({"session_id": "s", "turn_id": "t", "stop_hook_active": False,
                            "last_assistant_message": "done"}).encode("utf-8")
        run_one(host.checkout(), loose)
        code, answer = verify(host.journal)
        self.assertEqual(sum(answer["unjudgedInvocations"].values()), 1,
                         "an invocation answered without an identity left no trace")

    def test_a_ledger_written_under_no_journal_is_not_vouched_for(self):
        host = self.fresh("no-journal")
        document = json.loads(host.settings.read_text(encoding="utf-8"))
        document["journalPolicy"] = completion.NO_JOURNAL
        host.settings.write_text(json.dumps(document), encoding="utf-8")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        loose = json.dumps({"session_id": "s", "turn_id": "t", "stop_hook_active": False,
                            "last_assistant_message": "done"}).encode("utf-8")
        run_one(host.checkout(), loose)
        code, answer = verify(host.journal)
        self.assertNotEqual(code, 0, "no rows were kept, so unidentified invocations are unseen")


class ReviewRoundTwoControls(unittest.TestCase):
    """Red-first controls for the second independent review of #144."""

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = distinct_third_answer(fixture())

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def accepted_turn(self, host):
        for index in (0, 2, 4):
            run_one(host.checkout(), at_stop(host, self.document, index))
        return host.calls_made()

    def test_a_late_retry_that_names_an_earlier_stop_is_that_stops_duplicate(self):
        """Stop 2 reported "DONE" and Stop 3 "DONE.". A retry of Stop 2 after Stop 3's answer is
        recorded can only be Stop 2, so it is Stop 2's duplicate and asks nothing."""
        host = self.fresh("late-distinct")
        before = self.accepted_turn(host)
        stop_two = dict(self.document["stops"][2]["payload"],
                        transcript_path=str(host.transcript), cwd=str(host.root))
        run_one(host.checkout(), json.dumps(stop_two).encode())
        self.assertEqual(host.calls_made(), before, "a retry of an accepted Stop asked again")

    def test_a_broken_line_naming_the_turn_is_not_read_past(self):
        host = self.fresh("broken")
        payload = at_stop(host, self.document, 0)
        run_one(host.checkout(), payload)
        turn = self.document["stops"][0]["payload"]["turn_id"]
        with host.transcript.open("a", encoding="utf-8") as handle:
            handle.write('{"type":"event_msg","payload":{"turn_id":"' + turn + '","x":\n')
        run_one(host.checkout(), payload)
        self.assertEqual(host.calls_made(), 2, "a Stop behind a broken line was swallowed")

    def outcome_path(self, host):
        return host.journal / "accepted" / host.ledger()[1][0]

    def test_an_outcome_that_disagrees_with_its_claim_is_not_vouched_for(self):
        host = self.fresh("disagrees")
        self.accepted_turn(host)
        path = self.outcome_path(host)
        body = json.loads(path.read_text(encoding="utf-8"))
        body["sessionId"] = "another-session"
        path.write_text(written(body), encoding="utf-8")
        self.assertNotEqual(verify(host.journal)[0], 0)

    def test_an_outcome_without_its_time_is_not_vouched_for(self):
        host = self.fresh("no-at")
        self.accepted_turn(host)
        path = self.outcome_path(host)
        body = json.loads(path.read_text(encoding="utf-8"))
        body.pop("at")
        path.write_text(written(body), encoding="utf-8")
        self.assertNotEqual(verify(host.journal)[0], 0)

    def test_an_accepted_row_that_is_gone_under_every_invocation_is_not_vouched_for(self):
        host = self.fresh("row-gone")
        self.accepted_turn(host)
        body = json.loads(self.outcome_path(host).read_text(encoding="utf-8"))
        (host.journal / body["attemptRow"]).unlink()
        self.assertNotEqual(verify(host.journal)[0], 0)

    def test_a_ledger_path_that_is_not_a_directory_is_not_an_empty_ledger(self):
        good, broken = self.fresh("good"), self.fresh("broken-ledger")
        self.accepted_turn(good)
        broken.journal.mkdir(parents=True)
        (broken.journal / "accepted").write_text("not a directory", encoding="utf-8")
        self.assertNotEqual(verify(good.journal, broken.journal)[0], 0)

    def test_a_row_version_this_reader_does_not_know_is_not_legacy(self):
        host = self.fresh("version")
        self.accepted_turn(host)
        day = next(p for p in host.journal.iterdir() if p.name.isdigit())
        row = next(day.iterdir())
        body = json.loads(row.read_text(encoding="utf-8"))
        body["recordVersion"] = 3
        row.write_text(written(body), encoding="utf-8")
        self.assertNotEqual(verify(host.journal)[0], 0)


class DevinRoundTwoControls(unittest.TestCase):
    """Red-first controls for Devin's second round on #144."""

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def test_a_transcript_that_does_not_reach_the_turns_start_establishes_nothing(self):
        """Without the turn's task_started the scan cannot know it saw every earlier Stop, and an
        unseen Stop with the same text is exactly what lets a late delivery take this event."""
        host = self.fresh("truncated")
        stop = self.document["stops"][2]
        lines = [line for line in self.document["transcriptLines"][:stop["linesAtStop"]]
                 if "task_started" not in line]
        lines = lines[lines.index(next(l for l in lines if "HookPrompt" in l)):]
        host.transcript.write_text("".join(line + "\n" for line in lines), encoding="utf-8")
        payload = dict(stop["payload"], transcript_path=str(host.transcript), cwd=str(host.root))
        run_one(host.checkout(), json.dumps(payload).encode())
        run_one(host.checkout(), json.dumps(payload).encode())
        self.assertEqual(host.calls_made(), 2, "a Stop on a truncated transcript was deduplicated")

    def test_a_ledger_record_of_another_version_is_not_vouched_for(self):
        host = self.fresh("ledger-version")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        claim = host.journal / "accepted" / host.ledger()[0][0]
        body = json.loads(claim.read_text(encoding="utf-8"))
        body["ledgerVersion"] = 9
        claim.write_text(written(body), encoding="utf-8")
        self.assertNotEqual(verify(host.journal)[0], 0)

    def test_a_window_does_not_hide_a_malformed_row(self):
        host = self.fresh("row-window")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        day = next(p for p in host.journal.iterdir() if p.name.isdigit())
        (day / ("d" * 32 + ".json")).write_text(written(
            {"recordVersion": 2, "acceptance": "accepted"}), encoding="utf-8")
        for window in ({}, {"since": "2000-01-01T00:00:00Z"},
                       {"session": self.document["stops"][0]["payload"]["session_id"]}):
            with self.subTest(window=window):
                self.assertNotEqual(verify(host.journal, **window)[0], 0)


class ReviewRoundThreeControls(unittest.TestCase):
    """Red-first controls for the third review round (CRW-212, PR #144).

    Two registrations of one host are one host answering one Stop, whatever journal root each one's
    settings name. And the reading says TRUE only about what it judged: an invocation answered
    without an event, a row from before event identity, a record whose own fields are not its key's
    or a ledger entry it does not know keeps the window from TRUE.
    """

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def claims_in(self, *journals):
        return [path for journal in journals if (journal / "accepted").is_dir()
                for path in sorted((journal / "accepted").iterdir()) if CLAIM.match(path.name)]

    def test_two_registrations_with_their_own_journal_roots_accept_one_stop_once(self):
        for round_number in range(10):
            with self.subTest(round=round_number):
                host = self.fresh("roots-%d" % round_number)
                other, command = host.registration_with_its_own_root("other-journal")
                answers = run_together([host.declared(), command], at_stop(host, self.document, 0))
                for code, _out, err in answers:
                    self.assertEqual((code, err), (0, b""))
                self.assertEqual(host.calls_made(), 1,
                                 "two registrations of one host both asked about one Stop")
                self.assertEqual(len(self.claims_in(host.journal, other)), 1,
                                 "one Stop was accepted in two journal roots")
                code, answer = verify(host.journal, other)
                self.assertEqual((code, answer["verdict"]), (0, "TRUE"))

    def test_a_duplicate_whose_accepted_record_is_in_no_root_read_is_not_vouched_for(self):
        host = self.fresh("half")
        other, command = host.registration_with_its_own_root("other-journal")
        payload = at_stop(host, self.document, 0)
        run_one(host.declared(), payload)
        run_one(command, payload)
        self.assertEqual(host.calls_made(), 1, "the second registration asked again")
        code, answer = verify(other)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(len(answer["duplicatesWithoutClaim"]), 1)

    def test_a_stop_answered_without_an_identity_keeps_the_reading_from_true(self):
        """The fixture's third Stop cannot be told from a late delivery of its second, so both
        registrations asked about it: that Stop was answered twice, and a reading that cannot see
        it as one event does not vouch for the window it is in."""
        host = self.fresh("ambiguous", decision="block")
        for index in (0, 2, 4):
            run_together([host.declared(), host.checkout()], at_stop(host, self.document, index))
        self.assertEqual(host.calls_made(), 4)
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["unjudgedInvocations"],
                         {"unestablished:answer_text_ambiguous": 2})
        self.assertEqual(answer["events"], 2)

    def test_rows_from_before_event_identity_keep_their_window_from_true(self):
        host = self.fresh("mixed")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        day = host.journal / "20000101"
        day.mkdir(parents=True)
        (day / ("%032x.json" % 7)).write_text(written(
            {"recordVersion": 1, "sessionId": "s", "turnId": "t", "at": "2000-01-01T00:00:00Z",
             "stopHookActive": False}), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["legacyRows"], 1)
        code, answer = verify(host.journal, since="2001-01-01T00:00:00Z")
        self.assertEqual((code, answer["verdict"]), (0, "TRUE"),
                         "a window after the legacy row judges everything in it")

    def test_a_claim_whose_answer_is_not_its_keys_is_not_vouched_for(self):
        host = self.fresh("claim-item")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        claim = host.journal / "accepted" / host.ledger()[0][0]
        body = json.loads(claim.read_text(encoding="utf-8"))
        body["answerItem"] = "msg_another_answer"
        claim.write_text(written(body), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["ledgerUnreadable"], [str(claim)])

    def test_an_accepted_row_about_another_session_is_not_vouched_for(self):
        host = self.fresh("row-session")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        [path] = host.row_paths()
        row = json.loads(path.read_text(encoding="utf-8"))
        row["sessionId"] = "01a0cd4a-0000-7000-8000-000000000000"
        path.write_text(written(row), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["rowsUnreadable"], [str(path)])

    def test_a_duplicate_row_about_another_turn_is_not_vouched_for(self):
        host = self.fresh("row-turn")
        payload = at_stop(host, self.document, 0)
        run_one(host.checkout(), payload)
        run_one(host.checkout(), payload)
        [path] = [p for p in host.row_paths()
                  if json.loads(p.read_text(encoding="utf-8")).get("acceptance") == "duplicate"]
        row = json.loads(path.read_text(encoding="utf-8"))
        row["turnId"] = "01a0cd4a-0000-7000-8000-000000000001"
        path.write_text(written(row), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["rowsUnreadable"], [str(path)])

    def test_an_entry_in_the_ledger_this_reader_does_not_know_is_not_vouched_for(self):
        host = self.fresh("foreign")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        stray = host.journal / "accepted" / "pending.json.tmp"
        stray.write_text("", encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["foreignLedgerEntries"], [str(stray)])


def host_ledger_of(host):
    return host.codex_home / "crw-completion-hook" / "stop-events"


class ReviewRoundFourControls(unittest.TestCase):
    """Red-first controls for the fourth review round (CRW-212, PR #144).

    Only the invocation that made the host's file for an event may ask the guard about it, so a host
    whose file cannot be made asks nobody. And the reading judges an event as a unit: every record
    of an event the window reaches is checked whatever its own time, every host file must be
    accounted for by a claim, and every claim by its host file.
    """

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()

    def fresh(self, name, **kwargs):
        root = self.base / name
        root.mkdir()
        return Host(root, **kwargs)

    def rows_of(self, *journals):
        return [json.loads(entry.read_text(encoding="utf-8")) for journal in journals
                if journal.is_dir() for day in sorted(journal.iterdir())
                if day.is_dir() and DAY.match(day.name)
                for entry in sorted(day.iterdir()) if ROW.match(entry.name)]

    def test_a_host_file_that_cannot_be_made_asks_the_guard_about_nothing(self):
        for round_number in range(5):
            with self.subTest(round=round_number):
                host = self.fresh("blocked-%d" % round_number, decision="block")
                other, command = host.registration_with_its_own_root("other-journal")
                blocked = host_ledger_of(host)
                blocked.parent.mkdir(parents=True)
                blocked.write_text("not a directory", encoding="utf-8")
                answers = run_together([host.declared(), command], at_stop(host, self.document, 0))
                self.assertEqual([out for _code, out, _err in answers], [b"", b""],
                                 "a registration that owns nothing answered the host")
                self.assertEqual(host.calls_made(), 0,
                                 "a Stop nobody could own was asked about")
                self.assertEqual(sorted(str(row.get("acceptance"))
                                        for row in self.rows_of(host.journal, other)),
                                 ["unarbitrated", "unarbitrated"])
                code, answer = verify(host.journal, other)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))

    def test_settings_without_a_journal_root_still_meet_at_the_host_file(self):
        host = self.fresh("no-root")
        document = json.loads(host.settings.read_text(encoding="utf-8"))
        document.pop("journalRoot")
        host.settings.write_text(json.dumps(document), encoding="utf-8")
        run_together([host.declared(), host.checkout()], at_stop(host, self.document, 0))
        self.assertEqual(host.calls_made(), 1, "two registrations of one host both asked")

    def test_an_owner_that_died_between_the_host_file_and_its_claim_is_not_vouched_for(self):
        """The host's file for the fixture's second Stop, as an owner killed right after making it
        leaves it: no claim, no row. Beside a complete first event the reading must not say TRUE."""
        host = self.fresh("died-between")
        document = distinct_third_answer(self.document)
        run_one(host.checkout(), at_stop(host, document, 0))
        stop = json.loads(at_stop(host, document, 2))
        key, identity = completion.event_identity(stop)
        self.assertIsNotNone(key)
        marker = host_ledger_of(host) / (key + ".json")
        marker.write_text(written(
            {"ledgerVersion": 1, "eventKey": key, "sessionId": stop["session_id"],
             "turnId": stop["turn_id"], "stopHookActive": stop["stop_hook_active"],
             "answerItem": identity["answerItem"], "claimedAt": "2026-09-23T00:00:00Z",
             "claimedBy": {"pid": 1, "journalRoot": str(host.journal),
                           "attemptRow": "20260923/" + "0" * 32 + ".json"}}), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(len(answer["hostFilesWithoutClaim"]), 1)

    def test_an_outcome_outside_the_window_is_still_checked_against_its_claim(self):
        host = self.fresh("split")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        outcome = host.journal / "accepted" / host.ledger()[1][0]
        body = json.loads(outcome.read_text(encoding="utf-8"))
        body["at"] = "2000-01-01T00:00:00Z"
        body["sessionId"] = "wrong-session"
        outcome.write_text(written(body), encoding="utf-8")
        code, answer = verify(host.journal, since="2020-01-01T00:00:00Z")
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["ledgerUnreadable"], [str(outcome)])

    def test_a_claim_whose_host_file_is_gone_is_not_vouched_for(self):
        host = self.fresh("host-file-gone")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        [marker] = sorted(host_ledger_of(host).iterdir())
        marker.unlink()
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(len(answer["claimsWithoutHostFile"]), 1)

    def test_an_accepted_row_without_its_invocation_fields_is_not_vouched_for(self):
        host = self.fresh("thin-row")
        run_one(host.checkout(), at_stop(host, self.document, 0))
        [path] = host.row_paths()
        row = json.loads(path.read_text(encoding="utf-8"))
        for field in ("guardInvoked", "adapterOutcome", "acceptedAs"):
            row.pop(field)
        path.write_text(written(row), encoding="utf-8")
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
        self.assertEqual(answer["rowsUnreadable"], [str(path)])


class OneEventRecords:
    """One real event's records, written through both registrations, for the classes below."""

    SLOT = "20000101/" + "0" * 32 + ".json"

    def setUp(self):
        raw = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, raw, True)
        self.base = Path(raw)
        self.document = fixture()
        self.count = 0

    def one_event(self):
        """One Stop through both registrations of one root: host file, claim, outcome, an accepted
        row and a duplicate row, all written by the real paths."""
        self.count += 1
        root = self.base / ("event-%d" % self.count)
        root.mkdir()
        host = Host(root)
        payload = at_stop(host, self.document, 0)
        run_one(host.declared(), payload)
        run_one(host.checkout(), payload)
        rows = {json.loads(p.read_text(encoding="utf-8"))["acceptance"]: p
                for p in host.row_paths()}
        records = {"host": next(host_ledger_of(host).iterdir()),
                   "claim": host.journal / "accepted" / host.ledger()[0][0],
                   "outcome": host.journal / "accepted" / host.ledger()[1][0],
                   "accepted": rows["accepted"], "duplicate": rows["duplicate"]}
        return host, records

    def change(self, path, change):
        body = json.loads(path.read_text(encoding="utf-8"))
        change(body)
        path.write_text(written(body), encoding="utf-8")


class ReviewRoundFiveControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the fifth review round (CRW-212, PR #144).

    The records of one event are written by one owner in one run, so every reference between them
    resolves to the record it names and every value two of them carry agrees: the owner's slot and
    pid in the host file and the claim, the outcome's slot and guard result against its accepted
    row, the accepted row's place, and the claim a duplicate names. One value changed in any of
    them, and the reading does not vouch for the event.
    """

    def test_a_complete_event_reads_true(self):
        host, _records = self.one_event()
        self.assertEqual(verify(host.journal)[:1] + (verify(host.journal)[1]["verdict"],),
                         (0, "TRUE"))

    def test_records_of_one_event_that_disagree_are_not_vouched_for(self):
        slot = self.SLOT
        cases = [
            ("claim names another slot", "claim",
             lambda b: b["claimedBy"].__setitem__("attemptRow", slot)),
            ("host file names another slot", "host",
             lambda b: b["claimedBy"].__setitem__("attemptRow", slot)),
            ("host file names another owner", "host",
             lambda b: b["claimedBy"].__setitem__("pid", b["claimedBy"]["pid"] + 1)),
            ("outcome names another slot", "outcome", lambda b: b.__setitem__("attemptRow", slot)),
            ("outcome held what the row released", "outcome", lambda b: b.__setitem__("held", True)),
            ("outcome decided otherwise", "outcome",
             lambda b: b.__setitem__("guardDecision", "block")),
            ("outcome saw another state", "outcome",
             lambda b: b.__setitem__("guardState", "declared")),
            ("outcome ended otherwise", "outcome",
             lambda b: b.__setitem__("adapterOutcome", "guard_timed_out")),
            ("accepted row calls itself a duplicate", "accepted",
             lambda b: b.__setitem__("adapterOutcome", "duplicate_invocation")),
            ("accepted row held without an answer", "accepted",
             lambda b: (b.__setitem__("held", True),
                        b.__setitem__("adapterOutcome", "guard_timed_out"))),
            ("duplicate row printed a hold", "duplicate", lambda b: b.__setitem__("held", True)),
        ]
        for label, which, change in cases:
            with self.subTest(case=label):
                host, records = self.one_event()
                self.change(records[which], change)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 label + " was vouched for")

    def test_a_duplicate_naming_a_claim_its_own_root_does_not_hold_is_not_vouched_for(self):
        root = self.base / "two-roots"
        root.mkdir()
        host = Host(root)
        other, command = host.registration_with_its_own_root("other-journal")
        payload = at_stop(host, self.document, 0)
        run_one(host.declared(), payload)
        run_one(command, payload)
        [duplicate] = [p for day in sorted(other.iterdir()) if DAY.match(day.name)
                       for p in sorted(day.iterdir()) if ROW.match(p.name)]
        self.change(duplicate, lambda b: b.__setitem__("acceptedAs",
                                                        "accepted/" + b["eventKey"] + ".json"))
        code, answer = verify(host.journal, other)
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))


class ReviewRoundSixControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the sixth review round and Devin's sixth (CRW-212, PR #144).

    Records that agree with each other can still describe something the adapter never writes. Each
    record is checked against what run() can write: only a guard's answer carries a decision, and it
    holds exactly when it blocks; a duplicate or an unowned release asked nothing and carries no
    guard result; and the owner's policy decides whether its accepted row exists.
    """

    def test_values_the_runtime_cannot_write_are_not_vouched_for_even_when_they_agree(self):
        for field, value in (("adapterOutcome", "invented"), ("guardDecision", "nonsense")):
            with self.subTest(field=field):
                host, records = self.one_event()
                for which in ("accepted", "outcome"):
                    self.change(records[which], lambda b: b.__setitem__(field, value))
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 field + "=" + value + " was vouched for")

    def test_combinations_the_runtime_cannot_write_are_not_vouched_for(self):
        both = ("accepted", "outcome")
        cases = [
            ("a release that held", both, {"held": True}),
            ("a block that did not hold", both, {"guardDecision": "block"}),
            ("an answer under faults_only beside its row", ("outcome",),
             {"journalPolicy": "faults_only"}),
            ("a duplicate carrying a decision", ("duplicate",), {"guardDecision": "block"}),
            ("a duplicate carrying a guard state", ("duplicate",), {"guardState": "declared"}),
            ("a duplicate carrying a process ending", ("duplicate",), {"processEnding": "exited"}),
        ]
        for label, which, changes in cases:
            with self.subTest(case=label):
                host, records = self.one_event()
                for kind in which:
                    self.change(records[kind], lambda b: b.update(changes))
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 label + " was vouched for")

    def test_a_record_missing_a_field_it_is_written_with_is_read_and_not_vouched_for(self):
        """Found by a sweep over every field of every record: a missing field is unreadable, and the
        reader never stops on one."""
        cases = [("outcome", ("attemptRow",)), ("host", ("claimedBy", "journalRoot")),
                 ("duplicate", ("guardDecision",)), ("duplicate", ("processEnding",)),
                 ("accepted", ("event",)), ("accepted", ("configuration",))]
        for which, path in cases:
            with self.subTest(record=which, field=".".join(path)):
                host, records = self.one_event()
                self.change(records[which], lambda b: (b[path[0]] if len(path) > 1 else b).pop(
                    path[-1]))
                code, answer = verify(host.journal, **{"codex-home": str(host.codex_home)})
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
                self.assertNotIn("readerFault", answer)

    def test_a_row_that_asked_nothing_carries_no_receipt(self):
        for field in ("assignmentId", "guardRecordedAs"):
            with self.subTest(field=field):
                host, records = self.one_event()
                self.change(records["duplicate"], lambda b: b.__setitem__(field, "receipt"))
                self.assertEqual(verify(host.journal)[0], 3)

    def test_a_guard_outcome_that_does_not_follow_from_its_call_is_not_vouched_for(self):
        cases = [("processEnding", "signalled"), ("stdoutReading", "said_nothing"),
                 ("exitCode", 3), ("signal", 9)]
        for field, value in cases:
            with self.subTest(field=field):
                host, records = self.one_event()
                self.change(records["accepted"], lambda b: b.__setitem__(field, value))
                self.assertEqual(verify(host.journal)[0], 3, field + " was vouched for")


class ReviewRoundSevenControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the seventh review round (CRW-212, PR #144).

    A fault keeps whatever run() had recorded before it, in run()'s order: the guard is marked as
    asked, then the call's fields are recorded, then an answer's. A faulted record is one of those
    prefixes and nothing else. And a version field is the integer the adapter writes, not a value
    that merely compares equal to it.
    """

    def faulted(self, records, row_changes):
        def fault(body, extra):
            body["adapterOutcome"] = "adapter_faulted"
            body["held"] = False
            body.update(extra)
        self.change(records["accepted"],
                    lambda b: fault(b, dict({"fault": "OSError: injected"}, **row_changes)))
        self.change(records["outcome"], lambda b: fault(b, {}))

    def test_a_fault_after_the_answer_is_still_one_event_accepted_once(self):
        host, records = self.one_event()
        self.faulted(records, {})
        code, answer = verify(host.journal)
        self.assertEqual((code, answer["verdict"]), (0, "TRUE"),
                         "a prefix run() writes was not vouched for")

    def test_a_fault_that_is_not_a_prefix_of_runs_order_is_not_vouched_for(self):
        cases = [
            ("not asked, yet called and answered", {"guardInvoked": False}),
            ("answered without a call", {"processEnding": None, "stdoutReading": None,
                                         "exitCode": None}),
            ("called without the call's fields", {"__drop__": "guardStderr"}),
        ]
        for label, changes in cases:
            with self.subTest(case=label):
                host, records = self.one_event()
                drop = changes.pop("__drop__", None)
                self.faulted(records, changes)
                if drop:
                    self.change(records["accepted"], lambda b: b.pop(drop))
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 label + " was vouched for")

    def test_a_version_the_adapter_does_not_write_is_not_vouched_for(self):
        cases = [("host", "ledgerVersion", True), ("claim", "ledgerVersion", 1.0),
                 ("outcome", "ledgerVersion", True), ("accepted", "recordVersion", 2.0),
                 ("duplicate", "recordVersion", 2.0)]
        for which, field, value in cases:
            with self.subTest(record=which, value=repr(value)):
                host, records = self.one_event()
                self.change(records[which], lambda b: b.__setitem__(field, value))
                code, answer = verify(host.journal, **{"codex-home": str(host.codex_home)})
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 which + "." + field + "=" + repr(value) + " was vouched for")


class ReviewRoundEightControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the eighth review round and Devin's fractional bound (CRW-212).

    A time the adapter writes is a real UTC second in now()'s format, and a day directory is a real
    date: a record, a slot or a directory naming an impossible one was not written by the adapter.
    A window bound is a time in the records' own format, so it compares with them correctly.
    """

    IMPOSSIBLE = "2026-99-99T99:99:99Z"

    def test_an_impossible_time_is_not_vouched_for(self):
        for which, field in (("host", "claimedAt"), ("claim", "claimedAt"), ("outcome", "at"),
                             ("accepted", "at"), ("duplicate", "at")):
            with self.subTest(record=which):
                host, records = self.one_event()
                self.change(records[which], lambda b: b.__setitem__(field, self.IMPOSSIBLE))
                code, answer = verify(host.journal, **{"codex-home": str(host.codex_home)})
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 which + "." + field + " was vouched for")

    def test_a_row_under_an_impossible_day_is_not_vouched_for(self):
        host, records = self.one_event()
        day = host.journal / "99999999"
        day.mkdir()
        moved = day / records["accepted"].name
        moved.write_text(records["accepted"].read_text(encoding="utf-8"), encoding="utf-8")
        records["accepted"].unlink()
        slot = "99999999/" + moved.name
        self.change(records["claim"], lambda b: b["claimedBy"].__setitem__("attemptRow", slot))
        self.change(records["host"], lambda b: b["claimedBy"].__setitem__("attemptRow", slot))
        self.change(records["outcome"], lambda b: b.__setitem__("attemptRow", slot))
        code, answer = verify(host.journal, **{"codex-home": str(host.codex_home)})
        self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))

    def test_a_window_bound_not_in_the_records_format_is_refused(self):
        host, _records = self.one_event()
        done = subprocess.run([sys.executable, str(VERIFIER), "--journal-root", str(host.journal),
                               "--since", "2026-09-23T11:58:17.500Z"],
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
        self.assertEqual(done.returncode, 2, "a fractional bound was accepted as a window")
        with self.assertRaises(ValueError):
            completion.stop_events([host.journal], until="2026-09-23 11:58:17")


class ReviewRoundNineControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the ninth review round (CRW-212, PR #144).

    run() records in stages: the settings path, then, once the payload is read, its session, turn
    and flag together with the settings' mode, then the identity. Each value a stage writes is
    pinned by what run() does there -- a mode from the settings, an established identity's absolute
    transcript path, an absolute settings path, a release's detail, a call's detail exactly when
    the process neither exited nor was signalled -- and a row carrying another is not one it wrote.
    """

    def test_values_a_stage_pins_are_not_vouched_for_otherwise(self):
        cases = [
            ("accepted", "guardMode", None), ("duplicate", "guardMode", None),
            ("accepted", ("eventIdentity", "transcriptPath"), None),
            ("accepted", ("eventIdentity", "transcriptPath"), "rollout.jsonl"),
            ("accepted", "configuration", "crw-completion-hook.json"),
            ("duplicate", "detail", None),
            ("accepted", "detail", "the guard did not answer"),
        ]
        for which, field, value in cases:
            with self.subTest(record=which, field=str(field), value=repr(value)):
                host, records = self.one_event()
                def change(body):
                    target = body[field[0]] if isinstance(field, tuple) else body
                    target[field[-1] if isinstance(field, tuple) else field] = value
                self.change(records[which], change)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 which + "." + str(field) + "=" + repr(value) + " was vouched for")

    def test_an_unestablished_row_carries_only_what_its_reason_had_recorded(self):
        """event_identity() stops at its first failure: a reason it never gives, a path before the
        path was looked at, a relative path under any other reason, or an answer item before one was
        found is not a row it wrote, so it is not counted as an invocation left unjudged."""
        cases = [
            ("made_up", None, None, False), ("transcript_path_missing", "/x/rollout.jsonl", None, False),
            ("transcript_absent", None, None, False), ("transcript_absent", "/x/rollout.jsonl", "m", False),
            ("transcript_path_relative", "/x/rollout.jsonl", None, False),
            ("session_mismatch", "/x/rollout.jsonl", None, False),
            ("transcript_absent", "/x/rollout.jsonl", None, True),
            ("session_mismatch", "/x/rollout.jsonl", "m", True),
        ]
        for reason, path, item, written in cases:
            with self.subTest(reason=reason, path=path, item=item):
                self.count += 1
                root = self.base / ("unestablished-%d" % self.count)
                root.mkdir()
                host = Host(root)
                loose = json.dumps({"session_id": "s", "turn_id": "t", "stop_hook_active": False,
                                    "last_assistant_message": "done"}).encode("utf-8")
                run_one(host.checkout(), loose)
                def change(body):
                    body["eventIdentity"].update(reason=reason, transcriptPath=path, answerItem=item)
                self.change(host.row_paths()[0], change)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"))
                counted = {"unestablished:" + reason: 1} if written else {}
                self.assertEqual(answer["unjudgedInvocations"], counted,
                                 "a row event_identity() cannot write was counted as one it wrote"
                                 if not written else "a row it writes was not counted")


class ReviewRoundTenControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the tenth review round (CRW-212, PR #144).

    Each writer puts a fixed set of fields in its record: run() its row, with a fault and the
    journal's answer only on the fault path; claim_event() and _arbitrate() their files;
    record_outcome() the outcome; event_identity() the identity. A record carrying a field its
    writer never puts on that path is not one it wrote, whatever the other fields say.
    """

    def test_a_field_its_writer_never_puts_there_is_not_vouched_for(self):
        cases = [
            ("accepted", (), "fault", "RuntimeError: impossible"),
            ("duplicate", (), "fault", "RuntimeError: impossible"),
            ("accepted", (), "journalledAs", None),
            ("accepted", (), "unknownField", 1),
            ("duplicate", (), "unknownField", None),
            ("accepted", ("eventIdentity",), "unknownField", None),
            ("claim", (), "adapterOutcome", "guard_answered"),
            ("claim", ("claimedBy",), "journalRoot", None),
            ("outcome", (), "fault", "RuntimeError: impossible"),
            ("outcome", (), "answerItem", "m"),
            ("host", (), "held", False),
            ("host", ("claimedBy",), "hostLedger", "/x"),
        ]
        for which, inside, field, value in cases:
            where = ".".join((which,) + inside + (field,))
            with self.subTest(field=where):
                host, records = self.one_event()
                def change(body):
                    target = body
                    for part in inside:
                        target = target[part]
                    target[field] = value
                self.change(records[which], change)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 where + "=" + repr(value) + " was vouched for")


class ReviewRoundElevenControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the eleventh review round (CRW-212, PR #144).

    journal() writes rows only as <32 hex>.json files in day directories, and the claims and
    outcomes go under accepted/. Anything else in a journal root or a day directory is nothing the
    adapter wrote, and a reading that skipped it would pass a second copy of an accepted row kept
    under another name.
    """

    def test_an_entry_the_adapter_never_writes_is_reported_not_skipped(self):
        def hidden(row):
            return [row.parent / "hidden.json"]
        def backup(row):
            return [row.parent / (row.name + ".bak")]
        def nested(row):
            (row.parent / "sub").mkdir()
            return [row.parent / "sub" / row.name]
        def row_named_directory(row):
            (row.parent / ("0" * 32 + ".json")).mkdir()
            return []
        def copied_day(row):
            day = row.parent.parent / (row.parent.name + ".bak")
            day.mkdir()
            return [day / row.name]
        def stray_root_file(row):
            return [row.parent.parent / "stray.json"]
        cases = [hidden, backup, nested, row_named_directory, copied_day, stray_root_file]
        for place in cases:
            with self.subTest(case=place.__name__):
                host, records = self.one_event()
                row = records["accepted"]
                for target in place(row):
                    shutil.copyfile(row, target)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 place.__name__ + " was skipped and the reading vouched for")
                self.assertTrue(answer["foreignJournalEntries"], place.__name__ + " was not listed")


class ReviewRoundTwelveControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the twelfth review round (CRW-212, PR #144).

    A record is what the adapter's create produced: a regular file made with O_EXCL, which never
    makes or follows a link, in directories mkdir made, holding exactly the bytes json.dumps with
    sorted keys and a newline gives for its content. Anything else in a record's place was put
    there by something other than the adapter, whatever it parses to.
    """

    WHICH = ("accepted", "duplicate", "claim", "outcome", "host")

    def test_a_record_that_is_a_link_is_not_vouched_for(self):
        for which in self.WHICH:
            with self.subTest(record=which):
                host, records = self.one_event()
                elsewhere = self.base / ("elsewhere-%d.json" % self.count)
                shutil.copyfile(records[which], elsewhere)
                records[which].unlink()
                records[which].symlink_to(elsewhere)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 which + " replaced by a link was vouched for")

    def test_a_linked_directory_is_not_vouched_for(self):
        for which in ("day", "accepted"):
            with self.subTest(directory=which):
                host, records = self.one_event()
                real = records["accepted"].parent if which == "day" else records["claim"].parent
                moved = self.base / ("moved-%d" % self.count)
                real.rename(moved)
                real.symlink_to(moved, target_is_directory=True)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 "a linked " + which + " directory was vouched for")

    def test_bytes_its_writer_never_produces_are_not_vouched_for(self):
        def indented(body):
            return json.dumps(body, sort_keys=True, indent=1) + "\n"
        def unsorted(body):
            return json.dumps(dict(reversed(list(body.items())))) + "\n"
        def no_newline(body):
            return written(body)[:-1]
        def duplicate_key(body):
            first = sorted(body)[0]
            return '{"' + first + '": "decoy", ' + written(body)[1:]
        for which in self.WHICH:
            for form in (indented, unsorted, no_newline, duplicate_key):
                with self.subTest(record=which, form=form.__name__):
                    host, records = self.one_event()
                    body = json.loads(records[which].read_text(encoding="utf-8"))
                    records[which].write_text(form(body), encoding="utf-8")
                    code, answer = verify(host.journal)
                    self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                     which + " in " + form.__name__ + " bytes was vouched for")

    def test_a_record_in_its_writers_own_bytes_still_reads_true(self):
        for which in self.WHICH:
            with self.subTest(record=which):
                host, records = self.one_event()
                body = json.loads(records[which].read_text(encoding="utf-8"))
                records[which].write_text(written(body), encoding="utf-8")
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (0, "TRUE"),
                                 which + " rewritten in its writer's own bytes was refused")


class ReviewRoundThirteenControls(OneEventRecords, unittest.TestCase):
    """Red-first controls for the thirteenth review round (CRW-212, PR #144).

    Every path a record carries has the form its writer gives it: the host ledger a claim names is
    os.path.abspath of the Codex home joined with crw-completion-hook/stop-events, the settings path
    a row names is _settled() (absolute and normalized), and the journal root a host file names is
    the settings' own, which they require to be absolute.
    """

    def test_a_path_in_a_form_its_writer_never_gives_is_not_vouched_for(self):
        def relative(value):
            return os.path.relpath(value, os.getcwd())
        def dotted(value):
            head, tail = os.path.split(value)
            return head + "/./" + tail
        def copied_ledger(value):
            copy = self.base / ("ledger-copy-%d" % self.count)
            shutil.copytree(value, copy)
            return str(copy)
        cases = [
            ("claim", ("claimedBy", "hostLedger"), relative),
            ("claim", ("claimedBy", "hostLedger"), dotted),
            ("claim", ("claimedBy", "hostLedger"), copied_ledger),
            ("host", ("claimedBy", "journalRoot"), relative),
            ("accepted", ("configuration",), dotted),
            ("duplicate", ("configuration",), dotted),
        ]
        for which, field, form in cases:
            where = ".".join((which,) + field)
            with self.subTest(field=where, form=form.__name__):
                host, records = self.one_event()
                def change(body):
                    target = body
                    for part in field[:-1]:
                        target = target[part]
                    target[field[-1]] = form(target[field[-1]])
                self.change(records[which], change)
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 where + " in " + form.__name__ + " form was vouched for")

    def test_a_release_that_always_says_one_thing_says_nothing_else(self):
        """A duplicate and an unowned release carry run()'s one sentence for them."""
        for value in ("", "the guard was not asked", None):
            with self.subTest(value=value):
                host, records = self.one_event()
                self.change(records["duplicate"], lambda body: body.__setitem__("detail", value))
                code, answer = verify(host.journal)
                self.assertEqual((code, answer["verdict"]), (3, "UNREADABLE"),
                                 "a duplicate saying %r was vouched for" % (value,))
