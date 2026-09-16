"""The command surface. Every command is JSON-out and exit-coded, so a caller never parses prose.

Exit codes: 0 success, 2 a refusal carrying a machine-readable reason, 3 a host problem, 4 usage.

One thing is deliberately absent. There is no flag that makes the relay compute an acknowledgement
proof: --ack-proof is required, because a proof the relay produced would prove nothing about who
sent it. The separate ack-proof command exists so a parent can compute the value from its OWN turn
id, which is a different act entirely.
"""

import argparse
import json
import sys
from pathlib import Path

from .ack import AckService
from .admission import AnchorOrExplicit, admit_explicitly
from .assignment import AssignmentView
from .clock import SystemClock
from .criteria import CriteriaService
from .currency import head_revision
from .delivery import COMPLETION, DeliveryService
from .errors import RelayError
from .identity import ack_proof as derive_ack_proof
from .manifest import build as build_manifest, freeze as freeze_manifest, revision_hash
from .models import Endpoint, TurnRef
from .receipts import ReceiptIntake, contract_record
from .reconcile import Reconciler
from .registry import Registry, contract_record as relationship_record, record_settings
from .settings import REQUIRED as REQUIRED_SETTINGS
from .store import Store, state_dir
from .sync import SyncOutbox, render_progress_summary

EXIT_OK, EXIT_REFUSED, EXIT_HOST, EXIT_USAGE = 0, 2, 3, 4

# Which commands need to reach the App Server, and which do not. Reported by doctor, because a
# caller should learn this from one command instead of from a failure halfway through.
HOST_REQUIRED_COMMANDS = ("daemon", "deliver", "reconcile", "recover", "verify-acks")
OFFLINE_COMMANDS = (
    "ack", "ack-proof", "admit-turn", "assignment-show", "claim", "criteria-register",
    "criteria-show", "doctor", "emit", "generation-bind", "generation-open", "register",
    "relationship-resume", "relationship-status", "revision-head", "settings-record",
    "settings-show", "show", "status", "verdict",
)


class _LazyAdapter:
    """Stands in for the adapter so nothing is constructed until something actually uses it."""

    def __init__(self, services):
        self._services = services

    def __getattr__(self, name):
        return getattr(self._services.adapter, name)


class Services:
    def __init__(self, args):
        directory = Path(args.state) if args.state else state_dir(args.socket)
        self.store = Store(directory / "relay.sqlite3")
        self.clock = SystemClock()
        self.socket_path = args.socket
        self.adapter_requested = bool(args.socket)
        self._adapter = None
        self.criteria = CriteriaService(self.store, self.clock)
        self.registry = Registry(self.store, self.clock)
        self.intake = ReceiptIntake(
            self.store, self.registry, self.clock,
            admission=AnchorOrExplicit(_LazyAdapter(self) if self.adapter_requested else None),
        )
        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.ack = AckService(
            self.store, self.registry, self.intake, self.delivery, self.clock,
            criteria=self.criteria,
        )
        self.reconciler = Reconciler(self.store, self.registry, self.delivery, self.clock)
        self.sync = SyncOutbox(self.store, self.clock)
        self.ack.sync = self.sync
        self.assignments = AssignmentView(
            self.store, self.registry, self.clock, criteria=self.criteria
        )

    @property
    def adapter(self):
        """Built on first real use, so requiring one and probing for one are different acts."""
        if self._adapter is None and self.adapter_requested:
            from .bridge_adapter import BridgeHostAdapter

            self._adapter = BridgeHostAdapter(
                self.socket_path, store=self.store, clock=self.clock
            )
        return self._adapter

    def close(self):
        """Close what this Services owns, adapter first, then the store.

        Closing the store alone left the adapter's transport thread running: measured alive
        after close and gone only after an explicit adapter close. A one-shot CLI exit hides
        that behind process teardown, but anything that opens Services more than once in a
        process leaks a connection and a thread each time. Only this instance's own adapter is
        touched; no shared or global process is affected.
        """
        adapter = self._adapter
        if adapter is not None:
            try:
                adapter.close()
            except Exception:
                pass
            self._adapter = None
        self.store.close()


def _require_adapter(services):
    if not services.adapter_requested:
        raise SystemExit2("this command needs --socket to reach the host", EXIT_USAGE)


class SystemExit2(Exception):
    def __init__(self, message, code):
        super().__init__(message)
        self.code = code


# --------------------------------------------------------------------- commands


def cmd_register(services, args) -> dict:
    record = services.registry.register(
        parent=Endpoint(args.parent_task, args.parent_host, cwd=args.parent_cwd,
                        cxc_session=args.parent_cxc_session),
        child=Endpoint(args.child_task, args.child_host, cwd=args.child_cwd,
                       cxc_session=args.child_cxc_session),
        issue_key=args.issue,
        artifact_roots=args.artifact_root,
        allowed_recipients=args.allowed_recipient,
        scope_ref=args.scope_ref,
        dispatch_request_id=args.dispatch_request_id,
        dispatch_turn_id=args.dispatch_turn_id,
        supersedes=args.supersedes,
    )
    payload = relationship_record(record)
    # Execution settings come from the creation result the caller already holds. Recording them
    # here is what lets a later send preserve them instead of inheriting a host default.
    recorded = {}
    for task, raw in ((args.parent_task, args.parent_settings),
                      (args.child_task, args.child_settings)):
        if raw:
            record_settings(services.store, services.clock, task, _settings_json(raw),
                            source="creation_result")
            recorded[task] = "recorded"
    payload["authorizedSettings"] = recorded or None
    return payload


def _settings_json(raw: str) -> dict:
    """A JSON object, or @path to a file holding one."""
    if raw.startswith("@"):
        with open(raw[1:], encoding="utf-8") as handle:
            return json.load(handle)
    return json.loads(raw)


def cmd_settings_record(services, args) -> dict:
    """Record or replace one task's authorized execution settings.

    This is the exact interface JUN-92 populates from Run's creation result. Required fields:
    sandbox (the full SandboxPolicy object), approvalPolicy, cwd, runtimeWorkspaceRoots, model,
    reasoningEffort and environments. Anything missing is refused here rather than at send time.
    """
    return record_settings(
        services.store, services.clock, args.task, _settings_json(args.settings),
        source=args.source,
    )


def cmd_settings_show(services, args) -> dict:
    from .registry import load_settings

    settings = load_settings(services.store, args.task)
    if settings is None:
        return {"task": args.task, "settings": None, "usable": False,
                "missing": list(REQUIRED_SETTINGS)}
    return {"task": args.task, "settings": settings.data,
            "usable": not settings.missing(), "missing": settings.missing()}


def cmd_generation_open(services, args) -> dict:
    return services.registry.open_generation(
        args.relationship, dispatch_request_id=args.dispatch_request_id, reason=args.reason,
        dispatch_turn_id=args.dispatch_turn_id,
    )


def cmd_generation_bind(services, args) -> dict:
    return services.registry.bind_anchor(
        args.relationship, args.generation, dispatch_turn_id=args.dispatch_turn_id,
        source=args.source,
    )


def cmd_admit_turn(services, args) -> dict:
    admit_explicitly(
        services.store, services.clock, args.relationship, args.generation, args.turn,
        actor=args.actor, detail=args.reason,
    )
    return {"relationship": args.relationship, "generation": args.generation,
            "turn": args.turn, "evidence": "explicit_admission"}


def cmd_relationship_status(services, args) -> dict:
    return relationship_record(
        services.registry.set_status(args.relationship, args.status, actor=args.actor)
    )


def cmd_relationship_resume(services, args) -> dict:
    return relationship_record(services.registry.resume(
        args.relationship, expect_generation=args.expect_generation,
        expect_artifact_roots=args.expect_artifact_root,
        expect_allowed_recipients=args.expect_allowed_recipient, actor=args.actor,
    ))


def cmd_emit(services, args) -> dict:
    relationship = services.registry.require_active(args.relationship)
    roots = relationship["authorizedScope"]["artifactRoots"]
    if args.artifact:
        entries, _bindings = build_manifest(args.artifact, roots)
        digest = revision_hash(entries)
        manifest = [entry.to_record() for entry in entries]
    else:
        entries, manifest, digest = [], None, "0" * 64
    reference = args.manifest_ref
    if reference and entries:
        freeze_manifest(entries, reference)
    from .identity import event_id as derive_event_id

    observed_status, proof = _observed_turn_status(services, args)
    args.turn_status = observed_status

    event = derive_event_id(
        args.relationship, args.generation, digest, args.outcome,
        turn_id=args.turn_id, attempt=args.attempt,
    )
    payload = {
        "eventId": event,
        "relationshipId": args.relationship,
        "executionGeneration": args.generation,
        "attempt": args.attempt,
        "revisionHash": digest,
        "outcome": args.outcome,
        "producer": "child",
        "turnRef": {
            "threadId": args.turn_thread, "turnId": args.turn_id, "turnStatus": args.turn_status
        },
        "manifest": manifest,
        "emittedAt": services.clock.iso(),
    }
    if reference:
        payload["manifestRef"] = reference
    continuation = None
    if args.continues_anchor:
        # The child states its own continuation in the same call that completes the work, so a
        # multi-turn loop needs no extra step and no separate approval.
        continuation = {
            "anchorTurnId": args.continues_anchor,
            "actor": args.continuation_actor or args.turn_thread,
            "reason": args.continuation_reason or "continuation of this execution",
        }
    stored = services.intake.accept_child_receipt(
        payload,
        observation=TurnRef(args.turn_thread, args.turn_id, args.turn_status),
        continuation=continuation,
        # Stated by the child, out of band, because completion-receipt.json is frozen with
        # additionalProperties false and has no field for it.
        supersedes_revision=args.supersedes_revision,
    )
    result = {"receipt": contract_record(stored), "stage": stored.get("_stage"),
              "duplicate": stored.get("_duplicate"), "terminalProof": proof,
              "observedTurnStatus": observed_status}
    if stored.get("_stage") == "final" and not args.no_enqueue:
        result["delivery"] = dict(services.delivery.enqueue(event))
    return result


def _observed_turn_status(services, args):
    """Terminal readiness is read from the host, never taken from the caller.

    A caller passing --turn-status completed is making a claim about its own turn. With a host
    available, the claim is replaced by what the host actually reports, so an offline or
    mistaken claim cannot manufacture the terminal proof that turns a staged receipt into a
    deliverable one. Without a host, a readiness claim may only STAGE: it is admitted, stored
    and reported, and an independent observation later decides whether it becomes deliverable.
    """
    from .errors import RefusalReason, ReceiptRefused

    if services.adapter is not None:
        from .hostadapter import HostUnavailable

        try:
            turn = services.adapter.read_turn(args.turn_thread, args.turn_id)
        except HostUnavailable as error:
            raise ReceiptRefused(
                RefusalReason.UNASSIGNED_TURN,
                f"the host could not confirm turn {args.turn_id!r}: {error}",
            ) from error
        if turn is None:
            raise ReceiptRefused(
                RefusalReason.UNASSIGNED_TURN,
                f"turn {args.turn_id!r} does not exist on {args.turn_thread!r}",
            )
        return turn.status, "host_observed"
    if args.outcome == "ready_for_review" and args.turn_status != "inProgress":
        # No host to confirm with, so the claim is staged rather than trusted.
        return "inProgress", "unverified_staged"
    return args.turn_status, "claimed"


def cmd_deliver(services, args) -> dict:
    _require_adapter(services)
    if args.event:
        record = services.delivery.attempt(args.event, services.adapter)
        return {"attempt": record}
    out = []
    for row in services.delivery.eligible(now=services.clock.now(), limit=args.limit):
        out.append(services.delivery.attempt(row["event_id"], services.adapter))
    return {"attempts": out}


def cmd_reconcile(services, args) -> dict:
    _require_adapter(services)
    return services.reconciler.reconcile_attempt(args.request_id, services.adapter)


def cmd_recover(services, args) -> dict:
    _require_adapter(services)
    return services.reconciler.recover_on_start(services.adapter)


def cmd_claim(services, args) -> dict:
    return {"claim": services.ack.claim_verification(args.event, turn_id=args.turn)}


def cmd_ack_proof(services, args) -> dict:
    return {"eventId": args.event, "turnId": args.turn,
            "ackProof": derive_ack_proof(args.event, args.turn)}


def cmd_ack(services, args) -> dict:
    """With a host this verifies the turn. Without one it RECORDS the parent's intent.

    Refusing the call outright would throw away the one thing a parent can contribute from
    inside its own turn: an acknowledgement it authored, carrying a proof over its own turn id.
    What it cannot do offline is establish that the turn is real, so the record says so, it
    does not close the attempt, and it cannot yet produce a verdict. A process that holds host
    access completes it later with verify-acks.
    """
    record = services.ack.acknowledge(
        args.event, ack_turn_id=args.ack_turn, ack_proof=args.ack_proof,
        accepted=not args.reject, rejection_reason=args.reject, adapter=services.adapter,
    )
    if record.get("_verified") != "verified":
        record["_note"] = (
            "recorded as the parent's authored intent; this turn is not established yet, so it"
            " does not close the attempt and cannot yet produce a verdict. Run verify-acks from"
            " a process with host access."
        )
    return record


def cmd_verify_acks(services, args) -> dict:
    """Complete acknowledgements authored without a host, re-checking disposition as it goes."""
    _require_adapter(services)
    return {"results": services.ack.verify_pending_acks(services.adapter, limit=args.limit)}


def cmd_criteria_register(services, args) -> dict:
    optional = set(args.optional or [])
    entries = []
    for item in args.criterion:
        identifier, _, title = item.partition("=")
        entries.append({
            "id": identifier.strip(), "title": title.strip(),
            "required": identifier.strip() not in optional,
        })
    return services.criteria.register(args.relationship, entries, source_ref=args.source_ref)


def cmd_criteria_show(services, args) -> dict:
    registered = services.criteria.get(args.relationship)
    return {
        "relationshipId": args.relationship,
        "mode": services.criteria.mode(args.relationship),
        "criteria": registered["criteria"] if registered else None,
        "setDigest": registered["setDigest"] if registered else None,
        "sourceRef": registered["sourceRef"] if registered else None,
    }


def cmd_revision_head(services, args) -> dict:
    relationship = services.registry.get(args.relationship)
    generation = args.generation or relationship["executionGeneration"]
    head = head_revision(services.store.db, args.relationship, generation)
    return {"relationshipId": args.relationship, "executionGeneration": generation, "head": head}


def cmd_assignment_show(services, args) -> dict:
    if args.issue:
        return services.assignments.for_issue(args.issue)
    return services.assignments.state(args.relationship)


def cmd_assignment_find(services, args) -> dict:
    """What to call BEFORE creating a task, so a duplicate child is never opened by accident."""
    return services.assignments.for_issue(args.issue)


def cmd_assignment_mark(services, args) -> dict:
    return services.assignments.mark(
        args.relationship, args.mark, evidence=args.evidence, actor=args.actor,
        expected_event=args.expected_event,
    )


def cmd_sync_target(services, args) -> dict:
    return services.sync.set_target(args.relationship, args.target, args.target_ref)


def cmd_sync_next(services, args) -> dict:
    return {"jobs": services.sync.next(target=args.target, limit=args.limit)}


def cmd_sync_claim(services, args) -> dict:
    return services.sync.claim(args.sync, owner=args.owner)


def cmd_sync_operation(services, args) -> dict:
    return services.sync.operation(args.sync)


def cmd_sync_reconcile(services, args) -> dict:
    """Did this job's write already land? Answered from the document, before rewriting."""
    return services.sync.reconcile(args.sync, _read_text(args.observed))


def cmd_sync_complete(services, args) -> dict:
    return services.sync.complete(
        args.sync, claim_token=args.claim_token, target_ref=args.target_ref,
        readback=_read_text(args.readback), external_ref=args.external_ref,
    )


def cmd_sync_fail(services, args) -> dict:
    return services.sync.fail(args.sync, claim_token=args.claim_token, error=args.error)


def cmd_sync_retry(services, args) -> dict:
    return services.sync.retry(args.sync)


def cmd_sync_status(services, args) -> dict:
    return services.sync.snapshot(relationship_id=args.relationship)


def cmd_sync_progress(services, args) -> dict:
    """Queue the assignment's current state as a progress summary."""
    assignment = services.assignments.state(args.relationship)
    with services.store.transaction() as db:
        identifier = services.sync.enqueue_in(
            db, relationship_id=args.relationship, issue_key=assignment["issueKey"],
            subject_kind="progress", summary=render_progress_summary(assignment),
            event_id=assignment["head"]["eventId"],
            generation=assignment["executionGeneration"],
            revision=assignment["head"]["revisionHash"],
        )
    return {"syncId": identifier, "state": assignment["state"]}


def _read_text(value: str) -> str:
    """Inline text, or @path to a file holding it."""
    if value and value.startswith("@"):
        with open(value[1:], encoding="utf-8") as handle:
            return handle.read()
    return value or ""


def cmd_verdict(services, args) -> dict:
    criteria = []
    for item in args.criterion or []:
        name, _, value = item.partition("=")
        criteria.append({"id": name, "verdict": value or "verified"})
    findings = []
    for item in args.finding or []:
        name, _, rest = item.partition("=")
        disposition, _, note = rest.partition(":")
        findings.append({
            "id": name, "verdict": disposition or "verified", "note": note.strip(),
        })
    if args.criteria:
        findings.extend(_settings_json(args.criteria))
    return services.ack.record_verdict(
        args.event, verdict=args.verdict, verdict_turn_id=args.verdict_turn,
        criteria=criteria or None, findings=findings or None, reason=args.reason,
        expect_criteria_digest=args.expect_criteria_digest,
    )


def cmd_show(services, args) -> dict:
    """Everything about one event, so nothing a message abbreviated is unrecoverable.

    A delivered message is bounded on purpose. This is where the full manifest, the verdict
    findings, the attempt history and the acknowledgement actually live, and it is the command
    both directions are pointed at.
    """
    receipt = services.intake.get(args.event)
    row = services.intake.row(args.event)
    if receipt is None:
        raise SystemExit2(f"no event {args.event!r}", EXIT_USAGE)
    delivery = services.delivery.find(args.event)
    attempts = [
        {k: r[k] for k in r.keys()}
        for r in services.store.all(
            "SELECT * FROM attempts WHERE event_id = ? ORDER BY attempt_no", (args.event,)
        )
    ]
    ack = services.store.one("SELECT * FROM acks WHERE event_id = ?", (args.event,))
    verdict = services.store.one("SELECT * FROM verdicts WHERE event_id = ?", (args.event,))
    payload = {
        "event": args.event,
        "stage": row["stage"],
        "receipt": receipt,
        "delivery": dict(delivery) if delivery else None,
        "attempts": attempts,
        "acknowledgement": json.loads(ack["record"]) if ack else None,
        "acknowledgementVerified": ack["verified"] if ack else None,
        "verdict": json.loads(verdict["record"]) if verdict else None,
    }
    # The work report, whole. A delivered message may have had to elide part of it, and its
    # omission notice sends the recipient here, so this is the one place that must always
    # carry every field the message could have dropped. Imported locally to keep this change
    # out of the module import block, which a parallel branch is editing.
    from .report import read as read_work_report

    payload["workReport"] = read_work_report(services.store, args.event)
    if delivery is not None and args.message:
        # The bytes each attempt actually froze, with how far they got. A preview is offered
        # only when nothing has been prepared, and it is labelled a preview, because the old
        # behaviour re-rendered attempt_count + 1 and showed the NEXT request id as though it
        # were the one already sent.
        prepared = services.delivery.attempt_messages(args.event)
        payload["attemptMessages"] = prepared
        if not prepared:
            payload["previewMessage"] = services.delivery.preview_message(args.event)
    return payload


def cmd_status(services, args) -> dict:
    return services.delivery.snapshot(relationship_id=args.relationship)


def _scheduler_wait(clock, deadline, sleeper=None):
    """The real polling cadence, bounded by the run's own deadline.

    RelayDaemon.run only waits when it is GIVEN something to wait with, and the CLI used to give
    it nothing. A bounded run therefore ticked back to back and returned in milliseconds, and a
    deadline-only run busy-spun for its whole duration instead of polling. Neither is a bounded
    loop; both are the same missing argument.

    The wait is clamped to the time actually remaining, so a run never sleeps past its own
    deadline and a deadline that has already passed sleeps not at all. The sleeper stays
    injectable because the cadence tests drive it without real time passing.
    """
    import time as _time

    sleeper = sleeper or _time.sleep

    def wait(seconds):
        remaining = float(seconds)
        if deadline is not None:
            remaining = min(remaining, deadline - clock.now())
        if remaining > 0:
            sleeper(remaining)
            return remaining
        return 0.0

    return wait


def cmd_daemon(services, args) -> dict:
    _require_adapter(services)
    from .daemon import RelayDaemon, SingleInstance

    directory = Path(args.state) if args.state else state_dir(args.socket)
    daemon = RelayDaemon(
        services.store, services.registry, services.intake, services.delivery, services.ack,
        services.reconciler, services.adapter, clock=services.clock,
    )
    deadline = services.clock.now() + args.deadline if args.deadline else None
    with SingleInstance(directory):
        reports = daemon.run(
            max_ticks=args.max_ticks, deadline=deadline,
            sleep=_scheduler_wait(services.clock, deadline),
        )
    return {"ticks": [report.as_dict() for report in reports]}


def cmd_doctor(services, args) -> dict:
    import os

    return {
        "stateDirectory": str(services.store.path.parent),
        "procAvailable": os.path.isdir("/proc/self/fd"),
        "adapter": "bridge" if services.adapter_requested else "none (read-only, no --socket)",
        "relationships": services.store.one(
            "SELECT COUNT(*) AS c FROM relationships"
        )["c"],
        "openAttempts": len(Reconciler(
            services.store, services.registry, services.delivery, services.clock
        ).open_attempts()),
        "actorReachability": _reachability(services),
    }


def _reachability(services) -> dict:
    """What THIS process can actually do here, measured rather than assumed.

    A workspace-write task cannot write the default state directory and cannot connect to the
    App Server control socket. The honest answer for it is the offline subset plus a deferred
    acknowledgement, and it should learn that from one command rather than from a failure
    halfway through a delivery. The probe is a plain connect: it builds no bridge and starts no
    thread, which is why doctor can answer even where the bridge itself could not load.
    """
    import socket
    import tempfile

    directory = services.store.path.parent
    writable, detail = True, None
    try:
        with tempfile.NamedTemporaryFile(dir=directory, prefix=".reach-"):
            pass
    except OSError as error:
        writable, detail = False, f"{type(error).__name__}: {error}"

    connect = "not configured"
    if services.socket_path:
        probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        probe.settimeout(2)
        try:
            probe.connect(str(Path(services.socket_path).expanduser()))
            connect = "ok"
        except OSError as error:
            connect = f"{type(error).__name__}: {error}"
        finally:
            probe.close()

    return {
        "stateDirectoryWritable": writable,
        "stateDirectoryDetail": detail,
        "socketConfigured": bool(services.socket_path),
        "socketConnect": connect,
        "offlineCommands": list(OFFLINE_COMMANDS),
        "hostRequiredCommands": list(HOST_REQUIRED_COMMANDS),
    }


# ----------------------------------------------------------------------- wiring


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="codex-session-relay")
    parser.add_argument("--state")
    parser.add_argument("--socket")
    parser.add_argument("--json", action="store_true", default=True)
    subparsers = parser.add_subparsers(dest="command", required=True)

    register = subparsers.add_parser("register")
    register.add_argument("--parent-task", required=True)
    register.add_argument("--parent-host", required=True)
    register.add_argument("--parent-cwd")
    register.add_argument("--parent-cxc-session")
    register.add_argument("--child-task", required=True)
    register.add_argument("--child-host", required=True)
    register.add_argument("--child-cwd")
    register.add_argument("--child-cxc-session")
    register.add_argument("--issue", required=True)
    register.add_argument("--artifact-root", action="append", required=True)
    register.add_argument("--allowed-recipient", action="append", required=True)
    register.add_argument("--scope-ref")
    register.add_argument("--dispatch-request-id", required=True)
    register.add_argument("--dispatch-turn-id")
    register.add_argument("--supersedes")
    register.add_argument("--parent-settings",
                          help="authorized execution settings as JSON, or @path to a JSON file")
    register.add_argument("--child-settings",
                          help="authorized execution settings as JSON, or @path to a JSON file")
    register.set_defaults(handler=cmd_register)

    settings_record = subparsers.add_parser("settings-record")
    settings_record.add_argument("--task", required=True)
    settings_record.add_argument("--settings", required=True,
                                 help="JSON object, or @path to a JSON file")
    settings_record.add_argument("--source", default="creation_result")
    settings_record.set_defaults(handler=cmd_settings_record)

    settings_show = subparsers.add_parser("settings-show")
    settings_show.add_argument("--task", required=True)
    settings_show.set_defaults(handler=cmd_settings_show)

    opener = subparsers.add_parser("generation-open")
    opener.add_argument("--relationship", required=True)
    opener.add_argument("--dispatch-request-id", required=True)
    opener.add_argument("--reason", default="needs_changes_revision")
    opener.add_argument("--dispatch-turn-id")
    opener.set_defaults(handler=cmd_generation_open)

    binder = subparsers.add_parser("generation-bind")
    binder.add_argument("--relationship", required=True)
    binder.add_argument("--generation", type=int, required=True)
    binder.add_argument("--dispatch-turn-id", required=True)
    binder.add_argument("--source", default="dispatch_receipt")
    binder.set_defaults(handler=cmd_generation_bind)

    admit = subparsers.add_parser("admit-turn")
    admit.add_argument("--relationship", required=True)
    admit.add_argument("--generation", type=int, required=True)
    admit.add_argument("--turn", required=True)
    admit.add_argument("--actor", required=True)
    admit.add_argument("--reason", default="")
    admit.set_defaults(handler=cmd_admit_turn)

    status = subparsers.add_parser("relationship-status")
    status.add_argument("--relationship", required=True)
    status.add_argument("--status", required=True,
                        choices=["paused", "cancelled", "archived"])
    status.add_argument("--actor", required=True)
    status.set_defaults(handler=cmd_relationship_status)

    resume = subparsers.add_parser("relationship-resume")
    resume.add_argument("--relationship", required=True)
    resume.add_argument("--expect-generation", type=int, required=True)
    resume.add_argument("--expect-artifact-root", action="append", required=True)
    resume.add_argument("--expect-allowed-recipient", action="append", required=True)
    resume.add_argument("--actor", required=True)
    resume.set_defaults(handler=cmd_relationship_resume)

    emit = subparsers.add_parser("emit")
    emit.add_argument("--relationship", required=True)
    emit.add_argument("--generation", type=int, required=True)
    emit.add_argument("--attempt", type=int, default=1)
    emit.add_argument("--outcome", required=True,
                      choices=["ready_for_review", "failed", "interrupted", "blocked_needs_input"])
    emit.add_argument("--turn-thread", required=True)
    emit.add_argument("--turn-id", required=True)
    emit.add_argument("--turn-status", default="inProgress",
                      choices=["completed", "failed", "interrupted", "inProgress"])
    emit.add_argument("--artifact", action="append")
    emit.add_argument("--manifest-ref")
    emit.add_argument("--continues-anchor")
    emit.add_argument("--continuation-actor")
    emit.add_argument("--continuation-reason")
    emit.add_argument(
        "--supersedes-revision",
        help="the revision hash this one replaces, when re-emitting inside the same generation."
             " Without it, two revisions in one generation are a fork and neither is current.",
    )
    emit.add_argument("--no-enqueue", action="store_true")
    emit.set_defaults(handler=cmd_emit)

    deliver = subparsers.add_parser("deliver")
    deliver.add_argument("--event")
    deliver.add_argument("--limit", type=int, default=4)
    deliver.set_defaults(handler=cmd_deliver)

    reconcile = subparsers.add_parser("reconcile")
    reconcile.add_argument("--request-id", required=True)
    reconcile.set_defaults(handler=cmd_reconcile)

    subparsers.add_parser("recover").set_defaults(handler=cmd_recover)

    claim = subparsers.add_parser("claim")
    claim.add_argument("--event", required=True)
    claim.add_argument("--turn")
    claim.set_defaults(handler=cmd_claim)

    proof = subparsers.add_parser("ack-proof")
    proof.add_argument("--event", required=True)
    proof.add_argument("--turn", required=True)
    proof.set_defaults(handler=cmd_ack_proof)

    acknowledge = subparsers.add_parser("ack")
    acknowledge.add_argument("--event", required=True)
    acknowledge.add_argument("--ack-turn", required=True)
    acknowledge.add_argument("--ack-proof", required=True)
    acknowledge.add_argument("--reject")
    acknowledge.set_defaults(handler=cmd_ack)

    verdict = subparsers.add_parser("verdict")
    verdict.add_argument("--event", required=True)
    verdict.add_argument("--verdict", required=True,
                         choices=["verified", "needs_changes", "unverified", "aborted"])
    verdict.add_argument("--verdict-turn", required=True)
    verdict.add_argument("--criterion", action="append")
    verdict.add_argument(
        "--finding", action="append",
        help="id=disposition:note, repeatable. Disposition is verified, needs_changes or"
             " unverified, which is the contract's frozen enum.",
    )
    verdict.add_argument("--criteria", help="a JSON array of findings, or @path to one")
    verdict.add_argument("--reason", help="why an aborted or unverified verdict could not conclude")
    verdict.add_argument(
        "--expect-criteria-digest",
        help="refuse unless the canonical criteria set still has this digest",
    )
    verdict.set_defaults(handler=cmd_verdict)

    criteria_register = subparsers.add_parser("criteria-register")
    criteria_register.add_argument("--relationship", required=True)
    criteria_register.add_argument("--criterion", action="append", required=True,
                                   help="id=title, repeatable")
    criteria_register.add_argument("--optional", action="append",
                                   help="a criterion id that is not required for a verified verdict")
    criteria_register.add_argument("--source-ref",
                                   help="the issue or canonical document these came from")
    criteria_register.set_defaults(handler=cmd_criteria_register)

    criteria_show = subparsers.add_parser("criteria-show")
    criteria_show.add_argument("--relationship", required=True)
    criteria_show.set_defaults(handler=cmd_criteria_show)

    revision = subparsers.add_parser("revision-head")
    revision.add_argument("--relationship", required=True)
    revision.add_argument("--generation", type=int)
    revision.set_defaults(handler=cmd_revision_head)

    verify_acks = subparsers.add_parser("verify-acks")
    verify_acks.add_argument("--limit", type=int, default=8)
    verify_acks.set_defaults(handler=cmd_verify_acks)

    assignment_show = subparsers.add_parser("assignment-show")
    assignment_show.add_argument("--relationship")
    assignment_show.add_argument("--issue")
    assignment_show.set_defaults(handler=cmd_assignment_show)

    assignment_find = subparsers.add_parser("assignment-find")
    assignment_find.add_argument("--issue", required=True)
    assignment_find.set_defaults(handler=cmd_assignment_find)

    assignment_mark = subparsers.add_parser("assignment-mark")
    assignment_mark.add_argument("--relationship", required=True)
    assignment_mark.add_argument("--mark", required=True, choices=["merged"])
    assignment_mark.add_argument("--evidence", required=True)
    assignment_mark.add_argument("--actor", required=True)
    assignment_mark.add_argument(
        "--expected-event", required=True,
        help="the exact event id that was integrated; refused unless it is still the current"
             " verified revision",
    )
    assignment_mark.set_defaults(handler=cmd_assignment_mark)

    sync_target = subparsers.add_parser("sync-target")
    sync_target.add_argument("--relationship", required=True)
    sync_target.add_argument("--target", default="coordination_document")
    sync_target.add_argument("--target-ref", required=True)
    sync_target.set_defaults(handler=cmd_sync_target)

    sync_next = subparsers.add_parser("sync-next")
    sync_next.add_argument("--target")
    sync_next.add_argument("--limit", type=int, default=4)
    sync_next.set_defaults(handler=cmd_sync_next)

    sync_claim = subparsers.add_parser("sync-claim")
    sync_claim.add_argument("--sync", required=True)
    sync_claim.add_argument("--owner", required=True)
    sync_claim.set_defaults(handler=cmd_sync_claim)

    sync_operation = subparsers.add_parser("sync-operation")
    sync_operation.add_argument("--sync", required=True)
    sync_operation.set_defaults(handler=cmd_sync_operation)

    sync_reconcile = subparsers.add_parser("sync-reconcile")
    sync_reconcile.add_argument("--sync", required=True)
    sync_reconcile.add_argument("--observed", required=True, help="document text, or @path")
    sync_reconcile.set_defaults(handler=cmd_sync_reconcile)

    sync_complete = subparsers.add_parser("sync-complete")
    sync_complete.add_argument("--sync", required=True)
    sync_complete.add_argument("--claim-token", required=True)
    sync_complete.add_argument("--target-ref", required=True)
    sync_complete.add_argument("--readback", required=True, help="document text, or @path")
    sync_complete.add_argument("--external-ref")
    sync_complete.set_defaults(handler=cmd_sync_complete)

    sync_fail = subparsers.add_parser("sync-fail")
    sync_fail.add_argument("--sync", required=True)
    sync_fail.add_argument("--claim-token", required=True)
    sync_fail.add_argument("--error", required=True)
    sync_fail.set_defaults(handler=cmd_sync_fail)

    sync_retry = subparsers.add_parser("sync-retry")
    sync_retry.add_argument("--sync", required=True)
    sync_retry.set_defaults(handler=cmd_sync_retry)

    sync_status = subparsers.add_parser("sync-status")
    sync_status.add_argument("--relationship")
    sync_status.set_defaults(handler=cmd_sync_status)

    sync_progress = subparsers.add_parser("sync-progress")
    sync_progress.add_argument("--relationship", required=True)
    sync_progress.set_defaults(handler=cmd_sync_progress)

    show = subparsers.add_parser("show")
    show.add_argument("--event", required=True)
    show.add_argument("--message", action="store_true",
                      help="include the exact text a recipient was or would be sent")
    show.set_defaults(handler=cmd_show)

    snapshot = subparsers.add_parser("status")
    snapshot.add_argument("--relationship")
    snapshot.set_defaults(handler=cmd_status)

    daemon = subparsers.add_parser("daemon")
    daemon.add_argument("--max-ticks", type=int)
    daemon.add_argument("--deadline", type=float)
    daemon.set_defaults(handler=cmd_daemon)

    subparsers.add_parser("doctor").set_defaults(handler=cmd_doctor)
    return parser


def main(argv=None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    services = None
    try:
        services = Services(args)
        payload = args.handler(services, args)
        print(json.dumps(payload, indent=2, default=str))
        return EXIT_OK
    except RelayError as error:
        print(json.dumps({
            "error": "refused",
            "reason": error.reason.value if error.reason else None,
            "detail": error.detail,
        }, indent=2))
        return EXIT_REFUSED
    except SystemExit2 as error:
        print(json.dumps({"error": "usage", "detail": str(error)}, indent=2))
        return error.code
    except Exception as error:
        print(json.dumps({
            "error": "host", "detail": f"{type(error).__name__}: {error}"
        }, indent=2))
        return EXIT_HOST
    finally:
        if services is not None:
            services.close()


if __name__ == "__main__":
    sys.exit(main())
