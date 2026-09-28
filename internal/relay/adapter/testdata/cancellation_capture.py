import asyncio
import json
import sys
import tempfile
from pathlib import Path

from codex_session_relay.bridge_adapter import _guarded_send
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger

spec = json.load(sys.stdin)


async def one(stage):
    entered = asyncio.Event()
    release = asyncio.Event()
    calls = []

    class RPC:
        async def call(self, method, params):
            calls.append(method)
            if method == stage:
                entered.set()
                await release.wait()
            if method == "thread/read":
                return {"thread": {"status": {"type": "idle"}}}
            if method == "thread/resume":
                return spec["resume"]
            if method == "turn/start":
                return {"turn": {"id": "turn"}}
            raise AssertionError(method)

    ledger = Ledger(Path(tempfile.mkdtemp()) / "ops.sqlite3")
    request = "cancel-" + stage
    try:
        if stage == "before":
            calls = []
            status = None
        else:
            task = asyncio.create_task(_guarded_send(
                RPC(), ledger, request, "thread", "message", TaskSettings(spec["settings"])
            ))
            await asyncio.wait_for(entered.wait(), 5)
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass
            status = ledger.get(request)["status"]
        return {"stage": stage, "calls": calls, "status": status}
    finally:
        ledger.close()


async def main():
    result = []
    for stage in ("before", "thread/resume", "turn/start"):
        result.append(await one(stage))
    print(json.dumps(result, separators=(",", ":")))


asyncio.run(main())
