"""The real bridge adapter logic, driven through an injected RPC surface.

No socket, no live host. Every response shape here is the one the App Server schemas define, and
the archive case reproduces the real observation that started this: a task whose cwd filter misses
it while an unfiltered listing returns the exact same id.
"""

import unittest

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.hostadapter import HostUnavailable
from codex_session_relay.settings import TaskSettings

from .support import RelayTestCase

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
        self.assertEqual(receipt["status"], "failed")
        self.assertEqual(receipt["rpcError"]["code"], code)
        self.assertIsNone(receipt.get("turnId"))
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, WITHHELD_PRE_SEND)
        self.assertEqual(facts.send_attempted, "no")
        self.assertTrue(facts.retry_safe)
        self.assertEqual(facts.failed_operation, "thread/resume")

    # ------------------------------------------------------- the ordinary path

    def test_the_start_actually_carries_every_authorized_setting(self):
        """The start response cannot echo settings, so the OUTGOING params are asserted."""
        adapter, calls = self._adapter()
        receipt = adapter.send_message("del-100000000000-a1", "thread-1", "hi", AUTHORIZED)
        self.assertEqual(receipt["status"], "accepted")
        self.assertEqual(self._methods(calls), ["thread/read", "thread/resume", "turn/start"])
        start = dict(calls[-1][1])
        self.assertEqual(start["sandboxPolicy"]["type"], "workspaceWrite")
        self.assertFalse(start["sandboxPolicy"]["networkAccess"])
        self.assertEqual(start["approvalPolicy"], "never")
        self.assertEqual(start["cwd"], WORKTREE)
        self.assertEqual(start["runtimeWorkspaceRoots"], [WORKTREE])
        self.assertEqual(start["model"], "anthropic/claude-opus-5")
        self.assertEqual(start["effort"], "xhigh")
        self.assertEqual(start["environments"], AUTHORIZED_ENVIRONMENTS)
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
        # ThreadResumeParams has no effort field; the schema does accept config.
        self.assertEqual(resume["config"], {"model_reasoning_effort": "xhigh"})
        self.assertNotIn("environments", resume)

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
