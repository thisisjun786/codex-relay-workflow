"""What this package needs from a host, expressed as the smallest possible surface.

Every method is either a read or the one supported send. Nothing here can change a task's
model, effort, sandbox, approval policy, goal or archive state, because the relay has no
business doing any of that and an interface that cannot express it cannot do it by accident.
"""

from dataclasses import dataclass
from typing import Protocol


class HostUnavailable(Exception):
    """A read could not be completed. Not an answer, and never treated as one.

    Distinct from returning None: None means the host answered and the thing is absent, while
    this means we do not know. Conflating them is how an unreachable host becomes a false
    negative.
    """


@dataclass(frozen=True)
class TurnInfo:
    turn_id: str
    status: str
    started_at: float | None = None


@dataclass(frozen=True)
class TokenScan:
    """Result of looking for a delivery token in a recipient's items.

    The exhausted field is the important one. A scan that stopped at its bound has not
    searched the history, so not finding the token there means nothing at all.
    """

    found: bool
    turn_id: str | None
    exhausted: bool
    scanned: int


@dataclass(frozen=True)
class ThreadFacts:
    runtime_status: str
    can_accept_input: bool | None


class HostAdapter(Protocol):
    def read_thread(self, thread_id: str) -> ThreadFacts: ...

    def is_archived(self, thread_id: str, *, cwd: str | None = None) -> bool | None: ...

    def read_goal_status(self, thread_id: str) -> str | None: ...

    def list_turn_ids(self, thread_id: str, limit: int = 20) -> list: ...

    def read_turn(self, thread_id: str, turn_id: str) -> TurnInfo | None: ...

    def send_message(self, request_id: str, thread_id: str, message: str) -> dict: ...

    def get_operation(self, request_id: str) -> dict | None: ...

    def find_token(
        self, thread_id: str, token: str, *, limit: int = 200, turn_id: str | None = None
    ) -> TokenScan: ...

    def recipient_fingerprint(self, thread_id: str, *, window: int = 8) -> str: ...
