import asyncio
import json
import tempfile
from pathlib import Path

import pytest
from websockets.asyncio.server import unix_serve

from codex_thread_bridge.effects import recording
from codex_thread_bridge.rpc import (
    MAX_FRAME_BYTES,
    AppServer,
    ResponseTooLarge,
    RpcError,
    TransportError,
)


def one_thread(fake, thread_id="thread-1"):
    fake.threads[thread_id] = {
        "id": thread_id,
        "cwd": "/tmp",
        "status": {"type": "idle"},
        "turns": [],
    }
    return thread_id


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


async def test_a_response_past_the_real_limit_is_named_and_the_next_read_still_works(fake_server):
    """The 2026-09-17 failure, at the ceiling it actually hit.

    The host answered a history read with 82,078,441 bytes against 16,777,216 and the caller was
    handed "App Server transport failed: ConnectionClosedError", which reads like a broken thread
    rather than a response nobody could receive. The limit is deliberately the real one here: the
    number is the point.
    """
    fake, path = fake_server
    thread_id = one_thread(fake)
    client = AppServer(path)
    try:
        fake.oversize = lambda method, params: (
            17 * 1024 * 1024 if method == "thread/turns/list" else None
        )
        with pytest.raises(ResponseTooLarge) as refused:
            await client.call("thread/turns/list", {"threadId": thread_id, "limit": 10})
        error = refused.value
        assert isinstance(error, TransportError)
        assert error.limit == MAX_FRAME_BYTES == 16 * 1024 * 1024
        assert error.frame_bytes is not None and error.frame_bytes > error.limit
        assert "1009" in str(error) and str(error.frame_bytes) in str(error)
        # One request, and no second one: a response too big to take is not a reason to ask again.
        assert fake.count("thread/turns/list") == 1
        fake.oversize = lambda method, params: None
        assert await client.call("thread/goal/get", {"threadId": thread_id}) == {"goal": None}
    finally:
        await client.close()


async def test_an_oversized_frame_is_not_blamed_on_whatever_was_pending(fake_server):
    """A frame is refused from its header, and the one here is a notification with no id at all.

    Counting what was pending would name a culprit the transport cannot identify, and the one
    request outstanding here did not cause this.
    """
    fake, path = fake_server
    client = AppServer(path, max_frame_bytes=64 * 1024)
    try:
        fake.oversize_before = {"thread/goal/get": [100 * 1024]}
        with pytest.raises(ResponseTooLarge) as refused:
            await client.call("thread/goal/get", {"threadId": "thread-1"})
        error = refused.value
        assert error.methods == ("thread/goal/get",)
        assert "cannot be attributed to a request" in str(error)
        assert "in flight: thread/goal/get" in str(error)
    finally:
        await client.close()


async def test_a_peer_refusing_our_frame_is_not_our_own_receive_limit(fake_server):
    """Same close code, same reason text; only which side sent it differs, and that decides it."""
    fake, path = fake_server
    client = AppServer(path)
    try:
        fake.peer_close = "thread/goal/get"
        with pytest.raises(TransportError) as failed:
            await client.call("thread/goal/get", {"threadId": "thread-1"})
        assert not isinstance(failed.value, ResponseTooLarge)
    finally:
        await client.close()


async def test_an_item_read_is_an_observation_like_every_other_read(fake_server):
    """Reading a turn's items asks the host a question; losing the answer changed nothing."""
    fake, path = fake_server
    thread_id = one_thread(fake)
    client = AppServer(path, timeout=1)
    try:
        with recording() as effects:
            await client.call("thread/items/list", {"threadId": thread_id, "limit": 1})
        assert effects.attempted == []
        assert effects.observed == ["initialize", "thread/items/list"]
    finally:
        await client.close()
