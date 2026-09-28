import json
import sys
from pathlib import Path
from codex_session_relay.service import RelayService,ScopeRegistry
from codex_session_relay.store import resolve_state_dir
spec=json.load(sys.stdin)
selection=resolve_state_dir(spec['state'],socket_path=spec['socket'])
service=RelayService(selection,socket_path=spec['socket'],scope=ScopeRegistry(Path(spec['scope']),'isolated'),store_id=spec['storeId'])
service.installation_id=spec['installationId']
result=service.read_worker_policy()
print(json.dumps({'policy':result.get('policy'),'reason':result.get('reason')},sort_keys=True))
