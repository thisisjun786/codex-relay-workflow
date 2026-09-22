"""A multiplexed JSON-RPC client over the documented Unix WebSocket transport."""

import asyncio
import contextlib
import json
import re
import time
from collections import deque
from dataclasses import dataclass
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

# The server-to-client requests that ask a human to decide, from
# `codex app-server generate-json-schema --experimental` on codex-cli 0.154.0. They are named so a
# receipt can say which refusals were a decision somebody was waiting on, rather than lumping
# them in with an ordinary unsupported client call.
APPROVAL_METHODS = frozenset(
    {
        "execCommandApproval",
        "applyPatchApproval",
        "item/commandExecution/requestApproval",
        "item/fileChange/requestApproval",
        "item/permissions/requestApproval",
        "mcpServer/elicitation/request",
        "item/tool/requestUserInput",
    }
)

# How many refused server-to-client requests this connection remembers. Bounded, because a
# long-lived connection would otherwise grow one list forever; large enough to cover the requests
# a single dispatch can provoke.
REFUSED_REQUESTS_KEPT = 64


class RpcError(Exception):
    def __init__(self, method: str, error: dict[str, Any]):
        self.method = method
        self.error = error
        super().__init__(f"{method}: {error.get('message', error)}")


class TransportError(Exception):
    """A request may have reached the server. Mutations must not be retried."""


@dataclass(frozen=True)
class PhaseBounds:
    """One finite bound per transfer phase, because a shared budget is not isolation.

    One request is three waits, and they wait on different things. Establishment queues behind
    `_connect_lock`, so part of it is OTHER callers' reconnections. The write waits on the
    socket buffer draining. The acknowledgement waits on the host. Charging all three to one
    budget is what let a slow establishment for one recipient spend the budget a different
    recipient needed to finish its own send, so each gets its own bound and its own name.
    """

    establish: float
    transmit: float
    ack: float

    @classmethod
    def from_timeout(cls, timeout: float) -> "PhaseBounds":
        """One RPC timeout per phase, which is what the stage count always meant."""
        return cls(establish=timeout, transmit=timeout, ack=timeout)

    @property
    def per_request(self) -> float:
        return self.establish + self.transmit + self.ack


class PhaseTimeout(TransportError):
    """A transfer phase outlived its own bound, and says which one.

    A TransportError subclass rather than a sibling: a caller that cannot tell the phases apart
    keeps the conservative reading it already had. The message stays "<method>: <detail>", which
    is the shape callers parse to attribute a failure to the request it belongs to.
    """

    def __init__(self, method: str, phase: str, seconds: float, detail: str):
        self.method = method
        self.phase = phase
        self.seconds = seconds
        super().__init__(f"{method}: {detail}")


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
        self, socket_path: Path, timeout: float = 20, *, max_frame_bytes: int = MAX_FRAME_BYTES,
        phase_bounds: PhaseBounds | None = None,
    ):
        self.socket_path = socket_path
        self.timeout = timeout
        self.max_frame_bytes = max_frame_bytes
        # None means "follow whatever timeout currently says". Deliberately not resolved here:
        # callers set .timeout after construction, and a snapshot would ignore them silently.
        self._phase_bounds = phase_bounds
        self._ws = None
        self._reader = None
        # The method is kept beside the future so a failure that cannot say which request it
        # belongs to can at least say what was outstanding, and the connection is kept beside
        # both because this dict outlives any one of them: a reader unwinding must fail its own
        # connection's requests and not the replacement's.
        self._pending: dict[int, tuple[str, asyncio.Future, Any]] = {}
        self._counter = 0
        self._connect_lock = asyncio.Lock()
        self.info: dict[str, Any] = {}
        # Every server-to-client request this connection refused, newest last and bounded.
        # Kept because "this bridge granted no approval" should be evidence a caller can read,
        # not a claim it has to take on trust.
        self._refused = deque(maxlen=REFUSED_REQUESTS_KEPT)
        # Monotonic across the whole connection, so a caller can mark a point and ask only about
        # refusals recorded after it. The deque's own length cannot do that once it wraps.
        self.refused_total = 0

    def refusal_mark(self):
        """A point in the refusal stream, to be handed back to refusals_since."""
        return self.refused_total

    @property
    def phase_bounds(self) -> PhaseBounds:
        """The bound each transfer phase gets, derived from timeout unless one was injected."""
        if self._phase_bounds is not None:
            return self._phase_bounds
        return PhaseBounds.from_timeout(self.timeout)

    def refusals_since(self, mark: int, thread_id: str | None = None):
        """Refused server-to-client requests recorded after `mark`, attributed where possible.

        Split rather than filtered. One connection serves sequential dispatches, so counting every
        recent refusal as this thread's would let another thread's denied command appear on this
        receipt. A request carrying no threadId cannot be attributed at all, and saying so is more
        useful than quietly dropping it or quietly claiming it.
        """
        recent = [entry for entry in self._refused if entry["index"] > mark]
        mine = [entry for entry in recent if entry["threadId"] == thread_id]
        unattributed = [entry for entry in recent if entry["threadId"] is None]
        return {
            "thisThread": mine,
            "unattributed": unattributed,
            "otherThreads": len(recent) - len(mine) - len(unattributed),
            "approvalsRefusedForThisThread": sum(entry["approval"] for entry in mine),
            "note": "Refusals recorded between the two marks this receipt spans. A turn outlives "
            "that window, so a request the turn raises later is refused the same way and is "
            "simply not on this receipt. Nothing here was ever granted.",
        }

    def _record_refusal(self, message: dict[str, Any]):
        """Note a refused server-to-client request, keyed by what it says it is about."""
        params = message.get("params")
        params = params if isinstance(params, dict) else {}
        method = message.get("method")
        thread_id = params.get("threadId")
        self.refused_total += 1
        self._refused.append(
            {
                "index": self.refused_total,
                "method": method,
                "approval": method in APPROVAL_METHODS,
                "threadId": thread_id if isinstance(thread_id, str) else None,
                "turnId": params.get("turnId") if isinstance(params.get("turnId"), str) else None,
                "answered": "refused",
                "at": time.time(),
            }
        )

    async def connect(self):
        async with self._connect_lock:
            if self._reader is not None and not self._reader.done():
                return
            await self.close()
            try:
                ws = await unix_connect(
                    str(self.socket_path),
                    uri="ws://localhost/",
                    open_timeout=self.timeout,
                    close_timeout=2,
                    max_size=self.max_frame_bytes,
                    # Codex 0.153.4 closes Unix handshakes offering permessage-deflate.
                    compression=None,
                )
                self._ws = ws
                # The reader is told which connection it serves. Reading it back off self would
                # race a retirement that has already cleared the attribute.
                self._reader = asyncio.create_task(self._receive(ws))
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

    async def _receive(self, ws):
        failure = "App Server disconnected"
        oversized, frame_bytes = False, None
        try:
            async for raw in ws:
                message = json.loads(raw)
                if "method" in message:
                    if "id" in message:
                        # No silent approvals or fake results for client-side tools. The refusal
                        # is recorded first, so that "this bridge granted nothing" is evidence a
                        # receipt can carry rather than an assurance. The answer itself is
                        # unchanged on purpose: -32601 says this client does not implement the
                        # method, which declines to decide. Answering in the approval vocabulary
                        # instead -- denied, decline -- would write a verdict into the thread's
                        # record in the approver's own type, and make this bridge the approver.
                        self._record_refusal(message)
                        await ws.send(
                            json.dumps(
                                {
                                    "id": message["id"],
                                    "error": {
                                        "code": -32601,
                                        "message": "Unsupported client action; this bridge "
                                        "services no approval or client-side tool and has no "
                                        "route to this thread's approver. Nothing was granted. "
                                        "Continue this task in the client that owns the thread.",
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
            # This connection's requests only. _pending is shared across connections, so a
            # reader that failed every entry would kill the REPLACEMENT's requests too:
            # connect() can register a new socket's initialize while this coroutine is still
            # unwinding. What retiring a connection may fail is what it was actually carrying.
            carried = [entry for entry in self._pending.values() if entry[2] is ws]
            methods = tuple(method for method, _, _ in carried)
            for _, future, _ in carried:
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
        # Captured with the socket, so a retirement below discards the connection this request
        # actually used rather than whatever happens to be current by the time it runs.
        reader = self._reader
        bounds = self.phase_bounds
        self._counter += 1
        ident = self._counter
        future = asyncio.get_running_loop().create_future()
        self._pending[ident] = (method, future, ws)
        try:
            # Recorded before the write and not after it: a failure inside send does not prove
            # the frame never reached the server, and this is the only place that knows one was
            # about to go out for this method. A request that never gets this far — no socket, no
            # connection — leaves no mark, which is what lets a caller try it again.
            mark_sent(method)
            try:
                await asyncio.wait_for(
                    ws.send(json.dumps({"id": ident, "method": method, "params": params})),
                    bounds.transmit,
                )
            except TimeoutError as error:
                # The write is the only phase that was never bounded, and it is the one that can
                # hold a recipient forever: send() yields exactly when the buffer is full.
                # websockets says so itself — "Canceling send() is discouraged. Instead, you
                # should close the connection" — because a cancelled write leaves a partial
                # frame, and the stream it corrupts is shared by every recipient. So the
                # connection is retired rather than reused, by identity, and the next request
                # rebuilds. The mark above stands: this may have reached the server.
                await self._retire(ws, reader)
                raise PhaseTimeout(
                    method, "transmit", bounds.transmit,
                    f"the request frame did not drain within {bounds.transmit}s and the"
                    " connection was retired; response unavailable; do not resend",
                ) from error
            message = await asyncio.wait_for(future, bounds.ack)
        except TimeoutError as error:
            raise PhaseTimeout(
                method, "ack", bounds.ack, "response unavailable; do not resend"
            ) from error
        except (OSError, ConnectionClosed) as error:
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
        bounds = self.phase_bounds
        # Establishment is bounded on its own because it is the phase that is NOT this caller's
        # alone: connect() serialises on _connect_lock, so part of this wait is other recipients
        # rebuilding. Folded into one budget with the write and the response, a queue of their
        # reconnections spent the budget this send needed, and the deadline then fired on a send
        # that was still healthy. It now costs this caller its establish bound and no more.
        try:
            await asyncio.wait_for(self.connect(), bounds.establish)
        except TimeoutError as error:
            raise PhaseTimeout(
                method, "establish", bounds.establish,
                f"connection establishment exceeded {bounds.establish}s;"
                f" no {method} frame was sent",
            ) from error
        # Reconnect before a new request, never retry an already-sent request.
        return await self._request(method, params)

    async def close(self):
        await self._retire(self._ws, self._reader)

    async def _retire(self, ws, reader):
        """Discard exactly one connection, and never whatever replaced it.

        Both attributes are rebuilt by connect() while this coroutine is suspended in the close
        below, so clearing them afterwards can erase a live replacement and leave every later
        request writing to a socket nobody reads. They are therefore cleared BEFORE the awaits,
        and only while they still name the connection being retired.
        """
        if self._ws is ws:
            self._ws = None
        if self._reader is reader:
            self._reader = None
        if ws is not None:
            await ws.close()
        if reader is not None:
            reader.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await reader
