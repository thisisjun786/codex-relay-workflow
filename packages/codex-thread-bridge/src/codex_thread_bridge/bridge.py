"""Tool behavior, independent of MCP transport and the installed client."""

import asyncio
import copy
import re
from pathlib import Path

from .effects import recording
from .execution import EXCEPTION_ID_MAXIMUM, PRESENCE_ONLY
from .ledger import RETRYABLE_STATUSES, Ledger
from .rpc import AppServer, ResponseTooLarge, RpcError
from .settings import SettingsContract, annotation
from .worktrees import Worktree, WorktreeError


def nonempty(value: str, name: str, maximum: int = 100_000):
    if not isinstance(value, str) or not value.strip() or len(value) > maximum:
        raise ValueError(f"{name} must contain 1–{maximum} characters")


def absolute_directory(cwd: str):
    path = Path(cwd)
    if not path.is_absolute() or not path.is_dir():
        raise ValueError("cwd must be an existing absolute directory on the App Server host")
    return str(path.resolve())


def snapshot(value):
    """Freeze a caller-owned argument at the boundary, before anything can await.

    The ledger serialises params only after the mutation lock is acquired, while authorization
    and the settings contract read their own copy. Holding the caller's object in params meant a
    caller that mutated it during that wait could leave the recorded request identity describing
    a different request than the one that was authorized and dispatched: the real arguments would
    then be refused as different, and the mutated ones would replay a dispatch never made under
    them. One snapshot answers both questions.
    """
    return copy.deepcopy(value)


DISPLAY_FIELDS = frozenset({"text", "preview", "summary", "objective", "aggregatedOutput"})

# The turn statuses the host still owns. Anything else has finished and cannot be steered.
ACTIVE_TURN_STATUSES = frozenset({"inProgress"})

# How many of a page's newest turns get their items read. One, because "the latest turn" is what
# an observer is asking about, the rest of the page is already described by its summary, and an
# older turn's detail is reachable by paging with a cursor until it is the newest on its page.
# Every extra turn is another request and, on a real thread, another few megabytes.
DETAIL_TURNS = 1

# Items per detail request. Calibrated, and not a bound: the largest single item measured on a
# real thread was 1,179,797 bytes, so ten of them stay under the frame limit. Nothing caps one
# item, which is why a page that will not fit is asked again at one and then reported unobserved.
ITEM_PAGE = 10

# What the turns on a page actually contain, by how the page had to be asked for. Kept beside the
# statuses so the marker a caller reads can never drift from the request that produced it.
PAGE_ITEMS_VIEW = {
    "summary": "summary",
    "summary_narrowed": "summary",
    "not_loaded": "notLoaded",
    "not_observed": None,
}


def refusal(refused, **requested):
    """Everything an oversized frame permits us to say, and nothing it does not.

    Its size and our limit are facts. Whose response it was is not one: the frame is refused from
    its header, before any id inside it is read, and it may be a notification that never had one.
    So what is recorded here is the request that was pending, as what was asked — never as what
    overflowed.
    """
    return {
        "requested": requested,
        "frameBytes": refused.frame_bytes,
        "limit": refused.limit,
        "inFlight": list(refused.methods),
        "attribution": "unestablished",
        "note": "A response frame past this client's limit closed the connection while this "
        "request was pending. A frame is refused before its id is read, so it is not established "
        "that it was this request's response.",
    }


def page_note(status: str, detail_turns: int):
    """One sentence saying what this view of a thread is, and what it is not."""
    seen = {
        "not_observed": "No page of turns could be received at all, so only the thread's own "
        "metadata is here.",
        "not_loaded": "Turns are listed without any items, because no page carrying items would "
        "fit. Their ids, statuses and timestamps are real; none of their content is here.",
    }.get(
        status,
        "Turn items are the host's summary view: each turn's user and agent messages, not its "
        "tool calls or their output.",
    )
    if detail_turns == 1:
        detail = (
            " Item detail was read for the newest turn only, so every other turn on this page is "
            "marked not_requested, which is not a statement that it has no items."
        )
    elif detail_turns:
        detail = f" Item detail was read for the newest {detail_turns} turns only."
    else:
        detail = " No item detail was read."
    return (
        seen + detail + " This is a bounded observation of a thread rather than its whole "
        "history, and it says nothing about whether that thread finished, stalled, or has to be "
        "run again."
    )

# The host whose protocol this bridge's steer and pause paths were built against. Another server
# may well support them; this bridge does not probe for them, so it reports unknown rather than
# letting its own tool list stand in for a statement about that host.
TESTED_HOST_VERSION = "0.154.0"


def host_versions(user_agent: str):
    """The complete version tokens a user agent declares, as product/version pairs.

    Substring matching is not identification: "0.154.0" occurs inside "10.154.0", which would
    turn an unrecognised server into a tested one. Only a whole token counts.

    A token runs to its delimiter rather than stopping at the release numbers, so a prerelease
    or build-metadata suffix stays part of it. "0.154.0-alpha.1" is a different build from the
    tested release and has to read as one, since hostSupport is where the exposure-versus-support
    question is answered.
    """
    return set(re.findall(r"/(\d+(?:\.\d+)+[^\s()/;,]*)", user_agent or ""))


# Why a thread cannot be steered, named by the exact status the host reported. Collapsing these
# into one "not active" answer is how a system error gets handled as though it were an idle peer,
# and a correction then goes down a path that cannot carry it.
UNSTEERABLE_STATUS = {
    "idle": (
        "thread_idle",
        "Thread is idle; steer withheld. An idle thread takes send_message_to_thread.",
    ),
    "notLoaded": (
        "thread_not_loaded",
        "Thread is not loaded; steer withheld. Read it again before choosing a delivery path.",
    ),
    "systemError": (
        "thread_system_error",
        "Thread reports a system error; steer withheld and no delivery path is recommended.",
    ),
}

# The only keys send_message_to_thread's expected_settings accepts. Anything else is a caller
# mistake and is refused, because a discarded key looks identical to a setting never requested.
EXPECTED_SETTINGS_KEYS = frozenset(
    {
        "cwd",
        "sandbox",
        "expected_sandbox_policy",
        "model",
        "reasoning_effort",
        "runtime_workspace_roots",
    }
)


def validate_sandbox_policy(policy: dict):
    fields = {
        "readOnly": {"type", "networkAccess"},
        "dangerFullAccess": {"type"},
        "workspaceWrite": {
            "type",
            "networkAccess",
            "writableRoots",
            "excludeTmpdirEnvVar",
            "excludeSlashTmp",
        },
    }
    if set(policy) != fields.get(policy.get("type")):
        raise ValueError("Expected sandbox policy must contain all and only its protocol fields")
    for key in ("networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp"):
        if key in policy and type(policy[key]) is not bool:
            raise ValueError("Expected sandbox policy flags must be booleans")
    if "writableRoots" in policy and (
        not isinstance(policy["writableRoots"], list)
        or any(not isinstance(p, str) or not Path(p).is_absolute() for p in policy["writableRoots"])
    ):
        raise ValueError("Expected sandbox policy writableRoots must be absolute paths")


def clipped(value, limit: int, *, display_text: bool = False):
    """Bound display content while preserving opaque protocol fields verbatim."""
    if display_text and isinstance(value, str) and len(value) > limit:
        return value[:limit] + f"\n[truncated; original length {len(value)} characters]"
    if isinstance(value, list):
        return [clipped(item, limit, display_text=display_text) for item in value]
    if isinstance(value, dict):
        return {
            key: clipped(item, limit, display_text=key in DISPLAY_FIELDS)
            for key, item in value.items()
        }
    return value


def interrupted(effects):
    """The receipt fields for an operation that did not reach its own conclusion.

    outcome_unknown asks every later caller never to send this request again, and that demand is
    only honest when something actually went out. An operation that began nothing has nothing to
    reconcile and nothing to duplicate, so it says so plainly and leaves its request id able to
    carry a real attempt later. Which of the two this is comes from what was recorded as begun,
    never from where in the operation the failure happened.
    """
    attempted = list(effects.attempted)
    return {
        "status": "outcome_unknown" if attempted else "not_attempted",
        "retrySafe": not attempted,
        "attemptedEffects": attempted,
    }


class Bridge:
    def __init__(self, rpc: AppServer, ledger: Ledger, *, policy=None):
        self.rpc = rpc
        self.ledger = ledger
        # Keyword-only, and never mutated after construction. The allowlist reaches this object
        # from the server's own environment; no request can supply, widen or replace one.
        self.policy = policy or PRESENCE_ONLY
        self._mutation_lock = asyncio.Lock()

    async def _annotate_dispatch(self, receipt, contract):
        """Record what the thread reported after an accepted turn, without risking that turn.

        This runs only once _mutate has already persisted the acceptance, and never inside
        action(): a cancellation in there reaches _mutate's CancelledError handler, which would
        overwrite an acknowledged turn with outcome_unknown and leave a replay that never
        dispatches again. Here a failure, timeout, disconnect, malformed body or cancellation
        simply leaves the accepted receipt and its turnId exactly as they are.
        """
        if receipt.get("replayed"):
            # A retained receipt is answered from the ledger and nothing else. Reading the host
            # again would break that contract three ways: it makes a replay wait on a server that
            # may be offline, it re-observes state from long after the dispatch, and it would
            # overwrite the original annotation with that later observation.
            return receipt
        if receipt.get("status") != "accepted" or not receipt.get("turnId") or not contract:
            return receipt
        try:
            state = await self.rpc.call(
                "thread/read", {"threadId": receipt["threadId"], "includeTurns": False}
            )
            observed = (receipt.get("settings") or {}).get("actual") or {}
            note = annotation(observed, state["thread"])
        except asyncio.CancelledError:
            # Cancellation propagates. _mutate has already saved the acceptance durably, so
            # there is nothing here to protect by swallowing it, and swallowing it would let a
            # cancelled request or a shutdown return as though it had completed normally.
            raise
        except BaseException:  # noqa: BLE001 - a diagnostic must never endanger the dispatch
            return receipt
        return self.ledger.save({**receipt, "settingsAfterDispatch": note})

    async def capabilities(self):
        await self.rpc.connect()
        observed = self.rpc.info.get("userAgent", "")
        return {
            "server": self.rpc.info,
            "transport": "same-host Unix WebSocket",
            "socket": str(self.rpc.socket_path),
            "capabilities": {
                "createThread": True,
                "sendMessage": True,
                "listReadWait": True,
                "goalRead": True,
                "goalSet": False,
                "desktopManagedWorktrees": False,
                "bridgeManagedWorktrees": True,
                "desktopProjectRegistry": False,
                "clientSideToolsAndApprovals": False,
            },
            # Readable before creating anything, so a caller learns whether an allowlist is in
            # force instead of discovering it in a refusal or assuming one that does not exist.
            "executionPolicy": self.policy.summary(),
            # What THIS bridge offers. A tool missing here says nothing about the host: the
            # protocol has turn/interrupt and a turn queue, and this bridge withholds both.
            "exposure": {
                "steerActiveTurn": True,
                "goalPause": True,
                "goalObjectiveWrite": False,
                "turnInterrupt": False,
                "turnQueue": False,
                "note": "Tool exposure by this bridge version. Not a probe of the connected host.",
            },
            # What the HOST supports is a separate question, and this bridge answers it only for
            # the version it was built against. A different server leaves it unknown rather than
            # inheriting this tool list, and a -32601 from one method means that host lacks that
            # method, never that the capability is absent everywhere.
            "hostSupport": {
                "testedHost": f"codex-cli {TESTED_HOST_VERSION}",
                "observedServer": observed,
                "state": (
                    "tested"
                    if TESTED_HOST_VERSION in host_versions(observed)
                    else "unknown_host_version"
                ),
                "steerActiveTurn": "turn/steer, requires expectedTurnId",
                "goalPause": "thread/goal/set, status paused",
                "note": "Generated from the tested host's protocol. No live capability probe is "
                "performed, and an unrecognised server is reported unknown rather than assumed.",
            },
            "desktopVisibility": "Observed on Codex 0.153.4 with an existing project checkout; "
            "verify actual Desktop listing for each launch. Backend project IDs are separate.",
        }

    async def _mutate(
        self, request_id, method, params, action, *, validate_fresh, legacy_params=None,
        reconcile=None,
    ):
        async with self._mutation_lock:
            retained = self.ledger.lookup(request_id, method, params, legacy_params=legacy_params)
            # A retained receipt answers the request, with one exception: one recording that
            # nothing was begun answers nothing about the host, so the same id may try again
            # rather than being spent on a socket that was briefly gone. A known rejection is an
            # answer and keeps its id; only the absence of one is refundable.
            if retained is not None and retained.get("status") not in RETRYABLE_STATUSES:
                return {**retained, "replayed": True}
            # Required rather than optional, so a mutation added later cannot quietly dispatch
            # without being authorized. It runs after the replay lookup and before any ledger row
            # exists, which is what lets a refused request be corrected under the same id. A
            # retried request is authorized here again rather than inheriting the first attempt's
            # decision, because the policy may have changed since.
            validate_fresh()
            fresh, receipt = self.ledger.begin(
                request_id, method, params, legacy_params=legacy_params
            )
            if not fresh:
                return {**receipt, "replayed": True}
            with recording() as effects:

                def settled():
                    """Finish the receipt once its status and its evidence both exist.

                    An action may have to write a state before it can be known, because a crash
                    there would otherwise hide it. Only the action knows which field that was, so
                    correcting it belongs to the action rather than here; this passes it the
                    finished receipt and stays generic.
                    """
                    receipt["attemptedEffects"] = list(effects.attempted)
                    if reconcile is not None:
                        reconcile(receipt)
                    return receipt

                try:
                    await action(receipt)
                    receipt["status"] = "accepted"
                except RpcError as error:
                    receipt.update(status="failed", error=str(error), rpcError=error.error)
                except WorktreeError as error:
                    receipt.update(status="failed", error=str(error))
                except asyncio.CancelledError:
                    receipt.update(**interrupted(effects))
                    self.ledger.save(settled())
                    raise
                except Exception as error:
                    receipt.update(
                        **interrupted(effects), error=f"{type(error).__name__}: {error}"
                    )
                return self.ledger.save(settled())

    async def create_thread(
        self,
        request_id: str,
        cwd: str,
        prompt: str | None = None,
        title: str | None = None,
        sandbox: str = "read-only",
        model: str | None = None,
        app_server_project_id: str | None = None,
        reasoning_effort: str | None = None,
        runtime_workspace_roots: list[str] | None = None,
        expected_sandbox_policy: dict | None = None,
        policy_exception: str | None = None,
    ):
        nonempty(cwd, "cwd")
        if not Path(cwd).is_absolute():
            raise ValueError("cwd must be an existing absolute directory on the App Server host")
        if sandbox not in {"read-only", "workspace-write", "danger-full-access"}:
            raise ValueError("Unsupported sandbox")
        # model and reasoning_effort are deliberately absent here: the execution policy owns both
        # their presence and their shape, so an omitted one and a blank one get the same
        # structured refusal instead of one of them landing as a generic argument error.
        for name, value in [("prompt", prompt), ("title", title)]:
            if value is not None:
                nonempty(value, name, 100_000 if name == "prompt" else 500)
        # Snapshotted before the first await, so identity and behaviour cannot describe
        # different requests.
        expected_sandbox_policy = snapshot(expected_sandbox_policy)
        runtime_workspace_roots = snapshot(runtime_workspace_roots)
        if expected_sandbox_policy is not None:
            validate_sandbox_policy(expected_sandbox_policy)
        params = {"cwd": cwd, "sandbox": sandbox, "approvalPolicy": "never", "ephemeral": False}
        if model is not None:
            params["model"] = model
        if app_server_project_id is not None:
            nonempty(app_server_project_id, "app_server_project_id", 128)
            params["projectId"] = app_server_project_id
        # Appended only when supplied, so a caller that asks for nothing new keeps a
        # byte-identical fingerprint and its retained receipts still replay.
        if reasoning_effort is not None:
            params["reasoning_effort"] = reasoning_effort
        if runtime_workspace_roots is not None:
            params["runtime_workspace_roots"] = list(runtime_workspace_roots)
        if expected_sandbox_policy is not None:
            params["expected_sandbox_policy"] = expected_sandbox_policy
        if policy_exception is not None:
            nonempty(policy_exception, "policy_exception", EXCEPTION_ID_MAXIMUM)
            params["policy_exception"] = policy_exception
        request_params = {**params, "prompt": prompt, "title": title}
        built = {}

        def legacy_params():
            # Old receipts hashed a resolved cwd; only legacy lookups may use this form.
            return {**request_params, "cwd": str(Path(cwd).resolve())}

        def validate_fresh():
            # The fingerprint uses the supplied path, not mutable symlink resolution. Everything
            # else uses the resolved one: it is what the host is asked for and what an exception
            # is bound against, so a symlinked cwd cannot aim a request at a directory the
            # exception never covered.
            resolved = absolute_directory(cwd)
            execution = self.policy.authorize(
                model, reasoning_effort, cwd=resolved, exception=policy_exception
            )
            contract = SettingsContract(
                cwd=resolved,
                sandbox=sandbox,
                expected_sandbox_policy=expected_sandbox_policy,
                model=execution.model,
                reasoning_effort=execution.reasoning_effort,
                runtime_workspace_roots=runtime_workspace_roots,
            )
            launch = {
                "cwd": resolved,
                "sandbox": sandbox,
                "approvalPolicy": "never",
                "ephemeral": False,
                **contract.start_params(),
            }
            if app_server_project_id is not None:
                launch["projectId"] = app_server_project_id
            built.update(execution=execution, contract=contract, launch=launch)

        async def action(receipt):
            contract = built["contract"]
            # Recorded before the first call, because it is the decision that permitted the call.
            # No receipt exists while validate_fresh runs; this is the first moment there is one.
            receipt["executionPolicy"] = dict(built["execution"].receipt)
            self.ledger.save(receipt)
            if app_server_project_id is not None:
                await self.rpc.call("project/read", {"projectId": app_server_project_id})
            created = await self.rpc.call("thread/start", built["launch"])
            thread_id = created["thread"]["id"]
            receipt.update(threadId=thread_id, creation=created)
            self.ledger.save(receipt)  # Retain the ID even if naming or the first turn fails.
            # The contract that produced the launch parameters is the one that judges the answer,
            # so the comparison cannot be made against a different pair than the one transmitted.
            receipt["settings"] = contract.receipt(created, at="creation")
            self.ledger.save(receipt)
            findings = receipt["settings"]["findings"]
            if findings:
                first = findings[0]
                raise RpcError(
                    "thread/start",
                    {
                        "code": first["code"],
                        "message": f"{first['code']}: {first['field']} returned "
                        f"{first['returned']!r}, expected {first['expected']!r}; initial prompt "
                        "withheld. Inspect creation receipt. The thread remains retained.",
                    },
                )
            if title is not None:
                await self.rpc.call("thread/name/set", {"threadId": thread_id, "name": title})
                receipt["title"] = title
                self.ledger.save(receipt)
            if prompt is not None:
                turn = await self.rpc.call(
                    "turn/start",
                    {
                        "threadId": thread_id,
                        "input": [{"type": "text", "text": prompt}],
                    },
                )
                receipt["turnId"] = turn["turn"]["id"]
            receipt["desktopProjectAssociation"] = "unverified; check Desktop listing"

        receipt = await self._mutate(
            request_id,
            "create_thread",
            request_params,
            action,
            validate_fresh=validate_fresh,
            legacy_params=legacy_params,
        )
        return await self._annotate_dispatch(receipt, built.get("contract"))

    async def create_worktree_thread(
        self,
        request_id: str,
        source_repository: str,
        starting_revision: str,
        destination: str,
        worktree_mode: str,
        sandbox: str,
        expected_sandbox_policy: dict,
        prompt: str | None = None,
        title: str | None = None,
        model: str | None = None,
        reasoning_effort: str | None = None,
        app_server_project_id: str | None = None,
        policy_exception: str | None = None,
    ):
        if worktree_mode != "bridge-managed-retained":
            raise ValueError("Explicit bridge-managed-retained worktree ownership is required")
        # Snapshotted before it is validated, so the object that was checked is the object that
        # is transmitted, compared and fingerprinted.
        expected_sandbox_policy = snapshot(expected_sandbox_policy)
        validate_sandbox_policy(expected_sandbox_policy)
        sandbox_types = {
            "read-only": "readOnly",
            "workspace-write": "workspaceWrite",
            "danger-full-access": "dangerFullAccess",
        }
        if (
            sandbox not in sandbox_types
            or expected_sandbox_policy.get("type") != sandbox_types[sandbox]
        ):
            raise ValueError("sandbox and expected_sandbox_policy.type must agree")
        for name, value in [
            ("source_repository", source_repository),
            ("starting_revision", starting_revision),
            ("destination", destination),
            ("prompt", prompt),
            ("title", title),
            ("app_server_project_id", app_server_project_id),
        ]:
            if value is not None:
                nonempty(value, name)
        params = {
            "source_repository": source_repository,
            "starting_revision": starting_revision,
            "destination": destination,
            "worktree_mode": worktree_mode,
            "sandbox": sandbox,
            "expected_sandbox_policy": expected_sandbox_policy,
            "prompt": prompt,
            "title": title,
            "model": model,
            "reasoning_effort": reasoning_effort,
            "app_server_project_id": app_server_project_id,
        }
        # Only when supplied, so every retained receipt created before this argument existed keeps
        # its fingerprint and still replays.
        if policy_exception is not None:
            nonempty(policy_exception, "policy_exception", EXCEPTION_ID_MAXIMUM)
            params["policy_exception"] = policy_exception

        # Built inside validate_fresh, which _mutate runs only AFTER its ledger lookup. This tool
        # predates the transmittability check, so a receipt may be retained for a policy the check
        # now refuses, such as readOnly with networkAccess true. Constructing the contract out
        # here would raise before the lookup and make that receipt unrecoverable through the one
        # route this tool tells callers to use: reuse the stable request ID. New validation
        # applies to new requests, never to the recovery of an old one.
        built = {}

        def validate_fresh():
            # Bound against the destination string, which Worktree.validate already requires to be
            # canonical and which becomes the thread's cwd. Nothing exists on disk yet, and
            # nothing needs to: an exception names directories, not directories that exist.
            built["execution"] = self.policy.authorize(
                model, reasoning_effort, cwd=destination, exception=policy_exception
            )
            built["contract"] = SettingsContract(
                sandbox=sandbox,
                expected_sandbox_policy=expected_sandbox_policy,
                model=built["execution"].model,
                reasoning_effort=built["execution"].reasoning_effort,
            )

        async def action(receipt):
            contract, execution = built["contract"], built["execution"]

            def checkpoint(phase, **fields):
                receipt.update(phase=phase, **fields)
                self.ledger.save(receipt)

            checkpoint(
                "validating",
                executionPolicy=dict(execution.receipt),
                requestedCheckout=destination,
                initialPrompt={"state": "not_sent" if prompt is not None else "not_requested"},
                desktopProjectAssociation={
                    "status": "unverified",
                    "sourceRepository": source_repository,
                },
            )
            worktree = await Worktree.validate(source_repository, starting_revision, destination)
            if app_server_project_id is not None:
                await self.rpc.call("project/read", {"projectId": app_server_project_id})
            # The demand for recovery is recorded here rather than above, because everything
            # above only asks questions: a crash there leaves nothing on disk or on the host to
            # reconcile, and a receipt demanding recovery for it would send someone looking for
            # artifacts that were never made. From this line on the destination can exist.
            checkpoint(
                "reserving_destination",
                worktree=worktree.receipt(),
                recoveryRequired=True,
                recovery="Inspect this receipt, the destination and Git worktree list, and "
                "backend/Desktop tasks before manual recovery. Retain all artifacts; do not "
                "retry with a new request ID. Unknown thread/turn outcomes need reconciliation.",
            )
            worktree.reserve()
            receipt["worktree"]["state"] = "reserved"
            checkpoint("creating_worktree")
            await worktree.create()
            receipt["worktree"]["state"] = "registered"
            checkpoint("checking_out_worktree")
            await worktree.checkout()
            receipt["worktree"]["state"] = "created"
            checkpoint("checking_worktree")
            actual = await worktree.inspect()
            receipt["worktree"].update(actual)
            checkpoint("worktree_checked")
            if not worktree.matches(actual):
                raise WorktreeError("Worktree placement/base mismatch; initial prompt withheld")

            launch = {
                "cwd": str(worktree.destination),
                "sandbox": sandbox,
                "approvalPolicy": "never",
                "ephemeral": False,
                "runtimeWorkspaceRoots": [str(worktree.destination)],
            }
            launch["model"] = execution.model
            # Carries the effort AND every transmittable sandbox policy field; the mode string
            # alone cannot express writable roots or the network flag.
            config = contract.config()
            if config:
                launch["config"] = config
            if app_server_project_id is not None:
                launch["projectId"] = app_server_project_id
            checkpoint("creating_thread")
            created = await self.rpc.call("thread/start", launch)
            placed = SettingsContract(
                cwd=str(worktree.destination),
                sandbox=sandbox,
                expected_sandbox_policy=expected_sandbox_policy,
                model=execution.model,
                reasoning_effort=execution.reasoning_effort,
                runtime_workspace_roots=[str(worktree.destination)],
            )
            checkpoint(
                "checking_environment",
                threadId=created["thread"]["id"],
                creation=created,
                settings=placed.receipt(created, at="creation"),
                permissionReceipt={
                    key: created.get(key)
                    for key in (
                        "approvalPolicy",
                        "sandbox",
                        "activePermissionProfile",
                        "runtimeWorkspaceRoots",
                    )
                },
                desktopProjectAssociation={
                    "status": "unverified",
                    "sourceRepository": source_repository,
                    "checkout": created.get("cwd"),
                    "appServerProjectId": created["thread"].get("projectId"),
                },
            )
            findings = receipt["settings"]["findings"]
            if findings:
                first = findings[0]
                raise WorktreeError(
                    f"{first['code']}: {first['field']} returned {first['returned']!r}, expected "
                    f"{first['expected']!r}; initial prompt withheld. Inspect creation receipt"
                )
            # The worktree path owns two checks the shared contract does not: the thread's own
            # cwd, and the App Server project this checkout was meant to join.
            if created["thread"].get("cwd") != str(worktree.destination) or (
                app_server_project_id is not None
                and created["thread"].get("projectId") != app_server_project_id
            ):
                raise WorktreeError(
                    "Created thread placement differs; initial prompt withheld. "
                    "Inspect creation receipt"
                )
            if title is not None:
                checkpoint("naming_thread")
                await self.rpc.call(
                    "thread/name/set", {"threadId": receipt["threadId"], "name": title}
                )
                checkpoint("thread_named", title=title)
            # Thread startup and naming can take time; recheck placement just before dispatch.
            actual = await worktree.inspect()
            checkpoint("checking_before_dispatch", checkoutBeforeDispatch=actual)
            if not worktree.matches(actual):
                raise WorktreeError(
                    "Worktree changed during thread startup; initial prompt withheld"
                )
            if prompt is not None:
                checkpoint("dispatching_initial_prompt", initialPrompt={"state": "outcome_unknown"})
                turn = await self.rpc.call(
                    "turn/start",
                    {
                        "threadId": receipt["threadId"],
                        "input": [{"type": "text", "text": prompt}],
                    },
                )
                checkpoint(
                    "initial_prompt_accepted",
                    turnId=turn["turn"]["id"],
                    initialPrompt={"state": "accepted"},
                )
            checkpoint("complete", recoveryRequired=False)

        def reconcile(receipt):
            """Correct the prompt's state once the evidence says more than the guess did.

            The dispatching checkpoint writes outcome_unknown before the frame goes out, so that
            a process killed mid-dispatch cannot leave a receipt claiming the prompt was withheld.
            Once the operation ends, three different things can be true, and only one of them is
            the one that was written down in advance: the frame was never begun, the host
            answered and refused it, or it went out and the answer was lost.
            """
            if prompt is None or (receipt.get("initialPrompt") or {}).get("state") != (
                "outcome_unknown"
            ):
                return
            if "turn/start" not in receipt["attemptedEffects"]:
                receipt["initialPrompt"] = {"state": "not_sent"}
            elif receipt["status"] == "failed":
                receipt["initialPrompt"] = {"state": "rejected"}

        receipt = await self._mutate(
            request_id,
            "create_worktree_thread",
            params,
            action,
            validate_fresh=validate_fresh,
            reconcile=reconcile,
        )
        # Same diagnostic as the other two paths. It matters most here: this path checks its
        # settings at creation, then names the thread and re-inspects the checkout before
        # dispatching, so its window between observation and turn/start is the widest of the three.
        return await self._annotate_dispatch(receipt, built.get("contract"))

    async def send_message_to_thread(
        self,
        request_id: str,
        thread_id: str,
        message: str,
        expected_settings: dict | None = None,
        policy_exception: str | None = None,
    ):
        nonempty(thread_id, "thread_id", 128)
        nonempty(message, "message")
        if expected_settings is not None and not isinstance(expected_settings, dict):
            raise ValueError("expected_settings must be an object")
        supplied = snapshot(dict(expected_settings or {}))
        # An unrecognised key is refused, never ignored. The MCP schema admits any object, so a
        # caller who writes reasoningEffort instead of reasoning_effort would otherwise request
        # nothing at all: the resume would carry no effort, nothing would be compared, and the
        # message would go out under a "not_requested" receipt while the caller believed the
        # setting had been enforced. A silently discarded key is the exact failure this contract
        # exists to prevent.
        unknown = sorted(set(supplied) - EXPECTED_SETTINGS_KEYS)
        if unknown:
            raise ValueError(
                f"expected_settings has unknown keys {unknown}; supported keys are "
                f"{sorted(EXPECTED_SETTINGS_KEYS)}"
            )
        if supplied.get("expected_sandbox_policy") is not None:
            validate_sandbox_policy(supplied["expected_sandbox_policy"])
        params = {"threadId": thread_id, "message": message}
        # Only when supplied, so an existing caller's fingerprint is unchanged. When it IS
        # supplied it belongs to the request identity: retrying the same id with different
        # settings is a different request and the ledger must reject it.
        if expected_settings is not None:
            params["expected_settings"] = supplied
        if policy_exception is not None:
            nonempty(policy_exception, "policy_exception", EXCEPTION_ID_MAXIMUM)
            params["policy_exception"] = policy_exception
        built = {}

        def validate_fresh():
            # A turn on an existing thread costs exactly what a new one does, so the resume path
            # asks the same question as creation: which pair, and who approved it. Omitting
            # expected_settings entirely is refused here rather than resuming under whatever the
            # thread happens to carry.
            execution = self.policy.authorize(
                supplied.get("model"),
                supplied.get("reasoning_effort"),
                cwd=supplied.get("cwd"),
                exception=policy_exception,
            )
            built["execution"] = execution
            built["contract"] = SettingsContract(
                cwd=supplied.get("cwd"),
                sandbox=supplied.get("sandbox"),
                expected_sandbox_policy=supplied.get("expected_sandbox_policy"),
                model=execution.model,
                reasoning_effort=execution.reasoning_effort,
                runtime_workspace_roots=supplied.get("runtime_workspace_roots"),
            )

        async def action(receipt):
            contract = built["contract"]
            receipt["threadId"] = thread_id
            receipt["executionPolicy"] = dict(built["execution"].receipt)
            self.ledger.save(receipt)
            state = await self.rpc.call(
                "thread/read", {"threadId": thread_id, "includeTurns": False}
            )
            if state["thread"].get("status", {}).get("type") == "active":
                raise RpcError(
                    "thread/read",
                    {
                        "code": "thread_busy",
                        "message": "Thread is active; message withheld. This path starts a new "
                        "turn and an active thread already has one. To instruct the turn that "
                        "is running, read get_active_turn and call steer_thread with that turn "
                        "id. Waiting is for when there is nothing to say yet.",
                    },
                )
            # Resume is an explicit part of messaging, never part of discovery. It carries the
            # authorized settings and is then read as an OBSERVATION: this host reports a
            # thread's real state rather than adopting an override, which is exactly what
            # confirms the thread is already in the requested state. With nothing requested the
            # params stay {threadId, excludeTurns}, byte-identical to the original behaviour.
            resumed = await self.rpc.call("thread/resume", contract.resume_params(thread_id))
            receipt["resumed"] = resumed
            receipt["settings"] = contract.receipt(resumed, at="resume")
            self.ledger.save(receipt)
            findings = receipt["settings"]["findings"]
            if findings:
                first = findings[0]
                message_text = (
                    "Interactive approvals unsupported; message withheld. "
                    "Continue the thread in Desktop."
                    if first["code"] == "unsupported_approval_policy"
                    else f"{first['code']}: {first['field']} returned {first['returned']!r}, "
                    f"expected {first['expected']!r}; message withheld"
                )
                raise RpcError("thread/resume", {"code": first["code"], "message": message_text})
            # No turn/start overrides. TurnStartResponse reports only the turn, so a setting
            # bound here could never be read back; a clean resume already shows the thread is in
            # the requested state, which is the strongest claim this host supports.
            turn = await self.rpc.call(
                "turn/start",
                {
                    "threadId": thread_id,
                    "input": [{"type": "text", "text": message}],
                },
            )
            receipt["turnId"] = turn["turn"]["id"]

        receipt = await self._mutate(
            request_id, "send_message_to_thread", params, action, validate_fresh=validate_fresh
        )
        return await self._annotate_dispatch(receipt, built.get("contract"))

    async def get_goal(self, thread_id: str):
        nonempty(thread_id, "thread_id", 128)
        return clipped(await self.rpc.call("thread/goal/get", {"threadId": thread_id}), 4000)

    async def active_turn(self, thread_id: str):
        """Report the thread's status and the turn a steer could target, without resuming it.

        The protocol's active status carries activeFlags and no turn id, so the id turn/steer
        demands exists only as the newest turn the host still reports in progress. That makes this
        a derivation, and it is done in one place so every caller does not redo it — a caller
        reading a later history page would find a finished turn at the top and derive the wrong id.

        It is a snapshot. The turn can end immediately afterwards, which is exactly why steering
        takes the id as an explicit precondition instead of resolving it a second time itself.
        """
        nonempty(thread_id, "thread_id", 128)
        metadata = await self.rpc.call(
                "thread/read", {"threadId": thread_id, "includeTurns": False}
            )
        status = metadata["thread"].get("status") or {}
        page = await self.rpc.call(
            "thread/turns/list",
            {"threadId": thread_id, "limit": 1, "itemsView": "summary"},
        )
        data = page.get("data") or []
        newest = data[0] if data else None
        running = newest is not None and newest.get("status") in ACTIVE_TURN_STATUSES
        kind = status.get("type")
        if kind == "active" and running:
            observation = "active"
        elif kind == "active":
            # The status and the turn list disagree, so the turn ended between the two reads.
            observation = "active_without_in_progress_turn"
        elif running:
            observation = "in_progress_turn_without_active_status"
        else:
            observation = kind or "unknown"
        return clipped(
            {
                "threadId": thread_id,
                "status": status,
                "activeFlags": status.get("activeFlags", []),
                "activeTurnId": newest["id"] if running else None,
                "newestTurnId": newest["id"] if newest else None,
                "observation": observation,
                "derivation": "newest turn reported inProgress; the status carries no turn id",
                "steerable": observation == "active",
                "snapshot": "observed now; the turn may change before any steer is sent",
            },
            4000,
        )

    async def steer_thread(
        self, request_id: str, thread_id: str, expected_turn_id: str, message: str
    ):
        nonempty(thread_id, "thread_id", 128)
        nonempty(expected_turn_id, "expected_turn_id", 128)
        nonempty(message, "message")
        # Correlates this instruction with the item the host records, so a lost response is
        # settled by reading what the host kept rather than by sending the instruction again.
        client_message_id = f"steer:{request_id}"
        # expectedTurnId belongs to the request identity: the same id aimed at a different turn
        # is a different instruction, and the ledger must refuse it rather than replay.
        params = {"threadId": thread_id, "expectedTurnId": expected_turn_id, "message": message}

        async def action(receipt):
            receipt.update(
                threadId=thread_id,
                expectedTurnId=expected_turn_id,
                clientUserMessageId=client_message_id,
                # Nothing is resumed on this path, so no setting is observed. That is a different
                # statement from "nothing was requested", and the two must not read alike.
                settings={"verification": "not_observable", "reason": "steer performs no resume"},
            )
            self.ledger.save(receipt)
            state = await self.rpc.call(
                "thread/read", {"threadId": thread_id, "includeTurns": False}
            )
            kind = (state["thread"].get("status") or {}).get("type")
            if kind != "active":
                code, text = UNSTEERABLE_STATUS.get(
                    kind,
                    ("thread_not_steerable", f"Thread status {kind!r}; steer withheld."),
                )
                raise RpcError("thread/read", {"code": code, "message": text})
            # An active thread is not automatically a steerable one; the host owns that judgment
            # and its refusal is retained verbatim rather than being anticipated here.
            steered = await self.rpc.call(
                "turn/steer",
                {
                    "threadId": thread_id,
                    "expectedTurnId": expected_turn_id,
                    "clientUserMessageId": client_message_id,
                    "input": [{"type": "text", "text": message}],
                },
            )
            returned = steered.get("turnId")
            receipt["steeredTurnId"] = returned
            self.ledger.save(receipt)
            if returned != expected_turn_id:
                raise RpcError(
                    "turn/steer",
                    {
                        "code": "steered_turn_mismatch",
                        "message": f"turn/steer returned turn {returned!r}, not the guarded "
                        f"{expected_turn_id!r}. The instruction cannot be reported as delivered "
                        "to the observed turn; read the thread again and reclassify.",
                    },
                )
            receipt["delivery"] = "accepted_not_applied"
            receipt["deliveryMeaning"] = (
                "The host accepted this input into the guarded turn. It does not say the peer "
                "read it, and it does not say the peer acted on it."
            )

        # Nothing to authorize: a steer selects no model and starts no turn. It puts input into a
        # turn the host is already running, which some earlier creation already paid for and
        # already had its pair checked. Stated rather than omitted, because _mutate makes the
        # callback mandatory so that a new mutation has to answer this question out loud.
        return await self._mutate(
            request_id, "steer_thread", params, action, validate_fresh=lambda: None
        )

    async def pause_goal(self, request_id: str, thread_id: str):
        nonempty(thread_id, "thread_id", 128)
        # status is part of the request identity even though this tool sends only one value, so a
        # future status could never replay an earlier receipt.
        params = {"threadId": thread_id, "status": "paused"}

        async def action(receipt):
            receipt["threadId"] = thread_id
            self.ledger.save(receipt)
            before = (await self.rpc.call("thread/goal/get", {"threadId": thread_id})).get("goal")
            if before is None:
                raise RpcError(
                    "thread/goal/get",
                    {"code": "no_goal", "message": "Thread has no goal to pause."},
                )
            receipt["goalBefore"] = clipped(before, 4000)
            self.ledger.save(receipt)
            status = before.get("status")
            if status == "paused":
                receipt.update(
                    pause="already_paused",
                    delivery="no_change",
                    goalAfter=clipped(before, 4000),
                )
                return
            if status != "active":
                raise RpcError(
                    "thread/goal/get",
                    {
                        "code": "goal_not_active",
                        "message": f"Goal status is {status!r}; pause withheld. Only an active "
                        "goal is paused, so a goal that already ended is never overwritten.",
                    },
                )
            # Status only. No objective and no tokenBudget are sent, so this tool cannot rewrite
            # an objective even by accident, and cannot restore a stale one it read moments ago.
            response = await self.rpc.call(
                "thread/goal/set", {"threadId": thread_id, "status": "paused"}
            )
            after = response["goal"]
            receipt["goalAfter"] = clipped(after, 4000)
            # The protocol offers no expected-status or revision precondition on goal/set, so a
            # goal that turned terminal between the read above and this call could be overwritten.
            # Reading first narrows that window; nothing available here closes it.
            receipt["concurrency"] = "no_host_precondition_for_goal_status"
            receipt["concurrencyMeaning"] = (
                "thread/goal/set takes no expected status, so this pause is not atomic. The "
                "refusals above are judged on the status read a moment earlier: a goal that "
                "ended in between could still have been overwritten by this write, and the "
                "goal returned cannot show whether it did. Read the goal again afterwards "
                "rather than trusting this receipt as exclusive."
            )
            self.ledger.save(receipt)
            moved = [
                field
                for field in ("objective", "tokenBudget")
                if after.get(field) != before.get(field)
            ]
            if moved:
                raise RpcError(
                    "thread/goal/set",
                    {
                        "code": "goal_changed_under_pause",
                        "message": f"The host returned a different {', '.join(moved)} than the "
                        "goal read moments earlier. The pause is not reported as clean; inspect "
                        "goalBefore and goalAfter on this receipt.",
                    },
                )
            if after.get("status") != "paused":
                raise RpcError(
                    "thread/goal/set",
                    {
                        "code": "goal_not_paused",
                        "message": f"The host reported status {after.get('status')!r} after the "
                        "pause request; it is not recorded as applied.",
                    },
                )
            receipt["delivery"] = "applied_by_host"
            receipt["pause"] = "goal_paused_turn_may_still_be_running"
            receipt["pauseMeaning"] = (
                "The goal is paused. A turn already running is not stopped by this call: steer "
                "the observed turn to finish safely, and keep the two claims separate."
            )

        # Nothing to authorize, for the same reason: a pause changes goal status and selects no
        # model. It starts no turn and does not stop one.
        return await self._mutate(
            request_id, "pause_goal", params, action, validate_fresh=lambda: None
        )

    async def list_threads(self, cwd=None, limit=20, cursor=None):
        if not 1 <= limit <= 100:
            raise ValueError("limit must be between 1 and 100")
        params = {"limit": limit, "useStateDbOnly": True}
        if cwd is not None:
            params["cwd"] = absolute_directory(cwd)
        if cursor is not None:
            params["cursor"] = cursor
        return clipped(await self.rpc.call("thread/list", params), 4000)

    async def read_thread(self, thread_id: str, limit=10, cursor=None, max_text_chars=4000):
        nonempty(thread_id, "thread_id", 128)
        if not 1 <= limit <= 100 or not 100 <= max_text_chars <= 20_000:
            raise ValueError("limit must be 1–100 and max_text_chars must be 100–20000")
        # Stated rather than inherited. The host already leaves turns out of this answer, and it
        # is the one call whose size is reliably small — 1.2 KB on the thread that could not be
        # read at all — which is what lets every later failure still return a thread instead of
        # nothing. A failure here does propagate: then nothing was established.
        metadata = await self.rpc.call(
            "thread/read", {"threadId": thread_id, "includeTurns": False}
        )
        page, status, attempts = await self._turns_page(thread_id, limit, cursor)
        turns = (page or {}).get("data") or []
        for position, turn in enumerate(turns):
            if isinstance(turn, dict):
                await self._read_items(thread_id, turn, position)
        detail_turns = min(DETAIL_TURNS, len(turns))
        observation = {
            "turnsPageStatus": status,
            "itemsView": PAGE_ITEMS_VIEW[status],
            "detailTurns": detail_turns,
            "note": page_note(status, detail_turns),
        }
        if attempts:
            observation["pageAttempts"] = attempts
        return clipped(
            {"thread": metadata["thread"], "turnsPage": page, "observation": observation},
            max_text_chars,
        )

    async def _turns_page(self, thread_id: str, limit: int, cursor):
        """The cheapest page of turns this connection will carry, and what it cost to get one.

        Never the full view. That view is what made a long thread unreadable: one page of ten
        turns measured 754 MB, and the host spends the 96 seconds building it whether or not the
        client accepts a byte, so asking for it and recovering afterwards charges that every time.
        Each rung down is tried only after an oversized frame closed the connection while the one
        above it was pending, and the last rung asks for turns with no items at all, which for
        every turn of that same thread was 3,148 bytes.
        """
        rungs = [("summary", limit, "summary")]
        if limit > 1:
            rungs.append(("summary", 1, "summary_narrowed"))
        rungs.append(("notLoaded", limit, "not_loaded"))
        attempts = []
        for view, size, status in rungs:
            params = {"threadId": thread_id, "limit": size, "itemsView": view}
            if cursor is not None:
                params["cursor"] = cursor
            try:
                return await self.rpc.call("thread/turns/list", params), status, attempts
            except ResponseTooLarge as refused:
                attempts.append(refusal(refused, itemsView=view, limit=size))
        return None, "not_observed", attempts

    async def _read_items(self, thread_id: str, turn: dict, position: int):
        """Read one turn's items within a bound, and say exactly what was and was not seen.

        Nothing here can fail the read. An item page that will not arrive is an observation this
        bridge did not get, and a caller deciding whether a task finished, stalled or needs
        running again must not be handed that as though it were news about the task.
        """
        turn_id = turn.get("id")
        if position >= DETAIL_TURNS or not turn_id:
            turn["itemsDetail"] = None
            turn["itemsDetailStatus"] = "not_requested"
            return
        attempts = []
        for size in dict.fromkeys((ITEM_PAGE, 1)):
            try:
                items = await self.rpc.call(
                    "thread/items/list",
                    {"threadId": thread_id, "turnId": turn_id, "limit": size},
                )
            except ResponseTooLarge as refused:
                attempts.append(refusal(refused, method="thread/items/list", limit=size))
                continue
            except RpcError as error:
                code = error.error.get("code")
                turn["itemsDetail"] = None
                # -32601 is this host lacking the method, which is a fact about that host and
                # never about the capability existing anywhere else. Any other refusal is the
                # host's own and is repeated rather than reinterpreted.
                turn["itemsDetailStatus"] = "method_unavailable" if code == -32601 else "refused"
                turn["itemsDetailNote"] = {
                    "code": code,
                    "message": error.error.get("message"),
                    "note": "The host refused the item read. This turn's summary items are "
                    "unaffected, and none of this is a statement about the thread.",
                }
                return
            data = items.get("data") or []
            more = bool(items.get("nextCursor"))
            note = {}
            if attempts:
                turn["itemsDetailStatus"] = "narrowed"
                note = {"requestedLimit": ITEM_PAGE, "observedLimit": size, "attempts": attempts}
            else:
                turn["itemsDetailStatus"] = "partial" if more else "complete"
            if more:
                note["observed"] = len(data)
                note["more"] = True
                note["note"] = (
                    "This turn has more items than were read. read_thread pages turns rather "
                    "than items, so the rest cannot be reached through this tool."
                )
            turn["itemsDetail"] = data
            if note:
                turn["itemsDetailNote"] = note
            return
        turn["itemsDetail"] = None
        turn["itemsDetailStatus"] = "not_observed"
        turn["itemsDetailNote"] = {
            "attempts": attempts,
            "note": "This turn's items would not arrive even one at a time, and there is no "
            "query narrower than one item. Its summary items are all of it that can be seen "
            "here. That is a limit on observation, not a fact about the thread.",
        }

    async def wait_thread(self, thread_id: str, turn_id: str, timeout_seconds=20):
        nonempty(thread_id, "thread_id", 128)
        nonempty(turn_id, "turn_id", 128)
        if not 0 <= timeout_seconds <= 50:
            raise ValueError("timeout_seconds must be between 0 and 50")
        latest = None

        async def inspect():
            # Search recent turns only; never confuse a different completed turn with the target.
            page = await self.rpc.call(
                "thread/turns/list",
                {
                    "threadId": thread_id,
                    "limit": 100,
                    "itemsView": "summary",
                },
            )
            return next((turn for turn in page["data"] if turn["id"] == turn_id), None)

        if timeout_seconds == 0:
            latest = await inspect()
        else:
            try:
                async with asyncio.timeout(timeout_seconds):
                    while True:
                        latest = await inspect()
                        if latest and latest.get("status") in {
                            "completed",
                            "failed",
                            "interrupted",
                        }:
                            break
                        await asyncio.sleep(0.5)
            except TimeoutError:
                pass
        terminal = latest is not None and latest.get("status") in {
            "completed",
            "failed",
            "interrupted",
        }
        return clipped(
            {
                "threadId": thread_id,
                "turnId": turn_id,
                "timedOut": not terminal,
                "turn": latest,
                "observation": "found" if latest else "not observed in latest 100 turns",
            },
            4000,
        )
