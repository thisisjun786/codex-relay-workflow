import threading
from codex_session_relay import identity
from codex_session_relay.ack import AckService
from codex_session_relay.delivery import DeliveryService, REVISION
from codex_session_relay.models import Endpoint
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.registry import Registry, project_key, record_settings
from codex_session_relay.store import Store
from codex_session_relay.sync import SyncOutbox
from codex_session_relay.errors import RelayError
from tests.support import HOST, task_settings
PROJECTS = {"a": ("/repo-a", "AAA-1", "linear://project-alpha"), "b": ("/repo-b", "BBB-1", "linear://project-beta")}
ALPHA_DOC = "https://linear.app/example/document/project-alpha-0000"
BETA_DOC = "https://linear.app/example/document/project-beta-00000"
sync = SyncOutbox(c.store, c.clock); c.ack.sync = sync
def assignment(name, events=1):
    cwd, iss, ref = PROJECTS[name]
    par, chi = f"01parent-{name}", f"01child-{name}"
    root = os.path.join(c.root, name); os.makedirs(root, exist_ok=True)
    rel = c.registry.register(parent=Endpoint(par, HOST, cwd=cwd), child=Endpoint(chi, HOST, cwd=root), issue_key=iss, artifact_roots=[root], allowed_recipients=[par, chi], dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=f"turn-{name}", scope_ref=ref)
    c.adapter.add_thread(par); c.adapter.add_thread(chi)
    record_settings(c.store, c.clock, par, task_settings(cwd), source="creation_result")
    ids = []
    for i in range(events):
        path = os.path.join(root, f"out-{i}.txt")
        open(path, "w").write(f"{name}-{i}")
        p = c.ready_payload(rel, [path], attempt=i + 1, turn=c.assigned_turn(thread=chi, turn=f"turn-{name}"))
        c.accept(p); c.delivery.enqueue(p["eventId"]); ids.append(p["eventId"]); c.clock.advance(1)
    return rel, ids
def deliver(e):
    return c.delivery.attempt(e, c.adapter, now=c.clock.now())
def acked(name, e):
    deliver(e); c.clock.advance(5)
    t = c.adapter.start_turn(f"01parent-{name}", turn_id=f"ack-{name}", status="inProgress")
    return t
def parallel(work):
    barrier = threading.Barrier(len(work)); results, errors = {}, {}
    def wrap(name, call):
        def run():
            store = Store(c.store.path)
            try:
                reg = Registry(store, c.clock); intake = ReceiptIntake(store, reg, c.clock)
                d = DeliveryService(store, reg, intake, c.clock); a = AckService(store, reg, intake, d, c.clock); a.sync = SyncOutbox(store, c.clock)
                barrier.wait(timeout=20); results[name] = call(a)
            except Exception as error:
                errors[name] = repr(error)
            finally:
                store.close()
        return threading.Thread(target=run)
    ts = [wrap(n, f) for n, f in work.items()]
    [t.start() for t in ts]; [t.join(timeout=60) for t in ts]
    return results, errors
m = sys.argv[3]
if m == "scope":
    a, _ = assignment("a"); b, _ = assignment("b")
    out["keys"] = [project_key(a), project_key(b)]
elif m == "acks":
    a, ai = assignment("a"); b, bi = assignment("b")
    turns = {"a": acked("a", ai[0]), "b": acked("b", bi[0])}
    res, err = parallel({n: (lambda n, e: lambda s: s.acknowledge(e, ack_turn_id=turns[n].turn_id, ack_proof=identity.ack_proof(e, turns[n].turn_id), accepted=True, adapter=c.adapter))(n, e) for n, e in (("a", ai[0]), ("b", bi[0]))})
    out["errors"] = err
elif m == "verdicts":
    from codex_session_relay import identity
    a, ai = assignment("a"); b, bi = assignment("b")
    for n, ids in (("a", ai), ("b", bi)):
        t = acked(n, ids[0])
        c.ack.acknowledge(ids[0], ack_turn_id=t.turn_id, ack_proof=identity.ack_proof(ids[0], t.turn_id), accepted=True, adapter=c.adapter)
    res, err = parallel({n: (lambda n, e: lambda s: s.record_verdict(e, verdict="needs_changes", verdict_turn_id=f"verdict-{n}", findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix it"}]))(n, e) for n, e in (("a", ai[0]), ("b", bi[0]))})
    out["errors"] = err
    out["next"] = {n: r["nextExecutionGeneration"] for n, r in res.items()}
elif m == "outbox":
    from codex_session_relay import identity
    jobs = {}
    for n, target in (("a", ALPHA_DOC), ("b", BETA_DOC)):
        rel, ids = assignment(n)
        t = acked(n, ids[0])
        c.ack.acknowledge(ids[0], ack_turn_id=t.turn_id, ack_proof=identity.ack_proof(ids[0], t.turn_id), accepted=True, adapter=c.adapter)
        sync.set_target(rel["relationshipId"], "coordination_document", target)
        c.ack.record_verdict(ids[0], verdict="verified", verdict_turn_id=f"verdict-{n}")
        jobs[n] = sync.snapshot(relationship_id=rel["relationshipId"])["jobs"][0]["syncId"]
    out["jobs"] = jobs
    alpha = sync.claim(jobs["a"], owner="worker-1", now=c.clock.now())
    sync.claim(jobs["b"], owner="worker-1", now=c.clock.now())
    out["complete"] = refusal(sync.complete, jobs["b"], claim_token=alpha["claimToken"], target_ref=BETA_DOC, readback="", now=c.clock.now())
