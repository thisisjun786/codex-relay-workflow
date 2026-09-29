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
    """Cancel while stage["held"] is suspended; its answer lands after the cancellation."""
    entered = asyncio.Event()
    release = asyncio.Event()
    calls = []

    async def hold():
        entered.set()
        await release.wait()

    class RPC:
        async def call(self, method, params):
            calls.append(method)
            if method == stage["held"]:
                await hold()
            if method == "thread/read":
                return {"thread": {"status": {"type": "active" if stage["busy"] else "idle"}}}
            if method == "thread/resume":
                return spec["resume"]
            if method == "turn/start":
                return {"turn": {"id": "turn"}}
            raise AssertionError(method)

    async def guard(_rpc):
        await hold()
        return {"code": "managed_paused", "message": "paused"}

    ledger = Ledger(Path(tempfile.mkdtemp()) / "ops.sqlite3")
    request = "cancel"
    try:
        if stage["name"] == "before":
            calls = []
            status = None
        else:
            task = asyncio.create_task(_guarded_send(
                RPC(), ledger, request, "thread", "message", TaskSettings(spec["settings"]),
                before_start=guard if stage["held"] == "guard" else None,
            ))
            await asyncio.wait_for(entered.wait(), 5)
            task.cancel()
            release.set()
            try:
                await task
            except asyncio.CancelledError:
                pass
            status = ledger.get(request)["status"]
        return {"stage": stage["name"], "calls": calls, "status": status}
    finally:
        ledger.close()


async def main():
    result = []
    for stage in spec["stages"]:
        result.append(await one(stage))
    print(json.dumps(result, separators=(",", ":")))


asyncio.run(main())
