"""Tool behavior, independent of MCP transport and the installed client."""

import asyncio
from pathlib import Path

from .ledger import Ledger
from .rpc import AppServer, RpcError
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


DISPLAY_FIELDS = frozenset({"text", "preview", "summary", "objective", "aggregatedOutput"})

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


class Bridge:
    def __init__(self, rpc: AppServer, ledger: Ledger):
        self.rpc = rpc
        self.ledger = ledger
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
            state = await self.rpc.call("thread/read", {"threadId": receipt["threadId"]})
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
            "desktopVisibility": "Observed on Codex 0.153.4 with an existing project checkout; "
            "verify actual Desktop listing for each launch. Backend project IDs are separate.",
        }

    async def _mutate(
        self, request_id, method, params, action, *, validate_fresh=None, legacy_params=None
    ):
        async with self._mutation_lock:
            retained = self.ledger.lookup(request_id, method, params, legacy_params=legacy_params)
            if retained is not None:
                return {**retained, "replayed": True}
            if validate_fresh is not None:
                validate_fresh()
            fresh, receipt = self.ledger.begin(
                request_id, method, params, legacy_params=legacy_params
            )
            if not fresh:
                return {**receipt, "replayed": True}
            try:
                await action(receipt)
                receipt["status"] = "accepted"
            except RpcError as error:
                receipt.update(status="failed", error=str(error), rpcError=error.error)
            except WorktreeError as error:
                receipt.update(status="failed", error=str(error))
            except asyncio.CancelledError:
                self.ledger.save({**receipt, "status": "outcome_unknown"})
                raise
            except Exception as error:
                receipt.update(status="outcome_unknown", error=f"{type(error).__name__}: {error}")
            return self.ledger.save(receipt)

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
    ):
        nonempty(cwd, "cwd")
        if not Path(cwd).is_absolute():
            raise ValueError("cwd must be an existing absolute directory on the App Server host")
        if sandbox not in {"read-only", "workspace-write", "danger-full-access"}:
            raise ValueError("Unsupported sandbox")
        for name, value in [
            ("prompt", prompt),
            ("title", title),
            ("model", model),
            ("reasoning_effort", reasoning_effort),
        ]:
            if value is not None:
                nonempty(value, name, 100_000 if name == "prompt" else 500)
        if expected_sandbox_policy is not None:
            validate_sandbox_policy(expected_sandbox_policy)
        contract = SettingsContract(
            cwd=cwd,
            sandbox=sandbox,
            expected_sandbox_policy=expected_sandbox_policy,
            model=model,
            reasoning_effort=reasoning_effort,
            runtime_workspace_roots=runtime_workspace_roots,
        )
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

        launch_params = dict(params)
        for key in ("reasoning_effort", "runtime_workspace_roots", "expected_sandbox_policy"):
            launch_params.pop(key, None)
        launch_params.update(contract.start_params())
        launch_params["cwd"] = cwd
        launch_params["sandbox"] = sandbox
        request_params = {**params, "prompt": prompt, "title": title}

        def legacy_params():
            # Old receipts hashed a resolved cwd; only legacy lookups may use this form.
            return {**request_params, "cwd": str(Path(cwd).resolve())}

        def validate_fresh():
            # The fingerprint uses the supplied path, not mutable symlink resolution.
            launch_params["cwd"] = absolute_directory(cwd)

        async def action(receipt):
            if app_server_project_id is not None:
                await self.rpc.call("project/read", {"projectId": app_server_project_id})
            created = await self.rpc.call("thread/start", launch_params)
            thread_id = created["thread"]["id"]
            receipt.update(threadId=thread_id, creation=created)
            self.ledger.save(receipt)  # Retain the ID even if naming or the first turn fails.
            checked = SettingsContract(
                cwd=launch_params["cwd"],
                sandbox=sandbox,
                expected_sandbox_policy=expected_sandbox_policy,
                model=model,
                reasoning_effort=reasoning_effort,
                runtime_workspace_roots=runtime_workspace_roots,
            )
            receipt["settings"] = checked.receipt(created, at="creation")
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
        return await self._annotate_dispatch(receipt, contract)

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
    ):
        if worktree_mode != "bridge-managed-retained":
            raise ValueError("Explicit bridge-managed-retained worktree ownership is required")
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
            ("model", model),
            ("reasoning_effort", reasoning_effort),
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

        contract = SettingsContract(
            sandbox=sandbox,
            expected_sandbox_policy=expected_sandbox_policy,
            model=model,
            reasoning_effort=reasoning_effort,
        )

        async def action(receipt):
            def checkpoint(phase, **fields):
                receipt.update(phase=phase, **fields)
                self.ledger.save(receipt)

            checkpoint(
                "validating",
                recoveryRequired=True,
                recovery="Inspect this receipt, the destination and Git worktree list, and "
                "backend/Desktop tasks before manual recovery. Retain all artifacts; do not "
                "retry with a new request ID. Unknown thread/turn outcomes need reconciliation.",
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
            checkpoint("reserving_destination", worktree=worktree.receipt())
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
            if model is not None:
                launch["model"] = model
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
                model=model,
                reasoning_effort=reasoning_effort,
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

        receipt = await self._mutate(request_id, "create_worktree_thread", params, action)
        # Same diagnostic as the other two paths. It matters most here: this path checks its
        # settings at creation, then names the thread and re-inspects the checkout before
        # dispatching, so its window between observation and turn/start is the widest of the three.
        return await self._annotate_dispatch(receipt, contract)

    async def send_message_to_thread(
        self,
        request_id: str,
        thread_id: str,
        message: str,
        expected_settings: dict | None = None,
    ):
        nonempty(thread_id, "thread_id", 128)
        nonempty(message, "message")
        if expected_settings is not None and not isinstance(expected_settings, dict):
            raise ValueError("expected_settings must be an object")
        supplied = dict(expected_settings or {})
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
        contract = SettingsContract(
            cwd=supplied.get("cwd"),
            sandbox=supplied.get("sandbox"),
            expected_sandbox_policy=supplied.get("expected_sandbox_policy"),
            model=supplied.get("model"),
            reasoning_effort=supplied.get("reasoning_effort"),
            runtime_workspace_roots=supplied.get("runtime_workspace_roots"),
        )
        params = {"threadId": thread_id, "message": message}
        # Only when supplied, so an existing caller's fingerprint is unchanged. When it IS
        # supplied it belongs to the request identity: retrying the same id with different
        # settings is a different request and the ledger must reject it.
        if expected_settings is not None:
            params["expected_settings"] = expected_settings

        async def action(receipt):
            receipt["threadId"] = thread_id
            self.ledger.save(receipt)
            state = await self.rpc.call("thread/read", {"threadId": thread_id})
            if state["thread"].get("status", {}).get("type") == "active":
                raise RpcError(
                    "thread/read",
                    {
                        "code": "thread_busy",
                        "message": "Thread is active; message withheld. Wait for completion.",
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

        receipt = await self._mutate(request_id, "send_message_to_thread", params, action)
        return await self._annotate_dispatch(receipt, contract)

    async def get_goal(self, thread_id: str):
        nonempty(thread_id, "thread_id", 128)
        return clipped(await self.rpc.call("thread/goal/get", {"threadId": thread_id}), 4000)

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
        metadata = await self.rpc.call("thread/read", {"threadId": thread_id})
        params = {"threadId": thread_id, "limit": limit, "itemsView": "full"}
        if cursor is not None:
            params["cursor"] = cursor
        page = await self.rpc.call("thread/turns/list", params)
        return clipped({"thread": metadata["thread"], "turnsPage": page}, max_text_chars)

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
