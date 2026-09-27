"""Capture complete Python fault ledger tables after one F2 operation."""
import json
import pathlib
import sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.faults import FaultLedger, fault_id
from codex_session_relay.store import Store

state, action = sys.argv[1:]
path = pathlib.Path(state) / 'relay.sqlite3'
path.parent.mkdir(parents=True, exist_ok=True)
clock = FakeClock(start=100000)
db = Store(str(path))
try:
    ledger = FaultLedger(db, clock)
    observation = {'schema':'fault-observation/1', 'product':'crw', 'faultClass':'report_omitted', 'severity':'broken', 'signature':{'turn':'f2'}, 'occurrenceKey':'first', 'scope':{'projectKey':'old'}}
    ledger.record(observation)
    ident = fault_id('crw', 'report_omitted', {'turn':'f2'})
    publication = db.one("SELECT publication_id FROM fault_publications WHERE fault_id=? AND kind='open_record'", (ident,))['publication_id']
    if action in ('fail','fail_issued','fail_ended'):
        issued = action != 'fail'
        with db.transaction() as conn:
            conn.execute("UPDATE fault_publications SET state=?,claim_token='token',attempts=1,issued_at=? WHERE publication_id=?", ('issued' if issued else 'claimed',clock.iso() if issued else None,publication))
            conn.execute("INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts,issued_at) VALUES(?,1,'holder',0,?,?,?)", (publication, clock.iso(),clock.now(),clock.iso() if issued else None))
        reply = ledger.fail(publication, claim_token='token', error='connector refused',ended=action=='fail_ended')
    elif action == 'move':
        reply = ledger.move(ident, scope={'projectKey':'new'})
    elif action == 'adopt':
        reply = ledger.adopt(ident, external_ref='CRW-1', scope={'projectKey':'new'})
    elif action == 'update':
        reply = ledger.request_update(ident, op='add_label', value='urgent')
    elif action in ('update_owned', 'update_project'):
        ledger.adopt(ident, external_ref='CRW-1', scope={'projectKey':'old'})
        if action == 'update_project':
            ledger.set_target(product='crw',project='old',team='team',project_ref='project-1')
            reply = ledger.request_update(ident, op='set_project', value='project-1')
        else:
            reply = ledger.request_update(ident, op='add_label', value='urgent')
    rows = {}
    for (table,) in db.db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault_%' ORDER BY name"):
        found = [dict(row) for row in db.db.execute(f'SELECT * FROM {table} ORDER BY rowid')]
        if found: rows[table] = found
    print(json.dumps({'reply':reply,'tables':rows},sort_keys=True))
finally:
    db.close()
