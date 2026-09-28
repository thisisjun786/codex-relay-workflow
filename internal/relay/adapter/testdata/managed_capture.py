import json
import sys
from pathlib import Path
from unittest.mock import patch
from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.clock import FakeClock
from codex_session_relay.managed import ManagedStart
from codex_session_relay.store import Store
from codex_session_relay import rolepolicy
spec=json.load(sys.stdin)
clock=FakeClock()
store=Store(Path(spec['state'])/'relay.sqlite3',socket_path=spec['socket'])
policy=rolepolicy.declared()
adapter=BridgeHostAdapter(spec['socket'],ledger_directory=spec['state'])
try:
    with patch('codex_thread_bridge.ledger.time.time',return_value=1700000000.0):
        result=ManagedStart(store,clock,adapter,lambda:{'observed':True,'policy':policy.summary()},socket=spec['socket'],marker_root=spec['marker'],state_selector=spec['state']).run(spec['request'])
    print(json.dumps(result,indent=2))
finally:
    adapter.close()
    store.close()
