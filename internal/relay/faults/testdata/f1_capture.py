"""Capture whole Python fault_* state for F1 transitions in an isolated store."""
import json
import pathlib
import sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.faults import FaultLedger, FaultRefused, fault_id
from codex_session_relay.store import Store

def refusal(error: FaultRefused) -> dict[str, str]:
    assert error.reason is not None
    return {'error': type(error).__name__, 'reason': error.reason.value, 'detail': str(error)}


state, action = sys.argv[1:]
path = pathlib.Path(state) / 'relay.sqlite3'
path.parent.mkdir(parents=True, exist_ok=True)
clock = FakeClock(start=100000)
db = Store(str(path))
try:
    ledger = FaultLedger(db, clock)
    ledger.set_target(product='crw', project='P', team='team', project_ref='project-P')
    ledger.record({'schema':'fault-observation/1','product':'crw','faultClass':'report_omitted','severity':'broken','signature':{'turn':'f1'},'occurrenceKey':'first','scope':{'projectKey':'P'}})
    ident = fault_id('crw','report_omitted',{'turn':'f1'})
    pub = db.one("SELECT publication_id FROM fault_publications WHERE fault_id=?",(ident,))['publication_id']
    if action in ('sweep','sweep_readings'):
        from codex_session_relay import faultsweep
        readings = ([{'schema':'reporting-observation/1','relationshipId':'rel-1','selectors':{'turn':'turn-1'},'reportingState':'unreported'}] if action == 'sweep_readings' else [])
        faultsweep._now = lambda store: clock.iso()
        batch = faultsweep.sweep(db,product='crw',readings=readings)
        reply = faultsweep.record_all(ledger,batch,store=db)
    elif action in ('claim','claim_backoff'):
        import codex_session_relay.faults as faults
        original = faults.secrets.token_hex
        faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
        try:
            if action == 'claim_backoff':
                db.db.execute("UPDATE fault_publications SET next_attempt_at=? WHERE publication_id=?",(clock.now()+100,pub))
                try: reply = ledger.claim(pub,owner='operator')
                except FaultRefused as error: reply = refusal(error)
            else: reply = ledger.claim(pub,owner='operator')
        finally: faults.secrets.token_hex = original
    else:
        with db.transaction() as conn:
            conn.execute("UPDATE fault_publications SET state='claimed',claim_token='token',lease_owner='operator',lease_until=?,attempts=1 WHERE publication_id=?",(clock.now()+300,pub))
            conn.execute("INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts) VALUES(?,1,'operator',0,?,?)",(pub,clock.iso(),clock.now()))
        if action == 'operation': reply = ledger.operation(pub,claim_token='token')
        elif action == 'reconcile': reply = ledger.reconcile(pub,'',searched=True)
        elif action == 'complete_refusal':
            try: reply = ledger.complete(pub,claim_token='token',readback='not a block')
            except FaultRefused as error: reply = refusal(error)
        elif action == 'complete':
            block = ledger.operation(pub, claim_token='token')['block']
            reply = ledger.complete(pub,claim_token='token',readback=block,external_ref='REL-5',project_ref='project-P')
        elif action == 'reconcile_ended':
            conn = db.db
            conn.execute("UPDATE fault_publications SET state='uncertain',claim_token=NULL,lease_owner=NULL,lease_until=NULL WHERE publication_id=?",(pub,))
            reply = ledger.reconcile(pub,'',searched=True,prior_ended=True,reason='request ended')
        elif action == 'operation_retarget':
            ledger.set_target(product='crw',project='P',team='other',project_ref='project-other')
            try: reply = ledger.operation(pub,claim_token='token')
            except FaultRefused as error: reply = refusal(error)
        else:
            raise ValueError(f'unknown F1 capture action: {action}')
    tables = {}
    for (name,) in db.db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault_%' ORDER BY name"):
        rows = [dict(row) for row in db.db.execute(f'SELECT * FROM {name} ORDER BY rowid')]
        if rows: tables[name] = rows
    print(json.dumps({'reply':reply,'tables':tables},sort_keys=True))
finally:
    db.close()
