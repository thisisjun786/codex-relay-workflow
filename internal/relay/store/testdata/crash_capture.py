"""Live Python transaction crash oracle: readiness is the before-COMMIT hook."""
import json
import selectors
import subprocess
import sys
from pathlib import Path
from codex_session_relay.store import Store

path = Path(sys.argv[1])
if len(sys.argv) > 2:
    store = Store(path)
    def written():
        print('written', flush=True)
        sys.stdin.read()
    store.fault_hook = written
    with store.transaction():
        store.journal('crash', 's', 'd', at='t')
    raise AssertionError('commit reached')
store = Store(path)
child = subprocess.Popen([sys.executable, __file__, str(path), 'child'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
try:
    with selectors.DefaultSelector() as selector:
        selector.register(child.stdout, selectors.EVENT_READ)
        if not selector.select(5): raise AssertionError('fault hook not reached')
    if child.stdout.readline().strip() != 'written': raise AssertionError('unexpected child response')
    child.kill()
    child.wait(timeout=5)
    print(json.dumps({'journal': [dict(row) for row in store.all("SELECT * FROM journal WHERE kind='crash'")]}))
finally:
    if child.poll() is None:
        child.kill()
        child.wait(timeout=5)
    child.stdin.close()
    child.stdout.close()
    store.close()
