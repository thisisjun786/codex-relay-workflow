"""The real host adapter, over the read-only transport bridge.

The bridge is a pinned library and is never modified. Everything here is either one of its
supported operations or a plain read of the App Server, and the RPC surface is injectable so the
whole adapter is testable without a socket.

Two behaviours are worth reading closely, because both were wrong in an earlier design.

Archive discovery resolves by EXACT TASK ID. A listing filtered by cwd can miss a task whose cwd
was corrected after creation, so a miss there is never absence: the search falls back to an
unfiltered scan, and it resumes from a stored cursor so successive checks make progress instead of
re-reading the same prefix. When nothing conclusive is found the answer is None, meaning unknown,
which withholds delivery rather than guessing.

Item paging uses the forward cursor. The reverse cursor exists to change direction, and using it to
continue a descending scan re-serves the newest page forever.
"""

import hashlib
import json

from .hostadapter import HostUnavailable, ThreadFacts, TokenScan, TurnInfo

UNARCHIVED_CWD = "unarchived_cwd"
UNARCHIVED_ALL = "unarchived_all"
ARCHIVED = "archived"
PAGE = 50
MAX_PAGES_PER_CHECK = 4


class BridgeHostAdapter:
    def __init__(self, socket_path=None, *, call=None, ledger_get=None, store=None, clock=None,
                 timeout: float = 20.0, page: int = PAGE, **transport_options):
        """call(method, params) -> dict is the only way this class reaches the host.

        Supplying it directly is how the tests drive the real logic without a socket. Omitting
        it builds the transport from the pinned bridge library, imported lazily so that importing
        this package never requires it.
        """
        self.page = page
        self.store = store
        self.clock = clock
        self._bridge = None
        self._runner = None
        self._transport = None
        if call is not None:
            self._call = call
            self._ledger_get = ledger_get or (lambda request_id: None)
            self._send = None
            return
        self._transport = _Transport(socket_path, timeout, **transport_options)
        self._call = self._transport.call
        self._ledger_get = self._transport.ledger_get
        self._send = self._transport.send

    # ------------------------------------------------------------------ reads

    def read_thread(self, thread_id: str) -> ThreadFacts:
        result = self._call("thread/read", {"threadId": thread_id})
        thread = result.get("thread") or {}
        status = (thread.get("status") or {}).get("type", "unknown")
        return ThreadFacts(status, thread.get("canAcceptDirectInput"))

    def read_goal_status(self, thread_id: str):
        result = self._call("thread/goal/get", {"threadId": thread_id})
        goal = result.get("goal")
        # No goal is not a paused goal. It simply means the task has none.
        return goal.get("status") if isinstance(goal, dict) else None

    def list_turn_ids(self, thread_id: str, limit: int = 20) -> list:
        page = self._call(
            "thread/turns/list",
            {"threadId": thread_id, "limit": limit, "itemsView": "summary"},
        )
        return [turn["id"] for turn in page.get("data", [])]

    def read_turn(self, thread_id: str, turn_id: str):
        """Page until the turn is found or the listing is exhausted.

        A bounded miss RAISES rather than returning None. Returning None would say the turn does
        not exist, and admission would then reject a real continuation on the strength of a scan
        that simply stopped early.
        """
        cursor = None
        for _ in range(MAX_PAGES_PER_CHECK):
            params = {"threadId": thread_id, "limit": self.page, "itemsView": "summary"}
            if cursor:
                params["cursor"] = cursor
            page = self._call("thread/turns/list", params)
            for turn in page.get("data", []):
                if turn.get("id") == turn_id:
                    return TurnInfo(
                        turn["id"], turn.get("status", "unknown"), turn.get("startedAt")
                    )
            cursor = page.get("nextCursor")
            if not cursor:
                return None
        raise HostUnavailable(
            f"turn {turn_id!r} not found within {MAX_PAGES_PER_CHECK} pages; the listing was not "
            "exhausted, so this is not evidence of absence"
        )

    def is_archived(self, thread_id: str, *, cwd: str | None = None):
        """True, False, or None for unknown. Resolved by exact id, never by a filter's silence."""
        if cwd and self._scan_listing(thread_id, UNARCHIVED_CWD, {"archived": False, "cwd": cwd}):
            return False
        if self._scan_listing(thread_id, ARCHIVED, {"archived": True}):
            return True
        if self._scan_listing(thread_id, UNARCHIVED_ALL, {"archived": False}):
            return False
        return None

    def _scan_listing(self, thread_id: str, listing: str, filters: dict) -> bool:
        """One bounded, resumable pass. Returns True only on an exact id match."""
        cursor, exhausted = self._cursor(thread_id, listing)
        if exhausted:
            cursor = None  # a finished pass starts over; the task may exist now
        scanned = 0
        for _ in range(MAX_PAGES_PER_CHECK):
            params = {"limit": self.page, "useStateDbOnly": True, **filters}
            if cursor:
                params["cursor"] = cursor
            try:
                page = self._call("thread/list", params)
            except Exception as error:
                self._save_cursor(thread_id, listing, cursor, False, scanned, str(error))
                return False
            for thread in page.get("data", []):
                scanned += 1
                if thread.get("id") == thread_id:
                    self._save_cursor(thread_id, listing, None, True, scanned, None)
                    return True
            cursor = page.get("nextCursor")
            if not cursor:
                self._save_cursor(thread_id, listing, None, True, scanned, None)
                return False
        self._save_cursor(thread_id, listing, cursor, False, scanned, None)
        return False

    def _cursor(self, thread_id: str, listing: str):
        if self.store is None:
            return None, False
        row = self.store.one(
            "SELECT cursor, exhausted FROM discovery_cursors WHERE task_id = ? AND listing = ?",
            (thread_id, listing),
        )
        return (row["cursor"], bool(row["exhausted"])) if row else (None, False)

    def _save_cursor(self, thread_id, listing, cursor, exhausted, scanned, error) -> None:
        if self.store is None or self.clock is None:
            return
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO discovery_cursors (task_id, listing, cursor, exhausted, scanned,"
                " updated_at) VALUES (?,?,?,?,?,?)"
                " ON CONFLICT(task_id, listing) DO UPDATE SET cursor=excluded.cursor,"
                " exhausted=excluded.exhausted, scanned=excluded.scanned,"
                " updated_at=excluded.updated_at",
                (thread_id, listing, cursor, int(exhausted), scanned, self.clock.iso()),
            )

    def find_token(self, thread_id: str, token: str, *, limit: int = 200, turn_id=None) -> TokenScan:
        """Forward paging with the forward cursor, and honest exhaustion."""
        cursor, scanned = None, 0
        while scanned < limit:
            params = {
                "threadId": thread_id,
                "sortDirection": "desc",
                "limit": min(self.page, limit - scanned),
            }
            if turn_id:
                params["turnId"] = turn_id
            if cursor:
                params["cursor"] = cursor
            page = self._call("thread/items/list", params)
            entries = page.get("data", [])
            for entry in entries:
                scanned += 1
                if token in _item_text(entry):
                    return TokenScan(True, entry.get("turnId"), False, scanned)
            cursor = page.get("nextCursor")
            if not cursor:
                return TokenScan(False, None, True, scanned)
            if not entries:
                break
        return TokenScan(False, None, False, scanned)

    def recipient_fingerprint(self, thread_id: str, *, window: int = 8) -> str:
        """A content digest over the newest items, so an append to an existing item shows up."""
        page = self._call(
            "thread/items/list",
            {"threadId": thread_id, "sortDirection": "desc", "limit": window},
        )
        digest = hashlib.sha256()
        for entry in page.get("data", []):
            body = hashlib.sha256(_item_text(entry).encode("utf-8")).hexdigest()
            digest.update(f"{entry.get('turnId')}:{_item_id(entry)}:{body}|".encode())
        return digest.hexdigest()

    # ----------------------------------------------------------------- writes

    def send_message(self, request_id: str, thread_id: str, message: str, settings=None) -> dict:
        """Send under the authorized settings, or do not send.

        settings is required. The pinned bridge's own send resumes with {threadId, excludeTurns}
        and says in its source that no cwd, model, sandbox or reasoning overrides are supplied;
        on this host that resume returned dangerFullAccess for a task created workspaceWrite with
        networkAccess false. Falling back to that path when settings are absent would reintroduce
        exactly the behaviour this guards, so absence is refused rather than defaulted.
        """
        if self._send is None:
            raise HostUnavailable("this adapter was built read-only, with no transport to send on")
        if settings is None:
            raise HostUnavailable(
                "a send requires the authorized task settings; refusing to resume with host"
                " defaults"
            )
        return self._send(request_id, thread_id, message, settings)

    def close(self) -> None:
        """Shut the transport down: connection, ledger, loop and thread, in that order."""
        if self._transport is not None:
            self._transport.close()
            self._transport = None

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        self.close()

    def get_operation(self, request_id: str):
        # The transport ledger raises for an id it has never seen. That is an observation, not
        # an error: it means no operation was ever begun under this id.
        try:
            return self._ledger_get(request_id)
        except ValueError:
            return None


def _item_text(entry) -> str:
    item = entry.get("item") if isinstance(entry, dict) else None
    if isinstance(item, dict):
        for key in ("text", "preview", "summary", "aggregatedOutput"):
            if isinstance(item.get(key), str):
                return item[key]
        return json.dumps(item, sort_keys=True, default=str)
    return json.dumps(entry, sort_keys=True, default=str)


def _item_id(entry) -> str:
    item = entry.get("item") if isinstance(entry, dict) else None
    if isinstance(item, dict) and isinstance(item.get("id"), str):
        return item["id"]
    return str(entry.get("id", ""))


class _Transport:
    """Owns one worker thread, and everything that must live on it.

    Two constraints shape this. A sqlite connection belongs to the thread that created it, and
    the bridge's ledger is a sqlite connection the bridge touches from inside its coroutines, so
    building it on the caller's thread and using it on the worker's fails on the first send
    before a single RPC goes out. And on this host asyncio's cross-thread wakeup does not
    arrive: a coroutine submitted with run_coroutine_threadsafe to a run_forever loop in another
    thread never completes, measured on both CPython 3.13 and 3.14 here. So work is handed over
    through an ordinary queue that the worker polls, which depends on nothing but the loop's own
    timer.
    """

    POLL_SECONDS = 0.005

    def __init__(self, socket_path, timeout, *, app_server_factory=None, bridge_factory=None,
                 ledger_factory=None):
        import concurrent.futures
        import queue
        import threading

        self._futures = concurrent.futures
        self.timeout = timeout
        self._inbox = queue.Queue()
        self._stopping = False
        self._state = {}
        self._started = threading.Event()
        self._failure = None
        self.thread = threading.Thread(
            target=self._serve,
            args=(socket_path, app_server_factory, bridge_factory, ledger_factory),
            daemon=True,
        )
        self.thread.start()
        if not self._started.wait(timeout + 5):
            raise TimeoutError("the relay transport worker did not start")
        if self._failure is not None:
            raise self._failure

    # ------------------------------------------------------------- worker side

    def _serve(self, socket_path, app_server_factory, bridge_factory, ledger_factory):
        import asyncio

        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        try:
            loop.run_until_complete(
                self._worker(socket_path, app_server_factory, bridge_factory, ledger_factory)
            )
        except BaseException as error:  # noqa: BLE001 - surfaced to the constructor
            self._failure = error
            self._started.set()
        finally:
            try:
                loop.close()
            except Exception:
                pass

    async def _worker(self, socket_path, app_server_factory, bridge_factory, ledger_factory):
        import asyncio
        import queue

        try:
            await self._build(socket_path, app_server_factory, bridge_factory, ledger_factory)
        except BaseException as error:  # noqa: BLE001
            self._failure = error
            self._started.set()
            return
        self._started.set()
        while True:
            try:
                work, future = self._inbox.get_nowait()
            except queue.Empty:
                if self._stopping:
                    return
                await asyncio.sleep(self.POLL_SECONDS)
                continue
            if work is None:
                future.set_result(None)
                return
            try:
                result = await work()
            except BaseException as error:  # noqa: BLE001 - returned to the caller
                # Hand over the failure, but not this worker's own frame. The traceback
                # starts at the `await work()` line above, inside a coroutine that is
                # still suspended and still serving the queue. A caller is entitled to
                # clear the frames of what it catches, and unittest's assertRaises does
                # exactly that; on CPython 3.11 clearing this frame finalizes the worker
                # mid-flight, so every later submit waits out its timeout against a loop
                # that no longer reads its inbox. Dropping one frame keeps the type, the
                # message and every frame from inside the operation, which is what the
                # caller actually needs to debug it.
                # Through the built-in, never `error.with_traceback(...)`. A subclass can
                # override that method, and running its code here — inside the handler
                # whose whole job is to keep this worker alive — would reintroduce the
                # failure this guards against.
                inner = error.__traceback__
                BaseException.with_traceback(error, inner.tb_next if inner else None)
                future.set_exception(error)
            else:
                future.set_result(result)

    async def _build(self, socket_path, app_server_factory, bridge_factory, ledger_factory):
        from pathlib import Path

        from .store import state_dir

        if ledger_factory is None:
            from codex_thread_bridge.ledger import open_endpoint_ledger

            canonical, ledger = open_endpoint_ledger(Path(socket_path), state_dir(socket_path))
        else:
            canonical, ledger = ledger_factory()
        if app_server_factory is None:
            from codex_thread_bridge.rpc import AppServer

            rpc = AppServer(canonical, timeout=self.timeout)
        else:
            rpc = app_server_factory(canonical)
        if bridge_factory is None:
            from codex_thread_bridge.bridge import Bridge

            bridge = Bridge(rpc, ledger)
        else:
            bridge = bridge_factory(rpc, ledger)
        self._state.update(rpc=rpc, ledger=ledger, bridge=bridge)

    # ------------------------------------------------------------- caller side

    def _submit(self, work):
        if not self.thread.is_alive():
            raise RuntimeError("the relay transport worker is not running")
        future = self._futures.Future()
        self._inbox.put((work, future))
        return future.result(self.timeout + 10)

    def call(self, method, params):
        return self._submit(lambda: self._state["rpc"].call(method, params))

    def send(self, request_id, thread_id, message, settings):
        return self._submit(
            lambda: _guarded_send(
                self._state["rpc"], self._state["ledger"],
                request_id, thread_id, message, settings,
            )
        )

    def ledger_get(self, request_id):
        async def read():
            return self._state["ledger"].get(request_id)

        return self._submit(read)

    def close(self):
        if not self.thread.is_alive():
            return

        async def shutdown():
            rpc = self._state.get("rpc")
            if rpc is not None and hasattr(rpc, "close"):
                await rpc.close()
            ledger = self._state.get("ledger")
            if ledger is not None:
                ledger.close()

        try:
            self._submit(shutdown)
        finally:
            self._stopping = True
            future = self._futures.Future()
            self._inbox.put((None, future))
            self.thread.join(timeout=10)


class _Refusal(Exception):
    """A refusal decided locally, rendered exactly like an RPC error so one classifier reads both.

    The transport classifier keys on a method-prefixed error string and an rpcError.code, so a
    local refusal that wants the same treatment has to look the same on the wire.
    """

    def __init__(self, method: str, error: dict):
        self.method = method
        self.error = error
        super().__init__(f"{method}: {error.get('message', error.get('code', 'refused'))}")


async def _guarded_send(rpc, ledger, request_id, thread_id, message, settings):
    """The bridge's send sequence, with the authorized settings actually carried.

    Deliberately mirrors codex_thread_bridge.bridge.Bridge._mutate rather than calling it: the
    pinned send resumes with no overrides, and the bridge is never modified. Idempotency is the
    bridge's own ledger, with its exact operation name and argument fingerprint, so a replay is
    answered from the receipt and a reused id with different arguments is rejected by the ledger.

    The receipt stays byte-compatible with the bridge's: top-level status and turnId, resumed,
    rpcError, and a method-prefixed error.
    """
    import asyncio

    method = "send_message_to_thread"
    params = {"threadId": thread_id, "message": message}

    # Raises when the id was used with different arguments. That rejection is the point.
    retained = ledger.lookup(request_id, method, params)
    if retained is not None:
        return {**retained, "replayed": True}
    fresh, receipt = ledger.begin(request_id, method, params)
    if not fresh:
        return {**(receipt or {}), "replayed": True}

    try:
        from codex_thread_bridge.rpc import RpcError
    except ImportError:  # pragma: no cover - only when the pinned bridge is absent
        RpcError = _Refusal

    try:
        receipt["threadId"] = thread_id
        ledger.save(receipt)

        state = await rpc.call("thread/read", {"threadId": thread_id})
        if (state.get("thread") or {}).get("status", {}).get("type") == "active":
            raise _Refusal("thread/read", {
                "code": "thread_busy",
                "message": "Thread is active; message withheld. Wait for completion.",
            })

        # Resume carries the authorized settings. It is the DETECTOR: its response reports the
        # full derived policy, the effort and the environments, so a host that ignores overrides
        # is caught here, before anything has started.
        resumed = await rpc.call("thread/resume", settings.resume_params(thread_id))
        receipt["resumed"] = resumed
        ledger.save(receipt)

        findings = settings.mismatches(resumed)
        if findings:
            first = findings[0]
            receipt["settingsFindings"] = findings
            raise _Refusal("thread/resume", {
                "code": first["code"],
                "message": f"{first['code']}: {first['field']} returned"
                           f" {first.get('returned')!r}; message withheld",
            })

        # turn/start is the BINDER. It is the only call that accepts the full sandbox policy,
        # the effort and the environment selection, and the protocol scopes them to this turn
        # and subsequent turns. Its response defines only turn, so there is nothing to verify
        # here; that absence is a reported limit, not a failure.
        turn = await rpc.call("turn/start", {
            "threadId": thread_id,
            "input": [{"type": "text", "text": message}],
            **settings.start_overrides(),
        })
        receipt["turnId"] = turn["turn"]["id"]
        receipt["status"] = "accepted"
    except _Refusal as error:
        receipt.update(status="failed", error=str(error), rpcError=error.error)
    except RpcError as error:
        receipt.update(status="failed", error=str(error), rpcError=getattr(error, "error", None))
    except asyncio.CancelledError:
        # Cancelled after a start may already have delivered. Unknown, never non-delivery.
        ledger.save({**receipt, "status": "outcome_unknown"})
        raise
    except Exception as error:  # noqa: BLE001 - recorded, then classified by the transport
        receipt.update(status="outcome_unknown", error=f"{type(error).__name__}: {error}")
    return ledger.save(receipt)
