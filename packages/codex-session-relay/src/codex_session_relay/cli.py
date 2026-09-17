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
from .store import (
    Store, canonical_socket, compare_store, nonce_lookup, probe, resolve_state_dir,
    state_dir, store_socket,
)
from .sync import SyncOutbox, render_progress_summary

EXIT_OK, EXIT_REFUSED, EXIT_HOST, EXIT_USAGE = 0, 2, 3, 4

# Which commands need to reach the App Server, and which do not. Reported by doctor, because a
# caller should learn this from one command instead of from a failure halfway through.
HOST_REQUIRED_COMMANDS = (
    "daemon", "deliver", "reconcile", "recover", "service run", "service start",
    "service restart", "verify-acks",
)
OFFLINE_COMMANDS = (
    "ack", "ack-proof", "admit-turn", "assignment-show", "claim", "criteria-register",
    "criteria-show", "doctor", "emit", "generation-bind", "generation-open", "register",
    "relationship-resume", "relationship-status", "revision-head", "settings-record",
    "settings-show", "show", "status", "store-challenge", "store-identity", "verdict",
    "service status", "service enable", "service disable", "service stop",
)


class _LazyAdapter:
    """Stands in for the adapter so nothing is constructed until something actually uses it."""

    def __init__(self, services):
        self._services = services

    def __getattr__(self, name):
        return getattr(self._services.adapter, name)


class Services:
    """Everything a command might need, built only when the command actually needs it.

    Nothing is constructed here on purpose. doctor exists to describe a host where the store
    cannot be opened, and a Store built during construction opens the file O_RDWR, switches on
    WAL and runs the schema script - so it would raise before doctor could report why. Each
    dependency is cached after first use, so laziness never means two stores in one process.
    """

    def __init__(self, args):
        self.selection = resolve_state_dir(getattr(args, "state", None), args.socket)
        self.clock = SystemClock()
        self.socket_path = args.socket
        self.adapter_requested = bool(args.socket)
        self._store = None
        self._adapter = None
        self._criteria = None
        self._registry = None
        self._intake = None
        self._delivery = None
        self._ack = None
        self._reconciler = None
        self._sync = None
        self._assignments = None

    @property
    def state_directory(self):
        return self.selection.path

    @property
    def store(self):
        if self._store is None:
            # The socket travels with the store so a later invocation can find it by socket
            # rather than by the hash of the spelling that happened to create it.
            self._store = Store(self.selection.db_path, socket_path=self.socket_path)
        return self._store

    @property
    def criteria(self):
        if self._criteria is None:
            self._criteria = CriteriaService(self.store, self.clock)
        return self._criteria

    @property
    def registry(self):
        if self._registry is None:
            self._registry = Registry(self.store, self.clock)
        return self._registry

    @property
    def intake(self):
        if self._intake is None:
            self._intake = ReceiptIntake(
                self.store, self.registry, self.clock,
                admission=AnchorOrExplicit(
                    _LazyAdapter(self) if self.adapter_requested else None
                ),
            )
        return self._intake

    @property
    def delivery(self):
        if self._delivery is None:
            self._delivery = DeliveryService(
                self.store, self.registry, self.intake, self.clock
            )
        return self._delivery

    @property
    def ack(self):
        if self._ack is None:
            service = AckService(
                self.store, self.registry, self.intake, self.delivery, self.clock,
                criteria=self.criteria,
            )
            # Wired BEFORE the service is published. record_verdict skips its outbox
            # obligation when sync is absent, so an ack handed out unwired would drop the
            # obligation silently rather than fail.
            service.sync = self.sync
            self._ack = service
        return self._ack

    @property
    def reconciler(self):
        if self._reconciler is None:
            self._reconciler = Reconciler(
                self.store, self.registry, self.delivery, self.clock
            )
        return self._reconciler

    @property
    def sync(self):
        if self._sync is None:
            self._sync = SyncOutbox(self.store, self.clock)
        return self._sync

    @property
    def assignments(self):
        if self._assignments is None:
            self._assignments = AssignmentView(
                self.store, self.registry, self.clock, criteria=self.criteria
            )
        return self._assignments

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
        # Only a store that was actually built is closed. Reading the property here would
        # construct one during teardown, on the very host where constructing it fails.
        if self._store is not None:
            self._store.close()
            self._store = None


def _require_adapter(services):
    if not services.adapter_requested:
        raise SystemExit2("this command needs --socket to reach the host", EXIT_USAGE)


class SystemExit2(Exception):
    def __init__(self, message, code):
        super().__init__(message)
        self.code = code


class PayloadExit(Exception):
    """A completed answer that is still a refusal.

    doctor has to print its whole diagnosis AND exit non-zero when it cannot prove two
    participants share a store. A plain refusal would throw the diagnosis away, and a plain
    return would let exit 0 be read as yes.
    """

    def __init__(self, payload, code):
        super().__init__(payload.get("detail", "refused"))
        self.payload = payload
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
    if stored.get("_stage") == "final":
        # Whatever this event replaces stops being current the moment this one is final, and
        # that is true whether or not anyone asked to deliver THIS one. --no-enqueue skips
        # the queue, and the annotation used to ride on it, so a predecessor already in
        # flight kept being reported as the current delivery.
        services.delivery.annotate_predecessors(event)
        # Only when there is no delivery row yet. Acceptance and enqueue are separate
        # transactions here, so a receipt whose enqueue failed is retried to reach this line -
        # and a receipt that was already queued must not be queued twice for having been
        # re-emitted.
        if not args.no_enqueue and services.delivery.find(event) is None:
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
        # Every route to dispatched binds its anchor, not only the daemon's own.
        services.ack.bind_pending_anchors()
        return {"attempt": record}
    out = []
    # per_parent_limit is the TICK's fairness share, and an operator asking for --limit 20 is
    # not running a tick: capping each parent at two made a bulk deliver quietly send two.
    # The share still governs the daemon. Fairness across parents is unaffected, because
    # eligible() deals the rows one parent at a time whatever the per-parent window is.
    for row in services.delivery.eligible(
        now=services.clock.now(), limit=args.limit, per_parent_limit=args.limit,
    ):
        out.append(services.delivery.attempt(row["event_id"], services.adapter))
    # The bulk path dispatches revisions too, so it binds for exactly the same reason the
    # single-event path does.
    services.ack.bind_pending_anchors()
    return {"attempts": out}


def cmd_reconcile(services, args) -> dict:
    _require_adapter(services)
    outcome = services.reconciler.reconcile_attempt(args.request_id, services.adapter)
    services.ack.bind_pending_anchors()
    return outcome


def cmd_recover(services, args) -> dict:
    _require_adapter(services)
    outcome = services.reconciler.recover_on_start(services.adapter)
    outcome["anchorsBound"] = services.ack.bind_pending_anchors()
    return outcome


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
    from .report import read as read_work_report, read_all as read_work_reports

    payload["workReport"] = read_work_report(services.store, args.event)
    # Every submission, because an earlier message may have elided part of its report and
    # sent its recipient here for the rest.
    payload["workReportSubmissions"] = read_work_reports(services.store, args.event)
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
    payload = services.delivery.snapshot(relationship_id=args.relationship)
    # Scoped with the deliveries. A global health block beside a filtered list invites
    # reading another assignment's backlog as this one's.
    payload["observation"] = services.delivery.observation_health(
        relationship_id=args.relationship,
    )
    return payload


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
    # A bounded daemon run is an explicit operator action, so it does NOT require the managed
    # service's enable intent - but it does take the same scope claim, or two standalone runs
    # with different state directories could serve one App Server and never see each other.
    return _run_bounded(services, _service_for(services), args, require_intent=False)


def cmd_store_identity(services, args) -> dict:
    """One line each participant can emit, for a comparison to consume."""
    return {"stateSelection": services.selection.to_record(), "store": services.store.locate()}


def cmd_store_challenge(services, args) -> dict:
    """Write a nonce here, or look for one another participant wrote.

    This is the only evidence that survives a copied database: the identifier inside a copy is
    identical, but a value written AFTER the copy exists in exactly one of the two files.
    """
    if args.write:
        return services.store.write_challenge(actor=args.actor or "cli")
    if not args.read:
        raise SystemExit2("store-challenge needs --write or --read <nonce>", EXIT_USAGE)
    return services.store.read_challenge(args.read)


def _ledger_location(services) -> dict:
    """Where the transport ledger will actually live, which --state does not move.

    bridge_adapter._build resolves it with state_dir(socket_path), reading the environment
    only, so a run that overrides --state alone splits the relay store from the ledger that
    carries send idempotency. Mirrors codex_thread_bridge.ledger.open_endpoint_ledger, which
    cannot be called here because opening it is a side effect.
    """
    import hashlib

    if not services.socket_path:
        return {"configured": False, "directory": None, "path": None, "split": False}
    directory = Path(state_dir(services.socket_path)).expanduser()
    canonical = Path(services.socket_path).expanduser().absolute().resolve()
    endpoint = hashlib.sha256(str(canonical).encode()).hexdigest()[:16]
    split = directory.resolve() != services.selection.path.resolve()
    return {
        "configured": True, "directory": str(directory),
        "path": str(directory / f"operations-{endpoint}.sqlite3"), "split": split,
    }


def _contents(services, report) -> dict:
    """Counts, but only when the store can actually be opened for them."""
    from .store import read_only_rows

    if not report["access"]["dbReadable"]:
        return {"available": False, "relationships": None, "openAttempts": None,
                "detail": "the database is not readable from this process"}
    # Read through the probe's own read-only connection. services.store would construct a
    # Store, and Store.__init__ opens O_RDWR, switches on WAL and runs the whole schema
    # script - so asking doctor to COUNT rows in an empty, legacy or unrelated readable
    # relay.sqlite3 quietly turned it into a relay database. Diagnosis writes nothing.
    counted = read_only_rows(
        services.selection,
        "SELECT (SELECT COUNT(*) FROM relationships) AS relationships,"
        "       (SELECT COUNT(*) FROM attempts a"
        "          JOIN deliveries d ON d.event_id = a.event_id"
        "         WHERE a.internal_state = 'in_flight'"
        "            OR (a.state = 'held_uncertain'"
        "                AND d.state IN ('held_uncertain','sending'))) AS open_attempts",
    )
    if not counted["readable"] or counted["detail"] or not counted["rows"]:
        return {"available": False, "relationships": None, "openAttempts": None,
                "detail": counted["detail"] or "the store could not be read"}
    relationships = counted["rows"][0]["relationships"]
    open_attempts = counted["rows"][0]["open_attempts"]
    return {"available": True, "relationships": relationships,
            "openAttempts": open_attempts, "detail": None}


def _sibling_stores(services) -> dict:
    """Other stores beside this one that never recorded which socket they serve.

    A store is matched to a socket by provenance it records for itself. One created before
    that existed can only be matched by its directory hash, so if this command is about to
    create a fresh canonical database next to such a store, it may be hiding real data. That
    is reported rather than resolved: adopting on a guess is how the wrong store gets served.
    """
    from .store import stores_without_provenance

    root = services.selection.path.parent
    if services.selection.source in ("flag", "env"):
        # An explicit directory was chosen by a caller who already decided which participants
        # share it, so its neighbours are not candidates for anything.
        return {"checked": False, "reason": "the state directory was chosen explicitly",
                "withoutProvenance": []}
    without = stores_without_provenance(root, skip=services.selection.path.name)
    from .store import stores_claiming_socket

    # More than one store recording this socket is an ambiguity discovery refuses to resolve,
    # so it has to be visible here or a caller just gets a surprisingly empty database.
    claiming = stores_claiming_socket(
        root, services.socket_path, skip=services.selection.path.name,
    )
    return {"checked": True, "reason": None, "withoutProvenance": without,
            "claimingThisSocket": claiming,
            "ambiguous": len(claiming) > 1}


def cmd_doctor(services, args) -> dict:
    """What THIS process can actually do here, measured rather than assumed.

    Constructs no Store: probe() answers from stat, a read-only connection and a rolled-back
    write transaction, so a missing, unreadable or read-only state directory is an answer
    instead of the failure that would otherwise replace it.
    """
    import os

    report = probe(services.selection)
    report["procAvailable"] = os.path.isdir("/proc/self/fd")
    report["adapter"] = (
        "bridge" if services.adapter_requested else "none (read-only, no --socket)"
    )
    report["ledger"] = _ledger_location(services)
    report["actorReachability"] = _reachability(services, report)
    report["contents"] = _contents(services, report)
    report["siblingStores"] = _sibling_stores(services)

    nonce = nonce_lookup(services.selection, args.expect_nonce) if args.expect_nonce else None
    report["nonce"] = nonce
    comparison = compare_store(
        report["store"], expect_store=args.expect_store, expect_inode=args.expect_inode,
        nonce=nonce,
    )
    asked = any((args.expect_store, args.expect_inode, args.expect_nonce))
    report.update(comparison)
    if asked and comparison["sameStore"] != "proven":
        # A caller that asked whether this is the same store and got no proof must not read
        # exit 0 as yes. Unproven is refused for the same reason a mismatch is: the criterion
        # is that a different database is never reported as healthy.
        raise PayloadExit(report, EXIT_REFUSED)
    return report


def _service_for(services):
    """Built from the probe, so status stays an offline command that constructs no Store."""
    from .service import RelayService

    return RelayService(
        services.selection, socket_path=services.socket_path,
        store_id=probe(services.selection)["store"]["storeId"],
    )


def _refuse_unless_ok(payload: dict) -> dict:
    if payload.get("ok"):
        return payload
    raise PayloadExit(payload, EXIT_REFUSED)


def _run_bounded(services, service, args, *, require_intent: bool) -> dict:
    """Hold ownership for exactly as long as this process serves, then let it go.

    The daemon is constructed INSIDE the claim so a run that loses the race never opens a
    transport connection it is about to abandon.
    """
    from .daemon import RelayDaemon
    from .service import ServiceRefused, owned_service

    deadline = services.clock.now() + args.deadline if args.deadline else None
    allow_isolated = getattr(args, "allow_isolated_scope", False)
    # Before the claim, for the same reason _supervise does it: the probe that built this
    # service answers from a file that may not exist yet, and a scope registration recorded
    # with a null store id can later be overwritten by a different store.
    service.store_id = services.store.identity
    adopted = _adopt_supervised(service, args)
    try:
        with owned_service(
            service, allow_isolated=allow_isolated, require_intent=require_intent,
            adopt_lock_fd=adopted.get("lockFd"), adopt_scope_fd=adopted.get("scopeFd"),
        ) as record:
            daemon = RelayDaemon(
                services.store, services.registry, services.intake, services.delivery,
                services.ack, services.reconciler, services.adapter, clock=services.clock,
            )
            reports = daemon.run(
                max_ticks=args.max_ticks, deadline=deadline,
                sleep=_scheduler_wait(services.clock, deadline),
            )
    except ServiceRefused as refusal:
        raise PayloadExit(
            {"ok": False, "reason": refusal.reason, "detail": refusal.detail}, EXIT_REFUSED,
        ) from refusal
    return {"ok": True, "reason": None, "pid": record["pid"],
            "ticks": [report.as_dict() for report in reports]}


def _adopt_supervised(service, args) -> dict:
    """Validate a supervised invocation, or refuse it. Never fall back to an unlocked run.

    An fstat match proves the descriptor points at the right FILE, not that it shares the
    supervisor's open file description - an independently opened descriptor for the same path
    passes it. The token recorded in daemon.json and the recorded-parent check are what
    actually establish that this process was launched by that supervisor.
    """
    from .service import DAEMON_LOCK, arm_parent_death_signal

    import os

    token = getattr(args, "supervised_token", None)
    lock_fd = getattr(args, "supervised_lock_fd", None)
    scope_fd = getattr(args, "supervised_scope_fd", None)
    supplied = [value for value in (token, lock_fd, scope_fd) if value is not None]
    if not supplied:
        return {}

    def refuse(reason, detail):
        raise PayloadExit(
            {"ok": False, "reason": reason, "detail": detail}, EXIT_REFUSED,
        )

    if token is None or lock_fd is None or scope_fd is None:
        refuse("supervised_invocation_incomplete",
               "a supervised worker needs the token and both descriptors together")
    record = service.record() or {}
    if not record.get("token") or record["token"] != token:
        refuse("supervised_token_mismatch", "the token does not match this state directory")
    if record.get("stateDir") not in (None, str(service.selection.path)):
        refuse("supervised_state_mismatch", "the record names a different state directory")
    if record.get("socketPath") not in (None, service.socket_path):
        refuse("supervised_socket_mismatch", "the record names a different operating scope")
    # The supervisor recorded which store it registered the scope for. This worker opened
    # whatever relay.sqlite3 the path resolves to NOW, and a database deleted or atomically
    # replaced between segments is a different one - so without this the worker would serve an
    # empty or unrelated store while the supervisor and the scope registration still name the
    # original, and every participant comparing identities would be told they agree.
    if record.get("storeId") not in (None, service.store_id):
        refuse("supervised_store_mismatch",
               "the record names a different store than this worker opened")
    try:
        want = os.stat(service.selection.path / DAEMON_LOCK)
        got = os.fstat(lock_fd)
    except OSError as error:
        refuse("supervised_fd_unreadable", f"{type(error).__name__}: {error}")
    if (got.st_dev, got.st_ino) != (want.st_dev, want.st_ino):
        refuse("supervised_fd_mismatch", "the inherited descriptor is not this daemon lock")
    death = arm_parent_death_signal(record.get("pid") or -1)
    if death["orphaned"]:
        refuse("supervisor_already_gone",
               f"parent is {death['parent']}, not the recorded supervisor {record.get('pid')}")
    if not death["armed"]:
        # Recorded rather than refused. The getppid check above closes the window that
        # matters here - a supervisor that is ALREADY gone - and refusing outright would make
        # the relay unusable on any host without prctl. What is lost is the later case: if
        # the supervisor crashes mid-segment the kernel will not signal this worker, so it
        # runs to the end of its bounded segment holding the inherited locks. Bounded, but
        # real, and an operator can see it in the record instead of assuming it is armed.
        # Appended to the log rather than written into daemon.json: the supervisor owns that
        # record and rewrites it at every worker boundary, so a whole-document write from the
        # worker would race it and could erase the workerPid a stop needs.
        service.store_journal_note(
            f"worker {os.getpid()} could not arm PR_SET_PDEATHSIG"
            f" ({death.get('detail') or 'no detail'}); if the supervisor crashes this worker"
            " runs to the end of its segment holding the inherited locks"
        )
    return {"lockFd": lock_fd, "scopeFd": scope_fd if scope_fd >= 0 else None,
            "parentDeathSignal": "armed" if death["armed"] else "unarmed"}


def cmd_service(services, args) -> dict:
    service = _service_for(services)
    action = args.service_command
    if action == "status":
        return service.status()
    if action == "enable":
        # Through the same refusal path as every other mutating service command: returning
        # the payload directly exits zero, and automation would read a refused enable that
        # deliberately changed nothing as a success.
        return _refuse_unless_ok(service.enable(actor=args.actor or "cli"))
    if action == "disable":
        return _refuse_unless_ok(service.disable(actor=args.actor or "cli"))
    if action == "stop":
        return _refuse_unless_ok(service.stop(actor=args.actor or "cli"))
    if action in ("start", "restart"):
        _require_adapter(services)
        call = service.start if action == "start" else service.restart
        return _refuse_unless_ok(call(
            allow_isolated=args.allow_isolated_scope, deadline=args.deadline,
            segment_seconds=args.segment_seconds, max_segments=args.max_segments,
            actor=args.actor or "cli", takeover=getattr(args, "takeover_scope", False),
        ))
    if action == "run":
        _require_adapter(services)
        return _supervise(services, service, args)
    raise SystemExit2(f"unknown service action {action!r}", EXIT_USAGE)


def _supervise(services, service, args) -> dict:
    """The supervisor: it holds the locks and replaces bounded workers."""
    from .service import ServiceRefused

    service.launch_id = getattr(args, "launch_id", None)
    # This process is the one that claims the scope, so the flag has to be honoured here and
    # not only in the parent that decided to pass it.
    service.takeover = getattr(args, "takeover_scope", False)
    # The probe that built this service answers from a file that may not exist yet, so on a
    # fresh state directory it reports no store id at all. Opening the store HERE is not the
    # side effect doctor and status refuse: a supervisor is about to use it either way. It
    # matters because the scope registration is written next, and ScopeRegistry's mismatch
    # guard needs both ids to be present - a registration recorded with a null id could be
    # overwritten later by a different store, losing the evidence two stores served one socket.
    service.store_id = services.store.identity

    def recover():
        # Establish what happened to anything in flight BEFORE a worker can send. Recovery
        # itself sends nothing; it only decides what the evidence supports.
        services.reconciler.recover_on_start(services.adapter)
        _release_expired_leases(services)

    try:
        return service.supervise(
            allow_isolated=args.allow_isolated_scope, segment_seconds=args.segment_seconds,
            max_segments=args.max_segments, deadline=args.deadline, on_start=recover,
        )
    except ServiceRefused as refusal:
        raise PayloadExit(
            {"ok": False, "reason": refusal.reason, "detail": refusal.detail}, EXIT_REFUSED,
        ) from refusal


def _release_expired_leases(services) -> None:
    """An expired lease returns the attempt to reconciliation, never to the send queue.

    A sending row whose lease ran out may already have reached the recipient, so putting it
    back to queued would make it eligible to send again on no evidence at all. held_uncertain
    is where the reconciler can judge it (I-78).
    """
    now = services.clock.now()
    with services.store.transaction() as db:
        db.execute(
            "UPDATE deliveries SET state = 'held_uncertain', lease_owner = NULL,"
            " lease_until = NULL, updated_at = ?"
            " WHERE state = 'sending' AND lease_until IS NOT NULL AND lease_until <= ?",
            (services.clock.iso(), now),
        )


def _reachability(services, report) -> dict:
    """What THIS process can actually do here, measured rather than assumed.

    A workspace-write task cannot write the default state directory and cannot connect to the
    App Server control socket. The honest answer for it is the offline subset plus a deferred
    acknowledgement, and it should learn that from one command rather than from a failure
    halfway through a delivery. The probe is a plain connect: it builds no bridge and starts no
    thread, which is why doctor can answer even where the bridge itself could not load.
    """
    import socket

    # Reuses the probe's measurement rather than repeating it, so one command cannot report
    # two different answers about the same directory.
    access = report["access"]

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
        "stateDirectoryWritable": access["directoryWritable"],
        "stateDirectoryDetail": access["detail"],
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
    daemon.add_argument("--allow-isolated-scope", action="store_true")
    daemon.add_argument("--supervised-token")
    daemon.add_argument("--supervised-lock-fd", type=int)
    daemon.add_argument("--supervised-scope-fd", type=int)
    daemon.set_defaults(handler=cmd_daemon)

    service = subparsers.add_parser("service")
    actions = service.add_subparsers(dest="service_command", required=True)
    for name in ("status", "enable", "disable", "stop"):
        offline = actions.add_parser(name)
        offline.add_argument("--actor")
    for name in ("start", "restart", "run"):
        hosted = actions.add_parser(name)
        hosted.add_argument("--actor")
        hosted.add_argument("--allow-isolated-scope", action="store_true")
        # The WORKER's bound. The supervisor replaces workers; it is not itself bounded by
        # this, or the service would end after a single segment.
        hosted.add_argument("--segment-seconds", type=float)
        # The supervisor's own optional bounds, for a test or a deliberately finite run.
        hosted.add_argument("--max-segments", type=int)
        hosted.add_argument("--deadline", type=float)
        hosted.add_argument("--launch-id")
        # For a registration whose store no longer exists - deleted, lost or deliberately
        # replaced. Refused while anything is live on the scope, so this can only ever
        # replace a registration nothing is running behind.
        hosted.add_argument(
            "--takeover-scope", action="store_true",
            help="replace a stopped registration that names a store this one is not",
        )
    service.set_defaults(handler=cmd_service)

    doctor = subparsers.add_parser("doctor")
    doctor.add_argument("--expect-store", help="the store id another participant reported")
    doctor.add_argument("--expect-inode", help="the device:inode another participant reported")
    doctor.add_argument("--expect-nonce", help="a nonce another participant wrote here")
    doctor.set_defaults(handler=cmd_doctor)

    subparsers.add_parser("store-identity").set_defaults(handler=cmd_store_identity)

    challenge = subparsers.add_parser("store-challenge")
    challenge.add_argument("--write", action="store_true")
    challenge.add_argument("--read")
    challenge.add_argument("--actor")
    challenge.set_defaults(handler=cmd_store_challenge)
    return parser


def _quote(value) -> str:
    """Shell-safe, because these strings are printed to be pasted."""
    import shlex

    return shlex.quote(str(value))


def _without_state_env() -> str:
    """Drop the state pin from a line whose whole job is to discover by socket.

    Unconditional whenever the variable is set, because this prefix is only ever attached to
    a command carrying no --state of its own. Left in place, the variable would decide that
    command's answer instead of the socket, whichever rule happened to cause the refusal.

    Two narrower conditions were tried first and both were wrong in one direction or the
    other, which is why this one no longer decides anything: see _wrong_socket_recovery.
    """
    import os

    from .store import STATE_ENV

    return f"env -u {STATE_ENV} " if STATE_ENV in os.environ else ""


def _program() -> str:
    """How the operator invokes this CLI, so a printed command can be pasted.

    Derived from argv rather than hardcoded, because the console script and
    `python3 -m codex_session_relay.cli` are both ordinary ways to reach here and a recovery
    list that names the wrong one is a recovery list the operator has to translate.
    """
    import os
    import shlex
    import sys

    argv0 = sys.argv[0] or ""
    name = os.path.basename(argv0)
    if name in ("", "__main__.py", "cli.py", "-c"):
        # The running interpreter, not a bare python3. The relay may be under a virtualenv or
        # a versioned interpreter, and on a host where python3 is absent or resolves to a
        # DIFFERENT interpreter the printed line reaches another installation, or nothing.
        return f"{shlex.quote(sys.executable or 'python3')} -m codex_session_relay.cli"
    # The directory is kept when there is one. A console script that is not on PATH renders as
    # a bare name otherwise, and pasting that reaches a different installation or nothing.
    return shlex.quote(argv0 if os.path.dirname(argv0) else name)


def _recovery_commands(services, selection, contested: bool) -> list:
    """Complete commands for an operator who has only this refusal to work from.

    Every one of them READS. Provenance is recorded by opening a store with a socket, which
    is exactly what the refusal prevented, so inspection is safe to repeat and none of these
    adopts anything by running.

    The socket travels on each command deliberately. Dropping it would compare the candidates
    under different conditions from the ones that produced the refusal, and for the reason
    the doctor exemption exists in the first place: the socket is what makes two stores
    candidates for each other.
    """
    import shlex

    program = _program()
    # Quoted, every one of them. These are printed to be pasted, and a state directory or a
    # socket path containing shell syntax would otherwise be executed by the operator doing
    # exactly what the refusal told them to do.
    # Attached with '=' rather than a space, for a separate reason: a path may legitimately
    # begin with a dash, and argparse reads '--socket -odd.sock' as two options and fails
    # with "expected one argument". The '=' form keeps option and value one token.
    socket = (
        f" --socket={shlex.quote(str(services.socket_path))}"
        if services.socket_path else ""
    )
    lines = [
        f"{program}{socket} doctor",
        "  lists the candidates under siblingStores",
    ]
    for candidate in list(selection.ambiguous or selection.unidentified):
        quoted = shlex.quote(str(candidate))
        lines.append(f"{program} --state={quoted}{socket} doctor")
        lines.append(f"{program} --state={quoted}{socket} service status")
    lines.append(
        "  service status groups by project, so the candidate holding the assignments you"
        " expect is the one to keep"
    )
    if contested:
        # Said in the payload, not only in the docs. Choosing one of two claiming stores does
        # not retire the other, so the next default invocation is refused again and every
        # participant has to be given the same directory until one store is gone.
        lines.append(
            "  then pass --state=<the chosen directory> on EVERY participant of this"
            " assignment: both stores still record this socket, so default discovery keeps"
            " refusing until one of them is retired"
        )
    else:
        lines.append(
            f"  then pass --state={shlex.quote(str(selection.path))} once to create the new"
            " store deliberately, or --state=<the existing directory> to keep using it"
        )
    return lines


def _wrong_socket_recovery(selection, recorded, wanted) -> list:
    """Every candidate this refusal has, printed rather than chosen between.

    Two earlier versions tried to work out which store the operator meant. Dropping the state
    pin whenever it was set lost the environment store in a flag-caused refusal, where that
    store may be the one that records the requested socket. Dropping it only when the pin
    caused the refusal left it in place for `--state A` with the variable ALSO naming A,
    re-selecting the very store the refusal was about - and because doctor is exempt from this
    guard it then exits 0 under a caption claiming it found the requested socket's store.

    The CLI cannot tell those apart without opening the environment store, so it stops
    deciding. The socket-first line always runs unpinned, and a directory the environment
    names that this invocation did not use is printed beside it as its own line. The operator
    reads both; nothing here assumes which one is right.
    """
    import os
    from pathlib import Path

    from .store import STATE_ENV

    lines = [
        f"{_program()} --state={_quote(selection.path)}"
        f" --socket={_quote(recorded)} doctor",
        "  reads this store under the socket it actually records",
        f"{_without_state_env()}{_program()} --socket={_quote(wanted)} doctor",
        "  discovers by socket alone, ignoring any pinned directory",
    ]
    pinned = os.environ.get(STATE_ENV)
    if not pinned:
        return lines
    # Normalised the way resolve_state_dir normalises it, because that is what decides
    # whether these are the same directory at all. Compared as raw text, ~/relay-state and
    # /home/alice/relay-state look like two candidates, and the line this would add resolves
    # straight back to the store that caused the refusal: a dead end wearing the label of an
    # alternative. Printed resolved for the same reason - quoting ~ stops the shell expanding
    # it, so the pasted command would not mean what it reads.
    try:
        resolved = Path(pinned).expanduser().absolute()
    except RuntimeError:
        # ~someone whose home this host cannot resolve. The variable is never validated at
        # startup when --state overrides it, so this is the first thing that touches it - and
        # a refusal payload that becomes a traceback leaves the operator with nothing at all.
        # Said rather than dropped: it is the value they set, and it is not usable.
        lines.append(
            f"  {STATE_ENV} is set to {pinned!r}, which names a home directory that does not"
            " resolve on this host, so it is not offered as a candidate"
        )
        return lines
    if resolved != Path(selection.path).expanduser().absolute():
        lines.append(
            f"{_program()} --state={_quote(resolved)} --socket={_quote(wanted)} doctor"
        )
        lines.append(
            f"  reads the directory {STATE_ENV} names, which --state overrode on this run"
        )
    return lines

def _refuse_ambiguous_state(services, args) -> None:
    """Two stores already record this socket, so opening one of them would be a guess.

    Falling through to the canonical directory is not the neutral outcome it looks like. The
    first command that writes there creates a THIRD empty database, and once that exists it
    wins every later resolution and hides the assignments and pending deliveries in both of
    the others. Refusing costs one command; the third store costs the state.

    The same refusal covers a store that records NO socket. Its directory hash cannot be
    inverted, so if it is this socket's - created from a spelling we cannot reconstruct - then
    creating a canonical database beside it hides it just as permanently. That case fires only
    when a store would be created; an existing canonical store has already settled it.

    doctor and ack-proof are exempt for opposite reasons. doctor is how an operator finds out
    which store to pass to --state, so refusing it would remove the only way out. ack-proof is
    a derivation over its own two arguments that opens no store at all.

    An explicit --state or environment override never arrives here: both return from
    resolve_state_dir before any discovery runs, because a caller who named a directory has
    already decided which participants share it.

    This guard is on the command line rather than on Services.store. A library caller that
    builds Services itself bypasses it; every in-process caller in this package passes an
    explicit directory, and raising from a property would turn a diagnostic into a crash.
    """
    selection = services.selection
    # A store records the socket it serves, and the first recording wins so nothing rewrites
    # it silently. But an explicit --state or CODEX_SESSION_RELAY_STATE reused with a
    # DIFFERENT App Server is a real disagreement: the service would claim and serve the new
    # socket while the database goes on attributing itself to the old one, so assignments from
    # one App Server can be exposed through another and later discovery still matches the
    # store to the socket it no longer serves. Explicit selections reach this even though they
    # carry no discovery, because choosing a directory is not choosing what is already in it.
    if services.socket_path and selection.db_path.exists():
        recorded = store_socket(selection.db_path)
        wanted = canonical_socket(services.socket_path)
        if recorded is not None and recorded != wanted and (
            getattr(args, "handler", None) not in (cmd_doctor, cmd_ack_proof)
        ):
            raise PayloadExit({
                "error": "refused",
                "reason": "state_directory_serves_another_socket",
                "detail": (
                    "this store records a different App Server socket; serving the requested"
                    " one from it would expose one installation's assignments through another"
                ),
                "recordedSocket": recorded,
                "requestedSocket": wanted,
                "stateDirectory": str(selection.path),
                # Nothing here adopts anything. Using a store does not rewrite the socket it
                # recorded, so the fix is to point the command at the store that belongs to
                # this socket, or at the socket that belongs to this store.
                "recover": _wrong_socket_recovery(selection, recorded, wanted),
                "note": "using a store does not rewrite the socket it recorded, so neither"
                        " command here adopts anything; choose the matching pair",
            }, EXIT_REFUSED)
    if not (selection.ambiguous or selection.unidentified):
        return
    if getattr(args, "handler", None) in (cmd_doctor, cmd_ack_proof):
        return
    contested = bool(selection.ambiguous)
    raise PayloadExit({
        "error": "refused",
        "reason": ("ambiguous_state_directory" if contested
                   else "unidentified_state_directory"),
        "detail": (
            "more than one store already records this socket, and creating a new one here"
            " would hide them both"
        ) if contested else (
            "a store here records no socket, so it cannot be ruled out as this one's;"
            " creating a new store beside it would hide it permanently"
        ),
        "socketPath": services.socket_path,
        "candidates": list(selection.ambiguous or selection.unidentified),
        "wouldHaveCreated": str(selection.db_path),
        # Complete commands, carrying the socket. An operator has only this payload to work
        # from, and every one of these reads a store without recording anything, so they are
        # safe to repeat: provenance is written by opening a store, which is what the
        # refusal prevented.
        "recover": _recovery_commands(services, selection, contested),
    }, EXIT_REFUSED)


def main(argv=None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    services = None
    try:
        services = Services(args)
        _refuse_ambiguous_state(services, args)
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
    except PayloadExit as error:
        print(json.dumps(error.payload, indent=2, default=str))
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
