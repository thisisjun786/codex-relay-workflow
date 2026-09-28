"""Real guard reservations across adapter timeout and post-verdict bookkeeping.

The timeout edge is signalled by the child AFTER guard.evaluate has committed its
hold and observation. Only communicate's timeout notification is injected; the
adapter really kills that child. No sleeps or scheduling races select the edge.
"""
import contextlib
import io
import importlib
import json
import os
from pathlib import Path
import selectors
import signal
import sqlite3
import subprocess
import sys
import tempfile
from types import SimpleNamespace
from typing import Any
from unittest import mock

from codex_session_relay import guard, marker, stopadapter
from codex_session_relay.store import Store

NOW = '2026-01-01T00:00:00Z'


def setup(home: Path) -> Path:
    home.mkdir(parents=True, exist_ok=True)
    work, root = home / 'work', home / 'markers'
    work.mkdir(exist_ok=True)
    assignment = marker.assignment_id('dispatch')
    directory = marker.assignment_dir(root, work, assignment)
    for relative, value in {
        'intent.json': {'dispatchRequestIdHash': assignment},
        'bound.json': {'sessionId': 's'},
        'relationship.json': {'relationshipId': 'r'},
        'claims/s/claim.json': {'sessionId': 's', 'dispatchRequestId': 'dispatch'},
        'dispositions/s/t.json': {'sessionId': 's', 'turnId': 't', 'outcome': 'ready_for_review'},
    }.items():
        marker.publish(directory / relative, value, root=root)
    db = home / 'state/relay.sqlite3'
    with contextlib.closing(Store(str(db))):
        pass
    config = dict(configVersion=1, mode='hold', isolationAssertedBy='fixture',
                  relayExecutable=str(home / 'relay'), markerRoot=str(root), dbPath=str(db),
                  timeoutSeconds=5, journalRoot=str(home / 'journal'))
    (home / 'crw-completion-hook.json').write_text(json.dumps(config))
    # The real subprocess evaluates the real marker and store. It pauses only in
    # the timeout case, after publishing both reservation and observation.
    relay = home / 'relay'
    relay.write_text('#!' + sys.executable + '\nimport runpy,sys\nsys.argv = ['
                     + repr(str(Path(__file__).resolve())) + ', "child", ' + repr(str(home))
                     + ']\nrunpy.run_path(sys.argv[0], run_name="__main__")\n')
    relay.chmod(0o700)
    (home / 'stop.json').write_text(json.dumps(dict(cwd=str(work), session_id='s', turn_id='t', stop_hook_active=False)))
    return directory


def db_snapshot(home: Path) -> list[str]:
    with contextlib.closing(sqlite3.connect(home / 'state/relay.sqlite3')) as db:
        return list(db.iterdump())


def snapshot(home: Path) -> dict[str, Any]:
    def normalize(value: Any) -> Any:
        if isinstance(value, dict):
            return {k: normalize(v) for k, v in value.items()
                    if k not in ('at', 'elapsedMs', 'guardElapsedMs', 'identityScanMs')}
        if isinstance(value, list):
            return [normalize(v) for v in value]
        if isinstance(value, str):
            return value.replace(str(home), '<HOME>')
        return value
    rows = [normalize(json.loads(p.read_text())) for p in (home / 'journal').glob('*/*.json')]
    rows.sort(key=lambda r: r['guardState'] or '')
    records = {}
    for p in (home / 'markers').glob('*/*/hook/s/t/*.json'):
        records[p.name] = dict(value=normalize(json.loads(p.read_text())), mode=p.stat().st_mode & 0o777)
    return dict(rows=rows, records=records)


def compare(native: Path, edge: str, module: str) -> None:
    sys.path.insert(0, sys.argv[5] + '/scripts')
    completion = importlib.import_module('crw_runtime.completion')
    completion_hook = importlib.import_module('completion_hook')
    adapter = stopadapter if module == 'console' else completion
    with tempfile.TemporaryDirectory(prefix='late-python-') as temp:
        home = Path(temp)
        setup(home)
        before = db_snapshot(home)
        payload = (home / 'stop.json').read_bytes()
        clock = [0.0]
        real_journal = adapter.journal
        real_popen = subprocess.Popen
        children = []
        def journal(config, record, slot=None):
            result = real_journal(config, record, slot)
            if edge == 'bookkeeping':
                clock[0] = 6.0  # verdict accepted; journal finished after budget
            return result
        def popen(argv: list[str], **kwargs: Any) -> subprocess.Popen[bytes]:
            opened = real_popen[bytes](argv, **kwargs)
            children.append(opened)
            def timed_communicate(input: bytes | None = None, timeout: float | None = None) -> tuple[bytes, bytes]:
                assert opened.stdin is not None and opened.stdout is not None
                assert input is not None and timeout is not None
                opened.stdin.write(input)
                opened.stdin.close()
                with selectors.DefaultSelector() as ready:
                    ready.register(opened.stdout, selectors.EVENT_READ)
                    assert ready.select(10), 'guard did not publish its reservation'
                assert opened.stdout.readline() == b'reserved\n'
                clock[0] = 6.0
                raise subprocess.TimeoutExpired(argv, timeout)
            if edge == 'guard_timeout' and len(children) == 1:
                opened.communicate = timed_communicate
            return opened
        answers = []
        with mock.patch.dict(os.environ, HOME=str(home), CODEX_HOME=str(home),
                             CODEX_SESSION_RELAY_STATE=str(home / 'state'), LATE_EDGE=edge), \
                mock.patch.object(adapter, 'time', SimpleNamespace(monotonic=lambda: clock[0])), \
                mock.patch.object(adapter, 'journal', side_effect=journal), \
                mock.patch.object(adapter.subprocess, 'Popen', side_effect=popen):
            for attempt in range(2):
                if attempt:
                    os.environ['LATE_EDGE'] = 'normal'
                stdout = io.StringIO()
                with mock.patch.object(sys, 'argv', ['hook', str(home / 'crw-completion-hook.json')]), \
                        mock.patch.object(sys, 'stdin', SimpleNamespace(buffer=io.BytesIO(payload))), \
                        contextlib.redirect_stdout(stdout):
                    (stopadapter.main if module == 'console' else completion_hook.main)()
                answers.append(stdout.getvalue())
        for child in children:
            child.wait(timeout=10)
        assert db_snapshot(home) == before, 'guard changed the relay store'
        go = json.loads((native / 'result.json').read_text())
        assert answers == go['stdout'], (edge, module, answers, go)
        expected, actual = snapshot(home), snapshot(native)
        if edge == 'guard_timeout':
            # Native cancellation diagnostic (decision 24), not a Python process group.
            expected['rows'][0].pop('detail')
            actual['rows'][0].pop('detail')
        assert expected == actual, (edge, module, expected, actual)
        assert 'hold.json' in expected['records'], expected
        assert expected['records']['1.json']['value']['decisionState'] == 'hold_in_flight', expected
        assert answers[1] == ''
        print(json.dumps(dict(edge=edge, module=module, stdout=answers, equal=True, snapshot=expected)))


if __name__ == '__main__':
    action, home = sys.argv[1], Path(sys.argv[2])
    if action == 'setup':
        print(setup(home))
    elif action == 'child':
        config = json.loads((home / 'crw-completion-hook.json').read_text())
        verdict = guard.evaluate(config['markerRoot'], json.load(sys.stdin), now=NOW,
                                 mode='hold', db_path=config['dbPath'])
        if os.environ['LATE_EDGE'] == 'guard_timeout':
            print('reserved', flush=True)
            signal.pause()
        else:
            print(json.dumps(verdict))
    else:
        compare(home, sys.argv[3], sys.argv[4])
