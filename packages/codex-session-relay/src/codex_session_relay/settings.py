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

import json

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
# A thread loaded by a resume that transmitted nothing is not what the record says. Nothing was
# sent to change it, so what differs is the record or the host's own state, never a request the
# host ignored: re-record from a reading the user stands behind rather than retrying.
SETTINGS_DIFFER_AFTER_LOAD = "settings_differ_after_load"

# The approval policy a task this relay CREATES runs under (managed.py). It is not the set of
# policies the transport can carry; that is CARRIED_APPROVAL_POLICIES below.
AUTHORIZED_APPROVAL_POLICY = "never"

# The approval policies a recipient can be on for this transport to wake it (CRW-225). Measured on
# codex-cli 0.154.0, the host sends every approval request of a turn to each client subscribed to
# the thread, replays a pending one to a client that resumes the thread later, and applies the first
# answer; the bridge answers none. So a turn the relay starts on an on-request thread leaves every
# approval with the thread's own approver, and the turn waits for it. untrusted and a granular
# policy are not carried: they were not measured in that scope, and an untrusted thread asks even to
# record a report. The record is compared against this set in require_usable() and the host's answer
# in mismatches(); the relay never SENDS a policy on resume, so which of the two a thread is on is
# the thread's own state, never something this transport chose.
CARRIED_APPROVAL_POLICIES = ("never", "on-request")

# A carried policy that differs from the record: delivered and noted, not refused. The difference is
# the user's own change to the thread (nothing here sets a policy), and refusing it would leave the
# very case CRW-225 exists for - a parent switched to on-request in its own client - undeliverable.
APPROVAL_POLICY_DIFFERS_FROM_RECORD = "approval_policy_differs_from_record"

# Workspace roots a resume reported narrower than the record, delivered and noted (CRW-235). A
# resume never changes the roots of a thread the host already has loaded - the thread keeps the
# roots of whichever load brought it in, and a plain load brings it in with its cwd as the only
# root - and a workspace-write thread's reported writableRoots follow those roots. Measured on
# codex-cli 0.154.0 with an isolated app-server. Narrower is inside what the record authorizes,
# so it is accepted where mismatches() allows it and written on the transport receipt, because a
# recipient on fewer roots than recorded may lack write access to one the record names.
RUNTIME_ROOTS_NARROWER = "runtime_roots_narrower_than_record"

# Who recovers a settings hold, and how (CRW-235). One table for status, assignment-show and the
# fault sweep, so no two of them can name a different actor for one hold. The command is a NAME
# ("settings-show" or "show-event"); the caller renders it for its own store, because this module
# is imported everywhere and must not import the renderer's.
SETTINGS_SHOW = "settings-show"
SHOW_EVENT = "show-event"
# The two refusals the daemon itself may clear: the host answered without the setting, which a
# later answer can supply. Every other settings code is a difference only a person can resolve.
DAEMON_SETTINGS_CODES = (SETTING_UNOBSERVABLE, ENVIRONMENTS_UNKNOWN)
HOLD_OPERATOR_THEN = (
    "bring the recipient back under its recorded settings - another client loaded it under"
    " other ones, and it loads under the record once the host has unloaded it and the relay"
    " loads it again - or, if the user changed the task, re-record it with settings-record"
    " --source user_transition; a refusal of the record itself names its own recovery. The"
    " daemon retries on its own each pass until the attempt cap"
)
HOLD_DAEMON_THEN = (
    "the daemon retries on its own each pass; if the host keeps answering without the setting,"
    " the operator compares its reading with settings-show"
)
HOLD_CAPPED_THEN = (
    "nothing sends this delivery again: read the report, then open a fresh execution"
    " generation (generation-open) if the work still needs verifying"
)
HOLD_CHANNEL_THEN = "the report is stored where the recipient reads it: read it and acknowledge"
# A revision request the child's approval policy stored without waking it. It takes no
# acknowledgement (ack.py accepts one for a completion only), so the parent's path is the one a
# held correction already has (CRW-231): read it here, and open a fresh execution generation if the
# child still has to be given it (the review of 566eecb5).
HOLD_CORRECTION_CHANNEL_THEN = (
    "the revision request is stored where the child reads it, without waking the child, and it"
    " takes no acknowledgement: read it, then open a fresh execution generation (generation-open)"
    " if the child still has to be given it"
)
HOLD_UNDETERMINED_THEN = (
    "this state was recorded before its cause was written down, so the cause is not"
    " established here: read the event's attempts and their receipts, then settings-show;"
    " nothing here claims a settings fix"
)
LATER_BY_OPERATOR = (
    "for later deliveries, the operator brings the recipient back under its recorded settings"
    " or re-records it (settings-record --source user_transition); settings-show names the"
    " difference"
)
LATER_BY_OWNER = (
    "for later deliveries, the thread's owner switches it back to an approval policy this"
    " transport carries (never or on-request)"
)
# Refusals whose repair is not the thread's settings, each with its own step (Devin on 1f0f5a89):
# the role gate's two (docs/role-execution-policy.md, "What each refusal means"), which neither
# bringing the thread back under its record nor re-recording it repairs, and the record's own
# refusals, where there is no record to bring the thread back under.
HOLD_REFUSAL_THEN = {
    RefusalReason.ROLE_POLICY_UNCONFIGURED.value: (
        "the sending relay process has no declared role policy and the recipient is role-bound:"
        " set the role policy variable for the relay process and restart it, and the withheld"
        " delivery resumes by itself; re-recording settings does not change this"
    ),
    RefusalReason.ROLE_BINDING_MISMATCH.value: (
        "the task's cited role and its bound role disagree, or its recorded pair is not that"
        " role's pair: fix the binding or the creation; do not re-record over it"
    ),
    # The record itself refused before any host call: there is nothing to bring the thread back
    # under, so the step is the record (settings-show names what is missing or wrong).
    **{code: (
        "the recipient's authorization record was refused before any host call (missing,"
        " incomplete, mistyped, a sandbox type this transport cannot carry, or behind its role's"
        " current pair): record it again from the creation result or a user-attributed source"
        " (settings-record --source user_transition), and the next pass reads it again"
    ) for code in (RefusalReason.SETTINGS_UNAVAILABLE.value, RefusalReason.SETTINGS_INCOMPLETE.value,
                   RefusalReason.SETTINGS_MISTYPED.value,
                   RefusalReason.UNSUPPORTED_SANDBOX_TYPE.value,
                   RefusalReason.SETTINGS_RECORD_STALE_FOR_ROLE.value)},
}


def settings_hold_recovery(kind, code, source, *, revision=False) -> dict:
    """Who recovers a settings hold of this kind and code, and how: {actor, command, then,
    laterDeliveries}. kind is withheld, capped or channel_closed; source is the reader's
    (attempt, pre_send or undetermined); revision says the delivery is a revision request to the
    child rather than a completion, which only a closed channel reads differently."""
    if source == "undetermined":
        return {"actor": "operator", "command": SHOW_EVENT, "then": HOLD_UNDETERMINED_THEN,
                "laterDeliveries": None}
    role_then = HOLD_REFUSAL_THEN.get(code)
    if kind == "capped":
        return {"actor": "parent", "command": SHOW_EVENT, "then": HOLD_CAPPED_THEN,
                "laterDeliveries": ("for later deliveries, " + role_then) if role_then
                else LATER_BY_OPERATOR}
    if kind == "channel_closed":
        return {"actor": "parent", "command": SHOW_EVENT,
                "then": HOLD_CORRECTION_CHANNEL_THEN if revision else HOLD_CHANNEL_THEN,
                "laterDeliveries": LATER_BY_OWNER}
    if code in DAEMON_SETTINGS_CODES:
        return {"actor": "daemon", "command": SETTINGS_SHOW, "then": HOLD_DAEMON_THEN,
                "laterDeliveries": None}
    if role_then:
        return {"actor": "operator", "command": SETTINGS_SHOW, "then": role_then,
                "laterDeliveries": None}
    return {"actor": "operator", "command": SETTINGS_SHOW, "then": HOLD_OPERATOR_THEN,
            "laterDeliveries": None}


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
    declared = POLICY_DEFAULTS.get(kind, {})
    merged = dict(declared)
    merged.update({key: value for key, value in policy.items() if key != "type"})
    merged["type"] = kind
    # Every field the declared defaults name holds its default's type, exactly: a flag is a
    # boolean and nothing Python compares equal to one (0, 1), a roots list is a list of text.
    # Without this a record holding networkAccess 0 agreed with a host answering false, and a
    # record and an answer that both held [1] or [123] agreed with each other, so a turn
    # started on a sandbox nobody can read (the CRW-215 live-findings reviews). A field the
    # pinned policy does not declare has no type to hold it to; it is compared exactly, as a
    # JSON value (see _canonical), and nothing else is claimed for it.
    for key, default in declared.items():
        value = merged[key]
        if isinstance(default, bool):
            readable = type(value) is bool
        elif isinstance(default, list):
            readable = _text_list(value)
        else:
            readable = type(value) is type(default)
        if not readable:
            return None
    if "writableRoots" in merged:
        roots = merged["writableRoots"]
        if not _text_list(roots):
            return None
        merged["writableRoots"] = list(roots)
    return merged


def normalise_environments(environments):
    """None stays None. It means unknown, and unknown is never flattened into empty.

    Each entry is kept WHOLE. It used to keep only environmentId, cwd and runtimeWorkspaceRoots,
    so a key a recorded environment held and the answer lacked was dropped before any comparison
    could see it, and the turn started with it unverified (the CRW-215 live-findings review of
    bb6ca6e4). An environment is compared the way the sandbox is: its declared keys typed
    (environments_problem), every other key as the same JSON value, and only its roots filled
    in - TurnEnvironmentParams says an omitted roots list defaults to the cwd.
    """
    if environments is None:
        return None
    out = []
    for entry in environments:
        one = dict(entry)
        roots = one.get("runtimeWorkspaceRoots")
        one["runtimeWorkspaceRoots"] = list(roots) if roots is not None else [entry["cwd"]]
        out.append(one)
    return out


class TaskSettings:
    """Complete or unusable, and presence alone was never the whole of it.

    A record missing a required field cannot say what it is preserving. Neither can one whose
    cwd, model or reasoningEffort is present and is not a string, because resume_params would
    copy it onto the wire and only the host could then say what it had done with it.
    Nor can one whose approvalPolicy is outside the carried set (never, on-request), and that one
    is refused for a different reason: it is not a value the host answers for at all, because this
    transport carries no other policy. require_usable() decides all three, and it types
    runtimeWorkspaceRoots and environments as well: it used not to, and `runtimeWorkspaceRoots`
    of "/a/bc" then passed here, went on the wire as ["/", "a", "/", "b", "c"], and let the
    narrower-only comparison after a settings-free load read a host root "/" as one the record
    names (the CRW-215 live-findings review). Roots are a list of text; environments are a list
    of objects whose environmentId and cwd are text and whose roots, where given, are a list of
    text.

    JUN-92 populates this from the creation result Run already receives; it is not a separate
    handshake and it asks for nothing the host did not already report at creation.
    """

    def __init__(self, data: dict):
        self.data = dict(data or {})
        # Set by the delivery gate when this recipient's pair was not derived from its role's
        # declared pair: a supervisor's, which is the user's own selection, or one an exception
        # admitted. Such a pair is never transmitted, loaded or not. A resume can apply what it
        # transmits while the host materializes the thread, which would restore a value the user
        # may have changed, and the unload can happen between any read and the resume. So the
        # transport resumes this recipient with nothing requested (settings_free_resume_params):
        # an unloaded thread loads under its own persisted state, a loaded one reports it, and
        # mismatches(..., transmitted=False) compares that answer with the record before any
        # turn starts.
        self.settings_free_resume = False

    # ------------------------------------------------------------- validity

    def missing(self) -> list:
        absent = [field for field in REQUIRED if self.data.get(field) is None]
        # environments may legitimately be an empty list (no environments selected), which is a
        # decision, unlike None which is an absence. Re-admit that case.
        if "environments" in absent and isinstance(self.data.get("environments"), list):
            absent.remove("environments")
        return absent

    def mistyped(self) -> list:
        """Recorded fields whose shape this record does not hold.

        The three the resume contract types as strings, and the two lists every comparison
        reads (runtimeWorkspaceRoots, environments: see environments_problem). The sandbox is
        typed by normalise_policy, one gate later.

        Presence was never the whole question. resume_params copies each of these values
        straight into ThreadResumeParams, so a recorded `cwd: 7` used to be built and sent, and
        the only answer about it came back from a host whose schema is not in this repository.
        The receipt could not say whether the host had accepted it, which is why the rule has to
        run before the send rather than be read off the response.

        Only meaningful once missing() is empty, and it subscripts self.data to say so:
        require_usable() orders the two, exactly as resume_params and mismatches already assume
        a complete record. isinstance excludes bool as well, so True is not read as a model name.

        approvalPolicy is not here. The transport carries `never` and `on-request`
        (CARRIED_APPROVAL_POLICIES), and require_usable() compares the recorded value against
        that set immediately after this gate, so every value that field could hold and should
        not -- 7, True, a granular object, untrusted -- is already refused there, by the more
        specific of the two codes. A null is not among them: missing() has answered it one gate
        earlier.
        """
        wrong = []
        if not isinstance(self.data["cwd"], str):
            wrong.append("cwd")
        if not isinstance(self.data["model"], str):
            wrong.append("model")
        if not isinstance(self.data["reasoningEffort"], str):
            wrong.append("reasoningEffort")
        # The two fields every comparison reads as lists. Typed here and not only where they are
        # compared, because a record that holds text where a list belongs is read by list() as
        # its characters, and every comparison after that - exact on a transmitted resume,
        # narrower-only after a settings-free load - would be answering about the characters.
        if not _text_list(self.data["runtimeWorkspaceRoots"]):
            wrong.append("runtimeWorkspaceRoots")
        if environments_problem(self.data["environments"]) is not None:
            wrong.append("environments")
        # expectedPermissionProfile is not typed: it is the host's own value, carried whole from
        # the creation receipt (an object such as {"id", "extends", "rules"}), and nothing here
        # can say which shapes a host may report. mismatches() compares it with the answer as a
        # JSON value, where 0 and false differ and an absent key is not a null one.
        return wrong

    def _mistyped_detail(self, field) -> str:
        """What a mistyped field holds, in the words an operator re-records it from."""
        value = self.data[field]
        if field == "runtimeWorkspaceRoots":
            return f"runtimeWorkspaceRoots is {_shape(value)}, not a list of str"
        if field == "environments":
            where, what = environments_problem(value)
            return f"environments{where} {what}"
        return f"{field} is {type(value).__name__}, not str"

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
                "; ".join(self._mistyped_detail(field) for field in wrong),
            )
        if self.data["approvalPolicy"] not in CARRIED_APPROVAL_POLICIES:
            # Meaning, after shape, and FIRST among the meaning gates, because mismatches()
            # decides this same field first: a row that is wrong in both this and its sandbox
            # gets one answer rather than two that depend on which surface refused it.
            #
            # Compared against the RECORD and not only against the response, because a row this
            # transport cannot carry should never have been stored: refused here it is refused
            # once, by whoever recorded it, rather than at every send. The refusal is the same
            # code the verification reports, so the two surfaces name one situation alike.
            #
            # Not a type rule, which is why mistyped() leaves the field alone: this comparison
            # refuses every wrong value the field can hold, whatever its type, and says the
            # specific thing about it.
            raise DeliveryRefused(
                RefusalReason.UNSUPPORTED_APPROVAL_POLICY,
                f"the recorded approvalPolicy is {self.data['approvalPolicy']!r}; this transport"
                " carries only " + " and ".join(repr(p) for p in CARRIED_APPROVAL_POLICIES)
                + ", leaving every approval a turn raises with the thread's own approver",
            )
        if self.sandbox_mode() is None:
            # Two wordings for one decision. sandbox_mode() alone decides; the shape test below
            # only chooses what to SAY, because a row holding the bare mode name "workspaceWrite"
            # and a row holding {"type": "externalSandbox"} are wrong in different ways and the
            # repair differs. Saying "'workspaceWrite' has no ThreadResumeParams.sandbox mode"
            # would be false of that name -- it is one -- and would send an operator looking for
            # another sandbox type instead of re-recording the policy object.
            recorded = self.data["sandbox"]
            if not isinstance(recorded, dict):
                raise DeliveryRefused(
                    RefusalReason.UNSUPPORTED_SANDBOX_TYPE,
                    f"the recorded sandbox is {type(recorded).__name__}, not the policy object a"
                    " creation result reports, so it does not record the full policy a resume"
                    " would have to restore",
                )
            raise DeliveryRefused(
                RefusalReason.UNSUPPORTED_SANDBOX_TYPE,
                f"{recorded.get('type')!r} has no"
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
        """Total, for the same reason normalise_policy() is, and load-bearing for its caller.

        These rows can hold whatever an older writer or a hand edit left behind, and a value this
        function cannot read has to come BACK as None so a gate can refuse it. It used to raise
        instead: a sandbox recorded as the bare string "workspaceWrite", or as a list, reached
        .get() and left an AttributeError where require_usable() owes its caller a DeliveryRefused,
        and a {"type": {...}} reached the lookup below and raised TypeError on an unhashable key --
        which is the case normalise_policy() already guards one screen above. Registration,
        delivery and settings-show all validate through require_usable(), so each of them answered
        a legacy row with a traceback or a generic "unexpected" finding rather than with the
        refusal this package already had for an unreadable policy.
        """
        policy = self.data.get("sandbox")
        if not isinstance(policy, dict):
            return None
        kind = policy.get("type")
        # Not just a non-string: an unhashable one raises on the lookup below.
        if not isinstance(kind, str):
            return None
        return RESUME_SANDBOX_MODE.get(kind)

    # --------------------------------------------------------------- params

    def resume_params(self, thread_id: str) -> dict:
        """Only fields ThreadResumeParams actually defines. No effort field, no environments.

        Effort and the sandbox policy detail have no dedicated resume parameters, but the schema
        accepts a free-form config object, so both travel there under the same keys the bridge
        uses. This matters beyond the effort: ThreadResumeParams.sandbox is only a MODE, so a
        resume that sent the mode alone could not restore writable roots or the network flag, and
        would then compare against values it never asked for.

        No approvalPolicy, on purpose (CRW-225). The host applies a resume's policy to a thread
        that resume loads, so sending the recorded one would set it: a thread whose owner switched
        it to on-request after it was recorded would be loaded under never, which is forcing
        never. The thread's own policy is reported back and judged in mismatches() instead.
        """
        params = {
            "threadId": thread_id,
            "excludeTurns": True,
            "sandbox": self.sandbox_mode(),
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

    @staticmethod
    def settings_free_resume_params(thread_id: str) -> dict:
        """The resume that transmits nothing: the bridge's own nothing-requested form.

        It loads an unloaded thread under the thread's persisted state and reports what that
        state is, and on a loaded thread it only reports. Nothing in it can set a setting.
        """
        return {"threadId": thread_id, "excludeTurns": True}

    # ------------------------------------------------------------ verifying

    def approval_divergence(self, response):
        """A carried policy the thread reports that is not the recorded one, as a note or None.

        Read only after mismatches() found nothing, so the reported policy is one this transport
        carries. It is delivered: nothing here sent a policy, so the difference is the thread's
        own state (its owner switched it), and the turn's approvals go to that owner either way.
        The note keeps the difference visible on the transport receipt so the record can be
        re-recorded; it is not a refusal and not a retry.
        """
        if not isinstance(response, dict):
            return None
        observed = response.get("approvalPolicy")
        recorded = self.data.get("approvalPolicy")
        if observed == recorded or observed not in CARRIED_APPROVAL_POLICIES:
            return None
        return {"code": APPROVAL_POLICY_DIFFERS_FROM_RECORD, "field": "approvalPolicy",
                "recorded": recorded, "observed": observed}

    def mismatches(self, response: dict, *, transmitted: bool = True,
                   exact_approval_policy: bool = False, loaded_before: bool = False) -> list:
        """Ordered findings against a resume response. Order is behaviour, not presentation.

        The approval policy is checked FIRST, against the set this transport carries, not against
        the record. A returned policy outside that set (untrusted, granular) is the
        push-channel-closed case, and raising the generic mismatch first would turn a permanently
        closed channel into a retry loop. A carried policy is accepted even when it differs from
        the record: nothing here sent a policy, so the difference is the thread's own state, and
        approval_divergence() notes it without refusing.

        exact_approval_policy=True is for a task this relay CREATED (managed.py): what came back
        must be the policy that was requested, because a child created on another policy is not
        the child requested, whether or not this transport could carry it.

        What is in question here is the HOST's answer, not the record's request. require_usable()
        has already refused a record outside the carried set, so a finding on this line means the
        host reported a policy this transport cannot carry -- which is exactly the half only the
        host can settle, and the half no rule on the record can pre-empt.

        Within each field, ABSENCE is decided before difference. A host that reported nothing has
        told us nothing about whether the setting was applied, which is a different fact from a
        host that reported something else, and the two need different answers from a caller.

        transmitted=False reads a resume that requested nothing (settings_free_resume_params).
        The model, the effort, the sandbox policy, the cwd and the environment selection are
        compared exactly as ever, and the approval policy against the carried set as on the
        transmitted route. The workspace roots are not: a load that transmits nothing restores
        only what the host persists, and measured on the live host it brought a thread back with
        its roots reduced to its cwd while every other field held (CRW-215 live finding F2). So
        roots, at the top level and in each environment, may come back NARROWER than recorded and
        never wider - a narrower set is inside what the record authorizes, a wider one is not.

        loaded_before=True extends the same allowance to a resume that DID transmit the record,
        when this send's own thread/read found the recipient already loaded (idle) just before
        it (CRW-235). A resume never changes the roots of a thread the host already has loaded:
        the thread keeps the roots of whichever load brought it in, which for another task's
        plain bridge message is its cwd alone. Measured on codex-cli 0.154.0: a resume sending
        other roots to a loaded thread changes nothing, and one sending them to a notLoaded thread
        applies them, so a transmitted resume to a recipient read as notLoaded still has to come
        back exact (loaded_before stays False there).

        Wherever the roots may narrow, so may the one sandbox field the host derives from them: a
        workspace-write thread reports its non-cwd runtime roots as writableRoots, so a narrowed
        load reports fewer. Both sides must be workspaceWrite, every other sandbox field stays
        exact, and the writable roots may only be a subset of the recorded ones (_sandbox_within).
        """
        found = []
        if not isinstance(response, dict):
            # Total over the answer, like normalise_policy: this runs BEFORE turn/start, and an
            # exception here is recorded as an unknown outcome for a send that was in fact
            # withheld. A shape the comparison cannot read is a setting it cannot observe.
            return [{"code": SETTING_UNOBSERVABLE, "field": "response",
                     "expected": "a resume response object", "returned": None,
                     "returnedShape": type(response).__name__}]
        returned_policy = response.get("approvalPolicy")
        if returned_policy is None:
            # Not the closed-channel case: a policy we cannot see is not a policy we know is
            # interactive. It withholds, and stays eligible for a bounded pre-send retry.
            return [{"code": SETTING_UNOBSERVABLE, "field": "approvalPolicy",
                     "expected": self.data.get("approvalPolicy"), "returned": None}]
        if exact_approval_policy and returned_policy != self.data.get("approvalPolicy"):
            label = returned_policy if isinstance(returned_policy, str) else "granular"
            return [{"code": UNSUPPORTED_APPROVAL_POLICY, "field": "approvalPolicy",
                     "expected": self.data.get("approvalPolicy"), "returned": label,
                     "returnedShape": type(returned_policy).__name__}]
        if not isinstance(returned_policy, str) or returned_policy not in CARRIED_APPROVAL_POLICIES:
            # A granular policy is an object; it is never copied into the frozen record, which
            # types this field as string or null. Contract v1 admits only a plain string.
            label = returned_policy if isinstance(returned_policy, str) else "granular"
            found.append({"code": UNSUPPORTED_APPROVAL_POLICY, "field": "approvalPolicy",
                          "expected": list(CARRIED_APPROVAL_POLICIES), "returned": label,
                          "returnedShape": type(returned_policy).__name__})
            return found

        thread = response.get("thread")
        if thread is None:
            thread = {}
        if not isinstance(thread, dict):
            found.append({"code": SETTING_UNOBSERVABLE, "field": "environments",
                          "expected": self.data["environments"], "returned": None,
                          "returnedShape": "thread is " + type(thread).__name__})
            return found
        returned_environments = thread.get("environments")
        if returned_environments is None:
            # Thread.environments documents null as not loaded OR the server does not expose its
            # selection. Unknown is not absence, and it is never read as an empty selection.
            found.append({"code": ENVIRONMENTS_UNKNOWN, "field": "environments",
                          "expected": self.data["environments"], "returned": None})
            return found
        unreadable = environments_problem(returned_environments)
        if unreadable is not None:
            # Reported, but not as a selection this comparison can read: no more an answer
            # about the environments than a null is, and never a reason to raise.
            found.append({"code": SETTING_UNOBSERVABLE, "field": "environments",
                          "expected": self.data["environments"], "returned": None,
                          "returnedShape": "environments" + " ".join(unreadable)})
            return found
        got_environments = normalise_environments(returned_environments)
        # Where a narrower reading is inside what the record authorizes: after a resume that
        # requested nothing, or after one sent to a thread that was already loaded.
        narrowable = not transmitted or loaded_before
        if environments_problem(self.data["environments"]) is not None:
            # require_usable() refuses such a record before any send; this is the same rule
            # where the comparison stands on its own, so an unreadable record is never measured
            # through its characters and never granted the narrower-only allowance.
            found.append({"code": SETTINGS_NOT_PRESERVED, "field": "environments",
                          "expected": self.data["environments"],
                          "returned": got_environments})
        else:
            expected_environments = normalise_environments(self.data["environments"])
            if not (_environments_within(got_environments, expected_environments)
                    if narrowable
                    else _canonical(got_environments) == _canonical(expected_environments)):
                found.append({"code": SETTINGS_NOT_PRESERVED, "field": "environments",
                              "expected": expected_environments,
                              "returned": got_environments})

        recorded_roots = self.data["runtimeWorkspaceRoots"]
        roots_readable = _text_list(recorded_roots)
        expectations = {
            "sandbox": normalise_policy(self.data["sandbox"]),
            "cwd": self.data["cwd"],
            # An unreadable record is compared as it is, so it matches no list a host reports.
            "runtimeWorkspaceRoots": list(recorded_roots) if roots_readable else recorded_roots,
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
            if field == "sandbox" and narrowable and _sandbox_within(returned, expected):
                continue
            if field == "runtimeWorkspaceRoots":
                if not _text_list(returned):
                    # list(123) raised here, and list("/a/b") compared characters.
                    found.append({"code": SETTING_UNOBSERVABLE, "field": field,
                                  "expected": expected, "returned": None,
                                  "returnedShape": _shape(returned)})
                    continue
                returned = list(returned)
                if narrowable and roots_readable and _roots_within(returned, expected):
                    continue
            # As JSON values, not by Python equality, under which 0 == False and 1 == True.
            if _canonical(expected) != _canonical(returned):
                found.append({"code": SETTINGS_NOT_PRESERVED, "field": field,
                              "expected": expected, "returned": returned})

        profile = response.get("activePermissionProfile")
        expected_profile = self.data.get("expectedPermissionProfile")
        if profile is None and expected_profile is not None:
            # Absence is not agreement, here as for every other field the record holds: a record
            # that names a profile has to see the host report one before a turn may start. This
            # was skipped - only a REPORTED profile was compared - so a recorded profile the
            # answer left out, or answered null, went unverified (the CRW-215 live-findings
            # review of d88c169e). A record with no profile still accepts an answer with none.
            found.append({"code": SETTING_UNOBSERVABLE, "field": "activePermissionProfile",
                          "expected": expected_profile, "returned": None})
        elif profile is not None and _canonical(profile) != _canonical(expected_profile):
            # A profile we did not anticipate is a permission source we cannot interpret.
            found.append({"code": UNVERIFIABLE_PERMISSION_PROFILE,
                          "field": "activePermissionProfile",
                          "expected": expected_profile,
                              "returned": profile})
        return found

    def roots_narrowing(self, response, *, status_before=None) -> list:
        """Notes for each place a resume reported fewer roots than the record, or [].

        Read only after mismatches() found nothing, so every reported set is already within the
        recorded one. A note is written where the reported SET is a strict subset: a reordering of
        the same roots is no narrowing. Three places can narrow - the top-level roots, each
        environment's roots, and a workspace-write sandbox's writableRoots, compared on the
        normalised policies so an omitted list reads as the declared empty default. Total, like
        mismatches(): a shape it cannot read produces no note rather than an exception.
        """
        notes = []
        if not isinstance(response, dict):
            return notes

        def note(field, recorded, observed):
            if _text_list(recorded) and _text_list(observed) and set(observed) < set(recorded):
                notes.append({"code": RUNTIME_ROOTS_NARROWER, "field": field,
                              "recorded": list(recorded), "observed": list(observed),
                              "statusBeforeResume": status_before})

        note("runtimeWorkspaceRoots", self.data.get("runtimeWorkspaceRoots"),
             response.get("runtimeWorkspaceRoots"))
        thread = response.get("thread")
        returned = thread.get("environments") if isinstance(thread, dict) else None
        recorded = self.data.get("environments")
        if (returned is not None and environments_problem(returned) is None
                and environments_problem(recorded) is None):
            pairs = zip(normalise_environments(returned), normalise_environments(recorded))
            for index, (got, allowed) in enumerate(pairs):
                note(f"environments[{index}].runtimeWorkspaceRoots",
                     allowed["runtimeWorkspaceRoots"], got["runtimeWorkspaceRoots"])
        got_policy = normalise_policy(response.get("sandbox"))
        recorded_policy = normalise_policy(self.data.get("sandbox"))
        if (got_policy is not None and recorded_policy is not None
                and got_policy.get("type") == recorded_policy.get("type") == "workspaceWrite"):
            note("sandbox.writableRoots", recorded_policy.get("writableRoots"),
                 got_policy.get("writableRoots"))
        return notes


def _shape(value) -> str:
    """A value's shape in one phrase: its type, or for a list the first member that is not text."""
    if isinstance(value, list):
        for one in value:
            if not isinstance(one, str):
                return "a list holding " + ("None" if one is None else type(one).__name__)
        return "a list of str"
    return "absent" if value is None else type(value).__name__


def _canonical(value) -> str:
    """A value as the JSON it is, so two values agree only where their JSON does.

    Every comparison between a record and a host's answer is made on this. Python's == says
    0 == False, 1 == True and 1 == 1.0, and each of those once let an answer the record does not
    hold read as agreement. Keys are sorted, so an object's key order never differs.
    """
    return json.dumps(value, sort_keys=True, default=repr)


def _text_list(value) -> bool:
    """A list whose every member is text: the only roots shape either side may hold."""
    return isinstance(value, list) and all(isinstance(one, str) for one in value)


def environments_problem(environments):
    """Why an environment selection cannot be read, as (where, what), or None when it can.

    Readable is a list of objects, each with an environmentId and a cwd that are text and, where
    the key is present, runtimeWorkspaceRoots that are a list of text (an omitted list defaults to
    the cwd; a null one is present and is not a list).
    An empty list is readable: it is a selection of none. The caller decides what None means
    before asking; here it is simply not a list.
    """
    if not isinstance(environments, list):
        return ("", f"is {_shape(environments)}, not a list of environment objects")
    for index, entry in enumerate(environments):
        if not isinstance(entry, dict):
            return (f"[{index}]", f"is {_shape(entry)}, not an object")
        for key in ("environmentId", "cwd"):
            if not isinstance(entry.get(key), str):
                return (f"[{index}].{key}", f"is {_shape(entry.get(key))}, not str")
        # Absent is the documented default (TurnEnvironmentParams: the cwd). Null is not absent
        # and not a list, so it says nothing about the roots and is refused like any other
        # shape; reading it as the cwd agreed on a value neither side had reported.
        if "runtimeWorkspaceRoots" in entry:
            roots = entry["runtimeWorkspaceRoots"]
            if not _text_list(roots):
                return (f"[{index}].runtimeWorkspaceRoots",
                        f"is {_shape(roots)}, not a list of str")
    return None


def _roots_within(returned, recorded) -> bool:
    """Every root the host reports is one the record names: narrower is within, wider is not."""
    return all(root in recorded for root in returned)


def _sandbox_within(returned, recorded) -> bool:
    """A reported sandbox inside the recorded one: the same workspace-write policy with fewer
    writable roots at most.

    Both arguments are normalised policies (normalise_policy), and the caller has already refused
    either side being unreadable. Only workspaceWrite relaxes, and only its writableRoots: the
    type, the network flag, the tmp exclusions and any key the pinned contract does not declare
    are the same JSON on both sides, and the writable roots are text lists whose every member the
    record names. Another sandbox type carrying a writableRoots key compares exactly.
    """
    if not (isinstance(returned, dict) and isinstance(recorded, dict)):
        return False
    if returned.get("type") != "workspaceWrite" or recorded.get("type") != "workspaceWrite":
        return False
    rest = [{key: value for key, value in one.items() if key != "writableRoots"}
            for one in (returned, recorded)]
    if _canonical(rest[0]) != _canonical(rest[1]):
        return False
    got, allowed = returned.get("writableRoots"), recorded.get("writableRoots")
    return _text_list(got) and _text_list(allowed) and _roots_within(got, allowed)


def _environments_within(returned, recorded) -> bool:
    """The same environments, in the same order, each whole as recorded, with roots within.

    The selection itself is exact: another environment, a missing one or an empty selection is a
    different place to run, not a narrower one. Only each environment's roots may shrink.
    """
    if returned is None or recorded is None or len(returned) != len(recorded):
        return False
    for got, allowed in zip(returned, recorded):
        # Everything but the roots exactly, as JSON: the id, the cwd and any key the pinned
        # contract does not declare, which has no type to hold it to and so has to be the same.
        rest = [{key: value for key, value in one.items() if key != "runtimeWorkspaceRoots"}
                for one in (got, allowed)]
        if _canonical(rest[0]) != _canonical(rest[1]):
            return False
        if not _roots_within(got["runtimeWorkspaceRoots"], allowed["runtimeWorkspaceRoots"]):
            return False
    return True


def refusal_code(findings) -> str:
    """The code the transport classifies on: the first finding decides."""
    return findings[0]["code"] if findings else ""
