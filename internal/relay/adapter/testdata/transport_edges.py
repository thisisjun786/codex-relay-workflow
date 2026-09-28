import asyncio
import dataclasses
import json
import sys
import threading
from pathlib import Path
from unittest.mock import patch
from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger

spec = json.load(sys.stdin)
root = Path(spec['root'])
entered, closing, settled = threading.Event(), threading.Event(), threading.Event()
state = {}

class ObservedLedger(Ledger):
    def save(self, receipt):
        result = super().save(receipt)
        if receipt.get('requestId') == 'req-a' and receipt.get('status') == 'accepted':
            settled.set()
        return result

class RPC:
    async def call(self, method, params):
        if method == 'thread/read' and params['threadId'] == 'thread-a':
            state['loop'] = asyncio.get_running_loop()
            state['release'] = asyncio.Event()
            entered.set()
            await state['release'].wait()
        if method == 'thread/read': return {'thread': {'status': {'type': 'idle'}}}
        if method == 'thread/resume': return spec['resume']
        if method == 'turn/start': return {'turn': {'id': 'turn-' + params['threadId']}}
        raise AssertionError(method)
    async def close(self):
        if spec['case'] == 'racing-close':
            state['loop'] = asyncio.get_running_loop()
            state['finish'] = asyncio.Event()
            closing.set()
            await state['finish'].wait()

adapter = BridgeHostAdapter(str(root/'socket'), timeout=10, caller_slack=-9.9 if spec['case']=='abandoned' else 10, drain_seconds=0, execution_policy=None, app_server_factory=lambda _: RPC(), ledger_factory=lambda: (root/'socket', ObservedLedger(root/'python.sqlite3')))
settings = TaskSettings(spec['settings'])
result = {}
def send(request, thread):
    try: return adapter.send_message(request, thread, 'hello', settings)
    except Exception as error: return {'error': str(error)}
def wait(event):
    if not event.wait(5): raise AssertionError('transport state signal not received')

with patch('codex_thread_bridge.ledger.time.time', return_value=1700000000.125):
    try:
        if spec['case'] == 'abandoned':
            done = threading.Event()
            def caller():
                result['abandoned'] = send('req-a', 'thread-a')
                done.set()
            thread = threading.Thread(target=caller)
            thread.start()
            wait(entered)
            wait(done)
            thread.join()
            adapter._transport._caller_slack = 10
            result['other'] = send('req-b', 'thread-b')
            result['read'] = dataclasses.asdict(adapter.read_thread('thread-b'))
            state['loop'].call_soon_threadsafe(state['release'].set)
            wait(settled)
            result['settled'] = adapter.get_operation('req-a')
        elif spec['case'] == 'racing-close':
            thread = threading.Thread(target=adapter.close)
            thread.start()
            wait(closing)
            result['submission'] = send('never-sent', 'thread-b')
            state['loop'].call_soon_threadsafe(state['finish'].set)
            thread.join(5)
            if thread.is_alive(): raise AssertionError('close did not settle')
        elif spec['case'] == 'stopping':
            adapter._transport._stopping = True
            result['serving'] = send('still-serving', 'thread-b')
    finally:
        adapter.close()
print(json.dumps(result, sort_keys=True))
