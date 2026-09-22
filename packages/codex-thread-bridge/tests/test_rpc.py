import asyncio
import contextlib
import json
import tempfile
from pathlib import Path

import pytest
from websockets.asyncio.server import unix_serve

from codex_thread_bridge.effects import recording
from codex_thread_bridge.rpc import (
    MAX_FRAME_BYTES,
    AppServer,
    PhaseBounds,
    PhaseTimeout,
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


# ---------------------------------------------------------------- transfer phases (CRW-20)
#
# One request is three waits on three different things, and until these bounds existed they
# shared one budget held by the caller. That is what let a slow connection establishment for one
# recipient spend the budget a DIFFERENT recipient needed to finish its own send: connect()
# serialises on _connect_lock, so part of every establishment is other callers' reconnections.


async def test_establishment_is_bounded_on_its_own_and_names_the_phase(fake_server):
    """A handshake that never completes costs the caller its establish bound and nothing else."""
    fake, path = fake_server
    fake.pause_after = "initialize"
    client = AppServer(path, timeout=5, phase_bounds=PhaseBounds(establish=0.2, transmit=5, ack=5))
    try:
        with recording() as effects:
            with pytest.raises(PhaseTimeout) as caught:
                await client.call("thread/read", {"threadId": "thread-1"})
        assert caught.value.phase == "establish"
        assert caught.value.method == "thread/read"
        # The requested frame was never written, which is the whole claim the receipt makes.
        assert effects.attempted == []
        assert "no thread/read frame was sent" in str(caught.value)
    finally:
        fake.release.set()
        await client.close()


async def test_a_stalled_establishment_does_not_spend_another_call_s_send_budget(fake_server):
    """The residue CRW-20 records, as a test.

    A is inside connect() holding _connect_lock. B is a different recipient's call on the same
    client - the relay builds exactly one AppServer - so B queues behind A's handshake. Before
    the phases were bounded that wait was charged to B's single budget, and B could arrive at
    turn/start with nothing left. Now it costs B its establish bound, B provably writes nothing,
    and the transport is still usable the moment the stall clears.
    """
    fake, path = fake_server
    fake.pause_after = "initialize"
    client = AppServer(path, timeout=5, phase_bounds=PhaseBounds(establish=0.2, transmit=5, ack=5))
    try:
        stalled = asyncio.create_task(client.call("thread/goal/get", {"threadId": "thread-1"}))
        await asyncio.wait_for(fake.paused.wait(), 5)

        started = asyncio.get_running_loop().time()
        with recording() as effects:
            with pytest.raises(PhaseTimeout) as caught:
                await client.call("thread/read", {"threadId": "thread-1"})
        elapsed = asyncio.get_running_loop().time() - started

        assert caught.value.phase == "establish"
        assert effects.attempted == []
        # Causal, not a stopwatch: B never started a handshake of its own. The only initialize
        # on the wire is A's, so every second B waited was spent queued behind A on
        # _connect_lock - which is precisely the wait that used to come out of B's send budget.
        assert fake.count("initialize") == 1
        # Bounded by the phase it was actually waiting in, not by the three phases of three
        # requests it would have been charged under one fused budget.
        assert elapsed < 3 * PhaseBounds(0.2, 5, 5).per_request

        with pytest.raises(PhaseTimeout):
            await stalled
    finally:
        fake.release.set()
        await client.close()

    # And the stall was not terminal: a fresh client on the same socket completes normally.
    recovered = AppServer(path, timeout=5)
    try:
        assert await recovered.call("thread/goal/get", {"threadId": "thread-1"}) == {"goal": None}
    finally:
        await recovered.close()


async def test_the_acknowledgement_bound_keeps_its_do_not_resend_wording(fake_server):
    """The response wait was always bounded. It is now attributable as well, and says the same."""
    fake, path = fake_server
    one_thread(fake)
    fake.pause_after = "thread/read"
    client = AppServer(path, timeout=5, phase_bounds=PhaseBounds(establish=5, transmit=5, ack=0.2))
    try:
        with pytest.raises(TransportError, match="do not resend") as caught:
            await client.call("thread/read", {"threadId": "thread-1"})
        assert isinstance(caught.value, PhaseTimeout)
        assert caught.value.phase == "ack"
    finally:
        fake.release.set()
        await client.close()


async def test_a_write_that_never_drains_is_bounded_and_retires_its_connection(fake_server):
    """The one phase that had no bound at all.

    websockets only yields inside send() when the write buffer is full, and its own guidance is
    that a cancelled send must not reuse the connection - a partial frame corrupts the stream
    every recipient shares. So the bound retires the socket rather than writing the next request
    into it.
    """
    _, path = fake_server
    client = AppServer(path, timeout=5, phase_bounds=PhaseBounds(establish=5, transmit=0.2, ack=5))
    try:
        await client.connect()
        live = client._ws

        class Blocked:
            """The live connection, with a write that never drains."""

            def __init__(self, inner):
                self._inner = inner
                self.closed = False

            async def send(self, _payload):
                await asyncio.Event().wait()

            async def close(self):
                self.closed = True
                await self._inner.close()

            def __aiter__(self):
                return self._inner.__aiter__()

        blocked = Blocked(live)
        client._ws = blocked

        with pytest.raises(PhaseTimeout) as caught:
            await client.call("thread/read", {"threadId": "thread-1"})

        assert caught.value.phase == "transmit"
        assert "do not resend" in str(caught.value)
        assert blocked.closed, "a cancelled partial write left the shared socket in use"
        assert client._ws is None, "the retired connection is still the current one"
    finally:
        await client.close()


async def test_retiring_a_connection_spares_the_one_that_replaced_it(fake_server):
    """Retirement is by identity, because the attributes it clears are rebuilt behind its back.

    connect() can build a replacement while a retirement is suspended in close(). A retirement
    that cleared the attributes unconditionally would erase that replacement and leave every
    later request writing to a socket nobody reads.
    """
    _, path = fake_server
    client = AppServer(path, timeout=5)
    try:
        await client.connect()
        stale_ws, stale_reader = client._ws, client._reader

        # Force a rebuild, exactly as a dropped reader does in production.
        stale_reader.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await stale_reader
        await client.connect()
        live_ws, live_reader = client._ws, client._reader
        assert live_ws is not stale_ws

        await client._retire(stale_ws, stale_reader)

        assert client._ws is live_ws, "a stale retirement erased the live connection"
        assert client._reader is live_reader
        assert await client.call("thread/goal/get", {"threadId": "thread-1"}) == {"goal": None}
    finally:
        await client.close()


async def test_a_retired_reader_fails_only_the_requests_it_was_carrying(fake_server):
    """_pending outlives any one connection, so its finaliser has to know which rows are its own.

    Without the connection tag the unwinding reader fails every outstanding entry, including the
    initialize the replacement has just registered - the requests survive the pointer fix and die
    to this one instead.
    """
    _, path = fake_server
    client = AppServer(path, timeout=5)
    try:
        await client.connect()
        reader, carried_by = client._reader, client._ws
        replacement = object()

        loop = asyncio.get_running_loop()
        mine, theirs = loop.create_future(), loop.create_future()
        client._pending[9001] = ("thread/read", mine, carried_by)
        client._pending[9002] = ("initialize", theirs, replacement)

        reader.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await reader

        assert mine.done() and isinstance(mine.exception(), TransportError)
        assert not theirs.done(), "the replacement's request was failed by the old reader"
    finally:
        client._pending.clear()
        await client.close()


async def test_a_cancelled_write_detaches_its_connection_too(fake_server):
    """Our own bound is not the only thing that cuts a write short.

    The relay's submission backstop and its shutdown both cancel an in-flight send, and that
    arrives as CancelledError rather than TimeoutError. The frame is just as half-written, so
    the connection has to stop being the current one either way - otherwise the next recipient
    writes a request into a stream whose last frame may be a fragment.
    """
    _, path = fake_server
    client = AppServer(path, timeout=5)
    try:
        await client.connect()
        live, reader = client._ws, client._reader
        writing = asyncio.Event()

        class Blocked:
            def __init__(self, inner):
                self._inner = inner

            async def send(self, _payload):
                writing.set()
                await asyncio.Event().wait()

            async def close(self):
                await self._inner.close()

            def __aiter__(self):
                return self._inner.__aiter__()

        client._ws = Blocked(live)

        request = asyncio.create_task(client.call("thread/read", {"threadId": "thread-1"}))
        await asyncio.wait_for(writing.wait(), 5)
        request.cancel()
        with pytest.raises(asyncio.CancelledError):
            await request

        assert client._ws is None, "a cancelled write left its connection in use"
        assert client._reader is not reader or client._reader is None
    finally:
        await client.close()


async def test_a_handshake_phase_expiry_is_reported_as_the_caller_s_establishment(fake_server):
    """connect() runs initialize under the ordinary bounds, and the caller must not inherit them.

    With an ack bound shorter than establish, a slow initialize expires in "ack" against a method
    the caller never asked for. Reported that way it would tell the delivery layer that a
    thread/read which never started might have been delivered.
    """
    fake, path = fake_server
    fake.pause_after = "initialize"
    client = AppServer(path, timeout=5, phase_bounds=PhaseBounds(establish=5, transmit=5, ack=0.2))
    try:
        with recording() as effects:
            with pytest.raises(PhaseTimeout) as caught:
                await client.call("thread/read", {"threadId": "thread-1"})

        assert caught.value.phase == "establish", "the caller inherited the handshake's own phase"
        assert caught.value.method == "thread/read"
        assert "no thread/read frame was sent" in str(caught.value)
        # The inner phase is kept rather than dropped: an operator still sees where it stopped.
        assert "ack phase" in str(caught.value)
        assert effects.attempted == []
    finally:
        fake.release.set()
        await client.close()

async def test_a_cancelled_write_does_not_pay_for_the_close_handshake(fake_server):
    """Detachment is immediate; the teardown is this client's own business.

    The caller that cancelled is already waiting out its own deadline - the relay's submission
    backstop, or its shutdown drain - and awaiting the close inside the handler would spend the
    peer's close handshake, up to close_timeout, on top of the bound those two advertise.
    """
    _, path = fake_server
    client = AppServer(path, timeout=5)
    try:
        await client.connect()
        live = client._ws
        writing, closing = asyncio.Event(), asyncio.Event()

        class SlowToClose:
            def __init__(self, inner):
                self._inner = inner

            async def send(self, _payload):
                writing.set()
                await asyncio.Event().wait()

            async def close(self):
                await closing.wait()
                await self._inner.close()

            def __aiter__(self):
                return self._inner.__aiter__()

        client._ws = SlowToClose(live)

        request = asyncio.create_task(client.call("thread/read", {"threadId": "thread-1"}))
        await asyncio.wait_for(writing.wait(), 5)
        request.cancel()

        started = asyncio.get_running_loop().time()
        with pytest.raises(asyncio.CancelledError):
            await request
        elapsed = asyncio.get_running_loop().time() - started

        # The close has not even been allowed to begin, and the caller is already back.
        assert elapsed < 1, f"the cancelled caller waited {elapsed}s on someone else's teardown"
        assert client._ws is None, "the socket was still reachable after a cancelled write"
        assert client._retiring, "the teardown was dropped rather than handed off"

        # And it is owned rather than orphaned: close() drains it.
        closing.set()
        await client.close()
        assert not client._retiring
    finally:
        closing.set()
        await client.close()

