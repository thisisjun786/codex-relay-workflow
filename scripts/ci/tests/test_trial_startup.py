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
import signal
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

import trial_startup as startup  # noqa: E402

# The whole object a creation response reports, which is what a resume is checked against.
PROFILE = {"id": "profile-1", "name": "a profile", "extends": None, "rules": []}

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
# A trial can ask this stub to answer differently once a subcommand has been asked a number of
# times, which is what a row another process replaces partway through the run looks like from the
# caller's side. Counting that subcommand rather than every call is what makes the trigger mean
# "after the reading that already graded it" instead of "at some point".
after = payloads.get("after")
if isinstance(after, dict):
    made = len([c for c in (json.loads(l) for l in (here / "calls.jsonl").read_text().splitlines()
                            if l.strip()) if c["subcommand"] == after.get("subcommand")])
    if made > int(after.get("calls", 0)):
        for name, replacement in (after.get("payloads") or {}).items():
            payloads[name] = replacement
        entry = payloads.get(subcommand)
if entry is None:
    sys.stderr.write("no payload for " + subcommand + "\n")
    raise SystemExit(9)
payload = entry.get("payload", {})
if subcommand == "settings-show" and isinstance(payload, dict) and "--task" in argv:
    # The real command answers about the task it was asked about, so a stub that always answered
    # about one task would hide a checker that never compared the identity.
    asked = argv[argv.index("--task") + 1]
    payload = dict(payload, task=asked)
    workspaces = payloads.get("taskCwd") or {}
    if asked in workspaces and isinstance(payload.get("settings"), dict):
        payload["settings"] = dict(payload["settings"], cwd=workspaces[asked])
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

    def __init__(self, base, *, one_repository=False):
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
            if one_repository and name == "B":
                # B is a linked worktree of A: its own root, the same repository. Every checkout
                # on the host these trials run on is arranged this way, so a boundary declaration
                # meets it as the ordinary case rather than an exotic one. git needs a commit to
                # branch a worktree from, which is the only way this world differs elsewhere.
                identity = ["-c", "user.name=trial", "-c", "user.email=trial@example.invalid"]
                subprocess.run(["git", "-C", str(self.repos["A"]), *identity, "commit", "-q",
                                "--allow-empty", "-m", "a root to add a worktree from"],
                               check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                subprocess.run(["git", "-C", str(self.repos["A"]), "worktree", "add", "-q",
                                "-b", "linked", str(repo)],
                               check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            else:
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
        launched = time.time()
        self.supervisor = subprocess.Popen(
            [sys.executable, "-c", WITNESS_WRITER, str(witness)], start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.time() + 5
        while time.time() < deadline and not witness.exists():
            time.sleep(0.02)
        # The record declares a positive minimum uptime, because a bound of zero is met by a
        # process that started this instant and would report a persistence nobody observed. So
        # this waits for the supervisor to actually reach the fixture's bound instead of
        # declaring one nothing has to meet. Capped, because a case that declares a large bound
        # is asserting the refusal rather than waiting for it.
        #
        # Waited on the same measurement the reading uses, the process's own start time, and not
        # on when this call reached Popen. The two differ by however long the child took to
        # exist, which is small here and was not small on a loaded CI runner: the uptime cell
        # read under the bound there while this waited above it.
        wanted = min(self.record["supervisor"]["minimumAliveSeconds"], 1) + 0.1
        deadline = time.time() + 10
        while time.time() < deadline:
            started = startup.process_started_at(self.supervisor.pid)
            if started is None:
                # No /proc, so the reading uses the record's own launchedAt instead and this
                # wait has nothing to measure against.
                break
            if time.time() - started >= wanted:
                break
            time.sleep(0.02)
        self.record["supervisor"]["pid"] = self.supervisor.pid
        self.flush()
        return self.supervisor.pid

    def _write_host_record(self):
        directory = self.state / "codex-relay-workflow"
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "host-record.json").write_text(json.dumps({
            "recordVersion": 1, "definitionVersion": 1, "host": "test", "user": "test",
            # The shape runtime_install.py actually writes: an install's location is where the
            # module went, its entryPoint is that environment's own console script, and the
            # owned pointer is recorded separately as <destination>/current. A fixture that put
            # a pointer under the location would have passed a checker no real install can.
            "pointer": {"path": str(self.install / "current"), "target": str(self.install / "env")},
            "components": {"codex-session-relay": {
                "installs": [{
                    "location": str(self.install / "env" / "lib" / "python3.13"
                                    / "site-packages" / "codex_session_relay"),
                    "entryPoint": str(self.install / "env" / "bin" / "codex-session-relay"),
                    "environment": str(self.install / "env"),
                }],
                "measuredPoints": []}},
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
            # Each participant's own workspace, because the store records one per task and the
            # checker compares the one the record states for that participant.
            "taskCwd": {self.PARENT_A: str(self.repos["A"]), self.CHILD_A: str(self.repos["A"]),
                        self.PARENT_B: str(self.repos["B"]), self.CHILD_B: str(self.repos["B"])},
            "doctor": {"payload": {
                "sameStore": "proven",
                "store": {"storeId": self.STORE_ID, "device": self.DEVICE, "inode": self.INODE,
                          "createdAt": "2020-01-01T00:00:00Z",
                          # The database itself, not only the directory around it: every command
                          # opens it read-write on construction.
                          "observedAccess": {"read": True, "write": True,
                                             "directoryWritable": True}},
                "ledger": {"configured": True, "split": False},
                "actorReachability": {"socketConnect": "ok", "stateDirectoryWritable": True},
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
        return {"model": "a-model", "reasoningEffort": "xhigh", "sandbox": {"type": "dangerFullAccess"},
                "approvalPolicy": "never", "cwd": str(self.root / "workspace"),
                "runtimeWorkspaceRoots": [], "environments": [],
                "expectedPermissionProfile": PROFILE}

    def _write_captures(self):
        settings = self._settings()
        for task in (self.PARENT_A, self.CHILD_A, self.PARENT_B, self.CHILD_B):
            # The shape the bridge actually writes. Its observable list has no environments in
            # it, because the host reports the environment selection on the created thread, so a
            # receipt carries it there and not under settings.actual. A tidier fixture would have
            # made a cell pass here that refuses every real receipt.
            echo = {key: settings[key] for key in ("model", "reasoningEffort", "sandbox",
                                                   "approvalPolicy", "runtimeWorkspaceRoots")}
            echo["cwd"] = self.payloads["taskCwd"][task]
            # What the contract carries: approvalPolicy is decided first and alone and is not
            # one of the settings a creation asks for, so it is absent from requested and from
            # the list the receipt says it verified.
            asked = {key: value for key, value in echo.items()
                     if key in ("cwd", "model", "reasoningEffort", "runtimeWorkspaceRoots",
                                "sandbox")}
            self.captures["receipt-" + task + ".json"] = {
                # The bridge names the thread it created at threadId and writes no taskId at
                # all. The fixture preferred the relay's own word for the same participant, so a
                # reading that refuses every receipt a real bridge writes passed here.
                "threadId": task,
                # The profile arrives raw beside the thread, not inside it, and the store's
                # expectation is the whole object rather than an id-shaped stand-in.
                "creation": {"thread": {"id": task, "environments": settings["environments"]},
                             "activePermissionProfile": PROFILE},
                "settings": {"requested": asked, "actual": echo, "findings": [],
                             "verified": sorted(asked)}}
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
                # OPS-3.5: every relay command opens the store, so a peer that cannot write the
                # state directory cannot run one. A real peer's doctor reports both.
                "actorReachability": {"socketConnect": "ok", "stateDirectoryWritable": True},
                "store": {"storeId": self.STORE_ID, "device": self.DEVICE,
                          "inode": self.INODE,
                          "observedAccess": {"read": True, "write": True,
                                             "directoryWritable": True}},
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
                                    "sandbox": {"type": "dangerFullAccess"}, "approvalPolicy": "never"}},
                        {"role": "child", "taskId": child, "cwd": str(self.repos[name]),
                         "expect": {"model": "a-model", "reasoningEffort": "xhigh",
                                    "sandbox": {"type": "dangerFullAccess"}, "approvalPolicy": "never"}}]}

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
                           "launchedAt": startup.stamp(now - 120), "minimumAliveSeconds": 0.05,
                           "witnessAdvanceSeconds": 0.05, "service": False},
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
        return startup.preflight(record, sleeper=lambda seconds: time.sleep(min(seconds, 0.1)))

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
        # The window opens at the dispatch, so a ledger carries one. Supplied automatically at
        # the window's own open unless a case writes its own, which keeps every other case about
        # the thing it is testing.
        if not any(line.get("kind") == "dispatch" for line in lines):
            opens = next((line for line in lines if line.get("kind") == "window_open"), None)
            if opens is not None:
                lines = [{"at": opens["at"], "kind": "dispatch"}] + list(lines)
        (self.trial / "ledger.jsonl").write_text(
            "".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")

    def run_ledger(self):
        record = startup.load_start(str(self.trial / "start.json"),
                                    environment=self.environment(), mode="ledger")
        record["_now"] = startup.datetime.datetime.now(startup.datetime.timezone.utc)
        return startup.ledger(record)


def relay_source(*parts):
    return (ROOT.joinpath(*parts)).read_text(encoding="utf-8")


def assigned_literal(source, name):
    """A module-level constant read as a value rather than matched as text.

    This repository does not accept text matching as proof of behaviour, and a drift guard on a
    contract copied from another lane is exactly where a substring would rot: an equivalent
    rewrite fails it while a real change to the value slips past. So the value is parsed.
    """
    tree = ast.parse(source)
    return next(ast.literal_eval(node.value) for node in ast.walk(tree)
                if isinstance(node, ast.Assign)
                and [target for target in node.targets
                     if getattr(target, "id", None) == name])


def constants_in(source, function):
    """Every string constant a named function mentions, for the same reason."""
    tree = ast.parse(source)
    node = next(n for n in ast.walk(tree)
                if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == function)
    return {c.value for c in ast.walk(node)
            if isinstance(c, ast.Constant) and isinstance(c.value, str)}


def cells_of(document, reading):
    return {c["cell"]: c for c in document["readings"][reading]["cells"]}


def process_state_of(pid):
    """The kernel's state letter for a process, read here rather than through the module.

    A case about a state the module did not read yet has to be able to find that state at the
    commit before it could.
    """
    try:
        with open("/proc/" + str(pid) + "/stat", encoding="utf-8") as handle:
            return handle.read().rsplit(")", 1)[1].split()[0]
    except (OSError, ValueError, IndexError):
        return None



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
            "actorReachability": {"socketConnect": "ok", "stateDirectoryWritable": True},
            "store": {"storeId": "another-store", "device": 1, "inode": 2,
                      "observedAccess": {"read": True, "write": True,
                                         "directoryWritable": True}},
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
        ("cli.py", "_reachability"): ("socketConnect", "stateDirectoryWritable"),
        ("cli.py", "_ledger_location"): ("configured", "split"),
        ("store.py", "probe"): ("store", "storeId", "createdAt", "device", "inode"),
        ("cli.py", "_access_receipt"): ("observedAccess", "write"),
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
        own |= {"closesAt", "opensAt"}
        # The bridge's creation receipt, which is a capture rather than a relay payload. Its
        # observable list builds settings.actual and has no environments in it, so the host's
        # environment selection is read from the created thread. That claim is asserted against
        # the bridge's own source in ThirtyFirstHostedRound rather than taken on trust here.
        # verified is the same receipt's own list of the settings it was asked for and answered
        # on, which is what says empty findings established them; the keys that list can hold
        # are asserted against the contract's own source in FortyFifthHostedRound.
        own |= {"creation", "thread", "environments", "verified"}
        # The runtime host record's own shape, which is not a relay payload either: the owned
        # pointer is what says which command a host reaches the runtime through.
        own |= {"pointer"}
        # The permission profile, which the bridge passes through raw: the creation
        # response reports it and the settings record carries the expectation.
        own |= {"activePermissionProfile", "permissionReceipt",
                "expectedPermissionProfile"}
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
        self.world.captures["receipt-" + World.PARENT_A + ".json"]["threadId"] = "somebody-else"
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
        # a string rejected every capture a real host produces. The state sits at "type", which is
        # where the relay's own adapter reads it.
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A,
            "status": {"type": "idle", "activeTurn": None}, "goal": None}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
            VERIFIED)

    def test_an_empty_structured_status_is_still_not_resolved(self):
        # An earlier round pinned this as not_verified. A later one found that an object with
        # keys but no state passed, and the two cases are one question: whether a state can be
        # read at all. Both are now unreadable, and both still refuse the start.
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A, "status": {}}
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertFalse(cell["met"])

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
        # A close still ahead is a line dated after the grading, which is refused first and says
        # the same thing about the same ledger.
        self.assertIn("dated after the time it is being graded", raised.exception.reason)

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
        self.world.record["supervisor"]["witnessAdvanceSeconds"] = 1.2
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
            time.sleep(min(seconds, 0.1))

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
            time.sleep(min(seconds, 0.1))
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


class EighteenthHostedRound(TrialCase):
    """A window that opened while the run was working, and a line dated after its own grading."""

    def test_readiness_is_refused_when_the_window_opens_during_the_run(self):
        # A window a moment ahead, and a witness observation longer than that moment.
        self.world.start_supervisor()
        self.world.record["window"] = {"opensAt": startup.stamp(time.time() + 0.5),
                                       "closesAt": startup.stamp(time.time() + 600)}
        self.world.record["supervisor"]["witnessAdvanceSeconds"] = 1.2
        self.world.flush()
        document = startup.preflight(
            startup.load_start(str(self.world.trial / "start.json"),
                               environment=self.world.environment()),
            sleeper=time.sleep)
        self.assertFalse(document["windowStillAhead"]["passed"])
        self.assertIn("windowStillAhead.passed", document["judgmentsThatFailed"])
        self.assertFalse(document["readyToStart"])

    def test_the_window_is_still_ahead_on_an_ordinary_run(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["windowStillAhead"]["passed"])
        self.assertTrue(document["readyToStart"])

    def test_a_ledger_line_dated_after_its_grading_is_refused(self):
        now = time.time()
        opened, closed = now - 600, now - 60
        self.world.record["window"] = {"opensAt": startup.stamp(opened),
                                       "closesAt": startup.stamp(closed)}
        self.world.flush()
        self.world.ledger_lines([
            {"at": startup.stamp(opened), "kind": "window_open", "segment": "window"},
            {"at": startup.stamp(closed), "kind": "window_close", "segment": "window"},
            {"at": "2099-01-01T00:00:00Z", "kind": "intervention", "segment": "window",
             "actor": "operator", "target": "task", "action": "impossible"},
        ])
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("dated after the time it is being graded", raised.exception.reason)


class NineteenthHostedRound(TrialCase):
    """A window that is ahead, but not the one this dispatch opens."""

    def test_a_window_opening_hours_later_is_not_this_dispatchs(self):
        self.world.record["window"] = {"opensAt": startup.stamp(time.time() + 86400),
                                       "closesAt": startup.stamp(time.time() + 90000)}
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("too long after this preflight", refused.reason)
        self.assertEqual(refused.detail["allowanceSeconds"], startup.WINDOW_ALLOWANCE)

    def test_a_window_inside_the_allowance_is_accepted(self):
        self.world.start_supervisor()
        self.world.record["window"] = {
            "opensAt": startup.stamp(time.time() + startup.WINDOW_ALLOWANCE - 30),
            "closesAt": startup.stamp(time.time() + 3600)}
        self.world.flush()
        document = self.world.preflight()
        self.assertTrue(document["windowStillAhead"]["passed"])
        self.assertTrue(document["readyToStart"])


class TwentiethHostedRound(TrialCase):
    """A repository nested inside the trial root, and a refusal phrase in ordinary prose."""

    def test_a_private_record_inside_a_nested_worktree_is_refused(self):
        nested = self.world.trial / "nested-repo"
        nested.mkdir()
        subprocess.run(["git", "init", "-q", str(nested)], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        moved = nested / "lifecycle.json"
        moved.write_text(json.dumps({"threadId": World.PARENT_A, "status": "idle"}),
                         encoding="utf-8")
        self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["path"] = str(moved)
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused)
        self.assertIn("inside a git worktree", refused.reason)

    def test_a_refusal_phrase_in_ordinary_content_is_not_a_refusal(self):
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A, "status": "idle",
            "goal": {"objective": "Investigate why thread not found was reported yesterday"}}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
            VERIFIED)

    def test_the_same_phrase_in_an_error_field_still_refuses(self):
        for where in ({"error": "thread not found"},
                      {"error": {"message": "thread not found"}},
                      {"message": "no rollout found"},
                      {"detail": "missing source rollout"}):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.captures["lifecycle-" + World.PARENT_A + ".json"] = dict(
                {"threadId": World.PARENT_A, "status": "unknown"}, **where)
            world.flush()
            document = world.preflight()
            self.assertEqual(
                cells_of(document, "parentLifecycle")["lifecycle:" + World.PARENT_A]["value"],
                NOT_VERIFIED, json.dumps(where))


class TwentyFirstHostedRound(TrialCase):
    """The two private paths the containment check had exempted."""

    def nested_trial(self):
        nested = self.world.trial / "nested-repo"
        nested.mkdir()
        subprocess.run(["git", "init", "-q", str(nested)], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        return nested

    def test_a_start_record_inside_a_nested_worktree_is_refused(self):
        nested = self.nested_trial()
        moved = nested / "start.json"
        moved.write_text((self.world.trial / "start.json").read_text(encoding="utf-8"),
                         encoding="utf-8")
        try:
            startup.load_start(str(moved), environment=self.world.environment())
        except startup.Refused as refused:
            self.assertIn("inside a git worktree", refused.reason)
        else:                                                        # pragma: no cover
            self.fail("a start record inside a nested worktree was accepted")

    def test_a_ledger_linked_into_a_nested_worktree_is_refused(self):
        nested = self.nested_trial()
        inside = nested / "ledger.jsonl"
        inside.write_text("", encoding="utf-8")
        (self.world.trial / "ledger.jsonl").symlink_to(inside)
        with self.assertRaises(startup.Refused) as raised:
            self.world.run_ledger()
        self.assertIn("inside a git worktree", raised.exception.reason)


class TwentySecondHostedRound(TrialCase):
    """Uptime that belongs to the pid, and captures aged again at the end."""

    def test_uptime_is_the_observed_processs_own(self):
        # The record declares a launch two minutes ago; the process started a moment ago, and a
        # restarted supervisor keeps the old declaration.
        self.world.start_supervisor()
        self.world.record["supervisor"]["minimumAliveSeconds"] = 60
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "processPersistence")["uptime"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("this process has been running", cell["evidence"])

    def test_uptime_is_unknown_where_the_host_cannot_say(self):
        self.assertIsNone(startup.process_started_at(0))
        self.assertIsNotNone(startup.process_started_at(os.getpid()))

    def test_a_capture_that_expires_during_the_run_fails_at_the_end(self):
        self.world.start_supervisor()
        self.world.record["captureMaxAgeSeconds"] = 1
        self.world.record["supervisor"]["witnessAdvanceSeconds"] = 1.5
        for kind in ("parentLifecycle", "creationReceipt", "registration", "peerDoctor"):
            for name in self.world.record["captures"][kind]:
                self.world.record["captures"][kind][name]["capturedAt"] = startup.stamp(
                    time.time() - 0.2)
        self.world.flush()
        document = startup.preflight(
            startup.load_start(str(self.world.trial / "start.json"),
                               environment=self.world.environment()),
            sleeper=time.sleep)
        self.assertFalse(document["capturesStillFresh"]["passed"])
        self.assertTrue(document["capturesStillFresh"]["stale"])
        self.assertIn("capturesStillFresh.passed", document["judgmentsThatFailed"])
        self.assertFalse(document["readyToStart"])

    def test_captures_well_inside_the_bound_stay_fresh(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["capturesStillFresh"]["passed"])
        self.assertEqual(document["capturesStillFresh"]["stale"], [])
        self.assertTrue(document["readyToStart"])


class TwentyThirdHostedRound(TrialCase):
    """A host without /proc, and a setting that is an object."""

    def test_a_host_that_cannot_report_process_start_uses_the_declaration_and_says_so(self):
        self.world.start_supervisor()
        self.world.record["supervisor"]["minimumAliveSeconds"] = 60
        self.world.flush()
        record = startup.load_start(str(self.world.trial / "start.json"),
                                    environment=self.world.environment())
        record["_now"] = startup.datetime.datetime.now(startup.datetime.timezone.utc)
        original = startup.process_started_at
        startup.process_started_at = lambda pid: None
        try:
            document = startup.preflight(record, sleeper=lambda seconds: time.sleep(0.4))
        finally:
            startup.process_started_at = original
        cell = cells_of(document, "processPersistence")["uptime"]
        # The record declares two minutes, so it passes there, and the cell says which it read.
        self.assertEqual(cell["value"], VERIFIED)
        self.assertEqual(cell["provenance"], "captured")
        self.assertIn("does not report when a process started", cell["evidence"])

    def test_a_structured_setting_is_compared_structurally(self):
        sandbox = {"type": "workspaceWrite", "networkAccess": False}
        reordered = {"networkAccess": False, "type": "workspaceWrite"}
        self.assertTrue(startup.same_value(sandbox, reordered))
        self.assertFalse(startup.same_value(sandbox, str(sandbox)))
        self.assertFalse(startup.same_value(sandbox, {"type": "workspaceWrite"}))
        self.assertTrue(startup.same_value("1", 1))
        self.assertFalse(startup.same_value(True, "True"))

    def test_an_object_valued_expect_agrees_whatever_the_key_order(self):
        sandbox = {"type": "workspaceWrite", "networkAccess": False}
        for participant in self.world.record["boundaries"][0]["participants"]:
            participant["expect"]["sandbox"] = dict(sandbox)
        for task in (World.PARENT_A, World.CHILD_A):
            self.world.captures["receipt-" + task + ".json"]["settings"]["actual"]["sandbox"] = {
                "networkAccess": False, "type": "workspaceWrite"}
        self.world.payloads["settings-show"]["payload"]["settings"]["sandbox"] = {
            "networkAccess": False, "type": "workspaceWrite"}
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]["value"],
            VERIFIED)
        self.assertEqual(
            cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]["value"], VERIFIED)


class TwentyFourthHostedRound(TrialCase):
    """A pid that is not a pid, and a false that is not a zero."""

    def test_a_non_integral_pid_is_refused(self):
        for value in (5775.5, "5775", True, 0, -1):
            world = World(self.base)
            self.addCleanup(world.stop)
            world.record["supervisor"]["pid"] = value
            world.flush()
            refused = world.refusal()
            self.assertIsNotNone(refused, repr(value) + " was accepted as a pid")
            self.assertIn("positive integer", refused.reason)

    def test_a_boolean_inside_a_setting_is_not_a_number(self):
        self.assertFalse(startup.same_value({"networkAccess": False}, {"networkAccess": 0}))
        self.assertFalse(startup.same_value({"a": [True]}, {"a": [1]}))
        self.assertTrue(startup.same_value({"networkAccess": False}, {"networkAccess": False}))
        self.assertTrue(startup.same_value({"a": [1, "b"]}, {"a": [1, "b"]}))
        self.assertFalse(startup.same_value({"a": 1}, {"a": 1, "b": 2}))


class TwentyFifthHostedRound(TrialCase):
    """One JSON number written two ways, a counter that is not a number, and the example itself."""

    def test_one_json_number_written_two_ways_agrees(self):
        self.assertTrue(startup.same_value({"n": 1}, {"n": 1.0}))
        self.assertTrue(startup.same_value({"n": [2]}, {"n": [2.0]}))
        self.assertFalse(startup.same_value({"n": False}, {"n": 0.0}))
        self.assertFalse(startup.same_value({"n": 1}, {"n": "1"}))

    def test_an_infinite_progress_counter_is_not_progress(self):
        pid = self.world.start_supervisor()
        self.world.supervisor.terminate()
        self.world.supervisor.wait(timeout=5)
        witness = self.world.trial / "supervisor.jsonl"

        def write(lines):
            witness.write_text("".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")

        write([{"pid": pid, "progress": 1}])
        document = self.world.preflight_with(
            lambda seconds: write([{"pid": pid, "progress": 1},
                                   {"pid": pid, "progress": float("inf")}]))
        self.assertEqual(cells_of(document, "processPersistence")["witnessAdvance"]["value"],
                         NOT_VERIFIED)

    def test_the_documented_record_is_one_the_validator_accepts(self):
        """The document's own example, substituted and run through load_start.

        Searching the document for forbidden substrings is text matching, which this repository
        does not accept as proof of behaviour: a malformed structure, a missing field or an
        abbreviated boundary would all have kept such a test green. This builds the record the
        document shows, replaces every placeholder with a real value, and hands it to the real
        validator.
        """
        text = (ROOT / "docs" / "live-trial.md").read_text(encoding="utf-8")
        start = text.index("\n    {\n      \"source\": \"live-trial-start\"")
        block = []
        for line in text[start:].splitlines():
            if line.strip() and not line.startswith("    "):
                break
            block.append(line[4:] if line.startswith("    ") else line)
            if line.strip() == "}":
                break
        documented = json.loads("\n".join(block))

        # The placeholders the document writes in angle brackets, each becoming the real thing.
        world = self.world
        documented["trialRoot"] = str(world.trial)
        documented["relay"] = dict(world.record["relay"])
        documented["store"] = dict(world.record["store"])
        documented["supervisor"] = dict(documented["supervisor"], **{
            "pid": os.getpid(), "witness": str(world.trial / "supervisor.jsonl"),
            "launchedAt": startup.stamp(time.time() - 120)})
        documented["assignment"] = dict(world.record["assignment"])
        # The documented boundaries keep their own shape; only the angle-bracket placeholders
        # become real. Replacing the whole value with the fixture is what let an abbreviated
        # second boundary stay green here while load_start would have refused it from an
        # operator who copied the record as written.
        expect = dict(world.record["boundaries"][0]["participants"][0]["expect"])
        issues = {"A": World.ISSUE_A, "B": World.ISSUE_B}
        tasks = {("A", "parent"): World.PARENT_A, ("A", "child"): World.CHILD_A,
                 ("B", "parent"): World.PARENT_B, ("B", "child"): World.CHILD_B}
        for boundary in documented["boundaries"]:
            name = boundary["name"]
            boundary["issueKey"] = issues[name]
            boundary["scopeRef"] = "scope-" + name
            boundary["repositoryRoot"] = str(world.repos[name])
            self.assertIsInstance(
                boundary["participants"], list,
                "boundary " + name + " does not document its participants as a list")
            for participant in boundary["participants"]:
                self.assertIsInstance(
                    participant, dict,
                    "boundary " + name + " abbreviates a participant, so an operator copying this"
                    " record as written would have it refused")
                participant["taskId"] = tasks[(name, participant["role"])]
                participant["cwd"] = str(world.repos[name])
                participant["expect"] = dict(expect)
        # The documented captures keep their own shape too, for the same reason: substituting the
        # whole object hid a map that named one receipt and one registration for four
        # participants and two boundaries, which an operator following it would have read as
        # unknown cells and a refused start.
        names = {"<parent A>": World.PARENT_A, "<child A>": World.CHILD_A,
                 "<parent B>": World.PARENT_B, "<child B>": World.CHILD_B}
        real = world.record["captures"]
        captures = {}
        for kind, entries in documented["captures"].items():
            self.assertIsInstance(entries, dict, kind + " is not documented as a map of captures")
            captures[kind] = {}
            for key in entries:
                name = names.get(key, key)
                self.assertIn(name, real[kind],
                              kind + " documents a capture for " + key + ", which is not one of"
                              " this record's participants or boundaries")
                captures[kind][name] = dict(real[kind][name])
            self.assertEqual(sorted(captures[kind]), sorted(real[kind]),
                             kind + " documents captures for " + str(sorted(entries))
                             + ", so an operator following it would have a reading with no"
                             " capture of its own")
        documented["captures"] = captures
        documented["window"] = dict(world.record["window"])
        self.assertEqual(documented["source"], "live-trial-start")
        self.assertEqual(documented["recordVersion"], 1)

        written = world.trial / "documented-start.json"
        written.write_text(json.dumps(documented), encoding="utf-8")
        record = startup.load_start(str(written), environment=world.environment())
        self.assertEqual(record["trialRoot"], str(world.trial))

        # And the fields the document shows are the ones the validator requires, so a record
        # missing any of them is refused rather than quietly accepted.
        for path in startup.REQUIRED_FIELDS:
            node = documented
            for key in path[:-1]:
                node = node[key]
            self.assertIn(path[-1], node, ".".join(path) + " is required and undocumented")


class TwentySixthHostedRound(TrialCase):
    """A counter too large for a float, and the workspace the store recorded."""

    def test_a_counter_too_large_for_a_float_does_not_crash_the_run(self):
        pid = self.world.start_supervisor()
        self.world.supervisor.terminate()
        self.world.supervisor.wait(timeout=5)
        witness = self.world.trial / "supervisor.jsonl"
        huge = 10 ** 400

        def write(lines):
            witness.write_text("".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")

        write([{"pid": pid, "progress": huge}])
        document = self.world.preflight_with(
            lambda seconds: write([{"pid": pid, "progress": huge},
                                   {"pid": pid, "progress": huge + 1}]))
        # An integer is finite whatever its size, so this is progress rather than a crash.
        self.assertEqual(cells_of(document, "processPersistence")["witnessAdvance"]["value"],
                         VERIFIED)

    def test_a_settings_record_naming_another_workspace_fails(self):
        self.world.payloads["taskCwd"][World.PARENT_A] = str(self.world.repos["B"])
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "capability")["recordedSettings:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("cwd", cell["evidence"])


class TwentySeventhHostedRound(TrialCase):
    """A capture that names the right participant and carries something that is not a status.

    This reading exists to catch a participant the host has no rollout for. str() turns JSON
    false and 0 into nonempty words, so a malformed capture resolved the participant and verified
    the cell, and the start went ahead on a reading that never answered its own question.
    """

    def status_cell(self, value):
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A, "status": value, "goal": None}
        self.world.flush()
        return cells_of(self.world.preflight(), "parentLifecycle")["lifecycle:" + World.PARENT_A]

    def test_a_value_that_is_not_a_status_never_resolves_a_participant(self):
        for value in (False, True, 0, None, [], ["idle"]):
            with self.subTest(value=value):
                cell = self.status_cell(value)
                self.assertEqual(cell["value"], UNKNOWN,
                                 json.dumps(value) + " was read as a resolved participant")
                self.assertFalse(cell["met"])
                self.assertIn("no state can be read from it", cell["evidence"])

    def test_the_start_is_refused_when_a_status_is_not_a_status(self):
        self.world.start_supervisor()
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A, "status": False, "goal": None}
        self.world.flush()
        self.assertFalse(self.world.preflight()["readyToStart"])

    def test_a_refusal_carried_beside_a_malformed_status_is_still_named(self):
        # Unreadable never overtakes a refusal the host actually named.
        self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
            "threadId": World.PARENT_A, "status": 0, "error": "thread not found"}
        self.world.flush()
        cell = cells_of(self.world.preflight(), "parentLifecycle")["lifecycle:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("thread not found", cell["evidence"])

    def test_a_word_and_a_structured_status_are_still_resolved(self):
        # Support: the shapes a real host answers with keep passing.
        for value in ("idle", {"type": "idle", "activeTurn": None}):
            with self.subTest(value=value):
                self.assertEqual(self.status_cell(value)["value"], VERIFIED)


class TwentyEighthHostedRound(TrialCase):
    """Two more cells verified by a reading that could not establish what the cell claimed.

    A status object was accepted for being a nonempty object rather than for carrying a state,
    and the ledger's placement was graded without doctor ever saying a ledger was configured.
    """

    def test_a_status_object_carrying_no_state_resolves_nobody(self):
        for value in ({"unexpected": True}, {"type": ""}, {"type": 7}, {"activeTurn": None}):
            with self.subTest(value=value):
                self.world.captures["lifecycle-" + World.PARENT_A + ".json"] = {
                    "threadId": World.PARENT_A, "status": value, "goal": None}
                self.world.flush()
                cell = cells_of(self.world.preflight(),
                                "parentLifecycle")["lifecycle:" + World.PARENT_A]
                self.assertEqual(cell["value"], UNKNOWN,
                                 json.dumps(value) + " was read as a resolved participant")
                self.assertFalse(cell["met"])

    def test_the_state_is_read_where_the_relay_reads_it(self):
        # Support, and the reason the key is "type": the relay's own adapter reads the thread
        # status there, so the checker is not inventing a shape of its own.
        source = relay_source("packages", "codex-session-relay", "src",
                              "codex_session_relay", "bridge_adapter.py")
        self.assertIn("type", constants_in(source, "read_thread"))
        self.assertIn("status", constants_in(source, "read_thread"))

    def test_a_ledger_placement_without_a_configured_ledger_is_unknown(self):
        self.world.payloads["doctor"]["payload"]["ledger"] = {"split": False}
        self.world.flush()
        cell = cells_of(self.world.preflight(), "storeIdentity")["ledgerSplit"]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertFalse(cell["met"])

    def test_a_configured_ledger_beside_the_store_still_verifies(self):
        # Support: the arrangement a trial wants is unaffected.
        self.world.payloads["doctor"]["payload"]["ledger"] = {"configured": True, "split": False}
        self.world.flush()
        self.assertEqual(
            cells_of(self.world.preflight(), "storeIdentity")["ledgerSplit"]["value"], VERIFIED)


class LinkedWorktreesAreOneRepository(TrialCase):
    """Two boundaries whose roots are linked worktrees of a single repository.

    Their roots differ and `git rev-parse --show-toplevel` reports those differing roots, so a
    declaration settled on roots read one repository as two and let the start proceed. Every
    checkout on the host these trials run on is a linked worktree of one repository, so that is
    the ordinary arrangement rather than an edge of it. Nothing else in this world disagrees,
    which is what makes readiness the reading's own verdict.
    """

    def setUp(self):
        self.world = World(self.base, one_repository=True)
        self.addCleanup(self.world.stop)

    def test_the_fixture_is_one_repository_under_two_roots(self):
        # Support: git's own answer about the world this case is built on, so the cases below
        # are about the reading rather than about whether the fixture was built as claimed.
        def ask(path, *arguments):
            return subprocess.run(["git", "-C", str(path), "rev-parse", *arguments],
                                  capture_output=True, text=True, check=True).stdout.strip()

        self.assertNotEqual(ask(self.world.repos["A"], "--show-toplevel"),
                            ask(self.world.repos["B"], "--show-toplevel"))
        self.assertEqual(
            (self.world.repos["A"] / ask(self.world.repos["A"], "--git-common-dir")).resolve(),
            (self.world.repos["B"] / ask(self.world.repos["B"], "--git-common-dir")).resolve())

    def test_two_worktrees_of_one_repository_refuse_the_start(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "two linked worktrees of one repository were read as two repositories")

    def test_the_reading_says_which_repository_each_boundary_is_in(self):
        self.world.start_supervisor()
        cells = cells_of(self.world.preflight(), "boundaries")
        for name in ("A", "B"):
            cell = cells.get("repositoryIdentity:" + name)
            self.assertIsNotNone(cell, "no reading settled which repository boundary " + name
                                 + " is in")
            self.assertEqual(cell["value"], NOT_VERIFIED)
            self.assertIn("linked worktrees of one repository", cell["evidence"])
        # The root each participant sits in still agrees with the root its boundary declared,
        # which is why a reading of the roots cannot be the one that refuses this.
        self.assertEqual(cells["toplevel:A:" + World.PARENT_A]["value"], VERIFIED)
        self.assertEqual(cells["toplevel:B:" + World.PARENT_B]["value"], VERIFIED)

    def test_two_separate_repositories_are_still_two_identities(self):
        separate = World(self.base)
        self.addCleanup(separate.stop)
        separate.start_supervisor()
        cells = cells_of(separate.preflight(), "boundaries")
        for name in ("A", "B"):
            self.assertEqual(cells["repositoryIdentity:" + name]["value"], VERIFIED)

    def test_a_root_git_cannot_read_is_unknown_rather_than_another_repository(self):
        self.world.record["boundaries"][1]["repositoryRoot"] = str(self.world.root / "nowhere")
        self.world.flush()
        cells = cells_of(self.world.preflight(), "boundaries")
        self.assertEqual(cells["repositoryIdentity:B"]["value"], UNKNOWN)
        self.assertFalse(cells["repositoryIdentity:B"]["met"])


class TwentyNinthHostedRound(TrialCase):
    """A gate graded from an answer taken before the delay, and absences graded as disagreements.

    The order gate is the last thing between this run and a dispatch, and it was comparing
    against the assignment read taken before the witness delay and every probe after it. A
    relationship archived while those seconds passed was approved from state already stale,
    which is the race this whole procedure exists to close.
    """

    def during_the_delay(self, change):
        """Run the preflight with the store changing underneath it, the way it really can."""
        self.world.start_supervisor()

        def sleeper(seconds):
            change()
            self.world.flush()
            time.sleep(min(seconds, 0.1))

        return self.world.preflight_with(sleeper)

    def test_a_relationship_archived_during_the_run_refuses_the_start(self):
        def archive():
            self.world.payloads["assignment-find"]["payload"]["assignments"][0][
                "relationshipStatus"] = "archived"

        document = self.during_the_delay(archive)
        # Readiness first: it is the defect itself, and it is answerable whether or not a reading
        # of its own exists to name it.
        self.assertFalse(document["readyToStart"],
                         "a relationship archived while the run was working was approved anyway")
        cell = cells_of(document, "assignmentState")["relationshipStillCurrent"]
        self.assertEqual(cell["value"], NOT_VERIFIED)

    def test_the_gate_compares_the_store_as_it_is_when_the_gate_runs(self):
        def reassign():
            self.world.payloads["assignment-find"]["payload"]["assignments"][0][
                "childTaskId"] = "another-child"

        document = self.during_the_delay(reassign)
        self.assertFalse(document["orderGate"]["passed"])
        self.assertTrue([c for c in document["orderGate"]["comparisons"]
                         if c["field"] == "childTaskId" and c["agrees"] is False],
                        "the gate compared against the answer taken before the delay")

    def test_the_store_is_asked_again_where_the_gate_is(self):
        self.world.start_supervisor()
        self.world.preflight()
        asked = [line for line in self.world.calls.read_text().splitlines()
                 if line.strip() and json.loads(line)["subcommand"] == "assignment-find"]
        # Once by the reading, once for the gate, and once by the confirmation a later round
        # added after it. What this case is about is that the reading's answer is not reused.
        self.assertEqual(len(asked), 3,
                         "the store was asked once and the gate reused that answer")

    def test_an_absent_field_is_unknown_rather_than_a_disagreement(self):
        """Each compound cell, answerable only where every field its predicate reads is there."""
        def drop_findings(world):
            world.captures["receipt-" + World.PARENT_A + ".json"]["settings"].pop("findings")

        def drop_ownership(world):
            world.record["supervisor"]["service"] = True
            world.payloads["service status"]["payload"].pop("ownership")

        def drop_status(world):
            world.payloads["assignment-find"]["payload"]["assignments"][0].pop(
                "relationshipStatus")

        def drop_source(world):
            world.payloads["criteria-show"]["payload"].pop("sourceRef")

        for change, reading, name in ((drop_findings, "capability",
                                       "receiptEcho:" + World.PARENT_A),
                                      (drop_ownership, "processPersistence", "service"),
                                      (drop_status, "assignmentState", "relationship"),
                                      (drop_source, "assignmentState", "criteria")):
            with self.subTest(cell=name):
                world = World(self.base)
                self.addCleanup(world.stop)
                change(world)
                world.start_supervisor()
                world.flush()
                cell = cells_of(world.preflight(), reading)[name]
                self.assertEqual(cell["value"], UNKNOWN,
                                 name + " read an absent field as a disagreement")
                self.assertFalse(cell["met"])


class ThirtiethHostedRound(TrialCase):
    """The access a delivery runs with, and two more absences graded as disagreements.

    A record declares a model, an effort, a sandbox and an approval policy. The relay's settings
    contract carries three more — cwd, runtimeWorkspaceRoots and environments — and a store
    record can be usable, complete and agree on all four declared ones while those three say the
    trial will run somewhere else.
    """

    def test_wider_roots_than_creation_recorded_refuse_the_start(self):
        self.world.start_supervisor()
        settings = self.world.payloads["settings-show"]["payload"]["settings"]
        settings["runtimeWorkspaceRoots"] = [str(self.world.root)]
        self.world.flush()
        document = self.world.preflight()
        # Readiness first: it is the defect itself, and it is answerable whether or not a reading
        # of its own exists to name it.
        self.assertFalse(document["readyToStart"],
                         "the trial would have started with access the receipt never recorded")
        cell = cells_of(document, "capability")["deliveryAccess:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("runtimeWorkspaceRoots", cell["evidence"])

    def test_another_environment_than_creation_recorded_refuses_the_start(self):
        self.world.start_supervisor()
        self.world.payloads["settings-show"]["payload"]["settings"]["environments"] = [
            {"environmentId": "somewhere-else", "cwd": str(self.world.root),
             "runtimeWorkspaceRoots": [str(self.world.root)]}]
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "another environment than creation recorded was approved")
        cell = cells_of(document, "capability")["deliveryAccess:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("environments", cell["evidence"])

    def test_access_neither_payload_carries_is_unknown(self):
        for drop in ("runtimeWorkspaceRoots", "environments"):
            with self.subTest(field=drop):
                world = World(self.base)
                self.addCleanup(world.stop)
                world.payloads["settings-show"]["payload"]["settings"].pop(drop)
                world.start_supervisor()
                world.flush()
                cell = cells_of(world.preflight(),
                                "capability")["deliveryAccess:" + World.PARENT_A]
                self.assertEqual(cell["value"], UNKNOWN)
                self.assertFalse(cell["met"])

    def test_the_access_compared_is_the_contract_the_relay_states(self):
        # Support, and the reason these three keys: the relay's settings contract names them
        # beside the four a record declares, so the checker is not choosing a set of its own.
        source = relay_source("packages", "codex-session-relay", "src",
                              "codex_session_relay", "settings.py")
        required = assigned_literal(source, "REQUIRED")
        for key in startup.DELIVERY_ACCESS:
            self.assertIn(key, required,
                          key + " is not one of the settings the relay requires")

    def test_more_absences_are_unknown_rather_than_disagreements(self):
        def drop_child(world):
            world.payloads["assignment-find"]["payload"]["assignments"][0].pop("childTaskId")

        def drop_device(world):
            world.captures["doctor-" + World.PARENT_A + ".json"]["store"].pop("device")

        for change, reading, name in ((drop_child, "assignmentState", "relationshipStillCurrent"),
                                      (drop_device, "storeIdentity", "peer:" + World.PARENT_A)):
            with self.subTest(cell=name):
                world = World(self.base)
                self.addCleanup(world.stop)
                change(world)
                world.start_supervisor()
                world.flush()
                cell = cells_of(world.preflight(), reading)[name]
                self.assertEqual(cell["value"], UNKNOWN,
                                 name + " read an absent field as a disagreement")
                self.assertFalse(cell["met"])


class ThirtyFirstHostedRound(TrialCase):
    """The receipt shape the bridge actually writes, rather than the one the fixture preferred.

    A cell added to catch a trial starting with access nobody recorded looked for all three
    delivery settings under settings.actual. The bridge's observable list has no environments in
    it, so every real receipt made the cell unreadable, and an unreadable cell refuses the start.
    A check meant to refuse one bad arrangement would have refused every good one.
    """

    def test_a_receipt_in_the_shape_the_bridge_writes_still_starts(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])
        self.assertEqual(
            cells_of(document, "capability")["deliveryAccess:" + World.PARENT_A]["value"],
            VERIFIED)

    def test_the_environment_selection_is_read_from_the_created_thread(self):
        self.world.start_supervisor()
        self.world.captures["receipt-" + World.PARENT_A + ".json"]["creation"]["thread"][
            "environments"] = [{"environmentId": "somewhere-else", "cwd": str(self.world.root),
                                "runtimeWorkspaceRoots": [str(self.world.root)]}]
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "capability")["deliveryAccess:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("environments", cell["evidence"])

    def test_an_environment_selection_nobody_reported_is_unknown(self):
        # A null means unknown in the relay's own normalisation, and unknown is never flattened
        # into empty here either. An empty list is a selection; a null is the absence of one.
        for where in ("receipt", "store"):
            with self.subTest(where=where):
                world = World(self.base)
                self.addCleanup(world.stop)
                if where == "receipt":
                    world.captures["receipt-" + World.PARENT_A + ".json"]["creation"]["thread"][
                        "environments"] = None
                else:
                    world.payloads["settings-show"]["payload"]["settings"]["environments"] = None
                world.start_supervisor()
                world.flush()
                cell = cells_of(world.preflight(),
                                "capability")["deliveryAccess:" + World.PARENT_A]
                self.assertEqual(cell["value"], UNKNOWN)
                self.assertFalse(cell["met"])

    def test_the_bridge_does_not_observe_the_environment_selection(self):
        # Support, and the reason environments is read from the created thread rather than from
        # settings.actual: the bridge's own observable list is what builds that object.
        source = relay_source("packages", "codex-thread-bridge", "src",
                              "codex_thread_bridge", "settings.py")
        observable = assigned_literal(source, "OBSERVABLE")
        self.assertIn("runtimeWorkspaceRoots", observable)
        self.assertNotIn("environments", observable)


class ThirtySecondHostedRound(TrialCase):
    """Two more readings graded from an answer taken before the delay, and two records that
    could never have agreed with the store they describe."""

    def replaced_after(self, subcommand, calls, payload):
        """Answer differently once this subcommand has been asked enough times.

        The replacement lands after the reading that already graded it, which is the window the
        gate has to close: a row or a criteria set another process replaces while the rest of the
        readings run is the one delivery will actually use.
        """
        self.world.payloads["after"] = {"subcommand": subcommand, "calls": calls,
                                        "payloads": {subcommand: {"payload": payload}}}
        self.world.start_supervisor()
        return self.world.preflight()

    def test_settings_replaced_after_the_reading_refuse_the_start(self):
        widened = json.loads(json.dumps(self.world.payloads["settings-show"]["payload"]))
        widened["settings"]["runtimeWorkspaceRoots"] = [str(self.world.root)]
        # Four participants, so the fifth ask is the first one the gate makes.
        document = self.replaced_after("settings-show", 4, widened)
        self.assertFalse(document["readyToStart"],
                         "a settings row replaced while the run was working was approved anyway")
        cell = cells_of(document, "capability")["settingsStillCurrent:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)

    def test_criteria_replaced_after_the_reading_refuse_the_start(self):
        replaced = json.loads(json.dumps(self.world.payloads["criteria-show"]["payload"]))
        replaced.update(setDigest="digest-02", sourceRef="source-02", criteria=["c1", "c2"])
        # Asked once by the reading, so the second ask is the gate's own.
        document = self.replaced_after("criteria-show", 1, replaced)
        self.assertFalse(document["readyToStart"],
                         "a criteria set replaced while the run was working was approved anyway")
        cell = cells_of(document, "assignmentState")["criteriaStillCurrent"]
        self.assertEqual(cell["value"], NOT_VERIFIED)

    def test_a_sandbox_that_is_a_mode_on_its_own_is_refused(self):
        # The relay records a policy object and reads the mode out of it, so a record declaring a
        # bare mode could never agree with the row the store holds.
        self.world.record["boundaries"][0]["participants"][0]["expect"]["sandbox"] = (
            "dangerFullAccess")
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a bare sandbox mode was accepted")
        self.assertIn("policy object", refused.reason)

    def test_a_minimum_uptime_of_zero_is_refused(self):
        self.world.record["supervisor"]["minimumAliveSeconds"] = 0
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a bound met by a process that started this instant was"
                             " accepted")
        self.assertIn("greater than zero", refused.reason)

    def test_a_positive_minimum_uptime_is_still_accepted(self):
        # Support: the bound the fixture declares keeps working.
        self.assertIsNone(self.world.refusal())


class ThirtyThirdHostedRound(TrialCase):
    """The command a host actually reaches, the order of the final reads, and a time with no
    offset."""

    def test_the_launcher_is_the_owned_pointer_the_record_names(self):
        # The fixture writes the shape runtime_install.py writes: an install's location is where
        # the module went, and the owned pointer is recorded separately. Deriving the command
        # from the location named a path no real install has.
        self.assertIsNone(self.world.refusal(),
                          "a host record in the shape a real install writes was refused")
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertEqual(document["relay"]["launcher"], str(self.world.launcher))
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def test_a_record_with_no_owned_pointer_is_refused(self):
        directory = self.world.state / "codex-relay-workflow"
        record = json.loads((directory / "host-record.json").read_text(encoding="utf-8"))
        del record["pointer"]
        (directory / "host-record.json").write_text(json.dumps(record), encoding="utf-8")
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a record naming no owned pointer was accepted")
        self.assertIn("owned pointer", refused.reason)

    def test_the_pointer_is_where_the_runtime_lane_puts_it(self):
        # Support, and the reason the command is <pointer>/bin/<console>: the runtime lane owns
        # that layout, and this reads it rather than choosing one.
        source = relay_source("scripts", "crw_runtime", "pointer.py")
        self.assertEqual(assigned_literal(source, "POINTER_NAME"), "current")
        # And the pointer path is that name under the destination, read as the function's own
        # expression rather than as a line of text.
        tree = ast.parse(source)
        node = next(n for n in ast.walk(tree)
                    if isinstance(n, ast.FunctionDef) and n.name == "pointer_path")
        names = {n.id for n in ast.walk(node) if isinstance(n, ast.Name)}
        self.assertIn("POINTER_NAME", names)
        self.assertIn("destination", names)


class ThirtyFourthHostedRound(TrialCase):
    """The order the final reads are taken in, and a time that never said which offset it is in."""

    def asked(self):
        return [json.loads(line)["subcommand"]
                for line in self.world.calls.read_text().splitlines() if line.strip()]

    def test_the_assignment_is_the_last_read_the_gate_is_graded_from(self):
        self.world.start_supervisor()
        self.world.preflight()
        asked = self.asked()
        # Once by the reading, once for the gate, once by the confirmation that follows it.
        self.assertEqual(asked.count("assignment-find"), 3)
        graded_at = [i for i, name in enumerate(asked) if name == "assignment-find"][1]
        before, after = asked[:graded_at], asked[graded_at + 1:]
        # Four participants read twice, and the criteria read twice, all before the answer the
        # gate compares against rather than after it.
        self.assertEqual(before.count("settings-show"), 8,
                         "the gate's settings pass did not precede the answer it compares")
        self.assertEqual(before.count("criteria-show"), 2)
        # And everything after it is the confirmation that no read moved while it ran, the
        # relationship included: leaving that one out left the value the gate compares unguarded.
        # The store's own identity is asked last of all, after every probe that opened it, because
        # a database replaced while the readings ran is the one the confirmation pass itself used.
        self.assertEqual(sorted(set(after)),
                         ["assignment-find", "criteria-show", "doctor", "settings-show"])

    def test_a_relationship_archived_during_the_confirmation_is_caught(self):
        # Two assignment asks happen before the confirmation's own, so the third is its.
        archived = json.loads(json.dumps(self.world.payloads["assignment-find"]["payload"]))
        archived["assignments"][0]["relationshipStatus"] = "archived"
        self.world.payloads["after"] = {"subcommand": "assignment-find", "calls": 2,
                                        "payloads": {"assignment-find": {"payload": archived}}}
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "assignmentState")["gateReadsHeld"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("the responsible relationship", cell["evidence"])
        self.assertFalse(document["readyToStart"])

    def test_a_refusal_built_from_an_absent_field_still_prints_as_a_refusal(self):
        # MISSING is an object, so a refusal carrying it raised inside the handler that serialises
        # it: the command printed a traceback and lost both the refusal and its exit status.
        del self.world.record["captures"]["parentLifecycle"][World.PARENT_A]["path"]
        self.world.flush()
        code, payload, stderr = self.world.run_cli()
        self.assertEqual(code, 2, stderr)
        self.assertIsNotNone(payload, stderr)
        self.assertIn("refused", payload)
        self.assertNotIn("Traceback", stderr)

    def test_a_read_that_moves_while_the_last_one_runs_is_caught(self):
        # Eight settings-show asks happen before the assignment read, so the ninth is the
        # confirmation's own: this is a row replaced while that last read was running.
        widened = json.loads(json.dumps(self.world.payloads["settings-show"]["payload"]))
        widened["settings"]["runtimeWorkspaceRoots"] = [str(self.world.root)]
        self.world.payloads["after"] = {"subcommand": "settings-show", "calls": 8,
                                        "payloads": {"settings-show": {"payload": widened}}}
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        cell = cells_of(document, "assignmentState")["gateReadsHeld"]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("changed between the gate's own reads", cell["evidence"])
        self.assertFalse(document["readyToStart"])

    def test_a_time_without_an_offset_is_refused(self):
        # Read directly, because the consequence is a ledger line placed in the wrong stretch:
        # 03:30 written in UTC+02:00 and read as 03:30Z lands outside a 01:00Z to 02:00Z window,
        # so an intervention made during the window is counted as preparation and the window
        # passes with one in it.
        for value in ("2026-09-19T01:30:00", "2026-09-19", "2026-09-19 03:30:00"):
            with self.subTest(value=value):
                with self.assertRaises(startup.Refused) as caught:
                    startup.moment(value, "a ledger line")
                self.assertIn("UTC offset", caught.exception.reason)

    def test_an_offset_that_is_not_utc_is_still_placed(self):
        # Support: naming an offset is what is required, not naming Z. The same instant written
        # in another offset is the same instant.
        here = startup.moment("2026-09-19T03:30:00+02:00", "a ledger line")
        there = startup.moment("2026-09-19T01:30:00Z", "a ledger line")
        self.assertEqual(here, there)

    def test_a_naive_time_is_not_quietly_optional_either(self):
        # maybe_moment answers None where a time is optional, and None says it could not be read
        # rather than standing in for one.
        self.assertIsNone(startup.maybe_moment("2026-09-19T01:30:00"))
        self.assertIsNotNone(startup.maybe_moment("2026-09-19T01:30:00Z"))


class ThirtyFifthHostedRound(TrialCase):
    """A policy recorded with its defaults filled in, against a record that named a type.

    Requiring the policy object closed one hole and opened another: the host records the policy
    normalised, so a record naming {"type": "workspaceWrite"} compared unequal to the very
    payload that policy produces, and every valid workspace-write trial was refused.
    """

    def declare(self, policy, recorded):
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["sandbox"] = policy
        self.world.payloads["settings-show"]["payload"]["settings"]["sandbox"] = recorded
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            for half in ("requested", "actual"):
                capture["settings"][half]["sandbox"] = recorded
        self.world.start_supervisor()
        self.world.flush()
        return self.world.preflight()

    def test_a_declared_type_agrees_with_the_policy_the_host_recorded(self):
        normalised = {"type": "workspaceWrite", "networkAccess": False, "writableRoots": [],
                      "excludeTmpdirEnvVar": False, "excludeSlashTmp": False}
        document = self.declare({"type": "workspaceWrite"}, normalised)
        cells = cells_of(document, "capability")
        self.assertEqual(cells["receiptEcho:" + World.PARENT_A]["value"], VERIFIED)
        self.assertEqual(cells["recordedSettings:" + World.PARENT_A]["value"], VERIFIED)
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def test_a_declared_key_that_disagrees_still_fails(self):
        normalised = {"type": "workspaceWrite", "networkAccess": False, "writableRoots": []}
        document = self.declare({"type": "workspaceWrite", "networkAccess": True}, normalised)
        cell = cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("sandbox", cell["evidence"])

    def test_a_declared_key_the_payload_leaves_at_its_default_still_fails(self):
        # The payload names only a type, so its networkAccess is the type's default, false. A
        # record declaring true disagrees with it rather than going unanswered: both sides are
        # normalised, which is what the relay does before it compares them.
        document = self.declare({"type": "workspaceWrite", "networkAccess": True},
                                {"type": "workspaceWrite"})
        self.assertEqual(
            cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_a_policy_and_its_own_defaults_written_out_are_one_policy(self):
        # Support: normalising both sides is what makes an omitted default and an explicit one
        # the same policy, which is the relay's own rule.
        document = self.declare({"type": "workspaceWrite", "networkAccess": False},
                                {"type": "workspaceWrite"})
        self.assertEqual(
            cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]["value"], VERIFIED)

    def test_a_type_only_declaration_does_not_accept_a_widened_default(self):
        # The keys a type does not name still have defined values. Reading them as unconstrained
        # accepted a trial started with network access the record never declared.
        normalised = {"type": "workspaceWrite", "networkAccess": True, "writableRoots": [],
                      "excludeTmpdirEnvVar": False, "excludeSlashTmp": False}
        document = self.declare({"type": "workspaceWrite"}, normalised)
        cells = cells_of(document, "capability")
        self.assertEqual(cells["receiptEcho:" + World.PARENT_A]["value"], NOT_VERIFIED)
        self.assertEqual(cells["recordedSettings:" + World.PARENT_A]["value"], NOT_VERIFIED)
        self.assertFalse(document["readyToStart"],
                         "a trial started with access the record never declared")

    def test_a_key_outside_the_policy_contract_is_named_rather_than_compared(self):
        # Nothing is silently accepted: a key neither the record nor the policy's own defaults
        # name is reported, so an operator who cares about it can declare it.
        normalised = {"type": "workspaceWrite", "networkAccess": False, "writableRoots": [],
                      "excludeTmpdirEnvVar": False, "excludeSlashTmp": False,
                      "somethingThisCheckerDoesNotKnow": "x"}
        document = self.declare({"type": "workspaceWrite"}, normalised)
        cell = cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]
        self.assertEqual(cell["value"], VERIFIED)
        self.assertIn("The host also recorded", cell["evidence"])
        self.assertIn("sandbox.somethingThisCheckerDoesNotKnow", cell["evidence"])

    def test_the_policy_defaults_are_the_relays_own(self):
        # Support, and the guard on the one place this checker copies another lane's contract:
        # a copy that drifts would quietly widen what a record is read to have declared.
        source = (ROOT / "packages" / "codex-session-relay" / "src" / "codex_session_relay"
                  / "settings.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        theirs = next(ast.literal_eval(node.value) for node in ast.walk(tree)
                      if isinstance(node, ast.Assign)
                      and any(getattr(t, "id", None) == "POLICY_DEFAULTS" for t in node.targets))
        self.assertEqual(startup.POLICY_DEFAULTS, theirs,
                         "the checker's copy of the policy defaults is not the relay's")


class ThirtySixthHostedRound(TrialCase):
    """The permission profile a resume is checked against, which nothing here was reading.

    The relay compares the whole object a creation response reported against the settings row's
    expectation and withholds the send as an unverifiable permission profile when they differ.
    A preflight that never read it published readiness for a trial whose first send cannot land.
    """

    def with_profile(self, reported, expected):
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            if reported is None:
                capture["creation"].pop("activePermissionProfile", None)
            else:
                capture["creation"]["activePermissionProfile"] = reported
        settings = self.world.payloads["settings-show"]["payload"]["settings"]
        if expected is None:
            settings.pop("expectedPermissionProfile", None)
        else:
            settings["expectedPermissionProfile"] = expected
        self.world.start_supervisor()
        self.world.flush()
        return self.world.preflight()

    def test_a_profile_the_store_did_not_anticipate_refuses_the_start(self):
        document = self.with_profile({"id": "profile-good", "extends": None},
                                     {"id": "profile-bad", "extends": None})
        # Readiness first: it is the defect itself, and it is answerable whether or not a reading
        # of its own exists to name it.
        self.assertFalse(document["readyToStart"],
                         "a trial whose first send cannot land was reported ready")
        cell = cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("unverifiable permission profile", cell["evidence"])

    def test_an_id_shaped_stand_in_is_not_the_profile(self):
        # The later check is object equality, so a record holding only the id can never match.
        document = self.with_profile({"id": "profile-1", "name": "a profile", "extends": None},
                                     {"id": "profile-1"})
        self.assertEqual(
            cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_no_profile_reported_is_not_applicable_rather_than_a_failure(self):
        document = self.with_profile(None, None)
        cell = cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_APPLICABLE)
        self.assertTrue(cell["met"])
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def test_a_profile_the_store_carries_whole_verifies(self):
        # Support: the arrangement a trial wants.
        whole = {"id": "profile-1", "name": "a profile", "extends": None, "rules": []}
        document = self.with_profile(whole, dict(whole))
        self.assertEqual(
            cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]["value"],
            VERIFIED)

    def test_the_relay_is_what_checks_it(self):
        # Support, and the reason this reading exists: the comparison it mirrors.
        source = relay_source("packages", "codex-session-relay", "src",
                              "codex_session_relay", "settings.py")
        named = constants_in(source, "mismatches")
        self.assertIn("activePermissionProfile", named)
        self.assertIn("expectedPermissionProfile", named)

    def test_the_comparison_is_the_relays_own_rather_than_a_stricter_one(self):
        # The relay compares with Python equality, where a nested false and a nested zero are the
        # same value. Comparing more strictly than the check this cell predicts refuses a send
        # the relay would have allowed.
        document = self.with_profile({"id": "profile-1", "rule": False},
                                     {"id": "profile-1", "rule": 0})
        self.assertEqual(
            cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]["value"],
            VERIFIED)
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def test_a_profile_that_really_differs_still_refuses(self):
        # Support: the looser comparison is the relay's, not an absence of one.
        document = self.with_profile({"id": "profile-1", "rule": False},
                                     {"id": "profile-1", "rule": True})
        self.assertEqual(
            cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]["value"],
            NOT_VERIFIED)


class ThirtySeventhHostedRound(TrialCase):
    """Three settings a trial can agree on perfectly and still not deliver under.

    Every reading here exists so the round trip can happen. A record the relay will refuse at the
    resume is a trial that cannot send its own correction, however well its participants agree,
    and reporting it ready is a false pass of the same kind.
    """

    def declared(self, **changes):
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"].update(changes)
        self.world.flush()
        return self.world.refusal()

    def test_a_sandbox_the_relay_cannot_resume_is_refused(self):
        refused = self.declared(sandbox={"type": "externalSandbox", "networkAccess": "restricted"})
        self.assertIsNotNone(refused, "a sandbox with no resume mode was accepted")
        self.assertIn("no resume mode", refused.reason)

    def test_an_approval_policy_delivery_does_not_authorise_is_refused(self):
        refused = self.declared(approvalPolicy="on-request")
        self.assertIsNotNone(refused, "an unauthorised approval policy was accepted")
        self.assertIn("authorises one approval policy", refused.reason)

    def test_the_resumable_types_and_the_policy_are_the_relays_own(self):
        # Support, and the guard on two more copies of another lane's contract.
        source = (ROOT / "packages" / "codex-session-relay" / "src" / "codex_session_relay"
                  / "settings.py").read_text(encoding="utf-8")
        tree = ast.parse(source)

        def assigned(name):
            return next(ast.literal_eval(node.value) for node in ast.walk(tree)
                        if isinstance(node, ast.Assign)
                        and [t for t in node.targets if getattr(t, "id", None) == name])

        self.assertEqual(sorted(startup.RESUME_SANDBOX_TYPES),
                         sorted(assigned("RESUME_SANDBOX_MODE")),
                         "the checker's resumable sandbox types are not the relay's")
        self.assertEqual(startup.AUTHORIZED_APPROVAL_POLICY,
                         assigned("AUTHORIZED_APPROVAL_POLICY"))

    def test_the_resumable_types_a_trial_may_use_are_still_accepted(self):
        # Support: the three the relay can restore keep working.
        for kind in startup.RESUME_SANDBOX_TYPES:
            with self.subTest(sandbox=kind):
                world = World(self.base)
                self.addCleanup(world.stop)
                for boundary in world.record["boundaries"]:
                    for participant in boundary["participants"]:
                        participant["expect"]["sandbox"] = {"type": kind}
                world.flush()
                self.assertIsNone(world.refusal())

    def test_process_age_comes_from_the_kernels_own_start_time(self):
        # /proc/<pid>'s ctime is the process's start on some hosts and the first lookup on
        # others, which read a running supervisor as zero seconds old on a CI runner here. The
        # kernel's start-time field is defined, so this reads it and agrees about a process whose
        # /proc entry nothing has looked at until now.
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        self.addCleanup(child.wait)
        self.addCleanup(child.terminate)
        time.sleep(1.2)
        age = time.time() - startup.process_started_at(child.pid)
        self.assertGreaterEqual(age, 1.0,
                                "a process that had been running read as younger than it is")
        self.assertLess(age, 30)

    def test_an_unreadable_process_is_unknown_rather_than_a_time(self):
        self.assertIsNone(startup.process_started_at(0))
        self.assertIsNone(startup.process_started_at("not a pid"))


class ThirtyEighthHostedRound(TrialCase):
    """Write access per participant, and a boot epoch carried to the precision it is added to."""

    def test_a_peer_that_cannot_write_the_state_directory_refuses_the_start(self):
        # OPS-3.5: every relay command opens the store on construction, so a participant without
        # write access cannot run even a read-only-looking one. Proving it holds the same store
        # says nothing about whether it can use it.
        capture = self.world.captures["doctor-" + World.CHILD_A + ".json"]
        capture["actorReachability"]["stateDirectoryWritable"] = False
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "a participant that cannot run a relay command was dispatched to")
        cell = cells_of(document, "storeIdentity")["peer:" + World.CHILD_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("writable", cell["evidence"])

    def test_the_acting_process_needs_write_access_too(self):
        self.world.payloads["doctor"]["payload"]["actorReachability"][
            "stateDirectoryWritable"] = False
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"])
        self.assertEqual(cells_of(document, "storeIdentity")["stateWritable"]["value"],
                         NOT_VERIFIED)

    def test_a_peer_that_does_not_say_is_unknown_rather_than_writable(self):
        capture = self.world.captures["doctor-" + World.CHILD_A + ".json"]
        capture["actorReachability"].pop("stateDirectoryWritable")
        self.world.flush()
        cell = cells_of(self.world.preflight(), "storeIdentity")["peer:" + World.CHILD_A]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertFalse(cell["met"])

    def test_a_process_is_not_older_than_it_is_by_the_boot_seconds_fraction(self):
        # /proc/stat's btime is a whole second, so adding precise ticks to it moves the dropped
        # fraction into the age. A sub-second minimum could then be met early. The reconstructed
        # start must not sit before the moment the child was actually created.
        before = time.time()
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        self.addCleanup(child.wait)
        self.addCleanup(child.terminate)
        time.sleep(0.4)
        started = startup.process_started_at(child.pid)
        self.assertIsNotNone(started)
        # A tight bound on purpose: the fraction btime drops is whatever this host booted at,
        # 0.04s here and up to a second elsewhere, so a loose margin would pass on the hosts
        # where the error happens to be small and prove nothing.
        self.assertGreaterEqual(started, before - 0.01,
                                "the process was reconstructed as starting before it existed")
        self.assertLessEqual(started, time.time())


class ThirtyNinthHostedRound(TrialCase):
    """A policy the protocol cannot carry, and the database behind the writable directory."""

    def test_a_policy_no_resume_can_carry_is_refused(self):
        # readOnly has a resume mode and no config key for its network access, so asking for a
        # non-default one is a request the bridge refuses before any call is made. A type-only
        # allowlist accepted it and the trial could not have delivered under it.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["sandbox"] = {"type": "readOnly", "networkAccess": True}
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a policy no resume can carry was accepted")
        self.assertIn("no resume can carry", refused.reason)
        self.assertEqual(refused.detail["fields"], ["networkAccess"])

    def test_the_same_policy_at_its_own_default_is_still_accepted(self):
        # Support: a field equal to its type's default asks for nothing, so it is transmittable.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["sandbox"] = {"type": "readOnly", "networkAccess": False}
        self.world.flush()
        self.assertIsNone(self.world.refusal())

    def test_the_transmittable_fields_are_the_relays_own(self):
        # Support, and the guard on another copied contract, read as a value rather than matched.
        source = relay_source("packages", "codex-session-relay", "src", "codex_session_relay",
                              "settings.py")
        theirs = assigned_literal(source, "POLICY_CONFIG_KEYS")
        self.assertEqual({kind: sorted(fields)
                          for kind, fields in startup.POLICY_CONFIG_FIELDS.items()},
                         {kind: sorted(fields) for kind, fields in theirs.items()})

    def test_a_database_the_participant_cannot_write_refuses_the_start(self):
        # The directory probe writes a temporary file; the store opens the database read-write on
        # every construction. A writable directory holding a database this process cannot open
        # is an environment where the same-store proof succeeds and the next command fails.
        capture = self.world.captures["doctor-" + World.CHILD_A + ".json"]
        capture["store"]["observedAccess"]["write"] = False
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "a participant whose next relay command cannot open the store passed")
        cell = cells_of(document, "storeIdentity")["peer:" + World.CHILD_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("database openable for writing", cell["evidence"])

    def test_the_acting_processs_database_is_read_too(self):
        self.world.payloads["doctor"]["payload"]["store"]["observedAccess"]["write"] = False
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"])
        self.assertEqual(cells_of(document, "storeIdentity")["stateWritable"]["value"],
                         NOT_VERIFIED)

    def test_a_doctor_that_does_not_say_is_unknown_rather_than_writable(self):
        self.world.payloads["doctor"]["payload"]["store"]["observedAccess"].pop("write")
        self.world.flush()
        cell = cells_of(self.world.preflight(), "storeIdentity")["stateWritable"]
        self.assertEqual(cell["value"], UNKNOWN)
        self.assertFalse(cell["met"])


class FortiethHostedRound(TrialCase):
    """A row two payloads agree on that delivery cannot use, and one spelling of a selection.

    Equality says two payloads agree. It does not say the thing they agree on survives the
    transformations delivery performs on it before it sends, and three separate shapes reached
    readiness that way. The row is run through those transformations now.
    """

    def rowed(self, **changes):
        settings = self.world.payloads["settings-show"]["payload"]["settings"]
        settings.update(changes)
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            for half in ("requested", "actual"):
                capture["settings"][half].update(
                    {k: v for k, v in changes.items() if k != "environments"})
        self.world.start_supervisor()
        self.world.flush()
        return self.world.preflight()

    def test_roots_that_resume_parameters_cannot_be_built_from_refuse_the_start(self):
        document = self.rowed(runtimeWorkspaceRoots=7)
        self.assertFalse(document["readyToStart"],
                         "a row delivery raises on before thread/resume was approved")
        cell = cells_of(document, "capability")["deliverableSettings:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("runtimeWorkspaceRoots is not a list", cell["evidence"])

    def test_a_policy_the_relay_cannot_normalise_refuses_the_start(self):
        document = self.rowed(sandbox={"type": "workspaceWrite", "writableRoots": "not-a-list"})
        self.assertFalse(document["readyToStart"],
                         "a row whose policy cannot be normalised was approved")
        # The record naming the same policy is refused at ingress, which is the earlier and
        # better place; this is the row reaching the same verdict when only the store holds it.
        cell = cells_of(document, "capability").get("deliverableSettings:" + World.PARENT_A)
        self.assertIsNotNone(cell, "no reading ran the row through what delivery does to it")
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("cannot be read in full", cell["evidence"])

    def test_the_same_malformed_policy_is_refused_in_the_record_too(self):
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["sandbox"] = {"type": "workspaceWrite",
                                                    "writableRoots": "not-a-list"}
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a policy the relay cannot read was accepted")
        self.assertIn("cannot be read in full", refused.reason)

    def test_an_omitted_roots_list_is_the_same_selection_as_an_explicit_one(self):
        # The protocol says an omitted runtimeWorkspaceRoots is that entry's own cwd, so these
        # two spellings are one selection and refusing the trial was stricter than delivery.
        here = str(self.world.repos["A"])
        self.world.payloads["settings-show"]["payload"]["settings"]["environments"] = [
            {"environmentId": "local", "cwd": here, "runtimeWorkspaceRoots": [here]}]
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            capture["creation"]["thread"]["environments"] = [
                {"environmentId": "local", "cwd": here}]
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertEqual(
            cells_of(document, "capability")["deliveryAccess:" + World.PARENT_A]["value"],
            VERIFIED)
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def test_a_selection_that_really_differs_still_refuses(self):
        # Support: the defaulting rule is the relay's, not an absence of comparison.
        here = str(self.world.repos["A"])
        self.world.payloads["settings-show"]["payload"]["settings"]["environments"] = [
            {"environmentId": "local", "cwd": here, "runtimeWorkspaceRoots": [str(self.world.root)]}]
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            capture["creation"]["thread"]["environments"] = [
                {"environmentId": "local", "cwd": here}]
        self.world.start_supervisor()
        self.world.flush()
        self.assertEqual(
            cells_of(self.world.preflight(),
                     "capability")["deliveryAccess:" + World.PARENT_A]["value"],
            NOT_VERIFIED)

    def test_the_defaulting_rule_is_the_relays_own(self):
        # Support, read as the function's own expression rather than matched as text.
        source = relay_source("packages", "codex-session-relay", "src", "codex_session_relay",
                              "settings.py")
        named = constants_in(source, "normalise_environments")
        self.assertIn("runtimeWorkspaceRoots", named)
        self.assertIn("cwd", named)


class FortyFirstHostedRound(TrialCase):
    """The window the dispatch opened, a caller's own boolean, and roots that are not a list."""

    def window(self, *, opened, closed, extra=()):
        lines = [{"at": startup.stamp(opened), "kind": "window_open", "segment": "s"},
                 {"at": startup.stamp(closed), "kind": "window_close", "segment": "s"}]
        return list(extra) + lines

    def graded(self, lines, **window):
        self.world.record["window"] = dict(self.world.record["window"], **window)
        self.world.ledger_lines(lines)
        self.world.flush()
        try:
            return self.world.run_ledger(), None
        except startup.Refused as refused:
            return None, refused

    def test_a_window_that_does_not_open_at_the_dispatch_is_refused(self):
        # A record naming a time five minutes out and a dispatch that went at once left
        # everything between them outside the measured interval, so an intervention the trial
        # actually needed was counted as preparation and the window still read clean.
        now = time.time()
        opened, closed = now - 600, now - 60
        lines = self.window(opened=opened, closed=closed,
                            extra=[{"at": startup.stamp(opened - 300), "kind": "dispatch"}])
        report, refused = self.graded(lines, opensAt=startup.stamp(opened),
                                      closesAt=startup.stamp(closed))
        self.assertIsNotNone(refused, "a window that opened after its own dispatch was graded")
        self.assertIn("does not open at the dispatch", refused.reason)

    def test_a_ledger_with_no_dispatch_is_refused(self):
        now = time.time()
        opened, closed = now - 600, now - 60
        lines = [{"at": startup.stamp(opened), "kind": "window_open", "segment": "s"},
                 {"at": startup.stamp(closed), "kind": "window_close", "segment": "s"}]
        self.world.record["window"] = dict(self.world.record["window"],
                                           opensAt=startup.stamp(opened),
                                           closesAt=startup.stamp(closed))
        (self.world.trial / "ledger.jsonl").write_text(
            "".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")
        self.world.flush()
        with self.assertRaises(startup.Refused) as caught:
            self.world.run_ledger()
        self.assertIn("one dispatch", caught.exception.reason)

    def test_the_window_the_dispatch_opened_is_reported(self):
        # Support: the ordinary arrangement, and the dispatch the window opened at is in the
        # document rather than only checked and dropped.
        now = time.time()
        opened, closed = now - 600, now - 60
        report, refused = self.graded(self.window(opened=opened, closed=closed),
                                      opensAt=startup.stamp(opened),
                                      closesAt=startup.stamp(closed))
        self.assertIsNone(refused)
        self.assertEqual(report["window"]["dispatchedAt"], startup.stamp(opened))
        self.assertTrue(report["window"]["passed"])

    def test_a_callers_own_verdict_does_not_enter_the_judgment_walk(self):
        # Corroboration is the caller's evidence, not this checker's verdict. Copied whole, a
        # boolean named passed inside it was counted as a judgment the checker had reached.
        now = time.time()
        opened, closed = now - 600, now - 60
        report, refused = self.graded(
            self.window(opened=opened, closed=closed),
            opensAt=startup.stamp(opened), closesAt=startup.stamp(closed),
            corroboration={"opensAt": startup.stamp(opened), "passed": False, "met": False})
        self.assertIsNone(refused)
        document = dict(report)
        counted = startup.judgments(document)
        failed = [j["at"] for j in counted if not j["value"]]
        self.assertEqual(failed, [], "a caller's own boolean was counted as a verdict")
        self.assertEqual(report["window"]["corroboration"],
                         {"opensAt": startup.stamp(opened)})

    def test_environment_roots_that_are_not_a_list_are_a_cell_rather_than_a_crash(self):
        here = str(self.world.repos["A"])
        self.world.payloads["settings-show"]["payload"]["settings"]["environments"] = [
            {"environmentId": "local", "cwd": here, "runtimeWorkspaceRoots": 7}]
        self.world.start_supervisor()
        self.world.flush()
        try:
            document = self.world.preflight()
        except TypeError as error:
            self.fail("a row delivery cannot consume made the run raise instead of reporting: "
                      + repr(error))
        self.assertFalse(document["readyToStart"])
        cell = cells_of(document, "capability")["deliverableSettings:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("environments is not a list of selections", cell["evidence"])


class FortySecondHostedRound(TrialCase):
    """A policy whose outer shape is right and whose contents the two sides refuse."""

    def declare(self, policy):
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["sandbox"] = policy
        self.world.flush()
        return self.world.refusal()

    def test_a_writable_root_that_is_not_a_path_is_refused(self):
        # A list of the wrong things is still a list. The bridge refuses the policy before the
        # creation call, so a record carrying it could never have produced the receipt it
        # declares, and certifying it approved a round trip that cannot start.
        refused = self.declare({"type": "workspaceWrite", "writableRoots": [7],
                                "networkAccess": False, "excludeTmpdirEnvVar": False,
                                "excludeSlashTmp": False})
        self.assertIsNotNone(refused, "a writable root that is not a path was accepted")
        self.assertIn("cannot be read in full", refused.reason)

    def test_a_relative_writable_root_is_refused(self):
        refused = self.declare({"type": "workspaceWrite", "writableRoots": ["relative/path"],
                                "networkAccess": False, "excludeTmpdirEnvVar": False,
                                "excludeSlashTmp": False})
        self.assertIsNotNone(refused, "a relative writable root was accepted")

    def test_a_flag_that_is_not_a_boolean_is_refused(self):
        refused = self.declare({"type": "workspaceWrite", "writableRoots": [],
                                "networkAccess": "false", "excludeTmpdirEnvVar": False,
                                "excludeSlashTmp": False})
        self.assertIsNotNone(refused, "a policy flag that is not a boolean was accepted")

    def test_a_field_the_type_does_not_carry_is_refused(self):
        refused = self.declare({"type": "readOnly", "networkAccess": False,
                                "writableRoots": []})
        self.assertIsNotNone(refused, "a field this type does not carry was accepted")

    def test_the_policies_a_trial_may_use_are_still_accepted(self):
        # Support: each type at its own defaults, and workspaceWrite with a real root.
        for policy in ({"type": "dangerFullAccess"}, {"type": "readOnly"},
                       {"type": "workspaceWrite"},
                       {"type": "workspaceWrite", "writableRoots": ["/tmp"]}):
            with self.subTest(policy=policy):
                world = World(self.base)
                self.addCleanup(world.stop)
                for boundary in world.record["boundaries"]:
                    for participant in boundary["participants"]:
                        participant["expect"]["sandbox"] = dict(policy)
                world.flush()
                self.assertIsNone(world.refusal())

    def test_the_field_sets_are_the_bridges_own(self):
        # Support, and the guard on the fifth copied contract, read as a value.
        source = relay_source("packages", "codex-thread-bridge", "src", "codex_thread_bridge",
                              "bridge.py")
        theirs = assigned_literal(source, "fields")
        self.assertEqual({kind: {"type", *names}
                          for kind, names in startup.POLICY_PROTOCOL_FIELDS.items()},
                         {kind: set(names) for kind, names in theirs.items()})


class FortyThirdHostedRound(TrialCase):
    """What the confirmation pass can and cannot say, stated in the document rather than implied."""

    def test_the_confirmation_reports_the_span_it_covered(self):
        # The pass reads sequentially, so a row read at its start can be replaced before its end
        # and this will not see it. No finite number of passes closes that; the span is measured
        # and reported so the unguarded interval is a number rather than an assumption.
        self.world.start_supervisor()
        cell = cells_of(self.world.preflight(), "assignmentState")["gateReadsHeld"]
        self.assertEqual(cell["value"], VERIFIED)
        self.assertIn("reads them in sequence over", cell["evidence"])
        self.assertIn("bounds the window and does not remove it", cell["evidence"])

    def test_the_document_names_what_no_pass_can_close(self):
        self.world.start_supervisor()
        document = self.world.preflight()
        snapshot = document["standIns"]["storeSnapshot"]
        self.assertIn("No finite number of passes", snapshot)
        self.assertIn("relay exposes no revision", snapshot)

    def test_the_span_is_measured_on_a_clock_that_cannot_go_backwards(self):
        # A duration is not the difference between two moments: a synchronisation step during the
        # pass would otherwise be reported as part of the interval, or as a negative one.
        source = (ROOT / "scripts" / "trial_startup.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        node = next(n for n in ast.walk(tree)
                    if isinstance(n, ast.FunctionDef) and n.name == "gate_reads_held")
        used = {a.attr for a in ast.walk(node) if isinstance(a, ast.Attribute)}
        self.assertIn("monotonic", used)
        self.assertNotIn("time", used, "a wall-clock reading is not a duration")

        self.world.start_supervisor()
        cell = cells_of(self.world.preflight(), "assignmentState")["gateReadsHeld"]
        span = float(cell["evidence"].split("in sequence over ")[1].split(" seconds")[0])
        self.assertGreaterEqual(span, 0)

    def test_a_workspace_root_that_is_not_a_path_refuses_the_start(self):
        # A list of the wrong things is still a list. The creation path refuses a root that is
        # not an absolute string, so a receipt carrying one cannot be genuine and the resume
        # parameters built from it are invalid.
        for roots in ([7], ["relative/path"], ["/ok", 7]):
            with self.subTest(roots=roots):
                world = World(self.base)
                self.addCleanup(world.stop)
                world.payloads["settings-show"]["payload"]["settings"][
                    "runtimeWorkspaceRoots"] = roots
                for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
                    capture = world.captures["receipt-" + task + ".json"]
                    for half in ("requested", "actual"):
                        capture["settings"][half]["runtimeWorkspaceRoots"] = roots
                world.start_supervisor()
                world.flush()
                document = world.preflight()
                self.assertFalse(document["readyToStart"],
                                 "a root the creation path refuses was certified")
                cell = cells_of(document,
                                "capability")["deliverableSettings:" + World.PARENT_A]
                self.assertEqual(cell["value"], NOT_VERIFIED)
                self.assertIn("not a list of absolute paths", cell["evidence"])

    def test_real_absolute_roots_are_still_accepted(self):
        # Support: a root list that is what it should be keeps working.
        self.world.payloads["settings-show"]["payload"]["settings"][
            "runtimeWorkspaceRoots"] = [str(self.world.repos["A"])]
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            for half in ("requested", "actual"):
                capture["settings"][half]["runtimeWorkspaceRoots"] = [str(self.world.repos["A"])]
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def test_a_model_or_effort_that_is_not_a_word_is_refused(self):
        # The creation path requires each of these to be a non-empty string, so a record naming
        # anything else describes a receipt that could never have been produced. A presence check
        # read 7 as a model because str() makes it a word, and "" because two blanks agree.
        for field, value in (("model", 7), ("model", ""), ("model", "   "),
                             ("reasoningEffort", ""), ("reasoningEffort", False)):
            with self.subTest(field=field, value=value):
                world = World(self.base)
                self.addCleanup(world.stop)
                for boundary in world.record["boundaries"]:
                    for participant in boundary["participants"]:
                        participant["expect"][field] = value
                world.flush()
                refused = world.refusal()
                self.assertIsNotNone(refused, repr(value) + " was accepted as a " + field)
                self.assertIn("non-empty strings", refused.reason)
                self.assertIn(field, refused.detail["fields"])

    def test_a_stored_row_with_a_blank_setting_refuses_the_start(self):
        self.world.payloads["settings-show"]["payload"]["settings"]["model"] = ""
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            for half in ("requested", "actual"):
                capture["settings"][half]["model"] = ""
        self.world.start_supervisor()
        self.world.flush()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "a row the creation path could not have produced was certified")
        cell = cells_of(document, "capability")["deliverableSettings:" + World.PARENT_A]
        self.assertIn("model is not a non-empty string", cell["evidence"])

    def test_a_setting_longer_than_the_creation_policy_accepts_is_refused(self):
        # The creation policy refuses a model or an effort past its maximum before any host call,
        # so a record naming a longer one describes a participant no bridge created.
        # Written as a length rather than through the checker's own constant, so this says what
        # the policy requires instead of what this module happens to hold.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["model"] = "m" * 501
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a model past the creation policy's maximum was accepted")
        self.assertIn("model", refused.detail["fields"])

    def test_a_setting_at_the_maximum_is_still_accepted(self):
        # Support: the boundary itself is allowed, as the policy allows it.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["model"] = "m" * 500
        self.world.flush()
        self.assertIsNone(self.world.refusal())

    def test_a_long_working_directory_is_not_held_to_the_execution_policys_bound(self):
        # cwd goes through a different check with a far larger bound, so holding it to the
        # execution policy's 500 refused a working directory the bridge would have created.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["cwd"] = "/" + "d" * 600
        self.world.flush()
        self.assertIsNone(self.world.refusal(),
                          "a working directory the bridge accepts was refused")

    def test_the_maximum_is_the_bridges_own(self):
        # Support, and the guard on the sixth copied constant, read as a value.
        source = relay_source("packages", "codex-thread-bridge", "src", "codex_thread_bridge",
                              "execution.py")
        self.assertEqual(startup.SETTING_MAXIMUM, assigned_literal(source, "MAXIMUM"))


class FortyFourthHostedRound(TrialCase):
    """Three readings whose answers could expire before the gate they fed."""

    def test_a_supervisor_that_leaves_after_its_reading_is_caught_at_the_gate(self):
        # The process reading is taken near the start of the pass and every probe after it takes
        # real time. A supervisor that exits while they run left all of those cells verified and
        # published readiness for a trial with nothing polling, which is the staged-delivery
        # failure this preflight exists to prevent.
        self.world.start_supervisor()
        original = startup.reading_lifecycle

        def stop_the_supervisor_then_read(record):
            if self.world.supervisor is not None:
                self.world.supervisor.terminate()
                self.world.supervisor.wait(timeout=5)
                self.world.supervisor = None
            return original(record)

        startup.reading_lifecycle = stop_the_supervisor_then_read
        self.addCleanup(setattr, startup, "reading_lifecycle", original)
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "readiness was published for a trial whose poller had already exited")
        self.assertIn("supervisorStillRunning.passed", document["judgmentsThatFailed"])
        # The reading taken at the start still says what it saw, which is why nothing before this
        # noticed: it was true when it was taken.
        self.assertEqual(document["readings"]["processPersistence"]["value"], VERIFIED)

    def test_a_store_replaced_after_its_reading_is_caught_at_the_gate(self):
        # doctor runs first, before anything else constructs a store, and every settings,
        # criteria and assignment probe after it opens whatever the state directory names then.
        # A copy carries the same store id, the same challenge nonce and the same rows, and a
        # device and inode of its own.
        copied = json.loads(json.dumps(self.world.payloads["doctor"]["payload"]))
        copied["sameStore"] = "unproven"
        copied["store"]["inode"] = World.INODE + 1
        self.world.payloads["after"] = {"subcommand": "doctor", "calls": 1,
                                        "payloads": {"doctor": {"payload": copied}}}
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "readiness was published against a store the probes had stopped using")
        self.assertIn("storeStillTheSame.passed", document["judgmentsThatFailed"])
        self.assertEqual(document["storeStillTheSame"]["before"], "proven")
        self.assertEqual(document["readings"]["storeIdentity"]["value"], VERIFIED)

    def test_the_window_is_read_on_one_clock(self):
        # Two readings of the clock a moment apart called a window that opened between them both
        # still ahead and inside the allowance, and then dated that answer at a moment the window
        # had already opened.
        opens = time.time() + 60
        self.world.record["window"]["opensAt"] = startup.stamp(opens)
        self.world.record["window"]["closesAt"] = startup.stamp(opens + 600)
        self.world.start_supervisor()

        class Straddling:
            """Real time, except for the readings the window check itself takes."""

            def __init__(self):
                self.armed = False
                self.values = [opens - 0.1, opens + 0.1, opens + 0.1]

            def time(self):
                if self.armed and self.values:
                    return self.values.pop(0)
                return time.time()

            def __getattr__(self, name):
                return getattr(time, name)

        clock = Straddling()
        original = startup.captures_still_fresh

        def arm_the_clock_then_read(record):
            # The reading immediately before the window check in every version of this run, and
            # it takes no clock reading of its own, so the scripted values reach that check.
            answer = original(record)
            clock.armed = True
            return answer

        startup.time = clock
        startup.captures_still_fresh = arm_the_clock_then_read
        self.addCleanup(setattr, startup, "captures_still_fresh", original)
        self.addCleanup(setattr, startup, "time", time)
        document = self.world.preflight()
        window = document["windowStillAhead"]
        self.assertTrue(window["passed"], "the fixture never reached the straddle it is about")
        self.assertLess(startup.moment(window["readAt"], "readAt"),
                        startup.moment(window["opensAt"], "window.opensAt"),
                        "the window check passed at a moment the window had already opened")


class FortyFifthHostedRound(TrialCase):
    """A receipt shape no bridge writes, a reading that graded one field of three, and a zombie."""

    def test_a_receipt_in_the_bridges_own_shape_is_read(self):
        # The bridge identifies the thread it created at threadId. The fixture wrote the relay's
        # word for the same participant, so the cell passed here and would have answered
        # unreadable for every receipt a real trial captured, refusing every good trial.
        capture = self.world.captures["receipt-" + World.PARENT_A + ".json"]
        self.assertIn("threadId", capture)
        self.assertNotIn("taskId", capture)
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])
        self.assertEqual(
            cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]["value"], VERIFIED)

    def test_the_bridge_writes_the_created_thread_at_threadid(self):
        # Support, and the guard on that choice, read as the calls the creation path makes rather
        # than as words in a file: if the bridge ever writes the participant under another key,
        # this fails instead of the checker quietly refusing every receipt.
        source = relay_source("packages", "codex-thread-bridge", "src", "codex_thread_bridge",
                              "bridge.py")
        node = next(n for n in ast.walk(ast.parse(source))
                    if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
                    and n.name == "create_thread")
        written = {word.arg for call in ast.walk(node)
                   if isinstance(call, ast.Call) and isinstance(call.func, ast.Attribute)
                   and call.func.attr == "update"
                   and isinstance(call.func.value, ast.Name) and call.func.value.id == "receipt"
                   for word in call.keywords}
        self.assertIn("threadId", written)
        self.assertNotIn("taskId", written)

    def test_a_receipt_that_never_asked_for_a_setting_is_not_an_echo_of_it(self):
        # A field absent from what the creation asked for can never produce a finding, so a
        # receipt whose requested omits the execution settings reports empty findings while
        # actual carries whatever the thread inherited. Reading that as an echo confirmed a
        # model, an effort and a sandbox nobody ever asked the host for.
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            capture = self.world.captures["receipt-" + task + ".json"]
            asked = {key: value for key, value in capture["settings"]["requested"].items()
                     if key not in ("model", "reasoningEffort", "sandbox")}
            capture["settings"]["requested"] = asked
            capture["settings"]["verified"] = sorted(asked)
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "a receipt that never asked for the model was read as echoing it")
        cell = cells_of(document, "capability")["receiptEcho:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_VERIFIED)
        self.assertIn("never asked for", cell["evidence"])

    def test_the_requestable_settings_are_the_contracts_own(self):
        # Support, and the guard on the seventh copied constant, read as the keys the contract's
        # own requested property builds rather than as words in a file.
        source = relay_source("packages", "codex-thread-bridge", "src", "codex_thread_bridge",
                              "settings.py")
        node = next(n for n in ast.walk(ast.parse(source))
                    if isinstance(n, ast.FunctionDef) and n.name == "requested")
        keys = {target.slice.value for target in ast.walk(node)
                if isinstance(target, ast.Subscript) and isinstance(target.value, ast.Name)
                and target.value.id == "asked" and isinstance(target.slice, ast.Constant)}
        self.assertEqual(set(startup.REQUESTABLE_SETTINGS), keys)
        # And the one deliberately not there: the contract decides the approval policy first and
        # alone on every receipt, so empty findings do establish that one and requiring it here
        # would refuse every receipt a bridge writes.
        self.assertNotIn("approvalPolicy", keys)

    def test_a_creation_that_reported_no_profile_does_not_clear_a_stored_one(self):
        # The row is what a resume is compared against, so a store expectation does not stop
        # mattering because the creation response was silent about it. Answering not_applicable
        # there said the question did not arise while the store held the value it is about.
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            self.world.captures["receipt-" + task + ".json"]["creation"].pop(
                "activePermissionProfile")
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "a stored permission expectation nothing established was read as met")
        cell = cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]
        self.assertEqual(cell["value"], UNKNOWN)

    def test_neither_side_naming_a_profile_is_still_not_applicable(self):
        # Support: the arrangement the not_applicable answer is actually for. With no permission
        # source anywhere there is nothing for a resume to be checked against, and refusing it
        # would refuse a working trial.
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            self.world.captures["receipt-" + task + ".json"]["creation"].pop(
                "activePermissionProfile")
        self.world.payloads["settings-show"]["payload"]["settings"].pop(
            "expectedPermissionProfile")
        self.world.start_supervisor()
        document = self.world.preflight()
        cell = cells_of(document, "capability")["permissionProfile:" + World.PARENT_A]
        self.assertEqual(cell["value"], NOT_APPLICABLE)
        self.assertTrue(document["readyToStart"], document["judgmentsThatFailed"])

    def stopped_supervisor(self):
        """A real supervisor stopped by a signal, continued again before the world reaps it.

        Registered after the world's own cleanup and so run before it: SIGTERM does not reach a
        stopped process, and leaving one behind would outlive this suite.
        """
        pid = self.world.supervisor.pid
        self.addCleanup(self.let_it_run_again, pid)
        os.kill(pid, signal.SIGSTOP)
        deadline = time.time() + 5
        while time.time() < deadline and process_state_of(pid) != "T":
            time.sleep(0.02)
        return pid

    @staticmethod
    def let_it_run_again(pid):
        try:
            os.kill(pid, signal.SIGCONT)
        except OSError:                                              # pragma: no cover
            pass

    def test_a_stopped_supervisor_is_not_a_running_one_at_the_gate(self):
        # A supervisor stopped by a signal answers kill(pid, 0) and reports a detached session
        # exactly as a zombie does, and with an advance interval longer than the rest of the pass
        # its unchanged counter is not a regression either. Refusing the zombie by its own letter
        # closed that state and left this one.
        if process_state_of(os.getpid()) is None:
            raise unittest.SkipTest("this host does not report a process state to read")
        self.world.start_supervisor()
        self.world.record["supervisor"]["witnessAdvanceSeconds"] = startup.ADVANCE_CEILING
        self.world.flush()
        original = startup.reading_lifecycle

        def stop_the_supervisor_then_read(record):
            self.stopped_supervisor()
            return original(record)

        startup.reading_lifecycle = stop_the_supervisor_then_read
        self.addCleanup(setattr, startup, "reading_lifecycle", original)
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "readiness was published for a supervisor stopped by a signal")
        self.assertIn("supervisorStillRunning.passed", document["judgmentsThatFailed"])

    def test_the_first_reading_refuses_a_supervisor_that_is_not_running(self):
        # The control over the other liveness site, driven with each state the decision calls
        # stopped that a case can actually produce. Neither may answer alive.
        if process_state_of(os.getpid()) is None:
            raise unittest.SkipTest("this host does not report a process state to read")
        self.world.start_supervisor()
        pid = self.stopped_supervisor()
        self.assertEqual(process_state_of(pid), "T")
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "processPersistence")["alive"]["value"], NOT_VERIFIED)
        self.assertFalse(document["readyToStart"])

        self.let_it_run_again(pid)
        self.world.supervisor.terminate()
        deadline = time.time() + 5
        while time.time() < deadline and process_state_of(pid) != "Z":
            time.sleep(0.02)
        self.assertEqual(process_state_of(pid), "Z")
        document = self.world.preflight()
        self.assertEqual(cells_of(document, "processPersistence")["alive"]["value"], NOT_VERIFIED)
        self.assertFalse(document["readyToStart"])

    def test_every_state_the_kernel_defines_is_decided_once(self):
        # The letters proc(5) defines for the state field. They are written out here because that
        # definition is not in this repository to derive them from, and the point of writing them
        # is that the decision covers all of them rather than the two a review happened to report.
        defined = {"R", "S", "D", "Z", "T", "t", "W", "X", "x", "K", "P", "I"}
        running, stopped = set(startup.PROCESS_RUNNING), set(startup.PROCESS_STOPPED)
        self.assertEqual(running | stopped, defined, "a state the kernel defines is undecided")
        self.assertEqual(running & stopped, set(), "a state is decided both ways")
        for letter in sorted(stopped):
            self.assertIs(startup.running_state(letter), False, letter + " answered running")
        for letter in sorted(running):
            self.assertIs(startup.running_state(letter), True, letter + " did not answer running")
        # A letter nobody classified is not thereby a running one. Unreadable refuses the start.
        self.assertIsNone(startup.running_state("q"))

    def test_liveness_is_decided_in_one_place(self):
        # The sweep: every place in this module that decides a process is running goes through
        # alive(), which is enforced by there being nowhere else that asks the kernel.
        tree = ast.parse((ROOT / "scripts" / "trial_startup.py").read_text(encoding="utf-8"))
        asked = set()
        for node in ast.walk(tree):
            if not isinstance(node, ast.FunctionDef):
                continue
            for call in ast.walk(node):
                if (isinstance(call, ast.Call) and isinstance(call.func, ast.Attribute)
                        and call.func.attr == "kill" and isinstance(call.func.value, ast.Name)
                        and call.func.value.id == "os"):
                    asked.add(node.name)
        self.assertEqual(asked, {"alive"}, "a liveness decision is made outside alive()")

    def test_a_declared_setting_the_contract_cannot_carry_refuses_the_start(self):
        # A record could declare a setting no creation can ask for and no receipt can verify.
        # With the capture and the store row both copying it the comparison agreed, every
        # capability cell passed, and the trial was declared ready on a field nothing in the path
        # requests, verifies or preserves.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["futureSetting"] = "x"
        for task in (World.PARENT_A, World.CHILD_A, World.PARENT_B, World.CHILD_B):
            self.world.captures["receipt-" + task + ".json"]["settings"]["actual"][
                "futureSetting"] = "x"
        self.world.payloads["settings-show"]["payload"]["settings"]["futureSetting"] = "x"
        self.world.flush()
        refused = self.world.refusal()
        self.assertIsNotNone(refused, "a setting no creation can ask for was declared and accepted")
        self.assertIn("futureSetting", refused.detail["fields"])

    def test_every_setting_a_creation_can_ask_for_may_still_be_declared(self):
        # The bound on that refusal, so it refuses unsupported keys rather than unfamiliar ones:
        # the two carryable settings beyond the four a record must name still load.
        for boundary in self.world.record["boundaries"]:
            for participant in boundary["participants"]:
                participant["expect"]["cwd"] = participant["cwd"]
                participant["expect"]["runtimeWorkspaceRoots"] = []
        self.world.flush()
        self.assertIsNone(self.world.refusal())

    def test_what_a_record_may_declare_is_what_a_creation_can_carry(self):
        # Support: the declarable set is derived from the requestable one rather than written
        # beside it, and the approval policy is the single addition, which is the exception the
        # contract itself makes by deciding that one on every receipt.
        self.assertEqual(set(startup.DECLARABLE_SETTINGS),
                         set(startup.REQUESTABLE_SETTINGS) | {"approvalPolicy"})
        self.assertLessEqual(set(startup.REQUIRED_EXPECT), set(startup.DECLARABLE_SETTINGS))

    def test_the_final_doctor_grades_the_reachability_it_reports(self):
        # The socket and the write access were graded once, at the start. The same payload the
        # identity recheck reads answers both again, and a path that stopped answering leaves
        # those earlier cells verified while the dispatch this clears cannot use it.
        refused = json.loads(json.dumps(self.world.payloads["doctor"]["payload"]))
        refused["actorReachability"]["socketConnect"] = "refused"
        self.world.payloads["after"] = {"subcommand": "doctor", "calls": 1,
                                        "payloads": {"doctor": {"payload": refused}}}
        self.world.start_supervisor()
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "readiness was published for a relay path the dispatch cannot use")
        self.assertIn("storeStillTheSame.passed", document["judgmentsThatFailed"])
        self.assertEqual(document["storeStillTheSame"]["socketReachable"], "refused")
        # The first reading still says what it saw, which is why nothing else noticed.
        self.assertEqual(cells_of(document, "storeIdentity")["socketReachable"]["value"], VERIFIED)

    def test_a_zombie_supervisor_is_not_a_running_one(self):
        # A process that exited and has not been reaped still answers kill(pid, 0) and still
        # reports a detached session, and an advance interval longer than the rest of the pass
        # means its unchanged counter is not a regression either. Every reading the final gate
        # took said the poller was there, and it was polling nothing.
        if process_state_of(os.getpid()) is None:
            raise unittest.SkipTest("this host does not report a process state to read")
        self.world.start_supervisor()
        # The ceiling the record allows, which is longer than the rest of this pass takes. The
        # fixture's sleeper caps the pause itself, so nothing here waits that long.
        self.world.record["supervisor"]["witnessAdvanceSeconds"] = startup.ADVANCE_CEILING
        self.world.flush()
        pid = self.world.supervisor.pid
        original = startup.reading_lifecycle

        def leave_a_zombie_then_read(record):
            # Terminated and deliberately not reaped, which is the state this case is about. The
            # world's own cleanup reaps it afterwards.
            self.world.supervisor.terminate()
            deadline = time.time() + 5
            while time.time() < deadline and process_state_of(pid) != "Z":
                time.sleep(0.02)
            return original(record)

        startup.reading_lifecycle = leave_a_zombie_then_read
        self.addCleanup(setattr, startup, "reading_lifecycle", original)
        document = self.world.preflight()
        self.assertFalse(document["readyToStart"],
                         "readiness was published for a supervisor that had already exited")
        self.assertIn("supervisorStillRunning.passed", document["judgmentsThatFailed"])
        self.assertEqual(process_state_of(pid), "Z", "the case did not reach the state it is about")


if __name__ == "__main__":                                           # pragma: no cover
    unittest.main()
