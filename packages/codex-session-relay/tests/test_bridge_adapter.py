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
