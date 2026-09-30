import json
import sys
from pathlib import Path
from contextlib import closing
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RelayError
from codex_session_relay.models import TurnRef
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
spec=json.load(sys.stdin)
with closing(Store(Path(spec['store']))) as store:
    clock=FakeClock()
    try:
        receipt=ReceiptIntake(store,Registry(store,clock),clock).accept_child_receipt(spec['payload'],observation=TurnRef('01child-task','turn-dispatch-1','completed'))
        result={'accepted':True,'event':spec['payload']['eventId']}
    except Exception as error:
        result={'accepted':False,'reason':getattr(getattr(error,'reason',None),'value',None)}
        if not isinstance(error,RelayError):
            # What the CLI's host envelope carries for an exception that is not a refusal.
            result['host']=f"{type(error).__name__}: {error}"
    result['rows']=[list(row) for row in store.all('SELECT event_id,path_binding_mode FROM events ORDER BY event_id')]
    result['refusals']=[row['reason'] for row in store.all('SELECT reason FROM refusals ORDER BY id')]
    print(json.dumps(result,sort_keys=True))
