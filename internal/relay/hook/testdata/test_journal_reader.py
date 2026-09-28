"""Decision-22 native pre-scan journal contract, through the real hook and reader."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
from datetime import datetime, timedelta, timezone

import pytest

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / 'scripts'))
from crw_runtime import completion  # noqa: E402


@pytest.fixture(scope='session')
def crw(tmp_path_factory):
    supplied = os.environ.get('CRW_TEST_BINARY')
    if supplied:
        return supplied
    path = tmp_path_factory.mktemp('reader-binary') / 'crw'
    subprocess.run(['go', 'build', '-o', str(path), './cmd/crw'], cwd=ROOT, check=True,
                   capture_output=True, timeout=120)
    return str(path)


def produce(home, binary, native):
    home.mkdir()
    settings = dict(configVersion=1, mode='observe', relayExecutable=str(home / 'absent-relay'),
                    markerRoot=str(home / 'markers'), dbPath=str(home / 'state/relay.sqlite3'),
                    journalRoot=str(home / 'journal'), journalPolicy='every_invocation',
                    timeoutSeconds=5)
    (home / completion.CONFIG_NAME).write_text(json.dumps(settings))
    fixture = json.loads((ROOT / 'packages/codex-session-relay/tests/fixtures/stop_event_r1.json').read_text())
    first = fixture['stops'][0]
    transcript = home / 'transcript.jsonl'
    transcript.write_text('\n'.join(fixture['transcriptLines'][:first['linesAtStop']]) + '\n')
    payload = dict(first['payload'], cwd=str(home), transcript_path=str(transcript))
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'),
               CODEX_SESSION_RELAY_STATE=str(home / 'state'))
    command = [binary, 'hook'] if native else [sys.executable, str(ROOT / 'packages/codex-session-relay/src/codex_session_relay/stopadapter.py')]
    done = subprocess.run(command, input=json.dumps(payload).encode(), capture_output=True,
                          env=env, timeout=10)
    assert (done.returncode, done.stdout, done.stderr) == (0, b'', b'')
    [path] = list((home / 'journal').glob('[0-9]*/*.json'))
    return settings, path, json.loads(path.read_text())


def verify(root):
    done = subprocess.run([sys.executable, str(ROOT / 'scripts/stop_events.py'), '--journal-root', str(root)],
                          capture_output=True, timeout=10)
    assert not done.stderr
    return done.returncode, json.loads(done.stdout)


def before_reader():
    path = os.environ.get('CRW_READER_BASELINE')
    if not path:
        return None
    spec = importlib.util.spec_from_file_location('crw_runtime.before_decision22', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_native_unreachable_row_is_readable(crw, tmp_path):
    _, path, row = produce(tmp_path / 'native', crw, True)
    _, py_path, py_row = produce(tmp_path / 'python', crw, False)
    code, answer = verify(path.parents[1])
    py_code, py_answer = verify(py_path.parents[1])
    assert (code, answer['verdict']) == (py_code, py_answer['verdict']) == (0, 'TRUE')
    assert completion._row_shape(row)
    assert completion._row_shape(py_row)
    assert answer['rowsUnreadable'] == []
    assert answer['events'] == 0  # no event identity or acceptance was manufactured
    assert answer['unjudgedInvocations'] == {'no_event:guard_unreachable': 1}
    assert answer['supersededPerTurn']['pairs'] == 1
    baseline = before_reader()
    if baseline:
        assert not baseline._row_shape(row)
        before = baseline.stop_events([path.parents[1]])
        assert before['verdict'] == 'UNREADABLE' and before['rowsUnreadable'] == [str(path)]


NEIGHBOURS = [
    ('identityScanMs', 0), ('adapterOutcome', 'guard_timed_out'), ('errno', 'REMOVE'),
    ('errno', 'ETIMEDOUT'), ('errno', 'EPERM'), ('held', True), ('guardInvoked', False),
    ('processEnding', 'timed_out'), ('eventKey', 'a' * 64), ('eventIdentity', {}),
    ('acceptedAs', 'accepted/x.json'), ('guardStderr', 'unexpected'), ('exitCode', 0),
    ('signal', 9), ('guardDecision', 'release'), ('guardState', 'unmanaged'),
    ('assignmentId', 'a' * 64), ('guardRecordedAs', 'hook/s/t/0'), ('guardMode', None),
    ('sessionId', 'REMOVE'), ('stopHookActive', 'REMOVE'), ('recordVersion', True),
    ('elapsedMs', True), ('guardElapsedMs', -1), ('at', '2026-99-99T00:00:00Z'),
    ('configuration', 'relative'), ('detail', 'unreachable'), ('fault', 'extra'),
    ('counters', {}), ('observation', 'unmanaged'),
]


@pytest.mark.parametrize('field,value', NEIGHBOURS, ids=[item[0] + '-' + str(i) for i, item in enumerate(NEIGHBOURS)])
def test_neighbour_stays_rejected(crw, tmp_path, field, value):
    _, path, row = produce(tmp_path / 'native', crw, True)
    if value == 'REMOVE':
        row.pop(field)
    else:
        row[field] = value
    assert not completion._row_shape(row)
    baseline = before_reader()
    if baseline:
        assert not baseline._row_shape(row)
    path.write_bytes(completion._record_bytes(row))
    code, answer = verify(path.parents[1])
    assert (code, answer['verdict']) == (3, 'UNREADABLE')
    assert answer['rowsUnreadable'] == [str(path)]
    assert answer['unjudgedInvocations'] == {}


@pytest.mark.parametrize('name,number,phrase', [('ENOENT', 2, 'No such file or directory'),
    ('ECONNREFUSED', 111 if sys.platform.startswith('linux') else 61, 'Connection refused'),
    ('EACCES', 13, 'Permission denied')])
def test_exact_errno_detail_pair(crw, tmp_path, name, number, phrase):
    _, _, row = produce(tmp_path / 'native', crw, True)
    row['errno'] = name
    path = str(tmp_path / 'state/control.sock')
    row['detail'] = f'the configured runtime could not be run: [Errno {number}] {phrase}: {path!r}'
    assert completion._row_shape(row)
    row['detail'] = row['detail'].replace('control.sock', 'different.sock')
    assert not completion._row_shape(row)
    baseline = before_reader()
    if baseline:
        assert not baseline._row_shape(row)


def test_mixed_unjudged_window_stays_unreadable(crw, tmp_path):
    _, path, row = produce(tmp_path / 'native', crw, True)
    # A native exception cannot mask a readable but unjudged legacy invocation.
    old = dict(recordVersion=1, sessionId='s', turnId='t', at=row['at'])
    (path.parent / ('f' * 32 + '.json')).write_bytes(completion._record_bytes(old))
    code, answer = verify(path.parents[1])
    assert (code, answer['verdict']) == (3, 'UNREADABLE')
    assert answer['legacyRows'] == 1 and not answer['rowsUnreadable']


def test_row_count_and_retention_window_unchanged(crw, tmp_path):
    native_config, native_path, native = produce(tmp_path / 'native', crw, True)
    python_config, python_path, py_row = produce(tmp_path / 'python', crw, False)
    baseline = before_reader()
    for config, path, row in [(native_config, native_path, native), (python_config, python_path, py_row)]:
        before = path.read_bytes()
        assert completion._journal_cell(config)['value'] == '1'
        if baseline:
            assert baseline._journal_cell(config)['value'] == '1'
        verify(path.parents[1])
        assert path.read_bytes() == before  # reader neither deletes nor rewrites retention input
        stamp = datetime.strptime(row['at'], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
        # Cutover row 2 is an age predicate, independent of acceptance and verdict.
        for elapsed, live in [(0, True), (9, True), (10, False), (11, False)]:
            now = stamp + timedelta(seconds=elapsed)
            retained = [p for p in path.parents[1].glob('[0-9]*/*.json')
                        if now - datetime.strptime(json.loads(p.read_text())['at'], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
                        < timedelta(seconds=2 * config['timeoutSeconds'])]
            assert len(retained) == int(live)
