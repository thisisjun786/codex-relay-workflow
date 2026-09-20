"""Whether a task's recorded authorization still matches the role it holds.

The bridge can ask whether a stated pair is the pair a role runs on. It cannot ask whether a task
IS that role, because it reads no bindings: it only ever knows what a request claimed. This
package holds the bindings, so the half the bridge cannot answer lives here.

Two failures made that gap concrete. A task created citing one role and registered as another
passed every check that existed, because nothing compared the two. And a child sent a report to
its parent stating the parent's pair as it was recorded when the parent was created; the user had
since changed it, so the host reported something else and a correct message was withheld with a
diagnosis that pointed at the wrong thing. The record was not corrupt. It was out of date, and
nothing was looking.

So the role is a consistency check ON the recorded authorization, never a replacement for it.
What a send verifies against stays exactly what was authorized -- policy is not approval, and a
value observed on the host is evidence of what a thread is running, never a new approval. What
changes is that a record that has fallen behind the policy for its own role is now a named
refusal, decided before any transport call, with a recovery that says re-record it from a source
attributable to the user.

The policy is the bridge's file, read through the bridge's own parser so the two packages cannot
drift into two readings of one document. It is read from THIS process's environment, and the
daemon, the CLI and the hook are different processes: two of them reading two different files
would be a second policy source by deployment rather than by code, which no amount of care inside
either package would catch. That is why every finding here carries the digest it was decided
under, and why `doctor` compares its digest with the bridge's.
"""

import os

from .errors import DeliveryRefused, RefusalReason
from .linkage import LIVE, ROLE_SCOPE

ENVIRONMENT_VARIABLE = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"

RECOVERY = (
    "Re-record this task's authorized settings from a source attributable to the user, with "
    "relay settings record --source user_transition, once its current settings have been read."
)


class Unresolved:
    """No role policy is readable in this process. It refuses; it never passes silently.

    A role check that quietly does nothing when its policy is missing is the original failure
    with a green suite on top, so absence is an answer rather than a gap. It is not permanent:
    the refusal it produces is a pre-send withhold on the ordinary recheck cadence, so declaring
    the policy and restarting resumes every held delivery with nothing lost.
    """

    def __init__(self, detail: str):
        self.detail = detail
        self.digest = None

    def __bool__(self):
        return False


class Declared:
    """The roles this host declared, and the digest of the file they were read from."""

    def __init__(self, policy, digest):
        self._policy = policy
        self.digest = digest

    def __bool__(self):
        return True

    def expectation(self, role):
        return self._policy.role_expectation(role)


def declared(environ=None) -> "Declared | Unresolved":
    """Read the same file the bridge reads, through the same parser.

    Imported lazily, as every other bridge import in this package is: a relay process that never
    reaches a role question should not fail to start because the bridge is absent.
    """
    environ = os.environ if environ is None else environ
    configured = (environ.get(ENVIRONMENT_VARIABLE) or "").strip()
    if not configured:
        return Unresolved(
            f"{ENVIRONMENT_VARIABLE} is not set in this process, so no role policy can be read"
        )
    try:
        from codex_thread_bridge.execution import ExecutionPolicy, ExecutionPolicyError
    except ImportError as error:  # pragma: no cover - exercised by the import-failure test
        return Unresolved(f"the execution policy parser is unavailable: {error}")
    try:
        policy = ExecutionPolicy.from_file(configured)
    except ExecutionPolicyError as error:
        return Unresolved(str(error))
    if not policy.declares_roles:
        return Unresolved("this host's execution policy declares no roles")
    return Declared(policy, policy.summary().get("digest"))


def bound_role(store, task_id: str):
    """The role this task actually holds, or None. Read-only; it binds nothing.

    None means the task is bound to no scope, which is the ordinary state of work that has
    nothing to do with these three levels. Such a task is outside this policy entirely.
    """
    row = store.one(
        "SELECT role FROM scope_bindings WHERE task_id = ?"
        "   AND status IN (" + ",".join("?" * len(LIVE)) + ")"
        " ORDER BY updated_at DESC LIMIT 1",
        (task_id, *LIVE),
    )
    return row["role"] if row is not None else None


def _pair(settings) -> tuple:
    data = getattr(settings, "data", settings) or {}
    return data.get("model"), data.get("reasoningEffort")


def cited_role(settings):
    """The role the creation receipt said this task was made for, where one was recorded."""
    data = getattr(settings, "data", settings) or {}
    return data.get("citedRole")


def check_record(settings, role, policy) -> dict | None:
    """Has this task's recorded authorization fallen behind the policy for its own role?

    A supervisor never can: policy declares no pair for it, precisely because its model is the
    user's selection, so its recorded authorization IS the authority and there is nothing to
    compare it against.
    """
    expectation = policy.expectation(role)
    if expectation is None or expectation.expectation != "pair":
        return None
    model, effort = _pair(settings)
    if (model, effort) == (expectation.model, expectation.reasoning_effort):
        return None
    return {
        "code": RefusalReason.SETTINGS_RECORD_STALE_FOR_ROLE.value,
        "role": role,
        "recorded": {"model": model, "reasoningEffort": effort},
        "expected": {"model": expectation.model, "reasoningEffort": expectation.reasoning_effort},
        "digest": policy.digest,
        "recovery": RECOVERY,
    }


def check_binding(cited, bound, settings, policy) -> dict | None:
    """Does what this task was created as agree with what it is being bound as?

    Two independent disagreements, reported apart from the stale-record case on purpose. That
    one's recovery is "re-record", which applied here would write the wrong role's pair over a
    task and call the contradiction resolved.
    """
    if bound not in ROLE_SCOPE:
        return None
    if cited is not None and cited != bound:
        return {
            "code": RefusalReason.ROLE_BINDING_MISMATCH.value,
            "citedRole": cited,
            "boundRole": bound,
            "digest": policy.digest if policy else None,
            "detail": (
                f"this task was created citing role {cited!r} and is being bound as {bound!r}; "
                "one of the two is wrong, and re-recording its settings would only hide that"
            ),
        }
    if not policy:
        return None
    expectation = policy.expectation(bound)
    if expectation is None or expectation.expectation != "pair":
        return None
    model, effort = _pair(settings)
    if (model, effort) == (expectation.model, expectation.reasoning_effort):
        return None
    return {
        "code": RefusalReason.ROLE_BINDING_MISMATCH.value,
        "citedRole": cited,
        "boundRole": bound,
        "recorded": {"model": model, "reasoningEffort": effort},
        "expected": {"model": expectation.model, "reasoningEffort": expectation.reasoning_effort},
        "digest": policy.digest,
        "detail": (
            f"this task's recorded pair is not the pair role {bound!r} runs on, and it is being "
            "bound to that role now rather than having drifted afterwards"
        ),
    }


def refuse_unresolved(unresolved, role, task_id):
    """The one refusal that is about this process rather than about the task."""
    return DeliveryRefused(
        RefusalReason.ROLE_POLICY_UNCONFIGURED,
        f"{task_id!r} is bound as {role!r} and this process cannot read a role policy to check "
        f"its authorization against: {unresolved.detail}. Nothing was sent and no turn was "
        "started. Set the policy for this process and the held deliveries resume on the next "
        "pass.",
    )
