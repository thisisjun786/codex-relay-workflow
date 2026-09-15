import asyncio
import json
import tempfile
from pathlib import Path

import pytest
from websockets.asyncio.server import unix_serve

from codex_thread_bridge.bridge import Bridge
from codex_thread_bridge.ledger import Ledger
from codex_thread_bridge.rpc import AppServer


class FakeServer:
    def __init__(self):
        self.calls = []
        self.threads = {}
        self.reject = {}
        self.drop_after = None
        self.override_creation = {}
        self.approval_policy = "never"
        self.complete_turns = True
        self.goal = None
        self.handshake_extensions = []
        self.cursor_padding = 160
        self.pause_after = None
        self.paused = asyncio.Event()
        self.release = asyncio.Event()

    def cursor(self, offset):
        return f"{offset}:" + "x" * self.cursor_padding

    def offset(self, cursor):
        if cursor is None:
            return 0
        offset = int(cursor.split(":", 1)[0])
        if cursor != self.cursor(offset):
            raise ValueError("invalid cursor")
        return offset

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
                self.threads[tid] = thread
                result = {
                    "thread": dict(thread),
                    "cwd": params["cwd"],
                    "runtimeWorkspaceRoots": params.get("runtimeWorkspaceRoots", [params["cwd"]]),
                    "approvalPolicy": "never",
                    "sandbox": {
                        "type": {
                            "read-only": "readOnly",
                            "workspace-write": "workspaceWrite",
                            "danger-full-access": "dangerFullAccess",
                        }[params["sandbox"]],
                        **({"networkAccess": False} if params["sandbox"] == "read-only" else {}),
                    },
                    "model": params.get("model", "configured-default"),
                    "reasoningEffort": params.get("config", {}).get(
                        "model_reasoning_effort", "medium"
                    ),
                    **self.override_creation,
                }
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
                result = {"thread": {**thread, "turns": []}, "approvalPolicy": self.approval_policy}
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
                result = {
                    "data": turns[start:end],
                    "nextCursor": self.cursor(end) if end < len(turns) else None,
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
            await ws.send(json.dumps({"method": "thread/status/changed", "params": {}}))
            if method == self.drop_after:
                await ws.close()
                return
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
