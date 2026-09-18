"""A multiplexed JSON-RPC client over the documented Unix WebSocket transport."""

import asyncio
import contextlib
import json
import re
from pathlib import Path
from typing import Any

from websockets.asyncio.client import unix_connect
from websockets.exceptions import ConnectionClosed

from . import __version__
from .effects import mark_sent

# The largest response frame this client will buffer. It is a ceiling, not a target: raising it
# would only move the failure and would not touch the real cost, which the host pays building the
# response whether or not we accept it. A 754 MB page measured on a real thread took the host
# 96.7 s before a single byte reached us.
MAX_FRAME_BYTES = 16 * 1024 * 1024

# websockets writes the size into the close reason and nowhere else. For a fragmented message the
# reason reads "frame with N bytes after reading K bytes exceeds limit of M bytes", so the first
# integer is the frame and the last is our own limit; only the first is taken, and it is reported
# as that frame rather than as the size of a whole response.
_FRAME_BYTES = re.compile(r"frame with (\d+) bytes")


class RpcError(Exception):
    def __init__(self, method: str, error: dict[str, Any]):
        self.method = method
        self.error = error
        super().__init__(f"{method}: {error.get('message', error)}")


class TransportError(Exception):
    """A request may have reached the server. Mutations must not be retried."""


class ResponseTooLarge(TransportError):
    """A response frame past this client's limit, which took the connection down with it.

    It names no culprit, because it cannot. A frame is refused from its header, before any id
    inside it has been read, and it may be a notification carrying no id at all. `methods` is
    what happened to be in flight when the connection went down: context for a reader, never an
    accusation against one of them. A caller may ask again with a narrower query — every read on
    this transport is safe to repeat, and what a mutation may do is decided by the effects that
    were recorded, exactly as it is for any other transport failure.
    """

    def __init__(self, frame_bytes: int | None, limit: int, methods=()):
        self.frame_bytes = frame_bytes
        self.limit = limit
        self.methods = tuple(methods)
        size = f"{frame_bytes} bytes" if frame_bytes is not None else "an unreported size"
        in_flight = ", ".join(self.methods) or "nothing"
        super().__init__(
            f"App Server sent a response frame of {size}, past this client's {limit} byte "
            f"limit, and the connection closed with 1009. The frame was refused before its id "
            f"was read, so it cannot be attributed to a request; in flight: {in_flight}. "
            f"Ask again with a narrower query."
        )


def refused_frame(error: ConnectionClosed):
    """Whether we refused an oversized frame, and how big it was.

    Measured on websockets 15.0.1 and 17.1 alike: refusing a frame closes with 1009 and leaves
    the parser's PayloadTooBig unreachable, because the asyncio layer raises the close exception
    "from self.recv_exc", which is None for a parser failure and therefore erases __cause__. The
    size survives only in the close reason we sent.

    `rcvd` has to be absent. A 1009 arriving from the peer is the opposite event — the host
    refusing a frame of ours — and reading that as our own receive limit would send a caller
    looking for a smaller query when the problem is what it sent.
    """
    sent = getattr(error, "sent", None)
    if sent is None or sent.code != 1009 or getattr(error, "rcvd", None) is not None:
        return False, None
    found = _FRAME_BYTES.search(sent.reason or "")
    return True, int(found.group(1)) if found else None


class AppServer:
    def __init__(
        self, socket_path: Path, timeout: float = 20, *, max_frame_bytes: int = MAX_FRAME_BYTES
    ):
        self.socket_path = socket_path
        self.timeout = timeout
        self.max_frame_bytes = max_frame_bytes
        self._ws = None
        self._reader = None
        # The method is kept beside the future so a failure that cannot say which request it
        # belongs to can at least say what was outstanding.
        self._pending: dict[int, tuple[str, asyncio.Future]] = {}
        self._counter = 0
        self._connect_lock = asyncio.Lock()
        self.info: dict[str, Any] = {}

    async def connect(self):
        async with self._connect_lock:
            if self._reader is not None and not self._reader.done():
                return
            await self.close()
            try:
                self._ws = await unix_connect(
                    str(self.socket_path),
                    uri="ws://localhost/",
                    open_timeout=self.timeout,
                    close_timeout=2,
                    max_size=self.max_frame_bytes,
                    # Codex 0.153.4 closes Unix handshakes offering permessage-deflate.
                    compression=None,
                )
                self._reader = asyncio.create_task(self._receive())
                self.info = await self._request(
                    "initialize",
                    {
                        "clientInfo": {"name": "codex_thread_bridge", "version": __version__},
                        "capabilities": {"experimentalApi": True},
                    },
                )
                await self._ws.send(json.dumps({"method": "initialized", "params": {}}))
            except BaseException:
                await self.close()
                raise

    async def _receive(self):
        failure = "App Server disconnected"
        oversized, frame_bytes = False, None
        ws = self._ws
        assert ws is not None
        try:
            async for raw in ws:
                message = json.loads(raw)
                if "method" in message:
                    if "id" in message:
                        # No silent approvals or fake results for client-side tools.
                        await ws.send(
                            json.dumps(
                                {
                                    "id": message["id"],
                                    "error": {
                                        "code": -32601,
                                        "message": "Unsupported client action; "
                                        "continue this task in Desktop",
                                    },
                                }
                            )
                        )
                    # Reads and waits query the server; no unbounded event history.
                    continue
                waiting = self._pending.get(message.get("id"))
                future = waiting[1] if waiting is not None else None
                if future is not None and not future.done():
                    future.set_result(message)
        except asyncio.CancelledError:
            raise
        except ConnectionClosed as error:
            oversized, frame_bytes = refused_frame(error)
            if not oversized:
                failure = f"App Server transport failed: {type(error).__name__}: {error}"
        except Exception as error:
            failure = f"App Server transport failed: {type(error).__name__}: {error}"
        finally:
            waiting = list(self._pending.values())
            methods = tuple(method for method, _ in waiting)
            for _, future in waiting:
                if not future.done():
                    future.set_exception(
                        ResponseTooLarge(frame_bytes, self.max_frame_bytes, methods)
                        if oversized
                        else TransportError(failure)
                    )

    async def _request(self, method: str, params: dict[str, Any]):
        ws = self._ws
        if ws is None:
            raise TransportError("App Server is not connected")
        self._counter += 1
        ident = self._counter
        future = asyncio.get_running_loop().create_future()
        self._pending[ident] = (method, future)
        try:
            # Recorded before the write and not after it: a failure inside send does not prove
            # the frame never reached the server, and this is the only place that knows one was
            # about to go out for this method. A request that never gets this far — no socket, no
            # connection — leaves no mark, which is what lets a caller try it again.
            mark_sent(method)
            await ws.send(json.dumps({"id": ident, "method": method, "params": params}))
            message = await asyncio.wait_for(future, self.timeout)
        except (OSError, TimeoutError, ConnectionClosed) as error:
            raise TransportError(f"{method}: response unavailable; do not resend") from error
        finally:
            self._pending.pop(ident, None)
            if not future.done():
                future.cancel()
            elif not future.cancelled():
                # The reader may have settled this future while send was failing. Retrieving the
                # exception keeps asyncio from reporting it as one nobody ever looked at.
                future.exception()
        if "error" in message:
            raise RpcError(method, message["error"])
        if "result" not in message:
            raise TransportError(f"{method}: invalid response; outcome unknown")
        return message["result"]

    async def call(self, method: str, params: dict[str, Any]):
        await self.connect()
        # Reconnect before a new request, never retry an already-sent request.
        return await self._request(method, params)

    async def close(self):
        if self._ws is not None:
            await self._ws.close()
            self._ws = None
        if self._reader is not None:
            self._reader.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._reader
            self._reader = None
