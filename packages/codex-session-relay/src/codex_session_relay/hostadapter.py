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

    other_turn and other_kind name the first item the scan passed that carried the token but is
    neither the message nor agent output (find_token_in): a hook prompt, or a type the relay
    does not know. Such an item is not taken for the message, and not dismissed either.
    """

    found: bool
    turn_id: str | None
    exhausted: bool
    scanned: int
    other_turn: str | None = None
    other_kind: str | None = None


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
# How far back a dispatched-turn lookup pages before it gives up (1000 turns at the default page,
# ids and start times only). Checks run from 61 s after a send, so a parent has rarely begun more
# than a few turns by then; the bound is for a relay that was down while its parents kept working.
DISPATCHED_TURN_MAX_PAGES = 20
# The item type the host gives the message a turn/start delivered (App Server ThreadItem).
USER_MESSAGE = "userMessage"
# Every ThreadItem type App Server 0.154.0 names for the agent's own work, or for text the host
# derived from it. A relay command that prints a request id (status, show, reconcile), a file the
# parent wrote, a compaction summary: all of it is the parent reading or writing about the
# delivery, never the delivery (CRW-224 follow-up, H0R3; fileChange from review 1). The schema's
# two other types are userMessage and hookPrompt, which is input, not output.
AGENT_OUTPUT = frozenset({
    "agentMessage", "collabAgentToolCall", "commandExecution", "contextCompaction",
    "dynamicToolCall", "enteredReviewMode", "exitedReviewMode", "fileChange",
    "functionCallOutput", "imageGeneration", "imageView", "mcpToolCall", "plan", "reasoning",
    "sleep", "subAgentActivity", "webSearch",
})


def is_message(kind) -> bool:
    """Positive evidence that a delivery arrived: the host typed the item as a user message.

    An untyped item still counts, because a host that reports no types has always been read by
    its text alone.
    """
    return kind is None or kind == USER_MESSAGE


def may_be_message(kind) -> bool:
    """What a loss scan cannot pass over: anything the host did not type as agent output.

    Wider than is_message on purpose. A token in a hook prompt, or in a type the relay does not
    know, may be the message or an echo of it: the scan neither counts it as the message nor
    passes over it, and the reading it leads to is named instead of a loss (hostloss.py).
    """
    return kind not in AGENT_OUTPUT


class ListingBounded(HostUnavailable):
    """The bounded listing never reached the send.

    Not evidence of absence, like any HostUnavailable - but unlike a failed read, waiting does not
    change it, so the relay records it by name instead of retrying it as if it were transient.
    """


class ListingEmpty(HostUnavailable):
    """The recipient listed no turns at all.

    Not evidence of absence either: a thread the host answers for without its turns lists the
    same. But a parent whose only turn was the lost delivery lists exactly this for good, so the
    relay names it once the send is past the start-time allowance instead of retrying it as if it
    were transient.
    """


@dataclass(frozen=True)
class TurnPresence:
    """What a recipient's own turn list says about the turn a dispatch started.

    present: the listing holds the turn. absent: the listing ended, or reached a turn that began
    before the send, without it; stop says which. Anything short of either is not an answer and
    raises HostUnavailable instead: an empty listing, or a bounded scan that never reached the
    send, has not shown that the turn is gone.

    seen names every listed turn read before the stop: the turns begun since the send, which is
    where the delivered message could be if it is anywhere.

    older names the listed turns known to have begun before the send: the one the listing stopped
    at and the older ones on its page. Only their items end a token scan (find_token_in).
    A match reads on to the first of them as well, never counting the matched turn, so a listed
    turn that turns out to lack its message can be judged like an absent one. Reading on that
    fails or reaches the bound first leaves older empty and the match standing.
    """

    finding: str
    turn: TurnInfo | None
    scanned: int
    stop: str
    seen: tuple = ()
    older: tuple = ()


def find_in_listing(pages, turn_id: str, sent_at: float) -> TurnPresence:
    """The one rule both adapters apply to a newest-first turn listing.

    pages yields (turns, another_page_follows), each page's TurnInfo newest first. The id is
    compared before the cutoff: a start that steered a turn begun before the send returns that
    turn's id, and that turn was the newest the thread had, so no older turn precedes it. A turn
    with no start time never serves as the cutoff.
    """
    cutoff = sent_at - TURN_START_PRECISION_SECONDS - DISPATCH_TURN_SKEW_SECONDS
    scanned = 0
    seen = []
    pages = iter(pages)
    for turns, follows in pages:
        for index, turn in enumerate(turns):
            scanned += 1
            if turn.turn_id == turn_id:
                older = _older_after_match(turns[index + 1:], follows, pages, cutoff)
                return TurnPresence(TURN_PRESENT, turn, scanned, "matched", tuple(seen), older)
            if turn.started_at is not None and turn.started_at <= cutoff:
                return TurnPresence(TURN_ABSENT, None, scanned, "older_than_send", tuple(seen),
                                    _older(turns[index:], cutoff))
            seen.append(turn.turn_id)
        if not follows:
            if not scanned:
                raise ListingEmpty(
                    "the recipient's turn list is empty, which does not show that a turn is gone"
                )
            return TurnPresence(TURN_ABSENT, None, scanned, "listing_end", tuple(seen))
    raise ListingBounded(
        f"turn {turn_id!r} was not among {scanned} turns and the bounded listing never reached "
        "the send; this is not evidence of absence"
    )


def _older(turns, cutoff) -> tuple:
    return tuple(turn.turn_id for turn in turns
                 if turn.started_at is not None and turn.started_at <= cutoff)


def _older_after_match(rest, follows, pages, cutoff) -> tuple:
    """After a match, the turns begun before the send: read on to the first of them.

    The match is already the answer, so nothing here may take it away: a failed read or the page
    bound reached first gives (), which leaves a later token scan to the end of the items or its
    own bound.
    """
    try:
        while True:
            for index, turn in enumerate(rest):
                if turn.started_at is not None and turn.started_at <= cutoff:
                    return _older(rest[index:], cutoff)
            if not follows:
                return ()
            rest, follows = next(pages)
    except StopIteration:
        return ()
    except Exception:  # noqa: BLE001 - the match stands; only the extra history is missing
        return ()


def find_token_in(pages, token: str, older) -> TokenScan:
    """A token among a thread's items since a send, newest first: the rule both adapters apply.

    pages yields (items, another_page_follows), each item a (turn id, text, type) triple, newest
    first; the type is the host's item type, or None when it gives none.
    older names the turns the listing showed began before the send (TurnPresence.older).
    exhausted means covered: the scan reached an item of one of them, or the end of the items,
    so every newer item was read. An item of any other turn is read, listed or not: a turn the
    host dropped from its list can keep its items, and taking one for older history stopped the
    scan in front of a token that was there (review 6). A scan that stops at its bound first has
    not covered them, and not finding the token there shows nothing (I-42).

    Only a user message (is_message) is found. Agent output is passed over: a relay command that
    printed the request id, or a file the parent wrote with it, is not the message, and a loss
    such an echo vetoed was reported as delivered (H0R3, review 1). The scan also goes on past an
    item of any other type that carries the token (a hook prompt, a type the relay does not
    know), and reports the first one as other_turn and other_kind: neither the message nor
    absence, so the caller names it rather than concluding either.
    """
    boundary = set(older)
    scanned = 0
    other = (None, None)
    for items, follows in pages:
        for owner, text, kind in items:
            if owner is not None and owner in boundary:
                return TokenScan(False, None, True, scanned, *other)
            scanned += 1
            if not may_be_message(kind) or token not in text:
                continue
            if is_message(kind):
                return TokenScan(True, owner, False, scanned)
            if other == (None, None):
                other = (owner, kind)
        if not follows:
            return TokenScan(False, None, True, scanned, *other)
    return TokenScan(False, None, False, scanned, *other)


def find_token_in_turn_items(pages, token: str, turn_id: str) -> TokenScan:
    """This attempt's message among one turn's own items, oldest first: the rule both adapters
    apply to thread/items/list filtered by that turn.

    pages yields (items, another_page_follows) with items as in find_token_in. An item counts only
    when the host says it belongs to that turn and typed it as a user message (is_message): a
    host that ignored the turn filter answers with other turns' items, and an echo of the request
    id in the turn's own command output is not the message. The message is a turn's first item,
    so a few items read are enough, and a turn that does not have it is left to the thread-wide
    reading (hostloss.py) rather than concluded lost here.
    """
    scanned = 0
    for items, follows in pages:
        for owner, text, kind in items:
            scanned += 1
            if owner == turn_id and is_message(kind) and token in text:
                return TokenScan(True, owner, False, scanned)
        if not follows:
            return TokenScan(False, None, True, scanned)
    return TokenScan(False, None, False, scanned)


class HostAdapter(Protocol):
    def read_thread(self, thread_id: str) -> ThreadFacts: ...

    def is_archived(self, thread_id: str, *, cwd: str | None = None) -> bool | None: ...

    def read_goal_status(self, thread_id: str) -> str | None: ...

    def list_turn_ids(self, thread_id: str, limit: int = 20) -> list: ...

    def read_turn(self, thread_id: str, turn_id: str) -> TurnInfo | None: ...

    def find_dispatched_turn(
        self, thread_id: str, turn_id: str, *, sent_at: float
    ) -> TurnPresence: ...

    def find_token_since(
        self, thread_id: str, token: str, *, older, limit: int = 200
    ) -> TokenScan: ...

    def send_message(self, request_id: str, thread_id: str, message: str) -> dict: ...

    def get_operation(self, request_id: str) -> dict | None: ...

    def find_token(
        self, thread_id: str, token: str, *, limit: int = 200, turn_id: str | None = None,
        message_only: bool = False,
    ) -> TokenScan: ...

    def find_token_in_turn(
        self, thread_id: str, token: str, *, turn_id: str, limit: int = 8
    ) -> TokenScan: ...

    def recipient_fingerprint(self, thread_id: str, *, window: int = 8) -> str: ...
