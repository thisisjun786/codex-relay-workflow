"""Which execution settings are transmitted, and which the host will show us again.

Measured against a probe App Server on codex-cli 0.154.0, with the protocol bundle from
`codex app-server generate-json-schema --experimental` (schema v2) as the field reference.
Three calls carry settings and they do not carry the same ones:

    thread/start   cwd, sandbox as a MODE string, approvalPolicy, model, runtimeWorkspaceRoots,
                   environments, projectId, and a free-form config object. There is no effort
                   parameter, so reasoning effort travels as config.model_reasoning_effort or it
                   does not travel at all.
    thread/resume  the same set minus environments.
    turn/start     the full SandboxPolicy object, effort and environments, scoped to this turn
                   and the ones after it.

Observation is asymmetric, and that asymmetry decides the whole design. ThreadStartResponse and
ThreadResumeResponse report approvalPolicy, cwd, model, sandbox as the resolved policy object,
reasoningEffort, runtimeWorkspaceRoots and activePermissionProfile. TurnStartResponse defines only
`turn`. So the bridge never binds a setting through turn/start: a binding it cannot read back
could be reported as applied when it was ignored. It transmits at start or resume, reads the
answer, and withholds when the answer disagrees.

Resume is a detector, not a setter -- measured on a thread the host already had loaded. A resume
that explicitly carried sandbox "read-only" for a workspaceWrite thread came back reporting
workspaceWrite: the host accepts the parameter and reports the thread's real state. That is
precisely what verifying preservation needs, and it is the reason a clean resume is followed by a
plain turn/start with no overrides. What that measurement does NOT cover is a thread the host has
to materialize first. Three project parents asked to resume on a new pair answered differently
depending on whether the host still had them loaded, and residency was not recorded at the time,
so whether a not-loaded thread ADOPTS a transmitted setting is not established here. Callers
therefore record the runtime status they saw before the resume, and an agreeing echo from a
thread that was not loaded is recorded as agreement rather than as preservation.

An echo proves recording, not honouring. A start carrying
config.model_reasoning_effort "totally-not-a-real-effort" echoed that string back as
reasoningEffort. So a matching value means the host recorded what was asked for; whether a
provider acts on that effort level is not observable here and is never claimed.

The failures are kept apart because a caller has to answer each one differently:

    setting_untransmittable   this protocol cannot carry the request at all. Decided locally,
                              before any RPC, so a local limitation is never reported as though
                              the host disagreed.
    settings_not_preserved    the host reported a different value than the one requested.
    setting_unobservable      the host reported no value, so nothing here says the setting was
                              applied or ignored. It withholds; it is never a warning attached
                              to a success.
    unsupported_approval_policy / unsupported_sandbox_type
                              the requested shape has no representation this bridge can use.

The approval policy is the one setting a caller DECLARES rather than requests. It is never
transmitted on resume, because a value this bridge does not own has no business being sent to a
thread it did not create: the declaration is a claim about the thread, and `findings` judges the
host's answer against it. Measured on codex-cli 0.154.0 in both directions, omitting the
parameter preserves the thread's own policy and does NOT inherit the CODEX_HOME config default --
a thread on on-request under a config default of never resumed as on-request, and the mirror
resumed as never. That is what makes preservation a mechanism rather than a hope, and it is why
the guard's predicate is now "the observed policy is not the declared one" instead of "the
observed policy is not never". It refuses exactly as before for a caller that declares nothing.

A raised RpcError or TransportError is the third cause named in the issue, delivery failure, and
it deliberately stays on the bridge's existing failed / outcome_unknown / not_attempted path
rather than becoming a finding: whether the request arrived at all is a different question from
what the host did with one that did.
"""

from pathlib import Path

# ThreadStartParams.sandbox and ThreadResumeParams.sandbox are a SandboxMode string;
# the responses report the resolved SandboxPolicy object.
SANDBOX_MODES = {
    "readOnly": "read-only",
    "workspaceWrite": "workspace-write",
    "dangerFullAccess": "danger-full-access",
}
SANDBOX_TYPES = {mode: kind for kind, mode in SANDBOX_MODES.items()}

# The defaults SandboxPolicy declares. Filled on both sides before comparing, because the host
# returns the fully populated object and an omitted default must not read as a difference.
POLICY_DEFAULTS = {
    "readOnly": {"networkAccess": False},
    "externalSandbox": {"networkAccess": "restricted"},
    "dangerFullAccess": {},
    "workspaceWrite": {
        "writableRoots": [],
        "networkAccess": False,
        "excludeTmpdirEnvVar": False,
        "excludeSlashTmp": False,
    },
}

# Policy fields the free-form config object can actually carry, measured one at a time.
# A start carrying all four workspaceWrite keys returned exactly those values. readOnly has no
# entry on purpose: neither sandbox_read_only.network_access nor a top-level network_access moved
# readOnly networkAccess off false, so asking for it is refused instead of silently dropped.
POLICY_CONFIG_KEYS = {
    "workspaceWrite": {
        "writableRoots": ("sandbox_workspace_write", "writable_roots"),
        "networkAccess": ("sandbox_workspace_write", "network_access"),
        "excludeTmpdirEnvVar": ("sandbox_workspace_write", "exclude_tmpdir_env_var"),
        "excludeSlashTmp": ("sandbox_workspace_write", "exclude_slash_tmp"),
    },
}

# Every settings field the start and resume responses report.
OBSERVABLE = (
    "approvalPolicy",
    "cwd",
    "model",
    "reasoningEffort",
    "runtimeWorkspaceRoots",
    "sandbox",
)

# The order a mixed answer is reported in. The FIRST finding becomes the receipt's error code, so
# leaving it to dict insertion order would make the reported cause incidental, and would let this
# module and the relay's mirror describe one identical host answer with two different codes.
# Broadest blast radius first: a wrong sandbox matters more than a wrong effort.
# approvalPolicy is not here because it is decided before this list is ever reached.
FIELD_PRECEDENCE = ("sandbox", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort")

SETTING_UNTRANSMITTABLE = "setting_untransmittable"
SETTINGS_NOT_PRESERVED = "settings_not_preserved"
SETTING_UNOBSERVABLE = "setting_unobservable"
UNSUPPORTED_APPROVAL_POLICY = "unsupported_approval_policy"
UNSUPPORTED_SANDBOX_TYPE = "unsupported_sandbox_type"

# The approval policies AskForApproval gives a plain string name, from
# `codex app-server generate-json-schema --experimental` on codex-cli 0.154.0. Its fourth shape is
# a granular object, which has no name a caller could type, so it can be OBSERVED but never
# DECLARED -- and an observed granular policy therefore never equals a declaration and stays
# refused, which is the honest outcome for a policy this bridge cannot describe.
APPROVAL_POLICIES = ("never", "on-request", "untrusted")

# The only policy under which nothing can ever ask this bridge for a decision. Every other policy
# means the thread may raise an approval request mid-turn, which this bridge leaves unanswered for
# the thread's own approver.
UNATTENDED_APPROVAL_POLICY = "never"

APPROVAL_LIMITS = (
    "This bridge answers no approval request: it never grants one, never denies one, and a "
    "report message never stands in for one. Measured on codex-cli 0.154.0, the host sends each "
    "approval request of a turn to every client subscribed to the thread, replays a pending one "
    "to a client that resumes the thread later, and applies the first answer from any of them, "
    "an error answer as a denial. An answer from this bridge would therefore decide for the "
    "thread's approver, so it records each request and leaves it unanswered; the thread's own "
    "client decides it, now or when it next opens the thread, and the turn waits until then. "
    "That measurement is of that release, not a reading of the connected server, and how a "
    "particular client such as Desktop presents a request it did not ask for is not "
    "established here. Delivering a report and servicing the code execution a report may "
    "provoke are separate capabilities, and only the first one is claimed."
)

# thread/read reports model, reasoningEffort, cwd, environments and projectId, and nothing about
# sandbox or approvalPolicy. That bounds what the post-acceptance annotation can ever say.
ANNOTATED = ("model", "reasoningEffort", "cwd")

OBSERVATION_LIMITS = (
    "These are the settings the host reported at this observation, not a guarantee about the "
    "dispatched turn. The bridge holds no host-side exclusivity, so another client can change a "
    "thread's settings between the observation and turn/start. A matching value means the host "
    "recorded the request; it is not evidence that a provider honours it."
)


class UntransmittableSetting(ValueError):
    """Asked for something this protocol cannot carry. Raised before any RPC is issued."""

    def __init__(self, field: str, requested, detail: str):
        self.code = SETTING_UNTRANSMITTABLE
        self.field = field
        self.requested = requested
        super().__init__(f"{SETTING_UNTRANSMITTABLE}: {detail}")


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


class SettingsContract:
    """The settings a caller asked for, how each is carried, and how the answer is judged.

    Only fields the caller actually supplied are transmitted or compared. An omitted field is
    absent from `requested` and can never produce a finding, which is what lets an existing
    caller keep exactly its current behaviour.
    """

    def __init__(
        self,
        *,
        cwd=None,
        sandbox=None,
        expected_sandbox_policy=None,
        model=None,
        reasoning_effort=None,
        runtime_workspace_roots=None,
        approval_policy=UNATTENDED_APPROVAL_POLICY,
    ):
        # Declared, never transmitted. Checked here, before any RPC, so a policy this bridge
        # cannot name is refused locally rather than carried to the host and reported back as
        # though the host had disagreed. The default keeps every existing caller on exactly the
        # behaviour it had: declare nothing and only a never thread is accepted.
        if approval_policy not in APPROVAL_POLICIES:
            raise ValueError(
                f"approval_policy must be one of {list(APPROVAL_POLICIES)}; a granular policy has "
                "no name a caller can declare"
            )
        self.approval_policy = approval_policy
        self.cwd = cwd
        self.model = model
        self.reasoning_effort = reasoning_effort
        self.runtime_workspace_roots = (
            None if runtime_workspace_roots is None else list(runtime_workspace_roots)
        )

        if sandbox is not None and sandbox not in SANDBOX_TYPES:
            raise ValueError(f"Unsupported sandbox {sandbox!r}")
        self.sandbox_mode = sandbox
        self.expected_policy = normalise_policy(expected_sandbox_policy)
        if expected_sandbox_policy is not None and self.expected_policy is None:
            raise ValueError("expected_sandbox_policy must be an object carrying a type")
        if self.expected_policy is not None:
            kind = self.expected_policy["type"]
            if kind not in SANDBOX_MODES:
                # externalSandbox has no ThreadResumeParams mode, so it cannot be restored.
                raise UntransmittableSetting(
                    "sandbox",
                    self.expected_policy,
                    f"{kind!r} has no sandbox mode this bridge can send",
                )
            if sandbox is not None and SANDBOX_TYPES[sandbox] != kind:
                raise ValueError("sandbox and expected_sandbox_policy.type must agree")
            self.sandbox_mode = SANDBOX_MODES[kind]
            self._refuse_untransmittable_policy_fields(kind)

        for name, value in (("cwd", cwd), ("model", model), ("reasoning_effort", reasoning_effort)):
            if value is not None and (not isinstance(value, str) or not value.strip()):
                raise ValueError(f"{name} must be a non-empty string when supplied")
        if self.runtime_workspace_roots is not None and any(
            not isinstance(root, str) or not Path(root).is_absolute()
            for root in self.runtime_workspace_roots
        ):
            raise ValueError("runtime_workspace_roots must be absolute paths")

    def _refuse_untransmittable_policy_fields(self, kind):
        """A field with no config key is refused only when it actually asks for something.

        Equal to its declared default it asks for nothing, so it passes. Different from the
        default it is a real request this protocol cannot carry, and pretending otherwise would
        report a local gap as though the host had disagreed.
        """
        mapped = POLICY_CONFIG_KEYS.get(kind, {})
        defaults = POLICY_DEFAULTS.get(kind, {})
        for field, value in self.expected_policy.items():
            if field == "type" or field in mapped:
                continue
            if value != defaults.get(field):
                raise UntransmittableSetting(
                    f"sandbox.{field}",
                    value,
                    f"{kind} {field} cannot be carried by thread/start or thread/resume; "
                    f"only {sorted(mapped) or 'no field'} is transmittable for this type",
                )

    # ------------------------------------------------------------------ requested

    @property
    def requested(self):
        """What the caller asked for, under the protocol's own field names."""
        asked = {}
        if self.cwd is not None:
            asked["cwd"] = self.cwd
        if self.model is not None:
            asked["model"] = self.model
        if self.reasoning_effort is not None:
            asked["reasoningEffort"] = self.reasoning_effort
        if self.runtime_workspace_roots is not None:
            asked["runtimeWorkspaceRoots"] = list(self.runtime_workspace_roots)
        if self.expected_policy is not None:
            asked["sandbox"] = dict(self.expected_policy)
        elif self.sandbox_mode is not None:
            asked["sandbox"] = {"type": SANDBOX_TYPES[self.sandbox_mode]}
        return asked

    def __bool__(self):
        return bool(self.requested)

    # -------------------------------------------------------------------- params

    def config(self):
        """The free-form config object carrying what the typed parameters cannot.

        Every mapped policy field is serialised, including one whose value equals the schema
        default. The host's own configuration may already set the opposite, so a request for
        networkAccess false has to be sent to override it; comparing afterwards would only
        discover the conflict, never resolve it.
        """
        config = {}
        if self.reasoning_effort is not None:
            config["model_reasoning_effort"] = self.reasoning_effort
        if self.expected_policy is not None:
            for field, (section, key) in POLICY_CONFIG_KEYS.get(
                self.expected_policy["type"], {}
            ).items():
                if field in self.expected_policy:
                    config.setdefault(section, {})[key] = self.expected_policy[field]
        return config

    def start_params(self):
        """The ThreadStartParams subset the caller actually asked for."""
        params = {}
        if self.sandbox_mode is not None:
            params["sandbox"] = self.sandbox_mode
        if self.model is not None:
            params["model"] = self.model
        if self.runtime_workspace_roots is not None:
            params["runtimeWorkspaceRoots"] = list(self.runtime_workspace_roots)
        config = self.config()
        if config:
            params["config"] = config
        return params

    def resume_params(self, thread_id: str):
        """ThreadResumeParams. With nothing requested this is byte-identical to the old resume.

        approvalPolicy is deliberately absent, whatever the caller declared. ThreadResumeParams
        makes it optional, and sending it would be asking to SET the policy of a thread this
        bridge does not own -- the one move this path must never make. Measured on codex-cli
        0.154.0: omitting it returns the thread's own policy and does not pick up the CODEX_HOME
        config default, in both directions. Preservation is therefore a property of the request
        that goes out, not an assumption about how the host treats one that does.
        """
        params = {"threadId": thread_id, "excludeTurns": True}
        if not self.requested:
            return params
        if self.sandbox_mode is not None:
            params["sandbox"] = self.sandbox_mode
        if self.cwd is not None:
            params["cwd"] = self.cwd
        if self.model is not None:
            params["model"] = self.model
        if self.runtime_workspace_roots is not None:
            params["runtimeWorkspaceRoots"] = list(self.runtime_workspace_roots)
        config = self.config()
        if config:
            params["config"] = config
        return params

    # ----------------------------------------------------------------- observing

    @staticmethod
    def observed(response):
        """Every observable field the response actually reports.

        An explicit null is treated as not reported, because null carries no more information
        about what was applied than an absent key does.
        """
        seen = {}
        for field in OBSERVABLE:
            value = response.get(field)
            if value is None:
                continue
            if field == "sandbox":
                # Keep the raw value when it cannot be normalised. Storing None instead would
                # make an unreadable answer look like no answer in the receipt's actual.
                normalised = normalise_policy(value)
                seen[field] = value if normalised is None else normalised
            else:
                seen[field] = value
        return seen

    def findings(self, response):
        """Ordered findings against a start or resume response.

        The approval policy is decided first and alone, and it is judged against what the caller
        DECLARED. A thread whose policy is not the declared one is not one mismatch among several:
        the caller is addressing a thread in a state it did not expect, and letting a generic
        mismatch shadow that would report a wrongly-addressed thread as a retryable difference.

        Declaring nothing keeps the original meaning exactly: the default declaration is never, so
        a returned on-request still refuses. What changes is that a caller who knows the thread is
        on on-request can now say so and be judged against the truth, instead of against an
        assumption no caller could reach. The policy is still never transmitted, so this decides
        whether to USE the thread, never what the thread's policy becomes.
        """
        returned_policy = response.get("approvalPolicy")
        if returned_policy is None:
            return [
                {
                    "code": SETTING_UNOBSERVABLE,
                    "field": "approvalPolicy",
                    "expected": self.approval_policy,
                    "returned": None,
                }
            ]
        if returned_policy != self.approval_policy:
            label = returned_policy if isinstance(returned_policy, str) else "granular"
            return [
                {
                    "code": UNSUPPORTED_APPROVAL_POLICY,
                    "field": "approvalPolicy",
                    "expected": self.approval_policy,
                    "returned": label,
                }
            ]

        found = []
        seen = self.observed(response)
        asked = self.requested
        for field in FIELD_PRECEDENCE:
            if field not in asked:
                continue
            expected = asked[field]
            if field not in seen:
                found.append(
                    {
                        "code": SETTING_UNOBSERVABLE,
                        "field": field,
                        "expected": expected,
                        "returned": None,
                    }
                )
                continue
            returned = seen[field]
            if field == "sandbox":
                expected = normalise_policy(expected)
                normalised = normalise_policy(returned)
                if normalised is None:
                    # The host answered with something that is not a sandbox policy. That is a
                    # value we disagree with, not a value we could not see, and indexing it here
                    # would raise: _mutate would then record outcome_unknown, claiming the
                    # message may have been delivered when nothing was ever sent.
                    # This refuses even when only a MODE was requested and the reported type
                    # happens to match: a policy we cannot read in full is not a policy that
                    # confirms anything about the thread.
                    found.append(
                        {
                            "code": SETTINGS_NOT_PRESERVED,
                            "field": field,
                            "expected": expected,
                            "returned": returned,
                        }
                    )
                    continue
                if self.expected_policy is None:
                    # Only a mode was requested, so only the resolved type was asked about.
                    expected = {"type": expected["type"]}
                    returned = {"type": normalised["type"]}
                else:
                    returned = normalised
            if expected != returned:
                found.append(
                    {
                        "code": SETTINGS_NOT_PRESERVED,
                        "field": field,
                        "expected": expected,
                        "returned": returned,
                    }
                )
        return found

    def approvals(self, response):
        """What this observation supports saying about approvals on the thread just addressed.

        Separate from the settings receipt on purpose. That receipt answers "did the thread come
        back in the state I asked for"; this one answers "who decides, and what happens if this
        turn asks" -- which is the question a report delivery has to keep apart from the code
        execution a report may provoke.

        ThreadResumeResponse requires both approvalPolicy and approvalsReviewer, so both are read
        rather than guessed. Neither is ever written.
        """
        observed = response.get("approvalPolicy")
        interactive = observed != UNATTENDED_APPROVAL_POLICY
        return {
            "declared": self.approval_policy,
            "observed": observed,
            # The host's own answer to "who reviews approvals here". Read only; this bridge sends
            # no approvalsReviewer and so cannot move a thread's approvals to another reviewer.
            "reviewer": response.get("approvalsReviewer"),
            "transmitted": False,
            "preservation": "omitted_from_resume",
            "interactive": interactive,
            "servicedByThisBridge": False,
            "onApprovalRequest": "left_for_thread_approver",
            "meaning": (
                "This thread may ask for an approval during the turn. This bridge answers none; "
                "the host sends the request to every client subscribed to the thread, the "
                "thread's own included, and applies the first answer, so the decision stays with "
                "the thread's approver and the turn waits until it is made."
                if interactive
                else "Nothing on this thread can ask for an approval, so delivery and approval "
                "cannot be confused here."
            ),
            "limits": APPROVAL_LIMITS,
        }

    def receipt(self, response, *, at: str):
        """The observable settings receipt. It never claims more than the observation supports."""
        findings = self.findings(response)
        asked = self.requested
        if findings:
            verification = "refused"
        elif not asked:
            verification = "not_requested"
        else:
            verification = f"observed_at_{at}"
        return {
            "requested": asked,
            "actual": self.observed(response),
            "verified": [] if findings else sorted(asked),
            "unobservable": sorted(
                f["field"] for f in findings if f["code"] == SETTING_UNOBSERVABLE
            ),
            "findings": findings,
            "verification": verification,
            "observationLimits": OBSERVATION_LIMITS,
        }


def annotation(before: dict, thread: dict):
    """Compare a post-acceptance thread/read against what was observed before dispatch.

    thread/read reports model, reasoningEffort and cwd but nothing about sandbox or approval
    policy, so concurrentChange False means only that those three were unchanged. The caller
    records this beside an already accepted receipt; it can never change that receipt's status.
    """
    after, unobserved = {}, []
    for field in ANNOTATED:
        value = thread.get(field)
        if value is None:
            # A field that was observed before dispatch and is absent now was NOT compared.
            # Dropping it silently would let it sit in covers while concurrentChange said false,
            # which is the same "silence reads as proof" mistake this whole contract exists to
            # avoid, committed by its own diagnostic.
            if field in before:
                unobserved.append(field)
            continue
        after[field] = value
    changed = {
        field: {"observed": before.get(field), "afterDispatch": value}
        for field, value in after.items()
        if field in before and before[field] != value
    }
    return {
        "fields": after,
        "concurrentChange": bool(changed),
        "changed": changed,
        "covers": sorted(field for field in after if field in before),
        "unobserved": unobserved,
        "limit": "thread/read reports neither sandbox nor approvalPolicy, so no change here "
        "means only that the fields in covers were compared and unchanged. Anything in "
        "unobserved was not compared at all.",
    }
