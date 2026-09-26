from codex_session_relay.sync import SyncOutbox
from codex_session_relay.store import Store
from codex_session_relay.registry import Registry
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.delivery import DeliveryService
from codex_session_relay.reconcile import Reconciler
rel, e1 = c.queued_event()
out["first"] = c.attempt(e1)
p = c.ready_payload(rel, [c.artifact("second.txt", "still in flight")], attempt=2)
c.accept(p)
e2 = p["eventId"]
c.delivery.enqueue(e2)
c.adapter.script("transport_unknown")
later = c.clock.now() + 3600
out["second"] = c.attempt(e2, now=later)
SyncOutbox(c.store, c.clock).set_target(rel["relationshipId"], "coordination_document", "DOC-1")
path = c.store.path
c.store.close()
c.store = Store(path)
c.registry = Registry(c.store, c.clock)
c.intake = ReceiptIntake(c.store, c.registry, c.clock)
c.delivery = DeliveryService(c.store, c.registry, c.intake, c.clock)
c.reconciler = Reconciler(c.store, c.registry, c.delivery, c.clock)
out["recovered"] = c.reconciler.recover_on_start(c.adapter)
out["again"] = c.attempt(e1, now=later + 3600)
