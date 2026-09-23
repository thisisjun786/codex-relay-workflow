"""The one door from product routing into the fault ledger.

Routing decides where an incident belongs; the ledger owns everything that follows - identity,
suppression, targets and project readback, adoption, moves, updates, budgets, notifications,
publication kinds and the outbox. Every ledger call routing makes goes through a method here,
and nothing else in routing imports the ledger. That keeps the boundary the coordination parent
drew on 2026-09-23 in one place: CRW-206 builds no second copy of anything the ledger owns.

The methods are bound to CRW-205's corrected contract (docs/faults.md, "Corrected contract
(CRW-205 follow-up)"). The port opens only when that contract is present: every function and
keyword it needs exists and the ledger's unassigned-workspace sentinel is the one placement
uses. Otherwise every method refuses with route_ledger_pending and names what is missing. There
is deliberately no stand-in: a routing path that seemed to work against a local imitation of
adoption or targets would prove nothing about the real ledger, and a store written through one
would carry identities the real ledger never produces.
"""

import inspect

from . import faults
from .errors import RefusalReason
from .placement import UNASSIGNED
from .products import RouteRefused

# What the corrected contract must provide before anything binds: module names, and each ledger
# method with the keywords routing passes it. A partial contract binds nothing.
MODULE_FUNCTIONS = {
    "observation": ("product", "fault_class", "severity", "signature", "occurrence_key", "scope",
                    "observed_at", "detail", "evidence", "cleared"),
    "register_class": ("component", "clears", "threshold"),
    "register_kind": ("creates", "requires_issue", "target", "evidence", "confirm", "validate",
                      "pre_issue"),
    "target_key": ("workspace", "project"),
}
LEDGER_METHODS = {
    "canonical_id": ("workspace",),
    "record": ("adopt",),
    "get": (),
    "remediations": ("limit",),
    "snapshot": ("product", "fault_class", "state", "limit", "after"),
    "set_target": ("product", "workspace", "project", "team", "project_ref"),
    "adopt": ("external_ref", "scope"),
    "move": ("scope",),
    "request_update": ("op", "value"),
    "consume": ("ref",),
    "budget": (),
    "raise_notification": ("reason", "ref"),
    "notifications": ("state", "limit", "after"),
    "queue": ("kind", "trigger", "payload"),
    "publication": (),
    "publications": ("kind", "state", "limit", "after"),
    "cancel": ("reason",),
    "claim": ("owner", "takeover"),
    "operation": ("claim_token",),
    "complete": ("claim_token", "readback", "external_ref", "project_ref", "observed"),
    "fail": ("claim_token", "error", "ended"),
    "reconcile": ("observed_text", "searched", "observed", "prior_ended", "reason"),
    "record_fix": ("ref", "detail"),
    "record_reverification": ("method", "ref", "outcome", "detail"),
    "resolve": (),
}

# The ledger's fault states, named once for routing. A fault in one of ACTIVE is still being
# worked on; resolved and withdrawn are the two ways one ends.
OBSERVED, OPEN, FIX_PENDING, RESOLVED, WITHDRAWN = (
    faults.OBSERVED, faults.OPEN, faults.FIX_PENDING, faults.RESOLVED, faults.WITHDRAWN)
ACTIVE = (OBSERVED, OPEN, FIX_PENDING)
ENDED = (RESOLVED, WITHDRAWN)
FIX = faults.FIX

# The port's own surface, one name per capability. A test holds every one of them to the gate.
CAPABILITIES = (
    "fault_id", "observation", "register_class", "record", "get", "link", "remediations", "list",
    "ensure_target", "adopt", "move", "update", "consume", "budget", "notify", "notifications",
    "register_kind", "queue", "publication", "publications", "cancel", "operation", "complete",
    "claim", "fail", "reconcile", "record_fix", "record_reverification", "resolve",
)

# Ledger refusals routing tells apart BY VALUE: an owner who cannot take the fault now (another
# issue, an issued create, another workspace) becomes a hold somebody decides. A contract that
# renamed one of them would turn that hold into an uncaught refusal without failing a name or
# keyword check, so the gate requires these values too.
OWNER_REFUSALS = ("fault_adopt_conflict", "fault_scope_conflict")


def missing(module=None) -> list:
    """What the corrected contract still lacks, or nothing when the port can bind.

    Names AND every keyword the port passes: a contract that has grown the names but not yet the
    arguments binds nothing, so no call can fail half way through a routing transaction.
    """
    module = faults if module is None else module
    gaps = []
    for name, keywords in MODULE_FUNCTIONS.items():
        gaps.extend(_signature_gaps(getattr(module, name, None), f"faults.{name}", keywords))
    if not hasattr(module, "UNASSIGNED"):
        gaps.append("faults.UNASSIGNED")
    elif module.UNASSIGNED != UNASSIGNED:
        gaps.append(f"faults.UNASSIGNED == {UNASSIGNED!r}")
    ledger = getattr(module, "FaultLedger", None)
    for name, keywords in LEDGER_METHODS.items():
        gaps.extend(_signature_gaps(getattr(ledger, name, None), f"FaultLedger.{name}", keywords))
    reasons = {getattr(member, "value", None)
               for member in (getattr(module, "RefusalReason", None) or ())}
    gaps.extend(f"RefusalReason({value!r})" for value in OWNER_REFUSALS if value not in reasons)
    return gaps


def _signature_gaps(function, label, keywords):
    """The keywords this function cannot take BY KEYWORD.

    A parameter of the right name is judged by its kind first: positional-only is a gap even
    when a **kwargs catch-all is present, because Python binds the name to the positional slot
    and refuses it as a keyword. Only a name the function does not declare at all is accepted
    through **kwargs.
    """
    if function is None:
        return [label]
    parameters = inspect.signature(function).parameters
    catch_all = any(p.kind is inspect.Parameter.VAR_KEYWORD for p in parameters.values())
    by_keyword = (inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)
    gaps = []
    for keyword in keywords:
        parameter = parameters.get(keyword)
        if parameter is None:
            if not catch_all:
                gaps.append(f"{label}({keyword}=)")
        elif parameter.kind not in by_keyword:
            gaps.append(f"{label}({keyword}=)")
    return gaps


def register_kind(name, **declaration) -> list:
    """Register a publication kind in this process, at import, when the contract is present.

    Kinds are registered per process by importing the module that declares them, which has no
    store to hand; so this is a module-level act. It answers what the contract still lacks: an
    empty list means the kind is registered, and on a checkout without the corrected contract it
    registers nothing and names why, while the port's own methods keep refusing.
    """
    gaps = missing()
    if not gaps:
        faults.register_kind(name, **declaration)
    return gaps


def register_classes(classes) -> list:
    """Register routing's fault classes in this process, at import, when the contract is
    present. Like a kind, a class is a process-level declaration the ledger consults on every
    record; the answer is what the contract still lacks, empty once registered."""
    gaps = missing()
    if not gaps:
        for name, declaration in classes.items():
            faults.register_class(name, component=declaration["component"],
                                  clears=declaration["clears"],
                                  threshold=declaration.get("threshold"))
    return gaps


def _rows(answer, key):
    """A listing's rows. The contract names what a listing returns, not whether it arrives
    wrapped; routing reads rows in one place so a wrapper change is one edit."""
    if isinstance(answer, dict):
        return list(answer.get(key) or [])
    return list(answer or [])


class LedgerPort:
    """Routing's view of the fault ledger, bound only to the corrected contract."""

    def __init__(self, store, clock):
        self.store = store
        self.clock = clock
        self.missing = missing()
        self._ledger = None if self.missing else faults.FaultLedger(store, clock)

    def _require(self, capability):
        if self.missing:
            shown = ", ".join(self.missing[:6]) + (" ..." if len(self.missing) > 6 else "")
            raise RouteRefused(
                RefusalReason.ROUTE_LEDGER_PENDING,
                f"{capability} needs CRW-205's corrected ledger contract; this checkout lacks"
                f" {shown}",
            )
        return self._ledger

    def ready(self, capability):
        """Refuse now, naming the routing path, when the contract is absent. For a path that
        would otherwise answer from routing's own rows alone and look complete without the
        ledger state it exists to show."""
        self._require(capability)

    # ------------------------------------------------------------------ identity and records

    def fault_id(self, product, workspace, fault_class, signature):
        """The id the store keeps this failure under, aliases resolved, before any record."""
        return self._require("fault_id").canonical_id(product, fault_class, signature,
                                                      workspace=workspace)

    def observation(self, *, product, workspace, fault_class, severity, signature,
                    occurrence_key, project=None, observed_at=None, detail="", evidence=(),
                    cleared=False):
        """One fault-observation/1. The workspace and project travel in the scope; a fault
        recorded without a workspace (CRW's own, for one) is observed without one, which is the
        only way to reach its id, and the contract refuses a present but empty one."""
        self._require("observation")
        scope = {}
        if workspace:
            scope["workspace"] = workspace
        if project:
            scope["projectKey"] = project
        return faults.observation(product=product, fault_class=fault_class, severity=severity,
                                  signature=signature, occurrence_key=occurrence_key,
                                  scope=scope, observed_at=observed_at, detail=detail,
                                  evidence=evidence, cleared=cleared)

    def register_class(self, name, *, component, clears):
        self._require("register_class")
        return faults.register_class(name, component=component, clears=clears)

    def record(self, observation, *, adopt=None):
        """Record, adopting an existing issue in the same transaction when one owns it."""
        return self._require("record").record(observation, adopt=adopt)

    def get(self, fault_id):
        return self._require("get").get(fault_id)

    def link(self, fault_id):
        row = self._require("link").get(fault_id) or {}
        return {"linkState": row.get("linkState"), "linkedProject": row.get("linkedProject")}

    def remediations(self, fault_id, *, limit=20):
        return self._require("remediations").remediations(fault_id, limit=limit)

    def list(self, *, product=None, fault_class=None, state=None, limit=20, after=None):
        return self._require("list").snapshot(product=product, fault_class=fault_class,
                                              state=state, limit=limit, after=after)

    # ------------------------------------------------------------------ where writes go

    def ensure_target(self, *, product, workspace, project, team, project_ref):
        return self._require("ensure_target").set_target(
            product=product, workspace=workspace, project=project, team=team,
            project_ref=project_ref)

    def adopt(self, fault_id, *, external_ref, scope):
        return self._require("adopt").adopt(fault_id, external_ref=external_ref, scope=scope)

    def move(self, fault_id, *, scope):
        return self._require("move").move(fault_id, scope=scope)

    def update(self, fault_id, *, op, value):
        return self._require("update").request_update(fault_id, op=op, value=value)

    # ------------------------------------------------------------------ budgets and notices

    def consume(self, product, kind, *, ref):
        return self._require("consume").consume(product, kind, ref=ref)

    def budget(self, product, kind):
        return self._require("budget").budget(product, kind)

    def notify(self, fault_id, *, reason, ref):
        return self._require("notify").raise_notification(fault_id, reason=reason, ref=ref)

    def notifications(self, *, state=None, limit=20, after=None):
        return self._require("notifications").notifications(state=state, limit=limit,
                                                            after=after)

    # ------------------------------------------------------------------ publication kinds

    def register_kind(self, name, **declaration):
        self._require("register_kind")
        return register_kind(name, **declaration)

    def queue(self, fault_id, *, kind, trigger, payload=None):
        return self._require("queue").queue(fault_id, kind=kind, trigger=trigger,
                                            payload=payload)

    def publication(self, publication_id):
        return self._require("publication").publication(publication_id)

    def publications(self, fault_id, *, kind=None, state=None, limit=20, after=None):
        """The fault's writes as a list of rows, whatever wrapper the ledger returns them in."""
        return _rows(self._require("publications").publications(
            fault_id, kind=kind, state=state, limit=limit, after=after), "publications")

    def cancel(self, publication_id, *, reason):
        return self._require("cancel").cancel(publication_id, reason=reason)

    # ------------------------------------------------------------------ the holder surface

    def claim(self, publication_id, *, owner, takeover=False):
        return self._require("claim").claim(publication_id, owner=owner, takeover=takeover)

    def operation(self, publication_id, *, claim_token):
        return self._require("operation").operation(publication_id, claim_token=claim_token)

    def complete(self, publication_id, *, claim_token=None, readback=None, external_ref=None,
                 project_ref=None, observed=None):
        return self._require("complete").complete(
            publication_id, claim_token=claim_token, readback=readback,
            external_ref=external_ref, project_ref=project_ref, observed=observed)

    def fail(self, publication_id, *, claim_token, error, ended=False):
        return self._require("fail").fail(publication_id, claim_token=claim_token, error=error,
                                          ended=ended)

    def reconcile(self, publication_id, *, observed_text=None, searched=False, observed=None,
                  prior_ended=False, reason=None):
        return self._require("reconcile").reconcile(
            publication_id, observed_text=observed_text, searched=searched, observed=observed,
            prior_ended=prior_ended, reason=reason)

    # ------------------------------------------------------------------ remediation loop

    def record_fix(self, fault_id, *, ref, detail=""):
        return self._require("record_fix").record_fix(fault_id, ref=ref, detail=detail)

    def record_reverification(self, fault_id, *, method, ref, outcome, detail=""):
        return self._require("record_reverification").record_reverification(
            fault_id, method=method, ref=ref, outcome=outcome, detail=detail)

    def resolve(self, fault_id):
        return self._require("resolve").resolve(fault_id)
