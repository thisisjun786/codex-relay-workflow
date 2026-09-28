import asyncio
import json
import sys
import threading
from pathlib import Path
from unittest.mock import patch
from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger

spec=json.load(sys.stdin)
entered=threading.Event()
release=threading.Event()
loop_ready={}
class RPC:
    async def call(self,method,params):
        if method=='thread/read' and params['threadId']=='thread-a':
            loop_ready['loop']=asyncio.get_running_loop()
            loop_ready['gate']=asyncio.Event()
            entered.set()
            await loop_ready['gate'].wait()
        if method=='thread/read': return {'thread':{'status':{'type':'idle'}}}
        if method=='thread/resume': return spec['resume']
        if method=='turn/start': return {'turn':{'id':'turn-'+params['threadId']}}
        raise AssertionError(method)
    async def close(self): pass
root=Path(spec['root'])
adapter=BridgeHostAdapter(str(root/'socket'),timeout=10,drain_seconds=0,execution_policy=None,app_server_factory=lambda _:RPC(),ledger_factory=lambda:(root/'socket',Ledger(root/'python.sqlite3')))
settings=TaskSettings(spec['settings'])
result={}
def send():
    try: result['a']=adapter.send_message('req-a','thread-a','hello',settings)
    except Exception as error: result['a']={'error':str(error)}
with patch('codex_thread_bridge.ledger.time.time',return_value=1700000000.125):
    caller=threading.Thread(target=send)
    caller.start()
    if not entered.wait(5): raise AssertionError('RPC not reached')
    if spec['case']=='replay':
        result['replayed']=adapter.send_message('req-a','thread-a','hello',settings)
        try: adapter.send_message('req-a','thread-a','DIFFERENT',settings)
        except Exception as error: result['conflict']=str(error)
    elif spec['case']=='busy':
        result['busy']=adapter.send_message('req-busy','thread-a','second',settings)
    else:
        result['b']=adapter.send_message('req-b','thread-b','hello',settings)
    if spec['case']=='shutdown':
        adapter.close()
    else:
        loop_ready['loop'].call_soon_threadsafe(loop_ready['gate'].set)
    caller.join(5)
    if caller.is_alive(): raise AssertionError('caller not settled')
    adapter.close()
print(json.dumps(result,sort_keys=True))
