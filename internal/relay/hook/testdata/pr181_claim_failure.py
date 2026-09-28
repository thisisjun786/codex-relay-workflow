"""Python's claim ownership is O_EXCL creation, even when the subsequent write fails."""
import json
import os
from pathlib import Path
import sys
import tempfile
from unittest import mock

from codex_session_relay import stopadapter

root, native, role, number = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3], int(sys.argv[4])
payload = json.load(sys.stdin)
verdict = {'decision': 'block', 'state': 'receipt_missing', 'hook_output': {'decision': 'block', 'reason': 'verify', 'continue': True}}


def snapshot(home):
    result = {}
    for directory in [home / 'journal', home / 'crw-completion-hook']:
        for path in sorted(directory.rglob('*.json')):
            raw = path.read_bytes()
            try:
                value = json.loads(raw)
            except ValueError:
                value = {'torn': raw.hex()}
            def normalize(v):
                if isinstance(v, dict):
                    return {k: normalize(x) for k, x in v.items() if k not in ('pid', 'at', 'claimedAt', 'elapsedMs', 'guardElapsedMs', 'identityScanMs')}
                if isinstance(v, str):
                    v = v.replace(str(home), '<HOME>').replace(str(native), '<HOME>')
                    import re
                    return re.sub(r'[0-9]{8}/[0-9a-f]{32}\.json', '<ROW>', v)
                return v
            name = str(path.relative_to(home))
            if path.parent.name.isdigit():
                name = 'journal/' + json.loads(raw)['acceptance']
            result[name] = {'mode': path.stat().st_mode & 0o777, 'value': normalize(value)}
    return result

with tempfile.TemporaryDirectory(prefix='claim-py-') as temp:
    home = Path(temp)
    config = json.loads((native / 'crw-completion-hook.json').read_text())
    config['journalRoot'] = str(home / 'journal')
    settings = home / 'crw-completion-hook.json'; settings.write_text(json.dumps(config))
    original = stopadapter._write_whole
    def fail_claim(fd, document):
        if document.get('claimedBy') is not None:
            host = 'journalRoot' in document['claimedBy']
            if host == (role == 'host'):
                os.write(fd, b'{')
                raise OSError(number, os.strerror(number))
        return original(fd, document)
    ending = {'ending': 'exited', 'code': 0, 'signal': None, 'stdout': json.dumps(verdict), 'stderr': '', 'elapsedMs': 0, 'detail': None}
    with mock.patch.object(stopadapter, '_write_whole', side_effect=fail_claim), mock.patch.object(stopadapter, 'invoke_guard', return_value=ending) as invoke:
        first = stopadapter.run(json.dumps(payload).encode(), codex_home=home, settings=settings)
        second = stopadapter.run(json.dumps(payload).encode(), codex_home=home, settings=settings)
    assert first == json.dumps(verdict['hook_output']) and second is None and invoke.call_count == 1
    python, go = snapshot(home), snapshot(native)
    assert python == go, (python, go)
    print(json.dumps({'role': role, 'errno': number, 'first_stdout': first, 'second_stdout': '', 'exit': 0, 'equal': True, 'files': python}))
