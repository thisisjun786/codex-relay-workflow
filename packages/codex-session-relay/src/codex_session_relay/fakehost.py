"""A deterministic host, shaped like the real one where it matters.

Two details are reproduced on purpose because the delivery logic depends on them. The transport
writes its receipt BEFORE its calls run, so an interrupted send leaves an unfinished receipt
rather than nothing. And replaying a request id returns the cached receipt instead of sending
again, which is exactly why a retry has to open a new attempt number rather than reuse one.

No test sleeps. Time only moves when a test moves it.
"""

import hashlib
import json

from .hostadapter import (
    IN_TURN_ITEMS_MAX, USER_MESSAGE, ThreadActivity, ThreadActivityPage, ThreadFacts, TokenScan,
    TurnInfo, find_in_listing, find_token_in, find_token_in_turn_items, is_message,
)


class ProcessDied(Exception):
    """Raised by a scripted send to stand in for the relay process being killed mid-call."""


def _typed(item) -> tuple:
    """A thread item as (turn id, text, type). A pair is what a send appends: the user message."""
    if len(item) == 2:
        return item[0], item[1], USER_MESSAGE
    return tuple(item)


class FakeThread:
    def __init__(self, thread_id, *, status="idle", approval_policy="never", archived=False,
                 goal_status=None, can_accept_input=True, loaded_settings=None):
        self.thread_id = thread_id
        self.status = status
        self.approval_policy = approval_policy
        self.archived = archived
        self.goal_status = goal_status
        self.can_accept_input = can_accept_input
        # What a resume requesting nothing reports for this thread: a resume response. None
        # means the thread's own state is what its record says, as it is on a host that
        # persists every field a record holds.
        self.loaded_settings = loaded_settings
        self.turns = []
        # (turn id, text) is a user message, the item a send appends; (turn id, text, type) is
        # an item of the named type, such as a command's output.
        self.items = []
        # When the host last updated the thread, as its thread listing reports it.
        self.updated_at = 0.0
        # When its latest turn started, the listing's recencyAt.
        self.recency_at = None


def _loaded_as_recorded(thread, settings) -> dict:
    """The resume response of a thread whose own state is exactly its record."""
    data = settings.data
    return {
        "approvalPolicy": thread.approval_policy,
        "sandbox": data["sandbox"],
        "cwd": data["cwd"],
        "runtimeWorkspaceRoots": list(data["runtimeWorkspaceRoots"]),
        "model": data["model"],
        "reasoningEffort": data["reasoningEffort"],
        "activePermissionProfile": data.get("expectedPermissionProfile"),
        "thread": {"id": thread.thread_id,
                   "environments": [dict(one) for one in data["environments"]]},
    }


class FakeHostAdapter:
    def __init__(self, clock):
        self.clock = clock
        self.threads = {}
        self.ledger = {}
        self._script = []
        self.sends = []
        self.settings_seen = []
        # (request id, thread id) of every resume that requested nothing: the route a pair no
        # policy derived takes, so a test can say that nothing was transmitted to that thread.
        self.settings_free_resumes = []
        self.read_failures = set()
        self.scan_limit = None
        self.connected = True
        self._turn_counter = 0

    # ------------------------------------------------------------- fixtures

    def add_thread(self, thread_id, **kwargs) -> FakeThread:
        thread = FakeThread(thread_id, **kwargs)
        thread.updated_at = self._updated_now()
        self.threads[thread_id] = thread
        return thread

    def _updated_now(self) -> float:
        """When the host updates a thread now; a host built without a clock never moves."""
        return self.clock.now() if self.clock is not None else 0.0

    def start_turn(self, thread_id, *, turn_id=None, status="inProgress", text=None) -> TurnInfo:
        thread = self.threads[thread_id]
        self._turn_counter += 1
        turn_id = turn_id or f"turn-{thread_id}-{self._turn_counter}"
        turn = TurnInfo(turn_id, status, self.clock.now())
        thread.turns.append(turn)
        thread.updated_at = self._updated_now()
        thread.recency_at = self._updated_now()
        if text:
            thread.items.append((turn_id, text))
        return turn

    def finish_turn(self, thread_id, turn_id, status="completed") -> None:
        thread = self.threads[thread_id]
        thread.turns = [
            TurnInfo(t.turn_id, status, t.started_at) if t.turn_id == turn_id else t
            for t in thread.turns
        ]
        thread.updated_at = self._updated_now()

    def set_status(self, thread_id, status) -> None:
        self.threads[thread_id].status = status

    def script(self, outcome, *, times=1) -> None:
        self._script.extend([outcome] * times)

    def fail_reads(self, *names) -> None:
        self.read_failures.update(names)

    def restart(self) -> None:
        """An app or connection restart: live state is lost, the durable ledger is not."""
        self.connected = True
        self.read_failures.clear()

    # ---------------------------------------------------------------- reads

    def _guard(self, name) -> None:
        if name in self.read_failures:
            raise ConnectionError(f"{name} unavailable")

    def read_thread(self, thread_id) -> ThreadFacts:
        self._guard("read_thread")
        thread = self.threads[thread_id]
        return ThreadFacts(thread.status, thread.can_accept_input)

    def is_archived(self, thread_id, *, cwd=None):
        self._guard("is_archived")
        return self.threads[thread_id].archived

    def read_goal_status(self, thread_id):
        self._guard("read_goal_status")
        return self.threads[thread_id].goal_status

    def list_turn_ids(self, thread_id, limit=20) -> list:
        self._guard("list_turn_ids")
        return [t.turn_id for t in self.threads[thread_id].turns[-limit:]]

    def read_turn(self, thread_id, turn_id):
        self._guard("read_turn")
        for turn in self.threads[thread_id].turns:
            if turn.turn_id == turn_id:
                return turn
        return None

    def recent_threads(self, limit, cursor=None) -> ThreadActivityPage:
        """The host-wide listing, newest-updated first, as the real host keeps it.

        A thread running a turn is listed active and updated now, because the real host
        refreshes a running thread's updatedAt every few seconds; any other thread keeps its own
        status and the time it was last updated. Archived threads are left out, as the host's
        default listing leaves them. The cursor is a keyset, (updated_at, id) of the page's last
        thread, as the host's is a timestamp: a thread that leaves the listing or moves to its top
        never shifts the threads below the cursor.
        """
        self._guard("recent_threads")
        now = self._updated_now()
        listed = []
        for thread in self.threads.values():
            if thread.archived:
                continue
            running = thread.status == "active" or any(
                turn.status == "inProgress" for turn in thread.turns)
            listed.append(ThreadActivity(thread.thread_id, "active" if running else thread.status,
                                         now if running else thread.updated_at, thread.recency_at))
        listed.sort(key=lambda one: (one.updated_at, one.thread_id), reverse=True)
        if cursor:
            after = tuple(json.loads(cursor))
            listed = [one for one in listed if (one.updated_at, one.thread_id) < after]
        page = listed[:max(1, int(limit))]
        more = len(listed) > len(page)
        return ThreadActivityPage(tuple(page), json.dumps([page[-1].updated_at, page[-1].thread_id])
                                  if more else None)

    def find_dispatched_turn(self, thread_id, turn_id, *, sent_at):
        """The real adapter's rule over this thread's turns, newest first, as one final page."""
        self._guard("find_dispatched_turn")
        newest_first = list(reversed(self.threads[thread_id].turns))
        return find_in_listing([(newest_first, False)], turn_id, sent_at)

    def find_token_since(self, thread_id, token, *, older, limit=200) -> TokenScan:
        """The real adapter's rule over this thread's items, newest first, bounded like a scan."""
        self._guard("find_token_since")
        items = [_typed(item) for item in reversed(self.threads[thread_id].items)]
        bound = min(limit, self.scan_limit or limit)
        return find_token_in([(items[:bound], bound < len(items))], token, older)

    def find_token_in_turn(self, thread_id, token, *, turn_id, limit=IN_TURN_ITEMS_MAX) -> TokenScan:
        """The real adapter's rule over one turn's own items, oldest first, bounded like its read."""
        self._guard("find_token_in_turn")
        items = [_typed(item) for item in self.threads[thread_id].items if item[0] == turn_id]
        return find_token_in_turn_items([(items[:limit], limit < len(items))], token, turn_id)

    def get_operation(self, request_id):
        self._guard("get_operation")
        # The real ledger raises for an unknown id; the adapter turns that into a plain
        # absence, because "we have never heard of this" is an observation, not an error.
        return self.ledger.get(request_id)

    def recipient_fingerprint(self, thread_id, *, window=8) -> str:
        """Content, not just identity: a token appended to an existing item changes this."""
        self._guard("recipient_fingerprint")
        digest = hashlib.sha256()
        for item_turn, text, _kind in [_typed(item) for item in
                                       reversed(self.threads[thread_id].items)][:window]:
            digest.update(f"{item_turn}:{hashlib.sha256(text.encode()).hexdigest()}|".encode())
        return digest.hexdigest()

    def find_token(self, thread_id, token, *, limit=200, turn_id=None,
                   message_only=False) -> TokenScan:
        self._guard("find_token")
        items = [_typed(item) for item in reversed(self.threads[thread_id].items)]
        bound = min(limit, self.scan_limit or limit)
        scanned = 0
        for item_turn, text, kind in items[:bound]:
            scanned += 1
            if message_only and not is_message(kind):
                continue
            if token in text:
                return TokenScan(True, item_turn, scanned >= len(items), scanned)
        return TokenScan(False, None, bound >= len(items), scanned)

    # ---------------------------------------------------------------- write

    def send_message(self, request_id, thread_id, message, settings=None) -> dict:
        # Recorded so a delivery-level test can assert the authorized settings reached the
        # adapter. The no-widened-start guarantee is NOT proved here: this fake implements
        # delivery itself and never calls BridgeHostAdapter, so it could pass while the real
        # adapter still started an unguarded turn. That proof lives in GuardedSettingsSeam.
        self.settings_seen.append((request_id, settings))
        cached = self.ledger.get(request_id)
        if cached is not None and cached.get("status") != "in_progress_or_unknown":
            # Exactly what the real ledger does: a settled request id is answered from the
            # receipt, never resent. A retry that reuses an id gets the old failure forever.
            return {**cached, "replayed": True}
        outcome = self._script.pop(0) if self._script else "accepted"
        receipt = {"requestId": request_id, "operation": "send_message_to_thread",
                   "status": "in_progress_or_unknown", "threadId": thread_id, "retrySafe": False}
        self.ledger[request_id] = receipt
        self.sends.append((request_id, thread_id, message, outcome))
        thread = self.threads[thread_id]
        resumed = {"approvalPolicy": thread.approval_policy}

        if getattr(settings, "settings_free_resume", False) and outcome in (
                "accepted", "steer_existing"):
            # The real transport's route for a pair no policy derived: resume with nothing
            # requested, compare what the thread reports with the record through the same
            # TaskSettings.mismatches, and start nothing on a difference. An unloaded thread is
            # loaded by that resume.
            from .settings import SETTINGS_DIFFER_AFTER_LOAD, SETTINGS_NOT_PRESERVED

            self.settings_free_resumes.append((request_id, thread_id))
            resumed = (thread.loaded_settings if thread.loaded_settings is not None
                       else _loaded_as_recorded(thread, settings))
            findings = settings.mismatches(resumed, transmitted=False)
            if findings:
                first = findings[0]
                code = (SETTINGS_DIFFER_AFTER_LOAD if first["code"] == SETTINGS_NOT_PRESERVED
                        else first["code"])
                receipt.update(
                    status="failed", resumed=resumed, settingsFreeResume=True,
                    settingsFindings=findings,
                    error=f"thread/resume: {code}: {first['field']} differs; message withheld",
                    rpcError={"code": code, "message": f"{first['field']} differs"},
                )
                self.ledger[request_id] = receipt
                return dict(receipt)
            if thread.status == "notLoaded":
                thread.status = "idle"

        if outcome == "in_progress":
            return dict(receipt)
        if outcome == "process_death":
            raise ProcessDied("the relay process was killed mid-send")
        if outcome == "busy":
            receipt.update(
                status="failed",
                error="thread/read: Thread is active; message withheld. Wait for completion.",
                rpcError={"code": "thread_busy", "message": "Thread is active"},
            )
        elif outcome == "read_fail":
            receipt.update(status="failed", error="thread/read: transport refused",
                           rpcError={"code": "internal", "message": "transport refused"})
        elif outcome == "resume_fail":
            receipt.update(status="failed", error="thread/resume: cannot resume",
                           rpcError={"code": "internal", "message": "cannot resume"})
        elif outcome == "approval_policy":
            receipt.update(
                status="failed", resumed=resumed,
                error="thread/resume: Interactive approvals unsupported; message withheld.",
                rpcError={"code": "unsupported_approval_policy", "message": "unsupported"},
            )
        elif outcome == "turn_start_fail":
            receipt.update(status="failed", resumed=resumed,
                           error="turn/start: refused",
                           rpcError={"code": "internal", "message": "refused"})
        elif outcome == "initialize_fail":
            receipt.update(status="failed", resumed=resumed,
                           error="initialize: connection lost",
                           rpcError={"code": "internal", "message": "connection lost"})
        elif outcome == "transport_unknown":
            receipt.update(
                status="outcome_unknown",
                error="TransportError: turn/start: response unavailable; do not resend",
            )
        elif outcome == "steer_existing":
            existing = thread.turns[-1].turn_id if thread.turns else None
            receipt.update(status="accepted", resumed=resumed, turnId=existing)
            thread.items.append((existing, message))
            thread.updated_at = self._updated_now()
        else:  # accepted
            turn = self.start_turn(thread_id, status="inProgress", text=message)
            receipt.update(status="accepted", resumed=resumed, turnId=turn.turn_id)
        self.ledger[request_id] = receipt
        return dict(receipt)
