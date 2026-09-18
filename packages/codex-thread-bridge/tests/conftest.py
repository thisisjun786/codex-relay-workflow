import asyncio
import json
import tempfile
from pathlib import Path

import pytest
from websockets.asyncio.server import unix_serve

from codex_thread_bridge.bridge import Bridge
from codex_thread_bridge.execution import ExecutionPolicy
from codex_thread_bridge.ledger import Ledger
from codex_thread_bridge.rpc import AppServer

# The pair the suite uses wherever the guard is not what is being tested. It is also what a
# configured allowlist in these tests approves, so one constant covers both modes.
MODEL = "anthropic/claude-opus-5"
EFFORT = "xhigh"
EXECUTION = {"model": MODEL, "reasoning_effort": EFFORT}


class FakeServer:
    def __init__(self):
        self.calls = []
        self.threads = {}
        self.reject = {}
        self.drop_after = None
        self.override_creation = {}
        self.override_resume = {}
        # Fields the host simply does not report back, to exercise "we cannot tell".
        self.unreported = set()
        self.approval_policy = "never"
        self.complete_turns = True
        self.goal = None
        self.handshake_extensions = []
        self.cursor_padding = 160
        self.pause_after = None
        self.paused = asyncio.Event()
        self.release = asyncio.Event()
        # A response larger than the client will buffer. A callable rather than a knob per case:
        # it is handed the method and its params and returns padding bytes, so one test can
        # overflow a single view or a single page size without the fake growing a flag for each.
        self.oversize = lambda method, params: None
        # Padding for the notification this fake already interleaves BEFORE a method's response.
        # That is how an oversized frame belonging to no request at all takes the connection down
        # while somebody else's request is still pending.
        self.oversize_before = {}
        # A close the PEER initiates, carrying the same reason a locally refused frame produces,
        # so a test can prove that which side sent it is what tells the two apart.
        self.peer_close = None
        # None means turn/steer echoes the guarded turn back, which is what the real host does
        # on success. Set it to make the host answer with a different turn.
        self.steer_turn_id = None
        # Fields the host reports back after a goal/set, over and above the requested status.
        self.goal_after_set = {}

    def cursor(self, offset):
        return f"{offset}:" + "x" * self.cursor_padding

    def offset(self, cursor):
        if cursor is None:
            return 0
        offset = int(cursor.split(":", 1)[0])
        if cursor != self.cursor(offset):
            raise ValueError("invalid cursor")
        return offset

    def turn_view(self, turn, view):
        """One turn as each itemsView actually returns it, measured on codex-cli 0.154.0.

        "full" is every item, "summary" is the turn's user and agent messages and nothing else,
        and "notLoaded" is no items at all. A fake that ignored the parameter would let a test
        assert on content the real host would not have sent.
        """
        if view == "notLoaded":
            items = []
        elif view == "summary":
            items = [
                item
                for item in turn["items"]
                if item.get("type") in {"userMessage", "agentMessage"}
            ]
        else:
            items = list(turn["items"])
        return {**turn, "items": items, "itemsView": view}

    def settings_view(self, params):
        """What this host reports about a thread's settings, shaped like the real one.

        Measured on codex-cli 0.154.0: sandbox comes back as the FULL policy object with every
        declared default filled in, effort is whatever config.model_reasoning_effort asked for
        (the host echoes it without validating it), and the workspace-write policy fields are
        taken from the config section, since the sandbox parameter is only a mode.
        """
        config = params.get("config") or {}
        workspace = config.get("sandbox_workspace_write") or {}
        kind = {
            "read-only": "readOnly",
            "workspace-write": "workspaceWrite",
            "danger-full-access": "dangerFullAccess",
        }[params.get("sandbox", "read-only")]
        if kind == "workspaceWrite":
            sandbox = {
                "type": kind,
                "writableRoots": workspace.get("writable_roots", []),
                "networkAccess": workspace.get("network_access", False),
                "excludeTmpdirEnvVar": workspace.get("exclude_tmpdir_env_var", False),
                "excludeSlashTmp": workspace.get("exclude_slash_tmp", False),
            }
        elif kind == "readOnly":
            sandbox = {"type": kind, "networkAccess": False}
        else:
            sandbox = {"type": kind}
        return {
            "cwd": params["cwd"],
            "runtimeWorkspaceRoots": params.get("runtimeWorkspaceRoots", [params["cwd"]]),
            "approvalPolicy": "never",
            "sandbox": sandbox,
            "model": params.get("model", "configured-default"),
            "reasoningEffort": config.get("model_reasoning_effort", "medium"),
        }

    async def handle(self, ws):
        self.handshake_extensions.append(ws.request.headers.get("Sec-WebSocket-Extensions"))
        initialized = False
        async for raw in ws:
            message = json.loads(raw)
            if "method" not in message:
                continue
            method, params = message["method"], message.get("params", {})
            self.calls.append((method, params))
            if method == "initialized":
                continue
            ident = message["id"]
            if method == "initialize":
                initialized = True
                result = {"userAgent": "fake Codex/0.153.4", "platformOs": "linux"}
            elif not initialized:
                await ws.send(json.dumps({"id": ident, "error": {"message": "not initialized"}}))
                continue
            elif method in self.reject:
                await ws.send(json.dumps({"id": ident, "error": self.reject[method]}))
                continue
            elif method == "thread/start":
                tid = f"thread-{len(self.threads) + 1}"
                thread = {"id": tid, "cwd": params["cwd"], "status": {"type": "idle"}, "turns": []}
                # The real host reports the project a thread was started in, and
                # create_worktree_thread refuses a launch whose returned project differs from the
                # requested one. A fake that drops the field fails that check for a reason the
                # code under test never had.
                if params.get("projectId") is not None:
                    thread["projectId"] = params["projectId"]
                self.threads[tid] = thread
                result = {"thread": dict(thread), **self.settings_view(params)}
                result.update(self.override_creation)
                thread["settings"] = self.settings_view(params)
                # The real Thread object carries these three; measured on codex-cli 0.154.0,
                # thread/read returns model, reasoningEffort, cwd, environments and projectId,
                # and nothing about sandbox or approvalPolicy.
                thread["model"] = thread["settings"]["model"]
                thread["reasoningEffort"] = thread["settings"]["reasoningEffort"]
                result["thread"] = {
                    key: value for key, value in thread.items() if key != "settings"
                }
                for field in self.unreported:
                    result.pop(field, None)
            elif method == "thread/name/set":
                self.threads[params["threadId"]]["name"] = params["name"]
                result = {}
            elif method == "turn/start":
                thread = self.threads[params["threadId"]]
                turn = {
                    "id": f"turn-{len(thread['turns']) + 1}",
                    "status": "completed" if self.complete_turns else "inProgress",
                    "items": [{"type": "agentMessage", "text": params["input"][0]["text"]}],
                }
                thread["turns"].append(turn)
                result = {"turn": turn}
            elif method == "thread/read":
                thread = dict(self.threads[params["threadId"]])
                if not params.get("includeTurns"):
                    thread["turns"] = []
                result = {"thread": thread}
            elif method == "thread/resume":
                thread = self.threads[params["threadId"]]
                # The real host reports the thread's own state; it does not adopt an override.
                retained = dict(thread.get("settings") or {})
                retained["approvalPolicy"] = self.approval_policy
                result = {"thread": {**thread, "turns": []}, **retained}
                result.update(self.override_resume)
                for field in self.unreported:
                    result.pop(field, None)
            elif method == "thread/turns/list":
                turns = list(reversed(self.threads[params["threadId"]]["turns"]))
                try:
                    start = self.offset(params.get("cursor"))
                except ValueError:
                    await ws.send(
                        json.dumps(
                            {"id": ident, "error": {"code": -32602, "message": "invalid cursor"}}
                        )
                    )
                    continue
                end = start + params["limit"]
                view = params.get("itemsView", "full")
                result = {
                    "data": [self.turn_view(turn, view) for turn in turns[start:end]],
                    "nextCursor": self.cursor(end) if end < len(turns) else None,
                    "backwardsCursor": self.cursor(start),
                }
            elif method == "thread/items/list":
                items = []
                for turn in self.threads[params["threadId"]]["turns"]:
                    if params.get("turnId") in (None, turn["id"]):
                        items.extend(turn["items"])
                start = self.offset(params.get("cursor"))
                end = start + (params.get("limit") or len(items))
                result = {
                    "data": items[start:end],
                    "nextCursor": self.cursor(end) if end < len(items) else None,
                    "backwardsCursor": self.cursor(start),
                }
            elif method == "thread/list":
                threads = [{**t, "turns": []} for t in self.threads.values()]
                start = self.offset(params.get("cursor"))
                end = start + params["limit"]
                result = {
                    "data": threads[start:end],
                    "nextCursor": self.cursor(end) if end < len(threads) else None,
                }
            elif method == "thread/goal/get":
                result = {"goal": self.goal}
            elif method == "turn/steer":
                self.threads[params["threadId"]]
                result = {"turnId": self.steer_turn_id or params["expectedTurnId"]}
            elif method == "thread/goal/set":
                updated = dict(self.goal or {})
                if params.get("status") is not None:
                    updated["status"] = params["status"]
                updated.update(self.goal_after_set)
                self.goal = updated
                result = {"goal": updated}
            elif method == "project/read":
                result = {"project": {"id": params["projectId"]}}
            else:
                await ws.send(
                    json.dumps({"id": ident, "error": {"code": -32601, "message": method}})
                )
                continue
            # Exercise notification interleaving with every response.
            if method == self.pause_after:
                self.paused.set()
                await self.release.wait()
            padding = self.oversize_before.get(method)
            await ws.send(
                json.dumps(
                    {
                        "method": "thread/status/changed",
                        "params": {"padding": "x" * padding} if padding else {},
                    }
                )
            )
            if method == self.peer_close:
                # The host refusing a frame of OURS. Same code and the same reason text a local
                # refusal writes, so nothing but which side sent it can distinguish them.
                await ws.close(
                    code=1009,
                    reason="frame with 17825792 bytes exceeds limit of 16777216 bytes",
                )
                return
            if method == self.drop_after:
                await ws.close()
                return
            padding = self.oversize(method, params)
            if padding:
                result = {**result, "padding": "x" * padding}
            await ws.send(json.dumps({"id": ident, "result": result}))

    def count(self, method):
        return sum(name == method for name, _ in self.calls)


@pytest.fixture
async def fake_server():
    with tempfile.TemporaryDirectory(prefix="ctb-") as directory:
        path = Path(directory) / "app.sock"
        fake = FakeServer()
        async with unix_serve(fake.handle, str(path)):
            yield fake, path


@pytest.fixture
async def bridge(fake_server, tmp_path):
    _, socket = fake_server
    rpc = AppServer(socket, timeout=1)
    ledger = Ledger(tmp_path / "state" / "operations.sqlite3")
    try:
        yield Bridge(rpc, ledger)
    finally:
        await rpc.close()
        ledger.close()


@pytest.fixture
async def small_frame_bridge(fake_server, tmp_path):
    """A bridge whose frame limit is small enough to overflow cheaply.

    The 16 MiB ceiling is proved once at its real value, in test_rpc.py, because that number is
    the thing the incident hit. Every other test only needs a response the client will not take,
    and at 64 KiB that costs a hundred kilobytes instead of seventeen megabytes a time.
    """
    _, socket = fake_server
    rpc = AppServer(socket, timeout=1, max_frame_bytes=64 * 1024)
    ledger = Ledger(tmp_path / "small" / "operations.sqlite3")
    try:
        yield Bridge(rpc, ledger)
    finally:
        await rpc.close()
        ledger.close()


@pytest.fixture
async def configured_bridge(fake_server, tmp_path):
    """A bridge whose host configured an allowlist, built the way the real server builds one.

    The policy arrives as a constructor argument, exactly as it does in main(), so these tests
    exercise the same object a caller can never reach.
    """
    _, socket = fake_server
    built = []

    def build(mapping, *, digest="test-digest"):
        rpc = AppServer(socket, timeout=1)
        ledger = Ledger(tmp_path / "configured" / f"operations-{len(built)}.sqlite3")
        built.append((rpc, ledger))
        # A ready policy object is accepted too, so a test can hand in a stub and observe which
        # values the bridge actually transmits.
        policy = (
            mapping
            if hasattr(mapping, "authorize")
            else ExecutionPolicy.from_mapping(mapping, digest=digest)
        )
        return Bridge(rpc, ledger, policy=policy)

    try:
        yield build
    finally:
        for rpc, ledger in built:
            await rpc.close()
            ledger.close()
