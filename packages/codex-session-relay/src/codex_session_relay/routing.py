"""Product routing: the registry and bindings routing reads, and where incidents go.

The coordination parent (the credential holder) keeps two snapshots current here: the registry,
one record per product, and the bindings, the projects and issues it read back from Linear. Every
routing decision is made from those snapshots and from this store's own records, never from what
an incident says about itself - the incident's claim that a run belongs to an issue, for
instance, is checked against the relationship the relay registered for that run.

Writes to the ledger go through ledger_port and nothing else. See docs/product-routing.md.

Importing this module imports projects, which registers routing's fault classes and its
project_create kind with the ledger in this process; on a checkout without CRW-205's corrected
contract it registers nothing and every ledger-backed path refuses with route_ledger_pending.
"""

import json

from . import completion, digest, intake, projects, routes
from . import products
from .errors import RefusalReason
from .ledger_port import LedgerPort


def _simulated_use(db, product) -> list:
    """What of this product lives on its test target: simulated routes and test bindings."""
    used = [row["fault_id"] for row in db.execute(
        "SELECT fault_id FROM incident_routes WHERE product_key = ? AND origin = ? LIMIT 5",
        (product, products.SIMULATED)).fetchall()]
    for row in db.execute("SELECT ref, record FROM product_bindings WHERE product_key = ?",
                          (product,)).fetchall():
        if json.loads(row["record"]).get("test"):
            used.append(row["ref"])
    return used


def _real_on(db, product, project) -> list:
    """What of this product does real work in a project: bindings not marked test that name it,
    and routes of observed incidents that target it."""
    used = []
    for row in db.execute("SELECT kind, ref, record FROM product_bindings WHERE product_key = ?",
                          (product,)).fetchall():
        record = json.loads(row["record"])
        where = row["ref"] if row["kind"] == "project" else record.get("project")
        if not record.get("test") and where == project:
            used.append(row["ref"])
    used += [row["fault_id"] for row in db.execute(
        "SELECT fault_id FROM incident_routes WHERE product_key = ? AND origin != ?"
        " AND json_extract(target, '$.project') = ? LIMIT 5",
        (product, products.SIMULATED, project)).fetchall()]
    return used


class ProductRouter:
    def __init__(self, store, clock, port=None):
        self.store = store
        self.clock = clock
        self.port = port if port is not None else LedgerPort(store, clock)

    # ------------------------------------------------------------------ registry

    def register_product(self, record) -> dict:
        """Record what routing knows about one product. Replaces the previous record whole.

        The workspace is part of every routed fault's identity, so a product that has routes
        keeps its workspace: a registry naming another would file the same defects again under
        new identities. Every other field can change, and the product's held routes, and filed
        ones whose fault owns no issue yet, are decided again against the new record in the
        same transaction, as a binding decides them. A product nothing was routed for yet
        touches no ledger at all.
        """
        registry = products.read_registry(record)
        now = self.clock.iso()
        with self.store.composing() as db:
            previous = db.execute("SELECT record FROM product_registry WHERE product_key = ?",
                                  (registry["product"],)).fetchone()
            before = json.loads(previous["record"]) if previous else None
            if before is not None and before["workspace"] != registry["workspace"] and db.execute(
                    "SELECT 1 FROM incident_routes WHERE product_key = ? LIMIT 1",
                    (registry["product"],)).fetchone():
                products.refuse(RefusalReason.ROUTE_STATE_CONFLICT,
                                f"{registry['product']} has routed faults in workspace"
                                f" {before['workspace']}; the workspace is part of their"
                                f" identity, so a registry naming {registry['workspace']} would"
                                f" file the same defects again")
            if before is not None and before["testTarget"] != registry["testTarget"] and (
                    _simulated_use(db, registry["product"])):
                products.refuse(RefusalReason.ROUTE_STATE_CONFLICT,
                                f"{registry['product']} has simulated routes or test bindings"
                                f" on its test target {before['testTarget']}; they would be"
                                f" left on a target the product no longer names")
            target = registry["testTarget"]
            real = _real_on(db, registry["product"], target["project"]) if target else []
            if real:
                # One owned target per product, workspace and project: a test target on a
                # project real work uses would let a simulated record repoint that work's
                # writes to the test team.
                products.refuse(RefusalReason.ROUTE_STATE_CONFLICT,
                                f"{registry['product']} does real work in {target['project']}"
                                f" ({', '.join(real)}); a test target there would share that"
                                f" project's target with it")
            db.execute(
                "INSERT INTO product_registry (product_key, record, recorded_at) VALUES (?,?,?)"
                " ON CONFLICT(product_key) DO UPDATE SET record = excluded.record,"
                "   recorded_at = excluded.recorded_at",
                (registry["product"], products.canonical(registry), now))
            redecided = intake.redecide(self, registry["product"]) if before else []
            # Creates queued for what the registry said before, and goals whose members the
            # redecision just moved, are settled with the new record in the same transaction.
            revised = projects.revise(self, registry["product"]) if before else None
        return {**registry, "redecided": redecided,
                "projectsRevised": revised or {"cancelled": [], "queued": []}}

    def registry(self, product):
        row = self.store.one(
            "SELECT record FROM product_registry WHERE product_key = ?", (product,))
        return json.loads(row["record"]) if row else None

    def registries(self) -> dict:
        rows = self.store.all("SELECT record FROM product_registry ORDER BY product_key")
        answer = {}
        for row in rows:
            record = json.loads(row["record"])
            answer[record["product"]] = record
        return answer

    def bind(self, record) -> dict:
        """Record one project or issue as read back from Linear, against its product's registry.

        The registry is required first: a binding for a product routing does not know cannot be
        checked against a test target, and a test binding that sat on a real project is exactly
        how a simulated incident would end up commenting on a real issue.

        A binding can settle what routing held for want of it: every held route of the product,
        and every filed one whose fault owns no issue yet, is decided again from its latest
        stored incident. A product nothing was routed for yet touches no ledger at all.
        """
        product = record.get("product") if isinstance(record, dict) else None
        now = self.clock.iso()
        # One transaction with the decisions it settles: a binding whose consequences were
        # refused is not left behind as a snapshot routing never acted on. The registry it is
        # checked against is read inside it too, so a test target changed meanwhile is the one
        # the binding is checked against or waits for this binding.
        with self.store.composing() as db:
            registry = self.registry(product) if isinstance(product, str) else None
            if registry is None:
                products.refuse(RefusalReason.ROUTE_PRODUCT_UNKNOWN,
                                f"{product!r} is not a registered product; register it first")
            binding = products.read_binding(record, registry)
            db.execute(
                "INSERT INTO product_bindings (product_key, kind, ref, record, observed_at,"
                "  recorded_at) VALUES (?,?,?,?,?,?)"
                " ON CONFLICT(product_key, kind, ref) DO UPDATE SET record = excluded.record,"
                "   observed_at = excluded.observed_at, recorded_at = excluded.recorded_at",
                (binding["product"], binding["kind"], binding["ref"],
                 products.canonical(binding), binding["observedAt"], now))
            redecided = intake.redecide(self, binding["product"])
            # A binding that moved a member out of a goal changes that goal's create: it is
            # evaluated again with the members that are left, in the same transaction.
            evaluated = projects.evaluate(self, binding["product"])
        return {**binding, "redecided": redecided, "projectsQueued": evaluated["queued"]}

    def bindings(self, product) -> list:
        rows = self.store.all(
            "SELECT record FROM product_bindings WHERE product_key = ? ORDER BY kind, ref",
            (product,))
        return [json.loads(row["record"]) for row in rows]

    def set_policy(self, record) -> dict:
        """The explicit project creation policy. Without one routing never creates a project.

        In the same transaction every product's unissued creates that the new policy no longer
        allows are withdrawn, and their proposals settled, so a policy switched off leaves no
        proposal waiting on a create that will never be issued."""
        policy = products.read_policy(record)
        now = self.clock.iso()
        with self.store.composing() as db:
            db.execute(
                "INSERT INTO routing_policy (policy_key, record, basis, recorded_at)"
                " VALUES (?,?,?,?) ON CONFLICT(policy_key) DO UPDATE SET"
                "   record = excluded.record, basis = excluded.basis,"
                "   recorded_at = excluded.recorded_at",
                (policy["policy"], products.canonical(policy), policy["basis"], now))
            withdrawn = {product: projects.withdraw(self, product)
                         for product in self.registries()}
        return {**policy, "withdrawn": {p: c for p, c in withdrawn.items() if c}}

    def policy(self, key=products.PROJECT_CREATION):
        row = self.store.one("SELECT record FROM routing_policy WHERE policy_key = ?", (key,))
        return json.loads(row["record"]) if row else None

    def show_products(self, product=None) -> dict:
        """Each product with its bindings and what it watches. A surface nobody connected reads
        unobserved, which is a different fact from a watched surface that stayed quiet."""
        registries = self.registries()
        if product is not None:
            if product not in registries:
                products.refuse(RefusalReason.ROUTE_PRODUCT_UNKNOWN,
                                f"{product!r} is not a registered product")
            registries = {product: registries[product]}
        return {"products": [
            {**registry, "coverage": products.coverage(registry),
             "bindings": self.bindings(key)}
            for key, registry in registries.items()],
            "policy": self.policy()}

    # ------------------------------------------------------------------ evidence

    def run_issue(self, run):
        """The issue this store's own record says a managed run belongs to, or None.

        An incident saying "this failure is the current issue's" is a claim; this is the relay's
        record of which issue the run was registered for, and only it can make the claim hold.
        """
        if not isinstance(run, str) or not run:
            return None
        row = self.store.one(
            "SELECT issue_key FROM relationships WHERE relationship_id = ?", (run,))
        return row["issue_key"] if row else None

    # ------------------------------------------------------------------ ledger-backed paths

    def intake(self, record) -> dict:
        """Route one incident. An unwatched surface is refused before the ledger is asked."""
        return intake.intake(self, record)

    def classify(self, fault_id, record) -> dict:
        """Refuses before the ledger when the classified product does not watch a stored
        incident's surface, as an intake from that surface would have been."""
        return intake.classify(self, fault_id, record)

    def reconcile(self, *, product=None, limit=50, after=None) -> dict:
        self.port.ready("route-reconcile")
        return intake.reconcile(self, product=product, limit=limit, after=after)

    def evaluate_projects(self, product) -> dict:
        self.port.ready("route-projects")
        if self.registry(product) is None:
            products.refuse(RefusalReason.ROUTE_PRODUCT_UNKNOWN,
                            f"{product!r} is not a registered product")
        return projects.evaluate(self, product)

    def check_completion(self, record) -> dict:
        self.port.ready("completion-check")
        return completion.check(self, record)

    def digest(self, *, limit=500, after=None) -> dict:
        return digest.digest(self, limit=limit, after=after)

    def show(self, product=None, *, attention=False, limit=20, after=None) -> dict:
        """Routes with their faults' ledger state. With attention, only those waiting on a
        decision. Project proposals are listed apart: they are plans, not defects."""
        self.port.ready("route-show")
        page = routes.listing(self.store, product=product, limit=limit, after=after)
        shown, proposals = [], []
        for route in page["routes"]:
            row = self.port.get(route["fault_id"]) or {}
            now = routes.snapshot(route, row)
            waiting = routes.attention(now)
            target = route["target"]
            entry = {"faultId": route["fault_id"], "product": route["product_key"],
                     "workspace": route["workspace"], "disposition": route["disposition"],
                     "stage": route["stage"], "hold": target["hold"],
                     "unverifiedCause": target.get("unverifiedCause"),
                     "project": target["project"], "owner": target["owner"],
                     "team": target["team"], "origin": route["origin"],
                     "classification": route["classification"],
                     "supersededBy": route["superseded_by"], "detail": route["detail"],
                     "attention": waiting,
                     "ledger": {"state": now["state"], "severity": now["severity"],
                                "occurrences": now["occurrenceCount"],
                                "issue": now["externalRef"], "linkState": now["linkState"],
                                "linkedProject": row.get("linkedProject")}}
            if route["disposition"] == products.PROJECT_PROPOSAL:
                if not attention or waiting is not None:
                    proposals.append(entry)
            elif not attention or waiting is not None:
                shown.append(entry)
        return {"routes": shown, "projects": proposals, "attention": bool(attention),
                "next": page["next"],
                "limits": "routing's rows and the ledger's state in this store only; a queued"
                          " write is not an issue anybody has written, and a confirmed one is"
                          " not an issue anybody read."}
