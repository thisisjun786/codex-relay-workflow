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


# How far before the relay's own send stamp a turn's start may read and still be the turn that
# send started. The host reports whole seconds and the relay microseconds (ack.certainly_before
# allows the same second); the skew covers two clocks on one host. A turn that began earlier than
# this is older than the send, so a newest-first listing that reaches it has passed the place the
# dispatched turn would be.
TURN_START_PRECISION_SECONDS = 1.0
DISPATCH_TURN_SKEW_SECONDS = 60.0
TURN_PRESENT = "present"
TURN_ABSENT = "absent"


@dataclass(frozen=True)
class TurnPresence:
    """What a recipient's own turn list says about the turn a dispatch started.

    present: the listing holds the turn. absent: the listing ended, or reached a turn that began
    before the send, without it; stop says which. Anything short of either is not an answer and
    raises HostUnavailable instead: an empty listing, or a bounded scan that never reached the
    send, has not shown that the turn is gone.
    """

    finding: str
    turn: TurnInfo | None
    scanned: int
    stop: str


def find_in_listing(pages, turn_id: str, sent_at: float) -> TurnPresence:
    """The one rule both adapters apply to a newest-first turn listing.

    pages yields (turns, another_page_follows), each page's TurnInfo newest first. The id is
    compared before the cutoff: a start that steered a turn begun before the send returns that
    turn's id, and that turn was the newest the thread had, so no older turn precedes it. A turn
    with no start time never serves as the cutoff.
    """
    cutoff = sent_at - TURN_START_PRECISION_SECONDS - DISPATCH_TURN_SKEW_SECONDS
    scanned = 0
    for turns, follows in pages:
        for turn in turns:
            scanned += 1
            if turn.turn_id == turn_id:
                return TurnPresence(TURN_PRESENT, turn, scanned, "matched")
            if turn.started_at is not None and turn.started_at <= cutoff:
                return TurnPresence(TURN_ABSENT, None, scanned, "older_than_send")
        if not follows:
            if not scanned:
                raise HostUnavailable(
                    "the recipient's turn list is empty, which does not show that a turn is gone"
                )
            return TurnPresence(TURN_ABSENT, None, scanned, "listing_end")
    raise HostUnavailable(
        f"turn {turn_id!r} was not among {scanned} turns and the bounded listing never reached "
        "the send; this is not evidence of absence"
    )


class HostAdapter(Protocol):
    def read_thread(self, thread_id: str) -> ThreadFacts: ...

    def is_archived(self, thread_id: str, *, cwd: str | None = None) -> bool | None: ...

    def read_goal_status(self, thread_id: str) -> str | None: ...

    def list_turn_ids(self, thread_id: str, limit: int = 20) -> list: ...

    def read_turn(self, thread_id: str, turn_id: str) -> TurnInfo | None: ...

    def find_dispatched_turn(
        self, thread_id: str, turn_id: str, *, sent_at: float
    ) -> TurnPresence: ...

    def send_message(self, request_id: str, thread_id: str, message: str) -> dict: ...

    def get_operation(self, request_id: str) -> dict | None: ...

    def find_token(
        self, thread_id: str, token: str, *, limit: int = 200, turn_id: str | None = None
    ) -> TokenScan: ...

    def recipient_fingerprint(self, thread_id: str, *, window: int = 8) -> str: ...
