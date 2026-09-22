"""The execution settings a send must preserve, and how they are carried and checked.

This module exists because of one observed host behaviour: a thread/resume carrying only
{threadId, excludeTurns} returned sandbox dangerFullAccess for a task created with workspaceWrite
and networkAccess false. The pinned bridge sends exactly that resume and says so in its own
comment, so an adapter built on it cannot claim to preserve permissions.

thread/resume is the DETECTOR, and it is the only call this adapter relies on. Its params accept a
sandbox MODE string, approvalPolicy, cwd, runtimeWorkspaceRoots, model and a free-form config; its
RESPONSE reports the full derived policy, the effort and the environments. A host that does not
have the thread in the requested state is visible there, before anything starts.

turn/start is deliberately NOT used to bind settings, even though it is the only call that accepts
the full SandboxPolicy object plus effort plus environments. TurnStartResponse defines only turn,
so a setting bound there can never be read back, and a receipt that reported accepted off a turn
ID alone would be calling an unverifiable binding a success. A clean resume already establishes
that the thread IS in the requested state, which makes the overrides redundant; and on this host
the resume reports a thread's real state rather than adopting an override, so the binding was
never doing the work it appeared to do. Dropping it also removes an unintended mutation, because
TurnStartParams says a model override persists into subsequent turns: sending one message was
quietly rewriting the thread for every later turn.
"""

from .errors import DeliveryRefused, RefusalReason

# ThreadResumeParams.sandbox is a SandboxMode enum; TurnStartParams.sandboxPolicy is the object.
RESUME_SANDBOX_MODE = {
    "workspaceWrite": "workspace-write",
    "readOnly": "read-only",
    "dangerFullAccess": "danger-full-access",
}

# Policy fields the mode string cannot carry, and the config keys that can. Identical to the
# bridge's POLICY_CONFIG_KEYS; readOnly has no entry because no config spelling moves its network
# access, so a resume cannot restore it and must not pretend to.
POLICY_CONFIG_KEYS = {
    "workspaceWrite": {
        "writableRoots": ("sandbox_workspace_write", "writable_roots"),
        "networkAccess": ("sandbox_workspace_write", "network_access"),
        "excludeTmpdirEnvVar": ("sandbox_workspace_write", "exclude_tmpdir_env_var"),
        "excludeSlashTmp": ("sandbox_workspace_write", "exclude_slash_tmp"),
    },
}

# Defaults the pinned SandboxPolicy declares, applied to both sides before comparing so an
# omitted default and an explicit default are not read as a difference.
POLICY_DEFAULTS = {
    "workspaceWrite": {"writableRoots": [], "networkAccess": False,
                       "excludeTmpdirEnvVar": False, "excludeSlashTmp": False},
    "readOnly": {"networkAccess": False},
    "externalSandbox": {"networkAccess": "restricted"},
    "dangerFullAccess": {},
}

REQUIRED = (
    "sandbox", "approvalPolicy", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort",
    "environments",
)

# The order a mixed answer is reported in, identical to the bridge's FIELD_PRECEDENCE. The FIRST
# finding becomes the receipt's error code, so without a shared order the two implementations
# describe one identical host answer with two different codes: for a response that omits the model
# AND widens the sandbox, one would say settings_not_preserved and the other setting_unobservable.
# approvalPolicy and environments are not here; both are decided before this list is reached, and
# environments is relay-only because the bridge has no environments to compare.
FIELD_PRECEDENCE = ("sandbox", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort")

SETTINGS_UNAVAILABLE = "settings_unavailable"
SETTINGS_INCOMPLETE = "settings_incomplete"
SETTINGS_NOT_PRESERVED = "settings_not_preserved"
# The host reported no value at all, so nothing here says the setting was applied or ignored.
# Kept apart from SETTINGS_NOT_PRESERVED, which means the host reported something different.
SETTING_UNOBSERVABLE = "setting_unobservable"
ENVIRONMENTS_UNKNOWN = "environments_unknown"
UNVERIFIABLE_PERMISSION_PROFILE = "unverifiable_permission_profile"
UNSUPPORTED_SANDBOX_TYPE = "unsupported_sandbox_type"
UNSUPPORTED_APPROVAL_POLICY = "unsupported_approval_policy"

# The only approval policy a resume response may report. It is a VALUE constraint on the
# recorded row rather than something a transformation can fail on: a host that preserves what
# it was asked for returns what was recorded, so a row recording anything else is refused
# after the resume and never reaches turn/start. Named here so the verification below and
# `doctor`'s receipt read one definition instead of two spellings of it.
AUTHORIZED_APPROVAL_POLICY = "never"


def normalise_policy(policy):
    """Fill the declared defaults so an omitted default compares equal to an explicit one.

    Total by design: it returns None for anything it cannot read, and never raises. Its callers
    run BEFORE turn/start, so an exception here would leave _mutate recording outcome_unknown --
    telling a caller the message may have been delivered -- for a response that in fact withheld
    it. An unreadable policy has to come back as a value, so the comparison can refuse it.
    """
    if not isinstance(policy, dict):
        return None
    kind = policy.get("type")
    # Not just a missing type: an unhashable one would raise on the defaults lookup below.
    if not isinstance(kind, str):
        return None
    merged = dict(POLICY_DEFAULTS.get(kind, {}))
    merged.update({key: value for key, value in policy.items() if key != "type"})
    merged["type"] = kind
    if "writableRoots" in merged:
        roots = merged["writableRoots"]
        if not isinstance(roots, list):
            return None
        merged["writableRoots"] = list(roots)
    return merged


def normalise_environments(environments):
    """None stays None. It means unknown, and unknown is never flattened into empty."""
    if environments is None:
        return None
    out = []
    for entry in environments:
        roots = entry.get("runtimeWorkspaceRoots")
        out.append({
            "environmentId": entry["environmentId"],
            "cwd": entry["cwd"],
            # TurnEnvironmentParams says an omitted roots list defaults to cwd.
            "runtimeWorkspaceRoots": list(roots) if roots is not None else [entry["cwd"]],
        })
    return out


class TaskSettings:
    """Complete or unusable, and presence alone was never the whole of it.

    A record missing a required field cannot say what it is preserving. Neither can one whose
    cwd, model or reasoningEffort is present and is not a string, because resume_params would
    copy it onto the wire and only the host could then say what it had done with it.
    require_usable() decides both. It does NOT type runtimeWorkspaceRoots or environments;
    those fail later, inside the transformations that consume them.

    JUN-92 populates this from the creation result Run already receives; it is not a separate
    handshake and it asks for nothing the host did not already report at creation.
    """

    def __init__(self, data: dict):
        self.data = dict(data or {})
        # Set by the delivery gate when this recipient's pair was not derived from its role's
        # declared pair. The transport reads it after its OWN thread/read, because the status
        # the gate saw is older than the resume by a turn listing and a claim, and a recipient
        # that unloads in between would otherwise be resumed under exactly the pair the gate
        # meant never to transmit.
        self.refuse_when_unloaded = False

    # ------------------------------------------------------------- validity

    def missing(self) -> list:
        absent = [field for field in REQUIRED if self.data.get(field) is None]
        # environments may legitimately be an empty list (no environments selected), which is a
        # decision, unlike None which is an absence. Re-admit that case.
        if "environments" in absent and isinstance(self.data.get("environments"), list):
            absent.remove("environments")
        return absent

    def mistyped(self) -> list:
        """Recorded fields the resume contract types as strings, and this record does not.

        Presence was never the whole question. resume_params copies each of these values
        straight into ThreadResumeParams, so a recorded `cwd: 7` used to be built and sent, and
        the only answer about it came back from a host whose schema is not in this repository.
        The receipt could not say whether the host had accepted it, which is why the rule has to
        run before the send rather than be read off the response.

        Only meaningful once missing() is empty, and it subscripts self.data to say so:
        require_usable() orders the two, exactly as resume_params and mismatches already assume
        a complete record. isinstance excludes bool as well, so True is not read as a model name.

        approvalPolicy is not here. Contract v1 admits the single literal `never`, so the value
        comparison that decides it already refuses every non-string it could hold, and a type
        rule would answer that same row with the less specific of two codes.
        """
        wrong = []
        if not isinstance(self.data["cwd"], str):
            wrong.append("cwd")
        if not isinstance(self.data["model"], str):
            wrong.append("model")
        if not isinstance(self.data["reasoningEffort"], str):
            wrong.append("reasoningEffort")
        return wrong

    def require_usable(self) -> None:
        absent = self.missing()
        if absent:
            raise DeliveryRefused(
                RefusalReason.SETTINGS_INCOMPLETE,
                f"missing {', '.join(absent)}",
            )
        wrong = self.mistyped()
        if wrong:
            # Shape before meaning: what the record IS, then what a particular value means. The
            # managed admission path already decides in that order, typing model and
            # reasoningEffort before it looks at the sandbox type. Every offender is named at
            # once, like missing(), so a hand-edited row costs one round rather than three.
            raise DeliveryRefused(
                RefusalReason.SETTINGS_MISTYPED,
                "; ".join(
                    f"{field} is {type(self.data[field]).__name__}, not str" for field in wrong
                ),
            )
        if self.sandbox_mode() is None:
            raise DeliveryRefused(
                RefusalReason.UNSUPPORTED_SANDBOX_TYPE,
                f"{self.data['sandbox'].get('type')!r} has no"
                " ThreadResumeParams.sandbox mode, so it cannot be restored on a resume",
            )
        if normalise_policy(self.data["sandbox"]) is None:
            # A supported type is not enough. A stored policy we cannot read in full would
            # normalise to None, and so would an equally malformed response, and None == None
            # would then read as agreement and send under a sandbox nothing ever verified.
            raise DeliveryRefused(
                RefusalReason.UNSUPPORTED_SANDBOX_TYPE,
                "the recorded sandbox policy cannot be read in full, so no response could"
                " confirm it",
            )

    def sandbox_mode(self):
        policy = self.data.get("sandbox") or {}
        return RESUME_SANDBOX_MODE.get(policy.get("type"))

    # --------------------------------------------------------------- params

    def resume_params(self, thread_id: str) -> dict:
        """Only fields ThreadResumeParams actually defines. No effort field, no environments.

        Effort and the sandbox policy detail have no dedicated resume parameters, but the schema
        accepts a free-form config object, so both travel there under the same keys the bridge
        uses. This matters beyond the effort: ThreadResumeParams.sandbox is only a MODE, so a
        resume that sent the mode alone could not restore writable roots or the network flag, and
        would then compare against values it never asked for.
        """
        params = {
            "threadId": thread_id,
            "excludeTurns": True,
            "sandbox": self.sandbox_mode(),
            "approvalPolicy": self.data["approvalPolicy"],
            "cwd": self.data["cwd"],
            "runtimeWorkspaceRoots": list(self.data["runtimeWorkspaceRoots"]),
            "model": self.data["model"],
            "config": {"model_reasoning_effort": self.data["reasoningEffort"]},
        }
        policy = normalise_policy(self.data["sandbox"]) or {}
        for field, (section, key) in POLICY_CONFIG_KEYS.get(policy.get("type"), {}).items():
            if field in policy:
                params["config"].setdefault(section, {})[key] = policy[field]
        return params

    # ------------------------------------------------------------ verifying

    def mismatches(self, response: dict) -> list:
        """Ordered findings against a resume response. Order is behaviour, not presentation.

        The approval policy is checked FIRST. With an authorized policy of never, a returned
        on-request is both a mismatch and the push-channel-closed case, and raising the generic
        mismatch first would turn a permanently closed channel into a retry loop.

        Within each field, ABSENCE is decided before difference. A host that reported nothing has
        told us nothing about whether the setting was applied, which is a different fact from a
        host that reported something else, and the two need different answers from a caller.
        """
        found = []
        returned_policy = response.get("approvalPolicy")
        if returned_policy is None:
            # Not the closed-channel case: a policy we cannot see is not a policy we know is
            # interactive. It withholds, and stays eligible for a bounded pre-send retry.
            return [{"code": SETTING_UNOBSERVABLE, "field": "approvalPolicy",
                     "expected": AUTHORIZED_APPROVAL_POLICY, "returned": None}]
        if returned_policy != AUTHORIZED_APPROVAL_POLICY:
            # A granular policy is an object; it is never copied into the frozen record, which
            # types this field as string or null. Contract v1 admits only a plain string.
            label = returned_policy if isinstance(returned_policy, str) else "granular"
            found.append({"code": UNSUPPORTED_APPROVAL_POLICY, "field": "approvalPolicy",
                          "expected": AUTHORIZED_APPROVAL_POLICY, "returned": label,
                          "returnedShape": type(returned_policy).__name__})
            return found

        thread = response.get("thread") or {}
        returned_environments = thread.get("environments")
        if returned_environments is None:
            # Thread.environments documents null as not loaded OR the server does not expose its
            # selection. Unknown is not absence, and it is never read as an empty selection.
            found.append({"code": ENVIRONMENTS_UNKNOWN, "field": "environments",
                          "expected": self.data["environments"], "returned": None})
            return found
        expected_environments = normalise_environments(self.data["environments"])
        if normalise_environments(returned_environments) != expected_environments:
            found.append({"code": SETTINGS_NOT_PRESERVED, "field": "environments",
                          "expected": expected_environments,
                          "returned": normalise_environments(returned_environments)})

        expectations = {
            "sandbox": normalise_policy(self.data["sandbox"]),
            "cwd": self.data["cwd"],
            "runtimeWorkspaceRoots": list(self.data["runtimeWorkspaceRoots"]),
            "model": self.data["model"],
            "reasoningEffort": self.data["reasoningEffort"],
        }
        for field in FIELD_PRECEDENCE:
            expected = expectations[field]
            raw = response.get(field)
            if raw is None:
                # Previously runtimeWorkspaceRoots read `or []`, so an omitted list compared
                # EQUAL to an expected empty list and a send proceeded on an answer the host
                # never gave. Absence is not agreement.
                found.append({"code": SETTING_UNOBSERVABLE, "field": field,
                              "expected": expected, "returned": None})
                continue
            returned = normalise_policy(raw) if field == "sandbox" else raw
            if field == "sandbox" and (returned is None or expected is None):
                # Never let two unreadable policies agree by both becoming None.
                found.append({"code": SETTINGS_NOT_PRESERVED, "field": field,
                              "expected": expected if expected is not None else self.data["sandbox"],
                              "returned": raw})
                continue
            if field == "runtimeWorkspaceRoots":
                returned = list(returned)
            if expected != returned:
                found.append({"code": SETTINGS_NOT_PRESERVED, "field": field,
                              "expected": expected, "returned": returned})

        profile = response.get("activePermissionProfile")
        if profile is not None and profile != self.data.get("expectedPermissionProfile"):
            # A profile we did not anticipate is a permission source we cannot interpret.
            found.append({"code": UNVERIFIABLE_PERMISSION_PROFILE,
                          "field": "activePermissionProfile",
                          "expected": self.data.get("expectedPermissionProfile"),
                          "returned": profile})
        return found


def refusal_code(findings) -> str:
    """The code the transport classifies on: the first finding decides."""
    return findings[0]["code"] if findings else ""
