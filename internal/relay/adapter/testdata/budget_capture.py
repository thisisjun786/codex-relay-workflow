import asyncio
import json
import sys
from pathlib import Path
from unittest.mock import patch
from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger
spec=json.load(sys.stdin)
root=Path(spec['root'])
calls=[]
class RPC:
    async def call(self,method,params):
        calls.append(method)
        return {'thread/read':{'thread':{'status':{'type':'idle'}}},'thread/resume':spec['resume'],'thread/list':{'data':[{'id':'thread-1'}],'nextCursor':None},'turn/start':{'turn':{'id':'turn-guarded'}}}[method]
    async def close(self):pass
adapter=BridgeHostAdapter(str(root/'socket'),timeout=.2,caller_slack=.2,execution_policy=None,app_server_factory=lambda _:RPC(),ledger_factory=lambda:(root/'socket',Ledger(root/'python.sqlite3')))
seen={}
real_wait=asyncio.wait_for
real_result=adapter._transport._futures.Future.result
async def wait(awaitable,timeout):
    seen['execution']=timeout
    return await real_wait(awaitable,timeout)
def result(future,budget):
    seen['caller']=budget
    return real_result(future,budget)
async def guard(rpc):
    for _ in range(spec['guard']):await rpc.call('thread/list',{'limit':1})
with patch('codex_thread_bridge.ledger.time.time',return_value=1700000000.125),patch('asyncio.wait_for',wait),patch.object(adapter._transport._futures.Future,'result',result):
    receipt=adapter.send_message('send-guarded-budget','thread-1','hello',TaskSettings(spec['settings']),before_start=guard,guard_rpc_requests=spec['guard'])
seen['receipt']=receipt
seen['calls']=calls
invalid=[]
for value in (-1,11,True,1.5,None,'10'):
    try:adapter.send_message('send-bad-budget','thread-1','hello',TaskSettings(spec['settings']),guard_rpc_requests=value)
    except Exception as error:invalid.append(str(error))
seen['invalid']=invalid
adapter.close()
print(json.dumps(seen,sort_keys=True))
