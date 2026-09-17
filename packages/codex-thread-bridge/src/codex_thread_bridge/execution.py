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

ENVIRONMENT_VARIABLE = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"

SETTING_MISSING = "execution_setting_missing"
SETTING_INVALID = "execution_setting_invalid"
NOT_ALLOWED = "execution_not_allowed"
EXCEPTION_UNKNOWN = "execution_exception_unknown"
EXCEPTION_OUT_OF_SCOPE = "execution_exception_out_of_scope"
POLICY_UNREADABLE = "execution_policy_unreadable"

# The ceiling the bridge already applies to its other free-text arguments. Long enough for any
# provider-qualified model id, short enough that a runaway string cannot travel as a setting.
MAXIMUM = 500

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
    """

    model: str
    reasoning_effort: str
    receipt: dict


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


def _identifier(value, where: str) -> str:
    if not isinstance(value, str) or not value.strip() or len(value) > MAXIMUM:
        raise ExecutionPolicyError(f"{where} must be a non-empty string")
    return value


def _efforts(value, where: str) -> frozenset:
    if not isinstance(value, list) or not value:
        raise ExecutionPolicyError(f"{where} must be a non-empty list of efforts")
    return frozenset(_identifier(item, where) for item in value)


class ExecutionPolicy:
    """What this host allows. Constructed before the first tool call and never mutated after."""

    def __init__(self, allowed: dict | None = None, exceptions: dict | None = None, *, digest=None):
        # None is not an empty allowlist. It means no host answer exists to the approval question,
        # so only the presence question is asked, and the receipt says so.
        self._allowed = allowed
        self._exceptions = dict(exceptions or {})
        self._digest = digest

    @property
    def mode(self) -> str:
        return "presence_only" if self._allowed is None else "allowlist"

    def summary(self) -> dict:
        """What a caller may learn about the policy before creating anything.

        The digest identifies the file without disclosing its path or its contents, so a
        coordinator can tell that the allowlist changed without being handed the operator's notes.
        """
        return {"mode": self.mode, "digest": self._digest}

    @classmethod
    def from_mapping(cls, data, *, digest=None) -> "ExecutionPolicy":
        data = _object(data, "the execution policy must be a JSON object")
        _only(data, {"allowed", "exceptions"}, "the execution policy")
        entries = data.get("allowed")
        if not isinstance(entries, list) or not entries:
            raise ExecutionPolicyError("allowed must be a non-empty list of {model, efforts}")
        allowed: dict = {}
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
        exceptions: dict = {}
        declared = _object(data.get("exceptions", {}), "exceptions must be an object")
        for name, entry in declared.items():
            _identifier(name, "an exception id")
            entry = _object(entry, f"exception {name!r} must be an object")
            _only(entry, {"model", "reasoningEffort", "cwd", "reason"}, f"exception {name!r}")
            absent = sorted({"model", "reasoningEffort", "cwd"} - set(entry))
            if absent:
                raise ExecutionPolicyError(f"exception {name!r} is missing {absent}")
            roots = entry["cwd"]
            if not isinstance(roots, list) or not roots:
                raise ExecutionPolicyError(f"exception {name!r} needs at least one cwd")
            for root in roots:
                _identifier(root, f"a cwd of exception {name!r}")
                if not Path(root).is_absolute():
                    raise ExecutionPolicyError(f"exception {name!r} cwd {root!r} must be absolute")
            # One model and one effort, never a list: an exception is a single authorized triple.
            # A list here would be a second allowlist wearing an exception's name, and the caller
            # would choose within it. "reason" is read and discarded; it is the operator's note.
            exceptions[name] = {
                "model": _identifier(entry["model"], f"the model of exception {name!r}"),
                "reasoningEffort": _identifier(
                    entry["reasoningEffort"], f"the effort of exception {name!r}"
                ),
                "cwd": tuple(roots),
            }
        return cls(allowed, exceptions, digest=digest)

    @classmethod
    def from_file(cls, path) -> "ExecutionPolicy":
        path = Path(path)
        try:
            raw = path.read_bytes()
        except OSError as error:
            raise ExecutionPolicyError(f"cannot read {path}: {error}") from error
        try:
            data = json.loads(raw)
        except ValueError as error:
            raise ExecutionPolicyError(f"{path} is not valid JSON: {error}") from error
        return cls.from_mapping(data, digest=hashlib.sha256(raw).hexdigest())

    @classmethod
    def from_environment(cls, environ) -> "ExecutionPolicy":
        """Read from this process's environment, which no tool argument can influence."""
        configured = (environ.get(ENVIRONMENT_VARIABLE) or "").strip()
        return cls.from_file(configured) if configured else PRESENCE_ONLY

    def authorize(self, model, reasoning_effort, *, cwd=None, exception=None) -> Execution:
        """Decide before anything is sent. Every refusal happens here, with no RPC issued."""
        stated_model = _stated(model, "model")
        stated_effort = _stated(reasoning_effort, "reasoning_effort")
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
            if cwd is None or cwd not in entry["cwd"]:
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
        elif self._allowed is not None:
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
                "model": stated_model,
                "reasoningEffort": stated_effort,
                "limits": LIMITS,
            },
        )


# The default every Bridge gets unless a server hands it a configured one. Presence is still
# enforced here; only the approval question is unanswerable without a file.
PRESENCE_ONLY = ExecutionPolicy()
