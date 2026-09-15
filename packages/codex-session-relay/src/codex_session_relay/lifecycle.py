"""Whether a task can actually receive a message right now.

The relay's own relationship status is an AUTHORIZATION record: it says whether WE are allowed
to deliver. It says nothing about what the user did in their client. Someone can archive or
pause a task without ever touching this relay, so deliverability is observed from the host.

Three separate reads are needed because no single one answers the question. Thread status
carries the runtime state and has no archived flag; archived comes from the thread list filter;
paused, usage-limited and budget-limited come from the goal status. None is ever written back.
"""

from dataclasses import dataclass

from .policy import RECIPIENT_UNDELIVERABLE

ARCHIVED = "recipient_archived"
PAUSED = "recipient_paused"
USAGE_LIMITED = "recipient_usage_limited"
BUDGET_LIMITED = "recipient_budget_limited"
CANNOT_ACCEPT = "recipient_cannot_accept_input"
BUSY = "recipient_busy"
SYSTEM_ERROR = "recipient_system_error"
UNKNOWN = "lifecycle_unknown"

BLOCKING_GOAL_STATUS = {
    "paused": PAUSED,
    "usageLimited": USAGE_LIMITED,
    "budgetLimited": BUDGET_LIMITED,
}


@dataclass(frozen=True)
class Lifecycle:
    task_id: str
    runtime_status: str | None
    archived: bool | None
    goal_status: str | None
    can_accept_input: bool | None
    deliverable: str
    withhold_reason: str | None
    detail: str = ""

    @property
    def may_send(self) -> bool:
        return self.deliverable == "yes"

    @property
    def is_busy(self) -> bool:
        return self.deliverable == "busy"


def observe(adapter, task_id, *, cwd=None, require_evidence: bool = True) -> Lifecycle:
    """Read the host, decide, and never guess.

    With require_evidence set, a read that fails leaves the delivery withheld rather than sent,
    because deliverability needs positive evidence. That is a deferral and not a dead end: the
    next observation that succeeds releases it.
    """
    runtime = archived = goal = accepts = None
    problems = []
    try:
        facts = adapter.read_thread(task_id)
        runtime, accepts = facts.runtime_status, facts.can_accept_input
    except Exception as error:
        problems.append(f"thread read failed: {type(error).__name__}: {error}")
    try:
        archived = adapter.is_archived(task_id, cwd=cwd)
    except Exception as error:
        problems.append(f"archived check failed: {type(error).__name__}: {error}")
    try:
        goal = adapter.read_goal_status(task_id)
    except Exception as error:
        problems.append(f"goal read failed: {type(error).__name__}: {error}")

    detail = "; ".join(problems)

    def decide(state, reason):
        return Lifecycle(task_id, runtime, archived, goal, accepts, state, reason, detail)

    if archived is True:
        return decide("no", ARCHIVED)
    if goal in BLOCKING_GOAL_STATUS:
        return decide("no", BLOCKING_GOAL_STATUS[goal])
    if accepts is False:
        return decide("no", CANNOT_ACCEPT)
    if runtime == "systemError":
        return decide("no", SYSTEM_ERROR)
    if runtime == "active":
        return decide("busy", BUSY)
    if problems and require_evidence:
        # An absent goal is not a paused goal, but an unreadable host is not an idle one.
        return decide("unknown", UNKNOWN)
    if archived is None and require_evidence:
        # None means the archive state was not established, which is not the same as knowing
        # the task is not archived. A bounded or inconclusive listing lands here too, and
        # guessing "not archived" is exactly the guess that would wake an archived task.
        return decide("unknown", UNKNOWN)
    if runtime in ("idle", "notLoaded"):
        return decide("yes", None)
    if runtime is None and not require_evidence:
        return decide("yes", None)
    return decide("unknown", UNKNOWN)


def record(store, clock, observation: Lifecycle) -> None:
    with store.transaction() as db:
        db.execute(
            "INSERT INTO recipient_lifecycle (task_id, runtime_status, archived, goal_status,"
            " can_accept_input, deliverable, withhold_reason, detail, observed_at)"
            " VALUES (?,?,?,?,?,?,?,?,?)"
            " ON CONFLICT(task_id) DO UPDATE SET runtime_status=excluded.runtime_status,"
            " archived=excluded.archived, goal_status=excluded.goal_status,"
            " can_accept_input=excluded.can_accept_input, deliverable=excluded.deliverable,"
            " withhold_reason=excluded.withhold_reason, detail=excluded.detail,"
            " observed_at=excluded.observed_at",
            (
                observation.task_id, observation.runtime_status,
                None if observation.archived is None else int(observation.archived),
                observation.goal_status,
                None if observation.can_accept_input is None else int(observation.can_accept_input),
                observation.deliverable, observation.withhold_reason, observation.detail,
                clock.iso(),
            ),
        )


def hold_reason_for(observation: Lifecycle) -> str | None:
    """A recipient that cannot receive holds the delivery; it is never a delivery failure."""
    if observation.deliverable == "no":
        return RECIPIENT_UNDELIVERABLE
    return None
