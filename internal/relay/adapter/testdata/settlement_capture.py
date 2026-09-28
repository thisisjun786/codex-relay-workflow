import json
import shutil
import sqlite3
import sys
from pathlib import Path
from codex_session_relay.clock import FakeClock
from codex_session_relay.delivery import DeliveryService
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay.bridge_adapter import _ShutdownCancelled

root=Path(sys.argv[1]);mode=sys.argv[2]
sys.path.insert(0,str(Path(__file__).resolve().parents[4]/'packages/codex-session-relay'))
from tests.support import DeliveryTestCase
clock=FakeClock()
def tables(store):
    names=[r[0] for r in store.db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
    return {name:[list(row) for row in store.db.execute('SELECT * FROM "'+name+'" ORDER BY rowid')] for name in names}
if mode=='seed':
    case=DeliveryTestCase()
    case.tmp=str(root);case.root=str(root/'work');Path(case.root).mkdir()
    case.clock=clock;case.store=Store(root/'python.sqlite3');case.registry=Registry(case.store,clock);case.intake=ReceiptIntake(case.store,case.registry,clock)
    case.delivery=DeliveryService(case.store,case.registry,case.intake,clock)
    _,event=case.queued_event()
    target=sqlite3.connect(root/'go.sqlite3');case.store.db.backup(target);target.close()
    case.store.close()
    print(json.dumps({'event':event}))
else:
    store=Store(root/'python.sqlite3');registry=Registry(store,clock);intake=ReceiptIntake(store,registry,clock)
    class Cancelled(FakeHostAdapter):
        def send_message(self,*args,**kwargs):
            raise _ShutdownCancelled('the relay transport was shut down while this send was in flight; outcome unknown, do not resend under a new request id')
    host=Cancelled(clock);host.add_thread('01parent-task');host.add_thread('01child-task')
    result=DeliveryService(store,registry,intake,clock).attempt(sys.argv[3],host,now=clock.now())
    print(json.dumps({'result':result,'tables':tables(store)},sort_keys=True))
    store.close()
