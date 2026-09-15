"""Time is an input, never a decision.

No state in this package advances because time passed. The clock exists so records
carry honest timestamps and so backoff schedules are testable, not so anything can
conclude from elapsed time.
"""

import time
from datetime import datetime, timezone


class SystemClock:
    def now(self) -> float:
        return time.time()

    def iso(self) -> str:
        return datetime.now(timezone.utc).isoformat(timespec="microseconds")


class FakeClock:
    """A clock the tests move by hand, so no test sleeps."""

    def __init__(self, start: float = 1_700_000_000.0):
        self._now = float(start)

    def now(self) -> float:
        return self._now

    def advance(self, seconds: float) -> float:
        self._now += float(seconds)
        return self._now

    def iso(self) -> str:
        return datetime.fromtimestamp(self._now, timezone.utc).isoformat(timespec="microseconds")
