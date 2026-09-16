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
    min_relationship_share: int = 2
    max_sends_per_parent_per_tick: int = 2
    # Supervision cadence. A worker is bounded by segment_seconds; the supervisor replaces it,
    # which is what carries an assignment past any single process lifetime.
    segment_seconds: float = 3600.0
    restart_interval_seconds: float = 2.0
    restart_base_seconds: float = 2.0
    restart_backoff_max_seconds: float = 300.0
    repeat_failure_threshold: int = 3

    def restart_delay_for(self, consecutive_failures: int) -> float:
        """A clean segment waits the plain interval; repeated failure backs off and caps."""
        if consecutive_failures <= 0:
            return self.restart_interval_seconds
        # Bounded before the exponent is evaluated. A supervisor whose worker fails on every
        # segment keeps counting, and past about a thousand failures base * 2 ** (n - 1) is an
        # integer too large to convert to a float - so the supervisor would die of its own
        # backoff instead of continuing to retry at the cap.
        if consecutive_failures > 64:
            return self.restart_backoff_max_seconds
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
