"""A parent another task's bridge message loaded is still reached by the relay (CRW-235).

CRW-124 G2, on the shared host at codex-cli 0.154.0: a supervisor's bridge message loaded an
unloaded parent with a plain thread/resume, the host brought it back with its cwd as the only
workspace root, and every relay delivery to that parent was then withheld as
settings_not_preserved (one root against four recorded) until the attempt cap. Measured again on an
isolated app-server of the same release: a resume changes nothing on a thread the host already has
loaded; a plain load reports the cwd as the only root; a load that transmits the roots applies
them; and a workspace-write thread's reported writableRoots follow its runtime roots.

The fake host below reproduces exactly those four facts, and both senders run their real code
against it: the bridge's send_message_to_thread and the relay's guarded send.

Each test is labelled RED where it fails on the relay before this change, or GREEN where it pins
behaviour that must not change.
"""

import asyncio
import shutil
import tempfile
import unittest
from pathlib import Path

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_session_relay.transport import WITHHELD_PRE_SEND, classify_operation_receipt

PARENT = "01parent-loaded-elsewhere"
CWD = "/workspace/example/tasks/PA"
RUN = "/workspace/example/run"
EVIDENCE = "/workspace/example/evidence"
RELAY_STATE = "/workspace/example/relay-state"
ELSEWHERE = "/workspace/example/elsewhere"
RECORDED = [CWD, RUN, EVIDENCE, RELAY_STATE]
# The note an accepted narrowing leaves on the transport receipt (settings.RUNTIME_ROOTS_NARROWER).
RUNTIME_ROOTS_NARROWER = "runtime_roots_narrower_than_record"
MODEL = "anthropic/claude-opus-5"
EFFORT = "xhigh"
DANGER = "danger-full-access"
WRITE = "workspace-write"


class Host:
    """One App Server. A thread persists its cwd, model, effort and sandbox mode; its workspace
    roots exist only while it is loaded, and only a load sets them."""

    def __init__(self):
        self.threads = {}
        self.calls = []
        self.turns = 0
        self.before_resume = None
        self.socket_path = "/nonexistent/app-server.sock"
        self.info = {}

    def thread(self, thread_id=PARENT, *, mode=DANGER, network=False):
        self.threads[thread_id] = {"status": "notLoaded", "cwd": CWD, "mode": mode,
                                   "model": MODEL, "effort": EFFORT, "network": network,
                                   "roots": None, "env_roots": None, "writable": None}

    def load(self, thread_id=PARENT, roots=None, *, env_roots=None, writable=None):
        """A load somebody else made: roots as transmitted, or the cwd alone."""
        one = self.threads[thread_id]
        one["roots"] = list(roots) if roots else [one["cwd"]]
        one["env_roots"] = list(env_roots) if env_roots is not None else None
        one["writable"] = list(writable) if writable is not None else None
        one["status"] = "idle"

    def unload(self, thread_id=PARENT):
        one = self.threads[thread_id]
        one.update(status="notLoaded", roots=None, env_roots=None, writable=None)

    def _sandbox(self, one):
        if one["mode"] == WRITE:
            writable = (one["writable"] if one["writable"] is not None
                        else [root for root in one["roots"] if root != one["cwd"]])
            return {"type": "workspaceWrite", "writableRoots": writable,
                    "networkAccess": one["network"], "excludeTmpdirEnvVar": False,
                    "excludeSlashTmp": False}
        return {"type": "dangerFullAccess"}

    def _environments(self, one):
        if one["roots"] is None:
            return None
        roots = one["env_roots"] if one["env_roots"] is not None else one["roots"]
        return [{"environmentId": "local", "cwd": one["cwd"], "runtimeWorkspaceRoots": list(roots)}]

    def _answer(self, thread_id):
        one = self.threads[thread_id]
        return {
            "approvalPolicy": "never", "approvalsReviewer": "user",
            "sandbox": self._sandbox(one), "cwd": one["cwd"],
            "runtimeWorkspaceRoots": list(one["roots"]), "model": one["model"],
            "reasoningEffort": one["effort"], "activePermissionProfile": None,
            "thread": {"id": thread_id, "status": {"type": one["status"]},
                       "environments": self._environments(one), "cwd": one["cwd"],
                       "model": one["model"], "reasoningEffort": one["effort"]},
        }

    async def call(self, method, params):
        self.calls.append((method, dict(params)))
        thread_id = params["threadId"]
        one = self.threads[thread_id]
        if method == "thread/read":
            return {"thread": {"id": thread_id, "status": {"type": one["status"]},
                               "environments": self._environments(one), "cwd": one["cwd"],
                               "model": one["model"], "reasoningEffort": one["effort"]}}
        if method == "thread/resume":
            hook, self.before_resume = self.before_resume, None
            if hook is not None:
                hook(thread_id)
            if one["status"] == "notLoaded":
                # Materializing applies what the resume transmits; a loaded thread keeps its state.
                one["roots"] = list(params.get("runtimeWorkspaceRoots") or [one["cwd"]])
                one["status"] = "idle"
            return self._answer(thread_id)
        if method == "turn/start":
            self.turns += 1
            return {"turn": {"id": f"turn-{self.turns}"}}
        raise AssertionError(f"unexpected {method}")

    def request_mark(self):
        return 0

    def requests_since(self, mark, thread_id=None):
        return {"thisThread": [], "unattributed": [], "otherThreads": 0,
                "approvalsLeftForThisThread": 0, "refusedForThisThread": 0, "notRetained": 0,
                "note": "fake host"}

    async def connect(self):
        return None

    async def close(self):
        return None

    def methods(self, since=0):
        return [method for method, _params in self.calls[since:]]


def record(*, mode=DANGER, roots=RECORDED, settings_free=False, sandbox=None):
    """The parent's settings record, as a creation receipt with four roots would produce it."""
    if mode == WRITE:
        sandbox = {"type": "workspaceWrite", "writableRoots": [r for r in roots if r != CWD],
                   "networkAccess": False, "excludeTmpdirEnvVar": False, "excludeSlashTmp": False}
    elif sandbox is None:
        sandbox = {"type": "dangerFullAccess"}
    settings = TaskSettings({
        "sandbox": sandbox, "approvalPolicy": "never", "cwd": CWD,
        "runtimeWorkspaceRoots": list(roots), "model": MODEL, "reasoningEffort": EFFORT,
        "environments": [{"environmentId": "local", "cwd": CWD, "runtimeWorkspaceRoots": list(roots)}],
    })
    settings.settings_free_resume = settings_free
    return settings


class BridgeLoadThenRelay(unittest.TestCase):
    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        self.tmp = tempfile.mkdtemp(prefix="crw235-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.host = Host()
        self.sequence = 0

    def bridge_send(self, *, mode=DANGER, roots=None):
        """Another task's message through the bridge's own tool path."""
        from codex_thread_bridge.bridge import Bridge
        from codex_thread_bridge.ledger import Ledger

        expected = {"cwd": CWD, "sandbox": mode, "model": MODEL, "reasoning_effort": EFFORT}
        if roots is not None:
            expected["runtime_workspace_roots"] = list(roots)
        self.sequence += 1
        ledger = Ledger(Path(self.tmp) / "bridge-operations.sqlite3")
        return asyncio.run(Bridge(self.host, ledger).send_message_to_thread(
            f"other-task-{self.sequence}", PARENT, "a message from another task",
            expected_settings=expected))

    def relay_send(self, settings, number=1):
        from codex_thread_bridge.ledger import Ledger

        path = Path(self.tmp) / "relay-operations.sqlite3"
        adapter = BridgeHostAdapter(
            str(Path(self.tmp) / "socket"),
            app_server_factory=lambda canonical: self.host,
            ledger_factory=lambda: (Path(self.tmp) / "socket", Ledger(path)),
            timeout=10,
        )
        self.addCleanup(adapter.close)
        return adapter.send_message(f"del-235000000000-a{number}", PARENT, "the child's report",
                                    settings)

    def assert_withheld(self, receipt, since, code="settings_not_preserved", field=None):
        self.assertNotIn("turn/start", self.host.methods(since), "a turn was started anyway")
        self.assertEqual(receipt["status"], "failed", receipt)
        self.assertEqual(receipt["rpcError"]["code"], code)
        facts = classify_operation_receipt(receipt)
        self.assertEqual((facts.delivery_state, facts.send_attempted),
                         (WITHHELD_PRE_SEND, "no"))
        if field is not None:
            self.assertEqual(receipt["settingsFindings"][0]["field"], field)

    def assert_started(self, receipt, since):
        self.assertEqual(receipt["status"], "accepted", receipt.get("error"))
        self.assertEqual(self.host.methods(since).count("turn/start"), 1)

    @staticmethod
    def notes(receipt):
        return {note["field"]: note for note in receipt.get("settingsNotes") or []
                if note.get("code") == RUNTIME_ROOTS_NARROWER}

    # ------------------------------------------------------------------ the defect

    def test_a_parent_another_task_loaded_through_the_bridge_is_delivered(self):
        """RED (case 1): withheld as settings_not_preserved on every attempt."""
        self.host.thread()
        loaded = self.bridge_send()
        self.assertEqual(loaded["status"], "accepted", loaded.get("error"))
        self.assertEqual(loaded["statusBeforeResume"], "notLoaded")
        self.assertEqual(self.host.threads[PARENT]["roots"], [CWD], "the bridge load kept more roots")
        since = len(self.host.calls)
        receipt = self.relay_send(record())
        self.assert_started(receipt, since)
        resume = [params for method, params in self.host.calls[since:] if method == "thread/resume"]
        self.assertEqual(resume[0]["runtimeWorkspaceRoots"], RECORDED,
                         "the relay stopped transmitting the recorded roots")
        self.assertEqual(receipt["statusBeforeResume"], "idle")
        notes = self.notes(receipt)
        self.assertEqual(set(notes), {"runtimeWorkspaceRoots", "environments[0].runtimeWorkspaceRoots"})
        for note in notes.values():
            self.assertEqual((note["recorded"], note["observed"], note["statusBeforeResume"]),
                             (RECORDED, [CWD], "idle"))

    def test_a_workspace_write_parent_the_bridge_loaded_is_delivered(self):
        """RED (case 9): the host's writableRoots follow the narrowed roots, so the sandbox differs."""
        self.host.thread(mode=WRITE)
        self.assertEqual(self.bridge_send(mode=WRITE)["status"], "accepted")
        since = len(self.host.calls)
        receipt = self.relay_send(record(mode=WRITE))
        self.assert_started(receipt, since)
        notes = self.notes(receipt)
        self.assertEqual(set(notes), {"runtimeWorkspaceRoots", "environments[0].runtimeWorkspaceRoots",
                                      "sandbox.writableRoots"})
        self.assertEqual((notes["sandbox.writableRoots"]["recorded"],
                          notes["sandbox.writableRoots"]["observed"]),
                         ([RUN, EVIDENCE, RELAY_STATE], []))

    def test_the_settings_free_route_notes_the_narrowing_it_already_accepted(self):
        """RED for the note (case 6); GREEN for the delivery, which that route already made."""
        self.host.thread()
        self.bridge_send()
        since = len(self.host.calls)
        receipt = self.relay_send(record(settings_free=True))
        self.assert_started(receipt, since)
        self.assertEqual(set(self.notes(receipt)),
                         {"runtimeWorkspaceRoots", "environments[0].runtimeWorkspaceRoots"})

    def test_a_reordered_root_list_on_a_loaded_parent_is_the_same_roots(self):
        """RED (case 8): refused today; the same roots in another order are no narrowing either."""
        self.host.thread()
        self.host.load(roots=list(reversed(RECORDED)))
        since = len(self.host.calls)
        receipt = self.relay_send(record())
        self.assert_started(receipt, since)
        self.assertEqual(self.notes(receipt), {})

    # -------------------------------------------------------------- the controls

    def test_the_relay_loading_the_parent_itself_is_exact(self):
        """GREEN (case 2): the host applies what the relay transmits while it materializes."""
        self.host.thread()
        receipt = self.relay_send(record())
        self.assert_started(receipt, 0)
        self.assertEqual(receipt["statusBeforeResume"], "notLoaded")
        self.assertEqual(self.host.threads[PARENT]["roots"], RECORDED)
        self.assertEqual(self.notes(receipt), {})

    def test_an_unload_between_the_read_and_the_resume_is_loaded_exactly(self):
        """GREEN (case 7): read idle, unloaded before the resume, loaded by it with the record."""
        self.host.thread()
        self.host.load(roots=RECORDED)
        self.host.before_resume = self.host.unload
        receipt = self.relay_send(record())
        self.assert_started(receipt, 0)
        self.assertEqual(self.host.threads[PARENT]["roots"], RECORDED)
        self.assertEqual(self.notes(receipt), {})

    # ------------------------------------------------------ never wider, never other

    def test_a_root_the_record_does_not_name_is_refused_after_a_bridge_load(self):
        """GREEN (case 3): a bridge load that named another root is wider, whoever loaded it."""
        self.host.thread()
        self.assertEqual(self.bridge_send(roots=[CWD, ELSEWHERE])["status"], "accepted")
        since = len(self.host.calls)
        self.assert_withheld(self.relay_send(record()), since)

    def test_an_environment_root_the_record_does_not_name_is_refused(self):
        """GREEN (case 3, per environment): narrower at the top level, wider in the environment."""
        self.host.thread()
        self.host.load(roots=[CWD], env_roots=[CWD, ELSEWHERE])
        self.assert_withheld(self.relay_send(record()), 0, field="environments")

    def test_a_narrower_answer_to_a_resume_that_loaded_the_parent_is_refused(self):
        """GREEN (case 4): read notLoaded, loaded by somebody else before the relay's resume. The
        relay's resume transmitted the record to a thread it expected to load, so a narrower answer
        is a preservation failure there; retry-safe, and the next pass reads the thread idle."""
        self.host.thread()
        self.host.before_resume = lambda thread_id: self.host.load(thread_id)
        receipt = self.relay_send(record())
        self.assertEqual(receipt["statusBeforeResume"], "notLoaded")
        self.assert_withheld(receipt, 0)
        self.assertTrue(classify_operation_receipt(receipt).retry_safe)

    def test_the_sandbox_may_narrow_only_where_the_roots_may(self):
        """GREEN (case 12): a thread read as notLoaded whose roots come back exact and whose
        writableRoots come back narrower is refused on the sandbox."""
        self.host.thread(mode=WRITE)
        original = self.host._sandbox
        # Nobody else loads it: the relay's own resume materializes it, with its roots as
        # transmitted and fewer writable roots than the record.
        self.host._sandbox = lambda one: dict(original(one), writableRoots=[RUN])
        self.assert_withheld(self.relay_send(record(mode=WRITE)), 0, field="sandbox")

    def test_every_other_setting_is_still_compared_exactly_on_a_loaded_parent(self):
        """GREEN (cases 5, 10, 11): a loaded parent whose roots are the recorded ones, so each
        subtest isolates one other field."""
        cases = {
            "model": dict(model="someone/else"),
            "reasoningEffort": dict(effort="low"),
        }
        for number, (field, change) in enumerate(cases.items(), start=1):
            with self.subTest(field=field):
                self.host = Host()
                self.host.thread()
                self.host.load(roots=RECORDED)
                self.host.threads[PARENT].update(change)
                self.assert_withheld(self.relay_send(record(), number), 0, field=field)
        with self.subTest("networkAccess"):
            self.host = Host()
            self.host.thread(mode=WRITE, network=True)
            self.host.load(roots=RECORDED)
            self.assert_withheld(self.relay_send(record(mode=WRITE), 5), 0, field="sandbox")
        with self.subTest("writableRoots wider"):
            self.host = Host()
            self.host.thread(mode=WRITE)
            self.host.load(roots=RECORDED, writable=[RUN, EVIDENCE, RELAY_STATE, ELSEWHERE])
            self.assert_withheld(self.relay_send(record(mode=WRITE), 6), 0, field="sandbox")
        with self.subTest("writableRoots on a sandbox that has none"):
            self.host = Host()
            self.host.thread()
            self.host.load(roots=RECORDED)
            original = self.host._sandbox
            self.host._sandbox = lambda one: dict(original(one), writableRoots=[ELSEWHERE])
            self.assert_withheld(self.relay_send(record(), 7), 0, field="sandbox")
        with self.subTest("fewer writableRoots on a sandbox type that does not derive them"):
            # Only workspace-write derives writable roots from the runtime roots; any other type
            # carrying the key is compared exactly, even when the answer names fewer.
            self.host = Host()
            self.host.thread()
            self.host.load(roots=RECORDED)
            original = self.host._sandbox
            self.host._sandbox = lambda one: dict(original(one), writableRoots=[RUN])
            carrying = record(sandbox={"type": "dangerFullAccess", "writableRoots": [RUN, EVIDENCE]})
            self.assert_withheld(self.relay_send(carrying, 8), 0, field="sandbox")

    def test_another_setting_still_refuses_a_parent_whose_roots_narrowed(self):
        """RED for the field only: after the change the narrowed roots are accepted and the model
        is what refuses; before it, the roots were refused first."""
        self.host.thread()
        self.host.load()
        self.host.threads[PARENT]["model"] = "someone/else"
        self.assert_withheld(self.relay_send(record()), 0, field="model")


if __name__ == "__main__":
    unittest.main()
