"""Product routing: the registry and bindings routing reads, and where incidents go.

The coordination parent (the credential holder) keeps two snapshots current here: the registry,
one record per product, and the bindings, the projects and issues it read back from Linear. Every
routing decision is made from those snapshots and from this store's own records, never from what
an incident says about itself - the incident's claim that a run belongs to an issue, for
instance, is checked against the relationship the relay registered for that run.

Writes to the ledger go through ledger_port and nothing else. See docs/product-routing.md.
"""

import json

from . import products
from .errors import RefusalReason
from .ledger_port import LedgerPort


class ProductRouter:
    def __init__(self, store, clock, port=None):
        self.store = store
        self.clock = clock
        self.port = port if port is not None else LedgerPort(store, clock)

    # ------------------------------------------------------------------ registry

    def register_product(self, record) -> dict:
        """Record what routing knows about one product. Replaces the previous record whole."""
        registry = products.read_registry(record)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO product_registry (product_key, record, recorded_at) VALUES (?,?,?)"
                " ON CONFLICT(product_key) DO UPDATE SET record = excluded.record,"
                "   recorded_at = excluded.recorded_at",
                (registry["product"], products.canonical(registry), now))
        return registry

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
        """
        product = record.get("product") if isinstance(record, dict) else None
        registry = self.registry(product) if isinstance(product, str) else None
        if registry is None:
            products.refuse(RefusalReason.ROUTE_PRODUCT_UNKNOWN,
                            f"{product!r} is not a registered product; register it first")
        binding = products.read_binding(record, registry)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO product_bindings (product_key, kind, ref, record, observed_at,"
                "  recorded_at) VALUES (?,?,?,?,?,?)"
                " ON CONFLICT(product_key, kind, ref) DO UPDATE SET record = excluded.record,"
                "   observed_at = excluded.observed_at, recorded_at = excluded.recorded_at",
                (binding["product"], binding["kind"], binding["ref"],
                 products.canonical(binding), binding["observedAt"], now))
        return binding

    def bindings(self, product) -> list:
        rows = self.store.all(
            "SELECT record FROM product_bindings WHERE product_key = ? ORDER BY kind, ref",
            (product,))
        return [json.loads(row["record"]) for row in rows]

    def set_policy(self, record) -> dict:
        """The explicit project creation policy. Without one routing never creates a project."""
        policy = products.read_policy(record)
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO routing_policy (policy_key, record, basis, recorded_at)"
                " VALUES (?,?,?,?) ON CONFLICT(policy_key) DO UPDATE SET"
                "   record = excluded.record, basis = excluded.basis,"
                "   recorded_at = excluded.recorded_at",
                (policy["policy"], products.canonical(policy), policy["basis"], now))
        return policy

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
