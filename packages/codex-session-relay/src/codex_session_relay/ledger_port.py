"""The one door from product routing into the fault ledger.

Routing decides where an incident belongs; the ledger owns everything that follows - identity,
suppression, targets and project readback, adoption, moves, updates, limits, publication kinds
and the outbox. Every ledger call routing makes goes through a method here, and nothing else in
routing imports the ledger. That keeps the boundary the coordination parent drew on 2026-09-23
in one place: CRW-206 builds no second copy of anything the ledger owns.

The ledger capabilities routing needs arrive with CRW-205's corrected contract (see
docs/product-routing.md). Until that contract is bound here, every method refuses with
route_ledger_pending. There is deliberately no stand-in: a routing path that seemed to work
against a local imitation of adoption or targets would prove nothing about the real ledger, and
a store written through one would carry identities the real ledger never produces.
"""

from .errors import RefusalReason
from .products import RouteRefused

# The ledger surface routing uses, one name per capability, in the order docs/product-routing.md
# lists them. Kept as data so a test can hold the adapter to exactly this list.
CAPABILITIES = (
    "fault_id", "observation", "register_class", "record", "get", "remediations", "list",
    "link", "ensure_target", "adopt", "move", "update", "consume", "budget", "notify",
    "register_kind", "queue", "created_ref", "cancel", "operation", "complete", "claim", "fail",
    "reconcile", "record_fix", "record_reverification", "resolve",
)


class LedgerPort:
    """Routing's view of the fault ledger. Unbound on this checkout."""

    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    def __getattr__(self, name):
        if name in CAPABILITIES:
            def pending(*_args, **_kwargs):
                raise RouteRefused(
                    RefusalReason.ROUTE_LEDGER_PENDING,
                    f"{name} needs CRW-205's corrected ledger contract, which this checkout"
                    f" does not carry yet",
                )
            return pending
        raise AttributeError(name)
