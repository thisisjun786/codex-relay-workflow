"""One recoverable managed start, over the existing registry and bridge ledger.

Admission delivers an assignment. It never claims the child's consent, a hook firing,
review acceptance, or completion. The operation ids survive a caller crash; uncertain
host effects retain their reservation rather than authorizing another child.
"""

import hashlib
import json
from contextlib import closing
from pathlib import Path

from . import marker
from .criteria import finding_id
from .settings import AUTHORIZED_APPROVAL_POLICY, REQUIRED, TaskSettings

SCHEMA = "managed-start/1"
BOOTSTRAP_VERSION = 1
BOOTSTRAP = (
    "This is a managed-start standby turn, not an implementation assignment. "
    "Do not use tools, change files, initialize a workflow or create a goal. "
    "End this turn now. The registered assignment will arrive in a separate turn."
)
MAX_REQUEST_BYTES = 256_000
MAX_PROMPT = 90_000
MAX_HOST_CHECK_PAGES = 4
HOST_READY_RPC_REQUESTS = 2 * MAX_HOST_CHECK_PAGES + 2


def _object(value, required, optional=(), *, at):
    if not isinstance(value, dict):
        raise ValueError(f"{at} must be an object")
    missing, extra = set(required) - value.keys(), value.keys() - set(required) - set(optional)
    if missing or extra:
        raise ValueError(f"{at}: missing {sorted(missing)}, unknown {sorted(extra)}")
    return value


def _text(value, at, limit=4096):
    if not isinstance(value, str) or not value.strip() or len(value) > limit or "\0" in value:
        raise ValueError(f"{at} must be nonblank text of at most {limit} characters without NUL")
    return value


def _path(value, at, *, directory=True):
    _text(value, at)
    path = Path(value)
    if not path.is_absolute():
        raise ValueError(f"{at} must be absolute")
    if directory and not path.is_dir():
        raise ValueError(f"{at} must name an existing directory")
    return str(path.resolve())


def _strings(value, at):
    if not isinstance(value, list) or not value or len(value) > 256:
        raise ValueError(f"{at} must be a nonempty list of at most 256 entries")
    for item in value:
        _text(item, at)
    if len(set(value)) != len(value):
        raise ValueError(f"{at} contains duplicates")
    return value


def _settings(data, at):
    from codex_thread_bridge.settings import SettingsContract, POLICY_DEFAULTS

    _object(data, REQUIRED, ("expectedPermissionProfile",), at=at)
    for field in ("model", "reasoningEffort"):
        _text(data[field], f"{at}.{field}", 500)
    if data["approvalPolicy"] != AUTHORIZED_APPROVAL_POLICY:
        # A task this relay CREATES runs under never. That is a rule about what the relay
        # creates, not about whom it can wake: an existing supervisor or parent on on-request is
        # carried (settings.CARRIED_APPROVAL_POLICIES, CRW-225), because its approvals stay with
        # its own approver. Refused before creating any task, so the message names the REQUEST
        # field and nothing is created on the way to it. The literal is read from one definition
        # rather than spelled a second time here, which is how the two could have drifted apart.
        raise ValueError(
            f"{at}.approvalPolicy must explicitly be {AUTHORIZED_APPROVAL_POLICY}"
            " for this transport"
        )
    _path(data["cwd"], f"{at}.cwd")
    roots = _strings(data["runtimeWorkspaceRoots"], f"{at}.runtimeWorkspaceRoots")
    for root in roots:
        _path(root, f"{at}.runtimeWorkspaceRoots")
    policy = data["sandbox"]
    if not isinstance(policy, dict) or policy.get("type") not in (
        "readOnly", "workspaceWrite", "dangerFullAccess"
    ):
        raise ValueError(f"{at}.sandbox must carry a supported type")
    kind = policy["type"]
    _object(policy, ("type",), POLICY_DEFAULTS[kind], at=f"{at}.sandbox")
    for key, value in policy.items():
        if key == "type":
            continue
        if key == "writableRoots":
            if not isinstance(value, list) or len(value) > 256:
                raise ValueError(f"{at}.sandbox.writableRoots must be a list")
            for root in value:
                _path(root, f"{at}.sandbox.writableRoots")
        elif type(value) is not bool:
            raise ValueError(f"{at}.sandbox.{key} must be a boolean")
    environments = data["environments"]
    if not isinstance(environments, list) or len(environments) > 1:
        raise ValueError(f"{at}.environments must declare empty or one local environment")
    for environment in environments:
        _object(environment, ("environmentId", "cwd", "runtimeWorkspaceRoots"), at=at)
        if environment["environmentId"] != "local":
            raise ValueError(f"{at}: selecting remote environments is unsupported")
        if environment["cwd"] != data["cwd"] or environment["runtimeWorkspaceRoots"] != roots:
            raise ValueError(f"{at}: local environment must match the declared cwd and roots")
    if data.get("expectedPermissionProfile") is not None:
        _text(data["expectedPermissionProfile"], f"{at}.expectedPermissionProfile", 500)
    settings = TaskSettings(data)
    settings.require_usable()
    # Reuse the bridge's validator before a reservation can become armed. In
    # particular, readOnly network overrides cannot be transmitted by this bridge.
    contract = SettingsContract(
        cwd=data["cwd"], sandbox=settings.sandbox_mode(),
        expected_sandbox_policy=policy, model=data["model"],
        reasoning_effort=data["reasoningEffort"], runtime_workspace_roots=roots,
    )
    contract.start_params()
    return settings


def parse_request(raw):
    """Snapshot and validate the complete request before any persistent/host effect."""
    encoded = json.dumps(raw, ensure_ascii=False, allow_nan=False).encode("utf-8")
    if len(encoded) > MAX_REQUEST_BYTES:
        raise ValueError("managed request exceeds the byte limit")
    request = json.loads(encoded)
    _object(request, (
        "schema", "requestId", "issueKey", "parent", "child", "artifactRoots",
        "allowedRecipients", "criteria", "criteriaSource", "baselineRevision", "scopeRef", "prompt",
    ), ("projectKey",), at="request")
    if request["schema"] != SCHEMA:
        raise ValueError("unknown managed request schema")
    for key in ("requestId", "issueKey", "criteriaSource", "baselineRevision", "scopeRef"):
        _text(request[key], key, 128 if key == "requestId" else 4096)
    _text(request["prompt"], "prompt", MAX_PROMPT)
    if "projectKey" in request:
        _text(request["projectKey"], "projectKey")
    parent = _object(request["parent"], ("taskId", "hostId", "settings"), at="parent")
    child = _object(request["child"], ("hostId", "title", "settings"), at="child")
    for at, value in (("parent.taskId", parent["taskId"]), ("parent.hostId", parent["hostId"]),
                      ("child.hostId", child["hostId"]), ("child.title", child["title"])):
        _text(value, at, 500)
    if not marker.valid_segment(parent["taskId"]):
        raise ValueError("parent.taskId is not a valid task identity")
    if parent["hostId"] != child["hostId"]:
        raise ValueError("managed start supports one local host only")
    _settings(parent["settings"], "parent.settings")
    _settings(child["settings"], "child.settings")
    for root in _strings(request["artifactRoots"], "artifactRoots"):
        _path(root, "artifactRoots")
    _strings(request["allowedRecipients"], "allowedRecipients")
    if parent["taskId"] not in request["allowedRecipients"]:
        raise ValueError("the parent must be an allowed recipient")
    entries = request["criteria"]
    if not isinstance(entries, list) or not entries or len(entries) > 256:
        raise ValueError("criteria must be a nonempty list of at most 256 entries")
    seen = set()
    for entry in entries:
        _object(entry, ("id", "title", "required"), at="criterion")
        _text(entry["id"], "criterion.id", 500)
        _text(entry["title"], "criterion.title")
        if type(entry["required"]) is not bool:
            raise ValueError("criterion.required must be a boolean")
        identifier = finding_id(entry["id"])
        if identifier in seen:
            raise ValueError(f"duplicate criterion {identifier!r}")
        seen.add(identifier)
        entry["id"], entry["title"] = identifier, entry["title"].strip()
    return request


def operation_ids(request_id):
    digest = hashlib.sha256(request_id.encode("utf-8")).hexdigest()
    return f"managed-create-{digest}", f"managed-business-{digest}"


def request_identity(request, store, socket, marker_root, *, ledger_identity, state_selector=None):
    """The request plus its physical routing context, never business text in a new store."""
    location = store.locate()
    if location["device"] is None or location["inode"] is None:
        raise ValueError("the selected store has no observed physical identity")
    root = _path(str(marker_root), "markerRoot", directory=False)
    socket_identity = _path(str(socket), "socket", directory=False)
    workspace = _path(request["child"]["settings"]["cwd"], "workspace")
    selectors = {
        "original": {"state": str(state_selector if state_selector is not None else store.path.parent),
                     "socket": str(socket), "markerRoot": str(marker_root)},
        "db": {key: location[key] for key in ("storeId", "device", "inode", "realPath")},
        "socket": socket_identity, "markerRoot": root, "workspace": workspace,
        "ledger": ledger_identity,
        "artifactRoots": [_path(p, "artifactRoots") for p in request["artifactRoots"]],
        "bootstrapVersion": BOOTSTRAP_VERSION,
    }
    fingerprint = hashlib.sha256(json.dumps(
        {"request": request, "selectors": selectors}, sort_keys=True,
        separators=(",", ":"), ensure_ascii=False, allow_nan=False,
    ).encode("utf-8")).hexdigest()
    create, dispatch = operation_ids(request["requestId"])
    return {
        "request_id": request["requestId"], "issue_key": request["issueKey"],
        "request_fingerprint": fingerprint, "fingerprint_version": SCHEMA,
        "workspace": workspace, "marker_root": root, "socket_identity": socket_identity,
        "create_request_id": create, "dispatch_request_id": dispatch,
    }


class ManagedStart:
    """Coordinate bounded steps; the caller retries the same complete request."""

    def __init__(self, store, clock, adapter, worker_observation, *, socket, marker_root,
                 state_selector=None):
        from .registry import Registry
        from .criteria import CriteriaService

        self.store, self.clock, self.adapter = store, clock, adapter
        self.registry, self.criteria = Registry(store, clock), CriteriaService(store, clock)
        self.worker_observation = worker_observation
        self.socket, self.marker_root = str(socket), str(marker_root)
        self.state_selector = state_selector

    def run(self, raw):
        from . import rolepolicy
        from .errors import RegistrationError, RefusalReason

        request = parse_request(raw)
        ledger = self.adapter.ledger_identity_record()
        if not isinstance(ledger, dict) or any(ledger.get(key) is None
                for key in ("realPath", "device", "inode")):
            raise ValueError("managed start requires an observed bridge ledger identity")
        self.adapter.require_ledger(ledger)
        self._ledger_identity = ledger
        identity = request_identity(request, self.store, self.socket, self.marker_root,
                                    ledger_identity=ledger, state_selector=self.state_selector)
        self.request, self.identity = request, identity
        info = self.store.path.stat()
        self._physical = (info.st_dev, info.st_ino)
        self.assignment = marker.assignment_id(identity["dispatch_request_id"])
        self.row = self.registry.start_request(request["requestId"])
        if self.row and self.row["request_fingerprint"] != identity["request_fingerprint"]:
            raise RegistrationError(RefusalReason.RELATIONSHIP_CONFLICT,
                                    "managed request id belongs to different input or selectors")
        requirements = []
        policy = rolepolicy.declared()
        if not policy:
            return self.result("refused", "preflight", "caller_policy_unconfigured")
        for role in ("parent", "child"):
            settings = request[role]["settings"]
            finding = rolepolicy.check_record({**settings, "citedRole": role}, role, policy)
            if finding:
                return self.result("refused", "preflight", finding["code"])
            requirements.append({"role": role, "model": settings["model"],
                                 "reasoningEffort": settings["reasoningEffort"]})
        self.requirements = requirements
        readiness = rolepolicy.worker_readiness(self.worker_observation(), requirements)
        if not readiness["ready"]:
            return self.result("refused", "preflight", readiness["reason"])
        # Registry and bridge ledger provide the durable exclusion. This lock prevents
        # duplicate marker/settings work by simultaneous callers of this one request.
        with self._lock():
            self.row = self.registry.reserve_start(identity)
            return self._advance()

    def result(self, state, stage, reason=None, **observed):
        row = self.row or {}
        business = observed.get("businessTurnId")
        child = row.get("child_task_id")
        result = {
            "schema": SCHEMA, "requestId": self.request["requestId"],
            "state": state, "stage": stage, "reason": reason,
            "assignmentId": self.assignment,
            "relationshipId": row.get("relationship_id"),
            "executionGeneration": row.get("execution_generation"),
            "childTaskId": child,
            "standbyTurnId": row.get("standby_turn_id"),
            "creationRequestId": self.identity["create_request_id"],
            "businessRequestId": self.identity["dispatch_request_id"],
            "requestFingerprint": self.identity["request_fingerprint"],
            "ledger": self._ledger_identity,
            "reservationState": row.get("state"),
            "reservationRevision": row.get("revision"),
            "recovery": "Retry only this same complete request; do not create a replacement.",
            "selectors": {
                "state": str(self.store.path.parent),
                "markerRoot": self.identity["marker_root"],
                "workspace": self.identity["workspace"],
            },
            **observed,
        }
        if marker.valid_segment(business) and marker.valid_segment(child):
            result["reportingArgv"] = [
                "--state", str(result["selectors"]["state"]),
                "reporting-show",
                "--marker-root", result["selectors"]["markerRoot"],
                "--workspace", result["selectors"]["workspace"],
                "--assignment", self.assignment,
                "--session", child,
                "--turn", business,
            ]
        # Diagnostics do not authorize another host effect. Persist no business prompt.
        with self.store.transaction():
            self.store.journal("managed_start_observed", self.request["requestId"],
                               result, at=self.clock.iso())
        return result

    def _lock(self):
        import fcntl
        import os
        import stat
        from contextlib import contextmanager

        @contextmanager
        def held():
            name = hashlib.sha256(self.request["requestId"].encode()).hexdigest()
            path = self.store.path.parent / f"managed-start-{name}.lock"
            descriptor = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
            try:
                info = os.fstat(descriptor)
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
                    raise ValueError("managed request lock is not an owned regular file")
                try:
                    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError as error:
                    raise ValueError("this managed request is already being advanced") from error
                yield
            finally:
                os.close(descriptor)
        return held()

    def _advance(self):
        from . import intent, rolepolicy
        from .models import Endpoint
        from .registry import record_settings
        request, identity = self.request, self.identity
        workspace, root = identity["workspace"], identity["marker_root"]
        if self.row["state"] == "attached":
            problem = self._registered_problem()
            if problem:
                return self.result("refused", "replay", problem)
        declared = intent.declare_intent(
            root, workspace=workspace, dispatch_request_id=identity["dispatch_request_id"],
            issue_key=request["issueKey"], declared_at=self.clock.iso(),
            criteria_source=request["criteriaSource"], baseline_revision=request["baselineRevision"],
            authorized_settings=request["child"]["settings"], db_path=str(self.store.path),
        )
        if declared["outcome"] == "conflict":
            return self.result("refused", "intent", "intent_conflict")
        if self.row["state"] == "reserved":
            self.row = self.registry.arm_start(request["requestId"], identity["request_fingerprint"],
                                               self.row["revision"])
        settings = TaskSettings(request["child"]["settings"])
        self.adapter.require_ledger(self._ledger_identity)
        receipt = self.adapter.get_operation(identity["create_request_id"])
        if receipt is None or receipt.get("status") == "not_attempted":
            readiness = rolepolicy.worker_readiness(self.worker_observation(), self.requirements)
            if not readiness["ready"]:
                return self.result("refused", "creation", readiness["reason"])
            data = settings.data
            self.adapter.require_ledger(self._ledger_identity)
            receipt = self.adapter.create_thread(
                identity["create_request_id"], cwd=data["cwd"], prompt=BOOTSTRAP,
                title=request["child"]["title"], sandbox=settings.sandbox_mode(),
                model=data["model"], reasoning_effort=data["reasoningEffort"],
                runtime_workspace_roots=data["runtimeWorkspaceRoots"],
                expected_sandbox_policy=data["sandbox"], role="child",
            )
        if isinstance(receipt, dict) and receipt.get("status") == "failed":
            receipt = self._recover_standby(receipt, settings)
        if not isinstance(receipt, dict) or receipt.get("status") != "accepted":
            outcome = "failed" if isinstance(receipt, dict) and receipt.get("status") == "failed" else "unknown"
            intent.record_attempt(root, workspace=workspace, assignment=self.assignment,
                                  outcome=outcome, at=self.clock.iso(),
                                  task_id=receipt.get("threadId") if isinstance(receipt, dict) else None)
            return self.result("incomplete", "creation", "creation_" + outcome,
                               retainedChildTaskId=receipt.get("threadId") if isinstance(receipt, dict) else None,
                               standbyRecovery={key: receipt[key] for key in
                                   ("recoveryReason", "recoveryRequestId", "recoveryStatus")
                                   if isinstance(receipt, dict) and key in receipt})
        task, standby = receipt.get("threadId"), receipt.get("turnId")
        if not marker.valid_segment(task) or not marker.valid_segment(standby):
            return self.result("incomplete", "creation", "creation_identity_unobserved")
        creation = receipt.get("creation")
        if not isinstance(creation, dict) or settings.mismatches(
                creation, exact_approval_policy=True):
            return self.result("refused", "creation", "creation_settings_unverified")
        self.row = self.registry.record_start_receipt(request["requestId"],
                                                      identity["request_fingerprint"], receipt)
        bound = intent.bind(root, workspace=workspace, assignment=self.assignment,
                            session_id=task, task_id=task, at=self.clock.iso())
        if bound["outcome"] == intent.CONFLICT:
            return self.result("refused", "binding", "marker_identity_conflict")
        parent = request["parent"]
        record = self.registry.register(
            parent=Endpoint(parent["taskId"], parent["hostId"], cwd=parent["settings"]["cwd"]),
            child=Endpoint(task, request["child"]["hostId"], cwd=workspace, cxc_session=task),
            issue_key=request["issueKey"], artifact_roots=request["artifactRoots"],
            allowed_recipients=self._recipients(task), scope_ref=request["scopeRef"],
            dispatch_request_id=identity["dispatch_request_id"], dispatch_turn_id=standby,
            project_key=request.get("projectKey"), managed_request_id=request["requestId"],
        )
        rid = record["relationshipId"]
        self.row = self.registry.start_request(request["requestId"])
        self.criteria.ensure_registered(rid, request["criteria"], source_ref=request["criteriaSource"])
        for role, who in (("parent", parent["taskId"]), ("child", task)):
            record_settings(self.store, self.clock, who, request[role]["settings"],
                            source="managed_start", role=role, ensure_only=True)
        intent.register_relationship(root, workspace=workspace, assignment=self.assignment,
                                     relationship_id=rid, dispatch_request_id=identity["dispatch_request_id"],
                                     at=self.clock.iso(), db_path=str(self.store.path))
        problem = self._registered_problem(complete=True)
        if problem:
            return self.result("refused", "readback", problem)
        # Accepted or uncertain business effects are reconciled BEFORE checking busy:
        # the same request is allowed to observe its own running turn.
        self.adapter.require_ledger(self._ledger_identity)
        sent = self.adapter.get_operation(identity["dispatch_request_id"])
        if sent is not None and sent.get("status") != "not_attempted":
            return self._business_result(sent)
        turn = self.adapter.read_turn(task, standby)
        if turn is None or turn.status != "completed":
            return self.result("incomplete", "standby", "standby_incomplete")
        from .lifecycle import observe
        observed = observe(self.adapter, task, cwd=workspace)
        if not observed.may_send:
            return self.result("incomplete", "business", observed.withhold_reason)
        readiness = rolepolicy.worker_readiness(self.worker_observation(), self.requirements)
        if not readiness["ready"]:
            return self.result("refused", "business", readiness["reason"])
        self.adapter.require_ledger(self._ledger_identity)
        sent = self.adapter.send_message(identity["dispatch_request_id"], task, self._packet(),
                                         settings, before_start=self._before_start,
                                         guard_rpc_requests=HOST_READY_RPC_REQUESTS)
        return self._business_result(sent)

    def _recover_standby(self, creation, settings):
        """Resume a proven shell only when the bridge proves no first turn was sent.

        The original failed creation receipt remains immutable in its ledger. The
        separate deterministic send receipt owns the recovered standby turn.
        Unknown turn/start effects are never retried under this new identity.
        """
        task = creation.get("threadId")
        effects = creation.get("attemptedEffects")
        observed = creation.get("creation")
        if (not marker.valid_segment(task) or creation.get("turnId")
                or not isinstance(effects, list) or "thread/start" not in effects
                or "turn/start" in effects or not isinstance(observed, dict)
                or settings.mismatches(observed, exact_approval_policy=True)):
            return creation
        request_id = "managed-standby-" + hashlib.sha256(
            self.request["requestId"].encode()).hexdigest()
        self.adapter.require_ledger(self._ledger_identity)
        recovered = self.adapter.get_operation(request_id)
        if recovered is None or recovered.get("status") == "not_attempted":
            from .lifecycle import observe
            from . import rolepolicy
            lifecycle = observe(self.adapter, task, cwd=self.identity["workspace"])
            if not lifecycle.may_send:
                return {**creation, "recoveryReason": lifecycle.withhold_reason}
            readiness = rolepolicy.worker_readiness(self.worker_observation(), self.requirements)
            if not readiness["ready"]:
                return {**creation, "recoveryReason": readiness["reason"]}

            async def before_start(rpc):
                import sqlite3
                self.adapter.require_ledger(self._ledger_identity)
                readiness = rolepolicy.worker_readiness(self.worker_observation(), self.requirements)
                if not readiness["ready"]:
                    return {"code": readiness["reason"], "message": "Standby recovery withheld"}
                info = self.store.path.stat()
                if (info.st_dev, info.st_ino) != self._physical:
                    return {"code": "managed_store_changed", "message": "Standby recovery store changed"}
                with closing(sqlite3.connect(self.store.path.resolve().as_uri() + "?mode=ro", uri=True)) as db:
                    row = db.execute("SELECT request_fingerprint,state FROM managed_start_requests "
                                     "WHERE request_id=?", (self.request["requestId"],)).fetchone()
                if row != (self.identity["request_fingerprint"], "create_armed"):
                    return {"code": "managed_reservation_changed", "message": "Standby recovery reservation changed"}
                return await self._host_ready(rpc, task)

            self.adapter.require_ledger(self._ledger_identity)
            recovered = self.adapter.send_message(request_id, task, BOOTSTRAP, settings,
                                                  before_start=before_start,
                                                  guard_rpc_requests=HOST_READY_RPC_REQUESTS)
        if (not isinstance(recovered, dict) or recovered.get("status") != "accepted"
                or recovered.get("threadId") != task
                or not marker.valid_segment(recovered.get("turnId"))):
            return {**creation, "recoveryRequestId": request_id,
                    "recoveryStatus": recovered.get("status") if isinstance(recovered, dict) else "unknown"}
        # This is a composed admission fact, not a rewrite of the failed bridge operation.
        return {**creation, "status": "accepted", "turnId": recovered["turnId"],
                "creationStatus": creation["status"], "standbyRecovery": recovered,
                "recoveryRequestId": request_id}

    def _recipients(self, child):
        """Managed execution authorizes revisions to the same child it creates.

        The caller cannot name an as-yet uncreated task. Preserve its declared recipients
        and add only the retained creation identity, never an inferred replacement.
        """
        return list(dict.fromkeys([*self.request["allowedRecipients"], child]))

    def _registered_problem(self, *, complete=False):
        from .registry import load_settings
        from .criteria import set_digest, MANAGED
        from . import intent

        row = self.row
        if row.get("state") != "attached":
            return "reservation_not_attached"
        record = self.registry.get(row["relationship_id"])
        if record["status"] != "active":
            return "relationship_not_active"
        if (record["parent"]["taskId"] != self.request["parent"]["taskId"] or
                record["parent"]["hostId"] != self.request["parent"]["hostId"] or
                record["child"]["hostId"] != self.request["child"]["hostId"] or
                record["authorizedScope"] != {
                    "artifactRoots": self.request["artifactRoots"],
                    "allowedRecipients": self._recipients(row["child_task_id"]),
                    "scopeRef": self.request["scopeRef"],
                }):
            return "managed_scope_changed"
        if record["executionGeneration"] != row["execution_generation"]:
            return "stale_generation"
        generation = self.registry.generation(row["relationship_id"], row["execution_generation"])
        if (generation["dispatchTurnId"] != row["standby_turn_id"] or
                generation["dispatchRequestId"] != self.identity["dispatch_request_id"] or
                record["child"]["taskId"] != row["child_task_id"]):
            return "managed_identity_changed"
        if not complete:
            return None
        criteria = self.criteria.get(row["relationship_id"])
        if (criteria is None or set_digest(criteria["criteria"]) != set_digest(self.request["criteria"]) or criteria["setDigest"] != set_digest(self.request["criteria"]) or
                criteria["sourceRef"] != self.request["criteriaSource"] or
                self.criteria.mode(row["relationship_id"]) != MANAGED):
            return "managed_criteria_changed"
        for role, task in (("parent", self.request["parent"]["taskId"]),
                           ("child", row["child_task_id"])):
            recorded = load_settings(self.store, task)
            expected = {**self.request[role]["settings"], "citedRole": role}
            if recorded is None or recorded.data != expected:
                return "managed_settings_changed"
        directory = marker.assignment_dir(self.identity["marker_root"], self.identity["workspace"],
                                           self.assignment)
        facts, unreadable = marker.read_assignment(directory)
        if unreadable or intent.malformed(facts):
            return "managed_marker_unreadable"
        if (facts.get("bound", {}).get("taskId") != row["child_task_id"] or
                facts.get("bound", {}).get("sessionId") != row["child_task_id"] or
                facts.get("relationship", {}).get("relationshipId") != row["relationship_id"]):
            return "managed_marker_changed"
        return None

    def _packet(self):
        control = {
            "taskId": self.row["child_task_id"], "standbyTurnId": self.row["standby_turn_id"],
            "dispatchRequestId": self.identity["dispatch_request_id"], "assignmentId": self.assignment,
            "relationshipId": self.row["relationship_id"],
            "executionGeneration": self.row["execution_generation"],
            "state": str(self.store.path.parent), "socket": self.socket,
            "workspace": self.identity["workspace"], "markerRoot": self.identity["marker_root"],
        }
        return (
            "Managed assignment routing record:\n" + json.dumps(control, sort_keys=True) +
            "\nFirst publish your own intent-claim using this task, assignment and dispatch request. "
            "Publish your own per-turn disposition; continuation claims must name standbyTurnId. "
            "Do not fabricate completion, ACK or verification. Report through the registered relay. "
            "The following is the authorized business assignment:\n\n" + self.request["prompt"]
        )

    def _business_result(self, sent):
        from .admission import admit_explicitly

        if not isinstance(sent, dict) or sent.get("status") != "accepted":
            status = sent.get("status", "unknown") if isinstance(sent, dict) else "unknown"
            return self.result("incomplete", "business", "business_" + str(status))
        turn = sent.get("turnId")
        if not marker.valid_segment(turn) or sent.get("threadId") != self.row["child_task_id"]:
            return self.result("incomplete", "business", "business_identity_unobserved")
        problem = self._registered_problem(complete=True)
        if problem:
            return self.result("incomplete", "business_accepted", problem)
        admit_explicitly(self.store, self.clock, self.row["relationship_id"],
                         self.row["execution_generation"], turn,
                         actor=self.request["parent"]["taskId"],
                         detail="managed business dispatch confirmed by its retained bridge receipt")
        return self.result("admitted", "business_accepted", businessTurnId=turn,
                           childClaim="not_observed", hookFiring="not_observed")

    async def _before_start(self, rpc):
        """Fresh authorization after resume, without blocking the transport on itself.

        The UI does not take our locks. A user change after the final host read can
        still race turn/start; the host supplies no conditional start primitive.
        """
        import sqlite3
        from . import rolepolicy
        from .lifecycle import BLOCKING_GOAL_STATUS

        def refuse(code):
            return {"code": code, "message": "Managed business start withheld: " + code}

        self.adapter.require_ledger(self._ledger_identity)
        task = self.row["child_task_id"]
        readiness = rolepolicy.worker_readiness(self.worker_observation(), self.requirements)
        if not readiness["ready"]:
            return refuse(readiness["reason"])
        # This callback runs on the transport worker, so it must never touch the
        # main thread's sqlite connection or submit to its own transport inbox.
        path = self.store.path.resolve()
        info = path.stat()
        expected = self._physical
        if (info.st_dev, info.st_ino) != expected:
            return refuse("managed_store_changed")
        with closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)) as db:
            db.row_factory = sqlite3.Row
            db.execute("BEGIN")
            row = db.execute("SELECT * FROM relationships WHERE relationship_id=?",
                             (self.row["relationship_id"],)).fetchone()
            if row is None or row["status"] != "active":
                return refuse("relationship_not_active")
            if row["execution_generation"] != self.row["execution_generation"]:
                return refuse("stale_generation")
            expected_scope = {
                "issue_key": self.request["issueKey"],
                "parent_task_id": self.request["parent"]["taskId"],
                "parent_host_id": self.request["parent"]["hostId"],
                "child_task_id": task,
                "child_host_id": self.request["child"]["hostId"],
                "scope_ref": self.request["scopeRef"],
            }
            if (any(row[key] != value for key, value in expected_scope.items()) or
                    json.loads(row["artifact_roots"]) != self.request["artifactRoots"] or
                    json.loads(row["allowed_recipients"]) != self._recipients(task)):
                return refuse("managed_scope_changed")
            generation = db.execute(
                "SELECT dispatch_turn_id,dispatch_request_id FROM generations "
                "WHERE relationship_id=? AND execution_generation=?",
                (self.row["relationship_id"], self.row["execution_generation"]),
            ).fetchone()
            if (generation is None or generation["dispatch_turn_id"] != self.row["standby_turn_id"] or
                    generation["dispatch_request_id"] != self.identity["dispatch_request_id"]):
                return refuse("managed_identity_changed")
            mode = db.execute("SELECT mode FROM verification_mode WHERE relationship_id=?",
                              (self.row["relationship_id"],)).fetchone()
            if mode is None or mode["mode"] != "managed":
                return refuse("managed_criteria_changed")
            for role, who in (("parent", self.request["parent"]["taskId"]), ("child", task)):
                current = db.execute("SELECT settings FROM authorized_settings WHERE task_id=?",
                                     (who,)).fetchone()
                if current is None or json.loads(current["settings"]) != {
                    **self.request[role]["settings"], "citedRole": role,
                }:
                    return refuse("managed_settings_changed")
            criteria = db.execute("SELECT criterion_id,title,required,source_ref,set_digest "
                                  "FROM canonical_criteria WHERE relationship_id=?",
                                  (self.row["relationship_id"],)).fetchall()
            from .criteria import set_digest
            if (not criteria or any(entry["source_ref"] != self.request["criteriaSource"] or
                    entry["set_digest"] != set_digest(self.request["criteria"]) for entry in criteria) or
                    set_digest([{"id": e["criterion_id"], "title": e["title"],
                                 "required": bool(e["required"])} for e in criteria]) !=
                    set_digest(self.request["criteria"])):
                return refuse("managed_criteria_changed")
        return await self._host_ready(rpc, task)

    async def _host_ready(self, rpc, task):
        from .lifecycle import BLOCKING_GOAL_STATUS

        def refuse(code):
            return {"code": code, "message": "Managed turn withheld: " + code}

        # Enumerate by exact task identity; a bounded absence is unknown, not permission.
        archived = None
        for archive_filter in (True, False):
            cursor = None
            for _ in range(MAX_HOST_CHECK_PAGES):
                params = {"limit": 50, "archived": archive_filter, "useStateDbOnly": True}
                if cursor:
                    params["cursor"] = cursor
                page = await rpc.call("thread/list", params)
                if any(item.get("id") == task for item in page.get("data", [])):
                    archived = archive_filter
                    break
                cursor = page.get("nextCursor")
                if not cursor:
                    break
            if archived is not None:
                break
        if archived is not False:
            return refuse("recipient_archived" if archived else "lifecycle_unknown")
        goal = (await rpc.call("thread/goal/get", {"threadId": task})).get("goal")
        if goal is not None:
            if not isinstance(goal, dict) or not isinstance(goal.get("status"), str):
                return refuse("lifecycle_unknown")
            if goal["status"] in BLOCKING_GOAL_STATUS:
                return refuse(BLOCKING_GOAL_STATUS[goal["status"]])
        thread = (await rpc.call("thread/read", {"threadId": task})).get("thread") or {}
        if thread.get("canAcceptDirectInput") is False:
            return refuse("recipient_cannot_accept_input")
        if (thread.get("status") or {}).get("type") not in ("idle", "notLoaded"):
            return refuse("recipient_not_idle")
        return None
