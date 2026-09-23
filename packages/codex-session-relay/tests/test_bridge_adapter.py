"""The real bridge adapter logic, driven through an injected RPC surface.

No socket, no live host. Every response shape here is the one the App Server schemas define, and
the archive case reproduces the real observation that started this: a task whose cwd filter misses
it while an unfiltered listing returns the exact same id.
"""

import unittest
import json

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.hostadapter import HostUnavailable
from codex_session_relay.settings import TaskSettings

from .support import DeliveryTestCase, RelayTestCase


def capture_shutdown_cancellation(tmp):
    """Run a real send, stall it at its first RPC, then close() with a drain too short.

    Returns whatever the transport actually hands a caller whose work `_shut_down` had to
    cancel. Nothing here stands in for anything: it is the real BridgeHostAdapter, its real
    worker thread and its real shutdown path, with the stall injected at the RPC boundary
    through the `_build` factory seam.
    """
    import asyncio
    import threading
    from pathlib import Path

    from codex_thread_bridge.ledger import Ledger

    entered = threading.Event()
    gates = {}

    class Stalling:
        socket_path = None
        info = {}

        async def call(self, method, params):
            entered.set()
            await gates.setdefault("held", asyncio.Event()).wait()
            raise AssertionError("the stall was released, which this never does")

        async def close(self):
            return None

    socket = Path(tmp) / "cancel-socket"
    built = BridgeHostAdapter(
        str(socket), timeout=0.25, caller_slack=0.75, drain_seconds=0.05,
        app_server_factory=lambda canonical: Stalling(),
        ledger_factory=lambda: (socket, Ledger(Path(tmp) / "cancel-operations.sqlite3")),
    )
    outcome = {}

    def run():
        try:
            outcome["receipt"] = built.send_message(
                "req-cancelled", "thread-a", "hello", AUTHORIZED,
            )
        except BaseException as error:  # noqa: BLE001 - catching it is the whole point
            outcome["error"] = error

    caller = threading.Thread(target=run, daemon=True)
    caller.start()
    if not entered.wait(5):
        raise AssertionError("the send never reached the RPC boundary")
    built.close()
    caller.join(timeout=10)
    return outcome
# Shaped after the real correction receipt: a single local environment, workspaceWrite with
# networkAccess false, approvals never, Opus 5 at xhigh. The original dispatch receipt stays in
# the maintainer's private task record; the path below is synthetic and only has to be absolute.
WORKTREE = "/workspace/example/codex-session-relay/relay-core"
AUTHORIZED_POLICY = {
    "type": "workspaceWrite", "writableRoots": [], "networkAccess": False,
    "excludeTmpdirEnvVar": False, "excludeSlashTmp": False,
}
AUTHORIZED_ENVIRONMENTS = [
    {"environmentId": "local", "cwd": WORKTREE, "runtimeWorkspaceRoots": [WORKTREE]},
]
AUTHORIZED = TaskSettings({
    "sandbox": AUTHORIZED_POLICY,
    "approvalPolicy": "never",
    "cwd": WORKTREE,
    "runtimeWorkspaceRoots": [WORKTREE],
    "model": "anthropic/claude-opus-5",
    "reasoningEffort": "xhigh",
    "environments": AUTHORIZED_ENVIRONMENTS,
})


def authorized_resume_response(**overrides):
    """The real ThreadResumeResponse shape. environments live under thread, not at the top."""
    response = {
        "approvalPolicy": "never",
        "sandbox": dict(AUTHORIZED_POLICY),
        "cwd": WORKTREE,
        "runtimeWorkspaceRoots": [WORKTREE],
        "model": "anthropic/claude-opus-5",
        "reasoningEffort": "xhigh",
        "activePermissionProfile": None,
        "thread": {"id": "thread-1", "environments": [dict(e) for e in AUTHORIZED_ENVIRONMENTS]},
    }
    thread_overrides = overrides.pop("thread", None)
    response.update(overrides)
    if thread_overrides is not None:
        response["thread"] = {**response["thread"], **thread_overrides}
    return response

THREAD = "01child-task"


class FakeRpc:
    """Records every call so a test can assert what was actually asked of the host."""

    def __init__(self, responses):
        self.responses = responses
        self.calls = []

    def __call__(self, method, params):
        self.calls.append((method, params))
        handler = self.responses.get(method)
        if handler is None:
            raise AssertionError(f"unexpected call {method}")
        return handler(params) if callable(handler) else handler


class Pagination(unittest.TestCase):
    def _items(self, pages):
        def handler(params):
            cursor = params.get("cursor")
            index = 0 if cursor is None else int(cursor)
            return pages[index]

        return FakeRpc({"thread/items/list": handler})

    def test_a_token_beyond_the_first_page_is_found_with_the_forward_cursor(self):
        pages = [
            {"data": [{"turnId": "t1", "item": {"id": "i1", "text": "noise"}}],
             "nextCursor": "1", "backwardsCursor": "back-0"},
            {"data": [{"turnId": "t2", "item": {"id": "i2", "text": "carries del-abc-a1 here"}}],
             "nextCursor": None, "backwardsCursor": "back-1"},
        ]
        rpc = self._items(pages)
        adapter = BridgeHostAdapter(call=rpc, page=1)
        scan = adapter.find_token(THREAD, "del-abc-a1", limit=10)
        self.assertTrue(scan.found)
        self.assertEqual(scan.turn_id, "t2")
        # The forward cursor continues the scan; the reverse one is never used to continue.
        cursors = [params.get("cursor") for _method, params in rpc.calls]
        self.assertEqual(cursors, [None, "1"])
        self.assertNotIn("back-0", cursors)

    def test_a_bounded_scan_reports_that_it_did_not_exhaust_the_history(self):
        pages = [
            {"data": [{"turnId": "t1", "item": {"id": "i1", "text": "noise"}}], "nextCursor": "1"},
            {"data": [{"turnId": "t2", "item": {"id": "i2", "text": "more noise"}}],
             "nextCursor": "2"},
        ]
        adapter = BridgeHostAdapter(call=self._items(pages), page=1)
        scan = adapter.find_token(THREAD, "del-abc-a1", limit=2)
        self.assertFalse(scan.found)
        self.assertFalse(scan.exhausted, "a bounded stop is not proof of absence")

    def test_an_exhausted_scan_says_so(self):
        pages = [{"data": [{"turnId": "t1", "item": {"id": "i1", "text": "noise"}}],
                  "nextCursor": None}]
        adapter = BridgeHostAdapter(call=self._items(pages), page=5)
        scan = adapter.find_token(THREAD, "missing", limit=10)
        self.assertFalse(scan.found)
        self.assertTrue(scan.exhausted)

    def test_the_fingerprint_changes_when_content_is_appended_to_an_existing_item(self):
        state = {"text": "original"}
        rpc = FakeRpc({
            "thread/items/list": lambda params: {
                "data": [{"turnId": "t1", "item": {"id": "i1", "text": state["text"]}}],
                "nextCursor": None,
            }
        })
        adapter = BridgeHostAdapter(call=rpc)
        before = adapter.recipient_fingerprint(THREAD)
        state["text"] = "original plus a delivery token"
        self.assertNotEqual(before, adapter.recipient_fingerprint(THREAD))


class TurnLookup(unittest.TestCase):
    def test_a_bounded_miss_raises_rather_than_claiming_absence(self):
        rpc = FakeRpc({
            "thread/turns/list": lambda params: {"data": [{"id": "other", "status": "completed"}],
                                                 "nextCursor": "keep-going"}
        })
        adapter = BridgeHostAdapter(call=rpc, page=1)
        with self.assertRaises(HostUnavailable):
            adapter.read_turn(THREAD, "wanted")

    def test_an_exhausted_miss_returns_none(self):
        rpc = FakeRpc({
            "thread/turns/list": lambda params: {"data": [{"id": "other", "status": "completed"}],
                                                 "nextCursor": None}
        })
        adapter = BridgeHostAdapter(call=rpc, page=5)
        self.assertIsNone(adapter.read_turn(THREAD, "wanted"))

    def test_a_found_turn_carries_its_status_and_start(self):
        rpc = FakeRpc({
            "thread/turns/list": lambda params: {
                "data": [{"id": "wanted", "status": "completed", "startedAt": 1789420929}],
                "nextCursor": None,
            }
        })
        turn = BridgeHostAdapter(call=rpc).read_turn(THREAD, "wanted")
        self.assertEqual((turn.turn_id, turn.status, turn.started_at),
                         ("wanted", "completed", 1789420929))


class ThreadAndGoalReads(unittest.TestCase):
    def test_thread_facts_carry_the_direct_input_capability(self):
        rpc = FakeRpc({"thread/read": {"thread": {"status": {"type": "idle"},
                                                  "canAcceptDirectInput": False}}})
        facts = BridgeHostAdapter(call=rpc).read_thread(THREAD)
        self.assertEqual(facts.runtime_status, "idle")
        self.assertIs(facts.can_accept_input, False)

    def test_an_absent_goal_is_not_a_paused_goal(self):
        rpc = FakeRpc({"thread/goal/get": {"goal": None}})
        self.assertIsNone(BridgeHostAdapter(call=rpc).read_goal_status(THREAD))

    def test_a_goal_status_is_returned_verbatim(self):
        rpc = FakeRpc({"thread/goal/get": {"goal": {"status": "budgetLimited"}}})
        self.assertEqual(BridgeHostAdapter(call=rpc).read_goal_status(THREAD), "budgetLimited")

    def test_an_unknown_operation_id_is_an_absence_not_an_error(self):
        def raises(_request_id):
            raise ValueError("Unknown request_id")

        adapter = BridgeHostAdapter(call=FakeRpc({}), ledger_get=raises)
        self.assertIsNone(adapter.get_operation("del-aaaaaaaaaaaa-a1"))


class ArchiveDiscovery(RelayTestCase):
    """The real observation: a cwd-filtered listing misses a task an unfiltered one returns."""

    def _rpc(self, *, cwd_rows=(), unarchived_rows=(), archived_rows=()):
        def handler(params):
            if params.get("archived") is True:
                return {"data": list(archived_rows), "nextCursor": None}
            if params.get("cwd"):
                return {"data": list(cwd_rows), "nextCursor": None}
            return {"data": list(unarchived_rows), "nextCursor": None}

        return FakeRpc({"thread/list": handler})

    def _adapter(self, rpc):
        return BridgeHostAdapter(call=rpc, store=self.store, clock=self.clock)

    def test_a_cwd_filter_miss_falls_back_and_finds_the_task_unarchived(self):
        rpc = self._rpc(cwd_rows=(), unarchived_rows=({"id": THREAD},))
        self.assertIs(self._adapter(rpc).is_archived(THREAD, cwd="/corrected/cwd"), False)

    def test_an_archived_task_is_reported_archived(self):
        rpc = self._rpc(archived_rows=({"id": THREAD},))
        self.assertIs(self._adapter(rpc).is_archived(THREAD, cwd="/x"), True)

    def test_a_task_in_no_listing_is_unknown_rather_than_archived(self):
        rpc = self._rpc()
        self.assertIsNone(self._adapter(rpc).is_archived(THREAD, cwd="/x"))

    def test_every_listing_avoids_repairing_host_metadata(self):
        rpc = self._rpc(unarchived_rows=({"id": THREAD},))
        self._adapter(rpc).is_archived(THREAD, cwd="/x")
        for method, params in rpc.calls:
            self.assertEqual(method, "thread/list")
            self.assertIs(params.get("useStateDbOnly"), True)

    def test_discovery_makes_progress_across_checks(self):
        pages = {}

        def handler(params):
            if params.get("archived") is True or params.get("cwd"):
                return {"data": [], "nextCursor": None}
            cursor = params.get("cursor")
            index = 0 if cursor is None else int(cursor)
            pages.setdefault("seen", []).append(index)
            if index >= 6:
                return {"data": [{"id": THREAD}], "nextCursor": None}
            return {"data": [{"id": f"other-{index}"}], "nextCursor": str(index + 1)}

        rpc = FakeRpc({"thread/list": handler})
        adapter = BridgeHostAdapter(call=rpc, store=self.store, clock=self.clock, page=1)
        first = adapter.is_archived(THREAD, cwd=None)
        self.assertIsNone(first, "the first bounded pass has not reached it yet")
        stored = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE task_id = ? AND listing = ?",
            (THREAD, "unarchived_all"),
        )
        self.assertIsNotNone(stored["cursor"], "the pass recorded where it stopped")
        # The next pass resumes rather than re-reading the same prefix.
        self.assertIs(adapter.is_archived(THREAD, cwd=None), False)
        self.assertGreater(max(pages["seen"]), 3)


if __name__ == "__main__":
    unittest.main()


class RealTransportSeam(unittest.TestCase):
    """The thread seam the injected-call tests cannot reach.

    Uses the REAL pinned Bridge and the REAL sqlite Ledger with a fake RPC endpoint, so no
    socket is opened and no task is messaged. The failure this covers happened before any RPC
    went out: a ledger built on the caller's thread and used on the loop thread.
    """

    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        import shutil
        import tempfile

        self.tmp = tempfile.mkdtemp(prefix="relay-transport-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def _adapter(self):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        calls = []

        class FakeAppServer:
            def __init__(self, socket_path, timeout=20):
                self.socket_path = socket_path
                self.info = {}

            async def call(self, method, params):
                calls.append(method)
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    return authorized_resume_response()
                if method == "turn/start":
                    return {"turn": {"id": "fake-turn-1"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        def ledger_factory():
            return Path(self.tmp) / "socket", Ledger(Path(self.tmp) / "operations.sqlite3")

        adapter = BridgeHostAdapter(
            str(Path(self.tmp) / "socket"),
            app_server_factory=lambda canonical: FakeAppServer(canonical),
            ledger_factory=ledger_factory,
            timeout=10,
        )
        return adapter, calls

    def test_a_send_crosses_the_thread_boundary_without_a_sqlite_error(self):
        adapter, calls = self._adapter()
        self.addCleanup(adapter.close)
        receipt = adapter.send_message(
            "del-aaaaaaaaaaaa-a1", "thread-1", "hello", AUTHORIZED,
        )
        self.assertEqual(receipt["status"], "accepted")
        self.assertEqual(receipt["turnId"], "fake-turn-1")
        self.assertEqual(calls, ["thread/read", "thread/resume", "turn/start"])

    def test_the_ledger_is_readable_from_the_caller_thread_too(self):
        adapter, _calls = self._adapter()
        self.addCleanup(adapter.close)
        adapter.send_message("del-bbbbbbbbbbbb-a1", "thread-1", "hello", AUTHORIZED)
        receipt = adapter.get_operation("del-bbbbbbbbbbbb-a1")
        self.assertEqual(receipt["status"], "accepted")
        self.assertIsNone(adapter.get_operation("del-cccccccccccc-a9"))

    def test_close_shuts_the_loop_and_thread_down(self):
        adapter, _calls = self._adapter()
        thread = adapter._transport.thread
        adapter.close()
        self.assertFalse(thread.is_alive())
        adapter.close()  # idempotent


class GuardedSettingsSeam(unittest.TestCase):
    """The no-widened-start guarantee, proved at the REAL adapter.

    FakeHostAdapter implements delivery itself and never calls BridgeHostAdapter, so a test
    written against it could pass while this adapter still issued an unguarded start. These use
    the real pinned Ledger and the real guarded send with a fake RPC endpoint: no socket is
    opened and no task is messaged.

    The behaviour under test was observed on this host: a resume carrying no overrides returned
    sandbox dangerFullAccess for a task created workspaceWrite with networkAccess false.
    """

    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        import shutil
        import tempfile

        self.tmp = tempfile.mkdtemp(prefix="relay-guard-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def _adapter(self, resume=None, status="idle"):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        calls = []
        resume_response = resume if resume is not None else authorized_resume_response()

        class FakeAppServer:
            def __init__(self, socket_path, timeout=20):
                self.socket_path = socket_path
                self.info = {}

            async def call(self, method, params):
                calls.append((method, params))
                if method == "thread/read":
                    return {"thread": {"status": {"type": status}}}
                if method == "thread/resume":
                    return resume_response
                if method == "turn/start":
                    return {"turn": {"id": "fake-turn-1"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        self.ledger_path = Path(self.tmp) / "operations.sqlite3"

        def ledger_factory():
            return Path(self.tmp) / "socket", Ledger(self.ledger_path)

        adapter = BridgeHostAdapter(
            str(Path(self.tmp) / "socket"),
            app_server_factory=lambda canonical: FakeAppServer(canonical),
            ledger_factory=ledger_factory,
            timeout=10,
        )
        self.addCleanup(adapter.close)
        return adapter, calls

    @staticmethod
    def _methods(calls):
        return [method for method, _params in calls]

    def _assert_no_start(self, receipt, calls, code):
        from codex_session_relay.transport import WITHHELD_PRE_SEND, classify_operation_receipt

        self.assertNotIn("turn/start", self._methods(calls), "a turn was started anyway")
        self.assertIn(receipt["status"], ("failed", "not_attempted", "outcome_unknown"))
        if receipt["status"] != "failed":
            raise AssertionError(json.dumps({k: receipt.get(k) for k in ("status", "error", "threadId", "settings", "rpcError")}, default=str)[:2000])
        self.assertEqual(receipt["rpcError"]["code"], code)
        self.assertIsNone(receipt.get("turnId"))
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, WITHHELD_PRE_SEND)
        self.assertEqual(facts.send_attempted, "no")
        self.assertTrue(facts.retry_safe)
        self.assertEqual(facts.failed_operation, "thread/resume")

    # ------------------------------------------------------- the ordinary path

    def test_the_start_binds_nothing_it_could_never_read_back(self):
        """This asserts the opposite of what it used to, and the reversal is the point.

        It previously proved the turn carried all seven authorized settings. But
        TurnStartResponse defines only `turn`, so nothing bound there can be read back, and a
        receipt reporting accepted off a turn ID alone was calling an unverifiable binding a
        success. The resume above already establishes that the thread IS in the authorized
        state, which makes the overrides redundant rather than protective. Sending them also had
        a side effect worth losing: TurnStartParams says a model override persists into
        subsequent turns, so delivering one message quietly rewrote the thread for every later
        turn.
        """
        adapter, calls = self._adapter()
        receipt = adapter.send_message("del-100000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertEqual(receipt["status"], "accepted")
        self.assertEqual(self._methods(calls), ["thread/read", "thread/resume", "turn/start"])
        start = dict(calls[-1][1])
        self.assertEqual(set(start), {"threadId", "input"})
        self.assertEqual(start["input"], [{"type": "text", "text": "hi"}])

    def test_the_resume_carries_the_settings_it_can_express(self):
        adapter, calls = self._adapter()
        adapter.send_message("del-100000000000-a2", "thread-1", "hi", AUTHORIZED)
        resume = dict(calls[1][1])
        self.assertEqual(resume["sandbox"], "workspace-write")
        self.assertEqual(resume["approvalPolicy"], "never")
        self.assertEqual(resume["cwd"], WORKTREE)
        self.assertEqual(resume["runtimeWorkspaceRoots"], [WORKTREE])
        self.assertEqual(resume["model"], "anthropic/claude-opus-5")
        self.assertTrue(resume["excludeTurns"])
        # ThreadResumeParams has no effort field and its sandbox is only a MODE; the schema does
        # accept a free-form config, so the effort AND the policy detail the mode cannot carry
        # both travel there, under the same keys the bridge uses. Default-valued fields are sent
        # too: host configuration may set the opposite, and only a transmitted value overrides it.
        self.assertEqual(
            resume["config"],
            {
                "model_reasoning_effort": "xhigh",
                "sandbox_workspace_write": {
                    "writable_roots": [],
                    "network_access": False,
                    "exclude_tmpdir_env_var": False,
                    "exclude_slash_tmp": False,
                },
            },
        )
        self.assertNotIn("environments", resume)

    def test_a_setting_the_host_never_reported_withholds_and_is_not_a_mismatch(self):
        """Absence and difference are different facts, and they need different answers.

        A host that reported nothing has said nothing about whether the setting was applied.
        Calling that settings_not_preserved would assert it reported something else.
        """
        from codex_session_relay.settings import SETTING_UNOBSERVABLE

        fields = ("model", "reasoningEffort", "cwd", "runtimeWorkspaceRoots", "sandbox")
        for index, field in enumerate(fields):
            with self.subTest(field=field):
                adapter, calls = self._adapter(
                    resume=authorized_resume_response(**{field: None})
                )
                # A distinct id per case: the ledger answers a reused one from its receipt, so
                # a collision here would silently replay the previous field's result.
                receipt = adapter.send_message(
                    f"del-20000000000{index}-a1", "thread-1", "hi", AUTHORIZED
                )
                self._assert_no_start(receipt, calls, SETTING_UNOBSERVABLE)
                self.assertEqual(receipt["settingsFindings"][0]["field"], field)

    def test_an_absent_approval_policy_withholds_rather_than_closing_the_channel(self):
        """Not seeing a policy is not seeing an interactive one.

        unsupported_approval_policy means the push channel is permanently closed and the delivery
        goes inbox_only. An absent policy proves no such thing, so it withholds and stays eligible
        for a bounded pre-send retry.
        """
        from codex_session_relay.settings import SETTING_UNOBSERVABLE

        adapter, calls = self._adapter(resume=authorized_resume_response(approvalPolicy=None))
        receipt = adapter.send_message("del-300000000000-a1", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, SETTING_UNOBSERVABLE)
        self.assertEqual(receipt["settingsFindings"][0]["field"], "approvalPolicy")

    def test_omitted_null_and_empty_roots_are_three_different_answers(self):
        """Only an explicit empty list may agree with an expected empty list.

        The previous comparison read the roots as `list(response.get(...) or [])`, so an omitted
        or null list became [] and compared EQUAL to an expected empty list. A send then went out
        on a settings answer the host never gave.
        """
        from codex_session_relay.settings import SETTING_UNOBSERVABLE, TaskSettings

        expecting_empty = TaskSettings({**AUTHORIZED.data, "runtimeWorkspaceRoots": []})
        omitted = authorized_resume_response()
        del omitted["runtimeWorkspaceRoots"]
        explicit_null = authorized_resume_response(runtimeWorkspaceRoots=None)
        explicitly_empty = authorized_resume_response(runtimeWorkspaceRoots=[])

        for label, response in (("omitted", omitted), ("null", explicit_null)):
            with self.subTest(label):
                findings = expecting_empty.mismatches(response)
                self.assertTrue(findings, f"an {label} list must not read as agreement")
                self.assertEqual(findings[0]["code"], SETTING_UNOBSERVABLE)
                self.assertEqual(findings[0]["field"], "runtimeWorkspaceRoots")

        self.assertEqual(
            expecting_empty.mismatches(explicitly_empty), [],
            "an explicit empty list IS the answer that was asked for",
        )

    def test_retained_model_effort_cwd_and_roots_are_all_verified(self):
        for field, response in (
            ("model", authorized_resume_response(model="something-else")),
            ("reasoningEffort", authorized_resume_response(reasoningEffort="low")),
            ("cwd", authorized_resume_response(cwd="/somewhere/else")),
            ("runtimeWorkspaceRoots",
             authorized_resume_response(runtimeWorkspaceRoots=["/somewhere/else"])),
        ):
            with self.subTest(field=field):
                adapter, calls = self._adapter(resume=response)
                receipt = adapter.send_message(
                    f"del-2000000000{len(field):02d}-a1", "thread-1", "hi", AUTHORIZED,
                )
                self._assert_no_start(receipt, calls, "settings_not_preserved")

    # ------------------------------------------- the observed widening, refused

    def test_a_host_that_ignores_overrides_starts_no_widened_turn(self):
        """The exact observed boundary: we asked for workspace-write, it answered danger."""
        adapter, calls = self._adapter(
            resume=authorized_resume_response(sandbox={"type": "dangerFullAccess"}),
        )
        receipt = adapter.send_message("del-300000000000-a1", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, "settings_not_preserved")
        self.assertEqual(self._methods(calls), ["thread/read", "thread/resume"])

    def test_the_same_mode_with_network_widened_is_still_refused(self):
        widened = {**AUTHORIZED_POLICY, "networkAccess": True}
        adapter, calls = self._adapter(resume=authorized_resume_response(sandbox=widened))
        receipt = adapter.send_message("del-300000000000-a2", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, "settings_not_preserved")

    def test_the_same_mode_with_writable_roots_widened_is_still_refused(self):
        widened = {**AUTHORIZED_POLICY, "writableRoots": ["/"]}
        adapter, calls = self._adapter(resume=authorized_resume_response(sandbox=widened))
        receipt = adapter.send_message("del-300000000000-a3", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, "settings_not_preserved")

    # ------------------------------------------------------------ environments

    def test_an_unknown_environment_selection_withholds(self):
        adapter, calls = self._adapter(
            resume=authorized_resume_response(thread={"environments": None}),
        )
        receipt = adapter.send_message("del-400000000000-a1", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, "environments_unknown")

    def test_an_empty_environment_selection_is_not_the_authorized_one(self):
        adapter, calls = self._adapter(
            resume=authorized_resume_response(thread={"environments": []}),
        )
        receipt = adapter.send_message("del-400000000000-a2", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, "settings_not_preserved")

    def test_a_different_environment_withholds(self):
        other = [{"environmentId": "remote", "cwd": WORKTREE, "runtimeWorkspaceRoots": [WORKTREE]}]
        adapter, calls = self._adapter(
            resume=authorized_resume_response(thread={"environments": other}),
        )
        receipt = adapter.send_message("del-400000000000-a3", "thread-1", "hi", AUTHORIZED)
        self._assert_no_start(receipt, calls, "settings_not_preserved")

    # --------------------------------------------------------- approval policy

    def test_a_non_never_approval_policy_is_inbox_only_not_a_settings_mismatch(self):
        from codex_session_relay.transport import INBOX_ONLY, classify_operation_receipt

        adapter, calls = self._adapter(
            resume=authorized_resume_response(approvalPolicy="on-request"),
        )
        receipt = adapter.send_message("del-500000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertNotIn("turn/start", self._methods(calls))
        self.assertEqual(receipt["rpcError"]["code"], "unsupported_approval_policy")
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, INBOX_ONLY)
        self.assertFalse(facts.retry_safe, "a closed push channel is not a retry loop")

    def test_a_granular_approval_object_is_labelled_not_copied(self):
        """The frozen record types this field as string or null; a granular policy is an object."""
        granular = {"granular": {"mcp_elicitations": True, "rules": True,
                                 "sandbox_approval": True}}
        adapter, calls = self._adapter(
            resume=authorized_resume_response(approvalPolicy=granular),
        )
        receipt = adapter.send_message("del-500000000000-a2", "thread-1", "hi", AUTHORIZED)
        self.assertNotIn("turn/start", self._methods(calls))
        self.assertEqual(receipt["rpcError"]["code"], "unsupported_approval_policy")
        findings = AUTHORIZED.mismatches(authorized_resume_response(approvalPolicy=granular))
        self.assertEqual(findings[0]["returned"], "granular")
        self.assertIsInstance(findings[0]["returned"], str)

    # ------------------------------------------------------------ idempotency

    def test_a_settled_request_replays_without_another_rpc(self):
        adapter, calls = self._adapter()
        first = adapter.send_message("del-600000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertEqual(first["status"], "accepted")
        before = len(calls)
        again = adapter.send_message("del-600000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertTrue(again["replayed"])
        self.assertEqual(again["turnId"], first["turnId"])
        self.assertEqual(len(calls), before, "a replay must not reach the host again")

    def test_an_unfinished_request_replays_without_another_rpc(self):
        """An in-flight receipt is not permission to send again."""
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        adapter, calls = self._adapter()
        # Force the ledger into the in_progress_or_unknown state the real transport leaves
        # behind when it dies mid-send.
        adapter.send_message("del-700000000000-a0", "thread-1", "warm", AUTHORIZED)
        side = Ledger(Path(self.tmp) / "operations.sqlite3")
        try:
            fresh, _existing = side.begin(
                "del-700000000000-a1", "send_message_to_thread",
                {"threadId": "thread-1", "message": "hi"},
            )
            self.assertTrue(fresh)
        finally:
            side.close()
        before = len(calls)
        replayed = adapter.send_message("del-700000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertEqual(replayed["status"], "in_progress_or_unknown")
        self.assertTrue(replayed["replayed"])
        self.assertEqual(len(calls), before, "an unfinished request must not be resent")

    def test_a_reused_request_id_with_different_arguments_is_rejected(self):
        adapter, calls = self._adapter()
        adapter.send_message("del-800000000000-a1", "thread-1", "hi", AUTHORIZED)
        before = len(calls)
        with self.assertRaises(ValueError):
            adapter.send_message("del-800000000000-a1", "thread-1", "DIFFERENT", AUTHORIZED)
        with self.assertRaises(ValueError):
            adapter.send_message("del-800000000000-a1", "other-thread", "hi", AUTHORIZED)
        self.assertEqual(len(calls), before)

    def test_the_worker_survives_a_caller_clearing_the_frames_it_was_handed(self):
        """A failure crosses a thread boundary here, and the caller owns what it catches.

        `unittest.assertRaises` clears the frames of the exception it captures, which is
        why the test above is where this first showed up. While the transport handed over
        a traceback that still began at its own suspended worker frame, that clear
        finalized the worker on CPython 3.11, and every later submit waited out its full
        timeout against a loop that no longer read its inbox. Written against
        `traceback.clear_frames` directly, because that is the actual mechanism rather
        than an incidental detail of one assertion helper.
        """
        import traceback

        adapter, _ = self._adapter()
        adapter.send_message("del-b00000000000-a1", "thread-1", "hi", AUTHORIZED)
        try:
            adapter.send_message("del-b00000000000-a1", "thread-1", "DIFFERENT", AUTHORIZED)
        except ValueError as error:
            traceback.clear_frames(error.__traceback__)
        else:
            self.fail("a reused request id with different arguments must be rejected")
        self.assertEqual(adapter.read_thread("thread-1").runtime_status, "idle")

    def test_a_failure_that_fights_back_is_still_delivered(self):
        """The handler must not execute anything the failure brought with it.

        `with_traceback` is an ordinary method and a subclass may override it, so
        calling it through the instance would run that override inside the one handler
        that must not fail. The built-in is called unbound instead, which trims the
        frame without giving the exception a say.

        Written without `assertRaises`, which calls `with_traceback` on what it catches
        and would therefore trigger the override itself and prove nothing about the
        transport.
        """
        import traceback
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        class Hostile(RuntimeError):
            def with_traceback(self, tb):
                raise AssertionError("the transport must not run this")

        class RefusingAppServer:
            def __init__(self, socket_path, timeout=20):
                self.socket_path = socket_path
                self.info = {}

            async def call(self, method, params):
                raise Hostile("refused")

            async def close(self):
                return None

        adapter = BridgeHostAdapter(
            str(Path(self.tmp) / "hostile-socket"),
            app_server_factory=lambda canonical: RefusingAppServer(canonical),
            ledger_factory=lambda: (
                Path(self.tmp) / "hostile-socket",
                Ledger(Path(self.tmp) / "hostile.sqlite3"),
            ),
            timeout=3,
        )
        self.addCleanup(adapter.close)
        try:
            adapter.read_thread("thread-1")
        except Hostile as error:
            # Exactly what a caller is allowed to do with what it caught.
            traceback.clear_frames(error.__traceback__)
        else:
            self.fail("the refusal must reach the caller")
        # A worker taken down by the override or by that clear surfaces here as a
        # TimeoutError rather than the refusal.
        try:
            adapter.read_thread("thread-1")
        except Hostile:
            pass
        else:
            self.fail("the refusal must reach the caller a second time")

    # ------------------------------------------------------------ busy, unchanged

    def test_an_active_recipient_is_still_left_alone_before_any_resume(self):
        adapter, calls = self._adapter(status="active")
        receipt = adapter.send_message("del-900000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertEqual(self._methods(calls), ["thread/read"])
        self.assertEqual(receipt["rpcError"]["code"], "thread_busy")

    def test_a_send_without_settings_is_refused_rather_than_defaulted(self):
        adapter, calls = self._adapter()
        with self.assertRaises(HostUnavailable):
            adapter.send_message("del-a00000000000-a1", "thread-1", "hi")
        self.assertEqual(calls, [], "nothing may reach the host without authorized settings")


class MirrorMatchesTheBridge(unittest.TestCase):
    """The two implementations of one contract, pinned to agree where they must.

    The relay deliberately mirrors the bridge's send rather than calling it, so nothing but a test
    stops the two drifting. Constant equality is not enough on its own: the FIRST finding becomes
    the receipt's error code, so a shared vocabulary with a different order still makes the two
    sides describe one identical host answer with two different codes.
    """

    def setUp(self):
        try:
            import codex_thread_bridge.settings as bridge_settings
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        self.bridge = bridge_settings

    def test_the_shared_tables_are_identical(self):
        from codex_session_relay import settings as relay

        self.assertEqual(self.bridge.SANDBOX_MODES, relay.RESUME_SANDBOX_MODE)
        self.assertEqual(self.bridge.POLICY_DEFAULTS, relay.POLICY_DEFAULTS)
        self.assertEqual(self.bridge.POLICY_CONFIG_KEYS, relay.POLICY_CONFIG_KEYS)
        self.assertEqual(self.bridge.FIELD_PRECEDENCE, relay.FIELD_PRECEDENCE)
        self.assertEqual(self.bridge.SETTINGS_NOT_PRESERVED, relay.SETTINGS_NOT_PRESERVED)
        self.assertEqual(self.bridge.SETTING_UNOBSERVABLE, relay.SETTING_UNOBSERVABLE)
        self.assertEqual(
            self.bridge.UNSUPPORTED_APPROVAL_POLICY, relay.UNSUPPORTED_APPROVAL_POLICY
        )

    def _bridge_contract(self):
        return self.bridge.SettingsContract(
            cwd=WORKTREE,
            expected_sandbox_policy=dict(AUTHORIZED_POLICY),
            model="anthropic/claude-opus-5",
            reasoning_effort="xhigh",
            runtime_workspace_roots=[WORKTREE],
        )

    def test_both_sides_name_the_same_first_cause_for_a_mixed_answer(self):
        """The counterexample an audit found: model omitted AND sandbox widened.

        With only the codes shared and not the order, the relay reported settings_not_preserved
        for the widened sandbox while the bridge reported setting_unobservable for the absent
        model, for one identical response.
        """
        widened = dict(AUTHORIZED_POLICY, type="dangerFullAccess")
        cases = {
            "model absent and sandbox widened": authorized_resume_response(
                model=None, sandbox={"type": "dangerFullAccess"}
            ),
            "effort absent and cwd different": authorized_resume_response(
                reasoningEffort=None, cwd="/somewhere/else"
            ),
            "roots absent and model different": authorized_resume_response(
                runtimeWorkspaceRoots=None, model="gpt-6-astra"
            ),
            "sandbox absent and effort different": authorized_resume_response(
                sandbox=None, reasoningEffort="low"
            ),
            "everything reported and only the sandbox widened": authorized_resume_response(
                sandbox=widened
            ),
            "several absent at once": authorized_resume_response(
                model=None, reasoningEffort=None, cwd=None
            ),
        }
        contract = self._bridge_contract()
        for label, response in cases.items():
            with self.subTest(label):
                mine = AUTHORIZED.mismatches(response)
                theirs = contract.findings(response)
                self.assertTrue(mine and theirs, "both sides must find something")
                self.assertEqual(
                    (mine[0]["code"], mine[0]["field"]),
                    (theirs[0]["code"], theirs[0]["field"]),
                    f"{label}: relay said {mine[0]['code']} on {mine[0]['field']}, "
                    f"bridge said {theirs[0]['code']} on {theirs[0]['field']}",
                )

    def test_a_clean_answer_satisfies_both_sides(self):
        response = authorized_resume_response()
        self.assertEqual(AUTHORIZED.mismatches(response), [])
        self.assertEqual(self._bridge_contract().findings(response), [])


    def test_neither_side_raises_on_a_policy_it_cannot_read(self):
        """Both run before turn/start, so an exception would become outcome_unknown.

        That verdict tells a caller the message may have been delivered, and it is the one
        outcome a delivery cannot reconcile, for a response that in fact withheld everything.
        """
        from codex_session_relay import settings as relay

        unreadable = (
            None, "workspaceWrite", 42, ["workspaceWrite"], {"no_type": 1},
            {"type": {"unhashable": 1}}, {"type": ["not-a-string"]},
            {"type": "workspaceWrite", "writableRoots": None},
            {"type": "workspaceWrite", "writableRoots": 7},
        )
        for policy in unreadable:
            with self.subTest(policy=repr(policy)):
                self.assertIsNone(self.bridge.normalise_policy(policy))
                self.assertIsNone(relay.normalise_policy(policy))
                findings = AUTHORIZED.mismatches(authorized_resume_response(sandbox=policy))
                self.assertTrue(findings, "an unreadable policy must refuse")
                self.assertEqual(findings[0]["field"], "sandbox")
        # A well-formed policy still normalises identically on both sides.
        self.assertEqual(
            self.bridge.normalise_policy({"type": "workspaceWrite"}),
            relay.normalise_policy({"type": "workspaceWrite"}),
        )

    def test_the_resume_config_agrees_on_every_key(self):
        relay_config = AUTHORIZED.resume_params("thread-1")["config"]
        self.assertEqual(relay_config, self._bridge_contract().config())


class TransportIsolation(RelayTestCase):
    """One recipient must not be able to hold the transport against another.

    Criterion 8 asks for bounded connection wait, retry and error isolation. The scheduler
    half shipped in #9; this is the transport half. Everything here drives the REAL
    BridgeHostAdapter and its real worker thread, with the stall injected at the RPC boundary
    through _build's app_server_factory seam - fakehost.py cannot reach _Transport at all,
    because it is a separate implementation over its own in-memory state.
    """

    TIMEOUT = 0.25
    SLACK = 0.75

    def setUp(self):
        super().setUp()
        try:
            import codex_thread_bridge.ledger  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")

    def adapter(self, app_server, **options):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        socket = Path(self.tmp) / "socket"
        built = BridgeHostAdapter(
            str(socket),
            timeout=self.TIMEOUT,
            caller_slack=self.SLACK,
            app_server_factory=lambda canonical: app_server,
            ledger_factory=lambda: (socket, Ledger(Path(self.tmp) / "operations.sqlite3")),
            **options,
        )
        self.addCleanup(built.close)
        return built

    def barrier_server(self):
        """An App Server that answers normally, except for threads told to hold."""
        import asyncio
        import threading

        class Barrier:
            def __init__(self):
                self.socket_path = None
                self.info = {}
                self.entered = {}
                self.release = {}
                self.starts = []
                self.lock = threading.Lock()
                self.hold = set()
                self.closed = False

            async def call(self, method, params):
                thread_id = params.get("threadId")
                if thread_id in self.hold:
                    with self.lock:
                        event = self.entered.setdefault(thread_id, threading.Event())
                    event.set()
                    gate = self.release.setdefault(thread_id, asyncio.Event())
                    await gate.wait()
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    return authorized_resume_response(thread={"id": thread_id})
                if method == "turn/start":
                    with self.lock:
                        self.starts.append(params.get("threadId"))
                    return {"turn": {"id": f"turn-{len(self.starts)}"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                self.closed = True
                return None

        return Barrier()

    def send_in_background(self, built, request_id, thread_id):
        """Start a send on its own thread and hand back somewhere to read its outcome."""
        import threading

        outcome = {}

        def run():
            try:
                outcome["receipt"] = built.send_message(
                    request_id, thread_id, "hello", AUTHORIZED,
                )
            except BaseException as error:  # noqa: BLE001 - the test reads it
                outcome["error"] = error

        thread = threading.Thread(target=run, daemon=True)
        thread.start()
        return thread, outcome

    def test_a_stalled_recipient_does_not_hold_another_recipients_send(self):
        """The defect, and the assertion is causal rather than a stopwatch.

        B is asserted to complete WHILE A is still held at its RPC. Before this change the
        worker awaited one submission at a time, so B never even reached the RPC until A's
        chain ended - A's caller had already given up by then and released nothing.
        """
        server = self.barrier_server()
        server.hold.add("thread-a")
        built = self.adapter(server)

        thread, _outcome = self.send_in_background(built, "req-a", "thread-a")
        self.assertTrue(
            server.entered.setdefault("thread-a", __import__("threading").Event()).wait(5),
            "A never reached the RPC boundary",
        )

        receipt = built.send_message("req-b", "thread-b", "hello", AUTHORIZED)

        self.assertEqual(receipt["status"], "accepted", receipt)
        self.assertIn("thread-b", server.starts)
        self.assertNotIn(
            "thread-a", server.starts,
            "B was only served after A finished, which is the thing being fixed",
        )
        thread.join(timeout=10)

    def test_a_second_send_to_one_recipient_is_withheld_rather_than_started(self):
        """Two turns for one thread is the failure a bare concurrency change would cause.

        _guarded_send reads the thread, resumes it and starts a turn across separate awaits,
        so without a per-recipient bound both sends pass the idle check and both start. The
        second is withheld WITHOUT being sent, which is why it is reported as a busy recipient
        rather than an unknown outcome: it stays retry-safe.
        """
        server = self.barrier_server()
        server.hold.add("thread-a")
        built = self.adapter(server)

        thread, _outcome = self.send_in_background(built, "req-a1", "thread-a")
        self.assertTrue(
            server.entered.setdefault("thread-a", __import__("threading").Event()).wait(5),
        )

        second = built.send_message("req-a2", "thread-a", "hello", AUTHORIZED)

        self.assertEqual(second["status"], "failed", second)
        self.assertEqual(second["rpcError"]["code"], "thread_busy")
        self.assertEqual(server.starts, [], "a second turn was started for one thread")
        thread.join(timeout=10)

    def test_a_withheld_send_classifies_as_busy_and_retry_safe(self):
        """Nothing was sent, so the delivery layer must be able to say that precisely."""
        from codex_session_relay.transport import (
            DEFERRED_BUSY, assert_attempt_invariants, attempt_record,
            classify_operation_receipt,
        )

        server = self.barrier_server()
        server.hold.add("thread-a")
        built = self.adapter(server)
        thread, _outcome = self.send_in_background(built, "req-c1", "thread-a")
        self.assertTrue(
            server.entered.setdefault("thread-a", __import__("threading").Event()).wait(5),
        )

        facts = classify_operation_receipt(
            built.send_message("req-c2", "thread-a", "hello", AUTHORIZED)
        )

        self.assertEqual(facts.delivery_state, DEFERRED_BUSY)
        self.assertEqual(facts.send_attempted, "no")
        self.assertTrue(facts.retry_safe, "a send that never happened must stay retryable")
        # The bucket cannot carry the distinction and the schema will not let it: retrySafe is
        # pinned to thread/read or thread/resume, so a locally withheld send shares the busy
        # bucket with a host-reported one. What separates them is the error text, which is
        # exactly what the diagnostics criterion asks not to be hidden behind a generic state.
        self.assertEqual(facts.failed_operation, "thread/read")
        self.assertIn("this relay", facts.error_text)
        self.assertIn("without being sent", facts.error_text)
        # And the record it produces is one the frozen attempt schema accepts.
        assert_attempt_invariants(attempt_record(
            facts, request_id="req-c2", event_id="ev-c", attempt_no=1,
            recipient="01parent", status_before="unknown",
            observed_at="2026-09-17T00:00:00Z",
        ))
        thread.join(timeout=10)

    def test_the_worker_survives_an_abandoned_send_and_keeps_serving(self):
        """A's work outlives its caller. The worker must not be one of the casualties."""
        server = self.barrier_server()
        server.hold.add("thread-a")
        built = self.adapter(server)

        thread, outcome = self.send_in_background(built, "req-d1", "thread-a")
        thread.join(timeout=10)
        self.assertFalse(thread.is_alive(), "A's caller never gave up")
        self.assertIn("error", outcome, f"A was expected to be abandoned, got {outcome}")

        receipt = built.send_message("req-d2", "thread-b", "hello", AUTHORIZED)

        self.assertEqual(receipt["status"], "accepted", receipt)

    def test_closing_while_a_send_is_stalled_still_ends_the_worker(self):
        """Cancelled while the ledger is still open, so the send records its own outcome."""
        server = self.barrier_server()
        server.hold.add("thread-a")
        built = BridgeHostAdapter(
            str(__import__("pathlib").Path(self.tmp) / "socket"),
            timeout=self.TIMEOUT,
            caller_slack=self.SLACK,
            drain_seconds=0.2,
            app_server_factory=lambda canonical: server,
            ledger_factory=lambda: (
                __import__("pathlib").Path(self.tmp) / "socket",
                __import__("codex_thread_bridge.ledger", fromlist=["Ledger"]).Ledger(
                    __import__("pathlib").Path(self.tmp) / "operations.sqlite3"
                ),
            ),
        )
        thread, _outcome = self.send_in_background(built, "req-e1", "thread-a")
        self.assertTrue(
            server.entered.setdefault("thread-a", __import__("threading").Event()).wait(5),
        )

        worker = built._transport.thread
        built.close()

        self.assertFalse(
            worker.is_alive(),
            "the worker thread outlived close() with work still in flight",
        )
        thread.join(timeout=10)

    def test_a_stopping_flag_alone_never_ends_the_worker(self):
        """close() cannot set its flag and queue the sentinel in one indivisible step.

        An idle worker that woke up between those two statements used to leave through the
        empty-queue branch: no _shut_down ran, so the connection and the ledger stayed open
        and the sentinel close() blocks on was never settled. The interleaving window is a
        few bytecodes wide, so racing it would make a flaky test. This drives the state the
        race produces instead - flag set, sentinel not yet queued - and asserts the worker is
        still there to receive it. The sentinel is the only way out.
        """
        import time

        server = self.barrier_server()
        built = self.adapter(server)
        transport = built._transport

        transport._stopping = True
        time.sleep(transport.POLL_SECONDS * 40)

        self.assertTrue(
            transport.thread.is_alive(),
            "the worker left on the flag alone, so _shut_down never closed anything",
        )
        self.assertEqual(
            built.send_message("req-f1", "thread-b", "hello", AUTHORIZED)["status"],
            "accepted",
        )

        built.close()

        self.assertFalse(transport.thread.is_alive(), "the sentinel did not end the worker")
        self.assertTrue(server.closed, "shutdown never reached the connection")

    def test_a_replay_is_answered_from_the_ledger_while_the_recipient_is_busy(self):
        """A request the ledger has already settled has an answer. Busy must not replace it.

        The recipient bound is there to stop a second turn being started for one thread. A
        replay starts nothing and mutates nothing - it reads a receipt - so refusing it as a
        busy recipient turned a known outcome back into a retry, which is precisely what a
        request id exists to prevent. The precheck asks the ledger before the lock.
        """
        import threading

        server = self.barrier_server()
        built = self.adapter(server)

        first = built.send_message("req-g1", "thread-a", "hello", AUTHORIZED)
        self.assertEqual(first["status"], "accepted", first)

        # Now occupy that same recipient with a different request, held at its first RPC.
        server.hold.add("thread-a")
        thread, _outcome = self.send_in_background(built, "req-g2", "thread-a")
        self.assertTrue(
            server.entered.setdefault("thread-a", threading.Event()).wait(5),
            "the occupying send never reached the RPC boundary",
        )

        replay = built.send_message("req-g1", "thread-a", "hello", AUTHORIZED)

        self.assertTrue(replay.get("replayed"), f"the replay was not answered: {replay}")
        self.assertEqual(replay["status"], "accepted", replay)
        self.assertEqual(replay["turnId"], first["turnId"], "a different outcome was reported")
        self.assertEqual(
            server.starts, ["thread-a"], "the replay reached the host instead of the ledger",
        )
        thread.join(timeout=10)

    def test_a_reused_id_with_different_arguments_is_still_rejected(self):
        """The precheck must not become a way to smuggle a mismatched id past the ledger.

        ledger.lookup raises when an id was used with different arguments, and that raise is
        the rejection. Answering it before the recipient lock has to keep it, not swallow it
        into a busy report or a replayed receipt.
        """
        server = self.barrier_server()
        built = self.adapter(server)
        self.assertEqual(
            built.send_message("req-h1", "thread-a", "hello", AUTHORIZED)["status"],
            "accepted",
        )

        with self.assertRaises(ValueError) as caught:
            built.send_message("req-h1", "thread-a", "a different message", AUTHORIZED)

        self.assertIn("different arguments", str(caught.exception))
        self.assertEqual(server.starts, ["thread-a"], "the mismatched id still reached the host")

    def test_a_submission_racing_close_is_answered_rather_than_orphaned(self):
        """Checking acceptance and queueing the work are two steps, and close() fits between.

        A submission that passed the check and queued after the drain had already run left a
        future nobody would ever settle. Its caller waited out the whole budget and read a
        timeout - "outcome unknown" about work that was never started, which is the one
        report a send that never happened must not produce.

        The interleaving is a few bytecodes wide, so it is driven rather than raced: the
        submitter is held at the moment it is about to enqueue, close() is started behind it,
        and then the submitter is let go.
        """
        import threading

        server = self.barrier_server()
        built = self.adapter(server)
        transport = built._transport

        at_the_door = threading.Event()
        let_go = threading.Event()
        real_put = transport._inbox.put

        def held_put(item):
            if item[0] is not None:  # the sentinel must never be held
                at_the_door.set()
                let_go.wait(5)
            real_put(item)

        transport._inbox.put = held_put
        sender, outcome = self.send_in_background(built, "req-i1", "thread-b")
        self.assertTrue(at_the_door.wait(5), "the submission never reached the enqueue")

        closer = threading.Thread(target=built.close, daemon=True)
        closer.start()
        closer.join(timeout=0.5)
        let_go.set()

        sender.join(timeout=10)
        closer.join(timeout=10)

        self.assertFalse(sender.is_alive(), "the caller was left waiting on its own budget")
        error = outcome.get("error")
        self.assertNotIsInstance(
            error, TimeoutError,
            "the submission was orphaned: a timeout is not an answer about work never run",
        )
        if error is not None:
            self.assertIn("nothing was sent", str(error), outcome)
        else:
            self.assertEqual(outcome["receipt"]["status"], "accepted", outcome)

    def test_a_send_cancelled_by_shutdown_reaches_its_caller_catchably(self):
        """`_shut_down` cancels work that outlived the drain, and the caller must be able to catch it.

        `asyncio.CancelledError` inherits from BaseException, so forwarded unchanged it walks
        past every `except Exception` between here and the tick - including the one the
        delivery layer wraps the send in.
        """
        outcome = capture_shutdown_cancellation(self.tmp)

        error = outcome.get("error")
        self.assertIsNotNone(error, f"the cancelled send was not reported at all: {outcome}")
        self.assertIsInstance(
            error, Exception,
            f"a caller's 'except Exception' cannot see {type(error).__name__}",
        )
        self.assertIn("outcome unknown", str(error))
        # The reason is still readable rather than replaced.
        import asyncio
        self.assertIsInstance(error.__cause__, asyncio.CancelledError)


class ShutdownCancellationSettlesItsDelivery(DeliveryTestCase):
    """Criterion 4: what a cancelled send leaves behind has to be accountable after a restart.

    The transport half is asserted in TransportIsolation. This is the consequence that made
    it worth fixing: `DeliveryService.attempt` claims the delivery and inserts its attempt row
    in one transaction, then wraps only the send in `except Exception`. An exception it cannot
    catch unwinds the tick between those two points and leaves the delivery leased, in
    `sending`, with an attempt row nothing settles.
    """

    def setUp(self):
        super().setUp()
        try:
            import codex_thread_bridge.ledger  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")

    def test_a_claimed_delivery_is_settled_when_shutdown_cancels_its_send(self):
        # The exception is the real one: produced by a real transport cancelling a real send,
        # not a hand-written stand-in for what that path might raise.
        error = capture_shutdown_cancellation(self.tmp).get("error")
        self.assertIsNotNone(error, "the helper produced no error to work from")
        _relationship, event_id = self.queued_event()

        class CancelledByShutdown:
            """The ordinary adapter, except its send ends the way shutdown ends one."""

            def __init__(self, inner):
                self._inner = inner

            def __getattr__(self, name):
                return getattr(self._inner, name)

            def send_message(self, *args, **kwargs):
                raise error

        record = self.delivery.attempt(
            event_id, CancelledByShutdown(self.adapter), now=self.clock.now(),
        )

        self.assertIsNotNone(record, "attempt() unwound instead of settling its own claim")
        attempts = self.attempts_for(event_id)
        self.assertEqual(len(attempts), 1, attempts)
        self.assertEqual(
            attempts[0]["internal_state"], "settled",
            "the claim was taken and the attempt row was left open",
        )

        # The restart. A fresh service over the same store is what a supervisor segment
        # boundary produces, and it must not find a claim nobody can account for.
        from codex_session_relay.delivery import DeliveryService

        restarted = DeliveryService(self.store, self.registry, self.intake, self.clock)
        row = restarted.get(event_id)

        self.assertEqual(row["state"], "held_uncertain", dict(row))
        self.assertNotEqual(row["state"], "sending", "the delivery is still mid-send")
        self.assertIsNone(row["lease_owner"], "the lease outlived the process that took it")
        self.assertIsNone(row["lease_until"])


class ThreadCreationAndPreStartGuard(unittest.TestCase):
    """Creation and the optional pre-start guard, on the real worker and a real ledger.

    The RPC is fake. The ledger is the pinned bridge's sqlite ledger, built on the worker, so
    a retained thread id is a row rather than a stand-in. No socket is opened.
    """

    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        import shutil
        import tempfile

        self.tmp = tempfile.mkdtemp(prefix="admission-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.cwd = tempfile.mkdtemp(prefix="cwd-", dir=self.tmp)

    def _adapter(self, server):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        self.ledger_path = Path(self.tmp) / "operations.sqlite3"

        def ledger_factory():
            return Path(self.tmp) / "socket", Ledger(self.ledger_path)

        adapter = BridgeHostAdapter(
            str(Path(self.tmp) / "socket"),
            app_server_factory=lambda canonical: server,
            ledger_factory=ledger_factory,
            timeout=10,
        )
        self.addCleanup(adapter.close)
        return adapter

    def test_a_read_only_adapter_refuses_creation(self):
        adapter = BridgeHostAdapter(call=lambda method, params: {})
        with self.assertRaises(HostUnavailable) as raised:
            adapter.create_thread(request_id="create-readonly", cwd=self.cwd)
        self.assertIn("read-only", str(raised.exception))

    def test_creation_keeps_one_stable_id_and_a_partial_failure_is_not_duplicated(self):
        calls = []

        class Creating:
            socket_path = None
            info = {}

            async def call(self, method, params):
                from codex_thread_bridge.effects import mark_sent

                mark_sent(method)
                calls.append(method)
                if method == "thread/start":
                    return {
                        "thread": {
                            "id": "thread-created-1",
                        },
                        "cwd": self_cwd,
                        "approvalPolicy": "never",
                        "model": "gpt-5.4",
                        "reasoningEffort": "medium",
                        "runtimeWorkspaceRoots": [self_cwd],
                        "sandbox": {"type": "readOnly", "networkAccess": False},
                    }
                if method == "thread/name/set":
                    from codex_thread_bridge.rpc import RpcError

                    raise RpcError("thread/name/set", {
                        "code": "naming_failed",
                        "message": "naming failed after the shell existed",
                    })
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        self_cwd = self.cwd
        adapter = self._adapter(Creating())
        receipt = adapter.create_thread(
            "create-stable-1", cwd=self.cwd, title="bootstrap", model="gpt-5.4",
            reasoning_effort="medium", sandbox="read-only",
        )
        self.assertEqual(receipt.get("threadId"), "thread-created-1", json.dumps(receipt, default=str)[:2500])
        # Bridge._mutate records an RpcError-shaped failure as failed and keeps the shell.
        # A bare exception after thread/start is outcome_unknown. Either way the id stays,
        # and the same request does not create a second thread. This case is the named one:
        # naming raised through the fake RPC as a normal exception, which the bridge saves
        # as failed once the id is already on the receipt.
        self.assertEqual(receipt["status"], "failed")
        self.assertIn("thread-created-1", json.dumps(receipt))
        retained = adapter.get_operation("create-stable-1")
        self.assertEqual(retained["threadId"], "thread-created-1")
        again = adapter.create_thread(
            "create-stable-1", cwd=self.cwd, title="bootstrap", model="gpt-5.4",
            reasoning_effort="medium", sandbox="read-only",
        )
        self.assertTrue(again.get("replayed"))
        self.assertEqual(again["threadId"], "thread-created-1")
        self.assertEqual(calls, ["thread/start", "thread/name/set"])

    def test_a_pause_during_resume_starts_no_turn(self):
        import asyncio
        import threading

        calls = []
        resumed = threading.Event()
        release = threading.Event()
        goal = {"status": "active"}

        class Resuming:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append(method)
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    resumed.set()
                    await asyncio.to_thread(release.wait)
                    return authorized_resume_response()
                if method == "thread/goal/get":
                    return {"goal": {"status": goal["status"]}}
                if method == "turn/start":
                    return {"turn": {"id": "must-not-start"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        adapter = self._adapter(Resuming())

        async def before_start(rpc):
            # The same worker client the send already holds. A second transport, or a
            # synchronous call back into the adapter, would wait on this worker forever.
            fresh = await rpc.call("thread/goal/get", {"threadId": "thread-1"})
            if (fresh.get("goal") or {}).get("status") == "paused":
                return {"code": "recipient_paused", "message": "paused during resume"}
            return None

        outcome = {}

        def run():
            outcome["receipt"] = adapter.send_message(
                "send-paused-1", "thread-1", "hello", AUTHORIZED, before_start=before_start,
            )

        caller = threading.Thread(target=run, daemon=True)
        caller.start()
        self.assertTrue(resumed.wait(5), "resume never started")
        goal["status"] = "paused"
        release.set()
        caller.join(timeout=10)
        self.assertFalse(caller.is_alive())
        self.assertEqual(outcome["receipt"]["status"], "not_attempted")
        self.assertEqual(outcome["receipt"]["rpcError"]["code"], "recipient_paused")
        self.assertNotIn("turn/start", calls)
        self.assertIn("thread/goal/get", calls)
        self.assertEqual(outcome["receipt"]["status"], "not_attempted")
        self.assertIs(outcome["receipt"]["retrySafe"], True)
        self.assertEqual(outcome["receipt"]["attemptedEffects"], [])
        self.assertIn("resumed", outcome["receipt"])

    def test_a_retained_acceptance_does_not_run_the_guard_or_send_again(self):
        calls = []
        guards = []

        class Sending:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append(method)
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    return authorized_resume_response()
                if method == "turn/start":
                    return {"turn": {"id": "turn-kept-1"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        async def before_start(rpc):
            guards.append("ran")
            self.assertTrue(hasattr(rpc, "call"))
            return None

        adapter = self._adapter(Sending())
        first = adapter.send_message(
            "send-kept-1", "thread-1", "hello", AUTHORIZED, before_start=before_start,
        )
        again = adapter.send_message(
            "send-kept-1", "thread-1", "hello", AUTHORIZED, before_start=before_start,
        )
        self.assertEqual(first["status"], "accepted")
        self.assertEqual(first["turnId"], "turn-kept-1")
        self.assertTrue(again.get("replayed"))
        self.assertEqual(again["turnId"], "turn-kept-1")
        self.assertEqual(guards, ["ran"])
        self.assertEqual(calls.count("turn/start"), 1)

    def test_a_guard_refusal_retries_once_under_the_same_request(self):
        calls = []
        decision = {"refuse": True}

        class Sending:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append(method)
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    return authorized_resume_response()
                if method == "turn/start":
                    return {"turn": {"id": "turn-after-recovery"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        async def before_start(rpc):
            if decision["refuse"]:
                return {"code": "recipient_paused", "message": "paused before the business turn"}
            return None

        adapter = self._adapter(Sending())
        refused = adapter.send_message(
            "send-recover-1", "thread-1", "hello", AUTHORIZED, before_start=before_start,
        )
        self.assertEqual(refused["status"], "not_attempted")
        self.assertTrue(refused["retrySafe"])
        self.assertEqual(refused["attemptedEffects"], [])
        self.assertNotIn("turn/start", calls)
        decision["refuse"] = False
        accepted = adapter.send_message(
            "send-recover-1", "thread-1", "hello", AUTHORIZED, before_start=before_start,
        )
        self.assertEqual(accepted["status"], "accepted")
        self.assertEqual(accepted["turnId"], "turn-after-recovery")
        self.assertFalse(accepted.get("replayed"))
        self.assertEqual(calls.count("turn/start"), 1)
        replay = adapter.send_message(
            "send-recover-1", "thread-1", "hello", AUTHORIZED, before_start=before_start,
        )
        self.assertTrue(replay.get("replayed"))
        self.assertEqual(replay["turnId"], "turn-after-recovery")
        self.assertEqual(calls.count("turn/start"), 1)


class _UseProcessPolicy:
    """Leave BridgeHostAdapter on the policy this process snapshotted."""


_USE_PROCESS_POLICY = _UseProcessPolicy()


def _child_policy(directory, *, model="gpt-5.4", effort="medium"):
    """A host policy that declares a child pair and nothing the caller can invent."""
    import json
    from pathlib import Path

    path = Path(directory) / "execution-policy.json"
    path.write_text(json.dumps({
        "roles": {"child": {"model": model, "reasoningEffort": effort}},
    }))
    return path


class ChildCreationUnderDeclaredPolicy(unittest.TestCase):
    """A managed child is created on the real Bridge, under the policy this process declared.

    The RPC is a stand-in. The ledger and the execution policy are the pinned bridge's own,
    so a role the policy does not declare is refused before thread/start, and a declared
    child pair is the one the host is asked for.
    """

    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        import os
        import shutil
        import tempfile

        from codex_session_relay import rolepolicy

        root = os.environ.get("TMPDIR") or tempfile.gettempdir()
        self.tmp = tempfile.mkdtemp(prefix="child-policy-", dir=root)
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.cwd = tempfile.mkdtemp(prefix="cwd-", dir=self.tmp)
        self._policy_env = os.environ.get("CODEX_THREAD_BRIDGE_EXECUTION_POLICY")
        os.environ["CODEX_THREAD_BRIDGE_EXECUTION_POLICY"] = str(_child_policy(self.tmp))
        rolepolicy.reset()
        self.addCleanup(self._restore_policy)

    def _restore_policy(self):
        import os

        from codex_session_relay import rolepolicy

        if self._policy_env is None:
            os.environ.pop("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", None)
        else:
            os.environ["CODEX_THREAD_BRIDGE_EXECUTION_POLICY"] = self._policy_env
        rolepolicy.reset()

    def _adapter(self, server, *, execution_policy=_USE_PROCESS_POLICY):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        def ledger_factory():
            return Path(self.tmp) / "socket", Ledger(Path(self.tmp) / "operations.sqlite3")

        options = {}
        if execution_policy is not _USE_PROCESS_POLICY:
            options["execution_policy"] = execution_policy
        adapter = BridgeHostAdapter(
            str(Path(self.tmp) / "socket"),
            app_server_factory=lambda canonical: server,
            ledger_factory=ledger_factory,
            timeout=10,
            **options,
        )
        self.addCleanup(adapter.close)
        return adapter

    def test_a_child_role_is_refused_when_the_bridge_carries_no_declared_roles(self):
        """Presence-only has no roles, so role=child must not reach thread/start."""
        calls = []

        class Creating:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append(method)
                raise AssertionError(f"a refused creation reached {method}")

            async def close(self):
                return None

        from codex_thread_bridge.execution import PRESENCE_ONLY

        adapter = self._adapter(Creating(), execution_policy=PRESENCE_ONLY)
        with self.assertRaises(Exception) as raised:
            adapter.create_thread(
                "create-child-refused", cwd=self.cwd, title="bootstrap",
                model="gpt-5.4", reasoning_effort="medium", sandbox="read-only",
                role="child",
            )
        self.assertIn("execution_role", str(raised.exception))
        self.assertEqual(calls, [])

    def test_a_declared_child_pair_is_created_and_an_undeclared_pair_is_not(self):
        calls = []

        class Creating:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append((method, params))
                if method == "thread/start":
                    return {
                        "thread": {"id": "thread-child-1"},
                        "cwd": self_cwd,
                        "approvalPolicy": "never",
                        "model": params.get("model"),
                        "reasoningEffort": "medium",
                        "runtimeWorkspaceRoots": [self_cwd],
                        "sandbox": {"type": "readOnly", "networkAccess": False},
                    }
                if method == "thread/name/set":
                    return {}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        self_cwd = self.cwd
        adapter = self._adapter(Creating())
        with self.assertRaises(Exception) as raised:
            adapter.create_thread(
                "create-child-wrong-pair", cwd=self.cwd, model="devin/swe-2",
                reasoning_effort="max", sandbox="read-only", role="child",
            )
        self.assertIn("execution_role", str(raised.exception))
        self.assertEqual(calls, [])
        receipt = adapter.create_thread(
            "create-child-1", cwd=self.cwd, title="child", model="gpt-5.4",
            reasoning_effort="medium", sandbox="read-only", role="child",
        )
        self.assertEqual(receipt.get("status"), "accepted", json.dumps(receipt, default=str)[:2000])
        self.assertEqual(receipt.get("threadId"), "thread-child-1")
        self.assertEqual(receipt.get("executionPolicy", {}).get("role"), "child")
        started = [params for method, params in calls if method == "thread/start"]
        self.assertEqual(len(started), 1)
        self.assertEqual(started[0].get("model"), "gpt-5.4")


class GuardedSendBudget(unittest.TestCase):
    """A declared guard widens one submission; an ordinary send keeps both old bounds."""

    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        import shutil
        import tempfile

        self.tmp = tempfile.mkdtemp(prefix="guard-budget-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def _adapter(self, server):
        from pathlib import Path

        from codex_thread_bridge.ledger import Ledger

        socket = Path(self.tmp) / "socket"
        return BridgeHostAdapter(
            str(socket),
            timeout=0.2,
            caller_slack=0.2,
            app_server_factory=lambda canonical: server,
            ledger_factory=lambda: (socket, Ledger(Path(self.tmp) / "operations.sqlite3")),
        )

    def _observe(self, adapter, seen):
        import asyncio
        from contextlib import contextmanager
        from unittest.mock import patch

        real_wait_for = asyncio.wait_for
        real_result = adapter._transport._futures.Future.result

        async def observe_wait_for(awaitable, timeout):
            seen["execution"] = timeout
            return await real_wait_for(awaitable, timeout)

        def observe_result(future, budget):
            seen["caller"] = budget
            return real_result(future, budget)

        @contextmanager
        def observed():
            with patch.object(asyncio, "wait_for", observe_wait_for), patch.object(
                adapter._transport._futures.Future, "result", observe_result,
            ):
                try:
                    yield
                finally:
                    adapter.close()

        return observed()

    def test_a_declared_guard_widens_both_budgets_and_finishes(self):
        from codex_session_relay.bridge_adapter import _Transport

        calls = []

        class Guarded:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append(method)
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    return authorized_resume_response()
                if method == "thread/list":
                    return {"data": [{"id": "thread-1"}], "nextCursor": None}
                if method == "turn/start":
                    return {"turn": {"id": "turn-guarded"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        async def before_start(rpc):
            for _page in range(10):
                await rpc.call("thread/list", {"limit": 1})
            return None

        adapter = self._adapter(Guarded())
        stage = adapter._transport.timeout * _Transport.RPC_STAGES_PER_REQUEST
        ordinary = stage * _Transport.RPC_REQUESTS_PER_SEND
        execution = stage * (_Transport.RPC_REQUESTS_PER_SEND + 10)
        seen = {}
        with self._observe(adapter, seen):
            receipt = adapter.send_message(
                "send-guarded-budget", "thread-1", "hello", AUTHORIZED,
                before_start=before_start, guard_rpc_requests=10,
            )
            self.assertEqual(seen["execution"], execution)
            self.assertGreater(seen["execution"], ordinary)
            self.assertEqual(seen["caller"], execution + adapter._transport._caller_slack)
            self.assertGreater(seen["caller"], seen["execution"])
        self.assertEqual(receipt["status"], "accepted")
        self.assertEqual(receipt["turnId"], "turn-guarded")
        self.assertEqual(calls.count("thread/list"), 10)

    def test_an_ordinary_send_keeps_both_historical_budgets(self):
        from codex_session_relay.bridge_adapter import _Transport

        class Idle:
            socket_path = None
            info = {}

            async def call(self, method, params):
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}}}
                if method == "thread/resume":
                    return authorized_resume_response()
                if method == "turn/start":
                    return {"turn": {"id": "turn-ordinary"}}
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        adapter = self._adapter(Idle())
        execution = adapter._transport.timeout * _Transport.RPC_STAGES_PER_SEND
        caller = adapter._transport.timeout + adapter._transport._caller_slack
        seen = {}
        with self._observe(adapter, seen):
            receipt = adapter.send_message("send-ordinary-budget", "thread-1", "hello", AUTHORIZED)
            self.assertEqual(seen["execution"], execution)
            self.assertEqual(seen["caller"], caller)
            self.assertLess(seen["caller"], execution)
        self.assertEqual(receipt["status"], "accepted")

    def test_an_invalid_guard_budget_is_refused_before_any_send(self):
        calls = []

        class Untouched:
            socket_path = None
            info = {}

            async def call(self, method, params):
                calls.append(method)
                raise AssertionError("an invalid budget reached the host")

            async def close(self):
                return None

        adapter = self._adapter(Untouched())
        self.addCleanup(adapter.close)
        for budget in (-1, 11, True, 1.5, None, "10"):
            with self.subTest(budget=budget):
                with self.assertRaises(HostUnavailable) as raised:
                    adapter.send_message(
                        "send-bad-budget", "thread-1", "hello", AUTHORIZED,
                        guard_rpc_requests=budget,
                    )
                self.assertIn("guard_rpc_requests", str(raised.exception))
        self.assertEqual(calls, [])
        self.assertIsNone(adapter.get_operation("send-bad-budget"))
