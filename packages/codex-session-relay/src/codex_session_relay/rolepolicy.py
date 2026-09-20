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

    def exception_pair(self, name):
        return self._policy.exception_pair(name)


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


class Contested:
    """This task holds live bindings at more than one role. It refuses; it never picks.

    One task, one role is a rule the binding path enforces, so more than one live row is a store
    that contradicts itself. Choosing between them by recency would compare a record against an
    arbitrary role and report a clean answer, which is the one outcome worth refusing: the
    linkage readers already refuse to guess through a multi-owner store for the same reason.
    """

    def __init__(self, roles):
        self.roles = sorted(roles)


def bound_role(store, task_id: str):
    """The role this task actually holds, or None, or Contested. Read-only; it binds nothing.

    None means the task is bound to no scope, the ordinary state of work that has nothing to do
    with these three levels, and such a task is outside this policy entirely. superseded_by is
    part of the predicate because a superseded row is a former owner, and reading one as live
    would enforce a role the task no longer holds.
    """
    rows = store.all(
        "SELECT DISTINCT role FROM scope_bindings WHERE task_id = ?"
        "   AND status IN (" + ",".join("?" * len(LIVE)) + ")"
        "   AND superseded_by IS NULL",
        (task_id, *LIVE),
    )
    roles = {row["role"] for row in rows}
    if not roles:
        return None
    if len(roles) > 1:
        return Contested(roles)
    return next(iter(roles))


def bound_role_in(db, task_id: str):
    """bound_role against an already-open connection, so a caller can decide inside its write.

    Same predicate, deliberately. Two spellings of one rule is how the read a caller validates
    against drifts away from the read everybody else performs.
    """
    rows = db.execute(
        "SELECT DISTINCT role FROM scope_bindings WHERE task_id = ?"
        "   AND status IN (" + ",".join("?" * len(LIVE)) + ")"
        "   AND superseded_by IS NULL",
        (task_id, *LIVE),
    ).fetchall()
    roles = {row["role"] for row in rows}
    if not roles:
        return None
    if len(roles) > 1:
        return Contested(roles)
    return next(iter(roles))


def _pair(settings) -> tuple:
    data = getattr(settings, "data", settings) or {}
    return data.get("model"), data.get("reasoningEffort")


def cited_role(settings):
    """The role the creation receipt said this task was made for, where one was recorded."""
    data = getattr(settings, "data", settings) or {}
    return data.get("citedRole")


def cited_exception(settings):
    """The operator exception the creation receipt said authorized this pair, where one did."""
    data = getattr(settings, "data", settings) or {}
    return data.get("citedException")


def authorized_by_exception(settings, role, policy):
    """Was this record's pair authorized by an operator exception written for this role?

    The bridge deliberately lets a role-scoped exception replace the role-pair comparison, so a
    task legitimately created that way carries a pair its role's policy does not declare. Without
    this, registering one would be refused for being exactly what the operator approved.

    Verified rather than trusted: the exception is looked up in the same policy file and has to
    name this role and this pair. A record citing an id nobody wrote, or one written for another
    role, or one whose pair does not match, gets no exemption.
    """
    if not policy:
        return False
    entry = policy.exception_pair(cited_exception(settings))
    if entry is None or entry.get("role") != role:
        return False
    return _pair(settings) == (entry["model"], entry["reasoningEffort"])


def check_record(settings, role, policy) -> dict | None:
    """Has this task's recorded authorization fallen behind the policy for its own role?

    A supervisor never can: policy declares no pair for it, precisely because its model is the
    user's selection, so its recorded authorization IS the authority and there is nothing to
    compare it against.

    A role this policy does not declare AT ALL is a different answer and refuses, the same way
    the bridge refuses a cited role it has no entry for. Treating it as nothing to check would
    let a parent-only policy silently exempt every child on the host, which is a partial policy
    failing open.
    """
    expectation = policy.expectation(role)
    if expectation is None:
        return {
            "code": RefusalReason.ROLE_POLICY_UNCONFIGURED.value,
            "role": role,
            "digest": policy.digest,
            "undeclared": True,
            "recovery": f"declare role {role!r} in this host's execution policy",
        }
    if expectation.expectation != "pair":
        return None
    model, effort = _pair(settings)
    if (model, effort) == (expectation.model, expectation.reasoning_effort):
        return None
    if authorized_by_exception(settings, role, policy):
        # Approved under a named exception this same policy declares for this same role. Not a
        # record that fell behind; a record that was never supposed to match the role pair.
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
    if expectation is None:
        return {
            "code": RefusalReason.ROLE_POLICY_UNCONFIGURED.value,
            "citedRole": cited,
            "boundRole": bound,
            "digest": policy.digest,
            "detail": (
                f"this host's execution policy declares no role {bound!r}, so a task cannot be "
                "bound to it and checked; declare it before binding"
            ),
        }
    if expectation.expectation != "pair":
        return None
    model, effort = _pair(settings)
    if (model, effort) == (expectation.model, expectation.reasoning_effort):
        return None
    if authorized_by_exception(settings, bound, policy):
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


def refuse_contested(contested, task_id):
    """A store that says this task holds two roles at once cannot be checked against either."""
    return DeliveryRefused(
        RefusalReason.ROLE_BINDING_MISMATCH,
        f"{task_id!r} holds live bindings at {contested.roles}, and one task holds one role. "
        "Nothing was sent and no turn was started, because checking its authorization against "
        "either of them would report a clean answer derived from an arbitrary choice. Resolve "
       "the bindings first.",
   )


def check_unloaded_transmission(settings, role, policy, runtime_status):
    """The refusal for transmitting a pair policy did not derive to a thread not yet loaded.

    Returns None or the refusal, like every other check here, so a caller raises what it is
    given rather than reconstructing it from a yes or no.

    The bridge applies this rule on its own tool path and a relay delivery never takes that
    path: it resumes through its own transport. So the rule is applied here too, with the same
    predicate and the same reason. Where the role's pair comes from policy the record has
    already been compared against it, so a host that applies what it was sent lands the thread
    where policy says it belongs. A supervisor's pair is the user's own selection and the
    transmitted pair is whatever was recorded, which is exactly what goes stale when they change
    it; a pair admitted only by an operator exception is a file entry that can fall behind the
    same way.
    """
    if runtime_status != "notLoaded" or not policy:
        return None
    expectation = policy.expectation(role)
    derived = (
        expectation is not None
        and expectation.expectation == "pair"
        and _pair(settings) == (expectation.model, expectation.reasoning_effort)
    )
    if derived:
        return None
    return DeliveryRefused(
        RefusalReason.ROLE_POLICY_UNCONFIGURED,
        f"{task_id_of(settings)} is bound as {role!r}, the host reports it as notLoaded, and "
        f"the pair this send would transmit was not derived from a declared role pair (policy "
        f"{policy.digest}). Nothing was sent and no turn was started: a resume may apply what it "
        "transmits to a thread the host has to load first, which would restore a pair the user "
        "may have changed. Send once the host has the thread loaded, or read its current "
        "settings and re-record the authorization from that reading.",
    )


def task_id_of(settings) -> str:
    """Whatever names this record in a message. The cwd is what a reader recognises it by."""
    data = getattr(settings, "data", settings) or {}
    return repr(data.get("cwd") or "this recipient")
