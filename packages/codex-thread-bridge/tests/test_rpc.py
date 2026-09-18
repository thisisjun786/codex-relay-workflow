import asyncio
import json
import tempfile
from pathlib import Path

import pytest
from websockets.asyncio.server import unix_serve

from codex_thread_bridge.effects import recording
from codex_thread_bridge.rpc import AppServer, RpcError, TransportError


async def test_rpc_multiplexes_interleaved_notifications(fake_server):
    fake, path = fake_server
    client = AppServer(path)
    try:
        results = await asyncio.gather(
            *[client.call("thread/goal/get", {"threadId": f"thread-{i}"}) for i in range(10)]
        )
        assert results == [{"goal": None}] * 10
        assert fake.handshake_extensions == [None]
    finally:
        await client.close()


async def test_rpc_preserves_api_error_and_reconnects_only_for_new_requests(fake_server):
    fake, path = fake_server
    client = AppServer(path)
    try:
        fake.reject["thread/goal/get"] = {"code": -32601, "message": "unsupported"}
        with pytest.raises(RpcError, match="unsupported"):
            await client.call("thread/goal/get", {"threadId": "missing"})
        fake.reject.clear()
        fake.drop_after = "thread/goal/get"
        with pytest.raises(TransportError):
            await client.call("thread/goal/get", {"threadId": "dropped"})
        assert fake.count("thread/goal/get") == 2
        fake.drop_after = None
        assert await client.call("thread/goal/get", {"threadId": "new"}) == {"goal": None}
        assert fake.count("thread/goal/get") == 3
    finally:
        await client.close()


async def test_timeout_does_not_retry_request():
    calls = []
    stop = asyncio.Event()

    async def handler(ws):
        async for raw in ws:
            message = json.loads(raw)
            method = message["method"]
            calls.append(method)
            if method == "initialize":
                await ws.send(json.dumps({"id": message["id"], "result": {}}))
            elif method == "thread/start":
                await stop.wait()

    with tempfile.TemporaryDirectory(prefix="ctb-timeout-") as directory:
        path = Path(directory) / "app.sock"
        async with unix_serve(handler, str(path)):
            client = AppServer(path, timeout=0.05)
            try:
                with pytest.raises(TransportError, match="do not resend"):
                    await client.call("thread/start", {})
                assert calls.count("thread/start") == 1
            finally:
                stop.set()
                await client.close()


async def test_only_a_state_changing_frame_that_was_written_counts_as_an_attempt(fake_server):
    """What was begun is recorded where the frame is written and classified by the method itself.

    A method nobody has classified counts as a change, so a mutation added later cannot be read
    as nothing having happened because a set somewhere was not updated.
    """
    _, path = fake_server
    client = AppServer(path, timeout=1)
    try:
        with recording() as effects:
            await client.call("thread/list", {"limit": 1, "useStateDbOnly": True})
        assert effects.attempted == []
        assert effects.observed == ["initialize", "thread/list"]

        with recording() as effects:
            with pytest.raises(RpcError):
                await client.call("thread/archive", {"threadId": "thread-1"})
        assert effects.attempted == ["thread/archive"]
    finally:
        await client.close()


async def test_a_socket_that_cannot_be_reached_records_nothing(tmp_path):
    client = AppServer(tmp_path / "absent.sock", timeout=1)
    try:
        with recording() as effects:
            with pytest.raises(OSError):
                await client.call("turn/start", {"threadId": "thread-1"})
        assert effects.attempted == [] and effects.observed == []
    finally:
        await client.close()
