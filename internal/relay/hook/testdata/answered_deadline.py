"""An accepted Python verdict is still printed after the guard budget expires."""
import contextlib
import io
import json
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

from codex_session_relay import guard, stopadapter

home = Path(sys.argv[1])
native = sys.stdin.read()
config = json.loads((home / 'crw-completion-hook.json').read_text())
payload = (home / 'stop.json').read_bytes()
paths = list((home / 'markers').glob('*/*/hook/s/t/*.json'))
before = {str(p): p.read_bytes() for p in paths}
assert len(before) == 2 and any(p.name == 'hold.json' for p in paths)
for p in paths:
    p.unlink()
clock = [0.0]
verdicts = []
def invoke(config, payload):
    verdict = guard.evaluate(config['markerRoot'], json.loads(payload),
                             mode='hold', now='2026-01-01T00:00:00Z', db_path=config['dbPath'])
    verdicts.append(verdict)
    clock[0] = 6.0  # accepted answer, now beyond the configured five-second budget
    return dict(ending='exited', code=0, signal=None, stdout=json.dumps(verdict),
                stderr='', elapsedMs=6000, detail=None)
stdout = io.StringIO()
with mock.patch.object(stopadapter, 'time', SimpleNamespace(monotonic=lambda: clock[0])), \
        mock.patch.object(stopadapter, 'invoke_guard', side_effect=invoke), \
        mock.patch.object(sys, 'stdin', SimpleNamespace(buffer=io.BytesIO(payload))), \
        mock.patch.object(sys, 'argv', ['hook', str(home / 'crw-completion-hook.json')]), \
        contextlib.redirect_stdout(stdout):
    stopadapter.main()
assert verdicts[0]['decision'] == 'block'
assert stdout.getvalue() == native and native
assert before == {str(p): p.read_bytes() for p in paths}
print(json.dumps(dict(equal=True, stdout=native, hold_and_observation_bytes_equal=True)))
