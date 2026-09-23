"""The command surface. Every command is JSON-out and exit-coded, so a caller never parses prose.

Exit codes: 0 success, 2 a refusal carrying a machine-readable reason, 3 a host problem, 4 usage.

One thing is deliberately absent. There is no flag that makes the relay compute an acknowledgement
proof: --ack-proof is required, because a proof the relay produced would prove nothing about who
sent it. The separate ack-proof command exists so a parent can compute the value from its OWN turn
id, which is a different act entirely.
"""

import argparse
import json
import math
import sys
import time
from pathlib import Path

from .ack import AckService
from .admission import AnchorOrExplicit, admit_explicitly
from .assignment import AssignmentView
from .clock import SystemClock
from .criteria import CriteriaService, finding_id
from .currency import head_revision
from .delivery import COMPLETION, DeliveryService
from .errors import RelayError
from . import envelope, guard, intent, marker, packets, receiver, restoration, rolepolicy
from .identity import ack_proof as derive_ack_proof
from .manifest import build as build_manifest, freeze as freeze_manifest, revision_hash
from .models import Endpoint, TurnRef
from .receipts import ReceiptIntake, contract_record
from .reconcile import Reconciler
from .registry import (
    CLEAR_EXCEPTION,
    Registry,
    contract_record as relationship_record,
    record_settings,
)
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
    "service restart", "verify-acks", "managed-start",
    # A report upward resumes the supervisor's thread, and a readback is checked against the
    # host's own turn list and the recipient's transcript. Neither can answer without one.
    "supervisor-send", "supervisor-read",
)
# Every command that touches the managed marker and no store this invocation would have to select.
# intent-claim and intent-disposition also record into the store the INTENT names, after the marker
# write (declarations.py), which is a store the coordinator chose rather than one discovery guessed.
# Listed once so the store-selection refusal and the doctor reachability report cannot drift apart.
MARKER_COMMANDS_BY_NAME = (
    "intent-declare", "intent-attempt", "intent-bind", "intent-register", "intent-claim",
    "intent-disposition", "intent-resolve", "intent-show", "guard-evaluate",
)

OFFLINE_COMMANDS = (
    "ack", "ack-proof", "admit-turn", "assignment-show", "claim", "criteria-register",
    "criteria-show", "doctor", "emit", "generation-bind", "generation-open", "register",
    "relationship-resume", "relationship-status", "revision-head", "settings-record",
    "settings-show", "show", "status", "store-challenge", "store-identity", "verdict",
    # The linkage surface reads and writes the store and never calls the host, so every one of
    # these works without an App Server. Leaving them out made doctor under-report what an
    # operator can actually run offline.
    "linkage-attach", "linkage-bind", "linkage-counterpart", "linkage-directive",
    "linkage-completion", "linkage-down", "linkage-handover", "linkage-outstanding",
    "linkage-peer",
    "linkage-settle", "linkage-supervise", "linkage-up",
    # The fault ledger reads and writes this store and never calls the host. The Linear write
    # it queues is performed elsewhere by a credential holder, which is a different question
    # from whether these commands need an App Server: they do not.
    "fault-target", "fault-observe", "fault-sweep", "fault-show", "fault-fix",
    "fault-reverify", "fault-resolve", "fault-next", "fault-claim", "fault-operation",
    "fault-reconcile", "fault-complete", "fault-fail", "fault-retry", "fault-prune",
    "service status", "service enable", "service disable", "service stop",
    # Records which execution policy file this service's daemon is launched with. It writes
    # one small file next to the intent and reaches no host, so an operator can configure a
    # service before anything is running.
    "service declare",
    # Coordination between parents. Like the linkage surface these read and write the store
    # and never call the host, so an operator can run every one of them with no App Server.
    # Read-only, offline, and constructs no Store at all.
    "dispositions-show", "managed-show", "managed-release", "reporting-show",
    "capacity-show", "limit-declare", "merge-turn-attest", "merge-turn-check",
    "merge-turn-land", "merge-turn-ready", "merge-turn-release", "merge-turn-request",
    "merge-turn-request-return", "merge-turn-resolve", "merge-turn-show",
    "merge-turn-unknown", "merge-turn-withdraw", "merge-turn-acknowledge",
    "region-followup",
    "region-followup-accept", "region-followup-settle", "region-propose",
    "region-reaffirm", "region-restate-revision", "region-settle", "region-show",
    "slot-release", "slot-reserve", "usage-observe",
    # What is owed upward, read and recorded in this store alone. supervisor-select and
    # supervisor-standing only read, and supervisor-report-recorded writes one journal row;
    # none of the three reaches the host, so leaving them out under-reported what an operator
    # can run with no App Server.
    "supervisor-select", "supervisor-standing", "supervisor-report-recorded",
    # The reading the automatic pass derives an omission from, read from this store alone.
    "reporting-derive",
    # Staging writes the message and the journal entry for it and reaches no host, and
    # showing one reads the rows back. Sending and reading back are in the host list above.
    "supervisor-stage", "supervisor-show",
    # Compares a packet against the receiver's own reading. With --receiver it reads the
    # store read-only (never creating one) and writes only the receiver's own ledger file; with
    # --record it reads a supplied file. Neither reaches a host or decides anything about
    # delivery.
    "packet-check",
    # Reaches a forge and never the App Server, and constructs no Store at all.
    "merge-evidence",
) + MARKER_COMMANDS_BY_NAME


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
        self._pin_adapter_ledger = False
        self._store = None
        self._adapter = None
        self._criteria = None
        self._registry = None
        self._intake = None
        self._delivery = None
        self._ack = None
        self._reconciler = None
        self._sync = None
        self._faults = None
        self._assignments = None
        self._linkage = None
        self._merge_turn = None
        self._capacity = None
        self._edit_regions = None
        self._supervisor_channel = None

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
    def linkage(self):
        if self._linkage is None:
            from .linkage import Linkage

            self._linkage = Linkage(self.store, self.clock)
        return self._linkage

    @property
    def merge_turn(self):
        if self._merge_turn is None:
            from .mergeturn import MergeTurn

            # With the delivery service, so a promotion can hand the freed target's grant to
            # the relay's queue in the same transaction. This is the only construction of
            # MergeTurn outside the tests, so leaving it out would have made the wake path
            # unreachable everywhere it actually matters while every test still passed.
            self._merge_turn = MergeTurn(
                self.store, self.clock, self.linkage, delivery=self.delivery)
        return self._merge_turn

    @property
    def capacity(self):
        if self._capacity is None:
            from .capacity import Capacity

            self._capacity = Capacity(self.store, self.clock, self.linkage)
        return self._capacity

    @property
    def edit_regions(self):
        if self._edit_regions is None:
            from .editregion import EditRegions

            self._edit_regions = EditRegions(self.store, self.clock, self.linkage)
        return self._edit_regions

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
    def supervisor_channel(self):
        """What a parent owes the level above, staged, sent and read back.

        The linkage is passed because who supervises a project lives nowhere else: unlike a
        delivery, this channel has no frozen row to fall back on and must not acquire one.
        """
        if self._supervisor_channel is None:
            from .supervisorchannel import SupervisorChannel

            self._supervisor_channel = SupervisorChannel(
                self.store, self.registry, self.linkage, self.clock,
                # What every line the channel writes for a later step selects: this
                # invocation's store, and the host this invocation reaches.
                state_directory=self.state_directory, socket_path=self.socket_path)
        return self._supervisor_channel

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
    def faults(self):
        if self._faults is None:
            from .faults import FaultLedger

            self._faults = FaultLedger(self.store, self.clock)
        return self._faults

    @property
    def assignments(self):
        if self._assignments is None:
            self._assignments = AssignmentView(
                self.store, self.registry, self.clock, criteria=self.criteria,
                # Without this every production project_state() answered "unreadable" without
                # looking at a single assignment, so the reading existed only in its own tests.
                linkage=self.linkage,
            )
        return self._assignments

    @property
    def adapter(self):
        """Built on first real use, so requiring one and probing for one are different acts."""
        if self._adapter is None and self.adapter_requested:
            from .bridge_adapter import BridgeHostAdapter

            options = {}
            if self._pin_adapter_ledger:
                options["ledger_directory"] = self.selection.path
            self._adapter = BridgeHostAdapter(
                self.socket_path, store=self.store, clock=self.clock, **options,
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


def cmd_managed_start(services, args) -> dict:
    from .managed import ManagedStart, parse_request, MAX_REQUEST_BYTES

    if not services.socket_path or services.selection.source != "flag":
        raise SystemExit2("managed-start requires explicit --state and --socket", EXIT_USAGE)
    # This command alone pins the transport ledger to the selected store directory.
    # Every other command keeps resolving the ledger from the environment.
    services._pin_adapter_ledger = True
    try:
        if args.request.startswith("@"):
            with open(args.request[1:], "rb") as handle:
                raw = handle.read(MAX_REQUEST_BYTES + 1)
        else:
            raw = args.request.encode("utf-8")
        if len(raw) > MAX_REQUEST_BYTES:
            raise ValueError("managed request exceeds the byte limit")
        request = parse_request(json.loads(raw))
    except (ValueError, TypeError, OSError) as error:
        raise SystemExit2(str(error), EXIT_USAGE) from error
    # Read worker evidence without creating a missing store. A missing service must
    # fail before reservation or task creation, and diagnostics stay available.
    service = _service_for(services)
    requirements = [{"role": role, "model": request[role]["settings"]["model"],
                     "reasoningEffort": request[role]["settings"]["reasoningEffort"]}
                    for role in ("parent", "child")]
    readiness = rolepolicy.worker_readiness(service.read_worker_policy(), requirements)
    if not readiness["ready"]:
        raise PayloadExit({"schema": "managed-start/1", "state": "refused", "stage": "preflight",
                           "requestId": request["requestId"], "reason": readiness["reason"]}, EXIT_REFUSED)
    result = ManagedStart(services.store, services.clock, services.adapter,
                          service.read_worker_policy, socket=services.socket_path,
                          marker_root=args.marker_root, state_selector=args.state).run(request)
    if result["state"] != "admitted":
        raise PayloadExit(result, EXIT_REFUSED)
    return result


def cmd_managed_show(services, args) -> dict:
    from .store import read_only_rows

    observed = read_only_rows(services.selection,
        "SELECT r.*, (SELECT detail FROM journal WHERE kind='managed_start_observed'"
        " AND subject=r.request_id ORDER BY rowid DESC LIMIT 1) AS observation"
        " FROM managed_start_requests r WHERE request_id=?", (args.request_id,))
    row = observed["rows"][0] if observed["rows"] else None
    last = row.pop("observation") if row else None
    return {"request": row, "lastObservation": json.loads(last) if last else None,
            "readable": observed["readable"], "detail": observed["detail"]}


def cmd_managed_release(services, args) -> dict:
    return services.registry.release_unstarted(args.request_id, args.fingerprint,
                                               args.revision, args.reason)


def cmd_reporting_show(services, args) -> dict:
    """Read one exact turn's reporting observation and write nothing.

    The projection owns the diagnosis. This command only checks that every selector was named,
    refuses a store it would have to guess, and prints the completed observation, including an
    unmeasured one. A missing store stays missing: omitted.observe is given the selection and
    never a Store.
    """
    if services.selection.source != "flag":
        raise SystemExit2("reporting-show requires explicit --state", EXIT_USAGE)
    if services.socket_path:
        raise SystemExit2("reporting-show does not take --socket", EXIT_USAGE)
    from . import omitted

    try:
        return omitted.observe(
            services.selection,
            root=args.marker_root,
            workspace=args.workspace,
            assignment=args.assignment,
            session=args.session,
            turn=args.turn,
            now=services.clock.iso(),
        )
    except ValueError as error:
        raise SystemExit2(str(error), EXIT_USAGE) from error


def cmd_reporting_derive(services, args) -> dict:
    """The reading this store alone gives for one relationship's turn, written nowhere.

    What the automatic supervisor pass stages an omission from, and what supervisor-show prints
    as the recheck of one it staged: omitted.derive, through the same predicate reporting-show
    uses, over the declarations the child's relay recorded in this store. --grace defaults to
    the pass's own, so this answers what the pass would.
    """
    from . import omitted

    try:
        services.selection.db_path.stat()
    except FileNotFoundError:
        # Opening Services.store would create and migrate an empty store here, turning the
        # absence this read should report into a database that says nothing happened. Only
        # absence answers here; a store that cannot be looked at fails where it is opened.
        return {"schema": omitted.SCHEMA, "source": omitted.STORE_SOURCE,
                "reportingState": "unmeasured", "reason": "store_absent",
                "relationshipId": args.relationship, "observedAt": services.clock.iso(),
                "owed": False, "owedReason": omitted.NOT_AN_OMISSION,
                "detail": "no relay store exists at " + str(services.selection.db_path)
                          + "; nothing was created"}
    policy = services.supervisor_channel.policy
    return omitted.derive(
        services.store, args.relationship, state_directory=services.state_directory,
        now=services.clock.iso(),
        grace=policy.omission_grace_seconds if args.grace is None else args.grace,
        turn=args.turn)


def cmd_register(services, args) -> dict:
    # Validated BEFORE anything is written. registry.register commits the relationship and, with
    # --project, the child's binding; a settings refusal raised after that left a live wrong-role
    # assignment behind and reported only the refusal. This command is the one caller holding
    # both halves in its own arguments, so it can answer the question while there is still
    # nothing to unwind. The checks inside record_settings and binding_plan remain for callers
    # that arrive separately; neither of those can undo a write the other already committed.
    #
    # Validation cannot reach the OTHER way this command left half a registration behind. A
    # worker killed between the two commits, a disk refusing, COMMIT itself failing: those
    # arrive between the writes rather than before them, and the relationship was already
    # durable when they did. Measured, before this was one transaction: a kill at the settings
    # commit left an active relationship and its generation with no authorized settings for
    # either task. So the writes are composed into one transaction here, where both halves are
    # in hand, rather than by changing what registry.register promises its other callers.
    #
    # Settings are parsed ONCE, and before the lock. _settings_json may read a file: an @path
    # naming one that does not exist is a usage error rather than something to discover holding
    # the write lock, and reading it twice would let the content validated differ from the
    # content recorded.
    from .linkage import CHILD

    #
    # Each side carries its own exception, because recovering it from the settings value by
    # identity or equality asks the wrong question: two sides can pass the same string, and
    # then the child is validated against the parent's exception.
    #
    # The last element is the role THIS write would establish. With --project the registration
    # binds the relationship's child task to its issue scope as a child, so that -- not the
    # role the caller cited -- is what the citation has to agree with.
    writes = [
        (task, _settings_json(raw), role, exception, establishes)
        for task, raw, role, exception, establishes in (
            (args.parent_task, args.parent_settings, args.parent_role,
             args.parent_exception, None),
            (args.child_task, args.child_settings, args.child_role, args.child_exception,
             CHILD if args.project else None),
        )
        if raw
    ]
    recorded = {}
    try:
        with services.store.composing():
            # Inside the transaction now, so the bindings it reads cannot move between the
            # check and the write it authorizes.
            _refuse_role_disagreement(services, writes)
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
                project_key=args.project,
            )
            # Execution settings come from the creation result the caller already holds.
            # Recording them here is what lets a later send preserve them instead of
            # inheriting a host default.
            for task, values, role, exception, _establishes in writes:
                # The role the CREATION cited, from the receipt's executionPolicy. Recorded
                # beside the settings so a later binding can be checked against what the task
                # was made as.
                record_settings(services.store, services.clock, task, values,
                                source="creation_result", role=role, exception=exception)
                recorded[task] = "recorded"
    except RelayError as failure:
        # The transaction that just rolled back was this command's, so a contest recorded
        # inside it went down with the registration. The refusal travels on the error for
        # exactly this, and recording it is what every linkage write path promises. A failure
        # carrying no refusal records nothing.
        services.registry.record_refusal(failure, at=services.clock.iso())
        raise
    payload = relationship_record(record)
    payload["authorizedSettings"] = recorded or None
    return payload


def _settings_json(raw: str) -> dict:
    """A JSON object, or @path to a file holding one."""
    if raw.startswith("@"):
        with open(raw[1:], encoding="utf-8") as handle:
            return json.load(handle)
    return json.loads(raw)


def _refuse_role_disagreement(services, writes) -> None:
    """Refuse a registration whose stated roles and settings already contradict each other.

    Answers exactly what record_settings would answer afterwards, from the same predicate, so
    the two cannot disagree. It is given the settings its caller already parsed rather than
    re-reading them, so the content checked here is the content recorded afterwards even when
    an @path file changes underneath. The one thing it reads from the store is the binding each
    task already holds, because a citation is checked against the role the task will actually
    be in and that is not always the role the caller named.
    """
    from . import rolepolicy
    from .errors import RefusalReason, RegistrationError
    from .settings import TaskSettings

    policy = rolepolicy.declared()
    # The role a write would establish arrives with each entry. Comparing the cited role
    # against itself asked a different question and passed every time: a registration citing
    # parent for the child task committed the relationship and the binding, and only the
    # settings write after them refused, leaving a live wrong-role assignment this function
    # exists to prevent and nothing here could undo.
    for task, values, role, exception, establishes in writes:
        settings = dict(values)
        # The same completeness question record_settings asks, asked while there is still
        # nothing to unwind, and asked of every settings value rather than only the ones that
        # also name a role. Without it an incomplete settings file passed here, the relationship
        # committed, and require_usable then raised over the top of it -- the same partial
        # registration as the role disagreement, arriving through a second door.
        TaskSettings(settings).require_usable()
        if role is None:
            continue
        settings["citedRole"] = role
        if exception is not None:
            settings["citedException"] = exception
        # A binding the task already holds outranks the one this write would establish, because
        # registration does not move a task between roles; attaching an already-bound task is
        # the binding it has being confirmed, not replaced.
        bound = rolepolicy.bound_role(services.store, task)
        if isinstance(bound, rolepolicy.Contested):
            raise RegistrationError(
                RefusalReason.ROLE_BINDING_MISMATCH,
                f"{task!r} holds live bindings at {bound.roles}; one task holds one role, so "
                "there is no single role to register settings against",
            )
        # Falling back to the cited role is what keeps the pair check this function already
        # performed for a task that neither holds a binding nor gains one here.
        finding = rolepolicy.check_binding(
            role, bound or establishes or role, settings, policy if policy else None
        )
        if finding is not None:
            raise RegistrationError(RefusalReason(finding["code"]), finding["detail"])


def cmd_settings_record(services, args) -> dict:
    """Record or replace one task's authorized execution settings.

    This is the exact interface JUN-92 populates from Run's creation result. Required fields:
    sandbox (the full SandboxPolicy object), approvalPolicy, cwd, runtimeWorkspaceRoots, model,
    reasoningEffort and environments. Anything missing is refused here rather than at send
    time, and so is a cwd, model or reasoningEffort recorded as something other than a string.
    """
    if args.clear_exception and args.exception is not None:
        # Asking to cite one and to drop it are two different writes. Letting either win
        # silently would record the opposite of half of what was asked for.
        raise SystemExit2(
            "--clear-exception drops the citation and --exception records one; state one",
            EXIT_USAGE,
        )
    return record_settings(
        services.store, services.clock, args.task, _settings_json(args.settings),
        source=args.source, role=args.role,
        exception=CLEAR_EXCEPTION if args.clear_exception else args.exception,
    )


def cmd_settings_show(services, args) -> dict:
    from .errors import DeliveryRefused
    from .registry import load_settings

    settings = load_settings(services.store, args.task)
    if settings is None:
        return {"task": args.task, "settings": None, "usable": False,
                "deliverable": False, "missing": list(REQUIRED_SETTINGS),
                "recordFinding": None}
    # The role side is reported beside the settings because the two are only meaningful
    # together: a record is stale relative to the policy for the role its task actually holds,
    # and reading one without the other is how a correct record and a wrong one look alike.
    from . import rolepolicy
    from .errors import RefusalReason

    bound = rolepolicy.bound_role(services.store, args.task)
    policy = rolepolicy.declared()
    # Contested is not a role, and handing it to check_record produced a recovery telling an
    # operator to declare a role named after a Python object's memory address. A store that
    # says this task holds two roles at once is its own finding.
    contested = isinstance(bound, rolepolicy.Contested)
    finding = None
    if contested:
        finding = {
            "code": RefusalReason.ROLE_BINDING_MISMATCH.value,
            "boundRoles": bound.roles,
            "recovery": "one task holds one role; resolve these bindings before this task can "
                        "be checked against either of them",
        }
    elif bound and not policy:
        # Delivery treats this state as a refusal, so reporting it as deliverable would have
        # this command disagree with the only consumer that acts on the answer. A bound task
        # whose process cannot read a policy is not checkable, and not checkable is not clear.
        finding = {
            "code": RefusalReason.ROLE_POLICY_UNCONFIGURED.value,
            "boundRole": bound,
            "detail": policy.detail,
            "recovery": "set this process's execution policy and restart it; deliveries held "
                        "meanwhile resume on the next pass",
        }
    elif bound:
        finding = rolepolicy.check_record(settings, bound, policy)
    # Total, like doctor's own reader: these rows can hold whatever an older writer or a hand
    # edit left behind, and a diagnosis must not die on one.
    try:
        settings.require_usable()
        unusable = None
    except DeliveryRefused as refusal:
        unusable = refusal
    except Exception as error:  # noqa: BLE001 - total, for the reason above
        unusable = DeliveryRefused(None, f"{type(error).__name__}: {error}")

    # Three questions, three fields, because folding them together loses two of the answers.
    # "usable" is about the RECORD -- are the required fields there -- and it is paired with
    # "missing", so making a complete record report false would contradict the field beside it
    # and leave no way to say "complete, and refused for another reason". "deliverable" is the
    # question a preflight actually asks. It exists because a consumer written before roles
    # reads "usable", would have read true here, and would have gone on to a send this record
    # cannot carry.
    #
    # So "deliverable" is answered by the predicate a preflight really RUNS, not by a
    # restatement of part of it. Until the recorded string fields were typed, `not missing()`
    # and require_usable() agreed on every row this could be asked about; they no longer do,
    # and a row this command called deliverable would be withheld by delivery and reported
    # refused by doctor. They diverge twice now: a mistyped string field, and an approvalPolicy
    # this transport cannot carry, which is PRESENT on such a row and therefore invisible to
    # missing(). Running the predicate here also answers the case that was already wrong before
    # either of them: a sandbox type with no resume mode. "recordFinding" then says WHICH rule
    # refused, because a false deliverable beside an empty missing list and no roleFinding
    # names nothing.
    return {"task": args.task, "settings": settings.data,
            "usable": not settings.missing(), "missing": settings.missing(),
            "deliverable": unusable is None and finding is None,
            "recordFinding": None if unusable is None else {
                # doctor's vocabulary, so the two commands name one refusal alike.
                "code": unusable.reason.value if unusable.reason else "unexpected",
                "detail": unusable.detail,
            },
            "citedRole": rolepolicy.cited_role(settings),
            "citedException": rolepolicy.cited_exception(settings),
            "boundRole": None if contested else bound,
            "boundRoles": bound.roles if contested else None,
            "rolePolicyDigest": policy.digest if policy else None,
            "rolePolicy": "declared" if policy else "unresolved",
            "roleFinding": finding}


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
    """Deactivate a relationship, which also releases its issue scope when it has one."""
    return relationship_record(
        services.registry.set_status(args.relationship, args.status, actor=args.actor)
    )


# ------------------------------------------------------- three-level linkage


def cmd_linkage_bind(services, args) -> dict:
    return services.linkage.bind_scope(
        role=args.role, scope_key=args.scope,
        endpoint=Endpoint(args.task, args.host, cwd=args.cwd, cxc_session=args.cxc_session),
    )


def cmd_linkage_supervise(services, args) -> dict:
    return services.linkage.register_supervision(
        initiative_key=args.initiative, project_key=args.project,
        supervisor=Endpoint(args.supervisor_task, args.supervisor_host,
                            cwd=args.supervisor_cwd, cxc_session=args.supervisor_cxc_session),
        parent=Endpoint(args.parent_task, args.parent_host, cwd=args.parent_cwd,
                        cxc_session=args.parent_cxc_session),
        link_kind=args.kind,
    )


def cmd_linkage_peer(services, args) -> dict:
    return services.linkage.register_peer(
        left_project=args.left_project,
        left_parent=Endpoint(args.left_task, args.left_host),
        right_project=args.right_project,
        right_parent=Endpoint(args.right_task, args.right_host),
    )


def cmd_linkage_attach(services, args) -> dict:
    return services.linkage.attach_issue(args.relationship, args.project)


def cmd_linkage_handover(services, args) -> dict:
    return services.linkage.handover(
        role=args.role, scope_key=args.scope, expect_task_id=args.expect_task,
        endpoint=Endpoint(args.task, args.host, cwd=args.cwd,
                          cxc_session=args.cxc_session),
        acknowledged=args.acknowledge or [], evidence=args.evidence, actor=args.actor,
    )


def cmd_linkage_outstanding(services, args) -> dict:
    """What a replacement owner has to acknowledge before it can take over."""
    return {"projectKey": args.project, "taskId": args.task,
            "outstanding": services.linkage.outstanding(args.project, args.task)}


def cmd_linkage_completion(services, args) -> dict:
    """What a project's children report about being finished. A reading, never a verdict.

    Read-only, and it prints the four answers distinctly rather than reducing them to a yes:
    unreadable when the store did not answer, unregistered when nothing is attached, ambiguous
    when the project has more than one live owner, incomplete with the unfinished rows named,
    and complete_candidate when every live assignment reports a finished state.

    complete_candidate is deliberately the strongest word available here. Integration and
    verification are the parent's judgment and are not visible to this reader, so it never
    prints "complete".
    """
    return _with_enforcement(services, services.assignments.project_state(args.project))


def cmd_linkage_directive(services, args) -> dict:
    reference = args.reference
    if args.correlation and not args.purpose:
        # The correlation is a FIELD of the envelope pointer, so without a purpose there is no
        # pointer to put it in and it was silently dropped: the command reported success and
        # the reply link the caller asked for was simply absent from the stored row.
        raise SystemExit2(
            "--correlation is part of an envelope pointer, so it requires --purpose",
            EXIT_USAGE,
        )
    if args.purpose:
        if reference:
            raise SystemExit2(
                "--purpose derives the envelope pointer, so it cannot be given with"
                " --reference", EXIT_USAGE)
        reference = envelope.directive_reference(
            purpose=args.purpose, link_id=args.link, digest=args.digest,
            correlation_id=args.correlation)
    return services.linkage.record_directive(
        scope_kind=args.scope_kind, scope_key=args.scope, from_task_id=args.from_task,
        from_scope_key=args.from_scope, link_id_value=args.link, digest=args.digest,
        reference=reference,
    )


def cmd_supervisor_select(services, args) -> dict:
    """Whether one event is news for the level above. A read; it sends and records nothing."""
    return services.delivery.supervisor_selection(args.event, recipient=args.recipient)



def cmd_supervisor_stage(services, args) -> dict:
    """Freeze what is owed upward, before anything is sent.

    Staging is not sending. What this writes is the message and the record that a report was
    produced for the fact behind it; whether a supervisor ever sees it is two more commands
    and a host away.
    """
    from . import supervision
    from .report import read as read_work_report

    reading = None
    if args.project:
        if args.recipient:
            raise SystemExit2(
                "--recipient names the supervisor ONE message is addressed to, and --project"
                " stages every standing obligation, each resolved through its own"
                " relationship. Ignoring the one you typed is how a caller learns too late"
                " that it was never checked", EXIT_USAGE)
        readings = [_observation_file(path) for path in args.observation or []]
        return services.supervisor_channel.stage_standing(
            args.project, observations=readings)
    if args.event:
        if args.observation:
            raise SystemExit2(
                "--event and --observation are two different subjects: one obligation comes"
                " from an event in this store and the other from a turn that left no event at"
                " all. Name one", EXIT_USAGE)
        obligation = supervision.from_event(
            services.store, args.event, read_work_report(services.store, args.event))
        about = "event " + repr(args.event)
    elif args.observation and len(args.observation) == 1:
        reading = _observation_file(args.observation[0])
        obligation = supervision.from_observation(reading)
        about = "the observation at " + repr(args.observation[0])
    elif args.observation:
        raise SystemExit2(
            "one obligation is one message, so a single staging takes one observation. Pass"
            " --project to stage several, where each reading is placed by the relationship it"
            " names", EXIT_USAGE)
    else:
        raise SystemExit2(
            "supervisor-stage needs a subject: --event for one event's obligation, --project"
            " for everything a project owes, or one --observation for the obligation a turn"
            " left by ending without reporting", EXIT_USAGE)
    if obligation is None:
        raise SystemExit2(
            about + " raises no obligation, so there is nothing to stage. An event has to be"
            " a completion, a new block or a decision the user owes, and an observation has"
            " to report state unreported", EXIT_USAGE)
    return services.supervisor_channel.stage(
        obligation, expect_recipient=args.recipient,
        reading=reading if args.observation else None)


def cmd_supervisor_send(services, args) -> dict:
    """One attempt at one staged message. Nothing here is automatic.

    It reaches the host only past its own guards: a message that is held, inside its backoff or
    already sent answers sent: false without the adapter being touched.
    """
    from . import supervisorchannel as channel_module

    _require_host(services, "supervisor-send",
                  "a send observes the recipient's lifecycle and resumes its thread. Without"
                  " a host every read fails, which reads as an unmeasured recipient and would"
                  " record a withholding that describes this process rather than the task")
    record = services.supervisor_channel.attempt(args.message, _LazyAdapter(services))
    if record is None:
        row = services.supervisor_channel.get(args.message)
        return {"attempted": False, "sent": False, "messageId": args.message,
                "state": row["state"],
                "holdReason": row["hold_reason"],
                "nextEligibleAt": row["next_eligible_at"],
                "detail": "nothing was sent and nothing is wrong: a busy recipient, a"
                          " backoff still running, or a message already sent"}
    # sent is read off the receipt rather than asserted. An attempt that was made and refused
    # answers sendAttempted no, and reporting that as a send made a transport refusal read as
    # a delivery - the one reading this command exists to prevent.
    return {"attempted": True,
            "sent": record["deliveryState"] in channel_module.DELIVERED,
            **record}


def cmd_supervisor_read(services, args) -> dict:
    """The recipient answering a message it was sent.

    The proof is sha256(messageId|your own turn id), which the delivered bytes cannot contain,
    so an echo cannot produce it. That is all it establishes: nothing authenticates the caller
    and nothing shows the named turn produced the proof, so answering from inside your own turn
    is an instruction rather than a property this checks.

    A message that already has a verified readback answers from the stored row, before the
    proof is checked and without reaching the host.
    """
    _require_host(services, "supervisor-read",
                  "a readback is checked against the host's own turn list and the recipient's"
                  " transcript. Without a host it would record an unverified readback, which"
                  " is a statement about this process and reads as one about the recipient")
    return services.supervisor_channel.read_back(
        args.message, read_turn_id=args.turn, proof=args.proof,
        adapter=_LazyAdapter(services), asserted_by=args.asserted_by)


def cmd_supervisor_show(services, args) -> dict:
    """One staged message whole: what it says, every attempt, and what came back."""
    return services.supervisor_channel.show(args.message)


def _require_host(services, command, why) -> None:
    """Refuse a host-required command that was given no host, before it writes anything.

    HOST_REQUIRED_COMMANDS is a description doctor reports; it enforces nothing. Without this
    the absence of --socket does not stop the command, it changes what the command records:
    every host read fails, and the refusal that follows is written down as a fact about the
    recipient. A missing host is a fact about this invocation and is reported as one.
    """
    if not services.adapter_requested:
        raise SystemExit2(command + " needs --socket: " + why, EXIT_USAGE)

def cmd_packet_check(services, args) -> dict:
    """Whether a packet agrees with the record its receiver read. It decides nothing else.

    With --receiver the record is the receiver's own store reading (receiver.py): read-only,
    every field with what answered it, the head only from an observation, the mode from the
    receiver's ledger. That is the receive step, because no relay code path carries a
    parent-child packet: it arrives in a prompt, and this is what the receiver runs on it.

    With --record the reading is SUPPLIED, and the answer says so. A caller that handed it an
    agreeing record has proved only that the two files agree; what it closes is the case where
    nobody compared them at all.

    With --receiver, --ledger and --applied it checks nothing: it records in the receiver's
    ledger that the receiver has acted on a packet the ledger answered accepted, which is what
    turns act off for that packet's replays.
    """
    one = _json_document(args.packet, "relay-packet/1 message")
    if args.receiver is not None:
        return _packet_check_store(services, args, one)
    if args.observation or args.ledger or args.applied:
        raise SystemExit2(
            "--observation, --ledger and --applied belong to the store reading (--receiver); a"
            " supplied record answers for itself", EXIT_USAGE)
    record = _json_document(args.record, "receiver's own reading")
    answer = packets.reception(one, record)
    answer["recordSource"] = "supplied"
    # A supplied reading is checked, and an absent one is the honest starting ladder. Those
    # are different inputs: falling back on falsiness replaced a malformed reading with a
    # clean one and reported no promotions for it, and passing it through unchecked turned a
    # valid reception into a host failure. Present means checked; missing means unobserved.
    supplied = one.get("progression")
    answer["promotions"] = packets.unsupported_promotions(
        packets.unobserved() if supplied is None else supplied)
    return answer


def _packet_check_store(services, args, one) -> dict:
    """The receive step: the packet against the receiver's own store reading."""
    if args.applied:
        return _packet_applied(args, one)
    observed = None
    if args.observation:
        observed = receiver.observation(_json_document(args.observation, "observation"))
    connection = intent.read_only_connection(services.selection.db_path)
    try:
        if not args.ledger:
            return receiver.check(connection, one, receiver=args.receiver,
                                  observation=observed)
        with receiver.ledger_lock(args.ledger):
            try:
                ledger = receiver.load_ledger(args.ledger, args.receiver)
            except receiver.LedgerUnusable as refused:
                raise SystemExit2(str(refused), EXIT_USAGE) from refused
            answer = receiver.check(connection, one, receiver=args.receiver,
                                    observation=observed, ledger=ledger)
            if receiver.record_answer(ledger, one, answer):
                receiver.save_ledger(args.ledger, ledger)
        answer["ledger"] = args.ledger
        return answer
    finally:
        if connection is not None:
            connection.close()


def _packet_applied(args, one) -> dict:
    """After acting: record in the receiver's own ledger that this accepted packet was applied."""
    if not args.ledger:
        raise SystemExit2("--applied records what you did in your own reception ledger; name it"
                          " with --ledger", EXIT_USAGE)
    if args.observation:
        raise SystemExit2("--applied reads nothing; an observation belongs to the check that"
                          " came before it", EXIT_USAGE)
    with receiver.ledger_lock(args.ledger):
        try:
            ledger = receiver.load_ledger(args.ledger, args.receiver)
            recorded = receiver.record_applied(ledger, one)
        except (receiver.LedgerUnusable, receiver.NotApplicable) as refused:
            raise SystemExit2(str(refused), EXIT_USAGE) from refused
        if not recorded["alreadyApplied"]:
            receiver.save_ledger(args.ledger, ledger)
    recorded.update(receiver=args.receiver, ledger=args.ledger)
    return recorded


def _json_document(path, what) -> dict:
    """One JSON file from disk, named by what it was supposed to be.

    Separate from _observation_file rather than sharing it: that one names a
    reporting-observation, and a caller who mistyped a packet path is not helped by being
    told their packet is not an observation.
    """
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, ValueError, RecursionError) as error:
        # RecursionError: JSON nested deeper than the reader can descend is input it cannot
        # read, the same answer as JSON it cannot parse, and not a failure of this host.
        raise SystemExit2(
            f"the {what} at {path!r} could not be read: {type(error).__name__}: {error}",
            EXIT_USAGE,
        ) from error


def _observation_file(path) -> dict:
    """One reading from disk, with an unreadable file named rather than raised as itself.

    OSError and ValueError together, because a file this cannot decode and a file this cannot
    parse are the same answer to the caller - the reading is unusable - and a
    UnicodeDecodeError escaping as itself would reach a reader as something other than an
    unreadable observation.
    """
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, ValueError) as error:
        raise SystemExit2(
            f"the observation at {path!r} could not be read as a"
            f" reporting-observation/1 record: {type(error).__name__}: {error}",
            EXIT_USAGE,
        ) from error


def cmd_supervisor_standing(services, args) -> dict:
    """What a project still owes upward, and the project's own reading beside it.

    This is the answer to an explicit question. Automatic notification being suppressed is not
    a reason to withhold it, which is why it is a separate command rather than a flag on one.

    An observation is passed in with --observation because a turn that ended without reporting
    writes no row this store can find; reporting-show is what produces one.

    The omissions this store can derive itself - from the declarations a child's relay
    recorded here - are added beside them, as the automatic pass would stage them.
    """
    from . import supervision

    readings = [_observation_file(path) for path in args.observation or []]
    readings += services.supervisor_channel.store_readings(args.project, readings)
    return supervision.status_answer(
        services.store, services.linkage, services.assignments, args.project,
        observations=readings)


def cmd_supervisor_report_recorded(services, args) -> dict:
    """Record that a report was produced for an obligation, once.

    The journal entry is what makes the next reading of the same fact converge instead of
    waking the level above again. It says a report was composed; it does not say one arrived,
    and nothing in this store could.

    Keyed on the OBLIGATION, which is why an omission can be recorded here at all. A turn that
    ended without reporting has no event, so an event-only surface could record a report for
    every kind of news except the one nobody sent - and the next reading of that same omission
    would have produced another report, indefinitely.
    """
    from . import supervision
    from .report import read as read_work_report

    if args.event:
        obligation = supervision.from_event(
            services.store, args.event, read_work_report(services.store, args.event))
        about = f"event {args.event!r}"
    else:
        obligation = supervision.from_observation(_observation_file(args.observation))
        about = f"the observation at {args.observation!r}"
    if obligation is None:
        raise SystemExit2(
            f"{about} raises no obligation: an event has to be a completion, a new block or a"
            " decision the user owes, and an observation has to report state unreported. There"
            " is nothing here to record a report against", EXIT_USAGE)
    recorded = supervision.record_report(
        services.store, obligation, at=services.clock.iso(), messageId=args.message,
        note=args.note or "")
    return {**recorded, "obligation": obligation}


def cmd_linkage_settle(services, args) -> dict:
    return services.linkage.settle_directive(
        args.directive, args.disposition, decided_by=args.actor, reason=args.reason)


def _with_enforcement(services, answer) -> dict:
    """Say when the store could not install a guard index, where an operator is looking.

    An ambiguous answer and a missing index are the same fact seen from two sides, and the
    fact was recorded on the Store where nothing showed it. A reader deciding whether to act
    on a contested owner needs to know the database is not stopping a second one either.
    """
    unenforced = getattr(services.store, "unenforced_indexes", [])
    if unenforced:
        answer = dict(answer, unenforcedIndexes=unenforced)
    return answer


def cmd_linkage_down(services, args) -> dict:
    return _with_enforcement(services, services.linkage.down(args.scope_kind, args.scope))


def cmd_linkage_up(services, args) -> dict:
    if args.scope and not args.task:
        # --scope only narrows the task path, so naming it beside --issue or --relationship
        # reads as a second filter that was never applied.
        raise PayloadExit({
            "ok": False, "reason": "bad_invocation",
            "detail": "--scope chooses between the scopes one TASK owns, so it goes with"
                      " --task. With --issue or --relationship the starting scope is already"
                      " decided and --scope would be silently ignored.",
        }, EXIT_REFUSED)
    return _with_enforcement(services, services.linkage.up(
        task_id=args.task, issue_key=args.issue, relationship_id=args.relationship,
        scope_key=args.scope))

def cmd_linkage_counterpart(services, args) -> dict:
    return services.linkage.counterpart(
        args.from_task, args.to_task, quoted_revision=args.quoted_revision,
        quoted_scope=args.quoted_scope, from_scope=args.from_scope)
# ------------------------------------------------- coordination between parents


def _json_argument(raw, what):
    """A JSON argument decoded where the caller can be told which one was malformed.

    Letting json.loads raise would reach main's bare handler and be reported as a host
    fault, which is what an operator reads when something is wrong with the relay rather
    than with what they typed.
    """
    try:
        return json.loads(raw)
    except ValueError as fault:
        raise PayloadExit({
            "ok": False, "reason": "bad_invocation",
            "detail": what + " must be JSON: " + str(fault),
        }, EXIT_REFUSED) from fault


def _json_shape(raw, what, wanted):
    """Decoded AND the shape the caller downstream will index into.

    Valid JSON of the wrong shape reached code that indexes it and surfaced as a host fault,
    which is what an operator reads when the relay is broken rather than when their argument
    is. The shape is checked where the argument is named.
    """
    value = _json_argument(raw, what)
    if wanted is list:
        if not isinstance(value, list) or not all(isinstance(e, dict) for e in value):
            raise PayloadExit({
                "ok": False, "reason": "bad_invocation",
                "detail": what + " must be a JSON list of objects",
            }, EXIT_REFUSED)
    elif not isinstance(value, dict):
        raise PayloadExit({
            "ok": False, "reason": "bad_invocation",
            "detail": what + " must be a JSON object",
        }, EXIT_REFUSED)
    return value


def cmd_merge_turn_request(services, args) -> dict:
    return services.merge_turn.request(
        repository=args.repository, base_ref=args.base_ref, project_key=args.project,
        holder=Endpoint(args.task, args.host, cwd=args.cwd, cxc_session=args.cxc_session),
        candidate_head=args.head, pr_number=args.pr, relationship_id=args.relationship,
        ready=args.ready)


def cmd_merge_turn_ready(services, args) -> dict:
    return services.merge_turn.declare_ready(
        args.turn, actor=args.actor, ready=args.ready, candidate_head=args.head,
        cause=args.cause or "")


def cmd_merge_turn_acknowledge(services, args) -> dict:
    return services.merge_turn.acknowledge_grant(
        args.turn, actor=args.actor, grant=args.grant, evidence=args.evidence)


def cmd_merge_turn_attest(services, args) -> dict:
    return services.merge_turn.attest(
        args.turn, evidence_kind=args.evidence_kind, idempotency_key=args.idempotency_key,
        actor=args.actor, evidence=args.evidence)


def cmd_merge_turn_request_return(services, args) -> dict:
    return services.merge_turn.request_return(
        args.turn, actor=args.actor, evidence=args.evidence)


def cmd_merge_turn_check(services, args) -> dict:
    return services.merge_turn.begin_merge(
        args.turn, actor=args.actor, head_sha=args.head_sha, base_sha=args.base_sha,
        checks=_json_shape(args.checks, "--checks", list),
        review=_json_shape(args.review, "--review", dict),
        required=args.required or [])


def cmd_merge_turn_land(services, args) -> dict:
    return services.merge_turn.land(
        args.turn, actor=args.actor, landed_sha=args.landed_sha,
        observed_base_sha=args.observed_base_sha, evidence=args.evidence)


def cmd_merge_turn_unknown(services, args) -> dict:
    return services.merge_turn.report_unknown(
        args.turn, actor=args.actor, reason=args.reason)


def cmd_merge_turn_resolve(services, args) -> dict:
    return services.merge_turn.resolve_unknown(
        args.turn, actor=args.actor, observed_base_sha=args.observed_base_sha,
        pr_state=args.pr_state, evidence=args.evidence)


def cmd_merge_turn_release(services, args) -> dict:
    return services.merge_turn.release(
        args.turn, actor=args.actor, disposition=args.disposition, reason=args.reason,
        evidence=args.evidence or "")


def cmd_merge_turn_withdraw(services, args) -> dict:
    return services.merge_turn.withdraw(args.turn, actor=args.actor)


def cmd_merge_turn_show(services, args) -> dict:
    """One selector, named, because three forms resolved by branch order answered silently.

    A caller that passed --parent-task beside --repository got a successful answer about every
    claim that task holds anywhere, with no sign that the target it named was never consulted.
    A recovery read that quietly changed scope is worse than one that refuses.
    """
    named = [
        label for label, given in (
            ("--turn", bool(args.turn)),
            ("--parent-task", bool(args.parent_task)),
            ("--repository with --base-ref", bool(args.repository or args.base_ref)),
        ) if given
    ]
    if len(named) != 1:
        raise PayloadExit({
            "ok": False, "reason": "bad_invocation",
            "detail": "merge-turn-show takes exactly one selector - --turn, --parent-task, or"
                      " --repository with --base-ref - and this named "
                      + (", ".join(named) if named else "none"),
        }, EXIT_REFUSED)
    if bool(args.repository) != bool(args.base_ref):
        raise PayloadExit({
            "ok": False, "reason": "bad_invocation",
            "detail": "a target is a repository AND a base ref; --repository and --base-ref"
                      " are given together or not at all",
        }, EXIT_REFUSED)
    if args.turn:
        return _with_enforcement(services, services.merge_turn.turn(args.turn) or {
            "ok": False, "reason": "unregistered_scope", "turnId": args.turn})
    if args.parent_task:
        return _with_enforcement(
            services,
            {"parentTaskId": args.parent_task,
             "claims": services.merge_turn.outstanding(args.parent_task)})
    return _with_enforcement(
        services, services.merge_turn.target(args.repository, args.base_ref))


def cmd_slot_reserve(services, args) -> dict:
    return services.capacity.reserve(
        subject_kind=args.kind, subject_key=args.subject, parent_task_id=args.parent_task,
        project_key=args.project, reserved_by=args.actor, detail=args.detail)


def cmd_slot_release(services, args) -> dict:
    return services.capacity.release(
        subject_kind=args.kind, subject_key=args.subject, released_by=args.actor,
        reason=args.reason, tenure=args.tenure)


def cmd_limit_declare(services, args) -> dict:
    return services.capacity.declare_limit(
        scope_kind=args.scope_kind, scope_key=args.scope, dimension=args.dimension,
        unit=args.unit, ceiling=args.ceiling, declared_by=args.declared_by,
        source=args.source, enforce=not args.no_enforce)


def cmd_usage_observe(services, args) -> dict:
    return services.capacity.observe(
        scope_kind=args.scope_kind, scope_key=args.scope, dimension=args.dimension,
        observed=args.observed, observed_by=args.observed_by, method=args.method)


def cmd_capacity_show(services, args) -> dict:
    answer = services.capacity.report(
        project_key=args.project, parent_task_id=args.parent_task,
        initiative_key=args.initiative)
    if args.scope:
        answer["headroom"] = services.capacity.headroom(args.scope_kind, args.scope)
    return _with_enforcement(services, answer)


def cmd_region_propose(services, args) -> dict:
    return services.edit_regions.propose(
        repository=args.repository, base_revision=args.revision, path=args.path,
        region_kind=args.kind, region_key=args.key or "", region_class=args.region_class,
        regenerate_from=args.regenerate_from, left_project=args.left_project,
        right_project=args.right_project, peer_link_id=args.peer_link,
        proposer_task_id=args.task, constraint_text=args.constraint,
        condition=args.condition, issue_key=args.issue, next_owner=args.next_owner)


def cmd_region_settle(services, args) -> dict:
    return services.edit_regions.settle(
        args.agreement, actor=args.actor, disposition=args.disposition,
        condition=args.condition, reason=args.reason)


def cmd_region_restate_revision(services, args) -> dict:
    return services.edit_regions.restate_revision(
        repository=args.repository, from_revision=args.from_revision,
        to_revision=args.to_revision, actor=args.actor)


def cmd_region_reaffirm(services, args) -> dict:
    return services.edit_regions.reaffirm(
        args.agreement, actor=args.actor, base_revision=args.revision)


def cmd_region_followup(services, args) -> dict:
    return services.edit_regions.followup(
        args.agreement, trigger_text=args.trigger, acceptance_text=args.acceptance,
        recorded_by=args.recorded_by, issue_ref=args.issue_ref,
        assignee_task_id=args.assignee, assignee_project=args.assignee_project)


def cmd_region_followup_accept(services, args) -> dict:
    return services.edit_regions.accept_followup(
        args.followup, actor=args.actor, assignee_project=args.assignee_project)


def cmd_region_followup_settle(services, args) -> dict:
    return services.edit_regions.settle_followup(
        args.followup, actor=args.actor, disposition=args.disposition, reason=args.reason)


def cmd_region_show(services, args) -> dict:
    return _with_enforcement(services, services.edit_regions.show(
        repository=args.repository, base_revision=args.revision,
        project_key=args.project, path=args.path))




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


def cmd_dispositions_show(services, args) -> dict:
    """What this scope's children last reported, and whether the store observed it being sent.

    Constructs no Store. dispositions.read goes through read_only_rows, for the reason doctor does:
    Store.__init__ opens O_RDWR, switches on WAL and runs the whole schema script, so a reader
    pointed at a mistyped state directory would CREATE an empty relay database and then answer
    "nothing is blocked" honestly.

    An unreadable store exits refused rather than 0. doctor keeps exit 0 for an unreadable database
    because determining before anything exists is not an error, but this command answers "which
    children are blocked, and was anyone told", and a caller that reads only the exit code must not
    read an unreadable store as "nobody". The whole payload still travels, so a JSON consumer reads
    the reason rather than guessing it.
    """
    from .dispositions import read as read_dispositions

    report = read_dispositions(
        services.selection, project_key=args.project, relationship_id=args.relationship,
    )
    if not report["readable"]:
        raise PayloadExit(report, EXIT_REFUSED)
    return report

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




# Spelled at the CLI so --help lists them, and imported rather than restated so the surface
# cannot drift from what record_reverification actually accepts.
from . import faults as faults_module  # noqa: E402
from .faults import METHODS as FAULT_METHODS, OUTCOMES as FAULT_OUTCOMES  # noqa: E402


def cmd_fault_target(services, args) -> dict:
    return services.faults.set_target(args.scope, args.tracker_ref)


def cmd_fault_observe(services, args) -> dict:
    return services.faults.record(_fault_json(args.observation, "observation"))


def cmd_fault_sweep(services, args) -> dict:
    """Derive what the store currently shows is broken, and record it."""
    from . import faultsweep

    readings = _fault_json(args.readings, "readings") if args.readings else []
    batch = faultsweep.sweep(
        services.store, product=args.product,
        scope={"projectKey": args.project} if args.project else {}, readings=readings,
    )
    recorded = faultsweep.record_all(services.faults, batch, store=services.store)
    return {"read": recorded["read"], "recorded": recorded["recorded"],
            "queued": recorded["queued"], "gaps": recorded["gaps"],
            "limits": batch["limits"]}


def _positive(value, name):
    """A bound has to bound something. argparse accepts -1 happily and SQLite reads LIMIT -1
    as no limit, so the flag asking for a page would have materialised the whole queue."""
    from .errors import RefusalReason
    from .faults import FaultRefused

    if not isinstance(value, int) or value < 1:
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           f"{name} is a positive integer, not {value!r}")
    return value


def _fault_json(value, what):
    """Parse caller-supplied JSON into a refusal rather than a traceback.

    An unhandled decode error left the command reporting an outage for what is simply a
    malformed argument, which is the wrong diagnosis to hand somebody at 3am.
    """
    from .errors import RefusalReason
    from .faults import FaultRefused

    try:
        return json.loads(_read_text(value))
    except ValueError as error:
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           f"the {what} is not readable JSON: {error}") from error


def cmd_fault_show(services, args) -> dict:
    # One validated bound, applied on both branches. The listing branch used to take no bound
    # at all, so the flag an operator passed to keep the answer small reached nothing.
    limit = _positive(args.limit, "--limit")
    if args.fault:
        record = services.faults.get(args.fault)
        if record is None:
            raise PayloadExit({"faultId": args.fault, "found": False}, EXIT_REFUSED)
        record["occurrences"] = services.faults.occurrences(args.fault, limit=limit)
        record["remediations"] = services.faults.remediations(args.fault, limit=limit)
        return record
    return services.faults.snapshot(scope_key=args.scope, state=args.fault_state,
                                    limit=limit, after=args.after)


def cmd_fault_fix(services, args) -> dict:
    return services.faults.record_fix(args.fault, ref=args.ref, detail=args.detail or "")


def cmd_fault_reverify(services, args) -> dict:
    return services.faults.record_reverification(
        args.fault, method=args.method, ref=args.ref, outcome=args.outcome,
        detail=args.detail or "",
    )


def cmd_fault_resolve(services, args) -> dict:
    return services.faults.resolve(args.fault)


def cmd_fault_next(services, args) -> dict:
    services.faults.expire_leases()
    return {"publications": services.faults.next(limit=_positive(args.limit, "--limit"))}


def cmd_fault_claim(services, args) -> dict:
    return services.faults.claim(args.publication, owner=args.owner)


def cmd_fault_operation(services, args) -> dict:
    return services.faults.operation(args.publication, claim_token=args.claim_token)


def cmd_fault_reconcile(services, args) -> dict:
    return services.faults.reconcile(args.publication, _read_text(args.observed),
                                     searched=args.searched)


def cmd_fault_complete(services, args) -> dict:
    return services.faults.complete(
        args.publication, claim_token=args.claim_token, readback=_read_text(args.readback),
        external_ref=args.external_ref,
    )


def cmd_fault_fail(services, args) -> dict:
    return services.faults.fail(args.publication, claim_token=args.claim_token,
                                error=args.error)


def cmd_fault_retry(services, args) -> dict:
    return services.faults.retry(args.publication)


def cmd_fault_prune(services, args) -> dict:
    return services.faults.prune(args.fault, keep=args.keep)


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
    if args.restoration is not None:
        # Declared against a finding, because a finding is the only thing the correction
        # actually carries. Naming one that is not there is refused rather than ignored: a
        # flag that silently attaches to nothing is the same silence this whole path removes.
        #
        # Omitted and empty are different answers, and argparse leaves this None only when the
        # option is absent. A falsy check let --restoration "$UNSET" skip validation and
        # marking altogether, so the verdict opened the next generation recording not_carried
        # while every other carrier that names nothing is refused before that point.
        # Compared through criteria.finding_id on BOTH sides, so this surface and the
        # normalisation that follows it agree about which findings exist. Comparing the raw
        # argument rejected ' c2' as naming no finding while the verdict went on to accept it
        # as 'c2', and a rule that normalises one operand is half a rule.
        wanted = finding_id(args.restoration)
        if not wanted:
            raise SystemExit2(
                "--restoration names the criterion id whose finding carries the block, so it "
                "cannot be empty. Leave the option out to carry no block",
                EXIT_USAGE,
            )
        marked = [
            item for item in criteria + findings
            # Shape-checked here because --criteria accepts arbitrary JSON and this runs
            # before normalise_findings can refuse it. Calling .get on a null entry raised an
            # AttributeError out of a command whose contract is a named refusal and an exit
            # code, so a malformed array answered with a traceback.
            if isinstance(item, dict) and finding_id(item.get("id")) == wanted
        ]
        if not marked and all(isinstance(item, dict) for item in criteria + findings):
            # Only when every entry was well formed. Otherwise the array itself is the
            # problem, and normalise_findings owns that refusal and already words it.
            raise SystemExit2(
                f"--restoration names {args.restoration!r}, which is not one of the findings "
                "this verdict carries. The block travels inside a finding, so it names one",
                EXIT_USAGE,
            )
        for item in marked:
            # An entry that already disclaims the block is a contradiction with the option,
            # and overwriting it here would settle that argument before normalise_findings
            # could see there had been one: --criteria could carry restoration false while
            # --restoration named the same criterion, and the verdict would open the next
            # generation instead of refusing. Only an absent or agreeing declaration is
            # marked; a disagreeing one is returned to the caller to say once.
            existing = item.get(restoration.FIELD)
            if existing is False:
                raise SystemExit2(
                    f"--restoration names {wanted!r}, whose finding declares the restoration "
                    "block false. One correction carries one block and says so once",
                    EXIT_USAGE,
                )
            if existing is not None and not isinstance(existing, bool):
                # Left exactly as it arrived, so normalise_findings refuses it by type. That
                # rule belongs to the normaliser, and writing True over a bad value here would
                # turn an invalid declaration into a valid one and take the refusal away from
                # the only place that words it.
                continue
            item[restoration.FIELD] = True
    record = services.ack.record_verdict(
        args.event, verdict=args.verdict, verdict_turn_id=args.verdict_turn,
        criteria=criteria or None, findings=findings or None, reason=args.reason,
        expect_criteria_digest=args.expect_criteria_digest,
    )
    # Underscore-prefixed, which is this package's existing mark for a relay-owned annotation
    # on a contract-shaped record: record_verdict already returns _replay the same way, and
    # both the conformance suite and the ack tests strip exactly those keys before validating.
    # verification-verdict.json closes additionalProperties on the record, so an unprefixed
    # key here would be a contract violation dressed as observability - which is what the
    # comment this replaces claimed not to be doing while doing it.
    return dict(record, _restoration=services.ack.restoration_of(args.event))


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
    # What became of this event's restoration block, if one was declared. Three kinds live
    # here and they answer different questions. restoration_projected is what the ruling
    # established BEFORE it opened the next generation, recorded against the event that was
    # ruled on. restoration_rendered is what a later work report did to the message, recorded
    # against the revision event that report reshaped. Both are preflight. Only
    # restoration_attempted is about bytes that exist: it is written in the transaction that
    # froze one attempt's message, so it says what that attempt carried rather than what the
    # next one was expected to.
    payload["restoration"] = _restoration_entries(services.store, args.event)
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


def _restoration_entries(store, event_id) -> list:
    """Every recorded outcome for one event's restoration block, oldest first.

    All three kinds, because the per-attempt one is the only measurement about bytes that
    were actually frozen, and leaving it out of the documented inspection command would
    return exactly the preflight projections while withholding the evidence.
    """
    rows = store.all(
        "SELECT kind, at, detail FROM journal WHERE subject = ? AND kind IN (?,?,?)"
        " ORDER BY seq",
        (event_id, "restoration_projected", "restoration_rendered",
         "restoration_attempted"),
    )
    return [
        dict(json.loads(row["detail"]), kind=row["kind"], at=row["at"])
        for row in rows if row["detail"]
    ]


def cmd_status(services, args) -> dict:
    payload = services.delivery.snapshot(relationship_id=args.relationship)
    # Scoped with the deliveries. A global health block beside a filtered list invites
    # reading another assignment's backlog as this one's.
    payload["observation"] = services.delivery.observation_health(
        relationship_id=args.relationship,
    )
    # Degraded enforcement is an operator fact. An index the store could not install means
    # the database is no longer refusing a second owner, which is what makes the linkage
    # readers answer ambiguous; recording it on the Store and showing it nowhere left the
    # documented promise to name it unkept.
    unenforced = getattr(services.store, "unenforced_indexes", [])
    if unenforced:
        payload["unenforcedIndexes"] = unenforced
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

    It is the only LIVE evidence here: the identifier inside a copy is identical, while a
    value written after the copy was taken exists in exactly one of the two files. That
    ordering is what nothing enforces, though - a copy taken after the write carries the nonce
    too - so `compare_store` grades a found nonce as proof only alongside an agreeing device
    and inode AND an agreeing log location, and `doctor --expect-nonce` on its own is unproven.
    The log location is the one that separates a shared store from one inode reached at two
    pathnames, where a checkpointed nonce is readable through both while the two participants
    write into logs of their own.
    """
    if args.write:
        return services.store.write_challenge(actor=args.actor or "cli")
    if not args.read:
        raise SystemExit2("store-challenge needs --write or --read <nonce>", EXIT_USAGE)
    return services.store.read_challenge(args.read)


def _ledger_location(services) -> dict:
    """Where the transport ledger will actually live.

    Ordinary commands still resolve it with state_dir(socket_path), from the environment
    only, so a run that overrides --state alone splits the relay store from the ledger that
    carries send idempotency. managed-start is the exception: it pins the adapter ledger to
    the explicit store directory before the adapter is built. This report follows that pin
    and does not open the ledger.
    """
    import hashlib

    if not services.socket_path:
        return {"configured": False, "directory": None, "path": None, "split": False}
    if getattr(services, "_pin_adapter_ledger", False):
        directory = Path(services.selection.path)
    else:
        directory = Path(state_dir(services.socket_path)).expanduser()
    canonical = Path(services.socket_path).expanduser().absolute().resolve()
    endpoint = hashlib.sha256(str(canonical).encode()).hexdigest()[:16]
    split = directory.resolve() != services.selection.path.resolve()
    return {
        "configured": True, "directory": str(directory),
        "path": str(directory / f"operations-{endpoint}.sqlite3"), "split": split,
    }


def _sandbox_summary(row) -> dict:
    """The sandbox the adapter would actually carry for one participant.

    Read from the authorized settings the creation result reported, because that is what a
    send actually sends. A policy file on disk may describe something else entirely, and the
    question here is what this installation would really do, not what it is configured to do.

    Normalised through the same helper delivery uses rather than read raw, for that same
    reason. An authorized policy may legitimately omit a documented default -
    `{"type": "workspaceWrite"}` passes `TaskSettings.require_usable()` - and the adapter then
    applies `networkAccess: false`, empty writable roots and the two temporary-directory
    flags. A receipt built from the raw JSON would report `networkAccess` as null for a
    participant whose sends really do carry false, which is the opposite of what this field
    exists to answer.

    Total, like the helper it leans on. These rows can hold anything an older writer or a hand
    edit left behind, and this is a diagnosis: one unreadable participant must cost that
    participant's line, never the store identity and access evidence standing beside it.

    Readable is not the same as deliverable, and the difference is the whole point of the
    field. A row can parse, normalise and still be refused before its settings ever reach a
    host, in which case no sandbox goes on the wire at all - and reporting it as what the
    adapter would carry tells an operator access is fine for a participant whose sends are
    never made. So the recorded values are still shown, because they are the only clue to WHY
    delivery refuses this participant, and `deliverable` says whether a send can carry them.

    The set is the constraints delivery imposes on the RECORDED row before turn/start, and it
    has two kinds of member. A TRANSFORMATION can fail on the row by raising, so it is RUN
    here rather than described: `TaskSettings.require_usable()` in
    `DeliveryService._settings_for` (delivery.py), the resume-params construction in
    `_guarded_send` (bridge_adapter.py), and `normalise_environments`, which is the one the
    first two do not already reach. A VALUE CONSTRAINT cannot fail by raising on its own: it is
    a comparison against a fixed value. It reaches `deliverable` anyway, because
    `require_usable()` now makes that comparison against the recorded row and refuses it, so
    running the validator answers both kinds.

    This field used to report the two kinds separately, and `refusedIfPreserved` is gone with
    the reason it existed. It said a row recording another approval policy completed a send
    against a host that REPLACED the value and was refused only by one that reported it back -
    which was true, and was the defect: whether the message was delivered depended on what the
    host did with a fact the record already settled. Now the row is refused before any host is
    asked, so the field could never again carry a value, and leaving it would describe a send
    that is no longer attempted.

    Six versions of this field were wrong the same way before that could be written: each named
    the answer after the members it had been shown - the sandbox type, then `require_usable()`,
    then the params construction, then the post-response half, then the constraint that raises
    nothing at all, then that constraint reported beside the answer instead of in it. The set is
    not kept by hand here.
    `test_every_transformation_a_send_applies_to_the_record_is_covered` (tests/test_cli.py)
    derives both kinds from those two modules, in both directions - what the response
    verification refuses and what the record validator refuses, asserted equal - and fails if a
    member of either is added that this does not reach.

    What it does NOT answer is whether the host accepts the parameters. The App Server's own
    schema is not in this repository, so nothing here can say what it does with a value this
    package finds well-formed - a model name it does not serve, a `cwd` naming a directory it
    does not have. Those are built and sent, and the answer comes back from the wire.

    What it does answer grew by one when the recorded string fields were typed. A recorded
    `cwd: 7` is no longer among them: `require_usable()` refuses it, so it reaches
    `deliverable` as a failing transformation like any other, and the send is withheld rather
    than made against an answer only the wire could give.
    """
    import json

    from .errors import DeliveryRefused
    from .settings import TaskSettings, normalise_environments, normalise_policy

    try:
        settings = json.loads(row["settings"])
    except (TypeError, ValueError):
        return {"readable": False, "detail": "the recorded settings are not valid JSON"}
    if not isinstance(settings, dict):
        # json.loads happily returns a list or a number, and .get raises on both.
        return {
            "readable": False,
            "detail": f"the recorded settings are {type(settings).__name__}, not an object",
        }
    policy = normalise_policy(settings.get("sandbox"))
    if policy is None:
        return {"readable": False, "detail": "the recorded sandbox policy cannot be read"}
    view = TaskSettings(settings)
    refused = None
    try:
        view.require_usable()
        # Not an inspection of the params: the construction a send performs, run for real. A
        # record the validator accepts can still fail in it - runtimeWorkspaceRoots: 7 is
        # present, so require_usable() passes, and then list(7) raises. The thread id is the
        # one input that cannot change the answer: resume_params assigns it to
        # params["threadId"] and reads nothing from it, so a placeholder can neither hide a
        # failure nor invent one.
        view.resume_params("doctor-probe-thread")
        # The recorded half of what runs AFTER the response. `mismatches` needs a resume
        # response and cannot be run here, but the transformations it applies to the recorded
        # row can be, and this is the one the two calls above do not reach: environments is
        # read only by the verification, so a value the completeness gate admits and the
        # params never touch gets that far. Measured both ways - a response that reports its
        # environment selection raises here, one that reports null withholds the send as
        # environments_unknown - so no send completes for such a row either way.
        normalise_environments(settings.get("environments"))
    except DeliveryRefused as refusal:
        refused = refusal
    except Exception as error:  # noqa: BLE001 - total, like everything else in this helper
        # The validator is not written to be fed hand-edited rows, and a diagnosis must not
        # die on one. An unexpected failure is still a refusal, reported as what it was.
        refused = DeliveryRefused(None, f"{type(error).__name__}: {error}")

    cwd = settings.get("cwd")
    return {
        "readable": True,
        "deliverable": refused is None,
        # Which gate refused, in delivery's own vocabulary, so a receipt and a delivery
        # journal name the same thing.
        "refusedBy": None if refused is None else (
            refused.reason.value if refused.reason else "unexpected"
        ),
        "detail": None if refused is None else refused.detail,
        # What a send would really put on the wire. None whenever the record is refused,
        # because delivery sends no sandbox at all rather than downgrading to another one.
        "resumeMode": view.sandbox_mode() if refused is None else None,
        "mode": policy.get("type"),
        "writableRoots": policy.get("writableRoots"),
        "networkAccess": policy.get("networkAccess"),
        "excludeTmpdirEnvVar": policy.get("excludeTmpdirEnvVar"),
        "excludeSlashTmp": policy.get("excludeSlashTmp"),
        "cwd": cwd if isinstance(cwd, str) else None,
        "recordedFrom": row["source"],
        "recordedAt": row["recorded_at"],
    }


def _access_receipt(services, report) -> dict:
    """One participant's observed answer to: which store is this, and may I use it?

    Every field is measured rather than declared. The identity and the device/inode pair are
    fstat-ed from the descriptor the probe holds open, so they describe the file this command
    actually reached rather than whatever the name reaches now; the read and write answers
    come from a real read-only connection and a real rolled-back write transaction opened
    through that same descriptor, not from a permission bit; and the sandbox comes from the
    settings the adapter would carry rather than from configuration. SQLite reopens the name
    it resolves the descriptor to, so store._hold_database is where the remaining window is
    stated; what this receipt rules out is a move that had already happened when it asked.

    It exists to be COMPARED. Two participants put their receipts side by side to find out
    whether they are on one database or two, and a matching path does not settle that: two
    spellings can be one file, and one spelling can be two files on different mounts or in
    different sandboxes. The device and inode are what actually answer it, which is why they
    are here beside the path rather than instead of it.

    The pair is decisive in one direction only, and `compare_store` (store.py) grades it that
    way. A DIFFERENT pair means a different file; an agreeing pair is not sufficient for the
    same one. It is namespace-local, so participants in separate mount namespaces or on
    different hosts can hold one pair while sharing nothing, and one inode can be reached at
    more than one pathname, which is what decides the write-ahead log. That last part is what
    `logDevice`, `logInode` and `logName` answer: the directory entry this file's `-wal` would
    be created under, so two participants can compare where their logs GO rather than only
    which file they opened. `links` is still reported beside the pair, now as the narrower
    guard it always was - it catches the hardlink, while a file bind mount adds a pathname
    without changing it, which was measured on this host on 2026-09-22. What settles a shared
    store is `store-challenge` and `doctor --expect-nonce` TOGETHER with the peer's
    `--expect-inode` and `--expect-log`: the nonce is the live half, the physical identity says
    the file is still the same one, and the log location says both participants write into one
    log rather than two beside one set of bytes.

    The identity and the participants come out of ONE read for the same reason. Collected by
    two separate opens, an atomic replacement between them would pair one store's identity
    with another store's participants and the receipt would say nothing about it - a mismatch
    invisible in exactly the comparison this exists to support. One statement carries both,
    and what it returns is checked against what the probe measured - the store id AND the
    device and inode the rows were actually read from. The id alone was not that check: it is
    minted once and travels with a copy of the bytes, so a replacement by a copy satisfied it.
    """
    from .store import read_only_rows

    store, access = report["store"], report["access"]
    recorded = {"available": False, "participants": {}, "detail": None}
    if access["dbReadable"]:
        # Read-only, through the same door the probe used. Opening a Store here would create
        # and migrate one, which is the side effect doctor promises not to have.
        rows = read_only_rows(
            services.selection,
            "SELECT 'meta' AS kind, key AS task_id, value AS settings,"
            "       NULL AS source, NULL AS recorded_at"
            "  FROM schema_meta WHERE key = 'store_id'"
            " UNION ALL"
            " SELECT 'settings', task_id, settings, source, recorded_at"
            "   FROM authorized_settings"
            " ORDER BY kind, task_id",
        )
        if not rows["readable"] or rows["detail"]:
            recorded["detail"] = (
                rows["detail"] or "the authorized settings could not be read"
            )
        else:
            seen = next(
                (row["settings"] for row in rows["rows"] if row["kind"] == "meta"), None
            )
            read_from = (rows["device"], rows["inode"])
            measured = (store["device"], store["inode"])
            if seen != store["storeId"]:
                # The file this read opened is not the file the probe measured. Reporting
                # both halves as one receipt is the failure; saying so is not.
                recorded["detail"] = (
                    f"the store changed under this command: identity {store['storeId']!r}"
                    f" was measured, settings were read from {seen!r}"
                )
            elif read_from != measured:
                # Same identity, different file: a copy carries the store id. This is the
                # replacement the id comparison above cannot see.
                recorded["detail"] = (
                    "the store changed under this command: device:inode"
                    f" {measured[0]}:{measured[1]} was measured, rows were read from"
                    f" {read_from[0]}:{read_from[1]}"
                )
            else:
                recorded["available"] = True
                recorded["participants"] = {
                    row["task_id"]: _sandbox_summary(row)
                    for row in rows["rows"] if row["kind"] == "settings"
                }
    return {
        "storeId": store["storeId"],
        "dbPath": store["dbPath"],
        "realPath": store["realPath"],
        "device": store["device"],
        "inode": store["inode"],
        # How many names this inode has. One agreeing pair is not one live store if the peer
        # may have opened another name for it; compare_store grades that.
        "links": store["links"],
        # Where a connection on this file writes its write-ahead log: the directory entry it
        # would create `-wal` under. This is what a second pathname for one inode actually
        # changes and what the name count cannot see, because a file bind mount leaves the
        # count at one. A peer sends these three back as --expect-log.
        "logDevice": store["logDevice"],
        "logInode": store["logInode"],
        "logName": store["logName"],
        "selectedBy": {
            "source": services.selection.source,
            "detail": services.selection.detail,
        },
        "observedAccess": {
            "read": access["dbReadable"],
            "write": access["dbWritable"],
            "directoryWritable": access["directoryWritable"],
            "detail": access["detail"],
        },
        "recordedSandbox": recorded,
    }


def _contents(services, report) -> dict:
    """Counts, but only when the store can actually be opened for them."""
    from .store import read_only_rows

    if not report["access"]["dbReadable"]:
        return {"available": False, "relationships": None, "openAttempts": None,
                "detail": "the database is not readable from this process"}
    # Read through a descriptor held on the database, the same door the probe used.
    # services.store would construct a Store, and Store.__init__ opens O_RDWR, switches on WAL
    # and runs the whole schema script - so asking doctor to COUNT rows in an empty, legacy or
    # unrelated readable relay.sqlite3 quietly turned it into a relay database. Diagnosis
    # writes nothing.
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


def _issue_reading(services, report, issue_key: str) -> dict:
    """Whether a relay holds THIS issue, answered from the file the probe just measured.

    OPS-3.4 states the proof as a conjunction: doctor reporting the packet's state directory,
    TOGETHER WITH the issue lookup naming the expected relationship. Those were two commands
    and nothing ordered them. Run the lookup first against a mistyped state directory and it
    CONSTRUCTS an empty store, then honestly answers that nothing is assigned - after which a
    coordinator concludes the issue is unowned and opens a second writer. Answering both
    halves here is what removes the gap, because one read cannot disagree with itself about
    which file it read.

    Constructs no Store, like the rest of doctor: read_only_rows holds the database open and
    identifies it by that descriptor, refusing outright when the file it holds is no longer the
    one at this store's pathname. So a rename this read can observe returns no rows at all
    rather than rows a caller would attribute to the wrong file; store._hold_database states
    the in-call window that leaves.

    Deliberately narrow. It reports what ONE read-only connection can support - whether a live
    relationship exists here and which child owns it - and leaves the scoped/unscoped/
    ambiguous/unreadable project reading to AssignmentView.for_issue, which already owns it. A
    second implementation of those four states would be a second opinion about them.
    """
    from .store import read_only_rows

    blank = {"key": issue_key, "readable": False, "holds": None, "responsibleChild": None,
             "responsibleRelationship": None, "storeId": None, "storeAgreement": "unknown"}
    if not report["access"]["dbReadable"]:
        return {**blank, "detail": "the database is not readable from this process"}
    # Scalar subqueries, so the store id comes back on the SAME read even when the issue has
    # no assignment. A query that returned the identity only alongside rows would go silent in
    # exactly the case this command exists for: an empty store answering "nothing assigned".
    read = read_only_rows(
        services.selection,
        "SELECT (SELECT value FROM schema_meta WHERE key = 'store_id') AS store_id,"
        "       (SELECT relationship_id FROM relationships"
        "         WHERE issue_key = ? AND status IN ('active','paused')"
        "           AND superseded_by IS NULL"
        "         ORDER BY created_at LIMIT 1) AS relationship_id,"
        "       (SELECT child_task_id FROM relationships"
        "         WHERE issue_key = ? AND status IN ('active','paused')"
        "           AND superseded_by IS NULL"
        "         ORDER BY created_at LIMIT 1) AS child_task_id",
        (issue_key, issue_key),
    )
    if not read["readable"] or read["detail"] or not read["rows"]:
        return {**blank, "detail": read["detail"] or "the store could not be read"}

    row = read["rows"][0]
    located = report.get("store") or {}
    # Three facts have to agree before this reading may be acted on: the id minted in the file,
    # and the device and inode the READ itself measured, against what the probe measured. The
    # id alone cannot settle it, because a copy of the bytes carries the same id.
    agreement = "same"
    for observed, expected in (
        (row["store_id"], located.get("storeId")),
        (read.get("device"), located.get("device")),
        (read.get("inode"), located.get("inode")),
    ):
        if observed is None or expected is None:
            agreement = "unknown"
            break
        if observed != expected:
            agreement = "changed"
            break

    if agreement != "same":
        # A relationship read out of a file that is not the one measured is not this
        # assignment's answer, so holds stays null rather than being reported beside the
        # disagreement. cmd_doctor refuses on it.
        return {**blank, "readable": True, "storeId": row["store_id"],
                "storeAgreement": agreement,
                "detail": "the rows did not come from the store this process measured"}
    return {
        "key": issue_key,
        "readable": True,
        "holds": row["relationship_id"] is not None,
        "responsibleChild": row["child_task_id"],
        "responsibleRelationship": row["relationship_id"],
        "storeId": row["store_id"],
        "storeAgreement": "same",
        "detail": None,
    }


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


def _role_policy_report(services) -> dict:
    """What THIS process resolves as a role policy, and what it cannot see from here.

    "unresolved" is a finding, not a blank. It means role-bound deliveries are withheld in this
    process until the variable is set, which is deliberate: a role check that silently does
    nothing when its policy is missing is the failure it exists to prevent, wearing a green
    suite. The digest is reported so receipts from the daemon, the CLI and the hook can be laid
    beside each other, since each reads its own environment and two of them reading two
    different files is a second policy source nothing inside this package can detect alone.

    The bridge's digest is deliberately NOT fetched. It is reported by get_capabilities, which
    is an MCP tool of the bridge server rather than an App Server method, and this adapter's
    transport speaks only the latter. An earlier draft called it here and would have reported
    every ordinary run as unreachable, which reads as a broken bridge rather than as a question
    this surface cannot ask. Compare it by reading get_capabilities through the MCP client that
    owns that connection.
    """
    from . import rolepolicy

    policy = rolepolicy.declared()
    return {
        "state": "declared" if policy else "unresolved",
        "digest": policy.digest if policy else None,
        "detail": None if policy else policy.detail,
        "variable": rolepolicy.ENVIRONMENT_VARIABLE,
        "bridgeDigest": None,
        "agreement": "not_observable_from_here",
        "compareWith": "codex-thread-bridge get_capabilities -> executionPolicy.digest, read "
                       "through the MCP client that owns that connection",
    }


def cmd_doctor(services, args) -> dict:
    """What THIS process can actually do here, measured rather than assumed.

    Constructs no Store: probe() answers from stat, a read-only connection and a rolled-back
    write transaction, so a missing, unreadable or read-only state directory is an answer
    instead of the failure that would otherwise replace it.

    The same-store expectations are a conjunction, and a caller that supplies any of them and
    gets no proof exits non-zero. `--expect-inode` says the peer opened this file;
    `--expect-log` says the peer's write-ahead log goes where this one's does, which is the
    only thing that separates one shared store from one inode reached at two pathnames; and
    `--expect-nonce` is the live half. Each is compared against what the PEER reported - a
    caller that passes its own readings back in has stopped asking the question, which is true
    of every one of these flags and not a property of the newest.
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
    # Emitted by every participant, so parent, child and daemon receipts can be compared
    # against each other rather than each being read as healthy on its own.
    report["accessReceipt"] = _access_receipt(services, report)
    # ------------------------------------------------------------------ role policy
    # Two processes reading two different policy files is a second source of truth by
    # deployment rather than by code, and nothing inside either package can see it: each one
    # reads its own environment and finds a perfectly valid file. So this process's digest is
    # reported here, where a parent, a child and a daemon receipt are already read side by
    # side and can be laid against each other. The bridge's own digest is not fetched; the
    # report names where to read it.
    report["rolePolicy"] = _role_policy_report(services)
    relay_service = _service_for(services)
    report["workerPolicy"] = relay_service.read_worker_policy()
    # A third reading, and deliberately a different KIND of one. rolePolicy is what this
    # process resolved and workerPolicy is what the serving worker resolved; this is neither.
    # It is the input the NEXT daemon launched from this state directory would be given, so a
    # host that is about to be restarted can be read before the restart rather than after it.
    report["launchPolicy"] = relay_service.resolve_launch_policy()
    caller = rolepolicy.snapshot_record()
    worker = report["workerPolicy"].get("policy") or {}
    report["callerWorkerAgreement"] = (
        "same" if caller.get("digest") and worker == caller else
        "different" if caller.get("digest") and worker.get("digest") else "unknown"
    )
    requested = getattr(args, "require_worker_policy", None)
    if requested is not None:
        try:
            requirements = _settings_json(requested)
        except (OSError, ValueError, TypeError) as error:
            raise SystemExit2(f"invalid worker policy requirements: {error}", EXIT_USAGE) from error
        report["workerReadiness"] = rolepolicy.worker_readiness(report["workerPolicy"], requirements)

    # The other half of OPS-3.4's conjunction, on request. Answered from the same file the
    # probe measured, so a coordinator gets one answer instead of joining two commands and
    # owning the order between them.
    issue_key = getattr(args, "issue", None)
    if issue_key:
        report["issue"] = _issue_reading(services, report, issue_key)

    nonce = nonce_lookup(services.selection, args.expect_nonce) if args.expect_nonce else None
    report["nonce"] = nonce
    comparison = compare_store(
        report["store"], expect_store=args.expect_store, expect_inode=args.expect_inode,
        expect_log=args.expect_log, nonce=nonce,
    )
    # Asked, not answerable. `any` over the values counted only NON-EMPTY ones, so an empty
    # expectation was a question nobody had asked and its unproven answer still exited 0. The
    # comparison already grades an unusable value as unproven; this makes the exit agree.
    asked = any(value is not None for value in (
        args.expect_store, args.expect_inode, args.expect_log, args.expect_nonce))
    report.update(comparison)
    if (asked and comparison["sameStore"] != "proven") or (
            requested is not None and not report["workerReadiness"]["ready"]):
        # A caller that asked whether this is the same store and got no proof must not read
        # exit 0 as yes. Complete every requested reading before choosing the exit, so
        # a worker-policy refusal cannot hide the issue/store/nonce observations.
        raise PayloadExit(report, EXIT_REFUSED)
    if issue_key and report["issue"]["readable"] and report["issue"]["storeAgreement"] != "same":
        # Same criterion, one level down, and unproven is refused exactly as a mismatch is:
        # the rule a few lines up already says a caller that asked whether this is the same
        # store and got no proof must not read exit 0 as yes. "changed" and "unknown" are
        # both short of proof, so neither may exit 0 while the store WAS readable. An
        # unreadable store is a different answer - there is simply no relay here - and it
        # keeps exit 0 so that determining before anything exists is not an error.
        raise PayloadExit(report, EXIT_REFUSED)
    return report


def _service_for(services):
    """Built from the probe, so status stays an offline command that constructs no Store."""
    from .service import RelayService

    measured = probe(services.selection)["store"]
    return RelayService(
        services.selection, socket_path=services.socket_path,
        store_id=measured["storeId"],
        # A store that is HERE but would not state its identity is not the same as no store.
        # read_only_rows and the probe now refuse a read they cannot bind to this file, so the
        # identity comes back None in exactly the case a comparison matters most - the store
        # being moved under the command, for every move the read can observe. Passing the
        # distinction keeps ownership from reading that silence as nothing to compare.
        store_unidentified=measured["exists"] and measured["storeId"] is None,
    )


def _refuse_unless_ok(payload: dict) -> dict:
    if payload.get("ok"):
        return payload
    raise PayloadExit(payload, EXIT_REFUSED)


def _finite(value, flag):
    """A bound has to be a number a comparison can ever be true against.

    `type=float` accepts `nan` and `inf`. `nan` is truthy and every `>=` against it is False
    forever, so a run given one would never reach its bound - the unbounded mode this daemon
    is built not to have. `inf` is the same thing spelled honestly. Refused at the surface
    rather than discovered hours later by a process nobody can explain.
    """
    if value is None:
        return None
    if not math.isfinite(value):
        raise SystemExit2(f"{flag} must be a finite number of seconds", EXIT_USAGE)
    return value


def _declared_bound(args):
    """The one bound this invocation was given, in whichever of the two forms it arrived.

    `--deadline` is a DURATION somebody typed, and it starts counting at this process.
    `--deadline-monotonic` is the INSTANT a launching process already decided, in this host's
    and boot's CLOCK_MONOTONIC. Both together is a caller bug: they are two different end
    times, and silently preferring one would end the run when nobody asked.
    """
    duration = _finite(getattr(args, "deadline", None), "--deadline")
    instant = _finite(getattr(args, "deadline_monotonic", None), "--deadline-monotonic")
    if duration is not None and instant is not None:
        raise SystemExit2(
            "--deadline and --deadline-monotonic are two different bounds; pass one",
            EXIT_USAGE,
        )
    if duration is not None and duration < 0:
        raise SystemExit2("--deadline cannot be negative", EXIT_USAGE)
    if instant is not None and instant < 0:
        # CLOCK_MONOTONIC counts from a point at or before this boot, so it is never negative.
        # A negative instant is not an end time this host can have had.
        raise SystemExit2("--deadline-monotonic cannot be negative", EXIT_USAGE)
    return duration, instant


def _bound_already_spent(service, detail):
    """The ending for a bounded run that took no tick because its bound was already gone.

    A plain success would be counted by the supervisor as a clean segment and reset the failure
    streak; a plain failure would back it off from a process that did not fail. Neither is what
    happened, so this has its own exit code and its own line in the journal.
    """
    from .service import EXIT_BOUND_SPENT

    service.store_journal_note(f"this run served nothing: {detail}")
    return PayloadExit(
        {"ok": False, "reason": "bound_already_spent", "detail": detail}, EXIT_BOUND_SPENT,
    )


def _segment_seconds(args):
    """How long each worker gets, checked where a person typed it.

    It becomes the worker's own bound, and a worker given a length it must refuse exits before
    its first tick - which a supervisor reads as an ordinary failure and answers by launching
    another one. The service stays alive and serves nothing. Refusing the configuration once is
    the difference between a usage error and a silent outage.
    """
    value = _finite(getattr(args, "segment_seconds", None), "--segment-seconds")
    if value is not None and value <= 0:
        raise SystemExit2("--segment-seconds must be greater than zero", EXIT_USAGE)
    return value


def _asked_for_no_ticks(args):
    """A run given a tick budget of zero or less took no tick because it was asked for none.

    `RelayDaemon.run` breaks on the tick count BEFORE it looks at the deadline, so whatever the
    bound did meanwhile is not what emptied the result. Both places that classify a bound as
    spent ask this same question, because the two forms of a bound must not disagree about an
    identical request.
    """
    return getattr(args, "max_ticks", None) is not None and args.max_ticks <= 0


def _run_bounded(services, service, args, *, require_intent: bool, monotonic=None) -> dict:
    """Hold ownership for exactly as long as this process serves, then let it go.

    The daemon is constructed INSIDE the claim so a run that loses the race never opens a
    transport connection it is about to abandon.
    """
    from .daemon import RelayDaemon
    from .service import ServiceRefused, owned_service

    monotonic = monotonic or time.monotonic
    duration, instant = _declared_bound(args)
    if instant is None:
        # `is not None` rather than truthiness: `--deadline 0` is a bound that is already
        # spent, and reading it as "no bound given" turned an explicit zero into a run with
        # no bound at all.
        deadline = None if duration is None else services.clock.now() + duration
        bound = None if duration is None else monotonic() + duration
    else:
        # Converted against THIS process's monotonic clock, which is the whole point: fork,
        # interpreter start, imports and argument parsing are already behind us, and they come
        # out of the bound here instead of being spent before a duration started counting. The
        # comparison itself stays on the injected wall clock, exactly as a duration's does.
        remaining = instant - monotonic()
        if remaining <= 0 and not _asked_for_no_ticks(args):
            raise _bound_already_spent(
                service,
                "the instant this run was given had passed by the time it reached its own"
                " clock, so it took no tick",
            )
        deadline = services.clock.now() + remaining
        bound = instant
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
                # So a breakage is recorded without anybody having to notice it first. The
                # pass records and queues; the Linear write itself needs a credential this
                # process does not hold.
                faults=services.faults,
                # So what a project owes the level above goes up without the parent having to
                # remember it (CRW-215): the same staging and send path a parent runs by hand.
                supervisor_channel=services.supervisor_channel,
            )
            try:
                service.publish_worker_policy(rolepolicy.snapshot_record())
            except OSError as error:
                # Missing health evidence withholds new managed admissions; it must not stop
                # this worker's existing queue recovery or non-role-bound deliveries.
                service.store_journal_note(f"worker policy receipt unavailable: {error}")
            reports = daemon.run(
                max_ticks=args.max_ticks, deadline=deadline,
                sleep=_scheduler_wait(services.clock, deadline),
                # The deadline above is a WALL clock instant, because that is what the daemon
                # compares against, and wall clocks move: a backward step after the conversion
                # pushes that instant away and hands the run time nobody granted it. So the run
                # also gets `stop`, its own additional early exit, reading the monotonic bound
                # this process was actually given. A tick cannot START past that however the
                # wall clock behaves; a tick already under way still finishes.
                stop=None if bound is None else (lambda: monotonic() >= bound),
            )
            if not reports and bound is not None and monotonic() >= bound \
                    and not _asked_for_no_ticks(args):
                # The same ending as the check before the locks, reached one step later. The
                # bound was still there when this process read its own clock and was gone by
                # the time the run began - spent adopting the descriptors, taking the claim and
                # building the adapter - so the loop broke before its first tick. Returning
                # success here is what let a supervisor count a worker that served nothing as a
                # clean segment.
                raise _bound_already_spent(
                    service,
                    "the bound was spent while this run was taking its locks and building its"
                    " adapter, so it began with nothing left and took no tick",
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
    if action == "declare":
        # Writing the declaration never starts, stops or reconfigures anything that is
        # running: the daemon holding the lock keeps the policy it was launched with.
        if args.forget_execution_policy:
            return _refuse_unless_ok(service.forget_launch_policy(actor=args.actor or "cli"))
        return _refuse_unless_ok(service.declare_launch_policy(
            args.execution_policy, actor=args.actor or "cli"))
    if action == "stop":
        return _refuse_unless_ok(service.stop(actor=args.actor or "cli"))
    if action in ("start", "restart"):
        _require_adapter(services)
        # Validated where a person typed it, rather than reaching the launched supervisor as
        # an instant built out of nan.
        duration, _instant = _declared_bound(args)
        call = service.start if action == "start" else service.restart
        return _refuse_unless_ok(call(
            allow_isolated=args.allow_isolated_scope, deadline=duration,
            segment_seconds=_segment_seconds(args), max_segments=args.max_segments,
            actor=args.actor or "cli", takeover=getattr(args, "takeover_scope", False),
        ))
    if action == "run":
        _require_adapter(services)
        return _supervise(services, service, args)
    raise SystemExit2(f"unknown service action {action!r}", EXIT_USAGE)


def _launch_already_settled(args, environ) -> str | None:
    """The launch id this environment says was already decided, when it is this launch's.

    A service launching its own daemon has resolved the policy, frozen it and put it in that
    daemon's environment before stopping anything. The id ties the statement to one launch, so
    an environment left over from another one says nothing about this one.

    It is not a trust boundary and does not pretend to be. The user who can arrange this
    environment is the user who can rewrite the declaration, the policy file it names, or the
    record beside it; `read_worker_policy` states the same limit for the same reason. What it
    buys is that the question is settled once per launch rather than asked again by the process
    that is replacing a service already stopped.
    """
    from .service import LAUNCH_SETTLED_ENV

    settled = (environ.get(LAUNCH_SETTLED_ENV) or "").strip()
    launch = getattr(args, "launch_id", None)
    return settled if settled and launch and settled == launch else None


def _apply_launch_policy(service, environ) -> dict | None:
    """Give a supervisor started HERE the policy its service declares, or refuse to start it.

    `service start` builds that environment for the daemon it spawns. `service run` IS that
    daemon, started in the foreground or by a unit, and it was reading whatever shell it came
    from - the same defect one level down, and the one a unit file is most likely to meet.

    Applied to this process's environment before its role-policy snapshot is taken, so the
    process enforces one reading rather than holding a snapshot that disagrees with the
    declaration it is running under. Nothing is written: a declaration is made deliberately,
    never as a side effect of starting.
    """
    from .service import launch_policy_refusal

    resolution = service.resolve_launch_policy(environ)
    refusal = launch_policy_refusal(resolution)
    if refusal is not None:
        return refusal
    if resolution["source"] == "record":
        environ[resolution["variable"]] = resolution["path"]
    return None


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
        duration, instant = _declared_bound(args)
        return service.supervise(
            allow_isolated=args.allow_isolated_scope, segment_seconds=_segment_seconds(args),
            max_segments=args.max_segments, deadline=duration, deadline_monotonic=instant,
            on_start=recover,
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


# ------------------------------------------------------------------ managed marker

# The marker is deliberately NOT reached through Services. Services exists to build a Store, and a
# Store writes on open; every command below writes to the marker filesystem, and only two record
# anything in the relay's tables: intent-claim and intent-disposition, which after the marker write
# mirror the fact it stands on into the store the intent's dbPath names (declarations.py), so the
# relay can derive an omission without reading the marker. intent-register does more than read: it holds the
# relay's write lock across its generation check and its publication, so an advance cannot commit
# between them, then releases it with a rollback having written nothing. The state directory is
# still resolved, because the coordinator is the party that knows where the store it registered
# against actually lives.


def _marker_root(args):
    return marker.resolve_marker_root(getattr(args, "marker_root", None)).path


def _adjudicated(values):
    entries = []
    for value in values or []:
        fact_id, _, digest = str(value).partition("=")
        if not fact_id or not digest:
            raise SystemExit2(
                f"--adjudicate takes factId=digest, not {value!r}", EXIT_USAGE
            )
        entries.append({"factId": fact_id, "digest": digest})
    return entries


def cmd_intent_declare(services, args) -> dict:
    return intent.declare_intent(
        _marker_root(args),
        workspace=args.workspace,
        dispatch_request_id=args.dispatch_request_id,
        issue_key=args.issue,
        declared_at=args.declared_at or services.clock.iso(),
        criteria_source=args.criteria_source,
        baseline_revision=args.baseline_revision,
        authorized_settings=json.loads(args.settings) if args.settings else None,
        db_path=None if args.no_db_path else str(services.selection.db_path),
    )


def cmd_intent_attempt(services, args) -> dict:
    return intent.record_attempt(
        _marker_root(args),
        workspace=args.workspace,
        assignment=args.assignment,
        outcome=args.outcome,
        task_id=args.task_id,
        at=services.clock.iso(),
    )


def cmd_intent_bind(services, args) -> dict:
    return intent.bind(
        _marker_root(args),
        workspace=args.workspace,
        assignment=args.assignment,
        session_id=args.session,
        task_id=args.task_id,
        at=services.clock.iso(),
    )


def cmd_intent_register(services, args) -> dict:
    return intent.register_relationship(
        _marker_root(args),
        workspace=args.workspace,
        assignment=args.assignment,
        relationship_id=args.relationship,
        dispatch_request_id=args.dispatch_request_id,
        at=services.clock.iso(),
        # The relay is the only party that knows which relationship a dispatch actually opened,
        # so registration is confirmed against it rather than taken on the caller's word.
        db_path=args.db_path or str(services.selection.db_path),
    )


def cmd_intent_claim(services, args) -> dict:
    published = intent.publish_claim(
        _marker_root(args),
        workspace=args.workspace,
        assignment=args.assignment,
        session_id=args.session,
        dispatch_request_id=args.dispatch_request_id,
        first_turn_id=args.first_turn,
        at=services.clock.iso(),
    )
    # And in the relay store, after the marker: this session's relay records its declarations
    # there, which is what lets the store derive an omission for its turns at all. Recorded only
    # for the claim the marker now stands on, so a claim the marker refused records nothing.
    from . import declarations

    directory = marker.assignment_dir(_marker_root(args), args.workspace, args.assignment)
    facts, unreadable = marker.read_assignment(directory)
    standing = next((claim for claim in facts.get("claims", [])
                     if intent.claimant(claim) == published["sessionId"]), None)
    # The record is made only from a marker the marker reader itself could read: every fact
    # readable and the right shape, the claim standing and correlated, the intent declared for
    # this workspace. A marker reporting-show would answer unmeasured about is not one the store
    # may derive from, so the store stays silent - the legacy answer - rather than the two
    # readers disagreeing about whether a turn can be classified at all.
    if unreadable:
        record = declarations.not_recorded(
            "marker_unreadable", "the marker could not be read whole after the claim ("
            + ", ".join(sorted(unreadable)) + "), so this session records nothing about how it"
            " reports; running the claim again once the marker reads records it")
    elif intent.malformed(facts):
        # The marker reader answers unmeasured for a marker that is not the shape a fact must
        # be, so nothing the store could derive from it is recorded either.
        record = declarations.not_recorded(
            "marker_malformed", "the marker's " + str(intent.malformed(facts)) + " is not the"
            " shape a fact must be, so this session records nothing about how it reports")
    elif published["outcome"] == intent.CONFLICT or standing is None or (
            standing.get("dispatchRequestId") != args.dispatch_request_id):
        record = declarations.not_recorded(
            "claim_not_standing", "the marker does not stand on this claim, so this session"
            " records nothing about how it reports")
    elif (not intent.correlated(facts, published["sessionId"], args.assignment)
          or not (facts.get("intent") or {}).get("workspace")
          or _resolved(facts["intent"]["workspace"]) != _resolved(args.workspace)):
        # The same preconditions the marker reader checks before it classifies anything:
        # the claim correlates with the intent, and the intent was declared for this
        # workspace. A claim the marker reader would refuse to read is not recorded as one
        # the store may derive from.
        record = declarations.not_recorded(
            "claim_uncorrelated", "the claim does not correlate with the intent declared for"
            " this workspace, so the store derives nothing for this session")
    else:
        record = declarations.record_claim(
            declarations.store_of(facts), assignment=args.assignment,
            session_id=published["sessionId"], dispatch_request_id=args.dispatch_request_id,
            marker_root=_resolved(_marker_root(args)), workspace=_resolved(args.workspace),
            issue_key=(facts.get("intent") or {}).get("issueKey"), at=services.clock.iso())
    return _with_store_record(published, record)


def cmd_intent_disposition(services, args) -> dict:
    from . import declarations

    # The store the intent names, read before anything is published so its write lock can be
    # held across the publication: see declarations.Held. A marker this cannot even locate names
    # no store, and the publication below answers for itself.
    try:
        directory = marker.assignment_dir(_marker_root(args), args.workspace, args.assignment)
        before, unreadable_before = marker.read_assignment(directory)
    except (ValueError, OSError):
        directory, before, unreadable_before = None, {}, []
    with declarations.Held(declarations.store_of(before)) as held:
        published = intent.publish_disposition(
            _marker_root(args),
            workspace=args.workspace,
            assignment=args.assignment,
            session_id=args.session,
            turn_id=args.turn,
            outcome=args.outcome,
            at=services.clock.iso(),
        )
        # Mirroring the disposition the marker now stands on: after a create-once conflict that
        # is the first one, as it is for every marker reader.
        if directory is None:
            directory = marker.assignment_dir(_marker_root(args), args.workspace,
                                              args.assignment)
        facts, unreadable = marker.read_assignment(directory)
        standing, readable = marker.read_disposition(
            directory, published["sessionId"], published["turnId"])
        # Only the facts this record rests on: the intent (which store) and this disposition.
        if ("intent" in unreadable or "intent" in unreadable_before or not readable
                or not standing or intent.malformed_disposition(standing)):
            record = declarations.failure(
                "marker_unreadable", "the intent or the disposition could not be read back from"
                " the marker after it was published, so what the marker stands on is unknown",
                None)
        elif declarations.store_of(facts) != declarations.store_of(before):
            record = declarations.failure(
                "store_changed", "the intent named another store while this was being"
                " recorded, so the record was not written to either", None)
        else:
            record = held.disposition(
                assignment=args.assignment, session_id=published["sessionId"],
                turn_id=published["turnId"], outcome=standing.get("outcome"),
                declared_at=standing.get("at") or "", at=services.clock.iso())
    return _with_store_record(published, held.settled(record))


def _resolved(path) -> str:
    """A path as omitted.observe spells it, so both readers name one marker the same way."""
    return str(Path(path).expanduser().resolve())


def _with_store_record(published, record) -> dict:
    """The marker answer with the store record beside it; a failed record fails the command.

    Exit non-zero with the whole answer, because the marker write DID happen and the caller has
    to be able to see that as well as what did not. Running the command again is safe: the
    marker fact answers unchanged and only the store record is retried.
    """
    from . import declarations

    payload = {**published, "storeRecord": record}
    if record.get("state") == declarations.FAILED:
        raise PayloadExit({**payload, "detail": "the marker fact was published and the relay"
                           " store record was not: " + str(record["detail"])}, EXIT_REFUSED)
    return payload


def cmd_intent_resolve(services, args) -> dict:
    return intent.publish_resolution(
        _marker_root(args),
        workspace=args.workspace,
        assignment=args.assignment,
        chosen_task_id=args.chosen_task,
        chosen_session_id=args.chosen_session,
        reason=args.reason,
        at=services.clock.iso(),
        adjudicated=_adjudicated(args.adjudicate),
    )


def cmd_intent_show(services, args) -> dict:
    """What the marker says about this workspace, without asking the relay anything.

    This is the question a hook has that registered relationships cannot answer: an assignment whose
    registration was never written has no row anywhere, and it is exactly the one worth finding.
    """
    root = _marker_root(args)
    if args.assignment:
        directory = marker.assignment_dir(root, args.workspace, args.assignment)
        facts, unreadable = marker.read_assignment(directory)
        if not unreadable and "intent" not in facts:
            # Selection treats an assignment with no published intent as not selectable, so an
            # explicit one has to read the same way. Reporting it managed and then deriving
            # intent_declared out of nothing told a coordinator a failed declaration had landed.
            #
            # Absence is the test, not shape. read_assignment omits the key when the fact is not
            # there and keeps it when it parsed into something that is not an object, so asking
            # about shape here answered "unmanaged" for a corrupt intent and told an operator that
            # a managed workspace was an ordinary one. The malformed report below is what that case
            # is for, and it is the same answer the guard gives.
            return {
                "markerRoot": str(root),
                "workspace": args.workspace,
                "managed": False,
                "assignmentId": directory.name,
                "assignmentDir": str(directory),
                "unreadable": [],
                "detail": "no intent is published for this assignment",
            }
    else:
        directory, facts, unreadable = intent.select_assignment(root, args.workspace, args.session)
        if directory is None:
            return {
                "markerRoot": str(root),
                "workspace": args.workspace,
                "managed": False,
                "unreadable": list(unreadable or []),
            }
    malformed = intent.malformed(facts) if facts else None
    payload = {
        "markerRoot": str(root),
        "workspace": args.workspace,
        "managed": True,
        "assignmentId": directory.name,
        "assignmentDir": str(directory),
        "unreadable": list(unreadable or []),
        "malformed": malformed,
    }
    if malformed or unreadable:
        # Derivation follows the marker being readable. Summarising records that are not records is
        # how a reader ends in a traceback and reports nothing at all.
        return payload
    payload["assignmentState"] = intent.derive_assignment_state(
        facts, args.now or services.clock.iso()
    )
    payload["identityContested"] = intent.identity_contested(facts)
    payload["intent"] = facts.get("intent")
    payload["bound"] = facts.get("bound")
    payload["relationship"] = facts.get("relationship")
    payload["attempts"] = facts.get("attempts") or []
    payload["claims"] = facts.get("claims") or []
    payload["conflicts"] = facts.get("conflicts") or []
    payload["resolutions"] = facts.get("resolutions") or []
    return payload


def _guard_fallback(services, args):
    """The store this run resolved for itself, or the refusal that stands in its place.

    Returned as a resolver rather than a path because the guard asks for it only when the two
    sources that outrank it said nothing: the caller's --db-path, then the dbPath the coordinator
    recorded in the intent. Those two are selections somebody made - one explicit, one durable -
    and neither depends on discovery, so an ambiguity in discovery is genuinely unrelated to them
    and the hook goes on classifying and recording. What is left is not a selection at all.

    Or it is one made for this run only: a --state, or a CODEX_SESSION_RELAY_STATE the hook
    inherited. Either skips discovery instead of settling it, so where discovery for this socket
    would have refused as ambiguous or unidentified, the override is refused the same way rather
    than judged; see _override_refusal. Where discovery would have named one store, the override
    is used, which is the same answer a sole discovered store gets.

    Two answers this refuses, and they are not worth the same. A store that records a different
    App Server socket EXISTS and opens: the guard reads an unrelated installation's rows, finds no
    relationship, and receipt_missing holds a child that has finished. An ambiguous or
    unidentified selection is returned only when no canonical database is there yet, so today that
    fallback names a file nothing can open and the guard already answers state_unreadable and
    releases; refusing instead trades that recorded release for the candidates and the commands
    that tell them apart. An override under that ambiguity is worth as much as the first kind: it
    names a store that exists and opens, so judging it can hold a finished child on a store nobody
    recorded. All of them are refused, because a selection nobody made is not one this decision
    may rest on, and an operator who has to settle it should be told which stores.

    The Stop is then neither classified nor recorded, which is the cost and is said in the payload.
    Nothing is held: the adapter reads an exit of 2 carrying an error record as the relay refusing
    a request it understood, prints nothing, and lets the turn end.
    """
    del args  # the selection is the subject here; the arguments only chose it

    def resolve():
        refusal = _selection_refusal(services, for_stop=True) or _override_refusal(services)
        if refusal is None:
            return str(services.selection.db_path)
        refusal["stopNotJudged"] = (
            "this Stop was neither classified nor recorded: no --db-path named a receipt store"
            " and the coordinator recorded none in the intent, so the only candidate left was"
            + (" a state directory chosen for this run, while discovery for this socket does not"
               " name one store" if "overriddenBy" in refusal else " this selection")
        )
        raise guard.StoreNotSelected(refusal)

    return resolve


def _override_refusal(services):
    """The ambiguity discovery would have reported, when a one-run override skipped it.

    guard-evaluate only. resolve_state_dir returns --state and CODEX_SESSION_RELAY_STATE before
    discovery runs, so their selections carry no candidates and _selection_refusal has nothing to
    refuse. For every other command that is right: naming a directory is how an operator settles
    an ambiguity, and it is what the recovery lines tell them to do. For a Stop it is not, because
    the directory was chosen for one run and nobody recorded it, and the store it names exists and
    opens - reproduced as a hold on a finished child. So the question discovery would have asked
    is asked here, and answered the same way.

    Nothing is asked without a socket. Ambiguity is defined by stores recording one, and a host
    that configured none compares no provenance at all, which is the documented limit; refusing
    there would switch off hooks that were never ambiguous about anything.
    """
    from .store import OVERRIDES, discover_state_dir

    selection = services.selection
    if selection.source not in OVERRIDES or not services.socket_path:
        return None
    refusal = _ambiguity_refusal(
        services, discover_state_dir(services.socket_path), for_stop=True
    )
    if refusal is None:
        return None
    # Nothing would have been created: the override names a directory that already exists.
    refusal.pop("wouldHaveCreated", None)
    refusal["overriddenBy"] = selection.detail
    refusal["selectedDirectory"] = str(selection.path)
    refusal["detail"] += (
        "; this run's state directory came from " + selection.detail + ", which skips"
        " discovery rather than settling it, and a directory chosen for one run is not one"
        " anybody recorded"
    )
    refusal["recover"].append(
        "  this run's directory came from " + selection.detail + ". An override is read before"
        " discovery on every Stop that carries it, so while it is set, discovery naming one"
        " store does not decide which store is read"
    )
    return refusal


def cmd_guard_evaluate(services, args) -> dict:
    """Decide one Stop and record the observation.

    The Stop payload arrives as JSON on stdin, which is the shape the host delivers it in. Reading
    it from a file is for replaying a captured payload, never for inventing one.
    """
    if args.stop_input and args.stop_input != "-":
        try:
            text = Path(args.stop_input).expanduser().read_text(encoding="utf-8")
        except (OSError, ValueError) as error:
            # A sibling of the marker decode fault: UnicodeDecodeError is a ValueError, so a replay
            # file that is not UTF-8 would otherwise reach the generic handler and be reported as a
            # host failure. It is a named file the operator typed.
            raise SystemExit2(
                f"the Stop payload file could not be read: {error}", EXIT_USAGE
            ) from error
    else:
        text = sys.stdin.read()
    try:
        stop_input = json.loads(text)
    except ValueError as error:
        raise SystemExit2(f"the Stop payload is not JSON: {error}", EXIT_USAGE) from error
    if not isinstance(stop_input, dict):
        raise SystemExit2("the Stop payload must be a JSON object", EXIT_USAGE)
    if args.mode == guard.HOLD and args.no_record:
        # Refused here rather than downgraded silently. A hold is reserved, counted and released
        # through the observation record, so asking for one without recording asks for a hold
        # nothing can account for. evaluate() also downgrades and says so; an operator who typed
        # this at a terminal should be told instead of handed a release they did not expect.
        raise SystemExit2(
            "--mode hold cannot be combined with --no-record: a hold that publishes no observation "
            "cannot be released, counted against the bounds, or audited",
            EXIT_USAGE,
        )
    try:
        return guard.evaluate(
            _marker_root(args),
            stop_input,
            now=args.now or services.clock.iso(),
            mode=guard.HOLD if args.mode == guard.HOLD else guard.OBSERVE,
            # NOT "or the default": passing the resolved default here would make it always present
            # and the intent's recorded dbPath unreachable, so a hook invoked without the
            # coordinator's --state would silently read its own store. evaluate() owns the
            # precedence, and the third source is a resolver it calls only if it gets that far.
            db_path=args.db_path,
            default_db_path=_guard_fallback(services, args),
            record=not args.no_record,
        )
    except guard.StoreNotSelected as error:
        # The store-selection refusal every other command answers before its handler runs, answered
        # here instead because this is where the question could finally be settled.
        raise PayloadExit(error.detail, EXIT_REFUSED) from error



# The same nine commands as MARKER_COMMANDS_BY_NAME, by handler, for the store-selection exemption.
# Declared here because the handlers have to exist first, and checked against the names by a test so
# adding one command in a single place cannot go unnoticed.
MARKER_COMMANDS = (
    cmd_intent_declare, cmd_intent_attempt, cmd_intent_bind, cmd_intent_register,
    cmd_intent_claim, cmd_intent_disposition, cmd_intent_resolve, cmd_intent_show,
    cmd_guard_evaluate,
)


# ----------------------------------------------------------------------- wiring


# ------------------------------------------------------- CRW-26 forge evidence (read-only)
#
# One command, disjoint from everything above it. It constructs no Store, reaches no App Server,
# and issues GET requests and GraphQL queries to one forge. It is the only command here that
# talks to a network service that is not the host, which is why it lives in its own region.


def cmd_merge_evidence(services, args) -> dict:
    """Read a pull request and return the merge-readiness record, observed rather than asserted.

    Two modes, one command. Without --restate it takes a snapshot, and the payload's `handoff` is
    what a child pastes into its completion report. With --restate it takes a FRESH snapshot and
    grades a record the child already produced against it, which is the parent's currency check:
    the parent never reads the child's own numbers back, it reads the forge and compares.

    Exit 0 is the READY verdict and nothing else. A stale or unknown snapshot is still a complete
    answer and still exits 2, because a caller reading only the exit status must never take "I
    could not tell" for "yes" - which is the same mistake, in a different costume, as reading a
    truncated page as a count.
    """
    from . import forge as forge_evidence

    try:
        collector = forge_evidence.Forge(
            page_size=args.page_size, page_budget=args.page_budget,
            call_budget=args.call_budget, timeout=args.timeout,
        )
        snapshot = forge_evidence.collect(
            collector, repository=args.repository, number=args.pull_request,
        )
    except forge_evidence.ForgeUsage as error:
        raise SystemExit2(str(error), EXIT_USAGE)
    ready = snapshot["verdict"] == forge_evidence.READY
    if args.restate:
        document = _restated_record(args.restate)
        head = (args.restate_head or document.get("headSha")
                or (document.get("pinned") or {}).get("headSha"))
        if not head:
            raise SystemExit2(
                "the record does not say which head it is about; pass --restate-head", EXIT_USAGE)
        handoff = document.get("handoff") or document
        problems = forge_evidence.restate_problems(head, handoff, snapshot)
        snapshot["restatement"] = {
            "headSha": head,
            "current": not problems,
            "problems": [{"code": one.code, "detail": one.detail} for one in problems],
        }
        ready = ready and not problems
    if not ready:
        raise PayloadExit(snapshot, EXIT_REFUSED)
    return snapshot


def _restated_record(source: str) -> dict:
    """The record to restate, read from a file or from stdin.

    A snapshot this command produced earlier and a bare handoff are both accepted, because the
    child has one and the parent is handed the other, and making them convert by hand is how a
    field gets dropped in transit.

    The decode failure is caught beside the read failure rather than left to escape. A record
    written in another encoding is a file this command cannot read, which is the same answer as
    a file that is not there; letting UnicodeDecodeError out instead would report the host's
    traceback for what is an ordinary bad input.
    """
    try:
        raw = sys.stdin.read() if source == "-" else Path(source).read_text(encoding="utf-8")
    except (OSError, ValueError) as error:
        raise SystemExit2(f"the record to restate could not be read: {error}", EXIT_USAGE)
    try:
        document = json.loads(raw)
    except ValueError as error:
        raise SystemExit2(f"the record to restate is not JSON: {error}", EXIT_USAGE)
    if not isinstance(document, dict):
        raise SystemExit2("the record to restate is an object, not a "
                          f"{type(document).__name__}", EXIT_USAGE)
    return document


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="codex-session-relay")
    parser.add_argument("--state")
    parser.add_argument("--socket")
    parser.add_argument("--json", action="store_true", default=True)
    subparsers = parser.add_subparsers(dest="command", required=True)

    managed_start = subparsers.add_parser("managed-start")
    managed_start.add_argument("--request", required=True, help="complete request JSON or @file")
    managed_start.add_argument("--marker-root", required=True)
    managed_start.set_defaults(handler=cmd_managed_start)
    managed_show = subparsers.add_parser("managed-show")
    managed_show.add_argument("--request-id", required=True)
    managed_show.set_defaults(handler=cmd_managed_show)
    managed_release = subparsers.add_parser("managed-release")
    managed_release.add_argument("--request-id", required=True)
    managed_release.add_argument("--fingerprint", required=True)
    managed_release.add_argument("--revision", required=True, type=int)
    managed_release.add_argument("--reason", required=True)
    managed_release.set_defaults(handler=cmd_managed_release)

    reporting_show = subparsers.add_parser("reporting-show")
    reporting_show.add_argument("--marker-root", required=True)
    reporting_show.add_argument("--workspace", required=True)
    reporting_show.add_argument("--assignment", required=True)
    reporting_show.add_argument("--session", required=True)
    reporting_show.add_argument("--turn", required=True)
    reporting_show.set_defaults(handler=cmd_reporting_show)

    reporting_derive = subparsers.add_parser(
        "reporting-derive",
        help="the reporting reading this store alone gives for one relationship's turn, from"
             " the declarations its child's relay recorded here. Records nothing, and creates"
             " no store where none exists")
    reporting_derive.add_argument("--relationship", required=True)
    reporting_derive.add_argument(
        "--turn", help="the turn to read; the relationship's newest admitted turn by default")
    reporting_derive.add_argument(
        "--grace", type=float,
        help="seconds an omission waits after settlement before it is owed; the automatic"
             " pass's own by default")
    reporting_derive.set_defaults(handler=cmd_reporting_derive)

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
    register.add_argument("--project",
                          help="the Linear project this issue belongs to. Supplied, the same"
                               " transaction records the issue's project, binds the child and"
                               " links the project to the issue")
    register.add_argument("--parent-settings",
                          help="authorized execution settings as JSON, or @path to a JSON file")
    register.add_argument("--child-settings",
                          help="authorized execution settings as JSON, or @path to a JSON file")
    register.add_argument("--parent-role", choices=("supervisor", "parent", "child"),
                          help="the role the parent's CREATION cited, from its receipt's"
                               " executionPolicy.role. Recorded with the settings so a binding"
                               " that disagrees with it is refused rather than discovered later")
    register.add_argument("--child-role", choices=("supervisor", "parent", "child"),
                          help="the role the child's CREATION cited, read the same way")
    register.add_argument("--parent-exception",
                          help="the operator exception the parent's creation cited, from its"
                               " receipt's executionPolicy.exception, where one authorized the"
                               " pair instead of the role policy")
    register.add_argument("--child-exception",
                          help="the operator exception the child's creation cited, read the"
                               " same way")
    register.set_defaults(handler=cmd_register)

    settings_record = subparsers.add_parser("settings-record")
    settings_record.add_argument("--task", required=True)
    settings_record.add_argument("--settings", required=True,
                                 help="JSON object, or @path to a JSON file")
    settings_record.add_argument("--source", default="creation_result")
    settings_record.add_argument("--role", choices=("supervisor", "parent", "child"),
                                 help="the role this task's creation cited")
    settings_record.add_argument("--exception",
                                 help="the operator exception this task's creation cited")
    settings_record.add_argument("--clear-exception", action="store_true",
                                 help="drop the citation recorded for this task. An exception"
                                      " can stop applying without the pair moving, so this is"
                                      " said rather than inferred")
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

    bind = subparsers.add_parser("linkage-bind")
    bind.add_argument("--role", required=True, choices=["supervisor", "parent", "child"])
    bind.add_argument("--scope", required=True)
    bind.add_argument("--task", required=True)
    bind.add_argument("--host", required=True)
    bind.add_argument("--cwd")
    bind.add_argument("--cxc-session")
    bind.set_defaults(handler=cmd_linkage_bind)

    supervise = subparsers.add_parser("linkage-supervise")
    supervise.add_argument("--initiative", required=True)
    supervise.add_argument("--project", required=True)
    supervise.add_argument("--supervisor-task", required=True)
    supervise.add_argument("--supervisor-host", required=True)
    supervise.add_argument("--supervisor-cwd")
    supervise.add_argument("--supervisor-cxc-session")
    supervise.add_argument("--parent-task", required=True)
    supervise.add_argument("--parent-host", required=True)
    supervise.add_argument("--parent-cwd")
    supervise.add_argument("--parent-cxc-session")
    supervise.add_argument("--kind", default="execution", choices=["execution", "reference"])
    supervise.set_defaults(handler=cmd_linkage_supervise)

    peer = subparsers.add_parser("linkage-peer")
    peer.add_argument("--left-project", required=True)
    peer.add_argument("--left-task", required=True)
    peer.add_argument("--left-host", required=True)
    peer.add_argument("--right-project", required=True)
    peer.add_argument("--right-task", required=True)
    peer.add_argument("--right-host", required=True)
    peer.set_defaults(handler=cmd_linkage_peer)

    attach = subparsers.add_parser("linkage-attach")
    attach.add_argument("--relationship", required=True)
    attach.add_argument("--project", required=True)
    attach.set_defaults(handler=cmd_linkage_attach)

    outstanding = subparsers.add_parser("linkage-outstanding")
    outstanding.add_argument("--project", required=True)
    outstanding.add_argument("--task",
                             help="narrow to one parent's rows. A handover acknowledges the"
                                  " PROJECT's unfinished work, so leave this off for that")
    outstanding.set_defaults(handler=cmd_linkage_outstanding)

    completion = subparsers.add_parser("linkage-completion")
    completion.add_argument("--project", required=True)
    completion.set_defaults(handler=cmd_linkage_completion)

    handover = subparsers.add_parser("linkage-handover")
    handover.add_argument("--role", required=True, choices=["supervisor", "parent"],
                          help="a child is replaced by registering its successor with"
                               " --supersedes, which moves the assignment and its issue scope"
                               " together")
    handover.add_argument("--scope", required=True)
    handover.add_argument("--expect-task", required=True)
    handover.add_argument("--task", required=True)
    handover.add_argument("--host", required=True)
    handover.add_argument("--cwd")
    handover.add_argument("--cxc-session",
                          help="the replacement owner's CXC session, recorded on the new"
                               " binding exactly as linkage-bind and linkage-supervise record"
                               " it. A handover writes the endpoint it is given, so omitting"
                               " this stores no session for the incoming owner")
    handover.add_argument("--acknowledge", action="append",
                          help="a relationship id the replacement owner is taking on. Repeat"
                               " once per unfinished assignment; linkage-outstanding lists"
                               " exactly the set this must equal")
    handover.add_argument("--evidence", required=True)
    handover.add_argument("--actor", required=True)
    handover.set_defaults(handler=cmd_linkage_handover)

    directive = subparsers.add_parser("linkage-directive")
    directive.add_argument("--scope-kind", required=True,
                           choices=["initiative", "project", "issue"])
    directive.add_argument("--scope", required=True)
    directive.add_argument("--from-task", required=True)
    directive.add_argument("--from-scope", required=True)
    directive.add_argument("--link", required=True)
    directive.add_argument("--digest", required=True)
    directive.add_argument("--reference")
    directive.set_defaults(handler=cmd_linkage_directive)
    directive.add_argument("--purpose",
                           choices=sorted(envelope.PURPOSES[envelope.SUPERVISOR_TO_PARENT]),
                           help="derive the envelope pointer for this instruction instead of"
                                " writing --reference by hand. The pointer's message id is"
                                " computed from this link and digest, so a pointer belonging"
                                " to another instruction is refused")
    directive.add_argument("--correlation",
                           help="the message this instruction answers, when it answers one")

    select = subparsers.add_parser(
        "supervisor-select",
        help="whether one event is news for the level above. A read: it sends nothing,"
             " queues nothing and records nothing")
    select.add_argument("--event", required=True)
    select.add_argument("--recipient",
                        help="the supervisor task. Without it contactability is not consulted"
                             " and the answer covers the obligation only")
    select.set_defaults(handler=cmd_supervisor_select)

    standing = subparsers.add_parser(
        "supervisor-standing",
        help="what a project still owes upward, with the project's own reading beside it."
             " This answers an explicit question and is not suppressed by anything")
    standing.add_argument("--project", required=True)
    standing.add_argument("--observation", action="append",
                          help="a reporting-observation/1 file from reporting-show. A turn that"
                               " ended without reporting writes no row this store can find, so"
                               " it is present only when its observation is passed in or this"
                               " store derives it from the declarations a child's relay"
                               " recorded here. Repeat once per reading")
    standing.set_defaults(handler=cmd_supervisor_standing)

    recorded = subparsers.add_parser(
        "supervisor-report-recorded",
        help="record that a report was produced for this event's obligation, once. It says a"
             " report was composed, never that one arrived")
    # Exactly one subject. An omission has no event, so an event-only surface could record a
    # report for every kind of news except the one nobody sent.
    subject = recorded.add_mutually_exclusive_group(required=True)
    subject.add_argument("--event")
    subject.add_argument("--observation",
                         help="a reporting-observation/1 file from reporting-show, for an"
                              " obligation left by a turn that ended without reporting")
    recorded.add_argument("--message", help="the envelope messageId the report was sent under")
    recorded.add_argument("--note")
    recorded.set_defaults(handler=cmd_supervisor_report_recorded)

    stage = subparsers.add_parser(
        "supervisor-stage",
        help="freeze what is owed upward as a message, before anything is sent. Staging is"
             " not sending: one obligation is one message, however often it is staged")
    # Not required here: an observation on its own is a third subject, and it is checked in
    # the handler so the refusal can say what the three are.
    subject = stage.add_mutually_exclusive_group()
    subject.add_argument("--event")
    subject.add_argument("--project",
                         help="stage every standing obligation in one project, which is what"
                              " makes this one command rather than one decision per event")
    # NOT in the exclusive group. A project-wide staging needs these readings most: a turn
    # that ended without reporting writes no row any query over this store can find, so
    # excluding them from --project left the one obligation nobody else can see unstageable
    # by the command written to stage everything.
    stage.add_argument("--observation", action="append",
                       help="a reporting-observation/1 file from reporting-show. With"
                            " --project, repeat once per reading; on its own it is the"
                            " obligation a turn left by ending without reporting")
    stage.add_argument("--recipient",
                       help="the supervisor you believe this goes to. A disagreement with the"
                            " linkage is refused rather than resolved by picking one")
    stage.set_defaults(handler=cmd_supervisor_stage)

    send = subparsers.add_parser(
        "supervisor-send",
        help="one attempt at one staged message, through the same host rules a delivery"
             " obeys. A busy recipient is never interrupted, and a held, backed-off or"
             " already-sent message answers sent: false without reaching the host")
    send.add_argument("--message", required=True)
    send.set_defaults(handler=cmd_supervisor_send)

    readback = subparsers.add_parser(
        "supervisor-read",
        help="the recipient answering a message. The proof is sha256(messageId|your own turn"
             " id), which the delivered bytes cannot contain, so an echo cannot produce it;"
             " nothing authenticates the caller, so answering from your own turn is an"
             " instruction rather than a checked property")
    readback.add_argument("--message", required=True)
    readback.add_argument("--turn", required=True, help="your own turn id")
    readback.add_argument("--proof", required=True)
    readback.add_argument("--as", dest="asserted_by", required=True,
                          help="the task asserting this readback. Checked against the"
                               " message's recipient and written down, which is a"
                               " declaration rather than an authentication: nothing on this"
                               " side can establish who is calling")
    readback.set_defaults(handler=cmd_supervisor_read)

    shown = subparsers.add_parser(
        "supervisor-show",
        help="one staged message whole: what it says, every attempt, and what came back")
    shown.add_argument("--message", required=True)
    shown.set_defaults(handler=cmd_supervisor_show)


    packet = subparsers.add_parser(
        "packet-check",
        help="whether a relay-packet/1 message carries what its purpose requires and agrees"
             " with the record its receiver read. It sends nothing; with --receiver it reads"
             " the store read-only")
    packet.add_argument("--packet", required=True,
                        help="the packet, as relay-packet/1 JSON")
    reading = packet.add_mutually_exclusive_group(required=True)
    reading.add_argument("--receiver",
                         help="your own task id: the record is built from the relay store you"
                              " select with --state, read-only, and the answer says"
                              " recordSource: store with each field's provenance")
    reading.add_argument("--record",
                         help="offline: what the receiver read for ITSELF, as a JSON file. A"
                              " field this record omits comes back unavailable rather than"
                              " accepted; relationRevision null states an unscoped"
                              " relationship. The answer says recordSource: supplied")
    packet.add_argument("--observation",
                        help="with --receiver: a JSON forge or file reading with its own"
                             " source - repository, prNumber, headSha, artifactPath,"
                             " artifactDigest. The store never holds a head")
    packet.add_argument("--ledger",
                        help="with --receiver: your own reception ledger, created if absent."
                             " It tells a repeat from a first arrival and holds the mode your"
                             " accepted assignment gave; without it act is always false")
    packet.add_argument("--applied", action="store_true",
                        help="with --receiver and --ledger, after you acted on an accepted"
                             " packet: record it applied in your ledger, so its replays say"
                             " act false. Until then a replay of it says act true. Reads no"
                             " store")
    packet.set_defaults(handler=cmd_packet_check)

    settle = subparsers.add_parser("linkage-settle")
    settle.add_argument("--directive", required=True)
    settle.add_argument("--disposition", required=True, choices=["chosen", "superseded"])
    settle.add_argument("--actor", required=True)
    settle.add_argument("--reason")
    settle.set_defaults(handler=cmd_linkage_settle)

    down = subparsers.add_parser("linkage-down")
    down.add_argument("--scope-kind", required=True,
                      choices=["initiative", "project", "issue"])
    down.add_argument("--scope", required=True)
    down.set_defaults(handler=cmd_linkage_down)

    up = subparsers.add_parser("linkage-up")
    # Exactly one starting point. Left independently optional, argparse accepted none - which
    # answered nothing - and several at once, which _starting_scope resolved by its own
    # precedence: relationship over issue over task. A caller naming a relationship AND an
    # issue got the relationship's hierarchy back and no sign the issue was never consulted.
    start = up.add_mutually_exclusive_group(required=True)
    start.add_argument("--task")
    start.add_argument("--issue")
    start.add_argument("--relationship")
    up.add_argument("--scope",
                    help="which scope to walk from when --task owns more than one. Without"
                         " it a task holding several scopes is answered as ambiguous rather"
                         " than resolved down one arbitrary branch. Goes with --task only:"
                         " --issue and --relationship already decide the starting scope")
    up.set_defaults(handler=cmd_linkage_up)

    counterpart = subparsers.add_parser("linkage-counterpart")
    counterpart.add_argument("--from-task", required=True)
    counterpart.add_argument("--to-task", required=True)
    counterpart.add_argument("--from-scope",
                             help="the sender's Linear scope. A task may own several, and"
                                  " OPS-7.4 binds both scopes to a message")
    counterpart.add_argument("--quoted-scope",
                             help="the scope the message believes it is addressing")
    counterpart.add_argument("--quoted-revision", type=int)
    counterpart.set_defaults(handler=cmd_linkage_counterpart)

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
    verdict.add_argument(
        "--restoration",
        help="the criterion id whose finding carries this correction's restoration block."
             " The verdict is refused if that finding would not reach the child.",
    )
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

    # Read-only and offline: it opens no adapter and constructs no Store. The two selectors are
    # mutually exclusive because they answer different questions - a project asks about live work,
    # a relationship asks about one named assignment whatever its status - and required because
    # enumerating every relationship in a store is not a question this command is for.
    dispositions_show = subparsers.add_parser("dispositions-show")
    dispositions_scope = dispositions_show.add_mutually_exclusive_group(required=True)
    dispositions_scope.add_argument("--project")
    dispositions_scope.add_argument("--relationship")
    dispositions_show.set_defaults(handler=cmd_dispositions_show)
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

    fault_target = subparsers.add_parser("fault-target")
    fault_target.add_argument("--scope", required=True, help="product:projectKey")
    fault_target.add_argument("--tracker-ref", required=True)
    fault_target.set_defaults(handler=cmd_fault_target)

    fault_observe = subparsers.add_parser("fault-observe")
    fault_observe.add_argument("--observation", required=True, help="JSON, or @path")
    fault_observe.set_defaults(handler=cmd_fault_observe)

    fault_sweep = subparsers.add_parser("fault-sweep")
    fault_sweep.add_argument("--product", default="crw")
    fault_sweep.add_argument("--project")
    fault_sweep.add_argument("--readings", help="reporting-observation/1 JSON list, or @path")
    fault_sweep.set_defaults(handler=cmd_fault_sweep)

    fault_show = subparsers.add_parser("fault-show")
    fault_show.add_argument("--fault")
    fault_show.add_argument("--scope")
    # NOT --state. That is the global option naming the store directory, and a subcommand
    # option of the same name overwrites it in the namespace, so every fault-show read an
    # unconfigured default store and answered that the fault did not exist.
    fault_show.add_argument("--fault-state", choices=list(faults_module.STATES))
    fault_show.add_argument("--limit", type=int, default=20,
                            help="faults per page, or occurrences and remediations with --fault")
    fault_show.add_argument("--after", type=int,
                            help="continue a listing from the next value the last page returned")
    fault_show.set_defaults(handler=cmd_fault_show)

    fault_fix = subparsers.add_parser("fault-fix")
    fault_fix.add_argument("--fault", required=True)
    fault_fix.add_argument("--ref", required=True, help="the pull request or commit")
    fault_fix.add_argument("--detail")
    fault_fix.set_defaults(handler=cmd_fault_fix)

    fault_reverify = subparsers.add_parser("fault-reverify")
    fault_reverify.add_argument("--fault", required=True)
    fault_reverify.add_argument("--method", required=True, choices=list(FAULT_METHODS))
    fault_reverify.add_argument("--ref", required=True, help="the command or reading")
    fault_reverify.add_argument("--outcome", required=True, choices=list(FAULT_OUTCOMES))
    fault_reverify.add_argument("--detail")
    fault_reverify.set_defaults(handler=cmd_fault_reverify)

    fault_resolve = subparsers.add_parser("fault-resolve")
    fault_resolve.add_argument("--fault", required=True)
    fault_resolve.set_defaults(handler=cmd_fault_resolve)

    fault_next = subparsers.add_parser("fault-next")
    fault_next.add_argument("--limit", type=int, default=4)
    fault_next.set_defaults(handler=cmd_fault_next)

    fault_claim = subparsers.add_parser("fault-claim")
    fault_claim.add_argument("--publication", required=True)
    fault_claim.add_argument("--owner", required=True)
    fault_claim.set_defaults(handler=cmd_fault_claim)

    fault_operation = subparsers.add_parser("fault-operation")
    fault_operation.add_argument("--publication", required=True)
    fault_operation.add_argument("--claim-token", required=True)
    fault_operation.set_defaults(handler=cmd_fault_operation)

    fault_reconcile = subparsers.add_parser("fault-reconcile")
    fault_reconcile.add_argument("--publication", required=True)
    fault_reconcile.add_argument("--observed", required=True, help="observed text, or @path")
    fault_reconcile.add_argument(
        "--searched", action="store_true",
        help="attest that the search covered where the block would be. Without it a negative"
             " read is not read as absence",
    )
    fault_reconcile.set_defaults(handler=cmd_fault_reconcile)

    fault_complete = subparsers.add_parser("fault-complete")
    fault_complete.add_argument("--publication", required=True)
    fault_complete.add_argument("--claim-token")
    fault_complete.add_argument("--readback", required=True, help="the text, or @path")
    fault_complete.add_argument("--external-ref")
    fault_complete.set_defaults(handler=cmd_fault_complete)

    fault_fail = subparsers.add_parser("fault-fail")
    fault_fail.add_argument("--publication", required=True)
    fault_fail.add_argument("--claim-token", required=True)
    fault_fail.add_argument("--error", required=True)
    fault_fail.set_defaults(handler=cmd_fault_fail)

    fault_retry = subparsers.add_parser("fault-retry")
    fault_retry.add_argument("--publication", required=True)
    fault_retry.set_defaults(handler=cmd_fault_retry)

    fault_prune = subparsers.add_parser("fault-prune")
    fault_prune.add_argument("--fault", required=True)
    fault_prune.add_argument("--keep", type=int, default=20)
    fault_prune.set_defaults(handler=cmd_fault_prune)


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
    # The INSTANT a launching process already decided this run must stop at, read from
    # CLOCK_MONOTONIC on this host and this boot. A supervisor writes it when it spawns a
    # worker, so the worker's own startup is spent from the segment rather than added after
    # it; a person types --deadline instead. It is not a wall clock and it does not survive a
    # reboot: that clock restarts near zero, so a value carried into a later boot sits in that
    # boot's future and names a bound much later than anyone asked for. It fails open, which
    # is why nothing writes it down and why this is not a number to type by hand.
    daemon.add_argument("--deadline-monotonic", type=float)
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
    declare = actions.add_parser("declare")
    declare.add_argument("--actor")
    # One or the other. A declaration and its removal in one command would need an order to
    # be read in, and the answer to "which policy does this service launch with" would then
    # depend on that order rather than on what was typed.
    policy = declare.add_mutually_exclusive_group(required=True)
    policy.add_argument(
        "--execution-policy",
        help="the execution policy file this service's daemon is launched with; it is read"
             " back and refused unless it resolves",
    )
    policy.add_argument(
        "--forget-execution-policy", action="store_true",
        help="drop the declaration; later launches fall back to their own environment",
    )
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
        if name == "run":
            # Only the supervisor takes the instant form, because only the supervisor is ever
            # launched by another process that had already decided when it must stop. start
            # and restart are where a person says how long, and they convert it themselves.
            hosted.add_argument("--deadline-monotonic", type=float)
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
    doctor.add_argument(
        "--require-worker-policy",
        help="JSON list (or @file) of {role,model,reasoningEffort}; refuse unless the live worker matches",
    )
    doctor.add_argument("--expect-store", help="the store id another participant reported")
    doctor.add_argument("--expect-inode", help="the device:inode another participant reported")
    doctor.add_argument(
        "--expect-log",
        help="the device:inode:name another participant reported for its write-ahead log",
    )
    doctor.add_argument("--expect-nonce", help="a nonce another participant wrote here")
    doctor.add_argument(
        "--issue",
        help="also answer whether this store holds an assignment for this issue identity",
    )
    doctor.set_defaults(handler=cmd_doctor)

    subparsers.add_parser("store-identity").set_defaults(handler=cmd_store_identity)

    challenge = subparsers.add_parser("store-challenge")
    challenge.add_argument("--write", action="store_true")
    challenge.add_argument("--read")
    challenge.add_argument("--actor")
    challenge.set_defaults(handler=cmd_store_challenge)

    def marker_command(name):
        """One subparser shape for every marker command: a root and a workspace."""
        command = subparsers.add_parser(name)
        command.add_argument("--marker-root")
        command.add_argument("--workspace", required=True)
        return command

    intent_declare = marker_command("intent-declare")
    intent_declare.add_argument("--dispatch-request-id", required=True)
    intent_declare.add_argument("--issue", required=True)
    intent_declare.add_argument("--declared-at")
    intent_declare.add_argument("--criteria-source")
    intent_declare.add_argument("--baseline-revision")
    intent_declare.add_argument("--settings", help="the authorised execution settings, as JSON")
    intent_declare.add_argument(
        "--no-db-path", action="store_true",
        help="do not record where the relay store lives; the hook must then be told explicitly",
    )
    intent_declare.set_defaults(handler=cmd_intent_declare)

    intent_attempt = marker_command("intent-attempt")
    intent_attempt.add_argument("--assignment", required=True)
    intent_attempt.add_argument("--outcome", required=True, choices=intent.ATTEMPT_OUTCOMES)
    intent_attempt.add_argument("--task-id")
    intent_attempt.set_defaults(handler=cmd_intent_attempt)

    intent_bind = marker_command("intent-bind")
    intent_bind.add_argument("--assignment", required=True)
    intent_bind.add_argument("--session", required=True)
    intent_bind.add_argument("--task-id", required=True)
    intent_bind.set_defaults(handler=cmd_intent_bind)

    intent_register = marker_command("intent-register")
    intent_register.add_argument("--assignment", required=True)
    intent_register.add_argument("--relationship", required=True)
    intent_register.add_argument("--dispatch-request-id", required=True)
    intent_register.add_argument(
        "--db-path", help="the relay store to confirm this relationship against"
    )
    intent_register.set_defaults(handler=cmd_intent_register)

    intent_claim = marker_command("intent-claim")
    intent_claim.add_argument("--assignment", required=True)
    intent_claim.add_argument("--session", required=True)
    intent_claim.add_argument("--dispatch-request-id", required=True)
    intent_claim.add_argument("--first-turn")
    intent_claim.set_defaults(handler=cmd_intent_claim)

    intent_disposition = marker_command("intent-disposition")
    intent_disposition.add_argument("--assignment", required=True)
    intent_disposition.add_argument("--session", required=True)
    intent_disposition.add_argument("--turn", required=True)
    intent_disposition.add_argument(
        "--outcome", required=True, choices=intent.DISPOSITION_OUTCOMES
    )
    intent_disposition.set_defaults(handler=cmd_intent_disposition)

    intent_resolve = marker_command("intent-resolve")
    intent_resolve.add_argument("--assignment", required=True)
    intent_resolve.add_argument("--chosen-task", required=True)
    intent_resolve.add_argument("--chosen-session", required=True)
    intent_resolve.add_argument("--reason", required=True)
    intent_resolve.add_argument(
        "--adjudicate", action="append", required=True,
        help="factId=digest, repeatable; a resolution naming nothing adjudicates nothing",
    )
    intent_resolve.set_defaults(handler=cmd_intent_resolve)

    intent_show = marker_command("intent-show")
    intent_show.add_argument("--assignment")
    intent_show.add_argument("--session")
    intent_show.add_argument("--now")
    intent_show.set_defaults(handler=cmd_intent_show)

    guard_evaluate = subparsers.add_parser("guard-evaluate")
    guard_evaluate.add_argument("--marker-root")
    guard_evaluate.add_argument(
        "--stop-input", default="-", help="the Stop payload as JSON; - reads stdin"
    )
    guard_evaluate.add_argument(
        "--mode", default=guard.OBSERVE, choices=(guard.OBSERVE, guard.HOLD),
        help="observe classifies and records without ever holding, which is the default because "
             "holding depends on per-session write isolation the caller has to have granted",
    )
    guard_evaluate.add_argument("--db-path", help="the relay store to read receipts from")
    guard_evaluate.add_argument("--now")
    guard_evaluate.add_argument("--no-record", action="store_true")
    guard_evaluate.set_defaults(handler=cmd_guard_evaluate)

    # ------------------------------------------- coordination between parents

    turn_request = subparsers.add_parser("merge-turn-request")
    turn_request.add_argument("--repository", required=True)
    turn_request.add_argument("--base-ref", required=True)
    turn_request.add_argument("--project", required=True)
    turn_request.add_argument("--task", required=True)
    turn_request.add_argument("--host", required=True)
    turn_request.add_argument("--cwd")
    turn_request.add_argument("--cxc-session")
    turn_request.add_argument("--head", required=True,
                              help="the exact candidate head this claim is for. Required,"
                                   " because a claim with no head cannot be checked against"
                                   " one later")
    turn_request.add_argument("--pr", type=int)
    turn_request.add_argument("--relationship",
                              help="optional. When given, the recorded work report's head"
                                   " must agree with --head before a merge may begin")
    turn_request.add_argument("--ready", action="store_true")
    turn_request.set_defaults(handler=cmd_merge_turn_request)

    turn_ready = subparsers.add_parser("merge-turn-ready")
    turn_ready.add_argument("--turn", required=True)
    turn_ready.add_argument("--actor", required=True)
    turn_ready.add_argument("--head", help="restating a different head resets readiness")
    turn_ready.add_argument("--cause",
                            help="what changed. A head this package can notice for itself; a"
                                 " base that moved and a finding that arrived it cannot, so"
                                 " they are stated here and recorded either way")
    readiness = turn_ready.add_mutually_exclusive_group(required=True)
    readiness.add_argument("--ready", dest="ready", action="store_true")
    readiness.add_argument("--not-ready", dest="ready", action="store_false")
    turn_ready.set_defaults(handler=cmd_merge_turn_ready)

    turn_ack = subparsers.add_parser("merge-turn-acknowledge")
    turn_ack.add_argument("--turn", required=True)
    turn_ack.add_argument("--actor", required=True)
    turn_ack.add_argument("--grant", required=True,
                          help="the grant id you read. Compared against this tenure's own, so"
                               " a grant that was returned while you were away is refused"
                               " here rather than acted on")
    turn_ack.add_argument("--evidence", required=True,
                          help="what you read. A bare acknowledgement is the remembered"
                               " message this replaces")
    turn_ack.set_defaults(handler=cmd_merge_turn_acknowledge)

    turn_attest = subparsers.add_parser("merge-turn-attest")
    turn_attest.add_argument("--turn", required=True)
    turn_attest.add_argument("--evidence-kind", required=True)
    turn_attest.add_argument("--idempotency-key", required=True)
    turn_attest.add_argument("--actor", required=True)
    turn_attest.add_argument("--evidence", required=True)
    turn_attest.set_defaults(handler=cmd_merge_turn_attest)

    turn_ask = subparsers.add_parser("merge-turn-request-return")
    turn_ask.add_argument("--turn", required=True)
    turn_ask.add_argument("--actor", required=True)
    turn_ask.add_argument("--evidence", required=True)
    turn_ask.set_defaults(handler=cmd_merge_turn_request_return)

    turn_check = subparsers.add_parser("merge-turn-check")
    turn_check.add_argument("--turn", required=True)
    turn_check.add_argument("--actor", required=True)
    turn_check.add_argument("--head-sha", required=True)
    turn_check.add_argument("--base-sha", required=True)
    turn_check.add_argument("--checks", required=True,
                            help="JSON list of {runId, name, headSha, conclusion, attempt}")
    turn_check.add_argument("--review", required=True,
                            help="JSON {hasNextPage, pagesRead, totalCount, threadsSeen,"
                                 " unresolved}")
    turn_check.add_argument("--required", action="append",
                            help="a check name branch protection requires. Repeat once per"
                                 " name. This package never contacts a forge, so the set is"
                                 " your declaration and is stored as requiredDeclared")
    turn_check.set_defaults(handler=cmd_merge_turn_check)

    turn_land = subparsers.add_parser("merge-turn-land")
    turn_land.add_argument("--turn", required=True)
    turn_land.add_argument("--actor", required=True)
    turn_land.add_argument("--landed-sha", required=True)
    turn_land.add_argument("--observed-base-sha", required=True)
    turn_land.add_argument("--evidence", required=True)
    turn_land.set_defaults(handler=cmd_merge_turn_land)

    turn_unknown = subparsers.add_parser("merge-turn-unknown")
    turn_unknown.add_argument("--turn", required=True)
    turn_unknown.add_argument("--actor", required=True)
    turn_unknown.add_argument("--reason", required=True)
    turn_unknown.set_defaults(handler=cmd_merge_turn_unknown)

    turn_resolve = subparsers.add_parser("merge-turn-resolve")
    turn_resolve.add_argument("--turn", required=True)
    turn_resolve.add_argument("--actor", required=True)
    turn_resolve.add_argument("--observed-base-sha", required=True)
    turn_resolve.add_argument("--pr-state", required=True,
                              choices=["merged", "open", "closed"],
                              help="only these three establish an outcome; anything else is"
                                   " refused at the service too")
    turn_resolve.add_argument("--evidence", required=True,
                              help="what you observed. Elapsed time is not an observation"
                                   " and never becomes one")
    turn_resolve.set_defaults(handler=cmd_merge_turn_resolve)

    turn_release = subparsers.add_parser("merge-turn-release")
    turn_release.add_argument("--turn", required=True)
    turn_release.add_argument("--actor", required=True)
    turn_release.add_argument("--disposition", required=True,
                              choices=["returned", "cancelled"])
    turn_release.add_argument("--reason", required=True)
    turn_release.add_argument("--evidence",
                              help="required for cancelled, because the holder is not the"
                                   " one saying it is finished")
    turn_release.set_defaults(handler=cmd_merge_turn_release)

    turn_withdraw = subparsers.add_parser("merge-turn-withdraw")
    turn_withdraw.add_argument("--turn", required=True)
    turn_withdraw.add_argument("--actor", required=True)
    turn_withdraw.set_defaults(handler=cmd_merge_turn_withdraw)

    turn_show = subparsers.add_parser("merge-turn-show")
    turn_show.add_argument("--turn")
    turn_show.add_argument("--repository")
    turn_show.add_argument("--base-ref")
    turn_show.add_argument("--parent-task",
                           help="every live claim one task holds, across targets. What a"
                                " parent that just restarted can ask with the only identifier"
                                " it still has")
    turn_show.set_defaults(handler=cmd_merge_turn_show)

    slot_reserve = subparsers.add_parser("slot-reserve")
    slot_reserve.add_argument("--kind", required=True)
    slot_reserve.add_argument("--subject", required=True)
    slot_reserve.add_argument("--parent-task", required=True)
    slot_reserve.add_argument("--project", required=True)
    slot_reserve.add_argument("--actor", required=True)
    slot_reserve.add_argument("--detail")
    slot_reserve.set_defaults(handler=cmd_slot_reserve)

    slot_release = subparsers.add_parser("slot-release")
    slot_release.add_argument("--kind", required=True)
    slot_release.add_argument("--subject", required=True)
    slot_release.add_argument("--actor", required=True)
    slot_release.add_argument("--reason", required=True,
                              help="the first reason wins. A later notification restates it"
                                   " rather than replacing it")
    slot_release.add_argument("--tenure", type=int,
                              help="which tenure this release settles. Required once a"
                                   " subject has been reserved more than once, because the"
                                   " newest is not necessarily the one a delayed"
                                   " notification is about")
    slot_release.set_defaults(handler=cmd_slot_release)

    limit_declare = subparsers.add_parser("limit-declare")
    limit_declare.add_argument("--scope-kind", required=True,
                               choices=["initiative", "project", "store"])
    limit_declare.add_argument("--scope", required=True)
    limit_declare.add_argument("--dimension", required=True,
                               help="runs is the only dimension this store counts for"
                                    " itself; any other needs usage-observe")
    limit_declare.add_argument("--unit", required=True)
    limit_declare.add_argument("--ceiling", type=float, required=True)
    limit_declare.add_argument("--declared-by", required=True)
    limit_declare.add_argument("--source", required=True)
    limit_declare.add_argument("--no-enforce", action="store_true")
    limit_declare.set_defaults(handler=cmd_limit_declare)

    usage_observe = subparsers.add_parser("usage-observe")
    usage_observe.add_argument("--scope-kind", required=True,
                               choices=["initiative", "project", "store"])
    usage_observe.add_argument("--scope", required=True)
    usage_observe.add_argument("--dimension", required=True)
    usage_observe.add_argument("--observed", type=float, required=True)
    usage_observe.add_argument("--observed-by", required=True)
    usage_observe.add_argument("--method", required=True)
    usage_observe.set_defaults(handler=cmd_usage_observe)

    capacity_show = subparsers.add_parser("capacity-show")
    capacity_show.add_argument("--project")
    capacity_show.add_argument("--parent-task")
    capacity_show.add_argument("--initiative")
    capacity_show.add_argument("--scope")
    capacity_show.add_argument("--scope-kind", default="project",
                               choices=["initiative", "project", "store"])
    capacity_show.set_defaults(handler=cmd_capacity_show)

    region_propose = subparsers.add_parser("region-propose")
    region_propose.add_argument("--repository", required=True)
    region_propose.add_argument("--revision", required=True)
    region_propose.add_argument("--path", required=True,
                                help="repository-relative and canonical: no leading slash,"
                                     " no '..' and no redundant separator")
    region_propose.add_argument("--kind", required=True,
                                choices=["tree", "file", "symbol", "data"])
    region_propose.add_argument("--key", help="the symbol or data key inside --path")
    region_propose.add_argument("--class", dest="region_class", default="source",
                                choices=["source", "generated"])
    region_propose.add_argument("--regenerate-from",
                                help="required for a generated region: what re-derives it")
    region_propose.add_argument("--left-project", required=True)
    region_propose.add_argument("--right-project", required=True)
    region_propose.add_argument("--peer-link", required=True)
    region_propose.add_argument("--task", required=True)
    region_propose.add_argument("--constraint", required=True)
    region_propose.add_argument("--condition")
    region_propose.add_argument("--issue")
    region_propose.add_argument("--next-owner")
    region_propose.set_defaults(handler=cmd_region_propose)

    region_settle = subparsers.add_parser("region-settle")
    region_settle.add_argument("--agreement", required=True)
    region_settle.add_argument("--actor", required=True)
    region_settle.add_argument("--disposition", required=True,
                               choices=["accepted", "declined", "withdrawn", "released"])
    region_settle.add_argument("--condition",
                               help="what a decline WOULD accept under, kept after it closes")
    region_settle.add_argument("--reason")
    region_settle.set_defaults(handler=cmd_region_settle)

    region_restate = subparsers.add_parser("region-restate-revision")
    region_restate.add_argument("--repository", required=True)
    region_restate.add_argument("--from-revision", required=True)
    region_restate.add_argument("--to-revision", required=True)
    region_restate.add_argument("--actor", required=True)
    region_restate.set_defaults(handler=cmd_region_restate_revision)

    region_reaffirm = subparsers.add_parser("region-reaffirm")
    region_reaffirm.add_argument("--agreement", required=True)
    region_reaffirm.add_argument("--actor", required=True)
    region_reaffirm.add_argument("--revision", required=True)
    region_reaffirm.set_defaults(handler=cmd_region_reaffirm)

    region_followup = subparsers.add_parser("region-followup")
    region_followup.add_argument("--agreement", required=True)
    region_followup.add_argument("--trigger", required=True)
    region_followup.add_argument("--acceptance", required=True)
    region_followup.add_argument("--recorded-by", required=True)
    region_followup.add_argument("--issue-ref")
    region_followup.add_argument("--assignee")
    region_followup.add_argument("--assignee-project")
    region_followup.set_defaults(handler=cmd_region_followup)

    followup_accept = subparsers.add_parser("region-followup-accept")
    followup_accept.add_argument("--followup", required=True)
    followup_accept.add_argument("--actor", required=True)
    followup_accept.add_argument("--assignee-project", required=True)
    followup_accept.set_defaults(handler=cmd_region_followup_accept)

    followup_settle = subparsers.add_parser("region-followup-settle")
    followup_settle.add_argument("--followup", required=True)
    followup_settle.add_argument("--actor", required=True)
    followup_settle.add_argument("--disposition", required=True,
                                 choices=["done", "dropped"])
    followup_settle.add_argument("--reason")
    followup_settle.set_defaults(handler=cmd_region_followup_settle)

    region_show = subparsers.add_parser("region-show")
    region_show.add_argument("--repository", required=True)
    region_show.add_argument("--revision")
    region_show.add_argument("--project")
    region_show.add_argument("--path")
    region_show.set_defaults(handler=cmd_region_show)



    # CRW-26. Registered last, in its own region: the managed-* regions this file is about to
    # receive belong to another task, and a command wedged between them is a merge conflict for
    # no reason. Read-only, and the only command here that reaches a forge.
    evidence = subparsers.add_parser(
        "merge-evidence",
        help="read a pull request's review, checks and declared gates, and report whether the"
             " candidate is ready - enumerated to the end rather than asserted")
    evidence.add_argument("--repository", required=True, help="owner/name")
    evidence.add_argument("--pull-request", required=True, type=int)
    evidence.add_argument("--restate",
                          help="a handoff record or an earlier snapshot to grade against a fresh"
                               " reading; '-' reads it from stdin")
    evidence.add_argument("--restate-head",
                          help="the head the restated record is about, when the record itself"
                               " does not name one")
    evidence.add_argument("--page-size", type=int, default=100)
    evidence.add_argument("--page-budget", type=int, default=50,
                          help="how many pages one connection may take before the read is"
                               " reported unfinished rather than answered")
    evidence.add_argument("--call-budget", type=int, default=300)
    evidence.add_argument("--timeout", type=int, default=60)
    evidence.set_defaults(handler=cmd_merge_evidence)

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


# What a Stop hook's refusal says in place of the final line below. That line tells an operator to
# pass --state, which settles the ambiguity for every command except guard-evaluate: for a Stop, a
# directory chosen for one run is exactly what is refused. These say only what the code guarantees
# and prescribe nothing that writes, because which store holds an assignment, and what a host's
# environment pins, are facts this refusal cannot observe.
_STOP_RECOVERY = (
    "  this Stop was released without being judged. It is not judged again later, and a --state"
    " or CODEX_SESSION_RELAY_STATE does not change that: a directory chosen for one run is not"
    " one anybody recorded",
    "  a later Stop of this assignment is judged against a store when the hook's settings name it"
    " with --db-path, or the intent records it as dbPath (both are written once, so neither can"
    " be added to an existing installation or assignment), or when discovery for this socket"
    " names exactly one store and nothing pins another directory for the hook",
    "  which of the candidates holds this assignment is not something this refusal can"
    " establish; the lines above read them without changing anything",
)


def _recovery_commands(services, selection, contested: bool, *, for_stop=False) -> list:
    """Complete commands for an operator who has only this refusal to work from.

    Every one of them READS. Provenance is recorded by opening a store with a socket, which
    is exactly what the refusal prevented, so inspection is safe to repeat and none of these
    adopts anything by running.

    The socket travels on each command deliberately. Dropping it would compare the candidates
    under different conditions from the ones that produced the refusal, and for the reason
    the doctor exemption exists in the first place: the socket is what makes two stores
    candidates for each other.

    for_stop is guard-evaluate's refusal. Its first line drops CODEX_SESSION_RELAY_STATE when this
    process has it, because a hook that inherited the variable hands it to whoever pastes the line
    in the same environment, and doctor under a pinned directory does not look for siblings. And
    its closing lines are _STOP_RECOVERY rather than the advice to pass --state.
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
        f"{_without_state_env() if for_stop else ''}{program}{socket} doctor",
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
    if for_stop:
        lines.extend(_STOP_RECOVERY)
        return lines
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
    #
    # resolve() rather than absolute(), because absolute() keeps dot segments: /x/a/../store
    # and /x/store are one directory and one database, and comparing the spellings called
    # them two. The question being asked here is whether this is the same STORE, not whether
    # it is the same string.
    try:
        resolved = Path(pinned).expanduser().resolve()
    except (OSError, RuntimeError, ValueError):
        # ~someone whose home this host cannot resolve. The variable is never validated at
        # startup when --state overrides it, so this is the first thing that touches it - and
        # a refusal payload that becomes a traceback leaves the operator with nothing at all.
        # Said rather than dropped: it is the value they set, and it is not usable.
        #
        # Wider than the one failure measured here, on purpose. Only RuntimeError reproduces
        # on CPython 3.14.4 - an unsearchable parent returns the path rather than raising, and
        # so does an over-long one - but resolve() touches the filesystem and this class of
        # escape has already cost a refusal its whole payload once.
        lines.append(
            f"  {STATE_ENV} is set to {pinned!r}, which names a home directory that does not"
            " resolve on this host, so it is not offered as a candidate"
        )
        return lines
    if resolved != Path(selection.path).expanduser().resolve():
        lines.append(
            f"{_program()} --state={_quote(resolved)} --socket={_quote(wanted)} doctor"
        )
        lines.append(
            f"  reads the directory {STATE_ENV} names, which --state overrode on this run"
        )
    return lines

def _reads_no_selected_store(args) -> bool:
    """Whether this command can answer without the store default discovery would pick.

    The managed marker exists so that a hook can answer without asking the relay anything, and every
    marker command writes or reads the marker root alone. Left inside the store-selection refusal,
    an unrelated ambiguity in relay discovery made guard-evaluate exit 2 without classifying or
    recording the Stop, even when --db-path named the receipt database explicitly: legacy state
    nobody was using switched the hook off.

    intent-declare is the one exception, because it RECORDS services.selection.db_path into the
    intent for the hook to use later. Recording a path chosen by a guess is exactly what the
    refusal prevents, so it stays guarded unless --no-db-path says not to record one.

    guard-evaluate answers True here and is asked again later, which is not the same as never being
    asked. It reads receipts from the first of three sources that answers, and only the third is
    discovery's: with no --db-path and no dbPath in the intent it falls back to the selection, so
    the blanket exemption used to suppress a refusal about a store the guard then opened. The
    question cannot be settled on this line, because whether the coordinator recorded a path is a
    fact in a marker this command has not read yet: the workspace it belongs to arrives inside the
    Stop payload, on stdin. So cmd_guard_evaluate carries the question to the moment that third
    source would be used, and refuses there. See _guard_fallback.
    """
    handler = getattr(args, "handler", None)
    if handler is cmd_intent_declare:
        return bool(getattr(args, "no_db_path", False))
    if handler is cmd_merge_evidence:
        # It opens no store at all. An unrelated ambiguity in relay discovery used to refuse a
        # command whose whole subject is a pull request, which is the same class of failure the
        # marker exemption exists for.
        return True
    if handler is cmd_intent_register:
        # It confirms the relationship against a store, so it is only marker-only when the caller
        # named which store rather than letting discovery guess one.
        return bool(getattr(args, "db_path", None))
    return handler in MARKER_COMMANDS


def _selection_refusal(services, *, for_stop=False):
    """What is wrong with the store this run resolved for itself, as a payload, or None.

    Separated from the refusal so one question can be asked at two moments. Every other command
    asks it before its handler runs, where the answer is a refusal. guard-evaluate cannot: whether
    the coordinator recorded a receipt store is a fact in a marker whose workspace arrives inside
    the Stop payload, so it asks the same question again at the point that would consume this
    selection, and only if it gets there. Deciding nothing here is what makes that possible.

    Falling through to the canonical directory is not the neutral outcome it looks like. The
    first command that writes there creates a THIRD empty database, and once that exists it
    wins every later resolution and hides the assignments and pending deliveries in both of
    the others. Refusing costs one command; the third store costs the state.

    The same refusal covers a store that records NO socket. Its directory hash cannot be
    inverted, so if it is this socket's - created from a spelling we cannot reconstruct - then
    creating a canonical database beside it hides it just as permanently. That case fires only
    when a store would be created; an existing canonical store has already settled it.

    An explicit --state or environment override never arrives here: both return from
    resolve_state_dir before any discovery runs, because a caller who named a directory has
    already decided which participants share it. guard-evaluate is the one caller that asks what
    discovery would have said anyway, through _override_refusal, and for_stop is that caller's.
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
        if recorded is not None and recorded != wanted:
            return {
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
            }
    return _ambiguity_refusal(services, selection, for_stop=for_stop)


def _ambiguity_refusal(services, selection, *, for_stop=False):
    """The ambiguous or unidentified refusal for this selection, or None when it has neither.

    Takes the selection rather than reading services.selection, because guard-evaluate asks it
    about the selection discovery WOULD have made when an override skipped discovery.
    """
    if not (selection.ambiguous or selection.unidentified):
        return None
    contested = bool(selection.ambiguous)
    return {
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
        "recover": _recovery_commands(services, selection, contested, for_stop=for_stop),
    }


def _refuse_ambiguous_state(services, args) -> None:
    """Refuse before the handler runs, unless this command can answer without that store.

    doctor and ack-proof are exempt for opposite reasons. doctor is how an operator finds out
    which store to pass to --state, so refusing it would remove the only way out. ack-proof is
    a derivation over its own two arguments that opens no store at all. The marker commands are
    exempt because a legacy store nobody is using must not be able to switch a Stop hook off;
    _reads_no_selected_store owns which of them that is unconditional for.

    The exemption is settled before the selection is examined, not after. An exempt command used
    to pay for a read of the resolved store's recorded socket only to have the answer discarded,
    and one of them runs inside a five-second hook budget.

    This guard is on the command line rather than on Services.store. A library caller that
    builds Services itself bypasses it; every in-process caller in this package passes an
    explicit directory, and raising from a property would turn a diagnostic into a crash.
    """
    if getattr(args, "handler", None) in (cmd_doctor, cmd_ack_proof) or _reads_no_selected_store(
        args
    ):
        return
    refusal = _selection_refusal(services)
    if refusal is not None:
        raise PayloadExit(refusal, EXIT_REFUSED)


def main(argv=None) -> int:
    import os

    from . import rolepolicy

    parser = build_parser()
    args = parser.parse_args(argv)
    services = None
    try:
        services = Services(args)
        _refuse_ambiguous_state(services, args)
        # A supervisor started HERE - in the foreground, or by a unit - is the same daemon
        # `service start` spawns, so it runs on the same declaration. Resolved before the
        # snapshot below, because that snapshot is what this whole process then enforces.
        #
        # A supervisor launched BY that service already carries its decision in this
        # environment, and says so. Re-reading the declaration here would let one written in
        # the meantime refuse a launch whose predecessor has already been stopped.
        if (getattr(args, "service_command", None) == "run"
                and _launch_already_settled(args, os.environ) is None):
            refused = _apply_launch_policy(_service_for(services), os.environ)
            if refused is not None:
                raise PayloadExit(refused, EXIT_REFUSED)
        # Taken here, before any role question and before the handler, for the same reason the
        # bridge builds its policy in its own main(): the snapshot is supposed to be this
        # PROCESS's, and a lazy first read made it the snapshot of whenever a role question
        # first came up. A daemon could then start under one version of the file, serve unbound
        # work for hours, and adopt an edit the bridge had never seen -- two processes
        # enforcing different policies with neither one restarted, which is exactly the second
        # policy source this is built to keep visible. Nothing above asks a role question, and
        # it cannot fail startup: an unreadable or absent policy resolves to Unresolved, which
        # withholds rather than raises.
        rolepolicy.declared()
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
