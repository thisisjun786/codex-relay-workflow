"""D2: compare native guard-call fault prefix against Python BaseException path."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from unittest import mock

from codex_session_relay import stopadapter

root, native_home, native_path = map(Path, sys.argv[1:])
sys.path.insert(0, str(root / 'scripts'))
from crw_runtime import completion
payload = json.load(sys.stdin)
with tempfile.TemporaryDirectory(prefix='d2-python-') as temp:
    home = Path(temp)
    config = json.loads((native_home / 'crw-completion-hook.json').read_text())
    config['journalRoot'] = str(home / 'journal')
    settings = home / 'settings.json'; settings.write_text(json.dumps(config))
    with mock.patch.object(stopadapter, 'invoke_guard', side_effect=RuntimeError('injected guard fault')):
        assert stopadapter.run(json.dumps(payload).encode(), codex_home=home, settings=settings) is None
    [py_path] = list((home / 'journal').glob('[0-9]*/*.json'))
    py = json.loads(py_path.read_text())
    if str(native_path) == '-':
        # Python's row alone, aligned as below, recorded by the Go test (pyoracle).
        assert completion._row_shape(py)
        py['configuration'] = '<SETTINGS>'
        for field in ['at', 'elapsedMs', 'identityScanMs']:
            py.pop(field)
        sys.stdout.buffer.write(completion._record_bytes(py))
        sys.exit()
    go = json.loads(native_path.read_text())
    assert completion._row_shape(py) and completion._row_shape(go)
    # Align only generated path/time values; every remaining byte is compared.
    for row in [py, go]:
        row['configuration'] = '<SETTINGS>'
        for field in ['at', 'elapsedMs', 'identityScanMs']:
            row.pop(field)
    assert completion._record_bytes(py) == completion._record_bytes(go), (py, go)
    readings = []
    for journal in [home / 'journal', native_home / 'journal']:
        done = subprocess.run([sys.executable, str(root / 'scripts/stop_events.py'), '--journal-root', str(journal)], capture_output=True, timeout=10)
        answer = json.loads(done.stdout)
        assert done.returncode == 0 and answer['verdict'] == 'TRUE' and not answer['rowsUnreadable'], answer
        readings.append({'exit': done.returncode, 'verdict': answer['verdict']})
    print(json.dumps({'python_row': py, 'go_row': go, 'equal': True, 'reader': readings}))
