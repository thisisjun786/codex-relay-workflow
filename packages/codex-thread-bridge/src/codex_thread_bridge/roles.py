"""Which pair a ROLE runs on, and why this file declares none of them itself.

The execution policy already asks two questions of every creation and every resume: was a model
and an effort stated, and does this host allow that pair. Neither is the question that was
actually being got wrong. CRW runs three levels -- a supervisor bound to an initiative, a parent
to a project, a child to an issue -- and each level is meant to run on a different pair. A project
parent created on someone else's model answered both existing questions correctly and was still
wrong, because nothing related the task's ROLE to the pair it was started on.

So this module adds the third question, IS IT THIS ROLE'S PAIR, and deliberately supplies no
answer to it. The role ids live here because two packages have to agree on them. The values do
not, for the same reason the allowlist does not: a pair written into code would be a second
source of truth competing with the operator's file, and it would answer the approval question on
a host that configured nothing -- which is exactly what PRESENCE_ONLY refuses to do. A role this
host's policy does not declare is refused, never defaulted.

The supervisor is the one role with no pair, and that is a rule rather than an omission. Its model
is the user's own selection, so the policy expresses it as {"expectation": "record"} and the
supervisor's authorization is whatever its own recorded settings say. An entry that tries to pin a
supervisor's model does not load at all. That is what makes "the parent policy is never propagated
to the supervisor" a mechanism instead of a sentence somebody has to remember, and it is worth the
asymmetry: every other role's pair is derived from policy, and a derived pair can be restored
safely where a chosen one cannot.
"""

# The three levels, spelled exactly as the relay's own scope bindings spell them. A conformance
# test in the relay asserts the two sets are equal, because the packages cannot import each
# other's vocabulary in both directions and a silent divergence here would mean a role that is
# enforced on one side and unknown on the other.
SUPERVISOR = "supervisor"
PARENT = "parent"
CHILD = "child"
ROLES = (SUPERVISOR, PARENT, CHILD)

ROLE_UNKNOWN = "execution_role_unknown"
ROLE_MISMATCH = "execution_role_mismatch"

# The supervisor's expectation. Not a pair, and not a wildcard either: it means the authorization
# is the one recorded for that task, from a source attributable to the user.
RECORD = "record"
PAIR = "pair"

ROLE_MAXIMUM = 64

_ENTRY_KEYS = {"model", "reasoningEffort", "expectation"}


class RoleExpectation:
    """What one role is expected to run on, and where that expectation comes from."""

    __slots__ = ("role", "model", "reasoning_effort", "expectation")

    def __init__(self, role, model=None, reasoning_effort=None, expectation=PAIR):
        self.role = role
        self.model = model
        self.reasoning_effort = reasoning_effort
        self.expectation = expectation

    def __eq__(self, other):
        return isinstance(other, RoleExpectation) and self.receipt() == other.receipt()

    def __repr__(self):
        return f"RoleExpectation({self.receipt()!r})"

    def receipt(self) -> dict:
        """What a caller is told about this role. Model ids are not secrets; paths are."""
        return {
            "role": self.role,
            "expectation": self.expectation,
            "model": self.model,
            "reasoningEffort": self.reasoning_effort,
        }


def _text(value, where, error):
    if not isinstance(value, str) or not value.strip() or len(value) > ROLE_MAXIMUM * 8:
        raise error(f"{where} must be a non-empty string")
    return value


def parse(declared, error) -> dict:
    """Read the policy file's roles section. Absent means no role is enforced on this host.

    The caller supplies the exception class it wants raised, which keeps this module from
    importing the policy module that imports it. Every refusal below stops the server at startup,
    exactly like every other unusable policy: a roles section that was configured and then
    silently ignored is the one outcome an operator would never detect.
    """
    if declared is None:
        return {}
    if not isinstance(declared, dict):
        raise error("roles must be a JSON object keyed by role id")
    parsed = {}
    for role, entry in declared.items():
        if role not in ROLES:
            raise error(f"{role!r} is not a role; supported are {sorted(ROLES)}")
        if not isinstance(entry, dict):
            raise error(f"role {role!r} must be an object")
        unknown = sorted(set(entry) - _ENTRY_KEYS)
        if unknown:
            raise error(
                f"role {role!r} has unknown keys {unknown}; supported are {sorted(_ENTRY_KEYS)}"
            )
        expectation = entry.get("expectation", PAIR)
        if expectation not in (PAIR, RECORD):
            raise error(f"role {role!r} expectation must be {PAIR!r} or {RECORD!r}")
        if role == SUPERVISOR:
            # Refused rather than ignored. A file that pins the supervisor's model was written by
            # someone who believes it will take effect, and loading it while discarding that
            # belief is worse than refusing to start.
            named = sorted({"model", "reasoningEffort"} & set(entry))
            if named:
                raise error(
                    f"role {SUPERVISOR!r} cannot declare {named}: its model and effort are the "
                    "user's own selection, so its expectation is the recorded authorization"
                )
            parsed[role] = RoleExpectation(role, expectation=RECORD)
            continue
        if expectation == RECORD:
            parsed[role] = RoleExpectation(role, expectation=RECORD)
            continue
        absent = sorted({"model", "reasoningEffort"} - set(entry))
        if absent:
            raise error(f"role {role!r} is missing {absent}")
        parsed[role] = RoleExpectation(
            role,
            _text(entry["model"], f"the model of role {role!r}", error),
            _text(entry["reasoningEffort"], f"the effort of role {role!r}", error),
        )
    return parsed
