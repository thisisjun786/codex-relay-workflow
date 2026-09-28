"""Fixtures/CLI oracle for shared guard selection, routing, and microsecond time."""
import contextlib
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys

from codex_session_relay import guard, marker
from codex_session_relay.store import Store, resolve_state_dir

NOW = '2026-01-01T00:00:00.123456+00:00'


def prepare(home: Path, case: str):
    home.mkdir(exist_ok=True)
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'), CODEX_SESSION_RELAY_STATE='')
    wanted = home / 'app.sock'
    state = home / 'state'
    if case in ('wrong_socket','ambiguous','unidentified','override'):
        env['CODEX_SESSION_RELAY_STATE'] = str(state) if case in ('wrong_socket','override') else ''
        if case == 'wrong_socket':
            with contextlib.closing(Store(str(state / 'relay.sqlite3'), socket_path=str(home / 'other.sock'))) as db:
                db.db.execute("INSERT OR REPLACE INTO schema_meta VALUES('owner','go')")
            (state / 'takeover.json').write_text('{"owner":"go","phase":"active"}')
        else:
            for name in (['a','b'] if case in ('ambiguous','override') else ['a']):
                with contextlib.closing(Store(str(home / 'xdg/codex-session-relay' / name / 'relay.sqlite3'), socket_path=str(wanted) if case != 'unidentified' else None)):
                    pass
            if case == 'override':
                with contextlib.closing(Store(str(state / 'relay.sqlite3'), socket_path=str(wanted))):
                    pass
    if case in ('relative_xdg','legacy'):
        env['XDG_STATE_HOME'] = 'relative' if case == 'relative_xdg' else str(home / 'xdg')
        if case == 'legacy':
            real = home / 'real'; real.mkdir(); (home / 'link').symlink_to(real, target_is_directory=True)
            wanted = home / 'link/app.sock'
            scope = hashlib.sha256(str(wanted).encode()).hexdigest()[:16]
            with contextlib.closing(Store(str(home / 'xdg/codex-session-relay' / scope / 'relay.sqlite3'), socket_path=str(wanted))):
                pass
    with contextlib.chdir(home):
        before = dict(os.environ)
        os.environ.update(env)
        try:
            selection = resolve_state_dir(None, str(wanted))
        finally:
            os.environ.clear();os.environ.update(before)
    state = selection.path
    root, work = home / 'markers', home / 'work';work.mkdir()
    directory = marker.assignment_dir(root, work, marker.assignment_id('dispatch'))
    for name, record in {
        'intent.json': {'dispatchRequestIdHash': marker.assignment_id('dispatch')},
        'bound.json': {'sessionId':'s'},
        'relationship.json': {'relationshipId':'r'},
        'claims/s/claim.json': {'sessionId':'s','dispatchRequestId':'dispatch'},
        'dispositions/s/t.json': {'sessionId':'s','turnId':'t','outcome':'ready_for_review'},
    }.items():marker.publish(directory/name,record,root=root)
    stop = dict(cwd=str(work),session_id='s',turn_id='t',stop_hook_active=False)
    # Use the installed console script on both sides, including recovery argv[0].
    program = str(Path(sys.executable).parent / 'codex-session-relay')
    cfg = dict(configVersion=1,relayExecutable=program,markerRoot=str(root),socketPath=str(wanted),mode='hold',isolationAssertedBy='fixture',journalRoot=str(home/'journal'))
    (home/'crw-completion-hook.json').write_text(json.dumps(cfg))
    args=[program,'--socket',str(wanted),'guard-evaluate','--marker-root',str(root),'--mode','hold','--now',NOW]
    if case in ('wrong_socket','ambiguous','unidentified','override'):
        py=subprocess.run(args,input=json.dumps(stop).encode(),env=env,cwd=home,capture_output=True,timeout=10)
        assert py.returncode==2 and not py.stderr,py
        (home/'expected.json').write_bytes(py.stdout)
    (home/'fixture.json').write_text(json.dumps(dict(env=env,state=str(state),socket=str(wanted),root=str(root),stop=stop,program=program,now=NOW)))
    print(json.dumps(dict(state=str(state),source=selection.source,detail=selection.detail)))


def compare(home: Path):
    expected=json.loads((home/'expected.json').read_text())
    actual=json.loads((home/'actual.json').read_text())
    assert actual==expected,(actual,expected)
    assert not list((home/'markers').glob('*/*/hook/*/*/*.json')), 'refusal recorded an observation'
    print(json.dumps(dict(equal=True,reason=actual['reason'])))


def clock(home: Path):
    # Go prepared the marker and initial result; compare fresh Python evaluation
    # with the same injected datetime, including raw observation/hold bytes.
    from datetime import datetime, timezone
    f=json.loads((home/'fixture.json').read_text())
    root=Path(f['root']);after={str(p.relative_to(root)):p.read_bytes() for p in root.glob('*/*/hook/*/*/*.json')}
    for name in after:(root/name).unlink()
    now=datetime(2026,1,1,0,0,0,123456,tzinfo=timezone.utc).isoformat(timespec='microseconds')
    py=guard.evaluate(root,f['stop'],now=now,mode='hold',db_path=str(home/'clock.sqlite3'))
    assert (json.dumps(py,separators=(',',':'))+'\n').encode()==(home/'actual.json').read_bytes()
    assert after=={str(p.relative_to(root)):p.read_bytes() for p in root.glob('*/*/hook/*/*/*.json')}
    print('injected clock: envelope, observation and hold bytes equal')


if __name__=='__main__':
    action,home=sys.argv[1],Path(sys.argv[2])
    if action=='prepare':prepare(home,sys.argv[3])
    elif action=='clock':clock(home)
    else:compare(home)
