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

Which threads a listing contains depends on its source kinds, not only on its filters. With
sourceKinds omitted the App Server lists its interactive sources only (in 0.154.0: cli, vscode and
two custom sources), so a thread created by codex exec, including a child on the official worktree
path, is in no default listing at all. Two listings that name exec therefore run beside the
default ones. They are added rather than substituted, because no explicit list reproduces the
default: the custom sources have no source kind to name. The default listings keep their exact
parameters, so every thread they found before is found the same way.

Item paging uses the forward cursor. The reverse cursor exists to change direction, and using it to
continue a descending scan re-serves the newest page forever.
"""

import hashlib
import json
import os
import sqlite3
import stat
from pathlib import Path

from .hostadapter import HostUnavailable, ThreadFacts, TokenScan, TurnInfo
from .settings import SETTINGS_DIFFER_AFTER_LOAD, SETTINGS_NOT_PRESERVED

UNARCHIVED_CWD = "unarchived_cwd"
UNARCHIVED_ALL = "unarchived_all"
ARCHIVED = "archived"
# A source kind the default listing leaves out, and the listings that ask for it. Each keeps its
# own resumable cursor under its own key in discovery_cursors.
EXEC_SOURCE_KINDS = ("exec",)
ARCHIVED_EXEC = "archived:exec"
UNARCHIVED_ALL_EXEC = "unarchived_all:exec"
PAGE = 50
MAX_PAGES_PER_CHECK = 4


class _ProcessPolicy:
    """Ask _build to use the execution policy this process already snapshotted."""


_PROCESS_POLICY = _ProcessPolicy()


def ledger_identity(path) -> dict:
    """Physical identity of one ledger file, or a refusal when it cannot be stated.

    path, device and inode name the file this process can see. There is no second id:
    the bridge ledger has none, and this package does not add one. Any field that
    cannot be read refuses.
    """
    location = Path(path)
    try:
        real = location.resolve()
        info = os.stat(real, follow_symlinks=True)
    except OSError as error:
        raise HostUnavailable(f"ledger identity is unknown: {error}") from error
    if not stat.S_ISREG(info.st_mode):
        raise HostUnavailable(f"ledger identity is unknown: {real} is not a regular file")
    return {
        "path": str(location),
        "realPath": str(real),
        "device": info.st_dev,
        "inode": info.st_ino,
    }


def same_ledger(expected, observed) -> bool:
    """True only when path, device and inode still name the captured ledger."""
    if not isinstance(expected, dict) or not isinstance(observed, dict):
        return False
    keys = ("realPath", "device", "inode")
    return all(expected.get(key) is not None and expected.get(key) == observed.get(key) for key in keys)


def _identity_of_open_ledger(ledger):
    """Identity of the ledger file the open connection names, or None when unknown.

    This stats the path the connection reports. It does not mint an id. Replacement
    is caught by comparing the result with the identity captured when the file was
    opened, which require_ledger does.
    """
    database = getattr(ledger, "db", None)
    rows = None
    if database is not None:
        try:
            rows = database.execute("PRAGMA database_list").fetchall()
        except sqlite3.Error:
            rows = None
    location = None
    if rows:
        for row in rows:
            name = row[1] if len(row) > 1 else None
            file_name = row[2] if len(row) > 2 else None
            if name == "main" and file_name:
                location = file_name
                break
    if location is None:
        location = getattr(ledger, "path", None)
    if not location:
        return None
    try:
        return ledger_identity(location)
    except HostUnavailable:
        return None


class BridgeHostAdapter:
    def __init__(self, socket_path=None, *, call=None, ledger_get=None, store=None, clock=None,
                 timeout: float = 20.0, page: int = PAGE, execution_policy=_PROCESS_POLICY,
                 ledger_directory=None, **transport_options):
        """call(method, params) -> dict is the only way this class reaches the host.

        Supplying it directly is how the tests drive the real logic without a socket. Omitting
        it builds the transport from the pinned bridge library, imported lazily so that importing
        this package never requires it.

        execution_policy is the bridge ExecutionPolicy handed to Bridge. The default sentinel
        reads the process snapshot once, through rolepolicy.declared(). Pass an ExecutionPolicy,
        including the presence-only policy, to keep that snapshot out of this adapter. A
        read-only adapter never creates a thread, so it does not resolve one. ledger_directory,
        when given, is the store directory the transport ledger is opened in; omitting it keeps
        the environment resolution every existing caller already has.
        """
        self.page = page
        self.store = store
        self.clock = clock
        self._bridge = None
        self._runner = None
        self._transport = None
        self.ledger_directory = ledger_directory
        if call is not None:
            self._call = call
            self._ledger_get = ledger_get or (lambda request_id: None)
            self._send = None
            return
        if execution_policy is _PROCESS_POLICY:
            from . import rolepolicy

            resolved = rolepolicy.declared()
            execution_policy = getattr(resolved, "_policy", None)
        self._execution_policy = execution_policy
        self._transport = _Transport(
            socket_path, timeout, execution_policy=execution_policy,
            ledger_directory=ledger_directory, **transport_options,
        )
        self._call = self._transport.call
        self._ledger_get = self._transport.ledger_get
        self._send = self._transport.send
        self._ledger_identity = self._transport.ledger_identity

    def ledger_identity_record(self):
        """The ledger this transport opened, or None when this adapter has no transport.

        Read-only adapters have no ledger. A transport whose ledger cannot be identified
        already refused during construction.
        """
        if self._transport is None:
            return None
        return dict(self._ledger_identity)

    def require_ledger(self, expected):
        """Re-stat the ledger path captured at open and refuse unless it is that file.

        Synchronous on the caller thread and on the worker thread. It stats the path
        recorded when the ledger was opened; it does not touch the sqlite connection
        and does not submit work to the transport queue. A replaced file at that path
        has a different device or inode and is refused. Nothing is reopened or reminted.
        """
        if self._transport is None:
            raise HostUnavailable("this adapter has no ledger to revalidate")
        captured = self._ledger_identity or {}
        location = captured.get("realPath") or captured.get("path")
        if not location:
            raise HostUnavailable("ledger identity is unknown; refusing before mutation")
        try:
            observed = ledger_identity(location)
        except HostUnavailable:
            raise HostUnavailable(
                "ledger identity changed or is unknown; refusing before mutation"
            ) from None
        if observed is None or not same_ledger(expected, observed):
            raise HostUnavailable(
                "ledger identity changed or is unknown; refusing before mutation"
            )
        self._ledger_identity = observed
        return observed

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
        """True, False, or None for unknown. Resolved by exact id, never by a filter's silence.

        The exec listings run after the archived default listing and before the unarchived one,
        which is the listing that usually needs its cursor: an exec thread then resolves in a
        few pages on its first check instead of waiting behind the whole default listing. Each
        listing is bounded by MAX_PAGES_PER_CHECK.
        """
        if cwd and self._scan_listing(thread_id, UNARCHIVED_CWD, {"archived": False, "cwd": cwd}):
            return False
        if self._scan_listing(thread_id, ARCHIVED, {"archived": True}):
            return True
        exec_kinds = {"sourceKinds": list(EXEC_SOURCE_KINDS)}
        if self._scan_listing(thread_id, ARCHIVED_EXEC, {"archived": True, **exec_kinds}):
            return True
        if self._scan_listing(thread_id, UNARCHIVED_ALL_EXEC, {"archived": False, **exec_kinds}):
            return False
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

    def send_message(self, request_id: str, thread_id: str, message: str, settings=None, *, before_start=None, guard_rpc_requests=0) -> dict:
        """Send under the authorized settings, or do not send.

        settings is required. The pinned bridge's own send resumes with {threadId, excludeTurns}
        and says in its source that no cwd, model, sandbox or reasoning overrides are supplied;
        on this host that resume returned dangerFullAccess for a task created workspaceWrite with
        networkAccess false. Falling back to that path when settings are absent would reintroduce
        exactly the behaviour this guards, so absence is refused rather than defaulted.

        before_start is optional and keyword-only. None leaves the existing sequence. When given,
        it is an async callable of one argument, the worker's existing rpc, awaited after the
        resumed settings have been verified and immediately before turn/start. None continues;
        a dict with code and message refuses the turn through the same local refusal the
        settings check already uses. The callable runs on this worker, so a host read uses the
        rpc it was given. Calling this adapter again from inside it would wait on the same
        worker and never return.

        guard_rpc_requests counts the extra host calls before_start may make, not the three
        the send already makes. Zero, the default, keeps the ordinary budget. A positive
        integer widens only this submission. Anything else is refused here, before a worker
        is asked to send.
        """
        if self._send is None:
            raise HostUnavailable("this adapter was built read-only, with no transport to send on")
        if settings is None:
            raise HostUnavailable(
                "a send requires the authorized task settings; refusing to resume with host"
                " defaults"
            )
        _require_guard_budget(guard_rpc_requests)
        return self._send(
            request_id, thread_id, message, settings,
            before_start=before_start, guard_rpc_requests=guard_rpc_requests,
        )

    def create_thread(self, request_id: str, **settings) -> dict:
        """Create one thread on the existing bridge, and retain its whole receipt.

        The pinned Bridge.create_thread already writes the real thread id before the title or
        the first turn, and it keeps a failed known shell. This method only hands that call to
        the worker that owns the ledger. It does not open a second RPC client or a second
        ledger, and it does not invent a bootstrap of its own: the caller supplies the settings
        Bridge.create_thread already accepts.
        """
        if self._send is None or self._transport is None:
            raise HostUnavailable(
                "this adapter was built read-only, with no transport to create a thread on"
            )
        return self._transport.create_thread(request_id, **settings)

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

    A sqlite connection belongs to the thread that created it, and the bridge's ledger is a
    sqlite connection the bridge touches from inside its coroutines, so building it on the
    caller's thread and using it on the worker's fails on the first send before a single RPC
    goes out. That is why there is a worker thread at all, and it is unchanged.

    Work is handed over through an ordinary queue the worker polls. An earlier note here said
    that was forced - that a coroutine submitted with run_coroutine_threadsafe to a
    run_forever loop in another thread never completes on this host, measured on CPython 3.13
    and 3.14. Re-measured against the same shape on 2026-09-17: it completes and overlaps on
    both 3.13.14 and 3.14.4. Whatever that was, it is not true now, and the note is corrected
    rather than left to send the next reader down a road that is no longer closed. The queue
    stays because it depends on nothing but the loop's own timer, not because it has to.

    Submissions do NOT serialise. Each is dispatched as its own task, because awaiting them
    one at a time meant a send whose caller had already given up still held the dequeue until
    its own RPC chain ended - so the next parent's send waited out the abandoned one. Two
    bounds keep that from becoming a different problem: one mutation at a time per recipient,
    and a deadline on every executing submission.
    """

    POLL_SECONDS = 0.005
    # The phases one request is bounded through, in the order they happen. The bridge holds one
    # bound per phase; this tuple is both what the relay hands it and what the arithmetic below
    # counts, so the enforced bounds and the budget cannot drift apart.
    #
    #   establish   _connect_lock, then unix_connect and initialize whenever the reader has
    #               finished - AppServer.call awaits connect() before EVERY request, so a
    #               connection that drops mid-send is rebuilt in front of the NEXT one
    #   transmit    the request frame draining into the socket
    #   ack         the response coming back
    #
    # The count was already three. What it was not, until each phase was actually bounded, was
    # TRUE: it claimed three separately bounded stages while the websocket write had no bound at
    # all, so one stage could quietly spend the whole budget the other two were counted into.
    TRANSFER_PHASES = ("establish", "transmit", "ack")
    RPC_STAGES_PER_REQUEST = len(TRANSFER_PHASES)
    # _guarded_send makes exactly these three: thread/read, thread/resume, turn/start.
    RPC_REQUESTS_PER_SEND = 3
    # A managed pre-start guard may page thread/list four times for each archive filter,
    # then read the goal and the thread once more. That is the largest callback this
    # package currently makes. The number is a per-send argument, never a process setting:
    # an ordinary send still declares zero and keeps RPC_REQUESTS_PER_SEND.
    GUARD_RPC_REQUESTS_MAX = 10
    # The worst case a legitimately progressing send can reach. A send holding a live
    # connection throughout spends only three of these; the bound exists for the one that
    # does not.
    RPC_STAGES_PER_SEND = RPC_STAGES_PER_REQUEST * RPC_REQUESTS_PER_SEND
    # How long in-flight work gets to finish on the way out before it is cancelled.
    DRAIN_SECONDS = 5.0
    # How much longer than the RPC timeout a caller waits before giving up. It was written
    # inline; it is named here because it is also the bound on how long a submission may wait
    # for its recipient's turn, and a bound nobody can set is a bound nobody can test.
    CALLER_SLACK_SECONDS = 10.0

    def __init__(self, socket_path, timeout, *, app_server_factory=None, bridge_factory=None,
                 ledger_factory=None, drain_seconds=None, caller_slack=None,
                 execution_policy=None, ledger_directory=None):
        import concurrent.futures
        import queue
        import threading

        self._futures = concurrent.futures
        self.timeout = timeout
        self._execution_policy = execution_policy
        self._ledger_directory = ledger_directory
        self.ledger_identity = None
        self._inbox = queue.Queue()
        self._stopping = False
        self._accepting = True
        # Held across the acceptance check AND the enqueue, and taken again by close() to
        # flip acceptance. Checking a flag and then queueing are two steps, and a submission
        # that passed the check but queued after the drain had already run left its future
        # with nobody to answer it: the caller then waited out its whole budget and read a
        # timeout, which says "outcome unknown" about work that was never started.
        self._admission = threading.Lock()
        self._drain_seconds = self.DRAIN_SECONDS if drain_seconds is None else drain_seconds
        self._caller_slack = (
            self.CALLER_SLACK_SECONDS if caller_slack is None else caller_slack
        )
        # One asyncio.Lock per recipient, created on the worker's loop as sends arrive.
        self._locks = {}
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
        self.ledger_identity = self._state.get("ledgerIdentity")
        self._ledger = self._state.get("ledger")

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
        inflight = set()
        while True:
            try:
                (work, future, recipient, expires_at,
                 withheld, replay, *rest) = self._inbox.get_nowait()
            except queue.Empty:
                # No _stopping shortcut here. close() cannot set a flag and queue the sentinel
                # in one step, so an idle worker could see the flag first and leave through
                # this branch - never running _shut_down, never settling the sentinel, never
                # closing the rpc or the ledger. The sentinel is the only way out.
                await asyncio.sleep(self.POLL_SECONDS)
                continue
            if work is None:
                await self._shut_down(inflight, future)
                return
            # Dispatched, not awaited. Awaiting here is what let one stalled send hold the
            # dequeue: the caller gives up after its own budget, but that abandonment never
            # released this loop, so the NEXT submission - a different recipient, a different
            # parent - waited out the abandoned send's whole RPC chain rather than its own.
            task = asyncio.ensure_future(
                self._run(
                    work, future, recipient, expires_at, withheld, replay,
                    execution_budget=rest[0] if rest else None,
                )
            )
            inflight.add(task)
            task.add_done_callback(inflight.discard)

    async def _run(self, work, future, recipient, expires_at, withheld, replay=None, execution_budget=None):
        """One submission, bounded twice: by its recipient's turn and by its own deadline."""
        import asyncio
        import time

        held = None
        if replay is not None:
            # Asked BEFORE the recipient is considered. A replay of a request the ledger has
            # already settled makes no RPC and mutates nothing, so refusing it as a busy
            # recipient would hide a known outcome behind a retry - the opposite of what
            # request-id idempotency is for. A lookup that raises is the ledger rejecting the
            # same id under different arguments, and that rejection is the point.
            try:
                retained = replay()
            except BaseException as error:  # noqa: BLE001 - returned to the caller
                self._settle(future, error=error)
                return
            if retained is not None:
                self._settle(future, result=retained)
                return
        if recipient is not None:
            # One mutation at a time per recipient, and exactly one. _guarded_send reads the
            # thread, resumes it and starts a turn across separate awaits, so a second
            # concurrent send to the same thread can pass the idle check before the first
            # reaches turn/start, and both would start a turn. Request-id idempotency does
            # not catch that: the two requests are genuinely different. Different recipients
            # overlap freely, which is the whole point of this change.
            lock = self._locks.get(recipient)
            if lock is None:
                lock = self._locks[recipient] = asyncio.Lock()
            if time.monotonic() >= expires_at:
                # The caller has already given up. Starting a send now would occupy this
                # recipient on behalf of nobody, which is the accumulation a bound exists to
                # prevent, so expired work is answered and never dispatched late.
                self._settle(
                    future,
                    error=RuntimeError(
                        "the relay transport gave up on this submission before sending it"
                    ),
                )
                return
            if lock.locked():
                # Reported immediately rather than queued behind the turn in flight. A turn is
                # a real agent run, so waiting for one only converts a fast, accurate "busy"
                # into a slow one - and waiters are exactly what would pile up unbounded.
                # NOTHING was sent, and that is worth saying precisely: raising here would be
                # classified as outcome_unknown, which parks the delivery as possibly
                # delivered and blocks a clean retry. A recipient whose turn is already in
                # flight is busy in the sense the delivery layer already backs off from.
                self._settle(future, result=withheld())
                return
            # Free, and this is the only coroutine that could have taken it since the check:
            # acquiring an unlocked asyncio.Lock completes without yielding to the loop.
            await lock.acquire()
            held = lock
        try:
            # One budget per stage the bridge bounds separately - see RPC_STAGES_PER_SEND for
            # what they are. This exists to make the worst case FINITE, not to match any
            # caller: rpc.py awaits ws.send() OUTSIDE its response timeout, so without it a
            # write that never drains would hold this recipient's turn forever. The caller of
            # an ordinary send is long gone by the time it fires, having given up at timeout
            # plus caller slack. A guarded send waits for this same budget plus that slack.
            # Either way, what the bound decides is whether the bridge ledger ends up holding a
            # real receipt for this request id or an uncertain one - which is what
            # reconciliation reads later. Cancellation reaches _guarded_send, which records
            # its own outcome_unknown receipt before re-raising.
            result = await asyncio.wait_for(
                work(),
                self.timeout * self.RPC_STAGES_PER_SEND if execution_budget is None else execution_budget,
            )
        except BaseException as error:  # noqa: BLE001 - returned to the caller
            # Hand over the failure, but not this worker's own frame. The traceback starts at
            # the await above, inside a coroutine that is still suspended and still serving
            # the queue. A caller is entitled to clear the frames of what it catches, and
            # unittest's assertRaises does exactly that; on CPython 3.11 clearing this frame
            # finalizes the worker mid-flight, so every later submit waits out its timeout
            # against a loop that no longer reads its inbox. Dropping one frame keeps the
            # type, the message and every frame from inside the operation, which is what the
            # caller actually needs to debug it.
            # Through the built-in, never the bound with_traceback method. A subclass can
            # override that method, and running its code here - inside the handler whose
            # whole job is to keep this worker alive - would reintroduce the failure this
            # guards against.
            inner = error.__traceback__
            BaseException.with_traceback(error, inner.tb_next if inner else None)
            self._settle(future, error=_for_a_caller(error))
        else:
            self._settle(future, result=result)
        finally:
            if held is not None:
                held.release()

    @staticmethod
    def _settle(future, *, result=None, error=None) -> None:
        """A caller that gave up leaves a future nobody reads, never a broken one."""
        if future.done():
            return
        if error is not None:
            future.set_exception(error)
        else:
            future.set_result(result)

    async def _shut_down(self, inflight, sentinel) -> None:
        """Ordered, and finite at every step.

        Closing resources used to be ordinary queued work, which under concurrent dispatch
        could close the ledger while a send still needed to save its receipt. So: stop taking
        work, give what is in flight a bounded chance to finish, then cancel it WHILE THE
        LEDGER IS STILL OPEN - that is what lets a cancelled send record its own
        outcome_unknown - and only then close.

        On the drain loop below, which is not what it looks like. By the time this runs the
        inbox is normally empty: the worker takes one submission per iteration and dispatches
        each as a task, so anything queued ahead of the sentinel has already been STARTED, and
        it finishes or is cancelled as in-flight work like everything else. That is deliberate.
        Those submissions were accepted while the transport was still accepting - _submit holds
        _admission across the check and the enqueue, and close() takes the same lock to end
        acceptance - and their callers are blocked on them right now. Refusing work that could
        finish inside the drain window would turn it into "nothing was sent" for no gain.

        So the loop is a backstop rather than the main path. It answers anything that did land
        in the inbox without being dispatched, which is what a submission enqueued in the
        narrow window between the last get_nowait and the sentinel looks like.
        """
        import asyncio
        import queue

        self._accepting = False
        while True:
            try:
                work, future, *_rest = self._inbox.get_nowait()
            except queue.Empty:
                break
            if work is None:
                self._settle(future, result=None)
                continue
            self._settle(
                future,
                error=RuntimeError("the relay transport is shutting down; nothing was sent"),
            )
        if inflight:
            await asyncio.wait(set(inflight), timeout=self._drain_seconds)
            remaining = [task for task in inflight if not task.done()]
            for task in remaining:
                task.cancel()
            if remaining:
                await asyncio.gather(*remaining, return_exceptions=True)
        rpc = self._state.get("rpc")
        if rpc is not None and hasattr(rpc, "close"):
            try:
                await rpc.close()
            except Exception:  # noqa: BLE001 - shutdown has nobody to report to
                pass
        ledger = self._state.get("ledger")
        if ledger is not None:
            try:
                ledger.close()
            except Exception:  # noqa: BLE001
                pass
        self._settle(sentinel, result=None)

    async def _build(self, socket_path, app_server_factory, bridge_factory, ledger_factory):
        from pathlib import Path

        from .store import state_dir

        if ledger_factory is None:
            from codex_thread_bridge.ledger import open_endpoint_ledger

            directory = self._ledger_directory if self._ledger_directory is not None else state_dir(socket_path)
            canonical, ledger = open_endpoint_ledger(Path(socket_path), Path(directory))
        else:
            canonical, ledger = ledger_factory()
        identity = _identity_of_open_ledger(ledger)
        if identity is None:
            try:
                ledger.close()
            except Exception:  # noqa: BLE001 - the refusal below is the answer
                pass
            raise HostUnavailable("ledger identity is unknown; refusing to use this ledger")
        if app_server_factory is None:
            from codex_thread_bridge.rpc import AppServer, PhaseBounds

            # Built from this transport's own phase names, so a phase added here without a bound
            # over there is a TypeError at startup rather than an unbounded wait in production.
            rpc = AppServer(
                canonical, timeout=self.timeout,
                phase_bounds=PhaseBounds(**dict.fromkeys(self.TRANSFER_PHASES, self.timeout)),
            )
        else:
            rpc = app_server_factory(canonical)
        if bridge_factory is None:
            from codex_thread_bridge.bridge import Bridge

            bridge = Bridge(rpc, ledger, policy=self._execution_policy)
        else:
            bridge = bridge_factory(rpc, ledger)
        self._state.update(rpc=rpc, ledger=ledger, bridge=bridge, ledgerIdentity=identity)

    # ------------------------------------------------------------- caller side

    def _submit(self, work, *, recipient=None, withheld=None, replay=None, rpc_requests=None):
        import time

        if not self.thread.is_alive():
            raise RuntimeError("the relay transport worker is not running")
        requests = self.RPC_REQUESTS_PER_SEND if rpc_requests is None else rpc_requests
        # One timeout per reconnect and per request. Ordinary reads and the three-request
        # send keep the historical caller budget: one timeout plus slack, shorter than the
        # worker's nine-stage bound on purpose. A declared guard changes that. Its caller
        # waits for every stage that submission may spend, plus the same slack, so it is
        # still there when a slow valid guard finishes.
        if requests == self.RPC_REQUESTS_PER_SEND:
            execution_budget = self.timeout * self.RPC_STAGES_PER_SEND
            budget = self.timeout + self._caller_slack
        else:
            execution_budget = self.timeout * self.RPC_STAGES_PER_REQUEST * requests
            budget = execution_budget + self._caller_slack
        future = self._futures.Future()
        # The deadline travels WITH the submission. A caller that gives up leaves work whose
        # only remaining purpose would be to occupy its recipient, so waiting work that has
        # outlived its caller is answered rather than dispatched late.
        with self._admission:
            # Under the same lock close() uses, so a submission is either in the queue before
            # acceptance ends - and therefore drained and answered - or refused outright.
            if not self._accepting:
                raise RuntimeError("the relay transport is shutting down; nothing was sent")
            self._inbox.put((
                work, future, recipient, time.monotonic() + budget, withheld, replay,
                execution_budget,
            ))
        return future.result(budget)

    def call(self, method, params):
        # No recipient key: reads must stay available while a send to some thread is stalled.
        return self._submit(lambda: self._state["rpc"].call(method, params))

    def send(self, request_id, thread_id, message, settings, *, before_start=None, guard_rpc_requests=0):
        _require_guard_budget(guard_rpc_requests)
        def withheld():
            return {
                "requestId": request_id,
                "status": "failed",
                "error": (
                    "this relay already has a turn in flight for the recipient; message"
                    " withheld without being sent"
                ),
                "rpcError": {
                    "code": "thread_busy",
                    "message": (
                        "another send to this thread is still in flight in this process"
                    ),
                },
            }

        def replay():
            # Runs on the worker thread, which owns the ledger's sqlite connection, and is
            # consulted before the recipient lock. A request the ledger has already settled
            # has an answer; refusing it as a busy recipient would replace that answer with a
            # retry and lose it. Raises when the id was reused with different arguments, which
            # the caller needs to see rather than a busy report.
            retained = self._state["ledger"].lookup(
                request_id, *_send_identity(thread_id, message)
            )
            if retained is None or _retryable(retained):
                # None has never been seen. not_attempted began no business turn, so the same
                # id continues into _guarded_send, where Ledger.begin re-arms it. Every other
                # retained status is an answer and must not be replaced by a busy retry.
                return None
            return {**retained, "replayed": True}

        return self._submit(
            lambda: _guarded_send(
                self._state["rpc"], self._state["ledger"],
                request_id, thread_id, message, settings,
                before_start=before_start,
            ),
            recipient=thread_id,
            withheld=withheld,
            replay=replay,
            rpc_requests=self.RPC_REQUESTS_PER_SEND + guard_rpc_requests,
        )

    def create_thread(self, request_id, **settings):
        """Delegate one creation to the pinned Bridge on this worker's ledger."""
        return self._submit(
            lambda: self._state["bridge"].create_thread(request_id, **settings)
        )

    def ledger_get(self, request_id):
        async def read():
            return self._state["ledger"].get(request_id)

        return self._submit(read)

    def close(self):
        if not self.thread.is_alive():
            return
        # Stop accepting BEFORE the sentinel is queued, so nothing joins the queue behind a
        # shutdown that will refuse to run it. Resources are closed inside the worker, after
        # the drain - submitting their closure as ordinary work is what allowed the ledger to
        # be closed under a send that still needed it.
        with self._admission:
            self._accepting = False
        self._stopping = True
        future = self._futures.Future()
        self._inbox.put((None, future, None, 0.0, None, None, None))
        try:
            future.result(self._drain_seconds + self.timeout + self._caller_slack)
        except Exception:  # noqa: BLE001 - the join below is the real answer
            pass
        self.thread.join(timeout=self._drain_seconds + 10)


class _ShutdownCancelled(Exception):
    """Cancellation, in a shape an ordinary `except Exception` can catch.

    `asyncio.CancelledError` inherits from BaseException, so handing it across the thread
    boundary unchanged walks it straight past the delivery layer's `except Exception` around
    the send. The tick unwinds and the delivery it had already claimed is left leased, in
    `sending`, with an attempt row nothing ever settles - which is the opposite of what the
    diagnostics contract asks for and leaves a restart with a claim it cannot account for.

    The outcome genuinely is unknown rather than "nothing was sent": `_shut_down` cancels
    only work that outlived the drain window, so the write may well have reached the host
    before the cancel landed. This says that, and the delivery layer renders it as the
    `outcome_unknown` receipt it already knows how to settle.
    """


def _for_a_caller(error):
    """Translate what a caller cannot catch; pass everything else through untouched.

    The original is kept as `__cause__` with the trimmed traceback already applied, so the
    reason a send ended is still readable from the exception that reaches the caller.
    """
    import asyncio

    if not isinstance(error, asyncio.CancelledError):
        return error
    translated = _ShutdownCancelled(
        "the relay transport was shut down while this send was in flight;"
        " outcome unknown, do not resend under a new request id"
    )
    translated.__cause__ = error
    return translated


class _Refusal(Exception):
    """A refusal decided locally, rendered exactly like an RPC error so one classifier reads both.

    The transport classifier keys on a method-prefixed error string and an rpcError.code, so a
    local refusal that wants the same treatment has to look the same on the wire.
    """

    def __init__(self, method: str, error: dict):
        self.method = method
        self.error = error
        super().__init__(f"{method}: {error.get('message', error.get('code', 'refused'))}")


class _NoPhaseTimeout(Exception):
    """Stands in for PhaseTimeout when the pinned bridge is absent, and is never raised.

    An except clause needs a class either way; binding this one keeps the handler below inert
    rather than letting it catch something it was not written for.
    """


def _send_identity(thread_id, message):
    """The ledger identity of one send: its operation name and argument fingerprint.

    Defined once because two places ask the ledger the same question - the replay precheck
    before the recipient lock, and the send itself. If they ever disagreed the precheck would
    quietly stop matching and replays would go back to being refused as busy.
    """
    return "send_message_to_thread", {"threadId": thread_id, "message": message}


def _retryable(receipt):
    """True only for the one ledger status that began no business turn.

    Ledger.RETRYABLE_STATUSES is exactly {"not_attempted"}. A managed guard refusal is recorded
    as that status so the same request id can be re-armed after an explicit recovery. Every other
    retained status, including outcome_unknown from an interrupted RPC, stays terminal.
    """
    from codex_thread_bridge.ledger import RETRYABLE_STATUSES

    return isinstance(receipt, dict) and receipt.get("status") in RETRYABLE_STATUSES


def _require_guard_budget(guard_rpc_requests):
    """Refuse a budget that is not a bounded count of extra guard requests.

    The check is local and has no host effect. A bool is rejected even though it is an
    int, because True would silently buy one extra request. The ceiling is the largest
    callback this package's managed guard actually makes.
    """
    if type(guard_rpc_requests) is int and 0 <= guard_rpc_requests <= _Transport.GUARD_RPC_REQUESTS_MAX:
        return guard_rpc_requests
    raise HostUnavailable(
        "guard_rpc_requests must be an integer from 0 through"
        f" {_Transport.GUARD_RPC_REQUESTS_MAX}; refusing before any send"
    )


async def _guarded_send(rpc, ledger, request_id, thread_id, message, settings, *, before_start=None):
    """The bridge's send sequence, with the authorized settings actually carried.

    Mirrors codex_thread_bridge.bridge.Bridge._mutate rather than calling it, so the relay keeps
    its own ledger identity and never routes a delivery through the bridge's MCP surface.
    Idempotency is the bridge's own ledger, with its exact operation name and argument
    fingerprint, so a replay is answered from the receipt and a reused id with different arguments
    is rejected by the ledger.

    On the settings the two share, they now agree by construction and by test: the same mode and
    default tables, the same refusal codes, the same finding precedence, and the same shape of
    turn. They are not otherwise identical, and neither pretends to be: this side additionally
    refuses an unexpected activePermissionProfile, and the bridge additionally refuses a policy
    field it cannot transmit and carries observation limits and a post-dispatch annotation.

    The receipt stays byte-compatible with the bridge's: top-level status and turnId, resumed,
    rpcError, and a method-prefixed error.

    before_start, when supplied, is awaited as before_start(rpc) after the resume has been
    verified and immediately before turn/start. rpc is the worker's existing client, so a
    fresh host read does not open a second transport. The callback returns None to continue,
    or a refusal dict with code and message. A caller that passes None, including every
    existing call, keeps the previous sequence. The callback is not consulted on a retained
    replay: the ledger already answered, and running the guard again would re-decide a send
    that must not be repeated.
    """
    import asyncio

    method, params = _send_identity(thread_id, message)

    # Raises when the id was used with different arguments. That rejection is the point.
    retained = ledger.lookup(request_id, method, params)
    if retained is not None and not _retryable(retained):
        return {**retained, "replayed": True}
    fresh, receipt = ledger.begin(request_id, method, params)
    if not fresh:
        return {**(receipt or {}), "replayed": True}

    try:
        from codex_thread_bridge.rpc import PhaseTimeout, RpcError
    except ImportError:  # pragma: no cover - only when the pinned bridge is absent
        RpcError = _Refusal
        PhaseTimeout = _NoPhaseTimeout

    try:
        receipt["threadId"] = thread_id
        ledger.save(receipt)

        state = await rpc.call("thread/read", {"threadId": thread_id})
        status = (state.get("thread") or {}).get("status", {}).get("type")
        receipt["statusBeforeResume"] = status
        if status == "active":
            raise _Refusal("thread/read", {
                "code": "thread_busy",
                "message": "Thread is active; message withheld. Wait for completion.",
            })
        if getattr(settings, "settings_free_resume", False):
            # A pair no policy derived - a supervisor's, or one an exception admitted - is never
            # transmitted, whatever this read said: a resume can apply what it transmits while
            # the host materializes a thread, which would restore a value the user may have
            # changed, and a recipient can unload between this read and the resume. So the
            # resume requests nothing. An unloaded thread loads under its own persisted state, a
            # loaded one reports it, and the response is compared with the record before any
            # turn: the same detector, with nothing sent that could make it agree.
            resumed = await rpc.call("thread/resume",
                                     settings.settings_free_resume_params(thread_id))
            receipt["resumed"] = resumed
            receipt["settingsFreeResume"] = True
            ledger.save(receipt)
            findings = settings.mismatches(resumed, transmitted=False)
            if findings:
                first = findings[0]
                receipt["settingsFindings"] = findings
                # Only a DIFFERENCE is renamed. An absent answer, an unknown environment
                # selection, an unexpected permission profile and an interactive approval
                # policy this transport cannot carry keep their own codes, because each already
                # says something more specific than "differs" and the approval one decides the
                # inbox route.
                code = (SETTINGS_DIFFER_AFTER_LOAD if first["code"] == SETTINGS_NOT_PRESERVED
                        else first["code"])
                raise _Refusal("thread/resume", {
                    "code": code,
                    "message": f"{code}: {first['field']} is {first.get('returned')!r} on the"
                               f" loaded thread and {first.get('expected')!r} in the record;"
                               " nothing was transmitted and no turn was started",
                })
        else:
            # Resume carries the authorized settings. It is the DETECTOR: its response reports
            # the full derived policy, the effort and the environments, so a host that ignores
            # overrides is caught here, before anything has started. A pair policy derived
            # reaches this branch, so a host that applies it while loading the thread lands the
            # thread where policy says it belongs; so does a task bound to no role, which the
            # role policy does not govern at all.
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

        # A carried approval policy that is not the recorded one is delivered on either route and
        # noted here, so the record can be re-recorded (CRW-225). Nothing was sent that could set
        # it: neither resume carries an approvalPolicy, so the difference is the thread's own.
        divergence = getattr(settings, "approval_divergence", None)
        note = divergence(resumed) if divergence is not None else None
        if note is not None:
            receipt["settingsNotes"] = [note]
            ledger.save(receipt)

        if before_start is not None:
            # After the verified resume and before any turn. A pause that appears during the
            # resume is visible here; one that appears after this await and before the host
            # accepts turn/start is not, because the host has no conditional start.
            decision = await before_start(rpc)
            if decision is not None:
                if not isinstance(decision, dict) or "code" not in decision or "message" not in decision:
                    refusal = _Refusal("turn/start", {
                        "code": "managed_guard_invalid",
                        "message": "the pre-start guard returned neither None nor a refusal",
                    })
                else:
                    refusal = _Refusal("turn/start", {
                        "code": decision["code"],
                        "message": decision["message"],
                    })
                refusal.retryable = True
                raise refusal

        # No overrides. turn/start would accept the full policy, the effort and the environments,
        # but its response defines only turn, so anything bound here could never be read back and
        # an accepted receipt would be calling an unverifiable binding a success. The resume above
        # already established that the thread IS in the authorized state, which makes overrides
        # redundant; and TurnStartParams says a model override persists into subsequent turns, so
        # sending them would quietly rewrite the thread for every later turn as well.
        turn = await rpc.call("turn/start", {
            "threadId": thread_id,
            "input": [{"type": "text", "text": message}],
        })
        receipt["turnId"] = turn["turn"]["id"]
        receipt["status"] = "accepted"
    except _Refusal as error:
        if getattr(error, "retryable", False):
            # No business turn was started. Resume evidence stays on the receipt; the status is
            # the ledger's only re-armable one, so the same request can proceed after recovery.
            receipt.update(
                status="not_attempted",
                retrySafe=True,
                attemptedEffects=[],
                error=str(error),
                rpcError=error.error,
            )
        else:
            receipt.update(status="failed", error=str(error), rpcError=error.error)
    except RpcError as error:
        receipt.update(status="failed", error=str(error), rpcError=getattr(error, "error", None))
    except PhaseTimeout as error:
        # Which phase expired decides what this receipt is entitled to claim. An establish
        # expiry means no connection was ever obtained, so the frame for THIS request was never
        # written - recording it as a completed failure is what lets the classifier reach
        # withheld_pre_send instead of parking a delivery that demonstrably never left. The
        # initialize frame may well have gone out during establishment; it is an observation and
        # begins no effect, which is why it does not weaken the claim. transmit and ack both
        # expire AFTER the frame was handed to the socket, so they stay unknown.
        #
        # Reached only on thread/read and thread/resume in practice, and only those two get the
        # honest answer: turn/start keeps held_uncertain because assert_attempt_invariants
        # forbids sendAttempted "no" on a turn/start operation. Pessimistic, and safe.
        if error.phase == "establish":
            receipt.update(
                status="failed",
                error=str(error),
                rpcError={"code": "connection_unavailable", "message": str(error)},
            )
        else:
            receipt.update(status="outcome_unknown", error=f"{type(error).__name__}: {error}")
    except asyncio.CancelledError:
        # Cancelled after a start may already have delivered. Unknown, never non-delivery.
        # Deliberately not retrySafe: an interrupted RPC is not a guard refusal.
        ledger.save({**receipt, "status": "outcome_unknown"})
        raise
    except Exception as error:  # noqa: BLE001 - recorded, then classified by the transport
        receipt.update(status="outcome_unknown", error=f"{type(error).__name__}: {error}")
    return ledger.save(receipt)
