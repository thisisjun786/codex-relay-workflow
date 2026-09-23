"""Which model and reasoning effort a task may start on, and who is allowed to decide that.

One creation call omitted its model argument. thread/start does not require one, so the App Server
started the task on its own configured default and paid inference began before anyone could see
which model was answering. Nothing in the protocol refuses that: an omitted setting is simply not
transmitted, and SettingsContract never compares a field the caller did not request, so the receipt
reported the inherited default as `actual` without ever calling it wrong.

Two questions follow from that, and they need separate answers.

WAS IT STATED? Enforced on every host, in every mode, with no way to switch it off. A creation or a
resume that does not carry both a model and a reasoning effort is refused before any RPC. The
failure being fixed was an omission, and a guard you can forget to enable is not a guard against
forgetting.

WAS IT APPROVED? Only a host that configured a policy file can answer that, so the allowlist is
compared only where one exists, and every receipt says which of the two modes was in force rather
than letting a reader assume the stronger one.

IS IT THIS ROLE'S PAIR? A later failure showed the first two questions can both be answered
correctly by a task that is still on the wrong model, because CRW runs three levels and each is
meant to run on a different pair. A caller may therefore name the role it is creating for, and a
named role is checked against the pair this host's policy declares for it. Naming one is opt-in,
so nothing that does not name a role behaves differently than before; but naming one this host
does not declare is refused rather than defaulted, for the same reason an undeclared exception is:
the point of asking is to be checked by someone other than yourself. See `roles`.

The allowlist deliberately cannot be reached from a tool argument. It is read once, in the server's
main(), from a file named by this process's own environment, and handed to the Bridge as a
constructor argument with no setter. A caller's entire input surface is the tool parameter list, so
there is no parameter through which it could widen its own allowance. That is the point: a caller
that can approve itself has not been checked by anyone.

An exception exists for the case where the user really does authorize a different pair for one
task, and it is a NAME rather than a value. The operator writes the id, the one model, the one
effort and the exact directories it covers into the file; a caller may cite that id and nothing
else. An id nobody wrote is refused, an id whose pair or directory does not match is refused, and
neither the file path nor the operator's note is ever returned to the caller.

What this cannot do is worth stating plainly. It constrains what this bridge requests and what the
host reports back. The host echoes an arbitrary effort string unchanged, so agreement means the
request was recorded, never that a provider served it. Anyone able to rewrite the policy file or
this process's environment can change the allowlist, and another client of the same App Server is
not covered at all.
"""

import hashlib
import json
from pathlib import Path
from typing import NamedTuple

from . import roles
from .roles import ROLE_MISMATCH, ROLE_UNKNOWN

ENVIRONMENT_VARIABLE = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"
# The digest the file was registered under, when whoever started this process knows one. The
# plugin's launcher checks the file before it execs, and this closes the interval between that
# check and the read below: the digest that counts is the digest of the bytes actually parsed.
# It can only refuse. Leaving it unset changes nothing, and no value of it widens anything.
DIGEST_VARIABLE = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST"

SETTING_MISSING = "execution_setting_missing"
SETTING_INVALID = "execution_setting_invalid"
NOT_ALLOWED = "execution_not_allowed"
EXCEPTION_UNKNOWN = "execution_exception_unknown"
EXCEPTION_OUT_OF_SCOPE = "execution_exception_out_of_scope"
POLICY_UNREADABLE = "execution_policy_unreadable"

# How a pair came to be authorized. Only the first means "this was compared against the pair the
# policy declares for this role"; the other two are approvals that deliberately did not ask that
# question, and a caller that needs a policy-derived pair must not read them as one.
ROLE_PAIR_VERIFIED = "role_pair"
EXCEPTION_AUTHORIZED = "exception"
UNVERIFIED = "unverified"

# The ceiling the bridge already applies to its other free-text arguments. Long enough for any
# provider-qualified model id, short enough that a runaway string cannot travel as a setting.
MAXIMUM = 500

# An exception id is cited as a request argument, and the mutation boundary bounds that argument
# at 128 characters. Declared here so the two limits are one value: a longer id would otherwise
# load happily into a policy that no caller could ever cite.
EXCEPTION_ID_MAXIMUM = 128

LIMITS = (
    "This is what the bridge authorized and transmitted. A host reporting the same values has "
    "recorded the request; it is not evidence that a provider served this model or honoured this "
    "effort. An allowlist covers requests made through this bridge only."
)


class ExecutionRefused(ValueError):
    """Refused before any RPC because of what the request asked to run on."""

    def __init__(self, code: str, field: str, requested, detail: str, *, allowed=None):
        self.code = code
        self.field = field
        self.requested = requested
        self.allowed = allowed
        super().__init__(f"{code}: {detail}")


class ExecutionPolicyError(ValueError):
    """The configured policy file cannot be used. Raised at startup, never per request.

    It stops the server instead of degrading to presence-only, because a file that was configured
    and then silently ignored is the one outcome an operator would never detect.
    """

    code = POLICY_UNREADABLE

    def __init__(self, detail: str):
        super().__init__(f"{POLICY_UNREADABLE}: {detail}")


class Execution(NamedTuple):
    """The authorized pair, and the only description of it a caller is given.

    Everything downstream reads this rather than the original arguments, so a pair cannot be
    approved under one value and dispatched or compared under another.

    `provenance` says WHICH question approved it, because callers downstream need to tell a pair
    that was compared against a declared role pair apart from one that was approved some other
    way. Reconstructing that from the receipt would mean re-deriving a decision this object
    already made, and the two could disagree.
    """

    model: str
    reasoning_effort: str
    receipt: dict
    provenance: str = "unverified"


def _stated(value, field: str) -> str:
    """Presence, in the two shapes it actually arrives in: never asked for, and asked for badly."""
    if value is None:
        raise ExecutionRefused(
            SETTING_MISSING,
            field,
            None,
            f"{field} was not supplied; this bridge never inherits the host's configured default",
        )
    if not isinstance(value, str) or not value.strip() or len(value) > MAXIMUM:
        raise ExecutionRefused(
            SETTING_INVALID,
            field,
            value,
            f"{field} must be a non-empty string of at most {MAXIMUM} characters",
        )
    return value


def _object(value, detail: str) -> dict:
    if not isinstance(value, dict):
        raise ExecutionPolicyError(detail)
    return value


def _only(entry, keys: set, where: str) -> None:
    """An unknown key is a misspelling that would otherwise silently widen or narrow the policy."""
    unknown = sorted(set(entry) - keys)
    if unknown:
        raise ExecutionPolicyError(
            f"{where} has unknown keys {unknown}; supported are {sorted(keys)}"
        )


def _identifier(value, where: str, maximum: int = MAXIMUM) -> str:
    if not isinstance(value, str) or not value.strip() or len(value) > maximum:
        raise ExecutionPolicyError(f"{where} must be a non-empty string")
    return value


def _efforts(value, where: str) -> frozenset:
    if not isinstance(value, list) or not value:
        raise ExecutionPolicyError(f"{where} must be a non-empty list of efforts")
    return frozenset(_identifier(item, where) for item in value)


def _no_duplicates(pairs):
    """A repeated key would silently keep the last value and discard the first.

    Everywhere else an unusable policy stops the server, and this file is the authorization
    boundary: a hand-edited policy that lists "allowed" twice would enforce one of them while
    reading as the other, which is the kind of difference nobody notices until it matters.
    """
    seen: dict = {}
    for key, value in pairs:
        if key in seen:
            raise ExecutionPolicyError(f"duplicate key {key!r} in the execution policy")
        seen[key] = value
    return seen


class ExecutionPolicy:
    """What this host allows. Constructed before the first tool call and never mutated after."""

    def __init__(self, allowed: dict | None = None, exceptions: dict | None = None,
                 declared_roles: dict | None = None, *, digest=None):
        # None is not an empty allowlist. It means no host answer exists to the approval question,
        # so only the presence question is asked, and the receipt says so.
        self._allowed = allowed
        self._exceptions = dict(exceptions or {})
        # Same principle one question further along: no declared role means this host has no
        # answer to the role question, and a caller that names one is refused rather than
        # silently unchecked.
        self._roles = dict(declared_roles or {})
        self._digest = digest

    @property
    def mode(self) -> str:
        return "presence_only" if self._allowed is None else "allowlist"

    @property
    def declares_roles(self) -> bool:
        """Whether this host opted into the role question at all."""
        return bool(self._roles)

    def role_expectation(self, role):
        """The declared expectation for a role, or None. Read-only; it authorizes nothing."""
        return self._roles.get(role) if role is not None else None

    def exception_covers(self, name, *, role, model, reasoning_effort, cwd) -> bool:
        """Whether this exception authorizes exactly this role, pair and directory.

        A PREDICATE rather than an accessor, deliberately. A reader outside this class needs to
        recognise a record that was authorized under an id, and returning the entry to it would
        both disclose the operator's directories and leave the comparison to be written a second
        time -- which is how a second reader ends up approving something authorize() refuses.
        Every field authorize() checks is checked here, against the same values.

        Read-only. Citing an id here authorizes nothing; authorize() remains the only decision.
        """
        entry = self._exceptions.get(name) if name is not None else None
        if entry is None:
            return False
        return (
            entry.get("role") == role
            and entry["model"] == model
            and entry["reasoningEffort"] == reasoning_effort
            and cwd is not None
            and cwd in entry["cwd"]
        )

    def summary(self) -> dict:
        """What a caller may learn about the policy before creating anything.

        The digest identifies the file without disclosing its path or its contents, so a
        coordinator can tell that the allowlist changed without being handed the operator's notes.

        The declared roles and their pairs ARE disclosed, because the whole point of declaring
        them is that a caller states the pair this host expects for the role rather than one it
        remembered. A model id is not a secret; the exception directories and the operator's notes
        still are.
        """
        return {
            "mode": self.mode,
            "digest": self._digest,
            "roles": {role: expectation.receipt() for role, expectation in self._roles.items()},
        }

    @classmethod
    def from_mapping(cls, data, *, digest=None) -> "ExecutionPolicy":
        data = _object(data, "the execution policy must be a JSON object")
        _only(data, {"allowed", "exceptions", "roles"}, "the execution policy")
        # Parsed first so a malformed roles section stops startup naming itself, rather than
        # after an allowlist error that has nothing to do with what the operator just edited.
        declared_roles = roles.parse(data.get("roles"), ExecutionPolicyError)
        entries = data.get("allowed")
        # Optional ONLY because roles are declared. An allowlist constrains every task on this
        # host, so requiring one in order to declare roles would narrow what unrelated work may
        # run here as a side effect of a CRW decision. Absent, the approval question simply stays
        # unanswered and the mode says so. A file declaring neither is a mistake, not an empty
        # policy, so it is still refused.
        if entries is None and declared_roles:
            allowed = None
            entries = []
        elif not isinstance(entries, list) or not entries:
            raise ExecutionPolicyError("allowed must be a non-empty list of {model, efforts}")
        else:
            allowed = {}
        for entry in entries:
            entry = _object(entry, "each allowed entry must be an object")
            _only(entry, {"model", "efforts"}, "an allowed entry")
            if set(entry) != {"model", "efforts"}:
                raise ExecutionPolicyError("each allowed entry needs both model and efforts")
            # Efforts are scoped to their model on purpose. Two independent lists would admit
            # every crossing of them, so a file meaning opus/xhigh and sol/high would also
            # approve opus/high, which nobody wrote down.
            model = _identifier(entry["model"], "an allowed model")
            if model in allowed:
                raise ExecutionPolicyError(f"model {model!r} is listed twice")
            allowed[model] = _efforts(entry["efforts"], f"efforts for {model!r}")
        # The two sections have to agree, and the only place they can be compared is here.
        # A declared role pair is still asked the allowlist question -- only an exception skips
        # that, because an exception IS the operator writing a pair down -- so a file declaring
        # a role pair its own allowlist does not approve describes a role nobody can ever create:
        # the request matches its role, then fails execution_not_allowed. That loaded cleanly and
        # surfaced at the first creation attempt, as a refusal naming the allowlist rather than
        # the contradiction. Refused here instead, and refused rather than resolved: letting the
        # roles section authorize its own pair would make editing it a way to widen the
        # allowlist, and letting the allowlist win would silently unmake a role.
        if allowed is not None:
            for role in roles.ROLES:
                expectation = declared_roles.get(role)
                if expectation is None or expectation.expectation != roles.PAIR:
                    continue
                efforts = allowed.get(expectation.model)
                if efforts is None or expectation.reasoning_effort not in efforts:
                    raise ExecutionPolicyError(
                        f"role {role!r} is declared to run {expectation.model!r} at "
                        f"{expectation.reasoning_effort!r}, which this file's allowed list does "
                        "not approve; no such task could be created, so the two sections "
                        "disagree rather than one narrowing the other"
                    )
        exceptions: dict = {}
        declared = _object(data.get("exceptions", {}), "exceptions must be an object")
        for name, entry in declared.items():
            _identifier(name, "an exception id", EXCEPTION_ID_MAXIMUM)
            entry = _object(entry, f"exception {name!r} must be an object")
            _only(entry, {"model", "reasoningEffort", "cwd", "reason", "role"},
                  f"exception {name!r}")
            absent = sorted({"model", "reasoningEffort", "cwd"} - set(entry))
            if absent:
                raise ExecutionPolicyError(f"exception {name!r} is missing {absent}")
            # Optional, and it narrows rather than widens. A directory is not a task identity, so
            # without it an exception written for one task can be cited by anything else working
            # in the same directory. With it, only a request naming that role may cite it.
            scoped_role = entry.get("role")
            if scoped_role is not None and scoped_role not in roles.ROLES:
                raise ExecutionPolicyError(
                    f"exception {name!r} role {scoped_role!r} is not a role; "
                    f"supported are {sorted(roles.ROLES)}"
                )
            roots = entry["cwd"]
            if not isinstance(roots, list) or not roots:
                raise ExecutionPolicyError(f"exception {name!r} needs at least one cwd")
            for root in roots:
                _identifier(root, f"a cwd of exception {name!r}")
                path = Path(root)
                # Canonical as well as absolute, and settled here rather than per request. The
                # creation path compares against a resolved cwd and the worktree path against a
                # destination Git already requires to be canonical, so an entry that is merely
                # absolute would load cleanly and then match nothing. That reads to an operator
                # as an unexplained refusal instead of the misconfiguration it is.
                try:
                    canonical = str(path.resolve())
                except (OSError, RuntimeError, ValueError) as error:
                    # An embedded null or a symlink loop makes resolution itself fail. Without
                    # this it would leave the module as a bare traceback, and startup would stop
                    # without ever naming the policy as the reason.
                    raise ExecutionPolicyError(
                        f"exception {name!r} cwd {root!r} cannot be resolved: {error}"
                    ) from error
                if not path.is_absolute() or canonical != root:
                    raise ExecutionPolicyError(
                        f"exception {name!r} cwd {root!r} must be canonical and absolute"
                    )
            # One model and one effort, never a list: an exception is a single authorized triple.
            # A list here would be a second allowlist wearing an exception's name, and the caller
            # would choose within it. "reason" is read and discarded; it is the operator's note.
            exceptions[name] = {
                "model": _identifier(entry["model"], f"the model of exception {name!r}"),
                "reasoningEffort": _identifier(
                    entry["reasoningEffort"], f"the effort of exception {name!r}"
                ),
                "cwd": tuple(roots),
                "role": scoped_role,
            }
        return cls(allowed, exceptions, declared_roles, digest=digest)

    @classmethod
    def from_file(cls, path) -> "ExecutionPolicy":
        path = Path(path)
        try:
            raw = path.read_bytes()
        except OSError as error:
            raise ExecutionPolicyError(f"cannot read {path}: {error}") from error
        try:
            data = json.loads(raw, object_pairs_hook=_no_duplicates)
        except ExecutionPolicyError:
            raise
        except ValueError as error:
            raise ExecutionPolicyError(f"{path} is not valid JSON: {error}") from error
        return cls.from_mapping(data, digest=hashlib.sha256(raw).hexdigest())

    @classmethod
    def from_environment(cls, environ) -> "ExecutionPolicy":
        """Read from this process's environment, which no tool argument can influence."""
        configured = (environ.get(ENVIRONMENT_VARIABLE) or "").strip()
        expected = (environ.get(DIGEST_VARIABLE) or "").strip()
        if not configured:
            if expected:
                # A digest names a policy this process was started to enforce. Starting
                # presence-only because the file's name went missing on the way here would be
                # the silent downgrade the digest exists to make visible.
                raise ExecutionPolicyError(
                    f"{DIGEST_VARIABLE} expects a policy with digest {expected}, and "
                    f"{ENVIRONMENT_VARIABLE} names no file"
                )
            return PRESENCE_ONLY
        policy = cls.from_file(configured)
        if expected and policy._digest != expected:
            raise ExecutionPolicyError(
                f"{configured} hashes to {policy._digest}, and this process was started "
                f"expecting {expected}; it changed after it was registered, so it is refused "
                "rather than enforced unregistered"
            )
        return policy

    def authorize(self, model, reasoning_effort, *, cwd=None, exception=None,
                  role=None) -> Execution:
        """Decide before anything is sent. Every refusal happens here, with no RPC issued.

        The order of the three branches is behaviour, not tidiness. Presence is settled first, so
        an omitted value is always reported as omitted rather than as the wrong pair for a role.
        The exception comes next, because it is the user's explicit authorization for this task
        and checking the role first would refuse the very pair they approved. The role check runs
        only when no exception was cited, and the allowlist last, so a pair that is wrong for its
        role says so instead of reporting the broader "not allowed on this host".
        """
        stated_model = _stated(model, "model")
        stated_effort = _stated(reasoning_effort, "reasoning_effort")
        # Read before the branching, so a receipt for an exception still names the declared pair
        # it overrode. Assigning it only in the role branch reported an exception-authorized
        # creation as though its role had no policy at all, which is a different fact and the one
        # a reader would use to judge the override. None stays None where the role really is
        # undeclared.
        expectation = self._roles.get(role) if role is not None else None
        overridden_by = None
        provenance = UNVERIFIED
        if exception is not None:
            entry = self._exceptions.get(exception)
            if entry is None:
                # Also the answer when no policy file is configured at all: a caller can cite an
                # exception the operator wrote, and can never define one.
                raise ExecutionRefused(
                    EXCEPTION_UNKNOWN,
                    "policy_exception",
                    exception,
                    "no exception with this id is declared in this host's execution policy",
                )
            # Scope by role as well as by directory, in both directions. An exception written for
            # one role may only be cited by a request naming that role, and an exception with no
            # role may only be cited by a request naming none -- which is every caller that
            # existed before roles did, so nothing already working changes.
            if entry.get("role") != role:
                raise ExecutionRefused(
                    EXCEPTION_OUT_OF_SCOPE,
                    "policy_exception",
                    role,
                    f"exception {exception!r} is declared for role {entry.get('role')!r} and this "
                    f"request cites {role!r}",
                    allowed=[entry.get("role")],
                )
            if cwd is None:
                # Named apart from the wrong-directory case: the caller supplied no directory at
                # all, and on the resume path cwd is otherwise optional, so a message about which
                # directories are covered would not tell it what to add.
                # The field names cwd rather than the exception, because that is the argument the
                # caller has to add; the mismatched case below names the exception, because there
                # the directory is legitimate and the exception is what fails to cover it.
                raise ExecutionRefused(
                    EXCEPTION_OUT_OF_SCOPE,
                    "cwd",
                    None,
                    f"exception {exception!r} is bound to directories, so a request citing it "
                    f"must also state its cwd; it covers {list(entry['cwd'])}",
                    allowed=list(entry["cwd"]),
                )
            if cwd not in entry["cwd"]:
                raise ExecutionRefused(
                    EXCEPTION_OUT_OF_SCOPE,
                    "policy_exception",
                    cwd,
                    f"exception {exception!r} applies only to {list(entry['cwd'])}",
                    allowed=list(entry["cwd"]),
                )
            for field, requested, authorized in (
                ("model", stated_model, entry["model"]),
                ("reasoning_effort", stated_effort, entry["reasoningEffort"]),
            ):
                if requested != authorized:
                    raise ExecutionRefused(
                        NOT_ALLOWED,
                        field,
                        requested,
                        f"exception {exception!r} authorizes {field} {authorized!r} only",
                        allowed=[authorized],
                    )
            # Recorded rather than silent. A reader of this receipt can see that a role
            # expectation existed and exactly which exception replaced it.
            overridden_by = exception
            provenance = EXCEPTION_AUTHORIZED
        elif role is not None:
            if role not in roles.ROLES or expectation is None:
                raise ExecutionRefused(
                    ROLE_UNKNOWN,
                    "role",
                    role,
                    "no such role is declared in this host's execution policy; this bridge "
                    "declares no pair of its own for any role",
                    allowed=sorted(self._roles),
                )
            if expectation.expectation == roles.PAIR:
                for field, requested, authorized in (
                    ("model", stated_model, expectation.model),
                    ("reasoning_effort", stated_effort, expectation.reasoning_effort),
                ):
                    # Exact string equality, like every other comparison here. There is no alias
                    # table, so an effort named max and one named xhigh are two different values
                    # and neither stands in for the other.
                    if requested != authorized:
                        raise ExecutionRefused(
                            ROLE_MISMATCH,
                            field,
                            requested,
                            f"role {role!r} runs {field} {authorized!r} on this host",
                            allowed=[authorized],
                        )
                provenance = ROLE_PAIR_VERIFIED
        # The allowlist is the last question, and an exception has already answered it: the
        # operator wrote that pair down themselves, which is what an exception is for.
        if exception is None and self._allowed is not None:
            efforts = self._allowed.get(stated_model)
            if efforts is None:
                raise ExecutionRefused(
                    NOT_ALLOWED,
                    "model",
                    stated_model,
                    "this model is not in the execution policy configured on this host",
                    allowed=sorted(self._allowed),
                )
            if stated_effort not in efforts:
                raise ExecutionRefused(
                    NOT_ALLOWED,
                    "reasoning_effort",
                    stated_effort,
                    f"{stated_model!r} is approved only at {sorted(efforts)}",
                    allowed=sorted(efforts),
                )
        return Execution(
            stated_model,
            stated_effort,
            {
                "mode": self.mode,
                "digest": self._digest,
                "exception": exception,
                # Recorded beside "exception" and read the same way: None means the question was
                # not asked. roleExpectation is present only when a role WAS cited, because an
                # expectation for a role nobody named would be a field describing nothing.
                "role": role,
                **(
                    {
                        "roleExpectation": (
                            {**expectation.receipt(), "overriddenBy": overridden_by}
                            if expectation is not None
                            else {"role": role, "expectation": None, "model": None,
                                  "reasoningEffort": None, "overriddenBy": overridden_by}
                        )
                    }
                    if role is not None
                    else {}
                ),
                "model": stated_model,
                "reasoningEffort": stated_effort,
                "limits": LIMITS,
            },
            provenance,
        )


# The default every Bridge gets unless a server hands it a configured one. Presence is still
# enforced here; only the approval question is unanswerable without a file.
PRESENCE_ONLY = ExecutionPolicy()
