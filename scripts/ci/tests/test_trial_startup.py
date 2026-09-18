#!/usr/bin/env python3
"""What the live-trial preflight has to be true of, asserted against runs rather than declarations.

The module under test reports that every reading was met. That is the sentence most worth
distrusting, because the arrangement has a silent failure that produces it: hand it payloads that
carry none of the fields its predicates read and every cell answers unknown, which is not a pass but
is also not a crash. So this module asserts the states themselves, and it writes the inventories out
here rather than importing the module's own declarations. A checker that quietly changed what it
expected would otherwise change this check with it.

Nothing here needs a host. The relay is a stub launcher this test writes, the host record is one it
writes beside it, the git repositories are real but empty, and the supervisor is a real background
process this test starts and stops. The one thing it does not stub is the field names: the payload
contract test parses the relay's own source and asserts that every field name the module reads is
still there, so a stub that invented a field cannot make this suite pass.

The trial root has to sit outside every git worktree, because that is what the module refuses. On a
host whose temporary directory is itself a checkout, set CRW_TRIAL_TMPDIR to a clean path.
"""

import ast
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

import trial_startup as startup  # noqa: E402

READINGS = ("processPersistence", "parentLifecycle", "capability", "storeIdentity", "boundaries",
            "assignmentState")

VERIFIED, NOT_VERIFIED, UNKNOWN, NOT_APPLICABLE = (
    "verified", "not_verified", "unknown", "not_applicable")

# Every relay subcommand the module may compose, written here rather than imported.
ALLOWED = ("doctor", "assignment-find", "criteria-show", "settings-show", "service status")

LAUNCHER = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path

here = Path(__file__).resolve().parent
payloads = json.loads((here / "payloads.json").read_text())
argv = sys.argv[1:]
words = [a for a in argv if not a.startswith("--")]
# --state and --socket each take a value, so the subcommand is the first bare word after them.
subcommand = words[2] if len(words) > 2 else (words[-1] if words else "")
if subcommand == "service":
    subcommand = "service " + (words[3] if len(words) > 3 else "")
with (here / "calls.jsonl").open("a") as handle:
    handle.write(json.dumps({"subcommand": subcommand, "argv": sys.argv[1:]}) + "\n")
# A trial can ask this stub to replace itself partway through, which is what an update moving the
# pointer looks like from the caller's side.
rewrite = here / "rewrite-after"
if rewrite.exists():
    calls = len((here / "calls.jsonl").read_text().splitlines())
    if calls == int(rewrite.read_text().strip()):
        mine = Path(__file__)
        mine.write_text(mine.read_text() + "\n# a different build\n")
entry = payloads.get(subcommand)
if entry is None:
    sys.stderr.write("no payload for " + subcommand + "\n")
    raise SystemExit(9)
payload = entry.get("payload", {})
if subcommand == "settings-show" and isinstance(payload, dict) and "--task" in argv:
    # The real command answers about the task it was asked about, so a stub that always answered
    # about one task would hide a checker that never compared the identity.
    payload = dict(payload, task=argv[argv.index("--task") + 1])
if entry.get("stdout") is not None:
    sys.stdout.write(entry["stdout"])
else:
    sys.stdout.write(json.dumps(payload))
raise SystemExit(entry.get("exit", 0))
'''

WITNESS_WRITER = (
    "import json, os, sys, time\n"
    "path = sys.argv[1]\n"
    "n = 0\n"
    "while True:\n"
    "    n += 1\n"
    "    with open(path, 'a') as handle:\n"
    "        handle.write(json.dumps({'pid': os.getpid(), 'progress': n}) + '\\n')\n"
    "    time.sleep(0.05)\n"
)


def clean_base():
    """A temporary base outside every git worktree, which is what the module requires."""
    candidates = [os.environ.get("CRW_TRIAL_TMPDIR"), tempfile.gettempdir(), "/var/tmp"]
    for candidate in candidates:
        if not candidate:
            continue
        path = Path(candidate)
        if path.is_dir() and startup.git_worktree_of(path) is None:
            return path
    raise unittest.SkipTest(
        "every candidate temporary directory is inside a git worktree; set CRW_TRIAL_TMPDIR")


class World:
    """One complete, agreeing trial, and the handles to disagree with it one field at a time."""

    RELATIONSHIP = "rel-0000000000000001"
    ISSUE_A, ISSUE_B = "TRIAL-1", "TRIAL-2"
    PARENT_A, CHILD_A = "task-parent-a", "task-child-a"
    PARENT_B, CHILD_B = "task-parent-b", "task-child-b"
    STORE_ID, DEVICE, INODE, NONCE = "store0000000001", 64512, 4242, "nonce-01"

    def __init__(self, base):
        self.root = Path(tempfile.mkdtemp(dir=str(base), prefix="crw111-"))
        self.trial = self.root / "trial"
        self.install = self.root / "install"
        self.state = self.root / "state"
        self.bin = self.install / "current" / "bin"
        for directory in (self.trial, self.bin, self.state, self.root / "workspace"):
            directory.mkdir(parents=True, exist_ok=True)
        self.repos = {}
        for name, issue in (("A", self.ISSUE_A), ("B", self.ISSUE_B)):
            repo = self.root / ("repo-" + name)
            repo.mkdir(parents=True, exist_ok=True)
            subprocess.run(["git", "init", "-q", str(repo)], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            self.repos[name] = repo
        self.launcher = self.bin / "codex-session-relay"
        self.launcher.write_text(LAUNCHER, encoding="utf-8")
        self.launcher.chmod(0o755)
        self.payloads_path = self.bin / "payloads.json"
        self.calls = self.bin / "calls.jsonl"
        self.supervisor = None
        self.payloads = self._payloads()
        self.captures = {}
        self._write_host_record()
        self._write_captures()
        self._write_assignment_and_message()
        self.record = self._record()
        self.flush()

    # ------------------------------------------------------------------ writing

    def flush(self):
        self.payloads_path.write_text(json.dumps(self.payloads), encoding="utf-8")
        for name, payload in self.captures.items():
            (self.trial / name).write_text(json.dumps(payload), encoding="utf-8")
        (self.trial / "start.json").write_text(json.dumps(self.record), encoding="utf-8")

    def stop(self):
        if self.supervisor is not None:
            self.supervisor.terminate()
            try:
                self.supervisor.wait(timeout=5)
            except subprocess.TimeoutExpired:                       # pragma: no cover
                self.supervisor.kill()
            self.supervisor = None
        shutil.rmtree(self.root, ignore_errors=True)

    def start_supervisor(self):
        witness = self.trial / "supervisor.jsonl"
        self.supervisor = subprocess.Popen(
            [sys.executable, "-c", WITNESS_WRITER, str(witness)], start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.time() + 5
        while time.time() < deadline and not witness.exists():
            time.sleep(0.02)
        self.record["supervisor"]["pid"] = self.supervisor.pid
        self.flush()
        return self.supervisor.pid

    def _write_host_record(self):
        directory = self.state / "codex-relay-workflow"
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "host-record.json").write_text(json.dumps({
            "recordVersion": 1, "definitionVersion": 1, "host": "test", "user": "test",
            "components": {"codex-session-relay": {
                "installs": [{"location": str(self.install)}], "measuredPoints": []}},
        }), encoding="utf-8")

    def environment(self):
        return {"XDG_STATE_HOME": str(self.state), "HOME": str(self.root)}

    def child_environment(self):
        env = dict(os.environ)
        env.update(self.environment())
        return env


    # ----------------------------------------------------------------- payloads

    def _payloads(self):
        return {
            "doctor": {"payload": {
                "sameStore": "proven",
                "store": {"storeId": self.STORE_ID, "device": self.DEVICE, "inode": self.INODE,
                          "createdAt": "2020-01-01T00:00:00Z"},
                "ledger": {"configured": True, "split": False},
                "actorReachability": {"socketConnect": "ok"},
                "nonce": {"nonce": self.NONCE, "found": True, "readable": True},
            }},
            "service status": {"payload": {
                "lock": "held", "staleRecord": False, "ownership": "ours", "pid": 0,
                "storeId": self.STORE_ID,
            }},
            "assignment-find": {"payload": {
                "issueKey": self.ISSUE_A,
                "responsibleRelationship": self.RELATIONSHIP,
                "responsibleChild": self.CHILD_A,
                "assignments": [{
                    "relationshipId": self.RELATIONSHIP, "issueKey": self.ISSUE_A,
                    "parentTaskId": self.PARENT_A, "childTaskId": self.CHILD_A,
                    "relationshipStatus": "active", "state": "requested",
                    "executionGeneration": 1,
                    "criteria": {"mode": "registered", "registered": 4},
                }],
            }},
            "criteria-show": {"payload": {
                "relationshipId": self.RELATIONSHIP, "mode": "registered",
                "criteria": ["c1", "c2", "c3", "c4"], "setDigest": "digest-01",
                "sourceRef": "source-01",
            }},
            "settings-show": {"payload": {
                "task": self.PARENT_A, "usable": True, "missing": [],
                "settings": self._settings(),
            }},
        }

    def _settings(self):
        return {"model": "a-model", "reasoningEffort": "xhigh", "sandbox": "dangerFullAccess",
                "approvalPolicy": "never", "cwd": str(self.root / "workspace"),
                "runtimeWorkspaceRoots": []}

    def _write_captures(self):
        settings = self._settings()
        echo = {k: settings[k] for k in ("model", "reasoningEffort", "sandbox", "approvalPolicy")}
        for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B, self.CHILD_B):
            self.captures["receipt-" + task + ".json"] = {
                "taskId": task, "settings": {"requested": echo, "actual": echo, "findings": []}}
        for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B, self.CHILD_B):
            self.captures["lifecycle-" + task + ".json"] = {
                "threadId": task, "status": "idle", "goal": None}
        for name, issue in (("A", self.ISSUE_A), ("B", self.ISSUE_B)):
            child = self.CHILD_A if name == "A" else self.CHILD_B
            parent = self.PARENT_A if name == "A" else self.PARENT_B
            self.captures["register-" + name + ".json"] = {
                "relationshipId": self.RELATIONSHIP if name == "A" else "rel-0000000000000002",
                "issueKey": issue, "status": "active", "executionGeneration": 1,
                "parent": {"taskId": parent, "cwd": str(self.repos[name])},
                "child": {"taskId": child, "cwd": str(self.repos[name])},
                "authorizedScope": {"scopeRef": "scope-" + name,
                                    "artifactRoots": [str(self.repos[name])],
                                    "allowedRecipients": [parent, child]},
            }
        for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B, self.CHILD_B):
            self.captures["doctor-" + task + ".json"] = {
                "sameStore": "proven",
                "store": {"storeId": self.STORE_ID, "device": self.DEVICE, "inode": self.INODE},
                "nonce": {"nonce": self.NONCE, "found": True, "readable": True,
                          "device": self.DEVICE, "inode": self.INODE},
            }

    def _write_assignment_and_message(self):
        artifact = self.repos["A"] / "artifact.py"
        artifact.write_text("# the trial's artifact\n", encoding="utf-8")
        self.artifact = artifact
        self.assignment_file = self.repos["A"] / "assignment.json"
        self.assignment_file.write_text(json.dumps({
            "relationshipId": self.RELATIONSHIP, "childTaskId": self.CHILD_A,
            "executionGeneration": 1, "artifacts": [str(artifact)]}), encoding="utf-8")
        self.message_file = self.trial / "dispatch.txt"
        self.message_file.write_text(
            "Work on " + self.ISSUE_A + " under relationship " + self.RELATIONSHIP
            + " and emit " + str(artifact) + " when it is ready.\n", encoding="utf-8")

    def _record(self):
        def boundary(name, issue, parent, child):
            return {"name": name, "issueKey": issue, "scopeRef": "scope-" + name,
                    "repositoryRoot": str(self.repos[name]),
                    "participants": [
                        {"role": "parent", "taskId": parent, "cwd": str(self.repos[name]),
                         "expect": {"model": "a-model", "reasoningEffort": "xhigh",
                                    "sandbox": "dangerFullAccess", "approvalPolicy": "never"}},
                        {"role": "child", "taskId": child, "cwd": str(self.repos[name]),
                         "expect": {"model": "a-model", "reasoningEffort": "xhigh",
                                    "sandbox": "dangerFullAccess", "approvalPolicy": "never"}}]}

        now = time.time()
        captures = {
            "parentLifecycle": {task: {"path": str(self.trial / ("lifecycle-" + task + ".json")),
                                       "capturedAt": startup.stamp(now - 10)}
                                for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B,
                                             self.CHILD_B)},
            "creationReceipt": {task: {"path": str(self.trial / ("receipt-" + task + ".json")),
                                       "capturedAt": startup.stamp(now - 10)}
                                for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B,
                                             self.CHILD_B)},
            "registration": {name: {"path": str(self.trial / ("register-" + name + ".json")),
                                    "capturedAt": startup.stamp(now - 10)}
                             for name in ("A", "B")},
            "peerDoctor": {task: {"path": str(self.trial / ("doctor-" + task + ".json")),
                                  "capturedAt": startup.stamp(now - 10)}
                           for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B, self.CHILD_B)},
        }
        return {
            "source": "live-trial-start", "recordVersion": 1, "trialRoot": str(self.trial),
            "relay": {"launcher": str(self.launcher),
                      "launcherSha256": startup.digest_of(self.launcher),
                      "stateDirectory": str(self.state / "relay"), "socket": str(self.root / "sock")},
            "store": {"storeId": self.STORE_ID, "device": self.DEVICE, "inode": self.INODE,
                      "challengeNonce": self.NONCE},
            "supervisor": {"pid": os.getpid(), "witness": str(self.trial / "supervisor.jsonl"),
                           "launchedAt": startup.stamp(now - 120), "minimumAliveSeconds": 1,
                           "witnessAdvanceSeconds": 0.3, "service": False},
            "assignment": {"relationshipId": self.RELATIONSHIP, "parentTaskId": self.PARENT_A,
                           "childTaskId": self.CHILD_A, "issueKey": self.ISSUE_A,
                           "executionGeneration": 1, "artifacts": [str(self.artifact)],
                           "assignmentFile": str(self.assignment_file),
                           "dispatchMessageFile": str(self.message_file),
                           "criteria": {"setDigest": "digest-01", "sourceRef": "source-01",
                                        "count": 4}},
            "boundaries": [boundary("A", self.ISSUE_A, self.PARENT_A, self.CHILD_A),
                           boundary("B", self.ISSUE_B, self.PARENT_B, self.CHILD_B)],
            "captures": captures, "captureMaxAgeSeconds": 600,
            # A preflight runs before the window opens, so the default record declares one ahead.
            # The ledger cases set their own, which is what a finished trial's record carries.
            "window": {"opensAt": startup.stamp(now + 60), "closesAt": startup.stamp(now + 600)},
        }

    # ------------------------------------------------------------------ running

    def preflight(self):
        record = startup.load_start(str(self.trial / "start.json"),
                                    environment=self.environment())
        record["_now"] = startup.datetime.datetime.now(startup.datetime.timezone.utc)
        return startup.preflight(record, sleeper=lambda seconds: time.sleep(min(seconds, 0.4)))

    def preflight_with(self, sleeper):
        """The same run with the pause between the two witness observations under the test's
        control, so a case about what the witness says can write the second line itself."""
        record = startup.load_start(str(self.trial / "start.json"),
                                    environment=self.environment())
        record["_now"] = startup.datetime.datetime.now(startup.datetime.timezone.utc)
        return startup.preflight(record, sleeper=sleeper)

    def run_cli(self, command="preflight", extra=()):
        argv = [sys.executable, str(ROOT / "scripts" / "trial_startup.py"), command,
                "--start", str(self.trial / "start.json"), *extra]
        done = subprocess.run(argv, capture_output=True, text=True, cwd=str(self.root),
                              env=self.child_environment(), timeout=120)
        payload = None
        try:
            payload = json.loads(done.stdout)
        except ValueError:
            payload = None
        return done.returncode, payload, done.stderr

    def refusal(self):
        try:
            startup.load_start(str(self.trial / "start.json"), environment=self.environment())
        except startup.Refused as refused:
            return refused
        return None

    def ledger_lines(self, lines):
        (self.trial / "ledger.jsonl").write_text(
            "".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")

    def run_ledger(self):
        record = startup.load_start(str(self.trial / "start.json"),
                                    environment=self.environment(), mode="ledger")
        record["_now"] = startup.datetime.datetime.now(startup.datetime.timezone.utc)
        return startup.ledger(record)


def cells_of(document, reading):
    return {c["cell"]: c for c in document["readings"][reading]["cells"]}



class TrialCase(unittest.TestCase):
    """One world per case, because every case disagrees with it in a different place."""

    @classmethod
    def setUpClass(cls):
        cls.base = clean_base()

    def setUp(self):
        self.world = World(self.base)
        self.addCleanup(self.world.stop)


class AgreeingRun(TrialCase):
    def test_every_reading_is_met_and_the_gate_passes(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])
        for name in READINGS:
            self.assertEqual(document["readings"][name]["value"], VERIFIED,
                             name + " is not met: " + json.dumps(document["readings"][name]))
        self.assertTrue(document["orderGate"]["passed"], document["orderGate"]["comparisons"])
        self.assertEqual(document["judgmentsThatFailed"], [])
        self.assertGreater(document["judgmentsCounted"], len(READINGS))

    def test_the_command_exits_zero_and_prints_one_object(self):
        self.world.start_supervisor()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 0, stderr + json.dumps(payload or {}))
        self.assertEqual(payload["source"], "live-trial-startup")
        self.assertTrue(payload["readyToStart"])

    def test_only_allowlisted_subcommands_were_ever_composed(self):
        self.world.start_supervisor()
        self.world.preflight()
        seen = {json.loads(line)["subcommand"]
                for line in self.world.calls.read_text().splitlines() if line.strip()}
        self.assertTrue(seen)
        self.assertEqual(seen - set(ALLOWED), set())

    def test_doctor_runs_before_anything_that_constructs_a_store(self):
        self.world.start_supervisor()
        self.world.preflight()
        order = [json.loads(line)["subcommand"]
                 for line in self.world.calls.read_text().splitlines() if line.strip()]
        self.assertEqual(order[0], "doctor")

    def test_a_capture_is_labelled_captured_and_an_execution_is_not(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        lifecycle = cells_of(document, "parentLifecycle")
        self.assertEqual(lifecycle["lifecycle:" + World.PARENT_A]["provenance"], "captured")
        self.assertEqual(cells_of(document, "storeIdentity")["sameStore"]["provenance"], "executed")

    def test_a_refused_subcommand_never_reaches_a_process(self):
        relay = startup.Relay(startup.load_start(str(self.world.trial / "start.json"),
                                                 environment=self.world.environment()))
        for call in (("emit",), ("deliver",), ("register",), ("service", "start")):
            with self.assertRaises(startup.Refused):
                relay.relay(*call)
        with self.assertRaises(startup.Refused):
            relay.git(self.world.repos["A"], "push")
        self.assertFalse(self.world.calls.exists())


class UnknownIsNotFalse(TrialCase):
    def test_a_payload_without_the_field_answers_unknown(self):
        self.world.payloads["doctor"]["payload"].pop("sameStore")
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "storeIdentity")["sameStore"]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertFalse(cell["met"])
        self.assertFalse(document["readyToStart"])

    def test_output_that_is_not_json_answers_unknown(self):
        self.world.payloads["criteria-show"] = {"stdout": "not json at all"}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "assignmentState")["criteria"]["value"], UNKNOWN)

    def test_a_missing_capture_answers_unknown(self):
        (self.world.trial / ("lifecycle-" + World.PARENT_B + ".json")).unlink()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_B]["value"],
            UNKNOWN)

    def test_a_stale_capture_answers_unknown_rather_than_fresh(self):
        self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["capturedAt"] = (
            startup.stamp(time.time() - 4000))
        self.world.record["captureMaxAgeSeconds"] = 60
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertIn("seconds old", cell["evidence"])

    def test_a_missing_program_answers_unknown_rather_than_crashing(self):
        self.world.launcher.unlink()
        self.world.record["relay"]["launcherSha256"] = "0" * 64
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("launcher", refused.reason)


class DoctorRefusalIsAnAnswer(TrialCase):
    def test_a_non_zero_exit_carrying_a_verdict_is_graded_not_unknown(self):
        self.world.payloads["doctor"]["payload"]["sameStore"] = "unproven"
        self.world.payloads["doctor"]["exit"] = 2
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "storeIdentity")["sameStore"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertEqual(cell["exitCode"], 2)
        self.assertIn("unproven", cell["evidence"])


class ProcessPersistence(TrialCase):
    def test_a_supervisor_in_the_callers_own_session_is_not_detached(self):
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "processPersistence")["alive"]["value"], NOT_VERIFIED)

    def test_a_witness_that_does_not_advance_fails(self):
        self.world.start_supervisor()
        self.world.supervisor.terminate()
        self.world.supervisor.wait(timeout=5)
        document = self.world.preflight()
        cells = cells_of(document, "processPersistence")
        self.assertIn(cells["witnessAdvance"]["value"], (NOT_VERIFIED, UNKNOWN))
        self.assertEqual(cells["alive"]["value"], NOT_VERIFIED)

    def test_a_witness_naming_another_pid_fails(self):
        pid = self.world.start_supervisor()
        self.world.supervisor.terminate()
        self.world.supervisor.wait(timeout=5)
        witness = self.world.trial / "supervisor.jsonl"

        def write(lines):
            witness.write_text("".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")

        # A counter that advances, written by something claiming a pid that is not the
        # supervisor's. The advance alone would pass; the pid is what refuses.
        write([{"pid": 999999, "progress": 1}])
        document = self.world.preflight_with(
            lambda seconds: write([{"pid": 999999, "progress": 1}, {"pid": 999999, "progress": 2}]))
        self.assertEqual(cells_of(document, "processPersistence")["witnessAdvance"]["value"],
                         NOT_VERIFIED)

        # The same two lines under the supervisor's own pid pass, so the case above failed for the
        # pid rather than for the shape of the file.
        write([{"pid": pid, "progress": 1}])
        document = self.world.preflight_with(
            lambda seconds: write([{"pid": pid, "progress": 1}, {"pid": pid, "progress": 2}]))
        self.assertEqual(cells_of(document, "processPersistence")["witnessAdvance"]["value"],
                         VERIFIED)

    def test_a_service_trial_reads_the_service_and_compares_it(self):
        self.world.start_supervisor()
        self.world.record["supervisor"]["service"] = True
        self.world.payloads["service status"]["payload"]["pid"] = self.world.supervisor.pid
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "processPersistence")["service"]["value"], VERIFIED)
        for key, value in (("ownership", "foreign"), ("lock", "free"), ("staleRecord", True),
                           ("storeId", "another-store")):
            self.world.payloads["service status"]["payload"][key] = value
            self.world.flush()
            document = self.world.preflight()
            self.assertEqual(cells_of(document, "processPersistence")["service"]["value"],
                             NOT_VERIFIED, key + " did not fail the service cell")
            self.world.payloads["service status"]["payload"] = {
                "lock": "held", "staleRecord": False, "ownership": "ours",
                "pid": self.world.supervisor.pid, "storeId": World.STORE_ID}

    def test_no_service_declared_is_not_applicable_rather_than_a_failure(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        cell = cells_of(document, "processPersistence")["service"]
        self.assertEqual(cell["value"], NOT_APPLICABLE)
        self.assertTrue(cell["met"])


class ParentLifecycle(TrialCase):
    def test_each_refusal_string_fails_by_name(self):
        for text in ("thread not found", "missing source rollout", "no rollout found"):
            self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
                "threadId": World.PARENT_A, "status": "unknown", "error": text}
            self.world.flush()
            document = self.world.preflight()
            cell = cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]
            self.assertEqual(cell["value"], NOT_VERIFIED, text)
            self.assertIn(text, cell["evidence"])

    def test_a_healthy_capture_with_a_null_goal_is_met(self):
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
            VERIFIED)

    def test_a_capture_naming_another_task_is_not_this_ones_evidence(self):
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": "somebody-else", "status": "idle"}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
            NOT_VERIFIED)


class Capability(TrialCase):
    def test_a_setting_that_disagrees_fails_each_half(self):
        self.world.payloads["settings-show"]["payload"]["settings"]["model"] = "another-model"
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_an_approval_policy_that_disagrees_is_not_hidden_behind_usable(self):
        self.world.payloads["settings-show"]["payload"]["settings"]["approvalPolicy"] = "on-request"
        self.world.payloads["settings-show"]["payload"]["usable"] = True
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_findings_that_are_not_empty_fail_the_echo(self):
        capture = self.world.captures["receipt-" + World.PARENT_A + ".json"]
        capture["settings"]["findings"] = [{"code": "unobservable", "field": "approvalPolicy"}]
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_an_expect_missing_a_required_setting_is_refused(self):
        self.world.record["boundaries"][0]["participants"][0]["expect"].pop("approvalPolicy")
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("required setting", refused.reason)



class StoreIdentity(TrialCase):
    def test_unproven_is_not_a_pass(self):
        self.world.payloads["doctor"]["payload"]["sameStore"] = "unproven"
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "storeIdentity")["sameStore"]["value"], NOT_VERIFIED)

    def test_a_peer_reporting_proven_about_another_store_fails(self):
        self.world.captures["doctor-" + World.CHILD_A + ".json"] = {
            "sameStore": "proven",
            "store": {"storeId": "another-store", "device": 1, "inode": 2},
            "nonce": {"nonce": World.NONCE, "found": True, "readable": True}}
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "storeIdentity")["peer:" + World.CHILD_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("disagrees", cell["evidence"])

    def test_a_store_created_after_this_run_started_fails(self):
        self.world.payloads["doctor"]["payload"]["store"]["createdAt"] = startup.stamp(
            time.time() + 3600)
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "storeIdentity")["storeAge"]["value"], NOT_VERIFIED)

    def test_an_unconfigured_ledger_is_not_applicable_rather_than_a_failure(self):
        self.world.payloads["doctor"]["payload"]["ledger"] = {"configured": False, "split": False}
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "storeIdentity")["ledgerSplit"]
        self.assertEqual(cell["value"], NOT_APPLICABLE)
        self.assertTrue(cell["met"])

    def test_a_split_ledger_fails(self):
        self.world.payloads["doctor"]["payload"]["ledger"] = {"configured": True, "split": True}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "storeIdentity")["ledgerSplit"]["value"], NOT_VERIFIED)

    def test_the_expectations_are_actually_passed_to_doctor(self):
        self.world.preflight()
        call = next(json.loads(line) for line in self.world.calls.read_text().splitlines()
                    if json.loads(line)["subcommand"] == "doctor")
        self.assertIn("--expect-store", call["argv"])
        self.assertIn("--expect-nonce", call["argv"])
        self.assertIn(World.NONCE, call["argv"])


class Boundaries(TrialCase):
    def test_two_boundaries_sharing_an_issue_key_fail(self):
        self.world.record["boundaries"][1]["issueKey"] = World.ISSUE_A
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["declaration"]["value"], NOT_VERIFIED)

    def test_two_boundaries_in_one_repository_fail(self):
        self.world.record["boundaries"][1]["repositoryRoot"] = str(self.world.repos["A"])
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["declaration"]["value"], NOT_VERIFIED)

    def test_a_registration_naming_another_scope_fails(self):
        self.world.captures["register-A.json"]["authorizedScope"]["scopeRef"] = "scope-somewhere"
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"], NOT_VERIFIED)

    def test_a_missing_registration_for_one_boundary_is_unknown(self):
        (self.world.trial / "register-B.json").unlink()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["registration:B"]["value"], UNKNOWN)

    def test_a_directory_in_another_repository_fails(self):
        self.world.record["boundaries"][0]["participants"][0]["cwd"] = str(self.world.repos["B"])
        self.world.flush()
        document = self.world.preflight()
        cells = cells_of(document, "boundaries")
        self.assertEqual(cells["toplevel:A:" + World.PARENT_A]["value"], NOT_VERIFIED)


class OrderGate(TrialCase):
    def gate(self):
        return self.world.preflight()["orderGate"]

    def disagreement(self, gate, name):
        return [c for c in gate["comparisons"] if c["field"] == name and c["agrees"] is not True]

    def test_the_agreeing_case_passes(self):
        self.assertTrue(self.gate()["passed"])

    def test_a_file_carrying_the_previous_relationship_is_refused(self):
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": "rel-985d14634e4b7221", "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [str(self.world.artifact)]}), encoding="utf-8")
        gate = self.gate()
        self.assertFalse(gate["passed"])
        self.assertTrue(self.disagreement(gate, "relationshipId"))

    def test_an_archived_relationship_in_the_store_is_refused(self):
        self.world.payloads["assignment-find"]["payload"]["assignments"][0][
            "relationshipStatus"] = "archived"
        self.world.flush()
        gate = self.gate()
        self.assertFalse(gate["passed"])
        self.assertTrue(self.disagreement(gate, "relationshipStatus"))

    def test_a_generation_that_disagrees_is_refused(self):
        self.world.payloads["assignment-find"]["payload"]["assignments"][0][
            "executionGeneration"] = 2
        self.world.flush()
        gate = self.gate()
        self.assertFalse(gate["passed"])
        self.assertTrue(self.disagreement(gate, "executionGeneration"))

    def test_an_artifact_outside_the_authorised_roots_is_refused(self):
        outside = self.world.root / "loose-artifact.py"
        outside.write_text("# outside\n", encoding="utf-8")
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [str(outside)]}), encoding="utf-8")
        self.world.record["assignment"]["artifacts"] = [str(outside)]
        self.world.message_file.write_text(
            "relationship " + World.RELATIONSHIP + " artifact " + str(outside), encoding="utf-8")
        self.world.flush()
        gate = self.gate()
        self.assertFalse(gate["passed"])
        self.assertTrue(self.disagreement(gate, "artifact"))

    def test_a_message_that_does_not_carry_the_identity_is_refused(self):
        self.world.message_file.write_text("Please start whenever you like.\n", encoding="utf-8")
        gate = self.gate()
        self.assertFalse(gate["passed"])
        self.assertTrue(self.disagreement(gate, "messageCarriesRelationship"))

    def test_an_unreadable_assignment_file_is_not_a_pass(self):
        self.world.assignment_file.unlink()
        gate = self.gate()
        self.assertFalse(gate["passed"])
        self.assertTrue(gate["unreadable"])


class Ledger(TrialCase):
    def base_lines(self, extra=()):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        lines = [
            {"at": startup.stamp(now - 300), "kind": "segment_start", "segment": "window-1"},
            {"at": startup.stamp(now - 280), "kind": "intervention", "segment": "window-1",
             "actor": "operator", "target": "process", "action": "restarted the supervisor"},
            {"at": startup.stamp(now - 200), "kind": "segment_end", "segment": "window-1",
             "outcome": "failed"},
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window-4"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window-4"},
        ]
        lines.extend(extra)
        lines.sort(key=lambda line: line["at"])
        self.world.ledger_lines(lines)
        return now, opened, closed

    def test_preparation_and_window_are_counted_apart(self):
        self.base_lines()
        document = self.world.run_ledger()
        self.assertEqual(document["preparation"]["interventions"], 1)
        self.assertEqual(document["window"]["interventions"], 0)
        self.assertTrue(document["window"]["windowIsClean"])
        self.assertEqual(document["preparation"]["failedSegments"], ["window-1"])
        self.assertEqual(document["preparation"]["segments"][0]["interventions"], 1)
        self.assertEqual(document["judgmentsThatFailed"], [])

    def test_an_intervention_inside_the_window_fails_its_judgment(self):
        now, opened, closed = self.base_lines()
        self.base_lines([{"at": startup.stamp(opened + 10), "kind": "intervention",
                          "segment": "window-4", "actor": "operator", "target": "task",
                          "action": "nudged the child"}])
        document = self.world.run_ledger()
        self.assertEqual(document["window"]["interventions"], 1)
        self.assertFalse(document["window"]["windowIsClean"])
        self.assertFalse(document["window"]["passed"])
        self.assertIn("window.passed", document["judgmentsThatFailed"])

    def test_the_command_exits_non_zero_when_the_window_was_intervened(self):
        now, opened, closed = self.base_lines()
        self.base_lines([{"at": startup.stamp(opened + 10), "kind": "intervention",
                          "segment": "window-4", "actor": "operator", "target": "task",
                          "action": "nudged the child"}])
        code, payload, stderr = self.world.run_cli("ledger")
        self.assertEqual(code, 1, stderr)
        self.assertFalse(payload["window"]["windowIsClean"])

    def test_a_label_disagreeing_with_its_own_timestamp_is_refused(self):
        now, opened, closed = self.base_lines()
        self.base_lines([{"at": startup.stamp(opened + 10), "kind": "intervention",
                          "segment": "window-4", "actor": "operator", "target": "task",
                          "action": "nudged the child", "claimed": "preparation"}])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("claimed class disagrees", raised.exception.reason)

    def test_two_windows_are_refused(self):
        now, opened, closed = self.base_lines()
        self.base_lines([{"at": startup.stamp(opened + 1), "kind": "window_open",
                          "segment": "window-5"}])
        with self.assertRaises(startup.Refused):
            self.world.run_ledger()

    def test_overlapping_segments_are_refused(self):
        now, opened, closed = self.base_lines()
        self.base_lines([{"at": startup.stamp(now - 290), "kind": "segment_start",
                          "segment": "window-2"}])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("segment", raised.exception.reason)

    def test_a_line_without_a_time_is_refused(self):
        self.base_lines()
        lines = [json.loads(line) for line in
                 (self.world.trial / "ledger.jsonl").read_text().splitlines() if line.strip()]
        lines.append({"kind": "intervention", "segment": "window-4", "action": "untimed"})
        self.world.ledger_lines(lines)
        with self.assertRaises(startup.Refused):
            self.world.run_ledger()

    def test_the_bounds_carry_their_provenance(self):
        self.base_lines()
        document = self.world.run_ledger()
        self.assertEqual(document["window"]["provenance"], "declared")
        self.world.record["window"]["corroboration"] = {"opensAt": document["window"]["opensAt"]}
        self.world.flush()
        self.assertEqual(self.world.run_ledger()["window"]["provenance"], "corroborated")



class Refusals(TrialCase):
    def test_a_relative_path_is_refused(self):
        self.world.record["assignment"]["assignmentFile"] = "assignment.json"
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("absolute", refused.reason)

    def test_a_trial_root_inside_a_git_worktree_is_refused(self):
        inside = self.world.repos["A"] / "trial"
        inside.mkdir()
        self.world.record["trialRoot"] = str(inside)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("git worktree", refused.reason)

    def test_a_private_capture_outside_the_trial_root_is_refused(self):
        loose = self.world.root / "lifecycle-elsewhere.json"
        loose.write_text("{}", encoding="utf-8")
        self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["path"] = str(loose)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("outside the trial root", refused.reason)

    def test_a_private_capture_reached_by_a_symbolic_link_is_still_outside(self):
        loose = self.world.root / "lifecycle-elsewhere.json"
        loose.write_text("{}", encoding="utf-8")
        link = self.world.trial / "lifecycle-link.json"
        link.symlink_to(loose)
        self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["path"] = str(link)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("outside the trial root", refused.reason)

    def test_a_launcher_whose_bytes_changed_is_refused(self):
        self.world.record["relay"]["launcherSha256"] = "0" * 64
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("bytes changed", refused.reason)

    def test_a_launcher_the_host_record_does_not_name_is_refused(self):
        other = self.world.root / "somewhere" / "codex-session-relay"
        other.parent.mkdir(parents=True, exist_ok=True)
        other.write_text(LAUNCHER, encoding="utf-8")
        other.chmod(0o755)
        self.world.record["relay"]["launcher"] = str(other)
        self.world.record["relay"]["launcherSha256"] = startup.digest_of(other)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("host record names", refused.reason)

    def test_an_absent_host_record_is_refused(self):
        (self.world.state / "codex-relay-workflow" / "host-record.json").unlink()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("host record", refused.reason)

    def test_a_host_record_with_no_install_for_the_relay_is_refused(self):
        (self.world.state / "codex-relay-workflow" / "host-record.json").write_text(
            json.dumps({"recordVersion": 1, "definitionVersion": 1, "host": "t", "user": "t",
                        "components": {}}), encoding="utf-8")
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("no install", refused.reason)

    def test_a_capture_bound_above_the_ceiling_is_refused(self):
        self.world.record["captureMaxAgeSeconds"] = 5000
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("out of range", refused.reason)
        self.assertEqual(refused.detail["maximum"], 900)

    def test_a_capture_dated_in_the_future_is_refused(self):
        self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["capturedAt"] = (
            startup.stamp(time.time() + 3600))
        self.world.flush()
        record = startup.load_start(str(self.world.trial / "start.json"),
                                    environment=self.world.environment())
        record["_now"] = startup.datetime.datetime.now(startup.datetime.timezone.utc)
        with self.assertRaises(startup.Refused) as raised:
            startup.preflight(record, sleeper=lambda seconds: None)
        self.assertIn("future", raised.exception.reason)

    def test_the_command_exits_two_and_prints_the_refusal(self):
        self.world.record["captureMaxAgeSeconds"] = 5000
        self.world.flush()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 2, stderr)
        self.assertIn("refused", payload)


class WritesNothing(TrialCase):
    @staticmethod
    def snapshot(directory):
        found = {}
        for path in sorted(Path(directory).rglob("*")):
            if path.is_file():
                try:
                    found[str(path)] = path.stat().st_mtime_ns, path.stat().st_size
                except OSError:                                      # pragma: no cover
                    found[str(path)] = None
        return found

    def test_a_run_against_these_stubs_writes_no_file(self):
        self.world.start_supervisor()
        # The supervisor appends to its own witness by design, so it is stopped first and the
        # comparison is about what THIS command does.
        self.world.supervisor.terminate()
        self.world.supervisor.wait(timeout=5)
        before_trial = self.snapshot(self.world.trial)
        before_repo = self.snapshot(ROOT / "scripts")
        self.world.run_cli()
        self.assertEqual(self.snapshot(self.world.trial), before_trial)
        self.assertEqual(self.snapshot(ROOT / "scripts"), before_repo)


class NoHostIdentifier(TrialCase):
    PATTERNS = (
        (r"/home/[a-z]", "an absolute home directory"),
        (r"\brel-[0-9a-f]{16}\b", "a relationship id"),
        (r"\b01a0b[0-9a-f]{3}-[0-9a-f-]{20,}", "a task id"),
        (r"\b[0-9a-f]{32}\b", "a store or digest identifier"),
    )

    def test_the_committed_files_carry_no_host_identifier(self):
        import re

        for name in ("docs/live-trial.md", "scripts/trial_startup.py"):
            text = (ROOT / name).read_text(encoding="utf-8")
            for pattern, what in self.PATTERNS:
                self.assertIsNone(re.search(pattern, text),
                                  name + " carries " + what + ", which belongs in a private record")


class PayloadContract(TrialCase):
    """Every relay field name this module reads, found in the relay's own source.

    A stub prints whatever this test writes into it, so a suite built only on stubs agrees with a
    checker reading a field the relay never returns. That is the defect an earlier revision of this
    plan actually had, in four places at once, so the inventory is checked against the source that
    produces the payloads rather than against the fixtures.
    """

    RELAY = ROOT / "packages" / "codex-session-relay" / "src" / "codex_session_relay"

    # Field name -> the function that actually builds the payload carrying it. A string found
    # anywhere in a file is not evidence that a command returns it: an earlier revision of this
    # test looked for strings in whole modules and stayed green while four predicates read fields
    # no payload carried.
    PRODUCERS = {
        ("assignment.py", "state"): ("relationshipId", "issueKey", "childTaskId",
                                     "parentTaskId", "relationshipStatus", "executionGeneration",
                                     "criteria"),
        ("assignment.py", "for_issue"): ("issueKey", "assignments", "responsibleRelationship"),
        ("cli.py", "cmd_criteria_show"): ("setDigest", "sourceRef", "criteria"),
        ("cli.py", "cmd_settings_show"): ("task", "usable", "missing", "settings"),
        ("cli.py", "cmd_doctor"): ("ledger", "nonce", "actorReachability"),
        ("cli.py", "_reachability"): ("socketConnect",),
        ("cli.py", "_ledger_location"): ("configured", "split"),
        ("store.py", "probe"): ("store", "storeId", "createdAt", "device", "inode"),
        ("store.py", "compare_store"): ("sameStore",),
        ("service.py", "status"): ("lock", "staleRecord", "ownership", "pid", "storeId"),
        ("registry.py", "_row_to_record"): ("authorizedScope", "scopeRef", "artifactRoots",
                                            "allowedRecipients", "child", "parent", "taskId",
                                            "cwd"),
    }

    def keys_built_by(self, name, function):
        """Every payload key this function writes: dict literals it builds and subscripts it sets."""
        tree = ast.parse((self.RELAY / name).read_text(encoding="utf-8"))
        node = next((n for n in ast.walk(tree)
                     if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
                     and n.name == function), None)
        self.assertIsNotNone(node, function + " is gone from " + name)
        found = set()
        for inner in ast.walk(node):
            if isinstance(inner, ast.Dict):
                for key in inner.keys:
                    if isinstance(key, ast.Constant) and isinstance(key.value, str):
                        found.add(key.value)
            if isinstance(inner, (ast.Assign, ast.AugAssign, ast.AnnAssign)):
                targets = inner.targets if isinstance(inner, ast.Assign) else [inner.target]
                for target in targets:
                    if (isinstance(target, ast.Subscript)
                            and isinstance(target.slice, ast.Constant)
                            and isinstance(target.slice.value, str)):
                        found.add(target.slice.value)
        return found

    def test_every_field_name_this_module_reads_is_built_by_the_command_it_reads_it_from(self):
        for (name, function), fields in sorted(self.PRODUCERS.items()):
            built = self.keys_built_by(name, function)
            for expected in fields:
                self.assertIn(expected, built,
                              expected + " is not built by " + name + ":" + function + ", so this"
                              " module reads a field that payload does not carry")

    def test_the_producer_inventory_would_notice_a_field_that_moved(self):
        # The extraction has to be specific enough to fail. A name that belongs to another payload
        # must not be found in this one.
        self.assertNotIn("sameStore", self.keys_built_by("assignment.py", "state"))
        self.assertNotIn("authorizedScope", self.keys_built_by("assignment.py", "state"))

    def test_the_module_reads_no_field_this_inventory_forgot(self):
        source = ast.parse((ROOT / "scripts" / "trial_startup.py").read_text(encoding="utf-8"))
        reads = set()
        for node in ast.walk(source):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id == "field"):
                for argument in node.args[1:]:
                    if isinstance(argument, ast.Constant) and isinstance(argument.value, str):
                        reads.add(argument.value)
        declared = {name for fields in self.PRODUCERS.values() for name in fields}
        # Names this module invents for its own record, which no relay payload carries.
        own = {"relay", "launcher", "launcherSha256", "trialRoot", "supervisor", "witness",
               "witnessAdvanceSeconds", "assignment", "assignmentFile", "dispatchMessageFile",
               "captures", "path", "capturedAt", "components", "codex-session-relay", "installs",
               "window", "corroboration", "store", "actual", "findings", "error", "isError",
               "status", "threadId", "taskId", "stateDirectory", "socket", "launchedAt",
               "minimumAliveSeconds", "artifacts", "peerDoctor"}
        own |= {"closesAt"}
        self.assertEqual(reads - declared - own, set(),
                         "a field is read without being declared in the payload contract")


class ReproducedDefects(TrialCase):
    """The cases an independent review reproduced while this suite stayed green.

    Each one is a state the checker reported as met, or a failure it took without producing a
    document. They are here rather than in their topic classes so that the reason they exist stays
    attached to them: a suite that passes through a defect is the defect worth fixing first.
    """

    def test_a_settings_payload_with_no_settings_at_all_is_unknown(self):
        self.world.payloads["settings-show"]["payload"] = {"usable": True, "missing": []}
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertFalse(document["readyToStart"])

    def test_a_settings_payload_about_another_task_is_not_this_ones_evidence(self):
        self.world.payloads["settings-show"]["payload"]["task"] = "somebody-else"
        self.world.payloads["settings-show"]["stdout"] = json.dumps(
            self.world.payloads["settings-show"]["payload"])
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_a_receipt_about_another_task_is_not_this_ones_evidence(self):
        self.world.captures["receipt-" + World.PARENT_A + ".json"]["taskId"] = "somebody-else"
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_a_supervisor_that_has_not_yet_outlived_its_shell_fails(self):
        self.world.start_supervisor()
        self.world.record["supervisor"]["minimumAliveSeconds"] = 999999
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "processPersistence")["uptime"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("minimum", cell["evidence"])

    def test_a_launch_time_in_the_future_is_refused(self):
        self.world.record["supervisor"]["launchedAt"] = startup.stamp(time.time() + 3600)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("future", refused.reason)

    def test_no_peer_capture_at_all_is_unknown_rather_than_met(self):
        self.world.record["captures"]["peerDoctor"] = {}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(document["readings"]["storeIdentity"]["value"], UNKNOWN)
        peers = [c for c in document["readings"]["storeIdentity"]["cells"]
                 if c["cell"].startswith("peer:")]
        self.assertEqual(len(peers), 4)
        self.assertTrue(all(c["value"] == UNKNOWN for c in peers))

    def test_a_record_stating_nothing_agrees_with_nothing(self):
        for path in (("assignment", "relationshipId"), ("store", "storeId"),
                     ("assignment", "criteria", "setDigest")):
            world = World(self.base)
            self.addCleanup(world.stop)
            node = world.record
            for key in path[:-1]:
                node = node[key]
            node[path[-1]] = None
            world.flush()
            try:
                startup.load_start(str(world.trial / "start.json"),
                                   environment=world.environment())
            except startup.Refused as refused:
                self.assertIn("does not state", refused.reason)
            else:                                                    # pragma: no cover
                self.fail("a null " + ".".join(path) + " was accepted")

    def test_a_malformed_captures_object_is_refused_rather_than_raised(self):
        self.world.record["captures"] = ["not", "an", "object"]
        self.world.flush()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 2, stderr)
        self.assertIn("refused", payload)
        self.assertNotIn("Traceback", stderr)

    def test_a_receipt_with_no_findings_still_produces_a_document(self):
        self.world.captures["receipt-" + World.PARENT_A + ".json"]["settings"].pop("findings")
        self.world.flush()
        code, payload, stderr = self.world.run_cli()
        self.assertIn(code, (0, 1), stderr)
        self.assertIsNotNone(payload)
        self.assertNotIn("object object at", json.dumps(payload))

    def test_a_store_with_no_entry_for_this_relationship_still_produces_a_document(self):
        self.world.payloads["assignment-find"]["payload"]["assignments"] = []
        self.world.payloads["assignment-find"]["payload"]["responsibleRelationship"] = None
        self.world.flush()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 1, stderr)
        self.assertIsNotNone(payload, stderr)
        self.assertFalse(payload["orderGate"]["passed"])
        self.assertNotIn("object object at", json.dumps(payload))

    def test_segments_that_overlap_in_time_are_refused_whatever_order_they_were_written(self):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(now - 300), "kind": "segment_start", "segment": "one"},
            {"at": startup.stamp(now - 200), "kind": "segment_end", "segment": "one",
             "outcome": "failed"},
            {"at": startup.stamp(now - 250), "kind": "segment_start", "segment": "two"},
            {"at": startup.stamp(now - 150), "kind": "segment_end", "segment": "two",
             "outcome": "failed"},
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("overlap", raised.exception.reason)

    def test_a_corroborating_time_that_disagrees_is_refused(self):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {
            "opensAt": startup.stamp(opened), "closesAt": startup.stamp(closed),
            "corroboration": {"opensAt": startup.stamp(opened - 30)}}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("corroborating time disagrees", raised.exception.reason)

    def test_a_corroboration_naming_no_comparable_time_is_refused(self):
        now = time.time()
        self.world.record["window"] = {
            "opensAt": startup.stamp(now - 60), "closesAt": startup.stamp(now - 5),
            "corroboration": {"boundAt": startup.stamp(now - 60)}}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(now - 60), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(now - 5), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused):
            self.world.run_ledger()

    def test_a_timing_field_that_is_not_a_finite_number_is_refused(self):
        # NaN passes a range check from both sides at once, and every staleness comparison after
        # it is false as well, so a day-old capture read as fresh.
        for path, value in ((("captureMaxAgeSeconds",), float("nan")),
                            (("captureMaxAgeSeconds",), float("inf")),
                            (("captureMaxAgeSeconds",), True),
                            (("supervisor", "witnessAdvanceSeconds"), float("nan")),
                            (("supervisor", "minimumAliveSeconds"), float("nan"))):
            world = World(self.base)
            self.addCleanup(world.stop)
            node = world.record
            for key in path[:-1]:
                node = node[key]
            node[path[-1]] = value
            # json.dumps writes NaN and Infinity, which json.loads reads back, so this reaches the
            # module exactly as an operator's own file would.
            (world.trial / "start.json").write_text(json.dumps(world.record), encoding="utf-8")
            try:
                startup.load_start(str(world.trial / "start.json"),
                                   environment=world.environment())
            except startup.Refused as refused:
                self.assertIn("number", refused.reason)
            else:                                                    # pragma: no cover
                self.fail(".".join(path) + " accepted " + repr(value))

    def test_a_stale_capture_is_still_stale_under_a_nan_bound(self):
        self.world.record["captureMaxAgeSeconds"] = float("nan")
        (self.world.trial / "start.json").write_text(json.dumps(self.world.record),
                                                     encoding="utf-8")
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 2, stderr)
        self.assertIn("number", payload["refused"])


class HostedReviewFindings(TrialCase):
    """What two hosted reviewers found on the first pushed head, each with the case that reproduces it.

    Five were defects in the checker and two were claims it could not support. They are together
    because the reason they exist is one reason: every one of them passed the suite that was green
    when the pull request opened.
    """

    def test_a_structured_lifecycle_status_is_resolved(self):
        # The host's own lifecycle answer carries status as an object, so a predicate insisting on
        # a string rejected every capture a real host produces.
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A,
            "status": {"state": "idle", "activeTurn": None}, "goal": None}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
            VERIFIED)

    def test_an_empty_structured_status_is_still_not_resolved(self):
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A, "status": {}}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_a_relationship_under_another_parent_fails(self):
        self.world.payloads["assignment-find"]["payload"]["assignments"][0][
            "parentTaskId"] = "another-parent"
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "assignmentState")["relationship"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("another-parent", cell["evidence"])

    def test_a_registration_under_another_parent_fails(self):
        self.world.captures["register-A.json"]["parent"]["taskId"] = "another-parent"
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"], NOT_VERIFIED)

    def test_an_assignment_file_with_no_artifacts_is_a_mismatch(self):
        for value in ([], None, "not a list"):
            self.world.assignment_file.write_text(json.dumps({
                "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
                "executionGeneration": 1, "artifacts": value}), encoding="utf-8")
            gate = self.world.preflight()["orderGate"]
            self.assertFalse(gate["passed"], repr(value))
            self.assertTrue([c for c in gate["comparisons"]
                             if c["field"] == "artifacts" and c["agrees"] is False])

    def test_an_assignment_file_naming_other_artifacts_is_a_mismatch(self):
        other = self.world.repos["A"] / "other.py"
        other.write_text("# other\n", encoding="utf-8")
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [str(other)]}), encoding="utf-8")
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "artifacts" and c["agrees"] is False])

    def test_the_clock_cannot_be_pinned_from_the_command_line(self):
        code, payload, stderr = self.world.run_cli(extra=("--now", startup.stamp(time.time())))
        self.assertEqual(code, 2, stderr)
        self.assertIn("unrecognized arguments", stderr)

    def test_a_ledger_window_that_disagrees_with_the_record_is_refused(self):
        now = time.time()
        self.world.record["window"] = {"opensAt": startup.stamp(now - 600),
                                       "closesAt": startup.stamp(now - 500)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(now - 60), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(now - 5), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("does not match the one the record declares", raised.exception.reason)

    def test_a_segment_that_never_closes_is_refused(self):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(now - 300), "kind": "segment_start", "segment": "interrupted"},
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("never closes", raised.exception.reason)

    def test_a_segment_closing_without_an_outcome_is_refused(self):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        for outcome in (None, "went fine"):
            line = {"at": startup.stamp(now - 200), "kind": "segment_end", "segment": "one"}
            if outcome is not None:
                line["outcome"] = outcome
            self.world.ledger_lines([
                {"at": startup.stamp(now - 300), "kind": "segment_start", "segment": "one"},
                line,
                {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
                {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
            ])
            with self.assertRaises(startup.Refused) as raised:
                self.world.run_ledger()
            self.assertIn("failed or succeeded", raised.exception.reason)

    def test_the_write_free_claim_is_about_this_process_only(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        claim = document["wroteNothing"]
        self.assertIn("no file of its own", claim)
        self.assertIn("temporary file", claim)


class SecondHostedRound(TrialCase):
    """The four findings the hosted reviewers returned on the second pushed head."""

    def test_a_relative_artifact_in_the_assignment_file_is_refused(self):
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": ["artifact.py"]}), encoding="utf-8")
        self.world.record["assignment"]["artifacts"] = ["artifact.py"]
        self.world.flush()
        # The record refuses a relative artifact outright, and the gate refuses one that reached it.
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("absolute", refused.reason)

    def test_containment_answers_false_for_a_relative_path(self):
        self.assertFalse(startup.within("artifact.py", self.world.repos["A"]))
        self.assertTrue(startup.within(str(self.world.artifact), self.world.repos["A"]))

    def test_a_window_that_has_not_closed_is_refused(self):
        now = time.time()
        opened, closed = now - 60, now + 600
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("has not closed yet", raised.exception.reason)

    def test_a_registration_naming_another_issue_or_a_closed_one_fails(self):
        for key, value in (("issueKey", "SOMETHING-ELSE"), ("status", "archived"),
                           ("relationshipId", "rel-000000000000dead"),
                           ("executionGeneration", 9)):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.captures["register-A.json"][key] = value
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"],
                             NOT_VERIFIED, key + " did not fail the registration cell")

    def test_a_registration_for_another_boundary_may_carry_another_relationship(self):
        # Only the boundary owning the dispatched assignment is compared against its relationship
        # and generation; the other boundary legitimately has its own.
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["registration:B"]["value"], VERIFIED)

    def test_an_assignment_outside_every_declared_boundary_is_refused(self):
        self.world.record["assignment"]["issueKey"] = "NOT-A-BOUNDARY"
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("no declared boundary", refused.reason)


class ThirdHostedRound(TrialCase):
    """The findings the hosted reviewers returned on the second and third pushed heads."""

    def window_ledger(self, lines):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines(list(lines(now, opened, closed)))
        return now, opened, closed

    def test_a_capture_is_aged_at_the_moment_it_is_read(self):
        # The bound used to be compared against a clock sampled before the run, so a capture stayed
        # fresh for as long as the run took. The witness delay is real time inside one preflight.
        self.world.start_supervisor()
        self.world.record["captureMaxAgeSeconds"] = 1
        self.world.record["supervisor"]["witnessAdvanceSeconds"] = 2
        self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["capturedAt"] = (
            startup.stamp(time.time() - 0.5))
        self.world.flush()
        document = startup.preflight(
            startup.load_start(str(self.world.trial / "start.json"),
                               environment=self.world.environment()),
            sleeper=time.sleep)
        cell = cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertIn("seconds old", cell["evidence"])

    def test_a_boundary_that_is_not_an_object_is_named_rather_than_raised(self):
        self.world.record["boundaries"][1] = "not a boundary"
        self.world.flush()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 2, stderr)
        self.assertIn("boundary is not an object", payload["refused"])
        self.assertNotIn("raised before it could report", payload["refused"])

    def test_an_assignment_whose_participants_are_another_boundarys_is_refused(self):
        for key, value in (("parentTaskId", World.PARENT_B), ("childTaskId", World.CHILD_B)):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.record["assignment"][key] = value
            world.flush()
            try:
                startup.load_start(str(world.trial / "start.json"),
                                   environment=world.environment())
            except startup.Refused as refused:
                self.assertIn("is not the one its boundary declares", refused.reason)
            else:                                                    # pragma: no cover
                self.fail(key + " was accepted from another boundary")

    def test_two_boundaries_sharing_a_participant_are_not_two_parents(self):
        self.world.record["boundaries"][1]["participants"][0]["taskId"] = World.PARENT_A
        self.world.record["captures"]["creationReceipt"][World.PARENT_A] = (
            self.world.record["captures"]["creationReceipt"][World.PARENT_A])
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "boundaries")["declaration"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("two sharing a participant", cell["evidence"])

    def test_the_gate_refuses_roots_from_a_registration_of_another_relationship(self):
        self.world.captures["register-A.json"]["relationshipId"] = "rel-000000000000beef"
        self.world.flush()
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "registrationIdentity" and c["agrees"] is False])
        self.assertFalse([c for c in gate["comparisons"] if c["field"] == "artifact"])

    def test_an_unnormalised_artifact_path_is_refused(self):
        odd = str(self.world.repos["A"]) + "/sub/../artifact.py"
        self.world.record["assignment"]["artifacts"] = [odd]
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("normalised", refused.reason)
        for value in (str(self.world.repos["A"]) + "/", "~/artifact.py",
                      str(self.world.repos["A"]) + "//artifact.py"):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.record["assignment"]["artifacts"] = [value]
            world.flush()
            self.assertIsNotNone(world.refusal(), value + " was accepted")

    def test_an_unnormalised_artifact_in_the_assignment_file_fails_the_gate(self):
        odd = str(self.world.repos["A"]) + "/sub/../artifact.py"
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [odd]}), encoding="utf-8")
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "artifactIsCanonical" and c["agrees"] is False])

    def test_a_message_naming_a_longer_path_does_not_carry_the_artifact(self):
        self.world.message_file.write_text(
            "Work under relationship " + World.RELATIONSHIP + "x and emit "
            + str(self.world.artifact) + ".bak when ready.\n", encoding="utf-8")
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "messageCarriesArtifact" and c["agrees"] is False])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "messageCarriesRelationship" and c["agrees"] is False])

    def test_exact_naming_still_accepts_ordinary_prose(self):
        # The identities are named as words of their own; the prose around them is free.
        for around in ("relationship {id} and artifact {path} when ready",
                       "{id}\t{path}\n", "{id}\n{path}\n",
                       "emit\n  {path}\nunder\n  {id}\n"):
            self.world.message_file.write_text(
                around.format(id=World.RELATIONSHIP, path=str(self.world.artifact)),
                encoding="utf-8")
            gate = self.world.preflight()["orderGate"]
            self.assertTrue([c for c in gate["comparisons"]
                             if c["field"] == "messageCarriesArtifact" and c["agrees"] is True],
                            around)

    def test_a_window_whose_open_and_close_name_different_segments_is_refused(self):
        self.window_ledger(lambda now, opened, closed: [
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window-4"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window-5"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("different segments", raised.exception.reason)

    def test_a_preparation_segment_running_through_the_window_is_refused(self):
        self.window_ledger(lambda now, opened, closed: [
            {"at": startup.stamp(now - 300), "kind": "segment_start", "segment": "long"},
            {"at": startup.stamp(closed + 1), "kind": "segment_end", "segment": "long",
             "outcome": "failed"},
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("overlaps the trial window", raised.exception.reason)


class FourthHostedRound(TrialCase):
    """Symlinked artifacts, a point-sized window, and a boundary with two parents."""

    def test_an_artifact_reached_through_a_symlink_is_outside_the_root(self):
        # The relay compares normalised paths and never follows links, then opens every component
        # with O_NOFOLLOW, so a link outside the root whose target lands inside it is outside.
        outside = self.world.root / "linked-artifact.py"
        outside.symlink_to(self.world.artifact)
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [str(outside)]}), encoding="utf-8")
        self.world.record["assignment"]["artifacts"] = [str(outside)]
        self.world.message_file.write_text(
            "relationship " + World.RELATIONSHIP + " artifact " + str(outside) + "\n",
            encoding="utf-8")
        self.world.flush()
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "artifact" and c["agrees"] is False])

    def test_lexical_containment_answers_on_the_path_not_the_place(self):
        root = str(self.world.repos["A"])
        self.assertTrue(startup.lexically_within(root + "/artifact.py", root))
        self.assertTrue(startup.lexically_within(root, root))
        self.assertFalse(startup.lexically_within(str(self.world.root) + "/link.py", root))
        self.assertFalse(startup.lexically_within(root + "-next/artifact.py", root))
        self.assertFalse(startup.lexically_within("artifact.py", root))

    def test_private_containment_still_follows_the_link(self):
        # The two containments answer different questions, and this pins that they stay different.
        loose = self.world.root / "elsewhere.json"
        loose.write_text("{}", encoding="utf-8")
        link = self.world.trial / "link.json"
        link.symlink_to(loose)
        self.assertFalse(startup.within(str(link), self.world.trial))
        self.assertTrue(startup.lexically_within(str(link), str(self.world.trial)))

    def test_a_window_with_no_duration_is_refused(self):
        now = time.time()
        instant = startup.stamp(now - 30)
        self.world.record["window"] = {"opensAt": instant, "closesAt": instant}
        self.world.flush()
        self.world.ledger_lines([
            {"at": instant, "kind": "window_open", "segment": "window"},
            {"at": instant, "kind": "window_close", "segment": "window"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("no duration", raised.exception.reason)

    def test_a_boundary_with_two_parents_is_refused(self):
        boundary = self.world.record["boundaries"][0]
        boundary["participants"].append({
            "role": "parent", "taskId": "a-second-parent", "cwd": str(self.world.repos["A"]),
            "expect": dict(boundary["participants"][0]["expect"])})
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("exactly one parent", refused.reason)

    def test_a_boundary_with_no_child_is_refused(self):
        boundary = self.world.record["boundaries"][0]
        boundary["participants"] = [p for p in boundary["participants"] if p["role"] != "child"]
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("exactly one child", refused.reason)


class FifthHostedRound(TrialCase):
    """A NUL byte, a symlinked component, a per cent sign, and the boundary nobody checked."""

    def test_a_nul_byte_in_an_artifact_path_is_refused(self):
        self.world.record["assignment"]["artifacts"] = [str(self.world.artifact) + "\x00suffix"]
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("normalised absolute path", refused.reason)

    def test_an_artifact_under_a_symlinked_directory_fails_the_gate(self):
        # Lexical containment says the string starts beneath the root; the relay then opens every
        # component refusing to follow a link, so this is refused at emit however the string reads.
        real = self.world.repos["A"] / "real"
        real.mkdir()
        (real / "artifact.py").write_text("# real\n", encoding="utf-8")
        linked = self.world.repos["A"] / "linked"
        linked.symlink_to(real)
        artifact = str(linked / "artifact.py")
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [artifact]}), encoding="utf-8")
        self.world.record["assignment"]["artifacts"] = [artifact]
        self.world.message_file.write_text(
            "relationship " + World.RELATIONSHIP + " artifact " + artifact + "\n", encoding="utf-8")
        self.world.flush()
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "artifactFollowsNoLink" and c["agrees"] is False])

    def test_an_artifact_that_does_not_exist_yet_is_not_a_link_failure(self):
        # The child writes the artifact after this runs, so a component that is not there yet is
        # not an answer about links either way.
        future = str(self.world.repos["A"] / "not-written-yet.py")
        self.assertIsNone(startup.symlink_component(future))

    def test_a_message_naming_a_path_with_another_suffix_does_not_carry_it(self):
        for suffix in ("%backup", "@old", "=1", "\\\\copy", ".bak"):
            self.world.message_file.write_text(
                "relationship " + World.RELATIONSHIP + " artifact "
                + str(self.world.artifact) + suffix + "\n", encoding="utf-8")
            gate = self.world.preflight()["orderGate"]
            self.assertTrue([c for c in gate["comparisons"]
                             if c["field"] == "messageCarriesArtifact" and c["agrees"] is False],
                            suffix + " was read as naming the artifact")

    def test_a_secondary_boundary_with_two_parents_is_refused(self):
        boundary = self.world.record["boundaries"][1]
        boundary["participants"].append({
            "role": "parent", "taskId": "a-second-parent-b", "cwd": str(self.world.repos["B"]),
            "expect": dict(boundary["participants"][0]["expect"])})
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("exactly one parent", refused.reason)
        self.assertEqual(refused.detail["boundary"], "B")

    def test_a_secondary_boundary_with_no_child_is_refused(self):
        boundary = self.world.record["boundaries"][1]
        boundary["participants"] = [p for p in boundary["participants"] if p["role"] != "child"]
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("exactly one child", refused.reason)
        self.assertEqual(refused.detail["boundary"], "B")


class SixthHostedRound(TrialCase):
    """The launcher that could be swapped, the ledger that needed the relay, and the matching rule."""

    def test_the_declared_launcher_must_be_the_pointer_itself(self):
        link = self.world.root / "launcher-link"
        link.symlink_to(self.world.launcher)
        self.world.record["relay"]["launcher"] = str(link)
        self.world.record["relay"]["launcherSha256"] = startup.digest_of(link)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("host record names", refused.reason)

    def test_the_pointer_the_host_record_names_is_what_runs(self):
        record = startup.load_start(str(self.world.trial / "start.json"),
                                    environment=self.world.environment())
        self.assertEqual(record["_relay"]["launcher"], str(self.world.launcher))
        self.assertIn(str(self.world.launcher), record["_relay"]["namedBy"])

    def test_a_finished_trial_stays_gradable_when_the_installation_changes(self):
        now = time.time()
        opened, closed = now - 60, now - 5
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        # The relay is upgraded away after the window closed. Grading uses none of it.
        self.world.launcher.unlink()
        document = self.world.run_ledger()
        self.assertTrue(document["window"]["windowIsClean"])
        self.assertEqual(document["judgmentsThatFailed"], [])
        code, payload, stderr = self.world.run_cli("ledger")
        self.assertEqual(code, 0, stderr)

    def test_the_preflight_still_needs_the_installation(self):
        self.world.launcher.unlink()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 2, stderr)
        self.assertIn("launcher", payload["refused"])

    def test_a_path_the_message_only_contains_is_not_named(self):
        for suffix in ("!", "%backup", ".bak", ")", "x"):
            self.world.message_file.write_text(
                "relationship " + World.RELATIONSHIP + " artifact "
                + str(self.world.artifact) + suffix + "\n", encoding="utf-8")
            gate = self.world.preflight()["orderGate"]
            self.assertTrue([c for c in gate["comparisons"]
                             if c["field"] == "messageCarriesArtifact" and c["agrees"] is False],
                            suffix + " was read as naming the artifact")

    def test_an_artifact_whose_name_ends_in_a_bracket_is_named_when_written_as_a_word(self):
        odd = self.world.repos["A"] / "result)"
        odd.write_text("# odd\n", encoding="utf-8")
        self.world.assignment_file.write_text(json.dumps({
            "relationshipId": World.RELATIONSHIP, "childTaskId": World.CHILD_A,
            "executionGeneration": 1, "artifacts": [str(odd)]}), encoding="utf-8")
        self.world.record["assignment"]["artifacts"] = [str(odd)]
        self.world.message_file.write_text(
            "relationship " + World.RELATIONSHIP + " artifact " + str(odd) + "\n",
            encoding="utf-8")
        self.world.flush()
        gate = self.world.preflight()["orderGate"]
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "messageCarriesArtifact" and c["agrees"] is True],
                        json.dumps(gate["comparisons"]))


class SeventhHostedRound(TrialCase):
    """The state directory, the ledger's own path, and the parent's workspace."""

    def test_a_relay_state_directory_inside_a_worktree_is_refused(self):
        inside = self.world.repos["A"] / "state"
        inside.mkdir()
        self.world.record["relay"]["stateDirectory"] = str(inside)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("state directory is inside a git worktree", refused.reason)

    def test_a_ledger_linked_out_of_the_trial_root_is_refused(self):
        outside = self.world.root / "somebody-elses-ledger.jsonl"
        outside.write_text(json.dumps(
            {"at": startup.stamp(time.time() - 60), "kind": "window_open", "segment": "w"}) + "\n",
            encoding="utf-8")
        (self.world.trial / "ledger.jsonl").symlink_to(outside)
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("outside the trial root", raised.exception.reason)

    def test_a_registration_naming_another_parent_workspace_fails(self):
        self.world.captures["register-A.json"]["parent"]["cwd"] = str(self.world.repos["B"])
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"], NOT_VERIFIED)


class EighthHostedRound(TrialCase):
    """A workspace the receipt does not carry, and one that is not a place."""

    def test_a_receipt_without_a_workspace_is_unknown_rather_than_wrong(self):
        for side in ("parent", "child"):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.captures["register-A.json"][side].pop("cwd")
            world.flush()
            document = world.preflight()
            cell = cells_of(document, "boundaries")["registration:A"]
            self.assertEqual(cell["value"], UNKNOWN, side)
            self.assertIn("workspace", cell["evidence"])

    def test_a_workspace_that_is_not_absolute_is_not_a_place(self):
        for value in ("", ".", "repo-A"):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.captures["register-A.json"]["child"]["cwd"] = value
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"],
                             NOT_VERIFIED, repr(value) + " was read as a workspace")


class NinthHostedRound(TrialCase):
    """The recipients a registration authorises, and the challenge a peer was asked about."""

    def test_a_registration_that_does_not_authorise_both_endpoints_fails(self):
        for allowed in ([World.PARENT_A], [World.CHILD_A], ["somebody-else"], []):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.captures["register-A.json"]["authorizedScope"]["allowedRecipients"] = allowed
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"],
                             NOT_VERIFIED, json.dumps(allowed) + " was accepted")

    def test_a_peer_asked_about_another_challenge_is_not_this_trials_proof(self):
        self.world.captures["doctor-" + World.CHILD_A + ".json"]["nonce"] = {
            "nonce": "an-older-challenge", "found": True, "readable": True}
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "storeIdentity")["peer:" + World.CHILD_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("an-older-challenge", cell["evidence"])

    def test_a_peer_payload_without_its_challenge_is_unknown(self):
        self.world.captures["doctor-" + World.CHILD_A + ".json"].pop("nonce")
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "storeIdentity")["peer:" + World.CHILD_A]["value"],
                         UNKNOWN)


class TenthHostedRound(TrialCase):
    """A receipt missing a field its identity is decided from, and a pointer that moved."""

    def test_a_receipt_without_its_allowed_recipients_is_unknown(self):
        for path in (("authorizedScope", "allowedRecipients"), ("issueKey",), ("status",)):
            world = World(self.base)
            self.addCleanup(world.stop)
            node = world.captures["register-A.json"]
            for key in path[:-1]:
                node = node[key]
            node.pop(path[-1])
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"],
                             UNKNOWN, ".".join(path) + " was read as a disagreement")

    def test_the_gate_does_not_take_roots_from_a_receipt_it_could_not_identify(self):
        self.world.captures["register-A.json"]["authorizedScope"].pop("allowedRecipients")
        self.world.flush()
        gate = self.world.preflight()["orderGate"]
        self.assertFalse(gate["passed"])
        self.assertTrue([c for c in gate["comparisons"]
                         if c["field"] == "registrationIdentity" and c["agrees"] is False])

    def test_the_launcher_is_read_again_after_the_probes(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["launcherStillTheSameBytes"]["passed"])
        self.assertEqual(document["launcherStillTheSameBytes"]["before"],
                         document["launcherStillTheSameBytes"]["after"])

    def test_a_pointer_moved_during_the_run_is_reported(self):
        self.world.start_supervisor()

        def move(seconds):
            # The pointer is meant to move on update; this is the moment an update would do it.
            self.world.launcher.write_text(LAUNCHER + "\n# a different build\n", encoding="utf-8")
            self.world.launcher.chmod(0o755)
            # And the run's own pause still has to happen, or the witness would not advance and
            # this case would fail for that instead.
            time.sleep(min(seconds, 0.4))

        document = self.world.preflight_with(move)
        self.assertFalse(document["launcherStillTheSameBytes"]["passed"])
        self.assertIn("launcherStillTheSameBytes.passed", document["judgmentsThatFailed"])
        for name in READINGS:
            self.assertEqual(document["readings"][name]["value"], VERIFIED,
                             name + " failed, so this case would not have shown what it claims")
        self.assertTrue(document["orderGate"]["passed"])
        self.assertFalse(document["readyToStart"])


class EleventhHostedRound(TrialCase):
    """Readiness that outran its own judgment, and two more fields a receipt may not carry."""

    def test_an_assigned_receipt_without_its_relationship_is_unknown(self):
        for name in ("relationshipId", "executionGeneration"):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.captures["register-A.json"].pop(name)
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "boundaries")["registration:A"]["value"],
                             UNKNOWN, name + " was read as a disagreement")

    def test_the_other_boundarys_receipt_needs_neither_of_them(self):
        # Only the boundary owning the dispatched assignment is compared against a relationship.
        self.world.captures["register-B.json"].pop("relationshipId")
        self.world.captures["register-B.json"].pop("executionGeneration")
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "boundaries")["registration:B"]["value"], VERIFIED)

    def test_readiness_includes_every_judgment_the_document_carries(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["readyToStart"])
        self.assertEqual(document["judgmentsThatFailed"], [])
        # Readiness and the judgment walk answer together: neither may say yes while the other
        # says no, which is what readiness outrunning launcherStillTheSameBytes did.
        self.assertEqual(document["readyToStart"], not document["judgmentsThatFailed"])


class TwelfthHostedRound(TrialCase):
    """The socket, the decoy assignment file, the child nobody looked up, and the A-B-A move."""

    def test_an_unreachable_socket_fails_the_store_reading(self):
        for answer in ("refused", "timeout", None):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.payloads["doctor"]["payload"]["actorReachability"] = {"socketConnect": answer}
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "storeIdentity")["socketReachable"]["value"],
                             NOT_VERIFIED, repr(answer) + " was accepted as reachable")

    def test_a_doctor_payload_without_reachability_is_unknown(self):
        self.world.payloads["doctor"]["payload"].pop("actorReachability")
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "storeIdentity")["socketReachable"]["value"], UNKNOWN)

    def test_an_assignment_file_outside_the_owning_childs_workspace_is_refused(self):
        decoy = self.world.repos["B"] / "assignment.json"
        decoy.write_text((self.world.repos["A"] / "assignment.json").read_text(encoding="utf-8"),
                         encoding="utf-8")
        self.world.record["assignment"]["assignmentFile"] = str(decoy)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("owning child's workspace", refused.reason)

    def test_every_participant_needs_a_lifecycle_capture_not_only_the_parents(self):
        (self.world.trial / ("lifecycle-" + World.CHILD_A + ".json")).unlink()
        document = self.world.preflight()
        cells = cells_of(document, "parentLifecycle")
        self.assertEqual(len(cells), 4)
        self.assertEqual(cells["lifecycle:" + World.CHILD_A]["value"], UNKNOWN)
        self.assertFalse(document["readyToStart"])

    def test_a_child_with_no_rollout_fails_its_own_reading(self):
        self.world.captures["lifecycle-" + World.CHILD_A + ".json"] = {
            "threadId": World.CHILD_A, "status": "unknown", "error": "thread not found"}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.CHILD_A]["value"],
            NOT_VERIFIED)

    def test_a_replacement_present_at_a_later_probe_is_caught(self):
        # The stub replaces itself after its first call, which is a pointer moved mid-run.
        self.world.start_supervisor()
        (self.world.bin / "rewrite-after").write_text("1", encoding="utf-8")
        document = self.world.preflight()
        launcher = document["launcherStillTheSameBytes"]
        self.assertFalse(launcher["passed"])
        self.assertGreater(len(launcher["beforeEachProbe"]), 1)
        self.assertFalse(document["readyToStart"])

    def test_a_replacement_reverted_between_two_readings_is_not_claimed(self):
        # The stated limit, asserted rather than left implied: nothing spawns while it is changed,
        # so no reading sees it, and the document says whose witness that is.
        self.world.start_supervisor()
        original = self.world.launcher.read_text(encoding="utf-8")

        def swap(seconds):
            self.world.launcher.write_text(original + "\n# briefly another build\n",
                                           encoding="utf-8")
            self.world.launcher.chmod(0o755)
            time.sleep(min(seconds, 0.4))
            self.world.launcher.write_text(original, encoding="utf-8")
            self.world.launcher.chmod(0o755)

        document = self.world.preflight_with(swap)
        launcher = document["launcherStillTheSameBytes"]
        self.assertTrue(launcher["passed"])
        self.assertIn("CRW-102", launcher["detail"])


class ThirteenthHostedRound(TrialCase):
    """The last round: a shared capture, a relative socket, a boolean counter, a split ledger."""

    def test_one_capture_listed_for_several_participants_is_not_several_readings(self):
        shared = str(self.world.trial / ("doctor-" + World.PARENT_A + ".json"))
        for task in (World.PARENT_A, World.CHILD_A):
            self.world.record["captures"]["peerDoctor"][task]["path"] = shared
        self.world.flush()
        document = self.world.preflight()
        for task in (World.PARENT_A, World.CHILD_A):
            cell = cells_of(document, "storeIdentity")["peer:" + task]
            self.assertEqual(cell["value"], NOT_VERIFIED)
            self.assertIn("counted as", cell["evidence"])

    def test_a_relative_socket_is_refused(self):
        self.world.record["relay"]["socket"] = "sock"
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("absolute", refused.reason)

    def test_a_boolean_counter_is_not_progress(self):
        pid = self.world.start_supervisor()
        self.world.supervisor.terminate()
        self.world.supervisor.wait(timeout=5)
        witness = self.world.trial / "supervisor.jsonl"

        def write(lines):
            witness.write_text("".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")

        write([{"pid": pid, "progress": False}])
        document = self.world.preflight_with(
            lambda seconds: write([{"pid": pid, "progress": False},
                                   {"pid": pid, "progress": True}]))
        self.assertEqual(cells_of(document, "processPersistence")["witnessAdvance"]["value"],
                         NOT_VERIFIED)

    def test_the_probes_carry_the_state_environment_as_well_as_the_flag(self):
        self.world.preflight()
        seen = json.loads(self.world.calls.read_text().splitlines()[0])
        self.assertIn("--state", seen["argv"])
        relay = startup.Relay(startup.load_start(str(self.world.trial / "start.json"),
                                                 environment=self.world.environment()))
        self.assertEqual(relay.environment["CODEX_SESSION_RELAY_STATE"], str(relay.state))

    def test_a_settings_payload_missing_its_task_or_its_missing_list_is_unknown(self):
        for key in ("task", "missing", "usable"):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.payloads["settings-show"]["payload"].pop(key, None)
            world.payloads["settings-show"]["stdout"] = json.dumps(
                world.payloads["settings-show"]["payload"])
            world.flush()
            document = world.preflight()
            self.assertEqual(
                cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]["value"],
                UNKNOWN, key + " was read as a disagreement")

    def test_readiness_is_the_judgment_walks_own_answer(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["readyToStart"])
        self.world.payloads["doctor"]["payload"]["sameStore"] = "unproven"
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"])
        self.assertEqual(document["readyToStart"], not document["judgmentsThatFailed"])


class FourteenthHostedRound(TrialCase):
    """What a peer capture can and cannot establish, and a path the message could not name."""

    def test_identical_peer_payloads_are_reported_rather_than_graded(self):
        # doctor does not name the participant that ran it, so two peers legitimately produce the
        # same bytes. The cell says so instead of failing a start for it.
        document = self.world.preflight()
        cell = cells_of(document, "storeIdentity")["peer:" + World.CHILD_A]
        self.assertEqual(cell["value"], VERIFIED)
        self.assertIn("identical to", cell["evidence"])
        self.assertIn("does not\n" if False else "does not", cell["evidence"])

    def test_the_attribution_of_a_peer_capture_is_recorded_as_a_stand_in(self):
        document = self.world.preflight()
        self.assertIn("peerAttribution", document["standIns"])
        self.assertIn("operator's attribution", document["standIns"]["peerAttribution"])

    def test_an_artifact_path_with_whitespace_is_refused(self):
        spaced = str(self.world.repos["A"] / "output file.txt")
        self.world.record["assignment"]["artifacts"] = [spaced]
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("whitespace", refused.reason)


class FifteenthHostedRound(TrialCase):
    """Two spellings of one file are one file."""

    def test_a_peer_capture_reached_by_a_second_name_is_one_reading(self):
        for make in ("symlink", "hardlink"):
            world = World(self.base)
            self.addCleanup(world.stop)
            first = world.trial / ("doctor-" + World.PARENT_A + ".json")
            second = world.trial / "doctor-under-another-name.json"
            if make == "symlink":
                second.symlink_to(first)
            else:
                os.link(str(first), str(second))
            world.record["captures"]["peerDoctor"][World.CHILD_A]["path"] = str(second)
            world.flush()
            document = world.preflight()
            for task in (World.PARENT_A, World.CHILD_A):
                cell = cells_of(document, "storeIdentity")["peer:" + task]
                self.assertEqual(cell["value"], NOT_VERIFIED, make + " for " + task)
                self.assertIn("counted as", cell["evidence"])


class SixteenthHostedRound(TrialCase):
    """A record whose window has closed, an unreadable alias, and an unreadable pre-spawn read."""

    def test_a_record_whose_window_has_closed_cannot_be_started_from(self):
        now = time.time()
        self.world.record["window"] = {"opensAt": startup.stamp(now - 600),
                                       "closesAt": startup.stamp(now - 60)}
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("already opened", refused.reason)

    def test_the_ledger_still_grades_that_same_record(self):
        now = time.time()
        opened, closed = now - 600, now - 60
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
        ])
        document = self.world.run_ledger()
        self.assertTrue(document["window"]["windowIsClean"])

    def test_two_captures_that_cannot_be_statted_are_not_one_file(self):
        gone = str(self.world.trial / "not-there.json")
        self.assertFalse(startup.same_file(gone, str(self.world.trial / "also-not-there.json")))
        self.assertTrue(startup.same_file(gone, gone))

    def test_a_launcher_that_could_not_be_read_before_a_spawn_fails(self):
        self.world.start_supervisor()
        relay = startup.Relay(startup.load_start(str(self.world.trial / "start.json"),
                                                 environment=self.world.environment()))
        relay.digests.add(None)
        record = startup.load_start(str(self.world.trial / "start.json"),
                                    environment=self.world.environment())
        answer = startup.launcher_unchanged(record, relay)
        self.assertFalse(answer["passed"])
        self.assertTrue(answer["unreadableBeforeAProbe"])


class SeventeenthHostedRound(TrialCase):
    """A trial already running, a window that is not there, and identities left blank."""

    def test_a_trial_already_running_cannot_be_started_again(self):
        now = time.time()
        self.world.record["window"] = {"opensAt": startup.stamp(now - 60),
                                       "closesAt": startup.stamp(now + 600)}
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("already opened", refused.reason)

    def test_a_record_with_no_window_or_an_unreadable_one_is_refused(self):
        for window in (None, {}, {"opensAt": "not a time", "closesAt": "also not"},
                       {"opensAt": startup.stamp(time.time() + 60)},
                       "a window"):
            world = World(self.base)
            self.addCleanup(world.stop)
            if window is None:
                world.record.pop("window")
            else:
                world.record["window"] = window
            world.flush()
            self.assertIsNotNone(world.refusal(), repr(window) + " was accepted")

    def test_a_window_with_no_duration_is_refused_at_the_start_too(self):
        instant = startup.stamp(time.time() + 120)
        self.world.record["window"] = {"opensAt": instant, "closesAt": instant}
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("no duration", refused.reason)

    def test_blank_boundary_identities_are_not_identities(self):
        for key in ("issueKey", "scopeRef"):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.record["boundaries"][1][key] = ""
            world.captures["register-B.json"]["issueKey"] = (
                "" if key == "issueKey" else world.captures["register-B.json"]["issueKey"])
            world.captures["register-B.json"]["authorizedScope"]["scopeRef"] = (
                "" if key == "scopeRef"
                else world.captures["register-B.json"]["authorizedScope"]["scopeRef"])
            world.flush()
            document = world.preflight()
            self.assertEqual(cells_of(document, "boundaries")["declaration"]["value"],
                             NOT_VERIFIED, "a blank " + key + " was read as an identity")


if __name__ == "__main__":                                           # pragma: no cover
    unittest.main()
