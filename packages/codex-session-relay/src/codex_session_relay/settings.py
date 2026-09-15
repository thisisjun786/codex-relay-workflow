"""The execution settings a send must preserve, and how they are carried and checked.

This module exists because of one observed host behaviour: a thread/resume carrying only
{threadId, excludeTurns} returned sandbox dangerFullAccess for a task created with workspaceWrite
and networkAccess false. The pinned bridge sends exactly that resume and says so in its own
comment, so an adapter built on it cannot claim to preserve permissions.

The two calls have different jobs and neither replaces the other:

    thread/resume   DETECTS. Its params accept a sandbox MODE string, approvalPolicy, cwd,
                    runtimeWorkspaceRoots and model - but no effort and no environments. Its
                    RESPONSE reports the full derived policy, the effort and the environments,
                    which is what makes a host that ignores overrides visible before anything
                    starts.
    turn/start      BINDS. It is the only call that accepts the full SandboxPolicy object plus
                    effort plus environments, scoped to this turn and subsequent turns.

There is no verification after the start: TurnStartResponse defines only turn, and Turn carries
id, items, status, startedAt, completedAt, durationMs, error and itemsView. That absence is a
reported limit, not something to simulate, and it never withholds a send by itself.
"""

from .errors import DeliveryRefused, RefusalReason

# ThreadResumeParams.sandbox is a SandboxMode enum; TurnStartParams.sandboxPolicy is the object.
RESUME_SANDBOX_MODE = {
    "workspaceWrite": "workspace-write",
    "readOnly": "read-only",
    "dangerFullAccess": "danger-full-access",
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

SETTINGS_UNAVAILABLE = "settings_unavailable"
SETTINGS_INCOMPLETE = "settings_incomplete"
SETTINGS_NOT_PRESERVED = "settings_not_preserved"
ENVIRONMENTS_UNKNOWN = "environments_unknown"
UNVERIFIABLE_PERMISSION_PROFILE = "unverifiable_permission_profile"
UNSUPPORTED_SANDBOX_TYPE = "unsupported_sandbox_type"
UNSUPPORTED_APPROVAL_POLICY = "unsupported_approval_policy"


def normalise_policy(policy):
    """Fill the declared defaults so an omitted field compares equal to its default."""
    if not isinstance(policy, dict) or "type" not in policy:
        return None
    kind = policy["type"]
    merged = dict(POLICY_DEFAULTS.get(kind, {}))
    merged.update({k: v for k, v in policy.items() if k != "type"})
    merged["type"] = kind
    if "writableRoots" in merged:
        merged["writableRoots"] = list(merged["writableRoots"])
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
    """Complete or unusable. A partial record cannot say what it is preserving.

    JUN-92 populates this from the creation result Run already receives; it is not a separate
    handshake and it asks for nothing the host did not already report at creation.
    """

    def __init__(self, data: dict):
        self.data = dict(data or {})

    # ------------------------------------------------------------- validity

    def missing(self) -> list:
        absent = [field for field in REQUIRED if self.data.get(field) is None]
        # environments may legitimately be an empty list (no environments selected), which is a
        # decision, unlike None which is an absence. Re-admit that case.
        if "environments" in absent and isinstance(self.data.get("environments"), list):
            absent.remove("environments")
        return absent

    def require_usable(self) -> None:
        absent = self.missing()
        if absent:
            raise DeliveryRefused(
                RefusalReason.SETTINGS_INCOMPLETE,
                f"missing {', '.join(absent)}",
            )
        if self.sandbox_mode() is None:
            raise DeliveryRefused(
                RefusalReason.UNSUPPORTED_SANDBOX_TYPE,
                f"{self.data['sandbox'].get('type')!r} has no"
                " ThreadResumeParams.sandbox mode, so it cannot be restored on a resume",
            )

    def sandbox_mode(self):
        policy = self.data.get("sandbox") or {}
        return RESUME_SANDBOX_MODE.get(policy.get("type"))

    # --------------------------------------------------------------- params

    def resume_params(self, thread_id: str) -> dict:
        """Only fields ThreadResumeParams actually defines. No effort field, no environments.

        Effort has no dedicated resume parameter, but the schema does accept a free-form config
        object, so the effort is carried there as model_reasoning_effort. That makes the resume
        response a meaningful check on effort too, instead of one we can read but never set. The
        authoritative binding is still TurnStartParams.effort.
        """
        return {
            "threadId": thread_id,
            "excludeTurns": True,
            "sandbox": self.sandbox_mode(),
            "approvalPolicy": self.data["approvalPolicy"],
            "cwd": self.data["cwd"],
            "runtimeWorkspaceRoots": list(self.data["runtimeWorkspaceRoots"]),
            "model": self.data["model"],
            "config": {"model_reasoning_effort": self.data["reasoningEffort"]},
        }

    def start_overrides(self) -> dict:
        """The override fields only. The caller composes threadId and input, which are required."""
        return {
            "sandboxPolicy": normalise_policy(self.data["sandbox"]),
            "approvalPolicy": self.data["approvalPolicy"],
            "cwd": self.data["cwd"],
            "runtimeWorkspaceRoots": list(self.data["runtimeWorkspaceRoots"]),
            "model": self.data["model"],
            "effort": self.data["reasoningEffort"],
            "environments": [
                {"environmentId": e["environmentId"], "cwd": e["cwd"],
                 "runtimeWorkspaceRoots": list(e["runtimeWorkspaceRoots"])}
                for e in normalise_environments(self.data["environments"])
            ],
        }

    # ------------------------------------------------------------ verifying

    def mismatches(self, response: dict) -> list:
        """Ordered findings against a resume response. Order is behaviour, not presentation.

        The approval policy is checked FIRST. With an authorized policy of never, a returned
        on-request is both a mismatch and the push-channel-closed case, and raising the generic
        mismatch first would turn a permanently closed channel into a retry loop.
        """
        found = []
        returned_policy = response.get("approvalPolicy")
        if returned_policy != "never":
            # A granular policy is an object; it is never copied into the frozen record, which
            # types this field as string or null. Contract v1 admits only a plain string.
            label = returned_policy if isinstance(returned_policy, str) else "granular"
            found.append({"code": UNSUPPORTED_APPROVAL_POLICY, "field": "approvalPolicy",
                          "expected": "never", "returned": label,
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

        comparisons = (
            ("sandbox", normalise_policy(self.data["sandbox"]),
             normalise_policy(response.get("sandbox"))),
            ("cwd", self.data["cwd"], response.get("cwd")),
            ("runtimeWorkspaceRoots", list(self.data["runtimeWorkspaceRoots"]),
             list(response.get("runtimeWorkspaceRoots") or [])),
            ("model", self.data["model"], response.get("model")),
            ("reasoningEffort", self.data["reasoningEffort"], response.get("reasoningEffort")),
        )
        for field, expected, returned in comparisons:
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
