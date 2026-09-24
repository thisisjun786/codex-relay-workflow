"""Retry cadence and the bounds that stop a failure becoming a nuisance.

Everything here is a schedule or a cap. None of it can change a delivery state by itself:
time decides WHEN something may be tried again, never WHETHER it already succeeded.
"""

from dataclasses import dataclass

ATTEMPT_CAP = "attempt_cap"
BUSY_CAP = "busy_cap"
PUSH_CHANNEL_CLOSED = "push_channel_closed"
RELATIONSHIP_INACTIVE = "relationship_inactive"
SUPERSEDED = "superseded"
RECIPIENT_UNDELIVERABLE = "recipient_undeliverable"
# The host accepted an attempt's turn/start and no longer has that turn: the recipient's own
# turn list holds no such turn among the turns begun since the send (hostloss.py, CRW-224). One
# word in three places, all about the same loss: the lost attempt's state, the dispatch evidence
# of a delivery queued again because of it, and the hold of a delivery whose redelivery was lost
# as well. It is never a delivery state - the delivery itself goes back to queued - which is why
# it lives with the hold reasons rather than beside transport's states. The attempt's frozen
# record keeps deliveryState dispatched, because turn/start did return that turn id; this word is
# what the host said about the turn afterwards.
HOST_LOST_TURN = "host_lost_turn"
# A dispatched completion whose turn check cannot reach an answer by waiting: the listing never
# reached the send, the token scan could not cover the turns begun since it, or the attempt has no
# send time. Recorded on the attempt as "turn_check_undecided:<reason>" (hostloss.record_undecided)
# and read by status as awaiting_ack:turn_check_undecided, so it is named rather than silent.
TURN_CHECK_UNDECIDED = "turn_check_undecided"
# An uncertain send the recipient keeps no trace of (hostloss.read_unknown_send, CRW-231). turn/start
# went out, no usable answer came back and no turn id with it, and once the send is past the start-time
# allowance the recipient's own turns and items since it, and the turn it could have been folded into,
# show nothing of it. The hold of such a delivery, which stays held_uncertain: nothing shows that a live
# App Server cannot still apply the turn/start, so it is never sent again, and the parent recovers the
# report; a message that turns up later still confirms it. Named on the attempt as
# "unknown_send_lost:no_trace". Never an attempt state and never dispatch evidence.
UNKNOWN_SEND_LOST = "unknown_send_lost"
# An uncertain send whose reading cannot decide however long the relay waits: the same reasons the turn
# check names (a listing that never reached the send, an empty listing, a token scan that could not cover
# the items since it, the token only in an item that is neither the message nor agent output, no send
# time). The delivery stays held_uncertain, so a message found later still confirms it, and carries this
# hold so that nothing reads it as a wait the daemon will end. The attempt names the reason as
# "unknown_send_undecided:<reason>".
UNKNOWN_SEND_UNDECIDED = "unknown_send_undecided"
# Why a recipient may not be woken now (RetryPolicy.pacing). Neither is a hold or a failure: the delivery
# waits and goes out once the reading reopens (I-225).
MIN_SEND_INTERVAL = "min_send_interval"
HOURLY_CAP = "hourly_cap"
RATE_WINDOW_SECONDS = 3600


def pacing_holding(pacing, next_eligible_at):
    """The pacing, when the recipient's budget is what holds the delivery; otherwise None.

    A delivery also waits out its own backoff (a busy recipient, a pre-send failure). When that ends
    later than the budget reopens, the backoff is what holds it, and naming the budget would give a
    reopen time the delivery cannot use (Devin on bb1b6af6). A budget that never reopens (a cap of
    zero) holds it whatever its backoff says.
    """
    if pacing is None:
        return None
    if (pacing["reopensAt"] is None or next_eligible_at is None
            or next_eligible_at <= pacing["reopensAt"]):
        return pacing
    return None


@dataclass(frozen=True)
class RetryPolicy:
    busy_base_seconds: float = 15.0
    busy_max_seconds: float = 300.0
    busy_max_attempts: int = 40
    presend_base_seconds: float = 30.0
    presend_max_seconds: float = 900.0
    max_attempts: int = 6
    min_send_interval_seconds: float = 5.0
    max_sends_per_recipient_per_hour: int = 12
    lifecycle_recheck_seconds: float = 60.0
    lease_seconds: float = 300.0
    poll_interval_seconds: float = 20.0
    max_sends_per_tick: int = 4
    max_reconciles_per_tick: int = 8
    # Turn reads are the scarce thing in an observation pass, so they are capped directly
    # rather than implied by a per-relationship slice that grows with the relationship count.
    max_turn_reads_per_tick: int = 8
    # Recipient-turn lookups for delivered completions still awaiting their acknowledgement
    # (daemon._check_dispatched_turns). Separate from the observation reads above: those watch
    # children, these ask a parent whether the turn a delivery started still exists.
    max_turn_checks_per_tick: int = 4
    min_relationship_share: int = 2
    max_sends_per_parent_per_tick: int = 2
    # The automatic supervisor pass (CRW-215). Projects are re-derived per tick, so their count
    # is capped as well as the sends: a store with many projects must not make one tick long.
    max_supervisor_projects_per_tick: int = 4
    max_supervisor_sends_per_tick: int = 2
    # How long after the relay settles a turn an omission derived from the store waits before
    # it is owed (omitted.classify). Long enough for the parent - or the child, steered - to
    # answer it first; every tick inside it stages nothing for that turn.
    omission_grace_seconds: float = 300.0
    # Supervision cadence. A worker is bounded by segment_seconds; the supervisor replaces it,
    # which is what carries an assignment past any single process lifetime.
    segment_seconds: float = 3600.0
    restart_interval_seconds: float = 2.0
    restart_base_seconds: float = 2.0
    restart_backoff_max_seconds: float = 300.0
    repeat_failure_threshold: int = 3

    def restart_delay_for(self, consecutive_failures: int) -> float:
        """A clean segment waits the plain interval; repeated failure backs off and caps."""
        import math

        if consecutive_failures <= 0:
            return self.restart_interval_seconds
        base, ceiling = self.restart_base_seconds, self.restart_backoff_max_seconds
        # Bounded before the exponent is evaluated, because a supervisor whose worker fails on
        # every segment keeps counting and base * 2 ** (n - 1) eventually builds an integer too
        # large to convert to a float - so the supervisor would die of its own backoff instead
        # of retrying at the cap. The bound is DERIVED from this policy rather than fixed: a
        # small base needs more doublings to reach its ceiling, and a constant would have sent
        # a customized supervisor straight to the cap while its own backoff still had room.
        if base <= 0 or ceiling <= base:
            return ceiling
        if consecutive_failures - 1 >= math.ceil(math.log2(ceiling / base)):
            return ceiling
        return min(
            self.restart_backoff_max_seconds,
            self.restart_base_seconds * (2 ** (consecutive_failures - 1)),
        )

    def delay_for(self, attempt_no: int, reason: str) -> float:
        """Capped exponential backoff, so repeated failure slows down instead of hammering."""
        if reason == "busy":
            base, ceiling = self.busy_base_seconds, self.busy_max_seconds
        else:
            base, ceiling = self.presend_base_seconds, self.presend_max_seconds
        return min(ceiling, base * (2 ** max(0, attempt_no - 1)))

    def cap_for(self, reason: str) -> int:
        return self.busy_max_attempts if reason == "busy" else self.max_attempts

    def cap_reason(self, reason: str) -> str:
        return BUSY_CAP if reason == "busy" else ATTEMPT_CAP

    def rate_windows(self, now: float) -> tuple:
        """The hour window a send at now is counted in, and the earliest window the gap reaches.

        The gap is read across windows, because last_send_at lives on the hour's row and two sends a
        second apart can straddle the boundary; only the windows the gap can reach are read, never a
        later one (delivery.send_refusal).
        """
        window = int(now // RATE_WINDOW_SECONDS) * RATE_WINDOW_SECONDS
        reach = RATE_WINDOW_SECONDS * (1 + int(self.min_send_interval_seconds // RATE_WINDOW_SECONDS))
        return window, window - reach

    def pacing(self, now: float, *, sends, last):
        """Why a recipient may not be woken at now, and when that reading reopens, or None.

        sends is the count in now's window and last the newest send the gap reaches, as rate_windows
        bounds them. The one rule every reader applies, so status, assignment-show and the claim cannot
        disagree about why a delivery waits (CRW-124 H7, O-H0R4-2). A spent cap is named before the
        gap: when both hold, the cap is what keeps the delivery waiting, and its window's end (or the
        gap's, if later) is when it reopens. reopensAt is None for a cap of zero, which refuses every
        send in every window, and says so. A clock-ahead row in a later window can still refuse the
        send at reopensAt; that is read then, like every other pacing.
        """
        window, _earliest = self.rate_windows(now)
        sends = sends or 0
        cap = self.max_sends_per_recipient_per_hour
        gap_ends = None if last is None else last + self.min_send_interval_seconds
        reading = {"sends": sends, "cap": cap, "windowStart": window}
        if sends >= cap:
            if cap <= 0:
                return {"reason": HOURLY_CAP, "reopensAt": None, **reading,
                        "detail": "a cap of zero refuses every send; only a changed policy"
                                  " reopens it"}
            reopens = max(window + RATE_WINDOW_SECONDS, gap_ends or 0)
            return {"reason": HOURLY_CAP, "reopensAt": reopens, **reading}
        if gap_ends is not None and now < gap_ends:
            return {"reason": MIN_SEND_INTERVAL, "reopensAt": gap_ends, **reading}
        return None
